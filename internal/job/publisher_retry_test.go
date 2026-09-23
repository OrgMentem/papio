// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func publisherFailure(t *testing.T, js *Store) (string, int64) {
	t.Helper()
	ctx := context.Background()
	id := resolvingParkCandidate(t, js, "publisher-retry")
	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		"manual_download", "papio reached a different work", nil, Access(true, "landing_page"),
		WithHumanActionDiagnosis(DiagnosisReasonWrongWork)); err != nil {
		t.Fatal(err)
	}
	actions, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%v err=%v", actions, err)
	}
	return id, actions[0].ID
}

func TestPublisherRetryPreservesFailureAndIsBounded(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, action := publisherFailure(t, js)
	if err := js.RecordEvent(ctx, id, "job.latch", map[string]any{"kind": "no_positive_effects", "safety_domain": "institution:example.edu"}); err != nil {
		t.Fatal(err)
	}
	got, err := js.RetryPublisherHandoff(ctx, action, 1)
	if err != nil || got != id {
		t.Fatalf("retry=%q err=%v", got, err)
	}
	var status, detail, diagnosis string
	if err := js.S.DB().QueryRow(`SELECT status,detail,diagnosis FROM human_actions WHERE id=?`, action).Scan(&status, &detail, &diagnosis); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" || detail != "papio reached a different work" || diagnosis != DiagnosisReasonWrongWork {
		t.Fatalf("failure evidence changed: %s %s %s", status, detail, diagnosis)
	}
	open, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].Detail != PublisherHandoffDetail || !open[0].RequiresAuth {
		t.Fatalf("replacement=%+v err=%v", open, err)
	}
	if attempt, err := js.MaterializationAttemptRevision(ctx, id); err != nil || attempt != 2 {
		t.Fatalf("attempt=%d err=%v", attempt, err)
	}
	var latches int
	if err := js.S.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='job.latch'`, id).Scan(&latches); err != nil || latches != 1 {
		t.Fatalf("latches=%d err=%v", latches, err)
	}
	// Simulate another failed route after a process restart. A fresh Store
	// wrapper must still see the durable limit; no in-memory flag grants it.
	if _, err := js.S.DB().Exec(`UPDATE human_actions SET status='resolved' WHERE id=?`, open[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := js.OpenHumanAction(ctx, id, "manual_download", "still wrong", Access(true, "landing_page"), WithHumanActionDiagnosis(DiagnosisReasonWrongWork))
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Store{S: js.S}
	if _, err := restarted.RetryPublisherHandoff(ctx, second, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("second retry=%v", err)
	}
}

func TestPublisherRetryProfileGateScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gate      HumanGateType
		allowed   bool
		dependent bool
	}{
		{name: "terms at another provider", gate: HumanGateTermsRequired, allowed: true},
		{name: "terms for this job", gate: HumanGateTermsRequired, dependent: true},
		{name: "login", gate: HumanGateLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			js := testStore(t)
			id, action := publisherFailure(t, js)
			profile := institutionalProfile(t, js, testPolicy().Resolver, "digest", "auth")
			gate := gateObservation("other-provider-gate", tc.gate, HumanGateScopeInstitutionProfile, profile.ID, 1)
			gate.InstitutionProfileID = profile.ID
			if tc.dependent {
				gate.DependentJobIDs = []string{id}
			}
			if err := js.UpsertHumanGateObservation(ctx, gate); err != nil {
				t.Fatal(err)
			}

			got, err := js.RetryPublisherHandoff(ctx, action, 1)
			if tc.allowed {
				if err != nil || got != id {
					t.Fatalf("retry under unrelated terms gate = %q, %v; want job %q", got, err, id)
				}
				return
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("retry under %s gate = %q, %v; want ErrConflict", tc.gate, got, err)
			}
		})
	}
}

func TestPublisherRetryRefusesUnsafeOrStaleActions(t *testing.T) {
	for _, scenario := range []string{"revision", "no_doi", "invalid_doi", "sign_in", "terms", "identity", "other_action", "lease", "terminal", "held_effect", "unknown_effect", "profile_gate", "platform_gate", "live_claim"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			js := testStore(t)
			id, action := publisherFailure(t, js)
			revision := int64(1)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := js.S.DB().Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "revision":
				revision = 2
			case "no_doi":
				exec(`DELETE FROM identifiers`)
			case "invalid_doi":
				exec(`UPDATE identifiers SET value='javascript:bad' WHERE kind='doi'`)
			case "sign_in":
				exec(`UPDATE human_actions SET kind='human_auth_required' WHERE id=?`, action)
			case "terms":
				exec(`UPDATE human_actions SET kind='terms_acceptance_required' WHERE id=?`, action)
			case "identity":
				exec(`UPDATE human_actions SET diagnosis='adopted_pdf_failed_validation' WHERE id=?`, action)
			case "other_action":
				if _, err := js.OpenHumanAction(ctx, id, "human_auth_required", "sign in", Access(true, "paywall")); err != nil {
					t.Fatal(err)
				}
			case "lease":
				exec(`UPDATE jobs SET lease_owner='adopter',lease_expires_at='2099-01-01T00:00:00Z' WHERE id=?`, id)
			case "terminal":
				exec(`UPDATE jobs SET state='ready' WHERE id=?`, id)
			case "held_effect", "unknown_effect":
				busy := permitJob(t, js, "busy-download")
				permit := acquireDrive(t, js, driveIdentity(busy, "in-flight", 0, "generic"), "institution:example.edu", time.Now().Add(time.Minute))
				exec(`UPDATE effect_permits SET lease_until='2000-01-01T00:00:00Z' WHERE id=?`, permit.ID)
				if scenario == "unknown_effect" {
					exec(`UPDATE effect_permits SET status='unknown_completion' WHERE id=?`, permit.ID)
				}
			case "profile_gate":
				profile := institutionalProfile(t, js, "institute", "digest", "auth")
				exec(`INSERT INTO human_gate_observations(id,gate_type,scope_class,scope_key,institution_profile_id,observation_revision,status,created_at,updated_at) VALUES('gate','human_gate.captcha_or_security','institution_profile',?, ?,1,'open','now','now')`, profile.ID, profile.ID)
			case "platform_gate":
				exec(`INSERT INTO human_gate_observations(id,gate_type,scope_class,scope_key,observation_revision,status,created_at,updated_at) VALUES('gate','human_gate.downloads_folder_permission','platform','downloads',1,'open','now','now')`)
			case "live_claim":
				profile := institutionalProfile(t, js, "institute", "digest", "auth")
				candidate := institutionalCandidate(t, js, profile, "live", id)
				if _, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := js.RetryPublisherHandoff(ctx, action, revision); !errors.Is(err, ErrConflict) {
				t.Fatalf("retry=%v", err)
			}
			var status string
			if err := js.S.DB().QueryRow(`SELECT status FROM human_actions WHERE id=?`, action).Scan(&status); err != nil || status != "open" {
				t.Fatalf("failed action=%s err=%v", status, err)
			}
			if attempt, err := js.MaterializationAttemptRevision(ctx, id); err != nil || attempt != 1 {
				t.Fatalf("attempt=%d err=%v", attempt, err)
			}
		})
	}
}

func TestPublisherRetryRollsBackWholeReplacement(t *testing.T) {
	js := testStore(t)
	id, action := publisherFailure(t, js)
	if _, err := js.S.DB().Exec(`CREATE TRIGGER fail_retry BEFORE INSERT ON events WHEN NEW.kind='job.retry_requested' BEGIN SELECT RAISE(ABORT,'injected retry failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := js.RetryPublisherHandoff(context.Background(), action, 1); err == nil {
		t.Fatal("injected failure ignored")
	}
	open, err := js.ListOpenHumanActionsForJobs(context.Background(), []string{id})
	if err != nil || len(open) != 1 || open[0].ID != action {
		t.Fatalf("partial replacement=%+v err=%v", open, err)
	}
	var count int
	if err := js.S.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE kind='browser.publisher_retry_requested'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial event=%d err=%v", count, err)
	}
}

func TestPublisherRetryDiagnosisUsesCurrentAction(t *testing.T) {
	action := HumanAction{ID: 42, JobID: "job_publisher", Kind: "openurl_handoff", Status: "open", Detail: PublisherHandoffDetail}
	diagnosis := classifyAction(action, "wrong_work", "the resolver reached a different work")
	if diagnosis.Reason != DiagnosisReasonInProgress || diagnosis.Source != "action" || !diagnosis.NeedsBrowser || !diagnosis.CanOpenAction {
		t.Fatalf("publisher retry inherited obsolete failure guidance: %+v", diagnosis)
	}
	if diagnosis.Why != "an explicit publisher retry is pending through the paper's DOI" {
		t.Fatalf("why=%q", diagnosis.Why)
	}
}
