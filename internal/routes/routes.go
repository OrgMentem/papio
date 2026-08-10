// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package routes contains the daemon's compiled-in provider direct-route table.
package routes

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"papio/internal/work"
)

// Candidate is one provider-direct route for a canonical work identifier.
// Identifier is a work-scheme value such as "doi:10.1234/example" or
// "pii:S1234567890123456".
type Candidate struct {
	RouteRevision string
	URL           string
	AllowedOrigin string
	PathFamily    string
	TermsPolicy   string
	Identifier    string
}

const (
	termsPolicyNone = "none"
	identifierDOI   = "doi"
	identifierPII   = "pii"
)

type routeTemplate struct {
	routeRevision string
	provider      string
	identifier    string
	origin        string
	pathFamily    string
	pathPrefix    string
	pathSuffix    string
	query         string
}

// routeTable is deliberately data, not user-editable configuration. The
// compiler below has one expansion case per identifier kind; no caller can
// interpolate arbitrary text into a URL template.
var routeTable = []routeTemplate{
	{
		routeRevision: "wiley-doi-pdfdirect/1",
		provider:      "wiley-doi-pdfdirect",
		identifier:    identifierDOI,
		origin:        "https://onlinelibrary.wiley.com",
		pathFamily:    "/doi/pdfdirect/{doi}",
		pathPrefix:    "/doi/pdfdirect/",
		query:         "download=true",
	},
	{
		routeRevision: "sage-doi-pdf/1",
		provider:      "sage-doi-pdf",
		identifier:    identifierDOI,
		origin:        "https://journals.sagepub.com",
		pathFamily:    "/doi/pdf/{doi}",
		pathPrefix:    "/doi/pdf/",
		query:         "download=true",
	},
	{
		routeRevision: "sciencedirect-pii-pdfft/1",
		provider:      "sciencedirect-pii-pdfft",
		identifier:    identifierPII,
		origin:        "https://www.sciencedirect.com",
		pathFamily:    "/science/article/pii/{pii}/pdfft",
		pathPrefix:    "/science/article/pii/",
		pathSuffix:    "/pdfft",
	},
}

// Validate checks every compiled-in route's declared URL envelope. It is
// intentionally exported so release tests can fail closed when a seed row is
// edited incorrectly.
func Validate() error {
	if len(routeTable) == 0 {
		return fmt.Errorf("route table is empty")
	}
	seenRevisions := make(map[string]struct{}, len(routeTable))
	for i, route := range routeTable {
		if route.routeRevision == "" || !strings.Contains(route.routeRevision, "/") {
			return fmt.Errorf("route %d has invalid revision %q", i, route.routeRevision)
		}
		if _, exists := seenRevisions[route.routeRevision]; exists {
			return fmt.Errorf("route %d duplicates revision %q", i, route.routeRevision)
		}
		seenRevisions[route.routeRevision] = struct{}{}
		revisionParts := strings.Split(route.routeRevision, "/")
		if len(revisionParts) != 2 || revisionParts[0] != route.provider {
			return fmt.Errorf("route %q has invalid provider/revision", route.routeRevision)
		}
		revision, err := strconv.Atoi(revisionParts[1])
		if err != nil || revision < 1 {
			return fmt.Errorf("route %q has invalid revision number", route.routeRevision)
		}
		if route.origin == "" || route.identifier == "" || route.pathFamily == "" {
			return fmt.Errorf("route %q has an incomplete declaration", route.routeRevision)
		}
		origin, err := url.Parse(route.origin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return fmt.Errorf("route %q has invalid allowed origin %q", route.routeRevision, route.origin)
		}
		if route.query != "" && route.query != "download=true" {
			return fmt.Errorf("route %q has an unsupported query", route.routeRevision)
		}
		placeholder := "{" + route.identifier + "}"
		pathWithoutSlot := strings.Replace(route.pathFamily, placeholder, "", 1)
		if strings.Count(route.pathFamily, placeholder) != 1 || strings.ContainsAny(pathWithoutSlot, "{}") {
			return fmt.Errorf("route %q must contain exactly one named identifier slot", route.routeRevision)
		}
		if route.pathPrefix+placeholder+route.pathSuffix != route.pathFamily {
			return fmt.Errorf("route %q path family does not match its compiler", route.routeRevision)
		}
		if !strings.HasPrefix(route.pathPrefix, "/") || strings.Contains(route.pathPrefix, "?") || strings.Contains(route.pathSuffix, "?") {
			return fmt.Errorf("route %q has an invalid path family", route.routeRevision)
		}

		identifier := "10.1234/route-check"
		if route.identifier == identifierPII {
			identifier = "S1234567890123456"
		}
		candidate, err := expand(route, identifier)
		if err != nil {
			return fmt.Errorf("route %q does not compile: %w", route.routeRevision, err)
		}
		if err := validateCandidate(candidate, route, identifier); err != nil {
			return fmt.Errorf("route %q violates its envelope: %w", route.routeRevision, err)
		}
	}
	return nil
}

// CandidatesFor returns DOI-based direct routes. providerHint may be empty,
// a provider family (for example "sage-doi-pdf"), or a full route revision.
// ScienceDirect's PII route is intentionally absent here: callers with a PII
// must use CandidatesForIdentifiers.
func CandidatesFor(doi string, providerHint string) []Candidate {
	return candidatesFor(doi, "", providerHint)
}

// CandidatesForIdentifiers returns direct routes from a public identifier map.
// The supported keys are "doi" and "pii". PII is required for the
// ScienceDirect route; a DOI is not substituted for it. Values must be
// canonical public identifiers and are never treated as URL templates.
func CandidatesForIdentifiers(identifiers map[string]string, providerHint string) []Candidate {
	return candidatesFor(identifiers[identifierDOI], identifiers[identifierPII], providerHint)
}

func candidatesFor(doi, pii, providerHint string) []Candidate {
	canonicalDOI, doiOK := canonicalDOI(doi)
	pii, piiOK := canonicalIdentifier(pii)
	candidates := make([]Candidate, 0, len(routeTable))
	for _, route := range routeTable {
		if !matchesProvider(route, providerHint) {
			continue
		}
		var identifier string
		switch route.identifier {
		case identifierDOI:
			if !doiOK {
				continue
			}
			identifier = canonicalDOI
		case identifierPII:
			if !piiOK {
				continue
			}
			identifier = pii
		default:
			continue
		}
		candidate, err := expand(route, identifier)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func matchesProvider(route routeTemplate, hint string) bool {
	if hint == "" {
		return true
	}
	return hint == route.provider || hint == route.routeRevision
}

func canonicalDOI(raw string) (string, bool) {
	if !safeIdentifier(raw) {
		return "", false
	}
	doi, err := work.NormalizeDOI(raw)
	if err != nil || !safeIdentifier(doi) {
		return "", false
	}
	return doi, true
}

func canonicalIdentifier(raw string) (string, bool) {
	if !safeIdentifier(raw) {
		return "", false
	}
	return raw, raw != ""
}

func safeIdentifier(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '#' || r == '?' || r == '\\' || r == '@' {
			return false
		}
	}
	return true
}

func expand(route routeTemplate, identifier string) (Candidate, error) {
	if !safeIdentifier(identifier) {
		return Candidate{}, fmt.Errorf("identifier contains a forbidden character")
	}
	var escaped string
	switch route.identifier {
	case identifierDOI, identifierPII:
		escaped = escapePathIdentifier(identifier)
	default:
		return Candidate{}, fmt.Errorf("unsupported identifier slot %q", route.identifier)
	}
	path := route.pathPrefix + identifier + route.pathSuffix
	rawPath := route.pathPrefix + escaped + route.pathSuffix
	u := url.URL{Scheme: "https", Host: strings.TrimPrefix(route.origin, "https://"), Path: path, RawPath: rawPath, RawQuery: route.query}
	candidate := Candidate{
		RouteRevision: route.routeRevision,
		URL:           u.String(),
		AllowedOrigin: route.origin,
		PathFamily:    route.pathFamily,
		TermsPolicy:   termsPolicyNone,
		Identifier:    route.identifier + ":" + identifier,
	}
	if err := validateCandidate(candidate, route, identifier); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

func validateCandidate(candidate Candidate, route routeTemplate, identifier string) error {
	u, err := url.Parse(candidate.URL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	origin, err := url.Parse(candidate.AllowedOrigin)
	if err != nil {
		return fmt.Errorf("invalid allowed origin: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Host != origin.Host || u.Hostname() != origin.Hostname() {
		return fmt.Errorf("URL is outside HTTPS/origin envelope")
	}
	if candidate.TermsPolicy != termsPolicyNone || route.query != u.RawQuery {
		return fmt.Errorf("terms or query envelope mismatch")
	}
	placeholder := "{" + route.identifier + "}"
	expectedPath := strings.Replace(route.pathFamily, placeholder, escapePathIdentifier(identifier), 1)
	if u.EscapedPath() != expectedPath {
		return fmt.Errorf("path %q is outside %q", u.EscapedPath(), route.pathFamily)
	}
	if route.query != "" && (strings.Count(u.Query().Get("download"), "true") != 1 || len(u.Query()) != 1) {
		return fmt.Errorf("query is duplicated or unexpected")
	}
	if route.query == "" && len(u.Query()) != 0 {
		return fmt.Errorf("query is duplicated or unexpected")
	}
	return nil
}

func escapePathIdentifier(identifier string) string {
	var b strings.Builder
	for segmentIndex, segment := range strings.Split(identifier, "/") {
		if segmentIndex > 0 {
			b.WriteByte('/')
		}
		if segment == "." || segment == ".." {
			for i := range len(segment) {
				writePercentByte(&b, segment[i])
			}
			continue
		}
		for i := range len(segment) {
			c := segment[i]
			if isRFC3986Unreserved(c) {
				b.WriteByte(c)
			} else {
				writePercentByte(&b, c)
			}
		}
	}
	return b.String()
}

func isRFC3986Unreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func writePercentByte(b *strings.Builder, c byte) {
	const hex = "0123456789ABCDEF"
	b.WriteByte('%')
	b.WriteByte(hex[c>>4])
	b.WriteByte(hex[c&0x0f])
}
