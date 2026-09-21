// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package incident

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeyRestrictsUnixPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyName)
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := LoadOrCreateKey(dir); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("existing=%v mode=%o, want 600", existing, info.Mode().Perm())
		}
	}
}
