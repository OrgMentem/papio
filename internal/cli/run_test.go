package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
)

type decodedJSONError struct {
	Error map[string]any `json:"error"`
}

func decodeJSONError(t *testing.T, raw string) map[string]any {
	t.Helper()
	if strings.Count(strings.TrimSuffix(raw, "\n"), "\n") != 0 {
		t.Fatalf("stderr = %q, want exactly one line", raw)
	}
	var decoded decodedJSONError
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil || decoded.Error == nil {
		t.Fatalf("stderr = %q, want one {\"error\":{...}} object (%v)", raw, err)
	}
	return decoded.Error
}

func TestReportErrorJSONCodes(t *testing.T) {
	remote := &ipc.RemoteError{Code: "not_found", Message: "job nope not found",
		Detail: &ipc.ErrorDetail{ErrorClass: "missing", ErrorHint: "list jobs", ErrorHTTPStatus: 404}}
	dial := ipc.Unreachable(fmt.Errorf("dial ipc daemon: %w", errors.New("connect: no such file")))
	tests := []struct {
		name string
		err  error
		want map[string]any
	}{
		{"remote not found", fmt.Errorf("wrapped: %w", remote), map[string]any{
			"code": "not_found", "message": "job nope not found", "class": "missing", "hint": "list jobs", "http_status": float64(404)}},
		{"unreachable", dial, map[string]any{"code": "daemon_unreachable", "message": "dial ipc daemon: connect: no such file"}},
		{"usage", &usageError{err: errors.New("unknown flag: --bogus")}, map[string]any{"code": "invalid_argument", "message": "unknown flag: --bogus"}},
		{"cobra args", errors.New("accepts 1 arg(s), received 0"), map[string]any{"code": "invalid_argument", "message": "accepts 1 arg(s), received 0"}},
		{"other", errors.New("boom"), map[string]any{"code": "error", "message": "boom"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			reportError(&errOut, tc.err, true, false)
			got := decodeJSONError(t, errOut.String())
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("error object = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReportErrorPlainWhenNotJSONOrResultAlreadyPrinted(t *testing.T) {
	for _, tc := range []struct{ json, wrote bool }{{false, false}, {false, true}, {true, true}} {
		var errOut bytes.Buffer
		reportError(&errOut, errors.New("boom"), tc.json, tc.wrote)
		if errOut.String() != "papio: boom\n" {
			t.Fatalf("json=%v wrote=%v stderr = %q, want the plain line", tc.json, tc.wrote, errOut.String())
		}
	}
}

func TestExitCode(t *testing.T) {
	remote := &ipc.RemoteError{Code: "invalid_argument", Message: "the daemon refused"}
	dial := ipc.Unreachable(errors.New("dial ipc daemon: connect: no such file"))
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"usage error", &usageError{err: errors.New("unknown flag: --bogus")}, 2},
		{"cobra args", errors.New("accepts 1 arg(s), received 0"), 2},
		{"wrapped usage", fmt.Errorf("wrapped: %w", &usageError{err: errors.New("bad")}), 2},
		{"unreachable", dial, 3},
		{"daemon down", daemonDown{socket: "/tmp/x.sock"}, 3},
		{"daemon error keeps 1 even with a usage-like code", remote, 1},
		{"other", errors.New("boom"), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Fatalf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunBadFlagIsInvalidArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	// --bogus precedes --json: the argument scan must still find --json.
	if code := Run(context.Background(), []string{"--bogus", "--json", "version"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if got := decodeJSONError(t, errOut.String()); got["code"] != "invalid_argument" {
		t.Fatalf("error = %v, want invalid_argument", got)
	}
	errOut.Reset()
	if code := Run(context.Background(), []string{"--json", "jobs", "get"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if got := decodeJSONError(t, errOut.String()); got["code"] != "invalid_argument" {
		t.Fatalf("missing arg error = %v, want invalid_argument", got)
	}
}

func TestRunNonJSONFailureUnchanged(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"--bogus", "version"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if errOut.String() != "papio: unknown flag: --bogus\n" {
		t.Fatalf("stderr = %q, want the plain line", errOut.String())
	}
}

func TestRunDaemonUnreachable(t *testing.T) {
	cfgDir := t.TempDir()
	_, callErr := ipc.NewSocketClient(filepath.Join(cfgDir, "none.sock")).CallRaw(context.Background(), "request_01", "ping", json.RawMessage(`{}`))
	var errOut bytes.Buffer
	reportError(&errOut, callErr, true, false)
	if got := decodeJSONError(t, errOut.String()); got["code"] != "daemon_unreachable" {
		t.Fatalf("real dial failure = %v, want daemon_unreachable", got)
	}
}

func TestRunCommandRemoteNotFound(t *testing.T) {
	var out, errOut bytes.Buffer
	counted := &countingWriter{w: &out}
	root := NewInProcessRoot(counted, &errOut, config.Config{}, func(context.Context, string, any, any) error {
		return &ipc.RemoteError{Code: "not_found", Message: "no such job"}
	})
	root.SetArgs([]string{"--json", "jobs", "get", "nope"})
	err := root.Execute()
	if err == nil {
		t.Fatal("jobs get nope succeeded")
	}
	reportError(&errOut, err, jsonRequested(root, nil), counted.n.Load() > 0)
	if got := decodeJSONError(t, errOut.String()); got["code"] != "not_found" || got["message"] != "no such job" {
		t.Fatalf("error = %v, want not_found", got)
	}
}

func TestDaemonDownIsUnreachable(t *testing.T) {
	var errOut bytes.Buffer
	reportError(&errOut, daemonDown{socket: "/tmp/x.sock"}, true, false)
	if got := decodeJSONError(t, errOut.String()); got["code"] != "daemon_unreachable" {
		t.Fatalf("daemon status down = %v, want daemon_unreachable", got)
	}
}

func TestRunLastJSONFlagWins(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"--json", "--json=false", "jobs", "get"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if errOut.String() != "papio: accepts 1 arg(s), received 0\n" {
		t.Fatalf("stderr = %q, want the plain line", errOut.String())
	}
	errOut.Reset()
	Run(context.Background(), []string{"--json=false", "--json", "jobs", "get"}, &out, &errOut)
	if got := decodeJSONError(t, errOut.String()); got["code"] != "invalid_argument" {
		t.Fatalf("error = %v, want invalid_argument", got)
	}
}
