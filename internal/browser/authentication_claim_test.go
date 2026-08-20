// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Tests for the claim-observation protocol
// (dev/active/claim-observation-protocol.md §2.1/§2.2): the
// authentication_claim_request/response arbitration reducer and the
// claim_observation/claim_observation_ack idempotency/ordering reducer.

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// authClaimHello negotiates every feature the full candidate -> claim ->
// bind -> authentication-claim -> observation pipeline exercises in these
// tests.
func authClaimHello(t *testing.T) json.RawMessage {
	t.Helper()
	return helloWithFeatures(t, "0.14.0",
		institutionalMaterializationFeature, effectPermitFeature, institutionalAuthenticationClaimFeature)
}

// seedAuthenticationClaimProfile reconciles one institution profile with a
// caller-chosen authentication_claim_id, mirroring every existing
// institutional test's ReconcileInstitutionProfiles pattern.
func seedAuthenticationClaimProfile(t *testing.T, jobs *job.Store, claimID string) {
	t.Helper()
	profiles, err := jobs.ReconcileInstitutionProfiles(context.Background(), []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-" + claimID, AuthenticationClaimID: claimID,
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("reconcile institution profile: %v (%d)", err, len(profiles))
	}
}

// authClaimResponse extracts and requires the authentication_claim_response
// from a Sync reply.
func authClaimResponse(t *testing.T, msgs []*protocol.BrowserMessage) *protocol.AuthenticationClaimResponsePayload {
	t.Helper()
	m := firstOfType(msgs, protocol.MsgAuthenticationClaimResponse)
	if m == nil {
		t.Fatalf("authentication_claim_response missing: %v", msgs)
	}
	return m.Payload.(*protocol.AuthenticationClaimResponsePayload)
}

// claimObservationAck extracts and requires the claim_observation_ack from
// a Sync reply.
func claimObservationAckPayload(t *testing.T, msgs []*protocol.BrowserMessage) *protocol.ClaimObservationAckPayload {
	t.Helper()
	m := firstOfType(msgs, protocol.MsgClaimObservationAck)
	if m == nil {
		t.Fatalf("claim_observation_ack missing: %v", msgs)
	}
	return m.Payload.(*protocol.ClaimObservationAckPayload)
}

// bindCandidate runs institutional_claim_request then institutional_bind_request
// for one candidate and returns the resulting binding id.
func bindCandidate(t *testing.T, b *Bridge, jobID, candidateID, requestPrefix string, tabID int64) string {
	t.Helper()
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: requestPrefix + "-claim", CandidateID: candidateID, MaterializationKind: "browser_tab",
		}))
	claimResp := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResp == nil {
		t.Fatalf("institutional_claim_response missing: %v", claimed)
	}
	claimPayload := claimResp.Payload.(*protocol.InstitutionalClaimResponsePayload)
	if claimPayload.Outcome != "claimed" {
		t.Fatalf("institutional claim outcome = %s, want claimed: %+v", claimPayload.Outcome, claimPayload)
	}
	bound, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalBindRequest, jobID,
		protocol.InstitutionalBindRequestPayload{
			RequestID: requestPrefix + "-bind", ClaimID: claimPayload.ClaimID,
			BindingID: claimPayload.BindingID, TabID: tabID,
		}))
	bindResp := firstOfType(bound, protocol.MsgInstitutionalBindResponse)
	if bindResp == nil || bindResp.Payload.(*protocol.InstitutionalBindResponsePayload).Outcome != "bound" {
		t.Fatalf("institutional bind = %v, want bound", bound)
	}
	return claimPayload.BindingID
}

func TestAuthenticationClaimOpenNewThenNavigateExistingAfterBind(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_auth_claim_open_new", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-open-new")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-open-new")

	first, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-req-0001", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	openNew := authClaimResponse(t, first)
	if openNew.Outcome != "open_new" {
		t.Fatalf("outcome = %s, want open_new: %+v", openNew.Outcome, openNew)
	}
	if openNew.AuthenticationClaimID != "auth-claim-open-new" || openNew.GateOccurrenceID == "" || openNew.LeaseUntil == "" {
		t.Fatalf("open_new response missing required fields: %+v", openNew)
	}
	if openNew.OwnerBindingID != "" || openNew.DependentCount != nil {
		t.Fatalf("open_new carried fields forbidden on it: %+v", openNew)
	}

	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-claim-open-new", 11)

	second, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-req-0002", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	navigateExisting := authClaimResponse(t, second)
	if navigateExisting.Outcome != "navigate_existing" {
		t.Fatalf("outcome after bind = %s, want navigate_existing: %+v", navigateExisting.Outcome, navigateExisting)
	}
	if navigateExisting.OwnerBindingID != bindingID {
		t.Fatalf("owner_binding_id = %q, want %q", navigateExisting.OwnerBindingID, bindingID)
	}
	if navigateExisting.OwnerTabHint == nil || *navigateExisting.OwnerTabHint != 11 {
		t.Fatalf("owner_tab_hint = %v, want 11", navigateExisting.OwnerTabHint)
	}
	if navigateExisting.GateOccurrenceID != openNew.GateOccurrenceID {
		t.Fatalf("gate occurrence rolled over without a reopen: first=%s second=%s", openNew.GateOccurrenceID, navigateExisting.GateOccurrenceID)
	}
}

// TestInstitutionalBindAcquiresTheInstitutionEntry pins the live stall found on
// 2026-08-20: the daemon-orchestrated pipeline (candidate offer -> claim ->
// scaffold -> bind) has no consult in it, so a paper reached bind without ever
// reserving the institution's authentication entry. The bind records itself as
// the entry's owner-binding and fails closed when that write does not
// fence-match, so an entry row left by any other job made every bind answer
// "stale" forever - measured live at ~2s per attempt, minting and removing a
// scaffold each pass. The bind must acquire the slot through the same
// arbitration the consult uses.
func TestInstitutionalBindAcquiresTheInstitutionEntry(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	strandedJob := parkInstitutional(t, jobs, "wr_bind_acquire_stranded", handoffWork(), "")
	jobID := parkInstitutional(t, jobs, "wr_bind_acquire", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-bind-acquire")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-bind-acquire")

	// Another job's reservation, already lapsed - exactly the row the live
	// institution carried for twenty hours.
	if _, err := jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "auth-claim-bind-acquire", LeaseID: "lease-stranded",
		OwnerID: strandedJob, BrowserHolderGeneration: 1,
		LeaseUntil: b.now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// No authentication_claim_request anywhere: straight to claim and bind.
	bindingID := bindCandidate(t, b, jobID, candidateID, "bind-acquire", 31)

	lease, ok, err := jobs.GetAuthenticationEntryLease(ctx, "auth-claim-bind-acquire")
	if err != nil || !ok {
		t.Fatalf("entry lease after the bind: ok=%v err=%v", ok, err)
	}
	if lease.OwnerID != jobID {
		t.Fatalf("entry owner = %q, want the binding job %q", lease.OwnerID, jobID)
	}
	if lease.OwnerBindingID != bindingID {
		t.Fatalf("owner_binding_id = %q, want %q - the bind must record its own surface", lease.OwnerBindingID, bindingID)
	}
}

// TestInstitutionalBindRefusedWhileAnotherSignInIsLive pins the other half: the
// acquisition above is the arbitration, not a bypass of it. One sign-in surface
// per institution still holds, and the refusal must be the outcome the
// extension answers by retiring the scaffold rather than retrying forever.
func TestInstitutionalBindRefusedWhileAnotherSignInIsLive(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	ownerJob := parkInstitutional(t, jobs, "wr_bind_live_owner", handoffWork(), "")
	rivalJob := parkInstitutional(t, jobs, "wr_bind_live_rival", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-bind-live")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, ownerJob, "domain-bind-live-owner")
	rivalCandidate := explicitMaterializationCandidate(t, jobs, rivalJob, "domain-bind-live-rival")

	runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, ownerJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "bind-live-owner-consult", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	bindCandidate(t, b, ownerJob, ownerCandidate, "bind-live-owner", 41)

	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, rivalJob,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "bind-live-rival-claim", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab",
		}))
	claimResp := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResp == nil {
		t.Fatalf("institutional_claim_response missing: %v", claimed)
	}
	claimPayload := claimResp.Payload.(*protocol.InstitutionalClaimResponsePayload)
	if claimPayload.Outcome != "claimed" {
		t.Fatalf("rival claim outcome = %s, want claimed: %+v", claimPayload.Outcome, claimPayload)
	}
	bound, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalBindRequest, rivalJob,
		protocol.InstitutionalBindRequestPayload{
			RequestID: "bind-live-rival-bind", ClaimID: claimPayload.ClaimID,
			BindingID: claimPayload.BindingID, TabID: 42,
		}))
	bindResp := firstOfType(bound, protocol.MsgInstitutionalBindResponse)
	if bindResp == nil {
		t.Fatalf("institutional_bind_response missing: %v", bound)
	}
	payload := bindResp.Payload.(*protocol.InstitutionalBindResponsePayload)
	if payload.Outcome != "not_eligible" {
		t.Fatalf("rival bind outcome = %s, want not_eligible so the scaffold is retired: %+v", payload.Outcome, payload)
	}
	if payload.Detail == "" {
		t.Fatalf("refusal must name itself: %+v", payload)
	}
	lease, ok, err := jobs.GetAuthenticationEntryLease(ctx, "auth-claim-bind-live")
	if err != nil || !ok {
		t.Fatalf("entry lease after the refusal: ok=%v err=%v", ok, err)
	}
	if lease.OwnerID != ownerJob {
		t.Fatalf("entry owner = %q, want the live owner %q - a refused bind must not steal the slot", lease.OwnerID, ownerJob)
	}
}

func TestAuthenticationClaimBusyGrantsFocusOwnerExplicitAndParksAutomatic(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ownerJob := parkInstitutional(t, jobs, "wr_auth_claim_busy_owner", handoffWork(), "")
	rivalJob := parkInstitutional(t, jobs, "wr_auth_claim_busy_rival", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-busy")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, ownerJob, "domain-busy-owner")
	rivalCandidate := explicitMaterializationCandidate(t, jobs, rivalJob, "domain-busy-rival")

	runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, ownerJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-busy-owner-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	bindingID := bindCandidate(t, b, ownerJob, ownerCandidate, "auth-claim-busy-owner", 22)

	explicit, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, rivalJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-busy-rival-explicit", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab", Trigger: "explicit",
		}))
	focusOwner := authClaimResponse(t, explicit)
	if focusOwner.Outcome != "focus_owner" {
		t.Fatalf("explicit trigger outcome = %s, want focus_owner: %+v", focusOwner.Outcome, focusOwner)
	}
	if focusOwner.OwnerBindingID != bindingID {
		t.Fatalf("focus_owner owner_binding_id = %q, want %q", focusOwner.OwnerBindingID, bindingID)
	}
	if focusOwner.OwnerTabHint == nil || *focusOwner.OwnerTabHint != 22 {
		t.Fatalf("focus_owner owner_tab_hint = %v, want 22", focusOwner.OwnerTabHint)
	}

	automatic, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, rivalJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-busy-rival-automatic", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	park := authClaimResponse(t, automatic)
	if park.Outcome != "park" {
		t.Fatalf("automatic trigger outcome = %s, want park: %+v", park.Outcome, park)
	}
	if park.DependentCount == nil || *park.DependentCount < 1 {
		t.Fatalf("park dependent_count = %v, want at least 1", park.DependentCount)
	}
	if park.LeaseUntil != "" || park.OwnerBindingID != "" {
		t.Fatalf("park carried fields forbidden on it: %+v", park)
	}
}

// TestAuthenticationClaimParkReleasesTheUnconsumedClaim pins that a refusal
// gives back what the job took to ask. Claiming a candidate and arbitrating the
// institution's entry are two round trips and the claim comes first, flipping
// the candidate to 'claimed' — the state the scheduler reads as "in progress".
// A park therefore used to strand an unconsumed claim (tab 0, no route, no
// effect) for its whole lease: the paper could not retry and the scheduler could
// not re-offer it when the entry freed. Measured live 2026-08-19:
// claim_009d4edb minted 05:35:26Z, parked one second later, still 'claimed'
// with tab 0 half an hour on while its paper reported waiting for the operator.
func TestAuthenticationClaimParkReleasesTheUnconsumedClaim(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	ownerJob := parkInstitutional(t, jobs, "wr_auth_park_release_owner", handoffWork(), "")
	rivalWork := handoffWork()
	rivalWork.DOI = "10.1002/example.77"
	rivalJob := parkInstitutional(t, jobs, "wr_auth_park_release_rival", rivalWork, "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-park-release")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, ownerJob, "domain-park-release-owner")
	rivalCandidate := explicitMaterializationCandidate(t, jobs, rivalJob, "domain-park-release-rival")

	runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, ownerJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-park-release-owner-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	bindCandidate(t, b, ownerJob, ownerCandidate, "auth-park-release-owner", 31)

	// The rival claims its candidate, then loses the arbitration.
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, rivalJob,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "auth-park-release-rival-claim", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab",
		}))
	claimResp := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResp == nil {
		t.Fatalf("institutional_claim_response missing: %v", claimed)
	}
	claimPayload := claimResp.Payload.(*protocol.InstitutionalClaimResponsePayload)
	if claimPayload.Outcome != "claimed" {
		t.Fatalf("rival claim outcome = %s, want claimed: %+v", claimPayload.Outcome, claimPayload)
	}
	if candidate, err := jobs.GetBrowserCandidate(ctx, rivalCandidate); err != nil || candidate == nil || candidate.Status != "claimed" {
		t.Fatalf("candidate before the park = %+v err=%v, want claimed", candidate, err)
	}

	parked, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, rivalJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-park-release-rival-consult", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	if park := authClaimResponse(t, parked); park.Outcome != "park" {
		t.Fatalf("rival outcome = %s, want park: %+v", park.Outcome, park)
	}
	candidate, err := jobs.GetBrowserCandidate(ctx, rivalCandidate)
	if err != nil || candidate == nil {
		t.Fatalf("candidate after the park: %+v err=%v", candidate, err)
	}
	if candidate.Status != "eligible" {
		t.Fatalf("candidate after the park = %q, want eligible so the scheduler can re-offer it", candidate.Status)
	}
	claim, err := jobs.MaterializationClaimByBindingID(ctx, claimPayload.BindingID)
	if err != nil {
		t.Fatalf("claim read after the park: %v", err)
	}
	if claim != nil && claim.Phase != "abandoned" {
		t.Fatalf("claim after the park = %q, want abandoned (no surface was ever created)", claim.Phase)
	}
}

// TestAuthenticationClaimBusyWithoutBindingParksEvenExplicit pins the
// no-surface-to-focus fallback: a busy lease whose owner has not yet bound
// a surface has nothing for focus_owner to name, so even an explicit
// trigger parks.
func TestAuthenticationClaimBusyWithoutBindingParksEvenExplicit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ownerJob := parkInstitutional(t, jobs, "wr_auth_claim_unbound_owner", handoffWork(), "")
	rivalJob := parkInstitutional(t, jobs, "wr_auth_claim_unbound_rival", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-busy-unbound")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, ownerJob, "domain-unbound-owner")
	rivalCandidate := explicitMaterializationCandidate(t, jobs, rivalJob, "domain-unbound-rival")

	runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, ownerJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-unbound-owner-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))

	explicit, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, rivalJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-unbound-rival-explicit", CandidateID: rivalCandidate,
			MaterializationKind: "browser_tab", Trigger: "explicit",
		}))
	result := authClaimResponse(t, explicit)
	if result.Outcome != "park" {
		t.Fatalf("explicit trigger against an unbound owner = %s, want park: %+v", result.Outcome, result)
	}
}

// TestAuthenticationClaimDisabledResponseShape pins the defense-in-depth
// feature_disabled/rejected response institutionalAuthenticationClaimDisabled
// builds. It is unreachable from ordinary wire traffic (the dispatch gate in
// handle() checks the daemon's own hardcoded advertised feature list, never
// the session's — see that gate's doc comment), so this calls the helper
// directly, the same way genuinely defense-in-depth branches are pinned
// elsewhere in this package.
func TestAuthenticationClaimDisabledResponseShape(t *testing.T) {
	b, _, _, _ := newBridge(t)
	requestFrames, err := b.institutionalAuthenticationClaimDisabled(&protocol.BrowserMessage{
		Type: protocol.MsgAuthenticationClaimRequest, JobID: "job-claim-disabled-0001",
		Payload: &protocol.AuthenticationClaimRequestPayload{RequestID: "auth-claim-disabled-req"},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestMsg, err := protocol.DecodeBrowserMessage(requestFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	requestResult := requestMsg.Payload.(*protocol.AuthenticationClaimResponsePayload)
	if requestResult.Outcome != "feature_disabled" || requestResult.AuthenticationClaimID != "" || requestResult.GateOccurrenceID != "" {
		t.Fatalf("authentication_claim_request disabled response = %+v", requestResult)
	}

	observationFrames, err := b.institutionalAuthenticationClaimDisabled(&protocol.BrowserMessage{
		Type: protocol.MsgClaimObservation, JobID: "job-claim-disabled-0002",
		Payload: &protocol.ClaimObservationPayload{RequestID: "obs-disabled-req", GateOccurrenceID: "occurrence-disabled-0001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	observationMsg, err := protocol.DecodeBrowserMessage(observationFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	observationResult := observationMsg.Payload.(*protocol.ClaimObservationAckPayload)
	// claim_observation_ack has no feature_disabled outcome in its closed
	// vocabulary, so the unnegotiated case acks "rejected" instead, echoing
	// the request's own occurrence id since the daemon has no better one.
	if observationResult.Outcome != "rejected" || observationResult.GateOccurrenceID != "occurrence-disabled-0001" {
		t.Fatalf("claim_observation disabled response = %+v", observationResult)
	}
}

func TestAuthenticationClaimNotEligibleForForeignCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ownerJob := parkInstitutional(t, jobs, "wr_auth_claim_foreign_owner", handoffWork(), "")
	otherJob := parkInstitutional(t, jobs, "wr_auth_claim_foreign_other", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-foreign")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, ownerJob, "domain-foreign")

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, otherJob,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-claim-foreign-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	result := authClaimResponse(t, msgs)
	if result.Outcome != "not_eligible" {
		t.Fatalf("outcome for a candidate belonging to another job = %s, want not_eligible: %+v", result.Outcome, result)
	}
}

func claimObservationFrame(t *testing.T, jobID, requestID, claimID, bindingID, occurrenceID, observationID string, generation, ordinal int64, eventKind string) json.RawMessage {
	t.Helper()
	return inFrame(t, protocol.MsgClaimObservation, jobID, protocol.ClaimObservationPayload{
		RequestID: requestID, AuthenticationClaimID: claimID, BindingID: bindingID,
		BrowserHolderGeneration: generation, GateOccurrenceID: occurrenceID,
		ObservationID: observationID, EventOrdinal: ordinal, EventKind: eventKind,
	})
}

func TestClaimObservationIdempotencyTrio(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_trio", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-trio")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-trio")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-trio-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-trio", 3)
	generation := b.epoch

	applied, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-trio-req-1", "auth-observation-trio",
		bindingID, grant.GateOccurrenceID, "observation-trio-1", generation, 0, "wall_observed"))
	appliedAck := claimObservationAckPayload(t, applied)
	if appliedAck.Outcome != "applied" || appliedAck.LeaseUntil == "" {
		t.Fatalf("first observation = %+v, want applied with lease_until", appliedAck)
	}

	duplicate, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-trio-req-2", "auth-observation-trio",
		bindingID, grant.GateOccurrenceID, "observation-trio-1", generation, 0, "wall_observed"))
	duplicateAck := claimObservationAckPayload(t, duplicate)
	if duplicateAck.Outcome != "duplicate" {
		t.Fatalf("exact replay = %+v, want duplicate", duplicateAck)
	}

	rejected, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-trio-req-3", "auth-observation-trio",
		bindingID, grant.GateOccurrenceID, "observation-trio-1", generation, 1, "wall_observed"))
	rejectedAck := claimObservationAckPayload(t, rejected)
	if rejectedAck.Outcome != "rejected" {
		t.Fatalf("mismatched replay = %+v, want rejected", rejectedAck)
	}

	stale, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-trio-req-4", "auth-observation-trio",
		bindingID, grant.GateOccurrenceID, "observation-trio-stale", generation, 0, "wall_observed"))
	staleAck := claimObservationAckPayload(t, stale)
	if staleAck.Outcome != "stale" {
		t.Fatalf("superseded ordinal = %+v, want stale", staleAck)
	}

	next, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-trio-req-5", "auth-observation-trio",
		bindingID, grant.GateOccurrenceID, "observation-trio-next", generation, 1, "login_started"))
	nextAck := claimObservationAckPayload(t, next)
	if nextAck.Outcome != "applied" {
		t.Fatalf("higher ordinal = %+v, want applied", nextAck)
	}
}

func TestClaimObservationStaleGenerationMutatesNothing(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_stale_gen", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-stale-gen")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-stale-gen")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-stale-gen-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-stale-gen", 4)
	staleGeneration := b.epoch

	before, beforeFound, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-stale-gen")
	if err != nil || !beforeFound {
		t.Fatalf("lease before stale observation: %+v %v", before, err)
	}

	// Bump the holder generation so staleGeneration is now behind current.
	b.mu.Lock()
	b.epoch++
	b.mu.Unlock()

	stale, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-stale-gen-req", "auth-observation-stale-gen",
		bindingID, grant.GateOccurrenceID, "observation-stale-gen", staleGeneration, 0, "wall_observed"))
	staleAck := claimObservationAckPayload(t, stale)
	if staleAck.Outcome != "stale" {
		t.Fatalf("outcome = %+v, want stale", staleAck)
	}
	if staleAck.BrowserHolderGeneration != b.epoch {
		t.Fatalf("stale ack browser_holder_generation = %d, want current %d", staleAck.BrowserHolderGeneration, b.epoch)
	}

	after, afterFound, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-stale-gen")
	if err != nil || !afterFound {
		t.Fatalf("lease after stale observation: %+v %v", after, err)
	}
	if after.LeaseUntil != before.LeaseUntil || after.State != before.State {
		t.Fatalf("stale-generation observation mutated the lease: before=%+v after=%+v", before, after)
	}
	var journalRows int
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM claim_observation_journal WHERE observation_id='observation-stale-gen'`,
	).Scan(&journalRows); err != nil {
		t.Fatal(err)
	}
	if journalRows != 0 {
		t.Fatalf("stale-generation observation was journaled: %d rows", journalRows)
	}
}

func TestClaimObservationAuthReturnedPromotesLeaseToHuman(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_returned", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-returned")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-returned")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-returned-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-returned", 5)

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-returned-req", "auth-observation-returned",
		bindingID, grant.GateOccurrenceID, "observation-returned", b.epoch, 0, "auth_returned"))
	ack := claimObservationAckPayload(t, msgs)
	if ack.Outcome != "applied" {
		t.Fatalf("auth_returned outcome = %+v, want applied", ack)
	}
	if ack.LeaseUntil != "" {
		t.Fatalf("auth_returned ack carried lease_until, forbidden outside wall/login/mfa/challenge: %+v", ack)
	}
	lease, found, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-returned")
	if err != nil || !found || lease.State != job.AuthenticationEntryLeaseHuman || lease.HumanOwnerID != jobID {
		t.Fatalf("lease after auth_returned = %+v found=%v err=%v, want human owned by %s", lease, found, err, jobID)
	}
}

// TestClaimObservationSurvivesAReconnectSinceArbitration pins the operator's
// login journal against generation churn. The entry lease records the holder
// generation it was reserved under; a browser reconnect between arbitration and
// the human finishing sign-in promotes a new holder and bumps that generation.
// The reducer used to require the lease's own generation to equal the current
// one, so every observation for such a login was rejected for good, and since
// nothing logged or persisted the ack outcome, a permanently refused journal was
// indistinguishable from a login nobody attempted: measured live 2026-08-19,
// claim_observation_journal held zero rows across weeks of real sign-ins. The
// sender's own staleness is fenced separately (FrameGeneration != Generation ->
// stale), which TestClaimObservationStaleGenerationMutatesNothing pins.
func TestClaimObservationSurvivesAReconnectSinceArbitration(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_reconnect", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-reconnect")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-reconnect")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-reconnect-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-reconnect", 5)
	reservedUnder := b.epoch

	// The service worker dies mid-login and reconnects as a new session: the
	// port closes (goodbye), then the fresh worker says hello and is promoted,
	// which is what advances the holder generation live.
	if _, err := b.Sync(ctx, testSessionID, true, nil); err != nil {
		t.Fatalf("goodbye for the dying worker: %v", err)
	}
	runSyncAs(t, b, "session-after-reconnect", authClaimHello(t))
	if b.epoch == reservedUnder {
		t.Fatalf("reconnect did not advance the holder generation (still %d)", b.epoch)
	}

	msgs, _ := runSyncAs(t, b, "session-after-reconnect",
		claimObservationFrame(t, jobID, "obs-reconnect-req", "auth-observation-reconnect",
			bindingID, grant.GateOccurrenceID, "observation-reconnect", b.epoch, 0, "wall_observed"))
	ack := claimObservationAckPayload(t, msgs)
	if ack.Outcome != "applied" {
		t.Fatalf("wall_observed after a reconnect = %+v, want applied", ack)
	}
	lease, found, err := jobs.GetAuthenticationEntryLease(ctx, "auth-observation-reconnect")
	if err != nil || !found {
		t.Fatalf("lease read after renewal: found=%v err=%v", found, err)
	}
	if lease.BrowserHolderGeneration != b.epoch {
		t.Fatalf("renewal left generation %d, want it carried forward to %d",
			lease.BrowserHolderGeneration, b.epoch)
	}
	if lease.State != job.AuthenticationEntryLeaseReserved || lease.OwnerID != jobID {
		t.Fatalf("lease after renewal = %+v, want reserved and owned by %s", lease, jobID)
	}
}

// TestClaimObservationDuplicateStaleRejectedNeverTouchLeaseOrEvidence pins
// §3's no-op guarantee (claim_observation_apply.go's ApplyClaimObservation
// doc comment: "a duplicate, stale, or rejected observation is a true no-op
// that never touches lease state") specifically for auth_returned, the one
// event kind whose apply path ALSO writes profile_evidence
// (AuthReturnedEvidenceObservationID) alongside the lease promotion
// TestClaimObservationAuthReturnedPromotesLeaseToHuman already pins for the
// applied path. TestClaimObservationIdempotencyTrio already proves the
// outcome strings for duplicate/rejected/stale; checking outcome alone
// cannot catch a reducer that mutates durable state before returning one of
// those closed outcomes, so this reads both rows before and after each
// replay.
func TestClaimObservationDuplicateStaleRejectedNeverTouchLeaseOrEvidence(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_dup_evidence", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-dup-evidence")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-dup-evidence")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-dup-evidence-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-dup-evidence", 6)
	generation := b.epoch

	applied, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-dup-evidence-req-1", "auth-observation-dup-evidence",
		bindingID, grant.GateOccurrenceID, "observation-dup-evidence-1", generation, 0, "auth_returned"))
	appliedAck := claimObservationAckPayload(t, applied)
	if appliedAck.Outcome != "applied" {
		t.Fatalf("first auth_returned observation = %+v, want applied", appliedAck)
	}

	readState := func() (evidenceCount int, leaseState job.AuthenticationEntryLeaseState, leaseUntil string) {
		t.Helper()
		if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM profile_evidence`).Scan(&evidenceCount); err != nil {
			t.Fatal(err)
		}
		lease, found, err := jobs.GetAuthenticationEntryLease(ctx, "auth-observation-dup-evidence")
		if err != nil || !found {
			t.Fatalf("lease read: found=%v err=%v", found, err)
		}
		return evidenceCount, lease.State, lease.LeaseUntil
	}
	assertUnchanged := func(wantEvidence int, wantState job.AuthenticationEntryLeaseState, wantLeaseUntil string) {
		t.Helper()
		evidence, state, leaseUntil := readState()
		if evidence != wantEvidence || state != wantState || leaseUntil != wantLeaseUntil {
			t.Fatalf("observation mutated durable state: evidence=%d(%d) state=%q(%q) lease_until=%q(%q)",
				evidence, wantEvidence, state, wantState, leaseUntil, wantLeaseUntil)
		}
	}
	baselineEvidence, baselineState, baselineLeaseUntil := readState()
	if baselineState != job.AuthenticationEntryLeaseHuman {
		t.Fatalf("baseline lease state = %q, want human", baselineState)
	}

	// Exact replay: same observation_id, ordinal, and occurrence.
	duplicate, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-dup-evidence-req-2", "auth-observation-dup-evidence",
		bindingID, grant.GateOccurrenceID, "observation-dup-evidence-1", generation, 0, "auth_returned"))
	duplicateAck := claimObservationAckPayload(t, duplicate)
	if duplicateAck.Outcome != "duplicate" {
		t.Fatalf("exact replay = %+v, want duplicate", duplicateAck)
	}
	assertUnchanged(baselineEvidence, baselineState, baselineLeaseUntil)

	// Mismatched replay under the same observation_id: rejected.
	rejected, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-dup-evidence-req-3", "auth-observation-dup-evidence",
		bindingID, grant.GateOccurrenceID, "observation-dup-evidence-1", generation, 1, "auth_returned"))
	rejectedAck := claimObservationAckPayload(t, rejected)
	if rejectedAck.Outcome != "rejected" {
		t.Fatalf("mismatched replay = %+v, want rejected", rejectedAck)
	}
	assertUnchanged(baselineEvidence, baselineState, baselineLeaseUntil)

	// New observation_id whose ordinal does not exceed the highest applied
	// ordinal for this gate occurrence: stale.
	stale, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-dup-evidence-req-4", "auth-observation-dup-evidence",
		bindingID, grant.GateOccurrenceID, "observation-dup-evidence-stale", generation, 0, "auth_returned"))
	staleAck := claimObservationAckPayload(t, stale)
	if staleAck.Outcome != "stale" {
		t.Fatalf("superseded ordinal = %+v, want stale", staleAck)
	}
	assertUnchanged(baselineEvidence, baselineState, baselineLeaseUntil)
}

// TestClaimObservationEntitledLandingReoffersParkedSibling proves the
// end-to-end resumption path: entitled_landing on the owner's binding
// re-offers a sibling job parked on the SAME resolver profile through the
// existing legacy federated-login reoffer machinery
// (Bridge.reofferInstitutionalSiblings), exactly the mechanism
// TestAuthReturnedReoffersEligibleInstitutionalSiblingsOnce already pins
// for the frozen auth_returned message — entitled_landing wires the same
// resumption, not a new one.
func TestClaimObservationEntitledLandingReoffersParkedSibling(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	source := parkInstitutional(t, jobs, "wr_entitled_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_entitled_sibling", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-entitled")
	candidateID := explicitMaterializationCandidate(t, jobs, source, "domain-entitled")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, source,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-entitled-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, source, candidateID, "auth-observation-entitled", 6)

	runSync(t, b, claimObservationFrame(t, source, "obs-entitled-returned", "auth-observation-entitled",
		bindingID, grant.GateOccurrenceID, "observation-entitled-returned", b.epoch, 0, "auth_returned"))

	msgs, _ := runSync(t, b, claimObservationFrame(t, source, "obs-entitled-landing", "auth-observation-entitled",
		bindingID, grant.GateOccurrenceID, "observation-entitled-landing", b.epoch, 1, "entitled_landing"))
	ack := claimObservationAckPayload(t, msgs)
	if ack.Outcome != "applied" {
		t.Fatalf("entitled_landing outcome = %+v, want applied", ack)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != sibling {
		t.Fatalf("entitled_landing did not reoffer the parked sibling in the same reply: %v", msgs)
	}
	events, err := jobs.Events(ctx, sibling)
	if err != nil {
		t.Fatal(err)
	}
	reoffered := false
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			reoffered = true
		}
	}
	if !reoffered {
		t.Fatalf("sibling %s missing browser.handoff_reoffered event: %v", sibling, events)
	}
}

func TestClaimObservationOwnerClosedAbandonsClaimConsumesTokenAndLeavesDependentsTabless(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "wr_observation_closed", handoffWork(), "")
	dependentJob := parkInstitutional(t, jobs, "wr_observation_closed_dependent", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-closed")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-closed")
	dependentCandidateID := explicitMaterializationCandidate(t, jobs, dependentJob, "domain-closed-dependent")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-closed-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-closed", 8)

	claim, err := jobs.MaterializationClaimByBindingID(ctx, bindingID)
	if err != nil || claim == nil {
		t.Fatalf("materialization claim for binding: %v %v", claim, err)
	}
	closeID, _, err := jobs.IssueCloseAuthorization(ctx, bindingID, b.epoch, "scaffold_idle", b.now())
	if err != nil {
		t.Fatalf("issue close authorization: %v", err)
	}

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-closed-req", "auth-observation-closed",
		bindingID, grant.GateOccurrenceID, "observation-closed", b.epoch, 0, "owner_closed"))
	ack := claimObservationAckPayload(t, msgs)
	if ack.Outcome != "applied" {
		t.Fatalf("owner_closed outcome = %+v, want applied", ack)
	}

	afterClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || afterClaim == nil || afterClaim.Phase != "abandoned" {
		t.Fatalf("materialization claim after owner_closed = %+v, %v; want phase abandoned", afterClaim, err)
	}
	lease, found, err := jobs.GetAuthenticationEntryLease(ctx, "auth-observation-closed")
	if err != nil || !found || lease.OwnerBindingID != "" || lease.OwnerTabHint != nil {
		t.Fatalf("lease after owner_closed = %+v found=%v err=%v, want owner binding cleared", lease, found, err)
	}
	var tokenStatus string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT status FROM close_authorizations WHERE id=?`, closeID).Scan(&tokenStatus); err != nil {
		t.Fatal(err)
	}
	if tokenStatus != "consumed" {
		t.Fatalf("close authorization status = %q, want consumed", tokenStatus)
	}
	dependent, err := jobs.GetBrowserCandidate(ctx, dependentCandidateID)
	if err != nil || dependent == nil || dependent.Status != "eligible" {
		t.Fatalf("dependent candidate after owner_closed = %+v, %v; owner_closed must leave it tabless (still just eligible), never materializing it directly", dependent, err)
	}
}

func TestClaimObservationNavigationErrorParksWithoutMutatingLease(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_observation_naverror", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-observation-naverror")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-naverror")
	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-observation-naverror-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-observation-naverror", 9)

	before, foundBefore, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-naverror")
	if err != nil || !foundBefore {
		t.Fatalf("lease before navigation_error: %+v %v", before, err)
	}

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-naverror-req", "auth-observation-naverror",
		bindingID, grant.GateOccurrenceID, "observation-naverror", b.epoch, 0, "navigation_error"))
	ack := claimObservationAckPayload(t, msgs)
	if ack.Outcome != "applied" {
		t.Fatalf("navigation_error outcome = %+v, want applied", ack)
	}
	if ack.LeaseUntil != "" {
		t.Fatalf("navigation_error ack carried lease_until, forbidden: %+v", ack)
	}

	afterClaim, err := jobs.MaterializationClaimByBindingID(context.Background(), bindingID)
	if err != nil || afterClaim == nil || afterClaim.Phase != "abandoned" {
		t.Fatalf("materialization claim after navigation_error = %+v, %v; want abandoned", afterClaim, err)
	}
	closed, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "close-naverror-request", BindingID: bindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "claim_abandoned",
		GateOccurrenceID: grant.GateOccurrenceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if closeResp := decodeSurfaceCloseResponse(t, closed); closeResp.Outcome != "authorized" {
		t.Fatalf("navigation_error close outcome = %+v, want authorized", closeResp)
	}

	after, foundAfter, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-naverror")
	if err != nil || !foundAfter {
		t.Fatalf("lease after navigation_error: %+v %v", after, err)
	}
	if after.State != before.State || after.LeaseUntil != before.LeaseUntil || after.OwnerBindingID != before.OwnerBindingID {
		t.Fatalf("navigation_error mutated the entry lease: before=%+v after=%+v", before, after)
	}
}

// The following tests pin Slice 4 (dev/active/surface-lifecycle-plan.md):
// automatic (non-focus) materialization candidate offers, claim-paced by
// the authentication-entry lease this file already exercises. None of them
// ever call FocusHandoffs — the offers below are the daemon's own
// automatic admission (admitAutomaticMaterializationCandidates), not an
// explicit human gesture.

// TestAutomaticCandidateOfferForClaimGrantedInstitution proves an ordinary
// poll offers a candidate whose authentication claim is already granted to
// this exact job, with no focus request anywhere in the test.
func TestAutomaticCandidateOfferForClaimGrantedInstitution(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "auto-claim-granted", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auto-claim-granted-claim")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-auto-granted")
	// The institutional profile/candidate did not exist yet for the very
	// first poll above, so that poll necessarily fell back to a legacy
	// URL-bearing offer for this job. Item 5's double-drive guard now
	// correctly withholds automatic admission while that offer is still
	// live — a job migrates to the candidate path only after the legacy
	// offer is retired (cancel/timeout), never both at once. Simulate that
	// retirement directly (the daemon's own cancel/timeout paths are
	// exercised elsewhere) so this test can go on proving what it always
	// proved: an ordinary poll offers a candidate whose claim is already
	// granted, with no focus request.
	delete(b.offered, jobID)

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auto-claim-granted-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	if grant.Outcome != "open_new" {
		t.Fatalf("grant outcome = %s, want open_new", grant.Outcome)
	}
	offer := firstOfType(granted, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		// The grant reply itself may or may not carry the offer depending on
		// scheduler timing within the same poll; a following ordinary poll
		// must carry it regardless — no focus request is ever sent.
		var polled []*protocol.BrowserMessage
		polled, _ = runSync(t, b)
		offer = firstOfType(polled, protocol.MsgInstitutionalCandidateOffer)
	}
	if offer == nil || offer.JobID != jobID {
		t.Fatalf("claim-granted institution did not receive an automatic candidate offer")
	}
	if got := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID; got != candidateID {
		t.Fatalf("automatic offer candidate_id = %s, want %s", got, candidateID)
	}
}

// TestAutomaticCandidateOfferParksDependentUntilEntitledLanding proves a
// dependent sharing an unresolved authentication claim receives zero
// automatic candidate offers while the claim owner's sign-in is in
// progress, and is admitted only after the daemon observes
// entitled_landing — never via an explicit focus request.
func TestAutomaticCandidateOfferParksDependentUntilEntitledLanding(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	owner := parkInstitutional(t, jobs, "auto-claim-dep-owner", handoffWork(), "")
	dependent := parkInstitutional(t, jobs, "auto-claim-dep-dependent", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auto-claim-dep-shared")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, owner, "domain-dep-owner")
	dependentCandidate := explicitMaterializationCandidate(t, jobs, dependent, "domain-dep-dependent")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, owner,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auto-claim-dep-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	if grant.Outcome != "open_new" {
		t.Fatalf("owner grant outcome = %s, want open_new", grant.Outcome)
	}

	// The claim is reserved to owner but not yet landed: the dependent must
	// stay parked across several ordinary polls.
	for range 2 {
		polled, _ := runSync(t, b)
		if offer := firstOfType(polled, protocol.MsgInstitutionalCandidateOffer); offer != nil && offer.JobID == dependent {
			t.Fatalf("dependent of an unresolved claim received a candidate offer: %v", polled)
		}
	}

	bindingID := bindCandidate(t, b, owner, ownerCandidate, "auto-claim-dep", 4)
	runSync(t, b, claimObservationFrame(t, owner, "auto-claim-dep-obs-returned", "auto-claim-dep-shared",
		bindingID, grant.GateOccurrenceID, "auto-claim-dep-observation-returned", b.epoch, 0, "auth_returned"))
	landed, _ := runSync(t, b, claimObservationFrame(t, owner, "auto-claim-dep-obs-landing", "auto-claim-dep-shared",
		bindingID, grant.GateOccurrenceID, "auto-claim-dep-observation-landing", b.epoch, 1, "entitled_landing"))
	ack := claimObservationAckPayload(t, landed)
	if ack.Outcome != "applied" {
		t.Fatalf("entitled_landing outcome = %+v, want applied", ack)
	}

	resumed, _ := runSync(t, b)
	offer := firstOfType(resumed, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil || offer.JobID != dependent {
		t.Fatalf("dependent did not receive an automatic candidate offer after entitled_landing: %v", resumed)
	}
	if got := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID; got != dependentCandidate {
		t.Fatalf("resumed dependent offer candidate_id = %s, want %s", got, dependentCandidate)
	}
}

// TestAutomaticCandidateOfferSuppressedByLiveOwnerBinding proves the
// one-bound-scaffold-per-institution pacing: once the claim owner's lease
// carries a live owner_binding_id (a real bound scaffold), a sibling under
// the same claim receives no automatic candidate offer, even though its own
// candidate exists and is otherwise eligible.
func TestAutomaticCandidateOfferSuppressedByLiveOwnerBinding(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	owner := parkInstitutional(t, jobs, "auto-claim-bound-owner", handoffWork(), "")
	dependent := parkInstitutional(t, jobs, "auto-claim-bound-dependent", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auto-claim-bound-shared")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, owner, "domain-bound-owner")
	explicitMaterializationCandidate(t, jobs, dependent, "domain-bound-dependent")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, owner,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auto-claim-bound-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	authClaimResponse(t, granted)
	bindCandidate(t, b, owner, ownerCandidate, "auto-claim-bound", 3)

	lease, found, err := jobs.GetAuthenticationEntryLease(context.Background(), "auto-claim-bound-shared")
	if err != nil || !found || lease.OwnerBindingID == "" {
		t.Fatalf("owner binding was not recorded on the lease: %+v found=%v err=%v", lease, found, err)
	}

	polled, _ := runSync(t, b)
	if offer := firstOfType(polled, protocol.MsgInstitutionalCandidateOffer); offer != nil && offer.JobID == dependent {
		t.Fatalf("dependent received a candidate offer while the claim held a live owner binding: %v", polled)
	}
}

// TestAutomaticCandidateOfferWakeFloodPacesToOneClaimOwner is the wake-flood
// fence: four institutional jobs across four provider-safety domains share
// one still-unclaimed authentication claim, so the scheduler's fair,
// per-domain batch would otherwise hand all four to the offer loop in one
// poll. Claim pacing must reduce that to exactly one candidate offer. It
// also proves the sibling half-budget reservation
// (admitAutomaticMaterializationCandidates' automaticCap = maxOutstandingOffers/2):
// with a SECOND poll where four automatic-eligible candidates sit under
// four DISTINCT (unpaced) claims alongside two ordinary legacy handoffs,
// automatic admission must never spend more than half of the
// maxOutstandingOffers transport budget, leaving the legacy loop at least
// its reserved half.
func TestAutomaticCandidateOfferWakeFloodPacesToOneClaimOwner(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auto-claim-flood-shared")
	for i := range 4 {
		jobID := parkInstitutional(t, jobs, fmt.Sprintf("auto-claim-flood-%d", i), handoffWork(), "")
		explicitMaterializationCandidate(t, jobs, jobID, fmt.Sprintf("domain-flood-%d", i))
	}
	msgs, _ := runSync(t, b)
	if got := countType(msgs, protocol.MsgInstitutionalCandidateOffer); got != 1 {
		t.Fatalf("wake flood emitted %d candidate offers against one unresolved claim, want 1: %v", got, msgs)
	}

	mixed, mixedJobs, _, _ := newBridge(t)
	runSync(t, mixed, authClaimHello(t))
	specs := make([]job.InstitutionProfileSpec, 4)
	for i := range specs {
		claimID := fmt.Sprintf("auto-claim-flood-budget-%d", i)
		specs[i] = job.InstitutionProfileSpec{
			ConfiguredName:  fmt.Sprintf("auto-claim-flood-budget-institution-%d", i),
			AuthorityDigest: "digest-" + claimID, AuthenticationClaimID: claimID,
		}
	}
	// ReconcileInstitutionProfiles tombstones any active profile omitted from
	// its specs, so the four distinct (unpaced) profiles this scenario needs
	// must be reconciled together in one call, not one call per claim.
	profiles, err := mixedJobs.ReconcileInstitutionProfiles(context.Background(), specs)
	if err != nil || len(profiles) != 4 {
		t.Fatalf("reconcile institution profiles: %v (%d)", err, len(profiles))
	}
	for i, profile := range profiles {
		jobID := parkInstitutional(t, mixedJobs, fmt.Sprintf("auto-claim-flood-budget-%d", i), handoffWork(), "")
		attempt, err := mixedJobs.MaterializationAttemptRevision(context.Background(), jobID)
		if err != nil {
			t.Fatalf("materialization attempt: %v", err)
		}
		if _, err := mixedJobs.CreateBrowserCandidate(context.Background(), job.BrowserCandidateInput{
			JobID: jobID, JobAttemptRevision: attempt,
			InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
			RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
			PreRouteSafetyKey: fmt.Sprintf("pre-route-flood-budget-%d", i), SafetyDomainID: fmt.Sprintf("domain-flood-budget-%d", i),
			AdapterRevision: "test-adapter", EffectContractID: "test-effect", Status: "eligible",
		}); err != nil {
			t.Fatalf("create automatic candidate for profile %s: %v", profile.ConfiguredName, err)
		}
	}
	for i := range 2 {
		park(t, mixedJobs, fmt.Sprintf("auto-claim-flood-budget-legacy-%d", i), handoffWork())
	}
	mixedMsgs, _ := runSync(t, mixed)
	automaticOffers := countType(mixedMsgs, protocol.MsgInstitutionalCandidateOffer)
	legacyOffers := countType(mixedMsgs, protocol.MsgJobOffer)
	if automaticOffers > maxOutstandingOffers/2 {
		t.Fatalf("mixed poll admitted %d automatic candidate offers, want at most the half-budget cap %d", automaticOffers, maxOutstandingOffers/2)
	}
	if legacyOffers < 2 {
		t.Fatalf("mixed poll admitted %d legacy job offers, want at least 2 reserved by the half-budget cap", legacyOffers)
	}
	if automaticOffers+legacyOffers > maxOutstandingOffers {
		t.Fatalf("mixed poll admitted %d total offers, want at most the %d-slot transport budget", automaticOffers+legacyOffers, maxOutstandingOffers)
	}
}

// TestLegacySessionAutomaticPathStaysDarkWithoutMaterializationFeature pins
// the compatibility boundary: a session that never negotiated
// institutional_materialization_v1 gets exactly today's legacy behavior —
// the ordinary URL-bearing job offer, never a materialization candidate
// offer — regardless of any authentication-claim state.
func TestLegacySessionAutomaticPathStaysDarkWithoutMaterializationFeature(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "legacy-no-materialization", handoffWork(), "")
	initial, _ := runSync(t, b, hello())
	msgs, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) != nil || firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) != nil {
		t.Fatalf("legacy session received a materialization candidate offer: hello=%v poll=%v", initial, msgs)
	}
	offer := firstOfType(initial, protocol.MsgJobOffer)
	if offer == nil {
		offer = firstOfType(msgs, protocol.MsgJobOffer)
	}
	if offer == nil || offer.JobID != jobID {
		t.Fatalf("legacy session did not receive the ordinary job offer: hello=%v poll=%v", initial, msgs)
	}
}

// TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim
// covers the three checkpoints the claim-paced automatic admission switch
// (admitAutomaticMaterializationCandidates) must get right for its landed
// branch: auth_returned alone (lease state 'human') is NOT permission to
// resume dependents — only a fenced entitled_landing observation is, since
// state='human' alone only proves an IdP round trip happened, not that it
// reached entitled content. And owner_closed, even after entitled_landing
// already fired, fully retires the claim's current occupancy: dependents
// park again until a fresh arbitration grants a new reservation, never
// resuming on the stale entitlement alone.
func TestAutomaticCandidateOfferGatesOnEntitledLandingAndOwnerCloseRetiresClaim(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	owner := parkInstitutional(t, jobs, "auto-claim-gate-owner", handoffWork(), "")
	dependent := parkInstitutional(t, jobs, "auto-claim-gate-dependent", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auto-claim-gate-shared")
	ownerCandidate := explicitMaterializationCandidate(t, jobs, owner, "domain-gate-owner")
	dependentCandidate := explicitMaterializationCandidate(t, jobs, dependent, "domain-gate-dependent")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, owner,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auto-claim-gate-req", CandidateID: ownerCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	if grant.Outcome != "open_new" {
		t.Fatalf("owner grant outcome = %s, want open_new", grant.Outcome)
	}
	bindingID := bindCandidate(t, b, owner, ownerCandidate, "auto-claim-gate", 9)

	// Checkpoint 1: auth_returned lands the lease (state='human') but never
	// observed entitled_landing. The dependent must stay parked.
	authReturned, _ := runSync(t, b, claimObservationFrame(t, owner, "auto-claim-gate-obs-returned",
		"auto-claim-gate-shared", bindingID, grant.GateOccurrenceID,
		"auto-claim-gate-observation-returned", b.epoch, 0, "auth_returned"))
	if ack := claimObservationAckPayload(t, authReturned); ack.Outcome != "applied" {
		t.Fatalf("auth_returned outcome = %+v, want applied", ack)
	}
	for range 2 {
		polled, _ := runSync(t, b)
		if offer := firstOfType(polled, protocol.MsgInstitutionalCandidateOffer); offer != nil && offer.JobID == dependent {
			t.Fatalf("dependent received a candidate offer after auth_returned alone (before entitled_landing): %v", polled)
		}
	}

	// Checkpoint 2: entitled_landing durably marks the lease entitled. The
	// dependent is admitted on the next poll.
	landed, _ := runSync(t, b, claimObservationFrame(t, owner, "auto-claim-gate-obs-landing",
		"auto-claim-gate-shared", bindingID, grant.GateOccurrenceID,
		"auto-claim-gate-observation-landing", b.epoch, 1, "entitled_landing"))
	if ack := claimObservationAckPayload(t, landed); ack.Outcome != "applied" {
		t.Fatalf("entitled_landing outcome = %+v, want applied", ack)
	}
	resumed, _ := runSync(t, b)
	offer := firstOfType(resumed, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil || offer.JobID != dependent {
		t.Fatalf("dependent did not receive an automatic candidate offer after entitled_landing: %v", resumed)
	}
	if got := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID; got != dependentCandidate {
		t.Fatalf("resumed dependent offer candidate_id = %s, want %s", got, dependentCandidate)
	}

	// Checkpoint 3: owner_closed retires the claim's current occupancy even
	// though entitled_landing already fired. Any dependent still parked (or
	// re-parked by this retirement) must wait for a fresh arbitration
	// rather than resuming on the now-stale entitlement.
	closed, _ := runSync(t, b, claimObservationFrame(t, owner, "auto-claim-gate-obs-closed",
		"auto-claim-gate-shared", bindingID, grant.GateOccurrenceID,
		"auto-claim-gate-observation-closed", b.epoch, 2, "owner_closed"))
	if ack := claimObservationAckPayload(t, closed); ack.Outcome != "applied" {
		t.Fatalf("owner_closed outcome = %+v, want applied", ack)
	}
	for range 2 {
		polled, _ := runSync(t, b)
		if offer := firstOfType(polled, protocol.MsgInstitutionalCandidateOffer); offer != nil && offer.JobID == dependent {
			t.Fatalf("dependent received a candidate offer after owner_closed retired the claim: %v", polled)
		}
	}
	// A fresh arbitration (a new authentication_claim_request) must be able
	// to grant again — the retired lease is fully available, not merely
	// entitlement-cleared and permanently busy.
	reclaimed, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, dependent,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auto-claim-gate-req-2", CandidateID: dependentCandidate,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	reclaim := authClaimResponse(t, reclaimed)
	if reclaim.Outcome != "open_new" {
		t.Fatalf("post-owner_closed arbitration outcome = %s, want open_new", reclaim.Outcome)
	}
}

// rawOfType returns the first message of typ from an index-aligned
// (msgs, raw) pair returned by runSync/runSyncAs, alongside its exact
// on-wire JSON.
func rawOfType(msgs []*protocol.BrowserMessage, raw []json.RawMessage, typ string) (*protocol.BrowserMessage, string) {
	for i, m := range msgs {
		if m.Type == typ {
			return m, string(raw[i])
		}
	}
	return nil, ""
}

// TestClaimObservationCloseFramesCarryNoRawMaterial is the denylist
// regression for the oracle review's finding 6
// (dev/scratch/oracle/20260818T202529Z-lifecycle-endtoend/answer3.md,
// test gap 7's second half): it drives a job whose DOI, title, provider
// institution entity ID, and ProQuest account ID are all
// deliberately-recognisable secrets through the real
// candidate-offer -> authentication_claim -> claim_observation ->
// surface_close pipeline, then asserts that not one of those raw values
// appears on the wire in any authentication_claim_response,
// claim_observation_ack, or surface_close_response frame.
//
// institutional_candidate_offer is checked too, but as a POSITIVE
// control: dev/active/surface-lifecycle-plan.md's storage-tier scope note
// and dev/active/claim-observation-protocol.md's §2 scope note document
// that this frame (part of the pre-existing institutional_materialization_v1
// route/offer family, shipped in 0b716b3 before this effort) legitimately
// carries exactly this material, mirroring the long-standing job_offer
// contract (AGENTS.md) — the extension needs it to navigate and verify a
// route. If that ever stops being true the digest-only claim below is
// vacuous, so this test fails either way the boundary moves.
func TestClaimObservationCloseFramesCarryNoRawMaterial(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.ShibbolethEntityID = "https://idp.example-institute.edu/idp/shibboleth"
	cfg.Browser.ProquestAccountID = "679012345678"
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
	ctx := context.Background()
	runSync(t, b, authClaimHello(t))

	denylist := []string{
		handoffWork().DOI, handoffWork().Title,
		"idp.example-institute.edu", "679012345678",
	}

	// Positive control, on its own job: institutional_candidate_offer must
	// still carry every denylisted value, exactly as documented.
	offerJobID := parkInstitutional(t, jobs, "wr_denylist_offer", handoffWork(), "")
	explicitMaterializationCandidate(t, jobs, offerJobID, "domain-denylist-offer")
	if _, live, err := b.FocusHandoffs(ctx, []string{offerJobID}); err != nil || !live {
		t.Fatalf("focus handoff: live=%v err=%v", live, err)
	}
	offeredMsgs, offeredRaw := runSync(t, b)
	offerMsg, offerJSON := rawOfType(offeredMsgs, offeredRaw, protocol.MsgInstitutionalCandidateOffer)
	if offerMsg == nil {
		t.Fatalf("no institutional_candidate_offer emitted: %v", offeredMsgs)
	}
	for _, needle := range denylist {
		if !strings.Contains(offerJSON, needle) {
			t.Fatalf("candidate offer no longer carries documented route material %q (update the plan's scope note if intentional): %s", needle, offerJSON)
		}
	}
	// Settle the offer job so it stops being re-flushed as a fresh
	// candidate offer alongside every later Sync batch for this session.
	if err := jobs.Cancel(ctx, offerJobID, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)

	// The subject under test, on a fresh job: walk the full
	// authentication_claim -> claim_observation -> surface_close pipeline
	// and demand none of the denylisted values appear anywhere on the wire.
	jobID := parkInstitutional(t, jobs, "wr_denylist_claim", handoffWork(), "")
	seedAuthenticationClaimProfile(t, jobs, "auth-claim-denylist")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-denylist-claim")

	// Only these types are covered by the digest-only promise (the
	// scope notes added to both docs for finding 6); every batch below is
	// walked, but institutional_candidate_offer and any other
	// pre-existing materialization-family frame that a shared poll may
	// also flush alongside these is deliberately not asserted against —
	// that boundary is exercised separately above.
	claimObservationCloseFamily := map[string]bool{
		protocol.MsgAuthenticationClaimRequest:  true,
		protocol.MsgAuthenticationClaimResponse: true,
		protocol.MsgClaimObservation:            true,
		protocol.MsgClaimObservationAck:         true,
		protocol.MsgSurfaceCloseRequest:         true,
		protocol.MsgSurfaceCloseResponse:        true,
	}
	var allMsgs []*protocol.BrowserMessage
	var allRaw []json.RawMessage
	record := func(msgs []*protocol.BrowserMessage, raw []json.RawMessage) {
		for i, m := range msgs {
			if claimObservationCloseFamily[m.Type] {
				allMsgs = append(allMsgs, m)
				allRaw = append(allRaw, raw[i])
			}
		}
	}

	authMsgs, authRaw := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "denylist-auth-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	record(authMsgs, authRaw)
	authResp := authClaimResponse(t, authMsgs)
	if authResp.Outcome != "open_new" || authResp.GateOccurrenceID == "" {
		t.Fatalf("authentication_claim_response = %+v, want open_new with a gate_occurrence_id", authResp)
	}

	bindingID := bindCandidate(t, b, jobID, candidateID, "denylist", 41)

	obsMsgs, obsRaw := runSync(t, b, claimObservationFrame(t, jobID, "denylist-obs-req",
		"auth-claim-denylist", bindingID, authResp.GateOccurrenceID,
		"obs-denylist-0001", b.epoch, 0, "wall_observed"))
	record(obsMsgs, obsRaw)
	if ack := claimObservationAckPayload(t, obsMsgs); ack.Outcome != "applied" {
		t.Fatalf("claim_observation_ack outcome = %s, want applied: %+v", ack.Outcome, ack)
	}

	closeMsgs, closeRaw := runSync(t, b, inFrame(t, protocol.MsgSurfaceCloseRequest, "",
		protocol.SurfaceCloseRequestPayload{
			RequestID: "denylist-close-req", BindingID: bindingID,
			BrowserHolderGeneration: b.epoch, Disposition: "scaffold_idle",
		}))
	record(closeMsgs, closeRaw)
	if closeResp := decodeSurfaceCloseResponse(t, closeRaw); closeResp.Outcome != "authorized" {
		t.Fatalf("surface_close_response outcome = %s, want authorized: %+v", closeResp.Outcome, closeResp)
	}

	if len(allMsgs) == 0 {
		t.Fatalf("no claim/observation/close frames captured to check")
	}
	for i, msg := range allMsgs {
		raw := string(allRaw[i])
		for _, needle := range denylist {
			if strings.Contains(raw, needle) {
				t.Fatalf("%s frame leaked raw material %q: %s", msg.Type, needle, raw)
			}
		}
	}
}
