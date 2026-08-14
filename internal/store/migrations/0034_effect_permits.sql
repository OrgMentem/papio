-- PDF grab request identity is the durable replay fence. Legacy rows retain
-- the empty default and can never authorize a replay.
ALTER TABLE pdf_grabs ADD COLUMN effect_request_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX pdf_grabs_effect_request_id
  ON pdf_grabs(effect_request_id)
  WHERE effect_request_id <> '';
-- Leased effect permit (Decision 2) and legacy blockers (Decision 8).
-- Occupancy: slot_index = 0 single global slot + per-domain occupying, both for held|unknown_completion.
-- Kind-specific identities enforced by CHECKs and partial unique indexes; SQLite NULL-as-distinct
-- means nullable composite uniqueness alone would leave a loophole.

CREATE TABLE effect_permits (
  id TEXT PRIMARY KEY,
  job_id TEXT REFERENCES jobs(id) ON DELETE CASCADE,
  job_attempt_revision INTEGER NOT NULL CHECK (job_attempt_revision >= 0),
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  safety_domain_id TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
  effect_kind TEXT NOT NULL CHECK (effect_kind IN
    ('generic_drive','direct_get','pdf_grab','terms','institutional')),
  slot_index INTEGER NOT NULL DEFAULT 0 CHECK (slot_index = 0),
  drive_attempt_id TEXT,
  ordinal INTEGER,
  strategy TEXT,
  revision TEXT,
  claim_id TEXT,
  binding_id TEXT,
  effect_ordinal INTEGER,
  grab_id TEXT,
  terms_occurrence_id TEXT,
  institutional_request_id TEXT,
  status TEXT NOT NULL CHECK (status IN ('held','unknown_completion','settled')),
  lease_until TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (
    (effect_kind = 'generic_drive'
      AND job_id IS NOT NULL AND job_attempt_revision >= 1
      AND drive_attempt_id IS NOT NULL AND ordinal IS NOT NULL
      AND strategy IS NOT NULL AND strategy <> 'direct_get' AND revision IS NOT NULL
      AND claim_id IS NULL AND binding_id IS NULL
      AND effect_ordinal IS NULL AND grab_id IS NULL
      AND terms_occurrence_id IS NULL AND institutional_request_id IS NULL)
    OR
    (effect_kind = 'direct_get'
      AND job_id IS NOT NULL AND job_attempt_revision >= 1
      AND drive_attempt_id IS NOT NULL AND ordinal IS NOT NULL
      AND strategy = 'direct_get' AND revision IS NOT NULL
      AND claim_id IS NULL AND binding_id IS NULL
      AND effect_ordinal IS NULL AND grab_id IS NULL
      AND terms_occurrence_id IS NULL AND institutional_request_id IS NULL)
    OR
    (effect_kind = 'pdf_grab'
      AND job_id IS NULL AND job_attempt_revision = 0
      AND grab_id IS NOT NULL
      AND drive_attempt_id IS NULL AND ordinal IS NULL
      AND strategy IS NULL AND revision IS NULL
      AND claim_id IS NULL AND binding_id IS NULL AND effect_ordinal IS NULL
      AND terms_occurrence_id IS NULL AND institutional_request_id IS NULL)
    OR
    (effect_kind = 'terms'
      AND job_id IS NOT NULL AND job_attempt_revision >= 1
      AND terms_occurrence_id IS NOT NULL
      AND claim_id IS NULL AND binding_id IS NULL
      AND drive_attempt_id IS NULL AND ordinal IS NULL
      AND strategy IS NULL AND revision IS NULL
      AND grab_id IS NULL AND effect_ordinal IS NULL
      AND institutional_request_id IS NULL)
    OR
    (effect_kind = 'institutional'
      AND job_id IS NOT NULL AND job_attempt_revision >= 1
      AND claim_id IS NOT NULL AND binding_id IS NOT NULL
      AND effect_ordinal IS NOT NULL AND institutional_request_id IS NOT NULL
      AND drive_attempt_id IS NULL AND ordinal IS NULL
      AND strategy IS NULL AND revision IS NULL AND grab_id IS NULL
      AND terms_occurrence_id IS NULL)
  )
);
CREATE UNIQUE INDEX effect_permits_live_slot
  ON effect_permits(slot_index)
  WHERE status IN ('held','unknown_completion');
CREATE UNIQUE INDEX effect_permits_live_domain
  ON effect_permits(safety_domain_id)
  WHERE status IN ('held','unknown_completion');
CREATE UNIQUE INDEX effect_permits_drive_identity
  ON effect_permits(job_id, drive_attempt_id, ordinal, strategy, revision)
  WHERE effect_kind IN ('generic_drive','direct_get');
CREATE UNIQUE INDEX effect_permits_pdf_grab_identity
  ON effect_permits(grab_id)
  WHERE effect_kind = 'pdf_grab';
CREATE UNIQUE INDEX effect_permits_terms_identity
  ON effect_permits(job_id, terms_occurrence_id)
  WHERE effect_kind = 'terms';
CREATE UNIQUE INDEX effect_permits_institutional_request
  ON effect_permits(institutional_request_id)
  WHERE effect_kind = 'institutional';
CREATE UNIQUE INDEX effect_permits_institutional_identity
  ON effect_permits(claim_id, binding_id, effect_ordinal)
  WHERE effect_kind = 'institutional';

CREATE TABLE legacy_effect_blockers (
  id TEXT PRIMARY KEY,
  effect_kind TEXT NOT NULL CHECK (effect_kind IN
    ('generic_drive','direct_get','pdf_grab','institutional')),
  job_id TEXT REFERENCES jobs(id) ON DELETE CASCADE,
  safety_domain_id TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
  drive_attempt_id TEXT,
  ordinal INTEGER,
  strategy TEXT,
  revision TEXT,
  claim_id TEXT,
  binding_id TEXT,
  effect_ordinal INTEGER,
  grab_id TEXT,
  reconstructed_attempt INTEGER,
  reconstructed_holder INTEGER,
  cleanup_only INTEGER NOT NULL CHECK (cleanup_only IN (0, 1)),
  status TEXT NOT NULL CHECK (status IN ('unresolved','settled')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (
    (effect_kind = 'generic_drive'
      AND job_id IS NOT NULL AND drive_attempt_id IS NOT NULL
      AND ordinal IS NOT NULL AND ordinal >= 0
      AND strategy IS NOT NULL AND strategy <> 'direct_get' AND revision IS NOT NULL
      AND claim_id IS NULL AND binding_id IS NULL
      AND effect_ordinal IS NULL AND grab_id IS NULL)
    OR
    (effect_kind = 'direct_get'
      AND job_id IS NOT NULL AND drive_attempt_id IS NOT NULL
      AND ordinal IS NOT NULL AND ordinal >= 0
      AND strategy = 'direct_get' AND revision IS NOT NULL
      AND claim_id IS NULL AND binding_id IS NULL
      AND effect_ordinal IS NULL AND grab_id IS NULL)
    OR
    (effect_kind = 'pdf_grab'
      AND job_id IS NULL AND grab_id IS NOT NULL
      AND drive_attempt_id IS NULL AND ordinal IS NULL
      AND strategy IS NULL AND revision IS NULL
      AND claim_id IS NULL AND binding_id IS NULL AND effect_ordinal IS NULL)
    OR
    (effect_kind = 'institutional'
      AND job_id IS NOT NULL AND claim_id IS NOT NULL AND binding_id IS NOT NULL
      AND effect_ordinal IS NOT NULL AND effect_ordinal >= 1
      AND drive_attempt_id IS NULL AND ordinal IS NULL
      AND strategy IS NULL AND revision IS NULL AND grab_id IS NULL)
  )
);
CREATE UNIQUE INDEX legacy_effect_blockers_drive_identity
  ON legacy_effect_blockers(job_id, drive_attempt_id, ordinal, strategy, revision)
  WHERE effect_kind IN ('generic_drive','direct_get');
CREATE UNIQUE INDEX legacy_effect_blockers_pdf_grab_identity
  ON legacy_effect_blockers(grab_id)
  WHERE effect_kind = 'pdf_grab';
CREATE UNIQUE INDEX legacy_effect_blockers_institutional_identity
  ON legacy_effect_blockers(claim_id, binding_id, effect_ordinal)
  WHERE effect_kind = 'institutional';

-- Lookup indexes for live/domain/unresolved queries. Non-unique and do not
-- weaken the partial unique constraints above; they only accelerate the
-- occupying-status and per-job scans used by LiveEffectPermit,
-- DomainEffectPermit and UnresolvedLegacyEffectBlockers.
CREATE INDEX effect_permits_by_job ON effect_permits(job_id);
CREATE INDEX effect_permits_by_safety_domain ON effect_permits(safety_domain_id);
CREATE INDEX effect_permits_unresolved_lookup ON effect_permits(status) WHERE status IN ('held','unknown_completion');
CREATE INDEX legacy_effect_blockers_by_job ON legacy_effect_blockers(job_id);
CREATE INDEX legacy_effect_blockers_unresolved ON legacy_effect_blockers(status) WHERE status = 'unresolved';
