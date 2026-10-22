// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

// seedManagedFilingJob inserts one ready, active job whose main artifact passed
// identity validation, which is what the managed-filing check counts. A
// non-empty state also inserts a receipt for that destination.
func seedManagedFilingJob(t *testing.T, ctx context.Context, db *store.Store, id, destination, state string) {
	t.Helper()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	sha := id + "sha"
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, 'Example paper')`, []any{"wr_" + id, at}},
		{`INSERT INTO artifacts (sha256, size_bytes, mime, path, created_at) VALUES (?, 1, 'application/pdf', ?, ?)`, []any{sha, "/tmp/" + id, at}},
		{`INSERT INTO jobs (id, work_request_id, state, policy_json, artifact_sha256, created_at, updated_at) VALUES (?, ?, 'ready', '{}', ?, ?, ?)`, []any{id, "wr_" + id, sha, at, at}},
		{`INSERT INTO job_artifacts (job_id, artifact_sha256, role, identity_result, created_at) VALUES (?, ?, 'main', 'pass', ?)`, []any{id, sha, at}},
	} {
		if _, err := db.DB().ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	if state == "" {
		return
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO managed_filings (job_id, destination, idempotency_key, artifact_sha256, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, destination, "key_"+id, sha, state, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestManagedFilingCheckCountsJobsWithoutAFiledReceiptAtTheConfiguredDestination(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "collection")
	const missing = "1 validated acquisitions lack a managed folder receipt"
	const filed = "every active validated acquisition has a managed folder receipt"
	for _, tc := range []struct {
		name        string
		folder      string
		onReady     string
		destination string // receipt destination; ignored when state is empty
		state       string
		wantStatus  string
		wantDetail  string
	}{
		{name: "no receipt", folder: folder, wantStatus: Warn, wantDetail: missing},
		{name: "failed receipt is not filed", folder: folder, destination: folder, state: "failed", wantStatus: Warn, wantDetail: missing},
		{name: "pending receipt is not filed", folder: folder, destination: folder, state: "pending", wantStatus: Warn, wantDetail: missing},
		{name: "filed receipt", folder: folder, destination: folder, state: "filed", wantStatus: Pass, wantDetail: filed},
		{name: "filed at another destination", folder: folder, destination: filepath.Join(root, "elsewhere"), state: "filed", wantStatus: Warn, wantDetail: missing},
		{name: "managed folder wins over on_ready", folder: folder, onReady: "true", wantStatus: Warn, wantDetail: missing},
		{name: "managed folder wins over on_ready when filed", folder: folder, onReady: "true", destination: folder, state: "filed", wantStatus: Pass, wantDetail: filed},
		{name: "relative folder binds its absolute path", folder: "managed-relative", destination: absPath(t, "managed-relative"), state: "filed", wantStatus: Pass, wantDetail: filed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, storetest.DataDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			seedManagedFilingJob(t, ctx, db, "job_managed", tc.destination, tc.state)
			cfg := config.Default()
			cfg.AccessMode = config.ModeConservative
			cfg.DataDir = t.TempDir()
			cfg.Filing.Folder = tc.folder
			cfg.Hooks.OnReady = tc.onReady
			report := Run(ctx, cfg, db, pdf.Capability{}, "", nil)
			got, ok := findCheck(report, "filing")
			if !ok {
				t.Fatalf("no filing check: %+v", report.Checks)
			}
			if got.Status != tc.wantStatus || !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("filing check = %q %q, want %q containing %q", got.Status, got.Detail, tc.wantStatus, tc.wantDetail)
			}
		})
	}
}

func absPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
