-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- One-use close-authorization tokens for surface_close_v1
-- (dev/active/claim-observation-protocol.md §2.3/§4.3). Deliberately its own
-- table, not folded into materialization_claims, because a close
-- authorization can be issued for a scaffold that never had a live
-- materialization claim row at all (an idle, never-engaged scaffold that
-- timed out before any claim advanced past "claimed").
--
-- This migration deliberately carries ONLY this table. The design doc's §4
-- sketch bundled it with the authentication_entry_leases ALTER and the new
-- claim_observation_journal table in one migration; that bundling is
-- Slice 3 work (the authentication-claim arbitration protocol) and is
-- deferred to Slice 3's own migration so Slice 2b's close transaction can
-- ship under surface_close_v1 alone, with no dependency on Slice 3 landing
-- first.

CREATE TABLE close_authorizations (
  id                          TEXT PRIMARY KEY
    CHECK (length(id) BETWEEN 1 AND 128),
  binding_id                  TEXT NOT NULL
    CHECK (length(binding_id) BETWEEN 1 AND 256),
  browser_holder_generation   INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
  nonce                       TEXT NOT NULL CHECK (length(nonce) BETWEEN 1 AND 128),
  disposition                 TEXT NOT NULL CHECK (disposition IN
    ('scaffold_idle','materialization_settled','claim_abandoned')),
  status                      TEXT NOT NULL CHECK (status IN
    ('issued','consumed','expired')),
  issued_at                   TEXT NOT NULL,
  consumed_at                 TEXT
);

-- At most one live (status='issued') authorization per binding, the same
-- idiom materialization_claims_live_candidate (migration 0026) uses for "at
-- most one live claim per candidate". This is what makes token issuance
-- idempotent per binding: a repeated authorized request for the same live
-- binding finds and returns this same row instead of racing a second one
-- into existence.
CREATE UNIQUE INDEX close_authorizations_live_binding
  ON close_authorizations(binding_id)
  WHERE status = 'issued';

CREATE INDEX close_authorizations_by_status ON close_authorizations(status);
