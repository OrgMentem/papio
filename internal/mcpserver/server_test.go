// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/client"
	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/zotio"
)

// emptyFactory registers the facade tools without a real command tree, so
// surface assertions do not need the cli package (which would import-cycle).
func emptyFactory(_, _ io.Writer) *cobra.Command {
	return &cobra.Command{Use: "papio"}
}

func TestServerExposesExactToolSurface(t *testing.T) {
	c := newTestClient(t, nil, toolDependencies{now: time.Now, wait: waitForPoll}, emptyFactory)
	res, err := c.ListTools(context.Background(), mcplib.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"papio_acquire_batch", "papio_batch_wait", "papio_command_run", "papio_command_search"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestAcquireBatchCreatesReportableCLICompatibleManifest(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })

	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "zotio.lookup_works":
			return zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
				{Status: zotio.OwnershipNotOwned},
				{Status: zotio.OwnershipOwnedWithPDF},
				{Status: zotio.OwnershipOwnedMissingPDF, ItemKey: "EXIST0001"},
			}}, nil
		case "acquire.submit":
			var input struct {
				Request    protocol.WorkRequest `json:"request"`
				AutoImport *bool                `json:"auto_import"`
			}
			if err := json.Unmarshal(params, &input); err != nil {
				return nil, err
			}
			if input.AutoImport == nil || !*input.AutoImport {
				return nil, fmt.Errorf("auto_import was not defaulted to true")
			}
			id, err := system.App.SubmitWithAutoImport(context.Background(), input.Request, input.AutoImport)
			if err != nil {
				return nil, err
			}
			return struct {
				JobID string `json:"job_id"`
			}{JobID: id}, nil
		case "jobs.get":
			var input struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(params, &input); err != nil {
				return nil, err
			}
			row, err := system.Jobs.Get(context.Background(), input.JobID)
			if err != nil {
				return nil, err
			}
			return struct {
				Job *job.Row `json:"job"`
			}{Job: row}, nil
		default:
			return nil, fmt.Errorf("unexpected method %q", method)
		}
	}}
	c := newTestClient(t, system, toolDependencies{
		caller: callerFunc(fake.Call),
		now:    func() time.Time { return now },
		wait:   waitForPoll,
	}, nil)

	var output batch.SubmitOutput
	callToolJSON(t, c, "papio_acquire_batch", map[string]any{
		"works": []any{
			map[string]any{"doi": "10.1000/new", "title": "New"},
			map[string]any{"work": map[string]any{"doi": "10.1000/owned", "title": "Owned"}},
			map[string]any{"work": map[string]any{"arxiv": "arXiv:2601.12345v2", "title": "Existing"}},
		},
		"collection": "Reading",
		"label":      "weekly",
	}, &output)
	if len(output.Submitted) != 2 || len(output.SkippedOwned) != 1 || len(output.ExistingItem) != 1 {
		t.Fatalf("batch routing = %+v", output)
	}
	if output.ExistingItem[0].ZotioItemKey != "EXIST0001" || output.ExistingItem[0].Collection != "Reading" {
		t.Fatalf("existing-item routing = %+v", output.ExistingItem)
	}

	requests := make([]protocol.WorkRequest, 3)
	for i, raw := range []json.RawMessage{
		json.RawMessage(`{"doi":"10.1000/new","title":"New"}`),
		json.RawMessage(`{"work":{"doi":"10.1000/owned","title":"Owned"}}`),
		json.RawMessage(`{"work":{"arxiv":"arXiv:2601.12345v2","title":"Existing"}}`),
	} {
		requests[i], err = batch.ParseWork(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	wantManifest := batch.NewManifest(requests, "weekly", "Reading", now)
	if output.BatchID != wantManifest.ID {
		t.Fatalf("batch ID = %q, want %q", output.BatchID, wantManifest.ID)
	}
	for _, submitted := range output.Submitted {
		if submitted.RequestID != batch.RequestID(wantManifest.ID, requests[0]) && submitted.RequestID != batch.RequestID(wantManifest.ID, requests[2]) {
			t.Fatalf("submitted request ID %q is not CLI-compatible", submitted.RequestID)
		}
	}
	manifest, err := batch.Load(cfg.DataDir, output.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Label != "weekly" || manifest.Collection != "Reading" ||
		manifest.Works[0].Status != "submitted" || manifest.Works[1].Status != "skipped_owned" || manifest.Works[2].Status != "existing_item_attached" {
		t.Fatalf("manifest = %+v", manifest)
	}

	// The persisted manifest is joinable into a live report (the same path the
	// batch report command and papio_batch_wait use).
	report, err := api.BatchReport(context.Background(), system, output.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if report.BatchID != output.BatchID || report.Summary.Total != 3 {
		t.Fatalf("batch report = %+v", report)
	}

	capResult := callTool(t, c, "papio_acquire_batch", map[string]any{"works": make([]any, 51)})
	if !capResult.IsError {
		t.Fatalf("batch cap result should error: %+v", capResult)
	}
}

func TestBatchWaitToolSettlesAndTimesOut(t *testing.T) {
	t.Run("settles", func(t *testing.T) {
		now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
		reports := []batch.Report{
			{BatchID: "batch-deadbeef", Summary: batch.ReportSummary{Outcomes: map[string]int{"in_progress": 1}}, Works: []batch.ReportWork{{Outcome: "in_progress", Reason: job.StateFetching}}},
			{BatchID: "batch-deadbeef", Summary: batch.ReportSummary{Outcomes: map[string]int{"browser_fetched_then_imported": 1}}, Works: []batch.ReportWork{{Outcome: "browser_fetched_then_imported"}}},
		}
		index := 0
		fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
			if method != "acquire.report" {
				t.Fatalf("unexpected method %q", method)
			}
			requireJSONEqual(t, params, map[string]string{"batch_id": "latest"})
			report := reports[index]
			if index < len(reports)-1 {
				index++
			}
			return report, nil
		}}
		clock := now
		c := newTestClient(t, nil, toolDependencies{
			caller: callerFunc(fake.Call),
			now:    func() time.Time { return clock },
			wait: func(_ context.Context, duration time.Duration) error {
				clock = clock.Add(duration)
				return nil
			},
		}, nil)

		var output BatchWaitOutput
		callToolJSON(t, c, "papio_batch_wait", map[string]any{"batch_id": "latest", "timeout_seconds": 10, "poll_seconds": 2}, &output)
		if !output.Settled || output.Report == nil || output.Report.Works[0].Outcome != "browser_fetched_then_imported" {
			t.Fatalf("wait output = %+v", output)
		}
		if clock.Sub(now) != 2*time.Second || len(fake.calls) != 2 {
			t.Fatalf("clock = %s calls = %+v", clock, fake.calls)
		}
	})

	t.Run("times out", func(t *testing.T) {
		now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
		fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
			if method != "acquire.report" {
				t.Fatalf("unexpected method %q", method)
			}
			requireJSONEqual(t, params, map[string]string{"batch_id": "batch-deadbeef"})
			return batch.Report{BatchID: "batch-deadbeef", Summary: batch.ReportSummary{Outcomes: map[string]int{"in_progress": 1}}, Works: []batch.ReportWork{{Outcome: "in_progress", Reason: job.StateFetching}}}, nil
		}}
		clock := now
		c := newTestClient(t, nil, toolDependencies{
			caller: callerFunc(fake.Call),
			now:    func() time.Time { return clock },
			wait: func(_ context.Context, duration time.Duration) error {
				clock = clock.Add(duration)
				return nil
			},
		}, nil)

		var output BatchWaitOutput
		callToolJSON(t, c, "papio_batch_wait", map[string]any{"batch_id": "batch-deadbeef", "timeout_seconds": 1, "poll_seconds": 1}, &output)
		if output.Settled || output.Report == nil || output.Report.Works[0].Outcome != "in_progress" {
			t.Fatalf("wait output = %+v", output)
		}
		if len(fake.calls) != 2 {
			t.Fatalf("RPC calls = %+v", fake.calls)
		}
	})
}

type fakeRPCCall struct {
	Method string
	Params json.RawMessage
}

type fakeRPC struct {
	mu      sync.Mutex
	calls   []fakeRPCCall
	handler func(string, json.RawMessage) (any, error)
}

func (f *fakeRPC) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	f.calls = append(f.calls, fakeRPCCall{Method: method, Params: raw})
	value, err := f.handler(method, raw)
	if err != nil || result == nil {
		return err
	}
	response, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(response, result)
}

func newTestClient(t *testing.T, system *bootstrap.System, deps toolDependencies, factory func(io.Writer, io.Writer) *cobra.Command) *client.Client {
	t.Helper()
	s := newServer(system, factory, deps)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Initialize(ctx, mcplib.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	return c
}

func callTool(t *testing.T, c *client.Client, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func callToolJSON(t *testing.T, c *client.Client, name string, args map[string]any, target any) {
	t.Helper()
	res := callTool(t, c, name, args)
	if res.IsError {
		t.Fatalf("%s error: %s", name, resultText(res))
	}
	if err := json.Unmarshal([]byte(resultText(res)), target); err != nil {
		t.Fatalf("decode %s result %q: %v", name, resultText(res), err)
	}
}

func resultText(res *mcplib.CallToolResult) string {
	var b strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(mcplib.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func requireJSONEqual(t *testing.T, actual json.RawMessage, expected any) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(actual, &gotValue); err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expectedJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("params = %s, want %s", actual, expectedJSON)
	}
}
