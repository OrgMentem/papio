// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// State-machine behavior: CAS transitions, idempotent submission, lease
// claiming, and the crash-recovery rewind that keeps re-fetches duplicate-free.

package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/store"
	"papio/internal/work"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return &Store{S: s}
}

func testPolicy() Policy {
	return Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}
}

func testWork() work.Work {
	return work.Work{DOI: "10.1002/example", Title: "An Example Paper", Authors: []string{"Author, A."}, Year: 2020}
}

func TestCreateRequestIsIdempotent(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	j1, err := js.CreateRequest(ctx, "wr_test_0001", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	j2, err := js.CreateRequest(ctx, "wr_test_0001", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if j1 != j2 {
		t.Fatalf("resubmission created a second live job: %s vs %s", j1, j2)
	}
	row, err := js.Get(ctx, j1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.State != StateQueued || row.Work.DOI != "10.1002/example" || row.Policy.AccessMode != "conservative" {
		t.Fatalf("row = %+v, want queued job carrying work identity and policy snapshot", row)
	}
}

func TestTransitionCASRejectsWrongFromState(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0002", testWork(), "", "", testPolicy(), nil)

	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("queued->resolving: %v", err)
	}
	// Replaying the same transition must fail: the job is no longer queued.
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay err = %v, want ErrConflict", err)
	}
	// Disallowed edges fail closed.
	if err := js.Transition(ctx, id, StateResolving, StateValidating, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolving->validating err = %v, want ErrConflict (not an allowed edge)", err)
	}
}

func TestTerminalTransitionRecordsReasonAndClearsLease(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0003", testWork(), "", "", testPolicy(), nil)
	if _, err := js.ClaimNext(ctx, "owner1", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateUnavailable, nil, WithTerminalReason("no candidates")); err != nil {
		t.Fatalf("to unavailable: %v", err)
	}
	row, _ := js.Get(ctx, id)
	if row.TerminalReason != "no candidates" {
		t.Fatalf("terminal reason = %q", row.TerminalReason)
	}
	// Terminal jobs are not claimable.
	claimed, err := js.ClaimNext(ctx, "owner2", time.Minute)
	if err != nil || claimed != nil {
		t.Fatalf("claimed terminal job %v, %v", claimed, err)
	}
}

func TestClaimNextHonorsLeasesAndRetryAt(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0004", testWork(), "", "", testPolicy(), nil)

	got, err := js.ClaimNext(ctx, "owner1", time.Minute)
	if err != nil || got == nil || got.ID != id {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	// Live lease blocks a second claim.
	if again, _ := js.ClaimNext(ctx, "owner2", time.Minute); again != nil {
		t.Fatalf("second claim stole a live lease: %+v", again)
	}

	// retry_wait in the future is not runnable; due retry_wait is.
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateRetryWait, nil, WithRetryAt(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := js.ClaimNext(ctx, "owner1", time.Minute); claimed != nil {
		t.Fatalf("claimed a not-yet-due retry_wait job")
	}
	if err := js.Transition(ctx, id, StateRetryWait, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateRetryWait, nil, WithRetryAt(time.Now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	claimed, err := js.ClaimNext(ctx, "owner1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("due retry_wait not claimable: %v, %v", claimed, err)
	}
}

func TestRecoverStaleRewindsMidflightToResolving(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0005", testWork(), "", "", testPolicy(), nil)

	// Simulate a crashed daemon: job mid-fetch with an expired lease.
	if _, err := js.ClaimNext(ctx, "dead-daemon", -time.Second); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateFetching, nil); err != nil {
		t.Fatal(err)
	}

	recovered, err := js.RecoverStale(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != id {
		t.Fatalf("recovered = %v, want [%s]", recovered, id)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateResolving {
		t.Fatalf("state after recovery = %s, want resolving (bearer URLs are memory-only)", row.State)
	}
}

func TestRecoveredResolvingJobIsImmediatelyClaimable(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_reclaim", testWork(), "", "", testPolicy(), nil)
	if _, err := js.ClaimNext(ctx, "dead-daemon", -time.Second); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if recovered, err := js.RecoverStale(ctx); err != nil || len(recovered) != 1 {
		t.Fatalf("recover = %v, %v", recovered, err)
	}
	claimed, err := js.ClaimNext(ctx, "replacement-daemon", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != id {
		t.Fatalf("claim after recovery = %+v", claimed)
	}
}

func TestRetryReopensFailedJobOnlyByExplicitCommand(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_retry", testWork(), "", "", testPolicy(), nil)
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateFailed, nil, WithTerminalReason("network exhausted")); err != nil {
		t.Fatal(err)
	}
	if err := js.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateResolving || row.TerminalReason != "" {
		t.Fatalf("retried row = %+v", row)
	}
	if err := js.Retry(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("second retry err = %v, want ErrConflict", err)
	}
}

func TestCandidatesDedupeAndOrder(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0006", testWork(), "", "", testPolicy(), nil)

	cands := []Candidate{
		{JobID: id, Source: "unpaywall", URLRedacted: "https://x/1", URLKey: "k1", Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 1},
		{JobID: id, Source: "arxiv", URLRedacted: "https://x/0", URLKey: "k0", Version: "preprint", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 0},
		{JobID: id, Source: "arxiv", URLRedacted: "https://x/0", URLKey: "k0", Version: "preprint", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 0}, // dup
	}
	n, err := js.InsertCandidates(ctx, id, cands)
	if err != nil || n != 2 {
		t.Fatalf("inserted %d, %v; want 2 (dedupe by url_key)", n, err)
	}
	c, err := js.NextPendingCandidate(ctx, id)
	if err != nil || c == nil || c.URLKey != "k0" {
		t.Fatalf("next = %+v, %v; want rank-0 candidate", c, err)
	}
	if err := js.MarkCandidate(ctx, c.ID, "invalid"); err != nil {
		t.Fatal(err)
	}
	c2, _ := js.NextPendingCandidate(ctx, id)
	if c2 == nil || c2.URLKey != "k1" {
		t.Fatalf("after marking invalid, next = %+v; want k1", c2)
	}
	if err := js.MarkCandidate(ctx, c2.ID, "invalid"); err != nil {
		t.Fatal(err)
	}
	c3, _ := js.NextPendingCandidate(ctx, id)
	if c3 != nil {
		t.Fatalf("exhausted job still yields candidate %+v", c3)
	}
}

func TestArtifactCacheByDOI(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_test_0007", testWork(), "", "", testPolicy(), nil)

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := js.UpsertArtifact(ctx, Artifact{SHA256: sha, SizeBytes: 10, MIME: "application/pdf", PageCount: 3, Path: "/tmp/x.pdf", IdentityResult: "pass"}); err != nil {
		t.Fatal(err)
	}
	// Upsert again (content-addressed idempotency).
	if err := js.UpsertArtifact(ctx, Artifact{SHA256: sha, SizeBytes: 10, MIME: "application/pdf", PageCount: 3, Path: "/tmp/x.pdf", IdentityResult: "pass"}); err != nil {
		t.Fatal(err)
	}

	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateFetching, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateFetching, StateValidating, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateValidating, StateReady, nil, WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}

	hit, err := js.FindArtifactByDOI(ctx, "10.1002/example")
	if err != nil || hit == nil || hit.SHA256 != sha {
		t.Fatalf("cache lookup = %+v, %v; want artifact %s", hit, err, sha)
	}
	miss, err := js.FindArtifactByDOI(ctx, "10.9999/other")
	if err != nil || miss != nil {
		t.Fatalf("cache miss lookup = %+v, %v; want nil", miss, err)
	}
}

func TestFillWorkMetadataOnlyFillsMissingAndRejectsIdentifierConflict(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_metadata_01",
		work.Work{DOI: "10.1002/example", Title: "Requested Title"}, "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := js.FillWorkMetadata(ctx, id, work.Work{
		DOI: "10.1002/example", Title: "Resolver Title", Authors: []string{"Ada Lovelace"}, Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Work.Title != "Requested Title" || len(row.Work.Authors) != 1 || row.Work.Year != 2024 {
		t.Fatalf("fill result = %+v; request title should win, missing fields should fill", row.Work)
	}
	if _, err := js.FillWorkMetadata(ctx, id, work.Work{DOI: "10.9999/wrong"}); err == nil {
		t.Fatal("conflicting resolver DOI was accepted")
	}
	got, _ := js.Get(ctx, id)
	if got.Work.DOI != "10.1002/example" {
		t.Fatalf("conflict mutated DOI to %q", got.Work.DOI)
	}
}

func TestReserveCostIsDurableAndAtomic(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_cost_0001", testWork(), "", "", testPolicy(), nil)
	limit := 1.0
	if err := js.ReserveCost(ctx, id, "paid", 0.6, &limit); err != nil {
		t.Fatal(err)
	}
	err := js.ReserveCost(ctx, id, "paid", 0.41, &limit)
	var exceeded *ErrCostExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("second reservation = %v, want ErrCostExceeded", err)
	}
	row, _ := js.Get(ctx, id)
	if row.SpentUSD != 0.6 {
		t.Fatalf("spent = %.2f, rejected reservation changed it", row.SpentUSD)
	}
	if err := js.ReserveCost(ctx, id, "free", 0, &limit); err != nil {
		t.Fatal(err)
	}
}

func TestCancelIsIdempotentAndNeverOverwritesTerminalResult(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_cancel_001", testWork(), "", "", testPolicy(), nil)
	if err := js.Cancel(ctx, id, "user request"); err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, id, "again"); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateCancelled || row.TerminalReason != "user request" {
		t.Fatalf("cancelled row = %+v", row)
	}

	readyID, _ := js.CreateRequest(ctx, "wr_ready_term", testWork(), "", "", testPolicy(), nil)
	if err := js.Transition(ctx, readyID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, readyID, StateResolving, StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, readyID, "too late"); err != nil {
		t.Fatal(err)
	}
	ready, _ := js.Get(ctx, readyID)
	if ready.State != StateReady {
		t.Fatalf("cancel overwrote ready terminal state: %s", ready.State)
	}
}
