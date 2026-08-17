// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"strings"
	"testing"

	"papio/internal/work"
)

func TestQualifyCandidateFullAgreementQualifies(t *testing.T) {
	w := work.Work{
		DOI:     "10.1234/abcd.1",
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
	}
	candidate := BindCandidate{
		Key:   "job-1",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Title printed as its own line, author surname present, year present,
	// and the DOI printed on page one within the 1 KiB front-matter window.
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\n" +
		"Abstract\nWe study quantum networks.\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if !got.Qualifies {
		t.Fatalf("want Qualifies true, got %+v", got)
	}
	if got.Review {
		t.Fatalf("want Review false, got %+v", got)
	}
	if got.Reason != "" {
		t.Fatalf("want empty Reason, got %q evidence %v", got.Reason, got.Evidence)
	}
}

func TestQualifyCandidateIdentifierAbsentIsReview(t *testing.T) {
	w := work.Work{
		DOI:     "10.1234/abcd.1",
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
	}
	candidate := BindCandidate{
		Key:   "job-1",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Same document as the full-agreement case but with the DOI absent.
	// The document does not conclusively name a different work (no DOI in the
	// 1 KiB window), so the veto does not block — the failure is the
	// identifier corroboration on page one.
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n\n" +
		"Abstract\nWe study quantum networks.\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false when identifier absent, got %+v", got)
	}
	if !got.Review {
		t.Fatalf("want Review true when identifier absent, got %+v", got)
	}
	if !strings.Contains(got.Reason, "identifier_not_printed_on_page_one") {
		t.Fatalf("want Reason identifier_not_printed_on_page_one, got %q", got.Reason)
	}
}

func TestQualifyCandidateZeroAuthorDisqualified(t *testing.T) {
	w := work.Work{
		Title: "Quantum Networks Robustness Calibration Measurement",
		Year:  2026,
		DOI:   "10.1234/abcd.1",
		// Authors intentionally empty.
	}
	candidate := BindCandidate{
		Key:   "job-zero",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Place the DOI past the 1 KiB front-matter window but inside the 4 KiB
	// page-one window so MatchIdentity's exact-DOI early return is not taken;
	// the document must then be judged on title/author alone, where a zero-
	// author target vacuously satisfies MatchIdentity's authorOK.
	pad := strings.Repeat("x ", 600) // ~1200 bytes, past 1 KiB
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		pad + "\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	// MatchIdentity treats a zero-author target as vacuously satisfied and
	// would pass this document on its exact printed title.
	if got := MatchIdentity(text, w); got.Result != IdentityPass {
		t.Fatalf("MatchIdentity with zero authors: want %q got %+v", IdentityPass, got)
	}
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for zero-author target, got %+v", got)
	}
	if got.Review {
		t.Fatalf("want Review false for zero-author disqualification, got %+v", got)
	}
	if !strings.Contains(got.Reason, "author_evidence_required") {
		t.Fatalf("want Reason author_evidence_required, got %q", got.Reason)
	}
}

func TestQualifyCandidateConflictingYearDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2020,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{
		Key:   "job-year",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Exact printed title and author agree, but the byline window exposes a
	// conflicting year (2021 vs requested 2020). Place the DOI past the 1 KiB
	// front-matter window so MatchIdentity's DOI early return is not taken and
	// the yearConflict defeat is exercised on the title/author path.
	pad := strings.Repeat("x ", 600) // ~1200 bytes, past 1 KiB
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2021)\n" +
		pad + "\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	// MatchIdentity's yearConflict is gated on matches < len(tokens), so an
	// exact printed title defeats it and the document still passes (or at
	// least reaches review for a different reason, never reject for year).
	if got := MatchIdentity(text, w); got.Result == IdentityReject {
		t.Fatalf("MatchIdentity with exact title but conflicting year: want pass/review, got %+v (yearConflict defeated by exact title)", got)
	}
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for conflicting byline year, got %+v", got)
	}
	if got.Review {
		t.Fatalf("want Review false for year mismatch (hard disqualification), got %+v", got)
	}
	if !strings.Contains(got.Reason, "year_mismatch") {
		t.Fatalf("want Reason year_mismatch, got %q", got.Reason)
	}
}

func TestQualifyCandidateTitleNotPrintedAsLineDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{
		Key:   "job-title",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Tokens are present but the title is not printed as a delimited line:
	// it appears quoted inside a citing sentence, which titlePrintedAsLine
	// must refuse (no label terminator, offset would exceed the 3-word cap).
	text := "We extend and update the earlier work that cites \"Quantum Networks Robustness Calibration Measurement\" for guidance.\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false when title not printed as line, got %+v", got)
	}
	if !strings.Contains(got.Reason, "title_not_printed_as_line") {
		t.Fatalf("want Reason title_not_printed_as_line, got %q", got.Reason)
	}
}

func TestQualifyCandidateVetoForeignDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{
		Key:   "job-veto",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	// Foreign DOI in the 1 KiB front-matter window vetoes, even though the
	// requested DOI is also printed on page one (past the 1 KiB window) and
	// title/authors/year all agree.
	pad := strings.Repeat("x ", 650) // ~1300 bytes, pushes the second DOI past 1 KiB
	text := "Front matter DOI: 10.9999/foreign.1\n" +
		"Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		pad + "\n" +
		"DOI: 10.1234/abcd.1 footer\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false when veto blocks, got %+v", got)
	}
	if !strings.Contains(got.Reason, "conclusive_identity_blocks") {
		t.Fatalf("want Reason conclusive_identity_blocks, got %q", got.Reason)
	}
	if !strings.Contains(got.Reason, VetoForeign) {
		t.Fatalf("want Reason to name %q, got %q evidence %v", VetoForeign, got.Reason, got.Evidence)
	}
}

func TestQualifyCandidateIdentifierPastPageOneNotQualifies(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{
		Key:   "job-past",
		Work:  w,
		Bound: []string{"10.1234/abcd.1"},
	}
	pad := strings.Repeat("x ", 2200) // ~4400 bytes, past the 4 KiB page-one cap
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n\nAbstract\n" +
		pad + "\n" +
		"DOI: 10.1234/abcd.1 appears beyond page one\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false when identifier past page one, got %+v", got)
	}
	// Absent page-one corroboration is Review, not a hard reject: the
	// metadata agrees but the document does not say so itself on page one.
	if !got.Review {
		t.Fatalf("want Review true when identifier only past page one, got %+v", got)
	}
	if !strings.Contains(got.Reason, "identifier_not_printed_on_page_one") {
		t.Fatalf("want Reason identifier_not_printed_on_page_one, got %q", got.Reason)
	}
}

func TestSelectAutoBindCandidateExactlyOneQualifies(t *testing.T) {
	qualifying := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	other1 := work.Work{
		Title:   "Attention Mechanisms for Sequence Transduction Networks",
		Authors: []string{"David Chen"},
		Year:    2019,
		DOI:     "10.9999/other.1",
	}
	other2 := work.Work{
		Title:   "Explainable Artificial Intelligence Concepts Taxonomies Opportunities",
		Authors: []string{"Siham Tabik"},
		Year:    2020,
		DOI:     "10.9999/other.2",
	}
	candidates := []BindCandidate{
		{Key: "q", Work: qualifying, Bound: []string{"10.1234/abcd.1"}},
		{Key: "o1", Work: other1, Bound: []string{"10.9999/other.1"}},
		{Key: "o2", Work: other2, Bound: []string{"10.9999/other.2"}},
	}
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	winner, ok, reason := SelectAutoBindCandidate(BindDocument{Excerpt: text}, candidates)
	if !ok {
		t.Fatalf("want ok true when exactly one qualifies, got ok false reason %q", reason)
	}
	if winner.Key != "q" {
		t.Fatalf("want winner q, got %+v reason %q", winner, reason)
	}
	if !winner.Qualifies {
		t.Fatalf("winner must Qualify, got %+v", winner)
	}
}

func TestSelectAutoBindCandidateTwoQualifyAbstains(t *testing.T) {
	// Two candidates share the same printed title/authors/DOI shape — both
	// would qualify alone, together they are ambiguous.
	w1 := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	w2 := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	other := work.Work{
		Title:   "Attention Mechanisms for Sequence Transduction Networks",
		Authors: []string{"David Chen"},
		Year:    2019,
		DOI:     "10.9999/other.1",
	}
	candidates := []BindCandidate{
		{Key: "a", Work: w1, Bound: []string{"10.1234/abcd.1"}},
		{Key: "b", Work: w2, Bound: []string{"10.1234/abcd.1"}},
		{Key: "c", Work: other, Bound: []string{"10.9999/other.1"}},
	}
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	_, ok, reason := SelectAutoBindCandidate(BindDocument{Excerpt: text}, candidates)
	if ok {
		t.Fatalf("want ok false when two qualify, got ok true")
	}
	if !strings.Contains(reason, "multiple candidates qualify") {
		t.Fatalf("want reason multiple candidates qualify, got %q", reason)
	}
}

func TestSelectAutoBindCandidateQualifierAlongsideReviewAbstains(t *testing.T) {
	qualifying := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	// Same title/authors/year but a different DOI that is NOT printed on
	// page one -> Review, not Qualifies. To avoid the conclusive-identity
	// veto (which reads only the 1 KiB blind window) interfering, the
	// qualifying DOI is placed past the 1 KiB window but still inside the
	// 4 KiB page-one window so the veto sees absent for both.
	reviewWork := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.9999/review.1",
	}
	other := work.Work{
		Title:   "Attention Mechanisms for Sequence Transduction Networks",
		Authors: []string{"David Chen"},
		Year:    2019,
		DOI:     "10.9999/other.1",
	}
	candidates := []BindCandidate{
		{Key: "q", Work: qualifying, Bound: []string{"10.1234/abcd.1"}},
		{Key: "r", Work: reviewWork, Bound: []string{"10.9999/review.1"}},
		{Key: "o", Work: other, Bound: []string{"10.9999/other.1"}},
	}
	// Front-matter (1 KiB) has no DOI so the veto is absent for both q and
	// r. Page one (4 KiB) contains q's DOI, so q qualifies while r's DOI is
	// absent and r becomes Review. The combination must abstain even though
	// exactly one qualifies.
	pad := strings.Repeat("x ", 600) // ~1200 bytes, past 1 KiB front-matter
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		pad + "\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	// Sanity: r alone would be Review.
	if got := QualifyCandidate(BindDocument{Excerpt: text}, candidates[1]); !got.Review || got.Qualifies {
		t.Fatalf("sanity: review candidate want Review true Qualifies false, got %+v", got)
	}
	_, ok, reason := SelectAutoBindCandidate(BindDocument{Excerpt: text}, candidates)
	if ok {
		t.Fatalf("want ok false when qualifier alongside review, got ok true")
	}
	if !strings.Contains(reason, "review") {
		t.Fatalf("want reason to name review ambiguity, got %q", reason)
	}
}

func TestSelectAutoBindCandidateNoneQualifiesAbstains(t *testing.T) {
	w1 := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	w2 := work.Work{
		Title:   "Attention Mechanisms for Sequence Transduction Networks",
		Authors: []string{"David Chen"},
		Year:    2019,
		DOI:     "10.9999/other.1",
	}
	candidates := []BindCandidate{
		{Key: "a", Work: w1, Bound: []string{"10.1234/abcd.1"}},
		{Key: "b", Work: w2, Bound: []string{"10.9999/other.1"}},
	}
	// Excerpt matches neither candidate's title.
	text := "An Unrelated Study of Soil Composition\nWilhelmina Farnsworth (2022)\n\nAbstract\nWe examine soil.\n"
	_, ok, reason := SelectAutoBindCandidate(BindDocument{Excerpt: text}, candidates)
	if ok {
		t.Fatalf("want ok false when none qualifies, got ok true")
	}
	if !strings.Contains(reason, "no candidate qualifies") {
		t.Fatalf("want reason no candidate qualifies, got %q", reason)
	}
}

func TestSelectAutoBindCandidateEmptyPoolAbstains(t *testing.T) {
	_, ok, reason := SelectAutoBindCandidate(BindDocument{Excerpt: "anything"}, nil)
	if ok {
		t.Fatalf("want ok false for empty pool, got ok true")
	}
	if !strings.Contains(reason, "no candidates") {
		t.Fatalf("want reason no candidates, got %q", reason)
	}
}
func TestQualifyCandidateErratumMarkerDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-erratum", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Erratum front matter that reprints the target's title, authors, year and
	// DOI verbatim — the veto cannot catch it because the DOI is the target's.
	// Without the correction guard this would qualify; with it, abstain.
	text := "Erratum: Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\nThis erratum corrects the previous paper.\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for erratum marker, got %+v", got)
	}
	if got.Review {
		t.Fatalf("want Review false for correction-marker abstention (hard fail, not Review), got %+v", got)
	}
	if !strings.Contains(got.Reason, "correction_marker:") {
		t.Fatalf("want Reason correction_marker:, got %q evidence %v", got.Reason, got.Evidence)
	}
}

func TestQualifyCandidateNonArticleMarkerDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-supp", Work: w, Bound: []string{"10.1234/abcd.1"}}
	text := "Supplementary Information\n" +
		"Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\nSupplemental data.\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for non-article marker, got %+v", got)
	}
	if !strings.Contains(got.Reason, "non_article_marker:") {
		t.Fatalf("want Reason non_article_marker:, got %q", got.Reason)
	}
}

func TestQualifyCandidateChapterFootnoteNotErratum(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-chapter", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Springer chapter footnote pointing at an erratum published elsewhere must
	// NOT be treated as the document being an erratum. The footnote sits past
	// the 1 KiB front-matter DOI window but inside the 2 KiB byline window
	// (matching the real fixture at offset ~1874), so FrontMatterDOIs does not
	// see its DOI and the veto stays compatible; correctionMarkerIn must still
	// exclude it via correctionPointerPhrases.
	pad := strings.Repeat("x ", 600) // ~1200 bytes — pushes footnote past 1 KiB front-matter window, inside 2 KiB byline
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\nWe study quantum networks.\n" +
		pad + "\n" +
		"Erratum to this chapter is available at 10.1007/978-3-030-12345-6_12\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if !got.Qualifies {
		t.Fatalf("want Qualifies true for chapter footnote (not an erratum), got %+v", got)
	}
	if got.Reason != "" {
		t.Fatalf("want empty Reason for chapter footnote, got %q", got.Reason)
	}
}

func TestQualifyCandidateYearTokenBoundary(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2019,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-year-token", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Byline contains grant "20191" which embeds 2019 as substring but not as
	// a year token, plus the real document year 2024. Candidate year 2019 must
	// DISQUALIFY (year_mismatch) — strings.Contains would have found 2019 inside
	// 20191 and incorrectly qualified.
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2024) Grant 20191\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for year token mismatch (2019 vs 2024 with grant 20191), got %+v", got)
	}
	if !strings.Contains(got.Reason, "year_mismatch:") {
		t.Fatalf("want Reason year_mismatch:, got %q evidence %v", got.Reason, got.Evidence)
	}
	// Symmetric: candidate expecting 2024 should still qualify (2024 token present).
	w2 := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2024,
		DOI:     "10.1234/abcd.1",
	}
	candidate2 := BindCandidate{Key: "job-year-token2", Work: w2, Bound: []string{"10.1234/abcd.1"}}
	got2 := QualifyCandidate(BindDocument{Excerpt: text}, candidate2)
	if !got2.Qualifies {
		t.Fatalf("want Qualifies true for matching year token 2024, got %+v", got2)
	}
}

func TestQualifyCandidateStrictPrefixTitleDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Target Title",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-prefix", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Document's real title is "Target Title: A Different Study" — candidate
	// "Target Title" is a strict prefix across a subtitle boundary. For
	// single-target verification leniency is right; for 1-of-N it must abstain.
	text := "Target Title: A Different Study\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for strict-prefix title, got %+v", got)
	}
	if !strings.Contains(got.Reason, "title_not_printed_as_line") {
		t.Fatalf("want Reason title_not_printed_as_line for strict prefix, got %q", got.Reason)
	}
	// Exact-title document must still qualify: legitimate case still passes.
	wExact := work.Work{
		Title:   "Target Title",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidateExact := BindCandidate{Key: "job-exact", Work: wExact, Bound: []string{"10.1234/abcd.1"}}
	textExact := "Target Title\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\n"
	gotExact := QualifyCandidate(BindDocument{Excerpt: textExact}, candidateExact)
	if !gotExact.Qualifies {
		t.Fatalf("want Qualifies true for exact title, got %+v", gotExact)
	}
}

func TestQualifyCandidateAuthorOnlyInTitleDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Stone Analysis of Soil Composition",
		Authors: []string{"Stone"},
		Year:    2024,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-stone", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Title contains Stone, but byline has no author line with that surname —
	// only the title, journal name and DOI. Author gate must read positionally
	// (title segments excluded, only byline between title and abstract).
	text := "Stone Analysis of Soil Composition\n" +
		"Journal of Geosciences (2024)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\nWe study soil.\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false when surname only in title, got %+v", got)
	}
	if !strings.Contains(got.Reason, "author_evidence_required") {
		t.Fatalf("want Reason author_evidence_required for title-only surname, got %q", got.Reason)
	}
	// Legitimate paper with real author line still qualifies.
	textLegit := "Stone Analysis of Soil Composition\n" +
		"Margaret Stone (2024)\n" +
		"DOI: 10.1234/abcd.1\n\nAbstract\nWe study soil.\n"
	gotLegit := QualifyCandidate(BindDocument{Excerpt: textLegit}, candidate)
	if !gotLegit.Qualifies {
		t.Fatalf("want Qualifies true for legitimate author line, got %+v", gotLegit)
	}
}

func TestQualifyCandidateRunningHeadTitleDisqualified(t *testing.T) {
	w := work.Work{
		Title:   "Quantum Networks Robustness Calibration Measurement",
		Authors: []string{"Ada Lovelace"},
		Year:    2026,
		DOI:     "10.1234/abcd.1",
	}
	candidate := BindCandidate{Key: "job-head", Work: w, Bound: []string{"10.1234/abcd.1"}}
	// Title appears both as the document's real title and again as a running
	// head glued via wide-gap (simulated with double spaces). Positional gate
	// must treat repeated identical segment as running head, not title evidence.
	text := "Quantum Networks Robustness Calibration Measurement\n" +
		"Ada Lovelace (2026)\n" +
		"DOI: 10.1234/abcd.1  Quantum Networks Robustness Calibration Measurement\n\nAbstract\n"
	got := QualifyCandidate(BindDocument{Excerpt: text}, candidate)
	if got.Qualifies {
		t.Fatalf("want Qualifies false for running-head title, got %+v", got)
	}
	if !strings.Contains(got.Reason, "title_not_printed_as_line") {
		t.Fatalf("want Reason title_not_printed_as_line for running head, got %q", got.Reason)
	}
}
