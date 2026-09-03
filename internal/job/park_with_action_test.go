// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ParkWithHumanAction is the inverse of RepairAwaitingHuman: it opens the
// prompt and parks the job in one transaction. These tests inject SQLite
// trigger failures on each half and assert the other half did not land, which
// is the whole reason the two writes were merged.

package job

import (
	"context"
	"errors"
	"testing"
)

func resolvingParkCandidate(t *testing.T, js *Store, requestID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// humanActionRows counts every human_actions row for a job regardless of
// status: a rolled-back open must leave no row at all, not a row parked in
// some other status.
func humanActionRows(t *testing.T, js *Store, jobID string) int {
	t.Helper()
	var count int
	if err := js.S.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM human_actions WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestParkWithHumanActionDiscardsActionWhenTransitionFails(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "wr_park_action_transition_fails")

	if _, err := js.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_park_transition
		BEFORE UPDATE OF state ON jobs
		WHEN NEW.state = 'awaiting_human'
		BEGIN SELECT RAISE(ABORT, 'injected park failure'); END`); err != nil {
		t.Fatal(err)
	}

	err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "a document-delivery request needs reconciliation",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, ""))
	if err == nil {
		t.Fatal("park succeeded despite the injected transition failure")
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateResolving {
		t.Fatalf("state = %s, want resolving (the failed transition must not land)", row.State)
	}
	if rows := humanActionRows(t, js, id); rows != 0 {
		t.Fatalf("human_actions rows = %d, want 0: the failed transition left a stale prompt behind", rows)
	}
	open, err := js.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open actions = %+v, want none", open)
	}

	// The trigger, not a broken call, is what refused the park: the same
	// arguments must succeed once it is gone.
	if _, err := js.S.DB().ExecContext(ctx, `DROP TRIGGER reject_park_transition`); err != nil {
		t.Fatal(err)
	}
	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "a document-delivery request needs reconciliation",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, "")); err != nil {
		t.Fatalf("park after dropping the trigger: %v", err)
	}
}

func TestParkWithHumanActionLeavesJobUnparkedWhenActionOpenFails(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "wr_park_action_open_fails")

	if _, err := js.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_park_action_open
		BEFORE INSERT ON human_actions
		WHEN NEW.kind = 'document_delivery'
		BEGIN SELECT RAISE(ABORT, 'injected action failure'); END`); err != nil {
		t.Fatal(err)
	}

	err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "a document-delivery request needs reconciliation",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, ""))
	if err == nil {
		t.Fatal("park succeeded despite the injected action failure")
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateResolving {
		t.Fatalf("state = %s, want resolving: a job parked without a prompt has nothing for a human to answer", row.State)
	}
	if rows := humanActionRows(t, js, id); rows != 0 {
		t.Fatalf("human_actions rows = %d, want 0", rows)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		kind, _ := event["kind"].(string)
		if kind != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if to, _ := detail["to"].(string); to == StateAwaitingHuman {
			t.Fatalf("failed park still recorded a park transition event: %+v", event)
		}
	}
}

func TestParkWithHumanActionReusesItsOpenActionOnRepark(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "wr_park_action_repark")

	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "request TN-1 is pending",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, "")); err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateAwaitingHuman {
		t.Fatalf("state = %s, want awaiting_human", row.State)
	}
	if row.LeaseOwner != "" || row.LeaseExpiresAt != "" {
		t.Fatalf("park left a lease behind: owner=%q expires=%q", row.LeaseOwner, row.LeaseExpiresAt)
	}
	first, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("open actions = %+v, want exactly one", first)
	}
	if first[0].Kind != ActionKindDocumentDelivery || first[0].Detail != "request TN-1 is pending" || first[0].Revision != 1 {
		t.Fatalf("action = %+v", first[0])
	}

	// The scheduler re-drives the unleased park through resolving, which is
	// the only edge back: the reconciliation park then runs a second time on
	// the same job and must refresh its prompt rather than add another.
	if err := js.Transition(ctx, id, StateAwaitingHuman, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "request TN-1 was declined",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if rows := humanActionRows(t, js, id); rows != 1 {
		t.Fatalf("human_actions rows = %d, want 1: the re-park duplicated the prompt", rows)
	}
	second, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("open actions = %+v, want exactly one", second)
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("action id = %d, want the reused row %d", second[0].ID, first[0].ID)
	}
	if second[0].Detail != "request TN-1 was declined" || second[0].Revision != 2 {
		t.Fatalf("re-park did not refresh the reused row: %+v", second[0])
	}
	row, err = js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != StateAwaitingHuman {
		t.Fatalf("state after re-park = %s, want awaiting_human", row.State)
	}
}

// The state graph, not the caller, decides which parks are legal. An
// awaiting_human -> awaiting_human re-park is refused, so the caller must go
// back through resolving.
func TestParkWithHumanActionRefusesIllegalEdge(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id := resolvingParkCandidate(t, js, "wr_park_action_illegal_edge")

	if err := js.ParkWithHumanAction(ctx, id, StateResolving, StateAwaitingHuman,
		ActionKindDocumentDelivery, "request TN-2 is pending",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, "")); err != nil {
		t.Fatal(err)
	}
	err := js.ParkWithHumanAction(ctx, id, StateAwaitingHuman, StateAwaitingHuman,
		ActionKindDocumentDelivery, "request TN-2 is still pending",
		map[string]any{"reason": "document_delivery_reconciliation"}, Access(false, ""))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("awaiting_human -> awaiting_human err = %v, want ErrConflict", err)
	}
	actions, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Detail != "request TN-2 is pending" || actions[0].Revision != 1 {
		t.Fatalf("refused park still touched the prompt: %+v", actions)
	}
}
