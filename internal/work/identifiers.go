// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package work normalizes requested-work identity. Rules and edge cases follow
// the instsci behavioral reference (DOI/arXiv parsing) and Crossref/arXiv
// documentation; the fixture tests are the contract.
package work

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Work is the normalized identity a resolver receives.
type Work struct {
	DOI       string   `json:"doi,omitempty"`
	PMID      string   `json:"pmid,omitempty"`
	ArXiv     string   `json:"arxiv,omitempty"`
	ISBN      string   `json:"isbn,omitempty"`
	OpenAlex  string   `json:"openalex,omitempty"`
	Title     string   `json:"title,omitempty"`
	Authors   []string `json:"authors,omitempty"`
	Container string   `json:"container,omitempty"`
	Year      int      `json:"year,omitempty"`
}

// HasIdentifier reports whether any strong identifier is present.
func (w Work) HasIdentifier() bool {
	return w.DOI != "" || w.PMID != "" || w.ArXiv != "" || w.ISBN != "" || w.OpenAlex != ""
}

// HasFetchableIdentifier reports whether this work carries an identifier some
// acquisition route can actually resolve to full text.
//
// ISBN is deliberately excluded even though HasIdentifier counts it. An ISBN
// names an edition, and the only editions papio can fetch are the rare open
// ones: just the open_books resolver consumes it, and only for a work whose
// sole strong identifier is the ISBN (OpenBooksEligible). An institutional
// OpenURL built from an ISBN reaches a catalogue record or DRM'd ebook reader,
// not a PDF papio can automatically validate. Explicit human-assisted
// institutional handoff may still use ISBN book metadata without changing
// this predicate.
func (w Work) HasFetchableIdentifier() bool {
	return w.DOI != "" || w.PMID != "" || w.ArXiv != "" || w.OpenAlex != ""
}

// OpenBooksEligible reports whether the open-book ISBN resolver may act on this
// work: an ISBN is present and no other strong identifier is. An ISBN names a
// whole edition, and a chapter that also has a DOI must be fetched as the
// chapter (ownership treats ISBN as non-identifying for the same reason,
// ADR-0008), so a work that carries any fetchable identifier is never looked up
// by its ISBN.
func (w Work) OpenBooksEligible() bool {
	return w.ISBN != "" && !w.HasFetchableIdentifier()
}

// Describe renders a short human label for logs/CLI (never secret-bearing).
func (w Work) Describe() string {
	switch {
	case w.DOI != "":
		return "doi:" + w.DOI
	case w.ArXiv != "":
		return "arxiv:" + w.ArXiv
	case w.PMID != "":
		return "pmid:" + w.PMID
	case w.OpenAlex != "":
		return "openalex:" + w.OpenAlex
	case w.ISBN != "":
		return "isbn:" + w.ISBN
	case w.Title != "":
		if len(w.Title) > 60 {
			return "title:" + w.Title[:60] + "…"
		}
		return "title:" + w.Title
	default:
		return "<empty work>"
	}
}

var (
	doiCoreRE = regexp.MustCompile(`^10\.[0-9]{4,9}/\S{1,200}$`)
	pmidRE    = regexp.MustCompile(`^[0-9]{1,10}$`)
	// arXiv new style: YYMM.NNNN(N) with optional version.
	arxivNewRE = regexp.MustCompile(`^([0-9]{4}\.[0-9]{4,5})(v[0-9]+)?$`)
	// arXiv old style: archive(.SUB)/YYMMNNN with optional version.
	arxivOldRE   = regexp.MustCompile(`^([a-z-]+(?:\.[A-Z]{2})?/[0-9]{7})(v[0-9]+)?$`)
	openalexRE   = regexp.MustCompile(`^W[0-9]{4,12}$`)
	isbnDigitsRE = regexp.MustCompile(`^[0-9]{9}[0-9Xx]$|^[0-9]{13}$`)
)

// trailingPunct are characters commonly glued onto DOIs by prose, filenames,
// and markup; stripped iteratively from the right.
const trailingPunct = ".,;:)]}\"'>"

// NormalizeDOI canonicalizes a DOI: strips doi:/DOI: prefixes and
// https://doi.org/ (and dx.doi.org) forms, trims trailing punctuation,
// lowercases (DOIs are case-insensitive; Crossref canonical form is lower),
// and validates the 10.xxxx/suffix shape.
func NormalizeDOI(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty DOI")
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi.org/", "doi:", "doi "} {
		if strings.HasPrefix(lower, prefix) {
			s = s[len(prefix):]
			break
		}
	}
	s = strings.TrimSpace(s)
	// URL-encoded slashes appear in OpenURL/redirect contexts.
	s = strings.ReplaceAll(s, "%2F", "/")
	s = strings.ReplaceAll(s, "%2f", "/")
	for len(s) > 0 && strings.ContainsRune(trailingPunct, rune(s[len(s)-1])) {
		s = s[:len(s)-1]
	}
	s = strings.ToLower(s)
	if !doiCoreRE.MatchString(s) {
		return "", fmt.Errorf("invalid DOI %q", raw)
	}
	return s, nil
}

// NormalizeArXiv canonicalizes an arXiv ID: accepts arXiv:/arxiv: prefixes,
// abs//pdf URLs, old and new styles; strips a trailing ".pdf" and preserves an
// explicit version suffix.
func NormalizeArXiv(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty arXiv id")
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"https://arxiv.org/abs/", "http://arxiv.org/abs/", "https://arxiv.org/pdf/", "http://arxiv.org/pdf/", "arxiv.org/abs/", "arxiv.org/pdf/", "arxiv:"} {
		if strings.HasPrefix(lower, prefix) {
			s = s[len(prefix):]
			break
		}
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, ".pdf"), "/")
	if m := arxivNewRE.FindStringSubmatch(s); m != nil {
		return m[1] + m[2], nil
	}
	if m := arxivOldRE.FindStringSubmatch(s); m != nil {
		return m[1] + m[2], nil
	}
	return "", fmt.Errorf("invalid arXiv id %q", raw)
}

// ArXivFromDOI maps the DataCite arXiv DOI form (10.48550/arxiv.<id>) to the
// bare arXiv id. Returns "" when the DOI is not an arXiv DOI.
func ArXivFromDOI(doi string) string {
	const prefix = "10.48550/arxiv."
	if !strings.HasPrefix(doi, prefix) {
		return ""
	}
	id, err := NormalizeArXiv(doi[len(prefix):])
	if err != nil {
		return ""
	}
	return id
}

// NormalizePMID validates a bare PubMed ID (digits only, no pmid: prefix kept).
func NormalizePMID(raw string) (string, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "pmid:"))
	s = strings.TrimSpace(s)
	if !pmidRE.MatchString(s) {
		return "", fmt.Errorf("invalid PMID %q", raw)
	}
	normalized := strings.TrimLeft(s, "0")
	if normalized == "" {
		// pmidRE admits "0"/"0000000000", but a PMID is a positive integer;
		// trimming leading zeros must not yield an empty identifier that would
		// build an unbounded upstream query and match an arbitrary record.
		return "", fmt.Errorf("invalid PMID %q", raw)
	}
	return normalized, nil
}

// NormalizeISBN strips hyphens/spaces and validates length (10 or 13); the
// check digit is not verified here (resolvers treat ISBN as a lookup key).
func NormalizeISBN(raw string) (string, error) {
	s := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw))
	s = strings.ToUpper(s)
	if !isbnDigitsRE.MatchString(s) {
		return "", fmt.Errorf("invalid ISBN %q", raw)
	}
	return s, nil
}

var (
	isbnParentheticalRE = regexp.MustCompile(`\([^)]*\)`)
	isbnLabelRE         = regexp.MustCompile(`(?i)\bisbn(?:-1[03])?:?`)
	isbnListSeparatorRE = regexp.MustCompile(`[;,/|\n]+`)
)

// ISBNsIn lists every ISBN a free-text catalogue value names, in order of
// appearance, each in NormalizeISBN form. Repository records carry the field
// as written by a cataloguer: "9789240114487 (electronic version)", an
// "ISBN 978-92-4-011448-7" label, or two editions in one value. A parenthetical
// qualifier, an ISBN label, and list separators are read past; anything that
// does not normalize is dropped rather than guessed at.
func ISBNsIn(value string) []string {
	cleaned := isbnParentheticalRE.ReplaceAllString(value, " ")
	cleaned = isbnLabelRE.ReplaceAllString(cleaned, " ")
	var out []string
	add := func(raw string) {
		normalized, err := NormalizeISBN(raw)
		if err == nil && !slices.Contains(out, normalized) {
			out = append(out, normalized)
		}
	}
	for _, part := range isbnListSeparatorRE.Split(cleaned, -1) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// A hyphenated or spaced ISBN ("978 92 4 011448 7") is one value; two
		// bare ISBNs in one value are not, so try the whole part first.
		if normalized, err := NormalizeISBN(part); err == nil {
			add(normalized)
			continue
		}
		for _, field := range strings.Fields(part) {
			add(field)
		}
	}
	return out
}

// SameISBN reports whether two ISBNs name the same edition: equal after
// NormalizeISBN, or an ISBN-10 and an ISBN-13 whose check digits are valid and
// which convert to the same 978-prefixed ISBN-13. A value that does not
// normalize is never the same as anything, including itself.
func SameISBN(a, b string) bool {
	na, errA := NormalizeISBN(a)
	nb, errB := NormalizeISBN(b)
	if errA != nil || errB != nil {
		return false
	}
	if na == nb {
		return true
	}
	ca, okA := ISBN13(na)
	cb, okB := ISBN13(nb)
	return okA && okB && ca == cb
}

// ISBN13 converts a normalized ISBN to its ISBN-13 form. It reports false when
// the check digit is wrong, because converting an unchecked ISBN-10 would mint
// a valid-looking ISBN-13 for a number nobody assigned.
func ISBN13(normalized string) (string, bool) {
	switch len(normalized) {
	case 13:
		for i := range 13 {
			if normalized[i] < '0' || normalized[i] > '9' {
				return "", false
			}
		}
		if isbn13CheckDigit(normalized[:12]) != normalized[12] {
			return "", false
		}
		return normalized, true
	case 10:
		sum := 0
		for i := range 10 {
			digit := int(normalized[i] - '0')
			switch {
			case normalized[i] == 'X' && i == 9:
				digit = 10
			case normalized[i] < '0' || normalized[i] > '9':
				return "", false
			}
			sum += (10 - i) * digit
		}
		if sum%11 != 0 {
			return "", false
		}
		body := "978" + normalized[:9]
		return body + string(isbn13CheckDigit(body)), true
	default:
		return "", false
	}
}

// isbn13CheckDigit computes the check digit for a twelve-digit ISBN-13 body.
func isbn13CheckDigit(body string) byte {
	sum := 0
	for i := range 12 {
		weight := 1
		if i%2 == 1 {
			weight = 3
		}
		sum += weight * int(body[i]-'0')
	}
	return byte('0' + (10-sum%10)%10)
}

// NormalizeOpenAlex validates a work id (W…). It accepts the "openalex:"
// prefix, the canonical id URL (openalex.org/W…) that OpenAlex emits in a
// record's own `id` field, and the "/works/" URL shapes served by the API and
// by the web UI — the latter being what a user pastes out of a browser.
func NormalizeOpenAlex(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	lower := strings.ToLower(s)
	for _, prefix := range []string{
		"https://openalex.org/", "http://openalex.org/",
		"https://www.openalex.org/", "http://www.openalex.org/",
		"https://api.openalex.org/", "http://api.openalex.org/",
		"openalex:",
	} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		// Every prefix is ASCII, so a match means those bytes lowercased
		// one-for-one and the offset is valid in both strings.
		s, lower = s[len(prefix):], lower[len(prefix):]
		// The API and the web UI carry a "works/" collection segment the
		// canonical id form does not; both name the same work. It is stripped
		// only once a prefix has matched, so a bare argument must still be the
		// id itself: cli.inferBareIdentifier classifies a positional argument
		// by calling this function, and "works/W…" is not a bare identifier
		// shape it should start accepting.
		if strings.HasPrefix(lower, "works/") {
			s = s[len("works/"):]
		}
		break
	}
	s = strings.ToUpper(strings.TrimSuffix(s, "/"))
	if !openalexRE.MatchString(s) {
		return "", fmt.Errorf("invalid OpenAlex work id %q", raw)
	}
	return s, nil
}
