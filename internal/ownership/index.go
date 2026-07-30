// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ownership

import "strings"

// Entry is one holdings record reduced to what matching needs. Titles are
// deliberately absent: they are never matched (ADR-0008).
type Entry struct {
	Identifiers []Identifier
	// Artifact is what the *source* asserts about full text for this record,
	// never inferred by papio from a per-manager attachment convention. A
	// source declares this for everything it emits (`claim = "pdf_present"` /
	// `"record_present"`), or carries it per entry in papio's own holdings
	// format.
	Artifact string
	// ArtifactVersion is the manifestation the source holds, when it knows.
	// Unknown means the source said nothing, which restricts what it can
	// suppress — see Decide.
	ArtifactVersion string
	EntityKind      string
}

// Index is an immutable exact-match lookup over holdings entries. A loader
// builds one and swaps it in atomically; readers never mutate it, so a refresh
// can never expose a half-built index.
type Index struct {
	byKey map[string][]int
	// entries is append-only during Build and never mutated afterwards.
	entries []Entry
	// SkippedNoIdentifier counts records that parsed but carried no usable
	// identifier. They are not an error — a real library holds books, reports,
	// and hand-typed notes — but a source that is *entirely* unusable should be
	// visible rather than silently matching nothing.
	SkippedNoIdentifier int
}

// BuildIndex normalizes and indexes entries. Records whose identifiers are all
// unusable are counted and dropped; an individually invalid identifier is
// dropped while the record's remaining identifiers survive.
func BuildIndex(entries []Entry) *Index {
	index := &Index{byKey: make(map[string][]int), entries: make([]Entry, 0, len(entries))}
	for _, entry := range entries {
		normalized := make([]Identifier, 0, len(entry.Identifiers))
		seen := make(map[string]bool, len(entry.Identifiers))
		for _, id := range entry.Identifiers {
			canonical, ok := NormalizeIdentifier(id)
			if !ok || seen[canonical.Key()] {
				continue
			}
			seen[canonical.Key()] = true
			normalized = append(normalized, canonical)
		}
		if len(normalized) == 0 {
			index.SkippedNoIdentifier++
			continue
		}
		entry.Identifiers = normalized
		entry.Artifact = normalizeArtifact(entry.Artifact)
		entry.ArtifactVersion = normalizeVersion(entry.ArtifactVersion)
		entry.EntityKind = normalizeEntity(entry.EntityKind)
		position := len(index.entries)
		index.entries = append(index.entries, entry)
		for _, id := range normalized {
			index.byKey[id.Key()] = append(index.byKey[id.Key()], position)
		}
	}
	return index
}

// Len reports the number of indexed entries. A successfully parsed source with
// zero entries is a legitimate state and must stay distinguishable from an
// unreadable one, so callers report this alongside health rather than treating
// zero as failure.
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.entries)
}

// Claims returns one source's claims for a query: every entry reachable from
// any of the query's identifiers. Only positive evidence is produced — a miss
// yields no claim rather than a negative one.
func (i *Index) Claims(source string, query Query) []Claim {
	if i == nil {
		return nil
	}
	matches := make(map[int]Identifier)
	for _, raw := range query.Identifiers {
		id, ok := NormalizeIdentifier(raw)
		if !ok {
			continue
		}
		for _, position := range i.byKey[id.Key()] {
			current, exists := matches[position]
			if !exists || strongerIdentifier(id, current) {
				matches[position] = id
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	claims := make([]Claim, 0, len(matches))
	for position, entry := range i.entries {
		id, ok := matches[position]
		if !ok {
			continue
		}
		claims = append(claims, Claim{
			Source:          source,
			Matched:         id,
			RecordPresent:   true,
			Artifact:        entry.Artifact,
			ArtifactVersion: entry.ArtifactVersion,
			EntityKind:      entry.EntityKind,
		})
	}
	return claims
}

func strongerIdentifier(candidate, current Identifier) bool {
	candidateRank := identifierRank(candidate.Kind)
	currentRank := identifierRank(current.Kind)
	if candidateRank != currentRank {
		return candidateRank < currentRank
	}
	return candidate.Key() < current.Key()
}

func identifierRank(kind string) int {
	switch kind {
	case KindDOI:
		return 0
	case KindPMID:
		return 1
	case KindArXiv:
		return 2
	default:
		return 3
	}
}

func normalizeArtifact(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case ArtifactPresent:
		return ArtifactPresent
	case ArtifactMissing:
		return ArtifactMissing
	default:
		return ArtifactUnknown
	}
}

func normalizeVersion(version string) string {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case VersionPreprint:
		return VersionPreprint
	case VersionAccepted:
		return VersionAccepted
	case VersionPublished:
		return VersionPublished
	default:
		return VersionUnknown
	}
}
