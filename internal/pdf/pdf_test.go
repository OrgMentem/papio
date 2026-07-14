// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/work"
)

func TestValidatePayload(t *testing.T) {
	valid := append([]byte("%PDF-1.7\n"), []byte(strings.Repeat("x", MinimumPayloadBytes))...)
	valid = append(valid, []byte("\n%%EOF")...)
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"valid", valid, true},
		{"html claiming PDF", []byte("<html><body>nope</body></html>" + strings.Repeat("x", MinimumPayloadBytes)), false},
		{"short body", []byte("%PDF-1.7\n%%EOF"), false},
		{"header not at byte zero", append([]byte("\xef\xbb\xbf"), valid...), false},
		{"early eof with appended payload", append([]byte("%PDF-1.7\n%%EOF"), []byte(strings.Repeat("x", 9000))...), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidatePayload(tc.body, "text/html") // MIME must not be decisive.
			if got.OK != tc.want {
				t.Fatalf("OK=%v, reason=%q", got.OK, got.Reason)
			}
		})
	}
}

func TestStructuralWorkerContractCapsAndDeadline(t *testing.T) {
	path := writeTempPDF(t)
	t.Run("worker contract", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":true,"Pages":2}'`)
		got, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{MaxPages: 10})
		if err != nil || !got.Valid || got.Pages != 2 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("output cap", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; yes x | tr -d '\n' | head -c 10000`)
		_, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{MaxOutputBytes: 32})
		if err == nil || !strings.Contains(err.Error(), "output exceeds") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("deadline kills worker", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; sleep 10`)
		start := time.Now()
		_, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{Timeout: 50 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("worker was not killed promptly")
		}
	})
	t.Run("encrypted report is rejected", func(t *testing.T) {
		worker := fakeTool(t, `cat >/dev/null; printf '%s\n' '{"Valid":false,"Encrypted":true,"Reason":"encrypted PDF"}'`)
		got, err := ValidateStructural(context.Background(), worker, path, StructuralOptions{})
		if err != nil || got.Valid || !got.Encrypted {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
}

func TestStructuralWorkerParsesOnlyAtWorkerEntry(t *testing.T) {
	validPath := writeRealPDF(t)
	var out bytes.Buffer
	request, _ := json.Marshal(workerRequest{Path: validPath, MaxPages: 10})
	if err := RunStructuralWorker(bytes.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var valid StructuralReport
	if err := json.Unmarshal(out.Bytes(), &valid); err != nil || !valid.Valid || valid.Pages != 1 {
		t.Fatalf("valid report=%+v err=%v", valid, err)
	}

	malformed := filepath.Join(t.TempDir(), "malformed.pdf")
	if err := os.WriteFile(malformed, []byte("%PDF-1.7 broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	request, _ = json.Marshal(workerRequest{Path: malformed, MaxPages: 10})
	if err := RunStructuralWorker(bytes.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var bad StructuralReport
	if err := json.Unmarshal(out.Bytes(), &bad); err != nil || bad.Valid || bad.Reason == "" {
		t.Fatalf("malformed report=%+v err=%v", bad, err)
	}
	if err := RunStructuralWorker(bytes.NewReader(bytes.Repeat([]byte("x"), 16<<10+1)), &out); err == nil {
		t.Fatal("expected worker request body cap rejection")
	}
}

func writeRealPDF(t *testing.T) string {
	t.Helper()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	path := filepath.Join(t.TempDir(), "valid.pdf")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTextAndOCRCapabilityEvidence(t *testing.T) {
	path := writeTempPDF(t)
	pdftotext := fakeTool(t, `printf 'short text'`)
	report, err := ExtractText(context.Background(), path, Capability{PDFToText: pdftotext}, DefaultSemanticOptions())
	if err != nil || !report.NeedsReview || report.OCRUsed {
		t.Fatalf("got=%+v err=%v", report, err)
	}
	if !strings.Contains(strings.Join(report.Evidence, " "), "capability") {
		t.Fatalf("missing capability evidence: %+v", report)
	}

	hung := fakeTool(t, `sleep 10`)
	report, err = ExtractText(context.Background(), path, Capability{PDFToText: hung}, SemanticOptions{Timeout: 50 * time.Millisecond})
	if err != nil || !report.NeedsReview || !strings.Contains(strings.Join(report.Evidence, " "), "deadline") {
		t.Fatalf("got=%+v err=%v", report, err)
	}

	pdftoppm := fakeTool(t, `for last; do :; done; printf png > "$last-1.png"`)
	tesseract := fakeTool(t, `i=0; while [ "$i" -lt 300 ]; do printf 'imageword '; i=$((i+1)); done`)
	report, err = ExtractText(context.Background(), path, Capability{PDFToText: pdftotext, PDFToPPM: pdftoppm, Tesseract: tesseract}, DefaultSemanticOptions())
	if err != nil || !report.OCRUsed || report.NeedsReview || report.Chars < 1000 {
		t.Fatalf("OCR report=%+v err=%v", report, err)
	}
}

func TestMatchIdentity(t *testing.T) {
	target := work.Work{DOI: "10.1234/ABC.9", Title: "Deterministic Validation of Scholarly Article Identity", Authors: []string{"Ada Lovelace"}, Year: 2026}
	if got := MatchIdentity("Supporting Information doi:10.1234/abc.9", target); got.Result != IdentityReject {
		t.Fatalf("marker: %+v", got)
	}
	if got := MatchIdentity("doi:10.9999/nope", target); got.Result != IdentityReject {
		t.Fatalf("wrong DOI: %+v", got)
	}
	if got := MatchIdentity("References doi:10.9999/nope; article DOI:10.1234/abc.9", target); got.Result != IdentityPass {
		t.Fatalf("matching DOI after reference: %+v", got)
	}
	text := "Deterministic validation of scholarly article identity. Ada Lovelace (2026)."
	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("title match: %+v", got)
	}
  legacyAPA := work.Work{DOI: "10.1037/0021-9010.87.4.611"}
  if got := MatchIdentity("Copyright line DOI: 10.1037//0021-9010.87.4.611", legacyAPA); got.Result != IdentityPass {
    t.Fatalf("legacy APA DOI: %+v", got)
  }
}

func TestMatchIdentityHonorsTitleThreshold(t *testing.T) {
	target := work.Work{Title: "Quantum Networks Robustness Calibration Measurement", Authors: []string{"Lovelace"}, Year: 2026}
	text := "Quantum networks robustness. Lovelace (2026)."
	if got := MatchIdentityWithThreshold(text, target, 0.8); got.Result != IdentityReject {
		t.Fatalf("80%% threshold result = %+v, want reject", got)
	}
	if got := MatchIdentityWithThreshold(text, target, 0.6); got.Result != IdentityPass {
		t.Fatalf("60%% threshold result = %+v, want pass", got)
	}
}

func writeTempPDF(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.7\n"+strings.Repeat("x", MinimumPayloadBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeTool(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}
