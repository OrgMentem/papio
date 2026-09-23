// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
)

func TestNativeViewerAdoptionRequiresDurableAdmission(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_native_unadmitted")
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "papio-viewer-"+job.NewID("viewer")+".pdf")
	if err := os.WriteFile(path, pdfBytes("unadmitted native save"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptDownload(context.Background(), id, path); err == nil {
		t.Fatal("unadmitted native save bypassed its action binding through ordinary adoption")
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateAwaitingHuman || row.ArtifactSHA256 != "" {
		t.Fatalf("unadmitted result = %+v, %v", row, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refusal discarded source evidence: %v", err)
	}
}

func TestNativeViewerAdoptionAdmittedBytesReachReady(t *testing.T) {
	t.Run("direct", func(t *testing.T) { nativeViewerAdoptionReady(t, false) })
	t.Run("validation infrastructure retry", func(t *testing.T) { nativeViewerAdoptionReady(t, true) })
}

func nativeViewerAdoptionReady(t *testing.T, failFirstValidation bool) {
	t.Helper()
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Validate = passValidation()
	id := parkAwaitingHuman(t, jobs, "wr_native_admitted")
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
	generation, err := jobs.NextMaterializationHolderGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r, err := jobs.ReserveNativeViewerSave(ctx, job.NativeViewerSaveInput{
		RequestID: "prepare-app", OperationID: job.NewID("viewer"), JobID: id,
		ActionID: actions[0].ID, ActionRevision: actions[0].Revision, JobAttemptRevision: 1,
		HolderGeneration: generation, BindingSHA256: strings.Repeat("a", 64),
		SafetyDomainID: "fixture", ExpiresAtMS: now.Add(time.Minute).UnixMilli(), ExplicitSelection: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.BeginNativeViewerSaveStep(ctx, r, "advance-app", now); err != nil {
		t.Fatal(err)
	}
	body := pdfBytes("native adopted")
	digest := sha256.Sum256(body)
	if err := jobs.AdmitNativeViewerSave(ctx, r, r.Filename(), hex.EncodeToString(digest[:]), int64(len(body)), now); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, r.Filename())
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if failFirstValidation {
		if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER fail_native_validation BEFORE INSERT ON attempts WHEN NEW.stage='validate' BEGIN SELECT RAISE(FAIL, 'transient validation infrastructure failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := svc.AdoptDownload(ctx, id, path); err == nil {
			t.Fatal("expected infrastructure failure")
		}
		parked, err := jobs.Get(ctx, id)
		if err != nil || parked.State != job.StateAwaitingHuman {
			t.Fatalf("failed validation did not preserve parked work: %+v %v", parked, err)
		}
		if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_native_validation`); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AdoptDownload(ctx, id, path); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil || row.State != job.StateReady || row.ArtifactSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("admitted result=%+v err=%v", row, err)
	}
	if err := svc.Artifacts.Verify(row.ArtifactSHA256); err != nil {
		t.Fatal(err)
	}
}
