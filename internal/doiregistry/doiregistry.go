// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package doiregistry answers exactly one question about a DOI: is it
// registered with the global Handle System at all?
//
// This is deliberately not a metadata source. Crossref, OpenAlex, EuropePMC and
// Unpaywall all report "I have no record of this" and "this work exists but I
// hold no open copy" through the same empty result, so none of them can tell a
// typo'd DOI from a paywalled one. The Handle System can: a DOI either resolves
// to a registered handle or it does not, and that fact is free, unauthenticated,
// and independent of anyone's holdings.
//
// papio needs the distinction at one boundary — before it parks a job on an
// institutional sign-in. A DOI nobody registered cannot be matched by a link
// resolver, so the handoff bounces the user to doi.org's "DOI NOT FOUND" page
// and then re-offers forever.
package doiregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"papio/internal/work"
)

const (
	defaultBaseURL = "https://doi.org"
	defaultVersion = "0.1"
	defaultTimeout = 10 * time.Second
	// handlePathPrefix is the proxy's REST endpoint. It is asserted after the
	// DOI is appended, because the DOI is third-party input.
	handlePathPrefix = "/api/handles/"
	// Handle proxy responses are a handful of URL values; anything larger is
	// not a response this package knows how to read.
	maxResponseBytes = 64 << 10

	// Registration is close to permanent — a registered handle is essentially
	// never withdrawn — so a positive answer is worth holding for a long time.
	// A negative one is not: doi.org's own error page names "the DOI has not
	// been activated yet" as a cause, and a registrant can activate it minutes
	// later, so a miss expires quickly enough that a user who retries after
	// fixing things upstream gets a fresh answer.
	positiveTTL = 24 * time.Hour
	negativeTTL = 10 * time.Minute
	// The cache exists to bound outbound requests, not to be a store. Callers
	// re-probe the same small working set (the maintenance pass sweeps every
	// parked job once a minute), so a flat cap with a wholesale reset is
	// sufficient and cannot grow under adversarial submission.
	maxCacheEntries = 4096
)

// Handle System response codes (RFC 3650 proxy REST API). Only these four are
// meaningful here; everything else is an upstream fault.
const (
	handleSuccess        = 1   // handle resolved
	handleError          = 2   // proxy-side error
	handleNotFound       = 100 // no such handle: the DOI was never registered
	handleValuesNotFound = 200 // handle exists but holds no value of the requested type
)

// HTTPClient is the injection point for the daemon's bounded, SSRF-guarded
// client. It matches the same one-method shape the other metadata clients use.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Options configures a Client. Client is required in production: it carries the
// daemon's SSRF guard, redirect cap, and body bound, none of which this package
// reimplements.
type Options struct {
	Client       HTTPClient
	BaseURL      string
	Version      string
	ContactEmail string
}

type cacheEntry struct {
	registered bool
	expires    time.Time
}

// Client probes the DOI Handle System proxy, memoizing answers so a repeated
// sweep over the same parked jobs does not become a request per job per pass.
type Client struct {
	client  HTTPClient
	baseURL string
	agent   string

	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New builds a Client. A nil Options.Client is NOT replaced with a plain
// http.Client: that would silently drop the SSRF guard, proxy suppression, and
// redirect cap this package's only caller relies on. Registered then fails
// closed with an error, which the caller reads as "unknown" and fails open on.
func New(opts Options) *Client {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = defaultVersion
	}
	agent := "papio/" + version
	if email := strings.TrimSpace(opts.ContactEmail); email != "" {
		agent += " (mailto:" + email + ")"
	}
	return &Client{
		client: opts.Client, baseURL: baseURL, agent: agent,
		now: time.Now, cache: make(map[string]cacheEntry),
	}
}

type handleResponse struct {
	ResponseCode int `json:"responseCode"`
}

// Registered reports whether doi names a handle the DOI system knows.
//
// The bool is only meaningful when err is nil. An unreachable or malfunctioning
// registry returns an error and callers MUST treat that as "unknown" — reading
// it as "not registered" would terminate perfectly good jobs during an outage.
//
// Answers are memoized (see positiveTTL/negativeTTL). Errors never are: an
// outage must not be latched for hours.
func (c *Client) Registered(ctx context.Context, doi string) (bool, error) {
	if c == nil || c.client == nil {
		return false, errors.New("doiregistry: HTTP client is not configured")
	}
	normalized, err := work.NormalizeDOI(doi)
	if err != nil {
		// A string that is not even shaped like a DOI is not something the
		// registry can adjudicate; say so rather than reporting "unregistered".
		return false, fmt.Errorf("doiregistry: %w", err)
	}
	if registered, ok := c.cached(normalized); ok {
		return registered, nil
	}
	endpoint, err := c.handleURL(normalized)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("doiregistry: could not construct request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("doiregistry: handle request failed: %w", err)
	}
	if resp == nil {
		return false, errors.New("doiregistry: handle proxy returned an empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	// The proxy mirrors its response code in the HTTP status (200 for a
	// resolved handle, 404 for an unknown one), but the body is authoritative
	// and is present on both. Anything outside that pair is an upstream fault.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return false, fmt.Errorf("doiregistry: handle proxy returned HTTP %d for %q", resp.StatusCode, normalized)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return false, fmt.Errorf("doiregistry: reading handle response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return false, errors.New("doiregistry: handle response exceeds configured limit")
	}
	var payload handleResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("doiregistry: invalid handle response: %w", err)
	}
	switch payload.ResponseCode {
	case handleSuccess, handleValuesNotFound:
		// A registered handle carrying no URL value is still registered; that
		// is a metadata gap at the registrant, not a nonexistent DOI.
		c.remember(normalized, true)
		return true, nil
	case handleNotFound:
		c.remember(normalized, false)
		return false, nil
	case handleError:
		return false, fmt.Errorf("doiregistry: handle proxy reported an error for %q", normalized)
	default:
		return false, fmt.Errorf("doiregistry: unrecognized handle response code %d for %q", payload.ResponseCode, normalized)
	}
}

func (c *Client) cached(normalized string) (registered, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, hit := c.cache[normalized]
	if !hit || !c.now().Before(entry.expires) {
		return false, false
	}
	return entry.registered, true
}

func (c *Client) remember(normalized string, registered bool) {
	ttl := negativeTTL
	if registered {
		ttl = positiveTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCacheEntries {
		clear(c.cache)
	}
	c.cache[normalized] = cacheEntry{registered: registered, expires: c.now().Add(ttl)}
}

// handleURL builds the proxy REST path.
//
// Two traps, both live. path.Join is NOT usable here: it Cleans, and papio
// treats a repeated slash as significant — 10.48612//monograph-2025-2 and
// 10.48612/monograph-2025-2 are two separately registered works with different
// titles (see AGENTS.md and ownership_test.go's TestNormalizeIdentifier), so
// collapsing the run would probe a different DOI than the job names. Cleaning
// is also how a "." segment escapes: doiCoreRE admits any non-whitespace
// suffix, so 10.1234/../../../x is a legal DOI, and Join would resolve it to
// https://doi.org/x — doi.org's own resolver root, which 302s wherever that
// DOI's registrant points. Concatenate instead, and reject dot segments
// outright; no real DOI has one.
func (c *Client) handleURL(normalized string) (string, error) {
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("doiregistry: DOI %q contains a path segment %q", normalized, segment)
		}
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("doiregistry: invalid base URL: %w", err)
	}
	// Path, not the raw URL: String escapes per byte, so a DOI carrying ?, #,
	// % or a control byte cannot alter the request, while / stays a separator.
	u.Path += handlePathPrefix + normalized
	u.RawQuery = "type=URL"
	return u.String(), nil
}
