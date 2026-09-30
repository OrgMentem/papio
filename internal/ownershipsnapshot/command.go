// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"papio/internal/bibparse"
	"papio/internal/config"
	"papio/internal/hook"
	"papio/internal/ownership"
)

// commandWaitDelay bounds how long papio waits on the stdout pipe after the
// command exits or is killed, in case a descendant escaped the process tree
// and still holds it. Output that ends this way is a failed read.
const commandWaitDelay = 2 * time.Second

// commandRetryBackoff spaces reruns of a failing command. Without it every
// lookup after a failure would start the command again, and a command that
// hangs until its timeout would make every search wait that long. It is capped
// at the source's refresh interval.
const commandRetryBackoff = 30 * time.Second

// errOutputCap stops the copy of a command's stdout once it passes the cap.
var errOutputCap = errors.New("command output exceeds max_output_bytes")

// commandProvider answers lookups from the stdout of one configured command
// (ADR-0008, tier 1.1). One successful run answers every lookup for the
// refresh interval; after that, the next lookup starts one shared rerun.
type commandProvider struct {
	name     string
	argv     []string
	format   bibparse.Format
	artifact string
	limits   config.LibraryCommandLimits
	now      func() time.Time

	// mu guards the last good snapshot, the single pending run, and the last
	// failure. No caller holds it while the command runs or its output parses.
	mu sync.Mutex
	// current is the last successful run, nil until the first one.
	current *snapshot
	pending *pendingRefresh
	// failure and failedAt describe the most recent failed run; failure is
	// empty once a later run succeeds.
	failure  string
	failedAt time.Time
	runs     int
}

// commandSettings resolves the argv and bounds shared by the provider and by
// EnumerateLibraryRecords. A leading "~/" in argv[0] expands so a directly
// constructed source behaves as Config.Load would make it.
func commandSettings(source config.LibrarySource) ([]string, config.LibraryCommandLimits, error) {
	if len(source.Argv) == 0 || strings.TrimSpace(source.Argv[0]) == "" {
		return nil, config.LibraryCommandLimits{}, fmt.Errorf("argv is required for kind %q", config.LibraryKindCommand)
	}
	if source.Path != "" {
		return nil, config.LibraryCommandLimits{}, fmt.Errorf("path must be empty for kind %q", config.LibraryKindCommand)
	}
	limits, err := source.CommandLimits()
	if err != nil {
		return nil, config.LibraryCommandLimits{}, err
	}
	argv := slices.Clone(source.Argv)
	argv[0] = expandHome(argv[0])
	return argv, limits, nil
}

func newCommandProvider(name string, source config.LibrarySource, artifact string, now func() time.Time) (*commandProvider, error) {
	argv, limits, err := commandSettings(source)
	if err != nil {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	return &commandProvider{
		name:     name,
		argv:     argv,
		format:   bibparse.Format(source.Format),
		artifact: artifact,
		limits:   limits,
		now:      now,
	}, nil
}

func (p *commandProvider) Name() string { return p.name }

// Lookup answers from the last successful run while it is younger than the
// refresh interval. Otherwise it joins or starts the single shared run and
// answers from its result. A failed run is never an empty library: the source
// reports incomplete, and the last good index may still annotate, as stale,
// until it is older than the refresh interval plus freshnessWindow. Past that
// bound it asserts nothing at all.
func (p *commandProvider) Lookup(ctx context.Context, queries []ownership.Query) ([][]ownership.Claim, ownership.SourceHealth) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return answerFrom(p.name, p.servable(), false, ownership.FailureTimeout, queries)
	}
	pend, snap, failure := p.begin(ctx)
	if pend == nil {
		return answerFrom(p.name, snap, failure == "", failure, queries)
	}
	// The run itself is bounded by its timeout plus the pipe wait; the extra
	// second only absorbs scheduling, so a waiter never outlives the run by much.
	timer := time.NewTimer(p.limits.Timeout + commandWaitDelay + time.Second)
	defer timer.Stop()
	select {
	case <-pend.done:
		return answerFrom(p.name, pend.snap, pend.complete, pend.failure, queries)
	case <-ctx.Done():
		return answerFrom(p.name, p.servable(), false, ownership.FailureTimeout, queries)
	case <-timer.C:
		return answerFrom(p.name, p.servable(), false, ownership.FailureTimeout, queries)
	}
}

// begin decides, under one lock, whether this lookup can answer without
// running the command. It returns a pending run to wait on, or the snapshot and
// failure code to answer with now.
func (p *commandProvider) begin(ctx context.Context) (*pendingRefresh, *snapshot, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.current != nil {
		// A clock that moved backwards makes the age negative; rerunning is
		// the safe reading of an age papio cannot trust.
		if age := now.Sub(p.current.loadedAt); age >= 0 && age < p.limits.Refresh {
			return nil, p.current, ""
		}
	}
	if p.pending != nil {
		return p.pending, nil, ""
	}
	if p.failure != "" {
		backoff := min(commandRetryBackoff, p.limits.Refresh)
		if age := now.Sub(p.failedAt); age >= 0 && age < backoff {
			return nil, p.servableLocked(now), p.failure
		}
	}
	pend := &pendingRefresh{done: make(chan struct{})}
	p.pending = pend
	p.runs++
	previous := p.current
	// The run is detached from this waiter's cancellation so an abandoned wait
	// cannot cancel the run later lookups are already sharing; the command's
	// own timeout still bounds it.
	go func() {
		snap, failure := p.run(context.WithoutCancel(ctx), previous)
		p.mu.Lock()
		if failure == "" {
			p.current, p.failure, p.failedAt = snap, "", time.Time{}
			pend.snap, pend.complete = snap, true
		} else {
			p.failure, p.failedAt = failure, p.now()
			pend.snap, pend.failure = p.servableLocked(p.failedAt), failure
		}
		p.pending = nil
		p.mu.Unlock()
		close(pend.done)
	}()
	return pend, nil, ""
}

// run executes the command once and indexes its stdout. It publishes nothing:
// the caller decides what a success or failure changes.
func (p *commandProvider) run(ctx context.Context, previous *snapshot) (*snapshot, string) {
	data, failure := runCommand(ctx, p.argv, p.limits.Timeout, p.limits.MaxOutputBytes)
	if failure != "" {
		return nil, failure
	}
	records, failure := parseHoldings(p.format, "", data)
	if failure != "" {
		return nil, failure
	}
	if collapsed(previous, len(records)) {
		// A command killed mid-export can exit cleanly with a fraction of the
		// library; accepting it would quietly disable de-duplication.
		return nil, ownership.FailureCountCollapse
	}
	return &snapshot{
		index:      ownership.BuildIndex(entriesFromRecords(records, p.artifact)),
		loadedAt:   p.now(),
		entryCount: len(records),
	}, ""
}

func (p *commandProvider) servable() *snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.servableLocked(p.now())
}

// servableLocked is the last good snapshot an incomplete answer may still
// show, as stale annotation. Past the refresh interval plus freshnessWindow it
// keeps only the time of the last success, for diagnostics, and asserts no
// claims: an index that old is no longer evidence of what the library holds.
func (p *commandProvider) servableLocked(now time.Time) *snapshot {
	if p.current == nil {
		return nil
	}
	if age := now.Sub(p.current.loadedAt); age < 0 || age > p.limits.Refresh+freshnessWindow {
		return &snapshot{index: ownership.BuildIndex(nil), loadedAt: p.current.loadedAt}
	}
	return p.current
}

// runCommand runs argv once, with no shell, and returns its complete stdout or
// a bounded failure code. The command inherits the daemon environment; stdin
// is the null device and stderr is discarded unread, because it can print
// credentials. The whole process tree is killed at the timeout or as soon as
// stdout passes maxBytes, and the child is waited for exactly once. Output
// past the cap is a failed read, never a truncated one: a prefix of a library
// would parse cleanly and look like a smaller library.
func runCommand(ctx context.Context, argv []string, timeout time.Duration, maxBytes int64) ([]byte, string) {
	if len(argv) == 0 {
		return nil, ownership.FailureNotConfigured
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	stdout := &cappedOutput{limit: maxBytes, overflow: cancel}
	cmd.Stdout = stdout
	cmd.WaitDelay = commandWaitDelay
	err := hook.RunConfined(cmd)
	// RunConfined has returned, so the copy goroutine that wrote stdout is done.
	switch {
	case stdout.exceeded:
		return nil, ownership.FailureTruncated
	case runCtx.Err() != nil:
		return nil, ownership.FailureTimeout
	case err != nil:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, ownership.FailureExit
		}
		// A start failure, or exec.ErrWaitDelay: a descendant held stdout past
		// the command's exit, so the bytes read may not be the whole export.
		return nil, ownership.FailureUnreadable
	}
	return stdout.data, ""
}

// cappedOutput collects stdout up to limit bytes. Passing the limit marks the
// read failed and cancels the run, which kills the process tree, rather than
// waiting for a runaway command to reach its timeout. os/exec writes to it from
// one goroutine and Wait returns only after that goroutine ends.
type cappedOutput struct {
	data     []byte
	limit    int64
	exceeded bool
	overflow context.CancelFunc
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.exceeded {
		return 0, errOutputCap
	}
	if int64(len(c.data))+int64(len(p)) > c.limit {
		c.exceeded = true
		c.overflow()
		return 0, errOutputCap
	}
	c.data = append(c.data, p...)
	return len(p), nil
}
