// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/triage"
	"papio/internal/update"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
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

func TestRouterCapturesDisabledReturnsEmptyResults(t *testing.T) {
	system := testSystem(t)
	system.Captures = nil
	router := Router(system)

	var rows []json.RawMessage
	if rpcErr := callMethod(t, router, "adapter.captures.list", struct{}{}, &rows); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("capture list = %#v, want non-nil empty list", rows)
	}

	var purged CapturePurgeResult
	if rpcErr := callMethod(t, router, "adapter.captures.purge", map[string]string{"host": "provider.example.com"}, &purged); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if purged.Removed != 0 {
		t.Fatalf("removed = %d, want 0", purged.Removed)
	}
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

func TestTriageSnapshotCountsAndDismiss(t *testing.T) {
	system := testSystem(t)
	watched, err := system.Watches.Create(context.Background(), watch.CreateInput{
		Query: "triage API", Filters: watch.Filters{YearFrom: 2020, OAOnly: true},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Watches.RecordDigest(context.Background(), watched.ID, time.Now(), []watch.DigestEntry{{
		WorkKey: "10.1000/triage-api", Title: "Triage API", DOI: "10.1000/triage-api", Abstract: "Context",
	}}); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	var snapshot triage.Snapshot
	if rpcErr := callMethod(t, router, "triage.snapshot", map[string]any{"limit": 100}, &snapshot); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if snapshot.Schema != triage.SchemaVersion || len(snapshot.Items) != 1 || snapshot.Items[0].WatchHit == nil {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	var outcome triageDecideResult
	if rpcErr := callMethod(t, router, "triage.decide", map[string]any{
		"item_id": snapshot.Items[0].ID, "op": "dismiss", "watch_scope": "all",
	}, &outcome); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if outcome.Outcome != "applied" {
		t.Fatalf("dismiss outcome = %+v", outcome)
	}
	var counts triage.Counts
	if rpcErr := callMethod(t, router, "triage.counts", map[string]any{}, &counts); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if counts.PendingTotal != 0 || counts.WatchHits != 0 {
		t.Fatalf("counts after dismiss = %+v", counts)
	}
}

type preflightOnlyCLI struct {
	zotio.CLI
	result *zotio.PreflightResult
}

func (c preflightOnlyCLI) Preflight(context.Context) (*zotio.PreflightResult, error) {
	return c.result, nil
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

func TestRouterSubmitV2ReportsExistingAndHonorsForce(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "request_api_dedup_01",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/api-dedup"},
	}
	var first SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2", map[string]any{"request": request}, &first); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if first.JobID == "" || first.Existing {
		t.Fatalf("first submission = %+v, want new job", first)
	}
	request.RequestID = "request_api_dedup_02"
	var existing SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2", map[string]any{"request": request}, &existing); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !existing.Existing || existing.JobID != first.JobID {
		t.Fatalf("deduplicated submission = %+v, want existing job %q", existing, first.JobID)
	}
	var forced SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2", map[string]any{"request": request, "force": true}, &forced); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if forced.Existing || forced.JobID == first.JobID {
		t.Fatalf("forced submission = %+v, want fresh job", forced)
	}
	rows, err := system.Jobs.List(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("jobs = %+v, want two jobs after force", rows)
	}
}

func TestRouterSubmitEnvelopeAutoImportOverride(t *testing.T) {
	system := testSystem(t)
	system.App.Config.Zotio.AutoImport = true
	router := Router(system)
	disabled := false
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "request_api_auto_import",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/auto-import"},
	}
	var submitted SubmitResult
	params := struct {
		Request    protocol.WorkRequest `json:"request"`
		AutoImport *bool                `json:"auto_import"`
	}{Request: request, AutoImport: &disabled}
	if rpcErr := callMethod(t, router, "acquire.submit", params, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	row, err := system.Jobs.Get(context.Background(), submitted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Policy.AutoImport {
		t.Fatal("explicit auto_import=false did not override config default")
	}
}

func TestRouterRetryAndStrictEmptyParams(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "request_api_retry", work.Work{DOI: "10.1000/retry"}, "", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any"}, nil, job.PrincipalUnknown)
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

func TestRouterPingIncludesBrowserSession(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	var status struct {
		Status             string `json:"status"`
		Version            string `json:"version"`
		ExtensionConnected bool   `json:"extension_connected"`
		ExtensionVersion   string `json:"extension_version"`
	}
	if rpcErr := callMethod(t, router, "ping", struct{}{}, &status); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if status.Status != "ok" || status.Version != Version || status.ExtensionConnected || status.ExtensionVersion != "" {
		t.Fatalf("initial ping = %+v", status)
	}

	hello := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-status","seq":0,"payload":{"extension_version":"1.2.3"}}`)
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{"messages": []json.RawMessage{hello}}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if rpcErr := callMethod(t, router, "ping", struct{}{}, &status); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !status.ExtensionConnected || status.ExtensionVersion != "1.2.3" {
		t.Fatalf("connected ping = %+v", status)
	}
}

func TestRouterPingIncludesUpdatesOnlyWhenEnabled(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	var disabled map[string]json.RawMessage
	if rpcErr := callMethod(t, router, "ping", struct{}{}, &disabled); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, ok := disabled["update_available"]; ok {
		t.Fatalf("disabled papio update status = %s", disabled)
	}
	if _, ok := disabled["zotio_update_available"]; ok {
		t.Fatalf("disabled zotio update status = %s", disabled)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v99.0.0","html_url":"https://example.test/releases/v99.0.0"}`))
	}))
	defer server.Close()
	system.Updates = update.NewWithOptions(update.Options{
		DataDir:     system.Config.DataDir,
		ReleasesURL: server.URL,
		Client:      server.Client(),
	})
	papioCache := []byte(`{"latest_version":"99.0.0","url":"https://example.test/releases/v99.0.0"}`)
	if err := os.WriteFile(filepath.Join(system.Config.DataDir, "update-cache.json"), papioCache, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := []byte(`{"latest_version":"1.1.0","url":"https://example.test/zotio","installed_version":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(system.Config.DataDir, "update-cache-zotio.json"), cache, 0o600); err != nil {
		t.Fatal(err)
	}
	var enabled struct {
		UpdateAvailable      *bool  `json:"update_available"`
		LatestVersion        string `json:"latest_version"`
		ZotioUpdateAvailable *bool  `json:"zotio_update_available"`
		ZotioLatestVersion   string `json:"zotio_latest_version"`
	}
	if rpcErr := callMethod(t, router, "ping", struct{}{}, &enabled); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if enabled.UpdateAvailable == nil || !*enabled.UpdateAvailable || enabled.LatestVersion != "99.0.0" {
		t.Fatalf("enabled update status = %+v", enabled)
	}
	if enabled.ZotioUpdateAvailable == nil || !*enabled.ZotioUpdateAvailable || enabled.ZotioLatestVersion != "1.1.0" {
		t.Fatalf("enabled Zotio update status = %+v", enabled)
	}
}

func TestRouterPingReturnsCachedUpdateBeforeRefresh(t *testing.T) {
	system := testSystem(t)
	refreshStarted := make(chan struct{}, 1)
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseRefresh:
		case <-time.After(2 * time.Second):
		}
		_, _ = w.Write([]byte(`{"tag_name":"v99.0.0","html_url":"https://example.test/releases/v99.0.0"}`))
	}))
	defer server.Close()
	defer close(releaseRefresh)
	system.Updates = update.NewWithOptions(update.Options{
		DataDir:     system.Config.DataDir,
		ReleasesURL: server.URL,
		Client:      server.Client(),
	})

	start := time.Now()
	if rpcErr := callMethod(t, Router(system), "ping", struct{}{}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("ping blocked on update refresh for %s", elapsed)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("ping did not trigger an update refresh")
	}
}

func TestRouterPingStartsOnlyOneBackgroundUpdateRefresh(t *testing.T) {
	system := testSystem(t)
	refreshStarted := make(chan struct{}, 1)
	extraRefresh := make(chan struct{}, 1)
	releaseRefresh := make(chan struct{})
	released := false
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if refreshes.Add(1) > 1 {
			select {
			case extraRefresh <- struct{}{}:
			default:
			}
		}
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
		<-releaseRefresh
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer func() {
		if !released {
			close(releaseRefresh)
		}
	}()
	system.Updates = update.NewWithOptions(update.Options{
		DataDir:     system.Config.DataDir,
		ReleasesURL: server.URL,
		Client:      server.Client(),
	})
	router := Router(system)

	if rpcErr := callMethod(t, router, "ping", struct{}{}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("ping did not trigger an update refresh")
	}

	pingDone := make(chan *ipc.RPCError, 10)
	for range 10 {
		go func() {
			_, rpcErr := router.Handle(context.Background(), ipc.Request{Method: "ping", Params: json.RawMessage(`{}`)})
			pingDone <- rpcErr
		}()
	}
	// Give every rapid ping time to observe the active refresh before the
	// blocked checker is released.
	time.Sleep(50 * time.Millisecond)
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes started = %d, want 1", refreshes.Load())
	}
	close(releaseRefresh)
	released = true
	for range 10 {
		select {
		case rpcErr := <-pingDone:
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
		case <-time.After(time.Second):
			t.Fatal("ping did not return after refresh completed")
		}
	}
	select {
	case <-extraRefresh:
		t.Fatal("rapid pings started an additional queued refresh")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestZotioPreflightCachesInstalledVersion(t *testing.T) {
	system := testSystem(t)
	system.Zotio = &zotio.Service{
		CLI: preflightOnlyCLI{result: &zotio.PreflightResult{Version: "1.2.3"}},
	}
	var result zotio.PreflightResult
	if rpcErr := callMethod(t, Router(system), "zotio.preflight", struct{}{}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := update.NewZotio(system.Config.DataDir).InstalledVersion(); got != "1.2.3" {
		t.Fatalf("cached Zotio version = %q", got)
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

	hello := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-1","seq":0,"payload":{"extension_version":"1.0.0"}}`)
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
	bad := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"x","seq":0,"payload":{"extension_version":"1.0.0"}}`)
	rpcErr := callMethod(t, router, "browser.sync", map[string]any{"messages": []json.RawMessage{bad}}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("invalid frame error = %+v, want invalid_argument", rpcErr)
	}
}

func TestRouterActionsOpenQueuesCompatibleHandoff(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.DataDir = t.TempDir()
	system, err := bootstrap.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	id, err := system.Jobs.CreateRequest(ctx, "request_api_focus", work.Work{DOI: "10.1000/focus"}, "", "", job.Policy{
		AccessMode: config.ModeDelegated, DesiredVersion: "any",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := system.Jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_handoff", app.OABrowserHandoffActionDetail("https://oa.example.test/focus.pdf")); err != nil {
		t.Fatal(err)
	}
	router := Router(system)

	var unavailable ActionsOpenResult
	if rpcErr := callMethod(t, router, "actions.open", map[string]any{"job_ids": []string{id}}, &unavailable); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if unavailable.SessionLive || unavailable.Queued != 0 {
		t.Fatalf("no-session result = %+v, want queued:0 session_live:false", unavailable)
	}

	// A legacy native host (no session_id) is indistinguishable from any other
	// legacy host, and they all share one session identity — so a stale
	// pre-0.8.0 host can be the caller behind a newer host's version metadata.
	// Its parser fails closed on an unknown type and drops the whole session,
	// so handoff_focus must never be queued for a legacy session.
	legacyHello := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-focus-000","seq":0,"payload":{"extension_version":"0.8.0"}}`)
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{"messages": []json.RawMessage{legacyHello}}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var legacy ActionsOpenResult
	if rpcErr := callMethod(t, router, "actions.open", map[string]any{"job_ids": []string{id}}, &legacy); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if legacy.SessionLive || legacy.Queued != 0 {
		t.Fatalf("legacy-session result = %+v, want queued:0 session_live:false so the CLI takes the OS fallback", legacy)
	}
	var legacyPolled struct {
		Outbound []json.RawMessage `json:"outbound"`
	}
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{}, &legacyPolled); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	for _, raw := range legacyPolled.Outbound {
		if msg, err := protocol.DecodeBrowserMessage(raw); err == nil && msg.Type == protocol.MsgHandoffFocus {
			t.Fatal("legacy holder received handoff_focus; its parser would drop the whole session")
		}
	}

	const sessionID = "aaaabbbbccccddddeeeeffff00001111"
	hello := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-focus-001","seq":0,"payload":{"extension_version":"0.8.0"}}`)
	if rpcErr := callMethod(t, router, "browser.sync",
		map[string]any{"session_id": sessionID, "messages": []json.RawMessage{hello}}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var queued ActionsOpenResult
	if rpcErr := callMethod(t, router, "actions.open", map[string]any{"job_ids": []string{id}}, &queued); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !queued.SessionLive || queued.Queued != 1 {
		t.Fatalf("compatible-session result = %+v, want queued:1 session_live:true", queued)
	}
	var polled struct {
		Outbound []json.RawMessage `json:"outbound"`
	}
	if rpcErr := callMethod(t, router, "browser.sync", map[string]any{"session_id": sessionID}, &polled); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(polled.Outbound) != 1 {
		t.Fatalf("poll outbound = %d frames, want one handoff_focus", len(polled.Outbound))
	}
	msg, err := protocol.DecodeBrowserMessage(polled.Outbound[0])
	if err != nil || msg.Type != protocol.MsgHandoffFocus || msg.JobID != id {
		t.Fatalf("focus outbound = %+v, %v", msg, err)
	}
}

func TestRouterBrowserSessionsAndClaim(t *testing.T) {
	system := testSystem(t)
	router := Router(system)

	hello := json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-2","seq":0,"payload":{"extension_version":"1.2.3"}}`)
	if rpcErr := callMethod(t, router, "browser.sync",
		map[string]any{"session_id": "aaaabbbbccccddddeeeeffff00001111", "messages": []json.RawMessage{hello}}, nil); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var result struct {
		Sessions []struct {
			ID               string `json:"id"`
			ExtensionVersion string `json:"extension_version"`
			Holder           bool   `json:"holder"`
		} `json:"sessions"`
		DeniedHellos int `json:"denied_hellos"`
	}
	if rpcErr := callMethod(t, router, "browser.sessions", struct{}{}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(result.Sessions) != 1 || !result.Sessions[0].Holder || result.Sessions[0].ExtensionVersion != "1.2.3" {
		t.Fatalf("sessions = %+v", result)
	}
	rpcErr := callMethod(t, router, "browser.claim", map[string]string{"session_id": "nonexistent"}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("claim unknown session = %+v, want invalid_argument", rpcErr)
	}
}

func TestRouterResolveIdentityReviewAction(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "request_api_review", work.Work{DOI: "10.1000/review"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
		{job.StateValidating, job.StateNeedsReview},
	} {
		if err := system.Jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	actionID, err := system.Jobs.OpenHumanAction(ctx, id, "verify_identity", "local quarantine file: /tmp/review.pdf")
	if err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	var result struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if rpcErr := callMethod(t, router, "actions.resolve", map[string]any{"action_id": actionID, "verdict": "reject"}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.JobID != id || result.State != job.StateCancelled {
		t.Fatalf("resolve result = %+v", result)
	}
	if rpcErr := callMethod(t, router, "actions.resolve", map[string]any{"action_id": actionID, "verdict": "maybe"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("invalid verdict error = %+v", rpcErr)
	}

	wrongID, err := system.Jobs.OpenHumanAction(ctx, id, "manual_download", "not an identity review")
	if err != nil {
		t.Fatal(err)
	}
	if rpcErr := callMethod(t, router, "actions.resolve", map[string]any{"action_id": wrongID, "verdict": "accept"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("wrong action kind error = %+v", rpcErr)
	}
}

func TestRouterDiscoverySearchValidatesParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []any{
		map[string]any{},
		map[string]any{"query": " \t"},
		map[string]any{"query": "climate", "unexpected": true},
	} {
		if rpcErr := callMethod(t, router, "discovery.search", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("params %v: RPC error = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestRouterDiscoverySearchAcceptsCitationSnowballWithoutQuery(t *testing.T) {
	system := testSystem(t)
	system.Discovery = nil
	router := Router(system)
	params := map[string]any{
		"cites": "10.1000/seed", "cited_by": "10.1000/backward", "related_to": "10.1000/related",
	}
	if rpcErr := callMethod(t, router, "discovery.search", params, nil); rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("citation snowball params RPC error = %+v, want precondition_failed", rpcErr)
	}
}

func TestRouterAcquireReportJoinsManifestAgainstLiveJobState(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "batch-dededededededededededededededede-11111111111111111111111111111111", work.Work{DOI: "10.1000/report"}, "", "Reading", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", Collection: "Reading",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.RecordEvent(ctx, id, "zotio.auto_import", map[string]any{
		"status": "applied", "parent_key": "PA123", "attachment_key": "AT456",
	}); err != nil {
		t.Fatal(err)
	}
	manifest := &batch.Manifest{
		SchemaVersion: batch.SchemaVersion, ID: "batch-dededededededededededededededede", CreatedAt: "2026-07-15T12:00:00Z", Collection: "Reading",
		Works: []batch.ManifestWork{{
			RequestID: "batch-dededededededededededededededede-11111111111111111111111111111111", JobID: id, Status: "submitted",
			Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "batch-dededededededededededededededede-11111111111111111111111111111111", Identifiers: &protocol.Identifiers{DOI: "10.1000/report"}, Collection: "Reading"},
		}},
	}
	if err := batch.Write(system.Config.DataDir, manifest); err != nil {
		t.Fatal(err)
	}
	var report batch.Report
	if rpcErr := callMethod(t, Router(system), "acquire.report", AcquireReportParams{BatchID: manifest.ID}, &report); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(report.Works) != 1 || report.Works[0].Outcome != "imported" || report.Works[0].ParentKey != "PA123" || report.Works[0].AttachmentKey != "AT456" {
		t.Fatalf("report = %+v", report)
	}
}

// acquire.report must not collapse every failure into not_found: a well-formed
// but missing batch is not_found, while a malformed batch ID is invalid_argument
// so clients can distinguish a missing resource from a bad request.
func TestRouterAcquireReportClassifiesErrors(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	cases := []struct {
		name    string
		batchID string
		want    string
	}{
		{name: "missing but valid id", batchID: "batch-00000000000000000000000000000000", want: "not_found"},
		{name: "latest with none present", batchID: "latest", want: "not_found"},
		{name: "malformed id", batchID: "not-a-batch", want: "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := callMethod(t, router, "acquire.report", AcquireReportParams{BatchID: tc.batchID}, nil)
			if rpcErr == nil {
				t.Fatalf("expected %s error, got nil", tc.want)
			}
			if rpcErr.Code != tc.want {
				t.Fatalf("code = %q, want %q (message %q)", rpcErr.Code, tc.want, rpcErr.Message)
			}
		})
	}
}

func TestRouterWatchAddListAndRemove(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	input := watch.CreateInput{
		Query: "evidence synthesis", Filters: watch.Filters{YearFrom: 2020, OAOnly: true},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 3,
	}
	var created watch.Watch
	if rpcErr := callMethod(t, router, "watch.add", input, &created); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if created.ID == 0 || created.Label != input.Query || created.PerRunCap != 3 {
		t.Fatalf("created watch = %+v", created)
	}
	var listed []watch.Watch
	if rpcErr := callMethod(t, router, "watch.list", struct{}{}, &listed); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed watches = %+v", listed)
	}
	var removed WatchRemoveResult
	if rpcErr := callMethod(t, router, "watch.remove", watch.IDInput{ID: created.ID}, &removed); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !removed.Removed || removed.ID != created.ID {
		t.Fatalf("remove result = %+v", removed)
	}
}

func TestRouterWatchDigestAcquireClassifiesMissingResources(t *testing.T) {
	system := testSystem(t)
	created, err := system.Watches.Create(context.Background(), watch.CreateInput{
		Kind: watch.KindDiscovery, Mode: watch.ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Watches.RecordDigest(context.Background(), created.ID, time.Now(), []watch.DigestEntry{{
		WorkKey: "10.1000/digest", Title: "Digest",
	}}); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{name: "watch", params: map[string]any{"id": created.ID + 1}},
		{name: "key", params: map[string]any{"id": created.ID, "keys": []string{"missing"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := callMethod(t, router, "watch.digest_acquire", tc.params, nil)
			if rpcErr == nil || rpcErr.Code != "not_found" {
				t.Fatalf("watch.digest_acquire error = %#v, want not_found", rpcErr)
			}
		})
	}
}

func TestWatchFailureCarriesSafeRunnerDetail(t *testing.T) {
	_, rpcErr := watchFailure(errors.New("discovery search: discovery: invalid OpenAlex response: response exceeds configured limit"))
	if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Message != "watch execution failed" || rpcErr.Detail == nil {
		t.Fatalf("watchFailure() = %#v", rpcErr)
	}
	if rpcErr.Detail.ErrorClass != "watch_execution_failed" || rpcErr.Detail.ErrorHint != "OpenAlex response exceeds configured limit" {
		t.Fatalf("watch failure detail = %#v", rpcErr.Detail)
	}
}

func TestZotioFailureCarriesOnlySafeTaxonomyDetail(t *testing.T) {
	_, rpcErr := zotioFailure(errors.New("zotio stderr: unknown item field at https://zotero.example.test/users/42 /Users/reader/private.json"))
	if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Message != "operation failed" || rpcErr.Detail == nil {
		t.Fatalf("zotioFailure() = %#v", rpcErr)
	}
	if rpcErr.Detail.ErrorClass != "zotero_field_validation" || rpcErr.Detail.ErrorHint != "unknown item field" {
		t.Fatalf("zotio failure detail = %#v", rpcErr.Detail)
	}
	encoded, err := json.Marshal(rpcErr.Detail)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || string(encoded) == "null" || string(encoded) == "[]" {
		t.Fatalf("encoded detail = %s", encoded)
	}
	if strings.Contains(string(encoded), "zotero.example.test") || strings.Contains(string(encoded), "/Users/reader") {
		t.Fatalf("zotio failure leaked private detail: %s", encoded)
	}
}

func TestParseFailuresSinceRejectsNegativeLookbacks(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"-1", "-1d", "-1h"} {
		t.Run(value, func(t *testing.T) {
			_, err := parseFailuresSince(value, now)
			if err == nil {
				t.Fatalf("parseFailuresSince(%q) succeeded", value)
			}
			if err.Error() != "since must be a non-negative duration or RFC3339 timestamp" {
				t.Fatalf("parseFailuresSince(%q) error = %q", value, err)
			}
		})
	}
}

// fakeDiscoverySource is a minimal discovery.Source double local to this
// package: discovery's own fakeSource is unexported and lives in a
// different package, so a handler-level test needs its own.
type fakeDiscoverySource struct {
	name  string
	works []discovery.DiscoveredWork
	err   error
}

func (f fakeDiscoverySource) Name() string { return f.name }

func (f fakeDiscoverySource) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return f.works, f.err
}

// The regression: the per-source log loop only ran on the partial-success
// branch, so a backend that failed on every request logged nothing at all —
// backwards from what an operator needs — and the top-level RPC error named
// only whichever backend errors.As found first inside the joined error,
// silently dropping the rest even though every backend failed.
func TestDiscoverySearchLogsAndNamesEveryBackendOnTotalFailure(t *testing.T) {
	system := testSystem(t)
	system.Discovery = discovery.NewMulti(
		fakeDiscoverySource{name: "openalex", err: errors.New("boom one")},
		fakeDiscoverySource{name: "semanticscholar", err: errors.New("boom two")},
	)
	router := Router(system)

	orig := log.Writer()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(orig) })

	rpcErr := callMethod(t, router, "discovery.search", map[string]any{"query": "anything"}, nil)
	if rpcErr == nil {
		t.Fatal("expected a hard error when every discovery backend fails")
	}
	output := logged.String()
	if !strings.Contains(output, "openalex") || !strings.Contains(output, "boom one") {
		t.Fatalf("total failure did not log the openalex backend: %s", output)
	}
	if !strings.Contains(output, "semanticscholar") || !strings.Contains(output, "boom two") {
		t.Fatalf("total failure did not log the semanticscholar backend: %s", output)
	}
	if !strings.Contains(rpcErr.Message, "openalex") || !strings.Contains(rpcErr.Message, "boom one") {
		t.Fatalf("top-level error dropped the openalex cause: %s", rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "semanticscholar") || !strings.Contains(rpcErr.Message, "boom two") {
		t.Fatalf("top-level error dropped the semanticscholar cause: %s", rpcErr.Message)
	}
}

// The partial-success branch already logged one line per failed backend
// before this fix; this pins that it still does, alongside a healthy
// sibling, and does not also log the healthy backend.
func TestDiscoverySearchLogsPerSourceOnPartialFailure(t *testing.T) {
	system := testSystem(t)
	system.Discovery = discovery.NewMulti(
		fakeDiscoverySource{name: "openalex", works: []discovery.DiscoveredWork{
			{Work: work.Work{Title: "a usable result", DOI: "10.1000/usable-result"}},
		}},
		fakeDiscoverySource{name: "semanticscholar", err: errors.New("boom semanticscholar")},
	)
	router := Router(system)

	orig := log.Writer()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(orig) })

	if rpcErr := callMethod(t, router, "discovery.search", map[string]any{"query": "a usable result"}, nil); rpcErr != nil {
		t.Fatalf("discovery.search returned a hard error despite a usable backend: %+v", rpcErr)
	}
	output := logged.String()
	if !strings.Contains(output, "semanticscholar") || !strings.Contains(output, "boom semanticscholar") {
		t.Fatalf("partial failure did not log the broken backend: %s", output)
	}
	if strings.Contains(output, "warning: discovery backend openalex") {
		t.Fatalf("partial failure logged the healthy backend as failed too: %s", output)
	}
}

// jobs.repair_awaiting_human is the ONLY way a consumer can move an orphaned
// parked job (jobs.retry refuses parked jobs; actions.open needs an open handoff
// action). It must stay orphan-only: a job that still has an open action is
// reported, never repaired, because the verb takes no action ids and so cannot
// have read what it would be closing.
func TestRepairAwaitingHumanIsOrphanOnly(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	ctx := context.Background()

	park := func(t *testing.T, requestID string) string {
		t.Helper()
		id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID},
			"", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20},
			nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		for _, edge := range [][2]string{
			{job.StateQueued, job.StateResolving},
			{job.StateResolving, job.StateAwaitingHuman},
		} {
			if err := system.Jobs.Transition(ctx, id, edge[0], edge[1], nil); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}

	orphan := park(t, "repair_orphan")
	var result RepairResult
	if rpcErr := callMethod(t, router, "jobs.repair_awaiting_human", map[string]string{"job_id": orphan}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !result.Repaired || result.Outcome != "repaired" || result.State != job.StateResolving {
		t.Fatalf("orphan repair = %+v, want repaired to resolving", result)
	}
	row, err := system.Jobs.Get(ctx, orphan)
	if err != nil || row.State != job.StateResolving {
		t.Fatalf("orphan job = %+v, %v; want resolving", row, err)
	}

	withAction := park(t, "repair_with_action")
	if _, err := system.Jobs.OpenHumanAction(ctx, withAction, "openurl_handoff", "institutional handoff"); err != nil {
		t.Fatal(err)
	}
	result = RepairResult{}
	if rpcErr := callMethod(t, router, "jobs.repair_awaiting_human", map[string]string{"job_id": withAction}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Repaired || result.Outcome != "has_open_actions" {
		t.Fatalf("repair with an open action = %+v, want has_open_actions and no repair", result)
	}
	if row, err := system.Jobs.Get(ctx, withAction); err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job with an open action was disturbed: %+v, %v", row, err)
	}

	// A job that is not parked at all is reported, not errored: a consumer
	// reconciling a cohort should not have to distinguish "already moved on"
	// from a genuine failure.
	result = RepairResult{}
	if rpcErr := callMethod(t, router, "jobs.repair_awaiting_human", map[string]string{"job_id": orphan}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Repaired || result.Outcome != "not_parked" {
		t.Fatalf("repair of an unparked job = %+v, want not_parked", result)
	}
}

// The _v2 list methods exist for one reason: `truncated` must be a proof. A page
// that exactly fills the limit with nothing behind it reports false.
func TestListV2ReportsTruncationAsAProof(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	ctx := context.Background()
	for i := range 3 {
		id, err := system.Jobs.CreateRequest(ctx, fmt.Sprintf("wr_page_%02d", i),
			work.Work{DOI: fmt.Sprintf("10.1000/page-%02d", i)}, "", "",
			job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20},
			nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		limit         int
		wantJobs      int
		wantTruncated bool
	}{
		{limit: 2, wantJobs: 2, wantTruncated: true},
		{limit: 3, wantJobs: 3, wantTruncated: false},
		{limit: 4, wantJobs: 3, wantTruncated: false},
	} {
		var jobsPage JobsPage
		if rpcErr := callMethod(t, router, "jobs.list_v2", map[string]any{"limit": tc.limit}, &jobsPage); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if len(jobsPage.Jobs) != tc.wantJobs || jobsPage.Truncated != tc.wantTruncated {
			t.Fatalf("jobs.list_v2 limit %d = %d rows truncated %t, want %d/%t",
				tc.limit, len(jobsPage.Jobs), jobsPage.Truncated, tc.wantJobs, tc.wantTruncated)
		}
		var actionsPage ActionsPage
		if rpcErr := callMethod(t, router, "actions.list_v2", map[string]any{"limit": tc.limit}, &actionsPage); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if len(actionsPage.Actions) != tc.wantJobs || actionsPage.Truncated != tc.wantTruncated {
			t.Fatalf("actions.list_v2 limit %d = %d rows truncated %t, want %d/%t",
				tc.limit, len(actionsPage.Actions), actionsPage.Truncated, tc.wantJobs, tc.wantTruncated)
		}
	}
}
