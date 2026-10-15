// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"fmt"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/resolver"
)

// A job's wake time obeys the same horizon as the source gate it waits on.
func TestEarlierRetryClampsRetryAfterToTheGateHorizon(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	for _, delay := range []time.Duration{365 * 24 * time.Hour, time.Duration(1<<63 - 1)} {
		plan := retryPlan{}
		got := plan.observeResolverTemporary(now, delay, 30*time.Second)
		if want := now.Add(budget.MaxDeferHorizon); !got.Equal(want) || !plan.at().Equal(want) {
			t.Fatalf("delay %v: wake = %v (plan %v), want %v", delay, got, plan.at(), want)
		}
	}
	plan := retryPlan{}
	if got := plan.observeResolverTemporary(now, 2*time.Hour, 30*time.Second); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("in-horizon delay = %v, want untouched", got)
	}
}

// A credit refusal that surfaces through an adapter after the source-call
// count was taken sent nothing: it parks on the refusal's reset and must not
// read as a provider outage.
func TestObserveWireRefusalParksOnResetWithoutCountingACall(t *testing.T) {
	until := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	exceeded := &budget.ErrExceeded{Kind: budget.KindCredits, Window: budget.WindowUTCDay, Until: until}
	err := &resolver.TemporaryError{Err: fmt.Errorf("openalex: request failed: %w", exceeded)}

	plan := retryPlan{}
	plan.observeSourceCalled()
	if !plan.observeWireRefusal(err) {
		t.Fatal("observeWireRefusal must recognise a wrapped ErrExceeded")
	}
	if plan.sourcesCalled != 0 || plan.temporaryResolvers != 0 {
		t.Fatalf("plan = %+v, want no source call and no temporary failure", plan)
	}
	if got := plan.at(); !got.Equal(until) {
		t.Fatalf("wake = %v, want the reset %v", got, until)
	}
	if plan.kind() != retryKindSourceGate {
		t.Fatalf("kind = %q, want %q", plan.kind(), retryKindSourceGate)
	}
	if plan.observeWireRefusal(&resolver.TemporaryError{Err: fmt.Errorf("boom")}) {
		t.Fatal("an ordinary temporary failure is not a wire refusal")
	}
}
