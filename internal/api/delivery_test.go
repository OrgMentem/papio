// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/job"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// deliveryTestSystem builds a system whose default institution profile
// compiles Decision 3A's prefill_only class: openurl is permanently
// prefill-only regardless of every other declaration, so tests never risk
// an outbound illiad HTTP call.
func deliveryTestSystem(t *testing.T) *bootstrap.System {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.DataDir = storetest.DataDir(t)
	// Adoption is a filesystem contract: pin the root to this test's data
	// dir so nothing here ever reaches the real <downloads>/papio default.
	cfg.Browser.AdoptionRoot = filepath.Join(cfg.DataDir, "adoptions")
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind:              "openurl",
		BaseURL:           "https://ill.example.edu/request",
		SubmitPolicy:      "prefill_only",
		RequestClasses:    []string{"digital_journal_article"},
		LegalBasis:        "institution_policy",
		PatronAttestation: "not_required",
		PatronFeePolicy:   "zero_standard",
		MonthlyRequestCap: 25,
	}
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	return system
}

func deliveryTestJob(t *testing.T, system *bootstrap.System, requestID, doi string) string {
	t.Helper()
	result, err := system.Jobs.CreateRequestForWork(context.Background(), requestID,
		work.Work{DOI: doi, Title: "Delivery test article", Authors: []string{"Test, T."}, Year: 2026},
		"", "", job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil,
		job.Attribution{Principal: job.PrincipalCLI}, false)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	if err := system.Jobs.Transition(context.Background(), result.JobID, job.StateQueued, job.StateResolving,
		map[string]any{"reason": "test_setup"}); err != nil {
		t.Fatalf("advance %s to resolving: %v", requestID, err)
	}
	return result.JobID
}

func TestDeliveryGetRejectsMalformedAndUnknownJob(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)

	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": ""}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("empty job_id = %#v, want invalid_argument", rpcErr)
	}
	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": "job_does_not_exist"}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("unknown job = %#v, want not_found", rpcErr)
	}
	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": "job_does_not_exist", "extra": 1}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("unknown field = %#v, want invalid_argument (strict decode)", rpcErr)
	}
}

func TestDeliveryGetNoRequestYetIsNotFound(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_get_none", "10.1234/none")

	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": jobID}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("job with no delivery request = %#v, want not_found", rpcErr)
	}
}

func TestDeliverySubmitPrefillOnlyThenGetExplainsGate(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_submit", "10.1234/submit")

	var submitResult DeliverySubmitResult
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, &submitResult); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	if !submitResult.Configured {
		t.Fatalf("Configured = false, want true (document_delivery is configured)")
	}
	if submitResult.Action != "prefill" {
		t.Fatalf("Action = %q, want prefill (openurl is permanently prefill_only)", submitResult.Action)
	}
	if submitResult.Request == nil || submitResult.Request.State != "offered" {
		t.Fatalf("Request = %#v, want a fresh offered row", submitResult.Request)
	}

	row, err := system.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %s, want awaiting_human", row.State)
	}

	var detail DeliveryDetail
	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": jobID}, &detail); rpcErr != nil {
		t.Fatalf("delivery.get: %v", rpcErr)
	}
	if detail.Request == nil || detail.Request.JobID != jobID || detail.Request.Provider != "openurl" {
		t.Fatalf("Request = %#v", detail.Request)
	}
	if detail.Gate.Class != "prefill_only" {
		t.Fatalf("Gate.Class = %q, want prefill_only", detail.Gate.Class)
	}
	if detail.LastEvaluation == nil || detail.LastEvaluation.Decision != "prefill" {
		t.Fatalf("LastEvaluation = %#v, want decision prefill", detail.LastEvaluation)
	}
}

func TestDeliverySubmitUnconfiguredProfileReportsNotConfigured(t *testing.T) {
	system := deliveryTestSystem(t)
	system.Config.Browser.DocumentDelivery = nil
	system.App.Config.Browser.DocumentDelivery = nil
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_unconfigured", "10.1234/unconfigured")

	var result DeliverySubmitResult
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, &result); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	if result.Configured {
		t.Fatalf("Configured = true, want false — no document_delivery block on this profile")
	}
	if result.Request != nil {
		t.Fatalf("Request = %#v, want nil", result.Request)
	}

	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": jobID}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("delivery.get on an unconfigured, never-routed job = %#v, want not_found", rpcErr)
	}
}

func TestDeliveryCancelOfferedRowSucceedsLocally(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_cancel", "10.1234/cancel")

	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}

	var result DeliveryCancelResult
	if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": jobID}, &result); rpcErr != nil {
		t.Fatalf("delivery.cancel: %v", rpcErr)
	}
	if !result.Supported || !result.Cancelled {
		t.Fatalf("cancel result = %#v, want a locally cancellable offered row", result)
	}
	if result.State != "cancelled" {
		t.Fatalf("State = %q, want cancelled", result.State)
	}

	// Idempotent: cancelling an already-cancelled row is a routine no-op,
	// never an IPC error.
	var again DeliveryCancelResult
	if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": jobID}, &again); rpcErr != nil {
		t.Fatalf("second delivery.cancel: %v", rpcErr)
	}
	if !again.Cancelled || again.Reason == "" {
		t.Fatalf("second cancel = %#v, want an idempotent cancelled result with a reason", again)
	}
}

func TestDeliveryCancelLiveRequestReportsNotSupportedNotAnError(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_cancel_live", "10.1234/cancel-live")

	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	row, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
	if err != nil || row == nil {
		t.Fatalf("GetByJobID: row=%#v err=%v", row, err)
	}
	if err := system.App.Delivery.UpdateState(context.Background(), row.ID, delivery.StateSubmitted); err != nil {
		t.Fatal(err)
	}

	var result DeliveryCancelResult
	if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": jobID}, &result); rpcErr != nil {
		t.Fatalf("delivery.cancel returned an IPC error for a routine condition: %v", rpcErr)
	}
	if result.Supported || result.Cancelled || result.Reason == "" {
		t.Fatalf("cancel of a live-submitted row = %#v, want supported=false with a reason", result)
	}
}

func TestDeliveryActionConfirmRequestExistsMovesJobToRetryWait(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_confirm_exists", "10.1234/confirm-exists")

	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}

	if rpcErr := callMethod(t, router, "delivery.action",
		map[string]any{"job_id": jobID, "operation": "confirm_request_exists"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("confirm_request_exists without provider_reference = %#v, want invalid_argument", rpcErr)
	}

	var result DeliveryActionResult
	params := map[string]any{"job_id": jobID, "operation": "confirm_request_exists", "provider_reference": "TN-42"}
	if rpcErr := callMethod(t, router, "delivery.action", params, &result); rpcErr != nil {
		t.Fatalf("confirm_request_exists: %v", rpcErr)
	}
	if result.JobState != job.StateRetryWait {
		t.Fatalf("JobState = %q, want retry_wait", result.JobState)
	}
	if result.Detail == nil || result.Detail.Request == nil || result.Detail.Request.State != "pending" {
		t.Fatalf("Detail.Request = %#v, want state pending", result.Detail)
	}
	if result.Detail.Request.ProviderReference != "TN-42" {
		t.Fatalf("ProviderReference = %q, want TN-42", result.Detail.Request.ProviderReference)
	}

	row, err := system.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateRetryWait || row.RetryAt == "" {
		t.Fatalf("job row = %#v, want retry_wait with a scheduled retry_at", row)
	}

	actions, err := system.Jobs.ListHumanActionsForJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Action.Status == "open" {
			t.Fatalf("action %+v is still open after confirm_request_exists", a.Action)
		}
	}
}

func TestDeliveryActionConfirmRequestAbsentCancelsAndReRunsGate(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_confirm_absent", "10.1234/confirm-absent")

	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	before, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
	if err != nil || before == nil {
		t.Fatalf("GetByJobID before: row=%#v err=%v", before, err)
	}

	var result DeliveryActionResult
	params := map[string]any{"job_id": jobID, "operation": "confirm_request_absent"}
	if rpcErr := callMethod(t, router, "delivery.action", params, &result); rpcErr != nil {
		t.Fatalf("confirm_request_absent: %v", rpcErr)
	}
	// The stale row is cancelled and the gate re-evaluated through the
	// shared app seam; v1's resubmission policy re-opens the reconciliation
	// action (never retry_submission) rather than silently resubmitting.
	if row, err := system.Jobs.Get(context.Background(), jobID); err != nil {
		t.Fatal(err)
	} else if row.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %s, want awaiting_human (v1 escalates a fresh resubmission-policy decision to reconciliation)", row.State)
	}

	after, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
	if err != nil || after == nil {
		t.Fatalf("GetByJobID after: row=%#v err=%v", after, err)
	}
	if after.ID != before.ID {
		t.Fatalf("row identity changed from %d to %d — v1's resubmission policy reuses the existing row, it never opens a second one", before.ID, after.ID)
	}
	if after.State != delivery.StateCancelled {
		t.Fatalf("row state = %s, want cancelled", after.State)
	}

	actions, err := system.Jobs.ListHumanActionsForJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	openCount, resolvedCount := 0, 0
	for _, a := range actions {
		switch a.Action.Status {
		case "open":
			openCount++
		case "resolved":
			resolvedCount++
		}
	}
	if openCount != 1 || resolvedCount != 1 {
		t.Fatalf("actions = %d open, %d resolved, want exactly one of each (old closed, new opened)", openCount, resolvedCount)
	}
}

func TestDeliveryActionOpenRequestHistoryMatchesGet(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_delivery_history", "10.1234/history")

	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}

	var history DeliveryActionResult
	if rpcErr := callMethod(t, router, "delivery.action",
		map[string]any{"job_id": jobID, "operation": "open_request_history"}, &history); rpcErr != nil {
		t.Fatalf("open_request_history: %v", rpcErr)
	}
	if history.Detail == nil || history.Detail.Request == nil {
		t.Fatalf("history.Detail = %#v", history.Detail)
	}

	var get DeliveryDetail
	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": jobID}, &get); rpcErr != nil {
		t.Fatalf("delivery.get: %v", rpcErr)
	}
	if history.Detail.Request.ID != get.Request.ID || history.Detail.Gate.Class != get.Gate.Class {
		t.Fatalf("open_request_history detail %#v does not match delivery.get %#v", history.Detail, get)
	}
}

func TestDeliveryActionRejectsUnknownOperation(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	if rpcErr := callMethod(t, router, "delivery.action",
		map[string]any{"job_id": "job_01", "operation": "retry_submission"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("retry_submission operation = %#v, want invalid_argument (Decision 4 forbids it)", rpcErr)
	}
}

func TestDeliveryServiceUnconfiguredReturnsPreconditionFailed(t *testing.T) {
	system := deliveryTestSystem(t)
	system.App.Delivery = nil
	router := Router(system)
	if rpcErr := callMethod(t, router, "delivery.get", map[string]any{"job_id": "job_01"}, nil); rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("delivery.get with no delivery service = %#v, want precondition_failed", rpcErr)
	}
	if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": "job_01"}, nil); rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("delivery.cancel with no delivery service = %#v, want precondition_failed", rpcErr)
	}
}

func TestDeliveryConfirmRequestExistsAtomicityPerInjectionPoint(t *testing.T) {
	cases := []struct {
		name   string
		inject func() error
	}{
		{"update_state", func() error { return errors.New("injected update_state") }},
		{"record_poll", func() error { return errors.New("injected record_poll") }},
		{"repair", func() error { return errors.New("injected repair") }},
		{"transition", func() error { return errors.New("injected transition") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system := deliveryTestSystem(t)
			router := Router(system)
			jobID := deliveryTestJob(t, system, "req_exists_atomic_"+tc.name, "10.1234/exists-atomic-"+tc.name)
			if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
				t.Fatalf("delivery.submit: %v", rpcErr)
			}
			beforeRow, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
			if err != nil || beforeRow == nil {
				t.Fatalf("before GetByJobID: %v %v", beforeRow, err)
			}
			beforeJob, err := system.Jobs.Get(context.Background(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				delivery.SetBeforeUpdateStateTxForTest(nil)
				delivery.SetBeforeRecordPollTxForTest(nil)
				job.SetBeforeRepairTxForTest(nil)
				job.SetBeforeTransitionTxForTest(nil)
			})
			switch tc.name {
			case "update_state":
				delivery.SetBeforeUpdateStateTxForTest(tc.inject)
			case "record_poll":
				delivery.SetBeforeRecordPollTxForTest(tc.inject)
			case "repair":
				job.SetBeforeRepairTxForTest(tc.inject)
			case "transition":
				job.SetBeforeTransitionTxForTest(tc.inject)
			}
			params := map[string]any{"job_id": jobID, "operation": "confirm_request_exists", "provider_reference": "TN-ATOMIC"}
			var result DeliveryActionResult
			rpcErr := callMethod(t, router, "delivery.action", params, &result)
			if rpcErr == nil {
				t.Fatalf("expected failure for injection %s, got success", tc.name)
			}
			afterRow, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			if afterRow.State != beforeRow.State || afterRow.ProviderReference != beforeRow.ProviderReference {
				t.Fatalf("row mutated despite injected failure %s: before=%+v after=%+v", tc.name, beforeRow, afterRow)
			}
			afterJob, err := system.Jobs.Get(context.Background(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			if afterJob.State != beforeJob.State {
				t.Fatalf("job state mutated: before %s after %s (injection %s)", beforeJob.State, afterJob.State, tc.name)
			}
			actions, err := system.Jobs.ListHumanActionsForJob(context.Background(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			hasOpen := false
			for _, a := range actions {
				if a.Action.Kind == job.ActionKindDocumentDelivery && a.Action.Status == "open" {
					hasOpen = true
				}
			}
			if !hasOpen {
				t.Fatalf("no open document_delivery action after injected failure %s — operator lost affordance", tc.name)
			}
		})
	}
}

func TestDeliveryConfirmRequestAbsentRepairsBeforeSubmitAndReopensAction(t *testing.T) {
	// The absent path is Cancel row -> RepairAwaitingHuman -> SubmitDelivery,
	// and that order is the whole point: RepairAwaitingHuman is the legal
	// awaiting_human->resolving edge, so closing the action BEFORE the gate is
	// what lets SubmitDelivery park the job again. The earlier
	// Cancel -> Submit -> close order failed every call with an illegal
	// awaiting_human->awaiting_human park.
	//
	// What actually catches a return to that order is the RPC below
	// succeeding at all: the old order made every call fail. Note what does
	// NOT catch it — the end state is awaiting_human under both orders, since
	// the fixture starts parked there and a failed submit leaves it parked.
	// Assert the end state because it is the contract, not because it
	// discriminates.
	system := deliveryTestSystem(t)
	router := Router(system)
	jobID := deliveryTestJob(t, system, "req_absent_atomic", "10.1234/absent-atomic")
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	var result DeliveryActionResult
	if rpcErr := callMethod(t, router, "delivery.action", map[string]any{"job_id": jobID, "operation": "confirm_request_absent"}, &result); rpcErr != nil {
		t.Fatalf("confirm_request_absent: %v", rpcErr)
	}
	afterRow, err := system.App.Delivery.GetByJobID(context.Background(), jobID)
	if err != nil || afterRow == nil {
		t.Fatalf("after GetByJobID: %v %v", afterRow, err)
	}
	if afterRow.State != delivery.StateCancelled {
		t.Fatalf("after row state = %s, want cancelled", afterRow.State)
	}
	// Contract, not discriminator (see above): the job is parked again rather
	// than left mid-gate.
	jobRow, err := system.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if jobRow.State != job.StateAwaitingHuman {
		t.Fatalf("job state after absent = %s, want awaiting_human (the reconciliation park after a legal repair-then-submit)", jobRow.State)
	}
	// The real discriminator, and the one the state check cannot be: the
	// repair transition must have happened. Under cancel-submit-repair the
	// submit fails first and RepairAwaitingHuman never runs, so this event
	// does not exist at all.
	events, err := system.Jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	repairSeen, parkedAfterRepair := false, false
	for _, e := range events {
		if e["kind"] != "job.transition" {
			continue
		}
		detail, ok := e["detail"].(map[string]any)
		if !ok {
			continue
		}
		if detail["reason"] == "document_delivery_confirmed_absent" {
			repairSeen = true
			continue
		}
		if repairSeen && detail["to"] == job.StateAwaitingHuman {
			parkedAfterRepair = true
		}
	}
	if !repairSeen {
		t.Fatalf("no job.transition carrying the confirmed-absent repair; events = %v", events)
	}
	if !parkedAfterRepair {
		t.Fatalf("no park back to awaiting_human after the repair transition; events = %v", events)
	}
	actions, err := system.Jobs.ListHumanActionsForJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	open, resolved := 0, 0
	for _, a := range actions {
		if a.Action.Status == "open" {
			open++
		}
		if a.Action.Status == "resolved" {
			resolved++
		}
	}
	if open != 1 || resolved != 1 {
		t.Fatalf("actions after absent = %d open %d resolved, want 1/1 (old action repaired, fresh reconciliation action opened)", open, resolved)
	}
}

// countOpenActions counts the human actions still awaiting an operator. A
// refusal must never change this number: the open action IS the affordance.
func countOpenActions(actions []job.AttributedAction) int {
	open := 0
	for _, a := range actions {
		if a.Action.Status == "open" {
			open++
		}
	}
	return open
}

// delivery.resume's whole wire contract. A live row is resumed and its
// poll-failure bookkeeping actually cleared; every not-live row comes back
// as the structured refusal DeliveryResumeResult documents, never an IPC
// error; an unknown id is not_found; a missing or non-positive request_id
// is invalid_argument.
func TestDeliveryResumeWireContract(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	ctx := context.Background()

	for _, params := range []map[string]any{{}, {"request_id": 0}, {"request_id": -1}} {
		if rpcErr := callMethod(t, router, "delivery.resume", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("delivery.resume %v = %#v, want invalid_argument (request_id is required and positive)", params, rpcErr)
		}
	}

	// The live arm: a submitted row parked far in the future by a
	// contract-drift poll failure, which is exactly the state this method
	// exists to recover from.
	liveJob := deliveryTestJob(t, system, "req_delivery_resume_live", "10.1234/resume-live")
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": liveJob}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}
	live, err := system.App.Delivery.GetByJobID(ctx, liveJob)
	if err != nil || live == nil {
		t.Fatalf("GetByJobID: row=%#v err=%v", live, err)
	}
	if err := system.App.Delivery.UpdateState(ctx, live.ID, delivery.StateSubmitted); err != nil {
		t.Fatal(err)
	}
	farFuture := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := system.Store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET consecutive_poll_failures = 9, last_poll_error_class = 'contract_drift', next_check_at = ?
		WHERE id = ?`, farFuture, live.ID); err != nil {
		t.Fatal(err)
	}

	var resumed DeliveryResumeResult
	if rpcErr := callMethod(t, router, "delivery.resume", map[string]any{"request_id": live.ID}, &resumed); rpcErr != nil {
		t.Fatalf("delivery.resume on a live submitted row: %v", rpcErr)
	}
	if !resumed.Resumed || resumed.RequestID != live.ID || resumed.State != string(delivery.StateSubmitted) || resumed.Reason != "" {
		t.Fatalf("resume of a live submitted row = %#v, want resumed=true, state=submitted, no reason", resumed)
	}
	after, err := system.App.Delivery.Get(ctx, live.ID)
	if err != nil || after == nil {
		t.Fatalf("Get after resume: row=%#v err=%v", after, err)
	}
	if after.ConsecutivePollFailures != 0 || after.LastPollErrorClass != "" {
		t.Fatalf("after resume: consecutive_poll_failures=%d last_poll_error_class=%q, want 0 and cleared",
			after.ConsecutivePollFailures, after.LastPollErrorClass)
	}
	// Due, not merely "different". A mutant that rescheduled the row to
	// another future instant would satisfy an inequality while polling stayed
	// disabled, which is the exact stall this method exists to clear.
	dueBy, err := time.Parse(time.RFC3339Nano, after.NextCheckAt)
	if err != nil {
		t.Fatalf("next_check_at %q is unparseable: %v", after.NextCheckAt, err)
	}
	if dueBy.After(time.Now().UTC()) {
		t.Fatalf("next_check_at = %s, want a due schedule: a still-future check means the poll loop remains parked and nothing was resumed", after.NextCheckAt)
	}
	if after.State != delivery.StateSubmitted {
		t.Fatalf("resume changed the row state to %s; it only clears poll bookkeeping", after.State)
	}

	// StateSubmitted and StatePending are the only live states
	// (delivery.ErrRequestNotLive names both), so every other state must come
	// back as the structured refusal — including unknown_outcome, whose
	// refusal carries the confirm-absent reconciliation hint, and offered,
	// which was never submitted at all.
	for _, state := range []delivery.State{
		delivery.StateFulfilled,
		delivery.StateCancelled,
		delivery.StateDeclined,
		delivery.StateUnknownOutcome,
		delivery.StateOffered,
	} {
		jobID := deliveryTestJob(t, system, "req_delivery_resume_"+string(state), "10.1234/resume-"+string(state))
		if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
			t.Fatalf("delivery.submit for %s: %v", state, rpcErr)
		}
		row, err := system.App.Delivery.GetByJobID(ctx, jobID)
		if err != nil || row == nil {
			t.Fatalf("GetByJobID %s: row=%#v err=%v", state, row, err)
		}
		if err := system.App.Delivery.UpdateState(ctx, row.ID, state); err != nil {
			t.Fatal(err)
		}
		var refused DeliveryResumeResult
		if rpcErr := callMethod(t, router, "delivery.resume", map[string]any{"request_id": row.ID}, &refused); rpcErr != nil {
			t.Fatalf("delivery.resume on a %s row = IPC error %v, want a structured refusal (DeliveryResumeResult: Resumed false with Reason, never a bare error)", state, rpcErr)
		}
		if refused.Resumed {
			t.Fatalf("resume of a %s row = %#v, want resumed=false — a not-live row has no poll schedule to resume", state, refused)
		}
		if refused.RequestID != row.ID || refused.State != string(state) {
			t.Fatalf("resume of a %s row = %#v, want it to report the row's own id and state", state, refused)
		}
		if !strings.Contains(refused.Reason, "not live") {
			t.Fatalf("resume of a %s row Reason = %q, want it to name the not-live state", state, refused.Reason)
		}
	}

	// StatePending is the second live state, and it is the one a provider
	// moves a request into after acknowledging it. Resuming it must succeed
	// exactly like submitted, or half the live contract is unprotected.
	pendingJob := deliveryTestJob(t, system, "req_delivery_resume_pending", "10.1234/resume-pending")
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": pendingJob}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit for the pending arm: %v", rpcErr)
	}
	pendingRow, err := system.App.Delivery.GetByJobID(ctx, pendingJob)
	if err != nil || pendingRow == nil {
		t.Fatalf("GetByJobID pending: row=%#v err=%v", pendingRow, err)
	}
	if err := system.App.Delivery.UpdateState(ctx, pendingRow.ID, delivery.StatePending); err != nil {
		t.Fatal(err)
	}
	var resumedPending DeliveryResumeResult
	if rpcErr := callMethod(t, router, "delivery.resume", map[string]any{"request_id": pendingRow.ID}, &resumedPending); rpcErr != nil {
		t.Fatalf("delivery.resume on a live pending row: %v", rpcErr)
	}
	if !resumedPending.Resumed || resumedPending.State != string(delivery.StatePending) || resumedPending.Reason != "" {
		t.Fatalf("resume of a live pending row = %#v, want resumed=true, state=pending, no reason", resumedPending)
	}

	if rpcErr := callMethod(t, router, "delivery.resume", map[string]any{"request_id": live.ID + 1_000_000}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("delivery.resume on an unknown request_id = %#v, want not_found", rpcErr)
	}
}

// ADR-0017: a request already live at the provider — fulfilled, or stuck in
// unknown_outcome reconciliation — can only be reported not cancellable,
// never surfaced as an IPC error, and never closed locally behind the
// operator's back.
func TestDeliveryCancelRefusesFulfilledAndUnknownOutcomeRows(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	ctx := context.Background()

	cases := []struct {
		state      delivery.State
		reasonHint string
	}{
		{delivery.StateFulfilled, "fulfilled"},
		{delivery.StateUnknownOutcome, "confirm-absent"},
	}
	for _, tc := range cases {
		jobID := deliveryTestJob(t, system, "req_delivery_cancel_"+string(tc.state), "10.1234/cancel-"+string(tc.state))
		if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
			t.Fatalf("delivery.submit for %s: %v", tc.state, rpcErr)
		}
		row, err := system.App.Delivery.GetByJobID(ctx, jobID)
		if err != nil || row == nil {
			t.Fatalf("GetByJobID %s: row=%#v err=%v", tc.state, row, err)
		}
		if err := system.App.Delivery.UpdateState(ctx, row.ID, tc.state); err != nil {
			t.Fatal(err)
		}
		beforeJob, err := system.Jobs.Get(ctx, jobID)
		if err != nil || beforeJob == nil {
			t.Fatalf("Get job before refused cancel: row=%#v err=%v", beforeJob, err)
		}
		beforeActions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		openBefore := countOpenActions(beforeActions)
		if openBefore == 0 {
			t.Fatalf("fixture no longer discriminates: the %s job has no open human action, so a refusal that closed one could not be detected", tc.state)
		}

		var result DeliveryCancelResult
		if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": jobID}, &result); rpcErr != nil {
			t.Fatalf("delivery.cancel on a %s row = IPC error %v; ADR-0017 requires a not-cancellable report for this routine condition", tc.state, rpcErr)
		}
		if result.Supported || result.Cancelled {
			t.Fatalf("cancel of a %s row = %#v, want supported=false cancelled=false (v1 ships no provider remote-cancel capability)", tc.state, result)
		}
		if result.JobID != jobID || result.State != string(tc.state) {
			t.Fatalf("cancel of a %s row = %#v, want it to report this job and the row's own state", tc.state, result)
		}
		if !strings.Contains(result.Reason, tc.reasonHint) {
			t.Fatalf("cancel of a %s row Reason = %q, want it to name why (%q)", tc.state, result.Reason, tc.reasonHint)
		}

		// The row is not the only thing a bad refusal could close. A path that
		// cancelled the JOB and resolved its open document_delivery action
		// while leaving the provider row untouched satisfies the row check
		// alone, and that local close is exactly what ADR-0017 forbids.
		afterJob, err := system.Jobs.Get(ctx, jobID)
		if err != nil || afterJob == nil {
			t.Fatalf("Get job after refused cancel: row=%#v err=%v", afterJob, err)
		}
		if afterJob.State != beforeJob.State {
			t.Fatalf("refused cancel of a %s row moved the job from %s to %s — the refusal must leave local work alone", tc.state, beforeJob.State, afterJob.State)
		}
		afterActions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if got := countOpenActions(afterActions); got != openBefore {
			t.Fatalf("refused cancel of a %s row changed the open action count from %d to %d — the operator's affordance must survive a refusal", tc.state, openBefore, got)
		}

		unchanged, err := system.App.Delivery.Get(ctx, row.ID)
		if err != nil || unchanged == nil {
			t.Fatalf("Get after refused cancel: row=%#v err=%v", unchanged, err)
		}
		if unchanged.State != tc.state {
			t.Fatalf("refused cancel moved the %s row to %s — a refusal must never close a live request locally", tc.state, unchanged.State)
		}
	}

	// A job that never reached the delivery routing boundary has no row to
	// cancel, and that is a per-job not_found rather than a cancel result.
	bare := deliveryTestJob(t, system, "req_delivery_cancel_no_row", "10.1234/cancel-no-row")
	if rpcErr := callMethod(t, router, "delivery.cancel", map[string]any{"job_id": bare}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("delivery.cancel on a job with no delivery request = %#v, want not_found", rpcErr)
	}
}

// deliveryConfirmRequestAbsent's compensation branch. RepairAwaitingHuman
// closes the reconciliation action and commits BEFORE SubmitDelivery runs,
// so a SubmitDelivery failure has already consumed the operator's only
// affordance: the handler must park the job back at awaiting_human with
// reason document_delivery_reconciliation, re-open a fresh document_delivery
// action, and still return the submit error.
//
// The failure is injected without touching production: a test-local SQLite
// trigger refuses to INSERT a document_delivery prompt while the job is
// still in resolving. Post-repair the job IS resolving, so SubmitDelivery's
// own reconciliation action cannot open and SubmitDelivery fails. The
// compensation parks the job to awaiting_human BEFORE opening its prompt
// (delivery.go's documented order, mirroring internal/browser's bridge), so
// its own insert passes the same trigger. Reordering the compensation to
// prompt-then-park, or deleting it, therefore loses the affordance and
// fails this test.
func TestDeliveryConfirmRequestAbsentCompensatesWhenSubmitFailsAfterRepair(t *testing.T) {
	system := deliveryTestSystem(t)
	router := Router(system)
	ctx := context.Background()
	jobID := deliveryTestJob(t, system, "req_absent_compensate", "10.1234/absent-compensate")
	if rpcErr := callMethod(t, router, "delivery.submit", map[string]any{"job_id": jobID}, nil); rpcErr != nil {
		t.Fatalf("delivery.submit: %v", rpcErr)
	}

	if _, err := system.Store.DB().ExecContext(ctx, `
		CREATE TRIGGER injected_refuse_prompt_while_resolving
		BEFORE INSERT ON human_actions
		WHEN NEW.kind = 'document_delivery'
		 AND (SELECT state FROM jobs WHERE id = NEW.job_id) = 'resolving'
		BEGIN
			SELECT RAISE(ABORT, 'injected: no prompt while the job is resolving');
		END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = system.Store.DB().Exec(`DROP TRIGGER IF EXISTS injected_refuse_prompt_while_resolving`)
	})

	rpcErr := callMethod(t, router, "delivery.action", map[string]any{"job_id": jobID, "operation": "confirm_request_absent"}, nil)
	if rpcErr == nil {
		t.Fatal("confirm_request_absent returned a success result although SubmitDelivery failed; the submit error must reach the operator")
	}

	// The repair committed, so the row is cancelled: a documented
	// recoverable state, which is exactly why the affordance must come back.
	row, err := system.App.Delivery.GetByJobID(ctx, jobID)
	if err != nil || row == nil {
		t.Fatalf("GetByJobID: row=%#v err=%v", row, err)
	}
	if row.State != delivery.StateCancelled {
		t.Fatalf("row state = %s, want cancelled (the repair transaction committed before SubmitDelivery ran)", row.State)
	}

	jobRow, err := system.Jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if jobRow.State != job.StateAwaitingHuman {
		t.Fatalf("job state = %s, want awaiting_human: the repair moved it to resolving and a failed submit must park it back", jobRow.State)
	}

	events, err := system.Jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	repairSeen, compensated := false, false
	for _, e := range events {
		if e["kind"] != "job.transition" {
			continue
		}
		detail, ok := e["detail"].(map[string]any)
		if !ok {
			continue
		}
		if detail["reason"] == "document_delivery_confirmed_absent" {
			repairSeen = true
			continue
		}
		if repairSeen && detail["to"] == job.StateAwaitingHuman && detail["reason"] == "document_delivery_reconciliation" {
			compensated = true
		}
	}
	if !repairSeen {
		t.Fatalf("no confirmed-absent repair transition; the failure was injected before the repair, not after it: events = %v", events)
	}
	if !compensated {
		t.Fatalf("no post-repair park to awaiting_human with reason document_delivery_reconciliation; events = %v", events)
	}

	actions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	open, resolved := 0, 0
	for _, a := range actions {
		if a.Action.Kind != job.ActionKindDocumentDelivery {
			continue
		}
		switch a.Action.Status {
		case "open":
			open++
		case "resolved":
			resolved++
		}
	}
	if open != 1 || resolved != 1 {
		t.Fatalf("document_delivery actions = %d open, %d resolved, want 1/1 (the repaired one closed, a fresh reconciliation action re-opened)", open, resolved)
	}
}
