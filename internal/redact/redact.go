// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package redact sanitizes values before they reach durable storage or logs.
// Invariant 11 of the stack plan: signed query values, cookies, API keys,
// credential fields, and page bodies are never persisted.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// URL strips userinfo, query, and fragment, keeping scheme://host/path, and
// masks every token-shaped run inside a path segment. A bearer-signed URL
// therefore loses its token before persistence whether the provider put the
// grant in the query or in the path (https://cdn.example/grant/<token>/x.pdf).
// Unparseable input collapses to a fixed placeholder rather than leaking raw
// bytes.
func URL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<unparseable-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	path := maskPathTokens(u.EscapedPath())
	u.Path, u.RawPath, u.ForceQuery = "", "", false
	out := u.String() + path
	if strings.Contains(raw, "?") {
		// Mark that something was removed so operators know evidence is partial.
		return out + "?<redacted>"
	}
	return out
}

// pathTokenRE matches a token-shaped run inside one path segment: 24 or more
// contiguous URL-safe, base64, or percent-encoded characters. It mirrors the
// extension's URL_TOKEN_RE (extension/src/capture.ts) with the padding and
// escape bytes a Go-escaped path can carry. Long enough to catch signed
// grants, session ids, JWT segments, and API keys; short enough identifiers
// (PMC ids, arXiv ids, Elsevier PIIs, DOI suffixes split at dots) survive.
var pathTokenRE = regexp.MustCompile(`[A-Za-z0-9+_=%~-]{24,}`)

// semanticWordRE is one ordinary word of a hyphenated or camel-cased slug.
var semanticWordRE = regexp.MustCompile(`^[A-Za-z]{2,16}$`)

var camelBoundaryRE = regexp.MustCompile(`([a-z])([A-Z])`)

// maskPathTokens replaces token-shaped runs in each path segment with
// "<redacted>", keeping readable word slugs (download-full-text-article) as
// the extension's isSemanticSelectorToken does. Segments are checked
// independently: a slash is routing syntax, not part of a credential.
func maskPathTokens(escapedPath string) string {
	segments := strings.Split(escapedPath, "/")
	for i, segment := range segments {
		segments[i] = pathTokenRE.ReplaceAllStringFunc(segment, func(token string) string {
			if semanticSlug(token) {
				return token
			}
			return "<redacted>"
		})
	}
	return strings.Join(segments, "/")
}

func semanticSlug(token string) bool {
	words := strings.FieldsFunc(camelBoundaryRE.ReplaceAllString(token, "$1-$2"), func(r rune) bool { return r == '-' || r == '_' })
	if len(words) < 2 {
		return false
	}
	for _, word := range words {
		if !semanticWordRE.MatchString(word) {
			return false
		}
	}
	return true
}

// Host reduces a URL to scheme://host for error messages about untrusted
// destinations.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<unparseable-url>"
	}
	return u.Scheme + "://" + u.Host
}

var (
	ipv4Candidate = regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`)
	ipv6Candidate = regexp.MustCompile(`(?i)(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?`)
	versionLabel  = regexp.MustCompile(`(?i)\b(?:version|release|build|v)\s*:?\s*$`)
)

// IPAddresses replaces every IPv4 and IPv6 address in text with replacement.
// A page can print the reader's own address (Elsevier's refusal page does,
// beside its reference number), and nothing papio stores or reports needs it.
//
// The rules match extension/src/capture.ts, whose sanitizer runs first. An
// IPv4 address is four dotted octets that are not part of a longer dotted run
// or a name/version pair, so a DOI (10.1016/j.x), a date, and a user-agent
// version (Chrome/153.0.0.0) survive while a URL host (https://192.0.2.1/)
// does not. A four-part number labelled as a version also survives. An IPv6
// address needs eight groups or a "::", so a clock time (13:29:37) survives.
func IPAddresses(text, replacement string) string {
	text = replaceMatches(text, ipv4Candidate, replacement, func(start, end int) bool {
		if start > 0 {
			if before := text[start-1]; wordByte(before) || before == '.' || before == '-' {
				return false
			} else if before == '/' && start > 1 && (wordByte(text[start-2]) || text[start-2] == '.' || text[start-2] == '-') {
				return false
			}
		}
		if end < len(text) {
			if after := text[end]; wordByte(after) || after == '-' || (after == '.' && end+1 < len(text) && text[end+1] >= '0' && text[end+1] <= '9') {
				return false
			}
		}
		return !versionLabel.MatchString(text[max(0, start-16):start])
	})
	return replaceMatches(text, ipv6Candidate, replacement, func(start, end int) bool {
		if end-start == 2 { // a bare "::" is punctuation, not an address
			return false
		}
		if start > 0 && (wordByte(text[start-1]) || text[start-1] == ':' || text[start-1] == '.') {
			return false
		}
		return end == len(text) || (!wordByte(text[end]) && text[end] != ':')
	})
}

// replaceMatches replaces the matches of re that accept admits. accept reads
// the neighbouring bytes, which RE2 cannot express as lookaround.
func replaceMatches(text string, re *regexp.Regexp, replacement string, accept func(start, end int) bool) string {
	var out strings.Builder
	last := 0
	for _, span := range re.FindAllStringIndex(text, -1) {
		if !accept(span[0], span[1]) {
			continue
		}
		out.WriteString(text[last:span[0]])
		out.WriteString(replacement)
		last = span[1]
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

func wordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
