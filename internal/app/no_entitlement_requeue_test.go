// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
)

func resolvingExhaustionJob(t *testing.T, svc *Service, jobs *job.Store, requestID string) *job.Row {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequest(requestID))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func handoffActionsForJob(t *testing.T, jobs *job.Store, jobID string) []job.HumanAction {
	t.Helper()
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var handoffs []job.HumanAction
	for _, action := range actions {
		if action.JobID == jobID && action.Kind == "openurl_handoff" {
			handoffs = append(handoffs, action)
		}
	}
	return handoffs
}

func TestExhaustedCandidatesSkipsProvenEmptyInstitutionalRoute(t *testing.T) {
	ctx := context.Background()

	t.Run("requeue event makes direct exhaustion unavailable", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
		row := resolvingExhaustionJob(t, svc, jobs, "request_empty_institutional")
		if err := jobs.RecordEvent(ctx, row.ID, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
			t.Fatal(err)
		}

		if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates", ""); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateUnavailable || persisted.TerminalReason != "no_entitlement" {
			t.Fatalf("state/reason = %s/%q, want unavailable/no_entitlement", persisted.State, persisted.TerminalReason)
		}
		if actions := handoffActionsForJob(t, jobs, row.ID); len(actions) != 0 {
			t.Fatalf("proved-empty institutional route reopened handoffs: %+v", actions)
		}
	})

	t.Run("requeue event preserves OA browser handoff", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
		row := resolvingExhaustionJob(t, svc, jobs, "request_empty_institutional_oa")
		if err := jobs.RecordEvent(ctx, row.ID, "browser.no_entitlement_requeue", map[string]any{"outcome": "no_entitlement"}); err != nil {
			t.Fatal(err)
		}

		const oaURL = "https://oa.example.org/alternate.pdf"
		if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "candidates_exhausted", "all candidates exhausted", oaURL); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("state = %s, want awaiting_human", persisted.State)
		}
		actions := handoffActionsForJob(t, jobs, row.ID)
		if len(actions) != 1 || actions[0].Detail != OABrowserHandoffActionDetail(oaURL) {
			t.Fatalf("OA handoff actions = %+v, want one alternate-version handoff", actions)
		}
	})

	t.Run("without requeue event institutional handoff remains available", func(t *testing.T) {
		svc, jobs := newTestService(t)
		svc.Config.AccessMode = config.ModeDelegated
		svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
		row := resolvingExhaustionJob(t, svc, jobs, "request_fresh_institutional")

		if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates", ""); err != nil {
			t.Fatal(err)
		}
		persisted, err := jobs.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != job.StateAwaitingHuman {
			t.Fatalf("state = %s, want awaiting_human", persisted.State)
		}
		actions := handoffActionsForJob(t, jobs, row.ID)
		if len(actions) != 1 || actions[0].Detail != InstitutionalOpenURLHandoffDetail {
			t.Fatalf("institutional handoff actions = %+v, want one institutional handoff", actions)
		}
	})
}
