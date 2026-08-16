// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"fmt"
	"strings"

	"papio/internal/work"
)

// Candidate auto-bind is a 1-of-N selection, not a 1-of-1 verification.
//
// MatchIdentity answers "is this document the requested work?" for a single
// attested target. A wrong pass parks one job; the operator corrects it and
// no other job is harmed. A candidate bind answers "which of N live jobs, if
// any, does this DOI-less grab belong to?" without a human in the loop. A
// wrong accept there files the wrong PDF under the right citation and
// prospers silently — the cost is a corrupted library entry, not a park.
// The 1-of-N shape multiplies the prior over accepting wrong, because every
// non-target job is a distinct way to be wrong.
//
// That risk profile forbids the shortcuts MatchIdentity can afford for a
// single target: treating a zero-author request as vacuously satisfied,
// passing on a corroborating identifier before the title is shown to be
// printed as a line, and treating year disagreement as evidence only when the
// title match is already partial. Each shortcut saves correct documents at the
// cost of admitting a measured family of wrong ones; the corpus behind
// identity.go measured those families (9.9% wrong accepts from byline
// relaxation, 52 wrong accepts from 398,786 mismatched pairs through the
// unordered token gate, preprint year mismatches on exact titles). For a
// single attested target a park can absorb the residual; for autonomous
// selection among N candidates the same residual is an auto-filed wrong PDF.
// This ruleset therefore removes every shortcut and requires all hard gates
// simultaneously, even at the cost of lower auto-bind coverage — abstention
// is the safe default. Any change to a bound or threshold is a new rule
// version that must rerun the selection gate before it may auto-bind
// anything.
const CandidateBindingRule = "candidate_auto_bind/1"

// BindCandidate is one job inbound bytes might belong to.
type BindCandidate struct {
	Key   string // caller's opaque key, in practice a job id
	Work  work.Work
	Bound []string // DOIs durably bound to that job
}

// CandidateQualification explains one candidate's verdict.
type CandidateQualification struct {
	Key       string
	Qualifies bool     // safe to bind without asking a human
	Review    bool     // suggestive but insufficient: park for confirmation
	Reason    string   // why not, when Qualifies is false
	Evidence  []string // human-readable evidence for audit and review
}

// QualifyCandidate applies the candidate-binding acceptance rule to one candidate.
//
// The rule is deliberately stricter than MatchIdentity. Each numbered gate
// below carries a WHY comment recording the divergence and the measured risk
// it closes. All gates must pass for Qualifies to be true. Absent identifier
// corroboration on page one is the one gate that yields Review instead of a
// hard disqualification, because metadata agreement without a document-printed
// identifier is suggestive but not self-asserting.
func QualifyCandidate(excerpt string, candidate BindCandidate) CandidateQualification {
	q := CandidateQualification{Key: candidate.Key}

	// 1. Conclusive-identity veto must not block.
	//
	// WHY stricter than nothing: this veto is shared with ordinary validation
	// (validateCandidate) and applies even to DOI-less jobs whose own target
	// has no DOI. MatchIdentity's DOI gate is target-gated (wantDOI != ""), so
	// a DOI-less job would never see a DOI mismatch. The veto closes that
	// gap: a document that conclusively names a different work in its 1 KiB
	// blind front-matter window must not be auto-bound on title/author
	// similarity alone. CheckConclusiveIdentity is reused single-sourced so
	// the window definition never diverges.
	veto := CheckConclusiveIdentity(excerpt, candidate.Bound)
	if veto.Blocks() {
		q.Reason = "conclusive_identity_blocks: " + veto.Verdict
		q.Evidence = append([]string(nil), veto.Evidence...)
		return q
	}

	bylineText := identityByline(excerpt)
	bylineSet := documentTokens(bylineText)

	// 2. Real author evidence.
	//
	// WHY stricter than MatchIdentity: MatchIdentity treats a zero-author
	// target as vacuously satisfying authors (authorOK := len(target.Authors)==0 || ...).
	// For a single attested target that is a leniency — an authorless record
	// can still be corroborated by DOI/title/year — but for 1-of-N selection
	// it is a bypass: any document whose title happens to overlap would claim
	// every authorless candidate. A wrong accept there is not a park but a
	// misfile. This gate therefore requires a non-empty Work.Authors and real
	// byline evidence: an exact surname match, or at least two
	// affiliation-marker-prefixed surnames. The two-marker requirement is why
	// bylineMarkedSurname distinguishes marked from numeric: a single
	// lettered marker cannot be told from a different surname (Clarke vs
	// Clark), so two independent markers are needed to establish authorship
	// without an exact hit.
	if len(candidate.Work.Authors) == 0 {
		q.Reason = "author_evidence_required: target has no authors"
		return q
	}
	exact, prefixed := 0, 0
	var authorEvidence []string
	for _, author := range candidate.Work.Authors {
		family := familyToken(author)
		if family == "" {
			continue
		}
		if bylineHasExactly(bylineSet, family) {
			exact++
			authorEvidence = append(authorEvidence, "author family name matched: "+family)
			continue
		}
		if marked, _ := bylineMarkedSurname(bylineSet, family); marked {
			prefixed++
			authorEvidence = append(authorEvidence, "author family name matched with an affiliation marker: "+family)
		}
	}
	if !(exact > 0 || prefixed >= 2) {
		q.Reason = "author_evidence_required: no exact surname and fewer than two marked surnames"
		// Keep any partial author evidence for audit, but the verdict is fail.
		q.Evidence = authorEvidence
		return q
	}
	q.Evidence = append(q.Evidence, authorEvidence...)

	// 3. Exact printed title, unconditionally.
	//
	// WHY stricter than MatchIdentity: MatchIdentity's corroboratingIdentifier
	// early-return (identity.go:272) can pass on a whole-document DOI/PMID/arXiv
	// match plus authorOK, before titlePrintedAsLine is ever checked. For a
	// single target that is acceptable — the identifier is strong evidence and
	// the title gate is still required for the non-corroborated path — but for
	// 1-of-N selection it would let a comment or reply that cites the
	// candidate's DOI and shares an author family name auto-bind, because the
	// title gate would be skipped entirely. This rule requires
	// titlePrintedAsLine unconditionally, over the byline window, with the
	// same label and gluing recovery the identity rules use. Loose token
	// overlap (60% threshold) is insufficient for autonomous selection; the
	// whole title must be printed as a delimited line.
	phrase := identityTitlePhrase(candidate.Work.Title)
	if phrase == "" || !titlePrintedAsLine(bylineSegments(bylineText), phrase) {
		q.Reason = "title_not_printed_as_line"
		return q
	}
	q.Evidence = append(q.Evidence, "requested title printed as a line in the front matter")

	// 4. Candidate-binding year predicate — NOT MatchIdentity's yearConflict.
	//
	// WHY a new predicate: MatchIdentity defines yearConflict only when
	// matches < len(tokens) (identity.go:260). That conjunct makes the check
	// provably defeated by an exact printed title: when every significant
	// token is present — which titlePrintedAsLine guarantees — matches ==
	// len(tokens) and yearConflict is forced false. A preprint routinely
	// dated a year before its version of record therefore passes
	// MatchIdentity on its exact title, and that leniency is intentional for
	// single-target verification (the document is the same work). For
	// candidate selection among neighbouring papers by the same author, the
	// same leniency would auto-bind a 2020 preprint to a 2026 VoR job (or
	// vice versa) on title and author alone. This predicate therefore
	// evaluates year independently of title completeness:
	//   target year zero -> neutral, qualifies;
	//   byline window exposes no year (bylineYears false) -> neutral, qualifies;
	//   target year appears in the byline window -> compatible;
	//   window exposes one or more years and none equals the target year -> DISQUALIFIED.
	// The window is identityByline, the same 2 KiB byline the author/title
	// gates read, so the year evidence is page-one front matter, not
	// bibliography.
	if candidate.Work.Year != 0 {
		if bylineYears(bylineText) && !strings.Contains(bylineText, fmt.Sprint(candidate.Work.Year)) {
			q.Reason = fmt.Sprintf("year_mismatch: byline year does not match requested year %d", candidate.Work.Year)
			return q
		}
		if strings.Contains(bylineText, fmt.Sprint(candidate.Work.Year)) {
			q.Evidence = append(q.Evidence, "year matched")
		}
	}

	// 5. Identifier corroboration scoped to page one.
	//
	// WHY scoped and WHY review: corroboratingIdentifier is what separates
	// "the metadata agrees" from "the document says so itself". The blind
	// front-matter DOI window (1 KiB, conclusive DOIs only) misses a DOI
	// printed in a running footer or below the abstract — 17 of 40 real
	// papers — so a metadata-only title/author/year agreement would park
	// every such paper if the blind window were the only identifier source.
	// Searching the whole document is safe for MatchIdentity only because the
	// title gate has already fired; for candidate selection the same whole-
	// document search would let a reference-list citation of the candidate's
	// DOI corroborate the wrong document. This gate therefore reuses
	// corroboratingIdentifier but against identityPageOne (4 KiB, first page)
	// — the widest front-matter window that still excludes the bibliography
	// — never the whole excerpt. Absent corroboration the candidate is not
	// auto-bindable but IS Review:true: the metadata agreement is suggestive
	// and worth surfacing as a ranked suggestion, but without a document-
	// printed identifier it must not bind autonomously. A future change to
	// the 4 KiB bound is a new rule version.
	pageOne := identityPageOne(excerpt)
	if corr := corroboratingIdentifier(pageOne, candidate.Work); corr == "" {
		q.Review = true
		q.Reason = "identifier_not_printed_on_page_one"
		return q
	} else {
		q.Evidence = append(q.Evidence, corr)
	}

	q.Qualifies = true
	q.Reason = ""
	return q
}

// SelectAutoBindCandidate returns the single candidate safe to bind automatically.
// ok is false when none qualifies, more than one qualifies, or any candidate
// lands in Review — ambiguity always abstains.
//
// Deterministic iteration order; never depends on map order. The caller
// supplies candidates in a stable slice order and this function evaluates them
// in that order.
func SelectAutoBindCandidate(excerpt string, candidates []BindCandidate) (winner CandidateQualification, ok bool, reason string) {
	if len(candidates) == 0 {
		return CandidateQualification{}, false, "no candidates"
	}
	var qualified []CandidateQualification
	var hasReview bool
	var reviewKey string
	for _, c := range candidates {
		q := QualifyCandidate(excerpt, c)
		if q.Review {
			hasReview = true
			if reviewKey == "" {
				reviewKey = q.Key
			}
		}
		if q.Qualifies {
			qualified = append(qualified, q)
		}
	}
	// Any Review alongside a qualifier is ambiguity: the document is
	// suggestive of one candidate but conclusive for another. Abstain and
	// surface the ambiguity rather than picking the qualifier.
	if hasReview && len(qualified) > 0 {
		return CandidateQualification{}, false, "ambiguous: qualifier alongside review (review: " + reviewKey + ")"
	}
	if hasReview {
		return CandidateQualification{}, false, "ambiguous: candidate requires review (review: " + reviewKey + ")"
	}
	switch len(qualified) {
	case 0:
		return CandidateQualification{}, false, "no candidate qualifies"
	case 1:
		return qualified[0], true, ""
	default:
		return CandidateQualification{}, false, fmt.Sprintf("ambiguous: multiple candidates qualify (%d)", len(qualified))
	}
}
