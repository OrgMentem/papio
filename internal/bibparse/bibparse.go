// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package bibparse decodes standard bibliographic interchange formats — RIS,
// BibTeX/BibLaTeX, CSL-JSON, and MEDLINE/NBIB — into a neutral Record.
//
// It is deliberately dependency-free beyond the standard library. Two very
// different consumers share these parsers: internal/ingest converts records
// into acquisition work requests (strict — one unconvertible record aborts the
// batch, because a user asking to acquire twenty papers should not silently get
// nineteen), and internal/ownershipsnapshot indexes records as *holdings*
// (tolerant — one identifier-less entry in a five-thousand-entry library must
// not disable de-duplication for the whole source). Keeping the raw parsers
// here lets each own its own tolerance, and keeps internal/ingest's dependency
// on internal/batch out of the ownership graph (ADR-0008).
package bibparse

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrNoEntries reports a syntactically valid, completely empty source.
// Callers may treat it as proof that a source contains no records. Non-empty
// input that cannot yield a bibliographic record is an ordinary parse error so
// it is never mistaken for a complete empty export.
var ErrNoEntries = errors.New("no entries found")

// noEntries wraps ErrNoEntries while preserving each parser's own wording for
// syntactically valid empty input.
func noEntries(message string) error { return noEntriesError{message: message} }

type noEntriesError struct{ message string }

func (e noEntriesError) Error() string { return e.message }

func (e noEntriesError) Unwrap() error { return ErrNoEntries }

// Format identifies a supported bibliographic input encoding.
type Format string

const (
	// FormatJSONL is papio's native batch format; Detect returns it as the
	// default so existing `--batch` invocations are unchanged.
	FormatJSONL Format = "jsonl"
	// FormatRIS covers RIS exports (EndNote, Zotero, Covidence, Rayyan, most
	// databases): tagged `XX  - value` lines with `TY` opening and `ER`
	// closing each record.
	FormatRIS Format = "ris"
	// FormatBibTeX covers BibTeX/BibLaTeX files: `@type{key, field = value}`.
	FormatBibTeX Format = "bibtex"
	// FormatCSLJSON covers CSL-JSON: a top-level JSON array of item objects.
	FormatCSLJSON Format = "csl-json"
	// FormatNBIB covers MEDLINE/PubMed .nbib exports: `TAG - value` lines
	// with 4-character space-padded tags and indented continuations.
	FormatNBIB Format = "nbib"
)

// Record is one bibliographic entry, restricted to the fields papio's identity
// matching uses. Parsers leave fields they cannot recover empty.
//
// Identifier coverage differs per format because the formats differ, and
// guessing is worse than leaving a field empty:
//
//   - DOI: every format.
//   - PMID: NBIB (`PMID`), CSL-JSON (`PMID`), BibTeX (`pmid`, emitted by
//     several managers).
//   - ArXiv: BibTeX/BibLaTeX only, via `eprint` + `archiveprefix`/`eprinttype`.
//     RIS has no standardized arXiv tag and CSL-JSON has no arXiv field; both
//     are left empty rather than scraped out of a free-text note.
//   - ISBN: populated by no parser. ADR-0008 excludes ISBN from ownership
//     matching entirely — an edited volume shares one ISBN with every chapter
//     in it, so an ISBN match cannot tell twenty distinct chapter requests
//     apart. The field remains because acquisition metadata may carry an ISBN
//     from other input paths.
type Record struct {
	DOI     string
	PMID    string
	ArXiv   string
	ISBN    string
	Title   string
	Authors []string
	Year    int
}

// HasIdentifier reports whether a record carries any identifier papio can match
// on exactly. Holdings indexing counts records without one rather than
// rejecting them.
func (r Record) HasIdentifier() bool {
	return strings.TrimSpace(r.DOI) != "" || strings.TrimSpace(r.PMID) != "" || strings.TrimSpace(r.ArXiv) != ""
}

// Detect classifies batch input, extension first and content sniff second.
// The zero-signal answer is FormatJSONL: papio's native format keeps working
// for `-` stdin and extensionless paths that look like JSON objects.
func Detect(path string, data []byte) Format {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ris":
		return FormatRIS
	case ".bib", ".bibtex":
		return FormatBibTeX
	case ".json":
		// .json is CSL-JSON only when it is a top-level array; a JSON object
		// per line is native JSONL regardless of extension.
		if firstByte(data) == '[' {
			return FormatCSLJSON
		}
		return FormatJSONL
	case ".nbib":
		return FormatNBIB
	case ".jsonl", ".ndjson":
		return FormatJSONL
	}
	return sniff(data)
}

// sniff classifies extensionless input (stdin, tempfiles) by leading content.
func sniff(data []byte) Format {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	switch {
	case len(trimmed) == 0:
		return FormatJSONL
	case trimmed[0] == '[':
		return FormatCSLJSON
	case trimmed[0] == '{':
		return FormatJSONL
	case trimmed[0] == '@':
		return FormatBibTeX
	case bytes.HasPrefix(trimmed, []byte("TY  -")):
		return FormatRIS
	case bytes.HasPrefix(trimmed, []byte("PMID-")):
		return FormatNBIB
	}
	return FormatJSONL
}

// ParseRecords decodes input into raw records without validating them.
//
// A structural error — malformed BibTeX braces, invalid JSON, an RIS record
// with no terminator — is returned as an error, because the caller cannot tell
// how much of the file it lost. A record that merely carries no usable
// identifier is returned as an ordinary Record: only the caller knows whether
// that is a defect (acquisition) or an ordinary library entry such as a book or
// a hand-typed note (holdings).
//
// FormatJSONL is not handled here; papio's native batch reader owns it.
func ParseRecords(format Format, data []byte) ([]Record, error) {
	switch format {
	case FormatRIS:
		return parseRIS(data)
	case FormatBibTeX:
		return parseBibTeX(data)
	case FormatCSLJSON:
		return parseCSLJSON(data)
	case FormatNBIB:
		return parseNBIB(data)
	case FormatJSONL:
		return nil, fmt.Errorf("jsonl input is parsed by the batch reader, not bibparse")
	default:
		return nil, fmt.Errorf("unsupported bibliographic format %q", format)
	}
}

func firstByte(data []byte) byte {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}
