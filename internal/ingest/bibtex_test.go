package ingest

import (
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
  author = "Doe, Jane and Roe, Richard",
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
