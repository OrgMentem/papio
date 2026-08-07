// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"sort"
)

// PageBulkStatsRow is one origin-class breakdown row of the page-bulk
// funnel/yield read model behind `papio stats page-bulk`
// (dev/post-build-followups.md item 3). TotalScanSessions, UsefulScanRate,
// BulkLeverage, and SubmitConversion are computed once over the whole table
// and repeated on every row, so a single-row page — today's only case, see
// OriginClass — already carries a complete answer; a future per-origin
// protocol addition needs no shape change here.
type PageBulkStatsRow struct {
	// OriginClass is the scan's source_origin bucket. Always "" today:
	// page_bulk_status_request — the only message carrying both the funnel
	// counts and the rendered-record hint — has no page-origin field
	// (ADR-0019 keeps origin extension-side until submit; only
	// page_bulk_submit_request.source.origin exists, on a differently
	// shaped row). The grouping is real, not decorative.
	OriginClass string `json:"origin_class"`

	// TotalScanSessions counts page_bulk_status_request calls: one
	// page_bulk_runs row per scan (bridge.go's pageBulkStatus), identified
	// by batch_id == "". ADR-0019 Decision 4 still holds — the daemon keeps
	// no in-memory scan-state across calls — but the funnel counts a single
	// status call already computes over its own request/response are now
	// persisted honestly instead of discarded.
	TotalScanSessions int `json:"total_scan_sessions"`
	// UsefulScanRate is the fraction of scan sessions with >=1 raw
	// detection (detected_raw >= 1). 0 when there are no scan sessions.
	UsefulScanRate float64 `json:"useful_scan_rate"`
	// BulkLeverage is the median number of works submitted per completed
	// selection sheet — a page_bulk_submit_request row with >=1 item
	// selected — the primary thesis metric page_bulk_runs was built to
	// answer (0022's migration comment). Nil when no sheet has completed.
	BulkLeverage *float64 `json:"bulk_leverage"`
	// SubmitConversion is submit rows with >=1 work actually submitted,
	// over total scan sessions. This is an aggregate-population ratio, not
	// a per-session join: the daemon has no scan_id correlation between a
	// status-time row and a later submit-time row (see OriginClass).
	SubmitConversion float64 `json:"submit_conversion"`

	// CanonicalUnique sums canonical_unique across this origin class's scan
	// rows — the numerator of IdentifierYield.
	CanonicalUnique int `json:"canonical_unique"`
	// RenderedRecordCountHint sums rendered_record_count_hint across scan
	// rows in this class that reported one. Nil when none did.
	RenderedRecordCountHint *int64 `json:"rendered_record_count_hint"`
	// IdentifierYield = CanonicalUnique / RenderedRecordCountHint. Nil —
	// "no denominator" — whenever RenderedRecordCountHint is nil or zero;
	// never a guessed or divide-by-zero value.
	IdentifierYield *float64 `json:"identifier_yield"`
}

// pageBulkRunRow is the raw page_bulk_runs row shape PageBulkStats reads.
// batch_id distinguishes how the row was written: pageBulkStatus (behind
// page_bulk_status_request) writes one row per scan with batch_id == "" and
// the funnel counts its own request/response already compute; pageBulkSubmit
// (page_bulk_submit_request) writes one row per submit attempt with a real
// batch_id and no funnel counts, since a submit call only ever sees the
// caller-selected subset, never the whole page.
type pageBulkRunRow struct {
	sourceOrigin            string
	detectedRaw             int
	canonicalUnique         int
	selected                int
	submitted               int
	batchID                 string
	renderedRecordCountHint sql.NullInt64
}

// PageBulkStats aggregates the lifetime page_bulk_runs table into the
// papio stats page-bulk read model. It reads the whole table and aggregates
// in Go, matching triage.Stats' and FailureSummaries' convention — the table
// is a lifetime, non-paginated measurement log, not a hot path.
func (s *Store) PageBulkStats(ctx context.Context) ([]PageBulkStatsRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_origin, detected_raw, canonical_unique, selected, submitted, batch_id, rendered_record_count_hint
		FROM page_bulk_runs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var all []pageBulkRunRow
	for rows.Next() {
		var r pageBulkRunRow
		if err := rows.Scan(&r.sourceOrigin, &r.detectedRaw, &r.canonicalUnique, &r.selected, &r.submitted, &r.batchID, &r.renderedRecordCountHint); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type originAgg struct {
		canonicalUnique int
		hintSum         int64
		hintPresent     bool
	}
	originClasses := map[string]bool{"": true}
	byOrigin := map[string]*originAgg{}
	var totalScans, usefulScans, submitsWithWork int
	var completedSheetSubmitted []int64

	for _, r := range all {
		if r.batchID == "" {
			totalScans++
			if r.detectedRaw >= 1 {
				usefulScans++
			}
			originClasses[r.sourceOrigin] = true
			agg := byOrigin[r.sourceOrigin]
			if agg == nil {
				agg = &originAgg{}
				byOrigin[r.sourceOrigin] = agg
			}
			agg.canonicalUnique += r.canonicalUnique
			if r.renderedRecordCountHint.Valid {
				agg.hintSum += r.renderedRecordCountHint.Int64
				agg.hintPresent = true
			}
			continue
		}
		if r.submitted > 0 {
			submitsWithWork++
		}
		if r.selected > 0 {
			completedSheetSubmitted = append(completedSheetSubmitted, int64(r.submitted))
		}
	}

	usefulScanRate := 0.0
	submitConversion := 0.0
	if totalScans > 0 {
		usefulScanRate = float64(usefulScans) / float64(totalScans)
		submitConversion = float64(submitsWithWork) / float64(totalScans)
	}
	var bulkLeverage *float64
	if len(completedSheetSubmitted) > 0 {
		m := medianInt64(completedSheetSubmitted)
		bulkLeverage = &m
	}

	classes := make([]string, 0, len(originClasses))
	for class := range originClasses {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	out := make([]PageBulkStatsRow, 0, len(classes))
	for _, class := range classes {
		row := PageBulkStatsRow{
			OriginClass:       class,
			TotalScanSessions: totalScans,
			UsefulScanRate:    usefulScanRate,
			BulkLeverage:      bulkLeverage,
			SubmitConversion:  submitConversion,
		}
		if agg := byOrigin[class]; agg != nil {
			row.CanonicalUnique = agg.canonicalUnique
			if agg.hintPresent {
				hint := agg.hintSum
				row.RenderedRecordCountHint = &hint
				if hint > 0 {
					yield := float64(agg.canonicalUnique) / float64(hint)
					row.IdentifierYield = &yield
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func medianInt64(values []int64) float64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	return (float64(sorted[n/2-1]) + float64(sorted[n/2])) / 2
}
