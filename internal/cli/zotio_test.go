// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/zotio"
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

	socket := filepath.Join(dataDir, "papio.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var handled atomic.Bool
	server := &ipc.Server{SocketPath: socket, Handler: ipc.HandlerFunc(func(_ context.Context, request ipc.Request) ([]byte, *ipc.RPCError) {
		switch request.Method {
		case "ping":
			return []byte(`{"status":"ok","version":"` + api.Version + `","extension_connected":false,"extension_version":""}`), nil
		case "zotio.apply":
			handled.Store(true)
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
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve test socket: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("test socket did not stop after cancellation")
		}
	})
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if err := ipc.WaitForSocket(waitCtx, socket, time.Millisecond); err != nil {
		t.Fatalf("test socket did not become ready: %v", err)
	}

	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"--config", configPath, "zotio", "apply", "zplan_deadbeef", "--confirm-sha256", "sha256:test"})
	err = root.Execute()
	if !handled.Load() {
		t.Fatalf("zotio.apply did not reach the test daemon: %v", err)
	}
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

// `zotio plan` printed only the plan id, confirmation hash, route and job, so
// the operator pasted a hash into `zotio apply` for a mutation they were never
// shown. Each plan must name the item it changes, the attachment and the
// collection, and say where the full preview is.
func TestZotioPlanShowsTheMutationItConfirms(t *testing.T) {
	plans := []*zotio.Plan{
		{ID: "zplan_a", JobID: "job_a", Route: "existing_item", ExpectedParentKey: "ABCD2345", AttachmentMode: "stored", ArtifactPath: "/data/a.pdf", Collection: "Reading\x1b]0;x\x07", ConfirmationSHA256: "sha-a"},
		{ID: "zplan_b", JobID: "job_b", Route: "manifest_create", AttachmentMode: "linked-file", ArtifactPath: "/data/b.pdf", ConfirmationSHA256: "sha-b"},
	}
	var out bytes.Buffer
	root := NewInProcessRoot(&out, &bytes.Buffer{}, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "zotio.plan" {
			t.Fatalf("method = %q", method)
		}
		result.(*struct {
			Plans []*zotio.Plan `json:"plans"`
		}).Plans = plans
		return nil
	})
	root.SetArgs([]string{"zotio", "plan", "job_a", "job_b"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"zplan_a  sha-a  existing_item  job_a\n",
		"attach to existing item ABCD2345", "stored /data/a.pdf", "collection:   Reading]0;x\n",
		"zplan_b  sha-b  manifest_create  job_b\n",
		"create a new Zotero item", "linked-file /data/b.pdf", "collection:   none",
		"papio --json zotio plan job_b",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan output lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("plan output passed terminal controls through: %q", got)
	}
}
