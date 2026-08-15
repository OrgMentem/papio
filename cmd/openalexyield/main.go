// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Command openalexyield measures the yield of OpenAlex's title.search query
// shape: the free half reads the local papio store (read-only, no provider
// requests) and reports a lower-bound estimate of accepted artifacts
// attributable to a title search per title.search credit spent; the paid
// half, strictly opt-in, spends real credits comparing the three query
// shapes named in dev/active/openalex-spend-remainders.md item 0 against a
// sample drawn from the local library. See dev/openalex-yield.md.
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
	"syscall"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/openalexyield"
)

func defaultStorePath() string {
	return filepath.Join(config.Default().DataDir, "papio.db")
}

func main() {
	store := flag.String("store", defaultStorePath(), "path to the papio.db store to read (read-only)")
	since := flag.Duration("since", 0, "restrict the free half to the last N (e.g. -since 720h); 0 = all history")
	jsonOut := flag.Bool("json", false, "emit the free-half report as indented JSON instead of the rendered text report")

	compare := flag.Bool("compare", false, "also run the paid three-shape comparison (printed cost preview always shown first)")
	confirmSpend := flag.Bool("confirm-spend", false, "REQUIRED alongside -compare to actually spend credits; without it only the cost preview is printed")
	sample := flag.Int("sample", 25, "paid comparison: number of local-library titles to test per shape (bounded, see openalexyield.MaxSample)")
	email := flag.String("email", "", "paid comparison: contact email for OpenAlex's polite pool (defaults to the configured papio email)")
	apiKey := flag.String("api-key", "", "paid comparison: OpenAlex API key (defaults to the configured openalex source's key; empty samples the keyless tier)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := runFree(ctx, *store, *since, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "openalexyield:", err)
		os.Exit(1)
	}

	if !*compare {
		return
	}
	if err := runPaid(ctx, *store, *sample, *confirmSpend, *email, *apiKey); err != nil {
		fmt.Fprintln(os.Stderr, "openalexyield:", err)
		os.Exit(1)
	}
}

func runFree(ctx context.Context, storePath string, since time.Duration, jsonOut bool) error {
	db, err := openalexyield.OpenReadOnly(storePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	var window time.Time
	if since > 0 {
		window = time.Now().Add(-since)
	}
	report, err := openalexyield.Measure(ctx, db, window)
	if err != nil {
		return fmt.Errorf("measuring free-half yield: %w", err)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Println(report.Render())
	return nil
}

func runPaid(ctx context.Context, storePath string, sample int, confirm bool, email, apiKey string) error {
	plan := openalexyield.Plan(sample)
	fmt.Println(plan.Render())
	if !confirm {
		return nil
	}

	cfg, cfgErr := config.Load("")
	if email == "" {
		email = cfg.Email
	}
	if apiKey == "" && cfgErr == nil {
		apiKey = cfg.Sources[config.SourceOpenAlex].APIKey
	}
	if email == "" {
		return errors.New("no contact email available: pass -email or configure one in config.toml")
	}

	db, err := openalexyield.OpenReadOnly(storePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	titles, err := openalexyield.SampleTitles(ctx, db, sample)
	if err != nil {
		return err
	}

	// The paid comparison goes through the same bounded, non-redirect-
	// following HTTP client construction every other OpenAlex integration in
	// this tree uses (internal/bootstrap.go's mustOpenAlexClient), minus the
	// budget/sourcegate admission stack: this is a deliberate, once-off,
	// operator-confirmed sample, not part of the automated resolver loop.
	transport := fetch.MetadataTransport(false)
	metaPolicy := fetch.DefaultPolicy()
	metaPolicy.MaxBytes = 8 << 20
	client, err := fetch.NewSecureHTTPClientNoRedirect(metaPolicy, nil, transport)
	if err != nil {
		return fmt.Errorf("constructing OpenAlex HTTP client: %w", err)
	}

	report, err := openalexyield.Run(ctx, openalexyield.ComparisonConfig{
		Confirm: confirm, Sample: sample, Client: client, ContactEmail: email, APIKey: apiKey,
	}, titles)
	if err != nil {
		return err
	}
	fmt.Println(report.Render())
	return nil
}
