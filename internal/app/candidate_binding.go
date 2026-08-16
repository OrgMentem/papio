// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import "papio/internal/job"

// boundDOIs returns every DOI durably bound to the job: the attested submission
// anchor, identifiers recorded at submission or verified during resolution, and
// the job row's current working DOI. No runtime resolver lookup.
func boundDOIs(anchor job.SubmittedIdentity, row *job.Row) []string {
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
		if ident.Provenance != job.ProvenanceSubmitted && ident.Provenance != job.ProvenanceVerified {
			continue
		}
		add(ident.Value)
	}
	if row != nil {
		add(row.Work.DOI)
	}
	return out
}
