// Copyright 2026 OrgMentem. Licensed under MIT.

package watch

import (
	"context"
	"strings"
	"testing"
	"time"

	"papio/internal/discovery"
	"papio/internal/ownership"
	"papio/internal/work"
	"papio/internal/zotio"
)

type fakeHoldings struct {
	enabled bool
	result  ownership.Result
	queries [][]ownership.Query
}

func (f *fakeHoldings) Enabled() bool { return f.enabled }

func (f *fakeHoldings) Lookup(_ context.Context, queries []ownership.Query) ownership.Result {
	f.queries = append(f.queries, queries)
	result := f.result
	if len(result.Works) == 0 {
		result.Works = make([]ownership.WorkResult, len(queries))
	}
	return result
}

func holdingsWatchRunner(t *testing.T, holdings *fakeHoldings, works []discovery.DiscoveredWork) (*Runner, *fakeSubmitter, int64) {
	t.Helper()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAcquire, Query: "trust", CadenceHours: 24, PerRunCap: 5,
	})
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches, Discovery: &fakeDiscovery{works: works}, Holdings: holdings,
		Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	return runner, submitter, watched.ID
}

func TestAcquireWatchSkipsWorksAGenericSourceHolds(t *testing.T) {
	held := ownership.WorkResult{Claims: []ownership.Claim{{
		Source:        "papis",
		Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/held"},
		RecordPresent: true,
		Artifact:      ownership.ArtifactPresent,
	}}}
	holdings := &fakeHoldings{enabled: true, result: ownership.Result{
		Works:   []ownership.WorkResult{held, {}},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: true, EntryCount: 2}},
	}}
	runner, submitter, id := holdingsWatchRunner(t, holdings, []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/held", Title: "Held Work", Authors: []string{"Ada"}, Year: 2026}},
		{Work: work.Work{DOI: "10.1000/new", Title: "New Work", Authors: []string{"Bob"}, Year: 2026}},
	})

	result, err := runner.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 {
		t.Fatalf("Queued = %d, want 1", result.Queued)
	}
	if len(submitter.calls) != 1 || submitter.calls[0].Identifiers.DOI != "10.1000/new" {
		t.Fatalf("submitted = %+v, want only the unheld work", submitter.calls)
	}
}

// Automation must fail and retry rather than duplicate: one unreadable export
// would otherwise become a recurring burst of redundant acquisitions.
func TestAcquireWatchFailsWhenASourceIsUnreadable(t *testing.T) {
	holdings := &fakeHoldings{enabled: true, result: ownership.Result{
		Works:   []ownership.WorkResult{{}},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: false, FailureCode: ownership.FailureUnreadable}},
	}}
	runner, submitter, id := holdingsWatchRunner(t, holdings, []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/new", Title: "New Work", Authors: []string{"Ada"}, Year: 2026}},
	})

	_, err := runner.Run(context.Background(), id)
	if err == nil {
		t.Fatal("an incomplete lookup must fail the run")
	}
	if !strings.Contains(err.Error(), "papis") {
		t.Fatalf("error must name the unreadable source, got %v", err)
	}
	if len(submitter.calls) != 0 {
		t.Fatalf("nothing may be acquired on an unverified run, got %+v", submitter.calls)
	}
}

// A stale positive claim annotates but cannot suppress, so the watch must still
// acquire — the file may be gone.
func TestAcquireWatchAcquiresDespiteAStaleClaim(t *testing.T) {
	stale := ownership.WorkResult{Claims: []ownership.Claim{{
		Source:        "papis",
		Matched:       ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/stale"},
		RecordPresent: true,
		Artifact:      ownership.ArtifactPresent,
		Stale:         true,
	}}}
	holdings := &fakeHoldings{enabled: true, result: ownership.Result{
		Works:   []ownership.WorkResult{stale},
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: true, Stale: true, EntryCount: 1}},
	}}
	runner, submitter, id := holdingsWatchRunner(t, holdings, []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/stale", Title: "Stale Work", Authors: []string{"Ada"}, Year: 2026}},
	})

	result, err := runner.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 || len(submitter.calls) != 1 {
		t.Fatalf("result = %+v, submitted = %+v; a stale claim must not suppress", result, submitter.calls)
	}
}

// A disabled registry must leave the zotio path untouched.
func TestDisabledHoldingsFallsBackToZotio(t *testing.T) {
	holdings := &fakeHoldings{enabled: false}
	runner, _, id := holdingsWatchRunner(t, holdings, []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/new", Title: "New Work", Authors: []string{"Ada"}, Year: 2026}},
	})
	// No zotio Lookup is configured, so the run must fail by reaching that path
	// rather than silently answering from the disabled registry.
	if _, err := runner.Run(context.Background(), id); err == nil {
		t.Fatal("expected the zotio path to be taken and fail without a lookup")
	}
	if len(holdings.queries) != 0 {
		t.Fatalf("a disabled registry must not be consulted, got %+v", holdings.queries)
	}
}

func TestAlertWatchIgnoresIncompleteGenericHoldings(t *testing.T) {
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "trust", CadenceHours: 24, PerRunCap: 1,
	})
	holdings := &fakeHoldings{enabled: true, result: ownership.Result{
		Sources: []ownership.SourceHealth{{Name: "papis", Complete: false, FailureCode: ownership.FailureUnreadable}},
	}}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
		{Status: zotio.OwnershipOwnedWithPDF},
		{Status: zotio.OwnershipNotOwned},
	}}}
	runner := &Runner{
		Store: watches,
		Discovery: &fakeDiscovery{works: []discovery.DiscoveredWork{
			{Work: work.Work{DOI: "10.1000/owned", Title: "Owned Work", Authors: []string{"Ada"}, Year: 2026}},
			{Work: work.Work{DOI: "10.1000/new", Title: "New Work", Authors: []string{"Bob"}, Year: 2026}},
		}},
		Lookup: lookup, Holdings: holdings, DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	result, err := runner.Run(context.Background(), watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reported != 1 {
		t.Fatalf("Reported = %d, want 1", result.Reported)
	}
	if len(lookup.requests) != 1 {
		t.Fatalf("Zotio lookup calls = %d, want 1", len(lookup.requests))
	}
	if len(holdings.queries) != 0 {
		t.Fatalf("generic holdings must not be consulted for alerts, got %+v", holdings.queries)
	}
}
