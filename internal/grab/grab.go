// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Package grab owns the pdf_grabs table (ADR-0020): the durable record of
// one browser PDF grab, from allocation through identification. It never
// touches HTTP or the browser bridge protocol itself — internal/browser
// drives the sweep and the wire; this package only owns the row.
package grab

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"papio/internal/store"
	"strings"
	"sync"
	"time"
)

// State is a pdf_grabs.state value, mirroring the migration's CHECK
// constraint exactly (ADR-0020 Decision 3/4).
type State string

const (
	// StateAwaitingFile is a grab's initial state: the daemon allocated an
	// id and a steering directory, but no settled file has landed there yet.
	StateAwaitingFile State = "awaiting_file"
	// StateQuarantined marks a settled file copied into quarantine, ahead of
	// structural validation and front-matter identifier extraction.
	StateQuarantined State = "quarantined"
	// StateIdentified is reserved for a future finer-grained pipeline; the
	// v1 sweeper moves directly from quarantined to a terminal state (see
	// internal/browser.SweepGrabs) but the vocabulary is stable API surface.
	StateIdentified State = "identified"
	// StateJobCreated is terminal: an ordinary identifier-keyed job now owns
	// this grab, whether freshly created or an already-live/owned job the
	// extracted identifier deduplicated onto (see Outcome for which).
	StateJobCreated State = "job_created"
	// StateParkedNoIdentifier is settled but operator-actionable: no
	// front-matter identifier was found, so the grab row remains the durable
	// triage entity until an operator supplies one.
	StateParkedNoIdentifier State = "parked_no_identifier"
	// StateFailedValidation is terminal: the settled file failed structural
	// validation (not a valid PDF).
	StateFailedValidation State = "failed_validation"
	// StateAbandoned is terminal: the browser download was interrupted before
	// any file could settle.
	StateAbandoned State = "abandoned"
)

func validState(s State) bool {
	switch s {
	case StateAwaitingFile, StateQuarantined, StateIdentified, StateJobCreated, StateParkedNoIdentifier, StateFailedValidation, StateAbandoned:
		return true
	default:
		return false
	}
}

// Grab mirrors one pdf_grabs row.
type Grab struct {
	ID              string
	URLHost         string
	Title           string
	State           State
	EffectRequestID string
	QuarantinePath  string
	JobID           string
	// Outcome is the wire vocabulary internal/browser's poll() reports over
	// pdf_grab_result once terminal: "job_created", "already_owned",
	// "needs_identifier", or "failed_validation". Empty until terminal.
	Outcome    string
	Detail     string
	NotifiedAt string
	CreatedAt  string
	UpdatedAt  string
	// BindProvenance is the JSON audit for an automatic candidate binding,
	// or empty when no automatic decision has been recorded. NULL in the
	// database means "no automatic binding decision recorded" — every row
	// predating the provenance column and any grab not bound via
	// candidate_auto_bind is honestly absent rather than guessed.
	BindProvenance string
}

// Service is internal/grab's store-backed entry point.
type Service struct {
	store *store.Store
	now   func() time.Time
	mu    sync.Mutex
}

// New constructs a Service. now defaults to time.Now when nil.
func New(s *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: s, now: now}
}

// NewID returns a 26-hex-char random grab id, "grab_"-prefixed so it can
// never collide with a job.NewID("job") or job.NewID("wr") value sharing the
// same adoption root namespace.
func NewID() string {
	var b [13]byte
	_, _ = rand.Read(b[:])
	return "grab_" + hex.EncodeToString(b[:])
}

func newPermitID() string {
	var b [13]byte
	_, _ = rand.Read(b[:])
	return "permit_" + hex.EncodeToString(b[:])
}

// ErrBusy reports that the global effect lane is occupied or a legacy
// blocker remains unresolved. Bridge maps it to a structured
// pdf_grab_result unavailable refusal, never a raw error.
var ErrBusy = errors.New("pdf grab busy")

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique")
}

var columns = `id, url_host, title, state, effect_request_id, quarantine_path, job_id, outcome, detail, notified_at, created_at, updated_at, bind_provenance`

type scanner interface {
	Scan(dest ...any) error
}

func scanGrab(row scanner) (*Grab, error) {
	var g Grab
	var state string
	var jobID, notifiedAt, bindProvenance sql.NullString
	if err := row.Scan(
		&g.ID, &g.URLHost, &g.Title, &state, &g.EffectRequestID, &g.QuarantinePath, &jobID, &g.Outcome, &g.Detail, &notifiedAt,
		&g.CreatedAt, &g.UpdatedAt, &bindProvenance,
	); err != nil {
		return nil, err
	}
	g.State = State(state)
	g.JobID = jobID.String
	g.NotifiedAt = notifiedAt.String
	g.BindProvenance = bindProvenance.String
	if !validState(g.State) {
		// Fail closed on a row whose state this binary does not know —
		// hand-edited databases and future-schema rows must never be
		// processed as if they were in a known state.
		return nil, fmt.Errorf("grab %s: unknown state %q", g.ID, state)
	}
	return &g, nil
}

// At most one nonterminal grab may exist for a source host and title; a
// repeated request returns that durable row instead of creating a duplicate.
func (s *Service) Allocate(ctx context.Context, urlHost, title string) (*Grab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.store.DB().QueryRowContext(ctx, `
		SELECT `+columns+` FROM pdf_grabs
		WHERE url_host = ? AND title = ? AND state IN (?, ?, ?, ?)
		ORDER BY created_at ASC LIMIT 1`, urlHost, title,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified), string(StateParkedNoIdentifier))
	if g, err := scanGrab(row); err == nil {
		g.Outcome = "existing"
		return g, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("checking existing pdf grab: %w", err)
	}
	id := NewID()
	now := s.now()
	if _, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs (id, url_host, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, urlHost, title, string(StateAwaitingFile), now, now); err != nil {
		if existing, lookupErr := s.store.DB().QueryContext(ctx, `
			SELECT `+columns+` FROM pdf_grabs
			WHERE url_host = ? AND title = ? AND state IN (?, ?, ?, ?)
			ORDER BY created_at ASC LIMIT 1`,
			urlHost, title, string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified), string(StateParkedNoIdentifier)); lookupErr == nil {
			defer func() { _ = existing.Close() }()
			if existing.Next() {
				g, scanErr := scanGrab(existing)
				if scanErr == nil {
					g.Outcome = "existing"
					return g, nil
				}
			}
		}
		return nil, fmt.Errorf("inserting pdf grab: %w", err)
	}
	return s.Get(ctx, id)
}

// AllocateEffect is the permit-gated allocation used by the browser bridge.
// It runs in one DB transaction under the service mutex. Existing active
// grabs are returned with Outcome "existing" without acquiring occupancy or
// calling prepare. An unresolved legacy blocker or any held/unknown effect
// permit refuses with ErrBusy and no rows/prepare. Otherwise it mints a grab
// id and permit id, inserts pdf_grabs (awaiting_file), effect_permits (held,
// NULL job, attempt 0, holder/domain/kind pdf_grab/slot 0/grab id/lease), and
// a URL-free browser.pdf_grab_started event with NULL job_id in the same
// transaction, calls prepare(grabID) before commit, and returns the row with
// Outcome "steering" only after commit. Any error rolls back DB mutations.
func (s *Service) AllocateEffect(ctx context.Context, urlHost, title string, holderGeneration int64, safetyDomain string, leaseUntil time.Time, prepare func(string) error, requestIDs ...string) (*Grab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	requestID := ""
	if len(requestIDs) > 0 {
		requestID = requestIDs[0]
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Existing active grab is never an authorization by itself. Only the
	// original request identity, holder generation, and still-held permit may
	// replay the steering response after a lost delivery.
	row := tx.QueryRowContext(ctx, `
		SELECT `+columns+` FROM pdf_grabs
		WHERE url_host = ? AND title = ? AND state IN (?, ?, ?, ?)
		ORDER BY created_at ASC LIMIT 1`, urlHost, title,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified), string(StateParkedNoIdentifier))
	if g, err := scanGrab(row); err == nil {
		if requestID != "" && g.EffectRequestID == requestID {
			var status string
			var generation int64
			if permitErr := tx.QueryRowContext(ctx, `SELECT status, browser_holder_generation FROM effect_permits WHERE grab_id=? AND effect_kind='pdf_grab'`, g.ID).Scan(&status, &generation); permitErr == nil &&
				status == "held" && generation == holderGeneration {
				_ = tx.Rollback()
				g.Outcome = "steering"
				return g, nil
			}
		}
		_ = tx.Rollback()
		g.Outcome = "existing"
		return g, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("checking existing pdf grab: %w", err)
	}

	// Unresolved legacy blocker → busy.
	var blocker int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM legacy_effect_blockers WHERE status='unresolved' LIMIT 1`).Scan(&blocker); err == nil {
		return nil, ErrBusy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Any held/unknown effect permit → busy (global single slot).
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM effect_permits WHERE status IN ('held','unknown_completion') LIMIT 1`).Scan(&live); err == nil {
		return nil, ErrBusy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	grabID := NewID()
	permitID := newPermitID()
	nowStr := store.Now()
	leaseStr := leaseUntil.UTC().Format(time.RFC3339Nano)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pdf_grabs (id, url_host, title, effect_request_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		grabID, urlHost, title, requestID, string(StateAwaitingFile), nowStr, nowStr); err != nil {
		if isUniqueConstraintError(err) {
			// Race on active source unique index.
			row2 := tx.QueryRowContext(ctx, `
				SELECT `+columns+` FROM pdf_grabs
				WHERE url_host = ? AND title = ? AND state IN (?, ?, ?, ?)
				ORDER BY created_at ASC LIMIT 1`, urlHost, title,
				string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified), string(StateParkedNoIdentifier))
			if g2, err2 := scanGrab(row2); err2 == nil {
				_ = tx.Rollback()
				g2.Outcome = "existing"
				return g2, nil
			}
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("inserting pdf grab: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO effect_permits(id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind, slot_index, grab_id, status, lease_until, created_at, updated_at)
		VALUES(?, NULL, 0, ?, ?, 'pdf_grab', 0, ?, 'held', ?, ?, ?)`,
		permitID, holderGeneration, safetyDomain, grabID, leaseStr, nowStr, nowStr); err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("inserting effect permit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id, at, kind, detail_json) VALUES(NULL, ?, 'browser.pdf_grab_started', ?)`, nowStr, "{}"); err != nil {
		return nil, err
	}
	if prepare != nil {
		if err := prepare(grabID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	g, err := s.Get(ctx, grabID)
	if err != nil {
		return nil, err
	}
	if g != nil {
		g.Outcome = "steering"
	}
	return g, nil
}

// Get returns the row by id, or (nil, nil) when none exists.
func (s *Service) Get(ctx context.Context, id string) (*Grab, error) {
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+columns+` FROM pdf_grabs WHERE id = ?`, id)
	g, err := scanGrab(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// settleLegacyPDFBlockerTx settles only the legacy blocker whose exact grab ID
// matches the transitioned row. Missing rows and already-settled tombstones are
// intentionally harmless: post-0034 rows have no legacy blocker, while a
// legacy row may have been settled by an earlier exact transition. Any
// unexpected multi-row match fails closed rather than broadening settlement.
func settleLegacyPDFBlockerTx(ctx context.Context, tx *sql.Tx, id, now string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE legacy_effect_blockers
		SET status='settled', updated_at=?
		WHERE effect_kind='pdf_grab' AND grab_id=? AND status='unresolved'`,
		now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 1 {
		return fmt.Errorf("legacy PDF blocker identity for grab %q is ambiguous", id)
	}
	return nil
}

// MarkQuarantined transitions awaiting_file -> quarantined and records the
// quarantine copy's path. Settles a matching held/unknown pdf permit and the
// exact legacy blocker in the same transaction; either may be absent.
func (s *Service) MarkQuarantined(ctx context.Context, id, quarantinePath string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, quarantine_path = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(StateQuarantined), quarantinePath, now, id, string(StateAwaitingFile))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkJobCreated transitions to the terminal job_created state. outcome is
// the wire value to report later ("job_created" for a freshly created job,
// "already_owned" when the extracted identifier deduplicated onto an
// already-live job — ADR-0010's ledger dedupe applying naturally).
func (s *Service) MarkJobCreated(ctx context.Context, id, jobID, outcome string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = ?, outcome = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`,
		string(StateJobCreated), jobID, outcome, now, id,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// BindProvenance records why an automatic candidate binding was made, so a
// human reading the ledger later can reconstruct the decision.
type BindProvenance struct {
	Method               string   `json:"method"`
	Rule                 string   `json:"rule"`
	Winner               string   `json:"winner"`
	CandidatesConsidered int      `json:"candidates_considered"`
	Evidence             []string `json:"evidence,omitempty"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// MarkBoundToJob binds a settled grab to an existing job and records the
// provenance of that decision in the same transaction as the state change.
//
// The binding method is never encoded in the wire outcome — outcome remains
// the caller-supplied value (e.g. "job_created") so the extension's closed
// outcome vocabulary is unchanged. The method lives only in the provenance
// column, which is the durable audit trail for automatic decisions.
func (s *Service) MarkBoundToJob(ctx context.Context, id, jobID, outcome string, prov BindProvenance) error {
	return s.markBoundToJob(ctx, id, jobID, outcome, prov, nil)
}

// MarkBoundToJobFenced is MarkBoundToJob with a serialization fence.
//
// The fence runs INSIDE the same transaction that performs the CAS, before
// the row is updated. A recompute outside the serialization point fences
// nothing: another writer can change eligibility between the decision and the
// CAS, so the check and the commit must be atomic. Any non-nil error from
// fence aborts the transaction and rolls back without touching the row.
func (s *Service) MarkBoundToJobFenced(ctx context.Context, id, jobID, outcome string, prov BindProvenance, fence func(ctx context.Context, tx *sql.Tx) error) error {
	return s.markBoundToJob(ctx, id, jobID, outcome, prov, fence)
}

func (s *Service) markBoundToJob(ctx context.Context, id, jobID, outcome string, prov BindProvenance, fence func(ctx context.Context, tx *sql.Tx) error) error {
	if strings.TrimSpace(jobID) == "" {
		return fmt.Errorf("grab %s: job id is required", id)
	}
	if strings.TrimSpace(prov.Method) == "" {
		return fmt.Errorf("grab %s: binding method is required", id)
	}
	if strings.TrimSpace(prov.Rule) == "" {
		return fmt.Errorf("grab %s: binding rule is required", id)
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if fence != nil {
		if err := fence(ctx, tx); err != nil {
			return err
		}
	}
	now := store.Now()
	var provVal any
	// An empty/zero provenance stores SQL NULL rather than "{}" or a
	// half-populated literal — NULL means "no automatic binding decision
	// recorded", which must remain distinguishable from a real decision.
	// Validation above already rejected blank Method/Rule, so a zero value
	// here genuinely means "caller supplied no provenance".
	if prov.Method != "" || prov.Rule != "" || prov.Winner != "" || prov.CandidatesConsidered != 0 || len(prov.Evidence) != 0 {
		b, err := json.Marshal(prov)
		if err != nil {
			return err
		}
		provVal = nullable(string(b))
	} else {
		provVal = nil
	}
	// Mirror MarkJobCreated exactly: same CAS state set, requireOneRow,
	// effect_permits settle, settleLegacyPDFBlockerTx, then Commit.
	// The state written is still job_created and the outward outcome is
	// still caller-supplied — the binding method lives only in provenance.
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = ?, outcome = ?, bind_provenance = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`,
		string(StateJobCreated), jobID, outcome, provVal, now, id,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkIdentified records that identifier extraction completed while retaining
// the grab as a durable entity until the canonical job is created.
func (s *Service) MarkIdentified(ctx context.Context, id string) error {
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`,
		string(StateIdentified), store.Now(), id, string(StateQuarantined), string(StateAwaitingFile), string(StateParkedNoIdentifier))
	if err != nil {
		return err
	}
	return requireOneRow(res, id)
}

// MarkParkedNoIdentifier settles the captured file as parked_no_identifier.
// No synthetic title-only job is created; the grab is the durable pending
// triage entity and its wire outcome is fixed at "needs_identifier".
func (s *Service) MarkParkedNoIdentifier(ctx context.Context, id string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = NULL, outcome = 'needs_identifier', updated_at = ?
		WHERE id = ? AND state IN (?, ?, ?)`,
		string(StateParkedNoIdentifier), now, id,
		string(StateAwaitingFile), string(StateQuarantined), string(StateIdentified))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkFailedValidation transitions to the terminal failed_validation state:
// the correlated file arrived but was not a valid PDF. That observation is
// conclusive for the exact grab, so settle its permit and legacy blocker in
// the same transaction even when validation failed before quarantine.
func (s *Service) MarkFailedValidation(ctx context.Context, id, detail string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, outcome = 'failed_validation', detail = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		string(StateFailedValidation), detail, now, id, string(StateAwaitingFile), string(StateQuarantined))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkAbandoned settles an unfulfilled browser download after interruption.
// Settles a matching held/unknown pdf permit and exact legacy blocker in the
// same transaction; either may be absent for post-34 rows.
func (s *Service) MarkAbandoned(ctx context.Context, id, detail string) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, outcome = 'abandoned', detail = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(StateAbandoned), detail, now, id, string(StateAwaitingFile))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkAbandonedForRequest is the holder/request-fenced interruption path.
// A grab ID alone is deliberately insufficient to release occupancy. The
// persisted request identity remains sufficient for exact cleanup after a
// holder replacement; this path never grants steering.
func (s *Service) MarkAbandonedForRequest(ctx context.Context, id, requestID string, holderGeneration int64, detail string) error {
	_ = holderGeneration // retained in the API as the originating tuple component
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("grab %s: request identity is required", id)
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, outcome = 'abandoned', detail = ?, updated_at = ?
		WHERE id = ? AND effect_request_id = ? AND state = ?
		  AND EXISTS (SELECT 1 FROM effect_permits
		    WHERE grab_id=pdf_grabs.id AND effect_kind='pdf_grab'
		      AND status IN ('held','unknown_completion'))`,
		string(StateAbandoned), detail, now, id, requestID, string(StateAwaitingFile))
	if err != nil {
		return err
	}
	if err := requireOneRow(res, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE grab_id=? AND effect_kind='pdf_grab' AND status IN ('held','unknown_completion')`, now, id); err != nil {
		return err
	}
	if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AbandonStaleAwaiting settles captures that lost their browser correlation,
// except when an unresolved durable effect still makes completion unknown.
func (s *Service) AbandonStaleAwaiting(ctx context.Context, cutoff time.Time) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM pdf_grabs
		WHERE state = ? AND updated_at < ?
		  AND NOT EXISTS (
		    SELECT 1 FROM effect_permits
		    WHERE effect_permits.grab_id = pdf_grabs.id
		      AND status IN ('held','unknown_completion'))
		  AND NOT EXISTS (
		    SELECT 1 FROM legacy_effect_blockers
		    WHERE legacy_effect_blockers.effect_kind = 'pdf_grab'
		      AND legacy_effect_blockers.grab_id = pdf_grabs.id
		      AND legacy_effect_blockers.status = 'unresolved')
		ORDER BY id`,
		string(StateAwaitingFile), cutoffStr)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := store.Now()
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `
			UPDATE pdf_grabs
			SET state = ?, outcome = 'abandoned',
			    detail = 'The PDF grab download expired', updated_at = ?
			WHERE id = ? AND state = ?`,
			string(StateAbandoned), now, id, string(StateAwaitingFile))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if n > 1 {
			return fmt.Errorf("stale PDF grab update for %q changed %d rows", id, n)
		}
		if err := settleLegacyPDFBlockerTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingNotifications returns up to limit grabs whose terminal outcome has
// not yet been pushed to the extension (poll()'s notified_at bookkeeping),
// oldest first — durable, crash-safe queueing: a daemon restart loses no
// pending notification the way an in-memory-only queue would.
func (s *Service) PendingNotifications(ctx context.Context, limit int) ([]*Grab, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT `+columns+` FROM pdf_grabs
		WHERE outcome != '' AND notified_at IS NULL
		ORDER BY updated_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Grab
	for rows.Next() {
		g, err := scanGrab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarkNotified records that a terminal grab's outcome was pushed at least
// once. Best-effort, at-most-once from the extension's point of view: a
// push the extension never received (tab closed, daemon restarted mid-Sync)
// is not retried — the grab's durable disposition already lives in this row
// and, for job_created/already_owned, in the job itself.
func (s *Service) MarkNotified(ctx context.Context, id string) error {
	_, err := s.store.DB().ExecContext(ctx, `UPDATE pdf_grabs SET notified_at = ? WHERE id = ?`, store.Now(), id)
	return err
}

// Delete removes a grab row outright when its triage item is dismissed. It
// cancels nothing else because a parked grab has no job.
func (s *Service) Delete(ctx context.Context, id string) error {
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM pdf_grabs WHERE id = ?`, id)
	return err
}

// ByJobID returns the grab row bound to jobID, or (nil, nil) when none
// exists (an ordinary job never grabbed).
func (s *Service) ByJobID(ctx context.Context, jobID string) (*Grab, error) {
	if jobID == "" {
		return nil, nil
	}
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+columns+` FROM pdf_grabs WHERE job_id = ? ORDER BY created_at DESC LIMIT 1`, jobID)
	g, err := scanGrab(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

func requireOneRow(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("grab: %s changed underneath its own transition (or does not exist)", id)
	}
	return nil
}
