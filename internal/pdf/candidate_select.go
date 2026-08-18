// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"crypto/sha256"
	"encoding/hex"
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
//
// Version history. `/1` was the rule as first committed. Its acceptance set
// then moved materially — front-matter correction and non-article markers
// became gates, author evidence was scoped, title-prefix semantics tightened,
// year semantics changed — while the constant stayed `/1`. A version that
// names two different acceptance sets is not a version: a `/1` provenance row
// cannot be told apart from an unsafe pre-repair one. Hence `/2`. `/3` adds
// embedded metadata as a second source for gate 5's identifier corroboration
// (see metadata.go), which widens the acceptance set — a document naming
// itself only in its XMP packet qualifies under `/3` and reached Review under
// `/2`. Historical provenance is never rewritten; rows genuinely decided by an
// earlier rule keep saying so and `papio doctor` names them (see
// grab.Service.BoundByRule).
const CandidateBindingRule = "candidate_auto_bind/3"

// BindDocument is the inbound document side of a candidate decision: the text
// the identity rules read, plus the file's own embedded metadata.
//
// Metadata is optional and additive. An empty Metadata reproduces the text-only
// behaviour exactly, which is the ordinary case for scans and for any file no
// publisher produced — so a caller that cannot supply it (a synthetic fixture,
// a gate-reachability probe over constructed text) loses no correctness, only
// the coverage metadata would have added.
type BindDocument struct {
	Excerpt  string
	Metadata MetadataFields
}

// Digest identifies everything the predicate read, for callers that must pin a
// decision's input without storing scholarly text — in practice the auto-bind
// provenance row.
//
// A document carrying no usable metadata digests to exactly the SHA-256 of its
// excerpt, unchanged from when text was the only input, so the common case stays
// comparable across rule versions. Metadata is folded in only when it exists,
// separated by a NUL that cannot occur in either part, because two decisions
// differing only in metadata — one binding, one parking — must not record the
// same digest: an audit row that cannot distinguish its own inputs cannot
// reconstruct its decision.
func (d BindDocument) Digest() string {
	sum := sha256.Sum256([]byte(d.Excerpt))
	if canonical := d.Metadata.Canonical(); canonical != "" {
		sum = sha256.Sum256([]byte(d.Excerpt + "\x00" + canonical))
	}
	return hex.EncodeToString(sum[:])
}

// BindCandidate is one job inbound bytes might belong to.
type BindCandidate struct {
	Key   string // caller's opaque key, in practice a job id
	Work  work.Work
	Bound []string // DOIs durably bound to that job
}

// CandidateGate names one gate of the candidate-binding rule. It exists so a
// caller — in practice the Phase 2 measurement gate — can assert which gate a
// candidate actually REACHED rather than trusting a corpus label. A label is a
// claim about the rule; a trace is an observation of it, and the two diverge
// silently (a case labelled "year" that dies at the author gate still counted
// as year coverage before this existed). Gate identifiers are part of the
// measurement contract, not of the acceptance decision: nothing in
// QualifyCandidate branches on them.
type CandidateGate string

// Gates in rule order: gate 1 is the conclusive-identity veto, gate 1b splits
// into the non-article and correction marker checks, then gates 2 to 5 are
// author evidence, printed title, year token and page-one identifier. Every
// gate a run evaluates is appended to Reached, so the terminal gate plus the
// disposition fully locate a verdict.
const (
	GateConclusiveVeto CandidateGate = "conclusive-veto"
	GateNonArticle     CandidateGate = "non-article-marker"
	GateCorrection     CandidateGate = "correction-marker"
	GateAuthor         CandidateGate = "author-evidence"
	GateTitle          CandidateGate = "title-printed-as-line"
	GateYear           CandidateGate = "year-token"
	GateIdentifier     CandidateGate = "identifier-page-one"
)

// CandidateQualification explains one candidate's verdict.
type CandidateQualification struct {
	Key       string
	Qualifies bool     // safe to bind without asking a human
	Review    bool     // suggestive but insufficient: park for confirmation
	Reason    string   // why not, when Qualifies is false
	Evidence  []string // human-readable evidence for audit and review

	// Gate is the gate evaluation stopped at, and Reached is every gate
	// evaluated in rule order (Gate is its last element). Both are observed
	// during the traversal below; neither is read by any decision.
	Gate    CandidateGate
	Reached []CandidateGate
}

// Disposition names the terminal outcome: accept, review or abstain. The gate
// alone does not distinguish them — a run that reaches the identifier gate
// accepts when the identifier is printed and parks for review when it is not.
func (q CandidateQualification) Disposition() string {
	switch {
	case q.Qualifies:
		return "accept"
	case q.Review:
		return "review"
	default:
		return "abstain"
	}
}

// reach records arrival at a gate. Recording happens before the gate's
// evidence is examined, so a run that stops at a gate is reported as having
// reached it — reachability is the question the measurement gate asks.
func (q *CandidateQualification) reach(g CandidateGate) {
	q.Reached = append(q.Reached, g)
	q.Gate = g
}

// QualifyCandidate applies the candidate-binding acceptance rule to one candidate.
//
// The rule is deliberately stricter than MatchIdentity. Each numbered gate
// below carries a WHY comment recording the divergence and the measured risk
// it closes. All gates must pass for Qualifies to be true. Absent identifier
// corroboration — in the document's page-one text or in its own embedded
// metadata — is the one gate that yields Review instead of a hard
// disqualification, because curated-metadata agreement without the document
// naming itself is suggestive but not self-asserting.
func QualifyCandidate(doc BindDocument, candidate BindCandidate) CandidateQualification {
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
	q.reach(GateConclusiveVeto)
	veto := CheckConclusiveIdentity(doc.Excerpt, candidate.Bound)
	if veto.Blocks() {
		q.Reason = "conclusive_identity_blocks: " + veto.Verdict
		q.Evidence = append([]string(nil), veto.Evidence...)
		return q
	}

	bylineText := identityByline(doc.Excerpt)
	segments := bylineSegments(bylineText)
	phrase := identityTitlePhrase(candidate.Work.Title)

	// 1b. Front-matter correction and non-article markers.
	//
	// WHY this gate exists: candidate_select.go previously referenced
	// nonArticleMarkers and correctionMarkers zero times, while MatchIdentity
	// applies both (identity.go:22, :39, :55 and at :93, :112-122 via
	// correctionMarkerIn and chapterErratumPrefixes). Consequence: an erratum
	// whose front matter prints "Erratum: <target title>", the target's
	// authors, the target's year and the target's own DOI passes all five
	// gates and is auto-filed AS the original paper. The conclusive-identity
	// veto cannot catch it because the DOI the document prints IS the target's.
	// Reusing identity.go's helpers and its correctionPointerPhrases exclusion
	// ("Erratum to this chapter is available at ..." must NOT be treated as
	// the document being an erratum) closes that wrong-accept family.
	//
	// WHY the dispositions differ: a nonArticleMarkers hit (supplementary
	// information, etc.) is a document that is not a paper at all and should
	// hard-disqualify (fail, not Review). A correctionMarkers hit is a
	// document ABOUT another work that legitimately reprints the target's
	// front matter. For a single attested target MatchIdentity can afford
	// Review there — the operator may have genuinely requested the erratum —
	// and discarding it is irreversible while parking is not. A 1-of-N
	// selection has no such attestation: no job claims "I am the erratum",
	// so auto-binding the erratum to the paper's job is a silent misfile.
	// The correct disposition for candidate binding is therefore abstain
	// (hard fail, not Review, and never Qualifies): park the grab for a
	// human, do not file it under the paper. Both dispositions reuse the same
	// helpers (correctionMarkerIn already handles wide-gap gluing and the
	// pointer-phrase exclusion single-sourced from identity.go).
	foldedByline := typographicFolder.Replace(bylineText)
	q.reach(GateNonArticle)
	if m := candidateNonArticleMarker(foldedByline); m != "" {
		q.Reason = "non_article_marker: " + m
		q.Evidence = append(q.Evidence, "front matter marks non-article content: "+m)
		return q
	}
	q.reach(GateCorrection)
	if m := correctionMarkerIn(strings.Split(foldedByline, "\n")); m != "" {
		q.Reason = "correction_marker: " + m
		q.Evidence = append(q.Evidence, "front matter marks a correction or comment: "+m)
		return q
	}

	// Positional scoping for gates 2 and 3.
	//
	// WHY positional: gates 2 and 3 previously read a 2 KiB blob
	// (identityByline) as a bag and treated any hit anywhere as positional
	// evidence. documentTokens(bylineText) let a surname appearing only in the
	// printed title or journal name satisfy "real author evidence", and
	// titlePrintedAsLine accepted ANY matching segment, so a running head or
	// a right-column reference line glued by wide-gap recovery satisfied
	// "exact printed title". Combined with a shared author family name, a
	// compatible year and the target's DOI cited on page one, the wrong
	// document qualified. Both gates must read positionally: the author line
	// is between the title line and the abstract, not inside the title or
	// journal line; the title line is a plausible title position before the
	// byline/abstract, not a repeated header. A repeated identical segment is
	// evidence of a running head, not of a title. This fix shares one
	// segmentation (bylineSegments, identityByline) so the positional
	// definition never diverges.
	titleStart, titleEnd, titleFoundForScope := findCandidateTitleRange(segments, phrase)
	authorSet := candidateAuthorTokenSet(segments, titleStart, titleEnd, titleFoundForScope)

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
	//
	// WHY positional: author evidence is scoped to the byline segments
	// between the title line and the abstract, excluding the title phrase's
	// own tokens and journal/abstract segments. bylineHasExactly over the
	// whole 2 KiB window let a surname appearing only in the title ("Stone"
	// in "Stone Analysis of ...") satisfy the gate with no author line
	// present. That unsupported WHY is why this gate now reads
	// candidateAuthorTokenSet.
	q.reach(GateAuthor)
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
		if bylineHasExactly(authorSet, family) {
			exact++
			authorEvidence = append(authorEvidence, "author family name matched: "+family)
			continue
		}
		if marked, _ := bylineMarkedSurname(authorSet, family); marked {
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
	//
	// WHY strict prefix: titlePrintedAsLine accepts a punctuation boundary
	// and up to four label words, so candidate "Target Title" is accepted
	// against document "Target Title: A Different Study". For single-target
	// verification that leniency is right (same work, subtitle dropped in
	// metadata). For 1-of-N selection it lets a strict-prefix candidate win
	// against a different work. This gate therefore requires that the printed
	// line not be a strict extension across a subtitle boundary: after the
	// phrase matches, the remainder of that segment must not introduce
	// additional title content. Gluing recovery (ligature, hyphen-wrap,
	// column-glue via bylineSegments) is kept intact; only trailing content
	// is tightened. The same-work case (metadata genuinely lacking the
	// subtitle) is deliberately sacrificed to abstention here: a wrong accept
	// files the wrong paper, while abstention parks for a human and never
	// corrupts the library.
	//
	// WHY positional: a title that appears as a running head or a
	// right-column reference line glued by wide-gap recovery also satisfies
	// the bag check. Title evidence must come from a plausible title
	// position before the byline/abstract, not a repeated header. A repeated
	// identical segment is evidence of a running head, not of a title.
	q.reach(GateTitle)
	if phrase == "" || !candidateTitlePrintedAsLine(segments, phrase) {
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
	//
	// WHY token-boundary: the previous check used strings.Contains on the
	// raw byline text, so a year embedded in any longer number — a DOI like
	// 10.1234/j.2019.05.003, an ISSN, a grant number or a page range
	// containing 2019 — satisfied a 2019 candidate against a 2024 document
	// and symmetrically masked a real conflict. bylineYears already defines a
	// year token as \b(?:19|20)\d{2}\b; this gate reuses that definition via
	// bylineYearPattern so the predicate compares year tokens with real
	// boundaries, preserving the three neutral cases exactly.
	q.reach(GateYear)
	if candidate.Work.Year != 0 {
		yearTokens := bylineYearPattern.FindAllString(bylineText, -1)
		hasYears := len(yearTokens) > 0
		yearStr := fmt.Sprint(candidate.Work.Year)
		contains := false
		for _, y := range yearTokens {
			if y == yearStr {
				contains = true
				break
			}
		}
		if hasYears && !contains {
			q.Reason = fmt.Sprintf("year_mismatch: byline year does not match requested year %d", candidate.Work.Year)
			return q
		}
		if contains {
			q.Evidence = append(q.Evidence, "year matched")
		}
	}

	// 5. Identifier corroboration: the document naming itself, in page-one
	//    text or in its own embedded metadata.
	//
	// WHY scoped and WHY review: corroboration here is what separates "the
	// curated metadata agrees" from "the document says so itself". The blind
	// front-matter DOI window (1 KiB, conclusive DOIs only) misses a DOI
	// printed in a running footer or below the abstract — 17 of 40 real
	// papers — so a metadata-only title/author/year agreement would park
	// every such paper if the blind window were the only identifier source.
	// Searching the whole document is safe for MatchIdentity only because the
	// title gate has already fired; for candidate selection the same whole-
	// document search would let a reference-list citation of the candidate's
	// DOI corroborate the wrong document. The text arm therefore reuses
	// corroboratingIdentifier but against identityPageOne (4 KiB, first page)
	// — the widest front-matter window that still excludes the bibliography
	// — never the whole excerpt. A future change to the 4 KiB bound is a new
	// rule version.
	//
	// WHY embedded metadata satisfies the same gate: the attribution problem
	// the text arm can only approximate does not arise there. A reference list
	// cannot reach a file's XMP packet, and prism:doi means "this file is that
	// DOI" by definition of the field, so an allowlisted hit is the document
	// asserting its own identity rather than text that might be a citation.
	// Measured over the operator's library the shipped allowlisted reader
	// reaches 27.0% of the documents that get this far against the text arm's
	// 18%, nearly disjointly, with no document's metadata naming a different
	// library work (~185k pairs). 34.2% is the exploratory figure that also
	// counted free-text fields; those are excluded here on purpose, so do not
	// quote it for this reader.
	//
	// WHY it corroborates but never authorises: this is gate 5, so title,
	// author and year have already had to agree. A supplement's XMP ordinarily
	// carries its PARENT article's DOI, so metadata alone would bind
	// supplementary bytes under the article's citation.
	//
	// Which earlier gate actually stops that was measured, not assumed, by
	// scoring every secondary attachment in the operator's library against its
	// own parent's record — the exact adversary, since a supplement inherits
	// the parent's curated identity. Over 6 trials: 0 accepts, 5 hard-fenced
	// and 1 parked. The fencing was done by GateTitle (3), GateAuthor (1) and
	// GateYear (1); candidateNonArticleMarker fenced NOTHING, so the marker
	// vocabulary is not what protects this path and hardening it would be
	// theatre. The one park reached THIS gate and stopped only because the
	// file's metadata did not name its parent — i.e. the fence in front of the
	// metadata arm is one field thick, and 6 trials cannot measure it. Do not
	// read the 0 accepts as authorisation: a Zotero library holds the papers
	// the operator kept, so it under-samples supplements and aggregator cover
	// sheets, which are what the browser path actually captures. Metadata may
	// substitute for WHERE the identifier was found, never for identity-frame
	// agreement.
	q.reach(GateIdentifier)
	pageOne := identityPageOne(doc.Excerpt)
	switch corr := corroboratingIdentifier(pageOne, candidate.Work); {
	case corr != "":
		q.Evidence = append(q.Evidence, corr)
	default:
		meta := doc.Metadata.NamesWork(candidate.Work)
		if meta == "" {
			q.Review = true
			q.Reason = "identifier_not_printed_on_page_one"
			return q
		}
		q.Evidence = append(q.Evidence, meta)
	}

	q.Qualifies = true
	q.Reason = ""
	return q
}

// candidateNonArticleMarker reports the first nonArticleMarkers entry that
// prefixes a segment of the byline window, or "" if none does. It mirrors
// MatchIdentity's front-matter check but over the byline window and via
// wideGapSegments, so a marker glued to a running head or page number
// ("1  Supplementary information ...") is not missed for the same reason
// correctionMarkerIn needs wide-gap recovery.
func candidateNonArticleMarker(foldedByline string) string {
	for _, line := range strings.Split(foldedByline, "\n") {
		for _, seg := range wideGapSegments(line) {
			lower := strings.ToLower(strings.TrimSpace(seg))
			for _, marker := range nonArticleMarkers {
				if strings.HasPrefix(lower, marker) {
					return marker
				}
			}
		}
	}
	return ""
}

// findCandidateTitleRange returns the segment range that permissively matches
// phrase via titleRunMatches, for positional scoping of the author gate. It
// reuses titleRunMatches so the gluing and label recovery stay single-sourced.
func findCandidateTitleRange(segments []titleSegment, phrase string) (startSeg, endSeg int, ok bool) {
	if phrase == "" {
		return 0, 0, false
	}
	for start, seg := range segments {
		for offset := range min(len(seg.words), titleLabelWords+1) {
			if offset != 0 && !seg.labels[offset-1] {
				continue
			}
			if !titleRunMatches(segments[start:], offset, phrase) {
				continue
			}
			// Walk to find where phrase completes to determine endSeg.
			matched := 0
			curOff := offset
			for i := start; i < len(segments); i++ {
				if matched != 0 && numberedRun(segments[i].words[curOff:]) {
					curOff = 0
					continue
				}
				words := segments[i].words[curOff:]
				breaks := segments[i].breaks[curOff:]
				curOff = 0
				for j, word := range words {
					switch {
					case strings.HasPrefix(phrase[matched:], word):
						matched += len(word)
					case strings.HasPrefix(word, phrase[matched:]) && isASCIIDigits(word[len(phrase)-matched:]):
						return start, i, true
					default:
						matched = -1
						break
					}
					if matched == -1 {
						break
					}
					if matched == len(phrase) {
						if j == len(words)-1 || breaks[j] {
							return start, i, true
						}
						matched = -1
						break
					}
				}
				if matched == -1 {
					break
				}
				if i == len(segments)-1 {
					break
				}
			}
			return start, start, true
		}
	}
	return 0, 0, false
}

// candidateAuthorTokenSet builds the token set that gate 2 may use for author
// evidence. It excludes the title's own segments, any header before the title,
// and anything at or after the abstract/keywords heading, so a surname that
// appears only in the title or journal line cannot satisfy the gate.
func candidateAuthorTokenSet(segments []titleSegment, titleStart, titleEnd int, hasTitle bool) map[string]struct{} {
	set := make(map[string]struct{})
	abstractIdx := len(segments)
	for i, seg := range segments {
		for _, w := range seg.words {
			if w == "abstract" || w == "keywords" || w == "keyword" {
				abstractIdx = i
				break
			}
		}
		if abstractIdx != len(segments) {
			break
		}
	}
	for i, seg := range segments {
		if hasTitle {
			if i < titleStart {
				continue
			}
			if i >= titleStart && i <= titleEnd {
				continue
			}
		}
		if i >= abstractIdx {
			continue
		}
		for _, w := range seg.words {
			set[w] = struct{}{}
		}
	}
	// If we excluded everything (no title found or title at end), fall back to
	// all pre-abstract tokens so zero-author and similar gates still have
	// evidence to report. This preserves the original fail-open for the
	// author gate when title position is unknown, but title-positioned cases
	// use the scoped set.
	if len(set) == 0 && abstractIdx > 0 {
		for i := 0; i < abstractIdx && i < len(segments); i++ {
			for _, w := range segments[i].words {
				set[w] = struct{}{}
			}
		}
		if hasTitle {
			for i := titleStart; i <= titleEnd && i < len(segments); i++ {
				for _, w := range segments[i].words {
					delete(set, w)
				}
			}
		}
	}
	return set
}

// candidateTitlePrintedAsLine reports whether phrase is printed as a delimited
// unit in a plausible title position with strict trailing-content enforcement.
// It reuses titleRunMatches for gluing recovery but adds two tightenings over
// the base helper: (1) strict prefix: after the phrase matches, the remainder
// of that printed segment must not introduce additional title content (subtitle
// boundary), and (2) positional: the match must start in the early byline
// window before the author/abstract region, and a phrase that matches
// identically in more than one distinct position is treated as a running head,
// not a title.
func candidateTitlePrintedAsLine(segments []titleSegment, phrase string) bool {
	if phrase == "" {
		return false
	}
	abstractIdx := len(segments)
	for i, seg := range segments {
		for _, w := range seg.words {
			if w == "abstract" || w == "keywords" || w == "keyword" {
				abstractIdx = i
				break
			}
		}
		if abstractIdx != len(segments) {
			break
		}
	}
	type matchPos struct{ start, end int }
	var matches []matchPos
	for start := range segments {
		if start >= abstractIdx {
			break
		}
		if start > 3 {
			continue
		}
		seg := segments[start]
		for offset := range min(len(seg.words), titleLabelWords+1) {
			if offset != 0 && !seg.labels[offset-1] {
				continue
			}
			if !candidateStrictTitleRunMatches(segments[start:], offset, phrase) {
				continue
			}
			absEnd := start
			matched := 0
			curOff := offset
			for i := start; i < len(segments); i++ {
				if matched != 0 && numberedRun(segments[i].words[curOff:]) {
					curOff = 0
					continue
				}
				words := segments[i].words[curOff:]
				breaks := segments[i].breaks[curOff:]
				curOff = 0
				for j, word := range words {
					switch {
					case strings.HasPrefix(phrase[matched:], word):
						matched += len(word)
					case strings.HasPrefix(word, phrase[matched:]) && isASCIIDigits(word[len(phrase)-matched:]):
						absEnd = i
						matched = len(phrase)
						break
					default:
						matched = -1
						break
					}
					if matched == -1 {
						break
					}
					if matched == len(phrase) {
						if j == len(words)-1 || breaks[j] {
							absEnd = i
							break
						}
						matched = -1
						break
					}
				}
				if matched == len(phrase) {
					break
				}
				if matched == -1 {
					break
				}
			}
			matches = append(matches, matchPos{start: start, end: absEnd})
		}
	}
	if len(matches) == 0 {
		return false
	}
	if len(matches) > 1 {
		seen := make(map[int]struct{})
		for _, m := range matches {
			seen[m.start] = struct{}{}
		}
		if len(seen) > 1 {
			return false
		}
	}
	return true
}

// candidateStrictTitleRunMatches wraps titleRunMatches with strict trailing-
// content enforcement: after the phrase matches, the remainder of that printed
// segment must not introduce additional title content. A candidate "Target
// Title" against document "Target Title: A Different Study" matches as a
// prefix up to the colon, but the segment still contains "a different study"
// after the boundary — that is a different work and must not auto-bind. Gluing
// recovery (numberedRun, hyphen-wrap, ligature folding) is preserved because
// the check delegates to titleRunMatches first; only trailing words beyond the
// matched boundary are examined.
func candidateStrictTitleRunMatches(segments []titleSegment, offset int, phrase string) bool {
	if !titleRunMatches(segments, offset, phrase) {
		return false
	}
	matched := 0
	curOff := offset
	for _, seg := range segments {
		if matched != 0 && numberedRun(seg.words[curOff:]) {
			curOff = 0
			continue
		}
		words := seg.words[curOff:]
		breaks := seg.breaks[curOff:]
		curOff = 0
		for j, word := range words {
			switch {
			case strings.HasPrefix(phrase[matched:], word):
				matched += len(word)
			case strings.HasPrefix(word, phrase[matched:]) && isASCIIDigits(word[len(phrase)-matched:]):
				return true
			default:
				return false
			}
			if matched == len(phrase) {
				if j == len(words)-1 {
					return true
				}
				if breaks[j] {
					return false
				}
				return false
			}
		}
	}
	return false
}

// SelectAutoBindCandidate returns the single candidate safe to bind automatically.
// ok is false when none qualifies, more than one qualifies, or any candidate
// lands in Review — ambiguity always abstains.
//
// Deterministic iteration order; never depends on map order. The caller
// supplies candidates in a stable slice order and this function evaluates them
// in that order.
func SelectAutoBindCandidate(doc BindDocument, candidates []BindCandidate) (winner CandidateQualification, ok bool, reason string) {
	if len(candidates) == 0 {
		return CandidateQualification{}, false, "no candidates"
	}
	var qualified []CandidateQualification
	var hasReview bool
	var reviewKey string
	for _, c := range candidates {
		q := QualifyCandidate(doc, c)
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
