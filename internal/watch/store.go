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

	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
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
	YearFrom  int    `json:"year_from,omitempty"`
	YearTo    int    `json:"year_to,omitempty"`
	OAOnly    bool   `json:"oa_only,omitempty"`
	Cites     string `json:"cites,omitempty"`
	CitedBy   string `json:"cited_by,omitempty"`
	RelatedTo string `json:"related_to,omitempty"`
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
	WorkKey      string                `json:"work_key"`
	TitleKey     string                `json:"-"`
	Title        string                `json:"title"`
	Authors      string                `json:"authors"`
	AuthorNames  []string              `json:"-"`
	Year         int                   `json:"year"`
	DOI          string                `json:"doi,omitempty"`
	IsOA         bool                  `json:"is_oa"`
	FirstSeenAt  string                `json:"first_seen_at"`
	Identifiers  *protocol.Identifiers `json:"identifiers,omitempty"`
	titleAliases []string
}

// ErrDigestEntryNotFound indicates a requested digest work key is absent.
var ErrDigestEntryNotFound = errors.New("watch digest entry not found")

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

// RecordDigest stores alert-watch discoveries, retaining each identity's first
// sighting even after it has been cleared or acquired.
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
		entry.Authors = strings.TrimSpace(entry.Authors)
		entry.DOI = strings.TrimSpace(entry.DOI)
		if entry.Authors == "" && len(entry.AuthorNames) > 0 {
			entry.Authors = strings.Join(entry.AuthorNames, ", ")
		}
		if entry.WorkKey == "" || entry.Title == "" {
			return 0, errors.New("watch digest entry requires work_key and title")
		}

		existing, duplicates, err := findDigestIdentities(ctx, tx, watchID, entry)
		if err != nil {
			return 0, err
		}
		if existing != nil {
			identifiersJSON, doi, err := mergeDigestIdentifiers(existing.identifiersJSON, entry)
			if err != nil {
				return 0, err
			}
			for _, duplicate := range duplicates {
				identifiersJSON, err = mergeDigestIdentifierJSON(identifiersJSON, duplicate.identifiersJSON, duplicate.doi)
				if err != nil {
					return 0, err
				}
			}
			workKey, err := canonicalDigestWorkKey(entry, identifiersJSON)
			if err != nil {
				return 0, err
			}
			authorsJSON, err := mergeDigestAuthors(existing.authorsJSON, entry)
			if err != nil {
				return 0, err
			}
			authors := entry.Authors
			if authors == "" {
				authors = existing.authors
			}
			firstSeenAt := existing.firstSeenAt
			consumed := existing.consumed
			for _, duplicate := range duplicates {
				if authors == "" {
					authors = duplicate.authors
				}
				if authorsJSON == "" {
					authorsJSON = duplicate.authorsJSON
				}
				if duplicate.firstSeenAt < firstSeenAt {
					firstSeenAt = duplicate.firstSeenAt
				}
				consumed = consumed || duplicate.consumed
				if _, err := tx.ExecContext(ctx, `DELETE FROM watch_digest_entries WHERE id = ?`, duplicate.id); err != nil {
					return 0, fmt.Errorf("merging duplicate watch digest identity: %w", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE watch_digest_entries
				SET work_key = ?, title = ?, authors = ?, authors_json = ?, year = ?, doi = ?,
					identifiers_json = ?, is_oa = ?, first_seen_at = ?, consumed = ?
				WHERE id = ?`,
				workKey, entry.Title, authors, authorsJSON, entry.Year, doi,
				identifiersJSON, entry.IsOA, firstSeenAt, consumed, existing.id,
			); err != nil {
				return 0, fmt.Errorf("migrating watch digest: %w", err)
			}
			continue
		}

		identifiersJSON, doi, err := mergeDigestIdentifiers("", entry)
		if err != nil {
			return 0, err
		}
		workKey, err := canonicalDigestWorkKey(entry, identifiersJSON)
		if err != nil {
			return 0, err
		}
		authorsJSON, err := mergeDigestAuthors("", entry)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO watch_digest_entries
				(watch_id, work_key, title, authors, authors_json, year, doi, identifiers_json, is_oa, first_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(watch_id, work_key) DO NOTHING`,
			watchID, workKey, entry.Title, entry.Authors, authorsJSON, entry.Year,
			doi, identifiersJSON, entry.IsOA, firstSeenAt)
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
		SELECT work_key, title, authors, authors_json, year, doi, is_oa, first_seen_at, identifiers_json
		FROM watch_digest_entries
		WHERE watch_id = ? AND consumed = 0
		ORDER BY id DESC
		LIMIT ?`, watchID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing watch digest: %w", err)
	}
	defer rows.Close()
	entries := make([]DigestEntry, 0)
	for rows.Next() {
		var entry DigestEntry
		var identifiersJSON, authorsJSON string
		if err := rows.Scan(
			&entry.WorkKey, &entry.Title, &entry.Authors, &authorsJSON, &entry.Year,
			&entry.DOI, &entry.IsOA, &entry.FirstSeenAt, &identifiersJSON,
		); err != nil {
			return nil, err
		}
		if err := decodeDigestAuthors(&entry, authorsJSON); err != nil {
			return nil, err
		}
		if err := decodeDigestIdentifiers(&entry, identifiersJSON); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watch digest: %w", err)
	}
	return entries, nil
}

// ClearDigest consumes every pending alert discovery for watchID.
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
	result, err := s.S.DB().ExecContext(ctx, `
		UPDATE watch_digest_entries
		SET consumed = 1
		WHERE watch_id = ? AND consumed = 0`, watchID)
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
		SELECT work_key, title, authors, authors_json, year, doi, is_oa, first_seen_at, identifiers_json
		FROM watch_digest_entries
		WHERE watch_id = ? AND consumed = 0`
	args := []any{watchID}
	requested := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, errors.New("watch digest key must not be empty")
		}
		requested[key] = struct{}{}
	}
	query += ` ORDER BY id DESC`

	rows, err := s.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("taking watch digest: %w", err)
	}
	defer rows.Close()
	candidates := make([]DigestEntry, 0)
	for rows.Next() {
		var entry DigestEntry
		var identifiersJSON, authorsJSON string
		if err := rows.Scan(
			&entry.WorkKey, &entry.Title, &entry.Authors, &authorsJSON, &entry.Year,
			&entry.DOI, &entry.IsOA, &entry.FirstSeenAt, &identifiersJSON,
		); err != nil {
			return nil, err
		}
		if err := decodeDigestAuthors(&entry, authorsJSON); err != nil {
			return nil, err
		}
		if err := decodeDigestIdentifiers(&entry, identifiersJSON); err != nil {
			return nil, err
		}
		candidates = append(candidates, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watch digest: %w", err)
	}
	if len(requested) == 0 {
		return candidates, nil
	}

	selected := make(map[int]struct{})
	for key := range requested {
		directMatches := make([]int, 0, 1)
		titleMatches := make([]int, 0, 1)
		for i, entry := range candidates {
			if digestEntryMatchesKey(entry, key) {
				directMatches = append(directMatches, i)
			} else if digestEntryMatchesTitle(entry, key) {
				titleMatches = append(titleMatches, i)
			}
		}
		if len(directMatches) > 0 {
			for _, i := range directMatches {
				selected[i] = struct{}{}
			}
			continue
		}
		if len(titleMatches) != 1 {
			return nil, fmt.Errorf("%w: %q", ErrDigestEntryNotFound, key)
		}
		selected[titleMatches[0]] = struct{}{}
	}
	entries := make([]DigestEntry, 0, len(selected))
	for i, entry := range candidates {
		if _, ok := selected[i]; ok {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (s *Store) consumeDigestEntry(ctx context.Context, watchID int64, workKey string) error {
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
		UPDATE watch_digest_entries
		SET consumed = 1
		WHERE watch_id = ? AND work_key = ? AND consumed = 0`, watchID, workKey)
	if err != nil {
		return fmt.Errorf("consuming watch digest entry: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting consumed watch digest entries: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: %q", ErrDigestEntryNotFound, workKey)
	}
	return nil
}

type digestIdentity struct {
	id              int64
	workKey         string
	title           string
	doi             string
	identifiersJSON string
	authors         string
	authorsJSON     string
	firstSeenAt     string
	consumed        bool
}

func findDigestIdentities(ctx context.Context, tx *sql.Tx, watchID int64, entry DigestEntry) (*digestIdentity, []digestIdentity, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, work_key, title, doi, identifiers_json, authors, authors_json, first_seen_at, consumed
		FROM watch_digest_entries
		WHERE watch_id = ?
		ORDER BY first_seen_at ASC, id ASC`, watchID)
	if err != nil {
		return nil, nil, fmt.Errorf("finding watch digest identity: %w", err)
	}
	defer rows.Close()

	want := digestIdentityAliases(entry)
	wantStable := digestStableIdentityAliases(entry)
	var stableMatches []digestIdentity
	var exactMatch, match *digestIdentity
	for rows.Next() {
		var row digestIdentity
		if err := rows.Scan(
			&row.id, &row.workKey, &row.title, &row.doi, &row.identifiersJSON, &row.authors,
			&row.authorsJSON, &row.firstSeenAt, &row.consumed,
		); err != nil {
			return nil, nil, err
		}
		existing := DigestEntry{
			WorkKey: row.workKey,
			Title:   row.title,
			DOI:     row.doi,
		}
		if err := decodeDigestIdentifiers(&existing, row.identifiersJSON); err != nil {
			return nil, nil, err
		}
		existingAliases := digestIdentityAliases(existing)
		if !digestIdentitiesOverlap(want, existingAliases) {
			continue
		}
		existingStable := digestStableIdentityAliases(existing)
		if digestStableIdentifiersConflict(entry, existing) {
			continue
		}
		if digestIdentitiesOverlap(wantStable, existingStable) {
			stableMatches = append(stableMatches, row)
			continue
		}
		if len(wantStable) > 0 && len(existingStable) > 0 {
			// A shared title or legacy key cannot establish that two differently
			// identified works are the same work.
			continue
		}
		if row.workKey == entry.WorkKey {
			candidate := row
			exactMatch = &candidate
			continue
		}
		if match == nil {
			candidate := row
			match = &candidate
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating watch digest identities: %w", err)
	}
	if len(stableMatches) > 0 {
		return &stableMatches[0], stableMatches[1:], nil
	}
	if exactMatch != nil {
		return exactMatch, nil, nil
	}
	return match, nil, nil
}

func digestStableIdentifiersConflict(left, right DigestEntry) bool {
	leftIDs := digestEntryIdentifiers(left)
	rightIDs := digestEntryIdentifiers(right)
	return (leftIDs.DOI != "" && rightIDs.DOI != "" && !strings.EqualFold(leftIDs.DOI, rightIDs.DOI)) ||
		(leftIDs.ArXiv != "" && rightIDs.ArXiv != "" && !strings.EqualFold(leftIDs.ArXiv, rightIDs.ArXiv)) ||
		(leftIDs.OpenAlex != "" && rightIDs.OpenAlex != "" && !strings.EqualFold(leftIDs.OpenAlex, rightIDs.OpenAlex))
}

func digestIdentitiesOverlap(left, right map[string]struct{}) bool {
	for alias := range left {
		if _, ok := right[alias]; ok {
			return true
		}
	}
	return false
}

func digestIdentityAliases(entry DigestEntry) map[string]struct{} {
	aliases := make(map[string]struct{})
	add := func(kind, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			aliases[kind+":"+strings.ToLower(value)] = struct{}{}
		}
	}
	addTitle := func(value string) {
		if value = normalizeDigestTitle(value); value != "" {
			aliases["title:"+value] = struct{}{}
		}
	}

	add("key", entry.WorkKey)
	addTitle(entry.TitleKey)
	addTitle(entry.Title)
	for _, titleAlias := range entry.titleAliases {
		addTitle(titleAlias)
	}
	for alias := range digestStableIdentityAliases(entry) {
		aliases[alias] = struct{}{}
	}
	if entry.Identifiers != nil {
		add("pmid", entry.Identifiers.PMID)
		add("isbn", entry.Identifiers.ISBN)
	}
	return aliases
}

func digestEntryMatchesKey(entry DigestEntry, key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	aliases := digestIdentityAliases(entry)
	if _, ok := aliases["key:"+key]; ok && !digestEntryMatchesTitle(entry, key) {
		return true
	}
	if _, ok := aliases[key]; ok {
		return true
	}
	_, ok := aliases["doi:"+key]
	return ok
}

func digestEntryMatchesTitle(entry DigestEntry, key string) bool {
	_, ok := digestIdentityAliases(entry)["title:"+normalizeDigestTitle(key)]
	return ok
}

func digestStableIdentityAliases(entry DigestEntry) map[string]struct{} {
	identifiers := digestEntryIdentifiers(entry)
	aliases := make(map[string]struct{}, 3)
	add := func(kind, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			aliases[kind+":"+strings.ToLower(value)] = struct{}{}
		}
	}
	add("doi", identifiers.DOI)
	add("arxiv", identifiers.ArXiv)
	add("openalex", identifiers.OpenAlex)
	return aliases
}

func digestEntryIdentifiers(entry DigestEntry) protocol.Identifiers {
	var identifiers protocol.Identifiers
	if entry.Identifiers != nil {
		identifiers = *entry.Identifiers
	}
	if doi := strings.TrimSpace(entry.DOI); doi != "" {
		identifiers.DOI = doi
	}
	workKey := strings.TrimSpace(entry.WorkKey)
	lowerWorkKey := strings.ToLower(workKey)
	if identifiers.ArXiv == "" && strings.HasPrefix(lowerWorkKey, "arxiv:") {
		identifiers.ArXiv = workKey[len("arxiv:"):]
	}
	if identifiers.OpenAlex == "" && strings.HasPrefix(lowerWorkKey, "openalex:") {
		identifiers.OpenAlex = workKey[len("openalex:"):]
	}
	if identifiers.DOI == "" && strings.HasPrefix(lowerWorkKey, "10.") && strings.Contains(workKey, "/") {
		identifiers.DOI = workKey
	}
	return identifiers
}

func canonicalDigestWorkKey(entry DigestEntry, identifiersJSON string) (string, error) {
	identifiers := digestEntryIdentifiers(entry)
	if identifiersJSON != "" {
		if err := json.Unmarshal([]byte(identifiersJSON), &identifiers); err != nil {
			return "", fmt.Errorf("decoding watch digest identifiers: %w", err)
		}
	}
	if doi := strings.TrimSpace(identifiers.DOI); doi != "" {
		return doi, nil
	}
	if arXiv := strings.TrimSpace(identifiers.ArXiv); arXiv != "" {
		return "arxiv:" + strings.TrimPrefix(arXiv, "arXiv:"), nil
	}
	if openAlex := strings.TrimSpace(identifiers.OpenAlex); openAlex != "" {
		return "openalex:" + strings.TrimPrefix(openAlex, "openalex:"), nil
	}
	if titleKey := normalizeDigestTitle(entry.TitleKey); titleKey != "" {
		return titleKey, nil
	}
	if titleKey := normalizeDigestTitle(entry.Title); titleKey != "" {
		return titleKey, nil
	}
	return strings.TrimSpace(entry.WorkKey), nil
}

func normalizeDigestTitle(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

type digestIdentifierPayload struct {
	protocol.Identifiers
	TitleAliases []string `json:"title_aliases,omitempty"`
}

func decodeDigestIdentifierPayload(encoded string) (digestIdentifierPayload, error) {
	var payload digestIdentifierPayload
	if encoded == "" {
		return payload, nil
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return digestIdentifierPayload{}, fmt.Errorf("decoding watch digest identifiers: %w", err)
	}
	return payload, nil
}

func addDigestTitleAliases(payload *digestIdentifierPayload, values ...string) {
	seen := make(map[string]struct{}, len(payload.TitleAliases)+len(values))
	aliases := make([]string, 0, len(payload.TitleAliases)+len(values))
	for _, value := range append(payload.TitleAliases, values...) {
		if value = normalizeDigestTitle(value); value != "" {
			if _, duplicate := seen[value]; !duplicate {
				seen[value] = struct{}{}
				aliases = append(aliases, value)
			}
		}
	}
	payload.TitleAliases = aliases
}

func encodeDigestIdentifierPayload(payload digestIdentifierPayload) (string, error) {
	if payload.Identifiers == (protocol.Identifiers{}) && len(payload.TitleAliases) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding watch digest identifiers: %w", err)
	}
	return string(encoded), nil
}

func mergeDigestIdentifiers(existingJSON string, entry DigestEntry) (string, string, error) {
	payload, err := decodeDigestIdentifierPayload(existingJSON)
	if err != nil {
		return "", "", err
	}
	incoming := digestEntryIdentifiers(entry)
	if incoming.DOI != "" {
		payload.DOI = incoming.DOI
	}
	if incoming.PMID != "" {
		payload.PMID = incoming.PMID
	}
	if incoming.ArXiv != "" {
		payload.ArXiv = incoming.ArXiv
	}
	if incoming.ISBN != "" {
		payload.ISBN = incoming.ISBN
	}
	if incoming.OpenAlex != "" {
		payload.OpenAlex = incoming.OpenAlex
	}
	addDigestTitleAliases(&payload, entry.TitleKey, entry.Title)
	encoded, err := encodeDigestIdentifierPayload(payload)
	if err != nil {
		return "", "", err
	}
	return encoded, payload.DOI, nil
}

func mergeDigestIdentifierJSON(existingJSON, incomingJSON, incomingDOI string) (string, error) {
	existing, err := decodeDigestIdentifierPayload(existingJSON)
	if err != nil {
		return "", err
	}
	incoming, err := decodeDigestIdentifierPayload(incomingJSON)
	if err != nil {
		return "", err
	}
	if incoming.DOI == "" {
		incoming.DOI = incomingDOI
	}
	if existing.DOI == "" {
		existing.DOI = incoming.DOI
	}
	if existing.PMID == "" {
		existing.PMID = incoming.PMID
	}
	if existing.ArXiv == "" {
		existing.ArXiv = incoming.ArXiv
	}
	if existing.ISBN == "" {
		existing.ISBN = incoming.ISBN
	}
	if existing.OpenAlex == "" {
		existing.OpenAlex = incoming.OpenAlex
	}
	addDigestTitleAliases(&existing, incoming.TitleAliases...)
	return encodeDigestIdentifierPayload(existing)
}

func mergeDigestAuthors(existingJSON string, entry DigestEntry) (string, error) {
	authors := entry.AuthorNames
	if len(authors) == 0 && strings.TrimSpace(entry.Authors) == "" {
		return existingJSON, nil
	}
	if len(authors) == 0 {
		for _, author := range strings.Split(entry.Authors, ",") {
			if author = strings.TrimSpace(author); author != "" {
				authors = append(authors, author)
			}
		}
	}
	encoded, err := json.Marshal(authors)
	if err != nil {
		return "", fmt.Errorf("encoding watch digest authors: %w", err)
	}
	return string(encoded), nil
}

func decodeDigestAuthors(entry *DigestEntry, encoded string) error {
	if encoded == "" {
		for _, author := range strings.Split(entry.Authors, ",") {
			if author = strings.TrimSpace(author); author != "" {
				entry.AuthorNames = append(entry.AuthorNames, author)
			}
		}
		return nil
	}
	if err := json.Unmarshal([]byte(encoded), &entry.AuthorNames); err != nil {
		return fmt.Errorf("decoding watch digest authors: %w", err)
	}
	return nil
}

func decodeDigestIdentifiers(entry *DigestEntry, encoded string) error {
	payload, err := decodeDigestIdentifierPayload(encoded)
	if err != nil {
		return err
	}
	entry.titleAliases = append(entry.titleAliases, payload.TitleAliases...)
	if payload.Identifiers != (protocol.Identifiers{}) {
		identifiers := payload.Identifiers
		entry.Identifiers = &identifiers
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

func (filters Filters) citationSnowballCount() int {
	count := 0
	for _, doi := range []string{filters.Cites, filters.CitedBy, filters.RelatedTo} {
		if strings.TrimSpace(doi) != "" {
			count++
		}
	}
	return count
}

func normalizeCitationSnowballFilters(filters *Filters) error {
	for _, seed := range []struct {
		name  string
		value *string
	}{
		{name: "cites", value: &filters.Cites},
		{name: "cited_by", value: &filters.CitedBy},
		{name: "related_to", value: &filters.RelatedTo},
	} {
		if strings.TrimSpace(*seed.value) == "" {
			*seed.value = ""
			continue
		}
		doi, err := work.NormalizeDOI(*seed.value)
		if err != nil {
			return fmt.Errorf("watch invalid DOI for %s: %w", seed.name, err)
		}
		*seed.value = doi
	}
	return nil
}

func watchSnowballLabel(filters Filters) string {
	switch {
	case filters.Cites != "":
		return "cites: " + filters.Cites
	case filters.CitedBy != "":
		return "cited by: " + filters.CitedBy
	default:
		return "related to: " + filters.RelatedTo
	}
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
	if err := normalizeCitationSnowballFilters(&input.Filters); err != nil {
		return CreateInput{}, err
	}
	switch input.Kind {
	case KindDiscovery:
		snowballCount := input.Filters.citationSnowballCount()
		if input.Query == "" && snowballCount == 0 {
			return CreateInput{}, errors.New("watch query is required unless a citation snowball DOI is supplied")
		}
		// Watches do not select a single discovery source, so they must use the
		// common Semantic Scholar/OpenAlex snowball subset to avoid failing every
		// scheduled tick when Semantic Scholar is configured.
		if snowballCount > 1 {
			return CreateInput{}, errors.New("watch supports exactly one citation snowball DOI because configured discovery sources may include Semantic Scholar")
		}
		if input.Filters.RelatedTo != "" {
			return CreateInput{}, errors.New("watch related_to is unsupported because configured discovery sources may include Semantic Scholar")
		}
		if input.Query != "" && snowballCount != 0 {
			return CreateInput{}, errors.New("watch query cannot be combined with a citation snowball because configured discovery sources may include Semantic Scholar")
		}
		if input.Label == "" {
			if input.Query != "" {
				input.Label = input.Query
			} else {
				input.Label = watchSnowballLabel(input.Filters)
			}
		}
		if input.Collection == "" {
			if input.Query != "" {
				input.Collection = input.Query
			} else {
				input.Collection = watchSnowballLabel(input.Filters)
			}
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
