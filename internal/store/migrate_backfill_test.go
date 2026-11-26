// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"papio/internal/store"
)

// openFixture rolls a schemaFixture forward through every migration and returns
// the store, closed when the test ends.
func openFixture(t *testing.T, version int, seed string) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), schemaFixture(t, version, seed))
	if err != nil {
		t.Fatalf("roll schema v%d fixture forward: %v", version, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// queryRows returns every row of query as strings, NULL as "<null>".
func queryRows(t *testing.T, db *store.Store, query string, args ...any) [][]string {
	t.Helper()
	rows, err := db.DB().QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		row := make([]string, len(cols))
		for i, c := range cells {
			row[i] = "<null>"
			if c.Valid {
				row[i] = c.String
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRows(t *testing.T, db *store.Store, query string, want [][]string) {
	t.Helper()
	got := queryRows(t, db, query)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s\n got  %v\n want %v", query, got, want)
	}
}

// 0016 backfills one 'main' job_artifacts row per job that already holds an
// artifact: the shared artifacts.identity_result is copied to every holder,
// the selected candidate carries over, created_at is the job's updated_at, and
// a job with no artifact (or one whose digest has no artifacts row) gets none.
func TestSchema16BackfillsMainJobArtifacts(t *testing.T) {
	t.Parallel()
	db := openFixture(t, 15, `
		INSERT INTO work_requests(id, created_at, desired_version) VALUES
		  ('r1','2026-06-01T00:00:00.000000000Z','any'), ('r2','2026-06-01T00:00:00.000000000Z','any'),
		  ('r3','2026-06-01T00:00:00.000000000Z','any'), ('r4','2026-06-01T00:00:00.000000000Z','any'),
		  ('r5','2026-06-01T00:00:00.000000000Z','any');
		INSERT INTO artifacts(sha256, size_bytes, mime, identity_result, path, created_at) VALUES
		  ('sha-shared', 10, 'application/pdf', 'match', '/a', '2026-06-01T00:00:00.000000000Z'),
		  ('sha-unchecked', 11, 'application/pdf', NULL, '/b', '2026-06-01T00:00:00.000000000Z');
		INSERT INTO jobs(id, work_request_id, state, policy_json, artifact_sha256, created_at, updated_at) VALUES
		  ('j1','r1','ready','{}','sha-shared','2026-06-01T00:00:00.000000000Z','2026-06-02T00:00:00.000000000Z'),
		  ('j2','r2','ready','{}','sha-shared','2026-06-01T00:00:00.000000000Z','2026-06-03T00:00:00.000000000Z'),
		  ('j3','r3','ready','{}','sha-unchecked','2026-06-01T00:00:00.000000000Z','2026-06-04T00:00:00.000000000Z'),
		  ('j4','r4','failed','{}','sha-dangling','2026-06-01T00:00:00.000000000Z','2026-06-05T00:00:00.000000000Z'),
		  ('j5','r5','resolving','{}',NULL,'2026-06-01T00:00:00.000000000Z','2026-06-06T00:00:00.000000000Z');
		INSERT INTO candidates(job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
		VALUES ('j1','oa','https://x/<redacted>','k1','published','oa','unknown','2026-06-01T00:00:00.000000000Z');
		UPDATE jobs SET selected_candidate_id = 1 WHERE id = 'j1';
	`)
	assertRows(t, db, `
		SELECT job_id, artifact_sha256, role, COALESCE(CAST(candidate_id AS TEXT), '<null>'), COALESCE(identity_result, '<null>'), created_at
		FROM job_artifacts ORDER BY job_id`, [][]string{
		{"j1", "sha-shared", "main", "1", "match", "2026-06-02T00:00:00.000000000Z"},
		{"j2", "sha-shared", "main", "<null>", "match", "2026-06-03T00:00:00.000000000Z"},
		{"j3", "sha-unchecked", "main", "<null>", "<null>", "2026-06-04T00:00:00.000000000Z"},
	})
	// jobs.artifact_sha256 stays authoritative for the main component.
	assertRows(t, db, `SELECT id, artifact_sha256 FROM jobs WHERE artifact_sha256 IS NOT NULL ORDER BY id`, [][]string{
		{"j1", "sha-shared"}, {"j2", "sha-shared"}, {"j3", "sha-unchecked"}, {"j4", "sha-dangling"},
	})
}

// 0011 classifies legacy parked handoffs from their detail text, and marks only
// the exact landing-page manual_download detail as blocked_by landing_page.
func TestSchema11BackfillsHumanActionClassification(t *testing.T) {
	t.Parallel()
	const landing = "a resolver returned a landing page but no verified direct PDF"
	db := openFixture(t, 10, fmt.Sprintf(`
		INSERT INTO work_requests(id, created_at, desired_version) VALUES ('r1','2026-06-01T00:00:00.000000000Z','any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('j1','r1','awaiting_human','{}','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at) VALUES
		  ('j1','manual_download','%[1]s','2026-06-01T00:00:01.000000000Z'),
		  ('j1','manual_download','download it yourself','2026-06-01T00:00:02.000000000Z'),
		  ('j1','verify_identity','%[1]s','2026-06-01T00:00:03.000000000Z'),
		  ('j1','manual_download','%[1]s (retry)','2026-06-01T00:00:04.000000000Z'),
		  ('j1','openurl_handoff','open-access fetch via browser' || char(10) || 'https://oa.example.test/p.pdf','2026-06-01T00:00:05.000000000Z'),
		  ('j1','openurl_handoff','institutional OpenURL handoff','2026-06-01T00:00:06.000000000Z');
	`, landing))
	assertRows(t, db, `SELECT created_at, kind, requires_auth, blocked_by FROM human_actions ORDER BY created_at`, [][]string{
		{"2026-06-01T00:00:01.000000000Z", "manual_download", "0", "landing_page"},
		{"2026-06-01T00:00:02.000000000Z", "manual_download", "0", ""},
		{"2026-06-01T00:00:03.000000000Z", "verify_identity", "0", ""},
		{"2026-06-01T00:00:04.000000000Z", "manual_download", "0", ""},
		{"2026-06-01T00:00:05.000000000Z", "openurl_handoff", "0", "anti_bot"},
		{"2026-06-01T00:00:06.000000000Z", "openurl_handoff", "1", "paywall"},
	})
}

// 0033 re-keys artifact_winners on (job_id, job_attempt_revision). Rows written
// under the old job-only key survive unchanged, and a second attempt of the
// same job can now record its own winner.
func TestSchema33PreservesArtifactWinners(t *testing.T) {
	t.Parallel()
	db := openFixture(t, 32, `
		INSERT INTO work_requests(id, created_at, desired_version) VALUES
		  ('r1','2026-06-01T00:00:00.000000000Z','any'), ('r2','2026-06-01T00:00:00.000000000Z','any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at) VALUES
		  ('j1','r1','ready','{}','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z'),
		  ('j2','r2','ready','{}','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z');
		INSERT INTO institution_profiles(id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES ('p1','profile',1,'digest','claim','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z');
		INSERT INTO browser_candidates(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		  route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id, adapter_revision,
		  effect_contract_id, status, created_at, updated_at) VALUES
		  ('bc1','j1',2,'p1',1,1,'institutional','doi','k1','d1','a','e','succeeded','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z'),
		  ('bc2','j2',1,'p1',1,1,'institutional','doi','k2','d2','a','e','succeeded','2026-06-01T00:00:00.000000000Z','2026-06-01T00:00:00.000000000Z');
		INSERT INTO artifact_winners(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES
		  ('j1',2,'bc1',7,'sha-one','2026-06-01T00:00:01.000000000Z'),
		  ('j2',1,'bc2',0,'sha-two','2026-06-01T00:00:02.000000000Z');
	`)
	assertRows(t, db, `
		SELECT job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at
		FROM artifact_winners ORDER BY job_id`, [][]string{
		{"j1", "2", "bc1", "7", "sha-one", "2026-06-01T00:00:01.000000000Z"},
		{"j2", "1", "bc2", "0", "sha-two", "2026-06-01T00:00:02.000000000Z"},
	})
	ctx := context.Background()
	// The old job-only key would reject attempt 1 of j1 next to its attempt 2.
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO browser_candidates(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		  route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id, adapter_revision,
		  effect_contract_id, status, created_at, updated_at)
		VALUES ('bc3','j1',3,'p1',1,1,'institutional','doi','k3','d3','a','e','succeeded','2026-06-01T00:00:03.000000000Z','2026-06-01T00:00:03.000000000Z');
		INSERT INTO artifact_winners(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
		VALUES ('j1',3,'bc3',0,'sha-three','2026-06-01T00:00:03.000000000Z')`); err != nil {
		t.Fatalf("second attempt winner rejected after re-key: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO artifact_winners(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
		VALUES ('j1',3,'bc3',0,'dup','2026-06-01T00:00:04.000000000Z')`); err == nil {
		t.Fatal("duplicate (job, attempt) winner accepted; primary key lost")
	}
	assertRows(t, db, `SELECT name FROM sqlite_master WHERE type='index' AND name='artifact_winners_by_candidate'`,
		[][]string{{"artifact_winners_by_candidate"}})
	// ON DELETE CASCADE from jobs survives the table rebuild.
	if _, err := db.DB().ExecContext(ctx, `DELETE FROM jobs WHERE id = 'j2'`); err != nil {
		t.Fatalf("delete job with winner: %v", err)
	}
	assertRows(t, db, `SELECT job_id FROM artifact_winners WHERE job_id = 'j2'`, nil)
}

// 0018 keys source_budgets by credential identity. Rows written before it carry
// an unknowable credential, so they move to identity 'legacy' with every other
// column intact, and a live identity then gets an independent row.
func TestSchema18BackfillsLegacySourceBudgetIdentity(t *testing.T) {
	t.Parallel()
	db := openFixture(t, 17, `
		INSERT INTO source_budgets(source, window_start, requests_in_window, spent_usd, next_allowed_at) VALUES
		  ('openalex', '2026-09', 42, 1.5, '2026-09-24T06:19:08.500000000Z'),
		  ('crossref', NULL, 0, 0, NULL);
	`)
	assertRows(t, db, `
		SELECT source, identity, COALESCE(window_start,'<null>'), requests_in_window, spent_usd, COALESCE(next_allowed_at,'<null>')
		FROM source_budgets ORDER BY source`, [][]string{
		{"crossref", "legacy", "<null>", "0", "0", "<null>"},
		{"openalex", "legacy", "2026-09", "42", "1.5", "2026-09-24T06:19:08.500000000Z"},
	})
	if _, err := db.DB().ExecContext(context.Background(),
		`INSERT INTO source_budgets(source, identity) VALUES ('openalex', 'anonymous')`); err != nil {
		t.Fatalf("a live identity cannot sit beside the legacy row: %v", err)
	}
	if _, err := db.DB().ExecContext(context.Background(),
		`INSERT INTO source_budgets(source, identity) VALUES ('openalex', 'legacy')`); err == nil {
		t.Fatal("duplicate (source, identity) accepted; composite primary key lost")
	}
}

// 0045 marks every job with an open action and a quiesce event; 0046 must remove
// the marker for any job that has no open openurl_handoff action, whether its
// open action is another kind or its handoff already resolved, while keeping
// the marker for a surfaceless job with an open handoff and leaving resets that
// 0045 did not write alone.
func TestHandoffEpochRepairRemovesMarkersWithoutOpenHandoff(t *testing.T) {
	t.Parallel()
	db := openFixture(t, 44, `
		INSERT INTO work_requests(id, created_at, desired_version) VALUES
		  ('r-open','2026-08-21T00:00:00.000000000Z','any'), ('r-manual','2026-08-21T00:00:00.000000000Z','any'),
		  ('r-resolved','2026-08-21T00:00:00.000000000Z','any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at) VALUES
		  ('j-open','r-open','awaiting_human','{}','2026-08-21T00:00:00.000000000Z','2026-08-21T00:00:00.000000000Z'),
		  ('j-manual','r-manual','awaiting_human','{}','2026-08-21T00:00:00.000000000Z','2026-08-21T00:00:00.000000000Z'),
		  ('j-resolved','r-resolved','awaiting_human','{}','2026-08-21T00:00:00.000000000Z','2026-08-21T00:00:00.000000000Z');
		INSERT INTO human_actions(job_id, kind, status, detail, created_at) VALUES
		  ('j-open','openurl_handoff','open','handoff','2026-08-21T00:00:00.000000000Z'),
		  ('j-manual','manual_download','open','manual','2026-08-21T00:00:00.000000000Z'),
		  ('j-resolved','openurl_handoff','resolved','handoff','2026-08-21T00:00:00.000000000Z'),
		  ('j-resolved','verify_identity','open','verify','2026-08-21T00:00:00.000000000Z');
		INSERT INTO events(job_id, at, kind, detail_json) VALUES
		  ('j-open','2026-08-21T00:00:01.000000000Z','browser.handoff_quiesced','{"reason":"fruitless_drive_limit"}'),
		  ('j-manual','2026-08-21T00:00:01.000000000Z','browser.handoff_quiesced','{"reason":"fruitless_drive_limit"}'),
		  ('j-resolved','2026-08-21T00:00:01.000000000Z','browser.handoff_quiesced','{"reason":"fruitless_drive_limit"}'),
		  ('j-manual','2026-08-21T00:00:02.000000000Z','browser.handoff_epochs_reset','{"reason":"operator"}');
	`)
	assertRows(t, db, `
		SELECT job_id, COALESCE(json_extract(detail_json, '$.migration'), '<none>')
		FROM events WHERE kind = 'browser.handoff_epochs_reset' ORDER BY job_id`, [][]string{
		{"j-manual", "<none>"}, // not a 0045 marker: untouched
		{"j-open", "0045"},
	})
}

// 0043 drops the claim_observation_journal foreign key to human_gate_observations
// by recreating the table: rows must survive, the recreated table must carry no
// foreign key, and the occurrence rollover (an UPDATE of the gate row's id) that
// the foreign key blocked must succeed with journal rows recorded against it.
func TestSchema43DropsJournalForeignKeyAndKeepsRows(t *testing.T) {
	t.Parallel()
	db := openFixture(t, 42, `
		INSERT INTO human_gate_observations(id, gate_type, scope_class, scope_key, observation_revision, status, created_at, updated_at)
		VALUES ('gate-old','human_gate.login','authentication_claim','claim-1',1,'open','2026-08-25T00:00:00.000000000Z','2026-08-25T00:00:00.000000000Z');
		INSERT INTO claim_observation_journal(observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		  browser_holder_generation, event_kind, event_ordinal, applied_at) VALUES
		  ('obs-1','gate-old','claim-1','binding-1',3,'wall_observed',0,'2026-08-25T00:00:01.000000000Z'),
		  ('obs-2','gate-old','claim-1','binding-1',3,'login_started',1,'2026-08-25T00:00:02.000000000Z');
	`)
	assertRows(t, db, `
		SELECT observation_id, gate_occurrence_id, authentication_claim_id, binding_id, browser_holder_generation, event_kind, event_ordinal, applied_at
		FROM claim_observation_journal ORDER BY event_ordinal`, [][]string{
		{"obs-1", "gate-old", "claim-1", "binding-1", "3", "wall_observed", "0", "2026-08-25T00:00:01.000000000Z"},
		{"obs-2", "gate-old", "claim-1", "binding-1", "3", "login_started", "1", "2026-08-25T00:00:02.000000000Z"},
	})
	assertRows(t, db, `SELECT "table" FROM pragma_foreign_key_list('claim_observation_journal')`, nil)
	assertRows(t, db, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='claim_observation_journal' AND name LIKE 'claim_observation_journal_%' ORDER BY name`,
		[][]string{{"claim_observation_journal_by_claim"}, {"claim_observation_journal_ordinal"}})
	ctx := context.Background()
	if _, err := db.DB().ExecContext(ctx, `UPDATE human_gate_observations SET id = 'gate-new' WHERE id = 'gate-old'`); err != nil {
		t.Fatalf("occurrence rollover blocked by journal rows: %v", err)
	}
	// Open also ran 0055, which widened the ordinal index to include binding_id,
	// so this checks the final shape; TestSchema43JournalUniquenessAtSchema43
	// checks the index 0043 itself leaves behind.
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO claim_observation_journal(observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		  browser_holder_generation, event_kind, event_ordinal, applied_at)
		VALUES ('obs-dup','gate-old','claim-1','binding-1',3,'mfa',1,'2026-08-25T00:00:03.000000000Z')`); err == nil {
		t.Fatal("duplicate (gate_occurrence_id, binding_id, event_ordinal) accepted; unique index lost")
	}
}

// store.Open runs every later migration, and 0055 rewrites the journal's
// ordinal index. Apply 0043 alone to a schema-42 database to check the index
// it leaves: unique on (gate_occurrence_id, event_ordinal), whatever the binding.
func TestSchema43JournalUniquenessAtSchema43(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := schemaFixture(t, 42, `
		INSERT INTO human_gate_observations(id, gate_type, scope_class, scope_key, observation_revision, status, created_at, updated_at)
		VALUES ('gate-old','human_gate.login','authentication_claim','claim-1',1,'open','2026-08-25T00:00:00.000000000Z','2026-08-25T00:00:00.000000000Z');
		INSERT INTO claim_observation_journal(observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		  browser_holder_generation, event_kind, event_ordinal, applied_at)
		VALUES ('obs-1','gate-old','claim-1','binding-1',3,'wall_observed',0,'2026-08-25T00:00:01.000000000Z');
	`)
	migration, err := os.ReadFile(filepath.Join("migrations", "0043_claim_observation_journal_fk.sql"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "papio.db")+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply 0043: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO claim_observation_journal(observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		  browser_holder_generation, event_kind, event_ordinal, applied_at)
		VALUES ('obs-dup','gate-old','claim-1','other-binding',3,'mfa',0,'2026-08-25T00:00:02.000000000Z')`); err == nil {
		t.Fatal("0043 left (gate_occurrence_id, event_ordinal) non-unique across bindings")
	}
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM claim_observation_journal`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("journal rows after 0043 = %d, %v; want 1", n, err)
	}
}

// 0056 rewrites every timestamp column, not just a few families. Each table
// below holds one row whose every timestamp is an offset RFC 3339 value that
// must come out as fixed-width UTC.
func TestSchema56RewritesTimestampsAcrossTableFamilies(t *testing.T) {
	t.Parallel()
	const in = "2026-01-01T02:00:00.5+08:00"
	const want = "2025-12-31T18:00:00.500000000Z"
	sha := strings.Repeat("a", 64)
	seed := strings.NewReplacer("@T", in, "@SHA", sha).Replace(`
		INSERT INTO work_requests(id, created_at, desired_version) VALUES ('r1','@T','any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, lease_expires_at, retry_at, created_at, updated_at)
		  VALUES ('j1','r1','resolving','{}','@T','@T','@T','@T');
		INSERT INTO attempts(job_id, stage, started_at, ended_at) VALUES ('j1','fetch','@T','@T');
		INSERT INTO candidates(job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
		  VALUES ('j1','oa','u','k','published','oa','unknown','@T');
		INSERT INTO artifacts(sha256, size_bytes, mime, path, created_at) VALUES ('@SHA',1,'application/pdf','/p','@T');
		INSERT INTO job_artifacts(job_id, artifact_sha256, role, created_at) VALUES ('j1','@SHA','main','@T');
		INSERT INTO artifact_publications(id, job_id, candidate_id, role, sha256, quarantine_path, prepared_at)
		  VALUES ('pub1','j1',1,'main','@SHA','/q','@T');
		INSERT INTO human_actions(job_id, kind, created_at, resolved_at, expires_at) VALUES ('j1','verify_identity','@T','@T','@T');
		INSERT INTO exports(job_id, kind, idempotency_key, created_at) VALUES ('j1','bundle','export-key','@T');
		INSERT INTO institution_profiles(id, configured_name, revision, authority_digest, authentication_claim_id, tombstoned_at, created_at, updated_at)
		  VALUES ('p1','profile',1,'digest','claim','@T','@T','@T');
		INSERT INTO browser_candidates(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		  route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id, adapter_revision,
		  effect_contract_id, status, created_at, updated_at)
		  VALUES ('bc1','j1',1,'p1',1,1,'institutional','doi','k','d','a','e','eligible','@T','@T');
		INSERT INTO artifact_winners(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
		  VALUES ('j1',1,'bc1',0,'@SHA','@T');
		INSERT INTO watches(label, query, filters_json, cadence_hours, per_run_cap, last_run_at, created_at)
		  VALUES ('l','q','{}',1,1,'@T','@T');
		INSERT INTO watch_digest_entries(watch_id, work_key, title, first_seen_at) VALUES (1,'w','t','@T');
		INSERT INTO notification_intents(category, event_kind, aggregate_key, phase, window_start, payload_json, first_at, last_at,
		  count, available_at, desktop_state, desktop_reserved_at, desktop_attempted_at, webhook_state, webhook_attempted_at)
		  VALUES ('c','e','a','p','@T','{}','@T','@T',1,'@T','pending','@T','@T','pending','@T');
		INSERT INTO delivery_requests(job_id, institution_profile, provider, request_class, work_identity, idempotency_key, state,
		  submitted_at, last_checked_at, next_check_at, created_at, updated_at, last_poll_at, last_successful_poll_at)
		  VALUES ('j1','i','p','c','w','delivery-key','offered','@T','@T','@T','@T','@T','@T','@T');
		INSERT INTO retraction_acks(doi, nature, acked_at) VALUES ('10.1/x','retraction','@T');
		INSERT INTO validation_reports(job_id, candidate_id, sha256, outcome, recorded_at, document)
		  VALUES ('j1',1,'@SHA','ok','@T','{}');
		INSERT INTO source_credit_fuse(source, utc_day, drift_closed_at) VALUES ('openalex','2026-01-01','@T');
		INSERT INTO source_budgets(source, identity, next_allowed_at) VALUES ('openalex','anonymous','@T');
		INSERT INTO zotio_item_scope(item_key, scope, observed_at) VALUES ('K','personal','@T');
		INSERT INTO zotio_tag_state(item_key, tag, updated_at) VALUES ('K','t','@T');
		INSERT INTO acquisition_batches(id, cohort_id, source_kind, source_label, expected_total, created_at, updated_at, closed_at, membership_state)
		  VALUES ('b1','cohort','k','l',1,'@T','@T','@T','open');
		INSERT INTO acquisition_batch_members(batch_id, ordinal, canonical_key, submission_outcome, created_at)
		  VALUES ('b1',0,'ck','ok','@T');
		INSERT INTO acquisition_batch_chunks(batch_id, chunk_index, request_id, final_chunk, canonical_keys_json, result_json, created_at)
		  VALUES ('b1',0,'req',1,'[]','{}','@T');
		INSERT INTO page_bulk_runs(detector_id, source_origin, opened_at, submitted_at) VALUES ('d','o','@T','@T');
		INSERT INTO pdf_grabs(id, url_host, state, notified_at, created_at, updated_at) VALUES ('g1','h','awaiting_file','@T','@T','@T');
		INSERT INTO pdf_grab_eligibility_snapshots(grab_id, recorded_at, phase, snapshot) VALUES ('g1','@T','pre_bind','{}');
		INSERT INTO profile_evidence(observation_id, browser_holder_generation, institution_profile_id, institution_profile_revision,
		  verdict, source, producer_observed_at, daemon_received_at, expires_at)
		  VALUES ('o1',0,'p1',1,'unknown','probe','@T','@T','@T');
		INSERT INTO authentication_entry_leases(authentication_claim_id, lease_id, owner_id, browser_holder_generation, state,
		  lease_until, evidence_observation_id, created_at, updated_at, entitled_at)
		  VALUES ('claim','lease','owner',0,'reserved','@T','o1','@T','@T','@T');
		INSERT INTO human_gate_observations(id, gate_type, scope_class, scope_key, observation_revision, status, created_at, updated_at)
		  VALUES ('hg1','human_gate.login','authentication_claim','claim',1,'open','@T','@T');
		INSERT INTO claim_observation_journal(observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
		  browser_holder_generation, event_kind, event_ordinal, applied_at)
		  VALUES ('ob1','hg1','claim','binding',0,'wall_observed',0,'@T');
		INSERT INTO daemon_authority_key(singleton, hmac_key, created_at) VALUES (1, zeroblob(32), '@T');
		INSERT INTO effect_permits(id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind,
		  drive_attempt_id, ordinal, strategy, revision, status, lease_until, created_at, updated_at)
		  VALUES ('ep1','j1',1,0,'dom','generic_drive','da',0,'generic','1','held','@T','@T','@T');
		INSERT INTO legacy_effect_blockers(id, effect_kind, safety_domain_id, grab_id, cleanup_only, status, created_at, updated_at)
		  VALUES ('lb1','pdf_grab','dom','g1',0,'unresolved','@T','@T');
		INSERT INTO close_authorizations(id, binding_id, browser_holder_generation, nonce, disposition, status, issued_at, consumed_at)
		  VALUES ('ca1','binding',0,'nonce','job_inactive','consumed','@T','@T');
		INSERT INTO materialization_claims(id, candidate_id, browser_holder_generation, materialization_kind, binding_id, phase,
		  lease_until, created_at, updated_at)
		  VALUES ('mc1','bc1',0,'browser_tab','binding','claimed','@T','@T','@T');
		INSERT INTO route_suppressions(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		  route_revision, safety_domain_id, adapter_revision, identifier_strategy, reason, active, created_at, updated_at)
		  VALUES ('rs1','j1',1,'p1',1,1,'dom','a','doi','no_entitlement',1,'@T','@T');
	`)
	db := openFixture(t, 55, seed)

	columns := map[string][]string{
		"work_requests":                  {"created_at"},
		"jobs":                           {"lease_expires_at", "retry_at", "created_at", "updated_at"},
		"attempts":                       {"started_at", "ended_at"},
		"candidates":                     {"created_at"},
		"artifacts":                      {"created_at"},
		"job_artifacts":                  {"created_at"},
		"artifact_publications":          {"prepared_at"},
		"human_actions":                  {"created_at", "resolved_at", "expires_at"},
		"exports":                        {"created_at"},
		"institution_profiles":           {"tombstoned_at", "created_at", "updated_at"},
		"browser_candidates":             {"created_at", "updated_at"},
		"artifact_winners":               {"created_at"},
		"watches":                        {"last_run_at", "created_at"},
		"watch_digest_entries":           {"first_seen_at"},
		"notification_intents":           {"window_start", "first_at", "last_at", "available_at", "desktop_reserved_at", "desktop_attempted_at", "webhook_attempted_at"},
		"delivery_requests":              {"submitted_at", "last_checked_at", "next_check_at", "created_at", "updated_at", "last_poll_at", "last_successful_poll_at"},
		"retraction_acks":                {"acked_at"},
		"validation_reports":             {"recorded_at"},
		"source_credit_fuse":             {"drift_closed_at"},
		"source_budgets":                 {"next_allowed_at"},
		"zotio_item_scope":               {"observed_at"},
		"zotio_tag_state":                {"updated_at"},
		"acquisition_batches":            {"created_at", "updated_at", "closed_at"},
		"acquisition_batch_members":      {"created_at"},
		"acquisition_batch_chunks":       {"created_at"},
		"page_bulk_runs":                 {"opened_at", "submitted_at"},
		"pdf_grabs":                      {"notified_at", "created_at", "updated_at"},
		"pdf_grab_eligibility_snapshots": {"recorded_at"},
		"profile_evidence":               {"producer_observed_at", "daemon_received_at", "expires_at"},
		"authentication_entry_leases":    {"lease_until", "created_at", "updated_at", "entitled_at"},
		"human_gate_observations":        {"created_at", "updated_at"},
		"claim_observation_journal":      {"applied_at"},
		"daemon_authority_key":           {"created_at"},
		"effect_permits":                 {"lease_until", "created_at", "updated_at"},
		"legacy_effect_blockers":         {"created_at", "updated_at"},
		"close_authorizations":           {"issued_at", "consumed_at"},
		"materialization_claims":         {"lease_until", "created_at", "updated_at"},
		"route_suppressions":             {"created_at", "updated_at"},
	}
	for table, cols := range columns {
		for _, col := range cols {
			rows := queryRows(t, db, fmt.Sprintf(`SELECT %s FROM %s`, col, table))
			if len(rows) != 1 || rows[0][0] != want {
				t.Errorf("%s.%s = %v, want exactly [[%s]]", table, col, rows, want)
			}
		}
	}
}
