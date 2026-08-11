-- Copyright 2026 OrgMentem. Licensed under MIT.
-- Durable, daemon-owned authentication-entry lease authority. Rows carry no
-- provider URLs, credentials, or identity-provider values.
CREATE TABLE authentication_entry_leases (
  authentication_claim_id TEXT PRIMARY KEY
    CHECK (length(authentication_claim_id) BETWEEN 1 AND 256),
  lease_id                TEXT NOT NULL
    CHECK (length(lease_id) BETWEEN 1 AND 128),
  owner_id                TEXT NOT NULL
    CHECK (length(owner_id) BETWEEN 1 AND 256),
  browser_holder_generation INTEGER NOT NULL
    CHECK (browser_holder_generation >= 0),
  state                   TEXT NOT NULL
    CHECK (state IN ('reserved','human','expired')),
  lease_until             TEXT,
  human_owner_id          TEXT
    CHECK (human_owner_id IS NULL OR length(human_owner_id) BETWEEN 1 AND 256),
  evidence_observation_id TEXT REFERENCES profile_evidence(observation_id),
  created_at              TEXT NOT NULL,
  updated_at              TEXT NOT NULL
);
CREATE INDEX authentication_entry_leases_by_expiry
  ON authentication_entry_leases(state, lease_until);

-- Observation IDs, not producer timestamps, are the idempotency keys. A
-- single logical sync may legitimately contain multiple observations with the
-- same producer timestamp and source.
DROP INDEX profile_evidence_producer_observation;
CREATE INDEX profile_evidence_producer_observation
  ON profile_evidence(institution_profile_id, institution_profile_revision,
                      source, producer_observed_at);
