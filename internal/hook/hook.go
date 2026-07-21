// Package hook runs user-configured, fire-and-forget lifecycle commands.
//
// Hooks are deliberately unserialized: concurrent ready jobs may run their
// hooks concurrently, and the user's command owns its own locking if it
// needs any. A hook failure is recorded by the caller but never fails or
// retries the job that triggered it.
package hook

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// stderrTailLimit bounds how much captured stderr survives into Result.
const stderrTailLimit = 500

// Runner executes one configured shell command line.
type Runner struct {
	// Command is the shell command line; empty disables the runner.
	Command string
	// Timeout bounds one run. Non-positive means no deadline.
	Timeout time.Duration
	// Exec is injectable for tests; nil uses the real shell exec.
	Exec func(ctx context.Context, command string, env []string) Result
}

// Result reports one hook run. Errors are carried here, never returned:
// hooks are best-effort by contract.
type Result struct {
	// Ran is false when the runner is disabled (empty Command).
	Ran bool
	// ExitCode is the command's exit status; -1 when it failed to start
	// or was killed by the deadline.
	ExitCode int
	Err      error
	Duration time.Duration
	// StderrTail is the final <=500 bytes of stderr, coerced to valid UTF-8.
	StderrTail string
}

// Run executes the command with the supplied extra env vars appended to
// os.Environ(). It blocks up to Timeout and never returns an error to the
// caller - failures are carried in the Result.
func (r *Runner) Run(ctx context.Context, env map[string]string) Result {
	if r == nil || strings.TrimSpace(r.Command) == "" {
		return Result{}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	extra := make([]string, 0, len(env))
	for _, key := range keys {
		extra = append(extra, key+"="+env[key])
	}
	if r.Exec != nil {
		return r.Exec(ctx, r.Command, extra)
	}
	return runShell(ctx, r.Command, extra, r.Timeout)
}

func runShell(ctx context.Context, command string, extra []string, timeout time.Duration) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Env = append(os.Environ(), extra...)
	cmd.Stdout = io.Discard
	stderr := &tailBuffer{limit: stderrTailLimit}
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	result := Result{
		Ran:        true,
		Duration:   time.Since(start),
		StderrTail: strings.ToValidUTF8(stderr.String(), "\uFFFD"),
	}
	switch {
	case err == nil:
		result.ExitCode = 0
	case ctx.Err() != nil:
		result.Err, result.ExitCode = ctx.Err(), -1
	default:
		result.Err = err
		if exit, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exit.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}
	return result
}

// tailBuffer keeps only the final limit bytes written to it.
type tailBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.limit {
		t.buf.Reset()
		t.buf.Write(p[n-t.limit:])
		return n, nil
	}
	if overflow := t.buf.Len() + n - t.limit; overflow > 0 {
		rest := t.buf.Bytes()[overflow:]
		remaining := make([]byte, len(rest))
		copy(remaining, rest)
		t.buf.Reset()
		t.buf.Write(remaining)
	}
	t.buf.Write(p)
	return n, nil
}

func (t *tailBuffer) String() string { return t.buf.String() }
