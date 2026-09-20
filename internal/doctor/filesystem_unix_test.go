// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package doctor

import (
	"context"
	"os"
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
