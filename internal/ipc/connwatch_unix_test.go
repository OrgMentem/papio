// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !windows

package ipc

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A client that has sent its request has half-closed its socket and is still
// waiting for the answer. macOS reports that drained half-close as POLLHUP on
// the read side, which once cancelled every handler's context as soon as its
// request was read.
func TestWaitingClientKeepsItsHandlerContext(t *testing.T) {
	socket, _ := startTestServer(t, HandlerFunc(func(ctx context.Context, _ Request) ([]byte, *RPCError) {
		select {
		case <-ctx.Done():
			return nil, &RPCError{Code: "cancelled", Message: "handler context cancelled while its client waited"}
		case <-time.After(3 * connWatchInterval):
			return []byte(`{}`), nil
		}
	}))
	if _, err := NewSocketClient(socket).CallRaw(context.Background(), "request_01", "jobs.get", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallRaw = %v, want the handler to finish while its client waits", err)
	}
}
