// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"fmt"
	"strings"
	"unicode"

	"papio/internal/work"
)

// isbnFrontBytes bounds where a whole book may print its own ISBN.
//
// The page windows above are one printed page, which is right for an article
// and wrong for a book: the copyright page that carries the ISBN follows the
// cover and title pages. Four kilobytes of extracted text reaches the
// copyright page of a book with a few pages of front matter and stops before
// its contents list and first chapter, where another book's ISBN would be
// cited rather than printed.
const isbnFrontBytes = 4 << 10

// referenceHeadings are whole lines (lower-cased, punctuation dropped) that
// introduce a list of OTHER works. Text after one is cited, not the document's
// own, so neither a title nor an ISBN printed after it counts.
var referenceHeadings = map[string]bool{
	"references": true, "reference": true, "bibliography": true, "works cited": true,
	"cited works": true, "citations": true, "further reading": true, "see also": true,
	"related publications": true, "related titles": true, "other publications": true,
	"other titles": true, "also available": true, "previous editions": true,
	"forthcoming titles": true, "in this series": true, "other books in this series": true,
}

func isReferenceHeading(words []string) bool {
	return referenceHeadings[strings.Join(words, " ")]
}

// MatchISBNBoundIdentity judges a whole-book file against a candidate a
// repository bound to boundISBN: the repository's own record, which lists that
// ISBN, holds this file. target carries the title to look for — the
// requester's, or the record's when the requester gave an ISBN alone.
//
// A Pass needs the document itself to agree with the repository's claim, and
// the claim is never enough alone. It is returned only when ALL hold:
//
//   - the front matter prints the bound ISBN, hyphenated or not, as an
//     ISBN-13 or its ISBN-10 form, before any references-type heading (a
//     neighbouring edition has the same title and a different ISBN, so it is
//     parked); and
//   - the COMPLETE title, every word including short words and numbers, is
//     printed on the FIRST page — the cover or title page, the only place a
//     document names itself rather than another work — as its own run of
//     lines, beginning and ending on line edges, with no references-type
//     heading above it on that page; and
//   - the article rules (MatchIdentityWithThreshold) pass the file against
//     target, or the requester named no authors and the byline carries no
//     correction marker.
//
// A Reject from the article rules stands. Everything else is a Review, so a
// caller may adopt a Pass from here and otherwise keep what it already had.
func MatchISBNBoundIdentity(text string, target work.Work, boundISBN string, titleThreshold float64) IdentityDecision {
	base := MatchIdentityWithThreshold(text, target, titleThreshold)
	if base.Result == IdentityReject {
		return base
	}
	printed, printsISBN := frontMatterPrintsISBN(isbnFront(text), boundISBN)
	if !printsISBN {
		if base.Result == IdentityPass {
			return review(append(append([]string(nil), base.Evidence...),
				fmt.Sprintf("repository record binds this file to ISBN %s, but the front matter does not print it", boundISBN))...)
		}
		return base
	}
	bound := []string{
		fmt.Sprintf("repository record binds this file to ISBN %s", boundISBN),
		"document front matter prints the bound ISBN: " + printed,
	}
	if !standaloneTitlePrinted(firstPageOf(text), target.Title) {
		if base.Result == IdentityPass {
			return review(append(append([]string(nil), base.Evidence...),
				"the complete requested title is not printed as its own lines on the first page")...)
		}
		return base
	}
	title := "complete requested title printed as its own lines on the first page"
	if base.Result == IdentityPass {
		return pass(append(append([]string(nil), base.Evidence...), append(bound, title)...)...)
	}
	if len(target.Authors) != 0 {
		return base
	}
	if correctionMarkerIn(strings.Split(typographicFolder.Replace(identityByline(text)), "\n")) != "" {
		return base
	}
	return pass(append(bound, title)...)
}

// isbnFront is the head of the document in which the bound ISBN may be printed:
// the first isbnFrontBytes of text, cut at the first references-type heading.
func isbnFront(text string) string {
	if len(text) > isbnFrontBytes {
		text = text[:isbnFrontBytes]
	}
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if isReferenceHeading(normalizedTokens(line)) {
			return text[:offset]
		}
		offset += len(line)
	}
	return text
}

// firstPageOf is the document's first printed page: the text before the first
// form feed, after any blank leading leaves, bounded like the other windows.
func firstPageOf(text string) string { return identityWindow(text, isbnFrontBytes) }

// standaloneTitlePrinted reports whether the whole title is printed on the page
// as a run of consecutive lines that begins and ends on line edges, with no
// references-type heading above it. Every word counts, short words and numbers
// included, because those are what separate volumes and editions. Letter-spaced
// display type ("Me n t a l / He a l t h") is read through: words are matched
// by their characters, not as units.
func standaloneTitlePrinted(page, title string) bool {
	phrase := identityTitlePhrase(title)
	if phrase == "" {
		return false
	}
	segments := bylineSegments(page)
	for start := range segments {
		if introducedByReferenceHeading(segments[:start]) {
			return false
		}
		if standaloneRunAt(segments[start:], phrase) {
			return true
		}
	}
	return false
}

// standaloneRunAt matches phrase against whole consecutive segments from the
// first, and requires the last matched segment to be consumed to its end: a
// title followed by more words on its line is a citation or a longer title.
func standaloneRunAt(segments []titleSegment, phrase string) bool {
	matched := 0
	for _, segment := range segments {
		for j, word := range segment.words {
			if !strings.HasPrefix(phrase[matched:], word) {
				return false
			}
			matched += len(word)
			if matched == len(phrase) && j != len(segment.words)-1 {
				return false
			}
		}
		if matched == len(phrase) {
			return true
		}
	}
	return false
}

// introducedByReferenceHeading reports a references-type heading among the lines
// that precede a title run on its page.
func introducedByReferenceHeading(before []titleSegment) bool {
	for _, segment := range before {
		if isReferenceHeading(segment.words) {
			return true
		}
	}
	return false
}

// frontMatterPrintsISBN reports whether text prints isbn, and in what form. An
// ISBN is printed hyphenated ("978-92-4-011448-7"), so hyphens between digits
// (or before a terminal check letter X) are read through before the digits are
// compared; spaces are not, because a year or a page number set before an ISBN
// would otherwise fuse with it. The match must be a whole run, so a longer
// number containing the ISBN's digits is not the ISBN.
func frontMatterPrintsISBN(text, isbn string) (string, bool) {
	normalized, err := work.NormalizeISBN(isbn)
	if err != nil {
		return "", false
	}
	forms := []string{normalized}
	if thirteen, ok := work.ISBN13(normalized); ok {
		forms = []string{thirteen}
		if ten, ok := isbn10Of(thirteen); ok {
			forms = append(forms, ten)
		}
	}
	collapsed := strings.ToUpper(collapseDigitHyphens(typographicFolder.Replace(text)))
	for _, form := range forms {
		if containsDigitRun(collapsed, form) {
			return form, true
		}
	}
	return "", false
}

// isbn10Of derives the ISBN-10 of a 978-prefixed ISBN-13; a 979 prefix has none.
func isbn10Of(thirteen string) (string, bool) {
	if len(thirteen) != 13 || !strings.HasPrefix(thirteen, "978") {
		return "", false
	}
	body := thirteen[3:12]
	sum := 0
	for i := range 9 {
		sum += (10 - i) * int(body[i]-'0')
	}
	check := (11 - sum%11) % 11
	if check == 10 {
		return body + "X", true
	}
	return body + string(rune('0'+check)), true
}

// collapseDigitHyphens drops every hyphen (and dash variant) that sits between
// two digits, or between a digit and a terminal check letter X, leaving all
// other text untouched.
func collapseDigitHyphens(text string) string {
	runes := []rune(text)
	var b strings.Builder
	b.Grow(len(text))
	for i, r := range runes {
		if isHyphen(r) && i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) {
			next := runes[i+1]
			terminalX := (next == 'X' || next == 'x') && (i+2 == len(runes) || !isLetterOrDigit(runes[i+2]))
			if unicode.IsDigit(next) || terminalX {
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLetterOrDigit(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isHyphen(r rune) bool {
	switch r {
	case '-', '\u2010', '\u2011', '\u2012', '\u2013', '\u2014':
		return true
	}
	return false
}

// containsDigitRun reports whether text holds digits as a whole run: the byte
// before and after must not be a digit.
func containsDigitRun(text, digits string) bool {
	for start := 0; start < len(text); {
		at := strings.Index(text[start:], digits)
		if at < 0 {
			return false
		}
		at += start
		end := at + len(digits)
		if (at == 0 || !isASCIIDigit(text[at-1])) && (end == len(text) || !isASCIIDigit(text[end])) {
			return true
		}
		start = at + 1
	}
	return false
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }
