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
	"papio/internal/resolver"
	"papio/internal/retraction"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
)

func newBridge(t *testing.T) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	return newBridgeWithHoldings(t, nil)
}

func newBridgeWithHoldings(t *testing.T, holdings holdingsProvider) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	return newBridgeWithHoldingsAndZotio(t, holdings, nil)
}

func newBridgeWithHoldingsAndZotio(t *testing.T, holdings holdingsProvider, zotioService *zotio.Service) (*Bridge, *job.Store, config.Config, string) {
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
	if !slices.Equal(payload.Features, []string{
		pageAcquireFeature, triageSnapshotFeature, triageSnapshotSchema2Feature, triageMutationsFeature, reviewPreviewFeature, statsFeature, pageCaptureFeature, pageCaptureRequestFeature, activityFeedFeature, triageCountsSchema2Feature, sessionEvidenceFeature, deliveryContextFeature, pageCaptureTermsFeature, pageBulkAcquireFeature, triageSnapshotSchema3Feature, triageSnapshotSchema4Feature, pdfGrabV1Feature, handoffLinkV1Feature, providerDirectGetV1Feature, providerDriveEpochV1Feature, institutionalMaterializationFeature,
	}) {
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

func materializationHello(t *testing.T) json.RawMessage {
	t.Helper()
	return inFrame(t, protocol.MsgHello, "", map[string]any{
		"extension_version": "0.14.0",
		"features":          []string{institutionalMaterializationFeature},
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
		Features:         []string{institutionalMaterializationFeature},
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
// bounded by adoptionScanDeadline, ordinary handoff offers must keep
// flowing, and — because Go can never cancel the blocked syscall — at most
// one goroutine may ever be stuck in it, no matter how many polls arrive
// while it is latched.
func TestPollSuspendsAdoptionScanningOnHungReadDirAndStaysResponsive(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
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

	// Establish the compatible holder without offering the latched job.
	runSync(t, b, helloAs("0.14.0"))
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
	b, _, cfg, _ := newBridge(t)
	runSync(t, b, hello())
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
}

// TestPdfGrabRefusesOnUnhealthyLatch pins ADR-0020's fail-closed refusal: a
// missing-capability outcome, structured (never a raw Go error), and no
// grab is allocated (no landing directory left behind).
func TestPdfGrabRefusesOnUnhealthyLatch(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	runSync(t, b, hello())
	b.adoptionScanSuspended = true
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabRequest, "", map[string]any{
		"request_id": "grab-req-0002", "host": "pdf.example.org",
	}))
	got := firstOfType(msgs, protocol.MsgPdfGrabResult)
	if got == nil {
		t.Fatalf("no pdf_grab_result frame: %+v", msgs)
	}
	p := got.Payload.(*protocol.PdfGrabResultPayload)
	if p.Outcome != "unavailable" || p.GrabID != "" || p.SteeringPath != "" || p.Detail == "" {
		t.Fatalf("payload = %+v, want a structured unavailable refusal with no grab_id", p)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("refusal must not allocate a grab: found %d landing dirs", len(entries))
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
// half of Decision 4: an already-live job for the same DOI is joined, never
// duplicated, and the grab reports already_owned.
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
	if got.Outcome != "already_owned" || got.JobID != existingID {
		t.Fatalf("grab = %+v, want already_owned pointing at the existing job %s", got, existingID)
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

// TestSweepGrabsSkipsTickOnHungRoot mirrors
// TestSweepsSkipTickOnHungAdoptionRoot for the grabs/ subtree: a TCC-hung
// root must never wedge the grab sweeper, and the shared latch must not
// stack a second hung goroutine underneath it.
func TestSweepGrabsSkipsTickOnHungRoot(t *testing.T) {
	b, _, _, _ := newBridge(t)
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
	msgs, _ := runSync(t, b, helloAs("0.14.0"))
	req := firstOfType(msgs, protocol.MsgProviderDirectGetRequest)
	if req == nil {
		t.Fatalf("missing provider direct request: %v", msgs)
	}
	if firstOfType(msgs, protocol.MsgJobOffer) != nil {
		t.Fatalf("direct URL leaked through job_offer: %v", msgs)
	}
	p := req.Payload.(*protocol.ProviderDirectGetRequestPayload)
	result := inFrame(t, protocol.MsgProviderDirectGetResult, id, protocol.ProviderDirectGetResultPayload{
		DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal, RouteRevision: p.RouteRevision,
		Outcome: "not_pdf", LandingClass: "html",
	})
	runSync(t, b, result)
	runSync(t, b, result)
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
	msgs, _ := runSync(t, b, helloAs(DirectRouteMinExtensionVersion))
	if countJobOffersFor(msgs, id) != 1 {
		t.Fatalf("offer count = %d, want ordinary offer", countJobOffersFor(msgs, id))
	}
	if got := directRouteOfferURL(t, msgs); strings.Contains(got, "onlinelibrary.wiley.com") {
		t.Fatalf("job without provider evidence received direct route %q", got)
	}
}

func TestDirectRoutesRequireDelegationAndExtensionFloor(t *testing.T) {
	t.Run("assisted job receives ordinary offer only", func(t *testing.T) {
		b, jobs, _, _ := newBridge(t)
		parkWithPolicyMode(t, jobs, "wr_direct_route_assisted", "10.1002/assisted", config.ModeAssisted)
		msgs, _ := runSync(t, b, helloAs(DirectRouteMinExtensionVersion))
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
	msgs, _ := runSync(t, b, helloAs("0.14.0"))
	if countJobOffersFor(msgs, id) != 1 {
		t.Fatalf("latched job ordinary offers = %d, want 1", countJobOffersFor(msgs, id))
	}
	if firstOfType(msgs, protocol.MsgProviderDirectGetRequest) != nil {
		t.Fatalf("latched job received direct route request: %v", msgs)
	}
}
func TestProviderDriveEpochTupleLifecycle(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
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
		if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_superseded", map[string]any{
			"drive_attempt_id": attempt, "ordinal": int64(0), "strategy": "generic", "revision": "1",
			"safety_domain": "institution:example.edu",
		}); err != nil {
			t.Fatal(err)
		}
		// Everything before this boundary is authorized history: the first
		// start belongs to the pre-retry epoch, while superseded marks the
		// lifecycle boundary after which this tuple is stale. Snapshot both
		// the event sequence and mutable job state so the assertions below
		// inspect only effects of the late frames.
		boundary := snapshotProviderEpochLifecycleBoundary(t, jobs, id)
		if _, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
		}); err != nil {
			t.Fatal(err)
		}
		assertNoProviderEpochMutationSince(t, jobs, id, boundary)
		if _, err := b.providerDriveEpochResult(ctx, id, &protocol.ProviderDriveEpochResultRequestPayload{
			DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1", Outcome: "wrong_work",
		}); err != nil {
			t.Fatal(err)
		}
		assertNoProviderEpochMutationSince(t, jobs, id, boundary)
	})
}

type providerEpochLifecycleBoundary struct {
	eventSeqs []int64
	state     string
}

func snapshotProviderEpochLifecycleBoundary(t *testing.T, jobs *job.Store, id string) providerEpochLifecycleBoundary {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
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
	return providerEpochLifecycleBoundary{eventSeqs: seqs, state: row.State}
}

func assertNoProviderEpochMutationSince(t *testing.T, jobs *job.Store, id string, boundary providerEpochLifecycleBoundary) {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < len(boundary.eventSeqs) {
		t.Fatalf("event sequence shrank after late provider frame: before=%d after=%d", len(boundary.eventSeqs), len(events))
	}
	for i, wantSeq := range boundary.eventSeqs {
		seq, ok := events[i]["seq"].(int64)
		if !ok || seq != wantSeq {
			t.Fatalf("event sequence changed before late-frame boundary at index %d: got=%#v want=%d", i, events[i]["seq"], wantSeq)
		}
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != boundary.state {
		t.Fatalf("job state changed after late provider frame: got=%q want=%q", row.State, boundary.state)
	}
	for _, event := range events[len(boundary.eventSeqs):] {
		switch event["kind"] {
		case "browser.provider_drive_epoch_started", "browser.provider_drive_epoch_result", providerLatchEventKind:
			t.Fatalf("late provider frame mutated job: %#v", event)
		}
	}
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
	if got := msg.Payload.(*protocol.ProviderDriveEpochResultPayload).Outcome; got != "stale" {
		t.Fatalf("stale wrong-work result = %q, want stale", got)
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
		if detail["safety_domain"] == "institution:old.example" || detail["safety_domain"] == "institution:new.example" {
			t.Fatalf("stale tuple created safety latch: %#v", detail)
		}
	}
}
func TestInstitutionalRouteProfileFencePrecedesURLDerivation(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
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
		CandidateID: candidate.ID, BrowserHolderGeneration: int64(b.epoch),
		JobAttemptRevision:         candidate.JobAttemptRevision,
		InstitutionProfileRevision: candidate.InstitutionProfileRevision,
		RouteRevision:              candidate.RouteRevision, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, int64(b.epoch), profile.Revision, 0); err != nil {
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
		CandidateID: candidate.ID, BrowserHolderGeneration: int64(b.epoch),
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, int64(b.epoch), profile.Revision, 3); err != nil {
		t.Fatal(err)
	}
	ordinal, err := jobs.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, int64(b.epoch), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, int64(b.epoch), ordinal, 3); err != nil {
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
	if winner.CandidateID != candidate.ID || winner.BrowserHolderGeneration != int64(b.epoch) {
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
		CandidateID: candidate.ID, BrowserHolderGeneration: int64(b.epoch),
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, int64(b.epoch), profile.Revision, 0); err != nil {
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
		CandidateID: candidate.ID, BrowserHolderGeneration: int64(b.epoch),
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindMaterialization(ctx, claim.ID, claim.BindingID, int64(b.epoch), profile.Revision, 0); err != nil {
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
		{protocol.MsgInstitutionalRouteRequest, "job_inst_001", protocol.InstitutionalRouteRequestPayload{RequestID: "req_inst_003", ClaimID: "claim_001", BindingID: "bind_001"}, protocol.MsgInstitutionalRouteResponse},
		{protocol.MsgInstitutionalNavigatedRequest, "job_inst_001", protocol.InstitutionalNavigatedRequestPayload{RequestID: "req_inst_004", ClaimID: "claim_001", BindingID: "bind_001", RouteIssuanceOrdinal: 1, TabID: 4}, protocol.MsgInstitutionalNavigatedResponse},
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
		"features":          []string{protocol.InstitutionalMaterializationFeature},
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
	b.promote(&browserSession{ID: replacement, ExtensionVersion: "0.14.0", Features: []string{institutionalMaterializationFeature}, LastSyncAt: b.now()}, "live claim restart")
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
		Features: []string{institutionalMaterializationFeature}, LastSyncAt: b.now(),
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
	if b.epoch == uint64(firstGeneration) {
		t.Fatalf("holder takeover did not advance generation: %d", firstGeneration)
	}
	current, found, err := jobs.CurrentProfileEvidence(context.Background(), mustProfileID(t, jobs, "alpha"), 1, int64(b.epoch))
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
	if _, ok, err := jobs.CurrentProfileEvidence(context.Background(), alpha.ID, alpha.Revision, int64(b.epoch)); err != nil || !ok {
		t.Fatalf("alpha warm evidence missing: ok=%v err=%v", ok, err)
	}
	if _, ok, err := jobs.CurrentProfileEvidence(context.Background(), beta.ID, beta.Revision, int64(b.epoch)); err != nil || ok {
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
