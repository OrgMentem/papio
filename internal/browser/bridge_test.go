// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
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
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/protocol"
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
	cfg.AccessMode = config.ModeMaximal
	cfg.DataDir = data
	cfg.Browser.ExtensionID = strings.Repeat("a", 32)
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Browser.ActionExpirySeconds = 1800
	jobs := &job.Store{S: db}
	watches := watch.NewStore(db)
	triageService := triage.New(db, watches, jobs)
	previewServer := preview.New()
	t.Cleanup(func() {
		if err := previewServer.Shutdown(context.Background()); err != nil {
			t.Errorf("close preview: %v", err)
		}
	})
	svc := app.New(cfg, jobs, artifacts, nil)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 3},
			Text:       pdf.TextReport{Chars: 4000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
	return NewBridge(jobs, svc, triageService, &watch.Runner{Store: watches}, previewServer, cfg, "0.1.0-test", []string{"browser_handoff"}), jobs, cfg, data
}

func handoffWork() work.Work {
	return work.Work{DOI: "10.1002/example.42", Title: "An Institutional Paper", Authors: []string{"Lovelace, Ada"}, Year: 2024}
}

// park drives a fresh job into awaiting_human with an open openurl_handoff
// action, exactly as the app's exhaustion routing does.
func park(t *testing.T, jobs *job.Store, reqID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, w, "", "",
		job.Policy{AccessMode: config.ModeMaximal, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
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
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available"); err != nil {
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
		"browser_handoff", pageAcquireFeature, triageSnapshotFeature, triageMutationsFeature, reviewPreviewFeature,
	}) {
		t.Fatalf("features = %v, want required bridge feature set", payload.Features)
	}
}

func TestHelloAckFeatureCapReservesMandatoryFeatures(t *testing.T) {
	b, _, _, _ := newBridge(t)
	features := make([]string, 32)
	for i := range features {
		features[i] = strings.Repeat("x", i+1)
	}
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.cfg, b.Version, features)

	msgs, _ := runSync(t, b, hello())
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil {
		t.Fatalf("no hello_ack in %v", msgs)
	}
	got := ack.Payload.(*protocol.HelloAckPayload).Features
	want := append(append([]string(nil), features[:28]...),
		pageAcquireFeature, triageSnapshotFeature, triageMutationsFeature, reviewPreviewFeature)
	if !slices.Equal(got, want) {
		t.Fatalf("features = %v, want %v", got, want)
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

func TestReviewPreviewAndResolveNeverLeakQuarantinePath(t *testing.T) {
	b, jobs, _, data := newBridge(t)
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id, err := jobs.CreateRequest(context.Background(), "wr_browser_review",
		work.Work{DOI: "10.1000/review", Title: "Review"}, "", "",
		job.Policy{AccessMode: config.ModeMaximal, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
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
	actionID, err := jobs.OpenHumanAction(context.Background(), id, "verify_identity", "review the PDF",
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
	restarted := NewBridge(jobs, active.svc, active.triage, active.watchRunner, active.preview, cfg, active.Version, active.Features)
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

func TestOABrowserHandoffOffersCandidateThenFallsBackToInstitution(t *testing.T) {
	const oaURL = "https://oa.example.org/articles/blocked-paper.pdf"
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_oa_fallback", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.OABrowserHandoffActionDetail(oaURL)); err != nil {
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
		extraAction  string // additional open action kind expected (human_auth/terms)
		terminal     string
	}
	cases := map[string]expect{
		"cancelled":                   {state: job.StateCancelled, actionStatus: "cancelled"},
		"no_entitlement":              {state: job.StateUnavailable, actionStatus: "resolved", terminal: "no_entitlement"},
		"document_delivery_available": {state: job.StateUnavailable, actionStatus: "resolved", terminal: "document_delivery_available"},
		"wrong_work":                  {state: job.StateNeedsReview, actionStatus: "resolved"},
		"ui_changed":                  {state: job.StateNeedsReview, actionStatus: "resolved"},
		"rate_limited":                {state: job.StateRetryWait, actionStatus: "resolved"},
		"human_auth_required":         {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "human_auth_required"},
		"terms_acceptance_required":   {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "terms_acceptance_required"},
	}
	for outcome, want := range cases {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_"+outcome, handoffWork())
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
			extraOpen := false
			for _, a := range actions {
				if a.Kind == handoffActionKind {
					handoffStatus = a.Status
				}
				if want.extraAction != "" && a.Kind == want.extraAction && a.Status == "open" {
					extraOpen = true
				}
			}
			if handoffStatus != want.actionStatus {
				t.Fatalf("openurl_handoff status = %q, want %q", handoffStatus, want.actionStatus)
			}
			if want.extraAction != "" && !extraOpen {
				t.Fatalf("expected an open %s action", want.extraAction)
			}
		})
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

func TestOpenURLUsesSelectedResolverProfileForPrimoNDEAndVE(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	cfg.Browser.OpenURLBase = "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE"
	cfg.Browser.Resolvers = map[string]config.Institution{
		"institute": {OpenURLBase: "https://onesearch.library.example-institute.edu/discovery/openurl?vid=61INS_INST:INS"},
	}
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, cfg, b.Version, b.Features)
	for _, test := range []struct {
		name, resolver, wantPath, wantVID string
	}{
		{name: "NDE default", wantPath: "/nde/openurl", wantVID: "61EXL_INST:61EXL_NDE"},
		{name: "VE named", resolver: "institute", wantPath: "/discovery/openurl", wantVID: "61INS_INST:INS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := b.offer(job.Row{ID: "job-profile", Work: handoffWork(), Policy: job.Policy{Resolver: test.resolver}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true})
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
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, cfg, b.Version, b.Features)

	for _, test := range []struct {
		name, resolver, wantEntityID, wantAccountID string
	}{
		{name: "default", wantEntityID: "https://idp.example.edu/entity", wantAccountID: "12345"},
		{name: "named carries own identity", resolver: "institute", wantEntityID: "https://idp.example-institute.edu/idp/shibboleth", wantAccountID: "67890"},
		{name: "named without identity leaks nothing", resolver: "bare"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := b.offer(job.Row{ID: "job-login-route", Work: handoffWork(), Policy: job.Policy{Resolver: test.resolver}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true})
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
