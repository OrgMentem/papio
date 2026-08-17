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
	result := map[string]any{
		"status": "failed",
		"error":  "Zotero file storage refused upload (HTTP 413)",
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
		"WebDAV",
		"attachment_mode = \"linked-file\"",
		"do not sync to other devices",
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
