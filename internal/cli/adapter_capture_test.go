// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
)

func TestAdapterCaptureCommandForwardsStructuredRequestAndPrintsPath(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "adapter.capture_v1" {
			t.Fatalf("RPC method = %q, want adapter.capture_v1", method)
		}
		got, ok := params.(adapterCaptureParams)
		if !ok {
			t.Fatalf("params type = %T", params)
		}
		if got.URL != "https://www.jstor.org/stable/123" || got.Provider != "jstor" || got.Scenario != "success" || got.SettleMS == nil || *got.SettleMS != 2500 {
			t.Fatalf("params = %#v", got)
		}
		*result.(*api.AdapterCaptureResult) = api.AdapterCaptureResult{Outcome: "captured", Path: "/tmp/jstor-success.html"}
		return nil
	})
	root.SetArgs([]string{"adapter", "capture", "https://www.jstor.org/stable/123", "--provider", "jstor", "--scenario", "success", "--settle-ms", "2500"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("adapter capture: %v (stderr: %s)", err, errOut.String())
	}
	if got, want := out.String(), "captured\t/tmp/jstor-success.html\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// A busy, timed-out, refused or failed capture stored no page. It is still a
// structured outcome on stdout, but the command must exit non-zero: it used to
// exit 0, so a script checking only the status went on to use a fixture that
// was never saved.
func TestAdapterCaptureCommandJSONIsStructured(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.AdapterCaptureResult) = api.AdapterCaptureResult{RequestID: "capture-request-001", Outcome: "busy", Detail: "capture already running"}
		return nil
	})
	root.SetArgs([]string{"--json", "adapter", "capture", "https://www.jstor.org/stable/123", "--provider", "jstor", "--scenario", "drift"})
	if err := root.ExecuteContext(context.Background()); !errors.Is(err, errNoPageCaptured) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("adapter capture --json on busy: err = %v, want errNoPageCaptured naming the outcome", err)
	}
	var result api.AdapterCaptureResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if result.Outcome != "busy" || result.Detail != "capture already running" || result.RequestID != "capture-request-001" {
		t.Fatalf("result = %#v", result)
	}
}

func TestAdapterCaptureCommandFailsWhenNoPageWasStored(t *testing.T) {
	for _, result := range []api.AdapterCaptureResult{
		{Outcome: "timeout", Detail: "browser page capture timed out"},
		{Outcome: "not_permitted", Detail: "page capture storage is disabled"},
		{Outcome: "nav_failed", Detail: "capture content was not stored"},
		{Outcome: "captured"},
	} {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, got any) error {
			*got.(*api.AdapterCaptureResult) = result
			return nil
		})
		root.SetArgs([]string{"adapter", "capture", "https://www.jstor.org/stable/123", "--provider", "jstor", "--scenario", "drift"})
		err := root.ExecuteContext(context.Background())
		if !errors.Is(err, errNoPageCaptured) {
			t.Fatalf("%s: err = %v, want errNoPageCaptured", result.Outcome, err)
		}
		if result.Outcome == "not_permitted" && !strings.Contains(err.Error(), "[captures] enabled = true") {
			t.Fatalf("not_permitted: err = %v, want the remedy", err)
		}
		if !strings.HasPrefix(out.String(), result.Outcome) {
			t.Fatalf("%s: stdout = %q, want the outcome still printed", result.Outcome, out.String())
		}
	}
}

// Two papio binaries on one machine is documented as routine, so a new CLI
// meeting an older daemon is an ordinary outcome. Every other versioned
// command renders the actionable upgrade message; this one used to surface the
// raw JSON-RPC error instead.
func TestAdapterCaptureReportsDaemonUpgradeOnUnknownMethod(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _, _ any) error {
		if method != "adapter.capture_v1" {
			t.Fatalf("RPC method = %q, want adapter.capture_v1", method)
		}
		return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
	})
	root.SetArgs([]string{"adapter", "capture", "https://provider.example.edu/doi/10.1000/x", "--provider", "jstor", "--scenario", "success"})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("adapter capture against an older daemon: want error, got nil")
	}
	if !strings.Contains(err.Error(), "adapter.capture_v1") {
		t.Fatalf("error = %q, want it to name the unsupported method", err)
	}
	if strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("error = %q, want the upgrade guidance rather than the raw RPC error", err)
	}
}
