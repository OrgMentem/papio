// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"testing"
)

func TestFailureSummariesUseLatestDecisiveEventsAndSwitchGrouping(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	insertJob := func(id, state, updatedAt, source, candidateURL string) {
		t.Helper()
		workID := "wr_" + id
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, ?)`,
			workID, "2026-08-01T00:00:00Z", id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
			 VALUES (?, ?, ?, '{}', '2026-08-01T00:00:00Z', ?)`,
			id, workID, state, updatedAt); err != nil {
			t.Fatal(err)
		}
		result, err := db.DB().ExecContext(ctx,
			`INSERT INTO candidates
			 (job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
			 VALUES (?, ?, ?, ?, 'any', 'unknown', 'unknown', '2026-08-01T00:00:00Z')`,
			id, source, candidateURL, "key_"+id)
		if err != nil {
			t.Fatal(err)
		}
		candidateID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET selected_candidate_id = ? WHERE id = ?`, candidateID, id); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent := func(jobID, kind, detail string) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, '2026-08-01T00:00:01Z', ?, ?)`,
			jobID, kind, detail); err != nil {
			t.Fatal(err)
		}
	}

	insertJob("job_jstor_transition", "unavailable", "2026-08-01T00:00:01Z", "jstor", "https://www.jstor.org/stable/1")
	insertEvent("job_jstor_transition", "job.transition", `{"reason":"no_entitlement"}`)
	insertEvent("job_jstor_transition", "diagnostic.noise", `{"reason":"must_not_win"}`)

	insertJob("job_jstor_outcome", "awaiting_human", "2026-08-01T00:00:04Z", "jstor", "https://www.jstor.org/stable/2")
	insertEvent("job_jstor_outcome", "job.transition", `{"reason":"login_required"}`)
	insertEvent("job_jstor_outcome", "browser.provider_outcome", `{"outcome":"no_entitlement"}`)

	insertJob("job_informit_error", "awaiting_human", "2026-08-01T00:00:03Z", "informit", "not-a-url")
	insertEvent("job_informit_error", "browser.error", `{"code":"adapter_timeout"}`)

	insertJob("job_ebsco_outcome", "unavailable", "2026-08-01T00:00:02Z", "ebsco", "https://search.ebscohost.com/item/3")
	insertEvent("job_ebsco_outcome", "browser.provider_outcome", `{"outcome":"no_entitlement"}`)

	// Failed is terminal, but this read model is intentionally restricted to
	// the requested unavailable/awaiting_human operator cohort.
	insertJob("job_failed_excluded", "failed", "2026-08-01T00:00:05Z", "jstor", "https://www.jstor.org/stable/4")
	insertEvent("job_failed_excluded", "browser.error", `{"code":"adapter_timeout"}`)

	byReason, truncated, err := db.FailureSummaries(ctx, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(byReason) != 2 {
		t.Fatalf("by-reason page = %+v, truncated=%v, want two complete groups", byReason, truncated)
	}
	if got := byReason[0]; got.Reason != "no_entitlement" || got.Provider != "multiple" || got.Count != 3 || got.ExampleJobID != "job_jstor_outcome" {
		t.Fatalf("first by-reason group = %+v", got)
	}
	if got := byReason[1]; got.Reason != "adapter_timeout" || got.Provider != "informit" || got.Count != 1 || got.ExampleJobID != "job_informit_error" {
		t.Fatalf("second by-reason group = %+v", got)
	}

	byProvider, truncated, err := db.FailureSummaries(ctx, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(byProvider) != 3 {
		t.Fatalf("by-provider page = %+v, truncated=%v, want three complete groups", byProvider, truncated)
	}
	if got := byProvider[0]; got.Provider != "www.jstor.org" || got.Reason != "no_entitlement" || got.Count != 2 || got.ExampleJobID != "job_jstor_outcome" {
		t.Fatalf("jstor provider group = %+v", got)
	}

	limited, truncated, err := db.FailureSummaries(ctx, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(limited) != 1 || limited[0].Count != 3 {
		t.Fatalf("limited page = %+v, truncated=%v, want top group plus truncation proof", limited, truncated)
	}
}

func TestFailureSummariesExampleJobIDIsChronologicalNotLexical(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	insertJob := func(id, state, updatedAt, source, candidateURL string) {
		t.Helper()
		workID := "wr_" + id
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, ?)`,
			workID, "2026-08-01T00:00:00Z", id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
			 VALUES (?, ?, ?, '{}', '2026-08-01T00:00:00Z', ?)`,
			id, workID, state, updatedAt); err != nil {
			t.Fatal(err)
		}
		result, err := db.DB().ExecContext(ctx,
			`INSERT INTO candidates
			 (job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
			 VALUES (?, ?, ?, ?, 'any', 'unknown', 'unknown', '2026-08-01T00:00:00Z')`,
			id, source, candidateURL, "key_"+id)
		if err != nil {
			t.Fatal(err)
		}
		candidateID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `UPDATE jobs SET selected_candidate_id = ? WHERE id = ?`, candidateID, id); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent := func(jobID, kind, detail string) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, '2026-08-01T00:00:01Z', ?, ?)`,
			jobID, kind, detail); err != nil {
			t.Fatal(err)
		}
	}

	// job_a_early's updated_at has no fractional seconds; job_b_late's does
	// and is genuinely the later update (01.5s beats 01s). store.Now() formats
	// with time.RFC3339Nano, which omits the fraction entirely when it is
	// zero, so a byte-wise comparison sorts 'Z' (0x5A) above '.' (0x2E) and
	// would pick job_a_early's fraction-less timestamp as "more recent" even
	// though it is chronologically earlier than job_b_late's.
	insertJob("job_a_early", "unavailable", "2026-08-01T00:00:01Z", "jstor", "https://www.jstor.org/stable/1")
	insertEvent("job_a_early", "job.transition", `{"reason":"no_entitlement"}`)

	insertJob("job_b_late", "unavailable", "2026-08-01T00:00:01.5Z", "jstor", "https://www.jstor.org/stable/2")
	insertEvent("job_b_late", "job.transition", `{"reason":"no_entitlement"}`)

	byReason, _, err := db.FailureSummaries(ctx, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(byReason) != 1 {
		t.Fatalf("summaries = %+v, want one group", byReason)
	}
	if got := byReason[0].ExampleJobID; got != "job_b_late" {
		t.Fatalf("example job id = %q, want job_b_late (chronologically latest, not lexically greatest)", got)
	}
}
