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
	"strings"
	"testing"
	"time"

	"papio/internal/illiad"
	"papio/internal/store"
	"papio/internal/store/storetest"
)

// testServiceClock is testService with a mutable clock, so a test can
// advance "now" between successive Poll calls (backoff growth, the
// Request Finished delayed reconciliation pass, 404 propagation).
func testServiceClock(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
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
// once exhausted). userRequests, when non-nil, answers the patron-resolution
// and Transaction/UserRequests calls regardless of sequence position.
func sequencedIlliadServer(t *testing.T, userRequests []illiad.Transaction, responses ...http.HandlerFunc) *illiad.Client {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userRequests != nil {
			switch {
			case strings.HasPrefix(r.URL.Path, "/Transaction/UserRequests/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(userRequests)
				return
			case strings.HasPrefix(r.URL.Path, "/Users/ExternalUserId/"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"UserName":"patron1"}`))
				return
			}
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

func TestPoll404DuplicateTokenSettlesUnknownForHumanReconciliation(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "404-reconcile-duplicate", *clock)
	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE delivery_requests SET last_successful_poll_at = ? WHERE id = ?`, clock.Format(time.RFC3339Nano), req.ID); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	userRequests := []illiad.Transaction{
		{TransactionNumber: 1, TransactionStatus: illiadStatusAwaitingUnfilled, ItemInfo4: req.IdempotencyKey},
		{TransactionNumber: 2, TransactionStatus: illiadStatusAwaitingUnfilled, ItemInfo4: req.IdempotencyKey},
	}
	client := sequencedIlliadServer(t, userRequests, statusOnly(http.StatusNotFound))
	result, err := svc.Poll(ctx, req, PollDeps{Client: client, PatronRef: "patron1", ReferenceField: "ItemInfo4", StatusPollMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Settled || result.State != StateUnknownOutcome {
		t.Fatalf("result = %+v, want settled unknown_outcome", result)
	}
}

// TestPoll404AfterSuccessWithNoPatronRefSpendsOneRecheckThenSettlesUnknownOutcome pins
// the untested early return in reconcileViaUserRequests where PatronRef (or
// ReferenceField) is empty. With no patron_ref configured there is nothing to
// reconcile against, so papio does not silently keep polling forever — it spends
// the one delayed recheck and settles unknown_outcome. papio doctor is where
// the missing patron_ref is surfaced (doctor.go warns on it).
// Note: the recorded reason still reads "404_after_reconciliation" even though
// no reconciliation ran; that is the current wire vocabulary and is pinned here
// deliberately rather than being a bug to fix silently.
func TestPoll404AfterSuccessWithNoPatronRefSpendsOneRecheckThenSettlesUnknownOutcome(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	req := newLiveRequest(t, svc, "404-no-patron-ref", *clock)
	if _, err := svc.store.DB().ExecContext(ctx,
		`UPDATE delivery_requests SET last_successful_poll_at = ? WHERE id = ?`, clock.Format(time.RFC3339Nano), req.ID); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}

	var userRequestsHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/Transaction/UserRequests/") || strings.HasPrefix(r.URL.Path, "/Users/ExternalUserId/") {
			userRequestsHit = true
			t.Errorf("UserRequests listing was called despite empty PatronRef — reconcileViaUserRequests should have early-returned without any listing call (path %q)", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	client := illiad.New(srv.Client(), srv.URL, "key")

	deps := PollDeps{Client: client, PatronRef: "", ReferenceField: "ItemInfo4", StatusPollMinutes: 60}

	// First 404-after-success: must NOT settle, must record the reconciling
	// class, and must schedule the fixed recheck (not exponential backoff).
	firstNow := *clock
	result, err := svc.Poll(ctx, req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Settled {
		t.Fatalf("first 404-after-success with empty PatronRef must not settle — it should spend the delayed recheck: %+v", result)
	}
	if userRequestsHit {
		t.Fatalf("UserRequests listing was called on first 404 despite empty PatronRef")
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == StateUnknownOutcome {
		t.Fatalf("state = %q, want still live (not unknown_outcome) after first 404", got.State)
	}
	if got.LastPollErrorClass != PollErrorClassNotFoundReconciling {
		t.Fatalf("last_poll_error_class = %q, want %q", got.LastPollErrorClass, PollErrorClassNotFoundReconciling)
	}
	if got.NextCheckAt == "" {
		t.Fatalf("next_check_at is empty after first 404, want fixed recheck delay %s", notFoundReconciliationRecheckDelay)
	}
	nextAt, err := time.Parse(time.RFC3339Nano, got.NextCheckAt)
	if err != nil {
		t.Fatalf("parse next_check_at %q: %v", got.NextCheckAt, err)
	}
	expectedNext := firstNow.Add(notFoundReconciliationRecheckDelay)
	if !nextAt.Equal(expectedNext) {
		t.Fatalf("next_check_at = %s, want %s (fixed notFoundReconciliationRecheckDelay, not exponential backoff)", nextAt, expectedNext)
	}
	// Also verify Poll's returned NextCheckAt matches the persisted one.
	if !result.NextCheckAt.Equal(expectedNext) {
		t.Fatalf("PollResult NextCheckAt = %s, want %s", result.NextCheckAt, expectedNext)
	}
	// Keep the mutated req in sync for the second Poll (CAS predicate uses State/NextCheckAt).
	req = got

	// Advance to the delayed recheck: still 404, so this must be the call that settles unknown_outcome.
	*clock = clock.Add(notFoundReconciliationRecheckDelay + time.Second)

	result, err = svc.Poll(ctx, req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Settled || result.State != StateUnknownOutcome {
		t.Fatalf("result = %+v, want settled unknown_outcome after the delayed recheck", result)
	}
	if userRequestsHit {
		t.Fatalf("UserRequests listing was called on second 404 despite empty PatronRef — second 404 settles without reconciliation")
	}
	final, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != StateUnknownOutcome {
		t.Fatalf("state = %q, want %q after second 404", final.State, StateUnknownOutcome)
	}
	if final.NextCheckAt != "" {
		t.Fatalf("next_check_at = %q, want empty (polling stopped) after settling unknown_outcome", final.NextCheckAt)
	}
	var detailJSON string
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT detail_json FROM events WHERE job_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		req.JobID, eventKindPollSettled).Scan(&detailJSON); err != nil {
		t.Fatalf("query poll_settled event: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		t.Fatalf("unmarshal event detail %q: %v", detailJSON, err)
	}
	if detail["reason"] != "404_after_reconciliation" {
		t.Fatalf("event reason = %v, want %q (pinned wire vocabulary even when no reconciliation ran)", detail["reason"], "404_after_reconciliation")
	}
	if detail["state"] != string(StateUnknownOutcome) {
		t.Fatalf("event state = %v, want %q", detail["state"], string(StateUnknownOutcome))
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

// The zero-jitter test above cannot see the ceiling breach: NextCheck already
// caps the interval at maxPollInterval, so adding jitter on top pushed the
// scheduled check past the documented 24h ceiling by up to the jitter budget.
func TestBackoffWithJitterNeverExceedsMaxPollInterval(t *testing.T) {
	svc, _ := testServiceClock(t)
	svc.jitter = func(interval time.Duration) time.Duration { return interval } // worst case
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for attempt := 1; attempt <= 12; attempt++ {
		if got := svc.backoffWithJitter(now, attempt, 60).Sub(now); got > maxPollInterval {
			t.Fatalf("attempt %d backoff %s exceeds maxPollInterval %s", attempt, got, maxPollInterval)
		}
	}
	// Below the cap the jitter must still be applied, otherwise every row
	// retries in lockstep against the same outage.
	low := svc.backoffWithJitter(now, 1, 60)
	if !low.After(NextCheck(now, 1, 60)) {
		t.Fatalf("jitter was not applied below the cap: %s", low.Sub(now))
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

// --- LivePollHealth: doctor's poll-health thresholds ---------------------

// seedPollHealthRow creates one delivery_requests row under profile and
// stamps exactly the columns LivePollHealth reads. A zero lastSuccess
// leaves last_successful_poll_at NULL, so the freshness anchor falls back
// to submitted_at — a request papio submitted but never polled once.
func seedPollHealthRow(t *testing.T, svc *Service, jobID, profile string, state State, failures int, errorClass string, lastSuccess, submittedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	testJob(t, svc, jobID)
	created, err := svc.Create(ctx, CreateRequest{
		JobID:              jobID,
		InstitutionProfile: profile,
		Provider:           "illiad",
		RequestClass:       "digital_journal_article",
		WorkIdentity:       "doi:10.1000/" + jobID,
		GateProfileDigest:  "digest",
	})
	if err != nil {
		t.Fatalf("create %s: %v", jobID, err)
	}
	if err := svc.UpdateState(ctx, created.ID, state); err != nil {
		t.Fatalf("update state %s: %v", jobID, err)
	}
	var success, class any
	if !lastSuccess.IsZero() {
		success = lastSuccess.UTC().Format(time.RFC3339Nano)
	}
	if errorClass != "" {
		class = errorClass
	}
	if _, err := svc.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET consecutive_poll_failures = ?, last_poll_error_class = ?,
		    last_successful_poll_at = ?, submitted_at = ?
		WHERE id = ?`,
		failures, class, success, submittedAt.UTC().Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatalf("seed poll health %s: %v", jobID, err)
	}
	return created.ID
}

// pollHealthByID indexes LivePollHealth's rows by request id, so a test
// asserts per-row classification without depending on row order.
func pollHealthByID(t *testing.T, svc *Service, profile string) map[int64]PollHealth {
	t.Helper()
	rows, err := svc.LivePollHealth(context.Background(), profile)
	if err != nil {
		t.Fatalf("LivePollHealth: %v", err)
	}
	out := make(map[int64]PollHealth, len(rows))
	for _, h := range rows {
		if _, dup := out[h.RequestID]; dup {
			t.Fatalf("LivePollHealth returned request %d twice", h.RequestID)
		}
		out[h.RequestID] = h
	}
	return out
}

func TestLivePollHealthFreshRowIsHealthyAndScoped(t *testing.T) {
	svc, clock := testServiceClock(t)
	now := *clock
	fresh := seedPollHealthRow(t, svc, "ph-fresh", "default", StateSubmitted, 0, "",
		now.Add(-time.Minute), now.Add(-2*time.Hour))
	// A settled row and another institution's row are not part of this
	// profile's live poll loop; reporting either would put a request
	// papio no longer polls into doctor's document_delivery section.
	settled := seedPollHealthRow(t, svc, "ph-settled", "default", StateFulfilled, 9, "contract_drift",
		time.Time{}, now.Add(-72*time.Hour))
	foreign := seedPollHealthRow(t, svc, "ph-foreign", "campus", StateSubmitted, 9, "contract_drift",
		time.Time{}, now.Add(-72*time.Hour))

	byID := pollHealthByID(t, svc, "default")
	if _, ok := byID[settled]; ok {
		t.Fatalf("settled request %d reported as live poll health", settled)
	}
	if _, ok := byID[foreign]; ok {
		t.Fatalf("request %d from another institution_profile reported", foreign)
	}
	if len(byID) != 1 {
		t.Fatalf("LivePollHealth returned %d rows, want only the one live default-profile row", len(byID))
	}
	h, ok := byID[fresh]
	if !ok {
		t.Fatalf("live request %d missing from LivePollHealth", fresh)
	}
	if h.Degraded {
		t.Fatalf("fresh row with 0 failures reported Degraded")
	}
	if h.Unobservable {
		t.Fatalf("fresh row polled 1m ago reported Unobservable")
	}
	if h.Provider != "illiad" || h.State != StateSubmitted {
		t.Fatalf("provider/state = %q/%q, want illiad/submitted", h.Provider, h.State)
	}
	if h.ConsecutivePollFailures != 0 || h.LastPollErrorClass != "" {
		t.Fatalf("failures/class = %d/%q, want 0/empty", h.ConsecutivePollFailures, h.LastPollErrorClass)
	}
	if want := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano); h.LastSuccessfulPollAt != want {
		t.Fatalf("last_successful_poll_at = %q, want %q", h.LastSuccessfulPollAt, want)
	}
}

// Degraded turns on at the third consecutive failure, not the second: one
// off-by-one here either flags a poll loop that is merely retrying or
// hides a loop that has stopped observing the provider.
func TestLivePollHealthDegradedBoundaryAtThreeConsecutiveFailures(t *testing.T) {
	svc, clock := testServiceClock(t)
	now := *clock
	cases := []struct {
		job      string
		state    State
		failures int
		want     bool
	}{
		{"ph-deg-0", StateSubmitted, 0, false},
		{"ph-deg-2", StateSubmitted, 2, false},
		{"ph-deg-3", StatePending, 3, true},
		{"ph-deg-4", StateSubmitted, 4, true},
	}
	ids := make(map[string]int64, len(cases))
	for _, c := range cases {
		class := ""
		if c.failures > 0 {
			class = "transport"
		}
		// Fresh last success on every row, so Degraded is the only
		// classification under test here.
		ids[c.job] = seedPollHealthRow(t, svc, c.job, "default", c.state, c.failures, class,
			now.Add(-time.Minute), now.Add(-2*time.Hour))
	}

	byID := pollHealthByID(t, svc, "default")
	if len(byID) != len(cases) {
		t.Fatalf("LivePollHealth returned %d rows, want %d (submitted and pending are both live)", len(byID), len(cases))
	}
	for _, c := range cases {
		h, ok := byID[ids[c.job]]
		if !ok {
			t.Fatalf("%s: request %d missing from LivePollHealth", c.job, ids[c.job])
		}
		if h.Degraded != c.want {
			t.Fatalf("%s: %d consecutive failures -> Degraded = %v, want %v", c.job, c.failures, h.Degraded, c.want)
		}
		if h.Unobservable {
			t.Fatalf("%s: fresh last success reported Unobservable", c.job)
		}
		if h.ConsecutivePollFailures != c.failures {
			t.Fatalf("%s: failures = %d, want %d", c.job, h.ConsecutivePollFailures, c.failures)
		}
	}
}

// Unobservable is strictly more than 24h since the last success, so a row
// polled exactly 24h ago is still observed.
func TestLivePollHealthUnobservableBoundaryAtTwentyFourHours(t *testing.T) {
	svc, clock := testServiceClock(t)
	now := *clock
	cases := []struct {
		job   string
		since time.Duration
		want  bool
	}{
		{"ph-obs-under", 24*time.Hour - time.Nanosecond, false},
		{"ph-obs-exact", 24 * time.Hour, false},
		{"ph-obs-over", 24*time.Hour + time.Nanosecond, true},
		{"ph-obs-stale", 72 * time.Hour, true},
	}
	ids := make(map[string]int64, len(cases))
	for _, c := range cases {
		// submitted_at is deliberately ancient: last_successful_poll_at
		// is the anchor whenever it is set.
		ids[c.job] = seedPollHealthRow(t, svc, c.job, "default", StateSubmitted, 0, "",
			now.Add(-c.since), now.Add(-30*24*time.Hour))
	}

	byID := pollHealthByID(t, svc, "default")
	for _, c := range cases {
		h, ok := byID[ids[c.job]]
		if !ok {
			t.Fatalf("%s: request %d missing from LivePollHealth", c.job, ids[c.job])
		}
		if h.Unobservable != c.want {
			t.Fatalf("%s: last success %s ago -> Unobservable = %v, want %v", c.job, c.since, h.Unobservable, c.want)
		}
		if h.Degraded {
			t.Fatalf("%s: 0 consecutive failures reported Degraded", c.job)
		}
	}
}

// Until the first successful poll there is no last_successful_poll_at, so
// the 24h blind-spot window runs from submission instead. Without that
// fallback a request submitted days ago and never once polled would look
// perfectly healthy.
func TestLivePollHealthUnobservableAnchorsOnSubmittedAtUntilFirstSuccess(t *testing.T) {
	svc, clock := testServiceClock(t)
	now := *clock
	underID := seedPollHealthRow(t, svc, "ph-sub-exact", "default", StateSubmitted, 0, "",
		time.Time{}, now.Add(-24*time.Hour))
	overID := seedPollHealthRow(t, svc, "ph-sub-over", "default", StateSubmitted, 0, "",
		time.Time{}, now.Add(-24*time.Hour-time.Nanosecond))

	byID := pollHealthByID(t, svc, "default")
	under, ok := byID[underID]
	if !ok {
		t.Fatalf("request %d missing from LivePollHealth", underID)
	}
	if under.Unobservable {
		t.Fatalf("never-polled row submitted exactly 24h ago reported Unobservable")
	}
	if under.LastSuccessfulPollAt != "" {
		t.Fatalf("last_successful_poll_at = %q, want empty for a never-polled row", under.LastSuccessfulPollAt)
	}
	over, ok := byID[overID]
	if !ok {
		t.Fatalf("request %d missing from LivePollHealth", overID)
	}
	if !over.Unobservable {
		t.Fatalf("never-polled row submitted more than 24h ago reported observable")
	}
}

// A pending row that never reached submission has no anchor at all
// (submitted_at and last_successful_poll_at are both NULL). papio has no
// elapsed time to measure there, so it must not claim a blind spot.
func TestLivePollHealthWithoutAnchorIsNotUnobservable(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	id := seedPollHealthRow(t, svc, "ph-no-anchor", "default", StatePending, 1, "transport",
		time.Time{}, clock.Add(-30*24*time.Hour))
	if _, err := svc.store.DB().ExecContext(ctx,
		`UPDATE delivery_requests SET submitted_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("clear submitted_at: %v", err)
	}

	h, ok := pollHealthByID(t, svc, "default")[id]
	if !ok {
		t.Fatalf("pending request %d missing from LivePollHealth", id)
	}
	if h.Unobservable {
		t.Fatalf("row with no submitted_at and no successful poll reported Unobservable")
	}
	if h.Degraded {
		t.Fatalf("1 consecutive failure reported Degraded")
	}
}
