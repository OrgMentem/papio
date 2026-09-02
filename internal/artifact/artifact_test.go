// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestPromoteIsAtomicIdempotentAndVerifiable(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	temp := filepath.Join(q, "download.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := HashFile(temp)
	if err != nil || size != int64(len(body)) {
		t.Fatalf("hash: %v size=%d", err, size)
	}

	dest, err := s.Promote(temp, sha)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temp file survived promotion")
	}
	if err := s.Verify(sha); err != nil {
		t.Fatalf("verify: %v", err)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("artifact mode = %v, want read-only 0400", info.Mode().Perm())
	}

	// Second promotion of identical content is a no-op (crash-recovery re-fetch).
	temp2 := filepath.Join(q, "download2.tmp")
	if err := os.WriteFile(temp2, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dest2, err := s.Promote(temp2, sha)
	if err != nil || dest2 != dest {
		t.Fatalf("re-promote = %q, %v; want same path, no error", dest2, err)
	}

	// Wrong-hash promotion is refused.
	temp3 := filepath.Join(q, "download3.tmp")
	_ = os.WriteFile(temp3, []byte("different"), 0o600)
	if _, err := s.Promote(temp3, sha); err == nil {
		t.Fatal("promoted mismatched content")
	}
}

func TestPromoteConcurrentlyConvergesForSameHash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	tempPaths := []string{
		filepath.Join(q, "download1.tmp"),
		filepath.Join(q, "download2.tmp"),
	}
	for _, tempPath := range tempPaths {
		if err := os.WriteFile(tempPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sha, _, err := HashFile(tempPaths[0])
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan string, len(tempPaths))
	errs := make(chan error, len(tempPaths))
	var wg sync.WaitGroup
	for _, tempPath := range tempPaths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			<-start
			dest, err := s.Promote(path, sha)
			if err != nil {
				errs <- err
				return
			}
			results <- dest
		}(tempPath)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent promote: %v", err)
	}
	var dest string
	for result := range results {
		if dest == "" {
			dest = result
		} else if result != dest {
			t.Fatalf("promotion destinations = %q and %q, want one path", dest, result)
		}
	}
	if err := s.Verify(sha); err != nil {
		t.Fatalf("verify converged artifact: %v", err)
	}
}

func TestPromoteFallsBackWhenHardLinksUnsupported(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	temp := filepath.Join(q, "download.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}

	originalLink := linkFile
	linkFile = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EOPNOTSUPP}
	}
	t.Cleanup(func() { linkFile = originalLink })

	dest, err := s.Promote(temp, sha)
	if err != nil {
		t.Fatalf("promote without hard links: %v", err)
	}
	if err := s.Verify(sha); err != nil {
		t.Fatalf("verify fallback artifact: %v", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("fallback temp remains: %v", err)
	}

	duplicate := filepath.Join(q, "duplicate.tmp")
	if err := os.WriteFile(duplicate, body, 0o600); err != nil {
		t.Fatal(err)
	}
	duplicateDest, err := s.Promote(duplicate, sha)
	if err != nil {
		t.Fatalf("fallback duplicate promote: %v", err)
	}
	if duplicateDest != dest {
		t.Fatalf("fallback duplicate destination = %q, want %q", duplicateDest, dest)
	}
	if _, err := os.Stat(duplicate); !os.IsNotExist(err) {
		t.Fatalf("fallback duplicate temp remains: %v", err)
	}
}

func TestPromoteSucceedsWhenTempCleanupFails(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	temp := filepath.Join(q, "download.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}

	origRemove := removeFile
	removeFile = func(name string) error {
		return errors.New("injected remove failure")
	}
	t.Cleanup(func() { removeFile = origRemove })

	dest, err := s.Promote(temp, sha)
	if err != nil {
		t.Fatalf("promote with cleanup failure: %v", err)
	}
	// Artifact must be published despite cleanup failure.
	if err := s.Verify(sha); err != nil {
		t.Fatalf("verify after cleanup failure: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest after cleanup failure: %v", err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("artifact mode = %v, want 0400", info.Mode().Perm())
	}
	// Temp duplicate remains because removal was injected to fail,
	// but promotion itself succeeded — caller must not treat this as failure.
	if _, err := os.Stat(temp); err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat temp: %v", err)
	} else if os.IsNotExist(err) {
		t.Logf("temp was removed despite injected failure (hard-link accounting may have removed the name)")
	}
}

func TestPromotePrePublicationFailureLeavesNoDestination(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.QuarantineDir("job_x")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4 fixture body")
	temp := filepath.Join(q, "download.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	destPath, err := s.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}

	origChmod := chmodFile
	chmodFile = func(name string, mode os.FileMode) error {
		return errors.New("injected chmod failure")
	}
	t.Cleanup(func() { chmodFile = origChmod })

	if _, err := s.Promote(temp, sha); err == nil {
		t.Fatal("expected pre-publication chmod failure")
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination exists after pre-publication failure: %v", err)
	}
}
func TestConfineRegularFile(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a.pdf")
	_ = os.WriteFile(inside, []byte("x"), 0o600)
	if err := ConfineRegularFile(root, inside); err != nil {
		t.Fatalf("regular file inside root rejected: %v", err)
	}
	if err := ConfineRegularFile(root, filepath.Join(root, "..", "escape.pdf")); err == nil {
		t.Fatal("escaping path accepted")
	}
	link := filepath.Join(root, "link.pdf")
	if err := os.Symlink(inside, link); err == nil {
		if err := ConfineRegularFile(root, link); err == nil {
			t.Fatal("symlink accepted")
		}
	}
	if err := ConfineRegularFile(root, root); err == nil {
		t.Fatal("directory accepted")
	}
}

// CleanQuarantine runs unattended on every terminal job transition
// (internal/app/app.go, internal/job/job.go), so it is a destructive call that
// nobody reviews per invocation. Its job-id guard is path-traversal protection
// on that call, which is why the invalid cases assert what SURVIVED, not just
// that an error came back.
func TestCleanQuarantineRemovesOnlyTheNamedJobDirectory(t *testing.T) {
	data := t.TempDir()
	s, err := New(data)
	if err != nil {
		t.Fatal(err)
	}

	target, err := s.QuarantineDir("job_target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "download.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling, err := s.QuarantineDir("job_sibling")
	if err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "download.tmp")
	if err := os.WriteFile(siblingFile, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A published artifact: the traversal target one directory up from any
	// per-job quarantine directory, and immutable by contract.
	body := []byte("%PDF-1.4 published")
	temp := filepath.Join(sibling, "publish.tmp")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CleanQuarantine("job_target"); err != nil {
		t.Fatalf("clean quarantine: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("quarantine directory survived cleanup: stat err = %v", err)
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Fatalf("cleaning one job removed another job's quarantine file: %v", err)
	}

	// Idempotent: the terminal transition re-runs after crash recovery, and an
	// absent directory is the expected steady state, not an error.
	if err := s.CleanQuarantine("job_target"); err != nil {
		t.Fatalf("second cleanup of the same job: %v", err)
	}
	if err := s.CleanQuarantine("job_never_existed"); err != nil {
		t.Fatalf("cleanup of an absent job: %v", err)
	}

	// Invalid ids are refused before any removal happens. ".." is the one that
	// matters most: filepath.Join collapses it to the data directory itself, so
	// accepting it would turn one cleanup into os.RemoveAll(dataDir).
	for _, bad := range []string{"", ".", "..", "..\\artifacts", "../artifacts", "job/../..", "job\\..\\..", "/", "\\"} {
		if err := s.CleanQuarantine(bad); err == nil {
			t.Fatalf("accepted invalid job id %q", bad)
		}
	}
	for _, keep := range []string{
		published,
		filepath.Join(data, artifactsDir),
		filepath.Join(data, quarantineDir),
		sibling,
		siblingFile,
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("an invalid job id removed %s: %v", keep, err)
		}
	}
}
