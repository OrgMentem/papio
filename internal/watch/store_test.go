// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/protocol"
)

func TestRecordDigestMigratesTitleKeyToDOI(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest migration"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "the same work", Title: "The Same Work", Authors: "Ada", Year: 2025,
	}})
	if err != nil || reported != 1 {
		t.Fatalf("title-only RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	reported, err = watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
		WorkKey: "10.1000/the-same-work", TitleKey: "the same work", Title: "The Same Work",
		Authors: "Ada", Year: 2025, DOI: "10.1000/the-same-work",
	}})
	if err != nil || reported != 0 {
		t.Fatalf("DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
	}
	digest, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 1 || digest[0].WorkKey != "10.1000/the-same-work" || digest[0].DOI != "10.1000/the-same-work" {
		t.Fatalf("Digest() = %+v, want one DOI-keyed entry", digest)
	}
}

func TestRecordDigestPreservesIdentifiers(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("digest identifiers"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	want := &protocol.Identifiers{DOI: "10.1000/identifiers", OpenAlex: "W2741809807"}
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "10.1000/identifiers", Title: "Identifiers", DOI: want.DOI, Identifiers: want,
	}}); err != nil {
		t.Fatal(err)
	}
	digest, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 1 || digest[0].Identifiers == nil || *digest[0].Identifiers != *want {
		t.Fatalf("Digest() = %+v, want persisted identifiers %+v", digest, want)
	}
}

func TestClearDigest(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("clear digest"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{
		{WorkKey: "10.1000/one", Title: "One"},
		{WorkKey: "10.1000/two", Title: "Two"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		watchID int64
		want    int
		wantErr bool
	}{
		{name: "removes entries", watchID: created.ID, want: 2},
		{name: "requires existing watch", watchID: created.ID + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := watches.ClearDigest(ctx, test.watchID)
			if (err != nil) != test.wantErr {
				t.Fatalf("ClearDigest() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("ClearDigest() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestTakeDigest(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("take digest"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{
		{WorkKey: "10.1000/one", Title: "One"},
		{WorkKey: "10.1000/two", Title: "Two"},
		{WorkKey: "arxiv:2601.12345v2", Title: "Three"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name         string
		keys         []string
		want         []string
		wantErr      bool
		wantNotFound bool
	}{
		{name: "all entries", want: []string{"arxiv:2601.12345v2", "10.1000/two", "10.1000/one"}},
		{name: "selected entries", keys: []string{"10.1000/one", "arxiv:2601.12345v2"}, want: []string{"arxiv:2601.12345v2", "10.1000/one"}},
		{name: "unknown key", keys: []string{"10.1000/one", "missing"}, wantErr: true, wantNotFound: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries, err := watches.TakeDigest(ctx, created.ID, test.keys)
			if (err != nil) != test.wantErr {
				t.Fatalf("TakeDigest() error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantErr {
				if test.wantNotFound && !errors.Is(err, ErrDigestEntryNotFound) {
					t.Fatalf("TakeDigest() error = %v, want ErrDigestEntryNotFound", err)
				}
				return
			}
			if len(entries) != len(test.want) {
				t.Fatalf("TakeDigest() = %+v, want keys %v", entries, test.want)
			}
			for i, entry := range entries {
				if entry.WorkKey != test.want[i] {
					t.Fatalf("TakeDigest()[%d].WorkKey = %q, want %q", i, entry.WorkKey, test.want[i])
				}
			}
		})
	}
}
