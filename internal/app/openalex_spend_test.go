// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/sourcegate"
	"papio/internal/work"
)

// contextResolver records the context each Resolve call was made under, which
// is how the anonymous-fallback marker is observed. The shared fakeResolver
// deliberately discards its context, so this is a separate fake rather than a
// widening of that one.
type contextResolver struct {
	name        string
	cands       []resolver.Candidate
	err         error
	calls       int
	capturedCtx []context.Context
}

func (f *contextResolver) Name() string { return f.name }
func (f *contextResolver) Resolve(ctx context.Context, _ work.Work) ([]resolver.Candidate, error) {
	f.calls++
	f.capturedCtx = append(f.capturedCtx, ctx)
	return append([]resolver.Candidate(nil), f.cands...), f.err
}

// siblingFake implements SiblingResolver. It is deliberately NOT the shared
// fakeResolver: adding the method there would silently enrol every existing
// fake in the sibling hop.
type siblingFake struct {
	name     string
	cands    []resolver.Candidate
	err      error
	siblings int
}

func (f *siblingFake) Name() string { return f.name }
func (f *siblingFake) Resolve(context.Context, work.Work) ([]resolver.Candidate, error) {
	return nil, nil
}

func (f *siblingFake) ResolveSiblings(context.Context, work.Work) ([]resolver.Candidate, error) {
	f.siblings++
	return append([]resolver.Candidate(nil), f.cands...), f.err
}

// enricherFunc is an inline MetadataEnricher for the charging tests.
type enricherFunc struct {
	calls int
	fn    func(work.Work) (work.Work, bool, error)
}

func (f *enricherFunc) Enrich(_ context.Context, requested work.Work) (work.Work, bool, error) {
	f.calls++
	return f.fn(requested)
}

// lookupFunc is an inline WorkLookup for the DOI-enrichment charging tests.
type lookupFunc struct {
	calls int
	fn    func(string) (discovery.DiscoveredWork, error)
}

func (f *lookupFunc) LookupWork(_ context.Context, doi string) (discovery.DiscoveredWork, error) {
	f.calls++
	return f.fn(doi)
}

func TestRetryPlanKindChargesCalledPasses(t *testing.T) {
	t1 := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	if got := (retryPlan{SourcesCalled: 1, ClosedSourceGates: 1, Gate: t1}).Kind(); got != retryKindTemporary {
		t.Fatalf("mixed pass Kind() = %q, want %q so the retry budget bounds it", got, retryKindTemporary)
	}
	if got := (retryPlan{ClosedSourceGates: 1, Gate: t1}).Kind(); got != retryKindSourceGate {
		t.Fatalf("pure gate Kind() = %q, want %q", got, retryKindSourceGate)
	}
	if got := (retryPlan{AdvisoryBackoffs: 1}).Kind(); got != retryKindAdvisory {
		t.Fatalf("advisory-only Kind() = %q, want %q", got, retryKindAdvisory)
	}
}

func TestRetryCutoverDecisionChargesCalledPasses(t *testing.T) {
	t1 := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	decision := retryCutoverDecision(retryPlan{SourcesCalled: 1, ClosedSourceGates: 1, Gate: t1})
	if decision.Blocker != job.InstitutionCutoverBlockerTransientRetryRemaining {
		t.Fatalf("blocker = %q, want transient_retry_remaining: the pass reached a source", decision.Blocker)
	}
}

func TestRetryPlanMergeSumsSourcesCalled(t *testing.T) {
	plan := retryPlan{SourcesCalled: 1}
	plan.merge(retryPlan{SourcesCalled: 2})
	if plan.SourcesCalled != 3 {
		t.Fatalf("SourcesCalled = %d, want 3", plan.SourcesCalled)
	}
}

func TestRetryPlanMergeCarriesLatestGate(t *testing.T) {
	t1 := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	plan := retryPlan{Gate: t1, LatestGate: t1}
	plan.merge(retryPlan{Gate: t2, LatestGate: t2})
	if !plan.Gate.Equal(t1) {
		t.Fatalf("Gate = %v, want the earliest %v for scheduling", plan.Gate, t1)
	}
	if !plan.LatestGate.Equal(t2) {
		t.Fatalf("LatestGate = %v, want the latest %v for the one post-exhaustion wait", plan.LatestGate, t2)
	}
}

func TestGatePendingUsesTheLatestGate(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	plan := retryPlan{Gate: now.Add(-1 * time.Second), LatestGate: now.Add(24 * time.Hour)}
	if !plan.GatePending(now) {
		t.Fatal("GatePending = false while a day-long gate is still closed")
	}
}

// spendRetryBudget writes the durable transitions that make retryBudgetExhausted
// true, using the chargeable retry_kind so none of them is skipped.
func spendRetryBudget(t *testing.T, svc *Service, jobs *job.Store, id string) {
	t.Helper()
	ctx := context.Background()
	for range maxRetryAttempts {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateResolving {
			if err := jobs.Transition(ctx, id, row.State, job.StateResolving,
				map[string]any{"reason": "scheduler_dispatch"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := jobs.Transition(ctx, id, job.StateResolving, job.StateRetryWait,
			map[string]any{"reason": "resolver_temporarily_unavailable", "retry_kind": retryKindTemporary},
			job.WithRetryAt(svc.Now().Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if !svc.retryBudgetExhausted(ctx, id) {
		t.Fatal("retry budget is not exhausted after the bounded attempts")
	}
}

// When several sources are gated for different durations, the one wait the job
// gets past exhaustion must be long enough for the slowest of them — otherwise
// that source still never gets the call the rule exists to grant it.
func TestParkForRetryWaitsOutTheLongestPendingGate(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	svc.RetryDelay = 30 * time.Second
	id, err := svc.Submit(ctx, doiRequest("wr_latest_gate"))
	if err != nil {
		t.Fatal(err)
	}
	spendRetryBudget(t, svc, jobs, id)
	if err := jobs.Transition(ctx, id, job.StateRetryWait, job.StateResolving,
		map[string]any{"reason": "scheduler_dispatch"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	plan := retryPlan{
		Gate: now.Add(5 * time.Minute), LatestGate: now.Add(24 * time.Hour), ClosedSourceGates: 2,
	}
	if err := svc.parkForRetry(ctx, row, job.StateResolving, plan,
		map[string]any{"reason": "resolver_temporarily_unavailable"},
		job.TerminalReasonTemporarySourceFailuresDidNotClear, ""); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait: a pending gate buys one more wait", got.State)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, got.RetryAt)
	if err != nil {
		t.Fatalf("parsing retry_at %q: %v", got.RetryAt, err)
	}
	if want := now.Add(24 * time.Hour); !retryAt.Equal(want) {
		t.Fatalf("retry_at = %v, want the longest pending gate %v", retryAt, want)
	}
	if detail := retryWaitDetail(t, jobs, id); detail["retry_kind"] != retryKindExhaustedGate {
		t.Fatalf("retry_kind = %v, want %q", detail["retry_kind"], retryKindExhaustedGate)
	}
}

// gatedEnrichmentService builds a service whose only acquisition resolver is
// durably gated, so the metadata enricher is the pass's only outbound call.
func gatedEnrichmentService(t *testing.T, requestID string, enricher MetadataEnricher) (*Service, *job.Store, string) {
	t.Helper()
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.RetryDelay = time.Millisecond
	svc.Budgets = budget.New(jobs.S)
	if err := svc.Budgets.Defer(ctx, "fixture", config.Source{Enabled: true},
		time.Now().UTC().Add(18*time.Hour)); err != nil {
		t.Fatal(err)
	}
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
	svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: config.SourceCrossrefMetadata, Enricher: enricher}}
	svc.Config.Sources[config.SourceCrossrefMetadata] = config.Source{Enabled: true}
	id, err := svc.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: requestID,
		Title: "A bounded test work", Authors: []string{"A. Researcher"}, Year: 2026,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, jobs, id
}

func planFromResolve(t *testing.T, svc *Service, jobs *job.Store, id string) retryPlan {
	t.Helper()
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	_, plan, err := svc.resolve(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// A metadata enrichment request is a real, priced provider call — the most
// expensive request shape papio makes. A pass whose only outbound call was that
// one must still be charged, or a permanently-stuck job re-runs it every cycle
// for free behind an unrelated source's gate.
func TestEnrichmentAloneChargesThePass(t *testing.T) {
	for _, test := range []struct {
		name    string
		id      string
		enrich  func(work.Work) (work.Work, bool, error)
		charged int
	}{
		{
			name: "matched",
			id:   "matched",
			enrich: func(requested work.Work) (work.Work, bool, error) {
				enriched := requested
				enriched.DOI = "10.1000/enriched"
				return enriched, true, nil
			},
			charged: 1,
		},
		{
			name: "searched and found nothing",
			id:   "nomatch",
			enrich: func(requested work.Work) (work.Work, bool, error) {
				return requested, false, nil
			},
			charged: 1,
		},
		{
			name: "pre-wire decline",
			id:   "notapplicable",
			enrich: func(requested work.Work) (work.Work, bool, error) {
				return requested, false, resolver.ErrNotApplicable
			},
			charged: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			enricher := &enricherFunc{fn: test.enrich}
			svc, jobs, id := gatedEnrichmentService(t, "wr_enrich_charge_"+test.id, enricher)
			plan := planFromResolve(t, svc, jobs, id)
			if enricher.calls != 1 {
				t.Fatalf("enricher calls = %d, want 1", enricher.calls)
			}
			if plan.SourcesCalled != test.charged {
				t.Fatalf("SourcesCalled = %d, want %d", plan.SourcesCalled, test.charged)
			}
			if plan.ClosedSourceGates != 1 {
				t.Fatalf("ClosedSourceGates = %d, want the gated acquisition source recorded", plan.ClosedSourceGates)
			}
			wantKind := retryKindTemporary
			if test.charged == 0 {
				wantKind = retryKindSourceGate
			}
			if got := plan.Kind(); got != wantKind {
				t.Fatalf("Kind() = %q, want %q", got, wantKind)
			}
		})
	}
}

// A work that already has a fetchable identifier never reaches the enricher
// loop, so nothing is charged for it.
func TestEnrichmentSkippedEntirelyChargesNothing(t *testing.T) {
	enricher := &enricherFunc{fn: func(requested work.Work) (work.Work, bool, error) {
		t.Fatal("enricher ran for a work that already has a fetchable identifier")
		return requested, false, nil
	}}
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: config.SourceCrossrefMetadata, Enricher: enricher}}
	svc.Config.Sources[config.SourceCrossrefMetadata] = config.Source{Enabled: true}
	id, err := svc.Submit(ctx, doiRequest("wr_enrich_skip"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.enrich(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourcesCalled != 0 {
		t.Fatalf("SourcesCalled = %d, want 0", plan.SourcesCalled)
	}
}

// The DOI-only discovery lookup is budgeted provider traffic too, and it is
// charged by admission: whatever the response says, the request went out.
func TestDOIEnrichmentAloneChargesThePass(t *testing.T) {
	discovered := discovery.DiscoveredWork{Work: work.Work{
		DOI: "10.1002/example", Title: "Discovered title", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}}
	for _, test := range []struct {
		name      string
		id        string
		doi       string
		lookup    func(string) (discovery.DiscoveredWork, error)
		charged   int
		gates     int
		wantCalls int
	}{
		{
			name: "successful lookup",
			id:   "success",
			lookup: func(string) (discovery.DiscoveredWork, error) {
				return discovered, nil
			},
			charged: 1, wantCalls: 1,
		},
		{
			name: "post-wire failure",
			id:   "postwire",
			lookup: func(string) (discovery.DiscoveredWork, error) {
				return discovery.DiscoveredWork{}, errors.New("discovery: OpenAlex returned HTTP 404")
			},
			charged: 1, wantCalls: 1,
		},
		{
			name: "admission refused",
			id:   "deferred",
			lookup: func(string) (discovery.DiscoveredWork, error) {
				return discovery.DiscoveredWork{}, &budget.ErrDeferred{
					Source: config.SourceOpenAlex, Identity: "key-test",
					Until: time.Now().UTC().Add(20 * time.Hour),
				}
			},
			charged: 0, gates: 1, wantCalls: 1,
		},
		{
			name:    "invalid doi never reaches the wire",
			id:      "invaliddoi",
			doi:     "not a doi",
			lookup:  func(string) (discovery.DiscoveredWork, error) { return discovered, nil },
			charged: 0, wantCalls: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, jobs := newTestService(t)
			ctx := context.Background()
			lookup := &lookupFunc{fn: test.lookup}
			svc.Discovery = lookup
			id, err := svc.Submit(ctx, doiRequest("wr_doi_charge_"+test.id))
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if test.doi != "" {
				row.Work.DOI = test.doi
			}
			plan, err := svc.enrichDOIWork(ctx, row)
			if err != nil {
				t.Fatal(err)
			}
			if lookup.calls != test.wantCalls {
				t.Fatalf("LookupWork calls = %d, want %d", lookup.calls, test.wantCalls)
			}
			if plan.SourcesCalled != test.charged {
				t.Fatalf("SourcesCalled = %d, want %d", plan.SourcesCalled, test.charged)
			}
			if plan.ClosedSourceGates != test.gates {
				t.Fatalf("ClosedSourceGates = %d, want %d", plan.ClosedSourceGates, test.gates)
			}
		})
	}
}

// The charged loop, end to end: a job whose candidates are permanently dead
// makes its bounded number of charged passes and then gets exactly one wait for
// the gated source that never got a call — instead of re-running the whole
// resolver chain every cycle forever, which is what burned a day's OpenAlex
// quota in 25 minutes.
func TestChargedLoopSettlesAfterBudget(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clock := now
	svc.Now = func() time.Time { return clock }
	svc.RetryDelay = 30 * time.Second
	svc.Budgets = budget.New(jobs.S, budget.WithNow(func() time.Time { return clock }))
	gate := now.Add(20 * time.Hour)
	if err := svc.Budgets.Defer(ctx, "gatedsource", config.Source{Enabled: true}, gate); err != nil {
		t.Fatal(err)
	}
	svc.Config.Sources["deadlink"] = config.Source{Enabled: true}
	svc.Config.Sources["flaky"] = config.Source{Enabled: true}
	svc.Config.Sources["gatedsource"] = config.Source{Enabled: true}
	dead := &fakeResolver{name: "deadlink", cands: []resolver.Candidate{{
		Source: "deadlink", URL: "https://example.test/expired.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: dead, Policy: config.Source{Enabled: true}},
		{Adapter: &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: errors.New("upstream rate limited")}},
			Policy: config.Source{Enabled: true}},
		{Adapter: &fakeResolver{name: "gatedsource"}, Policy: config.Source{Enabled: true}},
	}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: 403, Msg: "permanently dead"}
	}
	svc.Validate = passValidation()
	id, err := svc.Submit(ctx, doiRequest("wr_charged_loop"))
	if err != nil {
		t.Fatal(err)
	}
	for range maxRetryAttempts + 1 {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateQueued && row.State != job.StateRetryWait {
			t.Fatalf("job left the retry cycle in %s before its budget was spent", row.State)
		}
		if err := svc.Process(ctx, row); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(svc.RetryDelay)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	temporary, exhausted := 0, 0
	var exhaustedAt string
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if to, _ := detail["to"].(string); to != job.StateRetryWait {
			continue
		}
		switch kind, _ := detail["retry_kind"].(string); kind {
		case retryKindTemporary:
			temporary++
		case retryKindExhaustedGate:
			exhausted++
			exhaustedAt, _ = detail["retry_at"].(string)
		}
	}
	if temporary != maxRetryAttempts {
		t.Fatalf("charged parks = %d, want %d: every pass that reached a source is charged", temporary, maxRetryAttempts)
	}
	if exhausted != 1 {
		t.Fatalf("exhausted-gate parks = %d, want exactly one", exhausted)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, row.RetryAt)
	if err != nil {
		t.Fatalf("parsing retry_at %q: %v (event detail %q)", row.RetryAt, err, exhaustedAt)
	}
	if !retryAt.Equal(gate) {
		t.Fatalf("retry_at = %v, want the gated source's own reset %v", retryAt, gate)
	}
}

// The exhaustion boundary itself: with an institutional route configured and a
// DOI to act on, a spent retry budget opens the OpenURL handoff rather than
// settling the job.
func TestExhaustionOpensInstitutionalHandoff(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.Config.AccessMode = config.ModeAssisted
	svc.Config.Browser.OpenURLBase = "https://resolver.example/openurl"
	id, err := svc.Submit(ctx, cutoverRequest("cutover_exhaustion_handoff", true))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving,
		map[string]any{"reason": "scheduler_dispatch"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// EffectiveAccessMode narrows to the more restrictive of config and policy,
	// so both must permit the assisted route.
	row.Policy.AccessMode = config.ModeAssisted
	if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "retry_budget_exhausted",
		job.TerminalReasonTemporarySourceFailuresDidNotClear, ""); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %s, want awaiting_human", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != "openurl_handoff" {
		t.Fatalf("actions = %#v, want one openurl_handoff", actions)
	}
	if actions[0].Detail != InstitutionalOpenURLHandoffDetail {
		t.Fatalf("detail = %q, want the institutional OpenURL handoff detail", actions[0].Detail)
	}
}

// fallbackService wires one openalex-named adapter with a keyed policy over a
// real budget manager, which is what the fallback arbitration needs to observe.
func fallbackService(t *testing.T, requestID, adapterName string) (*Service, *job.Store, *contextResolver, string) {
	t.Helper()
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.RetryDelay = time.Millisecond
	svc.Budgets = budget.New(jobs.S)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	svc.Config.Sources[adapterName] = keyed
	adapter := &contextResolver{name: adapterName}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: keyed}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("no candidate should be fetched")
	}
	svc.Validate = passValidation()
	id, err := svc.Submit(ctx, doiRequest(requestID))
	if err != nil {
		t.Fatal(err)
	}
	return svc, jobs, adapter, id
}

func TestFallbackRunsAdapterAnonymously(t *testing.T) {
	svc, jobs, adapter, id := fallbackService(t, "wr_anon_fallback", config.SourceOpenAlex)
	ctx := context.Background()
	keyed := svc.Config.SourcePolicy(config.SourceOpenAlex)
	if err := svc.Budgets.Defer(ctx, config.SourceOpenAlex+"_quota", keyed,
		time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	planFromResolve(t, svc, jobs, id)
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want the keyless tier attempted", adapter.calls)
	}
	if !resolver.AnonymousCredentials(adapter.capturedCtx[0]) {
		t.Fatal("adapter ran with keyed credentials while the keyed quota is exhausted")
	}
}

func TestFallbackRunsAnonymouslyOnProcessLocalLatchOnly(t *testing.T) {
	svc, jobs, adapter, id := fallbackService(t, "wr_latch_only_fallback", config.SourceOpenAlex)
	ctx := context.Background()
	keyed := svc.Config.SourcePolicy(config.SourceOpenAlex)
	until := time.Now().UTC().Add(6 * time.Hour)
	svc.Budgets.LatchQuota(config.SourceOpenAlex, budget.IdentityFor(keyed), until)
	quotaSnap, err := svc.Budgets.Snapshot(ctx, config.SourceOpenAlex+"_quota", keyed)
	if err != nil {
		t.Fatal(err)
	}
	if quotaSnap.NextAllowedAt != nil {
		t.Fatal("test setup: durable quota row must be absent when only the latch is set")
	}
	planFromResolve(t, svc, jobs, id)
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want the keyless tier attempted", adapter.calls)
	}
	if !resolver.AnonymousCredentials(adapter.capturedCtx[0]) {
		t.Fatal("resolve must run anonymously when keyed is latched process-locally only")
	}
}

func TestOrdinaryGateNeverAnonymous(t *testing.T) {
	svc, jobs, adapter, id := fallbackService(t, "wr_ordinary_gate", config.SourceOpenAlex)
	ctx := context.Background()
	keyed := svc.Config.SourcePolicy(config.SourceOpenAlex)
	// An ordinary retry gate under the bare source name says nothing about the
	// keyed identity's daily quota, so it must not switch credentials.
	if err := svc.Budgets.Defer(ctx, config.SourceOpenAlex, keyed, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := planFromResolve(t, svc, jobs, id)
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want none: the keyed identity is gated and no fallback is authorized", adapter.calls)
	}
	if plan.ClosedSourceGates != 1 {
		t.Fatalf("ClosedSourceGates = %d, want the ordinary gate recorded", plan.ClosedSourceGates)
	}
}

func TestAdvisoryThrottleSkipsAdapter(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.RetryDelay = time.Millisecond
	svc.Budgets = budget.New(jobs.S)
	// A refill slower than budget.MaxInlineWait is what makes the refusal an
	// advisory deferral rather than a brief inline wait the caller absorbs.
	keyed := config.Source{Enabled: true, APIKey: "private-key", RatePerSec: 0.1, Burst: 1}
	svc.Config.Sources[config.SourceOpenAlex] = keyed
	adapter := &contextResolver{name: config.SourceOpenAlex}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: keyed}}
	// Spend the source-wide token bucket, so this process's own throttle is the
	// only thing refusing the pass.
	if _, err := svc.Budgets.AcquireAny(ctx, config.SourceOpenAlex, []config.Source{keyed}, 0); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, doiRequest("wr_advisory_skip"))
	if err != nil {
		t.Fatal(err)
	}
	plan := planFromResolve(t, svc, jobs, id)
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want none while the local throttle is spent", adapter.calls)
	}
	if plan.AdvisoryBackoffs != 1 || plan.SourcesCalled != 0 {
		t.Fatalf("plan = %+v, want one advisory backoff and no charged call", plan)
	}
}

// Only OpenAlex has a keyless tier worth using. A keyed Crossref call stays
// keyed — and it must actually run, or the assertion would prove nothing.
func TestCrossrefNeverAnonymous(t *testing.T) {
	svc, jobs, adapter, id := fallbackService(t, "wr_crossref_keyed", config.SourceCrossrefMetadata)
	planFromResolve(t, svc, jobs, id)
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want exactly one", adapter.calls)
	}
	if resolver.AnonymousCredentials(adapter.capturedCtx[0]) {
		t.Fatal("a keyed Crossref call was marked anonymous; only OpenAlex has a keyless tier")
	}
}

// A candidate's bearer URL is a third-party file host, not the OpenAlex API:
// that site keeps reserving and deferring under the bare source name.
func TestCandidateDownloadStaysBareSource(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.RetryDelay = time.Millisecond
	svc.Budgets = budget.New(jobs.S)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	svc.Config.Sources[config.SourceOpenAlex] = keyed
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: config.SourceOpenAlex, cands: []resolver.Candidate{{
		Source: config.SourceOpenAlex, URL: "https://files.example/article.pdf", Version: resolver.VersionPublished,
		AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown", Direct: true, IdentityConfidence: 1,
	}}}, Policy: keyed}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, &fetch.Error{Class: fetch.ClassRetryable, HTTPStatus: 503, Msg: "host busy"}
	}
	svc.Validate = passValidation()
	id, err := svc.Submit(ctx, doiRequest("wr_candidate_bare"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	bare, err := svc.Budgets.Snapshot(ctx, config.SourceOpenAlex, keyed)
	if err != nil {
		t.Fatal(err)
	}
	if bare.NextAllowedAt == nil {
		t.Fatalf("bare-source snapshot = %+v, want the candidate retry deferred here", bare)
	}
	quota, err := svc.Budgets.Snapshot(ctx, config.SourceOpenAlex+"_quota", keyed)
	if err != nil {
		t.Fatal(err)
	}
	if quota.NextAllowedAt != nil {
		t.Fatalf("quota snapshot = %+v, want untouched: a file host's failure is not a quota signal", quota)
	}
	parked, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if parked.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait after a retryable candidate failure", parked.State)
	}
}

// A sibling hop that made no request must not be charged for one.
func TestSiblingNoSearchBasisUncharged(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	sibling := &siblingFake{name: "fixture", err: resolver.ErrNoSearchBasis}
	svc.Resolvers = []ResolverEntry{{Adapter: sibling, Policy: config.Source{Enabled: true}}}
	id, err := svc.Submit(ctx, doiRequest("wr_no_search_basis"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, plan := svc.resolveSiblings(ctx, row, true)
	if sibling.siblings != 1 {
		t.Fatalf("sibling calls = %d, want one", sibling.siblings)
	}
	if plan.SourcesCalled != 0 {
		t.Fatalf("SourcesCalled = %d, want 0: the hop made no request", plan.SourcesCalled)
	}
	var outcome, detail string
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT outcome, detail FROM attempts WHERE job_id = ? ORDER BY id DESC LIMIT 1`, id,
	).Scan(&outcome, &detail); err != nil {
		t.Fatal(err)
	}
	if detail != "no_search_basis" || outcome != "success" {
		t.Fatalf("attempt = %q/%q, want success/no_search_basis: nothing failed and nothing was spent", outcome, detail)
	}
}

// End to end across the observer and the fallback: one response reporting a
// nearly-spent daily budget must refuse the NEXT keyed request, not merely write
// a row somewhere.
func TestFloorPacingRefusesNextKeyedRequest(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	svc.Budgets = budget.New(jobs.S)
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	svc.Config.Sources[config.SourceOpenAlex] = keyed

	inner := &floorClient{}
	observer, err := newFloorObserver(svc.Budgets, keyed, inner)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := svc.Budgets.AcquireAny(ctx, config.SourceOpenAlex, acquirePolicies(config.SourceOpenAlex, keyed), 0)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.APIKey != keyed.APIKey {
		t.Fatalf("first admission = %+v, want the keyed identity", chosen)
	}
	if _, err := observer.Do(floorProbe(t, "https://api.openalex.org/works?api_key=private-key")); err != nil {
		t.Fatal(err)
	}
	if inner.keyed != 1 {
		t.Fatalf("keyed requests = %d, want one", inner.keyed)
	}
	next, err := svc.Budgets.AcquireAny(ctx, config.SourceOpenAlex, acquirePolicies(config.SourceOpenAlex, keyed), 0)
	if err != nil {
		t.Fatalf("next admission = %v, want the keyless identity admitted", err)
	}
	if next.APIKey != "" {
		t.Fatalf("next admission = %+v, want the anonymous identity after the floor fired", next)
	}
	if inner.keyed != 1 {
		t.Fatalf("keyed requests = %d, want no second keyed request past the floor", inner.keyed)
	}
}

// floorClient counts keyed requests and answers with the daily-budget headers
// OpenAlex sends on every response, reporting a nearly-spent budget.
type floorClient struct{ keyed int }

func (c *floorClient) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Query().Get("api_key") != "" {
		c.keyed++
	}
	header := make(http.Header)
	header.Set("X-RateLimit-Limit", "10000")
	header.Set("X-RateLimit-Remaining", "400")
	header.Set("X-RateLimit-Reset", "3600")
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: http.NoBody}, nil
}

func newFloorObserver(budgets *budget.Manager, keyed config.Source, inner *floorClient) (*sourcegate.Observer, error) {
	return sourcegate.NewObserver(budgets, budgets, config.SourceOpenAlex, keyed, inner)
}

func floorProbe(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return (&http.Request{Method: http.MethodGet, URL: parsed}).WithContext(context.Background())
}

// The ten-credit fuzzy search must not run while an ordinary temporary retry
// is still pending: the primary candidates deserve that attempt first, and the
// resolve() path used to pay for a search on every one of those passes.
func TestFuzzySiblingSearchWaitsForTheBoundary(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	sibling := &siblingFake{name: "fixture"}
	svc.Resolvers = []ResolverEntry{{Adapter: sibling, Policy: config.Source{Enabled: true}}}
	id, err := svc.Submit(ctx, doiRequest("wr_sibling_boundary"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, plan := svc.resolveSiblings(ctx, row, false); plan.SourcesCalled != 0 {
		t.Fatalf("SourcesCalled = %d, want 0 away from the boundary", plan.SourcesCalled)
	}
	if sibling.siblings != 0 {
		t.Fatalf("sibling calls = %d, want none while an ordinary retry remains", sibling.siblings)
	}
	if _, plan := svc.resolveSiblings(ctx, row, true); plan.SourcesCalled != 1 {
		t.Fatalf("SourcesCalled = %d, want 1 at the boundary", plan.SourcesCalled)
	}
	if sibling.siblings != 1 {
		t.Fatalf("sibling calls = %d, want exactly one at the boundary", sibling.siblings)
	}
}

// A completed search is a fact about the provider's index, so the identical
// question is never paid for twice — but a changed bibliography is a different
// question and buys exactly one more search.
func TestFuzzySiblingSearchRunsOncePerBasis(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	sibling := &siblingFake{name: "fixture"}
	svc.Resolvers = []ResolverEntry{{Adapter: sibling, Policy: config.Source{Enabled: true}}}
	id, err := svc.Submit(ctx, doiRequest("wr_sibling_basis"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for pass := range 3 {
		if _, plan := svc.resolveSiblings(ctx, row, true); pass > 0 && plan.SourcesCalled != 0 {
			t.Fatalf("pass %d charged %d sources, want 0 for a basis already searched", pass, plan.SourcesCalled)
		}
	}
	if sibling.siblings != 1 {
		t.Fatalf("sibling calls = %d, want exactly one across three identical passes", sibling.siblings)
	}

	enriched := *row
	enriched.Work.Title = "a materially different title"
	if _, plan := svc.resolveSiblings(ctx, &enriched, true); plan.SourcesCalled != 1 {
		t.Fatalf("SourcesCalled = %d, want 1: a new basis is a new question", plan.SourcesCalled)
	}
	if sibling.siblings != 2 {
		t.Fatalf("sibling calls = %d, want a second search after the basis changed", sibling.siblings)
	}
}

// A transport failure records nothing, so the question stays askable.
func TestFailedFuzzySiblingSearchStaysRetryable(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	sibling := &siblingFake{name: "fixture", err: errors.New("connection reset")}
	svc.Resolvers = []ResolverEntry{{Adapter: sibling, Policy: config.Source{Enabled: true}}}
	id, err := svc.Submit(ctx, doiRequest("wr_sibling_failed"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	svc.resolveSiblings(ctx, row, true)
	svc.resolveSiblings(ctx, row, true)
	if sibling.siblings != 2 {
		t.Fatalf("sibling calls = %d, want 2: a failed search suppresses nothing", sibling.siblings)
	}
}

// The retry budget is the only bound on provider spend for a job whose
// candidates are all dead, so an unreadable history must not authorize more
// paid passes.
func TestRetryBudgetFailsClosedOnUnreadableHistory(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest("wr_history_unreadable"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.retryBudgetExhausted(ctx, id) {
		t.Fatal("a fresh job has budget remaining")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !svc.retryBudgetExhausted(cancelled, id) {
		t.Fatal("an unreadable history must fail closed, not authorize another paid pass")
	}
}

// The same "unknown" that settles a job must NOT authorize the ten-credit
// search: exhaustion is read in two opposite senses, and only a proven fact is
// a spend permit.
func TestUnreadableHistoryIsNoPermitForTheExpensiveSearch(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest("wr_permit_unknown"))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !svc.retryBudgetExhausted(cancelled, id) {
		t.Fatal("liveness reading must fail closed on an unreadable history")
	}
	exhausted, err := svc.retryBudgetExhaustedProven(cancelled, id)
	if err == nil {
		t.Fatal("the proven form must surface the read failure instead of folding it into the verdict")
	}
	if exhausted {
		t.Fatal("an unreadable history proves nothing and must not permit the expensive search")
	}
}

// Jobs.Events decodes each detail with `_ = json.Unmarshal(...)`, so a corrupt
// row yields a nil detail rather than an error. A marker of the right kind is
// proof a search happened; an illegible one must not buy the ten-credit query
// again, least of all when storage is already misbehaving.
func TestUnreadableSiblingMarkerFailsClosed(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	sibling := &siblingFake{name: "fixture"}
	svc.Resolvers = []ResolverEntry{{Adapter: sibling, Policy: config.Source{Enabled: true}}}
	id, err := svc.Submit(ctx, doiRequest("wr_marker_corrupt"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
		id, svc.Now().UTC().Format(time.RFC3339Nano), siblingSearchEventKind, `{"basis":`,
	); err != nil {
		t.Fatal(err)
	}
	if _, plan := svc.resolveSiblings(ctx, row, true); plan.SourcesCalled != 0 {
		t.Fatalf("SourcesCalled = %d, want 0: an illegible marker must not buy another search", plan.SourcesCalled)
	}
	if sibling.siblings != 0 {
		t.Fatalf("sibling calls = %d, want none against an unreadable marker", sibling.siblings)
	}
}
