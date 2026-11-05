// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build unix

package browser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCopyGrabFileNeverExposesPartialBytesAtTheAdoptionName is the kill-point
// test: it observes the destination directory at the instant a SIGKILL would
// land, with bytes written and the copy not finished. Writing in place put a
// truncated file at the final adoption name, where the poll-time scan reads it
// as the user's settled download. The copy is held open mid-stream through a
// FIFO source — the only portable-enough way to stop io.Copy deterministically
// rather than racing a large file — so the assertions never depend on timing.
// Both publication paths run: the hard link, and the real exclusive rename a
// volume without hard links gets.
func TestCopyGrabFileNeverExposesPartialBytesAtTheAdoptionName(t *testing.T) {
	t.Run("HardLink", func(t *testing.T) { assertCopyNeverExposesPartialBytes(t, nil) })
	t.Run("ExclusiveRenameWithoutHardLinks", func(t *testing.T) { assertCopyNeverExposesPartialBytes(t, linkUnsupported) })
}

func assertCopyNeverExposesPartialBytes(t *testing.T, link func(string, string) error) {
	t.Helper()
	b, jobs, cfg, _ := newBridge(t)
	b.linkFile = link
	ctx := context.Background()
	id := park(t, jobs, "wr_killpoint", handoffWork())
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "paper.pdf")

	fifo := filepath.Join(t.TempDir(), "grab.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this filesystem: %v", err)
	}
	head := []byte("%PDF-1.4\nfirst chunk")
	tail := []byte("\nrest of the document\n%%EOF")

	copyDone := make(chan error, 1)
	go func() { copyDone <- b.copyGrabFile(fifo, dst) }()

	// Opening for write completes only once copyGrabFile has opened the read
	// end, so this also synchronises the start of the copy.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(head); err != nil {
		t.Fatal(err)
	}

	// Wait until those bytes are somewhere on disk. The copy is then provably
	// mid-stream — bytes written, source still open — which is the instant a
	// SIGKILL would freeze. Whatever holds them must be dot-prefixed.
	deadline := time.Now().Add(5 * time.Second)
	staged := ""
	for time.Now().Before(deadline) && staged == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && info.Size() >= int64(len(head)) {
				staged = e.Name()
			}
		}
		if staged == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if staged == "" {
		_ = w.Close()
		t.Fatal("the written bytes never reached disk; the copy did not run mid-stream")
	}
	if !strings.HasPrefix(staged, ".") {
		_ = w.Close()
		t.Fatalf("partial copy visible at an adoption name: %q", staged)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		_ = w.Close()
		t.Fatalf("destination exists while the copy is still streaming: %v", err)
	}
	if name, ok := b.scanAdoptionDir(ctx, id); ok {
		t.Fatalf("adoption scan selected %q from an unfinished copy", name)
	}

	if _, err := w.Write(tail); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-copyDone:
		if err != nil {
			t.Fatalf("copyGrabFile: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyGrabFile never returned after the source closed")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination missing after the copy completed: %v", err)
	}
	if !bytes.Equal(got, append(append([]byte{}, head...), tail...)) {
		t.Fatalf("destination holds %q, want every byte written to the source", got)
	}
	if leftover := stagingTempNames(t, dir); len(leftover) != 0 {
		t.Fatalf("staging temporaries left behind: %v", leftover)
	}
	if name, ok := b.scanAdoptionDir(ctx, id); !ok || name != "paper.pdf" {
		t.Fatalf("adoption scan = %q, %v after the copy completed; want paper.pdf, true", name, ok)
	}
}
