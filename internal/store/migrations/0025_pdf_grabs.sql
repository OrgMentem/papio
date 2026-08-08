-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0020 Decision 3/4: pdf_grabs is the durable record of one browser PDF
-- grab, from allocation through identification, so a grab survives a daemon
-- restart between the steered download landing and the sweeper claiming it.
--
-- Bytes never touch this row; url_host is the bare hostname only (ADR-0019
-- Decision 6's no-URL-telemetry discipline extends here), never the full
-- tab URL. title is the tab's own title as reported by the extension, kept
-- only to caption a triage inbox row.
--
-- state walks awaiting_file -> quarantined -> job_created, or parks at
-- parked_no_identifier (no front-matter identifier found) or
-- failed_validation (the settled file was not a valid PDF); 'identified'
-- records an operator-supplied identifier while the canonical job is being
-- created. quarantine_path binds the row to the quarantine copy internal/pdf's
-- structural validator inspected. job_id is set only once a canonical job
-- exists to carry the grab's outcome (either a freshly created job or an
-- already-live one the supplied/extracted identifier deduplicated onto).
--
-- outcome/detail/notified_at are poll()'s durable, crash-safe notification
-- queue: outcome is set at the same moment state becomes terminal (one of
-- "job_created", "already_owned", "needs_identifier", "failed_validation" —
-- finer-grained than state itself, since job_created covers both a fresh
-- job and an already-owned dedupe match); notified_at is stamped once the
-- daemon has pushed that outcome over pdf_grab_result at least once. A
-- daemon restart before delivery loses nothing: the row is re-queried by
-- outcome != '' AND notified_at IS NULL, never held only in memory.
CREATE TABLE pdf_grabs (
  id              TEXT PRIMARY KEY,
  url_host        TEXT NOT NULL,
  title           TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL
    CHECK (state IN ('awaiting_file','quarantined','identified','job_created','parked_no_identifier','failed_validation','abandoned')),
  quarantine_path TEXT NOT NULL DEFAULT '',
  job_id          TEXT REFERENCES jobs(id),
  outcome         TEXT NOT NULL DEFAULT '',
  detail          TEXT NOT NULL DEFAULT '',
  notified_at     TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);
CREATE INDEX pdf_grabs_by_state ON pdf_grabs(state);
CREATE INDEX pdf_grabs_by_job ON pdf_grabs(job_id);
CREATE INDEX pdf_grabs_pending_notify ON pdf_grabs(notified_at) WHERE outcome != '';

-- At most one in-flight grab may exist for a host/title. This closes the
-- cross-worker race; Allocate also serializes same-process callers.
CREATE UNIQUE INDEX pdf_grabs_active_source
  ON pdf_grabs(url_host, title)
  WHERE state IN ('awaiting_file','quarantined','identified','parked_no_identifier');
