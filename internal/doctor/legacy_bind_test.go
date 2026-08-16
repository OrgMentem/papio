// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/grab"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

const legacyBindJobID = "job_00000000000000000000000091"

func legacyBindFixture(t *testing.T) (context.Context, config.Config, *store.Store, *grab.Service) {
	t.Helper()
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := store.Now()
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-legacy-bind", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		legacyBindJobID, "req-legacy-bind", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	cfg.Email = "researcher@example.test"
	return ctx, cfg, db, grab.New(db, nil)
}

// bindUnderRule files one grab against the seeded job with the given rule
// version recorded, exactly the way a real automatic decision would have.
func bindUnderRule(t *testing.T, ctx context.Context, svc *grab.Service, rule string) string {
	t.Helper()
	g, err := svc.Allocate(ctx, "legacy.example.org", "legacy bind")
	if err != nil {
		t.Fatal(err)
	}
	prov := grab.BindProvenance{
		Method:               "candidate_auto_bind",
		Rule:                 rule,
		Winner:               legacyBindJobID,
		CandidatesConsidered: 1,
		Evidence:             []string{"title_printed"},
	}
	decide := func(context.Context, *sql.Tx) (grab.BindProvenance, error) { return prov, nil }
	if err := svc.MarkBoundToJobFenced(ctx, g.ID, legacyBindJobID, "job_created", decide); err != nil {
		t.Fatalf("MarkBoundToJobFenced: %v", err)
	}
	return g.ID
}

func findCheck(report Report, name string) (Check, bool) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// The expected state of every real install: the feature never shipped a
// decision, so there is nothing to report and doctor must stay quiet rather
// than carry a permanent line about decisions that were never made.
func TestLegacyBindCheckSilentWhenNothingWasBoundAutomatically(t *testing.T) {
	ctx, cfg, db, _ := legacyBindFixture(t)
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	if c, ok := findCheck(report, "grab_bind_legacy_rule"); ok {
		t.Fatalf("check reported on a clean install: %+v", c)
	}
	if c, ok := findCheck(report, "grab_bind_audit_unreadable"); ok {
		t.Fatalf("unreadable check reported on a clean install: %+v", c)
	}
}

// A bind recorded under the superseded rule is exactly the row this check
// exists to surface: its stored file may not be the paper its citation names,
// and only a human can settle that.
func TestLegacyBindCheckNamesTheAffectedJob(t *testing.T) {
	ctx, cfg, db, svc := legacyBindFixture(t)
	bindUnderRule(t, ctx, svc, "candidate_auto_bind/1")
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	c, ok := findCheck(report, "grab_bind_legacy_rule")
	if !ok {
		t.Fatalf("no legacy-bind check reported: %+v", report.Checks)
	}
	if c.Status != Warn {
		t.Fatalf("status = %q, want %q", c.Status, Warn)
	}
	if !strings.Contains(c.Remediation, legacyBindJobID) {
		t.Fatalf("remediation does not name the job to inspect: %q", c.Remediation)
	}
	if !strings.Contains(c.Remediation, "papio jobs receipt") {
		t.Fatalf("remediation does not name the command that shows the paper: %q", c.Remediation)
	}
	if !strings.Contains(c.Detail, "1 download was") {
		t.Fatalf("detail = %q, want a singular count", c.Detail)
	}
}

// The rule in force is not a finding. If the current version tripped this
// check, every install would be told to audit binds nothing distrusts.
func TestLegacyBindCheckIgnoresTheRuleInForce(t *testing.T) {
	ctx, cfg, db, svc := legacyBindFixture(t)
	bindUnderRule(t, ctx, svc, pdf.CandidateBindingRule)
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	if c, ok := findCheck(report, "grab_bind_legacy_rule"); ok {
		t.Fatalf("current rule %q reported as legacy: %+v", pdf.CandidateBindingRule, c)
	}
}

// A row whose audit trail cannot be parsed must not be counted as clean.
// Reporting zero because the answer was unreadable is the one outcome an
// operator cannot act on.
func TestLegacyBindCheckReportsUnreadableAudit(t *testing.T) {
	ctx, cfg, db, svc := legacyBindFixture(t)
	grabID := bindUnderRule(t, ctx, svc, "candidate_auto_bind/1")
	if _, err := db.DB().ExecContext(ctx, `UPDATE pdf_grabs SET bind_provenance = ? WHERE id = ?`, "{not json", grabID); err != nil {
		t.Fatal(err)
	}
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	if c, ok := findCheck(report, "grab_bind_legacy_rule"); ok {
		t.Fatalf("unreadable row counted as a legacy bind: %+v", c)
	}
	c, ok := findCheck(report, "grab_bind_audit_unreadable")
	if !ok {
		t.Fatalf("no unreadable-audit check reported: %+v", report.Checks)
	}
	if c.Status != Warn {
		t.Fatalf("status = %q, want %q", c.Status, Warn)
	}
	if !strings.Contains(c.Detail, "1 download has") {
		t.Fatalf("detail = %q, want a singular count", c.Detail)
	}
}
