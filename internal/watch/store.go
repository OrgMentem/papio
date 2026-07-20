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

	KindDiscovery = "discovery"
	KindBackfill  = "backfill"
	ModeAcquire   = "acquire"
	ModeAlert     = "alert"
)

// Filters is the persisted subset of discovery search filters that a watch may
// apply. Query and limit live on Watch so the search is always bounded by its
// per-run cap.
type Filters struct {
	YearFrom int  `json:"year_from,omitempty"`
	YearTo   int  `json:"year_to,omitempty"`
	OAOnly   bool `json:"oa_only,omitempty"`
}

// Watch is one durable scheduled discovery search or library backfill.
type Watch struct {
	ID                  int64   `json:"id"`
	Label               string  `json:"label"`
	Kind                string  `json:"kind"`
	Mode                string  `json:"mode"`
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
	Kind         string  `json:"kind,omitempty"`
	Mode         string  `json:"mode,omitempty"`
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

// DigestEntry is one previously unowned discovery reported by an alert watch.
type DigestEntry struct {
	WorkKey     string `json:"work_key"`
	TitleKey    string `json:"-"`
	Title       string `json:"title"`
	Authors     string `json:"authors"`
	Year        int    `json:"year"`
	DOI         string `json:"doi,omitempty"`
	IsOA        bool   `json:"is_oa"`
	FirstSeenAt string `json:"first_seen_at"`
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
		INSERT INTO watches (label, kind, mode, query, filters_json, collection, cadence_hours, per_run_cap, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		input.Label, input.Kind, input.Mode, input.Query, string(filters), input.Collection, input.CadenceHours, input.PerRunCap, store.Now())
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

// RecordDigest stores alert-watch discoveries, retaining only the first sighting
// of each stable work key.
func (s *Store) RecordDigest(ctx context.Context, watchID int64, at time.Time, entries []DigestEntry) (int, error) {
	if s == nil || s.S == nil {
		return 0, errors.New("watch store is not configured")
	}
	if watchID <= 0 {
		return 0, errors.New("watch id must be positive")
	}
	if _, err := s.Get(ctx, watchID); err != nil {
		return 0, err
	}
	tx, err := s.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("starting watch digest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	firstSeenAt := at.UTC().Format(time.RFC3339Nano)
	reported := 0
	for _, entry := range entries {
		entry.WorkKey = strings.TrimSpace(entry.WorkKey)
		entry.TitleKey = strings.TrimSpace(entry.TitleKey)
		entry.Title = strings.TrimSpace(entry.Title)
		entry.DOI = strings.TrimSpace(entry.DOI)
		if entry.WorkKey == "" || entry.Title == "" {
			return 0, errors.New("watch digest entry requires work_key and title")
		}
		if entry.DOI != "" && entry.TitleKey != "" && entry.TitleKey != entry.WorkKey {
			result, err := tx.ExecContext(ctx, `
				UPDATE watch_digest_entries
				SET work_key = ?, doi = ?
				WHERE watch_id = ? AND work_key = ?`,
				entry.WorkKey, entry.DOI, watchID, entry.TitleKey)
			if err != nil {
				return 0, fmt.Errorf("migrating watch digest: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if count > 0 {
				continue
			}
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO watch_digest_entries
				(watch_id, work_key, title, authors, year, doi, is_oa, first_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(watch_id, work_key) DO NOTHING`,
			watchID, entry.WorkKey, entry.Title, strings.TrimSpace(entry.Authors), entry.Year,
			entry.DOI, entry.IsOA, firstSeenAt)
		if err != nil {
			return 0, fmt.Errorf("recording watch digest: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		reported += int(count)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing watch digest: %w", err)
	}
	return reported, nil
}

// Digest returns recent alert-watch discoveries, newest first.
func (s *Store) Digest(ctx context.Context, watchID int64, limit int) ([]DigestEntry, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	if watchID <= 0 {
		return nil, errors.New("watch id must be positive")
	}
	if _, err := s.Get(ctx, watchID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.S.DB().QueryContext(ctx, `
		SELECT work_key, title, authors, year, doi, is_oa, first_seen_at
		FROM watch_digest_entries
		WHERE watch_id = ?
		ORDER BY id DESC
		LIMIT ?`, watchID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing watch digest: %w", err)
	}
	defer rows.Close()
	entries := make([]DigestEntry, 0)
	for rows.Next() {
		var entry DigestEntry
		if err := rows.Scan(
			&entry.WorkKey, &entry.Title, &entry.Authors, &entry.Year,
			&entry.DOI, &entry.IsOA, &entry.FirstSeenAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watch digest: %w", err)
	}
	return entries, nil
}

// ClearDigest removes every pending alert discovery for watchID.
func (s *Store) ClearDigest(ctx context.Context, watchID int64) (int, error) {
	if s == nil || s.S == nil {
		return 0, errors.New("watch store is not configured")
	}
	if watchID <= 0 {
		return 0, errors.New("watch id must be positive")
	}
	if _, err := s.Get(ctx, watchID); err != nil {
		return 0, err
	}
	result, err := s.S.DB().ExecContext(ctx, `DELETE FROM watch_digest_entries WHERE watch_id = ?`, watchID)
	if err != nil {
		return 0, fmt.Errorf("clearing watch digest: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting cleared watch digest entries: %w", err)
	}
	return int(count), nil
}

// TakeDigest returns pending alert discoveries without removing them.
func (s *Store) TakeDigest(ctx context.Context, watchID int64, keys []string) ([]DigestEntry, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	if watchID <= 0 {
		return nil, errors.New("watch id must be positive")
	}
	if _, err := s.Get(ctx, watchID); err != nil {
		return nil, err
	}

	query := `
		SELECT work_key, title, authors, year, doi, is_oa, first_seen_at
		FROM watch_digest_entries
		WHERE watch_id = ?`
	args := []any{watchID}
	requested := make(map[string]struct{}, len(keys))
	if len(keys) > 0 {
		placeholders := make([]string, 0, len(keys))
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key == "" {
				return nil, errors.New("watch digest key must not be empty")
			}
			if _, duplicate := requested[key]; duplicate {
				continue
			}
			requested[key] = struct{}{}
			placeholders = append(placeholders, "?")
			args = append(args, key)
		}
		query += ` AND work_key IN (` + strings.Join(placeholders, ", ") + `)`
	}
	query += ` ORDER BY id DESC`

	rows, err := s.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("taking watch digest: %w", err)
	}
	defer rows.Close()
	entries := make([]DigestEntry, 0)
	found := make(map[string]struct{}, len(requested))
	for rows.Next() {
		var entry DigestEntry
		if err := rows.Scan(
			&entry.WorkKey, &entry.Title, &entry.Authors, &entry.Year,
			&entry.DOI, &entry.IsOA, &entry.FirstSeenAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		found[entry.WorkKey] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watch digest: %w", err)
	}
	for key := range requested {
		if _, ok := found[key]; !ok {
			return nil, fmt.Errorf("watch digest entry %q not found", key)
		}
	}
	return entries, nil
}

func (s *Store) deleteDigestEntry(ctx context.Context, watchID int64, workKey string) error {
	if s == nil || s.S == nil {
		return errors.New("watch store is not configured")
	}
	if watchID <= 0 {
		return errors.New("watch id must be positive")
	}
	workKey = strings.TrimSpace(workKey)
	if workKey == "" {
		return errors.New("watch digest key must not be empty")
	}
	result, err := s.S.DB().ExecContext(ctx, `
		DELETE FROM watch_digest_entries
		WHERE watch_id = ? AND work_key = ?`, watchID, workKey)
	if err != nil {
		return fmt.Errorf("deleting watch digest entry: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting deleted watch digest entry: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("watch digest entry %q not found", workKey)
	}
	return nil
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
			// A single corrupt/legacy last_run_at (schema drift, manual DB
			// edit, or a future writer) must not abort the whole sweep and
			// silently halt every other enabled watch. Isolate the bad row:
			// skip it and record an operator-visible diagnostic. A later forced
			// run rewrites a valid timestamp and clears the annotation.
			s.recordCorruptLastRun(ctx, watch.ID, watch.LastRunAt, err)
			continue
		}
		if !lastRun.Add(time.Duration(watch.CadenceHours) * time.Hour).After(now) {
			due = append(due, watch)
		}
	}
	return due, nil
}

// recordCorruptLastRun best-effort annotates a watch whose stored last_run_at
// could not be parsed so the fault surfaces via `watch list` instead of
// silently halting scheduling. It never returns an error: Due has already
// isolated the row from the sweep, and a diagnostic write must not itself abort
// scheduling of the remaining healthy watches.
func (s *Store) recordCorruptLastRun(ctx context.Context, id int64, value string, cause error) {
	if s == nil || s.S == nil {
		return
	}
	message := storedError(fmt.Errorf("unparseable last_run_at %q: %w", value, cause))
	_, _ = s.S.DB().ExecContext(ctx, `UPDATE watches SET last_error = ? WHERE id = ?`, message, id)
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
	SELECT id, label, kind, mode, query, filters_json, collection, cadence_hours, per_run_cap,
	       enabled, COALESCE(last_run_at, ''), created_at, consecutive_failures, last_error
	FROM watches`

type watchScanner interface {
	Scan(...any) error
}

func scanWatch(scanner watchScanner) (*Watch, error) {
	var watch Watch
	var filters string
	if err := scanner.Scan(
		&watch.ID, &watch.Label, &watch.Kind, &watch.Mode, &watch.Query, &filters, &watch.Collection,
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
	input.Kind = strings.TrimSpace(input.Kind)
	input.Mode = strings.TrimSpace(input.Mode)
	input.Query = strings.TrimSpace(input.Query)
	input.Label = strings.TrimSpace(input.Label)
	input.Collection = strings.TrimSpace(input.Collection)
	if input.Kind == "" {
		input.Kind = KindDiscovery
	}
	if input.Mode == "" {
		input.Mode = ModeAcquire
	}
	if input.Kind != KindDiscovery && input.Kind != KindBackfill {
		return CreateInput{}, fmt.Errorf("unknown watch kind %q", input.Kind)
	}
	if input.Mode != ModeAcquire && input.Mode != ModeAlert {
		return CreateInput{}, fmt.Errorf("unknown watch mode %q", input.Mode)
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
	switch input.Kind {
	case KindDiscovery:
		if input.Query == "" {
			return CreateInput{}, errors.New("watch query is required")
		}
		if input.Label == "" {
			input.Label = input.Query
		}
		if input.Collection == "" {
			input.Collection = input.Query
		}
	case KindBackfill:
		if input.Query != "" {
			return CreateInput{}, errors.New("backfill watch does not accept a query")
		}
		if input.Filters != (Filters{}) {
			return CreateInput{}, errors.New("backfill watch does not accept filters")
		}
		if input.Mode != ModeAcquire {
			return CreateInput{}, errors.New("backfill watch mode must be acquire")
		}
		if input.Label == "" {
			input.Label = "backfill"
			if input.Collection != "" {
				input.Label += ": " + input.Collection
			}
		}
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
