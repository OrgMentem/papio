-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Source-wide daily OpenAlex credit fuse: one row per (source, UTC day).
-- credits_committed is crash-hard; denominator and prepaid baseline are
-- best-effort observations that may lag a busy disk.

CREATE TABLE source_credit_fuse (
  source              TEXT NOT NULL,
  utc_day             TEXT NOT NULL CHECK (length(utc_day) = 10),
  credits_committed   INTEGER NOT NULL DEFAULT 0 CHECK (credits_committed >= 0),
  denominator         INTEGER CHECK (denominator IS NULL OR denominator > 0),
  credits_used_seed   INTEGER CHECK (credits_used_seed IS NULL OR credits_used_seed >= 0),
  prepaid_baseline_usd REAL,
  drift_closed_at     TEXT,
  drift_reason        TEXT,
  PRIMARY KEY (source, utc_day)
);

CREATE INDEX source_credit_fuse_drift ON source_credit_fuse(source)
  WHERE drift_closed_at IS NOT NULL;
