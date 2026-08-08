// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"papio/internal/work"
)

var doiPattern = regexp.MustCompile(`(?i)\b10\.\d{4,9}/[-._;()/:a-z0-9]+`)

var nonArticleMarkers = []string{
	"supporting information", "supplementary information", "supplementary material",
	"supplemental information", "supplemental material", "online appendix",
	"electronic supplementary", "supporting data",
}

// correctionMarkers flags a document that is ABOUT another work rather than
// being the work itself — an erratum, correction, retraction, or comment
// that legitimately prints the requested paper's own title, authors, and
// DOI in its front matter, because that is the paper it is correcting.
// Unlike nonArticleMarkers above, a hit here does not reject: the operator
// may genuinely have requested the erratum rather than the paper, and
// discarding a candidate outright is not reversible the way parking it for
// review is. It is prefix-anchored on "retraction of" rather than the bare
// "retraction", and omits "response to" entirely, because "Retraction of
// scientific papers: a bibliometric study" and "Response to Intervention"
// are both real titles that the shorter prefixes would catch.
var correctionMarkers = []string{
	"erratum", "errata", "corrigendum", "correction to", "correction:", "author correction",
	"publisher correction", "retraction of", "retraction note", "retracted article",
	"expression of concern", "comment on", "comments on", "commentary on",
	"reply to", "rejoinder to", "withdrawal notice",
}

// correctionPointerPhrases catches the shape a corrected work prints ON
// ITSELF, not in its own voice but about a correction published elsewhere: a
// Springer book chapter that has since had an erratum issued says so in a
// footnote on its own first page, "Erratum to this chapter is available at
// 10.1007/...", and that sentence names the chapter being pointed AT — the
// very document this guard is trying to pass — not a chapter announcing its
// own erratum. A segment whose prefix is one of these is excluded from the
// correctionMarkers test in correctionMarkerIn before that test runs.
var correctionPointerPhrases = []string{
	"erratum to this chapter", "erratum to this article", "erratum to this paper",
	"correction to this chapter", "correction to this article", "correction to this paper",
}

var titleStopwords = map[string]bool{
	"about": true, "after": true, "also": true, "among": true, "and": true,
	"been": true, "between": true, "from": true, "into": true, "more": true,
	"over": true, "that": true, "the": true, "their": true, "these": true,
	"this": true, "through": true, "under": true, "using": true, "with": true,
	"what": true, "when": true, "where": true, "which": true, "while": true,
	"within": true, "without": true, "your": true,
}

// MatchIdentity compares extracted document text with the requested work using
// the default 60% title-token threshold.
func MatchIdentity(text string, target work.Work) IdentityDecision {
	return MatchIdentityWithThreshold(text, target, 0.6)
}

// MatchIdentityWithThreshold applies the front-matter DOI and non-article
// rules, then requires the configured share of significant title tokens.
//
// Title, author, and year evidence is read from the BYLINE WINDOW — the top of
// page one — not from the whole document. A paper's bibliography is a bag of
// several hundred other papers' titles and thousands of their authors, so
// whole-document matching made the fallback almost undiscriminating: measured
// over 1560 deliberately mismatched document/metadata pairs from one real
// library, 155 (9.9%) were accepted as the wrong work, unlocked by given names
// like "david" and "john" and by any recent year appearing in a citation.
// Scoped to the byline the same corpus yields none.
func MatchIdentityWithThreshold(text string, target work.Work, titleThreshold float64) IdentityDecision {
	if titleThreshold <= 0 || titleThreshold > 1 {
		titleThreshold = 0.6
	}
	frontMatter := identityFrontMatter(text)
	frontMatterLines := strings.Split(frontMatter, "\n")
	for _, line := range frontMatterLines {
		line = strings.ToLower(strings.TrimSpace(line))
		for _, marker := range nonArticleMarkers {
			if strings.HasPrefix(line, marker) {
				return reject("non-article marker: " + marker)
			}
		}
	}
	// The correction cap reads the wider byline window instead of the
	// front-matter DOI window above: pdftotext routinely glues a running
	// header or a page number onto the very line that carries the marker —
	// "J Sensor Syst 2025;12:1  Erratum: ..." — which can push the marker
	// past the first kilobyte even though it sits well inside the byline.
	// bylineText is computed once, here, ahead of the DOI branch below so
	// that branch's pass still routes through the cap, and reused for the
	// title and year checks further down instead of slicing the same window
	// under a second name.
	bylineText := identityByline(text)
	// Folded before the split for the same reason bylineSegments folds: a marker
	// hyphenated across a wrap ("Errat-\num:") is one word the text layer broke,
	// and splitting first leaves it invisible to every prefix test below.
	correctionMarker := correctionMarkerIn(strings.Split(typographicFolder.Replace(bylineText), "\n"))
	// A correction marker is diagnostic on every verdict a human will read, not
	// only on the ones it downgrades: "comment on" at the top of page one is the
	// first thing a reviewer wants to know, whatever else already withheld the
	// pass. So both helpers carry it, and the cap survives a later change to the
	// reasons a document reaches review by another route.
	noteMarker := func(evidence []string) []string {
		if correctionMarker == "" {
			return evidence
		}
		return append(evidence, "front matter marks a correction or comment: "+correctionMarker)
	}
	capPass := func(evidence ...string) IdentityDecision {
		if correctionMarker != "" {
			return review(noteMarker(evidence)...)
		}
		return pass(evidence...)
	}
	capReview := func(evidence ...string) IdentityDecision {
		return review(noteMarker(evidence)...)
	}

	// An identifier printed in the front matter is the strongest evidence
	// these rules have, except against a page that announces itself as being
	// about another work rather than being that work: an erratum reprints
	// the requested paper's own DOI in its own masthead. That is the one
	// shape a real library cannot measure — a library holds the paper, not
	// the erratum about it — so the 460,352-pair corpus run
	// (dev/identity-corpus.md) shows zero wrong accepts through this branch,
	// while a hand-built 1508-byte correction notice that inlines the
	// requested DOI passes it outright today. capPass, built from
	// correctionMarker above, is what closes that gap: every pass in this
	// function routes through it, and it downgrades to review instead of
	// rejecting, because the operator may genuinely have requested the
	// erratum, and discarding a candidate cannot be undone the way parking
	// it for a human can.
	wantDOI := normalizeDOI(target.DOI)
	gotDOIs := documentDOIs(frontMatter)
	if wantDOI != "" && len(gotDOIs) != 0 {
		for _, gotDOI := range gotDOIs {
			if gotDOI == wantDOI {
				return capPass("exact normalized DOI match: " + wantDOI)
			}
		}
		return reject("document DOI does not match requested DOI", "document DOI: "+strings.Join(gotDOIs, ", "))
	}

	tokens := identityTitleTokens(target.Title)
	if len(tokens) == 0 {
		return capReview("no usable requested DOI or title tokens")
	}
	byline := documentTokens(bylineText)
	matches := 0
	for _, token := range tokens {
		if _, ok := byline[token]; ok {
			matches++
		}
	}
	need := int(math.Ceil(float64(len(tokens)) * titleThreshold))
	if need < 1 {
		need = 1
	}
	if matches < need {
		// Reject discards the candidate outright, so it stays reserved for a
		// document that does not carry the requested title at all. A scanned or
		// badly OCR'd first page whose title surfaces further in is a question
		// for a human, not a verdict.
		if elsewhere := countTokens(documentTokens(text), tokens); elsewhere >= need {
			return capReview(fmt.Sprintf("title tokens matched only outside the front matter: %d/%d", elsewhere, len(tokens)))
		}
		return reject(fmt.Sprintf("title token evidence insufficient: %d/%d", matches, need))
	}

	evidence := []string{fmt.Sprintf("title tokens matched: %d/%d", matches, len(tokens))}

	// Token membership is a weak test and the corpus says exactly how weak: all
	// 52 wrong accepts over 398,786 mismatched pairs came through this gate, in
	// three families that share one cause. identityTitleTokens drops every word
	// under five runes, so "How to do a meta-analysis" reduces to {analysis} and
	// any paper by an author of that name clears 1/1; "Final Report - Volume 3,
	// Impacts" throws away the 3, so eighteen pairs of a numbered government
	// series matched each other at 3/4; and an unordered set cannot tell "Core
	// reporting practices in structural equation modeling" from "Update to core
	// reporting practices in structural equation modeling", which matched 5/5.
	//
	// A publisher prints the title as its own line, or as consecutive lines
	// where it wraps. Requiring that — the whole requested title, every short
	// word and stopword included, beginning at the start of a segment and ending
	// at the end of one — is what separates those three families from a real
	// match, because the tokens the set gate discards are precisely the ones
	// that distinguish a title from a title with words prepended.
	//
	// It is a REQUIREMENT for the final pass, not a substitute for the token
	// gate above: a scan whose title line is garbled still reaches review or a
	// corroborated pass on its printed identifier, never a discard.
	titlePrinted := titlePrintedAsLine(bylineSegments(bylineText), identityTitlePhrase(target.Title))
	if titlePrinted {
		evidence = append(evidence, "requested title printed as a line in the front matter")
	}

	exact, prefixed, numbered := 0, 0, 0
	for _, author := range target.Authors {
		switch family := familyToken(author); {
		case family == "":
		case bylineHasExactly(byline, family):
			exact++
			evidence = append(evidence, "author family name matched: "+family)
		default:
			marked, numeric := bylineMarkedSurname(byline, family)
			if !marked {
				continue
			}
			prefixed++
			if numeric {
				numbered++
			}
			evidence = append(evidence, "author family name matched with an affiliation marker: "+family)
		}
	}
	// One exact surname is real evidence. A prefix-only hit is not: the two-char
	// tolerance that lets "Tabikg" match "Tabik" also lets "Clarke" match
	// "Clark". Two of them together are, because a byline that marks one author
	// marks them all — the 12-author paper that motivated the tolerance had
	// every surname glued — whereas two unrelated near-miss surnames in one
	// byline is not a case that occurs.
	authorOK := len(target.Authors) == 0 || exact > 0 || prefixed >= 2

	// The year is corroboration, never a requirement. Restricted to the byline
	// it is absent from a fifth of legitimate papers, and unrestricted it
	// matched every wrong document in the corpus. But ABSENT and CONTRADICTED
	// differ, and a byline printing some other year is evidence against.
	//
	// It only counts against a document whose title match is INEXACT. A
	// preprint is routinely dated the year before its version of record — three
	// papers in the corpus — and its title matches completely; treating that as
	// a conflict rejected all three. Where some title tokens are missing too,
	// the pair of discrepancies is the signature of a neighbouring paper by the
	// same author, which is the case a bare author check would wave through.
	yearConflict := false
	if target.Year != 0 {
		switch {
		case strings.Contains(bylineText, fmt.Sprint(target.Year)):
			evidence = append(evidence, "year matched")
		case matches < len(tokens) && bylineYears(bylineText):
			yearConflict = true
		}
	}

	// An identifier printed anywhere in the document is the strongest signal
	// available and is what lets reprints through — the CACM edition of a NIPS
	// paper is catalogued under 2017 and contains no "2017" in its text. It is
	// not sufficient alone: the title gate is unordered token membership at 60%,
	// so "Comment on Deep Learning" clears a target titled "Deep Learning" and
	// could carry a bare "Correction to: <DOI>" further down. Requiring the
	// byline to agree on an author closes that, and still admits every reprint.
	if corroboration := corroboratingIdentifier(text, target); corroboration != "" {
		if authorOK {
			return capPass(append(evidence, corroboration)...)
		}
		// The two-marker rule is unsatisfiable for a target that names ONE
		// author: it reads a byline that marks EVERY surname, and with one
		// requested author there is only one marker to see. Wiley prints "Keith
		// D. Ciani1∗" and the catalogue record named Ciani alone, so a document
		// printing the requested DOI verbatim below its abstract went to review.
		//
		// A single marker stands in for the pair only when it is NUMBERED, and
		// only when the identifier is on the document's own page one. Both
		// bounds answer the same document: a comment, reply, or erratum on the
		// requested paper carries its title and cites its DOI, so on its own the
		// identifier does not establish that this is the paper rather than a
		// note about it. The numeric marker settles the surname — no surname
		// ends in a digit, so "Ciani1" is Ciani, whereas "Clarke" is
		// indistinguishable from Clark plus a lettered marker — and page one
		// settles the identifier: a paper prints its own in the footer or below
		// the abstract (17 of 40 real papers), while another paper's reaches it
		// through a citation.
		if numbered > 0 && len(target.Authors) == 1 {
			if pageOne := corroboratingIdentifier(identityPageOne(text), target); pageOne != "" {
				return capPass(append(evidence, pageOne)...)
			}
		}
	}

	switch {
	case !authorOK && prefixed > 0:
		return capReview(append(evidence, "only a prefix of one author's family name appears in the front matter")...)
	case !authorOK:
		return capReview(append(evidence, "no requested author family name in the front matter")...)
	case yearConflict:
		return capReview(append(evidence, fmt.Sprintf("front matter is dated differently to the requested year %d", target.Year))...)
	case !titlePrinted:
		return capReview(append(evidence, "requested title is not printed as a line in the front matter")...)
	}
	return capPass(evidence...)
}

// identityTitlePhrase is the requested title as the word sequence a publisher
// prints it in, keeping the short words and stopwords identityTitleTokens
// discards. Those words carry no weight in a membership test and all the weight
// in an ordering one: "update to" is what makes a different paper out of the
// same five significant words.
func identityTitlePhrase(title string) string { return strings.Join(normalizedTokens(title), "") }

// titleSegment is one printed run of words plus where punctuation fell inside
// it. Both edges matter: a title is a punctuation-delimited or line-delimited
// unit, and which side of it a stray word sits on is the whole question. A
// phrase that begins mid-clause is a title with words prepended — "Update to
// core reporting practices in structural equation modeling" against a request
// for "Core reporting practices in structural equation modeling", which matched
// 5/5 tokens in the corpus — while a phrase that ends mid-clause is a title
// quoted inside a longer one, which is what "Getting Safety Leadership Right"
// does to a request for "Safety Leadership".
type titleSegment struct {
	words []string
	// breaks[i] reports that punctuation, not a space, followed words[i], and
	// labels[i] narrows that to the punctuation a publisher ends a LABEL with.
	// The end edge accepts any punctuation; the start edge accepts only a label,
	// because a citing sentence reaches for a quote where a publisher reaches
	// for a colon.
	breaks []bool
	labels []bool
}

// bylineSegments returns the byline window as the runs a publisher printed on
// their own lines, split further where pdftotext glued a running head, a page
// number, or a second column on with two or more spaces — the same recovery
// correctionMarkerIn needs, for the same reason: a title line that arrived with
// "Downloaded from …" welded to its end is no longer a line, and the document
// stops looking like itself.
func bylineSegments(byline string) []titleSegment {
	var segments []titleSegment
	// Folding runs BEFORE the split so a title hyphenated across a line break
	// rejoins into one line: "self-deter-\nmination" is one word a publisher
	// broke, not two, and splitting first would leave the title unfindable in
	// exactly the documents that need it most.
	for _, line := range strings.Split(typographicFolder.Replace(byline), "\n") {
		for _, run := range wideGapSegments(line) {
			if segment := splitTitleSegment(run); len(segment.words) != 0 {
				segments = append(segments, segment)
			}
		}
	}
	return segments
}

func splitTitleSegment(run string) titleSegment {
	var segment titleSegment
	word := strings.Builder{}
	pending := rune(0)
	flush := func() {
		if word.Len() == 0 {
			return
		}
		segment.words = append(segment.words, strings.ToLower(word.String()))
		segment.breaks = append(segment.breaks, false)
		segment.labels = append(segment.labels, false)
		word.Reset()
	}
	mark := func() {
		if pending == 0 || len(segment.breaks) == 0 {
			return
		}
		segment.breaks[len(segment.breaks)-1] = true
		if strings.ContainsRune(labelTerminators, pending) {
			segment.labels[len(segment.labels)-1] = true
		}
	}
	for _, r := range run {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			mark()
			pending = 0
			word.WriteRune(r)
		default:
			flush()
			if !strings.ContainsRune(spaceCutset, r) && pending == 0 {
				pending = r
			}
		}
	}
	flush()
	mark()
	return segment
}

// labelTerminators end a label; other punctuation does not. A publisher labels a
// title with a colon, a full stop, or a rule — "Original Article:", "1.",
// "RESEARCH ARTICLE |" — where a sentence citing a title reaches for a quote,
// a dash, or a bracket: `We cite "Core reporting practices in structural
// equation modeling" for guidance` put a quote directly before the title and so
// satisfied any punctuation test, which is the citing-sentence false accept the
// label cap exists to refuse.
const labelTerminators = ":.|\u2022\u00b7"

// titlePrintedAsLine reports whether phrase is printed as a delimited unit:
// beginning where a segment begins or a short label ended, and ending where a
// segment ends or punctuation begins. A title that wraps spans consecutive
// segments, so intermediate segments must be consumed whole.
//
// The start edge allows a few words before the title because publishers print
// one: "Original Article:", "1.", "RESEARCH ARTICLE |". It allows only a FEW,
// because the other thing that precedes a delimited copy of the requested title
// is a sentence citing it — the three wrong accepts that survived this rule were
// all a newer paper quoting the requested title inside its own abstract, and a
// label is short where a sentence is not.
const titleLabelWords = 3

func titlePrintedAsLine(segments []titleSegment, phrase string) bool {
	if phrase == "" {
		return false
	}
	for start, segment := range segments {
		for offset := range min(len(segment.words), titleLabelWords+1) {
			if offset != 0 && !segment.labels[offset-1] {
				continue
			}
			if titleRunMatches(segments[start:], offset, phrase) {
				return true
			}
		}
	}
	return false
}

// titleRunMatches compares the printed words against the title as one character
// stream rather than word by word, because a text layer decides where the word
// boundaries are and it decides badly. "PsychologicalSafety and LearningBehavior
// in WorkTeams" lost the spaces inside three of its words, and matching tokens
// pairwise parked a paper whose printed title is otherwise byte-identical to the
// catalogue record. Concatenating both sides tolerates that, and the wrap across
// lines, without loosening either edge: the run must still start where a segment
// or a label ends and finish where the clause does.
func titleRunMatches(segments []titleSegment, offset int, phrase string) bool {
	matched := 0
	for i, segment := range segments {
		if matched != 0 && numberedRun(segment.words[offset:]) {
			// A line number, a page number, or a column marker interrupting the
			// wrap: a submitted manuscript numbers every line, so the title of
			// the Fransen preprint arrives as "…with an initial structure of",
			// "6", "vertical leadership". Digits alone cannot be part of a title
			// that has already begun, so stepping over them keeps the wrap
			// intact without letting any word through.
			offset = 0
			continue
		}
		words := segment.words[offset:]
		breaks := segment.breaks[offset:]
		offset = 0
		for j, word := range words {
			switch {
			case strings.HasPrefix(phrase[matched:], word):
				matched += len(word)
			case strings.HasPrefix(word, phrase[matched:]) && isASCIIDigits(word[len(phrase)-matched:]):
				// The last word carries a flattened superscript: a title ending
				// "MODEL OF CHANGE1" is the title plus a footnote mark, and the
				// mark closes the clause as surely as punctuation would.
				return true
			default:
				return false
			}
			if matched == len(phrase) {
				// The title ends here only if the page does too: a segment end,
				// or punctuation closing the clause.
				return j == len(words)-1 || breaks[j]
			}
		}
		// The phrase runs past this segment, so the wrap must continue into the
		// next one, and this segment must have been consumed to its end.
		if i == len(segments)-1 {
			return false
		}
	}
	return false
}

// numberedRun reports a segment that is nothing but digits — a line number, a
// page number, or a column marker. It is not part of any title, so a title
// already under way survives one.
func numberedRun(words []string) bool {
	if len(words) == 0 {
		return false
	}
	for _, word := range words {
		if !isASCIIDigits(word) {
			return false
		}
	}
	return true
}

// correctionMarkerIn reports the first correctionMarkers entry that prefixes
// a segment of the byline window, or "" if none does. pdftotext routinely
// glues a running header, a page number, or a second column onto the line
// that actually carries the marker, joined by two or more spaces where a
// human reader sees a column break — "J Sensor Syst 2025;12:1  Erratum:
// ..." and "1  Erratum: ..." are both real shapes — so a plain line-prefix
// test misses a marker a human reading the page would see immediately.
// correctionSegments recovers it by splitting on those wider gaps while
// leaving single spaces alone: "applied a Bonferroni correction for
// multiple comparisons" must stay one segment, or the word "correction"
// appearing mid-sentence would trip the cap on ordinary prose. Each segment
// is checked against correctionPointerPhrases first and skipped on a match,
// because a pointer to a correction published elsewhere is printed on the
// corrected work itself and would otherwise cap that document's own pass
// with a note about a correction to something else.
func correctionMarkerIn(lines []string) string {
	for _, line := range lines {
		for _, segment := range wideGapSegments(line) {
			if marker := correctionMarkerAt(strings.ToLower(segment)); marker != "" {
				return marker
			}
		}
	}
	return ""
}

// wideGapSegments splits a line where the text layer left a gap that joins two
// printed runs rather than separating two words of one: two or more whitespace
// characters, or a single tab or other control separator. pdftotext concatenates
// a running head, a page number, or a second column with either, and a tab
// defeated a two-space test entirely — 28 of 632 documents in a real library
// carry a literal tab.
//
// A single space is deliberately not a gap, and neither is a single no-break or
// thin space: those are word spaces, chosen by a typesetter to hold words
// together — French style puts one before a colon — so splitting on one would
// take a title apart. "applied a Bonferroni correction for multiple comparisons"
// must likewise stay one segment, or ordinary prose starts tripping every rule
// that reads a segment start. Zero-width leaders are trimmed because
// strings.TrimSpace does not, and a marker behind one is invisible to a prefix
// test.
func wideGapSegments(line string) []string {
	line = strings.TrimLeft(line, spaceCutset+zeroWidthCutset)
	var segments []string
	for start := 0; start < len(line); {
		gapAt, gapEnd := nextWideGap(line[start:])
		if gapAt < 0 {
			return append(segments, line[start:])
		}
		if segment := strings.TrimRight(line[start:start+gapAt], spaceCutset); segment != "" {
			segments = append(segments, segment)
		}
		start += gapEnd
	}
	return segments
}

const (
	spaceCutset     = " \t\v\f\r\u00a0\u2007\u202f\u2009\u2002\u2003"
	separatorCutset = "\t\v\f\r"
	zeroWidthCutset = "\ufeff\u200b\u200c\u200d\u200e\u200f"
)

// nextWideGap returns the byte bounds of the first gap in line that joins two
// printed runs rather than separating two words of one.
func nextWideGap(line string) (start, end int) {
	for offset, r := range line {
		if offset < start || !strings.ContainsRune(spaceCutset, r) {
			continue
		}
		width, runes, separator := 0, 0, false
		for _, gap := range line[offset:] {
			if !strings.ContainsRune(spaceCutset, gap) {
				break
			}
			if strings.ContainsRune(separatorCutset, gap) {
				separator = true
			}
			width += utf8.RuneLen(gap)
			runes++
		}
		if separator || runes > 1 {
			return offset, offset + width
		}
		start = offset + width
	}
	return -1, -1
}

// correctionMarkerAt reports the marker one segment declares, or "" for a
// segment that declares none.
//
// A pointer phrase suppresses the marker, because a correction published
// elsewhere is announced ON the corrected work — Springer prints "Erratum to
// this chapter is available at …" on the chapter itself — and reading that as a
// self-declaration would cap the pass of the very document the operator asked
// for. It suppresses only when no colon follows the phrase: "Erratum to this
// article: <title>" is a real erratum heading convention, so the colon is what
// separates a document announcing itself from one pointing elsewhere.
func correctionMarkerAt(segment string) string {
	for _, phrase := range correctionPointerPhrases {
		if !strings.HasPrefix(segment, phrase) {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(segment[len(phrase):], spaceCutset), ":") {
			break
		}
		return ""
	}
	for _, marker := range correctionMarkers {
		if strings.HasPrefix(segment, marker) {
			return marker
		}
	}
	return ""
}

func normalizeDOI(v string) string {
	n, err := work.NormalizeDOI(v)
	if err != nil {
		return ""
	}
	// Legacy APA PDFs print an extra slash after the registrant
	// (for example 10.1037//0021-9010.87.4.611), while Crossref and modern
	// resolvers identify the same work with one. Collapse that leading suffix
	// slash for identity comparison only; the canonical work identifier remains
	// untouched elsewhere.
	prefix, suffix, ok := strings.Cut(n, "/")
	if !ok {
		return n
	}
	return prefix + "/" + strings.TrimPrefix(suffix, "/")
}

func documentDOIs(text string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range doiPattern.FindAllString(text, -1) {
		if doi := normalizeDOI(raw); doi != "" && !seen[doi] {
			seen[doi] = true
			out = append(out, doi)
		}
	}
	return out
}

// FrontMatterDOIs returns the normalized, deduplicated DOIs printed in a
// document's front matter — the same window MatchIdentity reads its own-DOI
// evidence from. Exported for the PDF-grab pipeline (ADR-0020), which must
// identify a captured file before any job exists: there is no target
// work.Work yet to corroborate against, so corroboratingIdentifier's
// arXiv/PMID markers do not apply (they compare against a KNOWN
// identifier); inventing a target-less arXiv/PMID extractor is out of scope
// for this decision — only the DOI front-matter pattern extracts blind.
func FrontMatterDOIs(text string) []string {
	return documentDOIs(identityFrontMatter(text))
}

// identityWindow returns the head of page one, bounded by limit. Every
// front-matter rule reads one of these; only the bound differs, and each
// bound is a separately measured tradeoff.
//
// Leading blank matter is trimmed before the cut on the first form feed,
// not after: pdftotext renders a blank cover leaf as nothing but that form
// feed, so cutting on it first handed every rule below — the DOI window,
// the correction cap, the byline gate — an empty window, and a document
// with a blank first leaf parked (or passed) for whatever reason happened
// to fire on no evidence at all rather than on whatever the page that
// follows actually says. The cutset also strips a leading byte-order mark,
// which strings.TrimSpace does not remove, since some extractors emit one
// ahead of the leaf itself.
func identityWindow(text string, limit int) string {
	text = strings.TrimLeft(text, " \t\r\n\f\ufeff")
	if firstPage, _, ok := strings.Cut(text, "\f"); ok {
		text = firstPage
	}
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

const identityFrontMatterBytes = 1 << 10

func identityFrontMatter(text string) string { return identityWindow(text, identityFrontMatterBytes) }

// identityBylineBytes is wider than the DOI window above because the two
// answer different questions. A document's own DOI must sit at the very top or
// it is probably a reference; a byline legitimately runs long — a dozen authors
// with affiliation footnotes routinely pushes past a kilobyte. Widening this
// further starts pulling the opening paragraphs in, which is where a
// mismatched document begins to look like a match again: measured over 1560
// mismatched pairs, 2 KiB admitted none and 4 KiB admitted two.
//
// correctionMarkerIn scans this same window, for a matching reason: widening
// it to the 4 KiB page-one window would admit two real documents from a
// 679-document library whose page one carries correction vocabulary that is
// not a self-declaration — a Springer footnote pointing at an erratum
// published elsewhere, at offset 1874, and an ordinary sentence about
// dendritic retraction in unrelated neuroscience prose, at offset 2958. The
// pointer-phrase exclusion handles the first inside this window; the second
// stays inert only because it never reaches the window at all.
const identityBylineBytes = 2 << 10

func identityByline(text string) string { return identityWindow(text, identityBylineBytes) }

// identityPageOneBytes is the widest of the three: it bounds where a document
// may print its OWN identifier, which a publisher puts wherever page one has
// room — a footer, a masthead, or under the abstract past the correspondence
// footnote (byte 2377, form feed at 2403, for the Wiley paper that motivated
// it). The cap bites on a page one longer than 4 KiB — a dense two-column page
// whose columns pdftotext concatenates — and on a document that emits no form
// feed at all, where it is the only thing keeping a short comment or erratum
// from donating its reference list to this window. Both cases park a document
// this rule would otherwise have accepted, which is the direction to err in.
const identityPageOneBytes = 4 << 10

func identityPageOne(text string) string { return identityWindow(text, identityPageOneBytes) }

// IdentityWindows exposes the three windows the rules above read, so the corpus
// harness in internal/identitycorpus can report where real papers print their
// own identifier without keeping a second copy of these bounds. A histogram
// derived from a divergent copy would be measuring the wrong thing at exactly
// the moment someone is retuning a bound.
func IdentityWindows(text string) (frontMatter, byline, pageOne string) {
	return identityFrontMatter(text), identityByline(text), identityPageOne(text)
}

// IdentifierPrinted reports whether text prints an identifier the rules would
// accept as corroboration for target. The corpus harness asks this question to
// classify WHERE a real paper prints its own identifier, and asking it through
// the same code the verdict uses is the point: a histogram built on a bare
// substring scan counts a DOI the matcher rejects as unusable, and misses one
// the matcher finds through letter-spacing, so it would report a window gap
// where the truth is unusable metadata.
func IdentifierPrinted(text string, target work.Work) bool {
	return corroboratingIdentifier(text, target) != ""
}

// containsFlattenedToken reports whether text contains needle as a COMPLETE
// identifier once whitespace is ignored. Publishers letter-space identifiers
// for typographic effect — ACM prints a DOI as "DOI:10.1145/ 30 6 5 3 8 6" —
// and pdftotext preserves those gaps, so a plain substring or regex scan misses
// an identifier that is right there on the page.
//
// The trailing delimiter check is load-bearing, not tidiness: without it PMID
// 12345 is "found" in a document printing PMID:123456, and DOI 10.1/foo in one
// printing 10.1/foobar. Since a corroborated identifier is strong evidence for
// acceptance, a prefix collision would file the wrong PDF. Scans in place
// rather than building a flattened copy; this runs over whole documents.
func containsFlattenedToken(text, needle string) bool {
	if needle == "" {
		return false
	}
	for start := range len(text) {
		if isIdentitySpace(rune(text[start])) || !identifierBoundary(text, start) {
			continue
		}
		i, matched := start, 0
		for i < len(text) && matched < len(needle) {
			r, size := utf8.DecodeRuneInString(text[i:])
			if isIdentitySpace(r) {
				i += size
				continue
			}
			if size != 1 || lowerASCII(text[i]) != needle[matched] {
				break
			}
			i++
			matched++
		}
		// The boundary is the byte immediately after the last matched
		// character, with no whitespace skipped: a space ENDS the identifier.
		// Skipping it would defeat the whole point here, because a
		// letter-spaced DOI is always followed by whitespace and then the next
		// word, so every real match would be read as running into it.
		if matched == len(needle) && !identifierContinues(text, i) {
			return true
		}
	}
	return false
}

// identifierBoundary reports whether position i starts a token rather than
// continuing one, so "xx10.1/foo" cannot corroborate DOI 10.1/foo. Only a
// letter or digit continues a token here: an identifier is routinely preceded
// by punctuation that belongs to its label, as in "DOI:10.1145/3065386" or
// "doi.org/10.1145/3065386", and treating those as continuations would reject
// every real citation.
func identifierBoundary(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// identifierContinues reports whether the text at i extends the identifier that
// just matched. This is what stops PMID 12345 being "found" in PMID:123456 and
// DOI 10.1/foo in 10.1/foobar — a prefix collision that, since a corroborated
// identifier is strong evidence, would file the wrong PDF.
//
// A '.' is ambiguous: it is legal inside a DOI suffix and it is also how a
// sentence ends. It only continues the identifier when something identifier-ish
// follows it, so "…10.1/foo." at the end of a sentence still corroborates.
func identifierContinues(text string, i int) bool {
	if i >= len(text) {
		return false
	}
	r, size := utf8.DecodeRuneInString(text[i:])
	switch {
	case unicode.IsLetter(r) || unicode.IsDigit(r), r == '-', r == '_', r == '/':
		return true
	case r == '.':
		next, _ := utf8.DecodeRuneInString(text[i+size:])
		return unicode.IsLetter(next) || unicode.IsDigit(next)
	}
	return false
}

// isIdentitySpace also covers the Unicode separators publishers use for
// letter-spacing — NO-BREAK SPACE and THIN SPACE appear in typeset DOIs — which
// a byte-wise ASCII check would break on mid-identifier.
func isIdentitySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v', '\u00a0', '\u2007', '\u2009', '\u200a', '\u202f', '\u2060', '\ufeff':
		return true
	}
	return unicode.IsSpace(r)
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// corroboratingIdentifier reports the strong identifier the document itself
// prints, if any. The front-matter rules deliberately read only the top of page
// one so a reference-list DOI is never mistaken for the document's own, which
// also means they miss a DOI printed in a running footer or below the abstract
// — 17 of 40 real papers in one library. Searching the whole document is safe
// here only because the caller has already cleared the title gate.
func corroboratingIdentifier(text string, target work.Work) string {
	if doi := normalizeDOI(target.DOI); doi != "" && containsFlattenedToken(text, doi) {
		return "document prints the requested DOI: " + doi
	}
	// arXiv stamps the versioned form ("arXiv:1404.7828v4") while catalogues
	// record the bare id; they identify the same work, so the version suffix is
	// read as part of the token rather than as a different identifier.
	if arxiv := strings.ToLower(strings.TrimSpace(target.ArXiv)); arxiv != "" {
		if containsFlattenedToken(text, "arxiv:"+arxiv) {
			return "document prints the requested arXiv id: " + arxiv
		}
		for version := 1; version <= 9; version++ {
			if containsFlattenedToken(text, fmt.Sprintf("arxiv:%sv%d", arxiv, version)) {
				return fmt.Sprintf("document prints the requested arXiv id: %s (v%d)", arxiv, version)
			}
		}
	}
	if pmid := strings.TrimSpace(target.PMID); pmid != "" && containsFlattenedToken(text, "pmid:"+pmid) {
		return "document prints the requested PMID: " + pmid
	}
	return ""
}

func identityTitleTokens(title string) []string {
	fields := normalizedTokens(title)
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		if len([]rune(f)) < 5 || titleStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// familyToken returns the surname papio will look for in a byline. Matching ANY
// token of a name let given names carry the author check: "david", "john",
// "robert", and even an organisational "the" appear in almost every paper's
// reference list, which is what made 155 of 1560 mismatched pairs pass. Only
// the family name discriminates.
//
// Zotero stores "First Last", but RIS, BibTeX, and NBIB ingestion preserve
// "Last, First" verbatim (internal/ingest), so taking the last token blindly
// would look for the GIVEN name of every imported reference and review correct
// documents. The comma decides which half is the surname.
func familyToken(author string) string {
	if surname, _, ok := strings.Cut(author, ","); ok {
		if token := lastSignificantToken(normalizedTokens(surname)); token != "" {
			return token
		}
	}
	return lastSignificantToken(normalizedTokens(author))
}

func lastSignificantToken(fields []string) string {
	for i := len(fields) - 1; i >= 0; i-- {
		if len([]rune(fields[i])) >= 3 && !titleStopwords[fields[i]] {
			return fields[i]
		}
	}
	return ""
}

func bylineHasExactly(byline map[string]struct{}, family string) bool {
	_, ok := byline[family]
	return ok
}

// bylineMarkedSurname tolerates the superscript markers pdftotext glues onto a
// byline surname — "Alejandro Barredo Arrietaa", "Siham Tabikg", "Keith D.
// Ciani1". Every author of one real 12-author paper was marked this way, so
// exact matching alone failed a byline that was in fact perfect.
//
// marked is reported separately from an exact hit because the tolerance cannot
// tell a LETTERED marker from a different surname: "Clarke" is "Clark" plus one
// letter. The caller therefore requires two such matches, or an exact one,
// before treating authorship as established.
//
// numeric reports the unambiguous case, which needs no second opinion: no
// surname ends in a digit, so a token that is the requested surname followed
// only by digits is that surname carrying an affiliation number.
func bylineMarkedSurname(byline map[string]struct{}, family string) (marked, numeric bool) {
	if len([]rune(family)) < 5 {
		return false, false
	}
	for token := range byline {
		if len(token) <= len(family) || len(token) > len(family)+2 || !strings.HasPrefix(token, family) {
			continue
		}
		marked = true
		if isASCIIDigits(token[len(family):]) {
			return true, true
		}
	}
	return marked, false
}

func isASCIIDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) != 0
}

// bylineYears reports whether the front matter states a publication year at
// all. It distinguishes "this document is dated differently to the request"
// from "this document prints no date", which are opposite kinds of evidence.
func bylineYears(byline string) bool {
	return bylineYearPattern.MatchString(byline)
}

var bylineYearPattern = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)

// documentTokens folds text into a lookup set once. Membership used to
// re-tokenize the whole document per candidate token, walking and reallocating
// a long paper a dozen times for one decision.
func documentTokens(text string) map[string]struct{} {
	fields := normalizedTokens(text)
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	return set
}

func countTokens(set map[string]struct{}, tokens []string) int {
	n := 0
	for _, token := range tokens {
		if _, ok := set[token]; ok {
			n++
		}
	}
	return n
}

// typographicFolder repairs the two artefacts a PDF text layer reliably
// introduces into words, both of which silently cost title-token matches:
// justified text hyphenates across a line break ("classifi-\ncation"), and some
// producers keep the ligature codepoints ("classiﬁcation"). Neither survives
// tokenization as the word the title actually contains.
var typographicFolder = strings.NewReplacer(
	"-\r\n", "", "-\n", "", "\u00ad\n", "", "\u00ad", "",
	"\ufb00", "ff", "\ufb01", "fi", "\ufb02", "fl", "\ufb03", "ffi", "\ufb04", "ffl", "\ufb05", "st", "\ufb06", "st",
)

func normalizedTokens(s string) []string {
	folded := strings.ToLower(typographicFolder.Replace(s))
	return strings.FieldsFunc(folded, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func pass(evidence ...string) IdentityDecision {
	return IdentityDecision{Result: IdentityPass, Evidence: evidence}
}
func reject(evidence ...string) IdentityDecision {
	return IdentityDecision{Result: IdentityReject, Evidence: evidence}
}
func review(evidence ...string) IdentityDecision {
	return IdentityDecision{Result: IdentityReview, Evidence: evidence}
}
