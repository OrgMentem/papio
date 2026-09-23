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

// URL strips userinfo, query, and fragment, keeping scheme://host/path. A
// bearer-signed URL therefore loses its token before persistence. Unparseable
// input collapses to a fixed placeholder rather than leaking raw bytes.
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
	if u.RawQuery == "" && strings.Contains(raw, "?") {
		// Mark that something was removed so operators know evidence is partial.
		return u.String() + "?<redacted>"
	}
	return u.String()
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
