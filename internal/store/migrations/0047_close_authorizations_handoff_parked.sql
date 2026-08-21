-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Adds handoff_parked to surface_close_v1's closed disposition vocabulary.
--
-- A paper waiting for the operator keeps its handoff action open, so
-- job_inactive is false for it and the daemon refused every close request its
-- surface ever made ("the binding still has an active browser handoff").
-- Measured live 2026-08-21: four such surfaces, refused on every reconcile
-- pass, outliving the papers' last human contact by days. handoff_parked says
-- the true thing instead: the ask is still open, and this browser is driving
-- nothing through the tab.
--
-- SQLite cannot widen a CHECK constraint in place, so preserve every token,
-- rebuild the table, and recreate both indexes. Never edit migrations 0041 or
-- 0044: installations which already applied them retain their original CHECK
-- forever.

CREATE TABLE close_authorizations_v3 (
  id                          TEXT PRIMARY KEY
    CHECK (length(id) BETWEEN 1 AND 128),
  binding_id                  TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation   INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  nonce                       TEXT NOT NULL CHECK (length(nonce) BETWEEN 1 AND 128),
  disposition                 TEXT NOT NULL CHECK (disposition IN
    ('scaffold_idle','materialization_settled','claim_abandoned','job_inactive',
     'handoff_parked')),
  status                      TEXT NOT NULL CHECK (status IN
    ('issued','consumed','expired')),
  issued_at                   TEXT NOT NULL,
  consumed_at                 TEXT
);

INSERT INTO close_authorizations_v3
  (id, binding_id, browser_holder_generation, nonce, disposition, status,
   issued_at, consumed_at)
  SELECT id, binding_id, browser_holder_generation, nonce, disposition, status,
         issued_at, consumed_at
  FROM close_authorizations;

DROP TABLE close_authorizations;
ALTER TABLE close_authorizations_v3 RENAME TO close_authorizations;

CREATE UNIQUE INDEX close_authorizations_live_binding
  ON close_authorizations(binding_id)
  WHERE status = 'issued';

CREATE INDEX close_authorizations_by_status ON close_authorizations(status);
