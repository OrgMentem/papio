// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/protocol"
)

type manifestSubmitterFunc func(context.Context, protocol.WorkRequest, *bool) (string, error)

func (f manifestSubmitterFunc) SubmitWithAutoImport(ctx context.Context, request protocol.WorkRequest, auto *bool) (string, error) {
	return f(ctx, request, auto)
}

func TestRunnerAcquireInitialManifestFailureDoesNotSubmit(t *testing.T) {
	ctx := context.Background()
	pager := &fakePager{order: []string{"openalex"}, results: map[string][]discovery.DiscoveredWork{
		"openalex": papers("manifest", 0, 1),
	}}
	h := newPagedHarness(t, ModeAcquire, pager, nil)
	if err := os.WriteFile(filepath.Join(h.runner.DataDir, "batches"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := h.runner.Run(ctx, h.watch.ID)
	if err == nil || result.Queued != 0 || result.ManifestID != "" || len(h.submitter.calls) != 0 {
		t.Fatalf("Run() = %+v, %v; submissions = %+v; want failure before any submission", result, err, h.submitter.calls)
	}
	coverage, err := h.watches.ScanCoverage(ctx, h.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 0 {
		t.Fatalf("coverage = %+v, want no progress after initial manifest failure", coverage)
	}
}

func TestRunnerAcquireManifestWriteFailureResumesDurableSubmissions(t *testing.T) {
	ctx := context.Background()
	pager := &fakePager{order: []string{"openalex"}, results: map[string][]discovery.DiscoveredWork{
		"openalex": papers("manifest", 0, 3),
	}}
	h := newPagedHarness(t, ModeAcquire, pager, nil)
	batches := filepath.Join(h.runner.DataDir, "batches")
	backup := filepath.Join(h.runner.DataDir, "saved-batches")
	h.runner.Submitter = manifestSubmitterFunc(func(ctx context.Context, request protocol.WorkRequest, auto *bool) (string, error) {
		manifest, err := batch.Load(h.runner.DataDir, "latest")
		if err != nil {
			t.Fatalf("manifest before submission: %v", err)
		}
		if len(manifest.Works) != 3 {
			t.Fatalf("manifest before submission = %+v, want all selected works", manifest.Works)
		}
		switch len(h.submitter.calls) {
		case 0:
			for _, entry := range manifest.Works {
				if entry.JobID != "" || entry.RequestID == "" || entry.Work.RequestID != entry.RequestID {
					t.Fatalf("initial manifest entry = %+v, want resumable work without a job", entry)
				}
			}
		case 1:
			first := h.submitter.calls[0]
			if manifest.Works[0].JobID != h.submitter.byRequest[first.RequestID] || manifest.Works[1].JobID != "" {
				t.Fatalf("incremental manifest = %+v, want the first job durable before the second submission", manifest.Works)
			}
			// Simulate storage becoming unavailable after the second job is
			// created. Keep the previous durable directory for recovery.
			if err := os.Rename(batches, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(batches, []byte("storage unavailable"), 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("runner submitted another work after a manifest write failure")
		}
		return h.submitter.SubmitWithAutoImport(ctx, request, auto)
	})

	first, err := h.runner.Run(ctx, h.watch.ID)
	if err == nil || first.Queued != 2 || len(h.submitter.calls) != 2 {
		t.Fatalf("first Run() = %+v, %v; submissions = %+v; want a write failure after two jobs", first, err, h.submitter.calls)
	}
	coverage, err := h.watches.ScanCoverage(ctx, h.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 0 {
		t.Fatalf("coverage = %+v, want no progress after incremental manifest failure", coverage)
	}
	if err := os.Remove(batches); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, batches); err != nil {
		t.Fatal(err)
	}
	durable, err := batch.Load(h.runner.DataDir, first.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	firstRequestID := h.submitter.calls[0].RequestID
	if durable.Works[0].JobID != h.submitter.byRequest[firstRequestID] || durable.Works[1].JobID != "" || durable.Works[2].JobID != "" {
		t.Fatalf("durable manifest = %+v, want only the first job recorded", durable.Works)
	}

	// A new runner retries the same batch in a different result order. The
	// first work's durable JobID survives, and its request ID reconciles that
	// live job to itself rather than creating a second one.
	works := pager.results["openalex"]
	pager.results["openalex"] = []discovery.DiscoveredWork{works[2], works[1], works[0]}
	retryCalls := 0
	restarted := &Runner{
		Store: h.watches, Discovery: pager, Lookup: h.runner.Lookup,
		Submitter: manifestSubmitterFunc(func(ctx context.Context, request protocol.WorkRequest, auto *bool) (string, error) {
			// The rewritten manifest must carry the durable JobID before the
			// retry submits anything, or an interrupted retry loses the job.
			if retryCalls == 0 {
				manifest, err := batch.Load(h.runner.DataDir, first.ManifestID)
				if err != nil {
					t.Fatalf("retry manifest before submission: %v", err)
				}
				carried := ""
				for _, entry := range manifest.Works {
					if entry.RequestID == firstRequestID {
						carried = entry.JobID
					}
				}
				if carried != durable.Works[0].JobID {
					t.Fatalf("retry manifest carried %q for %q, want the durable job %q", carried, firstRequestID, durable.Works[0].JobID)
				}
			}
			retryCalls++
			return h.submitter.SubmitWithAutoImport(ctx, request, auto)
		}),
		DataDir: h.runner.DataDir,
		Now:     func() time.Time { return h.now.Add(time.Hour) },
	}
	second, err := restarted.Run(ctx, h.watch.ID)
	if err != nil || second.Queued != 3 || second.Failed != 0 || second.ManifestID != first.ManifestID {
		t.Fatalf("restarted Run() = %+v, %v; want the completed original batch", second, err)
	}
	if len(h.submitter.byRequest) != 3 {
		t.Fatalf("jobs = %+v, want one job per request ID", h.submitter.byRequest)
	}
	if got := h.submitter.byRequest[firstRequestID]; got != durable.Works[0].JobID {
		t.Fatalf("first job = %q, want the durable job %q reconciled, not replaced", got, durable.Works[0].JobID)
	}
	completed, err := batch.Load(restarted.DataDir, second.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range completed.Works {
		if entry.JobID != h.submitter.byRequest[entry.RequestID] || entry.JobID == "" || entry.Status != "submitted" || entry.Error != "" {
			t.Fatalf("completed manifest entry = %+v, want the original job with successful submission", entry)
		}
	}
	coverage, err = h.watches.ScanCoverage(ctx, h.watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 1 || coverage["openalex"].State != string(discovery.PageExhausted) {
		t.Fatalf("coverage = %+v, want progress only after the completed batch", coverage)
	}
}
