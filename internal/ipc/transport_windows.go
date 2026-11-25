// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// ErrUntrustedPipeServer means the named pipe answered but was not created by
// the current user, or its owner could not be read. The pipe namespace is
// shared by every user on the machine, so another account can create the
// predictable pipe name first; a client that trusted it would hand that
// account its requests, and the daemon could never listen there.
var ErrUntrustedPipeServer = errors.New("ipc named pipe is not owned by the current user")

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
// dial before listening means another daemon already owns the endpoint, unless
// that pipe belongs to another user: then it is a squatter and the error says
// so. ListenPipe itself also fails closed if the pipe already exists. Message
// mode is required: the local RPC framing relies on CloseWrite, which go-winio
// only implements for message-mode pipes (a byte-mode pipe has no CloseWrite
// and the client rejects it). Named pipes vanish with the owning process, so
// there is no stale artifact to clean up on shutdown.
func listenSocket(path string) (net.Listener, func() error, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	return listenSocketAs(path, user)
}

// listenSocketAs is listenSocket for an explicit current user, so tests can
// stand in a different identity.
func listenSocketAs(path string, user *windows.SID) (net.Listener, func() error, error) {
	name := pipeName(path)

	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	conn, err := dialPipeAs(probeCtx, name, user)
	cancel()
	if err == nil {
		_ = conn.Close()
		return nil, nil, ErrSocketInUse
	}
	if occupied := occupiedPipeError(name, err); occupied != nil {
		return nil, nil, occupied
	}

	listener, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: ownerOnlySDDL(user),
		MessageMode:        true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listen ipc pipe: %w", err)
	}
	return listener, func() error { return nil }, nil
}

// occupiedPipeError classifies a failed probe dial. It returns nil when no
// pipe answered, so the daemon may listen. Otherwise a pipe exists and the
// daemon cannot be its owner:
//   - the owner is another user, or could not be read: ErrUntrustedPipeServer;
//   - access was denied: the daemon's own pipe grants its user full access, so
//     the pipe is another account's: ErrUntrustedPipeServer, owner unknown;
//   - every instance stayed busy for the whole probe: something serves the pipe
//     but cannot be identified: ErrSocketInUse, owner unknown.
func occupiedPipeError(name string, err error) error {
	switch {
	case errors.Is(err, ErrUntrustedPipeServer):
		return fmt.Errorf("ipc pipe %s is already taken: %w", name, err)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return fmt.Errorf("ipc pipe %s is occupied and its owner is unknown (access denied): %w: %w", name, ErrUntrustedPipeServer, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("ipc pipe %s is occupied and its owner is unknown (busy): %w", name, ErrSocketInUse)
	}
	return nil
}

// dialSocket connects to the daemon's named-pipe endpoint and refuses a pipe
// that the current user did not create, before any request byte is written.
// The returned connection adopts message mode from the pipe's flags, so it
// supports the CloseWrite the local RPC framing depends on.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return dialPipeAs(ctx, pipeName(path), user)
}

// dialPipeAs dials the named pipe and closes it again unless its owner is user.
func dialPipeAs(ctx context.Context, name string, user *windows.SID) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeOwner(conn, user); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// verifyPipeOwner fails closed unless the pipe behind conn is owned by user.
// The owner is the one property of a pipe that its creator cannot choose
// freely: CreateNamedPipe assigns only the creator's own user SID (or a group
// flagged as an owner) and refuses any other account's SID without
// SeRestorePrivilege. listenSocket pins the owner to the daemon's user SID
// explicitly, because an elevated process otherwise defaults to the
// Administrators group. The check reads the owner from the handle, so it needs
// no access to the server process: an elevated daemon's token is closed to a
// non-elevated client of the same user, its pipe is not.
func verifyPipeOwner(conn net.Conn, user *windows.SID) error {
	owner, err := pipeOwner(conn)
	if err != nil {
		return fmt.Errorf("%w: read pipe owner: %w", ErrUntrustedPipeServer, err)
	}
	if !owner.Equals(user) {
		return fmt.Errorf("%w: pipe is owned by %s, not %s", ErrUntrustedPipeServer, owner, user)
	}
	return nil
}

// pipeOwner reads the owner SID of the named pipe behind conn.
func pipeOwner(conn net.Conn) (*windows.SID, error) {
	pipe, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return nil, errors.New("connection has no native handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(pipe.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("pipe has no owner")
	}
	return owner, nil
}

// currentUserSID returns the user SID of the process token.
func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current user SID: %w", err)
	}
	return user.User.Sid, nil
}

// ownerOnlySDDL builds a security descriptor that makes user the pipe's owner
// and grants user full access in a protected DACL, no one else, mirroring the
// Unix socket's 0600 permissions. go-winio's default named-pipe ACL is broader
// (it also grants other principals), so the owner-only guarantee must be set
// explicitly. The explicit owner is what clients verify (see verifyPipeOwner).
func ownerOnlySDDL(user *windows.SID) string {
	sid := user.String()
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")"
}
