// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

// A landing-directory listing that never completed (latched, gate held, or
// blocked in-kernel) says nothing about what the directory holds. For an
// awaiting grab that already means "wait"; for a quarantined grab the sweep
// must not fall through to "no landing file, process from quarantine" — that
// path stages into the adoption root and removes the landing directory it
// never saw. The same function's quarantine-only fallback is already skipped
// while the latch is unhealthy.
func TestSweepGrabsLeavesQuarantinedGrabWhenLandingListingTimesOut(t *testing.T) {
	disableAutoBindDecision(t)
	compressAdoptionScanDeadline(t)
	b, _, cfg, _ := newBridge(t)
	ctx := context.Background()
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Excerpt: "a paper with no identifier"},
		}, nil
	}
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Blocked landing listing")
	if err != nil {
		t.Fatal(err)
	}
	landing := filepath.Join(cfg.EffectiveAdoptionRoot(), grabsDirName, g.ID)
	landed := filepath.Join(landing, "paper.pdf")
	writeFixturePDF(t, landed)
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}

	// Only the landing directory blocks, so the root listing that finds the
	// grab succeeds: the timeout lands mid-loop, after the sweep committed to
	// looking at this grab.
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == landing {
			<-release
		}
		return os.ReadDir(dir)
	}

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.adoptionLatchUnhealthy() {
		t.Fatal("blocked landing listing did not latch")
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %+v, %v", got, err)
	}
	if got.State != grab.StateQuarantined {
		t.Fatalf("grab state = %s, want quarantined: an unlisted landing directory is not an empty one", got.State)
	}
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("landing file removed without being listed: %v", err)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("quarantine copy lost: %v", err)
	}

	// Once the blocked listing returns the latch clears and the next tick
	// finishes the grab.
	unblock()
	deadline := time.Now().Add(2 * time.Second)
	for b.adoptionLatchUnhealthy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.adoptionLatchUnhealthy() {
		t.Fatal("latch never cleared after the blocked listing returned")
	}
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil || got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab after recovery = %+v, %v; want parked_no_identifier", got, err)
	}
}

// A timeout is not the only way a listing fails to happen. A transient
// permission or I/O failure on grabs/<id> says exactly as little about the
// directory's contents, and the sweep used to read it as "empty": a quarantined
// grab then fell through to the quarantine-only fallback, which stages the
// quarantine copy and removes the landing directory it never saw. Any landed
// PDF in it — possibly a different file than the quarantined one — went with it.
func TestSweepGrabsLeavesQuarantinedGrabWhenLandingListingFails(t *testing.T) {
	disableAutoBindDecision(t)
	b, _, cfg, _ := newBridge(t)
	ctx := context.Background()
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Excerpt: "a paper with no identifier"},
		}, nil
	}
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Unreadable landing listing")
	if err != nil {
		t.Fatal(err)
	}
	landing := filepath.Join(cfg.EffectiveAdoptionRoot(), grabsDirName, g.ID)
	landed := filepath.Join(landing, "a-different-paper.pdf")
	writeFixturePDF(t, landed)
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}

	// Only the landing directory fails, so the root listing that finds the grab
	// succeeds: the failure lands mid-loop, after the sweep committed to looking
	// at this grab. The error returns immediately, so nothing latches and the
	// quarantine-only recovery pass is reached with the grab still pending.
	var failLanding atomic.Bool
	failLanding.Store(true)
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == landing && failLanding.Load() {
			return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrPermission}
		}
		return os.ReadDir(dir)
	}

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if b.adoptionLatchUnhealthy() {
		t.Fatal("a fast listing error must not latch the bridge")
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %+v, %v", got, err)
	}
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("landing file consumed without being listed: %v", err)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("quarantine copy lost: %v", err)
	}
	if got.State != grab.StateQuarantined {
		t.Fatalf("grab state = %s, want quarantined: an unreadable landing directory is not an empty one", got.State)
	}

	// Deferral, not strand: the next tick that can read the directory finishes
	// the grab.
	failLanding.Store(false)
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	got, err = b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil || got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab after recovery = %+v, %v; want parked_no_identifier", got, err)
	}
}

// Confirmed absence is the one conclusive answer a failed listing can carry: a
// landing directory removed under the sweep holds nothing, so the
// quarantine-only fallback may act on it. This is the half of the rule that
// must keep working — an old crash can leave a grab whose only copy is in
// quarantine, and refusing to process those would strand the paper.
func TestSweepGrabsProcessesQuarantineWhenLandingDirectoryIsGone(t *testing.T) {
	disableAutoBindDecision(t)
	b, _, cfg, _ := newBridge(t)
	ctx := context.Background()
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Excerpt: "a paper with no identifier"},
		}, nil
	}
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Vanished landing directory")
	if err != nil {
		t.Fatal(err)
	}
	landing := filepath.Join(cfg.EffectiveAdoptionRoot(), grabsDirName, g.ID)
	writeFixturePDF(t, filepath.Join(landing, "paper.pdf"))
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == landing {
			return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrNotExist}
		}
		return os.ReadDir(dir)
	}

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil || got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab = %+v, %v; want parked_no_identifier from the quarantine copy", got, err)
	}
}

// Terminal cleanup is the one sweep that deletes bytes, so the two nested
// listings that authorise it must both have completed. A committed auto-bind
// whose job never reached ready has no copy in the artifact store: the landing
// (or rejected) file is it. Reading an unreadable directory as empty turned a
// transient permission or consent failure into permanent loss of the only copy.
func TestSweepTerminalAdoptionsKeepsCommittedGrabBytesWhenNestedScanFails(t *testing.T) {
	type fixture struct {
		b     *Bridge
		jobs  *job.Store
		cfg   config.Config
		jobID string
	}
	// A committed auto-bind (grab job_created) whose job went terminal without
	// an artifact: exactly the shape the cleanup guard exists for.
	setup := func(t *testing.T, reqID string) fixture {
		t.Helper()
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, reqID, handoffWork())
		bindGrab(t, b, id, "Committed auto-bind", "")
		if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, job.StateUnavailable,
			map[string]any{"reason": "exhausted"}, job.WithTerminalReason("no_entitlement")); err != nil {
			t.Fatal(err)
		}
		return fixture{b: b, jobs: jobs, cfg: cfg, jobID: id}
	}

	t.Run("LandingListingFails", func(t *testing.T) {
		f := setup(t, "wr_terminal_landing_unreadable")
		landing := filepath.Join(f.cfg.EffectiveAdoptionRoot(), f.jobID)
		landed := filepath.Join(landing, "by-hand.pdf")
		writeFixturePDF(t, landed)
		f.b.readDir = func(dir string) ([]os.DirEntry, error) {
			if dir == landing {
				return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrPermission}
			}
			return os.ReadDir(dir)
		}

		if err := f.b.SweepTerminalAdoptions(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if _, err := os.Stat(landed); err != nil {
			t.Fatalf("the only copy of a committed grab's bytes was deleted after an unreadable listing: %v", err)
		}
	})

	// The reachable production trigger. A genuine permission denial also fails
	// the RemoveAll, so the bytes survive by luck; a listing that merely did
	// not happen in time does not — the latch tripping mid-sweep (or another
	// goroutine holding the single-flight gate for a full deadline) returns the
	// timeout instantly for a directory that is perfectly removable.
	t.Run("LandingListingTimesOut", func(t *testing.T) {
		compressAdoptionScanDeadline(t)
		f := setup(t, "wr_terminal_landing_timeout")
		landing := filepath.Join(f.cfg.EffectiveAdoptionRoot(), f.jobID)
		landed := filepath.Join(landing, "by-hand.pdf")
		writeFixturePDF(t, landed)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		f.b.readDir = func(dir string) ([]os.DirEntry, error) {
			if dir == landing {
				<-release
			}
			return os.ReadDir(dir)
		}

		if err := f.b.SweepTerminalAdoptions(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if !f.b.adoptionLatchUnhealthy() {
			t.Fatal("blocked landing listing did not latch; the test did not exercise the timeout path")
		}
		if _, err := os.Stat(landed); err != nil {
			t.Fatalf("the only copy of a committed grab's bytes was deleted after a timed-out listing: %v", err)
		}
	})

	t.Run("RejectedListingFails", func(t *testing.T) {
		f := setup(t, "wr_terminal_rejected_unreadable")
		landing := filepath.Join(f.cfg.EffectiveAdoptionRoot(), f.jobID)
		if err := os.MkdirAll(landing, 0o700); err != nil {
			t.Fatal(err)
		}
		// Keyed on the directory's shape, not its parent, so this stays a test
		// of the error rule rather than of where rejected/ is looked for.
		f.b.readDir = func(dir string) ([]os.DirEntry, error) {
			if filepath.Base(dir) == f.jobID && filepath.Base(filepath.Dir(dir)) == "rejected" {
				return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrPermission}
			}
			return os.ReadDir(dir)
		}

		if err := f.b.SweepTerminalAdoptions(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if _, err := os.Stat(landing); err != nil {
			t.Fatalf("landing directory collected while the rejected sibling was unreadable: %v", err)
		}
	})

	t.Run("BothListingsSucceedEmpty", func(t *testing.T) {
		f := setup(t, "wr_terminal_empty_collected")
		landing := filepath.Join(f.cfg.EffectiveAdoptionRoot(), f.jobID)
		if err := os.MkdirAll(landing, 0o700); err != nil {
			t.Fatal(err)
		}

		if err := f.b.SweepTerminalAdoptions(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if _, err := os.Stat(landing); !os.IsNotExist(err) {
			t.Fatalf("a committed grab's empty landing directory survived cleanup: %v", err)
		}
	})
}

// The latch check and the single-flight gate are two steps, but the gate alone
// bounds the work: while one scan is blocked, every concurrent caller either
// sees the latch or times out waiting for the gate, and none starts a second
// listing. adoptionScanMu is deliberately not held across the gate wait: the
// blocked worker needs it to release the gate, and every latch reader would
// queue behind a wait of up to a full deadline.
func TestReadAdoptionDirConcurrentCallersNeverStartSecondBlockedListing(t *testing.T) {
	compressAdoptionScanDeadline(t)
	b, _, cfg, _ := newBridge(t)
	dir := cfg.EffectiveAdoptionRoot()

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	b.readDir = func(string) ([]os.DirEntry, error) {
		calls.Add(1)
		<-release
		return nil, nil
	}

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = b.readAdoptionDir(dir)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrAdoptionScanTimeout) {
			t.Fatalf("caller %d err = %v, want ErrAdoptionScanTimeout", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("readDir ran %d times while the first was blocked, want 1", got)
	}
	if !b.adoptionLatchUnhealthy() {
		t.Fatal("blocked listing did not latch")
	}

	unblock()
	deadline := time.Now().Add(2 * time.Second)
	for b.adoptionLatchUnhealthy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := b.readAdoptionDir(dir); err != nil {
		t.Fatalf("fresh listing after the blocked one returned: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("readDir ran %d times, want 2 (one blocked, one fresh)", got)
	}
}

// BoundedReadDir's result channel has room for the one result, so the worker
// exits as soon as readDir returns — success or error, with or without an
// afterTimeout reader. Only the syscall itself can hold a goroutine.
func TestBoundedReadDirWorkerExitsAfterLateResult(t *testing.T) {
	compressAdoptionScanDeadline(t)
	for _, tc := range []struct {
		name string
		err  error
	}{{"entries", nil}, {"error", os.ErrPermission}} {
		t.Run(tc.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			release := make(chan struct{})
			entered := make(chan struct{})
			_, err := BoundedReadDir("unused", func(string) ([]os.DirEntry, error) {
				close(entered)
				<-release
				return nil, tc.err
			}, nil)
			<-entered
			if !errors.Is(err, ErrAdoptionScanTimeout) {
				t.Fatalf("err = %v, want ErrAdoptionScanTimeout", err)
			}
			close(release)
			deadline := time.Now().Add(2 * time.Second)
			for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if after := runtime.NumGoroutine(); after > before {
				t.Fatalf("goroutines %d -> %d after the late result: the worker did not exit", before, after)
			}
		})
	}
}

// A hung scan costs the sweeper at most its own deadline: cancellation is
// observed on the next select while the blocked listing is still running.
func TestRunSweeperExitsOnCancelWhileScanIsBlocked(t *testing.T) {
	compressAdoptionScanDeadline(t)
	b, _, _, _ := newBridge(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	b.readDir = func(string) ([]os.DirEntry, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.RunSweeper(ctx, time.Millisecond) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSweeper returned %v after cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunSweeper still running after cancel while the scan stays blocked")
	}
	if !b.adoptionLatchUnhealthy() {
		t.Fatal("scan was never blocked; the test did not exercise the hung path")
	}
}

// boundedNativeIO releases its gate when the timed-out worker's run finally
// returns, whether or not the caller is still waiting: the worker, not the
// caller, owns the gate.
func TestNativeDownloadBoundedIOReleasesGateAfterTimedOutRunReturns(t *testing.T) {
	gate := make(chan struct{}, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	_, err := boundedNativeIO(context.Background(), gate, 20*time.Millisecond, func(context.Context) (int, error) {
		close(entered)
		<-release
		return 7, nil
	}, func(int) {})
	<-entered
	if !errors.Is(err, ErrAdoptionScanTimeout) {
		t.Fatalf("err = %v, want ErrAdoptionScanTimeout", err)
	}
	if len(gate) != 1 {
		t.Fatal("gate free while the timed-out run is still blocked")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for len(gate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got, err := boundedNativeIO(context.Background(), gate, time.Second, func(context.Context) (int, error) { return 8, nil }, func(int) {})
	if err != nil || got != 8 {
		t.Fatalf("fresh run after the timed-out one returned = %d, %v; want 8, nil", got, err)
	}
}
