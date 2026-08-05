// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"strings"
	"testing"

	"papio/internal/batch"
	"papio/internal/protocol"
)

func TestPrintBatchReportShowsImportFailureClassAndHint(t *testing.T) {
	var out bytes.Buffer
	report := &batch.Report{
		BatchID: "batch-deadbeef",
		Summary: batch.ReportSummary{Total: 1, Outcomes: map[string]int{"import_failed": 1}},
		Works: []batch.ReportWork{{
			Outcome: "import_failed", JobID: "e484422626", Work: protocol.WorkRequest{Title: "Failed import"},
			ErrorClass: "zotero_field_validation", ErrorHint: "unknown item field",
		}},
	}
	if err := printBatchReport(&options{out: &out}, report); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"import_failed", "zotero_field_validation", "unknown item field"} {
		if !strings.Contains(got, want) {
			t.Fatalf("printBatchReport() missing %q:\n%s", want, got)
		}
	}
}

// reportWorkDescription's Title/identifier fallbacks are third-party
// bibliographic metadata a batch manifest can copy verbatim from a
// discovery search result. printBatchReport is the text-mode branch (the
// --json branch marshals *batch.Report directly and never calls this
// helper), so before this fix a poisoned title reached the terminal raw.
func TestPrintBatchReportStripsTerminalControlBytes(t *testing.T) {
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
			var out bytes.Buffer
			report := &batch.Report{
				BatchID: "batch-deadbeef",
				Summary: batch.ReportSummary{Total: 1, Outcomes: map[string]int{"imported": 1}},
				Works: []batch.ReportWork{{
					Outcome: "imported", JobID: "job-1", Work: protocol.WorkRequest{Title: tc.title},
				}},
			}
			if err := printBatchReport(&options{out: &out}, report); err != nil {
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
