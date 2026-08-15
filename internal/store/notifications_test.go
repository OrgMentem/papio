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

func TestNotificationTimeParseErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	makeRow := func(cat, kind, agg, phase string, window time.Time) NotificationRecord {
		r, err := ledger.Upsert(ctx, NotificationRecord{Category: cat, EventKind: kind, AggregateKey: agg, Phase: phase, WindowStart: window, FirstAt: window, LastAt: window, AvailableAt: window, Count: 1, PayloadJSON: `{"message":"ok"}`})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Row for DueDesktop
	r1 := makeRow("decision_opened", "action.opened", "agg:1", "opened", now)
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET first_at=? WHERE id=?`, "not-rfc3339", r1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.DueDesktop(ctx, now.Add(time.Hour), 10); err == nil {
		t.Fatal("DueDesktop with bad first_at = nil, want error")
	} else if !contains(err.Error(), itoa(r1.ID)) {
		t.Fatalf("DueDesktop error %q must name row %d", err, r1.ID)
	}
	// Fix r1 so later tests can proceed
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET first_at=? WHERE id=?`, now.Format(time.RFC3339Nano), r1.ID); err != nil {
		t.Fatal(err)
	}
	// Row for DueWebhook
	r2 := makeRow("decision_opened", "action.opened", "agg:2", "opened", now.Add(time.Hour))
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET first_at=? WHERE id=?`, "bad-time", r2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.DueWebhook(ctx, now.Add(2*time.Hour), 10); err == nil {
		t.Fatal("DueWebhook with bad first_at = nil, want error")
	} else if !contains(err.Error(), itoa(r2.ID)) {
		t.Fatalf("DueWebhook error %q must name row %d", err, r2.ID)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET first_at=? WHERE id=?`, now.Format(time.RFC3339Nano), r2.ID); err != nil {
		t.Fatal(err)
	}
	// Row for LatestCheckpoint
	r3 := makeRow("completion_batch", "batch.checkpoint", "cohort:1", "checkpoint", now.Add(2*time.Hour))
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET window_start=? WHERE id=?`, "bogus", r3.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.LatestCheckpoint(ctx, "cohort:1"); err == nil {
		t.Fatal("LatestCheckpoint with bad window_start = nil, want error")
	} else if !contains(err.Error(), itoa(r3.ID)) {
		t.Fatalf("LatestCheckpoint error %q must name row %d", err, r3.ID)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET window_start=? WHERE id=?`, now.Add(2*time.Hour).Format(time.RFC3339Nano), r3.ID); err != nil {
		t.Fatal(err)
	}
	// Row for scanNotification via Upsert getByIdentity
	r4 := makeRow("request_outcome", "request.outcome", "job:2", "terminal", now.Add(3*time.Hour))
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET desktop_reserved_at=? WHERE id=?`, "not-a-time", r4.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Upsert(ctx, NotificationRecord{Category: "request_outcome", EventKind: "request.outcome", AggregateKey: "job:2", Phase: "terminal", WindowStart: now.Add(3 * time.Hour), FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, PayloadJSON: `{"message":"x"}`}); err == nil {
		t.Fatal("Upsert getByIdentity with bad desktop_reserved_at = nil, want error")
	} else if !contains(err.Error(), itoa(r4.ID)) {
		t.Fatalf("Upsert error %q must name row %d", err, r4.ID)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || search(s, substr))
}
func search(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
func itoa(n int64) string {
	s := ""
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}
