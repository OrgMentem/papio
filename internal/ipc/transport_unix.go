// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package ipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// listenSocket prepares and listens on a Unix-domain socket at path with
// owner-only (0600) permissions. The returned cleanup removes only the socket
// this call created, so shutdown never deletes a replacement file.
func listenSocket(path string) (net.Listener, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create ipc directory: %w", err)
	}
	if err := prepareSocket(path); err != nil {
		return nil, nil, err
	}
	// Bind inside a fresh owner-only directory and restrict the socket there,
	// then link it into place: the published path never exists with the
	// process umask's permissions, so no other local user can connect in the
	// window between bind and chmod, whatever the parent directory allows.
	staging, err := makeStagingDir(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	staged := filepath.Join(staging, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: staged, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("listen ipc socket: %w", err)
	}
	// net.UnixListener otherwise unlinks its path on Close, which could remove
	// a replacement file created after the daemon started.
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(staged, 0o600); err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("restrict ipc socket permissions: %w", err)
	}
	// link, unlike rename, refuses an existing path, so a daemon that won the
	// race since prepareSocket keeps its socket.
	if err := os.Link(staged, path); err != nil {
		_ = listener.Close()
		if errors.Is(err, os.ErrExist) {
			return nil, nil, ErrSocketInUse
		}
		return nil, nil, fmt.Errorf("publish ipc socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("inspect created ipc socket: %w", err)
	}
	return listener, func() error { return removeSocketIfSame(path, info) }, nil
}

// makeStagingDir creates an owner-only directory beside the socket path. Its
// name is deliberately tiny: the staged socket "<dir>/.sXXXX/s" must fit
// sun_path (104 bytes on macOS) wherever the published "<dir>/papio.sock" does,
// and MkdirTemp's ten-digit suffix would make it longer.
func makeStagingDir(dir string) (string, error) {
	var suffix [2]byte
	for range 16 {
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("create ipc staging directory: %w", err)
		}
		staging := filepath.Join(dir, ".s"+hex.EncodeToString(suffix[:]))
		err := os.Mkdir(staging, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create ipc staging directory: %w", err)
		}
		// Mkdir honours the umask, which can only remove bits; chmod pins 0700.
		if err := os.Chmod(staging, 0o700); err != nil {
			_ = os.Remove(staging)
			return "", fmt.Errorf("restrict ipc staging directory: %w", err)
		}
		return staging, nil
	}
	return "", fmt.Errorf("create ipc staging directory: no free name in %s", dir)
}

// dialSocket connects to the daemon's Unix-domain socket.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

func prepareSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ipc socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ErrUnsafeSocketPath
	}

	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return ErrSocketInUse
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) && (errors.Is(netErr.Err, syscall.ECONNREFUSED) || errors.Is(netErr.Err, syscall.ENOENT)) {
		if err := removeSocketIfSame(path, info); err != nil {
			return fmt.Errorf("remove stale ipc socket: %w", err)
		}
		return nil
	}
	return fmt.Errorf("probe existing ipc socket: %w", err)
}

// removeSocketIfSame removes path only if it is still the socket identified by
// expected. It prevents shutdown cleanup from deleting a replacement file.
func removeSocketIfSame(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(expected, current) {
		return nil
	}
	return os.Remove(path)
}
