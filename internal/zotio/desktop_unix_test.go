// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build unix

package zotio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The waiter is a real child process that sleeps until Zotero starts. Daemon
// shutdown cancels its context, and the process must be gone when
// WaitForDesktop returns: a leaked waiter would outlive the daemon.
func TestWaitForDesktopKillsWaiterOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "waiter.pid")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = capabilities ]; then printf '%s' '" + strings.ReplaceAll(presenceRegistry, "\n", " ") + "'; exit 0; fi\n" +
		"echo $$ > " + pidFile + "\n" +
		"exec sleep 60\n"
	executable := filepath.Join(dir, "zotio")
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &Client{Executable: executable, Timeout: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.WaitForDesktop(ctx)
		done <- err
	}()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		if time.Now().After(deadline) {
			t.Fatal("fake waiter never started")
		}
		raw, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled wait did not return")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("waiter pid %d still runs after cancel", pid)
	}
}
