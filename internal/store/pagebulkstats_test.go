// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"math"
	"testing"
)

func TestPageBulkStatsEmptyTableReportsZeroesAndNoDenominator(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.PageBulkStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one (unknown-origin) row on an empty table", rows)
	}
	row := rows[0]
	if row.OriginClass != "" || row.TotalScanSessions != 0 || row.UsefulScanRate != 0 || row.SubmitConversion != 0 {
		t.Fatalf("row = %+v, want all-zero scalars on an empty table", row)
	}
	if row.BulkLeverage != nil {
		t.Fatalf("bulk_leverage = %v, want nil with no completed sheet", *row.BulkLeverage)
	}
	if row.RenderedRecordCountHint != nil || row.IdentifierYield != nil {
		t.Fatalf("row = %+v, want a null denominator and null yield on an empty table", row)
	}
}

func TestPageBulkStatsAggregatesScanAndSubmitRowsHonestly(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	insertScan := func(origin string, detectedRaw, canonicalUnique int, hint *int64) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO page_bulk_runs
				(detector_id, source_origin, detected_raw, canonical_unique, batch_id, opened_at, rendered_record_count_hint)
			VALUES ('generic-identifiers/1', ?, ?, ?, '', '2026-08-01T00:00:00Z', ?)`,
			origin, detectedRaw, canonicalUnique, hint); err != nil {
			t.Fatal(err)
		}
	}
	insertSubmit := func(batchID string, selected, submitted int) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO page_bulk_runs
				(detector_id, source_origin, selected, submitted, batch_id, opened_at, submitted_at)
			VALUES ('generic-identifiers/1', 'https://scholar.example.edu', ?, ?, ?, '2026-08-01T00:00:00Z', '2026-08-01T00:00:01Z')`,
			selected, submitted, batchID); err != nil {
			t.Fatal(err)
		}
	}

	hint := int64(10)
	// Unknown-origin scan rows (today's only real write shape): one page
	// with a recognized structural family and a hint, two without.
	insertScan("", 5, 3, &hint)
	insertScan("", 0, 0, nil)
	insertScan("", 2, 1, nil)
	// A distinct origin class, purely to prove the grouping mechanism
	// works — bridge.go never emits one today (page_bulk_status_request
	// carries no origin), but the query must not hardcode a single bucket.
	insertScan("https://scholar.example.edu", 1, 1, nil)

	insertSubmit("batch_bulk_00001", 3, 2)
	insertSubmit("batch_bulk_00002", 1, 1)

	rows, err := db.PageBulkStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 origin classes", rows)
	}
	byClass := make(map[string]PageBulkStatsRow, len(rows))
	for _, row := range rows {
		byClass[row.OriginClass] = row
	}

	unknown, ok := byClass[""]
	if !ok {
		t.Fatalf("rows = %+v, missing the unknown-origin class", rows)
	}
	if unknown.TotalScanSessions != 4 {
		t.Fatalf("total_scan_sessions = %d, want 4 (a global scalar over all 4 scan rows, not scoped to this origin class)", unknown.TotalScanSessions)
	}
	if got, want := unknown.UsefulScanRate, 3.0/4.0; got != want {
		t.Fatalf("useful_scan_rate = %v, want %v (3 of 4 scan rows across all origins had >=1 detection)", got, want)
	}
	if got, want := unknown.SubmitConversion, 2.0/4.0; got != want {
		t.Fatalf("submit_conversion = %v, want %v (2 submit rows with work, over 4 scan sessions)", got, want)
	}
	if unknown.BulkLeverage == nil || *unknown.BulkLeverage != 1.5 {
		t.Fatalf("bulk_leverage = %v, want 1.5 (median of [2, 1] submitted across 2 completed sheets)", unknown.BulkLeverage)
	}
	if unknown.CanonicalUnique != 4 {
		t.Fatalf("canonical_unique = %d, want 4 (3 + 0 + 1)", unknown.CanonicalUnique)
	}
	if unknown.RenderedRecordCountHint == nil || *unknown.RenderedRecordCountHint != 10 {
		t.Fatalf("rendered_record_count_hint = %v, want 10 (only one of three scan rows reported a hint)", unknown.RenderedRecordCountHint)
	}
	wantYield := 4.0 / 10.0
	if unknown.IdentifierYield == nil || math.Abs(*unknown.IdentifierYield-wantYield) > 1e-9 {
		t.Fatalf("identifier_yield = %v, want ~0.4 (4 canonical_unique / 10 rendered_record_count_hint)", unknown.IdentifierYield)
	}

	scholar, ok := byClass["https://scholar.example.edu"]
	if !ok {
		t.Fatalf("rows = %+v, missing the scholar.example.edu class", rows)
	}
	if scholar.CanonicalUnique != 1 {
		t.Fatalf("canonical_unique = %d, want 1", scholar.CanonicalUnique)
	}
	if scholar.RenderedRecordCountHint != nil || scholar.IdentifierYield != nil {
		t.Fatalf("row = %+v, want a null denominator and null yield — this class never reported a hint (no guessed value)", scholar)
	}
	// Global scalars are repeated verbatim on every row.
	if scholar.TotalScanSessions != unknown.TotalScanSessions || scholar.SubmitConversion != unknown.SubmitConversion {
		t.Fatalf("row = %+v, want the global scalars repeated from %+v", scholar, unknown)
	}
}
