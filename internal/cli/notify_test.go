// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/notify"
)

func TestNotifyShowTextAndJSONEnvelope(t *testing.T) {
	stub := func(_ context.Context, method string, _ any, result any) error {
		if method != "notify.show_v1" {
			t.Fatalf("method = %q", method)
		}
		result.(*api.NotifyShowResult).Preset = "milestones"
		result.(*api.NotifyShowResult).Rows = []api.NotifyRouteRow{{Category: "request_outcome", Desktop: "immediate", Webhook: "immediate", WindowSeconds: 60, Source: "preset"}}
		return nil
	}
	var text bytes.Buffer
	root := NewInProcessRoot(&text, &bytes.Buffer{}, config.Config{}, stub)
	root.SetArgs([]string{"notify", "show"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "preset: milestones") || !strings.Contains(text.String(), "request_outcome") {
		t.Fatalf("text = %q", text.String())
	}

	var encoded bytes.Buffer
	root = NewInProcessRoot(&encoded, &bytes.Buffer{}, config.Config{}, stub)
	root.SetArgs([]string{"notify", "show", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Rows []struct {
			api.NotifyRouteRow
			Preset string `json:"preset"`
		} `json:"rows"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	// The text form names the effective preset; --json used to drop it.
	if len(envelope.Rows) != 1 || envelope.Truncated || envelope.Rows[0].Preset != "milestones" || envelope.Rows[0].Category != "request_outcome" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestNotifyPreviewAndTestCommands(t *testing.T) {
	var methods []string
	stub := func(_ context.Context, method string, params any, result any) error {
		methods = append(methods, method)
		switch method {
		case "notify.preview_v1":
			p := params.(api.NotifyPreviewParams)
			*result.(*api.NotifyPreviewResult) = api.NotifyPreviewResult{Category: p.Category, Count: p.Count, Message: "preview"}
		case "notify.test_v1":
			*result.(*api.NotifyTestResult) = api.NotifyTestResult{Message: "test", Detail: "the platform provides no delivery acknowledgement"}
		}
		return nil
	}
	var out bytes.Buffer
	root := NewInProcessRoot(&out, &bytes.Buffer{}, config.Config{}, stub)
	root.SetArgs([]string{"notify", "preview", string(notify.CategoryRequestOutcome), "--count", "3"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	root.SetArgs([]string{"notify", "test", string(notify.CategoryRequestOutcome)})
	testErr := root.Execute()
	available, _ := notify.PlatformCapability()
	if !available {
		if testErr == nil || !strings.Contains(testErr.Error(), "desktop notifications are unavailable") {
			t.Fatalf("test error = %v, want platform unavailable", testErr)
		}
		if strings.Join(methods, ",") != "notify.preview_v1" {
			t.Fatalf("unsupported platform methods = %v", methods)
		}
		if !strings.Contains(out.String(), "preview") {
			t.Fatalf("output = %q", out.String())
		}
		return
	}
	if testErr != nil {
		t.Fatal(testErr)
	}
	if !strings.Contains(out.String(), "preview") || !strings.Contains(out.String(), "no delivery acknowledgement") {
		t.Fatalf("output = %q", out.String())
	}
	if strings.Join(methods, ",") != "notify.preview_v1,notify.test_v1" {
		t.Fatalf("methods = %v", methods)
	}
}

// `notify test --webhook` must call the new webhook method (never widening
// notify.test_v1), even where no desktop sender exists, and show/preview read
// the format from the CLI's own config, so no daemon result grows a field.
func TestNotifyWebhookFormatCommands(t *testing.T) {
	var methods []string
	stub := func(_ context.Context, method string, params any, result any) error {
		methods = append(methods, method)
		switch method {
		case "notify.show_v1":
			*result.(*api.NotifyShowResult) = api.NotifyShowResult{Preset: "milestones", Rows: []api.NotifyRouteRow{{Category: "request_outcome"}}}
		case "notify.preview_v1":
			*result.(*api.NotifyPreviewResult) = api.NotifyPreviewResult{Message: "preview copy"}
		case "notify.webhook_test_v1":
			if params.(api.NotifyWebhookTestParams).Category != "request_outcome" {
				t.Errorf("params = %+v", params)
			}
			*result.(*api.NotifyWebhookTestResult) = api.NotifyWebhookTestResult{Message: "test copy", Format: "ntfy", Acknowledged: true, Detail: "posted to the configured webhook as ntfy"}
		}
		return nil
	}
	configured := config.Config{Notify: config.Notify{WebhookURL: "https://ntfy.example.test/topic", WebhookFormat: config.WebhookFormatNtfy}}
	run := func(cfg config.Config, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		root := NewInProcessRoot(&out, &bytes.Buffer{}, cfg, stub)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run(configured, "notify", "show"); !strings.Contains(got, "webhook_format: ntfy") {
		t.Fatalf("show = %q", got)
	}
	if got := run(configured, "notify", "show", "--json"); !strings.Contains(got, `"webhook_format":"ntfy"`) {
		t.Fatalf("show --json = %q", got)
	}
	if got := run(configured, "notify", "preview", "request_outcome"); !strings.Contains(got, "preview copy") || !strings.Contains(got, "webhook_format: ntfy") {
		t.Fatalf("preview = %q", got)
	}
	if got := run(configured, "notify", "preview", "request_outcome", "--json"); !strings.Contains(got, `"webhook_format":"ntfy"`) || !strings.Contains(got, `"message":"preview copy"`) {
		t.Fatalf("preview --json = %q", got)
	}
	// A webhook record is configured by reference; the default format is json.
	byRecord := config.Config{Notify: config.Notify{WebhookCredentialRef: "env:PAPIO_WEBHOOK"}}
	if got := run(byRecord, "notify", "show"); !strings.Contains(got, "webhook_format: json") {
		t.Fatalf("show with a webhook record = %q", got)
	}
	// No webhook: nothing to report.
	for _, args := range [][]string{{"notify", "show"}, {"notify", "preview", "request_outcome"}} {
		if got := run(config.Config{}, args...); strings.Contains(got, "webhook_format") {
			t.Fatalf("%v without a webhook = %q", args, got)
		}
	}
	methods = nil
	if got := run(configured, "notify", "test", "request_outcome", "--webhook"); !strings.Contains(got, "posted to the configured webhook as ntfy") {
		t.Fatalf("test = %q", got)
	}
	if strings.Join(methods, ",") != "notify.webhook_test_v1" {
		t.Fatalf("methods = %v, want only notify.webhook_test_v1", methods)
	}
}
