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

// Reconstruction may only CONFIRM the requested DOI. A reassembled DOI is a
// guess about typesetting, so it must never be the thing that refuses a
// candidate: here the front matter reassembles into a different DOI while the
// title and author corroborate the target, and the wrong-work verdict must not
// come from the fused identifier.
func TestReconstructedDOINeverRefusesACandidate(t *testing.T) {
	target := work.Work{
		DOI:     "10.1371/journal.pone.0187342",
		Year:    2017,
		Title:   "Motor-based bodily self is selectively impaired in eating disorders",
		Authors: []string{"Campione"},
	}
	text := "RESEARCH ARTICLE\n\nMotor-based bodily self is selectively impaired\nin eating disorders\n" +
		"Giovanna Cristina Campione\n2017\n" +
		"Citation: Campione GC (2017) ... https://doi.org/10.1371/journal.\npone.0999999\n"
	if got := MatchIdentity(text, target); got.Result == IdentityReject {
		t.Fatalf("result = %+v, want no reject: a reassembled DOI is not refusal evidence", got)
	}
}

// A DOI that is simply the last thing on its line is COMPLETE. Fusing it with
// the first word of the following prose line invents an identifier no document
// contains — which is exactly what the PDF-grab sweep then filed a captured
// file under ("10.1234/grab.test" + "a" = "10.1234/grab.testa").
func TestCompleteDOIAtLineEndDoesNotAbsorbTheNextLine(t *testing.T) {
	got := FrontMatterDOIs("Grab Fixture\nDOI: 10.1234/grab.test\nabstract follows here\n")
	if len(got) != 1 || got[0] != "10.1234/grab.test" {
		t.Fatalf("FrontMatterDOIs = %v, want exactly the printed DOI", got)
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

// Blind identification must publish only what the document actually printed:
// neither the truncated prefix (a valid-looking DOI naming no work) nor the
// reconstruction (papio's own guess) may name a captured file.
func TestFrontMatterDOIsOmitsBothHalvesOfAWrappedDOI(t *testing.T) {
	if got := FrontMatterDOIs("Citation: ... https://doi.org/10.1371/journal.\npone.0187342\n"); len(got) != 0 {
		t.Fatalf("FrontMatterDOIs = %v, want nothing publishable from a wrapped DOI", got)
	}
}
