// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/nativeviewer"
	"papio/internal/protocol"
)

// freshViewerRecoveryBridge models a restarted daemon: no in-memory native
// state, a newer browser holder, and a driver that fails the test if touched.
func freshViewerRecoveryBridge(t *testing.T, b *Bridge) (*Bridge, *atomic.Int32) {
	t.Helper()
	fresh := NewBridge(b.jobs, b.svc, b.triage, b.watchRunner, b.preview, b.captureStore, b.holdings, b.zotio, b.cfg, b.Version)
	var prepares atomic.Int32
	fresh.SetNativeViewerDriver(viewerTestDriver{func(context.Context, nativeviewer.Request) (nativeviewer.Session, error) {
		prepares.Add(1)
		return nil, errors.New("recovery must never prepare a native save")
	}})
	runSyncAs(t, fresh, sessB, helloWithFeatures(t, "1.2.3", effectPermitFeature, nativeViewerDownloadFeature, protocol.NativeViewerSaveFeature))
	return fresh, &prepares
}

func viewerStagePath(source, operationID string) string {
	return filepath.Join(source, "papio", "native_stage_"+operationID+".tmp")
}

func viewerSHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// admitViewerStage reproduces the durable state a native worker leaves at a
// crash boundary, through the production store transitions only.
func admitViewerStage(t *testing.T, b *Bridge, jobs *job.Store, id, source string, p protocol.NativeViewerSaveRequestV1Payload, body []byte, admit bool) job.NativeViewerSaveReservation {
	t.Helper()
	ctx := context.Background()
	attempt, err := jobs.MaterializationAttemptRevision(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	generation := b.arbitration.generation()
	b.mu.Unlock()
	now := time.Now()
	r, err := jobs.ReserveNativeViewerSave(ctx, job.NativeViewerSaveInput{RequestID: "prepare-1", OperationID: job.NewID("viewer"), JobID: id,
		ActionID: p.ActionID, ActionRevision: p.ActionRevision, JobAttemptRevision: attempt, HolderGeneration: generation,
		BindingSHA256: strings.Repeat("c", 64), SafetyDomainID: "native-viewer:publisher.example", ExpiresAtMS: now.Add(time.Minute).UnixMilli()}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BeginNativeViewerSaveStep(ctx, r, "advance-1", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(viewerStagePath(source, r.OperationID), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if admit {
		if err := jobs.AdmitNativeViewerSave(ctx, r, r.Filename(), viewerSHA(body), int64(len(body)), now); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func viewerPermitStatus(t *testing.T, jobs *job.Store, permitID string) job.EffectPermitStatus {
	t.Helper()
	p, err := jobs.GetEffectPermit(context.Background(), permitID)
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	return p.Status
}

func viewerAbandonments(t *testing.T, jobs *job.Store, id string) []map[string]any {
	t.Helper()
	rows, err := jobs.S.DB().Query(`SELECT detail_json FROM events WHERE job_id=? AND kind='browser.native_viewer_save_result'
		AND json_extract(detail_json,'$.outcome')='admitted_unpublished' ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		var detail map[string]any
		if err := rows.Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &detail) != nil {
			t.Fatal(raw, err)
		}
		out = append(out, detail)
	}
	return out
}

// assertViewerRecoveredReady requires validated ready bytes carrying exact
// source-to-sanitized lineage for the admitted producer, released occupancy,
// no leftover daemon staging, and no new native authority or effect.
func assertViewerRecoveredReady(t *testing.T, fresh *Bridge, jobs *job.Store, source string, r job.NativeViewerSaveReservation, digest string, prepares *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	row, err := jobs.Get(ctx, r.JobID)
	if err != nil || row.State != job.StateReady {
		events, _ := jobs.Events(ctx, r.JobID)
		t.Fatalf("row=%+v err=%v events=%v", row, err, events)
	}
	if outcome, reason := fresh.nativeViewerOutcome(ctx, &nativeViewerSave{record: r, digest: digest}, false); outcome != "ready" {
		t.Fatalf("lineage proof=%s/%s", outcome, reason)
	}
	if got := viewerPermitStatus(t, jobs, r.PermitID); got != job.Settled {
		t.Fatalf("permit=%s", got)
	}
	if _, err := os.Lstat(viewerStagePath(source, r.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate stage retained: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(source, "papio", r.JobID, r.Filename()+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("part left: %v", err)
	}
	if prepares.Load() != 0 {
		t.Fatal("recovery prepared a native save")
	}
	for kind, want := range map[string]int{"browser.native_viewer_save_reserved": 1, "browser.native_viewer_save_step": 1, "browser.native_viewer_save_admitted": 1} {
		if n := nativeEventCount(t, jobs, r.JobID, kind); n != want {
			t.Fatalf("%s=%d want %d", kind, n, want)
		}
	}
	if len(viewerAbandonments(t, jobs, r.JobID)) != 0 {
		t.Fatal("recovered bytes were abandoned")
	}
}

// A genuine publication failure (an occupied final name) strands admitted
// bytes and the global lane. After the obstruction clears, a restarted daemon
// with a different browser holder publishes the exact stage to ready.
func TestNativeViewerRecoveryAfterPublicationFailure(t *testing.T) {
	for _, restart := range []bool{true, false} {
		name := "same_process"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			body := adoptionProbePDF(handoffWork().DOI)
			b, jobs, id, source, p, session := viewerFixture(t, body)
			enableAdoptionSanitization(t, b)
			p = viewerPrepare(t, b, id, p)
			viewerRequest(t, b, id, p)
			filename := "papio-viewer-" + p.OperationID + ".pdf"
			obstruction := filepath.Join(source, "papio", id, filename)
			if err := os.MkdirAll(obstruction, 0o700); err != nil {
				t.Fatal(err)
			}
			p.RequestID = "advance-2"
			if got := viewerRequest(t, b, id, p); got.Outcome != "unavailable" || got.Reason != "source_rejected" {
				t.Fatal(got)
			}
			b.mu.Lock()
			record := b.nativeViewerSaves[id].record
			b.mu.Unlock()
			if got := viewerPermitStatus(t, jobs, record.PermitID); got != job.UnknownCompletion {
				t.Fatalf("permit=%s", got)
			}
			if err := os.Remove(obstruction); err != nil {
				t.Fatal(err)
			}
			recoverer, prepares := b, new(atomic.Int32)
			if restart {
				recoverer, prepares = freshViewerRecoveryBridge(t, b)
				recoverer.mu.Lock()
				generation := recoverer.arbitration.generation()
				recoverer.mu.Unlock()
				if generation == record.HolderGeneration {
					t.Fatal("test did not change the browser holder")
				}
			}
			if err := recoverer.SweepAdoptions(ctx); err != nil {
				t.Fatal(err)
			}
			assertViewerRecoveredReady(t, recoverer, jobs, source, record, viewerSHA(body), prepares)
			row, _ := jobs.Get(ctx, id)
			if row.ArtifactSHA256 == viewerSHA(body) {
				t.Fatal("test did not sanitize")
			}
			if session.advances.Load() != 1 {
				t.Fatal("native Save repeated")
			}
			if !restart {
				p.RequestID = "advance-3"
				if got := viewerRequest(t, b, id, p); got.Outcome != "ready" {
					t.Fatalf("receipt=%+v", got)
				}
				if session.advances.Load() != 1 {
					t.Fatal("native Save repeated after recovery")
				}
			}
		})
	}
}

func TestNativeViewerRecoveryCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_admission", "after_admission", "mid_publication", "after_link"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			body := adoptionProbePDF(handoffWork().DOI)
			b, jobs, id, source, p, _ := viewerFixture(t, body)
			r := admitViewerStage(t, b, jobs, id, source, p, body, boundary != "before_admission")
			jobDir := filepath.Join(source, "papio", id)
			final := filepath.Join(jobDir, r.Filename())
			switch boundary {
			case "mid_publication":
				if err := os.MkdirAll(jobDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(final+".part", body[:len(body)/2], 0o600); err != nil {
					t.Fatal(err)
				}
			case "after_link":
				if err := os.MkdirAll(jobDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(final+".part", body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(final+".part", final); err != nil {
					t.Fatal(err)
				}
			}
			fresh, prepares := freshViewerRecoveryBridge(t, b)
			for range 2 {
				if err := fresh.SweepAdoptions(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if boundary != "before_admission" {
				assertViewerRecoveredReady(t, fresh, jobs, source, r, viewerSHA(body), prepares)
				return
			}
			// Unadmitted staging is never daemon-owned bytes: nothing publishes,
			// abandons or settles, and the uncertain step keeps its occupancy.
			row, _ := jobs.Get(ctx, id)
			if row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" {
				t.Fatal(row)
			}
			if got := viewerPermitStatus(t, jobs, r.PermitID); got != job.Held {
				t.Fatalf("permit=%s", got)
			}
			if got, err := os.ReadFile(viewerStagePath(source, r.OperationID)); err != nil || !bytes.Equal(got, body) {
				t.Fatal("unadmitted stage touched", err)
			}
			if _, err := os.Lstat(jobDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unadmitted stage published", err)
			}
			if len(viewerAbandonments(t, jobs, id)) != 0 || prepares.Load() != 0 {
				t.Fatal("unadmitted stage handled")
			}
		})
	}
}

// Permanently unpublishable evidence releases exactly its own occupancy with a
// durable reason, keeps whatever file is there, leaves the operator's action
// untouched, and never accepts different bytes or a changed authority.
func TestNativeViewerRecoveryRefusesAlteredOrStaleStage(t *testing.T) {
	for _, tc := range []struct{ change, reason string }{
		{"tampered", job.NativeViewerStageRejected},
		{"symlink", job.NativeViewerStageRejected},
		{"hardlink", job.NativeViewerStageRejected},
		{"directory", job.NativeViewerStageRejected},
		{"missing", job.NativeViewerStageMissing},
		{"final_occupied", job.NativeViewerPublicationBlocked},
		{"foreign_part", job.NativeViewerPublicationBlocked},
		{"cancelled", job.NativeViewerAuthorityLost},
		{"revision", job.NativeViewerAuthorityLost},
		{"retry", job.NativeViewerAuthorityLost},
		{"budget", job.NativeViewerPublicationFailed},
	} {
		t.Run(tc.change, func(t *testing.T) {
			ctx := context.Background()
			body := adoptionProbePDF(handoffWork().DOI)
			b, jobs, id, source, p, _ := viewerFixture(t, body)
			r := admitViewerStage(t, b, jobs, id, source, p, body, true)
			stage := viewerStagePath(source, r.OperationID)
			jobDir := filepath.Join(source, "papio", id)
			altered := bytes.Clone(body)
			altered[len(altered)/2] ^= 0xff
			switch tc.change {
			case "tampered":
				if err := os.WriteFile(stage, altered, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				elsewhere := filepath.Join(source, "papio", "exact-copy.bin")
				if err := os.Rename(stage, elsewhere); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("exact-copy.bin", stage); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(stage, filepath.Join(source, "papio", "second-name.bin")); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(stage); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(stage, 0o700); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(stage); err != nil {
					t.Fatal(err)
				}
			case "final_occupied":
				if err := os.MkdirAll(jobDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jobDir, r.Filename()), altered, 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign_part":
				// Not a prefix of the admitted bytes, so not provably ours.
				if err := os.MkdirAll(jobDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jobDir, r.Filename()+".part"), altered[:len(altered)/2+8], 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if err := jobs.Cancel(ctx, id, job.TerminalReasonCancelledByUser); err != nil {
					t.Fatal(err)
				}
			case "revision":
				if _, err := jobs.S.DB().Exec(`UPDATE human_actions SET revision=revision+1 WHERE id=?`, p.ActionID); err != nil {
					t.Fatal(err)
				}
			case "retry":
				if err := jobs.S.AppendEvent(ctx, id, "job.retry_requested", map[string]any{"reason": "test"}); err != nil {
					t.Fatal(err)
				}
			case "budget":
				for range nativeViewerRecoveryBudget {
					if err := jobs.S.AppendEvent(ctx, id, "browser.adoption_deferred", map[string]any{"filename": r.Filename(), "reason": "test"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, _ := os.Lstat(stage)
			fresh, prepares := freshViewerRecoveryBridge(t, b)
			for range 2 {
				if err := fresh.SweepAdoptions(ctx); err != nil {
					t.Fatal(err)
				}
			}
			abandoned := viewerAbandonments(t, jobs, id)
			if len(abandoned) != 1 || abandoned[0]["reason"] != tc.reason || abandoned[0]["sha256"] != viewerSHA(body) {
				t.Fatalf("abandonment=%v", abandoned)
			}
			if got := viewerPermitStatus(t, jobs, r.PermitID); got != job.Settled {
				t.Fatalf("permit=%s", got)
			}
			row, _ := jobs.Get(ctx, id)
			if row.State == job.StateReady || row.ArtifactSHA256 != "" {
				t.Fatalf("altered or stale bytes accepted: %+v", row)
			}
			// Whatever sat at the stage name is kept, untouched, and named.
			after, err := os.Lstat(stage)
			if tc.change == "missing" {
				if !errors.Is(err, os.ErrNotExist) || abandoned[0]["retained_stage"] != nil {
					t.Fatal(err, abandoned[0])
				}
			} else if err != nil || !os.SameFile(before, after) || abandoned[0]["retained_stage"] == nil {
				t.Fatal("stage not retained", err, abandoned[0])
			}
			if tc.change == "tampered" {
				if got, _ := os.ReadFile(stage); !bytes.Equal(got, altered) {
					t.Fatal("tampered evidence rewritten")
				}
			}
			if tc.change == "foreign_part" {
				if got, _ := os.ReadFile(filepath.Join(jobDir, r.Filename()+".part")); !bytes.Equal(got, altered[:len(altered)/2+8]) {
					t.Fatal("unproven part file deleted or rewritten")
				}
			}
			if tc.change != "final_occupied" {
				if _, err := os.Lstat(filepath.Join(jobDir, r.Filename())); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unrecoverable stage published", err)
				}
			}
			if tc.reason != job.NativeViewerAuthorityLost {
				// The operator's original action stays open; recovery never
				// revises it, so it cannot re-arm a native save either.
				actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
				if err != nil || len(actions) != 1 || actions[0].ID != p.ActionID || actions[0].Revision != p.ActionRevision {
					t.Fatal(actions, err)
				}
			}
			if prepares.Load() != 0 || nativeEventCount(t, jobs, id, "browser.native_viewer_save_step") != 1 {
				t.Fatal("recovery created native authority")
			}
		})
	}
}

// An infrastructure failure is retried with a durable explanation and pacing;
// it neither abandons the bytes nor repeats a native call.
func TestNativeViewerRecoveryTransientPublicationRetry(t *testing.T) {
	ctx := context.Background()
	body := adoptionProbePDF(handoffWork().DOI)
	b, jobs, id, source, p, _ := viewerFixture(t, body)
	r := admitViewerStage(t, b, jobs, id, source, p, body, true)
	jobDir := filepath.Join(source, "papio", id)
	if err := os.Mkdir(jobDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobDir, 0o700) })
	fresh, prepares := freshViewerRecoveryBridge(t, b)
	clock := time.Now()
	fresh.now = func() time.Time { return clock }
	for range 2 {
		if err := fresh.SweepAdoptions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := nativeEventCount(t, jobs, id, "browser.adoption_deferred"); n != 1 {
		t.Fatalf("deferred events=%d, want one paced explanation", n)
	}
	if got := viewerPermitStatus(t, jobs, r.PermitID); got != job.Held || len(viewerAbandonments(t, jobs, id)) != 0 {
		t.Fatalf("transient failure abandoned bytes: %s", got)
	}
	if err := os.Chmod(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3 * time.Second)
	if err := fresh.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	assertViewerRecoveredReady(t, fresh, jobs, source, r, viewerSHA(body), prepares)
}

// Only a running worker owns admitted staging. An idle admitted record that
// never reached a terminal outcome has no worker and must not strand it.
func TestNativeViewerRecoveryRespectsOnlyARunningWorker(t *testing.T) {
	ctx := context.Background()
	body := adoptionProbePDF(handoffWork().DOI)
	b, jobs, id, source, p, _ := viewerFixture(t, body)
	r := admitViewerStage(t, b, jobs, id, source, p, body, true)
	workerCtx, cancel := context.WithCancel(ctx)
	record := &nativeViewerSave{record: r, admitted: true, busy: true, digest: viewerSHA(body), ctx: workerCtx, cancel: cancel}
	b.mu.Lock()
	b.nativeViewerSaves = map[string]*nativeViewerSave{id: record}
	b.mu.Unlock()
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(viewerStagePath(source, r.OperationID)); err != nil {
		t.Fatal("recovery raced a running worker", err)
	}
	if row, _ := jobs.Get(ctx, id); row.State != job.StateAwaitingHuman {
		t.Fatal(row)
	}
	b.mu.Lock()
	record.busy = false
	b.mu.Unlock()
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatal(err)
	}
	assertViewerRecoveredReady(t, b, jobs, source, r, viewerSHA(body), new(atomic.Int32))
	b.mu.Lock()
	finished := record.terminal && record.outcome == "ready" && record.ctx.Err() != nil
	b.mu.Unlock()
	if !finished {
		t.Fatal("idle same-process record left non-terminal")
	}
}
