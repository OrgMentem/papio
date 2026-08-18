-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Slice 3's claim-observation protocol
-- (dev/active/claim-observation-protocol.md §2.1/§2.2, §4.1/§4.2). Alters
-- the already-shipped authentication_entry_leases (migration 0029) rather
-- than duplicating it, and adds the idempotency/ordering journal §3
-- requires. Deliberately does not touch close_authorizations (migration
-- 0041, Slice 2b) — see that migration's own note on the split.

-- owner_binding_id/owner_tab_hint name the surface currently occupying an
-- authentication-entry lease, set when the owning candidate's
-- institutional_bind_response lands and cleared on lease reassignment or
-- owner_closed (§4.1).
ALTER TABLE authentication_entry_leases ADD COLUMN owner_binding_id TEXT;
ALTER TABLE authentication_entry_leases ADD COLUMN owner_tab_hint INTEGER
  CHECK (owner_tab_hint IS NULL OR owner_tab_hint >= 0);

-- Idempotency and ordering ledger for §3: observation_id is the replay key,
-- and the (gate_occurrence_id, event_ordinal) unique index enforces the
-- monotonic-apply rule at the schema level as a second line of defense
-- behind the transactional MAX() check.
CREATE TABLE claim_observation_journal (
  observation_id            TEXT PRIMARY KEY
    CHECK (length(observation_id) BETWEEN 1 AND 128),
  gate_occurrence_id        TEXT NOT NULL
    REFERENCES human_gate_observations(id),
  authentication_claim_id   TEXT NOT NULL
    CHECK (length(authentication_claim_id) BETWEEN 1 AND 256),
  binding_id                TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  event_kind                TEXT NOT NULL CHECK (event_kind IN
    ('wall_observed','login_started','mfa','challenge','auth_returned',
     'entitled_landing','owner_closed','navigation_error')),
  event_ordinal              INTEGER NOT NULL CHECK (event_ordinal >= 0),
  applied_at                 TEXT NOT NULL
);
CREATE UNIQUE INDEX claim_observation_journal_ordinal
  ON claim_observation_journal(gate_occurrence_id, event_ordinal);
CREATE INDEX claim_observation_journal_by_claim
  ON claim_observation_journal(authentication_claim_id, applied_at DESC);
