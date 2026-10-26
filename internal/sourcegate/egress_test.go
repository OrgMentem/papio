// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"context"
	"errors"
	"net"
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
	// The metadata enricher searches via filter=title.search:, with no
	// search= parameter. The provider prices that shape at 10.
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?filter=title.search%3Atrust%2Cpublication_year%3A2020&per-page=5")); got != 10 {
		t.Fatalf("enricher title.search filter = %d, want 10", got)
	}
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?filter=abstract.search%3Atrust")); got != 10 {
		t.Fatalf("abstract.search filter = %d, want 10", got)
	}
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?filter=default.search%3Atrust")); got != 10 {
		t.Fatalf("default.search filter = %d, want 10", got)
	}
	// A citation filter with no search clause is not a search.
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?filter=cites%3AW2741809807")); got != 1 {
		t.Fatalf("citation filter = %d, want 1", got)
	}
	if got := OpenAlexCreditCost(guardedRequest(t, "https://api.openalex.org/works?filter=from_publication_date%3A2020-01-01%2Copen_access.is_oa%3Atrue")); got != 1 {
		t.Fatalf("discovery date filter = %d, want 1", got)
	}
}

// recordingAuthority captures the EgressRequest the guarded client builds, so a
// test can assert what actually reached the egress authority.
type recordingAuthority struct{ last budget.EgressRequest }

func (r *recordingAuthority) CommitEgress(_ context.Context, req budget.EgressRequest) error {
	r.last = req
	return nil
}

func TestGuardedClientAttributesEgressToTheContextJob(t *testing.T) {
	authority := &recordingAuthority{}
	inner := &countingHTTP{}
	g := MustGuarded(authority, config.SourceOpenAlex,
		config.Source{Enabled: true, APIKey: "key-a"}, OpenAlexCreditCost, inner)

	// The job id rides the context because the commit happens here, at the
	// wire, several layers below anything that knows what work is running.
	// If this attribution is lost the fair-share counter silently records
	// nothing and the share can never bind.
	req := guardedRequest(t, "https://api.openalex.org/works/W1?api_key=key-a")
	req = req.WithContext(budget.WithJobID(req.Context(), "job-abc"))
	if err := doAndClose(g, req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if authority.last.JobID != "job-abc" {
		t.Fatalf("JobID = %q, want job-abc", authority.last.JobID)
	}

	// An unattributed request must carry no job rather than inherit the last
	// one seen.
	plain := guardedRequest(t, "https://api.openalex.org/works/W2?api_key=key-a")
	if err := doAndClose(g, plain); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if authority.last.JobID != "" {
		t.Fatalf("JobID = %q, want empty for an unattributed request", authority.last.JobID)
	}
}

type failingHTTP struct{ err error }

func (f *failingHTTP) Do(*http.Request) (*http.Response, error) { return nil, f.err }

// A transport failure before any write reaches nothing, so it must not spend
// the day's allowance: otherwise an outage closes OpenAlex before any response
// can teach the fuse the real limit.
func TestGuardedRefundsCommitWhenNothingWasWritten(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	dead := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost,
		&failingHTTP{err: &net.OpError{Op: "dial", Err: errors.New("network is unreachable")}})
	for i := 0; i < 3*budget.ColdStartCreditCap/10; i++ {
		resp, err := dead.Do(guardedRequest(t, "https://api.openalex.org/works?search=x"))
		if resp != nil {
			_ = resp.Body.Close()
		}
		var exceeded *budget.ErrExceeded
		if errors.As(err, &exceeded) {
			t.Fatalf("request %d refused by the fuse after failed dials: %v", i, err)
		}
		if err == nil {
			t.Fatalf("request %d unexpectedly succeeded", i)
		}
	}
	inner := &countingHTTP{}
	live := MustGuarded(m, config.SourceOpenAlex, keyed, OpenAlexCreditCost, inner)
	resp, err := live.Do(guardedRequest(t, "https://api.openalex.org/works?search=x"))
	if err != nil {
		t.Fatalf("request after the outage: %v", err)
	}
	_ = resp.Body.Close()
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
}

// failingDeferFloor is the real budget.Manager (so the process-local latch is the
// production one) whose durable floor write always fails.
type failingDeferFloor struct{ *budget.Manager }

func (failingDeferFloor) Defer(context.Context, string, config.Source, time.Time) error {
	return errors.New("database is locked")
}

// A latched request must be refused before the credit commit, not after it:
// otherwise every refused attempt debits the day's allowance for a request
// that never reaches the wire. CommitEgress consults the same process-local
// latch the Observer sets, inside the committing transaction, before debiting.
func TestWrapOpenAlexLatchedRequestDebitsNoCredits(t *testing.T) {
	m := testGuardedBudget(t)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &headerClient{status: http.StatusOK, headers: map[string]string{
		"X-RateLimit-Limit": "10000", "X-RateLimit-Remaining": "100", "X-RateLimit-Reset": "3600",
	}}
	stack, err := WrapOpenAlex(m, failingDeferFloor{m}, config.SourceOpenAlex, keyed, inner)
	if err != nil {
		t.Fatal(err)
	}
	// The response that carries the low-quota news is served and charged; the
	// durable floor write fails, so only the process-local latch holds.
	if err := doAndClose(stack, guardedRequest(t, "https://api.openalex.org/works/W1?api_key=private-key")); err != nil {
		t.Fatalf("first request: %v", err)
	}
	before, err := m.CreditStatus(context.Background(), config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if before.Committed < 1 {
		t.Fatalf("committed = %d after one served request, want a debit", before.Committed)
	}

	for i := range 3 {
		resp, err := stack.Do(guardedRequest(t, "https://api.openalex.org/works?search=x&api_key=private-key"))
		if resp != nil {
			_ = resp.Body.Close()
		}
		var deferred *budget.ErrDeferred
		if !errors.As(err, &deferred) || !deferred.Quota {
			t.Fatalf("latched request %d = %v, want a quota *budget.ErrDeferred", i, err)
		}
	}
	after, err := m.CreditStatus(context.Background(), config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if after.Committed != before.Committed {
		t.Fatalf("committed = %d, want %d: latched requests must not debit credits", after.Committed, before.Committed)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want only the first request on the wire", inner.calls)
	}
}
