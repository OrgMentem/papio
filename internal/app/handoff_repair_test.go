// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

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
