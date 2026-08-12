// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
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

	previewRaw, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"request_outcome","count":2}`), system)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var preview NotifyPreviewResult
	if err := json.Unmarshal(previewRaw, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Category != "request_outcome" || preview.Count != 2 || preview.Message == "" {
		t.Fatalf("preview = %+v", preview)
	}
	for _, category := range notify.Categories() {
		raw, rpcErr := notifyPreview(context.Background(), []byte(`{"category":"`+string(category)+`","count":3}`), system)
		if rpcErr != nil {
			t.Fatalf("preview %s: %v", category, rpcErr)
		}
		var row NotifyPreviewResult
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row.Category != string(category) || row.Count != 3 || row.Message == "" {
			t.Fatalf("preview %s = %+v", category, row)
		}
	}

	testRaw, rpcErr := notifyTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
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
