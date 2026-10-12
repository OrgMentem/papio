// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// RetractedDOIs lists the distinct DOIs that carry a current retraction
// notice, for the Zotero retraction-tag reconciler. Only NatureRetraction
// counts: corrections and expressions of concern are excluded here. It does
// not read acknowledgements, so dismissing the inbox alert leaves the tag in
// place, and a notice that a later sweep drops from the cache (withdrawn)
// leaves the list on the next call.
//
// A missing or unreadable cache is an error rather than an empty list: "no
// notices known" must never read as "every retraction was withdrawn" and
// strip tags papio owns.
func (s *Sentinel) RetractedDOIs(context.Context) ([]string, error) {
	if s == nil {
		return nil, errors.New("retraction sentinel is not enabled")
	}
	s.mu.Lock()
	cached, ok, err := s.readCacheDetailed()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// A cache holding only a fetch failure has no CheckedAt: no sweep has
	// completed, so an empty notice set there means "unknown", not "withdrawn".
	if !ok || cached.CheckedAt.IsZero() {
		return nil, fmt.Errorf("no retraction sweep has completed yet")
	}
	if cached.Truncated {
		return nil, fmt.Errorf("retraction notice set was truncated at %d; refusing to authorise tag changes from an incomplete set", maxNotices)
	}
	seen := map[string]struct{}{}
	for _, finding := range validNotices(cached.Notices) {
		if finding.Nature == NatureRetraction {
			seen[finding.DOI] = struct{}{}
		}
	}
	dois := make([]string, 0, len(seen))
	for doi := range seen {
		dois = append(dois, doi)
	}
	sort.Strings(dois)
	return dois, nil
}
