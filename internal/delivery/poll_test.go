// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The poll executor's state map, failure discipline, and reconciliation
// paths (ADR-0017 Decision 4). Fixture/httptest ILLiad responses only —
// no live calls.
package delivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"papio/internal/illiad"
	"papio/internal/store"
)

// testServiceClock is testService with a mutable clock, so a test can
// advance "now" between successive Poll calls (backoff growth, the
// Request Finished delayed reconciliation pass, 404 propagation).
func testServiceClock(t *testing.T) (*Service, *time.Time) {
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
	clock := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	svc := New(s, nil, func() time.Time { return clock })
	return svc, &clock
}

// newLiveRequest creates a submitted delivery_requests row (job_id
// satisfied) with the given provider reference and a next_check_at due
// immediately at now, ready for Poll.
func newLiveRequest(t *testing.T, svc *Service, jobID string, now time.Time) *Request {
	t.Helper()
	testJob(t, svc, jobID)
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateRequest{
		JobID:              jobID,
		InstitutionProfile: "default",
		Provider:           "illiad",
		RequestClass:       "digital_journal_article",
		WorkIdentity:       "doi:10.1000/" + jobID,
		GateProfileDigest:  "digest",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.UpdateState(ctx, created.ID, StateSubmitted); err != nil {
		t.Fatalf("update state: %v", err)
	}
	if err := svc.RecordPoll(ctx, created.ID, "555", now.Add(-time.Minute)); err != nil {
		t.Fatalf("record poll: %v", err)
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v, %v", got, err)
	}
	return got
}

// sequencedIlliadServer replies to successive GetTransaction/UserRequests
// calls with responses[0], responses[1], ... (the last response repeats
// once exhausted). userRequests, when non-nil, answers every
// Transaction/User/ call regardless of sequence position.
func sequencedIlliadServer(t *testing.T, userRequests []illiad.Transaction, responses ...http.HandlerFunc) *illiad.Client {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userRequests != nil && len(r.URL.Path) >= len("/Transaction/User/") && r.URL.Path[:len("/Transaction/User/")] == "/Transaction/User/" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(userRequests)
			return
		}
		idx := calls
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		calls++
		responses[idx](w, r)
	}))
	t.Cleanup(srv.Close)
	return illiad.New(srv.Client(), srv.URL, "key")
}

func txnResponse(status string, number int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(illiad.Transaction{TransactionNumber: number, TransactionStatus: status})
	}
}

func statusOnly(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func invalidJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{not valid json"))
}

// --- classifyStatus: pure state-map coverage ---------------------------

func TestClassifyStatusStateMap(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		priorDisplay string
		wantState    State
		wantDisplay  string
		wantUnmapped bool
	}{
		{"delivered to web", illiadStatusDeliveredToWeb, "", StateFulfilled, illiadStatusDeliveredToWeb, false},
		{"cancelled by customer", illiadStatusCancelledByCustomer, "", StateCancelled, illiadStatusCancelledByCustomer, false},
		{"cancelled by ill staff", illiadStatusCancelledByILLStaff, "", StateDeclined, illiadStatusCancelledByILLStaff, false},
		{"awaiting unfilled processing stays pending, not declined", illiadStatusAwaitingUnfilled, "", StatePending, illiadStatusAwaitingUnfilled, false},
		{"unmapped custom status stays pending", "Awaiting Request Processing", "", StatePending, "", true},
		{"unmapped custom status preserves prior evidence", "Awaiting Request Processing", illiadStatusDeliveredToWeb, StatePending, illiadStatusDeliveredToWeb, true},
		{"request finished with delivered-to-web evidence fulfills", illiadStatusRequestFinished, illiadStatusDeliveredToWeb, StateFulfilled, illiadStatusRequestFinished, false},
		{"request finished with customer-cancellation evidence cancels", illiadStatusRequestFinished, illiadStatusCancelledByCustomer, StateCancelled, illiadStatusRequestFinished, false},
		{"request finished with staff-cancellation evidence declines", illiadStatusRequestFinished, illiadStatusCancelledByILLStaff, StateDeclined, illiadStatusRequestFinished, false},
		{"request finished with no evidence defers (first pass)", illiadStatusRequestFinished, "", "", illiadStatusRequestFinished, false},
		{"request finished with no evidence, second pass settles unknown", illiadStatusRequestFinished, illiadStatusRequestFinished, StateUnknownOutcome, illiadStatusRequestFinished, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, display, unmapped := classifyStatus(c.raw, c.priorDisplay)
			if state != c.wantState || display != c.wantDisplay || unmapped != c.wantUnmapped {
				t.Fatalf("classifyStatus(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
					c.raw, c.priorDisplay, state, display, unmapped, c.wantState, c.wantDisplay, c.wantUnmapped)
			}
		})
	}
}

// --- Poll: due-ness, fulfilled-once, raw status persistence ------------

func TestPollNotDueIsNoOp(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	testJob(t, svc, "notdue")
	created, err := svc.Create(ctx, CreateRequest{JobID: "notdue", InstitutionProfile: "default", Provider: "illiad", RequestClass: "digital_journal_article", WorkIdentity: "doi:x", GateProfileDigest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, created.ID, StateSubmitted); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordPoll(ctx, created.ID, "555", clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var providerCalled bool
	client := sequencedIlliadServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled || result.State != StateSubmitted {
		t.Fatalf("result = %+v, want unchanged submitted", result)
	}
	if providerCalled {
		t.Fatal("Poll must not call the provider before next_check_at is due")
	}
}

func TestPollDeliveredToWebFulfillsExactlyOnceWithEvent(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "fulfill1", *clock)
	client := sequencedIlliadServer(t, nil, txnResponse(illiadStatusDeliveredToWeb, 555))

	result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Settled || result.State != StateFulfilled {
		t.Fatalf("result = %+v, want settled fulfilled", result)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFulfilled {
		t.Fatalf("state = %q, want fulfilled", got.State)
	}
	if got.ProviderStatusRaw != illiadStatusDeliveredToWeb {
		t.Fatalf("provider_status_raw = %q, want %q", got.ProviderStatusRaw, illiadStatusDeliveredToWeb)
	}
	if got.NextCheckAt != "" {
		t.Fatalf("next_check_at = %q, want empty (polling stops)", got.NextCheckAt)
	}

	events, err := svc.store.DB().QueryContext(ctx, `SELECT kind FROM events WHERE job_id = ? AND kind = ?`, "fulfill1", eventKindFulfilled)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	n := 0
	for events.Next() {
		n++
	}
	if n != 1 {
		t.Fatalf("delivery.fulfilled events = %d, want exactly 1", n)
	}

	// Poll on an already-settled row must be an idempotent no-op: it
	// reports settled without a second provider call or a duplicate
	// delivery.fulfilled event.
	result2, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !result2.Settled || result2.State != StateFulfilled {
		t.Fatalf("re-polling an already-fulfilled row = %+v, want settled fulfilled no-op", result2)
	}
	var eventsAfter int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = ?`,
		"fulfill1", eventKindFulfilled).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != 1 {
		t.Fatalf("delivery.fulfilled events after a second Poll = %d, want still exactly 1", eventsAfter)
	}
}

// TestPollConcurrentRaceNeverDuplicatesSettlementOrRegressesState pins the
// compare-and-swap discipline persistPollSuccess enforces: two workers
// (e.g. two jobs joined on the same live delivery_requests row) each read
// their own snapshot before either wrote. The loser's CAS'd write must
// affect zero rows — no duplicate event, and never a regression of the
// winner's already-committed state.
func TestPollConcurrentRaceNeverDuplicatesSettlementOrRegressesState(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req1 := newLiveRequest(t, svc, "race1", *clock)
	// req2 is an independent snapshot of the SAME row, read before either
	// worker has written — exactly the shape two concurrent
	// Branch()/Get() calls produce.
	req2, err := svc.Get(ctx, req1.ID)
	if err != nil || req2 == nil {
		t.Fatalf("get: %v, %v", req2, err)
	}

	client := sequencedIlliadServer(t, nil,
		txnResponse(illiadStatusDeliveredToWeb, 555),
		txnResponse(illiadStatusDeliveredToWeb, 555),
	)

	result1, err := svc.Poll(ctx, req1, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !result1.Settled || result1.State != StateFulfilled {
		t.Fatalf("winner's poll = %+v, want settled fulfilled", result1)
	}

	result2, err := svc.Poll(ctx, req2, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result2.Settled {
		t.Fatalf("loser's poll from a stale snapshot must lose the CAS race, not re-settle: %+v", result2)
	}

	got, err := svc.Get(ctx, req1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFulfilled {
		t.Fatalf("state = %q after the losing write, want the winner's fulfilled preserved (no regression)", got.State)
	}

	var n int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = ?`,
		"race1", eventKindFulfilled).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("delivery.fulfilled events = %d, want exactly 1 despite the race", n)
	}
}

func TestPollUnmappedCustomStatusStaysPendingWithEvent(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "custom1", *clock)
	client := sequencedIlliadServer(t, nil, txnResponse("Awaiting Request Processing", 555))

	result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled || result.State != StatePending {
		t.Fatalf("result = %+v, want unsettled pending", result)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want pending", got.State)
	}
	if got.ProviderStatusRaw != "Awaiting Request Processing" {
		t.Fatalf("provider_status_raw = %q, want raw custom status persisted", got.ProviderStatusRaw)
	}
	if got.NextCheckAt == "" {
		t.Fatalf("next_check_at empty, want a rescheduled poll")
	}
	var n int
	if err := svc.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = ?`,
		"custom1", eventKindProviderStatusUnmapped).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("delivery.provider_status_unmapped events = %d, want 1", n)
	}
}

// --- Request Finished tri-branch ----------------------------------------

func TestPollRequestFinishedTriBranch(t *testing.T) {
	t.Run("fulfilled evidence", func(t *testing.T) {
		svc, clock := testServiceClock(t)
		ctx := context.Background()
		req := newLiveRequest(t, svc, "rf-fulfilled", *clock)
		client := sequencedIlliadServer(t, nil,
			txnResponse(illiadStatusDeliveredToWeb, 555),
			txnResponse(illiadStatusRequestFinished, 555),
		)
		if _, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60}); err != nil {
			t.Fatal(err)
		}
		req, err := svc.Get(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Fulfilled already stopped polling; force the row live again to
		// exercise the Request Finished leaf directly (a fulfilled row
		// never actually gets a second poll in production — Branch routes
		// it to adopt_fulfilled instead).
		if err := svc.UpdateState(ctx, req.ID, StateSubmitted); err != nil {
			t.Fatal(err)
		}
		if err := svc.RecordPoll(ctx, req.ID, "555", clock.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		req, err = svc.Get(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Settled || result.State != StateFulfilled {
			t.Fatalf("result = %+v, want settled fulfilled from prior evidence", result)
		}
	})

	t.Run("cancellation evidence", func(t *testing.T) {
		svc, clock := testServiceClock(t)
		ctx := context.Background()
		req := newLiveRequest(t, svc, "rf-cancelled", *clock)
		client := sequencedIlliadServer(t, nil,
			txnResponse(illiadStatusCancelledByCustomer, 555),
			txnResponse(illiadStatusRequestFinished, 555),
		)
		// First poll: direct classification, no tri-branch involved yet.
		if _, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60}); err != nil {
			t.Fatal(err)
		}
		req, err := svc.Get(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != StateCancelled {
			t.Fatalf("state after Cancelled by Customer = %q, want cancelled", req.State)
		}
		// ILLiad later reports the same transaction as Request Finished
		// (its terminal housekeeping status); the row's own
		// ProviderDisplayStatus already recorded the cancellation, so the
		// tri-branch must resolve from that evidence, not settle
		// unknown_outcome.
		if err := svc.UpdateState(ctx, req.ID, StateSubmitted); err != nil {
			t.Fatal(err)
		}
		if err := svc.RecordPoll(ctx, req.ID, "555", clock.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		req, err = svc.Get(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Settled || result.State != StateCancelled {
			t.Fatalf("result = %+v, want settled cancelled from prior evidence", result)
		}
	})

	t.Run("no evidence settles unknown_outcome only after one delayed pass", func(t *testing.T) {
		svc, clock := testServiceClock(t)
		ctx := context.Background()
		req := newLiveRequest(t, svc, "rf-unknown", *clock)
		client := sequencedIlliadServer(t, nil,
			txnResponse(illiadStatusRequestFinished, 555),
			txnResponse(illiadStatusRequestFinished, 555),
		)

		// First pass: no evidence yet — must not settle.
		result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
		if err != nil {
			t.Fatal(err)
		}
		if result.Settled {
			t.Fatalf("first Request Finished pass settled early: %+v", result)
		}
		req, err = svc.Get(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != StateSubmitted {
			t.Fatalf("state = %q, want unchanged (still submitted)", req.State)
		}
		if req.NextCheckAt == "" {
			t.Fatalf("next_check_at empty, want a delayed reconciliation pass scheduled")
		}

		// Advance the clock to the delayed pass and poll again. req's
		// NextCheckAt already carries the real stored value from the
		// Get() above (persistPollSuccess wrote it verbatim) — advancing
		// the clock past it, rather than fabricating a different string,
		// keeps the CAS predicate matching the true row.
		*clock = clock.Add(requestFinishedReconciliationDelay + time.Second)
		result, err = svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Settled || result.State != StateUnknownOutcome {
			t.Fatalf("result = %+v, want settled unknown_outcome after the delayed pass", result)
		}
	})
}

// --- Failure discipline: state never changes on a failed poll ----------

func TestPollFailureClassesLeaveStateUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		responder http.HandlerFunc
		wantClass string
	}{
		{"transient 5xx", statusOnly(http.StatusInternalServerError), PollErrorClassTransient},
		{"transient 429", statusOnly(http.StatusTooManyRequests), PollErrorClassTransient},
		{"credential 401", statusOnly(http.StatusUnauthorized), PollErrorClassCredential},
		{"credential 403", statusOnly(http.StatusForbidden), PollErrorClassCredential},
		{"decode/schema failure", invalidJSON, PollErrorClassContractDrift},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, clock := testServiceClock(t)
			ctx := context.Background()
			req := newLiveRequest(t, svc, "fail-"+c.wantClass+"-"+c.name, *clock)
			client := sequencedIlliadServer(t, nil, c.responder)

			result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
			if err != nil {
				t.Fatal(err)
			}
			if result.Settled {
				t.Fatalf("a failed poll must never settle: %+v", result)
			}
			if result.State != StateSubmitted {
				t.Fatalf("state = %q, want unchanged submitted", result.State)
			}
			got, err := svc.Get(ctx, req.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != StateSubmitted {
				t.Fatalf("persisted state = %q, want unchanged submitted", got.State)
			}
			if got.ConsecutivePollFailures != 1 {
				t.Fatalf("consecutive_poll_failures = %d, want 1", got.ConsecutivePollFailures)
			}
			if got.LastPollErrorClass != c.wantClass {
				t.Fatalf("last_poll_error_class = %q, want %q", got.LastPollErrorClass, c.wantClass)
			}
			if got.LastSuccessfulPollAt != "" {
				t.Fatalf("last_successful_poll_at = %q, want still unset after a failure", got.LastSuccessfulPollAt)
			}
			if got.NextCheckAt == "" {
				t.Fatalf("next_check_at empty, want a rescheduled retry")
			}
		})
	}
}

func TestPollContractDriftDisablesPolling(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "drift1", *clock)
	client := sequencedIlliadServer(t, nil, invalidJSON)

	if _, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, got.NextCheckAt)
	if err != nil {
		t.Fatalf("next_check_at %q did not parse: %v", got.NextCheckAt, err)
	}
	if next.Sub(*clock) < 24*time.Hour {
		t.Fatalf("contract-drift next_check_at only %s out, want polling effectively disabled", next.Sub(*clock))
	}
}

// --- 404 propagation delay and reconciliation ---------------------------

func TestPoll404BeforeAnySuccessIsPropagationDelayNotUnknownOutcome(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "404-fresh", *clock)
	client := sequencedIlliadServer(t, nil, statusOnly(http.StatusNotFound))

	result, err := svc.Poll(ctx, req, PollDeps{Client: client, StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled {
		t.Fatalf("a fresh submission's first 404 must never settle: %+v", result)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSubmitted {
		t.Fatalf("state = %q, want unchanged submitted", got.State)
	}
	if got.LastPollErrorClass != PollErrorClassNotFoundPropagationDelay {
		t.Fatalf("last_poll_error_class = %q, want %q", got.LastPollErrorClass, PollErrorClassNotFoundPropagationDelay)
	}
}

func TestPoll404ReconciliationRecoversViaUserRequests(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "404-reconcile-found", *clock)
	// Simulate an earlier successful poll (last_successful_poll_at set).
	if _, err := svc.store.DB().ExecContext(ctx,
		`UPDATE delivery_requests SET last_successful_poll_at = ? WHERE id = ?`, clock.Format(time.RFC3339Nano), req.ID); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	userRequests := []illiad.Transaction{
		{TransactionNumber: 999, TransactionStatus: illiadStatusAwaitingUnfilled, ItemInfo4: req.IdempotencyKey},
	}
	client := sequencedIlliadServer(t, userRequests, statusOnly(http.StatusNotFound))

	result, err := svc.Poll(ctx, req, PollDeps{Client: client, PatronRef: "patron1", ReferenceField: "ItemInfo4", StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled {
		t.Fatalf("a successful reconciliation must not settle unknown_outcome: %+v", result)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderReference != "999" {
		t.Fatalf("provider_reference = %q, want the reconciled 999", got.ProviderReference)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want pending (recovered transaction is awaiting unfilled processing)", got.State)
	}
}

func TestPoll404UnreconciledSettlesUnknownOutcomeOnlyAfterDelayedRecheck(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "404-reconcile-absent", *clock)
	if _, err := svc.store.DB().ExecContext(ctx,
		`UPDATE delivery_requests SET last_successful_poll_at = ? WHERE id = ?`, clock.Format(time.RFC3339Nano), req.ID); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	// UserRequests returns transactions, but none carry this row's
	// idempotency key.
	userRequests := []illiad.Transaction{
		{TransactionNumber: 42, TransactionStatus: illiadStatusAwaitingUnfilled, ItemInfo4: "someone-elses-key"},
	}
	client := sequencedIlliadServer(t, userRequests, statusOnly(http.StatusNotFound))

	result, err := svc.Poll(ctx, req, PollDeps{Client: client, PatronRef: "patron1", ReferenceField: "ItemInfo4", StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled {
		t.Fatalf("first 404-after-success must wait for the delayed recheck, not settle immediately: %+v", result)
	}
	req, err = svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if req.LastPollErrorClass != PollErrorClassNotFoundReconciling {
		t.Fatalf("last_poll_error_class = %q, want %q", req.LastPollErrorClass, PollErrorClassNotFoundReconciling)
	}

	// Advance to the delayed recheck: still 404 (client only has one
	// canned response), so this must be the call that finally settles
	// unknown_outcome. req's NextCheckAt already carries the real stored
	// value from the Get() above — advance the clock past it rather than
	// fabricating a different string, so the CAS predicate still matches
	// the true row.
	*clock = clock.Add(notFoundReconciliationRecheckDelay + time.Second)
	result, err = svc.Poll(ctx, req, PollDeps{Client: client, PatronRef: "patron1", ReferenceField: "ItemInfo4", StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Settled || result.State != StateUnknownOutcome {
		t.Fatalf("result = %+v, want settled unknown_outcome after the delayed recheck", result)
	}
}

// --- Backoff growth is bounded -------------------------------------------

func TestBackoffWithJitterGrowsAndIsBounded(t *testing.T) {
	svc, _ := testServiceClock(t)
	svc.jitter = func(time.Duration) time.Duration { return 0 } // deterministic bounds check
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var prev time.Duration
	for attempt := 1; attempt <= 12; attempt++ {
		next := svc.backoffWithJitter(now, attempt, 60)
		got := next.Sub(now)
		if got < prev {
			t.Fatalf("attempt %d backoff %s shrank from %s", attempt, got, prev)
		}
		if got > maxPollInterval {
			t.Fatalf("attempt %d backoff %s exceeds maxPollInterval %s", attempt, got, maxPollInterval)
		}
		prev = got
	}
	if prev != maxPollInterval {
		t.Fatalf("backoff never reached the maxPollInterval cap: last = %s", prev)
	}
}

func TestBackoffJitterStaysWithinBudget(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := NextCheck(now, 2, 60)
	interval := base.Sub(now)
	for range 50 {
		j := defaultJitter(interval)
		if j < 0 || j > interval/5 {
			t.Fatalf("jitter %s out of [0, %s] budget", j, interval/5)
		}
	}
}
