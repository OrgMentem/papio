package ingest

import (
	"reflect"
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
		name string
		data string
		want []Record
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
			name: "unterminated final record",
			data: "TY  - JOUR\nTI  - No terminator\n",
			want: []Record{{Title: "No terminator"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRIS([]byte(test.data))
			if err != nil {
				t.Fatalf("parseRIS() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("parseRIS() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseRISEmpty(t *testing.T) {
	_, err := parseRIS([]byte("\ufeff\r\n \r\n"))
	if err == nil {
		t.Fatal("parseRIS() error = nil, want no records error")
	}
	if err.Error() != "ris: no records found" {
		t.Errorf("parseRIS() error = %q, want %q", err, "ris: no records found")
	}
}
