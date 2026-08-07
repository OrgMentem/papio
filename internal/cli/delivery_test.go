// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
)

func TestDeliveryGetRendersRowGateAndEvaluation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "delivery.get" {
			t.Fatalf("method = %q, want delivery.get", method)
		}
		if got := params.(map[string]string)["job_id"]; got != "job_01" {
			t.Fatalf("job_id = %q, want job_01", got)
		}
		*result.(*api.DeliveryDetail) = api.DeliveryDetail{
			Request: &api.DeliveryRequest{JobID: "job_01", Provider: "illiad", RequestClass: "digital_journal_article", State: "pending", ProviderReference: "TN-9", NextCheckAt: "2026-08-08T00:00:00Z"},
			Gate:    api.DeliveryGateSummary{Class: "auto_capable"},
			LastEvaluation: &api.DeliveryGateEvent{
				ProfileClass: "auto_capable", Decision: "submit",
			},
		}
		return nil
	})
	root.SetArgs([]string{"delivery", "get", "job_01"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery get: %v (%s)", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"job_01", "illiad", "pending", "gate: auto_capable", "last evaluated: submit", "provider reference: TN-9", "next check: 2026-08-08T00:00:00Z"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("stdout = %q, want it to contain %q", got, want)
		}
	}
}

func TestDeliveryGetJSONPassthrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, _ string, _, result any) error {
		*result.(*api.DeliveryDetail) = api.DeliveryDetail{
			Request: &api.DeliveryRequest{JobID: "job_01", Provider: "openurl", State: "offered"},
			Gate:    api.DeliveryGateSummary{Class: "prefill_only", Blockers: []api.DeliveryBlocker{{Code: "provider_not_auto_capable", Evidence: "openurl routes to a form"}}},
		}
		return nil
	})
	root.SetArgs([]string{"--json", "delivery", "get", "job_01"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery get --json: %v (%s)", err, stderr.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v (%s)", err, stdout.Bytes())
	}
	gate, ok := decoded["gate"].(map[string]any)
	if !ok || gate["class"] != "prefill_only" {
		t.Fatalf("decoded = %#v, want gate.class = prefill_only", decoded)
	}
}

func TestDeliveryGetNoRequestPrintsPlainly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, _ string, _, result any) error {
		*result.(*api.DeliveryDetail) = api.DeliveryDetail{}
		return nil
	})
	root.SetArgs([]string{"delivery", "get", "job_01"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery get: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "job_01: no delivery request\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeliverySubmitReportsActionAndBlockers(t *testing.T) {
	for _, test := range []struct {
		name   string
		result api.DeliverySubmitResult
		want   string
	}{
		{name: "not configured", result: api.DeliverySubmitResult{Configured: false}, want: "no document-delivery route configured"},
		{name: "prefill with blockers", result: api.DeliverySubmitResult{Configured: true, Action: "prefill", Blockers: []string{"per_request_copyright_declaration"}}, want: "prefill (per_request_copyright_declaration)"},
		{name: "submit", result: api.DeliverySubmitResult{Configured: true, Action: "submit"}, want: "job_01: submit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
				if method != "delivery.submit" {
					t.Fatalf("method = %q, want delivery.submit", method)
				}
				if got := params.(map[string]string)["job_id"]; got != "job_01" {
					t.Fatalf("job_id = %q, want job_01", got)
				}
				*result.(*api.DeliverySubmitResult) = test.result
				return nil
			})
			root.SetArgs([]string{"delivery", "submit", "job_01"})
			if err := root.Execute(); err != nil {
				t.Fatalf("delivery submit: %v (%s)", err, stderr.String())
			}
			if got := stdout.String(); !bytes.Contains([]byte(got), []byte(test.want)) {
				t.Fatalf("stdout = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

func TestDeliveryCancelReportsSupportAndReason(t *testing.T) {
	for _, test := range []struct {
		name   string
		result api.DeliveryCancelResult
		want   string
	}{
		{name: "cancelled", result: api.DeliveryCancelResult{Cancelled: true, Supported: true, State: "cancelled"}, want: "Cancelled delivery request for job_01"},
		{name: "not supported", result: api.DeliveryCancelResult{Cancelled: false, Supported: false, State: "submitted", Reason: "no configured provider (illiad) supports API cancellation"}, want: "not cancelled (no configured provider (illiad) supports API cancellation)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
				if method != "delivery.cancel" {
					t.Fatalf("method = %q, want delivery.cancel", method)
				}
				if got := params.(map[string]string)["job_id"]; got != "job_01" {
					t.Fatalf("job_id = %q, want job_01", got)
				}
				*result.(*api.DeliveryCancelResult) = test.result
				return nil
			})
			root.SetArgs([]string{"delivery", "cancel", "job_01"})
			if err := root.Execute(); err != nil {
				t.Fatalf("delivery cancel: %v (%s)", err, stderr.String())
			}
			if got := stdout.String(); !bytes.Contains([]byte(got), []byte(test.want)) {
				t.Fatalf("stdout = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

func TestDeliveryHistoryCallsOpenRequestHistoryOperation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "delivery.action" {
			t.Fatalf("method = %q, want delivery.action", method)
		}
		p := params.(map[string]string)
		if p["job_id"] != "job_01" || p["operation"] != "open_request_history" {
			t.Fatalf("params = %#v, want job_id=job_01 operation=open_request_history", p)
		}
		*result.(*api.DeliveryActionResult) = api.DeliveryActionResult{
			JobID: "job_01", Operation: "open_request_history",
			Detail: &api.DeliveryDetail{
				Request: &api.DeliveryRequest{JobID: "job_01", Provider: "illiad", State: "unknown_outcome"},
				Gate:    api.DeliveryGateSummary{Class: "auto_capable"},
			},
		}
		return nil
	})
	root.SetArgs([]string{"delivery", "history", "job_01"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery history: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); !bytes.Contains([]byte(got), []byte("unknown_outcome")) {
		t.Fatalf("stdout = %q, want it to mention unknown_outcome", got)
	}
}

func TestDeliveryConfirmExistsSendsProviderReference(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "delivery.action" {
			t.Fatalf("method = %q, want delivery.action", method)
		}
		p := params.(map[string]string)
		if p["job_id"] != "job_01" || p["operation"] != "confirm_request_exists" || p["provider_reference"] != "TN-42" {
			t.Fatalf("params = %#v", p)
		}
		*result.(*api.DeliveryActionResult) = api.DeliveryActionResult{JobID: "job_01", Operation: "confirm_request_exists", JobState: "retry_wait"}
		return nil
	})
	root.SetArgs([]string{"delivery", "confirm-exists", "job_01", "TN-42"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery confirm-exists: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "job_01: confirmed pending with reference TN-42; job is now retry_wait\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeliveryConfirmAbsentReportsJobState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "delivery.action" {
			t.Fatalf("method = %q, want delivery.action", method)
		}
		p := params.(map[string]string)
		if p["job_id"] != "job_01" || p["operation"] != "confirm_request_absent" {
			t.Fatalf("params = %#v", p)
		}
		if _, hasRef := p["provider_reference"]; hasRef {
			t.Fatalf("confirm-absent must not send provider_reference: %#v", p)
		}
		*result.(*api.DeliveryActionResult) = api.DeliveryActionResult{JobID: "job_01", Operation: "confirm_request_absent", JobState: "awaiting_human"}
		return nil
	})
	root.SetArgs([]string{"delivery", "confirm-absent", "job_01"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery confirm-absent: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "job_01: closed the stale request; job is now awaiting_human\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeliveryResumeSendsRequestIDAndReportsState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		if method != "delivery.resume" {
			t.Fatalf("method = %q, want delivery.resume", method)
		}
		p := params.(map[string]any)
		if p["request_id"] != int64(42) {
			t.Fatalf("params = %#v, want request_id 42 (int64)", p)
		}
		*result.(*api.DeliveryResumeResult) = api.DeliveryResumeResult{RequestID: 42, Resumed: true, State: "pending"}
		return nil
	})
	root.SetArgs([]string{"delivery", "resume", "42"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery resume: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "request 42: resumed (state pending); run 'papio jobs retry <job-id>' to poll now\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeliveryResumeReportsRefusalOnTerminalRow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
		*result.(*api.DeliveryResumeResult) = api.DeliveryResumeResult{
			RequestID: 7, Resumed: false, State: "fulfilled",
			Reason: "state fulfilled is not live (submitted/pending); nothing to resume",
		}
		return nil
	})
	root.SetArgs([]string{"delivery", "resume", "7"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delivery resume: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "request 7: not resumed (state fulfilled is not live (submitted/pending); nothing to resume)\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeliveryResumeRejectsNonPositiveArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(context.Context, string, any, any) error {
		t.Fatal("delivery.resume must not be called with an invalid request-id")
		return nil
	})
	root.SetArgs([]string{"delivery", "resume", "0"})
	if err := root.Execute(); err == nil {
		t.Fatal("delivery resume 0: want an error, got nil")
	}
	root.SetArgs([]string{"delivery", "resume", "not-a-number"})
	if err := root.Execute(); err == nil {
		t.Fatal("delivery resume not-a-number: want an error, got nil")
	}
}
