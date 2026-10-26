// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/filing"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

func managedFixture(t *testing.T) (*app.Service, string, string, string) {
	t.Helper()
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
	jobs := &job.Store{S: db}
	svc := app.New(cfg, jobs, artifacts, nil)
	svc.ManagedFilingFolder = filepath.Join(t.TempDir(), "collection")
	id, err := jobs.CreateRequest(ctx, "wr_managed_io", work.Work{Title: "Managed I/O", DOI: "10.1000/managed-io"}, "", "", job.Policy{
		AccessMode:     config.ModeConservative,
		DesiredVersion: "any",
		FetchMaxBytes:  1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	body := []byte("%PDF-1.4\nmanaged I/O fixture\n%%EOF")
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	source := filepath.Join(t.TempDir(), "artifact.pdf")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := artifacts.Promote(source, sha); err != nil {
		t.Fatal(err)
	}
	source, err = artifacts.ArtifactPath(sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.UpsertArtifact(ctx, job.Artifact{SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", IdentityResult: "pass", Path: source}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	return svc, id, sha, source
}

func TestManagedPublishDoesNotHoldWriter(t *testing.T) {
	for _, archive := range []bool{false, true} {
		name := "receipt"
		if archive {
			name = "archive_compensation"
		}
		t.Run(name, func(t *testing.T) {
			svc, id, _, _ := managedFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			filing.SetBeforeIOForTest(t, func(op string) {
				if op == "link" {
					close(entered)
					<-release
				}
			})
			var got app.ManagedFiling
			var fileErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				got, fileErr = svc.RetryManagedJob(context.Background(), id, "")
			}()
			defer func() {
				unblock()
				<-done
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("publication did not reach link")
			}
			writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if archive {
				if _, err := svc.Jobs.Archive(writeCtx, id, true); err != nil {
					t.Fatalf("archive blocked by publication: %v", err)
				}
			} else {
				if _, err := svc.Jobs.S.DB().ExecContext(writeCtx, `INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'test.writer', '{}')`, id, store.Now()); err != nil {
					t.Fatalf("unrelated write blocked by publication: %v", err)
				}
			}
			unblock()
			<-done
			if archive {
				if !errors.Is(fileErr, job.ErrConflict) {
					t.Fatalf("archived publication = %+v, %v; want conflict", got, fileErr)
				}
				entries, err := os.ReadDir(svc.ManagedFilingFolder)
				if err != nil || len(entries) != 0 {
					t.Fatalf("archived publication leaves files = %v, %v", entries, err)
				}
			} else if fileErr != nil || got.State != "filed" || got.ReceiptPath == "" {
				t.Fatalf("publication = %+v, %v", got, fileErr)
			}
		})
	}
}

func archivedPublishFixture(t *testing.T) (*app.Service, string, string) {
	t.Helper()
	ctx := context.Background()
	svc, id, sha, source := managedFixture(t)
	destination := svc.ManagedFilingFolder
	key := filing.Key(id, destination, sha)
	now := store.Now()
	if _, err := svc.Jobs.S.DB().ExecContext(ctx, `INSERT INTO managed_filings(job_id, destination, idempotency_key, artifact_sha256, state, attempts, retry_at, created_at, updated_at) VALUES(?, ?, ?, ?, 'pending', 1, ?, ?, ?)`, id, destination, key, sha, store.FormatTime(time.Now().Add(time.Hour)), now, now); err != nil {
		t.Fatal(err)
	}
	staged, err := filing.Stage(ctx, source, destination, key, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Jobs.Archive(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	// Crash before receipt or compensation: retain the link, partial, and
	// pending journal, but discard the service's in-memory state.
	restarted := app.New(svc.Config, svc.Jobs, svc.Artifacts, nil)
	restarted.ManagedFilingFolder = destination
	return restarted, id, staged.Target
}

func assertArchivedFilingSettled(t *testing.T, svc *app.Service, id, target string) {
	t.Helper()
	rows, _, err := svc.ManagedFilings(context.Background(), id, 10)
	if err != nil || len(rows) != 1 || rows[0].State != "failed" || rows[0].ErrorCode != "archived" || rows[0].RetryAt != "" || rows[0].ReceiptPath != "" {
		t.Fatalf("archived journal = %+v, %v; want settled compensation", rows, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("archived publication remains: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 0 {
		t.Fatalf("archived publication leaves files = %v, %v", entries, err)
	}
}

func TestManagedMaintenanceRecoversArchivedPublicationAfterCrash(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "configured"
		if !enabled {
			name = "filing_disabled"
		}
		t.Run(name, func(t *testing.T) {
			svc, id, target := archivedPublishFixture(t)
			if !enabled {
				svc.ManagedFilingFolder = ""
			}
			if err := svc.ManagedFilingRetrier().RunDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertArchivedFilingSettled(t, svc, id, target)
		})
	}
}

func TestManagedMaintenanceRetriesFailedArchivedUndo(t *testing.T) {
	svc, id, target := archivedPublishFixture(t)
	destination := filepath.Dir(target)
	offline := destination + ".offline"
	var moveErr error
	moved := false
	filing.SetBeforeIOForTest(t, func(op string) {
		if op == "undo_remove" && !moved {
			// Lose the mounted path after verification, before unlink. This
			// causes a real removal error without relying on user privileges.
			moveErr = os.Rename(destination, offline)
			moved = moveErr == nil
		}
	})
	ctx := context.Background()
	err := svc.ManagedFilingRetrier().RunDue(ctx)
	if moved {
		if err := os.Rename(offline, destination); err != nil {
			t.Fatal(err)
		}
	}
	if err != nil || moveErr != nil || !moved {
		t.Fatalf("first compensation = %v, move = %v, moved = %v", err, moveErr, moved)
	}
	rows, _, err := svc.ManagedFilings(ctx, id, 10)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" || rows[0].ErrorCode != "destination_unavailable" || rows[0].ReceiptPath != "" {
		t.Fatalf("failed undo journal = %+v, %v; want pending compensation", rows, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("failed undo lost the publication: %v", err)
	}
	if err := svc.ManagedFilingRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	assertArchivedFilingSettled(t, svc, id, target)
}

func TestManagedMaintenancePreservesDifferentArchivedTarget(t *testing.T) {
	svc, id, target := archivedPublishFixture(t)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	const unrelated = "a different document"
	if err := os.WriteFile(target, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.ManagedFilingRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != unrelated {
		t.Fatalf("compensation changed an unrelated target: %q, %v", body, err)
	}
	rows, _, err := svc.ManagedFilings(ctx, id, 10)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" || rows[0].ErrorCode != "destination_conflict" {
		t.Fatalf("conflicting undo journal = %+v, %v", rows, err)
	}
}
