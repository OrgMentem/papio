// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package ipc

import (
	"context"
	"errors"
	"math"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// connWatchInterval is how often watchConnClose checks the peer, and so how
// long a disconnect can go unnoticed.
const connWatchInterval = 100 * time.Millisecond

// watchConnClose cancels cancel once the peer has fully closed conn. A
// half-close (the historical framing EOF) or trailing bytes are not a
// disconnect. The check asks poll(2) for POLLOUT and reads POLLHUP/POLLERR
// from the result. It must not watch the read side: macOS reports a drained
// half-close there as POLLHUP (it maps a requested POLLHUP to its read
// filter), which cancelled every request as soon as its frame was read. On
// the write side macOS and Linux both report POLLHUP only once the peer is
// gone. A connected socket answers POLLOUT at once, so the watch polls
// without blocking, once per connWatchInterval, on a dup'd descriptor so it
// never disturbs the runtime poller. It ends with the request context
// (serveConn and shutdown both cancel it).
func watchConnClose(ctx context.Context, conn net.Conn, cancel context.CancelFunc) {
	sc, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return
	}
	watch := -1
	if err := raw.Control(func(fd uintptr) {
		dup, derr := syscall.Dup(int(fd))
		if derr != nil {
			return
		}
		syscall.CloseOnExec(dup)
		watch = dup
	}); err != nil || watch < 0 {
		return
	}
	if watch > math.MaxInt32 {
		// Cannot happen on a real daemon (fds are small), but the int32
		// conversion below must stay in range.
		syscall.Close(watch)
		return
	}
	go func() {
		defer syscall.Close(watch)
		if watch < 0 || watch > math.MaxInt32 {
			return
		}
		fd := int32(watch)
		ticker := time.NewTicker(connWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			fds := [1]unix.PollFd{{Fd: fd, Events: unix.POLLOUT}}
			if _, err := unix.Poll(fds[:], 0); err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue // check again on the next tick
				}
				return
			}
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
				cancel()
				return
			}
		}
	}()
}
