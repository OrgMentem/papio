-- papio schema v20: per-consumer submitter attribution, and the durable
-- validation evidence a consumer needs to make its own rights/quality call.
--
-- work_requests.requester already records the transport PRINCIPAL (cli, mcp,
-- unknown). That answers "how did this arrive", never "who asked for it", so a
-- shared daemon's totals cannot be partitioned between the people using it.
-- consumer is deliberately nullable with no backfill and no default: a request
-- submitted before this column existed, or by a caller that named no consumer,
-- has no attribution, and inventing one ('', 'unknown', 'cli') would be a
-- fabricated fact rather than an absent one.
--
-- It lives on jobs, NOT on work_requests, because a submitter asks for an
-- ACQUISITION and a job is the acquisition. One work_request row outlives many
-- jobs: `INSERT OR IGNORE` reuses it whenever a request id is resubmitted, so a
-- consumer column there would make a fresh job silently inherit the previous
-- submitter's name once the earlier jobs went terminal — B's acquisition
-- reported and billed as A's. On jobs each acquisition carries its own
-- submitter, live convergence returns the original job with its original
-- attribution untouched, and a terminal resubmission is attributed to whoever
-- actually resubmitted it.
ALTER TABLE jobs ADD COLUMN consumer TEXT;
CREATE INDEX jobs_by_consumer ON jobs(consumer) WHERE consumer IS NOT NULL;

-- validation_reports keeps every stage's evidence from internal/pdf's
-- ValidationReport, which until now was computed, branched on, and discarded:
-- only the projections that fit the artifacts row (page_count, text_chars,
-- ocr_used, encrypted, has_active_content, identity_result) survived, so the
-- payload gate's reason, the structural rejection reason, and the identity and
-- capability evidence lines were unrecoverable after the fact.
--
-- Keyed by (job_id, candidate_id), NOT by sha256: an artifact is
-- content-addressed and shared across every job that obtained the same bytes,
-- and ADR-0007 forbids projecting one job's identity decision onto another's.
-- Re-validating the same candidate after a retry replaces its report.
--
-- document is a versioned JSON text blob (validation-report/1) rather than
-- columns, for the same reason bundle.document is text: it evolves under its
-- own schema_version instead of forcing a new RPC method per field.
CREATE TABLE validation_reports (
  job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  candidate_id INTEGER NOT NULL,
  sha256       TEXT NOT NULL,
  outcome      TEXT NOT NULL,
  recorded_at  TEXT NOT NULL,
  document     TEXT NOT NULL,
  PRIMARY KEY (job_id, candidate_id)
);
CREATE INDEX validation_reports_by_job ON validation_reports(job_id, recorded_at DESC);
