// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package ingest converts standard bibliographic interchange formats — RIS,
// BibTeX/BibLaTeX, CSL-JSON, and MEDLINE/NBIB — into the same canonical work
// requests that `papio acquire --batch` builds from JSONL. One robust standards
// pipeline connects reference managers, database exports, and systematic-review
// tools without bespoke integrations.
//
// Decoding lives in internal/bibparse; this package owns only the acquisition
// adapter. Every record funnels through batch.ParseWork, so identifier
// normalization, deterministic request IDs, and validation are identical to
// JSONL input by construction — which is also why the split exists: holdings
// indexing (ADR-0008) must reuse the parsers without inheriting the dependency
// on internal/batch, or the ownership graph inverts.
package ingest

import (
	"encoding/json"
	"fmt"
	"strings"

	"papio/internal/batch"
	"papio/internal/bibparse"
	"papio/internal/protocol"
)

// Format and Record are re-exported so existing callers keep one import.
type (
	Format = bibparse.Format
	Record = bibparse.Record
)

const (
	FormatJSONL   = bibparse.FormatJSONL
	FormatRIS     = bibparse.FormatRIS
	FormatBibTeX  = bibparse.FormatBibTeX
	FormatCSLJSON = bibparse.FormatCSLJSON
	FormatNBIB    = bibparse.FormatNBIB
)

// Detect classifies batch input; see bibparse.Detect.
func Detect(path string, data []byte) Format { return bibparse.Detect(path, data) }

// Parse converts input in the given format into canonical work requests.
// FormatJSONL is not handled here — the CLI's existing JSONL reader owns it —
// and requesting it is a programming error surfaced as such.
//
// Parsing is strict on purpose: the first record that cannot become a valid
// work request aborts the whole batch, because a user who asked to acquire
// twenty papers should not silently receive nineteen. Holdings indexing wants
// the opposite tolerance and therefore calls bibparse.ParseRecords directly.
func Parse(format Format, data []byte) ([]protocol.WorkRequest, error) {
	if format == FormatJSONL {
		return nil, fmt.Errorf("jsonl input is parsed by the batch reader, not ingest")
	}
	records, err := bibparse.ParseRecords(format, data)
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
