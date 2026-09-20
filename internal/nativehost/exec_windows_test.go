// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package nativehost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func windowsExecDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAPIO_CONFIG_DIR", dir)
	if ExecPath() != filepath.Join(dir, "bin", "papio-native-host.exe") {
		t.Fatalf("ExecPath() = %q, want fixed host path inside %q", ExecPath(), dir)
	}
	return filepath.Join(dir, "bin")
}

func windowsExecSource(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "papio.exe")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertWindowsExecBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("ReadFile(%q) = %q, %v; want %q", path, got, err, want)
	}
}

func assertWindowsExecClean(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(ExecPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ExecName && entry.Name() != filepath.Base(targetPath()) {
			t.Errorf("unexpected installer artifact: %s", entry.Name())
		}
	}
}

func TestWindowsExecutableInstallAndReplace(t *testing.T) {
	windowsExecDir(t)
	for _, contents := range []string{"old image", "new image"} {
		source := windowsExecSource(t, contents)
		path, err := InstallExecutable(source)
		if err != nil || path != ExecPath() {
			t.Fatalf("InstallExecutable() = %q, %v", path, err)
		}
		assertWindowsExecBytes(t, path, contents)
		assertWindowsExecBytes(t, source, contents)
		assertWindowsExecBytes(t, targetPath(), source)
		if target, ok := ExecTarget(); target != source || !ok {
			t.Fatalf("ExecTarget() = %q, %v; want %q, true", target, ok, source)
		}
		if target, err := resolveDaemonExecutable(); target != source || err != nil {
			t.Fatalf("resolveDaemonExecutable() = %q, %v; want %q", target, err, source)
		}
		assertWindowsExecClean(t)
	}
}

func TestWindowsExecutableInstallErrorsPreserveExisting(t *testing.T) {
	for _, invalid := range []string{"missing source", "directory source", "directory destination"} {
		t.Run(invalid, func(t *testing.T) {
			windowsExecDir(t)
			old := windowsExecSource(t, "old image")
			if _, err := InstallExecutable(old); err != nil {
				t.Fatal(err)
			}
			source := windowsExecSource(t, "new image")
			switch invalid {
			case "missing source":
				source += ".missing"
			case "directory source":
				source = t.TempDir()
			case "directory destination":
				if err := os.Remove(ExecPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(ExecPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if path, err := InstallExecutable(source); path != "" || err == nil {
				t.Fatalf("InstallExecutable() = %q, %v; want failure", path, err)
			}
			assertWindowsExecBytes(t, targetPath(), old)
			if invalid != "directory destination" {
				assertWindowsExecBytes(t, ExecPath(), "old image")
			} else if info, err := os.Stat(ExecPath()); err != nil || !info.IsDir() {
				t.Fatalf("nonregular destination was disturbed: %v, %v", info, err)
			}
			assertWindowsExecClean(t)
		})
	}
}

// Deny deletion/rename while allowing reads, independently of POSIX mode bits.
func denyWindowsExecRename(t *testing.T, path string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	closeHandle := func() {
		if !closed {
			closed = true
			if err := windows.CloseHandle(handle); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(closeHandle)
	return closeHandle
}

func TestWindowsExecutableRollsBackTargetPublicationFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			bin := windowsExecDir(t)
			old := windowsExecSource(t, "old image")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if existing {
				if _, err := InstallExecutable(old); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(targetPath(), []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			closeHandle := denyWindowsExecRename(t, targetPath())
			path, err := InstallExecutable(windowsExecSource(t, "new image"))
			var renameErr *os.LinkError
			if path != "" || !errors.As(err, &renameErr) || renameErr.New != targetPath() {
				t.Fatalf("InstallExecutable() = %q, %v; want original target publication rename error", path, err)
			}
			// Replacing an open destination can report either Windows lock
			// error; require it on the target rename itself, not on rollback.
			if !errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(renameErr, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("target publication error = %v; want a Windows locked-replacement error", renameErr)
			}
			assertWindowsExecBytes(t, targetPath(), old)
			if existing {
				assertWindowsExecBytes(t, ExecPath(), "old image")
			} else if _, err := os.Stat(ExecPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed first install left a host: %v", err)
			}
			assertWindowsExecClean(t)
			closeHandle()
			if _, err := InstallExecutable(old); err != nil {
				t.Fatalf("retry after releasing target: %v", err)
			}
		})
	}
}

func TestWindowsExecutableRollsBackImagePublicationFailure(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, ExecName)
	staged := filepath.Join(dir, "staged.exe")
	for path, data := range map[string]string{installed: "old image", staged: "new image"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	denyWindowsExecRename(t, staged)
	backup, err := publishExecutable(staged, installed)
	if backup != "" || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("publishExecutable() = %q, %v; want original sharing error", backup, err)
	}
	assertWindowsExecBytes(t, installed, "old image")
	assertWindowsExecBytes(t, staged, "new image")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("rollback left artifacts: %v, %v", entries, err)
	}
}

func TestWindowsExecutableCleanupPreservesSourceAndUnrelatedPaths(t *testing.T) {
	bin := windowsExecDir(t)
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// Even a deliberately selected old backup can be the source/daemon target.
	source := filepath.Join(bin, executableBackupPrefix+"123.exe")
	foreign := filepath.Join(bin, "other-backup.exe")
	dir := filepath.Join(bin, executableBackupPrefix+"456.exe")
	for _, path := range []string{source, foreign} {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallExecutable(source); err != nil {
		t.Fatal(err)
	}
	assertWindowsExecBytes(t, source, "keep")
	assertWindowsExecBytes(t, foreign, "keep")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("cleanup disturbed directory: %v, %v", info, err)
	}
}

const windowsExecImageMarker = "\npapio-native-host-test-image:"

func windowsExecImage(t *testing.T, label string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	path := filepath.Join(t.TempDir(), "papio.exe")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(out, windowsExecImageMarker+label+"\n"); err != nil {
		t.Fatal(err)
	}
	return path
}

// The helper exits without running deferred unlocks, proving that the kernel
// releases the install lock on process exit. Image helpers stay alive on stdin
// until the test finishes upgrading their installed path.
func TestWindowsExecutableHelper(t *testing.T) {
	mode := os.Getenv("PAPIO_EXEC_HELPER_MODE")
	if mode == "" {
		return
	}
	label := "locked"
	if mode == "lock" {
		if _, err := lockExecutableInstall(ExecPath()); err != nil {
			t.Fatal(err)
		}
	} else {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(-64, io.SeekEnd); err != nil {
			t.Fatal(err)
		}
		tail, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		index := strings.LastIndex(string(tail), windowsExecImageMarker)
		if index < 0 {
			t.Fatal("installed image has no test marker")
		}
		label = strings.TrimSpace(string(tail)[index+len(windowsExecImageMarker):])
	}
	fmt.Println(label)
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	fmt.Println("alive:" + label)
	os.Exit(0)
}

func startWindowsExecHelper(t *testing.T, image, mode, want string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, image, "-test.run=^TestWindowsExecutableHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "PAPIO_EXEC_HELPER_MODE="+mode)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
	})
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("helper ready = %q, %v; want %q", line, err, want)
	}
	return func() {
		t.Helper()
		if _, err := io.WriteString(stdin, "exit\n"); err != nil {
			t.Fatal(err)
		}
		_ = stdin.Close()
		if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "alive:"+want {
			t.Fatalf("helper after upgrade = %q, %v; want alive:%s", line, err, want)
		}
		err := cmd.Wait()
		waited = true
		if err != nil {
			t.Fatalf("helper exit: %v", err)
		}
	}
}

func TestWindowsExecutableReplacesRunningHost(t *testing.T) {
	windowsExecDir(t)
	oldImage := windowsExecImage(t, "old")
	newImage := windowsExecImage(t, "new")
	if _, err := InstallExecutable(oldImage); err != nil {
		t.Fatal(err)
	}
	finishOld := startWindowsExecHelper(t, ExecPath(), "image", "old")
	if _, err := InstallExecutable(newImage); err != nil {
		t.Fatalf("upgrade while old host runs: %v", err)
	}
	finishNew := startWindowsExecHelper(t, ExecPath(), "image", "new")
	// Both prior images remain running during another install. Reusing a
	// single backup name would try to overwrite the first running image.
	if _, err := InstallExecutable(newImage); err != nil {
		t.Fatalf("upgrade with two running prior images: %v", err)
	}
	assertWindowsExecBytes(t, targetPath(), newImage)
	finishNew()
	finishOld()
	if _, err := InstallExecutable(newImage); err != nil {
		t.Fatalf("cleanup after prior hosts exit: %v", err)
	}
	assertWindowsExecClean(t)
}

func TestWindowsExecutableInstallLockAcrossProcesses(t *testing.T) {
	windowsExecDir(t)
	old := windowsExecSource(t, "old image")
	if _, err := InstallExecutable(old); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	finish := startWindowsExecHelper(t, self, "lock", "locked")
	newImage := windowsExecSource(t, "new image")
	path, err := InstallExecutable(newImage)
	if path != "" || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("contending InstallExecutable() = %q, %v; want sharing error", path, err)
	}
	assertWindowsExecBytes(t, ExecPath(), "old image")
	assertWindowsExecBytes(t, targetPath(), old)
	finish()
	if _, err := InstallExecutable(newImage); err != nil {
		t.Fatalf("install after lock-owning process exited: %v", err)
	}
	assertWindowsExecClean(t)
}
