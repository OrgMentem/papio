// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import "context"

// Source is one discovery backend. Implementations must be bounded (request
// timeouts, response size caps) and must never create acquisition jobs.
type Source interface {
	Name() string
	Search(context.Context, SearchParams) ([]DiscoveredWork, error)
}

// PartialSearcher is a Source that can report which backends failed while
// others answered. Callers type-assert for it rather than widening Source,
// which every individual backend implements and none of them can satisfy.
type PartialSearcher interface {
	SearchPartial(ctx context.Context, params SearchParams) ([]DiscoveredWork, []BackendFailure, error)
}

// BackendHealth is a Source that retains the most recent failure of each
// backend currently failing, so a diagnostic surface can report a broken
// backend instead of leaving a user to guess why results look thin.
type BackendHealth interface {
	LastFailures() []BackendFailure
}
