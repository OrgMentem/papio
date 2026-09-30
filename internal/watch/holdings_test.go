// Copyright 2026 OrgMentem. Licensed under MIT.

package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ownership"
	"papio/internal/ownershipsnapshot"
	"papio/internal/work"
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

const (
	heldPDFExport      = "@article{held,\n  title = {Held Work},\n  doi = {10.1000/held},\n}\n"
	citationOnlyExport = "@article{cited,\n  title = {Cited Work},\n  doi = {10.1000/cited},\n}\n"
)

// fileHoldings is a real registry over BibTeX exports on disk, as the daemon
// builds it from library.sources without zotio: "papis" (read from pdfPath)
// vouches for its PDFs, and "jabref" only for citation records.
func fileHoldings(t *testing.T, pdfPath string) *ownership.Registry {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "records.bib")
	if err := os.WriteFile(recordPath, []byte(citationOnlyExport), 0o600); err != nil {
		t.Fatal(err)
	}
	providers := make([]ownership.Provider, 0, 2)
	for _, source := range []config.LibrarySource{
		{Name: "papis", Kind: config.LibraryKindFile, Path: pdfPath, Format: "bibtex", Claim: config.LibraryClaimPDFPresent},
		{Name: "jabref", Kind: config.LibraryKindFile, Path: recordPath, Format: "bibtex", Claim: config.LibraryClaimRecordPresent},
	} {
		provider, err := ownershipsnapshot.NewProvider(source, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		providers = append(providers, provider)
	}
	return ownership.NewRegistry(providers...)
}

func heldPDFExportPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pdfs.bib")
	if err := os.WriteFile(path, []byte(heldPDFExport), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func heldCitedAndNewDiscoveries() []discovery.DiscoveredWork {
	return []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/held", Title: "Held Work", Authors: []string{"Ada"}, Year: 2026}},
		{Work: work.Work{DOI: "10.1000/cited", Title: "Cited Work", Authors: []string{"Bob"}, Year: 2026}},
		{Work: work.Work{DOI: "10.1000/new", Title: "New Work", Authors: []string{"Cara"}, Year: 2026}},
	}
}

func heldCitedAndNewDigest() []DigestEntry {
	return []DigestEntry{
		{WorkKey: "10.1000/held", Title: "Held Work", Authors: "Ada", Year: 2026, DOI: "10.1000/held"},
		{WorkKey: "10.1000/cited", Title: "Cited Work", Authors: "Bob", Year: 2026, DOI: "10.1000/cited"},
		{WorkKey: "10.1000/new", Title: "New Work", Authors: "Cara", Year: 2026, DOI: "10.1000/new"},
	}
}

func pendingDigestKeys(t *testing.T, watches *Store, watchID int64) string {
	t.Helper()
	entries, err := watches.Digest(context.Background(), watchID, 100)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.WorkKey)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// Without zotio, an alert watch classifies against library.sources exactly as
// an acquire watch does: a held PDF is not new, while a citation-only record
// is still worth surfacing because its full text is missing.
func TestAlertWatchUsesFileHoldingsWithoutZotio(t *testing.T) {
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "trust", CadenceHours: 24, PerRunCap: 5,
	})
	notifier := &fakeNotifier{}
	runner := &Runner{
		Store: watches, Discovery: &fakeDiscovery{works: heldCitedAndNewDiscoveries()},
		Holdings: fileHoldings(t, heldPDFExportPath(t)), Notifier: notifier, DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	result, err := runner.Run(context.Background(), watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reported != 2 {
		t.Fatalf("Reported = %d, want 2", result.Reported)
	}
	if got := pendingDigestKeys(t, watches, watched.ID); got != "10.1000/cited,10.1000/new" {
		t.Fatalf("digest keys = %q, want the citation-only and new works only", got)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("notifications = %+v, want one alert", notifier.messages)
	}
}

// Selected digest acquisition rechecks library.sources without a zotio
// executable: a held PDF is consumed without a job, and a citation-only record
// is acquired like any unheld work.
func TestAcquireDigestUsesFileHoldingsWithoutZotio(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "trust", CadenceHours: 24, PerRunCap: 5,
	})
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, watched.ID, now, heldCitedAndNewDigest()); err != nil {
		t.Fatal(err)
	}
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches, Holdings: fileHoldings(t, heldPDFExportPath(t)), Submitter: submitter,
		DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	queued, err := runner.AcquireDigest(ctx, watched.ID, []string{"10.1000/held", "10.1000/new"})
	if err != nil || queued != 1 {
		t.Fatalf("AcquireDigest() = %d, %v; want 1 queued", queued, err)
	}
	if len(submitter.calls) != 1 || submitter.calls[0].Identifiers == nil || submitter.calls[0].Identifiers.DOI != "10.1000/new" {
		t.Fatalf("submitted = %+v, want only the new work", submitter.calls)
	}
	if got := pendingDigestKeys(t, watches, watched.ID); got != "10.1000/cited" {
		t.Fatalf("pending digest = %q, want only the unselected citation", got)
	}

	queued, err = runner.AcquireDigest(ctx, watched.ID, []string{"10.1000/cited"})
	if err != nil || queued != 1 {
		t.Fatalf("AcquireDigest(cited) = %d, %v; want 1 queued", queued, err)
	}
	if len(submitter.calls) != 2 || submitter.calls[1].Identifiers == nil || submitter.calls[1].Identifiers.DOI != "10.1000/cited" {
		t.Fatalf("submitted = %+v, want the citation-only work acquired", submitter.calls)
	}
}

// An unreadable source must stop both stages: the alert run fails and records
// no discoveries, and digest acquisition creates no jobs and keeps its entries.
// Treating the unread source as empty would surface the held work as new.
func TestUnreadableFileHoldingsFailAlertAndDigestClosed(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "trust", CadenceHours: 24, PerRunCap: 5,
	})
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches, Discovery: &fakeDiscovery{works: heldCitedAndNewDiscoveries()},
		Holdings: fileHoldings(t, filepath.Join(t.TempDir(), "missing.bib")), Submitter: submitter,
		Notifier: notifier, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	if _, err := runner.Run(ctx, watched.ID); err == nil || !strings.Contains(err.Error(), "papis") {
		t.Fatalf("Run() error = %v, want a failure naming the unreadable source", err)
	}
	stored, err := watches.Get(ctx, watched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConsecutiveFailures != 1 || !strings.Contains(stored.LastError, "library sources unavailable") {
		t.Fatalf("stored watch = %+v, want a recorded holdings failure", stored)
	}
	if got := pendingDigestKeys(t, watches, watched.ID); got != "" {
		t.Fatalf("digest after failed run = %q, want no discoveries", got)
	}
	if len(notifier.offered) != 0 {
		t.Fatalf("offered alerts = %+v, want none", notifier.offered)
	}

	if _, err := watches.RecordDigest(ctx, watched.ID, now, heldCitedAndNewDigest()); err != nil {
		t.Fatal(err)
	}
	queued, err := runner.AcquireDigest(ctx, watched.ID, []string{"10.1000/held"})
	if err == nil || queued != 0 || !strings.Contains(err.Error(), "papis") {
		t.Fatalf("AcquireDigest() = %d, %v; want a failure naming the unreadable source", queued, err)
	}
	if len(submitter.calls) != 0 {
		t.Fatalf("submitted = %+v, want no jobs from an unverified lookup", submitter.calls)
	}
	if got := pendingDigestKeys(t, watches, watched.ID); got != "10.1000/cited,10.1000/held,10.1000/new" {
		t.Fatalf("pending digest = %q, want every entry kept", got)
	}
}
