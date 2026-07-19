// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// defaultManifestDir is Chrome's per-user NativeMessagingHosts directory. On
// Unix the browser discovers the manifest by scanning this directory, so no
// registry step is needed.
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

// registerManifest and deregisterManifest are no-ops on Unix: browsers discover
// the host from the manifest file's location in the NativeMessagingHosts
// directory, not through a registry.
func registerManifest(browserKind, string) error { return nil }

func deregisterManifest(browserKind) error { return nil }
