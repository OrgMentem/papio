// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/sourcegate"
)

func TestRetryPolicyBudgetBoundaryKeepsOnlyPendingGate(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	var retryable retryPlan
	retryable.observeCandidateTemporary(now, time.Minute, time.Minute)
	if got := retryable.schedule(now, time.Minute, false, false); got.exhausted {
		t.Fatal("retryable candidate must schedule before the retry budget is exhausted")
	}
	if got := retryable.schedule(now, time.Minute, true, false); !got.exhausted {
		t.Fatal("retryable candidate must become terminal after the retry budget is exhausted")
	}

	var gated retryPlan
	gateAt := now.Add(time.Hour)
	gated.observeBudgetRefusal(&budget.ErrDeferred{Until: gateAt})
	got := gated.schedule(now, time.Minute, true, false)
	if got.exhausted {
		t.Fatal("a pending source gate must keep its one post-exhaustion wait")
	}
	if got.kind != retryKindExhaustedGate || !got.at.Equal(gateAt) {
		t.Fatalf("post-exhaustion schedule = %#v, want exhausted gate at %v", got, gateAt)
	}
}

func TestExplicitRetryRestoresTemporaryBudgetAndFinalGateWait(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_gate_%t", gated), func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
			gateAt := now.Add(6 * time.Hour)
			svc.Now = func() time.Time { return now }
			svc.RetryDelay = time.Second
			svc.Resolvers = []ResolverEntry{{
				Adapter: &fakeResolver{name: "fixture", err: &resolver.TemporaryError{Err: errors.New("upstream unavailable")}},
				Policy:  config.Source{Enabled: true},
			}}
			passes := maxRetryAttempts + 1
			if gated {
				passes++
				svc.Resolvers = append(svc.Resolvers, ResolverEntry{
					Adapter: &fakeResolver{name: "gated", err: &budget.ErrExceeded{
						Source: "gated", Kind: budget.KindCredits, Window: budget.WindowUTCDay, Until: gateAt,
					}},
					Policy: config.Source{Enabled: true},
				})
			}
			svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
				t.Fatal("no candidate should reach fetch")
				return fetch.Result{}, nil
			}
			svc.Validate = passValidation()
			id, err := svc.Submit(ctx, doiRequest("wr_explicit_retry_budget"))
			if err != nil {
				t.Fatal(err)
			}
			for epoch := range 3 {
				if epoch > 0 {
					if err := jobs.Retry(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				for pass := range passes {
					row, err := jobs.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if err := svc.Process(ctx, row); err != nil {
						t.Fatal(err)
					}
					after, err := jobs.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if pass == passes-1 {
						if after.State != job.StateUnavailable || after.TerminalReason != string(job.TerminalReasonTemporarySourceFailuresDidNotClear) {
							t.Fatalf("epoch %d final outcome = %s/%s, want unavailable/temporary_source_failures_did_not_clear",
								epoch, after.State, after.TerminalReason)
						}
						continue
					}
					if after.State != job.StateRetryWait {
						t.Fatalf("epoch %d pass %d state = %s, want retry_wait with a fresh bounded budget", epoch, pass, after.State)
					}
					wantKind := retryKindTemporary
					if gated && pass == maxRetryAttempts {
						wantKind = retryKindExhaustedGate
						at, err := time.Parse(time.RFC3339Nano, after.RetryAt)
						if err != nil || !at.Equal(gateAt) {
							t.Fatalf("epoch %d gate retry_at = %q (%v), want %v", epoch, after.RetryAt, err, gateAt)
						}
					}
					if detail := retryWaitDetail(t, jobs, id); detail["retry_kind"] != wantKind {
						t.Fatalf("epoch %d pass %d retry_kind = %v, want %s", epoch, pass, detail["retry_kind"], wantKind)
					}
				}
			}
		})
	}
}

func TestUnmarkedRetryDoesNotRestoreTemporaryBudgetOrGateWait(t *testing.T) {
	for _, reason := range []string{
		job.PublisherFirstFallbackReason,
		"operator_redrive",
		"explicit_publisher_retry",
		"explicit_open_spent_attempt",
		"adapter_upgraded",
		"explicit_retry",
	} {
		for _, gated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending_gate_%t", reason, gated), func(t *testing.T) {
				ctx := context.Background()
				svc, jobs := newTestService(t)
				now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
				svc.Now = func() time.Time { return now }
				svc.Resolvers = []ResolverEntry{{
					Adapter: &fakeResolver{name: "fixture", err: &resolver.TemporaryError{Err: errors.New("upstream unavailable")}},
					Policy:  config.Source{Enabled: true},
				}}
				passes := maxRetryAttempts
				if gated {
					passes++
					svc.Resolvers = append(svc.Resolvers, ResolverEntry{
						Adapter: &fakeResolver{name: "gated", err: &budget.ErrExceeded{
							Source: "gated", Kind: budget.KindCredits, Window: budget.WindowUTCDay, Until: now.Add(6 * time.Hour),
						}},
						Policy: config.Source{Enabled: true},
					})
				}
				svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
					t.Fatal("no candidate should reach fetch")
					return fetch.Result{}, nil
				}
				svc.Validate = passValidation()
				id, err := svc.Submit(ctx, doiRequest("wr_unmarked_retry_budget"))
				if err != nil {
					t.Fatal(err)
				}
				for range passes {
					row, err := jobs.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if err := svc.Process(ctx, row); err != nil {
						t.Fatal(err)
					}
				}
				// Retry reasons also describe automatic drive attempts. Without
				// Store.Retry's marker they must retain the acquisition budget.
				if err := jobs.RecordEvent(ctx, id, "job.retry_requested", map[string]any{
					"reason": reason,
				}); err != nil {
					t.Fatal(err)
				}
				row, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if row.State != job.StateRetryWait {
					t.Fatalf("state before unmarked retry pass = %s, want retry_wait", row.State)
				}
				if err := svc.Process(ctx, row); err != nil {
					t.Fatal(err)
				}
				after, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != job.StateUnavailable || after.TerminalReason != string(job.TerminalReasonTemporarySourceFailuresDidNotClear) {
					t.Fatalf("unmarked retry outcome = %s/%s, want exhausted unavailable outcome", after.State, after.TerminalReason)
				}
			})
		}
	}
}

func TestWireQuotaRefusalPreservesResetAndAttemptOutcome(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gateAt := now.Add(6 * time.Hour)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deferred", &budget.ErrDeferred{Source: "openalex", Until: gateAt, Quota: true}},
		{"latched", &sourcegate.ErrQuotaLatched{Source: "openalex", Until: gateAt}},
	} {
		for _, priorRetries := range []int{0, maxRetryAttempts} {
			t.Run(fmt.Sprintf("%s/prior_retries_%d", tc.name, priorRetries), func(t *testing.T) {
				ctx := context.Background()
				svc, jobs := newTestService(t)
				svc.Now = func() time.Time { return now }
				adapter := &fakeResolver{name: "openalex", err: &resolver.TemporaryError{Err: errors.New("upstream unavailable")}}
				svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
				svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
					t.Fatal("no candidate should reach fetch")
					return fetch.Result{}, nil
				}
				svc.Validate = passValidation()
				id, err := svc.Submit(ctx, doiRequest("wr_wire_quota_refusal"))
				if err != nil {
					t.Fatal(err)
				}
				for range priorRetries {
					row, err := jobs.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if err := svc.Process(ctx, row); err != nil {
						t.Fatal(err)
					}
				}
				adapter.err = fmt.Errorf("lookup: %w", &resolver.TemporaryError{Err: tc.err})
				row, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.Process(ctx, row); err != nil {
					t.Fatal(err)
				}
				after, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != job.StateRetryWait {
					t.Fatalf("quota refusal state = %s, want retry_wait", after.State)
				}
				at, err := time.Parse(time.RFC3339Nano, after.RetryAt)
				if err != nil || !at.Equal(gateAt) {
					t.Fatalf("quota refusal retry_at = %q (%v), want provider reset %v", after.RetryAt, err, gateAt)
				}
				wantKind := retryKindSourceGate
				if priorRetries == maxRetryAttempts {
					wantKind = retryKindExhaustedGate
				}
				if detail := retryWaitDetail(t, jobs, id); detail["retry_kind"] != wantKind {
					t.Fatalf("quota refusal retry_kind = %v, want %s", detail["retry_kind"], wantKind)
				}
				var outcome string
				if err := jobs.S.DB().QueryRowContext(ctx,
					`SELECT outcome FROM attempts WHERE job_id = ? AND stage = 'resolve' ORDER BY id DESC LIMIT 1`, id,
				).Scan(&outcome); err != nil {
					t.Fatal(err)
				}
				if outcome != "budget_blocked" {
					t.Fatalf("quota refusal attempt = %s, want budget_blocked", outcome)
				}
			})
		}
	}
}
