// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
)

func cutoverRequest(id string, withDOI bool) protocol.WorkRequest {
	req := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     id,
		Title:         "A bounded test work",
		Authors:       []string{"A. Researcher"},
		Year:          2026,
	}
	if withDOI {
		req.Identifiers = &protocol.Identifiers{DOI: "10.1000/cutover-test"}
	}
	return req
}

func cutoverDecisionForJob(t *testing.T, jobs *job.Store, id string) job.InstitutionCutoverDecision {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := job.LatestInstitutionCutoverDecision(events)
	if !ok {
		t.Fatalf("job %s has no valid cutover decision: %#v", id, events)
	}
	return decision
}

func processCutoverJob(t *testing.T, svc *Service, jobs *job.Store, request protocol.WorkRequest) (string, *job.Row) {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.ClaimNext(ctx, "cutover-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.ID != id {
		t.Fatalf("claimed row = %#v, want %s", row, id)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return id, got
}

func cutoverDecisionCount(t *testing.T, jobs *job.Store, id string) int {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event["kind"] != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if _, ok := job.ParseInstitutionCutoverDecision(detail); ok {
			count++
		}
	}
	return count
}

func TestInstitutionCutoverDecisionVocabularyAndPrivacy(t *testing.T) {
	values := []job.InstitutionCutoverBlocker{
		job.InstitutionCutoverBlockerNone,
		job.InstitutionCutoverBlockerSourceGateOnly,
		job.InstitutionCutoverBlockerLiveSourceRemaining,
		job.InstitutionCutoverBlockerTransientRetryRemaining,
		job.InstitutionCutoverBlockerNoLegalRoute,
		job.InstitutionCutoverBlockerPolicyGate,
		job.InstitutionCutoverBlockerIdentifierGate,
	}
	for _, want := range values {
		if got := job.NormalizeInstitutionCutoverBlocker(string(want)); got != want {
			t.Fatalf("normalize(%q) = %q", want, got)
		}
		detail := job.CutoverDecisionDetail(job.InstitutionCutoverDecision{Blocker: want})
		got, ok := job.ParseInstitutionCutoverDecision(detail)
		if !ok || got.Blocker != want || got.CanaryReadyRouteExists {
			t.Fatalf("round trip %q = %#v, %v", want, got, ok)
		}
	}
	if got := job.NormalizeInstitutionCutoverBlocker("provider_url"); got != "" {
		t.Fatalf("unknown blocker normalized to %q", got)
	}
	if _, ok := job.ParseInstitutionCutoverDecision(map[string]any{
		job.InstitutionCutoverBlockerKey: "source_gate_only",
	}); ok {
		t.Fatal("parser accepted decision with omitted canary flag")
	}

	svc, jobs := newTestService(t)
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("not reached")
	}
	svc.Validate = passValidation()
	id, got := processCutoverJob(t, svc, jobs, cutoverRequest("cutover_privacy", true))
	if got.State != job.StateUnavailable {
		t.Fatalf("state = %s, want unavailable", got.State)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if _, ok := job.ParseInstitutionCutoverDecision(detail); !ok {
			continue
		}
		encoded := strings.ToLower(detail[job.InstitutionCutoverBlockerKey].(string))
		for _, forbidden := range []string{"https://", "file://", "doi", "10.1000", "provider", "/"} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("cutover detail leaked %q: %#v", forbidden, detail)
			}
		}
		if detail[job.CanaryReadyRouteExistsKey] != false {
			t.Fatalf("phase-0 canary flag = %#v, want false", detail[job.CanaryReadyRouteExistsKey])
		}
	}
}

func TestLatestCutoverDecisionRetryRequestedStartsNewEpoch(t *testing.T) {
	detail := job.CutoverDecisionDetail(job.InstitutionCutoverDecision{
		Blocker:                job.InstitutionCutoverBlockerSourceGateOnly,
		CanaryReadyRouteExists: false,
	})
	events := []map[string]any{
		{"kind": "job.transition", "detail": detail},
		{"kind": "job.retry_requested", "detail": map[string]any{"reason": "operator_retry"}},
	}
	if _, ok := job.LatestInstitutionCutoverDecision(events); ok {
		t.Fatal("retry_requested must clear the prior decision epoch")
	}
}

func TestInstitutionCutoverDecisionBranches(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*Service, *job.Store)
		request protocol.WorkRequest
		state   string
		blocker job.InstitutionCutoverBlocker
		action  string
	}{
		{
			name: "source gate only",
			setup: func(svc *Service, jobs *job.Store) {
				svc.Budgets = budget.New(jobs.S)
				if err := svc.Budgets.Defer(context.Background(), "fixture", config.Source{Enabled: true}, time.Now().UTC().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_source_gate", true), state: job.StateRetryWait,
			blocker: job.InstitutionCutoverBlockerSourceGateOnly,
		},
		{
			name: "transient retry",
			setup: func(svc *Service, _ *job.Store) {
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", err: &resolver.TemporaryError{Err: errors.New("temporary")}}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_transient", true), state: job.StateRetryWait,
			blocker: job.InstitutionCutoverBlockerTransientRetryRemaining,
		},
		{
			name: "mixed temporary and gate",
			setup: func(svc *Service, jobs *job.Store) {
				svc.Budgets = budget.New(jobs.S)
				if err := svc.Budgets.Defer(context.Background(), "gated", config.Source{Enabled: true}, time.Now().UTC().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				svc.Config.Sources["gated"] = config.Source{Enabled: true}
				svc.Config.Sources["flaky"] = config.Source{Enabled: true}
				svc.Resolvers = []ResolverEntry{
					{Adapter: &fakeResolver{name: "gated"}, Policy: config.Source{Enabled: true}},
					{Adapter: &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: errors.New("temporary")}}, Policy: config.Source{Enabled: true}},
				}
			},
			request: cutoverRequest("cutover_mixed", true), state: job.StateRetryWait,
			blocker: job.InstitutionCutoverBlockerTransientRetryRemaining,
		},
		{
			name: "live source remaining",
			setup: func(svc *Service, _ *job.Store) {
				svc.Config.AccessMode = config.ModeDelegated
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{Source: "fixture", URL: "https://example.test/landing", Version: resolver.VersionPublished, ReuseLicense: "unknown", AccessBasis: resolver.AccessOpen, Direct: false, IdentityConfidence: 1}}}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_live_source", true), state: job.StateAwaitingHuman,
			blocker: job.InstitutionCutoverBlockerLiveSourceRemaining, action: "openurl_handoff",
		},
		{
			name: "identifier gate",
			setup: func(svc *Service, _ *job.Store) {
				svc.Config.AccessMode = config.ModeAssisted
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_identifier", false), state: job.StateUnavailable,
			blocker: job.InstitutionCutoverBlockerIdentifierGate,
		},
		{
			name: "no legal route",
			setup: func(svc *Service, _ *job.Store) {
				svc.Config.AccessMode = config.ModeAssisted
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_no_route", true), state: job.StateUnavailable,
			blocker: job.InstitutionCutoverBlockerNoLegalRoute,
		},
		{
			name: "policy gate",
			setup: func(svc *Service, _ *job.Store) {
				svc.Config.Browser.OpenURLBase = "https://resolver.example/openurl"
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_policy", true), state: job.StateUnavailable,
			blocker: job.InstitutionCutoverBlockerPolicyGate, action: "openurl_available",
		},
		{
			name: "none on institutional handoff",
			setup: func(svc *Service, _ *job.Store) {
				svc.Config.AccessMode = config.ModeAssisted
				svc.Config.Browser.OpenURLBase = "https://resolver.example/openurl"
				svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture"}, Policy: config.Source{Enabled: true}}}
			},
			request: cutoverRequest("cutover_none", true), state: job.StateAwaitingHuman,
			blocker: job.InstitutionCutoverBlockerNone, action: "openurl_handoff",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, jobs := newTestService(t)
			fetches := 0
			svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
				fetches++
				return fetch.Result{}, errors.New("fetch should not be needed")
			}
			svc.Validate = passValidation()
			tc.setup(svc, jobs)
			id, got := processCutoverJob(t, svc, jobs, tc.request)
			if got.State != tc.state {
				t.Fatalf("state = %s, want %s", got.State, tc.state)
			}
			if decision := cutoverDecisionForJob(t, jobs, id); decision.Blocker != tc.blocker || decision.CanaryReadyRouteExists {
				t.Fatalf("decision = %#v, want blocker %q and false canary", decision, tc.blocker)
			}
			if got := cutoverDecisionCount(t, jobs, id); got != 1 {
				t.Fatalf("decision count = %d, want exactly one decisive epoch payload", got)
			}
			if tc.action != "" {
				actions, err := jobs.ListHumanActions(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				if len(actions) != 1 || actions[0].Kind != tc.action {
					t.Fatalf("actions = %#v, want one %s", actions, tc.action)
				}
			}
			resolverCalls := 0
			for _, entry := range svc.Resolvers {
				if fake, ok := entry.Adapter.(*fakeResolver); ok {
					resolverCalls += fake.calls
				}
			}
			if tc.name != "source gate only" && resolverCalls == 0 {
				t.Fatalf("resolver calls = %d, want branch to exercise resolver facts", resolverCalls)
			}
			if tc.name == "source gate only" || tc.name == "transient retry" || tc.name == "mixed temporary and gate" {
				if fetches != 0 {
					t.Fatalf("fetches = %d, want zero on resolver decision", fetches)
				}
			}
		})
	}
}

func TestInstitutionCutoverExhaustedGateGetsOneWait(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Budgets = budget.New(jobs.S)
	svc.RetryDelay = time.Millisecond
	if err := svc.Budgets.Defer(context.Background(), "gated", config.Source{Enabled: true}, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	svc.Config.Sources["gated"] = config.Source{Enabled: true}
	svc.Config.Sources["flaky"] = config.Source{Enabled: true}
	flaky := &fakeResolver{name: "flaky", err: &resolver.TemporaryError{Err: errors.New("temporary")}}
	svc.Resolvers = []ResolverEntry{
		{Adapter: &fakeResolver{name: "gated"}, Policy: config.Source{Enabled: true}},
		{Adapter: flaky, Policy: config.Source{Enabled: true}},
	}
	fetches := 0
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		fetches++
		return fetch.Result{}, errors.New("unexpected fetch")
	}
	svc.Validate = passValidation()
	id, err := svc.Submit(context.Background(), cutoverRequest("cutover_exhausted_gate", true))
	if err != nil {
		t.Fatal(err)
	}
	for range maxRetryAttempts + 2 {
		row, err := jobs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Terminal(row.State) {
			break
		}
		if err := svc.Process(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !job.Terminal(row.State) {
		t.Fatalf("state = %s, want terminal after exhausted gate", row.State)
	}
	decision := cutoverDecisionForJob(t, jobs, id)
	if decision.Blocker != job.InstitutionCutoverBlockerNoLegalRoute {
		t.Fatalf("terminal decision = %#v, want no_legal_route", decision)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	gateWaits := 0
	exhaustedGateDecisions := 0
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if detail["retry_kind"] == retryKindExhaustedGate {
			gateWaits++
			exhaustedGateDecisions++
			decision, ok := job.ParseInstitutionCutoverDecision(detail)
			if !ok {
				t.Fatalf("exhausted gate transition has invalid decision: %#v", detail)
			}
			if decision.Blocker != job.InstitutionCutoverBlockerSourceGateOnly {
				t.Fatalf("exhausted gate decision = %#v, want source_gate_only", decision)
			}
			if decision.CanaryReadyRouteExists {
				t.Fatalf("exhausted gate canary flag = true, want false")
			}
		}
	}
	if gateWaits != 1 {
		t.Fatalf("exhausted gate waits = %d, want one", gateWaits)
	}
	if exhaustedGateDecisions != 1 {
		t.Fatalf("exhausted gate decisions = %d, want one", exhaustedGateDecisions)
	}
	if fetches != 0 || flaky.calls == 0 {
		t.Fatalf("counters fetch=%d flaky_resolver=%d, want no fetch and resolver attempts", fetches, flaky.calls)
	}
}

func TestInstitutionCutoverDeliveryTransitionCarriesDecision(t *testing.T) {
	svc, jobs := newTestService(t)
	id, err := svc.Submit(context.Background(), cutoverRequest("cutover_delivery_detail", true))
	if err != nil {
		t.Fatal(err)
	}
	decision := job.InstitutionCutoverDecision{
		Blocker:                job.InstitutionCutoverBlockerNone,
		CanaryReadyRouteExists: false,
	}
	ctx := withCutoverDecision(context.Background(), decision)
	detail := deliveryCutoverDetail(ctx, map[string]any{
		"reason":              "document_delivery_pending",
		"delivery_request_id": int64(7),
		"provider_reference":  "opaque-reference",
	})
	if parsed, ok := job.ParseInstitutionCutoverDecision(detail); !ok || parsed.Blocker != job.InstitutionCutoverBlockerNone {
		t.Fatalf("non-retry delivery detail = %#v, want blocker none", detail)
	}
	retryDetail := deliveryRetryCutoverDetail(ctx, map[string]any{
		"reason":              "resolver_temporarily_unavailable",
		"delivery_request_id": int64(7),
	})
	if parsed, ok := job.ParseInstitutionCutoverDecision(retryDetail); !ok || parsed.Blocker != job.InstitutionCutoverBlockerTransientRetryRemaining {
		t.Fatalf("pre-send delivery retry detail = %#v, want transient_retry_remaining", retryDetail)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateRetryWait, retryDetail, job.WithRetryAt(time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	decisionCount := 0
	for _, event := range events {
		if event["kind"] != "job.transition" {
			continue
		}
		eventDetail, _ := event["detail"].(map[string]any)
		if _, ok := job.ParseInstitutionCutoverDecision(eventDetail); ok {
			decisionCount++
		}
	}
	if decisionCount != 1 {
		t.Fatalf("delivery decision event count = %d, want one", decisionCount)
	}
}
