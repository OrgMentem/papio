// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"reflect"
	"testing"

	"papio/internal/ownership"
	"papio/internal/work"
)

type fakeHoldingsLookup struct {
	enabled bool
	result  ownership.Result
	queries []ownership.Query
	calls   int
}

func (f *fakeHoldingsLookup) Enabled() bool { return f.enabled }

func (f *fakeHoldingsLookup) Lookup(_ context.Context, queries []ownership.Query) ownership.Result {
	f.calls++
	f.queries = queries
	return f.result
}

func presentClaim(source string, matched ownership.Identifier) ownership.Claim {
	return ownership.Claim{
		Source:        source,
		Matched:       matched,
		RecordPresent: true,
		Artifact:      ownership.ArtifactPresent,
	}
}

func titleWork(title string) DiscoveredWork {
	return DiscoveredWork{Work: work.Work{Title: title}}
}

func doiWork(doi string) DiscoveredWork {
	return DiscoveredWork{Work: work.Work{DOI: doi}}
}

// ClassifyHoldings is the non-Zotero ownership path (ADR-0008). Its contract is
// deliberately different from ClassifyOwnership's: there is no item key to
// report, a citation-only record still counts as "in library" for annotation,
// and an incomplete lookup must never be reported as a clean "not held".
func TestClassifyHoldings(t *testing.T) {
	staleAnnotations := []DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/first"}, Owned: true, OwnedItemKey: "STALE001"},
		{Work: work.Work{DOI: "10.1000/second"}, Owned: true, OwnedItemKey: "STALE002"},
	}
	ownEverything := ownership.Result{Works: []ownership.WorkResult{
		{Claims: []ownership.Claim{presentClaim("holdings-file", ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/first"})}},
		{Claims: []ownership.Claim{presentClaim("holdings-file", ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/second"})}},
	}}

	cases := []struct {
		name string
		// works is mutated in place by ClassifyHoldings, as production does.
		works []DiscoveredWork
		// fake nil means a nil HoldingsLookup interface, i.e. holdings are
		// not configured at all.
		fake        *fakeHoldingsLookup
		wantOwned   []bool
		wantKeys    []string
		wantWarning string
		wantCalls   int
	}{
		{
			// A nil lookup means the caller has no holdings source. Whatever
			// Owned flags the works arrived with are not evidence, so they must
			// be cleared rather than surviving into the response.
			name:        "nil lookup clears stale annotations",
			works:       append([]DiscoveredWork(nil), staleAnnotations...),
			fake:        nil,
			wantOwned:   []bool{false, false},
			wantKeys:    []string{"", ""},
			wantWarning: "",
			wantCalls:   0,
		},
		{
			// A configured-but-disabled registry is never asked. Returning its
			// canned "owned" answer would be a claim with no source behind it.
			name:        "disabled lookup is not consulted and clears annotations",
			works:       append([]DiscoveredWork(nil), staleAnnotations...),
			fake:        &fakeHoldingsLookup{enabled: false, result: ownEverything},
			wantOwned:   []bool{false, false},
			wantKeys:    []string{"", ""},
			wantWarning: "",
			wantCalls:   0,
		},
		{
			name:        "empty works never consults the lookup",
			works:       nil,
			fake:        &fakeHoldingsLookup{enabled: true, result: ownEverything},
			wantOwned:   nil,
			wantKeys:    nil,
			wantWarning: "",
			wantCalls:   0,
		},
		{
			// An explicit artifact-present claim is ownership. Outside Zotero
			// there is no stable per-item handle, so OwnedItemKey must stay
			// empty: a caller that routed an attachment on it would be routing
			// on a fabricated key.
			name:  "artifact present annotates owned without an item key",
			works: []DiscoveredWork{doiWork("10.1000/absent"), doiWork("10.1000/held")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{Works: []ownership.WorkResult{
				{},
				{Claims: []ownership.Claim{presentClaim("holdings-file", ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/held"})}},
			}}},
			wantOwned:   []bool{false, true},
			wantKeys:    []string{"", ""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// The user asked what they already have, not what is complete: a
			// citation-only entry is "in library" for annotation even though
			// ownership.Decide refuses to suppress an acquisition for it.
			name:  "citation-only record annotates owned",
			works: []DiscoveredWork{doiWork("10.1000/citation-only")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{Works: []ownership.WorkResult{
				{Claims: []ownership.Claim{{
					Source:        "bibtex-export",
					Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/citation-only"},
					RecordPresent: true,
					Artifact:      ownership.ArtifactUnknown,
				}}},
			}}},
			wantOwned:   []bool{true},
			wantKeys:    []string{""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// Staleness bars suppression, not annotation: the record is still
			// known to the library, so the search result says so.
			name:  "stale claim still annotates owned",
			works: []DiscoveredWork{doiWork("10.1000/stale")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{Works: []ownership.WorkResult{
				{Claims: []ownership.Claim{{
					Source:        "holdings-index",
					Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/stale"},
					RecordPresent: true,
					Artifact:      ownership.ArtifactPresent,
					Stale:         true,
				}}},
			}}},
			wantOwned:   []bool{true},
			wantKeys:    []string{""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// Two things at once, because only this claim shape separates them.
			// The claim asserts a file but not a record, so Owned can only come
			// from the artifact half of the condition — every claim the bundled
			// index builds sets RecordPresent, which would otherwise mask it.
			// And discovery sends no desired version, so a preprint held under
			// an arXiv id answers "is this in my library at all"; asking for
			// `published` here would report the held paper as absent.
			name:  "artifact-present claim annotates owned even without a record claim",
			works: []DiscoveredWork{{Work: work.Work{ArXiv: "2301.08745v2"}}},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{Works: []ownership.WorkResult{
				{Claims: []ownership.Claim{{
					Source:          "holdings-file",
					Matched:         ownership.Identifier{Kind: ownership.KindArXiv, Value: "2301.08745"},
					RecordPresent:   false,
					Artifact:        ownership.ArtifactPresent,
					ArtifactVersion: ownership.VersionPreprint,
				}}},
			}}},
			wantOwned:   []bool{true},
			wantKeys:    []string{""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// No claims from sources that all answered is a real negative.
			name:  "no claims leaves works unowned when every source answered",
			works: append([]DiscoveredWork(nil), staleAnnotations...),
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{
				Works:   []ownership.WorkResult{{}, {}},
				Sources: []ownership.SourceHealth{{Name: "holdings-file", Complete: true, EntryCount: 12}},
			}},
			wantOwned:   []bool{false, false},
			wantKeys:    []string{"", ""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// A short Works slice must stop the annotation loop, not index past
			// it or recycle the last answer onto the remaining works.
			name:  "truncated result leaves trailing works unannotated",
			works: []DiscoveredWork{doiWork("10.1000/held"), doiWork("10.1000/second"), doiWork("10.1000/third")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{Works: []ownership.WorkResult{
				{Claims: []ownership.Claim{presentClaim("holdings-file", ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/held"})}},
			}}},
			wantOwned:   []bool{true, false, false},
			wantKeys:    []string{"", "", ""},
			wantWarning: "",
			wantCalls:   1,
		},
		{
			// The data-integrity stake from the doc comment: an unreadable
			// source must surface as a warning naming it. If this returned "",
			// every unclassified result would read as a confident "not held",
			// and one unreadable holdings file would turn into a batch of
			// duplicate downloads. Names are sorted and the message carries
			// only names, never provider output.
			name:  "incomplete sources warn without inventing ownership",
			works: []DiscoveredWork{doiWork("10.1000/first"), doiWork("10.1000/second")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{
				Works: []ownership.WorkResult{{}, {}},
				Sources: []ownership.SourceHealth{
					{Name: "zotero-csv", Complete: false, FailureCode: ownership.FailureUnreadable},
					{Name: "bibtex-export", Complete: true, EntryCount: 3},
					{Name: "calibre", Complete: false, FailureCode: ownership.FailureTimeout},
				},
			}},
			wantOwned:   []bool{false, false},
			wantKeys:    []string{"", ""},
			wantWarning: "library sources unavailable (calibre, zotero-csv); some search results are unclassified",
			wantCalls:   1,
		},
		{
			// Incompleteness does not discard the positive evidence that did
			// arrive: a healthy source's claim still annotates while the
			// warning covers the works nobody could answer for.
			name:  "incomplete sources still annotate the works that answered",
			works: []DiscoveredWork{doiWork("10.1000/held"), doiWork("10.1000/unknown")},
			fake: &fakeHoldingsLookup{enabled: true, result: ownership.Result{
				Works: []ownership.WorkResult{
					{Claims: []ownership.Claim{presentClaim("bibtex-export", ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/held"})}},
					{},
				},
				Sources: []ownership.SourceHealth{
					{Name: "bibtex-export", Complete: true, EntryCount: 3},
					{Name: "zotero-csv", Complete: false, FailureCode: ownership.FailureUnreadable},
				},
			}},
			wantOwned:   []bool{true, false},
			wantKeys:    []string{"", ""},
			wantWarning: "library sources unavailable (zotero-csv); some search results are unclassified",
			wantCalls:   1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var lookup HoldingsLookup
			if testCase.fake != nil {
				lookup = testCase.fake
			}
			warning := ClassifyHoldings(context.Background(), testCase.works, lookup)
			if warning != testCase.wantWarning {
				t.Fatalf("warning = %q, want %q", warning, testCase.wantWarning)
			}
			for index, wantOwned := range testCase.wantOwned {
				if got := testCase.works[index].Owned; got != wantOwned {
					t.Fatalf("works[%d].Owned = %t, want %t", index, got, wantOwned)
				}
				if got := testCase.works[index].OwnedItemKey; got != testCase.wantKeys[index] {
					t.Fatalf("works[%d].OwnedItemKey = %q, want %q", index, got, testCase.wantKeys[index])
				}
			}
			if testCase.fake != nil && testCase.fake.calls != testCase.wantCalls {
				t.Fatalf("lookup calls = %d, want %d", testCase.fake.calls, testCase.wantCalls)
			}
		})
	}
}

// The queries are the whole interface to a holdings source: an identifier that
// does not survive normalization here can never match, so a work the library
// holds would be reported as absent and re-downloaded. Discovery also asks the
// version-neutral question, so DesiredVersion and EntityKind stay empty.
func TestClassifyHoldingsBuildsExactIdentifierQueries(t *testing.T) {
	works := []DiscoveredWork{
		{Work: work.Work{DOI: "https://doi.org/10.1000/Mixed.Case"}},
		{Work: work.Work{ArXiv: "arXiv:2301.08745v2"}},
		{Work: work.Work{PMID: "pmid:0031452104"}},
		{Work: work.Work{DOI: "10.1000/all", ArXiv: "2301.09999", PMID: "31452105"}},
		// No matchable identifier: a title is never matched, because a
		// false-positive "already held" withholds requested work.
		titleWork("An untitled preprint with no identifiers"),
	}
	lookup := &fakeHoldingsLookup{enabled: true}

	if warning := ClassifyHoldings(context.Background(), works, lookup); warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	if lookup.calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", lookup.calls)
	}
	want := []ownership.Query{
		{Identifiers: []ownership.Identifier{{Kind: ownership.KindDOI, Value: "10.1000/mixed.case"}}},
		{Identifiers: []ownership.Identifier{{Kind: ownership.KindArXiv, Value: "2301.08745"}}},
		{Identifiers: []ownership.Identifier{{Kind: ownership.KindPMID, Value: "31452104"}}},
		{Identifiers: []ownership.Identifier{
			{Kind: ownership.KindDOI, Value: "10.1000/all"},
			{Kind: ownership.KindArXiv, Value: "2301.09999"},
			{Kind: ownership.KindPMID, Value: "31452105"},
		}},
		{},
	}
	if !reflect.DeepEqual(lookup.queries, want) {
		t.Fatalf("queries = %+v, want %+v", lookup.queries, want)
	}
}
