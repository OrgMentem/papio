-- Durable per-job monetary spend. Source budgets protect a provider-wide
-- monthly ceiling; this counter independently enforces WorkRequest.max_cost_usd
-- across retries and daemon restarts.
ALTER TABLE jobs ADD COLUMN spent_usd REAL NOT NULL DEFAULT 0 CHECK (spent_usd >= 0);
