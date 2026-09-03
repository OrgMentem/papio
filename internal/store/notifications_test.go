// Copyright 2026 OrgMentem. Licensed under MIT.

package store

import (
	"context"
	"encoding/json"
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
		WebhookState: "skipped",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := ledger.ReserveDesktop(ctx, first.ID, now, 0); err != nil || !reserved {
		t.Fatalf("reserve = %v, %v", reserved, err)
	}
	if applied, err := ledger.SetDesktopState(ctx, first.ID, "attempted", now.Add(time.Minute)); err != nil || !applied {
		t.Fatalf("attempted transition = %v, %v", applied, err)
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
	if replayed.DesktopSentCount != 1 || replayed.DesktopSentPayloadJSON != `{"count":1,"message":"first"}` {
		t.Fatalf("desktop snapshot = %d/%q, want the delivered count and payload", replayed.DesktopSentCount, replayed.DesktopSentPayloadJSON)
	}
}

// A finished desktop leg must not freeze a webhook digest that has not been
// sent: the shared row keeps coalescing for the pending webhook leg, while the
// desktop snapshot preserves exactly what the desktop already delivered.
func TestNotificationLedgerCoalescesForPendingWebhookAfterDesktopDelivery(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	first, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "discovery_new", EventKind: "watch.alert", AggregateKey: "watch:1",
		Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now,
		Count: 1, PayloadJSON: `{"count":1,"message":"first"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := ledger.ReserveDesktop(ctx, first.ID, now, 0); err != nil || !reserved {
		t.Fatalf("reserve = %v, %v", reserved, err)
	}
	if applied, err := ledger.SetDesktopState(ctx, first.ID, "attempted", now.Add(time.Minute)); err != nil || !applied {
		t.Fatalf("attempted transition = %v, %v", applied, err)
	}
	merged, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "discovery_new", EventKind: "watch.alert", AggregateKey: "watch:1",
		Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now.Add(2 * time.Minute),
		AvailableAt: now.Add(2 * time.Minute), Count: 1, PayloadJSON: `{"count":1,"message":"second"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Count != 2 {
		t.Fatalf("count after a second event = %d, want 2: the webhook digest lost an event", merged.Count)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(merged.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["message"] != "second" || payload["count"] != float64(2) {
		t.Fatalf("merged payload = %v, want the second message and count 2", payload)
	}
	if merged.DesktopSentCount != 1 || merged.DesktopSentPayloadJSON != `{"count":1,"message":"first"}` {
		t.Fatalf("coalescing rewrote the desktop audit: snapshot = %d/%q", merged.DesktopSentCount, merged.DesktopSentPayloadJSON)
	}
	// Once the webhook leg is sent too, neither leg can still deliver and the
	// row becomes an immutable audit record.
	if err := ledger.SetWebhookState(ctx, first.ID, "attempted", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	frozen, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "discovery_new", EventKind: "watch.alert", AggregateKey: "watch:1",
		Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now.Add(4 * time.Minute),
		AvailableAt: now.Add(4 * time.Minute), Count: 1, PayloadJSON: `{"count":1,"message":"third"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Count != 2 || frozen.PayloadJSON != merged.PayloadJSON {
		t.Fatalf("both legs terminal but the row still mutated: %+v", frozen)
	}
}

// A desktop leg that never delivered leaves the snapshot empty, so audit reads
// fall back to the live count and payload.
func TestNotificationLedgerUndeliveredDesktopLegKeepsNoSnapshot(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	off, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "discovery_new", EventKind: "watch.alert", AggregateKey: "watch:2",
		Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now,
		Count: 1, PayloadJSON: `{"count":1,"message":"first"}`, DesktopState: "platform_unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"desktop_sent_count", "desktop_sent_payload_json"} {
		if got := notificationColumn(t, db, off.ID, column); got != "" {
			t.Errorf("%s = %q for a desktop leg that never sent, want NULL", column, got)
		}
	}
	held, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "discovery_new", EventKind: "watch.alert", AggregateKey: "watch:3",
		Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now,
		Count: 1, PayloadJSON: `{"count":1,"message":"first"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := ledger.SetDesktopState(ctx, held.ID, "suppressed_presence", now); err != nil || !applied {
		t.Fatalf("suppression transition = %v, %v", applied, err)
	}
	if got := notificationColumn(t, db, held.ID, "desktop_sent_payload_json"); got != "" {
		t.Errorf("suppressed leg recorded a delivery snapshot %q", got)
	}
}

// Every nonterminal desktop transition is a compare-and-swap: a stale caller
// cannot pull a superseded row back into the drain.
func TestNotificationLedgerSetDesktopStateRefusesTerminalRows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	row, err := ledger.Upsert(ctx, NotificationRecord{
		Category: "completion_batch", EventKind: "batch.progress", AggregateKey: "batch-7",
		Phase: "checkpoint", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now,
		Count: 1, PayloadJSON: `{"count":1}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if superseded, err := ledger.SupersedeCheckpoints(ctx, "batch-7", now); err != nil || superseded != 1 {
		t.Fatalf("supersede = %d, %v", superseded, err)
	}
	for _, state := range []string{"held", "dropped_quiet", "platform_unavailable", "suppressed_presence", "reserved"} {
		applied, err := ledger.SetDesktopState(ctx, row.ID, state, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatalf("transition to %q applied to a superseded row", state)
		}
		if got := notificationColumn(t, db, row.ID, "desktop_state"); got != "superseded" {
			t.Fatalf("desktop_state = %q after a lost swap to %q, want superseded", got, state)
		}
	}
	if applied, err := ledger.SetDesktopState(ctx, row.ID, "attempted", now.Add(time.Minute)); err != nil || applied {
		t.Fatalf("attempted transition from superseded = %v, %v, want false", applied, err)
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

// notificationColumn reads one column straight from the row so a test can
// assert on state the ledger's own readers normalise away.
func notificationColumn(t *testing.T, db *Store, id int64, column string) string {
	t.Helper()
	var value *string
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT `+column+` FROM notification_intents WHERE id=?`, id).Scan(&value); err != nil {
		t.Fatalf("reading %s of intent %d: %v", column, id, err)
	}
	if value == nil {
		return ""
	}
	return *value
}

func notificationPhaseCount(t *testing.T, db *Store, aggregateKey, phase string) int {
	t.Helper()
	var count int
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM notification_intents WHERE aggregate_key=? AND phase=?`,
		aggregateKey, phase).Scan(&count); err != nil {
		t.Fatalf("counting %s/%s rows: %v", aggregateKey, phase, err)
	}
	return count
}

func TestNotificationLedgerSupersedeCheckpointsRetiresOnlyLiveCheckpoints(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	seed := func(aggregateKey, phase, desktopState string, window time.Time) NotificationRecord {
		t.Helper()
		rec, err := ledger.Upsert(ctx, NotificationRecord{Category: "completion_batch", EventKind: "batch.progress", AggregateKey: aggregateKey, Phase: phase, WindowStart: window, FirstAt: window, LastAt: window, AvailableAt: window, Count: 1, DesktopState: desktopState, PayloadJSON: `{"count":1}`})
		if err != nil {
			t.Fatalf("seeding %s/%s: %v", aggregateKey, phase, err)
		}
		return rec
	}
	pending := seed("batch-1", "checkpoint", "pending", now)
	held := seed("batch-1", "checkpoint", "held", now.Add(time.Minute))
	reserved := seed("batch-1", "checkpoint", "reserved", now.Add(2*time.Minute))
	final := seed("batch-1", "final", "pending", now)
	otherBatch := seed("batch-2", "checkpoint", "pending", now)

	superseded, err := ledger.SupersedeCheckpoints(ctx, "batch-1", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if superseded != 2 {
		t.Fatalf("superseded rows = %d, want 2 (the pending and held checkpoints)", superseded)
	}
	for _, want := range []struct {
		name  string
		id    int64
		state string
	}{
		{"pending checkpoint", pending.ID, "superseded"},
		{"held checkpoint", held.ID, "superseded"},
		{"reserved checkpoint", reserved.ID, "reserved"},
		{"final leg", final.ID, "pending"},
		{"other batch checkpoint", otherBatch.ID, "pending"},
	} {
		if got := notificationColumn(t, db, want.id, "desktop_state"); got != want.state {
			t.Errorf("%s desktop_state = %q, want %q", want.name, got, want.state)
		}
	}
	rows, err := ledger.DueDesktop(ctx, now.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("due desktop rows after supersession = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.ID == pending.ID || row.ID == held.ID {
			t.Errorf("superseded checkpoint %d is still due for the desktop", row.ID)
		}
	}
}

func TestNotificationLedgerSupersedeAndUpsertCheckpointIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	checkpoint, err := ledger.Upsert(ctx, NotificationRecord{Category: "completion_batch", EventKind: "batch.progress", AggregateKey: "batch-1", Phase: "checkpoint", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now, Count: 3, PayloadJSON: `{"count":3}`})
	if err != nil {
		t.Fatal(err)
	}
	finalAt := now.Add(5 * time.Minute)
	finalRec := NotificationRecord{Category: "completion_batch", EventKind: "batch.completed", AggregateKey: "batch-1", Phase: "final", WindowStart: finalAt, FirstAt: now, LastAt: finalAt, AvailableAt: finalAt, Count: 1, PayloadJSON: `{"count":7}`}

	// A rejecting trigger is the only way to fail the insert leg after the
	// supersession update has already run inside the transaction. That is
	// exactly the window the single transaction exists to close: a batch whose
	// checkpoint is cancelled without its replacement completion.
	if _, err := db.db.ExecContext(ctx, `CREATE TRIGGER notification_insert_fails BEFORE INSERT ON notification_intents
		BEGIN SELECT RAISE(ABORT, 'insert leg rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.SupersedeAndUpsertCheckpoint(ctx, "batch-1", finalAt, finalRec); err == nil {
		t.Fatal("SupersedeAndUpsertCheckpoint reported success while the insert leg was blocked")
	}
	if _, err := db.db.ExecContext(ctx, `DROP TRIGGER notification_insert_fails`); err != nil {
		t.Fatal(err)
	}
	if got := notificationColumn(t, db, checkpoint.ID, "desktop_state"); got != "pending" {
		t.Fatalf("checkpoint desktop_state = %q after a failed finalisation, want %q: the batch completion is lost", got, "pending")
	}
	if got := notificationPhaseCount(t, db, "batch-1", "final"); got != 0 {
		t.Fatalf("final rows after a failed finalisation = %d, want 0", got)
	}

	stored, err := ledger.SupersedeAndUpsertCheckpoint(ctx, "batch-1", finalAt, finalRec)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID == 0 || stored.Count != 1 || stored.PayloadJSON != `{"count":7}` {
		t.Fatalf("stored final leg = %+v, want one row carrying the final payload", stored)
	}
	if got := notificationColumn(t, db, checkpoint.ID, "desktop_state"); got != "superseded" {
		t.Fatalf("checkpoint desktop_state = %q after finalisation, want %q", got, "superseded")
	}
	rows, err := ledger.DueDesktop(ctx, finalAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != stored.ID {
		t.Fatalf("due desktop rows = %+v, want only the final leg %d", rows, stored.ID)
	}
	latest, ok, err := ledger.LatestCheckpoint(ctx, "batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || latest.ID != checkpoint.ID || latest.DesktopState != "superseded" {
		t.Fatalf("latest checkpoint = %+v (found=%v), want row %d superseded", latest, ok, checkpoint.ID)
	}

	// Re-finalising the same identity coalesces into the existing final row
	// and replaces its payload rather than duplicating the completion.
	replay := finalRec
	replay.PayloadJSON = `{"count":9}`
	again, err := ledger.SupersedeAndUpsertCheckpoint(ctx, "batch-1", now.Add(6*time.Minute), replay)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != stored.ID {
		t.Fatalf("re-finalisation created row %d, want the existing row %d", again.ID, stored.ID)
	}
	if again.Count != 2 || again.PayloadJSON != `{"count":9}` {
		t.Fatalf("re-finalised leg = %+v, want count 2 and the replacement payload", again)
	}
	if got := notificationPhaseCount(t, db, "batch-1", "final"); got != 1 {
		t.Fatalf("final rows after re-finalisation = %d, want 1", got)
	}
}

func TestNotificationLedgerWebhookStateAndDesktopAvailabilityRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := db.Notifications()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	row, err := ledger.Upsert(ctx, NotificationRecord{Category: "decision_opened", EventKind: "action.opened", AggregateKey: "batch-9", Phase: "opened", WindowStart: now, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, PayloadJSON: `{"count":1}`})
	if err != nil {
		t.Fatal(err)
	}
	due, err := ledger.DueWebhook(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != row.ID {
		t.Fatalf("due webhook rows = %+v, want the pending leg %d", due, row.ID)
	}
	attemptedAt := now.Add(time.Minute)
	if err := ledger.SetWebhookState(ctx, row.ID, "attempted", attemptedAt); err != nil {
		t.Fatal(err)
	}
	if got := notificationColumn(t, db, row.ID, "webhook_state"); got != "attempted" {
		t.Fatalf("webhook_state = %q, want %q", got, "attempted")
	}
	if got := notificationColumn(t, db, row.ID, "webhook_attempted_at"); got != formatNotificationTime(attemptedAt) {
		t.Fatalf("webhook_attempted_at = %q, want %q", got, formatNotificationTime(attemptedAt))
	}
	due, err = ledger.DueWebhook(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("attempted leg is still due for webhook delivery: %+v", due)
	}
	if err := ledger.SetWebhookState(ctx, row.ID, "sent", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := notificationColumn(t, db, row.ID, "webhook_state"); got != "sent" {
		t.Fatalf("webhook_state = %q, want %q", got, "sent")
	}
	if got := notificationColumn(t, db, row.ID, "webhook_attempted_at"); got != formatNotificationTime(attemptedAt) {
		t.Fatalf("a non-attempt transition restamped webhook_attempted_at = %q, want %q", got, formatNotificationTime(attemptedAt))
	}

	// SetDesktopAvailable defers a held leg only; a pending leg keeps its slot.
	deferred := now.Add(time.Hour)
	if err := ledger.SetDesktopAvailable(ctx, row.ID, deferred); err != nil {
		t.Fatal(err)
	}
	if got := notificationColumn(t, db, row.ID, "available_at"); got != formatNotificationTime(now) {
		t.Fatalf("pending leg available_at = %q, want %q left untouched", got, formatNotificationTime(now))
	}
	if applied, err := ledger.SetDesktopState(ctx, row.ID, "held", now); err != nil || !applied {
		t.Fatalf("hold transition = %v, %v", applied, err)
	}
	if err := ledger.SetDesktopAvailable(ctx, row.ID, deferred); err != nil {
		t.Fatal(err)
	}
	if got := notificationColumn(t, db, row.ID, "available_at"); got != formatNotificationTime(deferred) {
		t.Fatalf("held leg available_at = %q, want %q", got, formatNotificationTime(deferred))
	}
	early, err := ledger.DueDesktop(ctx, now.Add(30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("deferred leg is due before its new availability: %+v", early)
	}
	released, err := ledger.DueDesktop(ctx, deferred, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ID != row.ID || released[0].DesktopState != "held" {
		t.Fatalf("released desktop rows = %+v, want the held leg %d", released, row.ID)
	}
}
