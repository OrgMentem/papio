// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
	"papio/internal/zotio"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return NewStore(db)
}

func createWatch(t *testing.T, watches *Store, input CreateInput) *Watch {
	t.Helper()
	watch, err := watches.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

func testWatchInput(query string) CreateInput {
	return CreateInput{
		Query: query, Filters: Filters{YearFrom: 2020, YearTo: 2026, OAOnly: true},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	}
}

func TestCreateDefaultsCollectionToQuery(t *testing.T) {
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{Query: "protein folding", CadenceHours: 24, PerRunCap: 2})
	if created.Collection != "protein folding" {
		t.Fatalf("collection = %q, want query default", created.Collection)
	}
}

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("neural retrieval"))
	if created.ID == 0 || created.Label != "neural retrieval" || created.Query != "neural retrieval" {
		t.Fatalf("created watch = %+v", created)
	}
	if created.Filters.YearFrom != 2020 || created.Filters.YearTo != 2026 || !created.Filters.OAOnly || created.Collection != "Reading" || created.PerRunCap != 2 {
		t.Fatalf("created filters/policy = %+v", created)
	}
	got, err := watches.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.CreatedAt == "" || !got.Enabled {
		t.Fatalf("get watch = %+v", got)
	}
	listed, err := watches.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("list = %+v", listed)
	}
	if err := watches.Remove(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.Get(ctx, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get removed watch error = %v, want sql.ErrNoRows", err)
	}
	if err := watches.Remove(ctx, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("remove absent watch error = %v, want sql.ErrNoRows", err)
	}
}

func TestDueSelectsEnabledExpiredWatches(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	due := createWatch(t, watches, testWatchInput("due"))
	notDue := createWatch(t, watches, testWatchInput("not due"))
	disabled := createWatch(t, watches, testWatchInput("disabled"))
	if err := watches.MarkRun(ctx, due.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := watches.MarkRun(ctx, notDue.ID, now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.S.DB().ExecContext(ctx, `UPDATE watches SET enabled = 0 WHERE id = ?`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := watches.Due(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != due.ID {
		t.Fatalf("due watches = %+v, want only %d", selected, due.ID)
	}
}

func TestDueSkipsCorruptLastRun(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	healthy := createWatch(t, watches, testWatchInput("healthy"))
	corrupt := createWatch(t, watches, testWatchInput("corrupt"))
	if _, err := watches.S.DB().ExecContext(ctx, `UPDATE watches SET last_run_at = ? WHERE id = ?`, "not-a-timestamp", corrupt.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := watches.Due(ctx, now)
	if err != nil {
		t.Fatalf("Due returned error on corrupt row: %v", err)
	}
	var sawHealthy, sawCorrupt bool
	for _, watch := range selected {
		switch watch.ID {
		case healthy.ID:
			sawHealthy = true
		case corrupt.ID:
			sawCorrupt = true
		}
	}
	if !sawHealthy {
		t.Fatalf("healthy watch %d missing from due result %+v", healthy.ID, selected)
	}
	if sawCorrupt {
		t.Fatalf("corrupt watch %d must be skipped, got %+v", corrupt.ID, selected)
	}
	got, err := watches.Get(ctx, corrupt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError == "" {
		t.Fatalf("corrupt watch last_error = %q, want recorded parse failure", got.LastError)
	}
}

type fakeDiscovery struct {
	works  []discovery.DiscoveredWork
	err    error
	params []discovery.SearchParams
}

func (f *fakeDiscovery) Search(_ context.Context, params discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	f.params = append(f.params, params)
	if f.err != nil {
		return nil, f.err
	}
	return append([]discovery.DiscoveredWork(nil), f.works...), nil
}

type fakeLookup struct {
	result   *zotio.LookupWorksResult
	err      error
	requests []zotio.LookupWorksRequest
}

func (f *fakeLookup) LookupWorks(_ context.Context, request zotio.LookupWorksRequest) (*zotio.LookupWorksResult, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeSubmitter struct {
	calls      []protocol.WorkRequest
	byRequest  map[string]string
	err        error
	failOnCall map[int]error
	auto       []*bool
}

func (f *fakeSubmitter) SubmitWithAutoImport(_ context.Context, request protocol.WorkRequest, auto *bool) (string, error) {
	f.calls = append(f.calls, request)
	f.auto = append(f.auto, auto)
	if f.err != nil {
		return "", f.err
	}
	if err := f.failOnCall[len(f.calls)]; err != nil {
		return "", err
	}
	if f.byRequest == nil {
		f.byRequest = make(map[string]string)
	}
	if jobID, found := f.byRequest[request.RequestID]; found {
		return jobID, nil
	}
	jobID := fmt.Sprintf("job-%d", len(f.byRequest)+1)
	f.byRequest[request.RequestID] = jobID
	return jobID, nil
}

type fakeNotifier struct{ messages []string }

func (f *fakeNotifier) Send(_ context.Context, message string) {
	f.messages = append(f.messages, message)
}

func discovered(doi, openAlex string) discovery.DiscoveredWork {
	return discovery.DiscoveredWork{
		Work:       work.Work{DOI: doi, Title: "Paper " + doi, Authors: []string{"Ada"}, Year: 2025},
		OpenAlexID: openAlex,
	}
}

func TestOpenAlexOnlyDiscoveryRetainsIdentifier(t *testing.T) {
	requests := requestsForDiscovered([]discovery.DiscoveredWork{
		discovered("", "https://openalex.org/W2741809807"),
	})
	if len(requests) != 1 || requests[0].Identifiers == nil || requests[0].Identifiers.OpenAlex != "W2741809807" {
		t.Fatalf("requests = %+v, want one request with its normalized OpenAlex identifier", requests)
	}
	manifest := batch.NewManifest(requests, "watch: protocol", "", time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	if err := manifest.Works[0].Work.Validate(); err != nil {
		t.Fatalf("OpenAlex-only discovery request is not protocol-valid: %v (%+v)", err, manifest.Works[0].Work)
	}
}

func TestRunnerDeduplicatesCapsManifestsAndNotifies(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watch := createWatch(t, watches, testWatchInput("retrieval"))
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	discoveryFake := &fakeDiscovery{works: []discovery.DiscoveredWork{
		discovered("10.1000/owned", "W1001"),
		discovered("10.1000/new-one", "W1002"),
		discovered("10.1000/new-one", "W1999"),
		discovered("10.1000/new-two", "W1003"),
		discovered("10.1000/new-three", "W1004"),
	}}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
		{Status: zotio.OwnershipOwnedWithPDF},
		{Status: zotio.OwnershipNotOwned},
		{Status: zotio.OwnershipNotOwned},
		{Status: zotio.OwnershipNotOwned},
	}}}
	submitter := &fakeSubmitter{}
	notifier := &fakeNotifier{}
	runner := &Runner{
		Store: watches, Discovery: discoveryFake, Lookup: lookup, Submitter: submitter,
		Notifier: notifier, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	result, err := runner.Run(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 2 || result.ManifestID == "" {
		t.Fatalf("run result = %+v", result)
	}
	if len(discoveryFake.params) != 1 || discoveryFake.params[0].Limit != 6 || !discoveryFake.params[0].Slim || !discoveryFake.params[0].OAOnly || discoveryFake.params[0].YearFrom != 2020 {
		t.Fatalf("discovery params = %+v", discoveryFake.params)
	}
	if len(lookup.requests) != 1 || len(lookup.requests[0].Works) != 4 {
		t.Fatalf("lookup requests = %+v", lookup.requests)
	}
	if len(submitter.calls) != 2 || submitter.calls[0].Identifiers.DOI != "10.1000/new-one" || submitter.calls[1].Identifiers.DOI != "10.1000/new-two" {
		t.Fatalf("submitted calls = %+v", submitter.calls)
	}
	for _, auto := range submitter.auto {
		if auto == nil || !*auto {
			t.Fatalf("watch did not force auto-import: %v", auto)
		}
	}
	manifest, err := batch.Load(runner.DataDir, result.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Label != "watch: retrieval" || manifest.Collection != "Reading" || len(manifest.Works) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Works[0].JobID != "job-1" || manifest.Works[1].JobID != "job-2" {
		t.Fatalf("manifest jobs = %+v", manifest.Works)
	}
	if len(notifier.messages) != 1 || notifier.messages[0] != "watch retrieval: 2 new papers queued" {
		t.Fatalf("notifications = %+v", notifier.messages)
	}
	requestIDs := []string{submitter.calls[0].RequestID, submitter.calls[1].RequestID}
	now = now.Add(24 * time.Hour)

	second, err := runner.Run(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestID == result.ManifestID || len(submitter.byRequest) != 2 {
		t.Fatalf("repeat run = %+v; unique requests = %+v", second, submitter.byRequest)
	}
	if submitter.calls[2].RequestID != requestIDs[0] || submitter.calls[3].RequestID != requestIDs[1] {
		t.Fatalf("repeat request IDs = %q, %q; want %q, %q", submitter.calls[2].RequestID, submitter.calls[3].RequestID, requestIDs[0], requestIDs[1])
	}
}

func TestRunnerRecordsZeroRunsWithoutNotification(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watch := createWatch(t, watches, testWatchInput("owned"))
	notifier := &fakeNotifier{}
	runner := &Runner{
		Store:     watches,
		Discovery: &fakeDiscovery{works: []discovery.DiscoveredWork{discovered("10.1000/owned", "W2001")}},
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipOwnedWithPDF}}}},
		Submitter: &fakeSubmitter{}, Notifier: notifier, DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	}
	result, err := runner.Run(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 0 || result.ManifestID != "" || len(notifier.messages) != 0 {
		t.Fatalf("zero-work result = %+v, notifications = %+v", result, notifier.messages)
	}
	stored, err := watches.Get(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastRunAt == "" || stored.ConsecutiveFailures != 0 {
		t.Fatalf("stored zero-work run = %+v", stored)
	}
}

func TestRunnerRecordsPartialSubmissionFailureAsDegraded(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watch := createWatch(t, watches, testWatchInput("partial"))
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	submitter := &fakeSubmitter{failOnCall: map[int]error{2: errors.New("submit failed")}}
	runner := &Runner{
		Store: watches,
		Discovery: &fakeDiscovery{works: []discovery.DiscoveredWork{
			discovered("10.1000/partial-one", "W3001"),
			discovered("10.1000/partial-two", "W3002"),
		}},
		Lookup: &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
			{Status: zotio.OwnershipNotOwned}, {Status: zotio.OwnershipNotOwned},
		}}},
		Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	result, err := runner.Run(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 || result.Failed != 1 {
		t.Fatalf("partial result = %+v", result)
	}
	manifest, err := batch.Load(runner.DataDir, result.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Works[0].Status != "submitted" || manifest.Works[1].Status != "submission_failed" {
		t.Fatalf("partial manifest = %+v", manifest.Works)
	}
	stored, err := watches.Get(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConsecutiveFailures != 0 || stored.LastError != "1 of 2 watch submissions failed" || stored.LastRunAt == "" {
		t.Fatalf("degraded watch state = %+v", stored)
	}
}

func TestRunnerFailsWhenEverySubmissionFails(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watch := createWatch(t, watches, testWatchInput("all fail"))
	runner := &Runner{
		Store:     watches,
		Discovery: &fakeDiscovery{works: []discovery.DiscoveredWork{discovered("10.1000/all-fail", "W4001")}},
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: &fakeSubmitter{err: errors.New("submit failed")}, DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	}
	result, err := runner.Run(ctx, watch.ID)
	if err == nil || !strings.Contains(err.Error(), "all 1 watch submissions failed") || result.Failed != 1 {
		t.Fatalf("all-failed result = %+v, error = %v", result, err)
	}
	stored, err := watches.Get(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConsecutiveFailures != 1 || stored.LastError != "all 1 watch submissions failed" {
		t.Fatalf("all-failed watch state = %+v", stored)
	}
}

func TestRunnerCountsFailuresAndAutoDisables(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watch := createWatch(t, watches, testWatchInput("broken"))
	notifier := &fakeNotifier{}
	runner := &Runner{
		Store: watches, Discovery: &fakeDiscovery{err: errors.New("OpenAlex unavailable")},
		Lookup: &fakeLookup{}, Submitter: &fakeSubmitter{}, Notifier: notifier, DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	}
	for attempt := 1; attempt <= DisableAfterFailures; attempt++ {
		result, err := runner.Run(ctx, watch.ID)
		if err == nil || !strings.Contains(err.Error(), "discovery search") {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
		if result.ConsecutiveFailures != attempt || result.Disabled != (attempt == DisableAfterFailures) {
			t.Fatalf("attempt %d result = %+v", attempt, result)
		}
	}
	stored, err := watches.Get(ctx, watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled || stored.ConsecutiveFailures != DisableAfterFailures || stored.LastError == "" {
		t.Fatalf("stored failed watch = %+v", stored)
	}
	if len(notifier.messages) != 1 || notifier.messages[0] != "watch broken disabled after 5 consecutive failures" {
		t.Fatalf("failure notifications = %+v", notifier.messages)
	}
}
