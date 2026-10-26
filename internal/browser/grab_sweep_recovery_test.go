// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/grab"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/triage"
	"papio/internal/work"
)

func TestSweepAdoptionsContinuesAfterDeferredAutoBindFailure(t *testing.T) {
	for _, placement := range []string{"same-root", "later-root"} {
		t.Run(placement, func(t *testing.T) {
			b, jobs, cfg, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(cfg *config.Config) {
				cfg.Browser.AdoptionRoot = filepath.Join(cfg.DataDir, "active-adoptions")
			})
			ctx := context.Background()
			badID := parkManualDownload(t, jobs, "wr_deferred_bad", deferredWork("10.1234/deferred.bad", "Deferred bad"))
			goodID := parkManualDownload(t, jobs, "wr_deferred_good", deferredWork("10.1234/deferred.good", "Deferred good"))
			// ReadDir sorts by ID. Make the failing item first in the same root.
			if badID > goodID {
				badID, goodID = goodID, badID
			}
			if _, err := jobs.S.DB().ExecContext(ctx, fmt.Sprintf(`
				CREATE TRIGGER fail_deferred_ingestion BEFORE INSERT ON candidates
				WHEN NEW.job_id = '%s'
				BEGIN SELECT RAISE(ABORT, 'persistent deferred ingestion failure'); END`, badID)); err != nil {
				t.Fatal(err)
			}
			bindGrab(t, b, badID, "Failing deferred capture", "")
			badDir := filepath.Join(cfg.EffectiveAdoptionRoot(), badID)
			writeFixturePDF(t, filepath.Join(badDir, "paper.pdf"))
			writeFixturePDF(t, filepath.Join(badDir, "unrelated.pdf"))
			recordDeferredAdoption(t, jobs, badID, "paper.pdf")
			goodRoot := cfg.EffectiveAdoptionRoot()
			if placement == "later-root" {
				goodRoot = cfg.LegacyAdoptionRoot()
				if goodRoot == "" {
					t.Fatal("fixture has no second adoption root")
				}
			}
			writeFixturePDF(t, filepath.Join(goodRoot, goodID, "paper.pdf"))
			if err := b.SweepAdoptions(ctx); err == nil || !strings.Contains(err.Error(), "persistent deferred ingestion failure") {
				t.Fatalf("sweep did not report the failed recovery: %v", err)
			}
			requireRecovered(t, jobs, goodID)
			requireNotAdopted(t, jobs, badID)
		})
	}
}

func TestSweepGrabsRecoversQuarantineWithoutLandingDirectory(t *testing.T) {
	for _, outcome := range []string{"park", "bind", "invalid"} {
		t.Run(outcome, func(t *testing.T) {
			disableAutoBindDecision(t)
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				excerpt := "a paper with no identifier"
				if outcome == "bind" {
					excerpt = "DOI: 10.1234/quarantine.recovered"
				}
				return pdf.ValidationReport{
					Payload:    pdf.PayloadReport{OK: outcome != "invalid"},
					Structural: pdf.StructuralReport{Valid: outcome != "invalid", Pages: 1},
					Text:       pdf.TextReport{Excerpt: excerpt},
					Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass},
				}, nil
			}
			g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Quarantine without landing")
			if err != nil {
				t.Fatal(err)
			}
			landing := filepath.Join(cfg.EffectiveAdoptionRoot(), grabsDirName, g.ID)
			writeFixturePDF(t, filepath.Join(landing, "paper.pdf"))
			quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
			writeFixturePDF(t, quarantine)
			if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(landing); err != nil {
				t.Fatal(err)
			}
			if err := b.SweepGrabs(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := b.grabs.Get(ctx, g.ID)
			if err != nil || after == nil {
				t.Fatalf("recovered grab = %+v, %v", after, err)
			}
			switch outcome {
			case "park":
				if after.State != grab.StateParkedNoIdentifier {
					t.Fatalf("state = %s, want parked", after.State)
				}
				if _, err := os.Stat(after.QuarantinePath); err != nil {
					t.Fatalf("park lost captured bytes: %v", err)
				}
			case "bind":
				if after.State != grab.StateJobCreated || after.JobID == "" {
					t.Fatalf("capture did not bind: %+v", after)
				}
				requireRecovered(t, jobs, after.JobID)
			case "invalid":
				if after.State != grab.StateFailedValidation {
					t.Fatalf("state = %s, want failed validation", after.State)
				}
			}
		})
	}
}

func TestDismissingGrabIsNotActionableOrVisibleOnWire(t *testing.T) {
	b, _, _, _ := newBridge(t)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Dismissed capture")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("cleanup interrupted")
	if err := b.grabs.DeleteParked(ctx, g.ID, func(string) error { return interrupted }); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.Items {
		if item.ID == triage.PdfGrabIDPrefix+g.ID {
			t.Fatal("dismissal claim remains actionable in the inbox")
		}
	}
	if result := b.IdentifyGrab(ctx, g.ID, "doi", "10.1234/dismissed.capture"); result.Outcome != "wrong_state" {
		t.Fatalf("dismissed capture remained identifiable: %+v", result)
	}
	runSync(t, b, hello())
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabStatusRequest, "", map[string]any{
		"request_id": "dismissed-status-0001", "grab_id": g.ID,
	}))
	frame := firstOfType(msgs, protocol.MsgPdfGrabStatusResult)
	if frame == nil {
		t.Fatal("missing status response")
	}
	status := frame.Payload.(*protocol.PdfGrabStatusResultPayload)
	if status.Outcome != "not_found" || status.State != "" {
		t.Fatalf("dismissal claim leaked into the wire state set: %+v", status)
	}
	if _, err := b.pdfGrabAbandon(ctx, &protocol.PdfGrabAbandonRequestPayload{
		RequestID: "dismissed-abandon-0001", GrabID: g.ID,
	}); err != nil {
		t.Fatalf("abandon emitted an invalid wire state: %v", err)
	}
}
