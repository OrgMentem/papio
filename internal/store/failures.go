// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// FailureSummary is one operator-facing aggregate of terminal or parked jobs.
// Provider is the selected/latest candidate host, falling back to its source
// name when the candidate has no URL. ExampleJobID is the most recently updated
// job in the aggregate.
type FailureSummary struct {
	Provider     string `json:"provider"`
	Reason       string `json:"reason"`
	Count        int    `json:"count"`
	ExampleJobID string `json:"example_job_id"`
}

const (
	FailureSummaryLimitDefault = 20
	FailureSummaryLimitMax     = 200
)

// EffectiveFailureSummaryLimit resolves the public failures page limit.
func EffectiveFailureSummaryLimit(limit int) int {
	switch {
	case limit == 0:
		return FailureSummaryLimitDefault
	case limit < 1:
		return 1
	case limit > FailureSummaryLimitMax:
		return FailureSummaryLimitMax
	default:
		return limit
	}
}

// FailureSummaries aggregates unavailable and awaiting-human jobs using their
// most recent decisive event. By default the aggregate key is the reason; when
// byProvider is true it is the candidate host/source. The non-key column is
// reported as "multiple" when an aggregate spans more than one value.
func (s *Store) FailureSummaries(ctx context.Context, limit int, byProvider bool) ([]FailureSummary, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, j.updated_at, COALESCE(j.terminal_reason, ''),
		       COALESCE(c.source, ''), COALESCE(c.url_redacted, ''),
		       COALESCE(e.kind, ''), COALESCE(e.detail_json, '{}')
		FROM jobs j
		LEFT JOIN candidates c ON c.id = COALESCE(
			j.selected_candidate_id,
			(SELECT c2.id FROM candidates c2
			 WHERE c2.job_id = j.id
			 ORDER BY c2.created_at DESC, c2.id DESC LIMIT 1)
		)
		LEFT JOIN events e ON e.seq = (
			SELECT e2.seq FROM events e2
			WHERE e2.job_id = j.id
			  AND e2.kind IN ('job.transition', 'browser.provider_outcome', 'browser.error')
			ORDER BY e2.seq DESC LIMIT 1
		)
		WHERE j.state IN ('unavailable', 'awaiting_human')`)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	type aggregate struct {
		FailureSummary
		exampleUpdatedAt string
	}
	groups := make(map[string]*aggregate)
	for rows.Next() {
		var jobID, updatedAt, terminalReason, source, candidateURL, eventKind, detailJSON string
		if err := rows.Scan(&jobID, &updatedAt, &terminalReason, &source, &candidateURL, &eventKind, &detailJSON); err != nil {
			return nil, false, err
		}
		reason, err := decisiveFailureReason(eventKind, detailJSON)
		if err != nil {
			return nil, false, fmt.Errorf("decoding decisive event for job %s: %w", jobID, err)
		}
		if reason == "" {
			reason = strings.TrimSpace(terminalReason)
		}
		if reason == "" {
			reason = "unknown"
		}
		provider := failureProviderHost(candidateURL, source)

		key := reason
		if byProvider {
			key = provider
		}
		group := groups[key]
		if group == nil {
			group = &aggregate{
				FailureSummary: FailureSummary{
					Provider:     provider,
					Reason:       reason,
					ExampleJobID: jobID,
				},
				exampleUpdatedAt: updatedAt,
			}
			groups[key] = group
		} else if byProvider {
			if group.Reason != reason {
				group.Reason = "multiple"
			}
		} else if group.Provider != provider {
			group.Provider = "multiple"
		}
		if updatedAt > group.exampleUpdatedAt || (updatedAt == group.exampleUpdatedAt && jobID > group.ExampleJobID) {
			group.ExampleJobID = jobID
			group.exampleUpdatedAt = updatedAt
		}
		group.Count++
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	out := make([]FailureSummary, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.FailureSummary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].ExampleJobID < out[j].ExampleJobID
	})
	effective := EffectiveFailureSummaryLimit(limit)
	truncated := len(out) > effective
	if truncated {
		out = out[:effective]
	}
	return out, truncated, nil
}

func decisiveFailureReason(kind, detailJSON string) (string, error) {
	var detail map[string]any
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		return "", err
	}
	key := ""
	switch kind {
	case "job.transition":
		key = "reason"
	case "browser.provider_outcome":
		key = "outcome"
	case "browser.error":
		key = "code"
	}
	value, _ := detail[key].(string)
	return strings.TrimSpace(value), nil
}

func failureProviderHost(rawURL, source string) string {
	if parsed, err := url.Parse(rawURL); err == nil {
		if host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), "."); host != "" {
			return host
		}
	}
	if source = strings.TrimSpace(source); source != "" {
		return source
	}
	return "unknown"
}
