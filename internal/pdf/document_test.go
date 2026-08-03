// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocumentPreservesValidationEvidenceWithoutExcerpt(t *testing.T) {
	const excerpt = "EXCERPT_MUST_NOT_BE_PERSISTED_7f32c"
	report := ValidationReport{
		Payload: PayloadReport{
			OK:          false,
			SizeBytes:   4096,
			HasHeader:   true,
			HasEOF:      false,
			SniffedMIME: "application/pdf",
			Reason:      "payload gate: missing EOF marker",
		},
		Structural: StructuralReport{
			Valid:            false,
			Pages:            9,
			Encrypted:        true,
			HasJavaScript:    true,
			HasEmbeddedFiles: true,
			Reason:           "structural parser rejected the document",
		},
		Text: TextReport{
			Chars:       1234,
			Excerpt:     excerpt,
			OCRUsed:     true,
			NeedsReview: true,
			Evidence:    []string{"OCR capability used", "sparse extraction"},
		},
		Identity: IdentityDecision{
			Result:   IdentityReview,
			Evidence: []string{"title match was ambiguous"},
		},
	}
	want := ReportDocument{
		SchemaVersion: ReportSchemaVersion,
		Payload: PayloadDocument{
			OK:          false,
			SizeBytes:   4096,
			HasHeader:   true,
			HasEOF:      false,
			SniffedMIME: "application/pdf",
			Reason:      "payload gate: missing EOF marker",
		},
		Structural: StructuralDoc{
			Valid:            false,
			Pages:            9,
			Encrypted:        true,
			HasJavaScript:    true,
			HasEmbeddedFiles: true,
			Reason:           "structural parser rejected the document",
		},
		Text: TextDocument{
			Chars:       1234,
			OCRUsed:     true,
			NeedsReview: true,
			Evidence:    []string{"OCR capability used", "sparse extraction"},
		},
		Identity: IdentityDocument{
			Result:   IdentityReview,
			Evidence: []string{"title match was ambiguous"},
		},
	}

	document := Document(report)
	if !reflect.DeepEqual(document, want) {
		t.Fatalf("Document(report) = %+v, want %+v", document, want)
	}

	encoded, err := EncodeDocument(report)
	if err != nil {
		t.Fatalf("EncodeDocument: %v", err)
	}
	if bytes.Contains(encoded, []byte(excerpt)) {
		t.Fatalf("encoded document persisted text excerpt %q: %s", excerpt, encoded)
	}
	if bytes.Contains(encoded, []byte(`"excerpt"`)) {
		t.Fatalf("encoded document contains an excerpt field: %s", encoded)
	}

	var decoded ReportDocument
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode encoded document: %v", err)
	}
	if decoded.SchemaVersion != ReportSchemaVersion {
		t.Fatalf("schema version = %q, want %q", decoded.SchemaVersion, ReportSchemaVersion)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded document = %+v, want %+v", decoded, want)
	}
}

// TestDocumentBoundsHostileEvidence pins the sanitization the durable document
// owes its readers. These strings are not papio's prose: several are a
// third-party parser's stderr produced while reading a publisher-supplied file,
// and one used to carry the absolute quarantine path. The document is persisted,
// crosses IPC, and is served through an mcp:read-only command, so it inherits the
// same discipline api.safeMessage applies to an error on the wire.
func TestDocumentBoundsHostileEvidence(t *testing.T) {
	long := strings.Repeat("x", maxEvidenceBytes*3)
	report := ValidationReport{
		Payload: PayloadReport{OK: false, Reason: "payload gate rejected\nsecond line\ttabbed"},
		Structural: StructuralReport{
			Valid:  false,
			Reason: "pdfcpu: " + long,
		},
		Text: TextReport{
			Evidence: []string{"pdftotext failed: \x1b[31mred\x1b[0m", "   ", "\x07bell"},
		},
		Identity: IdentityDecision{Result: IdentityReject, Evidence: []string{"title similarity 0.11"}},
	}

	document := Document(report)
	if strings.ContainsAny(document.Payload.Reason, "\n\r\t") {
		t.Fatalf("payload reason kept a line break: %q — it would forge a record boundary in a log or listing", document.Payload.Reason)
	}
	if len(document.Structural.Reason) > maxEvidenceBytes+len("…") {
		t.Fatalf("structural reason = %d bytes, want it bounded to %d", len(document.Structural.Reason), maxEvidenceBytes)
	}
	if !strings.HasSuffix(document.Structural.Reason, "…") {
		t.Fatalf("truncated reason does not say it was cut: %q", document.Structural.Reason)
	}
	if !utf8.ValidString(document.Structural.Reason) {
		t.Fatal("truncation split a rune, leaving the document invalid UTF-8")
	}
	for _, line := range document.Text.Evidence {
		if strings.ContainsRune(line, 0x1b) || strings.ContainsRune(line, 0x07) {
			t.Fatalf("evidence line kept a control character a terminal would act on: %q", line)
		}
	}
	// The whitespace-only line carried no evidence and is dropped rather than
	// emitted as an empty string a reader has to filter.
	if len(document.Text.Evidence) != 2 {
		t.Fatalf("evidence lines = %#v, want the two that carry text", document.Text.Evidence)
	}
	encoded, err := EncodeDocument(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(encoded, 0x1b) {
		t.Fatalf("encoded document carries a raw escape byte: %s", encoded)
	}
}

// TestDocumentOmitsTheUnreachableTopLevelEvidenceKey: ValidationReport.Evidence
// has no producer in this package — pdf.Validate fills Payload, Structural, Text
// and Identity only — so projecting it would ship a key into a versioned schema
// that is permanently absent for every consumer decoding against it.
func TestDocumentOmitsTheUnreachableTopLevelEvidenceKey(t *testing.T) {
	encoded, err := EncodeDocument(ValidationReport{
		Payload:    PayloadReport{OK: true},
		Structural: StructuralReport{Valid: true},
		Identity:   IdentityDecision{Result: IdentityPass},
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["evidence"]; ok {
		t.Fatalf("validation-report/1 declares a top-level evidence key: %s", encoded)
	}
	for _, want := range []string{"schema_version", "payload", "structural", "text", "identity"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("document is missing %q: %s", want, encoded)
		}
	}
}
