// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func testCreditManager(t *testing.T, policy CreditPolicy) (*Manager, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	fixed := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	m := New(s, WithNow(func() time.Time { return fixed }), WithCreditPolicy(func(string) CreditPolicy { return policy }))
	return m, s
}

func openalexReq(identity string, credits int) EgressRequest {
	return EgressRequest{Source: config.SourceOpenAlex, Identity: identity, Credits: credits}
}

func TestFirstWriteOverLimitRefused(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 5})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 10, true); err != nil {
		t.Fatal(err)
	}
	err := m.CommitEgress(context.Background(), openalexReq("key-a", 10))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Kind != KindCredits {
		t.Fatalf("CommitEgress = %v, want KindCredits ErrExceeded", err)
	}
}

func TestFirstWriteOverLimit_guardRequired(t *testing.T) {
	old := egressTestHooks.enforceDebitLimit
	egressTestHooks.enforceDebitLimit = false
	t.Cleanup(func() { egressTestHooks.enforceDebitLimit = old })
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 5})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 10, true); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(context.Background(), openalexReq("key-a", 10)); err != nil {
		t.Fatal("with debit guard disabled, over-limit first write should succeed — proves TestFirstWriteOverLimitRefused exercises the guard")
	}
}

func TestConcurrentDebitsNeverExceedAllowance(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 1, DailyCreditLimit: 100})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 100, true); err != nil {
		t.Fatal(err)
	}
	const workers = 40
	var wg sync.WaitGroup
	var ok int64
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := m.CommitEgress(context.Background(), openalexReq("key-a", 3)); err == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if got := int(ok) * 3; got > 100 {
		t.Fatalf("committed %d credits, allowance 100", got)
	}
}

func TestCommitEgressRefusesGateInstalledAfterAcquire(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	p := config.Source{Enabled: true, APIKey: "secret"}
	if err := m.Acquire(context.Background(), config.SourceOpenAlex, p, 0); err != nil {
		t.Fatal(err)
	}
	far := m.now().UTC().Add(24 * time.Hour)
	if err := m.Defer(context.Background(), config.SourceOpenAlex, p, far); err != nil {
		t.Fatal(err)
	}
	err := m.CommitEgress(context.Background(), openalexReq(identityFor(p), 1))
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("CommitEgress = %v, want ErrDeferred after durable gate", err)
	}
}

func TestCommitEgressGateCheck_guardRequired(t *testing.T) {
	old := egressTestHooks.enforceEgressGates
	egressTestHooks.enforceEgressGates = false
	t.Cleanup(func() { egressTestHooks.enforceEgressGates = old })
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	p := config.Source{Enabled: true, APIKey: "secret"}
	far := m.now().UTC().Add(24 * time.Hour)
	if err := m.Defer(context.Background(), config.SourceOpenAlex, p, far); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(context.Background(), openalexReq(identityFor(p), 1)); err != nil {
		t.Fatal("with gate guard disabled, CommitEgress should succeed — proves gate check is load-bearing")
	}
}

func TestSourceWideAllowanceBoundsAllIdentities(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 1, DailyCreditLimit: 100})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 100, true); err != nil {
		t.Fatal(err)
	}
	keyed := identityFor(config.Source{APIKey: "k"})
	anon := identityFor(config.Source{})
	if err := m.CommitEgress(context.Background(), openalexReq(keyed, 60)); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(context.Background(), openalexReq(anon, 50)); err == nil {
		t.Fatal("anonymous egress exceeded shared allowance")
	}
	if err := m.CommitEgress(context.Background(), openalexReq(anon, 40)); err != nil {
		t.Fatalf("anonymous egress within shared allowance: %v", err)
	}
}

func TestObserveLimitLowersNeverRaises(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 5000})
	ctx := context.Background()
	if err := m.ObserveLimit(ctx, config.SourceOpenAlex, "key-a", 10_000, true); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveLimit(ctx, config.SourceOpenAlex, "anon", 1_000, false); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveLimit(ctx, config.SourceOpenAlex, "key-a", 1_000_000_000, true); err != nil {
		t.Fatal(err)
	}
	var denom int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT denominator FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&denom); err != nil {
		t.Fatal(err)
	}
	if denom != 10_000 {
		t.Fatalf("denominator = %d, want 10000", denom)
	}
}

func TestDriftCloseSurvivesManagerRestart(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	ctx := context.Background()
	if err := m.DriftClose(ctx, config.SourceOpenAlex, "cost drift"); err != nil {
		t.Fatal(err)
	}
	m2 := New(s, WithNow(m.now), WithCreditPolicy(func(string) CreditPolicy {
		return CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000}
	}))
	err := m2.CommitEgress(ctx, openalexReq("key-a", 1))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Window != WindowSticky || !exceeded.Until.IsZero() {
		t.Fatalf("after restart CommitEgress = %v, want sticky ErrExceeded with zero Until", err)
	}
}

// A drift or prepaid closure is source-wide and survives until acknowledged
// (ADR-0024 §8). It must outlive both UTC midnight and a daemon restart, and
// CreditStatus must keep reporting it.
func TestStickyClosureSurvivesMidnightAndRestart(t *testing.T) {
	policy := CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000}
	closers := map[string]func(context.Context, *Manager) error{
		"drift": func(ctx context.Context, m *Manager) error {
			return m.DriftClose(ctx, config.SourceOpenAlex, "cost drift")
		},
		"prepaid": func(ctx context.Context, m *Manager) error {
			if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 1.0); err != nil {
				return err
			}
			return m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 0.5)
		},
	}
	for name, closeSource := range closers {
		t.Run(name, func(t *testing.T) {
			m, s := testCreditManager(t, policy)
			clock := m.now()
			newManager := func() *Manager {
				return New(s, WithNow(func() time.Time { return clock }), WithCreditPolicy(func(string) CreditPolicy { return policy }))
			}
			original := newManager()
			ctx := context.Background()
			if err := closeSource(ctx, original); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(24 * time.Hour)
			assertClosed := func(label string, mgr *Manager) {
				t.Helper()
				err := mgr.CommitEgress(ctx, openalexReq("key-a", 1))
				var exceeded *ErrExceeded
				if !errors.As(err, &exceeded) || exceeded.Window != WindowSticky || !exceeded.Until.IsZero() {
					t.Fatalf("%s: CommitEgress = %v, want sticky ErrExceeded with zero Until", label, err)
				}
				status, err := mgr.CreditStatus(ctx, config.SourceOpenAlex)
				if err != nil {
					t.Fatal(err)
				}
				if status.DriftReason == "" || !status.Exhausted() {
					t.Fatalf("%s: CreditStatus = %+v, want the closure reported", label, status)
				}
				if status.Committed != 0 {
					t.Fatalf("%s: committed = %d, want no debit admitted", label, status.Committed)
				}
			}
			assertClosed("original manager across midnight", original)
			// A new Manager has no latch: it is a daemon restarted after midnight.
			assertClosed("restarted manager after midnight", newManager())
		})
	}
}

func TestBootstrapCapThenObserveCreditsUsedSeeds(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000})
	ctx := context.Background()
	for i := 0; i < BootstrapCreditCap; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
			t.Fatalf("bootstrap commit %d: %v", i, err)
		}
	}
	if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
		t.Fatalf("cold-start tranche after bootstrap: %v", err)
	}
	if err := m.CommitEgress(ctx, openalexReq("key-a", ColdStartCreditCap)); err == nil {
		t.Fatal("cold-start cap not enforced without denominator")
	}
	if err := m.ObserveCreditsUsed(ctx, config.SourceOpenAlex, 240); err != nil {
		t.Fatal(err)
	}
	var committed int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed < 240 {
		t.Fatalf("credits_committed = %d, want at least seeded 240", committed)
	}
}

func TestLatchQuotaRefusedAtCommit(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	until := m.now().UTC().Add(time.Hour)
	m.LatchQuota(config.SourceOpenAlex, "key-a", until)
	err := m.CommitEgress(context.Background(), openalexReq("key-a", 1))
	var deferred *ErrDeferred
	if !errors.As(err, &deferred) || !deferred.Quota {
		t.Fatalf("CommitEgress = %v, want quota ErrDeferred", err)
	}
}

// A process-local quota latch lifts at the provider's reset, not at process
// exit: a stale past instant read as "still latched" would close egress for the
// identity until the daemon restarts. This pins the real Manager's expiry; the
// sourcegate observer tests only exercise a fake floor's own copy of it.
func TestQuotaLatchExpiresAtItsReset(t *testing.T) {
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	m := New(s, WithNow(func() time.Time { return now }), WithCreditPolicy(func(string) CreditPolicy {
		return CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000}
	}))
	until := now.Add(time.Hour)
	m.LatchQuota(config.SourceOpenAlex, "key-a", until)

	got, latched := m.QuotaLatchedUntil(config.SourceOpenAlex, "key-a")
	if !latched || !got.Equal(until) {
		t.Fatalf("QuotaLatchedUntil = %v, %v, want latched until %v", got, latched, until)
	}
	now = until.Add(-time.Nanosecond)
	if _, latched := m.QuotaLatchedUntil(config.SourceOpenAlex, "key-a"); !latched {
		t.Fatal("latch lifted a nanosecond before the provider's reset")
	}
	// The reset instant itself is no longer latched: expiry is !until.After(now).
	now = until
	if _, latched := m.QuotaLatchedUntil(config.SourceOpenAlex, "key-a"); latched {
		t.Fatal("latch still held at the provider's reset instant")
	}
	m.latchMu.Lock()
	_, kept := m.quotaLatches[latchKey(config.SourceOpenAlex, "key-a")]
	m.latchMu.Unlock()
	if kept {
		t.Fatal("expired latch entry was not deleted")
	}

	// An expired latch does not refuse a commit.
	now = until.Add(time.Hour)
	if err := m.CommitEgress(context.Background(), openalexReq("key-a", 1)); err != nil {
		t.Fatalf("CommitEgress after the latch expired = %v, want admitted", err)
	}
}

func TestPrepaidDropTriggersDriftClose(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	ctx := context.Background()
	if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 1.0); err != nil {
		t.Fatal(err)
	}
	if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 0.5); err != nil {
		t.Fatal(err)
	}
	err := m.CommitEgress(ctx, openalexReq("key-a", 1))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Window != WindowSticky {
		t.Fatalf("CommitEgress = %v, want sticky refusal after prepaid drop", err)
	}
}

// An acknowledgement is the only thing that ends a sticky closure, whichever
// day recorded it and whichever Manager (a restarted daemon) reads it.
func TestAcknowledgeClosureReopensStickyClosure(t *testing.T) {
	policy := CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000}
	closers := map[string]func(context.Context, *Manager) error{
		"drift": func(ctx context.Context, m *Manager) error {
			return m.DriftClose(ctx, config.SourceOpenAlex, "cost drift")
		},
		"prepaid": func(ctx context.Context, m *Manager) error {
			if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 1.0); err != nil {
				return err
			}
			return m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 0.5)
		},
	}
	for name, closeSource := range closers {
		t.Run(name, func(t *testing.T) {
			m, s := testCreditManager(t, policy)
			clock := m.now()
			newManager := func() *Manager {
				return New(s, WithNow(func() time.Time { return clock }), WithCreditPolicy(func(string) CreditPolicy { return policy }))
			}
			original := newManager()
			ctx := context.Background()
			if err := closeSource(ctx, original); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(48 * time.Hour)
			if err := original.CommitEgress(ctx, openalexReq("key-a", 1)); err == nil {
				t.Fatal("closure did not survive to D+2")
			}

			cleared, err := original.AcknowledgeClosure(ctx, config.SourceOpenAlex)
			if err != nil || !cleared {
				t.Fatalf("AcknowledgeClosure = %v, %v; want cleared", cleared, err)
			}
			restarted := newManager()
			for label, mgr := range map[string]*Manager{"original": original, "restarted": restarted} {
				if err := mgr.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
					t.Fatalf("%s: CommitEgress after acknowledgement = %v", label, err)
				}
				status, err := mgr.CreditStatus(ctx, config.SourceOpenAlex)
				if err != nil {
					t.Fatal(err)
				}
				if status.DriftReason != "" {
					t.Fatalf("%s: CreditStatus still reports %q", label, status.DriftReason)
				}
			}
			var closedRows int
			if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM source_credit_fuse
				WHERE source = ? AND drift_closed_at IS NOT NULL`, config.SourceOpenAlex).Scan(&closedRows); err != nil {
				t.Fatal(err)
			}
			if closedRows != 0 {
				t.Fatalf("%d rows still carry a closure", closedRows)
			}

			cleared, err = restarted.AcknowledgeClosure(ctx, config.SourceOpenAlex)
			if err != nil || cleared {
				t.Fatalf("second AcknowledgeClosure = %v, %v; want cleared=false for an open source", cleared, err)
			}
		})
	}
}

// The prepaid baseline is dropped with the closure. Kept, it would close the
// source again at the next observation of the same UTC day.
func TestAcknowledgeClosureRebasesPrepaidBaseline(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	ctx := context.Background()
	for _, remaining := range []float64{1.0, 0.5} {
		if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, remaining); err != nil {
			t.Fatal(err)
		}
	}
	if cleared, err := m.AcknowledgeClosure(ctx, config.SourceOpenAlex); err != nil || !cleared {
		t.Fatalf("AcknowledgeClosure = %v, %v", cleared, err)
	}
	for _, remaining := range []float64{0.5, 0.5} {
		if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, remaining); err != nil {
			t.Fatal(err)
		}
		if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
			t.Fatalf("after observing %v: %v", remaining, err)
		}
	}
}

func TestAcknowledgeClosureRejectsBadSourcesAndClearsAMemoryOnlyLatch(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	ctx := context.Background()
	for _, source := range []string{"", "nonesuch"} {
		if _, err := m.AcknowledgeClosure(ctx, source); err == nil {
			t.Fatalf("AcknowledgeClosure(%q) succeeded, want an error", source)
		}
	}
	m.setDriftLatch(config.SourceOpenAlex, "persist failed")
	cleared, err := m.AcknowledgeClosure(ctx, config.SourceOpenAlex)
	if err != nil || !cleared {
		t.Fatalf("AcknowledgeClosure = %v, %v; want the in-process latch reported cleared", cleared, err)
	}
	if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
		t.Fatalf("CommitEgress after clearing the latch = %v", err)
	}
}

// A closure whose durable write failed leaves the baseline row open. The
// acknowledgement must drop that baseline so the same balance does not close
// the source again.
func TestAcknowledgeClosureRebasesBaselineOfMemoryOnlyClosure(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	ctx := context.Background()
	if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 1.0); err != nil {
		t.Fatal(err)
	}
	// State after a failed closure commit: latch set, row still open.
	m.setDriftLatch(config.SourceOpenAlex, "prepaid balance below starting observation")
	if cleared, err := m.AcknowledgeClosure(ctx, config.SourceOpenAlex); err != nil || !cleared {
		t.Fatalf("AcknowledgeClosure = %v, %v", cleared, err)
	}
	if err := m.ObservePrepaidRemaining(ctx, config.SourceOpenAlex, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
		t.Fatalf("CommitEgress after acknowledging a memory-only closure = %v, want open", err)
	}
}

func TestMonthlyErrExceededCarriesMonthWindow(t *testing.T) {
	m, _ := testCreditManager(t, CreditPolicy{})
	p := config.Source{Enabled: true, MaxCostUSD: 1}
	if err := m.Acquire(context.Background(), "paid", p, 0.6); err != nil {
		t.Fatal(err)
	}
	err := m.Acquire(context.Background(), "paid", p, 0.5)
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("error = %v, want ErrExceeded", err)
	}
	if exceeded.Kind != KindUSD || exceeded.Window != WindowMonth || exceeded.Until.IsZero() {
		t.Fatalf("ErrExceeded = %+v, want KindUSD WindowMonth with Until set", exceeded)
	}
}

func TestShippedDefaultConfigFiniteAllowanceRefusesOverLimit(t *testing.T) {
	// Shipped openalex default: daily_credit_fraction=0.5, daily_credit_limit=0 (no hard cap).
	// Pre-fix allowanceFor treated limit==0 as unmetered, shelving the fuse.
	cfg := config.Default()
	oa := cfg.Sources[config.SourceOpenAlex]
	if oa.DailyCreditFraction == 0 {
		t.Fatalf("shipped default fraction is 0 — fuse inert")
	}
	if oa.DailyCreditLimit != 0 {
		t.Fatalf("shipped default limit = %d, want 0 (no absolute override)", oa.DailyCreditLimit)
	}
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: oa.DailyCreditFraction, DailyCreditLimit: oa.DailyCreditLimit})
	ctx := context.Background()
	// No denominator yet -> allowance is BootstrapCreditCap (50).
	err := m.CommitEgress(ctx, openalexReq("key-a", BootstrapCreditCap+1))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) || exceeded.Kind != KindCredits || exceeded.Window != WindowUTCDay {
		t.Fatalf("CommitEgress = %v, want KindCredits ErrExceeded bounded by bootstrap cap", err)
	}
	if exceeded.Limit != float64(BootstrapCreditCap) {
		t.Fatalf("Limit = %v, want %d (bootstrap cap)", exceeded.Limit, BootstrapCreditCap)
	}
	// Within cap must succeed and commit.
	if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
		t.Fatalf("within-cap commit: %v", err)
	}
}

func TestShippedDefaultConfigFiniteAllowanceRefusesOverLimit_guardRequired(t *testing.T) {
	old := egressTestHooks.enforceDebitLimit
	egressTestHooks.enforceDebitLimit = false
	t.Cleanup(func() { egressTestHooks.enforceDebitLimit = old })
	cfg := config.Default()
	oa := cfg.Sources[config.SourceOpenAlex]
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: oa.DailyCreditFraction, DailyCreditLimit: oa.DailyCreditLimit})
	ctx := context.Background()
	if err := m.CommitEgress(ctx, openalexReq("key-a", BootstrapCreditCap+1)); err != nil {
		t.Fatal("with debit guard disabled, over-limit bootstrap write should succeed — proves default-config limit is load-bearing")
	}
}

func TestDailyCreditLimitClampsFractionProduct(t *testing.T) {
	// fraction 0.5 * 10_000 = 5000 but hard max 1000 must clamp.
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 10_000, true); err != nil {
		t.Fatal(err)
	}
	// 1000 within clamp ok, 1001 over.
	if err := m.CommitEgress(context.Background(), openalexReq("key-a", 1000)); err != nil {
		t.Fatalf("at clamp: %v", err)
	}
	err := m.CommitEgress(context.Background(), openalexReq("key-a", 1))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("want exceeded after clamp, got %v", err)
	}
	if int(exceeded.Limit) != 1000 {
		t.Fatalf("Limit = %v, want 1000", exceeded.Limit)
	}
}

func TestDailyCreditLimitClampsFractionProduct_guardRequired(t *testing.T) {
	old := egressTestHooks.enforceDebitLimit
	egressTestHooks.enforceDebitLimit = false
	t.Cleanup(func() { egressTestHooks.enforceDebitLimit = old })
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 1000})
	if err := m.ObserveLimit(context.Background(), config.SourceOpenAlex, "key-a", 10_000, true); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(context.Background(), openalexReq("key-a", 1000)); err != nil {
		t.Fatal(err)
	}
	// Without guard, the next debit past allowance would succeed.
	if err := m.CommitEgress(context.Background(), openalexReq("key-a", 10)); err != nil {
		t.Fatal("with clamp guard disabled, over-limit debit should succeed — proves clamp is load-bearing")
	}
}

func TestUnobservedDenominatorStillBoundsBootstrapThenColdStart(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 0})
	ctx := context.Background()
	// No denominator: first BootstrapCreditCap commits succeed (at bootstrap cap).
	for i := 0; i < BootstrapCreditCap; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
			t.Fatalf("bootstrap commit %d: %v", i, err)
		}
	}
	// Now committed==BootstrapCreditCap, so allowance becomes ColdStartCreditCap (500).
	// It is still bounded — just at the higher cold-start cap, not unmetered.
	err := m.CommitEgress(ctx, openalexReq("key-a", ColdStartCreditCap))
	var exceeded *ErrExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("over cold-start without denominator: got %v, want ErrExceeded", err)
	}
	if int(exceeded.Limit) != ColdStartCreditCap {
		t.Fatalf("Limit = %v, want %d (cold-start cap)", exceeded.Limit, ColdStartCreditCap)
	}
	// Within cold-start should still succeed.
	if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
		t.Fatalf("cold-start within-cap: %v", err)
	}
	var committed int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != BootstrapCreditCap+1 {
		t.Fatalf("committed = %d, want %d", committed, BootstrapCreditCap+1)
	}
}

func TestUnobservedDenominatorStillBoundsBootstrapThenColdStart_guardRequired(t *testing.T) {
	old := egressTestHooks.enforceDebitLimit
	egressTestHooks.enforceDebitLimit = false
	t.Cleanup(func() { egressTestHooks.enforceDebitLimit = old })
	m, _ := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 0})
	ctx := context.Background()
	for i := 0; i < BootstrapCreditCap; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 1)); err != nil {
			t.Fatalf("bootstrap commit %d: %v", i, err)
		}
	}
	if err := m.CommitEgress(ctx, openalexReq("key-a", ColdStartCreditCap)); err != nil {
		t.Fatal("with debit guard disabled, over-cold-start debit should succeed — proves cold-start bound is load-bearing")
	}
}

func TestDailyFractionZeroStillIncrementsCommitted(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0, DailyCreditLimit: 100})
	ctx := context.Background()
	// DailyCreditLimit is not part of the unmetered predicate (fraction==0 is),
	// so committing past the 100 limit proves the ceiling is off while every
	// request is still recorded. Disabling the debit guard cannot change an
	// unmetered commit, so this contract has no _guardRequired complement.
	for i := 0; i < 5; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 100)); err != nil {
			t.Fatalf("unmetered commit %d: %v", i, err)
		}
	}
	var committed int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 500 {
		t.Fatalf("committed = %d, want 500 (fraction 0 still increments)", committed)
	}
}
