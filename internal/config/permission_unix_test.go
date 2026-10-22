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

// A user-supplied --config path often sits in $HOME, the working directory or a
// shared directory. Saving must not strip that directory's mode.
func TestSaveLeavesPreexistingForeignDirectoryMode(t *testing.T) {
	t.Setenv("PAPIO_CONFIG_DIR", filepath.Join(t.TempDir(), "papio-default"))
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shared, "papio.toml")
	if err := Save(credentialConfig(), path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("foreign directory mode = %04o, want 0755", got)
	}
	file, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Mode().Perm(); got != 0o600 {
		t.Fatalf("config file mode = %04o, want 0600", got)
	}
}

// papio's own default config directory is still hardened to 0700.
func TestSaveTightensPreexistingDefaultConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "papio-default")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPIO_CONFIG_DIR", dir)
	if err := Save(credentialConfig(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("default config dir mode = %04o, want 0700", got)
	}
}
