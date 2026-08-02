// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ActivityEntry is one durable event joined with the current job read model.
//
// Events are append-only, while the title and state are read at query time so
// the activity feed can identify a job without duplicating mutable job data in
// every event row.
type ActivityEntry struct {
	Seq      int64          `json:"seq"`
	At       time.Time      `json:"at"`
	JobID    string         `json:"job_id"`
	Kind     string         `json:"kind"`
	Detail   map[string]any `json:"detail"`
	JobTitle string         `json:"title"`
	JobState string         `json:"job_state"`
}

// ActivityLimitMax and ActivityLimitDefault bound the activity feed. A
// non-positive limit means the caller did not specify one; an over-large limit
// is clamped down rather than reset to the default so asking for more never
// returns fewer rows.
const (
	ActivityLimitMax     = 200
	ActivityLimitDefault = 30
)

// EffectiveActivityLimit resolves a caller-supplied activity limit.
func EffectiveActivityLimit(limit int) int {
	if limit <= 0 {
		return ActivityLimitDefault
	}
	if limit > ActivityLimitMax {
		return ActivityLimitMax
	}
	return limit
}

// RecentEvents returns durable activity newest-first. When beforeSeq is
// positive, only rows with a lower sequence number are returned; non-positive
// values start at the newest event. Events without a job retain empty title,
// state, and job ID fields.
func (s *Store) RecentEvents(limit int, beforeSeq int64) ([]ActivityEntry, error) {
	return s.recentEvents(EffectiveActivityLimit(limit), beforeSeq, "")
}

// RecentEventsForJob is the job-filtered activity variant used by the RPC
// read model. It has the same ordering, cursor, and limit semantics as
// RecentEvents.
func (s *Store) RecentEventsForJob(limit int, beforeSeq int64, jobID string) ([]ActivityEntry, error) {
	return s.recentEvents(EffectiveActivityLimit(limit), beforeSeq, jobID)
}

// RecentEventsPage returns one bounded activity page and a proof that another
// row exists beyond it. It is used by activity.list, whose response must report
// truncation even when the requested limit is the store maximum.
func (s *Store) RecentEventsPage(limit int, beforeSeq int64, jobID string) ([]ActivityEntry, bool, error) {
	effective := EffectiveActivityLimit(limit)
	rows, err := s.recentEvents(effective+1, beforeSeq, jobID)
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > effective
	if truncated {
		rows = rows[:effective]
	}
	return rows, truncated, nil
}

func (s *Store) recentEvents(limit int, beforeSeq int64, jobID string) ([]ActivityEntry, error) {
	query := `
		SELECT e.seq, e.at, e.job_id, e.kind, e.detail_json,
		       COALESCE(wr.title, ''), COALESCE(j.state, '')
		FROM events e
		LEFT JOIN jobs j ON j.id = e.job_id
		LEFT JOIN work_requests wr ON wr.id = j.work_request_id
		WHERE 1 = 1`
	args := make([]any, 0, 3)
	if beforeSeq > 0 {
		query += " AND e.seq < ?"
		args = append(args, beforeSeq)
	}
	if jobID != "" {
		query += " AND e.job_id = ?"
		args = append(args, jobID)
	}
	query += " ORDER BY e.seq DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	entries := make([]ActivityEntry, 0, limit)
	for rows.Next() {
		var (
			entry     ActivityEntry
			atText    string
			jobIDText sql.NullString
			detailRaw string
		)
		if err := rows.Scan(&entry.Seq, &atText, &jobIDText, &entry.Kind, &detailRaw, &entry.JobTitle, &entry.JobState); err != nil {
			return nil, err
		}
		if jobIDText.Valid {
			entry.JobID = jobIDText.String
		}
		entry.At, err = time.Parse(time.RFC3339Nano, atText)
		if err != nil {
			return nil, fmt.Errorf("parsing event %d timestamp: %w", entry.Seq, err)
		}
		if err := json.Unmarshal([]byte(detailRaw), &entry.Detail); err != nil {
			return nil, fmt.Errorf("decoding event %d detail: %w", entry.Seq, err)
		}
		if entry.Detail == nil {
			entry.Detail = map[string]any{}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
