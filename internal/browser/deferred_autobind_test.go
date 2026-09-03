// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/work"
)

// recoverDeferredAutoBind is the sweep-time second chance for an auto-bind
// whose bytes were committed to a job but never ingested (the MV3
// worker-death class AGENTS.md records). Its whole reason to exist is that a
// recoverable paper must never stay orphaned, so every case here asserts the
// end state that proves recovery happened — the job holds a promoted
// artifact — rather than merely that the call returned nil. The function
// swallows every error by design, so "no error" is worthless as a signal.

func deferredWork(doi, title string) work.Work {
	return work.Work{DOI: doi, Title: title, Authors: []string{"Lovelace, Ada"}, Year: 2026}
}

// recordDeferredAdoption writes the durable browser.adoption_deferred event
// exactly as recordAdoptionDeferred does, which is the only thing
// recoverDeferredAutoBind reads to learn which filename to look for.
func recordDeferredAdoption(t *testing.T, jobs *job.Store, jobID, filename string) {
	t.Helper()
	if err := jobs.S.AppendEvent(context.Background(), jobID, "browser.adoption_deferred",
		map[string]any{"filename": filename, "reason": "staged file not settled yet"}); err != nil {
		t.Fatal(err)
	}
}

// bindGrab drives one grab to the terminal job_created state bound to jobID,
// which is the only shape the sweep hands to recoverDeferredAutoBind.
func bindGrab(t *testing.T, b *Bridge, jobID, title string, quarantinePath string) *grab.Grab {
	t.Helper()
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", title)
	if err != nil {
		t.Fatal(err)
	}
	if quarantinePath != "" {
		if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantinePath); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.grabs.MarkJobCreated(ctx, g.ID, jobID, "job_created"); err != nil {
		t.Fatal(err)
	}
	bound, err := b.grabs.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != grab.StateJobCreated {
		t.Fatalf("grab state = %s, want %s", bound.State, grab.StateJobCreated)
	}
	return bound
}

// requireRecovered asserts the recovery actually ingested the staged bytes:
// the job left awaiting_human for ready and carries a promoted artifact.
func requireRecovered(t *testing.T, jobs *job.Store, jobID string) {
	t.Helper()
	row, err := jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady {
		t.Fatalf("job state = %s, want %s (deferred bytes were not recovered)", row.State, job.StateReady)
	}
	if row.ArtifactSHA256 == "" {
		t.Fatal("job reached ready with no promoted artifact")
	}
}

// requireNotAdopted asserts nothing was fabricated: the job is still parked
// awaiting a human file and owns no artifact.
func requireNotAdopted(t *testing.T, jobs *job.Store, jobID string) {
	t.Helper()
	row, err := jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %s, want %s (adoption fabricated without staged bytes)", row.State, job.StateAwaitingHuman)
	}
	if row.ArtifactSHA256 != "" {
		t.Fatalf("job holds artifact %s with no recoverable file", row.ArtifactSHA256)
	}
}

// countDeferredAdoptions counts the durable browser.adoption_deferred events
// on a job. Recovery is silent by design — every failure path returns nil —
// so this count is the only externally visible trace of an ingest that was
// attempted. A recovery that declines to act must leave it untouched.
func countDeferredAdoptions(t *testing.T, jobs *job.Store, jobID string) int {
	t.Helper()
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, ev := range events {
		if ev["kind"] == "browser.adoption_deferred" {
			total++
		}
	}
	return total
}

func TestRecoverDeferredAutoBind(t *testing.T) {
	t.Run("ActiveAdoptionDir", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_active",
			deferredWork("10.1234/recover.active.1", "Deferred Recovery From Active Directory"))
		g := bindGrab(t, b, id, "Recover Active", "")
		writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
		recordDeferredAdoption(t, jobs, id, "paper.pdf")

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireRecovered(t, jobs, id)
	})

	t.Run("RejectedFallbackDir", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_rejected",
			deferredWork("10.1234/recover.rejected.1", "Deferred Recovery From Rejected Directory"))
		g := bindGrab(t, b, id, "Recover Rejected", "")
		// Only the rejected/<jobID> preservation copy exists: the active
		// adoption directory is genuinely empty, so recovery must reach into
		// the fallback and move the bytes back before ingesting them.
		rejected := filepath.Join(cfg.EffectiveAdoptionRoot(), "rejected", id, "paper.pdf")
		writeFixturePDF(t, rejected)
		recordDeferredAdoption(t, jobs, id, "paper.pdf")
		if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), id)); !os.IsNotExist(err) {
			t.Fatalf("active adoption dir must not pre-exist: %v", err)
		}

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireRecovered(t, jobs, id)
		if _, err := os.Stat(rejected); err == nil {
			t.Fatal("rejected copy still present: bytes were ingested from outside the adoption dir")
		}
	})

	t.Run("GrabStagingCleanedAfterRecovery", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_cleanup",
			deferredWork("10.1234/recover.cleanup.1", "Deferred Recovery Clears Grab Staging"))
		quarantine := filepath.Join(cfg.DataDir, "grab-quarantine-copy.pdf")
		writeFixturePDF(t, quarantine)
		g := bindGrab(t, b, id, "Recover Cleanup", quarantine)
		if g.QuarantinePath != quarantine {
			t.Fatalf("grab quarantine path = %q, want %q", g.QuarantinePath, quarantine)
		}
		staging := filepath.Join(cfg.EffectiveAdoptionRoot(), grabsDirName, g.ID)
		writeFixturePDF(t, filepath.Join(staging, "paper.pdf"))
		writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
		recordDeferredAdoption(t, jobs, id, "paper.pdf")

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireRecovered(t, jobs, id)
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Fatalf("grab staging dir %s survived recovery: %v", staging, err)
		}
		if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
			t.Fatalf("grab quarantine copy %s survived recovery: %v", quarantine, err)
		}
	})

	t.Run("NoDeferredEventAdoptsNothing", func(t *testing.T) {
		// A settled file in the adoption directory is not evidence that this
		// job's auto-bind was deferred. Recovery is keyed on the durable
		// event; without one it must adopt nothing, or an unrelated file
		// dropped into a job directory would be filed as that job's paper.
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_noevent",
			deferredWork("10.1234/recover.noevent.1", "Recovery Without Deferred Event"))
		g := bindGrab(t, b, id, "Recover No Event", "")
		staged := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
		writeFixturePDF(t, staged)
		before, err := os.ReadFile(staged)
		if err != nil {
			t.Fatal(err)
		}

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireNotAdopted(t, jobs, id)
		// With no deferred event there is no filename to recover, so
		// recovery must not reach ingestion at all: a failed attempt would
		// leave its own browser.adoption_deferred event behind.
		if got := countDeferredAdoptions(t, jobs, id); got != 0 {
			t.Fatalf("browser.adoption_deferred events = %d, want 0 (recovery guessed a filename)", got)
		}
		// The file must still be there, byte-identical. Adopting nothing is
		// only half the contract: recovery that deleted or moved an
		// unreferenced staged file would destroy an operator's download while
		// every state and event assertion above still passed.
		after, err := os.ReadFile(staged)
		if err != nil {
			t.Fatalf("staged file did not survive recovery: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("staged file changed during recovery: %d bytes before, %d after", len(before), len(after))
		}
	})

	t.Run("MissingStagedFileAdoptsNothing", func(t *testing.T) {
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_missing",
			deferredWork("10.1234/recover.missing.1", "Recovery With Missing Staged File"))
		g := bindGrab(t, b, id, "Recover Missing", "")
		recordDeferredAdoption(t, jobs, id, "paper.pdf")
		// A zero-byte placeholder is the browser's in-progress target, never
		// settled bytes; it must not be adopted either.
		empty := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
		if err := os.MkdirAll(filepath.Dir(empty), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireNotAdopted(t, jobs, id)
		if _, err := os.Stat(empty); err != nil {
			t.Fatalf("placeholder file disturbed: %v", err)
		}
		// Only the event this test seeded. A recovery that fed the
		// placeholder to ingestion would fail and append a second one.
		if got := countDeferredAdoptions(t, jobs, id); got != 1 {
			t.Fatalf("browser.adoption_deferred events = %d, want 1 (placeholder bytes were sent to ingestion)", got)
		}
	})

	t.Run("TerminalJobIsNeverRecovered", func(t *testing.T) {
		// A cancelled job's staged bytes are preserved for the human, not
		// re-adopted behind their back. The guard means recovery does no work
		// at all, so the seeded deferred event must stay the only one: an
		// attempted-then-failed ingest would append a second.
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_terminal",
			deferredWork("10.1234/recover.terminal.1", "Recovery Declines Terminal Job"))
		g := bindGrab(t, b, id, "Recover Terminal", "")
		staged := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
		writeFixturePDF(t, staged)
		recordDeferredAdoption(t, jobs, id, "paper.pdf")
		if err := jobs.Cancel(ctx, id, job.TerminalReasonUnknown); err != nil {
			t.Fatal(err)
		}

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		row, err := jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != job.StateCancelled {
			t.Fatalf("job state = %s, want %s (terminal job was re-adopted)", row.State, job.StateCancelled)
		}
		if row.ArtifactSHA256 != "" {
			t.Fatalf("terminal job gained artifact %s", row.ArtifactSHA256)
		}
		if _, err := os.Stat(staged); err != nil {
			t.Fatalf("staged bytes for a terminal job must be preserved: %v", err)
		}
		if got := countDeferredAdoptions(t, jobs, id); got != 1 {
			t.Fatalf("browser.adoption_deferred events = %d, want 1 (recovery attempted an ingest on a terminal job)", got)
		}
	})

	t.Run("IngestFailureStaysRecoverable", func(t *testing.T) {
		// Recovery is the last line of defence for these bytes, so its own
		// failure must be durable: a fresh browser.adoption_deferred event
		// and the staged file left where the next sweep will find it. A
		// missing validation dependency stands for the whole class of
		// infrastructure errors ingestion can return before it changes any
		// job state (a validation *verdict* is a different path: that parks
		// the job for review and is not a failed ingest).
		b, jobs, cfg, _ := newBridge(t)
		ctx := context.Background()
		id := parkManualDownload(t, jobs, "wr_recover_ingestfail",
			deferredWork("10.1234/recover.ingestfail.1", "Recovery Records Its Own Failure"))
		g := bindGrab(t, b, id, "Recover Ingest Failure", "")
		staged := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
		writeFixturePDF(t, staged)
		recordDeferredAdoption(t, jobs, id, "paper.pdf")
		b.svc.Validate = nil

		if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err != nil {
			t.Fatalf("recoverDeferredAutoBind: %v", err)
		}
		requireNotAdopted(t, jobs, id)
		if got := countDeferredAdoptions(t, jobs, id); got != 2 {
			t.Fatalf("browser.adoption_deferred events = %d, want 2 (failed recovery left no durable trace)", got)
		}
		if _, err := os.Stat(staged); err != nil {
			t.Fatalf("staged bytes discarded by a failed recovery: %v", err)
		}
	})
}
