package hook

import (
	"context"
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
