// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"encoding/json"
	"time"

	"papio/internal/store"
)

// StoreLedger adapts the store package's mirror record to the notify ledger
// interface while keeping the dependency direction one-way.
type StoreLedger struct{ ledger *store.NotificationLedger }

func NewStoreLedger(s *store.Store) *StoreLedger {
	if s == nil {
		return &StoreLedger{}
	}
	return &StoreLedger{ledger: s.Notifications()}
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
	return fromStoreRecord(stored), nil
}

func (l *StoreLedger) DueDesktop(ctx context.Context, now time.Time, limit int) ([]Record, error) {
	rows, err := l.ledger.DueDesktop(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromStoreRecord(row))
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
		out = append(out, fromStoreRecord(row))
	}
	return out, nil
}

func (l *StoreLedger) ReserveDesktop(ctx context.Context, id int64, now time.Time, maxPerHour int) (bool, error) {
	return l.ledger.ReserveDesktop(ctx, id, now, maxPerHour)
}
func (l *StoreLedger) SetDesktopState(ctx context.Context, id int64, state string, now time.Time) error {
	return l.ledger.SetDesktopState(ctx, id, state, now)
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
	return fromStoreRecord(row), nil
}

func (l *StoreLedger) LatestCheckpoint(ctx context.Context, aggregateKey string) (Record, bool, error) {
	row, ok, err := l.ledger.LatestCheckpoint(ctx, aggregateKey)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	return fromStoreRecord(row), true, nil
}

func fromStoreRecord(row store.NotificationRecord) Record {
	var detail Event
	_ = json.Unmarshal([]byte(row.PayloadJSON), &detail)
	return Record{ID: row.ID, Intent: Intent{EventKind: row.EventKind, Category: Category(row.Category), AggregateKey: row.AggregateKey, Phase: Phase(row.Phase), WindowStart: row.WindowStart, JobID: row.JobID, BatchID: row.BatchID, ScanID: row.ScanID, HappenedAt: row.LastAt, Message: detail.Message, Detail: detail}, FirstAt: row.FirstAt, LastAt: row.LastAt, AvailableAt: row.AvailableAt, Count: row.Count, DesktopState: row.DesktopState, WebhookState: row.WebhookState, DesktopReservedAt: row.DesktopReservedAt, DesktopAttemptedAt: row.DesktopAttemptedAt, WebhookAttemptedAt: row.WebhookAttemptedAt}
}

func (l *StoreLedger) SetDesktopAvailable(ctx context.Context, id int64, available time.Time) error {
	return l.ledger.SetDesktopAvailable(ctx, id, available)
}
