// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

func TestJobsFailuresOmitsIncidentsForOlderDaemon(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.failures":
			*result.(*jobsFailuresResult) = jobsFailuresResult{Failures: []job.FailureGroup{{State: job.StateFailed, Provider: "example.edu", Reason: "timeout", Count: 1, Sample: "job_1"}}}
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
	const want = `{"failures":[{"state":"failed","provider":"example.edu","reason":"timeout","count":1,"sample":"job_1"}],"truncated":false}
`
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}
