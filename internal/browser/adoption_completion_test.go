// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/work"
)

// The sweep can publish the artifact before download_complete arrives. Replays
// must bind to that accepted candidate, including when delivery_context is late.
func TestDownloadCompleteAfterSweepIsIdempotent(t *testing.T) {
	for _, contextFirst := range []bool{false, true} {
		name := "context_after"
		if contextFirst {
			name = "context_before"
		}
		t.Run(name, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t) // uses storetest.DataDir
			ctx := context.Background()
			runSync(t, b, hello())
			id := park(t, jobs, "swept-completion", handoffWork())
			writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
			if err := b.SweepAdoptions(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := jobs.Get(ctx, id)
			if err != nil || before.State != job.StateReady {
				t.Fatalf("sweep: row=%+v err=%v", before, err)
			}
			eventsBefore, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			// A replay must never invoke validation again.
			validate := b.svc.Validate
			validationCalls := 0
			b.svc.Validate = func(ctx context.Context, path, mime string, expected work.Work) (pdf.ValidationReport, error) {
				validationCalls++
				return validate(ctx, path, mime, expected)
			}
			for range 2 {
				key := browserDownloadKey{JobID: id, DownloadID: 7}
				provenance := inFrame(t, protocol.MsgDeliveryContext, id, map[string]any{
					"download_id": 7, "route": "resolver", "session_evidence": "warm", "page_host": "provider.example.edu",
				})
				if contextFirst {
					runSync(t, b, provenance)
				}
				msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, map[string]any{
					"download_id": 7, "filename": "paper.pdf", "size_bytes": 533,
				}))
				ack := firstOfType(msgs, protocol.MsgAck)
				if ack == nil || ack.JobID != id {
					t.Fatalf("completion ack = %+v", ack)
				}
				if !contextFirst {
					if pending := b.pendingDownloads[key]; pending.CandidateID != before.SelectedCandidateID || pending.Adopting {
						t.Errorf("completion did not recover the swept candidate: %+v", pending)
					}
					// An unrelated download's context cannot annotate the winner.
					runSync(t, b, inFrame(t, protocol.MsgDeliveryContext, id, map[string]any{
						"download_id": 8, "route": "direct", "session_evidence": "none",
					}))
					runSync(t, b, provenance)
				}
				if _, ok := b.pendingDownloads[key]; ok {
					t.Error("completed download remains pending after delivery context")
				}
				if _, ok := b.deliveryContexts[key]; ok {
					t.Error("paired delivery context remains pending")
				}
			}
			after, err := jobs.Get(ctx, id)
			if validationCalls != 0 {
				t.Errorf("duplicate validated %d times", validationCalls)
			}
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("duplicate changed job: before=%+v after=%+v err=%v", before, after, err)
			}
			candidate, err := jobs.GetCandidate(ctx, before.SelectedCandidateID)
			if err != nil || candidate.BrowserRoute != "resolver" || candidate.SessionEvidence != "warm" {
				t.Fatalf("candidate provenance = %+v err=%v", candidate, err)
			}
			eventsAfter, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(eventsAfter) < len(eventsBefore) || !reflect.DeepEqual(eventsBefore, eventsAfter[:len(eventsBefore)]) {
				t.Fatal("original events changed")
			}
			for _, event := range eventsAfter[len(eventsBefore):] {
				if event["kind"] == "browser.adoption_deferred" || event["kind"] == "job.transition" {
					t.Errorf("successful duplicate produced %+v", event)
				}
			}
		})
	}
}

func TestDownloadCompleteAfterSweepRefusesUnprovenArtifact(t *testing.T) {
	for _, mode := range []string{"different_bytes", "missing_download", "symlink", "missing_artifact", "wrong_candidate", "imported"} {
		t.Run(mode, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			runSync(t, b, hello())
			id := park(t, jobs, "unproven-completion", handoffWork())
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			writeFixturePDF(t, path)
			if err := b.SweepAdoptions(ctx); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil || row.State != job.StateReady {
				t.Fatalf("sweep: row=%+v err=%v", row, err)
			}
			artifactPath, err := b.svc.Artifacts.ArtifactPath(row.ArtifactSHA256)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "different_bytes":
				err = os.WriteFile(path, []byte("different download"), 0o600)
			case "missing_download":
				err = os.Remove(path)
			case "symlink":
				if err = os.Remove(path); err == nil {
					err = os.Symlink(artifactPath, path)
				}
			case "missing_artifact":
				err = os.Remove(artifactPath)
			case "wrong_candidate":
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE candidates SET url_key = 'browser-adopt:other' WHERE id = ?", row.SelectedCandidateID)
			case "imported":
				err = jobs.Transition(ctx, id, job.StateReady, job.StateImported, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := jobs.Get(ctx, id)
			runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, map[string]any{
				"download_id": 7, "filename": "paper.pdf", "size_bytes": 533,
			}), inFrame(t, protocol.MsgDeliveryContext, id, map[string]any{
				"download_id": 7, "route": "resolver", "session_evidence": "warm",
			}))
			if pending := b.pendingDownloads[browserDownloadKey{JobID: id, DownloadID: 7}]; pending.CandidateID != 0 {
				t.Fatalf("unproven download acquired a candidate: %+v", pending)
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused completion changed job: %+v err=%v", after, err)
			}
			candidate, err := jobs.GetCandidate(ctx, row.SelectedCandidateID)
			if err != nil || candidate.BrowserRoute != "" {
				t.Fatalf("unproven download applied provenance: %+v err=%v", candidate, err)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event["kind"] == "browser.adoption_deferred" {
					return
				}
			}
			t.Fatal("unproven completion silently accepted")
		})
	}
}

func TestDownloadCompleteReadyProducerRequiresPriorExactEvidence(t *testing.T) {
	for _, mode := range []string{"exact", "recovered", "uncorrelated", "mismatch", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			effectPermitHolder(t, b)
			id := park(t, jobs, "ready-producer", handoffWork())
			attempt := "ready-producer-attempt"
			effectPermitOffer(t, jobs, id, attempt, "ready-producer-domain")
			frames, err := b.providerDriveEpochStart(ctx, id, &protocol.ProviderDriveEpochStartRequestPayload{
				DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
			})
			if err != nil || permitOutcome(t, frames) != "started" {
				t.Fatalf("start: %v", err)
			}
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			writeFixturePDF(t, path)
			digest, err := fileDigest(path)
			if err != nil {
				t.Fatal(err)
			}
			ordinal := int64(0)
			producer := &job.ArtifactProducerIdentity{
				Kind: job.GenericDrive, DriveAttemptID: attempt, Ordinal: &ordinal, Strategy: "generic", Revision: "1",
			}
			if mode != "uncorrelated" {
				if err := b.persistArtifactCorrelation(ctx, id, "paper.pdf", digest, producer); err != nil {
					t.Fatal(err)
				}
			}
			other := *producer
			other.DriveAttemptID = "different-producer-attempt"
			if mode == "ambiguous" {
				if err := b.persistArtifactCorrelation(ctx, id, "paper.pdf", digest, &other); err != nil {
					t.Fatal(err)
				}
			}
			// Publish without bridge settlement: the crash/interleaving window
			// after validation commits but before the exact permit is released.
			candidateID, err := b.svc.AdoptDownloadCandidate(ctx, id, path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := jobs.Get(ctx, id)
			if err != nil || before.State != job.StateReady {
				t.Fatalf("adopt: %+v %v", before, err)
			}
			var evidenceBefore int
			if err := jobs.S.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE job_id = ? AND kind = 'browser.download_complete' AND json_extract(detail_json, '$.sha256') IS NOT NULL`, id).Scan(&evidenceBefore); err != nil {
				t.Fatal(err)
			}
			supplied := producer
			if mode == "mismatch" {
				supplied = &other
			}
			accepted := mode == "exact" || mode == "recovered"
			for range 2 {
				payload := map[string]any{"download_id": 7, "filename": "paper.pdf", "size_bytes": 533}
				if mode != "recovered" {
					payload["producer"] = supplied
				}
				msgs, _ := runSyncAs(t, b, "permit-test-holder", inFrame(t, protocol.MsgDownloadComplete, id, payload))
				if firstOfType(msgs, protocol.MsgAck) == nil {
					t.Fatal("completion was not acknowledged")
				}
				pending := b.pendingDownloads[browserDownloadKey{JobID: id, DownloadID: 7}]
				if accepted && pending.CandidateID != candidateID || !accepted && pending.CandidateID != 0 {
					t.Fatalf("producer %s candidate binding: %+v", mode, pending)
				}
			}
			permit, err := jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{
				JobID: id, Kind: job.GenericDrive, DriveAttemptID: attempt, Ordinal: 0, Strategy: "generic", Revision: "1",
			})
			wantStatus := job.EffectPermitHeld
			if accepted {
				wantStatus = job.EffectPermitSettled
			}
			if err != nil || permit == nil || permit.Status != wantStatus {
				t.Fatalf("permit: %+v err=%v want=%s", permit, err, wantStatus)
			}
			var evidenceAfter int
			if err := jobs.S.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE job_id = ? AND kind = 'browser.download_complete' AND json_extract(detail_json, '$.sha256') IS NOT NULL`, id).Scan(&evidenceAfter); err != nil {
				t.Fatal(err)
			}
			if evidenceAfter != evidenceBefore {
				t.Fatal("ready completion minted fresh producer authority")
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("producer replay changed job: %+v %v", after, err)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			deferred := false
			for _, event := range events {
				deferred = deferred || event["kind"] == "browser.adoption_deferred"
			}
			if deferred == accepted {
				t.Fatalf("deferred=%v accepted=%v", deferred, accepted)
			}
		})
	}
}

func TestDownloadCompleteRefusesUnsuccessfulTerminalJob(t *testing.T) {
	for _, state := range []string{job.StateCancelled, job.StateFailed, job.StateUnavailable} {
		t.Run(state, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			runSync(t, b, hello())
			id := park(t, jobs, "terminal-completion", handoffWork())
			if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, state, nil); err != nil {
				t.Fatal(err)
			}
			writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
			before, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, map[string]any{
				"download_id": 7, "filename": "paper.pdf", "size_bytes": 533,
			}))
			after, err := jobs.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal completion changed job: %+v %v", after, err)
			}
			if candidateID := b.pendingDownloads[browserDownloadKey{JobID: id, DownloadID: 7}].CandidateID; candidateID != 0 {
				t.Fatalf("terminal completion bound candidate %d", candidateID)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event["kind"] == "browser.adoption_deferred" {
					return
				}
			}
			t.Fatal("terminal completion silently accepted")
		})
	}
}
