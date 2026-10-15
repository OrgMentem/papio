// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The durable "a provider POST may have been sent" marker on an offered row:
// reservation and release.

package delivery

import (
	"context"
	"testing"
	"time"
)

func setJobState(t *testing.T, svc *Service, jobID, state string) {
	t.Helper()
	if _, err := svc.store.DB().ExecContext(context.Background(), `UPDATE jobs SET state = ? WHERE id = ?`, state, jobID); err != nil {
		t.Fatal(err)
	}
}

func offeredRow(t *testing.T, svc *Service, jobID, identity string) *Request {
	t.Helper()
	testJob(t, svc, jobID)
	req, err := svc.Create(context.Background(), CreateRequest{
		JobID: jobID, InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestReserveSubmissionAttemptRequiresAResolvingOwner pins the pre-POST
// condition: it is the same one RecordSubmission applies afterwards, so a POST
// is never sent that could not be recorded.
func TestReserveSubmissionAttemptRequiresAResolvingOwner(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	req := offeredRow(t, svc, "job_reserve", "doi:10.1/reserve")

	for _, state := range []string{"queued", "unavailable", "awaiting_human", "retry_wait", "cancelled"} {
		setJobState(t, svc, "job_reserve", state)
		ok, err := svc.ReserveSubmissionAttempt(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("reservation succeeded for an owning job in %q", state)
		}
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubmittedAt != "" {
		t.Fatalf("refused reservations left marker %q", got.SubmittedAt)
	}

	setJobState(t, svc, "job_reserve", "resolving")
	ok, err := svc.ReserveSubmissionAttempt(ctx, req.ID)
	if err != nil || !ok {
		t.Fatalf("reservation for a resolving owner = %v, %v, want true", ok, err)
	}
	got, err = svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubmittedAt == "" {
		t.Fatalf("row = %+v, want the attempt marker set", got)
	}
}

func TestReleaseSubmissionAttemptClearsOnlyAnOfferedMarker(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	req := offeredRow(t, svc, "job_release", "doi:10.1/release")
	setJobState(t, svc, "job_release", "resolving")
	if ok, err := svc.ReserveSubmissionAttempt(ctx, req.ID); err != nil || !ok {
		t.Fatalf("reserve = %v, %v", ok, err)
	}
	if err := svc.ReleaseSubmissionAttempt(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, req.ID)
	if got.SubmittedAt != "" {
		t.Fatalf("marker after release = %q, want cleared", got.SubmittedAt)
	}

	// A live row keeps its submitted_at: release never touches it.
	if ok, err := svc.ReserveSubmissionAttempt(ctx, req.ID); err != nil || !ok {
		t.Fatalf("second reserve = %v, %v", ok, err)
	}
	if won, err := svc.RecordSubmission(ctx, req.ID, "4242", time.Now().Add(time.Hour)); err != nil || !won {
		t.Fatalf("RecordSubmission = %v, %v", won, err)
	}
	if err := svc.ReleaseSubmissionAttempt(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(ctx, req.ID)
	if got.State != StateSubmitted || got.SubmittedAt == "" {
		t.Fatalf("row = %+v, want release to leave a submitted row alone", got)
	}
}
