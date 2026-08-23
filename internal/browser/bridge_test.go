// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/grab"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/protocol"
	"papio/internal/pulse"
	"papio/internal/resolver"
	"papio/internal/retraction"
	"papio/internal/routes"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
)

// compressAdoptionScanDeadline shrinks the hung-ReadDir bound for one test.
// The tests that prove the latch behaviour block a ReadDir seam forever, so
// they always pay the full deadline; at the production 2s that is four
// seconds of pure sleeping in this package alone, which is the slowest in the
// tree. 100ms is still orders of magnitude above any real listing latency, so
// nothing else about the tests changes. No test in this package calls
// t.Parallel, so a package-level knob is safe.
func compressAdoptionScanDeadline(t *testing.T) {
	t.Helper()
	previous := AdoptionScanDeadline
	AdoptionScanDeadline = 100 * time.Millisecond
	t.Cleanup(func() { AdoptionScanDeadline = previous })
}

func newBridge(t *testing.T) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	return newBridgeWithHoldings(t, nil)
}

func newBridgeWithHoldings(t *testing.T, holdings holdingsProvider) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	return newBridgeWithHoldingsAndZotio(t, holdings, nil)
}

// tweak, when supplied, adjusts the pinned config before the service and
// bridge are built — the seam a test needs when it must exercise an adoption
// root layout other than the default pinned one.
func newBridgeWithHoldingsAndZotio(t *testing.T, holdings holdingsProvider, zotioService *zotio.Service, tweak ...func(*config.Config)) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	ctx := context.Background()
	// Already migrated: 285 tests each running the full migration set under
	// -race is what pushed this package past CI's per-package timeout.
	data := storetest.DataDir(t)
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
	// Adoption is a filesystem contract: pin the root to this test's data
	// dir so nothing here ever reaches the real <downloads>/papio default.
	cfg.Browser.AdoptionRoot = filepath.Join(data, "adoptions")
	cfg.Browser.ExtensionID = strings.Repeat("a", 32)
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Browser.ActionExpirySeconds = 1800
	for _, fn := range tweak {
		fn(&cfg)
	}
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
	return NewBridge(jobs, svc, triageService, &watch.Runner{Store: watches}, previewServer, captureStore, holdings, zotioService, cfg, "0.1.0-test"), jobs, cfg, data
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

func helloWithFeatures(t *testing.T, extensionVersion string, features ...string) json.RawMessage {
	t.Helper()
	payload := map[string]any{"extension_version": extensionVersion}
	if len(features) != 0 {
		payload["features"] = features
	}
	return inFrame(t, protocol.MsgHello, "", payload)
}

func helloWithAdapterVersions(t *testing.T, extensionVersion string, adapterVersions map[string]string) json.RawMessage {
	t.Helper()
	return inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": extensionVersion,
		"adapter_versions":  adapterVersions,
	})
}
func effectPermitHolder(t *testing.T, b *Bridge) {
	t.Helper()
	b.holder = &browserSession{
		ID: "permit-test-holder", ExtensionVersion: "0.14.0",
		Features:   []string{providerDriveEpochV1Feature, effectPermitFeature},
		LastSyncAt: b.now(),
	}
	b.epoch = 0
}

func effectPermitOffer(t *testing.T, jobs *job.Store, id, attempt, domain string) {
	t.Helper()
	if err := jobs.RecordEvent(context.Background(), id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic",
		"revision": "1", "safety_domain": domain,
	}); err != nil {
		t.Fatal(err)
	}
}

func permitOutcome(t *testing.T, frames []json.RawMessage) string {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("frames=%d, want one", len(frames))
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := msg.Payload.(*protocol.ProviderDriveEpochStartResultPayload); ok {
		return p.Outcome
	}
	if p, ok := msg.Payload.(*protocol.ProviderDriveEpochResultPayload); ok {
		return p.Outcome
	}
	if p, ok := msg.Payload.(*protocol.TermsEffectStartResultPayload); ok {
		return p.Outcome
	}
	if p, ok := msg.Payload.(*protocol.TermsEffectResultPayload); ok {
		return p.Outcome
	}
	t.Fatalf("unexpected permit payload %T", msg.Payload)
	return ""
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
func countRawType(t *testing.T, frames []json.RawMessage, typ string) int {
	t.Helper()
	count := 0
	for _, frame := range frames {
		msg, err := protocol.DecodeBrowserMessage(frame)
		if err != nil {
			t.Fatalf("outbound frame failed protocol decode: %v", err)
		}
		if msg.Type == typ {
			count++
		}
	}
	return count
}

func pageCaptureFixture(html []byte) []byte {
	return []byte(fmt.Sprintf("<!-- papio-fixture provider=\"sage\" scenario=\"observed\" origin=\"https://sagepub.com/\" captured=\"2026-08-10T00:00:00Z\" -->\n%s", html))
}

func pageCapturePayload(t *testing.T, html []byte) protocol.PageCapturePayload {
	t.Helper()
	fixture := pageCaptureFixture(html)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(fixture); err != nil {
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
		Bytes:          int64(len(fixture)),
		Body:           base64.StdEncoding.EncodeToString(compressed.Bytes()),
	}
}

func TestPageCaptureDisabledDoesNotStore(t *testing.T) {
	b, _, cfg, data := newBridge(t)
	cfg.Captures.Enabled = false
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
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
			if len(listed) != 1 || listed[0].Size != int64(len(pageCaptureFixture([]byte("<html>survived</html>")))) {
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
	content.RequestID = request.RequestID
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

// TestCaptureLateResultWinsOverConcurrentTimeout covers the outcome side of
// ledger finding papio-373ff6c00ec87dbc: Capture's select race could pick the
// ctx.Done() arm even though a result already sat in the buffered
// pending.result channel, reporting "timeout" for a capture that actually
// succeeded — and silently orphaning the stored file (on disk, never
// reported).
//
// Scope, stated honestly: this test does NOT force the racing arm. Sending on
// pending.result wakes Capture's select immediately, before the cancel below
// is reached, so the result arm is what actually runs here and the recheck
// added inside the ctx.Done() arm is not exercised. Forcing the other arm
// would need both cases ready at the instant the select evaluates, which is
// not reachable from outside Capture — and even then Go chooses between two
// ready cases pseudo-randomly. So treat this as pinning the delivered-result
// OUTCOME, with the sibling test pinning the genuine-timeout outcome; the
// recheck itself is verified by reading, not by this test. Removing the
// recheck does not make either test fail.
func TestCaptureLateResultWinsOverConcurrentTimeout(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan CaptureResult, 1)
	go func() {
		resultCh <- b.Capture(ctx, CaptureRequest{
			URL: "https://sagepub.com/article/42", Provider: "sage", Scenario: "success",
		})
	}()

	deadline := time.Now().Add(time.Second)
	var pending *pendingPageCapture
	for {
		b.mu.Lock()
		pending = b.pendingCaptures[testSessionID]
		b.mu.Unlock()
		if pending != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture request was not queued")
		}
		time.Sleep(time.Millisecond)
	}

	const storedPath = "/tmp/papio-late-capture.html"
	b.mu.Lock()
	delete(b.pendingCaptures, testSessionID)
	pending.result <- CaptureResult{
		RequestID: pending.payload.RequestID,
		Outcome:   "captured",
		Path:      storedPath,
	}
	b.mu.Unlock()
	cancel()

	select {
	case result := <-resultCh:
		if result.Outcome != "captured" || result.RequestID != pending.payload.RequestID || result.Path != storedPath {
			t.Fatalf("capture result = %#v, want the delivered success rather than a timeout", result)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not return")
	}

	b.mu.Lock()
	leaked := b.pendingCaptures[testSessionID] != nil
	b.mu.Unlock()
	if leaked {
		t.Fatal("pending capture entry survived a resolved request, leaking the busy flag")
	}
}

// TestCaptureGenuineTimeoutClearsPendingState pins the other half of the
// papio-373ff6c00ec87dbc fix: an undelivered capture must still report a
// timeout, and the recheck added to close the race must not leave the
// pending/busy bookkeeping behind — a stuck entry would refuse every later
// capture on the session as "busy" forever.
func TestCaptureGenuineTimeoutClearsPendingState(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan CaptureResult, 1)
	go func() {
		resultCh <- b.Capture(ctx, CaptureRequest{
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
	cancel()

	var result CaptureResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("capture did not return")
	}
	if result.Outcome != "timeout" {
		t.Fatalf("capture outcome = %q, want timeout for an undelivered capture", result.Outcome)
	}

	b.mu.Lock()
	pendingCount := len(b.pendingCaptures)
	b.mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pendingCaptures = %d after a genuine timeout, want 0 (leaked busy flag)", pendingCount)
	}
}

// TestPageCaptureRedirectedToDifferentHostStillCorrelates pins the regression
// the host-guard revert exists to prevent (bc3f4b2). payload.Host is not the
// requested host — extension/src/capture.ts sets it from location.origin,
// the host the tab actually LANDED on — so an ordinary cross-host redirect
// (www canonicalization, a CDN host swap, an SSO round-trip that returns to
// a different host) makes the requested and landed hosts differ even though
// the capture genuinely succeeded. Correlation matches on the echoed
// request_id and never on a host: a build that reintroduces a requested-host
// match fails this test the same way it silently downgraded a real
// "captured" result to "nav_failed" (pending.path stays empty, so
// page_capture_request_result's "captured" is rewritten to "nav_failed").
func TestPageCaptureRedirectedToDifferentHostStillCorrelates(t *testing.T) {
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

	// The tab navigated to https://sagepub.com/article/42 but the provider
	// redirected it onto a CDN host before the content frame captured
	// location.origin — same session, provider, and scenario as the
	// pending request, but a different landed host.
	redirected := pageCapturePayload(t, []byte("<html>captured after redirect</html>"))
	redirected.Host = "cdn.sagepub.com"
	redirected.Scenario = "success"
	redirected.AdapterID = "sage"
	redirected.RequestID = request.RequestID
	runSync(t, b, inFrame(t, protocol.MsgPageCapture, "", redirected))

	runSync(t, b, inFrame(t, protocol.MsgPageCaptureRequestResult, "", protocol.PageCaptureRequestResultPayload{
		RequestID: request.RequestID, Outcome: "captured",
	}))

	select {
	case result := <-resultCh:
		if result.Outcome != "captured" || result.RequestID != request.RequestID || result.Path == "" {
			t.Fatalf("capture result = %#v, want captured with a stored path despite the host redirect", result)
		}
		if _, err := os.Stat(result.Path); err != nil {
			t.Fatalf("stored capture path: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture result did not correlate")
	}
}

// papio-85a7420f4cd2564f: correlation used to match on session + provider +
// scenario alone, so an UNSOLICITED capture — the developer capture panel's
// captureFixture, which answers no pending request — could satisfy a
// concurrent CLI `papio adapter capture` waiting on the same session for the
// same pair and hand that caller the other capture's file path, with no error
// surfaced. The requested capture now echoes request_id and an unsolicited
// one omits it, which is what makes the two distinguishable.
func TestUnsolicitedPageCaptureCannotSatisfyPendingRequest(t *testing.T) {
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

	// Same session, same provider, same scenario — everything the old match
	// keyed on — but no echoed request id, because nobody asked for it.
	unsolicited := pageCapturePayload(t, []byte("<html>panel capture</html>"))
	unsolicited.Scenario = "success"
	unsolicited.AdapterID = "sage"
	runSync(t, b, inFrame(t, protocol.MsgPageCapture, "", unsolicited))

	b.mu.Lock()
	bound := b.pendingCaptures[b.holder.ID].path
	b.mu.Unlock()
	if bound != "" {
		t.Fatalf("unsolicited capture bound to the pending request: path = %q", bound)
	}

	// The requested capture then arrives for real and must win the binding.
	requested := pageCapturePayload(t, []byte("<html>requested capture</html>"))
	requested.Scenario = "success"
	requested.AdapterID = "sage"
	requested.RequestID = request.RequestID
	runSync(t, b,
		inFrame(t, protocol.MsgPageCapture, "", requested),
		inFrame(t, protocol.MsgPageCaptureRequestResult, "", protocol.PageCaptureRequestResultPayload{
			RequestID: request.RequestID, Outcome: "captured",
		}),
	)
	select {
	case result := <-resultCh:
		if result.Outcome != "captured" || result.Path == "" {
			t.Fatalf("capture result = %#v, want captured with the requested capture's path", result)
		}
		stored, err := os.ReadFile(result.Path)
		if err != nil {
			t.Fatalf("stored capture path: %v", err)
		}
		if !bytes.Contains(stored, []byte("requested capture")) {
			t.Fatalf("bound path holds the wrong capture: %q", stored)
		}
	case <-time.After(time.Second):
		t.Fatal("capture result did not correlate")
	}
}

// Correlating a capture needs the extension to echo request_id, which nothing
// below CaptureRequestIDMinExtensionVersion does. Without this gate the
// capture runs, the page is stored, pending.path stays empty, and the
// page_capture_request_result handler rewrites "captured" into "nav_failed:
// capture content was not stored" — a false failure for work that succeeded.
// Refuse up front and name the reason instead. The global MinExtensionVersion
// deliberately does NOT move for this, so the same old extension must still be
// seated and serving handoffs.
func TestCaptureRefusesAnExtensionThatCannotEchoRequestID(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, helloAs("0.9.0"))

	result := b.Capture(context.Background(), CaptureRequest{
		URL: "https://sagepub.com/article/42", Provider: "sage", Scenario: "success",
	})
	if result.Outcome != "not_permitted" {
		t.Fatalf("capture outcome = %q, want not_permitted", result.Outcome)
	}
	if !strings.Contains(result.Detail, "0.9.0") ||
		!strings.Contains(result.Detail, CaptureRequestIDMinExtensionVersion) {
		t.Fatalf("detail = %q, want both the connected and required versions named", result.Detail)
	}

	b.mu.Lock()
	pendingCount := len(b.pendingCaptures)
	seated := b.holder != nil && !b.holder.Outdated
	b.mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pendingCaptures = %d after a refused capture, want 0", pendingCount)
	}
	if !seated {
		t.Fatal("the refused extension lost the bridge; the capture floor must not act as MinExtensionVersion")
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
		receipt["path"] != listed[0].Path || receipt["size_bytes"] != float64(len(pageCaptureFixture([]byte("<html>fixture</html>")))) {
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
	// This is the fail-closed 32-feature cap, exactly saturated
	// (dev/active/claim-observation-protocol.md §1): institutionalAuthenticationClaimFeature
	// is the 32nd and last slot. No feature may be added after it without
	// retiring or consolidating an existing one first.
	if !slices.Equal(payload.Features, []string{
		pageAcquireFeature, triageSnapshotFeature, triageSnapshotSchema2Feature, triageMutationsFeature, reviewPreviewFeature, statsFeature, pageCaptureFeature, pageCaptureRequestFeature, activityFeedFeature, triageCountsSchema2Feature, sessionEvidenceFeature, deliveryContextFeature, pageCaptureTermsFeature, pageBulkAcquireFeature, triageSnapshotSchema3Feature, triageSnapshotSchema4Feature, pdfGrabV1Feature, handoffLinkV1Feature, providerDirectGetV1Feature, providerDriveEpochV1Feature, protocol.EffectPermitFeature, institutionalMaterializationFeature,
		surfacePresenceFeature, workPulseFeature, activityPageV2Feature, pageBulkCohortV2Feature, triageCountsSchema3Feature, triageSnapshotSchema5Feature, sessionRolesFeature, pdfGrabSuggestV1Feature, surfaceCloseFeature,
		institutionalAuthenticationClaimFeature,
	}) {
		t.Fatalf("features = %v", payload.Features)
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

// nextSnapshotLimit only decides how fast the search for a fitting page
// converges, so the properties that matter are that it always makes progress
// (or the loop spins forever) and never proposes a page as large as the one
// that just overflowed (same). Landing near the right size is the performance
// claim; the caller re-measures, so overshooting is merely another pass.
func TestNextSnapshotLimitAlwaysMakesProgress(t *testing.T) {
	cap := protocol.MaxBrowserMessageBytes
	for _, tc := range []struct {
		name  string
		items int
		size  int
		want  int
	}{
		{"barely over the cap steps down by one", 100, cap + 1, 99},
		{"three times over lands near a third", 99, cap * 3, 33},
		{"ten times over lands near a tenth", 100, cap * 10, 10},
		{"a page that fits is still forced downward", 100, cap - 1, 99},
		{"two items cannot propose two", 2, cap * 9, 1},
		{"one item is already the floor", 1, cap * 9, 1},
		{"an unmeasurable frame falls back to the floor", 50, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextSnapshotLimit(tc.items, tc.size)
			if got != tc.want {
				t.Fatalf("nextSnapshotLimit(%d, %d) = %d, want %d", tc.items, tc.size, got, tc.want)
			}
			if got >= tc.items && tc.items > 1 {
				t.Fatalf("no progress: %d proposed for %d items", got, tc.items)
			}
			if got < 1 {
				t.Fatalf("proposed an empty page: %d", got)
			}
		})
	}
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

// parkDocumentDelivery parks a fresh job awaiting_human with an open
// document_delivery reconciliation action and a matching live
// delivery_requests row, mirroring what openDeliveryReconciliationAction
// (internal/app/app.go) leaves behind for fulfilled/unknown_outcome/
// declined/cancelled rows.
func parkDocumentDelivery(t *testing.T, jobs *job.Store, svc *app.Service, reqID string, w work.Work, provider, state, providerRef string) string {
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
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "document_delivery_reconciliation"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, job.ActionKindDocumentDelivery, "document delivery reconciliation", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Delivery.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: provider, RequestClass: "digital_journal_article",
		WorkIdentity: w.DOI, State: delivery.State(state), ProviderReference: providerRef,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestTriageSnapshotV3AttentionMapping pins dev/post-build-followups.md
// item 7's settled attention mapping and the document_delivery rendering
// it drives, and proves schema 2's emission stays byte-identical alongside
// it (AGENTS.md: an optional field added to an existing message is only
// backward compatible for a new parser reading an old frame).
func TestTriageSnapshotV3AttentionMapping(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	b.svc.Delivery = delivery.New(jobs.S, &cfg, nil)

	unknownAuthID := park(t, jobs, "wr_v3_unknown_auth", handoffWork())

	knownAuthID, err := jobs.CreateRequest(ctx, "wr_v3_known_auth",
		work.Work{DOI: "10.1002/example.43", Title: "Paywalled Paper", Authors: []string{"Turing, Alan"}, Year: 2023}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, knownAuthID, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, knownAuthID, handoffActionKind, "sign in", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}

	unresolvedID := parkDocumentDelivery(t, jobs, b.svc, "wr_v3_delivery_unkn",
		work.Work{DOI: "10.1002/example.44", Title: "ILL Request", Year: 2022}, "illiad", "unknown_outcome", "TN-1")
	fulfilledID := parkDocumentDelivery(t, jobs, b.svc, "wr_v3_delivery_full",
		work.Work{DOI: "10.1002/example.45", Title: "Fulfilled ILL", Year: 2021}, "illiad", "fulfilled", "TN-2")

	downloadsAccessID := park(t, jobs, "wr_v3_downloads_access", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, downloadsAccessID, job.ActionKindDownloadsAccessRequired,
		cfg.EffectiveAdoptionRoot(), job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())

	v3msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-v3-attention", SchemaVersions: []int64{3}, Limit: 50}))
	response := firstOfType(v3msgs, protocol.MsgTriageSnapshotResponse)
	if response == nil {
		t.Fatalf("v3 snapshot response missing: %v", v3msgs)
	}
	payload := response.Payload.(*protocol.TriageSnapshotResponsePayload)
	byJob := make(map[string]protocol.TriageSnapshotItem, len(payload.Items))
	for _, item := range payload.Items {
		byJob[item.JobID] = item
	}
	for _, tc := range []struct {
		jobID, want string
	}{
		{unknownAuthID, "working"},      // unknown-auth openurl handoff proceeds on its own
		{knownAuthID, "required"},       // known login/MFA boundary
		{unresolvedID, "required"},      // delivery unknown_outcome
		{fulfilledID, "working"},        // fulfilled delivery being autonomously retrieved
		{downloadsAccessID, "required"}, // TCC-blocked adoption root needs a human grant
	} {
		item, ok := byJob[tc.jobID]
		if !ok {
			t.Fatalf("no v3 snapshot item for job %s", tc.jobID)
		}
		if item.Attention != tc.want {
			t.Fatalf("job %s attention = %q, want %q (action_kind=%q auth_requirement=%q)",
				tc.jobID, item.Attention, tc.want, item.ActionKind, item.AuthRequirement)
		}
	}
	fulfilled := byJob[fulfilledID]
	if fulfilled.Delivery == nil || fulfilled.Delivery.Provider != "illiad" ||
		fulfilled.Delivery.State != "fulfilled" || fulfilled.Delivery.ProviderReference != "TN-2" {
		t.Fatalf("fulfilled delivery item = %+v, want a populated delivery sub-object", fulfilled)
	}
	if !slices.Equal(fulfilled.Ops, []string{"open_request_history", "confirm_request_exists", "confirm_request_absent"}) {
		t.Fatalf("document_delivery ops = %v, want the three reconciliation ops", fulfilled.Ops)
	}
	if fulfilled.RouteClass != "document_delivery" {
		t.Fatalf("route_class = %q, want document_delivery", fulfilled.RouteClass)
	}
	downloadsAccess := byJob[downloadsAccessID]
	if downloadsAccess.RouteClass != job.ActionKindDownloadsAccessRequired {
		t.Fatalf("route_class = %q, want %s", downloadsAccess.RouteClass, job.ActionKindDownloadsAccessRequired)
	}
	if downloadsAccess.Facts == nil {
		t.Fatalf("downloads_access_required item carries no facts (expected the adoption root detail)")
	}
	var sawRootDetail bool
	for _, fact := range downloadsAccess.Facts {
		if fact.Label == "Detail" && fact.Text == cfg.EffectiveAdoptionRoot() {
			sawRootDetail = true
		}
	}
	if !sawRootDetail {
		t.Fatalf("downloads_access_required facts = %+v, want a Detail fact carrying the adoption root %q",
			downloadsAccess.Facts, cfg.EffectiveAdoptionRoot())
	}

	// Schema 2 stays byte-identical: no triage-snapshot/3 field ever appears
	// on its wire, even though the same daemon just emitted them for v3.
	v2msgs, v2Raw := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-v2-unchanged", SchemaVersions: []int64{2}, Limit: 50}))
	if firstOfType(v2msgs, protocol.MsgTriageSnapshotResponse) == nil {
		t.Fatalf("v2 snapshot response missing: %v", v2msgs)
	}
	assertNoV3FieldsOnSchema2Snapshot(t, v2Raw)
}

func assertNoV3FieldsOnSchema2Snapshot(t *testing.T, frames []json.RawMessage) {
	t.Helper()
	forbidden := []string{"attention", "route_class", "auth_requirement", "delivery"}
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
		if envelope.Payload.Schema != 2 {
			t.Fatalf("schema-2 request received schema %d, want 2", envelope.Payload.Schema)
		}
		for _, item := range envelope.Payload.Items {
			for _, field := range forbidden {
				if _, present := item[field]; present {
					t.Fatalf("schema-2 snapshot item carried a triage-snapshot/3 field %q: %+v", field, item)
				}
			}
		}
		return
	}
	t.Fatal("no schema-2 snapshot response received")
}

// TestTriageSnapshotV3DeliveryLookupFailureDegradesGracefully pins the
// reviewPreview-class footgun (AGENTS.md): a non-nil error from a
// browser-bridge RPC handler kills the whole native-messaging session, not
// just the request, so a routine delivery-store failure must degrade the
// one item to an absent Delivery rather than aborting the snapshot.
func TestTriageSnapshotV3DeliveryLookupFailureDegradesGracefully(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	b.svc.Delivery = delivery.New(jobs.S, &cfg, nil)

	jobID := parkDocumentDelivery(t, jobs, b.svc, "wr_v3_delivery_lookup_fail",
		work.Work{DOI: "10.1002/example.46", Title: "Lookup Failure ILL", Year: 2020}, "illiad", "unknown_outcome", "TN-3")

	// Break only the delivery lookup: human_actions/jobs/work_requests (what
	// the triage query itself reads) are untouched, so a real, isolated
	// storage failure surfaces exactly where triageDeliveryFor's error path
	// is exercised, not as an incidental snapshot-wide failure.
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TABLE delivery_requests`); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-v3-delivery-fail", SchemaVersions: []int64{3}, Limit: 50}))
	response := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	if response == nil {
		t.Fatalf("v3 snapshot response missing despite the delivery lookup failing: %v", msgs)
	}
	payload := response.Payload.(*protocol.TriageSnapshotResponsePayload)
	var found *protocol.TriageSnapshotItem
	for i := range payload.Items {
		if payload.Items[i].JobID == jobID {
			found = &payload.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("document_delivery item missing from snapshot despite the lookup failure being non-fatal: %+v", payload.Items)
	}
	if found.Delivery != nil {
		t.Fatalf("delivery = %+v, want nil after a lookup failure", found.Delivery)
	}
	if found.Attention != "required" {
		t.Fatalf("attention = %q, want required (a nil delivery never reads as fulfilled)", found.Attention)
	}

	// The failed optional lookup must not poison the native session: a later
	// request on the same session still gets its own valid schema-v3 response.
	later, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-v3-after-delivery-fail", SchemaVersions: []int64{3}, Limit: 50}))
	laterResponse := firstOfType(later, protocol.MsgTriageSnapshotResponse)
	if laterResponse == nil {
		t.Fatalf("later schema-v3 snapshot response missing after optional delivery failure: %v", later)
	}
	laterPayload := laterResponse.Payload.(*protocol.TriageSnapshotResponsePayload)
	if laterPayload.RequestID != "request-v3-after-delivery-fail" || laterPayload.Schema != 3 {
		t.Fatalf("later snapshot payload = %+v, want request_id/schema v3", laterPayload)
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
	brokenDB, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	jobs := &job.Store{S: brokenDB}
	b.triage = triage.New(brokenDB, watch.NewStore(brokenDB), jobs)
	if err := brokenDB.Close(); err != nil {
		t.Fatal(err)
	}
}

// A raw Go error from triageCounts or stats would propagate through Sync into
// the native host's fatal error path (internal/nativehost/host.go), tearing
// down the whole native-messaging session over a routine, recoverable failure.
// The bridge must answer with a protocol error frame instead, so the
// extension keeps polling.
func TestReadModelFailureReportsErrorFrameNotFatal(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(t *testing.T, b *Bridge)
		requestType   string
		payload       any
		forbiddenType string
		requestLabel  string
		failureLabel  string
	}{
		{
			name:          "triage_counts_unconfigured",
			setup:         func(t *testing.T, b *Bridge) { b.triage = nil },
			requestType:   protocol.MsgTriageCountsRequest,
			payload:       protocol.TriageCountsRequestPayload{RequestID: "request-count-002"},
			forbiddenType: protocol.MsgTriageCountsResponse,
			requestLabel:  "an unconfigured triage_counts_request",
			failureLabel:  "unconfigured triage service",
		},
		{
			name:          "triage_counts_failing_query",
			setup:         breakTriage,
			requestType:   protocol.MsgTriageCountsRequest,
			payload:       protocol.TriageCountsRequestPayload{RequestID: "request-count-003"},
			forbiddenType: protocol.MsgTriageCountsResponse,
			requestLabel:  "a failing triage_counts_request",
			failureLabel:  "a failing triage counts query",
		},
		{
			name:          "stats_unconfigured",
			setup:         func(t *testing.T, b *Bridge) { b.triage = nil },
			requestType:   protocol.MsgStatsRequest,
			payload:       protocol.StatsRequestPayload{RequestID: "request-stats-002"},
			forbiddenType: protocol.MsgStatsResponse,
			requestLabel:  "an unconfigured stats_request",
			failureLabel:  "unconfigured stats service",
		},
		{
			name:          "stats_failing_query",
			setup:         breakTriage,
			requestType:   protocol.MsgStatsRequest,
			payload:       protocol.StatsRequestPayload{RequestID: "request-stats-003"},
			forbiddenType: protocol.MsgStatsResponse,
			requestLabel:  "a failing stats_request",
			failureLabel:  "a failing stats query",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, _, _, _ := newBridge(t)
			test.setup(t, b)
			msgs, _ := runSync(t, b, hello(), inFrame(t, test.requestType, "", test.payload))
			if firstOfType(msgs, protocol.MsgError) == nil {
				t.Fatalf("no error frame for %s: %v", test.failureLabel, msgs)
			}
			if countType(msgs, test.forbiddenType) != 0 {
				t.Fatalf("%s emitted despite %s: %v", test.forbiddenType, test.failureLabel, msgs)
			}
			if poll, _ := runSync(t, b); firstOfType(poll, protocol.MsgError) != nil {
				t.Fatalf("session did not survive %s: %v", test.requestLabel, poll)
			}
		})
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

func TestTriageDecideTwoWatchConflictLeavesFirstIntact(t *testing.T) {
	b, _, _, _ := newBridge(t)
	ctx := context.Background()
	w1, err := b.watchRunner.Store.Create(ctx, watch.CreateInput{
		Query: "w1", Filters: watch.Filters{YearFrom: 2020},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := b.watchRunner.Store.Create(ctx, watch.CreateInput{
		Query: "w2", Filters: watch.Filters{YearFrom: 2020},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := watch.DigestEntry{
		WorkKey: "10.1000/shared", DOI: "10.1000/shared", Title: "Shared work",
	}
	if _, err := b.watchRunner.Store.RecordDigest(ctx, w1.ID, b.now(), []watch.DigestEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.watchRunner.Store.RecordDigest(ctx, w2.ID, b.now(), []watch.DigestEntry{entry}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: 10})
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].WatchHit == nil {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	hit := snapshot.Items[0].WatchHit
	if len(hit.Watches) != 2 {
		t.Fatalf("grouped watches = %+v, want 2", hit.Watches)
	}
	// Pre-consume the second watch's entry so the batch conflicts on the second target.
	if _, err := b.watchRunner.Store.ConsumeDigest(ctx, w2.ID, []string{entry.WorkKey}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, hello(), inFrame(t, protocol.MsgTriageDecide, "",
		protocol.TriageDecidePayload{
			RequestID: "request-conflict-001", ItemID: snapshot.Items[0].ID, Op: "dismiss",
			WatchScope: json.RawMessage(`"all"`),
		}))
	result := firstOfType(msgs, protocol.MsgTriageDecideResult)
	if result == nil {
		t.Fatalf("triage decision response missing: %v", msgs)
	}
	payload := result.Payload.(*protocol.TriageDecideResultPayload)
	if payload.RequestID != "request-conflict-001" || payload.Outcome != "conflict" {
		t.Fatalf("triage decision payload = %+v, want conflict", payload)
	}
	// First watch's entry must remain unconsumed: atomic batch left neither consumed.
	entries, err := b.watchRunner.Store.Digest(ctx, w1.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkKey != entry.WorkKey {
		t.Fatalf("w1 digest after conflict = %+v, want one unconsumed entry", entries)
	}
	// The response is a structured outcome, not a transport error — Sync succeeded.
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

func TestPageAcquireRejectedInputReturnsErrorWithoutSubmit(t *testing.T) {
	tests := []struct {
		name          string
		payload       protocol.PageAcquirePayload
		wantErrSubstr string
	}{
		{
			name: "invalid_doi",
			payload: protocol.PageAcquirePayload{
				URL: "https://publisher.example.edu/article/42",
				DOI: "not-a-doi",
			},
			wantErrSubstr: "invalid page DOI",
		},
		{
			name: "missing_doi",
			payload: protocol.PageAcquirePayload{
				URL:   "https://publisher.example.edu/article/42",
				Title: "A DOI-less page",
			},
			wantErrSubstr: "page has no DOI",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			runSync(t, b, hello())

			msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPageAcquire, "", test.payload))
			ack := firstOfType(msgs, protocol.MsgPageAcquireAck)
			if ack == nil {
				t.Fatalf("no page_acquire_ack in %v", msgs)
			}
			payload := ack.Payload.(*protocol.PageAcquireAckPayload)
			if payload.Error == "" || !strings.Contains(payload.Error, test.wantErrSubstr) || payload.JobID != "" || payload.Duplicate {
				t.Fatalf("page_acquire_ack = %#v", payload)
			}
			var count int
			if err := jobs.S.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM jobs").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("jobs after rejected page acquire = %d, want 0", count)
			}
		})
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
	restarted := NewBridge(jobs, active.svc, active.triage, active.watchRunner, active.preview, active.captureStore, active.holdings, active.zotio, cfg, active.Version)
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
	// Ordinary offers carry only the route host and reviewed evidence for this
	// job; they must not expose the complete provider registry.
	if !slices.Contains(p.ProviderHosts, "openurl.example.edu") {
		t.Fatalf("provider_hosts = %v, missing resolver host", p.ProviderHosts)
	}
	if slices.Contains(p.ProviderHosts, "springer.com") || slices.Contains(p.ProviderHosts, "jstor.org") {
		t.Fatalf("provider_hosts = %v, contains unrelated registry hosts", p.ProviderHosts)
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
			// A focus still owed counts as queued: the request IS pending. It used
			// to count 0, which the CLI renders as a refusal ("handoffs were not
			// opened"), so an operator retrying a stalled sign-in was told papio
			// would not act on a paper it was already holding a focus for. The
			// frame count below is what pins "emits once".
			if !sessionLive || queued != 1 {
				t.Fatalf("duplicate focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
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
	// The premise this test's name claims: the target lies beyond the page the
	// ordinary pass would offer. That pass takes oldest-first and stops at
	// maxOutstandingOffers, so a page's worth of OLDER papers puts a freshly
	// parked target out of reach — and the premise is now asserted below
	// instead of tolerated. It used to age the target 48h *older*, which put it
	// first in line, then accept either outcome; 200 filler papers bought
	// nothing (the assertions passed with three) while costing ~7s of the
	// package under -race.
	for i := range maxOutstandingOffers + 1 {
		id := park(t, jobs, job.NewID("wr_focus_poll_page"), handoffWork())
		if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			time.Now().UTC().Add(-time.Duration(48+i)*time.Hour).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	target := park(t, jobs, "wr_focus_outside_poll_page", handoffWork())

	runSync(t, b, hello())
	if b.offered[target] {
		t.Fatal("the ordinary pass offered the target, so nothing here exercises a focus request reaching past the page")
	}
	queued, sessionLive, err := b.FocusHandoffs(ctx, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 1 {
		t.Fatalf("focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
	}
	// The ordinary pass filled every slot with the older papers, so free one:
	// a focus request is a priority claim on a slot, not a way to exceed the
	// transport budget.
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
	if !focused || !offered {
		t.Fatalf("focus frames offered:%t focused:%t, want both", offered, focused)
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

func materializationHello(t *testing.T) json.RawMessage {
	t.Helper()
	return inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.14.0",
		"features":          []string{institutionalMaterializationFeature, effectPermitFeature},
	})
}

func explicitMaterializationCandidate(t *testing.T, jobs *job.Store, jobID, domain string) string {
	t.Helper()
	ctx := context.Background()
	profiles, err := jobs.ListInstitutionProfiles(ctx, false)
	if err != nil || len(profiles) == 0 {
		t.Fatalf("list institution profiles: %v (%d)", err, len(profiles))
	}
	attempt, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatalf("materialization attempt: %v", err)
	}
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		JobID: jobID, JobAttemptRevision: attempt,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "pre-route-" + domain, SafetyDomainID: domain,
		AdapterRevision: "test-adapter", EffectContractID: "test-effect", Status: "eligible",
	})
	if err != nil {
		t.Fatalf("create explicit browser candidate: %v", err)
	}
	return candidate.ID
}
func schedulerDescriptor(t *testing.T, jobs *job.Store, candidateID, status string) job.BrowserCandidateDescriptor {
	t.Helper()
	var descriptor job.BrowserCandidateDescriptor
	if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision, route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id, adapter_revision, effect_contract_id, status, created_at FROM browser_candidates WHERE id=?`, candidateID).Scan(
		&descriptor.CandidateID, &descriptor.JobID, &descriptor.JobAttemptRevision, &descriptor.InstitutionProfileID, &descriptor.InstitutionProfileRevision, &descriptor.RouteRevision, &descriptor.RouteClass, &descriptor.IdentifierStrategy, &descriptor.PreRouteSafetyKey, &descriptor.SafetyDomainID, &descriptor.AdapterRevision, &descriptor.EffectContractID, &descriptor.Status, &descriptor.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	descriptor.Status = status
	return descriptor
}

func TestInstitutionalCandidateOfferReoffersUntilClaim(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-reoffer", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "domain-reoffer")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	first, _ := runSync(t, b)
	offer := firstOfType(first, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("first poll did not emit candidate offer: %v", first)
	}
	candidateID := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID
	offerPayload := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload)
	if len(offerPayload.ProviderHosts) == 0 || offerPayload.AccessMode == "" {
		t.Fatalf("candidate offer omitted job context: %#v", offerPayload)
	}
	if offerPayload.Expected == nil || offerPayload.Expected.DOI == "" {
		t.Fatalf("candidate offer omitted expected work identity: %#v", offerPayload)
	}
	rawOffer, err := json.Marshal(offerPayload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawOffer, []byte(`"openurl"`)) || bytes.Contains(rawOffer, []byte(`"url"`)) {
		t.Fatalf("candidate offer carried URL field: %s", rawOffer)
	}
	second, _ := runSync(t, b)
	reoffer := firstOfType(second, protocol.MsgInstitutionalCandidateOffer)
	if reoffer == nil || reoffer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("lost candidate offer was not re-emitted: first=%v second=%v", first, second)
	}
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "materialization-reoffer-claim", CandidateID: candidateID, MaterializationKind: "browser_tab",
		}))
	claimResult := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResult == nil {
		t.Fatalf("claim result missing: %v", claimed)
	}
	claimPayload := claimResult.Payload.(*protocol.InstitutionalClaimResponsePayload)
	if claimPayload.Outcome != "claimed" {
		t.Fatalf("claim result = %v, want claimed", claimed)
	}
	afterClaim, _ := runSync(t, b)
	recovered := firstOfType(afterClaim, protocol.MsgInstitutionalCandidateOffer)
	if recovered == nil || recovered.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("claimed candidate was not re-offered for recovery: %v", afterClaim)
	}
	bound, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalBindRequest, jobID,
		protocol.InstitutionalBindRequestPayload{
			RequestID: "materialization-reoffer-bind", ClaimID: claimPayload.ClaimID,
			BindingID: claimPayload.BindingID, TabID: 7,
		}))
	bindResult := firstOfType(bound, protocol.MsgInstitutionalBindResponse)
	if bindResult == nil || bindResult.Payload.(*protocol.InstitutionalBindResponsePayload).Outcome != "bound" {
		t.Fatalf("bind result = %v, want bound", bound)
	}
	if err := jobs.Cancel(ctx, jobID, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := runSync(t, b)
	if firstOfType(cancelled, protocol.MsgCancel) == nil {
		t.Fatalf("cancel after bind did not emit cancel: %v", cancelled)
	}
}

// TestFocusedOfferSurvivesACancelledSiblingOnTheSameDomain pins the operator's own
// request against delay by a corpse, through the whole chain: the real scheduler,
// the bridge's one-candidate-per-safety-domain admission, and the offer frame.
// Nothing retires a candidate row when its job is cancelled, so the dead paper
// kept taking turns at its domain's single admission slot and the focused live
// paper was not offered on the polls it lost. The rotation is fair, so this is
// delay rather than deadlock - measured live 2026-08-19 as seven minutes of
// silence after an explicit open on job_673c22adda606ce0959b4034df behind two
// cancelled siblings. An explicit request must be served on the next poll, which
// is what this asserts.
func TestFocusedOfferSurvivesACancelledSiblingOnTheSameDomain(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	dead := parkInstitutional(t, jobs, "materialization-starve-dead", handoffWork(), "")
	liveWork := handoffWork()
	liveWork.DOI = "10.1002/example.43"
	live := parkInstitutional(t, jobs, "materialization-starve-live", liveWork, "")
	runSync(t, b, materializationHello(t))
	// One safety domain, so only one of the two can ever be admitted per poll.
	explicitMaterializationCandidate(t, jobs, dead, "domain-starve")
	liveCandidate := explicitMaterializationCandidate(t, jobs, live, "domain-starve")
	if err := jobs.Cancel(ctx, dead, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatalf("cancel the sibling: %v", err)
	}

	if queued, sessionLive, err := b.FocusHandoffs(ctx, []string{live}); err != nil || !sessionLive || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, sessionLive, err)
	}
	msgs, _ := runSync(t, b)
	offer := firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("focused live paper was never offered behind a cancelled sibling: %v", msgs)
	}
	payload, ok := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload)
	if !ok {
		t.Fatalf("offer payload = %T, want *protocol.InstitutionalCandidateOfferPayload", offer.Payload)
	}
	if offer.JobID != live || payload.CandidateID != liveCandidate {
		t.Fatalf("offered job %q candidate %q, want %q/%q", offer.JobID, payload.CandidateID, live, liveCandidate)
	}
}

func TestInstitutionalCandidateOfferCancellationIsDelivered(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-cancel", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "domain-cancel")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	initial, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	if err := jobs.Cancel(ctx, jobID, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := runSync(t, b)
	if firstOfType(cancelled, protocol.MsgCancel) == nil {
		t.Fatalf("materialization-only cancellation did not emit cancel: %v", cancelled)
	}
}

func TestInstitutionalCandidateOfferRecoversAfterHolderRestart(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-restart-recovery", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "domain-restart")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	initial, _ := runSync(t, b)
	offer := firstOfType(initial, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	candidateID := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID

	const replacementSession = "sess-replacement-000000000000000000000"
	b.mu.Lock()
	b.promote(&browserSession{
		ID:               replacementSession,
		ExtensionVersion: "0.14.0",
		Features:         []string{institutionalMaterializationFeature, effectPermitFeature},
		LastSyncAt:       b.now(),
	}, "test holder restart")
	b.mu.Unlock()
	recovered, _ := runSyncAs(t, b, replacementSession)
	reoffer := firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer)
	if reoffer == nil || reoffer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("restart did not recover candidate offer: %v", recovered)
	}
}

func TestInstitutionalCandidateClaimOfferExpiryRetainsCancellationTracking(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-expiry-cancel", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "domain-expiry")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	initial, _ := runSync(t, b)
	offer := firstOfType(initial, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	candidateID := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "materialization-expiry-claim", CandidateID: candidateID, MaterializationKind: "browser_tab",
		}))
	claimResult := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResult == nil || claimResult.Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome != "claimed" {
		t.Fatalf("claim result = %v, want claimed", claimed)
	}
	future := b.now().Add(b.actionExpiry() + time.Second)
	b.now = func() time.Time { return future }
	expired, _ := runSync(t, b)
	recovered := firstOfType(expired, protocol.MsgInstitutionalCandidateOffer)
	if recovered == nil || recovered.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("expired claim did not converge to a fresh candidate offer: %v", expired)
	}
	if !b.materializationTracked[jobID] {
		t.Fatal("candidate cancellation tracking was cleared during claim expiry reconciliation")
	}
	if err := jobs.Cancel(ctx, jobID, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := runSync(t, b)
	if firstOfType(cancelled, protocol.MsgCancel) == nil {
		t.Fatalf("cancel after claimed offer expiry did not emit cancel: %v", cancelled)
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

// TestDocumentDeliveryRetrievalHandoffOffersFormPDFURL proves the
// 2026-08-07 ADR-0017 amendment's browser-side wiring: an openurl_handoff
// action carrying app.DocumentDeliveryRetrievalHandoffDetail offers the
// embedded form-75 "View PDF" URL — not the institution's ordinary
// resolver route — with its host on the offer's provider host list.
func TestDocumentDeliveryRetrievalHandoffOffersFormPDFURL(t *testing.T) {
	const retrievalURL = "https://illiadweb.example.edu/illiad/illiad.dll?Action=10&Form=75&Value=482910"
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, "wr_document_delivery_retrieval", handoffWork(), "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "document_delivery_retrieval"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind,
		app.DocumentDeliveryRetrievalHandoffDetail+"\n"+retrievalURL, job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	msgs, _ := runSync(t, b, hello())
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatal("missing document-delivery retrieval offer")
	}
	payload := offer.Payload.(*protocol.JobOfferPayload)
	if payload.OpenURL != retrievalURL {
		t.Fatalf("offer URL = %q, want the form-75 retrieval URL %q", payload.OpenURL, retrievalURL)
	}
	if !slices.Contains(payload.ProviderHosts, "illiadweb.example.edu") {
		t.Fatalf("offer hosts = %v, missing the patron-web host", payload.ProviderHosts)
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
	b.reofferSourceJobID = map[string]string{}
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

// The operator's own institution is configured twice - top level as the
// default, and named so a job can request it - so both entries carry one
// openurl_base_url and that origin serves two profiles. Resolving the hint to
// a single profile treated it as ambiguous and dropped the frame: no evidence
// row, no release. Live 2026-08-20, two session_evidence frames arrived the
// moment a real library sign-in completed and nothing moved, while 132 papers
// waited on that institution.
func TestSessionEvidenceSharedOriginReleasesEveryProfileItServes(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.OpenURLBase = "https://shared.example.edu/openurl"
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"named": {OpenURLBase: "https://shared.example.edu/openurl"},
		"other": {OpenURLBase: "https://other.example.edu/openurl"},
	}
	sourceDefault := parkInstitutional(t, jobs, "wr_shared_default_source", handoffWork(), "")
	siblingDefault := parkInstitutional(t, jobs, "wr_shared_default_sibling", handoffWork(), "")
	sourceNamed := parkInstitutional(t, jobs, "wr_shared_named_source", handoffWork(), "named")
	siblingNamed := parkInstitutional(t, jobs, "wr_shared_named_sibling", handoffWork(), "named")
	sourceOther := parkInstitutional(t, jobs, "wr_shared_other_source", handoffWork(), "other")
	siblingOther := parkInstitutional(t, jobs, "wr_shared_other_sibling", handoffWork(), "other")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{sourceDefault: true, sourceNamed: true, sourceOther: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence":    "warm_verified",
		"origin_hint": "https://shared.example.edu",
		"at":          "2026-08-03T12:00:00Z",
	}))
	reoffered := func(jobID string) bool {
		t.Helper()
		events, err := jobs.Events(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				return true
			}
		}
		return false
	}
	if !reoffered(siblingDefault) {
		t.Fatal("the default profile was not released by its own origin's sign-in")
	}
	if !reoffered(siblingNamed) {
		t.Fatal("the named profile sharing that origin was not released")
	}
	if reoffered(siblingOther) {
		t.Fatal("a profile on a different origin was released")
	}
}

// The claim is the boundary, not the origin. Two federated entities behind one
// resolver origin are two human sign-in entries, so one entry's session proves
// nothing about the other and the frame must stay unattributable - the
// corrected cardinality rule's other half ("resolving a claim never
// auto-asserts entitled session evidence for every profile grouped under it").
func TestSessionEvidenceSharedOriginAcrossTwoClaimsStaysFailClosed(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.OpenURLBase = "https://shared.example.edu/openurl"
	b.cfg.Browser.ShibbolethEntityID = "https://idp-a.example.edu/entity"
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"tenant": {
			OpenURLBase:        "https://shared.example.edu/openurl",
			ShibbolethEntityID: "https://idp-b.example.edu/entity",
		},
	}
	sourceDefault := parkInstitutional(t, jobs, "wr_two_claim_source", handoffWork(), "")
	siblingDefault := parkInstitutional(t, jobs, "wr_two_claim_sibling", handoffWork(), "")
	sourceTenant := parkInstitutional(t, jobs, "wr_two_claim_tenant_source", handoffWork(), "tenant")
	siblingTenant := parkInstitutional(t, jobs, "wr_two_claim_tenant_sibling", handoffWork(), "tenant")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{sourceDefault: true, sourceTenant: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence":    "warm_verified",
		"origin_hint": "https://shared.example.edu",
		"at":          "2026-08-03T12:00:00Z",
	}))
	for _, jobID := range []string{siblingDefault, siblingTenant} {
		events, err := jobs.Events(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				t.Fatalf("two human entries behind one origin released %s", jobID)
			}
		}
	}
}

// One library named twice is one sign-in entry. The claim id used to key on
// the config name whenever a profile declared no entity of its own, so naming
// your default institution minted a second claim for it: two sign-in slots
// where the cardinality invariant promises one.
func TestAuthenticationClaimGroupsProfilesSharingOneHumanEntry(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	b.cfg.Browser.OpenURLBase = "https://library.example.edu/openurl"
	b.cfg.Browser.ShibbolethEntityID = "https://idp.example.edu/entity"
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		// The operator's own shape: the same resolver, named, with no entity
		// repeated.
		"named": {OpenURLBase: "https://library.example.edu/openurl"},
		// A different library, no federated entity at all: its own origin is
		// its entry, never the config name.
		"elsewhere": {OpenURLBase: "https://other.example.edu/openurl"},
	}
	runSync(t, b, hello())

	claimOf := func(name string) string {
		t.Helper()
		profile, err := jobs.InstitutionProfileByConfiguredName(ctx, name)
		if err != nil || profile == nil {
			t.Fatalf("profile %s: %+v %v", name, profile, err)
		}
		return profile.AuthenticationClaimID
	}
	if claimOf("default") != claimOf("named") {
		t.Fatal("one library configured twice holds two sign-in slots")
	}
	if claimOf("elsewhere") == claimOf("default") {
		t.Fatal("two libraries were merged into one sign-in slot")
	}
}

func TestSessionEvidenceProfilesDrainIndependentlyInOneSync(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	sourceAlpha := parkInstitutional(t, jobs, "wr_batch_alpha_source", handoffWork(), "alpha")
	siblingAlpha := parkInstitutional(t, jobs, "wr_batch_alpha_sibling", handoffWork(), "alpha")
	sourceBeta := parkInstitutional(t, jobs, "wr_batch_beta_source", handoffWork(), "beta")
	siblingBeta := parkInstitutional(t, jobs, "wr_batch_beta_sibling", handoffWork(), "beta")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{sourceAlpha: true, sourceBeta: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	runSync(t, b,
		inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
			"evidence": "warm_verified", "origin_hint": "https://alpha.example.edu",
			"at": "2026-08-03T12:00:00Z",
		}),
		inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
			"evidence": "warm_verified", "origin_hint": "https://beta.example.edu",
			"at": "2026-08-03T12:00:01Z",
		}),
	)
	if !reoffered(t, jobs, siblingAlpha) {
		t.Fatalf("alpha sibling was not released by alpha evidence")
	}
	if !reoffered(t, jobs, siblingBeta) {
		t.Fatalf("beta sibling was not released by beta evidence in the same sync")
	}
}

func TestSessionEvidenceStoreFailureRemainsRetryable(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	source := parkInstitutional(t, jobs, "wr_retry_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_retry_sibling", handoffWork(), "")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{source: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.sessionEvidence(ctx, &protocol.SessionEvidencePayload{
		Evidence: "warm_verified",
		At:       "2026-08-03T12:00:00Z",
	})
	b.mu.Unlock()
	if err == nil {
		t.Fatal("canceled evidence store unexpectedly succeeded")
	}
	if reoffered(t, jobs, sibling) {
		t.Fatal("failed evidence store released a sibling")
	}
	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence": "warm_verified",
		"at":       "2026-08-03T12:00:01Z",
	}))
	if !reoffered(t, jobs, sibling) {
		t.Fatal("evidence was not retryable after store failure")
	}
}

// An absent origin_hint cannot be attributed to any institution. Treating it
// as a wildcard let whichever named profile sorted first be released, so one
// institution's sign-in reopened another's parked tabs.
func TestSessionEvidenceWithoutOriginHintNeverReleasesANamedProfile(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	siblingAlpha := parkInstitutional(t, jobs, "wr_nohint_alpha", handoffWork(), "alpha")
	sourceBeta := parkInstitutional(t, jobs, "wr_nohint_beta_source", handoffWork(), "beta")
	siblingBeta := parkInstitutional(t, jobs, "wr_nohint_beta_sibling", handoffWork(), "beta")
	runSync(t, b, hello())
	// A live, already-offered source in a NAMED profile is what the fallback
	// scan latches onto. With the hint present this releases beta's queue;
	// without one the frame belongs to no institution and must release none.
	b.mu.Lock()
	b.offered = map[string]bool{sourceBeta: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	// The ordinary sync offer path also emits offers for parked handoffs, so
	// the reoffer release is identified by its own event, not by an offer
	// count. The sibling-scoping test above reads the same signal.
	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{"evidence": "warm_verified", "at": "2026-08-03T12:00:00Z"}))
	for _, jobID := range []string{siblingAlpha, siblingBeta} {
		events, err := jobs.Events(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				t.Fatalf("named-profile job %s released by unhinted evidence: %#v", jobID, event)
			}
		}
	}
}

func TestSessionEvidenceUnknownOriginDoesNotReleaseProfiles(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	source := parkInstitutional(t, jobs, "wr_unknown_origin_source", handoffWork(), "alpha")
	sibling := parkInstitutional(t, jobs, "wr_unknown_origin_sibling", handoffWork(), "alpha")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{source: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence":    "warm_verified",
		"origin_hint": "https://unknown.example.edu",
		"at":          "2026-08-03T12:00:00Z",
	}))
	if reoffered(t, jobs, sibling) {
		t.Fatalf("unknown origin released alpha profile sibling")
	}
}

// Scoping the unhinted frame must not disable it: the default profile is the
// entire queue for a single-institution setup, which is the common case.
func TestSessionEvidenceWithoutOriginHintStillReleasesTheDefaultProfile(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	source := parkInstitutional(t, jobs, "wr_nohint_default_source", handoffWork(), "")
	sibling := parkInstitutional(t, jobs, "wr_nohint_default_sibling", handoffWork(), "")
	siblingNamed := parkInstitutional(t, jobs, "wr_nohint_named_bystander", handoffWork(), "alpha")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{source: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.mu.Unlock()

	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{"evidence": "warm_verified", "at": "2026-08-03T12:00:00Z"}))
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != sibling {
		t.Fatalf("unhinted offer = %#v, want default-profile sibling %s", offer, sibling)
	}
	namedEvents, err := jobs.Events(context.Background(), siblingNamed)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range namedEvents {
		if event["kind"] == "browser.handoff_reoffered" {
			t.Fatalf("named bystander released by unhinted evidence: %#v", event)
		}
	}
}

// An origin-less keepalive used to demote a live named pin to the default
// profile. The next genuine auth return then saw a different pinned job and
// released nothing, silently starving the named institution's queue.
func TestSessionEvidenceWithoutOriginHintPreservesNamedProfilePin(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	sourceBeta := parkInstitutional(t, jobs, "wr_pin_beta_source", handoffWork(), "beta")
	sourceDefault := parkInstitutional(t, jobs, "wr_pin_default_source", handoffWork(), "")
	siblingDefault := parkInstitutional(t, jobs, "wr_pin_default_sibling", handoffWork(), "")
	runSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{sourceBeta: true, sourceDefault: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{"beta": sourceBeta}
	b.mu.Unlock()

	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "",
		map[string]any{"evidence": "warm_verified", "at": "2026-08-03T12:00:00Z"}))

	b.mu.Lock()
	pinnedJobID := b.reofferSourceJobID["beta"]
	b.mu.Unlock()
	if pinnedJobID != sourceBeta {
		t.Fatalf("pin after unhinted evidence = %q, want %q", pinnedJobID, sourceBeta)
	}
	for _, jobID := range []string{sourceDefault, siblingDefault} {
		events, err := jobs.Events(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				t.Fatalf("default-profile job %s released despite live beta pin: %#v", jobID, event)
			}
		}
	}
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

// assertDownloadValidationDoesNotBlockSessionSync runs one adoption path while
// validation is blocked and proves a concurrent nil-frame poll still completes.
func assertDownloadValidationDoesNotBlockSessionSync(
	t *testing.T,
	b *Bridge,
	adoptionNeverValidated string,
	pollDuringValidation string,
	syncBlockedOnValidation string,
	adoptionFailed string,
	adoptionDidNotFinish string,
	runAdoption func() error,
) {
	t.Helper()
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
		adoptionDone <- runAdoption()
	}()
	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal(adoptionNeverValidated)
	}

	pollDone := make(chan error, 1)
	go func() {
		_, err := b.Sync(context.Background(), testSessionID, false, nil)
		pollDone <- err
	}()
	select {
	case err := <-pollDone:
		if err != nil {
			t.Fatalf("%s: %v", pollDuringValidation, err)
		}
	case <-time.After(time.Second):
		t.Fatal(syncBlockedOnValidation)
	}

	close(releaseValidation)
	validationReleased = true
	select {
	case err := <-adoptionDone:
		if err != nil {
			t.Fatalf("%s: %v", adoptionFailed, err)
		}
	case <-time.After(time.Second):
		t.Fatal(adoptionDidNotFinish)
	}
}

func TestDownloadValidationDoesNotBlockSessionSync(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_unblocked_sync", handoffWork())
	runSync(t, b, hello())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	frame := inFrame(t, protocol.MsgDownloadComplete, id,
		map[string]any{"download_id": 7, "filename": "paper.pdf", "size_bytes": 533})
	assertDownloadValidationDoesNotBlockSessionSync(t, b,
		"download adoption never reached validation",
		"poll during validation",
		"session sync blocked on download validation",
		"download adoption",
		"download adoption did not finish",
		func() error {
			_, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{frame})
			return err
		},
	)
}

func TestPollDiscoveredDownloadValidationDoesNotBlockSessionSync(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_poll_unblocked_sync", handoffWork())
	runSync(t, b, hello())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	assertDownloadValidationDoesNotBlockSessionSync(t, b,
		"poll-time adoption never reached validation",
		"poll during poll-time validation",
		"session sync blocked on poll-time download validation",
		"poll-time adoption",
		"poll-time adoption did not finish",
		func() error {
			_, err := b.Sync(context.Background(), testSessionID, false, nil)
			return err
		},
	)
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

// openDownloadsAccessActions filters open human actions to the
// downloads_access_required ones for one job, for the assertions below.
func openDownloadsAccessActions(t *testing.T, jobs *job.Store, jobID string) []job.HumanAction {
	t.Helper()
	open, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var matches []job.HumanAction
	for _, a := range open {
		if a.JobID == jobID && a.Kind == job.ActionKindDownloadsAccessRequired {
			matches = append(matches, a)
		}
	}
	return matches
}

// TestAdoptionDeferredWithHealthyLatchOpensNoAction is the negative case: an
// ordinary transient defer (here, download_complete racing a file that has
// not landed yet) with the adoption-scan latch healthy must never open a
// downloads_access_required action — that grant would not fix the actual
// problem.
func TestAdoptionDeferredWithHealthyLatchOpensNoAction(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	target := park(t, jobs, "wr_tcc_unlatched", handoffWork())
	runSync(t, b, hello())

	if b.adoptionLatchUnhealthy() {
		t.Fatal("latch must start healthy")
	}
	frame := inFrame(t, protocol.MsgDownloadComplete, target,
		map[string]any{"download_id": 1, "filename": "paper.pdf", "size_bytes": 533})
	msgs, _ := runSync(t, b, frame)
	if firstOfType(msgs, protocol.MsgAck) == nil {
		t.Fatalf("expected ack after deferred adoption: %v", msgs)
	}
	if matches := openDownloadsAccessActions(t, jobs, target); len(matches) != 0 {
		t.Fatalf("unlatched defer opened downloads_access_required actions: %+v", matches)
	}
}

// TestAdoptionDeferredWithUnhealthyLatchOpensExactlyOneAction is the
// positive case: while the adoption-scan latch is unhealthy, a deferred
// adoption opens a downloads_access_required action naming the adoption
// root, and repeated deferrals across polls (two separate download_complete
// events here) never open a second one — OpenHumanAction's own dedupe
// covers it.
func TestAdoptionDeferredWithUnhealthyLatchOpensExactlyOneAction(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	target := park(t, jobs, "wr_tcc_latched", handoffWork())
	runSync(t, b, hello())

	b.adoptionScanMu.Lock()
	b.adoptionScanSuspended = true
	b.adoptionScanMu.Unlock()

	for _, downloadID := range []int{1, 2} {
		frame := inFrame(t, protocol.MsgDownloadComplete, target,
			map[string]any{"download_id": downloadID, "filename": "paper.pdf", "size_bytes": 533})
		msgs, _ := runSync(t, b, frame)
		if firstOfType(msgs, protocol.MsgAck) == nil {
			t.Fatalf("download %d: expected ack after deferred adoption: %v", downloadID, msgs)
		}
	}

	matches := openDownloadsAccessActions(t, jobs, target)
	if len(matches) != 1 {
		t.Fatalf("open downloads_access_required actions = %d, want exactly 1: %+v", len(matches), matches)
	}
	if matches[0].Detail != cfg.EffectiveAdoptionRoot() {
		t.Fatalf("action detail = %q, want the adoption root %q", matches[0].Detail, cfg.EffectiveAdoptionRoot())
	}
}

// TestSweepAdoptionResolvesDownloadsAccessActionAfterLatchRecovers is
// acceptance point 2: the action opened mid-flight, while the latch was
// unhealthy, must close once the grant lands and a directory sweep — not
// another download_complete — adopts the file.
func TestSweepAdoptionResolvesDownloadsAccessActionAfterLatchRecovers(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	target := park(t, jobs, "wr_tcc_sweep_resolve", handoffWork())
	runSync(t, b, hello())

	b.adoptionScanMu.Lock()
	b.adoptionScanSuspended = true
	b.adoptionScanMu.Unlock()
	runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, target,
		map[string]any{"download_id": 1, "filename": "paper.pdf", "size_bytes": 533}))
	if matches := openDownloadsAccessActions(t, jobs, target); len(matches) != 1 {
		t.Fatalf("latched defer did not open exactly one action: %+v", matches)
	}

	// The user grants access; the latch clears and the file lands. A sweep
	// tick adopts it and must close the action left open mid-flight.
	b.adoptionScanMu.Lock()
	b.adoptionScanSuspended = false
	b.adoptionScanMu.Unlock()
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), target, "paper.pdf"))
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}

	row, err := jobs.Get(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("sweep did not adopt after latch recovery: %+v", row)
	}
	if matches := openDownloadsAccessActions(t, jobs, target); len(matches) != 0 {
		t.Fatalf("downloads_access_required action still open after adoption succeeded: %+v", matches)
	}
	all, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var resolved bool
	for _, a := range all {
		if a.JobID == target && a.Kind == job.ActionKindDownloadsAccessRequired && a.Status == "resolved" {
			resolved = true
		}
	}
	if !resolved {
		t.Fatalf("downloads_access_required action was not resolved: %+v", all)
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

// TestPollSuspendsAdoptionScanningOnHungReadDirAndStaysResponsive reproduces
// the incident this latch fixes: a ReadDir behind a TCC-protected adoption
// root can block in-kernel forever. Every Sync/poll call must still return
// bounded by AdoptionScanDeadline, ordinary handoff offers must keep
// flowing, and — because Go can never cancel the blocked syscall — at most
// one goroutine may ever be stuck in it, no matter how many polls arrive
// while it is latched.
func TestPollSuspendsAdoptionScanningOnHungReadDirAndStaysResponsive(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	compressAdoptionScanDeadline(t)
	ctx := context.Background()

	var calls int32
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the permanently-leaked goroutine so the test binary can exit
	b.readDir = func(string) ([]os.DirEntry, error) {
		atomic.AddInt32(&calls, 1)
		<-block
		return nil, errors.New("unreachable in this test")
	}

	scanTarget := park(t, jobs, "wr_wedge_scan", handoffWork())
	offerJob := park(t, jobs, "wr_wedge_offer", handoffWork())

	boundedSync := func(frames []json.RawMessage) []*protocol.BrowserMessage {
		t.Helper()
		type outcome struct {
			raw []json.RawMessage
			err error
		}
		ch := make(chan outcome, 1)
		go func() {
			raw, err := b.Sync(ctx, testSessionID, false, frames)
			ch <- outcome{raw, err}
		}()
		select {
		case o := <-ch:
			if o.err != nil {
				t.Fatalf("sync: %v", o.err)
			}
			msgs := make([]*protocol.BrowserMessage, 0, len(o.raw))
			for _, m := range o.raw {
				decoded, err := protocol.DecodeBrowserMessage(m)
				if err != nil {
					t.Fatalf("outbound frame failed protocol decode: %v", err)
				}
				msgs = append(msgs, decoded)
			}
			return msgs
		case <-time.After(4 * time.Second):
			t.Fatal("Sync did not return: a hung adoption scan wedged the bridge")
			return nil
		}
	}

	start := time.Now()
	msgs := boundedSync([]json.RawMessage{hello()})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("first poll took %s, want bounded near the adoption scan deadline", elapsed)
	}
	if got := countType(msgs, protocol.MsgJobOffer); got != 2 {
		t.Fatalf("job offers while the adoption scan was suspended = %d, want 2 (offers must keep flowing)", got)
	}

	for i := range 3 {
		start := time.Now()
		boundedSync(nil)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("latched poll #%d took %s, want near-instant (no goroutine should be spawned while latched)", i, elapsed)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("readDir invoked %d times, want exactly 1: a latched scan must never spawn another goroutine", got)
	}

	for _, id := range []string{scanTarget, offerJob} {
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateAwaitingHuman {
			t.Fatalf("job %s left awaiting_human during a suspended scan: %+v", id, row)
		}
	}
}

// TestScanAdoptionDirEPERMIsNotAdoptableWithoutLatching asserts that a fast
// error (EPERM from an outright-denied ReadDir, for instance) is treated as
// routine not-adoptable and never trips the hung-call latch: only a call
// that actually misses the deadline may suspend scanning.
func TestScanAdoptionDirEPERMIsNotAdoptableWithoutLatching(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()

	var calls int32
	b.readDir = func(string) ([]os.DirEntry, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("adoptions: %w", os.ErrPermission)
	}

	id := park(t, jobs, "wr_eperm_scan", handoffWork())
	msgs, _ := runSync(t, b, hello())
	if countType(msgs, protocol.MsgJobOffer) != 1 {
		t.Fatalf("EPERM scan must not block the ordinary handoff offer: %v", msgs)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("EPERM must fail closed to not-adoptable: %+v", row)
	}
	b.adoptionScanMu.Lock()
	suspended := b.adoptionScanSuspended
	b.adoptionScanMu.Unlock()
	if suspended {
		t.Fatal("a fast EPERM error must not latch adoption scanning")
	}

	// A second poll performs a fresh scan rather than short-circuiting on a
	// latch that should never have been set.
	runSync(t, b)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("readDir called %d times across two polls, want 2 (no latch on a fast error)", got)
	}
}

// TestScanAdoptionResumesAfterHungReadDirReturnsAndAdoptsSettledFile drives
// the full incident lifecycle: a scan hangs and latches, a settled file
// arrives while scanning is suspended, the hung call eventually returns and
// clears the latch, and the next poll performs a fresh scan that adopts it.
func TestScanAdoptionResumesAfterHungReadDirReturnsAndAdoptsSettledFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	compressAdoptionScanDeadline(t)
	ctx := context.Background()

	var calls int32
	release := make(chan struct{})
	b.readDir = func(dir string) ([]os.DirEntry, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return os.ReadDir(dir)
	}

	id := park(t, jobs, "wr_resume_scan", handoffWork())
	runSync(t, b, hello())

	// The scan hangs past its deadline and latches; poll still returns.
	runSync(t, b)
	b.adoptionScanMu.Lock()
	suspended := b.adoptionScanSuspended
	b.adoptionScanMu.Unlock()
	if !suspended {
		t.Fatal("scan did not latch after missing its deadline")
	}

	// A settled file arrives while the scan is latched.
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	// Unblock the hung call: it re-reads the now-current directory and the
	// latch clears once it finally returns.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.adoptionScanMu.Lock()
		suspended = b.adoptionScanSuspended
		b.adoptionScanMu.Unlock()
		if !suspended {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if suspended {
		t.Fatal("scan latch never cleared after the hung call returned")
	}

	// The next poll performs a fresh scan and adopts the settled file.
	runSync(t, b)
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("settled file was not adopted after scan resumed: %+v", row)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("readDir called %d times, want 2 (one hung call, one fresh scan after resume)", got)
	}
}

func TestProviderOutcomeMappings(t *testing.T) {
	type expect struct {
		outcome      string // wire outcome; defaults to the case name
		adapterID    string // adapter_id on the frame; "" means none was sent
		state        string
		actionStatus string // status the openurl_handoff action should end in
		extraAction  string // additional open action kind expected
		extraDetail  string // detail expected on the additional open action
		diagnosis    string // durable human_actions.diagnosis expected
		terminal     string
	}
	cases := map[string]expect{
		"cancelled":                   {state: job.StateCancelled, actionStatus: "cancelled"},
		"no_entitlement":              {state: job.StateUnavailable, actionStatus: "resolved", terminal: "no_entitlement"},
		"document_delivery_available": {state: job.StateUnavailable, actionStatus: "resolved", terminal: "document_delivery_available"},
		"wrong_work": {
			state: job.StateAwaitingHuman, actionStatus: "resolved", extraAction: "manual_download",
			extraDetail: "papio reached a different work; find and download the requested PDF yourself",
			diagnosis:   job.DiagnosisReasonWrongWork,
		},
		// The two ui_changed reasons are told apart by adapter_id alone. An
		// adapter that reported the page means it stopped matching (drift);
		// no adapter means none ever claimed the page. The prose that used to
		// make this call is deliberately absent from both frames.
		"ui_changed_adapter_drift": {
			outcome: "ui_changed", adapterID: "sciencedirect",
			state: job.StateAwaitingHuman, actionStatus: "resolved", extraAction: "manual_download",
			extraDetail: "papio could not drive the provider page; download the PDF yourself and papio will adopt it",
			diagnosis:   job.DiagnosisReasonProviderAdapterDrift,
		},
		"ui_changed": {
			state: job.StateAwaitingHuman, actionStatus: "resolved", extraAction: "manual_download",
			extraDetail: "papio has no adapter for this provider yet; download the PDF yourself for now",
			diagnosis:   job.DiagnosisReasonProviderAdapterMissing,
		},
		"rate_limited":              {state: job.StateRetryWait, actionStatus: "resolved"},
		"human_auth_required":       {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "human_auth_required"},
		"terms_acceptance_required": {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "terms_acceptance_required"},
	}
	for name, want := range cases {
		outcome := want.outcome
		if outcome == "" {
			outcome = name
		}
		t.Run(name, func(t *testing.T) {
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
					id := park(t, jobs, "wr_"+name, handoffWork())
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
					frame := map[string]any{"outcome": outcome}
					if want.adapterID != "" {
						frame["adapter_id"] = want.adapterID
					}
					runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, frame))

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
					if want.diagnosis != "" {
						var diagnosis string
						if err := jobs.S.DB().QueryRowContext(ctx,
							`SELECT COALESCE(diagnosis, '') FROM human_actions WHERE id = ?`, extraOpen[0].ID).Scan(&diagnosis); err != nil {
							t.Fatal(err)
						}
						if diagnosis != want.diagnosis {
							t.Fatalf("durable diagnosis = %q, want %q", diagnosis, want.diagnosis)
						}
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
				"adapter_id":      "sage",
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
					detail["adapter_id"] != "sage" ||
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

func countJobOffersFor(msgs []*protocol.BrowserMessage, jobID string) int {
	n := 0
	for _, msg := range msgs {
		if msg.Type == protocol.MsgJobOffer && msg.JobID == jobID {
			n++
		}
	}
	return n
}

func latchEvents(t *testing.T, jobs *job.Store, id string) []map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, event := range events {
		if event["kind"] == providerLatchEventKind {
			out = append(out, event)
		}
	}
	return out
}

func TestProviderWrongWorkLatchBlocksAutomaticBrowserOffer(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_latch_wrong_work", handoffWork())
	runSync(t, b, helloWithAdapterVersions(t, "1.0.0", map[string]string{"sage": "1.0.0"}))
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{
		"outcome": "wrong_work", "adapter_id": "sage", "adapter_version": "1.0.0",
	}))
	latches := latchEvents(t, jobs, id)
	if len(latches) != 1 {
		t.Fatalf("latch events = %d, want 1", len(latches))
	}
	detail, _ := latches[0]["detail"].(map[string]any)
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	wantDomain := actionSafetyDomain(cfg, *row, actions[0])
	if detail["kind"] != "no_positive_effects" || detail["safety_domain"] != wantDomain {
		t.Fatalf("wrong-work latch = %#v, want domain %q", detail, wantDomain)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available again", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	delete(b.offered, id)
	msgs, _ := runSync(t, b)
	if got := countJobOffersFor(msgs, id); got != 0 {
		t.Fatalf("latched wrong-work job offers = %d, want 0", got)
	}
}

func TestFocusDoesNotBypassBrowserLatchButExplicitRetryStartsFreshEpoch(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := parkWithProviderEvidence(t, jobs, "wr_focus_latched", handoffWork(), "onlinelibrary.wiley.com")
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("open actions = %d, want 1", len(actions))
	}
	action := actions[0]
	if err := jobs.RecordEvent(ctx, id, providerLatchEventKind, map[string]any{
		"kind": "no_positive_effects", "safety_domain": actionSafetyDomain(cfg, *row, action),
	}); err != nil {
		t.Fatal(err)
	}

	// Establish the compatible holder with effect permit capability.
	effectPermitHolder(t, b)
	runSync(t, b, helloAs("0.14.0"))
	// re-assert holder after hello which may reset holder state
	effectPermitHolder(t, b)
	initial, err := b.offer(*row, action, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	decodeAttempt := func(raw json.RawMessage) string {
		msg, decodeErr := protocol.DecodeBrowserMessage(raw)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID
	}
	initialAttempt := decodeAttempt(initial)
	if initialAttempt == "" {
		t.Fatal("initial offer did not mint a drive epoch")
	}

	queued, sessionLive, err := b.FocusHandoffs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 1 {
		t.Fatalf("focus result = queued:%d live:%t, want 1,true", queued, sessionLive)
	}
	msgs, _ := runSync(t, b)
	if got := countJobOffersFor(msgs, id); got != 0 {
		t.Fatalf("focused latched job offers = %d, want 0", got)
	}
	for _, msg := range msgs {
		if msg.JobID == id && msg.Type == protocol.MsgProviderDirectGetRequest {
			t.Fatalf("focused latched job received provider direct request: %v", msgs)
		}
		if msg.JobID == id && msg.Type == protocol.MsgHandoffFocus {
			t.Fatalf("focused latched job received handoff_focus: %v", msgs)
		}
	}

	// The explicit retry path is the authority reset and must supersede the
	// prior epoch rather than reusing it.
	retried, err := b.offerAtURL(*row, action, config.ModeDelegated, "", true)
	if err != nil {
		t.Fatal(err)
	}
	retryAttempt := decodeAttempt(retried)
	if retryAttempt == "" || retryAttempt == initialAttempt {
		t.Fatalf("retry epoch = %q, initial epoch = %q; want a fresh epoch", retryAttempt, initialAttempt)
	}
}

func TestProviderDriftLatchAllowsNewerAdapterRevision(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_latch_drift", handoffWork())
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind,
		app.OABrowserHandoffActionDetail("https://sagepub.com/article"),
		job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "browser.page_capture", map[string]any{
		"host": "sagepub.com", "adapter_id": "sage", "adapter_version": "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, helloWithAdapterVersions(t, "1.0.0", map[string]string{"sage": "1.0.0"}))
	runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{
		"outcome": "ui_changed", "adapter_id": "sage", "adapter_version": "1.0.0",
	}))
	latches := latchEvents(t, jobs, id)
	if len(latches) != 1 {
		t.Fatalf("latch events = %d, want 1", len(latches))
	}
	detail, _ := latches[0]["detail"].(map[string]any)
	if detail["kind"] != "drift" || detail["safety_domain"] != "oa:sagepub.com" ||
		detail["adapter_id"] != "sage" || detail["adapter_version"] != "1.0.0" ||
		detail["host"] != "sagepub.com" {
		t.Fatalf("drift latch = %#v", detail)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind,
		app.OABrowserHandoffActionDetail("https://sagepub.com/article"),
		job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	delete(b.offered, id)
	msgs, _ := runSync(t, b)
	if got := countJobOffersFor(msgs, id); got != 0 {
		t.Fatalf("same-revision drifted job offers = %d, want 0", got)
	}
	b.holder.AdapterVersions["sage"] = "1.1.0"
	delete(b.offered, id)
	msgs, _ = runSync(t, b)
	if got := countJobOffersFor(msgs, id); got != 1 {
		t.Fatalf("newer-revision drifted job offers = %d, want 1", got)
	}
}

func TestProviderOutcomeLatchIsIdempotentAndSurvivesBridgeRestart(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_latch_restart", handoffWork())
	p := &protocol.ProviderOutcomePayload{
		Outcome: "ui_changed", AdapterID: "sage", AdapterVersion: "1.0.0",
	}
	if err := b.outcome(ctx, id, "outcome-replay", p); err != nil {
		t.Fatal(err)
	}
	if err := b.outcome(ctx, id, "outcome-replay", p); err != nil {
		t.Fatal(err)
	}
	if got := len(latchEvents(t, jobs, id)); got != 1 {
		t.Fatalf("duplicate outcome latch events = %d, want 1", got)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available again", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	b2 := NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
	runSync(t, b2, helloWithAdapterVersions(t, "1.0.0", map[string]string{"sage": "1.0.0"}))
	delete(b2.offered, id)
	msgs, _ := runSync(t, b2)
	if got := countJobOffersFor(msgs, id); got != 0 {
		t.Fatalf("restart re-offered latched job %d times, want 0", got)
	}
}

func TestProviderLatchDoesNotAffectUnrelatedJob(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	latchedID := park(t, jobs, "wr_latch_scoped", handoffWork())
	otherID := park(t, jobs, "wr_latch_other", work.Work{DOI: "10.1002/other.43", Title: "Another Paper"})
	if err := b.recordProviderLatch(ctx, latchedID, &protocol.ProviderOutcomePayload{
		Outcome: "wrong_work", AdapterID: "sage", AdapterVersion: "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, helloWithAdapterVersions(t, "1.0.0", map[string]string{"sage": "1.0.0"}))
	if got := countJobOffersFor(msgs, latchedID); got != 0 {
		t.Fatalf("latched job offers = %d, want 0", got)
	}
	if got := countJobOffersFor(msgs, otherID); got != 1 {
		t.Fatalf("unrelated job offers = %d, want 1", got)
	}
}

func TestBrowserOfferLatchUsesRouteDomainAndLandingEvidence(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_latch_domain_scope", handoffWork())
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("open actions = %d, want 1", len(actions))
	}
	action := actions[0]
	if err := jobs.RecordEvent(ctx, id, providerLatchEventKind, map[string]any{
		"kind": "no_positive_effects", "safety_domain": "route:sage-doi-pdf",
	}); err != nil {
		t.Fatal(err)
	}
	if latched, err := b.browserOfferLatched(ctx, *row, action, "route:wiley-doi-pdf", "onlinelibrary.wiley.com"); err != nil {
		t.Fatal(err)
	} else if latched {
		t.Fatal("route B was blocked by route A's latch")
	}
	if latched, err := b.browserOfferLatched(ctx, *row, action, "route:sage-doi-pdf", "journals.sagepub.com"); err != nil {
		t.Fatal(err)
	} else if !latched {
		t.Fatal("same-domain route was not blocked")
	}
	b2 := NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
	if latched, err := b2.browserOfferLatched(ctx, *row, action, "route:sage-doi-pdf", "journals.sagepub.com"); err != nil {
		t.Fatal(err)
	} else if !latched {
		t.Fatal("same-domain latch did not survive bridge restart")
	}

	driftID := park(t, jobs, "wr_latch_global_host", handoffWork())
	driftRow, err := jobs.Get(ctx, driftID)
	if err != nil {
		t.Fatal(err)
	}
	driftActions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{driftID})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, driftID, providerLatchEventKind, map[string]any{
		"kind":            "drift",
		"safety_domain":   actionSafetyDomain(cfg, *driftRow, driftActions[0]),
		"adapter_id":      "sage",
		"adapter_version": "1.0.0",
		"host":            "sagepub.com",
	}); err != nil {
		t.Fatal(err)
	}
	if latched, err := b.browserOfferLatched(ctx, *driftRow, driftActions[0]); err != nil {
		t.Fatal(err)
	} else if latched {
		t.Fatal("global verified provider host vetoed an unrelated institution route")
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
		"outcome": "ui_changed", "adapter_id": "sage", "adapter_version": "0.1.0",
	}))
	assertManualProviderPark(t, jobs, id)

	// The extension bundle is unchanged: the exact captured adapter version is
	// the evidence that invalidates this park.
	runSync(t, b, helloWithAdapterVersions(t, "0.7.0", map[string]string{"sage": "0.2.0"}))
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
			detail["new_extension_version"] != "0.7.0" ||
			detail["adapter_id"] != "sage" ||
			detail["old_adapter_version"] != "0.1.0" ||
			detail["new_adapter_version"] != "0.2.0" {
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
	runSync(t, b, helloWithAdapterVersions(t, "0.7.0", map[string]string{"sage": "0.2.0"}))
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

			if err := b.svc.HandoffRepairer().RepairAdapterUpgrade(
				context.Background(), tc.current, nil, extensionVersionNewer,
			); err != nil {
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
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
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

func TestOfferRoutesThroughLibKeyAndKeepsResolverHostVisible(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.Browser.LibKeyMode = "link"
	cfg.Browser.LibKeyLibraryID = 1234
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)

	raw, err := b.offer(job.Row{ID: "job-libkey", Work: handoffWork(), Policy: job.Policy{}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true}, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*protocol.JobOfferPayload)
	if payload.OpenURL != "https://libkey.io/libraries/1234/10.1002/example.42" {
		t.Fatalf("offer URL = %q, want the LibKey institution link", payload.OpenURL)
	}
	// The tab opens on libkey.io and forwards through the institution's
	// resolver; both hosts must ride the offer or the extension goes blind
	// exactly at the redirect.
	if len(payload.ProviderHosts) < 2 || payload.ProviderHosts[0] != "libkey.io" || payload.ProviderHosts[1] != "resolver.example.edu" {
		t.Fatalf("provider hosts = %v, want libkey.io then resolver.example.edu first", payload.ProviderHosts)
	}
}

func TestOfferWithoutLibKeyIdentifierFallsBackToOpenURL(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.Browser.LibKeyMode = "link"
	cfg.Browser.LibKeyLibraryID = 1234
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)

	// An ISBN-only book has no LibKey route; the offer must land on the
	// plain resolver, not dead-end (LibKey augments, never replaces).
	bookWork := work.Work{ISBN: "9780306406157", Title: "A Book"}
	raw, err := b.offer(job.Row{ID: "job-book", Work: bookWork, Policy: job.Policy{}}, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff", RequiresAuth: true}, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*protocol.JobOfferPayload)
	if !strings.HasPrefix(payload.OpenURL, "https://resolver.example.edu/openurl?") {
		t.Fatalf("offer URL = %q, want the OpenURL fallback", payload.OpenURL)
	}
	if payload.ProviderHosts[0] != "resolver.example.edu" {
		t.Fatalf("provider hosts = %v, want the resolver host first", payload.ProviderHosts)
	}
}

func TestOfferProviderHostsStayOnCurrentProviderRoute(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_provider_host_scope", handoffWork())
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("open actions = %d, want one", len(actions))
	}
	for _, host := range []string{"springer.com", "jstor.org", "sagepub.com"} {
		if err := jobs.RecordEvent(ctx, id, "browser.page_capture", map[string]any{"host": host}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := b.offerAtURL(*row, actions[0], config.ModeDelegated, "https://springer.com/article/10.1002/example.42", false)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*protocol.JobOfferPayload)
	if !slices.Equal(payload.ProviderHosts, []string{"springer.com"}) {
		t.Fatalf("provider hosts = %v, want only the current provider route", payload.ProviderHosts)
	}
}

func TestOfferProviderHostsRetainReviewedResolverEvidenceOnly(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_provider_host_evidence", handoffWork())
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "browser.page_capture", map[string]any{"host": "springer.com"}); err != nil {
		t.Fatal(err)
	}
	raw, err := b.offer(*row, actions[0], config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*protocol.JobOfferPayload)
	if !slices.Equal(payload.ProviderHosts, []string{"openurl.example.edu", "springer.com"}) {
		t.Fatalf("provider hosts = %v, want resolver plus reviewed provider evidence", payload.ProviderHosts)
	}
}

func TestOfferUnknownRouteDoesNotReceiveProviderRegistry(t *testing.T) {
	b, _, _, _ := newBridge(t)
	raw, err := b.offer(job.Row{ID: "unknown-offer", Work: handoffWork()}, job.HumanAction{
		Kind: handoffActionKind, Detail: "institutional handoff",
	}, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*protocol.JobOfferPayload)
	if !slices.Equal(payload.ProviderHosts, []string{"openurl.example.edu"}) {
		t.Fatalf("unknown offer hosts = %v, want only resolver host", payload.ProviderHosts)
	}
	for _, host := range verifiedProviderHosts {
		if slices.Contains(payload.ProviderHosts, host) {
			t.Fatalf("unknown offer received registry host %q: %v", host, payload.ProviderHosts)
		}
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
	b = NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)

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

// The sweep is the path that runs precisely when correlation was missed — a
// file that landed during daemon downtime, or after a lost download_complete —
// so it is the last place that may adopt institutional bytes unfenced. It used
// to call b.adopt directly, winning nothing.
func TestSweepAdoptionRequiresWinningTheAttempt(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_sweep_fenced", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-sweep", AuthenticationClaimID: "auth-sweep",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-sweep", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	claimSweep, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-sweep", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = claimSweep
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), jobID, "paper.pdf"))

	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	row, _ := jobs.Get(ctx, jobID)
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("sweeper did not adopt: %+v", row)
	}
	// The attempt has materialization history, so the swept bytes owed a winner
	// even though no claim was ever live.
	winner, ok, err := jobs.ArtifactWinner(ctx, jobID, 1)
	if err != nil || !ok {
		t.Fatalf("swept institutional bytes adopted with no artifact winner: ok=%v err=%v", ok, err)
	}
	if winner.SHA256 != row.ArtifactSHA256 {
		t.Fatalf("winner %q does not describe the adopted artifact %q", winner.SHA256, row.ArtifactSHA256)
	}
}

// The winner must describe bytes that PASSED validation. Committing the CAS
// first meant a rejected file (an HTML interstitial, a wrong-work PDF)
// permanently won the attempt, and the correct PDF that landed afterwards
// hashed differently and was refused as superseded, so the job could never
// complete. The ordering is pinned at the weigh/commit seam rather than by
// feeding a bad file through adoption, because the adoption service in this
// harness accepts any bytes; the defect is the ordering itself.
func TestWinnerIsNotCommittedBeforeValidation(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_order", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-order", AuthenticationClaimID: "auth-order",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-order", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-order", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), jobID, "paper.pdf"))

	fence, err := b.weighArtifact(ctx, jobID, "paper.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !fence.governed || fence.digest == "" {
		t.Fatalf("institutional attempt not governed: %+v", fence)
	}
	// Nothing may be won until the bytes have been through adoption.
	if _, ok, err := jobs.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
		t.Fatalf("winner committed before validation: ok=%v err=%v", ok, err)
	}
	if err := b.commitArtifact(ctx, jobID, "paper.pdf", fence, nil); err != nil {
		t.Fatal(err)
	}
	winner, ok, err := jobs.ArtifactWinner(ctx, jobID, 1)
	if err != nil || !ok || winner.SHA256 != fence.digest {
		t.Fatalf("winner does not describe the validated bytes: %+v ok=%v err=%v", winner, ok, err)
	}
}

// Ordering defence for ingestAdoptedFile itself: when adoption refuses the
// bytes, the attempt must still be winnable by a later valid delivery. A
// cancelled job is the cheapest adoption refusal available here.
func TestFailedAdoptionLeavesTheAttemptWinnable(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_failed_adopt", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-failed", AuthenticationClaimID: "auth-failed",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-failed", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-failed", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), jobID, "paper.pdf"))
	if err := jobs.Cancel(ctx, jobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ingestAdoptedFile(ctx, jobID, "paper.pdf", nil, nil); err == nil {
		t.Fatal("adoption of a cancelled job unexpectedly succeeded; pick another refusal")
	}
	if _, ok, err := jobs.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
		t.Fatalf("bytes that failed adoption won the attempt: ok=%v err=%v", ok, err)
	}
}

// The bridge must supply the revision the observation was PRODUCED under, not
// the one live when it arrives. This used to fall through: the correlated
// lookup asked for the job's *current* candidate, a query that deliberately
// hides candidates whose profile revision is superseded, so a frame buffered
// across an authority edit found nothing and was stamped with the new
// revision — the store then accepted it, because that revision really is live.
func TestBufferedFrameDoesNotPromoteIntoTheNewRevision(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_stale_rev", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-v1", AuthenticationClaimID: "auth-rev",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	// The route was offered under revision 1, so the candidate records it.
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-stale-rev", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	// The operator changes authority-relevant configuration: revision 2.
	bumped, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-v2", AuthenticationClaimID: "auth-rev",
	}})
	if err != nil || len(bumped) != 1 || bumped[0].Revision != profile.Revision+1 {
		t.Fatalf("expected a revision bump, got %+v %v", bumped, err)
	}

	// A frame produced under revision 1 now arrives.
	accepted, _, err := b.recordProfileEvidence(ctx, "buffered-obs", "default", jobID,
		job.ProfileEvidenceWarmVerified, job.ProfileEvidenceProbe,
		b.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("a frame produced under the superseded revision was accepted")
	}
	if _, ok, err := jobs.CurrentProfileEvidence(ctx, profile.ID, bumped[0].Revision, b.epoch); err != nil || ok {
		t.Fatalf("stale observation became current evidence for the new revision: ok=%v err=%v", ok, err)
	}
}

// A second sign-out for the same job must raise the login gate again after a
// login resolved it. auth_pending frames carry elapsed_ms only sometimes, so
// when it is absent every occurrence for a job hashed to the same observation
// id; upsertProfileGate treats an id match as an exact replay and returns
// early regardless of status, so the reopen was silently dropped and every
// sibling on that claim stayed parked with nothing to click.
func TestRepeatedAuthPendingWithoutElapsedReopensTheLoginGate(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_gate_reopen_msgid", handoffWork(), "alpha")
	openLoginGates := func() int {
		t.Helper()
		attention, err := jobs.CurrentHumanAttention(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, gate := range attention.Gates {
			if gate.GateType == job.HumanGateLogin {
				n++
			}
		}
		return n
	}
	authFrame := func(msgID string, typ string) *protocol.BrowserMessage {
		return &protocol.BrowserMessage{
			Type: typ, MsgID: msgID, JobID: jobID,
			Payload: &protocol.AuthPayload{}, // no elapsed_ms, as in the field
		}
	}
	if err := b.recordAuth(ctx, authFrame("pending-1", protocol.MsgAuthPending)); err != nil {
		t.Fatal(err)
	}
	if got := openLoginGates(); got != 1 {
		t.Fatalf("first sign-out opened %d login gates, want 1", got)
	}
	if err := b.recordAuth(ctx, authFrame("returned-1", protocol.MsgAuthReturned)); err != nil {
		t.Fatal(err)
	}
	if got := openLoginGates(); got != 0 {
		t.Fatalf("login left %d gates open, want 0", got)
	}
	// Same job, same absent elapsed_ms, genuinely new occurrence.
	if err := b.recordAuth(ctx, authFrame("pending-2", protocol.MsgAuthPending)); err != nil {
		t.Fatal(err)
	}
	if got := openLoginGates(); got != 1 {
		t.Fatalf("second sign-out reopened %d login gates, want 1", got)
	}
	// Exact replay of that same frame stays idempotent.
	if err := b.recordAuth(ctx, authFrame("pending-2", protocol.MsgAuthPending)); err != nil {
		t.Fatal(err)
	}
	if got := openLoginGates(); got != 1 {
		t.Fatalf("replaying one frame produced %d gates, want 1", got)
	}
}

// Free text authored by an adapter and relayed by the extension is untrusted
// input. Truncation is not sanitisation: a 500-character cap still admits a
// whole URL, a query token or a credential, and durable events are exactly
// where those must never land.
func TestProviderFreeTextIsRedactedBeforeItIsDurable(t *testing.T) {
	for _, test := range []struct {
		name  string
		in    string
		clean string
	}{
		{"url", "failed at https://provider.example.com/doi/pdf?token=abc123", "https://"},
		{"bare host", "redirected to sso.provider.example.com/login", "provider.example.com"},
		{"query token", "gave up ?session=9f8e7d6c5b4a", "?session="},
		{"long hex", "cookie 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := redactProviderDetail(test.in)
			if strings.Contains(got, test.clean) {
				t.Fatalf("redaction left %q in %q", test.clean, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("nothing was redacted from %q -> %q", test.in, got)
			}
		})
	}
	if got := redactProviderDetail("captcha challenge shown"); got != "captcha challenge shown" {
		t.Fatalf("ordinary diagnostic text was mangled: %q", got)
	}
}

// An offered direct-route tuple remains in flight until its exact permit is
// settled. An elapsed offer lease is diagnostic only: it cannot authorize a
// replay, because at-most-once permit semantics must not mint a second
// attempt. Reconciliation or an explicit break-glass action resolves a lost
// completion.
func TestUnacknowledgedDirectRouteOfferRemainsInFlightWithoutReplay(t *testing.T) {
	candidates := []routes.Candidate{{RouteRevision: "rev-1"}}
	issued := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	events := []map[string]any{{
		"kind": "browser.direct_route",
		"at":   issued.Format(time.RFC3339Nano),
		"detail": map[string]any{
			"route_revision": "rev-1", "ordinal": float64(0),
			"drive_attempt_id": "attempt-1", "phase": "offered",
		},
	}}
	if _, inFlight, _ := directRouteProgress(events, candidates, issued.Add(time.Minute)); !inFlight {
		t.Fatal("a fresh offer must still count as in flight")
	}
	_, inFlight, retainedAttempt := directRouteProgress(events, candidates, issued.Add(directRouteOfferLease+time.Second))
	if !inFlight {
		t.Fatal("an unacknowledged offer must remain in flight after its diagnostic lease")
	}
	if retainedAttempt != "attempt-1" {
		t.Fatalf("in-flight projection lost its diagnostic identity: %q", retainedAttempt)
	}
}

// Reconcile must not let the extension assert which tab a claim is bound to.
// The daemon is the authority on that binding; echoing the reported number
// back would confirm any tab as this materialization's tab.
func TestReconcileRefusesATabThatIsNotTheClaimsOwn(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	jobID := parkInstitutional(t, jobs, "wr_reconcile_tab", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-tab", AuthenticationClaimID: "auth-tab",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-tab", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-tab", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	const boundTab = 41
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profiles[0].Revision, boundTab); err != nil {
		t.Fatal(err)
	}
	reconcile := func(tab int64) *protocol.InstitutionalReconcileResponsePayload {
		t.Helper()
		frames, err := b.institutionalReconcile(ctx, &protocol.InstitutionalReconcileRequestPayload{
			RequestID: "req_reconcile_tab_fence",
			Bindings:  []protocol.InstitutionalReconcileBinding{{BindingID: claim.BindingID, TabID: tab}},
		})
		if err != nil || len(frames) != 1 {
			t.Fatalf("reconcile: frames=%d err=%v", len(frames), err)
		}
		msg, decodeErr := protocol.DecodeBrowserMessage(frames[0])
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return msg.Payload.(*protocol.InstitutionalReconcileResponsePayload)
	}
	if got := reconcile(boundTab); len(got.Claims) != 1 || got.Claims[0].TabID == nil || *got.Claims[0].TabID != boundTab {
		t.Fatalf("the claim's own tab was not confirmed: %+v", got)
	}
	if got := reconcile(boundTab + 1); len(got.Claims) != 0 {
		t.Fatalf("a tab the claim is not bound to was confirmed: %+v", got.Claims)
	}
}

// A claim is inserted with tab_id 0 in phase "claimed" and only gains a real
// tab when the bind lands, while the extension opens its scaffold tab BEFORE
// sending the bind. Reconcile after a worker death must still confirm such a
// claim: the extension treats an omitted binding as dead and closes the tab and
// clears the workflow, while the durable claim stays live to its lease and
// keeps the candidate blocked.
func TestReconcileConfirmsAClaimedButUnboundClaim(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	jobID := parkInstitutional(t, jobs, "wr_reconcile_unbound", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-unbound", AuthenticationClaimID: "auth-unbound",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-unbound", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	// Claimed, never bound: tab_id is still 0.
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-unbound", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := b.institutionalReconcile(ctx, &protocol.InstitutionalReconcileRequestPayload{
		RequestID: "req_reconcile_unbound_tab",
		Bindings:  []protocol.InstitutionalReconcileBinding{{BindingID: claim.BindingID, TabID: 77}},
	})
	if err != nil || len(frames) != 1 {
		t.Fatalf("reconcile: frames=%d err=%v", len(frames), err)
	}
	msg, decodeErr := protocol.DecodeBrowserMessage(frames[0])
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	got := msg.Payload.(*protocol.InstitutionalReconcileResponsePayload)
	if len(got.Claims) != 1 {
		t.Fatalf("an unbound claim was dropped; the extension would close its live tab: %+v", got.Claims)
	}
	if got.Claims[0].TabID != nil {
		t.Fatalf("the daemon asserted a tab it never bound: %v", *got.Claims[0].TabID)
	}
}

// The safety domain is the ONLY thing serializing irreversible effects across
// jobs: the scheduler excludes a candidate when a sibling sharing this value
// has a parked claim. Mixing the job id in gave every job a private domain, so
// that anti-join matched nothing and the fence silently did not exist.
func TestSafetyDomainIsSharedAcrossJobsOnOneProfile(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	first := parkInstitutional(t, jobs, "wr_domain_a", handoffWork(), "")
	second := parkInstitutional(t, jobs, "wr_domain_b", handoffWork(), "")
	rowA, err := jobs.Get(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	rowB, err := jobs.Get(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	candA, err := b.prepareMaterializationCandidate(ctx, *rowA)
	if err != nil || candA == nil {
		t.Fatalf("candidate a: %+v %v", candA, err)
	}
	candB, err := b.prepareMaterializationCandidate(ctx, *rowB)
	if err != nil || candB == nil {
		t.Fatalf("candidate b: %+v %v", candB, err)
	}
	if candA.SafetyDomainID != candB.SafetyDomainID {
		t.Fatalf("two jobs on one profile got private safety domains (%q vs %q); the cross-job fence cannot match",
			candA.SafetyDomainID, candB.SafetyDomainID)
	}
	if candA.PreRouteSafetyKey == candB.PreRouteSafetyKey {
		t.Fatal("pre-route safety keys must stay per job")
	}
}

// seedSurfaceCloseClaim creates a job, institution profile, browser
// candidate, and materialization claim, then forces the claim to the given
// phase via direct SQL — settled/abandoned are real handler outcomes with no
// single store constructor, so the fixture reaches them the same way
// TestReconcileConfirmsAClaimedButUnboundClaim reaches "claimed, never
// bound": build the claim, then set the exact state under test.
func seedSurfaceCloseClaim(t *testing.T, b *Bridge, jobs *job.Store, prefix, phase string) *job.MaterializationClaim {
	t.Helper()
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "wr_"+prefix, handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-" + prefix, AuthenticationClaimID: "auth-" + prefix,
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-" + prefix, JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety-" + prefix, SafetyDomainID: "domain-" + prefix,
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-" + prefix, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if phase != "claimed" {
		if _, err := jobs.S.DB().ExecContext(ctx,
			`UPDATE materialization_claims SET phase = ? WHERE id = ?`, phase, claim.ID,
		); err != nil {
			t.Fatal(err)
		}
		claim.Phase = phase
	}
	return claim
}

func decodeSurfaceCloseResponse(t *testing.T, frames []json.RawMessage) *protocol.SurfaceCloseResponsePayload {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("surface close: got %d frames, want 1", len(frames))
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	got, ok := msg.Payload.(*protocol.SurfaceCloseResponsePayload)
	if !ok {
		t.Fatalf("surface close: unexpected payload type %T", msg.Payload)
	}
	return got
}

// The happy path: a settled claim with disposition materialization_settled
// is authorized and mints a durable token.
func TestSurfaceCloseAuthorizedHappyPath(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-happy", "settled")

	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-happy", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "materialization_settled",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "authorized" {
		t.Fatalf("outcome = %q, want authorized (detail=%q)", got.Outcome, got.Detail)
	}
	if got.CloseAuthorizationID == "" || got.Nonce == "" {
		t.Fatalf("authorized response missing id/nonce: %+v", got)
	}
	if got.BrowserHolderGeneration == nil || *got.BrowserHolderGeneration != b.epoch {
		t.Fatalf("browser_holder_generation = %v, want %d", got.BrowserHolderGeneration, b.epoch)
	}

	var status string
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT status FROM close_authorizations WHERE id = ?`, got.CloseAuthorizationID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "issued" {
		t.Fatalf("close_authorizations.status = %q, want issued", status)
	}
}

// A terminal job has no acquisition lifecycle left to own its browser
// surface. job_inactive authorizes that exact binding even when its claim is
// still navigated - the phase that had no close disposition before this fix.
func TestSurfaceCloseJobInactiveAuthorizesTerminalNavigatedBinding(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-job-inactive", "navigated")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}

	frames, err := b.surfaceClose(ctx, &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-job-inactive", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "job_inactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "authorized" {
		t.Fatalf("outcome = %q, want authorized for terminal job (detail=%q)", got.Outcome, got.Detail)
	}
	var disposition string
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT disposition FROM close_authorizations WHERE id=?`,
		got.CloseAuthorizationID).Scan(&disposition); err != nil {
		t.Fatal(err)
	}
	if disposition != "job_inactive" {
		t.Fatalf("stored disposition = %q, want job_inactive", disposition)
	}
}

// A daemon restart clears offered/materializationTracked, but the durable
// claim and terminal job remain. Poll must still emit cancel so a restarted
// daemon cannot strand the extension's browser-local job and tab forever.
func TestPollCancelsTerminalMaterializationWithoutMemoryTracking(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "cancel-after-restart", "navigated")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	if len(b.offered) != 0 || len(b.materializationOffered) != 0 || len(b.materializationTracked) != 0 {
		t.Fatalf("fixture unexpectedly has worker-memory tracking: offered=%v materialization=%v tracked=%v",
			b.offered, b.materializationOffered, b.materializationTracked)
	}

	msgs, _ := runSync(t, b)
	cancel := firstOfType(msgs, protocol.MsgCancel)
	if cancel == nil || cancel.JobID != candidate.JobID {
		t.Fatalf("poll cancel = %+v, want terminal job %s: %v", cancel, candidate.JobID, msgs)
	}
}

// Durable terminal claims must still be cancelled after a daemon restart even
// when the terminal state is not cancelled. The durable query covers every
// terminal job state, so the poll cannot rely on worker-memory tracking to
// decide whether the browser surface needs its teardown frame.
func TestPollCancelsFailedMaterializationWithoutMemoryTrackingAndRetiresClaim(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "cancel-failed-after-restart", "navigated")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.Transition(ctx, candidate.JobID, job.StateAwaitingHuman, job.StateFailed, nil,
		job.WithTerminalReason("failed while materializing")); err != nil {
		t.Fatal(err)
	}
	if len(b.offered) != 0 || len(b.materializationOffered) != 0 || len(b.materializationTracked) != 0 {
		t.Fatalf("fixture unexpectedly has worker-memory tracking: offered=%v materialization=%v tracked=%v",
			b.offered, b.materializationOffered, b.materializationTracked)
	}

	msgs, _ := runSync(t, b)
	cancel := firstOfType(msgs, protocol.MsgCancel)
	if cancel == nil || cancel.JobID != candidate.JobID {
		t.Fatalf("poll cancel = %+v, want failed terminal job %s: %v", cancel, candidate.JobID, msgs)
	}
	if got, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID); err != nil {
		t.Fatal(err)
	} else if got == nil || got.Phase == "abandoned" {
		t.Fatalf("claim retired in the poll that announced cancel: %+v", got)
	}

	runSync(t, b)
	retired, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if retired == nil || retired.Phase != "abandoned" {
		t.Fatalf("failed terminal claim must retire after cancel delivery, got %+v", retired)
	}
}

// A durable terminal-claim backlog is a transport concern as well as a
// cleanup concern: emitting every cancel in one poll can exceed the native
// host's fatal result cap before any frame reaches the extension. The query
// page must therefore emit a bounded prefix while leaving later claims live
// for subsequent polls.
func TestTerminalCancelBatchStaysWithinResultCapAndLeavesRemainder(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))

	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "terminal-cancel-batch", AuthorityDigest: "terminal-cancel-batch-authority",
		AuthenticationClaimID: "terminal-cancel-batch-auth",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("reconcile terminal-cancel profile: %v (%d)", err, len(profiles))
	}
	profile := profiles[0]
	total := maxTerminalCancelsPerPoll + 1
	for i := range total {
		jobID := parkInstitutional(t, jobs, fmt.Sprintf("terminal-cancel-batch-%02d", i), handoffWork(), "")
		candidateID := fmt.Sprintf("terminal-cancel-batch-candidate-%02d", i)
		candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
			ID: candidateID, JobID: jobID, JobAttemptRevision: 1,
			InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
			RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
			PreRouteSafetyKey: fmt.Sprintf("terminal-cancel-batch-pre-route-%02d", i),
			SafetyDomainID:    fmt.Sprintf("terminal-cancel-batch-domain-%02d", i),
			AdapterRevision:   "terminal-cancel-batch-adapter", EffectContractID: "terminal-cancel-batch-effect",
			Status: "eligible",
		})
		if err != nil {
			t.Fatalf("create candidate %d: %v", i, err)
		}
		claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
			CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
			JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
			RouteRevision: 1, MaterializationKind: "browser_tab",
			LeaseUntil: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("claim candidate %d: %v", i, err)
		}
		if _, err := jobs.S.DB().ExecContext(ctx,
			`UPDATE materialization_claims SET phase='navigated' WHERE id=?`, claim.ID); err != nil {
			t.Fatalf("mark claim %d navigated: %v", i, err)
		}
		if err := jobs.Cancel(ctx, jobID, job.TerminalReasonCancelledByUser); err != nil {
			t.Fatalf("cancel job %d: %v", i, err)
		}
	}

	msgs, raw := runSync(t, b)
	cancelled := countType(msgs, protocol.MsgCancel)
	if cancelled != maxTerminalCancelsPerPoll {
		t.Fatalf("terminal cancel frames = %d, want bounded page %d", cancelled, maxTerminalCancelsPerPoll)
	}
	response, err := json.Marshal(map[string]any{"outbound": raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(response) >= ipc.MaxResultBytes {
		t.Fatalf("bounded terminal-cancel response = %d bytes, want < ipc.MaxResultBytes %d",
			len(response), ipc.MaxResultBytes)
	}
	var liveClaims int
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM materialization_claims WHERE phase='navigated'`).Scan(&liveClaims); err != nil {
		t.Fatal(err)
	}
	if liveClaims <= cancelled {
		t.Fatalf("terminal-cancel poll emitted %d frames but left only %d live claims; remainder was not preserved",
			cancelled, liveClaims)
	}
}

// Looking old is not enough: a nonterminal job with an open browser handoff
// still owns its navigated surface, so job_inactive must fail closed.
func TestSurfaceCloseJobInactiveRefusesLiveHandoff(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-job-live", "navigated")

	frames, err := b.surfaceClose(ctx, &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-job-live", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "job_inactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "not_eligible" {
		t.Fatalf("outcome = %q, want not_eligible for live handoff: %+v", got.Outcome, got)
	}
	if got.Detail == "" {
		t.Fatalf("live-handoff refusal must name itself: %+v", got)
	}
}

// Job termination is not permission to interrupt an irreversible provider
// effect. A held permit for this exact claim vetoes job_inactive until the
// effect settles.
func TestSurfaceCloseJobInactiveRefusesUnsettledEffect(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-job-effect", "claimed")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch,
		candidate.InstitutionProfileRevision, 9); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := jobs.AcquireInstitutionalEffectPermit(ctx,
		job.InstitutionalEffectPermitAcquireInput{
			JobID: candidate.JobID, ClaimID: claim.ID, BindingID: claim.BindingID,
			SafetyDomainID: candidate.SafetyDomainID, InstitutionalRequestID: "close-effect-request",
			JobAttemptRevision: candidate.JobAttemptRevision, BrowserHolderGeneration: b.epoch,
			ExpectedEffectOrdinal: 0, LeaseUntil: b.now().Add(time.Minute),
			Authorization: job.EffectPermitEvent{Kind: "institutional.authorized"},
		}); err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("effect permit outcome=%v err=%v", outcome, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}

	frames, err := b.surfaceClose(ctx, &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-job-effect", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "job_inactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "not_eligible" || got.Detail == "" {
		t.Fatalf("unsettled effect outcome = %+v, want named not_eligible", got)
	}
}

// A repeated authorized request for the same live binding must return the
// exact same token rather than minting (or refusing to mint) a second one.
func TestSurfaceCloseAuthorizedRepeatIsIdempotent(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-idempotent", "abandoned")

	req := &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-idempotent", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "claim_abandoned",
	}
	first, err := b.surfaceClose(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	firstResp := decodeSurfaceCloseResponse(t, first)
	if firstResp.Outcome != "authorized" {
		t.Fatalf("first outcome = %q, want authorized (detail=%q)", firstResp.Outcome, firstResp.Detail)
	}

	second, err := b.surfaceClose(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	secondResp := decodeSurfaceCloseResponse(t, second)
	if secondResp.Outcome != "authorized" {
		t.Fatalf("second outcome = %q, want authorized (detail=%q)", secondResp.Outcome, secondResp.Detail)
	}
	if secondResp.CloseAuthorizationID != firstResp.CloseAuthorizationID || secondResp.Nonce != firstResp.Nonce {
		t.Fatalf("repeat request minted a new token: first=%+v second=%+v", firstResp, secondResp)
	}
}

// A binding_id with no materialization_claims row at all — an unknown
// binding, or an extension-minted pre-cutover scaffold — is never
// daemon-AUTHORIZED, and is now reported as "unclaimed" rather than
// "not_eligible". The distinction is the whole point: this test used to
// assert the daemon refuses, and the extension dutifully obeyed that refusal
// for every ordinary handoff tab in existence. The daemon still never issues
// an authorization here; it just no longer claims a stake it does not have.
func TestSurfaceCloseUnknownBindingIsUnclaimed(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))

	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-unknown", BindingID: "binding-never-claimed-00001",
		BrowserHolderGeneration: b.epoch, Disposition: "scaffold_idle",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "unclaimed" {
		t.Fatalf("outcome = %q, want unclaimed", got.Outcome)
	}
	if got.Outcome == "authorized" {
		t.Fatal("an unknown binding must never be authorized")
	}
	if got.CloseAuthorizationID != "" || got.Nonce != "" || got.BrowserHolderGeneration != nil {
		t.Fatalf("unclaimed response carries authorization fields: %+v", got)
	}
}

// A request reporting a browser_holder_generation below the daemon's
// current fence is stale, not authorized and not a transport error.
func TestSurfaceCloseStaleGenerationIsRejected(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-stale", "settled")

	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-stale", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch - 1, Disposition: "materialization_settled",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "stale" {
		t.Fatalf("outcome = %q, want stale", got.Outcome)
	}
}

// A disposition inconsistent with the binding's current claim phase must
// not be authorized: e.g. materialization_settled on a claim still merely
// claimed.
func TestSurfaceClosePhaseMismatchIsNotEligible(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-mismatch", "claimed")

	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-mismatch", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "materialization_settled",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "not_eligible" {
		t.Fatalf("outcome = %q, want not_eligible", got.Outcome)
	}
}

// TestSurfaceCloseRequestDispatchedThroughSyncAuthorizes drives
// surface_close_request through the real inbound frame decode and dispatch
// path (Bridge.Sync -> handle -> the MsgSurfaceCloseRequest case), not the
// private surfaceClose method every other test in this file calls
// directly. A missing or mis-gated dispatcher entry (the case in handle()'s
// switch, or the holder/outdated gate ahead of it) falls through to the
// generic unknown-frame default, which is ErrInvalidFrame and fatal to
// Sync — so this test's runSync would fail loudly rather than silently
// passing if that wiring regressed.
func TestSurfaceCloseRequestDispatchedThroughSyncAuthorizes(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-dispatch", "settled")

	frame := inFrame(t, protocol.MsgSurfaceCloseRequest, "", protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-dispatch", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "materialization_settled",
	})
	msgs, _ := runSync(t, b, frame)
	resp := firstOfType(msgs, protocol.MsgSurfaceCloseResponse)
	if resp == nil {
		t.Fatalf("dispatched surface_close_request produced no surface_close_response: %v", msgs)
	}
	got := resp.Payload.(*protocol.SurfaceCloseResponsePayload)
	if got.Outcome != "authorized" || got.CloseAuthorizationID == "" || got.Nonce == "" {
		t.Fatalf("dispatched outcome = %+v, want authorized with id/nonce", got)
	}
}

// TestSurfaceCloseOfUnclaimedBindingIsNotARefusal pins the distinction the
// extension acts on. An ordinary URL-bearing handoff tab has no browser
// candidate, so it can never have a materialization claim, so this branch is
// the one EVERY such tab takes. It used to answer "not_eligible", which the
// extension correctly obeyed as a refusal - and since the handoff-drive
// timeout's close intent runs through exactly this request, every tab papio
// opened for a paper that reached an authentication wall was retained forever.
// Measured live 2026-08-21: fifteen surviving resolver tabs, thirteen of them
// identical, outliving the papers that opened them by days.
//
// "unclaimed" says the daemon has no stake: no claim means no candidate, no
// route, and no effect permit, because all three are claim-scoped. A refusal
// must still be a refusal, which the sibling assertion below pins.
func TestSurfaceCloseOfUnclaimedBindingIsNotARefusal(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))

	frame := inFrame(t, protocol.MsgSurfaceCloseRequest, "", protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-unclaimed", BindingID: "binding-with-no-claim",
		BrowserHolderGeneration: b.epoch, Disposition: "job_inactive",
	})
	msgs, _ := runSync(t, b, frame)
	resp := firstOfType(msgs, protocol.MsgSurfaceCloseResponse)
	if resp == nil {
		t.Fatalf("no surface_close_response for an unclaimed binding: %v", msgs)
	}
	got := resp.Payload.(*protocol.SurfaceCloseResponsePayload)
	if got.Outcome != "unclaimed" {
		t.Fatalf("unclaimed binding outcome = %q, want \"unclaimed\" - %q reads as a refusal the extension must obey, which strands the surface",
			got.Outcome, got.Outcome)
	}
	if got.CloseAuthorizationID != "" || got.Nonce != "" {
		t.Fatalf("unclaimed answer carried authorization fields: %+v", got)
	}

	// A claim whose phase does not permit closure is still a real refusal: the
	// daemon has a stake there, and the extension must not fall back to its
	// own authority. Same request shape, one difference - a live claim exists.
	held := seedSurfaceCloseClaim(t, b, jobs, "close-unclaimed-sibling", "claimed")
	refused := inFrame(t, protocol.MsgSurfaceCloseRequest, "", protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-refused", BindingID: held.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "materialization_settled",
	})
	refusedMsgs, _ := runSync(t, b, refused)
	refusedResp := firstOfType(refusedMsgs, protocol.MsgSurfaceCloseResponse)
	if refusedResp == nil {
		t.Fatalf("no surface_close_response for a live claim: %v", refusedMsgs)
	}
	if out := refusedResp.Payload.(*protocol.SurfaceCloseResponsePayload).Outcome; out == "unclaimed" {
		t.Fatal("a live claim answered \"unclaimed\": the extension would close a surface the daemon still has a stake in")
	}
}

// TestSurfaceCloseHandoffParkedAuthorizesAnUntouchedAsk pins the disposition
// that job_inactive could not express. A paper waiting for the operator keeps
// its handoff action OPEN by definition, so job_inactive is false for it and
// was refused on every reconcile pass - "the binding still has an active
// browser handoff", measured live 2026-08-21 against four surfaces the
// operator had not touched in days. Waiting is not a reason to hold a tab: the
// paper keeps its action and its place, and `papio actions open` mints a fresh
// surface when the operator actually wants one.
//
// papio's only stake in a surface is an in-flight provider effect, so that
// permit is the one veto - pinned by the second half below.
func TestSurfaceCloseHandoffParkedAuthorizesAnUntouchedAsk(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-parked", "navigated")

	frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-parked", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "handoff_parked",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeSurfaceCloseResponse(t, frames)
	if got.Outcome != "authorized" {
		t.Fatalf("parked handoff outcome = %q (%s), want authorized: a paper waiting for a human must not hold a tab it is not using",
			got.Outcome, got.Detail)
	}

	// The same request while a provider effect for this exact claim is in
	// flight must be refused: that is the one thing papio still has at stake.
	held := seedSurfaceCloseClaim(t, b, jobs, "close-parked-inflight", "navigated")
	seedLiveEffectPermitForClaim(t, jobs, held, "permit-parked-inflight")
	inflight, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-parked-inflight", BindingID: held.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "handoff_parked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeSurfaceCloseResponse(t, inflight).Outcome; out == "authorized" {
		t.Fatal("authorized a close while this binding's provider effect was in flight")
	}
}

// TestSurfaceCloseRequestFromNonHolderSessionIsRefused pins the holder gate
// ahead of the MsgSurfaceCloseRequest dispatch case in handle() — the same
// class of gate TestHandoffLinkRoutineOutcomesAndFreshResolution pins for
// handoff_link_request: a session that never became holder must be
// refused session_busy through the real dispatch path, never routed to the
// handler.
func TestSurfaceCloseRequestFromNonHolderSessionIsRefused(t *testing.T) {
	const holder = "sess-close-dispatch-holder-00000000000"
	const nonHolder = "sess-close-dispatch-pending-0000000000"
	b, jobs, _, _ := newBridge(t)
	runSyncAs(t, b, holder, materializationHello(t))
	runSyncAs(t, b, nonHolder, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "close-dispatch-nonholder", "settled")

	frame := inFrame(t, protocol.MsgSurfaceCloseRequest, "", protocol.SurfaceCloseRequestPayload{
		RequestID: "req-close-dispatch-nonholder", BindingID: claim.BindingID,
		BrowserHolderGeneration: b.epoch, Disposition: "materialization_settled",
	})
	msgs, _ := runSyncAs(t, b, nonHolder, frame)
	errFrame := firstOfType(msgs, protocol.MsgError)
	if errFrame == nil || errFrame.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("non-holder surface_close_request = %v, want session_busy", msgs)
	}
}

// surfaceCloseEligibleAt encodes surfaceClose's shipped disposition x phase
// eligibility matrix (bridge.go): scaffold_idle authorizes claimed/bound
// (subject to LiveEffectPermit ownership, pinned separately below),
// claim_abandoned authorizes only abandoned, and materialization_settled
// authorizes only settled. Every other combination must answer
// not_eligible — never silently authorize, and never error.
var surfaceCloseEligibleAt = map[string]map[string]bool{
	"scaffold_idle":           {"claimed": true, "bound": true},
	"claim_abandoned":         {"abandoned": true},
	"materialization_settled": {"settled": true},
}

// TestSurfaceCloseEligibilityMatrixAcrossPhases exercises every
// (disposition, phase) combination the shipped switch in surfaceClose
// distinguishes, across the full phase set a materialization claim can
// occupy (claimed, bound, route_issued, navigated, settled, abandoned).
// TestSurfaceCloseAuthorizedHappyPath, TestSurfaceCloseAuthorizedRepeatIsIdempotent
// and TestSurfaceClosePhaseMismatchIsNotEligible each pin one cell already;
// this proves the rest of the matrix — most importantly that route_issued
// and navigated never authorize ANY disposition, which none of the other
// tests cover.
func TestSurfaceCloseEligibilityMatrixAcrossPhases(t *testing.T) {
	phases := []string{"claimed", "bound", "route_issued", "navigated", "settled", "abandoned"}
	dispositions := []string{"scaffold_idle", "claim_abandoned", "materialization_settled"}
	for _, disposition := range dispositions {
		for _, phase := range phases {
			wantAuthorized := surfaceCloseEligibleAt[disposition][phase]
			t.Run(disposition+"_on_"+phase, func(t *testing.T) {
				b, jobs, _, _ := newBridge(t)
				runSync(t, b, materializationHello(t))
				claim := seedSurfaceCloseClaim(t, b, jobs, "matrix-"+disposition+"-"+phase, phase)
				frames, err := b.surfaceClose(context.Background(), &protocol.SurfaceCloseRequestPayload{
					RequestID: "req-matrix-" + disposition + "-" + phase, BindingID: claim.BindingID,
					BrowserHolderGeneration: b.epoch, Disposition: disposition,
				})
				if err != nil {
					t.Fatal(err)
				}
				got := decodeSurfaceCloseResponse(t, frames)
				switch {
				case wantAuthorized && got.Outcome != "authorized":
					t.Fatalf("phase=%s disposition=%s outcome=%q, want authorized (detail=%q)", phase, disposition, got.Outcome, got.Detail)
				case !wantAuthorized && got.Outcome != "not_eligible":
					t.Fatalf("phase=%s disposition=%s outcome=%q, want not_eligible", phase, disposition, got.Outcome)
				}
			})
		}
	}
}

// seedLiveEffectPermitForClaim inserts a held institutional effect_permits
// row tied to claim — the same shape production code writes when a browser
// tab actually drives an institutional effect — so scaffold_idle's
// LiveEffectPermit occupancy check has something live to see. Only one live
// permit can ever exist at a time (effect_permits_live_slot's slot_index
// uniqueness), so a test using this must not seed a second one on the same
// store.
func seedLiveEffectPermitForClaim(t *testing.T, jobs *job.Store, claim *job.MaterializationClaim, permitID string) {
	t.Helper()
	ctx := context.Background()
	var jobID string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT job_id FROM browser_candidates WHERE id = ?`, claim.CandidateID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := jobs.S.DB().ExecContext(ctx, `
		INSERT INTO effect_permits
		  (id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind,
		   claim_id, binding_id, effect_ordinal, institutional_request_id, status, lease_until, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, 'institutional', ?, ?, 1, ?, 'held', ?, ?, ?)`,
		permitID, jobID, claim.BrowserHolderGeneration, "domain-"+permitID,
		claim.ID, claim.BindingID, "institutional-request-"+permitID,
		time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), now, now,
	); err != nil {
		t.Fatal(err)
	}
}

// TestSurfaceCloseScaffoldIdleRespectsLiveEffectPermitOwnership: the
// scaffold_idle phase check (claimed/bound) alone is not sufficient —
// LiveEffectPermit must also show no live effect occupying THIS exact
// claim. A permit already held for a DIFFERENT claim must never block; only
// occupancy on the claim being closed does.
func TestSurfaceCloseScaffoldIdleRespectsLiveEffectPermitOwnership(t *testing.T) {
	ctx := context.Background()
	t.Run("permit_on_same_claim_blocks", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		runSync(t, b, materializationHello(t))
		claim := seedSurfaceCloseClaim(t, b, jobs, "matrix-permit-same", "bound")
		seedLiveEffectPermitForClaim(t, jobs, claim, "permit-matrix-same")

		frames, err := b.surfaceClose(ctx, &protocol.SurfaceCloseRequestPayload{
			RequestID: "req-matrix-permit-same", BindingID: claim.BindingID,
			BrowserHolderGeneration: b.epoch, Disposition: "scaffold_idle",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := decodeSurfaceCloseResponse(t, frames)
		if got.Outcome != "not_eligible" {
			t.Fatalf("outcome = %q, want not_eligible (a live permit on the same claim occupies the scaffold)", got.Outcome)
		}
	})
	t.Run("permit_on_different_claim_does_not_block", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		runSync(t, b, materializationHello(t))
		claim := seedSurfaceCloseClaim(t, b, jobs, "matrix-permit-diff", "bound")
		other := seedSurfaceCloseClaim(t, b, jobs, "matrix-permit-diff-other", "bound")
		seedLiveEffectPermitForClaim(t, jobs, other, "permit-matrix-diff")

		frames, err := b.surfaceClose(ctx, &protocol.SurfaceCloseRequestPayload{
			RequestID: "req-matrix-permit-diff", BindingID: claim.BindingID,
			BrowserHolderGeneration: b.epoch, Disposition: "scaffold_idle",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := decodeSurfaceCloseResponse(t, frames)
		if got.Outcome != "authorized" {
			t.Fatalf("outcome = %q, want authorized (detail=%q)", got.Outcome, got.Detail)
		}
	})
}

// End-to-end: the redaction must be applied where the event is written, not
// merely available as a helper.
func TestPersistedProviderOutcomeDetailIsRedacted(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_redact_event", handoffWork(), "")
	if err := b.outcome(ctx, jobID, "msg-redact", &protocol.ProviderOutcomePayload{
		Outcome: "ui_changed",
		Detail:  "stuck at https://provider.example.com/doi/pdf?token=secret123",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] != "browser.provider_outcome" {
			continue
		}
		found = true
		detail, _ := event["detail"].(map[string]any)
		text, _ := detail["detail"].(string)
		if strings.Contains(text, "https://") || strings.Contains(text, "provider.example.com") || strings.Contains(text, "secret123") {
			t.Fatalf("durable event retained provider identity or a token: %q", text)
		}
	}
	if !found {
		t.Fatal("no provider outcome event was recorded")
	}
}

// The offer lease must never touch a tuple that was answered. Applying it to
// the in-flight latch itself un-terminated answered tuples: ten minutes after
// a login/terms result the identical candidate became re-drivable, and a
// not_pdf result stopped advancing the ordinal.
func TestDirectRouteLeaseDoesNotResurrectAnsweredTuples(t *testing.T) {
	candidates := []routes.Candidate{{RouteRevision: "rev-1"}, {RouteRevision: "rev-2"}}
	issued := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	longAfter := issued.Add(directRouteOfferLease + time.Hour)
	offer := map[string]any{
		"kind": "browser.direct_route", "at": issued.Format(time.RFC3339Nano),
		"detail": map[string]any{
			"route_revision": "rev-1", "ordinal": float64(0),
			"drive_attempt_id": "attempt-1", "phase": "offered",
		},
	}
	result := func(outcome string) map[string]any {
		return map[string]any{
			"kind": "browser.direct_route", "at": issued.Add(time.Second).Format(time.RFC3339Nano),
			"detail": map[string]any{
				"route_revision": "rev-1", "ordinal": float64(0),
				"drive_attempt_id": "attempt-1", "phase": "result", "outcome": outcome,
			},
		}
	}
	// A terminal non-advancing result stays terminal forever.
	next, inFlight, pending := directRouteProgress([]map[string]any{offer, result("login")}, candidates, longAfter)
	if next != 0 || !inFlight || pending != "" {
		t.Fatalf("answered login tuple = (next=%d inFlight=%v pending=%q); want it to stay terminal at ordinal 0",
			next, inFlight, pending)
	}
	// An advancing result keeps its advance.
	next, inFlight, _ = directRouteProgress([]map[string]any{offer, result("not_pdf")}, candidates, longAfter)
	if next != 1 || inFlight {
		t.Fatalf("answered not_pdf tuple = (next=%d inFlight=%v); want ordinal 1 and not in flight", next, inFlight)
	}
}

// A started effect remains occupying across lease expiry. Elapsed time does not
// authorize a successor or release the lane.
func TestStalledDriveEpochIsSupersededByTheNextOffer(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	ctx := context.Background()
	runSync(t, b, helloAs("0.14.0"))
	effectPermitHolder(t, b)
	id := park(t, jobs, "wr_epoch_stall", handoffWork())
	attempt := "epoch-stalled-0001"
	domain := "institution:example.edu"
	effectPermitOffer(t, jobs, id, attempt, domain)
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}
	if frames, err := b.providerDriveEpochStart(ctx, id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v", err)
	}
	// Lease expiry is not authorization.
	if b.driveEpochStalled(id, attempt, 0) {
		t.Fatal("a just-started epoch must not be reported as stalled")
	}
	b.now = func() time.Time { return time.Now().UTC().Add(providerDriveEpochLease + time.Minute) }
	if b.driveEpochStalled(id, attempt, 0) {
		t.Fatal("elapsed time must not report a stalled effect")
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Poll/offer while occupying must not mint a successor or a new drive tuple.
	msgs, _ := runSync(t, b)
	for _, m := range msgs {
		if m.JobID == id && m.Type == protocol.MsgJobOffer {
			t.Fatalf("occupying permit was re-offered at poll: %v", msgs)
		}
	}
	offer, err := b.offer(*row, job.HumanAction{Kind: handoffActionKind, Detail: "institutional handoff"}, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(offer)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID; got != "" {
		t.Fatalf("occupying offer minted drive_attempt_id %q, want no tuple", got)
	}
	next, _, ok := b.latestProviderDriveEpoch(id)
	if ok && next != attempt {
		t.Fatalf("offer minted successor %q, want no supersession", next)
	}
	// Same tuple remains the exact held authorization replay, never a
	// successor and never a new permit row.
	frames, err := b.providerDriveEpochStart(ctx, id, start)
	if err != nil || len(frames) != 1 {
		t.Fatalf("start after lease: frames=%d err=%v", len(frames), err)
	}
	if got := permitOutcome(t, frames); got != "started" {
		t.Fatalf("exact held re-start = %q, want started", got)
	}
}

// The drive-epoch result path is the third sink for the same adapter-authored
// free text and was left on truncate() while the other two were redacted.
func TestDriveEpochResultDetailIsRedacted(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_epoch_redact", handoffWork())
	attempt := "epoch-redact-0001"
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic",
		"revision": "1", "safety_domain": "institution:example.edu",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "not_pdf", Detail: "landed on https://provider.example.com/login?token=abc123",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] != "browser.provider_drive_epoch_result" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		text, _ := detail["detail"].(string)
		if strings.Contains(text, "https://") || strings.Contains(text, "provider.example.com") || strings.Contains(text, "token=") {
			t.Fatalf("drive epoch result retained provider identity or a token: %q", text)
		}
	}
}

// route_suppressions had a complete store implementation, a scheduler
// anti-join that consumes it, and no producer anywhere — so a route that proved
// it had no entitlement was re-selected on the very next pass.
func TestNoEntitlementSuppressesTheRouteItProved(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_suppress", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-sup", AuthenticationClaimID: "auth-sup",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-suppress", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 3, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain-sup",
		AdapterRevision: "adapter-7", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	key := job.RouteSuppressionKey{
		JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: candidate.RouteRevision, SafetyDomainID: candidate.SafetyDomainID,
		AdapterRevision: candidate.AdapterRevision, IdentifierStrategy: candidate.IdentifierStrategy,
	}
	if active, err := jobs.ActiveRouteSuppressions(ctx, key); err != nil || len(active) != 0 {
		t.Fatalf("expected no suppression before the outcome: %+v %v", active, err)
	}
	if err := b.outcome(ctx, jobID, "msg-no-ent", &protocol.ProviderOutcomePayload{
		Outcome: "no_entitlement", Detail: "not licensed",
	}); err != nil {
		t.Fatal(err)
	}
	active, err := jobs.ActiveRouteSuppressions(ctx, key)
	if err != nil || len(active) != 1 {
		t.Fatalf("no_entitlement did not suppress the route it disproved: %+v %v", active, err)
	}
	if active[0].Reason != job.RouteSuppressionNoEntitlement {
		t.Fatalf("suppression reason = %q", active[0].Reason)
	}
}

// A login or CAPTCHA is human-paced and routinely outlives the action expiry.
// RenewMaterializationClaim had no caller, so reconciliation abandoned the
// claim underneath the user and the bytes that finally landed could not be
// fenced.
func TestAuthTrafficRenewsTheMaterializationLease(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	jobID := parkInstitutional(t, jobs, "wr_renew", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-renew", AuthenticationClaimID: "auth-renew",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-renew", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-renew", BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.recordAuth(ctx, &protocol.BrowserMessage{
		Type: protocol.MsgAuthPending, MsgID: "renew-pending-1", JobID: jobID,
		Payload: &protocol.AuthPayload{},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LeaseUntil <= before.LeaseUntil {
		t.Fatalf("auth traffic did not extend the lease: %q -> %q", before.LeaseUntil, after.LeaseUntil)
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
func TestISBNBookHandoffOfferAdvertisesPersistedAssistedCeiling(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, "wr_offer_isbn_assisted", work.Work{
		ISBN: "9780306406157", Title: "A Book", Authors: []string{"Jane Smith"}, Year: 2024,
	}, "", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
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
	if err := jobs.NarrowPolicyAccessMode(ctx, id, config.ModeAssisted); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, app.InstitutionalBookOpenURLHandoffDetail,
		job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}

	msgs, _ := runSync(t, b, hello())
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatal("missing ISBN book handoff offer")
	}
	if got := offer.Payload.(*protocol.JobOfferPayload).AccessMode; got != config.ModeAssisted {
		t.Fatalf("ISBN offer access_mode = %q, want %q", got, config.ModeAssisted)
	}
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
// protocol.MaxBrowserMessageBytes, (b) at most one max-size solicited
// response rides one poll (job_offer/handoff-type replies are one-at-a-time
// correlated calls), and
// (c) every batch the daemon can otherwise grow unboundedly is capped: the
// offer, focus, and durable terminal-cancel batches by
// maxOutstandingOffers/maxFocusFramesPerPoll/maxTerminalCancelsPerPoll, and
// the claim_observation_ack batch by maxClaimObservationsPerPoll (the
// extension MUST send at most this many queued claim_observation frames per
// poll, dev/active/claim-observation-protocol.md §5). Loosening any of those
// trips this test.
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
	// A claim_observation_ack at its legal maxima: 128-char ids and a
	// full-width 1000-char detail (only legal on a non-applied outcome, but
	// this is a worst-case size bound, not a realistic single reply).
	ack, err := b.frame(protocol.MsgClaimObservationAck, "job_"+strings.Repeat("o", 26), protocol.ClaimObservationAckPayload{
		RequestID: strings.Repeat("r", 128), Outcome: "error", Detail: strings.Repeat("e", 1000),
		GateOccurrenceID: strings.Repeat("g", 128), BrowserHolderGeneration: protocol.MaxBrowserInteger,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cancel and focus frames carry only a job id and an empty payload, so
	// sizing every batched frame as a maximal offer is deliberately pessimistic.
	batched := (maxOutstandingOffers+maxFocusFramesPerPoll+maxTerminalCancelsPerPoll)*len(offer) +
		maxClaimObservationsPerPoll*len(ack)
	worst := protocol.MaxBrowserMessageBytes + batched
	if worst > ipc.MaxResultBytes {
		t.Fatalf("worst-case sync response %d bytes exceeds ipc.MaxResultBytes %d: one max-size solicited response (%d) + %d offer/focus/cancel frames of %d bytes + %d claim_observation_ack frames of %d bytes",
			worst, ipc.MaxResultBytes, protocol.MaxBrowserMessageBytes,
			maxOutstandingOffers+maxFocusFramesPerPoll+maxTerminalCancelsPerPoll, len(offer),
			maxClaimObservationsPerPoll, len(ack))
	}
}

// appendEventAt inserts a raw job event with a caller-controlled timestamp,
// bypassing store.Now() so a test can lay out a precise epoch timeline.
func appendEventAt(t *testing.T, jobs *job.Store, jobID, kind string, detail map[string]any, at time.Time) {
	t.Helper()
	if detail == nil {
		detail = map[string]any{}
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(),
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
		jobID, at.UTC().Format(time.RFC3339Nano), kind, string(data)); err != nil {
		t.Fatal(err)
	}
}

// openHandoffAction finds the open handoffActionKind action for a job, as
// jobs.OpenHumanAction records it — the fixture helpers below need its
// CreatedAt to anchor a synthetic event timeline to the real action.
func openHandoffAction(t *testing.T, jobs *job.Store, jobID string) job.HumanAction {
	t.Helper()
	ctx := context.Background()
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.JobID == jobID && a.Kind == handoffActionKind {
			return a
		}
	}
	t.Fatalf("no open handoff action for %s", jobID)
	return job.HumanAction{}
}

// TestAutomaticHandoffQuiescesFruitlessEpochsAcrossRestart reproduces the
// verified field incident purely through event evidence: an action seconds
// old — nowhere near QuiesceAfter's seven-day fence — whose accepted browser
// drives never produced a terminal outcome. Ten reconnect offer/accept pairs
// inside one lease must collapse into a single fruitless epoch, not ten;
// after three fruitless epochs the automatic offer must stop, with exactly
// one audit event and the action left open; an explicit `papio actions open`
// must still get its drive.
func TestAutomaticHandoffQuiescesFruitlessEpochsAcrossRestart(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_quiesce_evidence", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Epoch 1: initial offer+accept, then ten reconnect offer/accept pairs
	// inside the accepted lease — a service-worker restart re-acking the
	// same physical drive, not ten separate drives.
	epoch1Start := created.Add(time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch1Start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch1Start.Add(time.Second))
	for i := range 10 {
		at := epoch1Start.Add(time.Duration(i+1) * 20 * time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, at)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, at.Add(time.Second))
	}

	// Confirm the collapse directly: right after epoch 1's lease elapses,
	// with no outcome recorded, that is exactly ONE fruitless epoch — not ten.
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	afterEpoch1 := epoch1Start.Add(job.HandoffAcceptedLease + time.Second)
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, afterEpoch1)
	if state.FruitlessEpochs != 1 {
		t.Fatalf("fruitless epochs after ten reconnects = %d, want 1 (reconnects must collapse into one epoch)", state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced after only one fruitless epoch")
	}

	// Epoch 2 and epoch 3: same shape, each started after the previous
	// epoch's lease has already elapsed, neither ever seeing an outcome.
	epoch2Start := epoch1Start.Add(job.HandoffAcceptedLease + 10*time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch2Start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch2Start.Add(time.Second))

	epoch3Start := epoch2Start.Add(job.HandoffAcceptedLease + 10*time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch3Start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch3Start.Add(time.Second))

	finalNow := epoch3Start.Add(job.HandoffAcceptedLease + time.Second)
	b.now = func() time.Time { return finalNow }

	// First sweep: epoch 3's lease has just elapsed with no outcome, which is
	// the third fruitless epoch. The automatic offer must be suppressed and
	// the quiesce audited exactly once.
	msgs, _ := runSync(t, b, hello())
	if got := countType(msgs, protocol.MsgJobOffer); got != 0 {
		t.Fatalf("automatic offer after three fruitless epochs = %d, want 0", got)
	}
	if b.offered[id] {
		t.Fatalf("job marked offered despite fruitless-epoch quiesce: %#v", b.offered)
	}

	// A second sweep must not append a second audit event.
	runSync(t, b)

	all, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	quiescedEvents := 0
	for _, ev := range all {
		if kind, _ := ev["kind"].(string); kind == "browser.handoff_quiesced" {
			quiescedEvents++
		}
	}
	if quiescedEvents != 1 {
		t.Fatalf("browser.handoff_quiesced events = %d, want exactly 1", quiescedEvents)
	}

	openActions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	stillOpen := false
	for _, a := range openActions {
		if a.ID == action.ID && a.Status == "open" {
			stillOpen = true
		}
	}
	if !stillOpen {
		t.Fatalf("quiesced action must stay open, got %+v", openActions)
	}

	// An explicit `papio actions open` still overrides both fences.
	b.mu.Lock()
	b.focusPending[id] = true
	b.mu.Unlock()
	msgs, _ = runSync(t, b)
	if got := countType(msgs, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("focusPending offer after quiesce = %d, want 1", got)
	}
}

// TestProjectHandoffOfferStateProviderOutcomeResetsFruitlessCount confirms a
// terminal outcome inside an epoch — not a job_reject or a transport
// failure — is what clears the fruitless streak.
func TestProjectHandoffOfferStateProviderOutcomeResetsFruitlessCount(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_quiesce_reset", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// One fruitless epoch, same shape as the main test.
	epoch1Start := created.Add(time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch1Start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch1Start.Add(time.Second))

	// A second epoch that DOES get a terminal outcome inside its lease.
	epoch2Start := epoch1Start.Add(job.HandoffAcceptedLease + 10*time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch2Start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch2Start.Add(time.Second))
	appendEventAt(t, jobs, id, "browser.provider_outcome",
		map[string]any{"outcome": "landing_only"}, epoch2Start.Add(2*time.Minute))

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := epoch2Start.Add(job.HandoffAcceptedLease + time.Minute)
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, now)
	if state.FruitlessEpochs != 0 {
		t.Fatalf("fruitless epochs after a terminal outcome = %d, want 0 (provider_outcome must reset the streak)", state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced despite a terminal outcome resetting the streak")
	}
}

// A drive interrupted by a security check the operator then SOLVED must be
// charged nothing — and must credit nothing either. Measured live 2026-08-22:
// job_012f55be2bbfe0abd0ce456e36 quiesced at three epochs, every one of them
// interrupted by a Cloudflare check that was subsequently cleared, so the
// operator solved captchas and the paper was retired for it. The daemon could
// not see any of that: it only ever received `browser.error
// {challenge_blocked}`, which is neither terminal nor progress.
func TestProjectHandoffOfferStateClearedChallengeIsNeitherChargedNorCredited(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_challenge_cleared", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// One genuinely fruitless epoch, so the streak is non-zero and a wrongly
	// credited clear would be visible as a reset to 0.
	epoch1 := created.Add(time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch1)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch1.Add(time.Second))

	// A second epoch that hits a check and has it cleared, deliberately AFTER
	// the lease has elapsed: solving a captcha routinely outruns ten minutes,
	// which is exactly when the boundary would otherwise charge it first.
	epoch2 := epoch1.Add(job.HandoffAcceptedLease + 10*time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch2)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch2.Add(time.Second))
	appendEventAt(t, jobs, id, "browser.error", map[string]any{"code": "challenge_blocked"}, epoch2.Add(2*time.Second))
	appendEventAt(t, jobs, id, job.ChallengeClearedEvent, nil, epoch2.Add(job.HandoffAcceptedLease+5*time.Minute))

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := epoch2.Add(2 * job.HandoffAcceptedLease)
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, now)
	if state.FruitlessEpochs != 1 {
		t.Fatalf("fruitless epochs after a cleared challenge = %d, want 1: the interrupted drive must be neither charged (2) nor credited (0)", state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced on one fruitless epoch")
	}
}

// The other half, and the reason a cleared check must not credit: a provider
// that challenges on every single attempt would otherwise refill the budget
// forever and MaxAutomaticHandoffEpochs could never fire. That is the
// immortal-handoff hazard HandoffEpochsResetEvent's own comment refuses to
// open, so it must not be opened here by the back door.
func TestProjectHandoffOfferStateClearedChallengesCannotMakeAHandoffImmortal(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_challenge_immortal", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Three drives, each interrupted by a check that was cleared, and each
	// then going silent for its whole lease afterwards. The clear closes its
	// own epoch uncounted; the SILENCE that follows in the next epoch is what
	// still gets charged, so the budget still runs out.
	start := created.Add(time.Second)
	for i := range 4 {
		epoch := start.Add(time.Duration(i) * (job.HandoffAcceptedLease + time.Minute))
		appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epoch)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch.Add(time.Second))
		appendEventAt(t, jobs, id, job.ChallengeClearedEvent, nil, epoch.Add(2*time.Second))
		// Same epoch window, a second accept after the clear: this is the
		// resumed drive, and it reports nothing at all.
		appendEventAt(t, jobs, id, "browser.job_accept", nil, epoch.Add(3*time.Second))
	}

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := start.Add(5 * (job.HandoffAcceptedLease + time.Minute))
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, now)
	if state.FruitlessEpochs < job.MaxAutomaticHandoffEpochs {
		t.Fatalf("fruitless epochs after four cleared-then-silent drives = %d, want >= %d: a cleared check must not refill the budget",
			state.FruitlessEpochs, job.MaxAutomaticHandoffEpochs)
	}
	if !state.Quiesced {
		t.Fatal("a paper whose every drive went silent after its check cleared must still quiesce")
	}
}

// The wire half: the frame the extension sends on a positive re-assessment of
// its own tab lands on the event stream the fold reads. Timing-only payload —
// the provider host never crosses this channel.
func TestChallengeClearedFrameRecordsTheFoldsEvent(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_challenge_frame", handoffWork())
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgChallengeCleared, id, map[string]any{"elapsed_ms": 91_000}))

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind == job.ChallengeClearedEvent {
			found = true
		}
	}
	if !found {
		t.Fatalf("challenge_cleared frame recorded no %s event", job.ChallengeClearedEvent)
	}
}

// TestProjectHandoffOfferStateLateTerminalEventResetsInsteadOfCharging pins
// the P1 boundary bug terminalHandoffEvent fixes: a signal that proves the
// drive did something can land AFTER the lease already elapsed — a slow SSO
// or 2FA detour easily outruns ten minutes — and it must still reset the
// epoch, not get force-closed as fruitless by the boundary check first.
//
// Pre-fix, the boundary check ran unconditionally on every non-offer/accept
// event once the lease elapsed, so it force-closed the epoch as fruitless the
// instant that happened; the terminal event's own closeEpoch(false) then
// no-opped on an already-shut epoch (closeEpoch returns immediately when
// !open), silently losing the reset it should have recorded. Confirmed these
// subtests fail against that pre-fix shape by running the boundary-check
// logic exactly as reconstructed above (unconditional lease force-close, no
// terminalHandoffEvent exemption) against each history below in isolation:
// it reproduces FruitlessEpochs=1 for the two single-late-event cases and
// Quiesced=true for the three-drives case, all of which the assertions below
// now forbid.
func TestProjectHandoffOfferStateLateTerminalEventResetsInsteadOfCharging(t *testing.T) {
	t.Run("ProviderOutcomePastLease", func(t *testing.T) {
		_, jobs, _, _ := newBridge(t)
		ctx := context.Background()
		id := park(t, jobs, "wr_late_terminal", handoffWork())
		action := openHandoffAction(t, jobs, id)
		created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}

		epochStart := created.Add(time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epochStart)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, epochStart.Add(time.Second))
		// Past job.HandoffAcceptedLease (10m): a slow SSO or 2FA detour makes
		// this an ordinary drive, not a pathological input.
		lateOutcome := epochStart.Add(11 * time.Minute)
		appendEventAt(t, jobs, id, "browser.provider_outcome", map[string]any{"outcome": "resolved"}, lateOutcome)

		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		state := job.ProjectHandoffOfferState(events, action.CreatedAt, lateOutcome.Add(time.Minute))
		if state.FruitlessEpochs != 0 {
			t.Fatalf("fruitless epochs after a late-but-terminal outcome = %d, want 0 (the lease bounds silence, not a drive that eventually reported)", state.FruitlessEpochs)
		}
		if state.Quiesced {
			t.Fatal("quiesced a job whose drive eventually reported a terminal outcome, just past the lease")
		}
	})

	t.Run("ThreeSlowButSuccessfulDrives", func(t *testing.T) {
		_, jobs, _, _ := newBridge(t)
		ctx := context.Background()
		id := park(t, jobs, "wr_slow_drives", handoffWork())
		action := openHandoffAction(t, jobs, id)
		created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}

		// Three drives in a row, each genuinely slow (past the 10-minute
		// lease) but each eventually terminal. Pre-fix each one hit the
		// boundary check first and was force-closed as fruitless, reaching
		// MaxAutomaticHandoffEpochs (3) and quiescing a job that never
		// actually failed a single drive.
		epochStart := created.Add(time.Second)
		var lastOutcome time.Time
		for i := range 3 {
			if i > 0 {
				epochStart = lastOutcome.Add(10 * time.Second)
			}
			appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epochStart)
			appendEventAt(t, jobs, id, "browser.job_accept", nil, epochStart.Add(time.Second))
			lastOutcome = epochStart.Add(11 * time.Minute)
			appendEventAt(t, jobs, id, "browser.provider_outcome", map[string]any{"outcome": "resolved"}, lastOutcome)
		}

		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		state := job.ProjectHandoffOfferState(events, action.CreatedAt, lastOutcome.Add(time.Minute))
		if state.FruitlessEpochs != 0 {
			t.Fatalf("fruitless epochs after three slow-but-successful drives = %d, want 0", state.FruitlessEpochs)
		}
		if state.Quiesced {
			t.Fatal("quiesced a perfectly healthy job after three drives that were merely slow, not fruitless")
		}
	})

	t.Run("LateTransitionOutOfAwaitingHuman", func(t *testing.T) {
		_, jobs, _, _ := newBridge(t)
		ctx := context.Background()
		id := park(t, jobs, "wr_late_transition", handoffWork())
		action := openHandoffAction(t, jobs, id)
		created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}

		// The other terminalHandoffEvent branch: the job itself leaves
		// awaiting_human (e.g. resolved out of band) after the lease
		// elapsed, with no browser-side terminal event at all.
		epochStart := created.Add(time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, epochStart)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, epochStart.Add(time.Second))
		lateTransition := epochStart.Add(11 * time.Minute)
		appendEventAt(t, jobs, id, "job.transition",
			map[string]any{"from": job.StateAwaitingHuman, "to": job.StateResolving}, lateTransition)

		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		state := job.ProjectHandoffOfferState(events, action.CreatedAt, lateTransition.Add(time.Minute))
		if state.FruitlessEpochs != 0 {
			t.Fatalf("fruitless epochs after a late awaiting_human exit = %d, want 0", state.FruitlessEpochs)
		}
		if state.Quiesced {
			t.Fatal("quiesced a job whose awaiting_human exit landed just past the lease")
		}
	})
}

// TestAcceptedLeaseOutlastsTheExtensionQueueWait pins the reason the lease is
// ten minutes and not five. The extension sends job_accept on its QUEUED path
// too, so an accepted handoff can sit undriven while the drive governor is
// saturated — up to maxOutstandingOffers accepted handoffs against
// HANDOFF_DRIVE_LIMIT (extension/src/background.ts, 2) slots, each held up to
// HANDOFF_DRIVE_TIMEOUT_MS (extension/src/background.ts, 3 minutes), plus the
// QUEUED_HANDOFF_RELEASE_MS (extension/src/background.ts, 45 seconds)
// evidence-free release ADR-0013 ratifies. maxOutstandingOffers is referenced
// directly (both live in package browser) so a future change to the
// governor's shape must restate this arithmetic rather than silently
// invalidate the lease; the extension-side terms stay literal because they
// are TypeScript constants this package cannot import. Nothing daemon-side
// tells a queued handoff apart from a drive that produced nothing, so a lease
// shorter than the wait would charge a job for queueing and quiesce a healthy
// backlog. A five-minute lease failed this.
func TestAcceptedLeaseOutlastsTheExtensionQueueWait(t *testing.T) {
	const worstQueueWait = (maxOutstandingOffers/2)*3*time.Minute + 45*time.Second
	if job.HandoffAcceptedLease <= worstQueueWait {
		t.Fatalf("accepted lease %s does not outlast the extension's %s queue wait: a handoff still waiting its turn would be charged as a fruitless drive",
			job.HandoffAcceptedLease, worstQueueWait)
	}

	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_queue_wait", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Offered and accepted, then nothing: the extension queued it behind a
	// saturated governor and has not driven it even once.
	start := created.Add(time.Second)
	appendEventAt(t, jobs, id, "browser.handoff_offered", map[string]any{"requires_auth": false}, start)
	appendEventAt(t, jobs, id, "browser.job_accept", nil, start.Add(time.Second))

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, start.Add(worstQueueWait))
	if state.FruitlessEpochs != 0 {
		t.Fatalf("fruitless epochs while still inside the queue wait = %d, want 0", state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced a handoff that was only waiting for a drive slot")
	}
}

// bulkHoldingsProvider is an aligned ownership fixture with per-identifier
// artifact evidence. Missing entries are complete no-claims unless explicitly
// listed as incomplete.
type bulkHoldingsProvider struct {
	artifacts  map[string]string
	incomplete map[string]bool
}

func (p bulkHoldingsProvider) Name() string { return "test-holdings" }

func (p bulkHoldingsProvider) Lookup(_ context.Context, queries []ownership.Query) ([][]ownership.Claim, ownership.SourceHealth) {
	claims := make([][]ownership.Claim, len(queries))
	complete := true
	for i, query := range queries {
		for _, id := range query.Identifiers {
			if p.incomplete[id.Key()] {
				complete = false
			}
			artifact, ok := p.artifacts[id.Key()]
			if !ok {
				continue
			}
			claims[i] = append(claims[i], ownership.Claim{
				Source: p.Name(), Matched: id, RecordPresent: true, Artifact: artifact,
			})
		}
	}
	return claims, ownership.SourceHealth{Name: p.Name(), Complete: complete, EntryCount: len(p.artifacts)}
}

// bulkJob creates a durable job for a DOI without driving it through any
// state transitions, leaving it in the live "queued" state — exactly what
// canonicalJobStatus's "queued" branch should surface.
func bulkJob(t *testing.T, jobs *job.Store, reqID, doi string) string {
	t.Helper()
	id, err := jobs.CreateRequest(context.Background(), reqID, work.Work{DOI: doi}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// bulkUnavailableJob drives a fresh job straight to a terminal "unavailable"
// state, the fixture canonicalJobStatus's previously_unavailable branch reads.
func bulkUnavailableJob(t *testing.T, jobs *job.Store, reqID, doi string) string {
	t.Helper()
	ctx := context.Background()
	id := bulkJob(t, jobs, reqID, doi)
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateUnavailable, nil, job.WithTerminalReason(job.TerminalReasonUnknown)); err != nil {
		t.Fatal(err)
	}
	return id
}

// bulkReadyJob seeds a job driven to ready: papio's own validated bundle,
// the artifact-present claim canonicalJobStatus's owned branch reads — the
// only ownership source that exists under a zotio configuration.
func bulkReadyJob(t *testing.T, jobs *job.Store, reqID, doi string) string {
	t.Helper()
	ctx := context.Background()
	id := bulkJob(t, jobs, reqID, doi)
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPageBulkStatusMixedOutcomes exercises Decision 5's full status mapping
// end to end: durable job verdicts take precedence, positive holdings claims
// distinguish PDF-present from record-only evidence, a complete no-claim lookup
// is truly eligible, a partial no-claim lookup remains unknown, and an
// unrecognized DOI is invalid.
func TestPageBulkStatusMixedOutcomes(t *testing.T) {
	holdings := ownership.NewRegistry(bulkHoldingsProvider{
		artifacts: map[string]string{
			"doi:10.1000/owned-pdf.1":   ownership.ArtifactPresent,
			"doi:10.1000/record-only.1": ownership.ArtifactMissing,
		},
		incomplete: map[string]bool{"doi:10.1000/partial.1": true},
	})
	b, jobs, _, _ := newBridgeWithHoldings(t, holdings)
	runSync(t, b, hello())

	queuedID := bulkJob(t, jobs, "wr_bulk_queued", "10.1000/queued.1")
	bulkUnavailableJob(t, jobs, "wr_bulk_unavail", "10.1000/gone.1")
	readyID := bulkReadyJob(t, jobs, "wr_bulk_ready", "10.1000/papio-ready.1")

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-mixed001", ScanID: "scan-bulk-mixed0001",
		Identifiers: []protocol.PageBulkIdentifier{
			{LocalID: "row-1", Kind: "doi", Value: "10.1000/queued.1"},
			{LocalID: "row-2", Kind: "doi", Value: "10.1000/gone.1"},
			{LocalID: "row-3", Kind: "doi", Value: "10.1000/owned-pdf.1"},
			{LocalID: "row-4", Kind: "doi", Value: "10.1000/record-only.1"},
			{LocalID: "row-5", Kind: "doi", Value: "10.1000/eligible.1"},
			{LocalID: "row-6", Kind: "doi", Value: "10.1000/partial.1"},
			{LocalID: "row-7", Kind: "doi", Value: "not-a-doi"},
			{LocalID: "row-8", Kind: "doi", Value: "10.1000/papio-ready.1"},
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkStatusResultPayload)
	if payload.Truncated {
		t.Fatalf("truncated = true for a 7-item request, want false")
	}
	if len(payload.Items) != 8 {
		t.Fatalf("items = %d, want 8", len(payload.Items))
	}
	byLocalID := make(map[string]protocol.PageBulkStatusItem, 8)
	for _, item := range payload.Items {
		byLocalID[item.LocalID] = item
	}
	queued := byLocalID["row-1"]
	if queued.Status != "queued" || queued.JobID != queuedID || queued.CanonicalKey != "doi:10.1000/queued.1" || queued.OwnershipComplete {
		t.Fatalf("row-1 = %+v, want queued job %q without an ownership verdict", queued, queuedID)
	}
	unavailable := byLocalID["row-2"]
	if unavailable.Status != "previously_unavailable" || unavailable.CanonicalKey != "doi:10.1000/gone.1" || unavailable.JobID != "" || unavailable.OwnershipComplete {
		t.Fatalf("row-2 = %+v, want previously_unavailable without an ownership verdict", unavailable)
	}
	ownedPDF := byLocalID["row-3"]
	if ownedPDF.Status != "owned_with_pdf" || ownedPDF.CanonicalKey != "doi:10.1000/owned-pdf.1" || !ownedPDF.OwnershipComplete {
		t.Fatalf("row-3 = %+v, want owned_with_pdf with complete ownership", ownedPDF)
	}
	recordOnly := byLocalID["row-4"]
	if recordOnly.Status != "owned_missing_pdf" || recordOnly.CanonicalKey != "doi:10.1000/record-only.1" || !recordOnly.OwnershipComplete {
		t.Fatalf("row-4 = %+v, want owned_missing_pdf with complete ownership", recordOnly)
	}
	eligible := byLocalID["row-5"]
	if eligible.Status != "eligible" || eligible.CanonicalKey != "doi:10.1000/eligible.1" || !eligible.OwnershipComplete {
		t.Fatalf("row-5 = %+v, want eligible after a complete no-claim lookup", eligible)
	}
	incomplete := byLocalID["row-6"]
	if incomplete.Status != "ownership_incomplete" || incomplete.CanonicalKey != "doi:10.1000/partial.1" || incomplete.OwnershipComplete {
		t.Fatalf("row-6 = %+v, want ownership_incomplete after a partial no-claim lookup", incomplete)
	}
	invalid := byLocalID["row-7"]
	if invalid.Status != "invalid" || invalid.CanonicalKey != "" || invalid.JobID != "" || invalid.OwnershipComplete {
		t.Fatalf("row-7 = %+v, want invalid with no canonical_key", invalid)
	}
	papioReady := byLocalID["row-8"]
	if papioReady.Status != "owned_with_pdf" || papioReady.JobID != "" || !papioReady.OwnershipComplete {
		t.Fatalf("row-8 = %+v, want owned_with_pdf (job_id stays daemon-side) for ready bundle %q", papioReady, readyID)
	}
}

func TestPageBulkStatusNilHoldingsStaysIncomplete(t *testing.T) {
	var holdings *ownership.Registry
	b, _, _, _ := newBridgeWithHoldings(t, holdings)
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-nil00001", ScanID: "scan-bulk-nil000001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/unknown.1"}},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	item := result.Payload.(*protocol.PageBulkStatusResultPayload).Items[0]
	if item.Status != "ownership_incomplete" || item.OwnershipComplete {
		t.Fatalf("item = %+v with nil holdings, want ownership_incomplete and ownership_complete=false", item)
	}
}

// TestPageBulkStatusReadyBundleOwnsUnderNilHoldings pins the zotio scenario:
// the generic holdings registry is deliberately empty (ADR-0008 forbids
// mixing it with zotio), yet a work papio itself acquired must still render
// owned_with_pdf from the daemon's own ready bundle instead of drowning the
// workspace in "ownership unclear".
func TestPageBulkStatusReadyBundleOwnsUnderNilHoldings(t *testing.T) {
	var holdings *ownership.Registry
	b, jobs, _, _ := newBridgeWithHoldings(t, holdings)
	runSync(t, b, hello())
	readyID := bulkReadyJob(t, jobs, "wr_bulk_zotio_ready", "10.1000/zotio-owned.1")

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-zotio001", ScanID: "scan-bulk-zotio00001",
		Identifiers: []protocol.PageBulkIdentifier{
			{LocalID: "row-1", Kind: "doi", Value: "10.1000/zotio-owned.1"},
			{LocalID: "row-2", Kind: "doi", Value: "10.1000/never-seen.1"},
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkStatusResultPayload)
	owned := payload.Items[0]
	if owned.Status != "owned_with_pdf" || owned.JobID != "" || !owned.OwnershipComplete {
		t.Fatalf("items[0] = %+v, want owned_with_pdf (job_id daemon-side) for ready job %q despite nil holdings", owned, readyID)
	}
	if unknown := payload.Items[1]; unknown.Status != "ownership_incomplete" || unknown.OwnershipComplete {
		t.Fatalf("items[1] = %+v, want ownership_incomplete for a never-seen work under nil holdings", unknown)
	}
}

// TestPageBulkStatusOwnershipLookupFailureStaysIncomplete pins ADR-0008
// invariant 2 for the identity-lookup path canonicalJobStatus owns: a failed
// store read must surface as ownership_incomplete, never a negative "not
// owned" fact. The failure is forced on a second, non-holder session so the
// holder-only poll (which independently reads the jobs/identifiers join for
// open handoffs) never runs against the broken schema.
func TestPageBulkStatusOwnershipLookupFailureStaysIncomplete(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	if _, err := jobs.S.DB().ExecContext(ctx, "DROP TABLE identifiers"); err != nil {
		t.Fatal(err)
	}
	const pendingSession = "sess-pending-00000000000000000000000"
	runSyncAs(t, b, pendingSession, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-fail0001", ScanID: "scan-bulk-fail00001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/example.1"}},
	})
	msgs, _ := runSyncAs(t, b, pendingSession, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkStatusResultPayload)
	if len(payload.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if item.Status != "ownership_incomplete" || item.OwnershipComplete {
		t.Fatalf("item = %+v after a broken identity store, want ownership_incomplete and ownership_complete=false", item)
	}
	if item.CanonicalKey != "doi:10.1000/example.1" {
		t.Fatalf("canonical_key = %q, want the normalized identity even when the lookup failed", item.CanonicalKey)
	}
}

// fakeZotioCLI is a minimal zotio.CLI double for page-bulk merge tests: it
// answers `--agent items find --<kind> <value>` exactly like the real CLI's
// JSON output, and MissingPDF names which of those keys still lack a PDF.
type fakeZotioCLI struct {
	find      map[string]json.RawMessage
	findErr   error
	missing   []zotio.MissingPDFItem
	syncErr   error
	syncCalls int
}

func (f *fakeZotioCLI) Preflight(context.Context) (*zotio.PreflightResult, error) {
	return &zotio.PreflightResult{Executable: "zotio", Version: "1.0.0"}, nil
}
func (f *fakeZotioCLI) MissingPDF(context.Context, string, int) ([]zotio.MissingPDFItem, error) {
	return append([]zotio.MissingPDFItem(nil), f.missing...), nil
}
func (f *fakeZotioCLI) GetItem(context.Context, string) (*zotio.Item, error) {
	return nil, fmt.Errorf("fakeZotioCLI: item not found")
}
func (f *fakeZotioCLI) Sync(context.Context) error {
	f.syncCalls++
	return f.syncErr
}
func (f *fakeZotioCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if len(args) >= 5 && strings.Join(args[:3], " ") == "--agent items find" {
		key := strings.TrimPrefix(args[3], "--") + ":" + args[4]
		if raw := f.find[key]; raw != nil {
			return raw, nil
		}
		return json.RawMessage("[]"), nil
	}
	return nil, fmt.Errorf("fakeZotioCLI: unexpected RunJSON %q", args)
}

// TestPageBulkStatusZotioOwnedWithPDFHasNoPapioJob pins the follow-up's
// headline fix: a work that lives only in the user's Zotero library — papio
// itself never acquired it, so canonicalJobStatus has nothing — must still
// render owned_with_pdf, not ownership_incomplete.
func TestPageBulkStatusZotioOwnedWithPDFHasNoPapioJob(t *testing.T) {
	cli := &fakeZotioCLI{find: map[string]json.RawMessage{
		"doi:10.1000/zotio-lib.1": json.RawMessage(`[{"key":"ZOT00001","data":{}}]`),
	}}
	b, _, _, _ := newBridgeWithHoldingsAndZotio(t, nil, &zotio.Service{CLI: cli})
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-zowned01", ScanID: "scan-bulk-zowned0001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/zotio-lib.1"}},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	item := result.Payload.(*protocol.PageBulkStatusResultPayload).Items[0]
	if item.Status != "owned_with_pdf" || item.JobID != "" || !item.OwnershipComplete {
		t.Fatalf("item = %+v, want owned_with_pdf from zotio with no papio job", item)
	}
}

// TestPageBulkStatusZotioOwnedMissingPDFCarriesItemKey pins the
// owned_missing_pdf branch: zotio found the Zotero parent item but it has no
// PDF attached, so the status must name the item key for a direct handoff.
func TestPageBulkStatusZotioOwnedMissingPDFCarriesItemKey(t *testing.T) {
	cli := &fakeZotioCLI{
		find: map[string]json.RawMessage{
			"doi:10.1000/zotio-missing.1": json.RawMessage(`[{"key":"ZOT00002","data":{}}]`),
		},
		missing: []zotio.MissingPDFItem{{Key: "ZOT00002"}},
	}
	b, _, _, _ := newBridgeWithHoldingsAndZotio(t, nil, &zotio.Service{CLI: cli})
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-zmiss001", ScanID: "scan-bulk-zmiss00001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/zotio-missing.1"}},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	item := result.Payload.(*protocol.PageBulkStatusResultPayload).Items[0]
	if item.Status != "owned_missing_pdf" || item.ZotioItemKey != "ZOT00002" || !item.OwnershipComplete {
		t.Fatalf("item = %+v, want owned_missing_pdf carrying zotio_item_key ZOT00002", item)
	}
}

// TestPageBulkStatusZotioStalenessYieldsOwnershipUnknown pins ADR-0008
// invariant 2 for the zotio path specifically: a degraded zotio reading must
// never let a "no match" collapse into a false "eligible" claim. Page-bulk
// lookups run LocalOnly (no mirror sync — the workspace privacy line
// promises a purely local check), so degradation here means the local
// lookup itself failing, and the sync-call assertion pins that no sync is
// ever attempted from this path.
func TestPageBulkStatusZotioStalenessYieldsOwnershipUnknown(t *testing.T) {
	cli := &fakeZotioCLI{
		findErr: fmt.Errorf("zotio mirror unreadable"),
		syncErr: fmt.Errorf("sync must never run from page-bulk"),
	}
	b, _, _, _ := newBridgeWithHoldingsAndZotio(t, nil, &zotio.Service{CLI: cli})
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-zstale01", ScanID: "scan-bulk-zstale0001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/never-checked.1"}},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	item := result.Payload.(*protocol.PageBulkStatusResultPayload).Items[0]
	if item.Status != "ownership_unknown" || item.OwnershipComplete {
		t.Fatalf("item = %+v after a failed zotio lookup, want ownership_unknown (never a plain unowned/eligible claim)", item)
	}
	if cli.syncCalls != 0 {
		t.Fatalf("sync calls = %d, want 0 (page-bulk lookups are LocalOnly)", cli.syncCalls)
	}
}

// TestPageBulkStatusNilZotioMatchesPriorBehavior pins the byte-identical
// degrade: a Bridge built without a zotio service must classify exactly as
// it did before ownership was wired in — driven here by the generic holdings
// registry alone, with no zotio consulted at all.
func TestPageBulkStatusNilZotioMatchesPriorBehavior(t *testing.T) {
	holdings := ownership.NewRegistry(bulkHoldingsProvider{
		artifacts: map[string]string{"doi:10.1000/nil-zotio-owned.1": ownership.ArtifactPresent},
	})
	b, _, _, _ := newBridgeWithHoldingsAndZotio(t, holdings, nil)
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-nilz0001", ScanID: "scan-bulk-nilz00001",
		Identifiers: []protocol.PageBulkIdentifier{
			{LocalID: "row-1", Kind: "doi", Value: "10.1000/nil-zotio-owned.1"},
			{LocalID: "row-2", Kind: "doi", Value: "10.1000/nil-zotio-eligible.1"},
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	items := result.Payload.(*protocol.PageBulkStatusResultPayload).Items
	if owned := items[0]; owned.Status != "owned_with_pdf" || owned.ZotioItemKey != "" || !owned.OwnershipComplete {
		t.Fatalf("items[0] = %+v, want owned_with_pdf from generic holdings, unaffected by the absent zotio service", owned)
	}
	if eligible := items[1]; eligible.Status != "eligible" || !eligible.OwnershipComplete {
		t.Fatalf("items[1] = %+v, want eligible from a complete no-claim holdings lookup, unaffected by the absent zotio service", eligible)
	}
}

// bulkOpenAlexReadyJob mirrors bulkReadyJob but keys the durable job on an
// OpenAlex work identity instead of a DOI — canonicalJobStatus's ready
// branch must key correctly off an "openalex" identifiers row exactly as it
// does for "doi" (createRequest writes both from work.Work the same way;
// internal/job/job.go's kind loop).
func bulkOpenAlexReadyJob(t *testing.T, jobs *job.Store, reqID, openalexID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, work.Work{OpenAlex: openalexID}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPageBulkStatusOpenAlexWOnlyRowSkipsZotioAndFollowsLedger pins the
// zotio-lookup chunk builder's OpenAlex exclusion end to end: zotio.LookupWork
// carries only DOI/ArXiv/PMID fields, so an openalex-only row never reaches
// zotio at all (pageBulkZotioLookup's per-identifier switch falls through its
// `default: continue`, and pageBulkStatusItem's default ownership branch never
// calls the generic holdings registry for it either). Ownership completeness
// therefore follows papio's own ledger exclusively: a ready bundle still
// answers owned_with_pdf keyed on the openalex canonical identity
// ("openalex:W…", work.Describe's form), while a row with no ledger state at
// all gets the ordinary not-fully-checked ownership_incomplete presentation —
// never a false eligible-and-complete claim, and never ownership_unknown
// (which would mean zotio was wrongly consulted and came back stale).
func TestPageBulkStatusOpenAlexWOnlyRowSkipsZotioAndFollowsLedger(t *testing.T) {
	// findErr forces every zotio CLI call to fail loudly. If either the
	// chunk builder or the default ownership branch were ever wrongly
	// extended to consult zotio for kind "openalex", this round would
	// surface ownership_unknown (the stale-zotio branch) for row-2 instead
	// of the ownership_incomplete asserted below.
	cli := &fakeZotioCLI{findErr: fmt.Errorf("zotio must never be queried for an openalex-only row")}
	b, jobs, _, _ := newBridgeWithHoldingsAndZotio(t, nil, &zotio.Service{CLI: cli})
	runSync(t, b, hello())

	readyID := bulkOpenAlexReadyJob(t, jobs, "wr_bulk_openalex_ready", "W1976043798")

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-oax0001", ScanID: "scan-bulk-oax00001",
		Identifiers: []protocol.PageBulkIdentifier{
			{LocalID: "row-1", Kind: "openalex", Value: "w1976043798"},
			{LocalID: "row-2", Kind: "openalex", Value: "W2741809807"},
			{LocalID: "row-3", Kind: "openalex", Value: "not-a-work-id"},
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkStatusResult)
	if result == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}
	items := result.Payload.(*protocol.PageBulkStatusResultPayload).Items

	ready := items[0]
	if ready.Status != "owned_with_pdf" || ready.JobID != "" || !ready.OwnershipComplete || ready.CanonicalKey != "openalex:W1976043798" {
		t.Fatalf("items[0] = %+v, want owned_with_pdf (job_id daemon-side) for ready job %q keyed on the openalex canonical identity", ready, readyID)
	}
	unchecked := items[1]
	if unchecked.Status != "ownership_incomplete" || unchecked.OwnershipComplete || unchecked.ZotioItemKey != "" {
		t.Fatalf("items[1] = %+v, want the ordinary not-fully-checked ownership_incomplete presentation, never eligible/owned/unknown", unchecked)
	}
	if invalid := items[2]; invalid.Status != "invalid" || invalid.CanonicalKey != "" {
		t.Fatalf("items[2] = %+v, want invalid for a malformed OpenAlex work id with no canonical_key", invalid)
	}
	if cli.syncCalls != 0 {
		t.Fatalf("sync calls = %d, want 0 — an openalex-only row must never reach zotio at all", cli.syncCalls)
	}
}

// TestPageBulkSubmitCreatesJobsWithBrowserPageConsumer exercises Decision 5/7
// end to end: a live key joins, a fresh key creates a browser-page job, a fresh
// PDF-present holdings claim is skipped server-side as already_owned, and a key
// that no longer decodes counts as invalid.
func TestPageBulkSubmitCreatesJobsWithBrowserPageConsumer(t *testing.T) {
	holdings := ownership.NewRegistry(bulkHoldingsProvider{artifacts: map[string]string{
		"doi:10.1000/owned.8": ownership.ArtifactPresent,
	}})
	b, jobs, _, _ := newBridgeWithHoldings(t, holdings)
	ctx := context.Background()
	runSync(t, b, hello())
	existingID := bulkJob(t, jobs, "wr_bulk_existing", "10.1000/existing.1")
	bulkReadyJob(t, jobs, "wr_bulk_ready_own", "10.1000/papio-owned.9")

	frame := inFrame(t, protocol.MsgPageBulkSubmitRequest, "", protocol.PageBulkSubmitRequestPayload{
		RequestID: "request-bulk-submit002", ScanID: "scan-bulk-submit0002",
		CanonicalKeys: []string{"doi:10.1000/existing.1", "doi:10.1000/fresh.7", "doi:10.1000/owned.8", "doi:10.1000/papio-owned.9", "not-a-canonical-key"},
		Source: protocol.PageBulkSubmitSource{
			Kind: "browser_page", Origin: "https://scholar.example.edu", Detector: "generic-identifiers/1",
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkSubmitResult)
	if result == nil {
		t.Fatalf("no page_bulk_submit_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkSubmitResultPayload)
	if payload.Submitted != 1 || payload.Joined != 1 || payload.Invalid != 1 || payload.AlreadyOwned != 2 {
		t.Fatalf("counts = %+v, want {submitted:1 joined:1 already_owned:2 invalid:1}: papio's own ready bundle suppresses like a holdings claim", payload)
	}
	if payload.BatchID == "" {
		t.Fatal("batch_id is empty")
	}

	rows, err := jobs.S.DB().QueryContext(ctx, `
		SELECT j.id, COALESCE(j.consumer,''),
		       COALESCE((SELECT value FROM identifiers WHERE work_request_id = j.work_request_id AND kind = 'doi'), '')
		FROM jobs j`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	byID := map[string]struct {
		consumer, doi string
	}{}
	for rows.Next() {
		var id, consumer, doi string
		if err := rows.Scan(&id, &consumer, &doi); err != nil {
			t.Fatal(err)
		}
		byID[id] = struct{ consumer, doi string }{consumer, doi}
	}
	if len(byID) != 3 {
		t.Fatalf("jobs after submit = %d, want 3 (joined, papio-owned seed, one new): a ready-owned key must not create a fourth", len(byID))
	}
	if existing, ok := byID[existingID]; !ok || existing.consumer != "" {
		t.Fatalf("existing job %+v, want unattributed consumer (a join must not overwrite it)", byID[existingID])
	}
	var created *struct{ consumer, doi string }
	for id, row := range byID {
		if row.doi == "10.1000/fresh.7" {
			created = &row
		}
		_ = id
	}
	if created == nil || created.consumer != pageBulkConsumer || created.doi != "10.1000/fresh.7" {
		t.Fatalf("new job = %+v, want consumer %q and doi 10.1000/fresh.7", created, pageBulkConsumer)
	}

	var selected, submitted, invalid int
	var detectorID, sourceOrigin, batchID, openedAt, submittedAt string
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT detector_id, source_origin, selected, submitted, invalid, batch_id, opened_at, submitted_at FROM page_bulk_runs`,
	).Scan(&detectorID, &sourceOrigin, &selected, &submitted, &invalid, &batchID, &openedAt, &submittedAt); err != nil {
		t.Fatalf("page_bulk_runs row: %v", err)
	}
	if detectorID != "generic-identifiers/1" || sourceOrigin != "https://scholar.example.edu" {
		t.Fatalf("run source = %q/%q, want detector/origin from the submit request", detectorID, sourceOrigin)
	}
	if selected != 5 || submitted != 1 || invalid != 1 {
		t.Fatalf("run counts = selected:%d submitted:%d invalid:%d, want 5/1/1", selected, submitted, invalid)
	}
	if batchID != payload.BatchID {
		t.Fatalf("run batch_id = %q, want %q", batchID, payload.BatchID)
	}
	if openedAt == "" || submittedAt == "" {
		t.Fatal("run opened_at/submitted_at must be populated")
	}
}

// TestPageBulkSubmitOpenAlexCanonicalKeyCreatesJob pins the P1 fix to
// pageBulkWorkRequest/pageBulkIdentifierOf: before the fix, an "openalex:W…"
// canonical key decoded to a WorkRequest with a zero-value Identifiers (no
// case populated it), which app-side validation silently rejected — every
// OpenAlex submission counted invalid and no job was ever created. A fresh
// key must create exactly one job keyed on the openalex identity, and a key
// naming a work papio already holds ready must dedupe as already_owned
// (pageBulkIdentifierOf's ready-bundle short-circuit) rather than creating a
// second job — the same semantics doi/arxiv/pmid keys already get.
func TestPageBulkSubmitOpenAlexCanonicalKeyCreatesJob(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	bulkOpenAlexReadyJob(t, jobs, "wr_bulk_openalex_owned", "W2741809807")

	frame := inFrame(t, protocol.MsgPageBulkSubmitRequest, "", protocol.PageBulkSubmitRequestPayload{
		RequestID: "request-bulk-submit003", ScanID: "scan-bulk-submit0003",
		CanonicalKeys: []string{"openalex:W1976043798", "openalex:W2741809807", "openalex:not-a-work-id"},
		Source: protocol.PageBulkSubmitSource{
			Kind: "browser_page", Origin: "https://scholar.example.edu", Detector: "generic-identifiers/1",
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkSubmitResult)
	if result == nil {
		t.Fatalf("no page_bulk_submit_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkSubmitResultPayload)
	if payload.Submitted != 1 || payload.AlreadyOwned != 1 || payload.Invalid != 1 || payload.Joined != 0 {
		t.Fatalf("counts = %+v, want {submitted:1 already_owned:1 invalid:1}: a fresh W-id creates a job, a ready W-work dedupes, a malformed one is invalid", payload)
	}

	var openalexJobID, doi string
	if err := jobs.S.DB().QueryRowContext(ctx, `
		SELECT j.id, COALESCE((SELECT value FROM identifiers WHERE work_request_id = j.work_request_id AND kind = 'doi'), '')
		FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = 'openalex' AND i.value = 'W1976043798'`,
	).Scan(&openalexJobID, &doi); err != nil {
		t.Fatalf("no job created for the fresh openalex key: %v", err)
	}
	if openalexJobID == "" {
		t.Fatal("openalex job id is empty")
	}

	var jobCount int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 2 {
		t.Fatalf("jobs after submit = %d, want 2 (the seeded ready job plus one new): the already-owned key must not create a third", jobCount)
	}
}

// TestPageBulkSubmitCanonicalKeysCapRejectedByDecode pins that the 50-key cap
// is enforced by protocol decode (ADR-0019 Decision 5), not the handler: a
// 51-key frame never reaches pageBulkSubmit and fails the whole Sync call
// closed, exactly like any other malformed inbound frame.
func TestPageBulkSubmitCanonicalKeysCapRejectedByDecode(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())

	keys := make([]string, 51)
	for i := range keys {
		keys[i] = fmt.Sprintf("doi:10.1000/example.%d", i)
	}
	frame := inFrame(t, protocol.MsgPageBulkSubmitRequest, "", protocol.PageBulkSubmitRequestPayload{
		RequestID: "request-bulk-toomany01", ScanID: "scan-bulk-toomany001",
		CanonicalKeys: keys,
		Source: protocol.PageBulkSubmitSource{
			Kind: "browser_page", Origin: "https://scholar.example.edu", Detector: "generic-identifiers/1",
		},
	})
	_, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{frame})
	if !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("sync error = %v, want ErrInvalidFrame for a 51-key submit", err)
	}
}

// TestPageBulkSubmitEchoesUnknownScanID pins that scan_id is opaque
// correlation only: the daemon keeps no scan-side state, so a submit whose
// scan_id was never seen in a prior status_request (a stale sheet, a
// restarted daemon, or simply a caller that skipped status) still succeeds
// and echoes the same id back.
func TestPageBulkSubmitEchoesUnknownScanID(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkSubmitRequest, "", protocol.PageBulkSubmitRequestPayload{
		RequestID: "request-bulk-stale0001", ScanID: "scan-never-seen-before1",
		CanonicalKeys: []string{"doi:10.1000/stale.99"},
		Source: protocol.PageBulkSubmitSource{
			Kind: "browser_page", Origin: "https://scholar.example.edu", Detector: "generic-identifiers/1",
		},
	})
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkSubmitResult)
	if result == nil {
		t.Fatalf("no page_bulk_submit_result in %v", msgs)
	}
	payload := result.Payload.(*protocol.PageBulkSubmitResultPayload)
	if payload.ScanID != "scan-never-seen-before1" {
		t.Fatalf("scan_id = %q, want the unknown id echoed back unchanged", payload.ScanID)
	}
	if payload.Submitted != 1 || payload.Invalid != 0 {
		t.Fatalf("counts = %+v, want a clean submit despite the unknown scan_id", payload)
	}
}

// TestPageBulkStatusRecordsScanRowWithRenderedHint pins the honest-denominator
// follow-up (dev/post-build-followups.md item 3): a status call whose request
// carries rendered_record_count_hint persists a page_bulk_runs row with that
// hint, the request's detected_raw count, and the distinct canonical-key
// count — all previously left at the schema's zero default because only
// pageBulkSubmit ever wrote a row. batch_id stays empty: this is a scan row,
// not a submit row (see recordPageBulkScan's doc comment).
func TestPageBulkStatusRecordsScanRowWithRenderedHint(t *testing.T) {
	holdings := ownership.NewRegistry(bulkHoldingsProvider{
		artifacts: map[string]string{"doi:10.1000/hinted-owned.1": ownership.ArtifactPresent},
	})
	b, jobs, _, _ := newBridgeWithHoldings(t, holdings)
	ctx := context.Background()
	runSync(t, b, hello())

	hint := int64(12)
	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-hint0001", ScanID: "scan-bulk-hint0001",
		Identifiers: []protocol.PageBulkIdentifier{
			{LocalID: "row-1", Kind: "doi", Value: "10.1000/hinted-owned.1"},
			{LocalID: "row-2", Kind: "doi", Value: "10.1000/hinted-eligible.1"},
		},
		RenderedRecordCountHint: &hint,
	})
	msgs, _ := runSync(t, b, frame)
	if firstOfType(msgs, protocol.MsgPageBulkStatusResult) == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}

	var detectorID, sourceOrigin, batchID string
	var detectedRaw, canonicalUnique int
	var renderedHint sql.NullInt64
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT detector_id, source_origin, detected_raw, canonical_unique, batch_id, rendered_record_count_hint FROM page_bulk_runs`,
	).Scan(&detectorID, &sourceOrigin, &detectedRaw, &canonicalUnique, &batchID, &renderedHint); err != nil {
		t.Fatalf("page_bulk_runs scan row: %v", err)
	}
	if detectorID != "" || sourceOrigin != "" {
		t.Fatalf("scan row detector/origin = %q/%q, want empty — page_bulk_status_request carries neither", detectorID, sourceOrigin)
	}
	if batchID != "" {
		t.Fatalf("scan row batch_id = %q, want empty (a scan row, not a submit row)", batchID)
	}
	if detectedRaw != 2 || canonicalUnique != 2 {
		t.Fatalf("detected_raw/canonical_unique = %d/%d, want 2/2", detectedRaw, canonicalUnique)
	}
	if !renderedHint.Valid || renderedHint.Int64 != 12 {
		t.Fatalf("rendered_record_count_hint = %+v, want a valid 12", renderedHint)
	}
}

// TestPageBulkStatusWithoutHintRecordsNullDenominator pins the "never a
// guess" half of the same follow-up: an unrecognized page sends no hint, and
// the recorded row must carry a NULL denominator, never a fabricated one
// (e.g. 0, which would silently look like "zero rendered records").
func TestPageBulkStatusWithoutHintRecordsNullDenominator(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())

	frame := inFrame(t, protocol.MsgPageBulkStatusRequest, "", protocol.PageBulkStatusRequestPayload{
		RequestID: "request-bulk-nohint01", ScanID: "scan-bulk-nohint0001",
		Identifiers: []protocol.PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/no-hint.1"}},
	})
	msgs, _ := runSync(t, b, frame)
	if firstOfType(msgs, protocol.MsgPageBulkStatusResult) == nil {
		t.Fatalf("no page_bulk_status_result in %v", msgs)
	}

	var renderedHint sql.NullInt64
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT rendered_record_count_hint FROM page_bulk_runs`,
	).Scan(&renderedHint); err != nil {
		t.Fatalf("page_bulk_runs scan row: %v", err)
	}
	if renderedHint.Valid {
		t.Fatalf("rendered_record_count_hint = %v, want NULL when the request carried no hint", renderedHint.Int64)
	}
}

// TestSweepsSkipTickOnHungAdoptionRoot pins that both sweeper passes route the
// adoption-root listing through the bounded, latch-aware seam. Before this,
// SweepAdoptions and SweepTerminalAdoptions called os.ReadDir directly, so a
// TCC-hung root wedged the sweeper goroutine forever — retroactive adoption
// and landing-directory cleanup silently stopped until a daemon restart.
func TestSweepsSkipTickOnHungAdoptionRoot(t *testing.T) {
	b, _, _, _ := newBridge(t)
	compressAdoptionScanDeadline(t)
	ctx := context.Background()

	var calls int32
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	b.readDir = func(string) ([]os.DirEntry, error) {
		atomic.AddInt32(&calls, 1)
		<-block
		return nil, errors.New("unreachable in this test")
	}

	done := make(chan error, 2)
	go func() { done <- b.SweepAdoptions(ctx) }()
	go func() { done <- b.SweepTerminalAdoptions(ctx) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("sweep during hung root: %v, want nil (skip tick)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("sweep wedged on a hung adoption root instead of skipping the tick")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("readDir calls = %d, want 1 (latch must prevent stacking a second hung goroutine)", got)
	}
}

// TestSweepTerminalAdoptionsRemovesEmptyUnknownDirs pins the empty-stray rule:
// a directory not matching any known job is removed only when empty (rmdir
// semantics — atomically refused if a file lands concurrently), while an
// unknown directory holding a file is preserved for a human.
func TestSweepTerminalAdoptionsRemovesEmptyUnknownDirs(t *testing.T) {
	b, _, _, _ := newBridge(t)
	ctx := context.Background()
	root := b.cfg.EffectiveAdoptionRoot()

	empty := filepath.Join(root, "job_unknown_empty_stray_000000")
	full := filepath.Join(root, "job_unknown_with_contents_0000")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "paper.pdf"), []byte("%PDF-"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.SweepTerminalAdoptions(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty unknown dir survived the sweep: err=%v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(full, "paper.pdf")); err != nil {
		t.Fatalf("unknown dir with contents must be preserved for a human: %v", err)
	}
}

// TestPdfGrabAllocatesSteeringPath pins the happy path of ADR-0020's
// synchronous allocation reply: a grab id, a papio/grabs/<id>/ steering
// path, and a landing directory actually created under the adoption root.
func TestPdfGrabAllocatesSteeringPath(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": "grab-req-0001", "host": "pdf.example.org", "title": "A Paper",
	}))
	got := firstOfType(msgs, protocol.MsgPdfGrabResult)
	if got == nil {
		t.Fatalf("no pdf_grab_result frame: %+v", msgs)
	}
	p := got.Payload.(*protocol.PdfGrabResultPayload)
	if p.Outcome != "steering" || p.GrabID == "" {
		t.Fatalf("payload = %+v, want steering outcome with a grab id", p)
	}
	wantPrefix := "papio/grabs/" + p.GrabID + "/"
	if p.SteeringPath != wantPrefix {
		t.Fatalf("steering_path = %q, want %q", p.SteeringPath, wantPrefix)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", p.GrabID)); err != nil {
		t.Fatalf("landing directory not created: %v", err)
	}
	var jobID sql.NullString
	var attempt int64
	var holder int64
	var domain, kind, status, grabID string
	if err := jobs.S.DB().QueryRowContext(context.Background(), `
		SELECT job_id, job_attempt_revision, browser_holder_generation, safety_domain_id,
		       effect_kind, status, grab_id
		FROM effect_permits WHERE effect_kind = 'pdf_grab' AND grab_id = ?`, p.GrabID).
		Scan(&jobID, &attempt, &holder, &domain, &kind, &status, &grabID); err != nil {
		t.Fatalf("pdf grab permit lookup: %v", err)
	}
	if jobID.Valid || attempt != 0 || holder != b.epoch ||
		domain != "pdf_grab:pdf.example.org" || kind != "pdf_grab" ||
		status != "held" || grabID != p.GrabID {
		t.Fatalf("pdf grab permit = job_id=%v attempt=%d holder=%d domain=%q kind=%q status=%q grab_id=%q",
			jobID, attempt, holder, domain, kind, status, grabID)
	}
	var authEvents int
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE job_id IS NULL AND kind = 'browser.pdf_grab_started'`).Scan(&authEvents); err != nil {
		t.Fatalf("authorization event lookup: %v", err)
	}
	if authEvents != 1 {
		t.Fatalf("pdf grab authorization events = %d, want one", authEvents)
	}
}

// A capture whose permit is already settled must be cancellable by the browser
// even though it cannot present the originating request id — that identity dies
// with the worker generation that armed the capture, and session-scoped
// correlations do not survive an extension reload. Allocation is idempotent per
// host, so refusing here left every later Send PDF for that tab answered
// "existing" for a capture nobody could complete, until AbandonStaleAwaiting's
// six-hour cutoff. Nothing is in flight once the permit is settled, so there is
// no occupancy to protect.
func TestPdfGrabAbandonSessionClearsSettledCapture(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": "grab-req-settled-1", "host": "pdf.example.org", "title": "A Paper",
	}))
	allocated := firstOfType(msgs, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)

	// A fresh request id is refused while the capture still occupies its permit:
	// that is the fence, and it stays.
	refused, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabAbandonRequest, "", map[string]any{
		"request_id": "grab-abandon-settled-1", "grab_id": allocated.GrabID,
	}))
	held := firstOfType(refused, protocol.MsgPdfGrabAbandonResult).Payload.(*protocol.PdfGrabAbandonResultPayload)
	if held.Outcome != "conflict" {
		t.Fatalf("outcome = %q with a held permit, want conflict", held.Outcome)
	}

	if _, err := jobs.S.DB().Exec(`UPDATE effect_permits SET status='settled' WHERE grab_id=?`, allocated.GrabID); err != nil {
		t.Fatalf("settle permit: %v", err)
	}
	cleared, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabAbandonRequest, "", map[string]any{
		"request_id": "grab-abandon-settled-2", "grab_id": allocated.GrabID,
	}))
	done := firstOfType(cleared, protocol.MsgPdfGrabAbandonResult).Payload.(*protocol.PdfGrabAbandonResultPayload)
	if done.Outcome != "abandoned" || done.State != string(grab.StateAbandoned) {
		t.Fatalf("payload = %+v, want abandoned", done)
	}

	// And the tab is usable again: allocation no longer answers "existing".
	again, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": "grab-req-settled-2", "host": "pdf.example.org", "title": "A Paper",
	}))
	fresh := firstOfType(again, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)
	if fresh.Outcome != "steering" {
		t.Fatalf("outcome = %q after clearing, want steering", fresh.Outcome)
	}
}
func TestPdfGrabRequiresPermitAndRespectsOccupancy(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature))
		msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
			"request_id": "grab-req-no-permit", "host": "pdf.example.org", "title": "No permit",
		}))
		p := firstOfType(msgs, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)
		if p.Outcome != "unavailable" || p.GrabID != "" || p.SteeringPath != "" {
			t.Fatalf("unsupported payload = %+v, want unavailable without steering", p)
		}
		var grabs, permits int
		if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pdf_grabs`).Scan(&grabs); err != nil {
			t.Fatal(err)
		}
		if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM effect_permits`).Scan(&permits); err != nil {
			t.Fatal(err)
		}
		if grabs != 0 || permits != 0 {
			t.Fatalf("unsupported allocation rows = grabs %d permits %d, want 0/0", grabs, permits)
		}
		if entries, err := os.ReadDir(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs")); err == nil && len(entries) != 0 {
			t.Fatalf("unsupported allocation created landing dirs: %d", len(entries))
		}
	})
	t.Run("occupied lane and existing grab", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
		first, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
			"request_id": "grab-req-occupied-1", "host": "pdf.example.org", "title": "First",
		}))
		firstPayload := firstOfType(first, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)
		if firstPayload.Outcome != "steering" {
			t.Fatalf("first payload = %+v, want steering", firstPayload)
		}
		occupied, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
			"request_id": "grab-req-occupied-2", "host": "other.example.org", "title": "Second",
		}))
		occupiedPayload := firstOfType(occupied, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)
		if occupiedPayload.Outcome != "unavailable" || occupiedPayload.GrabID != "" || occupiedPayload.SteeringPath != "" {
			t.Fatalf("occupied payload = %+v, want unavailable without steering", occupiedPayload)
		}
		existing, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
			"request_id": "grab-req-existing", "host": "pdf.example.org", "title": "First",
		}))
		existingPayload := firstOfType(existing, protocol.MsgPdfGrabResult).Payload.(*protocol.PdfGrabResultPayload)
		if existingPayload.Outcome != "existing" || existingPayload.GrabID != firstPayload.GrabID || existingPayload.SteeringPath != "" {
			t.Fatalf("existing payload = %+v, want existing without second steering", existingPayload)
		}
		var grabs, permits int
		if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pdf_grabs`).Scan(&grabs); err != nil {
			t.Fatal(err)
		}
		if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM effect_permits`).Scan(&permits); err != nil {
			t.Fatal(err)
		}
		if grabs != 1 || permits != 1 {
			t.Fatalf("occupied/existing rows = grabs %d permits %d, want 1/1", grabs, permits)
		}
		if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", firstPayload.GrabID)); err != nil {
			t.Fatalf("first landing directory missing: %v", err)
		}
	})
}

// TestPdfGrabRefusesOnUnhealthyLatch pins ADR-0020's fail-closed refusal: a
// missing-capability outcome, structured (never a raw Go error), and no
// grab is allocated (no landing directory left behind).
func TestPdfGrabRefusesOnUnhealthyLatch(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
	b.adoptionScanSuspended = true
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": "grab-req-0002", "host": "pdf.example.org",
	}))
	got := firstOfType(msgs, protocol.MsgPdfGrabResult)
	if got == nil {
		t.Fatalf("no pdf_grab_result frame: %+v", msgs)
	}
	p := got.Payload.(*protocol.PdfGrabResultPayload)
	if p.Outcome != "unavailable" || p.Reason != grabReasonAdoptionUnhealthy ||
		p.GrabID != "" || p.SteeringPath != "" || p.Detail == "" {
		t.Fatalf("payload = %+v, want a structured adoption_unhealthy refusal with no grab_id", p)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("refusal must not allocate a grab: found %d landing dirs", len(entries))
	}
}

// pdfGrabVia drives one pdf_grab_request from sessionID through Sync — the
// production path, dispatcher included — and returns the decoded result.
func pdfGrabVia(t *testing.T, b *Bridge, sessionID, requestID, host string) *protocol.PdfGrabResultPayload {
	t.Helper()
	msgs, _ := runSyncAs(t, b, sessionID, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": requestID, "host": host, "title": "A Paper",
	}))
	result := firstOfType(msgs, protocol.MsgPdfGrabResult)
	if result == nil {
		t.Fatalf("no pdf_grab_result frame: %+v", msgs)
	}
	return result.Payload.(*protocol.PdfGrabResultPayload)
}

// pdfGrabDirect calls the handler with no dispatcher in front of it. Two
// refusal branches are only reachable this way, and both must stay: Sync
// answers an unknown session with hello_required before the grab path sees
// it, and the wire's bare-hostname rule rejects an unusable host at decode.
// Calling the handler directly also proves the branch answers with a frame
// rather than a raw Go error — which the native host treats as a dead
// connection and tears the browser session down for.
func pdfGrabDirect(t *testing.T, b *Bridge, sessionID, requestID, host string) *protocol.PdfGrabResultPayload {
	t.Helper()
	frames, err := b.pdfGrab(context.Background(), sessionID, &protocol.PdfGrabRequestPayload{
		RequestID: requestID, Host: host, Title: "A Paper",
	})
	if err != nil {
		t.Fatalf("pdf grab returned a raw Go error, which disconnects the browser: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want one pdf_grab_result", len(frames))
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatalf("refusal frame failed protocol decode: %v", err)
	}
	return msg.Payload.(*protocol.PdfGrabResultPayload)
}

func grabCapableHello(t *testing.T, b *Bridge) {
	t.Helper()
	runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
}

// TestPdfGrabIsServedFromANonHolderSession is the regression test for the
// field report behind this change: clicking "Send PDF" in a second browser
// refused with an internal-vocabulary message. A grab is user-initiated and
// carries its own routing — the requesting session gets its own steering
// path and downloads into it — so holdership, which exists to route
// daemon-initiated offers and handoffs, must not gate it. The grab must also
// not steal the session slot from the browser that holds it.
func TestPdfGrabIsServedFromANonHolderSession(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	const holder = "sess-pdf-holder-00000000000000000000001"
	const pending = "sess-pdf-pending-00000000000000000000001"
	runSyncAs(t, b, holder, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
	runSyncAs(t, b, pending, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))

	p := pdfGrabVia(t, b, pending, "pdf-non-holder-0001", "pdf.example.org")
	if p.Outcome != "steering" || p.GrabID == "" || p.Reason != "" {
		t.Fatalf("non-holder grab = %+v, want steering with a grab id and no reason", p)
	}
	if want := "papio/grabs/" + p.GrabID + "/"; p.SteeringPath != want {
		t.Fatalf("steering_path = %q, want %q", p.SteeringPath, want)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", p.GrabID)); err != nil {
		t.Fatalf("landing directory not created for the non-holder grab: %v", err)
	}
	if b.holder == nil || b.holder.ID != holder {
		t.Fatalf("holder = %+v, want the grab to leave the session slot with %s", b.holder, holder)
	}
	var n int
	if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pdf_grabs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("grab rows = %d, want the non-holder's single grab", n)
	}
}

// pdfGrabRefusalCase drives exactly one refusal condition on its own bridge.
type pdfGrabRefusalCase struct {
	name   string
	reason string
	run    func(t *testing.T) *protocol.PdfGrabResultPayload
}

// pdfGrabRefusalCases covers the closed reason vocabulary: one condition per
// value, so a reason cannot quietly become unreachable or start standing in
// for a second cause the way the old single "unavailable" string did.
func pdfGrabRefusalCases() []pdfGrabRefusalCase {
	return []pdfGrabRefusalCase{
		{"no session", grabReasonNoSession, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			return pdfGrabDirect(t, b, "sess-never-said-hello-000000000001", "pdf-no-session-0001", "pdf.example.org")
		}},
		{"session without the effect capability", grabReasonExtensionOutdated, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature))
			return pdfGrabVia(t, b, testSessionID, "pdf-no-permit-0001", "pdf.example.org")
		}},
		{"session without pdf_grab_v1", grabReasonExtensionOutdated, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			runSync(t, b, helloWithFeatures(t, "0.14.0", effectPermitFeature))
			return pdfGrabVia(t, b, testSessionID, "pdf-no-grab-feat-01", "pdf.example.org")
		}},
		{"session below the version floor", grabReasonExtensionOutdated, func(t *testing.T) *protocol.PdfGrabResultPayload {
			// A non-holder carries its own outdated verdict here: the
			// dispatcher's version gate only fires on the holder branch.
			b, _, _, _ := newBridge(t)
			const outdated = "sess-pdf-outdated-000000000000000001"
			runSync(t, b, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
			runSyncAs(t, b, outdated, helloWithFeatures(t, "0.4.0", pdfGrabV1Feature, effectPermitFeature))
			return pdfGrabVia(t, b, outdated, "pdf-outdated-0001", "pdf.example.org")
		}},
		{"daemon without pdf_grab_v1", grabReasonDaemonUnsupported, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			grabCapableHello(t, b)
			b.Features = slices.DeleteFunc(slices.Clone(b.Features), func(f string) bool { return f == pdfGrabV1Feature })
			return pdfGrabVia(t, b, testSessionID, "pdf-daemon-old-0001", "pdf.example.org")
		}},
		{"grab storage not configured", grabReasonNotConfigured, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			grabCapableHello(t, b)
			b.grabs = nil
			return pdfGrabVia(t, b, testSessionID, "pdf-unconfigured-01", "pdf.example.org")
		}},
		{"adoption latch unhealthy", grabReasonAdoptionUnhealthy, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			grabCapableHello(t, b)
			b.adoptionScanSuspended = true
			return pdfGrabVia(t, b, testSessionID, "pdf-latch-sick-0001", "pdf.example.org")
		}},
		{"tab url unusable", grabReasonTabUnusable, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			grabCapableHello(t, b)
			return pdfGrabDirect(t, b, testSessionID, "pdf-bad-host-0001", "")
		}},
		{"effect lane occupied", grabReasonBusy, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, _, _ := newBridge(t)
			grabCapableHello(t, b)
			if first := pdfGrabVia(t, b, testSessionID, "pdf-lane-first-001", "pdf.example.org"); first.Outcome != "steering" {
				t.Fatalf("first grab = %+v, want steering so the lane is occupied", first)
			}
			return pdfGrabVia(t, b, testSessionID, "pdf-lane-second-01", "other.example.org")
		}},
		{"allocation fails unexpectedly", grabReasonInternal, func(t *testing.T) *protocol.PdfGrabResultPayload {
			b, _, cfg, _ := newBridge(t)
			grabCapableHello(t, b)
			// A plain file where the grabs/ subtree belongs makes the
			// landing-directory preparation fail inside the allocation
			// transaction: a daemon-side fault, not a user-fixable one.
			root := cfg.EffectiveAdoptionRoot()
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "grabs"), []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
			return pdfGrabVia(t, b, testSessionID, "pdf-alloc-broken-1", "pdf.example.org")
		}},
	}
}

// TestPdfGrabRefusalsCarryTheirOwnReason pins every refusal to a distinct
// machine-readable cause. Before this, all of them collapsed into one
// free-text "unavailable" the popup could not switch on.
func TestPdfGrabRefusalsCarryTheirOwnReason(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range pdfGrabRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.run(t)
			if p.Outcome != "unavailable" || p.Reason != tc.reason {
				t.Fatalf("payload = %+v, want unavailable/%s", p, tc.reason)
			}
			if p.GrabID != "" || p.SteeringPath != "" {
				t.Fatalf("refusal %+v must name no grab and steer nowhere", p)
			}
			if p.Detail == "" {
				t.Fatalf("refusal %+v must keep a human-readable detail", p)
			}
		})
		seen[tc.reason] = true
	}
	for _, reason := range []string{
		grabReasonNoSession, grabReasonExtensionOutdated, grabReasonDaemonUnsupported,
		grabReasonBusy, grabReasonNotConfigured, grabReasonAdoptionUnhealthy,
		grabReasonTabUnusable, grabReasonInternal,
	} {
		if !seen[reason] {
			t.Errorf("no condition produces reason %q; a dead value in a closed enum is a bug in one of the two", reason)
		}
	}
}

// TestGrabPathDetailsCarryNoInternalVocabulary is the copy fence. Details
// reach the popup verbatim when it has no mapping for a reason, so the daemon
// may not leak its own arbitration words into them — "pdf grab requires the
// current holder with negotiated effect permits" is the string that started
// this.
func TestGrabPathDetailsCarryNoInternalVocabulary(t *testing.T) {
	details := []string{}
	for _, tc := range pdfGrabRefusalCases() {
		details = append(details, tc.run(t).Detail)
	}

	// The abandon reply is the same surface, including the originator refusal
	// that used to say "holder" out loud. A second session that said hello
	// without the grab capabilities is the condition that reaches it.
	b, _, _, _ := newBridge(t)
	const capable = "sess-abandon-capable-0000000000001"
	const incapable = "sess-abandon-incapable-000000000001"
	runSyncAs(t, b, capable, helloWithFeatures(t, "0.14.0", pdfGrabV1Feature, effectPermitFeature))
	runSyncAs(t, b, incapable, helloWithFeatures(t, "0.14.0"))
	// runSyncAs also proves the refusal frame passes outbound self-validation:
	// this branch used to emit "conflict" with no state, which errored out of
	// Sync and disconnected the browser instead of refusing one request.
	refusal, _ := runSyncAs(t, b, incapable, inFrame(t, protocol.MsgPdfGrabAbandonRequest, "", map[string]any{
		"request_id": "pdf-abandon-0001", "grab_id": "grab00000001",
	}))
	refused := firstOfType(refusal, protocol.MsgPdfGrabAbandonResult)
	if refused == nil || refused.Payload.(*protocol.PdfGrabAbandonResultPayload).Outcome != "unavailable" {
		t.Fatalf("abandon from an incapable session = %+v, want an unavailable refusal", refusal)
	}
	details = append(details, refused.Payload.(*protocol.PdfGrabAbandonResultPayload).Detail)

	// Status and abandon both answer "not configured" from their own copy.
	unconfigured, _, _, _ := newBridge(t)
	grabCapableHello(t, unconfigured)
	unconfigured.grabs = nil
	for _, frame := range []struct {
		typ     string
		payload map[string]any
	}{
		{protocol.MsgPdfGrabStatusRequest, map[string]any{"request_id": "pdf-status-0001", "grab_id": "grab00000001"}},
		{protocol.MsgPdfGrabAbandonRequest, map[string]any{"request_id": "pdf-abandon-0002", "grab_id": "grab00000001"}},
	} {
		msgs, _ := runSync(t, unconfigured, inFrame(t, frame.typ, "", frame.payload))
		for _, msg := range msgs {
			switch p := msg.Payload.(type) {
			case *protocol.PdfGrabStatusResultPayload:
				details = append(details, p.Detail)
			case *protocol.PdfGrabAbandonResultPayload:
				details = append(details, p.Detail)
			}
		}
	}

	if len(details) < len(pdfGrabRefusalCases())+3 {
		t.Fatalf("collected %d details, want one per refusal plus the abandon and status replies", len(details))
	}
	for _, detail := range details {
		lowered := strings.ToLower(detail)
		for _, banned := range []string{"holder", "permit", "negotiated"} {
			if strings.Contains(lowered, banned) {
				t.Errorf("user-visible grab detail %q leaks internal vocabulary %q", detail, banned)
			}
		}
	}
}

// grabDOIValidate returns a Validate stub whose Text.Excerpt prints doi in
// front matter, so SweepGrabs's documentDOIs extraction finds it without any
// network fetch.
func grabDOIValidate(doi string) func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
	return func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: 40, Excerpt: "DOI: " + doi + "\nA Paper Worth Grabbing\n"},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
}

// TestSweepGrabsCreatesJobFromDOI is ADR-0020 Decision 4's identifier-found
// path end to end: a settled grab file is quarantined, structurally
// validated, its front-matter DOI extracted, and an ordinary identifier-keyed
// job created and claimed — all from local text, no network fetch needed for
// the artifact itself.
func TestSweepGrabsCreatesJobFromDOI(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.test")
	ctx := context.Background()

	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "A Paper Worth Grabbing")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State != grab.StateJobCreated || got.Outcome != "job_created" || got.JobID == "" {
		t.Fatalf("grab = %+v, want job_created/job_created with a job id", got)
	}
	row, err := jobs.Get(ctx, got.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Work.DOI != "10.1234/grab.test" {
		t.Fatalf("job work.DOI = %q, want the extracted DOI", row.Work.DOI)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("job not claimed by adoption in the same pass: %+v", row)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grab landing dir survived claim: err=%v", err)
	}
}

// TestSweepGrabsCreatesJobFromDOIReportsAlreadyOwned pins the ledger-dedupe
// half of Decision 4: an already-live (awaiting_human) job for the same DOI
// is joined and its bytes are adopted, never duplicated, and the grab reports
// job_created.
func TestSweepGrabsCreatesJobFromDOIReportsAlreadyOwned(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.owned")
	ctx := context.Background()
	existingID := park(t, jobs, "wr_grab_owned", work.Work{DOI: "10.1234/grab.owned", Title: "Already Owned"})

	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Already Owned")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State != grab.StateJobCreated || got.Outcome != "job_created" || got.JobID != existingID {
		t.Fatalf("grab = %+v, want job_created pointing at the existing live job %s", got, existingID)
	}
	var jobCount int
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs j JOIN identifiers i ON i.work_request_id = j.work_request_id WHERE i.kind='doi' AND i.value=?`,
		"10.1234/grab.owned").Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("jobs for this DOI = %d, want exactly 1 (no duplicate)", jobCount)
	}
	row, err := jobs.Get(ctx, existingID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("existing live job not adopted in the same pass: %+v", row)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grab landing dir survived claim: err=%v", err)
	}
	// Ingest succeeded (job is ready) is sufficient — the adoption directory
	// file is promoted into the artifact store and may or may not remain as a
	// transient copy depending on timing, so do not assert its absence.
}

// TestSweepGrabsReadyJobIsAlreadyOwned pins the ready/terminal dedupe half:
// an already-ready bundle for the same DOI reports already_owned and discards
// the captured bytes.
func TestSweepGrabsReadyJobIsAlreadyOwned(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.ready")
	ctx := context.Background()
	readyID := bulkReadyJob(t, jobs, "wr_grab_ready", "10.1234/grab.ready")

	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Ready Already")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.Outcome != "already_owned" || got.JobID != readyID {
		t.Fatalf("grab = %+v, want already_owned pointing at the existing ready job %s", got, readyID)
	}
	if got.State != grab.StateJobCreated {
		t.Fatalf("grab state = %s, want %s", got.State, grab.StateJobCreated)
	}
	var jobCount int
	if err := jobs.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs j JOIN identifiers i ON i.work_request_id = j.work_request_id WHERE i.kind='doi' AND i.value=?`,
		"10.1234/grab.ready").Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("jobs for this DOI = %d, want exactly 1 (no duplicate)", jobCount)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grab landing dir survived already_owned: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), readyID, "main.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ready job must not have adopted file: err=%v", err)
	}
}

// TestSweepGrabsParksNoIdentifier is ADR-0020 Decision 4's no-identifier
// path: the captured bytes remain on a parked grab row, with no synthetic
// title-only job or human action. An explicit identifier later creates the
// canonical job from those same quarantined bytes.
func TestSweepGrabsParksNoIdentifier(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.renameFile = func(string, string) error { return syscall.EXDEV }
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: 40, Excerpt: "No identifier printed anywhere on this page.\n"},
		}, nil
	}
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Mystery Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State != grab.StateParkedNoIdentifier || got.Outcome != "needs_identifier" || got.JobID != "" {
		t.Fatalf("grab = %+v, want parked_no_identifier/needs_identifier without a job", got)
	}
	again, err := b.grabs.Allocate(ctx, "pdf.example.org", "Mystery Paper")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != g.ID || again.Outcome != "existing" {
		t.Fatalf("repeat allocation = %+v, want existing grab %s", again, g.ID)
	}
	var jobsBefore int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("human actions = %+v, want none", actions)
	}
	identified := b.IdentifyGrab(ctx, g.ID, "doi", "10.1234/grab.manual")
	if identified.Outcome != "job_created" || identified.JobID == "" {
		t.Fatalf("identify result = %+v, want job_created", identified)
	}
	var jobsAfter int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore+1 {
		t.Fatalf("jobs after identify = %d, before = %d; want exactly one new job", jobsAfter, jobsBefore)
	}
	final, err := b.grabs.Get(ctx, g.ID)
	if err != nil || final == nil || final.State != grab.StateJobCreated || final.JobID != identified.JobID {
		t.Fatalf("final grab = %+v, err=%v", final, err)
	}
}

// TestSweepGrabsFailsValidationForNonPDF pins the honest failed_validation
// state for a settled file that is not a PDF at all.
func TestSweepGrabsFailsValidationForNonPDF(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{Payload: pdf.PayloadReport{OK: false}}, nil
	}
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Not Actually A PDF")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pdf"), []byte("<html>not a pdf</html>"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State != grab.StateFailedValidation || got.Outcome != "failed_validation" {
		t.Fatalf("grab = %+v, want failed_validation/failed_validation", got)
	}
}

func TestSweepGrabsEmbeddedPDFStillJoinsLiveJob(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: false, Pages: 1, HasEmbeddedFiles: true, Reason: "embedded files"},
			Text:       pdf.TextReport{Chars: 40, Excerpt: "DOI: 10.1234/grab.embedded\nA Paper Worth Grabbing\n"},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
	ctx := context.Background()
	existingID := park(t, jobs, "wr_grab_embedded", work.Work{DOI: "10.1234/grab.embedded", Title: "A Paper Worth Grabbing"})
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Embedded Publisher PDF")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State == grab.StateFailedValidation {
		t.Fatalf("embedded PDF marked failed_validation: %+v", got)
	}
	if got.Outcome != "job_created" || got.JobID != existingID {
		t.Fatalf("grab = %+v, want job_created pointing at live job %s", got, existingID)
	}
}

func TestSweepGrabsReadyJobIdentityReviewKeepsBytes(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text:       pdf.TextReport{Chars: 80, Excerpt: "Erratum:\nDOI: 10.1234/grab.ready\nA Paper Worth Grabbing\n"},
		}, nil
	}
	ctx := context.Background()
	readyID := bulkReadyJob(t, jobs, "wr_grab_ready_review", "10.1234/grab.ready")
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Erratum About Ready Paper")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.Outcome == "already_owned" {
		t.Fatalf("erratum grab claimed already_owned of %s: %+v", readyID, got)
	}
	if got.State != grab.StateParkedNoIdentifier {
		t.Fatalf("grab = %+v, want parked_no_identifier so the capture is kept", got)
	}
	if _, err := os.Stat(got.QuarantinePath); err != nil {
		t.Fatalf("quarantine discarded on identity review: %v", err)
	}
}

func TestSweepGrabsDoesNotOverwritePendingAdoptionFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.unique")
	ctx := context.Background()
	existingID := park(t, jobs, "wr_grab_unique", work.Work{DOI: "10.1234/grab.unique", Title: "A Paper Worth Grabbing"})
	prior := filepath.Join(cfg.EffectiveAdoptionRoot(), existingID, "main.pdf")
	writeFixturePDF(t, prior)
	priorInfo, err := os.Stat(prior)
	if err != nil {
		t.Fatal(err)
	}
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "A Paper Worth Grabbing")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	after, err := os.Stat(prior)
	if err != nil {
		t.Fatalf("pending main.pdf was removed: %v", err)
	}
	if after.ModTime() != priorInfo.ModTime() || after.Size() != priorInfo.Size() {
		t.Fatalf("pending main.pdf was overwritten")
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil || got.JobID != existingID {
		t.Fatalf("grab = %+v, err=%v", got, err)
	}
}

// TestSweepGrabsSkipsTickOnHungRoot mirrors
// TestSweepsSkipTickOnHungAdoptionRoot for the grabs/ subtree: a TCC-hung
// root must never wedge the grab sweeper, and the shared latch must not
// stack a second hung goroutine underneath it.
func TestSweepGrabsSkipsTickOnHungRoot(t *testing.T) {
	b, _, _, _ := newBridge(t)
	compressAdoptionScanDeadline(t)
	ctx := context.Background()

	var calls int32
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	b.readDir = func(string) ([]os.DirEntry, error) {
		atomic.AddInt32(&calls, 1)
		<-block
		return nil, errors.New("unreachable in this test")
	}

	done := make(chan error, 1)
	go func() { done <- b.SweepGrabs(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SweepGrabs during hung root: %v, want nil (skip tick)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SweepGrabs wedged on a hung adoption root instead of skipping the tick")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("readDir calls = %d, want 1", got)
	}
}

// TestHumanActionDismissDiscardsGrabRowWithoutCancellingJob pins ADR-0020's
// TestPdfGrabDismissDeletesParkedRow verifies the v4 grab-backed dismiss
// operation deletes only the parked grab and creates no job or action.
func TestPdfGrabDismissDeletesParkedRow(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload: pdf.PayloadReport{OK: true}, Structural: pdf.StructuralReport{Valid: true, Pages: 1},
			Text: pdf.TextReport{Excerpt: "no identifier here"},
		}, nil
	}
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Dismiss Me")
	if err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if parked, err := b.grabs.Get(ctx, g.ID); err != nil || parked == nil || parked.JobID != "" {
		t.Fatalf("parked grab = %+v, err=%v", parked, err)
	}
	var jobsBefore int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageDecide, "", map[string]any{
		"request_id": "dismiss-req-0001", "item_id": triage.PdfGrabIDPrefix + g.ID, "op": "dismiss",
		"watch_scope": "all",
	}))
	res := firstOfType(msgs, protocol.MsgTriageDecideResult)
	if res == nil || res.Payload.(*protocol.TriageDecideResultPayload).Outcome != "applied" {
		t.Fatalf("dismiss result = %+v", msgs)
	}
	if after, err := b.grabs.Get(ctx, g.ID); err != nil || after != nil {
		t.Fatalf("grab row survived dismiss: %+v, err=%v", after, err)
	}
	var jobsAfter int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("jobs after dismiss = %d, before = %d; dismiss must create/cancel nothing", jobsAfter, jobsBefore)
	}
}

func TestTriageSnapshotOmitsInvalidDeliveryItem(t *testing.T) {
	item := protocol.TriageSnapshotItem{
		Kind: "human_action", ID: "action_bad_delivery", Rank: 1, Title: "delivery",
		Facts: []protocol.TriageFact{}, Links: []protocol.TriageLink{},
		ActionID: 1, JobID: "job_bad_delivery", ActionKind: "document_delivery",
		JobState: "awaiting_human", Revision: 1, RouteClass: "document_delivery",
		AuthRequirement: "unknown", Attention: "required",
		Ops:      []string{"open_request_history", "confirm_request_exists", "confirm_request_absent"},
		Delivery: &protocol.TriageDelivery{Provider: "provider", State: "impossible_state"},
	}
	if err := triageSnapshotItemValidationError(3, item); err == nil {
		t.Fatal("invalid delivery state unexpectedly validated")
	} else if !strings.Contains(err.Error(), `delivery.state`) {
		t.Fatalf("validation error = %v, want delivery state", err)
	}

	counts := triageCountsAfterOmission(protocol.TriageCounts{PendingTotal: 2, Actions: 2}, item, 3)
	if counts.PendingTotal != 1 || counts.Actions != 1 {
		t.Fatalf("v3 counts after omission = %+v, want pending_total/actions 1/1", counts)
	}
	v4Counts := triageCountsAfterOmission(protocol.TriageCounts{PendingTotal: 2, Actions: 2}, item, 4)
	if v4Counts.PendingTotal != 2 || v4Counts.Actions != 2 {
		t.Fatalf("v4 counts after omission = %+v, want global pending_total/actions 2/2", v4Counts)
	}
	valid := item
	valid.ID = "action_good_delivery"
	valid.ActionID = 2
	valid.JobID = "job_good_delivery"
	valid.Delivery = &protocol.TriageDelivery{Provider: "provider", State: "offered"}
	if err := triageSnapshotItemValidationError(3, valid); err != nil {
		t.Fatalf("offered delivery rejected: %v", err)
	}
	payload := protocol.TriageSnapshotResponsePayload{
		RequestID: "request-omit-delivery", Schema: 3,
		GeneratedAt: "2026-01-01T00:00:00Z",
		Counts:      counts, Items: []protocol.TriageSnapshotItem{valid},
		HasMore: false,
	}
	if err := validateTriageSnapshotPayload(payload); err != nil {
		t.Fatalf("remaining snapshot is invalid: %v", err)
	}
	v4 := payload
	v4.Schema = 4
	v4.Counts = protocol.TriageCounts{PendingTotal: 2, Actions: 2}
	if err := triageSnapshotItemValidationError(4, valid); err != nil {
		t.Fatalf("offered delivery rejected in v4: %v", err)
	}
	if err := validateTriageSnapshotPayload(v4); err != nil {
		t.Fatalf("v4 frame with omitted item is invalid: %v", err)
	}
}
func TestTriageSnapshotV4KeepsValidPdfGrab(t *testing.T) {
	b, _, _, _ := newBridge(t)
	payload := b.triageSnapshotPayload(context.Background(), "request-v4-grab", 4, triage.Snapshot{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Counts:      triage.Counts{PendingTotal: 1},
		Items: []triage.Item{{
			Kind: triage.KindPdfGrab, ID: triage.PdfGrabIDPrefix + "grab_valid_1",
			Title: "Reading copy", Ops: []string{"provide_identifier", "dismiss"},
			PdfGrab: &triage.PdfGrab{GrabID: "grab_valid_1", State: "awaiting_file"},
		}},
	})
	if len(payload.Items) != 1 || payload.Items[0].Kind != triage.KindPdfGrab {
		t.Fatalf("pdf grab payload items = %+v, want one retained grab", payload.Items)
	}
	if err := validateTriageSnapshotPayload(payload); err != nil {
		t.Fatalf("retained pdf grab payload invalid: %v", err)
	}
}

// TestTriageSnapshotV3OmitsUnrepresentableActionKinds pins the closed
// schema-3 route-class guard: an unknown action kind is omitted rather than
// poisoning the whole frame with an invalid route class.
func TestTriageSnapshotV3OmitsUnrepresentableActionKinds(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()

	unknownID := park(t, jobs, "wr_v3_omit_unknown", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, unknownID, "unknown_action_kind",
		"grabbed-paper.pdf", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	manualID := park(t, jobs, "wr_v3_omit_manual", handoffWork())
	if _, err := jobs.OpenHumanAction(ctx, manualID, "manual_download",
		"https://provider.example.edu/x", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "",
		protocol.TriageSnapshotRequestPayload{RequestID: "request-v3-omit-01", SchemaVersions: []int64{3}, Limit: 50}))
	snap := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	if snap == nil {
		t.Fatalf("no snapshot in %v", msgs)
	}
	payload := snap.Payload.(*protocol.TriageSnapshotResponsePayload)
	foundManual := false
	for _, item := range payload.Items {
		if item.ActionKind == "unknown_action_kind" {
			t.Fatalf("unknown action kind leaked into a v3 snapshot: %+v", item)
		}
		if item.JobID == manualID {
			foundManual = true
		}
	}
	if !foundManual {
		t.Fatal("representable manual_download item missing — guard over-filtered")
	}
}
func TestPdfGrabStatusUnknownIsRoutineNotFound(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabStatusRequest, "", map[string]any{
		"request_id": "grab-status-0001", "grab_id": "grab_missing_000000000000000000000000",
	}))
	got := firstOfType(msgs, protocol.MsgPdfGrabStatusResult)
	if got == nil {
		t.Fatalf("no pdf_grab_status_result frame: %+v", msgs)
	}
	p := got.Payload.(*protocol.PdfGrabStatusResultPayload)
	if p.Outcome != "not_found" || p.GrabID == "" || p.RequestID == "" {
		t.Fatalf("payload = %+v, want structured not_found", p)
	}
}

func TestPdfGrabAbandonIsIdempotentAndReportsConflicts(t *testing.T) {
	b, _, _, _ := newBridge(t)
	ctx := context.Background()
	abandon := func(t *testing.T, id, requestID string) *protocol.PdfGrabAbandonResultPayload {
		t.Helper()
		raw, err := b.pdfGrabAbandon(ctx, &protocol.PdfGrabAbandonRequestPayload{RequestID: requestID, GrabID: id})
		if err != nil {
			t.Fatal(err)
		}
		msg, err := protocol.DecodeBrowserMessage(raw[0])
		if err != nil {
			t.Fatal(err)
		}
		return msg.Payload.(*protocol.PdfGrabAbandonResultPayload)
	}

	first, err := b.grabs.Allocate(ctx, "example.edu", "retry")
	if err != nil {
		t.Fatal(err)
	}
	if got := abandon(t, first.ID, "grab-abandon-0001"); got.Outcome != "abandoned" || got.State != "abandoned" {
		t.Fatalf("first abandon = %+v", got)
	}
	if got := abandon(t, first.ID, "grab-abandon-0002"); got.Outcome != "abandoned" || got.State != "abandoned" {
		t.Fatalf("retry abandon = %+v", got)
	}

	conflict, err := b.grabs.Allocate(ctx, "example.edu", "conflict")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkQuarantined(ctx, conflict.ID, "quarantine.pdf"); err != nil {
		t.Fatal(err)
	}
	if got := abandon(t, conflict.ID, "grab-abandon-0003"); got.Outcome != "conflict" || got.State != string(grab.StateQuarantined) {
		t.Fatalf("quarantined abandon = %+v", got)
	}

	deleted, err := b.grabs.Allocate(ctx, "example.edu", "deleted")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.Delete(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	if got := abandon(t, deleted.ID, "grab-abandon-0004"); got.Outcome != "not_found" || got.State != "" {
		t.Fatalf("deleted abandon = %+v", got)
	}
}

func TestHandoffLinkRoutineOutcomesAndFreshResolution(t *testing.T) {
	t.Run("holder refusal", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := park(t, jobs, "wr_handoff_holder", handoffWork())
		runSync(t, b, hello())
		const other = "sess-secondary-000000000000000000000000"
		runSyncAs(t, b, other, hello())
		msgs, _ := runSyncAs(t, b, other, inFrame(t, protocol.MsgHandoffLinkRequest, "", protocol.HandoffLinkRequestPayload{JobID: id}))
		errFrame := firstOfType(msgs, protocol.MsgError)
		if errFrame == nil || errFrame.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
			t.Fatalf("non-holder response = %+v, want session_busy", msgs)
		}
	})

	t.Run("job gone", func(t *testing.T) {
		b, _, _, _ := newBridge(t)
		runSync(t, b, hello())
		msgs, _ := runSync(t, b, inFrame(t, protocol.MsgHandoffLinkRequest, "", protocol.HandoffLinkRequestPayload{RequestID: "request-gone-001", JobID: "job-gone-0000001"}))
		result := firstOfType(msgs, protocol.MsgHandoffLinkResult)
		if result == nil || result.Payload.(*protocol.HandoffLinkResultPayload).Outcome != "job_gone" {
			t.Fatalf("missing job response = %+v", msgs)
		}
	})

	t.Run("wrong action kind", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := park(t, jobs, "wr_handoff_kind", handoffWork())
		actions, err := jobs.ListHumanActions(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range actions {
			if action.JobID == id && action.Kind == handoffActionKind {
				if err := jobs.ResolveHumanAction(context.Background(), action.ID, "resolved"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := jobs.OpenHumanAction(context.Background(), id, "manual_download", "download it yourself", job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		msgs, err := b.handoffLink(context.Background(), &protocol.HandoffLinkRequestPayload{JobID: id})
		if err != nil {
			t.Fatal(err)
		}
		result, err := protocol.DecodeBrowserMessage(msgs[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Payload.(*protocol.HandoffLinkResultPayload).Outcome; got != "not_open_action" {
			t.Fatalf("wrong action response = %q, want not_open_action", got)
		}
	})

	t.Run("does not cache action detail", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := park(t, jobs, "wr_handoff_fresh", handoffWork())
		first := app.OABrowserHandoffActionDetail("https://oa.example.edu/fresh?execution=one")
		second := app.OABrowserHandoffActionDetail("https://oa.example.edu/fresh?execution=two")
		if _, err := jobs.OpenHumanAction(context.Background(), id, handoffActionKind, first, job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		mint := func() string {
			frames, err := b.handoffLink(context.Background(), &protocol.HandoffLinkRequestPayload{JobID: id})
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(frames[0])
			if err != nil {
				t.Fatal(err)
			}
			return msg.Payload.(*protocol.HandoffLinkResultPayload).URL
		}
		if got := mint(); got != "https://oa.example.edu/fresh?execution=one" {
			t.Fatalf("first URL = %q", got)
		}
		if _, err := jobs.OpenHumanAction(context.Background(), id, handoffActionKind, second, job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		if got := mint(); got != "https://oa.example.edu/fresh?execution=two" {
			t.Fatalf("second URL = %q, resolver appears cached", got)
		}
	})

	t.Run("outbound frame violations remain fatal", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := park(t, jobs, "wr_handoff_invalid_frame", handoffWork())
		detail := app.OABrowserHandoffActionDetail("https://oa.example.edu/report/{draft}")
		if _, err := jobs.OpenHumanAction(context.Background(), id, handoffActionKind, detail, job.Access(false, "")); err != nil {
			t.Fatal(err)
		}
		frames, err := b.handoffLink(context.Background(), &protocol.HandoffLinkRequestPayload{
			RequestID: "request-invalid-frame-001",
			JobID:     id,
		})
		if !errors.Is(err, ErrOutboundFrame) {
			t.Fatalf("handoffLink error = %v, want ErrOutboundFrame", err)
		}
		if len(frames) != 0 {
			t.Fatalf("handoffLink frames = %+v, want none on a self-validation failure", frames)
		}
	})
}

func directRouteOfferURL(t *testing.T, msgs []*protocol.BrowserMessage) string {
	t.Helper()
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil {
		t.Fatalf("missing job offer: %v", msgs)
	}
	return offer.Payload.(*protocol.JobOfferPayload).OpenURL
}
func parkWithProviderEvidence(t *testing.T, jobs *job.Store, reqID string, w work.Work, host string) string {
	t.Helper()
	id := park(t, jobs, reqID, w)
	if err := jobs.RecordEvent(context.Background(), id, "browser.page_capture", map[string]any{
		"host": host,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDirectRouteUsesTupleProtocol(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := parkWithProviderEvidence(t, jobs, "wr_direct_tuple", handoffWork(), "onlinelibrary.wiley.com")
	msgs, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature, effectPermitFeature))
	req := firstOfType(msgs, protocol.MsgProviderDirectGetRequest)
	if req == nil {
		t.Fatalf("missing provider direct request: %v", msgs)
	}
	if countType(msgs, protocol.MsgProviderDirectGetRequest) != 1 {
		t.Fatalf("direct request count = %d, want one", countType(msgs, protocol.MsgProviderDirectGetRequest))
	}
	if firstOfType(msgs, protocol.MsgJobOffer) != nil {
		t.Fatalf("direct URL leaked through job_offer: %v", msgs)
	}
	p := req.Payload.(*protocol.ProviderDirectGetRequestPayload)
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindDirectGet, DriveAttemptID: p.DriveAttemptID,
		Ordinal: p.Ordinal, Strategy: "direct_get", Revision: p.RouteRevision,
	})
	if err != nil || permit == nil {
		t.Fatalf("direct permit = %+v, err=%v", permit, err)
	}
	if permit.Status != job.EffectPermitHeld {
		t.Fatalf("direct permit status = %q, want held", permit.Status)
	}
	if permit.SafetyDomainID != routeSafetyDomain(p.RouteRevision) {
		t.Fatalf("direct permit domain = %q, want %q", permit.SafetyDomainID, routeSafetyDomain(p.RouteRevision))
	}
	attempt, err := jobs.MaterializationAttemptRevision(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if permit.JobAttemptRevision != attempt || permit.BrowserHolderGeneration != b.epoch {
		t.Fatalf("direct permit fences = attempt %d/holder %d, want %d/%d", permit.JobAttemptRevision, permit.BrowserHolderGeneration, attempt, b.epoch)
	}
	result := inFrame(t, protocol.MsgProviderDirectGetResult, id, protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal, RouteRevision: p.RouteRevision,
		Outcome: "not_pdf", LandingClass: "html",
	})
	runSync(t, b, result)
	runSync(t, b, result)
	permit, err = jobs.GetEffectPermit(context.Background(), permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if permit.Status != job.EffectPermitSettled {
		t.Fatalf("direct permit after result = %q, want settled", permit.Status)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, event := range events {
		if event["kind"] != "browser.direct_route" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["phase"] == "result" {
			results++
			if _, ok := detail["url"]; ok {
				t.Fatalf("direct result persisted bearer URL: %#v", detail)
			}
		}
	}
	if results != 1 {
		t.Fatalf("direct result count = %d, want one", results)
	}
}

func TestDirectRouteRequiresProviderEvidence(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_direct_route_no_provider", handoffWork())
	msgs, _ := runSync(t, b, helloWithFeatures(t, DirectRouteMinExtensionVersion, providerDirectGetV1Feature, effectPermitFeature))
	if countJobOffersFor(msgs, id) != 1 {
		t.Fatalf("offer count = %d, want ordinary offer", countJobOffersFor(msgs, id))
	}
	if got := directRouteOfferURL(t, msgs); strings.Contains(got, "onlinelibrary.wiley.com") {
		t.Fatalf("job without provider evidence received direct route %q", got)
	}
	if countType(msgs, protocol.MsgProviderDirectGetRequest) != 0 {
		t.Fatalf("job without provider evidence received direct request: %v", msgs)
	}
}

func TestDirectRoutesRequireDelegationAndExtensionFloor(t *testing.T) {
	t.Run("assisted job receives ordinary offer only", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		parkWithPolicyMode(t, jobs, "wr_direct_route_assisted", "10.1002/assisted", config.ModeAssisted)
		msgs, _ := runSync(t, b, helloWithFeatures(t, DirectRouteMinExtensionVersion, providerDirectGetV1Feature, effectPermitFeature))
		if got := directRouteOfferURL(t, msgs); strings.Contains(got, "onlinelibrary.wiley.com") {
			t.Fatalf("assisted job received direct route %q", got)
		}
	})
	t.Run("old extension receives ordinary offer only", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := parkWithProviderEvidence(t, jobs, "wr_direct_route_old_extension", handoffWork(), "onlinelibrary.wiley.com")
		msgs, _ := runSync(t, b, helloAs("0.12.9"))
		if countJobOffersFor(msgs, id) != 1 {
			t.Fatalf("offer count = %d, want ordinary offer", countJobOffersFor(msgs, id))
		}
		if got := directRouteOfferURL(t, msgs); strings.Contains(got, "onlinelibrary.wiley.com") {
			t.Fatalf("old extension received direct route %q", got)
		}
	})
}

func TestLatchedJobsReceiveOrdinaryOfferButNoDirectRoute(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := parkWithProviderEvidence(t, jobs, "wr_direct_route_latched", handoffWork(), "onlinelibrary.wiley.com")
	if err := jobs.RecordEvent(context.Background(), id, providerLatchEventKind, map[string]any{
		"safety_domain": "route:wiley-doi-pdfdirect", "kind": "no_positive_effects",
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature, effectPermitFeature))
	if countJobOffersFor(msgs, id) != 1 {
		t.Fatalf("latched job ordinary offers = %d, want 1", countJobOffersFor(msgs, id))
	}
	if firstOfType(msgs, protocol.MsgProviderDirectGetRequest) != nil {
		t.Fatalf("latched job received direct route request: %v", msgs)
	}
}
func TestDirectRouteEffectPermitFeatureGate(t *testing.T) {
	t.Run("featureless holder", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := parkWithProviderEvidence(t, jobs, "direct-featureless", handoffWork(), "onlinelibrary.wiley.com")
		msgs, _ := runSync(t, b, helloAs("0.14.0"))
		if countType(msgs, protocol.MsgProviderDirectGetRequest) != 0 {
			t.Fatalf("featureless holder received direct request: %v", msgs)
		}
		if live, err := jobs.LiveEffectPermit(context.Background()); err != nil || live != nil {
			t.Fatalf("featureless holder permit=%+v err=%v", live, err)
		}
		_ = id
	})
	t.Run("provider direct without permit", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		id := parkWithProviderEvidence(t, jobs, "direct-no-permit", handoffWork(), "onlinelibrary.wiley.com")
		msgs, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature))
		if countType(msgs, protocol.MsgProviderDirectGetRequest) != 0 {
			t.Fatalf("permit-unsupported holder received direct request: %v", msgs)
		}
		if live, err := jobs.LiveEffectPermit(context.Background()); err != nil || live != nil {
			t.Fatalf("permit-unsupported holder permit=%+v err=%v", live, err)
		}
		_ = id
	})
}

func TestDirectRouteOccupiedLaneDefersUntilSettlement(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	firstID := parkWithProviderEvidence(t, jobs, "direct-occupied-first", handoffWork(), "onlinelibrary.wiley.com")
	secondID := parkWithProviderEvidence(t, jobs, "direct-occupied-second", handoffWork(), "onlinelibrary.wiley.com")
	msgs, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature, effectPermitFeature))
	if countType(msgs, protocol.MsgProviderDirectGetRequest) != 1 {
		t.Fatalf("initial direct request count = %d, want one", countType(msgs, protocol.MsgProviderDirectGetRequest))
	}
	firstReq := firstOfType(msgs, protocol.MsgProviderDirectGetRequest)
	first := firstReq.Payload.(*protocol.ProviderDirectGetRequestPayload)
	occupyingID := firstReq.JobID
	deferredID := secondID
	if occupyingID == secondID {
		deferredID = firstID
	} else if occupyingID != firstID {
		t.Fatalf("first request targeted unexpected created job %q (created %q and %q)", occupyingID, firstID, secondID)
	}
	if deferredID == occupyingID {
		t.Fatalf("created jobs did not leave a deferred job distinct from occupying request: %q", occupyingID)
	}
	firstPermit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: occupyingID, Kind: job.EffectKindDirectGet, DriveAttemptID: first.DriveAttemptID,
		Ordinal: first.Ordinal, Strategy: "direct_get", Revision: first.RouteRevision,
	})
	if err != nil || firstPermit == nil {
		t.Fatalf("first direct permit=%+v err=%v", firstPermit, err)
	}
	deferredEvents, err := jobs.Events(context.Background(), deferredID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range deferredEvents {
		if event["kind"] == "browser.direct_route" {
			t.Fatalf("occupied lane recorded a second direct-route event: %#v", event)
		}
	}
	for _, msg := range msgs {
		if msg.JobID == deferredID && msg.Type == protocol.MsgProviderDirectGetRequest {
			t.Fatalf("occupied lane emitted second direct request: %v", msgs)
		}
	}
	result := inFrame(t, protocol.MsgProviderDirectGetResult, occupyingID, protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: first.DriveAttemptID, Ordinal: first.Ordinal, RouteRevision: first.RouteRevision,
		Outcome: "not_pdf", LandingClass: "html",
	})
	settlement, _ := runSync(t, b, result)
	retry := settlement
	if countType(retry, protocol.MsgProviderDirectGetRequest) == 0 {
		delete(b.offered, deferredID)
		b.reofferPending[deferredID] = true
		retry, _ = runSync(t, b)
	}
	if countType(retry, protocol.MsgProviderDirectGetRequest) == 0 {
		t.Fatalf("deferred direct route was not retried after settlement: %v", retry)
	}
	retriedMsg := firstOfType(retry, protocol.MsgProviderDirectGetRequest)
	if retriedMsg.JobID != deferredID {
		t.Fatalf("settled lane request targeted job %q, want deferred job %q", retriedMsg.JobID, deferredID)
	}
	retried := retriedMsg.Payload.(*protocol.ProviderDirectGetRequestPayload)
	if retried.Ordinal != 0 || retried.RouteRevision == "" || retried.DriveAttemptID == "" {
		t.Fatalf("retry direct tuple incomplete: %+v", retried)
	}
}

func TestDirectRouteHistoricalResultSettlesCleanupOnly(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := parkWithProviderEvidence(t, jobs, "direct-historical", handoffWork(), "onlinelibrary.wiley.com")
	msgs, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature, effectPermitFeature))
	req := firstOfType(msgs, protocol.MsgProviderDirectGetRequest)
	if req == nil {
		t.Fatalf("missing direct request: %v", msgs)
	}
	p := req.Payload.(*protocol.ProviderDirectGetRequestPayload)
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindDirectGet, DriveAttemptID: p.DriveAttemptID,
		Ordinal: p.Ordinal, Strategy: "direct_get", Revision: p.RouteRevision,
	})
	if err != nil || permit == nil {
		t.Fatalf("direct permit=%+v err=%v", permit, err)
	}
	if err := jobs.RecordEvent(context.Background(), id, "job.retry_requested", map[string]any{"reason": "historical direct result"}); err != nil {
		t.Fatal(err)
	}
	b.epoch++
	result := inFrame(t, protocol.MsgProviderDirectGetResult, id, protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal, RouteRevision: p.RouteRevision,
		Outcome: "not_pdf", LandingClass: "html",
	})
	runSync(t, b, result)
	got, err := jobs.GetEffectPermit(context.Background(), permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != job.EffectPermitSettled {
		t.Fatalf("historical permit status=%q, want settled", got.Status)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	providerResults, cleanupResults, visibleResults, latches, successors := 0, 0, 0, 0, 0
	for _, event := range events {
		switch event["kind"] {
		case "browser.provider_direct_get_result":
			providerResults++
		case "browser.direct_route":
			detail, _ := event["detail"].(map[string]any)
			if detail["phase"] == "result" {
				if cleanup, _ := detail["cleanup_only"].(bool); cleanup {
					cleanupResults++
				} else {
					visibleResults++
				}
			}
			if detail["phase"] == "offered" && intDetail(detail, "ordinal") == 1 {
				successors++
			}
		case providerLatchEventKind:
			latches++
		}
	}
	if providerResults != 1 || cleanupResults != 1 || visibleResults != 0 || latches != 0 || successors != 0 {
		t.Fatalf("historical cleanup events provider_result=%d cleanup_result=%d visible_result=%d latch=%d successor=%d",
			providerResults, cleanupResults, visibleResults, latches, successors)
	}
}
func TestProviderDriveEpochTupleLifecycle(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "wr_provider_epoch", handoffWork())
	ctx := context.Background()
	attempt := "epoch-test-0001"
	offered := map[string]any{"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1", "safety_domain": "institution:example.edu"}
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", offered); err != nil {
		t.Fatal(err)
	}
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}
	frames, err := b.providerDriveEpochStart(ctx, id, start)
	if err != nil || len(frames) != 1 {
		t.Fatalf("start: frames=%d err=%v", len(frames), err)
	}
	again, err := b.providerDriveEpochStart(ctx, id, start)
	if err != nil || len(again) != 1 {
		t.Fatalf("duplicate start: frames=%d err=%v", len(again), err)
	}
	// Replaying the exact held tuple after a worker death is the one
	// authorizing start replay. Once the permit settles, later replay is
	// non-authorizing.
	repeat, decodeErr := protocol.DecodeBrowserMessage(again[0])
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if got := repeat.Payload.(*protocol.ProviderDriveEpochStartResultPayload).Outcome; got != "started" {
		t.Fatalf("replayed held drive epoch start outcome = %q, want started", got)
	}
	stale := *start
	stale.Ordinal = 1
	staleFrames, err := b.providerDriveEpochStart(ctx, id, &stale)
	if err != nil || len(staleFrames) != 1 {
		t.Fatalf("stale start: frames=%d err=%v", len(staleFrames), err)
	}
	result := &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "not_pdf",
	}
	applied, err := b.providerDriveEpochResult(ctx, id, result)
	if err != nil || len(applied) != 1 {
		t.Fatalf("result: frames=%d err=%v", len(applied), err)
	}
	duplicate, err := b.providerDriveEpochResult(ctx, id, result)
	if err != nil || len(duplicate) != 1 {
		t.Fatalf("duplicate result: frames=%d err=%v", len(duplicate), err)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event["kind"] == "browser.provider_drive_epoch_result" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("durable result count=%d, want 1", count)
	}
}

func TestProviderDriveEpochLateFramesCannotMutateClosedOrSupersededJob(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		effectPermitHolder(t, b)
		ctx := context.Background()
		id := park(t, jobs, "wr_provider_epoch_cancelled", handoffWork())
		attempt := "epoch-cancelled-0001"
		if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
			"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1",
			"safety_domain": "institution:example.edu",
		}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Cancel(ctx, id, job.TerminalReasonBrowserCancelled); err != nil {
			t.Fatal(err)
		}
		start, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		})
		if err != nil || len(start) != 1 {
			t.Fatalf("late start: frames=%d err=%v", len(start), err)
		}
		result, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
		})
		if err != nil || len(result) != 1 {
			t.Fatalf("late result: frames=%d err=%v", len(result), err)
		}
		assertNoProviderEpochMutation(t, jobs, id)
	})

	t.Run("terminal", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		effectPermitHolder(t, b)
		ctx := context.Background()
		id := park(t, jobs, "wr_provider_epoch_terminal", handoffWork())
		attempt := "epoch-terminal-0001"
		if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
			"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1",
			"safety_domain": "institution:example.edu",
		}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, job.StateUnavailable, nil,
			job.WithTerminalReason(job.TerminalReasonNoEntitlement)); err != nil {
			t.Fatal(err)
		}
		start, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		})
		if err != nil || len(start) != 1 {
			t.Fatalf("late start: frames=%d err=%v", len(start), err)
		}
		result, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
		})
		if err != nil || len(result) != 1 {
			t.Fatalf("late result: frames=%d err=%v", len(result), err)
		}
		assertNoProviderEpochMutation(t, jobs, id)
	})

	t.Run("explicit retry supersedes old epoch", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		effectPermitHolder(t, b)
		ctx := context.Background()
		id := park(t, jobs, "wr_provider_epoch_retry_late", handoffWork())
		attempt := "epoch-retry-0001"
		if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
			"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1",
			"safety_domain": "institution:example.edu",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		}); err != nil {
			t.Fatal(err)
		}
		// Settle the historical permit so its late result is cleanup-only.
		if _, _, err := jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
			Identity: job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"},
			RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{
				"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1", "outcome": "applied", "safety_domain": "institution:example.edu",
			}}},
		}); err != nil {
			t.Fatal(err)
		}
		boundary := snapshotProviderEpochLifecycleBoundary(t, jobs, id)
		if err := jobs.RecordEvent(ctx, id, "job.retry_requested", map[string]any{"reason": "explicit_retry"}); err != nil {
			t.Fatal(err)
		}
		// Late frames after explicit retry: exact historical result is allowed as
		// cleanup (records evidence) but must not latch, create a successor, or
		// mutate current state beyond that.
		if _, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
		}); err != nil {
			t.Fatal(err)
		}
		// Verify historical result recorded but no latch/successor/current mutation.
		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) < len(boundary.eventSeqs) {
			t.Fatalf("events shrank")
		}
		for _, event := range events[len(boundary.eventSeqs):] {
			if event["kind"] == providerLatchEventKind {
				t.Fatalf("historical result created latch: %#v", event)
			}
			if event["kind"] == "browser.provider_drive_epoch_offered" {
				detail, _ := event["detail"].(map[string]any)
				if intDetail(detail, "ordinal") == 1 {
					t.Fatalf("historical result minted successor: %#v", event)
				}
			}
		}
		// Start of stale tuple remains stale.
		if _, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		}); err != nil {
			t.Fatal(err)
		}
		events2, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events2[len(events):] {
			if event["kind"] == "browser.provider_drive_epoch_started" {
				t.Fatalf("stale start mutated: %#v", event)
			}
		}
	})
}

type providerEpochLifecycleBoundary struct {
	eventSeqs []int64
}

func snapshotProviderEpochLifecycleBoundary(t *testing.T, jobs *job.Store, id string) providerEpochLifecycleBoundary {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([]int64, len(events))
	for i, event := range events {
		seq, ok := event["seq"].(int64)
		if !ok {
			t.Fatalf("event %d sequence has type %T, want int64", i, event["seq"])
		}
		seqs[i] = seq
	}
	return providerEpochLifecycleBoundary{eventSeqs: seqs}
}

func assertNoProviderEpochMutation(t *testing.T, jobs *job.Store, id string) {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		switch event["kind"] {
		case "browser.provider_drive_epoch_started", "browser.provider_drive_epoch_result", providerLatchEventKind:
			t.Fatalf("late provider frame mutated job: %#v", event)
		}
	}
}

func TestOfferAtURLEpochAuthorizationFailureEmitsNoTuple(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_epoch_append_failure", handoffWork())
	runSync(t, b, helloAs("0.14.0"))
	effectPermitHolder(t, b)
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	action := openHandoffAction(t, jobs, id)
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_provider_epoch_events
		BEFORE INSERT ON events
		WHEN NEW.kind IN ('browser.provider_drive_epoch_superseded', 'browser.provider_drive_epoch_offered')
		BEGIN SELECT RAISE(ABORT, 'provider epoch authorization rejected'); END`); err != nil {
		t.Fatal(err)
	}
	offer, err := b.offerAtURL(*row, action, config.ModeDelegated, "", true)
	if err == nil {
		t.Fatal("offerAtURL succeeded despite an authorization event write failure")
	}
	if len(offer) != 0 {
		t.Fatalf("offerAtURL emitted %d bytes after authorization failure", len(offer))
	}
}

func TestDirectRouteEventWriteFailureExitsPollWithoutOffering(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	parkWithProviderEvidence(t, jobs, "wr_direct_route_append_failure", handoffWork(), "onlinelibrary.wiley.com")
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_direct_route_events
		BEFORE INSERT ON events
		WHEN NEW.kind = 'browser.direct_route'
		BEGIN SELECT RAISE(ABORT, 'direct route authorization rejected'); END`); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, helloAs("0.14.0"))
	if firstOfType(msgs, protocol.MsgProviderDirectGetRequest) != nil ||
		firstOfType(msgs, protocol.MsgJobOffer) != nil {
		t.Fatalf("direct route was emitted after its event write failed: %v", msgs)
	}
}

func TestProviderDriftLatchMatchesRedirectProviderButNotUnrelatedRoute(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	latchedID := park(t, jobs, "wr_drift_redirect_provider", handoffWork())
	latchedRow, err := jobs.Get(ctx, latchedID)
	if err != nil {
		t.Fatal(err)
	}
	latchedAction := openHandoffAction(t, jobs, latchedID)
	domain := actionSafetyDomain(cfg, *latchedRow, latchedAction)
	if err := jobs.RecordEvent(ctx, latchedID, "browser.page_capture", map[string]any{
		"host": "onlinelibrary.wiley.com", "adapter_id": "wiley", "adapter_version": "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, latchedID, providerLatchEventKind, map[string]any{
		"kind": "drift", "safety_domain": domain, "adapter_id": "wiley",
		"adapter_version": "1.0.0", "host": "wiley.com",
	}); err != nil {
		t.Fatal(err)
	}
	if latched, err := b.browserOfferLatched(ctx, *latchedRow, latchedAction); err != nil {
		t.Fatal(err)
	} else if !latched {
		t.Fatal("resolver-to-provider redirect drift was not latched")
	}

	unrelatedID := park(t, jobs, "wr_drift_unrelated_provider", handoffWork())
	unrelatedRow, err := jobs.Get(ctx, unrelatedID)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedAction := openHandoffAction(t, jobs, unrelatedID)
	unrelatedDomain := actionSafetyDomain(cfg, *unrelatedRow, unrelatedAction)
	if err := jobs.RecordEvent(ctx, unrelatedID, providerLatchEventKind, map[string]any{
		"kind": "drift", "safety_domain": unrelatedDomain, "adapter_id": "wiley",
		"adapter_version": "1.0.0", "host": "wiley.com",
	}); err != nil {
		t.Fatal(err)
	}
	if latched, err := b.browserOfferLatched(ctx, *unrelatedRow, unrelatedAction); err != nil {
		t.Fatal(err)
	} else if latched {
		t.Fatal("unrelated route was blocked by provider drift without redirect evidence")
	}
}

func TestProviderDriveEpochOfferReusesAcrossRestartStyleReoffer(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "wr_provider_epoch_offer", handoffWork())
	row, err := jobs.Get(context.Background(), id)

	if err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var action job.HumanAction
	for _, candidate := range actions {
		if candidate.JobID == id {
			action = candidate
			break
		}
	}
	if action.ID == 0 {
		t.Fatal("open action missing")
	}
	runSync(t, b, helloAs("0.14.0"))
	effectPermitHolder(t, b)
	first, err := b.offer(*row, action, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.offer(*row, action, config.ModeDelegated)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw json.RawMessage) string {
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		return msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID
	}
	forced, err := b.offerAtURL(*row, action, config.ModeDelegated, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if decode(forced) == decode(first) {
		t.Fatal("explicit retry reused the open epoch")
	}
	if a, c := decode(first), decode(second); a == "" || a != c {
		t.Fatalf("offer epoch mismatch: first=%q second=%q", a, c)
	}
}
func TestProviderDriveWrongWorkLatchesOnlyCurrentDomain(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "wr_provider_epoch_wrong_work", handoffWork())
	ctx := context.Background()
	offered := map[string]any{
		"drive_attempt_id": "epoch-wrong-0001", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:example.edu",
	}
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", offered); err != nil {
		t.Fatal(err)
	}
	start := &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "epoch-wrong-0001", Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	if _, err := b.providerDriveEpochStart(ctx, id, start); err != nil {
		t.Fatal(err)
	}
	frames, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "epoch-wrong-0001", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Payload.(*protocol.ProviderDriveEpochResultPayload).Outcome; got != "applied" {
		t.Fatalf("wrong-work result outcome = %q, want applied", got)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] != providerLatchEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["kind"] == "no_positive_effects" && detail["safety_domain"] == "institution:example.edu" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing domain-scoped wrong-work latch: %v", events)
	}
	for _, event := range events {
		if event["kind"] != "browser.provider_drive_epoch_offered" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if intDetail(detail, "ordinal") != 0 {
			t.Fatalf("wrong-work advanced epoch: %#v", detail)
		}
	}
}
func TestProviderDriveWrongWorkStaleTupleCannotLatchNewDomain(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "wr_provider_epoch_stale_wrong_work", handoffWork())
	ctx := context.Background()
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": "epoch-old-0001", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:old.example",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "epoch-old-0001", Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": "epoch-new-0001", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:new.example",
	}); err != nil {
		t.Fatal(err)
	}
	frames, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "epoch-old-0001", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Payload.(*protocol.ProviderDriveEpochResultPayload).Outcome; got != "applied" {
		t.Fatalf("stale wrong-work result = %q, want applied (cleanup of exact historical permit)", got)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] != providerLatchEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["safety_domain"] == "institution:new.example" {
			t.Fatalf("stale tuple latched new domain: %#v", detail)
		}
	}
}
func TestInstitutionalRouteProfileFencePrecedesURLDerivation(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-route-profile-fence", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-a", AuthenticationClaimID: "auth-a",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-route-profile-fence", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision:         candidate.JobAttemptRevision,
		InstitutionProfileRevision: candidate.InstitutionProfileRevision,
		RouteRevision:              candidate.RouteRevision, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b",
	}}); err != nil {
		t.Fatal(err)
	}
	frames, err := b.institutionalRoute(ctx, jobID, &protocol.InstitutionalRouteRequestPayload{
		RequestID: "req_route_fence", ClaimID: claim.ID, BindingID: claim.BindingID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("route response frames=%d, want one", len(frames))
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := msg.Payload.(*protocol.InstitutionalRouteResponsePayload)
	if !ok {
		t.Fatalf("route payload=%T, want institutional response", msg.Payload)
	}
	if result.Outcome != "stale" || result.URL != "" || result.RouteIssuanceOrdinal != 0 {
		t.Fatalf("drifted route response=%+v, want stale with no URL/ordinal", result)
	}
	got, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "bound" || got.RouteIssuanceOrdinal != 0 {
		t.Fatalf("drifted claim=%+v, want bound ordinal 0", got)
	}
}

// Delivered bytes are bound to the browser effect authorized to produce them.
// The first adoption wins the attempt and settles its claim; a late producer
// delivering different bytes for the same attempt is refused rather than
// overwriting the artifact that already landed.
func TestDeliveredArtifactIsFencedToTheWinningMaterialization(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "materialization-artifact-winner", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-winner", AuthenticationClaimID: "auth-winner",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-artifact-winner", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profile.Revision, 3); err != nil {
		t.Fatal(err)
	}
	ordinal, err := jobs.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, b.epoch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, b.epoch, ordinal, 3); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(cfg.EffectiveAdoptionRoot(), jobID, "paper.pdf")
	writeFixturePDF(t, path)
	runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, jobID,
		map[string]any{"download_id": 1, "filename": "paper.pdf", "size_bytes": 533}))

	winner, ok, err := jobs.ArtifactWinner(ctx, jobID, 1)
	if err != nil || !ok {
		t.Fatalf("artifact winner after adoption ok=%v err=%v", ok, err)
	}
	if winner.CandidateID != candidate.ID || winner.BrowserHolderGeneration != b.epoch {
		t.Fatalf("winner = %+v, want the navigated candidate and holder", winner)
	}
	settled, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Phase != "settled" {
		t.Fatalf("claim phase = %q, want settled", settled.Phase)
	}

	// A late producer delivers different bytes for the same attempt.
	late := append([]byte("%PDF-1.4\nlate producer\n"), make([]byte, 512)...)
	late = append(late, []byte("\n%%EOF")...)
	if err := os.WriteFile(path, late, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, jobID,
		map[string]any{"download_id": 2, "filename": "paper.pdf", "size_bytes": 534}))
	after, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	superseded := 0
	for _, event := range after {
		if event["kind"] == "browser.artifact_superseded" {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("superseded events = %d (before=%d after=%d), want exactly one refusal",
			superseded, len(before), len(after))
	}
	replaced, ok, err := jobs.ArtifactWinner(ctx, jobID, 1)
	if err != nil || !ok || replaced.SHA256 != winner.SHA256 {
		t.Fatalf("winner after late delivery = %+v ok=%v err=%v, want the original artifact", replaced, ok, err)
	}
}

// A structurally valid late adoption with exact producer correlation records
// the per-attempt artifact winner and releases only that historical effect.
// Expiry or claim replacement must not mutate current claim/job authority.
// Exercise both because replacement is the daemon-restart shape seen in practice.
func TestLateArtifactStaleClaimRecordsWinnerAndSettlesExactProducer(t *testing.T) {
	tests := []struct {
		name string
		kind job.EffectKind
	}{
		{name: "generic", kind: job.GenericDrive},
		{name: "direct", kind: job.DirectGet},
		{name: "institutional", kind: job.Institutional},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, replaced := range []bool{false, true} {
				t.Run(map[bool]string{false: "expired", true: "replaced"}[replaced], func(t *testing.T) {
					b, jobs, _, _ := newBridge(t)
					ctx := context.Background()
					jobID := parkInstitutional(t, jobs, "late-artifact-"+tc.name, handoffWork(), "")
					profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
						ConfiguredName: "late-artifact", AuthorityDigest: "late-artifact-authority",
						AuthenticationClaimID: "late-artifact-auth",
					}})
					if err != nil || len(profiles) != 1 {
						t.Fatalf("profile reconcile: %+v %v", profiles, err)
					}
					candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
						ID: "late-artifact-" + tc.name, JobID: jobID, JobAttemptRevision: 1,
						InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
						RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
						PreRouteSafetyKey: "late-artifact-safety", SafetyDomainID: "late-artifact-domain",
						AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
					})
					if err != nil {
						t.Fatal(err)
					}
					holder := b.epoch
					claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
						CandidateID: candidate.ID, BrowserHolderGeneration: holder,
						JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
						RouteRevision: 1, MaterializationKind: "browser_tab",
						LeaseUntil: time.Now().UTC().Add(time.Minute),
					})
					if err != nil {
						t.Fatal(err)
					}
					if tc.kind == job.Institutional {
						if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, holder, profiles[0].Revision, 0); err != nil {
							t.Fatal(err)
						}
					}
					attempt := "late-artifact-" + tc.name
					ordinal := int64(0)
					strategy := "generic"
					if tc.kind == job.DirectGet {
						strategy = "direct_get"
					}
					producer := job.ArtifactProducerIdentity{
						Kind: tc.kind, DriveAttemptID: attempt, Ordinal: &ordinal,
						Strategy: strategy, Revision: "1",
					}
					identity := job.EffectPermitIdentity{
						JobID: jobID, Kind: tc.kind, DriveAttemptID: attempt,
						Ordinal: 0, Strategy: strategy, Revision: "1",
					}
					acquire := job.EffectPermitAcquireInput{
						Identity: identity, JobAttemptRevision: 1, BrowserHolderGeneration: holder,
						SafetyDomainID: "late-artifact-domain", LeaseUntil: time.Now().UTC().Add(time.Minute),
						Authorization: job.EffectPermitEvent{Kind: "effect.authorized"},
					}
					if tc.kind == job.Institutional {
						effectOrdinal := int64(1)
						producer = job.ArtifactProducerIdentity{
							Kind: tc.kind, ClaimID: claim.ID, BindingID: claim.BindingID,
							EffectOrdinal: &effectOrdinal, InstitutionalRequestID: "late-artifact-request",
						}
						permit, outcome, acquireErr := jobs.AcquireInstitutionalEffectPermit(ctx, job.InstitutionalEffectPermitAcquireInput{
							JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
							SafetyDomainID: "late-artifact-domain", InstitutionalRequestID: producer.InstitutionalRequestID,
							JobAttemptRevision: 1, BrowserHolderGeneration: holder,
							ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().UTC().Add(time.Minute),
							Authorization: job.EffectPermitEvent{Kind: "effect.authorized"},
						})
						if acquireErr != nil || outcome != job.EffectPermitAcquired || permit == nil {
							t.Fatalf("institutional permit=%+v outcome=%v err=%v", permit, outcome, acquireErr)
						}
						identity = job.EffectPermitIdentity{
							JobID: jobID, Kind: tc.kind, ClaimID: claim.ID, BindingID: claim.BindingID,
							EffectOrdinal: 1, InstitutionalRequestID: producer.InstitutionalRequestID,
						}
					} else {
						permit, outcome, acquireErr := jobs.AcquireEffectPermit(ctx, acquire)
						if acquireErr != nil || outcome != job.EffectPermitAcquired || permit == nil {
							t.Fatalf("permit=%+v outcome=%v err=%v", permit, outcome, acquireErr)
						}
					}

					digest := strings.Repeat("a", 64)
					if err := jobs.RecordEvent(ctx, jobID, "browser.download_complete", map[string]any{
						"filename": "late.pdf", "sha256": digest, "producer": producer,
					}); err != nil {
						t.Fatal(err)
					}
					if replaced {
						if _, err := jobs.S.DB().ExecContext(ctx,
							`UPDATE materialization_claims SET browser_holder_generation=? WHERE id=?`,
							holder+1, claim.ID); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := jobs.S.DB().ExecContext(ctx,
							`UPDATE materialization_claims SET lease_until=? WHERE id=?`,
							time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.ID); err != nil {
							t.Fatal(err)
						}
					}
					beforeClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
					if err != nil {
						t.Fatal(err)
					}
					beforeCandidate, err := jobs.GetBrowserCandidate(ctx, candidate.ID)
					if err != nil {
						t.Fatal(err)
					}
					beforeEvents, err := jobs.Events(ctx, jobID)
					if err != nil {
						t.Fatal(err)
					}
					fence := &artifactFence{
						attempt: 1, digest: digest, candidate: candidate, claim: claim, governed: true,
					}
					if err := b.commitArtifact(ctx, jobID, "late.pdf", fence, nil); err != nil {
						t.Fatal(err)
					}
					if err := b.commitArtifact(ctx, jobID, "late.pdf", fence, nil); err != nil {
						t.Fatal(err)
					}
					winner, ok, err := jobs.ArtifactWinner(ctx, jobID, 1)
					if err != nil || !ok || winner.CandidateID != candidate.ID || winner.SHA256 != digest {
						t.Fatalf("late correlated winner=%+v ok=%v err=%v", winner, ok, err)
					}
					permit, err := jobs.GetEffectPermitByIdentity(ctx, identity)
					if err != nil || permit == nil || permit.Status != job.EffectPermitSettled {
						t.Fatalf("late exact producer permit=%+v err=%v", permit, err)
					}
					afterClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
					if err != nil || !reflect.DeepEqual(afterClaim, beforeClaim) {
						t.Fatalf("stale claim changed: before=%+v after=%+v err=%v", beforeClaim, afterClaim, err)
					}
					afterCandidate, err := jobs.GetBrowserCandidate(ctx, candidate.ID)
					if err != nil || afterCandidate.Status != beforeCandidate.Status {
						t.Fatalf("stale candidate changed: before=%+v after=%+v err=%v", beforeCandidate, afterCandidate, err)
					}
					afterEvents, err := jobs.Events(ctx, jobID)
					if err != nil {
						t.Fatal(err)
					}
					if len(afterEvents) != len(beforeEvents)+1 {
						t.Fatalf("late cleanup events=%d before=%d, want one idempotent producer event", len(afterEvents), len(beforeEvents))
					}
					for _, event := range afterEvents[len(beforeEvents):] {
						if event["kind"] == "browser.artifact_unfenced" {
							t.Fatalf("stale adoption retained winner disposition as unfenced event: %#v", event)
						}
					}
				})
			}
		})
	}
}

func TestLateArtifactMissingProducerLeavesExactPermitHeld(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "late-artifact-missing", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "late-artifact-missing", AuthorityDigest: "late-artifact-missing-authority",
		AuthenticationClaimID: "late-artifact-missing-auth",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "late-artifact-missing-candidate", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "late-artifact-missing-safety", SafetyDomainID: "late-artifact-missing-domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	holder := b.epoch
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: holder,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt := "late-artifact-missing-attempt"
	identity := job.EffectPermitIdentity{
		JobID: jobID, Kind: job.GenericDrive, DriveAttemptID: attempt,
		Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	permit, outcome, err := jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
		Identity: identity, JobAttemptRevision: 1, BrowserHolderGeneration: holder,
		SafetyDomainID: "late-artifact-missing-domain", LeaseUntil: time.Now().UTC().Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err != nil || outcome != job.EffectPermitAcquired || permit == nil {
		t.Fatalf("permit=%+v outcome=%v err=%v", permit, outcome, err)
	}
	digest := strings.Repeat("b", 64)
	if err := jobs.RecordEvent(ctx, jobID, "browser.download_complete", map[string]any{
		"filename": "missing.pdf", "sha256": digest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET lease_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.commitArtifact(ctx, jobID, "missing.pdf", &artifactFence{
		attempt: 1, digest: digest, candidate: candidate, claim: claim, governed: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := jobs.ArtifactWinner(ctx, jobID, 1); err != nil || ok {
		t.Fatalf("missing producer created winner: ok=%v err=%v", ok, err)
	}
	got, err := jobs.GetEffectPermit(ctx, permit.ID)
	if err != nil || got == nil || got.Status != job.EffectPermitHeld {
		t.Fatalf("missing producer changed permit=%+v err=%v", got, err)
	}
	afterClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || !reflect.DeepEqual(beforeClaim, afterClaim) {
		t.Fatalf("missing producer changed claim: before=%+v after=%+v err=%v", beforeClaim, afterClaim, err)
	}
	afterEvents, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("missing producer appended events: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
}
func TestInstitutionalReconcileAcceptsTabZero(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-reconcile-tab-zero", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-a", AuthenticationClaimID: "auth-a",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-reconcile-tab-zero", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	frames, err := b.institutionalReconcile(ctx, &protocol.InstitutionalReconcileRequestPayload{
		RequestID: "req_reconcile_zero", Bindings: []protocol.InstitutionalReconcileBinding{{BindingID: claim.BindingID, TabID: 0}},
	})
	if err != nil || len(frames) != 1 {
		t.Fatalf("reconcile frames=%d err=%v", len(frames), err)
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := msg.Payload.(*protocol.InstitutionalReconcileResponsePayload)
	if !ok || len(result.Claims) != 1 || result.Claims[0].TabID == nil || *result.Claims[0].TabID != 0 {
		t.Fatalf("tab-zero reconcile result=%+v", msg.Payload)
	}
}

func TestInstitutionalReconcileStoreErrorIsStructured(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	if err := jobs.S.DB().Close(); err != nil {
		t.Fatal(err)
	}
	frames, err := b.institutionalReconcile(context.Background(), &protocol.InstitutionalReconcileRequestPayload{
		RequestID: "req_reconcile_error",
		Bindings:  []protocol.InstitutionalReconcileBinding{{BindingID: "binding_missing", TabID: 4}},
	})
	if err != nil || len(frames) != 1 {
		t.Fatalf("reconcile frames=%d err=%v, want structured response", len(frames), err)
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result := msg.Payload.(*protocol.InstitutionalReconcileResponsePayload)
	if result.Outcome != "error" || result.Detail == "" || len(result.Claims) != 0 {
		t.Fatalf("reconcile store error result=%+v", result)
	}
}

func TestMaterializationGenerationRetryOnSameSessionHello(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_holder_generation_once
		BEFORE UPDATE OF holder_generation ON daemon_authority_key
		WHEN OLD.holder_generation = 0
		BEGIN SELECT RAISE(ABORT, 'injected generation failure'); END`); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, materializationHello(t))
	if !b.materializationGenerationUnavailable {
		t.Fatal("generation failure did not fail closed")
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_holder_generation_once`); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, materializationHello(t))
	if b.materializationGenerationUnavailable || b.materializationAuthorityUncertain || b.epoch == 0 {
		t.Fatalf("same-session generation retry did not recover: epoch=%d unavailable=%v uncertain=%v", b.epoch, b.materializationGenerationUnavailable, b.materializationAuthorityUncertain)
	}
}

func TestMaterializationProfileFailureSurvivesClaimSweep(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_materialization_profile
		BEFORE INSERT ON institution_profiles
		BEGIN SELECT RAISE(ABORT, 'injected profile failure'); END`); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, materializationHello(t))
	if !b.materializationProfileAuthorityUnavailable || !b.materializationAuthorityUncertain {
		t.Fatalf("profile failure did not fail closed: profile=%v authority=%v", b.materializationProfileAuthorityUnavailable, b.materializationAuthorityUncertain)
	}
	runSync(t, b)
	if !b.materializationProfileAuthorityUnavailable || !b.materializationAuthorityUncertain {
		t.Fatal("claim sweep incorrectly cleared profile authority failure")
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_materialization_profile`); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, materializationHello(t))
	if b.materializationProfileAuthorityUnavailable || b.materializationAuthorityUncertain {
		t.Fatalf("profile retry did not recover: profile=%v authority=%v", b.materializationProfileAuthorityUnavailable, b.materializationAuthorityUncertain)
	}
}
func TestInstitutionalRouteClosedActionDoesNotIssueOrdinal(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-route-closed-action", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-a", AuthenticationClaimID: "auth-a",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-route-closed-action", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil || len(actions) != 1 {
		t.Fatalf("open actions=%+v err=%v", actions, err)
	}
	if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	frames, err := b.institutionalRoute(ctx, jobID, &protocol.InstitutionalRouteRequestPayload{
		RequestID: "req_route_closed", ClaimID: claim.ID, BindingID: claim.BindingID,
	})
	if err != nil || len(frames) != 1 {
		t.Fatalf("route frames=%d err=%v", len(frames), err)
	}
	msg, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result := msg.Payload.(*protocol.InstitutionalRouteResponsePayload)
	if result.Outcome != "not_eligible" || result.URL != "" || result.RouteIssuanceOrdinal != 0 {
		t.Fatalf("closed action route=%+v, want no issuance", result)
	}
	got, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "bound" || got.RouteIssuanceOrdinal != 0 {
		t.Fatalf("closed action claim=%+v, want bound ordinal 0", got)
	}
}

func TestInstitutionalMaterializationHandlersAreDarkAndContinue(t *testing.T) {
	b, _, _, _ := newBridge(t)
	requests := []struct {
		typ, jobID string
		payload    any
		response   string
	}{
		{protocol.MsgInstitutionalClaimRequest, "job_inst_001", protocol.InstitutionalClaimRequestPayload{RequestID: "req_inst_001", CandidateID: "cand_001", MaterializationKind: "browser_tab"}, protocol.MsgInstitutionalClaimResponse},
		{protocol.MsgInstitutionalBindRequest, "job_inst_001", protocol.InstitutionalBindRequestPayload{RequestID: "req_inst_002", ClaimID: "claim_001", BindingID: "bind_001", TabID: 4}, protocol.MsgInstitutionalBindResponse},
		{protocol.MsgInstitutionalRouteRequest, "job_inst_001", protocol.InstitutionalRouteRequestPayload{RequestID: "req_inst_003", ClaimID: "claim_001", BindingID: "bind_001", ExpectedEffectOrdinal: 0, InstitutionalRequestID: "inst_req_003"}, protocol.MsgInstitutionalRouteResponse},
		{protocol.MsgInstitutionalNavigatedRequest, "job_inst_001", protocol.InstitutionalNavigatedRequestPayload{RequestID: "req_inst_004", ClaimID: "claim_001", BindingID: "bind_001", RouteIssuanceOrdinal: 1, EffectOrdinal: 1, InstitutionalRequestID: "inst_req_003", TabID: 4}, protocol.MsgInstitutionalNavigatedResponse},
		{protocol.MsgInstitutionalReconcileRequest, "", protocol.InstitutionalReconcileRequestPayload{RequestID: "req_inst_005", Bindings: []protocol.InstitutionalReconcileBinding{{BindingID: "bind_001", TabID: 4}}}, protocol.MsgInstitutionalReconcileResponse},
	}
	msgs, _ := runSync(t, b, helloAs("0.13.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatal("hello_ack missing")
	}
	for _, req := range requests {
		msgs, _ = runSync(t, b, inFrame(t, req.typ, req.jobID, req.payload))
		got := firstOfType(msgs, req.response)
		if got == nil {
			t.Fatalf("%s response missing: %v", req.typ, msgs)
		}
		switch p := got.Payload.(type) {
		case *protocol.InstitutionalClaimResponsePayload:
			if p.Outcome != "feature_disabled" || p.ClaimID != "" || p.BindingID != "" {
				t.Fatalf("claim response = %#v", p)
			}
		case *protocol.InstitutionalBindResponsePayload:
			if p.Outcome != "feature_disabled" || p.ClaimID != "" || p.BindingID != "" {
				t.Fatalf("bind response = %#v", p)
			}
		case *protocol.InstitutionalRouteResponsePayload:
			if p.Outcome != "feature_disabled" || p.URL != "" {
				t.Fatalf("route response = %#v", p)
			}
		case *protocol.InstitutionalNavigatedResponsePayload:
			if p.Outcome != "feature_disabled" || p.ClaimID != "" {
				t.Fatalf("navigated response = %#v", p)
			}
		case *protocol.InstitutionalReconcileResponsePayload:
			if p.Outcome != "feature_disabled" || len(p.Claims) != 0 {
				t.Fatalf("reconcile response = %#v", p)
			}
		}
	}
	// A disabled materialization call cannot poison the holder/session: a
	// normal RPC immediately afterwards is still served.
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgTriageCountsRequest, "", protocol.TriageCountsRequestPayload{RequestID: "request_inst_continue"}))
	if firstOfType(msgs, protocol.MsgTriageCountsResponse) == nil {
		t.Fatalf("later RPC did not continue after dark handlers: %v", msgs)
	}
}

// Reconcile is only exercised above with the feature disabled, which is served
// by a different branch. With materialization enabled the frame fell out of the
// dispatch switch into the generic unknown-frame default; that default is
// classified ErrInvalidFrame, which is transport-fatal, so the extension's
// post-restart binding re-sync disconnected the session it was repairing.
func TestInstitutionalReconcileIsDispatchedWhenMaterializationIsEnabled(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, materializationHello(t))
	// runSync fails the test if Sync returns an error, which is the assertion
	// that matters here: the frame must never be a transport-fatal error.
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalReconcileRequest, "",
		protocol.InstitutionalReconcileRequestPayload{
			RequestID: "req_reconcile_enabled",
			Bindings:  []protocol.InstitutionalReconcileBinding{{BindingID: "bind_001", TabID: 4}},
		}))
	got := firstOfType(msgs, protocol.MsgInstitutionalReconcileResponse)
	if got == nil {
		t.Fatalf("reconcile response missing: %v", msgs)
	}
	p, ok := got.Payload.(*protocol.InstitutionalReconcileResponsePayload)
	if !ok || p.RequestID != "req_reconcile_enabled" {
		t.Fatalf("reconcile response = %#v", got.Payload)
	}
	if p.Outcome == "feature_disabled" {
		t.Fatalf("reconcile answered from the disabled branch while the feature is enabled: %#v", p)
	}
	// The session must still serve ordinary work afterwards.
	after, _ := runSync(t, b, inFrame(t, protocol.MsgTriageCountsRequest, "",
		protocol.TriageCountsRequestPayload{RequestID: "req_after_reconcile"}))
	if firstOfType(after, protocol.MsgTriageCountsResponse) == nil {
		t.Fatalf("session did not survive reconcile: %v", after)
	}
}

func TestInstitutionalMaterializationRequiresExplicitClientFeature(t *testing.T) {
	request := inFrame(t, protocol.MsgInstitutionalClaimRequest, "job_inst_001",
		protocol.InstitutionalClaimRequestPayload{RequestID: "req_feature_claim", CandidateID: "cand_001", MaterializationKind: "browser_tab"})

	withoutFeature, _, _, _ := newBridge(t)
	runSync(t, withoutFeature, helloAs("99.0.0"))
	msgs, _ := runSync(t, withoutFeature, request)
	disabled := firstOfType(msgs, protocol.MsgInstitutionalClaimResponse)
	if disabled == nil || disabled.Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome != "feature_disabled" {
		t.Fatalf("version-only materialization response = %#v, want feature_disabled", disabled)
	}

	withFeature, _, _, _ := newBridge(t)
	runSync(t, withFeature, inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.14.0",
		"features":          []string{protocol.InstitutionalMaterializationFeature, protocol.EffectPermitFeature},
	}))
	msgs, _ = runSync(t, withFeature, request)
	enabled := firstOfType(msgs, protocol.MsgInstitutionalClaimResponse)
	if enabled == nil || enabled.Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome != "stale" {
		t.Fatalf("explicitly negotiated materialization response = %#v, want stale candidate", enabled)
	}
}
func TestOrdinaryPollDoesNotCreateCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-no-auto-create", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	runSync(t, b)
	var count int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_candidates WHERE job_id=?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ordinary poll created %d candidate rows without explicit focus", count)
	}
}

func TestExplicitFocusCreatesAndOffersCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-focus-create", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	var count int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_candidates WHERE job_id=?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("explicit focus created %d candidate rows, want one", count)
	}
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("explicit focus did not emit candidate offer: %v", msgs)
	}
}

func TestMaterializationSchedulerKeepsOneSafetyDomainScaffold(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	first := parkInstitutional(t, jobs, "materialization-domain-first", handoffWork(), "")
	second := parkInstitutional(t, jobs, "materialization-domain-second", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, first, "same-domain")
	explicitMaterializationCandidate(t, jobs, second, "same-domain")
	if queued, live, err := b.FocusHandoffs(ctx, []string{first, second}); err != nil || !live || queued != 2 {
		t.Fatalf("focus queue = queued %d live %v err %v, want two live requests", queued, live, err)
	}
	msgs, _ := runSync(t, b)
	if got := countType(msgs, protocol.MsgInstitutionalCandidateOffer); got != 1 {
		t.Fatalf("same safety domain emitted %d candidate offers, want one: %v", got, msgs)
	}
}

// TestFocusHandoffStartsTheNextAttemptForASpentCandidate pins the operator's
// second ask. A paper whose institutional navigation completed but delivered
// nothing keeps its candidate owned (the one-navigation-per-attempt guard), and
// the only way out is a new attempt. The focus marker is sticky, so the second
// ask used to short-circuit on it and never reach that decision: measured live
// 2026-08-20, `papio actions open` on such a paper produced nothing at all
// while the daemon answered its claims 'busy' about once a second.
func TestFocusHandoffStartsTheNextAttemptForASpentCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "focus-spent-attempt", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "spent-domain")

	// First ask: ordinary, and it leaves the sticky focus marker behind.
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("first focus = queued %d live %v err %v, want one live request", queued, live, err)
	}
	before, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}

	// Spend the attempt: navigated, effect settled, diagnostic lease gone.
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: before,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: b.epoch,
		LeaseUntil: b.now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET phase='navigated', lease_until=? WHERE id=?`,
		b.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}

	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("second focus = queued %d live %v err %v, want the ask to be honoured", queued, live, err)
	}
	after, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("attempt revision %d -> %d, want the second ask to start the next attempt", before, after)
	}
	fresh, err := jobs.CurrentBrowserCandidateForJob(ctx, jobID, after)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == nil || fresh.ID == candidateID {
		t.Fatalf("candidate after the second ask = %+v, want a fresh one for the new attempt", fresh)
	}
}

// TestSpentCandidateStopsBeingOffered pins the churn's source. A candidate owned
// by a finished attempt was re-offered every poll, and each offer could only
// end in a claim answered 'busy' - measured live 2026-08-20 as a sustained
// round trip about once a second, per stuck paper, indefinitely.
func TestSpentCandidateStopsBeingOffered(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "spent-offer-suppressed", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	// No seeded candidate: the daemon mints its own during the focus, so the
	// attempt has exactly one, as it does live. A seeded sibling would leave an
	// older eligible row that the offer loop keeps handing out.
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	if offers, _ := runSync(t, b); firstOfType(offers, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("first poll offered no candidate: %v", offers)
	}

	// Spend the candidate the offer loop will reach for next - the same
	// selection it uses - the way the live papers were spent: claimed, its
	// claim navigated, the diagnostic lease over, no artifact winner.
	attempt, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := jobs.CurrentBrowserCandidateForJob(ctx, jobID, attempt)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v err=%v", current, err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: current.ID, JobAttemptRevision: current.JobAttemptRevision,
		InstitutionProfileRevision: current.InstitutionProfileRevision,
		RouteRevision:              current.RouteRevision,
		MaterializationKind:        "browser_tab", BrowserHolderGeneration: b.epoch,
		LeaseUntil: b.now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, current.InstitutionProfileRevision, 9); err != nil {
		t.Fatal(err)
	}
	// The settled institutional effect is what keeps the candidate owned past
	// its lease - without it reconciliation would simply re-arm the candidate,
	// which is not the live state at all.
	if _, outcome, err := jobs.AcquireInstitutionalEffectPermit(ctx, job.InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: current.SafetyDomainID, InstitutionalRequestID: "spent-offer-request",
		JobAttemptRevision: current.JobAttemptRevision, BrowserHolderGeneration: b.epoch,
		ExpectedEffectOrdinal: 0, LeaseUntil: b.now().Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "institutional.authorized"},
	}); err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("acquire institutional permit outcome=%v err=%v", outcome, err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE effect_permits SET status='settled' WHERE claim_id=?`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET phase='navigated', lease_until=? WHERE id=?`,
		b.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}
	if spent, err := jobs.SpentMaterializationCandidate(ctx, jobID); err != nil || !spent {
		t.Fatalf("attempt not spent after navigating its claim: spent=%v err=%v", spent, err)
	}

	after, _ := runSync(t, b)
	if got := countType(after, protocol.MsgInstitutionalCandidateOffer); got != 0 {
		t.Fatalf("spent candidate was offered %d times, want none", got)
	}
}

// TestSpentCandidateClaimAnswersStaleNotBusy pins the other half of the churn.
// The extension treats 'busy' as "try again shortly": it keeps the correlation
// and re-drives its bounded claim ladder on every keepalive tick, so a conflict
// that will never clear became a permanent retry loop - measured live
// 2026-08-20 as bursts of four attempts every ~60s, per paper, for twenty
// hours. A finished attempt must read as stale so the workflow is dropped.
func TestSpentCandidateClaimAnswersStaleNotBusy(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "spent-claim-stale", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	if offers, _ := runSync(t, b); firstOfType(offers, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("first poll offered no candidate: %v", offers)
	}
	attempt, err := jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := jobs.CurrentBrowserCandidateForJob(ctx, jobID, attempt)
	if err != nil || current == nil {
		t.Fatalf("current candidate: %+v err=%v", current, err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: current.ID, JobAttemptRevision: current.JobAttemptRevision,
		InstitutionProfileRevision: current.InstitutionProfileRevision,
		RouteRevision:              current.RouteRevision,
		MaterializationKind:        "browser_tab", BrowserHolderGeneration: b.epoch,
		LeaseUntil: b.now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, current.InstitutionProfileRevision, 9); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := jobs.AcquireInstitutionalEffectPermit(ctx, job.InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: current.SafetyDomainID, InstitutionalRequestID: "spent-claim-request",
		JobAttemptRevision: current.JobAttemptRevision, BrowserHolderGeneration: b.epoch,
		ExpectedEffectOrdinal: 0, LeaseUntil: b.now().Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "institutional.authorized"},
	}); err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("acquire institutional permit outcome=%v err=%v", outcome, err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE effect_permits SET status='settled' WHERE claim_id=?`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET phase='navigated', lease_until=? WHERE id=?`,
		b.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}

	// The extension re-drives the correlation it still holds.
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "spent-claim-redrive", CandidateID: current.ID,
			MaterializationKind: "browser_tab",
		}))
	resp := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if resp == nil {
		t.Fatalf("institutional_claim_response missing: %v", claimed)
	}
	payload := resp.Payload.(*protocol.InstitutionalClaimResponsePayload)
	if payload.Outcome != "stale" {
		t.Fatalf("claim on a spent attempt = %s, want stale so the workflow is dropped: %+v", payload.Outcome, payload)
	}
}

func TestMaterializationSchedulerErrorRetainsFocusUntilRecovery(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-scheduler-retry", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	b.materializationScheduleCursor = job.CandidateScheduleCursor{LastGroup: "prior"}
	beforeCursor := b.materializationScheduleCursor
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "retry-domain")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	var failed atomic.Bool
	b.scheduleEligibleCandidates = func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
		if failed.CompareAndSwap(false, true) {
			return job.CandidateSchedulePage{}, errors.New("temporary scheduler outage")
		}
		return job.CandidateSchedulePage{
			Candidates: []job.BrowserCandidateDescriptor{schedulerDescriptor(t, jobs, candidateID, "eligible")},
		}, nil
	}
	first, _ := runSync(t, b)
	if firstOfType(first, protocol.MsgInstitutionalCandidateOffer) != nil {
		t.Fatalf("scheduler error emitted a candidate offer: %v", first)
	}
	if !reflect.DeepEqual(b.materializationScheduleCursor, beforeCursor) {
		t.Fatalf("scheduler error advanced cursor: got=%#v want=%#v", b.materializationScheduleCursor, beforeCursor)
	}
	if !b.focusPending[jobID] || b.materializationTracked[jobID] {
		t.Fatalf("scheduler error lost explicit focus/tracking: focus=%v tracked=%v", b.focusPending[jobID], b.materializationTracked[jobID])
	}
	recovered, _ := runSync(t, b)
	if firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("scheduler recovery did not emit candidate offer: %v", recovered)
	}
}

func TestMaterializationSchedulerStallDoesNotBlockHolderTakeover(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-scheduler-stall", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "stall-domain")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v, want one live request", queued, live, err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b.scheduleEligibleCandidates = func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return job.CandidateSchedulePage{}, nil
	}
	oldSession := testSessionID
	go func() {
		_, _ = b.Sync(ctx, oldSession, false, nil)
	}()
	<-started
	b.mu.Lock()
	if b.holder != nil {
		b.holder.LastSyncAt = b.now().Add(-sessionStaleAfter - time.Second)
	}
	b.mu.Unlock()
	const replacement = "sess-scheduler-replacement-000000000000000000"
	replacementMsgs, _ := runSyncAs(t, b, replacement, hello())
	if firstOfType(replacementMsgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("replacement did not become holder while scheduler stalled: %v", replacementMsgs)
	}
	b.mu.Lock()
	holder := b.holder
	b.mu.Unlock()
	if holder == nil || holder.ID != replacement {
		t.Fatalf("holder = %#v, want replacement while first scheduler call is blocked", holder)
	}
	close(release)
}
func TestMaterializationCursorRetainedAcrossClaimReconcileFailure(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-cursor-reconcile-failure", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	offer := firstOfType(initial, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	candidateID := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "cursor-reconcile-claim", CandidateID: candidateID, MaterializationKind: "browser_tab",
		}))
	claimResult := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claimResult == nil || claimResult.Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome != "claimed" {
		t.Fatalf("claim result = %v", claimed)
	}
	b.now = func() time.Time { return time.Now().UTC().Add(2 * b.actionExpiry()) }
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_materialization_reconcile_once
		BEFORE UPDATE OF phase ON materialization_claims
		BEGIN SELECT RAISE(ABORT, 'injected reconcile failure'); END`); err != nil {
		t.Fatal(err)
	}
	cursorBefore := b.materializationScheduleCursor
	runSync(t, b)
	if b.materializationScheduleCursor.LastGroup != cursorBefore.LastGroup ||
		len(b.materializationScheduleCursor.Offsets) != len(cursorBefore.Offsets) {
		t.Fatalf("scheduler cursor advanced across reconcile failure: before=%+v after=%+v", cursorBefore, b.materializationScheduleCursor)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_materialization_reconcile_once`); err != nil {
		t.Fatal(err)
	}
	recovered, _ := runSync(t, b)
	if firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("reconcile recovery did not re-offer candidate: %v", recovered)
	}
}

func TestResolvedAwaitingMaterializationActionCancelsTrackedScaffold(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-resolved-action-cancel", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("candidate offer missing: %v", initial)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil || len(actions) == 0 {
		t.Fatalf("open handoff action = %v, %v", actions, err)
	}
	if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := runSync(t, b)
	if firstOfType(cancelled, protocol.MsgCancel) == nil {
		t.Fatalf("resolved awaiting action did not cancel scaffold: %v", cancelled)
	}
	if b.materializationTracked[jobID] {
		t.Fatal("resolved awaiting action left materialization tracking open")
	}
}
func TestMaterializationTakeoverResetsScheduleCursorForRecovery(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-takeover-cursor", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	b.mu.Lock()
	b.holder.LastSyncAt = b.now().Add(-sessionStaleAfter - time.Second)
	b.mu.Unlock()
	const replacement = "sess-takeover-cursor-000000000000000000000000"
	recovered, _ := runSyncAs(t, b, replacement, materializationHello(t))
	if firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("takeover did not re-offer candidate after cursor reset: %v", recovered)
	}
}
func TestMaterializationListErrorRetainsCursorUntilRecovery(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-list-error-cursor", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("initial candidate offer missing: %v", initial)
	}
	cursorBefore := b.materializationScheduleCursor
	var failed atomic.Bool
	b.listAwaitingHuman = func(context.Context, int) ([]job.Row, error) {
		if failed.CompareAndSwap(false, true) {
			return nil, errors.New("temporary list failure")
		}
		return jobs.List(ctx, job.StateAwaitingHuman, 200)
	}
	runSync(t, b)
	if b.materializationScheduleCursor.LastGroup != cursorBefore.LastGroup ||
		len(b.materializationScheduleCursor.Offsets) != len(cursorBefore.Offsets) {
		t.Fatalf("cursor advanced across list failure: before=%+v after=%+v", cursorBefore, b.materializationScheduleCursor)
	}
	b.listAwaitingHuman = nil
	recovered, _ := runSync(t, b)
	if firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("list recovery did not re-offer candidate: %v", recovered)
	}
}

func TestMaterializationNoDescriptorNeverFallsBackToLegacyOffer(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-no-descriptor", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	b.scheduleEligibleCandidates = func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
		return job.CandidateSchedulePage{}, nil
	}
	msgs, _ := runSync(t, b)
	if countJobOffersFor(msgs, jobID) != 0 {
		t.Fatalf("materialization-capable no-descriptor focus fell back to legacy job_offer: %v", msgs)
	}
	if !b.focusPending[jobID] {
		t.Fatal("no-descriptor focus intent was cleared")
	}
}
func TestMaterializationRestartRecoversLiveClaimWithoutSecondTab(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-live-claim-restart", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	offer := firstOfType(initial, protocol.MsgInstitutionalCandidateOffer)
	if offer == nil {
		t.Fatalf("candidate offer missing: %v", initial)
	}
	candidateID := offer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID
	claimed, _ := runSync(t, b, inFrame(t, protocol.MsgInstitutionalClaimRequest, jobID,
		protocol.InstitutionalClaimRequestPayload{
			RequestID: "live-claim-restart", CandidateID: candidateID, MaterializationKind: "browser_tab",
		}))
	claim := firstOfType(claimed, protocol.MsgInstitutionalClaimResponse)
	if claim == nil || claim.Payload.(*protocol.InstitutionalClaimResponsePayload).Outcome != "claimed" {
		t.Fatalf("claim result = %v", claimed)
	}
	const replacement = "sess-live-claim-replacement-00000000000000000"
	b.mu.Lock()
	b.promote(&browserSession{ID: replacement, ExtensionVersion: "0.14.0", Features: []string{institutionalMaterializationFeature, effectPermitFeature}, LastSyncAt: b.now()}, "live claim restart")
	b.mu.Unlock()
	recovered, _ := runSyncAs(t, b, replacement)
	reoffer := firstOfType(recovered, protocol.MsgInstitutionalCandidateOffer)
	if reoffer == nil || reoffer.Payload.(*protocol.InstitutionalCandidateOfferPayload).CandidateID != candidateID {
		t.Fatalf("live claim recovery offer = %v, want candidate %s", recovered, candidateID)
	}
	recoveredPayload := reoffer.Payload.(*protocol.InstitutionalCandidateOfferPayload)
	if recoveredPayload.ExpiresAt == "" {
		t.Fatal("live claim recovery offer omitted expiry")
	}
	expiresAt, err := time.Parse(time.RFC3339, recoveredPayload.ExpiresAt)
	if err != nil || !expiresAt.After(b.now()) {
		t.Fatalf("live claim recovery expiry = %q, parsed=%v now=%v", recoveredPayload.ExpiresAt, err, b.now())
	}
	if countJobOffersFor(recovered, jobID) != 0 {
		t.Fatalf("live claim recovery emitted legacy/second-tab job offer: %v", recovered)
	}
}
func TestMaterializationSameLandedDomainDifferentPreRouteOffersOnce(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	first := parkInstitutional(t, jobs, "materialization-domain-global-first", handoffWork(), "")
	second := parkInstitutional(t, jobs, "materialization-domain-global-second", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, first, "landed-global")
	secondCandidate := explicitMaterializationCandidate(t, jobs, second, "landed-global")
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET pre_route_safety_key=? WHERE id=?`, "different-pre-route", secondCandidate); err != nil {
		t.Fatal(err)
	}
	if queued, live, err := b.FocusHandoffs(ctx, []string{first, second}); err != nil || !live || queued != 2 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	msgs, _ := runSync(t, b)
	if got := countType(msgs, protocol.MsgInstitutionalCandidateOffer); got != 1 {
		t.Fatalf("same landed domain with different pre-route emitted %d offers, want one: %v", got, msgs)
	}
}
func TestMaterializationActionLookupTransientErrorRetainsScaffold(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-action-lookup-retry", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	initial, _ := runSync(t, b)
	if firstOfType(initial, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("candidate offer missing: %v", initial)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil || len(actions) == 0 {
		t.Fatalf("open action = %v, %v", actions, err)
	}
	b.listOpenHandoffs = func(context.Context, int) ([]job.OpenHandoffJob, bool, error) {
		return nil, false, nil
	}
	var failed atomic.Bool
	b.openHandoffForJobFn = func(context.Context, string) (*job.HumanAction, error) {
		if failed.CompareAndSwap(false, true) {
			return nil, errors.New("temporary action lookup failure")
		}
		return nil, sql.ErrNoRows
	}
	first, _ := runSync(t, b)
	if firstOfType(first, protocol.MsgCancel) != nil || !b.materializationTracked[jobID] {
		t.Fatalf("transient action lookup cancelled/lost scaffold: msgs=%v tracked=%v", first, b.materializationTracked[jobID])
	}
	b.openHandoffForJobFn = nil
	if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	resolved, _ := runSync(t, b)
	if firstOfType(resolved, protocol.MsgCancel) == nil || b.materializationTracked[jobID] {
		t.Fatalf("resolved action did not cancel scaffold: msgs=%v tracked=%v", resolved, b.materializationTracked[jobID])
	}
}
func TestConcurrentFocusHandoffsCreateOneCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-concurrent-focus", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, live, err := b.FocusHandoffs(ctx, []string{jobID})
			if err == nil && !live {
				err = errors.New("focus session unexpectedly offline")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_candidates WHERE job_id=?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent focus created %d candidates, want one", count)
	}
}

func TestFocusedCandidateOutsideBoundedHandoffPageUsesExactActionLookup(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-focus-outside-page", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	explicitMaterializationCandidate(t, jobs, jobID, "outside-page-domain")
	b.listOpenHandoffs = func(context.Context, int) ([]job.OpenHandoffJob, bool, error) {
		return nil, true, nil
	}
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("focused candidate outside handoff page did not offer: %v", msgs)
	}
}

func TestTerminalMaterializationCandidateClearsFocusWithoutOffer(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-terminal-focus", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "terminal-domain")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	if err := jobs.SetBrowserCandidateStatus(ctx, candidateID, "eligible", "succeeded"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) != nil || b.focusPending[jobID] {
		t.Fatalf("terminal candidate retained focus or emitted offer: msgs=%v focus=%v", msgs, b.focusPending[jobID])
	}
}
func TestOverlappingSyncsSingleFlightScheduler(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-overlap-sync", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "overlap-domain")
	if queued, live, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil || !live || queued != 1 {
		t.Fatalf("focus queue = queued %d live %v err %v", queued, live, err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b.scheduleEligibleCandidates = func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return job.CandidateSchedulePage{
			Candidates: []job.BrowserCandidateDescriptor{schedulerDescriptor(t, jobs, candidateID, "eligible")},
			Cursor:     job.CandidateScheduleCursor{LastGroup: "overlap-domain"},
			HasMore:    true,
		}, nil
	}
	first := make(chan []json.RawMessage, 1)
	go func() {
		msgs, _ := b.Sync(ctx, testSessionID, false, nil)
		first <- msgs
	}()
	<-started
	second, err := b.Sync(ctx, testSessionID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if countRawType(t, second, protocol.MsgInstitutionalCandidateOffer) != 0 {
		t.Fatalf("overlapping sync emitted offer before scheduler completion: %v", second)
	}
	close(release)
	firstMsgs := <-first
	if calls.Load() != 1 {
		t.Fatalf("scheduler calls = %d, want one", calls.Load())
	}
	if countRawType(t, firstMsgs, protocol.MsgInstitutionalCandidateOffer) != 1 {
		t.Fatalf("completed scheduler emitted %d offers: %v", countRawType(t, firstMsgs, protocol.MsgInstitutionalCandidateOffer), firstMsgs)
	}
	if b.materializationScheduleCursor.LastGroup != "overlap-domain" {
		t.Fatalf("cursor = %#v, want completed page cursor", b.materializationScheduleCursor)
	}
}
func TestMaterializationProfileRevisionBumpCreatesFreshCandidate(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-profile-revision", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	if _, _, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil {
		t.Fatal(err)
	}
	var firstID string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT id FROM browser_candidates WHERE job_id=?`, jobID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE institution_profiles SET revision=revision+1, authority_digest=? WHERE tombstoned_at IS NULL`, "authority-revision-2"); err != nil {
		t.Fatal(err)
	}
	delete(b.focusPending, jobID)
	if _, _, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_candidates WHERE job_id=?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("profile revision bump created %d candidates, want two", count)
	}
	var secondID string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT id FROM browser_candidates WHERE job_id=? AND id<>?`, jobID, firstID).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if secondID == firstID {
		t.Fatal("profile revision reused candidate identity")
	}
}

func TestSchedulerAuthorityDriftSuppressesStaleOffer(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-authority-drift", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	candidateID := explicitMaterializationCandidate(t, jobs, jobID, "authority-drift")
	if _, _, err := b.FocusHandoffs(ctx, []string{jobID}); err != nil {
		t.Fatal(err)
	}
	b.scheduleEligibleCandidates = func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
		if err := jobs.SetBrowserCandidateStatus(ctx, candidateID, "eligible", "succeeded"); err != nil {
			t.Fatal(err)
		}
		descriptor := schedulerDescriptor(t, jobs, candidateID, "eligible")
		return job.CandidateSchedulePage{Candidates: []job.BrowserCandidateDescriptor{descriptor}}, nil
	}
	msgs, _ := runSync(t, b)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) != nil {
		t.Fatalf("stale scheduler descriptor emitted candidate offer: %v", msgs)
	}
}
func TestFocusPreparationTakeoverReplacementOffersWithoutRetry(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "materialization-focus-takeover", handoffWork(), "")
	runSync(t, b, materializationHello(t))
	started, release := make(chan struct{}), make(chan struct{})
	var prepOnce atomic.Bool
	var prepFn func(context.Context, job.Row) (*job.BrowserCandidate, error)
	prepFn = func(pctx context.Context, row job.Row) (*job.BrowserCandidate, error) {
		if prepOnce.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		b.prepareMaterializationCandidateFn = nil
		candidate, err := b.prepareMaterializationCandidate(pctx, row)
		b.prepareMaterializationCandidateFn = prepFn
		return candidate, err
	}
	b.prepareMaterializationCandidateFn = prepFn
	result := make(chan error, 1)
	go func() {
		_, _, err := b.FocusHandoffs(ctx, []string{jobID})
		result <- err
	}()
	<-started
	const replacement = "sess-focus-takeover-replacement-000000000000000"
	b.mu.Lock()
	b.promote(&browserSession{
		ID: replacement, ExtensionVersion: "0.14.0",
		Features: []string{institutionalMaterializationFeature, effectPermitFeature}, LastSyncAt: b.now(),
	}, "focus preparation takeover")
	b.mu.Unlock()
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSyncAs(t, b, replacement)
	if firstOfType(msgs, protocol.MsgInstitutionalCandidateOffer) == nil {
		t.Fatalf("replacement did not offer prepared candidate without retry: %v", msgs)
	}
}

func TestBridgePersistsTwoProfileObservationsInOneSync(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl"},
	}
	b.cfg = cfg
	received := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return received }
	daemonStart := time.Now().UTC().Add(-time.Second)
	runSync(t, b, hello())
	runSync(t, b,
		inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
			"evidence": "warm_verified", "origin_hint": "https://alpha.example.edu",
			"at": "2026-08-12T09:59:00Z",
		}),
		inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
			"evidence": "warm_verified", "origin_hint": "https://beta.example.edu",
			"at": "2026-08-12T09:59:01Z",
		}),
	)
	var count int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM profile_evidence`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("profile evidence rows = %d, want 2", count)
	}
	var alphaReceipt, betaReceipt string
	if err := jobs.S.DB().QueryRow(`
		SELECT MIN(daemon_received_at), MAX(daemon_received_at)
		FROM profile_evidence`).Scan(&alphaReceipt, &betaReceipt); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{alphaReceipt, betaReceipt} {
		got, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || got.Before(daemonStart) || got.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("daemon receipt %q is not daemon-owned current time: %v", value, err)
		}
	}
}

func TestBridgeProviderOutcomeProjectsTypedGatesAndSignedOutEvidence(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	b.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "bridge-gate-provider", handoffWork(), "alpha")
	if err := b.outcome(context.Background(), jobID, "provider-gate", &protocol.ProviderOutcomePayload{Outcome: "human_auth_required"}); err != nil {
		t.Fatal(err)
	}
	attention, err := jobs.CurrentHumanAttention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if attention.Count != 1 || len(attention.Gates) != 1 || attention.Gates[0].GateType != job.HumanGateLogin {
		t.Fatalf("human attention = %+v, want one login gate", attention)
	}
	var verdict string
	if err := jobs.S.DB().QueryRow(`SELECT verdict FROM profile_evidence LIMIT 1`).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != string(job.ProfileEvidenceSignedOut) {
		t.Fatalf("provider evidence verdict = %q, want signed_out", verdict)
	}
}

// A successful login closes the gate for that episode only. When the session
// later expires, the next decisive signed-out observation must raise a live
// attention surface again; a resolved gate that could never reopen would park
// every sibling with no way for anyone to authenticate.
func TestBridgeResolvedLoginGateReopensOnNextAuthenticationCycle(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	b.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "bridge-gate-reopen", handoffWork(), "alpha")
	liveLoginGates := func() int {
		t.Helper()
		attention, err := jobs.CurrentHumanAttention(ctx)
		if err != nil {
			t.Fatal(err)
		}
		open := 0
		for _, gate := range attention.Gates {
			if gate.GateType == job.HumanGateLogin {
				open++
			}
		}
		return open
	}
	if err := b.outcome(ctx, jobID, "cycle-one", &protocol.ProviderOutcomePayload{Outcome: "human_auth_required"}); err != nil {
		t.Fatal(err)
	}
	if got := liveLoginGates(); got != 1 {
		t.Fatalf("login gates after first signed-out = %d, want 1", got)
	}
	elapsed := int64(1200)
	if err := b.recordAuth(ctx, &protocol.BrowserMessage{
		Type: protocol.MsgAuthReturned, MsgID: "auth-cycle-one", JobID: jobID,
		Payload: &protocol.AuthPayload{ElapsedMS: &elapsed},
	}); err != nil {
		t.Fatal(err)
	}
	if got := liveLoginGates(); got != 0 {
		t.Fatalf("login gates after authentication = %d, want 0", got)
	}
	if err := b.outcome(ctx, jobID, "cycle-two", &protocol.ProviderOutcomePayload{Outcome: "human_auth_required"}); err != nil {
		t.Fatal(err)
	}
	if got := liveLoginGates(); got != 1 {
		t.Fatalf("login gates after session expiry = %d, want 1", got)
	}
}

func TestBridgeEvidenceLostResponseIsIdempotent(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	b.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	runSync(t, b, hello())
	frame := &protocol.SessionEvidencePayload{
		Evidence: "warm_verified", OriginHint: "https://alpha.example.edu",
		At: "2026-08-12T09:59:00Z",
	}
	if err := b.sessionEvidence(context.Background(), frame, "lost-response-msg"); err != nil {
		t.Fatal(err)
	}
	if err := b.sessionEvidence(context.Background(), frame, "lost-response-msg"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM profile_evidence`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("idempotent evidence rows = %d, want 1", count)
	}
}

func TestBridgeEvidenceIsFencedToHolderGenerationAcrossTakeover(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	runSyncAs(t, b, "holder-a", hello())
	runSyncAs(t, b, "holder-a", inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence": "warm_verified", "origin_hint": "https://alpha.example.edu",
		"at": "2026-08-12T09:59:00Z",
	}))
	var firstGeneration int64
	if err := jobs.S.DB().QueryRow(`SELECT browser_holder_generation FROM profile_evidence LIMIT 1`).Scan(&firstGeneration); err != nil {
		t.Fatal(err)
	}
	now = now.Add(sessionStaleAfter + time.Second)
	runSyncAs(t, b, "holder-b", hello())
	if b.epoch == firstGeneration {
		t.Fatalf("holder takeover did not advance generation: %d", firstGeneration)
	}
	current, found, err := jobs.CurrentProfileEvidence(context.Background(), mustProfileID(t, jobs, "alpha"), 1, b.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if found || current.ObservationID != "" {
		t.Fatalf("stale holder evidence crossed generation: %+v found=%v", current, found)
	}
}

func mustProfileID(t *testing.T, jobs *job.Store, name string) string {
	t.Helper()
	profile, err := jobs.InstitutionProfileByConfiguredName(context.Background(), name)
	if err != nil || profile == nil {
		t.Fatalf("profile %q = %+v, %v", name, profile, err)
	}
	return profile.ID
}

func TestBridgeRestartRetainsEvidenceAndTypedAttention(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	b.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "bridge-restart-evidence", handoffWork(), "alpha")
	if err := b.outcome(context.Background(), jobID, "restart-gate", &protocol.ProviderOutcomePayload{Outcome: "human_auth_required"}); err != nil {
		t.Fatal(err)
	}
	restarted := NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
	attention, err := jobs.CurrentHumanAttention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if attention.Count != 1 || len(attention.Gates) != 1 {
		t.Fatalf("attention after bridge restart = %+v, want one durable gate", attention)
	}
	var evidenceCount int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM profile_evidence`).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 1 {
		t.Fatalf("evidence after bridge restart = %d, want 1", evidenceCount)
	}
	if restarted == nil {
		t.Fatal("bridge restart returned nil bridge")
	}
	runSync(t, restarted, hello())
	if err := restarted.recordAuth(context.Background(), &protocol.BrowserMessage{
		Type: protocol.MsgAuthReturned, MsgID: "restart-auth-return", JobID: jobID,
		Payload: &protocol.AuthPayload{},
	}); err != nil {
		t.Fatal(err)
	}
	attention, err = jobs.CurrentHumanAttention(context.Background())
	if err != nil || attention.Count != 0 {
		t.Fatalf("attention after restarted auth return = %+v, err=%v", attention, err)
	}
}

func TestBridgeWarmEvidenceIsExactProfileDespiteSharedAuthClaim(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl", ShibbolethEntityID: "shared-entity"},
		"beta":  {OpenURLBase: "https://beta.example.edu/openurl", ShibbolethEntityID: "shared-entity"},
	}
	b.cfg = cfg
	b.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence": "warm_verified", "origin_hint": "https://alpha.example.edu",
		"at": "2026-08-12T09:59:00Z",
	}))
	alpha := mustProfile(t, jobs, "alpha")
	beta := mustProfile(t, jobs, "beta")
	if alpha.AuthenticationClaimID != beta.AuthenticationClaimID {
		t.Fatalf("test setup claims differ: %q vs %q", alpha.AuthenticationClaimID, beta.AuthenticationClaimID)
	}
	if _, ok, err := jobs.CurrentProfileEvidence(context.Background(), alpha.ID, alpha.Revision, b.epoch); err != nil || !ok {
		t.Fatalf("alpha warm evidence missing: ok=%v err=%v", ok, err)
	}
	if _, ok, err := jobs.CurrentProfileEvidence(context.Background(), beta.ID, beta.Revision, b.epoch); err != nil || ok {
		t.Fatalf("beta inherited alpha warm evidence: ok=%v err=%v", ok, err)
	}
}

func mustProfile(t *testing.T, jobs *job.Store, name string) *job.InstitutionProfile {
	t.Helper()
	profile, err := jobs.InstitutionProfileByConfiguredName(context.Background(), name)
	if err != nil || profile == nil {
		t.Fatalf("profile %q = %+v, %v", name, profile, err)
	}
	return profile
}

func TestBridgeNoAutomaticFirstRouteWithoutExplicitFocus(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	b.cfg = cfg
	runSync(t, b, hello())
	_ = parkInstitutional(t, jobs, "bridge-no-auto-first-route", handoffWork(), "alpha")
	runSync(t, b)
	var candidates, claims int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM browser_candidates`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM materialization_claims`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if candidates != 0 || claims != 0 {
		t.Fatalf("automatic first route created candidates=%d claims=%d without focus", candidates, claims)
	}
}

// TestNegotiatedReadArbitration pins the session boundary for every newly
// negotiated read/hint frame. Reads are safe for a pending current peer, but
// an extension below the protocol floor is refused before it can see an
// unknown frame. Handoff-driving traffic remains holder-only.
func TestNegotiatedReadArbitration(t *testing.T) {
	type request struct {
		name string
		raw  func(*testing.T) json.RawMessage
	}
	requests := []request{
		{"presence", func(t *testing.T) json.RawMessage {
			return inFrame(t, protocol.MsgSurfacePresence, "", protocol.SurfacePresencePayload{
				RequestID: "presence-arb-1", InstanceID: "instance-arb-1", Surface: "popup",
				Focused: true, At: "2026-08-12T10:00:00Z",
			})
		}},
		{"pulse", func(t *testing.T) json.RawMessage {
			return inFrame(t, protocol.MsgWorkPulseRequest, "", protocol.WorkPulseRequestPayload{
				RequestID: "pulse-arb-1", SchemaVersions: []int64{1},
			})
		}},
		{"activity", func(t *testing.T) json.RawMessage {
			return inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
				RequestID: "activity-arb-1", Limit: 1,
			})
		}},
		{"cohort-v2", func(t *testing.T) json.RawMessage {
			return inFrame(t, protocol.MsgPageBulkSubmitV2Request, "", protocol.PageBulkSubmitV2RequestPayload{
				RequestID: "cohort-arb-1", ScanID: "scan-arb-1", CohortID: "cohort-arb-1",
				Source:      protocol.PageBulkSubmitSource{Kind: "browser_page", Origin: "https://reader.example.edu", Detector: "detector/1"},
				CohortTotal: 1, ChunkIndex: 0, FinalChunk: true, CanonicalKeys: []string{"doi:10.1000/arb.1"},
			})
		}},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("holder", func(t *testing.T) {
				b, _, _, _ := newBridge(t)
				runSyncAs(t, b, sessA, helloAs("1.2.3"))
				msgs, _ := runSyncAs(t, b, sessA, tc.raw(t))
				if errMsg := firstOfType(msgs, protocol.MsgError); errMsg != nil &&
					errMsg.Payload.(*protocol.ErrorPayload).Code == "session_busy" {
					t.Fatalf("holder was refused: %v", msgs)
				}
			})
			t.Run("non-holder", func(t *testing.T) {
				b, _, _, _ := newBridge(t)
				runSyncAs(t, b, sessA, helloAs("1.2.3"))
				runSyncAs(t, b, sessB, helloAs("1.2.3"))
				msgs, _ := runSyncAs(t, b, sessB, tc.raw(t))
				if errMsg := firstOfType(msgs, protocol.MsgError); errMsg != nil &&
					errMsg.Payload.(*protocol.ErrorPayload).Code == "session_busy" {
					t.Fatalf("current non-holder read was refused: %v", msgs)
				}
			})
			t.Run("outdated-holder", func(t *testing.T) {
				b, _, _, _ := newBridge(t)
				runSyncAs(t, b, sessA, helloAs("0.0.1"))
				msgs, _ := runSyncAs(t, b, sessA, tc.raw(t))
				errMsg := firstOfType(msgs, protocol.MsgError)
				if errMsg == nil || errMsg.Payload.(*protocol.ErrorPayload).Code != "extension_outdated" {
					t.Fatalf("outdated holder response = %v, want extension_outdated", msgs)
				}
			})
			t.Run("outdated-non-holder", func(t *testing.T) {
				b, _, _, _ := newBridge(t)
				runSyncAs(t, b, sessA, helloAs("1.2.3"))
				runSyncAs(t, b, sessB, helloAs("0.0.1"))
				// The outdated gate is holder-only; pending reads remain admitted.
				msgs, _ := runSyncAs(t, b, sessB, tc.raw(t))
				if errMsg := firstOfType(msgs, protocol.MsgError); errMsg != nil &&
					errMsg.Payload.(*protocol.ErrorPayload).Code == "session_busy" {
					t.Fatalf("outdated non-holder read was refused: %v", msgs)
				}
			})
		})
	}

	// A non-holder cannot drive handoff state even though it can read.
	b, jobs, _, _ := newBridge(t)
	jobID := park(t, jobs, "arbitration-handoff", handoffWork())
	runSyncAs(t, b, sessA, helloAs("1.2.3"))
	runSyncAs(t, b, sessB, helloAs("1.2.3"))
	msgs, _ := runSyncAs(t, b, sessB, inFrame(t, protocol.MsgJobAccept, jobID, map[string]any{}))
	errMsg := firstOfType(msgs, protocol.MsgError)
	// v2 page-bulk keeps the v1 admission class; neither read-only submission
	// path may be turned into a holder-only handoff by accident.
	b, _, _, _ = newBridge(t)
	runSyncAs(t, b, sessA, helloAs("1.2.3"))
	runSyncAs(t, b, sessB, helloAs("1.2.3"))
	v1 := inFrame(t, protocol.MsgPageBulkSubmitRequest, "", protocol.PageBulkSubmitRequestPayload{
		RequestID: "bulk-v1-admission", ScanID: "scan-v1-admission",
		Source:        protocol.PageBulkSubmitSource{Kind: "browser_page", Origin: "https://reader.example.edu", Detector: "detector/1"},
		CanonicalKeys: []string{"doi:10.1000/v1-admission"},
	})
	v2 := requests[3].raw(t)
	for name, frame := range map[string]json.RawMessage{"v1": v1, "v2": v2} {
		msgs, _ := runSyncAs(t, b, sessB, frame)
		if errMsg := firstOfType(msgs, protocol.MsgError); errMsg != nil &&
			errMsg.Payload.(*protocol.ErrorPayload).Code == "session_busy" {
			t.Fatalf("%s page-bulk request was holder-gated: %v", name, msgs)
		}
	}

	if errMsg == nil || errMsg.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("non-holder handoff response = %v, want session_busy", msgs)
	}
}

func TestSurfacePresenceLeaseContract(t *testing.T) {
	b, _, _, _ := newBridge(t)
	advance := settableClock(b)
	runSync(t, b, hello())
	send := func(id string, focused bool, at string) *protocol.SurfacePresenceAckPayload {
		t.Helper()
		msgs, _ := runSync(t, b, inFrame(t, protocol.MsgSurfacePresence, "", protocol.SurfacePresencePayload{
			RequestID: "presence-" + id, InstanceID: id, Surface: "popup", Focused: focused, At: at,
		}))
		ack := firstOfType(msgs, protocol.MsgSurfacePresenceAck)
		if ack == nil {
			t.Fatalf("presence ack missing: %v", msgs)
		}
		return ack.Payload.(*protocol.SurfacePresenceAckPayload)
	}
	if !send("instance-one-0001", true, "2026-08-12T10:00:00Z").Accepted || !b.AnyFocused(b.now()) {
		t.Fatal("focused presence was not acknowledged and reflected")
	}
	advance(surfacePresenceTTL - time.Second)
	if !b.AnyFocused(b.now()) {
		t.Fatal("lease unexpectedly expired before 120 seconds")
	}
	advance(2 * time.Second)
	if b.AnyFocused(b.now()) {
		t.Fatal("lease used client at timestamp instead of daemon receipt time")
	}
	send("instance-one-0001", true, "2099-01-01T00:00:00Z")
	send("instance-one-0001", false, "2099-01-01T00:00:00Z")
	if b.AnyFocused(b.now()) {
		t.Fatal("focused:false did not release the instance lease")
	}
	send("instance-one-0001", true, "2026-08-12T10:02:00Z")
	send("instance-two-0002", true, "2026-08-12T10:02:00Z")
	send("instance-one-0001", false, "2026-08-12T10:02:00Z")
	if !b.AnyFocused(b.now()) {
		t.Fatal("one focused instance was incorrectly cleared by another release")
	}
	send("instance-two-0002", false, "2026-08-12T10:02:00Z")
	if b.AnyFocused(b.now()) {
		t.Fatal("all focused instances were released, but AnyFocused stayed true")
	}

	bounded, _, _, _ := newBridge(t)
	runSync(t, bounded, hello())
	// The cap is per-frame bookkeeping, not per-Sync, and none of these frames
	// depends on the clock moving between them — so they ride in batches. One
	// Sync per frame cost ~280 round trips here (and ~1,000 across this test,
	// 18s of the package under -race) to make a claim about 256 leases. Batches
	// stay small because one response must still fit ipc.MaxResultBytes.
	const presenceBatch = 32
	overCap := maxPresenceLeases + 20
	for start := 0; start < overCap; start += presenceBatch {
		frames := make([]json.RawMessage, 0, presenceBatch)
		for i := start; i < min(start+presenceBatch, overCap); i++ {
			frames = append(frames, inFrame(t, protocol.MsgSurfacePresence, "", protocol.SurfacePresencePayload{
				RequestID: fmt.Sprintf("presence-bounded-%03d", i), InstanceID: fmt.Sprintf("bounded-%03d", i),
				Surface: "popup", Focused: false, At: "2026-08-12T10:02:00Z",
			}))
		}
		msgs, _ := runSync(t, bounded, frames...)
		acks := 0
		for _, msg := range msgs {
			if msg.Type == protocol.MsgSurfacePresenceAck {
				acks++
			}
		}
		if acks != len(frames) {
			t.Fatalf("bounded presence acks = %d for %d frames: %v", acks, len(frames), msgs)
		}
	}
	bounded.mu.Lock()
	leaseCount, orderCount := len(bounded.presence), len(bounded.presenceOrder)
	bounded.mu.Unlock()
	if leaseCount > maxPresenceLeases || orderCount > maxPresenceLeases || orderCount != leaseCount {
		t.Fatalf("presence lease bookkeeping = map:%d order:%d, want both <= %d and equal", leaseCount, orderCount, maxPresenceLeases)
	}

	churn, _, _, _ := newBridge(t)
	churnAdvance := settableClock(churn)
	runSync(t, churn, hello())
	// This loop cannot batch: expiring each lease before the next arrives is
	// the whole claim, and one Sync reads the clock once. It does not need
	// three times the cap to make that claim either — an implementation that
	// failed to prune would be caught on the second pass, and exceeding the cap
	// covers any interaction with eviction.
	for i := range maxPresenceLeases + 8 {
		id := fmt.Sprintf("churn-%03d", i)
		msgs, _ := runSync(t, churn, inFrame(t, protocol.MsgSurfacePresence, "", protocol.SurfacePresencePayload{
			RequestID: "presence-" + id, InstanceID: id, Surface: "popup", Focused: false, At: "2026-08-12T10:02:00Z",
		}))
		if firstOfType(msgs, protocol.MsgSurfacePresenceAck) == nil {
			t.Fatalf("churn presence ack missing: %v", msgs)
		}
		churnAdvance(surfacePresenceTTL + time.Second)
	}
	churn.mu.Lock()
	churnMap, churnOrder := len(churn.presence), len(churn.presenceOrder)
	churn.mu.Unlock()
	if churnMap > maxPresenceLeases || churnOrder > maxPresenceLeases || churnMap != churnOrder {
		t.Fatalf("expired presence churn grew bookkeeping = map:%d order:%d, cap %d", churnMap, churnOrder, maxPresenceLeases)
	}
	if churnMap != 1 {
		t.Fatalf("expected only latest lease after expiry churn, got %d", churnMap)
	}
}

func TestSurfacePresenceRejectsPrivateFields(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSync(t, b, hello())
	raw := inFrame(t, protocol.MsgSurfacePresence, "", map[string]any{
		"request_id": "presence-private-1", "instance_id": "private-instance-1",
		"surface": "popup", "focused": true, "at": "2026-08-12T10:00:00Z",
		"url": "https://private.example/article", "title": "Private title", "tab_id": 7,
		"host": "private.example", "identifier": "doi:10.1000/private", "content": "secret",
	})
	if _, err := b.Sync(context.Background(), testSessionID, false, []json.RawMessage{raw}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("presence with private fields error = %v, want ErrInvalidFrame", err)
	}
}

func TestWorkPulseHandlerValidatesAndDegradesStructurally(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	b.SetPulseService(&pulse.Service{Jobs: jobs, Now: b.now})
	runSync(t, b, hello())
	msgs, raw := runSync(t, b, inFrame(t, protocol.MsgWorkPulseRequest, "", protocol.WorkPulseRequestPayload{
		RequestID: "pulse-healthy-1", SchemaVersions: []int64{1},
	}))
	response := firstOfType(msgs, protocol.MsgWorkPulseResponse)
	if response == nil || len(raw) == 0 {
		t.Fatalf("healthy pulse response = %v", msgs)
	}
	if got := response.Payload.(*protocol.WorkPulseResponsePayload); got.RequestID != "pulse-healthy-1" || got.Schema != 1 {
		t.Fatalf("healthy pulse payload = %+v", response.Payload)
	}
	b.SetPulseService(nil)
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgWorkPulseRequest, "", protocol.WorkPulseRequestPayload{
		RequestID: "pulse-failed-1", SchemaVersions: []int64{1},
	}))
	errMsg := firstOfType(msgs, protocol.MsgError)
	if errMsg == nil {
		types := make([]string, len(msgs))
		for i, msg := range msgs {
			types[i] = msg.Type
		}
		t.Fatalf("pulse failure response types=%v, want structured pulse_unavailable", types)
	}
	errPayload := errMsg.Payload.(*protocol.ErrorPayload)
	if errPayload.Code != "pulse_unavailable" || errPayload.RequestID != "pulse-failed-1" {
		t.Fatalf("pulse failure payload = %+v, want correlated pulse-failed-1", errPayload)
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgActivityRequest, "", protocol.ActivityRequestPayload{
		RequestID: "pulse-following-old-activity", Limit: 1,
	}))
	if firstOfType(msgs, protocol.MsgActivityResponse) == nil {
		t.Fatalf("session did not survive pulse failure: %v", msgs)
	}
}

func TestWorkPulseEffectPermitDetailsRequirePermitFeature(t *testing.T) {
	for _, tc := range []struct {
		name      string
		features  []string
		wantNamed bool
	}{
		{name: "featureless peer", wantNamed: false},
		{name: "permit peer", features: []string{effectPermitFeature}, wantNamed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			id := park(t, jobs, "wr_pulse_permit_"+strings.ReplaceAll(tc.name, " ", "_"), handoffWork())
			attemptRevision, err := jobs.MaterializationAttemptRevision(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			permit, _, err := jobs.AcquireEffectPermit(context.Background(), job.EffectPermitAcquireInput{
				Identity: job.EffectPermitIdentity{
					JobID: id, Kind: job.EffectKindGenericDrive,
					DriveAttemptID: "pulse-permit-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
				},
				JobAttemptRevision: attemptRevision, BrowserHolderGeneration: 1,
				SafetyDomainID: "pulse-permit-domain", LeaseUntil: b.now().Add(time.Minute),
				Authorization: job.EffectPermitEvent{Kind: "browser.provider_drive_epoch_started", Detail: map[string]any{
					"drive_attempt_id": "pulse-permit-attempt", "ordinal": int64(0), "strategy": "generic",
					"revision": "1", "safety_domain": "pulse-permit-domain",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			b.SetPulseService(&pulse.Service{Jobs: jobs, EffectLimit: 1, Now: b.now})
			runSync(t, b, helloWithFeatures(t, "0.14.0", tc.features...))
			msgs, _ := runSync(t, b, inFrame(t, protocol.MsgWorkPulseRequest, "", protocol.WorkPulseRequestPayload{
				RequestID: "pulse-permit-details", SchemaVersions: []int64{1},
			}))
			response := firstOfType(msgs, protocol.MsgWorkPulseResponse)
			if response == nil {
				t.Fatalf("pulse response missing: %v", msgs)
			}
			got := response.Payload.(*protocol.WorkPulseResponsePayload).EffectPermits
			if tc.wantNamed {
				if len(got) != 1 || got[0].PermitID != permit.ID {
					t.Fatalf("effect permits = %+v, want %s", got, permit.ID)
				}
			} else if len(got) != 0 {
				t.Fatalf("featureless peer received additive effect permits: %+v", got)
			}
		})
	}
}
func TestActivityPageContractAndLegacyActivity(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	for i := range 60 {
		appendEventAt(t, jobs, "", "test.activity", map[string]any{"n": i}, base.Add(time.Duration(i)*time.Second))
	}
	if _, err := jobs.S.DB().Exec(`DELETE FROM events WHERE seq <= 5`); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
		RequestID: "activity-page-1", Limit: 50,
	}))
	page := firstOfType(msgs, protocol.MsgActivityPageResponse)
	if page == nil {
		t.Fatalf("activity page missing: %v", msgs)
	}
	p := page.Payload.(*protocol.ActivityPageResponsePayload)
	before := p.Cursor
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
		RequestID: "activity-page-2", Limit: 50, BeforeSeq: before,
	}))
	next := firstOfType(msgs, protocol.MsgActivityPageResponse).Payload.(*protocol.ActivityPageResponsePayload)
	if len(next.Entries) != 5 || next.HasMore || next.Cursor != "" || next.Entries[0].Seq >= p.Entries[len(p.Entries)-1].Seq {
		t.Fatalf("second activity page = %+v", next)
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
		RequestID: "activity-count", Limit: 1, SeenThroughSeq: "10",
	}))
	counted := firstOfType(msgs, protocol.MsgActivityPageResponse).Payload.(*protocol.ActivityPageResponsePayload)
	if counted.NewCountSince == nil || *counted.NewCountSince != 50 || counted.Gap != nil {
		t.Fatalf("new_count_since = %+v gap=%v, want 50 and no gap", counted.NewCountSince, counted.Gap)
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
		RequestID: "activity-gap", Limit: 1, SeenThroughSeq: "1",
	}))
	gap := firstOfType(msgs, protocol.MsgActivityPageResponse).Payload.(*protocol.ActivityPageResponsePayload)
	if gap.Gap == nil || !*gap.Gap || gap.NewCountSince != nil {
		t.Fatalf("retained-history gap = %+v", gap)
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgActivityRequest, "", protocol.ActivityRequestPayload{
		RequestID: "legacy-activity", Limit: 1,
	}))
	if firstOfType(msgs, protocol.MsgActivityResponse) == nil {
		t.Fatalf("legacy activity response missing: %v", msgs)
	}
}
func TestActivityPageReadFailureIsUnavailable(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())
	if _, err := jobs.S.DB().Exec(`DROP TABLE events`); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgActivityPageRequest, "", protocol.ActivityPageRequestPayload{
		RequestID: "activity-failed-1", Limit: 10, SeenThroughSeq: "1",
	}))
	errMsg := firstOfType(msgs, protocol.MsgError)
	if errMsg == nil {
		t.Fatalf("activity read failure = %v, want structured unavailable", msgs)
	}
	errPayload := errMsg.Payload.(*protocol.ErrorPayload)
	if errPayload.Code != "activity_page_unavailable" || errPayload.RequestID != "activity-failed-1" {
		t.Fatalf("activity read failure payload = %+v, want correlated unavailable", errPayload)
	}
	if firstOfType(msgs, protocol.MsgActivityPageResponse) != nil {
		t.Fatalf("activity read failure fabricated a page: %v", msgs)
	}
}

func TestPageBulkSubmitV2PersistsReplayAndConflict(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())
	req := protocol.PageBulkSubmitV2RequestPayload{
		RequestID: "cohort-submit-1", ScanID: "scan-submit-1", CohortID: "cohort-submit-1",
		Source:      protocol.PageBulkSubmitSource{Kind: "browser_page", Origin: "https://reader.example.edu", Detector: "detector/1"},
		CohortTotal: 1, ChunkIndex: 0, FinalChunk: true, CanonicalKeys: []string{"doi:10.1000/cohort.1"},
	}
	frame := inFrame(t, protocol.MsgPageBulkSubmitV2Request, "", req)
	msgs, _ := runSync(t, b, frame)
	result := firstOfType(msgs, protocol.MsgPageBulkSubmitV2Result)
	if result == nil {
		t.Fatalf("cohort result missing: %v", msgs)
	}
	first := *result.Payload.(*protocol.PageBulkSubmitV2ResultPayload)
	if first.Submitted != 1 || first.PersistedMembers != 1 || first.Membership != "complete" || first.CohortTotal == nil || *first.CohortTotal != 1 {
		t.Fatalf("first cohort result = %+v", first)
	}
	var jobsBefore, membersBefore int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM acquisition_batch_members`).Scan(&membersBefore); err != nil {
		t.Fatal(err)
	}
	msgs, _ = runSync(t, b, frame)
	replay := firstOfType(msgs, protocol.MsgPageBulkSubmitV2Result)
	if replay == nil || !reflect.DeepEqual(first, *replay.Payload.(*protocol.PageBulkSubmitV2ResultPayload)) {
		t.Fatalf("replay = %v, want identical cached result %+v", msgs, first)
	}
	var jobsAfter, membersAfter int
	_ = jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter)

	req.CanonicalKeys = []string{"doi:10.1000/conflict.1"}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgPageBulkSubmitV2Request, "", req))
	errMsg := firstOfType(msgs, protocol.MsgError)
	if errMsg == nil {
		t.Fatalf("conflicting replay = %v, want page_bulk_cohort_conflict", msgs)
	}
	errPayload := errMsg.Payload.(*protocol.ErrorPayload)
	if errPayload.Code != "page_bulk_cohort_conflict" || errPayload.RequestID != req.RequestID {
		t.Fatalf("conflicting replay error = %+v, want correlated request id %q", errPayload, req.RequestID)
	}
	_ = jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter)
	_ = jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM acquisition_batch_members`).Scan(&membersAfter)
	if jobsAfter != jobsBefore || membersAfter != membersBefore {
		t.Fatalf("conflict mutated domain: jobs %d/%d members %d/%d", jobsAfter, jobsBefore, membersAfter, membersBefore)
	}
}
func mappedFamilyJob(t *testing.T, jobs *job.Store, reqID string) string {
	t.Helper()
	id := park(t, jobs, reqID, handoffWork())
	if _, err := jobs.S.DB().Exec(
		`UPDATE human_actions SET kind = 'manual_download', detail = 'download manually', requires_auth = 0, blocked_by = 'landing_page' WHERE job_id = ?`,
		id,
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCountsV3AndSnapshotV5FamilyAgreement(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	mappedFamilyJob(t, jobs, "family-agreement")
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageCountsRequest, "", protocol.TriageCountsRequestPayload{
		RequestID: "family-counts-3", SchemaVersions: []int64{3},
	}))
	countsMsg := firstOfType(msgs, protocol.MsgTriageCountsResponse)
	if countsMsg == nil {
		t.Fatalf("counts v3 missing: %v", msgs)
	}
	counts := countsMsg.Payload.(*protocol.TriageCountsResponsePayload).Counts
	if counts.TurnsRequired == nil || counts.TurnsWorking == nil || counts.FamilyBreakdownComplete == nil || !*counts.FamilyBreakdownComplete {
		t.Fatalf("counts v3 family projection incomplete: %+v", counts)
	}
	msgs, _ = runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "", protocol.TriageSnapshotRequestPayload{
		RequestID: "family-snapshot-5", SchemaVersions: []int64{5}, Limit: 50,
	}))
	snapshotMsg := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	if snapshotMsg == nil {
		t.Fatalf("snapshot v5 missing: %v", msgs)
	}
	snapshot := snapshotMsg.Payload.(*protocol.TriageSnapshotResponsePayload)
	byRun := map[string]int{}
	for _, item := range snapshot.Items {
		if item.RunKey == "" {
			t.Fatalf("snapshot item lacks run_key: %+v", item)
		}
		byRun[item.RunKey]++
	}
	countedRuns := map[string]int{}
	for _, run := range counts.FamilyRuns {
		countedRuns[run.RunKey] = int(run.Count)
	}
	if !reflect.DeepEqual(byRun, countedRuns) {
		t.Fatalf("counts/snapshot family runs disagree: rows=%v runs=%v", byRun, countedRuns)
	}
	total := int64(0)
	for _, run := range counts.FamilyRuns {
		total += run.Count
	}
	if total != *counts.TurnsRequired+*counts.TurnsWorking {
		t.Fatalf("family run total=%d, want turns_required+turns_working=%d", total, *counts.TurnsRequired+*counts.TurnsWorking)
	}
}

func TestTriageNegotiatedSchemaFieldsAreProducerGated(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	mappedFamilyJob(t, jobs, "schema-gating")
	runSync(t, b, hello())
	requestCounts := func(schema int64) (protocol.TriageCounts, json.RawMessage) {
		msgs, raw := runSync(t, b, inFrame(t, protocol.MsgTriageCountsRequest, "", protocol.TriageCountsRequestPayload{
			RequestID: fmt.Sprintf("schema-counts-%d", schema), SchemaVersions: []int64{schema},
		}))
		msg := firstOfType(msgs, protocol.MsgTriageCountsResponse)
		if msg == nil {
			t.Fatalf("counts schema %d missing: %v", schema, msgs)
		}
		for _, frame := range raw {
			var env struct {
				Type    string `json:"type"`
				Payload struct {
					Counts map[string]json.RawMessage `json:"counts"`
				} `json:"payload"`
			}
			if json.Unmarshal(frame, &env) == nil && env.Type == protocol.MsgTriageCountsResponse {
				data, _ := json.Marshal(env.Payload.Counts)
				return msg.Payload.(*protocol.TriageCountsResponsePayload).Counts, data
			}
		}
		t.Fatalf("counts schema %d raw frame missing", schema)
		return protocol.TriageCounts{}, nil
	}
	v1, v1Raw := requestCounts(1)
	v2, v2Raw := requestCounts(2)
	v3, v3Raw := requestCounts(3)
	for name, raw := range map[string]json.RawMessage{"v1": v1Raw, "v2": v2Raw, "v3": v3Raw} {
		for _, field := range []string{"turns_required", "turns_working", "family_breakdown_complete", "family_runs", "required_turns_complete", "required_turns"} {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			_, present := payload[field]
			if name != "v3" && present {
				t.Fatalf("%s carried v3 field %q", name, field)
			}
			if name == "v3" && !present {
				t.Fatalf("v3 omitted negotiated field %q", field)
			}
		}
	}
	if v1.ActionsRequiresAuth != nil || v2.ActionsRequiresAuth == nil || v3.ActionsRequiresAuth != nil {
		t.Fatalf("actions_requires_auth schema gating = v1:%v v2:%v v3:%v", v1.ActionsRequiresAuth, v2.ActionsRequiresAuth, v3.ActionsRequiresAuth)
	}
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "", protocol.TriageSnapshotRequestPayload{
		RequestID: "schema-snapshot-4", SchemaVersions: []int64{4}, Limit: 50,
	}))
	v4 := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	msgs, raw := runSync(t, b, inFrame(t, protocol.MsgTriageSnapshotRequest, "", protocol.TriageSnapshotRequestPayload{
		RequestID: "schema-snapshot-5", SchemaVersions: []int64{5}, Limit: 50,
	}))
	v5 := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	if v4 == nil || v5 == nil {
		t.Fatalf("snapshot schema responses = v4:%v v5:%v", v4, v5)
	}
	for _, item := range v4.Payload.(*protocol.TriageSnapshotResponsePayload).Items {
		if item.RunKey != "" || item.NextActor != "" || item.GuidanceVariant != "" || item.OperationVariant != "" {
			t.Fatalf("schema 4 carried schema-5 quartet: %+v", item)
		}
	}
	seenQuartet := false
	for _, item := range v5.Payload.(*protocol.TriageSnapshotResponsePayload).Items {
		if item.RunKey != "" && item.NextActor != "" && item.GuidanceVariant != "" && item.OperationVariant != "" {
			seenQuartet = true
		}
	}
	if !seenQuartet {
		t.Fatalf("schema 5 carried no row quartet: %v", raw)
	}
}

func TestNewSolicitedResponsesFitResultCap(t *testing.T) {
	b := &Bridge{}
	stalls := make([]protocol.WorkPulseStallEpisode, 16)
	gates := make([]protocol.WorkPulseGate, 16)
	for i := range stalls {
		stalls[i] = protocol.WorkPulseStallEpisode{
			EpisodeKey: fmt.Sprintf("episode-%02d", i), CauseKind: "execution_lease_overdue",
			PublicLabel: strings.Repeat("s", 64), Since: "2026-08-12T00:00:00Z", Count: 1,
		}
		gates[i] = protocol.WorkPulseGate{Kind: "source_budget", Source: fmt.Sprintf("source-%02d", i), Until: "2026-08-12T23:59:59Z", Count: 1}
	}
	pulseFrame, err := b.frame(protocol.MsgWorkPulseResponse, "", protocol.WorkPulseResponsePayload{
		RequestID: "pulse-cap", Schema: 1, GeneratedAt: "2026-08-12T00:00:00Z",
		StallEpisodes: stalls, Gates: gates, StallEpisodesTruncated: new(bool), GatesTruncated: new(bool),
		EffectCapacity:       &protocol.WorkPulseCapacity{Busy: 1, Limit: 1, Waiting: 100},
		HumanSurfaceCapacity: &protocol.WorkPulseHumanSurfaceCapacity{Busy: 1, Limit: 1, WaitingClaims: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]protocol.ActivityEntryPayload, 50)
	for i := range entries {
		// 160 runes is the real worst case: the daemon clamps every Activity
		// text to that bound (store.clampActivityText) and the wire enforces it,
		// so a longer fixture would test a frame no producer can emit.
		entries[i] = protocol.ActivityEntryPayload{Seq: int64(i + 1), At: "2026-08-12T00:00:00Z", Kind: "test", Text: strings.Repeat("a", 160)}
	}
	newCount := int64(1_000_000)
	activityFrame, err := b.frame(protocol.MsgActivityPageResponse, "", protocol.ActivityPageResponsePayload{
		RequestID: "activity-cap", GeneratedAt: "2026-08-12T00:00:00Z", Entries: entries, HasMore: true, Cursor: "1", LatestSeq: 50,
		NewCountSince: &newCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The true worst case is a full breakdown: 128 family runs plus the 1024-turn
	// ceiling, with the counts algebra satisfied (runs sum to required+working).
	runs := make([]protocol.TriageFamilyRun, 128)
	for i := range runs {
		runs[i] = protocol.TriageFamilyRun{RunKey: fmt.Sprintf("fr1_%028x", i), FirstRank: int64(i),
			RouteClass: "manual_download", ActionKind: "manual_download", NextActor: "researcher",
			GuidanceVariant: "manual_download", OperationVariant: "dismiss_only", Count: 8}
	}
	turns := make([]protocol.TriageRequiredTurn, 1024)
	for i := range turns {
		actionID := int64(i + 1)
		turns[i] = protocol.TriageRequiredTurn{
			ItemID: fmt.Sprintf("action:%d", i+1), ItemKind: "human_action", ActionID: &actionID,
			JobID: fmt.Sprintf("job_%026d", i+1), RouteClass: "manual_download",
		}
	}
	required, working := int64(1024), int64(0)
	complete := true
	countsFrame, err := b.frame(protocol.MsgTriageCountsResponse, "", protocol.TriageCountsResponsePayload{
		RequestID: "counts-cap", Counts: protocol.TriageCounts{
			PendingTotal: 0, TurnsRequired: &required, TurnsWorking: &working,
			FamilyBreakdownComplete: &complete, FamilyRuns: runs,
			RequiredTurnsComplete: &complete, RequiredTurns: turns,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, frame := range map[string]json.RawMessage{"pulse": pulseFrame, "activity": activityFrame, "counts": countsFrame} {
		if len(frame) > ipc.MaxResultBytes {
			t.Fatalf("%s frame=%d exceeds ipc.MaxResultBytes=%d", name, len(frame), ipc.MaxResultBytes)
		}
	}

	maxSolicited := max(len(pulseFrame), len(activityFrame), len(countsFrame))
	batched := (maxOutstandingOffers + maxFocusFramesPerPoll) * len(pulseFrame)
	if maxSolicited+batched > ipc.MaxResultBytes {
		t.Fatalf("new solicited response plus bounded batches=%d exceeds ipc.MaxResultBytes=%d", maxSolicited+batched, ipc.MaxResultBytes)
	}
}
func TestOldPeerReceivesNoNewUnsolicitedFrames(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.13.0"))
	for _, msg := range msgs {
		switch msg.Type {
		case protocol.MsgSurfacePresenceAck, protocol.MsgWorkPulseResponse, protocol.MsgActivityPageResponse,
			protocol.MsgPageBulkSubmitV2Result:
			t.Fatalf("old peer received unsolicited new frame %q: %v", msg.Type, msgs)
		}
	}
	msgs, _ = runSyncAs(t, b, sessA, inFrame(t, protocol.MsgTriageCountsRequest, "", protocol.TriageCountsRequestPayload{
		RequestID: "old-peer-counts", SchemaVersions: []int64{1},
	}))
	counts := firstOfType(msgs, protocol.MsgTriageCountsResponse)
	if counts == nil {
		t.Fatalf("old peer counts response missing: %v", msgs)
	}
	raw, _ := json.Marshal(counts.Payload.(*protocol.TriageCountsResponsePayload).Counts)
	for _, field := range []string{"turns_required", "family_runs", "required_turns"} {
		if bytes.Contains(raw, []byte(`"`+field+`"`)) {
			t.Fatalf("old peer legacy counts carried %q: %s", field, raw)
		}
	}
	msgs, _ = runSyncAs(t, b, sessA, inFrame(t, protocol.MsgTriageSnapshotRequest, "", protocol.TriageSnapshotRequestPayload{
		RequestID: "old-peer-snapshot", SchemaVersions: []int64{4}, Limit: 1,
	}))
	snapshot := firstOfType(msgs, protocol.MsgTriageSnapshotResponse)
	if snapshot == nil || snapshot.Payload.(*protocol.TriageSnapshotResponsePayload).Schema != 4 {
		t.Fatalf("old peer schema fallback = %v", msgs)
	}
}
func TestProviderDriveEffectPermitFeaturelessPeerIsUnsupported(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "permit-featureless", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-featureless-attempt", "domain-featureless")
	b.holder = &browserSession{ID: "featureless", ExtensionVersion: "0.14.0", LastSyncAt: b.now()}
	frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "permit-featureless-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := permitOutcome(t, frames); got != "unsupported" {
		t.Fatalf("featureless start = %q, want unsupported", got)
	}
	live, err := jobs.LiveEffectPermit(context.Background())
	if err != nil || live != nil {
		t.Fatalf("featureless peer acquired permit=%+v err=%v", live, err)
	}
}

func TestProviderDriveEffectPermitStartDuplicateAndAgeDoNotSupersede(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-start-duplicate", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-start-attempt", "domain-start")
	start := &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "permit-start-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	if got, err := b.providerDriveEpochStart(context.Background(), id, start); err != nil || permitOutcome(t, got) != "started" {
		t.Fatalf("first start err=%v frames=%v", err, got)
	}
	again, err := b.providerDriveEpochStart(context.Background(), id, start)
	if err != nil || permitOutcome(t, again) != "started" {
		t.Fatalf("exact held replay err=%v frames=%v", err, again)
	}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "permit-start-attempt", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "not_pdf",
	}); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("settle after replay err=%v frames=%v", err, frames)
	}
	closed, err := b.providerDriveEpochStart(context.Background(), id, start)
	if err != nil || permitOutcome(t, closed) != "duplicate" {
		t.Fatalf("settled replay err=%v frames=%v", err, closed)
	}
	advance := settableClock(b)
	advance(11 * time.Minute)
	if b.driveEpochStalled(id, "permit-start-attempt", 0) {
		t.Fatal("elapsed time reported a stalled effect")
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	action := openHandoffAction(t, jobs, id)
	offer, err := b.offerAtURL(*row, action, config.ModeDelegated, "", false)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(offer)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID; got != "permit-start-attempt" {
		t.Fatalf("settled identity reoffer attempt=%q, want exact original tuple", got)
	}
}

func TestProviderDriveEffectPermitBusyStaleThenSameIdentityReoffers(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	firstID := park(t, jobs, "permit-busy-first", handoffWork())
	secondID := park(t, jobs, "permit-busy-second", handoffWork())
	effectPermitOffer(t, jobs, firstID, "permit-busy-first-attempt", "domain-busy")
	effectPermitOffer(t, jobs, secondID, "permit-busy-second-attempt", "domain-other")
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "permit-busy-first-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"}
	if frames, err := b.providerDriveEpochStart(context.Background(), firstID, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("first start err=%v frames=%v", err, frames)
	}
	busy, err := b.providerDriveEpochStart(context.Background(), secondID, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "permit-busy-second-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil || permitOutcome(t, busy) != "stale" {
		t.Fatalf("busy start err=%v frames=%v", err, busy)
	}
	if _, _, err := jobs.SettleEffectPermit(context.Background(), job.EffectPermitSettleInput{
		Identity:       job.EffectPermitIdentity{JobID: firstID, Kind: job.EffectKindGenericDrive, DriveAttemptID: "permit-busy-first-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"},
		RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.provider_drive_epoch_result", Detail: map[string]any{"drive_attempt_id": "permit-busy-first-attempt", "ordinal": int64(0), "strategy": "generic", "revision": "1", "outcome": "applied", "safety_domain": "domain-busy"}}},
	}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), secondID)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := b.offerAtURL(*row, openHandoffAction(t, jobs, secondID), config.ModeDelegated, "", false)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.DecodeBrowserMessage(offer)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Payload.(*protocol.JobOfferPayload).DriveAttemptID; got != "permit-busy-second-attempt" {
		t.Fatalf("busy retry minted %q, want same identity", got)
	}
	if frames, err := b.providerDriveEpochStart(context.Background(), secondID, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "permit-busy-second-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("retry start err=%v frames=%v", err, frames)
	}
}

func TestProviderDriveEffectPermitUnknownAndHistoricalResultsCleanupOnly(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-unknown", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-unknown-attempt", "domain-unknown")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "permit-unknown-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	p, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: "permit-unknown-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"})
	if err != nil || p == nil {
		t.Fatalf("permit=%+v err=%v", p, err)
	}
	if _, err := jobs.ReconcileEffectPermit(context.Background(), job.EffectPermitObservation{PermitID: p.ID, BrowserHolderGeneration: 0}); err != nil {
		t.Fatal(err)
	}
	// Unknown completion for current holder may still append its legitimate successor.
	result, err := b.providerDriveEpochResult(context.Background(), id, &protocol.ProviderDriveEpochResultRequestPayload{DriveAttemptID: "permit-unknown-attempt", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "not_pdf"})
	if err != nil || permitOutcome(t, result) != "applied" {
		t.Fatalf("unknown result err=%v frames=%v", err, result)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	successor := 0
	for _, event := range events {
		if event["kind"] == "browser.provider_drive_epoch_offered" {
			detail, _ := event["detail"].(map[string]any)
			if intDetail(detail, "ordinal") == 1 {
				successor++
			}
		}
	}
	if successor != 1 {
		t.Fatalf("unknown current result successor count=%d, want 1", successor)
	}
}
func TestProviderDriveEffectPermitHistoricalResultIsCleanupOnly(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-historical", handoffWork())
	identity := job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: "permit-historical-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"}
	_, outcome, err := jobs.AcquireEffectPermit(context.Background(), job.EffectPermitAcquireInput{
		Identity: identity, JobAttemptRevision: 1, BrowserHolderGeneration: 99,
		SafetyDomainID: "domain-historical", LeaseUntil: time.Now().Add(time.Hour),
		Authorization: job.EffectPermitEvent{Kind: "browser.provider_drive_epoch_started", Detail: map[string]any{
			"drive_attempt_id": identity.DriveAttemptID, "ordinal": int64(0), "strategy": "generic", "revision": "1", "safety_domain": "domain-historical",
		}},
	})
	if err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("historical acquire outcome=%v err=%v", outcome, err)
	}
	frames, err := b.providerDriveEpochResult(context.Background(), id, &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: identity.DriveAttemptID, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "not_pdf",
	})
	if err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("historical result err=%v frames=%v", err, frames)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == providerLatchEventKind {
			t.Fatalf("historical result created latch: %#v", event)
		}
		if event["kind"] == "browser.provider_drive_epoch_offered" {
			detail, _ := event["detail"].(map[string]any)
			if intDetail(detail, "ordinal") == 1 {
				t.Fatalf("historical result minted successor: %#v", event)
			}
		}
	}
}

func TestProviderDriveEffectPermitDuplicateResultRepairsRequiredEvents(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-repair", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-repair-attempt", "domain-repair")
	start := &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: "permit-repair-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"}
	if frames, err := b.providerDriveEpochStart(context.Background(), id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	result := &protocol.ProviderDriveEpochResultRequestPayload{DriveAttemptID: "permit-repair-attempt", Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "not_pdf", Detail: "validation failed"}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, result); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("first result err=%v frames=%v", err, frames)
	}
	if _, err := jobs.S.DB().ExecContext(context.Background(), `DELETE FROM events WHERE job_id=? AND kind IN (?,?)`, id, providerLatchEventKind, "browser.provider_drive_epoch_offered"); err != nil {
		t.Fatal(err)
	}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, result); err != nil || permitOutcome(t, frames) != "duplicate" {
		t.Fatalf("duplicate result err=%v frames=%v", err, frames)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	resultCount, latchCount, successorCount := 0, 0, 0
	for _, event := range events {
		switch event["kind"] {
		case "browser.provider_drive_epoch_result":
			resultCount++
		case providerLatchEventKind:
			latchCount++
		case "browser.provider_drive_epoch_offered":
			detail, _ := event["detail"].(map[string]any)
			if intDetail(detail, "ordinal") == 1 {
				successorCount++
			}
		}
	}
	if resultCount != 1 || latchCount != 0 || successorCount != 0 {
		t.Fatalf("historical duplicate repaired current-only events: result=%d latch=%d successor=%d", resultCount, latchCount, successorCount)
	}
}

func TestProviderDriveEffectPermitConflictingDuplicateUsesDurableResult(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-conflicting-duplicate", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-conflicting-attempt", "domain-conflicting")
	start := &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "permit-conflicting-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	if frames, err := b.providerDriveEpochStart(context.Background(), id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	first := &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "permit-conflicting-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "success",
	}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, first); err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("first result err=%v frames=%v", err, frames)
	}
	conflicting := &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "permit-conflicting-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "not_pdf", Detail: "validation failed",
	}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, conflicting); err != nil || permitOutcome(t, frames) != "duplicate" {
		t.Fatalf("conflicting result err=%v frames=%v", err, frames)
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	resultCount := 0
	for _, event := range events {
		switch event["kind"] {
		case "browser.provider_drive_epoch_result":
			resultCount++
			detail, _ := event["detail"].(map[string]any)
			if stringDetail(detail, "outcome") != "success" {
				t.Fatalf("durable result changed: %#v", event)
			}
		case providerLatchEventKind:
			t.Fatalf("conflicting duplicate minted a latch: %#v", event)
		case "browser.provider_drive_epoch_offered":
			detail, _ := event["detail"].(map[string]any)
			if intDetail(detail, "ordinal") == 1 {
				t.Fatalf("conflicting duplicate minted a successor: %#v", event)
			}
		}
	}
	if resultCount != 1 {
		t.Fatalf("result events=%d, want immutable first result", resultCount)
	}
}

func TestProviderDriveEffectPermitOverrideIgnoresLateResult(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-override-late", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-override-attempt", "domain-override")
	start := &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: "permit-override-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	if frames, err := b.providerDriveEpochStart(context.Background(), id, start); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	identity := job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindGenericDrive,
		DriveAttemptID: "permit-override-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
	}
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), identity)
	if err != nil || permit == nil {
		t.Fatalf("permit=%+v err=%v", permit, err)
	}
	if _, err := jobs.ReconcileEffectPermit(context.Background(), job.EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 0,
	}); err != nil {
		t.Fatalf("reconcile unknown completion: %v", err)
	}
	if err := jobs.ResolveUnknownEffectPermit(context.Background(), permit.ID, "operator verified no browser effect remains"); err != nil {
		t.Fatalf("resolve unknown completion: %v", err)
	}
	late := &protocol.ProviderDriveEpochResultRequestPayload{
		DriveAttemptID: "permit-override-attempt", Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "not_pdf", Detail: "validation failed",
	}
	if frames, err := b.providerDriveEpochResult(context.Background(), id, late); err != nil || permitOutcome(t, frames) != "duplicate" {
		t.Fatalf("late result err=%v frames=%v", err, frames)
	}
	if b.reofferPending[id] {
		t.Fatal("operator-resolved permit scheduled an automatic reoffer")
	}
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		switch event["kind"] {
		case "browser.provider_drive_epoch_result", providerLatchEventKind:
			t.Fatalf("operator-resolved permit applied late result state: %#v", event)
		case "browser.provider_drive_epoch_offered":
			detail, _ := event["detail"].(map[string]any)
			if intDetail(detail, "ordinal") == 1 {
				t.Fatalf("operator-resolved permit minted a successor: %#v", event)
			}
		}
	}
}

func TestArtifactProducerSettlesExactDrivePermit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-artifact-winner", handoffWork())
	attempt := "permit-artifact-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-artifact")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	ordinal := int64(0)
	settled, err := jobs.SettleArtifactProducer(context.Background(), id, job.ArtifactProducerIdentity{
		Kind: job.GenericDrive, DriveAttemptID: attempt, Ordinal: &ordinal,
		Strategy: "generic", Revision: "1",
	})
	if err != nil || !settled {
		t.Fatalf("artifact producer settlement settled=%v err=%v", settled, err)
	}
	p, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt,
		Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil || p == nil || p.Status != job.EffectPermitSettled {
		t.Fatalf("artifact permit=%+v err=%v", p, err)
	}
}

func TestUncorrelatedArtifactCannotSettleDriveSuccessor(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-artifact-ambiguous", handoffWork())
	attempt := "permit-artifact-ambiguous-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-artifact-ambiguous")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	p, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt,
		Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil || p == nil || p.Status != job.EffectPermitHeld {
		t.Fatalf("uncorrelated artifact changed permit=%+v err=%v", p, err)
	}
}
func TestArtifactProducerRecoveryRequiresExactDurableObservation(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "permit-artifact-producer-recovery", handoffWork())
	ordinal := int64(0)
	supplied := &job.ArtifactProducerIdentity{
		Kind: job.GenericDrive, DriveAttemptID: "producer-recovery-attempt",
		Ordinal: &ordinal, Strategy: "generic", Revision: "1",
	}
	digest := strings.Repeat("a", 64)
	if got := b.recoverArtifactProducer(ctx, id, "paper.pdf", digest, supplied); got != nil {
		t.Fatalf("uncorrelated supplied producer recovered: %+v", got)
	}
	if err := jobs.RecordEvent(ctx, id, "browser.download_complete", map[string]any{
		"filename": "paper.pdf", "sha256": digest, "producer": supplied,
	}); err != nil {
		t.Fatal(err)
	}
	mismatched := *supplied
	mismatched.Strategy = "other"
	if got := b.recoverArtifactProducer(ctx, id, "paper.pdf", digest, &mismatched); got != nil {
		t.Fatalf("mismatched supplied producer recovered: %+v", got)
	}
	got := b.recoverArtifactProducer(ctx, id, "paper.pdf", digest, supplied)
	if got == nil || got.DriveAttemptID != supplied.DriveAttemptID {
		t.Fatalf("matching durable producer = %+v", got)
	}
}

func TestUngovernedArtifactHashesAndSettlesExactProducer(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	effectPermitHolder(t, b)
	ctx := context.Background()
	id := park(t, jobs, "permit-artifact-ungoverned", handoffWork())
	attempt := "permit-artifact-ungoverned-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-artifact-ungoverned")
	if frames, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	filename := "paper.pdf"
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, filename))
	digest, err := fileDigest(filepath.Join(cfg.EffectiveAdoptionRoot(), id, filename))
	if err != nil {
		t.Fatal(err)
	}
	ordinal := int64(0)
	producer := &job.ArtifactProducerIdentity{
		Kind: job.GenericDrive, DriveAttemptID: attempt, Ordinal: &ordinal,
		Strategy: "generic", Revision: "1",
	}
	if err := jobs.RecordEvent(ctx, id, "browser.download_complete", map[string]any{
		"filename": filename, "sha256": digest, "producer": producer,
	}); err != nil {
		t.Fatal(err)
	}
	fence, err := b.weighArtifact(ctx, id, filename)
	if err != nil {
		t.Fatal(err)
	}
	if fence.governed || fence.digest != digest {
		t.Fatalf("ungoverned fence = %+v, want digest %q", fence, digest)
	}
	if err := b.commitArtifact(ctx, id, filename, fence, producer); err != nil {
		t.Fatal(err)
	}
	permit, err := jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{
		JobID: id, Kind: job.GenericDrive, DriveAttemptID: attempt,
		Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil || permit == nil || permit.Status != job.Settled {
		t.Fatalf("ungoverned artifact permit=%+v err=%v", permit, err)
	}
}

func TestEffectPermitReconcileOutboundExactIdentity(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-outbound", handoffWork())
	attempt := "reconcile-attempt-0001"
	domain := "domain-reconcile"
	effectPermitOffer(t, jobs, id, attempt, domain)
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v", err)
	}
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"})
	if err != nil || permit == nil {
		t.Fatal(err)
	}
	msgs, _ := runSyncAs(t, b, b.holder.ID)
	req := firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest)
	if req == nil {
		t.Fatalf("no reconcile request outbound: %v", msgs)
	}
	payload, ok := req.Payload.(*protocol.EffectPermitReconcileRequestPayload)
	if !ok {
		t.Fatalf("payload type %T", req.Payload)
	}
	if payload.PermitID != permit.ID || payload.EffectKind != string(job.EffectKindGenericDrive) || payload.DriveAttemptID != attempt || payload.Ordinal == nil || *payload.Ordinal != 0 || payload.Strategy != "generic" || payload.Revision != "1" {
		t.Fatalf("reconcile request identity mismatch: %+v vs permit %+v", payload, permit)
	}
	// No sensitive fields.
	raw, _ := json.Marshal(payload)
	s := strings.ToLower(string(raw))
	if strings.Contains(s, "https://") || strings.Contains(s, "provider") || strings.Contains(s, "/tmp") || strings.Contains(s, ".pdf") {
		t.Fatalf("reconcile request leaked sensitive text: %s", s)
	}
	if payload.RequestID == "" {
		t.Fatal("request_id empty")
	}
}

func nextEffectPermitReconcileRequest(t *testing.T, b *Bridge) *protocol.EffectPermitReconcileRequestPayload {
	t.Helper()
	msgs, _ := runSyncAs(t, b, b.holder.ID)
	req := firstOfType(msgs, protocol.MsgEffectPermitReconcileRequest)
	if req == nil {
		t.Fatalf("no reconcile request outbound: %v", msgs)
	}
	payload, ok := req.Payload.(*protocol.EffectPermitReconcileRequestPayload)
	if !ok {
		t.Fatalf("reconcile payload type %T", req.Payload)
	}
	return payload
}

func TestEffectPermitReconcileNoDispatchBecomesUnknown(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-unknown", handoffWork())
	attempt := "reconcile-unknown-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-unknown")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatal(err)
	}
	permit, _ := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"})
	request := nextEffectPermitReconcileRequest(t, b)
	// no-dispatch observation -> unknown_completion
	resp := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{

		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "recorded",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, resp)
	got, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if got.Status != job.EffectPermitUnknownCompletion {
		t.Fatalf("status=%q want unknown_completion", got.Status)
	}
}
func TestEffectPermitReconcileReplacementHolderClassifiesHistoricalPermit(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-replacement-holder", handoffWork())
	attempt := "reconcile-replacement-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-replacement-holder")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{
		DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
	}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatalf("start err=%v frames=%v", err, frames)
	}
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt,
		Ordinal: 0, Strategy: "generic", Revision: "1",
	})
	if err != nil || permit == nil {
		t.Fatalf("permit=%+v err=%v", permit, err)
	}
	request := nextEffectPermitReconcileRequest(t, b)
	// A replacement holder answers the request after its generation changes.
	// The bridge correlates the current request, while the store classifies
	// the historical permit using its stored generation.
	b.epoch = 77
	resp := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "recorded",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, resp)
	got, err := jobs.GetEffectPermit(context.Background(), permit.ID)
	if err != nil || got == nil || got.Status != job.EffectPermitUnknownCompletion {
		t.Fatalf("replacement reconcile permit=%+v err=%v, want unknown_completion", got, err)
	}
}

func TestEffectPermitReconcileDispatchedRemainsHeld(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-held-dispatched", handoffWork())
	attempt := "reconcile-held-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-held")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatal(err)
	}
	permit, _ := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"})
	for _, obs := range []map[string]any{
		{"outcome": "recorded", "dispatched": true, "download_present": false, "acknowledged": false, "tab_present": false},
		{"outcome": "recorded", "dispatched": false, "download_present": true, "acknowledged": false, "tab_present": false},
		{"outcome": "recorded", "dispatched": false, "download_present": false, "acknowledged": true, "tab_present": false},
	} {
		if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE effect_permits SET status='held' WHERE id=?`, permit.ID); err != nil {
			t.Fatal(err)
		}
		request := nextEffectPermitReconcileRequest(t, b)
		obs["request_id"] = request.RequestID
		obs["permit_id"] = permit.ID
		resp := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, obs)
		runSyncAs(t, b, b.holder.ID, resp)
		got, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
		if got.Status != job.EffectPermitHeld {
			t.Fatalf("held observation %v got status %q want held", obs, got.Status)
		}
	}
}

func TestEffectPermitReconcileNonTermsSettledProofDoesNotRelease(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-settled", handoffWork())
	attempt := "reconcile-settled-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-settled")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatal(err)
	}
	permit, _ := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"})
	request := nextEffectPermitReconcileRequest(t, b)
	resp := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "settled",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, resp)
	got, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if got.Status != job.EffectPermitHeld {
		t.Fatalf("status=%q want held", got.Status)
	}
}

func TestEffectPermitReconcileStaleOrWrongIDNoMutation(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "reconcile-stale", handoffWork())
	attempt := "reconcile-stale-attempt"
	effectPermitOffer(t, jobs, id, attempt, "domain-stale")
	if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil || permitOutcome(t, frames) != "started" {
		t.Fatal(err)
	}
	permit, _ := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1"})
	before, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	request := nextEffectPermitReconcileRequest(t, b)
	// stale outcome must not mutate
	stale := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "stale",
		"dispatched": true, "download_present": true, "acknowledged": true, "tab_present": true,
	})
	runSyncAs(t, b, b.holder.ID, stale)
	after, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if after.Status != before.Status {
		t.Fatalf("stale mutated %q -> %q", before.Status, after.Status)
	}
	// wrong permit id must not mutate even with an outstanding request.
	request = nextEffectPermitReconcileRequest(t, b)
	wrong := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
		"request_id": request.RequestID, "permit_id": "permit-does-not-exist", "outcome": "settled",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, wrong)
	after2, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if after2.Status != before.Status {
		t.Fatalf("wrong id mutated %q -> %q", before.Status, after2.Status)
	}
	request = nextEffectPermitReconcileRequest(t, b)
	otherID := park(t, jobs, "reconcile-other-job", handoffWork())
	wrongJob := inFrame(t, protocol.MsgEffectPermitReconcileResponse, otherID, map[string]any{
		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "recorded",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, wrongJob)
	afterWrongJob, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if afterWrongJob.Status != before.Status {
		t.Fatalf("wrong job mutated %q -> %q", before.Status, afterWrongJob.Status)
	}
	// wrong holder generation must not mutate: request under the current
	// generation, then fence it before the response arrives.
	request = nextEffectPermitReconcileRequest(t, b)
	b.epoch = 99
	mismatchGen := inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
		"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "settled",
		"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
	})
	runSyncAs(t, b, b.holder.ID, mismatchGen)
	after3, _ := jobs.GetEffectPermit(context.Background(), permit.ID)
	if after3.Status != before.Status {
		t.Fatalf("wrong generation mutated %q -> %q", before.Status, after3.Status)
	}
}

func TestLegacyProviderDriveResultSettlesCleanupBlocker(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	jobID := park(t, jobs, "legacy-result-cleanup", handoffWork())
	if err := jobs.RecordEvent(context.Background(), jobID, "browser.provider_drive_epoch_started", map[string]any{
		"drive_attempt_id": "legacy-result-attempt", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "domain-legacy-result",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ImportLegacyStartedEpochs(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := b.providerDriveEpochResult(context.Background(), jobID, &protocol.ProviderDriveEpochResultRequestPayload{
		RequestID: "legacy-result-request", DriveAttemptID: "legacy-result-attempt",
		Ordinal: 0, Strategy: "generic", Revision: "1",
		Outcome: "not_pdf", Detail: "historical result",
	})
	if err != nil || permitOutcome(t, frames) != "applied" {
		t.Fatalf("legacy result err=%v frames=%v", err, frames)
	}
	count, err := jobs.UnresolvedLegacyEffectBlockerCount(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("unresolved blockers=%d err=%v, want zero", count, err)
	}
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events[len(eventsBefore):] {
		t.Fatalf("cleanup-only legacy result mutated job history: %#v", event)
	}
}

func TestTermsEffectPermitAuthorizesAndSettlesExactOccurrence(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	ctx := context.Background()
	jobID := park(t, jobs, "terms-effect-permit", handoffWork())
	effectPermitOffer(t, jobs, jobID, "terms-drive-attempt", "domain-terms")
	request := &protocol.TermsEffectStartRequestPayload{
		RequestID: "request-terms-start",
		AdapterID: "jstor", AdapterVersion: "1.0.0",
		AuthorityDigest: strings.Repeat("a", 64),
	}
	first, err := b.termsEffectStart(ctx, jobID, request)
	if err != nil || permitOutcome(t, first) != "started" {
		t.Fatalf("first terms start err=%v frames=%v", err, first)
	}
	firstFrame, err := protocol.DecodeBrowserMessage(first[0])
	if err != nil {
		t.Fatal(err)
	}
	authorized := firstFrame.Payload.(*protocol.TermsEffectStartResultPayload)
	if authorized.PermitID == "" || authorized.TermsOccurrenceID == "" {
		t.Fatalf("terms authorization is incomplete: %+v", authorized)
	}
	replay, err := b.termsEffectStart(ctx, jobID, request)
	if err != nil || permitOutcome(t, replay) != "started" {
		t.Fatalf("terms start replay err=%v frames=%v", err, replay)
	}
	replayFrame, err := protocol.DecodeBrowserMessage(replay[0])
	if err != nil {
		t.Fatal(err)
	}
	replayed := replayFrame.Payload.(*protocol.TermsEffectStartResultPayload)
	if replayed.PermitID != authorized.PermitID || replayed.TermsOccurrenceID != authorized.TermsOccurrenceID {
		t.Fatalf("lost-response replay authorization=%+v, want exact original tuple", replayed)
	}
	b.epoch++
	fencedReplay, err := b.termsEffectStart(ctx, jobID, request)
	if err != nil || permitOutcome(t, fencedReplay) != "stale" {
		t.Fatalf("replacement-holder replay err=%v frames=%v", err, fencedReplay)
	}
	b.epoch--
	identity := job.EffectPermitIdentity{
		JobID: jobID, Kind: job.EffectKindTerms,
		TermsOccurrenceID: authorized.TermsOccurrenceID,
	}
	permit, err := jobs.GetEffectPermitByIdentity(ctx, identity)
	if err != nil || permit == nil || permit.ID != authorized.PermitID || permit.Status != job.EffectPermitHeld {
		t.Fatalf("held terms permit=%+v err=%v", permit, err)
	}
	result := &protocol.TermsEffectResultRequestPayload{
		RequestID: "request-terms-result", PermitID: authorized.PermitID,
		TermsOccurrenceID: authorized.TermsOccurrenceID, Outcome: "accepted",
	}
	applied, err := b.termsEffectResult(ctx, jobID, result)
	if err != nil || permitOutcome(t, applied) != "applied" {
		t.Fatalf("terms result err=%v frames=%v", err, applied)
	}
	duplicate, err := b.termsEffectResult(ctx, jobID, result)
	if err != nil || permitOutcome(t, duplicate) != "duplicate" {
		t.Fatalf("terms result replay err=%v frames=%v", err, duplicate)
	}
	permit, err = jobs.GetEffectPermit(ctx, authorized.PermitID)
	if err != nil || permit == nil || permit.Status != job.EffectPermitSettled {
		t.Fatalf("settled terms permit=%+v err=%v", permit, err)
	}
	closedReplay, err := b.termsEffectStart(ctx, jobID, request)
	if err != nil || permitOutcome(t, closedReplay) != "duplicate" {
		t.Fatalf("settled terms start replay err=%v frames=%v", err, closedReplay)
	}
	closedFrame, err := protocol.DecodeBrowserMessage(closedReplay[0])
	if err != nil {
		t.Fatal(err)
	}
	closed := closedFrame.Payload.(*protocol.TermsEffectStartResultPayload)
	if closed.PermitID != "" || closed.TermsOccurrenceID != "" {
		t.Fatalf("settled terms replay leaked authorization: %+v", closed)
	}
	events, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	authorizedEvents, resultEvents := 0, 0
	for _, event := range events {
		switch event["kind"] {
		case "browser.terms_effect_authorized":
			authorizedEvents++
		case "browser.terms_effect_result":
			resultEvents++
		}
		raw, _ := json.Marshal(event)
		if strings.Contains(strings.ToLower(string(raw)), "https://") {
			t.Fatalf("terms permit event leaked a URL: %s", raw)
		}
	}
	if authorizedEvents != 1 || resultEvents != 1 {
		t.Fatalf("terms events authorized=%d result=%d, want one each", authorizedEvents, resultEvents)
	}
}

func TestTermsEffectPermitRejectsClosedHandoff(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	ctx := context.Background()
	jobID := park(t, jobs, "terms-effect-closed-handoff", handoffWork())
	effectPermitOffer(t, jobs, jobID, "terms-closed-attempt", "domain-terms-closed")
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil || len(actions) != 1 {
		t.Fatalf("open actions=%+v err=%v", actions, err)
	}
	if err := jobs.ResolveHumanAction(ctx, actions[0].ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	frames, err := b.termsEffectStart(ctx, jobID, &protocol.TermsEffectStartRequestPayload{
		RequestID: "request-terms-closed",
		AdapterID: "jstor", AdapterVersion: "1.0.0",
		AuthorityDigest: strings.Repeat("b", 64),
	})
	if err != nil || permitOutcome(t, frames) != "stale" {
		t.Fatalf("closed handoff start err=%v frames=%v", err, frames)
	}
	if live, err := jobs.LiveEffectPermit(ctx); err != nil || live != nil {
		t.Fatalf("closed handoff permit=%+v err=%v", live, err)
	}
}

func TestHistoricalEffectResultNonHolderIsCleanupOnlyAndFeatureFenced(t *testing.T) {
	t.Run("negotiated non-holder settles cleanup only", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		effectPermitHolder(t, b)
		id := park(t, jobs, "permit-nonholder-cleanup", handoffWork())
		attempt := "permit-nonholder-attempt"
		effectPermitOffer(t, jobs, id, attempt, "domain-nonholder")
		if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		}); err != nil || permitOutcome(t, frames) != "started" {
			t.Fatalf("start err=%v frames=%v", err, frames)
		}
		permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
			JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt,
			Ordinal: 0, Strategy: "generic", Revision: "1",
		})
		if err != nil || permit == nil {
			t.Fatalf("permit=%+v err=%v", permit, err)
		}
		const pending = "permit-nonholder-session-00000000000000000001"
		runSyncAs(t, b, pending, helloWithFeatures(t, "0.14.0", providerDriveEpochV1Feature, effectPermitFeature))
		runSyncAs(t, b, pending, inFrame(t, protocol.MsgProviderDriveEpochResultRequest, id, map[string]any{
			"drive_attempt_id": attempt, "ordinal": 0, "strategy": "generic", "revision": "1", "outcome": "not_pdf",
		}))
		got, err := jobs.GetEffectPermit(context.Background(), permit.ID)
		if err != nil || got == nil || got.Status != job.EffectPermitSettled {
			t.Fatalf("non-holder result permit=%+v err=%v, want settled", got, err)
		}
		events, err := jobs.Events(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event["kind"] == providerLatchEventKind {
				t.Fatalf("non-holder result projected current event: %#v", event)
			}
			if event["kind"] == "browser.provider_drive_epoch_offered" {
				detail, _ := event["detail"].(map[string]any)
				if intDetail(detail, "ordinal") == 1 {
					t.Fatalf("non-holder result minted successor: %#v", event)
				}
			}
		}
	})

	t.Run("featureless non-holder cannot settle", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		effectPermitHolder(t, b)
		id := park(t, jobs, "permit-nonholder-featureless", handoffWork())
		attempt := "permit-nonholder-featureless-attempt"
		effectPermitOffer(t, jobs, id, attempt, "domain-nonholder-featureless")
		if frames, err := b.providerDriveEpochStart(context.Background(), id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		}); err != nil || permitOutcome(t, frames) != "started" {
			t.Fatalf("start err=%v frames=%v", err, frames)
		}
		permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{
			JobID: id, Kind: job.EffectKindGenericDrive, DriveAttemptID: attempt, Ordinal: 0,
			Strategy: "generic", Revision: "1",
		})
		if err != nil || permit == nil {
			t.Fatalf("permit=%+v err=%v", permit, err)
		}
		const pending = "permit-featureless-session-0000000000000000001"
		runSyncAs(t, b, pending, helloAs("0.14.0"))
		msgs, _ := runSyncAs(t, b, pending, inFrame(t, protocol.MsgProviderDriveEpochResultRequest, id, map[string]any{
			"drive_attempt_id": attempt, "ordinal": 0, "strategy": "generic", "revision": "1", "outcome": "not_pdf",
		}))
		errFrame := firstOfType(msgs, protocol.MsgError)
		if errFrame == nil || errFrame.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
			t.Fatalf("featureless non-holder result=%v, want session_busy", msgs)
		}
		got, err := jobs.GetEffectPermit(context.Background(), permit.ID)
		if err != nil || got == nil || got.Status != job.EffectPermitHeld {
			t.Fatalf("featureless non-holder mutated permit=%+v err=%v", got, err)
		}
	})
}

func TestProviderDriveStartRejectsSafetyDomainMismatch(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	effectPermitHolder(t, b)
	id := park(t, jobs, "permit-domain-mismatch", handoffWork())
	effectPermitOffer(t, jobs, id, "permit-domain-attempt", "domain-authorized")
	ok, err := b.providerDriveEpochAuthorized(context.Background(), id, "domain-other")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("provider start authorization accepted a mismatched safety domain")
	}
}

func TestLegacyDirectGetResultSettlesExactBlockerOnlyAndReopensAdmission(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkWithProviderEvidence(t, jobs, "legacy-direct-cleanup", handoffWork(), "onlinelibrary.wiley.com")
	const attempt = "legacy-direct-attempt"
	const revision = "legacy-direct/revision"
	if err := jobs.RecordEvent(ctx, jobID, "browser.direct_route", map[string]any{
		"phase": "offered", "drive_attempt_id": attempt, "ordinal": int64(0),
		"route_revision": revision, "safety_domain": "route:legacy-direct",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 1 {
		t.Fatalf("imported direct blockers=%d err=%v, want one", count, err)
	}
	// Importing the blocker fences the first admission globally.
	initial, _ := runSync(t, b, helloWithFeatures(t, "0.14.0", providerDirectGetV1Feature, effectPermitFeature))
	if firstOfType(initial, protocol.MsgProviderDirectGetRequest) != nil {
		t.Fatalf("legacy blocker did not fence initial direct admission: %v", initial)
	}
	before, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	wrong := &protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: attempt, Ordinal: 0, RouteRevision: "wrong/revision",
		Outcome: "not_pdf", LandingClass: "html",
	}
	if err := b.providerDirectGetResult(ctx, jobID, wrong); err != nil {
		t.Fatal(err)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 1 {
		t.Fatalf("wrong direct tuple blockers=%d err=%v, want unresolved one", count, err)
	}
	exact := &protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: attempt, Ordinal: 0, RouteRevision: revision,
		Outcome: "not_pdf", LandingClass: "html",
	}
	if err := b.providerDirectGetResult(ctx, jobID, exact); err != nil {
		t.Fatal(err)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 0 {
		t.Fatalf("exact direct tuple blockers=%d err=%v, want zero", count, err)
	}
	after, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("cleanup-only direct result appended job events: before=%d after=%d", len(before), len(after))
	}
	reopened, _ := runSync(t, b)
	if firstOfType(reopened, protocol.MsgProviderDirectGetRequest) == nil {
		t.Fatalf("fresh direct admission did not reopen after blocker settlement: %v", reopened)
	}
}

func TestLegacyInstitutionalNavigatedSettlesExactBlockerOnlyAndReopensAdmission(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	jobID := parkInstitutional(t, jobs, "legacy-institutional-cleanup", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "legacy-institution-digest", AuthenticationClaimID: "legacy-institution-auth",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	profile := profiles[0]
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "legacy-institution-candidate", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "legacy-institution-safety", SafetyDomainID: "institution:legacy",
		AdapterRevision: "legacy-adapter", EffectContractID: "legacy-effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profile.Revision, 7); err != nil {
		t.Fatal(err)
	}
	effectOrdinal, err := jobs.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, b.epoch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if effectOrdinal < 1 {
		t.Fatalf("issued effect ordinal=%d, want positive", effectOrdinal)
	}
	if err := jobs.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 1 {
		t.Fatalf("imported institutional blockers=%d err=%v, want one", count, err)
	}
	beforeEvents, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrongFrames, err := b.institutionalNavigated(ctx, jobID, &protocol.InstitutionalNavigatedRequestPayload{
		RequestID: "legacy-institution-wrong", ClaimID: claim.ID, BindingID: claim.BindingID,
		RouteIssuanceOrdinal: 1, EffectOrdinal: effectOrdinal + 1,
		InstitutionalRequestID: "legacy-institution-request", TabID: 7,
	})
	if err != nil || len(wrongFrames) != 1 {
		t.Fatalf("wrong institutional navigation frames=%d err=%v", len(wrongFrames), err)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 1 {
		t.Fatalf("wrong institutional tuple blockers=%d err=%v, want unresolved one", count, err)
	}
	frames, err := b.institutionalNavigated(ctx, jobID, &protocol.InstitutionalNavigatedRequestPayload{
		RequestID: "legacy-institution-exact", ClaimID: claim.ID, BindingID: claim.BindingID,
		RouteIssuanceOrdinal: 1, EffectOrdinal: effectOrdinal,
		InstitutionalRequestID: "legacy-institution-request", TabID: 7,
	})
	if err != nil || len(frames) != 1 {
		t.Fatalf("exact institutional navigation frames=%d err=%v", len(frames), err)
	}
	response, err := protocol.DecodeBrowserMessage(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	navigated := response.Payload.(*protocol.InstitutionalNavigatedResponsePayload)
	if navigated.Outcome != "stale" || navigated.Detail != "navigation was settled as historical cleanup" {
		t.Fatalf("legacy institutional response=%+v, want cleanup-only stale", navigated)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 0 {
		t.Fatalf("exact institutional tuple blockers=%d err=%v, want zero", count, err)
	}
	afterEvents, err := jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("cleanup-only institutional result appended job events: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
	current, err := jobs.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, beforeClaim) {
		t.Fatalf("legacy cleanup mutated current claim: before=%+v after=%+v", beforeClaim, current)
	}

	// A distinct fresh claim can acquire the now-free institutional effect lane.
	freshCandidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "legacy-institution-fresh-candidate", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 2, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "legacy-institution-fresh-safety", SafetyDomainID: "institution:fresh",
		AdapterRevision: "legacy-adapter", EffectContractID: "legacy-effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	freshClaim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: freshCandidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 2, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, freshClaim.ID, freshClaim.BindingID, b.epoch, profile.Revision, 8); err != nil {
		t.Fatal(err)
	}
	freshFrames, err := b.institutionalRoute(ctx, jobID, &protocol.InstitutionalRouteRequestPayload{
		RequestID: "legacy-institution-fresh-route", ClaimID: freshClaim.ID, BindingID: freshClaim.BindingID,
		InstitutionalRequestID: "legacy-institution-fresh-request",
	})
	if err != nil || len(freshFrames) != 1 {
		t.Fatalf("fresh institutional route frames=%d err=%v", len(freshFrames), err)
	}
	freshMsg, err := protocol.DecodeBrowserMessage(freshFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	freshRoute := freshMsg.Payload.(*protocol.InstitutionalRouteResponsePayload)
	if freshRoute.Outcome != "issued" {
		t.Fatalf("fresh institutional admission outcome=%q detail=%q", freshRoute.Outcome, freshRoute.Detail)
	}
}

func TestLegacyInstitutionalNavigatedWireUsesPrePermitNegotiation(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkInstitutional(t, jobs, "legacy-institution-wire", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "legacy-wire-digest", AuthenticationClaimID: "legacy-wire-auth",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	candidate, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "legacy-wire-candidate", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "legacy-wire-safety", SafetyDomainID: "institution:legacy-wire",
		AdapterRevision: "legacy-wire-adapter", EffectContractID: "legacy-wire-effect", Status: "eligible",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch, profiles[0].Revision, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, b.epoch, 0); err != nil {
		t.Fatal(err)
	}
	if err := jobs.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	legacyHello := inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.10.0",
		"features":          []string{institutionalMaterializationFeature},
	})
	legacyNavigation := inFrame(t, protocol.MsgInstitutionalNavigatedRequest, jobID,
		map[string]any{
			"request_id":             "legacy-wire-request",
			"claim_id":               claim.ID,
			"binding_id":             claim.BindingID,
			"route_issuance_ordinal": 1,
			"tab_id":                 7,
		})
	messages, _ := runSyncAs(t, b, testSessionID, legacyHello, legacyNavigation)
	var response *protocol.InstitutionalNavigatedResponsePayload
	for _, msg := range messages {
		if msg.Type == protocol.MsgInstitutionalNavigatedResponse {
			response = msg.Payload.(*protocol.InstitutionalNavigatedResponsePayload)
		}
	}
	if response == nil || response.Outcome != "stale" {
		t.Fatalf("legacy navigation response=%+v, want cleanup-only stale", response)
	}
	if count, err := jobs.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 0 {
		t.Fatalf("legacy wire blocker count=%d err=%v, want zero", count, err)
	}
}

func TestLegacyInstitutionalPeerRejectsCurrentPermitTuple(t *testing.T) {
	b, _, _, _ := newBridge(t)
	legacyHello := inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.10.0",
		"features":          []string{institutionalMaterializationFeature},
	})
	runSyncAs(t, b, testSessionID, legacyHello)
	currentNavigation := inFrame(t, protocol.MsgInstitutionalNavigatedRequest, "job-inst-current-shape",
		map[string]any{
			"request_id":               "legacy-current-shape-request",
			"claim_id":                 "legacy-current-shape-claim",
			"binding_id":               "legacy-current-shape-binding",
			"route_issuance_ordinal":   1,
			"effect_ordinal":           1,
			"institutional_request_id": "legacy-current-effect-request",
			"tab_id":                   7,
		})
	if _, err := b.Sync(context.Background(), testSessionID, false,
		[]json.RawMessage{currentNavigation}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("legacy peer current-shaped result err=%v, want invalid frame", err)
	}
}

// TestInstitutionalReofferSkipsFruitlessSiblings pins the head-of-line block
// measured live on 2026-08-21: the reoffer release budget is four slots, its
// candidate filter checked AGE only, and `reofferPending` then overrides the
// fruitless-epoch gate in the ordinary drain — on the claim that the reoffer
// path "was already filtered when it was set". It was not. Four papers that
// had each burned their fruitless budget (permanently excluded from ordinary
// offering, `browser.handoff_quiesced` already recorded) consumed the whole
// budget on every single sign-in, 424 releases deep, while 58 healthy papers
// behind them were never volunteered once.
//
// Age here is deliberately young for both siblings, so age-quiescence cannot
// account for the result: the only difference is drive evidence.
func TestInstitutionalReofferSkipsFruitlessSiblings(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()

	source := parkInstitutional(t, jobs, "wr_reoffer_source", handoffWork(), "")
	fruitless := parkInstitutional(t, jobs, "wr_reoffer_fruitless", handoffWork(), "")
	healthy := parkInstitutional(t, jobs, "wr_reoffer_healthy", handoffWork(), "")

	action := openHandoffAction(t, jobs, fruitless)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Three drives, each accepted, none ever reporting an outcome.
	last := created
	for range job.MaxAutomaticHandoffEpochs {
		last = last.Add(job.HandoffAcceptedLease + 10*time.Second)
		appendEventAt(t, jobs, fruitless, "browser.handoff_offered",
			map[string]any{"requires_auth": true}, last)
		appendEventAt(t, jobs, fruitless, "browser.job_accept", nil, last.Add(time.Second))
	}
	now := last.Add(job.HandoffAcceptedLease + time.Second)
	b.now = func() time.Time { return now }
	runSync(t, b, hello())

	// The fruitless sibling is already suppressed by the ordinary gate here;
	// what follows measures whether the sign-in release resurrects it.
	msgs, _ := runSync(t, b,
		inFrame(t, protocol.MsgAuthReturned, source, map[string]any{"elapsed_ms": 10}))

	reoffered := func(id string) int {
		events, err := jobs.Events(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, event := range events {
			if event["kind"] == "browser.handoff_reoffered" {
				n++
			}
		}
		return n
	}
	if got := reoffered(fruitless); got != 0 {
		t.Fatalf("fruitless sibling re-offers = %d, want 0 (it holds the budget forever)", got)
	}
	if got := reoffered(healthy); got != 1 {
		t.Fatalf("healthy sibling re-offers = %d, want 1 (the budget must reach it)", got)
	}
	if b.reofferPending[fruitless] {
		t.Fatal("fruitless sibling was marked for re-offer, overriding its own quiesce gate")
	}
	// And the release reaches the wire for the healthy paper, never the dead one.
	sawHealthy := false
	for _, frame := range msgs {
		if frame == nil || frame.Type != protocol.MsgJobOffer {
			continue
		}
		if frame.JobID == fruitless {
			t.Fatal("fruitless sibling reached the wire after a sign-in")
		}
		if frame.JobID == healthy {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Fatal("healthy sibling never reached the wire after a sign-in")
	}
}

// TestQueuedAcceptsAreNotFruitlessDrives pins the accounting bug that retired
// a whole backlog. `job_accept` used to mean both "driving" and "queued behind
// my one drive slot", and an OFFER opened an epoch by itself — so a paper that
// papio merely asked about, and that the extension never drove, was charged a
// fruitless drive. Measured live 2026-08-21: 78 papers permanently quiesced on
// 438 accepted handoffs, 77 of those papers having no `browser_candidates` row
// at all.
func TestQueuedAcceptsAreNotFruitlessDrives(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_queued_accounting", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Six offer/queued-accept cycles, each a full lease apart: the shape of a
	// paper waiting behind another paper's sign-in, twice over the budget.
	at := created
	for range 2 * job.MaxAutomaticHandoffEpochs {
		at = at.Add(job.HandoffAcceptedLease + 10*time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered",
			map[string]any{"requires_auth": true}, at)
		appendEventAt(t, jobs, id, "browser.job_accept",
			map[string]any{"disposition": job.JobAcceptDispositionQueued}, at.Add(time.Second))
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(job.HandoffAcceptedLease + time.Second)
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, now)
	if state.FruitlessEpochs != 0 {
		t.Fatalf("fruitless epochs after %d queue waits = %d, want 0",
			2*job.MaxAutomaticHandoffEpochs, state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced by waiting its turn, which is papio's own queue and not the paper's failure")
	}

	// An ack with no disposition is an older extension, whose acks have always
	// meant a drive: the silent-drive incident must still be caught.
	silent := park(t, jobs, "wr_silent_drive", handoffWork())
	silentAction := openHandoffAction(t, jobs, silent)
	silentCreated, err := time.Parse(time.RFC3339Nano, silentAction.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	at = silentCreated
	for range job.MaxAutomaticHandoffEpochs {
		at = at.Add(job.HandoffAcceptedLease + 10*time.Second)
		appendEventAt(t, jobs, silent, "browser.handoff_offered",
			map[string]any{"requires_auth": true}, at)
		appendEventAt(t, jobs, silent, "browser.job_accept", nil, at.Add(time.Second))
	}
	events, err = jobs.Events(ctx, silent)
	if err != nil {
		t.Fatal(err)
	}
	state = job.ProjectHandoffOfferState(events, silentAction.CreatedAt,
		at.Add(job.HandoffAcceptedLease+time.Second))
	if !state.Quiesced {
		t.Fatalf("dispositionless acks did not quiesce (epochs=%d); an older extension's ack means a drive",
			state.FruitlessEpochs)
	}
}

// TestQueuedOffersDoNotConsumeTheInFlightBudget is the transport-budget half of
// the accounting fixed in TestQueuedAcceptsAreNotFruitlessDrives above: the
// same "queued" signal, applied to the limit that actually gates new work.
//
// maxOutstandingOffers bounds the surfaces papio drives at once. Counting SENT
// offers instead of driven ones deadlocked the operator's queue: four papers
// with no browser candidates held all four slots while answering "queued",
// waiting behind one library sign-in they could never complete, and 128 papers
// behind them - including a paper the operator had explicitly clicked Open on -
// could not be offered at all. Measured 2026-08-21: 626 queued acks against
// 262 driving ones in one day.
func TestQueuedOffersDoNotConsumeTheInFlightBudget(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	runSync(t, b, hello())

	saturating := make([]string, 0, maxOutstandingOffers)
	for i := range maxOutstandingOffers {
		id := park(t, jobs, fmt.Sprintf("wr_budget_queued_%d", i), handoffWork())
		openHandoffAction(t, jobs, id)
		saturating = append(saturating, id)
	}

	// Drain offers until every one of them has been offered, then have the
	// extension answer exactly what it answers live: "I have queued it."
	offeredIDs := map[string]bool{}
	for range 8 {
		msgs, _ := runSync(t, b)
		for _, m := range msgs {
			if m.Type == protocol.MsgJobOffer {
				offeredIDs[m.JobID] = true
			}
		}
		if len(offeredIDs) >= len(saturating) {
			break
		}
	}
	if len(offeredIDs) < len(saturating) {
		t.Fatalf("setup: only %d of %d papers were offered", len(offeredIDs), len(saturating))
	}
	for id := range offeredIDs {
		runSync(t, b, inFrame(t, protocol.MsgJobAccept, id,
			protocol.JobAcceptPayload{Disposition: job.JobAcceptDispositionQueued}))
	}

	// Every slot is nominally spent, and nothing is being driven. A paper that
	// arrives now must still be offered.
	fresh := park(t, jobs, "wr_budget_fresh", handoffWork())
	openHandoffAction(t, jobs, fresh)
	for range 8 {
		msgs, _ := runSync(t, b)
		for _, m := range msgs {
			if m.Type == protocol.MsgJobOffer && m.JobID == fresh {
				return
			}
		}
	}
	t.Fatalf("a paper was starved by %d offers the extension said it had only QUEUED - this is the deadlock",
		len(offeredIDs))
}

// TestHandoffEpochsResetRepairsAStreak pins the repair path migration 0045
// uses. The verdict is derived from history, so correcting the rule cannot
// un-charge accepts already recorded without a disposition; the reset says so
// explicitly instead of guessing which historical acks were queue waits.
func TestHandoffEpochsResetRepairsAStreak(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_epoch_repair", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	at := created
	for range job.MaxAutomaticHandoffEpochs {
		at = at.Add(job.HandoffAcceptedLease + 10*time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered",
			map[string]any{"requires_auth": true}, at)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, at.Add(time.Second))
	}
	now := at.Add(job.HandoffAcceptedLease + time.Second)
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !job.ProjectHandoffOfferState(events, action.CreatedAt, now).Quiesced {
		t.Fatal("precondition: the streak must be quiesced before repair")
	}

	appendEventAt(t, jobs, id, job.HandoffEpochsResetEvent,
		map[string]any{"reason": "queued_accepts_charged_as_drives"}, now)
	events, err = jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, now.Add(time.Second))
	if state.Quiesced || state.FruitlessEpochs != 0 {
		t.Fatalf("after repair: quiesced=%v epochs=%d, want false/0",
			state.Quiesced, state.FruitlessEpochs)
	}

	// A genuinely dead paper re-quiesces on its next three real drives; the
	// repair is not an exemption.
	at = now
	for range job.MaxAutomaticHandoffEpochs {
		at = at.Add(job.HandoffAcceptedLease + 10*time.Second)
		appendEventAt(t, jobs, id, "browser.handoff_offered",
			map[string]any{"requires_auth": true}, at)
		appendEventAt(t, jobs, id, "browser.job_accept", nil, at.Add(time.Second))
	}
	events, err = jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !job.ProjectHandoffOfferState(events, action.CreatedAt,
		at.Add(job.HandoffAcceptedLease+time.Second)).Quiesced {
		t.Fatal("a repaired paper that then burned its budget again must re-quiesce")
	}
}

// TestTerminalClaimRetiresOnlyAfterItsCancelIsDelivered pins both halves of the
// immortal-claim repair. Both reconcile paths skip a claim carrying ANY
// effect_permits row, so a terminal job's claim outlived its lease, its tab and
// the browser itself: measured live 2026-08-21, eleven claims on cancelled jobs
// sat `navigated` with tab ids days dead, each with a settled permit, while
// poll re-sent their cancel on every daemon restart.
//
// The ordering is the load-bearing part. The cancel frame is emitted BECAUSE a
// terminal job still owns a live claim, so retiring the row in the same pass
// deletes the notice that tells the extension to close the tab - a stranded
// surface, which is the exact failure this subsystem exists to prevent.
func TestTerminalClaimRetiresOnlyAfterItsCancelIsDelivered(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	// Any live phase qualifies; the live rows were `navigated`, and this seed is
	// the one the permit APIs can still bind.
	claim := seedSurfaceCloseClaim(t, b, jobs, "terminal-immortal", "claimed")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	// A SETTLED permit is what made the row immortal: the effect is over, so it
	// protects nothing, yet the permit's mere existence vetoed both sweeps.
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch,
		candidate.InstitutionProfileRevision, 9); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := jobs.AcquireInstitutionalEffectPermit(ctx,
		job.InstitutionalEffectPermitAcquireInput{
			JobID: candidate.JobID, ClaimID: claim.ID, BindingID: claim.BindingID,
			SafetyDomainID: candidate.SafetyDomainID, InstitutionalRequestID: "immortal-request",
			JobAttemptRevision: candidate.JobAttemptRevision, BrowserHolderGeneration: b.epoch,
			ExpectedEffectOrdinal: 0, LeaseUntil: b.now().Add(time.Minute),
			Authorization: job.EffectPermitEvent{Kind: "institutional.authorized"},
		}); err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("effect permit outcome=%v err=%v", outcome, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}

	// An IN-FLIGHT permit is a real veto: the provider effect may still be
	// running, and job termination is not permission to interrupt it.
	msgs, _ := runSync(t, b)
	if cancel := firstOfType(msgs, protocol.MsgCancel); cancel == nil {
		t.Fatalf("first poll must still announce the cancel: %v", msgs)
	}
	runSync(t, b)
	held, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if held == nil || held.Phase == "abandoned" {
		t.Fatalf("a held permit must veto retirement, got %+v", held)
	}

	if _, outcome, err := jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
		Identity: job.EffectPermitIdentity{
			JobID: candidate.JobID, Kind: job.Institutional, ClaimID: claim.ID,
			BindingID: claim.BindingID, EffectOrdinal: 1, InstitutionalRequestID: "immortal-request",
		},
		RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": 1,
			"institutional_request_id": "immortal-request",
		}}},
	}); err != nil || outcome != job.EffectPermitApplied {
		t.Fatalf("settle outcome=%v err=%v", outcome, err)
	}
	// Settled now, and this session already delivered the cancel above, so the
	// next poll is the first one allowed to retire the row.
	runSync(t, b)
	retired, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if retired == nil || retired.Phase != "abandoned" {
		t.Fatalf("settled permit on a terminal job must retire the claim, got %+v", retired)
	}
}

// TestTerminalClaimSurvivesThePollThatAnnouncesIt pins the delivery gate. The
// cancel frame is emitted BECAUSE a terminal job still owns a live claim, so a
// same-poll retirement would let the frame be lost in transport with the row
// already gone - papio would have forgotten a surface it never proved the
// browser was told about, and the tab would outlive it. Retirement therefore
// waits for a poll after the one that queued the frame: if the session survived
// to poll again, the frame was delivered.
func TestTerminalClaimSurvivesThePollThatAnnouncesIt(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "terminal-announce", "claimed")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, b.epoch,
		candidate.InstitutionProfileRevision, 11); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := jobs.AcquireInstitutionalEffectPermit(ctx,
		job.InstitutionalEffectPermitAcquireInput{
			JobID: candidate.JobID, ClaimID: claim.ID, BindingID: claim.BindingID,
			SafetyDomainID: candidate.SafetyDomainID, InstitutionalRequestID: "announce-request",
			JobAttemptRevision: candidate.JobAttemptRevision, BrowserHolderGeneration: b.epoch,
			ExpectedEffectOrdinal: 0, LeaseUntil: b.now().Add(time.Minute),
			Authorization: job.EffectPermitEvent{Kind: "institutional.authorized"},
		}); err != nil || outcome != job.EffectPermitAcquired {
		t.Fatalf("effect permit outcome=%v err=%v", outcome, err)
	}
	// Settled BEFORE the first poll, so nothing but the delivery gate can hold
	// the row through it.
	if _, outcome, err := jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
		Identity: job.EffectPermitIdentity{
			JobID: candidate.JobID, Kind: job.Institutional, ClaimID: claim.ID,
			BindingID: claim.BindingID, EffectOrdinal: 1, InstitutionalRequestID: "announce-request",
		},
		RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": 1,
			"institutional_request_id": "announce-request",
		}}},
	}); err != nil || outcome != job.EffectPermitApplied {
		t.Fatalf("settle outcome=%v err=%v", outcome, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}

	msgs, _ := runSync(t, b)
	if cancel := firstOfType(msgs, protocol.MsgCancel); cancel == nil {
		t.Fatalf("the announcing poll must emit the cancel: %v", msgs)
	}
	announced, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if announced == nil || announced.Phase == "abandoned" {
		t.Fatalf("the row must outlive the poll that announces it, got %+v", announced)
	}

	runSync(t, b)
	retired, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if retired == nil || retired.Phase != "abandoned" {
		t.Fatalf("the poll after delivery must retire the row, got %+v", retired)
	}
}

// A browser-reported cancellation suppresses the daemon's redundant cancel
// frame, but suppression is not delivery: the extension sends
// `provider_outcome: cancelled` BEFORE its own asynchronous close, and that
// close can be refused while an institutional effect permit vetoes teardown.
// Retiring on the wider marker abandoned the claim behind a tab papio had
// never told anyone to close — the litter this mechanism exists to prevent,
// reached from the opposite direction.
func TestSuppressedCancelDoesNotAuthorizeRetirement(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, materializationHello(t))
	claim := seedSurfaceCloseClaim(t, b, jobs, "suppressed-cancel", "navigated")
	candidate, err := jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		t.Fatalf("binding candidate = %+v, err=%v", candidate, err)
	}
	if err := jobs.Cancel(ctx, candidate.JobID, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	// Exactly what the provider_outcome path leaves behind: no frame owed, and
	// no proof the browser ever heard anything.
	b.mu.Lock()
	b.cancelSent[candidate.JobID] = true
	b.mu.Unlock()

	for i := range 2 {
		msgs, _ := runSync(t, b)
		if cancel := firstOfType(msgs, protocol.MsgCancel); cancel != nil {
			t.Fatalf("poll %d re-emitted a suppressed cancel: %v", i, msgs)
		}
	}
	held, err := jobs.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if held == nil || held.Phase == "abandoned" {
		t.Fatalf("a claim whose browser was never told must not be retired, got %+v", held)
	}
}

// A binding can own several tabs, and before surface_superseded a duplicate
// could only be retired by asserting scaffold_idle - a binding-wide claim that
// any navigated claim structurally fails. So every duplicate survived, and the
// operator's browser accumulated three tabs on one paper (measured live
// 2026-08-22). The daemon does not take the browser's word: it authorizes only
// after comparing the named tab to the tab it believes drives the claim.
func TestSurfaceCloseSupersededAuthorizesOnlyANonDrivingTab(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase       string
		tabOffset   int64
		omitTab     bool
		wantOutcome string
		wantDetail  string
	}{
		{name: "duplicate of a navigated claim", phase: "navigated", tabOffset: 1, wantOutcome: "authorized"},
		{name: "duplicate of a bound claim", phase: "bound", tabOffset: 7, wantOutcome: "authorized"},
		{name: "the driving tab itself", phase: "navigated", tabOffset: 0,
			wantOutcome: "not_eligible", wantDetail: "the named surface is the binding's driven tab"},
		{name: "no tab named", phase: "navigated", omitTab: true,
			wantOutcome: "not_eligible", wantDetail: "surface_superseded requires the superseded tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			runSync(t, b, materializationHello(t))
			claim := seedSurfaceCloseClaim(t, b, jobs, "close-superseded", tc.phase)

			req := &protocol.SurfaceCloseRequestPayload{
				RequestID: "req-close-superseded", BindingID: claim.BindingID,
				BrowserHolderGeneration: b.epoch, Disposition: "surface_superseded",
			}
			if !tc.omitTab {
				tab := claim.TabID + tc.tabOffset
				req.SurfaceTabID = &tab
			}
			frames, err := b.surfaceClose(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			got := decodeSurfaceCloseResponse(t, frames)
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q (detail %q), want %q", got.Outcome, got.Detail, tc.wantOutcome)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			if tc.wantOutcome != "authorized" {
				return
			}
			var disposition string
			if err := jobs.S.DB().QueryRowContext(ctx,
				`SELECT disposition FROM close_authorizations WHERE id=?`,
				got.CloseAuthorizationID).Scan(&disposition); err != nil {
				t.Fatal(err)
			}
			if disposition != "surface_superseded" {
				t.Fatalf("stored disposition = %q, want surface_superseded", disposition)
			}
		})
	}
}

func TestRequestDevReloadRequiresHolder(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_, _, err := b.RequestDevReload()
	if err == nil || err.Error() != "no browser session holds the bridge" {
		t.Fatalf("RequestDevReload without holder err = %v, want %q", err, "no browser session holds the bridge")
	}
}

func TestRequestDevReloadRejectsOldExtensionVersionAndDoesNotLatch(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.14.0"))
	_, _, err := b.RequestDevReload()
	if err == nil {
		t.Fatal("RequestDevReload against 0.14.0 extension was accepted")
	}
	if !strings.Contains(err.Error(), "0.14.0") || !strings.Contains(err.Error(), DevReloadMinExtensionVersion) {
		t.Fatalf("error = %q, want both versions %q and %q named", err.Error(), "0.14.0", DevReloadMinExtensionVersion)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) != nil {
		t.Fatalf("rejected dev_reload was still latched: %v", msgs)
	}
	b.mu.Lock()
	latched := b.holder != nil && b.holder.pendingDevReload != ""
	b.mu.Unlock()
	if latched {
		t.Fatal("pendingDevReload is latched after a version-gated refusal")
	}
}

func TestRequestDevReloadHappyPathEmitsOnNextSync(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	sid, reloadID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	if sid != sessA {
		t.Fatalf("session id = %q, want %q", sid, sessA)
	}
	if reloadID == "" {
		t.Fatal("reload_id is empty")
	}
	msgs, _ := runSyncAs(t, b, sessA)
	got := firstOfType(msgs, protocol.MsgDevReload)
	if got == nil {
		t.Fatalf("next Sync did not emit dev_reload, got %v", msgs)
	}
	payload, ok := got.Payload.(*protocol.DevReloadPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *DevReloadPayload", got.Payload)
	}
	if payload.ReloadID != reloadID {
		t.Fatalf("reload_id = %q, want %q", payload.ReloadID, reloadID)
	}
	if got.JobID != "" {
		t.Fatalf("dev_reload job_id = %q, want empty (job-free)", got.JobID)
	}
}

func TestDevReloadLatchIsOneShotStopsReloadLoop(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	_, reloadID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("first Sync after latch did not emit dev_reload")
	}
	msgs, _ = runSyncAs(t, b, sessA)
	if second := firstOfType(msgs, protocol.MsgDevReload); second != nil {
		t.Fatalf("second Sync re-emitted dev_reload %v with reload_id %q; latch must be one-shot to stop a reload loop", msgs, second.Payload.(*protocol.DevReloadPayload).ReloadID)
	}
	// A second latch must produce a fresh id, not replay the old one.
	_, secondID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("second RequestDevReload: %v", err)
	}
	if secondID == reloadID {
		t.Fatalf("second reload_id %q equals first %q; each latch must be distinct", secondID, reloadID)
	}
	msgs, _ = runSyncAs(t, b, sessA)
	got := firstOfType(msgs, protocol.MsgDevReload)
	if got == nil || got.Payload.(*protocol.DevReloadPayload).ReloadID != secondID {
		t.Fatalf("fresh latch not emitted: %v want %q", msgs, secondID)
	}
	msgs, _ = runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) != nil {
		t.Fatalf("fresh latch was not one-shot: %v", msgs)
	}
}

func TestDevReloadPendingSessionDoesNotReceiveHolderLatch(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	runSyncAs(t, b, sessB, helloAs("0.15.0"))
	_, reloadID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessB)
	if got := firstOfType(msgs, protocol.MsgDevReload); got != nil {
		t.Fatalf("pending session received holder's dev_reload %v", msgs)
	}
	msgs, _ = runSyncAs(t, b, sessA)
	got := firstOfType(msgs, protocol.MsgDevReload)
	if got == nil {
		t.Fatalf("holder did not receive its own latched dev_reload")
	}
	if got.Payload.(*protocol.DevReloadPayload).ReloadID != reloadID {
		t.Fatalf("holder reload_id = %q, want %q", got.Payload.(*protocol.DevReloadPayload).ReloadID, reloadID)
	}
}
func TestDevReloadReservationHoldsSlotAgainstPendingSibling(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	msgs, _ := runSyncAs(t, b, sessB, helloAs("0.15.0"))
	busy := firstOfType(msgs, protocol.MsgError)
	if busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("B hello must be denied with session_busy, got %+v", msgs)
	}
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ = runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted on holder Sync, got %+v", msgs)
	}
	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatalf("goodbye Sync: %v", err)
	}
	msgs, _ = runSyncAs(t, b, sessB)
	b.mu.Lock()
	holder := b.holder
	_, stillPending := b.pending[sessB]
	reserved := b.devReloadReserved(b.now())
	b.mu.Unlock()
	if holder != nil {
		t.Fatalf("pending sibling stole the slot during reservation: holder=%+v", holder)
	}
	if !stillPending {
		t.Fatalf("B must remain pending while reservation is live")
	}
	if !reserved {
		t.Fatalf("reservation must still be live after holder goodbye")
	}
	if ack := firstOfType(msgs, protocol.MsgHelloAck); ack != nil {
		t.Fatalf("B must not receive a holder hello_ack during reservation, got %+v", msgs)
	}
}

func TestDevReloadFreshHelloReclaimsReservedSlot(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	runSyncAs(t, b, sessB, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}
	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatalf("goodbye Sync: %v", err)
	}
	// Sibling polling while reserved must not take the slot.
	if _, err := b.Sync(context.Background(), sessB, false, nil); err != nil {
		t.Fatalf("B Sync: %v", err)
	}
	b.mu.Lock()
	if b.holder != nil {
		b.mu.Unlock()
		t.Fatalf("holder must be nil before reloader returns, got %+v", b.holder)
	}
	b.mu.Unlock()
	const sessC = "cccc3333cccc3333cccc3333cccc3333"
	msgs, _ = runSyncAs(t, b, sessC, helloAs("0.15.0"))
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRoleHolder {
		t.Fatalf("fresh reloader hello must be granted holder, got %+v", msgs)
	}
	b.mu.Lock()
	holderID := ""
	if b.holder != nil {
		holderID = b.holder.ID
	}
	reserved := b.devReloadReserved(b.now())
	b.mu.Unlock()
	if holderID != sessC {
		t.Fatalf("holder = %q, want %q", holderID, sessC)
	}
	if reserved {
		t.Fatalf("reservation must be cleared after reloader reclaims slot")
	}
}

func TestDevReloadReservationExpiryPromotesWaitingSibling(t *testing.T) {
	b, _, _, _ := newBridge(t)
	advance := settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	runSyncAs(t, b, sessB, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}
	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatalf("goodbye Sync: %v", err)
	}
	advance(devReloadReservation + time.Second)
	msgs, _ = runSyncAs(t, b, sessB)
	b.mu.Lock()
	holderID := ""
	if b.holder != nil {
		holderID = b.holder.ID
	}
	b.mu.Unlock()
	if holderID != sessB {
		t.Fatalf("after reservation expiry holder = %q, want %q (B must be promoted)", holderID, sessB)
	}
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRoleHolder {
		t.Fatalf("promoted sibling must receive holder hello_ack after expiry, got %+v", msgs)
	}
}

func TestDevReloadOrdinaryGoodbyePromotesImmediatelyWithoutReservation(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	msgs, _ := runSyncAs(t, b, sessB, helloAs("0.15.0"))
	if busy := firstOfType(msgs, protocol.MsgError); busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("B hello must be denied, got %+v", msgs)
	}
	// No dev_reload latched — ordinary disconnect.
	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatalf("goodbye Sync: %v", err)
	}
	msgs, _ = runSyncAs(t, b, sessB)
	b.mu.Lock()
	holderID := ""
	if b.holder != nil {
		holderID = b.holder.ID
	}
	b.mu.Unlock()
	if holderID != sessB {
		t.Fatalf("ordinary goodbye must promote pending immediately, holder=%q want %q", holderID, sessB)
	}
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRoleHolder {
		t.Fatalf("promoted sibling must receive holder hello_ack on ordinary goodbye, got %+v", msgs)
	}
}

func TestDevReloadExplicitClaimOverridesReservation(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	runSyncAs(t, b, sessB, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}
	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatalf("goodbye Sync: %v", err)
	}
	b.mu.Lock()
	if b.holder != nil {
		b.mu.Unlock()
		t.Fatalf("holder must be nil before explicit claim, got %+v", b.holder)
	}
	if !b.devReloadReserved(b.now()) {
		b.mu.Unlock()
		t.Fatalf("reservation must be live before Claim override")
	}
	b.mu.Unlock()
	claimed, err := b.Claim(sessB)
	if err != nil {
		t.Fatalf("Claim(%q): %v", sessB, err)
	}
	if claimed != sessB {
		t.Fatalf("Claim returned %q, want %q", claimed, sessB)
	}
	b.mu.Lock()
	holderID := ""
	if b.holder != nil {
		holderID = b.holder.ID
	}
	reserved := b.devReloadReserved(b.now())
	b.mu.Unlock()
	if holderID != sessB {
		t.Fatalf("explicit Claim must make B the holder, got %q", holderID)
	}
	if reserved {
		t.Fatalf("reservation must be cleared after explicit Claim")
	}
	msgs, _ = runSyncAs(t, b, sessB)
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRoleHolder {
		t.Fatalf("claimed holder must receive holder hello_ack, got %+v", msgs)
	}
}

// A latch belongs to the session it was created for. When that session stops
// holding the bridge before the frame is emitted, the latch must be dropped:
// left in place it fired on any later promotion, restarting a browser long
// after the command that asked for it had been reported as failed.
func TestDevReloadLatchDroppedWhenHolderIsDemoted(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	runSyncAs(t, b, sessB, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	// Demote A before it ever polls, so the latch is still un-emitted.
	if _, err := b.Claim(sessB); err != nil {
		t.Fatalf("Claim(%q): %v", sessB, err)
	}
	// Hand the bridge back. A must not inherit the dropped latch.
	if _, err := b.Claim(sessA); err != nil {
		t.Fatalf("Claim(%q): %v", sessA, err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if frame := firstOfType(msgs, protocol.MsgDevReload); frame != nil {
		t.Fatalf("re-promoted session inherited a stale latch: %+v", frame)
	}
}

// The IPC server answers independent calls concurrently, so two dev_reload
// requests can both land before the holder's next poll. Overwriting the first
// would return the caller a reload_id that is never delivered, and that id is
// the only thing making a reload auditable.
func TestDevReloadRequestIsIdempotentUntilEmitted(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	firstSession, firstID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("first RequestDevReload: %v", err)
	}
	secondSession, secondID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("second RequestDevReload: %v", err)
	}
	if secondID != firstID {
		t.Fatalf("second request minted %q, want the un-emitted %q", secondID, firstID)
	}
	if secondSession != firstSession {
		t.Fatalf("second request named session %q, want %q", secondSession, firstSession)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	frame := firstOfType(msgs, protocol.MsgDevReload)
	if frame == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}
	if got := frame.Payload.(*protocol.DevReloadPayload).ReloadID; got != firstID {
		t.Fatalf("emitted reload_id %q, want the id both callers were given, %q", got, firstID)
	}
	// The latch is still one-shot: a third request after the emit mints a new id.
	_, thirdID, err := b.RequestDevReload()
	if err != nil {
		t.Fatalf("third RequestDevReload: %v", err)
	}
	if thirdID == firstID {
		t.Fatalf("request after emit reused %q; the latch must be one-shot", firstID)
	}
}

// An institutional materialization drive must be charged like an accepted
// handoff, because a job driven through claims and effects emits no
// browser.job_accept at all. Measured live 2026-08-23: 10.2196/83927 minted 17
// claims between 02:10 and 04:40 behind 14 authorized effects, 30
// browser.auth_pending and zero browser.job_accept, so this fence saw no drive
// and never quiesced. It held its institution's provider fence throughout,
// which starved 58 sibling handoffs: prepareMaterializationCandidate returned
// nil for each, FocusHandoffs counted zero, and `papio actions open` printed a
// link instead of driving. Reaching a sign-in wall repeatedly is exactly the
// silence this lease bounds.
func TestProjectHandoffOfferStateChargesInstitutionalDrives(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_institutional_fruitless", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	// The measured shape, three times over: authorize an effect, report the
	// wall, return from it - and never deliver. No job_accept anywhere.
	start := created.Add(time.Second)
	for epoch := 0; epoch < job.MaxAutomaticHandoffEpochs; epoch++ {
		at := start.Add(time.Duration(epoch) * (job.HandoffAcceptedLease + time.Minute))
		appendEventAt(t, jobs, id, "browser.institutional_effect_authorized",
			map[string]any{"binding_id": fmt.Sprintf("binding_%d", epoch)}, at)
		appendEventAt(t, jobs, id, "browser.institutional_effect_result",
			map[string]any{"outcome": "acknowledged"}, at.Add(time.Second))
		appendEventAt(t, jobs, id, "browser.auth_pending", map[string]any{}, at.Add(2*time.Second))
		appendEventAt(t, jobs, id, "browser.auth_returned",
			map[string]any{"elapsed_ms": 824}, at.Add(3*time.Second))
	}

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind == "browser.job_accept" {
			t.Fatal("precondition: this path must emit no job_accept, or the test proves the old fence")
		}
	}
	last := start.Add(time.Duration(job.MaxAutomaticHandoffEpochs-1) * (job.HandoffAcceptedLease + time.Minute))
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, last.Add(job.HandoffAcceptedLease+time.Minute))
	if state.FruitlessEpochs != job.MaxAutomaticHandoffEpochs {
		t.Fatalf("fruitless epochs after %d institutional drives into a sign-in wall = %d, want %d",
			job.MaxAutomaticHandoffEpochs, state.FruitlessEpochs, job.MaxAutomaticHandoffEpochs)
	}
	if !state.Quiesced {
		t.Fatal("an institutional drive that only ever reaches a wall must quiesce and release the provider fence")
	}
}

// The same drive that DELIVERS must clear the streak, or capping the
// institutional path would retire papers it is fetching correctly.
func TestProjectHandoffOfferStateInstitutionalDeliveryClearsStreak(t *testing.T) {
	_, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_institutional_delivers", handoffWork())
	action := openHandoffAction(t, jobs, id)
	created, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	start := created.Add(time.Second)
	appendEventAt(t, jobs, id, "browser.institutional_effect_authorized",
		map[string]any{"binding_id": "binding_fruitless"}, start)
	second := start.Add(job.HandoffAcceptedLease + time.Minute)
	appendEventAt(t, jobs, id, "browser.institutional_effect_authorized",
		map[string]any{"binding_id": "binding_delivers"}, second)
	appendEventAt(t, jobs, id, "browser.download_started",
		map[string]any{"bytes": 1}, second.Add(time.Minute))

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, second.Add(job.HandoffAcceptedLease+time.Minute))
	if state.FruitlessEpochs != 0 {
		t.Fatalf("fruitless epochs after an institutional drive that downloaded = %d, want 0", state.FruitlessEpochs)
	}
	if state.Quiesced {
		t.Fatal("quiesced despite delivering the paper")
	}
}
