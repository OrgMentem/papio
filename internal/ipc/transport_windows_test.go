// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPipeEndpointSecurityAndCloseWrite(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "regular-file")
	if err := os.WriteFile(path, []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener := listenTestEndpoint(t, path)
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		accepted <- acceptResult{conn: conn, err: err}
	}()
	client := dialTestEndpoint(t, path)
	var server net.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		server = result.conn
	case <-time.After(time.Second):
		t.Fatal("named-pipe server did not accept the client")
	}
	defer server.Close()
	// Read the kernel object's descriptor, not merely ownerOnlySDDL's string.
	fd, ok := server.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("named-pipe connection has no native handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("read named-pipe security descriptor: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("named-pipe DACL is not protected: %s (%v)", sd.String(), err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("named-pipe DACL must contain only the current user: %s (%v)", sd.String(), err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	wantAccess := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.WRITE_DAC)
	fullAccess := ace.Mask&windows.GENERIC_ALL != 0 || ace.Mask&wantAccess == wantAccess
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
		!sid.Equals(user.User.Sid) || !fullAccess {
		t.Fatalf("named-pipe DACL is not an explicit user-only read/write/control grant: %s", sd.String())
	}
	// An explicit owner: an elevated daemon would otherwise own its pipe as the
	// Administrators group, and clients verify the owner is the user.
	owner, err := pipeOwner(server)
	if err != nil || !owner.Equals(user.User.Sid) {
		t.Fatalf("named-pipe owner = %v (%v), want the current user %s", owner, err, user.User.Sid)
	}

	clientWriter, ok := client.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("named-pipe client does not support CloseWrite")
	}
	serverWriter, ok := server.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("named-pipe server does not support CloseWrite")
	}
	deadline := time.Now().Add(2 * time.Second)
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(server)
		if err != nil {
			done <- fmt.Errorf("read through request EOF: %w", err)
			return
		}
		if string(data) != "request" {
			done <- fmt.Errorf("read through request EOF: %q", data)
			return
		}
		if _, err := server.Write([]byte("response")); err != nil {
			done <- err
			return
		}
		done <- serverWriter.CloseWrite()
	}()
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := clientWriter.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	// Half-closing the request must preserve the response direction.
	response, err := io.ReadAll(client)
	if err != nil || string(response) != "response" {
		t.Fatalf("read response after CloseWrite: %q (%v)", response, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server CloseWrite did not complete")
	}
	_ = client.Close()
	_ = server.Close()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "do not remove" {
		t.Fatalf("named-pipe lifecycle modified the naming path: %q (%v)", contents, err)
	}
}

func TestWindowsPipeRejectsOccupiedEndpoint(t *testing.T) {
	path, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{"original":true}`), nil
	}))
	listener, cleanup, err := listenSocket(path)
	if listener != nil {
		_ = listener.Close()
	}
	if cleanup != nil {
		_ = cleanup()
	}
	if !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("occupied endpoint = %v, want ErrSocketInUse", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := NewSocketClient(path).CallRaw(ctx, "original_01", "jobs.get", json.RawMessage(`{}`))
	if err != nil || string(got) != `{"original":true}` {
		t.Fatalf("occupancy probe displaced original server: %s (%v)", got, err)
	}
}

// foreignSID is a well-formed SID that no test runner's token carries.
func foreignSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := windows.StringToSid("S-1-5-21-1-2-3-1000")
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// TestWindowsClientRefusesPipeOwnedByAnotherUser stands a different identity in
// for the current user, which makes the daemon's own pipe look like one that
// another account created first. The client must refuse it before it writes a
// request, so the squatter never sees the request.
func TestWindowsClientRefusesPipeOwnedByAnotherUser(t *testing.T) {
	var handled atomic.Int32
	path, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		handled.Add(1)
		return []byte(`{}`), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialPipeAs(ctx, pipeName(path), user)
	if err != nil {
		t.Fatalf("dial a pipe owned by the current user: %v", err)
	}
	_ = conn.Close()

	squatted := &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return dialPipeAs(ctx, pipeName(path), foreignSID(t))
	}}
	_, err = squatted.CallRaw(ctx, "request_01", "jobs.get", json.RawMessage(`{}`))
	if !errors.Is(err, ErrUntrustedPipeServer) || !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("CallRaw on a pipe owned by another user = %v, want ErrUntrustedPipeServer and ErrDaemonUnreachable", err)
	}
	if n := handled.Load(); n != 0 {
		t.Fatalf("the squatted pipe received %d request(s); the client must refuse it before writing", n)
	}

	// The genuine client still reaches the daemon.
	got, err := NewSocketClient(path).CallRaw(ctx, "request_02", "jobs.get", json.RawMessage(`{}`))
	if err != nil || string(got) != `{}` {
		t.Fatalf("CallRaw on the daemon's own pipe: %s (%v)", got, err)
	}
	if n := handled.Load(); n != 1 {
		t.Fatalf("daemon handled %d requests, want 1", n)
	}
}

// TestWindowsListenReportsSquattedEndpointDistinctly checks that a live pipe
// owned by another account is reported as hostile, not as a running daemon.
func TestWindowsListenReportsSquattedEndpointDistinctly(t *testing.T) {
	path, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{}`), nil
	}))
	listener, cleanup, err := listenSocketAs(path, foreignSID(t))
	if listener != nil {
		_ = listener.Close()
	}
	if cleanup != nil {
		_ = cleanup()
	}
	if !errors.Is(err, ErrUntrustedPipeServer) || errors.Is(err, ErrSocketInUse) {
		t.Fatalf("squatted endpoint = %v, want ErrUntrustedPipeServer and not ErrSocketInUse", err)
	}
}

// TestWindowsOccupiedPipeErrorClassifiesProbeFailures covers the probe
// failures that mean a pipe exists whose owner could not be read.
func TestWindowsOccupiedPipeErrorClassifiesProbeFailures(t *testing.T) {
	const name = `\\.\pipe\papio-test`
	denied := &os.PathError{Op: "open", Path: name, Err: windows.ERROR_ACCESS_DENIED}
	missing := &os.PathError{Op: "open", Path: name, Err: windows.ERROR_FILE_NOT_FOUND}
	cases := []struct {
		name      string
		err       error
		want      error // nil: the daemon may listen
		wantExtra error
	}{
		{name: "access denied", err: denied, want: ErrUntrustedPipeServer, wantExtra: windows.ERROR_ACCESS_DENIED},
		{name: "every instance busy", err: context.DeadlineExceeded, want: ErrSocketInUse},
		{name: "owner mismatch", err: fmt.Errorf("%w: pipe is owned by S-1-5-18", ErrUntrustedPipeServer), want: ErrUntrustedPipeServer},
		{name: "no pipe", err: missing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := occupiedPipeError(name, tc.err)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("occupiedPipeError = %v, want nil so the daemon listens", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("occupiedPipeError = %v, want %v", got, tc.want)
			}
			if tc.wantExtra != nil && !errors.Is(got, tc.wantExtra) {
				t.Fatalf("occupiedPipeError = %v, want it to wrap %v", got, tc.wantExtra)
			}
			if tc.want == ErrSocketInUse && errors.Is(got, ErrUntrustedPipeServer) {
				t.Fatalf("occupiedPipeError = %v, a busy pipe is not known to be hostile", got)
			}
		})
	}
}
