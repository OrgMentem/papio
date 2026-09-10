// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func publicationInput(id, jobID string, candidateID *int64, role PublicationRole, sha string) PublicationInput {
	return PublicationInput{
		ID: id, JobID: jobID, CandidateID: candidateID, Role: role, SHA256: sha,
		QuarantinePath: filepath.Join("/quarantine", id+".tmp"),
		Artifact: Artifact{
			SHA256: sha, SizeBytes: 7, MIME: "application/pdf", PageCount: 1,
			Path: filepath.Join("/artifacts", sha+".pdf"), IdentityResult: "pass",
		},
	}
}

func validatingPublicationJob(t *testing.T, js *Store, requestID string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateResolving, StateFetching, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateFetching, StateValidating, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.InsertCandidates(ctx, jobID, []Candidate{{
		JobID: jobID, Source: "test", URLRedacted: "https://example.test/pdf", URLKey: requestID,
		Version: "unknown", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := js.NextPendingCandidate(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil {
		t.Fatal("missing publication candidate")
	}
	return jobID, candidate.ID
}

func TestFinalizePublicationMainCommitsOneAcceptance(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_main")
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	input := publicationInput("publication_main", jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	input.TransitionDetail = map[string]any{"source": "test"}
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	})
	if err != nil || !result.Created {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
	prepared, err := js.PreparedPublications(ctx, jobID)
	if err != nil || len(prepared) != 0 {
		t.Fatalf("prepared = %+v, %v; want none", prepared, err)
	}
	edge, err := js.HasPublicationEdge(ctx, jobID, sha, PublicationRoleMain)
	if err != nil || !edge {
		t.Fatalf("main edge = %t, %v", edge, err)
	}
	candidate, err := js.GetCandidate(ctx, candidateID)
	if err != nil || candidate.Status != "accepted" {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	row, err := js.Get(ctx, jobID)
	if err != nil || row.State != StateReady || row.ArtifactSHA256 != sha || row.SelectedCandidateID != candidateID {
		t.Fatalf("job = %+v, %v", row, err)
	}
}

// TestConsumePublishedPublicationClearsRedundantJournalRow pins the recovery
// escape from a wedged job: a journal row whose acquisition edge is already
// committed cannot publish anything, but the scheduler reads ANY prepared
// publication as unfinished work and refuses to process the job, so recovery
// has to consume the row instead of skipping it. Artifact metadata must
// survive, because the committed edge still owns the digest.
func TestConsumePublishedPublicationClearsRedundantJournalRow(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_consume")
	sha := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	input := publicationInput("publication_consume", jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	}); err != nil {
		t.Fatal(err)
	}

	// A second journal row for the same job, digest and role: the shape a
	// duplicate adoption leaves behind once the edge exists.
	redundant := publicationInput("publication_consume_again", jobID, nil, PublicationRoleMain, sha)
	if err := js.PreparePublication(ctx, redundant); err != nil {
		t.Fatal(err)
	}
	consumed, err := js.ConsumePublishedPublication(ctx, redundant.ID)
	if err != nil || !consumed {
		t.Fatalf("consume = %t, %v; want true, nil", consumed, err)
	}
	prepared, err := js.PreparedPublications(ctx, jobID)
	if err != nil || len(prepared) != 0 {
		t.Fatalf("prepared = %+v, %v; want none, so the scheduler can process the job", prepared, err)
	}
	edge, err := js.HasPublicationEdge(ctx, jobID, sha, PublicationRoleMain)
	if err != nil || !edge {
		t.Fatalf("edge = %t, %v; want the committed acquisition retained", edge, err)
	}
	// Unlike DiscardPublication, consumption must leave the artifact row the
	// committed edge still points at.
	var artifacts int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE sha256 = ?`, sha).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 1 {
		t.Fatalf("artifact rows for %s = %d, want 1 retained", sha, artifacts)
	}

	// Without an edge the row is still owed real recovery, so consumption
	// must refuse rather than discard evidence.
	other, otherCandidate := validatingPublicationJob(t, js, "wr_publication_consume_noedge")
	unpublished := publicationInput("publication_consume_noedge", other, &otherCandidate, PublicationRoleMain,
		"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	if err := js.PreparePublication(ctx, unpublished); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ConsumePublishedPublication(ctx, unpublished.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("consume without edge = %v, want ErrConflict", err)
	}
	if prepared, err := js.PreparedPublications(ctx, other); err != nil || len(prepared) != 1 {
		t.Fatalf("unpublished journal = %+v, %v; want retained", prepared, err)
	}
}

func TestFinalizePublicationComponentDoesNotTransitionJob(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, _ := validatingPublicationJob(t, js, "wr_publication_component")
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	input := publicationInput("publication_component", jobID, nil, PublicationRoleSupplement, sha)
	input.Artifact.IdentityResult = ""
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	row, err := js.Get(ctx, jobID)
	if err != nil || row.State != StateValidating || row.ArtifactSHA256 != "" {
		t.Fatalf("job = %+v, %v", row, err)
	}
	edge, err := js.HasPublicationEdge(ctx, jobID, sha, PublicationRoleSupplement)
	if err != nil || !edge {
		t.Fatalf("component edge = %t, %v", edge, err)
	}
}

func TestFinalizePublicationRejectsLostLeaseBeforePromotion(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_lease")
	owner := "publication-owner"
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ?`, owner,
		time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
	sha := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	input := publicationInput("publication_lease", jobID, &candidateID, PublicationRoleMain, sha)
	input.LeaseOwner = &owner
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET lease_owner = 'replacement' WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		called = true
		return PromotionResult{}, nil
	})
	if !errors.Is(err, ErrConflict) || called {
		t.Fatalf("finalize = %v, callback called=%t; want lost lease before callback", err, called)
	}
	prepared, err := js.PreparedPublications(ctx, jobID)
	if err != nil || len(prepared) != 1 {
		t.Fatalf("prepared = %+v, %v; want retained owner", prepared, err)
	}
}

func TestPublicationAmbiguousCommitRecoversOnFreshStore(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	s, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	js := &Store{S: s}
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_ambiguous")
	sha := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	input := publicationInput("publication_ambiguous", jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	publicationAfterCommitForTest = func() error { return errors.New("commit reply lost") }
	t.Cleanup(func() { publicationAfterCommitForTest = nil })
	_, err = js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	})
	if err == nil {
		t.Fatal("finalize succeeded after simulated ambiguous commit")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	restarted := &Store{S: fresh}
	edge, err := restarted.HasPublicationEdge(ctx, jobID, sha, PublicationRoleMain)
	if err != nil || !edge {
		t.Fatalf("fresh edge = %t, %v", edge, err)
	}
	prepared, err := restarted.PreparedPublications(ctx, jobID)
	if err != nil || len(prepared) != 0 {
		t.Fatalf("fresh prepared = %+v, %v; want none", prepared, err)
	}
}

func TestDiscardPublicationPreservesSharedDigest(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	firstID, firstCandidate := validatingPublicationJob(t, js, "wr_publication_shared_first")
	secondID, _ := validatingPublicationJob(t, js, "wr_publication_shared_second")
	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	first := publicationInput("publication_shared_first", firstID, &firstCandidate, PublicationRoleMain, sha)
	first.FromState, first.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := js.FinalizePublication(ctx, first.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: first.Artifact.Path, Created: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	second := publicationInput("publication_shared_second", secondID, nil, PublicationRoleSupplement, sha)
	if err := js.PreparePublication(ctx, second); err != nil {
		t.Fatal(err)
	}
	deleted, err := js.DiscardPublication(ctx, second.ID)
	if !errors.Is(err, ErrConflict) || deleted {
		t.Fatalf("discard shared digest = deleted=%t, err=%v; want retained shared content", deleted, err)
	}
	artifact, err := js.GetArtifact(ctx, sha)
	if err != nil || artifact == nil {
		t.Fatalf("shared artifact = %+v, %v", artifact, err)
	}
}

func TestPreparedPublicationSurvivesRestartAndFinalCommitFailure(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	s, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	js := &Store{S: s}
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_restart")
	sha := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	input := publicationInput("publication_restart", jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	js = &Store{S: s}
	if prepared, err := js.PreparedPublications(ctx, jobID); err != nil || len(prepared) != 1 {
		t.Fatalf("restart prepared = %+v, %v; want one durable owner", prepared, err)
	}
	publicationBeforeCommitForTest = func() error { return errors.New("final commit interrupted") }
	t.Cleanup(func() { publicationBeforeCommitForTest = nil })
	promoted := 0
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		promoted++
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	}); err == nil {
		t.Fatal("finalize succeeded despite interrupted final commit")
	}
	if promoted != 1 {
		t.Fatalf("promotion calls = %d, want 1", promoted)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	js = &Store{S: s}
	if prepared, err := js.PreparedPublications(ctx, jobID); err != nil || len(prepared) != 1 {
		t.Fatalf("post-failure restart prepared = %+v, %v; want one owner", prepared, err)
	}
	publicationBeforeCommitForTest = nil
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		promoted++
		return PromotionResult{Path: input.Artifact.Path, Created: false}, nil
	}); err != nil {
		t.Fatalf("recovery finalization: %v", err)
	}
	if promoted != 2 {
		t.Fatalf("recovery promotion calls = %d, want 2", promoted)
	}
	edge, err := js.HasPublicationEdge(ctx, jobID, sha, PublicationRoleMain)
	if err != nil || !edge {
		t.Fatalf("recovered edge = %t, %v", edge, err)
	}
}

func TestPausedPublicationFenceBlocksRecovery(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	s, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	js := &Store{S: s}
	jobID, _ := validatingPublicationJob(t, js, "wr_publication_paused")
	sha := "1111111111111111111111111111111111111111111111111111111111111111"
	input := publicationInput("publication_paused", jobID, nil, PublicationRoleSupplement, sha)
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	finalized := make(chan error, 1)
	go func() {
		_, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
			close(entered)
			<-release
			return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
		})
		finalized <- err
	}()
	<-entered
	staleDone := make(chan error, 1)
	go func() {
		_, err := (&Store{S: recovery}).DiscardPublication(ctx, input.ID)
		staleDone <- err
	}()
	select {
	case err := <-staleDone:
		t.Fatalf("stale recovery completed during promotion fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-finalized; err != nil {
		t.Fatalf("finalize paused publication: %v", err)
	}
	if err := <-staleDone; !errors.Is(err, ErrConflict) {
		t.Fatalf("stale recovery after finalization = %v, want committed-edge conflict", err)
	}
}

func TestPreparePublicationFencesCandidateToItsJob(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	_, candidateID := validatingPublicationJob(t, js, "wr_publication_candidate_first")
	secondID, _ := validatingPublicationJob(t, js, "wr_publication_candidate_second")
	sha := "2222222222222222222222222222222222222222222222222222222222222222"
	input := publicationInput("publication_wrong_candidate", secondID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err == nil {
		t.Fatal("prepare accepted a candidate from another job")
	}
	if prepared, err := js.PreparedPublications(ctx, secondID); err != nil || len(prepared) != 0 {
		t.Fatalf("foreign candidate created publication = %+v, %v", prepared, err)
	}
}

func TestReclaimPublicationLeaseTransfersExpiredJournal(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "wr_publication_reclaim", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	owner := "crashed-owner"
	claimed, err := js.LeaseAwaitingHuman(ctx, jobID, owner, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("lease awaiting human = %t, %v", claimed, err)
	}
	sha := "3333333333333333333333333333333333333333333333333333333333333333"
	input := publicationInput("publication_reclaim", jobID, nil, PublicationRoleSupplement, sha)
	input.LeaseOwner = &owner
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
	prepared, err := js.ReclaimPublicationLease(ctx, input.ID, "recovery-owner", time.Minute)
	if err != nil || prepared.LeaseOwner == nil || *prepared.LeaseOwner != "recovery-owner" {
		t.Fatalf("reclaim = %+v, %v", prepared, err)
	}
	row, err := js.Get(ctx, jobID)
	if err != nil || row.LeaseOwner != "recovery-owner" || !row.LeaseActive(time.Now()) {
		t.Fatalf("reclaimed job = %+v, %v", row, err)
	}
}

func TestIsStandaloneJob(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "wr_publication_standalone", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := js.IsStandaloneJob(ctx, jobID)
	if err != nil || !standalone {
		t.Fatalf("standalone = %t, %v", standalone, err)
	}
	now := store.Now()
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO acquisition_batches(id, cohort_id, source_kind, source_label, expected_total, created_at, updated_at, membership_state)
		VALUES ('batch-publication', 'cohort-publication', 'test', 'test', 1, ?, ?, 'open')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO acquisition_batch_members(batch_id, ordinal, canonical_key, job_id, submission_outcome, created_at)
		VALUES ('batch-publication', 0, 'test-key', ?, 'created', ?)`, jobID, now); err != nil {
		t.Fatal(err)
	}
	standalone, err = js.IsStandaloneJob(ctx, jobID)
	if err != nil || standalone {
		t.Fatalf("batch member standalone = %t, %v", standalone, err)
	}
}

func TestSweepTerminalQuarantineRetainsPreparedPublication(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "wr_publication_sweep", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateResolving, StateReady, nil); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(filepath.Dir(js.S.Path()))
	if err != nil {
		t.Fatal(err)
	}
	qdir, err := artifacts.QuarantineDir(jobID)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(qdir, "component.tmp")
	if err := os.WriteFile(temp, []byte("component"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := "4444444444444444444444444444444444444444444444444444444444444444"
	input := publicationInput("publication_sweep", jobID, nil, PublicationRoleSupplement, sha)
	input.QuarantinePath = temp
	input.Artifact.IdentityResult = ""
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := js.SweepTerminalQuarantine(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatalf("sweep removed prepared component quarantine: %v", err)
	}
}

func TestRebindPublicationLeaseRequiresActiveExactOwner(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_rebind")
	oldOwner := "old-owner"
	if _, err := js.Claim(ctx, jobID, oldOwner, time.Minute); err != nil {
		t.Fatal(err)
	}
	sha := "5555555555555555555555555555555555555555555555555555555555555555"
	input := publicationInput("publication_rebind", jobID, &candidateID, PublicationRoleMain, sha)
	input.LeaseOwner = &oldOwner
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	activeOwner := "active-owner"
	if _, err := js.S.DB().ExecContext(ctx, `
		UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ?`,
		activeOwner, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
	prepared, err := js.RebindPublicationLease(ctx, input.ID, activeOwner)
	if err != nil || prepared.LeaseOwner == nil || *prepared.LeaseOwner != activeOwner {
		t.Fatalf("rebind = %+v, %v", prepared, err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET lease_owner = 'other-owner' WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RebindPublicationLease(ctx, input.ID, activeOwner); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebind after ownership change = %v, want conflict", err)
	}
}

func TestSweepOrphanComponentStagesRetainsPreparedStage(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "wr_component_stage_sweep", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(filepath.Dir(js.S.Path()))
	if err != nil {
		t.Fatal(err)
	}
	abandonedDir, err := artifacts.QuarantineDir("component-stage_abandoned")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandonedDir, "orphan.tmp"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownedDir, err := artifacts.QuarantineDir("component-stage_owned")
	if err != nil {
		t.Fatal(err)
	}
	ownedTemp := filepath.Join(ownedDir, "owned.tmp")
	if err := os.WriteFile(ownedTemp, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := "6666666666666666666666666666666666666666666666666666666666666666"
	input := publicationInput("publication_component_stage", jobID, nil, PublicationRoleSupplement, sha)
	input.QuarantinePath = ownedTemp
	input.Artifact.IdentityResult = ""
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := js.SweepOrphanComponentStages(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandonedDir); !os.IsNotExist(err) {
		t.Fatalf("orphan component stage stat = %v, want removed", err)
	}
	if _, err := os.Stat(ownedTemp); err != nil {
		t.Fatalf("prepared component stage removed: %v", err)
	}
}

func TestFinalizePublicationAfterRestartPreservesCancellation(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	s, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	js := &Store{S: s}
	jobID, candidateID := validatingPublicationJob(t, js, "wr_publication_cancelled")
	sha := "7777777777777777777777777777777777777777777777777777777777777777"
	input := publicationInput("publication_cancelled", jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, jobID, "test cancellation"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	js = &Store{S: s}
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: false}, nil
	}); err != nil {
		t.Fatalf("terminal recovery finalize: %v", err)
	}
	row, err := js.Get(ctx, jobID)
	if err != nil || row.State != StateCancelled {
		t.Fatalf("terminal recovery changed cancellation: %+v, %v", row, err)
	}
	edge, err := js.HasPublicationEdge(ctx, jobID, sha, PublicationRoleMain)
	if err != nil || !edge {
		t.Fatalf("terminal recovery edge = %t, %v", edge, err)
	}
	if prepared, err := js.PreparedPublications(ctx, jobID); err != nil || len(prepared) != 0 {
		t.Fatalf("terminal recovery journal = %+v, %v; want none", prepared, err)
	}
}
