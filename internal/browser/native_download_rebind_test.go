// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"papio/internal/acquisitionagent"
	"papio/internal/job"
	"papio/internal/protocol"
)

func nativeRebindAttempt(t *testing.T) (*Bridge, *job.Store, string, string, protocol.NativeDownloadArmRequestV1Payload) {
	t.Helper()
	b, jobs, id, source, p := nativeAttempt(t)
	b.SetAcquisitionBackend(agentBackendFunc(func(context.Context, acquisitionagent.Observation) (acquisitionagent.Decision, error) {
		t.Error("document rebind must not spend a model decision")
		return acquisitionagent.Decision{}, errors.New("unexpected decision")
	}))
	b.mu.Lock()
	b.arbitration.holderSession().Features = append(b.arbitration.holderSession().Features, agentFallbackFeature, protocol.AgentNavigationFeature)
	b.mu.Unlock()
	return b, jobs, id, source, p
}

func nativeRebindRequest(p protocol.NativeDownloadArmRequestV1Payload, arm *protocol.NativeDownloadArmResultV1Payload) protocol.NativeDownloadRebindRequestV1Payload {
	return protocol.NativeDownloadRebindRequestV1Payload{RequestID: "rebind-1", ReservationID: arm.ReservationID, Producer: p.Producer, BrowserEpoch: p.BrowserEpoch, DocumentID: p.DocumentID, NextDocumentID: "document-2"}
}

func nativeRebind(t *testing.T, b *Bridge, id string, p protocol.NativeDownloadRebindRequestV1Payload) *protocol.NativeDownloadRebindResultV1Payload {
	t.Helper()
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgNativeDownloadRebindRequestV1, id, p))
	r := firstOfType(msgs, protocol.MsgNativeDownloadRebindResultV1)
	if r == nil {
		t.Fatalf("rebind response missing: %+v", msgs)
	}
	return r.Payload.(*protocol.NativeDownloadRebindResultV1Payload)
}

func TestNativeDownloadRebindImportsOnlyNewDocument(t *testing.T) {
	b, jobs, id, source, p := nativeRebindAttempt(t)
	arm := nativeArm(t, b, id, p)
	if arm.Outcome != "armed" {
		t.Fatal(arm)
	}
	b.mu.Lock()
	original := b.nativeDownloads[id].record
	root := b.nativeDownloads[id].root
	b.mu.Unlock()
	request := nativeRebindRequest(p, arm)
	for range 2 {
		if got := nativeRebind(t, b, id, request); got.Outcome != "rebound" {
			t.Fatal(got)
		}
	}
	b.mu.Lock()
	r := b.nativeDownloads[id]
	preserved := r.record
	preserved.BindingSHA256 = original.BindingSHA256
	if r.root != root || r.busy || r.observation != "" || !reflect.DeepEqual(preserved, original) {
		b.mu.Unlock()
		t.Fatal("rebind replaced baseline, authority, timing or observation")
	}
	b.mu.Unlock()
	if nativeEventCount(t, jobs, id, "browser.native_download_reserved") != 1 || nativeEventCount(t, jobs, id, "browser.native_download_rebound") != 1 {
		t.Fatal("duplicate transfer created an event or a new reservation")
	}
	body := adoptionProbePDF(handoffWork().DOI)
	path := filepath.Join(source, "after-navigation.pdf")
	writeAdoptionProbeFile(t, path, body)
	observation := nativeObservation(p, arm, path, len(body))
	if got := nativeImport(t, b, id, observation); got.Outcome != "stale" {
		t.Fatalf("old document admitted: %+v", got)
	}
	observation.DocumentID = request.NextDocumentID
	if got := nativeImport(t, b, id, observation); got.Outcome != "ready" {
		t.Fatalf("new document import: %+v", got)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateReady || row.ArtifactSHA256 != digest {
		t.Fatalf("artifact=%+v err=%v", row, err)
	}
	if err := b.svc.Artifacts.Verify(digest); err != nil {
		t.Fatal(err)
	}
	producer, err := jobs.ArtifactProducerForArtifact(context.Background(), id, arm.ReservationID+".pdf", digest)
	if err != nil || producer == nil || !artifactProducersMatch(producer, artifactProducerIdentity(&p.Producer)) {
		t.Fatalf("producer=%+v err=%v", producer, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("source changed: %v", err)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_admitted") != 1 {
		t.Fatal("expected one exact admission")
	}
}

func TestNativeDownloadRebindRefusesLostOrConsumedAuthority(t *testing.T) {
	for _, condition := range []string{"observed", "busy", "root lost", "reservation lost", "generation", "attempt", "cancelled", "closed action", "expired", "wrong document", "wrong epoch", "wrong producer", "wrong reservation", "feature", "native feature", "agent feature", "missing backend", "closed backend", "session"} {
		t.Run(condition, func(t *testing.T) {
			b, jobs, id, _, p := nativeRebindAttempt(t)
			arm := nativeArm(t, b, id, p)
			if arm.Outcome != "armed" {
				t.Fatal(arm)
			}
			request := nativeRebindRequest(p, arm)
			want := "stale"
			b.mu.Lock()
			r := b.nativeDownloads[id]
			originalBinding := r.record.BindingSHA256
			switch condition {
			case "observed":
				r.observation, want = "consumed-observation", "refused"
			case "busy":
				r.busy, want = true, "refused"
			case "root lost":
				r.root.close()
				r.root, want = nil, "refused"
			case "reservation lost":
				r.root.close()
				delete(b.nativeDownloads, id)
			case "expired":
				r.record.ExpiresAtMS = b.now().UnixMilli() - 1
			case "feature":
				h := b.arbitration.holderSession()
				h.Features = slices.DeleteFunc(h.Features, func(s string) bool { return s == protocol.AgentNavigationFeature })
				want = "unavailable"
			case "native feature":
				h := b.arbitration.holderSession()
				h.Features = slices.DeleteFunc(h.Features, func(s string) bool { return s == protocol.NativeClickAdoptionFeature })
			case "agent feature":
				h := b.arbitration.holderSession()
				h.Features = slices.DeleteFunc(h.Features, func(s string) bool { return s == agentFallbackFeature })
				want = "unavailable"
			case "missing backend":
				b.agentBackend, want = nil, "unavailable"
			case "closed backend":
				b.agentClosed = true
			case "session":
				r.sessionID = "other-session"
			}
			b.mu.Unlock()
			var err error
			switch condition {
			case "generation":
				_, err = jobs.NextMaterializationHolderGeneration(context.Background())
			case "attempt":
				err = jobs.RecordEvent(context.Background(), id, "job.retry_requested", nil)
			case "cancelled":
				_, err = jobs.S.DB().Exec(`UPDATE jobs SET state='cancelled' WHERE id=?`, id)
			case "closed action":
				_, err = jobs.S.DB().Exec(`UPDATE human_actions SET status='resolved' WHERE job_id=?`, id)
			case "wrong document":
				request.DocumentID = "different-document"
			case "wrong epoch":
				request.BrowserEpoch = "different-epoch"
			case "wrong producer":
				request.Producer.DriveAttemptID = "different-drive"
			case "wrong reservation":
				request.ReservationID = "different-reservation"
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := nativeRebind(t, b, id, request); got.Outcome != want {
				t.Fatalf("rebind=%+v want=%s", got, want)
			}
			b.mu.Lock()
			if r.record.BindingSHA256 != originalBinding {
				b.mu.Unlock()
				t.Fatal("refusal changed binding")
			}
			// Restore test-only busy flag so normal fixture cleanup owns its root.
			r.busy = false
			b.mu.Unlock()
			if nativeEventCount(t, jobs, id, "browser.native_download_rebound") != 0 {
				t.Fatal("refused transfer recorded")
			}
		})
	}
}

func TestNativeDownloadRebindLosesNoBaselineExclusions(t *testing.T) {
	b, _, id, source, p := nativeRebindAttempt(t)
	body := adoptionProbePDF(handoffWork().DOI)
	path := filepath.Join(source, "preexisting.pdf")
	writeAdoptionProbeFile(t, path, body)
	arm := nativeArm(t, b, id, p)
	if arm.Outcome != "armed" {
		t.Fatal(arm)
	}
	request := nativeRebindRequest(p, arm)
	if got := nativeRebind(t, b, id, request); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	p.DocumentID = request.NextDocumentID
	if got := nativeImport(t, b, id, nativeObservation(p, arm, path, len(body))); got.Outcome != "refused" {
		t.Fatalf("rebind forgot original baseline exclusion: %+v", got)
	}
}

func TestNativeDownloadRebindFollowsOnlyCurrentDocument(t *testing.T) {
	b, jobs, id, _, p := nativeRebindAttempt(t)
	arm := nativeArm(t, b, id, p)
	first := nativeRebindRequest(p, arm)
	if got := nativeRebind(t, b, id, first); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	// Neither replaying the original arm nor changing its document is a new
	// authority grant. The original request remains immutable after transfer.
	if got := nativeArm(t, b, id, p); got.Outcome != "refused" {
		t.Fatalf("original arm replayed: %+v", got)
	}
	p.DocumentID = first.NextDocumentID
	if got := nativeArm(t, b, id, p); got.Outcome != "refused" {
		t.Fatalf("changed arm treated as exact retry: %+v", got)
	}
	second := first
	second.RequestID, second.DocumentID, second.NextDocumentID = "rebind-2", first.NextDocumentID, "document-3"
	if got := nativeRebind(t, b, id, second); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	if got := nativeRebind(t, b, id, first); got.Outcome != "stale" {
		t.Fatalf("old cached transfer resurrected retired document: %+v", got)
	}
	if got := nativeRebind(t, b, id, second); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_rebound") != 2 || nativeEventCount(t, jobs, id, "browser.native_download_reserved") != 1 {
		t.Fatal("transfer chain added duplicate events")
	}
	// The cached path must still consult durable holder authority even when
	// the bridge's generation snapshot has not yet observed its replacement.
	if _, err := jobs.NextMaterializationHolderGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := nativeRebind(t, b, id, second); got.Outcome != "stale" {
		t.Fatalf("cached transfer bypassed durable generation: %+v", got)
	}
}

func TestNativeDownloadRebindRestartCannotRearm(t *testing.T) {
	b, jobs, id, source, p := nativeRebindAttempt(t)
	arm := nativeArm(t, b, id, p)
	request := nativeRebindRequest(p, arm)
	if got := nativeRebind(t, b, id, request); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	// A restored reservation receipt does not restore the private baseline.
	b.mu.Lock()
	r := b.nativeDownloads[id]
	delete(b.nativeDownloads, id)
	b.mu.Unlock()
	r.root.close()
	if got := nativeRebind(t, b, id, request); got.Outcome != "stale" {
		t.Fatal(got)
	}
	p.DocumentID = request.NextDocumentID
	p.RequestID = "arm-after-restart"
	if got := nativeArm(t, b, id, p); got.Outcome != "refused" || got.Reason != "already_armed" {
		t.Fatalf("restart replay: %+v", got)
	}
	body := adoptionProbePDF(handoffWork().DOI)
	path := filepath.Join(source, "post-restart.pdf")
	writeAdoptionProbeFile(t, path, body)
	if got := nativeImport(t, b, id, nativeObservation(p, arm, path, len(body))); got.Outcome != "stale" {
		t.Fatal(got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_reserved") != 1 || nativeEventCount(t, jobs, id, "browser.native_download_admitted") != 0 {
		t.Fatal("restart changed one-shot latch")
	}
}

func TestNativeDownloadRebindPublicationFailureDoesNotChangeBinding(t *testing.T) {
	b, jobs, id, _, p := nativeRebindAttempt(t)
	arm := nativeArm(t, b, id, p)
	request := nativeRebindRequest(p, arm)
	if _, err := jobs.S.DB().Exec(`CREATE TRIGGER reject_rebind BEFORE INSERT ON events WHEN NEW.kind='browser.native_download_rebound' BEGIN SELECT RAISE(ABORT,'test rebind rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if got := nativeRebind(t, b, id, request); got.Outcome != "unavailable" {
		t.Fatal(got)
	}
	if b.nativeDownloads[id].record.BindingSHA256 != nativeBinding(testSessionID, id, &p) {
		t.Fatal("failed durable transfer changed private binding")
	}
	if _, err := jobs.S.DB().Exec(`DROP TRIGGER reject_rebind`); err != nil {
		t.Fatal(err)
	}
	if got := nativeRebind(t, b, id, request); got.Outcome != "rebound" {
		t.Fatal(got)
	}
}

func TestNativeDownloadRebindDoesNotExtendExpiry(t *testing.T) {
	b, jobs, id, _, p := nativeRebindAttempt(t)
	arm := nativeArm(t, b, id, p)
	request := nativeRebindRequest(p, arm)
	if got := nativeRebind(t, b, id, request); got.Outcome != "rebound" {
		t.Fatal(got)
	}
	b.mu.Lock()
	r := b.nativeDownloads[id]
	if r.record.ExpiresAtMS != arm.ExpiresAtMS {
		b.mu.Unlock()
		t.Fatal("expiry changed")
	}
	now := time.UnixMilli(arm.ExpiresAtMS)
	b.now = func() time.Time { return now }
	b.mu.Unlock()
	if got := nativeRebind(t, b, id, request); got.Outcome != "stale" {
		t.Fatalf("cached rebind outlived expiry: %+v", got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_rebound") != 1 {
		t.Fatal("expired request added transfer")
	}
	if _, err := os.Stat(filepath.Join(b.cfg.EffectiveAdoptionRoot(), id, arm.ReservationID+".pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rebind published a file: %v", err)
	}
}
