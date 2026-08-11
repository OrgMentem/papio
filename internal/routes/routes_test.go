// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package routes

import (
	"net/url"
	"strings"
	"testing"
)

func TestRouteTableValidates(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatalf("compiled route table is invalid: %v", err)
	}
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
	if len(withPII) != 3 {
		t.Fatalf("with PII got %d candidates, want 3", len(withPII))
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
