// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"fmt"
	"strings"

	"papio/internal/ownership"
)

// HoldingsLookup answers ownership from generic holdings sources, for users who
// do not run Zotero (ADR-0008).
type HoldingsLookup interface {
	Enabled() bool
	Lookup(context.Context, []ownership.Query) ownership.Result
}

// ClassifyHoldings annotates works with local-library ownership from generic
// sources. It returns a warning string, empty when every source answered.
//
// Unlike the zotio path there is no item key to report: outside Zotero there is
// no stable per-item handle, so OwnedItemKey stays empty and callers must not
// depend on it to route an attachment.
//
// An incomplete lookup leaves results *unannotated* rather than marking them
// unowned. The distinction matters: "unowned" is a claim papio would be making
// without evidence, and downstream a false unowned is what turns one unreadable
// file into a batch of duplicate downloads.
func ClassifyHoldings(ctx context.Context, works []DiscoveredWork, lookup HoldingsLookup) string {
	for index := range works {
		works[index].Owned = false
		works[index].OwnedItemKey = ""
	}
	if len(works) == 0 || lookup == nil || !lookup.Enabled() {
		return ""
	}

	queries := make([]ownership.Query, len(works))
	for index, discovered := range works {
		queries[index] = ownership.QueryFor(
			discovered.Work.DOI,
			discovered.Work.ArXiv,
			discovered.Work.PMID,
			"", // discovery asks "is this in my library at all", not for a version
			"",
		)
	}
	result := lookup.Lookup(ctx, queries)
	for index := range works {
		if index >= len(result.Works) {
			break
		}
		// A known citation counts as "in library" for annotation even without a
		// PDF: the user asked what they already have, not what is complete.
		// Suppressing an acquisition needs the stricter test in ownership.Decide.
		decision := ownership.Decide(queries[index], result.Works[index])
		if decision.Suppress || decision.RecordPresent {
			works[index].Owned = true
		}
	}
	if incomplete := result.Incomplete(); len(incomplete) != 0 {
		return fmt.Sprintf("library sources unavailable (%s); some search results are unclassified", strings.Join(incomplete, ", "))
	}
	return ""
}
