-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0019 Decision 10: local-only, URL-free measurement for on-page bulk
-- acquisition. No telemetry leaves the machine; source_origin is bare
-- scheme+host only, never path, query, fragment, or page title (Decision 6).
--
-- Every counter tracks one funnel stage from raw DOM candidates through to a
-- submitted job, so the primary thesis metric — median works submitted per
-- completed selection sheet — and the pilot/expand/retreat gates can be
-- computed without a second source of truth.
CREATE TABLE page_bulk_runs (
  id                     INTEGER PRIMARY KEY AUTOINCREMENT,
  detector_id            TEXT NOT NULL,
  source_origin          TEXT NOT NULL,
  detected_raw           INTEGER NOT NULL DEFAULT 0,
  canonical_unique       INTEGER NOT NULL DEFAULT 0,
  eligible               INTEGER NOT NULL DEFAULT 0,
  owned_with_pdf         INTEGER NOT NULL DEFAULT 0,
  owned_missing_pdf      INTEGER NOT NULL DEFAULT 0,
  queued                 INTEGER NOT NULL DEFAULT 0,
  ownership_incomplete   INTEGER NOT NULL DEFAULT 0,
  selected               INTEGER NOT NULL DEFAULT 0,
  submitted              INTEGER NOT NULL DEFAULT 0,
  invalid                INTEGER NOT NULL DEFAULT 0,
  batch_id               TEXT NOT NULL DEFAULT '',
  opened_at              TEXT NOT NULL,
  submitted_at           TEXT
);
CREATE INDEX page_bulk_runs_by_opened ON page_bulk_runs(opened_at);
