package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/ipc"
)

// TestSocketCallNamesTheDaemonLogForABareInternalFailure: the daemon answers an
// error it did not classify with the fixed template "operation failed" and
// logs the cause. The operator must be told where that log is; a failure that
// already carries a class, or a classified code, is passed through untouched.
func TestSocketCallNamesTheDaemonLogForABareInternalFailure(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "papio.sock")
	logPath := filepath.Join(filepath.Dir(socket), "daemon.log")
	for _, test := range []struct {
		name    string
		remote  *ipc.RemoteError
		wantLog bool
	}{
		{name: "bare internal", remote: &ipc.RemoteError{Code: "internal", Message: "operation failed"}, wantLog: true},
		{name: "internal with detail", remote: &ipc.RemoteError{Code: "internal", Message: "operation failed", Detail: &ipc.ErrorDetail{ErrorClass: "zotero_field_validation"}}},
		{name: "classified code", remote: &ipc.RemoteError{Code: "not_found", Message: "operation failed"}},
		{name: "specific internal message", remote: &ipc.RemoteError{Code: "internal", Message: "the request was recorded, but releasing its document-delivery request failed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opt := &options{rpcCall: func(context.Context, string, string, any, any) error { return test.remote }}
			err := opt.socketCall(context.Background(), socket, "jobs.retry", nil, nil)
			var remote *ipc.RemoteError
			if !errors.As(err, &remote) || remote.Code != test.remote.Code {
				t.Fatalf("err = %v, want the daemon's %s error", err, test.remote.Code)
			}
			if got := strings.Contains(err.Error(), logPath); got != test.wantLog {
				t.Fatalf("err = %q: names %s = %t, want %t", err, logPath, got, test.wantLog)
			}
			if !test.wantLog && err.Error() != test.remote.Error() {
				t.Fatalf("err = %q, want it unchanged (%q)", err, test.remote.Error())
			}
			if test.wantLog && !strings.HasPrefix(err.Error(), "internal: operation failed") {
				t.Fatalf("err = %q, want the daemon's message kept as the prefix", err)
			}
		})
	}
}
