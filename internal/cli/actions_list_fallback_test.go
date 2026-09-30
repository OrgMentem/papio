// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

// TestActionsListV1FallbackHonoursLimit: against a daemon that only has the
// unbounded actions.list, --limit must still bound the page and the envelope
// must say rows were dropped, the same contract jobs list keeps on its v1
// fallback. Returning every open action with truncated=false reads as a
// complete, under-limit answer.
func TestActionsListV1FallbackHonoursLimit(t *testing.T) {
	actions := []job.HumanAction{
		{ID: 3, JobID: "job_c", Kind: "openurl_handoff", Status: "open"},
		{ID: 2, JobID: "job_b", Kind: "openurl_handoff", Status: "open"},
		{ID: 1, JobID: "job_a", Kind: "openurl_handoff", Status: "open"},
	}
	newRoot := func(out, errOut *bytes.Buffer) *cobra.Command {
		return NewInProcessRoot(out, errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
			switch method {
			case "actions.list_v3", "actions.list_v2":
				return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
			case "actions.list":
				*result.(*[]job.HumanAction) = actions
				return nil
			}
			t.Fatalf("unexpected method %q", method)
			return nil
		})
	}

	var out, errOut bytes.Buffer
	root := newRoot(&out, &errOut)
	root.SetArgs([]string{"--json", "actions", "list", "--limit", "2"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions list --json: %v (%s)", err, errOut.String())
	}
	var page struct {
		Actions []struct {
			ID int64 `json:"id"`
		} `json:"actions"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%q)", err, out.String())
	}
	if len(page.Actions) != 2 || page.Actions[0].ID != 3 || page.Actions[1].ID != 2 {
		t.Fatalf("actions = %+v, want the first two rows", page.Actions)
	}
	if !page.Truncated {
		t.Fatal("truncated = false with a row dropped by --limit")
	}

	var textOut, textErr bytes.Buffer
	root = newRoot(&textOut, &textErr)
	root.SetArgs([]string{"actions", "list", "--limit", "2"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions list: %v (%s)", err, textErr.String())
	}
	if strings.Contains(textOut.String(), "job_a") {
		t.Fatalf("text listing shows the row past --limit:\n%s", textOut.String())
	}
	if !strings.Contains(textOut.String(), "truncated: showing 2 open actions") {
		t.Fatalf("text listing has no truncation notice:\n%s", textOut.String())
	}
}
