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
			wantRow: "2000000\twatch hit\tEvil]0;pwned Title31m\t[Reading]\n",
		},
		{
			name:    "printable non-ASCII survives byte-for-byte",
			title:   "Café Über 日本語のタイトル",
			wantRow: "2000000\twatch hit\tCafé Über 日本語のタイトル\t[Reading]\n",
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
			wantRow: "3000000\tretraction\t10.1000/evil]0;pwnedDOI31m\n",
		},
		{
			name:    "printable non-ASCII survives byte-for-byte",
			doi:     "10.1000/日本語のDOI",
			wantRow: "3000000\tretraction\t10.1000/日本語のDOI\n",
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
