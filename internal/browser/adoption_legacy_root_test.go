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

// legacyRootBridge builds a bridge whose effective adoption root is the new
// browser-steerable <downloads>/papio shape while <data_dir>/adoptions still
// exists as the superseded root — the exact layout an upgraded install has.
func legacyRootBridge(t *testing.T) (*Bridge, *job.Store, config.Config) {
	t.Helper()
	b, jobs, cfg, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) {
		c.Browser.AdoptionRoot = filepath.Join(c.DataDir, "Downloads", config.AdoptionDirName)
	})
	if cfg.LegacyAdoptionRoot() == "" {
		t.Fatalf("test setup: expected a distinct legacy root beside %q", cfg.EffectiveAdoptionRoot())
	}
	return b, jobs, cfg
}

// TestSweepAdoptionsStillDrainsTheLegacyRoot is the no-orphan guarantee for
// the adoption-root default change. An install that has been adopting into
// <data_dir>/adoptions may have settled files sitting there when the default
// moves under the browser's download directory; abandoning them would lose
// real PDFs a researcher already downloaded. Without the legacy root in the
// sweep's search path this job stays parked at awaiting_human forever.
func TestSweepAdoptionsStillDrainsTheLegacyRoot(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg := legacyRootBridge(t)

	id := park(t, jobs, "wr_legacy_sweep", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.LegacyAdoptionRoot(), id, "paper.pdf"))

	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady {
		t.Fatalf("state = %s, want ready: the file in the legacy root %q was not adopted",
			row.State, cfg.LegacyAdoptionRoot())
	}
	if row.ArtifactSHA256 == "" {
		t.Fatal("adopted job carries no artifact")
	}
}

// TestSweepAdoptionsPrefersTheEffectiveRoot pins the search order: the
// browser-steerable root wins, so a file the browser just delivered is never
// shadowed by stale bytes left in the drain-only legacy location.
func TestSweepAdoptionsPrefersTheEffectiveRoot(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg := legacyRootBridge(t)

	id := park(t, jobs, "wr_legacy_precedence", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "current.pdf"))
	writeFixturePDF(t, filepath.Join(cfg.LegacyAdoptionRoot(), id, "stale.pdf"))

	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady {
		t.Fatalf("state = %s, want ready", row.State)
	}
	// The legacy copy is never the source, and nothing removed it either: the
	// legacy root is drained by terminal sweeping, not by adoption.
	if _, err := os.Stat(filepath.Join(cfg.LegacyAdoptionRoot(), id, "stale.pdf")); err != nil {
		t.Fatalf("legacy file was disturbed by adoption: %v", err)
	}
}

// TestSweepTerminalAdoptionsCollectsAdoptedLegacyDirectories is what makes the
// legacy location transient rather than permanent for the ordinary case: once
// a job is ready its bytes are in the content-addressed artifact store, so the
// landing copy is provably redundant and the directory is collected.
func TestSweepTerminalAdoptionsCollectsAdoptedLegacyDirectories(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg := legacyRootBridge(t)

	id := park(t, jobs, "wr_legacy_terminal", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.LegacyAdoptionRoot(), id, "paper.pdf"))
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady {
		t.Fatalf("state = %s, want ready before terminal sweeping", row.State)
	}
	if err := b.SweepTerminalAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.LegacyAdoptionRoot(), id)); !os.IsNotExist(err) {
		t.Fatalf("legacy landing directory survived terminal sweeping: %v", err)
	}
}

// TestSweepTerminalAdoptionsNeverDeletesUnstoredLegacyBytes is the data-loss
// guard on the upgrade. The legacy root was unreachable and therefore never
// swept, so it can hold a PDF a researcher downloaded by hand for a job that
// later failed — the only copy. Bringing that directory into scope must not
// turn a state transition made before it was in scope into a deletion. The
// effective root keeps the original rule, because papio itself created and
// owned every directory in it.
func TestSweepTerminalAdoptionsNeverDeletesUnstoredLegacyBytes(t *testing.T) {
	ctx := context.Background()
	b, jobs, cfg := legacyRootBridge(t)

	legacyID := park(t, jobs, "wr_legacy_failed", handoffWork())
	effectiveID := park(t, jobs, "wr_effective_failed", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.LegacyAdoptionRoot(), legacyID, "by-hand.pdf"))
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), effectiveID, "by-hand.pdf"))
	for _, id := range []string{legacyID, effectiveID} {
		if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, job.StateUnavailable,
			map[string]any{"reason": "exhausted"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := b.SweepTerminalAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.LegacyAdoptionRoot(), legacyID, "by-hand.pdf")); err != nil {
		t.Fatalf("a hand-downloaded file in the legacy root was deleted for a job with no artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), effectiveID)); !os.IsNotExist(err) {
		t.Fatalf("the effective root's terminal collection rule changed: %v", err)
	}
}

// TestAdoptionWritesOnlyToTheEffectiveRoot pins the other half of drain-only:
// a browser-delivered job's landing directory is created under the effective
// root, never beside the legacy one.
func TestAdoptionWritesOnlyToTheEffectiveRoot(t *testing.T) {
	_, _, cfg := legacyRootBridge(t)

	roots := cfg.AdoptionRoots()
	if len(roots) != 2 || roots[0] != cfg.EffectiveAdoptionRoot() || roots[1] != cfg.LegacyAdoptionRoot() {
		t.Fatalf("AdoptionRoots() = %q, want [effective legacy]", roots)
	}
	if !config.BrowserSteerableAdoptionRoot(roots[0]) {
		t.Fatalf("the write target %q is not reachable by browser steering", roots[0])
	}
	if config.BrowserSteerableAdoptionRoot(roots[1]) {
		t.Fatalf("the legacy root %q looks steerable; this test no longer covers the upgrade shape", roots[1])
	}
}
