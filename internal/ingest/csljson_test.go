package ingest

import (
	"reflect"
	"strings"
	"testing"
)

const cslJSONFixture = `[
  {
    "DOI": "10.1000/first",
    "PMID": "12345678",
    "title": "First Article",
    "author": [
      {"given": "Ada", "family": "Lovelace"},
      {"given": "Grace", "family": "Hopper"}
    ],
    "issued": {"date-parts": [[2024, 3, 14]]}
  },
  {
    "DOI": "10.1000/second",
    "PMID": 87654321,
    "title": "Second Article",
    "author": [{"given": "Katherine", "family": "Johnson"}],
    "issued": {"date-parts": [[2023]]}
  }
]`

func TestParseCSLJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []Record
	}{
		{
			name: "multiple items",
			data: cslJSONFixture,
			want: []Record{
				{
					DOI:     "10.1000/first",
					PMID:    "12345678",
					Title:   "First Article",
					Authors: []string{"Ada Lovelace", "Grace Hopper"},
					Year:    2024,
				},
				{
					DOI:     "10.1000/second",
					PMID:    "87654321",
					Title:   "Second Article",
					Authors: []string{"Katherine Johnson"},
					Year:    2023,
				},
			},
		},
		{
			name: "literal author takes precedence and string year",
			data: `[{"title":"Proceedings","author":[{"literal":"The Example Consortium","given":"Ignored","family":"Name"}],"issued":{"date-parts":[["2022"]]}}]`,
			want: []Record{{
				Title:   "Proceedings",
				Authors: []string{"The Example Consortium"},
				Year:    2022,
			}},
		},
		{
			name: "doi org prefix and title only item",
			data: "\xef\xbb\xbf\r\n[\r\n  {\"DOI\": \"https://doi.org/10.5555/example\", \"title\": \"Prefixed DOI\"},\r\n  {\"title\": \"Title Only\"}\r\n]\r\n",
			want: []Record{
				{DOI: "10.5555/example", Title: "Prefixed DOI"},
				{Title: "Title Only"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCSLJSON([]byte(tt.data))
			if err != nil {
				t.Fatalf("parseCSLJSON() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCSLJSON() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseCSLJSONErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "top level object",
			data: `{"title":"not an array"}`,
			want: "csl-json: expected a top-level array of items",
		},
		{
			name: "empty array",
			data: "[]",
			want: "csl-json: no items found",
		},
		{
			name: "malformed json",
			data: "[{",
			want: "csl-json: decode:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCSLJSON([]byte(tt.data))
			if err == nil {
				t.Fatal("parseCSLJSON() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseCSLJSON() error = %q, want substring %q", err, tt.want)
			}
		})
	}
}
