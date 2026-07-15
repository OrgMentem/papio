// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// State-machine behavior: CAS transitions, idempotent submission, lease
// claiming, and the crash-recovery rewind that keeps re-fetches duplicate-free.

package job

import (
	"context"
	"database/sql"
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
	return Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "institute", FetchMaxBytes: 1 << 20}
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
	if row.State != StateQueued || row.Work.DOI != "10.1002/example" || row.Policy.AccessMode != "conservative" || row.Policy.Resolver != "institute" {
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

func TestCancelClosesAllOpenHumanActions(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_cancel_actions", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"openurl_handoff", "verify_identity"} {
		if _, err := js.OpenHumanAction(ctx, id, kind, "pending"); err != nil {
			t.Fatalf("open %s action: %v", kind, err)
		}
	}

	if err := js.Cancel(ctx, id, "user request"); err != nil {
		t.Fatal(err)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %+v, want two", actions)
	}
	for _, action := range actions {
		if action.Status != "cancelled" {
			t.Fatalf("action %q status = %q, want cancelled", action.Kind, action.Status)
		}
	}
}

func TestReadyTransitionResolvesOpenHumanActions(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_ready_actions", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "pending"); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateReady, nil); err != nil {
		t.Fatal(err)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Status != "resolved" {
		t.Fatalf("actions = %+v, want one resolved", actions)
	}
	open, err := js.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open actions = %+v, want none", open)
	}
}

func TestCloseStaleHumanActionsClosesOnlyTerminalJobs(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	terminalStates := []string{StateReady, StateUnavailable, StateFailed, StateCancelled}
	terminalIDs := make(map[string]bool, len(terminalStates))
	for _, state := range terminalStates {
		id, err := js.CreateRequest(ctx, "wr_stale_"+state, testWork(), "", "", testPolicy(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := js.OpenHumanAction(ctx, id, "stale_"+state, "stale"); err != nil {
			t.Fatal(err)
		}
		terminalIDs[id] = true
	}
	awaitingID, err := js.CreateRequest(ctx, "wr_stale_awaiting", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, awaitingID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, awaitingID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, awaitingID, "manual_download", "pending"); err != nil {
		t.Fatal(err)
	}

	if err := js.CloseStaleHumanActions(ctx); err != nil {
		t.Fatal(err)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != len(terminalIDs)+1 {
		t.Fatalf("actions = %+v", actions)
	}
	for _, action := range actions {
		want := "cancelled"
		if action.JobID == awaitingID {
			want = "open"
		} else if !terminalIDs[action.JobID] {
			t.Fatalf("unexpected action job %q", action.JobID)
		}
		if action.Status != want {
			t.Fatalf("action %+v status = %q, want %q", action, action.Status, want)
		}
	}
}

func TestConservativeAdvisorySurvivesTerminalCloseAndSweep(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_advisory", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	// Conservative mode records the advisory, then ends the job unavailable.
	if _, err := js.OpenHumanAction(ctx, id, "openurl_available", "not opened in conservative mode"); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateUnavailable, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.CloseStaleHumanActions(ctx); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "openurl_available" {
		t.Fatalf("open actions = %+v, want the surviving advisory", open)
	}
}

func TestOpenHumanActionRefreshesExistingOpenKind(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_action_dedupe", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := js.OpenHumanAction(ctx, id, "terms_acceptance_required", "first detail")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := js.OpenHumanAction(ctx, id, "terms_acceptance_required", "latest detail")
	if err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Fatalf("second action ID = %d, want existing ID %d", secondID, firstID)
	}
	otherID, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "other detail")
	if err != nil {
		t.Fatal(err)
	}
	if otherID == firstID {
		t.Fatalf("different action kind reused ID %d", otherID)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %+v, want two", actions)
	}
	for _, action := range actions {
		switch action.ID {
		case firstID:
			if action.Detail != "latest detail" || action.Status != "open" {
				t.Fatalf("refreshed action = %+v", action)
			}
		case otherID:
			if action.Kind != "openurl_handoff" || action.Detail != "other detail" {
				t.Fatalf("other action = %+v", action)
			}
		default:
			t.Fatalf("unexpected action = %+v", action)
		}
	}
}

func TestAwaitingHumanResumeEdgesForBrowserBridge(t *testing.T) {
	// The Phase 2 bridge parks handoffs in awaiting_human and then resumes them
	// directly: to validating (adopting a download, under a held lease) or to a
	// terminal/review/retry state driven by the extension's provider outcome.
	for _, to := range []string{StateValidating, StateUnavailable, StateNeedsReview, StateRetryWait} {
		if !allowed[StateAwaitingHuman][to] {
			t.Fatalf("awaiting_human->%s must be an allowed resume edge", to)
		}
	}
	js := testStore(t)
	ctx := context.Background()
	id, _ := js.CreateRequest(ctx, "wr_awaiting_edges", testWork(), "", "", testPolicy(), nil)
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateAwaitingHuman, StateUnavailable, nil, WithTerminalReason("browser_rejected")); err != nil {
		t.Fatalf("awaiting_human->unavailable: %v", err)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateUnavailable || row.TerminalReason != "browser_rejected" {
		t.Fatalf("row = %+v", row)
	}
}

func parkIdentityReview(t *testing.T, js *Store, requestID string) (string, int64, int64) {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.InsertCandidates(ctx, id, []Candidate{{
		JobID: id, Source: "fixture", URLRedacted: "https://example.test/paper.pdf", URLKey: requestID,
		Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := js.NextPendingCandidate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{StateQueued, StateResolving},
		{StateResolving, StateFetching},
		{StateFetching, StateValidating},
		{StateValidating, StateNeedsReview},
	} {
		if err := js.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	attempt, err := js.StartAttempt(ctx, id, candidate.ID, "validate", candidate.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.FinishAttempt(ctx, attempt, "needs_review", 0, "semantic_or_identity_review"); err != nil {
		t.Fatal(err)
	}
	if err := js.MarkCandidate(ctx, candidate.ID, "skipped"); err != nil {
		t.Fatal(err)
	}
	actionID, err := js.OpenHumanAction(ctx, id, "verify_identity", "local quarantine file: /tmp/paper.pdf")
	if err != nil {
		t.Fatal(err)
	}
	return id, candidate.ID, actionID
}

func TestResolveHumanActionRequiresOpenAction(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	if err := js.ResolveHumanAction(ctx, 999, "resolved"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing action error = %v, want sql.ErrNoRows", err)
	}
	if _, _, err := js.ResolveReview(ctx, 999, "accept"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing review action error = %v, want sql.ErrNoRows", err)
	}
	id, _, actionID := parkIdentityReview(t, js, "wr_review_closed")
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolved action error = %v, want ErrConflict", err)
	}
	if _, _, err := js.ResolveReview(ctx, actionID, "accept"); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-open review action error = %v, want ErrConflict", err)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateNeedsReview {
		t.Fatalf("generic action resolution changed job state to %s", row.State)
	}
}

func TestResolveReviewRejectCancelsJobAndResolvesAction(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _, actionID := parkIdentityReview(t, js, "wr_review_reject")
	jobID, state, err := js.ResolveReview(ctx, actionID, "reject")
	if err != nil {
		t.Fatal(err)
	}
	if jobID != id || state != StateCancelled {
		t.Fatalf("resolution = %q, %q", jobID, state)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateCancelled || row.TerminalReason != "review_rejected" {
		t.Fatalf("rejected row = %+v", row)
	}
	actions, _ := js.ListHumanActions(ctx, false)
	if len(actions) != 1 || actions[0].Status != "resolved" {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestResolveReviewAcceptResumesCandidateAndClearsTerminalFields(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, candidateID, actionID := parkIdentityReview(t, js, "wr_review_accept")
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET terminal_reason = 'stale', retry_at = '2099-01-01T00:00:00Z',
		        selected_candidate_id = ?, artifact_sha256 = 'stale' WHERE id = ?`, candidateID, id); err != nil {
		t.Fatal(err)
	}
	_, state, err := js.ResolveReview(ctx, actionID, "accept")
	if err != nil {
		t.Fatal(err)
	}
	if state != StateFetching {
		t.Fatalf("accept state = %s, want fetching", state)
	}
	row, _ := js.Get(ctx, id)
	if row.TerminalReason != "" || row.RetryAt != "" || row.SelectedCandidateID != 0 || row.ArtifactSHA256 != "" {
		t.Fatalf("accept left terminal fields behind: %+v", row)
	}
	candidate, err := js.GetCandidate(ctx, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != "pending" || !candidate.ReviewOverride {
		t.Fatalf("accepted candidate = %+v", candidate)
	}
	if claimed, err := js.ClaimNext(ctx, "review-worker", time.Minute); err != nil || claimed == nil || claimed.ID != id {
		t.Fatalf("accepted review was not immediately claimable: %+v, %v", claimed, err)
	}
}
