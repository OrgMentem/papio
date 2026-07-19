// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeName maps a filesystem socket path to a stable local named-pipe name.
// Windows has no filesystem sockets, so the daemon and its clients each derive
// the same pipe name from the configured socket path; distinct data directories
// therefore stay on distinct pipes. The hash keeps the name short and free of
// characters the pipe namespace forbids, and lower-casing matches Windows'
// case-insensitive paths so both processes agree.
func pipeName(path string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	return `\\.\pipe\papio-` + hex.EncodeToString(sum[:8])
}

// listenSocket opens the daemon's named-pipe endpoint restricted to the current
// user, the Windows analog of the owner-only (0600) Unix socket. A successful
// dial before listening means another daemon already owns the endpoint;
// ListenPipe itself also fails closed if the pipe already exists. Message mode
// is required: the local RPC framing relies on CloseWrite, which go-winio only
// implements for message-mode pipes (a byte-mode pipe has no CloseWrite and the
// client rejects it). Named pipes vanish with the owning process, so there is
// no stale artifact to clean up on shutdown.
func listenSocket(path string) (net.Listener, func() error, error) {
	name := pipeName(path)

	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	conn, err := winio.DialPipeContext(probeCtx, name)
	cancel()
	if err == nil {
		_ = conn.Close()
		return nil, nil, ErrSocketInUse
	}

	sddl, err := ownerOnlySDDL()
	if err != nil {
		return nil, nil, err
	}
	listener, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listen ipc pipe: %w", err)
	}
	return listener, func() error { return nil }, nil
}

// dialSocket connects to the daemon's named-pipe endpoint. The returned
// connection adopts message mode from the pipe's flags, so it supports the
// CloseWrite the local RPC framing depends on.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipeName(path))
}

// ownerOnlySDDL builds a protected DACL granting the current user full access
// and no one else, mirroring the Unix socket's 0600 permissions. go-winio's
// default named-pipe ACL is broader (it also grants other principals), so the
// owner-only guarantee must be set explicitly.
func ownerOnlySDDL() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolve current user SID: %w", err)
	}
	return "D:P(A;;GA;;;" + user.User.Sid.String() + ")", nil
}
