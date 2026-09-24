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

func TestRunReportsUndeliveredZoteroImports(t *testing.T) {
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	seedReadyImport(t, db, "job_waiting_import", old, true, "error")
	seedReadyImport(t, db, "job_imported", old, true, "applied")
	seedReadyImport(t, db, "job_no_auto_import", old, false, "error")

	got := undeliveredImportCheck(t, ctx, db)
	if got.Status != Warn {
		t.Fatalf("status = %q, want warn: %+v", got.Status, got)
	}
	if !strings.Contains(got.Detail, "1 validated paper is waiting for Zotero import") {
		t.Fatalf("detail = %q, want the one stranded auto-import job", got.Detail)
	}
	if !strings.Contains(got.Remediation, "zotio.auto_import") {
		t.Fatalf("remediation = %q, want the manual import path", got.Remediation)
	}
}

func TestRunPassesWhenValidatedImportsDelivered(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	got := undeliveredImportCheck(t, ctx, db)
	if got.Status != Pass {
		t.Fatalf("status = %q, want pass on empty store: %+v", got.Status, got)
	}
}

func undeliveredImportCheck(t *testing.T, ctx context.Context, db *store.Store) Check {
	t.Helper()
	return importCheck(t, ctx, db, "undelivered_zotero_imports")
}

func importCheck(t *testing.T, ctx context.Context, db *store.Store, name string) Check {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s check: %+v", name, report.Checks)
	return Check{}
}

// A paper waiting for a closed Zotero desktop is neither delivered nor
// failed. Doctor names the wait, its size, and the one action that ends it,
// and the undelivered check no longer counts it as a stranded import.
func TestRunReportsPapersWaitingForZoteroDesktop(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	waitingSince := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	seedReadyImport(t, db, "job_waiting_a", waitingSince, true, "waiting")
	seedReadyImport(t, db, "job_waiting_b", waitingSince, true, "waiting")
	seedReadyImport(t, db, "job_failed", waitingSince, true, "error")
	seedReadyImport(t, db, "job_waiting_no_auto", waitingSince, false, "waiting")

	got := importCheck(t, ctx, db, "zotero_desktop_waiting")
	if got.Status != Warn || !strings.Contains(got.Detail, "2 papers are ready to add") || !strings.Contains(got.Detail, "waiting for 3h") {
		t.Fatalf("zotero_desktop_waiting = %+v, want a warning naming 2 papers waiting 3h", got)
	}
	if !strings.Contains(got.Remediation, "open Zotero desktop") {
		t.Fatalf("remediation = %q, want the one action that ends the wait", got.Remediation)
	}
	undelivered := undeliveredImportCheck(t, ctx, db)
	if !strings.Contains(undelivered.Detail, "1 validated paper is waiting") {
		t.Fatalf("undelivered = %+v, want only the failed import counted", undelivered)
	}

	// When the newest wait says Zotero is open but not responding, doctor
	// says restart it, not open it.
	stuckSince := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	seedReadyImport(t, db, "job_waiting_stuck", stuckSince, true, "waiting")
	if _, err := db.DB().ExecContext(ctx, `UPDATE events SET detail_json = json_set(detail_json, '$.reason', 'zotero_unresponsive') WHERE job_id = 'job_waiting_stuck'`); err != nil {
		t.Fatal(err)
	}
	stuck := importCheck(t, ctx, db, "zotero_desktop_waiting")
	if !strings.Contains(stuck.Detail, "open but not responding: 3 papers") || !strings.HasPrefix(stuck.Remediation, "restart Zotero desktop") {
		t.Fatalf("stuck zotero_desktop_waiting = %+v, want the restart remedy for 3 papers", stuck)
	}

	// A busy Zotero (a large sync holds it) needs neither opening nor a
	// restart.
	if _, err := db.DB().ExecContext(ctx, `UPDATE events SET detail_json = json_set(detail_json, '$.reason', 'zotero_busy') WHERE job_id = 'job_waiting_stuck'`); err != nil {
		t.Fatal(err)
	}
	busy := importCheck(t, ctx, db, "zotero_desktop_waiting")
	if !strings.Contains(busy.Detail, "Zotero desktop is busy: 3 papers") ||
		strings.Contains(busy.Remediation, "restart") || strings.Contains(busy.Remediation, "open Zotero") {
		t.Fatalf("busy zotero_desktop_waiting = %+v, want the busy condition without an open or restart remedy", busy)
	}

	empty, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = empty.Close() })
	if got := importCheck(t, ctx, empty, "zotero_desktop_waiting"); got.Status != Pass {
		t.Fatalf("zotero_desktop_waiting on an empty store = %+v, want pass", got)
	}
}

// Once Zotero accepts papers, the daemon queues the papers that waited past
// one pass's bound. Doctor must stop telling the user to open Zotero, and a
// queued paper is not a stranded import either.
func TestRunReportsQueuedPapersAsReadyForZotero(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	queuedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	seedReadyImport(t, db, "job_queued_a", queuedAt, true, "queued")
	seedReadyImport(t, db, "job_queued_b", queuedAt, true, "queued")

	waiting := importCheck(t, ctx, db, "zotero_desktop_waiting")
	if waiting.Status != Pass || !strings.Contains(waiting.Detail, "2 papers are queued") {
		t.Fatalf("zotero_desktop_waiting = %+v, want a pass naming the 2 queued papers", waiting)
	}
	if undelivered := undeliveredImportCheck(t, ctx, db); undelivered.Status != Pass {
		t.Fatalf("undelivered_zotero_imports = %+v, want queued papers not counted as stranded", undelivered)
	}
}

// seedReadyImport inserts one validated ready job and, when importStatus is
// set, its latest zotio.auto_import outcome.
func seedReadyImport(t *testing.T, db *store.Store, id, settled string, autoImport bool, importStatus string) {
	t.Helper()
	ctx := context.Background()
	policy, err := json.Marshal(map[string]any{"auto_import": autoImport})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, 'Example paper')`,
		"wr_"+id, settled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES (?, ?, 'ready', ?, ?, ?)`, id, "wr_"+id, string(policy), settled, settled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO artifacts (sha256, size_bytes, mime, path, created_at)
		VALUES (?, 1, 'application/pdf', ?, ?)`, id+"sha", "/tmp/"+id, settled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO job_artifacts (job_id, artifact_sha256, role, identity_result, created_at)
		VALUES (?, ?, 'main', 'pass', ?)`, id, id+"sha", settled); err != nil {
		t.Fatal(err)
	}
	if importStatus != "" {
		detail, err := json.Marshal(map[string]any{
			"status":      importStatus,
			"parent_key":  "",
			"error_class": "bundle_validation",
			"error_hint":  "bundle title missing or out of range",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO events (job_id, at, kind, detail_json)
			VALUES (?, ?, 'zotio.auto_import', ?)`, id, settled, string(detail)); err != nil {
			t.Fatal(err)
		}
	}
}
