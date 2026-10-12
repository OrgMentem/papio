-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

ALTER TABLE jobs ADD COLUMN acquisition_disposition TEXT NOT NULL DEFAULT 'active'
  CHECK (acquisition_disposition IN ('active', 'archived'));

CREATE TABLE managed_filings (
  job_id TEXT NOT NULL REFERENCES jobs(id),
  destination TEXT NOT NULL,
  idempotency_key TEXT NOT NULL UNIQUE,
  artifact_sha256 TEXT NOT NULL REFERENCES artifacts(sha256),
  state TEXT NOT NULL CHECK (state IN ('pending', 'failed', 'filed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  retry_at TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  receipt_path TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(job_id, destination)
);
CREATE INDEX managed_filings_due ON managed_filings(state, retry_at);
