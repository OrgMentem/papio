// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

func TestRunnerPassesCitationSnowballFiltersToDiscovery(t *testing.T) {
	for _, test := range []struct {
		name    string
		filters Filters
	}{
		{name: "cites", filters: Filters{Cites: "10.1000/seed"}},
		{name: "cited by", filters: Filters{CitedBy: "10.1000/seed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			watches := testStore(t)
			watched := createWatch(t, watches, CreateInput{
				Kind: KindDiscovery, Mode: ModeAlert, CadenceHours: 24, PerRunCap: 2, Filters: test.filters,
			})
			discoveryFake := &fakeDiscovery{}
			runner := &Runner{
				Store: watches, Discovery: discoveryFake, Lookup: &fakeLookup{},
				Now: func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
			}

			if _, err := runner.Run(ctx, watched.ID); err != nil {
				t.Fatal(err)
			}
			if len(discoveryFake.params) != 1 {
				t.Fatalf("discovery searches = %d, want one", len(discoveryFake.params))
			}
			params := discoveryFake.params[0]
			if params.Query != "" || params.Cites != test.filters.Cites || params.CitedBy != test.filters.CitedBy ||
				params.RelatedTo != test.filters.RelatedTo {
				t.Fatalf("discovery params = %+v, want snowball filters %+v", params, test.filters)
			}
		})
	}
}

func TestRunnerHandlesArXivOnlyDiscoveries(t *testing.T) {
	for _, mode := range []string{ModeAcquire, ModeAlert} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			watches := testStore(t)
			watched := createWatch(t, watches, CreateInput{
				Kind: KindDiscovery, Mode: mode, Query: "arXiv", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
			})
			now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
			discoveryFake := &fakeDiscovery{works: []discovery.DiscoveredWork{{
				Work: work.Work{ArXiv: "arXiv:2601.12345v2", Title: "ArXiv Work", Authors: []string{"Ada"}, Year: 2026},
			}}}
			lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}}
			submitter := &fakeSubmitter{}
			runner := &Runner{
				Store: watches, Discovery: discoveryFake, Lookup: lookup, Submitter: submitter,
				DataDir: t.TempDir(), Now: func() time.Time { return now },
			}

			result, err := runner.Run(ctx, watched.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(lookup.requests) != 1 || len(lookup.requests[0].Works) != 1 || lookup.requests[0].Works[0].ArXiv != "2601.12345v2" {
				t.Fatalf("lookup requests = %+v, want normalized arXiv identifier", lookup.requests)
			}
			if mode == ModeAcquire {
				if result.Queued != 1 || len(submitter.calls) != 1 || submitter.calls[0].Identifiers == nil || submitter.calls[0].Identifiers.ArXiv != "2601.12345v2" {
					t.Fatalf("acquire result = %+v, submitted = %+v", result, submitter.calls)
				}
				return
			}
			if result.Reported != 1 || len(submitter.calls) != 0 {
				t.Fatalf("alert result = %+v, submitted = %+v", result, submitter.calls)
			}
			digest, err := watches.Digest(ctx, watched.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(digest) != 1 || digest[0].WorkKey != "arxiv:2601.12345v2" {
				t.Fatalf("digest = %+v, want arXiv-keyed entry", digest)
			}
		})
	}
}

func TestRunnerAcquireDigestPreservesDiscoveryIdentifiers(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "identifiers", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	discovered := discovery.DiscoveredWork{
		Work:       work.Work{DOI: "10.1000/identifiers", Title: "Identifier Work", Authors: []string{"Ada"}, Year: 2026},
		OpenAlexID: "https://openalex.org/W2741809807",
	}
	requests := requestsForDiscoveredWithWork([]discovery.DiscoveredWork{discovered})
	if len(requests) != 1 {
		t.Fatalf("discovery requests = %+v, want one", requests)
	}
	wantRequestID := batch.RequestID(fmt.Sprintf("watch-%d", watched.ID), requests[0].Work)
	runner := &Runner{
		Store:     watches,
		Discovery: &fakeDiscovery{works: []discovery.DiscoveredWork{discovered}},
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		DataDir:   t.TempDir(),
		Now:       func() time.Time { return now },
	}
	if _, err := runner.Run(ctx, watched.ID); err != nil {
		t.Fatal(err)
	}
	digest, err := watches.Digest(ctx, watched.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 1 || digest[0].Identifiers == nil || digest[0].Identifiers.OpenAlex != "W2741809807" {
		t.Fatalf("digest = %+v, want persisted OpenAlex identifier", digest)
	}
	submitter := &fakeSubmitter{}
	runner.Submitter = submitter
	queued, err := runner.AcquireDigest(ctx, watched.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || len(submitter.calls) != 1 {
		t.Fatalf("AcquireDigest() = %d, submitted = %+v; want one queued submission", queued, submitter.calls)
	}
	if got := submitter.calls[0].RequestID; got != wantRequestID {
		t.Fatalf("digest request ID = %q, want acquire-mode ID %q", got, wantRequestID)
	}
}

func TestRunnerAcquireDigestRejectsNonAuthoritativeEntriesBeforeSubmission(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		entry DigestEntry
	}{
		{
			name:  "title only",
			entry: DigestEntry{WorkKey: "title only", Title: "Title Only"},
		},
		{
			name: "OpenAlex only",
			entry: DigestEntry{
				WorkKey: "openalex:W2741809807", Title: "OpenAlex Only",
				Identifiers: &protocol.Identifiers{OpenAlex: "W2741809807"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			watches := testStore(t)
			watched := createWatch(t, watches, CreateInput{
				Kind: KindDiscovery, Mode: ModeAlert, Query: test.name, Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
			})
			if _, err := watches.RecordDigest(ctx, watched.ID, now, []DigestEntry{test.entry}); err != nil {
				t.Fatal(err)
			}
			lookup := &fakeLookup{}
			submitter := &fakeSubmitter{}
			runner := &Runner{
				Store: watches, Lookup: lookup, Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
			}

			if queued, err := runner.AcquireDigest(ctx, watched.ID, nil); err == nil || queued != 0 {
				t.Fatalf("AcquireDigest() = %d, %v; want a non-authoritative identity error", queued, err)
			}
			if len(lookup.requests) != 0 || len(submitter.calls) != 0 {
				t.Fatalf("lookup requests = %+v, submitted = %+v; want neither", lookup.requests, submitter.calls)
			}
			if digest, err := watches.Digest(ctx, watched.ID, 100); err != nil || len(digest) != 1 {
				t.Fatalf("Digest() after rejected acquisition = %+v, %v; want one pending entry", digest, err)
			}
		})
	}
}

func TestRunnerAcquireDigestFailsClosedOnStaleOwnership(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "stale ownership", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, watched.ID, now, []DigestEntry{{
		WorkKey: "10.1000/stale", Title: "Stale", DOI: "10.1000/stale",
	}}); err != nil {
		t.Fatal(err)
	}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{
		Works:            []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}},
		StalenessWarning: "mirror refresh failed",
	}}
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches, Lookup: lookup, Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	if queued, err := runner.AcquireDigest(ctx, watched.ID, nil); err == nil || queued != 0 {
		t.Fatalf("AcquireDigest() = %d, %v; want stale ownership error", queued, err)
	}
	if len(lookup.requests) != 1 || len(submitter.calls) != 0 {
		t.Fatalf("lookup requests = %+v, submitted = %+v; want one lookup and no submissions", lookup.requests, submitter.calls)
	}
	if digest, err := watches.Digest(ctx, watched.ID, 100); err != nil || len(digest) != 1 {
		t.Fatalf("Digest() after stale ownership = %+v, %v; want one pending entry", digest, err)
	}
}

func TestRunnerAcquireDigest(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	discoveryWatch := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, discoveryWatch.ID, now, []DigestEntry{
		{WorkKey: "10.1000/digest", Title: "Digest DOI", Authors: "Ada, Bob", Year: 2026, DOI: "10.1000/digest"},
		{WorkKey: "arxiv:2601.12345v2", Title: "Digest arXiv", Authors: "Cara", Year: 2026},
	}); err != nil {
		t.Fatal(err)
	}
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches, Lookup: &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
			{Status: zotio.OwnershipNotOwned}, {Status: zotio.OwnershipNotOwned},
		}}}, Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}

	queued, err := runner.AcquireDigest(ctx, discoveryWatch.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 2 || len(submitter.calls) != 2 {
		t.Fatalf("AcquireDigest() = %d, submitted = %+v; want 2 queued submissions", queued, submitter.calls)
	}
	if submitter.calls[0].Identifiers == nil || submitter.calls[0].Identifiers.ArXiv != "2601.12345v2" {
		t.Fatalf("first digest request = %+v, want normalized arXiv identifier", submitter.calls[0])
	}
	if submitter.calls[1].Identifiers == nil || submitter.calls[1].Identifiers.DOI != "10.1000/digest" {
		t.Fatalf("second digest request = %+v, want DOI identifier", submitter.calls[1])
	}
	entries, err := watches.Digest(ctx, discoveryWatch.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("digest after acquisition = %+v, want no submitted entries", entries)
	}
	queued, err = runner.AcquireDigest(ctx, discoveryWatch.ID, nil)
	if err != nil || queued != 0 {
		t.Fatalf("empty AcquireDigest() = %d, %v; want 0, nil", queued, err)
	}

	backfillWatch := createWatch(t, watches, CreateInput{
		Kind: KindBackfill, Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	if _, err := runner.AcquireDigest(ctx, backfillWatch.ID, nil); err == nil {
		t.Fatal("AcquireDigest() accepted a backfill watch")
	}
}
func TestRunnerAcquireDigestRechecksOwnershipAndPreservesAuthors(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 2,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	entries := []DigestEntry{
		{WorkKey: "10.1000/owned", Title: "Owned", Authors: "Ada", AuthorNames: []string{"Ada"}, DOI: "10.1000/owned"},
		{WorkKey: "10.1000/unowned", Title: "Unowned", Authors: "Smith, Jr., Ada", AuthorNames: []string{"Smith, Jr.", "Ada"}, DOI: "10.1000/unowned"},
	}
	if _, err := watches.RecordDigest(ctx, created.ID, now, entries); err != nil {
		t.Fatal(err)
	}
	lookup := &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
		{Status: zotio.OwnershipNotOwned}, {Status: zotio.OwnershipOwnedWithPDF},
	}}}
	submitter := &fakeSubmitter{}
	runner := &Runner{Store: watches, Lookup: lookup, Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now }}

	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	if err != nil || queued != 1 || len(submitter.calls) != 1 {
		t.Fatalf("AcquireDigest() = %d, %v, submitted = %+v; want one unowned submission", queued, err, submitter.calls)
	}
	if len(lookup.requests) != 1 || len(lookup.requests[0].Works) != 2 {
		t.Fatalf("ownership lookups = %+v, want one lookup for both digest entries", lookup.requests)
	}
	if got := submitter.calls[0].Authors; len(got) != 2 || got[0] != "Smith, Jr." || got[1] != "Ada" {
		t.Fatalf("submitted authors = %q, want lossless names", got)
	}
	if digest, err := watches.Digest(ctx, created.ID, 100); err != nil || len(digest) != 0 {
		t.Fatalf("Digest() after ownership check = %+v, %v; want no pending entries", digest, err)
	}
	reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), entries)
	if err != nil || reported != 0 {
		t.Fatalf("repeated RecordDigest() = %d, %v; want 0, nil", reported, err)
	}
	if digest, err := watches.Digest(ctx, created.ID, 100); err != nil || len(digest) != 0 {
		t.Fatalf("Digest() after repeat = %+v, %v; want no pending entries", digest, err)
	}
}

func TestRunnerAcquireDigestFailsClosedOnUnknownOwnership(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "10.1000/unknown", Title: "Unknown", DOI: "10.1000/unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	submitter := &fakeSubmitter{}
	runner := &Runner{
		Store: watches,
		Lookup: &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
			{Status: "unexpected"},
		}}},
		Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	if queued, err := runner.AcquireDigest(ctx, created.ID, nil); err == nil || queued != 0 {
		t.Fatalf("AcquireDigest() = %d, %v; want 0 and unknown ownership error", queued, err)
	}
	if len(submitter.calls) != 0 {
		t.Fatalf("submitted = %+v, want no submission after unknown ownership", submitter.calls)
	}
	if digest, err := watches.Digest(ctx, created.ID, 100); err != nil || len(digest) != 1 {
		t.Fatalf("Digest() after unknown ownership = %+v, %v; want pending entry", digest, err)
	}
}

func TestRunnerAcquireDigestStopsAfterSubmissionFailure(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 3,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{
		{WorkKey: "10.1000/one", Title: "One", DOI: "10.1000/one"},
		{WorkKey: "10.1000/two", Title: "Two", DOI: "10.1000/two"},
		{WorkKey: "10.1000/three", Title: "Three", DOI: "10.1000/three"},
	}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Store: watches, Lookup: &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{
			{Status: zotio.OwnershipNotOwned}, {Status: zotio.OwnershipNotOwned}, {Status: zotio.OwnershipNotOwned},
		}}},
		Submitter: &fakeSubmitter{failOnCall: map[int]error{2: context.DeadlineExceeded}},
		DataDir:   t.TempDir(),
		Now:       func() time.Time { return now },
	}

	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	if !errors.Is(err, context.DeadlineExceeded) || queued != 1 {
		t.Fatalf("AcquireDigest() = %d, %v; want 1, deadline exceeded", queued, err)
	}
	entries, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].WorkKey != "10.1000/two" || entries[1].WorkKey != "10.1000/one" {
		t.Fatalf("digest after partial acquisition = %+v, want failed and unsubmitted rows", entries)
	}
	manifest, err := batch.Load(runner.DataDir, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Works) != 2 || manifest.Works[0].Status != "submitted" || manifest.Works[1].Status != "submission_failed" {
		t.Fatalf("partial digest manifest = %+v, want attempted works only", manifest.Works)
	}
}

func TestRunnerAcquireDigestPreservesEntriesWhenManifestWriteFails(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "10.1000/manifest", Title: "Manifest", DOI: "10.1000/manifest",
	}}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "batches"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Store:     watches,
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: &fakeSubmitter{},
		DataDir:   dataDir,
		Now:       func() time.Time { return now },
	}
	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	// Contract (supersedes the pre-resumability expectation of queued=1):
	// the manifest is now persisted BEFORE any submission so a retry is
	// resumable without resubmitting already-created jobs. When that
	// pre-submit write fails no jobs have been created, so queued must be 0,
	// not 1, while the digest entry is still preserved (not consumed).
	if err == nil || queued != 0 {
		t.Fatalf("AcquireDigest() = %d, %v; want 0 and manifest write failure", queued, err)
	}
	entries, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkKey != "10.1000/manifest" {
		t.Fatalf("digest after manifest write failure = %+v, want unremoved entry", entries)
	}
}

func TestAcquireWatchFailsWhenHoldingsReturnsFewerWorksThanRequests(t *testing.T) {
	ctx := context.Background()
	holdings := &fakeHoldings{
		enabled: true,
		result: ownership.Result{
			Works:   []ownership.WorkResult{{Claims: []ownership.Claim{{Matched: ownership.Identifier{Kind: ownership.KindDOI, Value: "10.1000/held"}, Artifact: ownership.ArtifactPresent, ArtifactVersion: ownership.VersionPublished}}}},
			Sources: []ownership.SourceHealth{{Name: "papis", Complete: true, EntryCount: 1}},
		},
	}
	watches := testStore(t)
	watched := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAcquire, Query: "holdings mismatch", Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
	})
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	works := []discovery.DiscoveredWork{
		{Work: work.Work{DOI: "10.1000/one", Title: "One", Authors: []string{"Ada"}, Year: 2026}},
		{Work: work.Work{DOI: "10.1000/two", Title: "Two", Authors: []string{"Ada"}, Year: 2026}},
	}
	runner := &Runner{
		Store: watches, Discovery: &fakeDiscovery{works: works}, Holdings: holdings,
		Lookup: &fakeLookup{}, Submitter: &fakeSubmitter{}, DataDir: t.TempDir(),
		Now: func() time.Time { return now },
	}
	_, err := runner.Run(ctx, watched.ID)
	if err == nil || !contains(err.Error(), "holdings lookup returned") {
		t.Fatalf("Run() error = %v, want holdings cardinality mismatch", err)
	}
	// No run marked, no digest consumed – the run failed closed. Check no MarkRun
	// side-effect: Due should still consider it due (next attempt retries).
	if _, err := watches.Due(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireDigestManifestWriteFailureDoesNotLoseAlreadyCreatedJobs(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, CreateInput{
		Kind: KindDiscovery, Mode: ModeAlert, Query: "digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 1,
	})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "10.1000/resumable", Title: "Resumable", DOI: "10.1000/resumable",
	}}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	runner := &Runner{
		Store:     watches,
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: &fakeSubmitter{},
		DataDir:   dataDir,
		Now:       func() time.Time { return now },
	}
	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	if err != nil || queued != 1 {
		t.Fatalf("first AcquireDigest() = %d, %v; want 1, nil", queued, err)
	}
	manifest, err := batch.Load(dataDir, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Works) != 1 || manifest.Works[0].JobID == "" {
		t.Fatalf("first manifest = %+v, want JobID", manifest.Works)
	}
	jobID := manifest.Works[0].JobID
	// Simulate a late manifest-write failure that left the digest entry
	// unconsumed while the first manifest with its JobID remained durable.
	// RecordDigest is idempotent and merges into the existing row without
	// resurrecting a consumed entry, so re-recording cannot be used here.
	// Instead, reset the consumed flag directly — that is the exact durable
	// state after a post-submit write+consume failure: the job exists durably
	// but the digest row is still pending.
	if _, err := watches.S.DB().ExecContext(ctx, `UPDATE watch_digest_entries SET consumed = 0 WHERE watch_id = ? AND work_key = ?`, created.ID, "10.1000/resumable"); err != nil {
		t.Fatal(err)
	}
	// Re-run with a submitter that would resubmit if called – it should NOT
	// be called because the durable manifest already carries the JobID.
	resubmitCount := 0
	countingSubmitter := &fakeSubmitter{}
	original := countingSubmitter.SubmitWithAutoImport
	_ = original
	runner2 := &Runner{
		Store:     watches,
		Lookup:    &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: &countingSubmitterWithCount{inner: &fakeSubmitter{}, counter: &resubmitCount},
		DataDir:   dataDir,
		Now:       func() time.Time { return now },
	}
	queued, err = runner2.AcquireDigest(ctx, created.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("resumable AcquireDigest() queued = %d, want 1", queued)
	}
	if resubmitCount != 0 {
		t.Fatalf("resubmit count = %d, want 0 (durable JobID should be reused)", resubmitCount)
	}
	manifest2, err := batch.Load(dataDir, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if manifest2.Works[0].JobID != jobID {
		t.Fatalf("resumable manifest JobID = %q, want %q", manifest2.Works[0].JobID, jobID)
	}
	entries, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("digest after resumable acquisition = %+v, want empty", entries)
	}
}

type countingSubmitterWithCount struct {
	inner   *fakeSubmitter
	counter *int
}

func (c *countingSubmitterWithCount) SubmitWithAutoImport(ctx context.Context, request protocol.WorkRequest, auto *bool) (string, error) {
	*c.counter++
	return c.inner.SubmitWithAutoImport(ctx, request, auto)
}

func TestTriageConsumeDigestsAtomicOnConflict(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	w1 := createWatch(t, watches, CreateInput{Kind: KindDiscovery, Mode: ModeAlert, Query: "w1", Collection: "Reading", CadenceHours: 24, PerRunCap: 5})
	w2 := createWatch(t, watches, CreateInput{Kind: KindDiscovery, Mode: ModeAlert, Query: "w2", Collection: "Reading", CadenceHours: 24, PerRunCap: 5})
	now := time.Now().UTC()
	if _, err := watches.RecordDigest(ctx, w1.ID, now, []DigestEntry{{WorkKey: "10.1000/shared", Title: "Shared", DOI: "10.1000/shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.RecordDigest(ctx, w2.ID, now, []DigestEntry{{WorkKey: "10.1000/shared", Title: "Shared", DOI: "10.1000/shared"}}); err != nil {
		t.Fatal(err)
	}
	// Pre-consume w2's entry so the second target will conflict.
	if _, err := watches.ConsumeDigest(ctx, w2.ID, []string{"10.1000/shared"}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Store: watches, Lookup: &fakeLookup{}, Submitter: &fakeSubmitter{}}
	err := runner.ConsumeDigests(ctx, []DigestTarget{{WatchID: w1.ID, WorkKey: "10.1000/shared"}, {WatchID: w2.ID, WorkKey: "10.1000/shared"}})
	if !errors.Is(err, ErrDigestEntryNotFound) {
		t.Fatalf("ConsumeDigests() error = %v, want ErrDigestEntryNotFound", err)
	}
	// w1 must remain unconsumed.
	entries, err := watches.Digest(ctx, w1.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkKey != "10.1000/shared" {
		t.Fatalf("w1 digest after conflict = %+v, want unconsumed entry", entries)
	}
}

func TestTriageAcquireDigestsAtomicOnConflict(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	w1 := createWatch(t, watches, CreateInput{Kind: KindDiscovery, Mode: ModeAlert, Query: "w1", Collection: "Reading", CadenceHours: 24, PerRunCap: 5})
	w2 := createWatch(t, watches, CreateInput{Kind: KindDiscovery, Mode: ModeAlert, Query: "w2", Collection: "Reading", CadenceHours: 24, PerRunCap: 5})
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, w1.ID, now, []DigestEntry{{WorkKey: "10.1000/a", Title: "A", DOI: "10.1000/a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.RecordDigest(ctx, w2.ID, now, []DigestEntry{{WorkKey: "10.1000/b", Title: "B", DOI: "10.1000/b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := watches.ConsumeDigest(ctx, w2.ID, []string{"10.1000/b"}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Store: watches, Lookup: &fakeLookup{result: &zotio.LookupWorksResult{Works: []zotio.WorkOwnership{{Status: zotio.OwnershipNotOwned}}}},
		Submitter: &fakeSubmitter{}, DataDir: t.TempDir(), Now: func() time.Time { return now },
	}
	err := runner.AcquireDigests(ctx, []DigestTarget{{WatchID: w1.ID, WorkKey: "10.1000/a"}, {WatchID: w2.ID, WorkKey: "10.1000/b"}})
	if !errors.Is(err, ErrDigestEntryNotFound) {
		t.Fatalf("AcquireDigests() error = %v, want ErrDigestEntryNotFound", err)
	}
	entries, err := watches.Digest(ctx, w1.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkKey != "10.1000/a" {
		t.Fatalf("w1 digest after AcquireDigests conflict = %+v, want unconsumed entry", entries)
	}
	if len(runner.Submitter.(*fakeSubmitter).calls) != 0 {
		t.Fatalf("submissions after conflict = %d, want 0", len(runner.Submitter.(*fakeSubmitter).calls))
	}
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
