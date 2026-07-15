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
