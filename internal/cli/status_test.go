// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/job"
	"papio/internal/work"
)

func TestBuildStatusSnapshotGroupsRecentJobsAndDetails(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	rows := []job.Row{
		{ID: "working", State: job.StateFetching, UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: strings.Repeat("A", 60)}},
		{ID: "human", State: job.StateAwaitingHuman, UpdatedAt: now.Add(-5 * time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Needs a browser"}},
		{ID: "review", State: job.StateNeedsReview, UpdatedAt: now.Add(-90 * time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Needs review"}},
		{ID: "ready", State: job.StateReady, UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Imported paper"}},
		{ID: "failed", State: job.StateFailed, UpdatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Failed paper"}},
		{ID: "old-ready", State: job.StateReady, UpdatedAt: now.Add(-25 * time.Hour).Format(time.RFC3339Nano), Work: work.Work{Title: "Old paper"}},
	}
	details := map[string]api.JobDetail{
		"working": {Events: []map[string]any{{"detail": map[string]any{"source": "openalex"}}}},
		"human":   {Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{"to": job.StateAwaitingHuman, "reason": "institutional_handoff", "source": "library"}}}},
		"review":  {Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{"to": job.StateNeedsReview, "reason": "semantic_or_identity_review"}}}},
		"ready":   {Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{"source": "arxiv"}}, {"kind": "zotio.auto_import", "detail": map[string]any{"status": "applied"}}}},
	}

	snapshot := buildStatusSnapshot(rows, details, now)
	if len(snapshot.Groups) != 5 {
		t.Fatalf("groups = %#v", snapshot.Groups)
	}
	if snapshot.Groups[0].Phase != "working" || snapshot.Groups[0].Jobs[0].Provider != "openalex" || snapshot.Groups[0].Jobs[0].Age != "2h" {
		t.Fatalf("working group = %#v", snapshot.Groups[0])
	}
	if got := []rune(snapshot.Groups[0].Jobs[0].Title); len(got) != 50 || got[49] != '…' {
		t.Fatalf("working title = %q", snapshot.Groups[0].Jobs[0].Title)
	}
	if got := snapshot.Groups[1].Jobs[0].Reason; got != "institutional_handoff" {
		t.Fatalf("human reason = %q", got)
	}
	if got := snapshot.Groups[2].Jobs[0].Reason; got != "semantic_or_identity_review" {
		t.Fatalf("review reason = %q", got)
	}
	if got := snapshot.Groups[3].Jobs[0].ImportStatus; got != "applied" {
		t.Fatalf("ready import status = %q", got)
	}
	for _, group := range snapshot.Groups {
		for _, row := range group.Jobs {
			if row.ID == "old-ready" {
				t.Fatal("old ready job appeared in status")
			}
		}
	}
}

func TestRenderStatusRefreshPlainFollowRepaintsWithoutANSI(t *testing.T) {
	snapshot := statusSnapshot{
		GeneratedAt: "2026-07-15T12:00:00Z",
		Groups: []statusGroup{{Phase: "working", Jobs: []statusJob{{
			Title: "A paper", Provider: "arxiv", State: job.StateFetching, Age: "2m",
		}}}},
	}
	var out bytes.Buffer
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain follow output contained ANSI clear: %q", got)
	}
	if strings.Count(got, "papio status") != 2 || !strings.Contains(got, "A paper") {
		t.Fatalf("plain follow output = %q", got)
	}
}
