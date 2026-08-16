// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Command identity-corpus measures papio's PDF-identity rules against a real
// corpus: the operator's own Zotero library, read read-only. A cold-cache run
// extracts text from roughly 789 PDFs and takes several minutes. See
// dev/identity-corpus.md for how to run it and how to read the report.
//
// Two modes, one corpus. By default it measures internal/pdf/identity.go
// pairwise: every document against its own metadata (which must pass) and
// against every other document's metadata (which must never pass). Under
// -candidates it measures internal/pdf/candidate_select.go's selection
// instead — pdf.SelectAutoBindCandidate choosing at most one candidate from a
// pool of N — over the DOI-less subset of the library, because production
// reaches that selector only from the branch where the front-matter window
// yielded no DOI.
//
// This is a measurement instrument, not a gate: it always exits 0 once the
// corpus loads, even when wrong accepts are found — reporting the count is
// the entire point. A caller comparing two runs (before/after a rule
// change) needs both runs to exit 0 for the comparison to happen at all.
// An arm that cannot be built (no papio store to enumerate, no reviewed
// composite labels) is reported as an unfilled cell for the same reason.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"papio/internal/config"
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

// defaultSeed is the -seed default, and it is deliberately a constant rather
// than anything derived from the clock. Every candidate pool is drawn from
// this seed and the report records it, so two runs of the same seed over the
// same library draw the same pools — which is the only thing that makes a
// before/after comparison of a rule change a comparison at all. A
// time-seeded default would resample the library on every run and turn every
// diff into noise.
const defaultSeed = 20260816

// defaultPoolSizes is the -pool-sizes sweep. It starts at 2 because a pool of
// one cannot measure a 1-of-N selection: with a single candidate there is
// nothing to select between, so the only thing a "correct" bind would
// demonstrate is that the qualification gates passed — which is what the
// pairwise mode already measures, pair by pair.
const defaultPoolSizes = "2,5,10,25"

// candidateFlags names the flags that mean nothing outside -candidates.
// Passing one without -candidates is refused rather than ignored: the run
// would quietly produce the pairwise report, and a captured file would then
// look like it had measured pools at a seed it never used.
var candidateFlags = []string{"seed", "pool-sizes", "arms", "composite-labels", "true-classes", "papio-data-dir"}

// parsePoolSizes parses the -pool-sizes list. A size below 2 is refused by
// name instead of being dropped: a caller who asked for it has misunderstood
// what is being measured, and silently sweeping a different set than the one
// requested is exactly the kind of quiet substitution that makes a
// measurement unreadable later.
func parsePoolSizes(spec string) ([]int, error) {
	fields := strings.Split(spec, ",")
	sizes := make([]int, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%q is not a pool size", field)
		}
		switch {
		case n == 1:
			return nil, errors.New("pool size 1 cannot measure a 1-of-N selection: with one candidate there is nothing to select between, so the smallest measurable pool is 2")
		case n < 1:
			return nil, fmt.Errorf("pool size %d is not a size: a pool holds at least 2 candidates", n)
		}
		sizes = append(sizes, n)
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("no pool sizes given (the default sweep is %q)", defaultPoolSizes)
	}
	return sizes, nil
}

// parseArms resolves the -arms list to the arm set this run measures. An
// empty spec resolves to AllArms() here rather than being left nil for
// MeasureCandidateSets to default, because two of the CLI's own decisions
// depend on the resolved set: whether to read the papio store for the backlog
// arm, and whether the corpus must be loaded in all-attachments mode for the
// composite one.
func parseArms(spec string) ([]identitycorpus.Arm, error) {
	if strings.TrimSpace(spec) == "" {
		return identitycorpus.AllArms(), nil
	}
	var arms []identitycorpus.Arm
	seen := make(map[identitycorpus.Arm]bool)
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		arm, err := identitycorpus.ParseArm(field)
		if err != nil {
			return nil, err
		}
		if seen[arm] {
			continue
		}
		seen[arm] = true
		arms = append(arms, arm)
	}
	if len(arms) == 0 {
		return nil, errors.New(`no arms given (an empty -arms measures every arm)`)
	}
	return arms, nil
}

func hasArm(arms []identitycorpus.Arm, want identitycorpus.Arm) bool {
	for _, arm := range arms {
		if arm == want {
			return true
		}
	}
	return false
}

// resolveCompositeReview turns the -composite-labels path into the review the
// composite arm is built from, in the one direction that cannot lose a human
// label. That file is ground truth: signals only propose, a person confirms,
// and an unreviewed proposal counts as neither class. So a re-run re-proposes
// over the current library, merges the fresh proposals underneath the labels
// already recorded, and writes back only when the merged row set differs from
// the one on disk — rewriting an unchanged file would churn a file the
// operator is editing by hand, and rewriting it without merging would delete
// the only copy of the labels. SameRows compares key sets rather than counts
// because the case that matters is a swap: one document leaving the library
// while another starts being proposed keeps the count identical.
func resolveCompositeReview(path string, docs []identitycorpus.Document, opts identitycorpus.CompositeOptions) (identitycorpus.CompositeReview, error) {
	fresh := identitycorpus.ProposeComposites(docs, opts)
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return identitycorpus.CompositeReview{}, err
		}
		if err := identitycorpus.WriteCompositeReview(path, fresh); err != nil {
			return identitycorpus.CompositeReview{}, err
		}
		fmt.Fprintf(os.Stderr, "wrote %d composite proposals and %d audit rows for review; every row is unreviewed, so the composite arm measures nothing yet and prevalence has no upper bound until the audit rows are labelled\n",
			len(fresh.Proposals), len(fresh.AuditSample))
		return fresh, nil
	}
	prior, err := identitycorpus.LoadCompositeReview(path)
	if err != nil {
		return identitycorpus.CompositeReview{}, err
	}
	merged := identitycorpus.MergeCompositeReview(fresh, prior)
	if !merged.SameRows(prior) {
		if err := identitycorpus.WriteCompositeReview(path, merged); err != nil {
			return identitycorpus.CompositeReview{}, err
		}
		fmt.Fprintf(os.Stderr, "composite labels rewritten: %d proposals and %d audit rows, was %d and %d; any added row is unreviewed\n",
			len(merged.Proposals), len(merged.AuditSample), len(prior.Proposals), len(prior.AuditSample))
	}
	return merged, nil
}

// buildBacklogArm reads the papio store's own candidate-eligible jobs, so one
// arm stresses pools shaped like the ones the daemon would assemble. It
// reports false rather than failing the run when there is no store to read:
// this tool exits 0 once the corpus loads, and an absent papio store is a
// fact about the machine, not a defect in the rules under test. The arm then
// reads as an unfilled cell, which is what it is.
//
// What this arm is not: calibration. Grab persists no candidate snapshot, so
// the pool that existed when a historical grab settled is unrecoverable and a
// present-day eligibility read is descriptive stress coverage only. Its
// Render carries that caveat, which is why it is printed beside the report
// rather than folded into it.
func buildBacklogArm(ctx context.Context, dataDir string, docs []identitycorpus.Document) (identitycorpus.BacklogArm, bool) {
	elig, err := identitycorpus.OpenBacklogEligibility(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backlog arm skipped:", err)
		return identitycorpus.BacklogArm{}, false
	}
	defer elig.Close()
	arm, err := identitycorpus.BuildBacklogArm(ctx, elig, docs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backlog arm skipped:", err)
		return identitycorpus.BacklogArm{}, false
	}
	return arm, true
}

// candidateRun is the resolved -candidates configuration. Flag parsing and
// validation happen once, in main, so this carries values rather than the
// flag pointers.
type candidateRun struct {
	Seed            int64
	PoolSizes       []int
	Arms            []identitycorpus.Arm
	TrueClasses     map[string][]string
	CompositeLabels string
	PapioDataDir    string
	JSON            bool
}

// candidateOutput is the -json shape for the candidate mode: an object
// wrapping the report, where the pairwise mode emits the bare report. Two
// sections a reader must not lose live outside CandidateReport — the
// composite summary, whose prevalence is a LOWER bound until the audit rows
// are reviewed, and the backlog arm, whose caveat says its row is descriptive
// coverage rather than a production rate. Emitting the bare report would drop
// both from a captured JSON run and leave the backlog row quotable as if it
// were calibration.
type candidateOutput struct {
	Report    identitycorpus.CandidateReport   `json:"report"`
	Composite *identitycorpus.CompositeSummary `json:"composite,omitempty"`
	Backlog   *identitycorpus.BacklogArm       `json:"backlog,omitempty"`
}

// runCandidates measures pdf.SelectAutoBindCandidate over pools drawn from
// docs. Unlike the pairwise mode it scores a selection rather than a
// predicate: one choice out of N, where choosing nothing is the right answer
// whenever the target is absent. MeasureCandidateSets applies the DOI-less
// admission itself, so docs is handed over whole — pre-filtering here would
// make the report's LibraryDocuments count a filtered library and read the
// DOI-less share, which is a headline finding, as 100%.
func runCandidates(ctx context.Context, docs []identitycorpus.Document, run candidateRun) {
	opts := identitycorpus.CandidateOptions{
		Seed:        run.Seed,
		PoolSizes:   run.PoolSizes,
		Arms:        run.Arms,
		TrueClasses: run.TrueClasses,
		ExtraPools:  make(map[identitycorpus.Arm][]identitycorpus.Pool, 2),
	}

	var composite *identitycorpus.CompositeSummary
	if run.CompositeLabels != "" && hasArm(run.Arms, identitycorpus.ArmComposite) {
		// AuditSample is left zero so the package's own default applies: the
		// sample size belongs to the recall bound it supports, not to this tool.
		compositeOpts := identitycorpus.CompositeOptions{Seed: run.Seed, PoolSizes: run.PoolSizes}
		review, err := resolveCompositeReview(run.CompositeLabels, docs, compositeOpts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "composite arm skipped:", err)
		} else {
			pools, summary := identitycorpus.CompositePools(docs, review, compositeOpts)
			opts.ExtraPools[identitycorpus.ArmComposite] = pools
			composite = &summary
		}
	}

	var backlog *identitycorpus.BacklogArm
	if hasArm(run.Arms, identitycorpus.ArmBacklog) {
		if arm, ok := buildBacklogArm(ctx, run.PapioDataDir, docs); ok {
			opts.ExtraPools[identitycorpus.ArmBacklog] = arm.Pools
			backlog = &arm
		}
	}

	report := identitycorpus.MeasureCandidateSets(docs, opts)
	if run.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(candidateOutput{Report: report, Composite: composite, Backlog: backlog}); err != nil {
			fmt.Fprintln(os.Stderr, "identity-corpus:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println(report.Render())
	if composite != nil {
		fmt.Println(composite.Render())
	}
	if backlog != nil {
		fmt.Println(backlog.Render())
	}
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
	candidates := flag.Bool("candidates", false, "measure candidate-set binding (pdf.SelectAutoBindCandidate over pools, DOI-less subset only) instead of the pairwise identity report")
	seed := flag.Int64("seed", defaultSeed, "fixed seed every candidate pool is drawn from; the report records it, so two runs at one seed are comparable (-candidates only)")
	poolSizes := flag.String("pool-sizes", defaultPoolSizes, "comma-separated pool sizes to sweep, each at least 2 (-candidates only)")
	arms := flag.String("arms", "", `comma-separated pool-construction arms; empty measures every arm (-candidates only)`)
	compositeLabels := flag.String("composite-labels", "", "path to the human-reviewed composite label file, written with fresh proposals if absent; enables the composite arm (-candidates only)")
	trueClasses := flag.String("true-classes", "", "path to a JSON map of document key -> adjudicated equivalence class, for pairs (preprint/version of record) that must be enumerated rather than inferred (-candidates only)")
	papioDataDir := flag.String("papio-data-dir", config.Default().DataDir, "papio data directory whose store is read read-only to enumerate the backlog arm (-candidates only)")
	flag.Parse()

	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if !*candidates {
		for _, name := range candidateFlags {
			if explicit[name] {
				fmt.Fprintf(os.Stderr, "identity-corpus: -%s applies only to -candidates; without it this tool runs the pairwise identity measurement\n", name)
				os.Exit(1)
			}
		}
	}

	if *zotero == "" {
		fmt.Fprintln(os.Stderr, "identity-corpus: -zotero not set and could not resolve home directory:", homeErr)
		os.Exit(1)
	}

	workerCount := *workers
	if workerCount == 0 {
		workerCount = runtime.NumCPU()
	}

	// Everything the candidate mode needs from the flags is resolved before the
	// corpus loads: extraction is the expensive part of a cold run, and a typo
	// in -pool-sizes should not cost several minutes to discover.
	var (
		poolSweep []int
		armSet    []identitycorpus.Arm
		trueClass map[string][]string
	)
	if *candidates {
		var err error
		if poolSweep, err = parsePoolSizes(*poolSizes); err != nil {
			fmt.Fprintln(os.Stderr, "identity-corpus: -pool-sizes:", err)
			os.Exit(1)
		}
		if armSet, err = parseArms(*arms); err != nil {
			fmt.Fprintln(os.Stderr, "identity-corpus: -arms:", err)
			os.Exit(1)
		}
		if *trueClasses != "" {
			// A missing or malformed file is fatal rather than skipped: the
			// alternative is a run that silently falls back to identifier-derived
			// truth for the pairs the operator adjudicated by hand.
			if trueClass, err = identitycorpus.LoadTrueClasses(*trueClasses); err != nil {
				fmt.Fprintln(os.Stderr, "identity-corpus: -true-classes:", err)
				os.Exit(1)
			}
		}
		// An arm's own flag with that arm deselected is refused for the same
		// reason -candidates refuses the flags above: the run would go ahead
		// measuring something the flags say was measured, and the capture would
		// read as evidence about an arm that never ran.
		if *compositeLabels != "" && !hasArm(armSet, identitycorpus.ArmComposite) {
			fmt.Fprintln(os.Stderr, "identity-corpus: -composite-labels names a label file but -arms does not include the composite arm")
			os.Exit(1)
		}
		if explicit["papio-data-dir"] && !hasArm(armSet, identitycorpus.ArmBacklog) {
			fmt.Fprintln(os.Stderr, "identity-corpus: -papio-data-dir is only read for the backlog arm, which -arms does not include")
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The composite class is the one dedupOnePerParent hides: it keeps one PDF
	// per bibliographic parent and drops the supplements and second scans that
	// compose the failure the withdrawn rule died of. So every attachment is
	// loaded exactly when that arm is being built, and never by default —
	// all-attachments loading changes what the corpus counts, and two runs over
	// different corpora are not a comparison.
	var (
		docs  []identitycorpus.Document
		skips []identitycorpus.Skip
		err   error
	)
	if *candidates && *compositeLabels != "" && hasArm(armSet, identitycorpus.ArmComposite) {
		docs, skips, err = identitycorpus.LoadWithOptions(ctx, identitycorpus.LoadOptions{
			ZoteroDir:      *zotero,
			CacheDir:       *cache,
			Workers:        workerCount,
			AllAttachments: true,
		})
	} else {
		docs, skips, err = identitycorpus.Load(ctx, *zotero, *cache, workerCount)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "identity-corpus:", err)
		os.Exit(1)
	}
	skipCounts := make(map[string]int, len(skips))
	for _, s := range skips {
		fmt.Fprintf(os.Stderr, "skipped %s: %s\n", s.Key, s.Reason)
		skipCounts[classifySkipReason(s.Reason)]++
	}
	buckets := sortedSkipBuckets(skipCounts)

	if *candidates {
		// CandidateReport has no SkipsByReason field, and dev/identity-corpus.md
		// tells a reader to check the run's own skip summary to know what the
		// corpus left out. Rather than drop the aggregate, print it to stderr
		// beside the per-skip lines it summarizes — same library data, same
		// handling as the rest of that stream.
		for _, b := range buckets {
			fmt.Fprintf(os.Stderr, "skip summary: %s=%d\n", b.Label, b.Count)
		}
		runCandidates(ctx, docs, candidateRun{
			Seed:            *seed,
			PoolSizes:       poolSweep,
			Arms:            armSet,
			TrueClasses:     trueClass,
			CompositeLabels: *compositeLabels,
			PapioDataDir:    *papioDataDir,
			JSON:            *jsonOut,
		})
		return
	}

	report := identitycorpus.Measure(docs)
	report.SkipsByReason = buckets
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
