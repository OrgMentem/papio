-- artifact_winners was keyed by job_id alone, so it could hold at most one row
-- per job even though every column, index and caller treats the winner as a
-- decision about one job ATTEMPT. The consequence was silent: on attempt 2 the
-- insert-only CAS was swallowed by the attempt-1 row, the re-select by
-- (job_id, job_attempt_revision) found nothing, and the caller received a
-- conflict for bytes that had already been validated and attached. A retried
-- job could therefore never record a winner again, which left the
-- superseded-bytes refusal permanently disabled for the rest of its life.
--
-- Re-key on the pair the callers actually use. Existing rows are preserved and
-- remain valid: they already carry their attempt.
CREATE TABLE artifact_winners_next (
  job_id        TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  job_attempt_revision INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
  candidate_id  TEXT NOT NULL REFERENCES browser_candidates(id),
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  sha256        TEXT NOT NULL CHECK (length(sha256) BETWEEN 1 AND 128),
  created_at    TEXT NOT NULL,
  PRIMARY KEY (job_id, job_attempt_revision)
);

INSERT INTO artifact_winners_next
  (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
SELECT job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at
  FROM artifact_winners;

DROP TABLE artifact_winners;
ALTER TABLE artifact_winners_next RENAME TO artifact_winners;
CREATE INDEX artifact_winners_by_candidate ON artifact_winners(candidate_id);
