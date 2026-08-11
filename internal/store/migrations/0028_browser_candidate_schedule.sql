-- Copyright 2026 OrgMentem. Licensed under MIT.
-- Phase 3: indexed keyset traversal for explicit institutional candidates.
-- The partial index keeps ordinary candidate writes and non-scheduler reads small
-- while covering the stable traversal key used by the daemon scheduler.
CREATE INDEX browser_candidates_schedule_keyset
  ON browser_candidates(created_at, id)
  WHERE status = 'eligible';
