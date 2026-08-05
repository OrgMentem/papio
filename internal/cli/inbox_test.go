// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
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
			if got != tc.wantRow {
				t.Fatalf("stdout = %q, want %q", got, tc.wantRow)
			}
			for _, r := range got {
				if r == '\n' || r == '\t' {
					continue
				}
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
		})
	}
}
