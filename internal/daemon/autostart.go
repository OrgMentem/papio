// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package daemon contains process-lifetime services used by the papio daemon.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"papio/internal/ipc"
)

// CommandFactory constructs the daemon command. The default uses exec.Command
// with the current executable; it never invokes a shell.
type CommandFactory func(name string, args ...string) *exec.Cmd

// Autostarter starts the daemon when its local socket is unavailable and no
// daemon process is alive to answer it. Its seams make command wiring
// unit-testable without launching a daemon.
type Autostarter struct {
	SocketPath string
	// DataDir is the data directory the daemon serves, whose instance claim
	// says whether a daemon process is alive. It defaults to the socket's
	// directory, which is where papio puts the socket.
	DataDir  string
	Args     []string
	LockPath string
	LogPath  string

	// StartTimeout is how long a daemon this call started may take to reach
	// its socket before it is treated as hung and terminated. A daemon that
	// reports it is upgrading or checking its database is never terminated:
	// that work takes as long as the database needs.
	StartTimeout time.Duration
	// MaxWait bounds the whole call: waiting for another command that is
	// starting the daemon, and for a daemon that is upgrading its database or
	// stopping. A daemon still at work when it passes keeps running.
	MaxWait       time.Duration
	RetryInterval time.Duration
	// OnWait is told, once for each phase, when this call waits for a daemon
	// that is upgrading or checking its database or stopping, so that a
	// person can be told why the command is slow. The integrity check runs on
	// every start and is usually brief, so it is announced only once it has
	// lasted checkingNotice, or when it follows an announced upgrade.
	OnWait func(Status)

	Executable func() (string, error)
	Command    CommandFactory
	Start      func(context.Context, *exec.Cmd) error
	Ready      func(context.Context, string) error
	OpenNull   func() (*os.File, error)
	OpenLog    func() (*os.File, error)

	gracePeriod    time.Duration
	checkingNotice time.Duration
}

// NewAutostarter returns an autostarter with production-safe defaults.
func NewAutostarter(socketPath string) *Autostarter {
	return &Autostarter{SocketPath: socketPath}
}

// EnsureResult describes how EnsureWithResult made the daemon available.
type EnsureResult struct {
	// Started reports whether this call launched the daemon process that
	// answers. It stays true when readiness fails after the launch.
	Started bool
}

// Ensure ensures a daemon is ready. Callers that need to know whether this
// invocation launched it should use EnsureWithResult.
func (a *Autostarter) Ensure(ctx context.Context) error {
	_, err := a.EnsureWithResult(ctx)
	return err
}

// EnsureWithResult returns once a daemon's socket is ready. Contending
// callers share an advisory lock, so at most one of them launches a daemon at
// a time, and none launches one while a daemon process is alive: a daemon
// that is starting, upgrading its database or stopping is waited for, never
// joined by a second one.
func (a *Autostarter) EnsureWithResult(ctx context.Context) (EnsureResult, error) {
	cfg, err := a.defaults()
	if err != nil {
		return EnsureResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return EnsureResult{}, err
	}
	if err := cfg.Ready(ctx, cfg.SocketPath); err == nil {
		return EnsureResult{}, nil
	} else if ctx.Err() != nil {
		return EnsureResult{}, ctx.Err()
	}
	wait := &startWait{cfg: cfg, deadline: time.Now().Add(cfg.MaxWait)}
	unlock, ready, err := wait.lock(ctx)
	if err != nil || ready {
		return EnsureResult{}, err
	}
	defer unlock()
	return wait.run(ctx)
}

// startWait is one EnsureWithResult call's wait for a daemon.
type startWait struct {
	cfg      Autostarter
	deadline time.Time
	// announced is the last phase passed to OnWait.
	announced Phase
	// checkingSince is when this wait first saw the daemon checking.
	checkingSince time.Time
}

// errWaitExpired reports that MaxWait has passed.
var errWaitExpired = errors.New("autostart wait expired")

// lock takes the autostart lock. While another command holds it, the daemon
// that command started can become ready, and then no lock is needed.
func (w *startWait) lock(ctx context.Context) (unlock func(), ready bool, err error) {
	if err := os.MkdirAll(filepath.Dir(w.cfg.LockPath), 0o700); err != nil {
		return nil, false, fmt.Errorf("create autostart lock directory: %w", err)
	}
	file, err := os.OpenFile(w.cfg.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open autostart lock: %w", err)
	}
	for {
		locked, err := tryLockFile(file)
		if err != nil {
			_ = file.Close()
			return nil, false, fmt.Errorf("lock autostart: %w", err)
		}
		if locked {
			return func() {
				_ = unlockFile(file)
				_ = file.Close()
			}, false, nil
		}
		if w.cfg.Ready(ctx, w.cfg.SocketPath) == nil {
			_ = file.Close()
			return nil, true, nil
		}
		status, held := w.observe()
		if err := w.pause(ctx); err != nil {
			_ = file.Close()
			if errors.Is(err, errWaitExpired) {
				return nil, false, w.expired(nil, status, held)
			}
			return nil, false, err
		}
	}
}

// run holds the autostart lock. Each round it returns once the socket
// answers, waits while a daemon process is alive, or launches one when none
// is.
func (w *startWait) run(ctx context.Context) (EnsureResult, error) {
	var result EnsureResult
	var child *daemonChild
	for {
		if err := w.cfg.Ready(ctx, w.cfg.SocketPath); err == nil {
			return result, nil
		} else if ctx.Err() != nil {
			return result, w.abandon(child, ctx.Err())
		}
		status, held := w.observe()
		child.noteStoreWork(status, held)
		if held && status.Phase == PhaseRunning && status.Socket != "" && filepath.Clean(status.Socket) != filepath.Clean(w.cfg.SocketPath) {
			return result, fmt.Errorf("the daemon (pid %d) for %s serves %s, not %s; stop it with 'papio daemon stop' or use its socket", status.PID, w.cfg.DataDir, status.Socket, w.cfg.SocketPath)
		}
		switch {
		case child != nil && !child.exited():
			if !child.busy && time.Since(child.started) >= w.cfg.StartTimeout && !w.recheckStoreWork(child) {
				child.terminate(w.cfg.gracePeriod)
				return result, fmt.Errorf("wait for daemon socket: the daemon did not become ready within %s; see %s", w.cfg.StartTimeout, w.cfg.LogPath)
			}
		case child != nil:
			if !held {
				if child.err != nil {
					return result, fmt.Errorf("the daemon exited before it became ready: %w; see %s", child.err, w.cfg.LogPath)
				}
				return result, fmt.Errorf("the daemon exited before it became ready; see %s", w.cfg.LogPath)
			}
			// The daemon this call launched found another daemon process
			// holding the data directory and left. That one is what will
			// answer.
			child = nil
			result.Started = false
		case !held:
			launched, err := w.launch(ctx)
			if err != nil {
				return result, err
			}
			child = launched
			// Callers must learn a daemon was launched even when readiness
			// later fails: the CLI suppresses its version-skew warning on
			// this flag.
			result.Started = true
			continue
		}
		if err := w.pause(ctx); err != nil {
			if errors.Is(err, errWaitExpired) {
				return result, w.expired(child, status, held)
			}
			return result, w.abandon(child, err)
		}
	}
}

// observe reads the daemon instance, and announces through OnWait a phase
// that a person would otherwise wait through in silence.
func (w *startWait) observe() (Status, bool) {
	status, held := ReadInstance(w.cfg.DataDir)
	if held && status.Phase != w.announced && w.worthAnnouncing(status.Phase) {
		w.announced = status.Phase
		if w.cfg.OnWait != nil {
			w.cfg.OnWait(status)
		}
	}
	return status, held
}

// worthAnnouncing reports whether a person should be told that the command
// waits for a daemon in phase.
func (w *startWait) worthAnnouncing(phase Phase) bool {
	switch phase {
	case PhaseUpgrading, PhaseStopping:
		return true
	case PhaseChecking:
		if w.announced == PhaseUpgrading {
			return true
		}
		if w.checkingSince.IsZero() {
			w.checkingSince = time.Now()
		}
		return time.Since(w.checkingSince) >= w.cfg.checkingNotice
	default:
		return false
	}
}

// recheckStoreWork reads the instance again just before this call would
// terminate child, and reports whether child has begun store work since the
// wait loop last looked. Without it a daemon that recorded "upgrading" a
// moment after the last read would be stopped part way through a migration.
func (w *startWait) recheckStoreWork(child *daemonChild) bool {
	status, held := ReadInstance(w.cfg.DataDir)
	child.noteStoreWork(status, held)
	return child.busy
}

// pause waits one retry interval, unless the caller's context ends or MaxWait
// has passed.
func (w *startWait) pause(ctx context.Context) error {
	if !time.Now().Before(w.deadline) {
		return errWaitExpired
	}
	timer := time.NewTimer(w.cfg.RetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// abandon ends the wait for a caller whose context ended. A daemon this call
// launched that is still in its first steps is terminated, as a hung one is;
// one that is upgrading or checking its database carries on, and the next
// command finds it.
func (w *startWait) abandon(child *daemonChild, err error) error {
	if child == nil || child.busy || child.exited() || w.recheckStoreWork(child) {
		return err
	}
	child.terminate(w.cfg.gracePeriod)
	return fmt.Errorf("wait for daemon socket: %w", err)
}

// expired explains a wait that reached MaxWait. It terminates only a daemon
// this call launched that never reported store work.
func (w *startWait) expired(child *daemonChild, status Status, held bool) error {
	if child != nil && !child.busy && !child.exited() && !w.recheckStoreWork(child) {
		child.terminate(w.cfg.gracePeriod)
		return fmt.Errorf("wait for daemon socket: the daemon did not become ready within %s; see %s", w.cfg.MaxWait, w.cfg.LogPath)
	}
	if child != nil && child.busy {
		status, held = ReadInstance(w.cfg.DataDir)
	}
	if !held {
		return fmt.Errorf("wait for daemon socket: another papio process was still starting the daemon after %s; see %s", w.cfg.MaxWait, w.cfg.LogPath)
	}
	daemon := "the daemon"
	if status.PID != 0 {
		daemon = fmt.Sprintf("the daemon (pid %d)", status.PID)
	}
	if status.Phase.storeWork() {
		return fmt.Errorf("%s is still %s after %s; it keeps running, so retry in a few minutes (see %s)", daemon, status.Activity(), w.cfg.MaxWait, w.cfg.LogPath)
	}
	return fmt.Errorf("wait for daemon socket: %s is still %s after %s; see %s", daemon, status.Activity(), w.cfg.MaxWait, w.cfg.LogPath)
}

// launch starts one detached daemon process.
func (w *startWait) launch(ctx context.Context) (*daemonChild, error) {
	executable, err := w.cfg.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate papio executable: %w", err)
	}
	cmd := w.cfg.Command(executable, w.cfg.Args...)
	if cmd == nil {
		return nil, errors.New("daemon command factory returned nil")
	}
	null, err := w.cfg.OpenNull()
	if err != nil {
		return nil, fmt.Errorf("open detached daemon stdio: %w", err)
	}
	logFile, err := w.cfg.OpenLog()
	if err != nil {
		_ = null.Close()
		return nil, fmt.Errorf("open detached daemon log: %w", err)
	}
	// stdin is discarded; stdout/stderr persist to the daemon log so that
	// server-side errors (see the api failure handlers, which log the wrapped
	// error before returning a safe RPC message) remain diagnosable after the
	// detached daemon outlives this caller. Previously all three went to
	// /dev/null, turning any swallowed error into an undebuggable [unknown].
	cmd.Stdin = null
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := ctx.Err(); err != nil {
		_ = null.Close()
		_ = logFile.Close()
		return nil, err
	}
	configureDaemonProcessGroup(cmd)
	err = w.cfg.Start(ctx, cmd)
	_ = null.Close()
	_ = logFile.Close()
	if err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	return newDaemonChild(cmd), nil
}

// daemonChild is a daemon process this call launched. One goroutine reaps
// it, so exec.Cmd.Wait runs exactly once whether the daemon becomes ready,
// exits early, or is terminated.
type daemonChild struct {
	cmd     *exec.Cmd
	started time.Time
	// busy is set once the daemon reports store work. From then on no start
	// deadline terminates it.
	busy bool
	// done is closed once the process is reaped. It is nil when the Start
	// seam launched no process, which leaves nothing to reap or terminate.
	done chan struct{}
	// err is Wait's result, valid once done is closed.
	err error
}

func newDaemonChild(cmd *exec.Cmd) *daemonChild {
	child := &daemonChild{cmd: cmd, started: time.Now()}
	if cmd.Process != nil {
		child.done = make(chan struct{})
		go func() {
			child.err = cmd.Wait()
			close(child.done)
		}()
	}
	return child
}

// noteStoreWork marks c busy when the instance says c is doing store work.
// A nil c is no child, and nothing to mark.
func (c *daemonChild) noteStoreWork(status Status, held bool) {
	if c != nil && held && status.PID != 0 && status.PID == c.pid() && status.Phase.storeWork() {
		c.busy = true
	}
}

func (c *daemonChild) pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *daemonChild) exited() bool {
	if c.done == nil {
		return false
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// terminate signals the daemon's process group, escalates to a hard kill if
// the graceful signal is ignored, and waits for the reaper.
func (c *daemonChild) terminate(gracePeriod time.Duration) {
	if c.done == nil {
		return
	}
	if gracePeriod <= 0 {
		gracePeriod = 2 * time.Second
	}
	_ = terminateSignal(c.cmd, graceful)
	select {
	case <-c.done:
		return
	case <-time.After(gracePeriod):
	}
	_ = terminateSignal(c.cmd, hard)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
}

func (a *Autostarter) defaults() (Autostarter, error) {
	if a == nil || a.SocketPath == "" {
		return Autostarter{}, errors.New("daemon socket path is required")
	}
	cfg := *a
	if len(cfg.Args) == 0 {
		cfg.Args = []string{"daemon", "--socket", cfg.SocketPath}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Dir(cfg.SocketPath)
	}
	if cfg.LockPath == "" {
		cfg.LockPath = startLockPath(cfg.SocketPath)
	}
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(filepath.Dir(cfg.SocketPath), "daemon.log")
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 5 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 10 * time.Minute
	}
	if cfg.checkingNotice <= 0 {
		cfg.checkingNotice = 2 * time.Second
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 25 * time.Millisecond
	}
	if cfg.Executable == nil {
		cfg.Executable = os.Executable
	}
	if cfg.Command == nil {
		cfg.Command = exec.Command
	}
	if cfg.Start == nil {
		cfg.Start = func(ctx context.Context, cmd *exec.Cmd) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return cmd.Start()
		}
	}
	if cfg.Ready == nil {
		cfg.Ready = probeSocket
	}
	if cfg.OpenNull == nil {
		cfg.OpenNull = func() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) }
	}
	if cfg.OpenLog == nil {
		logPath := cfg.LogPath
		cfg.OpenLog = func() (*os.File, error) {
			return os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}
	return cfg, nil
}

// startLockPath is the lock every command that starts a daemon for
// socketPath takes, and that StopDaemon holds while the old daemon exits.
// Older papio binaries use the same path.
func startLockPath(socketPath string) string { return socketPath + ".start.lock" }

func probeSocket(ctx context.Context, socketPath string) error {
	conn, err := ipc.Dial(ctx, socketPath)
	if err != nil {
		return err
	}
	return conn.Close()
}
