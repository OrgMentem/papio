// Copyright 2026 OrgMentem. Licensed under MIT.

package store

import (
	"context"
	"testing"
	"time"
)

func TestNotificationLedgerUpsertCoalescesAndReservesOnce(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	first, err := ledger.Upsert(ctx, NotificationRecord{Category: "decision_opened", EventKind: "action.opened", AggregateKey: "batch-1", Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, PayloadJSON: `{"count":1}`})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Upsert(ctx, NotificationRecord{Category: "decision_opened", EventKind: "action.opened", AggregateKey: "batch-1", Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now.Add(time.Minute), AvailableAt: now.Add(time.Minute), Count: 1, PayloadJSON: `{"count":2}`})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Count != 2 || second.PayloadJSON != `{"count":2}` {
		t.Fatalf("coalesced = %+v then %+v", first, second)
	}
	rows, err := ledger.DueDesktop(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("due rows = %d, want 1", len(rows))
	}
	reserved, err := ledger.ReserveDesktop(ctx, first.ID, now.Add(2*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved {
		t.Fatal("first reservation rejected")
	}
	again, err := ledger.ReserveDesktop(ctx, first.ID, now.Add(2*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("reserved row replayed")
	}
}

func TestNotificationLedgerTerminalReplayIsImmutable(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	first, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "request_outcome", EventKind: "request.outcome", AggregateKey: "job:1",
		Phase: "terminal", WindowStart: now, FirstAt: now, LastAt: now,
		AvailableAt: now, Count: 1, PayloadJSON: `{"count":1,"message":"first"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := ledger.ReserveDesktop(ctx, first.ID, now, 0); err != nil || !reserved {
		t.Fatalf("reserve = %v, %v", reserved, err)
	}
	if err := ledger.SetDesktopState(ctx, first.ID, "attempted", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "request_outcome", EventKind: "request.outcome", AggregateKey: "job:1",
		Phase: "terminal", WindowStart: now, FirstAt: now, LastAt: now.Add(2 * time.Hour),
		AvailableAt: now.Add(2 * time.Hour), Count: 1, PayloadJSON: `{"count":1,"message":"replay"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Count != 1 || !replayed.LastAt.Equal(now) || replayed.PayloadJSON != `{"count":1,"message":"first"}` {
		t.Fatalf("terminal replay mutated audit row: %+v", replayed)
	}
}

func TestRecordSystemEventIsJobless(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RecordSystemEvent(ctx, "notify.attempted", map[string]any{"category": "request_outcome"}); err != nil {
		t.Fatal(err)
	}
	entries, err := db.RecentEvents(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].JobID != "" || entries[0].Kind != "notify.attempted" {
		t.Fatalf("entries = %+v", entries)
	}
}
