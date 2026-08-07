// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"papio/internal/illiad"
)

// TransactionLookup is the ILLiad transport surface the poll executor
// needs. Satisfied by *illiad.Client. This package still never constructs
// its own HTTP client (see delivery.go's package doc comment) — the
// caller's wiring layer (internal/app) builds the Client from the
// institution's document_delivery config, exactly as it already does for
// CreateTransaction, and injects it here.
type TransactionLookup interface {
	GetTransaction(ctx context.Context, number int) (illiad.Transaction, error)
	UserRequests(ctx context.Context, userRef string) ([]illiad.Transaction, error)
}

// PollDeps are the poll executor's per-call dependencies, all sourced from
// the institution's document_delivery profile.
type PollDeps struct {
	Client TransactionLookup
	// PatronRef is the configured patron reference used for the
	// UserRequests reconciliation fallback (empty disables reconciliation,
	// not the ordinary poll).
	PatronRef string
	// ReferenceField names the ILLiad transaction field papio's
	// idempotency key was written to at submission
	// (illiadIdempotencyReferenceField in internal/app), used to match a
	// reconciled transaction back to this row.
	ReferenceField    string
	StatusPollMinutes int
}

// PollResult is Poll's report to the caller: whether the row settled into
// a terminal outcome this call, its state after the call, and when to next
// check (zero when settled — polling stops).
type PollResult struct {
	Settled     bool
	State       State
	NextCheckAt time.Time
}

// Poll-health failure classes, recorded on delivery_requests.
// last_poll_error_class and read back by papio doctor
// (internal/doctor.checkDocumentDelivery). A row's state is NEVER changed
// by a failed poll (ADR-0017 Decision 4's failure discipline) — only these
// bookkeeping columns advance.
const (
	PollErrorClassTransient                = "transient_transport"
	PollErrorClassCredential               = "credential"
	PollErrorClassContractDrift            = "contract_drift"
	PollErrorClassNotFoundPropagationDelay = "not_found_propagation_delay"
	PollErrorClassNotFoundReconciling      = "not_found_reconciling"
)

// Well-known, terminal-meaning ILLiad TransactionStatus values. ILLiad
// statuses are institution-customizable (ADR-0017 Decision 4: "there is no
// exhaustive enum") — every other status, including ones that look
// terminal, is treated as pending and reported via
// eventKindProviderStatusUnmapped rather than asserted.
const (
	illiadStatusDeliveredToWeb      = "Delivered to Web"
	illiadStatusCancelledByCustomer = "Cancelled by Customer"
	illiadStatusCancelledByILLStaff = "Cancelled by ILL Staff"
	illiadStatusAwaitingUnfilled    = "Awaiting Unfilled Processing"
	illiadStatusRequestFinished     = "Request Finished"
)

// requestFinishedReconciliationDelay is the short, fixed wait before the
// Request Finished tri-branch's one delayed reconciliation pass — never
// the ordinary exponential poll cadence, since the ambiguity is expected
// to resolve quickly (ADR-0017 Decision 4).
const requestFinishedReconciliationDelay = 15 * time.Minute

// notFoundPropagationDelay is the short, fixed retry wait for a 404 seen
// before this row has ever had a successful poll — ILLiad has simply not
// indexed the transaction yet.
const notFoundPropagationDelay = 15 * time.Minute

// notFoundReconciliationRecheckDelay is the short, fixed wait before the
// one delayed recheck a 404-after-success gets after UserRequests also
// fails to find the transaction.
const notFoundReconciliationRecheckDelay = 15 * time.Minute

// pollDisabledDelay is how long a decode/schema (contract drift) failure
// parks a row's next_check_at: effectively "until an operator
// intervenes" — this package never resubmits or retries a shape it could
// not parse.
const pollDisabledDelay = 10 * 365 * 24 * time.Hour

const (
	eventKindFulfilled              = "delivery.fulfilled"
	eventKindPollSettled            = "delivery.poll_settled"
	eventKindProviderStatusUnmapped = "delivery.provider_status_unmapped"
)

func isPollTerminal(s State) bool {
	switch s {
	case StateFulfilled, StateDeclined, StateCancelled, StateUnknownOutcome:
		return true
	}
	return false
}

// classifyStatus maps one successfully-read ILLiad TransactionStatus to a
// poll outcome, given the row's own previously recorded
// ProviderDisplayStatus ("prior evidence" — see 0024_delivery_poll_health.sql's
// comment).
//
// newState is "" when the state column must not change (ambiguous Request
// Finished with no evidence yet, or an unrecognized/custom status).
// nextDisplay is the value ProviderDisplayStatus should hold after this
// poll. unmapped reports a custom status recorded via
// eventKindProviderStatusUnmapped, never a terminal assertion.
func classifyStatus(raw, priorDisplay string) (newState State, nextDisplay string, unmapped bool) {
	switch raw {
	case illiadStatusDeliveredToWeb:
		return StateFulfilled, raw, false
	case illiadStatusCancelledByCustomer:
		return StateCancelled, raw, false
	case illiadStatusCancelledByILLStaff:
		return StateDeclined, raw, false
	case illiadStatusAwaitingUnfilled:
		// Still seeking suppliers — pending, never declined.
		return StatePending, raw, false
	case illiadStatusRequestFinished:
		switch priorDisplay {
		case illiadStatusDeliveredToWeb:
			return StateFulfilled, raw, false
		case illiadStatusCancelledByCustomer:
			return StateCancelled, raw, false
		case illiadStatusCancelledByILLStaff:
			return StateDeclined, raw, false
		case illiadStatusRequestFinished:
			// Second consecutive Request Finished with no fulfillment/
			// cancellation evidence: the one delayed reconciliation pass
			// already ran and found nothing new.
			return StateUnknownOutcome, raw, false
		default:
			// First observation, no evidence yet: leave state unchanged;
			// ProviderDisplayStatus becomes "Request Finished" so the
			// *next* poll recognizes this as the already-reconciled pass.
			return "", raw, false
		}
	default:
		// Unrecognized/custom status: never asserted terminal. Prior
		// evidence (a real Delivered to Web/Cancelled observation) is
		// preserved for a later Request Finished read.
		return StatePending, priorDisplay, true
	}
}

// Poll checks req's provider status when due (NextCheckAt <= now) and
// applies ADR-0017 Decision 4's state map and failure discipline. It is a
// no-op — no HTTP call, no bookkeeping write — when the row is not yet
// due; the caller re-parks on the existing schedule exactly as before.
//
// req is mutated in place to reflect whatever this call persisted, so a
// caller reading req immediately after (e.g. to build a reconciliation
// action's detail string) sees the post-poll row without a re-fetch.
func (s *Service) Poll(ctx context.Context, req *Request, deps PollDeps) (PollResult, error) {
	if isPollTerminal(req.State) {
		// Idempotent no-op: a caller that already routed a settled row
		// away from join_poll (delivery.Branch no longer returns it once
		// terminal) still gets a safe answer, never a second provider
		// call or a duplicate settlement event.
		return PollResult{Settled: true, State: req.State}, nil
	}
	now := s.now().UTC()
	if req.NextCheckAt != "" {
		if next, err := time.Parse(time.RFC3339Nano, req.NextCheckAt); err == nil && next.After(now) {
			return PollResult{State: req.State, NextCheckAt: next}, nil
		}
	}
	number, convErr := strconv.Atoi(req.ProviderReference)
	if convErr != nil {
		return PollResult{}, fmt.Errorf("delivery: poll: request %d has a non-numeric provider reference %q", req.ID, req.ProviderReference)
	}
	txn, err := deps.Client.GetTransaction(ctx, number)
	if err != nil {
		return s.handlePollError(ctx, req, now, deps, err)
	}
	return s.recordPollSuccess(ctx, req, now, deps, txn)
}

func (s *Service) recordPollSuccess(ctx context.Context, req *Request, now time.Time, deps PollDeps, txn illiad.Transaction) (PollResult, error) {
	newState, nextDisplay, unmapped := classifyStatus(txn.TransactionStatus, req.ProviderDisplayStatus)
	settled := newState != "" && isPollTerminal(newState)

	var nextCheckAt time.Time
	switch {
	case settled:
		// Polling stops; NextCheckAt persists as NULL.
	case newState == "":
		nextCheckAt = now.Add(requestFinishedReconciliationDelay)
	default:
		nextCheckAt = NextCheck(now, 0, deps.StatusPollMinutes)
	}

	if err := s.persistPollSuccess(ctx, req, now, txn.TransactionStatus, nextDisplay, nextCheckAt, newState); err != nil {
		return PollResult{}, err
	}

	switch {
	case newState == StateFulfilled:
		if err := s.appendPollEvent(ctx, req, eventKindFulfilled, map[string]any{
			"provider_reference": req.ProviderReference,
			"provider_status":    txn.TransactionStatus,
		}); err != nil {
			return PollResult{}, err
		}
	case settled:
		if err := s.appendPollEvent(ctx, req, eventKindPollSettled, map[string]any{
			"state":           string(newState),
			"provider_status": txn.TransactionStatus,
		}); err != nil {
			return PollResult{}, err
		}
	}
	if unmapped {
		if err := s.appendPollEvent(ctx, req, eventKindProviderStatusUnmapped, map[string]any{
			"provider_status": txn.TransactionStatus,
		}); err != nil {
			return PollResult{}, err
		}
	}

	return PollResult{Settled: settled, State: req.State, NextCheckAt: nextCheckAt}, nil
}

// handlePollError classifies a GetTransaction failure and applies the
// matching failure-discipline branch. State is never changed here except
// via the two 404-reconciliation paths, which either recover a fresh
// successful read (recordPollSuccess) or, after their one delayed
// recheck, settle unknown_outcome — never as a bare inference from the
// failure itself.
func (s *Service) handlePollError(ctx context.Context, req *Request, now time.Time, deps PollDeps, err error) (PollResult, error) {
	if errors.Is(err, illiad.ErrNotFound) {
		return s.handleNotFound(ctx, req, now, deps)
	}
	var credErr *illiad.CredentialError
	if errors.As(err, &credErr) {
		return s.recordPollFailure(ctx, req, now, deps, PollErrorClassCredential)
	}
	if _, ok := illiad.Temporary(err); ok {
		return s.recordPollFailure(ctx, req, now, deps, PollErrorClassTransient)
	}
	// Anything else — a decode/schema failure or an unexpected HTTP
	// status — is contract drift: never asserted terminal, but polling is
	// disabled for this row until an operator intervenes.
	return s.recordPollFailure(ctx, req, now, deps, PollErrorClassContractDrift)
}

func (s *Service) handleNotFound(ctx context.Context, req *Request, now time.Time, deps PollDeps) (PollResult, error) {
	switch {
	case req.LastSuccessfulPollAt == "":
		// Shortly after submit: ILLiad has not indexed the transaction
		// yet. Propagation delay, not a reconciliation case.
		return s.recordPollFailureAt(ctx, req, now, PollErrorClassNotFoundPropagationDelay, now.Add(notFoundPropagationDelay))
	case req.LastPollErrorClass == PollErrorClassNotFoundReconciling:
		// The one delayed recheck already ran (see the default branch
		// below) and the transaction is still absent: settle, but only
		// through this explicit reconciliation path.
		return s.persistUnknownOutcome(ctx, req, now)
	default:
		matched, findErr := s.reconcileViaUserRequests(ctx, deps, req)
		if findErr != nil {
			// The listing call itself failed — ordinary transient
			// discipline, never escalate on a failed reconciliation
			// attempt.
			return s.recordPollFailure(ctx, req, now, deps, PollErrorClassTransient)
		}
		if matched != nil {
			req.ProviderReference = strconv.Itoa(matched.TransactionNumber)
			if err := s.persistProviderReference(ctx, req); err != nil {
				return PollResult{}, err
			}
			return s.recordPollSuccess(ctx, req, now, deps, *matched)
		}
		return s.recordPollFailureAt(ctx, req, now, PollErrorClassNotFoundReconciling, now.Add(notFoundReconciliationRecheckDelay))
	}
}

// reconcileViaUserRequests searches the patron's request listing for the
// transaction that carries req's idempotency key in the configured
// reference field — ILLiad may have re-numbered or otherwise relocated a
// transaction that a direct-by-number lookup now 404s on.
func (s *Service) reconcileViaUserRequests(ctx context.Context, deps PollDeps, req *Request) (*illiad.Transaction, error) {
	if deps.PatronRef == "" || deps.ReferenceField == "" {
		return nil, nil
	}
	txns, err := deps.Client.UserRequests(ctx, deps.PatronRef)
	if err != nil {
		return nil, err
	}
	for i := range txns {
		if v, ok := txns[i].ReferenceValue(deps.ReferenceField); ok && v == req.IdempotencyKey {
			return &txns[i], nil
		}
	}
	return nil, nil
}

// recordPollFailure applies the ordinary exponential-backoff-with-jitter
// failure discipline: state and every successful-poll bookkeeping column
// are left untouched.
func (s *Service) recordPollFailure(ctx context.Context, req *Request, now time.Time, deps PollDeps, class string) (PollResult, error) {
	failures := req.ConsecutivePollFailures + 1
	var nextCheck time.Time
	if class == PollErrorClassContractDrift {
		nextCheck = now.Add(pollDisabledDelay)
	} else {
		nextCheck = s.backoffWithJitter(now, failures, deps.StatusPollMinutes)
	}
	if err := s.persistPollFailure(ctx, req, now, class, failures, nextCheck); err != nil {
		return PollResult{}, err
	}
	return PollResult{State: req.State, NextCheckAt: nextCheck}, nil
}

// recordPollFailureAt is recordPollFailure with a caller-supplied fixed
// delay (the 404 propagation-delay and reconciliation-recheck cases,
// which use a short fixed wait instead of exponential backoff).
func (s *Service) recordPollFailureAt(ctx context.Context, req *Request, now time.Time, class string, nextCheck time.Time) (PollResult, error) {
	failures := req.ConsecutivePollFailures + 1
	if err := s.persistPollFailure(ctx, req, now, class, failures, nextCheck); err != nil {
		return PollResult{}, err
	}
	return PollResult{State: req.State, NextCheckAt: nextCheck}, nil
}

// backoffWithJitter is delivery.NextCheck's exponential schedule plus a
// small random jitter (0-20% of the interval), so many rows failing
// against the same outage do not all retry in lockstep.
func (s *Service) backoffWithJitter(now time.Time, attempt int, statusPollMinutes int) time.Time {
	base := NextCheck(now, attempt, statusPollMinutes)
	interval := base.Sub(now)
	jitterFn := s.jitter
	if jitterFn == nil {
		jitterFn = defaultJitter
	}
	return base.Add(jitterFn(interval))
}

func defaultJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	max := interval / 5 // 20%
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

func (s *Service) persistPollSuccess(ctx context.Context, req *Request, now time.Time, raw, display string, nextCheckAt time.Time, newState State) error {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	var nextCheckVal any
	if !nextCheckAt.IsZero() {
		nextCheckVal = nextCheckAt.UTC().Format(time.RFC3339Nano)
	}
	state := req.State
	if newState != "" {
		state = newState
	}
	_, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET state = ?, provider_status_raw = ?, provider_display_status = ?,
		    last_poll_at = ?, last_successful_poll_at = ?, consecutive_poll_failures = 0,
		    last_poll_error_class = NULL, next_check_at = ?, updated_at = ?
		WHERE id = ?`,
		string(state), raw, display, nowStr, nowStr, nextCheckVal, nowStr, req.ID)
	if err != nil {
		return err
	}
	req.State = state
	req.ProviderStatusRaw = raw
	req.ProviderDisplayStatus = display
	req.LastPollAt = nowStr
	req.LastSuccessfulPollAt = nowStr
	req.ConsecutivePollFailures = 0
	req.LastPollErrorClass = ""
	if nextCheckVal != nil {
		req.NextCheckAt = nextCheckVal.(string)
	} else {
		req.NextCheckAt = ""
	}
	return nil
}

func (s *Service) persistPollFailure(ctx context.Context, req *Request, now time.Time, class string, failures int, nextCheck time.Time) error {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	nextStr := nextCheck.UTC().Format(time.RFC3339Nano)
	_, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET last_poll_at = ?, consecutive_poll_failures = ?, last_poll_error_class = ?, next_check_at = ?, updated_at = ?
		WHERE id = ?`, nowStr, failures, class, nextStr, nowStr, req.ID)
	if err != nil {
		return err
	}
	req.LastPollAt = nowStr
	req.ConsecutivePollFailures = failures
	req.LastPollErrorClass = class
	req.NextCheckAt = nextStr
	return nil
}

// persistUnknownOutcome settles a row via the 404-reconciliation path,
// which has no fresh raw transaction to record — ProviderStatusRaw is left
// as whatever the last successful read observed.
func (s *Service) persistUnknownOutcome(ctx context.Context, req *Request, now time.Time) (PollResult, error) {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	_, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET state = ?, last_poll_at = ?, consecutive_poll_failures = 0, last_poll_error_class = NULL, next_check_at = NULL, updated_at = ?
		WHERE id = ?`, string(StateUnknownOutcome), nowStr, nowStr, req.ID)
	if err != nil {
		return PollResult{}, err
	}
	req.State = StateUnknownOutcome
	req.LastPollAt = nowStr
	req.ConsecutivePollFailures = 0
	req.LastPollErrorClass = ""
	req.NextCheckAt = ""
	if err := s.appendPollEvent(ctx, req, eventKindPollSettled, map[string]any{
		"state":  string(StateUnknownOutcome),
		"reason": "404_after_reconciliation",
	}); err != nil {
		return PollResult{}, err
	}
	return PollResult{Settled: true, State: StateUnknownOutcome}, nil
}

func (s *Service) persistProviderReference(ctx context.Context, req *Request) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	_, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests SET provider_reference = ?, updated_at = ? WHERE id = ?`,
		req.ProviderReference, now, req.ID)
	return err
}

func (s *Service) appendPollEvent(ctx context.Context, req *Request, kind string, detail map[string]any) error {
	detail["delivery_request_id"] = req.ID
	return s.store.AppendEvent(ctx, req.JobID, kind, detail)
}

// PollHealth summarizes one live delivery_requests row's poll health for
// papio doctor (ADR-0017 Decision 4's health thresholds).
type PollHealth struct {
	RequestID               int64
	Provider                string
	State                   State
	ConsecutivePollFailures int
	LastPollErrorClass      string
	LastSuccessfulPollAt    string
	// Degraded is true at 3+ consecutive failed polls.
	Degraded bool
	// Unobservable is true when papio has gone more than 24h without a
	// successful poll — this describes papio's own blind spot, never a
	// claim that the request itself failed.
	Unobservable bool
}

const (
	pollHealthDegradedThreshold = 3
	pollHealthUnobservableAfter = 24 * time.Hour
)

// LivePollHealth returns poll-health rows for every live (submitted or
// pending) delivery_requests row under institutionProfile, for
// papio doctor's document_delivery section.
func (s *Service) LivePollHealth(ctx context.Context, institutionProfile string) ([]PollHealth, error) {
	now := s.now().UTC()
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT id, provider, state, consecutive_poll_failures, COALESCE(last_poll_error_class,''),
		       COALESCE(last_successful_poll_at,''), submitted_at
		FROM delivery_requests
		WHERE institution_profile = ? AND state IN ('submitted','pending')`, institutionProfile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []PollHealth
	for rows.Next() {
		var h PollHealth
		var state string
		var lastSuccess, submittedAt sql.NullString
		if err := rows.Scan(&h.RequestID, &h.Provider, &state, &h.ConsecutivePollFailures, &h.LastPollErrorClass,
			&lastSuccess, &submittedAt); err != nil {
			return nil, err
		}
		h.State = State(state)
		h.LastSuccessfulPollAt = lastSuccess.String
		h.Degraded = h.ConsecutivePollFailures >= pollHealthDegradedThreshold
		anchor := submittedAt.String
		if lastSuccess.String != "" {
			anchor = lastSuccess.String
		}
		if anchor != "" {
			if t, err := time.Parse(time.RFC3339Nano, anchor); err == nil {
				h.Unobservable = now.Sub(t) > pollHealthUnobservableAfter
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
