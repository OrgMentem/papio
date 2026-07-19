// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"testing"
	"time"

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
