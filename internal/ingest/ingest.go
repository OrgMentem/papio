// Package ingest parses standard bibliographic interchange formats — RIS,
// BibTeX/BibLaTeX, CSL-JSON, and MEDLINE/NBIB — into the same canonical work
// requests that `papio acquire --batch` builds from JSONL. One robust
// standards pipeline connects reference managers, database exports, and
// systematic-review tools without bespoke integrations.
//
// Every record funnels through batch.ParseWork, so identifier
// normalization, deterministic request IDs, and validation are identical to
// JSONL input by construction.
package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"papio/internal/batch"
	"papio/internal/protocol"
)

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

// Record is one bibliographic entry extracted from an input file, restricted
// to the fields papio's acquisition identity uses. Parsers leave fields they
// cannot recover empty; conversion validates that enough identity remains.
type Record struct {
	DOI     string
	PMID    string
	ArXiv   string
	ISBN    string
	Title   string
	Authors []string
	Year    int
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

// Parse converts input in the given format into canonical work requests.
// FormatJSONL is not handled here — the CLI's existing JSONL reader owns it —
// and requesting it is a programming error surfaced as such.
func Parse(format Format, data []byte) ([]protocol.WorkRequest, error) {
	var records []Record
	var err error
	switch format {
	case FormatRIS:
		records, err = parseRIS(data)
	case FormatBibTeX:
		records, err = parseBibTeX(data)
	case FormatCSLJSON:
		records, err = parseCSLJSON(data)
	case FormatNBIB:
		records, err = parseNBIB(data)
	case FormatJSONL:
		return nil, fmt.Errorf("jsonl input is parsed by the batch reader, not ingest")
	default:
		return nil, fmt.Errorf("unsupported bibliographic format %q", format)
	}
	if err != nil {
		return nil, err
	}
	requests := make([]protocol.WorkRequest, 0, len(records))
	for i, record := range records {
		request, err := convert(record)
		if err != nil {
			return nil, fmt.Errorf("%s record %d (%s): %w", format, i+1, describe(record), err)
		}
		requests = append(requests, request)
	}
	return requests, nil
}

// convert funnels a parsed record through the exact JSONL work pipeline:
// synthesize the native envelope, then reuse batch.ParseWork so identifier
// normalization, request IDs, and validation cannot drift between formats.
func convert(record Record) (protocol.WorkRequest, error) {
	envelope := map[string]any{}
	for key, value := range map[string]string{
		"doi":   record.DOI,
		"pmid":  record.PMID,
		"arxiv": record.ArXiv,
		"isbn":  record.ISBN,
		"title": record.Title,
	} {
		if value = strings.TrimSpace(value); value != "" {
			envelope[key] = value
		}
	}
	if authors := nonempty(record.Authors); len(authors) != 0 {
		envelope["authors"] = authors
	}
	if record.Year != 0 {
		envelope["year"] = record.Year
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("encoding record: %w", err)
	}
	return batch.ParseWork(data)
}

func describe(record Record) string {
	switch {
	case record.DOI != "":
		return "doi:" + record.DOI
	case record.PMID != "":
		return "pmid:" + record.PMID
	case record.Title != "":
		return record.Title
	}
	return "unidentified"
}

func nonempty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstByte(data []byte) byte {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}
