// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func redriveRoute(name string) (string, bool) {
	if name == "institute" {
		return "https://resolver.example.edu/openurl", true
	}
	return "", false
}

// redriveOAHandoff stands in for app's open-access handoff detail check, which
// this package cannot import.
func redriveOAHandoff(detail string) bool {
	return strings.HasPrefix(detail, "open-access fetch via browser\n")
}

func redriveJob(t *testing.T, js *Store) (string, int64) {
	t.Helper()
	return redriveJobParkedOn(t, js, "manual_download")
}

func redriveJobParkedOn(t *testing.T, js *Store, kind string) (string, int64) {
	t.Helper()
	id := resolvingParkCandidate(t, js, "redrive")
	if err := js.ParkWithHumanAction(context.Background(), id, StateResolving, StateAwaitingHuman,
		kind, "papio could not drive the provider", nil, Access(true, "landing_page")); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(context.Background(), []string{id})
	if err != nil || len(open) != 1 {
		t.Fatalf("actions=%+v err=%v", open, err)
	}
	return id, open[0].ID
}

func TestRedriveReplacesManualDownloadAndRetiresClaim(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, old := redriveJob(t, js)
	profile := institutionalProfile(t, js, "institute", "digest", "auth")
	candidate := institutionalCandidate(t, js, profile, "redrive-claim", id)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := js.ReserveAuthenticationEntryLease(ctx, AuthenticationEntryLeaseInput{
		AuthenticationClaimID: "redrive-auth", LeaseID: "redrive-lease", OwnerID: id,
		BrowserHolderGeneration: 1, LeaseUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SetAuthenticationEntryLeaseOwnerBinding(ctx, "redrive-auth", id, 1, claim.BindingID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID == old || open[0].Kind != "openurl_handoff" || open[0].Detail != "institutional handoff detail" || !open[0].RequiresAuth || open[0].BlockedBy != "paywall" {
		t.Fatalf("replacement=%+v err=%v", open, err)
	}
	var status, phase string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, old).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("old status=%q err=%v", status, err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&phase); err != nil || phase != "abandoned" {
		t.Fatalf("claim phase=%q err=%v", phase, err)
	}
	lease, ok, err := js.GetAuthenticationEntryLease(ctx, "redrive-auth")
	if err != nil || !ok || lease.State != AuthenticationEntryLeaseExpired || lease.OwnerBindingID != "" {
		t.Fatalf("entry lease=%+v ok=%t err=%v", lease, ok, err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] == "job.retry_requested" {
			detail, _ := event["detail"].(map[string]any)
			found = detail["reason"] == "operator_redrive" && detail["action_id"] == float64(old) && detail["action_revision"] == float64(1)
		}
	}
	if !found {
		t.Fatalf("redrive event missing action identity: %+v", events)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 0, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat without new outcome=%v", err)
	}
}

// A job parked on the provider's terms step has no route left to open, so
// redrive is the operator's only way back once consent is settled in the
// extension. Measured live 2026-09-23: JSTOR job_edfe… sat on one open terms
// action after its handoff was cancelled, and every CLI verb refused it.
func TestRedriveReplacesTermsActionAndRetiresParkedClaim(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, terms := redriveJobParkedOn(t, js, "terms_acceptance_required")
	profile := institutionalProfile(t, js, "institute", "digest", "auth")
	candidate := institutionalCandidate(t, js, profile, "redrive-terms", id)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET phase='parked',lease_until=NULL WHERE id=?`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].Kind != "openurl_handoff" || open[0].Detail != "institutional handoff detail" || !open[0].RequiresAuth {
		t.Fatalf("replacement=%+v err=%v", open, err)
	}
	var status, phase string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, terms).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("terms status=%q err=%v", status, err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&phase); err != nil || phase != "abandoned" {
		t.Fatalf("parked claim phase=%q err=%v", phase, err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] == "job.retry_requested" {
			detail, _ := event["detail"].(map[string]any)
			found = detail["reason"] == "operator_redrive" && detail["action_id"] == float64(terms)
		}
	}
	if !found {
		t.Fatalf("redrive event missing terms action identity: %+v", events)
	}
}

func TestRedriveRefusesUnsafeOrStaleRequests(t *testing.T) {
	for _, scenario := range []string{
		"state", "revision", "other_action", "terms_live_claim", "verify_identity", "unsafe_pdf",
		"no_action", "no_identifier", "no_resolver", "lease", "held_effect",
		"unknown_effect", "already_redriven", "institutional_handoff", "unavailable_other_reason",
		"needs_review_undiagnosed", "needs_review_verify_identity",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			js := testStore(t)
			id, action := redriveJob(t, js)
			revision := int64(1)
			route := redriveRoute
			fileGone := false
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := js.S.DB().ExecContext(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "state":
				exec(`UPDATE jobs SET state='resolving' WHERE id=?`, id)
			case "revision":
				revision = 2
			case "other_action":
				_, err := js.OpenHumanAction(ctx, id, "human_auth_required", "login", Access(true, "paywall"))
				if err != nil {
					t.Fatal(err)
				}
			case "terms_live_claim":
				// A live claim is a drive still on the terms surface; only a
				// parked or retired claim leaves the job to the operator.
				exec(`UPDATE human_actions SET kind='terms_acceptance_required' WHERE id=?`, action)
				profile := institutionalProfile(t, js, "institute", "digest", "auth")
				candidate := institutionalCandidate(t, js, profile, "redrive-terms-live", id)
				if _, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
			case "verify_identity", "unsafe_pdf":
				exec(`UPDATE human_actions SET kind=? WHERE id=?`, scenario, action)
			case "institutional_handoff":
				// Only an open-access route is spent by parking on it; the
				// institutional handoff is the route redrive would open.
				exec(`UPDATE human_actions SET kind='openurl_handoff', detail='institutional OpenURL handoff' WHERE id=?`, action)
			case "unavailable_other_reason":
				// Only an empty browser job_reject retired a handoff without
				// evidence; every other unavailable outcome stays settled.
				exec(`UPDATE jobs SET state='unavailable', terminal_reason='no_entitlement' WHERE id=?`, id)
			case "needs_review_undiagnosed", "needs_review_verify_identity":
				// needs_review reopens only the park an adopted file that could
				// not be quarantined left, even once that file is gone.
				fileGone = true
				exec(`UPDATE jobs SET state='needs_review' WHERE id=?`, id)
				if scenario == "needs_review_verify_identity" {
					exec(`UPDATE human_actions SET kind='verify_identity', diagnosis=? WHERE id=?`, DiagnosisReasonAdoptedPDFInvalid, action)
				}
			case "no_action":
				exec(`UPDATE human_actions SET status='cancelled' WHERE id=?`, action)
				revision = 0
			case "no_identifier":
				exec(`DELETE FROM identifiers`)
			case "no_resolver":
				route = func(string) (string, bool) { return "", false }
			case "lease":
				exec(`UPDATE jobs SET lease_owner='other',lease_expires_at='2099-01-01T00:00:00Z' WHERE id=?`, id)
			case "held_effect", "unknown_effect":
				busy := permitJob(t, js, "redrive-busy")
				permit := acquireDrive(t, js, driveIdentity(busy, "redrive-effect", 0, "generic"), "institution:example.edu", time.Now().Add(time.Minute))
				if scenario == "unknown_effect" {
					exec(`UPDATE effect_permits SET status='unknown_completion' WHERE id=?`, permit.ID)
				}
			case "already_redriven":
				if err := js.RecordEvent(ctx, id, "job.retry_requested", map[string]any{"reason": "operator_redrive"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := js.RedriveInstitutionalHandoff(ctx, id, revision, route, redriveOAHandoff, fileGone, "institutional handoff detail"); !errors.Is(err, ErrConflict) {
				t.Fatalf("redrive=%v; want ErrConflict", err)
			}
			var status string
			if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, action).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if scenario != "no_action" && status != "open" {
				t.Fatalf("refusal changed action status to %s", status)
			}
		})
	}
}

// The paced drive ranks manual downloads by asking whether a redrive would be
// accepted. The check must agree with the verb and must change nothing: a
// status read that replaced the operator's manual download would be a redrive
// nobody asked for.
func TestCheckRedriveAgreesWithRedriveAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, action := redriveJob(t, js)
	if err := js.CheckRedriveInstitutionalHandoff(ctx, id, 2, redriveRoute, redriveOAHandoff); !errors.Is(err, ErrConflict) {
		t.Fatalf("check with a stale revision = %v, want conflict", err)
	}
	if err := js.CheckRedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff); err != nil {
		t.Fatalf("check on a redrivable park = %v", err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != action || open[0].Kind != "manual_download" {
		t.Fatalf("open actions after the check = %+v err=%v, want the manual download untouched", open, err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "job.retry_requested" {
			t.Fatalf("the check recorded a redrive: %+v", event)
		}
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatalf("redrive after a passing check = %v", err)
	}
}

// A redrive must not hand a paper to a library route that cannot serve it.
// Live 2026-09-24 (job_49a7…): a paced redrive turned the manual download the
// PMC open-access page left into a fresh library handoff, although that
// library had already reported no entitlement. Its second no_entitlement then
// ended the job unavailable with the PMC route still in hand. Both shapes go
// back to resolving, where exhaustion re-derives the live open-access route,
// or settles the job when no route remains.
func TestRedriveRediscoversInsteadOfOpeningASpentLibraryRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		park func(t *testing.T, js *Store) (string, int64)
	}{
		{"manual download left by the open-access route", func(t *testing.T, js *Store) (string, int64) {
			ctx := context.Background()
			id := resolvingParkCandidate(t, js, "redrive-oa-manual")
			if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman, "openurl_handoff",
				"open-access fetch via browser\nhttps://pmc.example.org/articles/PMC1/", nil, Access(false, "anti_bot")); err != nil {
				t.Fatal(err)
			}
			// The bridge resolves the driven handoff, then opens the manual
			// download for the page it could not drive.
			if _, err := js.S.DB().ExecContext(ctx, `UPDATE human_actions SET status='resolved' WHERE job_id=?`, id); err != nil {
				t.Fatal(err)
			}
			if err := js.RecordEvent(ctx, id, "browser.provider_outcome", map[string]any{"outcome": "ui_changed"}); err != nil {
				t.Fatal(err)
			}
			manual, err := js.OpenHumanAction(ctx, id, "manual_download",
				"papio has no adapter for this provider yet; download the PDF yourself for now", Access(false, "landing_page"))
			if err != nil {
				t.Fatal(err)
			}
			return id, manual
		}},
		{"library route that already reported no entitlement", func(t *testing.T, js *Store) (string, int64) {
			id, manual := redriveJob(t, js)
			if err := js.RecordEvent(context.Background(), id, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
				t.Fatal(err)
			}
			return id, manual
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			js := testStore(t)
			id, manual := tc.park(t, js)
			if err := js.CheckRedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff); err != nil {
				t.Fatalf("check = %v, want the spent route redrivable", err)
			}
			fresh, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail")
			if err != nil {
				t.Fatal(err)
			}
			open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
			if err != nil || fresh != 0 || len(open) != 0 {
				t.Fatalf("redrive opened action %d, open=%+v err=%v; want no library handoff", fresh, open, err)
			}
			row, err := js.Get(ctx, id)
			if err != nil || row.State != StateResolving {
				t.Fatalf("job after redrive = %+v err=%v; want resolving", row, err)
			}
			var status string
			if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, manual).Scan(&status); err != nil || status != "resolved" {
				t.Fatalf("manual download status=%q err=%v", status, err)
			}
			events, err := js.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			moved, requested := false, false
			for _, event := range events {
				detail, _ := event["detail"].(map[string]any)
				switch event["kind"] {
				case "job.transition":
					moved = moved || (detail["from"] == StateAwaitingHuman && detail["to"] == StateResolving && detail["reason"] == "operator_redrive")
				case "job.retry_requested":
					requested = requested || (detail["reason"] == "operator_redrive" && detail["action_id"] == float64(manual))
				}
			}
			if !moved || !requested {
				t.Fatalf("events %+v; want an operator_redrive transition to resolving and its retry request", events)
			}
		})
	}
}

func TestRedriveResolvedParkAndFreshOutcome(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, action := redriveJob(t, js)
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 {
		t.Fatalf("first open=%+v err=%v", open, err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE human_actions SET status='resolved' WHERE id=?`, open[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 0, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); !errors.Is(err, ErrConflict) {
		t.Fatalf("redrive without outcome=%v", err)
	}
	if err := js.RecordEvent(ctx, id, "browser.provider_outcome", map[string]any{"outcome": "wrong_work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 0, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatal(err)
	}
	open, err = js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].Kind != "openurl_handoff" || open[0].ID == action {
		t.Fatalf("resolved park handoff=%+v err=%v", open, err)
	}
}

func TestRedrivePreviouslyResolvedPark(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, old := redriveJob(t, js)
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE human_actions SET status='resolved' WHERE id=?`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 0, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID == old || open[0].Kind != "openurl_handoff" {
		t.Fatalf("replacement=%+v err=%v", open, err)
	}
}

// Live 2026-09-23 10:51:17Z: six queued institutional handoffs were answered
// with an empty browser job_reject after the extension lost its worker-local
// offer URLs, and the bridge moved them to unavailable/browser_rejected. The
// reject said nothing about the paper, so redrive reopens that park.
func TestRedriveReopensBrowserRejectedHandoff(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "redrive-rejected")
	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		"openurl_handoff", "institutional handoff detail", nil, Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 {
		t.Fatalf("parked actions=%+v err=%v", open, err)
	}
	old := open[0].ID
	if err := js.Transition(ctx, id, StateAwaitingHuman, StateUnavailable,
		map[string]any{"reason": "browser_rejected"}, WithTerminalReason(TerminalReasonBrowserRejected)); err != nil {
		t.Fatal(err)
	}

	fresh, err := js.RedriveInstitutionalHandoff(ctx, id, 0, redriveRoute, redriveOAHandoff, false, "institutional handoff detail")
	if err != nil {
		t.Fatalf("redrive of a browser_rejected handoff: %v", err)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateAwaitingHuman || row.TerminalReason != "" {
		t.Fatalf("row=%+v err=%v; want awaiting_human with no terminal reason", row, err)
	}
	open, err = js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != fresh || open[0].ID == old || open[0].Kind != "openurl_handoff" ||
		open[0].Detail != "institutional handoff detail" || !open[0].RequiresAuth || open[0].BlockedBy != "paywall" {
		t.Fatalf("reopened handoff=%+v err=%v", open, err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	reopened, requested := false, false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		switch event["kind"] {
		case "job.transition":
			reopened = reopened || (detail["from"] == StateUnavailable && detail["to"] == StateAwaitingHuman && detail["reason"] == "operator_redrive")
		case "job.retry_requested":
			requested = requested || (detail["reason"] == "operator_redrive" && detail["action_id"] == float64(old))
		}
	}
	if !reopened || !requested {
		t.Fatalf("redrive events reopened=%t requested=%t: %+v", reopened, requested, events)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, fresh, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat redrive=%v; want ErrConflict", err)
	}
}

// Live 2026-09-23: job_484c… sat in needs_review from 2026-08-30 on one open
// manual_download that asked the operator to remove an adopted file papio
// could not move to rejected/. The file was long gone, and redrive refused
// the state, so only daily reminders remained.
func TestRedriveReturnsUnquarantinedAdoptionParkOnceFileIsGone(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "redrive-unquarantined")
	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateNeedsReview, "manual_download",
		"the adopted download failed validation and could not be quarantined; remove or replace the file in the adoption directory",
		map[string]any{"reason": "adopted_download_rejected_unquarantined"}, Access(true, "landing_page"),
		WithHumanActionDiagnosis(DiagnosisReasonAdoptedPDFInvalid)); err != nil {
		t.Fatal(err)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 {
		t.Fatalf("parked actions=%+v err=%v", open, err)
	}
	old := open[0].ID

	// While the file may still be in the adoption directory, awaiting_human
	// would hand it straight back to the sweep, so the park stays.
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); !errors.Is(err, ErrConflict) {
		t.Fatalf("redrive with the adopted file still present=%v; want ErrConflict", err)
	}
	if row, err := js.Get(ctx, id); err != nil || row.State != StateNeedsReview {
		t.Fatalf("refused redrive moved the job: %+v err=%v", row, err)
	}
	fresh, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, true, "institutional handoff detail")
	if err != nil {
		t.Fatalf("redrive of an unquarantined adoption park whose file is gone: %v", err)
	}
	row, err := js.Get(ctx, id)
	if err != nil || row.State != StateAwaitingHuman {
		t.Fatalf("row=%+v err=%v; want awaiting_human", row, err)
	}
	open, err = js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != fresh || open[0].Kind != "openurl_handoff" || open[0].Detail != "institutional handoff detail" {
		t.Fatalf("replacement=%+v err=%v", open, err)
	}
	var status string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM human_actions WHERE id=?`, old).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("old status=%q err=%v", status, err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	reopened := false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if event["kind"] == "job.transition" && detail["from"] == StateNeedsReview && detail["to"] == StateAwaitingHuman {
			reopened = detail["reason"] == "operator_redrive"
		}
	}
	if !reopened {
		t.Fatalf("no needs_review -> awaiting_human operator_redrive transition: %+v", events)
	}
}

func TestRedriveRollsBackReplacementIfEventFails(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, old := redriveJob(t, js)
	if _, err := js.S.DB().ExecContext(ctx, `CREATE TRIGGER fail_redrive BEFORE INSERT ON events
		WHEN NEW.kind='job.retry_requested' BEGIN SELECT RAISE(ABORT,'injected event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RedriveInstitutionalHandoff(ctx, id, 1, redriveRoute, redriveOAHandoff, false, "institutional handoff detail"); err == nil {
		t.Fatal("injected event failure ignored")
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != old || open[0].Kind != "manual_download" {
		t.Fatalf("partial action replacement=%+v err=%v", open, err)
	}
}
