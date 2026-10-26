// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// orphanProofPermit acquires a direct_get permit and reconciles it into
// unknown_completion, the state the orphan-settlement rules may settle from.
func orphanProofPermit(t *testing.T, js *Store, name string) (string, string) {
	t.Helper()
	ctx := context.Background()
	jobID := permitJob(t, js, name)
	permit := acquireDrive(t, js, EffectPermitIdentity{
		JobID: jobID, Kind: DirectGet, DriveAttemptID: "attempt-" + name,
		Ordinal: 0, Strategy: "direct_get", Revision: "r1",
	}, "domain-"+name, time.Now().Add(time.Minute))
	got, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 1,
	})
	if err != nil || got == nil || got.Status != UnknownCompletion {
		t.Fatalf("reconcile = %+v err=%v, want unknown_completion", got, err)
	}
	return jobID, permit.ID
}

func orphanProofFile(pdf, movedAside bool, sha string) OrphanPermitFile {
	return OrphanPermitFile{
		Filename: "paper.pdf", SizeBytes: 2048, SHA256: sha,
		ModifiedAt: time.Now().UTC(), PDF: pdf, MovedAside: movedAside,
	}
}

// TestSettleOrphanedPermitFromFileProof pins the store half of the orphan
// settlement contract: settlement needs proof (bytes that are no paper, or a
// terminal job), and anything less leaves the permit unresolved.
func TestSettleOrphanedPermitFromFileProof(t *testing.T) {
	goodSHA := strings.Repeat("ab", 32)
	settled := func(t *testing.T, js *Store, permitID string) {
		t.Helper()
		got, err := js.GetEffectPermit(context.Background(), permitID)
		if err != nil || got == nil || got.Status != Settled {
			t.Fatalf("permit = %+v err=%v, want settled", got, err)
		}
	}
	unproven := func(t *testing.T, js *Store, permitID string, err error) {
		t.Helper()
		if !errors.Is(err, ErrOrphanPermitUnproven) {
			t.Fatalf("settle err = %v, want %v", err, ErrOrphanPermitUnproven)
		}
		got, getErr := js.GetEffectPermit(context.Background(), permitID)
		if getErr != nil || got == nil || got.Status != UnknownCompletion {
			t.Fatalf("permit = %+v err=%v, want still unknown_completion", got, getErr)
		}
	}

	t.Run("no paper settles", func(t *testing.T) {
		js := testStore(t)
		ctx := context.Background()
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_nopaper")
		if err := js.SettleOrphanedPermitFromFile(ctx, permitID, jobID, OrphanPermitEffectCompletedNoPaper, orphanProofFile(false, true, goodSHA)); err != nil {
			t.Fatalf("settle: %v", err)
		}
		settled(t, js, permitID)
	})

	t.Run("terminal job settles pdf", func(t *testing.T) {
		js := testStore(t)
		ctx := context.Background()
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_terminal")
		if err := js.Cancel(ctx, jobID, TerminalReasonCancelledByUser); err != nil {
			t.Fatal(err)
		}
		if err := js.SettleOrphanedPermitFromFile(ctx, permitID, jobID, OrphanPermitEffectCompletedJobTerminal, orphanProofFile(true, false, goodSHA)); err != nil {
			t.Fatalf("settle: %v", err)
		}
		settled(t, js, permitID)
	})

	t.Run("no paper refuses pdf", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_pdf")
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitEffectCompletedNoPaper, orphanProofFile(true, true, goodSHA))
		unproven(t, js, permitID, err)
	})

	t.Run("no paper refuses unmoved bytes", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_unmoved")
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitEffectCompletedNoPaper, orphanProofFile(false, false, goodSHA))
		unproven(t, js, permitID, err)
	})

	t.Run("terminal refuses live job", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_live")
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitEffectCompletedJobTerminal, orphanProofFile(true, false, goodSHA))
		unproven(t, js, permitID, err)
	})

	t.Run("unknown rule refused", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_rule")
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitRule("bogus"), orphanProofFile(false, true, goodSHA))
		unproven(t, js, permitID, err)
	})

	t.Run("short sha refused", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_sha")
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitEffectCompletedNoPaper, orphanProofFile(false, true, strings.Repeat("a", 63)))
		unproven(t, js, permitID, err)
	})

	t.Run("unattributable file refused", func(t *testing.T) {
		js := testStore(t)
		jobID, permitID := orphanProofPermit(t, js, "wr_orphan_proof_name")
		file := orphanProofFile(false, true, goodSHA)
		file.Filename = ""
		err := js.SettleOrphanedPermitFromFile(context.Background(), permitID, jobID, OrphanPermitEffectCompletedNoPaper, file)
		unproven(t, js, permitID, err)
	})
}
