// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
)

// TestActionsOpenJSONKeepsSkippedHandoffNoticeOffStdout: when the live
// daemon skips a handoff, the shortfall line is prose. Under --json it must
// go to stderr so stdout stays exactly one JSON document a parser accepts.
func TestActionsOpenJSONKeepsSkippedHandoffNoticeOffStdout(t *testing.T) {
	const target = "https://oa.example.test/skipped.pdf"
	action := job.HumanAction{
		ID: 1, JobID: "job_skip_001", Kind: "openurl_handoff", Status: "open",
		Detail: app.OABrowserHandoffActionDetail(target),
	}
	row := job.Row{ID: action.JobID, State: job.StateAwaitingHuman}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "actions.list":
			*result.(*[]job.HumanAction) = []job.HumanAction{action}
		case "jobs.list_v2":
			*result.(*api.JobsPage) = api.JobsPage{Jobs: []job.Row{row}}
		case "actions.open":
			*result.(*api.ActionsOpenResult) = api.ActionsOpenResult{Queued: 0, SessionLive: true}
		default:
			t.Fatalf("unexpected method %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"--json", "actions", "open"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open: %v (%s)", err, errOut.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var page struct {
		URLs      []string `json:"urls"`
		Truncated bool     `json:"truncated"`
	}
	if err := decoder.Decode(&page); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out.String())
	}
	if decoder.More() {
		t.Fatalf("stdout carries more than one JSON document: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "1 of 1 handoffs were not opened") {
		t.Fatalf("stderr = %q, want the skipped-handoff notice", errOut.String())
	}
}
