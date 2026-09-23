// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

func TestPublisherRetryOffersDOIWithoutReusingResolverAuthority(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := parkInstitutional(t, jobs, "publisher-retry", handoffWork(), "")
	if err := b.outcome(ctx, id, "wrong-work", &protocol.ProviderOutcomePayload{Outcome: "wrong_work", AdapterID: "proquest", AdapterVersion: "1.0.0", Detail: "different work"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		kind   string
		detail map[string]any
	}{
		{"browser.handoff_offered", map[string]any{"safety_domain": "institution:openurl.example.edu"}},
		{"browser.provider_drive_epoch_offered", map[string]any{"drive_attempt_id": "old-attempt", "ordinal": 0, "strategy": "generic", "revision": "1", "safety_domain": "institution:openurl.example.edu"}},
	} {
		if err := jobs.RecordEvent(ctx, id, event.kind, event.detail); err != nil {
			t.Fatal(err)
		}
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	runSync(t, b, materializationHello(t))
	if got, err := b.RetryPublisher(ctx, actions[0].ID, actions[0].Revision); err != nil || got != id {
		t.Fatalf("retry=%s err=%v", got, err)
	}
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("no publisher candidate: %+v", msgs)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	open, err := b.openHandoffForJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://doi.org/" + handoffWork().DOI
	if got := b.handoffURL(*row, *open); got != want {
		t.Fatalf("offer URL=%q", got)
	}
	if got, ok := app.ResolveHumanActionURL(*open, *row, b.cfg.InstitutionFor); !ok || got != want {
		t.Fatalf("route issuance URL=%q", got)
	}
	if got := actionSafetyDomain(b.cfg, *row, *open); got != "publisher:doi.org" {
		t.Fatalf("domain=%q", got)
	}
	candidate, err := jobs.CurrentBrowserCandidateForJob(ctx, id, 2)
	if err != nil || candidate == nil || candidate.SafetyDomainID != "publisher:doi.org" {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}
	if latched, err := b.browserOfferLatched(ctx, *row, *open); err != nil || latched {
		t.Fatalf("publisher wrongly latched=%v err=%v", latched, err)
	}
	// Exercise the generic drive offer too: it must carry a new epoch for
	// this route, while reconnecting reuses that new epoch exactly once.
	runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"))
	var attempt string
	for i := 0; i < 2; i++ {
		raw, err := b.offer(*row, *open, config.ModeDelegated)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		payload := msg.Payload.(*protocol.JobOfferPayload)
		if payload.OpenURL != want || payload.DriveAttemptID == "" || payload.DriveAttemptID == "old-attempt" || !payload.RequiresAuth {
			t.Fatalf("offer=%+v", payload)
		}
		if i == 0 {
			attempt = payload.DriveAttemptID
		} else if payload.DriveAttemptID != attempt {
			t.Fatal("reconnect minted another publisher attempt")
		}
	}
	if _, err := b.RetryPublisher(ctx, actions[0].ID, actions[0].Revision); err == nil {
		t.Fatal("replayed action retried")
	}
	if err := b.outcome(ctx, id, "publisher-wrong", &protocol.ProviderOutcomePayload{Outcome: "wrong_work"}); err != nil {
		t.Fatal(err)
	}
	failed, err := b.openHandoffForJob(ctx, id)
	if err != nil || failed.Kind != "manual_download" || b.handoffURL(*row, *failed) != want {
		t.Fatalf("publisher failure lost route: %+v %v", failed, err)
	}
	raw, err := b.offer(*row, *failed, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID != "" {
		t.Fatal("manual Open regained drive authority")
	}
}

func TestPublisherRetryReoffersOriginalInstitutionAfterAdapterUpgrade(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := parkInstitutional(t, jobs, "publisher-upgraded-original", handoffWork(), "")
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	original, err := b.openHandoffForJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	originalURL := b.handoffURL(*row, *original)
	originalDomain := actionSafetyDomain(b.cfg, *row, *original)
	hello := func(version string) {
		t.Helper()
		runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{
			"extension_version": version,
			"adapter_versions":  map[string]string{"proquest": version},
			"features":          []string{institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"},
		}))
	}
	hello("1.0.0")
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": "original-attempt", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": originalDomain,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.outcome(ctx, id, "wrong-original", &protocol.ProviderOutcomePayload{
		Outcome: "wrong_work", AdapterID: "proquest", AdapterVersion: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, providerLatchEventKind, map[string]any{
		"kind": "no_positive_effects", "safety_domain": originalDomain,
	}); err != nil {
		t.Fatal(err)
	}
	failed, err := b.openHandoffForJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.RetryPublisher(ctx, failed.ID, failed.Revision); err != nil {
		t.Fatal(err)
	}
	if err := b.outcome(ctx, id, "wrong-publisher", &protocol.ProviderOutcomePayload{Outcome: "wrong_work"}); err != nil {
		t.Fatal(err)
	}
	failed, err = b.openHandoffForJob(ctx, id)
	if err != nil || !job.IsPublisherHandoff(*failed) {
		t.Fatalf("failed publisher action=%+v err=%v", failed, err)
	}
	if _, err := b.RetryPublisher(ctx, failed.ID, failed.Revision); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("same-version retry=%v", err)
	}
	hello("1.0.1")
	if got, err := b.RetryPublisher(ctx, failed.ID, failed.Revision); err != nil || got != id {
		t.Fatalf("upgraded retry=%q err=%v", got, err)
	}
	reoffered, err := b.openHandoffForJob(ctx, id)
	if err != nil || reoffered.Kind != "openurl_handoff" || job.IsPublisherHandoff(*reoffered) {
		t.Fatalf("institutional handoff=%+v err=%v", reoffered, err)
	}
	if got := b.handoffURL(*row, *reoffered); got != originalURL {
		t.Fatalf("reoffered URL=%q, want %q", got, originalURL)
	}
	candidate, err := jobs.CurrentBrowserCandidateForJob(ctx, id, 3)
	if err != nil || candidate == nil || candidate.JobAttemptRevision != 3 || candidate.SafetyDomainID == "publisher:doi.org" {
		t.Fatalf("restored institutional candidate=%+v err=%v", candidate, err)
	}
	if latched, err := b.browserOfferLatched(ctx, *row, *reoffered); err != nil || latched {
		t.Fatalf("old institutional latch blocked new attempt=%v err=%v", latched, err)
	}
	raw, err := b.offer(*row, *reoffered, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	offer := msg.Payload.(*protocol.JobOfferPayload)
	if offer.OpenURL != originalURL || offer.DriveAttemptID == "" || offer.DriveAttemptID == "original-attempt" {
		t.Fatalf("institutional drive offer=%+v", offer)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	domain := ""
	for _, event := range events {
		if event["kind"] == "browser.provider_drive_epoch_offered" {
			detail, _ := event["detail"].(map[string]any)
			domain, _ = detail["safety_domain"].(string)
		}
	}
	if domain != originalDomain {
		t.Fatalf("restored drive domain=%q, want %q", domain, originalDomain)
	}
	if _, err := b.RetryPublisher(ctx, failed.ID, failed.Revision); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("replayed retry=%v", err)
	}
}

func TestPublisherRetryRequiresCurrentBrowserAndDelegatedMode(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "publisher-retry-refusal", handoffWork())
	if err := b.outcome(ctx, id, "wrong", &protocol.ProviderOutcomePayload{Outcome: "wrong_work"}); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	a := actions[0]
	if _, err := b.RetryPublisher(ctx, a.ID, a.Revision); err == nil {
		t.Fatal("retry without browser")
	}
	runSync(t, b, materializationHello(t))
	if _, err := jobs.S.DB().Exec(`UPDATE jobs SET policy_json=json_set(policy_json,'$.access_mode','conservative') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RetryPublisher(ctx, a.ID, a.Revision); err == nil {
		t.Fatal("retry in conservative mode")
	}
	open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].Kind != "manual_download" {
		t.Fatalf("refusal mutated action=%+v err=%v", open, err)
	}
}

// The old failure still prohibits its own route after a publisher retry.
func TestPublisherRetryDoesNotClearResolverLatch(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "publisher-preserve-latch", handoffWork())
	row, _ := jobs.Get(ctx, id)
	old, _ := b.openHandoffForJob(ctx, id)
	if err := jobs.RecordEvent(ctx, id, providerLatchEventKind, map[string]any{"kind": "no_positive_effects", "safety_domain": actionSafetyDomain(b.cfg, *row, *old)}); err != nil {
		t.Fatal(err)
	}
	if err := b.outcome(ctx, id, "wrong", &protocol.ProviderOutcomePayload{Outcome: "wrong_work"}); err != nil {
		t.Fatal(err)
	}
	actions, _ := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	runSync(t, b, materializationHello(t))
	if _, err := b.RetryPublisher(ctx, actions[0].ID, actions[0].Revision); err != nil {
		t.Fatal(err)
	}
	if latched, err := b.browserOfferLatched(ctx, *row, *old); err != nil || !latched {
		t.Fatalf("old route latch lost: %v %v", latched, err)
	}
}

func TestInstitutionalRouteFailureRetiresItsSignInOccupancy(t *testing.T) {
	for _, outcome := range []string{"wrong_work", "ui_changed"} {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			runSync(t, b, materializationHello(t))
			const prefix = "no-entitlement-retire"
			claim := seedSurfaceCloseClaim(t, b, jobs, prefix, "navigated")
			candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
			if err != nil || candidate == nil {
				t.Fatalf("candidate = %+v, %v", candidate, err)
			}
			const authClaimID = "auth-" + prefix
			if _, err := jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
				AuthenticationClaimID: authClaimID, LeaseID: "lease-" + prefix,
				OwnerID: candidate.JobID, BrowserHolderGeneration: b.arbitration.generation(),
				LeaseUntil: time.Now().UTC().Add(30 * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			if err := jobs.SetAuthenticationEntryLeaseOwnerBinding(
				ctx, authClaimID, candidate.JobID, b.arbitration.generation(), claim.BindingID, 99,
			); err != nil {
				t.Fatal(err)
			}

			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, candidate.JobID,
				map[string]any{"outcome": outcome}))

			retired, err := jobs.GetMaterializationClaim(ctx, claim.ID)
			if err != nil || retired == nil || retired.Phase != "abandoned" {
				t.Fatalf("retired claim = %+v, %v; want abandoned", retired, err)
			}
			lease, found, err := jobs.GetAuthenticationEntryLease(ctx, authClaimID)
			if err != nil || !found || lease == nil {
				t.Fatalf("retired lease = %+v, found=%v, err=%v", lease, found, err)
			}
			if lease.State != job.AuthenticationEntryLeaseExpired ||
				lease.OwnerBindingID != "" || lease.OwnerTabHint != nil {
				t.Fatalf("retired lease = %+v; want expired with no owner surface", lease)
			}

		})
	}
}

func TestPublisherRetryCannotRetireSecurityChallenge(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "publisher-challenge", "navigated")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, candidate.JobID, map[string]any{"outcome": "ui_changed", "detail": "provider security challenge"}))
	current, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || current.Phase != "navigated" {
		t.Fatalf("challenge lost ownership: %+v %v", current, err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{candidate.JobID})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	if _, err := b.RetryPublisher(ctx, actions[0].ID, actions[0].Revision); err == nil {
		t.Fatal("publisher retry bypassed challenge")
	}
}
