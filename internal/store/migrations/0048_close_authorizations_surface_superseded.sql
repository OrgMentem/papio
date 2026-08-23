-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Adds surface_superseded to surface_close_v1's closed disposition vocabulary.
--
-- Every other disposition is a claim about the BINDING, and a binding can own
-- several tabs. So a duplicate scaffold could only be retired by asserting
-- scaffold_idle - which requires a claim phase of claimed/bound and therefore
-- fails structurally for any navigated claim - and the duplicates survived.
-- Measured live 2026-08-22: an operator looking at three tabs open on one
-- paper. surface_superseded is the surface-scoped fact instead: this binding
-- drives another tab, not this one. The daemon proves it rather than trusting
-- it, comparing the request's surface_tab_id against the claim's tab.
--
-- SQLite cannot widen a CHECK constraint in place, so preserve every token,
-- rebuild the table, and recreate both indexes. Never edit migrations 0041,
-- 0044, or 0047: installations which already applied them retain their
-- original CHECK forever.

CREATE TABLE close_authorizations_v4 (
  id                          TEXT PRIMARY KEY
    CHECK (length(id) BETWEEN 1 AND 128),
  binding_id                  TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation   INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  nonce                       TEXT NOT NULL CHECK (length(nonce) BETWEEN 1 AND 128),
  disposition                 TEXT NOT NULL CHECK (disposition IN
    ('scaffold_idle','materialization_settled','claim_abandoned','job_inactive',
     'handoff_parked','surface_superseded')),
  status                      TEXT NOT NULL CHECK (status IN
    ('issued','consumed','expired')),
  issued_at                   TEXT NOT NULL,
  consumed_at                 TEXT
);

INSERT INTO close_authorizations_v4
  (id, binding_id, browser_holder_generation, nonce, disposition, status,
   issued_at, consumed_at)
  SELECT id, binding_id, browser_holder_generation, nonce, disposition, status,
         issued_at, consumed_at
  FROM close_authorizations;

DROP TABLE close_authorizations;
ALTER TABLE close_authorizations_v4 RENAME TO close_authorizations;

CREATE UNIQUE INDEX close_authorizations_live_binding
  ON close_authorizations(binding_id)
  WHERE status = 'issued';

CREATE INDEX close_authorizations_by_status ON close_authorizations(status);
