// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"fmt"
	"testing"
)

func TestRecentEventsOrderingCursorJoinAndClamp(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO work_requests (id, created_at, title)
		VALUES ('wr_activity', '2026-01-01T00:00:00Z', 'Activity title')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('job_activity', 'wr_activity', 'awaiting_human', '{}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	insertEvent := func(jobID any, at, kind, detail string) int64 {
		t.Helper()
		result, err := db.DB().ExecContext(ctx,
			`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
			jobID, at, kind, detail)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}

	first := insertEvent("job_activity", "2026-01-01T00:00:01Z", "job.transition", `{"to":"queued"}`)
	second := insertEvent(nil, "2026-01-01T00:00:02Z", "daemon.ready", `{}`)
	third := insertEvent("job_activity", "2026-01-01T00:00:03Z", "job.transition", `{"to":"awaiting_human"}`)

	rows, err := db.RecentEvents(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Seq != third || rows[1].Seq != second || rows[2].Seq != first {
		t.Fatalf("rows = %+v, want newest-first sequence [%d %d %d]", rows, third, second, first)
	}
	if rows[0].JobID != "job_activity" || rows[0].JobTitle != "Activity title" || rows[0].JobState != "awaiting_human" {
		t.Fatalf("joined job = %+v, want id/title/state", rows[0])
	}
	if rows[1].JobID != "" || rows[1].Detail == nil {
		t.Fatalf("jobless event = %+v, want empty job and object detail", rows[1])
	}

	cursorRows, err := db.RecentEvents(10, third)
	if err != nil {
		t.Fatal(err)
	}
	if len(cursorRows) != 2 || cursorRows[0].Seq != second || cursorRows[1].Seq != first {
		t.Fatalf("cursor rows = %+v, want sequence [%d %d]", cursorRows, second, first)
	}

	filtered, err := db.RecentEventsForJob(10, 0, "job_activity")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 || filtered[0].Seq != third || filtered[1].Seq != first {
		t.Fatalf("filtered rows = %+v, want job events only", filtered)
	}

	for i := range ActivityLimitMax + 5 {
		insertEvent(nil, fmt.Sprintf("2026-01-02T00:%02d:%02dZ", i/60, i%60), "bulk", `{}`)
	}
	clamped, err := db.RecentEvents(ActivityLimitMax+100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(clamped) != ActivityLimitMax {
		t.Fatalf("clamped rows = %d, want %d", len(clamped), ActivityLimitMax)
	}
	page, truncated, err := db.RecentEventsPage(ActivityLimitMax, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != ActivityLimitMax || !truncated {
		t.Fatalf("page = %d rows, truncated=%v, want %d and true", len(page), truncated, ActivityLimitMax)
	}
}
