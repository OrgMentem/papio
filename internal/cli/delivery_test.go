// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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
			Request: &api.DeliveryRequest{ID: 42, JobID: "job_01", Provider: "illiad", RequestClass: "digital_journal_article", State: "pending", ProviderReference: "TN-9", NextCheckAt: "2026-08-08T00:00:00Z"},
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
	for _, want := range []string{"job_01", "illiad", "pending", "request id: 42", "gate: auto_capable", "last evaluated: submit", "provider reference: TN-9", "next check: 2026-08-08T00:00:00Z"} {
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
			root.SetArgs([]string{"delivery", "submit", "job_01", "--confirm"})
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
		// refused: the live provider request was not cancelled, so the
		// command must exit nonzero after printing the reason.
		refused bool
	}{
		{name: "cancelled", result: api.DeliveryCancelResult{Cancelled: true, Supported: true, State: "cancelled"}, want: "Cancelled delivery request for job_01"},
		{name: "already cancelled", result: api.DeliveryCancelResult{Cancelled: true, Supported: true, State: "cancelled", Reason: "already cancelled"}, want: "Cancelled delivery request for job_01"},
		{name: "not supported", result: api.DeliveryCancelResult{Cancelled: false, Supported: false, State: "submitted", Reason: "no configured provider (illiad) supports API cancellation"}, want: "not cancelled (no configured provider (illiad) supports API cancellation)", refused: true},
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
			err := root.Execute()
			if test.refused != errors.Is(err, errDeliveryNotCancelled) {
				t.Fatalf("delivery cancel: err = %v, want refusal %v", err, test.refused)
			}
			if !test.refused && err != nil {
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
	if err := root.Execute(); !errors.Is(err, errDeliveryNotResumed) {
		t.Fatalf("delivery resume: err = %v, want errDeliveryNotResumed", err)
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
	// A job id is the natural mistake: the refusal must echo what was typed,
	// say resume wants a request id instead, and say where to find it.
	for _, arg := range []string{"0", "not-a-number", "job_abc123"} {
		root.SetArgs([]string{"delivery", "resume", arg})
		err := root.Execute()
		if err == nil {
			t.Fatalf("delivery resume %s: want an error, got nil", arg)
		}
		for _, want := range []string{`"` + arg + `"`, "request id", "not a job id", "papio delivery get <job-id>"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("delivery resume %s: err = %q, want it to contain %q", arg, err, want)
			}
		}
	}
}

// TestDeliveryCancelRefusalKeepsJSONAndExitsNonzero: an agent reading --json
// still gets the structured refusal, and the exit status no longer claims the
// live provider request is gone.
func TestDeliveryCancelRefusalKeepsJSONAndExitsNonzero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, _ string, _, result any) error {
		*result.(*api.DeliveryCancelResult) = api.DeliveryCancelResult{JobID: "job_01", State: "pending", Reason: "cancel directly with the institution"}
		return nil
	})
	root.SetArgs([]string{"--json", "delivery", "cancel", "job_01"})
	if err := root.Execute(); !errors.Is(err, errDeliveryNotCancelled) {
		t.Fatalf("err = %v, want errDeliveryNotCancelled", err)
	}
	var decoded api.DeliveryCancelResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v (%s)", err, stdout.Bytes())
	}
	if decoded.Cancelled || decoded.State != "pending" || decoded.Reason == "" {
		t.Fatalf("decoded = %+v, want the daemon's refusal", decoded)
	}
}

// TestDeliverySubmitWithoutConfirmNeverReachesTheProvider: the gate's submit
// verdict places an irreversible provider request, so without --confirm the
// command may only ask the daemon for a read-only preview. It says what
// --confirm would do, including for a job that has no request yet, and exits
// nonzero.
func TestDeliverySubmitWithoutConfirmNeverReachesTheProvider(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		preview api.DeliveryPreviewResult
		want    []string
	}{
		{
			name: "fresh job, auto capable gate", args: []string{"delivery", "submit", "job_01"},
			preview: api.DeliveryPreviewResult{Preview: true, Configured: true, Provider: "illiad", Branch: "evaluate_gate", Verdict: "submit", GateClass: "auto_capable"},
			want:    []string{"job_01: --confirm would place a real provider request with illiad (gate auto_capable)"},
		},
		{
			name: "fresh job, prefill only gate", args: []string{"delivery", "submit", "job_01"},
			preview: api.DeliveryPreviewResult{Preview: true, Configured: true, Provider: "openurl", Branch: "evaluate_gate", Verdict: "prefill", GateClass: "prefill_only", Blockers: []string{"provider_not_auto_capable"}},
			want:    []string{"would open a prefill action", "no provider request", "gate prefill_only: provider_not_auto_capable"},
		},
		{
			name: "existing live request", args: []string{"delivery", "submit", "job_01"},
			preview: api.DeliveryPreviewResult{Preview: true, Configured: true, Provider: "illiad", Branch: "join_poll", Request: &api.DeliveryRequest{ID: 7, Provider: "illiad", State: "pending"}},
			want:    []string{"no new provider request", "request 7", "state pending"},
		},
		{
			name: "no route configured", args: []string{"delivery", "submit", "job_01"},
			preview: api.DeliveryPreviewResult{Preview: true},
			want:    []string{"no document-delivery route is configured", "would do nothing"},
		},
		{
			name: "json", args: []string{"--json", "delivery", "submit", "job_01"},
			preview: api.DeliveryPreviewResult{Preview: true, Configured: true, Provider: "illiad", Branch: "evaluate_gate", Verdict: "submit", GateClass: "auto_capable"},
			want:    []string{`"preview":true`, `"verdict":"submit"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params, result any) error {
				if method != "delivery.preview" {
					t.Fatalf("method = %q without --confirm, want delivery.preview", method)
				}
				if got := params.(map[string]string)["job_id"]; got != "job_01" {
					t.Fatalf("job_id = %q, want job_01", got)
				}
				*result.(*api.DeliveryPreviewResult) = test.preview
				return nil
			})
			root.SetArgs(test.args)
			if err := root.Execute(); !errors.Is(err, errDeliverySubmitUnconfirmed) {
				t.Fatalf("err = %v, want errDeliverySubmitUnconfirmed", err)
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), want)
				}
			}
			if strings.Contains(stdout.String(), `"action"`) {
				t.Fatalf("stdout = %q: a preview must not carry the submit result's action field", stdout.String())
			}
		})
	}
}
