-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Per-source rate-limit state, keyed by the account it was earned under.
--
-- A provider meters by credential, not by source name. Measured against
-- OpenAlex from one machine in the same second: an anonymous read reports
-- x-ratelimit-limit 1000 with 0 remaining, while the same source carrying an
-- API key reports 10000 with 8792 remaining. Two independent budgets sharing
-- one row means a Retry-After earned by one is served to the other.
--
-- That is not hypothetical. A 429 taken anonymously wrote a gate lasting until
-- UTC midnight; adding an API key opened a fresh 10000-credit budget, but the
-- single row still said closed, and 95 jobs parked against a quota that had
-- nothing to do with them until the row was cleared by hand.
--
-- Identity is a non-secret fingerprint of the credential ('anonymous', or
-- 'key-' plus a truncated SHA-256), never the credential itself, because this
-- column is read back in diagnostics. Existing rows are backfilled to 'legacy'
-- rather than 'anonymous': which credential earned them is unknowable, and
-- 'legacy' can never collide with a live identity, so a stale gate cannot gate
-- live traffic. SQLite cannot alter a primary key in place, hence the
-- recreation.

CREATE TABLE source_budgets_v2 (
  source             TEXT NOT NULL,
  identity           TEXT NOT NULL,
  window_start       TEXT,
  requests_in_window INTEGER NOT NULL DEFAULT 0,
  spent_usd          REAL NOT NULL DEFAULT 0,
  next_allowed_at    TEXT,
  PRIMARY KEY (source, identity)
);

INSERT INTO source_budgets_v2
  (source, identity, window_start, requests_in_window, spent_usd, next_allowed_at)
  SELECT source, 'legacy', window_start, requests_in_window, spent_usd, next_allowed_at
  FROM source_budgets;

DROP TABLE source_budgets;

ALTER TABLE source_budgets_v2 RENAME TO source_budgets;
