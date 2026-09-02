// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"testing"

	"papio/internal/delivery"
	"papio/internal/job"
)

// A confirm-absent verdict that loses its race must leave nothing behind.
// The cancellation and the action repair are one transaction, so a repair
// that refuses — because a concurrent verdict already moved the job on —
// takes the row cancellation down with it. Committing the cancellation
// separately, as this path used to, let a losing operator decision cancel a
// delivery request the winning decision had just confirmed as live.
//
// The interleaving is modelled by its outcome: an open document_delivery
// action on a job that is no longer awaiting_human is exactly the state the
// winner leaves behind, and RepairAwaitingHuman refuses it.
func TestDeliveryConfirmRequestAbsentRollsBackWhenTheRepairLoses(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	ctx := context.Background()
	jobID := deliveryTestJob(t, system, "req_absent_rollback", "10.1234/absent-rollback")
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	before, err := system.App.Delivery.GetByJobID(ctx, jobID)
	if err != nil || before == nil {
		t.Fatalf("before GetByJobID: %v %v", before, err)
	}

	// Stand in for the winning verdict: it closed the reconciliation action
	// and moved the job out of awaiting_human.
	actions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var openID int64
	for _, a := range actions {
		if a.Action.Status == "open" {
			openID = a.Action.ID
		}
	}
	if openID == 0 {
		t.Fatalf("no open action after submit; actions = %+v", actions)
	}
	if err := system.Jobs.RepairAwaitingHuman(ctx, jobID, []int64{openID},
		map[string]any{"reason": "test_winner_closed_the_action"}); err != nil {
		t.Fatal(err)
	}
	// The stale loser still holds an open action to act on.
	if _, err := system.Jobs.OpenHumanAction(ctx, jobID, job.ActionKindDocumentDelivery, "stale reconciliation prompt", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	if rpcErr := callMethod(t, router, "delivery.action", map[string]any{"job_id": jobID, "operation": "confirm_request_absent"}, nil); rpcErr == nil {
		t.Fatal("confirm_request_absent succeeded, want it to fail against a job the winner already moved on")
	}

	after, err := system.App.Delivery.GetByJobID(ctx, jobID)
	if err != nil || after == nil {
		t.Fatalf("after GetByJobID: %v %v", after, err)
	}
	if after.State == delivery.StateCancelled && before.State != delivery.StateCancelled {
		t.Fatalf("row state = %s after a losing confirm-absent, want %s preserved: the cancellation must roll back with its repair", after.State, before.State)
	}
}
