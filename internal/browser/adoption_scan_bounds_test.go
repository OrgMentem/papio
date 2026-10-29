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

	"papio/internal/grab"
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
