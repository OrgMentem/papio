package bibparse

import (
	"errors"
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
			name: "EDTF date alongside date parts",
			data: `[{"DOI":"10.1000/first","issued":"2020-05-01"},{"DOI":"10.1000/second","issued":{"date-parts":[[2023]]}}]`,
			want: []Record{
				{DOI: "10.1000/first", Year: 2020},
				{DOI: "10.1000/second", Year: 2023},
			},
		},
		{
			name: "uncertain and interval EDTF dates",
			data: `[{"issued":"2020?"},{"issued":"2021/2022"},{"issued":"20XX"},{"issued":"unknown"}]`,
			want: []Record{{Year: 2020}, {Year: 2021}, {}, {}},
		},
		{
			name: "unsupported date shapes keep records",
			data: `[{"DOI":"10.1000/a","issued":true},{"DOI":"10.1000/b","issued":{"date-parts":false}},{"DOI":"10.1000/c","issued":null}]`,
			want: []Record{{DOI: "10.1000/a"}, {DOI: "10.1000/b"}, {DOI: "10.1000/c"}},
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
		name          string
		data          string
		want          string
		wantNoEntries bool
	}{
		{
			name: "top level object",
			data: `{"title":"not an array"}`,
			want: "csl-json: expected a top-level array of items",
		},
		{
			name: "null is not an array",
			data: "null",
			want: "csl-json: expected a top-level array of items",
		},
		{
			name: "scalar is not an array",
			data: "true",
			want: "csl-json: expected a top-level array of items",
		},
		{
			name:          "empty array",
			data:          "[]",
			want:          "csl-json: no items found",
			wantNoEntries: true,
		},
		{
			name: "null array item is structural error",
			data: "[null]",
			want: "csl-json: item 0: expected an object",
		},
		{
			name: "scalar array item is structural error",
			data: `[1]`,
			want: "csl-json: item 0: expected an object",
		},
		{
			name: "mixed array is structural error",
			data: `[{"title":"valid"}, null]`,
			want: "csl-json: item 1: expected an object",
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
			if got := errors.Is(err, ErrNoEntries); got != tt.wantNoEntries {
				t.Errorf("errors.Is(err, ErrNoEntries) = %t, want %t", got, tt.wantNoEntries)
			}
		})
	}
}
