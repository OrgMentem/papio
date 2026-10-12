// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/triage"
)

func TestInboxJSONEmitsSnapshotEnvelopeVerbatim(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	want := triage.Snapshot{
		Schema: triage.SchemaVersion, GeneratedAt: now.Format(time.RFC3339),
		Counts: triage.Counts{PendingTotal: 1, WatchHits: 1},
		Items: []triage.Item{{
			Kind: triage.KindWatchHit, ID: "hit:1:10.1000/example", Rank: 2_000_000, Title: "Example",
			Facts: []triage.Fact{}, Links: []triage.Link{{Rel: "doi", URL: "https://doi.org/10.1000/example"}}, Ops: []string{"acquire", "dismiss"},
			WatchHit: &triage.WatchHit{
				Work: triage.Work{DOI: "10.1000/example", Title: "Example"}, Abstract: "Context",
				Watches: []triage.Watch{{ID: 1, Label: "Reading"}}, FirstSeenAt: now.Format(time.RFC3339),
			},
		}},
		HasMore: false,
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "triage.snapshot" {
			t.Fatalf("RPC method = %q", method)
		}
		*result.(*triage.Snapshot) = want
		return nil
	})
	root.SetArgs([]string{"inbox", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(out.Bytes(), &gotValue); err != nil {
		t.Fatalf("inbox JSON = %q, %v", out.String(), err)
	}
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("inbox JSON = %s, want %s", out.String(), wantJSON)
	}
	inbox, _, err := root.Find([]string{"inbox"})
	if err != nil || inbox.Annotations["mcp:read-only"] != "true" {
		t.Fatalf("inbox annotations = %#v, %v", inbox.Annotations, err)
	}
}

// item.Title is third-party bibliographic metadata for a watch-hit row: it is
// hit.Work.Title from a Crossref/OpenAlex/RSS watch match (internal/triage/
// triage.go's bounded(), which truncates but never strips control bytes).
// Before this fix, printInboxItem wrote it straight to the terminal on the
// text-mode `papio inbox` row, reopening the same escape-injection hole
// store.StripTerminalControls closes for `papio activity` and
// `papio watch digest`.
func assertInboxSanitizedRow(t *testing.T, snapshot triage.Snapshot, wantRow string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "triage.snapshot" {
			t.Fatalf("method = %q, want triage.snapshot", method)
		}
		*result.(*triage.Snapshot) = snapshot
		return nil
	})
	root.SetArgs([]string{"inbox"})
	if err := root.Execute(); err != nil {
		t.Fatalf("inbox: %v (%s)", err, stderr.String())
	}
	got := stdout.String()
	if got != wantRow {
		t.Fatalf("stdout = %q, want %q", got, wantRow)
	}
	for _, r := range got {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Errorf("control byte %#U survived in %q", r, got)
		}
	}
}

func TestInboxWatchHitRowStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		title   string
		wantRow string
	}{
		{
			name:    "escape and osc sequence in title",
			title:   "Evil\x1b]0;pwned\x07 Title\u009b31m",
			wantRow: "hit:1:10.1000/example\twatch hit\tEvil]0;pwned Title31m\t[Reading]\n",
		},
		{
			name:    "printable non-ASCII survives byte-for-byte",
			title:   "Café Über 日本語のタイトル",
			wantRow: "hit:1:10.1000/example\twatch hit\tCafé Über 日本語のタイトル\t[Reading]\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			snapshot := triage.Snapshot{
				Schema: triage.SchemaVersion, GeneratedAt: now.Format(time.RFC3339),
				Counts: triage.Counts{PendingTotal: 1, WatchHits: 1},
				Items: []triage.Item{{
					Kind: triage.KindWatchHit, ID: "hit:1:10.1000/example", Rank: 2_000_000, Title: tc.title,
					Facts: []triage.Fact{}, Links: []triage.Link{{Rel: "doi", URL: "https://doi.org/10.1000/example"}}, Ops: []string{"acquire", "dismiss"},
					WatchHit: &triage.WatchHit{
						Work: triage.Work{DOI: "10.1000/example", Title: tc.title}, Abstract: "Context",
						Watches: []triage.Watch{{ID: 1, Label: "Reading"}}, FirstSeenAt: now.Format(time.RFC3339),
					},
				}},
			}
			assertInboxSanitizedRow(t, snapshot, tc.wantRow)
		})
	}
}

// item.Retraction.DOI is third-party bibliographic metadata from a
// Retraction Watch feed match (internal/retraction), normalized only by
// work.NormalizeDOI — which does NOT strip control bytes: doiCoreRE's \S
// excludes only [\t\n\f\r ] in RE2, so ESC, BEL, DEL, and the whole C1 block
// all match \S and survive normalization intact. Before this fix,
// printInboxItem's KindRetraction arm wrote the DOI straight to the
// terminal, four lines below the KindWatchHit arm that was already fixed —
// reopening the same escape-injection hole on a sibling row.
func TestInboxRetractionRowStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		doi     string
		wantRow string
	}{
		{
			name:    "escape and osc sequence in doi",
			doi:     "10.1000/evil\x1b]0;pwned\x07DOI\u009b31m",
			wantRow: "retraction:1:10.1000/evil]0;pwnedDOI31m\tretraction\t10.1000/evil]0;pwnedDOI31m\n",
		},
		{
			name:    "printable non-ASCII survives byte-for-byte",
			doi:     "10.1000/日本語のDOI",
			wantRow: "retraction:1:10.1000/日本語のDOI\tretraction\t10.1000/日本語のDOI\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			snapshot := triage.Snapshot{
				Schema: triage.SchemaVersion, GeneratedAt: now.Format(time.RFC3339),
				Counts: triage.Counts{PendingTotal: 1},
				Items: []triage.Item{{
					Kind: triage.KindRetraction, ID: "retraction:1:" + tc.doi, Rank: 3_000_000,
					Title: "Retracted work", Facts: []triage.Fact{}, Ops: []string{"dismiss"},
					Retraction: &triage.Retraction{DOI: tc.doi, Nature: "retraction", NoticedAt: now},
				}},
			}
			assertInboxSanitizedRow(t, snapshot, tc.wantRow)
		})
	}
}

// The id in the first column is the exact argument `papio inbox decide`
// takes. Rank, grab ids without their prefix, and a missing id left the
// operator re-running with --json to find it.
func TestInboxTextRowsLeadWithTheItemIDDecideAccepts(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	snapshot := triage.Snapshot{
		Schema: triage.SchemaVersion, GeneratedAt: now.Format(time.RFC3339),
		Counts: triage.Counts{PendingTotal: 4},
		Items: []triage.Item{
			{Kind: triage.KindHumanAction, ID: "action:12", Rank: 1_000_001, Title: "Handoff", Facts: []triage.Fact{}, Ops: []string{"dismiss"},
				HumanAction: &triage.HumanAction{ActionID: 12, JobID: "job_abc", ActionKind: "manual_download"}},
			{Kind: triage.KindWatchHit, ID: "hit:3:10.1000/x", Rank: 2_000_001, Title: "A hit", Facts: []triage.Fact{}, Ops: []string{"acquire", "dismiss"},
				WatchHit: &triage.WatchHit{Work: triage.Work{DOI: "10.1000/x", Title: "A hit"}, Watches: []triage.Watch{{ID: 3, Label: "Reading"}}, FirstSeenAt: now.Format(time.RFC3339)}},
			{Kind: triage.KindRetraction, ID: "retraction:10.1000/r", Rank: 3_000_001, Title: "Retracted", Facts: []triage.Fact{}, Ops: []string{"dismiss"},
				Retraction: &triage.Retraction{DOI: "10.1000/r", Nature: "retraction", NoticedAt: now}},
			{Kind: triage.KindPdfGrab, ID: triage.PdfGrabIDPrefix + "grab-9", Rank: 4_000_001, Title: "scan.pdf", Facts: []triage.Fact{}, Ops: []string{"dismiss"},
				PdfGrab: &triage.PdfGrab{GrabID: "grab-9", State: "parked_no_identifier"}},
		},
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*triage.Snapshot) = snapshot
		return nil
	})
	root.SetArgs([]string{"inbox"})
	if err := root.Execute(); err != nil {
		t.Fatalf("inbox: %v (%s)", err, errOut.String())
	}
	rows := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(rows) != len(snapshot.Items) {
		t.Fatalf("rows = %q, want one per item", rows)
	}
	for i, item := range snapshot.Items {
		if first, _, _ := strings.Cut(rows[i], "\t"); first != item.ID {
			t.Errorf("row %d = %q: first column %q, want the item id %q", i, rows[i], first, item.ID)
		}
	}
}

// A page the daemon truncated must end with a command the operator can run:
// the cursor is accepted back by --cursor, and the next page starts where this
// one stopped.
func TestInboxCursorFlagPagesAndNoticeNamesTheNextCommand(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	row := func(id string) triage.Item {
		return triage.Item{Kind: triage.KindRetraction, ID: id, Rank: 3_000_001, Title: "R", Facts: []triage.Fact{}, Ops: []string{"dismiss"},
			Retraction: &triage.Retraction{DOI: "10.1000/r", Nature: "retraction", NoticedAt: now}}
	}
	var requests []triage.SnapshotRequest
	stub := func(_ context.Context, method string, params, result any) error {
		request := params.(triage.SnapshotRequest)
		requests = append(requests, request)
		page := triage.Snapshot{Schema: triage.SchemaVersion, GeneratedAt: now.Format(time.RFC3339)}
		if request.Cursor == "" {
			page.Items, page.Cursor, page.HasMore = []triage.Item{row("retraction:first")}, "tok_page2", true
		} else {
			page.Items = []triage.Item{row("retraction:second")}
		}
		*result.(*triage.Snapshot) = page
		return nil
	}

	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"inbox", "--limit", "1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("first page: %v (%s)", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "retraction:first\t") || !strings.HasSuffix(out.String(), "papio inbox --cursor tok_page2 --limit 1\n") {
		t.Fatalf("first page = %q, want its row and a runnable next-page command", out.String())
	}
	if strings.Contains(out.String(), "--json") {
		t.Fatalf("first page = %q: must not send the operator to --json for a cursor", out.String())
	}

	out.Reset()
	root = NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"inbox", "--cursor", "tok_page2", "--limit", "1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("second page: %v (%s)", err, errOut.String())
	}
	if want := (triage.SnapshotRequest{Limit: 1, Cursor: "tok_page2"}); requests[len(requests)-1] != want {
		t.Fatalf("second request = %+v, want %+v", requests[len(requests)-1], want)
	}
	if strings.Contains(out.String(), "retraction:first") || !strings.HasPrefix(out.String(), "retraction:second\t") || strings.Contains(out.String(), "--cursor") {
		t.Fatalf("second page = %q, want only the next row and no further notice", out.String())
	}
}

func TestInboxDecideDismissOfAPdfGrabRequiresDeleteGrab(t *testing.T) {
	var calls []map[string]any
	stub := func(_ context.Context, method string, params, result any) error {
		if method != "triage.decide" {
			t.Fatalf("method = %q, want triage.decide", method)
		}
		calls = append(calls, params.(map[string]any))
		*result.(*triageDecision) = triageDecision{Outcome: string(triage.DecisionApplied)}
		return nil
	}
	item := triage.PdfGrabIDPrefix + "grab-1"

	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"inbox", "decide", item, "--op", "dismiss"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "--delete-grab") {
		t.Fatalf("dismiss of a pdf grab = %v, want a refusal naming --delete-grab", err)
	}
	if len(calls) != 0 {
		t.Fatalf("unconfirmed grab dismissal reached the daemon: %v", calls)
	}

	root = NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"inbox", "decide", item, "--op", "dismiss", "--delete-grab"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("confirmed grab dismissal: %v", err)
	}
	if len(calls) != 1 || calls[0]["item_id"] != item || calls[0]["op"] != "dismiss" {
		t.Fatalf("confirmed grab dismissal params = %v", calls)
	}

	// A watch hit is only a notification: dismissing it needs no confirmation.
	root = NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"inbox", "decide", "hit:1:10.1000/example", "--op", "dismiss"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("watch-hit dismissal: %v", err)
	}
}

func TestInboxDecideExitStatusFollowsTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome triage.DecisionOutcome
		wantErr bool
	}{
		{outcome: triage.DecisionApplied},
		{outcome: triage.DecisionAlreadyApplied},
		{outcome: triage.DecisionConflict, wantErr: true},
		{outcome: triage.DecisionInvalid, wantErr: true},
	} {
		for _, jsonOutput := range []bool{false, true} {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
				*result.(*triageDecision) = triageDecision{Outcome: string(tc.outcome), Detail: "why"}
				return nil
			})
			args := []string{"inbox", "decide", "hit:1:10.1000/example", "--op", "acquire"}
			if jsonOutput {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			err := root.ExecuteContext(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("outcome %s (json=%v): err = %v, want error %v", tc.outcome, jsonOutput, err, tc.wantErr)
			}
			// The outcome is printed either way, so a script can still read why.
			if !strings.Contains(out.String(), string(tc.outcome)) {
				t.Fatalf("outcome %s (json=%v): stdout = %q, want the outcome printed", tc.outcome, jsonOutput, out.String())
			}
		}
	}
}
