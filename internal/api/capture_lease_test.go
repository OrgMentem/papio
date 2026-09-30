// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// breakCaptureLeaseRelease makes every captures.Store.ReleaseJob fail by
// corrupting the pending-lease index it rewrites, and returns the repair.
func breakCaptureLeaseRelease(t *testing.T, system *bootstrap.System) func() {
	t.Helper()
	root := filepath.Join(system.Config.DataDir, "captures")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, ".pending.json")
	if err := os.WriteFile(index, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := system.Captures.ReleaseJob(context.Background(), "job_probe"); err == nil {
		t.Fatal("corrupt pending index did not make ReleaseJob fail; the injection is not reaching the release")
	}
	return func() {
		if err := os.Remove(index); err != nil {
			t.Fatal(err)
		}
	}
}

// jobs.retry releases the capture lease before the job becomes active, so a
// release failure leaves the job where it was and the retry can be repeated.
// Released after the commit instead, the failure stranded the job in
// resolving with its lease pinned and no retry left to reach the release.
func TestJobsRetryReleasesTheCaptureLeaseBeforeTheJobTurnsActive(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	jobID := storeHandlerUnavailableJob(t, system, "wr_retry_lease", job.TerminalReasonCandidatesExhausted, nil)
	repair := breakCaptureLeaseRelease(t, system)

	if rpcErr := callMethod(t, router, "jobs.retry", map[string]string{"job_id": jobID}, nil); rpcErr == nil {
		t.Fatal("jobs.retry succeeded although its capture lease could not be released")
	}
	row, err := system.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateUnavailable {
		t.Fatalf("job state after a failed release = %q, want still unavailable so the retry can be repeated", row.State)
	}

	repair()
	if rpcErr := callMethod(t, router, "jobs.retry", map[string]string{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("repeated jobs.retry = %v", rpcErr)
	}
	if row, err := system.Jobs.Get(context.Background(), jobID); err != nil || row.State != job.StateResolving {
		t.Fatalf("job after the repeated retry = %+v, %v; want resolving", row, err)
	}
}

// Cancel and review resolution commit before the capture lease is released,
// and neither can be repeated to reach the release again, so a release
// failure must not turn the committed change into an RPC failure.
func TestCommittedCancelAndReviewSurviveACaptureReleaseFailure(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	ctx := context.Background()

	var submitted SubmitResult
	if rpcErr := callMethod(t, router, "acquire.submit", protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "request_cancel_lease",
		Identifiers: &protocol.Identifiers{DOI: "10.1000/cancel-lease"},
	}, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	reviewJob, err := system.Jobs.CreateRequest(ctx, "request_review_lease", work.Work{DOI: "10.1000/review-lease"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateValidating},
		{job.StateValidating, job.StateNeedsReview},
	} {
		if err := system.Jobs.Transition(ctx, reviewJob, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	actionID, err := system.Jobs.OpenHumanAction(ctx, reviewJob, "verify_identity", "local quarantine file: /tmp/review-lease.pdf", job.Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	breakCaptureLeaseRelease(t, system)

	var cancelled struct {
		JobID     string `json:"job_id"`
		Cancelled bool   `json:"cancelled"`
	}
	if rpcErr := callMethod(t, router, "jobs.cancel", map[string]string{"job_id": submitted.JobID}, &cancelled); rpcErr != nil {
		t.Fatalf("jobs.cancel with a failing capture release = %v, want the committed cancel reported", rpcErr)
	}
	if !cancelled.Cancelled {
		t.Fatalf("jobs.cancel = %+v, want cancelled", cancelled)
	}

	var resolved struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if rpcErr := callMethod(t, router, "actions.resolve", map[string]any{"action_id": actionID, "verdict": "reject"}, &resolved); rpcErr != nil {
		t.Fatalf("actions.resolve with a failing capture release = %v, want the committed resolution reported", rpcErr)
	}
	if resolved.JobID != reviewJob || resolved.State != job.StateCancelled {
		t.Fatalf("actions.resolve = %+v, want the job cancelled by the rejection", resolved)
	}
}
