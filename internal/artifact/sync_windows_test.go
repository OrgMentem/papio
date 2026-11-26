// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

// The flush of a read-only file has to lift the attribute, and must put it back
// on the way out: after an earlier Promote linked the artifact, the file is the
// published artifact, and the attribute is shared by every name.
func TestSyncRegularFileRestoresReadOnlyAttribute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sealed.pdf")
	if err := os.WriteFile(path, stagedFixtureBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := syncRegularFile(path); err != nil {
		t.Fatalf("flushing a read-only file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != wantArtifactPerm {
		t.Fatalf("mode after flush = %v, want the read-only %v restored", info.Mode().Perm(), wantArtifactPerm)
	}
}

// A writable quarantine file needs no lift, so the flush must not seal it: the
// read-only mode is Promote's to apply, after the flush.
func TestSyncRegularFileLeavesWritableFileWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quarantine.tmp")
	if err := os.WriteFile(path, stagedFixtureBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncRegularFile(path); err != nil {
		t.Fatalf("flushing a writable file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("mode after flush = %v, want the file still writable", info.Mode().Perm())
	}
}

func TestSyncRegularFileMissingFileFails(t *testing.T) {
	if err := syncRegularFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("flushing a missing file succeeded")
	}
}
