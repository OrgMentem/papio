// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"papio/internal/store"
)

// StoreLedger adapts the store package's mirror record to the notify ledger
// interface while keeping the dependency direction one-way.
type StoreLedger struct {
	ledger *store.NotificationLedger
	store  *store.Store
}

func NewStoreLedger(s *store.Store) *StoreLedger {
	if s == nil {
		return &StoreLedger{}
	}
	return &StoreLedger{ledger: s.Notifications(), store: s}
}

func (l *StoreLedger) Upsert(ctx context.Context, rec Record) (Record, error) {
	payload, err := json.Marshal(rec.Intent.Detail)
	if err != nil {
		return Record{}, err
	}
	stored, err := l.ledger.Upsert(ctx, store.NotificationRecord{
		ID: rec.ID, Category: string(rec.Intent.Category), EventKind: rec.Intent.EventKind,
		AggregateKey: rec.Intent.AggregateKey, Phase: string(rec.Intent.Phase), WindowStart: rec.Intent.WindowStart,
		JobID: rec.Intent.JobID, BatchID: rec.Intent.BatchID, ScanID: rec.Intent.ScanID, PayloadJSON: string(payload),
		FirstAt: rec.FirstAt, LastAt: rec.LastAt, AvailableAt: rec.AvailableAt, Count: rec.Count,
		DesktopState: rec.DesktopState, WebhookState: rec.WebhookState,
		DesktopReservedAt: rec.DesktopReservedAt, DesktopAttemptedAt: rec.DesktopAttemptedAt, WebhookAttemptedAt: rec.WebhookAttemptedAt,
	})
	if err != nil {
		return Record{}, err
	}
	rec2, err := fromStoreRecord(stored)
	if err != nil {
		return Record{}, err
	}
	return rec2, nil
}

// UpsertWebhookGeneration stores one immediate-webhook event under the
// current pending generation for its identity: it merges only when the
// identity row's webhook leg is still pending, and redirects to a fresh
// generation row with its own delivery key once the identity row is
// claimed or settled. The store owns the redirect; the router then claims
// and POSTs whichever generation the store returned.
func (l *StoreLedger) UpsertWebhookGeneration(ctx context.Context, rec Record) (Record, error) {
	if l == nil || l.ledger == nil {
		return Record{}, fmt.Errorf("notification ledger is unavailable")
	}
	payload, err := json.Marshal(rec.Intent.Detail)
	if err != nil {
		return Record{}, err
	}
	stored, err := l.ledger.UpsertWebhookGeneration(ctx, store.NotificationRecord{
		ID: rec.ID, Category: string(rec.Intent.Category), EventKind: rec.Intent.EventKind,
		AggregateKey: rec.Intent.AggregateKey, Phase: string(rec.Intent.Phase), WindowStart: rec.Intent.WindowStart,
		JobID: rec.Intent.JobID, BatchID: rec.Intent.BatchID, ScanID: rec.Intent.ScanID, PayloadJSON: string(payload),
		FirstAt: rec.FirstAt, LastAt: rec.LastAt, AvailableAt: rec.AvailableAt, Count: rec.Count,
		DesktopState: rec.DesktopState, WebhookState: rec.WebhookState,
		DesktopReservedAt: rec.DesktopReservedAt, DesktopAttemptedAt: rec.DesktopAttemptedAt, WebhookAttemptedAt: rec.WebhookAttemptedAt,
	})
	if err != nil {
		return Record{}, err
	}
	return fromStoreRecord(stored)
}

// pendingDigestCoalescable mirrors the store merge guard: one row carries one
// shared count and payload for both legs, so a snapshot digest stays mutable
// while either leg can still deliver it.
const pendingDigestCoalescable = `(notification_intents.desktop_state IN ('pending','held') OR notification_intents.webhook_state='pending')`

// UpsertPendingDigest stores an absolute digest snapshot for
// CategoryDecisionPending/PhaseReminder. A retry that routes the identical
// total replaces the count and payload instead of adding them, so a crash
// between Route and the producer's markers cannot inflate the digest
// (2 -> 4). Growth in the same window replaces with the new total while the
// latest generation's webhook leg is still pending. Once that leg is claimed
// or settled its snapshot is immutable: a larger total for a pending webhook
// opens a fresh generation (window +1ns, +2ns, ...) with its own delivery
// key, and the previous generation's still-open desktop leg is superseded so
// the desktop shows the grown total once.
func (l *StoreLedger) UpsertPendingDigest(ctx context.Context, rec Record) (Record, error) {
	if l == nil || l.ledger == nil || l.store == nil || l.store.DB() == nil {
		return Record{}, fmt.Errorf("notification ledger is unavailable")
	}
	payload, err := json.Marshal(rec.Intent.Detail)
	if err != nil {
		return Record{}, err
	}
	count := rec.Count
	if count < 1 {
		count = 1
	}
	desktopState := rec.DesktopState
	if desktopState == "" {
		desktopState = "pending"
	}
	webhookState := rec.WebhookState
	if webhookState == "" {
		webhookState = "pending"
	}
	first := rec.FirstAt
	if first.IsZero() {
		first = rec.LastAt
	}
	last := rec.LastAt
	if last.IsZero() {
		last = first
	}
	available := rec.AvailableAt
	if available.IsZero() {
		available = last
	}
	category, phase := string(rec.Intent.Category), string(rec.Intent.Phase)
	tx, err := l.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	latest, latestWindow, found, err := latestPendingGeneration(ctx, tx, category, rec.Intent.EventKind, rec.Intent.AggregateKey, phase, rec.Intent.WindowStart)
	if err != nil {
		return Record{}, err
	}
	window := rec.Intent.WindowStart
	if found {
		window = latestWindow
		if webhookState == "pending" && latest.WebhookState != "pending" && count > latest.Count {
			// The latest generation's webhook snapshot is claimed or settled:
			// queue the grown total as a new generation instead of rewriting it.
			window = latestWindow.Add(time.Nanosecond)
			if latest.DesktopState == "pending" || latest.DesktopState == "held" {
				desktopState = latest.DesktopState
				if _, err := tx.ExecContext(ctx, `UPDATE notification_intents SET desktop_state='superseded' WHERE id=?`, latest.ID); err != nil {
					return Record{}, err
				}
			} else {
				// The desktop already delivered this window; the new
				// generation exists only for the webhook leg.
				desktopState = "superseded"
			}
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO notification_intents
		(category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,
		 payload_json,first_at,last_at,count,available_at,desktop_state,webhook_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(category,event_kind,aggregate_key,phase,window_start) DO UPDATE SET
			last_at=CASE WHEN `+pendingDigestCoalescable+` THEN excluded.last_at ELSE notification_intents.last_at END,
			count=CASE WHEN `+pendingDigestCoalescable+` THEN excluded.count ELSE notification_intents.count END,
			payload_json=CASE WHEN `+pendingDigestCoalescable+` THEN excluded.payload_json ELSE notification_intents.payload_json END,
			available_at=CASE WHEN `+pendingDigestCoalescable+` THEN excluded.available_at ELSE notification_intents.available_at END`,
		category, rec.Intent.EventKind, rec.Intent.AggregateKey, phase, formatPendingTime(window),
		nullIfEmpty(rec.Intent.JobID), nullIfEmpty(rec.Intent.BatchID), nullIfEmpty(rec.Intent.ScanID), string(payload),
		formatPendingTime(first), formatPendingTime(last), count,
		formatPendingTime(available), desktopState, webhookState)
	if err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return l.fetchPendingByIdentity(ctx, category, rec.Intent.EventKind, rec.Intent.AggregateKey, phase, formatPendingTime(window))
}

// latestPendingGeneration returns the newest generation row for a digest
// identity. Generations are dense from the identity window in 1ns steps and
// rows are never deleted, so the first missing window ends the scan.
func latestPendingGeneration(ctx context.Context, tx *sql.Tx, category, eventKind, aggregate, phase string, identity time.Time) (store.NotificationRecord, time.Time, bool, error) {
	var latest store.NotificationRecord
	var latestWindow time.Time
	found := false
	for offset := time.Duration(0); ; offset++ {
		window := identity.Add(offset)
		row := tx.QueryRowContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at,desktop_sent_count,desktop_sent_payload_json FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
			category, eventKind, aggregate, phase, formatPendingTime(window))
		rec, err := scanPendingRow(row)
		if errors.Is(err, sql.ErrNoRows) {
			return latest, latestWindow, found, nil
		}
		if err != nil {
			return store.NotificationRecord{}, time.Time{}, false, err
		}
		latest, latestWindow, found = rec, window, true
	}
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (l *StoreLedger) fetchPendingByIdentity(ctx context.Context, category, eventKind, aggregate, phase, window string) (Record, error) {
	row := l.store.DB().QueryRowContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at,desktop_sent_count,desktop_sent_payload_json FROM notification_intents WHERE category=? AND event_kind=? AND aggregate_key=? AND phase=? AND window_start=?`,
		category, eventKind, aggregate, phase, window)
	rec, err := scanPendingRow(row)
	if err != nil {
		return Record{}, err
	}
	return fromStoreRecord(rec)
}

type pendingRowScanner interface{ Scan(...any) error }

func scanPendingRow(scanner pendingRowScanner) (store.NotificationRecord, error) {
	var rec store.NotificationRecord
	var window, first, last, available sql.NullString
	var jobID, batchID, scanID sql.NullString
	var reserved, attempted, webhook sql.NullString
	var sentCount sql.NullInt64
	var sentPayload sql.NullString
	if err := scanner.Scan(&rec.ID, &rec.Category, &rec.EventKind, &rec.AggregateKey, &rec.Phase, &window, &jobID, &batchID, &scanID, &rec.PayloadJSON, &first, &last, &available, &rec.Count, &rec.DesktopState, &rec.WebhookState, &reserved, &attempted, &webhook, &sentCount, &sentPayload); err != nil {
		return store.NotificationRecord{}, err
	}
	var parseErr error
	if rec.WindowStart, parseErr = parseNotifyTime(rec.ID, "window_start", window); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.FirstAt, parseErr = parseNotifyTime(rec.ID, "first_at", first); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.LastAt, parseErr = parseNotifyTime(rec.ID, "last_at", last); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.AvailableAt, parseErr = parseNotifyTime(rec.ID, "available_at", available); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.DesktopReservedAt, parseErr = parseNotifyTime(rec.ID, "desktop_reserved_at", reserved); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.DesktopAttemptedAt, parseErr = parseNotifyTime(rec.ID, "desktop_attempted_at", attempted); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if rec.WebhookAttemptedAt, parseErr = parseNotifyTime(rec.ID, "webhook_attempted_at", webhook); parseErr != nil {
		return store.NotificationRecord{}, parseErr
	}
	if jobID.Valid {
		rec.JobID = jobID.String
	}
	if batchID.Valid {
		rec.BatchID = batchID.String
	}
	if scanID.Valid {
		rec.ScanID = scanID.String
	}
	if sentCount.Valid {
		rec.DesktopSentCount = int(sentCount.Int64)
	}
	if sentPayload.Valid {
		rec.DesktopSentPayloadJSON = sentPayload.String
	}
	return rec, nil
}

func (l *StoreLedger) DueDesktop(ctx context.Context, now time.Time, limit int) ([]Record, error) {
	rows, err := l.ledger.DueDesktop(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		rec, err := fromStoreRecord(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
func (l *StoreLedger) DueWebhook(ctx context.Context, now time.Time, limit int) ([]Record, error) {
	rows, err := l.ledger.DueWebhook(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		rec, err := fromStoreRecord(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (l *StoreLedger) ReserveDesktop(ctx context.Context, id int64, now time.Time, maxPerHour int) (bool, error) {
	return l.ledger.ReserveDesktop(ctx, id, now, maxPerHour)
}
func (l *StoreLedger) SetDesktopState(ctx context.Context, id int64, state string, now time.Time) (bool, error) {
	return l.ledger.SetDesktopState(ctx, id, state, now)
}
func (l *StoreLedger) ClaimWebhook(ctx context.Context, id int64) (Record, bool, error) {
	if l == nil || l.ledger == nil {
		return Record{}, false, fmt.Errorf("notification ledger is unavailable")
	}
	claimed, err := l.ledger.ClaimWebhook(ctx, id)
	if err != nil || !claimed {
		return Record{}, claimed, err
	}
	if l.store == nil || l.store.DB() == nil {
		return Record{}, true, fmt.Errorf("notification ledger store is unavailable")
	}
	// A concurrent Route can coalesce another event into the same row after
	// this Route's Upsert returned but before this claim won. The Upsert
	// snapshot is stale then, so re-read the claimed row and send its
	// authoritative count and payload. The loser of the claim sends nothing,
	// and the winner's POST already covers the merged event.
	fresh, err := l.fetchByID(ctx, id)
	if err != nil {
		return Record{}, true, err
	}
	return fresh, true, nil
}

func (l *StoreLedger) fetchByID(ctx context.Context, id int64) (Record, error) {
	row := l.store.DB().QueryRowContext(ctx, `SELECT id,category,event_kind,aggregate_key,phase,window_start,job_id,batch_id,scan_id,payload_json,first_at,last_at,available_at,count,desktop_state,webhook_state,desktop_reserved_at,desktop_attempted_at,webhook_attempted_at,desktop_sent_count,desktop_sent_payload_json FROM notification_intents WHERE id=?`, id)
	rec, err := scanPendingRow(row)
	if err != nil {
		return Record{}, err
	}
	return fromStoreRecord(rec)
}

// GetByID re-reads one ledger row by ID. It exists for diagnostics and
// tests; the send path uses the immutable ClaimWebhook snapshot and never
// re-sends a grown row under the same delivery key.
func (l *StoreLedger) GetByID(ctx context.Context, id int64) (Record, error) {
	if l == nil || l.ledger == nil || l.store == nil || l.store.DB() == nil {
		return Record{}, fmt.Errorf("notification ledger is unavailable")
	}
	return l.fetchByID(ctx, id)
}

func formatPendingTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return store.FormatTime(value)
}

func parseNotifyTime(rowID int64, column string, text sql.NullString) (time.Time, error) {
	if !text.Valid || text.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, text.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("notification %d: %s %q: %w", rowID, column, text.String, err)
	}
	return t, nil
}
func (l *StoreLedger) SetWebhookState(ctx context.Context, id int64, state string, now time.Time) error {
	return l.ledger.SetWebhookState(ctx, id, state, now)
}
func (l *StoreLedger) SupersedeCheckpoints(ctx context.Context, aggregateKey string, now time.Time) (int, error) {
	return l.ledger.SupersedeCheckpoints(ctx, aggregateKey, now)
}
func (l *StoreLedger) SupersedeAndUpsertCheckpoint(ctx context.Context, aggregateKey string, now time.Time, rec Record) (Record, error) {
	payload, err := json.Marshal(rec.Intent.Detail)
	if err != nil {
		return Record{}, err
	}
	row, err := l.ledger.SupersedeAndUpsertCheckpoint(ctx, aggregateKey, now, store.NotificationRecord{
		Category: string(rec.Intent.Category), EventKind: rec.Intent.EventKind,
		AggregateKey: rec.Intent.AggregateKey, Phase: string(rec.Intent.Phase), WindowStart: rec.Intent.WindowStart,
		JobID: rec.Intent.JobID, BatchID: rec.Intent.BatchID, ScanID: rec.Intent.ScanID, PayloadJSON: string(payload),
		FirstAt: rec.FirstAt, LastAt: rec.LastAt, AvailableAt: rec.AvailableAt, Count: rec.Count,
		DesktopState: rec.DesktopState, WebhookState: rec.WebhookState,
	})
	if err != nil {
		return Record{}, err
	}
	rec2, err := fromStoreRecord(row)
	if err != nil {
		return Record{}, err
	}
	return rec2, nil
}

func (l *StoreLedger) LatestCheckpoint(ctx context.Context, aggregateKey string) (Record, bool, error) {
	row, ok, err := l.ledger.LatestCheckpoint(ctx, aggregateKey)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	rec, err := fromStoreRecord(row)
	if err != nil {
		return Record{}, true, err
	}
	return rec, true, nil
}

func fromStoreRecord(row store.NotificationRecord) (Record, error) {
	var detail Event
	if err := json.Unmarshal([]byte(row.PayloadJSON), &detail); err != nil {
		return Record{}, fmt.Errorf("notification %d: payload_json: %w", row.ID, err)
	}
	rec := Record{ID: row.ID, Intent: Intent{EventKind: row.EventKind, Category: Category(row.Category), AggregateKey: row.AggregateKey, Phase: Phase(row.Phase), WindowStart: row.WindowStart, JobID: row.JobID, BatchID: row.BatchID, ScanID: row.ScanID, HappenedAt: row.LastAt, Message: detail.Message, Detail: detail}, FirstAt: row.FirstAt, LastAt: row.LastAt, AvailableAt: row.AvailableAt, Count: row.Count, DesktopState: row.DesktopState, WebhookState: row.WebhookState, DesktopReservedAt: row.DesktopReservedAt, DesktopAttemptedAt: row.DesktopAttemptedAt, WebhookAttemptedAt: row.WebhookAttemptedAt}
	if row.DesktopSentPayloadJSON != "" {
		var sent Event
		if err := json.Unmarshal([]byte(row.DesktopSentPayloadJSON), &sent); err != nil {
			return Record{}, fmt.Errorf("notification %d: desktop_sent_payload_json: %w", row.ID, err)
		}
		rec.DesktopSentDetail, rec.DesktopSentCount = sent, row.DesktopSentCount
	}
	return rec, nil
}

func (l *StoreLedger) SetDesktopAvailable(ctx context.Context, id int64, available time.Time) error {
	return l.ledger.SetDesktopAvailable(ctx, id, available)
}
