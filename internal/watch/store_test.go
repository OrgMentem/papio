// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"testing"
	"time"
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
