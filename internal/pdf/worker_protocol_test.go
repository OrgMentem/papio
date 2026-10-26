// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"context"
	"errors"
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

func TestCrossCheckPDFInfo(t *testing.T) {
	for _, tc := range []struct {
		name           string
		workerJSON     string
		pdfinfoScript  string
		maxOutputBytes int64
		wantValid      bool
		skipPDFInfo    bool
	}{
		{name: "agrees", workerJSON: `{"Valid":true,"Pages":2}`, pdfinfoScript: `printf 'Creator: test\nPages: 2\n'`, wantValid: true},
		{name: "disagrees", workerJSON: `{"Valid":true,"Pages":2}`, pdfinfoScript: `printf 'Pages: 5\n'`},
		{name: "exit nonzero", workerJSON: `{"Valid":true,"Pages":3}`, pdfinfoScript: `printf 'pdfinfo failed: boom' >&2; exit 1`},
		{name: "missing pages line", workerJSON: `{"Valid":true,"Pages":2}`, pdfinfoScript: `printf 'Title: hello\nCreator: test\nProducer: x\n'`},
		{name: "nonnumeric pages", workerJSON: `{"Valid":true,"Pages":2}`, pdfinfoScript: `printf 'Pages: abc\n'`},
		{name: "output exceeds cap", workerJSON: `{"Valid":true,"Pages":1}`, pdfinfoScript: `yes x | tr -d '\n' | head -c 200; printf '\nPages: 1\n'`, maxOutputBytes: 64},
		{name: "invalid worker report skips pdfinfo", workerJSON: `{"Valid":false,"Reason":"encrypted PDF"}`, pdfinfoScript: `exit 1`, skipPDFInfo: true},
		{name: "empty binary skips cross-check", workerJSON: `{"Valid":true,"Pages":2}`, wantValid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '`+tc.workerJSON+`'`)
			pdfinfo := ""
			marker := t.TempDir() + "/pdfinfo-invoked"
			if tc.pdfinfoScript != "" {
				script := tc.pdfinfoScript
				if tc.skipPDFInfo {
					script = `touch "` + marker + `"; ` + script
				}
				pdfinfo = fakeTool(t, script)
			}
			report, err := ValidateStructural(context.Background(), worker, writeTempPDF(t), StructuralOptions{
				MaxPages: 10, Timeout: 10 * time.Second,
				MaxOutputBytes: tc.maxOutputBytes, PDFInfoPath: pdfinfo,
			})
			if err != nil {
				t.Fatalf("ValidateStructural err=%v", err)
			}
			if report.Valid != tc.wantValid || (tc.wantValid && report.Pages != 2) {
				t.Fatalf("report=%+v, want valid=%v and two pages when valid", report, tc.wantValid)
			}
			if tc.skipPDFInfo {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("pdfinfo ran after an invalid worker report: stat err=%v", err)
				}
			}
		})
	}
}

func TestCrossCheckPDFInfoTimeout(t *testing.T) {
	// Exercise the deadline directly so this test does not depend on worker
	// startup time. The parent-path tests cover propagation of this error.
	pdfinfo := fakeTool(t, `sleep 0.5; printf 'Pages: 1\n'`)
	report := StructuralReport{Valid: true, Pages: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := crossCheckPDFInfo(ctx, pdfinfo, writeTempPDF(t), &report, 64<<10)
	elapsed := time.Since(start)
	var infrastructure *CheckerInfrastructureError
	if !errors.As(err, &infrastructure) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want checker infrastructure error wrapping deadline", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("timeout test took %v, exceeds 300ms budget (sleep should be interrupted by context)", elapsed)
	}
}

func TestStructuralPDFInfoInfrastructureFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		script        string
		wantCause     error
		timeout       time.Duration
		parentTimeout time.Duration
	}{
		{name: "cannot exec", wantCause: os.ErrNotExist},
		{name: "timeout", script: `sleep 5; printf 'Pages: 2\n'`, wantCause: context.DeadlineExceeded, timeout: 2 * time.Second},
		{name: "parent deadline", script: `sleep 5; printf 'Pages: 2\n'`, wantCause: context.DeadlineExceeded, parentTimeout: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
			pdfinfo := t.TempDir() + "/missing-pdfinfo"
			if tc.script != "" {
				pdfinfo = fakeTool(t, tc.script)
			}
			ctx := t.Context()
			if tc.parentTimeout != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parentTimeout)
				defer cancel()
			}
			report, err := ValidateStructural(ctx, worker, writeTempPDF(t), StructuralOptions{
				PDFInfoPath: pdfinfo, Timeout: tc.timeout,
			})
			var infrastructure *CheckerInfrastructureError
			if !errors.As(err, &infrastructure) || !errors.Is(err, tc.wantCause) {
				t.Fatalf("report=%+v err=%v, want checker infrastructure error wrapping %v", report, err, tc.wantCause)
			}
			if infrastructure.Tool != pdfinfo {
				t.Fatalf("checker=%q, want pdfinfo %q", infrastructure.Tool, pdfinfo)
			}
		})
	}
}

func TestStructuralPDFInfoParentCancellation(t *testing.T) {
	worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
	marker := t.TempDir() + "/pdfinfo-started"
	pdfinfo := fakeTool(t, `touch "`+marker+`"; sleep 5; printf 'Pages: 2\n'`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		report StructuralReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := ValidateStructural(ctx, worker, "unused.pdf", StructuralOptions{PDFInfoPath: pdfinfo})
		done <- result{report, err}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case got := <-done:
			t.Fatalf("validation finished before cancellation: report=%+v err=%v", got.report, got.err)
		case <-deadline.C:
			t.Fatal("pdfinfo did not start")
		case <-tick.C:
		}
	}
	cancel()
	got := <-done
	var infrastructure *CheckerInfrastructureError
	if !errors.As(got.err, &infrastructure) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("report=%+v err=%v, want checker infrastructure error wrapping cancellation", got.report, got.err)
	}
}

func TestCryptFilterFallbackInfrastructureFailure(t *testing.T) {
	for _, stage := range []string{"pdfinfo", "pdfdetach", "JavaScript timeout"} {
		t.Run(stage, func(t *testing.T) {
			worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Reason":"`+cryptFilterReason+`"}'`)
			pdfinfo := fakeTool(t, `if [ "$1" = "-js" ]; then exit 0; fi; printf 'Pages: 2\nEncrypted: no\n'`)
			pdfdetach := fakeTool(t, `printf '0 embedded files\n'`)
			wantCause := os.ErrNotExist
			var timeout time.Duration
			switch stage {
			case "pdfinfo":
				pdfinfo = t.TempDir() + "/missing-pdfinfo"
			case "pdfdetach":
				pdfdetach = t.TempDir() + "/missing-pdfdetach"
			case "JavaScript timeout":
				pdfinfo = fakeTool(t, `if [ "$1" = "-js" ]; then sleep 5; exit 0; fi; printf 'Pages: 2\nEncrypted: no\n'`)
				wantCause = context.DeadlineExceeded
				timeout = 2 * time.Second
			}
			report, err := ValidateStructural(t.Context(), worker, writeTempPDF(t), StructuralOptions{
				PDFInfoPath: pdfinfo, PDFDetachPath: pdfdetach, Timeout: timeout,
			})
			var infrastructure *CheckerInfrastructureError
			if !errors.As(err, &infrastructure) || !errors.Is(err, wantCause) {
				t.Fatalf("report=%+v err=%v, want checker infrastructure error wrapping %v", report, err, wantCause)
			}
		})
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
