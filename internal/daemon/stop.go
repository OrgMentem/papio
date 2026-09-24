// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"papio/internal/ipc"
)

// How long StopDaemon waits for the start lock, for the stopped process to
// exit, and before it says that it is waiting. Graceful shutdown drains the
// scheduler and can take 15-30 s.
var (
	stopLockWait    = 5 * time.Second
	stopExitWait    = 2 * time.Minute
	stopNoticeAfter = 500 * time.Millisecond
	stopPoll        = 25 * time.Millisecond
)

// StopDaemon asks the daemon at socketPath to shut down through shutdown, and
// returns once that daemon's process has exited.
//
// A daemon closes its socket as soon as it is asked to stop and then finishes
// its work in the database. A daemon that holds the instance claim keeps
// commands from starting a second daemon until it exits, but a daemon built
// before the claim existed does not, so the first command after an upgrade
// would start the new daemon, and its migration, beside the old one. StopDaemon
// therefore learns the serving process from the socket's peer credentials,
// holds the start lock that every autostart takes, older binaries included,
// and releases it only once that process is gone. onWait is told the pid when
// the exit takes long enough for a person to notice. Where the platform cannot
// name a socket's peer, StopDaemon only asks the daemon to stop.
func StopDaemon(ctx context.Context, socketPath string, shutdown func(context.Context) error, onWait func(pid int)) error {
	pid := daemonPID(ctx, socketPath)
	if unlock := holdStartLock(ctx, startLockPath(socketPath)); unlock != nil {
		defer unlock()
	}
	if err := shutdown(ctx); err != nil {
		return err
	}
	if pid <= 0 || pid == os.Getpid() {
		return nil
	}
	start := time.Now()
	announced := false
	for !processExited(pid) {
		waited := time.Since(start)
		if !announced && waited >= stopNoticeAfter {
			announced = true
			if onWait != nil {
				onWait(pid)
			}
		}
		if waited >= stopExitWait {
			return fmt.Errorf("the daemon (pid %d) was asked to stop but is still running after %s", pid, stopExitWait)
		}
		timer := time.NewTimer(stopPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// daemonPID names the process serving socketPath, or 0 when it cannot.
func daemonPID(ctx context.Context, socketPath string) int {
	conn, err := ipc.Dial(ctx, socketPath)
	if err != nil {
		return 0
	}
	defer conn.Close()
	pid, err := peerPID(conn)
	if err != nil {
		return 0
	}
	return pid
}

// holdStartLock takes the start lock for up to stopLockWait. It returns nil
// when the lock stays busy: an autostart holds it only while a daemon starts,
// and stopping should not fail on that.
func holdStartLock(ctx context.Context, path string) func() {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	deadline := time.Now().Add(stopLockWait)
	for {
		if locked, err := tryLockFile(file); err != nil {
			break
		} else if locked {
			return func() {
				_ = unlockFile(file)
				_ = file.Close()
			}
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(stopPoll)
	}
	_ = file.Close()
	return nil
}
