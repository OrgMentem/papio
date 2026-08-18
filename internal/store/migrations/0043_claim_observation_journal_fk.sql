-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Fixes a bug shipped in 0042: claim_observation_journal.gate_occurrence_id
-- REFERENCES human_gate_observations(id) with no ON UPDATE action, but
-- UpsertHumanGateObservation's occurrence rollover (institutional_evidence.go)
-- advances a login gate to a fresh occurrence by UPDATE-ing the existing
-- projection row's id column in place ("id = excluded.id" on conflict). With
-- foreign_keys(ON), the moment any journal row has recorded the old
-- occurrence id, that UPDATE is rejected by SQLite's default NO ACTION
-- foreign-key semantics — so the very next sign-out/regrant on that
-- authentication claim cannot open a new gate occurrence at all, and every
-- claim_observation the extension replays against the still-open old
-- occurrence keeps failing "gate occurrence state is unavailable".
--
-- The journal only ever needs gate_occurrence_id as an opaque replay/ordering
-- key (§3's (gate_occurrence_id, event_ordinal) uniqueness), never a joined
-- read against human_gate_observations, so the fix drops the foreign key
-- entirely rather than adding an unsupported "ON UPDATE CASCADE" (SQLite
-- cannot cascade into a column that is also that table's PRIMARY KEY target
-- of other, unrelated foreign keys) or complicating the rollover to preserve
-- old ids. SQLite cannot drop a column constraint in place, hence the
-- recreation (same idiom as 0018/0033/0038).

CREATE TABLE claim_observation_journal_v2 (
  observation_id            TEXT PRIMARY KEY
    CHECK (length(observation_id) BETWEEN 1 AND 128),
  gate_occurrence_id        TEXT NOT NULL,
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

INSERT INTO claim_observation_journal_v2
  (observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
   browser_holder_generation, event_kind, event_ordinal, applied_at)
  SELECT observation_id, gate_occurrence_id, authentication_claim_id, binding_id,
         browser_holder_generation, event_kind, event_ordinal, applied_at
  FROM claim_observation_journal;

DROP TABLE claim_observation_journal;
ALTER TABLE claim_observation_journal_v2 RENAME TO claim_observation_journal;

CREATE UNIQUE INDEX claim_observation_journal_ordinal
  ON claim_observation_journal(gate_occurrence_id, event_ordinal);
CREATE INDEX claim_observation_journal_by_claim
  ON claim_observation_journal(authentication_claim_id, applied_at DESC);

-- Slice 4 corrective: admitAutomaticMaterializationCandidates' landed-branch
-- admission ("lease is human, resume every eligible dependent") was gated on
-- authentication_entry_leases.state alone, which is already true the moment
-- auth_returned lands — before entitled_landing ever confirms the surface
-- reached real content rather than an IdP bounce or a wrong-work return.
-- entitled_at is the durable "entitled_landing was fenced-applied to this
-- exact human occupancy" signal that admission now requires in addition to
-- state='human'. It is cleared whenever the occupancy it was set for ends: a
-- fresh reservation cycle (ReserveAuthenticationEntryLease's reset branch)
-- or an owner_closed retirement (which also drops the lease back to
-- 'expired' so a later authentication_claim_request can grant a brand new
-- cycle instead of parking forever).
ALTER TABLE authentication_entry_leases ADD COLUMN entitled_at TEXT;
