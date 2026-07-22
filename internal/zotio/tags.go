// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"papio/internal/job"
)

// Exception tags are the only Zotero-visible state papio maintains: durable,
// low-churn answers to "is this item coming, or is it mine now?". Lifecycle
// states (queued/fetching/...) are deliberately never tagged — success is
// self-indicating (the attachment appears) and transient states have no
// reader inside Zotero. See zotio dev/adr/0004.
const (
	TagNeedsAction = "papio:needs-action"
	TagUnavailable = "papio:unavailable"

	// First zotio release with --automatic and --automatic-only.
	tagsMinimumZotioVersion = "0.13.0"
	tagBatchSize            = 50
	tagErrorLogInterval     = 15 * time.Minute

	tagStatusPending = "pending"
	tagStatusOwned   = "owned"
	tagStatusForeign = "foreign"
	tagStatusMissing = "missing"
)

// TagReconcileResult is stable machine output for `papio zotio tags reconcile`.
type TagReconcileResult struct {
	Checked int `json:"checked"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

type missingPDFKeyReader interface {
	MissingPDFKeys(context.Context, []string) ([]MissingPDFItem, error)
}

type tagLedgerState struct {
	Tag    string
	Status string
}

type tagMutationReason struct {
	TagTypes map[string]int `json:"tag_types"`
}

type tagMutationEnvelope struct {
	Result *struct {
		Items []struct {
			Key    string          `json:"key"`
			Status string          `json:"status"`
			Reason json.RawMessage `json:"reason"`
		} `json:"items"`
	} `json:"result"`
}

func desiredTag(state string) string {
	switch state {
	case job.StateAwaitingHuman, job.StateNeedsReview:
		return TagNeedsAction
	case job.StateUnavailable:
		return TagUnavailable
	default:
		return ""
	}
}

// ReconcileTags converges personal-library exception tags with current job and
// Zotero attachment state. The mutex covers snapshot -> remote mutation ->
// ledger update, so the scheduler and on-demand RPC cannot interleave passes.
// A pending row is written before an add; if the process dies after the remote
// write, the next pass can safely recover with zotio's idempotent operations.
func (s *Service) ReconcileTags(ctx context.Context) (*TagReconcileResult, error) {
	if s == nil || s.CLI == nil || s.Store == nil {
		return nil, fmt.Errorf("Zotio integration is not configured")
	}
	s.tagMu.Lock()
	defer s.tagMu.Unlock()

	desired, considered, err := s.desiredTags(ctx)
	if err != nil {
		return nil, err
	}
	if !s.ExceptionTags {
		desired = map[string]string{}
	} else if desired, err = s.keepMissingPDFs(ctx, desired); err != nil {
		return nil, err
	}
	ledger, err := s.tagLedger(ctx)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]struct{}, len(desired)+len(ledger))
	for key := range desired {
		keys[key] = struct{}{}
	}
	for key := range ledger {
		keys[key] = struct{}{}
		considered[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	result := &TagReconcileResult{Checked: len(considered)}

	if reconcileNeedsRemote(ordered, desired, ledger) {
		preflight, err := s.CLI.Preflight(ctx)
		if err != nil {
			return nil, err
		}
		if compareVersion(preflight.Version, tagsMinimumZotioVersion) < 0 {
			return nil, fmt.Errorf("zotio %s does not support automatic exception tags (requires >= %s) — update zotio, or disable zotio.exception_tags", preflight.Version, tagsMinimumZotioVersion)
		}
	}

	var reconcileErrs []error
	for _, key := range ordered {
		want := desired[key]
		have, tracked := ledger[key]

		if tracked && have.Tag != want {
			switch have.Status {
			case tagStatusOwned, tagStatusPending:
				outcome, runErr := s.mutateTag(ctx, false, key, have.Tag)
				if outcome == "applied" {
					result.Removed++
				}
				if outcome == "applied" || outcome == "no_op" || isZotioNotFound(runErr) {
					if err := s.deleteTagState(ctx, key); err != nil {
						reconcileErrs = append(reconcileErrs, err)
						continue
					}
					tracked = false
				} else {
					reconcileErrs = append(reconcileErrs, fmt.Errorf("removing %s from %s: %w", have.Tag, key, runErr))
					continue
				}
				if runErr != nil {
					reconcileErrs = append(reconcileErrs, runErr)
				}
			default: // foreign or missing: papio owns no remote tag to remove.
				if err := s.deleteTagState(ctx, key); err != nil {
					reconcileErrs = append(reconcileErrs, err)
					continue
				}
				tracked = false
			}
		}
		if want == "" {
			continue
		}
		if tracked && have.Tag == want && have.Status != tagStatusPending {
			continue
		}

		wasPending := tracked && have.Tag == want && have.Status == tagStatusPending
		if err := s.upsertTagState(ctx, key, want, tagStatusPending); err != nil {
			reconcileErrs = append(reconcileErrs, err)
			continue
		}
		outcome, reason, runErr := s.mutateTagWithReason(ctx, true, key, want)
		switch outcome {
		case "applied":
			if err := s.upsertTagState(ctx, key, want, tagStatusOwned); err != nil {
				reconcileErrs = append(reconcileErrs, err)
				continue
			}
			result.Added++
		case "no_op":
			status := tagStatusForeign
			// A pending retry seeing the requested automatic type is recovery
			// from an ambiguous prior add, not ownership of a pre-existing tag.
			if wasPending && reason.TagTypes[want] == 1 {
				status = tagStatusOwned
			}
			if err := s.upsertTagState(ctx, key, want, status); err != nil {
				reconcileErrs = append(reconcileErrs, err)
				continue
			}
		default:
			if isZotioNotFound(runErr) {
				if err := s.upsertTagState(ctx, key, want, tagStatusMissing); err != nil {
					reconcileErrs = append(reconcileErrs, err)
				}
			}
		}
		if runErr != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("adding %s to %s: %w", want, key, runErr))
		}
	}
	return result, errors.Join(reconcileErrs...)
}

func reconcileNeedsRemote(keys []string, desired map[string]string, ledger map[string]tagLedgerState) bool {
	for _, key := range keys {
		want := desired[key]
		have, tracked := ledger[key]
		if tracked && have.Tag != want && (have.Status == tagStatusOwned || have.Status == tagStatusPending) {
			return true
		}
		if want != "" && (!tracked || have.Tag != want || have.Status == tagStatusPending) {
			return true
		}
	}
	return false
}

// desiredTags considers only links whose key was observed by a personal Zotio
// scan after schema v14. Zotero keys are library-local; pre-existing/group links
// without provenance must never be interpreted in My Library.
func (s *Service) desiredTags(ctx context.Context) (map[string]string, map[string]struct{}, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `
		SELECT w.zotio_item_key, j.state
		FROM jobs j
		JOIN work_requests w ON w.id = j.work_request_id
		JOIN zotio_item_scope z ON z.item_key = w.zotio_item_key AND z.scope = 'personal'
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
		if _, seen := latest[key]; !seen {
			latest[key] = state
		}
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

// keepMissingPDFs removes desired tags for items that now have an attachment
// (including a PDF supplied manually outside papio). The exact-key lookup is a
// bounded personal-library read; no remote writes occur when state is stable.
func (s *Service) keepMissingPDFs(ctx context.Context, desired map[string]string) (map[string]string, error) {
	if len(desired) == 0 {
		return desired, nil
	}
	reader, ok := s.CLI.(missingPDFKeyReader)
	if !ok {
		return nil, fmt.Errorf("configured Zotio client cannot inspect linked item attachments")
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	missing := make(map[string]struct{}, len(keys))
	for _, batch := range batchKeys(keys) {
		items, err := reader.MissingPDFKeys(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("checking linked Zotero attachments: %w", err)
		}
		for _, item := range items {
			missing[item.Key] = struct{}{}
		}
	}
	filtered := make(map[string]string, len(missing))
	for key, tag := range desired {
		if _, ok := missing[key]; ok {
			filtered[key] = tag
		}
	}
	return filtered, nil
}

func (s *Service) tagLedger(ctx context.Context) (map[string]tagLedgerState, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT item_key, tag, status FROM zotio_tag_state`)
	if err != nil {
		return nil, fmt.Errorf("reading tag ledger: %w", err)
	}
	defer rows.Close()
	ledger := map[string]tagLedgerState{}
	for rows.Next() {
		var key string
		var state tagLedgerState
		if err := rows.Scan(&key, &state.Tag, &state.Status); err != nil {
			return nil, err
		}
		ledger[key] = state
	}
	return ledger, rows.Err()
}

func (s *Service) deleteTagState(ctx context.Context, key string) error {
	if _, err := s.Store.DB().ExecContext(ctx, `DELETE FROM zotio_tag_state WHERE item_key = ?`, key); err != nil {
		return fmt.Errorf("clearing tag ledger: %w", err)
	}
	return nil
}

func (s *Service) upsertTagState(ctx context.Context, key, tag, status string) error {
	_, err := s.Store.DB().ExecContext(ctx, `
		INSERT INTO zotio_tag_state(item_key, tag, status, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(item_key) DO UPDATE SET tag = excluded.tag, status = excluded.status, updated_at = excluded.updated_at`,
		key, tag, status, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("recording tag ledger: %w", err)
	}
	return nil
}

func (s *Service) mutateTag(ctx context.Context, add bool, key, tag string) (string, error) {
	outcome, _, err := s.mutateTagWithReason(ctx, add, key, tag)
	return outcome, err
}

func (s *Service) mutateTagWithReason(ctx context.Context, add bool, key, tag string) (string, tagMutationReason, error) {
	args := []string{"--agent", "--yes", "--group=", "items", "tags"}
	if add {
		args = append(args, "add", "--automatic")
	} else {
		args = append(args, "remove", "--automatic-only")
	}
	args = append(args, "--tag", tag, key)
	raw, runErr := s.CLI.RunJSON(ctx, args...)
	var envelope tagMutationEnvelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil && runErr == nil {
			return "", tagMutationReason{}, fmt.Errorf("decoding zotio tag mutation: %w", err)
		}
	}
	var reason tagMutationReason
	if envelope.Result != nil && len(envelope.Result.Items) == 1 {
		item := envelope.Result.Items[0]
		if item.Key == key {
			if len(item.Reason) > 0 {
				_ = json.Unmarshal(item.Reason, &reason)
			}
			if item.Status != "applied" && item.Status != "no_op" && runErr == nil {
				runErr = fmt.Errorf("zotio tag mutation returned status %q", item.Status)
			}
			return item.Status, reason, runErr
		}
	}
	if runErr == nil {
		runErr = errors.New("zotio tag mutation returned no item result")
	}
	return "", reason, runErr
}

func isZotioNotFound(err error) bool {
	return err != nil && ErrorInfoFrom(err).HTTPStatus == 404
}

func batchKeys(keys []string) [][]string {
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

// TagReconciler adapts ReconcileTags to the daemon's maintenance seam with
// throttled error logging. Disabled configurations keep a runner only while
// ledger state remains to clean, so the default-off path adds no maintenance
// work and an off transition still converges.
type TagReconciler struct {
	service    *Service
	lastErr    string
	lastErrLog time.Time
}

func (s *Service) TagReconciler() *TagReconciler {
	if s == nil || s.CLI == nil || s.Store == nil {
		return nil
	}
	if !s.ExceptionTags {
		var exists int
		err := s.Store.DB().QueryRow(`SELECT 1 FROM zotio_tag_state LIMIT 1`).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
	}
	return &TagReconciler{service: s}
}

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
