// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import "encoding/json"

// ReportSchemaVersion versions the persisted validation-evidence document.
// Readers route on it exactly as they do on acquisition-bundle/N.
const ReportSchemaVersion = "validation-report/1"

// ReportDocument is ValidationReport as a durable, machine-readable record.
//
// It exists as a separate type because ValidationReport is an in-process value:
// its fields carry no JSON tags, its shape follows the pipeline's convenience,
// and Text.Excerpt holds extracted document text that has no business being
// persisted or shipped over IPC — the excerpt is an input to the identity
// decision, and the decision plus its evidence is what a consumer needs. Every
// other stage field is preserved verbatim, including the reasons and evidence
// lines the artifacts row could never hold.
type ReportDocument struct {
	SchemaVersion string           `json:"schema_version"`
	Payload       PayloadDocument  `json:"payload"`
	Structural    StructuralDoc    `json:"structural"`
	Text          TextDocument     `json:"text"`
	Identity      IdentityDocument `json:"identity"`
	Evidence      []string         `json:"evidence,omitempty"`
}

// PayloadDocument is the cheap payload gate's verdict.
type PayloadDocument struct {
	OK          bool   `json:"ok"`
	SizeBytes   int64  `json:"size_bytes"`
	HasHeader   bool   `json:"has_header"`
	HasEOF      bool   `json:"has_eof"`
	SniffedMIME string `json:"sniffed_mime,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// StructuralDoc is the isolated worker's parse verdict.
type StructuralDoc struct {
	Valid            bool   `json:"valid"`
	Pages            int    `json:"pages"`
	Encrypted        bool   `json:"encrypted"`
	HasJavaScript    bool   `json:"has_javascript"`
	HasEmbeddedFiles bool   `json:"has_embedded_files"`
	Reason           string `json:"reason,omitempty"`
}

// TextDocument is the semantic extraction verdict, without the excerpt.
type TextDocument struct {
	Chars       int64    `json:"chars"`
	OCRUsed     bool     `json:"ocr_used"`
	NeedsReview bool     `json:"needs_review"`
	Evidence    []string `json:"evidence,omitempty"`
}

// IdentityDocument is the "is this the requested work" verdict.
type IdentityDocument struct {
	Result   string   `json:"result"`
	Evidence []string `json:"evidence,omitempty"`
}

// Document projects a report into its durable form.
func Document(report ValidationReport) ReportDocument {
	return ReportDocument{
		SchemaVersion: ReportSchemaVersion,
		Payload: PayloadDocument{
			OK: report.Payload.OK, SizeBytes: report.Payload.SizeBytes,
			HasHeader: report.Payload.HasHeader, HasEOF: report.Payload.HasEOF,
			SniffedMIME: report.Payload.SniffedMIME, Reason: report.Payload.Reason,
		},
		Structural: StructuralDoc{
			Valid: report.Structural.Valid, Pages: report.Structural.Pages,
			Encrypted: report.Structural.Encrypted, HasJavaScript: report.Structural.HasJavaScript,
			HasEmbeddedFiles: report.Structural.HasEmbeddedFiles, Reason: report.Structural.Reason,
		},
		Text: TextDocument{
			Chars: report.Text.Chars, OCRUsed: report.Text.OCRUsed,
			NeedsReview: report.Text.NeedsReview, Evidence: report.Text.Evidence,
		},
		Identity: IdentityDocument{Result: report.Identity.Result, Evidence: report.Identity.Evidence},
		Evidence: report.Evidence,
	}
}

// EncodeDocument renders the durable form of one report.
func EncodeDocument(report ValidationReport) ([]byte, error) {
	return json.Marshal(Document(report))
}
