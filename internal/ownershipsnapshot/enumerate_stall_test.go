// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// A file read stuck in the kernel must not wedge the caller, and a second
// enumeration for the same path while the first read is still blocked must
// fail fast instead of joining it. Both callers run on the shared maintenance
// goroutine, where one wedged read would stop every later runner.
func TestEnumerateFileReadStallFailsFast(t *testing.T) {
	oldRead := enumerateRead
	release := make(chan struct{})
	// finished closes when the worker's read returns, proving the worker
	// touches the seam no more; the cleanup below waits for it before
	// restoring the package var, so no goroutine can race the restore.
	finished := make(chan struct{})
	enumerateRead = func(_ context.Context, reader io.Reader) ([]byte, error) {
		<-release
		defer close(finished)
		return readBounded(context.Background(), reader)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-finished
		enumerateRead = oldRead
	})
	path := writeExport(t, "lib.bib", oneEntryBibTeX("10.1000/one"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := EnumerateBibliographicRecords(ctx, fileSource(path, "bibtex"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("blocked enumeration took %v; it must return at its context", elapsed)
	}
	if err == nil {
		t.Fatal("blocked enumeration succeeded, want a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked error = %v; want the caller's timeout", err)
	}

	// The first read is still blocked: the next enumeration must not wait.
	roomy, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	start = time.Now()
	_, err = EnumerateBibliographicRecords(roomy, fileSource(path, "bibtex"))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("second enumeration took %v; it must fail fast while the first read is blocked", elapsed)
	}
	if err == nil {
		t.Fatal("second enumeration succeeded, want an immediate timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second error = %v; want context.DeadlineExceeded", err)
	}
	close(release)
}
