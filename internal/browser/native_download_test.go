// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

func nativeAttempt(t *testing.T) (*Bridge, *job.Store, string, string, protocol.NativeDownloadArmRequestV1Payload) {
	t.Helper()
	source := t.TempDir()
	landing := filepath.Join(source, "papio")
	if err := os.Mkdir(landing, 0700); err != nil {
		t.Fatal(err)
	}
	b, jobs, _, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) { c.Browser.AdoptionRoot = landing })
	b.svc.Validate = adoptionProbeValidate
	t.Cleanup(b.CloseAcquisitionBackend)
	runSync(t, b, helloWithFeatures(t, "1.2.3", providerDriveEpochV1Feature, effectPermitFeature, protocol.NativeClickAdoptionFeature))
	id := park(t, jobs, "native-attempt", handoffWork())
	effectPermitOffer(t, jobs, id, "native-drive", "native-domain")
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgProviderDriveEpochStartRequest, id, protocol.ProviderDriveEpochStartRequestPayload{RequestID: "native-start", DriveAttemptID: "native-drive", Strategy: "generic", Revision: "1"}))
	start := firstOfType(msgs, protocol.MsgProviderDriveEpochStartResult)
	if start == nil || start.Payload.(*protocol.ProviderDriveEpochStartResultPayload).Outcome != "started" {
		t.Fatalf("start=%+v", msgs)
	}
	ordinal := int64(0)
	p := protocol.NativeDownloadArmRequestV1Payload{RequestID: "native-arm-1", Producer: protocol.ArtifactProducerPayload{EffectKind: "generic_drive", DriveAttemptID: "native-drive", Ordinal: &ordinal, Strategy: "generic", Revision: "1"}, BrowserEpoch: "browser-epoch", DocumentID: "document-1"}
	return b, jobs, id, source, p
}
func nativeArm(t *testing.T, b *Bridge, id string, p protocol.NativeDownloadArmRequestV1Payload) *protocol.NativeDownloadArmResultV1Payload {
	t.Helper()
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgNativeDownloadArmRequestV1, id, p))
	r := firstOfType(msgs, protocol.MsgNativeDownloadArmResultV1)
	if r == nil {
		t.Fatalf("arm response missing: %+v", msgs)
	}
	return r.Payload.(*protocol.NativeDownloadArmResultV1Payload)
}
func nativeImport(t *testing.T, b *Bridge, id string, p protocol.NativeDownloadImportRequestV1Payload) *protocol.NativeDownloadImportResultV1Payload {
	t.Helper()
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgNativeDownloadImportRequestV1, id, p))
	r := firstOfType(msgs, protocol.MsgNativeDownloadImportResultV1)
	if r == nil {
		t.Fatalf("import response missing: %+v", msgs)
	}
	return r.Payload.(*protocol.NativeDownloadImportResultV1Payload)
}
func nativeObservation(p protocol.NativeDownloadArmRequestV1Payload, r *protocol.NativeDownloadArmResultV1Payload, path string, size int) protocol.NativeDownloadImportRequestV1Payload {
	return protocol.NativeDownloadImportRequestV1Payload{RequestID: "native-import-1", ReservationID: r.ReservationID, Producer: p.Producer, BrowserEpoch: p.BrowserEpoch, DocumentID: p.DocumentID, DownloadID: 27, StartedAtMS: time.Now().UnixMilli(), SourcePath: path, SizeBytes: int64(size)}
}
func nativeEventCount(t *testing.T, jobs *job.Store, id, kind string) int {
	t.Helper()
	var n int
	if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, id, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNativeDownloadSyncFreshValidatedArtifact(t *testing.T) {
	b, jobs, id, source, p := nativeAttempt(t)
	arm := nativeArm(t, b, id, p)
	if arm.Outcome != "armed" {
		t.Fatalf("arm=%+v", arm)
	}
	body := adoptionProbePDF(handoffWork().DOI)
	path := filepath.Join(source, "Publisher article (27) ü.pdf")
	writeAdoptionProbeFile(t, path, body)
	observation := nativeObservation(p, arm, path, len(body))
	result := nativeImport(t, b, id, observation)
	if result.Outcome != "ready" {
		row, _ := jobs.Get(context.Background(), id)
		t.Fatalf("import=%+v state=%+v", result, row)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateReady || row.ArtifactSHA256 != digest {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	candidate, err := jobs.GetCandidate(context.Background(), row.SelectedCandidateID)
	if err != nil || candidate.Source != "browser" || candidate.Status != job.CandidateAccepted || candidate.URLKey != "browser-adopt:sha256:"+digest {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}
	if err := b.svc.Artifacts.Verify(digest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("original changed: %v", err)
	}
	producer, err := jobs.ArtifactProducerForArtifact(context.Background(), id, arm.ReservationID+".pdf", digest)
	if err != nil || producer == nil || !artifactProducersMatch(producer, artifactProducerIdentity(&p.Producer)) {
		t.Fatalf("producer=%+v err=%v", producer, err)
	}
	permit, err := jobs.GetEffectPermitByIdentity(context.Background(), job.EffectPermitIdentity{JobID: id, Kind: job.GenericDrive, DriveAttemptID: p.Producer.DriveAttemptID, Ordinal: 0, Strategy: "generic", Revision: "1"})
	if err != nil || permit.Status == job.Held {
		t.Fatalf("exact permit not settled: %+v %v", permit, err)
	}
	observation.RequestID = "native-import-2"
	if got := nativeImport(t, b, id, observation); got.Outcome != "ready" {
		t.Fatalf("receipt=%+v", got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_admitted") != 1 {
		t.Fatal("duplicate admitted")
	}
	observation.SourcePath = filepath.Join(source, "another.pdf")
	if got := nativeImport(t, b, id, observation); got.Outcome != "stale" {
		t.Fatalf("changed observation=%+v", got)
	}
}

func TestNativeDownloadValidationIsNotExistence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"wrong work", adoptionProbePDF("10.1234/different.paper"), job.StateNeedsReview},
		{"HTML", []byte("<html>Sign in</html>"), job.StateAwaitingHuman},
		{"malformed", []byte("%PDF-1.4\n" + strings.Repeat("not PDF\n", 700)), job.StateAwaitingHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, id, source, p := nativeAttempt(t)
			arm := nativeArm(t, b, id, p)
			if arm.Outcome != "armed" {
				t.Fatal(arm)
			}
			path := filepath.Join(source, "file.pdf")
			writeAdoptionProbeFile(t, path, tc.body)
			result := nativeImport(t, b, id, nativeObservation(p, arm, path, len(tc.body)))
			row, err := jobs.Get(context.Background(), id)
			if result.Outcome == "ready" || err != nil || row.State != tc.want || row.ArtifactSHA256 != "" {
				t.Fatalf("result=%+v row=%+v err=%v", result, row, err)
			}
		})
	}
}
func TestNativeDownloadLostBaselineCannotRearm(t *testing.T) {
	b, jobs, id, source, p := nativeAttempt(t)
	arm := nativeArm(t, b, id, p)
	if arm.Outcome != "armed" {
		t.Fatal(arm)
	}
	b.mu.Lock()
	r := b.nativeDownloads[id]
	delete(b.nativeDownloads, id)
	b.mu.Unlock()
	r.root.close()
	body := adoptionProbePDF(handoffWork().DOI)
	path := filepath.Join(source, "file.pdf")
	writeAdoptionProbeFile(t, path, body)
	if got := nativeImport(t, b, id, nativeObservation(p, arm, path, len(body))); got.Outcome != "stale" {
		t.Fatal(got)
	}
	p.RequestID = "native-arm-2"
	if got := nativeArm(t, b, id, p); got.Outcome != "refused" || got.Reason != "already_armed" {
		t.Fatal(got)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_admitted") != 0 {
		t.Fatal("lost baseline admitted")
	}
}
func TestNativeDownloadImportFencesAuthority(t *testing.T) {
	for _, condition := range []string{"document", "epoch", "producer", "generation", "attempt", "cancelled", "closed action", "expired", "feature"} {
		t.Run(condition, func(t *testing.T) {
			b, jobs, id, source, p := nativeAttempt(t)
			arm := nativeArm(t, b, id, p)
			if arm.Outcome != "armed" {
				t.Fatal(arm)
			}
			body := adoptionProbePDF(handoffWork().DOI)
			path := filepath.Join(source, "file.pdf")
			writeAdoptionProbeFile(t, path, body)
			obs := nativeObservation(p, arm, path, len(body))
			switch condition {
			case "document":
				obs.DocumentID = "another-document"
			case "epoch":
				obs.BrowserEpoch = "another-epoch"
			case "producer":
				obs.Producer.DriveAttemptID = "another-drive"
			case "generation":
				if _, err := jobs.NextMaterializationHolderGeneration(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "attempt":
				if err := jobs.RecordEvent(context.Background(), id, "job.retry_requested", nil); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if _, err := jobs.S.DB().Exec(`UPDATE jobs SET state='cancelled' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "closed action":
				if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET status='resolved' WHERE job_id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "expired":
				b.mu.Lock()
				b.nativeDownloads[id].record.ExpiresAtMS = b.now().UnixMilli() - 1
				b.mu.Unlock()
			case "feature":
				b.mu.Lock()
				b.arbitration.holderSession().Features = nil
				b.mu.Unlock()
			}
			got := nativeImport(t, b, id, obs)
			if got.Outcome != "stale" {
				t.Fatalf("result=%+v", got)
			}
			if nativeEventCount(t, jobs, id, "browser.native_download_admitted") != 0 {
				t.Fatal("stale source admitted")
			}
			if _, err := os.Stat(filepath.Join(b.cfg.EffectiveAdoptionRoot(), id, arm.ReservationID+".pdf")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale file published: %v", err)
			}
		})
	}
}
func TestNativeDownloadHelloCapAndLegacy(t *testing.T) {
	b, _, _, _ := newBridge(t)
	legacy, err := b.helloAck(sessionRoleHolder, SessionRolesMinExtensionVersion, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeBrowserMessage(legacy)
	if err != nil {
		t.Fatal(err)
	}
	old := decoded.Payload.(*protocol.HelloAckPayload).Features
	if !slices.Equal(old, b.Features) || slices.Contains(old, protocol.NativeClickAdoptionFeature) {
		t.Fatal("legacy capabilities changed")
	}
	frame, err := b.helloAck(sessionRoleHolder, SessionRolesMinExtensionVersion, []string{protocol.NativeClickAdoptionFeature, nativeViewerDownloadFeature, agentFallbackFeature})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = protocol.DecodeBrowserMessage(frame)
	if err != nil {
		t.Fatal(err)
	}
	features := decoded.Payload.(*protocol.HelloAckPayload).Features
	if len(features) > 32 || !slices.Contains(features, protocol.NativeClickAdoptionFeature) || !slices.Contains(features, triageSnapshotSchema5Feature) {
		t.Fatalf("features=%v", features)
	}
}
func TestNativeDownloadBoundedIOSingleFlightAndLateCleanup(t *testing.T) {
	gate := make(chan struct{}, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	cleaned := make(chan struct{})
	var calls atomic.Int32
	_, err := boundedNativeIO(context.Background(), gate, 20*time.Millisecond, func(context.Context) (int, error) { calls.Add(1); close(entered); <-release; return 7, nil }, func(value int) {
		if value != 7 {
			t.Errorf("late value=%d", value)
		}
		close(cleaned)
	})
	<-entered
	if !errors.Is(err, ErrAdoptionScanTimeout) {
		t.Fatalf("timeout=%v", err)
	}
	_, err = boundedNativeIO(context.Background(), gate, time.Second, func(context.Context) (int, error) { calls.Add(1); return 8, nil }, func(int) {})
	if !errors.Is(err, errNativeIOBusy) || calls.Load() != 1 {
		t.Fatalf("latched=%v calls=%d", err, calls.Load())
	}
	close(release)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("late handles leaked")
	}
}

// Admission consumes authority before publication. A restart can recover a
// published artifact using the exact durable producer, but it must never infer
// completion from a staging file or reconstruct the lost source baseline.
func TestNativeDownloadCrashBoundary(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "admitted only"
		if published {
			name = "published"
		}
		t.Run(name, func(t *testing.T) {
			b, jobs, id, source, p := nativeAttempt(t)
			arm := nativeArm(t, b, id, p)
			if arm.Outcome != "armed" {
				t.Fatal(arm)
			}
			body := adoptionProbePDF(handoffWork().DOI)
			path := filepath.Join(source, "fresh.pdf")
			writeAdoptionProbeFile(t, path, body)
			obs := nativeObservation(p, arm, path, len(body))
			b.mu.Lock()
			r := b.nativeDownloads[id]
			delete(b.nativeDownloads, id)
			b.mu.Unlock()
			staged, err := r.root.stage(context.Background(), path, r.record.ReservationID, int64(len(body)), b.cfg.Fetch.MaxBytes)
			if err != nil {
				t.Fatal(err)
			}
			a := job.NativeDownloadAdmission{NativeDownloadReservation: r.record, DownloadID: obs.DownloadID, StartedAtMS: obs.StartedAtMS, ObservationSHA256: nativeDigest(obs), Filename: r.record.ReservationID + ".pdf", SHA256: staged.digest, SizeBytes: staged.size}
			if err := jobs.AdmitNativeDownload(context.Background(), a, b.now()); err != nil {
				t.Fatal(err)
			}
			if published {
				if err := r.root.publish(context.Background(), id, a.Filename, staged); err != nil {
					t.Fatal(err)
				}
			}
			r.root.close() // emulate lost handles without deleting crash-left staging
			restarted := NewBridge(jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, b.cfg, b.Version)
			t.Cleanup(restarted.CloseAcquisitionBackend)
			if err := restarted.SweepAdoptions(context.Background()); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if published {
				if row.State != job.StateReady || row.ArtifactSHA256 != staged.digest {
					t.Fatalf("published bytes not recovered: %+v", row)
				}
				permit, err := jobs.GetEffectPermit(context.Background(), r.record.PermitID)
				if err != nil || permit.Status == job.Held {
					t.Fatalf("producer not recovered: %+v %v", permit, err)
				}
			} else {
				if row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" {
					t.Fatalf("unswept staging became success: %+v", row)
				}
				if _, err := os.Stat(filepath.Join(source, "papio", staged.name)); err != nil {
					t.Fatalf("expected stranded crash stage: %v", err)
				}
				if result := nativeImport(t, b, id, obs); result.Outcome != "stale" {
					t.Fatalf("lost reservation result=%+v", result)
				}
				p.RequestID = "native-rearm-after-crash"
				if result := nativeArm(t, b, id, p); result.Outcome != "refused" || result.Reason != "already_armed" {
					t.Fatalf("crash allowed replay: %+v", result)
				}
			}
		})
	}
}

// Successes share one bridge, holder and clock window. Each must leave room for
// new work without discarding duplicate receipts before capacity is needed.
func TestNativeDownloadTerminalReceiptCapacity(t *testing.T) {
	b, jobs, id, source, armRequest := nativeAttempt(t)
	generation := b.arbitration.generation()
	startedAt := b.now()
	type receipt struct {
		jobID   string
		request protocol.NativeDownloadImportRequestV1Payload
	}
	var receipts []receipt
	for i := 0; i < maxOutstandingOffers+1; i++ {
		if i > 0 {
			id, armRequest = nativeCapacityAttempt(t, b, jobs, i)
		}
		// Merely having reached capacity must not invalidate existing receipts.
		for _, previous := range receipts {
			if got := nativeImport(t, b, previous.jobID, previous.request); got.Outcome != "ready" {
				t.Fatalf("cached receipt before pressure: %+v", got)
			}
		}
		arm := nativeArm(t, b, id, armRequest)
		if arm.Outcome != "armed" {
			t.Fatalf("sequential acquisition %d: %+v", i+1, arm)
		}
		row, err := jobs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		body := adoptionProbePDF(row.Work.DOI)
		path := filepath.Join(source, fmt.Sprintf("sequential-%d.pdf", i))
		writeAdoptionProbeFile(t, path, body)
		observation := nativeObservation(armRequest, arm, path, len(body))
		observation.DownloadID += int64(i)
		if result := nativeImport(t, b, id, observation); result.Outcome != "ready" {
			t.Fatalf("sequential import %d: %+v", i+1, result)
		}
		receipts = append(receipts, receipt{id, observation})
		b.mu.Lock()
		count := len(b.nativeDownloads)
		b.mu.Unlock()
		if count != min(i+1, maxOutstandingOffers) {
			t.Fatalf("receipt cache size=%d after %d imports", count, i+1)
		}
	}
	if b.arbitration.generation() != generation || b.now().Sub(startedAt) >= agentAttemptDuration {
		t.Fatal("test escaped the same-holder ten-minute window")
	}
	evicted := 0
	for _, previous := range receipts {
		b.mu.Lock()
		_, cached := b.nativeDownloads[previous.jobID]
		b.mu.Unlock()
		want := "ready"
		if !cached {
			evicted++
			want = "stale"
		}
		if result := nativeImport(t, b, previous.jobID, previous.request); result.Outcome != want {
			t.Fatalf("receipt cached=%v: %+v", cached, result)
		}
		if nativeEventCount(t, jobs, previous.jobID, "browser.native_download_reserved") != 1 || nativeEventCount(t, jobs, previous.jobID, "browser.native_download_admitted") != 1 {
			t.Fatal("receipt eviction changed durable one-shot evidence")
		}
	}
	if evicted != 1 {
		t.Fatalf("evicted=%d, want exactly the one slot needed", evicted)
	}
}

func nativeCapacityAttempt(t *testing.T, b *Bridge, jobs *job.Store, index int) (string, protocol.NativeDownloadArmRequestV1Payload) {
	t.Helper()
	name := fmt.Sprintf("native-capacity-%d", index)
	target := handoffWork()
	target.DOI = "10.1002/" + name
	id := park(t, jobs, name, target)
	effectPermitOffer(t, jobs, id, name, "native-domain")
	messages, _ := runSync(t, b, inFrame(t, protocol.MsgProviderDriveEpochStartRequest, id, protocol.ProviderDriveEpochStartRequestPayload{RequestID: "native-start", DriveAttemptID: name, Strategy: "generic", Revision: "1"}))
	start := firstOfType(messages, protocol.MsgProviderDriveEpochStartResult)
	if start == nil || start.Payload.(*protocol.ProviderDriveEpochStartResultPayload).Outcome != "started" {
		t.Fatalf("capacity fixture start: %+v", messages)
	}
	ordinal := int64(0)
	return id, protocol.NativeDownloadArmRequestV1Payload{RequestID: "native-arm-1", Producer: protocol.ArtifactProducerPayload{EffectKind: "generic_drive", DriveAttemptID: name, Ordinal: &ordinal, Strategy: "generic", Revision: "1"}, BrowserEpoch: "browser-epoch", DocumentID: name}
}

func TestNativeDownloadActiveReservationCapacity(t *testing.T) {
	b, jobs, id, _, request := nativeAttempt(t)
	for i := 0; i < maxOutstandingOffers; i++ {
		if i > 0 {
			id, request = nativeCapacityAttempt(t, b, jobs, i)
		}
		if result := nativeArm(t, b, id, request); result.Outcome != "armed" {
			t.Fatal(result)
		}
		// Keep each baseline and uncompleted reservation live in the bridge. The
		// single effect lane must retire its prior permit before another can start.
		_, _, err := jobs.SettleEffectPermit(context.Background(), job.EffectPermitSettleInput{Identity: job.EffectPermitIdentity{JobID: id, Kind: job.GenericDrive, DriveAttemptID: request.Producer.DriveAttemptID, Ordinal: *request.Producer.Ordinal, Strategy: "generic", Revision: "1"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	id, request = nativeCapacityAttempt(t, b, jobs, maxOutstandingOffers)
	result := nativeArm(t, b, id, request)
	if result.Outcome != "unavailable" || result.Reason != "source_busy" {
		t.Fatalf("active reservations evicted: %+v", result)
	}
	if nativeEventCount(t, jobs, id, "browser.native_download_reserved") != 0 {
		t.Fatal("capacity refusal consumed an arm")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.nativeDownloads) != maxOutstandingOffers {
		t.Fatalf("active cache size=%d", len(b.nativeDownloads))
	}
	for _, r := range b.nativeDownloads {
		if r.root == nil || r.observation != "" || r.outcome != "" {
			t.Fatalf("active reservation changed: %+v", r)
		}
	}
}
