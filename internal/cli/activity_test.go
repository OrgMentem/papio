// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/store"
)

// entry.JobTitle is third-party bibliographic metadata: enrichment stores an
// external DOI record's title after only strings.TrimSpace, so whoever
// registers the DOI controls it. Before this fix, compactActivitySummary fed
// it straight through strings.Fields (which does not treat ESC/BEL/C1 as
// whitespace) onto the same terminal row store.ActivityText already
// sanitized, reopening the escape-injection hole this package's other column
// had just closed.
func TestCompactActivitySummaryStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry store.ActivityEntry
		want  string
	}{
		{
			name:  "escape sequence in title",
			entry: store.ActivityEntry{JobTitle: "Evil\x1b]0;pwned\x07 Title"},
			want:  "Evil]0;pwned Title",
		},
		{
			name:  "falls back to state when title empty after stripping",
			entry: store.ActivityEntry{JobTitle: "\x1b", JobState: "resolving"},
			want:  "resolving",
		},
		{
			name:  "control bytes in state fallback also stripped",
			entry: store.ActivityEntry{JobState: "resolv\x1b]0;pwned\x07ing\u009b31m"},
			want:  "resolv]0;pwneding31m",
		},
		{
			name:  "printable non-ASCII title preserved",
			entry: store.ActivityEntry{JobTitle: "Café Über Nïño"},
			want:  "Café Über Nïño",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := compactActivitySummary(tc.entry)
			if got != tc.want {
				t.Errorf("compactActivitySummary(%+v) = %q, want %q", tc.entry, got, tc.want)
			}
			for _, r := range got {
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
			if strings.ContainsAny(got, "\x1b\x07") {
				t.Errorf("ESC/BEL survived in %q", got)
			}
		})
	}
}

// TestActivityPagesPastTheNewestEvents: --limit stops at 200, so the cursor
// is the only way to reach older events. The flag must reach the daemon, and
// a truncated text page must name the exact invocation for the next page.
func TestActivityPagesPastTheNewestEvents(t *testing.T) {
	at := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	var gotParams map[string]any
	newRoot := func(out, errOut *bytes.Buffer) *cobra.Command {
		return NewInProcessRoot(out, errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
			if method != "activity.list" {
				t.Fatalf("method = %q", method)
			}
			gotParams = params.(map[string]any)
			*result.(*api.ActivityPage) = api.ActivityPage{Entries: []store.ActivityEntry{
				{Seq: 41, At: at, JobID: "job_older", Kind: "job.state"},
				{Seq: 40, At: at, JobID: "job_older", Kind: "job.state"},
			}, Truncated: true}
			return nil
		})
	}

	var out, errOut bytes.Buffer
	root := newRoot(&out, &errOut)
	root.SetArgs([]string{"activity", "--limit", "2", "--before-seq", "42", "--job", "job_older"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("activity: %v (%s)", err, errOut.String())
	}
	if gotParams["before_seq"] != int64(42) || gotParams["job_id"] != "job_older" {
		t.Fatalf("params = %#v, want before_seq 42 for job_older", gotParams)
	}
	if want := "truncated: showing 2 entries; older: papio activity --before-seq 40 --job job_older\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("output = %q, want it to end with %q", out.String(), want)
	}

	var jsonOut, jsonErr bytes.Buffer
	root = newRoot(&jsonOut, &jsonErr)
	root.SetArgs([]string{"--json", "activity", "--limit", "2"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("activity --json: %v (%s)", err, jsonErr.String())
	}
	if strings.Contains(jsonOut.String(), "truncated: showing") {
		t.Fatalf("--json output carries the text notice: %q", jsonOut.String())
	}

	root = newRoot(&bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"activity", "--before-seq", "-1"})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("activity --before-seq -1: want an error")
	}
}
