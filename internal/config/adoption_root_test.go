// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolatedHome points home-derived lookups at a temp directory and neutralizes
// the XDG variables, so a developer's real ~/Downloads (or a localized
// XDG_DOWNLOAD_DIR) can never decide the outcome of these tests.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// TestEffectiveAdoptionRootDefaultsIntoTheBrowserDownloadDirectory is the
// defect this default exists to close: the daemon used to scan
// <data_dir>/adoptions while every browser steering target is the relative
// path papio/<job-id>/…, which can only ever land inside the browser's own
// download directory. The two never met and a default install adopted
// nothing.
func TestEffectiveAdoptionRootDefaultsIntoTheBrowserDownloadDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows download directory comes from the registry, not HOME")
	}
	home := isolatedHome(t)
	cfg := Config{DataDir: filepath.Join(home, ".local", "share", "papio")}

	got := cfg.EffectiveAdoptionRoot()
	want := filepath.Join(home, "Downloads", AdoptionDirName)
	if got != want {
		t.Fatalf("EffectiveAdoptionRoot() = %q, want %q", got, want)
	}
	if !BrowserSteerableAdoptionRoot(got) {
		t.Fatalf("the default root %q is not reachable by browser steering", got)
	}
	if legacy := filepath.Join(cfg.DataDir, "adoptions"); got == legacy {
		t.Fatalf("EffectiveAdoptionRoot() still resolves to the unreachable legacy root %q", legacy)
	}
}

// TestExplicitAdoptionRootStillWins pins the override the live install relies
// on: a hand-set download_adoption_root is used verbatim, default derivation
// and all.
func TestExplicitAdoptionRootStillWins(t *testing.T) {
	isolatedHome(t)
	explicit := filepath.Join(t.TempDir(), "elsewhere", AdoptionDirName)
	cfg := Config{DataDir: t.TempDir(), Browser: Browser{AdoptionRoot: explicit}}

	if got := cfg.EffectiveAdoptionRoot(); got != explicit {
		t.Fatalf("EffectiveAdoptionRoot() = %q, want the explicit %q", got, explicit)
	}
	if roots := cfg.AdoptionRoots(); roots[0] != explicit {
		t.Fatalf("AdoptionRoots()[0] = %q, want the explicit root first", roots[0])
	}
}

// TestLegacyAdoptionRootStaysInTheSearchPath is the no-orphan guarantee: an
// install that has been adopting into <data_dir>/adoptions keeps it as a
// read/drain location even though nothing is ever written there again.
func TestLegacyAdoptionRootStaysInTheSearchPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows download directory comes from the registry, not HOME")
	}
	home := isolatedHome(t)
	data := filepath.Join(home, ".local", "share", "papio")
	cfg := Config{DataDir: data}

	legacy := filepath.Join(data, "adoptions")
	if got := cfg.LegacyAdoptionRoot(); got != legacy {
		t.Fatalf("LegacyAdoptionRoot() = %q, want %q", got, legacy)
	}
	roots := cfg.AdoptionRoots()
	want := []string{filepath.Join(home, "Downloads", AdoptionDirName), legacy}
	if len(roots) != len(want) || roots[0] != want[0] || roots[1] != want[1] {
		t.Fatalf("AdoptionRoots() = %q, want %q (effective first, legacy second)", roots, want)
	}
}

// TestLegacyAdoptionRootIsSuppressedWhenItIsTheEffectiveRoot keeps the search
// path free of a duplicate that would make every sweep scan the same
// directory twice.
func TestLegacyAdoptionRootIsSuppressedWhenItIsTheEffectiveRoot(t *testing.T) {
	isolatedHome(t)
	data := t.TempDir()
	cfg := Config{DataDir: data, Browser: Browser{AdoptionRoot: filepath.Join(data, "adoptions")}}

	if got := cfg.LegacyAdoptionRoot(); got != "" {
		t.Fatalf("LegacyAdoptionRoot() = %q, want \"\" when it is already the effective root", got)
	}
	if roots := cfg.AdoptionRoots(); len(roots) != 1 {
		t.Fatalf("AdoptionRoots() = %q, want exactly one root", roots)
	}
}

func TestBrowserSteerableAdoptionRoot(t *testing.T) {
	for _, test := range []struct {
		root string
		want bool
	}{
		{filepath.Join("/home/reader", "Downloads", "papio"), true},
		{filepath.Join("/home/reader", "Downloads", "papio") + string(filepath.Separator), true},
		{filepath.Join("/var", "data", "papio"), true}, // reachable if the browser downloads to /var/data
		{filepath.Join("/home/reader", ".local", "share", "papio", "adoptions"), false},
		{filepath.Join("/home/reader", "Downloads"), false},
		{filepath.Join("/home/reader", "Downloads", "Papio"), false}, // the steering segment is exact
		{"", false},
	} {
		if got := BrowserSteerableAdoptionRoot(test.root); got != test.want {
			t.Errorf("BrowserSteerableAdoptionRoot(%q) = %v, want %v", test.root, got, test.want)
		}
	}
}

// TestUserDownloadsDirFallsBackToHomeDownloads covers the plain case on every
// non-Windows platform: no XDG override, no user-dirs.dirs.
func TestUserDownloadsDirFallsBackToHomeDownloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows download directory comes from the registry, not HOME")
	}
	home := isolatedHome(t)
	if got, want := UserDownloadsDir(), filepath.Join(home, "Downloads"); got != want {
		t.Fatalf("UserDownloadsDir() = %q, want %q", got, want)
	}
}

// TestXDGDownloadDirReadsUserDirs proves the Linux/BSD derivation is a real
// lookup and not a hardcoded English path: a localized desktop names its
// download folder in user-dirs.dirs, and creating ~/Downloads beside it would
// give papio a directory no browser ever writes to. It drives xdgDownloadDir
// directly so the contract is exercised on every host, not only on the one
// platform that routes to it.
func TestXDGDownloadDirReadsUserDirs(t *testing.T) {
	home := isolatedHome(t)
	configHome := filepath.Join(home, ".config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# generated\nXDG_DESKTOP_DIR=\"$HOME/Bureau\"\nXDG_DOWNLOAD_DIR=\"$HOME/Téléchargements\"\n"
	if err := os.WriteFile(filepath.Join(configHome, "user-dirs.dirs"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := xdgDownloadDir(), filepath.Join(home, "Téléchargements"); got != want {
		t.Fatalf("xdgDownloadDir() = %q, want %q", got, want)
	}
}

// TestXDGDownloadDirPrefersTheEnvironmentOverride pins the documented lookup
// order: the environment variable beats user-dirs.dirs.
func TestXDGDownloadDirPrefersTheEnvironmentOverride(t *testing.T) {
	home := isolatedHome(t)
	configHome := filepath.Join(home, ".config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "user-dirs.dirs"),
		[]byte("XDG_DOWNLOAD_DIR=\"$HOME/from-file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	override := t.TempDir()
	t.Setenv("XDG_DOWNLOAD_DIR", override)
	if got := xdgDownloadDir(); got != override {
		t.Fatalf("xdgDownloadDir() = %q, want the XDG_DOWNLOAD_DIR override %q", got, override)
	}
}

// TestXDGDownloadDirIgnoresARelativeEnvironmentOverride keeps a stray
// relative value from producing an adoption root relative to the daemon's
// working directory.
func TestXDGDownloadDirIgnoresARelativeEnvironmentOverride(t *testing.T) {
	isolatedHome(t)
	t.Setenv("XDG_DOWNLOAD_DIR", "Downloads")
	if got := xdgDownloadDir(); got != "" {
		t.Fatalf("xdgDownloadDir() = %q, want \"\" for a relative override", got)
	}
}

func TestXDGDownloadDirFrom(t *testing.T) {
	const home = "/home/reader"
	for _, test := range []struct {
		name, body, want string
	}{
		{"home relative", `XDG_DOWNLOAD_DIR="$HOME/Downloads"`, filepath.Join(home, "Downloads")},
		{"localized", `XDG_DOWNLOAD_DIR="$HOME/Téléchargements"`, filepath.Join(home, "Téléchargements")},
		{"absolute", `XDG_DOWNLOAD_DIR="/mnt/bulk/dl"`, filepath.FromSlash("/mnt/bulk/dl")},
		{"unquoted absolute", `XDG_DOWNLOAD_DIR=/mnt/bulk/dl`, filepath.FromSlash("/mnt/bulk/dl")},
		{"exported", `export XDG_DOWNLOAD_DIR="$HOME/dl"`, filepath.Join(home, "dl")},
		{"commented out", `#XDG_DOWNLOAD_DIR="$HOME/Downloads"`, ""},
		{"other keys only", "XDG_DESKTOP_DIR=\"$HOME/Desktop\"\nXDG_MUSIC_DIR=\"$HOME/Music\"", ""},
		{"disabled by pointing at home", `XDG_DOWNLOAD_DIR="$HOME"`, ""},
		{"last assignment wins", "XDG_DOWNLOAD_DIR=\"$HOME/one\"\nXDG_DOWNLOAD_DIR=\"$HOME/two\"", filepath.Join(home, "two")},
		{"crlf", "XDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\r\n", filepath.Join(home, "Downloads")},
		{"empty file", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := xdgDownloadDirFrom(test.body, home); got != test.want {
				t.Fatalf("xdgDownloadDirFrom(%q) = %q, want %q", test.body, got, test.want)
			}
		})
	}
}
