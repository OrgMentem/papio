package bibparse

import (
	"reflect"
	"strings"
	"testing"
)

const nbibFixture = `PMID- 40123456
TI  - First study title
      continued across a field line
FAU - Doe, Jane
FAU - Smith, John Q
DP  - 2024 Jan 15
LID - 10.1000/first-study [doi]

PMID- 40123457
TI  - Second study
FAU - Nguyen, Mai
DP  - 2023 Fall
LID - S1234-5678(23)00001-2 [pii]
AID - 10.1000/second-study [doi]
`

func TestParseNBIB(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []Record
	}{
		{
			name: "multi-record fixture",
			data: nbibFixture,
			want: []Record{
				{
					PMID:    "40123456",
					DOI:     "10.1000/first-study",
					Title:   "First study title continued across a field line",
					Authors: []string{"Doe, Jane", "Smith, John Q"},
					Year:    2024,
				},
				{
					PMID:    "40123457",
					DOI:     "10.1000/second-study",
					Title:   "Second study",
					Authors: []string{"Nguyen, Mai"},
					Year:    2023,
				},
			},
		},
		{
			name: "AU used without FAU",
			data: "PMID- 999\nTI  - AU only\nAU  - Public, Jane\nAU  - Reader, John\nDP  - 2022\n",
			want: []Record{{
				PMID:    "999",
				Title:   "AU only",
				Authors: []string{"Public, Jane", "Reader, John"},
				Year:    2022,
			}},
		},
		{
			name: "BOM CRLF and title only",
			data: "\ufeffTI  - A title-only request\r\n      with a continuation\r\nDP  - Published 2021\r\n",
			want: []Record{{
				Title: "A title-only request with a continuation",
				Year:  2021,
			}},
		},
		{
			name: "LID pii ignored",
			data: "PMID- 777\nTI  - No DOI\nLID - S0022-5347(20)30000-0 [pii]\n",
			want: []Record{{PMID: "777", Title: "No DOI"}},
		},
		{
			name: "leading and trailing blank lines",
			data: "\n\nPMID- 123\nTI  - Blank boundaries\n\n\n",
			want: []Record{{PMID: "123", Title: "Blank boundaries"}},
		},
		{
			name: "CRLF line endings normalized",
			data: strings.ReplaceAll(nbibFixture, "\n", "\r\n"),
			want: []Record{
				{
					PMID:    "40123456",
					DOI:     "10.1000/first-study",
					Title:   "First study title continued across a field line",
					Authors: []string{"Doe, Jane", "Smith, John Q"},
					Year:    2024,
				},
				{
					PMID:    "40123457",
					DOI:     "10.1000/second-study",
					Title:   "Second study",
					Authors: []string{"Nguyen, Mai"},
					Year:    2023,
				},
			},
		},
		{
			name: "records without a blank separator split at each PMID",
			data: "PMID- 1\nTI  - Paper A\nFAU - Alpha, A\nLID - 10.1000/a [doi]\nPMID- 2\nTI  - Paper B\nFAU - Beta, B\nLID - 10.1000/b [doi]\n",
			want: []Record{
				{PMID: "1", DOI: "10.1000/a", Title: "Paper A", Authors: []string{"Alpha, A"}},
				{PMID: "2", DOI: "10.1000/b", Title: "Paper B", Authors: []string{"Beta, B"}},
			},
		},
		{
			name: "LID wrapped before the tag",
			data: "PMID- 5\nLID - 10.1000/x\n      [doi]\nTI  - Wrapped LID\n",
			want: []Record{{PMID: "5", DOI: "10.1000/x", Title: "Wrapped LID"}},
		},
		{
			name: "FAU and AU continuations extend the last author",
			data: "PMID- 6\nFAU - Doe, Jane\nFAU - Smith, John\n      Quincy\nAU  - Doe J\n",
			want: []Record{{PMID: "6", Authors: []string{"Doe, Jane", "Smith, John Quincy"}}},
		},
		{
			name: "AU continuation without FAU",
			data: "PMID- 7\nAU  - Public, Jane\nAU  - Reader, John\n      Quincy\n",
			want: []Record{{PMID: "7", Authors: []string{"Public, Jane", "Reader, John Quincy"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNBIB([]byte(tt.data))
			if err != nil {
				t.Fatalf("parseNBIB() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNBIB() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseNBIBEmptyInput(t *testing.T) {
	for _, data := range []string{"", "\n\r\n  \n"} {
		_, err := parseNBIB([]byte(data))
		if err == nil || err.Error() != "nbib: no records found" {
			t.Errorf("parseNBIB(%q) error = %v, want nbib: no records found", data, err)
		}
	}
}

func TestParseNBIBRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"leading continuation", "      orphan\nPMID- 1\nTI  - ok\n", "continuation without a field"},
		{"continuation after record boundary", "PMID- 1\n\n      orphan\nTI  - ok\n", "continuation without a field"},
		{"line without a tag", "PMID- 1\nnot a tag line\n", "malformed line"},
		{"tag longer than four characters", "PMID- 1\nTOOLONG- x\n", "malformed line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNBIB([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseNBIB(%q) error = %v, want substring %q", tt.data, err, tt.want)
			}
		})
	}
}
