package bibparse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const multiRecordRIS = `TY  - JOUR
TI  - First article
AU  - Smith, Ada
AU  - Jones, Ben
PY  - 2023/01/15
DO  - 10.1000/first
ER  - 

TY  - JOUR
TI  - Second article
AU  - Patel, Cora
PY  - 2024
DO  - 10.1000/second
ER  - 
`

func TestParseRIS(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    []Record
		wantErr string
	}{
		{
			name: "multiple records",
			data: multiRecordRIS,
			want: []Record{
				{DOI: "10.1000/first", Title: "First article", Authors: []string{"Smith, Ada", "Jones, Ben"}, Year: 2023},
				{DOI: "10.1000/second", Title: "Second article", Authors: []string{"Patel, Cora"}, Year: 2024},
			},
		},
		{
			name: "T1 and Y1 fallbacks",
			data: "TY  - BOOK\nT1  - Fallback title\nA1  - Doe, Jane\nY1  - published 2021-06-02\nER  - \n",
			want: []Record{{Title: "Fallback title", Authors: []string{"Doe, Jane"}, Year: 2021}},
		},
		{
			name: "DOI URL prefix",
			data: "TY  - JOUR\nTI  - DOI record\nDO  - https://doi.org/10.5555/example\nER  - \n",
			want: []Record{{DOI: "10.5555/example", Title: "DOI record"}},
		},
		{
			name: "DOI label prefix",
			data: "TY  - JOUR\nTI  - DOI record\nDO  - doi:10.5555/example\nER  - \n",
			want: []Record{{DOI: "10.5555/example", Title: "DOI record"}},
		},
		{
			name: "title continuation",
			data: "TY  - JOUR\nTI  - A title\n  continued here\nER  - \n",
			want: []Record{{Title: "A title continued here"}},
		},
		{
			name: "DOI continuation",
			data: "TY  - JOUR\nTI  - Wrapped DOI\nDO  - 10.1000/long\n  suffix\nER  - \n",
			want: []Record{{DOI: "10.1000/long suffix", Title: "Wrapped DOI"}},
		},
		{
			name: "terminator without trailing space after a DOI",
			data: "TY  - JOUR\nTI  - Stripped\nDO  - 10.1000/x\nER  -\nTY  - JOUR\nTI  - Next\nER  -",
			want: []Record{{DOI: "10.1000/x", Title: "Stripped"}, {Title: "Next"}},
		},
		{
			name: "empty tag without trailing space does not continue the previous field",
			data: "TY  - JOUR\nTI  - Title\nN1  -\n  note continuation\nAB  -\nER  - \n",
			want: []Record{{Title: "Title"}},
		},
		{
			name: "author continuation extends the last author only",
			data: "TY  - JOUR\nTI  - Wrapped author\nAU  - Smith, Ada\nAU  - Jones, Ben\n  Quincy\nER  - \n",
			want: []Record{{Title: "Wrapped author", Authors: []string{"Smith, Ada", "Jones, Ben Quincy"}}},
		},
		{
			name: "BOM and CRLF",
			data: "\ufeff\r\nTY  - JOUR\r\nTI  - Windows title\r\nAU  - Roe, Richard\r\nPY  - 2022\r\nER  - \r\n\r\n",
			want: []Record{{Title: "Windows title", Authors: []string{"Roe, Richard"}, Year: 2022}},
		},
		{
			name: "title only record",
			data: "\nTY  - JOUR\nTI  - Enough to search\nER  - \n",
			want: []Record{{Title: "Enough to search"}},
		},
		{
			name: "trailing blank lines after ER",
			data: "TY  - JOUR\nTI  - With trailing\nER  - \n\n   \n",
			want: []Record{{Title: "With trailing"}},
		},
		{
			name:    "unterminated final record",
			data:    "TY  - JOUR\nTI  - No terminator\n",
			wantErr: "unterminated",
		},
		{
			name:    "truncated single record from issue",
			data:    "TY  - JOUR\nTI  - x",
			wantErr: "ER",
		},
		{
			name:    "truncated final record after valid records",
			data:    "TY  - JOUR\nTI  - First\nER  - \nTY  - JOUR\nTI  - Truncated\n",
			wantErr: "unterminated",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRIS([]byte(test.data))
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("parseRIS() error = nil, want error containing %q (got %#v)", test.wantErr, got)
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantErr)) {
					t.Fatalf("parseRIS() error = %q, want containing %q", err.Error(), test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRIS() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("parseRIS() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestParseRISEmptySourceSentinel pins which inputs are a complete empty
// source: holdings snapshots read ErrNoEntries as "this library holds
// nothing", so non-empty input without a record must stay an ordinary error.
func TestParseRISEmptySourceSentinel(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantNoEntries bool
	}{
		{name: "blank input is a complete empty source", input: "\ufeff\r\n \r\n", wantNoEntries: true},
		{name: "text without a TY record is not an empty source", input: "@article{key, title = {BibTeX}}\n"},
		{name: "tags outside a record are not an empty source", input: "TI  - Orphan title\nDO  - 10.1000/orphan\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRIS([]byte(tt.input))
			if err == nil || err.Error() != "ris: no records found" {
				t.Fatalf("parseRIS() error = %v, want %q", err, "ris: no records found")
			}
			if got := errors.Is(err, ErrNoEntries); got != tt.wantNoEntries {
				t.Errorf("errors.Is(err, ErrNoEntries) = %t, want %t", got, tt.wantNoEntries)
			}
		})
	}
}
