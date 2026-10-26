// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/triage"
	"papio/internal/work"
)

// A failed terminal write must retain both recovery inputs until the retry.
func TestSettledGrabCommitBeforeFolderCleanup(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("valid=%t", valid), func(t *testing.T) {
			disableAutoBindDecision(t)
			b, jobs, cfg, _ := newBridge(t)
			b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				return pdf.ValidationReport{
					Payload:    pdf.PayloadReport{OK: valid},
					Structural: pdf.StructuralReport{Valid: valid, Pages: 1},
					Text:       pdf.TextReport{Excerpt: "no identifier here"},
				}, nil
			}
			ctx := context.Background()
			g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Commit Before Cleanup")
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
			landing := filepath.Join(dir, "main.pdf")
			writeFixturePDF(t, landing)
			if _, err := jobs.S.DB().ExecContext(ctx, `
				CREATE TRIGGER fail_grab_terminal BEFORE UPDATE OF state ON pdf_grabs
				WHEN NEW.state IN ('parked_no_identifier', 'failed_validation')
				BEGIN SELECT RAISE(ABORT, 'injected terminal failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := b.processSettledGrab(ctx, g, dir, "main.pdf"); err == nil {
				t.Fatal("terminal write unexpectedly succeeded")
			}
			after, err := b.grabs.Get(ctx, g.ID)
			if err != nil || after == nil || after.State != grab.StateQuarantined {
				t.Fatalf("grab after failed commit = %+v, err=%v", after, err)
			}
			for _, path := range []string{landing, after.QuarantinePath} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("recovery input %s lost before commit: %v", path, err)
				}
			}
			if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_grab_terminal`); err != nil {
				t.Fatal(err)
			}
			if err := b.SweepGrabs(ctx); err != nil {
				t.Fatal(err)
			}
			want := grab.StateFailedValidation
			if valid {
				want = grab.StateParkedNoIdentifier
			}
			after, err = b.grabs.Get(ctx, g.ID)
			if err != nil || after == nil || after.State != want {
				t.Fatalf("grab after retry = %+v, err=%v, want %s", after, err, want)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("landing directory remains after commit: %v", err)
			}
		})
	}
}

// A dismiss that races a bind must lose at the store CAS: the conditional
// delete matches only parked rows, so an already-bound grab reports conflict
// and keeps its row and quarantine copy for the committed job's recovery.
func TestPdfGrabDismissAfterBindKeepsRowAndQuarantineCopy(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	jobID := parkManualDownload(t, jobs, "wr_dismiss_race", handoffWork())
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Bound Before Dismiss")
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	// The bind on another surface commits first, keeping the quarantine copy
	// as the job's only bytes.
	if err := b.grabs.MarkIdentified(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkJobCreated(ctx, g.ID, jobID, "job_created"); err != nil {
		t.Fatal(err)
	}
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgTriageDecide, "", map[string]any{
		"request_id": "dismiss-race-0001", "item_id": triage.PdfGrabIDPrefix + g.ID,
		"op": "dismiss", "watch_scope": "all",
	}))
	res := firstOfType(msgs, protocol.MsgTriageDecideResult)
	if res == nil || res.Payload.(*protocol.TriageDecideResultPayload).Outcome != string(triage.DecisionConflict) {
		t.Fatalf("dismiss result = %+v, want conflict for a bound grab", msgs)
	}
	after, err := b.grabs.Get(ctx, g.ID)
	if err != nil || after == nil {
		t.Fatalf("bound grab row lost to a stale dismiss: %+v, err=%v", after, err)
	}
	if after.State != grab.StateJobCreated {
		t.Fatalf("grab state = %s, want job_created", after.State)
	}
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("quarantine copy removed by a stale dismiss: %v", err)
	}
}

// A filesystem failure must retain the claim, and the sweep must retry.
func TestPdfGrabDismissCleanupFailureKeepsRecoveryClaim(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Dismiss Cleanup Race")
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(cfg.DataDir, "quarantine", g.ID)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(blocker, "nested", "grab.pdf")
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	source := parkedGrabItemSource{store: b.jobs.S, grabs: b.grabs}
	if applied, err := source.DismissPdfGrab(ctx, g.ID); err == nil || applied {
		t.Fatalf("dismiss = %t, %v, want cleanup failure", applied, err)
	}
	after, err := b.grabs.Get(ctx, g.ID)
	if err != nil || after == nil || after.State != grab.StateDismissing {
		t.Fatalf("dismissal claim lost after cleanup failure: %+v, %v", after, err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, quarantine)
	// A later sweep resumes the durable claim without another operator click.
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("dismissal recovery: %v", err)
	}
	if after, err := b.grabs.Get(ctx, g.ID); err != nil || after != nil {
		t.Fatalf("grab survived successful dismiss: %+v, %v", after, err)
	}
	if _, err := os.Stat(filepath.Dir(quarantine)); !os.IsNotExist(err) {
		t.Fatalf("quarantine survived successful dismiss: %v", err)
	}
}

// A storage outage during deferred auto-bind recovery must surface to the
// sweep instead of reading as success: the tick fails while the staged bytes
// stay where the next tick finds them.
func TestRecoverDeferredAutoBindSurfacesStorageFailure(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := parkManualDownload(t, jobs, "wr_recover_store_fail",
		work.Work{DOI: "10.1234/recover.storefail.1", Title: "Deferred Recovery Surfaces Storage Failure", Authors: []string{"Lovelace, Ada"}, Year: 2026})
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Recover Store Failure")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkQuarantined(ctx, g.ID, filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkJobCreated(ctx, g.ID, id, "job_created"); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
	writeFixturePDF(t, staged)
	recordDeferredAdoption(t, jobs, id, "paper.pdf")
	brokenDB, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := brokenDB.Close(); err != nil {
		t.Fatal(err)
	}
	savedJobs := b.jobs
	b.jobs = &job.Store{S: brokenDB}
	defer func() { b.jobs = savedJobs }()
	if err := b.recoverDeferredAutoBind(ctx, g.ID, id); err == nil {
		t.Fatal("recoverDeferredAutoBind with a failing job store = nil, want the storage error")
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged bytes disturbed by a failed recovery: %v", err)
	}
	if _, err := b.grabs.Get(ctx, g.ID); err != nil {
		t.Fatalf("grab lookup after failed recovery: %v", err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %s, want awaiting_human (failed recovery adopts nothing)", row.State)
	}
}

func TestSweepAdoptionsReportsDeferredAutoBindFailure(t *testing.T) {
	for _, failRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("recordFailure=%t", failRecord), func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			id := parkManualDownload(t, jobs, "wr_sweep_deferred_failure", handoffWork())
			bindGrab(t, b, id, "Sweep Deferred Failure", "")
			dir := filepath.Join(cfg.EffectiveAdoptionRoot(), id)
			staged := filepath.Join(dir, "paper.pdf")
			writeFixturePDF(t, staged)
			writeFixturePDF(t, filepath.Join(dir, "unrelated.pdf"))
			recordDeferredAdoption(t, jobs, id, "paper.pdf")
			if failRecord {
				if _, err := jobs.S.DB().ExecContext(ctx, `
					CREATE TRIGGER fail_deferred_event BEFORE INSERT ON events
					WHEN NEW.kind = 'browser.adoption_deferred'
					BEGIN SELECT RAISE(ABORT, 'injected deferred record failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			b.svc.Validate = nil
			err := b.SweepAdoptions(ctx)
			if err == nil || !strings.Contains(err.Error(), "validation dependency") {
				t.Fatalf("sweep lost original ingestion failure: %v", err)
			}
			if failRecord && !strings.Contains(err.Error(), "injected deferred record failure") {
				t.Fatalf("sweep lost diagnostic write failure: %v", err)
			}
			if _, err := os.Stat(staged); err != nil {
				t.Fatalf("failed sweep discarded staged bytes: %v", err)
			}
			requireNotAdopted(t, jobs, id)
			wantEvents := 2
			if failRecord {
				wantEvents = 1
			}
			if got := countDeferredAdoptions(t, jobs, id); got != wantEvents {
				t.Fatalf("deferred events = %d, want %d", got, wantEvents)
			}
		})
	}
}
