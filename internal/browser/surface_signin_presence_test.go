// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"papio/internal/protocol"
)

// bindSignInSurface drives one requires_auth handoff to a bound sign-in
// surface: an authentication claim grant, a bind, and the claim moved to
// `navigated`. It returns the binding id and the authentication claim id.
func bindSignInSurface(t *testing.T, b *Bridge, prefix string) (jobID, candidateID, bindingID, gateOccurrence string) {
	t.Helper()
	jobs := b.jobs
	jobID = parkInstitutional(t, jobs, "wr_"+prefix, handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-"+prefix)
	candidateID = explicitMaterializationCandidate(t, jobs, jobID, "domain-"+prefix)
	// The real path: a poll offers the candidate, which is what puts it in
	// materializationOffered, and only then does the extension claim and bind.
	// The first poll above necessarily fell back to a legacy offer for this
	// job (no profile or candidate existed yet); retire it as its cancel or
	// timeout would.
	b.mu.Lock()
	delete(b.offered, jobID)
	b.mu.Unlock()
	offered, _ := runSync(t, b)
	if offer := firstOfType(offered, protocol.MsgInstitutionalCandidateOffer); offer == nil || offer.JobID != jobID {
		t.Fatalf("poll did not offer the sign-in candidate: %v", offered)
	}
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: prefix + "-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	if grant.Outcome != "open_new" {
		t.Fatalf("grant outcome = %s, want open_new", grant.Outcome)
	}
	bindingID = bindCandidate(t, b, jobID, candidateID, prefix, 11)
	if _, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE materialization_claims SET phase='navigated' WHERE binding_id=?`, bindingID); err != nil {
		t.Fatal(err)
	}
	return jobID, candidateID, bindingID, grant.GateOccurrenceID
}

func handoffParkedAnswer(t *testing.T, b *Bridge, bindingID, requestID string) *protocol.SurfaceCloseResponsePayload {
	t.Helper()
	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: requestID, BindingID: bindingID,
		BrowserHolderGeneration: b.arbitration.generation(), Disposition: "handoff_parked",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decodeSurfaceCloseResponse(t, frames)
}

// TestSurfaceCloseHandoffParkedRefusedWhileSignInLeaseLive pins rule 6 of the
// presence-aware surface lifecycle. A sign-in the operator is working through
// is not parked: an extension timer that fired three minutes after the tab
// opened asked to park it, and the daemon authorized the close because only an
// in-flight provider effect vetoed handoff_parked (measured 2026-10-14). The
// entry lease naming this binding is the daemon's own record of the live
// sign-in, so it vetoes too - for an old extension that still carries the timer.
func TestSurfaceCloseHandoffParkedRefusedWhileSignInLeaseLive(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	_, _, bindingID, _ := bindSignInSurface(t, b, "parked-live-lease")
	lease, found, err := jobs.GetAuthenticationEntryLease(ctx, "auth-parked-live-lease")
	if err != nil || !found || lease.OwnerBindingID != bindingID || lease.State != "reserved" {
		t.Fatalf("lease after bind = %+v found=%v err=%v, want reserved and owned by %s", lease, found, err, bindingID)
	}

	refused := func(label, id string) {
		t.Helper()
		got := handoffParkedAnswer(t, b, bindingID, id)
		if got.Outcome != "not_eligible" || !strings.Contains(got.Detail, "sign-in is in progress") ||
			got.CloseAuthorizationID != "" || got.Nonce != "" {
			t.Fatalf("%s: handoff_parked = %+v, want a structured not_eligible naming the live sign-in and no token", label, got)
		}
	}
	refused("reserved lease", "req-parked-reserved")

	// A human lease (auth_returned landed) is the same sign-in, one step on.
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='human', human_owner_id=owner_id, lease_until=NULL
		  WHERE authentication_claim_id=?`, "auth-parked-live-lease"); err != nil {
		t.Fatal(err)
	}
	refused("human lease", "req-parked-human")

	// The refusal is exactly the lease, not the claim: once the lease names no
	// live occupancy the request authorizes as before.
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='expired', owner_binding_id=NULL
		  WHERE authentication_claim_id=?`, "auth-parked-live-lease"); err != nil {
		t.Fatal(err)
	}
	if got := handoffParkedAnswer(t, b, bindingID, "req-parked-released"); got.Outcome != "authorized" {
		t.Fatalf("handoff_parked after the lease was released = %+v, want authorized", got)
	}
}

// A lease past its deadline, or one naming another binding, is not this
// binding's live sign-in.
func TestSurfaceCloseHandoffParkedIgnoresLapsedAndForeignLeases(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	_, _, bindingID, _ := bindSignInSurface(t, b, "parked-lapsed-lease")

	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET owner_binding_id='binding-of-another-surface'
		  WHERE authentication_claim_id=?`, "auth-parked-lapsed-lease"); err != nil {
		t.Fatal(err)
	}
	if got := handoffParkedAnswer(t, b, bindingID, "req-parked-foreign"); got.Outcome != "authorized" {
		t.Fatalf("handoff_parked with the lease owned by another binding = %+v, want authorized", got)
	}
	// Expire the issued token so the next request mints cleanly.
	if _, err := jobs.ExpireCloseAuthorizations(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	past := time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET owner_binding_id=?, lease_until=?
		  WHERE authentication_claim_id=?`, bindingID, past, "auth-parked-lapsed-lease"); err != nil {
		t.Fatal(err)
	}
	if got := handoffParkedAnswer(t, b, bindingID, "req-parked-lapsed"); got.Outcome != "authorized" {
		t.Fatalf("handoff_parked with a lapsed lease = %+v, want authorized", got)
	}
}

// TestOperatorClosedSignInIsNotAutomaticallyReoffered pins the other half of
// the same defect: two minutes after the tab vanished the daemon re-offered a
// new one by itself. When the operator closes the sign-in surface (owner_closed)
// the retired slot goes cold after
// AuthenticationEntryBindDeadline and used to be re-reserved and re-offered.
// The action stays open, and only an explicit open mints the next surface.
func TestOperatorClosedSignInIsNotAutomaticallyReoffered(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID, candidateID, bindingID, occurrence := bindSignInSurface(t, b, "operator-closed")

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-operator-closed", "auth-operator-closed",
		bindingID, occurrence, "observation-operator-closed", b.arbitration.generation(), 0, "owner_closed"))
	if ack := claimObservationAckPayload(t, msgs); ack.Outcome != "applied" {
		t.Fatalf("owner_closed outcome = %+v, want applied", ack)
	}
	closed, err := jobs.OperatorClosedSurfaceJobs(ctx, []string{jobID})
	if err != nil || !closed[jobID] {
		t.Fatalf("OperatorClosedSurfaceJobs after owner_closed = %v, %v; want the job", closed, err)
	}

	// The closed surface's own offer must be retired with it, or it reads as
	// a fresh outstanding offer: it lifts the hold and counts against the
	// budget. Nothing is deleted by hand here - only the real owner_closed.
	b.mu.Lock()
	_, stillTracked := b.materializationOffered[jobID]
	b.mu.Unlock()
	if stillTracked {
		t.Fatal("owner_closed left the closed surface's offer in materializationOffered")
	}

	// Well past the cold threshold and the offer's own expiry.
	future := b.now().Add(b.actionExpiry() + time.Hour)
	b.now = func() time.Time { return future }
	for range 3 {
		polled, _ := runSync(t, b)
		for _, m := range polled {
			if m.JobID == jobID && (m.Type == protocol.MsgInstitutionalCandidateOffer || m.Type == protocol.MsgJobOffer) {
				t.Fatalf("daemon re-offered an operator-closed sign-in on its own: %v", polled)
			}
		}
	}
	action, err := b.openHandoffForJob(ctx, jobID)
	if err != nil || action == nil || action.Status != "open" {
		t.Fatalf("handoff action after an operator close = %+v, %v; want it still open", action, err)
	}

	// Explicit open lifts the hold.
	b.mu.Lock()
	b.focusPending[jobID] = true
	b.mu.Unlock()
	opened, _ := runSync(t, b)
	offer := firstOfType(opened, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil || offer.JobID != jobID ||
		offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("explicit open after an operator close did not offer the candidate: %v", opened)
	}
}

// An authorization that was issued and never used (a deferred scaffold_idle,
// a tombstone an old extension cleared, one that expired) says nothing about
// who removed the tab, so it must not turn a genuine operator close into a
// papio close. The daemon never learns that an authorized close executed.
func TestUnusedCloseAuthorizationDoesNotDefeatAnOperatorClose(t *testing.T) {
	for _, disposition := range []string{"scaffold_idle", "handoff_parked"} {
		for _, expire := range []bool{false, true} {
			name := disposition
			if expire {
				name += "_expired"
			}
			t.Run(name, func(t *testing.T) {
				b, jobs, _, _ := newBridge(t)
				ctx := context.Background()
				prefix := "unused-auth-" + strings.ReplaceAll(name, "_", "-")
				jobID, _, bindingID, occurrence := bindSignInSurface(t, b, prefix)
				if _, _, err := jobs.IssueCloseAuthorization(ctx, bindingID, b.arbitration.generation(), disposition, b.now()); err != nil {
					t.Fatal(err)
				}
				if expire {
					if _, err := jobs.ExpireCloseAuthorizations(ctx, time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-"+prefix, "auth-"+prefix,
					bindingID, occurrence, "observation-"+prefix, b.arbitration.generation(), 0, "owner_closed"))
				if ack := claimObservationAckPayload(t, msgs); ack.Outcome != "applied" {
					t.Fatalf("owner_closed outcome = %+v, want applied", ack)
				}
				closed, err := jobs.OperatorClosedSurfaceJobs(ctx, []string{jobID})
				if err != nil || !closed[jobID] {
					t.Fatalf("OperatorClosedSurfaceJobs with an unused %s authorization = %v, %v; want the hold", name, closed, err)
				}
			})
		}
	}
}
