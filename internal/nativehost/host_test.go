// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package nativehost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/ipc"
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

// failWriter fails every write, standing in for a broken stdout pipe whose peer
// is gone while stdin remains open.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout gone") }

// TestPollWriteFailureTerminatesRun: a failed idle-poll write must tear the
// whole bridge down, not silently stop polling while the process stays alive.
// Otherwise the native host lingers as an inert connection and the extension
// receives no further offers or cancels. Regression for the stranded-host bug.
func TestPollWriteFailureTerminatesRun(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }() // stdin stays open: no EOF exit path.

	// Every idle poll (nil inbound) returns a daemon-initiated frame to write.
	fake := &fakeSyncer{onSync: func(messages []json.RawMessage) ([]json.RawMessage, error) {
		return []json.RawMessage{rawMsg(t, protocol.MsgCancel, "cancelid0001", "job_1", 0, map[string]any{})}, nil
	}}

	b := newBridge(fake, stdinR, failWriter{}, io.Discard)
	b.pollInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- b.run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil, want the poll write error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after a poll write failure")
	}
}

// TestValidateOrigin accepts only exact configured Chrome origins or Firefox IDs.
func TestValidateOrigin(t *testing.T) {
	const chromeID = "abcdefghijklmnopabcdefghijklmnop"
	const firefoxID = "papio@orgmentem.com"
	cases := []struct {
		name                string
		args                []string
		chromeIDs           []string
		configuredFirefoxID string
		wantErr             bool
	}{
		{"chrome exact", []string{"chrome-extension://" + chromeID + "/"}, nil, firefoxID, false},
		{"chrome with window handle", []string{"chrome-extension://" + chromeID + "/", "--parent-window=123"}, nil, firefoxID, false},
		{"chrome no trailing slash", []string{"chrome-extension://" + chromeID}, nil, firefoxID, true},
		{"wrong chrome ID", []string{"chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/"}, nil, firefoxID, true},
		{"second configured chrome ID", []string{"chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/"}, []string{chromeID, "ponmlkjihgfedcbaponmlkjihgfedcba"}, firefoxID, false},
		{"firefox exact", []string{"/path/to/com.orgmentem.papio.json", firefoxID}, nil, firefoxID, false},
		{"firefox configured empty", []string{"/path/to/com.orgmentem.papio.json", firefoxID}, nil, "", true},
		{"wrong Firefox ID", []string{"/path/to/com.orgmentem.papio.json", "other@orgmentem.org"}, nil, firefoxID, true},
		{"manifest path alone", []string{"/path/to/com.orgmentem.papio.json"}, nil, firefoxID, true},
		{"missing", []string{"--parent-window=123"}, nil, firefoxID, true},
		{"empty", nil, nil, firefoxID, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chromeIDs := tc.chromeIDs
			if chromeIDs == nil {
				chromeIDs = []string{chromeID}
			}
			err := validateOrigin(tc.args, chromeIDs, tc.configuredFirefoxID)
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

// The IPC envelope must carry a stable per-process session identity so the
// daemon can arbitrate between concurrently connected browsers, and the
// goodbye flag so a cleanly departing browser releases its session at once.
func TestSyncRequestCarriesSessionIdentityAndGoodbye(t *testing.T) {
	id := newSessionID()
	if len(id) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", id)
	}
	if other := newSessionID(); other == id {
		t.Fatal("session ids must be unique per process start")
	}
	syncer := &ipcSyncer{sessionID: id}

	normal := syncer.request(false, nil)
	if normal.SessionID != id || normal.Goodbye || normal.Messages == nil || len(normal.Messages) != 0 {
		t.Fatalf("normal request = %+v", normal)
	}
	encoded, err := json.Marshal(normal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"session_id":"`+id+`"`) || strings.Contains(string(encoded), `"goodbye"`) {
		t.Fatalf("normal request JSON = %s", encoded)
	}

	goodbye := syncer.request(true, nil)
	if !goodbye.Goodbye || goodbye.SessionID != id {
		t.Fatalf("goodbye request = %+v", goodbye)
	}
}

// TestSyncRequestFitsMaxBrowserFrame pins the cross-layer size invariant: a
// legal max-size browser frame, relayed as the single message of one
// browser.sync request, must fit the IPC request cap. When it does not, the
// host's Sync call fails ErrTooLarge, the host exits (fatal transport error),
// and the goodbye tears down the whole browser session — every large
// page_capture killed its session this way before the cap was raised.
func TestSyncRequestFitsMaxBrowserFrame(t *testing.T) {
	frame := json.RawMessage(append(
		[]byte(`{"protocol":"papio-browser/1","type":"page_capture","msg_id":"m_frame_fit_1","seq":1,"payload":`),
		append(make([]byte, 0, protocol.MaxBrowserMessageBytes), fmt.Sprintf(
			`{"host":"example.org","scenario":"observed","encoding":"gzip+base64","bytes":1,"body":%q}}`,
			strings.Repeat("A", protocol.MaxBrowserMessageBytes-256),
		)...)...,
	))
	if len(frame) > protocol.MaxBrowserMessageBytes {
		t.Fatalf("test frame %d bytes exceeds the browser cap %d", len(frame), protocol.MaxBrowserMessageBytes)
	}
	syncer := &ipcSyncer{sessionID: newSessionID()}
	params, err := json.Marshal(syncer.request(false, []json.RawMessage{frame}))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"protocol": ipc.ProtocolVersion,
		"id":       "request_frame_fit",
		"method":   "browser.sync",
		"params":   json.RawMessage(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope) > ipc.MaxRequestBytes {
		t.Fatalf("sync request %d bytes exceeds ipc.MaxRequestBytes %d; a max-size browser frame must transit the IPC", len(envelope), ipc.MaxRequestBytes)
	}
	if _, err := ipc.DecodeRequest(bytes.NewReader(envelope)); err != nil {
		t.Fatalf("DecodeRequest rejected a max-size browser sync request: %v", err)
	}
}

// TestRunReportsVersion pins the probe `papio doctor`'s host-skew check depends
// on. The host is entered by basename, so a bare `papio-native-host --version`
// reaches Run as what would otherwise be parsed as an extension origin; it must
// answer with its own version instead of rejecting the argument.
func TestRunReportsVersion(t *testing.T) {
	for _, flag := range []string{"--version", "-v"} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), []string{flag}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("Run(%s) = %v, want nil", flag, err)
		}
		if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "papio ") {
			t.Fatalf("Run(%s) stdout = %q, want a \"papio <version>\" line", flag, got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("Run(%s) stderr = %q, want empty", flag, stderr.String())
		}
	}
}

// The diagnostic log is the only place a host's dying words survive: browsers
// forward native-messaging stderr nowhere, so a host that rejects a frame and
// tears the session down would otherwise leave nothing behind. Every browser
// launch is a fresh process writing the same file, so appending — not
// truncating per start — is what keeps the previous session's failure readable
// after the browser has respawned the host.
func TestDiagLogAppendsAcrossProcessesAndBoundsItself(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, diagLogName)

	for _, line := range []string{"first session died\n", "second session died\n"} {
		log, err := openDiagLog(dir)
		if err != nil {
			t.Fatalf("openDiagLog: %v", err)
		}
		if _, err := io.WriteString(log, line); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := log.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if want := "first session died\nsecond session died\n"; string(contents) != want {
		t.Fatalf("log = %q, want %q", contents, want)
	}

	// Unbounded appending would grow forever across a browser's lifetime, so a
	// log past the cap rotates at the next start. Rotation rather than
	// truncation is what keeps a concurrent sibling host's in-progress trace:
	// its descriptor follows the inode under the rotated name.
	if err := os.WriteFile(path, make([]byte, maxDiagLogBytes+1), 0o600); err != nil {
		t.Fatalf("grow log: %v", err)
	}
	log, err := openDiagLog(dir)
	if err != nil {
		t.Fatalf("openDiagLog after growth: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("log size = %d after exceeding the cap, want a fresh file", info.Size())
	}
	rotated, err := os.Stat(path + rotatedDiagLogSuffix)
	if err != nil {
		t.Fatalf("stat rotated log: %v", err)
	}
	if rotated.Size() != maxDiagLogBytes+1 {
		t.Fatalf("rotated log size = %d, want the whole previous generation (%d)",
			rotated.Size(), maxDiagLogBytes+1)
	}
}

// A log that cannot be opened must degrade to plain stderr, never fail the
// relay: diagnostics are strictly best effort.
func TestDiagLogRefusesAnUnusableLocation(t *testing.T) {
	if _, err := openDiagLog(""); err == nil {
		t.Fatal("openDiagLog(\"\") = nil error, want a failure the caller can fall back from")
	}
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDiagLog(notADir); err == nil {
		t.Fatal("openDiagLog on a non-directory = nil error, want a failure")
	}
}

func TestApplicationSyncFailureKeepsSessionAlive(t *testing.T) {
	var stdout bytes.Buffer
	calls := 0
	fake := &fakeSyncer{onSync: func(_ []json.RawMessage) ([]json.RawMessage, error) {
		calls++
		if calls == 1 {
			return nil, applicationSyncFailure(errors.New("stale review action"))
		}
		return []json.RawMessage{rawMsg(t, protocol.MsgAck, "ackid000003", "", 0, map[string]any{})}, nil
	}}
	b := newBridge(fake, nil, &stdout, io.Discard)

	hello := rawMsg(t, protocol.MsgHello, "helloid00003", "", 0, map[string]any{"extension_version": "1.0.0"})
	if err := b.handleInbound(context.Background(), hello); err != nil {
		t.Fatalf("application failure escaped handleInbound: %v", err)
	}
	if code := errorCode(t, readTestFrame(t, bytes.NewReader(stdout.Bytes()))); code != "application_error" {
		t.Fatalf("error code = %q, want application_error", code)
	}

	ack := rawMsg(t, protocol.MsgAck, "ackid000004", "", 1, map[string]any{})
	if err := b.handleInbound(context.Background(), ack); err != nil {
		t.Fatalf("subsequent request failed after application error: %v", err)
	}
	if got := readTestFrame(t, bytes.NewReader(stdout.Bytes()[frameLength(stdout.Bytes()):])).Type; got != protocol.MsgAck {
		t.Fatalf("subsequent outbound type = %q, want ack", got)
	}
}

func TestTransportSyncFailureIsFatal(t *testing.T) {
	fake := &fakeSyncer{onSync: func(_ []json.RawMessage) ([]json.RawMessage, error) {
		return nil, transportSyncFailure(errors.New("daemon socket closed"))
	}}
	b := newBridge(fake, nil, io.Discard, io.Discard)
	hello := rawMsg(t, protocol.MsgHello, "helloid00004", "", 0, map[string]any{"extension_version": "1.0.0"})
	if err := b.handleInbound(context.Background(), hello); err == nil {
		t.Fatal("transport failure returned nil, want fatal error")
	}
}

func frameLength(data []byte) int {
	if len(data) < 4 {
		return len(data)
	}
	return 4 + int(binary.LittleEndian.Uint32(data[:4]))
}
