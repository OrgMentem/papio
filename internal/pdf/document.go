// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ReportSchemaVersion versions the persisted validation-evidence document.
// Readers route on it exactly as they do on acquisition-bundle/N.
const ReportSchemaVersion = "validation-report/1"

// maxEvidenceBytes bounds one evidence or reason line.
//
// These strings are not papio's prose. Several are a third-party parser's
// stderr, produced while reading a file a publisher served, and one used to
// carry the absolute quarantine path. Until this document existed they were
// computed and discarded, so nothing bounded them; now they are durable, they
// cross IPC, and `papio artifacts validation` is mcp:read-only, so an agent
// reads them too. api.safeMessage applies the same 500-byte discipline to every
// error crossing IPC and app.safeType refuses to persist upstream error text at
// all — this is the same rule at the one boundary every writer and reader of
// this document passes through.
const maxEvidenceBytes = 500

// ReportDocument is ValidationReport as a durable, machine-readable record.
//
// It exists as a separate type because ValidationReport is an in-process value:
// its fields carry no JSON tags, its shape follows the pipeline's convenience,
// and Text.Excerpt holds extracted document text that has no business being
// persisted or shipped over IPC — the excerpt is an input to the identity
// decision, and the decision plus its evidence is what a consumer needs. Every
// other stage field is preserved, bounded by safeEvidence, including the reasons
// the artifacts row could never hold.
//
// ValidationReport's own top-level Evidence field is deliberately NOT projected:
// no producer in this package ever sets it (pdf.Validate fills Payload,
// Structural, Text and Identity only), so carrying it would ship a key into a
// versioned schema that is unreachable by construction and permanently absent
// for every consumer writing a decoder against it.
type ReportDocument struct {
	SchemaVersion string           `json:"schema_version"`
	Payload       PayloadDocument  `json:"payload"`
	Structural    StructuralDoc    `json:"structural"`
	Text          TextDocument     `json:"text"`
	Identity      IdentityDocument `json:"identity"`
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

// Document projects a report into its durable form, bounding every
// caller-influenced string on the way through.
func Document(report ValidationReport) ReportDocument {
	return ReportDocument{
		SchemaVersion: ReportSchemaVersion,
		Payload: PayloadDocument{
			OK: report.Payload.OK, SizeBytes: report.Payload.SizeBytes,
			HasHeader: report.Payload.HasHeader, HasEOF: report.Payload.HasEOF,
			SniffedMIME: safeEvidence(report.Payload.SniffedMIME),
			Reason:      safeEvidence(report.Payload.Reason),
		},
		Structural: StructuralDoc{
			Valid: report.Structural.Valid, Pages: report.Structural.Pages,
			Encrypted: report.Structural.Encrypted, HasJavaScript: report.Structural.HasJavaScript,
			HasEmbeddedFiles: report.Structural.HasEmbeddedFiles,
			Reason:           safeEvidence(report.Structural.Reason),
		},
		Text: TextDocument{
			Chars: report.Text.Chars, OCRUsed: report.Text.OCRUsed,
			NeedsReview: report.Text.NeedsReview, Evidence: safeEvidenceLines(report.Text.Evidence),
		},
		Identity: IdentityDocument{
			Result:   report.Identity.Result,
			Evidence: safeEvidenceLines(report.Identity.Evidence),
		},
	}
}

// safeEvidence bounds one line and strips the control characters a terminal or a
// log would act on. Tabs and newlines become spaces rather than vanishing, so a
// multi-line parser error stays readable as one line; C0, DEL and C1 are dropped
// outright. Truncation is marked, because a silently cut reason reads like a
// parser that stopped mid-sentence.
func safeEvidence(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		default:
			return r
		}
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > maxEvidenceBytes {
		// Cut on a rune boundary: a half-encoded rune would make the whole
		// document invalid UTF-8 for a strict consumer.
		cut := maxEvidenceBytes
		for cut > 0 && !utf8.ValidString(value[:cut]) {
			cut--
		}
		return value[:cut] + "…"
	}
	return value
}

func safeEvidenceLines(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if bounded := safeEvidence(line); bounded != "" {
			out = append(out, bounded)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EncodeDocument renders the durable form of one report.
func EncodeDocument(report ValidationReport) ([]byte, error) {
	return json.Marshal(Document(report))
}
