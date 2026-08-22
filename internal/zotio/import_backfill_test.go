// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/bundle"
	"papio/internal/job"
	"papio/internal/redact"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

type trackingImporter struct {
	calls   []string
	status  map[string]string
	parents map[string]string
	errs    map[string]error
}

func (t *trackingImporter) PlanAndApply(_ context.Context, jobID string) (string, string, string, error) {
	t.calls = append(t.calls, jobID)
	if err := t.errs[jobID]; err != nil {
		return "failed", "", "", err
	}
	status := t.status[jobID]
	if status == "" {
		status = "applied"
	}
	parentKey := t.parents[jobID]
	if parentKey == "" {
		parentKey = "PARENT01"
	}
	return status, parentKey, "ATTACH01", nil
}

func seedImportBackfillJob(t *testing.T, ctx context.Context, db *store.Store, id, createdAt string, autoImport bool, identity string, importStatus string) {
	t.Helper()
	policy, err := json.Marshal(map[string]any{"auto_import": autoImport, "access_mode": "conservative"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, 'Example paper')`,
		"wr_"+id, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES (?, ?, 'ready', ?, ?, ?)`, id, "wr_"+id, string(policy), createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO artifacts (sha256, size_bytes, mime, path, created_at)
		VALUES (?, 1, 'application/pdf', ?, ?)`, id+"sha", "/tmp/"+id+".pdf", createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO job_artifacts (job_id, artifact_sha256, role, identity_result, created_at)
		VALUES (?, ?, 'main', ?, ?)`, id, id+"sha", identity, createdAt); err != nil {
		t.Fatal(err)
	}
	if importStatus != "" {
		detail, err := json.Marshal(map[string]any{"status": importStatus})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO events (job_id, at, kind, detail_json)
			VALUES (?, ?, 'zotio.auto_import', ?)`, id, createdAt, string(detail)); err != nil {
			t.Fatal(err)
		}
	}
}

func importBackfillService(t *testing.T, dataDir string, db *store.Store, cli CLI) *Service {
	t.Helper()
	jobs := &job.Store{S: db}
	artifacts, err := artifact.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{
		CLI:     cli,
		Bundle:  &bundle.Exporter{Jobs: jobs, Artifacts: artifacts, DataDir: dataDir},
		Store:   db,
		DataDir: dataDir,
	}
}

func enableAutoImportPolicy(t *testing.T, ctx context.Context, db *store.Store, jobID string) {
	t.Helper()
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE jobs SET policy_json = json_set(policy_json, '$.auto_import', json('true')) WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
}

func TestImportBackfillSelectionOldestFirstAndGate(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	mid := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	newest := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)

	seedImportBackfillJob(t, ctx, db, "job_old_requested", old, true, "pass", "")
	seedImportBackfillJob(t, ctx, db, "job_mid_requested", mid, true, "pass", "")
	seedImportBackfillJob(t, ctx, db, "job_new_requested", newest, true, "pass", "")
	seedImportBackfillJob(t, ctx, db, "job_not_requested", mid, false, "pass", "")
	seedImportBackfillJob(t, ctx, db, "job_imported", old, true, "pass", "applied")
	seedImportBackfillJob(t, ctx, db, "job_bad_identity", mid, true, "fail", "")

	service := importBackfillService(t, dataDir, db, nil)
	candidates, truncated, err := service.listImportBackfillCandidates(ctx, false, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || !truncated {
		t.Fatalf("candidates = %#v truncated=%t, want 2 and truncated", candidates, truncated)
	}
	if candidates[0].JobID != "job_old_requested" || candidates[1].JobID != "job_mid_requested" {
		t.Fatalf("order = %#v, want oldest requested first", candidates)
	}
	more, truncated, err := service.listImportBackfillCandidates(ctx, false, candidates[1].JobID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 1 || more[0].JobID != "job_new_requested" || truncated {
		t.Fatalf("cursor continuation = %#v truncated=%t, want the final page and no truncation", more, truncated)
	}
	excluded, err := service.countImportBackfillExcluded(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if excluded != 1 {
		t.Fatalf("not_requested_excluded = %d, want 1", excluded)
	}
	withFlag, _, err := service.listImportBackfillCandidates(ctx, true, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range withFlag {
		if candidate.JobID == "job_not_requested" {
			found = true
		}
	}
	if !found {
		t.Fatalf("include-not-requested cohort missing: %#v", withFlag)
	}
}

func TestImportBackfillDryRunDefault(t *testing.T) {
	ctx := context.Background()
	service, jobID := readyPlanService(t, "", &planCLI{})
	enableAutoImportPolicy(t, ctx, service.Store, jobID)

	importer := &trackingImporter{status: map[string]string{jobID: "applied"}}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Limit: 10}, importer)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun {
		t.Fatal("dry-run must be the default")
	}
	if len(importer.calls) != 0 {
		t.Fatalf("importer calls = %v, want none in dry-run", importer.calls)
	}
	var eventCount int
	if err := service.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = 'zotio.auto_import'`, jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("events = %d, want none in dry-run", eventCount)
	}
}

func TestImportBackfillMarksDuplicateForOwnedPapers(t *testing.T) {
	ctx := context.Background()
	ownedCLI := &fakeCLI{
		find: map[string]json.RawMessage{
			"doi:10.1002/example": json.RawMessage(`[{"key":"AB12CD34","data":{}}]`),
		},
	}
	service, ownedJobID := readyPlanService(t, "", ownedCLI)
	enableAutoImportPolicy(t, ctx, service.Store, ownedJobID)

	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.AlreadyOwned == nil || *result.Summary.AlreadyOwned != 1 || len(result.AlreadyOwned) != 1 || result.AlreadyOwned[0].JobID != ownedJobID {
		t.Fatalf("already_owned breakdown = %+v", result)
	}
	if result.Summary.NewlyFiled != 0 || len(result.NewlyFiled) != 0 {
		t.Fatalf("newly_filed = %+v, want none for owned duplicate", result.NewlyFiled)
	}
	if result.Summary.AlreadyInLibrary != 1 || len(result.AlreadyInLibrary) != 1 || result.AlreadyInLibrary[0].Status != "duplicate" {
		t.Fatalf("already_in_library apply = %+v", result.AlreadyInLibrary)
	}
	result2, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, service)
	if err != nil {
		t.Fatal(err)
	}
	if result2.Summary.Selected != 0 {
		t.Fatalf("second run selected = %d, want 0 after duplicate delivery", result2.Summary.Selected)
	}
}

func TestImportBackfillApplyIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:   `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	enableAutoImportPolicy(t, ctx, service.Store, jobID)

	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.NewlyFiled != 1 {
		t.Fatalf("first apply summary = %+v", result.Summary)
	}
	result2, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, service)
	if err != nil {
		t.Fatal(err)
	}
	if result2.Summary.Selected != 0 {
		t.Fatalf("second apply selected = %d, want 0", result2.Summary.Selected)
	}
	if cli.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1 across two apply backfills", cli.applyCalls)
	}
	events, err := service.Bundle.Jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	success := 0
	for _, event := range events {
		if event["kind"] != "zotio.auto_import" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["status"] == "applied" {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful auto_import events = %d, want 1", success)
	}
}

func addReadyPlanJob(t *testing.T, service *Service, requestID string) string {
	t.Helper()
	ctx := context.Background()
	jobID, err := service.Bundle.Jobs.CreateRequest(ctx, requestID, work.Work{
		DOI: "10.1002/second", Title: "Second Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Bundle.Jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		JobID: jobID, Source: "unpaywall", URLRedacted: redact.URL("https://example.test/second.pdf"), URLKey: "url-key-2",
		LandingRedacted: "https://example.test/second", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Bundle.Jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	if err := service.Bundle.Jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := service.Bundle.Artifacts.QuarantineDir(jobID)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(quarantine, "paper.tmp")
	body := []byte("%PDF-1.4\nfixture DOI 10.1002/second\n%%EOF")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := artifact.HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath, err := service.Bundle.Artifacts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bundle.Jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", PageCount: 1,
		TextChars: 1000, IdentityResult: "pass", Path: artifactPath,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := service.Bundle.Jobs.Transition(ctx, jobID, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Bundle.Jobs.Transition(ctx, jobID, job.StateValidating, job.StateReady, nil, job.WithCandidate(candidate.ID), job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func TestImportBackfillFailureIsolation(t *testing.T) {
	ctx := context.Background()
	service, jobFail := readyPlanService(t, "", &planCLI{})
	enableAutoImportPolicy(t, ctx, service.Store, jobFail)
	jobOK := addReadyPlanJob(t, service, "request_backfill_ok")
	enableAutoImportPolicy(t, ctx, service.Store, jobOK)

	importer := &trackingImporter{
		status: map[string]string{jobOK: "applied"},
		errs:   map[string]error{jobFail: fmt.Errorf("previewing Zotio mutation: boom")},
	}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, importer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Failed != 1 || result.Summary.NewlyFiled != 1 {
		t.Fatalf("summary = %+v, want one failed and one newly filed", result.Summary)
	}
	if len(importer.calls) != 2 {
		t.Fatalf("importer calls = %v, want both jobs attempted", importer.calls)
	}
}

func TestImportBackfillExpectedFailEmptyTitle(t *testing.T) {
	ctx := context.Background()
	service, jobID := readyPlanService(t, "", &planCLI{})
	enableAutoImportPolicy(t, ctx, service.Store, jobID)
	if _, err := service.Store.DB().Exec(`UPDATE work_requests SET title = '', authors_json = '[]' WHERE id = (SELECT work_request_id FROM jobs WHERE id = ?)`, jobID); err != nil {
		t.Fatal(err)
	}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Limit: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.ExpectedFail != 1 || len(result.ExpectedFail) != 1 {
		t.Fatalf("expected_fail = %+v", result.ExpectedFail)
	}
	if !strings.Contains(result.ExpectedFail[0].Reason, "title") {
		t.Fatalf("reason = %q, want a title validation failure", result.ExpectedFail[0].Reason)
	}
}

func TestImportBackfillNoOpNotCountedAsNewlyFiled(t *testing.T) {
	ctx := context.Background()
	service, jobID := readyPlanService(t, "", &planCLI{})
	enableAutoImportPolicy(t, ctx, service.Store, jobID)

	importer := &trackingImporter{
		status:  map[string]string{jobID: "no_op"},
		parents: map[string]string{jobID: "AB12CD34"},
	}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, importer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.NewlyFiled != 0 || len(result.NewlyFiled) != 0 {
		t.Fatalf("newly_filed = %+v, want no_op excluded", result)
	}
	if result.Summary.AlreadyInLibrary != 1 || result.AlreadyInLibrary[0].Status != "no_op" {
		t.Fatalf("already_in_library = %+v", result.AlreadyInLibrary)
	}
}

func TestImportBackfillDryRunOwnershipUndeterminedWithoutCLI(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	seedImportBackfillJob(t, ctx, db, "job_no_cli", createdAt, true, "pass", "")
	service := importBackfillService(t, dataDir, db, nil)

	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Limit: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summary.AlreadyOwnedUndetermined {
		t.Fatalf("summary = %+v, want ownership undetermined without CLI", result.Summary)
	}
	if result.Summary.AlreadyOwned != nil {
		t.Fatalf("already_owned = %v, want omitted when undetermined", result.Summary.AlreadyOwned)
	}
}

func TestImportBackfillDuplicateJobResolutionReported(t *testing.T) {
	ctx := context.Background()
	service, jobOne := readyPlanService(t, "", &planCLI{})
	jobTwo := addReadyPlanJob(t, service, "request_backfill_dup_b")
	enableAutoImportPolicy(t, ctx, service.Store, jobOne)
	enableAutoImportPolicy(t, ctx, service.Store, jobTwo)

	importer := &trackingImporter{
		status: map[string]string{
			jobOne: "duplicate",
			jobTwo: "duplicate",
		},
		parents: map[string]string{
			jobOne: "7HVY4FYV",
			jobTwo: "7HVY4FYV",
		},
	}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, Limit: 10}, importer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.AlreadyInLibrary != 2 {
		t.Fatalf("already_in_library = %d, want 2", result.Summary.AlreadyInLibrary)
	}
	if result.Summary.SharedResolutionJobs != 1 {
		t.Fatalf("shared_resolution_jobs = %d, want 1", result.Summary.SharedResolutionJobs)
	}
}

func TestImportBackfillMarshalJSONNormalizesNullSlices(t *testing.T) {
	raw, err := json.Marshal(ImportBackfillResult{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"would_import", "already_owned", "expected_fail"} {
		if string(payload[key]) == "null" {
			t.Fatalf("%s marshaled as null: %s", key, raw)
		}
	}
}

// repairingImporter is an importer that can also close a citation gap, which is
// what the daemon wires in production (bootstrap.citationEnrichingImporter).
type repairingImporter struct {
	trackingImporter
	repaired []string
	repair   func(jobID string)
}

func (r *repairingImporter) EnsureCitationMetadata(_ context.Context, jobID string) {
	r.repaired = append(r.repaired, jobID)
	if r.repair != nil {
		r.repair(jobID)
	}
}

// A ready job whose classification will refuse it (here: no exportable bundle)
// never reaches the importer — the switch records the reason and skips it. That
// ordering is why a repair hung on the importer alone never ran for the only
// class of job that needed it: papers already past their retry budget, reachable
// only through this operator-driven path. So the repair must be offered BEFORE
// the job is judged, and this pins exactly that: the repairer is consulted for a
// candidate the importer is never asked about.
//
// The end-to-end effect (a closed citation gap turning expected_fail into an
// import) was proved live against the operator's store and a real zotio, where
// expected_fail went 4 -> 0 and four titles were filled; this fixture's jobs
// cannot reach that classification because their artifacts are not real PDFs.
func TestImportBackfillOffersRepairBeforeClassifying(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	when := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	seedImportBackfillJob(t, ctx, db, "job_needs_repair", when, true, "pass", "")
	service := importBackfillService(t, dataDir, db, &planCLI{})

	repairing := &repairingImporter{}
	result, err := service.ImportBackfill(ctx, ImportBackfillRequest{Apply: true, IncludeNotRequested: true, Limit: 5}, repairing)
	if err != nil {
		t.Fatalf("ImportBackfill = %v", err)
	}
	if result.Summary.ExpectedFail != 1 {
		t.Fatalf("expected_fail = %d, want 1 for this fixture", result.Summary.ExpectedFail)
	}
	if len(repairing.calls) != 0 {
		t.Fatalf("importer calls = %v, want none: classification skips an expected failure", repairing.calls)
	}
	// The load-bearing assertion: repaired despite the importer never being
	// asked. A repair reachable only through PlanAndApply could not do this.
	if len(repairing.repaired) != 1 || repairing.repaired[0] != "job_needs_repair" {
		t.Fatalf("repaired = %v, want the skipped candidate repaired before classification", repairing.repaired)
	}

	// A dry run previews; it must not mutate a paper's metadata.
	preview := &repairingImporter{}
	if _, err := service.ImportBackfill(ctx, ImportBackfillRequest{IncludeNotRequested: true, Limit: 5}, preview); err != nil {
		t.Fatalf("dry-run ImportBackfill = %v", err)
	}
	if len(preview.repaired) != 0 {
		t.Fatalf("dry-run repaired = %v, want none: a preview must not write", preview.repaired)
	}
}

// queueCLI reports zotio's missing-PDF queue. It deliberately does not
// implement MissingPDFKeys, so this exercises the whole-queue fallback.
type queueCLI struct {
	planCLI
	missing []MissingPDFItem
}

func (c *queueCLI) MissingPDF(context.Context, string, int) ([]MissingPDFItem, error) {
	return c.missing, nil
}

func seedItemKey(t *testing.T, ctx context.Context, db *store.Store, jobID, itemKey string) {
	t.Helper()
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE work_requests SET zotio_item_key = ? WHERE id = ?`, itemKey, "wr_"+jobID); err != nil {
		t.Fatal(err)
	}
}

// A job whose Zotero item already holds a PDF is finished, and must be reported
// as already owned rather than sent down the existing-item attach route. On a
// library whose files live on the operator's own file store that attach can
// never succeed, so papio retried it on every pass forever.
func TestImportBackfillOwnsKeyedItemHoldingPDF(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	created := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	seedImportBackfillJob(t, ctx, db, "job_keyed_holds_pdf", created, true, "pass", "")
	seedItemKey(t, ctx, db, "job_keyed_holds_pdf", "ITEMHOLD1")

	// The queue names another item, so this one holds its PDF.
	held := importBackfillService(t, dataDir, db, &queueCLI{missing: []MissingPDFItem{{Key: "OTHERKEY"}}})
	result, err := held.ImportBackfill(ctx, ImportBackfillRequest{Limit: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.WouldImport != 0 {
		t.Fatalf("would_import = %d, want 0: the item already holds the PDF", result.Summary.WouldImport)
	}
	if result.Summary.AlreadyOwned == nil || *result.Summary.AlreadyOwned != 1 {
		t.Fatalf("already_owned = %v, want 1", result.Summary.AlreadyOwned)
	}
	if len(result.AlreadyOwned) != 1 || result.AlreadyOwned[0].ParentKey != "ITEMHOLD1" {
		t.Fatalf("already owned items = %#v, want the known item key", result.AlreadyOwned)
	}

	// The same job stays importable while zotio still reports the item as
	// missing its PDF: the attach is the whole point of that route.
	missing := importBackfillService(t, dataDir, db, &queueCLI{missing: []MissingPDFItem{{Key: "ITEMHOLD1"}}})
	result, err = missing.ImportBackfill(ctx, ImportBackfillRequest{Limit: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.AlreadyOwned == nil || *result.Summary.AlreadyOwned != 0 {
		t.Fatalf("already_owned = %v, want 0 while the item still lacks a PDF", result.Summary.AlreadyOwned)
	}
	if len(result.AlreadyOwned) != 0 {
		t.Fatalf("already owned items = %#v, want none: the attach is still owed", result.AlreadyOwned)
	}
}

// PlanAndApply must not attempt an attach for a job whose Zotero item already
// holds the PDF: on a library whose files live on the operator's own file store
// that upload is refused every time, and ready is terminal, so the job retries
// forever. The job is recorded as a duplicate and advances to imported.
func TestPlanAndApplySkipsKeyedItemHoldingPDF(t *testing.T) {
	ctx := context.Background()
	dataDir := storetest.DataDir(t)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	created := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	seedImportBackfillJob(t, ctx, db, "job_keyed_skip", created, true, "pass", "")
	seedItemKey(t, ctx, db, "job_keyed_skip", "ITEMSKIP1")

	cli := &queueCLI{missing: []MissingPDFItem{{Key: "OTHERKEY"}}}
	service := importBackfillService(t, dataDir, db, cli)
	status, parentKey, attachmentKey, err := service.PlanAndApply(ctx, "job_keyed_skip")
	if err != nil {
		t.Fatalf("PlanAndApply = %v, want the owned item to short-circuit", err)
	}
	if status != "duplicate" || parentKey != "ITEMSKIP1" || attachmentKey != "" {
		t.Fatalf("status=%q parent=%q attachment=%q, want duplicate on the known item", status, parentKey, attachmentKey)
	}
	if cli.previewCalls != 0 || cli.applyCalls != 0 {
		t.Fatalf("preview=%d apply=%d, want no Zotero mutation attempted", cli.previewCalls, cli.applyCalls)
	}
	row, err := (&job.Store{S: db}).Get(ctx, "job_keyed_skip")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateImported {
		t.Fatalf("state = %q, want %q", row.State, job.StateImported)
	}
}
