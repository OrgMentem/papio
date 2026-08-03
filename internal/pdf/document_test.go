// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
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
		Evidence: []string{"validator build 2026.08", "bounded parse completed"},
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
		Evidence: []string{"validator build 2026.08", "bounded parse completed"},
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
