// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventRecord is one decoded row of the append-only event stream. JobID is
// empty for a system event.
type EventRecord struct {
	Seq    int64
	JobID  string
	At     time.Time
	Kind   string
	Detail map[string]any
}

// EventsOfKind returns the newest events of one kind across every job and the
// system stream, newest first, at most limit rows. It is the paced drive's read
// model for its own opens and for provider cooldowns: both are small streams,
// and the events table is scanned rather than indexed by kind because it has
// no kind index and one pass a minute costs milliseconds.
func (js *Store) EventsOfKind(ctx context.Context, kind string, limit int) ([]EventRecord, error) {
	if kind == "" || limit <= 0 {
		return nil, errors.New("event kind and a positive limit are required")
	}
	return js.queryEvents(ctx, `SELECT seq, COALESCE(job_id,''), at, kind, detail_json FROM events
		WHERE kind = ? ORDER BY seq DESC LIMIT ?`, kind, limit)
}

// LatestSystemEvent returns the newest jobless event whose kind is one of
// kinds. The system rows are reached through the job_id index's NULL prefix.
func (js *Store) LatestSystemEvent(ctx context.Context, kinds ...string) (EventRecord, bool, error) {
	if len(kinds) == 0 {
		return EventRecord{}, false, errors.New("at least one event kind is required")
	}
	args := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		args = append(args, kind)
	}
	rows, err := js.queryEvents(ctx, `SELECT seq, '', at, kind, detail_json FROM events
		WHERE job_id IS NULL AND kind IN (?`+strings.Repeat(",?", len(kinds)-1)+`) ORDER BY seq DESC LIMIT 1`, args...)
	if err != nil || len(rows) == 0 {
		return EventRecord{}, false, err
	}
	return rows[0], true, nil
}

// JobEventsAfter returns one job's events with a sequence above afterSeq, in
// order.
func (js *Store) JobEventsAfter(ctx context.Context, jobID string, afterSeq int64) ([]EventRecord, error) {
	return js.queryEvents(ctx, `SELECT seq, COALESCE(job_id,''), at, kind, detail_json FROM events
		WHERE job_id = ? AND seq > ? ORDER BY seq ASC`, jobID, afterSeq)
}

// JobsWithEventHost returns every job with at least one event whose detail
// names host, compared case-insensitively. Provider outcomes, page captures and
// cooldowns record the lowercase host they observed; nothing else is matched.
func (js *Store) JobsWithEventHost(ctx context.Context, host string) ([]string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, nil
	}
	rows, err := js.S.DB().QueryContext(ctx, `SELECT DISTINCT job_id FROM events
		WHERE job_id IS NOT NULL AND lower(json_extract(detail_json,'$.host')) = ?`, host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LiveBrowserWork counts what is driving the operator's browser right now: a
// materialization claim in a live phase whose lease has not lapsed (an expired
// lease is reconciliation's to abandon, and must not strand a caller that
// waits on it), and every effect permit not yet settled. A permit in
// unknown_completion counts because papio cannot prove its provider action
// ended.
func (js *Store) LiveBrowserWork(ctx context.Context, now time.Time) (liveClaims, unsettledPermits int, err error) {
	rows, err := js.S.DB().QueryContext(ctx, `SELECT COALESCE(lease_until,'') FROM materialization_claims
		WHERE phase IN ('claimed','bound','route_issued','navigated')`)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var lease string
		if err := rows.Scan(&lease); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		until, parseErr := time.Parse(time.RFC3339Nano, lease)
		if lease == "" || parseErr != nil || until.After(now) {
			liveClaims++
		}
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	err = js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_permits WHERE status IN ('held','unknown_completion')`).Scan(&unsettledPermits)
	return liveClaims, unsettledPermits, err
}

func (js *Store) queryEvents(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	rows, err := js.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EventRecord
	for rows.Next() {
		var record EventRecord
		var at, detail string
		if err := rows.Scan(&record.Seq, &record.JobID, &at, &record.Kind, &detail); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("event %d has an unreadable time %q: %w", record.Seq, at, err)
		}
		record.At = parsed
		if err := json.Unmarshal([]byte(detail), &record.Detail); err != nil {
			return nil, fmt.Errorf("event %d has an unreadable detail: %w", record.Seq, err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
