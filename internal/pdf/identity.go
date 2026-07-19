// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"

	"papio/internal/work"
)

var doiPattern = regexp.MustCompile(`(?i)\b10\.\d{4,9}/[-._;()/:a-z0-9]+`)

var nonArticleMarkers = []string{
	"supporting information", "supplementary information", "supplementary material",
	"supplemental information", "supplemental material", "online appendix",
	"electronic supplementary", "supporting data",
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
func MatchIdentityWithThreshold(text string, target work.Work, titleThreshold float64) IdentityDecision {
	if titleThreshold <= 0 || titleThreshold > 1 {
		titleThreshold = 0.6
	}
	frontMatter := identityFrontMatter(text)
	for _, line := range strings.Split(frontMatter, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		for _, marker := range nonArticleMarkers {
			if strings.HasPrefix(line, marker) {
				return reject("non-article marker: " + marker)
			}
		}
	}

	haystack := strings.ToLower(text)
	wantDOI := normalizeDOI(target.DOI)
	gotDOIs := documentDOIs(frontMatter)
	if wantDOI != "" && len(gotDOIs) != 0 {
		for _, gotDOI := range gotDOIs {
			if gotDOI == wantDOI {
				return pass("exact normalized DOI match: " + wantDOI)
			}
		}
		return reject("document DOI does not match requested DOI", "document DOI: "+strings.Join(gotDOIs, ", "))
	}

	tokens := identityTitleTokens(target.Title)
	if len(tokens) == 0 {
		return review("no usable requested DOI or title tokens")
	}
	matches := 0
	for _, token := range tokens {
		if containsToken(haystack, token) {
			matches++
		}
	}
	need := int(math.Ceil(float64(len(tokens)) * titleThreshold))
	if need < 1 {
		need = 1
	}
	if matches < need {
		return reject(fmt.Sprintf("title token evidence insufficient: %d/%d", matches, need))
	}

	evidence := []string{fmt.Sprintf("title tokens matched: %d/%d", matches, len(tokens))}
	authorOK := len(target.Authors) == 0
	if !authorOK {
		for _, author := range target.Authors {
			for _, token := range authorTokens(author) {
				if containsToken(haystack, token) {
					authorOK = true
					evidence = append(evidence, "author token matched: "+token)
					break
				}
			}
			if authorOK {
				break
			}
		}
	}
	yearOK := target.Year == 0 || strings.Contains(haystack, fmt.Sprint(target.Year))
	if target.Year != 0 && yearOK {
		evidence = append(evidence, "year matched")
	}
	if authorOK && yearOK {
		return IdentityDecision{Result: IdentityPass, Evidence: evidence}
	}
	if !authorOK {
		evidence = append(evidence, "no requested author token found")
	}
	if !yearOK {
		evidence = append(evidence, "requested year not found")
	}
	return IdentityDecision{Result: IdentityReview, Evidence: evidence}
}

// IdentityMatch is an alias for MatchIdentity.
func IdentityMatch(text string, target work.Work) IdentityDecision {
	return MatchIdentity(text, target)
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

const identityFrontMatterBytes = 1 << 10

func identityFrontMatter(text string) string {
	if firstPage, _, ok := strings.Cut(text, "\f"); ok {
		text = firstPage
	}
	if len(text) > identityFrontMatterBytes {
		return text[:identityFrontMatterBytes]
	}
	return text
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

func authorTokens(author string) []string {
	fields := normalizedTokens(author)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

func normalizedTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func containsToken(text, token string) bool {
	for _, got := range normalizedTokens(text) {
		if got == token {
			return true
		}
	}
	return false
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
