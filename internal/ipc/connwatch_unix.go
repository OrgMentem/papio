// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package ipc

import (
	"context"
	"errors"
	"math"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// watchConnClose cancels cancel once the peer has fully closed conn. A
// half-close (the historical framing EOF) or trailing bytes are not a
// disconnect: only POLLHUP/POLLERR fires. Linux also reports POLLRDHUP for
// a half-close; it is deliberately not watched. The watch polls a dup'd
// descriptor so the blocking poll never disturbs the runtime poller, and it
// ends with the request context (serveConn and shutdown both cancel it,
// bounding the linger to one poll interval).
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
		for ctx.Err() == nil {
			var fds [1]unix.PollFd
			fds[0].Fd = fd
			fds[0].Events = unix.POLLERR | unix.POLLHUP
			n, err := unix.Poll(fds[:], 100)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				continue // timeout: re-check the context
			}
			if fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
				cancel()
				return
			}
			// Readable without hangup (framing FIN, trailing bytes after
			// dispatch): the peer is still there. Keep watching.
		}
	}()
}
