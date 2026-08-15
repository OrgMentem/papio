// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// CRUD, idempotency, branch-table, cap-counting, and poll-scheduling
// behavior for ADR-0017 Decision 1/3B/4.
package delivery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
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

func TestRecordSubmissionAtomicallyCommitsAllFieldsAndCAS(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	svc := testService(t, now)
	ctx := context.Background()
	testJob(t, svc, "job_record_submission")
	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'resolving' WHERE id = ?`, "job_record_submission"); err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(ctx, CreateRequest{
		JobID: "job_record_submission", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/record-submission",
	})
	if err != nil {
		t.Fatal(err)
	}
	next := now.Add(15 * time.Minute)
	won, err := svc.RecordSubmission(ctx, created.ID, "provider-123", next)
	if err != nil {
		t.Fatalf("RecordSubmission: %v", err)
	}
	if !won {
		t.Fatal("RecordSubmission won = false, want true")
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNow := now.Format(time.RFC3339Nano)
	wantNext := next.Format(time.RFC3339Nano)
	if got.State != StateSubmitted || got.ProviderReference != "provider-123" ||
		got.SubmittedAt != wantNow || got.LastCheckedAt != wantNow || got.NextCheckAt != wantNext {
		t.Fatalf("recorded row = %+v, want submitted/provider/timestamps committed together", got)
	}

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	won, err = svc.RecordSubmission(ctx, created.ID, "provider-other", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CAS re-record: %v", err)
	}
	if won {
		t.Fatal("CAS re-record won = true, want benign false")
	}
	unchanged, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ProviderReference != "provider-123" || unchanged.State != StateSubmitted {
		t.Fatalf("CAS re-record clobbered row = %+v", unchanged)
	}
	if !bytes.Contains(logs.Bytes(), []byte("CAS did not match")) {
		t.Fatalf("CAS re-record log = %q, want benign mismatch diagnostic", logs.String())
	}
}

func TestRecordSubmissionRollsBackOnPreCommitFailure(t *testing.T) {
	now := time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)
	svc := testService(t, now)
	ctx := context.Background()
	testJob(t, svc, "job_record_submission_rollback")
	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'resolving' WHERE id = ?`, "job_record_submission_rollback"); err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(ctx, CreateRequest{
		JobID: "job_record_submission_rollback", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/record-submission-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.beforeRecordSubmissionCommit = func(*sql.Tx) error {
		return errors.New("injected commit failure")
	}
	if _, err := svc.RecordSubmission(ctx, created.ID, "provider-rollback", now.Add(time.Minute)); err == nil {
		t.Fatal("RecordSubmission error = nil, want injected failure")
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateOffered || got.ProviderReference != "" || got.SubmittedAt != "" ||
		got.LastCheckedAt != "" || got.NextCheckAt != "" {

		t.Fatalf("failed transaction left partial submission = %+v", got)
	}
}

func TestRecordSubmissionRejectsEveryNonResolvingJobState(t *testing.T) {
	states := []string{
		job.StateQueued, job.StateResolving, job.StateFetching, job.StateValidating,
		job.StateReady, job.StateImported, job.StateAwaitingHuman, job.StateRetryWait,
		job.StateNeedsReview, job.StateUnavailable, job.StateFailed, job.StateCancelled,
	}
	file, err := parser.ParseFile(token.NewFileSet(), "../job/job.go", nil, 0)
	if err != nil {
		t.Fatalf("parse job.go: %v", err)
	}
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if !strings.HasPrefix(name.Name, "State") {
					continue
				}
				if len(value.Values) != len(value.Names) {
					t.Fatalf("%s has %d names and %d values", name.Name, len(value.Names), len(value.Values))
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s is not a string state literal", name.Name)
				}
				decoded, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared[decoded] = true
			}
		}
	}
	if len(declared) != len(states) {
		t.Fatalf("job.go declares %d states but test covers %d; update this table for every new state", len(declared), len(states))
	}
	for _, state := range states {
		if !declared[state] {
			t.Fatalf("test state %q is not declared by job.go", state)
		}
	}
	for i, state := range states {
		t.Run(state, func(t *testing.T) {
			svc := testService(t, time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC))
			ctx := context.Background()
			jobID := fmt.Sprintf("job_submission_state_%d", i)
			testJob(t, svc, jobID)
			if _, err := svc.store.DB().ExecContext(ctx, `UPDATE jobs SET state = ? WHERE id = ?`, state, jobID); err != nil {
				t.Fatal(err)
			}
			req, err := svc.Create(ctx, CreateRequest{
				JobID: jobID, InstitutionProfile: "campus", Provider: "illiad",
				RequestClass: "digital_journal_article", WorkIdentity: fmt.Sprintf("doi:10.1/submission-state-%d", i),
			})
			if err != nil {
				t.Fatal(err)
			}
			won, err := svc.RecordSubmission(ctx, req.ID, "provider-"+state, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatalf("state %s: %v", state, err)
			}
			if state == job.StateResolving {
				if !won {
					t.Fatal("resolving must win the submission CAS")
				}
			} else if won {
				t.Fatalf("state %s won CAS, want only resolving to be eligible", state)
			}
		})
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

// TestOrphanIfLiveDoesNotClobberTerminalOutcome proves the CAS discipline:
// a poll that settles the row to a terminal outcome between OrphanIfLive's
// read and write must not be clobbered with unknown_outcome, and no
// orphaned event is appended on the lost race. The interleaving is
// simulated via SetBeforeOrphanCASForTest, the package seam that runs
// between the GetByJobID snapshot and the guarded UPDATE — exactly the
// window where CancelJob/DismissAction race the poller in production.
// A second sub-case asserts the normal orphan path is unchanged: a
// genuinely live row is still orphaned exactly once with its event.
func TestOrphanIfLiveDoesNotClobberTerminalOutcome(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()

	// Race: submitted -> fulfilled between read and CAS.
	testJob(t, svc, "job_race_fulfilled")
	req, err := svc.Create(ctx, CreateRequest{
		JobID: "job_race_fulfilled", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/race-fulfilled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, req.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}
	svc.SetBeforeOrphanCASForTest(func() error {
		_, err := svc.store.DB().ExecContext(ctx,
			`UPDATE delivery_requests SET state = ?, updated_at = ? WHERE id = ?`,
			string(StateFulfilled), store.Now(), req.ID)
		return err
	})
	if err := svc.OrphanIfLive(ctx, "job_race_fulfilled", "job_cancelled"); err != nil {
		t.Fatalf("OrphanIfLive race (submitted->fulfilled): err = %v, want nil (successful no-op)", err)
	}
	svc.SetBeforeOrphanCASForTest(nil)
	after, err := svc.Get(ctx, req.ID)
	if err != nil || after == nil {
		t.Fatalf("Get after raced OrphanIfLive: %+v, %v", after, err)
	}
	if after.State != StateFulfilled {
		t.Fatalf("state = %q, want fulfilled — CAS must not overwrite poller's terminal settlement", after.State)
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?`, eventKindOrphaned, "job_race_fulfilled").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphaned events = %d, want 0 — CAS loss must not append event", count)
	}

	// Race: pending -> declined between read and CAS.
	testJob(t, svc, "job_race_declined")
	req2, err := svc.Create(ctx, CreateRequest{
		JobID: "job_race_declined", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/race-declined",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, req2.ID, StatePending); err != nil {
		t.Fatal(err)
	}
	svc.SetBeforeOrphanCASForTest(func() error {
		_, err := svc.store.DB().ExecContext(ctx,
			`UPDATE delivery_requests SET state = ?, updated_at = ? WHERE id = ?`,
			string(StateDeclined), store.Now(), req2.ID)
		return err
	})
	if err := svc.OrphanIfLive(ctx, "job_race_declined", "action_dismissed"); err != nil {
		t.Fatalf("OrphanIfLive race (pending->declined): err = %v, want nil", err)
	}
	svc.SetBeforeOrphanCASForTest(nil)
	after2, err := svc.Get(ctx, req2.ID)
	if err != nil || after2 == nil {
		t.Fatalf("Get after raced OrphanIfLive: %+v, %v", after2, err)
	}
	if after2.State != StateDeclined {
		t.Fatalf("state = %q, want declined — CAS must not overwrite poller's settlement", after2.State)
	}
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?`, eventKindOrphaned, "job_race_declined").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphaned events = %d, want 0 — CAS loss must not append event", count)
	}

	// Normal path unchanged: genuinely live rows are still orphaned exactly once.
	testJob(t, svc, "job_race_normal")
	req3, err := svc.Create(ctx, CreateRequest{
		JobID: "job_race_normal", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/race-normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, req3.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}
	if err := svc.OrphanIfLive(ctx, "job_race_normal", "job_cancelled"); err != nil {
		t.Fatalf("OrphanIfLive normal: %v", err)
	}
	after3, err := svc.Get(ctx, req3.ID)
	if err != nil || after3 == nil {
		t.Fatalf("Get after normal OrphanIfLive: %+v, %v", after3, err)
	}
	if after3.State != StateUnknownOutcome {
		t.Fatalf("state = %q, want unknown_outcome — live row must still be orphaned", after3.State)
	}
	var normalCount int
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?`, eventKindOrphaned, "job_race_normal").Scan(&normalCount); err != nil {
		t.Fatal(err)
	}
	if normalCount != 1 {
		t.Fatalf("orphaned events = %d, want 1 — live orphan must append exactly one event", normalCount)
	}
	var detailJSON string
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT detail_json FROM events WHERE kind = ? AND job_id = ?`, eventKindOrphaned, "job_race_normal").Scan(&detailJSON); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["cause"] != "job_cancelled" {
		t.Fatalf("detail cause = %v, want job_cancelled", detail["cause"])
	}
	if detail["delivery_request_id"] == nil {
		t.Fatalf("detail missing delivery_request_id: %v", detail)
	}
}

// TestResumeClearsFailureBookkeepingOnLiveRow proves the P2 recovery
// primitive: a contract-drift-parked live row (consecutive_poll_failures
// nonzero, last_poll_error_class set, next_check_at ~10 years out per
// poll.go's pollDisabledDelay) has all three reset by Resume, so a
// subsequent poll (NextCheckAt <= now) is no longer a no-op.
func TestResumeClearsFailureBookkeepingOnLiveRow(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	svc := testService(t, now)
	ctx := context.Background()
	testJob(t, svc, "job_resume_live")

	req, err := svc.Create(ctx, CreateRequest{
		JobID: "job_resume_live", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/resume-live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, req.ID, StatePending); err != nil {
		t.Fatal(err)
	}
	farFuture := now.Add(10 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET consecutive_poll_failures = 7, last_poll_error_class = 'contract_drift', next_check_at = ?
		WHERE id = ?`, farFuture, req.ID); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Resume(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil {
		t.Fatal("Resume returned a nil row for an existing id")
	}
	if after.State != StatePending {
		t.Fatalf("state = %q, want unchanged pending — Resume never changes state", after.State)
	}
	if after.ConsecutivePollFailures != 0 {
		t.Fatalf("consecutive_poll_failures = %d, want 0", after.ConsecutivePollFailures)
	}
	if after.LastPollErrorClass != "" {
		t.Fatalf("last_poll_error_class = %q, want cleared", after.LastPollErrorClass)
	}
	gotNext, err := time.Parse(time.RFC3339Nano, after.NextCheckAt)
	if err != nil {
		t.Fatalf("next_check_at = %q did not parse: %v", after.NextCheckAt, err)
	}
	if gotNext.After(now.Add(time.Minute)) {
		t.Fatalf("next_check_at = %v, want ~now (%v), not the stale far-future park", gotNext, now)
	}
}

// TestResumeRefusesTerminalRow proves Resume never touches a row whose
// state is not live — a fulfilled row (settled) and an offered row (never
// submitted, so nothing to resume) both come back ErrRequestNotLive with
// the unmodified row, never a bare error and never a silent no-op success.
func TestResumeRefusesTerminalRow(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	testJob(t, svc, "job_resume_fulfilled")
	testJob(t, svc, "job_resume_offered")

	fulfilled, err := svc.Create(ctx, CreateRequest{
		JobID: "job_resume_fulfilled", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/resume-fulfilled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, fulfilled.ID, StateFulfilled); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Resume(ctx, fulfilled.ID)
	if !errors.Is(err, ErrRequestNotLive) {
		t.Fatalf("Resume on a fulfilled row: err = %v, want ErrRequestNotLive", err)
	}
	if row == nil || row.State != StateFulfilled {
		t.Fatalf("Resume on a fulfilled row returned %+v, want the unmodified fulfilled row", row)
	}

	offered, err := svc.Create(ctx, CreateRequest{
		JobID: "job_resume_offered", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/resume-offered",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err = svc.Resume(ctx, offered.ID)
	if !errors.Is(err, ErrRequestNotLive) {
		t.Fatalf("Resume on an offered row: err = %v, want ErrRequestNotLive", err)
	}
	if row == nil || row.State != StateOffered {
		t.Fatalf("Resume on an offered row returned %+v, want the unmodified offered row", row)
	}

	// Not found: distinct nil, nil — never ErrRequestNotLive.
	row, err = svc.Resume(ctx, fulfilled.ID+1_000_000)
	if err != nil || row != nil {
		t.Fatalf("Resume on an unknown id = %+v, %v, want (nil, nil)", row, err)
	}
}
func TestListRecoverableOnlyOfferedWithoutProviderReference(t *testing.T) {
	ctx := context.Background()
	svc := testService(t, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name  string
		state State
		ref   string
		want  bool
	}{
		{"offered_without_reference", StateOffered, "", true},
		{"offered_with_reference", StateOffered, "txn-1", false},
		{"submitted", StateSubmitted, "", false},
		{"pending", StatePending, "", false},
		{"fulfilled", StateFulfilled, "", false},
		{"declined", StateDeclined, "", false},
		{"cancelled", StateCancelled, "", false},
		{"unknown_outcome", StateUnknownOutcome, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "job_recover_" + tc.name
			testJob(t, svc, jobID)
			req, err := svc.Create(ctx, CreateRequest{
				JobID: jobID, InstitutionProfile: "campus", Provider: "illiad",
				RequestClass: "digital_journal_article", WorkIdentity: jobID,
				State: tc.state, ProviderReference: tc.ref,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.ListRecoverable(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, candidate := range got {
				if candidate.ID == req.ID {
					found = true
					break
				}
			}
			if found != tc.want {
				t.Fatalf("row returned = %t, want %t; rows = %+v", found, tc.want, got)
			}
		})
	}
}
func TestCreatePersistsOnlyIdentityDigestNotRawBindingInputs(t *testing.T) {
	svc := testService(t, time.Now())
	testJob(t, svc, "job_digest_identity")
	profile := CompileGateProfile(config.Institution{DocumentDelivery: fullHouseDocumentDelivery()}, "campus")
	const rawSecret = "secret-key"
	const rawPatron = "configured-non-secret-reference"
	if strings.Contains(profile.Digest(), rawSecret) || strings.Contains(profile.Digest(), rawPatron) {
		t.Fatal("profile digest contains raw identity input")
	}
	if _, err := svc.Create(context.Background(), CreateRequest{
		JobID: "job_digest_identity", InstitutionProfile: "campus", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/digest",
		GateProfileDigest: profile.Digest(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var stored string
	if err := svc.store.DB().QueryRowContext(context.Background(),
		`SELECT gate_profile_digest FROM delivery_requests WHERE job_id = ?`, "job_digest_identity").Scan(&stored); err != nil {
		t.Fatalf("read stored digest: %v", err)
	}
	if strings.Contains(stored, rawSecret) || strings.Contains(stored, rawPatron) {
		t.Fatalf("stored digest contains raw identity input: %q", stored)
	}
}
