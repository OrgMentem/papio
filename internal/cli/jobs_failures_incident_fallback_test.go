// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

func TestJobsFailuresUnknownIncidentsMethodKeepsFailureGroups(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.failures":
			*result.(*jobsFailuresResult) = jobsFailuresResult{Failures: []job.FailureGroup{{
				State: job.StateFailed, Provider: "example.edu", Reason: "timeout", Count: 1, Sample: "job_1",
			}}}
		case "jobs.incidents":
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		default:
			t.Fatalf("method = %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "failures"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs failures: %v", err)
	}
	const want = `{"failures":[{"state":"failed","provider":"example.edu","reason":"timeout","count":1,"sample":"job_1"}],"incidents":[],"truncated":false}` + "\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

// TestJobsIncidentsRefusesDaemonWithoutIncidentModel: an older daemon never
// computed incidents, so an empty page with exit 0 would read as "no incident
// happened". The command must fail naming the missing method, print nothing
// on stdout, and never fall back to the legacy failure groups.
func TestJobsIncidentsRefusesDaemonWithoutIncidentModel(t *testing.T) {
	for _, args := range [][]string{{"jobs", "incidents"}, {"--json", "jobs", "incidents"}} {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
			if method != "jobs.incidents" {
				t.Fatalf("method = %q, want only jobs.incidents", method)
			}
			return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
		})
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), "jobs.incidents") || !strings.Contains(err.Error(), "daemon") {
			t.Fatalf("%v: error = %v, want an upgrade refusal naming jobs.incidents", args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%v: stdout = %q, want nothing", args, out.String())
		}
	}
}
