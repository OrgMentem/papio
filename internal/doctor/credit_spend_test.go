// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func creditFixture(t *testing.T, fraction float64, hard int) (context.Context, config.Config, *store.Store) {
	t.Helper()
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	cfg.Email = "researcher@example.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{
		Enabled:             true,
		APIKey:              "SUPER_SECRET_KEY",
		DailyCreditFraction: fraction,
		DailyCreditLimit:    hard,
	}
	return ctx, cfg, db
}

func creditCheck(t *testing.T, ctx context.Context, cfg config.Config, db *store.Store) Check {
	t.Helper()
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	for _, c := range report.Checks {
		if c.Name == "credits_"+config.SourceOpenAlex {
			return c
		}
	}
	t.Fatalf("no credit check reported: %+v", report.Checks)
	return Check{}
}

// Enforcement shipped without a readout: the fuse could refuse real work while
// no surface said how much of the allowance was gone. The number has to be
// visible BEFORE the ceiling is reached — a figure that only appears once work
// is already being refused explains an outage after the fact instead of letting
// anyone see it coming.
func TestCreditCheckNamesTodaysSpendWhileStillHealthy(t *testing.T) {
	ctx, cfg, db := creditFixture(t, 0.5, 0)
	budgets := budget.New(db, budget.WithCreditPolicy(budget.CreditPolicyFromConfig(cfg)))
	budget.EgressTestDisableGates(t)
	if err := budgets.ObserveLimit(ctx, config.SourceOpenAlex, "key-abc", 10000, true); err != nil {
		t.Fatal(err)
	}
	if err := budgets.CommitEgress(ctx, budget.EgressRequest{
		Source: config.SourceOpenAlex, Identity: "key-abc", Credits: 12,
	}); err != nil {
		t.Fatal(err)
	}

	got := creditCheck(t, ctx, cfg, db)
	if got.Status != Pass {
		t.Fatalf("status = %q, want pass while the allowance is live: %+v", got.Status, got)
	}
	for _, want := range []string{"12", "5000", "10000 reported by the provider"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("detail %q missing %q: spend, ceiling and its provenance are all required", got.Detail, want)
		}
	}
}

// The credential fingerprint may be named; the credential may not. doctor prints
// this on a terminal and serialises it to JSON, and the fingerprint exists so
// the allowance can be tied to credentials without that risk. It must read as
// what the ceiling is SHARED BY: the fuse keeps one row per source per day, so
// naming a spend as one credential's would invent a breakdown nothing recorded.
func TestCreditCheckNamesTheIdentityWithoutLeakingIt(t *testing.T) {
	ctx, cfg, db := creditFixture(t, 0.5, 0)
	budgets := budget.New(db, budget.WithCreditPolicy(budget.CreditPolicyFromConfig(cfg)))
	budget.EgressTestDisableGates(t)
	if err := budgets.Acquire(ctx, config.SourceOpenAlex, cfg.SourcePolicy(config.SourceOpenAlex), 0); err != nil {
		t.Fatal(err)
	}
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SUPER_SECRET_KEY") {
		t.Fatalf("credit readout leaked the credential: %s", encoded)
	}
	got := creditCheck(t, ctx, cfg, db)
	if !strings.Contains(got.Detail, "key-") {
		t.Fatalf("detail %q names no credential; an allowance nobody can tie to an account is not visibility", got.Detail)
	}
	if !strings.Contains(got.Detail, "shared by") {
		t.Fatalf("detail %q reads as per-credential attribution; the fuse records one row per source per day", got.Detail)
	}
}

// A spent allowance must read as a warning with the reset in the remedy, since
// that is the state where a user otherwise concludes papio is broken.
func TestCreditCheckWarnsWithTheResetOnceExhausted(t *testing.T) {
	ctx, cfg, db := creditFixture(t, 0.5, 4)
	budgets := budget.New(db, budget.WithCreditPolicy(budget.CreditPolicyFromConfig(cfg)))
	budget.EgressTestDisableGates(t)
	if err := budgets.ObserveLimit(ctx, config.SourceOpenAlex, "key-abc", 10000, true); err != nil {
		t.Fatal(err)
	}
	if err := budgets.CommitEgress(ctx, budget.EgressRequest{
		Source: config.SourceOpenAlex, Identity: "key-abc", Credits: 4,
	}); err != nil {
		t.Fatal(err)
	}
	got := creditCheck(t, ctx, cfg, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn once the allowance is spent: %+v", got.Status, got)
	}
	if !strings.Contains(got.Remediation, "00:00 UTC") {
		t.Fatalf("remediation %q does not say when work resumes", got.Remediation)
	}
}

// A source with no credit accounting must not acquire an empty credit readout:
// a ceiling reported for a provider that has none is a fabricated number.
func TestCreditCheckSkipsSourcesWithoutCreditAccounting(t *testing.T) {
	ctx, cfg, db := creditFixture(t, 0.5, 0)
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: false}
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "credits_") {
			t.Fatalf("unexpected credit check %q for a source with no credit policy: %+v", c.Name, c)
		}
	}
}
