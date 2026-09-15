// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"papio/internal/delivery"
	"papio/internal/job"
)

// TestCancelReportsThatTheCancellationCommittedWhenCompensationFails pins the
// operator-visible half of the cancellation compensation. app.Service cancels
// the job durably and only then releases the document-delivery request it was
// driving, so a release failure arrives AFTER the cancel has committed.
//
// failure()'s default arm answers "operation failed" for every unclassified
// error, and that message is a trap here: the obvious response to it is to
// re-run the cancel, which now conflicts against the cancellation its own
// first attempt recorded. The RPC must therefore say that the recorded change
// stands. Deleting the app.ErrCompensationIncomplete arm from failure(), or
// dropping the sentinel from the app-layer error, fails this test.
func TestCancelReportsThatTheCancellationCommittedWhenCompensationFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
	}{
		{name: "jobs_cancel", method: "jobs.cancel"},
		{name: "actions_dismiss", method: "actions.dismiss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system := deliveryTestSystem(t)
			router := Router(system)
			ctx := context.Background()
			jobID := deliveryTestJob(t, system, "req_cancel_comp_"+tc.name, "10.1234/cancel-comp-"+tc.name)

			request, err := system.App.Delivery.Create(ctx, delivery.CreateRequest{
				JobID: jobID, InstitutionProfile: "default", Provider: "illiad",
				RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1234/cancel-comp-" + tc.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Only a submitted or pending row is live enough to need orphaning,
			// which is what puts the compensation on this path at all.
			if err := system.App.Delivery.UpdateState(ctx, request.ID, delivery.StateSubmitted); err != nil {
				t.Fatal(err)
			}

			params := map[string]any{"job_id": jobID}
			if tc.method == "actions.dismiss" {
				if _, err := system.Jobs.OpenHumanAction(ctx, jobID, job.ActionKindDocumentDelivery,
					"a document-delivery request needs reconciliation", job.Access(false, "")); err != nil {
					t.Fatal(err)
				}
				if err := system.Jobs.Transition(ctx, jobID, job.StateResolving, job.StateAwaitingHuman,
					map[string]any{"reason": "document_delivery_reconciliation"}); err != nil {
					t.Fatal(err)
				}
				actions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
				if err != nil {
					t.Fatal(err)
				}
				if len(actions) != 1 {
					t.Fatalf("open actions = %d, want 1", len(actions))
				}
				params = map[string]any{
					"action_id":         actions[0].Action.ID,
					"expected_revision": actions[0].Action.Revision,
				}
			}

			orphanFailed := errors.New("injected: the delivery row could not be orphaned")
			system.App.Delivery.SetBeforeOrphanCASForTest(func() error { return orphanFailed })
			t.Cleanup(func() { system.App.Delivery.SetBeforeOrphanCASForTest(nil) })

			rpcErr := callMethod(t, router, tc.method, params, nil)
			if rpcErr == nil {
				t.Fatal("compensation failure returned no error: the stranded delivery request would be silent")
			}
			if rpcErr.Message == "operation failed" {
				t.Fatal("message = \"operation failed\": indistinguishable from the cancel itself failing, " +
					"so an operator retries a verb that already committed")
			}
			if !strings.Contains(rpcErr.Message, "recorded change stands") {
				t.Fatalf("message = %q, want it to state that the recorded change stands", rpcErr.Message)
			}
			// The wrapped cause is database detail this seam does not disclose.
			if strings.Contains(rpcErr.Message, orphanFailed.Error()) {
				t.Fatalf("message = %q, want the injected cause withheld from the caller", rpcErr.Message)
			}

			// The verb really did commit: that is what makes retrying it wrong.
			row, err := system.Jobs.Get(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.method == "jobs.cancel" && row.State != job.StateCancelled {
				t.Fatalf("job state = %q, want cancelled despite the compensation failure", row.State)
			}
			if tc.method == "actions.dismiss" {
				actions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
				if err != nil {
					t.Fatal(err)
				}
				for _, action := range actions {
					if action.Action.Status == "open" {
						t.Fatal("a human action is still open: the dismissal did not commit")
					}
				}
			}
		})
	}
}
