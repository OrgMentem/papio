// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows/registry"

	"papio/internal/config"
)

// defaultManifestDir is the papio-owned directory holding the Chrome native-
// messaging manifest on Windows. Browsers there locate the host through a
// per-user registry key (written by registerManifest) that points at this file,
// so the manifest itself lives under the papio config directory.
func defaultManifestDir() (string, error) {
	return filepath.Join(config.Dir(), "manifests", "chrome"), nil
}

// defaultFirefoxManifestDir is the papio-owned directory holding the Firefox
// native-messaging manifest on Windows.
func defaultFirefoxManifestDir() (string, error) {
	return filepath.Join(config.Dir(), "manifests", "firefox"), nil
}

// registerManifest points the browser's per-user native-messaging registry key
// at manifestPath. This is how Chromium and Firefox locate a native host on
// Windows; the manifest file's own location is otherwise arbitrary.
func registerManifest(browser browserKind, manifestPath string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, manifestRegistryPath(browser), registry.SET_VALUE)
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
func deregisterManifest(browser browserKind) error {
	if err := registry.DeleteKey(registry.CURRENT_USER, manifestRegistryPath(browser)); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete native-host registry key: %w", err)
	}
	return nil
}

func manifestRegistryPath(browser browserKind) string {
	switch browser {
	case browserFirefox:
		return `Software\Mozilla\NativeMessagingHosts\` + nativeHostManifestName
	default:
		return `Software\Google\Chrome\NativeMessagingHosts\` + nativeHostManifestName
	}
}
