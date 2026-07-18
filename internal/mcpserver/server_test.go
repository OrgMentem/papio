// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

func TestServerExposesExactToolSurface(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(nil).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "papio-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var actionsResolveTool, applyTool, batchAcquireTool, batchReportTool, batchWaitTool, doctorTool, exportTool, searchTool *mcp.Tool
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		switch tool.Name {
		case "papio_actions_resolve":
			actionsResolveTool = tool
		case "papio_acquire_batch":
			batchAcquireTool = tool
		case "papio_doctor":
			doctorTool = tool
		case "papio_zotio_apply":
			applyTool = tool
		case "papio_export_bundle":
			exportTool = tool
		case "papio_search":
			searchTool = tool
		case "papio_batch_report":
			batchReportTool = tool
		case "papio_batch_wait":
			batchWaitTool = tool
		}
	}
	sort.Strings(names)
	want := []string{"papio_acquire", "papio_acquire_batch", "papio_actions_list", "papio_actions_resolve", "papio_batch_report", "papio_batch_wait", "papio_doctor", "papio_export_bundle", "papio_search", "papio_status", "papio_watch_add", "papio_watch_list", "papio_watch_remove", "papio_zotio_apply", "papio_zotio_plan"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if applyTool == nil || applyTool.Annotations == nil || !applyTool.Annotations.IdempotentHint || applyTool.Annotations.ReadOnlyHint {
		t.Fatalf("apply annotations = %+v", applyTool)
	}
	schema, err := json.Marshal(applyTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"plan_id"`) || !strings.Contains(string(schema), `"confirmation_sha256"`) || strings.Contains(string(schema), `"job_id"`) {
		t.Fatalf("apply schema bypasses plan confirmation: %s", schema)
	}
	if doctorTool == nil || doctorTool.Annotations == nil || !doctorTool.Annotations.ReadOnlyHint {
		t.Fatalf("doctor annotations = %+v", doctorTool)
	}
	if exportTool == nil || exportTool.Annotations == nil || !exportTool.Annotations.ReadOnlyHint {
		t.Fatalf("export annotations = %+v", exportTool)
	}
	if batchAcquireTool == nil || batchAcquireTool.Annotations == nil || batchAcquireTool.Annotations.ReadOnlyHint {
		t.Fatalf("batch acquire annotations = %+v", batchAcquireTool)
	}
	batchAcquireSchema, err := json.Marshal(batchAcquireTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"works"`, `"auto_import"`, `"collection"`, `"label"`, `"include_owned"`} {
		if !strings.Contains(string(batchAcquireSchema), field) {
			t.Fatalf("batch acquire schema = %s", batchAcquireSchema)
		}
	}
	if batchReportTool == nil || batchReportTool.Annotations == nil || !batchReportTool.Annotations.ReadOnlyHint {
		t.Fatalf("batch report annotations = %+v", batchReportTool)
	}
	batchSchema, err := json.Marshal(batchReportTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(batchSchema), `"batch_id"`) || !strings.Contains(string(batchSchema), `"format"`) {
		t.Fatalf("batch report schema = %s", batchSchema)
	}
	if batchWaitTool == nil || batchWaitTool.Annotations == nil || !batchWaitTool.Annotations.ReadOnlyHint {
		t.Fatalf("batch wait annotations = %+v", batchWaitTool)
	}
	batchWaitSchema, err := json.Marshal(batchWaitTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(batchWaitSchema), `"batch_id"`) || !strings.Contains(string(batchWaitSchema), `"timeout_seconds"`) || !strings.Contains(string(batchWaitSchema), `"poll_seconds"`) {
		t.Fatalf("batch wait schema = %s", batchWaitSchema)
	}
	if actionsResolveTool == nil || !strings.Contains(actionsResolveTool.Description, "verify_identity") || !strings.Contains(actionsResolveTool.Description, "accept asserts") {
		t.Fatalf("actions resolve documentation = %+v", actionsResolveTool)
	}
	if searchTool == nil || searchTool.Annotations == nil || !searchTool.Annotations.ReadOnlyHint {
		t.Fatalf("search annotations = %+v", searchTool)
	}
	searchSchema, err := json.Marshal(searchTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(searchSchema), `"query"`) ||
		!strings.Contains(string(searchSchema), `"oa_only"`) ||
		!strings.Contains(string(searchSchema), `"cites"`) ||
		!strings.Contains(string(searchSchema), `"cited_by"`) ||
		!strings.Contains(string(searchSchema), `"related_to"`) ||
		!strings.Contains(string(searchSchema), `"new_only"`) ||
		!strings.Contains(searchTool.Description, "owned_item_key") ||
		!strings.Contains(searchTool.Description, "fewer results") {
		t.Fatalf("search schema = %s", searchSchema)
	}

	resources, err := clientSession.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, resource := range resources.Resources {
		uris = append(uris, resource.URI)
	}
	sort.Strings(uris)
	wantURIs := []string{"papio://artifacts", "papio://bundles", "papio://exports", "papio://jobs", "papio://zotio/plans"}
	if strings.Join(uris, ",") != strings.Join(wantURIs, ",") {
		t.Fatalf("resources = %v, want %v", uris, wantURIs)
	}
}

func TestDoctorReportsInProcessIntegrationFailures(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "papio-test-zotio-not-installed"
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(system).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "papio-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	var report doctor.Report
	callToolJSON(t, clientSession, "papio_doctor", map[string]any{}, &report)
	checks := make(map[string]doctor.Check, len(report.Checks))
	for _, check := range report.Checks {
		checks[check.Name] = check
	}
	if got, ok := checks["config"]; !ok || got.Status != doctor.Pass {
		t.Fatalf("config check = %#v", got)
	}
	if got, ok := checks["daemon"]; !ok || got.Status != doctor.Pass || got.Detail != "reachable; version "+api.Version {
		t.Fatalf("daemon check = %#v", got)
	}
	if got, ok := checks["zotio"]; !ok || got.Status != doctor.Fail {
		t.Fatalf("zotio check = %#v", got)
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
	caller := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
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
	client := testMCPClientForSystem(t, system, toolDependencies{
		caller: caller,
		now:    func() time.Time { return now },
		wait:   waitForPoll,
	})

	var output AcquireBatchOutput
	callToolJSON(t, client, "papio_acquire_batch", map[string]any{
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
	for _, manifestWork := range manifest.Works {
		if manifestWork.Work.Collection != "Reading" {
			t.Fatalf("manifest collection policy = %+v", manifest.Works)
		}
	}

	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "papio_batch_report", Arguments: map[string]any{"batch_id": output.BatchID},
	})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("batch report result = %+v, error = %v", result, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("batch report content = %#v", result.Content)
	}
	var report batch.Report
	if err := json.Unmarshal([]byte(text.Text), &report); err != nil {
		t.Fatal(err)
	}
	if report.BatchID != output.BatchID || report.Summary.Total != 3 {
		t.Fatalf("batch report = %+v", report)
	}

	capResult, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "papio_acquire_batch", Arguments: map[string]any{"works": make([]any, 51)},
	})
	if err != nil || !capResult.IsError {
		t.Fatalf("batch cap result = %+v, error = %v", capResult, err)
	}
}

func TestStatusToolGroupsLiveJobsThroughRPC(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	rows := []job.Row{
		{ID: "job-working", State: job.StateFetching, UpdatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Working paper"}},
		{ID: "job-human", State: job.StateAwaitingHuman, UpdatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Human review paper"}},
		{ID: "job-review", State: job.StateNeedsReview, UpdatedAt: now.Add(-15 * time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Review paper"}},
		{ID: "job-ready", State: job.StateReady, UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Ready paper"}},
		{ID: "job-failure", State: job.StateFailed, UpdatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Recent failure"}},
		{ID: "job-old-failure", State: job.StateFailed, UpdatedAt: now.Add(-25 * time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Old failure"}},
	}
	fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "jobs.list":
			requireJSONEqual(t, params, map[string]int{"limit": 500})
			return rows, nil
		case "jobs.get":
			var input struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(params, &input); err != nil {
				return nil, err
			}
			switch input.JobID {
			case "job-working":
				return map[string]any{"events": []map[string]any{{"kind": "candidate.selected", "detail": map[string]any{"source": "openalex"}}}}, nil
			case "job-human":
				return map[string]any{"events": []map[string]any{{"kind": "job.transition", "detail": map[string]any{"to": job.StateAwaitingHuman, "reason": "institutional sign-in required"}}}}, nil
			case "job-review":
				return map[string]any{"events": []map[string]any{{"kind": "job.transition", "detail": map[string]any{"to": job.StateNeedsReview, "reason": "identity confidence below threshold"}}}}, nil
			case "job-failure":
				return map[string]any{"events": []map[string]any{}}, nil
			case "job-ready":
				return map[string]any{"events": []map[string]any{{"kind": "zotio.auto_import", "detail": map[string]any{"status": "applied"}}}}, nil
			default:
				t.Fatalf("unexpected jobs.get job ID %q", input.JobID)
				return nil, nil
			}
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	}}
	client := testMCPClient(t, toolDependencies{caller: fake, now: func() time.Time { return now }, wait: waitForPoll})

	var snapshot StatusSnapshot
	callToolJSON(t, client, "papio_status", map[string]any{}, &snapshot)

	if snapshot.GeneratedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("generated_at = %q", snapshot.GeneratedAt)
	}
	if len(snapshot.Groups) != 5 {
		t.Fatalf("groups = %+v", snapshot.Groups)
	}
	if got := snapshot.Groups[0]; got.Phase != "working" || len(got.Jobs) != 1 || got.Jobs[0].Provider != "openalex" {
		t.Fatalf("working group = %+v", got)
	}
	if got := snapshot.Groups[1]; got.Phase != "awaiting_human" || got.Jobs[0].Reason != "institutional sign-in required" {
		t.Fatalf("human group = %+v", got)
	}
	if got := snapshot.Groups[2]; got.Phase != "needs_review" || got.Jobs[0].Reason != "identity confidence below threshold" {
		t.Fatalf("review group = %+v", got)
	}
	if got := snapshot.Groups[3]; got.Phase != "ready (last 24h)" || got.Jobs[0].ImportStatus != "applied" {
		t.Fatalf("ready group = %+v", got)
	}
	if got := snapshot.Groups[4]; got.Phase != "failed / unavailable" || got.Jobs[0].ID != "job-failure" {
		t.Fatalf("failed group = %+v", got)
	}
	if len(fake.calls) != 6 {
		t.Fatalf("RPC calls = %+v", fake.calls)
	}
}

func TestBuildStatusSnapshotSurfacesActionableCategory(t *testing.T) {
	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	rows := []job.Row{{
		ID: "job-login", State: job.StateAwaitingHuman,
		UpdatedAt: now.Add(-5 * time.Minute).Format(time.RFC3339Nano),
		Work:      work.Work{Title: "Parked on institutional login"},
	}}
	details := map[string]api.JobDetail{
		"job-login": {Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{"to": job.StateAwaitingHuman, "reason": "institutional_handoff"}}}},
	}
	snapshot := buildStatusSnapshot(rows, details, config.Config{}, now)
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Jobs) != 1 {
		t.Fatalf("groups = %+v", snapshot.Groups)
	}
	got := snapshot.Groups[0].Jobs[0]
	if got.Category != "login_required" || got.Guidance == "" {
		t.Fatalf("MCP status job = category %q guidance %q, want login_required with guidance", got.Category, got.Guidance)
	}
}

func TestActionsToolsPassThroughRPC(t *testing.T) {
	action := job.HumanAction{
		ID: 17, JobID: "job-review", Kind: "verify_identity", Status: "open",
		Detail: "/private/papio/quarantine/job-review.pdf", CreatedAt: "2026-07-15T12:00:00Z",
	}
	fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "actions.list":
			requireJSONEqual(t, params, map[string]bool{"open_only": true})
			return []job.HumanAction{action}, nil
		case "actions.resolve":
			requireJSONEqual(t, params, map[string]any{"action_id": float64(17), "verdict": "accept"})
			return ActionsResolveOutput{JobID: "job-review", State: job.StateReady}, nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	}}
	client := testMCPClient(t, defaultToolDependencies(fake))

	var listed ActionsListOutput
	callToolJSON(t, client, "papio_actions_list", map[string]any{}, &listed)
	if !reflect.DeepEqual(listed.Actions, []job.HumanAction{action}) {
		t.Fatalf("actions = %+v", listed.Actions)
	}

	var resolved ActionsResolveOutput
	callToolJSON(t, client, "papio_actions_resolve", map[string]any{"action_id": 17, "verdict": "accept"}, &resolved)
	if resolved.JobID != "job-review" || resolved.State != job.StateReady {
		t.Fatalf("resolved = %+v", resolved)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("RPC calls = %+v", fake.calls)
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
		dependencies := toolDependencies{
			caller: fake,
			now:    func() time.Time { return now },
			wait: func(_ context.Context, duration time.Duration) error {
				now = now.Add(duration)
				return nil
			},
		}
		client := testMCPClient(t, dependencies)

		var output BatchWaitOutput
		callToolJSON(t, client, "papio_batch_wait", map[string]any{"batch_id": "latest", "timeout_seconds": 10, "poll_seconds": 2}, &output)
		if !output.Settled || output.Report == nil || output.Report.Works[0].Outcome != "browser_fetched_then_imported" {
			t.Fatalf("wait output = %+v", output)
		}
		if now.Sub(time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)) != 2*time.Second || len(fake.calls) != 2 {
			t.Fatalf("clock = %s calls = %+v", now, fake.calls)
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
		dependencies := toolDependencies{
			caller: fake,
			now:    func() time.Time { return now },
			wait: func(_ context.Context, duration time.Duration) error {
				now = now.Add(duration)
				return nil
			},
		}
		client := testMCPClient(t, dependencies)

		var output BatchWaitOutput
		callToolJSON(t, client, "papio_batch_wait", map[string]any{"batch_id": "batch-deadbeef", "timeout_seconds": 1, "poll_seconds": 1}, &output)
		if output.Settled || output.Report == nil || output.Report.Works[0].Outcome != "in_progress" {
			t.Fatalf("wait output = %+v", output)
		}
		if len(fake.calls) != 2 {
			t.Fatalf("RPC calls = %+v", fake.calls)
		}
	})
}

func TestWatchToolsMapRPCParameters(t *testing.T) {
	watch := WatchOutput{
		ID: 7, Label: "Recent OA", Query: "machine learning", Filters: WatchFiltersInput{YearFrom: 2024, OAOnly: true},
		Collection: "COLLECTION", CadenceHours: 24, PerRunCap: 12, Enabled: true, CreatedAt: "2026-07-15T12:00:00Z",
	}
	fake := &fakeRPC{handler: func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "watch.add":
			requireJSONEqual(t, params, map[string]any{
				"label": "Recent OA", "query": "machine learning",
				"filters":    map[string]any{"year_from": float64(2024), "oa_only": true},
				"collection": "COLLECTION", "cadence_hours": float64(24), "per_run_cap": float64(12),
			})
			return watch, nil
		case "watch.list":
			requireJSONEqual(t, params, map[string]any{})
			return []WatchOutput{watch}, nil
		case "watch.remove":
			requireJSONEqual(t, params, map[string]any{"id": float64(7)})
			return WatchRemoveOutput{ID: 7, Removed: true}, nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	}}
	client := testMCPClient(t, defaultToolDependencies(fake))

	var added WatchOutput
	callToolJSON(t, client, "papio_watch_add", map[string]any{
		"label": "Recent OA", "query": "machine learning",
		"filters":    map[string]any{"year_from": 2024, "oa_only": true},
		"collection": "COLLECTION", "cadence_hours": 24, "per_run_cap": 12,
	}, &added)
	if !reflect.DeepEqual(added, watch) {
		t.Fatalf("added watch = %+v", added)
	}

	var listed WatchListOutput
	callToolJSON(t, client, "papio_watch_list", map[string]any{}, &listed)
	if !reflect.DeepEqual(listed.Watches, []WatchOutput{watch}) {
		t.Fatalf("listed watches = %+v", listed.Watches)
	}

	var removed WatchRemoveOutput
	callToolJSON(t, client, "papio_watch_remove", map[string]any{"id": 7}, &removed)
	if removed.ID != 7 || !removed.Removed {
		t.Fatalf("removed watch = %+v", removed)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("RPC calls = %+v", fake.calls)
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

func testMCPClient(t *testing.T, dependencies toolDependencies) *mcp.ClientSession {
	return testMCPClientForSystem(t, nil, dependencies)
}

func testMCPClientForSystem(t *testing.T, system *bootstrap.System, dependencies toolDependencies) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer(system, dependencies).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "papio-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callToolJSON(t *testing.T, client *mcp.ClientSession, name string, arguments, target any) {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		data, _ := json.Marshal(result.Content)
		t.Fatalf("%s error: %s", name, data)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s result %s: %v", name, data, err)
	}
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
