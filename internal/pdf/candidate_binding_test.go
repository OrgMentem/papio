// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"strings"
	"testing"

	"papio/internal/work"
)

func TestCheckConclusiveIdentityAbsentWhenNoDOI(t *testing.T) {
	text := "This document has no identifier in its front matter.\nJust prose and a title.\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/example"}); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/example"}); got.Blocks() {
		t.Fatalf("VetoAbsent must not block")
	}
	if got := CheckConclusiveIdentity(text, nil); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q", VetoAbsent, got.Verdict)
	}
}

func TestCheckConclusiveIdentityCompatible(t *testing.T) {
	text := "Front matter DOI: 10.1234/compatible-work is printed at the top.\n"
	bound := []string{"10.1234/compatible-work"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); got.Blocks() {
		t.Fatalf("VetoCompatible must not block")
	}
}

func TestCheckConclusiveIdentitySlashRunDifferenceParks(t *testing.T) {
	// 10.48612//monograph-2025-2 and 10.48612/monograph-2025-2 are two
	// separately registered DataCite works with different titles by the same
	// creators — internal/ownership's TestNormalizeIdentifier pins the pair, and
	// a tree-wide slash collapse was tried and reverted (5d1adce). Legacy APA
	// PDFs equally really do print 10.1037//0021-9010.87.4.611 for the work
	// Crossref names with one slash. Both facts are true, so the veto may
	// neither collapse the pair to compatible — that files one DataCite work
	// under the other's citation, the cardinal failure — nor call it foreign,
	// which would refuse the APA reprint the operator asked for by name.
	// Nothing lexical separates the two cases, so the only honest verdict is
	// that papio does not know: ambiguous, blocking, parked for a human.
	const doubled = "DOI: 10.48612//monograph-2025-2 appears in the front matter.\n"
	const single = "DOI: 10.48612/monograph-2025-2 appears in the front matter.\n"

	for _, tc := range []struct {
		name    string
		text    string
		bound   []string
		wantDOI string
	}{
		{"document doubled, job single", doubled, []string{"10.48612/monograph-2025-2"}, "10.48612//monograph-2025-2"},
		{"document single, job doubled", single, []string{"10.48612//monograph-2025-2"}, "10.48612/monograph-2025-2"},
		{"document doubled, job holds an unrelated DOI too", doubled,
			[]string{"10.9999/unrelated", "10.48612/monograph-2025-2"}, "10.48612//monograph-2025-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckConclusiveIdentity(tc.text, tc.bound)
			if got.Verdict != VetoAmbiguous {
				t.Fatalf("want %q got %q evidence %v dois %v", VetoAmbiguous, got.Verdict, got.Evidence, got.DOIs)
			}
			if !got.Blocks() {
				t.Fatalf("a slash-run difference must block and park, got %+v", got)
			}
			// The verbatim spelling must reach the human deciding the park:
			// which of the two names the document printed is the whole question.
			if len(got.DOIs) != 1 || got.DOIs[0] != tc.wantDOI {
				t.Fatalf("want document DOI %q verbatim, got %v", tc.wantDOI, got.DOIs)
			}
			if !strings.Contains(strings.Join(got.Evidence, " | "), "slash run") {
				t.Fatalf("evidence must name the slash run as the cause, got %v", got.Evidence)
			}
		})
	}

	// The same spelling on both sides is still an exact match, not a near one.
	if got := CheckConclusiveIdentity(doubled, []string{"10.48612//monograph-2025-2"}); got.Verdict != VetoCompatible {
		t.Fatalf("want %q for identical doubled spellings, got %q evidence %v", VetoCompatible, got.Verdict, got.Evidence)
	}
}

func TestCheckConclusiveIdentityBothSlashSpellingsPrintedIsAmbiguous(t *testing.T) {
	// A document printing both names prints two names. Deduplicating the
	// collapsed values made that look like a single identifier, which turned the
	// clearest possible conflict into a compatible verdict.
	text := "DOIs: 10.48612/monograph-2025-2 and 10.48612//monograph-2025-2 are both printed here.\n"
	// Sorted verbatim: '/' (0x2f) sorts before 'm', so the doubled form leads.
	want := []string{"10.48612//monograph-2025-2", "10.48612/monograph-2025-2"}
	for _, bound := range [][]string{
		{"10.48612/monograph-2025-2"},
		{"10.48612//monograph-2025-2"},
		nil,
	} {
		got := CheckConclusiveIdentity(text, bound)
		if got.Verdict != VetoAmbiguous {
			t.Fatalf("bound %v: want %q got %q evidence %v", bound, VetoAmbiguous, got.Verdict, got.Evidence)
		}
		if !got.Blocks() {
			t.Fatalf("bound %v: both spellings printed must block, got %+v", bound, got)
		}
		if len(got.DOIs) != len(want) || got.DOIs[0] != want[0] || got.DOIs[1] != want[1] {
			t.Fatalf("bound %v: want both spellings verbatim %v, got %v", bound, want, got.DOIs)
		}
	}
}

func TestCheckConclusiveIdentityAmbiguous(t *testing.T) {
	text := "DOIs in front matter: 10.1234/first and 10.1234/second both printed.\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); got.Verdict != VetoAmbiguous {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAmbiguous, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); !got.Blocks() {
		t.Fatalf("VetoAmbiguous must block")
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/first"}); len(got.DOIs) != 2 {
		t.Fatalf("want 2 DOIs got %v", got.DOIs)
	}
}

func TestCheckConclusiveIdentityForeignWithEmptyBound(t *testing.T) {
	// A PMID-only or arXiv-only job has no DOI recorded at all. A
	// conclusive front-matter DOI that names a work the job is not bound
	// to must be VetoForeign and must block, never VetoAbsent.
	text := "Front matter DOI: 10.1234/only-doi printed.\n"
	if got := CheckConclusiveIdentity(text, nil); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q evidence %v", VetoForeign, got.Verdict, got.Evidence)
	}
	if got := CheckConclusiveIdentity(text, nil); !got.Blocks() {
		t.Fatalf("VetoForeign must block")
	}
	if got := CheckConclusiveIdentity(text, []string{}); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q", VetoForeign, got.Verdict)
	}
}

func TestCheckConclusiveIdentityForeignDifferentWork(t *testing.T) {
	text := "DOI: 10.1234/foreign-work is in the front matter.\n"
	bound := []string{"10.1234/bound-work"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoForeign {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoForeign, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); !got.Blocks() {
		t.Fatalf("VetoForeign must block")
	}
}

func TestCheckConclusiveIdentityIgnoresDOIPastFrontMatterWindow(t *testing.T) {
	// The veto is deliberately narrow: it reads only the 1 KiB front-matter
	// window (identityFrontMatterBytes), never whole-document or
	// identityPageOne (4 KiB). A DOI that appears only past that window
	// must not be treated as a conclusive naming of the document, so the
	// verdict stays VetoAbsent. This is a separately gated decision — widening
	// the window is out of scope here and would reintroduce bibliography
	// false positives.
	filler := strings.Repeat("x ", 600) // > 1200 bytes, pushes the DOI past 1 KiB
	text := filler + "\n10.1234/past-window\n"
	if got := CheckConclusiveIdentity(text, []string{"10.1234/past-window"}); got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, []string{"10.1234/past-window"}); got.Blocks() {
		t.Fatalf("VetoAbsent must not block even when the DOI beyond the window matches bound")
	}
}

func TestCheckConclusiveIdentityBoundSetIgnoresJunkEntries(t *testing.T) {
	text := "DOI: 10.1234/good is in front matter.\n"
	bound := []string{"", "   ", "not-a-doi", "10.1234/good", "not-a-doi/again"}
	if got := CheckConclusiveIdentity(text, bound); got.Verdict != VetoCompatible {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoCompatible, got.Verdict, got.Evidence, got.DOIs)
	}
	if got := CheckConclusiveIdentity(text, bound); got.Blocks() {
		t.Fatalf("VetoCompatible must not block even with junk in bound set")
	}
}

func TestCheckConclusiveIdentityBlindPastWindowWhileCorroborationSees(t *testing.T) {
	// The composite, not the ingredients: the document's OWN DOI sits past the
	// 1 KiB blind window, so the veto reports absent, while the CANDIDATE's DOI
	// appears later on page one in relational prose ("Extended from ..."), so
	// target-aware corroboration succeeds over the whole document. Nothing in
	// the veto contradicts that hit, and a corroboration hit is not a
	// self-identification — a 2024 journal expansion citing the conference
	// paper's DOI satisfies both halves at once.
	//
	// This test asserts the gap rather than a fix. The predicate that consumed
	// it is disabled this round and the /2 self-identifier rule owns the repair;
	// pinning both halves here keeps the exposure measured instead of assumed
	// closed, and will fail loudly if either window silently moves.
	filler := strings.Repeat("x ", 600) // > 1200 bytes: nothing below reaches the blind window
	text := filler + "\nExtended from DOI 10.1234/candidate-work.\nJournal of Later Expansions\nDOI 10.5555/document-own-work\n"

	got := CheckConclusiveIdentity(text, []string{"10.1234/candidate-work"})
	if got.Verdict != VetoAbsent {
		t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
	}
	if got.Blocks() {
		t.Fatalf("VetoAbsent must not block")
	}
	if len(got.DOIs) != 0 {
		t.Fatalf("the veto must see no DOI at all past the blind window, got %v", got.DOIs)
	}
	// The document's own foreign DOI is equally invisible to the veto, which is
	// the half that makes the pair dangerous.
	if !IdentifierPrinted(text, work.Work{DOI: "10.1234/candidate-work"}) {
		t.Fatalf("corroboration must find the candidate DOI outside the blind window")
	}
	if !IdentifierPrinted(text, work.Work{DOI: "10.5555/document-own-work"}) {
		t.Fatalf("corroboration reads the whole document, so the own DOI is findable there too")
	}
}

func TestCheckConclusiveIdentityForeignArXivWithEmptyDOISet(t *testing.T) {
	// The chosen contract, stated in FrontMatterDOIs: DOI is the sole BLIND
	// identifier class; arXiv and PMID are target-aware only. A document
	// conclusively naming a foreign arXiv or PubMed record therefore yields the
	// empty set and VetoAbsent. That is a decision, not an omission, and this
	// test pins both the decision and the residual it leaves.
	//
	// Admitting the classes here would not add negative evidence, it would
	// fabricate it: the bound side (job.BoundDOIs) is DOI-typed, so the accepted
	// manuscript of the very article a job names — an arXiv stamp down the
	// margin, no DOI anywhere — would read as naming a work the job is not bound
	// to and park, and an arXiv-submitted job's own PDF would park against its
	// empty bound set. Closing the residual needs a typed identifier set on both
	// sides, which is the /2 design.
	arxivStamp := "arXiv:9999.99999v2 [cs.LG] 3 Feb 2026\n\nSome Unrelated Preprint Title\nAn Author\n"
	pmidStamp := "PMID: 31234567  PMCID: PMC1234567\n\nA Different Article Entirely\nAnother Author\n"
	for _, tc := range []struct{ name, text string }{
		{"foreign arXiv id, no DOI printed", arxivStamp},
		{"foreign PMID, no DOI printed", pmidStamp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckConclusiveIdentity(tc.text, []string{"10.1234/bound-work"})
			if got.Verdict != VetoAbsent {
				t.Fatalf("want %q got %q evidence %v dois %v", VetoAbsent, got.Verdict, got.Evidence, got.DOIs)
			}
			if got.Blocks() {
				t.Fatalf("the DOI-only blind contract cannot block on an arXiv or PMID stamp, got %+v", got)
			}
			if len(got.DOIs) != 0 {
				t.Fatalf("want the empty conclusive set, got %v", got.DOIs)
			}
		})
	}

	// Target-aware is where the two classes do their work: the request attests
	// the identifier, so a document printing THAT one corroborates and one
	// printing only another does not.
	arxivTarget := work.Work{ArXiv: "2401.12345"}
	if IdentifierPrinted(arxivStamp, arxivTarget) {
		t.Fatalf("a foreign arXiv stamp must not corroborate the requested id")
	}
	if !IdentifierPrinted("arXiv:2401.12345v3 [cs.LG] 1 Jan 2024\n", arxivTarget) {
		t.Fatalf("the requested arXiv id printed on the page must corroborate")
	}
	pmidTarget := work.Work{PMID: "31234567"}
	if !IdentifierPrinted(pmidStamp, pmidTarget) {
		t.Fatalf("the requested PMID printed on the page must corroborate")
	}
	if IdentifierPrinted(pmidStamp, work.Work{PMID: "3123456"}) {
		t.Fatalf("a PMID prefix must not corroborate a longer printed PMID")
	}
}

func TestConclusiveVetoAndMatchIdentityDisagreeOnSlashRunsByDesign(t *testing.T) {
	// The asymmetry, executable. One relation (collapseRegistrantSlashRun), two
	// opposite conclusions, because the two callers answer different questions.
	//
	// MatchIdentity validates against a SINGLE ATTESTED TARGET: the operator
	// already named the work, so reading a printed doubled slash as the
	// requested single one accepts the legacy APA reprint they asked for, and
	// the residual error is one document against one attested target.
	legacyAPA := work.Work{DOI: "10.1037/0021-9010.87.4.611", Title: "Legacy APA Reprint"}
	printed := "Copyright line DOI: 10.1037//0021-9010.87.4.611\n"
	if got := MatchIdentity(printed, legacyAPA); got.Result != IdentityPass {
		t.Fatalf("attested single target may be lenient: want pass, got %+v", got)
	}
	// And the reverse direction, which the collapse-by-default normalizer used
	// to reach only by accident.
	reversed := work.Work{DOI: "10.1037//0021-9010.87.4.611", Title: "Legacy APA Reprint"}
	if got := MatchIdentity("Copyright line DOI: 10.1037/0021-9010.87.4.611\n", reversed); got.Result != IdentityPass {
		t.Fatalf("leniency must be symmetric for an attested target: got %+v", got)
	}

	// The veto answers "is this a FOREIGN work?" without an attestation to lean
	// on, and the same collapse cannot license that inference in either
	// direction, so it parks instead.
	if got := CheckConclusiveIdentity(printed, []string{"10.1037/0021-9010.87.4.611"}); got.Verdict != VetoAmbiguous {
		t.Fatalf("the veto must park the identical pair MatchIdentity passes: want %q got %q evidence %v",
			VetoAmbiguous, got.Verdict, got.Evidence)
	}
}

func TestNormalizeDOIPreservesSlashRuns(t *testing.T) {
	// normalizeDOI is the exact relation: it canonicalizes prefixes, case and
	// trailing punctuation, and it decides nothing about slash runs.
	for _, tc := range []struct{ in, want string }{
		{"10.48612//monograph-2025-2", "10.48612//monograph-2025-2"},
		{"https://doi.org/10.48612//monograph-2025-2", "10.48612//monograph-2025-2"},
		{"DOI: 10.1037//0021-9010.87.4.611", "10.1037//0021-9010.87.4.611"},
		{"10.48612/monograph-2025-2", "10.48612/monograph-2025-2"},
		{"not-a-doi", ""},
	} {
		if got := normalizeDOI(tc.in); got != tc.want {
			t.Fatalf("normalizeDOI(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
	// The leniency is the separately named relation, and it folds a RUN, not
	// just one extra slash — a caller asking for it asks for all of it.
	for _, tc := range []struct{ in, want string }{
		{"10.48612//monograph-2025-2", "10.48612/monograph-2025-2"},
		{"10.48612///monograph-2025-2", "10.48612/monograph-2025-2"},
		{"10.48612/monograph-2025-2", "10.48612/monograph-2025-2"},
		{"10.48612/sub/path", "10.48612/sub/path"},
	} {
		if got := collapseRegistrantSlashRun(tc.in); got != tc.want {
			t.Fatalf("collapseRegistrantSlashRun(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}
