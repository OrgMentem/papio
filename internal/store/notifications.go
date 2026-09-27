// Copyright 2026 OrgMentem. Licensed under MIT.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// DesktopSentCount and DesktopSentPayloadJSON record what the desktop leg
	// actually delivered, captured when it left the reserved state. They are
	// zero for a leg that never sent anything, so a reader falls back to the
	// live count and payload.
	DesktopSentCount       int
	DesktopSentPayloadJSON string
}

// NotificationLedger owns the durable notification outbox and its desktop
// reservation budget. It is intentionally independent of notification policy.
type NotificationLedger struct{ s *Store }

func (s *Store) Notifications() *NotificationLedger { return &NotificationLedger{s: s} }

func formatNotificationTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return FormatTime(t)
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

// notificationColumns is the shared read projection. Every reader scans it in
// this order through scanNotificationRow.
const notificationColumns = `id,category,event_kind,aggregate_key,phase,window_start,
	job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,
	webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at,
	desktop_sent_count,desktop_sent_payload_json`

// notificationCoalescable gates every ON CONFLICT merge. One row carries one
// shared count and payload for both legs, so it stays mutable while either
// leg can still deliver it: the desktop leg is nonterminal, or the webhook
// leg is still pending. A desktop leg that already delivered keeps its own
// snapshot of what it sent, so later merging cannot rewrite that audit.
// A claimed or settled (non-pending) webhook row is an immutable snapshot
// for its delivery key: UpsertWebhookGeneration redirects a merge that
// lands after the claim to a new generation row instead of rewriting the
// claimed payload.
const notificationCoalescable = `(notification_intents.desktop_state IN ('pending','held') OR notification_intents.webhook_state='pending')`

// plain Upsert merges an intent by its five-part desktop identity. Rows merge
// only while the desktop leg can still deliver (pending/held): the desktop
// leg owns coalescing, and the desktopSent snapshot preserves what it
// delivered. The webhook leg owns no merge right.
func (l *NotificationLedger) Upsert(ctx context.Context, rec NotificationRecord) (NotificationRecord, error) {
	return l.upsert(ctx, rec, false)
}

// UpsertWebhookGeneration stores one immediate-webhook event under the
// current pending generation: it merges into the identity row only while the
// identity row's webhook leg is still pending, and redirects to a fresh
// generation row with its own delivery key once the identity row is claimed
// or settled. The claimed snapshot stays immutable; the late event is never
// dropped and never reuses the claimed key.
func (l *NotificationLedger) UpsertWebhookGeneration(ctx context.Context, rec NotificationRecord) (NotificationRecord, error) {
	return l.upsert(ctx, rec, true)
}

func (l *NotificationLedger) upsert(ctx context.Context, rec NotificationRecord, webhookGeneration bool) (NotificationRecord, error) {
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
	if webhookGeneration {
		return l.upsertWebhookGeneration(ctx, rec, first, last, available)
	}
	return l.upsertMerge(ctx, rec)
}

// upsertMerge is the plain desktop-coalescing path: one row carries one
// shared count and payload for both legs, and merges while either leg can
// still deliver it.
func (l *NotificationLedger) upsertMerge(ctx context.Context, rec NotificationRecord) (NotificationRecord, error) {
	_, err := l.s.db.ExecContext(ctx, `
		INSERT INTO notification_intents
		(category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,
		 payload_json,first_at,last_at,count,available_at,desktop_state,webhook_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(category,event_kind,aggregate_key,phase,window_start) DO UPDATE SET
			last_at=CASE WHEN `+notificationCoalescable+` THEN excluded.last_at ELSE notification_intents.last_at END,
			count=CASE WHEN `+notificationCoalescable+` THEN notification_intents.count+excluded.count ELSE notification_intents.count END,
			payload_json=CASE WHEN `+notificationCoalescable+` THEN
				CASE WHEN json_type(json_extract(excluded.payload_json,'$.count')) IN ('integer','real')
					THEN json_set(json_patch(notification_intents.payload_json, excluded.payload_json),'$.count',
						notification_intents.count+excluded.count)
					ELSE json_patch(notification_intents.payload_json, excluded.payload_json) END
				ELSE notification_intents.payload_json END,
			available_at=CASE WHEN `+notificationCoalescable+` THEN excluded.available_at ELSE notification_intents.available_at END`,
		rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, formatNotificationTime(rec.WindowStart),
		nullIfEmpty(rec.JobID), nullIfEmpty(rec.BatchID), nullIfEmpty(rec.ScanID), rec.PayloadJSON,
		formatNotificationTime(rec.FirstAt), formatNotificationTime(rec.LastAt), maxInt(rec.Count, 1),
		formatNotificationTime(rec.AvailableAt), defaultDesktopState(rec.DesktopState), defaultWebhookState(rec.WebhookState))
	if err != nil {
		return NotificationRecord{}, err
	}
	return l.getByIdentity(ctx, rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, rec.WindowStart)
}

// upsertWebhookGeneration is the atomic generation path for immediate
// webhooks. One transaction decides merge versus redirect under the write
// lock: when the identity row's webhook leg is still pending, the event
// merges into the identity generation; once it is claimed or settled, the
// event queues as a fresh pending generation with its own delivery key and
// never rewrites the claimed snapshot. A byte-identical retry of the same
// event returns the stored generation in any non-pending state, so the
// lost claim skips delivery without a duplicate POST; the replay search
// covers the identity row and every existing generation, so a retry of a
// redirected event matches its own generation. Any event with a later
// LastAt is a new event and opens a new generation. Redirect windows are
// identity+1ns, +2ns, and so on. Each attempt runs in one transaction, and
// a lost unique-index race on the same generation window retries so the
// re-read sees the winner.
func (l *NotificationLedger) upsertWebhookGeneration(ctx context.Context, rec NotificationRecord, first, last, available time.Time) (NotificationRecord, error) {
	for attempt := 0; ; attempt++ {
		row, retry, err := l.upsertWebhookGenerationOnce(ctx, rec, first, last, available)
		if err != nil {
			return NotificationRecord{}, err
		}
		if !retry {
			return row, nil
		}
		if attempt >= 32 {
			// A sustained insert race means concurrent Routes keep
			// colliding on the same window; surface it instead of
			// looping forever.
			return NotificationRecord{}, fmt.Errorf("notification generation insert race did not settle")
		}
	}
}

func (l *NotificationLedger) upsertWebhookGenerationOnce(ctx context.Context, rec NotificationRecord, first, last, available time.Time) (NotificationRecord, bool, error) {
	ok := func(row NotificationRecord) (NotificationRecord, bool, error) { return row, false, nil }
	retry := func() (NotificationRecord, bool, error) { return NotificationRecord{}, true, nil }
	fail := func(err error) (NotificationRecord, bool, error) {
		if isUniqueConflict(err) {
			// Lost a concurrent insert race for the same generation
			// window: roll back and retry so the re-read sees the
			// winner instead of reporting a failure.
			return retry()
		}
		return NotificationRecord{}, false, err
	}
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return NotificationRecord{}, false, err
	}
	rollback := func(err error) (NotificationRecord, bool, error) {
		_ = tx.Rollback()
		return fail(err)
	}
	identity := tx.QueryRowContext(ctx, `SELECT `+notificationColumns+`
		FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
		rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, formatNotificationTime(rec.WindowStart))
	current, err := scanNotificationRow(identity)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return NotificationRecord{}, false, err
		}
		inserted, err := insertGenerationRow(ctx, tx, rec, rec.WindowStart, first, last, available)
		if err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
		row, err := l.getByIdentity(ctx, inserted.Category, inserted.EventKind, inserted.AggregateKey, inserted.Phase, inserted.WindowStart)
		if err != nil {
			return NotificationRecord{}, false, err
		}
		return ok(row)
	}
	if current.WebhookState == "" || current.WebhookState == "pending" {
		merged, err := mergeGenerationRow(ctx, tx, current.ID, rec)
		if err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
		row, err := l.getByIdentity(ctx, merged.Category, merged.EventKind, merged.AggregateKey, merged.Phase, merged.WindowStart)
		if err != nil {
			return NotificationRecord{}, false, err
		}
		return ok(row)
	}
	// Fail open: every Route after the claim queues a fresh pending
	// generation with its own key, except the router's own byte-identical
	// retry of the same event, which returns the stored generation so the
	// lost claim skips delivery without a duplicate POST. The check
	// covers the identity row and every existing generation
	// (findGenerationReplayTx): a retry of a redirected event matches its
	// own generation instead of opening another one. Settled states are
	// included: an identical retry after settle returns the row, while a
	// later LastAt opens a new generation.
	if isPureReplay(current, rec) {
		if err := tx.Rollback(); err != nil {
			return NotificationRecord{}, false, err
		}
		return ok(current)
	}
	match, free, err := findGenerationReplayTx(ctx, tx, rec)
	if err != nil {
		_ = tx.Rollback()
		return NotificationRecord{}, false, err
	}
	if match != nil {
		if err := tx.Rollback(); err != nil {
			return NotificationRecord{}, false, err
		}
		return ok(*match)
	}
	inserted, err := insertGenerationRow(ctx, tx, rec, free, last, last, available)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	row, err := l.getByIdentity(ctx, inserted.Category, inserted.EventKind, inserted.AggregateKey, inserted.Phase, inserted.WindowStart)
	if err != nil {
		return NotificationRecord{}, false, err
	}
	return ok(row)
}

// isUniqueConflict reports a lost concurrent-insert race for the same
// notification identity window.
func isUniqueConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "constraint failed")
}

// insertGenerationRow writes one pending generation row inside the caller's
// transaction and returns its identity for the post-commit re-read.
func insertGenerationRow(ctx context.Context, tx *sql.Tx, rec NotificationRecord, window, first, last, available time.Time) (NotificationRecord, error) {
	redirect := rec
	redirect.ID = 0
	redirect.WindowStart = window
	redirect.FirstAt, redirect.LastAt, redirect.AvailableAt = first, last, available
	redirect.Count = maxInt(rec.Count, 1)
	redirect.DesktopState = ""
	redirect.WebhookState = ""
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_intents
		(category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,
		 payload_json,first_at,last_at,count,available_at,desktop_state,webhook_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		redirect.Category, redirect.EventKind, redirect.AggregateKey, redirect.Phase, formatNotificationTime(window),
		nullIfEmpty(redirect.JobID), nullIfEmpty(redirect.BatchID), nullIfEmpty(redirect.ScanID), redirect.PayloadJSON,
		formatNotificationTime(redirect.FirstAt), formatNotificationTime(redirect.LastAt), redirect.Count,
		formatNotificationTime(redirect.AvailableAt), defaultDesktopState(""), defaultWebhookState("")); err != nil {
		return NotificationRecord{}, err
	}
	return redirect, nil
}

// mergeGenerationRow adds one event to a pending identity generation inside
// the caller's transaction, mirroring the ON CONFLICT count and payload
// merge of the plain path, and returns the row identity for re-read.
func mergeGenerationRow(ctx context.Context, tx *sql.Tx, id int64, rec NotificationRecord) (NotificationRecord, error) {
	var storedCount int
	var storedPayload string
	if err := tx.QueryRowContext(ctx, `SELECT count, payload_json FROM notification_intents WHERE id=?`, id).Scan(&storedCount, &storedPayload); err != nil {
		return NotificationRecord{}, err
	}
	mergedCount := storedCount + maxInt(rec.Count, 1)
	mergedPayload := storedPayload
	if json.Valid([]byte(rec.PayloadJSON)) && json.Valid([]byte(storedPayload)) {
		mergedPayload = mergeWebhookPayload(storedPayload, rec.PayloadJSON, mergedCount)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_intents SET last_at=?, count=?, payload_json=?, available_at=? WHERE id=?`,
		formatNotificationTime(rec.LastAt), mergedCount, mergedPayload, formatNotificationTime(rec.AvailableAt), id); err != nil {
		return NotificationRecord{}, err
	}
	identity := rec
	identity.ID = id
	return identity, nil
}

// mergeWebhookPayload mirrors the ON CONFLICT payload merge: the incoming
// payload deep-patches the stored one key by key, so nested event maps
// keep both sides' entries, and a numeric $.count becomes the merged
// total. Non-object payloads fall back to the incoming payload.
func mergeWebhookPayload(stored, incoming string, mergedCount int) string {
	var a, b map[string]any
	if err := json.Unmarshal([]byte(stored), &a); err != nil {
		return incoming
	}
	if err := json.Unmarshal([]byte(incoming), &b); err != nil {
		return incoming
	}
	deepPatchWebhookPayload(a, b)
	a["count"] = mergedCount
	out, err := json.Marshal(a)
	if err != nil {
		return incoming
	}
	return string(out)
}

// isPureReplay reports whether rec is the router's own retry of the stored
// generation rather than a new producer event. It matches only the exact
// stored row: same payload bytes and count, same identity columns, and a
// LastAt that does not advance past the stored row. It applies in every
// non-pending webhook state, so an identical retry after the generation
// settles returns the row and the lost claim skips delivery without a
// duplicate POST. Any new producer event advances LastAt (the router
// stamps HappenedAt per Route), so it fails this check and queues a new
// generation. Unknown provenance fails open to a new generation.
func isPureReplay(current, rec NotificationRecord) bool {
	if current.PayloadJSON != rec.PayloadJSON {
		return false
	}
	if current.Count != maxInt(rec.Count, 1) {
		return false
	}
	if current.JobID != rec.JobID || current.BatchID != rec.BatchID || current.ScanID != rec.ScanID {
		return false
	}
	if rec.LastAt.After(current.LastAt) {
		return false
	}
	return true
}

// deepPatchWebhookPayload merges src into dst key by key, recursing into
// nested objects so per-event detail entries accumulate instead of
// replacing each other.
func deepPatchWebhookPayload(dst, src map[string]any) {
	for key, value := range src {
		srcMap, srcIsMap := value.(map[string]any)
		dstValue, dstHas := dst[key]
		dstMap, dstIsMap := dstValue.(map[string]any)
		if srcIsMap && dstHas && dstIsMap {
			deepPatchWebhookPayload(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}

// findGenerationReplayTx scans the existing generations after the identity
// row for an exact retry of rec, returning the matching row when present
// and the first free window otherwise. Generations are dense from
// identity+1ns while rows are never deleted, so the first free window ends
// the scan. A retry of a redirected event matches its own generation
// instead of opening another one; a later event matches nothing and queues
// a new generation at the free window.
func findGenerationReplayTx(ctx context.Context, tx *sql.Tx, rec NotificationRecord) (*NotificationRecord, time.Time, error) {
	for offset := int64(1); ; offset++ {
		candidate := rec.WindowStart.Add(time.Duration(offset) * time.Nanosecond)
		row := tx.QueryRowContext(ctx, `SELECT `+notificationColumns+`
			FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
			rec.Category, rec.EventKind, rec.AggregateKey, rec.Phase, formatNotificationTime(candidate))
		existing, err := scanNotificationRow(row)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, candidate, nil
			}
			return nil, time.Time{}, err
		}
		if isPureReplay(existing, rec) {
			match := existing
			return &match, time.Time{}, nil
		}
	}
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
	row := l.s.db.QueryRowContext(ctx, `SELECT `+notificationColumns+`
		FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
		category, eventKind, aggregate, phase, formatNotificationTime(window))
	return scanNotificationRow(row)
}

type notificationScanner interface{ Scan(...any) error }

func scanNotificationRow(row notificationScanner) (NotificationRecord, error) {
	var r NotificationRecord
	var window, first, last, available sql.NullString
	var jobID, batchID, scanID sql.NullString
	var reserved, attempted, webhook sql.NullString
	var sentCount sql.NullInt64
	var sentPayload sql.NullString
	err := row.Scan(&r.ID, &r.Category, &r.EventKind, &r.AggregateKey, &r.Phase, &window,
		&jobID, &batchID, &scanID, &r.PayloadJSON, &first, &last, &available, &r.Count,
		&r.DesktopState, &r.WebhookState, &reserved, &attempted, &webhook, &sentCount, &sentPayload)
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
	if sentCount.Valid {
		r.DesktopSentCount = int(sentCount.Int64)
	}
	if sentPayload.Valid {
		r.DesktopSentPayloadJSON = sentPayload.String
	}
	return r, nil
}

// DueDesktop returns due pending and held desktop legs in FIFO order.
func (l *NotificationLedger) DueDesktop(ctx context.Context, now time.Time, limit int) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.s.db.QueryContext(ctx, `SELECT `+notificationColumns+`
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
	// The caller's error is the one worth reporting; a rollback that also fails
	// adds nothing it can act on, and the transaction is abandoned either way.
	rollback := func(e error) (bool, error) { _ = tx.Rollback(); return false, e }
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
			if err := tx.Rollback(); err != nil {
				return false, err
			}
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
		if err := tx.Rollback(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// SetDesktopState moves the desktop leg with a compare-and-swap from the state
// the caller observed, and reports whether the swap applied. A lost swap means
// a concurrent terminal transition — a supersession, typically — already
// claimed the row; the caller must abandon its stale copy rather than
// resurrect it.
//
// The reserved -> attempted transition also snapshots the count and payload
// the desktop leg delivered. A leg that never delivered leaves the snapshot
// NULL, because it has nothing to preserve.
func (l *NotificationLedger) SetDesktopState(ctx context.Context, id int64, state string, now time.Time) (bool, error) {
	var query string
	var args []any
	switch state {
	case "attempted":
		query = `UPDATE notification_intents SET desktop_state=?, desktop_attempted_at=?,
			desktop_sent_count=notification_intents.count,
			desktop_sent_payload_json=notification_intents.payload_json
			WHERE id=? AND desktop_state='reserved'`
		args = []any{state, formatNotificationTime(now), id}
	case "reserved":
		query = `UPDATE notification_intents SET desktop_state=?, desktop_reserved_at=?
			WHERE id=? AND desktop_state IN ('pending','held')`
		args = []any{state, formatNotificationTime(now), id}
	default:
		query = `UPDATE notification_intents SET desktop_state=? WHERE id=? AND desktop_state IN ('pending','held')`
		args = []any{state, id}
	}
	result, err := l.s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// Webhook legs are at-most-once per row and per delivery key. ClaimWebhook
// atomically moves a pending leg to sending before its HTTP POST so a replay,
// a concurrent drain, or a restart cannot POST the same row twice. The
// claimed row is an immutable snapshot for its key: a merge that lands
// after the claim redirects to a fresh generation row with its own key
// (see Upsert), so the second POST never reuses the first key. The caller
// POSTs only when the claim applies, then settles with SetWebhookState to
// attempted (endpoint accepted, any 2xx) or failed (transport error or
// non-2xx, terminal best-effort loss). A crash between claim and settle
// leaves sending, which never becomes due again: the notification may be
// lost but is never duplicated. Local state cannot give exactly-once across
// a lost HTTP response, so sending rows stay terminal and the stable
// per-row delivery key lets receivers correlate.
func (l *NotificationLedger) ClaimWebhook(ctx context.Context, id int64) (bool, error) {
	result, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET webhook_state='sending' WHERE id=? AND webhook_state='pending'`, id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// SetWebhookState settles the webhook leg. Terminal attempt states
// (attempted, delivered, failed) stamp webhook_attempted_at so the failure
// stays visible next to the success; other transitions leave it untouched.
func (l *NotificationLedger) SetWebhookState(ctx context.Context, id int64, state string, now time.Time) error {
	switch state {
	case "attempted", "delivered", "failed":
		_, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET webhook_state=?, webhook_attempted_at=? WHERE id=?`, state, formatNotificationTime(now), id)
		return err
	default:
		_, err := l.s.db.ExecContext(ctx, `UPDATE notification_intents SET webhook_state=? WHERE id=?`, state, id)
		return err
	}
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
		// The caller's error is the one worth reporting; a rollback that also
		// fails adds nothing it can act on.
		_ = tx.Rollback()
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
			last_at=CASE WHEN `+notificationCoalescable+` THEN excluded.last_at ELSE notification_intents.last_at END,
			count=CASE WHEN `+notificationCoalescable+` THEN notification_intents.count+excluded.count ELSE notification_intents.count END,
			payload_json=CASE WHEN `+notificationCoalescable+` THEN excluded.payload_json ELSE notification_intents.payload_json END,
			available_at=CASE WHEN `+notificationCoalescable+` THEN excluded.available_at ELSE notification_intents.available_at END`,
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

// DueWebhook returns pending digest legs whose shared availability window has
// elapsed. Claimed (sending), delivered, failed, and skipped legs are never
// due again, which is what makes the webhook leg at-most-once per row.
func (l *NotificationLedger) DueWebhook(ctx context.Context, now time.Time, limit int) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.s.db.QueryContext(ctx, `SELECT `+notificationColumns+`
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
	row := l.s.db.QueryRowContext(ctx, `SELECT `+notificationColumns+`
		FROM notification_intents WHERE aggregate_key=? AND category='completion_batch' AND phase='checkpoint'
		ORDER BY id DESC LIMIT 1`, aggregateKey)
	r, err := scanNotificationRow(row)
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
