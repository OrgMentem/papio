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
