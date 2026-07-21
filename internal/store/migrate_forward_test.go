// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"context"
	"database/sql"
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/pdf"
	"papio/internal/store"

	_ "modernc.org/sqlite"
)

//go:embed migrations/0001_init.sql
var schemaV1 string

func TestOpenRollsForwardSchemaOneWithoutLosingDurableRows(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open schema-v1 database: %v", err)
	}
	if _, err := raw.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("apply 0001 only: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set schema version one: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO work_requests(id, created_at, requester, title, authors_json, year, desired_version)
		VALUES ('migration-request-0001', '2026-07-15T00:00:00Z', 'cli', 'Representative work', '["Ada Author"]', 2026, 'any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('migration-job-0001', 'migration-request-0001', 'resolving', '{"access_mode":"conservative","desired_version":"any","fetch_max_bytes":1048576}', '2026-07-15T00:00:00Z', '2026-07-15T00:00:00Z');
		INSERT INTO candidates(job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
		VALUES ('migration-job-0001', 'browser', 'https://example.test/<redacted>', 'migration-candidate-key', 'published', 'subscription', 'unknown', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'verify_identity', 'inspect local copy', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'openurl_handoff', 'open-access fetch via browser' || char(10) || 'https://oa.example.test/paper.pdf', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'openurl_handoff', 'open-access candidates exhausted; institutional OpenURL handoff available in your browser', '2026-07-15T00:00:01Z');
		INSERT INTO exports(job_id, kind, idempotency_key, path, result_json, created_at)
		VALUES ('migration-job-0001', 'bundle', 'bundle:migration-job-0001:fixture', '/tmp/fixture', '{"fixture":true}', '2026-07-15T00:00:00Z');
	`); err != nil {
		t.Fatalf("seed schema-v1 rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close schema-v1 database: %v", err)
	}

	migrated, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open and roll forward: %v", err)
	}
	defer migrated.Close()
	version, err := migrated.UserVersion(ctx)
	if err != nil || version != 11 {
		t.Fatalf("user_version = %d, %v; want 11", version, err)
	}

	var jobs, actions, exports int
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM human_actions").Scan(&actions); err != nil {
		t.Fatalf("count human actions: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM exports").Scan(&exports); err != nil {
		t.Fatalf("count exports: %v", err)
	}
	if jobs != 1 || actions != 3 || exports != 1 {
		t.Fatalf("migrated durable rows jobs=%d actions=%d exports=%d, want 1/3/1", jobs, actions, exports)
	}

	// 0011 backfill: legacy detail markers become structured classification.
	var oaAuth, instAuth int
	var oaBlocked, instBlocked string
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT requires_auth, blocked_by FROM human_actions WHERE kind = 'openurl_handoff' AND detail LIKE 'open-access fetch%'").Scan(&oaAuth, &oaBlocked); err != nil {
		t.Fatalf("read OA handoff backfill: %v", err)
	}
	if oaAuth != 0 || oaBlocked != "anti_bot" {
		t.Fatalf("OA handoff backfill = requires_auth %d blocked_by %q, want 0/anti_bot", oaAuth, oaBlocked)
	}
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT requires_auth, blocked_by FROM human_actions WHERE kind = 'openurl_handoff' AND detail LIKE 'open-access candidates exhausted%'").Scan(&instAuth, &instBlocked); err != nil {
		t.Fatalf("read institutional handoff backfill: %v", err)
	}
	if instAuth != 1 || instBlocked != "paywall" {
		t.Fatalf("institutional handoff backfill = requires_auth %d blocked_by %q, want 1/paywall", instAuth, instBlocked)
	}

	var spent float64
	if err := migrated.DB().QueryRowContext(ctx, "SELECT spent_usd FROM jobs WHERE id = 'migration-job-0001'").Scan(&spent); err != nil {
		t.Fatalf("read 0002 default: %v", err)
	}
	if spent != 0 {
		t.Fatalf("jobs.spent_usd = %v, want migration default 0", spent)
	}
	var accessBasis string
	var reviewOverride int
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT access_basis, review_override FROM candidates WHERE job_id = 'migration-job-0001'").Scan(&accessBasis, &reviewOverride); err != nil {
		t.Fatalf("read migrated candidate: %v", err)
	}
	if accessBasis != "institutional" || reviewOverride != 0 {
		t.Fatalf("candidate after migration = access_basis %q review_override %d, want institutional and 0", accessBasis, reviewOverride)
	}
	var watchCount int
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM watches").Scan(&watchCount); err != nil {
		t.Fatalf("query v5 watches table: %v", err)
	}
	if watchCount != 0 {
		t.Fatalf("new watches table count = %d, want 0", watchCount)
	}

	worker, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.Email = "reader@example.test"
	cfg.DataDir = dataDir
	report := doctor.Run(ctx, cfg, migrated, pdf.Capability{
		PDFToText: worker,
		PDFInfo:   worker,
		PDFToPPM:  worker,
		Tesseract: worker,
	}, worker)
	if !report.OK {
		t.Fatalf("doctor after roll-forward is unhealthy: %+v", report)
	}
}

func TestBackupCleansFailedOutputAndAllowsRetry(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	backupDir := t.TempDir()
	destination := filepath.Join(backupDir, "backup.db")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.Backup(canceled, destination); err == nil {
		t.Fatal("backup with canceled context succeeded")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed backup destination remains: %v", err)
	}
	partials, err := filepath.Glob(filepath.Join(backupDir, ".papio-backup-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("failed backup temporary files remain: %v", partials)
	}
	if err := db.Backup(ctx, destination); err != nil {
		t.Fatalf("backup retry: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("backup destination after retry: %v", err)
	}
}
