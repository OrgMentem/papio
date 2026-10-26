// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Vectors derived from the instsci behavioral reference (fetcher.py DOI
// parsing, sources/arxiv.py ID rules) plus papio's stricter canonical form:
// unlike the fork, papio lowercases DOIs and trims trailing punctuation.

package work

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeDOI(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means error expected
	}{
		{" 10.1002/example  ", "10.1002/example"},
		{"10.1002/Example", "10.1002/example"},                   // canonical lowercase
		{"10.1002/example.", "10.1002/example"},                  // trailing period
		{"https://doi.org/10.1002/example).", "10.1002/example"}, // prose glue
		{"https://doi.org/10.1002/example", "10.1002/example"},
		{"http://dx.doi.org/10.1002/example", "10.1002/example"},
		{"doi:10.1021/acs.est.6c00693", "10.1021/acs.est.6c00693"},
		{"10.1016/j.watres.2024.121507", "10.1016/j.watres.2024.121507"},
		{"10.1103/PhysRevLett.128.161102", "10.1103/physrevlett.128.161102"},
		{"10.1037%2F0022-3514.57.5.830", "10.1037/0022-3514.57.5.830"}, // URL-encoded slash
		{"10.1002", ""},         // no suffix
		{"11.1002/example", ""}, // wrong directory indicator
		{"10.12/example", ""},   // registrant too short
		{"10.1002/", ""},        // empty suffix
		{"", ""},
		{"not-a-doi", ""},
	}
	for _, c := range cases {
		got, err := NormalizeDOI(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("NormalizeDOI(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeDOI(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestNormalizeArXiv(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2301.08745", "2301.08745"},
		{"2301.08745v2", "2301.08745v2"}, // explicit version preserved
		{"https://arxiv.org/abs/2301.08745v2", "2301.08745v2"},
		{"https://arxiv.org/pdf/2301.08745.pdf", "2301.08745"},
		{"arXiv:2301.08745", "2301.08745"},
		{"hep-ph/0601001", "hep-ph/0601001"},
		{"hep-ph/0601001v3", "hep-ph/0601001v3"},
		{"math.GT/0309136", "math.GT/0309136"}, // dotted old-style category
		{"2301.123", ""},                       // too few digits
		{"2301.1234567", ""},                   // too many digits
		{"HEP-PH/0601001", ""},                 // old style is lowercase
		{"", ""},
	}
	for _, c := range cases {
		got, err := NormalizeArXiv(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("NormalizeArXiv(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeArXiv(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestArXivFromDOI(t *testing.T) {
	if got := ArXivFromDOI("10.48550/arxiv.2301.08745"); got != "2301.08745" {
		t.Errorf("ArXivFromDOI = %q, want 2301.08745", got)
	}
	if got := ArXivFromDOI("10.1002/example"); got != "" {
		t.Errorf("ArXivFromDOI non-arxiv = %q, want empty", got)
	}
	// A DataCite arXiv prefix with an unusable suffix must not yield a
	// partial identifier.
	for _, doi := range []string{"10.48550/arxiv.invalid", "10.48550/arxiv.", "10.48550/arxiv.2301.874"} {
		if got := ArXivFromDOI(doi); got != "" {
			t.Errorf("ArXivFromDOI(%q) = %q, want empty", doi, got)
		}
	}
}

func TestNormalizeISBN(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means error expected
	}{
		{"hyphenated ISBN-13", "978-0-123-45678-9", "9780123456789"},
		{"spaced and padded ISBN-13", "  978 0 306 40615 7 ", "9780306406157"},
		{"bare ISBN-10", "0306406152", "0306406152"},
		{"ISBN-10 with lowercase x", "0-306-40615-x", "030640615X"},
		{"ISBN-10 with uppercase X", "0-8044-2957-X", "080442957X"},
		{"12 digits rejected", "978012345678", ""},
		{"11 digits rejected", "03064061521", ""},
		{"X before the check position rejected", "03064061X5", ""},
		{"X in an ISBN-13 rejected", "978030640615X", ""},
		{"letters rejected", "isbn-not-a-number", ""},
		{"empty rejected", "", ""},
		{"blank rejected", "  - ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeISBN(c.in)
			if c.want == "" {
				if err == nil {
					t.Fatalf("NormalizeISBN(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("NormalizeISBN(%q) = %q, %v; want %q", c.in, got, err, c.want)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		name string
		work Work
		want string
	}{
		{"DOI outranks everything", Work{DOI: "10.1000/a", ArXiv: "2301.08745", PMID: "1", OpenAlex: "W1234", ISBN: "9780306406157", Title: "T"}, "doi:10.1000/a"},
		{"arXiv outranks PMID", Work{ArXiv: "2301.08745", PMID: "1", OpenAlex: "W1234", ISBN: "9780306406157", Title: "T"}, "arxiv:2301.08745"},
		{"PMID outranks OpenAlex", Work{PMID: "1", OpenAlex: "W1234", ISBN: "9780306406157", Title: "T"}, "pmid:1"},
		{"OpenAlex outranks ISBN", Work{OpenAlex: "W1234", ISBN: "9780306406157", Title: "T"}, "openalex:W1234"},
		{"ISBN outranks title", Work{ISBN: "9780306406157", Title: "T"}, "isbn:9780306406157"},
		{"short title", Work{Title: "A short title"}, "title:A short title"},
		{"60-byte title is not truncated", Work{Title: strings.Repeat("a", 60)}, "title:" + strings.Repeat("a", 60)},
		{"61-byte title is truncated with an ellipsis", Work{Title: strings.Repeat("a", 61)}, "title:" + strings.Repeat("a", 60) + "…"},
		{"empty work", Work{}, "<empty work>"},
		{"authors and year alone are not an identity", Work{Authors: []string{"Ada"}, Year: 2024}, "<empty work>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.work.Describe(); got != c.want {
				t.Fatalf("Describe() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDescribeTruncatesOnRuneBoundary(t *testing.T) {
	// The two-byte rune starts at byte 59, so a 60-byte cut would split it.
	title := strings.Repeat("a", 59) + "é" + strings.Repeat("b", 10)
	got := Work{Title: title}.Describe()
	if !utf8.ValidString(got) {
		t.Fatalf("Describe() = %q, want valid UTF-8", got)
	}
	want := "title:" + strings.Repeat("a", 59) + "…"
	if got != want {
		t.Fatalf("Describe() = %q, want %q", got, want)
	}
}

func TestNormalizePMID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"12345", "12345"},
		{"007", "7"},            // leading zeros trimmed, still a positive id
		{"pmid:12345", "12345"}, // prefix stripped
		{" 12345 ", "12345"},    // surrounding space trimmed
		{"0", ""},               // zero is not a positive PMID -> error
		{"0000000000", ""},      // all-zero trims to empty -> error
		{"12a45", ""},           // non-digits rejected
		{"", ""},
	}
	for _, c := range cases {
		got, err := NormalizePMID(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("NormalizePMID(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizePMID(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// The canonical id form OpenAlex emits (`id`: "https://openalex.org/W…") and
// the "/works/" form its web UI and API serve are both real, and the second is
// what a user copies out of a browser address bar. Matching a bare
// "https://openalex.org/" prefix left "works/W…" behind, so every pasted web
// URL was rejected as an invalid work id.
func TestNormalizeOpenAlex(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"W3128483323", "W3128483323"},
		{" w3128483323 ", "W3128483323"}, // trimmed and uppercased
		{"openalex:W3128483323", "W3128483323"},
		{"https://openalex.org/W3128483323", "W3128483323"}, // canonical id URL
		{"https://openalex.org/works/W3128483323", "W3128483323"},
		{"http://openalex.org/works/W3128483323", "W3128483323"},
		{"https://www.openalex.org/works/W3128483323", "W3128483323"},
		{"https://api.openalex.org/works/W3128483323", "W3128483323"},
		{"http://api.openalex.org/works/W3128483323", "W3128483323"},
		{"https://openalex.org/works/w3128483323/", "W3128483323"}, // browser paste
		{"works/W3128483323", ""},                                  // bare form must stay the id itself
		{"https://openalex.org/A3128483323", ""},                   // author id, not a work
		{"W123", ""},                                               // too few digits
		{"W31284833231234", ""},                                    // too many digits
		{"https://openalex.org/works/", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, err := NormalizeOpenAlex(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("NormalizeOpenAlex(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeOpenAlex(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// ISBN satisfies HasIdentifier but no resolver consumes it, so the two
// predicates must disagree exactly there. Conflating them is what routed
// printed monographs into an institutional sign-in no login could complete.
func TestHasFetchableIdentifierExcludesISBNOnly(t *testing.T) {
	for name, test := range map[string]struct {
		w                 Work
		identifier, fetch bool
	}{
		"empty":         {Work{Title: "T"}, false, false},
		"doi":           {Work{DOI: "10.1/x"}, true, true},
		"pmid":          {Work{PMID: "1"}, true, true},
		"arxiv":         {Work{ArXiv: "2401.00001"}, true, true},
		"openalex":      {Work{OpenAlex: "W123"}, true, true},
		"isbn only":     {Work{ISBN: "9781576753484"}, true, false},
		"isbn plus doi": {Work{ISBN: "9781576753484", DOI: "10.1/x"}, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.w.HasIdentifier(); got != test.identifier {
				t.Fatalf("HasIdentifier = %t, want %t", got, test.identifier)
			}
			if got := test.w.HasFetchableIdentifier(); got != test.fetch {
				t.Fatalf("HasFetchableIdentifier = %t, want %t", got, test.fetch)
			}
		})
	}
}

func TestISBNsInReadsCataloguedValues(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"9789240114487 (electronic version)", []string{"9789240114487"}},
		{"978-92-4-011448-7 (print)", []string{"9789240114487"}},
		{"ISBN 978-92-4-011448-7", []string{"9789240114487"}},
		{"ISBN-13: 9789240114487; ISBN-10: 9240114483", []string{"9789240114487", "9240114483"}},
		{"9789240114487 9789240114494", []string{"9789240114487", "9789240114494"}},
		{"978 92 4 011448 7", []string{"9789240114487"}},
		{"0-306-40615-2", []string{"0306406152"}},
		{"080442957x", []string{"080442957X"}},
		{"9789240114487, 9789240114487", []string{"9789240114487"}},
		{"not an isbn", nil},
		{"12345", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := ISBNsIn(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ISBNsIn(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ISBNsIn(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

func TestSameISBN(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"9789240114487", "978-92-4-011448-7", true},
		{"9780306406157", "0-306-40615-2", true}, // ISBN-13 and its ISBN-10
		{"0306406152", "9780306406157", true},
		{"080442957x", "080442957X", true},
		{"9789240114487", "9789240114494", false},
		{"9780306406157", "0306406153", false}, // ISBN-10 with a wrong check digit converts to nothing
		{"9780306406158", "0306406152", false}, // ISBN-13 with a wrong check digit
		{"9789240114487", "", false},
		{"", "", false},
		{"nope", "nope", false},
	}
	for _, tc := range cases {
		if got := SameISBN(tc.a, tc.b); got != tc.want {
			t.Errorf("SameISBN(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestISBN13RejectsBadCheckDigits(t *testing.T) {
	for in, want := range map[string]string{
		"9780306406157": "9780306406157",
		"0306406152":    "9780306406157",
		"080442957X":    "9780804429573",
	} {
		if got, ok := ISBN13(in); !ok || got != want {
			t.Errorf("ISBN13(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"9780306406158", "0306406153", "123", "97803064061X7", "03064061X2"} {
		if got, ok := ISBN13(in); ok {
			t.Errorf("ISBN13(%q) = %q; a bad check digit or shape must not convert", in, got)
		}
	}
}

func TestOpenBooksEligibleIsISBNOnly(t *testing.T) {
	cases := []struct {
		name string
		w    Work
		want bool
	}{
		{"isbn only", Work{ISBN: "9789240114487"}, true},
		{"isbn with title metadata", Work{ISBN: "9789240114487", Title: "A book", Year: 2025}, true},
		{"isbn and doi (a chapter)", Work{ISBN: "9789240114487", DOI: "10.1000/x"}, false},
		{"isbn and pmid", Work{ISBN: "9789240114487", PMID: "1"}, false},
		{"isbn and arxiv", Work{ISBN: "9789240114487", ArXiv: "2301.08745"}, false},
		{"isbn and openalex", Work{ISBN: "9789240114487", OpenAlex: "W2741809807"}, false},
		{"no isbn", Work{Title: "A book"}, false},
	}
	for _, tc := range cases {
		if got := tc.w.OpenBooksEligible(); got != tc.want {
			t.Errorf("%s: OpenBooksEligible = %v, want %v", tc.name, got, tc.want)
		}
		// Eligibility must never make an ISBN a general fetchable identifier.
		if tc.w.HasFetchableIdentifier() && tc.w.DOI == "" && tc.w.PMID == "" && tc.w.ArXiv == "" && tc.w.OpenAlex == "" {
			t.Errorf("%s: ISBN became fetchable", tc.name)
		}
	}
}
