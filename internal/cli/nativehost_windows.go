// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"

	"papio/internal/config"
)

// defaultManifestDir is the papio-owned directory holding the Chrome native-
// messaging manifest on Windows (the primary Chromium location, used by doctor).
// Browsers there locate the host through a per-user registry key (written by
// registerManifest) that points at this file, so the manifest itself lives under
// the papio config directory.
func defaultManifestDir() (string, error) {
	return filepath.Join(config.Dir(), "manifests", "chrome"), nil
}

// defaultFirefoxManifestDir is the papio-owned directory holding the Firefox
// native-messaging manifest on Windows.
func defaultFirefoxManifestDir() (string, error) {
	return filepath.Join(config.Dir(), "manifests", "firefox"), nil
}

// winBrowsers are the Chromium-based browsers on Windows. Each reads its own
// per-user registry key and is detected by its User Data directory under
// %LOCALAPPDATA%. Opera is omitted: on Windows it reads Chrome's registry key,
// so registering Chrome already covers it.
var winBrowsers = []struct{ id, label, userData string }{
	{"chrome", "Google Chrome", `Google\Chrome\User Data`},
	{"edge", "Microsoft Edge", `Microsoft\Edge\User Data`},
	{"brave", "Brave", `BraveSoftware\Brave-Browser\User Data`},
	{"vivaldi", "Vivaldi", `Vivaldi\User Data`},
	{"chromium", "Chromium", `Chromium\User Data`},
}

// browserTargets lists every browser papio can register the native host with.
// Chrome and Firefox are always present (the baseline); Chromium forks are
// included only when their User Data directory exists.
func browserTargets() ([]browserTarget, error) {
	base := config.Dir()
	local := os.Getenv("LOCALAPPDATA")
	var targets []browserTarget
	for _, b := range winBrowsers {
		present := b.id == "chrome" || (local != "" && dirExists(filepath.Join(local, b.userData)))
		targets = append(targets, browserTarget{
			id:      b.id,
			label:   b.label,
			family:  familyChromium,
			dir:     filepath.Join(base, "manifests", b.id),
			present: present,
		})
	}
	targets = append(targets, browserTarget{
		id:      "firefox",
		label:   "Firefox",
		family:  familyGecko,
		dir:     filepath.Join(base, "manifests", "firefox"),
		present: true,
	})
	return targets, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// registerManifest points the browser's per-user native-messaging registry key
// at manifestPath. This is how Chromium browsers and Firefox locate a native
// host on Windows; the manifest file's own location is otherwise arbitrary.
func registerManifest(t browserTarget, manifestPath string) error {
	keyPath := manifestRegistryPath(t.id)
	if keyPath == "" {
		return nil
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create native-host registry key: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue("", manifestPath); err != nil {
		return fmt.Errorf("set native-host registry value: %w", err)
	}
	return nil
}

// deregisterManifest removes the browser's native-messaging registry key,
// treating an already-absent key as success.
func deregisterManifest(t browserTarget) error {
	keyPath := manifestRegistryPath(t.id)
	if keyPath == "" {
		return nil
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, keyPath); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete native-host registry key: %w", err)
	}
	return nil
}

// manifestRegistryPath maps a browser id to its HKCU native-messaging key.
func manifestRegistryPath(id string) string {
	vendor := map[string]string{
		"chrome":   `Google\Chrome`,
		"edge":     `Microsoft\Edge`,
		"brave":    `BraveSoftware\Brave-Browser`,
		"vivaldi":  `Vivaldi`,
		"chromium": `Chromium`,
		"firefox":  `Mozilla`,
	}[id]
	if vendor == "" {
		return ""
	}
	return `Software\` + vendor + `\NativeMessagingHosts\` + nativeHostManifestName
}
