// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package job owns the durable acquisition state machine. Every transition is
// a compare-and-swap UPDATE plus an append-only event in one transaction;
// running work holds a lease; crash recovery expires leases and rewinds
// mid-flight stages to their last durable boundary (bearer URLs live only in
// the attempt's memory, so fetching/validating rewind to resolving).
package job

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"papio/internal/artifact"
	"papio/internal/store"
	"papio/internal/work"
)

// Job states (stack plan "Job states").
const (
	StateQueued        = "queued"
	StateResolving     = "resolving"
	StateFetching      = "fetching"
	StateValidating    = "validating"
	StateReady         = "ready"
	StateAwaitingHuman = "awaiting_human"
	StateRetryWait     = "retry_wait"
	StateNeedsReview   = "needs_review"
	StateUnavailable   = "unavailable"
	StateFailed        = "failed"
	StateCancelled     = "cancelled"
)

// Terminal reports whether a state ends the acquisition attempt. ready is the
// acquisition terminal; exports are separate idempotent records.
func Terminal(state string) bool {
	switch state {
	case StateReady, StateUnavailable, StateFailed, StateCancelled:
		return true
	}
	return false
}

// allowed maps from-state -> to-states. Recovery rewinds (fetching/validating
// -> resolving) are legal because candidates re-rank deterministically and the
// artifact store is content-addressed (no duplicates on re-fetch).
var allowed = map[string]map[string]bool{
	StateQueued: {StateResolving: true, StateCancelled: true},
	StateResolving: {
		StateFetching: true, StateReady: true, StateAwaitingHuman: true, StateRetryWait: true,
		StateNeedsReview: true, StateUnavailable: true, StateFailed: true, StateCancelled: true,
	},
	StateFetching: {
		StateValidating: true, StateResolving: true, StateRetryWait: true,
		StateAwaitingHuman: true, StateNeedsReview: true, StateUnavailable: true,
		StateFailed: true, StateCancelled: true,
	},
	StateValidating: {
		StateReady: true, StateFetching: true, StateResolving: true,
		StateNeedsReview: true, StateFailed: true, StateCancelled: true,
		// Adoption re-parks here on a transient validation/store error so the
		// supplied download is preserved for the directory sweep to retry,
		// rather than being rewound to resolving and replaced by an OA fetch.
		StateAwaitingHuman: true,
	},
	StateAwaitingHuman: {
		StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true,
		// Phase 2 browser bridge resumes a parked handoff directly: the extension's
		// terminal observations map to unavailable/needs_review/retry_wait, and an
		// adopted download re-enters validation. The adopting caller holds a lease so
		// the scheduler and RecoverStale cannot rewind the job mid-adoption.
		StateValidating: true, StateUnavailable: true, StateNeedsReview: true, StateRetryWait: true,
	},
	StateRetryWait:   {StateResolving: true, StateFetching: true, StateCancelled: true, StateFailed: true},
	StateNeedsReview: {StateResolving: true, StateFetching: true, StateCancelled: true},
}

// ErrConflict is returned when a CAS transition loses (state changed or the
// transition is not allowed).
var ErrConflict = errors.New("job state conflict")

// ErrHumanActionKind reports an action that cannot be resolved by the requested
// human workflow.
type ErrHumanActionKind struct {
	ActionID int64
	Kind     string
}

func (e *ErrHumanActionKind) Error() string {
	return fmt.Sprintf("human action %d has unsupported kind %q", e.ActionID, e.Kind)
}

// ErrCostExceeded means reserving a paid attempt would cross the job's
// explicit maximum. The reservation is atomic across daemon workers/restarts.
type ErrCostExceeded struct {
	JobID, Source    string
	Spent, Cost, Max float64
}

func (e *ErrCostExceeded) Error() string {
	return fmt.Sprintf("job %s cost limit exceeded for %s: $%.2f + $%.2f > $%.2f",
		e.JobID, e.Source, e.Spent, e.Cost, e.Max)
}

// Policy is the per-job policy snapshot stored in jobs.policy_json.
type Policy struct {
	AccessMode     string   `json:"access_mode"`
	DesiredVersion string   `json:"desired_version"`
	Resolver       string   `json:"resolver,omitempty"`
	MaxCostUSD     *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow   []string `json:"sources_allow,omitempty"`
	SourcesDeny    []string `json:"sources_deny,omitempty"`
	FetchMaxBytes  int64    `json:"fetch_max_bytes"`
	AutoImport     bool     `json:"auto_import,omitempty"`
	Collection     string   `json:"collection,omitempty"`
}

// SourceAllowed applies the allow/deny lists (deny wins; empty allow = all).
func (p Policy) SourceAllowed(name string) bool {
	for _, d := range p.SourcesDeny {
		if d == name {
			return false
		}
	}
	if len(p.SourcesAllow) == 0 {
		return true
	}
	for _, a := range p.SourcesAllow {
		if a == name {
			return true
		}
	}
	return false
}

// Row is one job with its request context.
type Row struct {
	ID                  string    `json:"id"`
	WorkRequestID       string    `json:"work_request_id"`
	State               string    `json:"state"`
	Policy              Policy    `json:"policy"`
	ArtifactSHA256      string    `json:"artifact_sha256,omitempty"`
	SelectedCandidateID int64     `json:"selected_candidate_id,omitempty"`
	SpentUSD            float64   `json:"spent_usd"`
	TerminalReason      string    `json:"terminal_reason,omitempty"`
	RetryAt             string    `json:"retry_at,omitempty"`
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
	Work                work.Work `json:"work"`
	ZotioItemKey        string    `json:"zotio_item_key,omitempty"`
}

// Candidate is one ranked acquisition option. URL is never stored; only the
// redacted form persists, so a crash discards bearer URLs by construction.
type Candidate struct {
	ID                 int64   `json:"id"`
	JobID              string  `json:"job_id"`
	Source             string  `json:"source"`
	URLRedacted        string  `json:"url_redacted"`
	URLKey             string  `json:"url_key"`
	LandingRedacted    string  `json:"landing_redacted,omitempty"`
	Version            string  `json:"version"`
	AccessBasis        string  `json:"access_basis"`
	ReuseLicense       string  `json:"reuse_license"`
	ExpectedMIME       string  `json:"expected_mime,omitempty"`
	CostUSD            float64 `json:"cost_usd"`
	Direct             bool    `json:"direct"`
	IdentityConfidence float64 `json:"identity_confidence"`
	RankEvidence       string  `json:"rank_evidence,omitempty"`
	Rank               int     `json:"rank"`
	Status             string  `json:"status"`
	ReviewOverride     bool    `json:"review_override"`
}

// Store layers job semantics over the shared SQLite store.
type Store struct{ S *store.Store }

// NewID returns a 26-hex-char random identifier with a type prefix.
func NewID(prefix string) string {
	var b [13]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// CreateRequest inserts a work request, its identifiers, and a queued job in
// one transaction. Resubmitting the same requestID returns the existing live
// job (idempotent submission).
func (js *Store) CreateRequest(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string) (string, error) {
	if requestID == "" {
		requestID = NewID("wr")
	}
	db := js.S.DB()

	// Idempotent resubmission: return the live job for this request if any.
	var existing string
	err := db.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE work_request_id = ? AND state NOT IN ('failed','cancelled','unavailable') ORDER BY created_at DESC LIMIT 1`,
		requestID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	polJSON, err := json.Marshal(pol)
	if err != nil {
		return "", err
	}
	authorsJSON, _ := json.Marshal(w.Authors)
	now := store.Now()
	jobID := NewID("job")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_requests (id, created_at, requester, zotio_item_key, collection_key, title, authors_json, year, desired_version, max_cost_usd)
		 VALUES (?, ?, 'cli', ?, ?, ?, ?, ?, ?, ?)`,
		requestID, now, nullable(zotioKey), nullable(collection), nullable(w.Title), string(authorsJSON), nullableInt(w.Year), pol.DesiredVersion, pol.MaxCostUSD); err != nil {
		return "", fmt.Errorf("inserting work request: %w", err)
	}
	for kind, value := range map[string]string{"doi": w.DOI, "pmid": w.PMID, "arxiv": w.ArXiv, "isbn": w.ISBN, "openalex": w.OpenAlex} {
		if value == "" {
			continue
		}
		raw := rawIDs[kind]
		if raw == "" {
			raw = value
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO identifiers (work_request_id, kind, value, raw) VALUES (?, ?, ?, ?)`,
			requestID, kind, value, raw); err != nil {
			return "", fmt.Errorf("inserting identifier %s: %w", kind, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?, ?)`,
		jobID, requestID, string(polJSON), now, now); err != nil {
		return "", fmt.Errorf("inserting job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.created', ?)`,
		jobID, now, fmt.Sprintf(`{"request_id":%q,"work":%q}`, requestID, w.Describe())); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return jobID, nil
}

// FillWorkMetadata fills fields absent from the original request using
// resolver-observed metadata. Request values remain authoritative; conflicting
// identifiers fail closed rather than silently changing the requested work.
func (js *Store) FillWorkMetadata(ctx context.Context, jobID string, discovered work.Work) (*Row, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var requestID string
	var title, authorsJSON sql.NullString
	var year sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT w.id, w.title, w.authors_json, w.year
		FROM jobs j JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(&requestID, &title, &authorsJSON, &year); err != nil {
		return nil, err
	}
	var authors []string
	if authorsJSON.Valid && authorsJSON.String != "" {
		if err := json.Unmarshal([]byte(authorsJSON.String), &authors); err != nil {
			return nil, fmt.Errorf("request %s authors: %w", requestID, err)
		}
	}
	if !title.Valid || title.String == "" {
		title.String, title.Valid = discovered.Title, discovered.Title != ""
	}
	if len(authors) == 0 && len(discovered.Authors) > 0 {
		authors = append([]string(nil), discovered.Authors...)
	}
	if !year.Valid || year.Int64 == 0 {
		year.Int64, year.Valid = int64(discovered.Year), discovered.Year != 0
	}
	encodedAuthors, err := json.Marshal(authors)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_requests SET title = ?, authors_json = ?, year = ? WHERE id = ?`,
		nullable(title.String), string(encodedAuthors), nullableInt(int(year.Int64)), requestID); err != nil {
		return nil, err
	}

	observed := map[string]string{
		"doi": discovered.DOI, "pmid": discovered.PMID, "arxiv": discovered.ArXiv,
		"isbn": discovered.ISBN, "openalex": discovered.OpenAlex,
	}
	for kind, value := range observed {
		if value == "" {
			continue
		}
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT value FROM identifiers WHERE work_request_id = ? AND kind = ?`, requestID, kind).Scan(&existing)
		switch {
		case err == nil && existing != value:
			return nil, fmt.Errorf("resolver metadata conflicts with requested %s: %q != %q", kind, value, existing)
		case err == nil:
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identifiers(work_request_id, kind, value, raw) VALUES(?, ?, ?, ?)`,
			requestID, kind, value, value); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.metadata_enriched', ?)`,
		jobID, store.Now(), `{"source":"resolver","policy":"fill_missing_only"}`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return js.Get(ctx, jobID)
}

// ReserveCost atomically charges one paid source attempt to a job. A nil limit
// tracks spend without imposing a ceiling. Zero-cost calls are not recorded.
func (js *Store) ReserveCost(ctx context.Context, jobID, source string, cost float64, limit *float64) error {
	if cost < 0 {
		return fmt.Errorf("negative job cost %.4f", cost)
	}
	if cost == 0 {
		return nil
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var spent float64
	if err := tx.QueryRowContext(ctx, `SELECT spent_usd FROM jobs WHERE id = ?`, jobID).Scan(&spent); err != nil {
		return err
	}
	if limit != nil && spent+cost > *limit+1e-9 {
		return &ErrCostExceeded{JobID: jobID, Source: source, Spent: spent, Cost: cost, Max: *limit}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET spent_usd = spent_usd + ?, updated_at = ? WHERE id = ?`,
		cost, store.Now(), jobID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"source": source, "cost_usd": cost})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.cost_reserved', ?)`,
		jobID, store.Now(), string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseReservedCost reverses a reservation when the paid source call did not
// start (for example, its durable monthly budget closed between checks).
func (js *Store) ReleaseReservedCost(ctx context.Context, jobID, source string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		`UPDATE jobs SET spent_usd = spent_usd - ?, updated_at = ?
		 WHERE id = ? AND spent_usd + 1e-9 >= ?`,
		cost, store.Now(), jobID, cost)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("cannot release unreserved job cost %.4f for %s", cost, jobID)
	}
	detail, _ := json.Marshal(map[string]any{"source": source, "cost_usd": cost})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.cost_released', ?)`,
		jobID, store.Now(), string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// Transition CAS-moves a job from -> to, appending an event in the same
// transaction. detail must be pre-redacted. retryAt applies to retry_wait;
// terminalReason applies to terminal states. Reaching any terminal state
// closes the job's open human actions ('resolved' on ready, 'cancelled'
// otherwise) so a finished job never strands an open action.
func (js *Store) Transition(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...TransitionOpt) error {
	return js.transition(ctx, jobID, from, to, detail, opts...)
}

func (js *Store) transition(ctx context.Context, jobID, from, to string, detail map[string]any, opts ...TransitionOpt) error {
	if !allowed[from][to] {
		return fmt.Errorf("%w: %s -> %s not allowed", ErrConflict, from, to)
	}
	var cfg transitionCfg
	for _, o := range opts {
		o(&cfg)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from"], detail["to"] = from, to
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	now := store.Now()
	// A parked or terminal job is owned by nobody: release the lease so the
	// scheduler can re-claim it when it becomes runnable again.
	releaseLease := Terminal(to) || to == StateRetryWait || to == StateAwaitingHuman || to == StateNeedsReview

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ?,
		        retry_at = ?,
		        terminal_reason = COALESCE(?, terminal_reason),
		        artifact_sha256 = COALESCE(?, artifact_sha256),
		        selected_candidate_id = COALESCE(?, selected_candidate_id),
		        lease_owner = CASE WHEN ? THEN NULL ELSE lease_owner END,
		        lease_expires_at = CASE WHEN ? THEN NULL ELSE lease_expires_at END
		 WHERE id = ? AND state = ?`,
		to, now, nullable(cfg.retryAt), nullable(cfg.terminalReason), nullable(cfg.artifactSHA), cfg.candidateID,
		releaseLease, releaseLease, jobID, from)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("%w: job %s not in state %s", ErrConflict, jobID, from)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		jobID, now, string(detailJSON)); err != nil {
		return err
	}
	if Terminal(to) {
		actionStatus := "cancelled"
		if to == StateReady {
			actionStatus = "resolved"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE human_actions SET status = ?, resolved_at = ?
			 WHERE job_id = ? AND status = 'open' AND kind != ?`,
			actionStatus, now, jobID, informationalActionKind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type transitionCfg struct {
	retryAt        string
	terminalReason string
	artifactSHA    string
	candidateID    any
}

// TransitionOpt customizes a transition.
type TransitionOpt func(*transitionCfg)

// WithRetryAt schedules the next attempt for a retry_wait transition.
func WithRetryAt(t time.Time) TransitionOpt {
	return func(c *transitionCfg) { c.retryAt = t.UTC().Format(time.RFC3339Nano) }
}

// WithTerminalReason records why a job ended.
func WithTerminalReason(reason string) TransitionOpt {
	return func(c *transitionCfg) { c.terminalReason = reason }
}

// WithArtifact links the accepted artifact.
func WithArtifact(sha string) TransitionOpt {
	return func(c *transitionCfg) { c.artifactSHA = sha }
}

// WithCandidate records the selected candidate.
func WithCandidate(id int64) TransitionOpt {
	return func(c *transitionCfg) { c.candidateID = id }
}

// ClaimNext leases the oldest runnable job: queued always; retry_wait when
// due. Mid-flight stages are claimable when unowned (the durable result of
// RecoverStale) or when their prior lease expired.
func (js *Store) ClaimNext(ctx context.Context, owner string, lease time.Duration) (*Row, error) {
	now := store.Now()
	expires := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
	db := js.S.DB()

	var id string
	err := db.QueryRowContext(ctx, `
		SELECT id FROM jobs
		WHERE (
		        (state = 'queued' AND (lease_owner IS NULL OR lease_expires_at < ?))
		     OR (state = 'retry_wait' AND retry_at <= ? AND (lease_owner IS NULL OR lease_expires_at < ?))
		     OR (state IN ('resolving','fetching','validating') AND (lease_owner IS NULL OR lease_expires_at < ?))
		      )
		ORDER BY created_at ASC LIMIT 1`, now, now, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE jobs SET lease_owner = ?, lease_expires_at = ? WHERE id = ? AND (lease_owner IS NULL OR lease_expires_at < ?)`,
		owner, expires, id, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil // lost the race; caller loops
	}
	return js.Get(ctx, id)
}

// Heartbeat extends a held lease.
func (js *Store) Heartbeat(ctx context.Context, jobID, owner string, lease time.Duration) error {
	expires := time.Now().UTC().Add(lease).Format(time.RFC3339Nano)
	res, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = ? WHERE id = ? AND lease_owner = ?`, expires, jobID, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: lease on %s not held by %s", ErrConflict, jobID, owner)
	}
	return nil
}

// Release drops a lease without changing state (job becomes claimable).
func (js *Store) Release(ctx context.Context, jobID, owner string) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = ? AND lease_owner = ?`, jobID, owner)
	return err
}

// Cancel moves a nonterminal job to cancelled. Repeated cancellation and
// cancellation after any terminal result are idempotent no-ops.
func (js *Store) Cancel(ctx context.Context, jobID, reason string) error {
	for {
		row, err := js.Get(ctx, jobID)
		if err != nil {
			return err
		}
		if Terminal(row.State) {
			return nil
		}
		err = js.transition(ctx, jobID, row.State, StateCancelled,
			map[string]any{"reason": reason}, WithTerminalReason(reason))
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err
	}
}

// Retry explicitly reopens a retry-wait, failed, or unavailable job at the
// durable resolving boundary. Ready, cancelled, active, and human-parked jobs
// require their dedicated command instead of silently changing meaning.
func (js *Store) Retry(ctx context.Context, jobID string) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&from); err != nil {
		return err
	}
	switch from {
	case StateRetryWait, StateFailed, StateUnavailable:
	default:
		return fmt.Errorf("%w: %s cannot be retried", ErrConflict, from)
	}
	now := store.Now()
	result, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'resolving', updated_at = ?, lease_owner = NULL,
		        lease_expires_at = NULL, retry_at = NULL, terminal_reason = NULL,
		        selected_candidate_id = NULL, artifact_sha256 = NULL
		  WHERE id = ? AND state = ?`, now, jobID, from)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE candidates SET status = 'pending'
		  WHERE job_id = ? AND status IN ('fetching','retryable')`, jobID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"from": from, "to": StateResolving, "reason": "explicit_retry"})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.retry_requested', ?)`,
		jobID, now, string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverStale rewinds expired mid-flight jobs to resolving: bearer URLs and
// quarantine temp files are per-attempt, so the durable boundary is the
// candidate set, which re-ranks deterministically. Content addressing makes
// re-fetches duplicate-free. Returns the rewound job IDs.
func (js *Store) RecoverStale(ctx context.Context) ([]string, error) {
	now := store.Now()
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT id, state FROM jobs WHERE state IN ('resolving','fetching','validating') AND (lease_expires_at IS NULL OR lease_expires_at < ?)`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type stale struct{ id, state string }
	var found []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.state); err != nil {
			_ = rows.Close()
			return nil, err
		}
		found = append(found, s)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var recovered []string
	for _, s := range found {
		if s.state == StateResolving {
			// Already at the durable boundary; clear only a still-stale lease.
			result, err := js.S.DB().ExecContext(ctx,
				`UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL
				 WHERE id = ? AND state = 'resolving'
				   AND (lease_expires_at IS NULL OR lease_expires_at < ?)`, s.id, now)
			if err != nil {
				return recovered, err
			}
			if changed, _ := result.RowsAffected(); changed == 1 {
				recovered = append(recovered, s.id)
			}
			continue
		}
		if err := js.Transition(ctx, s.id, s.state, StateResolving, map[string]any{"reason": "crash_recovery"}); err != nil && !errors.Is(err, ErrConflict) {
			return recovered, err
		}
		recovered = append(recovered, s.id)
	}
	return recovered, nil
}

// informationalActionKind marks the one advisory action that legitimately
// stays open on a terminal job: conservative mode records that an
// institutional OpenURL exists without opening it, and that trace must
// survive both the terminal transition and the startup sweep.
const informationalActionKind = "openurl_available"

// CloseStaleHumanActions cancels open non-advisory actions for jobs that have
// already reached a terminal state. It repairs rows left by older daemon
// versions.
func (js *Store) CloseStaleHumanActions(ctx context.Context) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = 'cancelled', resolved_at = ?
		 WHERE status = 'open'
		   AND kind != ?
		   AND EXISTS (
		       SELECT 1 FROM jobs
		       WHERE jobs.id = human_actions.job_id
		         AND jobs.state IN ('ready', 'unavailable', 'failed', 'cancelled')
		   )`, store.Now(), informationalActionKind)
	return err
}

// SweepTerminalQuarantine removes abandoned per-job download files only after
// their jobs become terminal. Human-review states deliberately retain their
// quarantine directory because action details point users to those files.
func (js *Store) SweepTerminalQuarantine(ctx context.Context) error {
	if js == nil || js.S == nil {
		return errors.New("job store is not initialized")
	}
	artifacts, err := artifact.New(filepath.Dir(js.S.Path()))
	if err != nil {
		return fmt.Errorf("open artifact layout: %w", err)
	}
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE state IN ('ready', 'unavailable', 'failed', 'cancelled')`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var cleanupErr error
	for _, id := range ids {
		if err := artifacts.CleanQuarantine(id); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clean terminal quarantine for %s: %w", id, err))
		}
	}
	return cleanupErr
}

// Get loads one job row with its work-request identity.
func (js *Store) Get(ctx context.Context, jobID string) (*Row, error) {
	db := js.S.DB()
	var r Row
	var polJSON string
	var artifact, terminal, retryAt sql.NullString
	var selected sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT j.id, j.work_request_id, j.state, j.policy_json, j.artifact_sha256, j.selected_candidate_id,
		       j.spent_usd, j.terminal_reason, j.retry_at, j.created_at, j.updated_at,
		       COALESCE(w.title,''), COALESCE(w.authors_json,'[]'), COALESCE(w.year,0), COALESCE(w.zotio_item_key,'')
		FROM jobs j JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(
		&r.ID, &r.WorkRequestID, &r.State, &polJSON, &artifact, &selected, &r.SpentUSD, &terminal, &retryAt, &r.CreatedAt, &r.UpdatedAt,
		&r.Work.Title, &jsonScanner{&r.Work.Authors}, &r.Work.Year, &r.ZotioItemKey)
	if err != nil {
		return nil, err
	}
	r.ArtifactSHA256, r.SelectedCandidateID, r.TerminalReason, r.RetryAt = artifact.String, selected.Int64, terminal.String, retryAt.String
	if err := json.Unmarshal([]byte(polJSON), &r.Policy); err != nil {
		return nil, fmt.Errorf("job %s policy: %w", jobID, err)
	}
	ids, err := db.QueryContext(ctx, `SELECT kind, value FROM identifiers WHERE work_request_id = ?`, r.WorkRequestID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ids.Close() }()
	for ids.Next() {
		var kind, value string
		if err := ids.Scan(&kind, &value); err != nil {
			_ = ids.Close()
			return nil, err
		}
		switch kind {
		case "doi":
			r.Work.DOI = value
		case "pmid":
			r.Work.PMID = value
		case "arxiv":
			r.Work.ArXiv = value
		case "isbn":
			r.Work.ISBN = value
		case "openalex":
			r.Work.OpenAlex = value
		}
	}
	if err := ids.Close(); err != nil {
		return nil, err
	}
	if err := ids.Err(); err != nil {
		return nil, err
	}
	return &r, nil
}

// jsonScanner scans a JSON array column into a []string.
type jsonScanner struct{ dst *[]string }

func (j *jsonScanner) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		return nil
	case string:
		return json.Unmarshal([]byte(v), j.dst)
	case []byte:
		return json.Unmarshal(v, j.dst)
	default:
		return fmt.Errorf("unexpected authors column type %T", src)
	}
}

// List returns jobs, optionally filtered by state, newest first.
func (js *Store) List(ctx context.Context, state string, limit int) ([]Row, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id FROM jobs`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := js.S.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Row
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		idList = append(idList, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range idList {
		r, err := js.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// InsertCandidates stores ranked candidates (redacted URLs only), deduplicated
// per job by url_key. Returns the number inserted.
func (js *Store) InsertCandidates(ctx context.Context, jobID string, cands []Candidate) (int, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	inserted := 0
	for _, c := range cands {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO candidates
			  (job_id, source, url_redacted, url_key, landing_redacted, version, access_basis, reuse_license,
			   expected_mime, cost_usd, direct, identity_confidence, rank_evidence, rank, status, review_override, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			jobID, c.Source, c.URLRedacted, c.URLKey, nullable(c.LandingRedacted), c.Version, c.AccessBasis,
			c.ReuseLicense, nullable(c.ExpectedMIME), c.CostUSD, boolInt(c.Direct), c.IdentityConfidence,
			nullable(c.RankEvidence), c.Rank, boolInt(c.ReviewOverride), now)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NextPendingCandidate returns the best-ranked candidate still pending, or nil.
func (js *Store) NextPendingCandidate(ctx context.Context, jobID string) (*Candidate, error) {
	row := js.S.DB().QueryRowContext(ctx, `
		SELECT id, job_id, source, url_redacted, url_key, COALESCE(landing_redacted,''), version, access_basis,
		       reuse_license, COALESCE(expected_mime,''), cost_usd, direct, identity_confidence,
		       COALESCE(rank_evidence,''), COALESCE(rank,0), status, review_override
		FROM candidates WHERE job_id = ? AND status = 'pending' ORDER BY rank ASC, id ASC LIMIT 1`, jobID)
	var c Candidate
	var direct, override int
	err := row.Scan(&c.ID, &c.JobID, &c.Source, &c.URLRedacted, &c.URLKey, &c.LandingRedacted, &c.Version,
		&c.AccessBasis, &c.ReuseLicense, &c.ExpectedMIME, &c.CostUSD, &direct, &c.IdentityConfidence,
		&c.RankEvidence, &c.Rank, &c.Status, &override)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Direct, c.ReviewOverride = direct == 1, override == 1
	return &c, nil
}

// MarkCandidate updates one candidate's status.
func (js *Store) MarkCandidate(ctx context.Context, id int64, status string) error {
	_, err := js.S.DB().ExecContext(ctx, `UPDATE candidates SET status = ? WHERE id = ?`, status, id)
	return err
}

// GetCandidate loads one candidate by its durable ID.
func (js *Store) GetCandidate(ctx context.Context, id int64) (*Candidate, error) {
	row := js.S.DB().QueryRowContext(ctx, `
		SELECT id, job_id, source, url_redacted, url_key, COALESCE(landing_redacted,''), version, access_basis,
		       reuse_license, COALESCE(expected_mime,''), cost_usd, direct, identity_confidence,
		       COALESCE(rank_evidence,''), COALESCE(rank,0), status, review_override
		FROM candidates WHERE id = ?`, id)
	var c Candidate
	var direct, override int
	if err := row.Scan(&c.ID, &c.JobID, &c.Source, &c.URLRedacted, &c.URLKey, &c.LandingRedacted, &c.Version,
		&c.AccessBasis, &c.ReuseLicense, &c.ExpectedMIME, &c.CostUSD, &direct, &c.IdentityConfidence,
		&c.RankEvidence, &c.Rank, &c.Status, &override); err != nil {
		return nil, err
	}
	c.Direct, c.ReviewOverride = direct == 1, override == 1
	return &c, nil
}

// ResetCandidates makes interrupted and retryable candidates runnable for a
// fresh resolution pass. Invalid/skipped candidates stay exhausted.
func (js *Store) ResetCandidates(ctx context.Context, jobID string) error {
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE candidates SET status = 'pending' WHERE job_id = ? AND status IN ('fetching','retryable')`, jobID)
	return err
}

// Attempt records one resolve/fetch/validate execution.
func (js *Store) StartAttempt(ctx context.Context, jobID string, candidateID int64, stage, source string) (int64, error) {
	var cand any
	if candidateID > 0 {
		cand = candidateID
	}
	res, err := js.S.DB().ExecContext(ctx,
		`INSERT INTO attempts (job_id, candidate_id, stage, source, started_at) VALUES (?, ?, ?, ?, ?)`,
		jobID, cand, stage, nullable(source), store.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishAttempt closes an attempt with its outcome. detail must be redacted.
func (js *Store) FinishAttempt(ctx context.Context, attemptID int64, outcome string, httpStatus int, detail string) error {
	var status any
	if httpStatus > 0 {
		status = httpStatus
	}
	_, err := js.S.DB().ExecContext(ctx,
		`UPDATE attempts SET ended_at = ?, outcome = ?, http_status = ?, detail = ? WHERE id = ?`,
		store.Now(), outcome, status, nullable(detail), attemptID)
	return err
}

// UpsertArtifact records a validated artifact (content-addressed; idempotent).
type Artifact struct {
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	MIME             string `json:"mime"`
	PageCount        int    `json:"page_count"`
	TextChars        int64  `json:"text_chars"`
	OCRUsed          bool   `json:"ocr_used"`
	Encrypted        bool   `json:"encrypted"`
	HasActiveContent bool   `json:"has_active_content"`
	IdentityResult   string `json:"identity_result,omitempty"`
	Path             string `json:"path"`
	CreatedAt        string `json:"created_at"`
}

// UpsertArtifact inserts the artifact row if new.
func (js *Store) UpsertArtifact(ctx context.Context, a Artifact) error {
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO artifacts (sha256, size_bytes, mime, page_count, text_chars, ocr_used, encrypted, has_active_content, identity_result, path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sha256) DO UPDATE SET identity_result = excluded.identity_result`,
		a.SHA256, a.SizeBytes, a.MIME, a.PageCount, a.TextChars, boolInt(a.OCRUsed), boolInt(a.Encrypted),
		boolInt(a.HasActiveContent), nullable(a.IdentityResult), a.Path, store.Now())
	return err
}

// GetArtifact loads one artifact row by hash.
func (js *Store) GetArtifact(ctx context.Context, sha string) (*Artifact, error) {
	var a Artifact
	var ocr, enc, active int
	var identity sql.NullString
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT sha256, size_bytes, mime, COALESCE(page_count,0), COALESCE(text_chars,0), ocr_used, encrypted,
		       has_active_content, identity_result, path, created_at
		FROM artifacts WHERE sha256 = ?`, sha).Scan(
		&a.SHA256, &a.SizeBytes, &a.MIME, &a.PageCount, &a.TextChars, &ocr, &enc, &active, &identity, &a.Path, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.OCRUsed, a.Encrypted, a.HasActiveContent = ocr == 1, enc == 1, active == 1
	a.IdentityResult = identity.String
	return &a, nil
}

// FindCandidateByArtifact returns the original accepted candidate provenance
// for an artifact, including when the current job completed from local cache.
func (js *Store) FindCandidateByArtifact(ctx context.Context, sha string) (*Candidate, error) {
	var id sql.NullInt64
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT selected_candidate_id FROM jobs
		WHERE artifact_sha256 = ? AND state = 'ready' AND selected_candidate_id IS NOT NULL
		ORDER BY updated_at ASC LIMIT 1`, sha).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return js.GetCandidate(ctx, id.Int64)
}

// FindArtifactByDOI returns a prior validated artifact for the same DOI, if
// any job with that DOI reached ready (resolver order step 1: local cache).
func (js *Store) FindArtifactByDOI(ctx context.Context, doi string) (*Artifact, error) {
	var sha string
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT j.artifact_sha256 FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = 'doi' AND i.value = ? AND j.state = 'ready' AND j.artifact_sha256 IS NOT NULL
		ORDER BY j.updated_at DESC LIMIT 1`, doi).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return js.GetArtifact(ctx, sha)
}

// OpenHumanAction records a required human step for a job. Re-parking the
// same job and action kind refreshes the existing action rather than creating
// another open prompt.
func (js *Store) OpenHumanAction(ctx context.Context, jobID, kind, detail string) (int64, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var actionID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM human_actions
		 WHERE job_id = ? AND kind = ? AND status = 'open'
		 ORDER BY id ASC LIMIT 1`, jobID, kind).Scan(&actionID)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx,
			`UPDATE human_actions SET detail = ? WHERE id = ?`, nullable(detail), actionID); err != nil {
			return 0, err
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO human_actions (job_id, kind, status, detail, created_at) VALUES (?, ?, 'open', ?, ?)`,
			jobID, kind, nullable(detail), store.Now())
		if err != nil {
			return 0, err
		}
		actionID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	default:
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return actionID, nil
}

// ResolveHumanAction closes one open action with a compare-and-swap update.
func (js *Store) ResolveHumanAction(ctx context.Context, actionID int64, status string) error {
	if status != "resolved" && status != "cancelled" {
		return fmt.Errorf("invalid human action status %q", status)
	}
	res, err := js.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = 'open'`,
		status, store.Now(), actionID)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		var exists int
		if err := js.S.DB().QueryRowContext(ctx, `SELECT 1 FROM human_actions WHERE id = ?`, actionID).Scan(&exists); err != nil {
			return err
		}
		return fmt.Errorf("%w: human action %d is not open", ErrConflict, actionID)
	}
	return nil
}

// ResolveReview applies a human accept or reject verdict to a parked identity
// review. It atomically closes the action and moves the job to its next durable
// boundary, leaving no interval in which a closed action still parks a job.
func (js *Store) ResolveReview(ctx context.Context, actionID int64, verdict string) (string, string, error) {
	if verdict != "accept" && verdict != "reject" {
		return "", "", fmt.Errorf("invalid review verdict %q", verdict)
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()

	var action HumanAction
	if err := tx.QueryRowContext(ctx,
		`SELECT id, job_id, kind, status, COALESCE(detail,''), created_at FROM human_actions WHERE id = ?`, actionID).
		Scan(&action.ID, &action.JobID, &action.Kind, &action.Status, &action.Detail, &action.CreatedAt); err != nil {
		return "", "", err
	}
	if action.Kind != "verify_identity" {
		return "", "", &ErrHumanActionKind{ActionID: actionID, Kind: action.Kind}
	}
	if action.Status != "open" {
		return "", "", fmt.Errorf("%w: human action %d is not open", ErrConflict, actionID)
	}
	var from string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, action.JobID).Scan(&from); err != nil {
		return "", "", err
	}
	if from != StateNeedsReview {
		return "", "", fmt.Errorf("%w: job %s is not awaiting identity review", ErrConflict, action.JobID)
	}

	now := store.Now()
	res, err := tx.ExecContext(ctx,
		`UPDATE human_actions SET status = 'resolved', resolved_at = ? WHERE id = ? AND status = 'open'`, now, actionID)
	if err != nil {
		return "", "", err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return "", "", fmt.Errorf("%w: human action %d is not open", ErrConflict, actionID)
	}

	to, reason := StateCancelled, "review_rejected"
	terminalReason := "review_rejected"
	if verdict == "accept" {
		var candidateID sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT candidate_id FROM attempts
			WHERE job_id = ? AND stage = 'validate' AND outcome = 'needs_review'
			ORDER BY id DESC LIMIT 1`, action.JobID).Scan(&candidateID)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err != nil {
			return "", "", err
		}
		if candidateID.Valid {
			res, err := tx.ExecContext(ctx,
				`UPDATE candidates SET review_override = 1, status = 'pending' WHERE id = ? AND job_id = ?`,
				candidateID.Int64, action.JobID)
			if err != nil {
				return "", "", err
			}
			if changed, _ := res.RowsAffected(); changed != 1 {
				return "", "", fmt.Errorf("%w: candidate %d not found for job %s", ErrConflict, candidateID.Int64, action.JobID)
			}
			to = StateFetching
		} else {
			to = StateResolving
		}
		reason = "review_accepted"
		terminalReason = ""
	}
	detail, err := json.Marshal(map[string]any{"from": from, "to": to, "reason": reason})
	if err != nil {
		return "", "", err
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ?, lease_owner = NULL, lease_expires_at = NULL,
		        retry_at = NULL, terminal_reason = ?, selected_candidate_id = NULL, artifact_sha256 = NULL
		WHERE id = ? AND state = ?`,
		to, now, nullable(terminalReason), action.JobID, from)
	if err != nil {
		return "", "", err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return "", "", fmt.Errorf("%w: job %s not in state %s", ErrConflict, action.JobID, from)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, 'job.transition', ?)`,
		action.JobID, now, string(detail)); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return action.JobID, to, nil
}

// HumanAction is one pending or resolved human step.
type HumanAction struct {
	ID        int64  `json:"id"`
	JobID     string `json:"job_id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ListHumanActions returns actions, optionally only open ones.
func (js *Store) ListHumanActions(ctx context.Context, openOnly bool) ([]HumanAction, error) {
	q := `SELECT id, job_id, kind, status, COALESCE(detail,''), created_at FROM human_actions`
	if openOnly {
		q += ` WHERE status = 'open'`
	}
	q += ` ORDER BY id DESC`
	rows, err := js.S.DB().QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HumanAction
	for rows.Next() {
		var h HumanAction
		if err := rows.Scan(&h.ID, &h.JobID, &h.Kind, &h.Status, &h.Detail, &h.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// RecordEvent appends a durable event to a job's ordered event stream.
func (js *Store) RecordEvent(ctx context.Context, jobID, kind string, detail map[string]any) error {
	if jobID == "" || kind == "" {
		return errors.New("job event requires job ID and kind")
	}
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshaling job event: %w", err)
	}
	_, err = js.S.DB().ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, ?, ?)`,
		jobID, store.Now(), kind, string(encoded))
	if err != nil {
		return fmt.Errorf("recording job event: %w", err)
	}
	return nil
}

// Events returns a job's event stream in order.
func (js *Store) Events(ctx context.Context, jobID string) ([]map[string]any, error) {
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT seq, at, kind, detail_json FROM events WHERE job_id = ? ORDER BY seq ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var seq int64
		var at, kind, detail string
		if err := rows.Scan(&seq, &at, &kind, &detail); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(detail), &d)
		out = append(out, map[string]any{"seq": seq, "at": at, "kind": kind, "detail": d})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}
