-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- The publication journal is the durable owner between an artifact quarantine
-- file and its content-addressed path. A journal row commits before a caller
-- can make bytes visible, then finalization commits the acquisition edge and
-- removes the row in one transaction.
--
-- A candidate ID alone is not an ownership proof: candidate IDs are global.
-- The composite reference fences a publication candidate to its journal job.
CREATE UNIQUE INDEX candidates_job_id_id ON candidates(job_id, id);

CREATE TABLE artifact_publications (
  id                     TEXT PRIMARY KEY
    CHECK (length(id) BETWEEN 1 AND 128),
  job_id                 TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  candidate_id           INTEGER,
  role                   TEXT NOT NULL
    CHECK (role IN ('main', 'html_fulltext', 'supplement', 'appendix')),
  sha256                 TEXT NOT NULL REFERENCES artifacts(sha256)
    CHECK (length(sha256) = 64),
  quarantine_path        TEXT NOT NULL
    CHECK (length(quarantine_path) BETWEEN 1 AND 4096),
  lease_owner            TEXT,
  from_state             TEXT,
  to_state               TEXT,
  transition_detail_json TEXT,
  prepared_at            TEXT NOT NULL,
  FOREIGN KEY (job_id, candidate_id)
    REFERENCES candidates(job_id, id),
  CHECK ((from_state IS NULL) = (to_state IS NULL)),
  CHECK ((from_state IS NULL) = (transition_detail_json IS NULL))
);

-- Recovery reads one job's oldest durable intent before ordinary work resumes.
CREATE INDEX artifact_publications_by_job_prepared
  ON artifact_publications(job_id, prepared_at);
-- Last-reference cleanup must prove no other publication still owns this digest.
CREATE INDEX artifact_publications_by_sha256
  ON artifact_publications(sha256);
