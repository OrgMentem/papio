// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"papio/internal/job"
)

// Exception tags are the only Zotero-visible state papio maintains: durable,
// low-churn answers to "is this item coming, or is it mine now?". Lifecycle
// states (queued/fetching/...) are deliberately never tagged — success is
// self-indicating (the attachment appears) and transient states have no
// reader inside Zotero. See zotio dev/adr/0004.
const (
	// TagNeedsAction marks an item whose acquisition parked on a human
	// action (SSO login, terms consent, identity review).
	TagNeedsAction = "papio:needs-action"
	// TagUnavailable marks an item papio exhausted OA and institutional
	// routes for, as of the last attempt. Backfill re-checks it after the
	// configured cool-down, so the tag self-clears when access appears.
	TagUnavailable = "papio:unavailable"

	// tagsMinimumZotioVersion is the first zotio release whose
	// `items tags add` supports --automatic (Zotero automatic-type tags).
	tagsMinimumZotioVersion = "0.13.0"

	// tagBatchSize bounds item keys per zotio invocation, well under
	// zotio's per-mutation change cap and any argv limit.
	tagBatchSize = 50

	// tagErrorLogInterval throttles repeated identical reconcile failures in
	// the minutely maintenance loop.
	tagErrorLogInterval = 15 * time.Minute
)

// TagReconcileResult is stable machine output for `papio zotio tags reconcile`.
type TagReconcileResult struct {
	// Checked is the number of Zotero items considered (items with any
	// papio job history or a previously applied exception tag).
	Checked int `json:"checked"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// desiredTag maps one job state to the exception tag the linked item should
// carry. Pure by design: tags are a function of current state, never of the
// event history, so a lost write is repaired by the next pass.
func desiredTag(state string) string {
	switch state {
	case job.StateAwaitingHuman, job.StateNeedsReview:
		return TagNeedsAction
	case job.StateUnavailable:
		return TagUnavailable
	default:
		// Live states clear the tag ("papio is working on it"), ready and
		// imported are self-indicating, and failed/cancelled are papio's or
		// the user's own doing — none of them is the user's cue to act.
		return ""
	}
}

// ReconcileTags converges the exception tags on linked Zotero items with the
// current job states. It computes deltas against the applied-state ledger
// (zotio_tag_state) first and invokes zotio only when something changed, so
// the steady-state pass costs one SQLite query and zero subprocesses.
func (s *Service) ReconcileTags(ctx context.Context) (*TagReconcileResult, error) {
	if s == nil || s.CLI == nil || s.Store == nil {
		return nil, fmt.Errorf("Zotio integration is not configured")
	}
	desired, considered, err := s.desiredTags(ctx)
	if err != nil {
		return nil, err
	}
	applied, err := s.appliedTags(ctx)
	if err != nil {
		return nil, err
	}

	removals := map[string][]string{} // tag -> item keys
	additions := map[string][]string{}
	keys := make(map[string]struct{}, len(desired)+len(applied))
	for key := range desired {
		keys[key] = struct{}{}
	}
	for key := range applied {
		keys[key] = struct{}{}
		if _, seen := considered[key]; !seen {
			considered[key] = struct{}{}
		}
	}
	result := &TagReconcileResult{Checked: len(considered)}
	for key := range keys {
		want, have := desired[key], applied[key]
		if want == have {
			continue
		}
		if have != "" {
			removals[have] = append(removals[have], key)
		}
		if want != "" {
			additions[want] = append(additions[want], key)
		}
	}
	if len(removals) == 0 && len(additions) == 0 {
		return result, nil
	}

	preflight, err := s.CLI.Preflight(ctx)
	if err != nil {
		return nil, err
	}
	if compareVersion(preflight.Version, tagsMinimumZotioVersion) < 0 {
		return nil, fmt.Errorf("zotio %s does not support automatic exception tags (requires >= %s) — update zotio, or disable zotio.exception_tags", preflight.Version, tagsMinimumZotioVersion)
	}

	// Removals first: a tag transition (needs-action -> unavailable) must
	// never leave both tags on an item, even across a crash between batches.
	for _, tag := range sortedTagKeys(removals) {
		for _, batch := range batchKeys(removals[tag]) {
			// Explicitly mark the persistent flag as changed with an empty
			// value, so zotio ignores an inherited ZOTERO_GROUP and writes
			// papio's personal access verdict only to My Library.
			args := append([]string{"--agent", "--yes", "--group=", "items", "tags", "remove", "--tag", tag}, batch...)
			if _, err := s.CLI.RunJSON(ctx, args...); err != nil {
				return result, fmt.Errorf("removing %s from %d items: %w", tag, len(batch), err)
			}
			if err := s.deleteTagState(ctx, batch); err != nil {
				return result, err
			}
			result.Removed += len(batch)
		}
	}
	for _, tag := range sortedTagKeys(additions) {
		for _, batch := range batchKeys(additions[tag]) {
			// See removal above: exception state is personal, never shared
			// into a Zotero group library.
			args := append([]string{"--agent", "--yes", "--group=", "items", "tags", "add", "--automatic", "--tag", tag}, batch...)
			if _, err := s.CLI.RunJSON(ctx, args...); err != nil {
				return result, fmt.Errorf("adding %s to %d items: %w", tag, len(batch), err)
			}
			if err := s.upsertTagState(ctx, batch, tag); err != nil {
				return result, err
			}
			result.Added += len(batch)
		}
	}
	return result, nil
}

// desiredTags returns item key -> wanted tag, derived from the most recent
// job of each Zotero-linked work request, plus the full set of item keys
// considered (including those whose latest state wants no tag).
func (s *Service) desiredTags(ctx context.Context) (map[string]string, map[string]struct{}, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `
		SELECT w.zotio_item_key, j.state
		FROM jobs j
		JOIN work_requests w ON w.id = j.work_request_id
		WHERE w.zotio_item_key IS NOT NULL AND w.zotio_item_key != ''
		ORDER BY j.created_at DESC, j.id DESC`)
	if err != nil {
		return nil, nil, fmt.Errorf("reading job states for tag reconcile: %w", err)
	}
	defer rows.Close()
	latest := map[string]string{}
	for rows.Next() {
		var key, state string
		if err := rows.Scan(&key, &state); err != nil {
			return nil, nil, err
		}
		if _, seen := latest[key]; seen {
			continue // newest job already decided this item
		}
		latest[key] = state
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	desired := make(map[string]string, len(latest))
	considered := make(map[string]struct{}, len(latest))
	for key, state := range latest {
		considered[key] = struct{}{}
		if tag := desiredTag(state); tag != "" {
			desired[key] = tag
		}
	}
	return desired, considered, nil
}

// appliedTags returns the applied-state ledger: item key -> tag papio
// believes is currently written.
func (s *Service) appliedTags(ctx context.Context) (map[string]string, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT item_key, tag FROM zotio_tag_state`)
	if err != nil {
		return nil, fmt.Errorf("reading applied tag state: %w", err)
	}
	defer rows.Close()
	applied := map[string]string{}
	for rows.Next() {
		var key, tag string
		if err := rows.Scan(&key, &tag); err != nil {
			return nil, err
		}
		applied[key] = tag
	}
	return applied, rows.Err()
}

func (s *Service) deleteTagState(ctx context.Context, keys []string) error {
	placeholders := strings.Repeat("?,", len(keys))
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	_, err := s.Store.DB().ExecContext(ctx,
		"DELETE FROM zotio_tag_state WHERE item_key IN ("+placeholders[:len(placeholders)-1]+")", args...)
	if err != nil {
		return fmt.Errorf("clearing applied tag state: %w", err)
	}
	return nil
}

func (s *Service) upsertTagState(ctx context.Context, keys []string, tag string) error {
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO zotio_tag_state(item_key, tag, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(item_key) DO UPDATE SET tag = excluded.tag, updated_at = excluded.updated_at`,
			key, tag, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording applied tag state: %w", err)
		}
	}
	return tx.Commit()
}

func batchKeys(keys []string) [][]string {
	sort.Strings(keys) // deterministic invocations for tests and logs
	batches := make([][]string, 0, (len(keys)+tagBatchSize-1)/tagBatchSize)
	for len(keys) > tagBatchSize {
		batches = append(batches, keys[:tagBatchSize])
		keys = keys[tagBatchSize:]
	}
	if len(keys) > 0 {
		batches = append(batches, keys)
	}
	return batches
}

func sortedTagKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for tag := range m {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// TagReconciler adapts ReconcileTags to the daemon's maintenance seam with
// throttled error logging (maintenance discards returned errors by design).
type TagReconciler struct {
	service    *Service
	lastErr    string
	lastErrLog time.Time
}

// TagReconciler returns the maintenance runner, or nil when the exception-tag
// ledger is disabled or zotio is not configured.
func (s *Service) TagReconciler() *TagReconciler {
	if s == nil || s.CLI == nil || s.Store == nil || !s.ExceptionTags {
		return nil
	}
	return &TagReconciler{service: s}
}

// RunDue performs one reconcile pass. Errors are logged (throttled) and
// returned; the scheduler treats maintenance as best-effort.
func (r *TagReconciler) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	_, err := r.service.ReconcileTags(ctx)
	if err == nil {
		r.lastErr = ""
		return nil
	}
	if msg := err.Error(); msg != r.lastErr || time.Since(r.lastErrLog) >= tagErrorLogInterval {
		log.Printf("zotio tag reconcile: %v", err)
		r.lastErr = msg
		r.lastErrLog = time.Now()
	}
	return err
}
