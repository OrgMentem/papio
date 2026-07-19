// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"papio/internal/work"
)

// Multi searches its sources in preference order and merges their results.
type Multi struct {
	sources []Source
}

// NewMulti returns a Source that fans a search across backends in preference
// order and merges results. With no explicit source parameter, the supplied
// sources are searched in their given order.
func NewMulti(sources ...Source) *Multi {
	return &Multi{sources: sources}
}

// Name identifies the composed backend.
func (m *Multi) Name() string {
	return "multi"
}

// Search queries selected backends sequentially so each backend remains
// independently bounded. A usable backend result is returned even when another
// configured source is unavailable.
func (m *Multi) Search(ctx context.Context, params SearchParams) ([]DiscoveredWork, error) {
	if m == nil || len(m.sources) == 0 {
		return nil, errors.New("discovery: no discovery sources are configured")
	}
	params = normalizeParams(params)
	if params.Source != "" {
		for _, source := range m.sources {
			if source != nil && source.Name() == params.Source {
				works, err := source.Search(ctx, params)
				if err != nil {
					return nil, err
				}
				return mergeWorks([][]DiscoveredWork{withSource(works, source.Name())}, params.Limit), nil
			}
		}
		return nil, fmt.Errorf("unknown discovery source %q", params.Source)
	}

	results := make([][]DiscoveredWork, 0, len(m.sources))
	failures := make([]error, 0, len(m.sources))
	for _, source := range m.sources {
		if source == nil {
			failures = append(failures, errors.New("discovery: configured source is nil"))
			continue
		}
		works, err := source.Search(ctx, params)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", source.Name(), err))
			continue
		}
		results = append(results, withSource(works, source.Name()))
	}
	if len(results) == 0 {
		return nil, errors.Join(failures...)
	}
	return mergeWorks(results, params.Limit), nil
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

func mergeWorks(results [][]DiscoveredWork, limit int) []DiscoveredWork {
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
			if limit > 0 && len(merged) >= limit {
				return merged
			}
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
