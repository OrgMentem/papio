// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

func publisherRetryAfterConsumedOA(t *testing.T) (*Bridge, *job.Store, string) {
	t.Helper()
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, providerDriveEpochV1Feature))
	id := park(t, jobs, "publisher-consumed-oa", handoffWork())
	effectPermitOffer(t, jobs, id, "consumed-oa", "oa:doi.org")
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "consumed-oa", Ordinal: 0, Strategy: "generic", Revision: "1"}
	if frames, err := b.providerDriveEpochStart(ctx, id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("OA start: %v", err)
	}
	if frames, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "consumed-oa", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "unknown",
	}); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("OA result: %v", err)
	}
	if err := b.outcome(ctx, id, "oa-identity-missing", &protocol.ProviderOutcomePayload{Outcome: "ui_changed", Detail: "article identity missing"}); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	if _, err := b.RetryPublisher(ctx, actions[0].ID, actions[0].Revision); err != nil {
		t.Fatal(err)
	}
	return b, jobs, id
}

func TestPublisherRetryConsumedOAEpochCandidateWire(t *testing.T) {
	b, jobs, id := publisherRetryAfterConsumedOA(t)
	ctx := context.Background()
	var fresh *protocol.InstitutionalCandidateOfferPayload
	for i := 0; i < 2; i++ {
		messages, _ := runSync(t, b)
		message := firstOfType(messages, protocol.MsgInstitutionalCandidateOffer)
		if message == nil || message.JobID != id || firstOfType(messages, protocol.MsgJobOffer) != nil {
			t.Fatalf("expected negotiated candidate offer only: %+v", messages)
		}
		p := message.Payload.(*protocol.InstitutionalCandidateOfferPayload)
		if p.DriveAttemptID == "" || p.DriveAttemptID == "consumed-oa" || p.DriveOrdinal == nil || *p.DriveOrdinal != 0 || p.DriveStrategy != "generic" || p.DriveRevision != "1" {
			t.Fatalf("candidate reused consumed OA authority: %+v", p)
		}
		if fresh != nil && p.DriveAttemptID != fresh.DriveAttemptID {
			t.Fatal("candidate refresh minted another epoch")
		}
		fresh = p
	}
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: fresh.DriveAttemptID, Ordinal: *fresh.DriveOrdinal, Strategy: fresh.DriveStrategy, Revision: fresh.DriveRevision}
	if frames, err := b.providerDriveEpochStart(ctx, id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("publisher start: %v", err)
	}
	permit, err := jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{JobID: id, Kind: job.GenericDrive, DriveAttemptID: fresh.DriveAttemptID, Ordinal: 0, Strategy: "generic", Revision: "1"})
	if err != nil || permit == nil || permit.SafetyDomainID != "publisher:doi.org" || permit.JobAttemptRevision != 2 {
		t.Fatalf("publisher permit=%+v err=%v", permit, err)
	}
	if frames, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{DriveAttemptID: fresh.DriveAttemptID, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "unknown"}); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("publisher result: %v", err)
	}
	// Consuming the publisher tuple is not another explicit retry. Neither a
	// candidate refresh nor an ordinary offer may silently mint its successor.
	messages, _ := runSync(t, b)
	reoffered := firstOfType(messages, protocol.MsgInstitutionalCandidateOffer)
	if reoffered == nil || reoffered.Payload.(*protocol.InstitutionalCandidateOfferPayload).DriveAttemptID != fresh.DriveAttemptID {
		t.Fatalf("consumed candidate changed epoch: %+v", messages)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b.offer(*row, openHandoffAction(t, jobs, id), config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Payload.(*protocol.JobOfferPayload).DriveAttemptID != fresh.DriveAttemptID {
		t.Fatal("ordinary reoffer minted an implicit successor")
	}
	if frames, err := b.providerDriveEpochStart(ctx, id, start); err != nil || permitOutcome(t, frames) != "duplicate" {
		t.Fatalf("consumed publisher start should be duplicate: %v", err)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	offered, superseded, oldResults, oldLatches := 0, 0, 0, 0
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		switch event["kind"] {
		case "browser.provider_drive_epoch_offered":
			offered++
		case "browser.provider_drive_epoch_superseded":
			superseded++
			if detail["safety_domain"] != "oa:doi.org" {
				t.Fatalf("superseded original epoch acquired successor domain: %+v", detail)
			}
		case "browser.provider_drive_epoch_result":
			if detail["drive_attempt_id"] == "consumed-oa" {
				oldResults++
			}
		case providerLatchEventKind:
			if detail["safety_domain"] == "oa:doi.org" {
				oldLatches++
			}
		}
	}
	if offered != 2 || superseded != 1 || oldResults != 1 || oldLatches != 1 {
		t.Fatalf("history: offered=%d superseded=%d oldResults=%d oldLatches=%d", offered, superseded, oldResults, oldLatches)
	}
	// Startup imports this exact history. A legitimate retry must not make
	// the daemon refuse its next restart or gain a legacy unresolved effect.
	if err := jobs.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatalf("restart after publisher retry: %v", err)
	}
	if blockers, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || blockers != 0 {
		t.Fatalf("legacy blockers after completed retry=%d err=%v", blockers, err)
	}
}

func TestPublisherRetryEpochOffersWaitForGlobalPermit(t *testing.T) {
	for _, status := range []job.EffectPermitStatus{job.Held, job.UnknownCompletion} {
		t.Run(string(status), func(t *testing.T) {
			b, jobs, id := publisherRetryAfterConsumedOA(t)
			ctx := context.Background()
			// Cache the scheduler descriptor before another job acquires the
			// global lane, as can happen between scheduling and offer service.
			page, err := jobs.ScheduleEligibleBrowserCandidates(ctx, 10, job.CandidateScheduleCursor{})
			if err != nil || len(page.Candidates) != 1 {
				t.Fatalf("schedule=%+v err=%v", page, err)
			}
			other := park(t, jobs, "publisher-occupier", handoffWork())
			effectPermitOffer(t, jobs, other, "occupier", "other:provider")
			if frames, err := b.providerDriveEpochStart(ctx, other, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "occupier", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
				t.Fatalf("occupier start: %v", err)
			}
			permit, err := jobs.LiveEffectPermit(ctx)
			if err != nil || permit == nil {
				t.Fatalf("permit=%+v err=%v", permit, err)
			}
			if status == job.UnknownCompletion {
				if _, err := jobs.ReconcileEffectPermit(ctx, job.EffectPermitObservation{PermitID: permit.ID, BrowserHolderGeneration: b.arbitration.generation()}); err != nil {
					t.Fatal(err)
				}
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			action := openHandoffAction(t, jobs, id)
			raw, err := b.serviceMaterializationCandidate(ctx, id, row, action, config.ModeDelegated, map[string]job.BrowserCandidateDescriptor{id: page.Candidates[0]}, nil)
			if err != nil || raw == nil {
				t.Fatalf("candidate=%s err=%v", raw, err)
			}
			candidate, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if p := candidate.Payload.(*protocol.InstitutionalCandidateOfferPayload); p.DriveAttemptID != "" || p.DriveOrdinal != nil {
				t.Fatalf("occupied candidate offered epoch: %+v", p)
			}
			raw, err = b.offer(*row, action, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if ordinary.Payload.(*protocol.JobOfferPayload).DriveAttemptID != "" {
				t.Fatal("occupied ordinary offer gained epoch")
			}
			if attempt, _, _ := b.latestProviderDriveEpoch(id); attempt != "consumed-oa" {
				t.Fatalf("occupied offer minted %q", attempt)
			}
			live, err := jobs.LiveEffectPermit(ctx)
			if err != nil || live == nil || live.ID != permit.ID || live.Status != status {
				t.Fatalf("occupier changed: %+v %v", live, err)
			}
		})
	}
}

// A generic drive result with no daemon successor ends the drive, and the
// provider outcome that follows it is what retires the navigated claim.
// Measured live 2026-09-23 (job_2c3c6f40ad69e1e4282a272d25, Nature Medicine):
// the extension settled `html` and then sent nothing, so the claim kept
// publisher:doi.org for its whole 30-minute lease and the two sibling papers
// queued behind that domain were never authorized. Silence past the grace
// must end the drive and release the domain; silence inside it must not.
func TestSilentGenericHTMLResultReleasesSafetyDomainForSibling(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	var offset time.Duration
	b.now = func() time.Time { return time.Now().Add(offset) }
	runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, providerDriveEpochV1Feature))
	const prefix = "silent-html"
	claim := seedSurfaceCloseClaim(t, b, jobs, prefix, "navigated")
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=? WHERE id=?`,
		time.Now().UTC().Add(30*time.Minute).Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	stranded, domain := candidate.JobID, candidate.SafetyDomainID
	siblingWork := handoffWork()
	siblingWork.DOI = "10.1002/example.44"
	sibling := parkInstitutional(t, jobs, "wr_"+prefix+"-sibling", siblingWork, "")
	explicitMaterializationCandidate(t, jobs, sibling, domain)

	effectPermitOffer(t, jobs, stranded, prefix, domain)
	tuple := protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: prefix, Ordinal: 0, Strategy: "generic", Revision: "1"}
	if frames, err := b.providerDriveEpochStart(ctx, stranded, &tuple); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start: %v", err)
	}
	if frames, err := b.providerDriveEpochResult(ctx, stranded, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: prefix, Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "html", Detail: "generic candidate returned HTML",
	}); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("html result: %v", err)
	}
	blockedBy := func() string {
		t.Helper()
		blocker, err := jobs.HandoffQueueBlocker(ctx, sibling, false)
		if err != nil {
			t.Fatal(err)
		}
		if blocker == nil {
			return ""
		}
		return blocker.JobID
	}

	offset = providerDriveOutcomeGrace - time.Second
	runSync(t, b)
	if got := blockedBy(); got != stranded {
		t.Fatalf("inside the grace the sibling is blocked by %q, want the live drive %q", got, stranded)
	}

	offset = providerDriveOutcomeGrace + time.Second
	runSync(t, b)
	if got := blockedBy(); got != "" {
		t.Fatalf("after a silent html result the sibling is still blocked by %q", got)
	}
	retired, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || retired == nil || retired.Phase != "abandoned" {
		t.Fatalf("stranded claim = %+v, %v; want abandoned", retired, err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{stranded})
	if err != nil || len(actions) != 1 || actions[0].Kind != "manual_download" {
		t.Fatalf("stranded job actions = %+v, %v; want one manual_download", actions, err)
	}
	// One ending only: a later poll must not record a second outcome.
	runSync(t, b)
	events, err := jobs.Events(ctx, stranded)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := 0
	for _, event := range events {
		if event["kind"] == "browser.provider_outcome" {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Fatalf("provider outcomes = %d, want 1", outcomes)
	}
}
