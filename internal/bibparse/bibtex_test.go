package bibparse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const bibTeXFixture = `@article{garcia2024,
  title = {An {\"O}verview of {\'e}vidence---a study},
  author = {Garc{\'i}a, Jos{\'e} and Research {and} Development, Ada and Smith, John},
  doi = {https://doi.org/10.1000/example.1},
  year = {2024},
}

@inproceedings{doe2023,
  title = "Quoted   title -- result",
  author = "Doe, Jane AND Roe, Richard",
  year = "Published 2023",
}
`

func TestParseBibTeX(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Record
		wantErr string
	}{
		{
			name:  "parses braced and quoted entries",
			input: "\ufeff" + strings.ReplaceAll(bibTeXFixture, "\n", "\r\n"),
			want: []Record{
				{
					DOI:     "10.1000/example.1",
					Title:   "An Overview of evidence-a study",
					Authors: []string{"Garcia, Jose", "Research and Development, Ada", "Smith, John"},
					Year:    2024,
				},
				{
					Title:   "Quoted title - result",
					Authors: []string{"Doe, Jane", "Roe, Richard"},
					Year:    2023,
				},
			},
		},
		{
			name: "accepts title-only entry",
			input: `@misc{untitled,
  title = {A title without an identifier},
}`,
			want: []Record{{Title: "A title without an identifier"}},
		},
		{
			name: "skips BibTeX meta entries",
			input: `@comment{A comment with {nested braces}}
@string{journal = "Ignored Journal"}
@preamble{"Ignored preamble"}
@book{kept,
  title = {Kept record},
  doi = doi:10.5555/kept,
}`,
			want: []Record{{DOI: "10.5555/kept", Title: "Kept record"}},
		},
		{
			name: "accepts punctuated citation key and nested values",
			input: `@article{smith:2024/alpha,
  doi = {10.1000/x=y, nested {value}},
  title = "Quoted = value, with {braces}",
}`,
			want: []Record{{
				DOI:   "10.1000/x=y, nested value",
				Title: "Quoted = value, with braces",
			}},
		},
		{
			name:    "rejects field assignment before citation-key comma",
			input:   "@article{key doi={10.1000/x}, title={T}}",
			wantErr: "bibtex: entry at byte 0: field assignment before citation key comma",
		},
		{
			name:    "rejects empty input",
			input:   "\ufeff\r\n \t\r\n",
			wantErr: "bibtex: no entries found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBibTeX([]byte(tt.input))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("parseBibTeX() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBibTeX() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseBibTeX() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseBibTeXEmptySourceSentinel(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantErr       string
		wantNoEntries bool
	}{
		{
			name:          "whitespace is a complete empty source",
			input:         "\ufeff\r\n \t\r\n",
			wantErr:       "bibtex: no entries found",
			wantNoEntries: true,
		},
		{
			name:          "malformed entry is not an empty source",
			input:         "@article(a, title = {Not BibTeX})",
			wantErr:       "bibtex: entry at byte 0: expected opening brace",
			wantNoEntries: false,
		},
		{
			name:          "entry without a citation-key comma is not an empty source",
			input:         "@article{key}",
			wantErr:       "bibtex: entry at byte 0: missing comma after citation key",
			wantNoEntries: false,
		},
		{
			name:          "citation key followed only by whitespace is not an empty source",
			input:         "@article{key  }",
			wantErr:       "bibtex: entry at byte 0: missing comma after citation key",
			wantNoEntries: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseBibTeX([]byte(tt.input))
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("parseBibTeX() error = %v, want %q", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrNoEntries); got != tt.wantNoEntries {
				t.Errorf("errors.Is(err, ErrNoEntries) = %t, want %t", got, tt.wantNoEntries)
			}
		})
	}
}

func TestParseBibTeXEprintArchiveMetadata(t *testing.T) {
	tests := []struct {
		name   string
		fields string
		want   string
	}{
		{
			name: "bare eprint is assumed arxiv",
			want: "2401.00001",
		},
		{
			name: "archiveprefix jstor then eprinttype arxiv",
			fields: `
  archiveprefix = {JSTOR},
  eprinttype = {arXiv},`,
		},
		{
			name: "archiveprefix arxiv then eprinttype jstor",
			fields: `
  archiveprefix = {arXiv},
  eprinttype = {JSTOR},`,
		},
		{
			name: "archiveprefix then eprinttype both arxiv",
			fields: `
  archiveprefix = {arXiv},
  eprinttype = {arXiv},`,
			want: "2401.00001",
		},
		{
			name: "eprinttype then archiveprefix both arxiv",
			fields: `
  eprinttype = {arXiv},
  archiveprefix = {arXiv},`,
			want: "2401.00001",
		},
		{
			name: "archiveprefix jstor then arxiv remains foreign",
			fields: `
  archiveprefix = {JSTOR},
  archiveprefix = {arXiv},`,
		},
		{
			name: "archiveprefix arxiv then jstor remains foreign",
			fields: `
  archiveprefix = {arXiv},
  archiveprefix = {JSTOR},`,
		},
		{
			name: "eprinttype jstor then arxiv remains foreign",
			fields: `
  eprinttype = {JSTOR},
  eprinttype = {arXiv},`,
		},
		{
			name: "eprinttype arxiv then jstor remains foreign",
			fields: `
  eprinttype = {arXiv},
  eprinttype = {JSTOR},`,
		},
		{
			name: "repeated archiveprefix arxiv is accepted",
			fields: `
  archiveprefix = {arXiv},
  archiveprefix = {arXiv},`,
			want: "2401.00001",
		},
		{
			name: "repeated eprinttype arxiv is accepted",
			fields: `
  eprinttype = {arXiv},
  eprinttype = {arXiv},`,
			want: "2401.00001",
		},
		{
			name: "hal archiveprefix is not arxiv",
			fields: `
  archiveprefix = {HAL},`,
		},
		{
			name: "pubmed eprinttype is not arxiv",
			fields: `
  eprinttype = {PubMed},`,
		},
		{
			name: "jstor eprinttype is not arxiv",
			fields: `
  eprinttype = {JSTOR},`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "@article{a,\n  eprint = {2401.00001}," + tt.fields + "\n}"
			records, err := parseBibTeX([]byte(input))
			if err != nil {
				t.Fatalf("parseBibTeX() error = %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("len(records) = %d, want 1", len(records))
			}
			if records[0].ArXiv != tt.want {
				t.Errorf("ArXiv = %q, want %q", records[0].ArXiv, tt.want)
			}
		})
	}
}
