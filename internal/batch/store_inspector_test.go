// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package batch

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// A receipt whose job row is gone must not fail the scoped lookup: it falls
// back to the job committed under the request ID, or reports nothing found.
func TestJobForScopedRequestFallsBackWhenReceiptJobIsMissing(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	policy := job.Policy{AccessMode: "conservative", DesiredVersion: "any"}
	created, err := jobs.CreateRequestForWork(ctx, "request-live", work.Work{DOI: "10.1000/live"}, "", "", policy, nil, job.Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}

	// The daemon connection enforces the receipt->jobs foreign key, so a
	// dangling receipt only exists after a restore or manual repair; switch
	// enforcement off on one connection to reproduce that state.
	conn, err := db.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"request-live", "request-absent"} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO submission_receipts(request_id,fingerprint,job_id,created_at) VALUES(?,?,?,?)`,
			requestID, "fp", "job-missing", store.Now()); err != nil {
			t.Fatal(err)
		}
	}

	inspector, err := openStoreInspector(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspector.close() })

	jobID, state, found, err := inspector.jobForScopedRequest(ctx, "request-live")
	if err != nil || !found || jobID != created.JobID || state == "" {
		t.Fatalf("live request = %q %q %v, %v; want job %s", jobID, state, found, err, created.JobID)
	}
	jobID, state, found, err = inspector.jobForScopedRequest(ctx, "request-absent")
	if err != nil || found || jobID != "" || state != "" {
		t.Fatalf("absent request = %q %q %v, %v; want not found", jobID, state, found, err)
	}
}
