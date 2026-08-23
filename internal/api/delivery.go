// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/delivery"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/store"
)

// DeliveryRequest is the wire projection of one delivery_requests row
// (ADR-0017 Decision 1). It is a distinct, explicitly tagged type rather
// than delivery.Request itself: that struct carries no JSON tags of its
// own (internal/delivery never serializes it), and internal/api owns the
// wire contract for every ratified result the same way JobRow owns job.Row's.
type DeliveryRequest struct {
	ID                 int64  `json:"id"`
	JobID              string `json:"job_id"`
	InstitutionProfile string `json:"institution_profile"`
	Provider           string `json:"provider"`
	RequestClass       string `json:"request_class"`
	WorkIdentity       string `json:"work_identity"`
	IdempotencyKey     string `json:"idempotency_key"`
	State              string `json:"state"`
	ProviderReference  string `json:"provider_reference,omitempty"`
	GateProfileDigest  string `json:"gate_profile_digest,omitempty"`
	SubmittedAt        string `json:"submitted_at,omitempty"`
	LastCheckedAt      string `json:"last_checked_at,omitempty"`
	NextCheckAt        string `json:"next_check_at,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

func deliveryRequestRow(r *delivery.Request) *DeliveryRequest {
	if r == nil {
		return nil
	}
	return &DeliveryRequest{
		ID: r.ID, JobID: r.JobID, InstitutionProfile: r.InstitutionProfile,
		Provider: r.Provider, RequestClass: r.RequestClass, WorkIdentity: r.WorkIdentity,
		IdempotencyKey: r.IdempotencyKey, State: string(r.State), ProviderReference: r.ProviderReference,
		GateProfileDigest: r.GateProfileDigest, SubmittedAt: r.SubmittedAt, LastCheckedAt: r.LastCheckedAt,
		NextCheckAt: r.NextCheckAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// DeliveryBlocker is one closed-vocabulary Decision 3A blocker with its
// recorded evidence, exactly what Decision 3C requires papio to be able to
// explain: "PREFILL ONLY with the specific blocker".
type DeliveryBlocker struct {
	Code     string `json:"code"`
	Evidence string `json:"evidence,omitempty"`
}

// DeliveryGateSummary is the compiled Decision 3A/3B verdict for one
// institution profile: auto_capable, prefill_only, or invalid, plus every
// blocker that kept it from compiling higher.
type DeliveryGateSummary struct {
	Class    string            `json:"class"`
	Blockers []DeliveryBlocker `json:"blockers,omitempty"`
	// FulfillmentChannel is "" or "patron_web" (2026-08-07 ADR-0017
	// amendment): the compiled end-to-end retrieval capability, distinct
	// from Class — a profile can be auto_capable for submission with no
	// fulfillment channel.
	FulfillmentChannel string `json:"fulfillment_channel,omitempty"`
}

func deliveryGateSummaryFrom(profile delivery.GateProfile) DeliveryGateSummary {
	blockers := make([]DeliveryBlocker, 0, len(profile.Blockers))
	for _, b := range profile.Blockers {
		blockers = append(blockers, DeliveryBlocker{Code: b.Code, Evidence: b.Evidence})
	}
	return DeliveryGateSummary{Class: string(profile.Class), Blockers: blockers, FulfillmentChannel: profile.FulfillmentChannel}
}

// DeliveryGateEvent is the redaction-safe delivery.gate_evaluated verdict
// papio actually recorded for a job (Decision 3B), read back rather than
// recomputed so an explanation never disagrees with a since-edited profile.
type DeliveryGateEvent struct {
	ProfileClass       string   `json:"profile_class"`
	ProfileDigest      string   `json:"profile_digest,omitempty"`
	Decision           string   `json:"decision"`
	Blockers           []string `json:"blockers,omitempty"`
	FulfillmentChannel string   `json:"fulfillment_channel,omitempty"`
}

func deliveryGateEventFrom(evt *delivery.GateEvaluated) *DeliveryGateEvent {
	if evt == nil {
		return nil
	}
	return &DeliveryGateEvent{
		ProfileClass: string(evt.ProfileClass), ProfileDigest: evt.ProfileDigest,
		Decision: string(evt.Decision.Action), Blockers: evt.Decision.Blockers,
		FulfillmentChannel: evt.FulfillmentChannel,
	}
}

// DeliveryDetail is delivery.get's result and also backs delivery.action's
// open_request_history operation (Decision 4): the durable row, today's
// compiled gate summary for the row's institution profile, and the most
// recent gate evaluation that explains the row's current state.
type DeliveryDetail struct {
	Request        *DeliveryRequest    `json:"request"`
	Gate           DeliveryGateSummary `json:"gate"`
	LastEvaluation *DeliveryGateEvent  `json:"last_evaluation,omitempty"`
}

// deliveryService returns the configured delivery service, or a
// precondition_failed RPCError when document delivery has no wiring at all
// (app.Service.Delivery is nil) rather than a panic or a bare "not found"
// that would look like a per-job condition.
func deliveryService(system *bootstrap.System) (*delivery.Service, *ipc.RPCError) {
	if system == nil || system.App == nil || system.App.Delivery == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "document delivery is not configured"}
	}
	return system.App.Delivery, nil
}

func failureErr(err error) *ipc.RPCError {
	_, rpcErr := failure(err)
	return rpcErr
}

// deliveryRequestDetail loads the full explainable picture for one job's
// delivery request: the job must exist, a delivery_requests row must exist
// for it (a job that never reached the delivery routing boundary has
// nothing to report), and the gate summary is resolved fresh against the
// row's own institution profile.
func deliveryRequestDetail(ctx context.Context, system *bootstrap.System, jobID string) (*DeliveryDetail, *ipc.RPCError) {
	svc, rpcErr := deliveryService(system)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if _, err := system.Jobs.Get(ctx, jobID); err != nil {
		return nil, failureErr(err)
	}
	row, err := svc.GetByJobID(ctx, jobID)
	if err != nil {
		return nil, failureErr(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "no delivery request for this job"}
	}
	profile, err := svc.ResolveGateProfileFor(ctx, row.InstitutionProfile)
	if err != nil {
		return nil, failureErr(err)
	}
	evt, err := svc.LatestGateEvent(ctx, jobID)
	if err != nil {
		return nil, failureErr(err)
	}
	return &DeliveryDetail{
		Request:        deliveryRequestRow(row),
		Gate:           deliveryGateSummaryFrom(profile),
		LastEvaluation: deliveryGateEventFrom(evt),
	}, nil
}

func deliveryGet(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	detail, rpcErr := deliveryRequestDetail(ctx, system, jobID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return marshal(detail)
}

// DeliverySubmitResult is delivery.submit's result: the Branch/gate outcome
// app.Service.SubmitDelivery computed through the exact seam
// exhaustedCandidates uses automatically, plus the row it left behind.
// Configured is false — Action, Blockers, Branch empty, Request nil — when
// the job's institution profile has no document_delivery configured at all;
// that is not a failure, just nothing for this route to do.
type DeliverySubmitResult struct {
	Configured bool             `json:"configured"`
	Branch     string           `json:"branch,omitempty"`
	Action     string           `json:"action,omitempty"`
	Blockers   []string         `json:"blockers,omitempty"`
	Request    *DeliveryRequest `json:"request,omitempty"`
}

func deliverySubmit(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if system == nil || system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "document delivery is not configured"}
	}
	outcome, err := system.App.SubmitDelivery(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	return marshal(DeliverySubmitResult{
		Configured: outcome.Configured,
		Branch:     string(outcome.Branch),
		Action:     string(outcome.Decision.Action),
		Blockers:   outcome.Decision.Blockers,
		Request:    deliveryRequestRow(outcome.Request),
	})
}

// DeliveryCancelResult reports whether delivery.cancel actually cancelled a
// request. V1 ships no provider adapter with a remote cancel capability —
// illiad, the only source-controlled API integration Decision 3A allows to
// compile auto_capable, documents create/lookup/list but never cancel — so
// a request already live at the provider (submitted, pending, or stuck in
// unknown_outcome reconciliation) can only be reported not cancellable
// here, never as an IPC error for this routine condition. A request that
// never reached the provider, or is already at rest, is safe to close
// locally.
type DeliveryCancelResult struct {
	JobID     string `json:"job_id"`
	Supported bool   `json:"supported"`
	Cancelled bool   `json:"cancelled"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

func deliveryCancel(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	svc, rpcErr := deliveryService(system)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if _, err := system.Jobs.Get(ctx, jobID); err != nil {
		return failure(err)
	}
	row, err := svc.GetByJobID(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "no delivery request for this job"}
	}
	switch row.State {
	case delivery.StateOffered, delivery.StateDeclined:
		if err := svc.UpdateState(ctx, row.ID, delivery.StateCancelled); err != nil {
			return failure(err)
		}
		return marshal(DeliveryCancelResult{JobID: jobID, Supported: true, Cancelled: true, State: string(delivery.StateCancelled)})
	case delivery.StateCancelled:
		return marshal(DeliveryCancelResult{JobID: jobID, Supported: true, Cancelled: true, State: string(row.State), Reason: "already cancelled"})
	case delivery.StateFulfilled:
		return marshal(DeliveryCancelResult{JobID: jobID, Supported: false, Cancelled: false, State: string(row.State), Reason: "already fulfilled; nothing to cancel"})
	case delivery.StateUnknownOutcome:
		return marshal(DeliveryCancelResult{
			JobID: jobID, Supported: false, Cancelled: false, State: string(row.State),
			Reason: "reconciliation is open on this request; use 'papio delivery confirm-absent' once you have checked with the provider",
		})
	default: // StateSubmitted, StatePending: a live provider transaction exists.
		return marshal(DeliveryCancelResult{
			JobID: jobID, Supported: false, Cancelled: false, State: string(row.State),
			Reason: fmt.Sprintf("no configured provider (%s) supports API cancellation of a live request in this version; cancel directly with the institution", row.Provider),
		})
	}
}

// Decision 4's three document_delivery reconciliation operations. This is a
// new method rather than a widened actions.resolve, which ADR-0017's own
// investigation found is closed to identity review (its CAS verdict
// vocabulary is exactly "accept"/"reject" against a candidate binding) —
// mirroring its shape here would mean overloading that vocabulary with
// document-delivery semantics it was never ratified to carry. Decision 4
// explicitly forbids a fourth operation (retry_submission), so this
// vocabulary is exactly as closed as the blocker vocabulary above it.
const (
	deliveryOpOpenRequestHistory   = "open_request_history"
	deliveryOpConfirmRequestExists = "confirm_request_exists"
	deliveryOpConfirmRequestAbsent = "confirm_request_absent"
)

// DeliveryActionParams selects one Decision 4 operation for a job's open
// document_delivery human action. ProviderReference is required only for
// confirm_request_exists — the human is supplying the fact deterministic
// reconciliation could not determine.
type DeliveryActionParams struct {
	JobID             string `json:"job_id"`
	Operation         string `json:"operation"`
	ProviderReference string `json:"provider_reference,omitempty"`
}

// DeliveryActionResult reports what one Decision 4 operation did: the job
// state it left behind (empty for the read-only open_request_history), and
// the same explainable detail delivery.get returns.
type DeliveryActionResult struct {
	JobID     string          `json:"job_id"`
	Operation string          `json:"operation"`
	JobState  string          `json:"job_state,omitempty"`
	Detail    *DeliveryDetail `json:"detail,omitempty"`
}

func deliveryAction(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params DeliveryActionParams
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	switch params.Operation {
	case deliveryOpOpenRequestHistory:
		detail, rpcErr := deliveryRequestDetail(ctx, system, params.JobID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return marshal(DeliveryActionResult{JobID: params.JobID, Operation: params.Operation, Detail: detail})
	case deliveryOpConfirmRequestExists:
		if strings.TrimSpace(params.ProviderReference) == "" {
			return badParams(errors.New("provider_reference is required for confirm_request_exists"))
		}
		return deliveryConfirmRequestExists(ctx, system, params.JobID, params.ProviderReference)
	case deliveryOpConfirmRequestAbsent:
		return deliveryConfirmRequestAbsent(ctx, system, params.JobID)
	default:
		return badParams(fmt.Errorf("unknown operation %q; want one of %s, %s, %s",
			params.Operation, deliveryOpOpenRequestHistory, deliveryOpConfirmRequestExists, deliveryOpConfirmRequestAbsent))
	}
}

// openDocumentDeliveryAction finds the one open document_delivery human
// action Decision 4 says a job in this reconciliation state must have.
func openDocumentDeliveryAction(ctx context.Context, system *bootstrap.System, jobID string) (*job.HumanAction, *ipc.RPCError) {
	actions, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		return nil, failureErr(err)
	}
	for _, a := range actions {
		if a.Action.Kind == job.ActionKindDocumentDelivery && a.Action.Status == "open" {
			action := a.Action
			return &action, nil
		}
	}
	return nil, &ipc.RPCError{Code: "not_found", Message: "no open document_delivery action for this job"}
}

// deliveryConfirmRequestExists implements Decision 4's "the operator checked
// with the institution and a request really is on file": the row moves to
// pending with the human-supplied provider reference, the document_delivery
// action closes, and the job resumes as an ordinary pending delivery poll
// (StateRetryWait, RetryReasonDocumentDeliveryPending) — never
// retry_submission.
func deliveryConfirmRequestExists(ctx context.Context, system *bootstrap.System, jobID, providerReference string) ([]byte, *ipc.RPCError) {
	svc, rpcErr := deliveryService(system)
	if rpcErr != nil {
		return nil, rpcErr
	}
	row, err := svc.GetByJobID(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "no delivery request for this job"}
	}
	action, rpcErr := openDocumentDeliveryAction(ctx, system, jobID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	profile, err := svc.ResolveGateProfileFor(ctx, row.InstitutionProfile)
	if err != nil {
		return failure(err)
	}
	next := delivery.NextCheck(time.Now(), 0, profile.StatusPollMinutes)
	// One threaded tx over the single pooled *sql.DB (SetMaxOpenConns(1)).
	// Validate prerequisites before mutating; then apply delivery row mutations
	// and job transitions atomically. Human action is closed LAST via
	// RepairAwaitingHuman so a mid-sequence failure leaves the action OPEN and
	// the operator retains the reconciliation affordance.
	//
	// Full atomicity is via one sql.Tx threaded through Tx-variants on the
	// delivery and job stores (both share the same *store.Store / *sql.DB).
	// If future absent-path logic must call app.Service.SubmitDelivery inside
	// the same tx, that seam must gain a Tx-variant.
	db := system.Store.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return failure(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := svc.UpdateStateTx(ctx, tx, row.ID, delivery.StatePending); err != nil {
		return failure(err)
	}
	if err := svc.RecordPollTx(ctx, tx, row.ID, providerReference, next); err != nil {
		return failure(err)
	}
	repairDetail := map[string]any{"reason": "document_delivery_confirmed_exists"}
	repairDetail["from"], repairDetail["to"] = job.StateAwaitingHuman, job.StateResolving
	repairJSON, err := json.Marshal(repairDetail)
	if err != nil {
		return failure(err)
	}
	now := store.Now()
	if err := system.Jobs.RepairAwaitingHumanTx(ctx, tx, jobID, []int64{action.ID}, string(repairJSON), now); err != nil {
		return failure(err)
	}
	retryDetail := map[string]any{"reason": job.RetryReasonDocumentDeliveryPending, "provider_reference": providerReference}
	retryDetail["from"], retryDetail["to"] = job.StateResolving, job.StateRetryWait
	retryJSON, err := json.Marshal(retryDetail)
	if err != nil {
		return failure(err)
	}
	if err := system.Jobs.TransitionTx(ctx, tx, jobID, job.StateResolving, job.StateRetryWait, string(retryJSON), job.TransitionTxConfig{RetryAt: next.UTC().Format(time.RFC3339Nano)}, now); err != nil {
		return failure(err)
	}
	if err := tx.Commit(); err != nil {
		return failure(err)
	}
	detail, rpcErr := deliveryRequestDetail(ctx, system, jobID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return marshal(DeliveryActionResult{JobID: jobID, Operation: deliveryOpConfirmRequestExists, JobState: job.StateRetryWait, Detail: detail})
}

// deliveryConfirmRequestAbsent implements Decision 4's "the operator checked
// and no request exists": the stale row is cancelled, the document_delivery
// action closes, and the job re-enters the exact Branch/gate seam
// exhaustedCandidates uses (app.Service.SubmitDelivery) — never a duplicated
// policy implementation. In v1 that seam always resolves a cancelled row to
// BranchResubmissionPolicy, which deliveryRoute routes straight back to
// reconciliation (a fresh document_delivery action, job awaiting_human)
// rather than re-entering the gate: this operation never auto-resubmits.
// It exists to close the stale reconciliation action on a deliberate
// operator decision — "no request was ever lodged" — and open a new one,
// not to give the job another shot at ActionSubmit.
func deliveryConfirmRequestAbsent(ctx context.Context, system *bootstrap.System, jobID string) ([]byte, *ipc.RPCError) {
	svc, rpcErr := deliveryService(system)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "document delivery is not configured"}
	}
	row, err := svc.GetByJobID(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "no delivery request for this job"}
	}
	action, rpcErr := openDocumentDeliveryAction(ctx, system, jobID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// Ordering that satisfies both atomicity and the legal state graph:
	//   allowed[A->R] via RepairAwaitingHuman, allowed[R->A] via SubmitDelivery's
	//   reconciliation park. The prior Cancel->Submit->Repair order kept the
	//   action open on Submit failure (good) but Submit saw job still A and
	//   attempted A->A (illegal). Repair->Submit is the only legal order:
	//   Cancel row, Repair A->R (close old), Submit R->A (open new). If Submit
	//   fails after Repair, we compensate by re-opening a reconciliation
	//   action so the operator never loses the affordance (row cancelled is a
	//   documented recoverable state).
	if err := svc.UpdateState(ctx, row.ID, delivery.StateCancelled); err != nil {
		return failure(err)
	}
	if err := system.Jobs.RepairAwaitingHuman(ctx, jobID, []int64{action.ID},
		map[string]any{"reason": "document_delivery_confirmed_absent"}); err != nil {
		return failure(err)
	}
	_, submitErr := system.App.SubmitDelivery(ctx, jobID)
	if submitErr != nil {
		ref := row.ProviderReference
		if ref == "" {
			ref = "(no provider reference recorded)"
		}
		detail := fmt.Sprintf("a document-delivery request (provider %s, reference %s, state %s) needs reconciliation; run 'papio delivery get %s' for its history and resolve it by hand — papio never resubmits automatically",
			row.Provider, ref, string(delivery.StateCancelled), jobID)
		// Park first, then open the prompt — see the bridge sibling in
		// internal/browser for the full reasoning. Opening first could commit
		// the action and then fail the transition, leaving a document_delivery
		// prompt visible on a job still in resolving that RepairAwaitingHuman
		// refuses to act on.
		if parkErr := system.Jobs.Transition(ctx, jobID, job.StateResolving, job.StateAwaitingHuman, map[string]any{"reason": "document_delivery_reconciliation"}); parkErr == nil {
			_, _ = system.Jobs.OpenHumanAction(ctx, jobID, job.ActionKindDocumentDelivery, detail, job.Access(false, ""))
		}
		return failure(submitErr)
	}
	after, err := system.Jobs.Get(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	detail, rpcErr := deliveryRequestDetail(ctx, system, jobID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return marshal(DeliveryActionResult{JobID: jobID, Operation: deliveryOpConfirmRequestAbsent, JobState: after.State, Detail: detail})
}

// DeliveryResumeResult reports delivery.resume's outcome: Resumed is false
// with Reason, never a bare error, when request_id names a row that is not
// currently live (submitted/pending) — a terminal or never-submitted row
// has no live poll schedule to resume.
type DeliveryResumeResult struct {
	RequestID int64  `json:"request_id"`
	Resumed   bool   `json:"resumed"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// deliveryResume implements the P2 recovery gap a contract-drift poll park
// otherwise leaves with no operator-visible way out: delivery.cancel
// refuses a live row, confirm_request_exists/confirm_request_absent both
// require an open document_delivery human action a live submitted/pending
// row never has, and jobs.retry alone re-parks silently on the row's own
// still-future next_check_at (Poll is a no-op until it is due). This
// clears that row's poll-failure bookkeeping via delivery.Service.Resume
// so a subsequent `papio jobs retry <job-id>` actually attempts a poll —
// it deliberately does not itself force the job to wake; see Resume's doc
// comment for why the two stay separate operations.
//
// Keyed by request_id (delivery_requests.id, e.g. from delivery.get's
// Request.ID), not job_id: unlike every other delivery.* method, the
// operator-visible failure this recovers from is reported per poll-health
// row (papio doctor), and Decision 1's job_id is scoped to whichever job
// first created the row, not necessarily the job an operator is looking at
// today.
func deliveryResume(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		RequestID int64 `json:"request_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.RequestID <= 0 {
		if err == nil {
			err = errors.New("request_id is required and must be a positive integer")
		}
		return badParams(err)
	}
	svc, rpcErr := deliveryService(system)
	if rpcErr != nil {
		return nil, rpcErr
	}
	row, err := svc.Resume(ctx, params.RequestID)
	if err != nil {
		if errors.Is(err, delivery.ErrRequestNotLive) {
			return marshal(DeliveryResumeResult{
				RequestID: params.RequestID, Resumed: false, State: string(row.State),
				Reason: fmt.Sprintf("state %s is not live (submitted/pending); nothing to resume", row.State),
			})
		}
		return failure(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "no delivery request with this id"}
	}
	return marshal(DeliveryResumeResult{RequestID: params.RequestID, Resumed: true, State: string(row.State)})
}
