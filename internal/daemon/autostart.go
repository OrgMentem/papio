// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package daemon contains process-lifetime services used by the papio daemon.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// CommandFactory constructs the daemon command. The default uses exec.Command
// with the current executable; it never invokes a shell.
type CommandFactory func(name string, args ...string) *exec.Cmd

// Autostarter starts the daemon once when its local socket is unavailable.
// Its seams make command wiring unit-testable without launching a daemon.
type Autostarter struct {
	SocketPath string
	Args       []string
	LockPath   string
	LogPath    string

	StartTimeout  time.Duration
	RetryInterval time.Duration

	Executable func() (string, error)
	Command    CommandFactory
	Start      func(context.Context, *exec.Cmd) error
	Ready      func(context.Context, string) error
	OpenNull   func() (*os.File, error)
	OpenLog    func() (*os.File, error)
}

// NewAutostarter returns an autostarter with production-safe defaults.
func NewAutostarter(socketPath string) *Autostarter {
	return &Autostarter{SocketPath: socketPath}
}

// EnsureResult describes how EnsureWithResult made the daemon available.
type EnsureResult struct {
	// Started reports whether this call launched the daemon process.
	Started bool
}

// Ensure ensures a daemon is ready. Callers that need to know whether this
// invocation launched it should use EnsureWithResult.
func (a *Autostarter) Ensure(ctx context.Context) error {
	_, err := a.EnsureWithResult(ctx)
	return err
}

// EnsureWithResult returns once another daemon is ready or a single daemon
// command has been started and its socket becomes ready. Contending callers
// share an advisory lock and always check readiness both before and after
// acquiring it.
func (a *Autostarter) EnsureWithResult(ctx context.Context) (EnsureResult, error) {
	result := EnsureResult{}
	cfg, err := a.defaults()
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := cfg.Ready(ctx, cfg.SocketPath); err == nil {
		return result, nil
	} else if ctx.Err() != nil {
		return result, ctx.Err()
	}
	unlock, err := acquireLock(ctx, cfg.LockPath, cfg.RetryInterval)
	if err != nil {
		return result, err
	}
	defer unlock()

	if err := cfg.Ready(ctx, cfg.SocketPath); err == nil {
		return result, nil
	} else if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	executable, err := cfg.Executable()
	if err != nil {
		return result, fmt.Errorf("locate papio executable: %w", err)
	}
	cmd := cfg.Command(executable, cfg.Args...)
	if cmd == nil {
		return result, errors.New("daemon command factory returned nil")
	}
	null, err := cfg.OpenNull()
	if err != nil {
		return result, fmt.Errorf("open detached daemon stdio: %w", err)
	}
	logFile, err := cfg.OpenLog()
	if err != nil {
		_ = null.Close()
		return result, fmt.Errorf("open detached daemon log: %w", err)
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
		return result, err
	}
	err = cfg.Start(ctx, cmd)
	_ = null.Close()
	_ = logFile.Close()
	if err != nil {
		return result, fmt.Errorf("start daemon: %w", err)
	}
	result.Started = true
	if cmd.Process != nil {
		go func() { _ = cmd.Wait() }()
	}

	readyCtx, cancel := context.WithTimeout(ctx, cfg.StartTimeout)
	defer cancel()
	if err := waitReady(readyCtx, cfg.Ready, cfg.SocketPath, cfg.RetryInterval); err != nil {
		return result, fmt.Errorf("wait for daemon socket: %w", err)
	}
	return result, nil
}

func (a *Autostarter) defaults() (Autostarter, error) {
	if a == nil || a.SocketPath == "" {
		return Autostarter{}, errors.New("daemon socket path is required")
	}
	cfg := *a
	if len(cfg.Args) == 0 {
		cfg.Args = []string{"daemon", "--socket", cfg.SocketPath}
	}
	if cfg.LockPath == "" {
		cfg.LockPath = cfg.SocketPath + ".start.lock"
	}
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(filepath.Dir(cfg.SocketPath), "daemon.log")
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 5 * time.Second
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

func acquireLock(ctx context.Context, path string, retry time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create autostart lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open autostart lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock autostart: %w", err)
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func probeSocket(ctx context.Context, socketPath string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	return conn.Close()
}

func waitReady(ctx context.Context, ready func(context.Context, string) error, socketPath string, retry time.Duration) error {
	for {
		if err := ready(ctx, socketPath); err == nil {
			return nil
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
