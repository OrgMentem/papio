// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const cryptFilterReason = "pdfcpu inspection: Invalid filter: <Crypt>"

func TestCryptFilterFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reason      string
		jsOutput    string
		infoWarning bool
		attachment  string
		wantValid   bool
	}{
		{name: "identity filter without active content", attachment: "0 embedded files", wantValid: true},
		{name: "lowercase parser wording", reason: "pdfcpu inspection: info: prepare PDF context: invalid filter: <Crypt>", attachment: "0 embedded files", wantValid: true},
		{name: "pdfinfo warns about identity filter", infoWarning: true, attachment: "0 embedded files", wantValid: true},
		{name: "embedded file", attachment: "1 embedded files"},
		{name: "JavaScript", jsOutput: "name: script", attachment: "0 embedded files"},
		{name: "JavaScript output is not empty", jsOutput: " ", attachment: "0 embedded files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := tc.reason
			if reason == "" {
				reason = cryptFilterReason
			}
			worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"`+reason+`"}'`)
			warning := ""
			if tc.infoWarning {
				warning = `printf 'Syntax Error: identity stream\n' >&2;`
			}
			pdfinfo := fakeTool(t, `if [ "$1" = "-js" ]; then printf '%s' '`+tc.jsOutput+`'; else `+warning+` printf 'Pages: 10\nEncrypted: no\n'; fi`)
			pdfdetach := fakeTool(t, `printf '%s\n' '`+tc.attachment+`'`)
			report, err := ValidateStructural(t.Context(), worker, writeTempPDF(t), StructuralOptions{
				PDFInfoPath: pdfinfo, PDFDetachPath: pdfdetach,
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Valid != tc.wantValid {
				t.Fatalf("report=%+v, want valid=%v", report, tc.wantValid)
			}
			if tc.wantValid {
				if report.Pages != 10 || report.Encrypted || report.HasJavaScript || report.HasEmbeddedFiles || report.Reason != "" {
					t.Fatalf("fallback report=%+v", report)
				}
			} else if report.Reason != reason {
				t.Fatalf("report=%+v, want unchanged pdfcpu rejection", report)
			}
		})
	}
}

func TestCryptFilterFallbackDoesNotRunForOtherErrors(t *testing.T) {
	const reason = "pdfcpu inspection: malformed xref"
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"`+reason+`"}'`)
	marker := t.TempDir() + "/invoked"
	unwanted := fakeTool(t, `touch "`+marker+`"; exit 1`)
	report, err := ValidateStructural(t.Context(), worker, writeTempPDF(t), StructuralOptions{
		PDFInfoPath: unwanted, PDFDetachPath: unwanted,
	})
	if err != nil || report.Valid || report.Reason != reason {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unexpected fallback invocation: stat err=%v", err)
	}
}

func TestCryptFilterFallbackFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		info        string
		detach      string
		maxPages    int
		noPDFInfo   bool
		noPDFDetach bool
	}{
		{name: "encrypted", info: "Pages: 10\nEncrypted: yes\n", detach: "0 embedded files"},
		{name: "missing encryption evidence", info: "Pages: 10\n", detach: "0 embedded files"},
		{name: "page cap", info: "Pages: 10\nEncrypted: no\n", detach: "0 embedded files", maxPages: 9},
		{name: "missing pdfinfo", noPDFInfo: true},
		{name: "missing pdfdetach", info: "Pages: 10\nEncrypted: no\n", noPDFDetach: true},
		{name: "unrecognized attachment status", info: "Pages: 10\nEncrypted: no\n", detach: "nothing found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"`+cryptFilterReason+`"}'`)
			pdfinfo := fakeTool(t, `if [ "$1" = "-js" ]; then exit 0; fi; printf '`+tc.info+`'`)
			pdfdetach := fakeTool(t, `printf '%s\n' '`+tc.detach+`'`)
			if tc.noPDFInfo {
				pdfinfo = ""
			}
			if tc.noPDFDetach {
				pdfdetach = ""
			}
			report, err := ValidateStructural(t.Context(), worker, writeTempPDF(t), StructuralOptions{
				MaxPages: tc.maxPages, PDFInfoPath: pdfinfo, PDFDetachPath: pdfdetach,
			})
			if err != nil || report.Valid || report.Reason != cryptFilterReason {
				t.Fatalf("report=%+v err=%v, want unchanged pdfcpu rejection", report, err)
			}
		})
	}
}

func TestCryptFilterFallbackNeverSanitizes(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"`+cryptFilterReason+`"}'`)
	pdfinfo := fakeTool(t, `if [ "$1" = "-js" ]; then exit 0; fi; printf 'Pages: 10\nEncrypted: no\n'`)
	pdfdetach := fakeTool(t, `printf '0 embedded files\n'`)
	report, err := SanitizeEmbeddedFiles(t.Context(), worker, writeTempPDF(t), t.TempDir()+"/sanitized.pdf", StructuralOptions{
		PDFInfoPath: pdfinfo, PDFDetachPath: pdfdetach,
	})
	if err != nil || report.Valid || report.Reason != cryptFilterReason {
		t.Fatalf("report=%+v err=%v, sanitizer must not use the inspection fallback", report, err)
	}
}

func TestStructuralParentRejectsWorkerPageCapViolation(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":11}'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{MaxPages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid || !strings.Contains(report.Reason, "exceeds cap") {
		t.Fatalf("report=%+v", report)
	}
}

func TestCrossCheckPDFInfoAgrees(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	pdfinfo := fakeTool(t, `printf 'Creator: test\nPages: 2\n'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:    10,
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if !report.Valid {
		t.Fatalf("expected Valid cross-check to pass, got report=%+v", report)
	}
	if report.Pages != 2 {
		t.Fatalf("Pages=%d want 2 report=%+v", report.Pages, report)
	}
	if report.Reason != "" {
		t.Fatalf("Reason=%q want empty report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoDisagrees(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	pdfinfo := fakeTool(t, `printf 'Pages: 5\n'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:    10,
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected disagreement to invalidate report, got %+v", report)
	}
	if !strings.Contains(report.Reason, "pdfinfo page count disagrees with worker") {
		t.Fatalf("Reason=%q want pdfinfo page count disagrees with worker report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoExitNonZero(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":3}'`)
	pdfinfo := fakeTool(t, `printf 'pdfinfo failed: boom' >&2; exit 1`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:    10,
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected non-zero pdfinfo to invalidate report, got %+v", report)
	}
	if !strings.Contains(report.Reason, "pdfinfo cross-check failed") {
		t.Fatalf("Reason=%q want pdfinfo cross-check failed report=%+v", report.Reason, report)
	}
	if !strings.Contains(report.Reason, "boom") {
		t.Fatalf("Reason=%q should contain stderr boom report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoMissingPagesLine(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	pdfinfo := fakeTool(t, `printf 'Title: hello\nCreator: test\nProducer: x\n'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:    10,
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected missing Pages line to invalidate report, got %+v", report)
	}
	if !strings.Contains(report.Reason, "pdfinfo output did not contain page count") {
		t.Fatalf("Reason=%q want pdfinfo output did not contain page count report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoNonNumericPages(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	pdfinfo := fakeTool(t, `printf 'Pages: abc\n'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:    10,
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected non-numeric Pages to invalidate report, got %+v", report)
	}
	if !strings.Contains(report.Reason, "pdfinfo page count disagrees with worker") {
		t.Fatalf("Reason=%q want pdfinfo page count disagrees with worker report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoTimeout(t *testing.T) {
	// ValidateStructural shares a single deadline (workerCtx) between the worker
	// and pdfinfo, so a short Timeout would kill the worker before pdfinfo ever
	// runs (worker cold-start is ~0.4-1s on this platform). To isolate the
	// pdfinfo timeout branch without flakiness or multi-second wall time,
	// exercise crossCheckPDFInfo directly with a tight context — sleep is
	// 500ms but the context cancels after ~120ms, so wall time stays well under
	// 300ms (measured ~130ms). A pdfinfo timeout via the end-to-end
	// ValidateStructural path would require a deadline long enough for the worker
	// cold-start plus the pdfinfo sleep — covered reliably here; adding it as
	// an end-to-end test would add multi-second wall time, so it is skipped by
	// design (see assignment rule: if timeout is hardcoded >200ms, skip rather
	// than slow the suite).
	pdfinfo := fakeTool(t, `sleep 0.5; printf 'Pages: 1\n'`)
	report := StructuralReport{Valid: true, Pages: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := crossCheckPDFInfo(ctx, pdfinfo, writeTempPDF(t), &report, 64<<10)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "pdfinfo timed out") {
		t.Fatalf("err=%v want pdfinfo timed out", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("timeout test took %v, exceeds 300ms budget (sleep should be interrupted by context)", elapsed)
	}
}

func TestCrossCheckPDFInfoOutputExceedsCap(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":1}'`)
	pdfinfo := fakeTool(t, `yes x | tr -d '\n' | head -c 200; printf '\nPages: 1\n'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages:       10,
		Timeout:        10 * time.Second,
		MaxOutputBytes: 64,
		PDFInfoPath:    pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected pdfinfo output cap to invalidate report, got %+v", report)
	}
	if !strings.Contains(report.Reason, "pdfinfo output exceeds cap") {
		t.Fatalf("Reason=%q want pdfinfo output exceeds cap report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoSkippedForInvalidReport(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"encrypted PDF"}'`)
	pdfinfo := fakeTool(t, `printf 'boom' >&2; exit 1`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		Timeout:     10 * time.Second,
		PDFInfoPath: pdfinfo,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if report.Valid {
		t.Fatalf("expected worker invalid to stay invalid, got %+v", report)
	}
	if report.Reason != "encrypted PDF" {
		t.Fatalf("Reason=%q want encrypted PDF (cross-check must be skipped) report=%+v", report.Reason, report)
	}
}

func TestCrossCheckPDFInfoSkippedWhenBinaryEmpty(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
		MaxPages: 10,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("ValidateStructural err=%v", err)
	}
	if !report.Valid || report.Pages != 2 {
		t.Fatalf("expected cross-check skip with empty binary to keep Valid report, got %+v err=%v", report, err)
	}
}

func TestCrossCheckPDFInfoDirectBranches(t *testing.T) {
	t.Run("agree case-insensitive with spaces", func(t *testing.T) {
		pdfinfo := fakeTool(t, `printf 'pages:    2   \n'`)
		report := StructuralReport{Valid: true, Pages: 2}
		if err := crossCheckPDFInfo(context.Background(), pdfinfo, writeTempPDF(t), &report, 64<<10); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("empty output means no page count", func(t *testing.T) {
		pdfinfo := fakeTool(t, `printf ''`)
		report := StructuralReport{Valid: true, Pages: 1}
		err := crossCheckPDFInfo(context.Background(), pdfinfo, writeTempPDF(t), &report, 64<<10)
		if err == nil || !strings.Contains(err.Error(), "pdfinfo output did not contain page count") {
			t.Fatalf("err=%v want did not contain page count", err)
		}
	})
	t.Run("pages line with colon in value", func(t *testing.T) {
		pdfinfo := fakeTool(t, `printf 'Pages: 2: extra\n'`)
		report := StructuralReport{Valid: true, Pages: 2}
		err := crossCheckPDFInfo(context.Background(), pdfinfo, writeTempPDF(t), &report, 64<<10)
		if err == nil || !strings.Contains(err.Error(), "pdfinfo page count disagrees with worker") {
			t.Fatalf("err=%v want disagrees", err)
		}
	})
}
