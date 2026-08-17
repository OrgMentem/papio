// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package openaire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/work"
)

// tokenServer serves the AAI client-credentials endpoint and counts exchanges,
// so a test can assert that a cached token is reused rather than re-fetched.
type tokenServer struct {
	exchanges int
	status    int
	body      string
	authSeen  string
	grantSeen string
}

func (s *tokenServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.exchanges++
		if id, secret, ok := r.BasicAuth(); ok {
			s.authSeen = id + ":" + secret
		}
		_ = r.ParseForm()
		s.grantSeen = r.Form.Get("grant_type")
		status := s.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := s.body
		if body == "" {
			body = `{"access_token":"tok-1","token_type":"Bearer","expires_in":3600}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientCredentialsExchangesAndCaches(t *testing.T) {
	server := &tokenServer{}
	srv := server.start(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	source := NewClientCredentials(ClientCredentialsOptions{
		Client:       srv.Client(),
		ClientID:     "svc-id",
		ClientSecret: "svc-secret",
		TokenURL:     srv.URL,
		Now:          func() time.Time { return now },
	})

	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "tok-1" {
		t.Fatalf("token = %q, want tok-1", token)
	}
	if server.grantSeen != "client_credentials" {
		t.Fatalf("grant_type = %q, want client_credentials", server.grantSeen)
	}
	if server.authSeen != "svc-id:svc-secret" {
		t.Fatalf("basic auth = %q, want svc-id:svc-secret", server.authSeen)
	}

	// A second call inside the token's lifetime must not spend another
	// exchange: papio resolves continuously, so one request per pass would
	// turn an hourly credential into per-pass traffic against the AAI host.
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if server.exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 (cached token reused)", server.exchanges)
	}

	// Past expiry (minus skew) it must exchange again rather than send a
	// credential OpenAIRE has already retired.
	now = now.Add(3600 * time.Second)
	server.body = `{"access_token":"tok-2","token_type":"Bearer","expires_in":3600}`
	token, err = source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after expiry: %v", err)
	}
	if token != "tok-2" {
		t.Fatalf("token after expiry = %q, want tok-2", token)
	}
	if server.exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2", server.exchanges)
	}
}

func TestClientCredentialsExpiryHonoursSkew(t *testing.T) {
	server := &tokenServer{body: `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`}
	srv := server.start(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	source := NewClientCredentials(ClientCredentialsOptions{
		Client: srv.Client(), ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL, Now: func() time.Time { return now },
	})
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}

	// Just inside the skew window the token is retired early, because a
	// token that expires mid-request costs a whole resolve pass.
	now = now.Add(3600*time.Second - tokenExpirySkew + time.Second)
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token near expiry: %v", err)
	}
	if server.exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2 (skew retires the token early)", server.exchanges)
	}
}

func TestClientCredentialsRejectionIsPermanentAndSecretFree(t *testing.T) {
	server := &tokenServer{status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`}
	srv := server.start(t)
	source := NewClientCredentials(ClientCredentialsOptions{
		Client: srv.Client(), ClientID: "id", ClientSecret: "super-secret-value",
		TokenURL: srv.URL,
	})

	_, err := source.Token(context.Background())
	if err == nil {
		t.Fatal("Token: want error for rejected credentials")
	}
	// A rejected credential pair is not going to start working on retry, so
	// it must not be classified temporary and burn a job's retry budget.
	var temporary *resolver.TemporaryError
	if errors.As(err, &temporary) {
		t.Fatalf("rejected credentials classified temporary: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error leaked the client secret: %v", err)
	}
}

func TestClientCredentialsServerFailureIsTemporary(t *testing.T) {
	server := &tokenServer{status: http.StatusBadGateway, body: `{}`}
	srv := server.start(t)
	source := NewClientCredentials(ClientCredentialsOptions{
		Client: srv.Client(), ClientID: "id", ClientSecret: "secret", TokenURL: srv.URL,
	})

	_, err := source.Token(context.Background())
	var temporary *resolver.TemporaryError
	if !errors.As(err, &temporary) {
		t.Fatalf("auth server 502 = %v, want a temporary error", err)
	}
}

func TestTokenTTLBoundsMissingExpiry(t *testing.T) {
	// A response without expires_in must not be cached forever: trusting it
	// indefinitely is exactly the api_key failure this type removes.
	if got := tokenTTL(0); got != tokenFallbackTTL {
		t.Fatalf("tokenTTL(0) = %v, want %v", got, tokenFallbackTTL)
	}
	if got := tokenTTL(3600); got != 3600*time.Second-tokenExpirySkew {
		t.Fatalf("tokenTTL(3600) = %v, want lifetime minus skew", got)
	}
	if got := tokenTTL(30); got != 30*time.Second {
		t.Fatalf("tokenTTL(30) = %v, want the raw short lifetime", got)
	}
}

func TestResolveSendsExchangedBearer(t *testing.T) {
	var seen string
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(graph.Close)
	server := &tokenServer{}
	auth := server.start(t)

	r := NewWithOptions(Options{
		Client:  graph.Client(),
		BaseURL: graph.URL,
		Tokens: NewClientCredentials(ClientCredentialsOptions{
			Client: auth.Client(), ClientID: "id", ClientSecret: "secret", TokenURL: auth.URL,
		}),
	})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0173664"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if seen != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want the exchanged bearer", seen)
	}
}

func TestResolveSurfacesTokenFailureWithoutCallingGraph(t *testing.T) {
	calls := 0
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(graph.Close)
	server := &tokenServer{status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`}
	auth := server.start(t)

	r := NewWithOptions(Options{
		Client:  graph.Client(),
		BaseURL: graph.URL,
		Tokens: NewClientCredentials(ClientCredentialsOptions{
			Client: auth.Client(), ClientID: "id", ClientSecret: "secret", TokenURL: auth.URL,
		}),
	})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0173664"}); err == nil {
		t.Fatal("Resolve: want the token failure surfaced")
	}
	// An unauthenticated Graph request would silently succeed at 1/120th of
	// the rate the operator configured for, hiding a broken credential.
	if calls != 0 {
		t.Fatalf("graph calls = %d, want 0 when the credential fails", calls)
	}
}

func TestStaticTokenIsUsedWhenNoSourceConfigured(t *testing.T) {
	var seen string
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(graph.Close)

	r := NewWithOptions(Options{Client: graph.Client(), BaseURL: graph.URL, APIKey: "  personal-token  "})
	if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1371/journal.pone.0173664"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if seen != "Bearer personal-token" {
		t.Fatalf("Authorization = %q, want the trimmed static token", seen)
	}
}
