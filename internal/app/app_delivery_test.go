// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0017 Decisions 3B/4: document-delivery routing at exhaustedCandidates'
// candidate-exhaustion boundary (deliveryRoute and its helpers in app.go).
package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/store"
)

// newDeliveryTestService builds an app.Service backed by a real, temporary
// SQLite store (the same setup newTestService in app_test.go uses) plus a
// delivery.Service over that same store, so tests can inspect
// delivery_requests rows directly rather than only observing job state.
// Fetch/Validate are wired to fail the test if ever called: every test here
// exhausts candidates by resolving zero of them, so the fetch/validate loop
// must never run.
func newDeliveryTestService(t *testing.T) (*Service, *job.Store, *delivery.Service) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	artifacts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.DataDir = data
	cfg.Sources["fixture"] = config.Source{Enabled: true}
	svc := New(cfg, &job.Store{S: db}, artifacts, nil)
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		t.Fatal("fetch must not run: every delivery test resolves zero candidates")
		return fetch.Result{}, nil
	}
	svc.Validate = passValidation()
	deliverySvc := delivery.New(db, &svc.Config, nil)
	return svc, svc.Jobs, deliverySvc
}

// deliveryWorkRequest is a DOI+title work: it carries deliveryHasRequiredFields'
// citation minimum (DOI or PMID plus title) and deliveryRequestClass's DOI/PMID
// signal, so a fully auto-capable profile reaches the submit verdict.
func deliveryWorkRequest(id, doi string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     id,
		Identifiers:   &protocol.Identifiers{DOI: doi},
		Title:         "A Grounded Result",
		Authors:       []string{"A. Author"},
		Year:          2024,
	}
}

// deliveryTestResolvers makes svc.resolve() return zero legal candidates —
// the "no direct candidates" boundary exhaustedCandidates itself handles —
// without any institutional OpenURL base configured, so the very first pass
// lands on exhaustedCandidates' default branch (no openurl_handoff to try
// first) where deliveryRoute is called.
func deliveryTestResolvers() []ResolverEntry {
	return []ResolverEntry{{
		Adapter: &fakeResolver{name: "fixture"},
		Policy:  config.Source{Enabled: true},
	}}
}

// autoCapableDocumentDelivery is a document_delivery block that compiles
// GateClassAutoCapable once live acceptance is recorded (ADR-0017 Decision
// 3A's three hard rules): illiad provider, digital_journal_article,
// auto_if_unconditional, zero patron fee, no per-request declaration, both
// credential fields set.
func autoCapableDocumentDelivery(baseURL string) *config.DocumentDelivery {
	return &config.DocumentDelivery{
		Kind:              "illiad",
		BaseURL:           baseURL,
		SubmitPolicy:      "auto_if_unconditional",
		RequestClasses:    []string{"digital_journal_article"},
		LegalBasis:        "institution_policy",
		PatronAttestation: "not_required",
		PatronFeePolicy:   "zero_standard",
		MonthlyRequestCap: 25,
		StatusPollMinutes: 60,
		APIKey:            "campus-secret",
		PatronRef:         "patron-ref-1",
	}
}

func lastTransitionReason(t *testing.T, jobs *job.Store, id string) string {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "job.transition" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		reason, _ := detail["reason"].(string)
		return reason
	}
	t.Fatalf("job %s recorded no job.transition event", id)
	return ""
}

func gateEventCount(t *testing.T, jobs *job.Store, id string) int {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e["kind"] == "delivery.gate_evaluated" {
			n++
		}
	}
	return n
}

// TestExhaustedCandidatesNilDeliveryUnconfiguredRoute proves that with
// s.Delivery nil and no institutional OpenURL route configured either, a job
// that exhausts every candidate reaches exactly the pre-ADR-0017 terminal
// path: the caller's own terminal reason, untouched.
func TestExhaustedCandidatesNilDeliveryUnconfiguredRoute(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		t.Fatal("fetch must not run: this test resolves zero candidates")
		return fetch.Result{}, nil
	}
	svc.Validate = passValidation()
	svc.Resolvers = deliveryTestResolvers()
	id, err := svc.Submit(context.Background(), deliveryWorkRequest("wr_nil_delivery_001", "10.1000/nil-delivery-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable || got.TerminalReason != string(job.TerminalReasonNoLegalCandidates) {
		t.Fatalf("job = %+v, want unavailable/no_legal_candidates exactly as pre-ADR-0017", got)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — nil Delivery must never open a document_delivery action", actions)
	}
}

// TestExhaustedCandidatesNilDeliveryInstitutionalRouteExhausted proves the
// same byte-for-bit contract for the OTHER pre-ADR-0017 terminal path this
// file's default branch now also covers: an institutional OpenURL route that
// was actually offered and exhausted still collapses to NoEntitlement when
// s.Delivery is nil, exactly as before ADR-0017.
func TestExhaustedCandidatesNilDeliveryInstitutionalRouteExhausted(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	svc.Config.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	svc.Fetch = func(context.Context, resolver.Candidate, string) (fetch.Result, error) {
		t.Fatal("fetch must not run: this test resolves zero candidates")
		return fetch.Result{}, nil
	}
	svc.Validate = passValidation()
	svc.Resolvers = deliveryTestResolvers()
	id, err := svc.Submit(context.Background(), deliveryWorkRequest("wr_nil_delivery_002", "10.1000/nil-delivery-002"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the browser bridge's own institutional-route rediscovery
	// pass already having proved the route empty (bridge.go's
	// browser.no_entitlement_requeue), so institutionalRouteExhausted is
	// true on this very first Process pass.
	if err := jobs.RecordEvent(context.Background(), id, "browser.no_entitlement_requeue", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable || got.TerminalReason != string(job.TerminalReasonNoEntitlement) {
		t.Fatalf("job = %+v, want unavailable/no_entitlement exactly as pre-ADR-0017", got)
	}
}

// TestExhaustedCandidatesUnconfiguredProfileKeepsTerminalPath proves the
// same contract with s.Delivery wired but this job's institution profile
// carrying no document_delivery block: Configured must stay false, and the
// job's outcome is untouched.
func TestExhaustedCandidatesUnconfiguredProfileKeepsTerminalPath(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Resolvers = deliveryTestResolvers()
	id, err := svc.Submit(context.Background(), deliveryWorkRequest("wr_unconfigured_001", "10.1000/unconfigured-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(context.Background(), "w", time.Minute)
	if err := svc.Process(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable || got.TerminalReason != string(job.TerminalReasonNoLegalCandidates) {
		t.Fatalf("job = %+v, want unavailable/no_legal_candidates — unconfigured profile must not route to delivery", got)
	}
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
}

// TestExhaustedCandidatesAutoCapableProfileSubmitsAndParksPending is the
// configured-auto case: a profile compiled auto_capable submits through
// internal/illiad, carries the idempotency key in the configured reference
// field, creates a delivery_requests row in state submitted, and parks the
// job retry_wait/document_delivery_pending — never awaiting_human. One
// delivery.gate_evaluated event is appended.
func TestExhaustedCandidatesAutoCapableProfileSubmitsAndParksPending(t *testing.T) {
	var postedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&postedBody); err != nil {
			t.Fatalf("decode posted body: %v", err)
		}
		_, _ = w.Write([]byte(`{"TransactionNumber": 4821, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_auto_capable_001", "10.1000/auto-capable-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait || got.RetryAt == "" {
		t.Fatalf("job = %+v, want retry_wait parked with a schedule", got)
	}
	if got.TerminalReason != "" {
		t.Fatalf("terminal_reason = %q, want empty — a lodged request never makes a job terminal", got.TerminalReason)
	}
	if reason := lastTransitionReason(t, jobs, id); reason != string(job.RetryReasonDocumentDeliveryPending) {
		t.Fatalf("retry reason = %q, want %q", reason, job.RetryReasonDocumentDeliveryPending)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — a pending delivery is not an open action (Decision 4)", actions)
	}

	key := delivery.IdempotencyKey("default", "doi:10.1000/auto-capable-001", "illiad", "digital_journal_article")
	reqRow, err := deliverySvc.Lookup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if reqRow == nil {
		t.Fatal("no delivery_requests row was created")
	}
	if reqRow.State != delivery.StateSubmitted {
		t.Fatalf("delivery request state = %q, want submitted", reqRow.State)
	}
	if reqRow.ProviderReference != "4821" {
		t.Fatalf("provider reference = %q, want 4821", reqRow.ProviderReference)
	}
	if reqRow.SubmittedAt == "" {
		t.Fatal("submitted_at was never stamped")
	}

	if postedBody["ItemInfo4"] != key {
		t.Fatalf("posted ItemInfo4 = %v, want the idempotency key %q", postedBody["ItemInfo4"], key)
	}
	if postedBody["ExternalUserID"] != "patron-ref-1" {
		t.Fatalf("posted ExternalUserID = %v, want the configured patron_ref", postedBody["ExternalUserID"])
	}
	if postedBody["DOI"] != "10.1000/auto-capable-001" {
		t.Fatalf("posted DOI = %v", postedBody["DOI"])
	}

	if n := gateEventCount(t, jobs, id); n != 1 {
		t.Fatalf("delivery.gate_evaluated events = %d, want exactly 1", n)
	}
}

// TestExhaustedCandidatesPrefillOnlyProfileOpensAction is the
// configured-prefill case: a profile that never compiles auto_capable
// (kind = openurl is permanently prefill-only, ADR-0017 Decision 3A) opens
// the document_delivery human action and parks awaiting_human — it must
// never call internal/illiad.
func TestExhaustedCandidatesPrefillOnlyProfileOpensAction(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	const formURL = "https://ill.example.edu/request-form"
	svc.Config.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind:    "openurl",
		BaseURL: formURL,
	}
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_prefill_001", "10.1000/prefill-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != job.ActionKindDocumentDelivery {
		t.Fatalf("actions = %+v, want one document_delivery action", actions)
	}
	if actions[0].Detail != DeliveryPrefillActionDetail(formURL) {
		t.Fatalf("action detail = %q, want the prefill detail for %q", actions[0].Detail, formURL)
	}
	if actions[0].RequiresAuth {
		t.Fatalf("prefill action must not claim to require auth: %+v", actions[0])
	}

	key := delivery.IdempotencyKey("default", "doi:10.1000/prefill-001", "openurl", "digital_journal_article")
	reqRow, err := deliverySvc.Lookup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if reqRow == nil || reqRow.State != delivery.StateOffered {
		t.Fatalf("delivery request = %+v, want one row in state offered", reqRow)
	}

	if n := gateEventCount(t, jobs, id); n != 1 {
		t.Fatalf("delivery.gate_evaluated events = %d, want exactly 1", n)
	}
}

// TestExhaustedCandidatesReevaluationJoinsExistingPoll proves Decision 3B's
// idempotency branch: once a request is live (submitted/pending), a later
// re-evaluation of the SAME job must never create a second delivery_requests
// row — it joins the existing one and re-parks retry_wait pending, still
// without opening any human action.
func TestExhaustedCandidatesReevaluationJoinsExistingPoll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"TransactionNumber": 555, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_join_poll_001", "10.1000/join-poll-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	firstRow, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if firstRow.State != job.StateRetryWait {
		t.Fatalf("first pass job state = %q, want retry_wait", firstRow.State)
	}
	key := delivery.IdempotencyKey("default", "doi:10.1000/join-poll-001", "illiad", "digital_journal_article")
	firstDelivery, err := deliverySvc.Lookup(ctx, key)
	if err != nil || firstDelivery == nil {
		t.Fatalf("delivery request after first pass: %v, %v", firstDelivery, err)
	}

	// Force the job back to resolving — the same "re-evaluated while a
	// request is already live" scenario a manual retry or a scheduler wake
	// produces — and run it through the pipeline again.
	if err := jobs.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	row2, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row2); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait {
		t.Fatalf("second pass job state = %q, want retry_wait again (joined poll)", got.State)
	}
	if reason := lastTransitionReason(t, jobs, id); reason != string(job.RetryReasonDocumentDeliveryPending) {
		t.Fatalf("second pass retry reason = %q, want %q", reason, job.RetryReasonDocumentDeliveryPending)
	}
	secondDelivery, err := deliverySvc.Lookup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if secondDelivery.ID != firstDelivery.ID {
		t.Fatalf("second pass created a different delivery_requests row (%d != %d) — Decision 1 forbids a second live request", secondDelivery.ID, firstDelivery.ID)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — joining a poll never opens a human action", actions)
	}
	if n := gateEventCount(t, jobs, id); n != 1 {
		t.Fatalf("delivery.gate_evaluated events = %d, want exactly 1 (join_poll never re-evaluates the gate)", n)
	}
}

// TestSubmitDeliveryRequestTransportFailureThenRetrySubmitsOnce proves the
// P1 fix: a transport failure durably occupies the idempotency key (state
// offered, no provider_reference) and parks retry_wait/
// resolver_temporarily_unavailable — never a human action — and the next
// pass, once the provider recovers, retries submission against that SAME
// row rather than misrouting to reconciliation for a request that was
// never actually lodged. The vendor must see exactly one successful POST.
func TestSubmitDeliveryRequestTransportFailureThenRetrySubmitsOnce(t *testing.T) {
	var postCount int
	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			http.Error(w, "illiad unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"TransactionNumber": 9001, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = toggleFailureHTTPClient{fail: &failing, base: server.Client(), calls: &postCount}
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_transport_fail_001", "10.1000/transport-fail-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}

	afterFail, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if afterFail.State != job.StateRetryWait || afterFail.RetryAt == "" {
		t.Fatalf("job after transport failure = %+v, want retry_wait parked with a schedule", afterFail)
	}
	if reason := lastTransitionReason(t, jobs, id); reason != "resolver_temporarily_unavailable" {
		t.Fatalf("retry reason after transport failure = %q, want resolver_temporarily_unavailable", reason)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions after transport failure = %+v, want none — a transient outage must not require a human", actions)
	}

	key := delivery.IdempotencyKey("default", "doi:10.1000/transport-fail-001", "illiad", "digital_journal_article")
	offeredRow, err := deliverySvc.Lookup(ctx, key)
	if err != nil || offeredRow == nil {
		t.Fatalf("delivery request after transport failure: %v, %v", offeredRow, err)
	}
	if offeredRow.State != delivery.StateOffered || offeredRow.ProviderReference != "" {
		t.Fatalf("delivery request after transport failure = %+v, want offered with no provider_reference", offeredRow)
	}

	// The vendor recovers; the next pass must retry the SAME row.
	failing = false
	if err := jobs.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	row2, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row2); err != nil {
		t.Fatal(err)
	}

	final, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != job.StateRetryWait {
		t.Fatalf("final job state = %q, want retry_wait (now polling a submitted request)", final.State)
	}
	if reason := lastTransitionReason(t, jobs, id); reason != string(job.RetryReasonDocumentDeliveryPending) {
		t.Fatalf("final retry reason = %q, want %q", reason, job.RetryReasonDocumentDeliveryPending)
	}
	finalActions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalActions) != 0 {
		t.Fatalf("actions after successful retry = %+v, want none", finalActions)
	}

	finalReq, err := deliverySvc.Lookup(ctx, key)
	if err != nil || finalReq == nil {
		t.Fatalf("delivery request after retry: %v, %v", finalReq, err)
	}
	if finalReq.ID != offeredRow.ID {
		t.Fatalf("retry created a second delivery_requests row (%d != %d) — Decision 1 forbids a second live request", finalReq.ID, offeredRow.ID)
	}
	if finalReq.State != delivery.StateSubmitted || finalReq.ProviderReference != "9001" {
		t.Fatalf("delivery request after retry = %+v, want submitted with reference 9001", finalReq)
	}
	if postCount != 2 {
		t.Fatalf("illiad received %d requests, want exactly 2 (one failed, one succeeded) — never a duplicate live submission", postCount)
	}
}

func TestSubmitDeliveryAmbiguousFailureDoesNotRepost(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		http.Error(w, "provider outcome unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_ambiguous_submission", "10.1000/ambiguous-submission"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "worker", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", got.State)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("provider posts = %d, want exactly one ambiguous attempt", posts)
	}
}

// TestSubmitDeliveryRequestDuplicateLiveRowRoutesToReconciliation proves the
// P1 fix does not reopen the double-submission hole it closes: when Create
// loses a race to a row that already reached the provider (state
// submitted), submitDeliveryRequest must still route to reconciliation and
// must never call internal/illiad a second time for that row.
func TestSubmitDeliveryRequestDuplicateLiveRowRoutesToReconciliation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("illiad must never be called for a duplicate that already reached the provider")
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_dup_live_001", "10.1000/dup-live-001"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v, %v", claimed, err)
	}
	if err := jobs.Transition(ctx, claimed.ID, job.StateQueued, job.StateResolving, map[string]any{"reason": "test_setup"}); err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	profileName := deliveryProfileName(row.Policy.Resolver)
	requestClass := deliveryRequestClass(row.Work)
	workIdentity := row.Work.Describe()
	key := delivery.IdempotencyKey(profileName, workIdentity, "illiad", requestClass)

	// A concurrent evaluation (a different job, same idempotency key) has
	// already won the Create race and reached the provider — the exact
	// TOCTOU window between Branch's Lookup and this call's own Create.
	otherJobID, err := svc.Submit(ctx, deliveryWorkRequest("wr_dup_live_other_001", "10.1000/dup-live-other-001"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: otherJobID, InstitutionProfile: profileName, Provider: "illiad",
		RequestClass: requestClass, WorkIdentity: workIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deliverySvc.UpdateState(ctx, other.ID, delivery.StateSubmitted); err != nil {
		t.Fatal(err)
	}

	inst, dd, ok := svc.deliveryConfigured(row)
	if !ok {
		t.Fatal("delivery must be configured")
	}
	profile, err := deliverySvc.ResolveGateProfile(ctx, profileName, inst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.submitDeliveryRequest(ctx, row, job.StateResolving, profileName, dd, requestClass, workIdentity, key, profile); err != nil {
		t.Fatal(err)
	}

	parked, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if parked.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human (reconciliation)", parked.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].JobID != id || actions[0].Kind != job.ActionKindDocumentDelivery {
		t.Fatalf("actions = %+v, want one document_delivery reconciliation action on %s", actions, id)
	}

	rows, err := deliverySvc.Lookup(ctx, key)
	if err != nil || rows == nil || rows.ID != other.ID || rows.State != delivery.StateSubmitted {
		t.Fatalf("delivery request for key = %+v, %v, want the other job's untouched submitted row", rows, err)
	}
}

// TestCancelJobOrphansLiveDeliveryRequest proves the P2 fix: cancelling a
// job that is actively polling a live (submitted) delivery_requests row
// marks that row unknown_outcome with a durable event, rather than leaving
// it looking watched when nothing will ever poll it again.
func TestCancelJobOrphansLiveDeliveryRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"TransactionNumber": 4242, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_cancel_live_001", "10.1000/cancel-live-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	before, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != job.StateRetryWait {
		t.Fatalf("job before cancel = %+v, want retry_wait (polling a live request)", before)
	}
	key := delivery.IdempotencyKey("default", "doi:10.1000/cancel-live-001", "illiad", "digital_journal_article")
	beforeReq, err := deliverySvc.Lookup(ctx, key)
	if err != nil || beforeReq == nil || beforeReq.State != delivery.StateSubmitted {
		t.Fatalf("delivery request before cancel: %+v, %v, want state submitted", beforeReq, err)
	}

	if err := svc.CancelJob(ctx, id, job.TerminalReasonUserDismissed); err != nil {
		t.Fatal(err)
	}

	cancelled, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != job.StateCancelled {
		t.Fatalf("job state after CancelJob = %q, want cancelled", cancelled.State)
	}
	afterReq, err := deliverySvc.Lookup(ctx, key)
	if err != nil || afterReq == nil {
		t.Fatalf("delivery request after cancel: %v, %v", afterReq, err)
	}
	if afterReq.State != delivery.StateUnknownOutcome {
		t.Fatalf("delivery request state after cancelling its driving job = %q, want unknown_outcome", afterReq.State)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e["kind"] == "delivery.orphaned" {
			found = true
		}
	}
	if !found {
		t.Fatal("no delivery.orphaned event recorded for the cancelled job")
	}
}

// TestDismissDocumentDeliveryActionOrphansLiveDeliveryRequest proves the P2
// fix on the dismiss path: dismissing an open document_delivery action that
// cancels its parked job also reconciles a live delivery_requests row the
// job was associated with.
func TestDismissDocumentDeliveryActionOrphansLiveDeliveryRequest(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery("https://illiad.example.edu")
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_dismiss_live_001", "10.1000/dismiss-live-001"))
	if err != nil {
		t.Fatal(err)
	}

	// Directly wire the state deliveryRoute's own call sites never
	// currently combine (an open document_delivery action never coincides
	// with its OWN job owning a live submitted/pending row today — see
	// OrphanIfLive) so DismissAction's compensation wiring itself is
	// exercised in isolation, independent of whether today's routing logic
	// happens to reach it.
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1000/dismiss-live-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deliverySvc.UpdateState(ctx, req.ID, delivery.StateSubmitted); err != nil {
		t.Fatal(err)
	}

	actionID, err := jobs.OpenHumanAction(ctx, id, job.ActionKindDocumentDelivery,
		"a document-delivery request needs reconciliation", job.Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving,
		map[string]any{"reason": "test_setup"}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		map[string]any{"reason": "document_delivery_reconciliation"}); err != nil {
		t.Fatal(err)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var action *job.HumanAction
	for i := range actions {
		if actions[i].ID == actionID {
			action = &actions[i]
		}
	}
	if action == nil {
		t.Fatal("document_delivery action not found after opening it")
	}

	dismissedJobID, err := svc.DismissAction(ctx, action.ID, action.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if dismissedJobID != id {
		t.Fatalf("dismissed job id = %q, want %q", dismissedJobID, id)
	}

	cancelled, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != job.StateCancelled {
		t.Fatalf("job state after dismiss = %q, want cancelled", cancelled.State)
	}
	afterReq, err := deliverySvc.Get(ctx, req.ID)
	if err != nil || afterReq == nil {
		t.Fatalf("delivery request after dismiss: %v, %v", afterReq, err)
	}
	if afterReq.State != delivery.StateUnknownOutcome {
		t.Fatalf("live delivery request state after dismissing its job's action = %q, want unknown_outcome", afterReq.State)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e["kind"] == "delivery.orphaned" {
			found = true
		}
	}
	if !found {
		t.Fatal("no delivery.orphaned event recorded for the dismissed job")
	}
}

// TestSubmitDeliveryMatchesAutomaticRoute proves the explicit
// operator/RPC-triggered SubmitDelivery entrypoint runs the same
// Branch-then-gate path and job transition exhaustedCandidates' automatic
// call takes — it is a thin fetch-the-row-then-deliveryRoute wrapper, not a
// second implementation.
func TestSubmitDeliveryMatchesAutomaticRoute(t *testing.T) {
	var postedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&postedBody); err != nil {
			t.Fatalf("decode posted body: %v", err)
		}
		_, _ = w.Write([]byte(`{"TransactionNumber": 9001, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_submit_delivery_001", "10.1000/submit-delivery-001"))
	if err != nil {
		t.Fatal(err)
	}
	// Claim so the job leaves queued, then move it to resolving the way
	// Process's own queued case does (SubmitDelivery does not run the
	// pipeline itself — like Process, it expects a caller-owned lease and a
	// row already past the queued bookkeeping step).
	if _, err := jobs.ClaimNext(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}

	result, err := svc.SubmitDelivery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Configured || result.Branch != delivery.BranchEvaluateGate || result.Decision.Action != delivery.ActionSubmit {
		t.Fatalf("result = %+v, want Configured/evaluate_gate/submit", result)
	}
	if result.Request == nil || result.Request.State != delivery.StateSubmitted {
		t.Fatalf("result.Request = %+v, want a submitted row", result.Request)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait || got.RetryAt == "" {
		t.Fatalf("job = %+v, want retry_wait parked with a schedule", got)
	}
	if postedBody["DOI"] != "10.1000/submit-delivery-001" {
		t.Fatalf("posted DOI = %v", postedBody["DOI"])
	}
}

// TestExhaustedCandidatesConservativeModeRecordsDeliveryAdvisoryEvent proves
// ADR-0017 Decision 3B condition 1's conservative carve-out: a configured
// document-delivery route is discovered and recorded (a durable event) but
// never opened as an action or submitted — no Branch/gate evaluation, no
// delivery_requests row, no gate event, no human action at all.
func TestExhaustedCandidatesConservativeModeRecordsDeliveryAdvisoryEvent(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.AccessMode = config.ModeConservative
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery("https://illiad.example.edu/ILLiadWebPlatform")
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_conservative_delivery_001", "10.1000/conservative-delivery-001"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.ClaimNext(ctx, "w", time.Minute)
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateUnavailable {
		t.Fatalf("job state = %q, want unavailable — conservative mode never opens or submits", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — conservative mode records an event, never an action", actions)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e["kind"] != "delivery.route_discovered" {
			continue
		}
		found = true
		detail, _ := e["detail"].(map[string]any)
		if detail["provider"] != "illiad" {
			t.Fatalf("delivery.route_discovered detail = %+v, want provider illiad", detail)
		}
	}
	if !found {
		t.Fatal("no delivery.route_discovered event was recorded")
	}

	key := delivery.IdempotencyKey("default", "doi:10.1000/conservative-delivery-001", "illiad", "digital_journal_article")
	reqRow, err := deliverySvc.Lookup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if reqRow != nil {
		t.Fatalf("delivery request = %+v, want none — conservative mode never creates a row", reqRow)
	}
	if n := gateEventCount(t, jobs, id); n != 0 {
		t.Fatalf("delivery.gate_evaluated events = %d, want 0 — conservative mode never evaluates the gate", n)
	}
}

// TestSubmitDeliveryFulfilledWithPatronWebEnqueuesHandoff is the 2026-08-07
// amendment's core acceptance case: a fulfilled row with
// patron_web_base_url configured enqueues exactly one document_delivery
// openurl_handoff row carrying the form-75 "View PDF" URL and the provider
// reference, and parks the job awaiting_human — delegated mode drives (this
// test only asserts the action/event papio's own routing produced; whether
// the extension is actually offered the frame is internal/browser's own
// offerableAccessMode/offer() concern, exercised in bridge_test.go).
func TestSubmitDeliveryFulfilledWithPatronWebEnqueuesHandoff(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	dd := autoCapableDocumentDelivery("https://illiad.example.edu/ILLiadWebPlatform")
	dd.PatronWebBaseURL = "https://illiadweb.example.edu/illiad/illiad.dll"
	svc.Config.Browser.DocumentDelivery = dd
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_fulfilled_patronweb_001", "10.1000/fulfilled-patronweb-001"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimNext(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1000/fulfilled-patronweb-001",
		State: delivery.StateFulfilled, ProviderReference: "482910",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.SubmitDelivery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Configured || result.Branch != delivery.BranchAdoptFulfilled {
		t.Fatalf("result = %+v, want Configured/adopt_fulfilled", result)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", got.State)
	}

	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var forJob []job.HumanAction
	for _, a := range actions {
		if a.JobID == id {
			forJob = append(forJob, a)
		}
	}
	if len(forJob) != 1 {
		t.Fatalf("open actions for job = %+v, want exactly one", forJob)
	}
	if forJob[0].Kind != "openurl_handoff" {
		t.Fatalf("action kind = %q, want openurl_handoff (routed through the existing browser-handoff machinery)", forJob[0].Kind)
	}
	wantURL := delivery.FulfillmentRetrievalURL(dd.PatronWebBaseURL, "482910")
	wantDetail := DocumentDeliveryRetrievalHandoffDetail + "\n" + wantURL
	if forJob[0].Detail != wantDetail {
		t.Fatalf("action detail = %q, want %q", forJob[0].Detail, wantDetail)
	}
	if gotURL, ok := DocumentDeliveryRetrievalHandoffURL(forJob[0].Detail); !ok || gotURL != wantURL {
		t.Fatalf("DocumentDeliveryRetrievalHandoffURL(%q) = %q, %t, want %q, true", forJob[0].Detail, gotURL, ok, wantURL)
	}
	if !strings.Contains(wantURL, "Action=10") || !strings.Contains(wantURL, "Form=75") || !strings.Contains(wantURL, "Value=482910") {
		t.Fatalf("retrieval URL = %q, want the form-75 View PDF query", wantURL)
	}

	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var sawEnqueued bool
	for _, e := range events {
		if e["kind"] != "delivery.retrieval_enqueued" {
			continue
		}
		sawEnqueued = true
		detail, _ := e["detail"].(map[string]any)
		if detail["route_class"] != "document_delivery" || detail["provider_reference"] != "482910" {
			t.Fatalf("delivery.retrieval_enqueued detail = %+v, want route_class document_delivery, provider_reference 482910", detail)
		}
	}
	if !sawEnqueued {
		t.Fatal("no delivery.retrieval_enqueued event was recorded")
	}
}

// TestSubmitDeliveryFulfilledWithoutPatronWebOpensReconciliationAction pins
// the fallback: absent patron_web_base_url means papio cannot construct a
// retrieval route, so a fulfilled row still surfaces an operator-visible
// human action (the pre-existing Decision 4 reconciliation action) instead
// of silently dropping it.
func TestSubmitDeliveryFulfilledWithoutPatronWebOpensReconciliationAction(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	dd := autoCapableDocumentDelivery("https://illiad.example.edu/ILLiadWebPlatform") // no PatronWebBaseURL
	svc.Config.Browser.DocumentDelivery = dd
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_fulfilled_no_patronweb_001", "10.1000/fulfilled-no-patronweb-001"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimNext(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1000/fulfilled-no-patronweb-001",
		State: delivery.StateFulfilled, ProviderReference: "482911",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SubmitDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var forJob []job.HumanAction
	for _, a := range actions {
		if a.JobID == id {
			forJob = append(forJob, a)
		}
	}
	if len(forJob) != 1 || forJob[0].Kind != job.ActionKindDocumentDelivery {
		t.Fatalf("open actions for job = %+v, want exactly one document_delivery reconciliation action", forJob)
	}
	if !strings.Contains(forJob[0].Detail, "needs reconciliation") {
		t.Fatalf("action detail = %q, want the reconciliation wording (never an automated retrieval route)", forJob[0].Detail)
	}
}

// TestSubmitDeliveryFulfilledConservativeRecordsWithoutOpening proves
// ADR-0017 Decision 3B condition 1 applies to fulfilled retrieval exactly
// as it applies to submission and prefill: conservative mode never opens
// or drives anything, even with patron_web_base_url configured — it only
// records the discovery, reusing the same event-not-action pattern
// exhaustedCandidates' conservative branch already uses for route
// discovery.
func TestSubmitDeliveryFulfilledConservativeRecordsWithoutOpening(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.AccessMode = config.ModeConservative
	dd := autoCapableDocumentDelivery("https://illiad.example.edu/ILLiadWebPlatform")
	dd.PatronWebBaseURL = "https://illiadweb.example.edu/illiad/illiad.dll"
	svc.Config.Browser.DocumentDelivery = dd
	svc.Resolvers = deliveryTestResolvers()
	ctx := context.Background()

	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_fulfilled_conservative_001", "10.1000/fulfilled-conservative-001"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimNext(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1000/fulfilled-conservative-001",
		State: delivery.StateFulfilled, ProviderReference: "482912",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SubmitDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateResolving {
		t.Fatalf("job state = %q, want resolving (untouched — conservative mode never transitions on discovery)", got.State)
	}
	actions, err := jobs.ListHumanActions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.JobID == id {
			t.Fatalf("actions = %+v, want none — conservative mode records without opening", a)
		}
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var sawDiscovered bool
	for _, e := range events {
		if e["kind"] != "delivery.retrieval_discovered" {
			continue
		}
		sawDiscovered = true
		detail, _ := e["detail"].(map[string]any)
		if detail["route_class"] != "document_delivery" || detail["provider_reference"] != "482912" {
			t.Fatalf("delivery.retrieval_discovered detail = %+v, want route_class document_delivery, provider_reference 482912", detail)
		}
	}
	if !sawDiscovered {
		t.Fatal("no delivery.retrieval_discovered event was recorded")
	}
}
func TestOfferedDeliveryRecoverySubmitsThroughGatedRoute(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_, _ = w.Write([]byte(`{"TransactionNumber": 7401, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_submit", "10.1000/offered-recovery-submit"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"delivery_request_id": req.ID, "profile_class": "auto_capable",
		"profile_digest": profile.Digest(), "decision": "submit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.submission_failure_classified", map[string]any{
		"delivery_request_id": req.ID,
		"class":               "pre_send",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := deliverySvc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != delivery.StateSubmitted || got.ProviderReference != "7401" {
		t.Fatalf("recovered request = %+v, want submitted with provider reference", got)
	}
	if posts != 1 {
		t.Fatalf("provider posts = %d, want 1", posts)
	}
}

func TestOfferedDeliveryRecoveryStaleProfileSurfacesHumanAction(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		t.Fatal("stale gate profile must never submit")
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_stale", "10.1000/offered-recovery-stale"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: "stale-profile-digest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"delivery_request_id": req.ID, "profile_class": "auto_capable",
		"profile_digest": "stale-profile-digest", "decision": "submit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	gotJob, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if gotJob.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %q, want awaiting_human", gotJob.State)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != job.ActionKindDocumentDelivery {
		t.Fatalf("open actions = %+v, want one document_delivery action", actions)
	}
	got, err := deliverySvc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != delivery.StateOffered || posts != 0 {
		t.Fatalf("request = %+v, posts = %d; stale consent must remain offered without submission", got, posts)
	}
}

type preSendFailureRoundTripper func(*http.Request) (*http.Response, error)

func (f preSendFailureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type toggleFailureHTTPClient struct {
	fail  *bool
	base  *http.Client
	calls *int
}

func (c toggleFailureHTTPClient) Do(req *http.Request) (*http.Response, error) {
	(*c.calls)++
	if *c.fail {
		return nil, http.ErrServerClosed
	}
	return c.base.Do(req)
}
func TestOfferedDeliveryRecoveryCapsTransportFailures(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	svc.RetryDelay = time.Second
	svc.IlliadHTTPClient = &http.Client{Transport: preSendFailureRoundTripper(func(*http.Request) (*http.Response, error) {
		posts++
		return nil, http.ErrServerClosed
	})}
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_cap", "10.1000/offered-recovery-cap"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"delivery_request_id": req.ID, "profile_class": "auto_capable",
		"profile_digest": profile.Digest(), "decision": "submit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.submission_failure_classified", map[string]any{
		"delivery_request_id": req.ID,
		"class":               "pre_send",
	}); err != nil {
		t.Fatal(err)
	}
	for range offeredRecoveryMaxAttempts + 1 {
		if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Minute)
	}
	if posts != offeredRecoveryMaxAttempts {
		t.Fatalf("provider posts = %d, want cap %d", posts, offeredRecoveryMaxAttempts)
	}
	request, err := deliverySvc.GetByJobID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if request.State != delivery.StateOffered || request.ProviderReference != "" {
		t.Fatalf("capped request = %+v, want recoverable offered row", request)
	}
}
func TestOfferedDeliveryRecoverySkipsPrefillDecisionForAutoProfile(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind: "openurl", BaseURL: "https://example.edu/request",
		SubmitPolicy: "prefill_only",
	}
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_prefill", "10.1000/offered-recovery-prefill"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery("https://example.edu/request")
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"delivery_request_id": req.ID, "profile_class": "auto_capable",
		"profile_digest": profile.Digest(), "decision": "prefill",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	gotJob, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if gotJob.State != job.StateQueued {
		t.Fatalf("job state = %q, want queued", gotJob.State)
	}
	gotReq, err := deliverySvc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.State != delivery.StateOffered || gotReq.ProviderReference != "" {
		t.Fatalf("request = %+v, want untouched offered row", gotReq)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "delivery.offered_recovery_attempt" {
			t.Fatal("prefill-only row recorded a recovery attempt")
		}
	}
}
func TestOfferedDeliveryRecoverySkipsPrefillOnlyProfile(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind: "openurl", BaseURL: "https://example.edu/request", SubmitPolicy: "prefill_only",
	}
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_prefill_only", "10.1000/offered-recovery-prefill-only"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "openurl",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"profile_class": "prefill_only", "profile_digest": profile.Digest(), "decision": "prefill",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	assertOfferedRecoveryUntouched(t, jobs, deliverySvc, id, req.ID)
}

func TestOfferedDeliveryRecoverySkipsWithoutGateEvidence(t *testing.T) {
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery("https://example.edu/request")
	ctx := context.Background()
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_no_gate", "10.1000/offered-recovery-no-gate"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.OfferedDeliveryRecovery().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	assertOfferedRecoveryUntouched(t, jobs, deliverySvc, id, req.ID)
}

func assertOfferedRecoveryUntouched(t *testing.T, jobs *job.Store, deliverySvc *delivery.Service, jobID string, requestID int64) {
	t.Helper()
	gotJob, err := jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if gotJob.State != job.StateQueued {
		t.Fatalf("job state = %q, want queued", gotJob.State)
	}
	if gotJob.LeaseOwner != "" || gotJob.LeaseExpiresAt != "" {
		t.Fatalf("job lease = owner %q expiry %q, want no claim", gotJob.LeaseOwner, gotJob.LeaseExpiresAt)
	}
	gotReq, err := deliverySvc.Get(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.State != delivery.StateOffered || gotReq.ProviderReference != "" {
		t.Fatalf("request = %+v, want untouched offered row", gotReq)
	}
	actions, err := jobs.ListOpenHumanActionsForJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "delivery.offered_recovery_attempt" {
			t.Fatal("offered row recorded a recovery attempt")
		}
	}
}

func TestOfferedDeliveryRecoveryConcurrentClaimSubmitsOnce(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte(`{"TransactionNumber": 8801, "TransactionStatus": "Awaiting Request Processing"}`))
	}))
	defer server.Close()
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = server.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(server.URL)
	ctx := context.Background()
	if err := deliverySvc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Submit(ctx, deliveryWorkRequest("wr_offered_recovery_concurrent", "10.1000/offered-recovery-concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	row, _ := jobs.Get(ctx, id)
	profile, err := deliverySvc.ResolveGateProfileFor(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	req, err := deliverySvc.Create(ctx, delivery.CreateRequest{
		JobID: id, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: row.Work.Describe(),
		GateProfileDigest: profile.Digest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.gate_evaluated", map[string]any{
		"delivery_request_id": req.ID, "profile_class": "auto_capable",
		"profile_digest": profile.Digest(), "decision": "submit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, "delivery.submission_failure_classified", map[string]any{
		"delivery_request_id": req.ID, "class": "pre_send",
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- svc.OfferedDeliveryRecovery().RunDue(ctx) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if posts.Load() != 1 {
		t.Fatalf("provider posts = %d, want one", posts.Load())
	}
}
func TestOfferedDeliveryRecoveryUsesUniqueLeaseOwners(t *testing.T) {
	svc := &Service{}
	first := svc.OfferedDeliveryRecovery()
	second := svc.OfferedDeliveryRecovery()
	if first.owner == "" || first.owner == second.owner {
		t.Fatalf("recovery owners = %q and %q, want unique non-empty owners", first.owner, second.owner)
	}
}
