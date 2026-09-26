// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/config"
)

func orphanProbeFixture(body string) []byte {
	return []byte("<!-- papio-fixture provider=\"provider\" scenario=\"drift\" origin=\"https://provider.example/\" captured=\"2026-08-10T00:00:00Z\" -->\n" + body)
}

func capturePaths(t *testing.T, b *Bridge, ctx context.Context) map[string]bool {
	t.Helper()
	listed, err := b.captureStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(listed))
	for _, capture := range listed {
		seen[capture.Path] = true
	}
	return seen
}

// The pre-index-first orphan scan is a migration, not steady-state work:
// index-first ordering makes new orphans impossible, so the full capture-tree
// scan runs at most once per bridge. A fresh orphan created after that scan
// is (by design) left for retention rather than triggering another scan.
func TestCaptureOrphanReconciliationRunsOncePerBridge(t *testing.T) {
	ctx := context.Background()
	b, _, _, data := newBridgeWithHoldingsAndZotio(t, nil, nil, func(cfg *config.Config) {
		cfg.Captures.MaxPerHost = 1
	})
	indexPath := filepath.Join(data, "captures", ".pending.json")

	if _, err := b.captureStore.StoreSanitizedPinned(ctx, "job-orphan-a", "provider.example", "drift", "provider", "1", orphanProbeFixture("orphan-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.captureStore.StoreSanitizedPinned(ctx, "job-orphan-b", "provider.example", "drift", "provider", "1", orphanProbeFixture("orphan-b")); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-fix crash window for both jobs: pins on disk, no
	// index entry pointing at them.
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	runSync(t, b)
	if !b.captureOrphanPinsReconciled {
		t.Fatal("orphan reconciliation did not run on the first polls")
	}
	// The collected pins are ordinary captures again, so the sweep enforces
	// the per-host bound instead of keeping both forever.
	if err := b.captureStore.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(capturePaths(t, b, ctx)); got != 1 {
		t.Fatalf("captures after orphan collection and sweep = %d, want 1", got)
	}

	// A fresh orphan after the one-time scan triggers no second scan: the
	// pin survives the next poll and its sweep.
	pathC, err := b.captureStore.StoreSanitizedPinned(ctx, "job-orphan-c", "provider.example", "drift", "provider", "1", orphanProbeFixture("orphan-c"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	if !b.captureOrphanPinsReconciled {
		t.Fatal("one-time flag lost")
	}
	if err := b.captureStore.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if seen := capturePaths(t, b, ctx); !seen[pathC] {
		t.Fatalf("fresh orphan %q was rescanned; the one-time scan must not repeat", pathC)
	}
}

// A failed orphan scan must not latch the done flag: the next poll retries.
func TestCaptureOrphanReconciliationRetriesAfterError(t *testing.T) {
	ctx := context.Background()
	b, _, _, data := newBridge(t)
	indexPath := filepath.Join(data, "captures", ".pending.json")

	if _, err := b.captureStore.StoreSanitizedPinned(ctx, "job-retry", "provider.example", "drift", "provider", "1", orphanProbeFixture("retry")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	runSync(t, b)
	if b.captureOrphanPinsReconciled {
		t.Fatal("reconciliation latched done despite a corrupt pending index")
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	if !b.captureOrphanPinsReconciled {
		t.Fatal("reconciliation did not retry after the index was repaired")
	}
}
