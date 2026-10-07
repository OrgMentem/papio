-- Durable once-only consumers retain the exact chosen job, even after terminal state.
CREATE TABLE submission_receipts (
    request_id TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL,
    job_id TEXT NOT NULL REFERENCES jobs(id),
    created_at TEXT NOT NULL
);
CREATE INDEX submission_receipts_job ON submission_receipts(job_id);
