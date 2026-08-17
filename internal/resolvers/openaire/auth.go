// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package openaire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"papio/internal/resolver"
)

// OpenAIRE issues two kinds of credential, and only one of them can run a
// daemon unattended.
//
// A *personal access token* (develop.openaire.eu/personal-token) is a bearer
// string that OpenAIRE documents as "valid for an hour". Pasted into
// [sources.openaire].api_key it authenticates correctly, raises the rate
// ceiling, and then starts failing every request 60 minutes later — a failure
// that looks like a provider outage rather than an expiry, because the request
// shape never changed. papio still accepts api_key for exactly that reason:
// short manual checks, not operation.
//
// A *registered service* (develop.openaire.eu/apis, Basic security level)
// yields a client id and secret that do not expire. The service exchanges them
// for a short-lived access token whenever it needs one, which is what this
// token source does. That is the credential an unattended papio wants.
const (
	defaultTokenURL = "https://aai.openaire.eu/oidc/token"

	// tokenExpirySkew retires a cached token early. The exchange is cheap
	// and hourly; a token that expires mid-flight costs a whole resolve
	// pass, so buy margin rather than precision.
	tokenExpirySkew = 2 * time.Minute

	// tokenFallbackTTL bounds a token whose response omitted expires_in.
	// Trusting such a token indefinitely would reintroduce the api_key
	// failure this type exists to remove.
	tokenFallbackTTL = 30 * time.Minute

	maxTokenResponseBytes = int64(1 << 16)
)

// TokenSource supplies a bearer credential for OpenAIRE Graph requests.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a pre-issued bearer string, used for api_key. It is not
// refreshable: when OpenAIRE expires it, every later request fails.
type StaticToken string

// Token returns the static credential.
func (s StaticToken) Token(context.Context) (string, error) {
	value := strings.TrimSpace(string(s))
	if value == "" {
		return "", errors.New("openaire: empty static token")
	}
	return value, nil
}

// ClientCredentialsOptions configures a registered-service token source.
type ClientCredentialsOptions struct {
	Client       HTTPClient
	ClientID     string
	ClientSecret string
	// TokenURL overrides OpenAIRE's AAI token endpoint for tests.
	TokenURL string
	// Now overrides the clock for tests.
	Now func() time.Time
}

// ClientCredentials exchanges a registered service's non-expiring client id
// and secret for short-lived access tokens, caching each until shortly before
// it expires.
type ClientCredentials struct {
	client       HTTPClient
	tokenURL     string
	clientID     string
	clientSecret string
	now          func() time.Time

	// mu is held across the exchange, not merely around the cache read, so
	// that a fleet of concurrent resolve passes performs ONE token request
	// between expiries instead of one per pass.
	mu     sync.Mutex
	token  string
	expiry time.Time
}

var _ TokenSource = (*ClientCredentials)(nil)

// NewClientCredentials constructs a caching client-credentials token source.
func NewClientCredentials(opts ClientCredentialsOptions) *ClientCredentials {
	tokenURL := strings.TrimSpace(opts.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &ClientCredentials{
		client:       opts.Client,
		tokenURL:     tokenURL,
		clientID:     strings.TrimSpace(opts.ClientID),
		clientSecret: strings.TrimSpace(opts.ClientSecret),
		now:          now,
	}
}

// Token returns a cached access token, exchanging the client credentials for a
// fresh one when none is cached or the cached one is near expiry.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("openaire: token source is not configured")
	}
	if c.client == nil {
		return "", errors.New("openaire: token source has no HTTP client")
	}
	if c.clientID == "" || c.clientSecret == "" {
		return "", errors.New("openaire: client_id and client_secret must both be set")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiry) {
		return c.token, nil
	}
	token, ttl, err := c.exchange(ctx)
	if err != nil {
		return "", err
	}
	c.token = token
	c.expiry = c.now().Add(ttl)
	return token, nil
}

// exchange performs the OAuth2 client-credentials request. Its failures are
// classified for the resolver: a credential OpenAIRE rejects is permanent and
// naming it is the whole diagnostic, while an unreachable or broken auth
// server is temporary and must not burn a job's retry budget.
func (c *ClientCredentials) exchange(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": []string{"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, errors.New("openaire: could not construct token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.clientID, c.clientSecret)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, &resolver.TemporaryError{Err: errors.New("openaire: token request failed")}
	}
	if resp == nil {
		return "", 0, &resolver.TemporaryError{Err: errors.New("openaire: empty token response")}
	}
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// Never echo the response body: it is an auth failure on a
		// credential pair, and papio does not put secrets in errors.
		return "", 0, fmt.Errorf("openaire: registered service credentials were rejected (HTTP %d); check client_id and client_secret at https://develop.openaire.eu/apis", resp.StatusCode)
	default:
		return "", 0, temporaryStatus(resp)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := decodeBoundedJSON(resp.Body, maxTokenResponseBytes, &payload); err != nil {
		return "", 0, &resolver.TemporaryError{Err: errors.New("openaire: token response was not readable JSON")}
	}
	token := strings.TrimSpace(payload.AccessToken)
	if token == "" {
		return "", 0, &resolver.TemporaryError{Err: errors.New("openaire: token response carried no access_token")}
	}
	if kind := strings.TrimSpace(payload.TokenType); kind != "" && !strings.EqualFold(kind, "bearer") {
		return "", 0, fmt.Errorf("openaire: token response declared unsupported token_type %q", kind)
	}
	return token, tokenTTL(payload.ExpiresIn), nil
}

// tokenTTL converts a provider-reported lifetime into a cache duration. A
// missing, absurd, or already-elapsed lifetime falls back to a bounded default
// rather than being trusted or treated as an error.
func tokenTTL(expiresIn int64) time.Duration {
	if expiresIn <= 0 {
		return tokenFallbackTTL
	}
	lifetime := time.Duration(expiresIn) * time.Second
	if lifetime <= tokenExpirySkew {
		return lifetime
	}
	return lifetime - tokenExpirySkew
}
