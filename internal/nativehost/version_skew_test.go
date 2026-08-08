// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package nativehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/protocol"
)

func protocolSkewFixture(t *testing.T, version string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "invalid", "browser-wrong-protocol.json"))
	if err != nil {
		t.Fatalf("read shared wrong-protocol corpus fixture: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode shared wrong-protocol corpus fixture: %v", err)
	}
	envelope["protocol"] = version
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode %s skew frame: %v", version, err)
	}
	return out
}

func TestVersionSkewInboundFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
	}{
		{name: "draft_protocol", protocol: "papio-browser/0.1"},
		{name: "unknown_future_minor", protocol: "papio-browser/1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			forwarded := 0
			bridge := newBridge(&fakeSyncer{onSync: func([]json.RawMessage) ([]json.RawMessage, error) {
				forwarded++
				return nil, nil
			}}, nil, &stdout, ioDiscard{})

			if err := bridge.handleInbound(context.Background(), protocolSkewFixture(t, tc.protocol)); err == nil {
				t.Fatal("version-skew frame was accepted")
			}
			if forwarded != 0 {
				t.Fatalf("version-skew frame reached daemon %d times, want 0", forwarded)
			}
			if code := errorCode(t, readTestFrame(t, bytes.NewReader(stdout.Bytes()))); code != "invalid_frame" {
				t.Fatalf("error code = %q, want invalid_frame", code)
			}
		})
	}
}

func TestOlderDaemonApplicationRejectionKeepsSession(t *testing.T) {
	var stdout bytes.Buffer
	forwarded := 0
	bridge := newBridge(&fakeSyncer{onSync: func([]json.RawMessage) ([]json.RawMessage, error) {
		forwarded++
		return nil, applicationSyncFailure(errors.New("unsupported browser protocol by older daemon"))
	}}, nil, &stdout, ioDiscard{})

	// The newer extension still emits a syntactically valid locked browser
	// frame. A daemon that cannot meet its expectation reports an application
	// failure; the native host must keep the browser session available for
	// subsequent requests rather than treating that response as a broken port.
	frame := rawMsg(t, protocol.MsgHello, "future-extension-hello", "", 0,
		map[string]any{"extension_version": "2.0.0"})
	if err := bridge.handleInbound(context.Background(), frame); err != nil {
		t.Fatalf("application rejection killed the session: %v", err)
	}
	if forwarded != 1 {
		t.Fatalf("daemon calls = %d, want exactly one hello forward", forwarded)
	}
	if code := errorCode(t, readTestFrame(t, bytes.NewReader(stdout.Bytes()))); code != "application_error" {
		t.Fatalf("error code = %q, want application_error", code)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
