// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/notify"
)

type notifyTestSender struct{ messages []string }

func (s *notifyTestSender) Send(_ context.Context, message string) {
	s.messages = append(s.messages, message)
}

func testNotifySystem(sender notify.Sender) *bootstrap.System {
	cfg := config.Default()
	policy, _ := notify.ResolvePolicy(cfg.Notify)
	return &bootstrap.System{Config: cfg, Notify: notify.NewRouter(notify.RouterOptions{Policy: policy, Desktop: sender})}
}

func TestNotifyMethodsReturnPurposeBuiltResults(t *testing.T) {
	sender := &notifyTestSender{}
	system := testNotifySystem(sender)
	showRaw, rpcErr := notifyShow(context.Background(), []byte(`{}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var show NotifyShowResult
	if err := json.Unmarshal(showRaw, &show); err != nil {
		t.Fatal(err)
	}
	if show.Preset == "" || len(show.Rows) != len(notify.Categories()) {
		t.Fatalf("show = %+v", show)
	}

	previewRaw, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"request_outcome","count":1}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var preview NotifyPreviewResult
	if err := json.Unmarshal(previewRaw, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Category != "request_outcome" || preview.Count != 1 || preview.Message == "" {
		t.Fatalf("preview = %+v", preview)
	}
	for _, category := range notify.Categories() {
		// Categories that stand for exactly one event reject an aggregate
		// count instead of rendering copy the daemon can never send.
		count := 3
		if notify.CategoryRepresentsOneEvent(category) {
			count = 1
		}
		raw, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"`+string(category)+`","count":`+strconv.Itoa(count)+`}`), system)
		if rpcErr != nil {
			t.Fatalf("preview %s: %v", category, rpcErr)
		}
		var row NotifyPreviewResult
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row.Category != string(category) || row.Count != count || row.Message == "" {
			t.Fatalf("preview %s = %+v", category, row)
		}
		if count > 1 {
			continue
		}
		_, rpcErr = notifyPreview(context.Background(), []byte(`{"category":"`+string(category)+`","count":27}`), system)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("preview %s count 27 = %+v, want invalid_argument", category, rpcErr)
		}
		if !strings.Contains(rpcErr.Message, "count of 27") {
			t.Fatalf("preview %s rejection = %q, want the impossible count explained", category, rpcErr.Message)
		}
	}

	testRaw, rpcErr := notifyTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
	available, _ := notify.PlatformCapability()
	if !available {
		if rpcErr == nil || rpcErr.Code != "unavailable" {
			t.Fatalf("test error = %+v, want unavailable", rpcErr)
		}
		if len(sender.messages) != 0 {
			t.Fatalf("unsupported platform sent %d messages", len(sender.messages))
		}
		return
	}
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var result NotifyTestResult
	if err := json.Unmarshal(testRaw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || result.DeliveryAcknowledged || !strings.Contains(result.Detail, "no delivery acknowledgement") {
		t.Fatalf("test = %+v", result)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %d", len(sender.messages))
	}
}
func TestNotifyShowReportsOverrideSource(t *testing.T) {
	cfg := config.Default()
	cfg.Notify.Categories = map[string]config.NotifyCategory{"request_outcome": {Desktop: "digest"}}
	policy, err := notify.ResolvePolicy(cfg.Notify)
	if err != nil {
		t.Fatal(err)
	}
	system := &bootstrap.System{Config: cfg, Notify: notify.NewRouter(notify.RouterOptions{Policy: policy})}
	raw, rpcErr := notifyShow(context.Background(), []byte(`{}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var result NotifyShowResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range result.Rows {
		if row.Category != "request_outcome" {
			continue
		}
		found = true
		if row.Source != "override" {
			t.Fatalf("row = %+v", row)
		}
	}
	if !found {
		t.Fatal("request_outcome row missing")
	}
}

func TestNotifyMethodsRejectUnknownCategoryAndUnavailableSender(t *testing.T) {
	system := testNotifySystem(nil)
	_, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"bogus"}`), system)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("preview error = %#v", rpcErr)
	}
	for _, valid := range []string{"request_outcome", "decision_opened", "decision_pending", "completion_batch", "discovery_new", "integrity_notice", "system_degraded"} {
		if !strings.Contains(rpcErr.Message, valid) {
			t.Fatalf("unknown category error %q missing %q", rpcErr.Message, valid)
		}
	}
	_, rpcErr = notifyTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
	if rpcErr == nil || rpcErr.Code != "unavailable" || !strings.Contains(rpcErr.Message, "desktop notification") {
		t.Fatalf("test error = %#v", rpcErr)
	}
}

func TestNotifyWebhookTestMethod(t *testing.T) {
	type seen struct{ title, body string }
	requests := make(chan seen, 2)
	status := http.StatusNoContent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{title: r.Header.Get("Title"), body: string(body)}
		w.WriteHeader(status)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.Notify.WebhookFormat = config.WebhookFormatNtfy
	policy, _ := notify.ResolvePolicy(cfg.Notify)
	sender := notify.NewWebhook(server.URL+"/private-topic-token", "")
	sender.Format = cfg.Notify.EffectiveWebhookFormat()
	system := &bootstrap.System{Config: cfg, Notify: notify.NewRouter(notify.RouterOptions{Policy: policy, Webhook: sender})}

	// The existing results keep their exact field set: an older CLI decodes
	// them with DisallowUnknownFields, so a configured webhook must not add
	// a field to either.
	assertKeys := func(name string, raw []byte, want ...string) {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if len(object) != len(want) {
			t.Fatalf("%s keys = %v, want exactly %v", name, object, want)
		}
		for _, key := range want {
			if _, ok := object[key]; !ok {
				t.Fatalf("%s keys = %v, want %v", name, object, want)
			}
		}
	}
	showRaw, rpcErr := notifyShow(context.Background(), []byte(`{}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	assertKeys("show", showRaw, "preset", "rows")
	previewRaw, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"request_outcome"}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	assertKeys("preview", previewRaw, "category", "count", "message")
	if len(requests) != 0 {
		t.Fatal("show/preview sent a request")
	}

	testRaw, rpcErr := notifyWebhookTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var result NotifyWebhookTestResult
	if err := json.Unmarshal(testRaw, &result); err != nil || !result.Acknowledged || result.Format != "ntfy" {
		t.Fatalf("test = %+v err=%v", result, err)
	}
	got := <-requests
	if got.title != "papio: request_outcome" || got.body != result.Message {
		t.Fatalf("request = %+v, want ntfy title and the previewed message", got)
	}

	// A refused delivery is reported without echoing the endpoint URL, whose
	// path can be a secret.
	status = http.StatusInternalServerError
	_, rpcErr = notifyWebhookTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
	if rpcErr == nil || rpcErr.Code != "unavailable" || strings.Contains(rpcErr.Message, "private-topic-token") {
		t.Fatalf("failed webhook test = %+v", rpcErr)
	}

	// No webhook configured: a typed unavailable error, never the desktop path.
	_, rpcErr = notifyWebhookTest(context.Background(), []byte(`{"category":"request_outcome"}`), testNotifySystem(&notifyTestSender{}))
	if rpcErr == nil || rpcErr.Code != "unavailable" || !strings.Contains(rpcErr.Message, "no notification webhook") {
		t.Fatalf("unconfigured webhook test = %+v", rpcErr)
	}
	if _, rpcErr = notifyWebhookTest(context.Background(), []byte(`{"category":"bogus"}`), system); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("unknown category = %+v, want invalid_argument", rpcErr)
	}
}
