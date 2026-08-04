// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/retraction"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
)

func newBridge(t *testing.T) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	artifacts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.DataDir = data
	cfg.Browser.ExtensionID = strings.Repeat("a", 32)
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Browser.ActionExpirySeconds = 1800
	jobs := &job.Store{S: db}
	watches := watch.NewStore(db)
	triageService := triage.New(db, watches, jobs)
	previewServer := preview.New(jobs)
	t.Cleanup(func() {
		if err := previewServer.Shutdown(context.Background()); err != nil {
			t.Errorf("close preview: %v", err)
		}
	})
	captureStore := captures.New(data, captures.Retention{MaxPerHost: cfg.Captures.MaxPerHost, MaxAge: time.Duration(cfg.Captures.MaxAgeDays) * 24 * time.Hour})
	svc := app.New(cfg, jobs, artifacts, nil)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 3},
			Text:       pdf.TextReport{Chars: 4000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
	return NewBridge(jobs, svc, triageService, &watch.Runner{Store: watches}, previewServer, captureStore, cfg, "0.1.0-test"), jobs, cfg, data
}

func handoffWork() work.Work {
	return work.Work{DOI: "10.1002/example.42", Title: "An Institutional Paper", Authors: []string{"Lovelace, Ada"}, Year: 2024}
}

// park drives a fresh job into awaiting_human with an open openurl_handoff
// action, exactly as the app's exhaustion routing does.
func park(t *testing.T, jobs *job.Store, reqID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, w, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	return id
}

func parkInstitutional(t *testing.T, jobs *job.Store, reqID string, w work.Work, resolverProfile string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, w, "", "", job.Policy{
		AccessMode: config.ModeDelegated, DesiredVersion: "any", Resolver: resolverProfile, FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available",
		job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id
}

var inSeq int64

func inFrame(t *testing.T, typ, jobID string, payload any) json.RawMessage {
	t.Helper()
	inSeq++
	env := map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     typ,
		"msg_id":   "client-msg-0001",
		"seq":      inSeq,
		"payload":  payload,
	}
	if jobID != "" {
		env["job_id"] = jobID
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// testSessionID is the default native-host session identity for tests that
// exercise a single browser.
const testSessionID = "sess-primary-000000000000000000000000"

// sync runs one Sync batch and decodes every outbound frame (asserting each is a
// valid papio-browser frame), returning the decoded messages and their raw bytes.
func runSync(t *testing.T, b *Bridge, frames ...json.RawMessage) ([]*protocol.BrowserMessage, []json.RawMessage) {
	t.Helper()
	return runSyncAs(t, b, testSessionID, frames...)
}

// runSyncAs is runSync for a specific native-host session.
func runSyncAs(t *testing.T, b *Bridge, sessionID string, frames ...json.RawMessage) ([]*protocol.BrowserMessage, []json.RawMessage) {
	t.Helper()
	out, err := b.Sync(context.Background(), sessionID, false, frames)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs := make([]*protocol.BrowserMessage, 0, len(out))
	for _, raw := range out {
		m, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatalf("outbound frame failed protocol decode: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, out
}

func hello() json.RawMessage {
	return json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-1","seq":0,"payload":{"extension_version":"1.2.3"}}`)
}

func helloWithAdapterVersions(t *testing.T, extensionVersion string, adapterVersions map[string]string) json.RawMessage {
	t.Helper()
	return inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": extensionVersion,
		"adapter_versions":  adapterVersions,
	})
}

func firstOfType(msgs []*protocol.BrowserMessage, typ string) *protocol.BrowserMessage {
	for _, m := range msgs {
		if m.Type == typ {
			return m
		}
	}
	return nil
}

func countType(msgs []*protocol.BrowserMessage, typ string) int {
	n := 0
	for _, m := range msgs {
		if m.Type == typ {
			n++
		}
	}
	return n
}

func pageCapturePayload(t *testing.T, html []byte) protocol.PageCapturePayload {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(html); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return protocol.PageCapturePayload{
		Host:           "sagepub.com",
		Scenario:       "observed",
		AdapterID:      "sage",
		AdapterVersion: "2026.07.27",
		Encoding:       "gzip+base64",
		Bytes:          int64(len(html)),
		Body:           base64.StdEncoding.EncodeToString(compressed.Bytes()),
	}
}

func TestPageCaptureDisabledDoesNotStore(t *testing.T) {
	b, _, cfg, data := newBridge(t)
	cfg.Captures.Enabled = false
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, cfg, b.Version)
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgPageCapture, "", pageCapturePayload(t, []byte("<html>disabled</html>"))))

	listed, err := b.captureStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("stored captures = %#v, want none when disabled", listed)
	}
	if _, err := os.Stat(filepath.Join(data, "captures")); !os.IsNotExist(err) {
		t.Fatalf("disabled capture intake created a captures directory: %v", err)
	}
}

func TestPageCaptureContentFailureKeepsSession(t *testing.T) {
	cases := []struct {
		name    string
		payload protocol.PageCapturePayload
	}{
		{
			name: "corrupt gzip",
			payload: protocol.PageCapturePayload{
				Host: "sagepub.com", Scenario: "observed", Encoding: "gzip+base64", Bytes: 7,
				Body: base64.StdEncoding.EncodeToString([]byte("not-gzip")),
			},
		},
		{
			name: "length mismatch",
			payload: func() protocol.PageCapturePayload {
				payload := pageCapturePayload(t, []byte("<html>mismatch</html>"))
				payload.Bytes++
				return payload
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _, _ := newBridge(t)
			runSync(t, b, hello())
			runSync(t, b, inFrame(t, protocol.MsgPageCapture, "", tc.payload))

			runSync(t, b, inFrame(t, protocol.MsgPageCapture, "", pageCapturePayload(t, []byte("<html>survived</html>"))))
			listed, err := b.captureStore.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Size != int64(len("<html>survived</html>")) {
				t.Fatalf("captures after rejected content = %#v, want only follow-up capture", listed)
			}
		})
	}
}

func TestPageCaptureRequestDeliveredCorrelatedStoredAndBusy(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())

	resultCh := make(chan CaptureResult, 1)
	go func() {
		resultCh <- b.Capture(context.Background(), CaptureRequest{
			URL: "https://sagepub.com/article/42", Provider: "sage", Scenario: "success",
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		queued := len(b.pendingCaptures) == 1
		b.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture request was not queued")
		}
		time.Sleep(time.Millisecond)
	}

	msgs, _ := runSync(t, b)
	directive := firstOfType(msgs, protocol.MsgPageCaptureRequest)
	if directive == nil {
		t.Fatal("sync did not deliver page_capture_request")
	}
	request := directive.Payload.(*protocol.PageCaptureRequestPayload)
	if request.URL != "https://sagepub.com/article/42" || request.Provider != "sage" || request.Scenario != "success" {
		t.Fatalf("directive payload = %#v", request)
	}
	if busy := b.Capture(context.Background(), CaptureRequest{
		URL: "https://sagepub.com/other", Provider: "sage", Scenario: "drift",
	}); busy.Outcome != "busy" {
		t.Fatalf("second capture outcome = %q, want busy", busy.Outcome)
	}

	content := pageCapturePayload(t, []byte("<html>captured fixture</html>"))
	content.Scenario = "success"
	content.AdapterID = "sage"
	runSync(t, b,
		inFrame(t, protocol.MsgPageCapture, "", content),
		inFrame(t, protocol.MsgPageCaptureRequestResult, "", protocol.PageCaptureRequestResultPayload{
			RequestID: request.RequestID, Outcome: "captured",
		}),
	)
	select {
	case result := <-resultCh:
		if result.Outcome != "captured" || result.RequestID != request.RequestID || result.Path == "" {
			t.Fatalf("capture result = %#v", result)
		}
		if _, err := os.Stat(result.Path); err != nil {
			t.Fatalf("stored capture path: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture result did not correlate")
	}
}
func TestJobScopedPageCaptureRecordsEvent(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := park(t, jobs, "wr_page_capture_event", handoffWork())
	payload := pageCapturePayload(t, []byte("<html>fixture</html>"))
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgPageCapture, jobID, payload))

	listed, err := b.captureStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("stored captures = %#v, want one", listed)
	}
	events, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	for _, event := range events {
		if event["kind"] == "browser.page_capture" {
			receipt, _ = event["detail"].(map[string]any)
			break
		}
	}
	if receipt == nil {
		t.Fatalf("job events = %#v, want page capture receipt", events)
	}
	if receipt["host"] != payload.Host || receipt["scenario"] != payload.Scenario ||
		receipt["adapter_id"] != payload.AdapterID || receipt["adapter_version"] != payload.AdapterVersion ||
		receipt["path"] != listed[0].Path || receipt["size_bytes"] != float64(len("<html>fixture</html>")) {
		t.Fatalf("page capture receipt = %#v", receipt)
	}
}

func TestHelloIsAcknowledged(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello())
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("no hello_ack in %v", msgs)
	}
	if msgs[0].Seq != 1 {
		t.Fatalf("first outbound seq = %d, want 1", msgs[0].Seq)
	}
}

func TestHelloAckAnnouncesDaemonVersion(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello())
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil {
		t.Fatalf("no hello_ack in %v", msgs)
	}
	payload := ack.Payload.(*protocol.HelloAckPayload)
	if payload.DaemonVersion != "0.1.0-test" {
		t.Fatalf("daemon_version = %q, want 0.1.0-test", payload.DaemonVersion)
	}
	if !slices.Equal(payload.Features, []string{
		pageAcquireFeature, triageSnapshotFeature, triageSnapshotSchema2Feature, triageMutationsFeature, reviewPreviewFeature, statsFeature, pageCaptureFeature, pageCaptureRequestFeature, activityFeedFeature, triageCountsSchema2Feature, sessionEvidenceFeature, deliveryContextFeature,
	}) {
		t.Fatalf("features = %v, want required bridge feature set", payload.Features)
	}
}

func TestOldHelloNeverReceivesUnsolicitedTriageFrames(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello())
	for _, msg := range msgs {
		switch msg.Type {
		case protocol.MsgTriageSnapshotResponse, protocol.MsgTriageCountsResponse,
			protocol.MsgTriageDecideResult, protocol.MsgHumanActionResolveResult,
			protocol.MsgReviewPreviewResult:
			t.Fatalf("old hello received unsolicited new frame %q", msg.Type)
		}
	}
}

func TestTriageSnapshotReducesMaximalPageToFrameCap(t *testing.T) {
	b, _, _, _ := newBridge(t)
	watched, err := b.watchRunner.Store.Create(context.Background(), watch.CreateInput{
		Query: "frame boundary", Filters: watch.Filters{YearFrom: 2020},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]watch.DigestEntry, 0, 100)
	for i := 1; i <= 100; i++ {
		suffix := strings.Repeat("x", i)
		entries = append(entries, watch.DigestEntry{
			WorkKey: "10.1000/" + suffix, DOI: "10.1000/" + suffix,
			Title: strings.Repeat("T", 500), Authors: strings.Repeat("A", 200),
			Abstract: strings.Repeat("B", 2000), Year: 2026, IsOA: true,
		})
	}
	if _, err := b.watchRunner.Store.RecordDigest(context.Background(), watched.ID, b.now(), entries); err != nil {
		t.Fatal(err)
	}

	msgs, raw := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-frame-001", SchemaVersions: []int64{1}, Limit: 100}))
	for i, msg := range msgs {
		if msg.Type != protocol.MsgTriageSnapshotResponse {
			continue
		}
		if len(raw[i]) > protocol.MaxBrowserMessageBytes {
			t.Fatalf("snapshot frame is %d bytes, cap %d", len(raw[i]), protocol.MaxBrowserMessageBytes)
		}
		payload := msg.Payload.(*protocol.TriageSnapshotResponsePayload)
		if len(payload.Items) == 0 || len(payload.Items) >= 100 || !payload.HasMore || payload.Cursor == "" {
			t.Fatalf("frame-limited snapshot = %+v", payload)
		}
		return
	}
	t.Fatal("triage snapshot response missing")
}

func TestTriageSnapshotNegotiatesSchema2AccessClassification(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_snapshot_schema", handoffWork())
	if _, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE human_actions SET requires_auth = 1, blocked_by = 'paywall' WHERE job_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())

	schema1, schema1Raw := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-schema1-001", SchemaVersions: []int64{1}}))
	response := firstOfType(schema1, protocol.MsgTriageSnapshotResponse)
	if response == nil {
		t.Fatal("schema-1 snapshot response missing")
	}
	legacy := response.Payload.(*protocol.TriageSnapshotResponsePayload)
	if legacy.Schema != 1 || len(legacy.Items) != 1 || legacy.Items[0].RequiresAuth != nil || legacy.Items[0].BlockedBy != "" {
		t.Fatalf("schema-1 locked action shape = %+v", legacy)
	}
	assertLockedSchema1Snapshot(t, schema1Raw)

	schema2, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-schema2-001", SchemaVersions: []int64{2}}))
	response = firstOfType(schema2, protocol.MsgTriageSnapshotResponse)
	if response == nil {
		t.Fatal("schema-2 snapshot response missing")
	}
	current := response.Payload.(*protocol.TriageSnapshotResponsePayload)
	if current.Schema != 2 || len(current.Items) != 1 || current.Items[0].RequiresAuth == nil ||
		!*current.Items[0].RequiresAuth || current.Items[0].BlockedBy != "paywall" {
		t.Fatalf("schema-2 access classification = %+v", current)
	}
}

func assertLockedSchema1Snapshot(t *testing.T, frames []json.RawMessage) {
	t.Helper()
	allowed := map[string]bool{
		"kind": true, "id": true, "rank": true, "title": true, "facts": true, "links": true, "ops": true,
		"action_id": true, "job_id": true, "action_kind": true, "job_state": true, "revision": true, "sha256": true, "size_bytes": true,
	}
	for _, frame := range frames {
		var envelope struct {
			Type    string `json:"type"`
			Payload struct {
				Schema int                          `json:"schema"`
				Items  []map[string]json.RawMessage `json:"items"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Type != protocol.MsgTriageSnapshotResponse {
			continue
		}
		if envelope.Payload.Schema != 1 {
			t.Fatalf("locked parser received schema %d, want 1", envelope.Payload.Schema)
		}
		for _, item := range envelope.Payload.Items {
			var kind string
			if err := json.Unmarshal(item["kind"], &kind); err != nil {
				t.Fatal(err)
			}
			if kind != "human_action" {
				continue
			}
			for field := range item {
				if !allowed[field] {
					t.Fatalf("locked schema-1 parser rejected unknown human_action field %q", field)
				}
			}
		}
		return
	}
	t.Fatal("locked schema-1 parser received no snapshot response")
}

func TestTriageCountsResponseEchoesRequestID(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "request-count-001"}))
	result := firstOfType(msgs, protocol.MsgTriageCountsResponse)
	if result == nil {
		t.Fatalf("triage counts response missing: %v", msgs)
	}
	if payload := result.Payload.(*protocol.TriageCountsResponsePayload); payload.RequestID != "request-count-001" {
		t.Fatalf("counts response request_id = %q", payload.RequestID)
	}
}

func TestTriageCountsNegotiatesAuthField(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	parkInstitutional(t, jobs, "wr_counts_auth", handoffWork(), "")

	legacy, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "request-count-v1"}))
	v1 := firstOfType(legacy, protocol.MsgTriageCountsResponse)
	if v1 == nil {
		t.Fatalf("legacy counts response missing: %v", legacy)
	}
	if got := v1.Payload.(*protocol.TriageCountsResponsePayload).Counts.ActionsRequiresAuth; got != nil {
		t.Fatalf("legacy counts unexpectedly included auth count: %v", *got)
	}

	negotiated, _ := runSync(t, b, inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "request-count-v2", SchemaVersions: []int64{2}}))
	v2 := firstOfType(negotiated, protocol.MsgTriageCountsResponse)
	if v2 == nil {
		t.Fatalf("schema-2 counts response missing: %v", negotiated)
	}
	if got := v2.Payload.(*protocol.TriageCountsResponsePayload).Counts.ActionsRequiresAuth; got == nil || *got != 1 {
		t.Fatalf("schema-2 auth count = %v, want 1", got)
	}
}

// breakTriage rewires b's triage service onto an already-closed database, so
// any Counts/Stats query it issues fails, without touching the healthy jobs
// database that poll() and the rest of Sync depend on.
func breakTriage(t *testing.T, b *Bridge) {
	t.Helper()
	brokenDB, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs := &job.Store{S: brokenDB}
	b.triage = triage.New(brokenDB, watch.NewStore(brokenDB), jobs)
	if err := brokenDB.Close(); err != nil {
		t.Fatal(err)
	}
}

// A raw Go error from triageCounts would propagate through Sync into the
// native host's fatal error path (internal/nativehost/host.go), tearing down
// the whole native-messaging session over a routine, recoverable failure.
func TestTriageCountsUnconfiguredReportsErrorFrameNotFatal(t *testing.T) {
	b, _, _, _ := newBridge(t)
	b.triage = nil
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "request-count-002"}))
	errFrame := firstOfType(msgs, protocol.MsgError)
	if errFrame == nil {
		t.Fatalf("no error frame for unconfigured triage service: %v", msgs)
	}
	if countType(msgs, protocol.MsgTriageCountsResponse) != 0 {
		t.Fatalf("triage_counts_response emitted despite unconfigured triage service: %v", msgs)
	}
	if poll, _ := runSync(t, b); firstOfType(poll, protocol.MsgError) != nil {
		t.Fatalf("session did not survive an unconfigured triage_counts_request: %v", poll)
	}
}

func TestTriageCountsQueryFailureReportsErrorFrameNotFatal(t *testing.T) {
	b, _, _, _ := newBridge(t)
	breakTriage(t, b)
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "request-count-003"}))
	errFrame := firstOfType(msgs, protocol.MsgError)
	if errFrame == nil {
		t.Fatalf("no error frame for a failing triage counts query: %v", msgs)
	}
	if countType(msgs, protocol.MsgTriageCountsResponse) != 0 {
		t.Fatalf("triage_counts_response emitted despite a failing query: %v", msgs)
	}
	if poll, _ := runSync(t, b); firstOfType(poll, protocol.MsgError) != nil {
		t.Fatalf("session did not survive a failing triage_counts_request: %v", poll)
	}
}

// statsAcquiredJob drives a fresh job to ready with an accepted candidate at
// accessBasis, optionally passing through an awaiting_human handoff first —
// the shape browser stats' HandoffsRequired counts.
func statsAcquiredJob(t *testing.T, jobs *job.Store, requestID, accessBasis string, handoff bool) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Stats work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if handoff {
		for _, step := range [][2]string{
			{job.StateQueued, job.StateResolving},
			{job.StateResolving, job.StateAwaitingHuman},
		} {
			if err := jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
				t.Fatalf("%s->%s: %v", step[0], step[1], err)
			}
		}
		if _, err := jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff available", job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
	} else if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		Source: "fixture", URLRedacted: "https://example.test/" + requestID, URLKey: requestID,
		Version: resolver.VersionPublished, AccessBasis: accessBasis, ReuseLicense: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.NextPendingCandidate(ctx, id)
	if err != nil || candidate == nil {
		t.Fatalf("next pending candidate for %s: %+v, %v", id, candidate, err)
	}
	if err := jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	return id
}

// statsFailedJob drives a fresh job to failed, the shape browser stats'
// FailedTotal counts.
func statsFailedJob(t *testing.T, jobs *job.Store, requestID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Stats failed work"}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "fixture", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateFailed, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStatsResponseReflectsAcquisitionAggregates(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	statsAcquiredJob(t, jobs, "stats-acquired-001", resolver.AccessOpen, true)
	statsFailedJob(t, jobs, "stats-failed-001")

	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgStatsRequest, "",
		protocol.StatsRequestPayload{RequestID: "request-stats-001"}))
	result := firstOfType(msgs, protocol.MsgStatsResponse)
	if result == nil {
		t.Fatalf("stats response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.StatsResponsePayload)
	if payload.RequestID != "request-stats-001" {
		t.Fatalf("stats response request_id = %q", payload.RequestID)
	}
	if _, err := time.Parse(time.RFC3339, payload.GeneratedAt); err != nil {
		t.Fatalf("generated_at not RFC3339: %q (%v)", payload.GeneratedAt, err)
	}
	if payload.AcquiredTotal != 1 || payload.FailedTotal != 1 || payload.HandoffsRequired != 1 {
		t.Fatalf("stats totals = %+v", payload)
	}
	wantAccess := protocol.StatsAccess{OpenAccess: 1}
	if payload.Access != wantAccess {
		t.Fatalf("access = %+v, want %+v", payload.Access, wantAccess)
	}
	if len(payload.Series) != 12 {
		t.Fatalf("series length = %d, want 12", len(payload.Series))
	}
	total := int64(0)
	for _, bucket := range payload.Series {
		total += bucket.Acquired
	}
	if total != 1 {
		t.Fatalf("series total = %d, want 1", total)
	}
}

// A raw Go error from stats would propagate through Sync into the native
// host's fatal error path (internal/nativehost/host.go), tearing down the
// whole native-messaging session over a routine, recoverable failure.
func TestStatsUnconfiguredReportsErrorFrameNotFatal(t *testing.T) {
	b, _, _, _ := newBridge(t)
	b.triage = nil
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgStatsRequest, "",
		protocol.StatsRequestPayload{RequestID: "request-stats-002"}))
	errFrame := firstOfType(msgs, protocol.MsgError)
	if errFrame == nil {
		t.Fatalf("no error frame for unconfigured triage service: %v", msgs)
	}
	if countType(msgs, protocol.MsgStatsResponse) != 0 {
		t.Fatalf("stats_response emitted despite unconfigured triage service: %v", msgs)
	}
	if poll, _ := runSync(t, b); firstOfType(poll, protocol.MsgError) != nil {
		t.Fatalf("session did not survive an unconfigured stats_request: %v", poll)
	}
}

func TestStatsQueryFailureReportsErrorFrameNotFatal(t *testing.T) {
	b, _, _, _ := newBridge(t)
	breakTriage(t, b)
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgStatsRequest, "",
		protocol.StatsRequestPayload{RequestID: "request-stats-003"}))
	errFrame := firstOfType(msgs, protocol.MsgError)
	if errFrame == nil {
		t.Fatalf("no error frame for a failing stats query: %v", msgs)
	}
	if countType(msgs, protocol.MsgStatsResponse) != 0 {
		t.Fatalf("stats_response emitted despite a failing query: %v", msgs)
	}
	if poll, _ := runSync(t, b); firstOfType(poll, protocol.MsgError) != nil {
		t.Fatalf("session did not survive a failing stats_request: %v", poll)
	}
}

func TestTriageDismissConsumesSelectedWatchHit(t *testing.T) {
	b, _, _, _ := newBridge(t)
	watched, err := b.watchRunner.Store.Create(context.Background(), watch.CreateInput{
		Query: "dismiss", Filters: watch.Filters{YearFrom: 2020},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.watchRunner.Store.RecordDigest(context.Background(), watched.ID, b.now(), []watch.DigestEntry{{
		WorkKey: "10.1000/dismiss", DOI: "10.1000/dismiss", Title: "Dismiss me",
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := b.triage.Snapshot(context.Background(), triage.SnapshotRequest{Limit: 1})
	if err != nil || len(snapshot.Items) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageDecide, "",
		protocol.TriageDecidePayload{
			RequestID: "request-dismiss-001", ItemID: snapshot.Items[0].ID, Op: "dismiss",
			WatchScope: json.RawMessage(`"all"`),
		}))
	result := firstOfType(msgs, protocol.MsgTriageDecideResult)
	if result == nil {
		t.Fatalf("triage decision response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.TriageDecideResultPayload)
	if payload.RequestID != "request-dismiss-001" || payload.Outcome != "applied" {
		t.Fatalf("triage decision payload = %+v", payload)
	}
	after, err := b.triage.Snapshot(context.Background(), triage.SnapshotRequest{Limit: 1})
	if err != nil || len(after.Items) != 0 {
		t.Fatalf("dismissed snapshot = %+v, %v", after, err)
	}
}

// The shipping path for a retraction dismissal: a real Crossref sweep fills the
// sentinel, the inbox frame carries the notice, and the extension's existing
// dismiss frame - watch_scope and all - clears it for good.
func TestTriageDismissAcknowledgesRetractionNotice(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, "wr_browser_retraction", work.Work{DOI: "10.1000/retracted", Title: "Retracted work"}, "", "",
		job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := jobs.UpsertArtifact(ctx, job.Artifact{SHA256: sha, SizeBytes: 1, MIME: "application/pdf", Path: filepath.Join(cfg.DataDir, "retracted.pdf"), IdentityResult: "pass"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Transition(ctx, id, job.StateValidating, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"update-to":[{"DOI":"10.2000/notice","updated":"retraction"}]}}`))
	}))
	defer crossref.Close()
	sentinel := retraction.New(retraction.Options{
		Store: jobs.S, Budgets: unlimitedBudget{}, Policy: config.Source{Enabled: true},
		Client: crossref.Client(), BaseURL: crossref.URL, DataDir: cfg.DataDir,
	})
	if err := sentinel.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	b.triage.RegisterSource(sentinel)

	snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: 10})
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Kind != triage.KindRetraction {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	if !slices.Contains(snapshot.Items[0].Ops, "dismiss") {
		t.Fatalf("retraction ops = %v, want a dismiss operation", snapshot.Items[0].Ops)
	}
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageDecide, "",
		protocol.TriageDecidePayload{
			RequestID: "request-retraction-001", ItemID: snapshot.Items[0].ID, Op: "dismiss",
			WatchScope: json.RawMessage(`"all"`),
		}))
	result := firstOfType(msgs, protocol.MsgTriageDecideResult)
	if result == nil {
		t.Fatalf("triage decision response missing: %v", msgs)
	}
	if payload := result.Payload.(*protocol.TriageDecideResultPayload); payload.Outcome != "applied" {
		t.Fatalf("retraction dismissal payload = %+v", payload)
	}
	after, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: 10})
	if err != nil || len(after.Items) != 0 || after.Counts.Retractions != 0 {
		t.Fatalf("snapshot after acknowledgement = %+v, %v", after, err)
	}
}

type unlimitedBudget struct{}

func (unlimitedBudget) Acquire(context.Context, string, config.Source, float64) error { return nil }

func TestReviewPreviewAndResolveNeverLeakQuarantinePath(t *testing.T) {
	b, jobs, _, data := newBridge(t)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id, err := jobs.CreateRequest(context.Background(), "wr_browser_review", work.Work{DOI: "10.1000/review", Title: "Review"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET state = 'needs_review' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "quarantine-review.pdf")
	if err := os.WriteFile(path, []byte("%PDF-preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	actionID, err := jobs.OpenHumanAction(context.Background(), id, "verify_identity", "review the PDF", job.Access(false, ""),
		job.WithHumanActionBinding(job.HumanActionBinding{
			CandidateID: 1, QuarantinePath: path, QuarantineSHA256: sha,
		}))
	if err != nil {
		t.Fatal(err)
	}

	msgs, raw := runSync(t, b, hello(), inFrame(t, protocol.MsgReviewPreviewRequest, "",
		protocol.ReviewPreviewRequestPayload{RequestID: "request-preview-001", ActionID: actionID}))
	previewIndex := -1
	var previewURL string
	for i, msg := range msgs {
		if msg.Type != protocol.MsgReviewPreviewResult {
			continue
		}
		payload := msg.Payload.(*protocol.ReviewPreviewResultPayload)
		if payload.RequestID != "request-preview-001" || payload.Outcome != "ok" || payload.SHA256 != sha || payload.SizeBytes != int64(len("%PDF-preview")) {
			t.Fatalf("preview payload = %+v", payload)
		}
		if strings.Contains(string(raw[i]), path) || strings.Contains(string(raw[i]), "quarantine_path") {
			t.Fatalf("preview frame leaked quarantine path: %s", raw[i])
		}
		previewIndex, previewURL = i, payload.URL
	}
	if previewIndex < 0 {
		t.Fatalf("review preview response missing: %v", msgs)
	}

	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgHumanActionResolve, "",
		protocol.HumanActionResolvePayload{RequestID: "request-resolve-001", ActionID: actionID, Verdict: "reject", ExpectedRevision: 1}))
	result := firstOfType(msgs, protocol.MsgHumanActionResolveResult)
	if result == nil {
		t.Fatalf("human action resolve response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.HumanActionResolveResultPayload)
	if payload.RequestID != "request-resolve-001" || payload.Outcome != "applied" {
		t.Fatalf("human action resolve payload = %+v", payload)
	}
	request := httptest.NewRequest(http.MethodGet, previewURL, nil)
	response := httptest.NewRecorder()
	b.preview.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked preview status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

// A preview failure that predates this fix (action already gone, quarantine
// file missing, etc.) was returned as a raw Go error, which propagated
// through Sync() and, per internal/nativehost/host.go's fatal-error
// contract, killed the whole native-messaging connection on every click.
// It must instead come back as an ordinary review_preview_result frame with
// outcome "error", leaving Sync() (and the connection) untouched.
func TestReviewPreviewOnMissingActionReturnsErrorOutcomeWithoutFailingSync(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgReviewPreviewRequest, "",
		protocol.ReviewPreviewRequestPayload{RequestID: "request-preview-missing", ActionID: 999999}))
	result := firstOfType(msgs, protocol.MsgReviewPreviewResult)
	if result == nil {
		t.Fatalf("review preview result missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.ReviewPreviewResultPayload)
	if payload.RequestID != "request-preview-missing" || payload.Outcome != "error" || payload.Detail == "" {
		t.Fatalf("review preview error payload = %+v", payload)
	}
	if payload.URL != "" || payload.SHA256 != "" || payload.SizeBytes != 0 || payload.ExpiresAt != "" {
		t.Fatalf("error outcome leaked capability fields: %+v", payload)
	}
	// The connection must still be usable: a follow-up sync succeeds (runSync
	// itself fails the test if Sync returns an error).
	runSync(t, b)
}

// A stale action can survive after its job moves away from the handoff park.
// Dismiss must close the stale inbox row without cancelling the live job.
func TestHumanActionResolveDismissClosesStaleNonReviewAction(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id, err := jobs.CreateRequest(context.Background(), "wr_browser_dismiss", work.Work{DOI: "10.1000/dismiss", Title: "Dismiss me"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	actionID, err := jobs.OpenHumanAction(context.Background(), id, "manual_download", "a resolver returned a landing page", job.Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgHumanActionResolve, "",
		protocol.HumanActionResolvePayload{RequestID: "request-dismiss-001", ActionID: actionID, Verdict: "dismiss", ExpectedRevision: 1}))
	result := firstOfType(msgs, protocol.MsgHumanActionResolveResult)
	if result == nil {
		t.Fatalf("human action resolve response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.HumanActionResolveResultPayload)
	if payload.RequestID != "request-dismiss-001" || payload.Outcome != "applied" {
		t.Fatalf("dismiss payload = %+v", payload)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateQueued || row.TerminalReason != "" {
		t.Fatalf("stale-action dismiss changed job = %+v, %v", row, err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.ID == actionID {
			t.Fatalf("dismissed action still open: %+v", action)
		}
	}
}

func TestHumanActionResolveDismissCancelsAwaitingHandoff(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id, err := jobs.CreateRequest(context.Background(), "wr_browser_dismiss_awaiting", work.Work{DOI: "10.1000/dismiss", Title: "Dismiss me"}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(context.Background(), id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(context.Background(), id, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	actionID, err := jobs.OpenHumanAction(context.Background(), id, "manual_download", "a resolver returned a landing page", job.Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgHumanActionResolve, "",
		protocol.HumanActionResolvePayload{RequestID: "request-dismiss-awaiting-001", ActionID: actionID, Verdict: "dismiss", ExpectedRevision: 1}))
	result := firstOfType(msgs, protocol.MsgHumanActionResolveResult)
	if result == nil {
		t.Fatalf("human action resolve response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.HumanActionResolveResultPayload)
	if payload.RequestID != "request-dismiss-awaiting-001" || payload.Outcome != "applied" {
		t.Fatalf("dismiss payload = %+v", payload)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateCancelled || row.TerminalReason != "user_dismissed" {
		t.Fatalf("awaiting handoff dismiss = %+v, %v", row, err)
	}
}

func TestHelloAckAdvertisesResolverOrigins(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello())
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil {
		t.Fatalf("no hello_ack in %v", msgs)
	}
	origins := ack.Payload.(*protocol.HelloAckPayload).ResolverOrigins
	if !slices.Equal(origins, []string{"https://openurl.example.edu"}) {
		t.Fatalf("resolver_origins = %v, want [https://openurl.example.edu]", origins)
	}
}

func TestPageAcquireSubmitsNormalizedDOI(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPageAcquire, "", protocol.PageAcquirePayload{
		URL:    "https://publisher.example.edu/article/42",
		DOI:    "https://doi.org/10.1000/Example.42",
		Title:  "An Example Paper",
		Source: "popup",
	}))
	ack := firstOfType(msgs, protocol.MsgPageAcquireAck)
	if ack == nil {
		t.Fatalf("no page_acquire_ack in %v", msgs)
	}
	payload := ack.Payload.(*protocol.PageAcquireAckPayload)
	if payload.JobID == "" || payload.Duplicate || payload.Error != "" {
		t.Fatalf("page_acquire_ack = %#v", payload)
	}
	row, err := jobs.Get(context.Background(), payload.JobID)
	if err != nil {
		t.Fatalf("submitted job: %v", err)
	}
	if row.Work.DOI != "10.1000/example.42" {
		t.Fatalf("submitted DOI = %q, want normalized DOI", row.Work.DOI)
	}
}

func TestPageAcquireInvalidDOIReturnsErrorWithoutSubmit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPageAcquire, "", protocol.PageAcquirePayload{
		URL: "https://publisher.example.edu/article/42",
		DOI: "not-a-doi",
	}))
	ack := firstOfType(msgs, protocol.MsgPageAcquireAck)
	if ack == nil {
		t.Fatalf("no page_acquire_ack in %v", msgs)
	}
	payload := ack.Payload.(*protocol.PageAcquireAckPayload)
	if payload.Error == "" || payload.JobID != "" || payload.Duplicate {
		t.Fatalf("page_acquire_ack = %#v", payload)
	}
	var count int
	if err := jobs.S.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("jobs after invalid page acquire = %d, want 0", count)
	}
}

func TestPageAcquireWithoutDOIReturnsErrorWithoutSubmit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPageAcquire, "", protocol.PageAcquirePayload{
		URL:   "https://publisher.example.edu/article/42",
		Title: "A DOI-less page",
	}))
	ack := firstOfType(msgs, protocol.MsgPageAcquireAck)
	if ack == nil {
		t.Fatalf("no page_acquire_ack in %v", msgs)
	}
	payload := ack.Payload.(*protocol.PageAcquireAckPayload)
	if payload.Error != "page has no DOI" || payload.JobID != "" || payload.Duplicate {
		t.Fatalf("page_acquire_ack = %#v", payload)
	}
	var count int
	if err := jobs.S.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("jobs after DOI-less page acquire = %d, want 0", count)
	}
}

func TestPageAcquireDuplicateSurfacesExistingJob(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())
	frame := inFrame(t, protocol.MsgPageAcquire, "", protocol.PageAcquirePayload{
		URL: "https://publisher.example.edu/article/42",
		DOI: "10.1000/example.42",
	})
	first, _ := runSync(t, b, frame)
	second, _ := runSync(t, b, frame)
	firstAck := firstOfType(first, protocol.MsgPageAcquireAck)
	secondAck := firstOfType(second, protocol.MsgPageAcquireAck)
	if firstAck == nil || secondAck == nil {
		t.Fatalf("page acquire acknowledgements = %v / %v", first, second)
	}
	firstPayload := firstAck.Payload.(*protocol.PageAcquireAckPayload)
	secondPayload := secondAck.Payload.(*protocol.PageAcquireAckPayload)
	if firstPayload.JobID == "" || secondPayload.JobID != firstPayload.JobID || !secondPayload.Duplicate {
		t.Fatalf("duplicate acknowledgements = %#v / %#v", firstPayload, secondPayload)
	}
}

func TestSessionInfoAfterHello(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "1.2.3",
		"adapter_versions":  map[string]string{"jstor": "1.0.0"},
	}))
	version, adapterCount, helloSeen := b.SessionInfo()
	if version != "1.2.3" {
		t.Fatalf("extension version = %q, want 1.2.3", version)
	}
	if adapterCount != 1 {
		t.Fatalf("adapter count = %d, want 1", adapterCount)
	}
	if !helloSeen {
		t.Fatal("hello was not recorded")
	}
}

func TestOutdatedExtensionReceivesUpdateError(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.0.9",
	}))
	if len(msgs) != 1 {
		t.Fatalf("hello replies = %d, want one error", len(msgs))
	}
	if firstOfType(msgs, protocol.MsgHelloAck) != nil {
		t.Fatalf("outdated extension received hello_ack: %v", msgs)
	}
	errMsg := firstOfType(msgs, protocol.MsgError)
	if errMsg == nil {
		t.Fatalf("outdated extension did not receive error: %v", msgs)
	}
	payload := errMsg.Payload.(*protocol.ErrorPayload)
	if payload.Code != "extension_outdated" {
		t.Fatalf("error code = %q, want extension_outdated", payload.Code)
	}
	if !strings.Contains(payload.Message, "update the extension from the store") {
		t.Fatalf("error message = %q", payload.Message)
	}
}

func TestDaemonRestartReturnsHelloRequired(t *testing.T) {
	active, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_restart", handoffWork())
	runSync(t, active, hello())

	// A new daemon has the same durable jobs but no in-memory hello-session.
	restarted := NewBridge(jobs, active.svc, active.triage, active.watchRunner, active.preview, active.captureStore, cfg, active.Version)
	msgs, _ := runSync(t, restarted)
	if len(msgs) != 1 {
		t.Fatalf("restart poll frames = %d, want 1", len(msgs))
	}
	required := firstOfType(msgs, protocol.MsgError)
	if required == nil {
		t.Fatalf("restart poll did not return an error frame: %v", msgs)
	}
	payload := required.Payload.(*protocol.ErrorPayload)
	if payload.Code != "expected_hello" {
		t.Fatalf("restart error code = %q, want expected_hello", payload.Code)
	}

	// A concurrent relay has the same recoverable result and is never applied.
	msgs, _ = runSync(t, restarted, inFrame(t, protocol.MsgJobAccept, id, map[string]any{}))
	required = firstOfType(msgs, protocol.MsgError)
	if required == nil || required.Payload.(*protocol.ErrorPayload).Code != "expected_hello" {
		t.Fatalf("pre-hello relay = %v, want expected_hello error", msgs)
	}
}

func TestHandoffJobOfferedExactlyOncePerHelloSession(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_offer", handoffWork())

	msgs, _ := runSync(t, b, hello())
	if got := countType(msgs, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("job_offer count on hello = %d, want 1", got)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer.JobID != id {
		t.Fatalf("offer job_id = %s, want %s", offer.JobID, id)
	}
	p := offer.Payload.(*protocol.JobOfferPayload)

	// KEV encoding: url_ver + rft_id=info:doi/<doi>, URL-escaped.
	u, err := url.Parse(p.OpenURL)
	if err != nil {
		t.Fatalf("openurl parse: %v", err)
	}
	q := u.Query()
	if q.Get("url_ver") != "Z39.88-2004" {
		t.Fatalf("url_ver = %q", q.Get("url_ver"))
	}
	if q.Get("rft_id") != "info:doi/10.1002/example.42" {
		t.Fatalf("rft_id = %q", q.Get("rft_id"))
	}
	if !strings.Contains(p.OpenURL, "info%3Adoi%2F10.1002%2Fexample.42") {
		t.Fatalf("openurl not URL-escaped: %s", p.OpenURL)
	}
	if p.AccessMode != cfg.AccessMode {
		t.Fatalf("access_mode = %q", p.AccessMode)
	}
	// The wire list is capped at 20 by the protocol; adapter families beyond
	// the pre-0.4.1 set are recognized by the extension's own registry instead.
	if !slices.Contains(p.ProviderHosts, "springer.com") {
		t.Fatalf("provider_hosts = %v, missing springer.com", p.ProviderHosts)
	}
	if len(p.ProviderHosts) > 20 {
		t.Fatalf("provider_hosts %d entries exceeds the protocol cap", len(p.ProviderHosts))
	}
	if _, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil {
		t.Fatalf("expires_at not RFC3339: %q (%v)", p.ExpiresAt, err)
	}
	if p.Expected == nil || p.Expected.DOI != "10.1002/example.42" {
		t.Fatalf("expected hints = %+v", p.Expected)
	}

	// A subsequent poll in the same hello-session must not re-offer.
	msgs2, _ := runSync(t, b)
	if got := countType(msgs2, protocol.MsgJobOffer); got != 0 {
		t.Fatalf("re-offered %d times in same hello session", got)
	}

	// A new hello (service-worker restart) resets the session and re-offers.
	msgs3, _ := runSync(t, b, hello())
	if got := countType(msgs3, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("job_offer after new hello = %d, want 1", got)
	}
}

func TestFocusHandoffsWithholdsBelowExtensionFloor(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	advance := settableClock(b)
	id := park(t, jobs, "wr_focus_old", handoffWork())
	runSyncAs(t, b, sessA, helloAs(HandoffFocusMinExtensionVersion))

	queued, sessionLive, err := b.FocusHandoffs(context.Background(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 1 {
		t.Fatalf("compatible focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
	}

	// The request was queued for a compatible holder, then that holder went
	// away before polling. The new holder parses normal handoffs but not this
	// frame, so this is the shipped-extension disconnect guard.
	advance(sessionStaleAfter + time.Second)
	msgs, _ := runSyncAs(t, b, sessB, helloAs("0.7.9"))
	if got := countType(msgs, protocol.MsgHandoffFocus); got != 0 {
		t.Fatalf("below-floor holder received %d handoff_focus frames", got)
	}
	queued, sessionLive, err = b.FocusHandoffs(context.Background(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if sessionLive || queued != 0 {
		t.Fatalf("below-floor focus result = queued:%d live:%t, want 0,false", queued, sessionLive)
	}
	if _, err := b.Sync(context.Background(), sessB, true, nil); err != nil {
		t.Fatal(err)
	}
	msgs, _ = runSyncAs(t, b, sessA, helloAs(HandoffFocusMinExtensionVersion))
	if got := countType(msgs, protocol.MsgHandoffFocus); got != 1 {
		t.Fatalf("compatible replacement holder received %d handoff_focus frames, want 1", got)
	}
}

func TestFocusHandoffsTreatsLegacySessionAsFallback(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_focus_legacy_fallback", handoffWork())
	runSyncAs(t, b, "", helloAs(HandoffFocusMinExtensionVersion))

	queued, sessionLive, err := b.FocusHandoffs(context.Background(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 0 || sessionLive {
		t.Fatalf("legacy focus result = queued:%d live:%t, want 0,false", queued, sessionLive)
	}
	if b.focusPending[id] {
		t.Fatalf("legacy session queued focus request: %#v", b.focusPending)
	}
}

func TestPollNeverEmitsHandoffFocusForLegacySession(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_focus_legacy_drain", handoffWork())
	runSyncAs(t, b, "", helloAs(HandoffFocusMinExtensionVersion))
	b.mu.Lock()
	b.focusPending[id] = true
	b.offered[id] = true
	b.mu.Unlock()

	msgs, _ := runSyncAs(t, b, "")
	if got := countType(msgs, protocol.MsgHandoffFocus); got != 0 {
		t.Fatalf("legacy session received %d handoff_focus frames", got)
	}
}

func TestFocusHandoffsTreatsStaleHolderAsFallback(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	advance := settableClock(b)
	id := park(t, jobs, "wr_focus_stale", handoffWork())
	runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{"extension_version": HandoffFocusMinExtensionVersion}))
	advance(sessionStaleAfter + time.Second)

	queued, sessionLive, err := b.FocusHandoffs(context.Background(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if sessionLive || queued != 0 {
		t.Fatalf("stale holder focus result = queued:%d live:%t, want 0,false", queued, sessionLive)
	}
}

func TestFocusHandoffsEmitsOnceAtAndAboveExtensionFloor(t *testing.T) {
	for _, version := range []string{HandoffFocusMinExtensionVersion, "0.8.1"} {
		t.Run(version, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			id := park(t, jobs, "wr_focus_"+strings.ReplaceAll(version, ".", "_"), handoffWork())
			runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{"extension_version": version}))

			queued, sessionLive, err := b.FocusHandoffs(context.Background(), []string{id})
			if err != nil {
				t.Fatal(err)
			}
			if !sessionLive || queued != 1 {
				t.Fatalf("focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
			}
			queued, sessionLive, err = b.FocusHandoffs(context.Background(), []string{id})
			if err != nil {
				t.Fatal(err)
			}
			if !sessionLive || queued != 0 {
				t.Fatalf("duplicate focus result = queued:%d live:%t, want 0,true", queued, sessionLive)
			}

			msgs, _ := runSync(t, b)
			focus := firstOfType(msgs, protocol.MsgHandoffFocus)
			if focus == nil || focus.JobID != id {
				t.Fatalf("focus frame = %#v, want job %q", focus, id)
			}
			if _, ok := focus.Payload.(*protocol.EmptyPayload); !ok {
				t.Fatalf("focus payload = %T, want *protocol.EmptyPayload", focus.Payload)
			}
			if got := countType(msgs, protocol.MsgHandoffFocus); got != 1 {
				t.Fatalf("focus frame count = %d, want 1", got)
			}
			msgs, _ = runSync(t, b)
			if got := countType(msgs, protocol.MsgHandoffFocus); got != 0 {
				t.Fatalf("focus re-emitted %d times after its request drained", got)
			}
		})
	}
}

func TestFocusHandoffsOffersTargetOutsidePollPage(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	target := park(t, jobs, "wr_focus_outside_poll_page", handoffWork())
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339Nano), target); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		park(t, jobs, job.NewID("wr_focus_poll_page"), handoffWork())
	}

	runSync(t, b, hello())
	targetInitiallyOffered := b.offered[target]
	queued, sessionLive, err := b.FocusHandoffs(ctx, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 1 {
		t.Fatalf("focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
	}
	// If the oldest-first ordinary pass already offered the target, preserve
	// that offer while freeing a different slot for the focus request.
	b.mu.Lock()
	for id := range b.offered {
		if id == target {
			continue
		}
		delete(b.offered, id)
		break
	}
	b.mu.Unlock()

	msgs, _ := runSync(t, b)
	var offered, focused bool
	for _, msg := range msgs {
		if msg.JobID != target {
			continue
		}
		switch msg.Type {
		case protocol.MsgJobOffer:
			offered = true
		case protocol.MsgHandoffFocus:
			focused = true
		}
	}
	if !focused || (!targetInitiallyOffered && !offered) {
		t.Fatalf("focus frames initially_offered:%t offered:%t focused:%t", targetInitiallyOffered, offered, focused)
	}
}

func TestFocusHandoffsDropsClosedOrUnparkedJobs(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	closedID := park(t, jobs, "wr_focus_closed", handoffWork())
	cancelledID := park(t, jobs, "wr_focus_cancelled", work.Work{DOI: "10.1002/focus.cancelled"})
	runSync(t, b, inFrame(t, protocol.MsgHello, "", map[string]any{"extension_version": HandoffFocusMinExtensionVersion}))

	queued, sessionLive, err := b.FocusHandoffs(ctx, []string{closedID, cancelledID})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 2 {
		t.Fatalf("focus result = queued:%d live:%t, want 2,true", queued, sessionLive)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == closedID && action.Kind == handoffActionKind {
			if err := jobs.ResolveHumanAction(ctx, action.ID, "resolved"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := jobs.Cancel(ctx, cancelledID, "test"); err != nil {
		t.Fatal(err)
	}

	msgs, _ := runSync(t, b)
	if got := countType(msgs, protocol.MsgHandoffFocus); got != 0 {
		t.Fatalf("focus frames for closed or unparked jobs = %d, want 0", got)
	}
	if b.focusPending[closedID] || b.focusPending[cancelledID] {
		t.Fatalf("invalid focus requests remained pending: %#v", b.focusPending)
	}
}

func TestOABrowserHandoffOffersCandidateThenFallsBackToInstitution(t *testing.T) {
	const oaURL = "https://oa.example.org/articles/blocked-paper.pdf"
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_oa_fallback", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.OABrowserHandoffActionDetail(oaURL), job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	msgs, _ := runSync(t, b, hello())
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatal("missing OA browser offer")
	}
	oaOffer := offer.Payload.(*protocol.JobOfferPayload)
	if oaOffer.OpenURL != oaURL {
		t.Fatalf("OA offer URL = %q, want %q", oaOffer.OpenURL, oaURL)
	}
	if !slices.Contains(oaOffer.ProviderHosts, "oa.example.org") {
		t.Fatalf("OA offer hosts = %v, missing OA host", oaOffer.ProviderHosts)
	}

	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": "no_entitlement"}))
	fallback := firstOfType(msgs, protocol.MsgJobOffer)
	if fallback == nil {
		t.Fatal("failed OA offer did not re-park with institutional handoff")
	}
	institutional := fallback.Payload.(*protocol.JobOfferPayload)
	if institutional.OpenURL == oaURL || !strings.HasPrefix(institutional.OpenURL, cfg.Browser.OpenURLBase+"?") {
		t.Fatalf("fallback offer URL = %q, want institutional OpenURL", institutional.OpenURL)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("state after OA failure = %s, want awaiting_human", row.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	foundAction := false
	for _, action := range actions {
		if action.JobID != id || action.Kind != handoffActionKind {
			continue
		}
		foundAction = true
		if action.Detail != app.InstitutionalOpenURLHandoffDetail {
			t.Fatalf("fallback action detail = %q, want institutional handoff", action.Detail)
		}
	}
	if !foundAction {
		t.Fatal("missing fallback handoff action")
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgJobReject, id, map[string]any{}))
	if countType(msgs, protocol.MsgJobOffer) != 0 {
		t.Fatal("institutional fallback must not re-open the OA browser offer")
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateUnavailable {
		t.Fatalf("state after institutional rejection = %s, want unavailable", row.State)
	}
}

func TestSentinelSecretNeverEntersMessagesOrDurableRows(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_sentinel", handoffWork())
	const sentinel = "SENTINEL_IDP_SECRET_ac9f"

	helloMsgs, helloRaw := runSync(t, b, hello())

	// A client attempt to smuggle an IdP URL as an extra field on an auth frame
	// fails the strict decode and stores nothing.
	bad := json.RawMessage(`{"protocol":"papio-browser/1","type":"auth_returned","msg_id":"client-msg-9","seq":9,"job_id":"` + id +
		`","payload":{"elapsed_ms":10,"idp_url":"https://idp.example/saml?token=` + sentinel + `"}}`)
	if _, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{bad}); err == nil {
		t.Fatal("smuggled idp_url field must be rejected")
	}

	// Legitimate timing-only auth frames carry no address.
	authMsgs, authRaw := runSync(t, b,
		inFrame(t, protocol.MsgAuthPending, id, map[string]any{"elapsed_ms": 5}),
		inFrame(t, protocol.MsgAuthReturned, id, map[string]any{"elapsed_ms": 900}),
	)

	// Scan every outbound frame across the session.
	var outbound strings.Builder
	for _, raw := range append(append([]json.RawMessage{}, helloRaw...), authRaw...) {
		outbound.Write(raw)
	}
	_ = helloMsgs
	_ = authMsgs
	if strings.Contains(outbound.String(), sentinel) {
		t.Fatal("sentinel leaked into an outbound frame")
	}

	// Scan every durable row that could conceivably hold text.
	db := jobs.S.DB()
	var dump strings.Builder
	for _, q := range []string{
		`SELECT COALESCE(detail_json,'') FROM events`,
		`SELECT COALESCE(detail,'') FROM human_actions`,
		`SELECT COALESCE(kind,'')||COALESCE(status,'') FROM human_actions`,
		`SELECT COALESCE(url_redacted,'')||COALESCE(landing_redacted,'') FROM candidates`,
		`SELECT COALESCE(terminal_reason,'') FROM jobs`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			dump.WriteString(s)
		}
		_ = rows.Close()
	}
	if strings.Contains(dump.String(), sentinel) {
		t.Fatal("sentinel leaked into a durable row")
	}
	// Sanity: the timing events were actually recorded (elapsed only).
	events, _ := jobs.Events(context.Background(), id)
	encoded, _ := json.Marshal(events)
	if !strings.Contains(string(encoded), "auth_returned") || !strings.Contains(string(encoded), "elapsed_ms") {
		t.Fatalf("timing-only auth events missing: %s", encoded)
	}
}

func TestAuthReturnedReoffersEligibleInstitutionalSiblingsOnce(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"other": {OpenURLBase: "https://other-openurl.example.edu/resolve"},
	}

	source := parkInstitutional(t, jobs, "wr_auth_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_auth_sibling", handoffWork(), "")
	provenEmpty := parkInstitutional(t, jobs, "wr_auth_empty", handoffWork(), "")
	leased := parkInstitutional(t, jobs, "wr_auth_leased", handoffWork(), "")
	otherProfile := parkInstitutional(t, jobs, "wr_auth_other", handoffWork(), "other")
	if err := jobs.RecordEvent(ctx, provenEmpty, "browser.no_entitlement_requeue",
		map[string]any{"outcome": "no_entitlement"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ?`,
		"adopt-in-progress", time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), leased); err != nil {
		t.Fatal(err)
	}

	initial, _ := runSync(t, b, hello())
	if got := countType(initial, protocol.MsgJobOffer); got != 4 {
		t.Fatalf("initial job offers = %d, want 4", got)
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAuthReturned, source, map[string]any{"elapsed_ms": 10}))
	if got := countType(msgs, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("post-auth job offers = %d, want exactly one sibling", got)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != sibling {
		t.Fatalf("post-auth offer = %#v, want sibling %s", offer, sibling)
	}
	for _, untouched := range []string{provenEmpty, leased, otherProfile} {
		if b.reofferPending[untouched] {
			t.Fatalf("ineligible job %s was marked for re-offer", untouched)
		}
	}

	events, err := jobs.Events(ctx, sibling)
	if err != nil {
		t.Fatal(err)
	}
	reoffers := 0
	for _, event := range events {
		if event["kind"] != "browser.handoff_reoffered" {
			continue
		}
		reoffers++
		detail, ok := event["detail"].(map[string]any)
		if !ok || detail["reason"] != "institutional_session_live" {
			t.Fatalf("re-offer detail = %#v, want institutional session reason", event["detail"])
		}
	}
	if reoffers != 1 {
		t.Fatalf("sibling re-offer events = %d, want 1", reoffers)
	}
	for _, untouched := range []string{provenEmpty, leased, otherProfile} {
		events, err := jobs.Events(ctx, untouched)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				t.Fatalf("ineligible job %s was re-offered: %#v", untouched, event)
			}
		}
	}

	for _, frames := range [][]json.RawMessage{
		nil,
		{inFrame(t, protocol.MsgAuthReturned, sibling, map[string]any{"elapsed_ms": 11})},
	} {
		msgs, _ := runSync(t, b, frames...)
		if got := countType(msgs, protocol.MsgJobOffer); got != 0 {
			t.Fatalf("sibling was re-offered %d additional times", got)
		}
	}
}

func pacedEventDetails(t *testing.T, jobs *job.Store) []map[string]any {
	t.Helper()
	rows, err := jobs.S.DB().Query(`SELECT detail_json FROM events WHERE kind = 'browser.offers_paced' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestOfferPacingLimitsOutstandingHandoffsAndReportsHeld(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	for range 20 {
		park(t, jobs, job.NewID("paced"), handoffWork())
	}

	initial, _ := runSync(t, b, hello())
	if got := countType(initial, protocol.MsgJobOffer); got != maxOutstandingOffers {
		t.Fatalf("initial job offers = %d, want %d", got, maxOutstandingOffers)
	}
	if got := len(b.offered); got != maxOutstandingOffers {
		t.Fatalf("initial outstanding offers = %d, want %d", got, maxOutstandingOffers)
	}
	events := pacedEventDetails(t, jobs)
	if len(events) != 1 || events[0]["held"] != float64(16) {
		t.Fatalf("initial paced events = %#v, want one event held=16", events)
	}

	// The browser polls every two seconds. An unchanged backlog is still the
	// same pacing episode and must not append an activity row per poll.
	runSync(t, b)
	if events = pacedEventDetails(t, jobs); len(events) != 1 {
		t.Fatalf("unchanged backlog produced duplicate paced events: %#v", events)
	}

	// Complete two offered downloads. Their actions close through adoption,
	// freeing exactly two governor slots for the next poll.
	var settled []json.RawMessage
	downloadID := int64(1)
	for id := range b.offered {
		writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
		settled = append(settled, inFrame(t, protocol.MsgDownloadComplete, id,
			map[string]any{"download_id": downloadID, "filename": "paper.pdf", "size_bytes": 533}))
		downloadID++
		if len(settled) == 2 {
			break
		}
	}
	next, _ := runSync(t, b, settled...)
	if got := countType(next, protocol.MsgJobOffer); got != 2 {
		t.Fatalf("offers after two settlements = %d, want 2", got)
	}
	events = pacedEventDetails(t, jobs)
	if len(events) != 2 || events[1]["held"] != float64(14) {
		t.Fatalf("paced events after refill = %#v, want one new event held=14", events)
	}
}

func TestInstitutionalReofferPacingReleasesOldestFourAndContinuesOnSync(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	ids := make([]string, 0, 10)
	for i := range 10 {
		id := parkInstitutional(t, jobs, job.NewID("reoffer"), handoffWork(), "")
		ids = append(ids, id)
		if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			time.Now().UTC().Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	runSync(t, b, hello())

	// Make four governor slots available, then report an authenticated source.
	b.mu.Lock()
	b.offered = map[string]bool{}
	b.mu.Unlock()
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgAuthReturned, ids[0],
		map[string]any{"elapsed_ms": 10}))
	if got := countType(msgs, protocol.MsgJobOffer); got != maxInstitutionalReoffers {
		t.Fatalf("initial reoffer burst = %d, want %d", got, maxInstitutionalReoffers)
	}
	for _, id := range ids[1 : 1+maxInstitutionalReoffers] {
		if !b.offered[id] {
			t.Fatalf("oldest sibling %s was not re-offered; offered=%#v", id, b.offered)
		}
	}
	for _, id := range ids[1+maxInstitutionalReoffers:] {
		if b.offered[id] {
			t.Fatalf("newer sibling %s jumped ahead of oldest four", id)
		}
	}

	// Ordinary sync ticks continue the same authenticated queue after slots
	// free; no second evidence frame is required.
	b.mu.Lock()
	delete(b.offered, ids[1])
	delete(b.offered, ids[2])
	b.mu.Unlock()
	msgs, _ = runSync(t, b)
	if got := countType(msgs, protocol.MsgJobOffer); got != 2 {
		t.Fatalf("continued reoffer burst = %d, want 2 governor slots", got)
	}
	if !b.offered[ids[5]] || !b.offered[ids[6]] {
		t.Fatalf("continued reoffer did not choose oldest remaining siblings: %#v", b.offered)
	}
	reoffers := 0
	for _, id := range ids[1:] {
		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				reoffers++
			}
		}
	}
	if reoffers != 6 {
		t.Fatalf("institutional reoffer events = %d, want six across two invocations", reoffers)
	}
}

func TestOrdinaryOffersChooseOldestHandoffsFirst(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	ids := make([]string, 0, maxOutstandingOffers+2)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxOutstandingOffers + 2 {
		id := park(t, jobs, job.NewID("ordinary_oldest"), handoffWork())
		ids = append(ids, id)
		if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	runSync(t, b, hello())
	for _, id := range ids[:maxOutstandingOffers] {
		if !b.offered[id] {
			t.Fatalf("oldest handoff %s was not offered; offered=%#v", id, b.offered)
		}
	}
	for _, id := range ids[maxOutstandingOffers:] {
		if b.offered[id] {
			t.Fatalf("newer handoff %s jumped ahead; offered=%#v", id, b.offered)
		}
	}
}

func TestSessionEvidenceReoffersParkedOpenURLHandoffsNotManualDownloads(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	source := parkInstitutional(t, jobs, "wr_evidence_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_evidence_sibling", handoffWork(), "")
	manual := parkInstitutional(t, jobs, "wr_evidence_manual", handoffWork(), "")
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET kind = 'manual_download' WHERE job_id = ?`, manual); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	if !b.offered[source] {
		t.Fatal("source handoff was not initially offered")
	}
	b.mu.Lock()
	delete(b.offered, sibling)
	delete(b.offered, manual)
	b.mu.Unlock()

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{"evidence": "warm_verified", "at": "2026-08-03T12:00:00Z"}))
	if got := countType(msgs, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("parked openurl reoffers = %d, want one", got)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != sibling {
		t.Fatalf("re-offered job = %#v, want %s", offer, sibling)
	}
	repeat, _ := runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{"evidence": "warm_verified", "at": "2026-08-03T12:00:01Z"}))
	if got := countType(repeat, protocol.MsgJobOffer); got != 0 {
		t.Fatalf("throttled evidence emitted %d offers", got)
	}
	var evidenceEvents int
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind = 'browser.session_evidence'`).Scan(&evidenceEvents); err != nil {
		t.Fatal(err)
	}
	if evidenceEvents != 1 {
		t.Fatalf("session evidence events = %d, want one", evidenceEvents)
	}
	events, err := jobs.Events(ctx, sibling)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing reoffer event for parked sibling: %#v", events)
	}
	events, err = jobs.Events(ctx, manual)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			t.Fatalf("manual download was re-offered: %#v", event)
		}
	}
}

func TestSessionEvidenceOriginScopesReoffersToMatchingProfile(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	sourceAlpha := parkInstitutional(t, jobs, "wr_origin_alpha_source", handoffWork(), "alpha")
	siblingAlpha := parkInstitutional(t, jobs, "wr_origin_alpha_sibling", handoffWork(), "alpha")
	sourceBeta := parkInstitutional(t, jobs, "wr_origin_beta_source", handoffWork(), "beta")
	siblingBeta := parkInstitutional(t, jobs, "wr_origin_beta_sibling", handoffWork(), "beta")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{sourceBeta: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = ""
	b.reofferProfile = ""
	b.mu.Unlock()

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{
			"evidence":    "warm_verified",
			"origin_hint": "https://beta.example.edu",
			"at":          "2026-08-03T12:00:00Z",
		}))
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != siblingBeta {
		t.Fatalf("origin-scoped offer = %#v, want beta sibling %s", offer, siblingBeta)
	}
	alphaEvents, err := jobs.Events(context.Background(), siblingAlpha)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range alphaEvents {
		if event["kind"] == "browser.handoff_reoffered" {
			t.Fatalf("alpha sibling was released by beta evidence: %#v", event)
		}
	}
	betaEvents, err := jobs.Events(context.Background(), siblingBeta)
	if err != nil {
		t.Fatal(err)
	}
	foundBetaReoffer := false
	for _, event := range betaEvents {
		if event["kind"] == "browser.handoff_reoffered" {
			foundBetaReoffer = true
			break
		}
	}
	if !foundBetaReoffer {
		t.Fatalf("beta sibling was not released: %#v", betaEvents)
	}
	if !b.offered[siblingBeta] {
		t.Fatalf("beta sibling was not offered: %#v", b.offered)
	}
	_ = sourceAlpha
}

func TestAuthReturnedDoesNotReofferWithoutLiveHolder(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	source := parkInstitutional(t, jobs, "wr_auth_stale_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_auth_stale_sibling", handoffWork(), "")
	runSync(t, b, hello())

	b.mu.Lock()
	b.holder.LastSyncAt = b.now().Add(-sessionStaleAfter - time.Second)
	err := b.recordAuth(ctx, &protocol.BrowserMessage{
		Type:    protocol.MsgAuthReturned,
		JobID:   source,
		Payload: &protocol.AuthPayload{},
	})
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !b.offered[sibling] {
		t.Fatal("stale holder marked sibling for re-offer")
	}
	events, err := jobs.Events(ctx, sibling)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			t.Fatalf("stale holder re-offered sibling: %#v", event)
		}
	}
}

func writeFixturePDF(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte("%PDF-1.4\nadopted\n"), make([]byte, 512)...)
	body = append(body, []byte("\n%%EOF")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadCompleteRejectsTraversalAndAdoptsValidFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_dl", handoffWork())
	runSync(t, b, hello())

	// Path-separated filenames fail the protocol decode before any adoption.
	for _, bad := range []string{"../evil.pdf", "/etc/passwd.pdf"} {
		frame := inFrame(t, protocol.MsgDownloadComplete, id,
			map[string]any{"download_id": 1, "filename": bad, "size_bytes": 100})
		if _, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{frame}); err == nil {
			t.Fatalf("filename %q must be rejected", bad)
		}
	}

	// A valid download under adoptions/<job>/ is adopted and reaches ready.
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id,
		map[string]any{"download_id": 7, "filename": "paper.pdf", "size_bytes": 533}))
	if firstOfType(msgs, protocol.MsgAck) == nil {
		t.Fatalf("no ack for download_complete: %v", msgs)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("adopted job not ready: %+v", row)
	}
	if err := b.svc.Artifacts.Verify(row.ArtifactSHA256); err != nil {
		t.Fatalf("artifact verify: %v", err)
	}
}

func TestDeliveryContextAnnotatesTheMatchingDownloadCandidate(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_delivery_binding", handoffWork())
	if _, err := jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "browser", URLRedacted: "browser://older",
		URLKey: "browser-adopt:older", Version: resolver.VersionUnknown,
		AccessBasis: resolver.AccessManual, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5,
	}}); err != nil {
		t.Fatal(err)
	}
	var olderID int64
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT id FROM candidates WHERE job_id = ? AND url_key = 'browser-adopt:older'`, id).Scan(&olderID); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
	runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id,
		map[string]any{"download_id": 11, "filename": "paper.pdf", "size_bytes": 533}))
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.SelectedCandidateID == 0 || row.SelectedCandidateID == olderID {
		t.Fatalf("selected candidate = %d, want the newly adopted candidate (older=%d)", row.SelectedCandidateID, olderID)
	}
	runSync(t, b, inFrame(t, protocol.MsgDeliveryContext, id,
		map[string]any{"download_id": 11, "route": "resolver", "session_evidence": "warm", "page_host": "provider.example.edu"}))
	older, err := jobs.GetCandidate(ctx, olderID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := jobs.GetCandidate(ctx, row.SelectedCandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if older.BrowserRoute != "" || older.SessionEvidence != "" || older.AccessBasis != resolver.AccessManual {
		t.Fatalf("older candidate received delivery context: %+v", older)
	}
	if newer.BrowserRoute != "resolver" || newer.SessionEvidence != "warm" || newer.AccessBasis != resolver.AccessInstitutional {
		t.Fatalf("new candidate provenance = %+v", newer)
	}
}

func TestDownloadCompleteForLiveJobAdoptsAndAcks(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())

	for _, initial := range []string{job.StateQueued, job.StateResolving, job.StateFetching} {
		t.Run(initial, func(t *testing.T) {
			id, err := jobs.CreateRequest(ctx, "wr_live_download_"+initial, handoffWork(), "", "",
				job.Policy{
					AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
				}, nil, job.PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			if initial == job.StateResolving || initial == job.StateFetching {
				if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving,
					map[string]any{"reason": "test"}); err != nil {
					t.Fatal(err)
				}
			}
			if initial == job.StateFetching {
				if err := jobs.Transition(ctx, id, job.StateResolving, job.StateFetching,
					map[string]any{"reason": "test"}); err != nil {
					t.Fatal(err)
				}
			}

			started, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadStarted, id,
				map[string]any{"download_id": 9, "filename": "paper.pdf"}))
			if len(started) != 0 {
				t.Fatalf("download_started unexpectedly returned frames: %v", started)
			}
			writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
			msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id,
				map[string]any{"download_id": 9, "filename": "paper.pdf", "size_bytes": 533}))
			if firstOfType(msgs, protocol.MsgAck) == nil {
				t.Fatalf("no structured ack for live download_complete: %v", msgs)
			}

			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateReady || row.ArtifactSHA256 == "" {
				t.Fatalf("live %s job was not adopted: %+v", initial, row)
			}
			var version string
			if err := jobs.S.DB().QueryRowContext(ctx,
				`SELECT version FROM candidates WHERE job_id = ? AND source = 'browser' ORDER BY id DESC LIMIT 1`,
				id).Scan(&version); err != nil {
				t.Fatalf("browser candidate version: %v", err)
			}
			if version != resolver.VersionUnknown {
				t.Fatalf("browser adoption version = %q, want %q", version, resolver.VersionUnknown)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, event := range events {
				if kind, ok := event["kind"].(string); ok {
					seen[kind] = true
				}
			}
			for _, kind := range []string{"browser.download_started", "browser.download_complete"} {
				if !seen[kind] {
					t.Fatalf("missing %s event", kind)
				}
			}
		})
	}
}

func TestDownloadValidationDoesNotBlockSessionSync(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_unblocked_sync", handoffWork())
	runSync(t, b, hello())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	validationReleased := false
	t.Cleanup(func() {
		if !validationReleased {
			close(releaseValidation)
		}
	})
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		close(validationStarted)
		<-releaseValidation
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 3},
			Text:       pdf.TextReport{Chars: 4000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}

	frame := inFrame(t, protocol.MsgDownloadComplete, id,
		map[string]any{"download_id": 7, "filename": "paper.pdf", "size_bytes": 533})
	adoptionDone := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{frame})
		adoptionDone <- err
	}()
	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("download adoption never reached validation")
	}

	pollDone := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, nil)
		pollDone <- err
	}()
	select {
	case err := <-pollDone:
		if err != nil {
			t.Fatalf("poll during validation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session sync blocked on download validation")
	}

	close(releaseValidation)
	validationReleased = true
	select {
	case err := <-adoptionDone:
		if err != nil {
			t.Fatalf("download adoption: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download adoption did not finish")
	}
}

func TestPollDiscoveredDownloadValidationDoesNotBlockSessionSync(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_poll_unblocked_sync", handoffWork())
	runSync(t, b, hello())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	validationReleased := false
	t.Cleanup(func() {
		if !validationReleased {
			close(releaseValidation)
		}
	})
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		close(validationStarted)
		<-releaseValidation
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 3},
			Text:       pdf.TextReport{Chars: 4000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}

	adoptionDone := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, nil)
		adoptionDone <- err
	}()
	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("poll-time adoption never reached validation")
	}

	pollDone := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, nil)
		pollDone <- err
	}()
	select {
	case err := <-pollDone:
		if err != nil {
			t.Fatalf("poll during poll-time validation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session sync blocked on poll-time download validation")
	}

	close(releaseValidation)
	validationReleased = true
	select {
	case err := <-adoptionDone:
		if err != nil {
			t.Fatalf("poll-time adoption: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poll-time adoption did not finish")
	}
}

func TestDownloadForUnrelatedJobDoesNotAdoptAnotherJobsFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	target := park(t, jobs, "wr_target", handoffWork())

	// A different job, also parked, but with no download of its own.
	other := park(t, jobs, "wr_other", handoffWork())
	runSync(t, b, hello())
	// Place target's file: the poll-time scan may legitimately adopt it for
	// TARGET (its own directory) — never for `other`.
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), target, "paper.pdf"))

	// A download_complete correlated to `other` must not adopt target's file: it
	// only ever looks under adoptions/<other>/, which is empty. The miss is
	// non-fatal: the bridge acks, records a deferral, and keeps `other` parked.
	frame := inFrame(t, protocol.MsgDownloadComplete, other,
		map[string]any{"download_id": 3, "filename": "paper.pdf", "size_bytes": 533})
	msgs, _ := runSync(t, b, frame)
	if firstOfType(msgs, protocol.MsgAck) == nil {
		t.Fatalf("expected ack after deferred adoption: %v", msgs)
	}
	oRow, _ := jobs.Get(ctx, other)
	if oRow.State != job.StateAwaitingHuman || oRow.ArtifactSHA256 != "" {
		t.Fatalf("other job must stay parked: %+v", oRow)
	}
	events, _ := jobs.Events(ctx, other)
	deferred := false
	for _, e := range events {
		if e["kind"] == "browser.adoption_deferred" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("missing browser.adoption_deferred event")
	}
	tRow, _ := jobs.Get(ctx, target)
	if tRow.State == job.StateReady && tRow.ArtifactSHA256 == "" {
		t.Fatalf("target adopted without artifact: %+v", tRow)
	}
	if tRow.State != job.StateReady && tRow.State != job.StateAwaitingHuman {
		t.Fatalf("target in unexpected state: %+v", tRow)
	}
}

func TestPollScanAdoptsSingleSettledFileAndDefersAmbiguity(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	single := park(t, jobs, "wr_scan_single", handoffWork())
	ambig := park(t, jobs, "wr_scan_ambig", handoffWork())
	partial := park(t, jobs, "wr_scan_partial", handoffWork())
	ffPartial := park(t, jobs, "wr_scan_ff_partial", handoffWork())
	placeholder := park(t, jobs, "wr_scan_placeholder", handoffWork())
	root := cfg.EffectiveAdoptionRoot()
	writeFixturePDF(t, filepath.Join(root, single, "paper.pdf"))
	if err := os.WriteFile(filepath.Join(root, single, ".DS_Store"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(root, ambig, "a.pdf"))
	writeFixturePDF(t, filepath.Join(root, ambig, "b.pdf"))
	writeFixturePDF(t, filepath.Join(root, partial, "c.pdf"))
	if err := os.WriteFile(filepath.Join(root, partial, "c.pdf.crdownload"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	// Firefox streams into name.part beside a zero-byte final-name placeholder.
	if err := os.MkdirAll(filepath.Join(root, ffPartial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ffPartial, "d.pdf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(root, ffPartial, "d.pdf.part"))
	if err := os.MkdirAll(filepath.Join(root, placeholder), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, placeholder, "e.pdf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	if _, err := b.Sync(ctx, testSessionID, false, nil); err != nil { // poll triggers the scan
		t.Fatal(err)
	}

	sRow, _ := jobs.Get(ctx, single)
	if sRow.State != job.StateReady || sRow.ArtifactSHA256 == "" {
		t.Fatalf("single settled file not adopted: %+v", sRow)
	}
	aRow, _ := jobs.Get(ctx, ambig)
	if aRow.State != job.StateAwaitingHuman {
		t.Fatalf("ambiguous dir must stay with the user: %+v", aRow)
	}
	pRow, _ := jobs.Get(ctx, partial)
	if pRow.State != job.StateAwaitingHuman {
		t.Fatalf("in-progress .crdownload must defer the scan: %+v", pRow)
	}
	fRow, _ := jobs.Get(ctx, ffPartial)
	if fRow.State != job.StateAwaitingHuman {
		t.Fatalf("in-progress Firefox .part must defer the scan: %+v", fRow)
	}
	eRow, _ := jobs.Get(ctx, placeholder)
	if eRow.State != job.StateAwaitingHuman {
		t.Fatalf("zero-byte placeholder must defer the scan: %+v", eRow)
	}
}

func TestProviderOutcomeMappings(t *testing.T) {
	type expect struct {
		state        string
		actionStatus string // status the openurl_handoff action should end in
		extraAction  string // additional open action kind expected
		extraDetail  string // detail expected on the additional open action
		terminal     string
	}
	cases := map[string]expect{
		"cancelled":                   {state: job.StateCancelled, actionStatus: "cancelled"},
		"no_entitlement":              {state: job.StateUnavailable, actionStatus: "resolved", terminal: "no_entitlement"},
		"document_delivery_available": {state: job.StateUnavailable, actionStatus: "resolved", terminal: "document_delivery_available"},
		"wrong_work": {
			state: job.StateAwaitingHuman, actionStatus: "resolved", extraAction: "manual_download",
			extraDetail: "papio reached a different work; find and download the requested PDF yourself",
		},
		"ui_changed": {
			state: job.StateAwaitingHuman, actionStatus: "resolved", extraAction: "manual_download",
			extraDetail: "papio could not drive the provider page; download the PDF yourself and papio will adopt it",
		},
		"rate_limited":              {state: job.StateRetryWait, actionStatus: "resolved"},
		"human_auth_required":       {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "human_auth_required"},
		"terms_acceptance_required": {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "terms_acceptance_required"},
	}
	for outcome, want := range cases {
		t.Run(outcome, func(t *testing.T) {
			classifications := []struct {
				name         string
				requiresAuth bool
			}{{name: "default", requiresAuth: false}}
			if want.extraAction == "manual_download" {
				classifications = []struct {
					name         string
					requiresAuth bool
				}{
					{name: "handoff_requires_auth_false", requiresAuth: false},
					{name: "handoff_requires_auth_true", requiresAuth: true},
				}
			}
			for _, classification := range classifications {
				t.Run(classification.name, func(t *testing.T) {
					b, jobs, _, _ := newBridge(t)
					ctx := context.Background()
					id := park(t, jobs, "wr_"+outcome, handoffWork())
					if want.extraAction == "manual_download" {
						if _, err := jobs.S.DB().ExecContext(ctx,
							`UPDATE human_actions SET requires_auth = ? WHERE job_id = ? AND kind = ?`,
							classification.requiresAuth, id, handoffActionKind); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "no_entitlement" || outcome == "document_delivery_available" {
						if err := jobs.RecordEvent(ctx, id, "browser.no_entitlement_requeue", map[string]any{"outcome": outcome}); err != nil {
							t.Fatal(err)
						}
					}
					runSync(t, b, hello())
					runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": outcome}))

					row, err := jobs.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if row.State != want.state {
						t.Fatalf("state = %s, want %s", row.State, want.state)
					}
					if want.terminal != "" && row.TerminalReason != want.terminal {
						t.Fatalf("terminal reason = %q, want %q", row.TerminalReason, want.terminal)
					}
					if want.state == job.StateRetryWait && row.RetryAt == "" {
						t.Fatal("rate_limited did not schedule a retry_at")
					}

					actions, err := jobs.ListHumanActions(ctx, false)
					if err != nil {
						t.Fatal(err)
					}
					var handoffStatus string
					var extraOpen []job.HumanAction
					for _, a := range actions {
						if a.Kind == handoffActionKind {
							handoffStatus = a.Status
						}
						if want.extraAction != "" && a.Kind == want.extraAction && a.Status == "open" {
							extraOpen = append(extraOpen, a)
						}
					}
					if handoffStatus != want.actionStatus {
						t.Fatalf("openurl_handoff status = %q, want %q", handoffStatus, want.actionStatus)
					}
					if want.extraAction != "" && len(extraOpen) != 1 {
						t.Fatalf("open %s actions = %d, want 1", want.extraAction, len(extraOpen))
					}
					if want.extraDetail != "" && extraOpen[0].Detail != want.extraDetail {
						t.Fatalf("%s detail = %q, want %q", want.extraAction, extraOpen[0].Detail, want.extraDetail)
					}
					if want.extraAction == "manual_download" && extraOpen[0].RequiresAuth != classification.requiresAuth {
						t.Fatalf("manual_download requires_auth = %t, want %t", extraOpen[0].RequiresAuth, classification.requiresAuth)
					}
				})
			}
		})
	}
}

func TestMissingAdapterOutcomeExplainsCaptureAndManualFallback(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_missing_adapter", handoffWork())
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{
		"outcome": "ui_changed",
		"detail": "No source-controlled adapter matched this provider page. " +
			"A sanitized diagnostic was saved locally for adapter development.",
	}))

	actions, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == id && action.Kind == "manual_download" && action.Status == "open" {
			want := "papio has no adapter for this provider yet; download the PDF yourself for now; " +
				"a sanitized page diagnostic is saved locally; run 'papio adapter captures' to inspect it"
			if action.Detail != want {
				t.Fatalf("manual-download detail = %q, want %q", action.Detail, want)
			}
			return
		}
	}
	t.Fatal("missing-adapter outcome did not open a manual-download action")
}

func TestProviderOutcomeRecordsAdapterDiagnostics(t *testing.T) {
	for _, outcome := range []string{"wrong_work", "ui_changed"} {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_provider_diagnostics_"+outcome, handoffWork())
			runSync(t, b, hello())
			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{
				"outcome":         outcome,
				"adapter_version": "sage-2026.07.27",
				"detail":          "download control was absent after provider landing",
			}))

			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			diagnostics := 0
			for _, event := range events {
				if event["kind"] != "browser.provider_outcome" {
					continue
				}
				diagnostics++
				detail, ok := event["detail"].(map[string]any)
				if !ok {
					t.Fatalf("provider outcome detail = %#v, want map", event["detail"])
				}
				if detail["outcome"] != outcome ||
					detail["adapter_version"] != "sage-2026.07.27" ||
					detail["extension_version"] != "1.2.3" ||
					detail["detail"] != "download control was absent after provider landing" {
					t.Fatalf("provider diagnostics = %#v", detail)
				}
			}
			if diagnostics != 1 {
				t.Fatalf("provider diagnostic events = %d, want 1", diagnostics)
			}
		})
	}
}

func manualProviderUpgradePark(t *testing.T, jobs *job.Store, requestID, extensionVersion string) string {
	t.Helper()
	ctx := context.Background()
	id := park(t, jobs, requestID, handoffWork())
	open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != handoffActionKind {
		t.Fatalf("initial handoff actions = %+v", open)
	}
	if err := jobs.ResolveHumanAction(ctx, open[0].ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, "manual_download", "download the requested PDF yourself", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "browser.provider_outcome", map[string]any{
		"outcome":           "ui_changed",
		"adapter_version":   "0.1.0",
		"extension_version": extensionVersion,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertManualProviderPark(t *testing.T, jobs *job.Store, id string) {
	t.Helper()
	ctx := context.Background()
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("state = %s, want awaiting_human", row.State)
	}
	open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "manual_download" {
		t.Fatalf("open actions = %+v, want one manual_download", open)
	}
}

func TestProviderAdapterUpgradeRequeuesOnceForLiveRegistry(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_provider_adapter_upgrade", handoffWork())

	runSync(t, b, helloWithAdapterVersions(t, "0.7.0", map[string]string{"sage": "0.1.0"}))
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{
		"outcome": "ui_changed", "adapter_version": "0.1.0",
	}))
	assertManualProviderPark(t, jobs, id)

	runSync(t, b, helloWithAdapterVersions(t, "0.8.0", map[string]string{"sage": "0.2.0"}))
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateResolving {
		t.Fatalf("upgraded adapter did not return job to resolving: %s", row.State)
	}
	open, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("upgrade repair left manual action open: %+v", open)
	}

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	repairs := 0
	for _, event := range events {
		if event["kind"] != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["reason"] != "adapter_upgrade_repair" {
			continue
		}
		repairs++
		if detail["old_extension_version"] != "0.7.0" ||
			detail["new_extension_version"] != "0.8.0" ||
			detail["adapter_version"] != "0.1.0" {
			t.Fatalf("upgrade repair detail = %#v", detail)
		}
	}
	if repairs != 1 {
		t.Fatalf("adapter-upgrade repairs = %d, want 1", repairs)
	}

	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		map[string]any{"reason": "provider_repark"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, "manual_download", "download the requested PDF yourself", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, helloWithAdapterVersions(t, "0.8.0", map[string]string{"sage": "0.2.0"}))
	assertManualProviderPark(t, jobs, id)
}

func TestProviderAdapterUpgradeDeclinesUnprovenVersions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous string
		current  string
	}{
		{name: "equal", previous: "0.7.0", current: "0.7.0"},
		{name: "older", previous: "0.7.0", current: "0.6.0"},
		{name: "missing previous", previous: "", current: "0.8.0"},
		{name: "malformed previous", previous: "not-a-version", current: "0.8.0"},
		{name: "missing current", previous: "0.7.0", current: ""},
		{name: "malformed current", previous: "0.7.0", current: "not-a-version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			id := manualProviderUpgradePark(t, jobs, "wr_adapter_upgrade_"+strings.ReplaceAll(tc.name, " ", "_"), tc.previous)

			if err := b.svc.HandoffRepairer().RepairAdapterUpgrade(context.Background(), tc.current, extensionVersionNewer); err != nil {
				t.Fatal(err)
			}
			assertManualProviderPark(t, jobs, id)
		})
	}
}

func TestProviderAdapterUpgradeRequiresLiveAdapterRegistry(t *testing.T) {
	t.Run("no live session", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := manualProviderUpgradePark(t, jobs, "wr_adapter_upgrade_no_session", "0.7.0")

		runSync(t, b)
		assertManualProviderPark(t, jobs, id)
	})

	t.Run("empty adapter registry", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := manualProviderUpgradePark(t, jobs, "wr_adapter_upgrade_no_registry", "0.7.0")

		runSync(t, b, helloWithAdapterVersions(t, "0.8.0", map[string]string{}))
		assertManualProviderPark(t, jobs, id)
	})
}

func TestOABrowserOfferWithoutIdentifierDoesNotEscalateAuth(t *testing.T) {
	const oaURL = "https://oa.example.org/title-match.pdf"
	for _, outcome := range []string{"human_auth_required", "terms_acceptance_required"} {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_oa_no_identifier_"+outcome, work.Work{
				Title: "A title-matched report without an identifier",
			})
			if _, err := jobs.S.DB().ExecContext(ctx,
				`UPDATE human_actions SET detail = ?, requires_auth = 0, blocked_by = 'anti_bot' WHERE job_id = ?`,
				app.OABrowserHandoffActionDetail(oaURL), id); err != nil {
				t.Fatal(err)
			}

			msgs, _ := runSync(t, b, hello())
			offer := firstOfType(msgs, protocol.MsgJobOffer)
			if offer == nil || offer.Payload.(*protocol.JobOfferPayload).OpenURL != oaURL {
				t.Fatalf("OA offer = %#v, want retained URL %q", offer, oaURL)
			}
			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": outcome}))

			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateUnavailable || row.TerminalReason != "no_identifier" {
				t.Fatalf("auth escalation result = state:%s terminal:%q, want unavailable/no_identifier", row.State, row.TerminalReason)
			}
			actions, err := jobs.ListHumanActions(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 0 {
				t.Fatalf("open actions after no-identifier auth wall = %+v", actions)
			}
		})
	}
}

func TestOAFallbackRequiresFetchableIdentifier(t *testing.T) {
	const oaURL = "https://oa.example.org/title-match.pdf"
	type testCase struct {
		name         string
		w            work.Work
		messageType  string
		payload      any
		wantFallback bool
	}
	cases := []testCase{
		{
			name:        "no entitlement without identifier settles",
			w:           work.Work{Title: "A title-matched report without an identifier"},
			messageType: protocol.MsgProviderOutcome,
			payload:     map[string]any{"outcome": "no_entitlement"},
		},
		{
			name:        "browser rejection without identifier settles",
			w:           work.Work{Title: "A title-matched report without an identifier"},
			messageType: protocol.MsgJobReject,
			payload:     map[string]any{},
		},
		{
			name:         "identified OA handoff retains institutional fallback",
			w:            handoffWork(),
			messageType:  protocol.MsgProviderOutcome,
			payload:      map[string]any{"outcome": "no_entitlement"},
			wantFallback: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, job.NewID("wr_oa_fallback_gate"), tc.w)
			if _, err := jobs.S.DB().ExecContext(ctx,
				`UPDATE human_actions SET detail = ?, requires_auth = 0, blocked_by = 'anti_bot' WHERE job_id = ?`,
				app.OABrowserHandoffActionDetail(oaURL), id); err != nil {
				t.Fatal(err)
			}

			msgs, _ := runSync(t, b, hello())
			offer := firstOfType(msgs, protocol.MsgJobOffer)
			if offer == nil || offer.Payload.(*protocol.JobOfferPayload).OpenURL != oaURL || offer.Payload.(*protocol.JobOfferPayload).RequiresAuth {
				t.Fatalf("OA anti-bot offer = %#v, want no-login URL %q", offer, oaURL)
			}
			msgs, _ = runSync(t, b, inFrame(t, tc.messageType, id, tc.payload))
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantFallback {
				if row.State != job.StateUnavailable || row.TerminalReason != "no_identifier" {
					t.Fatalf("identifier-less fallback result = state:%s terminal:%q, want unavailable/no_identifier", row.State, row.TerminalReason)
				}
				if countType(msgs, protocol.MsgJobOffer) != 0 {
					t.Fatalf("identifier-less OA failure emitted an institutional offer: %+v", msgs)
				}
				actions, err := jobs.ListHumanActions(ctx, true)
				if err != nil {
					t.Fatal(err)
				}
				if len(actions) != 0 {
					t.Fatalf("identifier-less OA failure left actions open: %+v", actions)
				}
				return
			}
			if row.State != job.StateAwaitingHuman {
				t.Fatalf("identified OA fallback state = %s, want awaiting_human", row.State)
			}
			fallback := firstOfType(msgs, protocol.MsgJobOffer)
			if fallback == nil || !strings.HasPrefix(fallback.Payload.(*protocol.JobOfferPayload).OpenURL, cfg.Browser.OpenURLBase+"?") {
				t.Fatalf("identified OA fallback offer = %#v, want institutional OpenURL", fallback)
			}
		})
	}
}

func TestInstitutionalNoEntitlementRequeuesExactlyOnce(t *testing.T) {
	for _, outcome := range []string{"no_entitlement", "document_delivery_available"} {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_requeue_"+outcome, handoffWork())
			runSync(t, b, hello())

			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": outcome}))
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateResolving {
				t.Fatalf("state after first %s = %s, want resolving", outcome, row.State)
			}

			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			requeues := 0
			for _, event := range events {
				if event["kind"] != "browser.no_entitlement_requeue" {
					continue
				}
				requeues++
				detail, ok := event["detail"].(map[string]any)
				if !ok || detail["outcome"] != outcome {
					t.Fatalf("requeue detail = %#v, want outcome %q", event["detail"], outcome)
				}
			}
			if requeues != 1 {
				t.Fatalf("requeue events = %d, want 1", requeues)
			}

			actions, err := jobs.ListHumanActions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 1 || actions[0].Status != "resolved" {
				t.Fatalf("first institutional action = %+v, want resolved", actions)
			}

			if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available", job.Access(false, "")); err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman,
				map[string]any{"reason": "institutional_handoff"}); err != nil {
				t.Fatal(err)
			}
			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": outcome}))

			row, err = jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateUnavailable || row.TerminalReason != outcome {
				t.Fatalf("state/reason after second %s = %s/%q, want unavailable/%q", outcome, row.State, row.TerminalReason, outcome)
			}
			events, err = jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			requeues = 0
			for _, event := range events {
				if event["kind"] == "browser.no_entitlement_requeue" {
					requeues++
				}
			}
			if requeues != 1 {
				t.Fatalf("second %s recorded %d requeue events, want 1", outcome, requeues)
			}
		})
	}
}

func TestRequeuedRouteNeverConvertsOAHandoffBackToInstitution(t *testing.T) {
	const oaURL = "https://oa.example.org/articles/alternate-version.pdf"
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_oa_after_requeue", handoffWork())
	runSync(t, b, hello())

	// The institutional route proves empty and earns its one rediscovery pass.
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": "no_entitlement"}))
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateResolving {
		t.Fatalf("state after first no_entitlement = %s, want resolving", row.State)
	}

	// Rediscovery finds a bot-blocked OA alternate and re-parks with an OA action.
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.OABrowserHandoffActionDetail(oaURL), job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		map[string]any{"reason": "open_access_browser_handoff"}); err != nil {
		t.Fatal(err)
	}

	// The OA alternate also reports no_entitlement. The proven-empty
	// institutional route must not be resurrected via the OA fallback.
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": "no_entitlement"}))
	if countType(msgs, protocol.MsgJobOffer) != 0 {
		t.Fatal("proven-empty institutional route was offered again")
	}
	row, err = jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateUnavailable || row.TerminalReason != "no_entitlement" {
		t.Fatalf("state/reason = %s/%q, want unavailable/no_entitlement", row.State, row.TerminalReason)
	}
	actions, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Status == "open" {
			t.Fatalf("action %d (%s) left open after terminal no_entitlement: %+v", action.ID, action.Kind, action)
		}
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	requeues, fallbacks := 0, 0
	for _, event := range events {
		switch event["kind"] {
		case "browser.no_entitlement_requeue":
			requeues++
		case "browser.oa_handoff_fallback":
			fallbacks++
		}
	}
	if requeues != 1 {
		t.Fatalf("requeue events = %d, want 1", requeues)
	}
	if fallbacks != 0 {
		t.Fatalf("OA action fell back to the proven-empty institutional route %d times, want 0", fallbacks)
	}
}

func TestJobRejectEndsHandoffUnavailable(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_reject", handoffWork())
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgJobReject, id, map[string]any{}))

	row, _ := jobs.Get(ctx, id)
	if row.State != job.StateUnavailable || row.TerminalReason != "browser_rejected" {
		t.Fatalf("rejected job = %+v", row)
	}
	actions, _ := jobs.ListHumanActions(ctx, false)
	for _, a := range actions {
		if a.Kind == handoffActionKind && a.Status == "open" {
			t.Fatal("handoff action still open after reject")
		}
	}
}

func TestHandoffOutcomeIsAuditOnlyAndKeepsActionOpen(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_hfail", handoffWork())
	// hello + first poll offers the handoff once.
	runSync(t, b, hello())
	if !b.offered[id] {
		t.Fatal("handoff was not offered on first sync")
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgHandoffOutcome, id,
		map[string]any{"outcome": "stale_sso", "final_host": "login.openathens.net"}))
	// Recovery lives in the extension (it re-drives the tab through the
	// resolver); the daemon must NOT emit a duplicate job_offer — the
	// deterministic offer URL would be deduplicated without a reload anyway.
	for _, m := range msgs {
		if m.Type == protocol.MsgJobOffer {
			t.Fatalf("unexpected duplicate job_offer after IdP failure: %+v", m)
		}
	}

	row, _ := jobs.Get(ctx, id)
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", row.State)
	}
	actions, _ := jobs.ListHumanActions(ctx, true)
	stillOpen := false
	for _, a := range actions {
		if a.JobID == id && a.Kind == handoffActionKind {
			stillOpen = true
		}
	}
	if !stillOpen {
		t.Fatal("handoff action must stay open after an IdP failure")
	}
	events, _ := jobs.Events(ctx, id)
	var failed, offered bool
	for _, e := range events {
		switch e["kind"] {
		case "browser.handoff_failed":
			detail, _ := e["detail"].(map[string]any)
			if detail["outcome"] != "stale_sso" || detail["final_host"] != "login.openathens.net" {
				t.Fatalf("handoff_failed detail = %+v", detail)
			}
			failed = true
		case "browser.handoff_offered":
			offered = true
		}
	}
	if !failed || !offered {
		t.Fatalf("events missing handoff_failed=%v handoff_offered=%v", failed, offered)
	}
	if !b.offered[id] {
		t.Fatal("offer bookkeeping must be untouched by an IdP failure report")
	}

	// Unknown job: dropped fail-closed, no error, no event.
	runSync(t, b, inFrame(t, protocol.MsgHandoffOutcome, "job_unknown_0001",
		map[string]any{"outcome": "auth_error", "final_host": "idp.example.edu"}))
}

func TestDaemonSideCancelEmitsCancelFrameOnce(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_dcancel", handoffWork())
	runSync(t, b, hello()) // offers the job

	if err := jobs.Cancel(ctx, id, "user request"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b)
	c := firstOfType(msgs, protocol.MsgCancel)
	if c == nil || c.JobID != id {
		t.Fatalf("expected a cancel frame for %s, got %v", id, msgs)
	}
	// Not repeated on the next poll.
	msgs2, _ := runSync(t, b)
	if firstOfType(msgs2, protocol.MsgCancel) != nil {
		t.Fatal("cancel frame emitted more than once")
	}
}

func TestOpenURLPMIDFallbackAndYear(t *testing.T) {
	got := OpenURL("https://openurl.example.edu/resolve", work.Work{PMID: "123456", Title: "T", Year: 2020})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("rft_id") != "info:pmid/123456" {
		t.Fatalf("rft_id = %q", q.Get("rft_id"))
	}
	if q.Get("rft.date") != "2020" {
		t.Fatalf("rft.date = %q", q.Get("rft.date"))
	}
}

// A monograph's title in rft.atitle asks the resolver for an ARTICLE by that
// name. Real libraries answer that query with nothing, or with a review of the
// book — which is how printed books reached the catalogue as an unmatchable
// article lookup. An ISBN-only work must be described as a book instead.
func TestOpenURLDescribesAnISBNOnlyWorkAsABook(t *testing.T) {
	got := OpenURL("https://openurl.example.edu/resolve", work.Work{
		ISBN: "9781576753484", Title: "Evaluating training programs: the four levels",
		Authors: []string{"Donald L. Kirkpatrick"}, Year: 2012,
	})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("rft.btitle") != "Evaluating training programs: the four levels" {
		t.Fatalf("rft.btitle = %q", q.Get("rft.btitle"))
	}
	if q.Has("rft.atitle") {
		t.Fatalf("book carries rft.atitle = %q; an article-title query is the bug", q.Get("rft.atitle"))
	}
	if q.Get("rft.isbn") != "9781576753484" {
		t.Fatalf("rft.isbn = %q", q.Get("rft.isbn"))
	}
	if q.Get("rft_val_fmt") != "info:ofi/fmt:kev:mtx:book" || q.Get("rft.genre") != "book" {
		t.Fatalf("book metadata format = %q genre = %q", q.Get("rft_val_fmt"), q.Get("rft.genre"))
	}
}

// A chapter with a Springer DOI is fetchable and stays article-shaped, so the
// book branch cannot swallow the case institutional access actually resolves.
func TestOpenURLKeepsArticleShapeWhenAStrongIdentifierExists(t *testing.T) {
	for _, test := range []struct {
		name string
		w    work.Work
	}{
		{"doi wins over isbn", work.Work{DOI: "10.1007/978-1-4613-3087-5_2", ISBN: "9781461330875", Title: "Equity theory"}},
		{"pmid wins over isbn", work.Work{PMID: "123456", ISBN: "9781461330875", Title: "Equity theory"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			u, err := url.Parse(OpenURL("https://openurl.example.edu/resolve", test.w))
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if q.Get("rft.atitle") != "Equity theory" || q.Has("rft.btitle") || q.Has("rft.isbn") {
				t.Fatalf("identifier-bearing work rendered as a book: %v", q)
			}
		})
	}
}

func TestOpenURLUsesSelectedResolverProfileForPrimoNDEAndVE(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	cfg.Browser.OpenURLBase = "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE"
	cfg.Browser.Resolvers = map[string]config.Institution{
		"institute": {OpenURLBase: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
	}
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, cfg, b.Version)
	for _, test := range []struct {
		name, resolver, wantPath, wantVID string
	}{
		{name: "NDE default", wantPath: "/nde/openurl", wantVID: "61EXL_INST:61EXL_NDE"},
		{name: "VE named", resolver: "institute", wantPath: "/discovery/openurl", wantVID: "61INS_INST:INS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := b.offer(job.Row{ID: "job-profile", Work: handoffWork(), Policy: job.Policy{Resolver: test.resolver}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true}, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			message, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(message.Payload.(*protocol.JobOfferPayload).OpenURL)
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != test.wantPath || u.Query().Get("vid") != test.wantVID {
				t.Fatalf("resolver URL = %s, want path %s and vid %s", u, test.wantPath, test.wantVID)
			}
			if u.Query().Get("rft_id") != "info:doi/10.1002/example.42" {
				t.Fatalf("rft_id = %q", u.Query().Get("rft_id"))
			}
		})
	}
}

func TestOfferLoginRoutingIsPerResolverProfile(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	cfg.Browser.ShibbolethEntityID = "https://idp.example.edu/entity"
	cfg.Browser.ProquestAccountID = "12345"
	cfg.Browser.Resolvers = map[string]config.Institution{
		// A named institution carries its own login identity...
		"institute": {
			OpenURLBase:        "https://onesearch.library.example-institute.edu/discovery/openurl",
			ShibbolethEntityID: "https://idp.example-institute.edu/idp/shibboleth",
			ProquestAccountID:  "67890",
		},
		// ...and one without an identity gets none (no default leakage).
		"bare": {OpenURLBase: "https://library.example.edu/openurl"},
	}
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, cfg, b.Version)

	for _, test := range []struct {
		name, resolver, wantEntityID, wantAccountID string
	}{
		{name: "default", wantEntityID: "https://idp.example.edu/entity", wantAccountID: "12345"},
		{name: "named carries own identity", resolver: "institute", wantEntityID: "https://idp.example-institute.edu/idp/shibboleth", wantAccountID: "67890"},
		{name: "named without identity leaks nothing", resolver: "bare"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := b.offer(job.Row{ID: "job-login-route", Work: handoffWork(), Policy: job.Policy{Resolver: test.resolver}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true}, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			message, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := message.Payload.(*protocol.JobOfferPayload).LoginEntityID; got != test.wantEntityID {
				t.Fatalf("login_entity_id = %q, want %q", got, test.wantEntityID)
			}
			if got := message.Payload.(*protocol.JobOfferPayload).ProquestAccountID; got != test.wantAccountID {
				t.Fatalf("proquest_account_id = %q, want %q", got, test.wantAccountID)
			}
		})
	}
}

// SweepAdoptions adopts a settled file WITHOUT any hello/extension connection —
// the daemon owns completion; the browser plane is only a delivery hint.
func TestSweepAdoptionsAdoptsWithoutHello(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_sweep", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	// No hello was ever sent; poll() would offer nothing. The sweeper still adopts.
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	row, _ := jobs.Get(ctx, id)
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("sweeper did not adopt: %+v", row)
	}
}

// RunSweeper must survive a transient store error: a dead adoption loop would
// silently strand every subsequently downloaded PDF, and the daemon supervisor
// does not watch this goroutine. Closing the DB forces every sweep to error;
// the loop must keep running and return nil only on cancellation (the pre-fix
// code returned the store error on the first failing tick).
func TestRunSweeperSurvivesStoreError(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	if err := jobs.S.DB().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.RunSweeper(ctx, time.Millisecond) }()
	time.Sleep(25 * time.Millisecond) // let several sweeps fail
	select {
	case err := <-done:
		t.Fatalf("RunSweeper exited early with %v; a transient store error must not kill the adoption loop", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSweeper returned %v after cancel; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweeper did not return after cancel")
	}
}

func TestSweepTerminalAdoptionsRemovesOnlyTerminalDirs(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	root := cfg.EffectiveAdoptionRoot()

	readyID := park(t, jobs, "wr_ready", handoffWork())
	if err := jobs.Transition(ctx, readyID, job.StateAwaitingHuman, job.StateValidating, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, readyID, job.StateValidating, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	unavailableID := park(t, jobs, "wr_unavailable", handoffWork())
	if err := jobs.Transition(ctx, unavailableID, job.StateAwaitingHuman, job.StateUnavailable, nil, job.WithTerminalReason("no_entitlement")); err != nil {
		t.Fatal(err)
	}
	awaitingID := park(t, jobs, "wr_awaiting", handoffWork())

	place := func(parts ...string) {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("%PDF-1.4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	place(readyID, "paper.pdf")
	place(unavailableID, "paper.pdf")
	place(awaitingID, "paper.pdf")
	place("rejected", "wr_x", "bad.pdf")
	place("job_stray_dir", "stray.pdf")

	if err := b.SweepTerminalAdoptions(ctx); err != nil {
		t.Fatal(err)
	}

	gone := func(name string) {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err = %v", name, err)
		}
	}
	kept := func(name string) {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected %s preserved: %v", name, err)
		}
	}
	gone(readyID)       // ready: PDF is promoted to the artifact store
	gone(unavailableID) // terminal: nothing here the user needs
	kept(awaitingID)    // non-terminal handoff may still receive a download
	kept("rejected")    // user-facing rejected files are preserved
	kept("job_stray_dir")
}

// parkWithPolicyMode parks a handoff-ready job carrying an explicit policy
// access mode, so a test can make the job's mode differ from the daemon's.
func parkWithPolicyMode(t *testing.T, jobs *job.Store, reqID, doi, mode string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, work.Work{DOI: doi}, "", "",
		job.Policy{AccessMode: mode, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
		nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available",
		job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestJobOfferAdvertisesTheJobsOwnAccessMode pins the browser half of the
// access_mode_override fix: the daemon must not tell the extension something
// false about the job in front of it.
//
// Note the shipped extension parses access_mode and never branches on it, so
// this frame is not an enforcement point today — unattended-download capability
// is gated by granted host permissions. The plumbing is still correct, and the
// hard requirement is the one below: papio-browser/1 admits only assisted and
// delegated, so a conservative job must never reach an offer at all.
func TestJobOfferAdvertisesTheJobsOwnAccessMode(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	if cfg.AccessMode != config.ModeDelegated {
		t.Fatalf("fixture daemon mode = %q, want delegated so the job policy can differ", cfg.AccessMode)
	}
	parkWithPolicyMode(t, jobs, "wr_offer_mode_assisted", "10.1000/offer-mode", config.ModeAssisted)

	msgs, _ := runSync(t, b, hello())
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatal("missing job offer")
	}
	if got := offer.Payload.(*protocol.JobOfferPayload).AccessMode; got != config.ModeAssisted {
		t.Fatalf("offer access_mode = %q, want the job's own %q; the daemon-wide %q leaked into the frame",
			got, config.ModeAssisted, cfg.AccessMode)
	}
}

// TestStaleConservativeParkedJobIsSkippedInsteadOfKillingTheSession covers the
// hazard the policy-first read introduces. papio-browser/1 allows only assisted
// and delegated in a job_offer, and a non-nil error out of Sync is treated by
// the native host as a dead connection — so emitting a conservative offer would
// disconnect the extension entirely rather than drop one row. A job that
// resolves to conservative must be skipped silently, and its siblings must
// still be offered.
func TestStaleConservativeParkedJobIsSkippedInsteadOfKillingTheSession(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	stale := parkWithPolicyMode(t, jobs, "wr_offer_mode_stale", "10.1000/stale-conservative", config.ModeConservative)
	live := parkWithPolicyMode(t, jobs, "wr_offer_mode_live", "10.1000/live-delegated", config.ModeDelegated)

	// runSync fails the test on a Sync error, which is the tear-down this
	// guards against: the native host treats any error out of Sync as a dead
	// connection and drops the whole session.
	msgs, _ := runSync(t, b, hello())
	var offered []string
	for _, msg := range msgs {
		if msg.Type == protocol.MsgJobOffer {
			offered = append(offered, msg.JobID)
		}
	}
	if !slices.Contains(offered, live) {
		t.Fatalf("offered = %v, want the delegated job %s", offered, live)
	}
	if slices.Contains(offered, stale) {
		t.Fatalf("offered = %v, must not contain the conservative job %s", offered, stale)
	}
}

// TestFocusHandoffsRefusesAnUnofferableJob covers the second offerability call
// site. actions.open reports how many focus requests reached the extension, and
// the CLI suppresses its own explicit-open fallback when a session is live — so
// counting a job the offer loop will silently skip tells the user papio acted
// when nothing happened and nothing will.
func TestFocusHandoffsRefusesAnUnofferableJob(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	stale := parkWithPolicyMode(t, jobs, "wr_focus_stale", "10.1000/focus-stale", config.ModeConservative)
	live := parkWithPolicyMode(t, jobs, "wr_focus_live", "10.1000/focus-live", config.ModeDelegated)
	// A holder at or above the focus floor is required before focus is accepted.
	runSync(t, b, hello())

	queued, sessionLive, err := b.FocusHandoffs(ctx, []string{stale, live})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive {
		t.Fatal("session not live; the fixture hello did not establish a holder")
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want 1: only the delegated job can actually be focused", queued)
	}
}

// TestSyncResponseFitsResultCap pins the response half of the transport
// invariant that TestSyncRequestFitsMaxBrowserFrame pins for requests. One
// browser.sync response MUST fit ipc.MaxResultBytes: an oversized response
// fails the native host's Sync call, and the host treats any Sync failure as
// fatal — it says goodbye and the daemon tears down the live browser session,
// losing the capture and re-parking every in-flight handoff.
//
// The bound holds because (a) b.frame self-validates every outbound frame
// through the strict decoder, so no single frame exceeds
// protocol.MaxBrowserMessageBytes, (b) the host relays at most one inbound
// frame per sync, so at most one max-size solicited response can ride one
// response, and (c) the offer and focus batches are capped by
// maxOutstandingOffers and maxFocusFramesPerPoll. Loosening any of those trips
// this test.
func TestSyncResponseFitsResultCap(t *testing.T) {
	b := &Bridge{}
	// A job_offer is the largest frame the daemon emits unsolicited, and every
	// one of its variable fields is bounded by JobOfferPayload.validate: openurl
	// 4000, provider_hosts 20 entries, expected DOI 300 and title 500. Build one
	// at those legal maxima so the arithmetic below is a real ceiling, not a
	// sample. Only encoded length matters, so identical hosts are fine.
	hosts := make([]string, 20)
	for i := range hosts {
		hosts[i] = strings.Repeat("h", 60) + ".example.com"
	}
	openURLPrefix := "https://resolver.example.edu/openurl?"
	offer, err := b.frame(protocol.MsgJobOffer, "job_"+strings.Repeat("f", 26), protocol.JobOfferPayload{
		OpenURL:       openURLPrefix + strings.Repeat("k", 4000-len(openURLPrefix)),
		ProviderHosts: hosts,
		Expected: &protocol.JobOfferExpected{
			DOI:   "10.1234/" + strings.Repeat("d", 292),
			Title: strings.Repeat("t", 500),
		},
		AccessMode:        "delegated",
		ProquestAccountID: strings.Repeat("9", 64),
		RequiresAuth:      true,
		ExpiresAt:         "2026-08-04T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cancel and focus frames carry only a job id and an empty payload, so
	// sizing every batched frame as a maximal offer is deliberately pessimistic.
	batched := (maxOutstandingOffers + maxFocusFramesPerPoll) * len(offer)
	worst := protocol.MaxBrowserMessageBytes + batched
	if worst > ipc.MaxResultBytes {
		t.Fatalf("worst-case sync response %d bytes exceeds ipc.MaxResultBytes %d: one max-size solicited response (%d) + %d batched frames of %d bytes",
			worst, ipc.MaxResultBytes, protocol.MaxBrowserMessageBytes, maxOutstandingOffers+maxFocusFramesPerPoll, len(offer))
	}
}
