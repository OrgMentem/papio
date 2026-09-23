// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/work"
)

// Exercise the real adoption and sanitization publication path with deterministic
// validator/rewriter seams. The source remains in the confined download folder;
// only the rewritten bytes enter the artifact store.
func enableAdoptionSanitization(t *testing.T, b *Bridge) {
	t.Helper()
	validate := b.svc.Validate
	b.svc.Validate = func(ctx context.Context, path, mime string, expected work.Work) (pdf.ValidationReport, error) {
		report, err := validate(ctx, path, mime, expected)
		report.Structural.HasEmbeddedFiles = !strings.HasSuffix(path, ".sanitized")
		return report, err
	}
	b.svc.Sanitize = func(_ context.Context, _, dest string) (pdf.StructuralReport, error) {
		body := append(adoptionProbePDF(handoffWork().DOI), []byte("\n% sanitized fixture\n")...)
		return pdf.StructuralReport{Valid: true, Pages: 3}, os.WriteFile(dest, body, 0o600)
	}
}

func TestDownloadCompleteAfterSanitizedSweep(t *testing.T) {
	for _, mode := range []string{"context_after", "context_before", "restart"} {
		t.Run(mode, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t) // uses storetest.DataDir
			ctx := context.Background()
			runSync(t, b, hello())
			id := park(t, jobs, "sanitized-swept-completion", handoffWork())
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			writeFixturePDF(t, path)
			sourceSHA, err := fileDigest(path)
			if err != nil {
				t.Fatal(err)
			}
			enableAdoptionSanitization(t, b)
			if err := b.SweepAdoptions(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := jobs.Get(ctx, id)
			if err != nil || before.State != job.StateReady || before.ArtifactSHA256 == sourceSHA {
				t.Fatalf("sanitized sweep: row=%+v source=%s err=%v", before, sourceSHA, err)
			}
			if err := b.svc.Artifacts.Verify(before.ArtifactSHA256); err != nil {
				t.Fatal(err)
			}
			if mode == "restart" {
				b = NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, cfg, b.Version)
				runSync(t, b, hello())
			}
			b.svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
				t.Error("late completion revalidated a published artifact")
				return pdf.ValidationReport{}, nil
			}
			key := browserDownloadKey{JobID: id, DownloadID: 7}
			provenance := inFrame(t, protocol.MsgDeliveryContext, id, map[string]any{
				"download_id": 7, "route": "direct", "session_evidence": "fresh_auth", "page_host": "provider.example.edu",
			})
			for range 2 {
				if mode == "context_before" {
					runSync(t, b, provenance)
				}
				msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, map[string]any{
					"download_id": 7, "filename": "paper.pdf", "size_bytes": 533,
				}))
				if firstOfType(msgs, protocol.MsgAck) == nil {
					t.Fatal("completion was not acknowledged")
				}
				if mode != "context_before" {
					if got := b.pendingDownloads[key].CandidateID; got != before.SelectedCandidateID {
						t.Errorf("recovered candidate=%d, want accepted candidate %d", got, before.SelectedCandidateID)
					}
					runSync(t, b, provenance)
				}
				if _, ok := b.pendingDownloads[key]; ok {
					t.Error("reconciled download remains pending")
				}
				if _, ok := b.deliveryContexts[key]; ok {
					t.Error("paired delivery context remains pending")
				}
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("late completion changed ready job: before=%+v after=%+v err=%v", before, after, err)
			}
			candidate, err := jobs.GetCandidate(ctx, before.SelectedCandidateID)
			if err != nil || candidate.URLKey != "browser-adopt:sha256:"+sourceSHA || candidate.BrowserRoute != "direct" || candidate.SessionEvidence != "fresh_auth" {
				t.Fatalf("accepted source candidate/provenance: %+v err=%v", candidate, err)
			}
			producer, err := jobs.ArtifactProducerForArtifact(ctx, id, "paper.pdf", sourceSHA)
			if err != nil || producer != nil {
				t.Fatalf("late completion invented source producer: %+v err=%v", producer, err)
			}
			events, err := jobs.Events(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event["kind"] == "browser.adoption_deferred" {
					t.Errorf("verified sanitized completion reported as deferred: %+v", event)
				}
			}
		})
	}
}

func TestDownloadCompleteAfterSanitizedSweepRefusesUnprovenLineage(t *testing.T) {
	for _, mode := range []string{
		"unrelated_bytes", "missing_lineage", "wrong_source_hash", "wrong_artifact_hash",
		"wrong_event_candidate", "other_job_lineage", "wrong_candidate_key", "wrong_candidate_source",
		"unaccepted_candidate", "corrupt_artifact", "missing_artifact", "symlink",
	} {
		t.Run(mode, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			runSync(t, b, hello())
			id := park(t, jobs, "unproven-sanitized-completion", handoffWork())
			path := filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf")
			writeFixturePDF(t, path)
			enableAdoptionSanitization(t, b)
			if err := b.SweepAdoptions(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := jobs.Get(ctx, id)
			if err != nil || before.State != job.StateReady {
				t.Fatalf("sanitized sweep: %+v err=%v", before, err)
			}
			artifactPath, err := b.svc.Artifacts.ArtifactPath(before.ArtifactSHA256)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unrelated_bytes":
				err = os.WriteFile(path, []byte("unrelated download"), 0o600)
			case "missing_lineage":
				_, err = jobs.S.DB().ExecContext(ctx, "DELETE FROM events WHERE job_id=? AND kind='job.pdf_sanitized'", id)
			case "wrong_source_hash", "wrong_artifact_hash", "wrong_event_candidate":
				field, value := "$.source_sha256", any(strings.Repeat("0", 64))
				switch mode {
				case "wrong_artifact_hash":
					field = "$.adopted_sha256"
				case "wrong_event_candidate":
					field, value = "$.candidate_id", before.SelectedCandidateID+1
				}
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE events SET detail_json=json_set(detail_json,?,?) WHERE job_id=? AND kind='job.pdf_sanitized'", field, value, id)
			case "other_job_lineage":
				other := park(t, jobs, "other-sanitized-job", handoffWork())
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE events SET job_id=? WHERE job_id=? AND kind='job.pdf_sanitized'", other, id)
			case "wrong_candidate_key":
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE candidates SET url_key=? WHERE id=?", "browser-adopt:sha256:"+strings.Repeat("0", 64), before.SelectedCandidateID)
			case "wrong_candidate_source":
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE candidates SET source='unpaywall' WHERE id=?", before.SelectedCandidateID)
			case "unaccepted_candidate":
				_, err = jobs.S.DB().ExecContext(ctx, "UPDATE candidates SET status='invalid' WHERE id=?", before.SelectedCandidateID)
			case "corrupt_artifact":
				if err = os.Chmod(artifactPath, 0o600); err == nil {
					err = os.WriteFile(artifactPath, []byte("corrupt stored artifact"), 0o600)
				}
			case "missing_artifact":
				err = os.Remove(artifactPath)
			case "symlink":
				source := filepath.Join(t.TempDir(), "outside.pdf")
				writeFixturePDF(t, source)
				if err = os.Remove(path); err == nil {
					adoptionTestSymlink(t, source, path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id, map[string]any{
				"download_id": 7, "filename": "paper.pdf", "size_bytes": 533,
			}), inFrame(t, protocol.MsgDeliveryContext, id, map[string]any{
				"download_id": 7, "route": "direct", "session_evidence": "fresh_auth",
			}))
			if got := b.pendingDownloads[browserDownloadKey{JobID: id, DownloadID: 7}].CandidateID; got != 0 {
				t.Errorf("unproven source bound candidate %d", got)
			}
			after, err := jobs.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unproven completion changed ready job: %+v err=%v", after, err)
			}
			candidate, err := jobs.GetCandidate(ctx, before.SelectedCandidateID)
			if err != nil || candidate.BrowserRoute != "" || candidate.SessionEvidence != "" {
				t.Fatalf("unproven completion attached provenance: %+v err=%v", candidate, err)
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
			t.Fatal("unproven completion was silently accepted")
		})
	}
}
