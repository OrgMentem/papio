// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// attachedPDF builds a real PDF carrying one embedded file, which is what a
// publisher's supplementary attachment looks like to the structural worker.
func attachedPDF(t *testing.T) string {
	t.Helper()
	plain := writePDFWithPages(t, 2)
	dir := t.TempDir()
	payload := filepath.Join(dir, "supplement.txt")
	if err := os.WriteFile(payload, []byte("supplementary material"), 0o600); err != nil {
		t.Fatal(err)
	}
	attached := filepath.Join(dir, "attached.pdf")
	if err := api.AddAttachmentsFile(plain, attached, []string{payload}, false, model.NewDefaultConfiguration()); err != nil {
		t.Fatal(err)
	}
	return attached
}

func workerReport(t *testing.T, req workerRequest) StructuralReport {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunStructuralWorker(bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var report StructuralReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

// The rewrite is what the report describes, so a caller can trust the answer
// without trusting the removal. Measured live 2026-08-27 against two SAGE
// papers whose only marker was one attachment: both rewrote to Valid with no
// markers and their full page count.
func TestSanitizeWorkerReportsTheRewriteNotTheSource(t *testing.T) {
	attached := attachedPDF(t)
	if source := workerReport(t, workerRequest{Path: attached, MaxPages: 10}); !source.HasEmbeddedFiles {
		t.Fatalf("fixture carries no attachment: %+v", source)
	}
	dest := filepath.Join(t.TempDir(), "sanitized.pdf")
	report := workerReport(t, workerRequest{Path: attached, MaxPages: 10, SanitizeTo: dest})
	if !report.Valid || report.HasEmbeddedFiles || report.HasJavaScript || report.Encrypted {
		t.Fatalf("sanitized report = %+v, want a clean valid PDF", report)
	}
	if report.Pages != 2 {
		t.Fatalf("pages = %d, want the source's 2", report.Pages)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("sanitized file missing: %v", err)
	}
	// The source is evidence and must survive its own rewrite.
	if _, err := os.Stat(attached); err != nil {
		t.Fatalf("source removed by sanitizing: %v", err)
	}
}

// A PDF with nothing to remove must fail rather than report a clean rewrite:
// pdfcpu removes no attachment, and a destination that silently became the
// answer would let this path launder any file it cannot actually change.
func TestSanitizeWorkerFailsWithNothingToRemove(t *testing.T) {
	plain := writePDFWithPages(t, 1)
	dest := filepath.Join(t.TempDir(), "sanitized.pdf")
	report := workerReport(t, workerRequest{Path: plain, MaxPages: 10, SanitizeTo: dest})
	if report.Valid {
		t.Fatalf("report = %+v, want a refusal", report)
	}
	if report.Reason != "strip embedded files failed" {
		t.Fatalf("reason = %q, want the category only", report.Reason)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a failed rewrite must leave no destination file")
	}
}

// The parent seam cross-checks pdfinfo against the file the report describes,
// so it must never be handed the source path.
func TestSanitizeEmbeddedFilesRefusesDegenerateDestinations(t *testing.T) {
	if _, err := SanitizeEmbeddedFiles(t.Context(), "papio", "/tmp/in.pdf", "", StructuralOptions{}); err == nil {
		t.Fatal("empty destination accepted")
	}
	if _, err := SanitizeEmbeddedFiles(t.Context(), "papio", "/tmp/in.pdf", "/tmp/in.pdf", StructuralOptions{}); err == nil {
		t.Fatal("in-place rewrite accepted")
	}
}
