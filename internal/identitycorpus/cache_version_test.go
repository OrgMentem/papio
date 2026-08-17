package identitycorpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"papio/internal/pdf"
)

// TestCacheEntryRoundTripsExtractionFlags pins the flags, and now the
// embedded metadata, into the entry.
//
// A cache hit used to rebuild only the text and character count, so OCRUsed and
// NeedsReview came back false for every cached document. A warm run therefore
// reported a library with no OCR in it, and any rule conditioned on OCR — the
// structural work's refusal to trust page boundaries in OCR text is the one
// that matters — read that as "this document has a real text layer". Metadata
// is pinned for the identical reason: a cache hit that dropped it would go on
// reporting a document with embedded metadata as carrying none, forever,
// until its cache entry happened to expire some other way.
func TestCacheEntryRoundTripsExtractionFlags(t *testing.T) {
	want := cacheEntry{
		Text:        "page one\fpage two",
		Chars:       17,
		OCRUsed:     true,
		NeedsReview: true,
		Metadata:    pdf.MetadataFields{{Field: "xmp/prism:doi", Value: "10.5555/test.2022.501"}},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got cacheEntry
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry round-tripped as %+v, want %+v", got, want)
	}
}

// TestCacheKeyIsVersioned is the regression for the reason the OCR page-separator
// defect could have outlived its own fix.
//
// Entries were named "<key>-<size>-<mtime>.txt" — everything about the input
// file and nothing about the extractor. The separator changed what ExtractText
// produces, and a warm cache would have gone on serving pre-fix text while the
// code under test was fixed, with nothing recording the difference. That is the
// same shape as editing an applied migration in place.
func TestCacheKeyIsVersioned(t *testing.T) {
	const (
		key   = "ABCD1234"
		size  = int64(4096)
		mtime = int64(1750000000)
	)
	name := fmt.Sprintf("%s-%d-%d-v%d.json", key, size, mtime, cacheFormatVersion)

	if cacheFormatVersion < 2 {
		t.Fatalf("cacheFormatVersion is %d; version 1 is the unversioned .txt era and must not be reused", cacheFormatVersion)
	}

	// A version-1 entry for the same PDF must not be reachable under the
	// current name, or the bump bought nothing.
	legacy := fmt.Sprintf("%s-%d-%d.txt", key, size, mtime)
	if name == legacy {
		t.Fatalf("versioned name %q collides with the legacy name", name)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacy), []byte("unseparated pre-fix text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("a legacy entry must not satisfy a versioned lookup; got err %v", err)
	}
}
