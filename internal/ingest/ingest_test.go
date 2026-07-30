// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ingest

import (
	"strings"
	"testing"
)

// Acquisition parsing is strict where holdings parsing is tolerant: a user who
// asked to acquire several papers must not silently receive fewer. This guards
// the asymmetry that justifies the bibparse/ingest split.
func TestParseAbortsOnAnIdentifierlessRecord(t *testing.T) {
	input := "@article{a,\n title = {Has A DOI},\n doi = {10.1000/one},\n}\n\n@book{b,\n title = {No Identifier},\n}\n"
	_, err := Parse(FormatBibTeX, []byte(input))
	if err == nil {
		t.Fatal("a record that cannot become a work request must abort the batch")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error must name the offending record, got %v", err)
	}
}

func TestParseConvertsIdentifiers(t *testing.T) {
	input := "@article{a,\n title = {A Real Title},\n doi = {https://doi.org/10.1000/One},\n eprint = {2401.00001},\n archiveprefix = {arXiv},\n pmid = {12345678},\n year = {2024},\n}\n"
	requests, err := Parse(FormatBibTeX, []byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	ids := requests[0].Identifiers
	if ids == nil {
		t.Fatal("identifiers not carried through conversion")
	}
	if ids.DOI == "" {
		t.Fatalf("DOI missing: %+v", ids)
	}
	if ids.ArXiv != "2401.00001" {
		t.Fatalf("ArXiv = %q, want 2401.00001", ids.ArXiv)
	}
	if ids.PMID != "12345678" {
		t.Fatalf("PMID = %q, want 12345678", ids.PMID)
	}
}

func TestParseRefusesJSONL(t *testing.T) {
	_, err := Parse(FormatJSONL, []byte(`{"doi":"10.1/a"}`))
	if err == nil {
		t.Fatal("jsonl is the batch reader's format and must be refused here")
	}
	if got, want := err.Error(), "jsonl input is parsed by the batch reader, not ingest"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// Detect is re-exported so callers keep one import after the split.
func TestDetectIsReExported(t *testing.T) {
	if got := Detect("refs.ris", nil); got != FormatRIS {
		t.Fatalf("Detect = %q, want %q", got, FormatRIS)
	}
}
