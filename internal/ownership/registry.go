// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ownership

import "context"

// Registry is the daemon's configured set of holdings providers, shared by
// search, batch submission, and watches so they read one cache rather than each
// stampeding the user's library.
//
// A nil or empty Registry is a supported state, not an error: it means the user
// configured no generic sources. It answers every query with no claims and
// reports the result as complete, because "no libraries were configured" is a
// successful check of nothing — unlike a configured source that could not be
// read, which is incomplete and must not license a skip.
type Registry struct {
	providers []Provider
}

// NewRegistry returns a registry over the given providers. Nil providers are
// dropped so a partially failed construction cannot panic a lookup.
func NewRegistry(providers ...Provider) *Registry {
	live := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			live = append(live, provider)
		}
	}
	if len(live) == 0 {
		return &Registry{}
	}
	return &Registry{providers: live}
}

// Enabled reports whether any provider is configured. Callers use it to keep
// today's behaviour untouched when no generic source exists.
func (r *Registry) Enabled() bool {
	return r != nil && len(r.providers) > 0
}

// Names lists the configured provider names, for diagnostics.
func (r *Registry) Names() []string {
	if !r.Enabled() {
		return nil
	}
	names := make([]string, 0, len(r.providers))
	for _, provider := range r.providers {
		names = append(names, provider.Name())
	}
	return names
}

// Lookup aggregates every provider's claims for the given queries.
func (r *Registry) Lookup(ctx context.Context, queries []Query) Result {
	if !r.Enabled() {
		return Result{Works: make([]WorkResult, len(queries))}
	}
	return Aggregate(ctx, r.providers, queries)
}

// QueryFor builds a query from the identifiers a caller holds, dropping the ones
// papio cannot match exactly. A work with no matchable identifier yields a query
// with none, which can never produce a claim — the correct outcome, since
// absence of an identifier is absence of evidence, not evidence of absence.
func QueryFor(doi, arxiv, pmid, desiredVersion, entityKind string) Query {
	query := Query{DesiredVersion: desiredVersion, EntityKind: entityKind}
	// Fixed order, not a map range: claim ordering ends up in machine-readable
	// output, and a nondeterministic one would make results diff noisily.
	for _, candidate := range []Identifier{
		{Kind: KindDOI, Value: doi},
		{Kind: KindArXiv, Value: arxiv},
		{Kind: KindPMID, Value: pmid},
	} {
		if id, ok := NormalizeIdentifier(candidate); ok {
			query.Identifiers = append(query.Identifiers, id)
		}
	}
	return query
}
