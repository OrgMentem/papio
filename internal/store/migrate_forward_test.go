// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store_test

import (
	"context"
	"database/sql"
	_ "embed"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/store"

	_ "modernc.org/sqlite"
)

func schema33Fixture(t *testing.T, seed string) string {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		base := filepath.Base(path)
		// This fixture must stay a schema-33 database: rolling the later
		// migrations forward is exactly what the test exercises, so applying one
		// here would make store.Open apply it twice. Deriving the bound from the
		// filename keeps the next migration from silently landing in the fixture
		// and failing as a duplicate column.
		if migrationNumber(t, base) > 33 {
			continue
		}
		migration, err := os.ReadFile(path)
		if err != nil {
			_ = raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, string(migration)); err != nil {
			_ = raw.Close()
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 33"); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, seed); err != nil {
		_ = raw.Close()
		t.Fatalf("seed schema-33 fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// migrationNumber reads the NNNN prefix every migration filename carries.
func migrationNumber(t *testing.T, base string) int {
	t.Helper()
	digits, _, ok := strings.Cut(base, "_")
	if !ok {
		t.Fatalf("migration %s: want a NNNN_name.sql filename", base)
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		t.Fatalf("migration %s: %v", base, err)
	}
	return number
}

func TestSchema33UpgradeImportsEveryLegacyEffectKind(t *testing.T) {
	const seed = `
		INSERT INTO work_requests(id, created_at, desired_version)
		VALUES ('req-generic', '2026-08-13T00:00:00Z', 'any'),
		       ('req-direct', '2026-08-13T00:00:00Z', 'any'),
		       ('req-inst', '2026-08-13T00:00:00Z', 'any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('job-generic', 'req-generic', 'resolving', '{}', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z'),
		       ('job-direct', 'req-direct', 'resolving', '{}', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z'),
		       ('job-inst', 'req-inst', 'resolving', '{}', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
		INSERT INTO events(job_id, at, kind, detail_json)
		VALUES ('job-generic', '2026-08-13T00:00:00Z', 'browser.provider_drive_epoch_started',
		        '{"drive_attempt_id":"legacy-generic","ordinal":0,"strategy":"generic","revision":"1","safety_domain":"domain:generic"}'),
		       ('job-generic', '2026-08-13T00:00:01Z', 'browser.provider_drive_epoch_superseded',
		        '{"drive_attempt_id":"legacy-generic","ordinal":0,"strategy":"generic","revision":"1"}'),
		       ('job-direct', '2026-08-13T00:00:00Z', 'browser.direct_route',
		        '{"phase":"offered","drive_attempt_id":"legacy-direct","ordinal":1,"route_revision":"route-1","safety_domain":"domain:direct"}');
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES ('legacy-grab', 'example.test', 'legacy grab', 'awaiting_file', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
		INSERT INTO institution_profiles
		  (id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES ('legacy-profile', 'legacy profile', 1, 'digest', 'auth-claim', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
		INSERT INTO browser_candidates
		  (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		   route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
		   adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES ('legacy-candidate', 'job-inst', 1, 'legacy-profile', 1, 1, 'institutional', 'doi',
		        'pre-route', 'domain:institutional', 'adapter-1', 'effect-1', 'claimed',
		        '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
		INSERT INTO materialization_claims
		  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
		   phase, route_issuance_ordinal, effect_ordinal, created_at, updated_at)
		VALUES ('legacy-claim', 'legacy-candidate', 1, 'browser_tab', 'legacy-binding',
		        'route_issued', 2, 3, '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
	`
	dir := schema33Fixture(t, seed)
	ctx := context.Background()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	js := &job.Store{S: db}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := db.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM legacy_effect_blockers WHERE status='unresolved'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("schema-33 upgrade imported %d blockers, want four kinds", total)
	}
	var generic, direct, grab, institutional int
	if err := db.DB().QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(effect_kind='generic_drive'),0),
		  COALESCE(SUM(effect_kind='direct_get'),0),
		  COALESCE(SUM(effect_kind='pdf_grab'),0),
		  COALESCE(SUM(effect_kind='institutional'),0)
		FROM legacy_effect_blockers WHERE status='unresolved'`).Scan(&generic, &direct, &grab, &institutional); err != nil {
		t.Fatal(err)
	}
	if generic != 1 || direct != 1 || grab != 1 || institutional != 1 {
		t.Fatalf("schema-33 imported kinds generic=%d direct=%d grab=%d institutional=%d", generic, direct, grab, institutional)
	}
}

func TestSchema33UpgradeRejectsMalformedLegacyStart(t *testing.T) {
	dir := schema33Fixture(t, `
		INSERT INTO work_requests(id, created_at, desired_version)
		VALUES ('req-malformed', '2026-08-13T00:00:00Z', 'any');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('job-malformed', 'req-malformed', 'resolving', '{}', '2026-08-13T00:00:00Z', '2026-08-13T00:00:00Z');
		INSERT INTO events(job_id, at, kind, detail_json)
		VALUES ('job-malformed', '2026-08-13T00:00:00Z', 'browser.provider_drive_epoch_started',
		        '{"drive_attempt_id":"malformed","ordinal":0,"strategy":"generic","revision":"1"}');
	`)
	ctx := context.Background()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = (&job.Store{S: db}).ImportLegacyStartedEpochs(ctx)
	if err == nil || !strings.Contains(err.Error(), "unclassifiable legacy provider drive effect") {
		t.Fatalf("malformed schema-33 start error = %v, want precise refusal", err)
	}
	var count int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_effect_blockers`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("malformed import left %d blockers, want rollback", count)
	}
}

//go:embed migrations/0001_init.sql
var schemaV1 string

//go:embed migrations/0013_zotio_tag_state.sql
var schemaV13 string

func TestOpenRollsForwardSchemaThirteenTagLedger(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("apply schema v1: %v", err)
	}
	if _, err := raw.ExecContext(ctx, schemaV13); err != nil {
		t.Fatalf("apply schema v13: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO zotio_tag_state (item_key, tag, updated_at)
		VALUES ('LEGACY13', 'papio:unavailable', '2026-07-23T00:00:00Z');
		PRAGMA user_version = 13;
	`); err != nil {
		t.Fatalf("seed schema v13: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("roll schema v13 forward: %v", err)
	}
	defer migrated.Close()
	version, err := migrated.UserVersion(ctx)
	if err != nil || version != 40 {
		t.Fatalf("user_version = %d, %v; want 40", version, err)

	}
	assertInstitutionalMaterializationSchema(t, ctx, migrated)

	var status string
	if err := migrated.DB().QueryRowContext(ctx,
		`SELECT status FROM zotio_tag_state WHERE item_key = 'LEGACY13'`,
	).Scan(&status); err != nil {
		t.Fatalf("read migrated tag ownership: %v", err)
	}
	if status != "owned" {
		t.Fatalf("migrated status = %q, want owned", status)
	}
	var scopes int
	if err := migrated.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM zotio_item_scope`).Scan(&scopes); err != nil {
		t.Fatalf("read new scope table: %v", err)
	}
	if scopes != 0 {
		t.Fatalf("migrated scope rows = %d, want 0", scopes)
	}
}

func TestOpenRollsForwardSchemaOneWithoutLosingDurableRows(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	// t.TempDir() is 0755 under the default umask, and doctor's privacy check
	// legitimately fails a group/world-readable data directory.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatalf("chmod data dir: %v", err)
	}
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open schema-v1 database: %v", err)
	}
	if _, err := raw.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("apply 0001 only: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set schema version one: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO work_requests(id, created_at, requester, title, authors_json, year, desired_version, access_mode_override)
		VALUES ('migration-request-0001', '2026-07-15T00:00:00Z', 'cli', 'Representative work', '["Ada Author"]', 2026, 'any', 'maximal');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('migration-job-0001', 'migration-request-0001', 'resolving', '{"access_mode":"conservative","desired_version":"any","fetch_max_bytes":1048576}', '2026-07-15T00:00:00Z', '2026-07-15T00:00:00Z');
		INSERT INTO jobs(id, work_request_id, state, policy_json, created_at, updated_at)
		VALUES ('migration-job-delegated-0001', 'migration-request-0001', 'ready', '{"access_mode":"maximal","desired_version":"any","fetch_max_bytes":1048576}', '2026-07-15T00:00:00Z', '2026-07-15T00:00:00Z');
		INSERT INTO candidates(job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
		VALUES ('migration-job-0001', 'browser', 'https://example.test/<redacted>', 'migration-candidate-key', 'published', 'institutional', 'unknown', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'verify_identity', 'inspect local copy', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'openurl_handoff', 'open-access fetch via browser' || char(10) || 'https://oa.example.test/paper.pdf', '2026-07-15T00:00:00Z');
		INSERT INTO human_actions(job_id, kind, detail, created_at)
		VALUES ('migration-job-0001', 'openurl_handoff', 'open-access candidates exhausted; institutional OpenURL handoff available in your browser', '2026-07-15T00:00:01Z');
		INSERT INTO exports(job_id, kind, idempotency_key, path, result_json, created_at)
		VALUES ('migration-job-0001', 'bundle', 'bundle:migration-job-0001:fixture', '/tmp/fixture', '{"fixture":true}', '2026-07-15T00:00:00Z');
	`); err != nil {
		t.Fatalf("seed schema-v1 rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close schema-v1 database: %v", err)
	}

	migrated, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open and roll forward: %v", err)
	}
	defer migrated.Close()
	version, err := migrated.UserVersion(ctx)
	if err != nil || version != 40 {
		t.Fatalf("user_version = %d, %v; want 40", version, err)

	}
	assertInstitutionalMaterializationSchema(t, ctx, migrated)

	var jobs, actions, exports int
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM human_actions").Scan(&actions); err != nil {
		t.Fatalf("count human actions: %v", err)
	}
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM exports").Scan(&exports); err != nil {
		t.Fatalf("count exports: %v", err)
	}
	if jobs != 2 || actions != 3 || exports != 1 {
		t.Fatalf("migrated durable rows jobs=%d actions=%d exports=%d, want 2/3/1", jobs, actions, exports)
	}

	var delegatedPolicy, delegatedOverride string
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT policy_json FROM jobs WHERE id = 'migration-job-delegated-0001'").Scan(&delegatedPolicy); err != nil {
		t.Fatalf("read delegated policy migration: %v", err)
	}
	if delegatedPolicy != `{"access_mode":"delegated","desired_version":"any","fetch_max_bytes":1048576}` {
		t.Fatalf("delegated policy = %q", delegatedPolicy)
	}
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT access_mode_override FROM work_requests WHERE id = 'migration-request-0001'").Scan(&delegatedOverride); err != nil {
		t.Fatalf("read delegated override migration: %v", err)
	}
	if delegatedOverride != "delegated" {
		t.Fatalf("delegated override = %q", delegatedOverride)
	}

	// 0011 backfill: legacy detail markers become structured classification.
	var oaAuth, instAuth int
	var oaBlocked, instBlocked string
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT requires_auth, blocked_by FROM human_actions WHERE kind = 'openurl_handoff' AND detail LIKE 'open-access fetch%'").Scan(&oaAuth, &oaBlocked); err != nil {
		t.Fatalf("read OA handoff backfill: %v", err)
	}
	if oaAuth != 0 || oaBlocked != "anti_bot" {
		t.Fatalf("OA handoff backfill = requires_auth %d blocked_by %q, want 0/anti_bot", oaAuth, oaBlocked)
	}
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT requires_auth, blocked_by FROM human_actions WHERE kind = 'openurl_handoff' AND detail LIKE 'open-access candidates exhausted%'").Scan(&instAuth, &instBlocked); err != nil {
		t.Fatalf("read institutional handoff backfill: %v", err)
	}
	if instAuth != 1 || instBlocked != "paywall" {
		t.Fatalf("institutional handoff backfill = requires_auth %d blocked_by %q, want 1/paywall", instAuth, instBlocked)
	}

	var spent float64
	if err := migrated.DB().QueryRowContext(ctx, "SELECT spent_usd FROM jobs WHERE id = 'migration-job-0001'").Scan(&spent); err != nil {
		t.Fatalf("read 0002 default: %v", err)
	}
	if spent != 0 {
		t.Fatalf("jobs.spent_usd = %v, want migration default 0", spent)
	}
	var accessBasis string
	var browserRoute, sessionEvidence sql.NullString
	var reviewOverride int
	if err := migrated.DB().QueryRowContext(ctx,
		"SELECT access_basis, browser_route, session_evidence, review_override FROM candidates WHERE job_id = 'migration-job-0001'").Scan(&accessBasis, &browserRoute, &sessionEvidence, &reviewOverride); err != nil {
		t.Fatalf("read migrated candidate: %v", err)
	}
	if accessBasis != "manual" || browserRoute.Valid || sessionEvidence.Valid || reviewOverride != 0 {
		t.Fatalf("candidate after migration = access_basis %q browser_route=%v session_evidence=%v review_override %d, want manual, NULL, NULL, 0", accessBasis, browserRoute, sessionEvidence, reviewOverride)
	}
	var watchCount int
	if err := migrated.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM watches").Scan(&watchCount); err != nil {
		t.Fatalf("query v5 watches table: %v", err)
	}
	if watchCount != 0 {
		t.Fatalf("new watches table count = %d, want 0", watchCount)
	}

	worker, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.Email = "reader@example.test"
	cfg.DataDir = dataDir
	report := doctor.Run(ctx, cfg, migrated, pdf.Capability{
		PDFToText: worker,
		PDFInfo:   worker,
		PDFToPPM:  worker,
		Tesseract: worker,
	}, worker, nil)
	if !report.OK {
		t.Fatalf("doctor after roll-forward is unhealthy: %+v", report)
	}
}
func assertInstitutionalMaterializationSchema(t *testing.T, ctx context.Context, db *store.Store) {
	t.Helper()
	const tables = `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name IN (
			'daemon_authority_key',
			'institution_profiles',
			'browser_candidates',
			'materialization_claims',
			'profile_evidence',
			'human_gate_observations',
			'route_suppressions',
			'artifact_winners',
			'authentication_entry_leases',
			'effect_permits',
			'legacy_effect_blockers'
		)`

	rows, err := db.DB().QueryContext(ctx, tables)
	if err != nil {
		t.Fatalf("list institutional materialization tables: %v", err)
	}
	defer rows.Close()
	foundTables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan institutional materialization table: %v", err)
		}
		foundTables[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate institutional materialization tables: %v", err)
	}
	for _, name := range []string{
		"daemon_authority_key",
		"institution_profiles",
		"browser_candidates",
		"materialization_claims",
		"profile_evidence",
		"human_gate_observations",
		"route_suppressions",
		"artifact_winners",
		"authentication_entry_leases",
		"effect_permits",
		"legacy_effect_blockers",
	} {
		if !foundTables[name] {
			t.Errorf("migration did not create table %q", name)
		}
	}
	tableColumns := map[string][]string{
		"effect_permits": {
			"id", "job_id", "job_attempt_revision", "browser_holder_generation",
			"safety_domain_id", "effect_kind", "slot_index", "drive_attempt_id",
			"ordinal", "strategy", "revision", "claim_id", "binding_id",
			"effect_ordinal", "grab_id", "terms_occurrence_id",
			"institutional_request_id", "status", "lease_until", "created_at",
			"updated_at",
		},
		"legacy_effect_blockers": {
			"id", "effect_kind", "job_id", "safety_domain_id", "drive_attempt_id", "ordinal",
			"strategy", "revision", "claim_id", "binding_id", "effect_ordinal", "grab_id",
			"reconstructed_attempt", "reconstructed_holder", "cleanup_only", "status", "created_at", "updated_at",
		},
	}
	for table, wantColumns := range tableColumns {
		rows, err := db.DB().QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			t.Fatalf("describe %s: %v", table, err)
		}
		foundColumns := map[string]bool{}
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull, primaryKey int
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s columns: %v", table, err)
			}
			foundColumns[name] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterate %s columns: %v", table, err)
		}
		_ = rows.Close()
		for _, name := range wantColumns {
			if !foundColumns[name] {
				t.Errorf("migration did not create %s column %q", table, name)
			}
		}
	}

	const indexes = `
		SELECT name FROM sqlite_master
		WHERE type = 'index' AND name IN (
			'institution_profiles_active_name',
			'browser_candidates_by_job',
			'browser_candidates_by_profile',
			'browser_candidates_schedule_keyset',
			'materialization_claims_by_candidate',
			'materialization_claims_live_candidate',
			'profile_evidence_by_profile',
			'profile_evidence_producer_observation',
			'human_gate_observations_by_status',
			'route_suppressions_by_job',
			'route_suppressions_active_exact',
			'artifact_winners_by_candidate',
			'authentication_entry_leases_by_expiry',
			'effect_permits_live_slot',
			'effect_permits_live_domain',
			'effect_permits_drive_identity',
			'effect_permits_pdf_grab_identity',
			'effect_permits_terms_identity',
			'effect_permits_institutional_request',
			'effect_permits_institutional_identity',
			'legacy_effect_blockers_drive_identity',
			'legacy_effect_blockers_pdf_grab_identity',
			'legacy_effect_blockers_institutional_identity',
			'effect_permits_by_job',
			'effect_permits_by_safety_domain',
			'effect_permits_unresolved_lookup',
			'legacy_effect_blockers_by_job',
			'legacy_effect_blockers_unresolved'
		)`

	rows, err = db.DB().QueryContext(ctx, indexes)
	if err != nil {
		t.Fatalf("list institutional materialization indexes: %v", err)
	}
	defer rows.Close()
	foundIndexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan institutional materialization index: %v", err)
		}
		foundIndexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate institutional materialization indexes: %v", err)
	}
	for _, name := range []string{
		"effect_permits_live_slot",
		"effect_permits_live_domain",
		"effect_permits_drive_identity",
		"effect_permits_pdf_grab_identity",
		"effect_permits_terms_identity",
		"effect_permits_institutional_request",
		"effect_permits_institutional_identity",
		"legacy_effect_blockers_drive_identity",
		"legacy_effect_blockers_pdf_grab_identity",
		"legacy_effect_blockers_institutional_identity",
		"effect_permits_by_job",
		"effect_permits_by_safety_domain",
		"effect_permits_unresolved_lookup",
		"legacy_effect_blockers_by_job",
		"legacy_effect_blockers_unresolved",
		"institution_profiles_active_name",
		"browser_candidates_by_job",
		"browser_candidates_by_profile",
		"browser_candidates_schedule_keyset",
		"materialization_claims_by_candidate",
		"materialization_claims_live_candidate",
		"profile_evidence_by_profile",
		"profile_evidence_producer_observation",
		"human_gate_observations_by_status",
		"route_suppressions_by_job",
		"route_suppressions_active_exact",
		"artifact_winners_by_candidate",
		"authentication_entry_leases_by_expiry",
	} {
		if !foundIndexes[name] {
			t.Errorf("migration did not create index %q", name)
		}
	}
}

// 0025_pdf_grabs.sql shipped without 'abandoned' in its state CHECK and was
// later edited in place to add it, so databases migrated in between kept the
// original constraint while every fresh one — including every test database —
// got the corrected one. That drift is invisible to the schema version and
// silently fatal: no abandonment can be recorded, so a capture sticks in
// awaiting_file, its tab answers "existing" for good, and the stale sweep
// cannot retire it either. 0038 rebuilds the table; this pins the repair
// against a database that actually carries the legacy constraint.
func TestOpenRepairsLegacyPdfGrabStateConstraint(t *testing.T) {
	ctx := context.Background()
	dataDir := schema33Fixture(t, "")
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the pre-edit table, its three original indexes, and its rows.
	// The index set matters: the unique one that keeps a paper to a single active
	// capture arrived with the same in-place edit, so a database this old does
	// not have it either.
	for _, stmt := range []string{
		`CREATE TABLE pdf_grabs_legacy (
			id              TEXT PRIMARY KEY,
			url_host        TEXT NOT NULL,
			title           TEXT NOT NULL DEFAULT '',
			state           TEXT NOT NULL
			  CHECK (state IN ('awaiting_file','quarantined','identified','job_created','parked_no_identifier','failed_validation')),
			quarantine_path TEXT NOT NULL DEFAULT '',
			job_id          TEXT REFERENCES jobs(id),
			outcome         TEXT NOT NULL DEFAULT '',
			detail          TEXT NOT NULL DEFAULT '',
			notified_at     TEXT,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		)`,
		`INSERT INTO pdf_grabs_legacy(id, url_host, title, state, created_at, updated_at)
		 VALUES ('grab-legacy-01', 'pdf.example.org', 'A Paper', 'awaiting_file',
		         '2026-08-17T00:00:00Z', '2026-08-17T00:00:00Z')`,
		`DROP TABLE pdf_grabs`,
		`ALTER TABLE pdf_grabs_legacy RENAME TO pdf_grabs`,
		`CREATE INDEX pdf_grabs_by_state ON pdf_grabs(state)`,
		`CREATE INDEX pdf_grabs_by_job ON pdf_grabs(job_id)`,
		`CREATE INDEX pdf_grabs_pending_notify ON pdf_grabs(notified_at) WHERE outcome != ''`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			_ = raw.Close()
			t.Fatalf("build legacy pdf_grabs: %v", err)
		}
	}
	// Prove the fixture reproduces the defect rather than assuming it.
	if _, err := raw.ExecContext(ctx, `UPDATE pdf_grabs SET state='abandoned' WHERE id='grab-legacy-01'`); err == nil {
		_ = raw.Close()
		t.Fatal("legacy fixture accepted an abandoned state; it does not reproduce the drift")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var host, state string
	if err := db.DB().QueryRowContext(ctx, `SELECT url_host, state FROM pdf_grabs WHERE id='grab-legacy-01'`).Scan(&host, &state); err != nil {
		t.Fatalf("legacy row lost in rebuild: %v", err)
	}
	if host != "pdf.example.org" || state != "awaiting_file" {
		t.Fatalf("row = (%q, %q), want (pdf.example.org, awaiting_file)", host, state)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE pdf_grabs SET state='abandoned' WHERE id='grab-legacy-01'`); err != nil {
		t.Fatalf("abandoned state still rejected after migration: %v", err)
	}
	// The columns later migrations added must survive the rebuild, and so must
	// the partial unique index that closes the cross-worker allocation race.
	if _, err := db.DB().ExecContext(ctx, `UPDATE pdf_grabs SET effect_request_id='req-legacy-01', bind_provenance='{}' WHERE id='grab-legacy-01'`); err != nil {
		t.Fatalf("columns from later migrations missing: %v", err)
	}
	var indexes int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='pdf_grabs' AND name IN ('pdf_grabs_by_state','pdf_grabs_by_job','pdf_grabs_pending_notify','pdf_grabs_active_source','pdf_grabs_effect_request_id')`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 5 {
		t.Fatalf("pdf_grabs indexes = %d, want 5", indexes)
	}
}

// A database predating the in-place edit also predates Allocate's existence
// check, so it can hold several active captures for one paper. The unique index
// cannot be created over those rows, and creating it inside the migration would
// mean Store.Open failing and papio refusing to start. Retiring the extra rows
// instead is not allowed either: a duplicate may be 'quarantined', holding the
// only copy of a paper's bytes, and no migration may guess which capture is the
// real one. So the database must open, keep every row, and go without the index
// until a human resolves the collision.
func TestOpenToleratesDuplicateActiveCaptures(t *testing.T) {
	ctx := context.Background()
	dataDir := schema33Fixture(t, "")
	dbPath := filepath.Join(dataDir, "papio.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		// The legacy state this models: the guard did not exist yet.
		`DROP INDEX IF EXISTS pdf_grabs_active_source`,
		`INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		 VALUES ('grab-dup-01', 'pdf.example.org', 'One Paper', 'quarantined',
		         '2026-08-17T00:00:00Z', '2026-08-17T00:00:00Z')`,
		`INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		 VALUES ('grab-dup-02', 'pdf.example.org', 'One Paper', 'awaiting_file',
		         '2026-08-17T01:00:00Z', '2026-08-17T01:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			_ = raw.Close()
			t.Fatalf("seed duplicate captures: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("Open refused a database with duplicate captures: %v", err)
	}
	defer db.Close()

	var rows int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pdf_grabs WHERE id IN ('grab-dup-01','grab-dup-02')`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("pdf_grabs rows = %d, want both duplicates kept", rows)
	}
	// Neither row may have been retired: the quarantined one owns bytes the
	// sweep still has to adopt, and SweepGrabs skips retired captures.
	var quarantined string
	if err := db.DB().QueryRowContext(ctx, `SELECT state FROM pdf_grabs WHERE id='grab-dup-01'`).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined != "quarantined" {
		t.Fatalf("grab-dup-01 state = %q, want quarantined", quarantined)
	}
	var index int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='pdf_grabs_active_source'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if index != 0 {
		t.Fatal("pdf_grabs_active_source exists despite duplicate active captures")
	}

	// Once the collision is resolved the guard installs itself on the next open.
	if _, err := db.DB().ExecContext(ctx, `UPDATE pdf_grabs SET state='job_created' WHERE id='grab-dup-02'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if err := reopened.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='pdf_grabs_active_source'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if index != 1 {
		t.Fatal("pdf_grabs_active_source still absent after the collision was resolved")
	}
}
