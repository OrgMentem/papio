// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"strings"
	"testing"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// parkPublisherFirst parks a job exactly as app.exhaustedCandidates does for a
// DOI whose packaged adapter is offered before the resolver.
func parkPublisherFirst(t *testing.T, jobs *job.Store, reqID string) (string, work.Work) {
	t.Helper()
	ctx := context.Background()
	w := work.Work{DOI: "10.1176/appi.ajp.2010.09111680", Title: "Personalized Medicine for Depression", Authors: []string{"Simon, Gregory E."}, Year: 2010}
	id := parkInstitutional(t, jobs, reqID, w, "")
	// An earlier pass of this job may already hold a resolver drive epoch.
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": "earlier-resolver-attempt", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:openurl.example.edu",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, app.PublisherFirstEventKind, map[string]any{"adapter_id": "psychiatryonline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, job.PublisherFirstHandoffDetail, job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id, w
}

// The DOI route is offered first, under its own safety domain and a fresh
// drive epoch; when it fails, the institutional resolver is the next offer in
// a new attempt, not a manual download and not a second DOI attempt.
func TestPublisherFirstFailureFallsBackToInstitutionalRoute(t *testing.T) {
	for _, outcome := range []protocol.ProviderOutcomePayload{
		{Outcome: "ui_changed", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1", Detail: "Article agent fallback stopped [identity_missing]"},
		{Outcome: "wrong_work", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1"},
		{Outcome: "no_entitlement", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1"},
	} {
		t.Run(outcome.Outcome, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			id, w := parkPublisherFirst(t, jobs, "publisher-first-"+outcome.Outcome)
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"))

			first, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			doiURL := "https://doi.org/" + w.DOI
			raw, err := b.offer(*row, *first, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			offer := msg.Payload.(*protocol.JobOfferPayload)
			if offer.OpenURL != doiURL || offer.DriveAttemptID == "" || offer.DriveAttemptID == "earlier-resolver-attempt" {
				t.Fatalf("first offer = %+v, want a fresh DOI drive at %s", offer, doiURL)
			}
			if got := actionSafetyDomain(b.cfg, *row, *first); got != "publisher:doi.org" {
				t.Fatalf("first offer domain = %q", got)
			}
			doiAttempt := offer.DriveAttemptID

			payload := outcome
			if err := b.outcome(ctx, id, "publisher-first-"+outcome.Outcome, &payload); err != nil {
				t.Fatal(err)
			}
			next, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if next.Kind != handoffActionKind || next.Detail != app.InstitutionalOpenURLHandoffDetail {
				t.Fatalf("after DOI %s the open action = %+v, want the institutional handoff", outcome.Outcome, next)
			}
			if got := b.handoffURL(*row, *next); !strings.HasPrefix(got, cfg.Browser.OpenURLBase+"?") {
				t.Fatalf("second offer URL = %q, want the resolver", got)
			}
			if attempt, err := jobs.MaterializationAttemptRevision(ctx, id); err != nil || attempt != 2 {
				t.Fatalf("attempt = %d err=%v, want the resolver in a new attempt", attempt, err)
			}
			if state, err := jobs.Get(ctx, id); err != nil || state.State != job.StateAwaitingHuman {
				t.Fatalf("state = %+v err=%v, want awaiting_human", state, err)
			}
			if latched, err := b.browserOfferLatched(ctx, *row, *next); err != nil || latched {
				t.Fatalf("DOI failure latched the resolver route=%v err=%v", latched, err)
			}
			if _, err := b.prepareMaterializationCandidate(ctx, *row); err != nil {
				t.Fatal(err)
			}
			candidate, err := jobs.CurrentBrowserCandidateForJob(ctx, id, 2)
			if err != nil || candidate == nil || candidate.SafetyDomainID == "publisher:doi.org" {
				t.Fatalf("resolver candidate=%+v err=%v", candidate, err)
			}
			raw, err = b.offer(*row, *next, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err = protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			second := msg.Payload.(*protocol.JobOfferPayload)
			if !strings.HasPrefix(second.OpenURL, cfg.Browser.OpenURLBase+"?") || second.DriveAttemptID == "" || second.DriveAttemptID == doiAttempt {
				t.Fatalf("resolver drive offer = %+v, want a fresh epoch", second)
			}

			// The resolver is the last candidate: its failure is an ordinary
			// manual download, never a return to the DOI.
			if outcome.Outcome == "no_entitlement" {
				return
			}
			if err := b.outcome(ctx, id, "resolver-"+outcome.Outcome, &payload); err != nil {
				t.Fatal(err)
			}
			last, err := b.openHandoffForJob(ctx, id)
			if err != nil || last.Kind != "manual_download" || job.IsPublisherHandoff(*last) {
				t.Fatalf("resolver failure action=%+v err=%v, want an institutional manual download", last, err)
			}
		})
	}
}
