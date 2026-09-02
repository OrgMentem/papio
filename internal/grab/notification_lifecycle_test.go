// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package grab

import (
	"context"
	"testing"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

// pendingIDs reads the durable notification queue and counts each grab id it
// reports. A count above one is an endless replay; a missing id is a silently
// dropped notification. Both are invisible to a length-only assertion.
func pendingIDs(t *testing.T, ctx context.Context, svc *Service, limit int) map[string]int {
	t.Helper()
	pending, err := svc.PendingNotifications(ctx, limit)
	if err != nil {
		t.Fatalf("PendingNotifications(%d): %v", limit, err)
	}
	got := make(map[string]int, len(pending))
	for _, g := range pending {
		if g == nil {
			t.Fatalf("PendingNotifications(%d) returned a nil grab", limit)
		}
		if g.Outcome == "" {
			t.Fatalf("PendingNotifications(%d) returned non-terminal grab %q", limit, g.ID)
		}
		if g.NotifiedAt != "" {
			t.Fatalf("PendingNotifications(%d) returned already-notified grab %q", limit, g.ID)
		}
		got[g.ID]++
	}
	return got
}

func wantPending(t *testing.T, got map[string]int, ids ...string) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("pending queue = %v, want exactly %v", got, ids)
	}
	for _, id := range ids {
		if n := got[id]; n != 1 {
			t.Fatalf("pending queue reported %q %d times, want exactly once (queue = %v)", id, n, got)
		}
	}
}

// TestGrabNotificationLifecycle walks one grab through the durable
// notification queue: terminal grabs are queued exactly once,
// MarkNotified retires exactly the grab it names, and Delete removes
// exactly the row it names.
func TestGrabNotificationLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	db := s.DB()
	now := store.Now()

	const (
		jobA = "job_00000000000000000000000101"
		jobB = "job_00000000000000000000000102"
	)
	for _, j := range []struct{ job, req string }{{jobA, "request-notify-a"}, {jobB, "request-notify-b"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, j.req, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			j.job, j.req, "awaiting_human", `{}`, now, now); err != nil {
			t.Fatal(err)
		}
	}

	// Two terminal grabs are queued for notification; a third is still
	// awaiting its file and must never appear in the queue.
	target, err := svc.Allocate(ctx, "pdf.example.org", "Queued paper")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := svc.Allocate(ctx, "pdf.example.org", "Sibling paper")
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := svc.Allocate(ctx, "pdf.example.org", "Still downloading")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkJobCreated(ctx, target.ID, jobA, "job_created"); err != nil {
		t.Fatalf("MarkJobCreated target: %v", err)
	}
	if err := svc.MarkJobCreated(ctx, sibling.ID, jobB, "already_owned"); err != nil {
		t.Fatalf("MarkJobCreated sibling: %v", err)
	}

	// Oldest first, not merely "one row". Both grabs settle inside the same
	// clock tick, so stamp both explicitly: deriving the sibling's time from
	// the clock leaves equal timestamps theoretically possible, and a queue
	// flipped to DESC would keep passing a length-only assertion.
	for _, stamp := range []struct{ id, at string }{
		{target.ID, "2000-01-01T00:00:00.000000000Z"},
		{sibling.ID, "2001-01-01T00:00:00.000000000Z"},
	} {
		if _, err := db.ExecContext(ctx, `UPDATE pdf_grabs SET updated_at = ? WHERE id = ?`, stamp.at, stamp.id); err != nil {
			t.Fatal(err)
		}
	}
	if first := pendingIDs(t, ctx, svc, 1); len(first) != 1 || first[target.ID] != 1 {
		t.Fatalf("PendingNotifications(1) = %v, want exactly the oldest grab %q: the queue must honour both the limit and its oldest-first order", first, target.ID)
	}
	// limit <= 0 falls back to the default batch, which covers both rows.
	wantPending(t, pendingIDs(t, ctx, svc, 0), target.ID, sibling.ID)

	// Notifying the target retires exactly the target.
	if err := svc.MarkNotified(ctx, target.ID); err != nil {
		t.Fatalf("MarkNotified target: %v", err)
	}
	notified, err := svc.Get(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notified == nil || notified.NotifiedAt == "" {
		t.Fatalf("target notified_at = %q, want a stamp", notified.NotifiedAt)
	}
	// The sibling is still queued (a broader UPDATE would silently drop its
	// notification) and the target is gone (a lost stamp would replay it).
	wantPending(t, pendingIDs(t, ctx, svc, 10), sibling.ID)

	if err := svc.MarkNotified(ctx, sibling.ID); err != nil {
		t.Fatalf("MarkNotified sibling: %v", err)
	}
	if drained := pendingIDs(t, ctx, svc, 10); len(drained) != 0 {
		t.Fatalf("pending queue after draining = %v, want empty", drained)
	}
	// The non-terminal grab was never notified by either call.
	stillAwaiting, err := svc.Get(ctx, inflight.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillAwaiting == nil || stillAwaiting.NotifiedAt != "" {
		t.Fatalf("in-flight grab notified_at = %q, want unset", stillAwaiting.NotifiedAt)
	}

	// Delete removes exactly the dismissed row.
	if err := svc.Delete(ctx, target.ID); err != nil {
		t.Fatalf("Delete target: %v", err)
	}
	gone, err := svc.ByJobID(ctx, jobA)
	if err != nil {
		t.Fatalf("ByJobID after delete: %v", err)
	}
	if gone != nil {
		t.Fatalf("ByJobID(%q) after Delete = %+v, want nil", jobA, gone)
	}
	if got, err := svc.Get(ctx, target.ID); err != nil || got != nil {
		t.Fatalf("Get after Delete = (%+v, %v), want (nil, nil)", got, err)
	}
	kept, err := svc.ByJobID(ctx, jobB)
	if err != nil {
		t.Fatalf("ByJobID sibling: %v", err)
	}
	if kept == nil || kept.ID != sibling.ID {
		t.Fatalf("sibling grab after Delete = %+v, want %q", kept, sibling.ID)
	}
	if got, err := svc.Get(ctx, inflight.ID); err != nil || got == nil {
		t.Fatalf("in-flight grab after Delete = (%+v, %v), want the row", got, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM pdf_grabs`); n != 2 {
		t.Fatalf("pdf_grabs after Delete = %d, want 2", n)
	}
}
