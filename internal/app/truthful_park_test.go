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
	midnight := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	plan := retryPlan{}
	plan.recordExceeded(&budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowUTCDay,
		Until:  midnight,
	})
	if plan.Gate != midnight {
		t.Fatalf("Gate = %v, want %v", plan.Gate, midnight)
	}
	if plan.LatestGate != midnight {
		t.Fatalf("LatestGate = %v, want %v", plan.LatestGate, midnight)
	}
	if plan.StickyBudgetGate {
		t.Fatal("UTC-day refusal must not mark sticky")
	}
}

func TestRecordExceededStickyLeavesGateZero(t *testing.T) {
	plan := retryPlan{}
	plan.recordExceeded(&budget.ErrExceeded{
		Source: config.SourceOpenAlex,
		Kind:   budget.KindCredits,
		Window: budget.WindowSticky,
	})
	if !plan.StickyBudgetGate {
		t.Fatal("sticky refusal must set StickyBudgetGate")
	}
	if !plan.Gate.IsZero() || !plan.LatestGate.IsZero() {
		t.Fatalf("sticky refusal must leave Gate/LatestGate zero: gate=%v latest=%v", plan.Gate, plan.LatestGate)
	}
	if plan.IsZero() {
		t.Fatal("sticky gate must keep plan non-zero so the job parks instead of settling")
	}
}

func TestRecordExceededDistinctWindowsDoNotCollapse(t *testing.T) {
	day := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	plan := retryPlan{}
	plan.recordExceeded(&budget.ErrExceeded{Window: budget.WindowUTCDay, Until: day})
	plan.recordExceeded(&budget.ErrExceeded{Kind: budget.KindUSD, Window: budget.WindowMonth, Until: month})
	if plan.Gate != day {
		t.Fatalf("Gate = %v, want earliest reset %v", plan.Gate, day)
	}
	if plan.LatestGate != month {
		t.Fatalf("LatestGate = %v, want latest reset %v", plan.LatestGate, month)
	}
}

func TestAbsorbBudgetRefusal_guardRequired(t *testing.T) {
	plan := retryPlan{}
	until := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	if !absorbBudgetRefusal(&plan, &budget.ErrExceeded{Window: budget.WindowUTCDay, Until: until}) {
		t.Fatal("absorbBudgetRefusal must recognise ErrExceeded")
	}
	if plan.Gate.IsZero() {
		t.Fatal("guard: recordExceeded must set Gate — disabling it leaves an empty plan")
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
