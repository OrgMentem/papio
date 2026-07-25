// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"papio/internal/work"
)

// Multi searches its sources in preference order and merges their results.
//
// It is the daemon's long-lived discovery source, so it is also the natural
// owner of per-backend health: failures observed during a search are retained
// here for diagnostics rather than discarded.
type Multi struct {
	sources []Source

	mu       sync.Mutex
	failures map[string]BackendFailure
	// now is injectable so failure timestamps are deterministic under test.
	now func() time.Time
}

// Compile-time assertions that *Multi satisfies the optional interfaces its
// two consumers — internal/api's discovery.search handler and papio doctor's
// discovery check — reach only by runtime type-assertion. Without these, a
// signature drift here would silently downgrade both to their degraded
// fallback path (plain Search, no partial results or health reporting)
// instead of failing the build.
var (
	_ PartialSearcher = (*Multi)(nil)
	_ BackendHealth   = (*Multi)(nil)
)

// NewMulti returns a Source that fans a search across backends in preference
// order and merges results. With no explicit source parameter, the supplied
// sources are searched in their given order.
func NewMulti(sources ...Source) *Multi {
	return &Multi{sources: sources, failures: make(map[string]BackendFailure, len(sources))}
}

// Name identifies the composed backend.
func (m *Multi) Name() string {
	return "multi"
}

// Search satisfies Source. It reports usable results and hard failures only;
// callers wanting to know that a backend broke while another answered use
// SearchPartial.
func (m *Multi) Search(ctx context.Context, params SearchParams) ([]DiscoveredWork, error) {
	works, _, err := m.SearchPartial(ctx, params)
	return works, err
}

// SearchPartial queries selected backends sequentially so each remains
// independently bounded, and returns any usable result together with the
// failures of the backends that did not answer.
//
// Those failures used to be discarded whenever at least one backend succeeded,
// which made a broken backend invisible: results looked merely thin. They are
// returned here for the caller to report, and retained on the Multi for
// diagnostics. A backend that answers successfully has its retained failure
// cleared, so a transient outage does not linger.
func (m *Multi) SearchPartial(ctx context.Context, params SearchParams) ([]DiscoveredWork, []BackendFailure, error) {
	if m == nil || len(m.sources) == 0 {
		return nil, nil, errors.New("discovery: no discovery sources are configured")
	}
	params = normalizeParams(params)
	if params.Source != "" {
		for _, source := range m.sources {
			if source != nil && source.Name() == params.Source {
				works, err := source.Search(ctx, params)
				if err != nil {
					// An explicitly named source failing is a hard error, and
					// still worth remembering — unless the caller is the one
					// who gave up: a cancelled outer context says nothing
					// about this backend's health, so recording it would
					// leave a bogus failure for a backend that never got a
					// real chance to answer. Checking ctx.Err() rather than
					// errors.Is on err distinguishes that from the backend's
					// own internal deadline expiring independently of the
					// caller, which is real signal and must still be
					// recorded.
					if ctx.Err() != nil {
						return nil, nil, err
					}
					return nil, []BackendFailure{m.recordFailure(source.Name(), err)}, err
				}
				m.clearFailure(source.Name())
				return finalize([][]DiscoveredWork{withSource(works, source.Name())}, params), nil, nil
			}
		}
		return nil, nil, fmt.Errorf("unknown discovery source %q", params.Source)
	}

	results := make([][]DiscoveredWork, 0, len(m.sources))
	failures := make([]BackendFailure, 0, len(m.sources))
	hard := make([]error, 0, len(m.sources))
	for _, source := range m.sources {
		if source == nil {
			hard = append(hard, errors.New("discovery: configured source is nil"))
			continue
		}
		works, err := source.Search(ctx, params)
		if err != nil {
			// See the identical ctx.Err() guard in the named-source branch
			// above: a cancelled caller must not be recorded as this
			// backend's failure.
			if ctx.Err() == nil {
				failures = append(failures, m.recordFailure(source.Name(), err))
			}
			hard = append(hard, fmt.Errorf("%s: %w", source.Name(), err))
			continue
		}
		m.clearFailure(source.Name())
		results = append(results, withSource(works, source.Name()))
	}
	if len(results) == 0 {
		return nil, failures, errors.Join(hard...)
	}
	return finalize(results, params), failures, nil
}

func withSource(works []DiscoveredWork, name string) []DiscoveredWork {
	for _, discovered := range works {
		if discovered.Source == "" {
			tagged := append([]DiscoveredWork(nil), works...)
			for i := range tagged {
				if tagged[i].Source == "" {
					tagged[i].Source = name
				}
			}
			return tagged
		}
	}
	return works
}

// finalize turns per-backend results into the answer: dedupe, judge each title
// against the query, promote confident matches, then cut to the limit.
//
// The order matters. Truncating during the merge — as this did before ranking
// existed — discards rows the backend ranked low, which is exactly where a
// buried title match sits. Scoring has to see everything that was fetched or it
// cannot promote anything.
func finalize(results [][]DiscoveredWork, params SearchParams) []DiscoveredWork {
	merged := mergeWorks(results)
	rank(merged, params.Query)
	if params.Limit > 0 && len(merged) > params.Limit {
		return merged[:params.Limit]
	}
	return merged
}

// mergeWorks concatenates backend results in preference order, keeping the first
// copy of any work two backends both returned.
func mergeWorks(results [][]DiscoveredWork) []DiscoveredWork {
	capacity := 0
	for _, works := range results {
		capacity += len(works)
	}
	merged := make([]DiscoveredWork, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	for _, works := range results {
		for _, discovered := range works {
			key := discoveredWorkKey(discovered)
			if key != "" {
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
			}
			merged = append(merged, discovered)
		}
	}
	return merged
}

func discoveredWorkKey(discovered DiscoveredWork) string {
	doi := strings.TrimSpace(discovered.Work.DOI)
	if doi != "" {
		if normalized, err := work.NormalizeDOI(doi); err == nil {
			return "doi:" + normalized
		}
		return "doi:" + strings.ToLower(doi)
	}
	title := strings.Join(strings.Fields(strings.ToLower(discovered.Work.Title)), " ")
	if title == "" {
		return ""
	}
	return "title:" + title
}
