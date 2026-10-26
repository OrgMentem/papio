// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package bibparse

import "testing"

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
		{"jsonl takes precedence over later markers", "-", "{\n@article{not a record}\n}", FormatJSONL},
		{"csl takes precedence over later markers", "-", "[\nTY  - not a record\n]", FormatCSLJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.path, []byte(tc.data)); got != tc.want {
				t.Fatalf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAndParseExportHeaders(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		data   string
		format Format
	}{
		{"bibtex stdin", "-", "% Encoding: UTF-8\n\n@article{a, doi={10.1000/header}}", FormatBibTeX},
		{"bibtex command", "", "% Encoding: UTF-8\r\n@article{a, doi={10.1000/header}}", FormatBibTeX},
		{"ris text file", "refs.txt", "Provider: JSTOR\nDatabase: JSTOR\n\nTY  - JOUR\nDO  - 10.1000/header\nER  -\n", FormatRIS},
		{"ris command", "", "Provider: JSTOR\r\nTY  - JOUR\r\nDO  - 10.1000/header\r\nER  -\r\n", FormatRIS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(tt.data)
			format := Detect(tt.path, data)
			if format != tt.format {
				t.Fatalf("Detect = %q, want %q", format, tt.format)
			}
			records, err := ParseRecords(format, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].DOI != "10.1000/header" {
				t.Fatalf("records = %+v, want the exported DOI", records)
			}
		})
	}
}
