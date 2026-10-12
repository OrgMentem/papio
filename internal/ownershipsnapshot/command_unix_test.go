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

	"papio/internal/config"
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

// waitForPID polls until the script has written its pid file.
func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not written", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The shared run belongs to the provider, so no lookup's cancellation reaches
// the exporter, but shutting the provider down must. Without that, a daemon
// stopped during a hung export exits and leaves the exporter's process tree
// running, and the restarted daemon starts a second copy.
func TestCommandSourceCloseKillsAHungExporterProcessTree(t *testing.T) {
	dir := t.TempDir()
	script := `echo $$ > "$1/shell"; sleep 60 & echo $! > "$1/child"; wait`
	provider, err := NewProvider(config.LibrarySource{
		Name:   "papis-live",
		Kind:   config.LibraryKindCommand,
		Argv:   []string{"/bin/sh", "-c", script, "sh", dir},
		Format: "bibtex",
		Claim:  config.LibraryClaimPDFPresent,
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	commandProvider := provider.(*commandProvider)

	ctx, cancel := context.WithCancel(context.Background())
	lookupDone := make(chan ownership.SourceHealth, 1)
	go func() {
		_, health := commandProvider.Lookup(ctx, []ownership.Query{doiQuery("10.1000/one")})
		lookupDone <- health
	}()
	shell := waitForPID(t, filepath.Join(dir, "shell"))
	child := waitForPID(t, filepath.Join(dir, "child"))
	t.Cleanup(func() {
		_ = syscall.Kill(shell, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
	})

	cancel()
	if health := <-lookupDone; health.Complete || health.FailureCode != ownership.FailureTimeout {
		t.Fatalf("cancelled lookup health = %+v, want incomplete timeout", health)
	}
	if processGone(shell, 200*time.Millisecond) {
		t.Fatal("cancelling one lookup killed the run that later lookups share")
	}

	closed := make(chan error, 1)
	go func() { closed <- commandProvider.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(commandWaitDelay + 5*time.Second):
		t.Fatal("Close did not return")
	}
	for name, pid := range map[string]int{"exporter shell": shell, "exporter child": child} {
		if !processGone(pid, 5*time.Second) {
			t.Fatalf("%s (pid %d) outlived Close", name, pid)
		}
	}

	// A lookup after shutdown must not start a second exporter.
	if err := os.Remove(filepath.Join(dir, "shell")); err != nil {
		t.Fatal(err)
	}
	if _, health := commandProvider.Lookup(context.Background(), []ownership.Query{doiQuery("10.1000/one")}); health.Complete {
		t.Fatalf("health after Close = %+v, want incomplete", health)
	}
	if _, err := os.Stat(filepath.Join(dir, "shell")); err == nil {
		t.Fatal("a lookup after Close started a second exporter")
	}
}
