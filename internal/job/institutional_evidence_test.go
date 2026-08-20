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

func authorizeEffectPermitJob(t *testing.T, js *Store, jobID string) {
	t.Helper()
	ctx := context.Background()
	if err := js.Transition(ctx, jobID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, jobID, "openurl_handoff", "artifact producer test handoff", Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
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

// An auth_returned that lands before any surface is bound must not make the
// entry immortal. Measured live 2026-08-20: one unbound `human` entry with a
// NULL lease_until refused 71 institutional binds for every other paper at the
// operator's library, each logged "another sign-in for this institution is in
// progress" while its owner had no candidate to bind at all.
func TestUnboundHumanConversionKeepsTheBindDeadline(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	seedInstitutionProfile(t, js, "profile-unbound")
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE institution_profiles SET authentication_claim_id='claim-unbound' WHERE id='profile-unbound'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	evidence := ProfileEvidenceObservation{
		ObservationID: "unbound-evidence", BrowserHolderGeneration: 5,
		InstitutionProfileID: "profile-unbound", InstitutionProfileRevision: 1,
		Verdict: ProfileEvidenceAuthReturned, Source: ProfileEvidenceAuthReturn,
		ProducerObservedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		DaemonReceivedAt:   now.Format(time.RFC3339Nano),
	}
	if err := js.RecordProfileEvidence(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-unbound", LeaseID: "lease-unbound", OwnerID: "job-unbound",
		BrowserHolderGeneration: 5, LeaseUntil: now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.ConvertAuthenticationEntryLeaseToHuman(ctx,
		"claim-unbound", "lease-unbound", "job-unbound", 5, evidence); err != nil {
		t.Fatal(err)
	}
	current, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-unbound")
	if err != nil || !ok {
		t.Fatalf("lease = %+v ok=%v err=%v", current, ok, err)
	}
	if current.LeaseUntil == "" {
		t.Fatal("unbound human lease has no deadline: it can never expire, and its institution is held forever")
	}
	until, err := time.Parse(time.RFC3339Nano, current.LeaseUntil)
	if err != nil {
		t.Fatalf("lease_until %q: %v", current.LeaseUntil, err)
	}
	if until.After(now.Add(AuthenticationEntryBindDeadline + 5*time.Second)) {
		t.Fatalf("unbound human lease holds until %s, want no later than the bind deadline %s",
			until, now.Add(AuthenticationEntryBindDeadline))
	}

	// The other half of the rule: a BOUND surface earns the unbounded
	// human-paced window, so a real sign-in is never cut short.
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx,
		"claim-unbound", "job-unbound", 5, "binding-unbound", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='reserved' WHERE authentication_claim_id='claim-unbound'`); err != nil {
		t.Fatal(err)
	}
	if err := js.ConvertAuthenticationEntryLeaseToHuman(ctx,
		"claim-unbound", "lease-unbound", "job-unbound", 5, evidence); err != nil {
		t.Fatal(err)
	}
	bound, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-unbound")
	if err != nil || !ok {
		t.Fatalf("bound lease = %+v ok=%v err=%v", bound, ok, err)
	}
	if bound.LeaseUntil != "" {
		t.Fatalf("bound human lease capped at %s, want the unbounded human window", bound.LeaseUntil)
	}
}

// The sweep heals a row written before the conversion above kept the deadline:
// its lease_until is NULL, so no reservation attempt can ever find it expired.
func TestExpireUnboundAuthenticationEntryLeasesFreesAnImmortalEntry(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	now := time.Now().UTC()
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-immortal", LeaseID: "lease-immortal", OwnerID: "job-immortal",
		BrowserHolderGeneration: 2, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Exactly the live shape: human, unbound, no deadline.
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='human', human_owner_id='job-immortal', lease_until=NULL
		  WHERE authentication_claim_id='claim-immortal'`); err != nil {
		t.Fatal(err)
	}
	// Inside its bind window it is left alone - a bind may still be in flight.
	if freed, err := js.ExpireUnboundAuthenticationEntryLeases(ctx, now); err != nil || freed != 0 {
		t.Fatalf("swept an entry inside its bind window: freed=%d err=%v", freed, err)
	}
	// A bound entry is never swept, however long it holds.
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx,
		"claim-immortal", "job-immortal", 2, "binding-immortal", 1); err != nil {
		t.Fatal(err)
	}
	later := now.Add(AuthenticationEntryBindDeadline + time.Minute)
	if freed, err := js.ExpireUnboundAuthenticationEntryLeases(ctx, later); err != nil || freed != 0 {
		t.Fatalf("swept a bound sign-in: freed=%d err=%v", freed, err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET owner_binding_id=NULL WHERE authentication_claim_id='claim-immortal'`); err != nil {
		t.Fatal(err)
	}
	if freed, err := js.ExpireUnboundAuthenticationEntryLeases(ctx, later); err != nil || freed != 1 {
		t.Fatalf("unbound entry past its deadline: freed=%d err=%v, want 1", freed, err)
	}
	// Freed, so the next paper takes the institution. The fresh reservation is
	// stamped by the store's own clock, so this asserts against that clock
	// rather than the synthetic `later` used above: a just-made reservation is
	// inside its bind window and must survive the sweep.
	next, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-immortal", LeaseID: "lease-next", OwnerID: "job-next",
		BrowserHolderGeneration: 2, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || next.OwnerID != "job-next" {
		t.Fatalf("reserve after sweep = %+v, %v", next, err)
	}
	if freed, err := js.ExpireUnboundAuthenticationEntryLeases(ctx, time.Now().UTC()); err != nil || freed != 0 {
		t.Fatalf("swept a just-made reservation: freed=%d err=%v", freed, err)
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

func TestArtifactWinnerAndExactProducerCommitAtomically(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner-producer-atomic")
	authorizeEffectPermitJob(t, js, jobID)
	ctx := context.Background()
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 1,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := driveIdentity(jobID, "winner-producer-attempt", 0, "generic")
	acquireDrive(t, js, identity, "winner-producer-domain", time.Now().Add(time.Minute))
	ordinal := int64(0)
	producer := ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID, Ordinal: &ordinal,
		Strategy: identity.Strategy, Revision: identity.Revision,
	}
	winner, won, settled, err := js.CommitArtifactWinnerAndProducer(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: claim.BrowserHolderGeneration, SHA256: strings.Repeat("e", 64),
	}, &producer)
	if err != nil || !won || !settled || winner.CandidateID != candidateID {
		t.Fatalf("atomic commit winner=%+v won=%v settled=%v err=%v", winner, won, settled, err)
	}
	closed, err := js.GetEffectPermitByIdentity(ctx, identity)
	if err != nil || closed == nil || closed.Status != Settled {
		t.Fatalf("settled permit=%+v err=%v", closed, err)
	}

	// An invalid producer aborts the whole transaction; no winner leaks.
	otherJob, otherCandidate := seedJobAndCandidate(t, js, "winner-producer-rollback")
	otherClaim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: otherCandidate, BrowserHolderGeneration: 8,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = js.CommitArtifactWinnerAndProducer(ctx, ArtifactWinner{
		JobID: otherJob, JobAttemptRevision: 1, CandidateID: otherCandidate,
		BrowserHolderGeneration: otherClaim.BrowserHolderGeneration, SHA256: strings.Repeat("f", 64),
	}, &ArtifactProducerIdentity{Kind: GenericDrive})
	if err == nil {
		t.Fatal("invalid producer unexpectedly committed")
	}
	if _, ok, winnerErr := js.ArtifactWinner(ctx, otherJob, 1); winnerErr != nil || ok {
		t.Fatalf("winner leaked from rolled-back producer: ok=%v err=%v", ok, winnerErr)
	}
}

func TestLateArtifactWinnerUsesExactProducerHolderGeneration(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner-producer-replaced-holder")
	authorizeEffectPermitJob(t, js, jobID)
	ctx := context.Background()
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 1,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := driveIdentity(jobID, "winner-replaced-holder-attempt", 0, "generic")
	permit := acquireDrive(t, js, identity, "winner-replaced-holder-domain", time.Now().Add(time.Minute))
	ordinal := int64(0)
	winner, won, settled, err := js.CommitArtifactWinnerAndProducer(ctx, ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		// Holder 9 is the bridge current when the late file is adopted. The
		// exact producer belongs to holder 1 and is the authoritative fence.
		BrowserHolderGeneration: 9, SHA256: strings.Repeat("9", 64),
	}, &ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID, Ordinal: &ordinal,
		Strategy: identity.Strategy, Revision: identity.Revision,
	})
	if err != nil || !won || !settled {
		t.Fatalf("late commit winner=%+v won=%v settled=%v err=%v", winner, won, settled, err)
	}
	if winner.BrowserHolderGeneration != permit.BrowserHolderGeneration ||
		winner.BrowserHolderGeneration != claim.BrowserHolderGeneration {
		t.Fatalf("winner holder=%d permit holder=%d claim holder=%d",
			winner.BrowserHolderGeneration, permit.BrowserHolderGeneration, claim.BrowserHolderGeneration)
	}
}

func TestExistingArtifactWinnerRepairsExactProducerSettlement(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := seedJobAndCandidate(t, js, "winner-producer-repair")
	authorizeEffectPermitJob(t, js, jobID)
	ctx := context.Background()
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 9,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	winner := ArtifactWinner{
		JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
		BrowserHolderGeneration: claim.BrowserHolderGeneration, SHA256: sha,
	}
	if _, won, err := js.ClaimArtifactWinner(ctx, winner); err != nil || !won {
		t.Fatalf("seed winner won=%v err=%v", won, err)
	}
	identity := driveIdentity(jobID, "winner-repair-attempt", 0, "generic")
	permit, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
		Identity: identity, JobAttemptRevision: 1,
		BrowserHolderGeneration: claim.BrowserHolderGeneration,
		SafetyDomainID:          "winner-repair-domain",
		LeaseUntil:              time.Now().Add(time.Minute),
		Authorization:           EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired || permit == nil {
		t.Fatalf("acquire outcome=%v permit=%+v err=%v", outcome, permit, err)
	}
	ordinal := int64(0)
	_, won, settled, err := js.CommitArtifactWinnerAndProducer(ctx, winner, &ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID, Ordinal: &ordinal,
		Strategy: identity.Strategy, Revision: identity.Revision,
	})
	if err != nil || !won || !settled {
		t.Fatalf("repair won=%v settled=%v err=%v", won, settled, err)
	}
	closed, err := js.GetEffectPermitByIdentity(ctx, identity)
	if err != nil || closed == nil || closed.Status != Settled {
		t.Fatalf("repaired permit=%+v err=%v", closed, err)
	}
}

func TestLosingArtifactWinnerSettlesExactProducerByKind(t *testing.T) {
	tests := []struct {
		name string
		kind EffectKind
	}{
		{name: "generic", kind: GenericDrive},
		{name: "direct", kind: DirectGet},
		{name: "institutional", kind: Institutional},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID, candidateID := seedJobAndCandidate(t, js, "losing-winner-"+tc.name)
			authorizeEffectPermitJob(t, js, jobID)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, BrowserHolderGeneration: 7,
				JobAttemptRevision: 1, InstitutionProfileRevision: 1,
				RouteRevision: 1, MaterializationKind: "browser_tab",
				LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}

			var producer ArtifactProducerIdentity
			switch tc.kind {
			case GenericDrive, DirectGet:
				strategy := "generic"
				if tc.kind == DirectGet {
					strategy = "direct_get"
				}
				identity := EffectPermitIdentity{
					JobID: jobID, Kind: tc.kind,
					DriveAttemptID: "losing-" + tc.name + "-attempt",
					Ordinal:        0, Strategy: strategy, Revision: "r1",
				}
				_, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
					Identity: identity, JobAttemptRevision: 1,
					BrowserHolderGeneration: 7,
					SafetyDomainID:          "losing-" + tc.name + "-domain",
					LeaseUntil:              time.Now().UTC().Add(time.Minute),
					Authorization:           EffectPermitEvent{Kind: "effect.authorized"},
				})
				if err != nil || outcome != EffectPermitAcquired {
					t.Fatalf("acquire outcome=%v err=%v", outcome, err)
				}
				ordinal := identity.Ordinal
				producer = ArtifactProducerIdentity{
					Kind: tc.kind, DriveAttemptID: identity.DriveAttemptID,
					Ordinal: &ordinal, Strategy: identity.Strategy,
					Revision: identity.Revision,
				}
			case Institutional:
				if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 7, 1, 9); err != nil {
					t.Fatal(err)
				}
				_, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
					JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
					SafetyDomainID:         "domain",
					InstitutionalRequestID: "losing-institutional-request",
					JobAttemptRevision:     1, BrowserHolderGeneration: 7,
					ExpectedEffectOrdinal: 0,
					LeaseUntil:            time.Now().UTC().Add(time.Minute),
					Authorization:         EffectPermitEvent{Kind: "institutional.authorized"},
				})
				if err != nil || outcome != EffectPermitAcquired {
					t.Fatalf("institutional acquire outcome=%v err=%v", outcome, err)
				}
				effectOrdinal := int64(1)
				producer = ArtifactProducerIdentity{
					Kind: Institutional, ClaimID: claim.ID,
					BindingID: claim.BindingID, EffectOrdinal: &effectOrdinal,
					InstitutionalRequestID: "losing-institutional-request",
				}
			}

			winner := ArtifactWinner{
				JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
				BrowserHolderGeneration: claim.BrowserHolderGeneration,
				SHA256:                  strings.Repeat("a", 64),
			}
			if _, won, err := js.ClaimArtifactWinner(ctx, winner); err != nil || !won {
				t.Fatalf("seed winner won=%v err=%v", won, err)
			}
			before, ok, err := js.ArtifactWinner(ctx, jobID, 1)
			if err != nil || !ok {
				t.Fatalf("seeded winner=%+v ok=%v err=%v", before, ok, err)
			}

			unrelatedJob := permitJob(t, js, "unrelated-"+tc.name)
			unrelatedIdentity := driveIdentity(unrelatedJob, "unrelated-"+tc.name+"-attempt", 0, "generic")
			now := store.Now()
			if _, err := js.S.DB().ExecContext(ctx, `
				INSERT INTO legacy_effect_blockers
				  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
				   strategy, revision, cleanup_only, status, created_at, updated_at)
				VALUES (?, 'generic_drive', ?, ?, ?, 0, ?, ?, 1, 'unresolved', ?, ?)`,
				"unrelated-"+tc.name+"-blocker", unrelatedJob,
				"unrelated-"+tc.name+"-domain", unrelatedIdentity.DriveAttemptID,
				unrelatedIdentity.Strategy, unrelatedIdentity.Revision, now, now); err != nil {
				t.Fatal(err)
			}

			loser := winner
			loser.SHA256 = strings.Repeat("b", 64)
			got, won, settled, err := js.CommitArtifactWinnerAndProducer(ctx, loser, &producer)
			if err != nil || won || !settled {
				t.Fatalf("losing commit winner=%+v won=%v settled=%v err=%v", got, won, settled, err)
			}
			if got != before {
				t.Fatalf("returned winner changed: got=%+v before=%+v", got, before)
			}
			after, ok, err := js.ArtifactWinner(ctx, jobID, 1)
			if err != nil || !ok || after != before {
				t.Fatalf("stored winner changed: after=%+v before=%+v ok=%v err=%v", after, before, ok, err)
			}
			closed, err := js.GetEffectPermitByIdentity(ctx, producer.effectIdentity(jobID))
			if err != nil || closed == nil || closed.Status != Settled {
				t.Fatalf("losing producer permit=%+v err=%v", closed, err)
			}
			var unrelatedStatus string
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT status FROM legacy_effect_blockers WHERE id=?`,
				"unrelated-"+tc.name+"-blocker").Scan(&unrelatedStatus); err != nil {
				t.Fatal(err)
			}
			if unrelatedStatus != string(LegacyEffectBlockerUnresolved) {
				t.Fatalf("unrelated occupancy changed status=%q", unrelatedStatus)
			}
		})
	}
}

func TestLosingArtifactWinnerProducerFailuresRollBack(t *testing.T) {
	tests := []struct {
		name  string
		setup func(context.Context, *Store, string) (ArtifactProducerIdentity, func(*testing.T))
	}{
		{
			name: "missing",
			setup: func(_ context.Context, _ *Store, jobID string) (ArtifactProducerIdentity, func(*testing.T)) {
				ordinal := int64(0)
				return ArtifactProducerIdentity{
					Kind: GenericDrive, DriveAttemptID: "missing-producer",
					Ordinal: &ordinal, Strategy: "generic", Revision: "r1",
				}, func(*testing.T) {}
			},
		},
		{
			name: "wrong-job",
			setup: func(ctx context.Context, js *Store, jobID string) (ArtifactProducerIdentity, func(*testing.T)) {
				otherJob := permitJob(t, js, "wrong-job-producer")
				identity := driveIdentity(otherJob, "wrong-job-attempt", 0, "generic")
				permit := acquireDrive(t, js, identity, "wrong-job-domain", time.Now().Add(time.Minute))
				ordinal := int64(0)
				return ArtifactProducerIdentity{
						Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID,
						Ordinal: &ordinal, Strategy: identity.Strategy, Revision: identity.Revision,
					}, func(t *testing.T) {
						got, err := js.GetEffectPermit(ctx, permit.ID)
						if err != nil || got == nil || got.Status != Held {
							t.Fatalf("wrong-job occupancy changed permit=%+v err=%v", got, err)
						}
					}
			},
		},
		{
			name: "permit-and-legacy-ambiguous",
			setup: func(ctx context.Context, js *Store, jobID string) (ArtifactProducerIdentity, func(*testing.T)) {
				identity := driveIdentity(jobID, "ambiguous-losing-attempt", 0, "generic")
				permit := acquireDrive(t, js, identity, "ambiguous-losing-domain", time.Now().Add(time.Minute))
				now := store.Now()
				if _, err := js.S.DB().ExecContext(ctx, `
					INSERT INTO legacy_effect_blockers
					  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
					   strategy, revision, cleanup_only, status, created_at, updated_at)
					VALUES (?, 'generic_drive', ?, ?, ?, 0, ?, ?, 1, 'unresolved', ?, ?)`,
					"ambiguous-losing-blocker", jobID, "ambiguous-losing-domain",
					identity.DriveAttemptID, identity.Strategy, identity.Revision, now, now); err != nil {
					t.Fatal(err)
				}
				ordinal := int64(0)
				return ArtifactProducerIdentity{
						Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID,
						Ordinal: &ordinal, Strategy: identity.Strategy, Revision: identity.Revision,
					}, func(t *testing.T) {
						got, err := js.GetEffectPermit(ctx, permit.ID)
						if err != nil || got == nil || got.Status != Held {
							t.Fatalf("ambiguous permit changed=%+v err=%v", got, err)
						}
						var status string
						if err := js.S.DB().QueryRowContext(ctx,
							`SELECT status FROM legacy_effect_blockers WHERE id=?`,
							"ambiguous-losing-blocker").Scan(&status); err != nil {
							t.Fatal(err)
						}
						if status != string(LegacyEffectBlockerUnresolved) {
							t.Fatalf("ambiguous blocker status=%q", status)
						}
					}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID, candidateID := seedJobAndCandidate(t, js, "losing-failure-"+tc.name)
			authorizeEffectPermitJob(t, js, jobID)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, BrowserHolderGeneration: 11,
				JobAttemptRevision: 1, InstitutionProfileRevision: 1,
				RouteRevision: 1, MaterializationKind: "browser_tab",
				LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			producer, check := tc.setup(ctx, js, jobID)
			_, _, _, err = js.CommitArtifactWinnerAndProducer(ctx, ArtifactWinner{
				JobID: jobID, JobAttemptRevision: 1, CandidateID: candidateID,
				BrowserHolderGeneration: claim.BrowserHolderGeneration,
				SHA256:                  strings.Repeat("c", 64),
			}, &producer)
			if err == nil {
				t.Fatal("invalid producer unexpectedly committed")
			}
			if _, ok, winnerErr := js.ArtifactWinner(ctx, jobID, 1); winnerErr != nil || ok {
				t.Fatalf("failed producer leaked winner: ok=%v err=%v", ok, winnerErr)
			}
			check(t)
		})
	}
}
func TestArtifactProducerForArtifactMixedEvidenceFailsClosedInEitherOrder(t *testing.T) {
	orders := []struct {
		name    string
		reverse bool
	}{
		{name: "correlated then uncorrelated"},
		{name: "uncorrelated then correlated", reverse: true},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID := permitJob(t, js, "artifact-producer-mixed-"+strings.ReplaceAll(tc.name, " ", "-"))
			identity := driveIdentity(jobID, "artifact-producer-mixed-attempt", 0, "generic")
			permit := acquireDrive(t, js, identity, "artifact-producer-mixed-domain", time.Now().Add(time.Minute))
			now := store.Now()
			if _, err := js.S.DB().ExecContext(ctx, `
				INSERT INTO legacy_effect_blockers
				  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
				   strategy, revision, cleanup_only, status, created_at, updated_at)
				VALUES (?, 'generic_drive', ?, ?, ?, 0, ?, ?, 1, 'unresolved', ?, ?)`,
				"artifact-producer-mixed-blocker", jobID, "artifact-producer-mixed-domain",
				identity.DriveAttemptID, identity.Strategy, identity.Revision, now, now); err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("d", 64)
			ordinal := int64(0)
			correlated := map[string]any{
				"filename": "mixed.pdf", "sha256": sha,
				"producer": ArtifactProducerIdentity{
					Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID,
					Ordinal: &ordinal, Strategy: identity.Strategy, Revision: identity.Revision,
				},
			}
			uncorrelated := map[string]any{
				"filename": "mixed.pdf", "sha256": sha,
			}
			events := []map[string]any{correlated, uncorrelated}
			if tc.reverse {
				events[0], events[1] = events[1], events[0]
			}
			for _, detail := range events {
				if err := js.RecordEvent(ctx, jobID, "browser.download_complete", detail); err != nil {
					t.Fatal(err)
				}
			}
			producer, err := js.ArtifactProducerForArtifact(ctx, jobID, "mixed.pdf", sha)
			if !errors.Is(err, ErrArtifactProducerAmbiguous) || producer != nil {
				t.Fatalf("producer=%+v err=%v, want typed ambiguity", producer, err)
			}
			// Model the adoption caller: ambiguity must prevent the exact
			// settlement call, so neither occupancy projection can release.
			gotPermit, err := js.GetEffectPermit(ctx, permit.ID)
			if err != nil || gotPermit == nil || gotPermit.Status != EffectPermitHeld {
				t.Fatalf("mixed evidence changed permit=%+v err=%v", gotPermit, err)
			}
			var blockerStatus string
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT status FROM legacy_effect_blockers WHERE id=?`,
				"artifact-producer-mixed-blocker").Scan(&blockerStatus); err != nil {
				t.Fatal(err)
			}
			if blockerStatus != string(LegacyEffectBlockerUnresolved) {
				t.Fatalf("mixed evidence changed blocker status=%q", blockerStatus)
			}
			if _, ok, err := js.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
				t.Fatalf("mixed evidence created wrong winner: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestArtifactProducerAmbiguousPermitAndLegacyRollsBack(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, _ := seedJobAndCandidate(t, js, "winner-producer-ambiguous")
	authorizeEffectPermitJob(t, js, jobID)
	identity := driveIdentity(jobID, "ambiguous-producer-attempt", 0, "generic")
	acquireDrive(t, js, identity, "ambiguous-producer-domain", time.Now().Add(time.Minute))
	now := store.Now()
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
		   strategy, revision, cleanup_only, status, created_at, updated_at)
		VALUES (?, 'generic_drive', ?, ?, ?, 0, ?, ?, 1, 'unresolved', ?, ?)`,
		"legacy-ambiguous-producer", jobID, "ambiguous-producer-domain",
		identity.DriveAttemptID, identity.Strategy, identity.Revision, now, now); err != nil {
		t.Fatal(err)
	}
	ordinal := int64(0)
	settled, err := js.SettleArtifactProducer(ctx, jobID, ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID,
		Ordinal: &ordinal, Strategy: identity.Strategy, Revision: identity.Revision,
	})
	if !errors.Is(err, ErrEffectPermitStale) || settled {
		t.Fatalf("ambiguous artifact settled=%v err=%v", settled, err)
	}
	permit, err := js.GetEffectPermitByIdentity(ctx, identity)
	if err != nil || permit == nil || permit.Status != Held {
		t.Fatalf("ambiguous permit=%+v err=%v", permit, err)
	}
	var status string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT status FROM legacy_effect_blockers WHERE id=?`,
		"legacy-ambiguous-producer").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(LegacyEffectBlockerUnresolved) {
		t.Fatalf("ambiguous blocker status=%q", status)
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
