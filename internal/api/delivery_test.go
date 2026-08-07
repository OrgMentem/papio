// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/job"
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
	cfg.DataDir = t.TempDir()
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
