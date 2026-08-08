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
	"errors"
	"fmt"
	"time"

	"papio/internal/store"
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
	// StateParkedNoIdentifier is terminal: no front-matter identifier was
	// found; the grab parked a human action instead (ADR-0019's title-only
	// stance — never an identifier-less submission).
	StateParkedNoIdentifier State = "parked_no_identifier"
	// StateFailedValidation is terminal: the settled file failed structural
	// validation (not a valid PDF).
	StateFailedValidation State = "failed_validation"
)

func validState(s State) bool {
	switch s {
	case StateAwaitingFile, StateQuarantined, StateIdentified, StateJobCreated, StateParkedNoIdentifier, StateFailedValidation:
		return true
	default:
		return false
	}
}

// Grab mirrors one pdf_grabs row.
type Grab struct {
	ID             string
	URLHost        string
	Title          string
	State          State
	QuarantinePath string
	JobID          string
	// Outcome is the wire vocabulary internal/browser's poll() reports over
	// pdf_grab_result once terminal: "job_created", "already_owned",
	// "needs_identifier", or "failed_validation". Empty until terminal.
	Outcome    string
	Detail     string
	NotifiedAt string
	CreatedAt  string
	UpdatedAt  string
}

// Service is internal/grab's store-backed entry point.
type Service struct {
	store *store.Store
	now   func() time.Time
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

var columns = `id, url_host, title, state, quarantine_path, job_id, outcome, detail, notified_at, created_at, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanGrab(row scanner) (*Grab, error) {
	var g Grab
	var state string
	var jobID, notifiedAt sql.NullString
	if err := row.Scan(
		&g.ID, &g.URLHost, &g.Title, &state, &g.QuarantinePath, &jobID, &g.Outcome, &g.Detail, &notifiedAt,
		&g.CreatedAt, &g.UpdatedAt,
	); err != nil {
		return nil, err
	}
	g.State = State(state)
	if !validState(g.State) {
		// Fail closed on a row whose state this binary does not know —
		// hand-edited databases and future-schema rows must never be
		// processed as if they were in a known state.
		return nil, fmt.Errorf("grab %s: unknown state %q", g.ID, state)
	}
	g.JobID = jobID.String
	g.NotifiedAt = notifiedAt.String
	return &g, nil
}

// Allocate inserts a new grab row in StateAwaitingFile and returns it.
func (s *Service) Allocate(ctx context.Context, urlHost, title string) (*Grab, error) {
	id := NewID()
	now := store.Now()
	if _, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs (id, url_host, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, urlHost, title, string(StateAwaitingFile), now, now); err != nil {
		return nil, fmt.Errorf("inserting pdf grab: %w", err)
	}
	return s.Get(ctx, id)
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

// MarkQuarantined transitions awaiting_file -> quarantined and records the
// quarantine copy's path.
func (s *Service) MarkQuarantined(ctx context.Context, id, quarantinePath string) error {
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, quarantine_path = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(StateQuarantined), quarantinePath, store.Now(), id, string(StateAwaitingFile))
	if err != nil {
		return err
	}
	return requireOneRow(res, id)
}

// MarkJobCreated transitions to the terminal job_created state. outcome is
// the wire value to report later ("job_created" for a freshly created job,
// "already_owned" when the extracted identifier deduplicated onto an
// already-live job — ADR-0010's ledger dedupe applying naturally).
func (s *Service) MarkJobCreated(ctx context.Context, id, jobID, outcome string) error {
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = ?, outcome = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		string(StateJobCreated), jobID, outcome, store.Now(), id, string(StateAwaitingFile), string(StateQuarantined))
	if err != nil {
		return err
	}
	return requireOneRow(res, id)
}

// MarkParkedNoIdentifier transitions to the terminal parked_no_identifier
// state: no front-matter identifier was found, so jobID (a title-only job
// created solely to host the pdf_identifier_needed human action) is
// recorded and the wire outcome is fixed at "needs_identifier".
func (s *Service) MarkParkedNoIdentifier(ctx context.Context, id, jobID string) error {
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, job_id = ?, outcome = 'needs_identifier', updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		string(StateParkedNoIdentifier), jobID, store.Now(), id, string(StateAwaitingFile), string(StateQuarantined))
	if err != nil {
		return err
	}
	return requireOneRow(res, id)
}

// MarkFailedValidation transitions to the terminal failed_validation state:
// the settled file was not a valid PDF.
func (s *Service) MarkFailedValidation(ctx context.Context, id, detail string) error {
	res, err := s.store.DB().ExecContext(ctx, `
		UPDATE pdf_grabs SET state = ?, outcome = 'failed_validation', detail = ?, updated_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		string(StateFailedValidation), detail, store.Now(), id, string(StateAwaitingFile), string(StateQuarantined))
	if err != nil {
		return err
	}
	return requireOneRow(res, id)
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

// Delete removes a grab row outright. Used when a pdf_identifier_needed
// human action is dismissed: the job it parked stays (dismiss cancels
// nothing — see internal/job's dismissalCancelsParkedJob), but the grab
// bookkeeping itself is discarded (ADR-0020's dismissal disposition).
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
