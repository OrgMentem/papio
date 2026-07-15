// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

func TestSchedulerSweepsOnlyTerminalQuarantine(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	artifacts, err := artifact.New(dataDir)
	if err != nil {
		t.Fatalf("create artifact layout: %v", err)
	}
	policy := job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}
	workItem := work.Work{DOI: "10.1000/scheduler-sweep", Title: "Scheduler sweep", Authors: []string{"A"}, Year: 2026}

	terminalID, err := jobs.CreateRequest(ctx, "scheduler-sweep-terminal-0001", workItem, "", "", policy, nil)
	if err != nil {
		t.Fatalf("create terminal job: %v", err)
	}
	if err := jobs.Transition(ctx, terminalID, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatalf("terminal queued->resolving: %v", err)
	}
	if err := jobs.Transition(ctx, terminalID, job.StateResolving, job.StateUnavailable, nil, job.WithTerminalReason("fixture")); err != nil {
		t.Fatalf("terminal resolving->unavailable: %v", err)
	}

	reviewID, err := jobs.CreateRequest(ctx, "scheduler-sweep-review-0001", workItem, "", "", policy, nil)
	if err != nil {
		t.Fatalf("create review job: %v", err)
	}
	if err := jobs.Transition(ctx, reviewID, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatalf("review queued->resolving: %v", err)
	}
	if err := jobs.Transition(ctx, reviewID, job.StateResolving, job.StateNeedsReview, nil); err != nil {
		t.Fatalf("review resolving->needs_review: %v", err)
	}
	if _, err := jobs.OpenHumanAction(ctx, reviewID, "verify_identity", "inspect quarantine PDF"); err != nil {
		t.Fatalf("open review action: %v", err)
	}

	writeQuarantine := func(jobID, name string) string {
		t.Helper()
		dir, err := artifacts.QuarantineDir(jobID)
		if err != nil {
			t.Fatalf("create quarantine for %s: %v", jobID, err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write quarantine %s: %v", name, err)
		}
		return path
	}
	terminalTemp := writeQuarantine(terminalID, "interrupted.tmp")
	reviewPDF := writeQuarantine(reviewID, "identity-review.pdf")

	scheduler, err := NewScheduler(jobs, ProcessorFunc(func(context.Context, *job.Row) error {
		t.Fatal("scheduler claimed a terminal or human-parked job")
		return nil
	}), SchedulerConfig{
		Owner: "quarantine-sweep", Workers: 1, LeaseDuration: 100 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(runCtx) }()

	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(terminalTemp)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			cancel()
			<-done
			t.Fatalf("stat terminal quarantine: %v", err)
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("scheduler did not sweep terminal quarantine")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(reviewPDF); err != nil {
		cancel()
		<-done
		t.Fatalf("scheduler removed needs-review quarantine: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("scheduler shutdown: %v", err)
	}
}
