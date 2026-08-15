// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package pdf

import (
	"testing"

	"papio/internal/work"
)

// PLOS front matter wraps the citation DOI mid-token, so pdftotext renders
// "https://doi.org/10.1371/journal." then "pone.0187342" on the next line. The
// truncated prefix normalizes to a syntactically valid DOI naming no work, and
// reading it as "this document is a different paper" rejected every correct
// PLOS candidate in production (job_11a276a2ea8e, four sources, all rejected
// wrong_work against the very paper they had fetched).
func TestWrappedFrontMatterDOIIsReconstructed(t *testing.T) {
	target := work.Work{
		DOI:     "10.1371/journal.pone.0187342",
		Year:    2017,
		Title:   "Motor-based bodily self is selectively impaired in eating disorders",
		Authors: []string{"Campione"},
	}
	text := "RESEARCH ARTICLE\n\nMotor-based bodily self is selectively impaired\nin eating disorders\n" +
		"Giovanna Cristina Campione\n2017\n" +
		"Citation: Campione GC (2017) ... PLoS ONE 12(11):\ne0187342. https://doi.org/10.1371/journal.\npone.0187342\n"
	got := MatchIdentity(text, target)
	if got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass on the reconstructed DOI", got)
	}
}

// Reconstruction may only rescue a match. A wrapped DOI that reassembles into
// a DIFFERENT work still refuses the candidate.
func TestWrappedFrontMatterDOIStillRefusesAnotherWork(t *testing.T) {
	target := work.Work{DOI: "10.1371/journal.pone.0187342", Title: "Motor-based bodily self", Authors: []string{"Campione"}}
	text := "RESEARCH ARTICLE\n\nSomething Else Entirely\nAda Other\n2019\n" +
		"Citation: ... https://doi.org/10.1371/journal.\npone.0999999\n"
	if got := MatchIdentity(text, target); got.Result != IdentityReject {
		t.Fatalf("result = %+v, want reject: the reassembled DOI names another work", got)
	}
}

// Reconstruction crosses exactly one line break. A blank line starts a new
// block, so fusing across it would invent a DOI from unrelated text.
func TestWrappedDOIDoesNotFuseAcrossABlankLine(t *testing.T) {
	got := FrontMatterDOIs("Citation: ... https://doi.org/10.1371/journal.\n\npone.0187342\n")
	for _, doi := range got {
		if doi == "10.1371/journal.pone.0187342" {
			t.Fatalf("FrontMatterDOIs = %v, want no DOI fused across the blank line", got)
		}
	}
}

// Blind identification must never publish a DOI known to be cut off.
func TestFrontMatterDOIsOmitsTruncatedPrefix(t *testing.T) {
	got := FrontMatterDOIs("Citation: ... https://doi.org/10.1371/journal.\npone.0187342\n")
	if len(got) != 1 || got[0] != "10.1371/journal.pone.0187342" {
		t.Fatalf("FrontMatterDOIs = %v, want only the reconstructed DOI", got)
	}
}
