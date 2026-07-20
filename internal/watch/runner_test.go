// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/work"
	"papio/internal/zotio"
)

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
	runner := &Runner{Store: watches, Submitter: submitter, DataDir: t.TempDir(), Now: func() time.Time { return now }}

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
		Store:     watches,
		Submitter: &fakeSubmitter{failOnCall: map[int]error{2: context.DeadlineExceeded}},
		DataDir:   t.TempDir(),
		Now:       func() time.Time { return now },
	}

	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	if err != context.DeadlineExceeded || queued != 1 {
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
		Submitter: &fakeSubmitter{},
		DataDir:   dataDir,
		Now:       func() time.Time { return now },
	}
	queued, err := runner.AcquireDigest(ctx, created.ID, nil)
	if err == nil || queued != 1 {
		t.Fatalf("AcquireDigest() = %d, %v; want 1 and manifest write failure", queued, err)
	}
	entries, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkKey != "10.1000/manifest" {
		t.Fatalf("digest after manifest write failure = %+v, want unremoved entry", entries)
	}
}
