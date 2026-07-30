// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package ownership models what papio knows about libraries the user already
// holds, independent of where those libraries live. See ADR-0008.
//
// The model is deliberately asymmetric. A provider emits only *positive*
// evidence — this record exists, this artifact is present, this identifier
// matched — and never "not owned". Absence of a claim is computed here, and it
// is actionable only when every source required for a negative answer was read
// successfully: a provider failure is incompleteness, never a negative fact.
// Suppressing an acquisition on a failed lookup would silently withhold a paper
// the user asked for, whereas re-downloading one costs a download.
//
// This package stays dependency-light on purpose: it must not import
// internal/ingest (which imports internal/batch) or internal/zotio. Providers
// adapt to it, not the reverse.
package ownership

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Identifier kinds papio matches on. Titles are never matched: a false-positive
// skip withholds requested work, so only exact identifiers qualify. ISBN is
// deliberately absent — an edited volume shares one ISBN with every chapter in
// it, so an ISBN match cannot distinguish twenty distinct requests (ADR-0008
// invariant 6).
const (
	KindDOI   = "doi"
	KindArXiv = "arxiv"
	KindPMID  = "pmid"
)

// Identifier is one normalized bibliographic identifier.
type Identifier struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// EntityKind records what a request or holdings entry is *about*. It exists so
// that a future book-level identifier can never satisfy a chapter-level request.
const (
	EntityUnknown = "unknown"
	EntityArticle = "article"
	EntityChapter = "chapter"
	EntityBook    = "book"
)

// ArtifactState is what a source knows about a full-text file for a record.
// Unknown is the honest default: a bibliographic export that carries no
// attachment information asserts nothing about whether a PDF exists.
const (
	ArtifactUnknown = "unknown"
	ArtifactMissing = "missing"
	ArtifactPresent = "present"
)

// Version values mirror protocol.WorkRequest.DesiredVersion and the candidate
// version enum: these name a *manifestation*, not merely a work identity.
const (
	VersionUnknown   = "unknown"
	VersionPreprint  = "preprint"
	VersionAccepted  = "accepted"
	VersionPublished = "published"
	VersionAny       = "any"
)

// Query is one work papio is about to acquire, as asked of every provider.
type Query struct {
	Identifiers    []Identifier `json:"identifiers"`
	DesiredVersion string       `json:"desired_version,omitempty"`
	EntityKind     string       `json:"entity_kind,omitempty"`
}

// Claim is one source's positive statement about one query. A source that has
// nothing to say emits no claim; there is no negative claim.
type Claim struct {
	Source string `json:"source"`
	// Matched is the identifier that produced the hit, retained because the
	// version rules depend on which kind matched.
	Matched         Identifier `json:"matched"`
	RecordPresent   bool       `json:"record_present"`
	Artifact        string     `json:"artifact"`
	ArtifactVersion string     `json:"artifact_version,omitempty"`
	EntityKind      string     `json:"entity_kind,omitempty"`
	// Stale marks a claim served from a last-known-good index whose freshness
	// window has expired. Such a claim is still evidence — it may annotate a
	// search result — but it may not suppress an acquisition: the user may have
	// moved or deleted the file since, and a wrong skip withholds requested work.
	Stale bool `json:"stale,omitempty"`
}

// SourceHealth is per-operation, decision-critical health. It is not
// diagnostics: `papio doctor` reports standing state, but a caller deciding
// whether "no match" is trustworthy needs the answer in the lookup result
// itself (ADR-0008 invariant 1).
type SourceHealth struct {
	Name string `json:"name"`
	// Complete reports that this source was read successfully and its claims
	// may participate in a negative decision. A successful but empty snapshot
	// is complete; an unavailable one is not.
	Complete bool `json:"complete"`
	// Stale reports a last-known-good index served past its freshness window.
	// Stale positives may annotate a search result but never suppress work.
	Stale       bool      `json:"stale,omitempty"`
	EntryCount  int       `json:"entry_count"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	// FailureCode is a bounded category, never raw stderr or command output:
	// a provider inherits the daemon environment and its output can carry
	// credentials.
	FailureCode string `json:"failure_code,omitempty"`
}

// Failure codes are a closed set so that callers and `doctor` can branch on
// them and neither ever prints provider output.
const (
	FailureUnreadable    = "unreadable"
	FailureParse         = "parse"
	FailureTimeout       = "timeout"
	FailureExit          = "exit"
	FailureTruncated     = "truncated"
	FailureCountCollapse = "count_collapse"
	FailureMisaligned    = "misaligned"
	FailureNotConfigured = "not_configured"
)

// Provider is one library papio can ask about. Lookup returns claims aligned by
// index with the queries. It reports health instead of an error: a broken
// provider must degrade the aggregate to incomplete, not fail the caller's
// operation, and an error return would tempt callers to treat failure as "no
// match".
type Provider interface {
	Name() string
	Lookup(ctx context.Context, queries []Query) (claims [][]Claim, health SourceHealth)
}

// WorkResult is every source's claims about one query.
type WorkResult struct {
	Claims []Claim `json:"claims"`
}

// Result is one aggregated lookup.
type Result struct {
	Works   []WorkResult   `json:"works"`
	Sources []SourceHealth `json:"sources"`
}

// Complete reports whether every source was usable, i.e. whether an absence of
// claims may be read as "not held".
func (r Result) Complete() bool {
	for _, source := range r.Sources {
		if !source.Complete {
			return false
		}
	}
	return true
}

// Incomplete names the sources that could not be read, for a bounded warning.
func (r Result) Incomplete() []string {
	var names []string
	for _, source := range r.Sources {
		if !source.Complete {
			names = append(names, source.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Aggregate asks every provider and unions their claims. Source facts are
// unioned because negative claims are source-scoped: one library not holding a
// paper never negates another library that does (ADR-0008 invariant 10).
// Providers are asked in order but their answers carry no precedence — order
// affects only claim ordering within a work, for stable output.
func Aggregate(ctx context.Context, providers []Provider, queries []Query) Result {
	result := Result{Works: make([]WorkResult, len(queries))}
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		claims, health := provider.Lookup(ctx, queries)
		if health.Name == "" {
			health.Name = provider.Name()
		}
		// Claims are unioned even from an unhealthy source. Health governs only
		// the right to conclude *absence*: a provider serving a last-known-good
		// index still holds real positive evidence, and discarding it would make
		// a transient read failure re-download a paper the user demonstrably has.
		// A provider's response must be aligned both by query position and matched
		// identifier. Otherwise, discard all of its claims: misattribution could
		// suppress an unrelated paper, and malformed output proves this lookup
		// cannot support a negative conclusion.
		if !claimsAlign(queries, claims) {
			health.Complete = false
			health.FailureCode = FailureMisaligned
			result.Sources = append(result.Sources, health)
			continue
		}
		result.Sources = append(result.Sources, health)
		for i := range queries {
			for _, claim := range claims[i] {
				claim.Matched, _ = NormalizeIdentifier(claim.Matched)
				claim.Stale = health.Stale
				result.Works[i].Claims = append(result.Works[i].Claims, claim)
			}
		}
	}
	return result
}

// Decision is what a caller should do about one work.
type Decision struct {
	// Suppress reports that acquisition should be skipped because a source
	// proved a usable full-text file already exists.
	Suppress bool
	// RecordPresent reports that some source knows the citation, whether or not
	// a file exists. It may annotate discovery output; it must never suppress
	// acquisition, because a citation-only entry is precisely what a backfill
	// user wants acquired.
	RecordPresent bool
	// Source names the claim that drove Suppress, for reporting.
	Source string
}

// Decide applies the suppression rules to one work's claims.
//
// Only a fresh, explicit artifact-present claim may suppress (invariant 2), and
// the matched identifier must be able to satisfy the requested version:
//
//   - An arXiv match cannot satisfy a request for `published` or `accepted`:
//     holding the preprint is not holding the version of record.
//   - A claim whose ArtifactVersion is unknown satisfies only an unspecified or
//     `any` request. papio's version enum names a manifestation, so a
//     bibliographic export that says nothing about which file it holds cannot
//     answer a request that asked for a specific one.
//   - A claim whose ArtifactVersion is known must equal the requested version.
//
// Absence of a suppressing claim is not itself a decision: callers must consult
// Result.Complete before treating a false Suppress as "not held".
func Decide(query Query, work WorkResult) Decision {
	decision := Decision{}
	for _, claim := range work.Claims {
		if claim.RecordPresent {
			decision.RecordPresent = true
		}
		if claim.Artifact != ArtifactPresent {
			continue
		}
		// "Fresh" is part of invariant 2. A claim from an index whose freshness
		// window lapsed may still annotate a result, but the file it describes may
		// be gone, so it cannot justify skipping an acquisition.
		if claim.Stale {
			continue
		}
		if !satisfiesVersion(query.DesiredVersion, claim) {
			continue
		}
		if !satisfiesEntity(query.EntityKind, claim) {
			continue
		}
		if !decision.Suppress {
			decision.Suppress = true
			decision.Source = claim.Source
		}
	}
	return decision
}

func satisfiesVersion(desired string, claim Claim) bool {
	desired = strings.TrimSpace(desired)
	if desired == "" {
		desired = VersionAny
	}
	if claim.Matched.Kind == KindArXiv && (desired == VersionPublished || desired == VersionAccepted) {
		return false
	}
	version := strings.TrimSpace(claim.ArtifactVersion)
	if version == "" {
		version = VersionUnknown
	}
	if version == VersionUnknown {
		return desired == VersionAny
	}
	if desired == VersionAny {
		return true
	}
	return version == desired
}

// satisfiesEntity accepts an unknown kind because it has no conflicting
// identity; when both kinds are known, they must agree.
func satisfiesEntity(requested string, claim Claim) bool {
	requested = normalizeEntity(requested)
	held := normalizeEntity(claim.EntityKind)
	return requested == EntityUnknown || held == EntityUnknown || requested == held
}

func normalizeEntity(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case EntityArticle:
		return EntityArticle
	case EntityChapter:
		return EntityChapter
	case EntityBook:
		return EntityBook
	default:
		return EntityUnknown
	}
}

func claimsAlign(queries []Query, claims [][]Claim) bool {
	if len(claims) != len(queries) {
		return false
	}
	for i, query := range queries {
		identifiers := make(map[string]struct{}, len(query.Identifiers))
		for _, raw := range query.Identifiers {
			identifier, ok := NormalizeIdentifier(raw)
			if ok {
				identifiers[identifier.Key()] = struct{}{}
			}
		}
		for _, claim := range claims[i] {
			identifier, ok := NormalizeIdentifier(claim.Matched)
			if !ok {
				return false
			}
			if _, ok := identifiers[identifier.Key()]; !ok {
				return false
			}
		}
	}
	return true
}

// NormalizeIdentifier canonicalizes one identifier for exact matching, and
// reports whether the result is usable. An unusable identifier is dropped
// without discarding a record's other identifiers.
func NormalizeIdentifier(id Identifier) (Identifier, bool) {
	kind := strings.ToLower(strings.TrimSpace(id.Kind))
	value := strings.TrimSpace(id.Value)
	if value == "" {
		return Identifier{}, false
	}
	switch kind {
	case KindDOI:
		value = normalizeDOI(value)
	case KindArXiv:
		value = normalizeArXiv(value)
	case KindPMID:
		value = normalizePMID(value)
	default:
		return Identifier{}, false
	}
	if value == "" {
		return Identifier{}, false
	}
	return Identifier{Kind: kind, Value: value}, true
}

// Key is the exact-match index key for a normalized identifier.
func (i Identifier) Key() string { return i.Kind + ":" + i.Value }

func normalizeDOI(value string) string {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "doi:"} {
		if strings.HasPrefix(lower, prefix) {
			value = value[len(prefix):]
			break
		}
	}
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "10.") {
		return ""
	}
	// DOIs are case-insensitive; lowercase so an exported uppercase DOI still
	// matches a resolver-supplied one.
	return strings.ToLower(value)
}

func normalizeArXiv(value string) string {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"arxiv:", "https://arxiv.org/abs/", "http://arxiv.org/abs/"} {
		if strings.HasPrefix(lower, prefix) {
			value = value[len(prefix):]
			break
		}
	}
	value = strings.ToLower(strings.TrimSpace(value))
	// A version suffix names the same work, so v2 must match v1.
	if idx := strings.LastIndex(value, "v"); idx > 0 && allDigits(value[idx+1:]) {
		value = value[:idx]
	}
	if value == "" {
		return ""
	}
	return value
}

func normalizePMID(value string) string {
	value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "pmid:")
	value = strings.TrimSpace(value)
	if value == "" || !allDigits(value) {
		return ""
	}
	return strings.TrimLeft(value, "0")
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
