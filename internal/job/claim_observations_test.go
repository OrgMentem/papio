// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedGateOccurrence(t *testing.T, js *Store, id string) {
	t.Helper()
	if err := js.UpsertHumanGateObservation(context.Background(), HumanGateObservation{
		ID: id, GateType: HumanGateLogin, ScopeClass: string(HumanGateScopeAuthenticationClaim),
		ScopeKey: "claim-journal", ObservationRevision: 1, Status: HumanGateOpen, DetailJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimObservationJournalIdempotencyTrio(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	seedGateOccurrence(t, js, "occurrence-1")
	seedGateOccurrence(t, js, "occurrence-2")

	// Fresh: nothing recorded yet.
	outcome, err := js.CheckClaimObservationJournal(ctx, "observation-fresh", "occurrence-1", 1)
	if err != nil || outcome != "" {
		t.Fatalf("fresh check = %q, %v; want empty outcome", outcome, err)
	}
	record := ClaimObservationRecord{
		ObservationID: "observation-fresh", GateOccurrenceID: "occurrence-1",
		AuthenticationClaimID: "claim-journal", BindingID: "binding-journal",
		BrowserHolderGeneration: 3, EventKind: "wall_observed", EventOrdinal: 1,
	}
	if err := js.RecordClaimObservation(ctx, record); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Duplicate: exact same observation_id, ordinal, and occurrence replayed.
	outcome, err = js.CheckClaimObservationJournal(ctx, "observation-fresh", "occurrence-1", 1)
	if err != nil || outcome != "duplicate" {
		t.Fatalf("duplicate check = %q, %v; want duplicate", outcome, err)
	}

	// Rejected: same observation_id, but the replayed frame disagrees with
	// what was actually recorded (mismatched ordinal).
	outcome, err = js.CheckClaimObservationJournal(ctx, "observation-fresh", "occurrence-1", 2)
	if err != nil || outcome != "rejected" {
		t.Fatalf("mismatched replay check = %q, %v; want rejected", outcome, err)
	}

	// Stale: a genuinely new observation_id whose ordinal does not exceed
	// the highest applied ordinal for this occurrence.
	outcome, err = js.CheckClaimObservationJournal(ctx, "observation-stale", "occurrence-1", 1)
	if err != nil || outcome != "stale" {
		t.Fatalf("stale check = %q, %v; want stale", outcome, err)
	}

	// A higher ordinal for the same occurrence is genuinely fresh.
	outcome, err = js.CheckClaimObservationJournal(ctx, "observation-next", "occurrence-1", 2)
	if err != nil || outcome != "" {
		t.Fatalf("next ordinal check = %q, %v; want empty outcome", outcome, err)
	}

	// A different occurrence entirely starts its own ordinal sequence at 0.
	outcome, err = js.CheckClaimObservationJournal(ctx, "observation-other-occurrence", "occurrence-2", 0)
	if err != nil || outcome != "" {
		t.Fatalf("independent occurrence check = %q, %v; want empty outcome", outcome, err)
	}
}

func TestRecordClaimObservationRejectsInvalidRecord(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	if err := js.RecordClaimObservation(ctx, ClaimObservationRecord{}); err == nil {
		t.Fatal("empty record was accepted")
	}
	if err := js.RecordClaimObservation(ctx, ClaimObservationRecord{
		ObservationID: "observation-bad-kind", GateOccurrenceID: "occurrence-1",
		AuthenticationClaimID: "claim-journal", BindingID: "binding-journal",
		BrowserHolderGeneration: 3, EventKind: "not_a_real_kind", EventOrdinal: 1,
	}); err == nil {
		t.Fatal("unrecognized event kind was accepted")
	}
}

func TestEligibleAuthenticationClaimDependentsCountsOnlyEligibleSiblings(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	seedInstitutionProfile(t, js, "profile-dependents")
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE institution_profiles SET authentication_claim_id='claim-dependents' WHERE id='profile-dependents'`); err != nil {
		t.Fatal(err)
	}
	jobA, err := js.CreateRequest(ctx, "req-dep-a", testWork(), "", "", testPolicy(), map[string]string{"doi": testWork().DOI}, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	seedCandidate(t, js, jobA, "profile-dependents", "candidate-dep-a")
	workB := testWork()
	workB.DOI = "10.1000/dependents-b"
	jobB, err := js.CreateRequest(ctx, "req-dep-b", workB, "", "", testPolicy(), map[string]string{"doi": workB.DOI}, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	seedCandidate(t, js, jobB, "profile-dependents", "candidate-dep-b")
	workC := testWork()
	workC.DOI = "10.1000/dependents-c"
	jobC, err := js.CreateRequest(ctx, "req-dep-c", workC, "", "", testPolicy(), map[string]string{"doi": workC.DOI}, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	seedCandidate(t, js, jobC, "profile-dependents", "candidate-dep-c")
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE browser_candidates SET status='claimed' WHERE id='candidate-dep-c'`); err != nil {
		t.Fatal(err)
	}

	n, err := js.EligibleAuthenticationClaimDependents(ctx, "claim-dependents")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("dependent count = %d, want 2 (candidate-dep-c is claimed, not eligible)", n)
	}
	if n, err := js.EligibleAuthenticationClaimDependents(ctx, "claim-no-dependents"); err != nil || n != 0 {
		t.Fatalf("unrelated claim dependent count = %d, %v; want 0", n, err)
	}
}

func TestAbandonMaterializationClaimByBindingOnlyTouchesLivePhases(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := seedJobAndCandidate(t, js, "abandon-binding")
	authorizeEffectPermitJob(t, js, jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 1, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.AbandonMaterializationClaimByBinding(ctx, claim.BindingID); err != nil {
		t.Fatal(err)
	}
	got, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || got == nil || got.Phase != "abandoned" {
		t.Fatalf("claim after abandon = %+v, %v; want phase abandoned", got, err)
	}
	// Idempotent: abandoning an already-abandoned claim's binding is a no-op,
	// not an error.
	if err := js.AbandonMaterializationClaimByBinding(ctx, claim.BindingID); err != nil {
		t.Fatalf("re-abandon: %v", err)
	}
	// An unknown binding is also a silent no-op.
	if err := js.AbandonMaterializationClaimByBinding(ctx, "binding-never-existed"); err != nil {
		t.Fatalf("unknown binding: %v", err)
	}
}

func TestConsumeCloseAuthorizationForBindingMarksIssuedTokenConsumed(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _, err := js.IssueCloseAuthorization(ctx, "binding-consume-1", 1, "claim_abandoned", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.ConsumeCloseAuthorizationForBinding(ctx, "binding-consume-1", now); err != nil {
		t.Fatal(err)
	}
	var status string
	var consumedAt string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT status, COALESCE(consumed_at,'') FROM close_authorizations WHERE id=?`, id,
	).Scan(&status, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if status != "consumed" || consumedAt == "" {
		t.Fatalf("token status=%q consumed_at=%q, want consumed with a timestamp", status, consumedAt)
	}
	// A binding with no issued token is a silent no-op.
	if err := js.ConsumeCloseAuthorizationForBinding(ctx, "binding-never-issued", now); err != nil {
		t.Fatalf("no issued token: %v", err)
	}
}

func TestAuthenticationEntryLeaseOwnerBindingSetAndClear(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-owner-binding", LeaseID: "lease-owner-binding", OwnerID: "job-owner-binding",
		BrowserHolderGeneration: 5, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-owner-binding", "job-owner-binding", 5, "binding-owner-1", 42); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-owner-binding")
	if err != nil || !ok || lease.OwnerBindingID != "binding-owner-1" || lease.OwnerTabHint == nil || *lease.OwnerTabHint != 42 {
		t.Fatalf("lease after set = %+v ok=%v err=%v", lease, ok, err)
	}
	// A mismatched fence (wrong owner) is a silent no-op.
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-owner-binding", "job-someone-else", 5, "binding-owner-2", 7); err != nil {
		t.Fatal(err)
	}
	lease, ok, err = js.GetAuthenticationEntryLease(ctx, "claim-owner-binding")
	if err != nil || !ok || lease.OwnerBindingID != "binding-owner-1" {
		t.Fatalf("lease after mismatched set = %+v ok=%v err=%v; owner binding must be unchanged", lease, ok, err)
	}
	if err := js.ClearAuthenticationEntryLeaseOwnerBinding(ctx, "claim-owner-binding", "binding-owner-1"); err != nil {
		t.Fatal(err)
	}
	lease, ok, err = js.GetAuthenticationEntryLease(ctx, "claim-owner-binding")
	if err != nil || !ok || lease.OwnerBindingID != "" || lease.OwnerTabHint != nil {
		t.Fatalf("lease after clear = %+v ok=%v err=%v", lease, ok, err)
	}
}

func TestAuthenticationEntryLeaseReassignmentClearsPriorOwnerBinding(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-reassign", LeaseID: "lease-old", OwnerID: "job-old",
		BrowserHolderGeneration: 1, LeaseUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-reassign", "job-old", 1, "binding-old", 1); err != nil {
		t.Fatal(err)
	}
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-reassign", LeaseID: "lease-new", OwnerID: "job-new",
		BrowserHolderGeneration: 2, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.OwnerBindingID != "" || lease.OwnerTabHint != nil {
		t.Fatalf("reassigned lease retained the prior owner's binding: %+v", lease)
	}
}

// TestAuthenticationEntryLeaseOwnerReReservesAcrossGenerations pins whose lease
// this is. A reserved entry belongs to the job signing in, not to the browser
// session that arbitrated for it, so its own owner may re-reserve under a new
// holder generation with a fresh lease id — the shape a service-worker restart
// mid-login produces, since the lease id is derived from the epoch. Requiring
// lease-id and generation equality stranded the institution's only entry for
// the full action-expiry window after every reconnect: unrenewable, and
// un-re-reservable even by its owner, while every claim_observation for that
// login was refused as "owned elsewhere". The live sign-in surface stays put
// across the renewal (nothing about the paper changed), which is what
// distinguishes this from the reassignment above.
func TestAuthenticationEntryLeaseOwnerReReservesAcrossGenerations(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-regen", LeaseID: "lease-gen-1", OwnerID: "job-regen",
		BrowserHolderGeneration: 1, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-regen", "job-regen", 1, "binding-regen", 9); err != nil {
		t.Fatal(err)
	}
	renewed, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-regen", LeaseID: "lease-gen-2", OwnerID: "job-regen",
		BrowserHolderGeneration: 2, LeaseUntil: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("owner re-reserving under a new generation: %v", err)
	}
	if renewed.BrowserHolderGeneration != 2 || renewed.LeaseID != "lease-gen-2" {
		t.Fatalf("renewal did not carry the new holder identity forward: %+v", renewed)
	}
	if renewed.OwnerBindingID != "binding-regen" || renewed.OwnerTabHint == nil || *renewed.OwnerTabHint != 9 {
		t.Fatalf("renewal disturbed the live sign-in surface: %+v", renewed)
	}
	// A different job still cannot take a live reserved entry: one sign-in per
	// institution is the invariant the generation fence was mistaken for.
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-regen", LeaseID: "lease-gen-3", OwnerID: "job-stranger",
		BrowserHolderGeneration: 2, LeaseUntil: now.Add(3 * time.Minute),
	}); !errors.Is(err, ErrAuthenticationEntryLeaseBusy) {
		t.Fatalf("stranger reserving a live entry = %v, want ErrAuthenticationEntryLeaseBusy", err)
	}
}

// TestAuthenticationEntryUnboundReservationYieldsAtTheBindDeadline pins what an
// unconsumed grant costs. A reservation with no bound surface is a permission to
// open one, not a sign-in in progress, so it may only hold its institution long
// enough to bind. It used to get the caller's full human-paced window, and
// nothing could retire it early — the owner-close retirement fences on
// owner_binding_id, which it does not have — so one paper that never bound held
// the operator's institution for thirty minutes while every other consult was
// answered "authentication entry lease is unavailable" (observed live
// 2026-08-19). A BOUND entry keeps the full window, which is the other half of
// the rule and what stops a real sign-in being cut short.
func TestAuthenticationEntryUnboundReservationYieldsAtTheBindDeadline(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	full := now.Add(30 * time.Minute)

	unbound, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-bind-deadline", LeaseID: "lease-unbound", OwnerID: "job-unbound",
		BrowserHolderGeneration: 3, LeaseUntil: full,
	})
	if err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339Nano, unbound.LeaseUntil)
	if err != nil {
		t.Fatalf("lease_until %q: %v", unbound.LeaseUntil, err)
	}
	if until.After(now.Add(AuthenticationEntryBindDeadline + time.Second)) {
		t.Fatalf("unbound reservation holds until %s, want no later than the bind deadline (%s)",
			until, now.Add(AuthenticationEntryBindDeadline))
	}

	// Binding a surface earns the full window on the next renewal.
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-bind-deadline", "job-unbound", 3, "binding-bound", 4); err != nil {
		t.Fatal(err)
	}
	bound, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-bind-deadline", LeaseID: "lease-bound", OwnerID: "job-unbound",
		BrowserHolderGeneration: 3, LeaseUntil: full,
	})
	if err != nil {
		t.Fatalf("renewing a bound entry: %v", err)
	}
	if bound.LeaseUntil != full.Format(time.RFC3339Nano) {
		t.Fatalf("bound renewal holds until %s, want the caller's full window %s",
			bound.LeaseUntil, full.Format(time.RFC3339Nano))
	}
}

// TestAuthenticationEntryLeaseExpiryRefusedWhileEffectPermitHeld pins §4.5:
// expiry alone never authorizes a replacement while an effect permit is
// unresolved for the lease's own occupying binding.
func TestAuthenticationEntryLeaseExpiryRefusedWhileEffectPermitHeld(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := seedJobAndCandidate(t, js, "permit-hold")
	authorizeEffectPermitJob(t, js, jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 9, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-permit-hold", LeaseID: "lease-permit-hold", OwnerID: jobID,
		BrowserHolderGeneration: 9, LeaseUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-permit-hold", jobID, 9, claim.BindingID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO effect_permits
		  (id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind,
		   claim_id, binding_id, effect_ordinal, institutional_request_id, status, lease_until, created_at, updated_at)
		VALUES ('permit-hold-1', ?, 1, 9, 'domain-permit-hold', 'institutional',
		        ?, ?, 1, 'institutional-request-permit-hold', 'held', ?, ?, ?)`,
		jobID, claim.ID, claim.BindingID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// A different owner trying to reserve the same claim must be refused
	// Busy, not granted a fresh reservation, because the prior owner's
	// browser-local effect is still occupying.
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-permit-hold", LeaseID: "lease-permit-hold-new", OwnerID: "job-other",
		BrowserHolderGeneration: 9, LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); !errors.Is(err, ErrAuthenticationEntryLeaseBusy) {
		t.Fatalf("reserve while effect permit held err=%v, want ErrAuthenticationEntryLeaseBusy", err)
	}

	// Settling the permit releases the occupancy, and the same expired
	// lease now genuinely replaces.
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE effect_permits SET status='settled' WHERE id='permit-hold-1'`); err != nil {
		t.Fatal(err)
	}
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-permit-hold", LeaseID: "lease-permit-hold-new", OwnerID: "job-other",
		BrowserHolderGeneration: 9, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || lease.OwnerID != "job-other" {
		t.Fatalf("reserve after permit settled = %+v, %v; want fresh reservation for job-other", lease, err)
	}
}

// A dead paper must not keep its institution's only sign-in slot. Reservation
// already treats a terminal owner as expired, but nothing forced that
// evaluation: measured live 2026-08-20, a cancelled paper held the operator's
// entry in state `human` for over ten hours because no other candidate existed
// to contend for it, while every report of that institution read as a sign-in
// in progress.
func TestRetireTerminalAuthenticationEntryLeasesFreesADeadOwnersSlot(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := seedJobAndCandidate(t, js, "terminal-slot")
	authorizeEffectPermitJob(t, js, jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 4, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-terminal-slot", LeaseID: "lease-terminal-slot", OwnerID: jobID,
		BrowserHolderGeneration: 4, LeaseUntil: time.Now().UTC().Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "claim-terminal-slot", jobID, 4, claim.BindingID, 1); err != nil {
		t.Fatal(err)
	}
	// A live owner's slot is never swept, however long its lease runs.
	if retired, err := js.RetireTerminalAuthenticationEntryLeases(ctx, time.Now().UTC()); err != nil || retired != 0 {
		t.Fatalf("swept a live owner's slot: retired=%d err=%v", retired, err)
	}
	if err := js.Cancel(ctx, jobID, TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	// §4.5 still holds: an unresolved institutional effect keeps occupying.
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO effect_permits
		  (id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind,
		   claim_id, binding_id, effect_ordinal, institutional_request_id, status, lease_until, created_at, updated_at)
		VALUES ('permit-terminal-slot', ?, 1, 4, 'domain-terminal-slot', 'institutional',
		        ?, ?, 1, 'institutional-request-terminal-slot', 'held', ?, ?, ?)`,
		jobID, claim.ID, claim.BindingID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if retired, err := js.RetireTerminalAuthenticationEntryLeases(ctx, time.Now().UTC()); err != nil || retired != 0 {
		t.Fatalf("swept a slot whose institutional effect is unresolved: retired=%d err=%v", retired, err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE effect_permits SET status='settled' WHERE id='permit-terminal-slot'`); err != nil {
		t.Fatal(err)
	}
	if retired, err := js.RetireTerminalAuthenticationEntryLeases(ctx, time.Now().UTC()); err != nil || retired != 1 {
		t.Fatalf("terminal owner's settled slot: retired=%d err=%v, want 1", retired, err)
	}
	// Freed, so the next paper takes the institution without contending with a
	// corpse - and the sweep is idempotent.
	lease, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-terminal-slot", LeaseID: "lease-terminal-next", OwnerID: "job-next",
		BrowserHolderGeneration: 4, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || lease.OwnerID != "job-next" {
		t.Fatalf("reserve after retirement = %+v, %v", lease, err)
	}
	if retired, err := js.RetireTerminalAuthenticationEntryLeases(ctx, time.Now().UTC()); err != nil || retired != 0 {
		t.Fatalf("sweep is not idempotent: retired=%d err=%v", retired, err)
	}
}

// A retired claim's surface is gone by definition, so the institution slot its
// binding occupied must go with it. Without this, a claim that dies unobserved
// (its tab closed while the extension was reloading, a browser crash) leaves
// the entry `human` with a NULL deadline naming a dead tab - held forever,
// blocking every sibling. Measured live 2026-08-20.
func TestReconcileMaterializationClaimsReleasesTheEntryItOccupied(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := seedJobAndCandidate(t, js, "orphan-surface")
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidateID, BrowserHolderGeneration: 6, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-orphan-surface", LeaseID: "lease-orphan", OwnerID: jobID,
		BrowserHolderGeneration: 6, LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx,
		"claim-orphan-surface", jobID, 6, claim.BindingID, 1); err != nil {
		t.Fatal(err)
	}
	// Bound and human-paced: exactly the live shape, with no deadline at all.
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE authentication_entry_leases SET state='human', human_owner_id=?, lease_until=NULL
		  WHERE authentication_claim_id='claim-orphan-surface'`, jobID); err != nil {
		t.Fatal(err)
	}

	// An unexpired claim keeps its institution.
	if _, err := js.ReconcileMaterializationClaims(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-orphan-surface")
	if err != nil || !ok || lease.State != AuthenticationEntryLeaseHuman {
		t.Fatalf("live claim's entry = %+v ok=%v err=%v, want still held", lease, ok, err)
	}

	// Its lease expires with nothing left alive to report the loss.
	retired, err := js.ReconcileMaterializationClaims(ctx, time.Now().UTC().Add(2*time.Minute))
	if err != nil || len(retired) != 1 {
		t.Fatalf("reconcile retired %d claims, err=%v, want 1", len(retired), err)
	}
	freed, ok, err := js.GetAuthenticationEntryLease(ctx, "claim-orphan-surface")
	if err != nil || !ok {
		t.Fatalf("entry after reconcile = %+v ok=%v err=%v", freed, ok, err)
	}
	if freed.State == AuthenticationEntryLeaseHuman {
		t.Fatal("a retired claim left its institution held by a paper with no surface")
	}
	// Freed for real: the next paper takes the library.
	next, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "claim-orphan-surface", LeaseID: "lease-after", OwnerID: "job-after",
		BrowserHolderGeneration: 6, LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || next.OwnerID != "job-after" {
		t.Fatalf("reserve after release = %+v, %v", next, err)
	}
}
