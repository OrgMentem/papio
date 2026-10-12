// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zotio

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestImportBackfillNeverSelectsArchivedEvenWithIncludeNotRequested(t *testing.T) {
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	seedImportBackfillJob(t, ctx, db, "job_archived_requested", store.FormatTime(time.Now().Add(-time.Hour)), true, "pass", "")
	seedImportBackfillJob(t, ctx, db, "job_archived_unrequested", store.Now(), false, "pass", "")
	if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET artifact_sha256 = (SELECT artifact_sha256 FROM job_artifacts WHERE job_id = jobs.id AND role = 'main')`); err != nil {
		t.Fatal(err)
	}
	jobs := &job.Store{S: db}
	for _, id := range []string{"job_archived_requested", "job_archived_unrequested"} {
		if _, err := jobs.Archive(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	service := importBackfillService(t, data, db, nil)
	for _, include := range []bool{false, true} {
		candidates, truncated, err := service.listImportBackfillCandidates(ctx, include, "", 10)
		if err != nil || truncated || len(candidates) != 0 {
			t.Fatalf("archived candidates(include=%v) = %+v, %v, %v", include, candidates, truncated, err)
		}
		excluded, err := service.countImportBackfillExcluded(ctx, include)
		if err != nil || excluded != 0 {
			t.Fatalf("archived not-requested count = %d, %v", excluded, err)
		}
	}
	class, reason, _, err := service.classifyImportBackfillJob(ctx, "job_archived_requested", map[string]string{"job_archived_requested": "PARENT01"}, true)
	if err != nil || class != importBackfillExpectedFail || reason != "archived" {
		t.Fatalf("archived classify = %v, %q, %v", class, reason, err)
	}
	if _, err := jobs.Restore(ctx, "job_archived_requested"); err != nil {
		t.Fatal(err)
	}
	candidates, _, err := service.listImportBackfillCandidates(ctx, true, "", 10)
	if err != nil || len(candidates) != 1 || candidates[0].JobID != "job_archived_requested" {
		t.Fatalf("restored candidates = %+v, %v", candidates, err)
	}
}
