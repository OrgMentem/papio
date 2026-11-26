// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/work"
)

func landAdoptionPDF(t *testing.T, svc *Service, id, label string) string {
	t.Helper()
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, pdfBytes(label), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The pdf_sanitized event is the provenance for rewritten bytes: when it
// cannot be recorded the adoption must fail rather than file the rewrite.
func TestSanitizeProvenanceFailureAbortsAdoption(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	id := parkAwaitingHuman(t, jobs, "wr_sanitize_provenance_failure")
	path := landAdoptionPDF(t, svc, id, "embedded provenance")
	validations := 0
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		validations++
		report, err := passValidation()(context.Background(), "", "", work.Work{})
		if err != nil {
			return pdf.ValidationReport{}, err
		}
		if validations == 1 {
			report.Structural.HasEmbeddedFiles = true
		}
		return report, nil
	}
	svc.Sanitize = func(_ context.Context, src, dst string) (pdf.StructuralReport, error) {
		data, err := os.ReadFile(src)
		if err != nil {
			return pdf.StructuralReport{}, err
		}
		if err := os.WriteFile(dst, append(data, []byte(" sanitized")...), 0o600); err != nil {
			return pdf.StructuralReport{}, err
		}
		return pdf.StructuralReport{Valid: true, Pages: 2}, nil
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_sanitized_event BEFORE INSERT ON events
		WHEN NEW.kind = 'job.pdf_sanitized'
		BEGIN SELECT RAISE(ABORT, 'injected provenance failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := svc.AdoptDownload(ctx, id, path)
	if err == nil || !strings.Contains(err.Error(), "injected provenance failure") {
		t.Fatalf("AdoptDownload = %v, want the provenance failure", err)
	}
	row, getErr := jobs.Get(ctx, id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if row.State == job.StateReady || row.State == job.StateImported {
		t.Fatalf("job reached %s without sanitization provenance", row.State)
	}
	if _, statErr := os.Stat(path + ".sanitized"); statErr == nil {
		t.Fatal("sanitized rewrite left behind after provenance failure")
	}
}

// A validation outcome already carried by the job's state must not be
// disturbed when its attempt row cannot be settled: the failure is logged,
// and the park or rejection handling still runs.
func TestValidationOutcomeSurvivesFinishAttemptFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate      func(*pdf.ValidationReport)
		wantState   string
		wantOutcome string
	}{
		"unsafe_pdf": {func(r *pdf.ValidationReport) { r.Structural.HasEmbeddedFiles = true }, job.StateNeedsReview, "needs_review"},
		"invalid":    {func(r *pdf.ValidationReport) { r.Structural.Valid = false }, job.StateAwaitingHuman, "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			mutate := tc.mutate
			svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				r, err := passValidation()(context.Background(), "", "", work.Work{})
				if err != nil {
					return pdf.ValidationReport{}, err
				}
				mutate(&r)
				return r, nil
			}
			id := parkAwaitingHuman(t, jobs, "wr_finish_failure_"+name)
			path := landAdoptionPDF(t, svc, id, name)
			if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_validate_finish BEFORE UPDATE OF ended_at ON attempts
				WHEN NEW.stage = 'validate'
				BEGIN SELECT RAISE(ABORT, 'injected validate finish failure'); END`); err != nil {
				t.Fatal(err)
			}
			err := svc.AdoptDownload(ctx, id, path)
			if err != nil && strings.Contains(err.Error(), "injected validate finish failure") {
				t.Fatalf("AdoptDownload = %v; the settlement failure must not change the outcome's control flow", err)
			}
			row, getErr := jobs.Get(ctx, id)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if row.State != tc.wantState {
				t.Fatalf("job state = %s, want %s", row.State, tc.wantState)
			}
			if tc.wantState == job.StateAwaitingHuman {
				if open := openActionIDs(t, jobs, id, "manual_download"); len(open) != 1 {
					t.Fatalf("open manual_download actions = %v; the rejection must reopen the prompt", open)
				}
			}
			events, evErr := jobs.Events(ctx, id)
			if evErr != nil {
				t.Fatal(evErr)
			}
			found := false
			for _, event := range events {
				if event["kind"] != "attempt.settlement_failed" {
					continue
				}
				detail, _ := event["detail"].(map[string]any)
				if detail["intended_outcome"] == tc.wantOutcome && detail["attempt_id"] != nil {
					found = true
				}
			}
			if !found {
				t.Fatalf("no attempt.settlement_failed event for outcome %q in %v", tc.wantOutcome, events)
			}
		})
	}
}

// An unreadable event stream proves nothing: it must not read as "the sibling
// search already ran" nor as "the hook already dispatched".
func TestEventReadFailureIsNotProofOfPriorWork(t *testing.T) {
	svc, jobs := newTestService(t)
	id := createFilingStateJob(t, jobs, "wr_event_read_failure", job.StateReady)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if recorded, err := svc.siblingSearchRecorded(cancelled, id, "basis"); err == nil || recorded {
		t.Fatalf("siblingSearchRecorded = %v, %v; want an error and not recorded", recorded, err)
	}
	svc.ReadyHook = readyHookRunnerForTest()
	if dispatched, err := svc.readyHookDispatched(cancelled, id); err == nil || dispatched {
		t.Fatalf("readyHookDispatched = %v, %v; want an error and not dispatched", dispatched, err)
	}
}

// A refused hook admission (shutdown) must leave no dispatch marker, or
// recovery would never launch the hook the marker claims was queued.
func TestReadyHookRefusedAdmissionLeavesJobRecoverable(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.ReadyHook = readyHookRunnerForTest()
	id := createFilingStateJob(t, jobs, "wr_ready_hook_closed", job.StateReady)
	seedReadyArtifact(t, svc, jobs, id, "%PDF-1.4\nclosed\n%%EOF")
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	svc.CloseHookAdmission()
	svc.runReadyHook(ctx, row, row.ArtifactSHA256)
	if dispatched, err := svc.readyHookDispatched(ctx, id); err != nil || dispatched {
		t.Fatalf("readyHookDispatched = %v, %v; a refused admission must leave no marker", dispatched, err)
	}
}

// A failed manual-lease release is reported with the route outcome.
func TestSubmitDeliverySurfacesLeaseReleaseFailure(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusOK, `{"TransactionNumber":4242}`)
	svc, jobs, _ := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_submit_release_failure", "10.1000/submit-release-failure"))
	if err != nil {
		t.Fatal(err)
	}
	// The route fails (the retry_wait park is refused), which leaves the job
	// resolving under the manual lease; the release is then refused by a
	// separate trigger that only matches a same-state lease clear.
	if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_route_park BEFORE UPDATE OF state ON jobs
		WHEN NEW.state = 'retry_wait'
		BEGIN SELECT RAISE(ABORT, 'injected route failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_lease_release BEFORE UPDATE OF lease_owner ON jobs
		WHEN NEW.lease_owner IS NULL AND NEW.state = OLD.state AND OLD.lease_owner LIKE '%-' || OLD.id
		BEGIN SELECT RAISE(ABORT, 'injected release failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = svc.SubmitDelivery(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "injected route failure") || !strings.Contains(err.Error(), "injected release failure") {
		t.Fatalf("SubmitDelivery = %v, want both the route and the release failure", err)
	}
}

// parkForAdoption must not report a job parked when its action cannot open.
func TestParkForAdoptionSurfacesActionFailure(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	id, err := jobs.CreateRequest(ctx, "wr_park_action_failure", work.Work{DOI: "10.1002/example"}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_action BEFORE INSERT ON human_actions
		BEGIN SELECT RAISE(ABORT, 'injected action failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.parkForAdoption(ctx, id, "browser_download_adoption"); err == nil || !strings.Contains(err.Error(), "injected action failure") {
		t.Fatalf("parkForAdoption = %v, want the action failure", err)
	}
}

func readyHookRunnerForTest() *hook.Runner {
	return &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, _ []string) hook.Result {
			return hook.Result{Ran: true}
		},
	}
}

// One job with an unreadable event stream must not fail startup recovery or
// stop other jobs from filing.
func TestReadyHookRecoverySkipsJobWithCorruptEvents(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	var ran atomic.Int32
	svc.ReadyHook = &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, _ []string) hook.Result {
			ran.Add(1)
			return hook.Result{Ran: true}
		},
	}
	if err := svc.RecoverPreparedPublications(ctx); err != nil { // records the hook epoch
		t.Fatal(err)
	}
	corrupt := createFilingStateJob(t, jobs, "wr_ready_hook_corrupt", job.StateReady)
	seedReadyArtifact(t, svc, jobs, corrupt, "%PDF-1.4\ncorrupt\n%%EOF")
	healthy := createFilingStateJob(t, jobs, "wr_ready_hook_healthy", job.StateReady)
	seedReadyArtifact(t, svc, jobs, healthy, "%PDF-1.4\nhealthy\n%%EOF")
	if _, err := jobs.S.DB().ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'zotio.auto_import', 'not json')`,
		corrupt, store.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverPreparedPublications(ctx); err != nil {
		t.Fatalf("startup recovery failed on one job's corrupt events: %v", err)
	}
	if !svc.DrainHooks(5 * time.Second) {
		t.Fatal("hooks did not drain")
	}
	if got := waitForHookEvents(t, jobs, healthy, 1); len(got) != 1 {
		t.Fatalf("healthy job hook events = %#v, want one", got)
	}
	if ran.Load() != 1 {
		t.Fatalf("hook ran %d times, want only the healthy job", ran.Load())
	}
}

// A dispatch marker with no outcome and no live run is a crash between the
// marker and the launch: recovery must run the hook again.
func TestReadyHookRecoveryRedispatchesMarkerWithoutOutcome(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.ReadyHook = readyHookRunnerForTest()
	if err := svc.RecoverPreparedPublications(ctx); err != nil { // records the hook epoch
		t.Fatal(err)
	}
	id := createFilingStateJob(t, jobs, "wr_ready_hook_marker_only", job.StateReady)
	seedReadyArtifact(t, svc, jobs, id, "%PDF-1.4\nmarker only\n%%EOF")
	if err := jobs.RecordEvent(ctx, id, "hook.on_ready_dispatched", map[string]any{"trigger": "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverPreparedPublications(ctx); err != nil {
		t.Fatal(err)
	}
	if !svc.DrainHooks(5 * time.Second) {
		t.Fatal("recovered hook did not drain")
	}
	if got := waitForHookEvents(t, jobs, id, 1); got[0]["trigger"] != "ready" {
		t.Fatalf("hook events = %#v, want one recovered ready run", got)
	}
	// With an outcome recorded, a further recovery must not run it again.
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.recoverMissingReadyHook(ctx, row); err != nil {
		t.Fatal(err)
	}
	if !svc.DrainHooks(time.Second) {
		t.Fatal("unexpected hook work")
	}
	if got := waitForHookEvents(t, jobs, id, 1); len(got) != 1 {
		t.Fatalf("hook events = %#v, want exactly one run", got)
	}
}

// Crash recovery that finds the artifact name already published must flush it
// before consuming the journal; while the flush keeps failing the journal
// stays and the job is not ready.
func TestPreparedPublicationRecoveryKeepsJournalWhenFlushFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block opens on Windows, and SyncDir is a no-op there")
	}
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	ctx := context.Background()
	svc, jobs := newTestService(t)
	row, candidate, body, tempPath, sha := seedValidatingCandidate(t, svc, jobs, "wr_recovery_flush", "recovery-flush", "recovery-flush")
	dest, err := svc.Artifacts.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		// Promote fails (no quarantine file) after another publisher exposed
		// the destination name, leaving the journal row behind.
		if err := os.Remove(tempPath); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, body, 0o400); err != nil {
			t.Fatal(err)
		}
		return passValidation()(context.Background(), "", "", work.Work{})
	}
	if _, _, err := svc.validateCandidate(ctx, row, candidate, fetch.Result{
		TempPath: tempPath, SHA256: sha, SizeBytes: int64(len(body)), SniffedMIME: "application/pdf",
	}); err == nil {
		t.Fatal("validation succeeded despite promotion failure")
	}
	dir := filepath.Dir(dest)
	// Traversal still works, so the destination verifies; the directory
	// flush cannot open the directory for reading.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := svc.reconcilePreparedPublications(ctx, row.ID); err == nil {
		t.Fatal("recovery consumed the journal without a successful flush")
	}
	prepared, err := jobs.PreparedPublications(ctx, row.ID)
	if err != nil || len(prepared) != 1 {
		t.Fatalf("prepared publications = %+v, %v; want the journal kept", prepared, err)
	}
	if got, err := jobs.Get(ctx, row.ID); err != nil || got.State == job.StateReady {
		t.Fatalf("job = %+v, %v; must not be ready while the flush fails", got, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := svc.reconcilePreparedPublications(ctx, row.ID); err != nil {
		t.Fatalf("recovery after the flush works: %v", err)
	}
	if got, err := jobs.Get(ctx, row.ID); err != nil || got.State != job.StateReady {
		t.Fatalf("job = %+v, %v; want ready after a successful flush", got, err)
	}
}

// A corrupt job.transition or hook.on_ready row must not fail the recovery
// scan itself: the query parses around it and the per-job read skips the job.
func TestReadyHookRecoveryScanSurvivesCorruptTransitionAndOutcomeRows(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.ReadyHook = readyHookRunnerForTest()
	if err := svc.RecoverPreparedPublications(ctx); err != nil { // records the hook epoch
		t.Fatal(err)
	}
	corrupt := createFilingStateJob(t, jobs, "wr_ready_hook_scan_corrupt", job.StateReady)
	seedReadyArtifact(t, svc, jobs, corrupt, "%PDF-1.4\nscan corrupt\n%%EOF")
	healthy := createFilingStateJob(t, jobs, "wr_ready_hook_scan_healthy", job.StateReady)
	seedReadyArtifact(t, svc, jobs, healthy, "%PDF-1.4\nscan healthy\n%%EOF")
	for _, kind := range []string{"job.transition", "hook.on_ready"} {
		if _, err := jobs.S.DB().ExecContext(ctx,
			`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, 'not json')`,
			corrupt, store.Now(), kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RecoverPreparedPublications(ctx); err != nil {
		t.Fatalf("startup recovery failed on corrupt rows: %v", err)
	}
	if !svc.DrainHooks(5 * time.Second) {
		t.Fatal("hooks did not drain")
	}
	if got := waitForHookEvents(t, jobs, healthy, 1); len(got) != 1 {
		t.Fatalf("healthy job hook events = %#v, want one", got)
	}
}

// A hook process that outlived the daemon keeps its filing reservation, which
// fences marker-only replay; once the reservation is released, replay runs.
func TestReadyHookReplayIsFencedByUnresolvedReservation(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	var ran atomic.Int32
	svc.ReadyHook = &hook.Runner{
		Command: "configured",
		Exec: func(_ context.Context, _ string, _ []string) hook.Result {
			ran.Add(1)
			return hook.Result{Ran: true}
		},
	}
	id := createFilingStateJob(t, jobs, "wr_ready_hook_fenced", job.StateReady)
	seedReadyArtifact(t, svc, jobs, id, "%PDF-1.4\nfenced\n%%EOF")
	if err := jobs.RecordEvent(ctx, id, "hook.on_ready_dispatched", map[string]any{"trigger": "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "hook.filing_reserved", map[string]any{"reservation_id": "hook_survivor"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.recoverMissingReadyHook(ctx, row); err != nil {
		t.Fatal(err)
	}
	if !svc.DrainHooks(time.Second) || ran.Load() != 0 {
		t.Fatalf("replay ran %d hooks beside a surviving reservation", ran.Load())
	}
	if err := jobs.RecordEvent(ctx, id, "hook.filing_released", map[string]any{"reservation_id": "hook_survivor"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.recoverMissingReadyHook(ctx, row); err != nil {
		t.Fatal(err)
	}
	if !svc.DrainHooks(5 * time.Second) {
		t.Fatal("replayed hook did not drain")
	}
	if got := waitForHookEvents(t, jobs, id, 1); len(got) != 1 || ran.Load() != 1 {
		t.Fatalf("after release: events %#v, ran %d; want one replay", got, ran.Load())
	}
}
