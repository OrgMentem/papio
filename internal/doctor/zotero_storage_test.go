// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func seedFailedZotioApply(t *testing.T, ctx context.Context, db *store.Store, jobID, createdAt string, zotioEnvelope map[string]any) {
	t.Helper()
	seedFailedZotioApplyError(t, ctx, db, jobID, createdAt, "Zotero file storage refused upload (HTTP 413)", zotioEnvelope)
}

// seedFailedZotioApplyError carries the error text too, because the routing
// refusal is identified by its structured precondition and the ABSENCE of an
// HTTP status - a fixture that always says "HTTP 413" cannot express it.
func seedFailedZotioApplyError(t *testing.T, ctx context.Context, db *store.Store, jobID, createdAt, errText string, zotioEnvelope map[string]any) {
	t.Helper()
	result := map[string]any{
		"status": "failed",
		"error":  errText,
		"zotio":  zotioEnvelope,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO exports (job_id, kind, idempotency_key, result_json, created_at)
		VALUES (?, 'zotio_apply', ?, ?, ?)`,
		jobID, "apply_"+jobID, string(raw), createdAt); err != nil {
		t.Fatal(err)
	}
}

func zoteroFileStorageRefusedCheck(t *testing.T, ctx context.Context, db *store.Store) Check {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	for _, c := range report.Checks {
		if c.Name == "zotero_file_storage_refused" {
			return c
		}
	}
	t.Fatalf("no zotero_file_storage_refused check: %+v", report.Checks)
	return Check{}
}

func TestRunWarnsOnRecentZoteroFileStorageRefusedApplies(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first := time.Now().UTC().Add(-36 * time.Hour)
	seedFailedZotioApply(t, ctx, db, "job_refused_1", first.Format(time.RFC3339Nano), map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413},
	})
	seedFailedZotioApply(t, ctx, db, "job_refused_2", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339Nano), map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413},
	})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn: %+v", got.Status, got)
	}
	if !strings.Contains(got.Detail, "2 recent Zotero apply failures returned HTTP 413 (file storage refused the upload)") {
		t.Fatalf("detail = %q, want the recent failure count", got.Detail)
	}
	if !strings.Contains(got.Detail, "first seen "+first.Format("2006-01-02")) {
		t.Fatalf("detail = %q, want first failure date", got.Detail)
	}
	for _, want := range []string{
		"Sync pane",
		"attachment_mode = \"linked-file\"",
		"do not sync to other devices",
	} {
		if !strings.Contains(got.Remediation, want) {
			t.Fatalf("remediation = %q, missing %q", got.Remediation, want)
		}
	}
}

// The operator's real rows carry Zotero's own sentence, and the check must
// report that instead of an anonymous 413: a full plan and an unexplained
// refusal have different remedies, and telling someone with their own WebDAV
// server to inspect it when Zotero's plan is full wastes the one thing the
// upstream response already settled.
func TestRunReportsZoteroQuotaWhenZoteroExplainedIt(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first := time.Now().UTC().Add(-24 * time.Hour)
	seedFailedZotioApply(t, ctx, db, "job_quota_1", first.Format(time.RFC3339Nano), map[string]any{
		"ok": false,
		"error": map[string]any{
			"http_status": 413,
			"message":     "authorizing upload: File would exceed quota (300.4 > 300)",
		},
	})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn: %+v", got.Status, got)
	}
	if !strings.Contains(got.Detail, "storage plan is full (300.4 of 300 MB used)") {
		t.Fatalf("detail = %q, want Zotero's own figures", got.Detail)
	}
	if strings.Contains(got.Remediation, "Sync pane") {
		t.Fatalf("remediation = %q, must not send the operator upstream when Zotero named the cause", got.Remediation)
	}
	for _, want := range []string{
		"free space in Zotero",
		"attachment_mode = \"linked-file\"",
		"not a WebDAV target",
	} {
		if !strings.Contains(got.Remediation, want) {
			t.Fatalf("remediation = %q, missing %q", got.Remediation, want)
		}
	}
}

func TestRunPassesWhenFileStorageRefusedFailuresAreOld(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	seedFailedZotioApply(t, ctx, db, "job_refused_old", old, map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413},
	})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Pass {
		t.Fatalf("status = %q, want pass once failures aged out: %+v", got.Status, got)
	}
}

func TestRunPassesWhenNoFileStorageRefusedFailures(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	result, err := json.Marshal(map[string]any{
		"status": "failed",
		"error":  "Zotero HTTP 429",
		"zotio":  map[string]any{"ok": false, "error": map[string]any{"http_status": 429}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO exports (job_id, kind, idempotency_key, result_json, created_at)
		VALUES ('job_rate_limited', 'zotio_apply', 'apply_rate', ?, ?)`, string(result), recent); err != nil {
		t.Fatal(err)
	}

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Pass {
		t.Fatalf("status = %q, want pass for non-413 failures: %+v", got.Status, got)
	}
}

// A quota reading is a measurement with a timestamp. Stating an old one in the
// present tense made a plan the operator had already cleared look like a live
// blocker, and sent a diagnosis of genuinely live refusals down the wrong path.
func TestZoteroFileStorageRefusedDatesTheQuotaReading(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stale := time.Now().UTC().Add(-5 * 24 * time.Hour)
	seedFailedZotioApply(t, ctx, db, "job_quota_old", stale.Format(time.RFC3339Nano), map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413, "message": "File would exceed quota (300.4 > 300)"},
	})
	// A newer refusal that is NOT a quota failure: the operator is still being
	// refused, for a different reason.
	seedFailedZotioApply(t, ctx, db, "job_other_new", time.Now().UTC().Add(-1*time.Hour).Format(time.RFC3339Nano), map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413},
	})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn: %+v", got.Status, got)
	}
	if !strings.Contains(got.Detail, "as measured "+stale.Format("2006-01-02")) {
		t.Fatalf("detail = %q, want the quota figures dated", got.Detail)
	}
	if !strings.Contains(got.Detail, "1 of them was refused for another reason") {
		t.Fatalf("detail = %q, want the non-quota refusals distinguished", got.Detail)
	}
	if !strings.Contains(got.Detail, "last "+time.Now().UTC().Format("2006-01-02")) {
		t.Fatalf("detail = %q, want the newest refusal dated", got.Detail)
	}
}

// The routing refusal - zotio declining a stored upload because the library
// keeps its files on the operator's own file store - is a different fact from a
// full storage plan, and the quota advice is wrong for it in both directions:
// freeing space changes nothing, and no retry can succeed. Reported as HTTP 413
// it sent a diagnosis looking for a status code Zotero never sent, and folded
// into "another reason" behind a five-day-old quota figure it was invisible.
const routingRefusalError = `zotio: attachments add refused: {"outcome":"precondition_unmet",` +
	`"capability":"attachments add","precondition":"zotero_file_storage",` +
	`"detail":"Zotero desktop keeps personal-library attachment files on your own file store, ` +
	`but a stored attachment uploaded through the Zotero Web API always lands in Zotero's own cloud storage"}`

func TestZoteroFileStorageRefusedNamesTheRoutingRefusal(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	seedFailedZotioApplyError(t, ctx, db, "job_routing", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		routingRefusalError, map[string]any{"ok": false})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn: %+v", got.Status, got)
	}
	if strings.Contains(got.Detail, "413") {
		t.Fatalf("detail = %q, must not claim an HTTP status Zotero never sent", got.Detail)
	}
	if !strings.Contains(got.Detail, "no route to the file store") {
		t.Fatalf("detail = %q, want the routing refusal named", got.Detail)
	}
	for _, want := range []string{"freeing space will not help", "--via connector", "Upgrade zotio"} {
		if !strings.Contains(got.Remediation, want) {
			t.Fatalf("remediation = %q, missing %q", got.Remediation, want)
		}
	}
	if strings.Contains(got.Remediation, "free space in Zotero") {
		t.Fatalf("remediation = %q, must not offer quota advice for a refusal freeing space cannot fix", got.Remediation)
	}
	if strings.Contains(got.Remediation, "Papio retries once uploads are accepted") {
		t.Fatalf("remediation = %q, must not promise a retry that cannot succeed", got.Remediation)
	}
}

// A stale quota reading must not swallow the one live cause: this is the exact
// shape of the operator's machine on 2026-08-22, where ten quota failures from
// five days earlier were the headline and the single routing refusal from that
// morning was a footnote whose remediation addressed the resolved cause.
func TestZoteroFileStorageRefusedReportsBothCauses(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stale := time.Now().UTC().Add(-5 * 24 * time.Hour)
	seedFailedZotioApply(t, ctx, db, "job_quota_stale", stale.Format(time.RFC3339Nano), map[string]any{
		"ok": false, "error": map[string]any{"http_status": 413, "message": "File would exceed quota (300.4 > 300)"},
	})
	live := time.Now().UTC().Add(-time.Hour)
	seedFailedZotioApplyError(t, ctx, db, "job_routing_live", live.Format(time.RFC3339Nano),
		routingRefusalError, map[string]any{"ok": false})

	got := zoteroFileStorageRefusedCheck(t, ctx, db)
	if !strings.Contains(got.Detail, "as measured "+stale.Format("2006-01-02")) {
		t.Fatalf("detail = %q, want the quota figures dated", got.Detail)
	}
	if !strings.Contains(got.Detail, "1 had no route to the file store (last "+live.Format("2006-01-02")+")") {
		t.Fatalf("detail = %q, want the routing refusal counted and dated, not folded into 'another reason'", got.Detail)
	}
	if strings.Contains(got.Detail, "refused for another reason") {
		t.Fatalf("detail = %q, the routing refusal is a named cause, not an anonymous remainder", got.Detail)
	}
	for _, want := range []string{"free space in Zotero", "--via connector"} {
		if !strings.Contains(got.Remediation, want) {
			t.Fatalf("remediation = %q, missing %q - both causes are present so both need advice", got.Remediation, want)
		}
	}
}
