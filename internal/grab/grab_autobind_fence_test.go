// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package grab

import (
	"database/sql"
	"strings"
	"testing"

	"context"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

// MarkBoundToJobFenced with a fence returning an error leaves the row
// completely untouched (state, job_id, outcome and provenance all unchanged),
// proving rollback.
func TestMarkBoundToJobFencedRollbackOnFenceError(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000077"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-fence-rollback", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-fence-rollback", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Allocate(ctx, "fence.example.org", "rollback case")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkQuarantined(ctx, g.ID, "/tmp/q/"+g.ID); err != nil {
		t.Fatalf("MarkQuarantined: %v", err)
	}
	before, _ := svc.Get(ctx, g.ID)
	decide := func(ctx context.Context, tx *sql.Tx) (BindProvenance, error) {
		return BindProvenance{}, sql.ErrNoRows // arbitrary fence rejection
	}
	err = svc.MarkBoundToJobFenced(ctx, g.ID, jobID, "job_created", decide)
	if err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("MarkBoundToJobFenced err = %v, want fence error", err)
	}
	after, _ := svc.Get(ctx, g.ID)
	if after.State != before.State || after.JobID != before.JobID || after.Outcome != before.Outcome || after.BindProvenance != before.BindProvenance {
		t.Fatalf("row changed after fence rejection: before %+v after %+v", before, after)
	}
	var raw sql.NullString
	if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Fatalf("bind_provenance = %q, want NULL after rollback", raw.String)
	}
}

func TestMarkBoundToJobFencedHappyWithFence(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000078"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-fence-ok", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-fence-ok", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Allocate(ctx, "fenceok.example.org", "fence ok case")
	if err != nil {
		t.Fatal(err)
	}
	decide := func(ctx context.Context, tx *sql.Tx) (BindProvenance, error) {
		return BindProvenance{Method: "candidate_auto_bind", Rule: "candidate_auto_bind/2", Winner: jobID, CandidatesConsidered: 1, Evidence: []string{"title_printed"}}, nil
	}
	if err := svc.MarkBoundToJobFenced(ctx, g.ID, jobID, "job_created", decide); err != nil {
		t.Fatalf("MarkBoundToJobFenced: %v", err)
	}
	got, _ := svc.Get(ctx, g.ID)
	if got.State != StateJobCreated || got.JobID != jobID {
		t.Fatalf("got %+v, want job_created/%s", got, jobID)
	}
	if got.BindProvenance == "" {
		t.Fatal("bind_provenance empty after fenced bind")
	}
}
