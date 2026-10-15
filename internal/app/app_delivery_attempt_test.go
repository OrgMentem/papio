// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// An ambiguous ILLiad POST leaves an offered row that cancel must treat as
// possibly live.

package app

import (
	"context"
	"net/http"
	"papio/internal/delivery"
	"papio/internal/job"
	"testing"
)

// parkAmbiguousJob drives a new job to an ambiguous provider POST: the 5xx
// leaves the outcome unknown, the token lookup finds nothing yet, and the job
// parks retry_wait with its row still offered.
func parkAmbiguousJob(t *testing.T, svc *Service, jobs *job.Store, deliverySvc *delivery.Service, requestID, doi string) (string, *delivery.Request) {
	t.Helper()
	ctx := context.Background()
	id := processNewDeliveryJob(t, svc, jobs, requestID, doi)
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait {
		t.Fatalf("setup: job state = %q, want retry_wait after an ambiguous POST", got.State)
	}
	row, err := deliverySvc.GetByJobID(ctx, id)
	if err != nil || row == nil {
		t.Fatalf("setup: delivery row = %+v, err = %v", row, err)
	}
	if row.State != delivery.StateOffered || row.ProviderReference != "" {
		t.Fatalf("setup: row = %+v, want offered with no reference", row)
	}
	return id, row
}

// TestAmbiguousPostLeavesAMarker pins the durable attempt marker an ambiguous
// POST leaves on the offered row: the cancel and orphan checks read it.
func TestAmbiguousPostLeavesAMarker(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	_, row := parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_amb_marker", "10.1000/amb-marker")
	if !row.SubmissionMayBeLive() {
		t.Fatalf("ambiguous offered row = %+v, want the attempt marker set", row)
	}
}

// TestCancelJobOrphansAnAmbiguousOfferedRow proves cancelling a job whose
// POST outcome is unknown does not drop the row as never submitted.
func TestCancelJobOrphansAnAmbiguousOfferedRow(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, row := parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_amb_cancel", "10.1000/amb-cancel")

	if err := svc.CancelJob(ctx, id, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	got, err := deliverySvc.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != delivery.StateUnknownOutcome {
		t.Fatalf("row state = %q, want unknown_outcome so reconciliation can still find it", got.State)
	}
}

// TestCancelJobOrphansAnAmbiguousRowWithoutAMarker covers a row an earlier
// daemon left behind: an ambiguous classification event but no attempt marker.
func TestCancelJobOrphansAnAmbiguousRowWithoutAMarker(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusServiceUnavailable, "provider outcome unavailable")
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, row := parkAmbiguousJob(t, svc, jobs, deliverySvc, "wr_amb_legacy", "10.1000/amb-legacy")
	if err := deliverySvc.ReleaseSubmissionAttempt(ctx, row.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.CancelJob(ctx, id, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatal(err)
	}
	got, err := deliverySvc.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != delivery.StateUnknownOutcome {
		t.Fatalf("row state = %q, want unknown_outcome for an ambiguous row with no marker", got.State)
	}
}
