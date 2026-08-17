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
-- forever: allocation is idempotent per host and title, so every later Send PDF
-- for that paper is answered "existing" for a capture nothing can complete or
-- retire, and the six-hour stale sweep cannot clear it either. Tests never saw
-- it because they migrate a fresh database, which gets the edited constraint.
--
-- SQLite cannot alter a CHECK, so the table is rebuilt. Column list, indexes
-- and the partial unique indexes are reproduced from 0025 plus the columns
-- added by 0034 (effect_request_id) and 0037 (bind_provenance). On a database
-- created after the edit this is an identity rebuild.
--
-- No PRAGMA foreign_keys here: Store.migrate runs each migration inside a
-- transaction, and that pragma is a no-op inside one — writing it would state a
-- protection this file does not have. The rebuild is safe with enforcement left
-- on because nothing references pdf_grabs: its only foreign key is outbound
-- (job_id -> jobs.id), reproduced below, and no table, trigger or view in the
-- schema references it. A future rebuild of a table with *inbound* references
-- cannot copy this pattern — it needs PRAGMA defer_foreign_keys, which does
-- work inside a transaction.

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

-- The pdf_grabs_active_source unique index is deliberately NOT recreated here.
-- It was added by the same in-place edit that added 'abandoned', so a database
-- old enough to need this migration also lacks that index — and the Allocate of
-- that era was an unconditional INSERT with no existence check, so repeated
-- Send PDF calls for one paper really did leave several active rows. Creating a
-- unique index over them fails, the migration's transaction rolls back, and
-- Store.Open then refuses to open the database at all: papio would not start,
-- with no way for a researcher to recover.
--
-- Retiring the extra rows to make the index fit was tried and rejected. A
-- duplicate may be 'quarantined', which means it owns the only copy of a
-- paper's bytes, and SweepGrabs skips retired rows — so the repair would
-- silently discard a paper rather than file it. Under this project's cardinal
-- rule, no migration may guess which of two captures is the real one.
--
-- So this migration changes no data at all. ensurePdfGrabActiveSourceIndex in
-- store.go creates the index after migrating, tolerating exactly the duplicate
-- collision, and papio doctor reports a database left without it.

CREATE INDEX pdf_grabs_by_state ON pdf_grabs(state);
CREATE INDEX pdf_grabs_by_job ON pdf_grabs(job_id);
CREATE INDEX pdf_grabs_pending_notify ON pdf_grabs(notified_at) WHERE outcome != '';

-- pdf_grabs_effect_request_id came from 0034, a migration nobody edited, so it
-- has always been enforced and cannot have duplicates to collide with.

CREATE UNIQUE INDEX pdf_grabs_effect_request_id
  ON pdf_grabs(effect_request_id)
  WHERE effect_request_id <> '';
