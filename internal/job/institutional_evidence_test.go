// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
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
	received := time.Now().UTC().Add(-time.Minute)
	decisive := ProfileEvidenceObservation{
		ObservationID: "obs-decisive", BrowserHolderGeneration: 7,
		InstitutionProfileID: "profile-evidence", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceWarmVerified, Source: ProfileEvidenceProbe,
		ProducerObservedAt: received.Add(-time.Second).Format(time.RFC3339Nano),
		DaemonReceivedAt:   received.Format(time.RFC3339Nano),
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
		ProducerObservedAt: received.Add(time.Second).Format(time.RFC3339Nano),
		DaemonReceivedAt:   received.Add(2 * time.Second).Format(time.RFC3339Nano),
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

func TestProfileEvidenceReceiptTTLAndSignedOutRevokesWarm(t *testing.T) {
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-ttl")
	ctx := context.Background()
	now := time.Now().UTC()
	record := func(id string, verdict ProfileEvidenceVerdict, received time.Time) {
		t.Helper()
		if err := js.RecordProfileEvidence(ctx, ProfileEvidenceObservation{
			ObservationID: id, BrowserHolderGeneration: 3,
			InstitutionProfileID: "profile-ttl", InstitutionProfileRevision: 1,
			Verdict: verdict, Source: ProfileEvidenceProbe,
			ProducerObservedAt: received.Add(-time.Hour).Format(time.RFC3339Nano),
			DaemonReceivedAt:   received.Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx, `
			UPDATE profile_evidence SET daemon_received_at=?, expires_at=? WHERE observation_id=?`,
			received.Format(time.RFC3339Nano), received.Add(ProfileEvidenceTTL).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	record("warm-old", ProfileEvidenceWarmVerified, now.Add(-ProfileEvidenceTTL-time.Minute))
	if _, ok, err := js.CurrentProfileEvidence(ctx, "profile-ttl", 1, 3); err != nil || ok {
		t.Fatalf("producer-old evidence remained current: ok=%v err=%v", ok, err)
	}
	record("warm-current", ProfileEvidenceWarmVerified, now.Add(-time.Minute))
	record("signed-out", ProfileEvidenceSignedOut, now)
	record("warm-delayed", ProfileEvidenceWarmVerified, now.Add(time.Second))
	got, ok, err := js.CurrentProfileEvidence(ctx, "profile-ttl", 1, 3)
	if err != nil || !ok || got.Verdict != ProfileEvidenceSignedOut {
		t.Fatalf("signed-out did not revoke warm: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestProfileEvidenceIndependentProfilesSameSync(t *testing.T) {
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-sync-a")
	seedInstitutionProfile(t, js, "profile-sync-b")
	ctx := context.Background()
	received := time.Now().UTC().Format(time.RFC3339Nano)
	for _, profile := range []string{"profile-sync-a", "profile-sync-b"} {
		if err := js.RecordProfileEvidence(ctx, ProfileEvidenceObservation{
			ObservationID: profile + "-observation", BrowserHolderGeneration: 8,
			InstitutionProfileID: profile, InstitutionProfileRevision: 1,
			Verdict: ProfileEvidenceWarmVerified, Source: ProfileEvidenceProbe,
			ProducerObservedAt: received, DaemonReceivedAt: received,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, profile := range []string{"profile-sync-a", "profile-sync-b"} {
		if got, ok, err := js.CurrentProfileEvidence(ctx, profile, 1, 8); err != nil || !ok || got.InstitutionProfileID != profile {
			t.Fatalf("profile %s evidence = %+v ok=%v err=%v", profile, got, ok, err)
		}
	}
}

func TestAuthenticationEntryLeaseReserveExpiryConversionAndRestart(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-lease")
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET authentication_claim_id='claim-shared' WHERE id='profile-lease'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	evidence := ProfileEvidenceObservation{
		ObservationID: "lease-evidence", BrowserHolderGeneration: 12,
		InstitutionProfileID: "profile-lease", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceAuthReturned, Source: ProfileEvidenceAuthReturn,
		ProducerObservedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		DaemonReceivedAt:   now.Format(time.RFC3339Nano),
	}
	if err := js.RecordProfileEvidence(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-shared", LeaseID: "lease-1", OwnerID: "job-a",
		BrowserHolderGeneration: 12, LeaseUntil: now.Add(time.Minute),
	})
	if err != nil || lease.State != AuthenticationEntryLeaseReserved {
		t.Fatalf("reserve = %+v err=%v", lease, err)
	}
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-shared", LeaseID: "lease-2", OwnerID: "job-b",
		BrowserHolderGeneration: 12, LeaseUntil: now.Add(time.Minute),
	}); !errors.Is(err, ErrAuthenticationEntryLeaseBusy) {
		t.Fatalf("second owner reserve err=%v, want busy", err)
	}
	if err := js.ConvertAuthenticationEntryLeaseToHuman(ctx, "claim-shared", "lease-1", "job-a", 12, evidence); err != nil {
		t.Fatalf("convert to human: %v", err)
	}
	current, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-shared")
	if err != nil || !ok || current.State != AuthenticationEntryLeaseHuman || current.HumanOwnerID != "job-a" {
		t.Fatalf("human lease = %+v ok=%v err=%v", current, ok, err)
	}
	if err := js.ExpireAuthenticationEntryLease(ctx, "claim-shared", 11, "lease-1"); !errors.Is(err, ErrAuthenticationEntryLeaseStale) {
		t.Fatalf("stale expiry err=%v", err)
	}
	dataDir := filepath.Dir(js.S.Path())
	if err := js.S.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, ok, err := (&Store{S: restarted}).GetAuthenticationEntryLease(ctx, "claim-shared")
	if err != nil || !ok || persisted.State != AuthenticationEntryLeaseHuman {
		t.Fatalf("restarted lease = %+v ok=%v err=%v", persisted, ok, err)
	}
}

func TestAuthenticationEntryLeaseExpiryAllowsNewOwner(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-expire", LeaseID: "lease-old", OwnerID: "old",
		BrowserHolderGeneration: 4, LeaseUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-expire", LeaseID: "lease-new", OwnerID: "new",
		BrowserHolderGeneration: 5, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || lease.OwnerID != "new" || lease.State != AuthenticationEntryLeaseReserved {
		t.Fatalf("expired lease replacement = %+v err=%v", lease, err)
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

func TestAuthenticationEntryLeaseSignedOutDominatesLaterWarm(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-lease-precedence")
	if _, err := js.S.DB().ExecContext(ctx, `
		UPDATE institution_profiles SET authentication_claim_id='claim-precedence'
		WHERE id='profile-lease-precedence'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	authReturned := ProfileEvidenceObservation{
		ObservationID: "precedence-auth-return", BrowserHolderGeneration: 17,
		InstitutionProfileID: "profile-lease-precedence", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceAuthReturned, Source: ProfileEvidenceAuthReturn,
		ProducerObservedAt: now.Format(time.RFC3339Nano),
	}
	if err := js.RecordProfileEvidence(ctx, authReturned); err != nil {
		t.Fatal(err)
	}
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-precedence", LeaseID: "lease-owner-a", OwnerID: "job-a",
		BrowserHolderGeneration: 17, LeaseUntil: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	currentEvidence, ok, err := js.CurrentProfileEvidence(ctx, "profile-lease-precedence", 1, 17)
	if err != nil || !ok {
		t.Fatalf("current auth-return evidence = %+v, ok=%v err=%v", currentEvidence, ok, err)
	}
	if err := js.ConvertAuthenticationEntryLeaseToHuman(ctx, "claim-precedence", lease.LeaseID, "job-a", 17, currentEvidence); err != nil {
		t.Fatal(err)
	}
	for _, observation := range []ProfileEvidenceObservation{
		{
			ObservationID: "precedence-signed-out", BrowserHolderGeneration: 17,
			InstitutionProfileID: "profile-lease-precedence", InstitutionProfileRevision: 1,
			Verdict: ProfileEvidenceSignedOut, Source: ProfileEvidenceProviderOutcome,
			ProducerObservedAt: now.Add(time.Second).Format(time.RFC3339Nano),
		},
		{
			ObservationID: "precedence-warm-late", BrowserHolderGeneration: 17,
			InstitutionProfileID: "profile-lease-precedence", InstitutionProfileRevision: 1,
			Verdict: ProfileEvidenceWarmVerified, Source: ProfileEvidenceProbe,
			ProducerObservedAt: now.Add(2 * time.Second).Format(time.RFC3339Nano),
		},
	} {
		if err := js.RecordProfileEvidence(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	replacement, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-precedence", LeaseID: "lease-owner-b", OwnerID: "job-b",
		BrowserHolderGeneration: 17, LeaseUntil: now.Add(time.Minute),
	})
	if err != nil || replacement.OwnerID != "job-b" || replacement.State != AuthenticationEntryLeaseReserved {
		t.Fatalf("replacement lease = %+v, err=%v", replacement, err)
	}
}

func TestProfileEvidenceRejectsSupersededRevisionAndTombstone(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-revision-fence")
	observation := func(id string, revision int64) ProfileEvidenceObservation {
		return ProfileEvidenceObservation{
			ObservationID: id, BrowserHolderGeneration: 4,
			InstitutionProfileID: "profile-revision-fence", InstitutionProfileRevision: revision,
			Verdict: ProfileEvidenceWarmVerified, Source: ProfileEvidenceProbe,
			ProducerObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
	if err := js.RecordProfileEvidence(ctx, observation("live-revision", 1)); err != nil {
		t.Fatalf("live revision evidence: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE institution_profiles SET revision=2 WHERE id='profile-revision-fence'`); err != nil {
		t.Fatal(err)
	}
	// A frame produced under revision 1 and delivered after the edit must not
	// become revision-1 evidence again, and must never be promoted to 2.
	if err := js.RecordProfileEvidence(ctx, observation("delayed-old-revision", 1)); !errors.Is(err, ErrProfileEvidenceStale) {
		t.Fatalf("superseded revision evidence = %v, want stale", err)
	}
	if err := js.RecordProfileEvidence(ctx, observation("current-revision", 2)); err != nil {
		t.Fatalf("current revision evidence: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE institution_profiles SET tombstoned_at='2026-01-02T00:00:00Z' WHERE id='profile-revision-fence'`); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordProfileEvidence(ctx, observation("after-tombstone", 2)); !errors.Is(err, ErrProfileEvidenceStale) {
		t.Fatalf("tombstoned profile evidence = %v, want stale", err)
	}
}
