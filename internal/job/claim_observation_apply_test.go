// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"reflect"
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

// A challenge observation is rejected once auth_returned has promoted the
// entry to a human lease, in BOTH intervals that follow: before the entitled
// landing and after it. That is deliberate. The observation's only daemon
// effect is renewing a reserved lease (§2.2.1), and a bound human lease has no
// deadline to renew. The browser reports the same challenge through its own
// challenge_blocked error frame, which parks the handoff, so nothing is lost
// that a journal row would keep: the journal is read only for ordering and for
// owner_closed. What the rejection must guarantee is that it is inert: the
// settled lease is untouched, no journal row is written, and the refused
// ordinal stays free for the observation that follows.
func TestChallengeAfterAuthReturnedIsRejectedAndInert(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		entitled bool
	}{
		{name: "before entitled_landing"},
		{name: "after entitled_landing", entitled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			jobID, candidateID := seedJobAndCandidate(t, js, "late-challenge")
			claimID := "late-challenge-profile-claim"
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, BrowserHolderGeneration: 7, JobAttemptRevision: 1,
				InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
				LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
				AuthenticationClaimID: claimID, LeaseID: "lease-late-challenge", OwnerID: jobID,
				BrowserHolderGeneration: 7, LeaseUntil: time.Now().UTC().Add(time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, claimID, jobID, 7, claim.BindingID, 3); err != nil {
				t.Fatal(err)
			}
			apply := func(observationID, kind string, ordinal int64) ApplyClaimObservationResult {
				t.Helper()
				now := time.Now().UTC()
				result, err := js.ApplyClaimObservation(ctx, ApplyClaimObservationInput{
					JobID: jobID, AuthenticationClaimID: claimID, BindingID: claim.BindingID,
					ObservationID: observationID, GateOccurrenceID: "gate-late-challenge",
					EventKind: kind, EventOrdinal: ordinal,
					AuthReturnedEvidenceObservationID: "evidence-late-challenge",
					FrameGeneration:                   7, Generation: 7,
					LeaseUntil: now.Add(time.Minute), Now: now,
				})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}

			if got := apply("obs-returned", "auth_returned", 0); got.Outcome != "applied" {
				t.Fatalf("auth_returned = %+v, want applied", got)
			}
			if tc.entitled {
				if got := apply("obs-landing", "entitled_landing", 1); got.Outcome != "applied" || !got.EntitledLanding {
					t.Fatalf("entitled_landing = %+v, want applied and entitled", got)
				}
			}
			before, found, err := js.GetAuthenticationEntryLease(ctx, claimID)
			if err != nil || !found || before.State != AuthenticationEntryLeaseHuman || before.HumanOwnerID != jobID {
				t.Fatalf("lease before the challenge = %+v, found=%v, err=%v; want a human lease for the owner", before, found, err)
			}
			if before.LeaseUntil != "" {
				t.Fatalf("bound human lease has deadline %q; the rejection rationale assumes none to renew", before.LeaseUntil)
			}

			nextOrdinal := int64(1)
			if tc.entitled {
				nextOrdinal = 2
			}
			got := apply("obs-challenge", "challenge", nextOrdinal)
			if got.Outcome != "rejected" || got.Detail != "no live reserved entry for this owner" || got.LeaseUntil != "" {
				t.Fatalf("challenge after auth_returned = %+v, want rejected without a lease_until", got)
			}
			after, found, err := js.GetAuthenticationEntryLease(ctx, claimID)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("a rejected challenge disturbed the settled lease:\n before=%+v\n after=%+v\n err=%v", before, after, err)
			}
			var journaled int
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM claim_observation_journal WHERE event_kind='challenge'`).Scan(&journaled); err != nil || journaled != 0 {
				t.Fatalf("challenge journal rows = %d, err=%v; want none for a rejected observation", journaled, err)
			}

			// The refused ordinal was never consumed: the next observation takes
			// it. Before the landing that is the landing itself.
			if !tc.entitled {
				landing := apply("obs-landing", "entitled_landing", nextOrdinal)
				if landing.Outcome != "applied" || !landing.EntitledLanding {
					t.Fatalf("entitled_landing after a rejected challenge = %+v, want applied and entitled", landing)
				}
			}
		})
	}
}
