// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// State-machine behavior: CAS transitions, idempotent submission, lease
// claiming, and the crash-recovery rewind that keeps re-fetches duplicate-free.

package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	j1, err := js.CreateRequest(ctx, "wr_test_0001", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	j2, err := js.CreateRequest(ctx, "wr_test_0001", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, _ := js.CreateRequest(ctx, "wr_test_0002", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)

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
	id, _ := js.CreateRequest(ctx, "wr_test_0003", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if _, err := js.ClaimNext(ctx, "owner1", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateUnavailable, nil, WithTerminalReason(TerminalReasonCandidatesExhausted)); err != nil {
		t.Fatalf("to unavailable: %v", err)
	}
	row, _ := js.Get(ctx, id)
	if row.TerminalReason != string(TerminalReasonCandidatesExhausted) {
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
	id, _ := js.CreateRequest(ctx, "wr_test_0004", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)

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
	id, _ := js.CreateRequest(ctx, "wr_test_0005", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)

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
	id, _ := js.CreateRequest(ctx, "wr_test_reclaim", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, _ := js.CreateRequest(ctx, "wr_test_retry", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, _ := js.CreateRequest(ctx, "wr_test_0006", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)

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
	id, _ := js.CreateRequest(ctx, "wr_test_0007", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)

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

	hit, source, err := js.FindArtifactByDOI(ctx, "10.1002/example")
	if err != nil || hit == nil || hit.SHA256 != sha {
		t.Fatalf("cache lookup = %+v, %v; want artifact %s", hit, err, sha)
	}
	// This fixture's source job reached ready with no candidate at all, so there
	// is no provenance to carry: the lookup must say so rather than guess.
	if source != nil {
		t.Fatalf("cache lookup source candidate = %+v, want nil for a job with no selected candidate", source)
	}
	miss, missSource, err := js.FindArtifactByDOI(ctx, "10.9999/other")
	if err != nil || miss != nil || missSource != nil {
		t.Fatalf("cache miss lookup = %+v/%+v, %v; want nil", miss, missSource, err)
	}
}

func TestFillWorkMetadataOnlyFillsMissingAndRejectsIdentifierConflict(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_metadata_01", work.Work{DOI: "10.1002/example", Title: "Requested Title"}, "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, _ := js.CreateRequest(ctx, "wr_cost_0001", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, _ := js.CreateRequest(ctx, "wr_cancel_001", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err := js.Cancel(ctx, id, TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, id, TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateCancelled || row.TerminalReason != string(TerminalReasonCancelledByUser) {
		t.Fatalf("cancelled row = %+v", row)
	}

	readyID, _ := js.CreateRequest(ctx, "wr_ready_term", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	id, err := js.CreateRequest(ctx, "wr_cancel_actions", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"openurl_handoff", "verify_identity"} {
		if _, err := js.OpenHumanAction(ctx, id, kind, "pending", Access(false, "")); err != nil {
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
	id, err := js.CreateRequest(ctx, "wr_ready_actions", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "pending", Access(false, "")); err != nil {
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
	terminalStates := []string{StateReady, StateImported, StateUnavailable, StateFailed, StateCancelled}
	terminalIDs := make(map[string]bool, len(terminalStates))
	for _, state := range terminalStates {
		id, err := js.CreateRequest(ctx, "wr_stale_"+state, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
		if err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if state == StateImported {
			// imported is only reachable through ready.
			if err := js.Transition(ctx, id, StateResolving, StateReady, nil); err != nil {
				t.Fatal(err)
			}
			if err := js.Transition(ctx, id, StateReady, StateImported, nil); err != nil {
				t.Fatal(err)
			}
		} else if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := js.OpenHumanAction(ctx, id, "stale_"+state, "stale", Access(false, "")); err != nil {
			t.Fatal(err)
		}
		terminalIDs[id] = true
	}
	awaitingID, err := js.CreateRequest(ctx, "wr_stale_awaiting", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, awaitingID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, awaitingID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, awaitingID, "manual_download", "pending", Access(false, "")); err != nil {
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
	id, err := js.CreateRequest(ctx, "wr_advisory", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	// Conservative mode records the advisory, then ends the job unavailable.
	if _, err := js.OpenHumanAction(ctx, id, "openurl_available", "not opened in conservative mode", Access(false, "")); err != nil {
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

// A retry is the user taking the advisory's own advice, so the advisory must
// not outlive it: the terminal sweep exempts the kind, and unavailable has no
// other outbound edge, so Retry is the only place that can clear it.
func TestRetryClearsTheConservativeAdvisory(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_advisory_retry", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "openurl_available", "not opened in conservative mode", Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateUnavailable, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open actions after retry = %+v, want the advisory cleared", open)
	}
	// The job now succeeds, and the advice it gave is not re-issued.
	if err := js.Transition(ctx, id, StateResolving, StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.CloseStaleHumanActions(ctx); err != nil {
		t.Fatal(err)
	}
	if open, err := js.ListHumanActions(ctx, true); err != nil || len(open) != 0 {
		t.Fatalf("open actions on the succeeded job = %+v, %v", open, err)
	}
}

func TestOpenHumanActionRefreshesExistingOpenKind(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_action_dedupe", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := js.OpenHumanAction(ctx, id, "terms_acceptance_required", "first detail", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := js.OpenHumanAction(ctx, id, "terms_acceptance_required", "latest detail", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Fatalf("second action ID = %d, want existing ID %d", secondID, firstID)
	}
	otherID, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "other detail", Access(false, ""))
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
			if action.Detail != "latest detail" || action.Status != "open" || action.Revision != 2 {
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
	id, _ := js.CreateRequest(ctx, "wr_awaiting_edges", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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

func TestRepairParkWithActionLeavesLateLeaseUntouched(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_repair_late_lease", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateNeedsReview, nil); err != nil {
		t.Fatal(err)
	}

	snapshot, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 0 {
		t.Fatalf("open action snapshot = %+v, want none", snapshot)
	}
	now := time.Now().UTC()
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ?`,
		"adopt-in-progress", now.Add(time.Minute).Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}

	err = js.RepairParkWithAction(ctx, id, StateNeedsReview, StateAwaitingHuman, nil,
		"manual_download", "download the requested PDF yourself",
		map[string]any{"reason": "stranded_handoff_repair"},
		Access(false, "landing_page"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("repair after adoption lease = %v, want ErrConflict", err)
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateNeedsReview || row.LeaseOwner != "adopt-in-progress" || !row.LeaseActive(time.Now()) {
		t.Fatalf("late lease did not protect review job: %+v", row)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("late lease let repair change actions: %+v", actions)
	}
}
func parkIdentityReview(t *testing.T, js *Store, requestID string) (string, int64, int64) {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
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
	actionID, err := js.OpenHumanAction(ctx, id, "verify_identity", "local quarantine file: /tmp/paper.pdf", Access(false, ""),
		WithHumanActionBinding(HumanActionBinding{
			CandidateID: candidate.ID, QuarantinePath: "/tmp/paper.pdf",
			QuarantineSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return id, candidate.ID, actionID
}

func TestResolveReviewCASOutcomes(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name     string
		revision int64
		sha      string
		want     ReviewOutcome
	}{
		{name: "wrong revision conflicts", revision: 2, sha: sha, want: ReviewConflict},
		{name: "wrong SHA conflicts", revision: 1, sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", want: ReviewConflict},
		{name: "applies", revision: 1, sha: sha, want: ReviewApplied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			js := testStore(t)
			id, _, actionID := parkIdentityReview(t, js, "wr_review_cas_"+test.name)
			got, err := js.ResolveReviewCAS(context.Background(), ResolveReviewInput{
				ActionID: actionID, Verdict: "accept", ExpectedRevision: test.revision, ExpectedSHA256: test.sha,
			})
			if err != nil || got.Outcome != test.want {
				t.Fatalf("ResolveReviewCAS() = %+v, %v; want %s, nil", got, err, test.want)
			}
			if test.want != ReviewApplied {
				row, _ := js.Get(context.Background(), id)
				if row.State != StateNeedsReview {
					t.Fatalf("conflicted resolution changed job = %+v", row)
				}
			}
		})
	}

	js := testStore(t)
	_, _, actionID := parkIdentityReview(t, js, "wr_review_cas_replay")
	input := ResolveReviewInput{
		ActionID: actionID, Verdict: "accept", ExpectedRevision: 1, ExpectedSHA256: sha,
	}
	if got, err := js.ResolveReviewCAS(context.Background(), input); err != nil || got.Outcome != ReviewApplied {
		t.Fatalf("first ResolveReviewCAS() = %+v, %v", got, err)
	}
	if got, err := js.ResolveReviewCAS(context.Background(), input); err != nil || got.Outcome != ReviewAlreadyApplied {
		t.Fatalf("replayed ResolveReviewCAS() = %+v, %v; want already_applied", got, err)
	}
}

func TestAcceptedReviewBindingRequiresPendingOverriddenCandidate(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	js := testStore(t)
	ctx := context.Background()
	id, candidateID, actionID := parkIdentityReview(t, js, "wr_review_binding")

	binding, err := js.AcceptedReviewBinding(ctx, id)
	if err != nil || binding != nil {
		t.Fatalf("open review binding = %+v, %v; want nil, nil", binding, err)
	}
	if resolution, err := js.ResolveReviewCAS(ctx, ResolveReviewInput{
		ActionID: actionID, Verdict: "accept", ExpectedRevision: 1, ExpectedSHA256: sha,
	}); err != nil || resolution.Outcome != ReviewApplied {
		t.Fatalf("ResolveReviewCAS() = %+v, %v", resolution, err)
	}
	binding, err = js.AcceptedReviewBinding(ctx, id)
	if err != nil || binding == nil ||
		binding.CandidateID != candidateID || binding.QuarantinePath != "/tmp/paper.pdf" || binding.QuarantineSHA256 != sha {
		t.Fatalf("accepted review binding = %+v, %v", binding, err)
	}
	if err := js.MarkCandidate(ctx, candidateID, "accepted"); err != nil {
		t.Fatal(err)
	}
	binding, err = js.AcceptedReviewBinding(ctx, id)
	if err != nil || binding != nil {
		t.Fatalf("completed candidate binding = %+v, %v; want nil, nil", binding, err)
	}
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

func TestDismissHumanActionCancelsNeedsReviewVerifyIdentity(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _, actionID := parkIdentityReview(t, js, "wr_dismiss")
	jobID, err := js.DismissHumanAction(ctx, actionID, 1)
	if err != nil || jobID != id {
		t.Fatalf("DismissHumanAction() = %q, %v; want %q, nil", jobID, err, id)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateCancelled || row.TerminalReason != "user_dismissed" {
		t.Fatalf("dismissed job = %+v, %v", row, err)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil || len(actions) != 1 || actions[0].Status != "cancelled" {
		t.Fatalf("actions = %+v, %v", actions, err)
	}
	if _, err := js.DismissHumanAction(ctx, actionID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed dismiss error = %v, want ErrConflict", err)
	}
}

func TestDismissHumanActionClosesStaleHandoffWithoutCancellingNeedsReview(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _, reviewID := parkIdentityReview(t, js, "wr_dismiss_stale_handoff")
	quarantine := filepath.Join(t.TempDir(), "quarantine.pdf")
	if err := os.WriteFile(quarantine, []byte("quarantined bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		UPDATE human_actions SET quarantine_path = ?, quarantine_sha256 = ?
		WHERE id = ?`,
		quarantine, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", reviewID); err != nil {
		t.Fatal(err)
	}
	handoffID, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "stale handoff", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := js.DismissHumanAction(ctx, handoffID, 1)
	if err != nil || jobID != id {
		t.Fatalf("DismissHumanAction() = %q, %v; want %q, nil", jobID, err, id)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateNeedsReview || row.TerminalReason != "" {
		t.Fatalf("stale handoff dismiss changed job = %+v, %v", row, err)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("stale handoff dismiss removed quarantine file: %v", err)
	}
	actions, err := js.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var handoff, review *HumanAction
	for i := range actions {
		action := &actions[i]
		switch action.ID {
		case handoffID:
			handoff = action
		case reviewID:
			review = action
		}
	}
	if handoff == nil || handoff.Status != "cancelled" {
		t.Fatalf("handoff action = %+v, want cancelled", handoff)
	}
	if review == nil || review.Status != "open" {
		t.Fatalf("verify_identity action = %+v, want open", review)
	}
}

func TestDismissHumanActionCancelsAwaitingHandoff(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_dismiss_awaiting_handoff", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{StateQueued, StateResolving},
		{StateResolving, StateAwaitingHuman},
	} {
		if err := js.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	actionID, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "institutional handoff", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.DismissHumanAction(ctx, actionID, 1); err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateCancelled || row.TerminalReason != "user_dismissed" {
		t.Fatalf("awaiting handoff dismiss = %+v, %v", row, err)
	}
}

func TestDismissHumanActionWrongRevisionConflicts(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, _, actionID := parkIdentityReview(t, js, "wr_dismiss_conflict")
	if _, err := js.DismissHumanAction(ctx, actionID, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-revision dismiss error = %v, want ErrConflict", err)
	}
	row, _ := js.Get(ctx, id)
	if row.State != StateNeedsReview {
		t.Fatalf("conflicted dismiss changed job state to %s", row.State)
	}
}

func TestDismissHumanActionMissingActionConflicts(t *testing.T) {
	js := testStore(t)
	if _, err := js.DismissHumanAction(context.Background(), 999999, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing action dismiss error = %v, want ErrConflict", err)
	}
}

// Regression: this is the exact production bug — a verify_identity action
// created before quarantine bindings were mandatory has an empty
// quarantine_path/quarantine_sha256, so it can never preview or be accepted,
// and before dismiss existed had no way to leave the inbox at all. Dismiss
// must close it without touching the (empty) binding fields.
func TestDismissHumanActionWorksWithoutQuarantineBinding(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_dismiss_unbound", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	actionID, err := js.OpenHumanAction(ctx, id, "verify_identity", "legacy row with no binding", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := js.DismissHumanAction(ctx, actionID, 1)
	if err != nil || jobID != id {
		t.Fatalf("DismissHumanAction() on unbound action = %q, %v; want %q, nil", jobID, err, id)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateCancelled {
		t.Fatalf("dismissed unbound job = %+v, %v", row, err)
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

// Asking for more rows must never return fewer. An over-large limit used to
// reset to the default, so --limit 600 yielded 100 where --limit 500 yielded
// 500 — silently, and in the direction of under-reporting, which is the worst
// way to be wrong for the only people who pass a large limit: the ones
// counting. Two separate consumers hit it on the same day.
func TestEffectiveListLimitNeverReturnsFewerForMore(t *testing.T) {
	if got := EffectiveListLimit(ListLimitMax + 100); got != ListLimitMax {
		t.Fatalf("EffectiveListLimit(%d) = %d, want the maximum %d", ListLimitMax+100, got, ListLimitMax)
	}
	if got := EffectiveListLimit(0); got != ListLimitDefault {
		t.Fatalf("unspecified limit = %d, want the default %d", got, ListLimitDefault)
	}
	if got := EffectiveListLimit(-5); got != ListLimitDefault {
		t.Fatalf("negative limit = %d, want the default %d", got, ListLimitDefault)
	}
	prev := 0
	for _, limit := range []int{1, 50, ListLimitDefault, ListLimitMax - 1, ListLimitMax, ListLimitMax + 1, 10000} {
		got := EffectiveListLimit(limit)
		if got < prev {
			t.Fatalf("EffectiveListLimit(%d) = %d, below the previous %d: asking for more returned fewer", limit, got, prev)
		}
		prev = got
	}
}
func TestListOldestRotatesPastPriorMaintenancePage(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	const pageSize = 2
	ids := make([]string, 0, pageSize+1)
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	for i := range pageSize + 1 {
		id, err := js.CreateRequest(ctx, NewID("wr_list_oldest"), testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE jobs SET state = ?, created_at = ? WHERE id = ?`,
			StateAwaitingHuman, start.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	first, err := js.ListOldest(ctx, []string{StateAwaitingHuman}, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range first {
		if row.ID == ids[pageSize] {
			t.Fatalf("first page included later job %q: %+v", ids[pageSize], first)
		}
	}
	second, err := js.ListOldest(ctx, []string{StateAwaitingHuman}, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range second {
		if row.ID == ids[pageSize] {
			return
		}
	}
	t.Fatalf("later job %q was not reached after the first page: %+v", ids[pageSize], second)
}

func TestAttemptedTiers(t *testing.T) {
	type testCase struct {
		name  string
		setup func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64)
		want  []string
	}
	cases := []testCase{
		{
			name: "ranked but never attempted is excluded",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				add("institutional", 0)
			},
		},
		{
			name: "started attempt is included",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				candidateID := add("institutional", 0)
				if _, err := js.StartAttempt(ctx, jobID, candidateID, "fetch", "fixture"); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"institutional"},
		},
		{
			name: "repeated attempts of one basis are deduplicated",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				for _, candidateID := range []int64{add("open_access", 0), add("open_access", 1)} {
					if _, err := js.StartAttempt(ctx, jobID, candidateID, "fetch", "fixture"); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: []string{"open_access"},
		},
		{
			name: "attempt survives candidate reset",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				candidateID := add("institutional", 0)
				if _, err := js.StartAttempt(ctx, jobID, candidateID, "fetch", "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := js.MarkCandidate(ctx, candidateID, "retryable"); err != nil {
					t.Fatal(err)
				}
				if err := js.ResetCandidates(ctx, jobID); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"institutional"},
		},
		{
			name: "prior tier survives retry when new policy does not revisit it",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				previous := add("institutional", 0)
				if _, err := js.StartAttempt(ctx, jobID, previous, "fetch", "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := js.MarkCandidate(ctx, previous, "retryable"); err != nil {
					t.Fatal(err)
				}
				if err := js.ResetCandidates(ctx, jobID); err != nil {
					t.Fatal(err)
				}
				current := add("open_access", 1)
				if _, err := js.StartAttempt(ctx, jobID, current, "fetch", "fixture"); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"institutional", "open_access"},
		},
		{
			name: "skipped before I/O is excluded",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				if err := js.MarkCandidate(ctx, add("institutional", 0), "skipped"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "accepted is included without an attempt record",
			setup: func(t *testing.T, js *Store, ctx context.Context, jobID string, add func(string, int) int64) {
				if err := js.MarkCandidate(ctx, add("open_access", 0), "accepted"); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"open_access"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID, err := js.CreateRequest(ctx, "wr_attempted_tiers", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			next := 0
			add := func(accessBasis string, rank int) int64 {
				t.Helper()
				urlKey := jobID + "-" + string(rune('a'+next))
				next++
				if _, err := js.InsertCandidates(ctx, jobID, []Candidate{{
					JobID: jobID, Source: "fixture", URLRedacted: "https://example.test/" + urlKey, URLKey: urlKey,
					Version: "published", AccessBasis: accessBasis, ReuseLicense: "unknown", Rank: rank,
				}}); err != nil {
					t.Fatal(err)
				}
				var candidateID int64
				if err := js.S.DB().QueryRowContext(ctx,
					`SELECT id FROM candidates WHERE job_id = ? AND url_key = ?`, jobID, urlKey).Scan(&candidateID); err != nil {
					t.Fatal(err)
				}
				return candidateID
			}

			tc.setup(t, js, ctx, jobID, add)
			got, err := js.AttemptedTiers(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("AttemptedTiers() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("AttemptedTiers() = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestRetryKeepsThePinnedAccessModeWithoutAConservativeAdvisory is the negative
// half of the advisory-scoped policy clear. Retry releases a job's pinned
// access mode so the conservative advisory's own remedy — widen access_mode and
// retry — can work, but only for a job that actually carried that advisory.
//
// Without this, a regression that cleared the mode on every retry would pass
// the whole suite: the positive test only exercises the advisory-present
// branch, so nothing would notice an ordinary failed-job retry silently
// discarding a deliberate narrowing.
func TestRetryKeepsThePinnedAccessModeWithoutAConservativeAdvisory(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_retry_keeps_mode", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	before, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Policy.AccessMode == "" {
		t.Fatal("fixture policy carries no access mode, so this test cannot detect a clear")
	}
	for _, step := range [][2]string{
		{StateQueued, StateResolving},
		{StateResolving, StateFetching},
		{StateFetching, StateFailed},
	} {
		if err := js.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if err := js.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	after, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Policy.AccessMode != before.Policy.AccessMode {
		t.Fatalf("access mode after an ordinary retry = %q, want it preserved as %q",
			after.Policy.AccessMode, before.Policy.AccessMode)
	}
	// The rest of the policy must survive the retry untouched either way.
	if after.Policy.DesiredVersion != before.Policy.DesiredVersion {
		t.Fatalf("desired_version = %q, want %q", after.Policy.DesiredVersion, before.Policy.DesiredVersion)
	}
}

// TestConcurrentSubmissionsOfOneWorkConvergeOnOneJob pins the guarantee
// ADR-0010 makes to consumers: `existing: true` means a live job already owns
// this work, and two submissions of the same work cannot both create one.
//
// createRequest reads (liveJobForCanonicalWork) and then writes in a DEFERRED
// transaction, so a read-then-write window is at least arguable on paper. It
// does not open in practice: sixteen simultaneous submissions of one work
// produce one job and exactly one existing=false, under -race.
//
// What this test does NOT establish is WHICH mechanism closes the window.
// store.Open sets db.SetMaxOpenConns(1), and database/sql then hands the single
// connection to one transaction at a time — but raising that limit to 8 does
// not make this test fail, so the pool setting is not demonstrably the thing
// doing the work, and SQLite's own write locking may be sufficient. The claim
// asserted here is therefore the observable guarantee ADR-0010 gives consumers,
// not an explanation of it. Do not read this as coverage for the connection
// limit; it is not.
func TestConcurrentSubmissionsOfOneWorkConvergeOnOneJob(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	w := testWork()

	const submissions = 16
	var wg sync.WaitGroup
	results := make([]CreateResult, submissions)
	errs := make([]error, submissions)
	start := make(chan struct{})
	for i := range submissions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = js.CreateRequestForWork(ctx,
				fmt.Sprintf("wr_concurrent_%02d", i), w, "", "", testPolicy(), nil, PrincipalCLI, false)
		}()
	}
	close(start)
	wg.Wait()

	created := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submission %d: %v", i, err)
		}
		created[results[i].JobID]++
	}
	if len(created) != 1 {
		t.Fatalf("%d concurrent submissions of one work produced %d distinct jobs, want 1: %v",
			submissions, len(created), created)
	}
	fresh := 0
	for _, r := range results {
		if !r.Existing {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d submissions reported existing=false, want exactly 1", fresh)
	}
}

// TestForcedSubmissionsDeliberatelyDoNotConverge is the other half. A duplicate
// job for one work is reachable, but only when the caller asks: force skips both
// dedup lookups (job.go:382). Nothing server-side sets it — the flag reaches
// createRequest only from acquire.submit_v2's `force` param — so two live jobs
// for identical identifiers is evidence the submitter forced, not evidence that
// convergence raced.
func TestForcedSubmissionsDeliberatelyDoNotConverge(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	w := testWork()

	first, err := js.CreateRequestForWork(ctx, "wr_force_first", w, "", "", testPolicy(), nil, PrincipalCLI, false)
	if err != nil {
		t.Fatal(err)
	}
	forced, err := js.CreateRequestForWork(ctx, "wr_force_second", w, "", "", testPolicy(), nil, PrincipalCLI, true)
	if err != nil {
		t.Fatal(err)
	}
	if forced.Existing || forced.JobID == first.JobID {
		t.Fatalf("forced submission = %+v, want a distinct fresh job separate from %s", forced, first.JobID)
	}
	unforced, err := js.CreateRequestForWork(ctx, "wr_force_third", w, "", "", testPolicy(), nil, PrincipalCLI, false)
	if err != nil {
		t.Fatal(err)
	}
	if !unforced.Existing {
		t.Fatal("an unforced submission after a forced one created a third job")
	}
}

// TestEnrichmentRecordsADuplicateItCannotHaveCaughtAtSubmit reproduces the real
// 2-in-309 case. Two citations for one paper: one carried a DOI, the other did
// not because the consumer's own identity extraction was defeated. papio
// deduplicates at submit, and a title-only request correctly matches nothing —
// liveJobForCanonicalWork keys on strong identifiers only. Enrichment later
// discovers the DOI, and at that instant the duplication becomes knowable.
//
// The assertion is that papio writes it down and changes nothing else. Both
// jobs stay live and keep their handles: the consumer is polling both, so
// silently merging would cost it a work it believes it is tracking, against a
// duplicate fetch that content addressing already collapses to one file.
func TestEnrichmentRecordsADuplicateItCannotHaveCaughtAtSubmit(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	const doi = "10.3389/fpsyg.2016.00079"

	identified, err := js.CreateRequestForWork(ctx, "wr_dup_with_doi",
		work.Work{DOI: doi, Title: "A Paper With A Clean Citation"}, "", "", testPolicy(), nil, PrincipalCLI, false)
	if err != nil {
		t.Fatal(err)
	}
	// The second citation yields no identifier, so submit has nothing to match.
	titleOnly, err := js.CreateRequestForWork(ctx, "wr_dup_title_only",
		work.Work{Title: "A Paper Whose Citation Defeated Extraction", Authors: []string{"A. Author"}, Year: 2016},
		"", "", testPolicy(), nil, PrincipalCLI, false)
	if err != nil {
		t.Fatal(err)
	}
	if titleOnly.Existing || titleOnly.JobID == identified.JobID {
		t.Fatalf("title-only submission = %+v, want a distinct job: submit cannot know these match", titleOnly)
	}

	// Enrichment supplies the DOI, exactly as crossref_metadata does.
	enriched, err := js.FillWorkMetadata(ctx, titleOnly.JobID, work.Work{DOI: doi})
	if err != nil {
		t.Fatal(err)
	}
	other, err := js.RecordDuplicateWork(ctx, titleOnly.JobID, enriched.Work)
	if err != nil {
		t.Fatal(err)
	}
	if other != identified.JobID {
		t.Fatalf("duplicate_of = %q, want the live job %s that already held the DOI", other, identified.JobID)
	}

	// Recorded, not converged: both handles still resolve to their own live job.
	for _, id := range []string{identified.JobID, titleOnly.JobID} {
		row, err := js.Get(ctx, id)
		if err != nil {
			t.Fatalf("handle %s no longer resolves: %v", id, err)
		}
		if Terminal(row.State) {
			t.Fatalf("job %s is %s; recording a duplicate must not end either job", id, row.State)
		}
	}

	// And the note is idempotent enough to survive a second enrichment pass
	// without inventing a self-reference.
	if self, err := js.RecordDuplicateWork(ctx, identified.JobID, enriched.Work); err != nil {
		t.Fatal(err)
	} else if self == identified.JobID {
		t.Fatal("a job was recorded as a duplicate of itself")
	}
}
