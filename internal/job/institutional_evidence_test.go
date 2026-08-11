// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedInstitutionProfile(t *testing.T, js *Store, id string) {
	t.Helper()
	_, err := js.S.DB().ExecContext(context.Background(), `
		INSERT INTO institution_profiles
		 (id, configured_name, revision, authority_digest, authentication_claim_id,
		  tombstoned_at, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, id+"-configured", id+"-authority", id+"-claim")
	if err != nil {
		t.Fatal(err)
	}
}

func seedCandidate(t *testing.T, js *Store, jobID, profileID, candidateID string) {
	t.Helper()
	_, err := js.S.DB().ExecContext(context.Background(), `
		INSERT INTO browser_candidates
		 (id, job_id, job_attempt_revision, institution_profile_id,
		  institution_profile_revision, route_revision, route_class,
		  identifier_strategy, pre_route_safety_key, safety_domain_id,
		  adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES (?, ?, 1, ?, 1, 1, 'institutional', 'doi', 'pre-route', 'domain',
		        'adapter-1', 'effect-1', 'eligible', ?, ?)`,
		candidateID, jobID, profileID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
}

func seedJobAndCandidate(t *testing.T, js *Store, prefix string) (string, string) {
	t.Helper()
	ctx := context.Background()
	profileID := prefix + "-profile"
	seedInstitutionProfile(t, js, profileID)
	jobID, err := js.CreateRequest(ctx, prefix+"-request", testWork(), "", "", testPolicy(), map[string]string{"doi": testWork().DOI}, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := prefix + "-candidate"
	seedCandidate(t, js, jobID, profileID, candidateID)
	return jobID, candidateID
}

func TestProfileEvidenceLostResponseAndFences(t *testing.T) {
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-evidence")
	ctx := context.Background()
	decisive := ProfileEvidenceObservation{
		ObservationID: "obs-decisive", BrowserHolderGeneration: 7,
		InstitutionProfileID: "profile-evidence", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceWarmVerified, Source: ProfileEvidenceProbe,
		ProducerObservedAt: "2026-01-01T00:00:01Z", DaemonReceivedAt: "2026-01-01T00:00:02Z",
	}
	if err := js.RecordProfileEvidence(ctx, decisive); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordProfileEvidence(ctx, decisive); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordProfileEvidence(ctx, ProfileEvidenceObservation{
		ObservationID: "obs-unknown", BrowserHolderGeneration: 7,
		InstitutionProfileID: "profile-evidence", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceUnknown, Source: ProfileEvidenceProviderOutcome,
		ProducerObservedAt: "2026-01-01T00:00:03Z", DaemonReceivedAt: "2026-01-01T00:00:04Z",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := js.CurrentProfileEvidence(ctx, "profile-evidence", 1, 7)
	if err != nil || !ok {
		t.Fatalf("current evidence = %+v, %v, %v", got, ok, err)
	}
	if got.ObservationID != decisive.ObservationID {
		t.Fatalf("unknown erased decisive evidence: %+v", got)
	}
	if _, ok, err := js.CurrentProfileEvidence(ctx, "profile-evidence", 2, 7); err != nil || ok {
		t.Fatalf("stale profile revision became eligible: ok=%v err=%v", ok, err)
	}
}

func TestHumanGateCurrentScopeProjection(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	base := HumanGateObservation{ID: "gate-1", GateType: HumanGateLogin, ScopeClass: string(HumanGateScopeInstitutionProfile), ScopeKey: "profile-gate", ObservationRevision: 2, Status: HumanGateOpen, DetailJSON: `{}`}
	if err := js.UpsertHumanGateObservation(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.ID, base.ObservationRevision, base.Status = "gate-stale", 1, HumanGateCancelled
	if err := js.UpsertHumanGateObservation(ctx, base); err != nil {
		t.Fatal(err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeInstitutionProfile), "profile-gate")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "gate-1" || rows[0].ObservationRevision != 2 {
		t.Fatalf("scope projection = %+v", rows)
	}
}

func TestRouteSuppressionExactInvalidation(t *testing.T) {
	js := testStore(t)
	jobID, _ := seedJobAndCandidate(t, js, "suppression")
	ctx := context.Background()
	key := RouteSuppressionKey{JobID: jobID, JobAttemptRevision: 1, InstitutionProfileID: "suppression-profile", InstitutionProfileRevision: 1, RouteRevision: 2, SafetyDomainID: "domain-suppress", AdapterRevision: "adapter-1", IdentifierStrategy: "doi"}
	if err := js.AddRouteSuppression(ctx, RouteSuppression{RouteSuppressionKey: key, Reason: RouteSuppressionNoEntitlement}); err != nil {
		t.Fatal(err)
	}
	rows, err := js.ActiveRouteSuppressions(ctx, key)
	if err != nil || len(rows) != 1 {
		t.Fatalf("active suppression = %+v, %v", rows, err)
	}
	key.RouteRevision++
	if err := js.InvalidateRouteSuppressions(ctx, key); err != nil {
		t.Fatal(err)
	}
	old := key
	old.RouteRevision--
	rows, err = js.ActiveRouteSuppressions(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("invalidated suppression remained active: %+v", rows)
	}
}

func TestArtifactWinnerDuplicateAndRollback(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner")
	ctx := context.Background()
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 7,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if _, _, err := js.ClaimArtifactWinner(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: claim.BrowserHolderGeneration, SHA256: sha,
	}); err != nil {
		t.Fatal(err)
	}
	winner, won, err := js.ClaimArtifactWinner(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: claim.BrowserHolderGeneration, SHA256: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if won || winner.CandidateID != candidateID || winner.BrowserHolderGeneration != claim.BrowserHolderGeneration {
		t.Fatalf("loser result = %+v, won=%v", winner, won)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := js.ClaimArtifactWinner(ctx, ArtifactWinner{JobID: "job-rollback", JobAttemptRevision: 1, CandidateID: candidateID, BrowserHolderGeneration: 7, SHA256: sha}); err == nil {
		t.Fatal("cancelled winner unexpectedly committed")
	}
	if _, ok, err := js.ArtifactWinner(context.Background(), "job-rollback", 1); err != nil || ok {
		t.Fatalf("rolled-back winner = ok=%v err=%v", ok, err)
	}
}

func TestArtifactWinnerRejectsStaleHolder(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner-stale")
	ctx := context.Background()
	if _, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 9,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := js.ClaimArtifactWinner(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: 8, SHA256: strings.Repeat("c", 64),
	})
	if !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("stale winner = %v, want stale", err)
	}
	if _, ok, err := js.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
		t.Fatalf("stale holder created winner: ok=%v err=%v", ok, err)
	}
}

func TestArtifactWinnerRejectsRevisedProfileClaim(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner-profile-revision")
	ctx := context.Background()
	if _, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 9,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE institution_profiles SET revision=2 WHERE id=?`, "winner-profile-revision-profile"); err != nil {
		t.Fatal(err)
	}
	_, _, err := js.ClaimArtifactWinner(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: 9, SHA256: strings.Repeat("d", 64),
	})
	if !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("revised profile winner = %v, want stale", err)
	}
	if _, ok, err := js.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
		t.Fatalf("revised profile created winner: ok=%v err=%v", ok, err)
	}
}
