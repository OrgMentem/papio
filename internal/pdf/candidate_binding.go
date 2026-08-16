// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"sort"
	"strings"
)

// Veto verdicts.
const (
	VetoAbsent     = "absent"     // |D| == 0: the document names no work conclusively
	VetoCompatible = "compatible" // |D| == 1 and it is one of the job's durably bound DOIs
	VetoAmbiguous  = "ambiguous"  // |D| > 1
	VetoForeign    = "foreign"    // |D| == 1 and it names a work this job is not bound to
)

// ConclusiveIdentity is the daemon-side veto verdict derived from the
// document's conclusive front-matter DOI set D and the job's durably bound
// DOI set.
type ConclusiveIdentity struct {
	Verdict  string
	DOIs     []string // the conclusive set D, normalized, sorted, deduplicated
	Evidence []string
}

// Blocks reports whether the verdict should prevent filing the document
// under the job without human review.
func (c ConclusiveIdentity) Blocks() bool {
	return c.Verdict == VetoAmbiguous || c.Verdict == VetoForeign
}

// CheckConclusiveIdentity derives D from excerpt with exactly FrontMatterDOIs
// semantics (the 1 KiB blind window — never whole-document, never
// identityPageOne) and compares it against the DOIs durably bound to the job.
func CheckConclusiveIdentity(excerpt string, bound []string) ConclusiveIdentity {
	// Derive D with the same semantics as FrontMatterDOIs: the 1 KiB blind
	// window, conclusive set only. Reuse FrontMatterDOIs directly so the
	// window definition stays single-sourced.
	raw := FrontMatterDOIs(excerpt)

	// FrontMatterDOIs returns normalized, deduplicated entries, but the
	// contract requires normalize/dedupe/sort here so the comparison is
	// resilient regardless, and so the doubled-suffix-slash collapse
	// (normalizeDOI) applies comparison-scoped on both sides.
	seen := make(map[string]bool)
	var dois []string
	for _, v := range raw {
		n := normalizeDOI(v)
		if n == "" {
			// FrontMatterDOIs may already have normalized; if v was
			// normalized the second pass is idempotent. If it was not,
			// this is the comparison-scoped collapse. An empty result
			// means unnormalizable and is ignored.
			continue
		}
		if !seen[n] {
			seen[n] = true
			dois = append(dois, n)
		}
	}
	sort.Strings(dois)

	// Normalize the bound set the same way. Empty/unnormalizable entries
	// are ignored. Dedup and sort for deterministic evidence.
	boundSeen := make(map[string]bool)
	var boundNorm []string
	for _, v := range bound {
		n := normalizeDOI(v)
		if n == "" {
			continue
		}
		if !boundSeen[n] {
			boundSeen[n] = true
			boundNorm = append(boundNorm, n)
		}
	}
	sort.Strings(boundNorm)

	if len(dois) == 0 {
		return ConclusiveIdentity{
			Verdict:  VetoAbsent,
			DOIs:     nil,
			Evidence: []string{"no conclusive DOI in document front matter"},
		}
	}
	if len(dois) > 1 {
		return ConclusiveIdentity{
			Verdict:  VetoAmbiguous,
			DOIs:     dois,
			Evidence: []string{"document DOIs: " + strings.Join(dois, ", ")},
		}
	}
	single := dois[0]
	if boundSeen[single] {
		return ConclusiveIdentity{
			Verdict:  VetoCompatible,
			DOIs:     dois,
			Evidence: []string{"document DOI: " + single},
		}
	}
	// |D| == 1 but not in bound — includes the case where bound is empty
	// (e.g. a PMID-only or arXiv-only job with no recorded DOI equivalence).
	if len(boundNorm) == 0 {
		return ConclusiveIdentity{
			Verdict:  VetoForeign,
			DOIs:     dois,
			Evidence: []string{"document DOI: " + single, "job is bound to no DOI"},
		}
	}
	return ConclusiveIdentity{
		Verdict:  VetoForeign,
		DOIs:     dois,
		Evidence: []string{"document DOI: " + single, "job is bound to: " + strings.Join(boundNorm, ", ")},
	}
}
