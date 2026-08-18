// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package grab

import (
	"context"
	"strings"
	"testing"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

// bindFixture allocates a grab, binds it to a fresh job through the production
// fenced path, and forces updated_at so ordering is a property of the query
// rather than of how fast the test ran. store.Now() has second granularity, so
// three binds in one test would otherwise tie and fall through to the id
// tiebreak, which would let a reversed ORDER BY pass.
func bindFixture(t *testing.T, ctx context.Context, svc *Service, s *store.Store, title, jobID, boundAt string) string {
	t.Helper()
	now := store.Now()
	reqID := "req-" + jobID
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, reqID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, reqID, "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Allocate(ctx, "binds.example.org", title)
	if err != nil {
		t.Fatal(err)
	}
	prov := BindProvenance{
		Method:               "candidate_auto_bind",
		Rule:                 "candidate_auto_bind/3",
		Winner:               jobID,
		CandidatesConsidered: 2,
		Evidence:             []string{"title matched: " + title, "year matched"},
		Candidates: []CandidateVerdict{
			{JobID: jobID, Verdict: "qualifies"},
			{JobID: "job_loser", Verdict: "rejected", Reason: "year_mismatch"},
		},
		ExcerptSHA256: "sha-" + jobID,
	}
	if err := svc.MarkBoundToJobFenced(ctx, g.ID, jobID, "job_created", fixedDecision(prov)); err != nil {
		t.Fatalf("MarkBoundToJobFenced(%s): %v", title, err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE pdf_grabs SET updated_at = ? WHERE id = ?`, boundAt, g.ID); err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// TestListAutonomousBindsIsNewestFirstAndBounded covers the audit surface's two
// load-bearing properties. Newest-first matters because the operator's question
// is "what did papio just file while I wasn't looking", and a bound limit
// matters because this listing is the only recourse for a decision that has no
// unbind — an unbounded scan of a long-lived store is not a surface anyone reads.
func TestListAutonomousBindsIsNewestFirstAndBounded(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)

	oldest := bindFixture(t, ctx, svc, s, "Oldest Bind", "job_00000000000000000000000101", "2026-08-16T00:00:00Z")
	middle := bindFixture(t, ctx, svc, s, "Middle Bind", "job_00000000000000000000000102", "2026-08-17T00:00:00Z")
	newest := bindFixture(t, ctx, svc, s, "Newest Bind", "job_00000000000000000000000103", "2026-08-18T00:00:00Z")

	got, err := svc.ListAutonomousBinds(ctx, 10)
	if err != nil {
		t.Fatalf("ListAutonomousBinds: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 binds", len(got))
	}
	if got[0].GrabID != newest || got[1].GrabID != middle || got[2].GrabID != oldest {
		t.Fatalf("order = [%s %s %s], want newest first [%s %s %s]",
			got[0].GrabID, got[1].GrabID, got[2].GrabID, newest, middle, oldest)
	}

	// The provenance is the whole point of the row: an operator re-reading a
	// filing needs the rule version that decided it, the evidence it was made
	// on, and the losing candidates, not just the fact that something happened.
	first := got[0]
	if first.Provenance.Rule != "candidate_auto_bind/3" {
		t.Fatalf("rule = %q, want candidate_auto_bind/3", first.Provenance.Rule)
	}
	if first.Provenance.Winner != "job_00000000000000000000000103" || first.JobID != first.Provenance.Winner {
		t.Fatalf("winner = %q, job_id = %q, want both the bound job", first.Provenance.Winner, first.JobID)
	}
	if len(first.Provenance.Evidence) != 2 || !strings.Contains(first.Provenance.Evidence[0], "Newest Bind") {
		t.Fatalf("evidence = %v, want the winner's evidence preserved", first.Provenance.Evidence)
	}
	if len(first.Provenance.Candidates) != 2 || first.Provenance.Candidates[1].Reason != "year_mismatch" {
		t.Fatalf("candidates = %+v, want both verdicts with the loser's reason", first.Provenance.Candidates)
	}
	if first.BoundAt.IsZero() {
		t.Fatal("bound_at is zero, want the instant the bind committed")
	}
	if !first.BoundAt.After(got[2].BoundAt) {
		t.Fatalf("bound_at %v not after oldest %v", first.BoundAt, got[2].BoundAt)
	}

	limited, err := svc.ListAutonomousBinds(ctx, 2)
	if err != nil {
		t.Fatalf("ListAutonomousBinds(2): %v", err)
	}
	if len(limited) != 2 || limited[0].GrabID != newest || limited[1].GrabID != middle {
		t.Fatalf("limited = %+v, want the two newest", limited)
	}
}

// TestListAutonomousBindsExcludesOtherMethods pins the filter. A human running
// grabs.identify writes no provenance at all today, so the WHERE clause alone
// would be enough — but this listing's entire claim is "these are the filings
// nobody approved", and that claim must survive the column being reused for a
// method that did involve a human. The filter is what makes the claim durable.
func TestListAutonomousBindsExcludesOtherMethods(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)

	auto := bindFixture(t, ctx, svc, s, "Automatic Bind", "job_00000000000000000000000201", "2026-08-18T00:00:00Z")
	other := bindFixture(t, ctx, svc, s, "Human Bind", "job_00000000000000000000000202", "2026-08-18T01:00:00Z")
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE pdf_grabs SET bind_provenance = ? WHERE id = ?`,
		`{"method":"human_identify","rule":"operator/1","winner":"job_00000000000000000000000202"}`, other); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListAutonomousBinds(ctx, 10)
	if err != nil {
		t.Fatalf("ListAutonomousBinds: %v", err)
	}
	if len(got) != 1 || got[0].GrabID != auto {
		t.Fatalf("got %d rows %+v, want only the candidate_auto_bind row %s", len(got), got, auto)
	}

	// Unreadable provenance is a defect the operator must be told about. An
	// audit listing that silently drops the one row it cannot explain is worse
	// than one that fails, because the gap is invisible.
	if _, err := s.DB().ExecContext(ctx, `UPDATE pdf_grabs SET bind_provenance = ? WHERE id = ?`, `{not json`, auto); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListAutonomousBinds(ctx, 10); err == nil {
		t.Fatal("unparseable provenance returned no error, want the row surfaced as a defect")
	}
}
