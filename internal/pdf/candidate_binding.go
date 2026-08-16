// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"sort"
	"strings"
)

// Veto verdicts.
const (
	VetoAbsent     = "absent"     // |D| == 0: the document names no work conclusively
	VetoCompatible = "compatible" // |D| == 1 and it is EXACTLY one of the job's durably bound DOIs
	VetoAmbiguous  = "ambiguous"  // papio cannot resolve D against the job: |D| > 1, or a slash-run-only near-match
	VetoForeign    = "foreign"    // |D| == 1 and it names a work this job is not bound to
)

// ConclusiveIdentity is the daemon-side veto verdict derived from the
// document's conclusive front-matter DOI set D and the job's durably bound
// DOI set.
type ConclusiveIdentity struct {
	Verdict  string
	DOIs     []string // the conclusive set D, normalized verbatim, sorted, deduplicated
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
//
// Comparison is EXACT: normalizeDOI's canonical form with the suffix verbatim,
// slash runs preserved. A veto exists to notice a foreign work, and the one
// leniency this package has cannot serve that purpose, because the two facts it
// sits between are both true. DataCite holds 10.48612//monograph-2025-2 and
// 10.48612/monograph-2025-2 as two separately registered works with different
// titles (pinned by internal/ownership's TestNormalizeIdentifier), so collapsing
// them here files one work under the other's citation. Legacy APA PDFs print
// 10.1037//0021-9010.87.4.611 for the work Crossref names with one slash, so
// calling the pair foreign is equally wrong. Nothing lexical separates the two
// cases, and both wrong answers are wrong accepts of a kind this project treats
// as cardinal — so a slash-run-only difference resolves to neither: it is
// VetoAmbiguous, which blocks, parks, and asks a human. See
// collapseRegistrantSlashRun (identity.go) for the same asymmetry from the
// attested-single-target side.
//
// D is DOI-only. arXiv and PMID are target-aware identifier classes and
// deliberately do not enter this verdict; FrontMatterDOIs states that contract
// and its residual in full.
func CheckConclusiveIdentity(excerpt string, bound []string) ConclusiveIdentity {
	// Derive D with the same semantics as FrontMatterDOIs: the 1 KiB blind
	// window, conclusive set only. Reuse FrontMatterDOIs directly so the
	// window definition stays single-sourced.
	//
	// FrontMatterDOIs already returns normalized, deduplicated, verbatim
	// entries; both sides are run through normalizeDOI here anyway so the
	// bound set — which arrives as whatever the job durably recorded — meets
	// the document set under one relation, and so the comparison is right
	// regardless of what the extractor did first.
	dois, _ := normalizedDOISet(FrontMatterDOIs(excerpt))
	boundNorm, boundSeen := normalizedDOISet(bound)

	if len(dois) == 0 {
		return ConclusiveIdentity{
			Verdict:  VetoAbsent,
			DOIs:     nil,
			Evidence: []string{"no conclusive DOI in document front matter"},
		}
	}
	if len(dois) > 1 {
		// Reached by a document printing 10.48612/x and 10.48612//x both: those
		// are two names, deduplicating them to one would hide the conflict, and
		// the conflict is the answer.
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
	if near := slashRunNearMatch(single, boundNorm); near != "" {
		return ConclusiveIdentity{
			Verdict: VetoAmbiguous,
			DOIs:    dois,
			Evidence: []string{
				"document DOI: " + single,
				"job is bound to: " + near,
				"the two differ only by a slash run after the registrant, which may be one work spelled two ways (legacy APA) or two separately registered works (DataCite); papio cannot tell which, so a human must",
			},
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

// normalizedDOISet returns the exact-normalized, deduplicated, sorted values and
// a membership set over them. Empty and unnormalizable entries are dropped;
// sorting keeps evidence deterministic.
func normalizedDOISet(in []string) ([]string, map[string]bool) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		n := normalizeDOI(v)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, seen
}

// slashRunNearMatch returns the first bound DOI that differs from doi only by a
// slash run after the registrant, or "" when none does. Callers have already
// ruled out exact equality, so a hit here is strictly the undecidable case.
//
// This reuses collapseRegistrantSlashRun rather than testing the spelling
// itself: the package holds exactly two relations over DOIs — exact, and that
// collapse — and a third one written here would be the drift the tree's
// one-normalizer rule exists to prevent. What differs is the conclusion drawn,
// not the relation: MatchIdentity reads collapse-equality as sameness, this
// reads it as indeterminacy.
func slashRunNearMatch(doi string, bound []string) string {
	collapsed := collapseRegistrantSlashRun(doi)
	for _, b := range bound {
		if collapseRegistrantSlashRun(b) == collapsed {
			return b
		}
	}
	return ""
}
