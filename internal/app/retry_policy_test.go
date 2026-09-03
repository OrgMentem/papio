// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"testing"
	"time"

	"papio/internal/budget"
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
