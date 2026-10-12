// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/filing"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

func readyManagedFixture(t *testing.T, svc *Service, request, body string) string {
	t.Helper()
	ctx := context.Background()
	id := createFilingStateJob(t, svc.Jobs, request, job.StateResolving)
	sha := seedReadyArtifact(t, svc, svc.Jobs, id, body)
	path, err := svc.Artifacts.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Jobs.UpsertArtifact(ctx, job.Artifact{SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", IdentityResult: "pass", Path: path}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestManagedFolderTransientFailureRestartAndExactlyOneArtifactPerJob(t *testing.T) {
	ctx := context.Background()
	data := storetest.DataDir(t)
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	artifacts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = data
	cfg.Browser.AdoptionRoot = filepath.Join(data, "adoptions")
	svc := New(cfg, &job.Store{S: db}, artifacts, nil)
	now := time.Now().UTC()
	svc.Now = func() time.Time { return now }
	folder := filepath.Join(t.TempDir(), "collection")
	svc.ManagedFilingFolder = folder
	bodies := []string{"%PDF-1.4\nfirst fixture\n%%EOF", "%PDF-1.4\nsecond fixture\n%%EOF"}
	ids := []string{readyManagedFixture(t, svc, "wr_managed_one", bodies[0]), readyManagedFixture(t, svc, "wr_managed_two", bodies[1])}
	// A regular file temporarily occupies the destination directory.
	if err := os.WriteFile(folder, []byte("temporary destination outage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.RetryManagedFiling(ctx); err != nil {
		t.Fatal(err)
	}
	failed, truncated, err := svc.ManagedFilings(ctx, "", 10)
	if err != nil || truncated || len(failed) != 2 {
		t.Fatalf("failed receipts = %+v, %v, %v", failed, truncated, err)
	}
	for _, f := range failed {
		if f.State != "failed" || f.Attempts != 1 || f.ErrorCode != "destination_unavailable" || f.RetryAt == "" || f.ReceiptPath != "" {
			t.Fatalf("failed state = %+v", f)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	svc = New(cfg, &job.Store{S: db}, artifacts, nil)
	svc.ManagedFilingFolder = folder
	now = now.Add(2 * time.Minute)
	svc.Now = func() time.Time { return now }
	if err := svc.RecoverPreparedPublications(ctx); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		rows, _, err := svc.ManagedFilings(ctx, id, 10)
		if err != nil || len(rows) != 1 || rows[0].State != "filed" || rows[0].Attempts != 2 || rows[0].RetryAt != "" {
			t.Fatalf("restart receipt = %+v, %v", rows, err)
		}
		body, err := os.ReadFile(rows[0].ReceiptPath)
		if err != nil || !bytes.Equal(body, []byte(bodies[i])) {
			t.Fatalf("filed bytes = %q, %v", body, err)
		}
		if _, err := svc.RetryManagedJob(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RetryManagedFiling(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) != 2 {
		t.Fatalf("folder contains %v, %v; want exactly two final artifacts", entries, err)
	}
	for _, f := range failed {
		rows, _, err := svc.ManagedFilings(ctx, f.JobID, 10)
		if err != nil || rows[0].Attempts != 2 {
			t.Fatalf("safe replay spent another attempt: %+v, %v", rows, err)
		}
	}
}

func TestManagedFolderRecoversPublishBeforeReceiptAndRejectsArchived(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.ManagedFilingFolder = filepath.Join(t.TempDir(), "collection")
	id := readyManagedFixture(t, svc, "wr_receipt_gap", "%PDF-1.4\nreceipt gap\n%%EOF")
	if err := svc.queueManagedFiling(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows, _, err := svc.ManagedFilings(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	f := rows[0]
	source, err := svc.Artifacts.ArtifactPath(f.ArtifactSHA256)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := filing.Stage(ctx, source, f.Destination, f.IdempotencyKey, f.ArtifactSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(staged.Cleanup)
	// The file exists, but no receipt commits before the simulated restart.
	cfg := svc.Config
	restarted := New(cfg, jobs, svc.Artifacts, nil)
	restarted.ManagedFilingFolder = svc.ManagedFilingFolder
	got, err := restarted.RetryManagedJob(ctx, id, "")
	if err != nil || got.State != "filed" || got.ReceiptPath != staged.Target {
		t.Fatalf("receipt recovery = %+v, %v", got, err)
	}
	other := readyManagedFixture(t, restarted, "wr_archived_managed", "%PDF-1.4\narchived\n%%EOF")
	if err := restarted.queueManagedFiling(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Archive(ctx, other, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RetryManagedFiling(ctx); err != nil {
		t.Fatal(err)
	}
	archived, _, err := restarted.ManagedFilings(ctx, other, 10)
	if err != nil || len(archived) != 1 || archived[0].State != "pending" || archived[0].Attempts != 0 || archived[0].Disposition != job.DispositionArchived {
		t.Fatalf("archived journal = %+v, %v", archived, err)
	}
	entries, err := os.ReadDir(svc.ManagedFilingFolder)
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive produced another artifact: %v, %v", entries, err)
	}
}

func TestManagedFolderChangeDoesNotRetryHistoricalDestination(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	now := time.Now().UTC()
	svc.Now = func() time.Time { return now }
	oldFolder := filepath.Join(t.TempDir(), "old")
	svc.ManagedFilingFolder = oldFolder
	id := readyManagedFixture(t, svc, "wr_folder_change", "%PDF-1.4\nfolder change\n%%EOF")
	if err := os.WriteFile(oldFolder, []byte("temporary outage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.RetryManagedFiling(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldFolder); err != nil {
		t.Fatal(err)
	}
	svc.ManagedFilingFolder = filepath.Join(t.TempDir(), "new")
	now = now.Add(2 * time.Minute)
	if err := svc.RetryManagedFiling(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oldFolder); !os.IsNotExist(err) {
		t.Fatalf("automatic retry writes old destination: %v", err)
	}
	rows, _, err := svc.ManagedFilings(ctx, id, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("destination receipts = %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.Destination == oldFolder && (row.State != "failed" || row.Attempts != 1) {
			t.Fatalf("old receipt changes: %+v", row)
		}
		if row.Destination == svc.ManagedFilingFolder && row.State != "filed" {
			t.Fatalf("new destination does not file: %+v", row)
		}
	}
	deliberate, err := svc.RetryManagedJob(ctx, id, oldFolder)
	if err != nil || deliberate.State != "filed" || deliberate.Destination != oldFolder {
		t.Fatalf("explicit historical retry = %+v, %v", deliberate, err)
	}
}
