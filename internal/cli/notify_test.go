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
		Rows      []api.NotifyRouteRow `json:"rows"`
		Truncated bool                 `json:"truncated"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Rows) != 1 || envelope.Truncated {
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
