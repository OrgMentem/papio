// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Events feeds the guards that cap retries, count decision budgets and match
// permit ids by their detail. A row whose detail_json does not decode used to
// become a nil detail: indistinguishable from an event that carried none, so a
// guard silently counted it as absent. It must now fail the read and name the
// job and sequence an operator has to repair.
func TestEventsRefusesCorruptDetail(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_events_corrupt", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	other, err := js.CreateRequest(ctx, "wr_events_clean", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.RecordEvent(ctx, id, "browser.agent_decision_requested", map[string]any{"permit_id": "permit-1"}); err != nil {
		t.Fatal(err)
	}
	if events, err := js.Events(ctx, id); err != nil || len(events) == 0 {
		t.Fatalf("clean events = %v, %v", events, err)
	}

	for _, tc := range []struct {
		name, detail string
	}{
		{"truncated", `{"permit_id":`},
		{"empty string", ``},
		{"not an object", `["permit_id"]`},
		{"not json", `permit_id=permit-1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seq int64
			if err := js.S.DB().QueryRowContext(ctx,
				`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, '2026-01-01T00:00:00.000000000Z', 'browser.agent_decision_requested', ?) RETURNING seq`,
				id, tc.detail).Scan(&seq); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := js.S.DB().ExecContext(context.Background(), `DELETE FROM events WHERE seq = ?`, seq); err != nil {
					t.Error(err)
				}
			})
			events, err := js.Events(ctx, id)
			if err == nil {
				t.Fatalf("Events returned %d events and no error for detail_json %q", len(events), tc.detail)
			}
			if events != nil {
				t.Fatalf("Events returned a partial stream alongside its error: %v", events)
			}
			for _, want := range []string{id, "browser.agent_decision_requested", "corrupt detail_json", fmt.Sprintf(" event %d ", seq)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			// The corruption is one job's: another job's history stays readable.
			if _, err := js.Events(ctx, other); err != nil {
				t.Fatalf("unrelated job events: %v", err)
			}
		})
	}
}

// A JSON null detail is a valid encoding of "no detail", and a job.created
// detail written before details were real JSON is the one documented legacy
// corruption (see TestRecheckOriginToleratesLegacyCorruptDetail). Neither may
// make the whole stream unreadable.
func TestEventsToleratesNullAndLegacyCreatedDetail(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	id, err := js.CreateRequest(ctx, "wr_events_legacy", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE events SET detail_json = ? WHERE job_id = ? AND kind = 'job.created'`,
		`{"request_id":"wr_events_legacy","work":"title:caf\xe9"}`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, '2026-01-01T00:00:00.000000000Z', 'job.note', 'null')`, id); err != nil {
		t.Fatal(err)
	}
	events, err := js.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawCreated, sawNull bool
	for _, event := range events {
		switch event["kind"] {
		case "job.created":
			sawCreated = true
		case "job.note":
			sawNull = true
		}
	}
	if !sawCreated || !sawNull {
		t.Fatalf("events = %v, want the legacy job.created and the null-detail event", events)
	}
}
