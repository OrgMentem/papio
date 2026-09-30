// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

// TestStatusFindsAnActiveJobBehindAFullRecentPage: jobs.list orders by
// creation, so a full page of newer settled jobs used to push an older job
// that still waits on a human off the board, and status showed nothing. The
// active states are read on their own, and a state too large for one read
// is reported instead of silently cut.
func TestStatusFindsAnActiveJobBehindAFullRecentPage(t *testing.T) {
	now := time.Now().UTC()
	old := store.FormatTime(now.Add(-72 * time.Hour))
	recent := make([]job.Row, 0, job.ListLimitMax)
	for i := range job.ListLimitMax {
		recent = append(recent, job.Row{ID: fmt.Sprintf("job_settled_%03d", i), State: job.StateFailed, UpdatedAt: old})
	}
	parked := job.Row{ID: "job_parked", State: job.StateAwaitingHuman, UpdatedAt: old, Work: work.Work{Title: "A parked paper"}}
	newRoot := func(out, errOut *bytes.Buffer) func(args ...string) error {
		root := NewInProcessRoot(out, errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
			switch method {
			case "zotio.missing_count":
				return fmt.Errorf("zotio not configured")
			case "jobs.list_v2":
				state, _ := params.(map[string]any)["state"].(string)
				page := api.JobsPage{Jobs: []job.Row{}}
				switch state {
				case "":
					page = api.JobsPage{Jobs: recent, Truncated: true}
				case job.StateAwaitingHuman:
					page = api.JobsPage{Jobs: []job.Row{parked}}
				case job.StateRetryWait:
					page = api.JobsPage{Jobs: []job.Row{}, Truncated: true}
				}
				*result.(*api.JobsPage) = page
			case "jobs.list":
				// The pre-fix read: one newest-first page, no state filter.
				*result.(*[]job.Row) = recent
			case "jobs.get":
				id := params.(map[string]string)["job_id"]
				if id != parked.ID {
					t.Fatalf("jobs.get for %q, want only the parked job", id)
				}
				row := parked
				*result.(*api.JobDetail) = api.JobDetail{Job: &row}
			default:
				t.Fatalf("unexpected method %q", method)
			}
			return nil
		})
		return func(args ...string) error {
			root.SetArgs(args)
			return root.ExecuteContext(context.Background())
		}
	}

	var out, errOut bytes.Buffer
	if err := newRoot(&out, &errOut)("--json", "status"); err != nil {
		t.Fatalf("status --json: %v (%s)", err, errOut.String())
	}
	var snapshot statusSnapshot
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode: %v (%q)", err, out.String())
	}
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Jobs) != 1 || snapshot.Groups[0].Jobs[0].ID != parked.ID {
		t.Fatalf("groups = %+v, want only the parked job", snapshot.Groups)
	}
	if len(snapshot.IncompleteStates) != 1 || snapshot.IncompleteStates[0] != job.StateRetryWait {
		t.Fatalf("incomplete_states = %v, want [retry_wait]", snapshot.IncompleteStates)
	}

	var textOut, textErr bytes.Buffer
	if err := newRoot(&textOut, &textErr)("status"); err != nil {
		t.Fatalf("status: %v (%s)", err, textErr.String())
	}
	for _, want := range []string{"A parked paper", "truncated: more than 500 retry_wait jobs; showing the newest 500"} {
		if !strings.Contains(textOut.String(), want) {
			t.Fatalf("status output lacks %q:\n%s", want, textOut.String())
		}
	}
}
