// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Vectors derived from the instsci behavioral reference (fetcher.py DOI
// parsing, sources/arxiv.py ID rules) plus papio's stricter canonical form:
// unlike the fork, papio lowercases DOIs and trims trailing punctuation.

package work

import "testing"

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

func TestFindDOIInText(t *testing.T) {
	if got := FindDOI("see https://doi.org/10.1111/dmcn.70356, cited above"); got != "10.1111/dmcn.70356" {
		t.Errorf("FindDOI prose = %q", got)
	}
	if got := FindDOI("no identifier here"); got != "" {
		t.Errorf("FindDOI none = %q", got)
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
}

func TestClassifyIdentifier(t *testing.T) {
	cases := []struct {
		in        string
		kind, val string
	}{
		{"10.1002/example", "doi", "10.1002/example"},
		{"https://doi.org/10.1002/Example.", "doi", "10.1002/example"},
		{"doi:10.48550/arXiv.2301.08745", "arxiv", "2301.08745"}, // arXiv DOI collapses to arXiv id
		{"arXiv:2301.08745v2", "arxiv", "2301.08745v2"},
		{"2301.08745", "arxiv", "2301.08745"},
		{"pmid:15676839", "pmid", "15676839"},
		{"15676839", "pmid", "15676839"},
		{"isbn:978-1-4613-3087-5", "isbn", "9781461330875"},
		{"W2036177018", "openalex", "W2036177018"},
	}
	for _, c := range cases {
		kind, val, err := ClassifyIdentifier(c.in)
		if err != nil || kind != c.kind || val != c.val {
			t.Errorf("ClassifyIdentifier(%q) = %s %q %v; want %s %q", c.in, kind, val, err, c.kind, c.val)
		}
	}
	if _, _, err := ClassifyIdentifier("gibberish!!"); err == nil {
		t.Error("ClassifyIdentifier(gibberish) succeeded, want error")
	}
}
