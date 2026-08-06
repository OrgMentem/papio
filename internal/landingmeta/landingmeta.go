// Package landingmeta extracts the citation_pdf_url a publisher landing page
// advertises.
//
// The incident that motivated this package: DOI 10.3389/feduc.2018.00095 is
// fully open access. Both Unpaywall and OpenAlex pointed at the same Azure
// blob URL with an expired SAS signature; both fetches 403'd, both
// candidates were correctly marked invalid, and the job then wasted 3 days
// parking and retrying an institutional handoff for a paper that needed no
// institution — while the candidate rows' own landing_redacted URL
// (doi.org, which redirects to the publisher) was sitting right there
// advertising a working, unauthenticated PDF via citation_pdf_url. This
// package is what reads that tag so a dead publisher link can fall back to
// the landing page instead of an institutional OpenURL.
package landingmeta

import (
	"bytes"
	"errors"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ErrConflictingPDFURL reports two citation_pdf_url tags with different
// resolved values.
var ErrConflictingPDFURL = errors.New("conflicting citation_pdf_url")

// metaName is the Highwire Press tag Unpaywall, OpenAlex and the extension's
// own landing-page classifier all key off of.
const metaName = "citation_pdf_url"

// maxCandidates bounds how many name="citation_pdf_url" tags we'll look at
// before giving up on the page. The incident had exactly one real tag; a
// page carrying hundreds is not a landing page, it's an attempt to make this
// loop do unbounded work on hostile input the caller already fetched over
// the network.
const maxCandidates = 512

// PDFURL extracts the citation_pdf_url a publisher landing page advertises.
// Returns ("", nil) when the page advertises none. base is the FINAL landing
// URL after redirects; relative content resolves against it.
//
// PDFURL does no I/O: it tokenizes the given bytes directly rather than
// building a full document tree, which is what lets it enforce the head
// bound below without first paying to parse whatever the caller downloaded.
// Fetch size limits and redirect policy belong to the caller.
func PDFURL(htmlBytes []byte, base *url.URL) (string, error) {
	z := html.NewTokenizer(bytes.NewReader(htmlBytes))

	var resolved []string // deduplicated by appendResolved
	candidates := 0

scan:
	for {
		switch z.Next() {
		case html.ErrorToken:
			// EOF (or a malformed-enough document the tokenizer gave up on)
			// — either way, nothing more to scan.
			break scan

		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "head" {
				// citation_pdf_url is a head tag by convention (Highwire
				// Press, and every publisher we've seen). Once head closes
				// we stop scanning outright rather than continuing into
				// body — that's the other half of the bound: a document
				// that never closes head is still capped by maxCandidates
				// above, and one with no head tag at all just scans to EOF
				// like any other bounded-by-input-size pass.
				break scan
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			tag, hasAttr := z.TagName()
			if string(tag) != "meta" {
				continue
			}

			var name, content string
			var hasContent bool
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				switch string(key) {
				case "name":
					name = string(val)
				case "content":
					content, hasContent = string(val), true
				}
			}

			// Byte-exact, case-sensitive: mirrors the extension's
			// querySelector('meta[name="citation_pdf_url"]'), which is
			// exact by construction. citation_pdf_URL and friends are
			// silently not this tag, same as they aren't a CSS attribute
			// match.
			if name != metaName {
				continue
			}

			candidates++

			if hasContent {
				if u := resolveCandidate(base, content); u != "" {
					resolved = appendResolved(resolved, u)
				}
			}

			if candidates >= maxCandidates {
				break scan
			}
		}
	}

	switch len(resolved) {
	case 0:
		return "", nil
	case 1:
		return resolved[0], nil
	default:
		return "", ErrConflictingPDFURL
	}
}

// resolveCandidate resolves a citation_pdf_url content value against base,
// returning "" for anything that isn't a usable absolute https URL: empty
// (or whitespace-only) content, content that fails to parse, and non-https
// schemes (http, file, data, javascript, ...) are all rejected here rather
// than surfaced as errors, because none of them are the caller's problem —
// they're just this tag saying "no PDF here."
func resolveCandidate(base *url.URL, content string) string {
	// WHATWG URL parsing (what the extension's `new URL()` does, and what
	// we mirror here) trims leading/trailing ASCII whitespace before
	// parsing; net/url does not, and errors on the embedded control
	// characters a trailing newline leaves behind. Trim first.
	trimmed := strings.TrimFunc(content, isASCIISpace)
	if trimmed == "" {
		// An empty content resolves against base to the landing page
		// itself, not a PDF — e.g. the extension's own extractMetaURL,
		// which has no such guard, does exactly this and hands the landing
		// HTML to its downloader as if it were the PDF (see
		// testdata/contract.json's empty_content case). Reject before we'd
		// make the same mistake.
		return ""
	}

	u, err := base.Parse(trimmed)
	if err != nil {
		return ""
	}
	if u.Scheme != "https" {
		return ""
	}
	return u.String()
}

// appendResolved adds u to resolved unless it's already present, preserving
// PDFURL's "identical duplicates aren't a conflict" rule without needing a
// set for what is, in practice, at most a handful of entries.
func appendResolved(resolved []string, u string) []string {
	for _, existing := range resolved {
		if existing == u {
			return resolved
		}
	}
	return append(resolved, u)
}

func isASCIISpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}
