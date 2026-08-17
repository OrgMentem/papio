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

	seed := func(id, settled string, autoImport bool, importStatus string) {
		t.Helper()
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

	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	seed("job_waiting_import", old, true, "error")
	seed("job_imported", old, true, "applied")
	seed("job_no_auto_import", old, false, "error")

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
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
	for _, c := range report.Checks {
		if c.Name == "undelivered_zotero_imports" {
			return c
		}
	}
	t.Fatalf("no undelivered_zotero_imports check: %+v", report.Checks)
	return Check{}
}
