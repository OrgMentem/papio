// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package watch owns durable scheduled discovery watchlists and their bounded
// execution over the existing discovery, Zotio, acquisition, batch, and notify
// services.
package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/store"
)

const (
	DefaultPerRunCap     = 10
	MaxPerRunCap         = 50
	DisableAfterFailures = 5
)

// Filters is the persisted subset of discovery search filters that a watch may
// apply. Query and limit live on Watch so the search is always bounded by its
// per-run cap.
type Filters struct {
	YearFrom int  `json:"year_from,omitempty"`
	YearTo   int  `json:"year_to,omitempty"`
	OAOnly   bool `json:"oa_only,omitempty"`
}

// Watch is one durable scheduled discovery search.
type Watch struct {
	ID                  int64   `json:"id"`
	Label               string  `json:"label"`
	Query               string  `json:"query"`
	Filters             Filters `json:"filters"`
	Collection          string  `json:"collection,omitempty"`
	CadenceHours        int     `json:"cadence_hours"`
	PerRunCap           int     `json:"per_run_cap"`
	Enabled             bool    `json:"enabled"`
	LastRunAt           string  `json:"last_run_at,omitempty"`
	CreatedAt           string  `json:"created_at"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastError           string  `json:"last_error,omitempty"`
}

// CreateInput is the strict API and CLI input for a new watch.
type CreateInput struct {
	Label        string  `json:"label,omitempty"`
	Query        string  `json:"query"`
	Filters      Filters `json:"filters,omitempty"`
	Collection   string  `json:"collection,omitempty"`
	CadenceHours int     `json:"cadence_hours"`
	PerRunCap    int     `json:"per_run_cap"`
}

// IDInput names one durable watch.
type IDInput struct {
	ID int64 `json:"id"`
}

// FailureResult describes a recorded execution failure.
type FailureResult struct {
	ConsecutiveFailures int
	Disabled            bool
}

// Store layers watch semantics over the process-wide single-writer SQLite
// handle.
type Store struct{ S *store.Store }

// NewStore creates the watch store over the existing process-wide database.
func NewStore(s *store.Store) *Store { return &Store{S: s} }

// Create persists one enabled watch.
func (s *Store) Create(ctx context.Context, input CreateInput) (*Watch, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	input, err := normalizeCreateInput(input)
	if err != nil {
		return nil, err
	}
	filters, err := json.Marshal(input.Filters)
	if err != nil {
		return nil, fmt.Errorf("encoding watch filters: %w", err)
	}
	result, err := s.S.DB().ExecContext(ctx, `
		INSERT INTO watches (label, query, filters_json, collection, cadence_hours, per_run_cap, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		input.Label, input.Query, string(filters), input.Collection, input.CadenceHours, input.PerRunCap, store.Now())
	if err != nil {
		return nil, fmt.Errorf("creating watch: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading watch ID: %w", err)
	}
	return s.Get(ctx, id)
}

// Get returns one watch by durable ID.
func (s *Store) Get(ctx context.Context, id int64) (*Watch, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	if id <= 0 {
		return nil, errors.New("watch id must be positive")
	}
	return scanWatch(s.S.DB().QueryRowContext(ctx, watchSelect+` WHERE id = ?`, id))
}

// List returns watches in creation order so the scheduler executes due watches
// serially and deterministically.
func (s *Store) List(ctx context.Context) ([]Watch, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	rows, err := s.S.DB().QueryContext(ctx, watchSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing watches: %w", err)
	}
	defer rows.Close()
	watches := make([]Watch, 0)
	for rows.Next() {
		watch, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		watches = append(watches, *watch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watches: %w", err)
	}
	return watches, nil
}

// Remove permanently deletes a watch. Existing acquisition jobs and manifests
// remain durable history; only future scheduling is removed.
func (s *Store) Remove(ctx context.Context, id int64) error {
	if s == nil || s.S == nil {
		return errors.New("watch store is not configured")
	}
	if id <= 0 {
		return errors.New("watch id must be positive")
	}
	result, err := s.S.DB().ExecContext(ctx, `DELETE FROM watches WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("removing watch: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Due returns enabled watches whose previous attempt is at least one cadence
// old. A newly created watch is immediately due.
func (s *Store) Due(ctx context.Context, now time.Time) ([]Watch, error) {
	watches, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	due := make([]Watch, 0, len(watches))
	for _, watch := range watches {
		if !watch.Enabled {
			continue
		}
		if watch.LastRunAt == "" {
			due = append(due, watch)
			continue
		}
		lastRun, err := time.Parse(time.RFC3339Nano, watch.LastRunAt)
		if err != nil {
			return nil, fmt.Errorf("parsing watch %d last_run_at: %w", watch.ID, err)
		}
		if !lastRun.Add(time.Duration(watch.CadenceHours) * time.Hour).After(now) {
			due = append(due, watch)
		}
	}
	return due, nil
}

// MarkRun records a successful (including zero-new-work) execution and resets
// its consecutive failure counter.
func (s *Store) MarkRun(ctx context.Context, id int64, at time.Time) error {
	if s == nil || s.S == nil {
		return errors.New("watch store is not configured")
	}
	result, err := s.S.DB().ExecContext(ctx, `
		UPDATE watches
		SET last_run_at = ?, consecutive_failures = 0, last_error = ''
		WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("recording watch run: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkDegradedRun records a partially successful submission batch. It advances
// the cadence and resets consecutive complete-run failures while retaining the
// bounded failure count for the user to inspect.
func (s *Store) MarkDegradedRun(ctx context.Context, id int64, at time.Time, failed, total int) error {
	if s == nil || s.S == nil {
		return errors.New("watch store is not configured")
	}
	if failed < 1 || total < failed {
		return errors.New("invalid degraded watch run counts")
	}
	result, err := s.S.DB().ExecContext(ctx, `
		UPDATE watches
		SET last_run_at = ?, consecutive_failures = 0, last_error = ?
		WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano),
		fmt.Sprintf("%d of %d watch submissions failed", failed, total), id)
	if err != nil {
		return fmt.Errorf("recording degraded watch run: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordFailure records an execution failure as an attempted run. The fifth
// consecutive failure disables the watch so periodic scheduling cannot loop
// indefinitely on a broken dependency.
func (s *Store) RecordFailure(ctx context.Context, id int64, at time.Time, runErr error) (FailureResult, error) {
	if s == nil || s.S == nil {
		return FailureResult{}, errors.New("watch store is not configured")
	}
	if runErr == nil {
		return FailureResult{}, errors.New("watch failure requires an error")
	}
	tx, err := s.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return FailureResult{}, fmt.Errorf("starting watch failure transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures, enabled FROM watches WHERE id = ?`, id).Scan(&failures, &enabled); err != nil {
		return FailureResult{}, err
	}
	failures++
	newEnabled := enabled && failures < DisableAfterFailures
	disabled := enabled && !newEnabled
	if _, err := tx.ExecContext(ctx, `
		UPDATE watches
		SET last_run_at = ?, consecutive_failures = ?, last_error = ?, enabled = ?
		WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), failures, storedError(runErr), newEnabled, id); err != nil {
		return FailureResult{}, fmt.Errorf("recording watch failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FailureResult{}, fmt.Errorf("committing watch failure: %w", err)
	}
	return FailureResult{ConsecutiveFailures: failures, Disabled: disabled}, nil
}

const watchSelect = `
	SELECT id, label, query, filters_json, collection, cadence_hours, per_run_cap,
	       enabled, COALESCE(last_run_at, ''), created_at, consecutive_failures, last_error
	FROM watches`

type watchScanner interface {
	Scan(...any) error
}

func scanWatch(scanner watchScanner) (*Watch, error) {
	var watch Watch
	var filters string
	if err := scanner.Scan(
		&watch.ID, &watch.Label, &watch.Query, &filters, &watch.Collection,
		&watch.CadenceHours, &watch.PerRunCap, &watch.Enabled, &watch.LastRunAt,
		&watch.CreatedAt, &watch.ConsecutiveFailures, &watch.LastError,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filters), &watch.Filters); err != nil {
		return nil, fmt.Errorf("decoding watch %d filters: %w", watch.ID, err)
	}
	return &watch, nil
}

func normalizeCreateInput(input CreateInput) (CreateInput, error) {
	input.Query = strings.TrimSpace(input.Query)
	input.Label = strings.TrimSpace(input.Label)
	input.Collection = strings.TrimSpace(input.Collection)
	if input.Query == "" {
		return CreateInput{}, errors.New("watch query is required")
	}
	if input.Label == "" {
		input.Label = input.Query
	}
	if input.Collection == "" {
		input.Collection = input.Query
	}
	if input.CadenceHours <= 0 {
		return CreateInput{}, errors.New("watch cadence_hours must be positive")
	}
	if input.PerRunCap == 0 {
		input.PerRunCap = DefaultPerRunCap
	}
	if input.PerRunCap < 1 || input.PerRunCap > MaxPerRunCap {
		return CreateInput{}, fmt.Errorf("watch per_run_cap must be 1-%d", MaxPerRunCap)
	}
	if input.Filters.YearFrom < 0 || input.Filters.YearTo < 0 {
		return CreateInput{}, errors.New("watch years must be positive")
	}
	if input.Filters.YearFrom > 0 && input.Filters.YearTo > 0 && input.Filters.YearFrom > input.Filters.YearTo {
		return CreateInput{}, errors.New("watch year_from cannot exceed year_to")
	}
	return input, nil
}

func storedError(err error) string {
	message := strings.TrimSpace(err.Error())
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 500 {
		message = message[:500]
	}
	if message == "" {
		return "watch execution failed"
	}
	return message
}
