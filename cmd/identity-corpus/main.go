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
	"syscall"

	"papio/internal/identitycorpus"
)

func main() {
	homeDir, homeErr := os.UserHomeDir()
	var defaultZotero string
	if homeErr == nil {
		defaultZotero = filepath.Join(homeDir, "Zotero")
	}

	zotero := flag.String("zotero", defaultZotero, "Zotero data directory (containing zotero.sqlite and storage/)")
	cache := flag.String("cache", filepath.Join(os.TempDir(), "papio-identity-corpus"), `extracted-text cache directory ("" disables caching)`)
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
	for _, s := range skips {
		fmt.Fprintf(os.Stderr, "skipped %s: %s\n", s.Key, s.Reason)
	}

	report := identitycorpus.Measure(docs)
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
