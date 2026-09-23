// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

// termsParkFixture builds two papers at one institution profile and one
// landed safety domain. The owner claims, binds and so takes the
// institution's single sign-in slot, exactly as the live JSTOR drive did.
type termsParkFixture struct {
	b                                 *Bridge
	jobs                              *job.Store
	owner, sibling                    string
	ownerCandidate, siblingCandidate  string
	ownerBinding, authenticationClaim string
}

func newTermsParkFixture(t *testing.T, prefix string) termsParkFixture {
	t.Helper()
	b, jobs, _, _ := newBridge(t)
	f := termsParkFixture{b: b, jobs: jobs}
	f.owner = parkInstitutional(t, jobs, "wr_"+prefix+"_owner", handoffWork(), "")
	siblingWork := handoffWork()
	siblingWork.DOI = "10.1002/example.43"
	f.sibling = parkInstitutional(t, jobs, "wr_"+prefix+"_sibling", siblingWork, "")
	runSync(t, b, materializationHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-"+prefix)
	f.ownerCandidate = explicitMaterializationCandidate(t, jobs, f.owner, "terms-"+prefix+"-owner")
	f.siblingCandidate = explicitMaterializationCandidate(t, jobs, f.sibling, "terms-"+prefix+"-sibling")
	f.ownerBinding = bindCandidate(t, b, f.owner, f.ownerCandidate, prefix+"-owner", 31)
	profiles, err := jobs.ListInstitutionProfiles(context.Background(), false)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("institution profiles = %+v, %v", profiles, err)
	}
	f.authenticationClaim = profiles[0].AuthenticationClaimID
	lease := f.lease(t)
	if lease.State != job.AuthenticationEntryLeaseReserved || lease.OwnerID != f.owner || lease.OwnerBindingID != f.ownerBinding {
		t.Fatalf("owner bind lease = %+v; the test needs the owner holding the slot", lease)
	}
	return f
}

func (f termsParkFixture) lease(t *testing.T) *job.AuthenticationEntryLease {
	t.Helper()
	lease, found, err := f.jobs.GetAuthenticationEntryLease(context.Background(), f.authenticationClaim)
	if err != nil || !found {
		t.Fatalf("lease = %+v found=%v err=%v", lease, found, err)
	}
	return lease
}

func (f termsParkFixture) ownerClaim(t *testing.T) *job.MaterializationClaim {
	t.Helper()
	claim, err := f.jobs.MaterializationClaimByBindingID(context.Background(), f.ownerBinding)
	if err != nil || claim == nil {
		t.Fatalf("owner claim = %+v, %v", claim, err)
	}
	return claim
}

func (f termsParkFixture) terms(t *testing.T) {
	t.Helper()
	runSync(t, f.b, inFrame(t, protocol.MsgProviderOutcome, f.owner,
		map[string]any{"outcome": "terms_acceptance_required", "adapter_id": "jstor"}))
}

// focusSibling opens the sibling the way the operator did at 09:41:47Z and
// reports whether the next poll offered its candidate.
func (f termsParkFixture) focusSiblingOffer(t *testing.T) *protocol.InstitutionalCandidateOfferPayload {
	t.Helper()
	if _, live, err := f.b.FocusHandoffs(context.Background(), []string{f.sibling}); err != nil || !live {
		t.Fatalf("focus sibling: live=%v err=%v", live, err)
	}
	msgs, _ := runSync(t, f.b)
	for _, msg := range msgs {
		if msg.Type == protocol.MsgInstitutionalCandidateOffer && msg.JobID == f.sibling {
			return msg.Payload.(*protocol.InstitutionalCandidateOfferPayload)
		}
	}
	return nil
}

// A terms page reached after the sign-in returned waits only for the operator.
// It must not keep the institution: measured live 2026-09-23, one JSTOR terms
// modal held the library's only slot and a `navigated` claim for 14 minutes,
// and six opened papers got no institutional effect until the holder was
// cycled by hand.
func TestTermsAfterSignInParksTheClaimAndFreesTheInstitution(t *testing.T) {
	f := newTermsParkFixture(t, "terms-park")
	ctx := context.Background()
	runSync(t, f.b,
		inFrame(t, protocol.MsgAuthPending, f.owner, map[string]any{"elapsed_ms": 5}),
		inFrame(t, protocol.MsgAuthReturned, f.owner, map[string]any{"elapsed_ms": 900}))
	if lease := f.lease(t); lease.State != job.AuthenticationEntryLeaseHuman || lease.OwnerBindingID != f.ownerBinding {
		t.Fatalf("lease after auth_returned = %+v; want the owner's returned sign-in", lease)
	}

	f.terms(t)

	if lease := f.lease(t); lease.State == job.AuthenticationEntryLeaseHuman ||
		lease.State == job.AuthenticationEntryLeaseReserved || lease.OwnerBindingID != "" {
		t.Fatalf("lease after terms = %+v; the returned sign-in still holds the institution", lease)
	}
	claim := f.ownerClaim(t)
	if claim.Phase != "parked" || claim.TabID != 31 {
		t.Fatalf("owner claim after terms = %+v; want parked on its own tab", claim)
	}
	if live, _, err := f.jobs.LiveMaterializationClaimForJob(ctx, f.owner, 1, f.b.arbitration.generation()); err != nil || live != nil {
		t.Fatalf("owner still has a live claim %+v, %v", live, err)
	}
	actions, err := f.jobs.ListOpenHumanActionsForJobs(ctx, []string{f.owner})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, action := range actions {
		kinds[action.Kind] = true
	}
	if !kinds[handoffActionKind] || !kinds[termsActionKind] {
		t.Fatalf("owner actions = %+v; want the handoff and the terms step both open", actions)
	}

	// The sibling proceeds on the next poll: offered, bound, and its
	// institutional effect authorized - no sweep, no holder round trip.
	offer := f.focusSiblingOffer(t)
	if offer == nil {
		t.Fatal("sibling was not offered while the owner waited on its terms page")
	}
	claimed, _ := runSync(t, f.b, inFrame(t, protocol.MsgInstitutionalClaimRequest, f.sibling,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "terms-park-sibling-claim", CandidateID: offer.CandidateID, MaterializationKind: "browser_tab",
		}))
	claimPayload := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse).Payload.(*protocol.InstitutionalClaimResponsePayload)
	if claimPayload.Outcome != "claimed" {
		t.Fatalf("sibling claim = %+v, want claimed", claimPayload)
	}
	bound, _ := runSync(t, f.b, inFrame(t, protocol.MsgInstitutionalBindRequest, f.sibling,
		protocol.InstitutionalBindRequestPayload{
			RequestID: "terms-park-sibling-bind", ClaimID: claimPayload.ClaimID,
			BindingID: claimPayload.BindingID, TabID: 32,
		}))
	if bindPayload := firstOfType(bound, protocol.MsgInstitutionalBindResponse).Payload.(*protocol.InstitutionalBindResponsePayload); bindPayload.Outcome != "bound" {
		t.Fatalf("sibling bind = %+v, want bound", bindPayload)
	}
	siblingBinding := claimPayload.BindingID
	siblingClaim, err := f.jobs.MaterializationClaimByBindingID(ctx, siblingBinding)
	if err != nil || siblingClaim == nil {
		t.Fatalf("sibling claim = %+v, %v", siblingClaim, err)
	}
	routed, _ := runSync(t, f.b, inFrame(t, protocol.MsgInstitutionalRouteRequest, f.sibling,
		protocol.InstitutionalRouteRequestPayload{
			RequestID: "terms-park-sibling-route", ClaimID: siblingClaim.ID, BindingID: siblingBinding,
			InstitutionalRequestID: "terms-park-sibling-request",
		}))
	route := firstOfType(routed, protocol.MsgInstitutionalRouteResponse)
	if route == nil || route.Payload.(*protocol.InstitutionalRouteResponsePayload).Outcome != "issued" {
		t.Fatalf("sibling route = %v; want issued", routed)
	}
	events, err := f.jobs.Events(ctx, f.sibling)
	if err != nil {
		t.Fatal(err)
	}
	authorized := false
	for _, event := range events {
		authorized = authorized || event["kind"] == "browser.institutional_effect_authorized"
	}
	if !authorized {
		t.Fatal("sibling institutional effect was not authorized")
	}

	// The parked tab is still papio's: reconciliation confirms it, so the
	// extension keeps the tab the operator must accept the terms on.
	reconciled, _ := runSync(t, f.b, inFrame(t, protocol.MsgInstitutionalReconcileRequest, "",
		protocol.InstitutionalReconcileRequestPayload{
			RequestID: "terms-park-reconcile",
			Bindings:  []protocol.InstitutionalReconcileBinding{{BindingID: f.ownerBinding, TabID: 31}},
		}))
	reconcile := firstOfType(reconciled, protocol.MsgInstitutionalReconcileResponse)
	if reconcile == nil {
		t.Fatalf("reconcile response missing: %v", reconciled)
	}
	kept := false
	for _, c := range reconcile.Payload.(*protocol.InstitutionalReconcileResponsePayload).Claims {
		kept = kept || c.BindingID == f.ownerBinding
	}
	if !kept {
		t.Fatalf("reconcile dropped the parked tab: %+v", reconcile.Payload)
	}

	// The extension repeats the outcome every minute while the modal shows,
	// and a session going live re-offers siblings. Neither may drive the
	// owner again, nor take the slot back from the sibling.
	f.terms(t)
	for range 2 {
		msgs, _ := runSync(t, f.b,
			inFrame(t, protocol.MsgAuthReturned, f.sibling, map[string]any{"elapsed_ms": 700}))
		if offersForJob(msgs, f.owner) != 0 || countJobOffersFor(msgs, f.owner) != 0 {
			t.Fatalf("owner re-offered while its terms step is open: %v", msgs)
		}
	}
	events, err = f.jobs.Events(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			t.Fatalf("owner re-offered on a live session while its terms step is open: %v", event)
		}
	}
	// The repeated outcome retires nothing (the claim is already parked),
	// so it must not evict the job that now owns the slot.
	if lease := f.lease(t); lease.State == job.AuthenticationEntryLeaseReserved && lease.OwnerID == f.owner {
		t.Fatalf("lease = %+v; the repeated terms outcome stole the slot from the sibling", lease)
	}
}

// A terms outcome while the sign-in has not returned is a human still at the
// identity provider. The slot and the live claim stay theirs.
func TestTermsBeforeSignInReturnsKeepsTheInstitution(t *testing.T) {
	f := newTermsParkFixture(t, "terms-hold")
	runSync(t, f.b, inFrame(t, protocol.MsgAuthPending, f.owner, map[string]any{"elapsed_ms": 5}))

	f.terms(t)

	if lease := f.lease(t); lease.State != job.AuthenticationEntryLeaseReserved ||
		lease.OwnerID != f.owner || lease.OwnerBindingID != f.ownerBinding {
		t.Fatalf("lease = %+v; a sign-in in progress lost the institution", lease)
	}
	if claim := f.ownerClaim(t); claim.Phase != "bound" {
		t.Fatalf("owner claim = %+v; want it still live", claim)
	}
	// The sibling is offered here: an explicit operator Open always serves its
	// own job, even while another job's sign-in is in progress, and at the
	// time of the incident that focus lane had no slot arbitration. The
	// acceptance is only that the slot stays reserved to the owner: a second
	// terms outcome may not take it, and the owner's claim stays live.
	f.terms(t)
	if lease := f.lease(t); lease.State != job.AuthenticationEntryLeaseReserved ||
		lease.OwnerID != f.owner || lease.OwnerBindingID != f.ownerBinding {
		t.Fatalf("lease = %+v; a repeated terms outcome took the sign-in slot", lease)
	}
	if claim := f.ownerClaim(t); claim.Phase != "bound" {
		t.Fatalf("owner claim = %+v; a repeated terms outcome retired the sign-in", claim)
	}
}
