// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package nativehost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"papio/internal/config"
)

// ExecName is the fixed basename browsers launch to reach the native host.
// Windows requires the launched file to carry the .exe extension.
const ExecName = "papio-native-host.exe"

// ExecPath is the installed host executable a browser manifest points at.
func ExecPath() string {
	return filepath.Join(config.Dir(), "bin", ExecName)
}

// targetPath records the real papio.exe the host copy was installed from, so the
// daemon can be autostarted from a distinct binary. Windows has no unprivileged
// symlinks, so the host is a copy and cannot resolve its own origin.
func targetPath() string {
	return filepath.Join(config.Dir(), "bin", "papio-native-host.target")
}

// InstallExecutable copies realExe to the fixed-name host executable and records
// realExe as the daemon target, returning the path browsers should launch.
// Windows lacks unprivileged symlinks, so a copy is the portable choice; it is
// refreshed on every install, so upgrading papio means re-running native-host
// install (already required to redeploy the daemon binary).
func InstallExecutable(realExe string) (string, error) {
	path := ExecPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := copyExecutable(realExe, path); err != nil {
		return "", err
	}
	if err := os.WriteFile(targetPath(), []byte(realExe), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// copyExecutable copies src to dst via a same-dir temp file and rename, so a
// browser never launches a half-written host.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".papio-native-host-*.exe")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// RemoveExecutable deletes the host executable and its recorded target.
func RemoveExecutable() ([]string, error) {
	var removed []string
	for _, path := range []string{ExecPath(), targetPath()} {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// ExecTarget reports the recorded real executable and whether both it and the
// host copy currently exist.
func ExecTarget() (string, bool) {
	data, err := os.ReadFile(targetPath())
	if err != nil {
		return "", false
	}
	target := strings.TrimSpace(string(data))
	if _, err := os.Stat(ExecPath()); err != nil {
		return target, false
	}
	if _, err := os.Stat(target); err != nil {
		return target, false
	}
	return target, true
}

// resolveDaemonExecutable returns the real papio binary recorded at install
// time so the autostarted daemon is a distinct binary from the host copy.
func resolveDaemonExecutable() (string, error) {
	data, err := os.ReadFile(targetPath())
	if err != nil {
		return "", fmt.Errorf("read native-host daemon target (run papio native-host install): %w", err)
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return "", fmt.Errorf("native-host daemon target is empty (run papio native-host install)")
	}
	return resolveExecutablePath(target)
}
