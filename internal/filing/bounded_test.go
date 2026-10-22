// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	source, folder, key, sha string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	body := []byte("%PDF-1.4\nbounded fixture\n%%EOF")
	source := filepath.Join(root, "source.pdf")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	folder := filepath.Join(root, "collection")
	return fixture{source: source, folder: folder, key: Key("job_bounded", folder, sha), sha: sha}
}

// wedge makes the destination's op step block in the "kernel": it ignores the
// context and returns only when release is closed. The deadline is compressed
// and everything is restored once the wedged worker has drained.
func wedge(t *testing.T, op string, deadline time.Duration) (release func(), calls *atomic.Int32) {
	t.Helper()
	oldHook, oldDeadline := beforeIO, StallDeadline
	hold := make(chan struct{})
	calls = new(atomic.Int32)
	StallDeadline = deadline
	beforeIO = func(got string) {
		if got == op {
			calls.Add(1)
			<-hold
		}
	}
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			close(hold)
		}
	}
	t.Cleanup(func() {
		release()
		waitFor(t, func() bool { return !Stalled() && len(gate) == 0 })
		beforeIO, StallDeadline = oldHook, oldDeadline
	})
	return release, calls
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStageOnStalledDestinationFailsFastLatchesAndRecovers(t *testing.T) {
	f := newFixture(t)
	release, calls := wedge(t, "mkdir", 50*time.Millisecond)

	start := time.Now()
	if _, err := Stage(context.Background(), f.source, f.folder, f.key, f.sha); !errors.Is(err, ErrDestinationStalled) {
		t.Fatalf("stalled Stage = %v, want ErrDestinationStalled", err)
	}
	if !Stalled() {
		t.Fatal("a stalled call must latch")
	}
	for range 3 {
		if _, err := Stage(context.Background(), f.source, f.folder, f.key, f.sha); !errors.Is(err, ErrDestinationStalled) {
			t.Fatalf("latched Stage = %v, want ErrDestinationStalled", err)
		}
	}
	if err := CleanupStages(context.Background(), f.folder, f.key); !errors.Is(err, ErrDestinationStalled) {
		t.Fatalf("latched CleanupStages = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stalled calls were not bounded")
	}
	if calls.Load() != 1 {
		t.Fatalf("destination entered %d times, want exactly 1 stuck call", calls.Load())
	}

	release()
	waitFor(t, func() bool { return !Stalled() })
	// The abandoned worker finished its copy after the caller left; nobody
	// will publish that partial, so it must not outlive the call.
	if entries, err := os.ReadDir(f.folder); err != nil || len(entries) != 0 {
		t.Fatalf("abandoned Stage left %v, %v", entries, err)
	}
	staged, err := Stage(context.Background(), f.source, f.folder, f.key, f.sha)
	if err != nil {
		t.Fatalf("Stage after recovery: %v", err)
	}
	staged.Cleanup()
}

func TestStageReturnsOnCancelWhileDestinationBlocked(t *testing.T) {
	f := newFixture(t)
	wedge(t, "mkdir", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := Stage(ctx, f.source, f.folder, f.key, f.sha); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation did not release the caller from a blocked destination")
	}
}

// A late link must not leave a file the caller was told was never published:
// the receipt is written failed, and the job may since have been archived.
func TestAbandonedPublishUndoesLateLink(t *testing.T) {
	f := newFixture(t)
	staged, err := Stage(context.Background(), f.source, f.folder, f.key, f.sha)
	if err != nil {
		t.Fatal(err)
	}
	release, _ := wedge(t, "link", 50*time.Millisecond)
	if err := staged.Publish(context.Background()); !errors.Is(err, ErrDestinationStalled) {
		t.Fatalf("stalled Publish = %v", err)
	}
	release()
	waitFor(t, func() bool { return !Stalled() })
	if _, err := os.Lstat(staged.Target); !os.IsNotExist(err) {
		t.Fatalf("abandoned Publish left its target behind: %v", err)
	}
	if entries, err := os.ReadDir(f.folder); err != nil || len(entries) != 0 {
		t.Fatalf("abandoned Publish left %v, %v", entries, err)
	}
}

func TestStallBoundMeasuresProgressNotElapsedTime(t *testing.T) {
	old := StallDeadline
	StallDeadline = 600 * time.Millisecond
	t.Cleanup(func() { StallDeadline = old })

	got, err := run(context.Background(), func(_ context.Context, w *watch) (int, error) {
		for range 8 {
			time.Sleep(100 * time.Millisecond)
			w.touch()
		}
		return 7, nil
	}, nil)
	if err != nil || got != 7 {
		t.Fatalf("steady progress past the deadline = %d, %v; want it to finish", got, err)
	}

	release := make(chan struct{})
	_, err = run(context.Background(), func(context.Context, *watch) (int, error) {
		<-release
		return 0, nil
	}, nil)
	if !errors.Is(err, ErrDestinationStalled) {
		t.Fatalf("no progress = %v, want ErrDestinationStalled", err)
	}
	close(release)
	waitFor(t, func() bool { return !Stalled() && len(gate) == 0 })
}
