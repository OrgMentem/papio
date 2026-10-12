-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- New lifecycle tables keep older watch IPC result shapes unchanged.
CREATE TABLE watch_backfill_sources (
  watch_id INTEGER PRIMARY KEY REFERENCES watches(id) ON DELETE CASCADE,
  source_name TEXT NOT NULL CHECK (length(source_name) BETWEEN 1 AND 128)
);
CREATE TABLE watch_backfill_submissions (
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  work_key TEXT NOT NULL,
  request_id TEXT NOT NULL UNIQUE,
  work_json TEXT NOT NULL,
  job_id TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY (watch_id, work_key)
);
CREATE TABLE publication_watches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id),
  source_doi TEXT NOT NULL,
  artifact_sha256 TEXT NOT NULL REFERENCES artifacts(sha256),
  artifact_version TEXT NOT NULL CHECK (artifact_version IN ('preprint','accepted')),
  cadence_hours INTEGER NOT NULL CHECK (cadence_hours BETWEEN 1 AND 87600),
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  created_at TEXT NOT NULL,
  last_run_at TEXT,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE publication_notices (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  watch_id INTEGER NOT NULL REFERENCES publication_watches(id) ON DELETE CASCADE,
  source_doi TEXT NOT NULL,
  target_doi TEXT NOT NULL,
  relation_type TEXT NOT NULL CHECK (relation_type = 'is-preprint-of'),
  provider TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  notified_at TEXT,
  request_id TEXT UNIQUE,
  acquired_job_id TEXT,
  UNIQUE (watch_id, target_doi)
);
