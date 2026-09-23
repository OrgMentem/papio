// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/job"
	"papio/internal/protocol"
)

// wileyPDFDirect is the shape Unpaywall/OpenAlex hand papio for a Wiley
// article they believe is free: the pdfdirect URL answers HTML to a browser
// without the institution's entitlement (measured 2026-09-23 on
// job_02d22c5578cbf5f5130c2b8ca9, three download_not_pdf in two minutes).
const wileyPDFDirect = "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/j.1469-7610.2010.02303.x"

func downloadNotPDF(t *testing.T, jobID string) []byte {
	t.Helper()
	return inFrame(t, protocol.MsgError, jobID, map[string]any{
		"code":    "download_not_pdf",
		"message": "provider returned HTML instead of a PDF; access could not be determined from this download",
	})
}

func openHandoffDetails(t *testing.T, jobs *job.Store, jobID string) []string {
	t.Helper()
	actions, err := jobs.ListOpenHumanActionsForJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, action := range actions {
		if action.Kind == handoffActionKind {
			details = append(details, action.Detail)
		}
	}
	return details
}

func countEvents(t *testing.T, jobs *job.Store, jobID, kind string) int {
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

// An open-access browser route that answers HTML has proved it cannot serve
// this browser. It earns the same one institutional fallback as an explicit
// no_entitlement; re-offering the same URL is the loop measured live.
func TestOAHandoffDownloadNotPDFFallsBackToInstitutionOnce(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_oa_not_pdf", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.OABrowserHandoffActionDetail(wileyPDFDirect), job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, hello())
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.Payload.(*protocol.JobOfferPayload).OpenURL != wileyPDFDirect {
		t.Fatalf("fixture must first offer the OA URL, got %+v", offer)
	}

	msgs, _ = runSync(t, b, downloadNotPDF(t, id))
	fallback := firstOfType(msgs, protocol.MsgJobOffer)
	if fallback == nil {
		t.Fatal("download_not_pdf on the OA route did not re-park with the institutional handoff")
	}
	if got := fallback.Payload.(*protocol.JobOfferPayload).OpenURL; got == wileyPDFDirect || !strings.HasPrefix(got, cfg.Browser.OpenURLBase+"?") {
		t.Fatalf("next route after download_not_pdf = %q, want institutional OpenURL", got)
	}
	if details := openHandoffDetails(t, jobs, id); len(details) != 1 || details[0] != app.InstitutionalOpenURLHandoffDetail {
		t.Fatalf("open handoff after download_not_pdf = %q, want the institutional handoff only", details)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after fallback = %+v %v, want awaiting_human", row, err)
	}

	// HTML from the institutional route is diagnostic only: it must neither
	// resurrect the OA URL nor fall back a second time.
	msgs, _ = runSync(t, b, downloadNotPDF(t, id))
	for _, msg := range msgs {
		if msg.Type == protocol.MsgJobOffer && msg.Payload.(*protocol.JobOfferPayload).OpenURL == wileyPDFDirect {
			t.Fatal("the OA URL that answered HTML was offered again")
		}
	}
	if details := openHandoffDetails(t, jobs, id); len(details) != 1 || details[0] != app.InstitutionalOpenURLHandoffDetail {
		t.Fatalf("open handoff after institutional HTML = %q, want institutional handoff unchanged", details)
	}
	if n := countEvents(t, jobs, id, "browser.oa_handoff_fallback"); n != 1 {
		t.Fatalf("oa_handoff_fallback events = %d, want exactly one", n)
	}
}

// The live loop ran through institutional materialization: every claim on the
// job issued the same pdfdirect URL and authorized a fresh effect for it. After
// one download_not_pdf the binding must retire, and the next claim must be
// issued the institutional route under the institution fence, not the OA URL
// again.
func TestMaterializedOARouteDownloadNotPDFIssuesInstitutionalRouteNext(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	id := parkInstitutional(t, jobs, "wr_oa_not_pdf_materialized", handoffWork(), "")
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.OABrowserHandoffActionDetail(wileyPDFDirect), job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	issue := func(requestID string, tabID int64) (*job.MaterializationClaim, *protocol.InstitutionalRouteResponsePayload, *job.BrowserCandidate) {
		t.Helper()
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := b.prepareMaterializationCandidate(ctx, *row)
		if err != nil || candidate == nil {
			t.Fatalf("candidate: %+v %v", candidate, err)
		}
		claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
			CandidateID: candidate.ID, BrowserHolderGeneration: b.arbitration.generation(),
			JobAttemptRevision: candidate.JobAttemptRevision, InstitutionProfileRevision: candidate.InstitutionProfileRevision,
			RouteRevision: candidate.RouteRevision, MaterializationKind: "browser_tab",
			LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		})
		if err != nil {
			t.Fatalf("claim %s: %v", requestID, err)
		}
		if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.arbitration.generation(), candidate.InstitutionProfileRevision, tabID); err != nil {
			t.Fatal(err)
		}
		frames, err := b.institutionalRoute(ctx, id, &protocol.InstitutionalRouteRequestPayload{
			RequestID: requestID, ClaimID: claim.ID, BindingID: claim.BindingID,
			InstitutionalRequestID: requestID + "-institutional",
		})
		if err != nil || len(frames) != 1 {
			t.Fatalf("route frames=%d err=%v", len(frames), err)
		}
		msg, err := protocol.DecodeBrowserMessage(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		route := msg.Payload.(*protocol.InstitutionalRouteResponsePayload)
		if route.Outcome != "issued" {
			t.Fatalf("route %s outcome=%q detail=%q, want issued", requestID, route.Outcome, route.Detail)
		}
		return claim, route, candidate
	}

	claim, route, candidate := issue("oa-route", 11)
	if route.URL != wileyPDFDirect || !strings.HasPrefix(candidate.SafetyDomainID, "oa:") {
		t.Fatalf("fixture must first issue the OA URL under the OA fence, got %q / %q", route.URL, candidate.SafetyDomainID)
	}
	if _, err := b.institutionalNavigated(ctx, id, &protocol.InstitutionalNavigatedRequestPayload{
		RequestID: "oa-navigated", ClaimID: claim.ID, BindingID: claim.BindingID,
		RouteIssuanceOrdinal: route.RouteIssuanceOrdinal, EffectOrdinal: route.EffectOrdinal,
		InstitutionalRequestID: route.InstitutionalRequestID, TabID: 11,
	}); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, downloadNotPDF(t, id))

	retired, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || retired == nil || retired.Phase != "abandoned" {
		t.Fatalf("OA claim after download_not_pdf = %+v %v, want abandoned", retired, err)
	}
	// The next claim - an operator open, a holder promotion, a re-offer - must
	// be issued the institutional route, never the URL that answered HTML.
	_, next, nextCandidate := issue("next-route", 12)
	if next.URL == wileyPDFDirect {
		t.Fatal("the next claim re-authorized the OA URL that answered HTML")
	}
	if !strings.HasPrefix(next.URL, cfg.Browser.OpenURLBase+"?") {
		t.Fatalf("next route = %q, want institutional OpenURL", next.URL)
	}
	if strings.HasPrefix(nextCandidate.SafetyDomainID, "oa:") {
		t.Fatalf("institutional route fenced as %q; it must serialize with the institution's siblings", nextCandidate.SafetyDomainID)
	}
}
