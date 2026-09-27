// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
)

// An interrupted provisional write for a job that is still awaiting_human must
// survive the poll that reconciles leases and collects genuine orphans: a pin
// without its index is re-indexed, an index without its pin gains the pin
// back, and a pin for a job that already left is still collected.
func TestCapturePollPreservesActiveInterruptedLeases(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg, data := newBridgeWithHoldingsAndZotio(t, nil, nil, func(cfg *config.Config) {
		cfg.Captures.MaxPerHost = 1
	})
	indexPath := filepath.Join(data, "captures", ".pending.json")
	live := park(t, jobs, "wr_live", handoffWork())

	livePath, err := b.captureStore.StoreSanitizedPinned(ctx, live, "provider.example", "drift", "provider", "1", orphanProbeFixture("live"))
	if err != nil {
		t.Fatal(err)
	}
	deadPath, err := b.captureStore.StoreSanitizedPinned(ctx, "job-dead-no-row", "provider.example", "drift", "provider", "1", orphanProbeFixture("dead"))
	if err != nil {
		t.Fatal(err)
	}

	// Crash window one: pins on disk, no index entry for either job.
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	runSync(t, b)
	if !b.captureOrphanPinsReconciled {
		t.Fatal("lease reconciliation did not run on the first polls")
	}

	// The live lease is re-indexed before orphan cleanup, so its pin survives;
	// the dead pin belongs to no awaiting job and is collected.
	pending, err := b.captureStore.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != live {
		t.Fatalf("pending jobs after poll = %v, want [%s]", pending, live)
	}
	if seen := capturePaths(t, b, ctx); !seen[livePath] {
		t.Fatalf("live capture %q was collected; active leases must be re-indexed first", livePath)
	}
	// The dead job has no row, so the release pass drops its lease directly;
	// either way its pin must not survive as leased evidence.
	if err := b.captureStore.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if seen := capturePaths(t, b, ctx); !seen[livePath] {
		t.Fatalf("live capture %q missing after sweep", livePath)
	}
	_ = deadPath

	// Crash window two, after a restart: the index names the live lease but
	// the pin sidecar never landed. Retention must keep the bytes and the
	// fresh bridge must restore the pin.
	// Find the live pin path via the store List (pin sidecar sits beside it).
	listed, err := b.captureStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var liveHTML string
	for _, row := range listed {
		if row.Path == livePath {
			liveHTML = row.Path
		}
	}
	if liveHTML == "" {
		t.Fatalf("live capture %q not listed", livePath)
	}
	// Derive the pin sidecar path the same way the store does.
	pinSidecar := liveHTML[:len(liveHTML)-len(".html")] + ".pin.json"
	if err := os.Remove(pinSidecar); err != nil {
		t.Fatal(err)
	}

	restarted := NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
	runSync(t, restarted, hello())
	runSync(t, restarted)
	if !restarted.captureOrphanPinsReconciled {
		t.Fatal("restarted bridge did not reconcile leases")
	}
	if seen := capturePaths(t, b, ctx); !seen[livePath] {
		t.Fatalf("live indexed-but-unpinned capture %q was pruned before its pin was restored", livePath)
	}
	pending, err = b.captureStore.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != live {
		t.Fatalf("pending jobs after restart = %v, want [%s]", pending, live)
	}
}

// The awaiting page is capped, so a live job past the cap with a pin-first
// crash (pin on disk, no index entry) is invisible to the page alone. The
// poll must complete its active set from on-disk lease traces before orphan
// cleanup, or it drops the live pin.
func TestCapturePollPreservesActiveLeasePastAwaitingCap(t *testing.T) {
	ctx := context.Background()
	b, jobs, _, data := newBridgeWithHoldingsAndZotio(t, nil, nil, func(cfg *config.Config) {
		cfg.Captures.MaxPerHost = 10
	})
	live := park(t, jobs, "wr_capped", handoffWork())

	livePath, err := b.captureStore.StoreSanitizedPinned(ctx, live, "provider.example", "drift", "provider", "1", orphanProbeFixture("capped"))
	if err != nil {
		t.Fatal(err)
	}
	// Pin-first crash: pin durable, index never landed.
	if err := os.Remove(filepath.Join(data, "captures", ".pending.json")); err != nil {
		t.Fatal(err)
	}

	// Hide the live job from the awaiting page, as the cap would.
	b.listAwaitingHuman = func(context.Context, int) ([]job.Row, error) {
		return nil, nil
	}

	runSync(t, b, hello())
	runSync(t, b)
	if !b.captureOrphanPinsReconciled {
		t.Fatal("lease reconciliation did not run")
	}
	pending, err := b.captureStore.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != live {
		t.Fatalf("pending jobs after poll = %v, want [%s]", pending, live)
	}
	if seen := capturePaths(t, b, ctx); !seen[livePath] {
		t.Fatalf("live capture %q was collected despite its job awaiting", livePath)
	}
}
