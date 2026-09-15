// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

func TestUnavailableRecheck(t *testing.T) {
	const windowDays = 14
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)

	request := func(id, doi string) protocol.WorkRequest {
		return protocol.WorkRequest{
			SchemaVersion:      protocol.WorkRequestSchemaVersion,
			RequestID:          id,
			Identifiers:        &protocol.Identifiers{DOI: doi},
			Title:              "A source-independent recheck",
			Authors:            []string{"A. Researcher"},
			Year:               2025,
			DesiredVersion:     "published",
			AccessModeOverride: "conservative",
			SourcesAllow:       []string{"fixture"},
		}
	}
	seedUnavailable := func(t *testing.T, svc *Service, jobs *job.Store, wr protocol.WorkRequest, reason job.TerminalReason, updatedAt time.Time) string {
		t.Helper()
		id, err := svc.Submit(context.Background(), wr)
		if err != nil {
			t.Fatalf("submit %s: %v", wr.RequestID, err)
		}
		if err := jobs.Transition(context.Background(), id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		if err := jobs.Transition(context.Background(), id, job.StateResolving, job.StateUnavailable, nil, job.WithTerminalReason(reason)); err != nil {
			t.Fatalf("terminalize %s: %v", id, err)
		}
		if _, err := jobs.S.DB().ExecContext(context.Background(), `UPDATE jobs SET updated_at = ? WHERE id = ?`, updatedAt.Format(time.RFC3339Nano), id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
		return id
	}
	recheckEvents := func(t *testing.T, jobs *job.Store, id string) []map[string]any {
		t.Helper()
		events, err := jobs.Events(context.Background(), id)
		if err != nil {
			t.Fatalf("events %s: %v", id, err)
		}
		var out []map[string]any
		for _, event := range events {
			if event["kind"] == "unavailable.recheck" {
				out = append(out, event)
			}
		}
		return out
	}
	newService := func(t *testing.T) (*Service, *job.Store) {
		t.Helper()
		svc, jobs := newTestService(t)
		svc.Config.Zotio.UnavailableRecheckDays = windowDays
		svc.Now = func() time.Time { return now }
		return svc, jobs
	}

	t.Run("eligible job is resubmitted once", func(t *testing.T) {
		svc, jobs := newService(t)
		svc.Config.Zotio.AutoImport = true
		wr := request("wr_recheck_due_001", "10.1000/recheck-due-001")
		oldID := seedUnavailable(t, svc, jobs, wr, job.TerminalReasonNoEntitlement, now.AddDate(0, 0, -windowDays-1))
		oldRow, err := jobs.Get(context.Background(), oldID)
		if err != nil {
			t.Fatal(err)
		}
		svc.Config.AccessMode = config.ModeDelegated
		svc.Config.Zotio.AutoImport = false
		runner := svc.UnavailableRechecker()

		if err := runner.RunDue(context.Background()); err != nil {
			t.Fatalf("first RunDue: %v", err)
		}
		rows, err := jobs.List(context.Background(), "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("jobs after first pass = %d, want 2", len(rows))
		}
		var fresh *job.Row
		for i := range rows {
			if rows[i].ID != oldID {
				fresh = &rows[i]
			}
		}
		if fresh == nil || (fresh.State != job.StateQueued && fresh.State != job.StateResolving) {
			t.Fatalf("fresh job = %+v, want queued or resolving", fresh)
		}
		if fresh.Work.Describe() != oldRow.Work.Describe() {
			t.Fatalf("fresh work = %q, want original work %q", fresh.Work.Describe(), oldRow.Work.Describe())
		}
		if fresh.Policy.AccessMode != oldRow.Policy.AccessMode {
			t.Fatalf("fresh access mode = %q, want old mode %q", fresh.Policy.AccessMode, oldRow.Policy.AccessMode)
		}
		if fresh.Policy.AutoImport != oldRow.Policy.AutoImport {
			t.Fatalf("fresh auto-import = %t, want old choice %t", fresh.Policy.AutoImport, oldRow.Policy.AutoImport)
		}
		events := recheckEvents(t, jobs, oldID)
		if len(events) != 1 {
			t.Fatalf("recheck events after first pass = %d, want 1", len(events))
		}
		detail := events[0]["detail"].(map[string]any)
		if detail["new_job_id"] != fresh.ID || detail["window_days"] != float64(windowDays) {
			t.Fatalf("recheck detail = %#v, want new job %s and window %d", detail, fresh.ID, windowDays)
		}

		if err := runner.RunDue(context.Background()); err != nil {
			t.Fatalf("second RunDue: %v", err)
		}
		rows, err = jobs.List(context.Background(), "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || len(recheckEvents(t, jobs, oldID)) != 1 {
			t.Fatalf("second pass produced jobs=%d events=%d, want 2 and 1", len(rows), len(recheckEvents(t, jobs, oldID)))
		}
	})

	t.Run("exempt reason is skipped", func(t *testing.T) {
		svc, jobs := newService(t)
		wr := request("wr_recheck_exempt_001", "10.1000/recheck-exempt-001")
		wr.Identifiers = nil
		oldID := seedUnavailable(t, svc, jobs, wr, job.TerminalReasonNoIdentifier, now.AddDate(0, 0, -windowDays-1))
		if err := svc.UnavailableRechecker().RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		rows, _ := jobs.List(context.Background(), "", 10)
		if len(rows) != 1 || len(recheckEvents(t, jobs, oldID)) != 0 {
			t.Fatalf("exempt pass produced jobs=%d events=%d, want 1 and 0", len(rows), len(recheckEvents(t, jobs, oldID)))
		}
	})

	t.Run("recent outcome is skipped", func(t *testing.T) {
		svc, jobs := newService(t)
		oldID := seedUnavailable(t, svc, jobs, request("wr_recheck_recent_001", "10.1000/recheck-recent-001"), job.TerminalReasonNoEntitlement, now.AddDate(0, 0, -windowDays+1))
		if err := svc.UnavailableRechecker().RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		rows, _ := jobs.List(context.Background(), "", 10)
		if len(rows) != 1 || len(recheckEvents(t, jobs, oldID)) != 0 {
			t.Fatalf("recent pass produced jobs=%d events=%d, want 1 and 0", len(rows), len(recheckEvents(t, jobs, oldID)))
		}
	})

	t.Run("existing live job blocks recheck", func(t *testing.T) {
		svc, jobs := newService(t)
		wr := request("wr_recheck_live_001", "10.1000/recheck-live-001")
		oldID := seedUnavailable(t, svc, jobs, wr, job.TerminalReasonNoEntitlement, now.AddDate(0, 0, -windowDays-1))
		liveRequest := wr
		liveRequest.RequestID = "wr_recheck_live_002"
		liveID, err := svc.Submit(context.Background(), liveRequest)
		if err != nil {
			t.Fatal(err)
		}
		if liveID == oldID {
			t.Fatal("setup did not create a live replacement")
		}
		if err := svc.UnavailableRechecker().RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		rows, _ := jobs.List(context.Background(), "", 10)
		if len(rows) != 2 || len(recheckEvents(t, jobs, oldID)) != 0 {
			t.Fatalf("blocked pass produced jobs=%d events=%d, want 2 and 0", len(rows), len(recheckEvents(t, jobs, oldID)))
		}
	})

	t.Run("ready or imported job blocks recheck", func(t *testing.T) {
		for _, state := range []string{job.StateReady, job.StateImported} {
			t.Run(state, func(t *testing.T) {
				svc, jobs := newService(t)
				wr := request("wr_recheck_delivered_old_"+state, "10.1000/recheck-delivered-"+state)
				oldID := seedUnavailable(t, svc, jobs, wr, job.TerminalReasonNoEntitlement, now.AddDate(0, 0, -windowDays-1))
				deliveredRequest := wr
				deliveredRequest.RequestID = "wr_recheck_delivered_new_" + state
				deliveredID, err := svc.Submit(context.Background(), deliveredRequest)
				if err != nil {
					t.Fatal(err)
				}
				if err := jobs.Transition(context.Background(), deliveredID, job.StateQueued, job.StateResolving, nil); err != nil {
					t.Fatal(err)
				}
				if err := jobs.Transition(context.Background(), deliveredID, job.StateResolving, job.StateReady, nil); err != nil {
					t.Fatal(err)
				}
				if state == job.StateImported {
					if err := jobs.Transition(context.Background(), deliveredID, job.StateReady, job.StateImported, nil); err != nil {
						t.Fatal(err)
					}
				}
				if err := svc.UnavailableRechecker().RunDue(context.Background()); err != nil {
					t.Fatal(err)
				}
				rows, _ := jobs.List(context.Background(), "", 10)
				if len(rows) != 2 || len(recheckEvents(t, jobs, oldID)) != 0 {
					t.Fatalf("blocked pass produced jobs=%d events=%d, want 2 and 0", len(rows), len(recheckEvents(t, jobs, oldID)))
				}
			})
		}
	})

	t.Run("batch is bounded and oldest first", func(t *testing.T) {
		svc, jobs := newService(t)
		oldest := make([]string, 0, unavailableRecheckScanLimit)
		newer := make([]string, 0, 3)
		for i := range unavailableRecheckScanLimit + 3 {
			wr := request(fmt.Sprintf("wr_recheck_batch_%03d", i), fmt.Sprintf("10.1000/recheck-batch-%03d", i))
			id := seedUnavailable(t, svc, jobs, wr, job.TerminalReasonNoEntitlement, now.AddDate(0, 0, -windowDays-10).Add(time.Duration(i)*time.Second))
			if i < unavailableRecheckScanLimit {
				oldest = append(oldest, id)
			} else {
				newer = append(newer, id)
			}
		}
		if err := svc.UnavailableRechecker().RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		var total int
		if err := jobs.S.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM jobs`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if total != 2*unavailableRecheckScanLimit+3 {
			t.Fatalf("jobs after bounded pass = %d, want %d", total, 2*unavailableRecheckScanLimit+3)
		}
		for _, id := range oldest {
			if len(recheckEvents(t, jobs, id)) != 1 {
				t.Fatalf("oldest job %s was not rechecked", id)
			}
		}
		for _, id := range newer {
			if len(recheckEvents(t, jobs, id)) != 0 {
				t.Fatalf("newer job %s was rechecked before the oldest batch", id)
			}
		}
	})
}
