-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Adds 'parked' to materialization_claims.phase.
--
-- A parked claim still owns its bound browser tab, but it no longer occupies
-- its institution. A provider outcome that waits for an operator decision
-- after the sign-in has returned (terms_acceptance_required) moves the claim
-- here: the tab stays bound so the operator can finish the step on it, while
-- sibling papers at the same institution are no longer held behind a drive
-- that is waiting for a human. Measured live 2026-09-23: one JSTOR terms
-- modal held a `navigated` claim and the library's only sign-in slot for 14
-- minutes while six opened papers waited for the 30-minute stranded sweep.
--
-- 'parked' is deliberately NOT in the live-candidate index predicate: the
-- live set stays ('claimed','bound','route_issued','navigated').
--
-- SQLite cannot widen a CHECK constraint in place, so the table is rebuilt.
-- The column list is 0026's plus 0027's tab_id; both indexes are reproduced
-- from 0026. No PRAGMA foreign_keys: Store.migrate runs each migration inside a
-- transaction, where that pragma is a no-op. The rebuild is safe with
-- enforcement on because nothing references materialization_claims; its only
-- foreign key is outbound (candidate_id -> browser_candidates.id). Never edit
-- migrations 0026 or 0027: installations that applied them keep their
-- original constraint.

CREATE TABLE materialization_claims_rebuilt (
  id                       TEXT PRIMARY KEY,
  candidate_id             TEXT NOT NULL REFERENCES browser_candidates(id) ON DELETE CASCADE,
  browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  materialization_kind     TEXT NOT NULL CHECK (materialization_kind IN ('browser_tab','direct_download')),
  binding_id               TEXT NOT NULL UNIQUE CHECK (length(binding_id) BETWEEN 1 AND 256),
  phase                    TEXT NOT NULL CHECK (phase IN
    ('claimed','bound','route_issued','navigated','parked','settled','abandoned')),
  route_issuance_ordinal   INTEGER NOT NULL DEFAULT 0 CHECK (route_issuance_ordinal >= 0),
  effect_ordinal           INTEGER NOT NULL DEFAULT 0 CHECK (effect_ordinal >= 0),
  lease_until              TEXT,
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL,
  tab_id                   INTEGER NOT NULL DEFAULT 0 CHECK (tab_id >= 0)
);

INSERT INTO materialization_claims_rebuilt
  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
   phase, route_issuance_ordinal, effect_ordinal, lease_until, created_at,
   updated_at, tab_id)
  SELECT id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
         phase, route_issuance_ordinal, effect_ordinal, lease_until, created_at,
         updated_at, tab_id
  FROM materialization_claims;

DROP TABLE materialization_claims;
ALTER TABLE materialization_claims_rebuilt RENAME TO materialization_claims;

CREATE INDEX materialization_claims_by_candidate ON materialization_claims(candidate_id, updated_at DESC);
CREATE UNIQUE INDEX materialization_claims_live_candidate
  ON materialization_claims(candidate_id)
  WHERE phase IN ('claimed','bound','route_issued','navigated');
