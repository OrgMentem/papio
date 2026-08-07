// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Package delivery owns the delivery_requests table (ADR-0017 Decision 1):
// the durable, idempotency-keyed record of a document-delivery/ILL request,
// the Decision 3A/3B compiled gate profile and per-request gate, and the
// provider-specific status-poll budget. It never touches HTTP itself —
// internal/illiad is the transport this service will drive in the CLI/daemon
// wiring layer — and it owns no job state; internal/job keeps that, exactly
// as ADR-0017 Decision 1 requires.
package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"papio/internal/config"
	"papio/internal/store"
)

// State is a delivery_requests.state value, mirroring the migration's CHECK
// constraint exactly (ADR-0017 Decision 1).
type State string

const (
	StateOffered        State = "offered"
	StateSubmitted      State = "submitted"
	StatePending        State = "pending"
	StateFulfilled      State = "fulfilled"
	StateDeclined       State = "declined"
	StateCancelled      State = "cancelled"
	StateUnknownOutcome State = "unknown_outcome"
)

func validState(s State) bool {
	switch s {
	case StateOffered, StateSubmitted, StatePending, StateFulfilled, StateDeclined, StateCancelled, StateUnknownOutcome:
		return true
	}
	return false
}

// Request mirrors one delivery_requests row.
type Request struct {
	ID                 int64
	JobID              string
	InstitutionProfile string
	Provider           string
	RequestClass       string
	WorkIdentity       string
	IdempotencyKey     string
	State              State
	ProviderReference  string
	GateProfileDigest  string
	SubmittedAt        string // RFC3339Nano, "" when unset
	LastCheckedAt      string
	NextCheckAt        string
	CreatedAt          string
	UpdatedAt          string

	// Poll health bookkeeping (0024_delivery_poll_health.sql). See
	// poll.go's doc comment for the exact semantics of
	// ProviderDisplayStatus vs. ProviderStatusRaw.
	ProviderStatusRaw       string
	ProviderDisplayStatus   string
	LastPollAt              string
	LastSuccessfulPollAt    string
	ConsecutivePollFailures int
	LastPollErrorClass      string
}

// Service is internal/delivery's store-backed entry point: request CRUD,
// the idempotency branch, live-acceptance bookkeeping, and gate events.
type Service struct {
	store *store.Store
	cfg   *config.Config
	now   func() time.Time
	// jitter overrides Poll's default backoff jitter (poll.go); nil uses
	// defaultJitter. Test-only seam, never set by New.
	jitter func(time.Duration) time.Duration
}

// New constructs a Service. now defaults to time.Now when nil.
func New(store *store.Store, cfg *config.Config, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, cfg: cfg, now: now}
}

// IdempotencyKey computes Decision 1's idempotency key: SHA-256 hex over
// institution profile + canonical work identity + provider + request class,
// NUL-separated so no field's content can ever bleed into an adjacent one.
func IdempotencyKey(institutionProfile, workIdentity, provider, requestClass string) string {
	h := sha256.New()
	h.Write([]byte(institutionProfile))
	h.Write([]byte{0})
	h.Write([]byte(workIdentity))
	h.Write([]byte{0})
	h.Write([]byte(provider))
	h.Write([]byte{0})
	h.Write([]byte(requestClass))
	return hex.EncodeToString(h.Sum(nil))
}

// ErrDuplicateRequest reports that Create found a live row already
// occupying the idempotency key. The caller receives that existing row
// alongside this error — Decision 1: "a resubmission attempt for the same
// key must resolve against the existing row, never open a second one."
var ErrDuplicateRequest = errors.New("delivery: a request already exists for this idempotency key")

// ErrRequestNotLive reports that Resume was asked to act on a request
// whose state is not submitted/pending — there is no live poll schedule
// to resume. The caller receives the unmodified row alongside this error
// so it can report the actual state without a second fetch.
var ErrRequestNotLive = errors.New("delivery: request is not live (submitted or pending)")

// CreateRequest is Create's input. State defaults to StateOffered when
// empty — the row a compiled-prefill route occupies before any live
// submission exists.
type CreateRequest struct {
	JobID              string
	InstitutionProfile string
	Provider           string
	RequestClass       string
	WorkIdentity       string
	State              State
	ProviderReference  string
	GateProfileDigest  string
}

// Create inserts a new delivery_requests row, keyed by IdempotencyKey. If a
// row already occupies that key, Create returns the existing row and
// ErrDuplicateRequest — never a second row.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Request, error) {
	state := req.State
	if state == "" {
		state = StateOffered
	}
	if !validState(state) {
		return nil, fmt.Errorf("delivery: invalid initial state %q", state)
	}
	key := IdempotencyKey(req.InstitutionProfile, req.WorkIdentity, req.Provider, req.RequestClass)
	now := store.Now()
	res, err := s.store.DB().ExecContext(ctx, `
		INSERT OR IGNORE INTO delivery_requests
			(job_id, institution_profile, provider, request_class, work_identity, idempotency_key,
			 state, provider_reference, gate_profile_digest, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.JobID, req.InstitutionProfile, req.Provider, req.RequestClass, req.WorkIdentity, key,
		string(state), req.ProviderReference, req.GateProfileDigest, now, now)
	if err != nil {
		return nil, fmt.Errorf("inserting delivery request: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		existing, err := s.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, fmt.Errorf("delivery: idempotency key %s reported a conflict but no row was found", key)
		}
		return existing, ErrDuplicateRequest
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// scanner is satisfied by both *sql.Row and *sql.Rows, letting Lookup, Get,
// and any future list query share one row decoder.
type scanner interface {
	Scan(dest ...any) error
}

var requestColumns = `id, job_id, institution_profile, provider, request_class, work_identity, idempotency_key,
	state, provider_reference, gate_profile_digest, submitted_at, last_checked_at, next_check_at, created_at, updated_at,
	provider_status_raw, provider_display_status, last_poll_at, last_successful_poll_at, consecutive_poll_failures, last_poll_error_class`

func scanRequest(row scanner) (*Request, error) {
	var r Request
	var state string
	var submittedAt, lastCheckedAt, nextCheckAt sql.NullString
	var providerStatusRaw, providerDisplayStatus, lastPollAt, lastSuccessfulPollAt, lastPollErrorClass sql.NullString
	if err := row.Scan(
		&r.ID, &r.JobID, &r.InstitutionProfile, &r.Provider, &r.RequestClass, &r.WorkIdentity, &r.IdempotencyKey,
		&state, &r.ProviderReference, &r.GateProfileDigest,
		&submittedAt, &lastCheckedAt, &nextCheckAt, &r.CreatedAt, &r.UpdatedAt,
		&providerStatusRaw, &providerDisplayStatus, &lastPollAt, &lastSuccessfulPollAt, &r.ConsecutivePollFailures, &lastPollErrorClass,
	); err != nil {
		return nil, err
	}
	r.State = State(state)
	r.SubmittedAt = submittedAt.String
	r.LastCheckedAt = lastCheckedAt.String
	r.NextCheckAt = nextCheckAt.String
	r.ProviderStatusRaw = providerStatusRaw.String
	r.ProviderDisplayStatus = providerDisplayStatus.String
	r.LastPollAt = lastPollAt.String
	r.LastSuccessfulPollAt = lastSuccessfulPollAt.String
	r.LastPollErrorClass = lastPollErrorClass.String
	return &r, nil
}

// Lookup returns the row for an idempotency key, or (nil, nil) when none
// exists.
func (s *Service) Lookup(ctx context.Context, idempotencyKey string) (*Request, error) {
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+requestColumns+` FROM delivery_requests WHERE idempotency_key = ?`, idempotencyKey)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Get returns the row by primary key, or (nil, nil) when none exists.
func (s *Service) Get(ctx context.Context, id int64) (*Request, error) {
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+requestColumns+` FROM delivery_requests WHERE id = ?`, id)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// GetByJobID returns the delivery_requests row for a job, or (nil, nil) when
// the job was never routed through document delivery. A job's idempotency
// key is scoped to institution+work+provider+request_class (Decision 1), not
// to the job itself, so in principle more than one row could reference the
// same job_id across resubmission policy changes; ORDER BY id DESC picks the
// most recently created one, which is always the row a caller asking "what
// is this job's delivery state" means.
func (s *Service) GetByJobID(ctx context.Context, jobID string) (*Request, error) {
	row := s.store.DB().QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM delivery_requests WHERE job_id = ? ORDER BY id DESC LIMIT 1`, jobID)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// UpdateState transitions a row's state. Entering StateSubmitted stamps
// submitted_at the first time only (COALESCE), so a later re-observation of
// the same live request never resets the cap-counting anchor.
func (s *Service) UpdateState(ctx context.Context, id int64, state State) error {
	if !validState(state) {
		return fmt.Errorf("delivery: invalid state %q", state)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if state == StateSubmitted {
		_, err := s.store.DB().ExecContext(ctx, `
			UPDATE delivery_requests
			SET state = ?, updated_at = ?, submitted_at = COALESCE(submitted_at, ?)
			WHERE id = ?`, string(state), now, now, id)
		return err
	}
	_, err := s.store.DB().ExecContext(ctx, `UPDATE delivery_requests SET state = ?, updated_at = ? WHERE id = ?`, string(state), now, id)
	return err
}

// RecordPoll updates a row's provider reference and poll bookkeeping after
// a status check: last_checked_at becomes now, next_check_at becomes the
// caller-supplied schedule (typically NextCheck's result).
func (s *Service) RecordPoll(ctx context.Context, id int64, providerReference string, nextCheckAt time.Time) error {
	now := store.Now()
	next := nextCheckAt.UTC().Format(time.RFC3339Nano)
	_, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET provider_reference = ?, last_checked_at = ?, next_check_at = ?, updated_at = ?
		WHERE id = ?`, providerReference, now, next, now, id)
	return err
}

// Resume clears a live (submitted/pending) request's poll-failure
// bookkeeping — consecutive_poll_failures, last_poll_error_class, and
// next_check_at — including a contract-drift park at
// pollDisabledDelay (internal/delivery/poll.go, effectively "until an
// operator intervenes"). It never changes state or any successful-poll
// column: only an actual provider response ever does that (the same
// invariant recordPollFailure's own discipline already protects).
//
// This alone does not force papio to poll again — Poll is a no-op until
// NextCheckAt is due, and the row's own next_check_at governs that, not
// the driving job's retry_at — so an operator combines this with
// `papio jobs retry <job-id>` (jobs.Retry) to actually wake the job and
// retry now. Splitting the two matters: `jobs retry` alone previously
// re-parked silently on the still-future next_check_at Poll no-op'd
// against, with no operator-visible recovery path at all for a
// contract-drift park (delivery.cancel refuses a live row, and
// confirm-exists/confirm-absent both require an open document_delivery
// human action a live submitted/pending row never has).
//
// Returns (row, ErrRequestNotLive) — never a bare error — when id names a
// row whose state is not live, so callers can report a structured refusal
// instead of propagating a raw error. Returns (nil, nil) when no row with
// this id exists.
func (s *Service) Resume(ctx context.Context, id int64) (*Request, error) {
	req, err := s.Get(ctx, id)
	if err != nil || req == nil {
		return req, err
	}
	if req.State != StateSubmitted && req.State != StatePending {
		return req, ErrRequestNotLive
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.store.DB().ExecContext(ctx, `
		UPDATE delivery_requests
		SET consecutive_poll_failures = 0, last_poll_error_class = NULL, next_check_at = ?, updated_at = ?
		WHERE id = ?`, now, now, id); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// BranchDecision is the idempotency branch Decision 3B evaluates before the
// per-request gate: "no existing row → evaluate the gate; submitted/pending
// → join and poll; fulfilled → fetch, adopt, validate; unknown_outcome →
// reconcile; declined/cancelled → apply the explicit resubmission policy."
type BranchDecision string

const (
	BranchEvaluateGate       BranchDecision = "evaluate_gate"
	BranchJoinPoll           BranchDecision = "join_poll"
	BranchAdoptFulfilled     BranchDecision = "adopt_fulfilled"
	BranchReconcile          BranchDecision = "reconcile"
	BranchResubmissionPolicy BranchDecision = "resubmission_policy"
)

// Branch implements Decision 3B's idempotency branch for the given key,
// evaluated strictly before the per-request gate (the former condition 8,
// "not already submitted", was a state-machine bug, not a gate condition).
func (s *Service) Branch(ctx context.Context, key string) (BranchDecision, *Request, error) {
	row, err := s.Lookup(ctx, key)
	if err != nil {
		return "", nil, err
	}
	if row == nil {
		return BranchEvaluateGate, nil, nil
	}
	switch row.State {
	case StateSubmitted, StatePending:
		return BranchJoinPoll, row, nil
	case StateFulfilled:
		return BranchAdoptFulfilled, row, nil
	case StateUnknownOutcome:
		return BranchReconcile, row, nil
	case StateDeclined, StateCancelled:
		return BranchResubmissionPolicy, row, nil
	default:
		// StateOffered: the row exists (occupying the idempotency key) but
		// no live submission was ever attempted — the gate previously
		// routed to prefill/enrich_then_prefill. The ADR's branch table
		// (Decision 3B) does not name this state explicitly; treating it
		// like "no existing row" for gating purposes is the only reading
		// consistent with the ADR ("no live subscription-provider request"
		// exists yet) while Create's duplicate-key rejection still stops a
		// second row: a caller re-evaluating gets a fresh verdict (an
		// acceptance recorded since the last offer, say), but any create it
		// attempts resolves onto this same row.
		return BranchEvaluateGate, row, nil
	}
}

// SubmittedThisMonth counts requests this institution profile has actually
// submitted to provider within the current UTC month (Decision 3B
// condition 7's cap headroom check). It counts by submitted_at, not current
// state, so a request that later moved to fulfilled/declined/unknown_outcome
// still counts against the month it was submitted in. Both the lower bound
// (first of this month) and upper bound (first of next month) are applied
// so a row submitted in a later month never bleeds into an earlier month's
// count.
func (s *Service) SubmittedThisMonth(ctx context.Context, institutionProfile, provider string) (int, error) {
	now := s.now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	var count int
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM delivery_requests
		WHERE institution_profile = ? AND provider = ?
		  AND submitted_at IS NOT NULL AND submitted_at >= ? AND submitted_at < ?`,
		institutionProfile, provider,
		monthStart.Format(time.RFC3339Nano), monthEnd.Format(time.RFC3339Nano)).Scan(&count)
	return count, err
}

// ResolveGateProfile is the store-aware wrapper around the pure
// CompileGateProfile: it compiles the static profile, then folds in the
// real, store-backed live-acceptance fact CompileGateProfile can never
// itself observe.
func (s *Service) ResolveGateProfile(ctx context.Context, profileName string, inst config.Institution) (GateProfile, error) {
	profile := CompileGateProfile(inst, profileName)
	if profile.Provider == "" {
		return profile, nil
	}
	accepted, err := s.HasLiveAcceptance(ctx, profileName, profile.Provider)
	if err != nil {
		return GateProfile{}, err
	}
	return profile.WithLiveAcceptance(accepted), nil
}

// ResolveGateProfileFor is ResolveGateProfile's config-driven convenience:
// it resolves profileName through the Service's own configuration before
// folding in live acceptance, so callers that only have a profile name
// (the CLI, the poller) never need to thread an Institution through by hand.
func (s *Service) ResolveGateProfileFor(ctx context.Context, profileName string) (GateProfile, error) {
	inst, ok := s.cfg.InstitutionFor(profileName)
	// InstitutionFor's ok-flag keys on the OpenURL base, but a document-
	// delivery route needs no OpenURL base to exist (the app reaches the
	// delivery path without one). A profile whose only institutional fact
	// is its document_delivery block is still a real profile here.
	if !ok && inst.DocumentDelivery == nil {
		return GateProfile{ProfileName: profileName, Class: GateClassInvalid, Blockers: []Blocker{{
			Code:     BlockerInstitutionPolicyUnknown,
			Evidence: "no institution profile named " + quote(profileName) + " is configured",
		}}}, nil
	}
	return s.ResolveGateProfile(ctx, profileName, inst)
}

// eventKindLiveAcceptance is the global (job-less) event kind
// RecordLiveAcceptance appends. ADR-0017 Decision 3C already names this
// concept as an event, not a row: "papio doctor ... never records
// live-accepted without the Decision 3A acceptance event."
//
// Persistence choice (Decision 3A): delivery_requests.job_id is NOT NULL
// REFERENCES jobs(id), so a live-acceptance marker — which has no
// associated job — cannot live there without fabricating a job row, which
// would be exactly the table abuse the ADR warns against. The schema (as of
// migrations 0001-0022) has no settings/meta/kv table either. events.job_id
// carries no NOT NULL or foreign-key constraint (0001_init.sql) and
// store.AppendEvent already accepts an empty jobID as a global event, so the
// existing append-only events table is the least-invasive durable home:
// exactly the "acceptance event" Decision 3C's own language names.
const eventKindLiveAcceptance = "delivery.live_acceptance_recorded"

type liveAcceptanceDetail struct {
	InstitutionProfile string `json:"institution_profile"`
	Provider           string `json:"provider"`
}

// RecordLiveAcceptance durably records that one supervised submit-and-
// reconcile against the real deployment completed under the institution's
// authority (Decision 3A's third hard rule). Wiring an operator-facing
// trigger for this call is Phase C's CLI/doctor concern; this is the
// storage primitive it will call.
func (s *Service) RecordLiveAcceptance(ctx context.Context, profileName, provider string) error {
	return s.store.AppendEvent(ctx, "", eventKindLiveAcceptance, map[string]any{
		"institution_profile": profileName,
		"provider":            provider,
	})
}

// HasLiveAcceptance reports whether RecordLiveAcceptance has ever fired for
// this institution profile + provider pair.
func (s *Service) HasLiveAcceptance(ctx context.Context, profileName, provider string) (bool, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT detail_json FROM events WHERE kind = ?`, eventKindLiveAcceptance)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var detail liveAcceptanceDetail
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			continue
		}
		if detail.InstitutionProfile == profileName && detail.Provider == provider {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// GateEvaluated is the redaction-safe summary of one Decision 3B gate
// evaluation, persisted so `papio delivery get` and `jobs get_v3` can
// explain why a nominally auto-capable profile did or did not submit.
type GateEvaluated struct {
	ProfileClass  GateClass
	ProfileDigest string
	Decision      Decision
	// FulfillmentChannel snapshots GateProfile.FulfillmentChannel at
	// evaluation time (2026-08-07 ADR-0017 amendment): "" or
	// FulfillmentChannelPatronWeb. Recorded alongside the submission
	// verdict so `papio delivery get`/`jobs get_v3` can distinguish
	// submission-auto from end-to-end-auto without recomputing against a
	// since-edited profile.
	FulfillmentChannel string
}

const eventKindGateEvaluated = "delivery.gate_evaluated"

// AppendGateEvent writes one delivery.gate_evaluated event: profile class,
// profile digest, decision, and blockers — never patron_ref, api_key, or
// any other secret/redacted field (ADR-0017 Decision 3B).
func (s *Service) AppendGateEvent(ctx context.Context, jobID string, evt GateEvaluated) error {
	return s.store.AppendEvent(ctx, jobID, eventKindGateEvaluated, map[string]any{
		"profile_class":       string(evt.ProfileClass),
		"profile_digest":      evt.ProfileDigest,
		"decision":            string(evt.Decision.Action),
		"blockers":            evt.Decision.Blockers,
		"fulfillment_channel": evt.FulfillmentChannel,
	})
}

// eventKindOrphaned records that OrphanIfLive found a live request whose
// driving job stopped watching it (ADR-0017 Decision 4).
const eventKindOrphaned = "delivery.orphaned"

// OrphanIfLive reconciles jobID's delivery_requests row when the job that
// was driving it is cancelled, or its document_delivery action is
// dismissed: papio stops polling that job entirely, so a row still
// submitted/pending would otherwise look like it is still being watched
// when nothing will ever check on it again. Only a genuinely live row is
// touched — offered (no vendor request exists yet), fulfilled, declined,
// cancelled, and already-unknown_outcome rows need no compensation, and a
// job with no delivery row at all is a no-op. The request itself is
// untouched at the provider; this only marks papio's own record honestly,
// so reconciliation (`papio delivery get`/confirm-exists/confirm-absent)
// is the way an operator picks it back up, exactly as an automatic poll
// finding an unrecoverable outcome already uses unknown_outcome for.
func (s *Service) OrphanIfLive(ctx context.Context, jobID, cause string) error {
	req, err := s.GetByJobID(ctx, jobID)
	if err != nil || req == nil {
		return err
	}
	if req.State != StateSubmitted && req.State != StatePending {
		return nil
	}
	if err := s.UpdateState(ctx, req.ID, StateUnknownOutcome); err != nil {
		return err
	}
	return s.store.AppendEvent(ctx, jobID, eventKindOrphaned, map[string]any{
		"delivery_request_id": req.ID,
		"cause":               cause,
	})
}

// LatestGateEvent returns the most recently recorded delivery.gate_evaluated
// verdict for jobID, or (nil, nil) when the job has never been gated. Reading
// the recorded event rather than recomputing EvaluateGate against current
// configuration is deliberate: `jobs.get_v3` and `delivery.get` explain the
// decision papio actually made, which a live recompute could silently
// disagree with after a profile edit.
func (s *Service) LatestGateEvent(ctx context.Context, jobID string) (*GateEvaluated, error) {
	var detailJSON string
	err := s.store.DB().QueryRowContext(ctx,
		`SELECT detail_json FROM events WHERE job_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		jobID, eventKindGateEvaluated).Scan(&detailJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var detail struct {
		ProfileClass       string   `json:"profile_class"`
		ProfileDigest      string   `json:"profile_digest"`
		Decision           string   `json:"decision"`
		Blockers           []string `json:"blockers"`
		FulfillmentChannel string   `json:"fulfillment_channel"`
	}
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		return nil, fmt.Errorf("decoding gate event: %w", err)
	}
	return &GateEvaluated{
		ProfileClass:       GateClass(detail.ProfileClass),
		ProfileDigest:      detail.ProfileDigest,
		Decision:           Decision{Action: Action(detail.Decision), Blockers: detail.Blockers},
		FulfillmentChannel: detail.FulfillmentChannel,
	}, nil
}

// Digest is a stable fingerprint of the compiled profile, recorded as
// delivery_requests.gate_profile_digest so a later profile recompile never
// gets silently misattributed to an older decision (0021_delivery_requests.sql).
func (p GateProfile) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%t\x00%t",
		p.Class, p.Provider, p.SubmitPolicy, p.PatronFeePolicy, p.MonthlyRequestCap, p.RequiresOperatorStep, p.LiveAccepted)
	classes := make([]string, 0, len(p.SupportedRequestClasses))
	for c := range p.SupportedRequestClasses {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		fmt.Fprintf(h, "\x00%s", c)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// defaultStatusPollMinutes is used when a profile's status_poll_minutes is
// 0 (config's documented default resolution).
const defaultStatusPollMinutes = 60

// maxPollInterval bounds NextCheck's backoff (Decision 4's separate poll
// budget, never the ordinary resolver/HTTP retry budget).
const maxPollInterval = 24 * time.Hour

// NextCheck computes the next scheduler-visible poll time for a pending
// delivery request: bounded exponential backoff (statusPollMinutes * 2^attempt,
// capped at maxPollInterval) on the provider's own status-poll budget
// (ADR-0017 Decision 1/4). attempt is the number of polls already made
// (0 for the first scheduled check).
func NextCheck(now time.Time, attempt int, statusPollMinutes int) time.Time {
	base := statusPollMinutes
	if base <= 0 {
		base = defaultStatusPollMinutes
	}
	if attempt < 0 {
		attempt = 0
	}
	shift := attempt
	if shift > 10 {
		// 60min << 10 is already ~42 days, far past the 24h cap; clamping
		// the shift avoids any risk of overflowing the time.Duration
		// multiplication for a pathologically large attempt count.
		shift = 10
	}
	backoff := time.Duration(base) * time.Minute * time.Duration(uint64(1)<<uint(shift))
	if backoff <= 0 || backoff > maxPollInterval {
		backoff = maxPollInterval
	}
	return now.Add(backoff)
}
