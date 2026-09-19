// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Command live-cohort measures papio's unattended acquisition behaviour
// against a cohort of real works, through the operator's running daemon.
// See dev/live-cohort.md for how to run it and how to read the report.
//
// It is the live counterpart to `papio bench`, which is hermetic. This one
// submits real jobs, spends real source budget, and reaches real providers.
// It never answers a human action and never drives the browser: a job parked
// on a person is a settled measurement here, recorded with the action kind
// that parked it.
//
// This is a measurement instrument, not a gate. It exits 0 once the run
// completes, even with wrong accepts, because a caller comparing two runs
// needs both to exit 0 for the comparison to happen at all. It exits 1 only
// when the run could not be taken: no cohort, no daemon, a malformed file.
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
	"strings"
	"syscall"
	"time"

	"papio/internal/bench"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/livecohort"
)

// socketCaller adapts ipc.Client to livecohort.Caller, minting a fresh
// request id per call the way the CLI's own wiring does.
type socketCaller struct {
	client  *ipc.Client
	timeout time.Duration
	seq     int
}

func (c *socketCaller) Call(ctx context.Context, method string, params, result any) error {
	c.seq++
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.client.Call(ctx, fmt.Sprintf("live-cohort-%d", c.seq), method, params, result)
}

func main() {
	cfg, cfgErr := config.Load("")
	if cfgErr != nil {
		// A measurement against an unparseable config would silently use
		// defaults for the data dir and socket, and so could measure a
		// different daemon than the operator runs.
		fmt.Fprintln(os.Stderr, "live-cohort: loading config:", cfgErr)
		os.Exit(1)
	}

	cohortPath := flag.String("cohort", "", "path to a papio-bench-cohort/1 document (required)")
	dataDir := flag.String("data-dir", cfg.DataDir, "papio data directory holding papio.sock and papio.db")
	budget := flag.Duration("budget", 5*time.Minute, "per-work settlement budget; a work still working at the budget is recorded as timed_out")
	poll := flag.Duration("poll", 2*time.Second, "interval between settlement checks")
	parkSettle := flag.Duration("park-settle", time.Minute, "how long a job seen in awaiting_human or needs_review must stay there before the run believes it; those states are not terminal and the daemon advances out of them")
	rpcTimeout := flag.Duration("rpc-timeout", 30*time.Second, "per-RPC timeout")
	force := flag.Bool("force", false, "submit even when the daemon already holds a live job for a work; without it such works are skipped and disclosed")
	keep := flag.Bool("keep", false, "keep the jobs this run created; by default every created job that produced no artifact is cancelled")
	noStore := flag.Bool("no-store", false, "skip the read-only store read that fills the untried-candidate column")
	jsonOut := flag.Bool("json", false, "emit the report as indented JSON instead of the rendered text report")
	out := flag.String("out", "", "write the report to this path as well as stdout")
	flag.Parse()

	if *cohortPath == "" {
		fmt.Fprintln(os.Stderr, "live-cohort: -cohort is required; see dev/live-cohort.md")
		os.Exit(1)
	}
	cohort, err := bench.LoadCohort(*cohortPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live-cohort:", err)
		os.Exit(1)
	}

	socket := filepath.Join(*dataDir, "papio.sock")
	caller := &socketCaller{client: ipc.NewSocketClient(socket), timeout: *rpcTimeout}

	// Ctrl-C stops polling and still renders what was measured: a run
	// interrupted after twenty of thirty works still holds twenty real
	// results, and discarding them would make a long run all-or-nothing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := probe(ctx, caller); err != nil {
		fmt.Fprintf(os.Stderr, "live-cohort: no daemon at %s: %v\n", socket, err)
		fmt.Fprintln(os.Stderr, "live-cohort: start it with `papio daemon status` (any ordinary command autostarts it), then re-run")
		os.Exit(1)
	}

	opts := livecohort.Options{
		Cohort:        cohort,
		Caller:        caller,
		RunID:         livecohort.NewRunID(time.Now()),
		PerWorkBudget: *budget,
		Poll:          *poll,
		ParkSettle:    *parkSettle,
		Force:         *force,
		Cleanup:       !*keep,
	}
	if !*noStore {
		inspector, err := livecohort.OpenStoreInspector(*dataDir)
		if err != nil {
			// Unfilled, not fatal: the column is evidence, and a run that
			// cannot gather it is still a run.
			fmt.Fprintln(os.Stderr, "live-cohort: untried-candidate column unfilled:", err)
		} else {
			defer func() { _ = inspector.Close() }()
			opts.Inspector = inspector
		}
	}

	fmt.Fprintf(os.Stderr, "live-cohort: %d work(s), budget %s each, cleanup %v — this submits REAL jobs to %s\n",
		len(cohort.Works), *budget, !*keep, socket)

	report, err := livecohort.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live-cohort:", err)
		os.Exit(1)
	}

	rendered, err := render(report, *jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live-cohort: rendering report:", err)
		os.Exit(1)
	}
	fmt.Print(rendered)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(rendered), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "live-cohort: writing report:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "live-cohort: report written to", *out)
	}
}

// probe fails fast when no daemon is listening, so a thirty-work run does
// not discover that on its first submission and report thirty submit
// failures as a measurement.
func probe(ctx context.Context, caller *socketCaller) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result map[string]any
	err := caller.Call(ctx, "stats.get", map[string]any{}, &result)
	var remote *ipc.RemoteError
	if errors.As(err, &remote) {
		// The daemon answered. Whatever it disliked about the probe, it is up.
		return nil
	}
	return err
}

func render(report livecohort.Report, asJSON bool) (string, error) {
	if asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", err
		}
		return string(data) + "\n", nil
	}
	var b strings.Builder
	if err := report.Render(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}
