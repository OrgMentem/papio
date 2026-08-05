// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/errcat"
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

	snapshot := buildStatusSnapshot(rows, details, now, config.Config{})
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
	if got := snapshot.Groups[1].Jobs[0].Category; got != "login_required" {
		t.Fatalf("human category = %q", got)
	}
	if got := snapshot.Groups[2].Jobs[0].Category; got != "identity_review" || snapshot.Groups[2].Jobs[0].Guidance == "" {
		t.Fatalf("review category/guidance = %q / %q", got, snapshot.Groups[2].Jobs[0].Guidance)
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

func TestBuildStatusSnapshotUsesCurrentOpenActionGuidance(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	rows := []job.Row{{
		ID: "doi:10.1177/0018720814547570", State: job.StateAwaitingHuman,
		UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), Work: work.Work{Title: "Current action wins"},
	}}
	details := map[string]api.JobDetail{
		rows[0].ID: {
			Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{
				"to": job.StateAwaitingHuman, "reason": "login_required",
			}}},
			Actions: []job.HumanAction{{
				ID: 228, JobID: rows[0].ID, Kind: "manual_download", Status: "open", RequiresAuth: true, BlockedBy: "landing_page",
			}},
		},
	}

	snapshot := buildStatusSnapshot(rows, details, now, config.Config{AccessMode: config.ModeDelegated})
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Jobs) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	item := snapshot.Groups[0].Jobs[0]
	if item.Reason != "login_required" {
		t.Fatalf("reason = %q, want historical login_required", item.Reason)
	}
	if item.Category != "manual_download" {
		t.Fatalf("category = %q, want manual_download", item.Category)
	}
	for _, want := range []string{"Sign in at your institution", "download the PDF yourself"} {
		if !strings.Contains(item.Guidance, want) {
			t.Fatalf("guidance = %q, want %q", item.Guidance, want)
		}
	}
	if strings.Contains(item.Guidance, "papio actions open") {
		t.Fatalf("guidance names an inapplicable handoff command: %q", item.Guidance)
	}
}

func TestBuildStatusSnapshotKeepsTerminalGuidanceWithoutOpenAction(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	row := job.Row{
		ID: "terminal-no-identifier", State: job.StateUnavailable,
		UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), Policy: job.Policy{AccessMode: config.ModeDelegated},
		Work: work.Work{Title: "No identifier"},
	}
	detail := api.JobDetail{
		Events: []map[string]any{{"kind": "job.transition", "detail": map[string]any{
			"to": job.StateUnavailable, "reason": "no_identifier",
		}}},
		Actions: []job.HumanAction{{ID: 228, JobID: row.ID, Kind: "manual_download", Status: "resolved", RequiresAuth: true}},
	}
	cfg := config.Config{AccessMode: config.ModeDelegated}

	snapshot := buildStatusSnapshot([]job.Row{row}, map[string]api.JobDetail{row.ID: detail}, now, cfg)
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Jobs) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	got := snapshot.Groups[0].Jobs[0]
	want := errcat.Explain(row.State, "no_identifier", row.Policy.Resolver, row.Policy.AccessMode, cfg)
	if got.Category != want.Category || got.Guidance != want.Guidance {
		t.Fatalf("terminal explanation = %q / %q, want %q / %q", got.Category, got.Guidance, want.Category, want.Guidance)
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

func TestRenderStatusRefreshShowsCategoryAndGuidance(t *testing.T) {
	snapshot := statusSnapshot{
		GeneratedAt: "2026-07-18T00:00:00Z",
		Groups: []statusGroup{{Phase: "failed / unavailable", Jobs: []statusJob{{
			Title: "Some paper", Provider: "—", State: job.StateUnavailable, Age: "3h",
			Reason: "no_legal_candidates", Category: "institution_not_configured",
			Guidance: "No institution is configured, so institutional access was never attempted. Run `papio init`.",
		}}}},
	}
	var out bytes.Buffer
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "institution_not_configured") {
		t.Fatalf("category not shown in DETAIL: %q", got)
	}
	if !strings.Contains(got, "    → No institution is configured") {
		t.Fatalf("guidance sub-line not rendered: %q", got)
	}
}
func TestRenderStatusRefreshShowsLibraryCompleteness(t *testing.T) {
	missing := 87
	snapshot := statusSnapshot{GeneratedAt: "2026-07-22T00:00:00Z", LibraryMissingPDFs: &missing}
	var out bytes.Buffer
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Library: 87 item(s) missing PDFs — papio acquire --from-zotio queues them (25 per run by default)") {
		t.Fatalf("library line missing: %q", got)
	}

	complete := 0
	snapshot.LibraryMissingPDFs = &complete
	out.Reset()
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Library: complete — no items missing PDFs") {
		t.Fatalf("complete line missing: %q", got)
	}

	// Without zotio the line is absent entirely.
	snapshot.LibraryMissingPDFs = nil
	out.Reset()
	if err := renderStatusRefresh(&out, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "Library:") {
		t.Fatalf("library line rendered without zotio: %q", got)
	}
}

// statusJob.Title (shortTitle(row.Work.Describe())) backs both `papio
// status`'s text row and `papio status --json`. buildStatusSnapshot must
// keep the raw bytes intact here for the --json exact-byte contract; only
// the text-mode renderStatusRefresh strips them (see the comment above that
// call site in status.go).
func TestBuildStatusSnapshotPreservesControlBytesForJSON(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	rows := []job.Row{
		{ID: "poisoned", State: job.StateFetching, UpdatedAt: now.Format(time.RFC3339Nano), Work: work.Work{Title: "Evil\x1b[31mTitle"}},
	}
	snapshot := buildStatusSnapshot(rows, nil, now, config.Config{})
	if len(snapshot.Groups) != 1 || len(snapshot.Groups[0].Jobs) != 1 {
		t.Fatalf("groups = %#v", snapshot.Groups)
	}
	want := "title:Evil\x1b[31mTitle"
	if got := snapshot.Groups[0].Jobs[0].Title; got != want {
		t.Fatalf("statusJob.Title = %q, want raw %q (must survive verbatim for --json)", got, want)
	}
}

// item.Title is third-party bibliographic metadata (a discovery-registered
// title or DOI, via row.Work.Describe()); shortText's strings.Fields fold
// does not remove control bytes since unicode.IsSpace covers none of them.
// Before this fix, renderStatusRefresh printed it straight to the terminal.
func TestRenderStatusRefreshStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "escape and osc sequence in title",
			title: "Evil\x1b]0;pwned\x07 Title\u009b31m",
			want:  "Evil]0;pwned Title31m",
		},
		{
			name:  "printable non-ASCII survives byte-for-byte",
			title: "Café Über 日本語のタイトル",
			want:  "Café Über 日本語のタイトル",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := statusSnapshot{
				GeneratedAt: "2026-07-15T12:00:00Z",
				Groups: []statusGroup{{Phase: "working", Jobs: []statusJob{{
					Title: tc.title, Provider: "arxiv", State: job.StateFetching, Age: "2m",
				}}}},
			}
			var out bytes.Buffer
			if err := renderStatusRefresh(&out, snapshot, false); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("output = %q, want it to contain %q", got, tc.want)
			}
			if tc.title != tc.want && strings.Contains(got, tc.title) {
				t.Fatalf("raw unstripped title leaked into output: %q", got)
			}
			for _, r := range got {
				if r == '\n' {
					continue
				}
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
		})
	}
}
