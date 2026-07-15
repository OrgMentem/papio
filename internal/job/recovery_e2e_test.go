// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"papio/internal/artifact"
)

func TestRecoverStaleRewindsDurableBoundariesAndSweepsTerminalQuarantine(t *testing.T) {
	ctx := context.Background()
	jobs := testStore(t)
	artifacts, err := artifact.New(filepath.Dir(jobs.S.Path()))
	if err != nil {
		t.Fatalf("create artifact layout: %v", err)
	}

	newInterrupted := func(requestID, state string) string {
		t.Helper()
		id, err := jobs.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil)
		if err != nil {
			t.Fatalf("create %s: %v", state, err)
		}
		if err := jobs.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
			t.Fatalf("%s queued->resolving: %v", state, err)
		}
		if state == StateFetching || state == StateValidating {
			if err := jobs.Transition(ctx, id, StateResolving, StateFetching, nil); err != nil {
				t.Fatalf("%s resolving->fetching: %v", state, err)
			}
		}
		if state == StateValidating {
			if err := jobs.Transition(ctx, id, StateFetching, StateValidating, nil); err != nil {
				t.Fatalf("validating fetching->validating: %v", err)
			}
		}
		if _, err := jobs.S.DB().ExecContext(ctx,
			"UPDATE jobs SET lease_owner = 'crashed-daemon', lease_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?", id); err != nil {
			t.Fatalf("expire %s lease: %v", state, err)
		}
		return id
	}

	resolvingID := newInterrupted("recover-resolving-0001", StateResolving)
	fetchingID := newInterrupted("recover-fetching-0001", StateFetching)
	validatingID := newInterrupted("recover-validating-0001", StateValidating)

	candidate := Candidate{
		Source: "fixture", URLRedacted: "https://example.test/<redacted>", URLKey: "recover-candidate-key",
		Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Direct: true,
		IdentityConfidence: 1, Rank: 0,
	}
	if inserted, err := jobs.InsertCandidates(ctx, fetchingID, []Candidate{candidate}); err != nil || inserted != 1 {
		t.Fatalf("seed interrupted candidate inserted=%d err=%v, want 1 nil", inserted, err)
	}
	stored, err := jobs.NextPendingCandidate(ctx, fetchingID)
	if err != nil || stored == nil {
		t.Fatalf("load interrupted candidate: %+v, %v", stored, err)
	}
	if err := jobs.MarkCandidate(ctx, stored.ID, "fetching"); err != nil {
		t.Fatalf("mark interrupted candidate fetching: %v", err)
	}
	if _, err := jobs.StartAttempt(ctx, fetchingID, stored.ID, "fetch", "fixture"); err != nil {
		t.Fatalf("record interrupted fetch attempt: %v", err)
	}

	needsReviewID, err := jobs.CreateRequest(ctx, "recover-review-0001", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create needs-review job: %v", err)
	}
	if err := jobs.Transition(ctx, needsReviewID, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("review queued->resolving: %v", err)
	}
	if err := jobs.Transition(ctx, needsReviewID, StateResolving, StateNeedsReview, nil); err != nil {
		t.Fatalf("review resolving->needs_review: %v", err)
	}
	if _, err := jobs.OpenHumanAction(ctx, needsReviewID, "verify_identity", "inspect quarantine file"); err != nil {
		t.Fatalf("open review action: %v", err)
	}

	awaitingID, err := jobs.CreateRequest(ctx, "recover-awaiting-0001", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create awaiting-human job: %v", err)
	}
	if err := jobs.Transition(ctx, awaitingID, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("awaiting queued->resolving: %v", err)
	}
	if err := jobs.Transition(ctx, awaitingID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatalf("awaiting resolving->awaiting_human: %v", err)
	}
	if _, err := jobs.OpenHumanAction(ctx, awaitingID, "manual_download", "wait for user-selected file"); err != nil {
		t.Fatalf("open awaiting action: %v", err)
	}

	terminalID, err := jobs.CreateRequest(ctx, "recover-terminal-0001", testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create terminal job: %v", err)
	}
	if err := jobs.Transition(ctx, terminalID, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("terminal queued->resolving: %v", err)
	}
	if err := jobs.Transition(ctx, terminalID, StateResolving, StateUnavailable, nil, WithTerminalReason("fixture")); err != nil {
		t.Fatalf("terminal resolving->unavailable: %v", err)
	}

	writeQuarantineFile := func(jobID, name string) string {
		t.Helper()
		dir, err := artifacts.QuarantineDir(jobID)
		if err != nil {
			t.Fatalf("quarantine dir for %s: %v", jobID, err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("stale download"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	staleTemp := writeQuarantineFile(terminalID, "interrupted-download.tmp")
	reviewTemp := writeQuarantineFile(needsReviewID, "review-copy.pdf")
	awaitingTemp := writeQuarantineFile(awaitingID, "user-download.pdf")

	recovered, err := jobs.RecoverStale(ctx)
	if err != nil {
		t.Fatalf("recover stale jobs: %v", err)
	}
	sort.Strings(recovered)
	wantRecovered := []string{fetchingID, resolvingID, validatingID}
	sort.Strings(wantRecovered)
	if len(recovered) != len(wantRecovered) {
		t.Fatalf("recovered = %v, want %v", recovered, wantRecovered)
	}
	for i := range wantRecovered {
		if recovered[i] != wantRecovered[i] {
			t.Fatalf("recovered = %v, want %v", recovered, wantRecovered)
		}
	}
	for _, id := range wantRecovered {
		row, err := jobs.Get(ctx, id)
		if err != nil || row.State != StateResolving {
			t.Fatalf("recovered %s = %+v, %v; want resolving", id, row, err)
		}
	}
	for _, id := range []string{needsReviewID, awaitingID} {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatalf("get parked %s: %v", id, err)
		}
		if row.State != map[string]string{needsReviewID: StateNeedsReview, awaitingID: StateAwaitingHuman}[id] {
			t.Fatalf("parked job %s changed state to %s", id, row.State)
		}
	}

	// Recovery itself is metadata-only: it cannot manufacture an extra network
	// attempt, and resubmitting the same live request or candidate is idempotent.
	var attempts int
	if err := jobs.S.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM attempts WHERE job_id = ?", fetchingID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts after recovery: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("recovery attempts = %d, want existing interrupted attempt only", attempts)
	}
	replayed, err := jobs.CreateRequest(ctx, "recover-fetching-0001", testWork(), "", "", testPolicy(), nil)
	if err != nil || replayed != fetchingID {
		t.Fatalf("resubmission after recovery = %q, %v; want live job %q", replayed, err, fetchingID)
	}
	if err := jobs.ResetCandidates(ctx, fetchingID); err != nil {
		t.Fatalf("reset candidates for resume: %v", err)
	}
	if inserted, err := jobs.InsertCandidates(ctx, fetchingID, []Candidate{candidate}); err != nil || inserted != 0 {
		t.Fatalf("resume candidate dedupe inserted=%d err=%v, want 0 nil", inserted, err)
	}

	if err := jobs.SweepTerminalQuarantine(ctx); err != nil {
		t.Fatalf("sweep terminal quarantine: %v", err)
	}
	if _, err := os.Stat(staleTemp); !os.IsNotExist(err) {
		t.Fatalf("terminal stale temp stat = %v, want removed", err)
	}
	for _, path := range []string{reviewTemp, awaitingTemp} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("parked quarantine file %s was removed: %v", path, err)
		}
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatalf("list open human actions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("open human actions = %+v, want review and awaiting actions untouched", actions)
	}
}
