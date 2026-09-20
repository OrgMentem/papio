// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package ipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Windows endpoints are named kernel objects; transport_windows_test.go checks
// their protected DACL and that the path used to name them is never modified.
func TestSocketPermissionsAndRegularFileSafety(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "s")
	server := &Server{SocketPath: socket, Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil })}
	listener, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = server.cleanup()
	})
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("socket mode = %o, want 0600", got)
	}
	unsafe := filepath.Join(dir, "regular")
	if err := os.WriteFile(unsafe, []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	unsafeServer := &Server{SocketPath: unsafe, Handler: server.Handler}
	if _, err := unsafeServer.Listen(); !errors.Is(err, ErrUnsafeSocketPath) {
		t.Fatalf("Listen regular file error = %v, want ErrUnsafeSocketPath", err)
	}
	contents, err := os.ReadFile(unsafe)
	if err != nil || string(contents) != "do not remove" {
		t.Fatalf("regular file changed: %q, %v", contents, err)
	}
}
