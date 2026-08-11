// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/incident"
	"papio/internal/job"
)

func TestJobsFailuresKeepsStableCollectionsForEveryResponse(t *testing.T) {
	first := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	failure := job.FailureGroup{
		State: job.StateFailed, Provider: "example.edu", Reason: "timeout", Count: 2, Sample: "job_failure",
	}
	incidentRow := incident.Group{
		Fingerprint: "0123456789abcdef0123456789abcdef", SafetyDomain: "sage",
		HostFamily: "example.edu", Outcome: "ui_changed", Jobs: 3,
		FirstSeen: first, LastSeen: first.Add(time.Hour),
	}

	cases := []struct {
		name      string
		failures  []job.FailureGroup
		incidents []incident.Group
		want      string
	}{
		{
			name:     "ordinary-only",
			failures: []job.FailureGroup{failure},
			want:     `{"failures":[{"state":"failed","provider":"example.edu","reason":"timeout","count":2,"sample":"job_failure"}],"incidents":[],"truncated":false}` + "\n",
		},
		{
			name:      "incident-only",
			incidents: []incident.Group{incidentRow},
			want:      `{"failures":[],"incidents":[{"fingerprint":"0123456789abcdef0123456789abcdef","safety_domain":"sage","host_family":"example.edu","outcome":"ui_changed","jobs":3,"first_seen":"2026-08-01T00:00:00Z","last_seen":"2026-08-01T01:00:00Z"}],"truncated":false}` + "\n",
		},
		{
			name:      "mixed",
			failures:  []job.FailureGroup{failure},
			incidents: []incident.Group{incidentRow},
			want:      `{"failures":[{"state":"failed","provider":"example.edu","reason":"timeout","count":2,"sample":"job_failure"}],"incidents":[{"fingerprint":"0123456789abcdef0123456789abcdef","safety_domain":"sage","host_family":"example.edu","outcome":"ui_changed","jobs":3,"first_seen":"2026-08-01T00:00:00Z","last_seen":"2026-08-01T01:00:00Z"}],"truncated":false}` + "\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				switch method {
				case "jobs.failures":
					*result.(*jobsFailuresResult) = jobsFailuresResult{Failures: tc.failures}
				case "jobs.incidents":
					*result.(*jobsIncidentsResult) = jobsIncidentsResult{Incidents: tc.incidents}
				default:
					t.Fatalf("method = %q", method)
				}
				return nil
			})
			root.SetArgs([]string{"--json", "jobs", "failures"})
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("jobs failures: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJobsFailuresHumanOutputIncludesBothCollections(t *testing.T) {
	var out, errOut bytes.Buffer
	first := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.failures":
			*result.(*jobsFailuresResult) = jobsFailuresResult{Failures: []job.FailureGroup{{
				State: job.StateFailed, Provider: "example.edu", Reason: "timeout", Count: 2, Sample: "job_failure",
			}}}
		case "jobs.incidents":
			*result.(*jobsIncidentsResult) = jobsIncidentsResult{Incidents: []incident.Group{{
				Fingerprint: "0123456789abcdef0123456789abcdef", SafetyDomain: "sage",
				HostFamily: "example.edu", Outcome: "ui_changed", Jobs: 3,
				FirstSeen: first, LastSeen: first.Add(time.Hour),
			}}}
		default:
			t.Fatalf("method = %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"jobs", "failures"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs failures: %v", err)
	}
	want := "2 | failed | example.edu | timeout (sample: job_failure)\n" +
		"0123456789abcdef0123456789abcdef | sage | example.edu | ui_changed | 3 | 2026-08-01T00:00:00Z | 2026-08-01T01:00:00Z\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestJobsIncidentsUseSeparateEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	first := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	want := incident.Group{
		Fingerprint: "0123456789abcdef0123456789abcdef", SafetyDomain: "sage",
		HostFamily: "example.edu", Outcome: "ui_changed", Jobs: 3,
		FirstSeen: first, LastSeen: first.Add(time.Hour),
	}
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "jobs.incidents" {
			t.Fatalf("method = %q, want jobs.incidents", method)
		}
		*result.(*jobsIncidentsResult) = jobsIncidentsResult{Incidents: []incident.Group{want}}
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "incidents"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs incidents: %v", err)
	}
	const expected = `{"incidents":[{"fingerprint":"0123456789abcdef0123456789abcdef","safety_domain":"sage","host_family":"example.edu","outcome":"ui_changed","jobs":3,"first_seen":"2026-08-01T00:00:00Z","last_seen":"2026-08-01T01:00:00Z"}],"truncated":false}` + "\n"
	if out.String() != expected {
		t.Fatalf("output = %q, want %q", out.String(), expected)
	}
}
