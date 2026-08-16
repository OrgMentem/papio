//go:build !windows

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isProcessGone asks the kernel, never the *exec.Cmd. Both the ready path
// (autostart.go:139) and terminateOrphan reap the child in their OWN goroutine,
// so cmd.ProcessState is written concurrently and a test that reads it is
// racing the reaper for an answer signal 0 already gives correctly.
func isProcessGone(t *testing.T, cmd *exec.Cmd) bool {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return true
	}
	// This file is //go:build !windows, so signal 0 is always available here.
	err := syscall.Kill(cmd.Process.Pid, 0)
	if err != nil && errors.Is(err, syscall.ESRCH) {
		return true
	}
	// Either the signal landed (still alive) or it failed for another reason,
	// which is not evidence the process is gone.
	return false
}

func TestAutostarterTerminatesOrphanOnReadinessTimeout(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "papio.sock")
	var launched *exec.Cmd
	starter := &Autostarter{
		SocketPath:    socket,
		StartTimeout:  120 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Command: func(name string, args ...string) *exec.Cmd {
			// Sleep is the long-lived helper that never creates the socket.
			cmd := exec.Command("sleep", "30")
			launched = cmd
			return cmd
		},
		OpenNull: func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) },
		OpenLog: func() (*os.File, error) {
			f, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Ready: func(context.Context, string) error { return errors.New("not ready") },
	}
	start := time.Now()
	result, err := starter.EnsureWithResult(context.Background())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "wait for daemon socket") {
		t.Fatalf("EnsureWithResult err = %v, want wait for daemon socket", err)
	}
	if !result.Started {
		t.Fatal("EnsureWithResult Started = false, want true (process was launched before readiness failed)")
	}
	if launched == nil || launched.Process == nil {
		t.Fatal("no process was launched")
	}
	// The helper must be terminated AND reaped, not merely signalled.
	// terminateOrphan does SIGTERM, waits 2s, escalates to SIGKILL and waits 3s.
	// If it only sent SIGTERM without waiting, ProcessState would still be nil.
	if !isProcessGone(t, launched) {
		// Clean up for test hygiene if our fix regresses.
		_ = launched.Process.Kill()
		t.Fatalf("orphan process pid %d is still running after readiness timeout (elapsed %v); expected terminated and reaped", launched.Process.Pid, elapsed)
	}
	if launched.ProcessState == nil {
		t.Fatalf("orphan ProcessState = %v, want exited after reap", launched.ProcessState)
	}
}

func TestAutostarterEscalatesToHardKillWhenGracefulIgnored(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "papio.sock")
	var launched *exec.Cmd
	// sh with trap "" TERM makes the exec'd sleep inherit ignored SIGTERM,
	// mirroring the macOS TCC hang where the daemon ignores SIGTERM mid-open.
	starter := &Autostarter{
		SocketPath:    socket,
		StartTimeout:  80 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Command: func(name string, args ...string) *exec.Cmd {
			cmd := exec.Command("sh", "-c", `trap "" TERM; exec sleep 30`)
			launched = cmd
			return cmd
		},
		OpenNull: func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) },
		OpenLog: func() (*os.File, error) {
			f, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Ready: func(context.Context, string) error { return errors.New("not ready") },
	}
	starter.gracePeriod = 15 * time.Millisecond
	start := time.Now()
	_, err := starter.EnsureWithResult(context.Background())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "wait for daemon socket") {
		t.Fatalf("EnsureWithResult err = %v, want wait for daemon socket", err)
	}
	if launched == nil || launched.Process == nil {
		t.Fatal("no process was launched")
	}
	// Must be gone despite ignoring SIGTERM — proves escalation to SIGKILL
	// happened within a bounded time (2s graceful wait + 3s hard wait = ~5s max).
	if elapsed > 7*time.Second {
		t.Fatalf("EnsureWithResult took %v; escalation must be bounded (graceful 2s + hard kill)", elapsed)
	}
	if !isProcessGone(t, launched) {
		_ = launched.Process.Kill()
		_ = syscall.Kill(-launched.Process.Pid, syscall.SIGKILL)
		t.Fatalf("process pid %d ignored SIGTERM and was not hard-killed within %v", launched.Process.Pid, elapsed)
	}
}

func TestAutostarterLeavesReadyDaemonRunning(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "papio.sock")
	var launched *exec.Cmd
	starter := &Autostarter{
		SocketPath:    socket,
		StartTimeout:  2 * time.Second,
		RetryInterval: 5 * time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Command: func(name string, args ...string) *exec.Cmd {
			cmd := exec.Command("sleep", "30")
			launched = cmd
			return cmd
		},
		OpenNull: func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) },
		OpenLog: func() (*os.File, error) {
			f, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Ready: func(context.Context, string) error {
			if launched == nil || launched.Process == nil {
				return errors.New("not ready")
			}
			return nil
		},
	}
	result, err := starter.EnsureWithResult(context.Background())
	if err != nil {
		t.Fatalf("EnsureWithResult: %v", err)
	}
	if !result.Started {
		t.Fatal("Started = false, want true")
	}
	if launched == nil || launched.Process == nil {
		t.Fatal("no process was launched")
	}
	// Successful readiness must detach and leave the daemon running — the
	// opposite of the failure path. Signal 0 succeeding is the whole proof, and
	// it is the kernel's answer rather than a field the reaper is writing.
	if err := syscall.Kill(launched.Process.Pid, 0); err != nil {
		t.Fatalf("ready daemon pid %d not running after detach: %v", launched.Process.Pid, err)
	}
	// Cleanup. The test does NOT own this process: EnsureWithResult reaps a
	// ready child in its own goroutine (autostart.go:139), and exec.Cmd.Wait
	// may be called exactly once, so waiting here raced that reaper — which is
	// what the race detector caught. Kill the group and let the reaper reap;
	// the pid disappearing is the observable completion.
	_ = syscall.Kill(-launched.Process.Pid, syscall.SIGKILL)
	_ = launched.Process.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for !isProcessGone(t, launched) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ready daemon cleanup")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
