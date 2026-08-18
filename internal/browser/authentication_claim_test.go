// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Tests for the claim-observation protocol
// (dev/active/claim-observation-protocol.md §2.1/§2.2): the
// authentication_claim_request/response arbitration reducer and the
// claim_observation/claim_observation_ack idempotency/ordering reducer.

package browser

import (
	"context"
	"encoding/json"
	"testing"

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

	after, foundAfter, err := jobs.GetAuthenticationEntryLease(context.Background(), "auth-observation-naverror")
	if err != nil || !foundAfter {
		t.Fatalf("lease after navigation_error: %+v %v", after, err)
	}
	if after.State != before.State || after.LeaseUntil != before.LeaseUntil || after.OwnerBindingID != before.OwnerBindingID {
		t.Fatalf("navigation_error mutated the entry lease: before=%+v after=%+v", before, after)
	}
}
