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

func TestJobsUnknownIncidentsMethodKeepsStableEmptySurface(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		// servesFailures says whether jobs.failures is a legal call for this
		// argv; "jobs incidents" must never reach for the failures method.
		servesFailures bool
		want           string
	}{
		{name: "jobs failures keeps an empty incidents list beside its groups", args: []string{"--json", "jobs", "failures"}, servesFailures: true, want: `{"failures":[{"state":"failed","provider":"example.edu","reason":"timeout","count":1,"sample":"job_1"}],"incidents":[],"truncated":false}` + "\n"},
		{name: "jobs incidents keeps its own separate empty surface", args: []string{"--json", "jobs", "incidents"}, want: `{"incidents":[],"truncated":false}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				switch {
				case method == "jobs.failures" && test.servesFailures:
					*result.(*jobsFailuresResult) = jobsFailuresResult{Failures: []job.FailureGroup{{
						State: job.StateFailed, Provider: "example.edu", Reason: "timeout", Count: 1, Sample: "job_1",
					}}}
				case method == "jobs.incidents":
					return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
				default:
					t.Fatalf("method = %q", method)
				}
				return nil
			})
			root.SetArgs(test.args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%v: %v", test.args, err)
			}
			if out.String() != test.want {
				t.Fatalf("output = %q, want %q", out.String(), test.want)
			}
		})
	}
}
