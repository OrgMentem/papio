// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// SourcePage is one backend's share of a Multi paged search.
type SourcePage struct {
	Source string    `json:"source"`
	State  PageState `json:"state"`
	// Works are this backend's rows for the page, before cross-source
	// deduplication, so a caller can judge each backend's coverage alone.
	Works []DiscoveredWork `json:"works"`
	// Next is the token to persist for this backend: the continuation when
	// State is PageMore, the token the caller passed when State is
	// PageFailed (so the position is kept), and empty otherwise.
	Next string `json:"next,omitempty"`
	// Failure is set only when State is PageFailed.
	Failure *BackendFailure `json:"failure,omitempty"`
	// Err is the unsanitized cause behind Failure, kept for errors.Is
	// (ErrInvalidPageToken, a budget deferral). Never display it: use Failure.
	Err error `json:"-"`
}

// MultiPage is one page of a Multi paged search.
type MultiPage struct {
	// Works merges every answering backend's page in preference order,
	// deduplicated and ranked like Search, but never cut to the page size:
	// each backend's token has already moved past every row it returned, so
	// dropping one here would skip it for good.
	Works []DiscoveredWork `json:"works"`
	// Sources holds one entry per searched backend, in preference order.
	Sources []SourcePage `json:"sources"`
}

// SearchPages is the paged form of SearchPartial. tokens maps a backend name
// to the Next it returned last time; a missing or empty entry requests that
// backend's first page. Each backend resumes independently, so one backend
// running dry or failing never moves another's position.
//
// Every searched backend reports its own State: a backend that fails is
// PageFailed while the others still answer, and a backend that cannot page
// answers its single page as PageUnsupported. A token for a backend that is
// not searched (not configured, or excluded by params.Source) is ignored, and
// that backend is absent from Sources.
//
// A hard error is returned when no backend is configured, params.Source names
// an unknown backend, or every searched backend failed; in the last case the
// returned MultiPage still lists each backend's failed page.
func (m *Multi) SearchPages(ctx context.Context, params SearchParams, tokens map[string]string) (MultiPage, error) {
	if m == nil || len(m.sources) == 0 {
		return MultiPage{}, errors.New("discovery: no discovery sources are configured")
	}
	params = normalizeParams(params)
	selected := m.sources
	if params.Source != "" {
		selected = nil
		for _, source := range m.sources {
			if source != nil && source.Name() == params.Source {
				selected = []Source{source}
				break
			}
		}
		if selected == nil {
			return MultiPage{}, fmt.Errorf("unknown discovery source %q", params.Source)
		}
	}
	result := MultiPage{Sources: make([]SourcePage, 0, len(selected))}
	answered := make([][]DiscoveredWork, 0, len(selected))
	hard := make([]error, 0, len(selected))
	for _, source := range selected {
		if source == nil {
			hard = append(hard, errors.New("discovery: configured source is nil"))
			continue
		}
		page := m.searchSourcePage(ctx, source, params, tokens[source.Name()])
		result.Sources = append(result.Sources, page)
		if page.State == PageFailed {
			hard = append(hard, fmt.Errorf("%s: %w", page.Source, page.Err))
			continue
		}
		answered = append(answered, page.Works)
	}
	if len(answered) == 0 {
		return result, errors.Join(hard...)
	}
	result.Works = mergeWorks(answered)
	rank(result.Works, params.Query)
	return result, nil
}

// searchSourcePage fetches one backend's page and folds any error into a
// PageFailed entry, retaining real backend failures for diagnostics exactly
// as SearchPartial does.
func (m *Multi) searchSourcePage(ctx context.Context, source Source, params SearchParams, token string) SourcePage {
	name := source.Name()
	var page Page
	var err error
	if pager, ok := source.(PageSearcher); ok {
		page, err = pager.SearchPage(ctx, params, token)
		if err == nil {
			err = checkPage(page)
		}
	} else if token != "" {
		err = fmt.Errorf("%w: %s cannot page, so it never issued a token", ErrInvalidPageToken, name)
	} else {
		var works []DiscoveredWork
		works, err = source.Search(ctx, params)
		page = Page{Works: works, State: PageUnsupported}
	}
	if err != nil {
		failure := m.pageFailure(ctx, name, err)
		return SourcePage{Source: name, State: PageFailed, Next: token, Failure: &failure, Err: err}
	}
	m.clearFailure(name)
	return SourcePage{Source: name, State: page.State, Works: withSource(page.Works, name), Next: page.Next}
}

// checkPage rejects a backend page whose state and token disagree, which would
// otherwise send a caller back to page one or end its walk silently.
func checkPage(page Page) error {
	switch page.State {
	case PageMore:
		if page.Next == "" {
			return errors.New("discovery: backend reported more results without a page token")
		}
		return nil
	case PageExhausted, PageTruncated:
		if page.Next != "" {
			return fmt.Errorf("discovery: backend returned a page token with state %q", page.State)
		}
		return nil
	default:
		return fmt.Errorf("discovery: backend returned invalid page state %q", page.State)
	}
}

// pageFailure describes a failed page. Like SearchPartial it does not retain
// a failure caused by the caller giving up; nor one caused by the caller's
// own bad token, which says nothing about the backend's health.
func (m *Multi) pageFailure(ctx context.Context, name string, err error) BackendFailure {
	if ctx.Err() != nil || errors.Is(err, ErrInvalidPageToken) {
		return BackendFailure{Source: name, Message: SanitizeError(err), At: m.clock()}
	}
	return m.recordFailure(name, err)
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
	title := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(discovered.Work.Title))), " ")
	if title == "" {
		return ""
	}
	year := discovered.Work.Year
	authorsKey := normalizedAuthorsKey(discovered.Work.Authors)
	if year == 0 && authorsKey == "" {
		return ""
	}
	key := "title:" + title
	if year != 0 {
		key += fmt.Sprintf("|year:%d", year)
	}
	if authorsKey != "" {
		key += "|authors:" + authorsKey
	}
	return key
}

func normalizedAuthorsKey(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	normed := make([]string, 0, len(authors))
	for _, a := range authors {
		n := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(a))), " ")
		if n != "" {
			normed = append(normed, n)
		}
	}
	if len(normed) == 0 {
		return ""
	}
	sort.Strings(normed)
	return strings.Join(normed, "|")
}
