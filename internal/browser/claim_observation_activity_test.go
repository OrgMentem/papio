// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

// countJobEvents counts the durable Activity rows of one kind for one job.
func countJobEvents(t *testing.T, jobs *job.Store, jobID, kind string) int {
	t.Helper()
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range events {
		if event["kind"] == kind {
			n++
		}
	}
	return n
}

// A lost access surface reached no operator surface at all: the badge counts
// auth walls and required turns, the pulse skips gate members, and
// owner_closed's own effects touch claim, lease, and journal only. So papio
// released a paper the researcher had just watched disappear and said nothing.
// One durable Activity row closes that, and the popup's catch-up card plus
// `papio activity` already read it.
func TestClaimObservationOwnerClosedRecordsLostSurfaceActivity(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_surface_closed_activity", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-surface-closed-activity")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-surface-closed-activity")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-surface-closed-activity-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-surface-closed-activity", 11)

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-surface-closed-activity",
		"auth-surface-closed-activity", bindingID, grant.GateOccurrenceID,
		"observation-surface-closed-activity", b.epoch, 0, "owner_closed"))
	if ack := claimObservationAckPayload(t, msgs); ack.Outcome != "applied" {
		t.Fatalf("owner_closed outcome = %+v, want applied", ack)
	}

	if got := countJobEvents(t, jobs, jobID, "browser.surface_closed"); got != 1 {
		t.Fatalf("browser.surface_closed events = %d, want 1: a lost access surface must be legible", got)
	}
}

// The discriminator, and the reason the reducer reports whether it abandoned
// anything rather than just that owner_closed applied. A successful provider
// outcome retires the binding through RetireMaterializationBindingAfterOutcome
// BEFORE the tab physically closes, so the trailing owner_closed abandons
// nothing. Announcing a loss there would contradict the delivery the
// researcher is about to receive. A test asserting only "the event exists
// after owner_closed" passes under both behaviours and would not catch this.
func TestClaimObservationOwnerClosedAfterOutcomeRecordsNoLostSurface(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "wr_surface_settled_activity", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-surface-settled-activity")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-surface-settled-activity")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-surface-settled-activity-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-surface-settled-activity", 12)

	// The provider outcome path: this route is finished, so the binding and
	// its institution occupancy retire now.
	if err := jobs.RetireMaterializationBindingAfterOutcome(ctx, bindingID); err != nil {
		t.Fatalf("retire binding after outcome: %v", err)
	}

	msgs, _ := runSync(t, b, claimObservationFrame(t, jobID, "obs-surface-settled-activity",
		"auth-surface-settled-activity", bindingID, grant.GateOccurrenceID,
		"observation-surface-settled-activity", b.epoch, 0, "owner_closed"))
	if ack := claimObservationAckPayload(t, msgs); ack.Outcome != "applied" {
		t.Fatalf("owner_closed after an outcome = %+v, want applied: it stays idempotent", ack)
	}

	if got := countJobEvents(t, jobs, jobID, "browser.surface_closed"); got != 0 {
		t.Fatalf("browser.surface_closed events = %d, want 0: an already-retired claim lost nothing", got)
	}
}

// A replayed observation is a true no-op, so it must not report a second loss
// for the one surface that closed.
func TestClaimObservationOwnerClosedReplayRecordsOneLostSurface(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := parkInstitutional(t, jobs, "wr_surface_replay_activity", handoffWork(), "")
	runSync(t, b, authClaimHello(t))
	seedAuthenticationClaimProfile(t, jobs, "auth-surface-replay-activity")
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "domain-surface-replay-activity")

	granted, _ := runSync(t, b, inFrame(t, protocol.MsgAuthenticationClaimRequest, jobID,
		protocol.AuthenticationClaimRequestPayload{
			RequestID: "auth-surface-replay-activity-req", CandidateID: candidateID,
			MaterializationKind: "browser_tab", Trigger: "automatic",
		}))
	grant := authClaimResponse(t, granted)
	bindingID := bindCandidate(t, b, jobID, candidateID, "auth-surface-replay-activity", 13)

	frame := claimObservationFrame(t, jobID, "obs-surface-replay-activity",
		"auth-surface-replay-activity", bindingID, grant.GateOccurrenceID,
		"observation-surface-replay-activity", b.epoch, 0, "owner_closed")
	if ack := claimObservationAckPayload(t, mustSync(t, b, frame)); ack.Outcome != "applied" {
		t.Fatalf("first owner_closed = %+v, want applied", ack)
	}
	replay := claimObservationFrame(t, jobID, "obs-surface-replay-activity",
		"auth-surface-replay-activity", bindingID, grant.GateOccurrenceID,
		"observation-surface-replay-activity", b.epoch, 0, "owner_closed")
	if ack := claimObservationAckPayload(t, mustSync(t, b, replay)); ack.Outcome != "duplicate" {
		t.Fatalf("replayed owner_closed = %+v, want duplicate", ack)
	}

	if got := countJobEvents(t, jobs, jobID, "browser.surface_closed"); got != 1 {
		t.Fatalf("browser.surface_closed events = %d, want 1: one closed tab is one loss", got)
	}
}
