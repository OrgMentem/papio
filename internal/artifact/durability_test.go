// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package artifact

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// publicationModes runs a body once with real hard links and once with the
// rename fallback, so each durability contract holds on both publication paths.
func publicationModes(t *testing.T, body func(t *testing.T, hardLinks bool)) {
	t.Helper()
	for _, hardLinks := range []bool{true, false} {
		name := "hard links"
		if !hardLinks {
			name = "rename fallback"
		}
		t.Run(name, func(t *testing.T) {
			if !hardLinks {
				original := linkFile
				linkFile = func(oldname, newname string) error {
					return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EOPNOTSUPP}
				}
				t.Cleanup(func() { linkFile = original })
			}
			body(t, hardLinks)
		})
	}
}

// exists reports whether a name is present, failing the test on any error that
// is not plain absence.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return false
}

// A power loss between publication and writeback must not leave a visible
// artifact name with missing bytes or a missing directory entry. The bytes are
// flushed before the destination can exist, and the directory is flushed only
// after the destination exists, so each flush protects what it is meant to.
func TestPromoteFlushesFileBeforePublicationAndDirectoryAfter(t *testing.T) {
	publicationModes(t, func(t *testing.T, _ bool) {
		s, temp, sha := stagedFixture(t)
		dest, err := s.ArtifactPath(sha)
		if err != nil {
			t.Fatal(err)
		}
		var events []string
		origFile, origDir := syncFile, syncDir
		syncFile = func(path string) error {
			events = append(events, "file:"+filepath.Base(path))
			if exists(t, dest) {
				t.Error("quarantine file flushed after the destination became visible")
			}
			return origFile(path)
		}
		syncDir = func(dir string) error {
			events = append(events, "dir:"+filepath.Base(dir))
			if !exists(t, dest) {
				t.Error("artifacts directory flushed before the destination became visible")
			}
			return origDir(dir)
		}
		t.Cleanup(func() { syncFile, syncDir = origFile, origDir })

		if _, created, err := s.Promote(temp, sha); err != nil || !created {
			t.Fatalf("promote = created %v, %v", created, err)
		}
		want := []string{"file:" + filepath.Base(temp), "dir:" + artifactsDir}
		if strings.Join(events, ",") != strings.Join(want, ",") {
			t.Fatalf("flush order = %v, want %v", events, want)
		}
	})
}

// A file whose bytes cannot be flushed must never become visible: the hash name
// would promise content the disk may not hold.
func TestPromoteFileFlushFailurePublishesNothing(t *testing.T) {
	publicationModes(t, func(t *testing.T, _ bool) {
		s, temp, sha := stagedFixture(t)
		dest, err := s.ArtifactPath(sha)
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected file flush failure")
		origFile, origDir := syncFile, syncDir
		syncFile = func(string) error { return injected }
		syncDir = func(string) error {
			t.Error("directory flushed after a failed file flush")
			return nil
		}
		t.Cleanup(func() { syncFile, syncDir = origFile, origDir })

		got, created, err := s.Promote(temp, sha)
		if !errors.Is(err, injected) || !strings.Contains(err.Error(), "syncing quarantine file") {
			t.Fatalf("promote error = %v, want the wrapped file flush failure", err)
		}
		if created || got != "" {
			t.Fatalf("promote reported path %q created=%v after a failed flush", got, created)
		}
		if exists(t, dest) {
			t.Fatal("destination exists although the file flush failed")
		}
		kept, err := os.ReadFile(temp)
		if err != nil || !bytes.Equal(kept, stagedFixtureBody) {
			t.Fatalf("quarantine source = %q, %v; want it intact for a retry", kept, err)
		}
	})
}

// A directory flush failure means the new name may not survive a crash, so
// Promote must not report success even though the destination is visible. The
// quarantine file stays so recovery still owns the bytes.
func TestPromoteDirectoryFlushFailureFailsPromotion(t *testing.T) {
	publicationModes(t, func(t *testing.T, hardLinks bool) {
		s, temp, sha := stagedFixture(t)
		dest, err := s.ArtifactPath(sha)
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected directory flush failure")
		origDir := syncDir
		syncDir = func(string) error { return injected }
		t.Cleanup(func() { syncDir = origDir })

		got, created, err := s.Promote(temp, sha)
		if !errors.Is(err, injected) || !strings.Contains(err.Error(), "syncing artifact directory") {
			t.Fatalf("promote error = %v, want the wrapped directory flush failure", err)
		}
		if created || got != "" {
			t.Fatalf("promote reported path %q created=%v after a failed flush", got, created)
		}
		if !exists(t, dest) {
			t.Fatal("published destination was removed; an immutable artifact must never be mutated")
		}
		if !hardLinks {
			return // the rename moved the only quarantine name into the store
		}
		if !exists(t, temp) {
			t.Fatal("quarantine file was cleaned up although the directory flush failed")
		}

		// A retry after the flush recovers converges on the existing destination
		// and then discards the quarantine name.
		syncDir = origDir
		path, created, err := s.Promote(temp, sha)
		if err != nil || created || path != dest {
			t.Fatalf("retry = %q created=%v, %v; want existing destination %q", path, created, err, dest)
		}
		if exists(t, temp) {
			t.Fatal("quarantine file survived the successful retry")
		}
		if err := s.Verify(sha); err != nil {
			t.Fatalf("verify after retry: %v", err)
		}
	})
}

// Reusing a destination another publisher created is also a claim that the
// artifact is durably published: that publisher may not have flushed yet, and
// the caller commits its database row on this call's success.
func TestPromoteFlushesDirectoryWhenReusingExistingDestination(t *testing.T) {
	s, temp, sha := stagedFixture(t)
	if _, created, err := s.Promote(temp, sha); err != nil || !created {
		t.Fatalf("first promote = created %v, %v", created, err)
	}
	duplicate := filepath.Join(filepath.Dir(temp), "duplicate.tmp")
	if err := os.WriteFile(duplicate, stagedFixtureBody, 0o600); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected directory flush failure")
	var flushed []string
	origDir := syncDir
	syncDir = func(dir string) error {
		flushed = append(flushed, filepath.Base(dir))
		return injected
	}
	t.Cleanup(func() { syncDir = origDir })

	if path, created, err := s.Promote(duplicate, sha); !errors.Is(err, injected) || created || path != "" {
		t.Fatalf("reuse promote = %q created=%v, %v; want the directory flush failure", path, created, err)
	}
	if len(flushed) != 1 || flushed[0] != artifactsDir {
		t.Fatalf("directory flushes = %v, want one of %q", flushed, artifactsDir)
	}
	if !exists(t, duplicate) {
		t.Fatal("duplicate quarantine file was discarded although the flush failed")
	}
}
