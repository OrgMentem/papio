// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bibparse

import "testing"

// The eprint pair is the only arXiv source papio trusts, and BibLaTeX's eprint
// is archive-agnostic — so the archive prefix decides whether an id is an arXiv
// id at all. A wrong identifier here would match the wrong paper during
// ownership lookup and silently suppress an acquisition.
func TestParseBibTeXIdentifiers(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantArXiv string
		wantPMID  string
	}{
		{
			name:      "eprint with arxiv archiveprefix",
			input:     "@article{a,\n title = {T},\n eprint = {2401.00001},\n archiveprefix = {arXiv},\n}\n",
			wantArXiv: "2401.00001",
		},
		{
			name:      "eprint with no prefix is assumed arxiv",
			input:     "@article{a,\n title = {T},\n eprint = {2401.00001},\n}\n",
			wantArXiv: "2401.00001",
		},
		{
			name:      "archiveprefix may precede eprint",
			input:     "@article{a,\n archiveprefix = {arXiv},\n eprint = {2401.00001},\n title = {T},\n}\n",
			wantArXiv: "2401.00001",
		},
		{
			name:      "eprinttype is honoured like archiveprefix",
			input:     "@article{a,\n title = {T},\n eprint = {2401.00001},\n eprinttype = {arxiv},\n}\n",
			wantArXiv: "2401.00001",
		},
		{
			name:      "non-arxiv eprinttype must not yield an arxiv id",
			input:     "@article{a,\n title = {T},\n eprint = {12345678},\n eprinttype = {jstor},\n}\n",
			wantArXiv: "",
		},
		{
			name:      "non-arxiv archiveprefix must not yield an arxiv id",
			input:     "@article{a,\n title = {T},\n eprint = {hal-01234567},\n archiveprefix = {HAL},\n}\n",
			wantArXiv: "",
		},
		{
			name:      "explicit arxiv field wins over a foreign eprint",
			input:     "@article{a,\n title = {T},\n arxiv = {2401.00001},\n eprint = {99999},\n eprinttype = {jstor},\n}\n",
			wantArXiv: "2401.00001",
		},
		{
			name:     "pmid field",
			input:    "@article{a,\n title = {T},\n pmid = {12345678},\n}\n",
			wantPMID: "12345678",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, err := ParseRecords(FormatBibTeX, []byte(tc.input))
			if err != nil {
				t.Fatalf("ParseRecords: %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
			if records[0].ArXiv != tc.wantArXiv {
				t.Fatalf("ArXiv = %q, want %q", records[0].ArXiv, tc.wantArXiv)
			}
			if records[0].PMID != tc.wantPMID {
				t.Fatalf("PMID = %q, want %q", records[0].PMID, tc.wantPMID)
			}
		})
	}
}

// ParseRecords is the holdings entry point: an entry with no usable identifier
// is an ordinary library record (a book, a hand-typed note), not an error. Only
// the caller knows whether that is a defect.
func TestParseRecordsKeepsIdentifierlessEntries(t *testing.T) {
	input := "@book{b,\n title = {Some Book},\n author = {Author, A},\n year = {2020},\n}\n"
	records, err := ParseRecords(FormatBibTeX, []byte(input))
	if err != nil {
		t.Fatalf("ParseRecords must not reject an identifier-less record: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].HasIdentifier() {
		t.Fatal("record unexpectedly reports an identifier")
	}
	if records[0].Title != "Some Book" {
		t.Fatalf("title = %q", records[0].Title)
	}
}

func TestParseRecordsStructuralErrorIsReported(t *testing.T) {
	if _, err := ParseRecords(FormatCSLJSON, []byte("not json")); err == nil {
		t.Fatal("malformed CSL-JSON must be a structural error")
	}
	if _, err := ParseRecords(FormatJSONL, []byte("{}")); err == nil {
		t.Fatal("jsonl belongs to the batch reader and must be refused here")
	}
	if _, err := ParseRecords(Format("toml"), nil); err == nil {
		t.Fatal("an unsupported format must be refused")
	}
}

func TestHasIdentifier(t *testing.T) {
	cases := []struct {
		name   string
		record Record
		want   bool
	}{
		{"doi", Record{DOI: "10.1/a"}, true},
		{"pmid", Record{PMID: "1"}, true},
		{"arxiv", Record{ArXiv: "2401.1"}, true},
		{"isbn alone is not matchable", Record{ISBN: "9780262035613"}, false},
		{"title alone is not matchable", Record{Title: "T"}, false},
		{"whitespace only", Record{DOI: "  "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.record.HasIdentifier(); got != tc.want {
				t.Fatalf("HasIdentifier() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		path string
		data string
		want Format
	}{
		{"bib extension", "refs.bib", "", FormatBibTeX},
		{"ris extension", "refs.ris", "", FormatRIS},
		{"nbib extension", "refs.nbib", "", FormatNBIB},
		{"json array is csl", "refs.json", "[{}]", FormatCSLJSON},
		{"json object is jsonl", "refs.json", "{}", FormatJSONL},
		{"jsonl extension", "refs.jsonl", "", FormatJSONL},
		{"sniff bibtex", "-", "@article{a,", FormatBibTeX},
		{"sniff ris", "-", "TY  - JOUR", FormatRIS},
		{"sniff nbib", "-", "PMID- 1", FormatNBIB},
		{"sniff csl", "-", "  [ {} ]", FormatCSLJSON},
		{"empty defaults to jsonl", "-", "", FormatJSONL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.path, []byte(tc.data)); got != tc.want {
				t.Fatalf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}
