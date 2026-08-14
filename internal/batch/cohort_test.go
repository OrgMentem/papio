// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	conflictError := &ConflictError{}
	if !errors.As(conflictErr, &conflictError) {
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

// liveCohort mirrors the 2026-08-12 browser cohort: 199 canonical keys split
// 50/50/50/49, whose chunk rows were written before the cache had explicit
// json tags and whose jobs are still executing.
func liveCohortChunks() []ChunkRequest {
	manifest := keys(199)
	sizes := []int{50, 50, 50, 49}
	reqs := make([]ChunkRequest, len(sizes))
	offset := 0
	for i, size := range sizes {
		reqs[i] = ChunkRequest{
			RequestID: fmt.Sprintf("live-request-%d", i), CohortID: "live-cohort", Source: browserSource(),
			CohortTotal: 199, ChunkIndex: i, FinalChunk: i == len(sizes)-1,
			CanonicalKeys: manifest[offset : offset+size],
		}
		offset += size
	}
	return reqs
}

func storedResultJSON(t *testing.T, s *store.Store, batchID string, index int) string {
	t.Helper()
	var raw string
	if err := s.DB().QueryRowContext(context.Background(), `SELECT result_json FROM acquisition_batch_chunks WHERE batch_id=? AND chunk_index=?`, batchID, index).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// goCasedResultJSON reproduces the exact bytes the cache held before
// persistedChunkResult existed, when it was marshalled straight from
// ChunkResult. It is a literal rather than a re-marshal of ChunkResult so that
// later changes to that type cannot quietly stop exercising the legacy path.
func goCasedResultJSON(r ChunkResult) string {
	total := "null"
	if r.CohortTotal != nil {
		total = fmt.Sprintf("%d", *r.CohortTotal)
	}
	return fmt.Sprintf(`{"BatchID":%q,"Membership":%q,"CohortTotal":%s,"PersistedMembers":%d,"Submitted":%d,"Joined":%d,"AlreadyOwned":%d,"Invalid":%d}`,
		r.BatchID, r.Membership, total, r.PersistedMembers, r.Submitted, r.Joined, r.AlreadyOwned, r.Invalid)
}

func submitLiveCohort(t *testing.T, cohorts *Cohorts) []ChunkResult {
	t.Helper()
	ctx := context.Background()
	results := make([]ChunkResult, 0, 4)
	for _, req := range liveCohortChunks() {
		res, err := cohorts.SubmitChunk(ctx, req, func(_ context.Context, k []string) ([]MemberOutcome, error) {
			return outcomes(k, "submitted", "job-"), nil
		})
		if err != nil {
			t.Fatalf("chunk %d: %v", req.ChunkIndex, err)
		}
		results = append(results, res)
	}
	return results
}

func TestChunkCachePersistsWireNames(t *testing.T) {
	s := cohortStore(t)
	results := submitLiveCohort(t, New(s))
	raw := storedResultJSON(t, s, results[1].BatchID, 1)
	want := fmt.Sprintf(`{"batch_id":%q,"membership":"open","cohort_total":null,"persisted_members":100,"submitted":50,"joined":0,"already_owned":0,"invalid":0}`, results[1].BatchID)
	if raw != want {
		t.Fatalf("stored chunk 1 = %s, want %s", raw, want)
	}
	final := storedResultJSON(t, s, results[3].BatchID, 3)
	wantFinal := fmt.Sprintf(`{"batch_id":%q,"membership":"complete","cohort_total":199,"persisted_members":199,"submitted":49,"joined":0,"already_owned":0,"invalid":0}`, results[3].BatchID)
	if final != wantFinal {
		t.Fatalf("stored chunk 3 = %s, want %s", final, wantFinal)
	}
	decoded, err := decodeChunkResult([]byte(final))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.BatchID != results[3].BatchID || decoded.Membership != "complete" || decoded.CohortTotal == nil || *decoded.CohortTotal != 199 || decoded.PersistedMembers != 199 || decoded.Submitted != 49 {
		t.Fatalf("round trip = %+v", decoded)
	}
}

func TestLegacyGoCasedChunkCacheReplaysWithoutResubmitting(t *testing.T) {
	ctx := context.Background()
	s := cohortStore(t)
	cohorts := New(s)
	original := submitLiveCohort(t, cohorts)

	// Rewrite every chunk row into the shape the live database holds.
	for i, res := range original {
		if _, err := s.DB().ExecContext(ctx, `UPDATE acquisition_batch_chunks SET result_json=? WHERE batch_id=? AND chunk_index=?`, goCasedResultJSON(res), res.BatchID, i); err != nil {
			t.Fatal(err)
		}
	}
	if raw := storedResultJSON(t, s, original[0].BatchID, 0); !strings.Contains(raw, `"PersistedMembers"`) || strings.Contains(raw, `"persisted_members"`) {
		t.Fatalf("fixture is not the legacy shape: %s", raw)
	}

	wantCumulative := []int{50, 100, 150, 199}
	wantSubmitted := []int{50, 50, 50, 49}
	for i, req := range liveCohortChunks() {
		replay, err := cohorts.SubmitChunk(ctx, req, func(context.Context, []string) ([]MemberOutcome, error) {
			t.Fatalf("chunk %d replay re-invoked submit", req.ChunkIndex)
			return nil, nil
		})
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if replay.Membership != original[i].Membership || replay.PersistedMembers != original[i].PersistedMembers {
			t.Fatalf("chunk %d replay = %+v, want %+v", i, replay, original[i])
		}
		if replay.BatchID != original[i].BatchID || replay.PersistedMembers != wantCumulative[i] || replay.Submitted != wantSubmitted[i] {
			t.Fatalf("chunk %d replay = %+v", i, replay)
		}
		if replay.Joined != 0 || replay.AlreadyOwned != 0 || replay.Invalid != 0 {
			t.Fatalf("chunk %d replay counts = %+v", i, replay)
		}
		if i < 3 {
			if replay.Membership != "open" || replay.CohortTotal != nil {
				t.Fatalf("chunk %d replay = %+v, want open with no denominator", i, replay)
			}
			continue
		}
		if replay.Membership != "complete" || replay.CohortTotal == nil || *replay.CohortTotal != 199 {
			t.Fatalf("final replay = %+v", replay)
		}
	}
}

func TestDecodeChunkResultRejectsUnrecognisedShape(t *testing.T) {
	// Neither decoder can name the batch, so the row cannot answer a replay and
	// must not silently decode to a zero-valued result.
	if _, err := decodeChunkResult([]byte(`{"cohort":"ab_1","members":100}`)); err == nil {
		t.Fatal("unrecognised cache shape accepted")
	}
	if _, err := decodeChunkResult([]byte(`{`)); err == nil {
		t.Fatal("malformed cache accepted")
	}
	legacy, err := decodeChunkResult([]byte(`{"BatchID":"ab_fb7b","Membership":"open","CohortTotal":null,"PersistedMembers":100,"Submitted":50,"Joined":0,"AlreadyOwned":0,"Invalid":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy != (ChunkResult{BatchID: "ab_fb7b", Membership: "open", PersistedMembers: 100, Submitted: 50}) {
		t.Fatalf("legacy decode = %+v", legacy)
	}
}
