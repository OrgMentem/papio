// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package nativehost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"papio/internal/protocol"
)

// fakeSyncer stands in for the daemon's browser.sync RPC. onSync is invoked
// under a lock so the read path and idle-poll goroutine can call it safely.
type fakeSyncer struct {
	mu     sync.Mutex
	onSync func(messages []json.RawMessage) ([]json.RawMessage, error)
}

func (f *fakeSyncer) Sync(_ context.Context, messages []json.RawMessage) ([]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onSync == nil {
		return nil, nil
	}
	return f.onSync(messages)
}

func rawMsg(t *testing.T, typ, msgID, jobID string, seq int64, payload any) json.RawMessage {
	t.Helper()
	env := map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     typ,
		"msg_id":   msgID,
		"seq":      seq,
		"payload":  payload,
	}
	if jobID != "" {
		env["job_id"] = jobID
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal %s frame: %v", typ, err)
	}
	return data
}

func frameBytes(raw []byte) []byte {
	out := make([]byte, 4+len(raw))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(raw)))
	copy(out[4:], raw)
	return out
}

func readTestFrame(t *testing.T, r io.Reader) *protocol.BrowserMessage {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	n := binary.LittleEndian.Uint32(header[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	msg, err := protocol.DecodeBrowserMessage(body)
	if err != nil {
		t.Fatalf("decode outbound frame: %v", err)
	}
	return msg
}

func errorCode(t *testing.T, msg *protocol.BrowserMessage) string {
	t.Helper()
	if msg.Type != protocol.MsgError {
		t.Fatalf("outbound type = %q, want error", msg.Type)
	}
	p, ok := msg.Payload.(*protocol.ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *protocol.ErrorPayload", msg.Payload)
	}
	return p.Code
}

// TestHelloRoundTrip: a framed hello is forwarded and the daemon's hello_ack is
// written back framed correctly, then stdin EOF exits cleanly (covers case 6).
func TestHelloRoundTrip(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	fake := &fakeSyncer{onSync: func([]json.RawMessage) ([]json.RawMessage, error) {
		return []json.RawMessage{rawMsg(t, protocol.MsgHelloAck, "helloackid01", "", 0, map[string]any{})}, nil
	}}

	done := make(chan error, 1)
	go func() { done <- newBridge(fake, stdinR, stdoutW, io.Discard).run(context.Background()) }()

	hello := rawMsg(t, protocol.MsgHello, "helloid00001", "", 0, map[string]any{"extension_version": "1.0.0"})
	go func() { _, _ = stdinW.Write(frameBytes(hello)) }()

	got := readTestFrame(t, stdoutR)
	if got.Type != protocol.MsgHelloAck {
		t.Fatalf("outbound type = %q, want hello_ack", got.Type)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run returned %v, want clean exit on EOF", err)
	}
}

// TestOversizedFrameRejectedWithoutBody: a length prefix over the cap is
// rejected before any body byte is read; only the 4-byte header is supplied.
func TestOversizedFrameRejectedWithoutBody(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan error, 1)
	go func() { done <- newBridge(&fakeSyncer{}, stdinR, stdoutW, io.Discard).run(context.Background()) }()

	go func() {
		var header [4]byte
		binary.LittleEndian.PutUint32(header[:], protocol.MaxBrowserMessageBytes+1)
		_, _ = stdinW.Write(header[:]) // header only, no body
	}()

	if code := errorCode(t, readTestFrame(t, stdoutR)); code != "frame_too_large" {
		t.Fatalf("error code = %q, want frame_too_large", code)
	}
	if err := <-done; !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("run returned %v, want errFrameTooLarge", err)
	}
	_ = stdinW.Close()
}

// TestFirstFrameNotHello: the first frame must be hello; anything else is a
// protocol violation that emits an expected_hello error and exits non-zero.
func TestFirstFrameNotHello(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer func() { _ = stdinW.Close() }()

	done := make(chan error, 1)
	go func() { done <- newBridge(&fakeSyncer{}, stdinR, stdoutW, io.Discard).run(context.Background()) }()

	frame := rawMsg(t, protocol.MsgAck, "ackid000001", "", 0, map[string]any{})
	go func() { _, _ = stdinW.Write(frameBytes(frame)) }()

	if code := errorCode(t, readTestFrame(t, stdoutR)); code != "expected_hello" {
		t.Fatalf("error code = %q, want expected_hello", code)
	}
	if err := <-done; err == nil {
		t.Fatal("run returned nil, want non-nil for non-hello first frame")
	}
}

// TestSeqRegressionRejected: after hello, a frame whose seq does not strictly
// increase is rejected with seq_regression.
func TestSeqRegressionRejected(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer func() { _ = stdinW.Close() }()

	fake := &fakeSyncer{onSync: func([]json.RawMessage) ([]json.RawMessage, error) {
		return []json.RawMessage{rawMsg(t, protocol.MsgHelloAck, "helloackid02", "", 0, map[string]any{})}, nil
	}}
	done := make(chan error, 1)
	go func() { done <- newBridge(fake, stdinR, stdoutW, io.Discard).run(context.Background()) }()

	hello := rawMsg(t, protocol.MsgHello, "helloid00002", "", 0, map[string]any{"extension_version": "1.0.0"})
	go func() { _, _ = stdinW.Write(frameBytes(hello)) }()
	if got := readTestFrame(t, stdoutR); got.Type != protocol.MsgHelloAck {
		t.Fatalf("outbound type = %q, want hello_ack", got.Type)
	}

	// seq 0 again: not strictly greater than the hello's seq of 0.
	regress := rawMsg(t, protocol.MsgAck, "ackid000002", "", 0, map[string]any{})
	go func() { _, _ = stdinW.Write(frameBytes(regress)) }()
	if code := errorCode(t, readTestFrame(t, stdoutR)); code != "seq_regression" {
		t.Fatalf("error code = %q, want seq_regression", code)
	}
	if err := <-done; err == nil {
		t.Fatal("run returned nil, want non-nil for seq regression")
	}
}

// TestStdinEOFCleanExit: an immediate EOF (no frames) exits with nil.
func TestStdinEOFCleanExit(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	_ = stdinW.Close() // EOF before any frame.

	done := make(chan error, 1)
	go func() { done <- newBridge(&fakeSyncer{}, stdinR, io.Discard, io.Discard).run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil on EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit on stdin EOF")
	}
}

// TestValidateOrigin: only the exact configured extension origin is accepted.
func TestValidateOrigin(t *testing.T) {
	const id = "abcdefghijklmnopabcdefghijklmnop"
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"exact", []string{"chrome-extension://" + id + "/"}, false},
		{"no-trailing-slash", []string{"chrome-extension://" + id}, false},
		{"with-window-handle", []string{"chrome-extension://" + id + "/", "--parent-window=123"}, false},
		{"wrong-id", []string{"chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/"}, true},
		{"missing", []string{"--parent-window=123"}, true},
		{"empty", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOrigin(tc.args, id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOrigin(%v) err = %v, wantErr = %v", tc.args, err, tc.wantErr)
			}
		})
	}
}

// TestResolveExecutableThroughSymlink proves that when the process is launched
// via the installed papio-native-host symlink, the autostarter receives the
// resolved real (non-symlink, non-native-host) executable path so the spawned
// child starts the daemon instead of re-dispatching into native-host mode.
func TestResolveExecutableThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	realExe := filepath.Join(dir, "papio")
	if err := os.WriteFile(realExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write real exe: %v", err)
	}
	symlink := filepath.Join(dir, nativeHostBasename)
	if err := os.Symlink(realExe, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	want, err := filepath.EvalSymlinks(realExe)
	if err != nil {
		t.Fatalf("canonicalize real exe: %v", err)
	}

	got, err := resolveExecutablePath(symlink)
	if err != nil {
		t.Fatalf("resolveExecutablePath(symlink) error: %v", err)
	}
	if got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}
	if base := filepath.Base(got); base == nativeHostBasename {
		t.Fatalf("resolved basename = %q, must not dispatch as native host", base)
	}
	if canon, err := filepath.EvalSymlinks(got); err != nil || canon != got {
		t.Fatalf("resolved path is not a real non-symlink target: canon=%q err=%v", canon, err)
	}
}

// TestResolveExecutableRejectsNativeHostTarget: a resolved path whose basename
// is still papio-native-host is refused rather than looping autostart.
func TestResolveExecutableRejectsNativeHostTarget(t *testing.T) {
	dir := t.TempDir()
	hostExe := filepath.Join(dir, nativeHostBasename)
	if err := os.WriteFile(hostExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write host exe: %v", err)
	}
	if _, err := resolveExecutablePath(hostExe); err == nil {
		t.Fatal("resolveExecutablePath accepted a native-host basename, want error")
	}
}
