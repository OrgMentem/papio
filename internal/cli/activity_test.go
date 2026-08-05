// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"

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
			name:  "c1 csi in title",
			entry: store.ActivityEntry{JobTitle: "Title\u009b31mRed"},
			want:  "Title31mRed",
		},
		{
			name:  "falls back to state when title empty after stripping",
			entry: store.ActivityEntry{JobTitle: "\x1b", JobState: "resolving"},
			want:  "resolving",
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
