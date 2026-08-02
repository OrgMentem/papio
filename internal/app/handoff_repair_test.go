// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
)

func parkedHandoffJob(t *testing.T, svc *Service, jobs *job.Store, requestID string) *job.Row {
	t.Helper()
	row := resolvingExhaustionJob(t, svc, jobs, requestID)
	if err := jobs.Transition(context.Background(), row.ID, job.StateResolving, job.StateAwaitingHuman,
		map[string]any{"reason": "institutional_handoff"}); err != nil {
		t.Fatal(err)
	}
	return row
}

func strandedNeedsReviewJob(t *testing.T, svc *Service, jobs *job.Store, requestID string) *job.Row {
	t.Helper()
	row := resolvingExhaustionJob(t, svc, jobs, requestID)
	if err := jobs.Transition(context.Background(), row.ID, job.StateResolving, job.StateNeedsReview,
		map[string]any{"reason": "ui_changed"}); err != nil {
		t.Fatal(err)
	}
	return row
}

func setHandoffRepairUpdatedAt(t *testing.T, jobs *job.Store, jobID string, at time.Time) {
	t.Helper()
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET updated_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffRepairerHealsStrandedParks(t *testing.T) {
	ctx := context.Background()

	t.Run("orphaned park with no open action re-enters resolving", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		row := parkedHandoffJob(t, svc, jobs, "request_orphaned_park")

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateResolving {
			t.Fatalf("state = %s, want resolving", persisted.State)
		}
	})

	t.Run("contradicted park resolves the dead route and terminalizes", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
		row := parkedHandoffJob(t, svc, jobs, "request_contradicted_park")
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
			job.Access(true, "paywall")); err != nil {
			t.Fatal(err)
		}
		if err := jobs.RecordEvent(ctx, row.ID, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
			t.Fatal(err)
		}

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateResolving {
			t.Fatalf("state = %s, want resolving", persisted.State)
		}
		if actions := handoffActionsForJob(t, jobs, row.ID); len(actions) != 0 {
			t.Fatalf("proven-empty institutional action left open: %+v", actions)
		}

		// The healed job reaches exhaustion, observes the durable event, and
		// parks terminally instead of re-offering the dead route.
		persisted, err = jobs.Get(ctx, persisted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.exhaustedCandidates(ctx, persisted, job.StateResolving, "no_legal_candidates", "no legal candidates", ""); err != nil {
			t.Fatal(err)
		}
		persisted, err = jobs.Get(ctx, persisted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateUnavailable || persisted.TerminalReason != "no_entitlement" {
			t.Fatalf("state/reason = %s/%q, want unavailable/no_entitlement", persisted.State, persisted.TerminalReason)
		}
	})

	t.Run("fresh institutional park is untouched", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		row := parkedHandoffJob(t, svc, jobs, "request_fresh_park")
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
			job.Access(true, "paywall")); err != nil {
			t.Fatal(err)
		}

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("state = %s, want awaiting_human", persisted.State)
		}
		if actions := handoffActionsForJob(t, jobs, row.ID); len(actions) != 1 {
			t.Fatalf("fresh institutional action count = %d, want 1", len(actions))
		}
	})

	t.Run("non-institutional open action holds the park despite the event", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		row := parkedHandoffJob(t, svc, jobs, "request_oa_park")
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", OABrowserHandoffActionDetail("https://oa.example.org/alt.pdf"),
			job.Access(false, "anti_bot")); err != nil {
			t.Fatal(err)
		}
		if err := jobs.RecordEvent(ctx, row.ID, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
			t.Fatal(err)
		}

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("state = %s, want awaiting_human", persisted.State)
		}
		if actions := handoffActionsForJob(t, jobs, row.ID); len(actions) != 1 {
			t.Fatalf("OA action count = %d, want 1 still open", len(actions))
		}
	})
}

func TestHandoffRepairerHealsStrandedNeedsReview(t *testing.T) {
	ctx := context.Background()

	t.Run("old review with no action becomes an adoptable manual download", func(t *testing.T) {
		svc, jobs := newTestService(t)
		now := time.Now().UTC()
		svc.Now = func() time.Time { return now }
		row := strandedNeedsReviewJob(t, svc, jobs, "request_stranded_review")
		setHandoffRepairUpdatedAt(t, jobs, row.ID, now.Add(-strandedNeedsReviewMinAge-time.Second))

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("state = %s, want awaiting_human", persisted.State)
		}
		actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 1 || actions[0].Kind != "manual_download" || !actions[0].RequiresAuth {
			t.Fatalf("open actions = %+v, want one auth-requiring manual_download", actions)
		}
	})

	t.Run("replacement inherits the resolved handoff's sign-in requirement", func(t *testing.T) {
		for _, requiresAuth := range []bool{true, false} {
			t.Run(fmt.Sprintf("requires_auth_%t", requiresAuth), func(t *testing.T) {
				svc, jobs := newTestService(t)
				now := time.Now().UTC()
				svc.Now = func() time.Time { return now }
				row := resolvingExhaustionJob(t, svc, jobs, fmt.Sprintf("request_resolved_handoff_%t", requiresAuth))
				olderHandoffID, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
					job.Access(!requiresAuth, "paywall"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := jobs.S.DB().ExecContext(ctx,
					`UPDATE human_actions SET status = 'resolved', resolved_at = ? WHERE id = ?`,
					now.Add(-time.Second).Format(time.RFC3339Nano), olderHandoffID); err != nil {
					t.Fatal(err)
				}
				handoffID, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
					job.Access(requiresAuth, "paywall"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := jobs.S.DB().ExecContext(ctx,
					`UPDATE human_actions SET status = 'resolved', resolved_at = ? WHERE id = ?`,
					now.Format(time.RFC3339Nano), handoffID); err != nil {
					t.Fatal(err)
				}
				if err := jobs.Transition(ctx, row.ID, job.StateResolving, job.StateNeedsReview,
					map[string]any{"reason": "ui_changed"}); err != nil {
					t.Fatal(err)
				}
				setHandoffRepairUpdatedAt(t, jobs, row.ID, now.Add(-strandedNeedsReviewMinAge-time.Second))

				if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
					t.Fatal(err)
				}
				actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
				if err != nil {
					t.Fatal(err)
				}
				if len(actions) != 1 || actions[0].Kind != "manual_download" {
					t.Fatalf("open actions = %+v, want one manual_download", actions)
				}
				if actions[0].RequiresAuth != requiresAuth {
					t.Fatalf("manual_download requires_auth = %t, want %t", actions[0].RequiresAuth, requiresAuth)
				}
			})
		}
	})

	t.Run("fresh review with no action is left alone", func(t *testing.T) {
		svc, jobs := newTestService(t)
		now := time.Now().UTC()
		svc.Now = func() time.Time { return now }
		row := strandedNeedsReviewJob(t, svc, jobs, "request_fresh_review")
		setHandoffRepairUpdatedAt(t, jobs, row.ID, now.Add(-strandedNeedsReviewMinAge+time.Second))

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateNeedsReview {
			t.Fatalf("state = %s, want needs_review", persisted.State)
		}
		actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 0 {
			t.Fatalf("fresh review gained actions: %+v", actions)
		}
	})

	t.Run("identity review action holds the review park", func(t *testing.T) {
		svc, jobs := newTestService(t)
		now := time.Now().UTC()
		svc.Now = func() time.Time { return now }
		row := strandedNeedsReviewJob(t, svc, jobs, "request_identity_review")
		setHandoffRepairUpdatedAt(t, jobs, row.ID, now.Add(-strandedNeedsReviewMinAge-time.Second))
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "verify_identity", "inspect the quarantined download", job.Access(false, "")); err != nil {
			t.Fatal(err)
		}

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateNeedsReview {
			t.Fatalf("state = %s, want needs_review", persisted.State)
		}
		actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 1 || actions[0].Kind != "verify_identity" {
			t.Fatalf("open actions = %+v, want one verify_identity", actions)
		}
	})

	t.Run("leased review is left alone", func(t *testing.T) {
		svc, jobs := newTestService(t)
		now := time.Now().UTC()
		svc.Now = func() time.Time { return now }
		row := strandedNeedsReviewJob(t, svc, jobs, "request_leased_review")
		setHandoffRepairUpdatedAt(t, jobs, row.ID, now.Add(-strandedNeedsReviewMinAge-time.Second))
		if _, err := jobs.S.DB().ExecContext(ctx,
			`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ?`,
			"adopt-in-progress", now.Add(time.Minute).Format(time.RFC3339Nano), row.ID); err != nil {
			t.Fatal(err)
		}

		if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateNeedsReview || !persisted.LeaseActive(now) {
			t.Fatalf("leased review = %+v, want untouched needs_review job", persisted)
		}
		actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 0 {
			t.Fatalf("leased review gained actions: %+v", actions)
		}
	})
}
func TestHandoffRepairerLeavesAnActivelyLeasedParkUntouched(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row := parkedHandoffJob(t, svc, jobs, "request_leased_park")
	held, err := svc.leaseAwaitingHuman(ctx, row.ID, "adopt-in-progress", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("precondition: adoption lease was not acquired")
	}

	if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := jobs.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != job.StateAwaitingHuman || !persisted.LeaseActive(time.Now()) {
		t.Fatalf("leased park = %+v, want untouched awaiting_human job", persisted)
	}
}

func TestHandoffRepairerLeavesLateAdoptionLeaseUntouched(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row := parkedHandoffJob(t, svc, jobs, "request_late_adoption_lease")
	actionID, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
		job.Access(true, "paywall"))
	if err != nil {
		t.Fatal(err)
	}

	// Capture the exact maintenance snapshot before the adopter wins its CAS.
	rows, err := jobs.ListOldest(ctx, []string{job.StateAwaitingHuman}, job.ListLimitMax)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{rows[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	held, err := svc.leaseAwaitingHuman(ctx, row.ID, "adopt-in-progress", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("precondition: adoption lease was not acquired")
	}

	err = jobs.RepairAwaitingHuman(ctx, rows[0].ID, []int64{actions[0].ID},
		map[string]any{"reason": "unfetchable_handoff_repair"})
	if !errors.Is(err, job.ErrConflict) {
		t.Fatalf("repair after adoption lease = %v, want ErrConflict", err)
	}
	persisted, err := jobs.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != job.StateAwaitingHuman || !persisted.LeaseActive(time.Now()) {
		t.Fatalf("late lease did not protect parked job: %+v", persisted)
	}
	open, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range open {
		if action.ID == actionID {
			return
		}
	}
	t.Fatalf("late adoption lease let repair close action %d: %+v", actionID, open)
}

func TestHandoffRepairerReachesOldestParkBeyondMaintenancePage(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	oldest := parkedHandoffJob(t, svc, jobs, "request_oldest_orphan")
	now := time.Now().UTC()
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
		now.Add(-48*time.Hour).Format(time.RFC3339Nano), oldest.ID); err != nil {
		t.Fatal(err)
	}
	for i := range job.ListLimitMax {
		row := parkedHandoffJob(t, svc, jobs, fmt.Sprintf("request_newer_handoff_%03d", i))
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
			job.Access(true, "paywall")); err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			now.Add(time.Duration(i+1)*time.Second).Format(time.RFC3339Nano), row.ID); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := jobs.Get(ctx, oldest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != job.StateResolving {
		t.Fatalf("oldest park state = %s, want resolving despite newer full page", persisted.State)
	}
}

func providerUpgradePark(t *testing.T, svc *Service, jobs *job.Store, requestID, extensionVersion, adapterVersion string) *job.Row {
	t.Helper()
	ctx := context.Background()
	row := parkedHandoffJob(t, svc, jobs, requestID)
	if _, err := jobs.OpenHumanAction(ctx, row.ID, "manual_download", "download the requested PDF yourself", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, row.ID, "browser.provider_outcome", map[string]any{
		"outcome":           "ui_changed",
		"adapter_version":   adapterVersion,
		"extension_version": extensionVersion,
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestHandoffRepairerRepairsAdapterUpgradeParks(t *testing.T) {
	ctx := context.Background()
	newer := func(previous, current string) bool {
		return previous == "0.7.0" && current == "0.8.0"
	}

	t.Run("retries once and records the extension upgrade", func(t *testing.T) {
		svc, jobs := newTestService(t)
		row := providerUpgradePark(t, svc, jobs, "request_adapter_upgrade_once", "0.7.0", "0.1.0")

		if err := svc.HandoffRepairer().RepairAdapterUpgrade(ctx, "0.8.0", newer); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateResolving {
			t.Fatalf("state = %s, want resolving", persisted.State)
		}
		open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 0 {
			t.Fatalf("manual action left open after repair: %+v", open)
		}
		events, err := jobs.Events(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		var repairDetail map[string]any
		for _, event := range events {
			if event["kind"] != "job.transition" {
				continue
			}
			detail, _ := event["detail"].(map[string]any)
			if detail["reason"] == adapterUpgradeRepairReason {
				repairDetail = detail
			}
		}
		if repairDetail == nil ||
			repairDetail["old_extension_version"] != "0.7.0" ||
			repairDetail["new_extension_version"] != "0.8.0" ||
			repairDetail["adapter_version"] != "0.1.0" {
			t.Fatalf("adapter-upgrade repair event = %#v", repairDetail)
		}

		if err := jobs.Transition(ctx, row.ID, job.StateResolving, job.StateAwaitingHuman,
			map[string]any{"reason": "provider_repark"}); err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.OpenHumanAction(ctx, row.ID, "manual_download", "download the requested PDF yourself", job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		if err := svc.HandoffRepairer().RepairAdapterUpgrade(ctx, "0.8.0", newer); err != nil {
			t.Fatal(err)
		}
		persisted, err = jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("same extension upgrade retried again: state = %s", persisted.State)
		}
		open, err = jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 1 || open[0].Kind != "manual_download" {
			t.Fatalf("same extension upgrade changed open actions: %+v", open)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, svc *Service, jobs *job.Store, row *job.Row)
		kind  string
	}{
		{
			name: "active adoption lease",
			setup: func(t *testing.T, svc *Service, _ *job.Store, row *job.Row) {
				t.Helper()
				held, err := svc.leaseAwaitingHuman(ctx, row.ID, "adopt-in-progress", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if !held {
					t.Fatal("precondition: adoption lease was not acquired")
				}
			},
			kind: "manual_download",
		},
		{
			name: "proven-empty institutional route",
			setup: func(t *testing.T, _ *Service, jobs *job.Store, row *job.Row) {
				t.Helper()
				if err := jobs.RecordEvent(ctx, row.ID, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
					t.Fatal(err)
				}
			},
			kind: "manual_download",
		},
		{
			name: "identity review action",
			setup: func(t *testing.T, _ *Service, jobs *job.Store, row *job.Row) {
				t.Helper()
				actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
				if err != nil {
					t.Fatal(err)
				}
				if len(actions) != 1 {
					t.Fatalf("manual action count = %d, want 1", len(actions))
				}
				if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "resolved"); err != nil {
					t.Fatal(err)
				}
				if _, err := jobs.OpenHumanAction(ctx, row.ID, "verify_identity", "inspect the quarantined download", job.Access(false, "")); err != nil {
					t.Fatal(err)
				}
			},
			kind: "verify_identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, jobs := newTestService(t)
			row := providerUpgradePark(t, svc, jobs, "request_adapter_upgrade_"+strings.ReplaceAll(tc.name, " ", "_"), "0.7.0", "0.1.0")
			tc.setup(t, svc, jobs, row)

			if err := svc.HandoffRepairer().RepairAdapterUpgrade(ctx, "0.8.0", newer); err != nil {
				t.Fatal(err)
			}
			persisted, err := jobs.Get(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.State != job.StateAwaitingHuman {
				t.Fatalf("state = %s, want awaiting_human", persisted.State)
			}
			open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(open) != 1 || open[0].Kind != tc.kind {
				t.Fatalf("open actions = %+v, want one %s", open, tc.kind)
			}
		})
	}
}
