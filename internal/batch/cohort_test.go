// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/store"
)

func cohortStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func browserSource() Source {
	return Source{Kind: "browser_page", Label: "https://results.example", Detector: "detector-v1", ScanID: "scan-1"}
}

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "doi:10.1/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	return out
}

func outcomes(in []string, outcome, jobPrefix string) []MemberOutcome {
	out := make([]MemberOutcome, len(in))
	for i, key := range in {
		out[i] = MemberOutcome{CanonicalKey: key, Outcome: outcome, JobID: jobPrefix + key}
	}
	return out
}

func TestValidateChunkDeterministicManifest(t *testing.T) {
	manifest := keys(133)
	for index, size := range []int{50, 50, 33} {
		req := ChunkRequest{RequestID: "req-" + string(rune('0'+index)), CohortID: "cohort-1", Source: browserSource(), CohortTotal: len(manifest), ChunkIndex: index, FinalChunk: index == 2, CanonicalKeys: manifest[index*50 : index*50+size]}
		if err := validateChunkRequest(req); err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
	}
	bad := ChunkRequest{RequestID: "req-bad", CohortID: "cohort-1", Source: browserSource(), CohortTotal: 133, ChunkIndex: 2, FinalChunk: false, CanonicalKeys: manifest[100:]}
	if err := validateChunkRequest(bad); err == nil {
		t.Fatal("early final flag accepted")
	}
}

func TestSubmitChunkReplayAndConflictBeforeSubmit(t *testing.T) {
	ctx := context.Background()
	cohorts := New(cohortStore(t))
	manifest := keys(1)
	req := ChunkRequest{RequestID: "request-1", CohortID: "cohort-1", Source: browserSource(), CohortTotal: 1, ChunkIndex: 0, FinalChunk: true, CanonicalKeys: manifest}
	calls := 0
	submit := func(context.Context, []string) ([]MemberOutcome, error) {
		calls++
		return outcomes(manifest, "already_owned", ""), nil
	}
	first, err := cohorts.SubmitChunk(ctx, req, submit)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.CohortTotal == nil || first.PersistedMembers != 1 {
		t.Fatalf("first = %+v calls=%d", first, calls)
	}
	replay, err := cohorts.SubmitChunk(ctx, req, func(context.Context, []string) ([]MemberOutcome, error) {
		calls++
		return nil, errors.New("must not submit")
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || replay.BatchID != first.BatchID || replay.Membership != first.Membership {
		t.Fatalf("replay = %+v calls=%d", replay, calls)
	}
	conflict := req
	conflict.CanonicalKeys = []string{"doi:10.1/different"}
	_, conflictErr := cohorts.SubmitChunk(ctx, conflict, func(context.Context, []string) ([]MemberOutcome, error) { calls++; return nil, nil })
	if conflictErr == nil {
		t.Fatal("conflicting replay accepted")
	}
	if _, ok := conflictErr.(*ConflictError); !ok {
		t.Fatalf("error = %T, want ConflictError", conflictErr)
	}
	if calls != 1 {
		t.Fatalf("conflict called submit: %d", calls)
	}
}
func TestConflictingFirstChunkLeavesNoCohortRows(t *testing.T) {
	ctx := context.Background()
	s := cohortStore(t)
	cohorts := New(s)
	req := ChunkRequest{
		RequestID: "request-gap-first", CohortID: "cohort-gap-first", Source: browserSource(),
		CohortTotal: 51, ChunkIndex: 1, FinalChunk: true, CanonicalKeys: keys(1),
	}
	if _, err := cohorts.SubmitChunk(ctx, req, func(context.Context, []string) ([]MemberOutcome, error) {
		t.Fatal("conflicting chunk must not invoke submission")
		return nil, nil
	}); err == nil {
		t.Fatal("conflicting first chunk accepted")
	}
	for _, table := range []string{"acquisition_batches", "acquisition_batch_chunks", "acquisition_batch_members"} {
		var count int
		if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows = %d, want 0", table, count)
		}
	}
}

func TestSubmitChunkSequenceAndDuplicateKey(t *testing.T) {
	ctx := context.Background()
	cohorts := New(cohortStore(t))
	manifest := keys(51)
	first := ChunkRequest{RequestID: "request-1", CohortID: "cohort-1", Source: browserSource(), CohortTotal: 51, ChunkIndex: 0, CanonicalKeys: manifest[:50]}
	if _, err := cohorts.SubmitChunk(ctx, first, func(context.Context, []string) ([]MemberOutcome, error) {
		return outcomes(manifest[:50], "submitted", "job-"), nil
	}); err != nil {
		t.Fatal(err)
	}
	gap := first
	gap.RequestID = "request-3"
	gap.ChunkIndex = 2
	gap.FinalChunk = true
	gap.CanonicalKeys = manifest[50:]
	if _, err := cohorts.SubmitChunk(ctx, gap, func(context.Context, []string) ([]MemberOutcome, error) {
		return outcomes(gap.CanonicalKeys, "submitted", "job-"), nil
	}); err == nil {
		t.Fatal("out-of-sequence chunk accepted")
	}
	second := first
	second.RequestID = "request-2"
	second.ChunkIndex = 1
	second.FinalChunk = true
	second.CanonicalKeys = manifest[50:]
	if _, err := cohorts.SubmitChunk(ctx, second, func(context.Context, []string) ([]MemberOutcome, error) {
		return outcomes(second.CanonicalKeys, "submitted", "job-"), nil
	}); err != nil {
		t.Fatal(err)
	}
	// A different cohort is an independent manifest, so it starts at chunk zero
	// and may legitimately contain a key another cohort already owns.
	dup := second
	dup.RequestID = "request-4"
	dup.CohortID = "cohort-2"
	dup.CanonicalKeys = []string{manifest[0]}
	dup.CohortTotal = 1
	dup.ChunkIndex = 0
	dup.FinalChunk = true
	if _, err := cohorts.SubmitChunk(ctx, dup, func(context.Context, []string) ([]MemberOutcome, error) {
		return outcomes(dup.CanonicalKeys, "invalid", ""), nil
	}); err != nil {
		t.Fatalf("independent cohort should allow same key: %v", err)
	}
}

func TestPartialProjectionOmitsDenominatorAfterInactivity(t *testing.T) {
	s := cohortStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cohorts := New(s)
	cohorts.Now = func() time.Time { return now }
	req := ChunkRequest{RequestID: "request-1", CohortID: "cohort-1", Source: browserSource(), CohortTotal: 51, ChunkIndex: 0, CanonicalKeys: keys(50)}
	if _, err := cohorts.SubmitChunk(context.Background(), req, func(context.Context, []string) ([]MemberOutcome, error) {
		return outcomes(req.CanonicalKeys, "invalid", ""), nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := cohorts.Projection(context.Background(), "missing", now.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatal("missing batch projected")
	}
	var id string
	if err := s.DB().QueryRowContext(context.Background(), `SELECT id FROM acquisition_batches WHERE cohort_id='cohort-1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	p, err = cohorts.Projection(context.Background(), id, now.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if p.Membership != "partial" || p.ProjectionComplete || p.Total != nil {
		t.Fatalf("partial projection = %+v", p)
	}
}
