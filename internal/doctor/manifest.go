// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DefaultChromeNativeMessagingHostsDir returns Chrome's per-user native-messaging
// manifest directory.
func DefaultChromeNativeMessagingHostsDir() (string, error) {
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

// DefaultFirefoxNativeMessagingHostsDir returns Firefox's per-user
// native-messaging manifest directory.
func DefaultFirefoxNativeMessagingHostsDir() (string, error) {
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
