-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Per-job share of a source's daily credit allowance: one row per
-- (job, source, UTC day). Written in the same transaction as
-- source_credit_fuse.credits_committed, so a job's share is exactly as durable
-- as the source-wide debit it is part of.
--
-- credits_committed is MONOTONE for the life of a row and is never reset. That
-- is the whole design: a progress-triggered reset was the earlier proposal and
-- it could not deliver a bound, because a source dribbling low-value novelty
-- kept resetting the episode and the job lived forever. The only legitimate
-- reset is a human resubmitting the work, which is a new job row. Monotonicity
-- also makes the counter safe without lease fencing: a stale pass's increment
-- can only move the counter toward the bound, and the worst consequence is
-- slightly less availability for the job already identified as the hog.
--
-- A new UTC day is a new row, so nothing has to be swept: the share is a share
-- of TODAY's allowance, and yesterday's rows are history.

CREATE TABLE job_credit_share (
  job_id            TEXT NOT NULL,
  source            TEXT NOT NULL,
  utc_day           TEXT NOT NULL CHECK (length(utc_day) = 10),
  credits_committed INTEGER NOT NULL DEFAULT 0 CHECK (credits_committed >= 0),
  PRIMARY KEY (job_id, source, utc_day)
);

-- The gate reads (source, utc_day) for one job at a time, and operators read a
-- day's distribution across jobs; the primary key serves the first, this index
-- the second.
CREATE INDEX job_credit_share_day ON job_credit_share(source, utc_day);
