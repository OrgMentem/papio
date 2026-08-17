-- Normalise pdf_grabs.state's CHECK constraint so 'abandoned' is writable.
--
-- 0025_pdf_grabs.sql shipped without 'abandoned' in the constraint and was
-- later edited in place to add it. Editing an applied migration only changes
-- what *new* databases get: every database migrated in between kept the
-- original constraint, and no schema version records the difference. Those
-- databases cannot record an abandonment at all — MarkAbandoned,
-- MarkAbandonedForRequest, MarkAbandonedUnoccupied and AbandonStaleAwaiting
-- each fail with a CHECK violation, which the browser bridge reports as
-- "conflict". The observable symptom is a capture stuck in awaiting_file
-- forever: allocation is idempotent per host, so every later Send PDF for that
-- tab is answered "existing" for a capture nothing can complete or retire, and
-- the six-hour stale sweep cannot clear it either. Tests never saw it because
-- they migrate a fresh database, which gets the edited constraint.
--
-- SQLite cannot alter a CHECK, so the table is rebuilt. Column list, indexes
-- and the partial unique indexes are reproduced from 0025 plus the columns
-- added by 0034 (effect_request_id) and 0037 (bind_provenance). On a database
-- created after the edit this is an identity rebuild.
PRAGMA foreign_keys=OFF;

CREATE TABLE pdf_grabs_rebuilt (
  id                 TEXT PRIMARY KEY,
  url_host           TEXT NOT NULL,
  title              TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL
    CHECK (state IN ('awaiting_file','quarantined','identified','job_created','parked_no_identifier','failed_validation','abandoned')),
  quarantine_path    TEXT NOT NULL DEFAULT '',
  job_id             TEXT REFERENCES jobs(id),
  outcome            TEXT NOT NULL DEFAULT '',
  detail             TEXT NOT NULL DEFAULT '',
  notified_at        TEXT,
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  effect_request_id  TEXT NOT NULL DEFAULT '',
  bind_provenance    TEXT
);

INSERT INTO pdf_grabs_rebuilt (
  id, url_host, title, state, quarantine_path, job_id, outcome, detail,
  notified_at, created_at, updated_at, effect_request_id, bind_provenance
)
SELECT
  id, url_host, title, state, quarantine_path, job_id, outcome, detail,
  notified_at, created_at, updated_at, effect_request_id, bind_provenance
FROM pdf_grabs;

DROP TABLE pdf_grabs;
ALTER TABLE pdf_grabs_rebuilt RENAME TO pdf_grabs;

CREATE INDEX pdf_grabs_by_state ON pdf_grabs(state);
CREATE INDEX pdf_grabs_by_job ON pdf_grabs(job_id);
CREATE INDEX pdf_grabs_pending_notify ON pdf_grabs(notified_at) WHERE outcome != '';

-- At most one in-flight grab may exist for a host/title. This closes the
-- cross-worker race; Allocate also serializes same-process callers.
CREATE UNIQUE INDEX pdf_grabs_active_source
  ON pdf_grabs(url_host, title)
  WHERE state IN ('awaiting_file','quarantined','identified','parked_no_identifier');

CREATE UNIQUE INDEX pdf_grabs_effect_request_id
  ON pdf_grabs(effect_request_id)
  WHERE effect_request_id <> '';

PRAGMA foreign_keys=ON;
