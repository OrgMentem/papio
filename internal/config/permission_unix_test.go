// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func configPermissionRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func assertSavedConfigPermissions(t *testing.T, path string) {
	t.Helper()
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o600}, {filepath.Dir(path), 0o700}} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != item.mode {
			t.Fatalf("%s mode = %04o, want %04o", item.path, info.Mode().Perm(), item.mode)
		}
	}
}
