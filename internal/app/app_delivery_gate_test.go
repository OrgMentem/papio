// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Re-evaluating the delivery gate after an ambiguous POST must reconcile the
// request, and the monthly cap must count the attempt.

package app

import (
	"context"
	"net/http"
	"papio/internal/delivery"
	"papio/internal/job"
	"strings"
	"testing"
)

// TestAmbiguousPostCountsTowardTheCap proves an attempt whose response never
// arrived counts toward the monthly cap.
func TestAmbiguousPostCountsTowardTheCap(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_amb_count", "10.1000/amb-count")
	count, err := deliverySvc.SubmittedThisMonth(context.Background(), "default", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("SubmittedThisMonth = %d, want 1: the ambiguous attempt counts", count)
	}
}

// TestAmbiguousPostIsNeverPrefilledAfterAPolicyChange proves a row the
// institution may already hold is never presented to the human as a request
// to place by hand, whatever the gate now says.
func TestAmbiguousPostIsNeverPrefilledAfterAPolicyChange(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, _ := parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_amb_prefill", "10.1000/amb-prefill")

	// An operator flips the profile to prefill_only; the gate now says prefill.
	svc.Config.Browser.DocumentDelivery.SubmitPolicy = "prefill_only"
	if err := jobs.Transition(ctx, id, job.StateRetryWait, job.StateResolving, map[string]any{"reason": "test_redrive"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.deliveryRoute(ctx, row, row.State)
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != delivery.BranchReconcile {
		t.Fatalf("branch = %q, want reconcile for a possibly-live offered row", result.Branch)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Kind == job.ActionKindDocumentDelivery && strings.Contains(string(action.Detail), "prefill") {
			t.Fatalf("a prefill action was opened for a possibly-live request: %s", action.Detail)
		}
	}
	if p.posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want exactly the one ambiguous attempt", p.posts.Load())
	}
	if p.gets.Load() == 0 {
		t.Fatal("no token reconciliation read was made")
	}
	// The policy change moved the profile digest, so reconciliation hands the
	// row to a person — through the reconciliation action, never the prefill.
	after, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human on the reconciliation action", after.State)
	}
	var reconcile bool
	for _, action := range actions {
		if action.Kind == job.ActionKindDocumentDelivery && strings.Contains(string(action.Detail), "needs reconciliation") {
			reconcile = true
		}
	}
	if !reconcile {
		t.Fatalf("open actions = %+v, want a document_delivery reconciliation action", actions)
	}
}

// TestAmbiguousAttemptsCountTowardTheMonthlyCap proves a provider slowdown
// cannot walk past the cap: with a cap of one, the second job's POST must not
// be sent because the first ambiguous attempt already counts.
func TestAmbiguousAttemptsCountTowardTheMonthlyCap(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	svc.Config.Browser.DocumentDelivery.MonthlyRequestCap = 1
	ctx := context.Background()

	parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_cap_a", "10.1000/cap-a")
	second := processNewDeliveryJob(t, svc, jobs, "wr_cap_b", "10.1000/cap-b")

	if p.posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want 1: the cap is spent by the ambiguous first attempt", p.posts.Load())
	}
	got, err := jobs.Get(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("second job state = %q, want awaiting_human on the cap's prefill verdict", got.State)
	}
}

// TestCappedGateReconcilesAnAmbiguousRowInsteadOfPrefilling proves the cap
// verdict for the very job whose attempt spent it: the attempt counts itself,
// the gate says prefill, and the row must still go to token reconciliation
// (no prefill action, no second POST).
func TestCappedGateReconcilesAnAmbiguousRowInsteadOfPrefilling(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	svc.Config.Browser.DocumentDelivery.MonthlyRequestCap = 1
	ctx := context.Background()
	id, _ := parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_cap_self", "10.1000/cap-self")

	if err := jobs.Transition(ctx, id, job.StateRetryWait, job.StateResolving, map[string]any{"reason": "test_redrive"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.deliveryRoute(ctx, row, row.State)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Action != delivery.ActionPrefill || result.Branch != delivery.BranchReconcile {
		t.Fatalf("result = branch %q action %q, want the cap's prefill verdict routed to reconcile", result.Branch, result.Decision.Action)
	}
	if actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id}); err != nil || len(actions) != 0 {
		t.Fatalf("open actions = %+v, err = %v, want none: reconciliation is still bounded and automatic", actions, err)
	}
	if p.posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want 1", p.posts.Load())
	}
	after, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != job.StateRetryWait {
		t.Fatalf("job state = %q, want retry_wait", after.State)
	}
}
