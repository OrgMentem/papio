// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"papio/internal/discovery"
	"papio/internal/zotio"
)

// fakePager is a PagedDiscovery over fixed per-source result lists. A token is
// "<query>|<offset>", so a token issued for another query is rejected the way
// discovery binds real tokens to their search.
type fakePager struct {
	order   []string
	results map[string][]discovery.DiscoveredWork
	// fail makes every deeper page (non-empty token) of a source fail, the
	// way a spent source budget does.
	fail  map[string]error
	calls []pagerCall
}

type pagerCall struct {
	source string
	token  string
	limit  int
}

func (f *fakePager) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return nil, errors.New("fakePager: the runner must use SearchPages")
}

func (f *fakePager) SearchPages(_ context.Context, params discovery.SearchParams, tokens map[string]string) (discovery.MultiPage, error) {
	names := f.order
	if params.Source != "" {
		names = []string{params.Source}
	}
	var result discovery.MultiPage
	answered := 0
	for _, name := range names {
		token := tokens[name]
		f.calls = append(f.calls, pagerCall{source: name, token: token, limit: params.Limit})
		page := f.page(name, params, token)
		result.Sources = append(result.Sources, page)
		if page.State != discovery.PageFailed {
			answered++
			result.Works = append(result.Works, page.Works...)
		}
	}
	if answered == 0 {
		return result, errors.New("every searched source failed")
	}
	return result, nil
}

func (f *fakePager) page(name string, params discovery.SearchParams, token string) discovery.SourcePage {
	offset := 0
	if token != "" {
		query, raw, ok := strings.Cut(token, "|")
		parsed, err := strconv.Atoi(raw)
		if !ok || err != nil || query != params.Query {
			return failedPage(name, token, fmt.Errorf("%w: issued for another search", discovery.ErrInvalidPageToken))
		}
		if err := f.fail[name]; err != nil {
			return failedPage(name, token, err)
		}
		offset = parsed
	}
	all := f.results[name]
	offset = min(offset, len(all))
	end := min(offset+params.Limit, len(all))
	page := discovery.SourcePage{Source: name, State: discovery.PageExhausted, Works: withSourceName(all[offset:end], name)}
	if end < len(all) {
		page.State = discovery.PageMore
		page.Next = fmt.Sprintf("%s|%d", params.Query, end)
	}
	return page
}

func (f *fakePager) tokens(source string) []string {
	tokens := make([]string, 0)
	for _, call := range f.calls {
		if call.source == source {
			tokens = append(tokens, call.token)
		}
	}
	return tokens
}

func failedPage(name, token string, err error) discovery.SourcePage {
	return discovery.SourcePage{
		Source: name, State: discovery.PageFailed, Next: token, Err: err,
		Failure: &discovery.BackendFailure{Source: name, Message: discovery.SanitizeError(err)},
	}
}

func withSourceName(works []discovery.DiscoveredWork, name string) []discovery.DiscoveredWork {
	out := append([]discovery.DiscoveredWork(nil), works...)
	for i := range out {
		out[i].Source = name
	}
	return out
}

// doiOwnership answers Zotio ownership by DOI, whatever the page size.
type doiOwnership struct {
	owned map[string]bool
}

func (f *doiOwnership) LookupWorks(_ context.Context, request zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error) {
	result := &zotio.LookupWorksResult{Works: make([]zotio.WorkOwnership, len(request.Works))}
	for i, lookup := range request.Works {
		result.Works[i].Status = zotio.OwnershipNotOwned
		if f.owned[lookup.DOI] {
			result.Works[i].Status = zotio.OwnershipOwnedWithPDF
		}
	}
	return result, nil
}

// papers returns works 10.1000/<prefix>-<from> up to but excluding <to>.
func papers(prefix string, from, to int) []discovery.DiscoveredWork {
	works := make([]discovery.DiscoveredWork, 0, to-from)
	for i := from; i < to; i++ {
		works = append(works, discovered(fmt.Sprintf("10.1000/%s-%d", prefix, i), ""))
	}
	return works
}

func ownedDOIs(works []discovery.DiscoveredWork) map[string]bool {
	owned := make(map[string]bool, len(works))
	for _, work := range works {
		owned[work.Work.DOI] = true
	}
	return owned
}

type pagedHarness struct {
	watches   *Store
	watch     *Watch
	pager     *fakePager
	submitter *fakeSubmitter
	notifier  *fakeNotifier
	runner    *Runner
	now       time.Time
}

// newPagedHarness builds a watch with a per-run cap of 10, so its first page
// asks for 25 results, over a paged discovery source.
func newPagedHarness(t *testing.T, mode string, pager *fakePager, owned map[string]bool) *pagedHarness {
	t.Helper()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: mode, Query: "reliance", Collection: "Reading", CadenceHours: 24, PerRunCap: 10,
	})
	h := &pagedHarness{
		watches:   watches,
		watch:     watched,
		pager:     pager,
		submitter: &fakeSubmitter{},
		notifier:  &fakeNotifier{},
		now:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	h.runner = &Runner{
		Store: watches, Discovery: pager, Lookup: &doiOwnership{owned: owned},
		Submitter: h.submitter, Notifier: h.notifier, DataDir: t.TempDir(),
		Now: func() time.Time { return h.now },
	}
	return h
}

func (h *pagedHarness) run(t *testing.T) *RunResult {
	t.Helper()
	h.now = h.now.Add(24 * time.Hour)
	result, err := h.runner.Run(context.Background(), h.watch.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

func (h *pagedHarness) stored(t *testing.T) *Watch {
	t.Helper()
	stored, err := h.watches.Get(context.Background(), h.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (h *pagedHarness) coverage(t *testing.T, source string) SourceCoverage {
	t.Helper()
	coverage, err := h.watches.ScanCoverage(context.Background(), h.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return coverage[source]
}

func (h *pagedHarness) pendingDOIs(t *testing.T) []string {
	t.Helper()
	entries, err := h.watches.Digest(context.Background(), h.watch.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	dois := make([]string, 0, len(entries))
	for _, entry := range entries {
		dois = append(dois, entry.DOI)
	}
	return dois
}

// A mature query whose whole first page is already handled must still reach
// the new paper on page two, in both watch modes, and a run that read to the
// source's end is a clean success with no position left to resume.
func TestPagedScanReachesNewWorkBehindAHandledFirstPage(t *testing.T) {
	ctx := context.Background()
	handled := papers("seen", 0, 25)
	results := map[string][]discovery.DiscoveredWork{
		"openalex": append(append([]discovery.DiscoveredWork(nil), handled...), discovered("10.1000/new", "")),
	}

	t.Run("acquire skips owned works", func(t *testing.T) {
		pager := &fakePager{order: []string{"openalex"}, results: results}
		h := newPagedHarness(t, ModeAcquire, pager, ownedDOIs(handled))
		result := h.run(t)
		if result.Queued != 1 || len(h.submitter.calls) != 1 || h.submitter.calls[0].Identifiers.DOI != "10.1000/new" {
			t.Fatalf("result = %+v, submitted = %+v; want only the page-two DOI queued", result, h.submitter.calls)
		}
		if got := pager.tokens("openalex"); len(got) != 2 || got[0] != "" || got[1] != "reliance|25" {
			t.Fatalf("openalex tokens = %q; want the first page, then its continuation", got)
		}
		if result.Degraded || h.stored(t).LastError != "" {
			t.Fatalf("result = %+v, last_error = %q; a scan that reached the end is complete", result, h.stored(t).LastError)
		}
		if coverage := h.coverage(t, "openalex"); coverage.NextToken != "" || coverage.State != string(discovery.PageExhausted) {
			t.Fatalf("coverage = %+v; want the position cleared at exhaustion", coverage)
		}
	})

	t.Run("alert skips works its digest already has", func(t *testing.T) {
		pager := &fakePager{order: []string{"openalex"}, results: results}
		h := newPagedHarness(t, ModeAlert, pager, nil)
		// The first page's works were reported earlier and then cleared:
		// cleared entries must still never be reported again.
		entries := make([]DigestEntry, 0, len(handled))
		for _, work := range handled {
			entries = append(entries, DigestEntry{WorkKey: work.Work.DOI, Title: work.Work.Title, DOI: work.Work.DOI})
		}
		if _, err := h.watches.RecordDigest(ctx, h.watch.ID, h.now, entries); err != nil {
			t.Fatal(err)
		}
		if _, err := h.watches.ClearDigest(ctx, h.watch.ID); err != nil {
			t.Fatal(err)
		}
		result := h.run(t)
		if result.Reported != 1 {
			t.Fatalf("result = %+v; want the page-two DOI reported", result)
		}
		if got := h.pendingDOIs(t); len(got) != 1 || got[0] != "10.1000/new" {
			t.Fatalf("pending digest = %q; want only the page-two DOI", got)
		}
		if len(h.notifier.intents) != 1 {
			t.Fatalf("alerts = %+v; want one alert for the new work", h.notifier.messages)
		}
	})
}

// A run that spends its page budget with results left is recorded as an
// incomplete scan, keeps its position, and the next run continues from that
// position instead of re-reading the same pages.
func TestPagedScanRecordsScanLimitAndResumes(t *testing.T) {
	owned := papers("owned", 0, 110)
	pager := &fakePager{order: []string{"openalex"}, results: map[string][]discovery.DiscoveredWork{
		"openalex": append(append([]discovery.DiscoveredWork(nil), owned...), discovered("10.1000/deep", "")),
	}}
	h := newPagedHarness(t, ModeAcquire, pager, ownedDOIs(owned))

	first := h.run(t)
	if first.Queued != 0 || !first.Degraded {
		t.Fatalf("first run = %+v; want nothing queued and an incomplete scan", first)
	}
	if got := pager.tokens("openalex"); len(got) != ScanPagesPerSource {
		t.Fatalf("openalex pages = %q; want exactly %d", got, ScanPagesPerSource)
	}
	// A deeper page larger than the first page's cap fails on real
	// providers (a 50-work OpenAlex page outgrows the discovery body cap),
	// so no page may ask for more.
	for _, call := range pager.calls {
		if call.limit > scanPageLimit {
			t.Fatalf("page calls = %+v; no page may ask for more than %d results", pager.calls, scanPageLimit)
		}
	}
	stored := h.stored(t)
	if stored.ConsecutiveFailures != 0 || stored.LastRunAt == "" || !strings.Contains(stored.LastError, "openalex: scan limit") {
		t.Fatalf("watch = %+v; want an advanced run naming the scan limit", stored)
	}
	// Four pages of 25.
	if coverage := h.coverage(t, "openalex"); coverage.NextToken != "reliance|100" || coverage.State != string(discovery.PageMore) {
		t.Fatalf("coverage = %+v; want the position after the last page read", coverage)
	}

	pager.calls = nil
	second := h.run(t)
	if got := pager.tokens("openalex"); len(got) != 2 || got[0] != "" || got[1] != "reliance|100" {
		t.Fatalf("second run tokens = %q; want the first page, then the stored position", got)
	}
	if second.Queued != 1 || second.Degraded || h.submitter.calls[0].Identifiers.DOI != "10.1000/deep" {
		t.Fatalf("second run = %+v, submitted = %+v; want the deep DOI queued from a complete scan", second, h.submitter.calls)
	}
	if stored := h.stored(t); stored.LastError != "" {
		t.Fatalf("last_error = %q; want a clean run once the scan reached the end", stored.LastError)
	}
	if coverage := h.coverage(t, "openalex"); coverage.NextToken != "" || coverage.State != string(discovery.PageExhausted) {
		t.Fatalf("coverage = %+v; want the position cleared at exhaustion", coverage)
	}
}

// Editing a watch's search invalidates its stored position. The stale token is
// discarded and the walk restarts after the new first page; the run is not a
// failure.
func TestPagedScanDiscardsStalePositionAfterQueryChange(t *testing.T) {
	owned := papers("owned", 0, 250)
	pager := &fakePager{order: []string{"openalex"}, results: map[string][]discovery.DiscoveredWork{"openalex": owned}}
	h := newPagedHarness(t, ModeAcquire, pager, ownedDOIs(owned))
	h.run(t)
	if coverage := h.coverage(t, "openalex"); coverage.NextToken != "reliance|100" {
		t.Fatalf("coverage = %+v; want a stored position before the edit", coverage)
	}
	if _, err := h.watches.S.DB().ExecContext(context.Background(), `UPDATE watches SET query = 'trust' WHERE id = ?`, h.watch.ID); err != nil {
		t.Fatal(err)
	}

	pager.calls = nil
	result := h.run(t)
	got := pager.tokens("openalex")
	want := []string{"", "reliance|100", "trust|25", "trust|50", "trust|75"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens after the edit = %q; want %q (stale position tried once, then a restart after the first page)", got, want)
	}
	stored := h.stored(t)
	if stored.ConsecutiveFailures != 0 || strings.Contains(stored.LastError, "invalid") {
		t.Fatalf("watch = %+v; a stale position must not fail the watch", stored)
	}
	if !result.Degraded || !strings.Contains(stored.LastError, "scan limit") {
		t.Fatalf("result = %+v, last_error = %q; want the new walk's own scan limit", result, stored.LastError)
	}
	if coverage := h.coverage(t, "openalex"); coverage.NextToken != "trust|100" {
		t.Fatalf("coverage = %+v; want the new search's position", coverage)
	}
}

// A source whose deeper page fails (here, a spent budget) makes the run
// incomplete, not empty, and keeps that source's position for a retry while
// the healthy source advances.
func TestPagedScanFailedSourceKeepsItsPosition(t *testing.T) {
	openalex := papers("oa", 0, 400)
	arxiv := papers("ax", 0, 400)
	owned := ownedDOIs(append(append([]discovery.DiscoveredWork(nil), openalex...), arxiv...))
	pager := &fakePager{order: []string{"openalex", "arxiv"}, results: map[string][]discovery.DiscoveredWork{
		"openalex": openalex, "arxiv": arxiv,
	}}
	h := newPagedHarness(t, ModeAcquire, pager, owned)
	h.run(t)
	if coverage := h.coverage(t, "arxiv"); coverage.NextToken != "reliance|100" {
		t.Fatalf("arxiv coverage = %+v; want a stored position", coverage)
	}

	pager.fail = map[string]error{"arxiv": errors.New("arxiv request budget spent for today")}
	result := h.run(t)
	if !result.Degraded {
		t.Fatalf("result = %+v; a failed source makes the scan incomplete", result)
	}
	stored := h.stored(t)
	if stored.ConsecutiveFailures != 0 || !strings.Contains(stored.LastError, "arxiv: arxiv request budget spent") {
		t.Fatalf("watch = %+v; want a partial run naming the failed source", stored)
	}
	if coverage := h.coverage(t, "arxiv"); coverage.NextToken != "reliance|100" || coverage.State != string(discovery.PageFailed) {
		t.Fatalf("arxiv coverage = %+v; want the position kept for a retry", coverage)
	}
	if coverage := h.coverage(t, "openalex"); coverage.NextToken != "reliance|175" {
		t.Fatalf("openalex coverage = %+v; want the healthy source to advance", coverage)
	}
}

// A work reported from the first page and met again deeper — in a later run,
// or twice in one run — is never reported a second time.
func TestPagedScanReportsAWorkOnce(t *testing.T) {
	owned := papers("owned", 0, 25)
	pager := &fakePager{order: []string{"openalex"}, results: map[string][]discovery.DiscoveredWork{
		"openalex": {discovered("10.1000/a", "")},
	}}
	h := newPagedHarness(t, ModeAlert, pager, ownedDOIs(owned))
	if first := h.run(t); first.Reported != 1 {
		t.Fatalf("first run = %+v; want work a reported", first)
	}

	// The first page is now full of owned works; a sits on page two beside
	// a new work b.
	pager.results["openalex"] = append(append([]discovery.DiscoveredWork(nil), owned...), discovered("10.1000/a", ""), discovered("10.1000/b", ""))
	if second := h.run(t); second.Reported != 1 {
		t.Fatalf("second run = %+v; want only work b reported", second)
	}

	// a is on the first page again and repeats on the next page.
	pager.results["openalex"] = append(append([]discovery.DiscoveredWork{discovered("10.1000/a", "")}, owned...), discovered("10.1000/a", ""))
	if third := h.run(t); third.Reported != 0 {
		t.Fatalf("third run = %+v; want nothing reported", third)
	}
	if got := h.pendingDOIs(t); strings.Join(got, ",") != "10.1000/b,10.1000/a" {
		t.Fatalf("pending digest = %q; want a and b once each", got)
	}
}
