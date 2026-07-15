// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package batch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

type fakeJobs struct {
	rows    map[string]*job.Row
	events  map[string][]map[string]any
	actions []job.HumanAction
}

func (f fakeJobs) Get(_ context.Context, id string) (*job.Row, error) { return f.rows[id], nil }
func (f fakeJobs) Events(_ context.Context, id string) ([]map[string]any, error) {
	return f.events[id], nil
}
func (f fakeJobs) ListHumanActions(_ context.Context, _ bool) ([]job.HumanAction, error) {
	return f.actions, nil
}

func TestManifestWriteAndLoadPreservesBatchShape(t *testing.T) {
	requests := []protocol.WorkRequest{
		{SchemaVersion: protocol.WorkRequestSchemaVersion, Identifiers: &protocol.Identifiers{DOI: "10.1000/one"}},
		{SchemaVersion: protocol.WorkRequestSchemaVersion, Identifiers: &protocol.Identifiers{ArXiv: "2601.12345"}},
	}
	manifest := NewManifest(requests, "  literature review ", " Reading ", time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	manifest.Works[0].JobID = "job_one"
	manifest.Works[1].Status = "skipped_owned"

	dataDir := t.TempDir()
	if err := Write(dataDir, manifest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dataDir, "batches", manifest.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "batches", manifest.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	works, ok := shape["works"].([]any)
	if !ok || shape["schema_version"] != SchemaVersion || len(works) != 2 {
		t.Fatalf("manifest JSON shape = %s", raw)
	}
	loaded, err := Load(dataDir, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != SchemaVersion || loaded.Label != "literature review" || loaded.Collection != "Reading" || len(loaded.Works) != 2 {
		t.Fatalf("manifest = %+v", loaded)
	}
	if loaded.Works[0].RequestID == "" || loaded.Works[0].JobID != "job_one" || loaded.Works[1].Status != "skipped_owned" {
		t.Fatalf("work records = %+v", loaded.Works)
	}
	if loaded.Works[0].RequestID == loaded.Works[1].RequestID {
		t.Fatalf("batch request IDs collided: %+v", loaded.Works)
	}
	if !strings.HasPrefix(loaded.Works[0].RequestID, loaded.ID+"-") || !strings.HasPrefix(loaded.Works[1].RequestID, loaded.ID+"-") {
		t.Fatalf("request IDs do not carry batch identity: %+v", loaded.Works)
	}
	latest, err := Load(dataDir, "latest")
	if err != nil || latest.ID != manifest.ID {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
}

func TestBuildReportClassifiesSeededJobsEventsAndActions(t *testing.T) {
	manifest := &Manifest{
		SchemaVersion: SchemaVersion, ID: "batch-deadbeef", CreatedAt: "2026-07-15T12:00:00Z", Label: "weekly", Collection: "Reading",
		Works: []ManifestWork{
			manifestWork("wr-import", "job-import", "submitted", "Imported paper"),
			manifestWork("wr-browser", "job-browser", "submitted", "Browser paper"),
			manifestWork("wr-institution", "job-institution", "submitted", "Institutional paper"),
			manifestWork("wr-oa", "job-oa", "submitted", "OA browser paper"),
			manifestWork("wr-terms", "job-terms", "submitted", "Terms paper"),
			manifestWork("wr-review", "job-review", "submitted", "Review paper"),
			manifestWork("wr-failed", "job-failed", "submitted", "Failed paper"),
			manifestWork("wr-owned", "", "skipped_owned", "Owned paper"),
			manifestWork("wr-attach", "job-attach", "existing_item_attached", "Attachment paper"),
		},
	}
	jobs := fakeJobs{
		rows: map[string]*job.Row{
			"job-import":      reportRow("job-import", job.StateReady, ""),
			"job-browser":     reportRow("job-browser", job.StateReady, ""),
			"job-institution": reportRow("job-institution", job.StateAwaitingHuman, ""),
			"job-oa":          reportRow("job-oa", job.StateAwaitingHuman, ""),
			"job-terms":       reportRow("job-terms", job.StateAwaitingHuman, ""),
			"job-review":      reportRow("job-review", job.StateNeedsReview, ""),
			"job-failed":      reportRow("job-failed", job.StateFailed, "network_exhausted"),
			"job-attach":      reportRow("job-attach", job.StateReady, ""),
		},
		events: map[string][]map[string]any{
			"job-import": {autoImportEvent("PA123", "AT456")},
			"job-browser": {
				{"kind": "browser.download_complete", "detail": map[string]any{}},
				autoImportEvent("PB123", "AB456"),
			},
			"job-attach": {autoImportEvent("PX123", "AX456")},
		},
		actions: []job.HumanAction{
			{JobID: "job-institution", Kind: "openurl_handoff", Status: "open", Detail: "institutional handoff"},
			{JobID: "job-oa", Kind: "openurl_handoff", Status: "open", Detail: "open-access fetch via browser\nhttps://example.test/paper.pdf"},
			{JobID: "job-terms", Kind: "terms_acceptance_required", Status: "open"},
		},
	}
	report, err := BuildReport(context.Background(), manifest, jobs)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ outcome, reason, failure string }{
		{"imported", "", ""},
		{"browser_fetched_then_imported", "", ""},
		{"awaiting_human", "institutional", ""},
		{"awaiting_human", "oa_browser", ""},
		{"awaiting_human", "terms", ""},
		{"needs_review", "", ""},
		{"failed", "", "network_exhausted"},
		{"skipped_owned", "", ""},
		{"existing_item_attached", "", ""},
	}
	for i, expected := range want {
		got := report.Works[i]
		if got.Outcome != expected.outcome || got.Reason != expected.reason || got.FailureClass != expected.failure {
			t.Fatalf("work %d = %+v, want outcome=%s reason=%s failure=%s", i, got, expected.outcome, expected.reason, expected.failure)
		}
	}
	if got := report.Works[0]; got.ParentKey != "PA123" || got.AttachmentKey != "AT456" || got.Collection != "Reading" {
		t.Fatalf("import detail = %+v", got)
	}
	if report.Summary.Outcomes["awaiting_human"] != 3 || report.Summary.Outcomes["imported"] != 1 || report.Summary.Total != len(want) {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestMarkdownSnapshot(t *testing.T) {
	report := &Report{
		BatchID: "batch-deadbeef", Label: "weekly", Summary: ReportSummary{Total: 3, Outcomes: map[string]int{
			"imported": 1, "awaiting_human": 1, "failed": 1,
		}},
		Works: []ReportWork{
			{Outcome: "imported", JobID: "job-import", Work: protocol.WorkRequest{Title: "Imported"}, ParentKey: "PA123", AttachmentKey: "AT456", Collection: "Reading"},
			{Outcome: "awaiting_human", JobID: "job-human", Work: protocol.WorkRequest{Title: "Needs browser"}, Reason: "oa_browser"},
			{Outcome: "failed", JobID: "job-failed", Work: protocol.WorkRequest{Title: "Broken"}, FailureClass: "network_exhausted"},
		},
	}
	const want = "# Papio batch `batch-deadbeef`\n\nLabel: weekly\n\n3 works: 1 imported, 1 awaiting_human, 1 failed.\n\n## Imported (1)\n- Imported (`job-import`): parent `PA123`; attachment `AT456`; collection `Reading`\n\n## Awaiting Human (1)\n- Needs browser (`job-human`): oa_browser\n\n## Failed (1)\n- Broken (`job-failed`): network_exhausted\n"
	if got := Markdown(report); got != want {
		t.Fatalf("markdown snapshot (-want +got):\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestBuildReportClassifiesLatestAutoImportOutcome(t *testing.T) {
	manifest := &Manifest{
		SchemaVersion: SchemaVersion,
		ID:            "batch-deadbeef",
		CreatedAt:     "2026-07-15T12:00:00Z",
		Works: []ManifestWork{
			manifestWork("wr-live-one", "e484422626", "submitted", "Failed auto import"),
			manifestWork("wr-live-two", "dddf9b73", "submitted", "Recovered auto import"),
			manifestWork("wr-missing-keys", "job-missing-keys", "submitted", "Incomplete applied result"),
			manifestWork("wr-duplicate", "job-duplicate", "submitted", "Duplicate import"),
		},
	}
	jobs := fakeJobs{
		rows: map[string]*job.Row{
			"e484422626":       reportRow("e484422626", job.StateReady, ""),
			"dddf9b73":         reportRow("dddf9b73", job.StateReady, ""),
			"job-missing-keys": reportRow("job-missing-keys", job.StateReady, ""),
			"job-duplicate":    reportRow("job-duplicate", job.StateReady, ""),
		},
		events: map[string][]map[string]any{
			"e484422626": {
				autoImportErrorEvent("zotero_field_validation", "unknown item field"),
			},
			"dddf9b73": {
				autoImportErrorEvent("network", "network connection failed"),
				autoImportEvent("PA123", "AT456"),
			},
			"job-missing-keys": {
				{"kind": "zotio.auto_import", "detail": map[string]any{"status": "applied", "parent_key": "PA123"}},
			},
			"job-duplicate": {
				autoImportErrorEvent("network", "network connection failed"),
				{"kind": "zotio.auto_import", "detail": map[string]any{"status": "duplicate", "parent_key": "PA123"}},
			},
		},
	}
	report, err := BuildReport(context.Background(), manifest, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Works[0]; got.Outcome != "import_failed" || got.ErrorClass != "zotero_field_validation" || got.ErrorHint != "unknown item field" {
		t.Fatalf("failed auto import = %+v", got)
	}
	if got := report.Works[1]; got.Outcome != "imported" || got.ParentKey != "PA123" || got.AttachmentKey != "AT456" {
		t.Fatalf("recovered auto import = %+v", got)
	}
	for _, index := range []int{2, 3} {
		if got := report.Works[index]; got.Outcome == "imported" || got.Outcome == "import_failed" {
			t.Fatalf("work %d falsely classified as imported failure = %+v", index, got)
		}
	}
	if report.Summary.Outcomes["import_failed"] != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestMarkdownRendersImportFailedErrorClassAndHint(t *testing.T) {
	report := &Report{
		BatchID: "batch-deadbeef",
		Summary: ReportSummary{Total: 1, Outcomes: map[string]int{"import_failed": 1}},
		Works: []ReportWork{{
			Outcome: "import_failed", JobID: "e484422626", Work: protocol.WorkRequest{Title: "Failed import"},
			ErrorClass: "zotero_field_validation", ErrorHint: "unknown item field",
		}},
	}
	rendered := Markdown(report)
	for _, want := range []string{"## Import Failed (1)", "zotero_field_validation", "unknown item field"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Markdown() missing %q:\\n%s", want, rendered)
		}
	}
}

func manifestWork(requestID, jobID, status, title string) ManifestWork {
	return ManifestWork{
		RequestID: requestID, JobID: jobID, Status: status,
		Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: requestID, Title: title, Authors: []string{"Ada"}, Year: 2026},
	}
}

func reportRow(id, state, terminalReason string) *job.Row {
	return &job.Row{ID: id, State: state, TerminalReason: terminalReason, Policy: job.Policy{Collection: "Reading"}}
}

func autoImportEvent(parent, attachment string) map[string]any {
	return map[string]any{"kind": "zotio.auto_import", "detail": map[string]any{"status": "applied", "parent_key": parent, "attachment_key": attachment}}
}

func autoImportErrorEvent(class, hint string) map[string]any {
	return map[string]any{
		"kind": "zotio.auto_import",
		"detail": map[string]any{
			"status": "error", "error_class": class, "error_hint": hint,
		},
	}
}

func collectionFilingEvent(status, class string) map[string]any {
	detail := map[string]any{"status": status, "collection": "Reading"}
	if class != "" {
		detail["error_class"] = class
	}
	return map[string]any{"kind": "zotio.collection_filing", "detail": detail}
}

func TestBuildReportSurfacesCollectionFilingOutcome(t *testing.T) {
	manifest := &Manifest{
		SchemaVersion: SchemaVersion, ID: "batch-deadbeef", CreatedAt: "2026-07-15T12:00:00Z", Collection: "Reading",
		Works: []ManifestWork{
			manifestWork("wr-filed", "job-filed", "submitted", "Filed paper"),
			manifestWork("wr-unfiled", "job-unfiled", "submitted", "Unfiled paper"),
		},
	}
	jobs := fakeJobs{
		rows: map[string]*job.Row{
			"job-filed":   reportRow("job-filed", job.StateReady, ""),
			"job-unfiled": reportRow("job-unfiled", job.StateReady, ""),
		},
		events: map[string][]map[string]any{
			"job-filed":   {autoImportEvent("PA1", "AT1"), collectionFilingEvent("applied", "")},
			"job-unfiled": {autoImportEvent("PB2", "AT2"), collectionFilingEvent("error", "zotero_field_validation")},
		},
	}
	report, err := BuildReport(context.Background(), manifest, jobs)
	if err != nil {
		t.Fatal(err)
	}
	filed, unfiled := report.Works[0], report.Works[1]
	if filed.Outcome != "imported" || filed.FilingStatus != "filed" || filed.FilingError != "" {
		t.Fatalf("filed work = %+v", filed)
	}
	if unfiled.Outcome != "imported" || unfiled.FilingStatus != "file_failed" || unfiled.FilingError != "zotero_field_validation" {
		t.Fatalf("unfiled work = %+v", unfiled)
	}
	rendered := Markdown(report)
	if !strings.Contains(rendered, "collection filing failed (zotero_field_validation)") {
		t.Fatalf("markdown missing filing failure note:\n%s", rendered)
	}
	if strings.Count(rendered, "collection filing failed") != 1 {
		t.Fatalf("filed work must not carry a failure note:\n%s", rendered)
	}
}

// Settled is the single source of truth for batch-wait completion: it settles
// only on canonical terminal outcomes emitted by buildWorkReport. A legacy or
// unknown token must NOT settle (regression for the stale hand-maintained list
// that treated legacy spellings like "browser_imported" as settled).
func TestReportSettledUsesCanonicalTerminalOutcomes(t *testing.T) {
	terminal := []string{
		OutcomeImported, OutcomeBrowserFetchedThenImported, OutcomeImportFailed,
		OutcomeExistingItemAttached, OutcomeAwaitingHuman, OutcomeNeedsReview,
		OutcomeFailed, OutcomeSkippedOwned,
	}
	for _, outcome := range terminal {
		report := &Report{Works: []ReportWork{{Outcome: outcome}}}
		if !report.Settled() {
			t.Fatalf("outcome %q should settle", outcome)
		}
	}
	nonTerminal := []string{OutcomeInProgress, "browser_imported", "browser-imported", "skipped", "existing-item attached", ""}
	for _, outcome := range nonTerminal {
		report := &Report{Works: []ReportWork{{Outcome: outcome}}}
		if report.Settled() {
			t.Fatalf("outcome %q must not settle", outcome)
		}
	}
	// A report settles only when every work is terminal.
	mixed := &Report{Works: []ReportWork{{Outcome: OutcomeImported}, {Outcome: OutcomeInProgress}}}
	if mixed.Settled() {
		t.Fatal("a report with an in-progress work must not settle")
	}
	if !(&Report{}).Settled() {
		t.Fatal("an empty report settles vacuously")
	}
}
