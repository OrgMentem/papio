// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
)

// A store written before migration 0056 holds trimmed time.RFC3339Nano text,
// and the binary that opens it binds fixed-width bounds. Left as they were,
// the old rows would compare wrongly against those bounds inside a second.
// grab.Service.Allocate also bound a time.Time directly, which stored Go's
// String() form in local time.
func TestOpenRewritesStoredTimestampsToFixedWidth(t *testing.T) {
	ctx := context.Background()
	dataDir := schemaFixture(t, 55, `
		INSERT INTO events(job_id, at, kind, detail_json) VALUES
		  ('job-whole-second', '2026-09-24T06:19:05Z', 'artifact.producer', '{"producer":"manual"}'),
		  ('job-trimmed', '2026-09-24T06:19:06.59561Z', 'artifact.producer', '{"producer":"adapter"}'),
		  ('job-unrecorded', '2026-09-24T06:19:06.5956Z', 'job.transition', '{"to":"ready"}'),
		  ('job-0045', '2026-09-24T06:19:07.123000Z', 'browser.handoff_epochs_reset', '{}'),
		  ('job-fixed', '2026-09-24T06:19:07.000000001Z', 'fixture', '{}');
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at) VALUES
		  ('grab-go-string', 'example.org', 'Go String form', 'abandoned',
		   '2026-08-13 14:23:49.227869 +0800 AWST m=+21436.124126501', '2026-08-17T02:00:46.46978Z'),
		  ('grab-go-string-whole', 'example.org', 'Whole second west of UTC', 'awaiting_file',
		   '2026-08-12 23:59:59 -0530 -0530', '2026-08-12 23:59:59.5 -0530 -0530 m=+1.000000001'),
		  ('grab-unknown', 'example.org', 'Unknown form', 'abandoned', 'yesterday', '2026-08-13 14:23:49 AWST');
		INSERT INTO source_budgets(source, identity, window_start, next_allowed_at)
		VALUES ('openalex', 'anonymous', '2026-09', '2026-09-24T06:19:08.5Z');
	`)
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, want := range []struct{ query, value string }{
		{`SELECT at FROM events WHERE job_id = 'job-whole-second'`, "2026-09-24T06:19:05.000000000Z"},
		{`SELECT at FROM events WHERE job_id = 'job-trimmed'`, "2026-09-24T06:19:06.595610000Z"},
		{`SELECT at FROM events WHERE job_id = 'job-unrecorded'`, "2026-09-24T06:19:06.595600000Z"},
		{`SELECT at FROM events WHERE job_id = 'job-0045'`, "2026-09-24T06:19:07.123000000Z"},
		{`SELECT at FROM events WHERE job_id = 'job-fixed'`, "2026-09-24T06:19:07.000000001Z"},
		{`SELECT updated_at FROM pdf_grabs WHERE id = 'grab-go-string'`, "2026-08-17T02:00:46.469780000Z"},
		{`SELECT next_allowed_at FROM source_budgets`, "2026-09-24T06:19:08.500000000Z"},
		{`SELECT created_at FROM pdf_grabs WHERE id = 'grab-go-string'`, "2026-08-13T06:23:49.227869000Z"},
		{`SELECT created_at FROM pdf_grabs WHERE id = 'grab-go-string-whole'`, "2026-08-13T05:29:59.000000000Z"},
		{`SELECT updated_at FROM pdf_grabs WHERE id = 'grab-go-string-whole'`, "2026-08-13T05:29:59.500000000Z"},
		// Not instants papio can read: left exactly as they were.
		{`SELECT created_at FROM pdf_grabs WHERE id = 'grab-unknown'`, "yesterday"},
		{`SELECT updated_at FROM pdf_grabs WHERE id = 'grab-unknown'`, "2026-08-13 14:23:49 AWST"},
		{`SELECT window_start FROM source_budgets`, "2026-09"},
	} {
		var got string
		if err := db.DB().QueryRowContext(ctx, want.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", want.query, err)
		}
		if got != want.value {
			t.Errorf("%s = %q, want %q", want.query, got, want.value)
		}
	}

	// The rewritten rows now split a second by instant against the bounds the
	// new binary binds: the whole-second event lies before since, and the
	// trimmed ones lie before until.
	second := time.Date(2026, 9, 24, 6, 19, 5, 0, time.UTC)
	stats, err := (&job.Store{S: db}).ProducerStats(ctx, second.Add(100*time.Millisecond), second.Add(time.Second+595612*time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acquired != 1 || stats.Producers[job.ProducerAdapter] != 1 || stats.Producers[job.ProducerManual] != 0 || stats.Unrecorded != 1 {
		t.Fatalf("stats over migrated rows = %+v, want only the adapter event and the unrecorded promotion", stats)
	}
}
