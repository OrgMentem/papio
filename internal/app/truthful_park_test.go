// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

func stubProcessDeps(svc *Service) {
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		return fetch.Result{}, errors.New("unused")
	}
	svc.Validate = passValidation()
}

func TestRecordExceededUTCDaySetsGate(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	midnight := now.Add(24 * time.Hour)
	plan := retryPlan{}
	if !plan.observeBudgetRefusal(&budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowUTCDay,
		Until:  midnight,
	}) {
		t.Fatal("typed budget refusal was not observed")
	}
	if got := plan.schedule(now, time.Minute, false, false).at; !got.Equal(midnight) {
		t.Fatalf("retry wake = %v, want %v", got, midnight)
	}
}

func TestRecordExceededStickyUsesRetryCadence(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	plan := retryPlan{}
	plan.observeBudgetRefusal(&budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowSticky,
	})
	if plan.empty() {
		t.Fatal("sticky gate must keep the plan non-empty so the job parks")
	}
	if got, want := plan.schedule(now, time.Minute, false, false).at, now.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("retry wake = %v, want cadence floor %v", got, want)
	}
}

func TestRecordExceededDistinctWindowsDoNotCollapse(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	day := now.Add(24 * time.Hour)
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	plan := retryPlan{}
	plan.observeBudgetRefusal(&budget.ErrExceeded{Window: budget.WindowUTCDay, Until: day})
	plan.observeBudgetRefusal(&budget.ErrExceeded{Kind: budget.KindUSD, Window: budget.WindowMonth, Until: month})
	if got := plan.schedule(now, time.Minute, false, false).at; !got.Equal(day) {
		t.Fatalf("ordinary retry wake = %v, want earliest reset %v", got, day)
	}
	if got := plan.schedule(now, time.Minute, true, false).at; !got.Equal(month) {
		t.Fatalf("post-exhaustion wake = %v, want latest reset %v", got, month)
	}
}

func TestObserveBudgetRefusalRequiresTypedError(t *testing.T) {
	plan := retryPlan{}
	until := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	if !plan.observeBudgetRefusal(&budget.ErrExceeded{Window: budget.WindowUTCDay, Until: until}) {
		t.Fatal("observeBudgetRefusal must recognise ErrExceeded")
	}
	if got := plan.at(); !got.Equal(until) {
		t.Fatalf("retry wake = %v, want %v", got, until)
	}
}

func TestProcessParksMonthlyBudgetNotNoLegalCandidates(t *testing.T) {
	svc, jobs := newTestService(t)
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	svc.Budgets = budget.New(jobs.S, budget.WithNow(svc.Now))
	stubProcessDeps(svc)
	ctx := context.Background()

	policy := config.Source{Enabled: true, MaxCostUSD: 1}
	svc.Config.Sources["fixture"] = policy
	if err := svc.Budgets.Acquire(ctx, "fixture", policy, 0.6); err != nil {
		t.Fatal(err)
	}

	adapter := &fakeResolver{name: "fixture", cands: nil}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: policy, EstimatedCost: 0.6}}
	svc.MetadataEnrichers = nil
	svc.Discovery = nil

	id, err := jobs.CreateRequest(ctx, "wr_truth_month", work.Work{Title: "Monthly budget park"}, "", "", testPolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State == job.StateUnavailable && row.TerminalReason == string(job.TerminalReasonNoLegalCandidates) {
		t.Fatal("monthly budget exhaustion must park, not no_legal_candidates")
	}
	if row.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait", row.State)
	}
	wantMonth := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	retryAt, err := time.Parse(time.RFC3339Nano, row.RetryAt)
	if err != nil {
		t.Fatal(err)
	}
	if !retryAt.Equal(wantMonth) {
		t.Fatalf("retry_at = %v, want month boundary %v", retryAt, wantMonth)
	}
	detail := retryWaitDetail(t, jobs, id)
	if detail["retry_kind"] != retryKindSourceGate {
		t.Fatalf("retry_kind = %v, want %q", detail["retry_kind"], retryKindSourceGate)
	}
}

type lookupCreditDayExceeded struct{ until time.Time }

func (l lookupCreditDayExceeded) LookupWork(context.Context, string) (discovery.DiscoveredWork, error) {
	return discovery.DiscoveredWork{}, &budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowUTCDay,
		Until:  l.until,
	}
}

func TestProcessParksCreditBudgetUTC(t *testing.T) {
	svc, jobs := newTestService(t)
	now := time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC)
	midnight := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	stubProcessDeps(svc)
	svc.Discovery = lookupCreditDayExceeded{until: midnight}
	svc.Resolvers = nil
	svc.MetadataEnrichers = nil

	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, "wr_truth_credit", work.Work{DOI: "10.1234/credit.utc"}, "", "", testPolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State == job.StateUnavailable && row.TerminalReason == string(job.TerminalReasonNoLegalCandidates) {
		t.Fatal("credit fuse exhaustion must park, not no_legal_candidates")
	}
	if row.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait", row.State)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, row.RetryAt)
	if err != nil {
		t.Fatal(err)
	}
	if !retryAt.Equal(midnight) {
		t.Fatalf("retry_at = %v, want next UTC midnight %v", retryAt, midnight)
	}
}

type lookupStickyExceeded struct{}

func (lookupStickyExceeded) LookupWork(context.Context, string) (discovery.DiscoveredWork, error) {
	return discovery.DiscoveredWork{}, &budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowSticky,
	}
}

func TestEnrichmentParksStickyBudgetIdentically(t *testing.T) {
	svc, jobs := newTestService(t)
	now := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	svc.RetryDelay = 2 * time.Minute
	stubProcessDeps(svc)
	svc.Discovery = lookupStickyExceeded{}
	svc.Resolvers = nil
	svc.MetadataEnrichers = nil

	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, "wr_sticky_enrich", work.Work{DOI: "10.1234/sticky.enrich"}, "", "", testPolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait after sticky enrichment refusal", row.State)
	}
	firstRetry, err := time.Parse(time.RFC3339Nano, row.RetryAt)
	if err != nil {
		t.Fatal(err)
	}
	if !firstRetry.After(now) {
		t.Fatalf("retry_at = %v, want a future park time", firstRetry)
	}
	if firstRetry.Equal(time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("sticky park must not schedule at UTC midnight")
	}

	svc.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if err := jobs.Transition(ctx, id, job.StateRetryWait, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State == job.StateUnavailable {
		t.Fatal("sticky budget must not become unavailable after UTC rollover")
	}
	if row.State != job.StateRetryWait {
		t.Fatalf("state = %s, want retry_wait still parked on sticky closure", row.State)
	}
}
