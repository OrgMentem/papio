// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package doiregistry answers exactly one question about a DOI: is it
// registered with the global Handle System at all?
//
// This is deliberately not a metadata source. Crossref, OpenAlex, and Unpaywall
// all report "I have no record of this" and "this work exists but I hold no
// open copy" through the same empty result, so none of them can tell a typo'd
// DOI from a paywalled one. The Handle System can: a DOI either resolves to a
// registered handle or it does not, and that fact is free, unauthenticated, and
// independent of anyone's holdings.
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
	"path"
	"strings"
	"time"

	"papio/internal/work"
)

const (
	defaultBaseURL = "https://doi.org"
	defaultVersion = "0.1"
	defaultTimeout = 10 * time.Second
	// Handle proxy responses are a handful of URL values; anything larger is
	// not a response this package knows how to read.
	maxResponseBytes = 64 << 10
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

// Options configures a Client. Every field has a working default except
// Client, which should be the daemon's secure metadata client in production.
type Options struct {
	Client       HTTPClient
	BaseURL      string
	Version      string
	ContactEmail string
}

// Client probes the DOI Handle System proxy.
type Client struct {
	client  HTTPClient
	baseURL string
	agent   string
}

// New builds a Client. A nil Options.Client falls back to a plain timeout
// client so tests and one-off tools do not have to build the secure stack.
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
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{client: client, baseURL: baseURL, agent: agent}
}

type handleResponse struct {
	ResponseCode int `json:"responseCode"`
}

// Registered reports whether doi names a handle the DOI system knows.
//
// The bool is only meaningful when err is nil. An unreachable or malfunctioning
// registry returns an error and callers MUST treat that as "unknown" — reading
// it as "not registered" would terminate perfectly good jobs during an outage.
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
		return true, nil
	case handleNotFound:
		return false, nil
	case handleError:
		return false, fmt.Errorf("doiregistry: handle proxy reported an error for %q", normalized)
	default:
		return false, fmt.Errorf("doiregistry: unrecognized handle response code %d for %q", payload.ResponseCode, normalized)
	}
}

// handleURL builds the proxy REST path. The DOI's own slashes are path
// separators here, so the suffix is joined into Path (which String escapes
// per-segment) rather than concatenated into a raw URL.
func (c *Client) handleURL(normalized string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("doiregistry: invalid base URL: %w", err)
	}
	u.Path = path.Join(u.Path, "api", "handles", normalized)
	u.RawQuery = "type=URL"
	return u.String(), nil
}
