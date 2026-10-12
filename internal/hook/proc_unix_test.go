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
