// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import "context"

// Source is one discovery backend. Implementations must be bounded (request
// timeouts, response size caps) and must never create acquisition jobs.
type Source interface {
	Name() string
	Search(context.Context, SearchParams) ([]DiscoveredWork, error)
}
