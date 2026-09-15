// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"fmt"
	"testing"

	"papio/internal/job"
)

func TestJobsGetScopesHumanActionReadToRequestedJob(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()

	requestedJobID := createAPIAttributionTestJob(t, system.Jobs,
		"api-jobs-get-scope-requested", "10.1000/api-jobs-get-scope-requested", "")
	openActionID, err := system.Jobs.OpenHumanAction(ctx, requestedJobID,
		"requested_open", "requested open action", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open requested action: %v", err)
	}
	resolvedActionID, err := system.Jobs.OpenHumanAction(ctx, requestedJobID,
		"requested_resolved", "requested resolved action", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open second requested action: %v", err)
	}
	if err := system.Jobs.ResolveHumanAction(ctx, resolvedActionID, "resolved"); err != nil {
		t.Fatalf("resolve requested action: %v", err)
	}

	var poisonedActionID int64
	for jobIndex := range 12 {
		otherJobID := createAPIAttributionTestJob(t, system.Jobs,
			fmt.Sprintf("api-jobs-get-scope-other-%02d", jobIndex),
			fmt.Sprintf("10.1000/api-jobs-get-scope-other-%02d", jobIndex), "")
		for actionIndex := range 4 {
			actionID, err := system.Jobs.OpenHumanAction(ctx, otherJobID,
				fmt.Sprintf("other_%02d_%02d", jobIndex, actionIndex), "unrelated action", job.Access(false, ""))
			if err != nil {
				t.Fatalf("open unrelated action %d/%d: %v", jobIndex, actionIndex, err)
			}
			if poisonedActionID == 0 {
				poisonedActionID = actionID
			}
		}
	}

	// A global action read must scan this unrelated row and fail its integer
	// conversion. A job-scoped read never reads it. This makes the test detect
	// the data-access path rather than only the handler's output filtering.
	if _, err := system.Store.DB().ExecContext(ctx,
		`UPDATE human_actions SET revision = 'not-an-integer' WHERE id = ?`, poisonedActionID); err != nil {
		t.Fatalf("poison unrelated action: %v", err)
	}
	if _, err := system.Jobs.ListHumanActions(ctx, false); err == nil {
		t.Fatal("global action read unexpectedly ignored the poisoned unrelated row")
	}

	var detail JobDetail
	if rpcErr := callMethod(t, Router(system), "jobs.get",
		map[string]string{"job_id": requestedJobID}, &detail); rpcErr != nil {
		t.Fatalf("jobs.get returned %s: %s", rpcErr.Code, rpcErr.Message)
	}
	if len(detail.Actions) != 2 {
		t.Fatalf("jobs.get actions = %+v, want exactly the two requested-job actions", detail.Actions)
	}
	if detail.Actions[0].ID != resolvedActionID || detail.Actions[0].JobID != requestedJobID || detail.Actions[0].Status != "resolved" {
		t.Fatalf("first jobs.get action = %+v, want requested resolved action %d", detail.Actions[0], resolvedActionID)
	}
	if detail.Actions[1].ID != openActionID || detail.Actions[1].JobID != requestedJobID || detail.Actions[1].Status != "open" {
		t.Fatalf("second jobs.get action = %+v, want requested open action %d", detail.Actions[1], openActionID)
	}

	emptyJobID := createAPIAttributionTestJob(t, system.Jobs,
		"api-jobs-get-scope-empty", "10.1000/api-jobs-get-scope-empty", "")
	var emptyDetail JobDetail
	if rpcErr := callMethod(t, Router(system), "jobs.get",
		map[string]string{"job_id": emptyJobID}, &emptyDetail); rpcErr != nil {
		t.Fatalf("jobs.get for empty job returned %s: %s", rpcErr.Code, rpcErr.Message)
	}
	if emptyDetail.Actions == nil || len(emptyDetail.Actions) != 0 {
		t.Fatalf("jobs.get empty actions = %#v, want legacy non-nil empty list while unrelated actions exist", emptyDetail.Actions)
	}
}
