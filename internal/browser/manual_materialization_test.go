// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

// A provider outcome can replace the handoff while an already-offered browser
// request is in transit. The remaining manual-download action must not authorize
// another automated attempt or reserve the institution's sign-in slot again.
func TestManualDownloadRefusesLateMaterialization(t *testing.T) {
	for _, stage := range []string{"claim", "bind", "route"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			b, jobs, _, _ := newBridge(t)
			id := parkInstitutional(t, jobs, "wr_manual_late_"+stage, handoffWork(), "")
			runSync(t, b, authClaimHello(t))
			seedAuthenticationClaimProfile(t, jobs, "auth-manual-late")
			candidateID := explicitMaterializationCandidate(t, jobs, id, "domain-manual-late")
			var claim *protocol.InstitutionalClaimResponsePayload
			if stage != "claim" {
				frames, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, id,
					protocol.InstitutionalClaimRequestPayload{RequestID: "initial-claim", CandidateID: candidateID, MaterializationKind: "browser_tab"}))
				claim = firstOfType(frames, protocol.MsgInstitutionalClaimResponse).Payload.(*protocol.InstitutionalClaimResponsePayload)
				if claim.Outcome != "claimed" {
					t.Fatalf("initial claim = %+v", claim)
				}
			}
			if stage == "route" {
				frames, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalBindRequest, id,
					protocol.InstitutionalBindRequestPayload{RequestID: "initial-bind", ClaimID: claim.ClaimID, BindingID: claim.BindingID, TabID: 71}))
				if got := firstOfType(frames, protocol.MsgInstitutionalBindResponse).Payload.(*protocol.InstitutionalBindResponsePayload); got.Outcome != "bound" {
					t.Fatalf("initial bind = %+v", got)
				}
			}
			actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
			if err != nil || len(actions) != 1 {
				t.Fatalf("initial actions = %+v, %v", actions, err)
			}
			if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "resolved"); err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.OpenHumanAction(ctx, id, manualDownloadActionKind, "download the requested PDF yourself", job.Access(true, "landing_page")); err != nil {
				t.Fatal(err)
			}
			var outcome string
			switch stage {
			case "claim":
				frames, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, id,
					protocol.InstitutionalClaimRequestPayload{RequestID: "late-claim", CandidateID: candidateID, MaterializationKind: "browser_tab"}))
				outcome = firstOfType(frames, protocol.MsgInstitutionalClaimResponse).Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome
			case "bind":
				frames, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalBindRequest, id,
					protocol.InstitutionalBindRequestPayload{RequestID: "late-bind", ClaimID: claim.ClaimID, BindingID: claim.BindingID, TabID: 71}))
				outcome = firstOfType(frames, protocol.MsgInstitutionalBindResponse).Payload.(*protocol.InstitutionalBindResponsePayload).Outcome
			case "route":
				frames, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalRouteRequest, id,
					protocol.InstitutionalRouteRequestPayload{RequestID: "late-route", ClaimID: claim.ClaimID, BindingID: claim.BindingID, InstitutionalRequestID: "request-late-route"}))
				outcome = firstOfType(frames, protocol.MsgInstitutionalRouteResponse).Payload.(*protocol.InstitutionalRouteResponsePayload).Outcome
			}
			if outcome != "not_eligible" {
				t.Fatalf("late %s = %s, want not_eligible", stage, outcome)
			}
			assertManualProviderPark(t, jobs, id)
		})
	}
}
