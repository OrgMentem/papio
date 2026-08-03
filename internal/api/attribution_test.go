// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func createAPIAttributionTestJob(t *testing.T, systemJobStore *job.Store, requestID, doi, consumer string) string {
	t.Helper()
	result, err := systemJobStore.CreateRequestForWork(context.Background(), requestID,
		work.Work{DOI: doi, Title: "Attribution test", Authors: []string{"Test, T."}, Year: 2026},
		"", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil,
		job.Attribution{Principal: job.PrincipalCLI, Consumer: consumer}, false)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	return result.JobID
}

func assertAPIAttributionJobRowShape(t *testing.T, object map[string]json.RawMessage, row job.Row, consumer string) {
	t.Helper()
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal expected job row: %v", err)
	}
	var expected map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &expected); err != nil {
		t.Fatalf("decode expected job row: %v", err)
	}
	keys := make([]string, 0, len(expected)+1)
	for key := range expected {
		keys = append(keys, key)
	}
	if consumer != "" {
		expected["consumer"] = json.RawMessage(fmt.Sprintf("%q", consumer))
		keys = append(keys, "consumer")
	}
	assertRatifiedKeySet(t, object, keys...)
	if consumer == "" {
		if _, ok := object["consumer"]; ok {
			t.Fatalf("unattributed job row carried a consumer key: %s", object)
		}
		return
	}
	var got string
	if err := json.Unmarshal(object["consumer"], &got); err != nil {
		t.Fatalf("decode consumer: %v", err)
	}
	if got != consumer {
		t.Fatalf("consumer = %q, want %q", got, consumer)
	}
}

func TestJobsListV3EnvelopeAndAttributionRowShape(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	attributedID := createAPIAttributionTestJob(t, system.Jobs, "api-list-v3-attributed", "10.1000/api-list-v3-attributed", "inscribi")
	unattributedID := createAPIAttributionTestJob(t, system.Jobs, "api-list-v3-unattributed", "10.1000/api-list-v3-unattributed", "")

	var envelope map[string]json.RawMessage
	if rpcErr := callMethod(t, Router(system), "jobs.list_v3", map[string]any{"limit": 10}, &envelope); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	assertRatifiedKeySet(t, envelope, "jobs", "truncated")
	var rawRows []json.RawMessage
	if err := json.Unmarshal(envelope["jobs"], &rawRows); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(rawRows) != 2 {
		t.Fatalf("jobs.list_v3 returned %d rows, want 2", len(rawRows))
	}
	seen := map[string]bool{}
	for _, rawRow := range rawRows {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(rawRow, &object); err != nil {
			t.Fatalf("decode job row: %v", err)
		}
		var id string
		if err := json.Unmarshal(object["id"], &id); err != nil {
			t.Fatalf("decode job id: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate job row %s", id)
		}
		seen[id] = true
		row, err := system.Jobs.Get(ctx, id)
		if err != nil {
			t.Fatalf("load job %s: %v", id, err)
		}
		switch id {
		case attributedID:
			assertAPIAttributionJobRowShape(t, object, *row, "inscribi")
		case unattributedID:
			assertAPIAttributionJobRowShape(t, object, *row, "")
		default:
			t.Fatalf("unexpected job row %s", id)
		}
	}
	if !seen[attributedID] || !seen[unattributedID] {
		t.Fatalf("jobs.list_v3 rows = %v, want attributed and unattributed jobs", seen)
	}
}

func TestJobsListV3HonoursConsumerFilter(t *testing.T) {
	system := testSystem(t)
	want := map[string]bool{}
	for i := 0; i < 2; i++ {
		id := createAPIAttributionTestJob(t, system.Jobs, fmt.Sprintf("api-list-v3-filter-a-%d", i),
			fmt.Sprintf("10.1000/api-list-v3-filter-a-%d", i), "A")
		want[id] = true
	}
	createAPIAttributionTestJob(t, system.Jobs, "api-list-v3-filter-b", "10.1000/api-list-v3-filter-b", "B")
	createAPIAttributionTestJob(t, system.Jobs, "api-list-v3-filter-none", "10.1000/api-list-v3-filter-none", "")

	var page JobsPageV3
	if rpcErr := callMethod(t, Router(system), "jobs.list_v3", map[string]any{"consumer": "A", "limit": 10}, &page); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(page.Jobs) != len(want) || page.Truncated {
		t.Fatalf("filtered jobs page = %d rows truncated %t, want %d/false", len(page.Jobs), page.Truncated, len(want))
	}
	for _, row := range page.Jobs {
		if !want[row.ID] {
			t.Fatalf("consumer A filter returned job %s", row.ID)
		}
		if row.Consumer == nil || *row.Consumer != "A" {
			t.Fatalf("filtered job %s consumer = %v, want A", row.ID, row.Consumer)
		}
		delete(want, row.ID)
	}
	if len(want) != 0 {
		t.Fatalf("consumer A filter omitted jobs: %v", want)
	}

}

func TestActionsListV3ReportsOpenStalenessAndNeverStalesResolvedActions(t *testing.T) {
	system := testSystem(t)
	system.Config.Actions.StaleAfterSeconds = 60
	ctx := context.Background()

	oldJobID := createAPIAttributionTestJob(t, system.Jobs, "api-actions-v3-old", "10.1000/api-actions-v3-old", "inscribi")
	freshJobID := createAPIAttributionTestJob(t, system.Jobs, "api-actions-v3-fresh", "10.1000/api-actions-v3-fresh", "inscribi")
	resolvedJobID := createAPIAttributionTestJob(t, system.Jobs, "api-actions-v3-resolved", "10.1000/api-actions-v3-resolved", "inscribi")
	oldActionID, err := system.Jobs.OpenHumanAction(ctx, oldJobID, "old_action", "old", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open old action: %v", err)
	}
	freshActionID, err := system.Jobs.OpenHumanAction(ctx, freshJobID, "fresh_action", "fresh", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open fresh action: %v", err)
	}
	resolvedActionID, err := system.Jobs.OpenHumanAction(ctx, resolvedJobID, "resolved_action", "resolved", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open resolved action: %v", err)
	}
	if err := system.Jobs.ResolveHumanAction(ctx, resolvedActionID, "resolved"); err != nil {
		t.Fatalf("resolve action: %v", err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	for _, actionID := range []int64{oldActionID, resolvedActionID} {
		if _, err := system.Store.DB().ExecContext(ctx, `UPDATE human_actions SET created_at = ? WHERE id = ?`, old, actionID); err != nil {
			t.Fatalf("age action %d: %v", actionID, err)
		}
	}

	var page ActionsPageV3
	if rpcErr := callMethod(t, Router(system), "actions.list_v3", map[string]any{"open_only": false, "limit": 20}, &page); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	byID := make(map[int64]ActionRow, len(page.Actions))
	for _, row := range page.Actions {
		byID[row.ID] = row
	}
	oldAction := byID[oldActionID]
	if oldAction.Status != "open" || !oldAction.Stale || oldAction.AgeSeconds < 3600 {
		t.Fatalf("old action = %+v, want open/stale with plausible age", oldAction)
	}
	freshAction := byID[freshActionID]
	if freshAction.Status != "open" || freshAction.Stale || freshAction.AgeSeconds >= 60 {
		t.Fatalf("fresh action = %+v, want open/fresh", freshAction)
	}
	resolvedAction := byID[resolvedActionID]
	if resolvedAction.Status != "resolved" || resolvedAction.Stale || resolvedAction.AgeSeconds < 3600 {
		t.Fatalf("resolved action = %+v, want resolved/non-stale with old age", resolvedAction)
	}

	var openPage ActionsPageV3
	if rpcErr := callMethod(t, Router(system), "actions.list_v3", map[string]any{"open_only": true, "limit": 20}, &openPage); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	for _, row := range openPage.Actions {
		if row.ID == resolvedActionID {
			t.Fatalf("resolved action %d appeared in open queue", resolvedActionID)
		}
	}
}

func TestJobsGetV2CarriesAttributionAndActionStaleness(t *testing.T) {
	system := testSystem(t)
	system.Config.Actions.StaleAfterSeconds = 60
	ctx := context.Background()
	jobID := createAPIAttributionTestJob(t, system.Jobs, "api-job-get-v2", "10.1000/api-job-get-v2", "inscribi")
	actionID, err := system.Jobs.OpenHumanAction(ctx, jobID, "old_action", "old", job.Access(false, ""))
	if err != nil {
		t.Fatalf("open action: %v", err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := system.Store.DB().ExecContext(ctx, `UPDATE human_actions SET created_at = ? WHERE id = ?`, old, actionID); err != nil {
		t.Fatalf("age action: %v", err)
	}

	var rawDetail map[string]json.RawMessage
	if rpcErr := callMethod(t, Router(system), "jobs.get_v2", map[string]string{"job_id": jobID}, &rawDetail); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var detail JobDetailV2
	encoded, err := json.Marshal(rawDetail)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &detail); err != nil {
		t.Fatalf("decode job detail: %v", err)
	}
	if detail.Job == nil || detail.Job.ID != jobID || detail.Job.Consumer == nil || *detail.Job.Consumer != "inscribi" {
		t.Fatalf("job detail job = %+v, want attributed %s", detail.Job, jobID)
	}
	row, err := system.Jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var rawJob map[string]json.RawMessage
	if err := json.Unmarshal(rawDetail["job"], &rawJob); err != nil {
		t.Fatalf("decode raw job: %v", err)
	}
	assertAPIAttributionJobRowShape(t, rawJob, *row, "inscribi")
	if len(detail.Actions) != 1 || detail.Actions[0].ID != actionID || !detail.Actions[0].Stale || detail.Actions[0].AgeSeconds < 3600 {
		t.Fatalf("job detail actions = %+v, want one stale old action", detail.Actions)
	}
}

func TestArtifactsValidationReturnsEvidenceNewestFirstAndEmptyWhenAbsent(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	jobID := createAPIAttributionTestJob(t, system.Jobs, "api-validation-reports", "10.1000/api-validation-reports", "inscribi")
	if inserted, err := system.Jobs.InsertCandidates(ctx, jobID, []job.Candidate{
		{JobID: jobID, Source: "fixture", URLRedacted: "https://example.test/one.pdf", URLKey: "one", Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 0},
		{JobID: jobID, Source: "fixture", URLRedacted: "https://example.test/two.pdf", URLKey: "two", Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 1},
	}); err != nil || inserted != 2 {
		t.Fatalf("insert candidates = %d, %v; want 2", inserted, err)
	}
	selected, err := system.Jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || selected == nil {
		t.Fatalf("selected candidate = %+v, %v", selected, err)
	}
	if err := system.Jobs.MarkCandidate(ctx, selected.ID, job.CandidateAccepted); err != nil {
		t.Fatal(err)
	}
	other, err := system.Jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || other == nil {
		t.Fatalf("other candidate = %+v, %v", other, err)
	}
	if err := system.Jobs.Transition(ctx, jobID, job.StateQueued, job.StateResolving, nil, job.WithCandidate(selected.ID)); err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	selectedDocument := `{"schema_version":"validation-report/test-selected","stage":"selected"}`
	otherDocument := `{"schema_version":"validation-report/test-other","stage":"other"}`
	if err := system.Jobs.RecordValidationReport(ctx, job.ValidationRecord{
		JobID: jobID, CandidateID: other.ID, SHA256: "sha-other", Outcome: "structure_rejected",
		RecordedAt: "2026-02-01T00:00:01Z", Document: otherDocument,
	}); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.RecordValidationReport(ctx, job.ValidationRecord{
		JobID: jobID, CandidateID: selected.ID, SHA256: "sha-selected", Outcome: "pass",
		RecordedAt: "2026-02-01T00:00:02Z", Document: selectedDocument,
	}); err != nil {
		t.Fatal(err)
	}

	var result ValidationResult
	if rpcErr := callMethod(t, Router(system), "artifacts.validation", map[string]string{"job_id": jobID}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.JobID != jobID || len(result.Reports) != 2 {
		t.Fatalf("validation result = %+v, want job %s and two reports", result, jobID)
	}
	first, second := result.Reports[0], result.Reports[1]
	if first.CandidateID != selected.ID || !first.Accepted || first.Document != selectedDocument || first.SchemaVersion != "validation-report/test-selected" {
		t.Fatalf("newest selected report = %+v, want selected evidence and declared schema", first)
	}
	if second.CandidateID != other.ID || second.Accepted || second.Document != otherDocument || second.SchemaVersion != "validation-report/test-other" {
		t.Fatalf("older other report = %+v, want rejected evidence and declared schema", second)
	}

	emptyID := createAPIAttributionTestJob(t, system.Jobs, "api-validation-reports-empty", "10.1000/api-validation-reports-empty", "inscribi")
	var empty ValidationResult
	if rpcErr := callMethod(t, Router(system), "artifacts.validation", map[string]string{"job_id": emptyID}, &empty); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if empty.JobID != emptyID || empty.Reports == nil || len(empty.Reports) != 0 {
		t.Fatalf("validation result without evidence = %+v, want non-nil empty reports", empty)
	}
}
