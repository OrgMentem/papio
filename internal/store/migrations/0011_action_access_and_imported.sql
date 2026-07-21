-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Structured access classification on human actions, and the new terminal
-- job state 'imported' excluded from the one-live-job-per-request invariant.

ALTER TABLE human_actions ADD COLUMN requires_auth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE human_actions ADD COLUMN blocked_by TEXT NOT NULL DEFAULT '';

-- Backfill existing parked handoffs from the legacy detail markers so no
-- runtime prefix-sniff fallback is needed anywhere.
UPDATE human_actions SET requires_auth = 0, blocked_by = 'anti_bot'
 WHERE kind = 'openurl_handoff' AND detail LIKE 'open-access fetch via browser' || char(10) || '%';
UPDATE human_actions SET requires_auth = 1, blocked_by = 'paywall'
 WHERE kind = 'openurl_handoff' AND blocked_by = '';
UPDATE human_actions SET blocked_by = 'landing_page'
 WHERE kind = 'manual_download' AND detail = 'a resolver returned a landing page but no verified direct PDF';

-- Recreate the one-live-job partial index (SQLite cannot alter a predicate) so
-- 'imported' is terminal like ready/failed/cancelled/unavailable.
DROP INDEX jobs_active_per_request;
CREATE UNIQUE INDEX jobs_active_per_request ON jobs(work_request_id)
  WHERE state NOT IN ('ready','failed','cancelled','unavailable','imported');
