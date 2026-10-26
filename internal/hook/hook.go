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

// PipeWaitDelay bounds how long Run waits for output pipes a background
// descendant still holds after the leader exits. A hook run therefore ends
// no later than its Timeout plus this delay, which lets startup recovery
// age-gate reservations no live run can still hold.
const PipeWaitDelay = 3 * time.Second

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
	// or was killed by the deadline. A background handoff that exits 0 but
	// still holds the output pipes past WaitDelay reports 0 with a nil Err.
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
	// WaitDelay only bounds papio's wait on the pipes if a descendant escaped
	// the confinement RunConfined applies; it never ends one.
	cmd.WaitDelay = PipeWaitDelay

	start := time.Now()
	err := RunConfined(cmd)
	result := Result{
		Ran:        true,
		Duration:   time.Since(start),
		StderrTail: capUTF8(stderr.String(), stderrTailLimit),
	}
	exitedZero := err == nil || (cmd.ProcessState != nil && cmd.ProcessState.Success())
	// A WaitDelay after a zero exit (the leader handed work to a background
	// child that still holds the pipes) is a success: the leader already
	// exited 0, so the pipes Wait closed under the child carry no failure.
	// The ctx.Err check stays first — a timed-out or cancelled context still
	// reports its own error even if the pipes also hit WaitDelay — and the
	// old default (WaitDelay with exit -1) is how this code behaves once the
	// WaitDelay-after-zero-success is excluded.
	var waitErr error
	switch {
	case ctx.Err() != nil && err != nil:
		result.Err, result.ExitCode = ctx.Err(), -1
		return result
	case errors.Is(err, exec.ErrWaitDelay) && exitedZero:
		waitErr = nil
	default:
		waitErr = err
	}
	classification := classifyWaitResult(waitErr)
	result.ExitCode, result.Err = classification.ExitCode, classification.Err
	return result
}

// classifyWaitResult maps one wait error to its Result prefix (ExitCode and
// Err only; runShell fills the rest). The background-handoff rule lives in
// runShell, which sees the live *exec.Cmd and ctx; the classifier stays a
// pure mapping so unit tests pin it without a subprocess.
func classifyWaitResult(waitErr error) Result {
	if waitErr == nil {
		return Result{ExitCode: 0}
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		return Result{ExitCode: exit.ExitCode(), Err: waitErr}
	}
	return Result{ExitCode: -1, Err: waitErr}
}

// RunConfined runs cmd as one confined process tree and waits for it exactly
// once. The tree is confined the moment it starts - a process group on Unix,
// a kill-on-close Job Object on Windows - and cmd.Cancel is set so a context
// deadline kills the whole tree, not just the direct child: a command like
// `worker & wait` must not outlive its timeout or hold an output pipe open.
// A background descendant left running after cmd exits in time survives on
// Unix; an on_ready hook may deliberately hand work off that way.
// cmd must come from exec.CommandContext and must not be started yet; RunConfined
// owns cmd.Cancel and the Unix process-group attribute.
func RunConfined(cmd *exec.Cmd) error {
	return runGuarded(cmd, false)
}

// RunConfinedAndReap is RunConfined for a command whose output is its whole
// result, such as a library command source. Once Wait returns, on every path -
// a clean exit, a non-zero exit, the deadline, or exec.ErrWaitDelay - it also
// kills every process left in the tree, so a `worker & exit 0` descendant
// cannot outlive the run.
func RunConfinedAndReap(cmd *exec.Cmd) error {
	return runGuarded(cmd, true)
}

func runGuarded(cmd *exec.Cmd, reap bool) error {
	guard := newProcGuard(cmd)
	defer guard.close()
	cmd.Cancel = func() error { return guard.kill(cmd) }
	err := runConfined(cmd, guard)
	if reap {
		guard.reap(cmd)
	}
	return err
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

// capUTF8 repairs invalid UTF-8 first, then keeps the final at most limit
// bytes, starting on a rune boundary — replacement runes are three bytes
// each, so repairing after truncation could exceed the cap.
func capUTF8(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
