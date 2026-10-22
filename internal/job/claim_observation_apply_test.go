// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"testing"
	"time"
)

// ApplyClaimObservation's doc promise: the side effects and the journal row
// commit or roll back together. A journal-write failure after the side effects
// ran used to be reported as an "error" outcome while the transaction still
// committed, leaving a half-applied observation (claim abandoned, entitlement
// recorded) with no journal row to make the retry idempotent.
func TestApplyClaimObservationJournalWriteFailureRollsBackSideEffects(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T, prefix string) (*Store, string, *MaterializationClaim, string) {
		t.Helper()
		js := testStore(t)
		jobID, candidateID := seedJobAndCandidate(t, js, prefix)
		claimID := prefix + "-profile-claim"
		claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidateID, BrowserHolderGeneration: 7, JobAttemptRevision: 1,
			InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
			LeaseUntil: time.Now().UTC().Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE materialization_claims SET phase='navigated' WHERE id=?`, claim.ID); err != nil {
			t.Fatal(err)
		}
		return js, jobID, claim, claimID
	}
	failJournalWrites := func(t *testing.T, js *Store) {
		t.Helper()
		if _, err := js.S.DB().ExecContext(ctx, `
			CREATE TRIGGER fail_claim_observation_journal
			BEFORE INSERT ON claim_observation_journal
			BEGIN SELECT RAISE(ABORT, 'journal write refused'); END`); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(t *testing.T, js *Store, jobID, claimID, bindingID, kind string) ApplyClaimObservationResult {
		t.Helper()
		now := time.Now().UTC()
		applied, err := js.ApplyClaimObservation(ctx, ApplyClaimObservationInput{
			JobID: jobID, AuthenticationClaimID: claimID, BindingID: bindingID,
			ObservationID: "obs-" + kind, GateOccurrenceID: "gate-" + kind,
			EventKind: kind, EventOrdinal: 1,
			FrameGeneration: 7, Generation: 7,
			LeaseUntil: now.Add(time.Minute), Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}

	t.Run("owner_closed keeps the claim live", func(t *testing.T) {
		js, jobID, claim, claimID := seed(t, "rollback-closed")
		failJournalWrites(t, js)
		applied := apply(t, js, jobID, claimID, claim.BindingID, "owner_closed")
		if applied.Outcome != "error" {
			t.Fatalf("owner_closed with a failing journal = %+v; want outcome error", applied)
		}
		var phase string
		if err := js.S.DB().QueryRowContext(ctx,
			`SELECT phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&phase); err != nil {
			t.Fatal(err)
		}
		if phase != "navigated" {
			t.Fatalf("claim phase = %q after a rolled-back owner_closed; want navigated", phase)
		}
		var journaled int
		if err := js.S.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM claim_observation_journal WHERE observation_id='obs-owner_closed'`).Scan(&journaled); err != nil || journaled != 0 {
			t.Fatalf("journal rows = %d, err=%v; want 0", journaled, err)
		}
		// The retry must still see the live claim, so the loss is reported.
		if _, err := js.S.DB().ExecContext(ctx, `DROP TRIGGER fail_claim_observation_journal`); err != nil {
			t.Fatal(err)
		}
		retried := apply(t, js, jobID, claimID, claim.BindingID, "owner_closed")
		if retried.Outcome != "applied" || !retried.SurfaceLost {
			t.Fatalf("retry = %+v; want applied with SurfaceLost", retried)
		}
	})

	t.Run("entitled_landing leaves the lease unentitled", func(t *testing.T) {
		js, jobID, claim, claimID := seed(t, "rollback-entitled")
		now := time.Now().UTC()
		if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
			AuthenticationClaimID: claimID, LeaseID: "lease-rollback-entitled", OwnerID: jobID,
			BrowserHolderGeneration: 7, LeaseUntil: now.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		evidence := ProfileEvidenceObservation{
			ObservationID: "evidence-rollback-entitled", BrowserHolderGeneration: 7,
			InstitutionProfileID: "rollback-entitled-profile", InstitutionProfileRevision: 1,
			Verdict: ProfileEvidenceAuthReturned, Source: ProfileEvidenceAuthReturn,
			ProducerObservedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
			DaemonReceivedAt:   now.Format(time.RFC3339Nano),
		}
		if err := js.RecordProfileEvidence(ctx, evidence); err != nil {
			t.Fatal(err)
		}
		if err := js.ConvertAuthenticationEntryLeaseToHuman(ctx, claimID, "lease-rollback-entitled", jobID, 7, evidence); err != nil {
			t.Fatal(err)
		}
		failJournalWrites(t, js)
		applied := apply(t, js, jobID, claimID, claim.BindingID, "entitled_landing")
		if applied.Outcome != "error" || applied.EntitledLanding {
			t.Fatalf("entitled_landing with a failing journal = %+v; want outcome error and no entitlement", applied)
		}
		lease, ok, err := js.GetAuthenticationEntryLease(ctx, claimID)
		if err != nil || !ok {
			t.Fatalf("lease = ok %v, %v", ok, err)
		}
		if lease.EntitledAt != "" {
			t.Fatalf("lease entitled_at = %q after a rolled-back entitled_landing; want empty", lease.EntitledAt)
		}
	})
}
