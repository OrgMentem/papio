// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func TestAdapterDiagnoseCommandRendersReport(t *testing.T) {
	detail := diagnoseTestDetail()
	for _, tc := range []struct {
		name string
		json bool
	}{
		{name: "human"},
		{name: "json", json: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var methods []string
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
				methods = append(methods, method)
				switch method {
				case "jobs.get":
					if got, want := params, map[string]string{"job_id": "job_01"}; !reflect.DeepEqual(got, want) {
						t.Fatalf("jobs.get params = %#v, want %#v", got, want)
					}
					*result.(*api.JobDetail) = detail
				case "ping":
					*result.(*daemonPingResult) = daemonPingResult{Version: "0.8.0", ExtensionConnected: true}
				default:
					t.Fatalf("unexpected RPC %q", method)
				}
				return nil
			})
			args := []string{"adapter", "diagnose", "job_01"}
			if tc.json {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("adapter diagnose: %v", err)
			}
			if want := []string{"jobs.get", "ping"}; !reflect.DeepEqual(methods, want) {
				t.Fatalf("RPC methods = %#v, want %#v", methods, want)
			}

			if tc.json {
				var report diagnoseReport
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatalf("decode output: %v", err)
				}
				if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
					t.Fatalf("generated_at = %q: %v", report.GeneratedAt, err)
				}
				if report.DaemonVersion != "0.8.0" || !report.ExtensionConnected {
					t.Fatalf("daemon report = %#v", report)
				}
				if report.Job.ID != "job_01" || report.Job.Policy.AccessMode != "assisted" || report.Job.Policy.ResolverProfile != "campus" {
					t.Fatalf("job = %#v", report.Job)
				}
				if got, want := report.ProviderHosts, []string{"https://provider.example.com"}; !reflect.DeepEqual(got, want) {
					t.Fatalf("provider hosts = %#v, want %#v", got, want)
				}
				if len(report.Actions) != 1 || !report.Actions[0].RequiresAuth || report.Actions[0].BlockedBy != "institutional_sign_in" {
					t.Fatalf("actions = %#v", report.Actions)
				}
				if len(report.Events) != 1 || report.Events[0].Kind != "adapter.failure" {
					t.Fatalf("events = %#v", report.Events)
				}
				return
			}
			got := out.String()
			for _, want := range []string{
				"Job: job_01 awaiting_human (assisted; resolver campus)",
				"Provider hosts: https://provider.example.com",
				"requires authentication; blocked by institutional_sign_in",
				"2026-07-22T12:01:00Z adapter.failure",
				"This report is sanitized and safe to attach to an issue.",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q: %q", want, got)
				}
			}
		})
	}
}

func TestAdapterDiagnoseRedactsURLsInBothOutputModes(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
			switch method {
			case "jobs.get":
				*result.(*api.JobDetail) = diagnoseTestDetail()
			case "ping":
				*result.(*daemonPingResult) = daemonPingResult{}
			default:
				t.Fatalf("unexpected RPC %q", method)
			}
			return nil
		})
		args := []string{"adapter", "diagnose", "job_01"}
		if jsonOutput {
			args = append([]string{"--json"}, args...)
		}
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("adapter diagnose: %v", err)
		}
		if got := out.String(); strings.Contains(got, "token=SECRET") {
			t.Fatalf("secret leaked in output: %q", got)
		}
	}
}

func TestAdapterDiagnoseCommandIsReadOnly(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
		t.Fatalf("unexpected RPC %q", method)
		return nil
	})
	diagnose, _, err := root.Find([]string{"adapter", "diagnose"})
	if err != nil {
		t.Fatalf("find adapter diagnose: %v", err)
	}
	if diagnose.Annotations["mcp:read-only"] != "true" {
		t.Fatalf("diagnose annotations = %#v", diagnose.Annotations)
	}
}

func diagnoseTestDetail() api.JobDetail {
	const providerURL = "https://provider.example.com/pdf?token=SECRET"
	return api.JobDetail{
		Job: &job.Row{
			ID:        "job_01",
			State:     job.StateAwaitingHuman,
			CreatedAt: "2026-07-22T12:00:00Z",
			UpdatedAt: "2026-07-22T12:01:00Z",
			Policy:    job.Policy{AccessMode: "assisted", Resolver: "campus"},
			Work:      work.Work{DOI: "10.1000/example", PMID: "12345678", Title: "Example paper"},
		},
		Actions: []job.HumanAction{{
			Kind:         "browser_handoff",
			Status:       "open",
			RequiresAuth: true,
			BlockedBy:    "institutional_sign_in",
			Revision:     3,
			CreatedAt:    "2026-07-22T12:01:00Z",
			Detail:       "Open " + providerURL,
		}},
		Events: []map[string]any{{
			"at":     "2026-07-22T12:01:00Z",
			"kind":   "adapter.failure",
			"detail": map[string]any{"provider_url": providerURL},
		}},
	}
}
