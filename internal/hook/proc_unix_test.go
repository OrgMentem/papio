//go:build !windows

package hook

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

func TestTimeoutKillsWholeProcessTree(t *testing.T) {
	// A background child holding stderr must not keep Run blocked past the
	// deadline, and must be killed with the group - not merely released
	// when WaitDelay gives up on the pipes.
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	r := &Runner{Command: `sleep 30 & echo $! > "$PIDFILE"; wait`, Timeout: 500 * time.Millisecond}
	start := time.Now()
	result := r.Run(context.Background(), map[string]string{"PIDFILE": pidFile})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run blocked %v; the deadline kill did not release the pipes before WaitDelay", elapsed)
	}
	if result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want deadline error with exit -1", result)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("background pid never recorded (%v); the test proves nothing", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse background pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Signal 0 probes existence; ESRCH means the descendant is gone.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background descendant %d survived the deadline kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A hook that hands work to a background child and exits 0 is the supported
// handoff RunConfined documents: the zero exit is the outcome, not the pipes
// the child still holds past WaitDelay. The old runShell classified
// exec.ErrWaitDelay as a failure with exit -1; it must report ok instead.
// The child keeps the shell's own pipes (nothing is redirected: redirecting
// would close the very pipes the test needs held open, and Wait would return
// nil at once without exercising the rule). sleep never writes, so no
// SIGPIPE can fire when Wait later closes those pipes under it.
func TestBackgroundHandoffAfterExitZeroIsSuccess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	r := &Runner{Command: `sleep 5 & echo $! > "$PIDFILE"; exit 0`, Timeout: 10 * time.Second}
	start := time.Now()
	result := r.Run(context.Background(), map[string]string{"PIDFILE": pidFile})
	elapsed := time.Since(start)
	if result.Err != nil || result.ExitCode != 0 || !result.Ran {
		t.Fatalf("result = %+v, want ok with exit 0 and nil Err", result)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("background pid never recorded (%v); the test proves nothing", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse background pid %q: %v", raw, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// Run returned while the handed-off child still lived: the pid answers
	// signal 0, and Run came back well before the child's 5 s. If Run had
	// waited for the child instead of giving up at WaitDelay, the child
	// would be gone here.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("background descendant %d is gone; Run outlived the handoff instead of returning at WaitDelay", pid)
	}
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("Run blocked %v on a handed-off child past WaitDelay", elapsed)
	}
}
