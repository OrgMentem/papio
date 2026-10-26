// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package routes

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestRouteTableValidates(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatalf("compiled route table is invalid: %v", err)
	}
}

// Validate is the release guard for the compiled-in table: an edited seed row
// must fail it, and the failure must name the row so the author can find it.
// Each case starts from a good row and breaks exactly one thing. The table is
// package state, so these cases run serially and restore it.
func TestValidateRejectsMalformedSeedRows(t *testing.T) {
	good := func(identifier string) routeTemplate {
		for _, route := range routeTable {
			if route.identifier == identifier && route.query != "pii={pii}" {
				return route
			}
		}
		t.Fatalf("no shipped %s route", identifier)
		return routeTemplate{}
	}
	goodDOI, goodPII := good(identifierDOI), good(identifierPII)
	var goodQueryPII routeTemplate
	for _, route := range routeTable {
		if route.query == "pii={pii}" {
			goodQueryPII = route
		}
	}
	if goodQueryPII.routeRevision == "" {
		t.Fatal("no shipped route with a pii query slot")
	}

	tests := []struct {
		name   string
		rows   func() []routeTemplate
		wantIn string
	}{
		{"revision without a slash", func() []routeTemplate {
			r := goodDOI
			r.routeRevision = "wiley-doi-pdfdirect"
			return []routeTemplate{r}
		}, "wiley-doi-pdfdirect"},
		{"duplicate revision", func() []routeTemplate { return []routeTemplate{goodDOI, goodDOI} }, goodDOI.routeRevision},
		{"revision names another provider", func() []routeTemplate {
			r := goodDOI
			r.provider = "other-provider"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"revision with extra segments", func() []routeTemplate {
			r := goodDOI
			r.routeRevision = r.provider + "/1/2"
			return []routeTemplate{r}
		}, goodDOI.provider + "/1/2"},
		{"revision number not numeric", func() []routeTemplate {
			r := goodDOI
			r.routeRevision = r.provider + "/x"
			return []routeTemplate{r}
		}, goodDOI.provider + "/x"},
		{"revision number zero", func() []routeTemplate {
			r := goodDOI
			r.routeRevision = r.provider + "/0"
			return []routeTemplate{r}
		}, goodDOI.provider + "/0"},
		{"missing origin", func() []routeTemplate {
			r := goodDOI
			r.origin = ""
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"missing identifier kind", func() []routeTemplate {
			r := goodDOI
			r.identifier = ""
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"missing path family", func() []routeTemplate {
			r := goodDOI
			r.pathFamily = ""
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"http origin", func() []routeTemplate {
			r := goodDOI
			r.origin = "http://onlinelibrary.wiley.com"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"origin with a path", func() []routeTemplate {
			r := goodDOI
			r.origin += "/doi"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"origin with credentials", func() []routeTemplate {
			r := goodDOI
			r.origin = "https://user@onlinelibrary.wiley.com"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"origin without a host", func() []routeTemplate {
			r := goodDOI
			r.origin = "https://"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"two identifier slots", func() []routeTemplate {
			r := goodDOI
			r.pathFamily += "/{doi}"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"no identifier slot", func() []routeTemplate {
			r := goodDOI
			r.pathFamily = r.pathPrefix
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"stray brace beside the slot", func() []routeTemplate {
			r := goodDOI
			r.pathFamily += "{x}"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"unsupported query on a path slot", func() []routeTemplate {
			r := goodDOI
			r.query = "download=false"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"query slot on a DOI route", func() []routeTemplate {
			r := goodDOI
			r.pathFamily, r.pathPrefix, r.pathSuffix = "/doi/pdf", "/doi/pdf", ""
			r.query = "doi={doi}"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"unsupported query slot spelling", func() []routeTemplate {
			r := goodQueryPII
			r.query = "id={pii}"
			return []routeTemplate{r}
		}, goodQueryPII.routeRevision},
		{"query-slot path family differs from its compiler", func() []routeTemplate {
			r := goodQueryPII
			r.pathFamily = "/action/other"
			return []routeTemplate{r}
		}, goodQueryPII.routeRevision},
		{"path family differs from prefix, slot, and suffix", func() []routeTemplate {
			r := goodPII
			r.pathSuffix = "/other"
			return []routeTemplate{r}
		}, goodPII.routeRevision},
		{"path prefix without a leading slash", func() []routeTemplate {
			r := goodDOI
			r.pathPrefix = "doi/pdfdirect/"
			r.pathFamily = "doi/pdfdirect/{doi}"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"path suffix with a query mark", func() []routeTemplate {
			r := goodDOI
			r.pathSuffix = "?x"
			r.pathFamily = r.pathPrefix + "{doi}?x"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
		{"identifier kind with no expansion", func() []routeTemplate {
			r := goodDOI
			r.identifier = "isbn"
			r.pathFamily = r.pathPrefix + "{isbn}"
			return []routeTemplate{r}
		}, goodDOI.routeRevision},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			saved := routeTable
			t.Cleanup(func() { routeTable = saved })
			routeTable = tc.rows()
			err := Validate()
			if err == nil {
				t.Fatal("Validate accepted a malformed seed row")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("Validate error %q does not name the row %q", err, tc.wantIn)
			}
		})
	}

	t.Run("empty table", func(t *testing.T) {
		saved := routeTable
		t.Cleanup(func() { routeTable = saved })
		routeTable = nil
		if err := Validate(); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("Validate on an empty table = %v, want an empty-table error", err)
		}
	})
}

func TestCandidatesForDOIEncodingMatrix(t *testing.T) {
	tests := []struct {
		name       string
		doi        string
		wantPath   string
		wantID     string
		wantRoutes int
	}{
		{
			name:       "plain DOI",
			doi:        "10.48612/monograph-2025-2",
			wantPath:   "/doi/pdfdirect/10.48612/monograph-2025-2",
			wantID:     "doi:10.48612/monograph-2025-2",
			wantRoutes: 2,
		},
		{
			name:       "repeated slash remains distinct",
			doi:        "10.48612//monograph-2025-2",
			wantPath:   "/doi/pdfdirect/10.48612//monograph-2025-2",
			wantID:     "doi:10.48612//monograph-2025-2",
			wantRoutes: 2,
		},
		{
			name:       "dot segments are escaped",
			doi:        "10.48612/../monograph-2025-2",
			wantPath:   "/doi/pdfdirect/10.48612/%2E%2E/monograph-2025-2",
			wantID:     "doi:10.48612/../monograph-2025-2",
			wantRoutes: 2,
		},
		{
			name:       "unicode and reserved bytes",
			doi:        "10.48612/über+paper%edition",
			wantPath:   "/doi/pdfdirect/10.48612/%C3%BCber%2Bpaper%25edition",
			wantID:     "doi:10.48612/über+paper%edition",
			wantRoutes: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidates := CandidatesFor(tt.doi, "")
			if len(candidates) != tt.wantRoutes {
				t.Fatalf("candidate count = %d, want %d", len(candidates), tt.wantRoutes)
			}
			for _, candidate := range candidates {
				if err := ValidateCandidate(candidate); err != nil {
					t.Fatalf("compiler emitted candidate rejected by validator: %+v: %v", candidate, err)
				}
			}
			if candidates[0].Identifier != tt.wantID {
				t.Fatalf("identifier = %q, want %q", candidates[0].Identifier, tt.wantID)
			}
			u, err := url.Parse(candidates[0].URL)
			if err != nil {
				t.Fatal(err)
			}
			if u.EscapedPath() != tt.wantPath {
				t.Fatalf("escaped path = %q, want %q", u.EscapedPath(), tt.wantPath)
			}
			if u.RawQuery != "download=true" || len(u.Query()) != 1 {
				t.Fatalf("query = %q, want exactly one download=true", u.RawQuery)
			}
		})
	}
}

func TestCandidatesForRejectsInjectionAndUnroundTrippableIdentifiers(t *testing.T) {
	for _, doi := range []string{
		"10.48612/has whitespace",
		"10.48612/has\ttab",
		"10.48612/has\nnewline",
		"10.48612/fragment#not-path",
		"10.48612/query?download=false",
		"10.48612/back\\slash",
		"10.48612/user@example",
	} {
		t.Run(strings.ReplaceAll(doi, "/", "_"), func(t *testing.T) {
			if got := CandidatesFor(doi, ""); len(got) != 0 {
				t.Fatalf("got %d candidates for unsafe DOI %q", len(got), doi)
			}
		})
	}
}

func TestCandidatesForProviderHints(t *testing.T) {
	if got := CandidatesFor("10.48612/example", "sage-doi-pdf"); len(got) != 1 || got[0].RouteRevision != "sage-doi-pdf/1" {
		t.Fatalf("provider-family hint = %+v", got)
	}
	if got := CandidatesFor("10.48612/example", "wiley-doi-pdfdirect/1"); len(got) != 1 || got[0].RouteRevision != "wiley-doi-pdfdirect/1" {
		t.Fatalf("revision hint = %+v", got)
	}
	if got := CandidatesFor("10.48612/example", "unknown-provider"); len(got) != 0 {
		t.Fatalf("unknown provider hint returned %+v", got)
	}
}

func TestCandidatesForIdentifiersRequiresPIIForScienceDirect(t *testing.T) {
	withoutPII := CandidatesForIdentifiers(map[string]string{"doi": "10.48612/example"}, "")
	if len(withoutPII) != 2 {
		t.Fatalf("without PII got %d candidates, want 2", len(withoutPII))
	}
	withPII := CandidatesForIdentifiers(map[string]string{
		"doi": "10.48612/example",
		"pii": "S1234567890123456",
	}, "")
	if len(withPII) != 4 {
		t.Fatalf("with PII got %d candidates, want 4", len(withPII))
	}
	got := withPII[2]
	if got.RouteRevision != "sciencedirect-pii-pdfft/1" || got.Identifier != "pii:S1234567890123456" {
		t.Fatalf("ScienceDirect candidate = %+v", got)
	}
	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://www.sciencedirect.com/science/article/pii/S1234567890123456/pdfft" {
		t.Fatalf("ScienceDirect URL = %q", u.String())
	}
	if u.RawQuery != "" {
		t.Fatalf("ScienceDirect query = %q, want empty", u.RawQuery)
	}
}

func TestCandidatesForIdentifiersEscapesPIIAndHonorsHint(t *testing.T) {
	got := CandidatesForIdentifiers(map[string]string{"pii": "S123/../über"}, "sciencedirect-pii-pdfft")
	if len(got) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(got))
	}
	u, err := url.Parse(got[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/science/article/pii/S123/%2E%2E/%C3%BCber/pdfft"
	if u.EscapedPath() != wantPath {
		t.Fatalf("escaped PII path = %q, want %q", u.EscapedPath(), wantPath)
	}
}

func TestCandidatesForIdentifiersCellPII(t *testing.T) {
	pii := "S240584401730308X"
	got := CandidatesForIdentifiers(map[string]string{"pii": pii}, "")
	var cell *Candidate
	for i := range got {
		if got[i].RouteRevision == "cell-pii-showpdf/1" {
			cell = &got[i]
			break
		}
	}
	if cell == nil {
		t.Fatalf("cell candidate missing in %+v", got)
	}
	wantURL := "https://www.cell.com/action/showPdf?pii=" + pii
	if cell.URL != wantURL {
		t.Fatalf("cell URL = %q, want %q", cell.URL, wantURL)
	}
	if err := ValidateCandidate(*cell); err != nil {
		t.Fatalf("cell candidate failed ValidateCandidate: %v", err)
	}
	u, err := url.Parse(cell.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.EscapedPath() != "/action/showPdf" {
		t.Fatalf("cell path = %q, want %q", u.EscapedPath(), "/action/showPdf")
	}
	if u.RawQuery != "pii="+pii {
		t.Fatalf("cell query = %q, want %q", u.RawQuery, "pii="+pii)
	}
	if cell.AllowedOrigin != "https://www.cell.com" || cell.PathFamily != "/action/showPdf" || cell.Identifier != "pii:"+pii {
		t.Fatalf("cell candidate fields = %+v", *cell)
	}
	// DOI-only CandidatesFor must not emit the cell route.
	for _, c := range CandidatesFor("10.1234/example", "") {
		if c.RouteRevision == "cell-pii-showpdf/1" {
			t.Fatalf("DOI-only CandidatesFor emitted cell route: %+v", c)
		}
	}
	if got := CandidatesForIdentifiers(map[string]string{"doi": "10.1234/example"}, ""); len(got) != 2 {
		t.Fatalf("DOI-only CandidatesForIdentifiers got %d, want 2 (wiley+sage)", len(got))
	}
	for _, c := range CandidatesForIdentifiers(map[string]string{"doi": "10.1234/example"}, "") {
		if c.RouteRevision == "cell-pii-showpdf/1" {
			t.Fatalf("DOI-only CandidatesForIdentifiers emitted cell route: %+v", c)
		}
	}
	// Malformed PII must not emit the cell route.
	for _, bad := range []string{"S123 #bad", "S123?bad", "S123\\bad", "S123@bad", "S123 bad", "S123#bad"} {
		if got := CandidatesForIdentifiers(map[string]string{"pii": bad}, ""); len(got) != 0 {
			// wiley/sage require DOI, so zero is expected; any cell emission is failure
			for _, c := range got {
				if c.RouteRevision == "cell-pii-showpdf/1" {
					t.Fatalf("malformed PII %q emitted cell route", bad)
				}
			}
		}
		if got := CandidatesForIdentifiers(map[string]string{"pii": bad}, "cell-pii-showpdf"); len(got) != 0 {
			t.Fatalf("malformed PII %q with hint emitted %d candidates, want 0", bad, len(got))
		}
	}
	// Existing routes unchanged: wiley and sage still emit with DOI, sciencedirect still requires PII.
	doiCandidates := CandidatesFor("10.48612/example", "")
	if len(doiCandidates) != 2 {
		t.Fatalf("DOI candidates = %d, want 2", len(doiCandidates))
	}
	wantRevisions := map[string]bool{"wiley-doi-pdfdirect/1": true, "sage-doi-pdf/1": true}
	for _, c := range doiCandidates {
		if !wantRevisions[c.RouteRevision] {
			t.Fatalf("unexpected DOI revision %q", c.RouteRevision)
		}
	}
	// Cell hint isolates the route.
	hinted := CandidatesForIdentifiers(map[string]string{"pii": pii}, "cell-pii-showpdf/1")
	if len(hinted) != 1 || hinted[0].RouteRevision != "cell-pii-showpdf/1" {
		t.Fatalf("cell hint = %+v", hinted)
	}
}

func TestValidateCandidateRejectsMalformedPlaceholders(t *testing.T) {
	candidates := CandidatesFor("10.48612/example", "")
	if len(candidates) == 0 {
		t.Fatal("expected a compiled candidate")
	}
	for _, tt := range []struct {
		name       string
		pathFamily string
	}{
		{name: "extra braces", pathFamily: "/doi/pdfdirect/{doi}{extra}"},
		{name: "mismatched placeholder", pathFamily: "/doi/pdfdirect/{pii}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidate := candidates[0]
			candidate.PathFamily = tt.pathFamily
			if err := ValidateCandidate(candidate); err == nil {
				t.Fatalf("ValidateCandidate accepted malformed path family %q", tt.pathFamily)
			}
		})
	}
}

func TestValidateCandidateRejectsAdversarialManualCandidates(t *testing.T) {
	candidates := CandidatesFor("10.48612/../über+paper%edition", "wiley-doi-pdfdirect")
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	base := candidates[0]
	for _, tt := range []struct {
		name string
		edit func(Candidate) Candidate
	}{
		{
			name: "unknown revision",
			edit: func(candidate Candidate) Candidate {
				candidate.RouteRevision = "operator-url/1"
				return candidate
			},
		},
		{
			name: "foreign origin",
			edit: func(candidate Candidate) Candidate {
				candidate.AllowedOrigin = "https://evil.example"
				return candidate
			},
		},
		{
			name: "unescaped dot segment",
			edit: func(candidate Candidate) Candidate {
				candidate.URL = strings.Replace(candidate.URL, "%2E%2E", "..", 1)
				return candidate
			},
		},
		{
			name: "lowercase percent encoding",
			edit: func(candidate Candidate) Candidate {
				candidate.URL = strings.Replace(candidate.URL, "%C3%BC", "%c3%bc", 1)
				return candidate
			},
		},
		{
			name: "duplicated query",
			edit: func(candidate Candidate) Candidate {
				candidate.URL += "&download=true"
				return candidate
			},
		},
		{
			name: "fragment",
			edit: func(candidate Candidate) Candidate {
				candidate.URL += "#download"
				return candidate
			},
		},
		{
			name: "wrong identifier kind",
			edit: func(candidate Candidate) Candidate {
				candidate.Identifier = "pii:S1234567890123456"
				return candidate
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCandidate(tt.edit(base)); err == nil {
				t.Fatal("ValidateCandidate accepted a malformed manual candidate")
			}
		})
	}
}

// The query-route branch (Cell pii={pii}) needs its own adversarial rows:
// the path-placeholder cases above never enter it, so disabling its path,
// query and canonical-URL checks used to pass the suite while letting a Cell
// candidate carry a foreign host, an outside path or extra query data.
func TestValidateCandidateRejectsCellQueryDeviations(t *testing.T) {
	cands := CandidatesForIdentifiers(map[string]string{"pii": "S240584401730308X"}, "")
	var cell *Candidate
	for i := range cands {
		if cands[i].RouteRevision == "cell-pii-showpdf/1" {
			cell = &cands[i]
			break
		}
	}
	if cell == nil {
		t.Fatalf("cell candidate missing in %+v", cands)
	}
	base := *cell
	if err := ValidateCandidate(base); err != nil {
		t.Fatalf("unedited Cell candidate rejected: %v", err)
	}
	for _, tt := range []struct {
		name string
		edit func(Candidate) Candidate
	}{
		{
			name: "foreign host keeps path and query",
			edit: func(c Candidate) Candidate {
				c.URL = strings.Replace(c.URL, "https://www.cell.com/", "https://evil.example/", 1)
				return c
			},
		},
		{
			name: "path outside the family",
			edit: func(c Candidate) Candidate {
				c.URL = strings.Replace(c.URL, "/action/showPdf?", "/action/download?", 1)
				return c
			},
		},
		{
			name: "extra query parameter",
			edit: func(c Candidate) Candidate {
				c.URL += "&download=true"
				return c
			},
		},
		{
			name: "duplicated pii",
			edit: func(c Candidate) Candidate {
				c.URL += "&pii=S240584401730308X"
				return c
			},
		},
		{
			name: "non-canonical identifier case",
			edit: func(c Candidate) Candidate {
				c.URL = strings.Replace(c.URL, "S240584401730308X", "s240584401730308x", 1)
				return c
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCandidate(tt.edit(base)); err == nil {
				t.Fatal("ValidateCandidate accepted a malformed Cell query candidate")
			}
		})
	}
}

func TestPublisherFirstAdapterMatchesOnlyTheRegistrantPrefix(t *testing.T) {
	for _, tc := range []struct {
		doi, adapter string
	}{
		{"10.1176/appi.ajp.2010.09111680", "psychiatryonline"},
		{"https://doi.org/10.1176/appi.ajp.2010.09111680", "psychiatryonline"},
		{"10.1021/acs.jcim.6c00481", "acs"},
		{"10.1126/science.adz4433", "science"},
		{"10.11260/science.adz4433", ""},
		{"10.11760/appi.ajp.2010.09111680", ""},
		{"10.1177/0018720814547570", ""},
		{"10.1002/example", ""},
		{"10.1176", ""},
		{"", ""},
	} {
		got, ok := PublisherFirstAdapter(tc.doi, false)
		if got != tc.adapter || ok != (tc.adapter != "") {
			t.Errorf("PublisherFirstAdapter(%q) = %q, %v; want %q", tc.doi, got, ok, tc.adapter)
		}
	}
}

// With an OpenAthens domain the packaged-adapter prefixes join the DOI-first
// set; without one they stay with the institutional resolver. A prefix with no
// packaged adapter (10.1287 INFORMS, 10.1007 Springer's measured plain-DOI
// answer) is never offered first, because the one DOI attempt would be wasted.
func TestPublisherFirstAdapterOpenAthensAddsPackagedAdapterPrefixes(t *testing.T) {
	for _, tc := range []struct {
		doi, adapter string
	}{
		{"10.1016/j.jval.2022.04.1227", "sciencedirect"},
		{"10.1080/0144929x.2019.1578828", "tandfonline"},
		{"10.1177/0018720814547570", "sage"},
		{"10.1037/xge0001234", "psycnet"},
		{"10.1002/example", "wiley"},
		{"10.1111/example", "wiley"},
		{"10.1093/example", "oup"},
		{"10.1017/example", "cambridge"},
		{"10.1038/example", "nature"},
		{"https://doi.org/10.1016/j.jval.2022.04.1227", "sciencedirect"},
		{"10.1176/appi.ajp.2010.09111680", "psychiatryonline"},
		{"10.1021/acs.jcim.6c00481", "acs"},
		{"10.1287/isre.2024.0857", ""},
		{"10.1007/s11121-020-01111-w", ""},
		{"10.10160/example", ""},
		{"10.1016", ""},
		{"", ""},
	} {
		got, ok := PublisherFirstAdapter(tc.doi, true)
		if got != tc.adapter || ok != (tc.adapter != "") {
			t.Errorf("PublisherFirstAdapter(%q, true) = %q, %v; want %q", tc.doi, got, ok, tc.adapter)
		}
		// Without a domain only the three unconditional prefixes match.
		want := ""
		switch tc.adapter {
		case "psychiatryonline", "acs", "science":
			want = tc.adapter
		}
		got, ok = PublisherFirstAdapter(tc.doi, false)
		if got != want || ok != (want != "") {
			t.Errorf("PublisherFirstAdapter(%q, false) = %q, %v; want %q", tc.doi, got, ok, want)
		}
	}
}

// Every adapter a DOI-first prefix names must exist in the packaged extension
// registry: a typo would offer the DOI first to a page no adapter drives.
func TestPublisherFirstTablesNameRealExtensionAdapters(t *testing.T) {
	raw, err := os.ReadFile("../../extension/src/adapters/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+id: "([a-z0-9-]+)",$`).FindAllStringSubmatch(string(raw), -1) {
		known[m[1]] = true
	}
	if !known["acs"] || !known["nature"] {
		t.Fatalf("adapter registry scan found %d ids and missed known ones; the pattern is stale", len(known))
	}
	for _, table := range [][]struct{ doiPrefix, adapterID string }{publisherFirstTable, openAthensFirstTable} {
		for _, row := range table {
			if !known[row.adapterID] {
				t.Errorf("prefix %s names adapter %q, which the extension does not package", row.doiPrefix, row.adapterID)
			}
		}
	}
}
