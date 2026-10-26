// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package store_test

import (
	"context"
	"testing"

	"papio/internal/store"
)

func TestSchema62GrabDismissalPreservesEligibilitySnapshots(t *testing.T) {
	dir := schemaFixture(t, 61, `
		INSERT INTO pdf_grabs(id, url_host, title, state, quarantine_path, outcome, created_at, updated_at, effect_request_id, bind_provenance)
		VALUES ('grab_schema62', 'pdf.example.org', 'Captured paper', 'parked_no_identifier', '/quarantine/grab.pdf',
		        'needs_identifier', '2026-10-26T00:00:00.000000000Z', '2026-10-26T00:00:00.000000000Z', 'effect-schema62', '{"rule":"preserved"}');
		INSERT INTO pdf_grab_eligibility_snapshots(grab_id, recorded_at, phase, snapshot)
		VALUES ('grab_schema62', '2026-10-26T00:00:00.000000000Z', 'pre_bind', '{"schema":"eligibility_pool_snapshot/1"}');
	`)
	ctx := context.Background()
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var state, path, provenance, snapshot string
	if err := s.DB().QueryRowContext(ctx, `
		SELECT g.state, g.quarantine_path, g.bind_provenance, e.snapshot
		FROM pdf_grabs g JOIN pdf_grab_eligibility_snapshots e ON e.grab_id = g.id
		WHERE g.id = 'grab_schema62'`).Scan(&state, &path, &provenance, &snapshot); err != nil {
		t.Fatal(err)
	}
	if state != "parked_no_identifier" || path != "/quarantine/grab.pdf" || provenance != `{"rule":"preserved"}` || snapshot != `{"schema":"eligibility_pool_snapshot/1"}` {
		t.Fatalf("migration changed durable capture data: %q %q %q %q", state, path, provenance, snapshot)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE pdf_grabs SET state = 'dismissing' WHERE id = 'grab_schema62'`); err != nil {
		t.Fatalf("new dismissal state rejected: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM pdf_grabs WHERE id = 'grab_schema62'`); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pdf_grab_eligibility_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Fatalf("rebuilt parent lost cascading cleanup: %d snapshots remain", snapshots)
	}
}
