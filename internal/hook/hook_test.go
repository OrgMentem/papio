package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEmptyCommandDoesNotRun(t *testing.T) {
	r := &Runner{Command: "   "}
	result := r.Run(context.Background(), map[string]string{"PAPIO_JOB_ID": "j1"})
	if result.Ran {
		t.Fatalf("empty command ran: %+v", result)
	}
}

func TestExitCodePropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: "exit 3", Timeout: 10 * time.Second}
	result := r.Run(context.Background(), nil)
	if !result.Ran || result.ExitCode != 3 {
		t.Fatalf("result = %+v, want Ran exit 3", result)
	}
}

func TestStderrTailCaptured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: "echo oops >&2; exit 1", Timeout: 10 * time.Second}
	result := r.Run(context.Background(), nil)
	if result.ExitCode != 1 || !strings.Contains(result.StderrTail, "oops") {
		t.Fatalf("result = %+v, want exit 1 with stderr oops", result)
	}
}

func TestStderrTailBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: "yes A 2>/dev/null | head -c 4000 >&2; exit 0", Timeout: 10 * time.Second}
	result := r.Run(context.Background(), nil)
	if len(result.StderrTail) > stderrTailLimit {
		t.Fatalf("stderr tail = %d bytes, want <= %d", len(result.StderrTail), stderrTailLimit)
	}
}

func TestTimeoutKillsCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: "sleep 5", Timeout: 100 * time.Millisecond}
	start := time.Now()
	result := r.Run(context.Background(), nil)
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not kill the command promptly")
	}
	if result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want deadline error with exit -1", result)
	}
}

func TestEnvDelivered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: `test "$PAPIO_DOI" = 10.1000/x`, Timeout: 10 * time.Second}
	result := r.Run(context.Background(), map[string]string{"PAPIO_DOI": "10.1000/x"})
	if result.ExitCode != 0 {
		t.Fatalf("result = %+v, want env-visible exit 0", result)
	}
}

func TestInjectableExecReceivesSortedEnv(t *testing.T) {
	var gotCommand string
	var gotEnv []string
	r := &Runner{
		Command: "anything",
		Exec: func(_ context.Context, command string, env []string) Result {
			gotCommand, gotEnv = command, env
			return Result{Ran: true}
		},
	}
	result := r.Run(context.Background(), map[string]string{"B_KEY": "2", "A_KEY": "1"})
	if !result.Ran || gotCommand != "anything" {
		t.Fatalf("exec not used: %+v command=%q", result, gotCommand)
	}
	if len(gotEnv) != 2 || gotEnv[0] != "A_KEY=1" || gotEnv[1] != "B_KEY=2" {
		t.Fatalf("env = %v, want sorted A_KEY,B_KEY", gotEnv)
	}
}

func TestTimeoutKillsWholeProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group test")
	}
	// A background child holding stderr must not keep Run blocked past the
	// deadline, and must be killed with the group.
	r := &Runner{Command: "sleep 30 & wait", Timeout: 100 * time.Millisecond}
	start := time.Now()
	result := r.Run(context.Background(), nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run blocked %v; descendant held the pipes past WaitDelay", elapsed)
	}
	if result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want deadline error with exit -1", result)
	}
}

func TestTimeoutKillsWindowsJobTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Job Object test: no job object to confine the tree on this platform")
	}
	// The hook shell detaches a grandchild and then blocks past the deadline.
	// Killing cmd.exe alone leaves that grandchild running - Windows has no
	// implicit tree kill - so it announces itself immediately and writes the
	// escape marker only after the hook has been killed. A marker means the
	// job did not take the tree with it.
	dir := t.TempDir()
	script := filepath.Join(dir, "child.cmd")
	body := "@echo off\r\n" +
		"echo started> \"%~dp0started.txt\"\r\n" +
		"ping -n 8 127.0.0.1 >NUL\r\n" +
		"echo escaped> \"%~dp0escaped.txt\"\r\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatalf("write child script: %v", err)
	}
	r := &Runner{
		Command: `start "" /b cmd /c "` + script + `" & ping -n 60 127.0.0.1 >NUL`,
		Timeout: 2 * time.Second,
	}
	start := time.Now()
	result := r.Run(context.Background(), nil)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run blocked %v; the descendant held the pipes past WaitDelay", elapsed)
	}
	if !errors.Is(result.Err, context.DeadlineExceeded) || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want deadline-exceeded with exit -1", result)
	}
	// Without this the test could pass vacuously: a deadline that fired before
	// the shell reached `start` proves nothing about the job.
	if _, err := os.Stat(filepath.Join(dir, "started.txt")); err != nil {
		t.Fatalf("descendant never started (%v); the test proves nothing", err)
	}
	// Outlast the grandchild's own delay, then require its marker absent.
	time.Sleep(12 * time.Second)
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape marker stat = %v; the descendant survived the job kill", err)
	}
}

func TestCapUTF8BoundsAfterRepair(t *testing.T) {
	// Alternating invalid/valid bytes: each invalid byte becomes a 3-byte
	// replacement rune, so repairing 500 raw bytes can exceed 500. The cap
	// must apply after repair, on a rune boundary.
	raw := strings.Repeat("\xffA", stderrTailLimit)
	capped := capUTF8(raw, stderrTailLimit)
	if len(capped) > stderrTailLimit {
		t.Fatalf("capped length = %d, want <= %d", len(capped), stderrTailLimit)
	}
	if !strings.HasPrefix(capped, "\uFFFDA") {
		t.Fatalf("capped prefix = %q, want replacement-rune repair", capped[:6])
	}
	if capUTF8("short", stderrTailLimit) != "short" {
		t.Fatal("short valid input must pass through")
	}
}

func TestNULInEnvValueDoesNotBreakExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	r := &Runner{Command: `test "$PAPIO_TITLE" = AB`, Timeout: 10 * time.Second}
	result := r.Run(context.Background(), map[string]string{"PAPIO_TITLE": "A\x00B"})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want NUL stripped and exec to run", result)
	}
}
