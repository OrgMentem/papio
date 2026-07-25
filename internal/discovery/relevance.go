// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"sort"
	"strings"
	"unicode"
)

// Match kinds form the vocabulary reported on every discovered work. They are a
// stable, documented set: a consumer may switch on them, and Confident reports
// which ones mean "this really is the work you asked for".
const (
	// MatchExactTitle: the query and the title are the same text once
	// normalized. The strongest possible signal.
	MatchExactTitle = "exact_title"
	// MatchTitlePhrase: one contains the other as a contiguous phrase, so the
	// query is a title fragment or a title with trailing noise.
	MatchTitlePhrase = "title_phrase"
	// MatchTitleTokens: the query and title share most of their words but not
	// in order — a reworded or truncated title.
	MatchTitleTokens = "title_tokens"
	// MatchWeak: the backend returned this, but it does not look like the work
	// the query names. Ordinary for a keyword search; a warning sign when the
	// query was a title.
	MatchWeak = "weak"
	// MatchUnscored: no title judgement was possible or meaningful — a citation
	// snowball with no query, or a query too short to be a title.
	MatchUnscored = "unscored"
)

const (
	// minQueryTokens is the shortest query treated as a possible title. Below
	// this a query is a keyword search, where the backend's own relevance
	// (which weighs abstract, concepts and citations) beats naive title
	// comparison — so papio scores nothing and reorders nothing.
	minQueryTokens = 3
	// confidentOverlap is the token-overlap floor at which an out-of-order
	// title match counts as confident.
	confidentOverlap = 0.6
	// phraseFloor is the band boundary between contiguous and out-of-order
	// matches: a phrase match starts here and rises with coverage, while token
	// overlap is scaled into the band below. That separation is what guarantees
	// exact > phrase > tokens by score alone, so sorting on the score agrees
	// with the kind hierarchy instead of occasionally contradicting it.
	phraseFloor = 0.75
)

// Confident reports whether a match kind means the work is very likely the one
// the query named. Callers use it to decide whether to trust the top result;
// when nothing in a result set is confident, the query found no strong match.
func Confident(kind string) bool {
	switch kind {
	case MatchExactTitle, MatchTitlePhrase, MatchTitleTokens:
		return true
	default:
		return false
	}
}

// normalizeText lowercases and reduces text to alphanumeric words separated by
// single spaces, so punctuation, casing, and spacing differences between a typed
// query and a publisher's title stop mattering.
//
// Diacritics are deliberately left alone rather than folded: it would promote
// golang.org/x/text from an indirect to a direct dependency to fix a case that
// barely occurs, since queries are normally pasted from the same metadata the
// backends index.
func normalizeText(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	space := true // leading spaces are never emitted
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// score judges how well a title answers a query, returning a 0..1 score and the
// kind explaining it. The kind is the useful half: it tells a reader *why* a row
// ranked where it did, which a bare number cannot.
func score(query, title string) (float64, string) {
	normalizedQuery, normalizedTitle := normalizeText(query), normalizeText(title)
	if normalizedQuery == "" || normalizedTitle == "" {
		return 0, MatchUnscored
	}
	queryTokens := strings.Fields(normalizedQuery)
	if len(queryTokens) < minQueryTokens {
		return 0, MatchUnscored
	}
	if normalizedQuery == normalizedTitle {
		return 1, MatchExactTitle
	}
	// Containment either way: the query is a fragment of the title, or the user
	// pasted a title plus extra context. Coverage decides how much of the other
	// string the match accounted for, so a query matching most of a title
	// outranks one matching a few words of a long one.
	if strings.Contains(normalizedTitle, normalizedQuery) {
		return phraseScore(len(normalizedQuery), len(normalizedTitle)), MatchTitlePhrase
	}
	if strings.Contains(normalizedQuery, normalizedTitle) {
		return phraseScore(len(normalizedTitle), len(normalizedQuery)), MatchTitlePhrase
	}
	// Out-of-order overlap is scaled into the band below phraseFloor. Contiguity
	// is a stronger signal of identity than a shared bag of words — a pasted
	// title fragment is almost certainly the work meant, whereas a high word
	// overlap can just as easily be a different paper on the same subject. The
	// threshold for calling it confident stays on the raw measure; only the
	// reported score is banded, which is what makes score order and kind
	// strength agree.
	overlap := tokenOverlap(queryTokens, strings.Fields(normalizedTitle))
	if overlap >= confidentOverlap {
		return overlap * phraseFloor, MatchTitleTokens
	}
	return overlap * phraseFloor, MatchWeak
}

// phraseScore scales a containment match by how much of the longer string the
// shorter one covered, staying within (phraseFloor, 1).
func phraseScore(matched, whole int) float64 {
	if whole <= 0 {
		return phraseFloor
	}
	coverage := float64(matched) / float64(whole)
	return phraseFloor + (1-phraseFloor)*coverage
}

// tokenOverlap is the Sørensen–Dice coefficient over the two token sets:
// twice the shared vocabulary divided by the combined vocabulary. It rewards
// titles that share most of the query's words without demanding word order,
// and unlike a plain intersection count it is not fooled by a very long title
// that happens to contain every query word.
func tokenOverlap(queryTokens, titleTokens []string) float64 {
	if len(queryTokens) == 0 || len(titleTokens) == 0 {
		return 0
	}
	querySet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		querySet[token] = struct{}{}
	}
	titleSet := make(map[string]struct{}, len(titleTokens))
	for _, token := range titleTokens {
		titleSet[token] = struct{}{}
	}
	shared := 0
	for token := range titleSet {
		if _, ok := querySet[token]; ok {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(querySet)+len(titleSet))
}

// rank annotates every work with its match score and kind, then promotes the
// confident matches to the front.
//
// The policy is deliberately conservative: papio does not try to out-rank a
// discovery backend in general. A backend's ordering weighs the abstract,
// concepts and citation graph, which naive title comparison cannot match on a
// keyword search. What it does badly is bury an obvious title match under
// better-cited papers — a user searching an exact title found it at rank four or
// five behind unrelated high-citation reviews. So the only reordering here is:
// confident title matches first, best score first, and every non-confident row
// keeps the order the backend gave it.
func rank(works []DiscoveredWork, query string) {
	for i := range works {
		works[i].MatchScore, works[i].MatchKind = score(query, works[i].Work.Title)
	}
	sort.SliceStable(works, func(i, j int) bool {
		left, right := Confident(works[i].MatchKind), Confident(works[j].MatchKind)
		if left != right {
			return left
		}
		if !left {
			// Neither is a title match: preserve the backend's judgement.
			return false
		}
		return works[i].MatchScore > works[j].MatchScore
	})
}
