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
}

func TestRefundEgressCreditsBackWithoutDroppingBelowProviderSeed(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000})
	ctx := context.Background()
	newReq := func() EgressRequest {
		req := openalexReq("key-a", 10)
		req.JobID = "job-1"
		req.Receipt = &EgressReceipt{}
		return req
	}
	for i := 0; i < BootstrapCreditCap/10; i++ {
		if err := m.CommitEgress(ctx, newReq()); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := m.CommitEgress(ctx, newReq()); err != nil {
		t.Fatalf("cold-start tranche: %v", err)
	}
	// Every failed attempt is credited back, so the day never fills.
	for i := 0; i < 200; i++ {
		req := newReq()
		if err := m.CommitEgress(ctx, req); err != nil {
			t.Fatalf("commit after refund %d: %v", i, err)
		}
		if err := m.RefundEgress(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.ObserveCreditsUsed(ctx, config.SourceOpenAlex, 30); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := m.RefundEgress(ctx, EgressRequest{Source: config.SourceOpenAlex, Credits: 10, Receipt: &EgressReceipt{Day: utcDay(m.now())}}); err != nil {
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

// A refund belongs to the day the commit charged. After midnight it must not
// remove credits that another request spent on the new day.
func TestRefundEgressCreditsTheChargedDayAcrossMidnight(t *testing.T) {
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 8, 15, 23, 59, 59, 0, time.UTC)
	m := New(s, WithNow(func() time.Time { return now }),
		WithCreditPolicy(func(string) CreditPolicy { return CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000} }))
	ctx := context.Background()
	charged := openalexReq("key-a", 10)
	charged.Receipt = &EgressReceipt{}
	if err := m.CommitEgress(ctx, charged); err != nil {
		t.Fatal(err)
	}
	oldDay := charged.Receipt.Day
	now = now.Add(2 * time.Second) // midnight passes while the request is in flight
	if err := m.CommitEgress(ctx, openalexReq("key-a", 5)); err != nil {
		t.Fatal(err)
	}
	if err := m.RefundEgress(ctx, charged); err != nil {
		t.Fatal(err)
	}
	read := func(day string) int {
		var n int
		if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
			config.SourceOpenAlex, day).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := read(oldDay); got != 0 {
		t.Fatalf("old day committed = %d, want the refund applied there", got)
	}
	if got := read(utcDay(now)); got != 5 {
		t.Fatalf("new day committed = %d, want 5 untouched", got)
	}
}

// The clamp must be persisted: re-clamping to now+24h on every read would
// keep a gate written 30 days ahead closed until its original deadline.
func TestSkewedGateOpensOnceTheCorrectedClockReachesTheHorizon(t *testing.T) {
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
	now = base // clock corrected
	var deferred *ErrDeferred
	if err := m.Acquire(ctx, "skewed", p, 0); !errors.As(err, &deferred) {
		t.Fatalf("Acquire = %v, want deferred while inside the horizon", err)
	}
	now = base.Add(MaxDeferHorizon + time.Minute)
	if err := m.Acquire(ctx, "skewed", p, 0); err != nil {
		t.Fatalf("Acquire after the horizon passed = %v, want the gate open", err)
	}
}
