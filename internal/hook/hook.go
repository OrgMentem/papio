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
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
		// NUL in a value (e.g. hostile remote metadata in a title) would make
		// os/exec reject the entire environment and the hook never start.
		extra = append(extra, key+"="+strings.ReplaceAll(env[key], "\x00", ""))
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
	cmd := shellCommand(ctx, command)
	cmd.Env = append(os.Environ(), extra...)
	cmd.Stdout = io.Discard
	stderr := &tailBuffer{limit: 2 * stderrTailLimit}
	cmd.Stderr = stderr
	// Kill the whole process tree at deadline, not just the shell: a hook
	// like `worker & wait` must not outlive the timeout or hold the stderr
	// pipe open. The tree is confined the moment it starts - a process group
	// on Unix, a kill-on-close Job Object on Windows. WaitDelay only bounds
	// papio's wait on the pipes if a descendant escaped; it never ends one.
	guard := newProcGuard(cmd)
	defer guard.close()
	cmd.Cancel = func() error { return guard.kill(cmd) }
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	err := runConfined(cmd, guard)
	result := Result{
		Ran:        true,
		Duration:   time.Since(start),
		StderrTail: capUTF8(stderr.String(), stderrTailLimit),
	}
	switch {
	case err == nil:
		result.ExitCode = 0
	case ctx.Err() != nil:
		result.Err, result.ExitCode = ctx.Err(), -1
	default:
		result.Err = err
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}
	return result
}

// runConfined starts the shell, confines it before it can spawn a descendant,
// then waits for it. Splitting exec.Cmd.Run's start and wait is what makes
// room for the Windows job assignment, which needs a process to assign.
func runConfined(cmd *exec.Cmd, guard *procGuard) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := guard.confine(cmd); err != nil {
		_ = guard.kill(cmd)
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
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

// capUTF8 repairs invalid UTF-8 first, then truncates to at most limit bytes
// on a rune boundary — replacement runes are three bytes each, so repairing
// after truncation could exceed the cap.
func capUTF8(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
