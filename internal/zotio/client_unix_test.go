// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build unix

package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const appliedEnvelope = `{"ok":true,"mode":"apply","result":{"summary":{"applied":1}}}`

// launcherClient builds a Client over a shell script that starts a long-lived
// descendant which inherits stderr but not stdout, records its pid, and then
// runs body.
func launcherClient(t *testing.T, body string, timeout time.Duration) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	script := "#!/bin/sh\n" +
		"sleep 60 >/dev/null &\n" +
		"echo $! > " + pidFile + "\n" +
		body + "\n"
	executable := filepath.Join(dir, "zotio")
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Executable: executable, Timeout: timeout, waitDelay: 300 * time.Millisecond}, pidFile
}

func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(pidFile)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant pid was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Signal 0 probes existence; ESRCH means the process is gone.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d outlived the command", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type runResult struct {
	out json.RawMessage
	err error
}

// runBounded fails the test, and kills the descendant, when RunJSON does not
// return within limit: the unbounded runner would sit until the descendant
// closed the pipes.
func runBounded(t *testing.T, client *Client, ctx context.Context, pidFile string, limit time.Duration) runResult {
	t.Helper()
	done := make(chan runResult, 1)
	go func() {
		out, err := client.RunJSON(ctx, "--agent", "--yes", "import", "apply")
		done <- runResult{out, err}
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(limit):
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Fatalf("RunJSON did not return within %s: a descendant holding the output pipes blocked it", limit)
		return runResult{}
	}
}

// A launcher's descendant can inherit stderr and outlive zotio. zotio has
// already printed its envelope and exited 0, so the result stands, the call
// returns after the bounded drain, and the descendant is collected.
func TestRunJSONReturnsEnvelopeWhenDescendantHoldsStderrPastExit(t *testing.T) {
	client, pidFile := launcherClient(t, "printf '%s' '"+appliedEnvelope+"'\nexit 0", 30*time.Second)
	result := runBounded(t, client, context.Background(), pidFile, 10*time.Second)
	if result.err != nil {
		t.Fatalf("RunJSON err = %v, want the successful envelope", result.err)
	}
	if string(result.out) != appliedEnvelope {
		t.Fatalf("RunJSON out = %s, want the envelope zotio printed", result.out)
	}
	requireGone(t, readPID(t, pidFile))
}

// The deadline kills the whole tree. The envelope zotio printed before the
// deadline is preserved beside the timeout error, and the descendant dies with
// the tree instead of holding the pipes.
func TestRunJSONDeadlineKillsDescendantAndKeepsEnvelope(t *testing.T) {
	client, pidFile := launcherClient(t, "printf '%s' '"+appliedEnvelope+"'\nwait", 500*time.Millisecond)
	start := time.Now()
	result := runBounded(t, client, context.Background(), pidFile, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RunJSON took %s after a 500ms deadline", elapsed)
	}
	if !errors.Is(result.err, context.DeadlineExceeded) || !strings.Contains(result.err.Error(), "timed out") {
		t.Fatalf("RunJSON err = %v, want a timeout wrapping DeadlineExceeded", result.err)
	}
	if string(result.out) != appliedEnvelope {
		t.Fatalf("RunJSON out = %s, want the envelope zotio printed before the deadline", result.out)
	}
	requireGone(t, readPID(t, pidFile))
}

// Parent cancellation is a cancel, not a timeout, and it also ends promptly
// with the descendant collected.
func TestRunJSONParentCancelKillsDescendantAndClassifiesAsCancel(t *testing.T) {
	client, pidFile := launcherClient(t, "printf '%s' '"+appliedEnvelope+"'\nwait", 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		readPID(t, pidFile)
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	result := runBounded(t, client, ctx, pidFile, 10*time.Second)
	if !errors.Is(result.err, context.Canceled) || strings.Contains(result.err.Error(), "timed out") {
		t.Fatalf("RunJSON err = %v, want cancel wording wrapping context.Canceled", result.err)
	}
	if string(result.out) != appliedEnvelope {
		t.Fatalf("RunJSON out = %s, want the envelope zotio printed before the cancel", result.out)
	}
	requireGone(t, readPID(t, pidFile))
}

// A chatty stderr must not discard the stdout envelope of a command that
// exited 0: a mutation that succeeded would otherwise read as a failure.
func TestRunJSONKeepsEnvelopeWhenStderrOverflows(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"head -c " + strconv.Itoa(maxStderrBytes+4096) + " /dev/zero | tr '\\0' 'x' >&2\n" +
		"printf '%s' '" + appliedEnvelope + "'\n"
	executable := filepath.Join(dir, "zotio")
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &Client{Executable: executable, Timeout: 30 * time.Second}
	out, err := client.RunJSON(context.Background(), "--agent", "--yes", "import", "apply")
	if err != nil || string(out) != appliedEnvelope {
		t.Fatalf("RunJSON = %s, %v; want the envelope and no error", out, err)
	}
}

// A mutation killed by a signal reports nothing, and a mutation that cannot
// start reports nothing either. Only the second is a refusal that proves
// Zotero is unchanged.
func TestRunJSONSeparatesSignalDeathFromCleanExit(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		definite bool
	}{
		{"signal", "kill -KILL $$", false},
		{"clean non-zero exit", "echo 'refused' >&2\nexit 3", true},
		{"unreadable exit-0 output", "printf 'not json'\nexit 0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "zotio")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+tc.body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			client := &Client{Executable: executable, Timeout: 30 * time.Second}
			out, err := client.RunJSON(context.Background(), "--agent", "--yes", "import", "apply")
			if err == nil || len(out) != 0 {
				t.Fatalf("RunJSON = %s, %v; want an error and no envelope", out, err)
			}
			if got := isDefiniteFailure(err); got != tc.definite {
				t.Fatalf("isDefiniteFailure(%v) = %t, want %t", err, got, tc.definite)
			}
		})
	}
}
