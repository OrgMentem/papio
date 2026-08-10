// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/incident"
)

// FailuresLimitMax and FailuresLimitDefault bound Store.Failures's limit
// parameter (see the clamp below). Exported so internal/cli can compute the
// same effective limit the daemon will actually use — see
// internal/agentjson.Capped and its callers.
const (
	FailuresLimitDefault = 50
	FailuresLimitMax     = 200
	failureReasonLimit   = 80
	failureCutoffPad     = 5 * time.Second
)

// FailureGroup describes a recurring acquisition outcome and a recent example.
type FailureGroup struct {
	State    string `json:"state"`
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
	Sample   string `json:"sample"`
}

// Failures groups jobs that did not complete without intervention.
func (js *Store) Failures(ctx context.Context, since time.Time, limit int) ([]FailureGroup, error) {
	if limit == 0 {
		limit = FailuresLimitDefault
	} else if limit < 1 {
		limit = 1
	} else if limit > FailuresLimitMax {
		limit = FailuresLimitMax
	}

	query := `
		SELECT j.id, j.state, j.updated_at, COALESCE(j.terminal_reason, ''),
		       COALESCE(
		         (SELECT c.url_redacted FROM candidates c WHERE c.id = j.selected_candidate_id),
		         (SELECT c.url_redacted FROM candidates c WHERE c.job_id = j.id ORDER BY c.created_at DESC, c.id DESC LIMIT 1),
		         ''
		       ) AS candidate_url,
		       COALESCE(
		         (SELECT e.detail_json FROM events e
		          WHERE e.job_id = j.id AND e.kind = 'job.transition'
		          ORDER BY e.seq DESC LIMIT 1),
		         '{}'
		       ) AS detail_json
		FROM jobs j
		WHERE j.state IN ('failed', 'unavailable', 'needs_review', 'awaiting_human')
		  AND (? = '' OR julianday(j.updated_at) >= julianday(?))`
	coarseCutoff := ""
	if !since.IsZero() {
		coarseCutoff = since.Add(-failureCutoffPad).UTC().Format(time.RFC3339Nano)
	}
	rows, err := js.S.DB().QueryContext(ctx, query, coarseCutoff, coarseCutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type aggregate struct {
		FailureGroup
		sampleUpdatedAt time.Time
	}
	groups := make(map[string]*aggregate)
	for rows.Next() {
		var state, candidateURL, detailJSON, terminalReason, id, updatedAtRaw string
		if err := rows.Scan(&id, &state, &updatedAtRaw, &terminalReason, &candidateURL, &detailJSON); err != nil {
			return nil, err
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
		if err != nil {
			return nil, err
		}
		if !since.IsZero() && updatedAt.Before(since) {
			continue
		}
		provider := failureProvider(candidateURL)
		reason := failureReason(detailJSON)
		if reason == "" {
			reason = normalizeFailureReason(terminalReason)
		}
		if reason == "" {
			reason = "-"
		}
		key := state + "\x00" + provider + "\x00" + reason
		group := groups[key]
		if group == nil {
			group = &aggregate{FailureGroup: FailureGroup{
				State:    state,
				Provider: provider,
				Reason:   reason,
				Sample:   id,
			}, sampleUpdatedAt: updatedAt}
			groups[key] = group
		} else if updatedAt.After(group.sampleUpdatedAt) || (updatedAt.Equal(group.sampleUpdatedAt) && id > group.Sample) {
			group.Sample, group.sampleUpdatedAt = id, updatedAt
		}
		group.Count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FailureGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.FailureGroup)
	}
	sortFailureGroups(out)
	for i := range out {
		out[i].Reason = displayFailureReason(out[i].Reason)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// IncidentFailures groups terminal and parked jobs by the installation-keyed
// failure shape. The key is loaded from the same data directory as papio.db,
// so fingerprints remain local and stable across daemon restarts.
func (js *Store) IncidentFailures(ctx context.Context, since time.Time, limit int) ([]incident.Group, error) {
	key, err := incident.LoadOrCreateKey(filepath.Dir(js.S.Path()))
	if err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = FailuresLimitDefault
	}
	if limit > FailuresLimitMax {
		limit = FailuresLimitMax
	}
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT id, state, updated_at
		FROM jobs
		WHERE state IN ('failed', 'unavailable', 'needs_review', 'awaiting_human', 'retry_wait', 'cancelled')
		  AND (? = '' OR julianday(updated_at) >= julianday(?))`,
		func() string {
			if since.IsZero() {
				return ""
			}
			return since.Add(-failureCutoffPad).UTC().Format(time.RFC3339Nano)
		}(), func() string {
			if since.IsZero() {
				return ""
			}
			return since.Add(-failureCutoffPad).UTC().Format(time.RFC3339Nano)
		}())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	observations := make([]incident.JobObservation, 0)
	for rows.Next() {
		var id, state, updatedRaw string
		if err := rows.Scan(&id, &state, &updatedRaw); err != nil {
			return nil, err
		}
		updated, err := time.Parse(time.RFC3339Nano, updatedRaw)
		if err != nil {
			return nil, err
		}
		if !since.IsZero() && updated.Before(since) {
			continue
		}
		events, err := js.Events(ctx, id)
		if err != nil {
			return nil, err
		}
		observations = append(observations, incident.JobObservation{JobID: id, State: state, UpdatedAt: updated, Events: events})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groups := incident.Aggregate(key, observations)
	if len(groups) > limit {
		groups = groups[:limit]
	}
	return groups, nil
}

// FailureGroupCount returns the number of distinct recent failure groups from
// an existing read transaction. It keeps triage counts on the same SQLite
// snapshot as its inbox items.
func (js *Store) FailureGroupCount(ctx context.Context, tx *sql.Tx, since time.Time) (int, error) {
	if tx == nil {
		return 0, errors.New("failure group count requires a transaction")
	}
	coarseCutoff := ""
	if !since.IsZero() {
		coarseCutoff = since.Add(-failureCutoffPad).UTC().Format(time.RFC3339Nano)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, j.state, j.updated_at, COALESCE(j.terminal_reason, ''),
		       COALESCE(
		         (SELECT c.url_redacted FROM candidates c WHERE c.id = j.selected_candidate_id),
		         (SELECT c.url_redacted FROM candidates c WHERE c.job_id = j.id ORDER BY c.created_at DESC, c.id DESC LIMIT 1),
		         ''
		       ) AS candidate_url,
		       COALESCE(
		         (SELECT e.detail_json FROM events e
		          WHERE e.job_id = j.id AND e.kind = 'job.transition'
		          ORDER BY e.seq DESC LIMIT 1),
		         '{}'
		       ) AS detail_json
		FROM jobs j
		WHERE j.state IN ('failed', 'unavailable', 'needs_review', 'awaiting_human')
		  AND (? = '' OR julianday(j.updated_at) >= julianday(?))`, coarseCutoff, coarseCutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	groups := make(map[string]struct{})
	for rows.Next() {
		var state, candidateURL, detailJSON, terminalReason, id, updatedAtRaw string
		if err := rows.Scan(&id, &state, &updatedAtRaw, &terminalReason, &candidateURL, &detailJSON); err != nil {
			return 0, err
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
		if err != nil {
			return 0, err
		}
		if !since.IsZero() && updatedAt.Before(since) {
			continue
		}
		reason := failureReason(detailJSON)
		if reason == "" {
			reason = normalizeFailureReason(terminalReason)
		}
		if reason == "" {
			reason = "-"
		}
		groups[state+"\x00"+failureProvider(candidateURL)+"\x00"+reason] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return len(groups), nil
}

func failureProvider(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "-"
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return "-"
	}
	return host
}

func failureReason(detailJSON string) string {
	var detail map[string]any
	if json.Unmarshal([]byte(detailJSON), &detail) != nil {
		return ""
	}
	reason, ok := detail["reason"].(string)
	if !ok {
		return ""
	}
	return normalizeFailureReason(reason)
}

func normalizeFailureReason(reason string) string {
	return strings.TrimSpace(reason)
}

func displayFailureReason(reason string) string {
	runes := []rune(reason)
	if len(runes) > failureReasonLimit {
		return string(runes[:failureReasonLimit])
	}
	return reason
}

func sortFailureGroups(groups []FailureGroup) {
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && failureGroupBefore(groups[j], groups[j-1]); j-- {
			groups[j], groups[j-1] = groups[j-1], groups[j]
		}
	}
}

func failureGroupBefore(left, right FailureGroup) bool {
	if left.Count != right.Count {
		return left.Count > right.Count
	}
	if left.State != right.State {
		return left.State < right.State
	}
	if left.Provider != right.Provider {
		return left.Provider < right.Provider
	}
	return left.Reason < right.Reason
}
