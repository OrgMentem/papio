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

	"github.com/mark3labs/mcp-go/client"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/store/storetest"
	"papio/internal/work"
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
	cfg.DataDir = storetest.DataDir(t)
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
				{Status: zotio.OwnershipOwnedMissingPDF, ItemKey: "EXIST001"},
			}}, nil
		case "acquire.submit_v2":
			var input struct {
				Request    protocol.WorkRequest `json:"request"`
				AutoImport *bool                `json:"auto_import"`
				Force      bool                 `json:"force"`
			}
			if err := json.Unmarshal(params, &input); err != nil {
				return nil, err
			}
			if input.AutoImport == nil || !*input.AutoImport {
				return nil, fmt.Errorf("auto_import was not defaulted to true")
			}
			result, err := system.App.SubmitWithOptionsAs(context.Background(), job.PrincipalMCP, input.Request,
				app.SubmitOptions{AutoImport: input.AutoImport, Force: input.Force})
			if err != nil {
				return nil, err
			}
			return struct {
				JobID    string `json:"job_id"`
				Existing bool   `json:"existing"`
			}{JobID: result.JobID, Existing: result.Existing}, nil
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
	if output.ExistingItem[0].ZotioItemKey != "EXIST001" || output.ExistingItem[0].Collection != "Reading" {
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

func TestJobsResourceEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		truncated bool
	}{
		{name: "truncated", count: resourceRowCap + 1, truncated: true},
		{name: "complete", count: 3, truncated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system := newResourceTestSystem(t)
			seedResourceJobs(t, system, tc.count)

			value, err := jobsResource(context.Background(), system)
			if err != nil {
				t.Fatal(err)
			}
			assertResourcePage(t, value, "jobs", min(tc.count, resourceRowCap), tc.truncated)
		})
	}
}

func TestJobsResourceEmptyEnvelopeUsesArray(t *testing.T) {
	value, err := jobsResource(context.Background(), newResourceTestSystem(t))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"jobs":[]`) {
		t.Fatalf("jobs resource JSON = %s, want empty jobs array", data)
	}
}

func TestArtifactsResourceEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		truncated bool
	}{
		{name: "truncated", count: resourceRowCap + 1, truncated: true},
		{name: "complete", count: 3, truncated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system := newResourceTestSystem(t)
			seedResourceArtifacts(t, system, tc.count)

			value, err := artifactsResource(context.Background(), system)
			if err != nil {
				t.Fatal(err)
			}
			assertResourcePage(t, value, "artifacts", min(tc.count, resourceRowCap), tc.truncated)
		})
	}
}

func TestExportsResourceEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		kind      string
		filter    string
		key       string
		truncated bool
	}{
		{name: "exports truncated", count: resourceRowCap + 1, kind: "zotio_apply", key: "exports", truncated: true},
		{name: "exports complete", count: 3, kind: "zotio_apply", key: "exports", truncated: false},
		{name: "bundles complete", count: 3, kind: "bundle", filter: "bundle", key: "bundles", truncated: false},
		{name: "plans complete", count: 3, kind: "zotio_plan", filter: "zotio_plan", key: "plans", truncated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system := newResourceTestSystem(t)
			seedResourceExports(t, system, tc.count, tc.kind)

			value, err := exportsResource(context.Background(), system, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			assertResourcePage(t, value, tc.key, min(tc.count, resourceRowCap), tc.truncated)
		})
	}
}

func newResourceTestSystem(t *testing.T) *bootstrap.System {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = storetest.DataDir(t)
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	return system
}

func seedResourceJobs(t *testing.T, system *bootstrap.System, count int) {
	t.Helper()
	ctx := context.Background()
	policy := job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any"}
	for i := range count {
		if _, err := system.Jobs.CreateRequest(ctx, fmt.Sprintf("resource-job-%03d", i), work.Work{
			DOI: fmt.Sprintf("10.1000/resource-job-%03d", i),
		}, "", "", policy, nil, job.PrincipalUnknown); err != nil {
			t.Fatal(err)
		}
	}
}

func seedResourceArtifacts(t *testing.T, system *bootstrap.System, count int) {
	t.Helper()
	ctx := context.Background()
	for i := range count {
		if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{
			SHA256:    fmt.Sprintf("%064x", i+1),
			SizeBytes: 1,
			MIME:      "application/pdf",
			Path:      fmt.Sprintf("/tmp/resource-%03d.pdf", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedResourceExports(t *testing.T, system *bootstrap.System, count int, kind string) {
	t.Helper()
	ctx := context.Background()
	for i := range count {
		if _, err := system.Store.DB().ExecContext(ctx,
			`INSERT INTO exports(job_id, kind, idempotency_key, created_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("resource-job-%03d", i), kind, fmt.Sprintf("resource-export-%03d", i), "2026-07-20T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
}

func assertResourcePage(t *testing.T, value any, key string, wantCount int, wantTruncated bool) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("resource page has %d keys, want %q and %q", len(page), key, "truncated")
	}
	rows, ok := page[key]
	if !ok {
		t.Fatalf("resource page = %s, missing %q", data, key)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rows, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != wantCount {
		t.Fatalf("%s count = %d, want %d", key, len(entries), wantCount)
	}
	var truncated bool
	if err := json.Unmarshal(page["truncated"], &truncated); err != nil {
		t.Fatal(err)
	}
	if truncated != wantTruncated {
		t.Fatalf("truncated = %t, want %t", truncated, wantTruncated)
	}
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

// resourceEnvelope is one decoded agentjson page: the row list under its own
// key plus the cap flag, and nothing else.
type resourceEnvelope struct {
	rows      []json.RawMessage
	truncated bool
}

// TestServerExposesResources pins the whole MCP resource surface. A client that
// only calls ListTools/CallTool never reaches registerResources, so both the URI
// set and the internal/agentjson envelope shape can drift silently. The URI
// assertion is exact, like TestServerExposesExactToolSurface: a new resource has
// to be declared here before it can ship.
func TestServerExposesResources(t *testing.T) {
	ctx := context.Background()
	system := newResourceSystem(t)
	seedResourceRows(t, system)
	c := newTestClient(t, system, toolDependencies{now: time.Now, wait: waitForPoll}, emptyFactory)

	listed, err := c.ListResources(ctx, mcplib.ListResourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	uris := make([]string, 0, len(listed.Resources))
	for _, resource := range listed.Resources {
		uris = append(uris, resource.URI)
		if resource.MIMEType != "application/json" {
			t.Errorf("%s mime = %q, want application/json", resource.URI, resource.MIMEType)
		}
		if strings.TrimSpace(resource.Description) == "" {
			t.Errorf("%s has no description", resource.URI)
		}
	}
	sort.Strings(uris)
	wantURIs := []string{"papio://artifacts", "papio://bundles", "papio://exports", "papio://jobs", "papio://zotio/plans"}
	if strings.Join(uris, ",") != strings.Join(wantURIs, ",") {
		t.Fatalf("resources = %v, want %v", uris, wantURIs)
	}

	// Counts follow seedResourceRows: one job and one bundle export past the
	// row cap, two artifacts, one plan, one apply record.
	cases := []struct {
		uri       string
		key       string
		rows      int
		truncated bool
	}{
		{"papio://jobs", "jobs", resourceRowCap, true},
		{"papio://artifacts", "artifacts", 2, false},
		{"papio://bundles", "bundles", resourceRowCap, true},
		{"papio://zotio/plans", "plans", 1, false},
		{"papio://exports", "exports", resourceRowCap, true},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			page := readResourceEnvelope(t, c, tc.uri, tc.key)
			if len(page.rows) != tc.rows {
				t.Errorf("%s rows = %d, want %d", tc.uri, len(page.rows), tc.rows)
			}
			if page.truncated != tc.truncated {
				t.Errorf("%s truncated = %v, want %v", tc.uri, page.truncated, tc.truncated)
			}
		})
	}

	// artifactsResource joins each hash back through Jobs.GetArtifact, so the
	// rows must carry the seeded digests, not just the right count.
	artifacts := readResourceEnvelope(t, c, "papio://artifacts", "artifacts")
	shas := make([]string, 0, len(artifacts.rows))
	for _, row := range artifacts.rows {
		var artifact job.Artifact
		if err := json.Unmarshal(row, &artifact); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, artifact.SHA256)
	}
	sort.Strings(shas)
	if want := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}; strings.Join(shas, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact hashes = %v, want %v", shas, want)
	}

	// Kind filtering is the only difference between the three export
	// resources, so the plan resource must not leak the bundle rows.
	plans := readResourceEnvelope(t, c, "papio://zotio/plans", "plans")
	var plan exportRecord
	if err := json.Unmarshal(plans.rows[0], &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Kind != "zotio_plan" {
		t.Fatalf("plan kind = %q, want zotio_plan", plan.Kind)
	}

	t.Run("empty", func(t *testing.T) {
		// An empty surface still owes a list: `null` would break the obvious
		// `for row in payload[key]` every agent consumer writes.
		empty := newTestClient(t, newResourceSystem(t), toolDependencies{now: time.Now, wait: waitForPoll}, emptyFactory)
		for _, tc := range cases {
			page := readResourceEnvelope(t, empty, tc.uri, tc.key)
			if len(page.rows) != 0 || page.truncated {
				t.Errorf("%s on empty store = %d rows, truncated %v", tc.uri, len(page.rows), page.truncated)
			}
		}
	})
}

func newResourceSystem(t *testing.T) *bootstrap.System {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = storetest.DataDir(t)
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	return system
}

// seedResourceRows fills every resource-backed table. jobs and bundle exports
// get one row more than resourceRowCap so an off-by-one in the LIMIT or in
// agentjson.Truncate shows up as a wrong count or a false truncated flag.
func seedResourceRows(t *testing.T, system *bootstrap.System) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i <= resourceRowCap; i++ {
		if _, err := system.App.SubmitWithOptionsAs(ctx, job.PrincipalMCP, protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion,
			RequestID:     fmt.Sprintf("wr_resource_%03d", i),
			Identifiers:   &protocol.Identifiers{DOI: fmt.Sprintf("10.1000/resource-%03d", i)},
		}, app.SubmitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sha := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{
			SHA256: sha, SizeBytes: 1024, MIME: "application/pdf", PageCount: 3, TextChars: 900,
			Path: "artifacts/" + sha + ".pdf",
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(kind, key string) {
		if _, err := system.Store.DB().ExecContext(ctx,
			`INSERT INTO exports (job_id, kind, idempotency_key, path, result_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			"job_resource", kind, key, "exports/"+key, `{"ok":true}`, "2026-07-15T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i <= resourceRowCap; i++ {
		insert("bundle", fmt.Sprintf("bundle_%03d", i))
	}
	insert("zotio_plan", "plan_000")
	insert("zotio_apply", "apply_000")
}

// readResourceEnvelope reads one resource and pins the agentjson contract on it:
// a JSON object with exactly the named row key and truncated, the rows a real
// list rather than null, and never a bare top-level array.
func readResourceEnvelope(t *testing.T, c *client.Client, uri, key string) resourceEnvelope {
	t.Helper()
	req := mcplib.ReadResourceRequest{}
	req.Params.URI = uri
	res, err := c.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("%s returned %d contents, want 1", uri, len(res.Contents))
	}
	text, ok := res.Contents[0].(mcplib.TextResourceContents)
	if !ok {
		t.Fatalf("%s content = %T, want text", uri, res.Contents[0])
	}
	if text.URI != uri || text.MIMEType != "application/json" {
		t.Fatalf("%s content header = %q %q", uri, text.URI, text.MIMEType)
	}
	if trimmed := strings.TrimSpace(text.Text); !strings.HasPrefix(trimmed, "{") {
		t.Fatalf("%s payload is not an envelope object: %s", uri, trimmed)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil {
		t.Fatalf("decode %s: %v", uri, err)
	}
	keys := make([]string, 0, len(envelope))
	for name := range envelope {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	want := []string{key, "truncated"}
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("%s envelope keys = %v, want %v", uri, keys, want)
	}
	if string(envelope[key]) == "null" {
		t.Fatalf("%s rows are null, want a list", uri)
	}
	page := resourceEnvelope{}
	if err := json.Unmarshal(envelope[key], &page.rows); err != nil {
		t.Fatalf("%s rows are not a list: %v", uri, err)
	}
	if err := json.Unmarshal(envelope["truncated"], &page.truncated); err != nil {
		t.Fatalf("%s truncated is not a bool: %v", uri, err)
	}
	return page
}
