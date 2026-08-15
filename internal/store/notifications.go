// Copyright 2026 OrgMentem. Licensed under MIT.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// NotificationRecord is the store-side representation of one durable
// notification intent. The notify package adapts this type without introducing
// a store -> notify import cycle.
type NotificationRecord struct {
	ID                                                        int64
	Category                                                  string
	EventKind                                                 string
	AggregateKey                                              string
	Phase                                                     string
	WindowStart                                               time.Time
	JobID                                                     string
	BatchID                                                   string
	ScanID                                                    string
	PayloadJSON                                               string
	FirstAt                                                   time.Time
	LastAt                                                    time.Time
	AvailableAt                                               time.Time
	Count                                                     int
	DesktopState                                              string
	WebhookState                                              string
	DesktopReservedAt, DesktopAttemptedAt, WebhookAttemptedAt time.Time
}

// NotificationLedger owns the durable notification outbox and its desktop
// reservation budget. It is intentionally independent of notification policy.
type NotificationLedger struct{ s *Store }

func (s *Store) Notifications() *NotificationLedger { return &NotificationLedger{s: s} }

func formatNotificationTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseNotificationTime(rowID int64, column string, text sql.NullString) (time.Time, error) {
	if !text.Valid || text.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, text.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("notification %d: %s %q: %w", rowID, column, text.String, err)
	}
	return t, nil
}

// Upsert merges an intent by its five-part desktop identity. Coalesced rows
// remain mutable only while their desktop leg is nonterminal; an attempted
// or otherwise terminal leg is an immutable audit record.
func (l *NotificationLedger) Upsert(ctx context.Context, rec NotificationRecord) (NotificationRecord, error) {
	if l == nil || l.s == nil || l.s.db == nil {
		return NotificationRecord{}, fmt.Errorf("notification ledger is unavailable")
	}
	if rec.PayloadJSON == "" {
		rec.PayloadJSON = "{}"
	}
	if !json.Valid([]byte(rec.PayloadJSON)) {
		return NotificationRecord{}, fmt.Errorf("notification payload is not valid JSON")
	}
	first := rec.FirstAt
	if first.IsZero() {
		first = rec.LastAt
	}
	if first.IsZero() {
		first = time.Now().UTC()
	}
	last := rec.LastAt
	if last.IsZero() {
		last = first
	}
	available := rec.AvailableAt
	if available.IsZero() {
		available = last
	}
	rec.FirstAt, rec.LastAt, rec.AvailableAt = first, last, available
	_, err := l.s.db.ExecContext(ctx, `
		INSERT INTO notification_intents
		(category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,
		 payload_json,first_at,last_at,count,available_at,desktop_state,webhook_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(category,event_kind,aggregate_key,phase,window_start) DO UPDATE SET
			last_at=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN excluded.last_at ELSE notification_intents.last_at END,
			count=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN notification_intents.count+excluded.count ELSE notification_intents.count END,
			payload_json=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN
				CASE WHEN json_type(json_extract(excluded.payload_json,'$.count')) IN ('integer','real')
					THEN json_set(json_patch(notification_intents.payload_json, excluded.payload_json),'$.count',
						notification_intents.count+excluded.count)
					ELSE json_patch(notification_intents.payload_json, excluded.payload_json) END
				ELSE notification_intents.payload_json END,
			available_at=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN excluded.available_at ELSE notification_intents.available_at END`,
		rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, formatNotificationTime(rec.WindowStart),
		nullIfEmpty(rec.JobID), nullIfEmpty(rec.BatchID), nullIfEmpty(rec.ScanID), rec.PayloadJSON,
		formatNotificationTime(rec.FirstAt), formatNotificationTime(rec.LastAt), maxInt(rec.Count, 1),
		formatNotificationTime(rec.AvailableAt), defaultDesktopState(rec.DesktopState), defaultWebhookState(rec.WebhookState))
	if err != nil {
		return NotificationRecord{}, err
	}
	return l.getByIdentity(ctx, rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, rec.WindowStart)
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func maxInt(value, fallback int) int {
	if value < fallback {
		return fallback
	}
	return value
}

func defaultDesktopState(value string) string {
	if value == "" {
		return "pending"
	}
	return value
}
func defaultWebhookState(value string) string {
	if value == "" {
		return "pending"
	}
	return value
}

func (l *NotificationLedger) getByIdentity(ctx context.Context, category, eventKind, aggregate, phase string, window time.Time) (NotificationRecord, error) {
	row := l.s.db.QueryRowContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,
		job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,
		webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at
		FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
		category, eventKind, aggregate, phase, formatNotificationTime(window))
	return scanNotification(row)
}

func scanNotification(row *sql.Row) (NotificationRecord, error) {
	var r NotificationRecord
	var window, first, last, available sql.NullString
	var jobID, batchID, scanID sql.NullString
	var reserved, attempted, webhook sql.NullString
	err := row.Scan(&r.ID, &r.Category, &r.EventKind, &r.AggregateKey, &r.Phase, &window,
		&jobID, &batchID, &scanID, &r.PayloadJSON, &first, &last, &available, &r.Count,
		&r.DesktopState, &r.WebhookState, &reserved, &attempted, &webhook)
	if err != nil {
		return NotificationRecord{}, err
	}
	if r.WindowStart, err = parseNotificationTime(r.ID, "window_start", window); err != nil {
		return NotificationRecord{}, err
	}
	if r.FirstAt, err = parseNotificationTime(r.ID, "first_at", first); err != nil {
		return NotificationRecord{}, err
	}
	if r.LastAt, err = parseNotificationTime(r.ID, "last_at", last); err != nil {
		return NotificationRecord{}, err
	}
	if r.AvailableAt, err = parseNotificationTime(r.ID, "available_at", available); err != nil {
		return NotificationRecord{}, err
	}
	if r.DesktopReservedAt, err = parseNotificationTime(r.ID, "desktop_reserved_at", reserved); err != nil {
		return NotificationRecord{}, err
	}
	if r.DesktopAttemptedAt, err = parseNotificationTime(r.ID, "desktop_attempted_at", attempted); err != nil {
		return NotificationRecord{}, err
	}
	if r.WebhookAttemptedAt, err = parseNotificationTime(r.ID, "webhook_attempted_at", webhook); err != nil {
		return NotificationRecord{}, err
	}
	if jobID.Valid {
		r.JobID = jobID.String
	}
	if batchID.Valid {
		r.BatchID = batchID.String
	}
	if scanID.Valid {
		r.ScanID = scanID.String
	}
	return r, nil
}

type notificationScanner interface{ Scan(...any) error }

func scanNotificationRow(row notificationScanner) (NotificationRecord, error) {
	var r NotificationRecord
	var window, first, last, available sql.NullString
	var jobID, batchID, scanID sql.NullString
	var reserved, attempted, webhook sql.NullString
	err := row.Scan(&r.ID, &r.Category, &r.EventKind, &r.AggregateKey, &r.Phase, &window,
		&jobID, &batchID, &scanID, &r.PayloadJSON, &first, &last, &available, &r.Count,
		&r.DesktopState, &r.WebhookState, &reserved, &attempted, &webhook)
	if err != nil {
		return NotificationRecord{}, err
	}
	var parseErr error
	if r.WindowStart, parseErr = parseNotificationTime(r.ID, "window_start", window); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.FirstAt, parseErr = parseNotificationTime(r.ID, "first_at", first); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.LastAt, parseErr = parseNotificationTime(r.ID, "last_at", last); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.AvailableAt, parseErr = parseNotificationTime(r.ID, "available_at", available); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.DesktopReservedAt, parseErr = parseNotificationTime(r.ID, "desktop_reserved_at", reserved); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.DesktopAttemptedAt, parseErr = parseNotificationTime(r.ID, "desktop_attempted_at", attempted); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if r.WebhookAttemptedAt, parseErr = parseNotificationTime(r.ID, "webhook_attempted_at", webhook); parseErr != nil {
		return NotificationRecord{}, parseErr
	}
	if jobID.Valid {
		r.JobID = jobID.String
	}
	if batchID.Valid {
		r.BatchID = batchID.String
	}
	if scanID.Valid {
		r.ScanID = scanID.String
	}
	return r, nil
}

// DueDesktop returns due pending and held desktop legs in FIFO order.
func (l *NotificationLedger) DueDesktop(ctx context.Context, now time.Time, limit int) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.s.db.QueryContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,
		job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,
		webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at
		FROM notification_intents WHERE desktop_state IN ('pending','held') AND available_at <= ?
		ORDER BY available_at,id LIMIT ?`, formatNotificationTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationRecord, 0, limit)
	for rows.Next() {
		r, err := scanNotificationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReserveDesktop atomically consumes one rolling-hour slot and marks a due
// leg reserved. Reserved rows are deliberately never eligible for replay.
func (l *NotificationLedger) ReserveDesktop(ctx context.Context, id int64, now time.Time, maxPerHour int) (bool, error) {
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	rollback := func(e error) (bool, error) { tx.Rollback(); return false, e }
	if maxPerHour > 0 {
		cutoff := now.Add(-time.Hour)
		var used int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_intents
			WHERE (desktop_state='reserved' AND desktop_reserved_at >= ?)
			   OR (desktop_state='attempted' AND desktop_attempted_at >= ?)`, formatNotificationTime(cutoff), formatNotificationTime(cutoff)).Scan(&used)
		if err != nil {
			return rollback(err)
		}
		if used >= maxPerHour {
			tx.Rollback()
			return false, nil
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_intents SET desktop_state='reserved', desktop_reserved_at=?
		WHERE id=? AND desktop_state IN ('pending','held') AND available_at <= ?`, formatNotificationTime(now), id, formatNotificationTime(now))
	if err != nil {
		return rollback(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return rollback(err)
	}
	if changed != 1 {
		tx.Rollback()
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (l *NotificationLedger) SetDesktopState(ctx context.Context, id int64, state string, now time.Time) error {
	var query string
	switch state {
	case "attempted":
		query = `UPDATE notification_intents SET desktop_state=?, desktop_attempted_at=? WHERE id=? AND desktop_state='reserved'`
	case "reserved":
		query = `UPDATE notification_intents SET desktop_state=?, desktop_reserved_at=? WHERE id=?`
	default:
		query = `UPDATE notification_intents SET desktop_state=? WHERE id=?`
	}
	var args []any
	if state == "attempted" || state == "reserved" {
		args = []any{state, formatNotificationTime(now), id}
	} else {
		args = []any{state, id}
	}
	_, err := l.s.db.ExecContext(ctx, query, args...)
	return err
}

func (l *NotificationLedger) SetWebhookState(ctx context.Context, id int64, state string, now time.Time) error {
	if state == "attempted" {
		_, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET webhook_state=?, webhook_attempted_at=? WHERE id=?`, state, formatNotificationTime(now), id)
		return err
	}
	_, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET webhook_state=? WHERE id=?`, state, id)
	return err
}

func (l *NotificationLedger) SupersedeCheckpoints(ctx context.Context, aggregateKey string, now time.Time) (int, error) {
	result, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET desktop_state='superseded'
		WHERE aggregate_key=? AND category='completion_batch' AND phase='checkpoint' AND desktop_state IN ('pending','held')`, aggregateKey)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// SupersedeAndUpsertCheckpoint performs checkpoint supersession and final
// insertion in one transaction. A process crash cannot cancel the previous
// checkpoint without also creating the replacement final intent.
func (l *NotificationLedger) SupersedeAndUpsertCheckpoint(ctx context.Context, aggregateKey string, now time.Time, rec NotificationRecord) (NotificationRecord, error) {
	if l == nil || l.s == nil || l.s.db == nil {
		return NotificationRecord{}, fmt.Errorf("notification ledger is unavailable")
	}
	if rec.PayloadJSON == "" {
		rec.PayloadJSON = "{}"
	}
	if !json.Valid([]byte(rec.PayloadJSON)) {
		return NotificationRecord{}, fmt.Errorf("notification payload is not valid JSON")
	}
	first := rec.FirstAt
	if first.IsZero() {
		first = rec.LastAt
	}
	if first.IsZero() {
		first = now.UTC()
	}
	last := rec.LastAt
	if last.IsZero() {
		last = first
	}
	available := rec.AvailableAt
	if available.IsZero() {
		available = last
	}
	rec.FirstAt, rec.LastAt, rec.AvailableAt = first, last, available
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return NotificationRecord{}, err
	}
	rollback := func(err error) (NotificationRecord, error) {
		tx.Rollback()
		return NotificationRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_intents SET desktop_state='superseded'
		WHERE aggregate_key=? AND category='completion_batch' AND phase='checkpoint'
		  AND desktop_state IN ('pending','held')`, aggregateKey); err != nil {
		return rollback(err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO notification_intents
		(category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,
		 payload_json,first_at,last_at,count,available_at,desktop_state,webhook_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(category,event_kind,aggregate_key,phase,window_start) DO UPDATE SET
			last_at=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN excluded.last_at ELSE notification_intents.last_at END,
			count=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN notification_intents.count+excluded.count ELSE notification_intents.count END,
			payload_json=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN excluded.payload_json ELSE notification_intents.payload_json END,
			available_at=CASE WHEN notification_intents.desktop_state IN ('pending','held') THEN excluded.available_at ELSE notification_intents.available_at END`,
		rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, formatNotificationTime(rec.WindowStart),
		nullIfEmpty(rec.JobID), nullIfEmpty(rec.BatchID), nullIfEmpty(rec.ScanID), rec.PayloadJSON,
		formatNotificationTime(rec.FirstAt), formatNotificationTime(rec.LastAt), maxInt(rec.Count, 1),
		formatNotificationTime(rec.AvailableAt), defaultDesktopState(rec.DesktopState), defaultWebhookState(rec.WebhookState))
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return NotificationRecord{}, err
	}
	return l.getByIdentity(ctx, rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, rec.WindowStart)
}

// DueWebhook returns digest legs whose shared availability window has elapsed.
func (l *NotificationLedger) DueWebhook(ctx context.Context, now time.Time, limit int) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.s.db.QueryContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,
		job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,
		webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at
		FROM notification_intents WHERE webhook_state='pending' AND available_at <= ?
		ORDER BY available_at,id LIMIT ?`, formatNotificationTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationRecord, 0, limit)
	for rows.Next() {
		r, err := scanNotificationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (l *NotificationLedger) LatestCheckpoint(ctx context.Context, aggregateKey string) (NotificationRecord, bool, error) {
	row := l.s.db.QueryRowContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,
		job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,
		webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at
		FROM notification_intents WHERE aggregate_key=? AND category='completion_batch' AND phase='checkpoint'
		ORDER BY id DESC LIMIT 1`, aggregateKey)
	r, err := scanNotification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationRecord{}, false, nil
	}
	return r, err == nil, err
}

// SetDesktopAvailable defers a held leg without changing its terminal
// disposition. It is kept out of notify.Ledger so alternate ledger
// implementations can adopt their own scheduling strategy.
func (l *NotificationLedger) SetDesktopAvailable(ctx context.Context, id int64, available time.Time) error {
	_, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET available_at=? WHERE id=? AND desktop_state='held'`, formatNotificationTime(available), id)
	return err
}

// HeldCount reports desktop notification legs waiting for a quiet-window or
// rate-budget release. It is intentionally a ledger read rather than an
// Activity-derived estimate.
func (l *NotificationLedger) HeldCount(ctx context.Context) (int, error) {
	if l == nil || l.s == nil || l.s.db == nil {
		return 0, fmt.Errorf("notification ledger is unavailable")
	}
	var count int
	if err := l.s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_intents WHERE desktop_state = 'held'`,
	).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
