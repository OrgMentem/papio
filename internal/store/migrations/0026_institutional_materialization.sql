-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0022 Phase 1: daemon-owned, dark institutional materialization state.
-- This migration contains no route, provider, or browser-location data.
-- Fresh route values are transient and never belong in these rows.

CREATE TABLE institution_profiles (
  id                       TEXT PRIMARY KEY,
  configured_name          TEXT NOT NULL CHECK (length(configured_name) BETWEEN 1 AND 256),
  revision                 INTEGER NOT NULL CHECK (revision >= 1),
  authority_digest         TEXT NOT NULL CHECK (length(authority_digest) BETWEEN 1 AND 256),
  authentication_claim_id  TEXT NOT NULL CHECK (length(authentication_claim_id) BETWEEN 1 AND 256),
  tombstoned_at            TEXT,
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL
);
CREATE UNIQUE INDEX institution_profiles_active_name
  ON institution_profiles(configured_name)
  WHERE tombstoned_at IS NULL;

CREATE TABLE browser_candidates (
  id                           TEXT PRIMARY KEY,
  job_id                       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  job_attempt_revision         INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
  institution_profile_id       TEXT NOT NULL REFERENCES institution_profiles(id),
  institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
  route_revision               INTEGER NOT NULL CHECK (route_revision >= 1),
  route_class                  TEXT NOT NULL CHECK (route_class IN ('institutional','delivery')),
  identifier_strategy          TEXT NOT NULL CHECK (identifier_strategy IN ('doi','pmid','arxiv','isbn','openalex','title')),
  pre_route_safety_key         TEXT NOT NULL CHECK (length(pre_route_safety_key) BETWEEN 1 AND 256),
  safety_domain_id             TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
  adapter_revision             TEXT NOT NULL CHECK (length(adapter_revision) BETWEEN 1 AND 256),
  effect_contract_id           TEXT NOT NULL CHECK (length(effect_contract_id) BETWEEN 1 AND 256),
  status                       TEXT NOT NULL CHECK (status IN ('eligible','claimed','materializing','succeeded','suppressed','abandoned')),
  created_at                   TEXT NOT NULL,
  updated_at                   TEXT NOT NULL
);
CREATE INDEX browser_candidates_by_job ON browser_candidates(job_id, status, id);
CREATE INDEX browser_candidates_by_profile
  ON browser_candidates(institution_profile_id, institution_profile_revision, status, id);

CREATE TABLE materialization_claims (
  id                       TEXT PRIMARY KEY,
  candidate_id             TEXT NOT NULL REFERENCES browser_candidates(id) ON DELETE CASCADE,
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  materialization_kind     TEXT NOT NULL CHECK (materialization_kind IN ('browser_tab','direct_download')),
  binding_id               TEXT NOT NULL UNIQUE CHECK (length(binding_id) BETWEEN 1 AND 256),
  phase                    TEXT NOT NULL CHECK (phase IN ('claimed','bound','route_issued','navigated','settled','abandoned')),
  route_issuance_ordinal   INTEGER NOT NULL DEFAULT 0 CHECK (route_issuance_ordinal >= 0),
  effect_ordinal           INTEGER NOT NULL DEFAULT 0 CHECK (effect_ordinal >= 0),
  lease_until              TEXT,
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL
);
CREATE INDEX materialization_claims_by_candidate ON materialization_claims(candidate_id, updated_at DESC);
CREATE UNIQUE INDEX materialization_claims_live_candidate
  ON materialization_claims(candidate_id)
  WHERE phase IN ('claimed','bound','route_issued','navigated');

CREATE TABLE profile_evidence (
  observation_id             TEXT PRIMARY KEY,
  browser_holder_generation  INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id) ON DELETE CASCADE,
  institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
  verdict                    TEXT NOT NULL CHECK (verdict IN ('unknown','inconclusive','signed_out','warm_verified','auth_returned')),
  source                     TEXT NOT NULL CHECK (source IN ('probe','auth_return','provider_outcome')),
  producer_observed_at      TEXT NOT NULL,
  daemon_received_at        TEXT NOT NULL,
  expires_at                 TEXT
);
CREATE INDEX profile_evidence_by_profile
  ON profile_evidence(institution_profile_id, institution_profile_revision, daemon_received_at DESC);
CREATE UNIQUE INDEX profile_evidence_producer_observation
  ON profile_evidence(institution_profile_id, institution_profile_revision, source, producer_observed_at);

CREATE TABLE human_gate_observations (
  id                       TEXT PRIMARY KEY,
  gate_type                TEXT NOT NULL CHECK (gate_type IN ('human_gate.login','human_gate.mfa','human_gate.captcha_or_security','human_gate.browser_host_permission','human_gate.downloads_folder_permission','human_gate.terms_required','human_gate.contractual_declaration','human_gate.identity_ambiguous')),
  scope_class              TEXT NOT NULL CHECK (scope_class IN ('authentication_claim','institution_profile','browser_host','platform','binding')),
  scope_key                TEXT NOT NULL CHECK (length(scope_key) BETWEEN 1 AND 256),
  institution_profile_id   TEXT REFERENCES institution_profiles(id),
  binding_id               TEXT,
  observation_revision     INTEGER NOT NULL CHECK (observation_revision >= 1),
  status                   TEXT NOT NULL CHECK (status IN ('open','resolved','cancelled')),
  detail_json              TEXT NOT NULL DEFAULT '{}',
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL,
  UNIQUE (gate_type, scope_class, scope_key)
);
CREATE INDEX human_gate_observations_by_status ON human_gate_observations(status, updated_at DESC);

CREATE TABLE route_suppressions (
  id                         TEXT PRIMARY KEY,
  job_id                     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  job_attempt_revision       INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
  institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id),
  institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
  route_revision             INTEGER NOT NULL CHECK (route_revision >= 1),
  safety_domain_id           TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
  adapter_revision           TEXT NOT NULL CHECK (length(adapter_revision) BETWEEN 1 AND 256),
  identifier_strategy       TEXT NOT NULL CHECK (identifier_strategy IN ('doi','pmid','arxiv','isbn','openalex','title')),
  evidence_observation_id   TEXT REFERENCES profile_evidence(observation_id),
  reason                    TEXT NOT NULL CHECK (reason IN ('no_entitlement','provider_challenge','rate_limited','adapter_drift')),
  active                    INTEGER NOT NULL CHECK (active IN (0,1)),
  created_at                TEXT NOT NULL,
  updated_at                TEXT NOT NULL
);
CREATE INDEX route_suppressions_by_job ON route_suppressions(job_id, updated_at DESC);
CREATE UNIQUE INDEX route_suppressions_active_exact
  ON route_suppressions(
    job_id,
    job_attempt_revision,
    institution_profile_id,
    institution_profile_revision,
    route_revision,
    safety_domain_id,
    adapter_revision,
    identifier_strategy
  ) WHERE active = 1;

CREATE TABLE artifact_winners (
  job_id        TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  job_attempt_revision INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
  candidate_id  TEXT NOT NULL REFERENCES browser_candidates(id),
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  sha256        TEXT NOT NULL CHECK (length(sha256) BETWEEN 1 AND 128),
  created_at    TEXT NOT NULL
);
CREATE INDEX artifact_winners_by_candidate ON artifact_winners(candidate_id);
