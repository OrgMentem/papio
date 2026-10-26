// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// An offered row carrying the attempt marker is possibly live: cancel refuses
// it and orphaning moves it to unknown_outcome.

package delivery

import (
	"context"
	"testing"
	"time"
)

func TestCancelUnattemptedRefusesAMarkedRow(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	clean := offeredRow(t, svc, "job_clean", "doi:10.1/clean")
	marked := offeredRow(t, svc, "job_marked", "doi:10.1/marked")
	setJobState(t, svc, "job_marked", "resolving")
	if ok, err := svc.ReserveSubmissionAttempt(ctx, marked.ID); err != nil || !ok {
		t.Fatalf("reserve = %v, %v", ok, err)
	}

	if applied, err := svc.CancelUnattempted(ctx, marked.ID); err != nil || applied {
		t.Fatalf("CancelUnattempted on a marked row = %v, %v, want false", applied, err)
	}
	if got, _ := svc.Get(ctx, marked.ID); got.State != StateOffered {
		t.Fatalf("marked row state = %q, want offered", got.State)
	}
	if applied, err := svc.CancelUnattempted(ctx, clean.ID); err != nil || !applied {
		t.Fatalf("CancelUnattempted on a clean row = %v, %v, want true", applied, err)
	}
}

func TestOrphanIfLiveMarksAnAttemptedOfferedRowUnknownOutcome(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	req := offeredRow(t, svc, "job_orphan_attempt", "doi:10.1/orphan-attempt")
	setJobState(t, svc, "job_orphan_attempt", "resolving")
	if ok, err := svc.ReserveSubmissionAttempt(ctx, req.ID); err != nil || !ok {
		t.Fatalf("reserve = %v, %v", ok, err)
	}
	setJobState(t, svc, "job_orphan_attempt", "cancelled")
	if err := svc.OrphanIfLive(ctx, "job_orphan_attempt", "job_cancelled"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, req.ID)
	if got.State != StateUnknownOutcome {
		t.Fatalf("state = %q, want unknown_outcome for a possibly-live offered row", got.State)
	}
}

func TestOrphanIfLivePreservesRequestAfterOwnerChanges(t *testing.T) {
	for _, reassign := range []bool{false, true} {
		name := "owner_revived"
		if reassign {
			name = "request_reassigned"
		}
		t.Run(name, func(t *testing.T) {
			svc := testService(t, time.Now())
			ctx := context.Background()
			req := offeredRow(t, svc, "job_old", "doi:10.1/orphan-owner-race")
			setJobState(t, svc, "job_old", "resolving")
			if won, err := svc.ReserveSubmissionAttempt(ctx, req.ID); err != nil || !won {
				t.Fatalf("reserve = %v, %v", won, err)
			}
			setJobState(t, svc, "job_old", "cancelled")
			testJob(t, svc, "job_new")
			setJobState(t, svc, "job_new", "resolving")
			expectedOwner := "job_old"
			svc.SetBeforeOrphanCASForTest(func() error {
				if reassign {
					won, err := svc.ReassignOfferedRequest(ctx, req.ID, "job_new", "job_old")
					if err != nil || !won {
						t.Fatalf("reassign = %v, %v", won, err)
					}
					expectedOwner = "job_new"
				} else {
					setJobState(t, svc, "job_old", "resolving")
				}
				return nil
			})
			if err := svc.OrphanIfLive(ctx, "job_old", "job_cancelled"); err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(ctx, req.ID)
			if err != nil || got == nil {
				t.Fatalf("request = %+v, %v", got, err)
			}
			if got.JobID != expectedOwner || got.State != StateOffered || !got.SubmissionMayBeLive() {
				t.Fatalf("request = %+v; want live attempted request owned by %s", got, expectedOwner)
			}
			var events int
			if err := svc.store.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM events WHERE kind = 'delivery.orphaned'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events != 0 {
				t.Fatalf("orphan events = %d, want none after owner changes", events)
			}
		})
	}
}
