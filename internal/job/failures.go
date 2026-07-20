// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

const (
	failureDefaultLimit = 50
	failureMaxLimit     = 200
	failureReasonLimit  = 80
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
		limit = failureDefaultLimit
	} else if limit < 1 {
		limit = 1
	} else if limit > failureMaxLimit {
		limit = failureMaxLimit
	}

	query := `
		WITH failure_jobs AS (
			SELECT j.id, j.state, j.updated_at,
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
			  AND (? = '' OR j.updated_at >= ?)
		), ranked AS (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY state, candidate_url, detail_json
				ORDER BY updated_at DESC, id DESC
			) AS sample_rank
			FROM failure_jobs
		)
		SELECT state, candidate_url, detail_json, COUNT(*),
		       MAX(CASE WHEN sample_rank = 1 THEN id END),
		       MAX(updated_at)
		FROM ranked
		GROUP BY state, candidate_url, detail_json`
	cutoff := ""
	if !since.IsZero() {
		cutoff = since.UTC().Format(time.RFC3339Nano)
	}
	rows, err := js.S.DB().QueryContext(ctx, query, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type aggregate struct {
		FailureGroup
		sampleUpdatedAt string
	}
	groups := make(map[string]*aggregate)
	for rows.Next() {
		var state, candidateURL, detailJSON, id, updatedAt string
		var count int
		if err := rows.Scan(&state, &candidateURL, &detailJSON, &count, &id, &updatedAt); err != nil {
			return nil, err
		}
		provider := failureProvider(candidateURL)
		reason := failureReason(detailJSON)
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
		} else if updatedAt > group.sampleUpdatedAt || (updatedAt == group.sampleUpdatedAt && id > group.Sample) {
			group.Sample, group.sampleUpdatedAt = id, updatedAt
		}
		group.Count += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FailureGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.FailureGroup)
	}
	sortFailureGroups(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func failureProvider(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "-"
	}
	return parsed.Hostname()
}

func failureReason(detailJSON string) string {
	var detail map[string]any
	if json.Unmarshal([]byte(detailJSON), &detail) != nil {
		return "-"
	}
	reason, ok := detail["reason"].(string)
	if !ok {
		return "-"
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "-"
	}
	runes := []rune(reason)
	if len(runes) > failureReasonLimit {
		reason = string(runes[:failureReasonLimit])
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
