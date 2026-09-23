// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

const (
	orphanFirstSession  = "sess-orphan-first-000000000000000001"
	orphanSecondSession = "sess-orphan-second-00000000000000001"
)

type orphanPermitFixture struct {
	b        *Bridge
	jobs     *job.Store
	cfg      config.Config
	jobID    string
	permitID string
}

// newOrphanPermitFixture replays job_30768938d8e8460d03338473f8's sequence:
// the holder is offered a direct_get route and acquires its permit. With
// reload, the extension reloads seconds later and a new holder generation says
// hello. With reconcile, the permit becomes unknown_completion the way
// production makes it: the replacement holder answers the reconcile request
// with no dispatch, or, without reload, a same-generation observation does.
func newOrphanPermitFixture(t *testing.T, name string, reload, reconcile bool) orphanPermitFixture {
	t.Helper()
	ctx := context.Background()
	b, jobs, cfg, _ := newBridge(t)
	id := parkWithProviderEvidence(t, jobs, "wr_orphan_"+name, handoffWork(), "onlinelibrary.wiley.com")
	msgs, _ := runSyncAs(t, b, orphanFirstSession, helloWithFeatures(t, "0.15.0", providerDirectGetV1Feature, effectPermitFeature))
	req := firstOfType(msgs, protocol.MsgProviderDirectGetRequest)
	if req == nil {
		t.Fatalf("missing provider direct request: %v", msgs)
	}
	p := req.Payload.(*protocol.ProviderDirectGetRequestPayload)
	permit, err := jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{
		JobID: id, Kind: job.EffectKindDirectGet, DriveAttemptID: p.DriveAttemptID,
		Ordinal: p.Ordinal, Strategy: "direct_get", Revision: p.RouteRevision,
	})
	if err != nil || permit == nil || permit.Status != job.EffectPermitHeld {
		t.Fatalf("direct permit = %+v err=%v", permit, err)
	}
	if reload {
		if _, _, err := b.RequestDevReload(""); err != nil {
			t.Fatalf("request dev reload: %v", err)
		}
		if reloadMsgs, _ := runSyncAs(t, b, orphanFirstSession); firstOfType(reloadMsgs, protocol.MsgDevReload) == nil {
			t.Fatalf("holder poll did not emit dev_reload: %v", reloadMsgs)
		}
		if _, err := b.Sync(ctx, orphanFirstSession, true, nil); err != nil {
			t.Fatalf("release holder: %v", err)
		}
		runSyncAs(t, b, orphanSecondSession, helloWithFeatures(t, "0.15.0", providerDirectGetV1Feature, effectPermitFeature))
		if b.arbitration.generation() <= permit.BrowserHolderGeneration {
			t.Fatalf("holder generation %d did not supersede the permit's %d", b.arbitration.generation(), permit.BrowserHolderGeneration)
		}
	}
	if reconcile {
		if reload {
			request := nextEffectPermitReconcileRequest(t, b)
			runSyncAs(t, b, orphanSecondSession, inFrame(t, protocol.MsgEffectPermitReconcileResponse, id, map[string]any{
				"request_id": request.RequestID, "permit_id": permit.ID, "outcome": "recorded",
				"dispatched": false, "download_present": false, "acknowledged": false, "tab_present": false,
			}))
		} else if _, err := jobs.ReconcileEffectPermit(ctx, job.EffectPermitObservation{
			PermitID: permit.ID, BrowserHolderGeneration: permit.BrowserHolderGeneration,
		}); err != nil {
			t.Fatal(err)
		}
		if got := orphanPermitStatus(t, jobs, permit.ID); got != job.EffectPermitUnknownCompletion {
			t.Fatalf("permit status = %q, want unknown_completion", got)
		}
	}
	return orphanPermitFixture{b: b, jobs: jobs, cfg: cfg, jobID: id, permitID: permit.ID}
}

func (f orphanPermitFixture) expireLease(t *testing.T) {
	t.Helper()
	if _, err := f.jobs.S.DB().Exec(`UPDATE effect_permits SET lease_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), f.permitID); err != nil {
		t.Fatal(err)
	}
}

func (f orphanPermitFixture) jobFile(name string) string {
	return filepath.Join(f.cfg.EffectiveAdoptionRoot(), f.jobID, name)
}

func (f orphanPermitFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.b.SweepAdoptions(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

func orphanPermitStatus(t *testing.T, jobs *job.Store, permitID string) job.EffectPermitStatus {
	t.Helper()
	permit, err := jobs.GetEffectPermit(context.Background(), permitID)
	if err != nil || permit == nil {
		t.Fatalf("permit %s = %+v err=%v", permitID, permit, err)
	}
	return permit.Status
}

// assertLaneFree checks the global lane admits the next effect.
func (f orphanPermitFixture) assertLaneFree(t *testing.T) {
	t.Helper()
	if got := orphanPermitStatus(t, f.jobs, f.permitID); got != job.EffectPermitSettled {
		t.Fatalf("orphaned permit status = %q, want settled", got)
	}
	if live, err := f.jobs.LiveEffectPermit(context.Background()); err != nil || live != nil {
		t.Fatalf("effect lane still occupied by %+v err=%v", live, err)
	}
}

func orphanResolution(t *testing.T, jobs *job.Store, jobID string) map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, event := range events {
		if event["kind"] != job.OrphanPermitResolvedEvent {
			continue
		}
		if found != nil {
			t.Fatalf("orphaned permit resolved twice: %v", events)
		}
		found, _ = event["detail"].(map[string]any)
	}
	return found
}

func TestOrphanedDirectPermitSettlesFromHTMLAndMovesItAside(t *testing.T) {
	f := newOrphanPermitFixture(t, "html", true, true)
	html := append([]byte("<!DOCTYPE html><html><head><title>Session expired</title></head><body>"), make([]byte, 207*1024)...)
	if err := os.MkdirAll(filepath.Dir(f.jobFile("paper.pdf")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.jobFile("paper.pdf"), html, 0o600); err != nil {
		t.Fatal(err)
	}
	f.sweep(t)
	f.assertLaneFree(t)
	if _, err := os.Stat(f.jobFile("paper.pdf")); !os.IsNotExist(err) {
		t.Fatalf("HTML still in the adoption directory: err=%v", err)
	}
	aside := filepath.Join(f.cfg.EffectiveAdoptionRoot(), "rejected", f.jobID, "paper.pdf")
	if got, err := os.ReadFile(aside); err != nil || len(got) != len(html) {
		t.Fatalf("HTML was not kept aside at %s: %d bytes, err=%v", aside, len(got), err)
	}
	detail := orphanResolution(t, f.jobs, f.jobID)
	if detail["rule"] != string(job.OrphanPermitEffectCompletedNoPaper) || detail["permit_id"] != f.permitID ||
		detail["filename"] != "paper.pdf" || detail["moved_to"] != "rejected" || detail["pdf"] != false ||
		len(stringDetail(detail, "sha256")) != 64 {
		t.Fatalf("resolution evidence = %#v", detail)
	}
	row, err := f.jobs.Get(context.Background(), f.jobID)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after no-paper settlement = %+v err=%v, want awaiting_human", row, err)
	}
}

func TestOrphanedDirectPermitPDFIsAdoptedAndSettlesThePermit(t *testing.T) {
	f := newOrphanPermitFixture(t, "pdf", true, true)
	writeFixturePDF(t, f.jobFile("paper.pdf"))
	f.sweep(t)
	row, err := f.jobs.Get(context.Background(), f.jobID)
	if err != nil || row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("orphaned PDF was not adopted: %+v err=%v", row, err)
	}
	f.assertLaneFree(t)
	if got := countEvents(t, f.jobs, f.jobID, job.OrphanPermitAttributedEvent); got != 1 {
		t.Fatalf("attribution events = %d, want 1", got)
	}
	producer, err := f.jobs.ArtifactProducerForArtifact(context.Background(), f.jobID, "paper.pdf", row.ArtifactSHA256)
	if err != nil || producer == nil || producer.Kind != job.DirectGet {
		t.Fatalf("adopted bytes producer = %+v err=%v, want the orphaned direct_get", producer, err)
	}
	permit, err := f.jobs.GetEffectPermit(context.Background(), f.permitID)
	if err != nil || producer.DriveAttemptID != permit.DriveAttemptID || producer.Revision != permit.Revision {
		t.Fatalf("producer %+v does not name permit %+v (err=%v)", producer, permit, err)
	}
}

func TestOrphanedDirectPermitWithNoEffectSettlesAndALateDownloadStillAdopts(t *testing.T) {
	f := newOrphanPermitFixture(t, "none", true, true)
	f.expireLease(t)
	f.sweep(t)
	f.assertLaneFree(t)
	detail := orphanResolution(t, f.jobs, f.jobID)
	if detail["rule"] != string(job.OrphanPermitNoEffectObserved) ||
		intDetail(detail, "download_events_since_permit") != 0 || intDetail(detail, "entries_since_permit") != 0 ||
		intDetail(detail, "current_holder_generation") <= intDetail(detail, "permit_holder_generation") {
		t.Fatalf("resolution evidence = %#v", detail)
	}
	// A download that completes after settlement is still adopted safely.
	writeFixturePDF(t, f.jobFile("paper.pdf"))
	f.sweep(t)
	row, err := f.jobs.Get(context.Background(), f.jobID)
	if err != nil || row.State != job.StateReady {
		t.Fatalf("late download after settlement = %+v err=%v, want ready", row, err)
	}
}

// Only evidence settles an orphan. Each case removes exactly one piece of it.
func TestOrphanedDirectPermitWithoutProofIsNotTouched(t *testing.T) {
	for _, tc := range []struct {
		name              string
		reload, reconcile bool
		expire            bool
		prepare           func(t *testing.T, f orphanPermitFixture)
	}{
		{name: "held permit", reload: true, expire: true},
		{name: "same live generation and unexpired lease", reconcile: true},
		{name: "superseded generation and unexpired lease", reload: true, reconcile: true},
		{name: "download still being written", reload: true, reconcile: true, expire: true,
			prepare: func(t *testing.T, f orphanPermitFixture) {
				writeFixturePDF(t, f.jobFile("paper.pdf.crdownload"))
			}},
		{name: "download event since the permit", reload: true, reconcile: true, expire: true,
			prepare: func(t *testing.T, f orphanPermitFixture) {
				if err := f.jobs.RecordEvent(context.Background(), f.jobID, "browser.download_started",
					map[string]any{"download_id": 7, "filename": "paper.pdf"}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "two files since the permit", reload: true, reconcile: true, expire: true,
			prepare: func(t *testing.T, f orphanPermitFixture) {
				writeFixturePDF(t, f.jobFile("paper.pdf"))
				writeFixturePDF(t, f.jobFile("paper (1).pdf"))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrphanPermitFixture(t, "untouched", tc.reload, tc.reconcile)
			before, err := f.jobs.GetEffectPermit(context.Background(), f.permitID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.expire {
				f.expireLease(t)
			}
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			f.sweep(t)
			f.sweep(t)
			if got := orphanPermitStatus(t, f.jobs, f.permitID); got != before.Status {
				t.Fatalf("permit status = %q, want untouched %q", got, before.Status)
			}
			if live, err := f.jobs.LiveEffectPermit(context.Background()); err != nil || live == nil || live.ID != f.permitID {
				t.Fatalf("lane occupant = %+v err=%v, want %s", live, err, f.permitID)
			}
			if detail := orphanResolution(t, f.jobs, f.jobID); detail != nil {
				t.Fatalf("unproven orphan resolved: %#v", detail)
			}
		})
	}
}
