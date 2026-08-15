// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
)

// CreditCost classifies a request shape's conservative credit debit. OpenAlex
// supplies OpenAlexCreditCost; pure-rate sources can use UnitCreditCost so the
// fuse vocabulary never leaks into their wiring.
type CreditCost func(req *http.Request) int

// UnitCreditCost charges one unit per request for sources whose provider limit
// is a rate rather than a depletable credit balance.
func UnitCreditCost(*http.Request) int { return 1 }

// OpenAlexCreditCost maps OpenAlex works lookups to provider credits: singleton
// entity GET = 1, any search query = 10.
func OpenAlexCreditCost(req *http.Request) int {
	if req == nil || req.URL == nil {
		return 1
	}
	if req.URL.Query().Get("search") != "" {
		return 10
	}
	return 1
}

// EgressAuthority is the narrow budget capability the guarded client needs at
// the wire.
type EgressAuthority interface {
	CommitEgress(ctx context.Context, req budget.EgressRequest) error
}

// QuotaFloorController records durable quota floors and the process-local latch
// shared with CommitEgress.
type QuotaFloorController interface {
	Deferrer
	LatchQuota(source, identity string, until time.Time)
	QuotaLatchedUntil(source, identity string) (time.Time, bool)
}

type egressCommitted struct{}

// ErrUncommittedEgress means a physical OpenAlex request reached the transport
// without a successful CommitEgress on this stack.
var ErrUncommittedEgress = errors.New("sourcegate: HTTP egress without credit commit")

// GuardedClient commits egress authority, then performs exactly one inner Do.
// It never follows redirects, retries, or blocks after a successful commit.
type GuardedClient struct {
	inner     HTTPClient
	authority EgressAuthority
	source    string
	keyed     config.Source
	credits   CreditCost
}

// NewGuarded wraps inner so each request commits at the wire before one
// physical call. A nil authority or inner client is a wiring error.
func NewGuarded(authority EgressAuthority, source string, keyed config.Source, credits CreditCost, inner HTTPClient) (*GuardedClient, error) {
	if authority == nil {
		return nil, fmt.Errorf("sourcegate: %s guarded client has no egress authority", source)
	}
	if inner == nil {
		return nil, fmt.Errorf("sourcegate: %s guarded client has no inner client", source)
	}
	if credits == nil {
		credits = UnitCreditCost
	}
	keyed.APIKey = trimAPIKey(keyed.APIKey)
	return &GuardedClient{inner: inner, authority: authority, source: source, keyed: keyed, credits: credits}, nil
}

// MustGuarded panics on a wiring error, like bootstrap startup wiring.
func MustGuarded(authority EgressAuthority, source string, keyed config.Source, credits CreditCost, inner HTTPClient) *GuardedClient {
	g, err := NewGuarded(authority, source, keyed, credits, inner)
	if err != nil {
		panic(err)
	}
	return g
}

// Do derives identity from the outgoing request, commits egress, then forwards
// once. No blocking wait may follow a nil commit error.
func (g *GuardedClient) Do(req *http.Request) (*http.Response, error) {
	served, known := ServedIdentity(req, g.keyed)
	if !known {
		return nil, fmt.Errorf("sourcegate: %s request bears an unknown credential", g.source)
	}
	identity := budget.IdentityFor(served)
	credits := g.credits(req)
	if credits < 1 {
		credits = 1
	}
	if err := g.authority.CommitEgress(req.Context(), budget.EgressRequest{
		Source:   g.source,
		Identity: identity,
		Credits:  credits,
	}); err != nil {
		return nil, err
	}
	ctx := context.WithValue(req.Context(), egressCommitted{}, struct{}{})
	return g.inner.Do(req.WithContext(ctx))
}

// RequireEgressCommit is the innermost tripwire on an OpenAlex stack. It fails
// loudly when a request would reach the transport without a prior commit.
type RequireEgressCommit struct {
	inner HTTPClient
}

// NewRequireEgressCommit wraps inner with an egress-commit tripwire.
func NewRequireEgressCommit(inner HTTPClient) (*RequireEgressCommit, error) {
	if inner == nil {
		return nil, errors.New("sourcegate: egress commit guard has no inner client")
	}
	return &RequireEgressCommit{inner: inner}, nil
}

// Do refuses requests that did not pass through GuardedClient.Do.
func (c *RequireEgressCommit) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.Context().Value(egressCommitted{}) == nil {
		return nil, ErrUncommittedEgress
	}
	return c.inner.Do(req)
}

// PacingClient applies identity-agnostic token-bucket pacing only. OpenAlex
// discovery uses this beneath GuardedClient so construction-time api_key policy
// cannot pre-empt the wire-derived identity authority.
type PacingClient struct {
	inner    HTTPClient
	reserve  Reserver
	source   string
	pacing   config.Source
	costEach float64
}

// NewPacingOnly wraps inner with source-wide pacing from policy's rate/burst.
// The policy's API key is ignored for admission identity.
func NewPacingOnly(reserve Reserver, source string, policy config.Source, costEach float64, inner HTTPClient) (*PacingClient, error) {
	if reserve == nil {
		return nil, fmt.Errorf("sourcegate: %s pacing client has no reserver", source)
	}
	if inner == nil {
		return nil, fmt.Errorf("sourcegate: %s pacing client has no inner client", source)
	}
	pacing := policy
	pacing.APIKey = ""
	return &PacingClient{inner: inner, reserve: reserve, source: source, pacing: pacing, costEach: costEach}, nil
}

// Do paces, then forwards without re-reserving per identity.
func (c *PacingClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.reserve.Acquire(req.Context(), c.source, c.pacing, c.costEach); err != nil {
		return nil, err
	}
	return c.inner.Do(req)
}

// WrapOpenAlex builds the production OpenAlex HTTP stack:
// GuardedClient → Observer → RequireEgressCommit → inner transport.
//
// OpenAlex discovery deliberately does NOT use sourcegate.Client: its
// construction-time policy would race the wire-derived identity that
// GuardedClient commits. Token-bucket pacing for discovery is applied outside
// this helper via NewPacingOnly. resolverEntries injects the returned client
// without a second Client wrapper for the same reason.
func WrapOpenAlex(authority EgressAuthority, floor QuotaFloorController, source string, keyed config.Source, inner HTTPClient) (HTTPClient, error) {
	tripwire, err := NewRequireEgressCommit(inner)
	if err != nil {
		return nil, err
	}
	observer, err := NewObserver(floor, source, keyed, tripwire)
	if err != nil {
		return nil, err
	}
	return NewGuarded(authority, source, keyed, OpenAlexCreditCost, observer)
}
