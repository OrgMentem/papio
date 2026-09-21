// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func nativeReservationFixture(t *testing.T) (*Store, NativeDownloadReservation, time.Time) {
	t.Helper()
	js := testStore(t)
	ctx := context.Background()
	id := permitJob(t, js, "native-admission")
	generation, err := js.NextMaterializationHolderGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("generation=%d", generation)
	}
	now := time.Now()
	identity := driveIdentity(id, "native-drive", 0, "generic")
	permit := acquireDrive(t, js, identity, "native-domain", now.Add(time.Minute))
	detail := map[string]any{"drive_attempt_id": identity.DriveAttemptID, "ordinal": identity.Ordinal, "strategy": identity.Strategy, "revision": identity.Revision, "safety_domain": "native-domain"}
	for _, kind := range []string{"browser.provider_drive_epoch_offered", "browser.provider_drive_epoch_started"} {
		if err := js.RecordEvent(ctx, id, kind, detail); err != nil {
			t.Fatal(err)
		}
	}
	ordinal := int64(0)
	return js, NativeDownloadReservation{ReservationID: "native-reservation", PermitID: permit.ID, JobID: id, Producer: ArtifactProducerIdentity{Kind: GenericDrive, DriveAttemptID: identity.DriveAttemptID, Ordinal: &ordinal, Strategy: "generic", Revision: identity.Revision}, JobAttemptRevision: 1, HolderGeneration: 1, BindingSHA256: strings.Repeat("a", 64), ArmedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(time.Minute).UnixMilli()}, now
}
func nativeAdmission(r NativeDownloadReservation) NativeDownloadAdmission {
	return NativeDownloadAdmission{NativeDownloadReservation: r, DownloadID: 27, StartedAtMS: r.ArmedAtMS, ObservationSHA256: strings.Repeat("b", 64), Filename: r.ReservationID + ".pdf", SHA256: strings.Repeat("c", 64), SizeBytes: 4321}
}
func nativeLedgerCount(t *testing.T, js *Store, id, kind string) int {
	t.Helper()
	var n int
	if err := js.S.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind=?`, id, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNativeDownloadAdmissionUsesLatestDurableBinding(t *testing.T) {
	js, original, now := nativeReservationFixture(t)
	ctx := context.Background()
	if err := js.ReserveNativeDownload(ctx, original, now); err != nil {
		t.Fatal(err)
	}
	next := original
	next.BindingSHA256 = strings.Repeat("d", 64)
	// Model the committed transfer, including a freshly constructed Store that
	// has none of the bridge's private document or filesystem observations.
	raw, _ := json.Marshal(next)
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	detail["previous_binding_sha256"] = original.BindingSHA256
	detail["request_id"] = "rebind-1"
	if err := js.RecordEvent(ctx, original.JobID, "browser.native_download_rebound", detail); err != nil {
		t.Fatal(err)
	}
	reopened := &Store{S: js.S}
	if err := reopened.AdmitNativeDownload(ctx, nativeAdmission(original), now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("old document admitted after committed rebind: %v", err)
	}
	if err := reopened.AdmitNativeDownload(ctx, nativeAdmission(next), now); err != nil {
		t.Fatalf("current document rejected after committed rebind: %v", err)
	}
	if nativeLedgerCount(t, js, original.JobID, "browser.native_download_admitted") != 1 {
		t.Fatal("expected exactly one admission")
	}
}

func TestNativeDownloadRebindPreservesReservationAndIsIdempotent(t *testing.T) {
	js, original, now := nativeReservationFixture(t)
	ctx := context.Background()
	if err := js.ReserveNativeDownload(ctx, original, now); err != nil {
		t.Fatal(err)
	}
	next := original
	next.BindingSHA256 = strings.Repeat("d", 64)
	for range 2 {
		if err := js.RebindNativeDownload(ctx, original, next.BindingSHA256, "rebind-1", now); err != nil {
			t.Fatal(err)
		}
	}
	if nativeLedgerCount(t, js, original.JobID, "browser.native_download_rebound") != 1 || nativeLedgerCount(t, js, original.JobID, "browser.native_download_reserved") != 1 {
		t.Fatal("retry added reservation or transfer")
	}
	if err := js.RebindNativeDownload(ctx, next, strings.Repeat("e", 64), "rebind-1", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("reused request changed binding: %v", err)
	}
	if err := js.RebindNativeDownload(ctx, original, strings.Repeat("e", 64), "rebind-2", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("stale source changed binding: %v", err)
	}
	reopened := &Store{S: js.S}
	if err := reopened.AdmitNativeDownload(ctx, nativeAdmission(original), now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("old binding survived reload: %v", err)
	}
	if err := reopened.AdmitNativeDownload(ctx, nativeAdmission(next), now); err != nil {
		t.Fatalf("new binding did not survive reload: %v", err)
	}
	if err := reopened.RebindNativeDownload(ctx, original, next.BindingSHA256, "rebind-1", now); !errors.Is(err, ErrNativeDownloadReserved) {
		t.Fatalf("consumed transfer replayed: %v", err)
	}
	if err := reopened.ReserveNativeDownload(ctx, next, now); !errors.Is(err, ErrNativeDownloadReserved) {
		t.Fatalf("rebind reset arm latch: %v", err)
	}
}

func TestNativeDownloadRebindFencesDurableAuthority(t *testing.T) {
	for _, condition := range []string{"missing", "old binding", "expiry changed", "armed time changed", "same binding", "invalid binding", "admitted", "new generation", "new attempt", "cancelled", "closed action", "expired", "settled", "new offer", "finished"} {
		t.Run(condition, func(t *testing.T) {
			js, original, now := nativeReservationFixture(t)
			ctx := context.Background()
			if condition != "missing" {
				if err := js.ReserveNativeDownload(ctx, original, now); err != nil {
					t.Fatal(err)
				}
			}
			r := original
			next := strings.Repeat("d", 64)
			var err error
			switch condition {
			case "old binding":
				r.BindingSHA256 = strings.Repeat("f", 64)
			case "expiry changed":
				r.ExpiresAtMS++
			case "armed time changed":
				r.ArmedAtMS--
			case "same binding":
				next = r.BindingSHA256
			case "invalid binding":
				next = "invalid"
			case "admitted":
				err = js.AdmitNativeDownload(ctx, nativeAdmission(r), now)
			case "new generation":
				_, err = js.NextMaterializationHolderGeneration(ctx)
			case "new attempt":
				err = js.RecordEvent(ctx, r.JobID, "job.retry_requested", nil)
			case "cancelled":
				_, err = js.S.DB().Exec(`UPDATE jobs SET state='cancelled' WHERE id=?`, r.JobID)
			case "closed action":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET status='resolved' WHERE job_id=?`, r.JobID)
			case "expired":
				now = time.UnixMilli(r.ExpiresAtMS)
			case "settled":
				_, _, err = js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: driveIdentity(r.JobID, "native-drive", 0, "generic")})
			case "new offer":
				err = js.RecordEvent(ctx, r.JobID, "browser.provider_drive_epoch_offered", map[string]any{"drive_attempt_id": "new-drive"})
			case "finished":
				err = js.RecordEvent(ctx, r.JobID, "browser.provider_drive_epoch_result", map[string]any{"drive_attempt_id": "native-drive", "ordinal": 0, "strategy": "generic", "revision": "r1"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := js.RebindNativeDownload(ctx, r, next, "rebind-1", now); !errors.Is(err, ErrEffectPermitStale) && !errors.Is(err, ErrNativeDownloadReserved) {
				t.Fatalf("unsafe transfer accepted: %v", err)
			}
			if nativeLedgerCount(t, js, r.JobID, "browser.native_download_rebound") != 0 {
				t.Fatal("rejected transfer recorded")
			}
		})
	}
}

func TestNativeDownloadRebindAndAdmissionHaveOneWinner(t *testing.T) {
	js, r, now := nativeReservationFixture(t)
	ctx := context.Background()
	if err := js.ReserveNativeDownload(ctx, r, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- js.RebindNativeDownload(ctx, r, strings.Repeat("d", 64), "rebind-1", now) })
	wg.Go(func() { results <- js.AdmitNativeDownload(ctx, nativeAdmission(r), now) })
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrEffectPermitStale) && !errors.Is(err, ErrNativeDownloadReserved) {
			t.Fatal(err)
		}
	}
	if wins != 1 || nativeLedgerCount(t, js, r.JobID, "browser.native_download_rebound")+nativeLedgerCount(t, js, r.JobID, "browser.native_download_admitted") != 1 {
		t.Fatalf("race committed %d winners", wins)
	}
}
func TestNativeDownloadAtomicAdmissionAndOneShot(t *testing.T) {
	js, r, now := nativeReservationFixture(t)
	ctx := context.Background()
	if err := js.ReserveNativeDownload(ctx, r, now); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.ReservationID = "replacement-token"
	if err := js.ReserveNativeDownload(ctx, changed, now); !errors.Is(err, ErrNativeDownloadReserved) {
		t.Fatalf("second reservation=%v", err)
	}
	a := nativeAdmission(r)
	if _, err := js.S.DB().Exec(`CREATE TRIGGER reject_native_complete BEFORE INSERT ON events WHEN NEW.kind='browser.download_complete' BEGIN SELECT RAISE(ABORT,'test admission rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeDownload(ctx, a, now); err == nil {
		t.Fatal("failed second event committed")
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.native_download_admitted") != 0 {
		t.Fatal("admission not atomic")
	}
	if _, err := js.S.DB().Exec(`DROP TRIGGER reject_native_complete`); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeDownload(ctx, a, now); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeDownload(ctx, a, now); !errors.Is(err, ErrNativeDownloadReserved) {
		t.Fatalf("duplicate admission=%v", err)
	}
	producer, err := js.ArtifactProducerForArtifact(ctx, r.JobID, a.Filename, a.SHA256)
	if err != nil || producer == nil || producer.DriveAttemptID != r.Producer.DriveAttemptID {
		t.Fatalf("exact producer=%+v %v", producer, err)
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != 1 {
		t.Fatal("wrong completion count")
	}
}
func TestNativeDownloadConcurrentArmOneWinner(t *testing.T) {
	js, r, now := nativeReservationFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { results <- js.ReserveNativeDownload(context.Background(), r, now) })
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 || nativeLedgerCount(t, js, r.JobID, "browser.native_download_reserved") != 1 {
		t.Fatalf("wins=%d", wins)
	}
}
func TestNativeDownloadAdmissionRechecksDurableAuthority(t *testing.T) {
	for _, condition := range []string{"missing reservation", "changed binding", "changed producer", "closed action", "new attempt", "new generation", "terminal job", "expired", "settled permit", "new offer", "finished epoch"} {
		t.Run(condition, func(t *testing.T) {
			js, r, now := nativeReservationFixture(t)
			ctx := context.Background()
			if condition != "missing reservation" {
				if err := js.ReserveNativeDownload(ctx, r, now); err != nil {
					t.Fatal(err)
				}
			}
			a := nativeAdmission(r)
			var err error
			switch condition {
			case "changed binding":
				a.BindingSHA256 = strings.Repeat("d", 64)
			case "changed producer":
				a.Producer.DriveAttemptID = "wrong-drive"
			case "closed action":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET status='resolved' WHERE job_id=?`, r.JobID)
			case "new attempt":
				err = js.RecordEvent(ctx, r.JobID, "job.retry_requested", nil)
			case "new generation":
				_, err = js.NextMaterializationHolderGeneration(ctx)
			case "terminal job":
				_, err = js.S.DB().Exec(`UPDATE jobs SET state='cancelled' WHERE id=?`, r.JobID)
			case "expired":
				now = now.Add(2 * time.Minute)
			case "settled permit":
				_, _, err = js.SettleEffectPermit(ctx, EffectPermitSettleInput{Identity: driveIdentity(r.JobID, "native-drive", 0, "generic")})
			case "new offer":
				err = js.RecordEvent(ctx, r.JobID, "browser.provider_drive_epoch_offered", map[string]any{"drive_attempt_id": "different-drive"})
			case "finished epoch":
				err = js.RecordEvent(ctx, r.JobID, "browser.provider_drive_epoch_result", map[string]any{"drive_attempt_id": "native-drive", "ordinal": 0, "strategy": "generic", "revision": "r1"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := js.AdmitNativeDownload(ctx, a, now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("stale authority=%v", err)
			}
			if nativeLedgerCount(t, js, r.JobID, "browser.native_download_admitted") != 0 || nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != 0 {
				t.Fatal("stale admission emitted artifact authority")
			}
		})
	}
}
