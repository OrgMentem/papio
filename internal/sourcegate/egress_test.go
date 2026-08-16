// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

type countingHTTP struct{ calls int }

func (c *countingHTTP) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
}

func testGuardedBudget(t *testing.T) *budget.Manager {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return budget.New(s, budget.WithCreditPolicy(func(string) budget.CreditPolicy {
		return budget.CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000}
	}))
}

func guardedRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: http.MethodGet, URL: u}
}

func TestGuardedCommitRefusalProducesZeroInnerRequests(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &countingHTTP{}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	if err := m.Defer(context.Background(), budget.QuotaSourceName(config.SourceOpenAlex), keyed, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	refusedResp, err := guarded.Do(guardedRequest(t, "https://api.openalex.org/works?api_key=private-key"))
	if refusedResp != nil {
		_ = refusedResp.Body.Close()
	}
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Do = %v, want *budget.ErrDeferred", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0 when commit refuses", inner.calls)
	}
}

func TestGuardedCommitRefusal_guardRequired(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &countingHTTP{}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	if err := m.Defer(context.Background(), budget.QuotaSourceName(config.SourceOpenAlex), keyed, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	budget.EgressTestDisableGates(t)
	if err := doAndClose(guarded, guardedRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatalf("guard disabled: err=%v, want commit to ignore quota gate", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 when gate check disabled", inner.calls)
	}
}

func TestGuardedOneDoOnePhysicalRequest(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &countingHTTP{}
	tripwire, err := NewRequireEgressCommit(inner)
	if err != nil {
		t.Fatal(err)
	}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, tripwire)
	if err := doAndClose(guarded, guardedRequest(t, "https://api.openalex.org/works/W1234567890?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want exactly one physical request", inner.calls)
	}
}

func TestGuardedIdentityFromOutgoingRequest(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &countingHTTP{}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	// Construction-time policy is keyed; the wire omits api_key (anonymous fallback).
	if err := doAndClose(guarded, guardedRequest(t, "https://api.openalex.org/works/W1234567890")); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
}

func TestRequireEgressCommitFailsWithoutCommit(t *testing.T) {
	inner := &countingHTTP{}
	tripwire, err := NewRequireEgressCommit(inner)
	if err != nil {
		t.Fatal(err)
	}
	tripwireResp, err := tripwire.Do(guardedRequest(t, "https://api.openalex.org/works"))
	if tripwireResp != nil {
		_ = tripwireResp.Body.Close()
	}
	if !errors.Is(err, ErrUncommittedEgress) {
		t.Fatalf("err = %v, want ErrUncommittedEgress", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
}

func TestLatchObservedAtCommit(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	identity := budget.IdentityFor(keyed)
	until := time.Now().UTC().Add(time.Hour)
	m.LatchQuota(config.SourceOpenAlex, identity, until)
	inner := &countingHTTP{}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	latchedResp, err := guarded.Do(guardedRequest(t, "https://api.openalex.org/works?api_key=private-key"))
	if latchedResp != nil {
		_ = latchedResp.Body.Close()
	}
	var deferred *budget.ErrDeferred
	if !errors.As(err, &deferred) || !deferred.Quota {
		t.Fatalf("Do = %v, want quota ErrDeferred from latch at commit", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
}

func TestLatchObservedAtCommit_guardRequired(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	m.LatchQuota(config.SourceOpenAlex, budget.IdentityFor(keyed), time.Now().UTC().Add(time.Hour))
	inner := &countingHTTP{}
	guarded := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	budget.EgressTestDisableGates(t)
	if err := doAndClose(guarded, guardedRequest(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatalf("guard disabled: err=%v, want latch ignored at commit", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 when latch gate disabled", inner.calls)
	}
}

func TestOpenAlexCreditCostClassifier(t *testing.T) {
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works/W1")); got != 1 {
		t.Fatalf("singleton = %d, want 1", got)
	}
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?search=trust")); got != 10 {
		t.Fatalf("search = %d, want 10", got)
	}
}
