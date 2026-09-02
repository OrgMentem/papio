// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package nativehost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execTestDir points config.Dir() at a fresh temp directory and returns it. It
// refuses to run unless ExecPath() lands inside that directory, so a broken
// override can never make these tests unlink the developer's real host.
func execTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAPIO_CONFIG_DIR", dir)
	path := ExecPath()
	if !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		t.Fatalf("ExecPath() = %q, want it inside temp config dir %q", path, dir)
	}
	return dir
}

// fakeExe writes an executable file standing in for the papio binary.
func fakeExe(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake exe: %v", err)
	}
	return path
}

func TestExecPathIsConfigBinFixedName(t *testing.T) {
	dir := execTestDir(t)
	want := filepath.Join(dir, "bin", ExecName)
	if got := ExecPath(); got != want {
		t.Fatalf("ExecPath() = %q, want %q", got, want)
	}
	if ExecName != "papio-native-host" {
		t.Fatalf("ExecName = %q; browser manifests pin papio-native-host", ExecName)
	}
}

func TestExecInstallCreatesParentAndSymlink(t *testing.T) {
	dir := execTestDir(t)
	target := fakeExe(t, "papio")

	binDir := filepath.Join(dir, "bin")
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("stat %q before install: err = %v, want not-exist", binDir, err)
	}

	path, err := InstallExecutable(target)
	if err != nil {
		t.Fatalf("InstallExecutable: %v", err)
	}
	if path != ExecPath() {
		t.Fatalf("InstallExecutable returned %q, want %q", path, ExecPath())
	}

	info, err := os.Stat(binDir)
	if err != nil {
		t.Fatalf("stat parent dir after install: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory after install", binDir)
	}

	link, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat installed host: %v", err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("installed host mode = %v, want a symlink (a copy goes stale on upgrade)", link.Mode())
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink installed host: %v", err)
	}
	if got != target {
		t.Fatalf("symlink target = %q, want %q", got, target)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("installed host does not resolve: %v", err)
	}
}

func TestExecInstallOverwritesExistingSymlink(t *testing.T) {
	execTestDir(t)
	oldTarget := fakeExe(t, "papio-old")
	newTarget := fakeExe(t, "papio-new")

	if _, err := InstallExecutable(oldTarget); err != nil {
		t.Fatalf("first InstallExecutable: %v", err)
	}
	path, err := InstallExecutable(newTarget)
	if err != nil {
		t.Fatalf("second InstallExecutable: %v", err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink after reinstall: %v", err)
	}
	if got != newTarget {
		t.Fatalf("symlink target after reinstall = %q, want %q", got, newTarget)
	}
	if _, err := os.Stat(oldTarget); err != nil {
		t.Fatalf("reinstall disturbed the previous target %q: %v", oldTarget, err)
	}
}

// TestExecInstallOverwritesDanglingSymlink covers the recorded incident: a host
// symlink left pointing at a vanished versioned brew path must be replaced, not
// treated as already installed.
func TestExecInstallOverwritesDanglingSymlink(t *testing.T) {
	execTestDir(t)
	gone := fakeExe(t, "papio-brew-1.2.3")
	if _, err := InstallExecutable(gone); err != nil {
		t.Fatalf("InstallExecutable: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove target: %v", err)
	}

	live := fakeExe(t, "papio")
	path, err := InstallExecutable(live)
	if err != nil {
		t.Fatalf("InstallExecutable over dangling link: %v", err)
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink after repair: %v", err)
	}
	if got != live {
		t.Fatalf("symlink target after repair = %q, want %q", got, live)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("repaired host still does not resolve: %v", err)
	}
}

func TestExecTargetReportsTargetAndExistence(t *testing.T) {
	execTestDir(t)

	if target, ok := ExecTarget(); target != "" || ok {
		t.Fatalf("ExecTarget() with nothing installed = (%q, %v), want (\"\", false)", target, ok)
	}

	exe := fakeExe(t, "papio")
	if _, err := InstallExecutable(exe); err != nil {
		t.Fatalf("InstallExecutable: %v", err)
	}
	target, ok := ExecTarget()
	if target != exe || !ok {
		t.Fatalf("ExecTarget() after install = (%q, %v), want (%q, true)", target, ok, exe)
	}

	// Dangling link: the target path is still reportable so diagnostics can name
	// the vanished binary, but it must not claim the host resolves.
	if err := os.Remove(exe); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	target, ok = ExecTarget()
	if target != exe {
		t.Fatalf("ExecTarget() target for dangling link = %q, want %q", target, exe)
	}
	if ok {
		t.Fatal("ExecTarget() reported ok for a dangling symlink; a dead host breaks the daemon session")
	}
}

// TestExecTargetRejectsRegularFile guards the copy regression: a plain file at
// ExecPath is not a tracked host, so ExecTarget must not report one.
func TestExecTargetRejectsRegularFile(t *testing.T) {
	execTestDir(t)
	path := ExecPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale copy"), 0o755); err != nil {
		t.Fatalf("write stale copy: %v", err)
	}
	if target, ok := ExecTarget(); target != "" || ok {
		t.Fatalf("ExecTarget() for a regular file = (%q, %v), want (\"\", false)", target, ok)
	}
}

// TestExecInstallReportsUnusableParent checks install fails loudly when
// config/bin cannot be created, instead of returning a path no browser can
// launch.
func TestExecInstallReportsUnusableParent(t *testing.T) {
	dir := execTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	path, err := InstallExecutable(fakeExe(t, "papio"))
	if err == nil {
		t.Fatalf("InstallExecutable succeeded with an unusable parent, returned %q", path)
	}
	if path != "" {
		t.Fatalf("InstallExecutable returned path %q alongside error %v, want empty", path, err)
	}
}

// TestExecInstallReportsUndeletableHost checks install surfaces a stale entry it
// cannot clear, rather than reporting an install that never happened.
func TestExecInstallReportsUndeletableHost(t *testing.T) {
	execTestDir(t)
	blocked := ExecPath()
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o755); err != nil {
		t.Fatalf("mkdir blocking dir: %v", err)
	}

	path, err := InstallExecutable(fakeExe(t, "papio"))
	if err == nil {
		t.Fatalf("InstallExecutable succeeded over an undeletable entry, returned %q", path)
	}
	if path != "" {
		t.Fatalf("InstallExecutable returned path %q alongside error %v, want empty", path, err)
	}

	removed, err := RemoveExecutable()
	if err == nil {
		t.Fatalf("RemoveExecutable succeeded over an undeletable entry, returned %v", removed)
	}
	if len(removed) != 0 {
		t.Fatalf("RemoveExecutable() = %v alongside error %v, want no paths", removed, err)
	}
}

func TestExecRemoveExecutable(t *testing.T) {
	execTestDir(t)
	exe := fakeExe(t, "papio")
	path, err := InstallExecutable(exe)
	if err != nil {
		t.Fatalf("InstallExecutable: %v", err)
	}

	removed, err := RemoveExecutable()
	if err != nil {
		t.Fatalf("RemoveExecutable: %v", err)
	}
	if len(removed) != 1 || removed[0] != path {
		t.Fatalf("RemoveExecutable() = %v, want [%q]", removed, path)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("lstat after remove: err = %v, want not-exist", err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("RemoveExecutable deleted the real binary %q: %v", exe, err)
	}

	removed, err = RemoveExecutable()
	if err != nil {
		t.Fatalf("second RemoveExecutable: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("second RemoveExecutable() = %v, want no paths", removed)
	}
}

// TestExecRemoveExecutableClearsDanglingLink makes sure uninstall can clean up
// the dangling-symlink state that Stat cannot see.
func TestExecRemoveExecutableClearsDanglingLink(t *testing.T) {
	execTestDir(t)
	exe := fakeExe(t, "papio")
	path, err := InstallExecutable(exe)
	if err != nil {
		t.Fatalf("InstallExecutable: %v", err)
	}
	if err := os.Remove(exe); err != nil {
		t.Fatalf("remove target: %v", err)
	}

	removed, err := RemoveExecutable()
	if err != nil {
		t.Fatalf("RemoveExecutable on dangling link: %v", err)
	}
	if len(removed) != 1 || removed[0] != path {
		t.Fatalf("RemoveExecutable() = %v, want [%q]", removed, path)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("dangling link survived removal: err = %v", err)
	}
}
