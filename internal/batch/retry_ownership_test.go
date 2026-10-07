// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package batch

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

func TestScopedRetryRetainsJobsAfterWorksBecomeOwned(t *testing.T) {
	for _, receipt := range []string{"manifest", "store", "converged_receipt"} {
		t.Run(receipt, func(t *testing.T) {
			var dataDir string
			if receipt == "manifest" {
				dataDir = t.TempDir()
			} else {
				dataDir = storetest.DataDir(t)
			}
			now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
			works := []protocol.WorkRequest{holdingsRequest("10.1000/campaign-retry")}
			manifest := NewManifest(works, "", "", now)
			manifest.ID = ScopedID(works, now, "campaign-fixture")
			manifest.Works[0].RequestID = RequestID(manifest.ID, works[0])
			manifest.Works[0].Work.RequestID = manifest.Works[0].RequestID
			jobID := "job-original"
			if receipt == "manifest" {
				manifest.Works[0].JobID = jobID
			} else {
				ctx := context.Background()
				db, err := store.Open(ctx, dataDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				jobs := &job.Store{S: db}
				requestID := manifest.Works[0].RequestID
				if receipt == "converged_receipt" {
					requestID = "another-request"
				}
				policy := job.Policy{AccessMode: "conservative", DesiredVersion: "any"}
				candidate := work.Work{DOI: works[0].Identifiers.DOI}
				created, err := jobs.CreateRequestForWork(ctx, requestID, candidate, "", "", policy, nil, job.Attribution{}, false)
				if err != nil {
					t.Fatal(err)
				}
				jobID = created.JobID
				if receipt == "converged_receipt" {
					joined, err := jobs.CreateOnceRequestForWork(ctx, manifest.Works[0].RequestID, candidate, "", "", policy, nil, job.Attribution{})
					if err != nil || !joined.Existing || joined.JobID != jobID {
						t.Fatalf("once-only convergence = %+v, %v", joined, err)
					}
				}
			}
			if err := Write(dataDir, manifest); err != nil {
				t.Fatal(err)
			}
			caller := &holdingsCaller{result: ownership.Result{
				Works:   []ownership.WorkResult{heldClaim("10.1000/campaign-retry")},
				Sources: []ownership.SourceHealth{{Name: "papis", Complete: true}},
			}}
			output, err := Submit(context.Background(), caller, dataDir, works, SubmitOptions{
				Now: now, IdentityScope: "campaign-fixture", Holdings: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(output.Submitted) != 1 || output.Submitted[0].JobID != jobID || output.Submitted[0].State != "queued" {
				t.Fatalf("retry lost its active job association: %+v", output)
			}
			if len(output.SkippedOwned) != 0 || caller.called("acquire.submit_v2") {
				t.Fatalf("retry replaced an existing submission with current ownership: %+v", output)
			}
			stored, err := Load(dataDir, manifest.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Works[0].JobID != jobID {
				t.Fatalf("durable job receipt = %q, want %q", stored.Works[0].JobID, jobID)
			}
		})
	}
}
