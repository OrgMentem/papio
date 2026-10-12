// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestArchivedAndExportedAcquisitionsDoNotWarnUncollected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	old := store.FormatTime(time.Now().Add(-10 * 24 * time.Hour))
	seedReadyImport(t, db, "job_archive_doctor", old, true, "error")
	seedReadyImport(t, db, "job_export_doctor", old, false, "")
	before, _, err := uncollectedAcquisitions(ctx, db)
	if err != nil || before != 2 {
		t.Fatalf("uncollected before = %d, %v", before, err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET artifact_sha256 = (SELECT artifact_sha256 FROM job_artifacts WHERE job_id = jobs.id AND role = 'main') WHERE id = 'job_archive_doctor'`); err != nil {
		t.Fatal(err)
	}
	jobs := &job.Store{S: db}
	if _, err := jobs.Archive(ctx, "job_archive_doctor", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO exports(job_id, kind, idempotency_key, created_at) VALUES('job_export_doctor', 'bundle', 'doctor-export', ?)`, old); err != nil {
		t.Fatal(err)
	}
	after, _, err := uncollectedAcquisitions(ctx, db)
	if err != nil || after != 0 {
		t.Fatalf("uncollected after = %d, %v", after, err)
	}
	undelivered, _, err := undeliveredZoteroImports(ctx, db)
	if err != nil || undelivered != 0 {
		t.Fatalf("archived import warnings = %d, %v", undelivered, err)
	}
	if _, err := jobs.Restore(ctx, "job_archive_doctor"); err != nil {
		t.Fatal(err)
	}
	restored, _, err := uncollectedAcquisitions(ctx, db)
	if err != nil || restored != 1 {
		t.Fatalf("restored uncollected = %d, %v", restored, err)
	}
}
