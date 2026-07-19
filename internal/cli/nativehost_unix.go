// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// defaultManifestDir is Chrome's per-user NativeMessagingHosts directory (the
// primary Chromium manifest location, used by doctor). On Unix each browser
// discovers the manifest by scanning its own directory, so no registry step is
// needed.
func defaultManifestDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts"), nil
	case "linux":
		return filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts"), nil
	default:
		return "", fmt.Errorf("native-messaging host install is not supported on %s (register the manifest manually)", runtime.GOOS)
	}
}

// defaultFirefoxManifestDir is Firefox's per-user NativeMessagingHosts directory.
func defaultFirefoxManifestDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Mozilla", "NativeMessagingHosts"), nil
	case "linux":
		return filepath.Join(home, ".mozilla", "native-messaging-hosts"), nil
	default:
		return "", fmt.Errorf("Firefox native-messaging host install is not supported on %s (register the manifest manually)", runtime.GOOS)
	}
}

// chromiumForks are the Chromium-based browsers besides Chrome that each read
// their own NativeMessagingHosts directory under their own profile root.
var chromiumForks = []struct{ id, label, mac, linux string }{
	{"edge", "Microsoft Edge", "Microsoft Edge", "microsoft-edge"},
	{"brave", "Brave", "BraveSoftware/Brave-Browser", "BraveSoftware/Brave-Browser"},
	{"vivaldi", "Vivaldi", "Vivaldi", "vivaldi"},
	{"opera", "Opera", "com.operasoftware.Opera", "opera"},
	{"chromium", "Chromium", "Chromium", "chromium"},
}

func forkManifestDir(home string, mac, linux string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", mac, "NativeMessagingHosts")
	case "linux":
		return filepath.Join(home, ".config", linux, "NativeMessagingHosts")
	default:
		return ""
	}
}

// browserTargets lists every browser papio can register the native host with.
// Chrome and Firefox are always present (the baseline); Chromium forks are
// included only when their profile directory exists, so papio auto-targets the
// forks the user actually has.
func browserTargets() ([]browserTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	chromeDir, err := defaultManifestDir()
	if err != nil {
		return nil, err
	}
	firefoxDir, err := defaultFirefoxManifestDir()
	if err != nil {
		return nil, err
	}
	targets := []browserTarget{
		{id: "chrome", label: "Google Chrome", family: familyChromium, dir: chromeDir, present: true},
	}
	for _, f := range chromiumForks {
		mdir := forkManifestDir(home, f.mac, f.linux)
		if mdir == "" {
			continue
		}
		targets = append(targets, browserTarget{
			id:      f.id,
			label:   f.label,
			family:  familyChromium,
			dir:     mdir,
			present: dirExists(filepath.Dir(mdir)),
		})
	}
	targets = append(targets, browserTarget{id: "firefox", label: "Firefox", family: familyGecko, dir: firefoxDir, present: true})
	return targets, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// registerManifest and deregisterManifest are no-ops on Unix: browsers discover
// the host from the manifest file's location in their NativeMessagingHosts
// directory, not through a registry.
func registerManifest(browserTarget, string) error { return nil }

func deregisterManifest(browserTarget) error { return nil }
