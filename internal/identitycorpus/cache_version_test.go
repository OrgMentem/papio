package identitycorpus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestExtractOneDoesNotCacheInterruptedMetadata(t *testing.T) {
	for _, mode := range []string{"cancelled", "timed out"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "metadata-started")
			p, capability := cacheExtractionFixture(t, fmt.Sprintf(
				"if [ ! -f %q ]; then\n  : > %q\n  exec sleep 30\nfi\n%s",
				marker, marker, cacheMetadataScript))
			cacheDir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			opts := pdf.SemanticOptions{MinChars: 1, Timeout: time.Second}
			if mode == "cancelled" {
				opts.Timeout = 5 * time.Second
				stopped := make(chan struct{})
				go func() {
					defer close(stopped)
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						if _, err := os.Stat(marker); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
				defer func() {
					cancel()
					<-stopped
				}()
			}

			_, skip, ok := extractOne(ctx, p, capability, opts, cacheDir)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("metadata stage never started: %v", err)
			}
			if ok || !strings.Contains(skip.Reason, "metadata extraction failed") {
				t.Errorf("ok=%v skip=%+v, want an interrupted metadata skip", ok, skip)
			}
			entries, err := os.ReadDir(cacheDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("interrupted extraction wrote %d cache entries", len(entries))
			}

			opts.Timeout = 5 * time.Second
			doc, skip, ok := extractOne(context.Background(), p, capability, opts, cacheDir)
			if !ok || len(doc.Metadata) != 1 || doc.Metadata[0].Value != "10.5555/cache" {
				t.Fatalf("retry: ok=%v skip=%+v metadata=%+v, want fresh metadata", ok, skip, doc.Metadata)
			}
			cached, skip, ok := extractOne(context.Background(), p, pdf.Capability{}, opts, cacheDir)
			if !ok || !reflect.DeepEqual(cached.Metadata, doc.Metadata) {
				t.Fatalf("cache hit: ok=%v skip=%+v metadata=%+v", ok, skip, cached.Metadata)
			}
		})
	}
}

func TestExtractOneIgnoresPotentiallyInterruptedLegacyCache(t *testing.T) {
	p, capability := cacheExtractionFixture(t, cacheMetadataScript)
	cacheDir := t.TempDir()
	legacy := filepath.Join(cacheDir, fmt.Sprintf("%s-%d-%d-v3.json", p.cand.attachmentKey, p.info.Size(), p.info.ModTime().Unix()))
	if err := os.WriteFile(legacy, []byte(`{"text":"old extraction with missing metadata","chars":36}`), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, skip, ok := extractOne(context.Background(), p, capability, pdf.SemanticOptions{MinChars: 1}, cacheDir)
	if !ok || len(doc.Metadata) != 1 || doc.Metadata[0].Value != "10.5555/cache" {
		t.Fatalf("ok=%v skip=%+v metadata=%+v, want fresh metadata instead of the legacy cache", ok, skip, doc.Metadata)
	}
}

const cacheMetadataScript = `printf '%s\n' '<prism:doi xmlns:prism="http://prismstandard.org/namespaces/basic/2.1/">10.5555/cache</prism:doi>'`

func cacheExtractionFixture(t *testing.T, metadataBody string) (prepared, pdf.Capability) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are POSIX shell scripts")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tool := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return prepared{cand: candidate{attachmentKey: "CACHE001"}, path: path, info: info}, pdf.Capability{
		PDFToText: tool("pdftotext", "printf 'A complete extracted document with usable text.\\n'"),
		PDFInfo:   tool("pdfinfo", metadataBody),
	}
}
