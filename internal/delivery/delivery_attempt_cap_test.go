// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Attempted offered rows and adopted pending rows count toward the monthly cap.

package delivery

import (
	"context"
	"testing"
	"time"
)

func TestSubmittedThisMonthCountsAttemptedOfferedAndPendingRows(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	attempted := offeredRow(t, svc, "job_cap_attempt", "doi:10.1/cap-attempt")
	offeredRow(t, svc, "job_cap_clean", "doi:10.1/cap-clean")
	adopted := offeredRow(t, svc, "job_cap_adopted", "doi:10.1/cap-adopted")
	setJobState(t, svc, "job_cap_attempt", "resolving")
	if ok, err := svc.ReserveSubmissionAttempt(ctx, attempted.ID); err != nil || !ok {
		t.Fatalf("reserve = %v, %v", ok, err)
	}
	// confirm-exists adopts straight into pending.
	if err := svc.UpdateState(ctx, adopted.ID, StatePending); err != nil {
		t.Fatal(err)
	}
	count, err := svc.SubmittedThisMonth(ctx, "campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("SubmittedThisMonth = %d, want 2 (attempted offered + adopted pending; the clean offered row is not counted)", count)
	}
}
