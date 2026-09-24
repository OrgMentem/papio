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

	"golang.org/x/sys/windows"

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
	unlock, err := lockExecutableInstall(path)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Prepare the target before touching the installed image, and publish it
	// atomically so an error cannot truncate the previous daemon target.
	target, err := os.CreateTemp(filepath.Dir(path), ".papio-native-host-target-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(target.Name()) }()
	if _, err := io.WriteString(target, realExe); err != nil {
		_ = target.Close()
		return "", err
	}
	if err := target.Close(); err != nil {
		return "", err
	}
	backup, err := copyExecutable(realExe, path)
	if err != nil {
		return "", err
	}
	// These are separate publications: a browser can launch the new image
	// before its daemon target is updated.
	if err := os.Rename(target.Name(), targetPath()); err != nil {
		return "", errors.Join(err, rollbackExecutable(path, backup))
	}
	cleanupExecutableBackups(filepath.Dir(path), realExe)
	return path, nil
}

// The exclusive handle serializes installers across processes, including target
// publication and backup cleanup. Closing it (also on process exit) removes the
// lock file; a competing installer fails promptly instead of interleaving swaps.
func lockExecutableInstall(path string) (func(), error) {
	name, err := windows.UTF16PtrFromString(path + ".install.lock")
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return nil, fmt.Errorf("locking native-host installation: %w", err)
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}

// copyExecutable stages a complete image, then publishes it at the fixed path.
// The caller holds the install lock and keeps the returned backup until the
// daemon target is also published. Running Windows images can be renamed aside
// but cannot be overwritten or necessarily deleted until their process exits.
func copyExecutable(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".papio-native-host-*.exe")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return publishExecutable(name, dst)
}

const executableBackupPrefix = ".papio-native-host-backup-"

func moveExecutableAside(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("native-host executable %q is not a regular file", path)
	}
	// Reserve a unique name instead of reusing a backup that may still be a
	// running image from an earlier upgrade.
	backup, err := os.CreateTemp(filepath.Dir(path), executableBackupPrefix+"*.exe")
	if err != nil {
		return "", err
	}
	name := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Rename(path, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func restoreExecutable(backup, path string) error {
	if backup == "" {
		return nil
	}
	if err := os.Rename(backup, path); err != nil {
		return fmt.Errorf("restoring native host (previous image retained at %q): %w", backup, err)
	}
	return nil
}

func publishExecutable(staged, path string) (string, error) {
	backup, err := moveExecutableAside(path)
	if err != nil {
		return "", err
	}
	if err := os.Rename(staged, path); err != nil {
		return "", errors.Join(err, restoreExecutable(backup, path))
	}
	return backup, nil
}

func rollbackExecutable(path, backup string) error {
	// A browser may already have launched the newly published image. Rename
	// that aside too, so rollback does not depend on deleting a running image.
	failed, err := moveExecutableAside(path)
	if err != nil {
		return fmt.Errorf("rolling back native host (previous image retained at %q): %w", backup, err)
	}
	if err := restoreExecutable(backup, path); err != nil {
		return err
	}
	if failed != "" {
		_ = os.Remove(failed)
	}
	return nil
}

func cleanupExecutableBackups(dir, source string) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), executableBackupPrefix) && strings.HasSuffix(entry.Name(), ".exe") {
			info, err := entry.Info()
			if err != nil || os.SameFile(info, sourceInfo) {
				continue
			}
			// A still-running image remains private here until a later successful
			// install can remove it. Never terminate a host to reclaim its backup.
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
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
	// #nosec G703 -- target is the path native-host install recorded in this
	// user's own config dir; the Stat only reports whether it still exists.
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
