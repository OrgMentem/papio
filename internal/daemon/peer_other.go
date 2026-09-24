// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !darwin && !linux && !windows

package daemon

import (
	"errors"
	"net"
)

// peerPID is unavailable here, so StopDaemon only asks the daemon to stop.
func peerPID(net.Conn) (int, error) {
	return 0, errors.New("socket peer pid is not supported on this platform")
}
