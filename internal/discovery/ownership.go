// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"fmt"
	"strings"

	"papio/internal/zotio"
)

// OwnershipLookup classifies a bounded batch of work identifiers against the
// local Zotio mirror.
type OwnershipLookup interface {
	LookupWorks(context.Context, zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error)
}

// ClassifyOwnership annotates works with local-library ownership in one lookup.
// It leaves all works unowned and returns a warning when classification cannot
// be completed, so a Zotio outage never prevents discovery.
func ClassifyOwnership(ctx context.Context, works []DiscoveredWork, lookup OwnershipLookup) string {
	for index := range works {
		works[index].Owned = false
		works[index].OwnedItemKey = ""
	}
	if len(works) == 0 {
		return ""
	}
	if lookup == nil {
		return "Zotio ownership lookup is unavailable; search results are unclassified"
	}

	request := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(works))}
	for index, discovered := range works {
		request.Works[index] = zotio.LookupWork{
			DOI:   discovered.Work.DOI,
			ArXiv: discovered.Work.ArXiv,
		}
	}
	result, err := lookup.LookupWorks(ctx, request)
	if err != nil {
		return fmt.Sprintf("Zotio ownership lookup failed; search results are unclassified: %v", err)
	}
	if result == nil || len(result.Works) != len(works) {
		return "Zotio ownership lookup returned an invalid result; search results are unclassified"
	}
	for index, ownership := range result.Works {
		if ownership.Status != zotio.OwnershipOwnedWithPDF && ownership.Status != zotio.OwnershipOwnedMissingPDF {
			continue
		}
		works[index].Owned = true                                        //nolint:gosec // G602: index bounded by len(result.Works)==len(works) guard above.
		works[index].OwnedItemKey = strings.TrimSpace(ownership.ItemKey) //nolint:gosec // G602: same bound.
	}
	return strings.TrimSpace(result.StalenessWarning)
}
