// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestAdapterCaptureCommandJSONIsStructured(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.AdapterCaptureResult) = api.AdapterCaptureResult{RequestID: "capture-request-001", Outcome: "busy", Detail: "capture already running"}
		return nil
	})
	root.SetArgs([]string{"--json", "adapter", "capture", "https://www.jstor.org/stable/123", "--provider", "jstor", "--scenario", "drift"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("adapter capture --json: %v (stderr: %s)", err, errOut.String())
	}
	var result api.AdapterCaptureResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if result.Outcome != "busy" || result.Detail != "capture already running" || result.RequestID != "capture-request-001" {
		t.Fatalf("result = %#v", result)
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
