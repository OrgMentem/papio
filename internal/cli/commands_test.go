// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"papio/internal/app"
	"papio/internal/browser"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func TestAccessHintClassifiesOpenAndInstitutionalAccess(t *testing.T) {
	tests := []struct {
		name   string
		action job.HumanAction
		want   string
	}{
		{
			name:   "open access",
			action: job.HumanAction{RequiresAuth: false, BlockedBy: "anti_bot"},
			want:   "\topen access — no login needed",
		},
		{
			name:   "institutional",
			action: job.HumanAction{RequiresAuth: true, BlockedBy: "paywall"},
			want:   "\tsign in to your institution first, then 'papio actions open'",
		},
		{
			name: "unclassified",
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := accessHint(test.action); got != test.want {
				t.Fatalf("access hint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestActionURLsSelectAwaitingActionsMostRecentAndDryRun(t *testing.T) {
	base := "https://openurl.example.test/resolve"
	instituteBase := "https://institute.example.test/resolve"
	baseFor := func(name string) (string, bool) {
		switch name {
		case "", "default":
			return base, true
		case "institute":
			return instituteBase, true
		}
		return "", false
	}
	oaURL := "https://oa.example.test/paper.pdf"
	rows := []job.Row{
		{ID: "oa", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/oa"}},
		{ID: "institutional", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/institutional", Title: "Institutional"}},
		{ID: "review", State: job.StateNeedsReview, Work: work.Work{DOI: "10.1000/review"}},
		{ID: "manual", State: job.StateAwaitingHuman, Work: work.Work{DOI: "10.1000/manual"}},
		{ID: "profiled", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "institute"}, Work: work.Work{DOI: "10.1000/profiled"}},
		{ID: "unknownprofile", State: job.StateAwaitingHuman, Policy: job.Policy{Resolver: "gone"}, Work: work.Work{DOI: "10.1000/unknown"}},
	}
	actions := []job.HumanAction{
		{ID: 6, JobID: "profiled", Kind: "openurl_handoff", Status: "open"},
		{ID: 5, JobID: "unknownprofile", Kind: "openurl_handoff", Status: "open"},
		{ID: 4, JobID: "oa", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(oaURL)},
		{ID: 3, JobID: "institutional", Kind: "openurl_handoff", Status: "open"},
		{ID: 2, JobID: "review", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail("https://oa.example.test/review.pdf")},
		{ID: 1, JobID: "manual", Kind: "manual_download", Status: "open", Detail: "choose a file"},
	}

	want := []string{browser.OpenURL(instituteBase, rows[4].Work), oaURL, browser.OpenURL(base, rows[1].Work)}
	if got := actionURLs(actions, rows, baseFor, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs = %#v, want %#v", got, want)
	}
	if got := actionURLs(actions, rows, baseFor, 1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("limited URLs = %#v, want %#v", got, want[:1])
	}

	var out bytes.Buffer
	if err := openActionURLs(context.Background(), want, true, &out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != want[0]+"\n"+want[1]+"\n"+want[2]+"\n" {
		t.Fatalf("dry-run output = %q", got)
	}
}

func TestJobsFailuresCommandOutputsGroups(t *testing.T) {
	want := jobsFailuresResult{
		Failures: []job.FailureGroup{{Count: 2, State: job.StateFailed, Provider: "api.example.test", Reason: "timeout", Sample: "job_01"}},
		Since:    "2026-01-01T00:00:00Z",
	}
	tests := []struct {
		name string
		json bool
	}{
		{name: "aligned rows"},
		{name: "json", json: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var gotParams map[string]any
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params, result any) error {
				if method != "jobs.failures" {
					t.Fatalf("method = %q, want jobs.failures", method)
				}
				gotParams = params.(map[string]any)
				*result.(*jobsFailuresResult) = want
				return nil
			})
			args := []string{"jobs", "failures", "--since", "30d", "--limit", "2"}
			if tc.json {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("jobs failures: %v", err)
			}
			if !reflect.DeepEqual(gotParams, map[string]any{"since": "30d", "limit": 2}) {
				t.Fatalf("params = %#v", gotParams)
			}
			if tc.json {
				var got jobsFailuresResult
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatalf("decode output: %v", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("JSON = %#v, want %#v", got, want)
				}
				return
			}
			if got := out.String(); got != "2 | failed | api.example.test | timeout (sample: job_01)\n" {
				t.Fatalf("output = %q", got)
			}
		})
	}

	command := newJobsCommand(&options{})
	failures, _, err := command.Find([]string{"failures"})
	if err != nil {
		t.Fatalf("find failures command: %v", err)
	}
	if failures.Annotations["mcp:read-only"] != "true" {
		t.Fatalf("failures annotations = %#v", failures.Annotations)
	}
}
