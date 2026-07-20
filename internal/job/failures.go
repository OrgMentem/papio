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
		WHERE j.state IN ('failed', 'unavailable', 'needs_review', 'awaiting_human')`
	rows, err := js.S.DB().QueryContext(ctx, query)
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
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
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
