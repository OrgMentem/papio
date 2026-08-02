// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return New(s)
}

func TestAcquireReservesMonthlyBudgetAtomically(t *testing.T) {
	m := testManager(t)
	p := config.Source{Enabled: true, MaxCostUSD: 1}
	if err := m.Acquire(context.Background(), "paid", p, 0.60); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "paid", p, 0.41); err == nil {
		t.Fatal("request crossing monthly limit was accepted")
	} else {
		var exceeded *ErrExceeded
		if !errors.As(err, &exceeded) {
			t.Fatalf("error = %T %v, want ErrExceeded", err, err)
		}
	}
	snap, err := m.Snapshot(context.Background(), "paid")
	if err != nil {
		t.Fatal(err)
	}
	if snap.RequestsInWindow != 1 || snap.SpentUSD != 0.60 {
		t.Fatalf("snapshot = %+v, rejected reservation mutated counters", snap)
	}
}

func TestAcquireTokenBucketHonorsBurstAndCancellation(t *testing.T) {
	m := testManager(t)
	p := config.Source{Enabled: true, RatePerSec: 1, Burst: 2}
	ctx := context.Background()
	if err := m.Acquire(ctx, "limited", p, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, "limited", p, 0); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := m.Acquire(blocked, "limited", p, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third immediate token = %v, want context deadline", err)
	}
	snap, _ := m.Snapshot(ctx, "limited")
	if snap.RequestsInWindow != 2 {
		t.Fatalf("cancelled wait reserved a request: %+v", snap)
	}
}

func TestDurableRetryAfterGateSurvivesManager(t *testing.T) {
	m := testManager(t)
	until := time.Now().UTC().Add(time.Hour)
	if err := m.Defer(context.Background(), "remote", until); err != nil {
		t.Fatal(err)
	}
	// A new manager over the same DB must still observe the gate.
	m2 := &Manager{db: m.db, limiters: make(map[string]*tokenBucket), now: time.Now}
	err := m2.Acquire(context.Background(), "remote", config.Source{Enabled: true}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("durable gate returned %T %v, want *ErrDeferred", err, err)
	}
	if deferred.Source != "remote" || deferred.Until.Sub(until).Abs() > time.Second {
		t.Fatalf("deferred = %+v, want the persisted gate for remote at %s", deferred, until)
	}
}

// A provider expresses an exhausted daily quota as a Retry-After pointing at
// the next reset — up to a day out. Acquire must hand that back rather than
// sleep, because the caller holds an acquisition worker whose lease heartbeat
// would keep the job claimed for the whole window and stall the queue behind
// it. Pins the bug that froze a 309-job cohort on three claimed rows.
func TestAcquireReturnsRatherThanSleepingOnADailyQuotaGate(t *testing.T) {
	m := testManager(t)
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if err := m.Defer(context.Background(), "openalex", midnight); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := m.Acquire(context.Background(), "openalex", config.Source{Enabled: true, RatePerSec: 2, Burst: 2}, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Acquire = %T %v, want *ErrDeferred", err, err)
	}
	if elapsed := time.Since(start); elapsed > MaxInlineWait {
		t.Fatalf("Acquire blocked %v on a far gate; it must return within %v", elapsed, MaxInlineWait)
	}
	// The gate must not be consumed: it is durable state the next caller reads.
	snap, err := m.Snapshot(context.Background(), "openalex")
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v; the deferral must survive a rejected Acquire", snap, err)
	}
	if snap.RequestsInWindow != 0 {
		t.Fatalf("requests_in_window = %d, want 0: a deferred call never reached the source", snap.RequestsInWindow)
	}
}

// The other half of the bound: a short Retry-After blip is still absorbed
// inline, so a two-second gate does not drop a resolver out of the chain.
func TestAcquireStillWaitsOutAShortGate(t *testing.T) {
	m := testManager(t)
	if err := m.Defer(context.Background(), "unpaywall", time.Now().UTC().Add(20*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "unpaywall", config.Source{Enabled: true}, 0); err != nil {
		t.Fatalf("Acquire on a short gate = %v, want it waited and succeeded", err)
	}
}

// A provider with a clock bug or a malformed Retry-After could otherwise park
// every job needing that source for as long as it asked, recoverable only by
// editing the database. Every real quota resets within a day.
func TestDeferIsClampedToADayEvenIfTheServerAsksForLonger(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	if err := m.Defer(ctx, "confused", time.Now().UTC().Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, "confused")
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if until := time.Until(*snap.NextAllowedAt); until > MaxDeferHorizon+time.Minute {
		t.Fatalf("gate is %v out, want no more than %v", until, MaxDeferHorizon)
	}
	// A gate inside the horizon is still honoured exactly.
	want := time.Now().UTC().Add(2 * time.Hour)
	if err := m.Defer(ctx, "polite", want); err != nil {
		t.Fatal(err)
	}
	snap, err = m.Snapshot(ctx, "polite")
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if snap.NextAllowedAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("gate = %s, want the server's %s untouched", snap.NextAllowedAt, want)
	}
}

func TestDeferNeverShortensExistingGate(t *testing.T) {
	m := testManager(t)
	later := time.Now().UTC().Add(2 * time.Hour)
	if err := m.Defer(context.Background(), "remote", later); err != nil {
		t.Fatal(err)
	}
	if err := m.Defer(context.Background(), "remote", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background(), "remote")
	if err != nil || snap.NextAllowedAt == nil {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if snap.NextAllowedAt.Before(later.Add(-time.Second)) {
		t.Fatalf("gate shortened: got %s, wanted at least %s", snap.NextAllowedAt, later)
	}
}

func TestDisabledAndInvalidRequestsFailBeforeMutation(t *testing.T) {
	m := testManager(t)
	if err := m.Acquire(context.Background(), "off", config.Source{}, 0); err == nil {
		t.Fatal("disabled source acquired")
	}
	if err := m.Acquire(context.Background(), "on", config.Source{Enabled: true}, -1); err == nil {
		t.Fatal("negative cost acquired")
	}
	if err := m.Acquire(context.Background(), "", config.Source{Enabled: true}, 0); err == nil {
		t.Fatal("empty source acquired")
	}
}
