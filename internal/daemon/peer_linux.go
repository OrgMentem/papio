// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerPID reads the pid of the process at the other end of a Unix socket.
func peerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, sockErr
	}
	return int(cred.Pid), nil
}
