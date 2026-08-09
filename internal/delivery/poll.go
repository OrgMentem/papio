// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"papio/internal/illiad"
	"papio/internal/store"
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

// casConflictRecheckDelay is the short wait a compare-and-swap loss
// schedules: two workers joined on the same live delivery_requests row
// each read their own snapshot before either wrote, so the loser's write
// never applies (see persistPollSuccess/persistPollFailure). Losing the
// race means the winner already advanced the row — this only makes sure
// the loser's job wakes again soon enough to observe the real outcome via
// a fresh delivery.Branch/Get, not that it retries the same work.
const casConflictRecheckDelay = time.Minute

// pendingEvent is one event a successful CAS'd write should append, in
// the SAME transaction as the write — so a lost CAS race (another worker
// already advanced this row) can never leave behind a duplicate event for
// a write that didn't actually apply.
type pendingEvent struct {
	kind   string
	detail map[string]any
}

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
//
// The three Request Finished sub-branches that resolve from
// DeliveredToWeb/Cancelled* prior evidence are defense-in-depth, not a
// live production path: Poll's own isPollTerminal guard stops polling the
// instant a row settles fulfilled/declined/cancelled, so
// ProviderDisplayStatus can never hold one of those values while the row
// is still live enough to receive another read — reaching them requires
// an out-of-band state edit (a test, a future reconciliation feature that
// reopens a row, manual DB surgery). They exist so such an edit still
// fails safe instead of forcing an incorrect unknown_outcome. The only
// Request Finished path a row actually walks in production is the
// ambiguous one: defer once, then settle unknown_outcome on the second
// consecutive read with still no evidence.
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
			// Defense-in-depth only — see the func doc comment.
			return StateFulfilled, raw, false
		case illiadStatusCancelledByCustomer:
			// Defense-in-depth only — see the func doc comment.
			return StateCancelled, raw, false
		case illiadStatusCancelledByILLStaff:
			// Defense-in-depth only — see the func doc comment.
			return StateDeclined, raw, false
		case illiadStatusRequestFinished:
			// The live production path: second consecutive Request
			// Finished with no fulfillment/cancellation evidence — the
			// one delayed reconciliation pass already ran and found
			// nothing new.
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

	events := pollSuccessEvents(req, newState, settled, unmapped, txn.TransactionStatus)
	applied, err := s.persistPollSuccess(ctx, req, now, txn.TransactionStatus, nextDisplay, nextCheckAt, newState, events)
	if err != nil {
		return PollResult{}, err
	}
	if !applied {
		// Lost the race: another worker's Poll call already advanced this
		// row past the snapshot this call read (two jobs joined on the
		// same live request each read before either wrote). Losing the
		// race means someone else already polled it — nothing left to do
		// here, and never a duplicate settlement event or a regression of
		// whatever state the winner wrote.
		return PollResult{State: req.State, NextCheckAt: now.Add(casConflictRecheckDelay)}, nil
	}

	return PollResult{Settled: settled, State: req.State, NextCheckAt: nextCheckAt}, nil
}

// pollSuccessEvents decides which event(s) a successful poll's CAS'd write
// appends, computed before the write so persistPollSuccess can append them
// in the SAME transaction — a lost CAS race then never leaves behind an
// event for a write that did not actually apply.
func pollSuccessEvents(req *Request, newState State, settled, unmapped bool, rawStatus string) []pendingEvent {
	var events []pendingEvent
	switch {
	case newState == StateFulfilled:
		events = append(events, pendingEvent{kind: eventKindFulfilled, detail: map[string]any{
			"provider_reference":  req.ProviderReference,
			"provider_status":     rawStatus,
			"delivery_request_id": req.ID,
		}})
	case settled:
		events = append(events, pendingEvent{kind: eventKindPollSettled, detail: map[string]any{
			"state":               string(newState),
			"provider_status":     rawStatus,
			"delivery_request_id": req.ID,
		}})
	}
	if unmapped {
		events = append(events, pendingEvent{kind: eventKindProviderStatusUnmapped, detail: map[string]any{
			"provider_status":     rawStatus,
			"delivery_request_id": req.ID,
		}})
	}
	return events
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
			var semanticErr *reconciliationSemanticError
			if errors.As(findErr, &semanticErr) {
				return s.persistUnknownOutcome(ctx, req, now)
			}
			// The listing call itself failed — ordinary transient
			// discipline, never escalate on a failed reconciliation attempt.
			return s.recordPollFailure(ctx, req, now, deps, PollErrorClassTransient)
		}
		if matched != nil {
			req.ProviderReference = strconv.Itoa(matched.TransactionNumber)
			applied, err := s.persistProviderReference(ctx, req)
			if err != nil {
				return PollResult{}, err
			}
			if !applied {
				return PollResult{State: req.State, NextCheckAt: now.Add(casConflictRecheckDelay)}, nil
			}
			return s.recordPollSuccess(ctx, req, now, deps, *matched)
		}
		return s.recordPollFailureAt(ctx, req, now, PollErrorClassNotFoundReconciling, now.Add(notFoundReconciliationRecheckDelay))
	}
}

// reconcileViaUserRequests searches the patron's request listing for the
// transaction that carries req's idempotency key in the configured reference
// field. It is the poll adapter over the shared exact-one matcher used by
// ambiguous submission reconciliation.
func (s *Service) reconcileViaUserRequests(ctx context.Context, deps PollDeps, req *Request) (*illiad.Transaction, error) {
	if deps.PatronRef == "" || deps.ReferenceField == "" {
		return nil, nil
	}
	identity := ReconciliationIdentity{RequestClass: req.RequestClass}
	if strings.HasPrefix(req.WorkIdentity, "doi:") {
		identity.DOI = strings.TrimPrefix(req.WorkIdentity, "doi:")
	} else if strings.HasPrefix(req.WorkIdentity, "pmid:") {
		identity.PMID = strings.TrimPrefix(req.WorkIdentity, "pmid:")
	}
	found, reason, err := findReconciledTransaction(ctx, deps.Client, deps.PatronRef, deps.ReferenceField, req, identity)
	if err != nil {
		return nil, classifyReconciliationReadError(err)
	}
	if reason != "" {
		return nil, &reconciliationSemanticError{reason: reason}
	}
	return found, nil
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
	applied, err := s.persistPollFailure(ctx, req, now, class, failures, nextCheck)
	if err != nil {
		return PollResult{}, err
	}
	if !applied {
		return PollResult{State: req.State, NextCheckAt: now.Add(casConflictRecheckDelay)}, nil
	}
	return PollResult{State: req.State, NextCheckAt: nextCheck}, nil
}

// recordPollFailureAt is recordPollFailure with a caller-supplied fixed
// delay (the 404 propagation-delay and reconciliation-recheck cases,
// which use a short fixed wait instead of exponential backoff).
func (s *Service) recordPollFailureAt(ctx context.Context, req *Request, now time.Time, class string, nextCheck time.Time) (PollResult, error) {
	failures := req.ConsecutivePollFailures + 1
	applied, err := s.persistPollFailure(ctx, req, now, class, failures, nextCheck)
	if err != nil {
		return PollResult{}, err
	}
	if !applied {
		return PollResult{State: req.State, NextCheckAt: now.Add(casConflictRecheckDelay)}, nil
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
	return time.Duration(rand.Int64N(int64(max) + 1)) //nolint:gosec // G404: non-cryptographic poll jitter.
}

// persistPollSuccess compare-and-swaps the row: the UPDATE only applies
// WHERE state/next_check_at still match what this call originally read
// (req.State/req.NextCheckAt, captured before any write). Two workers
// joined on the same live request each read their own snapshot before
// either wrote — the first commit wins, the second's WHERE clause no
// longer matches and RowsAffected is 0, reported back as applied=false.
// events append inside the same transaction as the CAS'd UPDATE, so a
// lost race can never leave a duplicate (or orphaned) event behind.
func (s *Service) persistPollSuccess(ctx context.Context, req *Request, now time.Time, raw, display string, nextCheckAt time.Time, newState State, events []pendingEvent) (bool, error) {
	origState := req.State
	origNextCheckAt := req.NextCheckAt
	nowStr := now.UTC().Format(time.RFC3339Nano)
	var nextCheckVal any
	if !nextCheckAt.IsZero() {
		nextCheckVal = nextCheckAt.UTC().Format(time.RFC3339Nano)
	}
	state := origState
	if newState != "" {
		state = newState
	}

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

	res, err := tx.ExecContext(ctx, `
		UPDATE delivery_requests
		SET state = ?, provider_status_raw = ?, provider_display_status = ?,
		    last_poll_at = ?, last_successful_poll_at = ?, consecutive_poll_failures = 0,
		    last_poll_error_class = NULL, next_check_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND COALESCE(next_check_at,'') = ?`,
		string(state), raw, display, nowStr, nowStr, nextCheckVal, nowStr,
		req.ID, string(origState), origNextCheckAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	for _, ev := range events {
		if err := appendEventTx(ctx, tx, req.JobID, ev.kind, ev.detail); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
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
	return true, nil
}

// persistPollFailure is persistPollSuccess's CAS discipline for the
// failure path: no event is ever appended here (a failed poll never
// settles anything), so a single guarded UPDATE is enough.
func (s *Service) persistPollFailure(ctx context.Context, req *Request, now time.Time, class string, failures int, nextCheck time.Time) (bool, error) {
	origState := req.State
	origNextCheckAt := req.NextCheckAt
	nowStr := now.UTC().Format(time.RFC3339Nano)
	nextStr := nextCheck.UTC().Format(time.RFC3339Nano)
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET last_poll_at = ?, consecutive_poll_failures = ?, last_poll_error_class = ?, next_check_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND COALESCE(next_check_at,'') = ?`,
		nowStr, failures, class, nextStr, nowStr, req.ID, string(origState), origNextCheckAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	req.LastPollAt = nowStr
	req.ConsecutivePollFailures = failures
	req.LastPollErrorClass = class
	req.NextCheckAt = nextStr
	return true, nil
}

// persistUnknownOutcome CAS-settles a row via the 404-reconciliation path,
// which has no fresh raw transaction to record — ProviderStatusRaw is left
// as whatever the last successful read observed. The event appends in the
// same transaction as the CAS'd UPDATE, same discipline as
// persistPollSuccess.
func (s *Service) persistUnknownOutcome(ctx context.Context, req *Request, now time.Time) (PollResult, error) {
	origState := req.State
	origNextCheckAt := req.NextCheckAt
	nowStr := now.UTC().Format(time.RFC3339Nano)

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return PollResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE delivery_requests
		SET state = ?, last_poll_at = ?, consecutive_poll_failures = 0, last_poll_error_class = NULL, next_check_at = NULL, updated_at = ?
		WHERE id = ? AND state = ? AND COALESCE(next_check_at,'') = ?`,
		string(StateUnknownOutcome), nowStr, nowStr, req.ID, string(origState), origNextCheckAt)
	if err != nil {
		return PollResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PollResult{}, err
	}
	if n == 0 {
		// Lost the race: leave whatever the winner already wrote alone.
		return PollResult{State: req.State, NextCheckAt: now.Add(casConflictRecheckDelay)}, nil
	}
	if err := appendEventTx(ctx, tx, req.JobID, eventKindPollSettled, map[string]any{
		"state":               string(StateUnknownOutcome),
		"reason":              "404_after_reconciliation",
		"delivery_request_id": req.ID,
	}); err != nil {
		return PollResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PollResult{}, err
	}

	req.State = StateUnknownOutcome
	req.LastPollAt = nowStr
	req.ConsecutivePollFailures = 0
	req.LastPollErrorClass = ""
	req.NextCheckAt = ""
	return PollResult{Settled: true, State: StateUnknownOutcome}, nil
}

// persistProviderReference CAS-guards the reconciliation-recovered
// provider_reference write the same way as every other persist* helper:
// applied=false means another worker already advanced this row past the
// snapshot this call read, so the caller must not proceed to
// recordPollSuccess against a reference that no longer belongs to this
// row's current, already-changed state.
func (s *Service) persistProviderReference(ctx context.Context, req *Request) (bool, error) {
	origState := req.State
	origNextCheckAt := req.NextCheckAt
	now := s.now().UTC().Format(time.RFC3339Nano)
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests SET provider_reference = ?, updated_at = ?
		WHERE id = ? AND state = ? AND COALESCE(next_check_at,'') = ?`,
		req.ProviderReference, now, req.ID, string(origState), origNextCheckAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// appendEventTx is store.AppendEvent's insert, scoped to an in-flight
// transaction so a CAS'd state write and its settlement event commit or
// roll back together (never one without the other).
func appendEventTx(ctx context.Context, tx *sql.Tx, jobID, kind string, detail map[string]any) error {
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var job any
	if jobID != "" {
		job = jobID
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)", job, store.Now(), kind, string(data))
	return err
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
