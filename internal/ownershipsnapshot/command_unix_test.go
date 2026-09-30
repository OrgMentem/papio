// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package ownershipsnapshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"papio/internal/ownership"
)

// A command source that leaves a background process behind must not leak it
// into the daemon: the run's output is its whole result, so nothing it started
// may outlive it. Both exit paths matter. With the descendant detached from
// stdout, Wait returns at once; with it holding stdout, Wait returns only at
// commandWaitDelay with exec.ErrWaitDelay.
func TestCommandSourceKillsBackgroundDescendantsAfterExit(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"detached from stdout", `sleep 30 >/dev/null 2>&1 & echo $! > "$1"; exit 0`, ""},
		{"holding stdout", `sleep 30 & echo $! > "$1"; exit 0`, ownership.FailureUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			argv := []string{"/bin/sh", "-c", tc.script, "sh", pidFile}
			_, failure := runCommand(context.Background(), argv, 10*time.Second, 1024)
			if failure != tc.want {
				t.Fatalf("failure = %q, want %q", failure, tc.want)
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatalf("pid file = %q", data)
			}
			if !processGone(pid, 5*time.Second) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("background sleep (pid %d) outlived the command run", pid)
			}
		})
	}
}

// processGone reports whether pid has exited within wait. A killed orphan is
// reaped by init, but a zombie awaiting that reap counts as gone.
func processGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
