// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func makeTestConfigPublic(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnixWorkerExecutableRequiresExecutePermission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "papio")
	if err := os.WriteFile(path, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkerExecutable(context.Background(), path); err == nil {
		t.Fatal("worker without execute permission passed")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkerExecutable(context.Background(), path); err != nil {
		t.Fatalf("Unix executable check changed: %v", err)
	}
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if err := checkWorkerExecutable(context.Background(), path); err == nil {
			t.Fatalf("non-executable %q passed", path)
		}
	}
}

// An execute bit that does not apply to the current user is not runnable:
// the owner class decides for the owner, so a file of ours with execute only
// for group and other fails execve with EACCES. The same holds for a
// root-owned 0700 worker seen by an ordinary user.
func TestUnixWorkerExecutableRequiresExecuteForTheCurrentUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may execute any file with an execute bit")
	}
	path := filepath.Join(t.TempDir(), "papio")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o611); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(path).Run(); err == nil {
		t.Fatal("fixture is executable by its owner; the test proves nothing")
	}
	if err := checkWorkerExecutable(context.Background(), path); err == nil {
		t.Fatal("worker the current user cannot execute passed")
	}
}
