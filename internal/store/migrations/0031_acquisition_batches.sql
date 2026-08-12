CREATE TABLE acquisition_batches(
  id TEXT PRIMARY KEY,
  cohort_id TEXT NOT NULL UNIQUE,
  source_kind TEXT NOT NULL,
  source_label TEXT NOT NULL,
  source_detector TEXT,
  source_scan_id TEXT,
  expected_total INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at TEXT,
  membership_state TEXT NOT NULL
);
CREATE TABLE acquisition_batch_chunks(
  batch_id TEXT NOT NULL REFERENCES acquisition_batches(id) ON DELETE CASCADE,
  chunk_index INTEGER NOT NULL,
  request_id TEXT NOT NULL UNIQUE,
  final_chunk INTEGER NOT NULL,
  canonical_keys_json TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(batch_id, chunk_index)
);
CREATE TABLE acquisition_batch_members(
  batch_id TEXT NOT NULL REFERENCES acquisition_batches(id) ON DELETE CASCADE,
  ordinal INTEGER NOT NULL,
  canonical_key TEXT NOT NULL,
  job_id TEXT,
  submission_outcome TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(batch_id, ordinal),
  UNIQUE(batch_id, canonical_key)
);
CREATE INDEX acquisition_batch_members_job ON acquisition_batch_members(job_id);
