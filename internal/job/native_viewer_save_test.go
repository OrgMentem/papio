// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func viewerSaveFixture(t *testing.T) (*Store, NativeViewerSaveInput, time.Time) {
	t.Helper()
	js := testStore(t)
	ctx := context.Background()
	id := permitJob(t, js, "viewer-save")
	actions, err := js.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if err := js.ResolveHumanAction(ctx, a.ID, "resolved"); err != nil {
			t.Fatal(err)
		}
	}
	action, err := js.OpenHumanAction(ctx, id, "manual_download", "save loaded viewer", Access(false, "landing_page"), WithHumanActionDiagnosis(DiagnosisReasonNativeViewerDownload))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := js.NextMaterializationHolderGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return js, NativeViewerSaveInput{RequestID: "prepare-1", OperationID: NewID("viewer"), JobID: id, ActionID: action, ActionRevision: 1,
		JobAttemptRevision: 1, HolderGeneration: generation, BindingSHA256: strings.Repeat("a", 64),
		SafetyDomainID: "viewer-domain", ExpiresAtMS: now.Add(time.Minute).UnixMilli()}, now
}

func reserveViewer(t *testing.T, js *Store, in NativeViewerSaveInput, now time.Time) NativeViewerSaveReservation {
	t.Helper()
	r, err := js.ReserveNativeViewerSave(context.Background(), in, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNativeViewerSaveFreshPermitAndExactProducer(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	p, err := js.GetEffectPermit(ctx, r.PermitID)
	if err != nil || p == nil || p.Status != Held || p.Kind != GenericDrive || p.Strategy != NativeViewerSaveStrategy || p.DriveAttemptID != r.OperationID || p.Revision != in.BindingSHA256 {
		t.Fatalf("permit=%+v err=%v", p, err)
	}
	if err := js.CheckNativeViewerSave(ctx, r, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{Identity: r.Producer.effectIdentity(r.JobID),
		JobAttemptRevision: 1, BrowserHolderGeneration: r.HolderGeneration, SafetyDomainID: in.SafetyDomainID,
		LeaseUntil: now.Add(time.Minute), Authorization: EffectPermitEvent{Kind: "forged"}}); err == nil {
		t.Fatal("ordinary acquire accepted reserved strategy")
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := js.CheckNativeViewerSave(ctx, r, now); err != nil {
		t.Fatalf("admitted held record should remain current: %v", err)
	}
	producer, err := js.ArtifactProducerForArtifact(ctx, r.JobID, r.Filename(), digest)
	if err != nil || producer == nil || !artifactProducerEqual(*producer, r.Producer) {
		t.Fatalf("producer=%+v err=%v", producer, err)
	}
	wrong, err := js.ArtifactProducerForArtifact(ctx, r.JobID, r.Filename(), strings.Repeat("c", 64))
	if err != nil || wrong != nil {
		t.Fatalf("wrong digest matched: %+v %v", wrong, err)
	}
	settled, err := js.SettleArtifactProducer(ctx, r.JobID, *producer)
	if err != nil || !settled {
		t.Fatalf("settled=%v err=%v", settled, err)
	}
	for _, kind := range []string{"browser.provider_drive_epoch_offered", "browser.provider_drive_epoch_started", "browser.provider_drive_epoch_result"} {
		if nativeLedgerCount(t, js, r.JobID, kind) != 0 {
			t.Fatalf("fabricated %s", kind)
		}
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_result") != 1 {
		t.Fatal("missing native result")
	}
	other := permitJob(t, js, "viewer-successor")
	successor := acquireDrive(t, js, driveIdentity(other, "successor", 0, "generic"), "other-domain", now.Add(time.Minute))
	if _, err := js.SettleArtifactProducer(ctx, r.JobID, *producer); err != nil {
		t.Fatal(err)
	}
	live, err := js.LiveEffectPermit(ctx)
	if err != nil || live == nil || live.ID != successor.ID {
		t.Fatalf("late producer released successor: %+v %v", live, err)
	}
}

// Abandonment settles only the authenticated admitted operation, refuses to
// treat a lost holder as lost authority, and cannot touch a successor.
func TestNativeViewerStagedAdmissionAbandonsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	if listed, err := js.NativeViewerStagedAdmissions(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("unadmitted operation listed: %+v %v", listed, err)
	}
	digest := strings.Repeat("b", 64)
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); err != nil {
		t.Fatal(err)
	}
	// A newer holder generation must not make admitted bytes unadoptable.
	generation, err := js.NextMaterializationHolderGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := js.NativeViewerStagedAdmissions(ctx)
	if err != nil || len(listed) != 1 || !listed[0].Adoptable || listed[0].SHA256 != digest || listed[0].Reservation.PermitID != r.PermitID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if err := js.AbandonNativeViewerStagedAdmission(ctx, r.JobID, r.OperationID, NativeViewerAuthorityLost, ""); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("abandoned adoptable bytes as authority_lost: %v", err)
	}
	if err := js.AbandonNativeViewerStagedAdmission(ctx, r.JobID, r.OperationID, NativeViewerStageMissing, "/tmp/other.tmp"); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("foreign retained path accepted: %v", err)
	}
	if err := js.AbandonNativeViewerStagedAdmission(ctx, r.JobID, r.OperationID, NativeViewerStageMissing, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := js.GetEffectPermit(ctx, r.PermitID); p == nil || p.Status != Settled {
		t.Fatalf("permit=%+v", p)
	}
	other := permitJob(t, js, "viewer-successor")
	successor := acquireDrive(t, js, driveIdentity(other, "successor", 0, "generic"), "other-domain", now.Add(time.Minute))
	if err := js.AbandonNativeViewerStagedAdmission(ctx, r.JobID, r.OperationID, NativeViewerStageMissing, ""); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("second abandonment: %v", err)
	}
	if listed, err := js.NativeViewerStagedAdmissions(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("settled operation listed: %+v %v", listed, err)
	}
	live, err := js.LiveEffectPermit(ctx)
	if err != nil || live == nil || live.ID != successor.ID || live.Status != Held {
		t.Fatalf("successor released: %+v %v", live, err)
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_result") != 1 {
		t.Fatal("abandonment result not exactly once")
	}
	in.HolderGeneration = generation
	if _, err := js.ReserveNativeViewerSave(ctx, in, now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("abandonment re-armed the action: %v", err)
	}
}

func TestNativeViewerSaveOneShotSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	reopened := &Store{S: js.S}
	for _, request := range []string{in.RequestID, "different-prepare"} {
		next := in
		next.RequestID = request
		if _, err := reopened.ReserveNativeViewerSave(ctx, next, now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
			t.Fatalf("rearm %s: %v", request, err)
		}
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("repeated step: %v", err)
	}
	if _, err := js.ReconcileEffectPermit(ctx, EffectPermitObservation{PermitID: r.PermitID, BrowserHolderGeneration: r.HolderGeneration}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BeginNativeViewerSaveStep(ctx, r, "advance-2", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("unknown step: %v", err)
	}
	if err := js.ResolveUnknownEffectPermit(ctx, r.PermitID, "test cleanup"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReserveNativeViewerSave(ctx, in, now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("override rearmed action: %v", err)
	}
}

func TestNativeViewerSaveCurrentAuthorityFences(t *testing.T) {
	for _, condition := range []string{"cancel", "dismiss", "revision", "closed", "holder", "retry", "binding", "expiry", "permit"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			js, in, now := viewerSaveFixture(t)
			r := reserveViewer(t, js, in, now)
			if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
				t.Fatal(err)
			}
			var err error
			switch condition {
			case "cancel":
				err = js.Cancel(ctx, r.JobID, TerminalReasonUserDismissed)
			case "dismiss":
				_, err = js.DismissHumanAction(ctx, r.ActionID, r.ActionRevision)
			case "revision":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, r.ActionID)
			case "closed":
				err = js.ResolveHumanAction(ctx, r.ActionID, "resolved")
			case "holder":
				_, err = js.NextMaterializationHolderGeneration(ctx)
			case "retry":
				err = js.RecordEvent(ctx, r.JobID, "job.retry_requested", nil)
			case "binding":
				r.BindingSHA256 = strings.Repeat("d", 64)
			case "expiry":
				now = time.UnixMilli(r.ExpiresAtMS)
			case "permit":
				r.PermitID = "permit-other"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := js.CheckNativeViewerSave(ctx, r, now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("check: %v", err)
			}
			if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-2", now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("step: %v", err)
			}
			if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), strings.Repeat("b", 64), 100, now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("admit: %v", err)
			}
			if nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != 0 {
				t.Fatal("stale producer published")
			}
		})
	}
}

func TestNativeViewerSaveReserveEligibilityAndOccupancy(t *testing.T) {
	for _, condition := range []string{"wrong action", "wrong diagnosis", "explicit selection", "requires auth", "holder", "attempt", "held", "unknown", "missing operation", "bad operation", "long expiry"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			js, in, now := viewerSaveFixture(t)
			var err error
			switch condition {
			case "missing operation":
				in.OperationID = ""
			case "bad operation":
				in.OperationID = "../viewer"
			case "long expiry":
				in.ExpiresAtMS = now.Add(2*time.Minute + time.Millisecond).UnixMilli()
			case "requires auth":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET requires_auth=1 WHERE id=?`, in.ActionID)
			case "wrong action":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET kind='openurl_handoff' WHERE id=?`, in.ActionID)
			case "wrong diagnosis", "explicit selection":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET diagnosis='provider_adapter_missing' WHERE id=?`, in.ActionID)
				in.ExplicitSelection = condition == "explicit selection"
			case "holder":
				in.HolderGeneration++
			case "attempt":
				in.JobAttemptRevision++
			case "held", "unknown":
				other := permitJob(t, js, "busy-provider")
				p := acquireDrive(t, js, driveIdentity(other, "occupied", 0, "generic"), "other-domain", now.Add(time.Minute))
				if condition == "unknown" {
					_, err = js.ReconcileEffectPermit(ctx, EffectPermitObservation{PermitID: p.ID, BrowserHolderGeneration: 1})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = js.ReserveNativeViewerSave(ctx, in, now)
			want := ErrEffectPermitStale
			if condition == "held" || condition == "unknown" {
				want = ErrEffectPermitBusy
			}
			if condition == "explicit selection" || condition == "requires auth" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}

func TestNativeViewerSaveConcurrentReserveOneWinner(t *testing.T) {
	js, in, now := viewerSaveFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := js.ReserveNativeViewerSave(context.Background(), in, now)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrNativeViewerSaveConsumed) {
			t.Fatal(err)
		}
	}
	if winners != 1 || nativeLedgerCount(t, js, in.JobID, "browser.native_viewer_save_reserved") != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestNativeViewerSaveAdmissionAndPendingPolls(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	digest := strings.Repeat("b", 64)
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("no dispatch: %v", err)
	}
	for i := 1; i <= 12; i++ {
		if err := js.BeginNativeViewerSaveStep(ctx, r, fmt.Sprintf("step-%d", i), now); err != nil {
			t.Fatal(err)
		}
	}
	for _, filename := range []string{"other.pdf", "../" + r.Filename()} {
		if err := js.AdmitNativeViewerSave(ctx, r, filename, digest, 100, now); !errors.Is(err, ErrEffectPermitStale) {
			t.Fatalf("filename %q: %v", filename, err)
		}
	}
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("duplicate: %v", err)
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != 1 {
		t.Fatal("duplicate producer")
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "after-admission", now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("admitted operation dispatched again: %v", err)
	}
}

func TestNativeViewerSaveCancelClassifiesOnlyExactOperation(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			ctx := context.Background()
			js, in, now := viewerSaveFixture(t)
			r := reserveViewer(t, js, in, now)
			if started {
				if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
					t.Fatal(err)
				}
			}
			// Cleanup remains available after lease and holder loss.
			if _, err := js.NextMaterializationHolderGeneration(ctx); err != nil {
				t.Fatal(err)
			}
			now = now.Add(3 * time.Minute)
			bad := r
			bad.BindingSHA256 = strings.Repeat("c", 64)
			if _, err := js.CancelNativeViewerSave(ctx, bad, now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("wrong binding cleanup: %v", err)
			}
			want := Settled
			if started {
				want = UnknownCompletion
			}
			for range 2 {
				status, err := js.CancelNativeViewerSave(ctx, r, now)
				if err != nil || status != want {
					t.Fatalf("status=%s want=%s err=%v", status, want, err)
				}
			}
			if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-2", now); !errors.Is(err, ErrEffectPermitStale) {
				t.Fatalf("cancelled operation advanced: %v", err)
			}
			count := 0
			if !started {
				count = 1
			}
			if nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_result") != count {
				t.Fatal("incorrect definitive result")
			}
		})
	}
}

func TestNativeViewerSaveCancelRacesBegin(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	var wg sync.WaitGroup
	var stepErr, cancelErr error
	var status EffectPermitStatus
	wg.Add(2)
	go func() { defer wg.Done(); stepErr = js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now) }()
	go func() { defer wg.Done(); status, cancelErr = js.CancelNativeViewerSave(ctx, r, now) }()
	wg.Wait()
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if stepErr == nil {
		if status != UnknownCompletion {
			t.Fatalf("dispatched operation freed: %s", status)
		}
	} else if !errors.Is(stepErr, ErrEffectPermitStale) || status != Settled {
		t.Fatalf("step=%v status=%s", stepErr, status)
	}
}

func TestNativeViewerSaveAdmissionProducerRollback(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().Exec(`CREATE TRIGGER fail_viewer_producer BEFORE INSERT ON events
		WHEN NEW.kind='browser.download_complete' BEGIN SELECT RAISE(ABORT,'test producer failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), strings.Repeat("b", 64), 100, now); err == nil {
		t.Fatal("wanted injected failure")
	}
	if nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_admitted") != 0 || nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != 0 {
		t.Fatal("partial admission committed")
	}
}

func TestNativeViewerSaveOperationIDCannotMoveToAnotherAction(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	if _, err := js.CancelNativeViewerSave(ctx, r, now); err != nil {
		t.Fatal(err)
	}
	other := permitJob(t, js, "other-viewer-job")
	actions, err := js.ListOpenHumanActionsForJobs(ctx, []string{other})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if err := js.ResolveHumanAction(ctx, a.ID, "resolved"); err != nil {
			t.Fatal(err)
		}
	}
	action, err := js.OpenHumanAction(ctx, other, "manual_download", "loaded viewer", Access(false, ""), WithHumanActionDiagnosis(DiagnosisReasonNativeViewerDownload))
	if err != nil {
		t.Fatal(err)
	}
	in.JobID, in.ActionID = other, action
	if _, err := js.ReserveNativeViewerSave(ctx, in, now); !errors.Is(err, ErrNativeViewerSaveConsumed) {
		t.Fatalf("operation ID reused: %v", err)
	}
}

func TestNativeViewerSaveAdmissionRacesCancellation(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var admissionErr, cancelErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		admissionErr = js.AdmitNativeViewerSave(ctx, r, r.Filename(), strings.Repeat("b", 64), 100, now)
	}()
	go func() { defer wg.Done(); cancelErr = js.Cancel(ctx, r.JobID, TerminalReasonUserDismissed) }()
	wg.Wait()
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if admissionErr != nil && !errors.Is(admissionErr, ErrEffectPermitStale) {
		t.Fatal(admissionErr)
	}
	want := 0
	if admissionErr == nil {
		want = 1
	} // admission committed before cancellation
	if nativeLedgerCount(t, js, r.JobID, "browser.download_complete") != want || nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_admitted") != want {
		t.Fatal("split admission and producer")
	}
	row, err := js.Get(ctx, r.JobID)
	if err != nil || row.State != StateCancelled || row.ArtifactSHA256 != "" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-2", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("cancelled advance: %v", err)
	}
}

func TestNativeViewerSaveRejectsProviderResult(t *testing.T) {
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	identity := r.Producer.effectIdentity(r.JobID)
	detail := map[string]any{"drive_attempt_id": r.OperationID, "ordinal": int64(0), "strategy": NativeViewerSaveStrategy, "revision": r.BindingSHA256, "outcome": "applied"}
	if _, _, err := js.SettleEffectPermit(context.Background(), EffectPermitSettleInput{Identity: identity, RequiredEvents: []EffectPermitEvent{{Kind: "browser.provider_drive_epoch_result", Detail: detail}}}); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("provider result settled native operation: %v", err)
	}
	if _, _, err := js.SettleEffectPermit(context.Background(), EffectPermitSettleInput{Identity: identity, RequiredEvents: []EffectPermitEvent{{Kind: "browser.native_viewer_save_result", Detail: detail}}}); err != nil {
		t.Fatal(err)
	}
}

func admitViewerCandidate(t *testing.T, js *Store, r NativeViewerSaveReservation, now time.Time) (int64, string) {
	t.Helper()
	ctx := context.Background()
	digest := strings.Repeat("b", 64)
	if err := js.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	if err := js.AdmitNativeViewerSave(ctx, r, r.Filename(), digest, 100, now); err != nil {
		t.Fatal(err)
	}
	key := "browser-adopt:sha256:" + digest
	if _, err := js.InsertCandidates(ctx, r.JobID, []Candidate{{JobID: r.JobID, Source: "browser", URLRedacted: "browser://adopted-download", URLKey: key,
		Version: "unknown", AccessBasis: "manual", ReuseLicense: "unknown", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 0.5}}); err != nil {
		t.Fatal(err)
	}
	id, err := js.CandidateIDByKey(ctx, r.JobID, key)
	if err != nil {
		t.Fatal(err)
	}
	return id, digest
}

func TestNativeViewerSaveAdoptionAfterRestart(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	candidate, digest := admitViewerCandidate(t, js, r, now)
	if _, err := js.NextMaterializationHolderGeneration(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CancelNativeViewerSave(ctx, r, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	reopened := &Store{S: js.S}
	if err := reopened.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); err != nil {
		t.Fatal(err)
	}
	row, err := js.Get(ctx, r.JobID)
	if err != nil || row.State != StateValidating || row.SelectedCandidateID != candidate {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "after-restart", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("historical proof dispatched effect: %v", err)
	}
}

func TestNativeViewerSaveAdoptionRejectsChangedActionAndBytes(t *testing.T) {
	for _, condition := range []string{"revision", "resolved with other open action", "retry", "cancel", "wrong digest", "unknown filename", "wrong candidate", "no admission", "wrong admitted producer"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			js, in, now := viewerSaveFixture(t)
			r := reserveViewer(t, js, in, now)
			candidate, digest := admitViewerCandidate(t, js, r, now)
			filename := r.Filename()
			var err error
			switch condition {
			case "revision":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, r.ActionID)
			case "resolved with other open action":
				err = js.ResolveHumanAction(ctx, r.ActionID, "resolved")
				if err == nil {
					_, err = js.OpenHumanAction(ctx, r.JobID, "manual_download", "replacement", Access(false, ""))
				}
			case "retry":
				err = js.RecordEvent(ctx, r.JobID, "job.retry_requested", nil)
			case "cancel":
				err = js.Cancel(ctx, r.JobID, TerminalReasonUserDismissed)
			case "wrong digest":
				digest = strings.Repeat("c", 64)
			case "unknown filename":
				filename = "papio-viewer-" + NewID("viewer") + ".pdf"
			case "wrong candidate":
				candidate++
			case "no admission":
				_, err = js.S.DB().Exec(`DELETE FROM events WHERE job_id=? AND kind='browser.native_viewer_save_admitted'`, r.JobID)
			case "wrong admitted producer":
				_, err = js.S.DB().Exec(`UPDATE events SET detail_json=json_set(detail_json,'$.producer.drive_attempt_id','wrong-operation') WHERE job_id=? AND kind='browser.native_viewer_save_admitted'`, r.JobID)
			}
			if err != nil {
				t.Fatal(err)
			}
			reopened := &Store{S: js.S}
			if err := reopened.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, filename, digest); !errors.Is(err, ErrAdoptNotAwaiting) {
				t.Fatalf("transition: %v", err)
			}
			row, err := js.Get(ctx, r.JobID)
			if err != nil || row.State == StateValidating || row.SelectedCandidateID != 0 {
				t.Fatalf("refused file selected: %+v %v", row, err)
			}
		})
	}
}

func TestNativeViewerSaveProducerRequiresNativeAdmission(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	digest := strings.Repeat("b", 64)
	// A normal download_complete cannot manufacture native admission.
	if err := js.RecordEvent(ctx, r.JobID, "browser.download_complete", map[string]any{"filename": r.Filename(), "sha256": digest, "producer": r.Producer}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ArtifactProducerForArtifact(ctx, r.JobID, r.Filename(), digest); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("forged producer recovered: %v", err)
	}
	if _, err := js.SettleArtifactProducer(ctx, r.JobID, r.Producer); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("unadmitted producer settled: %v", err)
	}
	permit, err := js.GetEffectPermit(ctx, r.PermitID)
	if err != nil || permit.Status != Held {
		t.Fatalf("permit=%+v err=%v", permit, err)
	}
	_, _ = admitViewerCandidate(t, js, r, now)
	otherDigest := strings.Repeat("c", 64)
	if err := js.RecordEvent(ctx, r.JobID, "browser.download_complete", map[string]any{"filename": "unrelated.pdf", "sha256": otherDigest, "producer": r.Producer}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ArtifactProducerForArtifact(ctx, r.JobID, "unrelated.pdf", otherDigest); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("unadmitted bytes recovered producer: %v", err)
	}
	if _, _, _, err := js.CommitArtifactWinnerAndProducer(ctx, ArtifactWinner{JobID: r.JobID, JobAttemptRevision: r.JobAttemptRevision, CandidateID: "other", BrowserHolderGeneration: r.HolderGeneration, SHA256: otherDigest}, &r.Producer); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("wrong digest winner: %v", err)
	}
}

func TestNativeViewerSaveAdoptionRacesActionRevision(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	candidate, digest := admitViewerCandidate(t, js, r, now)
	var wg sync.WaitGroup
	var transitionErr, revisionErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		transitionErr = js.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest)
	}()
	go func() {
		defer wg.Done()
		_, revisionErr = js.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, r.ActionID)
	}()
	wg.Wait()
	if revisionErr != nil {
		t.Fatal(revisionErr)
	}
	if transitionErr != nil && !errors.Is(transitionErr, ErrAdoptNotAwaiting) {
		t.Fatal(transitionErr)
	}
	row, err := js.Get(ctx, r.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if transitionErr == nil {
		if row.State != StateValidating || row.SelectedCandidateID != candidate {
			t.Fatalf("transition didn't commit: %+v", row)
		}
	} else if row.State != StateAwaitingHuman || row.SelectedCandidateID != 0 {
		t.Fatalf("stale transition mutated job: %+v", row)
	}
}

func viewerActionState(t *testing.T, js *Store, actionID int64) (status, resolved string, revision int64) {
	t.Helper()
	if err := js.S.DB().QueryRow(`SELECT status,COALESCE(resolved_at,''),revision FROM human_actions WHERE id=?`, actionID).Scan(&status, &resolved, &revision); err != nil {
		t.Fatal(err)
	}
	return
}

func TestNativeViewerSaveRepeatedValidationInfrastructureRetry(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	candidate, digest := admitViewerCandidate(t, js, r, now)
	if err := js.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); err != nil {
		t.Fatal(err)
	}
	status, resolved, revision := viewerActionState(t, js, r.ActionID)
	if status != "resolved" || resolved == "" || revision != r.ActionRevision {
		t.Fatalf("action=%s %s %d", status, resolved, revision)
	}
	for range 3 {
		if err := js.Transition(ctx, r.JobID, StateValidating, StateAwaitingHuman, map[string]any{"reason": "adoption_validation_error"}); err != nil {
			t.Fatal(err)
		}
		if _, err := js.NextMaterializationHolderGeneration(ctx); err != nil {
			t.Fatal(err)
		}
		reopened := &Store{S: js.S}
		if err := reopened.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); err != nil {
			t.Fatal(err)
		}
		gotStatus, gotResolved, gotRevision := viewerActionState(t, js, r.ActionID)
		if gotStatus != status || gotResolved != resolved || gotRevision != revision {
			t.Fatal("validation retry changed original action")
		}
	}
	var closures int
	if err := js.S.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='job.transition' AND json_extract(detail_json,'$.native_viewer_action_closed')=1`, r.JobID).Scan(&closures); err != nil {
		t.Fatal(err)
	}
	if closures != 1 || nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_reserved") != 1 || nativeLedgerCount(t, js, r.JobID, "browser.native_viewer_save_step") != 1 {
		t.Fatal("retry minted new action/effect authority")
	}
	if err := js.BeginNativeViewerSaveStep(ctx, r, "retry-save", now); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("validation retry resumed save: %v", err)
	}
}

func TestNativeViewerSaveValidationRetryRefusesChangedProof(t *testing.T) {
	for _, condition := range []string{"revision", "cancelled action", "replacement open", "replacement closed", "replacement dismissed", "cancel", "retry attempt", "selected candidate", "wrong candidate", "wrong digest", "wrong repark", "other route", "missing closure proof", "changed closure time", "reopened original"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			js, in, now := viewerSaveFixture(t)
			r := reserveViewer(t, js, in, now)
			candidate, digest := admitViewerCandidate(t, js, r, now)
			if err := js.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); err != nil {
				t.Fatal(err)
			}
			if err := js.Transition(ctx, r.JobID, StateValidating, StateAwaitingHuman, map[string]any{"reason": "adoption_validation_error"}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch condition {
			case "revision":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, r.ActionID)
			case "cancelled action":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET status='cancelled' WHERE id=?`, r.ActionID)
			case "changed closure time":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET resolved_at='other' WHERE id=?`, r.ActionID)
			case "reopened original":
				_, err = js.S.DB().Exec(`UPDATE human_actions SET status='open',resolved_at=NULL WHERE id=?`, r.ActionID)
			case "replacement open", "replacement closed", "replacement dismissed":
				var action int64
				action, err = js.OpenHumanAction(ctx, r.JobID, "manual_download", "replacement", Access(false, ""))
				if err == nil && condition == "replacement closed" {
					err = js.ResolveHumanAction(ctx, action, "resolved")
				}
				if err == nil && condition == "replacement dismissed" {
					_, err = js.DismissHumanAction(ctx, action, 1)
				}
			case "cancel":
				err = js.Cancel(ctx, r.JobID, TerminalReasonUserDismissed)
			case "retry attempt":
				err = js.RecordEvent(ctx, r.JobID, "job.retry_requested", nil)
			case "selected candidate":
				_, err = js.S.DB().Exec(`UPDATE jobs SET selected_candidate_id=NULL WHERE id=?`, r.JobID)
			case "wrong candidate":
				candidate++
			case "wrong digest":
				digest = strings.Repeat("c", 64)
			case "wrong repark":
				_, err = js.S.DB().Exec(`UPDATE events SET detail_json=json_set(detail_json,'$.reason','adopted_download_rejected') WHERE seq=(SELECT MAX(seq) FROM events WHERE job_id=? AND kind='job.transition')`, r.JobID)
			case "other route":
				err = js.Transition(ctx, r.JobID, StateAwaitingHuman, StateResolving, nil)
				if err == nil {
					err = js.Transition(ctx, r.JobID, StateResolving, StateAwaitingHuman, map[string]any{"reason": "adoption_validation_error"})
				}
			case "missing closure proof":
				_, err = js.S.DB().Exec(`UPDATE events SET detail_json=json_remove(detail_json,'$.native_viewer_action_closed') WHERE job_id=? AND kind='job.transition'`, r.JobID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := js.Get(ctx, r.JobID)
			if err != nil {
				t.Fatal(err)
			}
			reopened := &Store{S: js.S}
			if err := reopened.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); !errors.Is(err, ErrAdoptNotAwaiting) {
				t.Fatalf("retry accepted: %v", err)
			}
			after, err := js.Get(ctx, r.JobID)
			if err != nil || after.State != before.State || after.SelectedCandidateID != before.SelectedCandidateID {
				t.Fatalf("refused retry mutated job: %+v %v", after, err)
			}
		})
	}
}

func TestNativeViewerSaveFirstValidationClosureRollsBack(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	candidate, digest := admitViewerCandidate(t, js, r, now)
	if _, err := js.S.DB().Exec(`CREATE TRIGGER fail_viewer_validation_transition BEFORE INSERT ON events WHEN NEW.kind='job.transition'
		AND json_extract(NEW.detail_json,'$.native_viewer_operation_id') IS NOT NULL BEGIN SELECT RAISE(ABORT,'test transition failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := js.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest); err == nil {
		t.Fatal("wanted transition failure")
	}
	status, resolved, revision := viewerActionState(t, js, r.ActionID)
	if status != "open" || resolved != "" || revision != r.ActionRevision {
		t.Fatalf("action close escaped rollback: %s %s %d", status, resolved, revision)
	}
	row, err := js.Get(ctx, r.JobID)
	if err != nil || row.State != StateAwaitingHuman || row.SelectedCandidateID != 0 {
		t.Fatalf("state escaped rollback: %+v %v", row, err)
	}
}

func TestNativeViewerSaveFirstValidationRacesDismiss(t *testing.T) {
	ctx := context.Background()
	js, in, now := viewerSaveFixture(t)
	r := reserveViewer(t, js, in, now)
	candidate, digest := admitViewerCandidate(t, js, r, now)
	var wg sync.WaitGroup
	var transitionErr, dismissErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		transitionErr = js.TransitionAwaitingToValidatingForNativeViewer(ctx, r.JobID, candidate, r.Filename(), digest)
	}()
	go func() { defer wg.Done(); _, dismissErr = js.DismissHumanAction(ctx, r.ActionID, r.ActionRevision) }()
	wg.Wait()
	status, _, _ := viewerActionState(t, js, r.ActionID)
	if transitionErr == nil {
		if status != "resolved" || !errors.Is(dismissErr, ErrConflict) {
			t.Fatalf("validation winner: action=%s dismiss=%v", status, dismissErr)
		}
	} else {
		if !errors.Is(transitionErr, ErrAdoptNotAwaiting) || dismissErr != nil || status != "cancelled" {
			t.Fatalf("dismiss winner: transition=%v dismiss=%v action=%s", transitionErr, dismissErr, status)
		}
	}
}
