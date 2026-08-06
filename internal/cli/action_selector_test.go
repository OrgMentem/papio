// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
)

// handoffQueue is three openable handoffs on three parked jobs, newest first —
// the shape `actions open` sees, and the shape that made head-of-queue-only
// opening unusable for a consumer that ranked the queue itself.
func handoffQueue() ([]job.HumanAction, []job.Row, []string) {
	urls := []string{
		"https://oa.example.test/first.pdf",
		"https://oa.example.test/second.pdf",
		"https://oa.example.test/third.pdf",
	}
	actions := []job.HumanAction{
		{ID: 3, JobID: "job_c", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(urls[0])},
		{ID: 2, JobID: "job_b", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(urls[1])},
		{ID: 1, JobID: "job_a", Kind: "openurl_handoff", Status: "open", Detail: app.OABrowserHandoffActionDetail(urls[2])},
	}
	rows := []job.Row{
		{ID: "job_c", State: job.StateAwaitingHuman},
		{ID: "job_b", State: job.StateAwaitingHuman},
		{ID: "job_a", State: job.StateAwaitingHuman},
	}
	return actions, rows, urls
}

func handoffRoot(t *testing.T, out, errOut *bytes.Buffer, opened *[]string) *cobra.Command {
	t.Helper()
	actions, rows, _ := handoffQueue()
	return NewInProcessRoot(out, errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		switch method {
		case "actions.list":
			*result.(*[]job.HumanAction) = actions
		case "jobs.list_v2":
			*result.(*api.JobsPage) = api.JobsPage{Jobs: rows}
		case "actions.open":
			if opened != nil {
				*opened = append(*opened, params.(map[string]any)["job_ids"].([]string)...)
			}
			*result.(*api.ActionsOpenResult) = api.ActionsOpenResult{Queued: 1, SessionLive: true}
		default:
			t.Fatalf("unexpected method %q", method)
		}
		return nil
	})
}

func dryRunURLs(t *testing.T, out *bytes.Buffer) []string {
	t.Helper()
	var page struct {
		URLs      []string `json:"urls"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode urls page: %v (%q)", err, out.String())
	}
	return page.URLs
}

// TestActionsOpenSelectorOpensOnlyTheNamedJob is the whole point of the
// selector: a consumer that ranked the queue by its own criteria must be able to
// open the row it chose, not the head of the queue.
func TestActionsOpenSelectorOpensOnlyTheNamedJob(t *testing.T) {
	_, _, urls := handoffQueue()
	var out, errOut bytes.Buffer
	root := handoffRoot(t, &out, &errOut, nil)
	root.SetArgs([]string{"--json", "actions", "open", "--dry-run", "--job", "job_b"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open --job job_b: %v (%s)", err, errOut.String())
	}
	got := dryRunURLs(t, &out)
	if len(got) != 1 || got[0] != urls[1] {
		t.Fatalf("selected URLs = %v, want exactly job_b's %q — head-of-queue leakage", got, urls[1])
	}
}

func TestActionsOpenSelectorOpensOnlyTheNamedAction(t *testing.T) {
	_, _, urls := handoffQueue()
	var out, errOut bytes.Buffer
	root := handoffRoot(t, &out, &errOut, nil)
	root.SetArgs([]string{"--json", "actions", "open", "--dry-run", "--action", "1"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open --action 1: %v (%s)", err, errOut.String())
	}
	got := dryRunURLs(t, &out)
	if len(got) != 1 || got[0] != urls[2] {
		t.Fatalf("selected URLs = %v, want exactly action 1's %q", got, urls[2])
	}
}

// TestActionsOpenSelectorFocusesOnlyTheNamedJob covers the non-dry-run path: the
// daemon must be asked to focus one job, not the queue. A selector that filtered
// only the printed URLs while still handing every job id to actions.open would
// pass the dry-run tests above and still open three tabs.
func TestActionsOpenSelectorFocusesOnlyTheNamedJob(t *testing.T) {
	var out, errOut bytes.Buffer
	var opened []string
	root := handoffRoot(t, &out, &errOut, &opened)
	root.SetArgs([]string{"actions", "open", "--job", "job_b"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open --job job_b: %v (%s)", err, errOut.String())
	}
	if len(opened) != 1 || opened[0] != "job_b" {
		t.Fatalf("focused job ids = %v, want only job_b", opened)
	}
}

// TestActionsOpenUnknownSelectorIsAnError is the acceptance criterion that
// matters most: an unknown id must NOT fall back to the head of the queue. The
// old behaviour would have opened job_c here and reported success.
func TestActionsOpenUnknownSelectorIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown job", []string{"--job", "job_missing"}, `no open human action for job "job_missing"`},
		{"unknown action", []string{"--action", "999"}, "no open human action with id 999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := handoffRoot(t, &out, &errOut, nil)
			root.SetArgs(append([]string{"--json", "actions", "open", "--dry-run"}, tc.args...))
			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("unknown selector succeeded and emitted %q; a clean error is required, never a head-of-queue fallback", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name the missing row (%q)", err, tc.want)
			}
			if strings.Contains(out.String(), "oa.example.test") {
				t.Fatalf("unknown selector still emitted handoff URLs: %q", out.String())
			}
		})
	}
}

func TestActionsOpenRefusesBothSelectors(t *testing.T) {
	var out, errOut bytes.Buffer
	root := handoffRoot(t, &out, &errOut, nil)
	root.SetArgs([]string{"actions", "open", "--job", "job_b", "--action", "2"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "either --job or --action") {
		t.Fatalf("combining selectors = %v, want a refusal naming both flags", err)
	}
}

// TestActionsOpenWithoutSelectorOpensTheQueue pins that the selector is opt-in:
// the default behaviour is unchanged.
func TestActionsOpenWithoutSelectorOpensTheQueue(t *testing.T) {
	_, _, urls := handoffQueue()
	var out, errOut bytes.Buffer
	root := handoffRoot(t, &out, &errOut, nil)
	root.SetArgs([]string{"--json", "actions", "open", "--dry-run"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("actions open: %v (%s)", err, errOut.String())
	}
	if got := dryRunURLs(t, &out); len(got) != len(urls) {
		t.Fatalf("unselected URLs = %v, want the whole queue %v", got, urls)
	}
}
