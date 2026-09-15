// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"testing"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
)

func TestJobsUnfiledJSONUsesEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "jobs.unfiled" {
			t.Fatalf("method = %q, want jobs.unfiled", method)
		}
		got := params.(map[string]any)
		if got["filter"] != "failed" || got["limit"] != job.ListLimitDefault {
			t.Fatalf("params = %#v", got)
		}
		*result.(*api.UnfiledPage) = api.UnfiledPage{
			Jobs: []app.UnfiledJob{{
				JobID: "job_filing_01", State: job.StateReady, Title: "Unfiled paper",
				DOI: "10.1000/unfiled", Filing: "failed", LastStatus: "failed",
				LastExitCode: 1, LastAttemptAt: "2026-09-15T12:00:00Z", Attempts: 1,
			}},
		}
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "unfiled", "--filter", "failed"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs unfiled: %v (%s)", err, errOut.String())
	}
	const want = `{"jobs":[{"job_id":"job_filing_01","state":"ready","title":"Unfiled paper","doi":"10.1000/unfiled","filing":"failed","last_status":"failed","last_exit_code":1,"last_attempt_at":"2026-09-15T12:00:00Z","attempts":1}],"truncated":false}` + "\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestJobsRefileHumanOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		exit   int
		want   string
	}{
		{name: "success", status: "ok", exit: 0, want: "filed job_filing_01: ok (exit 0, 120ms)\n"},
		{name: "hook failure", status: "failed", exit: 1, want: "refile failed: job_filing_01: failed (exit 1, 120ms)\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
				if method != "jobs.refile" {
					t.Fatalf("method = %q, want jobs.refile", method)
				}
				if got := params.(map[string]string)["job_id"]; got != "job_filing_01" {
					t.Fatalf("job_id = %q", got)
				}
				*result.(*app.RefileResult) = app.RefileResult{
					JobID: "job_filing_01", Status: test.status, ExitCode: test.exit, DurationMS: 120,
				}
				return nil
			})
			root.SetArgs([]string{"jobs", "refile", "job_filing_01"})
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("jobs refile: %v (%s)", err, errOut.String())
			}
			if out.String() != test.want {
				t.Fatalf("output = %q, want %q", out.String(), test.want)
			}
		})
	}
}
