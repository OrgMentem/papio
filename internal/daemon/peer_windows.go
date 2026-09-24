// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"errors"
	"net"

	"golang.org/x/sys/windows"
)

// peerPID reads the pid of the process serving a named pipe.
func peerPID(conn net.Conn) (int, error) {
	pipe, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return 0, errors.New("not a named pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(pipe.Fd()), &pid); err != nil {
		return 0, err
	}
	return int(pid), nil
}
