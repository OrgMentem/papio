// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
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
			job.WithAccessClassification(true, "paywall")); err != nil {
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
			job.WithAccessClassification(true, "paywall")); err != nil {
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
			job.WithAccessClassification(false, "anti_bot")); err != nil {
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
		job.WithAccessClassification(true, "paywall"))
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
			job.WithAccessClassification(true, "paywall")); err != nil {
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
