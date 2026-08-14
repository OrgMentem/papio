package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
)

func permitJob(t *testing.T, js *Store, name string) string {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, name, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "openurl_handoff", "effect permit test handoff", Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id
}
func permitInstitutionalJob(t *testing.T, js *Store, prefix string) (string, string) {
	t.Helper()
	ctx := context.Background()
	jobID, candidateID := seedJobAndCandidate(t, js, prefix)
	if err := js.Transition(ctx, jobID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, jobID, "openurl_handoff", "effect permit institutional handoff", Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return jobID, candidateID
}

func driveIdentity(job, attempt string, ordinal int64, strategy string) EffectPermitIdentity {
	return EffectPermitIdentity{JobID: job, Kind: GenericDrive, DriveAttemptID: attempt, Ordinal: ordinal, Strategy: strategy, Revision: "r1"}
}
func acquireDrive(t *testing.T, js *Store, id EffectPermitIdentity, domain string, lease time.Time) *EffectPermit {
	t.Helper()
	p, outcome, err := js.AcquireEffectPermit(context.Background(), EffectPermitAcquireInput{
		Identity: id, JobAttemptRevision: 1, BrowserHolderGeneration: 1,
		SafetyDomainID: domain, LeaseUntil: lease,
		Authorization: EffectPermitEvent{Kind: "effect.authorized", Detail: map[string]any{"identity": id.DriveAttemptID}},
	})
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire outcome=%v permit=%+v err=%v", outcome, p, err)
	}
	return p
}

func TestEffectPermitOccupancyAndIdentity(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	firstJob := permitJob(t, js, "permit-occupancy-first")
	first := acquireDrive(t, js, driveIdentity(firstJob, "attempt-1", 1, "fallback"), "domain-a", time.Now().Add(time.Minute))
	if _, err := js.LiveEffectPermit(ctx); err != nil {
		t.Fatal(err)
	}
	secondJob := permitJob(t, js, "permit-occupancy-second")
	for _, domain := range []string{"domain-a", "domain-b"} {
		_, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
			Identity: driveIdentity(secondJob, "attempt-2-"+domain, 1, "fallback"), JobAttemptRevision: 1,
			BrowserHolderGeneration: 2, SafetyDomainID: domain, LeaseUntil: time.Now().Add(time.Minute),
			Authorization: EffectPermitEvent{Kind: "effect.authorized"},
		})
		if !errors.Is(err, ErrEffectPermitBusy) || outcome != EffectPermitBusyOutcome {
			t.Fatalf("occupied %s outcome=%v err=%v", domain, outcome, err)
		}
	}
	// Lease expiry is not authorization to release occupancy.
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE effect_permits SET lease_until=? WHERE id=?`, time.Now().Add(-time.Minute).Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}
	if live, err := js.LiveEffectPermit(ctx); err != nil || live == nil || live.ID != first.ID {
		t.Fatalf("expired permit live=%+v err=%v", live, err)
	}
	// Replaying the exact identity returns the same row even after its lease expires.
	replay, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{Identity: driveIdentity(firstJob, "attempt-1", 1, "fallback"), JobAttemptRevision: 1, BrowserHolderGeneration: 1, SafetyDomainID: "domain-a", LeaseUntil: time.Now(), Authorization: EffectPermitEvent{Kind: "effect.authorized"}})
	if err != nil || outcome != EffectPermitAcquired || replay.ID != first.ID {
		t.Fatalf("exact held replay permit=%+v outcome=%v err=%v", replay, outcome, err)
	}
	if _, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{Identity: driveIdentity(firstJob, "attempt-1", 1, "fallback"), JobAttemptRevision: 1, BrowserHolderGeneration: 9, SafetyDomainID: "domain-a", LeaseUntil: time.Now(), Authorization: EffectPermitEvent{Kind: "effect.authorized"}}); !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome {
		t.Fatalf("replacement holder replay outcome=%v err=%v, want stale", outcome, err)
	}
}

func TestEffectPermitCrossKindIdentityAndUnknownResolution(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	job := permitJob(t, js, "permit-cross-kind")
	id := driveIdentity(job, "shared-drive", 4, "fallback")
	p := acquireDrive(t, js, id, "domain-cross", time.Now().Add(time.Minute))
	if _, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{Identity: EffectPermitIdentity{JobID: job, Kind: DirectGet, DriveAttemptID: id.DriveAttemptID, Ordinal: id.Ordinal, Strategy: "direct_get", Revision: id.Revision}, JobAttemptRevision: 1, BrowserHolderGeneration: 1, SafetyDomainID: "domain-cross", LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "effect.authorized"}}); !errors.Is(err, ErrEffectPermitBusy) || outcome != EffectPermitBusyOutcome {
		t.Fatalf("cross-kind outcome=%v err=%v", outcome, err)
	}
	if _, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{PermitID: p.ID, BrowserHolderGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	unknown, err := js.GetEffectPermit(ctx, p.ID)
	if err != nil || unknown.Status != EffectPermitUnknownCompletion {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: id}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("late settle outcome=%v err=%v", outcome, err)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, p.ID, "late result path was unavailable"); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("resolved settled permit err=%v", err)
	}
}

func TestEffectPermitDuplicateSettlementPreservesFirstResult(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-first-result")
	identity := driveIdentity(jobID, "attempt-first-result", 0, "generic")
	acquireDrive(t, js, identity, "domain-first-result", time.Now().Add(time.Minute))
	first := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "article",
	}}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: identity, RequiredEvents: []EffectPermitEvent{first}}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("first settlement outcome=%q err=%v", outcome, err)
	}
	changed := first
	changed.Detail = map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "not_pdf",
	}
	conflictingSideEffect := EffectPermitEvent{Kind: "browser.provider_drive_epoch_offered", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal + 1,
		"strategy": identity.Strategy, "revision": identity.Revision,
	}}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity, RequiredEvents: []EffectPermitEvent{changed, conflictingSideEffect},
	}); err != nil || outcome != EffectPermitSettleDuplicate {
		t.Fatalf("duplicate settlement outcome=%q err=%v", outcome, err)
	}
	var count int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.provider_drive_epoch_result'`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("result events=%d, want first result only", count)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.provider_drive_epoch_offered'`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("conflicting duplicate appended %d result-side events", count)
	}
}

func TestEffectPermitOverrideKeepsLateResultCleanupOnly(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-override-late-result")
	identity := driveIdentity(jobID, "attempt-override-late", 0, "generic")
	permit := acquireDrive(t, js, identity, "domain-override-late", time.Now().Add(time.Minute))
	if _, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 1,
	}); err != nil {
		t.Fatalf("reconcile unknown completion: %v", err)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, permit.ID, "operator verified no browser effect remains"); err != nil {
		t.Fatalf("resolve unknown completion: %v", err)
	}
	result := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "not_pdf",
	}}
	successor := EffectPermitEvent{Kind: "browser.provider_drive_epoch_offered", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal + 1,
		"strategy": identity.Strategy, "revision": identity.Revision,
	}}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity, RequiredEvents: []EffectPermitEvent{result, successor},
	}); err != nil || outcome != EffectPermitSettleDuplicate {
		t.Fatalf("late result outcome=%q err=%v", outcome, err)
	}
	var count int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind IN ('browser.provider_drive_epoch_result','browser.provider_drive_epoch_offered')`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("operator-resolved permit applied %d late result events", count)
	}
}

func TestEffectPermitDuplicateSettlementScopesResultToExactIdentity(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-result-identity")
	firstID := driveIdentity(jobID, "attempt-result-identity", 0, "generic")
	acquireDrive(t, js, firstID, "domain-result-identity", time.Now().Add(time.Minute))
	first := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": firstID.DriveAttemptID, "ordinal": firstID.Ordinal,
		"strategy": firstID.Strategy, "revision": firstID.Revision, "outcome": "success",
	}}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: firstID, RequiredEvents: []EffectPermitEvent{first}}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("first settlement outcome=%q err=%v", outcome, err)
	}
	secondID := driveIdentity(jobID, "attempt-result-identity", 1, "generic")
	acquireDrive(t, js, secondID, "domain-result-identity", time.Now().Add(time.Minute))
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: secondID}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("second settlement outcome=%q err=%v", outcome, err)
	}
	second := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": secondID.DriveAttemptID, "ordinal": secondID.Ordinal,
		"strategy": secondID.Strategy, "revision": secondID.Revision, "outcome": "not_pdf",
	}}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: secondID, RequiredEvents: []EffectPermitEvent{second}}); err != nil || outcome != EffectPermitSettleDuplicate {
		t.Fatalf("duplicate second settlement outcome=%q err=%v", outcome, err)
	}
	var count int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.provider_drive_epoch_result'`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("result events=%d, want one per exact permit identity", count)
	}
}

func TestEffectPermitRetryFencesDoNotFreeOccupancy(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	job := permitJob(t, js, "permit-retry-fence")
	p := acquireDrive(t, js, driveIdentity(job, "attempt-old", 1, "fallback"), "domain-retry", time.Now().Add(time.Minute))
	if err := js.RecordEvent(ctx, job, "job.retry_requested", map[string]any{"reason": "ordinary_retry"}); err != nil {
		t.Fatal(err)
	}
	if live, err := js.LiveEffectPermit(ctx); err != nil || live == nil || live.ID != p.ID {
		t.Fatalf("retry freed permit=%+v err=%v", live, err)
	}
	if _, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{Identity: driveIdentity(job, "attempt-new", 1, "fallback"), JobAttemptRevision: 2, BrowserHolderGeneration: 2, SafetyDomainID: "domain-retry", LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "effect.authorized"}}); !errors.Is(err, ErrEffectPermitBusy) || outcome != EffectPermitBusyOutcome {
		t.Fatalf("retry occupancy outcome=%v err=%v", outcome, err)
	}
}

func TestEffectPermitExactReplayAfterRetryIsStale(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		identity EffectPermitIdentity
	}{
		{
			name: "generic",
			identity: EffectPermitIdentity{
				Kind: GenericDrive, DriveAttemptID: "replay-generic", Ordinal: 0,
				Strategy: "generic", Revision: "r1",
			},
		},
		{
			name: "direct",
			identity: EffectPermitIdentity{
				Kind: DirectGet, DriveAttemptID: "replay-direct", Ordinal: 0,
				Strategy: "direct_get", Revision: "r1",
			},
		},
		{
			name: "terms",
			identity: EffectPermitIdentity{
				Kind: Terms, TermsOccurrenceID: "replay-terms",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			jobID := permitJob(t, js, "permit-replay-after-retry-"+tc.name)
			identity := tc.identity
			identity.JobID = jobID
			input := EffectPermitAcquireInput{
				Identity: identity, JobAttemptRevision: 1,
				BrowserHolderGeneration: 1, SafetyDomainID: "domain-replay-" + tc.name,
				LeaseUntil:    time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "effect.authorized"},
			}
			first, outcome, err := js.AcquireEffectPermit(ctx, input)
			if err != nil || outcome != EffectPermitAcquired {
				t.Fatalf("first acquire permit=%+v outcome=%v err=%v", first, outcome, err)
			}
			if err := js.RecordEvent(ctx, jobID, "job.retry_requested", map[string]any{"reason": "replay regression"}); err != nil {
				t.Fatal(err)
			}
			replay, outcome, err := js.AcquireEffectPermit(ctx, input)
			if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || replay == nil || replay.ID != first.ID {
				t.Fatalf("stale replay permit=%+v outcome=%v err=%v", replay, outcome, err)
			}
			live, err := js.LiveEffectPermit(ctx)
			if err != nil || live == nil || live.ID != first.ID || live.Status != EffectPermitHeld {
				t.Fatalf("stale replay changed occupancy live=%+v err=%v", live, err)
			}
			var authorizations int
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='effect.authorized'`, jobID).Scan(&authorizations); err != nil {
				t.Fatal(err)
			}
			if authorizations != 1 {
				t.Fatalf("authorization events=%d, want exactly one", authorizations)
			}
		})
	}
}

func TestEffectPermitCancellationWinsAcquireAndReplay(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		kind EffectKind
	}{
		{name: "generic", kind: GenericDrive},
		{name: "direct", kind: DirectGet},
		{name: "terms", kind: Terms},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			jobID := permitJob(t, js, "permit-cancel-"+tc.name)
			identity := EffectPermitIdentity{JobID: jobID, Kind: tc.kind}
			switch tc.kind {
			case GenericDrive:
				identity.DriveAttemptID, identity.Ordinal, identity.Strategy, identity.Revision = "cancel-generic", 0, "generic", "r1"
			case DirectGet:
				identity.DriveAttemptID, identity.Ordinal, identity.Strategy, identity.Revision = "cancel-direct", 0, "direct_get", "r1"
			case Terms:
				identity.TermsOccurrenceID = "cancel-terms"
			}
			input := EffectPermitAcquireInput{
				Identity: identity, JobAttemptRevision: 1,
				BrowserHolderGeneration: 1, SafetyDomainID: "cancel-" + tc.name,
				LeaseUntil:    time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "effect.authorized"},
			}
			if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
				t.Fatal(err)
			}
			if permit, outcome, err := js.AcquireEffectPermit(ctx, input); !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
				t.Fatalf("cancelled acquire permit=%+v outcome=%v err=%v, want stale/no permit", permit, outcome, err)
			}
			var permits, authorizations int
			if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_permits WHERE job_id=?`, jobID).Scan(&permits); err != nil {
				t.Fatal(err)
			}
			if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='effect.authorized'`, jobID).Scan(&authorizations); err != nil {
				t.Fatal(err)
			}
			if permits != 0 || authorizations != 0 {
				t.Fatalf("cancelled acquire wrote permits=%d authorizations=%d", permits, authorizations)
			}

			js = testStore(t)
			jobID = permitJob(t, js, "permit-cancel-replay-"+tc.name)
			input.Identity.JobID = jobID
			first, outcome, err := js.AcquireEffectPermit(ctx, input)
			if err != nil || outcome != EffectPermitAcquired {
				t.Fatalf("first acquire permit=%+v outcome=%v err=%v", first, outcome, err)
			}
			if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
				t.Fatal(err)
			}
			replay, outcome, err := js.AcquireEffectPermit(ctx, input)
			if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || replay == nil || replay.ID != first.ID {
				t.Fatalf("cancelled replay permit=%+v outcome=%v err=%v, want stale/existing permit", replay, outcome, err)
			}
			if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='effect.authorized'`, jobID).Scan(&authorizations); err != nil {
				t.Fatal(err)
			}
			if authorizations != 1 {
				t.Fatalf("cancelled replay authorizations=%d, want 1", authorizations)
			}
		})
	}
}

func TestInstitutionalEffectPermitCancellationWinsAcquireAndReplay(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, js *Store, prefix string) (string, *MaterializationClaim, InstitutionalEffectPermitAcquireInput) {
		t.Helper()
		jobID, candidateID := permitInstitutionalJob(t, js, prefix)
		claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidateID, JobAttemptRevision: 1,
			InstitutionProfileRevision: 1, RouteRevision: 1,
			MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
			LeaseUntil: time.Now().Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
			t.Fatal(err)
		}
		return jobID, claim, InstitutionalEffectPermitAcquireInput{
			JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
			SafetyDomainID: "domain", InstitutionalRequestID: prefix + "-request",
			JobAttemptRevision: 1, BrowserHolderGeneration: 3,
			ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
			Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
		}
	}

	t.Run("initial acquire", func(t *testing.T) {
		js := testStore(t)
		jobID, claim, input := setup(t, js, "permit-cancel-institutional")
		if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
			t.Fatal(err)
		}
		if permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, input); !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
			t.Fatalf("cancelled institutional acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
		}
		var effectOrdinal, routeOrdinal, permits, authorizations int
		var phase string
		if err := js.S.DB().QueryRowContext(ctx, `SELECT effect_ordinal,route_issuance_ordinal,phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&effectOrdinal, &routeOrdinal, &phase); err != nil {
			t.Fatal(err)
		}
		if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_permits WHERE job_id=?`, jobID).Scan(&permits); err != nil {
			t.Fatal(err)
		}
		if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='institutional.authorized'`, jobID).Scan(&authorizations); err != nil {
			t.Fatal(err)
		}
		if effectOrdinal != 0 || routeOrdinal != 0 || phase != "bound" || permits != 0 || authorizations != 0 {
			t.Fatalf("cancelled institutional acquire mutated effect=%d route=%d phase=%q permits=%d authorizations=%d", effectOrdinal, routeOrdinal, phase, permits, authorizations)
		}
	})

	t.Run("held replay", func(t *testing.T) {
		js := testStore(t)
		jobID, claim, input := setup(t, js, "permit-cancel-institutional-replay")
		first, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, input)
		if err != nil || outcome != EffectPermitAcquired {
			t.Fatalf("first institutional acquire permit=%+v outcome=%v err=%v", first, outcome, err)
		}
		if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
			t.Fatal(err)
		}
		replay, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, input)
		if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || replay != nil {
			t.Fatalf("cancelled institutional replay permit=%+v outcome=%v err=%v", replay, outcome, err)
		}
		var effectOrdinal, routeOrdinal int
		var phase string
		if err := js.S.DB().QueryRowContext(ctx, `SELECT effect_ordinal,route_issuance_ordinal,phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&effectOrdinal, &routeOrdinal, &phase); err != nil {
			t.Fatal(err)
		}
		if effectOrdinal != 1 || routeOrdinal != 1 || phase != "route_issued" {
			t.Fatalf("cancelled institutional replay mutated effect=%d route=%d phase=%q", effectOrdinal, routeOrdinal, phase)
		}
	})
}

func TestEffectPermitInstitutionalRequestReplayCAS(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	job, candidate := permitInstitutionalJob(t, js, "permit-institutional")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate, JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab", BrowserHolderGeneration: 3, LeaseUntil: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	in := InstitutionalEffectPermitAcquireInput{JobID: job, ClaimID: claim.ID, BindingID: claim.BindingID, SafetyDomainID: "domain", InstitutionalRequestID: "request-1", JobAttemptRevision: 1, BrowserHolderGeneration: 3, ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "institutional.authorized"}}
	first, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in)
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("institutional first=%+v outcome=%v err=%v", first, outcome, err)
	}
	mismatched := in
	mismatched.ClaimID = "claim_other_0001"
	if permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, mismatched); !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
		t.Fatalf("cross-claim replay permit=%+v outcome=%v err=%v, want stale", permit, outcome, err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}
	second, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in)
	if err != nil || outcome != EffectPermitDuplicate || second.ID != first.ID {
		t.Fatalf("institutional replay=%+v outcome=%v err=%v", second, outcome, err)
	}
	var effectOrdinal, routeOrdinal, authorizationEvents int
	var phase string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT effect_ordinal,route_issuance_ordinal,phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&effectOrdinal, &routeOrdinal, &phase); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='institutional.authorized'`, job).Scan(&authorizationEvents); err != nil {
		t.Fatal(err)
	}
	if effectOrdinal != 1 || routeOrdinal != 1 || phase != "route_issued" || authorizationEvents != 1 {
		t.Fatalf("replay claim effect=%d route=%d phase=%q authorization_events=%d, want 1/1/route_issued/1", effectOrdinal, routeOrdinal, phase, authorizationEvents)
	}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: EffectPermitIdentity{JobID: job, Kind: Institutional, ClaimID: claim.ID, BindingID: claim.BindingID, EffectOrdinal: 1, InstitutionalRequestID: "request-1"}}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("settle outcome=%v err=%v", outcome, err)
	}
	third, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in)
	if err != nil || outcome != EffectPermitDuplicate || third.ID != first.ID {
		t.Fatalf("settled replay=%+v outcome=%v err=%v", third, outcome, err)
	}
}

func TestEffectPermitClosedHandoffIsStaleWhileJobRemainsNonterminal(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID := permitJob(t, js, "permit-closed-handoff")
	var actionID int64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT id FROM human_actions WHERE job_id=? AND kind='openurl_handoff' AND status='open'`,
		jobID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != StateAwaitingHuman {
		t.Fatalf("closed handoff changed job state to %q", state)
	}
	input := EffectPermitAcquireInput{
		Identity:           driveIdentity(jobID, "closed-handoff-attempt", 0, "generic"),
		JobAttemptRevision: 1, BrowserHolderGeneration: 1,
		SafetyDomainID: "closed-handoff-domain", LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "effect.authorized"},
	}
	if permit, outcome, err := js.AcquireEffectPermit(ctx, input); !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
		t.Fatalf("closed handoff acquire permit=%+v outcome=%v err=%v, want stale/no permit", permit, outcome, err)
	}
}

func TestInstitutionalEffectPermitAcquireAtomicallyFencesAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Store, string, string) error
	}{
		{
			name: "tombstoned profile",
			mutate: func(ctx context.Context, js *Store, _, profileID string) error {
				_, err := js.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET tombstoned_at=? WHERE id=?`, store.Now(), profileID)
				return err
			},
		},
		{
			name: "profile revision drift",
			mutate: func(ctx context.Context, js *Store, _, profileID string) error {
				_, err := js.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET revision=revision+1 WHERE id=?`, profileID)
				return err
			},
		},
		{
			name: "job retry",
			mutate: func(ctx context.Context, js *Store, jobID, _ string) error {
				return js.RecordEvent(ctx, jobID, "job.retry_requested", map[string]any{"reason": "authority fence test"})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			prefix := "permit-institutional-fence-" + strings.ReplaceAll(tc.name, " ", "-")
			jobID, candidateID := permitInstitutionalJob(t, js, prefix)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, JobAttemptRevision: 1,
				InstitutionProfileRevision: 1, RouteRevision: 1,
				MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
				LeaseUntil: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(ctx, js, jobID, prefix+"-profile"); err != nil {
				t.Fatal(err)
			}
			permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
				JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
				SafetyDomainID: "domain", InstitutionalRequestID: "request-" + prefix,
				JobAttemptRevision: 1, BrowserHolderGeneration: 3,
				ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
			})
			if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
				t.Fatalf("permit=%+v outcome=%v err=%v, want stale", permit, outcome, err)
			}
			var ordinal, routeOrdinal, authorizationEvents int
			var phase string
			if err := js.S.DB().QueryRowContext(ctx, `SELECT effect_ordinal,route_issuance_ordinal,phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&ordinal, &routeOrdinal, &phase); err != nil {
				t.Fatal(err)
			}
			if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='institutional.authorized'`, jobID).Scan(&authorizationEvents); err != nil {
				t.Fatal(err)
			}
			if ordinal != 0 || routeOrdinal != 0 || phase != "bound" || authorizationEvents != 0 {
				t.Fatalf("stale authority mutated claim effect=%d route=%d phase=%q events=%d", ordinal, routeOrdinal, phase, authorizationEvents)
			}
		})
	}
}

func TestInstitutionalEffectPermitReplayRechecksAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Store, string, string, string) error
	}{
		{
			name: "tombstoned profile",
			mutate: func(ctx context.Context, js *Store, _, profileID, _ string) error {
				_, err := js.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET tombstoned_at=? WHERE id=?`, store.Now(), profileID)
				return err
			},
		},
		{
			name: "profile revision drift",
			mutate: func(ctx context.Context, js *Store, _, profileID, _ string) error {
				_, err := js.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET revision=revision+1 WHERE id=?`, profileID)
				return err
			},
		},
		{
			name: "job retry",
			mutate: func(ctx context.Context, js *Store, jobID, _, _ string) error {
				return js.RecordEvent(ctx, jobID, "job.retry_requested", map[string]any{"reason": "replay fence test"})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			prefix := "permit-institutional-replay-" + strings.ReplaceAll(tc.name, " ", "-")
			jobID, candidateID := permitInstitutionalJob(t, js, prefix)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, JobAttemptRevision: 1,
				InstitutionProfileRevision: 1, RouteRevision: 1,
				MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
				LeaseUntil: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
				t.Fatal(err)
			}
			in := InstitutionalEffectPermitAcquireInput{
				JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
				SafetyDomainID: "domain", InstitutionalRequestID: "request-" + prefix,
				JobAttemptRevision: 1, BrowserHolderGeneration: 3,
				ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
			}
			if _, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in); err != nil || outcome != EffectPermitAcquired {
				t.Fatalf("first acquire outcome=%v err=%v", outcome, err)
			}
			if err := tc.mutate(ctx, js, jobID, prefix+"-profile", claim.ID); err != nil {
				t.Fatal(err)
			}
			permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in)
			if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
				t.Fatalf("replay permit=%+v outcome=%v err=%v, want stale", permit, outcome, err)
			}
		})
	}
}

func TestOccupyingInstitutionalPermitBlocksClaimRetirement(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Store, time.Time) (int, error)
	}{
		{
			name: "replacement holder",
			run: func(ctx context.Context, js *Store, _ time.Time) (int, error) {
				count, err := js.AbandonStaleMaterializations(ctx, 4)
				return int(count), err
			},
		},
		{
			name: "expired claim",
			run: func(ctx context.Context, js *Store, lease time.Time) (int, error) {
				expired, err := js.ReconcileMaterializationClaims(ctx, lease.Add(time.Second))
				return len(expired), err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID, candidateID := permitInstitutionalJob(t, js, "permit-claim-retirement-"+tc.name)
			lease := time.Now().UTC().Add(time.Minute)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, JobAttemptRevision: 1,
				InstitutionProfileRevision: 1, RouteRevision: 1,
				MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
				LeaseUntil: lease,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
				t.Fatal(err)
			}
			permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
				JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
				SafetyDomainID: "domain", InstitutionalRequestID: "request-" + tc.name,
				JobAttemptRevision: 1, BrowserHolderGeneration: 3,
				ExpectedEffectOrdinal: 0, LeaseUntil: lease,
				Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
			})
			if err != nil || outcome != EffectPermitAcquired {
				t.Fatalf("acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
			}
			if retired, err := tc.run(ctx, js, lease); err != nil || retired != 0 {
				t.Fatalf("retired=%d err=%v, want zero", retired, err)
			}
			current, err := js.GetMaterializationClaim(ctx, claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Phase != "route_issued" {
				t.Fatalf("occupying claim phase=%q, want route_issued", current.Phase)
			}
			candidate, err := js.GetBrowserCandidate(ctx, candidateID)
			if err != nil {
				t.Fatal(err)
			}
			if candidate == nil || candidate.Status != "claimed" {
				t.Fatalf("occupying candidate=%+v, want claimed", candidate)
			}
		})
	}
}

func TestSettledInstitutionalPermitKeepsExpiredClaimOwnedUntilWinner(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-settled-claim")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	requestID := "request-settled-claim"
	if _, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: "domain", InstitutionalRequestID: requestID,
		JobAttemptRevision: 1, BrowserHolderGeneration: 3,
		ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	}); err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire outcome=%v err=%v", outcome, err)
	}
	if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: EffectPermitIdentity{
			JobID: jobID, Kind: Institutional, ClaimID: claim.ID, BindingID: claim.BindingID,
			EffectOrdinal: 1, InstitutionalRequestID: requestID,
		},
		RequiredEvents: []EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": 1,
			"institutional_request_id": requestID,
		}}},
	}); err != nil || outcome != EffectPermitApplied {
		t.Fatalf("settle outcome=%v err=%v", outcome, err)
	}
	expiredAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=? WHERE id=?`, expiredAt, claim.ID); err != nil {
		t.Fatal(err)
	}
	if expired, err := js.ReconcileMaterializationClaims(ctx, time.Now()); err != nil || len(expired) != 0 {
		t.Fatalf("expired claims=%d err=%v, want settled effect claim retained", len(expired), err)
	}
	current, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || current == nil || current.Phase != "route_issued" {
		t.Fatalf("claim=%+v err=%v, want route_issued", current, err)
	}
	candidate, err := js.GetBrowserCandidate(ctx, candidateID)
	if err != nil || candidate == nil || candidate.Status != "claimed" {
		t.Fatalf("candidate=%+v err=%v, want claimed", candidate, err)
	}
	if _, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 4,
		LeaseUntil: time.Now().Add(time.Minute),
	}); !errors.Is(err, ErrMaterializationBusy) {
		t.Fatalf("replacement claim err=%v, want busy", err)
	}
}

func TestClaimMaterializationCannotReplaceExpiredPermitOwner(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-claim-replacement")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 3,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 7); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID:         "domain",
		InstitutionalRequestID: "request-claim-replacement",
		JobAttemptRevision:     1, BrowserHolderGeneration: 3,
		ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	}); err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire institutional outcome=%q err=%v", outcome, err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET lease_until=? WHERE id=?`,
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), claim.ID,
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 4,
		JobAttemptRevision: 1, InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute),
	})
	if !errors.Is(err, ErrMaterializationBusy) || replacement != nil {
		t.Fatalf("replacement claim=%+v err=%v, want busy", replacement, err)
	}
	current, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || current == nil || current.Phase != "route_issued" {
		t.Fatalf("current claim=%+v err=%v, want original route-issued owner", current, err)
	}
	candidate, err := js.GetBrowserCandidate(ctx, candidateID)
	if err != nil || candidate == nil || candidate.Status != "claimed" {
		t.Fatalf("candidate=%+v err=%v, want claimed", candidate, err)
	}
}

func TestEffectPermitPDFJoblessValidation(t *testing.T) {
	js := testStore(t)
	// PDF grab identity must be jobless; providing a job_id is rejected.
	if err := (EffectPermitIdentity{JobID: "job-any", Kind: PDFGrab, GrabID: "grab-1"}).validate(); err == nil || !strings.Contains(err.Error(), "jobless") {
		t.Fatalf("pdf grab with job_id should fail jobless check, got %v", err)
	}
	// PDF grab with empty grab id fails.
	if err := (EffectPermitIdentity{Kind: PDFGrab, GrabID: ""}).validate(); err == nil {
		t.Fatalf("pdf grab with empty grab_id should fail")
	}
	// Valid jobless PDF identity passes.
	if err := (EffectPermitIdentity{Kind: PDFGrab, GrabID: "grab-valid-001"}).validate(); err != nil {
		t.Fatalf("valid pdf grab identity: %v", err)
	}
	// Non-PDF kinds must have a job.
	for _, kind := range []EffectKind{GenericDrive, DirectGet, Terms, Institutional} {
		id := EffectPermitIdentity{Kind: kind, JobID: ""}
		switch kind {
		case GenericDrive:
			id.DriveAttemptID = "a"
			id.Ordinal = 0
			id.Strategy = "fallback"
			id.Revision = "r1"
		case DirectGet:
			id.DriveAttemptID = "a"
			id.Ordinal = 0
			id.Strategy = "direct_get"
			id.Revision = "r1"
		case Terms:
			id.TermsOccurrenceID = "terms-1"
		case Institutional:
			id.ClaimID = "c"
			id.BindingID = "b"
			id.EffectOrdinal = 1
			id.InstitutionalRequestID = "r1"
		}
		if err := id.validate(); err == nil {
			t.Fatalf("kind %s with empty job should fail", kind)
		}
	}
	// AcquireEffectPermit must reject PDF because allocation owns its transaction.
	_, out, err := js.AcquireEffectPermit(context.Background(), EffectPermitAcquireInput{
		Identity: EffectPermitIdentity{Kind: PDFGrab, GrabID: "grab-acquire-reject-001"}, JobAttemptRevision: 0, BrowserHolderGeneration: 1, SafetyDomainID: "domain", LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err == nil || out != EffectPermitStaleOutcome || !strings.Contains(err.Error(), "allocation") {
		t.Fatalf("pdf grab via AcquireEffectPermit should be rejected with allocation detail, got out=%v err=%v", out, err)
	}
	// Non-PDF kinds cannot use attempt 0.
	job := permitJob(t, js, "permit-pdf-attempt-fence")
	for _, kind := range []EffectKind{GenericDrive, DirectGet} {
		id := EffectPermitIdentity{JobID: job, Kind: kind, DriveAttemptID: "attempt-zero", Ordinal: 0, Strategy: func() string {
			if kind == DirectGet {
				return "direct_get"
			}
			return "fallback"
		}(), Revision: "r1"}
		_, out, err := js.AcquireEffectPermit(context.Background(), EffectPermitAcquireInput{Identity: id, JobAttemptRevision: 0, BrowserHolderGeneration: 1, SafetyDomainID: "domain", LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "effect.authorized"}})
		if err == nil || out != EffectPermitStaleOutcome {
			t.Fatalf("kind %s with attempt 0 should be stale, got out=%v err=%v", kind, out, err)
		}
	}
	// Direct SQL check: jobless PDF row persists; attempt-0 non-PDF is blocked by CHECK.
	if _, err := js.S.DB().ExecContext(context.Background(), `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,grab_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'held', ?, ?, ?)`, "permit-pdf-jobless-001", nil, 0, 1, "d", string(PDFGrab), "grab-jobless-001", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err != nil {
		t.Fatalf("jobless pdf insert: %v", err)
	}
	if _, err := js.S.DB().ExecContext(context.Background(), `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,drive_attempt_id,ordinal,strategy,revision,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,0,'fallback','r1','held',?,?,?)`, "permit-bad-attempt0", job, 0, 1, "d", string(GenericDrive), "attempt-x", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err == nil {
		t.Fatalf("generic drive with attempt 0 should be rejected by CHECK")
	}
	if _, err := js.S.DB().ExecContext(context.Background(), `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,grab_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'held', ?, ?, ?)`, "permit-bad-jobless-other", nil, 0, 1, "d2", string(GenericDrive), "grab-x", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err == nil {
		t.Fatalf("non-pdf with NULL job should be rejected by CHECK")
	}
}

func TestAcquireTermsEffectPermitLeavesDriveOrdinalNull(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-terms-null-ordinal")
	permit, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
		Identity: EffectPermitIdentity{
			JobID: jobID, Kind: EffectKindTerms,
			TermsOccurrenceID: "terms-occurrence-null-ordinal",
		},
		JobAttemptRevision: 1, BrowserHolderGeneration: 1,
		SafetyDomainID: "domain-terms", LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired || permit == nil {
		t.Fatalf("acquire terms permit=%+v outcome=%q err=%v", permit, outcome, err)
	}
	var ordinal any
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT ordinal FROM effect_permits WHERE id=?`, permit.ID,
	).Scan(&ordinal); err != nil {
		t.Fatal(err)
	}
	if ordinal != nil {
		t.Fatalf("terms permit drive ordinal=%v, want NULL", ordinal)
	}
}

func TestEffectPermitPDFExactSettleAndUnknownOverride(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	// Insert a jobless PDF permit directly to exercise settle/reconcile/override.
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,grab_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'held', ?, ?, ?)`, "permit-pdf-override-001", nil, 0, 7, "pdf.example", string(PDFGrab), "grab-override-001", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	pdfID := EffectPermitIdentity{Kind: PDFGrab, GrabID: "grab-override-001"}
	// Exact settle with no RequiredEvents must succeed for PDF (no job event).
	if _, out, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: pdfID}); err != nil || out != EffectPermitApplied {
		t.Fatalf("pdf settle applied out=%v err=%v", out, err)
	}
	// Second settle is duplicate.
	if _, out, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: pdfID}); err != nil || out != EffectPermitSettleDuplicate {
		t.Fatalf("pdf settle duplicate out=%v err=%v", out, err)
	}
	// Unknown override: held -> unknown -> override settles exact id.
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,grab_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'held', ?, ?, ?)`, "permit-pdf-override-002", nil, 0, 7, "pdf.example", string(PDFGrab), "grab-override-002", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{PermitID: "permit-pdf-override-002", BrowserHolderGeneration: 7}); err != nil {
		t.Fatalf("reconcile to unknown: %v", err)
	}
	if p, _ := js.GetEffectPermit(ctx, "permit-pdf-override-002"); p == nil || p.Status != EffectPermitUnknownCompletion {
		t.Fatalf("expected unknown_completion, got %+v", p)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, "permit-pdf-override-002", "operator override for test"); err != nil {
		t.Fatalf("resolve unknown: %v", err)
	}
	if p, _ := js.GetEffectPermit(ctx, "permit-pdf-override-002"); p == nil || p.Status != EffectPermitSettled {
		t.Fatalf("expected settled after override, got %+v", p)
	}
	// Override on held (not unknown) is stale.
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,grab_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'held', ?, ?, ?)`, "permit-pdf-override-003", nil, 0, 7, "pdf.example", string(PDFGrab), "grab-override-003", time.Now().Add(time.Minute).Format(time.RFC3339Nano), store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, "permit-pdf-override-003", "should be stale"); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("held override should be stale, got %v", err)
	}
}
func TestEffectPermitReplacementHolderReconcileUsesStoredGeneration(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-replacement-reconcile")
	identity := driveIdentity(jobID, "attempt-replacement-reconcile", 0, "generic")
	permit := acquireDrive(t, js, identity, "domain-replacement-reconcile", time.Now().Add(time.Minute))

	// The replacement holder's generation is deliberately different from
	// the generation that created the permit. Correlation belongs to the
	// current request, but classification is historical.
	got, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 44,
	})
	if err != nil || got == nil || got.Status != EffectPermitUnknownCompletion {
		t.Fatalf("replacement all-false reconcile got=%+v err=%v, want unknown_completion", got, err)
	}
	got, err = js.ReconcileEffectPermit(ctx, EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 44, SettledProof: true,
	})
	if err != nil || got == nil || got.Status != EffectPermitSettled {
		t.Fatalf("replacement settled reconcile got=%+v err=%v, want settled", got, err)
	}
}

func TestEffectPermitSettlementSuppressesCurrentProjectionAfterRetry(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-settlement-retry-race")
	identity := driveIdentity(jobID, "attempt-settlement-retry-race", 0, "generic")
	acquireDrive(t, js, identity, "domain-settlement-retry-race", time.Now().Add(time.Minute))
	if err := js.RecordEvent(ctx, jobID, "job.retry_requested", map[string]any{"reason": "settlement race"}); err != nil {
		t.Fatal(err)
	}
	result := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "cancelled",
	}}
	current := EffectPermitEvent{Kind: "browser.provider_drive_epoch_latch", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision,
	}}
	permit, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity, RequiredEvents: []EffectPermitEvent{result},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 0,
		CurrentEvents: []EffectPermitEvent{current},
	})
	if err != nil || outcome != EffectPermitApplied || permit == nil || permit.CurrentAtSettlement {
		t.Fatalf("settlement after retry permit=%+v outcome=%v err=%v, want applied historical", permit, outcome, err)
	}
	var resultCount, currentCount int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, result.Kind).Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, current.Kind).Scan(&currentCount); err != nil {
		t.Fatal(err)
	}
	if resultCount != 1 || currentCount != 0 {
		t.Fatalf("post-retry events result=%d current=%d, want 1/0", resultCount, currentCount)
	}
}

func TestEffectPermitSettlementSuppressesCurrentProjectionAfterCancellation(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-settlement-cancel-race")
	identity := driveIdentity(jobID, "attempt-settlement-cancel-race", 0, "generic")
	acquireDrive(t, js, identity, "domain-settlement-cancel-race", time.Now().Add(time.Minute))
	if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	result := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "cancelled",
	}}
	currentEvents := []EffectPermitEvent{
		{Kind: "job.latch", Detail: map[string]any{"safety_domain": "domain-settlement-cancel-race"}},
		{Kind: "browser.provider_drive_epoch_offered", Detail: map[string]any{
			"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal + 1,
			"strategy": identity.Strategy, "revision": identity.Revision,
		}},
		{Kind: "browser.handoff_reoffered", Detail: map[string]any{"reason": "cancelled regression"}},
	}
	permit, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity, RequiredEvents: []EffectPermitEvent{result},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 1,
		CurrentEvents: currentEvents,
	})
	if err != nil || outcome != EffectPermitApplied || permit == nil || permit.CurrentAtSettlement {
		t.Fatalf("settlement after cancellation permit=%+v outcome=%v err=%v, want applied historical only", permit, outcome, err)
	}
	var historicalCount int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, result.Kind).Scan(&historicalCount); err != nil {
		t.Fatal(err)
	}
	if historicalCount != 1 {
		t.Fatalf("historical result events=%d, want one", historicalCount)
	}
	for _, event := range currentEvents {
		var count int
		if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, event.Kind).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("cancelled settlement wrote current-only %s events=%d", event.Kind, count)
		}
	}
}

func TestEffectPermitSettlementSuppressesCurrentProjectionAfterHandoffClosure(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "permit-settlement-closed-handoff")
	identity := driveIdentity(jobID, "attempt-settlement-closed-handoff", 0, "generic")
	acquireDrive(t, js, identity, "domain-settlement-closed-handoff", time.Now().Add(time.Minute))
	var actionID int64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT id FROM human_actions WHERE job_id=? AND kind='openurl_handoff' AND status='open'`,
		jobID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); err != nil {
		t.Fatal(err)
	}
	result := EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision, "outcome": "cancelled",
	}}
	current := EffectPermitEvent{Kind: "browser.provider_drive_epoch_latch", Detail: map[string]any{
		"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal,
		"strategy": identity.Strategy, "revision": identity.Revision,
	}}
	permit, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity, RequiredEvents: []EffectPermitEvent{result},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 1,
		CurrentEvents: []EffectPermitEvent{current},
	})
	if err != nil || outcome != EffectPermitApplied || permit == nil || permit.CurrentAtSettlement {
		t.Fatalf("settlement after handoff closure permit=%+v outcome=%v err=%v, want applied historical", permit, outcome, err)
	}
	var resultCount, currentCount int
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, result.Kind).Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, jobID, current.Kind).Scan(&currentCount); err != nil {
		t.Fatal(err)
	}
	if resultCount != 1 || currentCount != 0 {
		t.Fatalf("closed-handoff events result=%d current=%d, want 1/0", resultCount, currentCount)
	}
}

func TestInstitutionalSettlementSuppressesNavigationAfterHandoffClosure(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-institutional-closed-handoff")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	const requestID = "request-institutional-closed-handoff"
	permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID:         "domain",
		InstitutionalRequestID: requestID, JobAttemptRevision: 1,
		BrowserHolderGeneration: 3, ExpectedEffectOrdinal: 0,
		LeaseUntil:    time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
	}
	var actionID int64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT id FROM human_actions WHERE job_id=? AND kind='openurl_handoff' AND status='open'`,
		jobID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); err != nil {
		t.Fatal(err)
	}
	settled, settleOutcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: EffectPermitIdentity{
			JobID: jobID, Kind: Institutional, ClaimID: claim.ID, BindingID: claim.BindingID,
			EffectOrdinal: 1, InstitutionalRequestID: requestID,
		},
		RequiredEvents: []EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": int64(1),
			"institutional_request_id": requestID, "outcome": "acknowledged",
		}}},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 3,
		Navigation: &EffectPermitNavigationFence{
			ClaimID: claim.ID, BindingID: claim.BindingID, RouteIssuanceOrdinal: 1, TabID: 9,
		},
	})
	if err != nil || settleOutcome != EffectPermitApplied || settled == nil || settled.CurrentAtSettlement {
		t.Fatalf("settlement after handoff closure permit=%+v outcome=%v err=%v, want historical applied", settled, settleOutcome, err)
	}
	gotClaim, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotClaim.Phase != "route_issued" || gotClaim.TabID != 9 {
		t.Fatalf("closed-handoff settlement mutated claim=%+v", gotClaim)
	}
}

func TestEffectPermitOperatorOverrideSuppressesLateInstitutionalNavigation(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-institutional-override")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID:         "domain",
		InstitutionalRequestID: "request-institutional-override",
		JobAttemptRevision:     1, BrowserHolderGeneration: 3,
		ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
	}
	if _, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, permit.ID, "operator verified no navigation"); err != nil {
		t.Fatal(err)
	}
	before, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity := EffectPermitIdentity{
		JobID: jobID, Kind: Institutional, ClaimID: claim.ID,
		BindingID: claim.BindingID, EffectOrdinal: 1,
		InstitutionalRequestID: "request-institutional-override",
	}
	settled, settleOutcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: identity,
		RequiredEvents: []EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": int64(1),
			"institutional_request_id": "request-institutional-override", "outcome": "acknowledged",
		}}},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 3,
		Navigation: &EffectPermitNavigationFence{
			ClaimID: claim.ID, BindingID: claim.BindingID,
			RouteIssuanceOrdinal: 1, TabID: 9,
		},
	})
	if err != nil || settleOutcome != EffectPermitSettleDuplicate || settled == nil || !settled.OperatorOverridden {
		t.Fatalf("override settle permit=%+v outcome=%v err=%v, want operator-overridden duplicate", settled, settleOutcome, err)
	}
	after, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Phase != before.Phase || after.RouteIssuanceOrdinal != before.RouteIssuanceOrdinal || after.TabID != before.TabID {
		t.Fatalf("late navigation mutated claim before=%+v after=%+v", before, after)
	}
}

func TestInstitutionalSettlementRevalidatesCurrentSafetyDomain(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-institutional-domain-fence")
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET safety_domain_id=? WHERE id=?`,
		"domain-original", candidateID); err != nil {
		t.Fatal(err)
	}
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: "domain-original", InstitutionalRequestID: "request-domain-fence",
		JobAttemptRevision: 1, BrowserHolderGeneration: 3, ExpectedEffectOrdinal: 0,
		LeaseUntil:    time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET safety_domain_id=? WHERE id=?`,
		"domain-replaced", candidateID); err != nil {
		t.Fatal(err)
	}
	settled, settleOutcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: EffectPermitIdentity{
			JobID: jobID, Kind: Institutional, ClaimID: claim.ID,
			BindingID: claim.BindingID, EffectOrdinal: 1,
			InstitutionalRequestID: "request-domain-fence",
		},
		RequiredEvents: []EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": int64(1),
			"institutional_request_id": "request-domain-fence", "outcome": "acknowledged",
		}}},
		CurrentAttemptRevision: 1, CurrentBrowserHolderGeneration: 3,
		Navigation: &EffectPermitNavigationFence{
			ClaimID: claim.ID, BindingID: claim.BindingID, RouteIssuanceOrdinal: 1, TabID: 9,
		},
	})
	if err != nil || settleOutcome != EffectPermitApplied || settled == nil || settled.CurrentAtSettlement {
		t.Fatalf("domain-fenced settlement permit=%+v outcome=%v err=%v, want historical applied", settled, settleOutcome, err)
	}
	gotClaim, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotClaim.Phase != "route_issued" || gotClaim.TabID != 9 {
		t.Fatalf("domain-fenced settlement mutated claim=%+v", gotClaim)
	}
}
func TestInstitutionalPermitSettlementRejectsWrongJobIdentity(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobA, candidate := permitInstitutionalJob(t, js, "permit-wrong-job")
	jobB := permitJob(t, js, "permit-wrong-job-result")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 4,
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 4, 1, 1); err != nil {
		t.Fatal(err)
	}
	in := InstitutionalEffectPermitAcquireInput{
		JobID: jobA, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID:         "domain",
		InstitutionalRequestID: "permit-wrong-job-request",
		JobAttemptRevision:     1, BrowserHolderGeneration: 4,
		ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().UTC().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	}
	permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, in)
	if err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire permit=%+v outcome=%v err=%v", permit, outcome, err)
	}
	_, settleOutcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
		Identity: EffectPermitIdentity{
			JobID: jobB, Kind: Institutional, ClaimID: claim.ID,
			BindingID: claim.BindingID, EffectOrdinal: 1,
			InstitutionalRequestID: in.InstitutionalRequestID,
		},
	})
	if !errors.Is(err, ErrEffectPermitStale) || settleOutcome != EffectPermitSettleStale {
		t.Fatalf("wrong-job settlement outcome=%v err=%v, want stale", settleOutcome, err)
	}
	got, err := js.GetEffectPermit(ctx, permit.ID)
	if err != nil || got == nil || got.Status != EffectPermitHeld {
		t.Fatalf("wrong-job settlement changed permit=%+v err=%v", got, err)
	}
	var results int
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind='browser.institutional_effect_result'`,
	).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if results != 0 {
		t.Fatalf("wrong-job settlement appended %d result events", results)
	}
}

func TestArtifactAdoptionRejectsWrongJobProducer(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobA := permitJob(t, js, "adoption-wrong-job-owner")
	jobB := permitJob(t, js, "adoption-wrong-job-observer")
	identity := driveIdentity(jobA, "adoption-wrong-job-attempt", 0, "generic")
	permit := acquireDrive(t, js, identity, "adoption-wrong-job-domain", time.Now().Add(time.Minute))
	ordinal := int64(0)
	producer := ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID,
		Ordinal: &ordinal, Strategy: identity.Strategy, Revision: identity.Revision,
	}
	settled, err := js.SettleArtifactProducer(ctx, jobB, producer)
	if err != nil || settled {
		t.Fatalf("wrong-job adoption settled=%v err=%v, want no match", settled, err)
	}
	got, err := js.GetEffectPermit(ctx, permit.ID)
	if err != nil || got == nil || got.Status != EffectPermitHeld {
		t.Fatalf("wrong-job adoption changed permit=%+v err=%v", got, err)
	}
}
func TestAcquireEffectPermitSettledLegacyTombstonesNeverAuthorize(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := store.Now()
	genericJob := permitJob(t, js, "permit-legacy-tombstone-generic")
	directJob := permitJob(t, js, "permit-legacy-tombstone-direct")
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
		   strategy, revision, cleanup_only, status, created_at, updated_at)
		VALUES
		  ('legacy-settled-generic', 'generic_drive', ?, 'domain:generic',
		   'old-generic', 0, 'fallback', 'r1', 1, 'unresolved', ?, ?),
		  ('legacy-settled-direct', 'direct_get', ?, 'domain:direct',
		   'old-direct', 0, 'direct_get', 'r1', 1, 'unresolved', ?, ?)`,
		genericJob, now, now, directJob, now, now); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{
		Kind: GenericDrive, JobID: genericJob, DriveAttemptID: "old-generic",
		Ordinal: 0, Strategy: "fallback", Revision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{
		Kind: DirectGet, JobID: directJob, DriveAttemptID: "old-direct",
		Ordinal: 0, Strategy: "direct_get", Revision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		id   EffectPermitIdentity
		job  string
	}{
		{
			name: "generic",
			job:  genericJob,
			id:   EffectPermitIdentity{JobID: genericJob, Kind: GenericDrive, DriveAttemptID: "old-generic", Ordinal: 0, Strategy: "fallback", Revision: "r1"},
		},
		{
			name: "direct",
			job:  directJob,
			id:   EffectPermitIdentity{JobID: directJob, Kind: DirectGet, DriveAttemptID: "old-direct", Ordinal: 0, Strategy: "direct_get", Revision: "r1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before int
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='effect.authorized'`, tc.job).Scan(&before); err != nil {
				t.Fatal(err)
			}
			permit, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
				Identity: tc.id, JobAttemptRevision: 1, BrowserHolderGeneration: 1,
				SafetyDomainID: "domain:" + tc.name, LeaseUntil: time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "effect.authorized"},
			})
			if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
				t.Fatalf("settled exact acquire permit=%+v outcome=%v err=%v, want stale/no permit", permit, outcome, err)
			}
			var permits, after int
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM effect_permits WHERE job_id=?`, tc.job).Scan(&permits); err != nil {
				t.Fatal(err)
			}
			if err := js.S.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='effect.authorized'`, tc.job).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if permits != 0 || after != before {
				t.Fatalf("settled exact acquire permits=%d events before=%d after=%d, want 0/%d", permits, before, after, before)
			}
		})
	}

	// A distinct identity is still admissible after all unresolved global
	// blockers are absent; the settled row is not occupancy.
	fresh, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
		Identity:           EffectPermitIdentity{JobID: genericJob, Kind: GenericDrive, DriveAttemptID: "fresh-generic", Ordinal: 0, Strategy: "fallback", Revision: "r1"},
		JobAttemptRevision: 1, BrowserHolderGeneration: 1, SafetyDomainID: "domain:fresh",
		LeaseUntil: time.Now().Add(time.Minute), Authorization: EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err != nil || outcome != EffectPermitAcquired || fresh == nil {
		t.Fatalf("fresh identity acquire permit=%+v outcome=%v err=%v", fresh, outcome, err)
	}
}
func TestAcquireInstitutionalEffectPermitSettledLegacyTombstoneNeverAuthorizes(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-legacy-tombstone-institutional")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, claim_id, binding_id,
		   effect_ordinal, cleanup_only, status, created_at, updated_at)
		VALUES ('legacy-settled-institutional', 'institutional', ?, 'domain',
		        ?, ?, 1, 1, 'settled', ?, ?)`,
		jobID, claim.ID, claim.BindingID, now, now); err != nil {
		t.Fatal(err)
	}
	var beforeEvents, beforePermits int
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='institutional.authorized'`, jobID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_permits WHERE job_id=?`, jobID).Scan(&beforePermits); err != nil {
		t.Fatal(err)
	}
	permit, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: "domain", InstitutionalRequestID: "fresh-institutional-request",
		JobAttemptRevision: 1, BrowserHolderGeneration: 3, ExpectedEffectOrdinal: 0,
		LeaseUntil:    time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	})
	if !errors.Is(err, ErrEffectPermitStale) || outcome != EffectPermitStaleOutcome || permit != nil {
		t.Fatalf("settled exact institutional acquire permit=%+v outcome=%v err=%v, want stale/no permit", permit, outcome, err)
	}
	var afterEvents, afterPermits int
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='institutional.authorized'`, jobID).Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_permits WHERE job_id=?`, jobID).Scan(&afterPermits); err != nil {
		t.Fatal(err)
	}
	if afterEvents != beforeEvents || afterPermits != beforePermits {
		t.Fatalf("settled institutional tombstone changed permits/events %d/%d -> %d/%d", beforePermits, beforeEvents, afterPermits, afterEvents)
	}
	current, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Phase != "bound" || current.EffectOrdinal != 0 {
		t.Fatalf("settled institutional tombstone mutated claim=%+v", current)
	}
}
