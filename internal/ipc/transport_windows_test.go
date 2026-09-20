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
		if err != nil || string(data) != "request" {
			done <- fmt.Errorf("read through request EOF: %q (%v)", data, err)
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
