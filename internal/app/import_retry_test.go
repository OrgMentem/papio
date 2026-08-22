// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"papio/internal/discovery"
	"papio/internal/job"
	"papio/internal/work"
	"papio/internal/zotio"
)

// selectiveImporter lets tests control per-job outcome while still tracking
// which job IDs were retried. It is needed to prove RunDue continues past a
// per-item failure (the real contract: retryPendingImports swallows per-job
// errors and returns nil).
type selectiveImporter struct {
	mu        sync.Mutex
	calls     []string
	perJobErr map[string]error
	fallback  error
	status    string
}

func (s *selectiveImporter) PlanAndApply(_ context.Context, jobID string) (string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, jobID)
	if err, ok := s.perJobErr[jobID]; ok {
		return "", "", "", err
	}
	if s.fallback != nil {
		return "", "", "", s.fallback
	}
	if s.status != "" {
		return s.status, "PARENT", "ATTACH", nil
	}
	return "applied", "PARENT", "ATTACH", nil
}

func (s *selectiveImporter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *selectiveImporter) calledFor(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == id {
			return true
		}
	}
	return false
}

// seedReadyJobWithImportResult submits a job, drives it to ready via
// Process (which inline imports exactly once), and returns the job ID.
// The caller controls the inline import outcome via svc.AutoImporter.
func seedReadyJobWithImportResult(t *testing.T, svc *Service, jobs *job.Store, wrID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, doiRequestFor(wrID))
	if err != nil {
		t.Fatalf("submit %s: %v", wrID, err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", 100000000000)
	if err != nil || row == nil {
		t.Fatalf("claim %s: %v %+v", wrID, err, row)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("process %s: %v", wrID, err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get %s: %v", wrID, err)
	}
	if got.State != job.StateReady {
		t.Fatalf("job %s state = %s, want ready", wrID, got.State)
	}
	return id
}

func TestImportRetrierRunDueRetriesOnlyDueJobs(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	// Job A: inline import fails → importNeedsRetry true (due).
	svc.AutoImporter = &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("transient"))}
	dueID := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_due_001")

	// Job B: inline import succeeds → importNeedsRetry false (not due).
	svc.AutoImporter = &fakeAutoImporter{status: "applied", parentKey: "P1", attachmentKey: "A1"}
	notDueID := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_notdue_001")

	// capturing importer for the RunDue pass: succeeds for any retried job.
	capt := &selectiveImporter{status: "applied"}
	svc.AutoImporter = capt

	dueBefore, err := jobs.Events(ctx, dueID)
	if err != nil {
		t.Fatal(err)
	}
	notDueBefore, err := jobs.Events(ctx, notDueID)
	if err != nil {
		t.Fatal(err)
	}

	retrier := svc.ImportRetrier()
	if retrier == nil {
		t.Fatal("ImportRetrier returned nil")
	}
	if err := retrier.RunDue(ctx); err != nil {
		t.Fatalf("RunDue = %v, want nil (best-effort, per-job failures are swallowed)", err)
	}

	// Observable state: only the due job was retried.
	if !capt.calledFor(dueID) {
		t.Fatalf("due job %s was not retried", dueID)
	}
	if capt.calledFor(notDueID) {
		t.Fatalf("not-due job %s was retried, want untouched", notDueID)
	}
	if n := capt.callCount(); n != 1 {
		t.Fatalf("PlanAndApply calls = %d, want 1 (only due job)", n)
	}

	dueAfter, err := jobs.Events(ctx, dueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dueAfter) != len(dueBefore)+1 {
		t.Fatalf("due job events: before %d after %d, want +1", len(dueBefore), len(dueAfter))
	}
	// The new event must be a successful applied import.
	last := dueAfter[len(dueAfter)-1]
	if last["kind"] != "zotio.auto_import" {
		t.Fatalf("due job last event kind = %v, want zotio.auto_import", last["kind"])
	}
	detail, _ := last["detail"].(map[string]any)
	if detail["status"] != "applied" {
		t.Fatalf("due job last event status = %v, want applied", detail["status"])
	}
	if importNeedsRetry(dueAfter) {
		t.Fatal("due job should no longer need retry after successful RunDue")
	}

	notDueAfter, err := jobs.Events(ctx, notDueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notDueAfter) != len(notDueBefore) {
		t.Fatalf("not-due job events changed: before %d after %d, want untouched", len(notDueBefore), len(notDueAfter))
	}
}

func TestImportRetrierRunDueAtCapIsNotRetried(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	// Seed a job then drive it to maxImportAttempts errors via inline + retries.
	svc.AutoImporter = &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("transient"))}
	id := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_cap_001")
	// Already 1 error from inline import. Need maxImportAttempts-1 more via retryPendingImports.
	for i := 1; i < maxImportAttempts; i++ {
		if err := svc.retryPendingImports(ctx); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if importNeedsRetry(events) {
		t.Fatal("job should be at cap and not need retry")
	}
	beforeLen := len(events)

	capt := &selectiveImporter{status: "applied"}
	svc.AutoImporter = capt
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatalf("RunDue = %v, want nil", err)
	}
	if capt.callCount() != 0 {
		t.Fatalf("at-cap job was retried %d times, want 0", capt.callCount())
	}
	after, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != beforeLen {
		t.Fatalf("at-cap events: before %d after %d, want untouched", beforeLen, len(after))
	}
}

func TestImportRetrierRunDueContinuesAfterPerItemFailure(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	// Seed two due jobs.
	svc.AutoImporter = &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("transient"))}
	due1 := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_err_001")
	due2 := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_err_002")

	events1Before, _ := jobs.Events(ctx, due1)
	events2Before, _ := jobs.Events(ctx, due2)

	// Next pass: first job fails again, second succeeds. The loop must continue
	// past the first failure and still attempt the second (contract: continue,
	// best-effort, return nil overall — not aggregate error, not early return).
	capt := &selectiveImporter{
		perJobErr: map[string]error{
			due1: zotio.WithErrorInfo(errors.New("still failing")),
		},
		status: "applied",
	}
	svc.AutoImporter = capt

	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatalf("RunDue = %v, want nil (must continue past per-item failure and return nil)", err)
	}
	if n := capt.callCount(); n != 2 {
		t.Fatalf("PlanAndApply calls = %d, want 2 (both due jobs, despite first failing)", n)
	}
	if !capt.calledFor(due1) || !capt.calledFor(due2) {
		t.Fatalf("calls = %v, want both %s and %s", capt.calls, due1, due2)
	}

	// Observable: due1 gained one more error event, due2 gained a successful one.
	events1After, _ := jobs.Events(ctx, due1)
	events2After, _ := jobs.Events(ctx, due2)
	if len(events1After) != len(events1Before)+1 {
		t.Fatalf("due1 events: before %d after %d, want +1", len(events1Before), len(events1After))
	}
	if len(events2After) != len(events2Before)+1 {
		t.Fatalf("due2 events: before %d after %d, want +1", len(events2Before), len(events2After))
	}
	last1, _ := events1After[len(events1After)-1]["detail"].(map[string]any)
	if last1["status"] != "error" {
		t.Fatalf("due1 last status = %v, want error", last1["status"])
	}
	last2, _ := events2After[len(events2After)-1]["detail"].(map[string]any)
	if last2["status"] != "applied" {
		t.Fatalf("due2 last status = %v, want applied", last2["status"])
	}
	// due1 still needs retry (under cap), due2 no longer does.
	if !importNeedsRetry(events1After) {
		t.Fatal("due1 should still need retry after another error")
	}
	if importNeedsRetry(events2After) {
		t.Fatal("due2 should not need retry after success")
	}
}

func TestImportRetrierRunDueNilReceiver(t *testing.T) {
	var r *ImportRetrier
	if err := r.RunDue(context.Background()); err != nil {
		t.Fatalf("nil RunDue = %v, want nil", err)
	}
}

func TestImportRetrierRunDueRespectsAutoImportPolicy(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	// Create a ready job with AutoImport false by toggling config before submit.
	svc.Config.Zotio.AutoImport = false
	svc.AutoImporter = &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("transient"))}
	// Even though we set an importer, autoImportReady early-returns when
	// row.Policy.AutoImport is false, so no zotio event is recorded.
	idNoAuto := seedReadyJobWithImportResult(t, svc, jobs, "wr_import_noauto_001")
	eventsBefore, _ := jobs.Events(ctx, idNoAuto)
	// Restore auto-import for the retry pass so the service itself is eligible,
	// but the job's own policy must still gate.
	svc.Config.Zotio.AutoImport = true
	capt := &selectiveImporter{status: "applied"}
	svc.AutoImporter = capt

	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatalf("RunDue = %v, want nil", err)
	}
	if capt.callCount() != 0 {
		t.Fatalf("non-auto-import job was retried %d times, want 0", capt.callCount())
	}
	eventsAfter, _ := jobs.Events(ctx, idNoAuto)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("non-auto-import events: before %d after %d, want untouched", len(eventsBefore), len(eventsAfter))
	}
}

// One maintenance pass must not drain the whole queue: every runner shares one
// goroutine on a one-minute ticker, and each import drives Zotero's desktop
// connector. An unbounded pass starved its siblings and wedged the operator's
// library application; the next tick continues from where this one stopped.
func TestImportRetrierRunDueStopsAtPerPassCap(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)

	// Seed strictly more due jobs than one pass may import.
	due := make([]string, 0, maxImportsPerPass+2)
	for i := range maxImportsPerPass + 2 {
		svc.AutoImporter = &fakeAutoImporter{err: zotio.WithErrorInfo(errors.New("transient"))}
		due = append(due, seedReadyJobWithImportResult(t, svc, jobs, fmt.Sprintf("wr_import_cap_%03d", i)))
	}

	capt := &selectiveImporter{status: "applied"}
	svc.AutoImporter = capt
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatalf("RunDue = %v, want nil", err)
	}
	if n := capt.callCount(); n != maxImportsPerPass {
		t.Fatalf("PlanAndApply calls = %d, want %d (per-pass cap)", n, maxImportsPerPass)
	}

	// The work is not dropped: a second pass picks up jobs the first left.
	before := capt.callCount()
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatalf("second RunDue = %v, want nil", err)
	}
	if capt.callCount() <= before {
		t.Fatalf("second pass imported nothing; calls still %d, so the remainder was stranded", before)
	}
	_ = due
}

// A ready job whose citation title and authors are empty fails zotio's bundle
// validation every time, and ready is terminal: the resolve-time enrichment that
// fills those fields can never run again, so the job holds a validated PDF that
// papio will never file. Measured on the operator's machine 2026-08-22: 21 of 72
// ready jobs, 16 of them already at the retry cap, while the configured
// discovery backend answered their DOIs on demand.
func TestEnsureCitationMetadataFillsTitlelessReadyJob(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	svc.AutoImporter = &fakeAutoImporter{status: "applied"}
	jobID := seedReadyJobWithImportResult(t, svc, jobs, "wr_enrich_gap_001")

	if _, err := jobs.FillWorkMetadata(ctx, jobID, work.Work{}); err != nil {
		t.Fatalf("clearing metadata: %v", err)
	}
	row, err := jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(row.Work.Title) != "" {
		t.Fatalf("precondition: title = %q, want empty", row.Work.Title)
	}

	lookup := &fakeWorkLookup{result: discovery.DiscoveredWork{Work: work.Work{
		Title:   "Recovered Citation Title",
		Authors: []string{"Adams, A."},
		Year:    2026,
	}}}
	svc.Discovery = lookup

	svc.EnsureCitationMetadata(ctx, jobID)

	after, err := jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Work.Title != "Recovered Citation Title" {
		t.Fatalf("title = %q, want the looked-up citation title", after.Work.Title)
	}
	if len(after.Work.Authors) == 0 {
		t.Fatalf("authors = %v, want the looked-up authors", after.Work.Authors)
	}

	// A job that already has a citation must not spend a provider request.
	before := lookup.calls
	svc.EnsureCitationMetadata(ctx, jobID)
	if lookup.calls != before {
		t.Fatalf("lookup calls = %d, want %d: a job with a title must not be looked up again", lookup.calls, before)
	}
}
