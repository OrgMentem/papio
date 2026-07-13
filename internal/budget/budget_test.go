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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := m2.Acquire(ctx, "remote", config.Source{Enabled: true}, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("durable gate returned %v, want context deadline", err)
	}
	if err := m2.ClearDefer(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	if err := m2.Acquire(context.Background(), "remote", config.Source{Enabled: true}, 0); err != nil {
		t.Fatalf("cleared gate still blocked: %v", err)
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
