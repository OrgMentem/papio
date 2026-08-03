// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"fmt"
	"testing"

	"papio/internal/work"
)

func createAttributionTestJob(t *testing.T, js *Store, requestID string, w work.Work, consumer string) string {
	t.Helper()
	result, err := js.CreateRequestForWork(context.Background(), requestID, w, "", "", testPolicy(), nil,
		Attribution{Principal: PrincipalCLI, Consumer: consumer}, false)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	return result.JobID
}

func TestConsumerRoundTripDistinguishesRecordedAttributionFromAbsence(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	attributedID := createAttributionTestJob(t, js, "wr_attribution_present", testWork(), "inscribi")
	unattributedWork := testWork()
	unattributedWork.DOI = "10.1002/unattributed"
	unattributedID := createAttributionTestJob(t, js, "wr_attribution_absent", unattributedWork, "")

	consumer, recorded, err := js.Consumer(ctx, attributedID)
	if err != nil {
		t.Fatalf("consumer for attributed job: %v", err)
	}
	if consumer != "inscribi" || !recorded {
		t.Fatalf("attributed consumer = %q, recorded = %t; want inscribi/true", consumer, recorded)
	}

	consumer, recorded, err = js.Consumer(ctx, unattributedID)
	if err != nil {
		t.Fatalf("consumer for unattributed job: %v", err)
	}
	if consumer != "" || recorded {
		t.Fatalf("unattributed consumer = %q, recorded = %t; want empty/false", consumer, recorded)
	}
}

func TestConsumersForOmitsUnattributedJobs(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	attributedID := createAttributionTestJob(t, js, "wr_consumers_for_present", testWork(), "inscribi")
	unattributedWork := testWork()
	unattributedWork.DOI = "10.1002/consumers-for-absent"
	unattributedID := createAttributionTestJob(t, js, "wr_consumers_for_absent", unattributedWork, "")

	consumers, err := js.ConsumersFor(ctx, []string{attributedID, unattributedID})
	if err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if got := consumers[attributedID]; got != "inscribi" {
		t.Fatalf("attributed map entry = %q, want inscribi", got)
	}
	if _, ok := consumers[unattributedID]; ok {
		t.Fatalf("unattributed job %s present in consumer map: %#v", unattributedID, consumers)
	}
}

func TestListPageForFiltersBeforeLimitAndReportsFilteredTruncation(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		w := testWork()
		w.DOI = fmt.Sprintf("10.1002/list-filter-a-%d", i)
		createAttributionTestJob(t, js, fmt.Sprintf("wr_list_filter_a_%d", i), w, "A")
	}
	for i := 0; i < 2; i++ {
		w := testWork()
		w.DOI = fmt.Sprintf("10.1002/list-filter-b-%d", i)
		createAttributionTestJob(t, js, fmt.Sprintf("wr_list_filter_b_%d", i), w, "B")
	}
	unattributedWork := testWork()
	unattributedWork.DOI = "10.1002/list-filter-unattributed"
	createAttributionTestJob(t, js, "wr_list_filter_unattributed", unattributedWork, "")

	rows, truncated, err := js.ListPageFor(ctx, "", "A", 2)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("filtered page = %d rows, truncated %t; want 2/true", len(rows), truncated)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	consumers, err := js.ConsumersFor(ctx, ids)
	if err != nil {
		t.Fatalf("consumers for page: %v", err)
	}
	for _, row := range rows {
		if consumers[row.ID] != "A" {
			t.Fatalf("filtered row %s has consumer %q, want A", row.ID, consumers[row.ID])
		}
	}
}

func TestReusedLiveWorkKeepsOriginalAttribution(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	w := testWork()

	first, err := js.CreateRequestForWork(ctx, "wr_reused_attribution_a", w, "", "", testPolicy(), nil,
		Attribution{Principal: PrincipalCLI, Consumer: "A"}, false)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if first.Existing {
		t.Fatal("first submit marked existing")
	}
	second, err := js.CreateRequestForWork(ctx, "wr_reused_attribution_b", w, "", "", testPolicy(), nil,
		Attribution{Principal: PrincipalCLI, Consumer: "B"}, false)
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if !second.Existing || second.JobID != first.JobID {
		t.Fatalf("second submit = %+v, want existing first job %s", second, first.JobID)
	}

	consumer, recorded, err := js.Consumer(ctx, first.JobID)
	if err != nil {
		t.Fatalf("consumer after reuse: %v", err)
	}
	if consumer != "A" || !recorded {
		t.Fatalf("reused job consumer = %q, recorded = %t; want A/true", consumer, recorded)
	}
}

func TestRecordValidationReportUpsertsByCandidateAndListsNewestFirst(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := createAttributionTestJob(t, js, "wr_validation_report", testWork(), "inscribi")

	if err := js.RecordValidationReport(ctx, ValidationRecord{
		JobID: jobID, CandidateID: 1, SHA256: "sha-old", Outcome: "structure_rejected",
		RecordedAt: "2026-01-01T00:00:01Z", Document: `{"schema_version":"validation-report/old"}`,
	}); err != nil {
		t.Fatalf("first candidate report: %v", err)
	}
	if err := js.RecordValidationReport(ctx, ValidationRecord{
		JobID: jobID, CandidateID: 2, SHA256: "sha-two", Outcome: "pass",
		RecordedAt: "2026-01-01T00:00:02Z", Document: `{"schema_version":"validation-report/two"}`,
	}); err != nil {
		t.Fatalf("second candidate report: %v", err)
	}
	if err := js.RecordValidationReport(ctx, ValidationRecord{
		JobID: jobID, CandidateID: 1, SHA256: "sha-new", Outcome: "pass",
		RecordedAt: "2026-01-01T00:00:03Z", Document: `{"schema_version":"validation-report/new"}`,
	}); err != nil {
		t.Fatalf("replacement candidate report: %v", err)
	}

	reports, err := js.ValidationReports(ctx, jobID)
	if err != nil {
		t.Fatalf("validation reports: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("validation reports = %d, want 2 after upsert", len(reports))
	}
	if reports[0].CandidateID != 1 || reports[0].RecordedAt != "2026-01-01T00:00:03Z" || reports[0].Document != `{"schema_version":"validation-report/new"}` {
		t.Fatalf("newest report = %+v, want replacement candidate 1", reports[0])
	}
	if reports[1].CandidateID != 2 || reports[1].RecordedAt != "2026-01-01T00:00:02Z" || reports[1].Document != `{"schema_version":"validation-report/two"}` {
		t.Fatalf("second report = %+v, want candidate 2", reports[1])
	}
}
