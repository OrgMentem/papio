// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
)

func TestPublisherRetryCLIRequiresRevisionAndUsesDedicatedMethod(t *testing.T) {
	for _, revision := range []bool{false, true} {
		var out, errOut bytes.Buffer
		calls := 0
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
			calls++
			p := params.(map[string]any)
			if method != "actions.retry_publisher" || p["action_id"] != int64(42) || p["expected_revision"] != int64(3) {
				t.Fatalf("%s %v", method, params)
			}
			*result.(*api.SubmitResult) = api.SubmitResult{JobID: "job_publisher"}
			return nil
		})
		args := []string{"--json", "actions", "retry-publisher", "42"}
		if revision {
			args = append(args, "--revision", "3")
		}
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		if revision {
			if err != nil || calls != 1 || out.String() != "{\"job_id\":\"job_publisher\"}\n" {
				t.Fatalf("err=%v calls=%d out=%s", err, calls, out.String())
			}
		} else if err == nil || calls != 0 {
			t.Fatalf("missing revision err=%v calls=%d", err, calls)
		}
	}
}
