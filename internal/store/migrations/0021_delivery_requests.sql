-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0017 Decision 1: delivery_requests is a new durable table, owned by
-- internal/delivery, that turns "only obtainable through interlibrary loan"
-- from a dead end into a configured, durable route.
--
-- Idempotency key is institution profile + canonical work identity + provider
-- + request class, digested into idempotency_key. One work produces at most
-- one live subscription-provider request; a resubmission attempt for the same
-- key resolves against the existing row, never opens a second one — the same
-- one-durable-row-per-(scope,identity) shape ADR-0008 protects for holdings
-- claims and ADR-0014 Decision 1 protects for job attribution.
--
-- gate_profile_digest records which compiled Decision 3A gate profile
-- produced this request, so a later profile recompile never gets silently
-- misattributed to an older decision. next_check_at is scheduler-visible: a
-- pending request polls on its own provider-specific budget (Decision 1),
-- never on ordinary resolver or HTTP retry counts.
CREATE TABLE delivery_requests (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id              TEXT NOT NULL REFERENCES jobs(id),
  institution_profile TEXT NOT NULL,
  provider            TEXT NOT NULL,
  request_class       TEXT NOT NULL,
  work_identity       TEXT NOT NULL,
  idempotency_key     TEXT NOT NULL UNIQUE,
  state               TEXT NOT NULL
    CHECK (state IN ('offered','submitted','pending','fulfilled','declined','cancelled','unknown_outcome')),
  provider_reference  TEXT NOT NULL DEFAULT '',
  gate_profile_digest TEXT NOT NULL DEFAULT '',
  submitted_at        TEXT,
  last_checked_at     TEXT,
  next_check_at       TEXT,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX delivery_requests_by_job ON delivery_requests(job_id);
CREATE INDEX delivery_requests_by_state_next_check ON delivery_requests(state, next_check_at);

-- jobs gains a foreign reference to its live delivery request (ADR-0017
-- Decision 1). Nullable with no backfill: a job that predates this migration,
-- or one that never took the delivery route, has no delivery request to
-- point at.
ALTER TABLE jobs ADD COLUMN delivery_request_id INTEGER REFERENCES delivery_requests(id);
