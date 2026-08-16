// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package budget

import (
	"context"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func statusManager(t *testing.T, cfg config.Config, now time.Time) *Manager {
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
	return New(s, WithNow(func() time.Time { return now }), WithCreditPolicy(CreditPolicyFromConfig(cfg)))
}

func statusConfig(fraction float64, hard int) config.Config {
	cfg := config.Default()
	cfg.Sources[config.SourceOpenAlex] = config.Source{
		Enabled:             true,
		APIKey:              "key",
		DailyCreditFraction: fraction,
		DailyCreditLimit:    hard,
	}
	return cfg
}

// The status a diagnostic prints and the ceiling the fuse enforces must be the
// same number, derived from the same policy mapping. A separate reader that
// recomputed the allowance would be a second answer to "how much may this
// identity spend today" — the exact class of defect this whole change set keeps
// finding — so CreditStatus is asserted against the allowance CommitEgress
// actually refuses at.
func TestCreditStatusReportsTheCeilingCommitEgressEnforces(t *testing.T) {
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	m := statusManager(t, statusConfig(0.5, 0), now)
	ctx := context.Background()
	EgressTestDisableGates(t)

	// Seed the day's denominator the way an observed provider response does.
	if err := m.ObserveLimit(ctx, config.SourceOpenAlex, "key-abc", 1000, true); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := m.CommitEgress(ctx, EgressRequest{Source: config.SourceOpenAlex, Identity: "key-abc", Credits: 10}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	got, err := m.CreditStatus(ctx, config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if got.Committed != 30 {
		t.Fatalf("committed = %d, want the 30 credits actually paid for", got.Committed)
	}
	if got.Limit != 500 || got.Denominator != 1000 {
		t.Fatalf("limit/denominator = %d/%d, want 500 of a provider-reported 1000", got.Limit, got.Denominator)
	}
	if got.Unmetered || got.Exhausted() {
		t.Fatalf("status = %+v, want a live metered allowance", got)
	}
	if got.Day != "2026-08-16" {
		t.Fatalf("day = %q, want the manager's own UTC day", got.Day)
	}
}

// Before the day's first response arrives there is no provider figure to take a
// fraction of, and the fuse falls back to a conservative cap. A readout that
// presented that cap as the provider's limit would misreport the quota as tiny,
// so the two cases must stay distinguishable.
func TestCreditStatusDistinguishesAnUnobservedDenominator(t *testing.T) {
	now := time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)
	m := statusManager(t, statusConfig(0.5, 0), now)
	got, err := m.CreditStatus(context.Background(), config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if got.Denominator != 0 {
		t.Fatalf("denominator = %d, want 0 for an unobserved day", got.Denominator)
	}
	if got.Limit != BootstrapCreditCap {
		t.Fatalf("limit = %d, want the bootstrap cap %d", got.Limit, BootstrapCreditCap)
	}
	if got.Committed != 0 {
		t.Fatalf("committed = %d, want nothing spent", got.Committed)
	}
}

// A spent allowance and a hard operator ceiling both have to read as exhausted,
// because both refuse work: the readout exists so that refusal is explicable.
func TestCreditStatusReportsExhaustionAtTheHardCeiling(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	m := statusManager(t, statusConfig(0.5, 5), now)
	ctx := context.Background()
	EgressTestDisableGates(t)
	if err := m.ObserveLimit(ctx, config.SourceOpenAlex, "key-abc", 10000, true); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitEgress(ctx, EgressRequest{Source: config.SourceOpenAlex, Identity: "key-abc", Credits: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := m.CreditStatus(ctx, config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit != 5 {
		t.Fatalf("limit = %d, want the operator's hard ceiling to win over 50%% of 10000", got.Limit)
	}
	if !got.Exhausted() {
		t.Fatalf("status = %+v, want exhausted at the ceiling", got)
	}
}

// A zero fraction disables the ceiling but not the accounting: every request
// still commits, so the readout must show real spend and must not present a
// meaningless limit as one.
func TestCreditStatusReportsUnmeteredSpend(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	m := statusManager(t, statusConfig(0, 0), now)
	ctx := context.Background()
	EgressTestDisableGates(t)
	if err := m.CommitEgress(ctx, EgressRequest{Source: config.SourceOpenAlex, Identity: "key-abc", Credits: 7}); err != nil {
		t.Fatal(err)
	}
	got, err := m.CreditStatus(ctx, config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Unmetered {
		t.Fatalf("status = %+v, want unmetered", got)
	}
	if got.Committed != 7 {
		t.Fatalf("committed = %d, want spend recorded even with no ceiling", got.Committed)
	}
	if got.Exhausted() {
		t.Fatalf("status = %+v, want an unmetered source never to read as exhausted", got)
	}
}
