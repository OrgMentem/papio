// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
)

func TestZotioApplyRendersSafeFailureDetail(t *testing.T) {
	dataDir, err := os.MkdirTemp("", "papio-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = dataDir
	if err := config.Save(cfg, configPath); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", filepath.Join(dataDir, "papio.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &ipc.Server{Handler: ipc.HandlerFunc(func(_ context.Context, request ipc.Request) ([]byte, *ipc.RPCError) {
		switch request.Method {
		case "ping":
			return []byte(`{"status":"ok","version":"` + api.Version + `","extension_connected":false,"extension_version":""}`), nil
		case "zotio.apply":
			return nil, &ipc.RPCError{
				Code:    "internal",
				Message: "operation failed",
				Detail: &ipc.ErrorDetail{
					ErrorClass: "zotero_field_validation",
					ErrorHint:  "unknown item field",
				},
			}
		default:
			return nil, &ipc.RPCError{Code: "unexpected", Message: "unexpected method"}
		}
	})}
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Errorf("serve test socket: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"--config", configPath, "zotio", "apply", "zplan_deadbeef", "--confirm-sha256", "sha256:test"})
	err = root.Execute()
	if err == nil {
		t.Fatal("zotio apply unexpectedly succeeded")
	}
	got := err.Error()
	for _, want := range []string{"internal: operation failed", "zotero_field_validation", "unknown item field"} {
		if !strings.Contains(got, want) {
			t.Fatalf("apply error %q missing %q", got, want)
		}
	}
	if strings.Contains(got, dataDir) || strings.Contains(got, "https://") {
		t.Fatalf("apply error leaked private detail: %q", got)
	}
}
