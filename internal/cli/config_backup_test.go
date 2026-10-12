// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConfigWritesNameTheirBackup pins that a config rewrite which drops
// comments keeps the previous bytes in <path>.bak and says so, and that a
// fresh write names no backup.
func TestConfigWritesNameTheirBackup(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"set-access-mode", []string{"config", "set-access-mode", "assisted"}},
		{"init --force", []string{"config", "init", "--access-mode", "assisted", "--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := cliPrivateTestDir(t)
			isolateCLIProfile(t, home)
			path := filepath.Join(home, "config", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			old := []byte("# my precious comment\naccess_mode = \"conservative\"\n")
			if err := os.WriteFile(path, old, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			root := NewRoot(&stdout, &bytes.Buffer{})
			root.SetArgs(append([]string{"--config", path}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			bak := path + ".bak"
			got, err := os.ReadFile(bak)
			if err != nil || !bytes.Equal(got, old) {
				t.Fatalf("backup = %q, %v; want the old bytes %q", got, err, old)
			}
			if info, err := os.Stat(bak); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("backup mode = %v, %v; want 0600", info, err)
			}
			if want := "previous version saved to " + bak + " (comments are not preserved)"; !strings.Contains(stdout.String(), want) {
				t.Fatalf("output %q does not contain %q", stdout.String(), want)
			}
		})
	}

	t.Run("fresh init", func(t *testing.T) {
		home := cliPrivateTestDir(t)
		isolateCLIProfile(t, home)
		path := filepath.Join(home, "config", "config.toml")
		var stdout bytes.Buffer
		root := NewRoot(&stdout, &bytes.Buffer{})
		root.SetArgs([]string{"--config", path, "config", "init", "--access-mode", "assisted"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
			t.Fatalf("fresh write created a backup: %v", err)
		}
		if strings.Contains(stdout.String(), "previous version") {
			t.Fatalf("fresh write output %q names a backup", stdout.String())
		}
	})
}
