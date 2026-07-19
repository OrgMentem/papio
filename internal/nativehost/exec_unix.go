// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package nativehost

import (
	"errors"
	"os"
	"path/filepath"

	"papio/internal/config"
)

// ExecName is the fixed basename browsers launch to reach the native host.
const ExecName = "papio-native-host"

// ExecPath is the installed host executable a browser manifest points at.
func ExecPath() string {
	return filepath.Join(config.Dir(), "bin", ExecName)
}

// InstallExecutable points the fixed-name host executable at realExe and returns
// the path browsers should launch. On Unix this is a symlink into config/bin, so
// it always tracks the current binary without a copy to refresh.
func InstallExecutable(realExe string) (string, error) {
	path := ExecPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Symlink(realExe, path); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveExecutable deletes the installed host executable, returning the paths it
// removed (empty when nothing was present).
func RemoveExecutable() ([]string, error) {
	path := ExecPath()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return []string{path}, nil
}

// ExecTarget reports the real executable the installed host points at and
// whether it currently resolves.
func ExecTarget() (string, bool) {
	target, err := os.Readlink(ExecPath())
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(ExecPath()); err != nil {
		return target, false
	}
	return target, true
}

// resolveDaemonExecutable returns the real papio binary to autostart as the
// daemon, resolving the installed host symlink to its target.
func resolveDaemonExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolveExecutablePath(exe)
}
