// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
)

// initHome isolates the home-derived download directory so these tests never
// touch a developer's real ~/Downloads.
func initHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// TestInitCreatesTheBrowserAdoptionRoot pins where the macOS Files-and-Folders
// consent prompt is paid. The adoption root lives inside the TCC-protected
// Downloads folder; a background daemon that opens it first does not get a
// clean EPERM, its open(2) blocks in-kernel on tccd and the process then
// ignores SIGTERM. Setup is interactive and user-launched, so it creates the
// directory once, here.
func TestInitCreatesTheBrowserAdoptionRoot(t *testing.T) {
	home := initHome(t)
	path := filepath.Join(home, ".config", "papio", "config.toml")

	out, err := runInitForTest(t, path, initTestDependencies(t),
		"--non-interactive", "--email", "reader@example.test", "--skip-browser")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	root := filepath.Join(home, "Downloads", config.AdoptionDirName)
	info, statErr := os.Stat(root)
	if statErr != nil {
		t.Fatalf("adoption root %q was not created: %v\n%s", root, statErr, out)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", root)
	}
	if !strings.Contains(out, "✓ Downloads: "+root) {
		t.Fatalf("init output does not report the created adoption root %q:\n%s", root, out)
	}

	// The default stays derived: freezing an absolute path into config.toml
	// would strand a user who later moves their download folder.
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(body), "download_adoption_root") {
		t.Fatalf("init wrote download_adoption_root into the config; the default must stay derived:\n%s", body)
	}
}

// TestInitReportsAnAdoptionRootItCannotCreate is the honest-failure path. A
// denied grant or a read-only home means adoption will not work, and setup
// must say so with the fix rather than print a tick and leave the user to
// discover silence later.
func TestInitReportsAnAdoptionRootItCannotCreate(t *testing.T) {
	home := initHome(t)
	// A regular file where Downloads/ belongs makes MkdirAll fail the same way
	// a denied grant or read-only home does, without needing either.
	if err := os.WriteFile(filepath.Join(home, "Downloads"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "papio", "config.toml")

	out, err := runInitForTest(t, path, initTestDependencies(t),
		"--non-interactive", "--email", "reader@example.test", "--skip-browser")
	if err != nil {
		t.Fatalf("a failed adoption root must not abort setup: %v\n%s", err, out)
	}

	root := filepath.Join(home, "Downloads", config.AdoptionDirName)
	if !strings.Contains(out, "✗ Downloads: ") {
		t.Fatalf("init reported no failure for an uncreatable adoption root:\n%s", out)
	}
	if !strings.Contains(out, root) {
		t.Fatalf("failure line does not name the path %q:\n%s", root, out)
	}
	if !strings.Contains(out, "cannot be adopted") {
		t.Fatalf("failure line does not say adoption is broken:\n%s", out)
	}
}

// TestInitReportsAnUnsteerableConfiguredRoot covers the operator who points
// download_adoption_root somewhere the browser can never write. Creating it
// succeeds and means nothing, so setup must not report a tick.
func TestInitReportsAnUnsteerableConfiguredRoot(t *testing.T) {
	initHome(t)
	unreachable := filepath.Join(t.TempDir(), "adoptions")
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.Browser.AdoptionRoot = unreachable

	var out strings.Builder
	writeAdoptionRootLine(&out, cfg)

	if _, err := os.Stat(unreachable); err != nil {
		t.Fatalf("the root should still be created: %v", err)
	}
	line := out.String()
	if !strings.HasPrefix(line, "✗ Downloads: ") {
		t.Fatalf("want a failure line, got %q", line)
	}
	if !strings.Contains(line, unreachable) || !strings.Contains(line, "download_adoption_root") {
		t.Fatalf("line = %q, want it to name %q and the setting", line, unreachable)
	}
}
