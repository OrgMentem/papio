// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package resolver

import (
	"math"
	"testing"
)

func TestRankIsDeterministicAndPolicyOrdered(t *testing.T) {
	cands := []Candidate{
		{Source: "core", URL: "https://c/1", Version: VersionPublished, AccessBasis: AccessLicensedAPI, ReuseLicense: "unknown", IdentityConfidence: 0.95},
		{Source: "unpaywall", URL: "https://u/1", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "cc-by", IdentityConfidence: 0.95, Direct: true},
		{Source: "arxiv", URL: "https://a/1", Version: VersionPreprint, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.95, Direct: true},
		{Source: "openalex", URL: "https://o/1", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.4},
	}
	ranked, evidence := Rank("any", cands)
	if len(ranked) != 4 || len(evidence) != 4 {
		t.Fatalf("rank returned %d/%d entries", len(ranked), len(evidence))
	}
	// OA published direct with explicit license beats everything.
	if ranked[0].Source != "unpaywall" {
		t.Fatalf("first = %s, want unpaywall", ranked[0].Source)
	}
	// OA preprint beats licensed-API published under access-basis ordering.
	if ranked[1].Source != "arxiv" || ranked[2].Source != "core" {
		t.Fatalf("order = %s, %s; want arxiv then core", ranked[1].Source, ranked[2].Source)
	}
	// Low identity confidence sinks to the bottom regardless of access.
	if ranked[3].Source != "openalex" {
		t.Fatalf("last = %s, want low-confidence openalex", ranked[3].Source)
	}

	// Determinism: same input, same order.
	again, _ := Rank("any", cands)
	for i := range ranked {
		if again[i].Source != ranked[i].Source {
			t.Fatalf("rank not deterministic at %d: %s vs %s", i, again[i].Source, ranked[i].Source)
		}
	}
}

func TestRankHonorsDesiredVersion(t *testing.T) {
	cands := []Candidate{
		{Source: "unpaywall", URL: "https://u/pub", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.95},
		{Source: "arxiv", URL: "https://a/pre", Version: VersionPreprint, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.95},
	}
	ranked, _ := Rank(VersionPreprint, cands)
	if ranked[0].Version != VersionPreprint {
		t.Fatalf("desired preprint but first = %s", ranked[0].Version)
	}
	ranked, _ = Rank("any", cands)
	if ranked[0].Version != VersionPublished {
		t.Fatalf("desired any but first = %s, want published preference", ranked[0].Version)
	}
}

func TestValidateCandidateFailsClosed(t *testing.T) {
	good := Candidate{Source: "unpaywall", URL: "https://x/y.pdf", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.9}
	if err := ValidateCandidate(good); err != nil {
		t.Fatalf("good candidate rejected: %v", err)
	}
	bad := []Candidate{
		{URL: "https://x", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown"},            // no source
		{Source: "s", URL: "ftp://x", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown"}, // scheme
		{Source: "s", URL: "https://x", Version: "final", AccessBasis: AccessOpen, ReuseLicense: "unknown"},        // version enum
		{Source: "s", URL: "https://x", Version: VersionPublished, AccessBasis: "free", ReuseLicense: "unknown"},   // access enum
		{Source: "s", URL: "https://x", Version: VersionPublished, AccessBasis: AccessOpen},                        // license required
		{Source: "s", URL: "https://x", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 1.5},
	}
	for i, c := range bad {
		if err := ValidateCandidate(c); err == nil {
			t.Errorf("bad candidate %d accepted", i)
		}
	}
}

// The old prefix check accepted any string starting with "https://", so
// "https://" itself and "https:///relative" crossed the fail-closed source
// boundary the function documents as requiring an absolute http(s) URL.
func TestValidateCandidateRequiresAbsoluteURL(t *testing.T) {
	base := func(rawURL string) Candidate {
		return Candidate{Source: "unpaywall", URL: rawURL, Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.9}
	}
	rejected := []string{
		"https://",
		"http://",
		"https:///relative",
		"/relative/path",
		"example.com/paper.pdf",
		"https://host with spaces/paper.pdf",
		"",
	}
	for _, rawURL := range rejected {
		if err := ValidateCandidate(base(rawURL)); err == nil {
			t.Errorf("URL %q accepted", rawURL)
		}
	}
	accepted := []string{
		"https://onlinelibrary.wiley.com/doi/pdfdirect/10.1002/example?download=true",
		"https://proxy.example.edu:8443/openurl/handler",
		"http://export.arxiv.org/pdf/2101.00001v2",
	}
	for _, rawURL := range accepted {
		if err := ValidateCandidate(base(rawURL)); err != nil {
			t.Errorf("URL %q rejected: %v", rawURL, err)
		}
	}
}

func TestValidateCandidateRejectsNonFinite(t *testing.T) {
	base := func() Candidate {
		return Candidate{Source: "unpaywall", URL: "https://x/y.pdf", Version: VersionPublished, AccessBasis: AccessOpen, ReuseLicense: "unknown", IdentityConfidence: 0.5, CostUSD: 0}
	}
	cases := []struct {
		name    string
		mutate  func(Candidate) Candidate
		wantErr bool
	}{
		{name: "NaN confidence", mutate: func(c Candidate) Candidate { c.IdentityConfidence = math.NaN(); return c }, wantErr: true},
		{name: "+Inf confidence", mutate: func(c Candidate) Candidate { c.IdentityConfidence = math.Inf(1); return c }, wantErr: true},
		{name: "-Inf confidence", mutate: func(c Candidate) Candidate { c.IdentityConfidence = math.Inf(-1); return c }, wantErr: true},
		{name: "NaN cost", mutate: func(c Candidate) Candidate { c.CostUSD = math.NaN(); return c }, wantErr: true},
		{name: "+Inf cost", mutate: func(c Candidate) Candidate { c.CostUSD = math.Inf(1); return c }, wantErr: true},
		{name: "-Inf cost", mutate: func(c Candidate) Candidate { c.CostUSD = math.Inf(-1); return c }, wantErr: true},
		{name: "confidence 0", mutate: func(c Candidate) Candidate { c.IdentityConfidence = 0; return c }, wantErr: false},
		{name: "confidence 1", mutate: func(c Candidate) Candidate { c.IdentityConfidence = 1; return c }, wantErr: false},
		{name: "cost 0", mutate: func(c Candidate) Candidate { c.CostUSD = 0; return c }, wantErr: false},
	}
	for _, tc := range cases {
		c := tc.mutate(base())
		err := ValidateCandidate(c)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
	}
}

func TestCandidateKeyStableAndURLFree(t *testing.T) {
	c := Candidate{Source: "unpaywall", URL: "https://host/path?signature=SECRET"}
	k1, k2 := c.Key(), c.Key()
	if k1 != k2 || len(k1) != 32 {
		t.Fatalf("key unstable or wrong length: %q %q", k1, k2)
	}
	if k1 == c.URL || len(k1) > 32 {
		t.Fatal("key must be a hash, never the URL")
	}
}
