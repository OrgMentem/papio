// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"context"
	"fmt"
	"strings"
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

// Migration 0056 must agree with the parser papio reads timestamps with: a
// value time.RFC3339Nano accepts becomes FormatTime of that exact instant,
// whatever its offset or fraction width, and a value it rejects stays as it is.
func TestOpenRewritesEveryRFC3339FormToUTCAndLeavesOtherTextAlone(t *testing.T) {
	ctx := context.Background()
	converted := map[string]string{
		// Offsets of both signs, across a day, a month and a year.
		"2026-01-01T02:00:00+08:00":           "2025-12-31T18:00:00.000000000Z",
		"2026-12-31T23:30:00.5-05:00":         "2027-01-01T04:30:00.500000000Z",
		"2026-02-28T22:15:00.123456789-02:45": "2026-03-01T01:00:00.123456789Z",
		"2026-03-01T00:10:00+05:30":           "2026-02-28T18:40:00.000000000Z",
		"2024-03-01T01:00:00.25+14:00":        "2024-02-29T11:00:00.250000000Z",
		"2026-09-24T06:19:05-00:00":           "2026-09-24T06:19:05.000000000Z",
		"2026-09-24T06:19:05+00:00":           "2026-09-24T06:19:05.000000000Z",
		// Every fraction width in UTC; nine digits is already the fixed form.
		"2026-09-24T06:19:05Z":           "2026-09-24T06:19:05.000000000Z",
		"2026-09-24T06:19:05.1Z":         "2026-09-24T06:19:05.100000000Z",
		"2026-09-24T06:19:05.12Z":        "2026-09-24T06:19:05.120000000Z",
		"2026-09-24T06:19:05.123Z":       "2026-09-24T06:19:05.123000000Z",
		"2026-09-24T06:19:05.1234Z":      "2026-09-24T06:19:05.123400000Z",
		"2026-09-24T06:19:05.12345Z":     "2026-09-24T06:19:05.123450000Z",
		"2026-09-24T06:19:05.123456Z":    "2026-09-24T06:19:05.123456000Z",
		"2026-09-24T06:19:05.1234567Z":   "2026-09-24T06:19:05.123456700Z",
		"2026-09-24T06:19:05.12345678Z":  "2026-09-24T06:19:05.123456780Z",
		"2026-09-24T06:19:05.123456789Z": "2026-09-24T06:19:05.123456789Z",
		"2026-09-24T06:19:05.000000000Z": "2026-09-24T06:19:05.000000000Z",
		// Every fraction width with an offset.
		"2026-09-24T14:19:05.1+08:00":         "2026-09-24T06:19:05.100000000Z",
		"2026-09-24T14:19:05.1234+08:00":      "2026-09-24T06:19:05.123400000Z",
		"2026-09-24T01:19:05.12345678-05:00":  "2026-09-24T06:19:05.123456780Z",
		"2026-09-24T01:19:05.000000001-05:00": "2026-09-24T06:19:05.000000001Z",
	}
	untouched := []string{
		"2026-13-99T88:77:66Z",
		"2026-00-10T00:00:00Z",
		"2026-02-29T00:00:00Z", // not a leap year
		"2026-04-31T00:00:00Z",
		"2026-01-01T24:00:00Z",
		"2026-01-01T23:60:00Z",
		"2026-01-01T23:59:60Z",
		"2026-01-01T00:00:00.Z",
		"2026-01-01T00:00:00.12a4Z",
		"2026-01-01T00:00:00",
		"2026-01-01T00:00:00+0800",
		"2026-01-01T00:00:00+8:00",
		"2026-01-01 00:00:00Z",
		"2026-09",
		"not a timestamp",
	}
	var seed strings.Builder
	seed.WriteString(`INSERT INTO events(job_id, at, kind, detail_json) VALUES `)
	values := make([]string, 0, len(converted)+len(untouched))
	for old := range converted {
		values = append(values, old)
	}
	values = append(values, untouched...)
	for i, old := range values {
		if i > 0 {
			seed.WriteString(", ")
		}
		fmt.Fprintf(&seed, "('case-%d', '%s', 'fixture', '{}')", i, old)
	}
	db, err := store.Open(ctx, schemaFixture(t, 55, seed.String()+";"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	stored := func(i int) string {
		t.Helper()
		var got string
		if err := db.DB().QueryRowContext(ctx, `SELECT at FROM events WHERE job_id = ?`, fmt.Sprintf("case-%d", i)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for i, old := range values {
		got := stored(i)
		parsed, parseErr := time.Parse(time.RFC3339Nano, old)
		if want, ok := converted[old]; ok {
			if parseErr != nil || store.FormatTime(parsed) != want {
				t.Fatalf("case %q: Go reads %s (%v), which is not the expected %q", old, parsed, parseErr, want)
			}
			if got != want {
				t.Errorf("0056 rewrote %q to %q, want %q", old, got, want)
			}
			continue
		}
		if parseErr == nil {
			t.Fatalf("case %q is a valid time.RFC3339Nano value, so it cannot be left as it is", old)
		}
		if got != old {
			t.Errorf("0056 rewrote non-timestamp text %q to %q, want it unchanged", old, got)
		}
	}
}
