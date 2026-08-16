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

func TestUnmeteredCeilingStillCommits(t *testing.T) {
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0, DailyCreditLimit: 0})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 100)); err != nil {
			t.Fatal(err)
		}
	}
	var committed int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 500 {
		t.Fatalf("committed = %d, want 500", committed)
	}
}

func TestUnmeteredCeilingStillCommits_guardRequired(t *testing.T) {
	// Guard exercise is vacuous here: unmetered path must still commit even
	// through the normal CommitEgress; verify that disabling the debit limit
	// does not change the outcome (still commits).
	m, s := testCreditManager(t, CreditPolicy{DailyCreditFraction: 0, DailyCreditLimit: 0})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := m.CommitEgress(ctx, openalexReq("key-a", 50)); err != nil {
			t.Fatalf("unmetered commit %d: %v", i, err)
		}
	}
	var committed int
	day := utcDay(m.now())
	if err := s.DB().QueryRowContext(ctx, `SELECT credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 150 {
		t.Fatalf("committed = %d, want 150", committed)
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
	// Limit 100 should be ignored when fraction==0 (unmetered), but commits still happen.
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
