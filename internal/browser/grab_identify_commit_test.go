// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/grab"
)

func TestIdentifyGrabCommitFailureKeepsQuarantineAndParkedState(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Identify Commit Failure")
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	original, err := os.ReadFile(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_identify_commit AFTER UPDATE OF state ON pdf_grabs
		WHEN NEW.state = 'job_created'
		BEGIN SELECT RAISE(ABORT, 'injected identify commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := b.IdentifyGrab(ctx, g.ID, "doi", "10.1234/identify.commit.failure")
	if result.Outcome != "failed" {
		t.Fatalf("identify = %+v, want structured failure", result)
	}
	after, err := b.grabs.Get(ctx, g.ID)
	if err != nil || after == nil || after.State != grab.StateParkedNoIdentifier || after.JobID != "" {
		t.Fatalf("grab after failed commit = %+v, %v, want unbound parked row", after, err)
	}
	if after.QuarantinePath != quarantine {
		t.Fatalf("quarantine path changed: %q", after.QuarantinePath)
	}
	remaining, err := os.ReadFile(after.QuarantinePath)
	if err != nil || !bytes.Equal(remaining, original) {
		t.Fatalf("failed commit lost the row's captured bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.EffectiveAdoptionRoot(), result.JobID)); !os.IsNotExist(err) {
		t.Fatalf("uncommitted identification exposed an adoption directory: %v", err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER fail_identify_commit`); err != nil {
		t.Fatal(err)
	}
	retry := b.IdentifyGrab(ctx, g.ID, "doi", "10.1234/identify.commit.failure")
	if retry.Outcome != "job_created" || retry.JobID != result.JobID {
		t.Fatalf("retry = %+v, want committed original job %s", retry, result.JobID)
	}
	requireRecovered(t, jobs, retry.JobID)
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("quarantine survived successful ingestion: %v", err)
	}
}

func TestIdentifyGrabStagingFailureRecoversFromQuarantine(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := parkManualDownload(t, jobs, "wr_identify_stage_failure", handoffWork())
	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Identify Staging Failure")
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(cfg.DataDir, "quarantine", g.ID, "grab.pdf")
	writeFixturePDF(t, quarantine)
	if err := b.grabs.MarkQuarantined(ctx, g.ID, quarantine); err != nil {
		t.Fatal(err)
	}
	if err := b.grabs.MarkParkedNoIdentifier(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.EffectiveAdoptionRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(cfg.EffectiveAdoptionRoot(), id)
	if err := os.WriteFile(jobDir, []byte("block staging"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := b.IdentifyGrab(ctx, g.ID, "doi", handoffWork().DOI)
	if result.Outcome != "job_created" || result.JobID != id {
		t.Fatalf("identify = %+v, want committed job %s", result, id)
	}
	after, err := b.grabs.Get(ctx, g.ID)
	if err != nil || after == nil || after.State != grab.StateJobCreated || after.JobID != id {
		t.Fatalf("grab after staging failure = %+v, %v", after, err)
	}
	if _, err := os.Stat(after.QuarantinePath); err != nil {
		t.Fatalf("staging failure lost quarantine: %v", err)
	}
	if got := countDeferredAdoptions(t, jobs, id); got != 1 {
		t.Fatalf("deferred events = %d, want 1", got)
	}
	if err := os.Remove(jobDir); err != nil {
		t.Fatal(err)
	}
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatal(err)
	}
	requireRecovered(t, jobs, id)
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("quarantine survived successful recovery: %v", err)
	}
}
