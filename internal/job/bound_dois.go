// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import "papio/internal/work"

// BoundDOIs returns every DOI durably bound to the job: the attested submission
// anchor, identifiers recorded at submission or verified during resolution, and
// the currently-known working DOI. No runtime resolver lookup.
//
// This is the ONLY implementation of the bound-DOI rule in the tree on
// purpose. Two normalizers for one equivalence relation is a documented
// footgun in this tree (see AGENTS.md), and the conclusive-identity veto
// plus the auto-bind pool must agree by construction, not by review. Do not
// add a second copy in another package; call this one.
func BoundDOIs(anchor SubmittedIdentity, current work.Work) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		if v == "" {
			return
		}
		if seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	// Attested anchor DOI (the work object materialized from the identifiers
	// table for submitted/verified provenance). Empty when the job never
	// carried a DOI.
	add(anchor.Work.DOI)
	for _, ident := range anchor.Identifiers {
		if ident.Kind != "doi" {
			continue
		}
		if ident.Provenance != ProvenanceSubmitted && ident.Provenance != ProvenanceVerified {
			continue
		}
		add(ident.Value)
	}
	add(current.DOI)
	return out
}
