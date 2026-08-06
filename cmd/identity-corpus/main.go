// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Command identity-corpus measures papio's PDF-identity rules
// (internal/pdf/identity.go) against a real corpus: every document's own
// metadata (which must pass) and every document against every other
// document's metadata (which must never pass). The corpus is the
// operator's own Zotero library, read read-only; a cold-cache run extracts
// text from roughly 789 PDFs and takes several minutes. See
// dev/identity-corpus.md for how to run it and how to read the report.
//
// This is a measurement instrument, not a gate: it always exits 0 once the
// corpus loads, even when wrong accepts are found — reporting the count is
// the entire point. A caller comparing two runs (before/after a rule
// change) needs both runs to exit 0 for the comparison to happen at all.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"papio/internal/identitycorpus"
)

// cacheDefault resolves the default -cache directory. os.UserCacheDir() is
// per-user on every OS (~/Library/Caches on macOS, $XDG_CACHE_HOME or
// ~/.cache on Linux, %LocalAppData% on Windows). os.TempDir() is not: on
// Linux it's the shared, world-writable /tmp, and this cache holds the
// front matter of every paper in the operator's library — a co-tenant on
// that machine has no business reading it. Fall back to the old temp-dir
// path only when the user cache directory can't be resolved at all.
func cacheDefault() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "papio", "identity-corpus")
	}
	return filepath.Join(os.TempDir(), "papio-identity-corpus")
}

// classifySkipReason maps a Skip's free-text Reason (which names a specific
// file or attachment key, and so is never comparable run to run) onto the
// coarse, stable class the report's skip summary tallies. The prefixes and
// exact matches here track the reasons corpus.go's Load actually produces;
// anything that doesn't match one of those known shapes still gets counted,
// under "other", rather than silently dropped.
func classifySkipReason(reason string) string {
	switch {
	case reason == "papio-owned artifact":
		return "papio-owned artifact"
	case strings.HasPrefix(reason, "file missing"), strings.HasPrefix(reason, "linked attachment base path not configured"):
		return "file missing"
	case reason == "output cap":
		return "output cap"
	case strings.HasPrefix(reason, "extraction failed"):
		return "extraction failed"
	case strings.HasPrefix(reason, "parent has another PDF attachment"):
		// Seven of these on the reference library: an item with a supplement or
		// a second scan alongside the article. Its own class rather than
		// "other" because it is a deliberate one-PDF-per-work rule, not an
		// unclassified surprise, and a reader comparing runs needs to see the
		// difference between the two.
		return "duplicate attachment"
	case reason == "no title or identifier":
		return "no title or identifier"
	default:
		return "other"
	}
}

// sortedSkipBuckets turns a class -> count tally into the descending-count,
// then-alphabetical order the report renders in. Go map iteration order is
// randomized, and the whole point of this summary is that two runs over the
// same library produce an identical report.
func sortedSkipBuckets(counts map[string]int) []identitycorpus.OffsetBucket {
	buckets := make([]identitycorpus.OffsetBucket, 0, len(counts))
	for label, count := range counts {
		buckets = append(buckets, identitycorpus.OffsetBucket{Label: label, Count: count})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		return buckets[i].Label < buckets[j].Label
	})
	return buckets
}

func main() {
	homeDir, homeErr := os.UserHomeDir()
	var defaultZotero string
	if homeErr == nil {
		defaultZotero = filepath.Join(homeDir, "Zotero")
	}

	zotero := flag.String("zotero", defaultZotero, "Zotero data directory (containing zotero.sqlite and storage/)")
	cache := flag.String("cache", cacheDefault(), `extracted-text cache directory ("" disables caching)`)
	workers := flag.Int("workers", 0, "extraction concurrency (0 = runtime.NumCPU())")
	jsonOut := flag.Bool("json", false, "emit the report as indented JSON instead of the rendered text report")
	flag.Parse()

	if *zotero == "" {
		fmt.Fprintln(os.Stderr, "identity-corpus: -zotero not set and could not resolve home directory:", homeErr)
		os.Exit(1)
	}

	workerCount := *workers
	if workerCount == 0 {
		workerCount = runtime.NumCPU()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	docs, skips, err := identitycorpus.Load(ctx, *zotero, *cache, workerCount)
	if err != nil {
		fmt.Fprintln(os.Stderr, "identity-corpus:", err)
		os.Exit(1)
	}
	skipCounts := make(map[string]int, len(skips))
	for _, s := range skips {
		fmt.Fprintf(os.Stderr, "skipped %s: %s\n", s.Key, s.Reason)
		skipCounts[classifySkipReason(s.Reason)]++
	}

	report := identitycorpus.Measure(docs)
	report.SkipsByReason = sortedSkipBuckets(skipCounts)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, "identity-corpus:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println(report.Render())
}
