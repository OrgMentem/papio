// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
)

// adoptionHome isolates the home-derived download directory so a developer's
// real ~/Downloads never decides these outcomes.
func adoptionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func collectChecks(t *testing.T, run func(func(string, string, string, string))) []Check {
	t.Helper()
	var checks []Check
	run(func(name, status, detail, remediation string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	})
	return checks
}

// TestCheckAdoptionRootFailsWhenBrowserSteeringCannotReachIt is the defect
// this check exists for. A root outside the browser's papio/ steering segment
// is perfectly readable and adopts nothing, forever, with no error anywhere —
// which is exactly what the old <data_dir>/adoptions default did. Readability
// alone must not be reported as health.
func TestCheckAdoptionRootFailsWhenBrowserSteeringCannotReachIt(t *testing.T) {
	adoptionHome(t)
	data := t.TempDir()
	unreachable := filepath.Join(data, "adoptions")
	if err := os.MkdirAll(unreachable, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: data, Browser: config.Browser{AdoptionRoot: unreachable}}

	checks := collectChecks(t, func(add func(string, string, string, string)) { checkAdoptionRoot(cfg, add) })
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	c := checks[0]
	if c.Name != "adoption_root" || c.Status != Fail {
		t.Fatalf("check = %#v, want a Fail adoption_root check for an unsteerable root", c)
	}
	if !strings.Contains(c.Detail, unreachable) {
		t.Fatalf("detail = %q, want it to name the effective path %q", c.Detail, unreachable)
	}
	if !strings.Contains(c.Remediation, "download_adoption_root") ||
		!strings.Contains(c.Remediation, config.DefaultAdoptionRoot()) {
		t.Fatalf("remediation = %q, want the setting name and the default root %q",
			c.Remediation, config.DefaultAdoptionRoot())
	}
}

// TestCheckAdoptionRootPassesForTheReachableDefault is the other half: the
// derived default under the user's download directory is the shape browser
// steering can actually reach.
func TestCheckAdoptionRootPassesForTheReachableDefault(t *testing.T) {
	home := adoptionHome(t)
	root := filepath.Join(home, "Downloads", config.AdoptionDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: filepath.Join(home, ".local", "share", "papio")}

	checks := collectChecks(t, func(add func(string, string, string, string)) { checkAdoptionRoot(cfg, add) })
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	c := checks[0]
	if c.Name != "adoption_root" || c.Status != Pass {
		t.Fatalf("check = %#v, want a Pass adoption_root check for %q", c, root)
	}
	if !strings.Contains(c.Detail, root) {
		t.Fatalf("detail = %q, want it to name the effective path %q", c.Detail, root)
	}
}

// TestCheckAdoptionRootMissingDirIsHealthy keeps ENOENT a Pass: the root simply
// has not been created yet, which is the ordinary state before the first
// download, not a permissions fault or a TCC hang. This replaces the same-named
// test in doctor_test.go, which set cfg.DataDir to bound the root — a lever the
// <downloads>/papio default no longer answers to, so it silently began probing
// the developer's real ~/Downloads/papio instead of a missing directory.
func TestCheckAdoptionRootMissingDirIsHealthy(t *testing.T) {
	home := adoptionHome(t)
	root := filepath.Join(home, "Downloads", config.AdoptionDirName)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("stat %q = %v, want the directory to be absent", root, err)
	}
	cfg := config.Config{DataDir: filepath.Join(home, ".local", "share", "papio")}

	checks := collectChecks(t, func(add func(string, string, string, string)) { checkAdoptionRoot(cfg, add) })
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	if c := checks[0]; c.Name != "adoption_root" || c.Status != Pass {
		t.Fatalf("check = %#v, want a Pass adoption_root check for the absent %q", c, root)
	}
}

// TestCheckAdoptionRootWarnsOutsideThisAccountsDownloadFolder keeps the
// structurally-valid-but-unusual case honest: a papio/ directory somewhere
// else works only if the browser downloads there, so it is a Warn naming the
// condition rather than a silent Pass or a wrong Fail.
func TestCheckAdoptionRootWarnsOutsideThisAccountsDownloadFolder(t *testing.T) {
	adoptionHome(t)
	elsewhere := filepath.Join(t.TempDir(), "bulk", config.AdoptionDirName)
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), Browser: config.Browser{AdoptionRoot: elsewhere}}

	checks := collectChecks(t, func(add func(string, string, string, string)) { checkAdoptionRoot(cfg, add) })
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	if c := checks[0]; c.Status != Warn || !strings.Contains(c.Detail, elsewhere) {
		t.Fatalf("check = %#v, want a Warn naming %q", c, elsewhere)
	}
}

// TestCheckLegacyAdoptionRootNamesFilesLeftBehind covers the upgrade path the
// default change creates. The situation must be described — including that
// papio still adopts from there — rather than leaving a user to wonder why a
// directory full of PDFs stopped growing.
func TestCheckLegacyAdoptionRootNamesFilesLeftBehind(t *testing.T) {
	home := adoptionHome(t)
	data := filepath.Join(home, ".local", "share", "papio")
	legacy := filepath.Join(data, "adoptions")
	if err := os.MkdirAll(filepath.Join(legacy, "job_leftover"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: data}

	checks := collectChecks(t, func(add func(string, string, string, string)) { checkLegacyAdoptionRoot(cfg, add) })
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one legacy check", checks)
	}
	c := checks[0]
	if c.Name != "adoption_root_legacy" || c.Status != Warn {
		t.Fatalf("check = %#v, want a Warn adoption_root_legacy check", c)
	}
	if !strings.Contains(c.Detail, legacy) {
		t.Fatalf("detail = %q, want it to name %q", c.Detail, legacy)
	}
	if !strings.Contains(c.Remediation, cfg.EffectiveAdoptionRoot()) {
		t.Fatalf("remediation = %q, want it to name the new root %q", c.Remediation, cfg.EffectiveAdoptionRoot())
	}
}

// TestCheckLegacyAdoptionRootIsSilentWhenThereIsNothingToSay stops the upgrade
// notice from becoming permanent furniture on a clean install.
func TestCheckLegacyAdoptionRootIsSilentWhenThereIsNothingToSay(t *testing.T) {
	home := adoptionHome(t)
	data := filepath.Join(home, ".local", "share", "papio")

	t.Run("absent", func(t *testing.T) {
		cfg := config.Config{DataDir: data}
		if checks := collectChecks(t, func(add func(string, string, string, string)) {
			checkLegacyAdoptionRoot(cfg, add)
		}); len(checks) != 0 {
			t.Fatalf("checks = %#v, want none", checks)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(data, "adoptions"), 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{DataDir: data}
		if checks := collectChecks(t, func(add func(string, string, string, string)) {
			checkLegacyAdoptionRoot(cfg, add)
		}); len(checks) != 0 {
			t.Fatalf("checks = %#v, want none", checks)
		}
	})

	t.Run("only a stray dotfile", func(t *testing.T) {
		legacy := filepath.Join(data, "adoptions")
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			t.Fatal(err)
		}
		// A Finder .DS_Store is not a download; warning about it forever
		// would be noise nobody can clear by finishing their work.
		if err := os.WriteFile(filepath.Join(legacy, ".DS_Store"), []byte{0}, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{DataDir: data}
		if checks := collectChecks(t, func(add func(string, string, string, string)) {
			checkLegacyAdoptionRoot(cfg, add)
		}); len(checks) != 0 {
			t.Fatalf("checks = %#v, want none for a dotfile-only legacy root", checks)
		}
	})

	t.Run("legacy is the effective root", func(t *testing.T) {
		legacy := filepath.Join(data, "adoptions")
		if err := os.MkdirAll(filepath.Join(legacy, "job_leftover"), 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{DataDir: data, Browser: config.Browser{AdoptionRoot: legacy}}
		if checks := collectChecks(t, func(add func(string, string, string, string)) {
			checkLegacyAdoptionRoot(cfg, add)
		}); len(checks) != 0 {
			t.Fatalf("checks = %#v, want none when the legacy root is still the configured one", checks)
		}
	})
}
