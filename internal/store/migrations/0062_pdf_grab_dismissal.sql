-- A durable dismissal claim excludes binding while filesystem cleanup runs
-- outside the database connection. The sweep resumes claims after a crash.
-- Rebuild the CHECK without changing existing rows or their audit snapshots.
-- Dropping the old parent cascades to eligibility snapshots, so preserve and
-- restore those rows within this migration transaction.
CREATE TEMP TABLE pdf_grab_snapshots_saved AS
  SELECT grab_id, recorded_at, phase, snapshot FROM pdf_grab_eligibility_snapshots;

CREATE TABLE pdf_grabs_rebuilt (
  id                 TEXT PRIMARY KEY,
  url_host           TEXT NOT NULL,
  title              TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL
    CHECK (state IN ('awaiting_file','quarantined','identified','job_created','parked_no_identifier','failed_validation','abandoned','dismissing')),
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

INSERT INTO pdf_grab_eligibility_snapshots (grab_id, recorded_at, phase, snapshot)
SELECT grab_id, recorded_at, phase, snapshot FROM pdf_grab_snapshots_saved;
DROP TABLE pdf_grab_snapshots_saved;

CREATE INDEX pdf_grabs_by_state ON pdf_grabs(state);
CREATE INDEX pdf_grabs_by_job ON pdf_grabs(job_id);
CREATE INDEX pdf_grabs_pending_notify ON pdf_grabs(notified_at) WHERE outcome != '';
CREATE UNIQUE INDEX pdf_grabs_effect_request_id
  ON pdf_grabs(effect_request_id) WHERE effect_request_id <> '';

-- As in 0038, store.ensurePdfGrabActiveSourceIndex restores the active-source
-- index after migration, tolerating duplicate captures from older databases.
