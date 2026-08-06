// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/store"
)

func TestFailuresCommandUsesVersionedReadRPCAndEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	var gotParams map[string]any
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "failures.list_v1" {
			t.Fatalf("RPC method = %q, want failures.list_v1", method)
		}
		gotParams = params.(map[string]any)
		*result.(*api.FailuresPage) = api.FailuresPage{
			Failures: []store.FailureSummary{{
				Provider: "www.jstor.org", Reason: "no_entitlement", Count: 4, ExampleJobID: "job_example",
			}},
			Truncated: true,
		}
		return nil
	})
	root.SetArgs([]string{"--json", "failures", "--limit", "7", "--by-provider"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("failures: %v (%s)", err, errOut.String())
	}
	if !reflect.DeepEqual(gotParams, map[string]any{"limit": 7, "by_provider": true}) {
		t.Fatalf("RPC params = %#v", gotParams)
	}
	var page struct {
		Failures  []store.FailureSummary `json:"failures"`
		Truncated bool                   `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || len(page.Failures) != 1 || page.Failures[0].ExampleJobID != "job_example" {
		t.Fatalf("JSON page = %+v", page)
	}
}

func TestFailuresCommandRendersRequestedHumanTable(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "failures.list_v1" {
			t.Fatalf("RPC method = %q", method)
		}
		if got := params.(map[string]any); !reflect.DeepEqual(got, map[string]any{"limit": store.FailureSummaryLimitDefault, "by_provider": false}) {
			t.Fatalf("RPC params = %#v", got)
		}
		*result.(*api.FailuresPage) = api.FailuresPage{Failures: []store.FailureSummary{{
			Provider: "www.jstor.org", Reason: "login_required", Count: 2, ExampleJobID: "job_latest",
		}}}
		return nil
	})
	root.SetArgs([]string{"failures"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("failures: %v (%s)", err, errOut.String())
	}
	want := "provider/host | reason | count | example job id\nwww.jstor.org | login_required | 2 | job_latest\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestJobsShowIsExactGetAlias(t *testing.T) {
	run := func(verb string) ([]byte, map[string]string) {
		t.Helper()
		var out, errOut bytes.Buffer
		var gotParams map[string]string
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
			// Both verbs go through jobDetail: jobs.get_v2 first, with the
			// jobs.get fallback reserved for a daemon that predates it.
			if method != "jobs.get_v2" {
				t.Fatalf("%s RPC method = %q, want jobs.get_v2", verb, method)
			}
			gotParams = params.(map[string]string)
			*result.(*api.JobDetailV2) = api.JobDetailV2{
				Job: &api.JobRow{Row: job.Row{ID: "job_alias", State: job.StateUnavailable}},
			}
			return nil
		})
		command, _, err := root.Find([]string{"jobs", verb})
		if err != nil {
			t.Fatalf("find jobs %s: %v", verb, err)
		}
		if command.Flags().Lookup("wait") == nil {
			t.Fatalf("jobs %s has no --wait flag", verb)
		}
		root.SetArgs([]string{"--json", "jobs", verb, "job_alias"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("jobs %s: %v (%s)", verb, err, errOut.String())
		}
		return out.Bytes(), gotParams
	}

	getOutput, getParams := run("get")
	showOutput, showParams := run("show")
	if !bytes.Equal(showOutput, getOutput) {
		t.Fatalf("show output = %s, get output = %s", showOutput, getOutput)
	}
	wantParams := map[string]string{"job_id": "job_alias"}
	if !reflect.DeepEqual(showParams, wantParams) || !reflect.DeepEqual(getParams, wantParams) {
		t.Fatalf("show/get params = %#v / %#v, want %#v", showParams, getParams, wantParams)
	}
}
