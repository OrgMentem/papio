// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

// A gate written while the wall clock ran ahead must not outlive the clock
// correction: nothing else clears a source gate.
func TestGateWrittenUnderForwardClockSkewIsClampedOnceClockIsCorrected(t *testing.T) {
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base.Add(30 * 24 * time.Hour)
	m := New(s, WithNow(func() time.Time { return now }))
	ctx := context.Background()
	p := config.Source{Enabled: true, RatePerSec: 100, Burst: 100}
	if err := m.Defer(ctx, "skewed", p, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = base // the clock is corrected

	err = m.Acquire(ctx, "skewed", p, 0)
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("Acquire = %v, want the gate to still bind within the horizon", err)
	}
	if limit := base.Add(MaxDeferHorizon); deferred.Until.After(limit) {
		t.Fatalf("gate until %s, want at most %s", deferred.Until, limit)
	}
	snap, err := m.Snapshot(ctx, "skewed", p)
	if err != nil || snap.NextAllowedAt == nil || snap.NextAllowedAt.After(base.Add(MaxDeferHorizon)) {
		t.Fatalf("snapshot = %+v, %v, want the gate clamped to the horizon", snap, err)
	}

	// A correct, shorter Retry-After now replaces the bogus stored gate.
	short := base.Add(time.Hour)
	if err := m.Defer(ctx, "skewed", p, short); err != nil {
		t.Fatal(err)
	}
	snap, err = m.Snapshot(ctx, "skewed", p)
	if err != nil || snap.NextAllowedAt == nil || !snap.NextAllowedAt.Equal(short) {
		t.Fatalf("snapshot = %+v, %v, want gate %s", snap, err, short)
	}
}

func TestRefundEgressCreditsBackWithoutDroppingBelowProviderSeed(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000})
	ctx := context.Background()
	req := openalexReq("key-a", 10)
	req.JobID = "job-1"
	for i := 0; i < BootstrapCreditCap/10; i++ {
		if err := m.CommitEgress(ctx, req); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := m.CommitEgress(ctx, req); err != nil {
		t.Fatalf("cold-start tranche: %v", err)
	}
	// Every failed attempt is credited back, so the day never fills.
	for i := 0; i < 200; i++ {
		if err := m.RefundEgress(ctx, req); err != nil {
			t.Fatal(err)
		}
		if err := m.CommitEgress(ctx, req); err != nil {
			t.Fatalf("commit after refund %d: %v", i, err)
		}
	}
	if err := m.ObserveCreditsUsed(ctx, config.SourceOpenAlex, 30); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := m.RefundEgress(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var committed int
	if err := m.db.QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, utcDay(m.now())).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed < 30 {
		t.Fatalf("credits_committed = %d, want it floored at the provider seed 30", committed)
	}
}
