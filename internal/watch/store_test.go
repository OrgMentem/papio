// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"encoding/json"
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

func TestRecordDigestMigratesIdentifierAliases(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	t.Run("arXiv to DOI", func(t *testing.T) {
		created := createWatch(t, watches, testWatchInput("arxiv digest migration"))
		firstSeen := now.Format(time.RFC3339Nano)
		if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
			WorkKey: "arxiv:2601.12345v2", Title: "ArXiv Work",
			Identifiers: &protocol.Identifiers{ArXiv: "2601.12345v2"},
		}}); err != nil || reported != 1 {
			t.Fatalf("arXiv RecordDigest() = %d, %v; want 1, nil", reported, err)
		}
		if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
			WorkKey: "10.1000/arxiv-work", Title: "DOI Work", DOI: "10.1000/arxiv-work",
			Identifiers: &protocol.Identifiers{DOI: "10.1000/arxiv-work", ArXiv: "2601.12345v2"},
		}}); err != nil || reported != 0 {
			t.Fatalf("DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
		}
		digest, err := watches.Digest(ctx, created.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(digest) != 1 || digest[0].WorkKey != "10.1000/arxiv-work" || digest[0].FirstSeenAt != firstSeen {
			t.Fatalf("Digest() = %+v, want one DOI identity with original first seen time", digest)
		}
	})

	t.Run("OpenAlex and title to DOI", func(t *testing.T) {
		created := createWatch(t, watches, testWatchInput("openalex digest migration"))
		if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
			WorkKey: "same work", Title: "Same Work",
		}}); err != nil || reported != 1 {
			t.Fatalf("title RecordDigest() = %d, %v; want 1, nil", reported, err)
		}
		if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
			WorkKey: "same work", Title: "Same Work",
			Identifiers: &protocol.Identifiers{OpenAlex: "W2741809807"},
		}}); err != nil || reported != 0 {
			t.Fatalf("OpenAlex RecordDigest() = %d, %v; want 0, nil", reported, err)
		}
		if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(2*time.Hour), []DigestEntry{{
			WorkKey: "10.1000/same-work", Title: "Same Work (DOI)", DOI: "10.1000/same-work",
			Identifiers: &protocol.Identifiers{DOI: "10.1000/same-work", OpenAlex: "W2741809807"},
		}}); err != nil || reported != 0 {
			t.Fatalf("DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
		}
		digest, err := watches.Digest(ctx, created.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(digest) != 1 || digest[0].WorkKey != "10.1000/same-work" || digest[0].Identifiers == nil ||
			digest[0].Identifiers.OpenAlex != "W2741809807" {
			t.Fatalf("Digest() = %+v, want one DOI identity retaining OpenAlex", digest)
		}
	})

	t.Run("existing DOI wins over alias", func(t *testing.T) {
		created := createWatch(t, watches, testWatchInput("canonical digest migration"))
		if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
			WorkKey: "legacy alias", Title: "Legacy Alias",
		}}); err != nil || reported != 1 {
			t.Fatalf("alias RecordDigest() = %d, %v; want 1, nil", reported, err)
		}
		if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
			WorkKey: "10.1000/canonical", Title: "Canonical", DOI: "10.1000/canonical",
		}}); err != nil || reported != 1 {
			t.Fatalf("canonical RecordDigest() = %d, %v; want 1, nil", reported, err)
		}
		if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(2*time.Hour), []DigestEntry{{
			WorkKey: "10.1000/canonical", TitleKey: "legacy alias", Title: "Enriched Canonical", DOI: "10.1000/canonical",
		}}); err != nil || reported != 0 {
			t.Fatalf("conflicting DOI RecordDigest() = %d, %v; want 0, nil", reported, err)
		}
		digest, err := watches.Digest(ctx, created.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(digest) != 2 || digest[0].WorkKey != "10.1000/canonical" || digest[0].Title != "Enriched Canonical" {
			t.Fatalf("Digest() = %+v, want the existing DOI row to win", digest)
		}
	})
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

func TestRecordDigestSeparatesConflictingStableTitleAliases(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("stable title conflict"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	first := DigestEntry{
		WorkKey: "10.1000/first", Title: "Identical Title", DOI: "10.1000/first",
	}
	if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{first}); err != nil || reported != 1 {
		t.Fatalf("first RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	if err := watches.consumeDigestEntry(ctx, created.ID, first.WorkKey); err != nil {
		t.Fatal(err)
	}
	second := DigestEntry{
		WorkKey: "10.1000/second", Title: "Identical Title", DOI: "10.1000/second",
	}
	if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{second}); err != nil || reported != 1 {
		t.Fatalf("second RecordDigest() = %d, %v; want 1, nil", reported, err)
	}

	var rows int
	if err := watches.S.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM watch_digest_entries WHERE watch_id = ?`, created.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("digest row count = %d, want two distinct DOI rows", rows)
	}
	digest, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 1 || digest[0].WorkKey != second.WorkKey {
		t.Fatalf("pending Digest() = %+v, want only the second DOI row", digest)
	}
}

func TestRecordDigestRetainsCanonicalDOIAndAuthorsOnSparseAliasUpdate(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	created := createWatch(t, watches, testWatchInput("canonical digest key"))
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	const doi = "10.1000/canonical"
	const arXiv = "2601.12345v2"

	if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
		WorkKey: "arxiv:" + arXiv, Title: "Canonical Work", DOI: doi, Authors: "Ada, Bob",
		AuthorNames: []string{"Ada", "Bob"},
		Identifiers: &protocol.Identifiers{DOI: doi, ArXiv: arXiv},
	}}); err != nil || reported != 1 {
		t.Fatalf("DOI RecordDigest() = %d, %v; want 1, nil", reported, err)
	}
	if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
		WorkKey: "arxiv:" + arXiv, Title: "Canonical Work",
		Identifiers: &protocol.Identifiers{ArXiv: arXiv},
	}}); err != nil || reported != 0 {
		t.Fatalf("arXiv-only RecordDigest() = %d, %v; want 0, nil", reported, err)
	}
	digest, err := watches.Digest(ctx, created.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 1 || digest[0].WorkKey != doi || digest[0].Authors != "Ada, Bob" ||
		len(digest[0].AuthorNames) != 2 || digest[0].AuthorNames[0] != "Ada" || digest[0].AuthorNames[1] != "Bob" {
		t.Fatalf("Digest() = %+v, want DOI key and preserved authors", digest)
	}
	selected, err := watches.TakeDigest(ctx, created.ID, []string{"arxiv:" + arXiv})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].WorkKey != doi {
		t.Fatalf("TakeDigest() = %+v, want the DOI-canonical entry selected by its arXiv alias", selected)
	}
}

func TestTakeDigestResolvesOnlyUniqueTitleAliases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	t.Run("unique title alias", func(t *testing.T) {
		watches := testStore(t)
		created := createWatch(t, watches, testWatchInput("unique title alias"))
		if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{{
			WorkKey: "retained title", Title: "Retained Title",
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{{
			WorkKey: "10.1000/retained", TitleKey: "retained title", Title: "Enriched Title", DOI: "10.1000/retained",
		}}); err != nil {
			t.Fatal(err)
		}
		entries, err := watches.TakeDigest(ctx, created.ID, []string{"retained title"})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].WorkKey != "10.1000/retained" {
			t.Fatalf("TakeDigest() = %+v, want the DOI-canonical title alias", entries)
		}
	})

	t.Run("ambiguous title alias", func(t *testing.T) {
		watches := testStore(t)
		created := createWatch(t, watches, testWatchInput("ambiguous title alias"))
		if _, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{
			{WorkKey: "10.1000/first", Title: "Shared Title", DOI: "10.1000/first"},
			{WorkKey: "10.1000/second", Title: "Shared Title", DOI: "10.1000/second"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := watches.TakeDigest(ctx, created.ID, []string{"shared title"}); !errors.Is(err, ErrDigestEntryNotFound) {
			t.Fatalf("TakeDigest() error = %v, want ambiguous title alias to be rejected", err)
		}
	})
}

func TestRecordDigestBridgesStableAliasesInEitherOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	const doi = "10.1000/bridge"
	const arXiv = "2601.12345v2"

	for _, test := range []struct {
		name  string
		first DigestEntry
		next  DigestEntry
	}{
		{
			name:  "arXiv then DOI",
			first: DigestEntry{WorkKey: "arxiv:" + arXiv, Title: "ArXiv Record", Authors: "Ada", Identifiers: &protocol.Identifiers{ArXiv: arXiv}},
			next:  DigestEntry{WorkKey: doi, Title: "DOI Record", Authors: "Bob", DOI: doi},
		},
		{
			name:  "DOI then arXiv",
			first: DigestEntry{WorkKey: doi, Title: "DOI Record", Authors: "Ada", DOI: doi},
			next:  DigestEntry{WorkKey: "arxiv:" + arXiv, Title: "ArXiv Record", Authors: "Bob", Identifiers: &protocol.Identifiers{ArXiv: arXiv}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			watches := testStore(t)
			created := createWatch(t, watches, testWatchInput("stable bridge "+test.name))
			if reported, err := watches.RecordDigest(ctx, created.ID, now, []DigestEntry{test.first}); err != nil || reported != 1 {
				t.Fatalf("first RecordDigest() = %d, %v; want 1, nil", reported, err)
			}
			if err := watches.consumeDigestEntry(ctx, created.ID, test.first.WorkKey); err != nil {
				t.Fatal(err)
			}
			if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{test.next}); err != nil || reported != 1 {
				t.Fatalf("second RecordDigest() = %d, %v; want 1, nil", reported, err)
			}
			if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(2*time.Hour), []DigestEntry{{
				WorkKey: doi, Title: "Bridged Record", DOI: doi,
				Identifiers: &protocol.Identifiers{DOI: doi, ArXiv: arXiv},
			}}); err != nil || reported != 0 {
				t.Fatalf("bridge RecordDigest() = %d, %v; want 0, nil", reported, err)
			}

			var count int
			if err := watches.S.DB().QueryRowContext(ctx, `
				SELECT COUNT(*) FROM watch_digest_entries WHERE watch_id = ?`, created.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			var workKey, firstSeenAt, authors, identifiersJSON string
			var consumed bool
			if err := watches.S.DB().QueryRowContext(ctx, `
				SELECT work_key, first_seen_at, authors, identifiers_json, consumed
				FROM watch_digest_entries
				WHERE watch_id = ?`, created.ID).Scan(&workKey, &firstSeenAt, &authors, &identifiersJSON, &consumed); err != nil {
				t.Fatal(err)
			}
			var identifiers protocol.Identifiers
			if err := json.Unmarshal([]byte(identifiersJSON), &identifiers); err != nil {
				t.Fatal(err)
			}
			if count != 1 || workKey != doi || firstSeenAt != now.Format(time.RFC3339Nano) || !consumed ||
				authors != "Ada" || identifiers.DOI != doi || identifiers.ArXiv != arXiv {
				t.Fatalf("bridged row = key %q, first seen %q, authors %q, ids %+v, consumed %t, count %d",
					workKey, firstSeenAt, authors, identifiers, consumed, count)
			}
		})
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
	if reported, err := watches.RecordDigest(ctx, created.ID, now.Add(time.Hour), []DigestEntry{
		{WorkKey: "10.1000/one", Title: "One"},
		{WorkKey: "10.1000/two", Title: "Two"},
	}); err != nil || reported != 0 {
		t.Fatalf("repeat RecordDigest() after clear = %d, %v; want 0, nil", reported, err)
	}
	if digest, err := watches.Digest(ctx, created.ID, 100); err != nil || len(digest) != 0 {
		t.Fatalf("Digest() after clear and repeat = %+v, %v; want no pending entries", digest, err)
	}
	if pending, err := watches.TakeDigest(ctx, created.ID, nil); err != nil || len(pending) != 0 {
		t.Fatalf("TakeDigest() after clear and repeat = %+v, %v; want no pending entries", pending, err)
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
