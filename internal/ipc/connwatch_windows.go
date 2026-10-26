// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package ipc

import (
	"context"
	"net"
)

// watchConnClose is a no-op on Windows: named pipes offer no passive
// full-close signal distinct from the framing half-close, so handlers keep
// the server context there, as on main.
func watchConnClose(context.Context, net.Conn, context.CancelFunc) {}
