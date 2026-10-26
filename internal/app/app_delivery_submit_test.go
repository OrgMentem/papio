// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A manual document-delivery submit must own the job it POSTs for.

package app

import (
	"context"
	"errors"
	"net/http"
	"papio/internal/delivery"
	"papio/internal/job"
	"testing"
)

// TestSubmitDeliveryRefusesAJobItCannotOwn proves a manual submit on a job
// that already settled unavailable places no provider request. Before the fix
// it POSTed, RecordSubmission then matched nothing because the job was not
// resolving, and the request the library held was recorded nowhere.
func TestSubmitDeliveryRefusesAJobItCannotOwn(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusOK, `{"TransactionNumber":4242}`)
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()

	// Delivery is not configured yet, so the job settles unavailable.
	configured := svc.Config.Browser.DocumentDelivery
	svc.Config.Browser.DocumentDelivery = nil
	id := processNewDeliveryJob(t, svc, jobs, "wr_submit_unavailable", "10.1000/submit-unavailable")
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable {
		t.Fatalf("setup: job state = %q, want unavailable", got.State)
	}
	svc.Config.Browser.DocumentDelivery = configured

	_, err = svc.SubmitDelivery(ctx, id)
	if !errors.Is(err, job.ErrConflict) {
		t.Fatalf("SubmitDelivery on an unavailable job = %v, want job.ErrConflict", err)
	}
	if p.posts.Load() != 0 {
		t.Fatalf("provider POSTs = %d, want 0 for a job that cannot own a request", p.posts.Load())
	}
	if row, err := deliverySvc.GetByJobID(ctx, id); err != nil || row != nil {
		t.Fatalf("delivery row = %+v, err = %v, want no row created by a refused submit", row, err)
	}
	after, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != job.StateUnavailable {
		t.Fatalf("refused submit moved the job to %q, want unavailable", after.State)
	}
}

// TestSubmitDeliveryClaimsAQueuedJob proves the ordinary manual submit still
// works: a queued job is claimed, moved to resolving, submitted once and
// parked on the poll cadence with its lease released.
func TestSubmitDeliveryClaimsAQueuedJob(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusOK, `{"TransactionNumber":4242}`)
	svc, jobs, _ := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_submit_queued", "10.1000/submit-queued"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.SubmitDelivery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Request == nil || result.Request.State != delivery.StateSubmitted || result.Request.ProviderReference != "4242" {
		t.Fatalf("result.Request = %+v, want submitted with reference 4242", result.Request)
	}
	if p.posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want 1", p.posts.Load())
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait || got.LeaseOwner != "" {
		t.Fatalf("job = state %q lease %q, want retry_wait with no lease held", got.State, got.LeaseOwner)
	}
}

// TestSubmitDeliveryRefusesAQueuedJobAWorkerHolds proves the claim is a real
// lease: a worker that already leased the job keeps it, and no POST is sent.
func TestSubmitDeliveryRefusesAQueuedJobAWorkerHolds(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusOK, `{"TransactionNumber":4242}`)
	svc, jobs, _ := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_submit_leased", "10.1000/submit-leased"))
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := jobs.ClaimNext(ctx, "worker", 60e9); err != nil || claimed == nil {
		t.Fatalf("ClaimNext = %v, %v", claimed, err)
	}
	if _, err := svc.SubmitDelivery(ctx, id); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("SubmitDelivery on a worker-leased job = %v, want job.ErrConflict", err)
	}
	if p.posts.Load() != 0 {
		t.Fatalf("provider POSTs = %d, want 0", p.posts.Load())
	}
}

func TestSubmitToProviderLostReservationDoesNotStopWorker(t *testing.T) {
	for _, cancelOwner := range []bool{false, true} {
		name := "request_cancelled"
		if cancelOwner {
			name = "owner_cancelled"
		}
		t.Run(name, func(t *testing.T) {
			p := newAmbiguousProvider(t, http.StatusOK, `{"TransactionNumber":4242}`)
			svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
			ctx := context.Background()
			id, err := svc.Submit(ctx, deliveryWorkRequest("wr_lost_reservation", "10.1000/lost-reservation"))
			if err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, map[string]any{"reason": "test_setup"}); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			request := seedOfferedRecovery(t, jobs, deliverySvc, id, "default", "illiad", "digest")
			wantState := job.StateRetryWait
			if cancelOwner {
				if err := jobs.Transition(ctx, id, job.StateResolving, job.StateCancelled, map[string]any{"reason": "test_cancel"}); err != nil {
					t.Fatal(err)
				}
				wantState = job.StateCancelled
			} else {
				if err := deliverySvc.UpdateState(ctx, request.ID, delivery.StateCancelled); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.submitToProvider(ctx, row, job.StateResolving, svc.Config.Browser.DocumentDelivery,
				request.IdempotencyKey, delivery.GateProfile{}, request); err != nil {
				t.Fatalf("lost submission reservation: %v", err)
			}
			if p.posts.Load() != 0 {
				t.Fatalf("provider POSTs = %d, want none after losing reservation", p.posts.Load())
			}
			got, err := jobs.Get(ctx, id)
			if err != nil || got.State != wantState {
				t.Fatalf("job = %+v, %v; want %s", got, err, wantState)
			}
		})
	}
}
