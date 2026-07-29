-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- The acquisition edge: which artifacts one job obtained, in which role, from
-- which candidate, and what identity check that acquisition recorded.
--
-- Two defects share one root cause, so they share one table (ADR-0007):
--
--   * artifacts is content-addressed and shared across jobs, so its
--     identity_result is last-writer-wins. Identity is computed against a
--     per-job target, so a later acquisition of the same bytes silently
--     rewrites an earlier job's finding. Identity is a property of the
--     acquisition, not of the digest.
--   * jobs.artifact_sha256 is singular, so a job cannot retain a supplement or
--     an HTML full text alongside its main PDF.
--
-- role is acquisition-local for the same reason: identical bytes may be the
-- main file of one job and a supplement of another, so the role cannot live on
-- the shared artifacts row either.
--
-- jobs.artifact_sha256 deliberately REMAINS the main component. It is read by
-- the on_ready hook's frozen PAPIO_PDF/PAPIO_SHA256 contract (ADR-0004), by
-- zotio attach, by the retraction sweep, and by bundle export; keeping it
-- authoritative for the main file means this migration adds a capability
-- without rewriting those paths, and an older binary opening a migrated
-- database still reads and writes a coherent main component.

CREATE TABLE job_artifacts (
  job_id          TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  artifact_sha256 TEXT NOT NULL REFERENCES artifacts(sha256),
  role            TEXT NOT NULL
    CHECK (role IN ('main','html_fulltext','supplement','appendix')),
  candidate_id    INTEGER REFERENCES candidates(id),
  identity_result TEXT,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (job_id, role, artifact_sha256)
);
CREATE INDEX job_artifacts_by_artifact ON job_artifacts(artifact_sha256);

-- Backfill one main component per job that already holds an artifact, carrying
-- the identity finding that was current for it. Where two jobs share a digest
-- the shared artifacts.identity_result is the only value on record, so both
-- rows inherit it: the backfill cannot recover what a prior writer overwrote,
-- it can only stop the overwriting from continuing.
INSERT INTO job_artifacts (job_id, artifact_sha256, role, candidate_id, identity_result, created_at)
SELECT j.id, j.artifact_sha256, 'main', j.selected_candidate_id, a.identity_result, j.updated_at
  FROM jobs j
  JOIN artifacts a ON a.sha256 = j.artifact_sha256
 WHERE j.artifact_sha256 IS NOT NULL;
