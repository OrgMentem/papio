// Package sourcegate binds an HTTP client to a source's budget, so that every
// request it makes is reserved and paced like any other call to that source.
//
// It exists because papio reached OpenAlex through two clients and only one of
// them was accounted for. The acquisition resolvers call budget.Acquire at the
// job level, but internal/discovery — used by search, MCP, watch digests and
// the DOI-only enrichment path — held a bare HTTP client and drew on the same
// provider quota invisibly. The budget manager therefore under-reported papio's
// own consumption by an unknown amount, and a durable next_allowed_at gate that
// paused acquisition did not pause discovery at all: it kept calling an API
// that had already said stop.
//
// The gate lives here rather than in internal/budget so that package stays
// free of net/http, and rather than in internal/discovery so it is not specific
// to one caller.
package sourcegate

import (
	"context"
	"fmt"
	"net/http"

	"papio/internal/config"
)

// HTTPClient is the subset of an HTTP client this package wraps. It matches
// both *fetch.SecureHTTPClient and *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Reserver is the budget manager's reservation call. Taking an interface keeps
// the dependency one-way and makes the failure modes testable without a store.
type Reserver interface {
	Acquire(ctx context.Context, source string, policy config.Source, estimatedCost float64) error
}

// Client reserves against a source budget before every request it forwards.
type Client struct {
	inner    HTTPClient
	reserve  Reserver
	source   string
	policy   config.Source
	costEach float64
}

// New wraps inner so each request reserves one call against source's budget.
//
// A nil reserver is a programming error, not a supported mode. Returning inner
// unwrapped would leave a provider client that works perfectly and is invisible
// to accounting — the exact defect this package was written to end — so letting
// a wiring mistake reproduce it silently would be self-defeating. Tests that
// genuinely want no reservation pass a no-op Reserver and say so.
func New(reserve Reserver, source string, policy config.Source, costEach float64, inner HTTPClient) (HTTPClient, error) {
	if reserve == nil {
		return nil, fmt.Errorf("sourcegate: %s has no reserver; every provider client must be accounted for", source)
	}
	if inner == nil {
		return nil, fmt.Errorf("sourcegate: %s has no inner client", source)
	}
	return &Client{inner: inner, reserve: reserve, source: source, policy: policy, costEach: costEach}, nil
}

// Do reserves, then forwards. A refused reservation is returned unwrapped so
// callers can recognise *budget.ErrDeferred and *budget.ErrExceeded with
// errors.As and report a rate limit as such rather than as a transport fault.
//
// Reserving per HTTP REQUEST rather than per logical call is deliberate: a
// discovery search that resolves a seed DOI first issues two requests, and the
// provider counts two. Accounting for one is the under-reporting this package
// exists to end.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if err := c.reserve.Acquire(req.Context(), c.source, c.policy, c.costEach); err != nil {
		return nil, err
	}
	return c.inner.Do(req)
}
