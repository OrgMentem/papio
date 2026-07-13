// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

func testSystem(t *testing.T) *bootstrap.System {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	return system
}

func callMethod(t *testing.T, router ipc.Router, method string, params any, result any) *ipc.RPCError {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	data, rpcErr := router.Handle(context.Background(), ipc.Request{Method: method, Params: raw})
	if rpcErr == nil && result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			t.Fatalf("decode %s result: %v (%s)", method, err, data)
		}
	}
	return rpcErr
}

func TestRouterSubmitListGetAndCancel(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "request_api_01",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/example"},
	}
	var submitted SubmitResult
	if rpcErr := callMethod(t, router, "acquire.submit", request, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if submitted.JobID == "" {
		t.Fatal("empty submitted job id")
	}
	var rows []job.Row
	if rpcErr := callMethod(t, router, "jobs.list", map[string]any{"limit": 10}, &rows); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(rows) != 1 || rows[0].ID != submitted.JobID || rows[0].Work.DOI != "10.1000/example" {
		t.Fatalf("job list = %+v", rows)
	}
	var detail JobDetail
	if rpcErr := callMethod(t, router, "jobs.get", map[string]string{"job_id": submitted.JobID}, &detail); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if detail.Job.ID != submitted.JobID || len(detail.Events) == 0 {
		t.Fatalf("job detail = %+v", detail)
	}
	if rpcErr := callMethod(t, router, "jobs.cancel", map[string]string{"job_id": submitted.JobID}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	cancelled, err := system.Jobs.Get(context.Background(), submitted.JobID)
	if err != nil || cancelled.State != job.StateCancelled {
		t.Fatalf("cancelled job = %+v, %v", cancelled, err)
	}
}

func TestRouterRetryAndStrictEmptyParams(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "request_api_retry", work.Work{DOI: "10.1000/retry"}, "", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateFailed, nil, job.WithTerminalReason("test")); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	if rpcErr := callMethod(t, router, "jobs.retry", map[string]string{"job_id": id}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	row, _ := system.Jobs.Get(ctx, id)
	if row.State != job.StateResolving {
		t.Fatalf("retry state = %s", row.State)
	}
	if rpcErr := callMethod(t, router, "ping", map[string]bool{"unexpected": true}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("ping unknown params error = %+v", rpcErr)
	}
}

func TestRouterDoctorProducesStructuredReport(t *testing.T) {
	system := testSystem(t)
	var result struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if rpcErr := callMethod(t, Router(system), "doctor.run", struct{}{}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(result.Checks) == 0 {
		t.Fatal("doctor returned no checks")
	}
}

func TestRouterShutdownRespondsThenCancels(t *testing.T) {
	system := testSystem(t)
	ctx, cancel := context.WithCancel(context.Background())
	var result map[string]bool
	if rpcErr := callMethod(t, RouterWithShutdown(system, cancel), "daemon.shutdown", struct{}{}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !result["stopping"] {
		t.Fatalf("shutdown result = %v", result)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not invoked")
	}
}

func TestRouterBrowserSyncHandshakeAndInvalidFrame(t *testing.T) {
	system := testSystem(t)
	router := Router(system)

	hello := json.RawMessage(`{"protocol":"papio-browser/0.1","type":"hello","msg_id":"client-hello-1","seq":0,"payload":{"extension_version":"1.0.0"}}`)
	var result struct {
		Outbound []json.RawMessage `json:"outbound"`
	}
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{"messages": []json.RawMessage{hello}}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(result.Outbound) != 1 {
		t.Fatalf("outbound = %d frames, want 1 (hello_ack)", len(result.Outbound))
	}
	msg, err := protocol.DecodeBrowserMessage(result.Outbound[0])
	if err != nil || msg.Type != protocol.MsgHelloAck {
		t.Fatalf("outbound[0] = %+v, %v", msg, err)
	}

	// An empty poll is valid and returns no frames.
	var empty struct {
		Outbound []json.RawMessage `json:"outbound"`
	}
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{}, &empty); rpcErr != nil {
		t.Fatal(rpcErr)
	}

	// A structurally invalid frame fails closed as invalid_argument.
	bad := json.RawMessage(`{"protocol":"papio-browser/0.1","type":"hello","msg_id":"x","seq":0,"payload":{"extension_version":"1.0.0"}}`)
	rpcErr := callMethod(t, router, "browser.sync", map[string]any{"messages": []json.RawMessage{bad}}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("invalid frame error = %+v, want invalid_argument", rpcErr)
	}
}
