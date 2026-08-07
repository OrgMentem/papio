// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// CRUD, idempotency, branch-table, cap-counting, and poll-scheduling
// behavior for ADR-0017 Decision 1/3B/4.
package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"papio/internal/store"
)

func testService(t *testing.T, now time.Time) *Service {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return New(s, nil, func() time.Time { return now })
}

// testJob inserts the minimum work_requests/jobs rows delivery_requests.job_id
// requires (it is NOT NULL REFERENCES jobs(id)).
func testJob(t *testing.T, svc *Service, id string) {
	t.Helper()
	ctx := context.Background()
	now := store.Now()
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO work_requests (id, created_at) VALUES (?, ?)`, "wr_"+id, now); err != nil {
		t.Fatalf("insert work_request: %v", err)
	}
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', '{}', ?, ?)`,
		id, "wr_"+id, now, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}
}

func TestCreateDuplicateKeyReturnsExistingRow(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	testJob(t, svc, "job_a")
	testJob(t, svc, "job_b")

	first, err := svc.Create(ctx, CreateRequest{
		JobID: "job_a", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/x",
		GateProfileDigest: "digest1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.State != StateOffered {
		t.Fatalf("State = %q, want offered (default)", first.State)
	}

	// Same institution profile + work identity + provider + request class,
	// different job — Decision 1: "a resubmission attempt for the same key
	// must resolve against the existing row, never open a second one."
	dup, err := svc.Create(ctx, CreateRequest{
		JobID: "job_b", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/x",
	})
	if !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("Create duplicate: err = %v, want ErrDuplicateRequest", err)
	}
	if dup == nil || dup.ID != first.ID || dup.JobID != "job_a" {
		t.Fatalf("Create duplicate returned %+v, want the existing row (job_a, id %d)", dup, first.ID)
	}

	var count int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("delivery_requests has %d rows, want exactly 1 (no second row created)", count)
	}
}

func TestCreateDifferentKeyComponentsAreIndependent(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_a")
	testJob(t, svc, "job_b")

	if _, err := svc.Create(ctx, CreateRequest{
		JobID: "job_a", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/x",
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// A different work identity is a different key — not a duplicate.
	if _, err := svc.Create(ctx, CreateRequest{
		JobID: "job_b", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/y",
	}); err != nil {
		t.Fatalf("second Create (different work identity) should not collide: %v", err)
	}
}

func TestBranchTable(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()

	// No existing row → evaluate_gate, nil row.
	noRowKey := IdempotencyKey("campus", "doi:10.1/none", "illiad", "digital_journal_article")
	branch, row, err := svc.Branch(ctx, noRowKey)
	if err != nil {
		t.Fatal(err)
	}
	if branch != BranchEvaluateGate || row != nil {
		t.Fatalf("Branch(no row) = %q, %+v, want evaluate_gate, nil", branch, row)
	}

	for i, test := range []struct {
		state State
		want  BranchDecision
	}{
		{StateOffered, BranchEvaluateGate},
		{StateSubmitted, BranchJoinPoll},
		{StatePending, BranchJoinPoll},
		{StateFulfilled, BranchAdoptFulfilled},
		{StateUnknownOutcome, BranchReconcile},
		{StateDeclined, BranchResubmissionPolicy},
		{StateCancelled, BranchResubmissionPolicy},
	} {
		jobID := "job_branch_" + string(rune('a'+i))
		testJob(t, svc, jobID)
		created, err := svc.Create(ctx, CreateRequest{
			JobID: jobID, InstitutionProfile: "campus", Provider: "illiad",
			RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/" + jobID,
		})
		if err != nil {
			t.Fatalf("Create for state %q: %v", test.state, err)
		}
		if test.state != StateOffered {
			if err := svc.UpdateState(ctx, created.ID, test.state); err != nil {
				t.Fatalf("UpdateState(%q): %v", test.state, err)
			}
		}
		key := IdempotencyKey("campus", "doi:10.1/"+jobID, "illiad", "digital_journal_article")
		branch, row, err := svc.Branch(ctx, key)
		if err != nil {
			t.Fatalf("Branch state %q: %v", test.state, err)
		}
		if branch != test.want {
			t.Errorf("Branch(state=%q) = %q, want %q", test.state, branch, test.want)
		}
		if row == nil || row.State != test.state {
			t.Errorf("Branch(state=%q) row = %+v, want State = %q", test.state, row, test.state)
		}
	}
}

func TestUpdateStateStampsSubmittedAtOnce(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_stamp")
	created, err := svc.Create(ctx, CreateRequest{
		JobID: "job_stamp", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/stamp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.SubmittedAt != "" {
		t.Fatalf("SubmittedAt = %q before submission, want empty", created.SubmittedAt)
	}
	if err := svc.UpdateState(ctx, created.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}
	submitted, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.SubmittedAt == "" {
		t.Fatal("SubmittedAt still empty after transitioning to submitted")
	}
	first := submitted.SubmittedAt
	if err := svc.UpdateState(ctx, created.ID, StatePending); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, created.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.SubmittedAt != first {
		t.Fatalf("SubmittedAt changed on re-entering submitted: %q -> %q, want stable", first, again.SubmittedAt)
	}
}

func TestSubmittedThisMonthCountsAndScopesByMonth(t *testing.T) {
	svc := testService(t, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()

	for _, jobID := range []string{"job_m1", "job_m2"} {
		testJob(t, svc, jobID)
		created, err := svc.Create(ctx, CreateRequest{
			JobID: jobID, InstitutionProfile: "campus", Provider: "illiad",
			RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/" + jobID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.UpdateState(ctx, created.ID, StateSubmitted); err != nil {
			t.Fatal(err)
		}
	}
	// A third request for a different provider must not count against this
	// provider's cap.
	testJob(t, svc, "job_other_provider")
	otherCreated, err := svc.Create(ctx, CreateRequest{
		JobID: "job_other_provider", InstitutionProfile: "campus", Provider: "openurl",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, otherCreated.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}

	count, err := svc.SubmittedThisMonth(ctx, "campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("SubmittedThisMonth = %d, want 2", count)
	}

	// A Service clocked into the next month must not see March's submissions.
	nextMonth := New(svc.store, nil, func() time.Time { return time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) })
	count, err = nextMonth.SubmittedThisMonth(ctx, "campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("SubmittedThisMonth (next month) = %d, want 0", count)
	}
}

func TestNextCheckBounds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if got := NextCheck(now, 0, 0); got.Sub(now) != 60*time.Minute {
		t.Fatalf("NextCheck(default poll minutes, attempt 0) = %v after now, want 60m", got.Sub(now))
	}
	if got := NextCheck(now, 1, 60); got.Sub(now) != 120*time.Minute {
		t.Fatalf("NextCheck(attempt 1) = %v after now, want 120m (backoff doubles)", got.Sub(now))
	}
	if got := NextCheck(now, 2, 60); got.Sub(now) != 240*time.Minute {
		t.Fatalf("NextCheck(attempt 2) = %v after now, want 240m", got.Sub(now))
	}
	if got := NextCheck(now, 1000, 60); got.Sub(now) != maxPollInterval {
		t.Fatalf("NextCheck(huge attempt) = %v after now, want capped at %v", got.Sub(now), maxPollInterval)
	}
	if got := NextCheck(now, 0, 90); got.Sub(now) != 90*time.Minute {
		t.Fatalf("NextCheck(custom poll minutes) = %v after now, want 90m", got.Sub(now))
	}
	if got := NextCheck(now, -5, 60); got.Sub(now) != 60*time.Minute {
		t.Fatalf("NextCheck(negative attempt) = %v after now, want treated as attempt 0 (60m)", got.Sub(now))
	}
}

func TestAppendGateEventDetail(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_evt")

	err := svc.AppendGateEvent(ctx, "job_evt", GateEvaluated{
		ProfileClass:       GateClassAutoCapable,
		ProfileDigest:      "digest-xyz",
		Decision:           Decision{Action: ActionSubmit, Blockers: nil},
		FulfillmentChannel: FulfillmentChannelPatronWeb,
	})
	if err != nil {
		t.Fatal(err)
	}

	var jobID sql.NullString
	var kind, detailJSON string
	err = svc.store.DB().QueryRowContext(ctx, `SELECT job_id, kind, detail_json FROM events WHERE kind = 'delivery.gate_evaluated'`).
		Scan(&jobID, &kind, &detailJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !jobID.Valid || jobID.String != "job_evt" {
		t.Fatalf("job_id = %v, want job_evt", jobID)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["profile_class"] != "auto_capable" || detail["profile_digest"] != "digest-xyz" || detail["decision"] != "submit" {
		t.Fatalf("detail = %v, missing expected profile_class/profile_digest/decision", detail)
	}
	if detail["fulfillment_channel"] != "patron_web" {
		t.Fatalf("detail[fulfillment_channel] = %v, want patron_web", detail["fulfillment_channel"])
	}
}

// TestLatestGateEventRoundTripsFulfillmentChannel proves the 2026-08-07
// amendment's addition to GateEvaluated survives the AppendGateEvent ->
// LatestGateEvent round trip papio delivery get/jobs get_v3 rely on, and
// that an event recorded before this field existed decodes to "" rather
// than erroring (the json field is simply absent from old detail_json).
func TestLatestGateEventRoundTripsFulfillmentChannel(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_evt_channel")

	if err := svc.AppendGateEvent(ctx, "job_evt_channel", GateEvaluated{
		ProfileClass:       GateClassAutoCapable,
		ProfileDigest:      "digest-abc",
		Decision:           Decision{Action: ActionSubmit},
		FulfillmentChannel: FulfillmentChannelPatronWeb,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.LatestGateEvent(ctx, "job_evt_channel")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FulfillmentChannel != FulfillmentChannelPatronWeb {
		t.Fatalf("LatestGateEvent = %+v, want FulfillmentChannel %q", got, FulfillmentChannelPatronWeb)
	}

	testJob(t, svc, "job_evt_no_channel")
	if err := svc.AppendGateEvent(ctx, "job_evt_no_channel", GateEvaluated{
		ProfileClass:  GateClassPrefillOnly,
		ProfileDigest: "digest-def",
		Decision:      Decision{Action: ActionPrefill, Blockers: []string{BlockerAPICredentialMissing}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.LatestGateEvent(ctx, "job_evt_no_channel")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FulfillmentChannel != "" {
		t.Fatalf("LatestGateEvent = %+v, want empty FulfillmentChannel", got)
	}
}

func TestRecordAndHasLiveAcceptance(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()

	accepted, err := svc.HasLiveAcceptance(ctx, "campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("HasLiveAcceptance = true before any RecordLiveAcceptance call")
	}

	if err := svc.RecordLiveAcceptance(ctx, "campus", "illiad"); err != nil {
		t.Fatal(err)
	}

	accepted, err = svc.HasLiveAcceptance(ctx, "campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("HasLiveAcceptance = false after RecordLiveAcceptance")
	}

	// A different profile or provider must not be affected.
	accepted, err = svc.HasLiveAcceptance(ctx, "other-campus", "illiad")
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("HasLiveAcceptance = true for an unrelated institution profile")
	}
	accepted, err = svc.HasLiveAcceptance(ctx, "campus", "openurl")
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("HasLiveAcceptance = true for an unrelated provider")
	}
}

// TestOrphanIfLiveMarksSubmittedRowUnknownOutcome proves the P2 compensation
// primitive: a submitted row whose driving job stopped watching it is
// marked unknown_outcome with a recorded delivery.orphaned event, never
// left looking like papio is still polling it.
func TestOrphanIfLiveMarksSubmittedRowUnknownOutcome(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_live")

	req, err := svc.Create(ctx, CreateRequest{
		JobID: "job_live", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/orphan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, req.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}

	if err := svc.OrphanIfLive(ctx, "job_live", "job_cancelled"); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Get(ctx, req.ID)
	if err != nil || after == nil {
		t.Fatalf("Get after OrphanIfLive: %+v, %v", after, err)
	}
	if after.State != StateUnknownOutcome {
		t.Fatalf("state = %q, want unknown_outcome", after.State)
	}

	var jobID sql.NullString
	var kind, detailJSON string
	err = svc.store.DB().QueryRowContext(ctx, `SELECT job_id, kind, detail_json FROM events WHERE kind = 'delivery.orphaned'`).
		Scan(&jobID, &kind, &detailJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !jobID.Valid || jobID.String != "job_live" {
		t.Fatalf("job_id = %v, want job_live", jobID)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["cause"] != "job_cancelled" {
		t.Fatalf("detail = %v, want cause=job_cancelled", detail)
	}
}

// TestOrphanIfLiveIgnoresNonLiveRows proves OrphanIfLive only ever touches a
// row actually submitted/pending: an offered row (no vendor request exists
// yet), an already-resolved row, and a job with no delivery row at all are
// all untouched no-ops.
func TestOrphanIfLiveIgnoresNonLiveRows(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_offered")
	testJob(t, svc, "job_fulfilled")
	testJob(t, svc, "job_none")

	offered, err := svc.Create(ctx, CreateRequest{
		JobID: "job_offered", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/offered",
	})
	if err != nil {
		t.Fatal(err)
	}
	fulfilled, err := svc.Create(ctx, CreateRequest{
		JobID: "job_fulfilled", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/fulfilled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, fulfilled.ID, StateFulfilled); err != nil {
		t.Fatal(err)
	}

	if err := svc.OrphanIfLive(ctx, "job_offered", "job_cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := svc.OrphanIfLive(ctx, "job_fulfilled", "job_cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := svc.OrphanIfLive(ctx, "job_none", "job_cancelled"); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Get(ctx, offered.ID)
	if err != nil || after == nil || after.State != StateOffered {
		t.Fatalf("offered row after OrphanIfLive = %+v, %v, want untouched offered", after, err)
	}
	after, err = svc.Get(ctx, fulfilled.ID)
	if err != nil || after == nil || after.State != StateFulfilled {
		t.Fatalf("fulfilled row after OrphanIfLive = %+v, %v, want untouched fulfilled", after, err)
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = 'delivery.orphaned'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("delivery.orphaned events = %d, want 0 — none of these rows were ever live", count)
	}
}
