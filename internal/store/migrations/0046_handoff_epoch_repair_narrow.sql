-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Remove 0045 reset markers outside the structurally identifiable population.
--
-- 0045 intentionally avoided guessing which historical acknowledgements were
-- queue waits. A job with no browser_candidates row has never had a browser
-- surface, so its open handoff action is the only population whose accepts
-- could not have been real driving. Keep those markers; undo 0045's markers
-- for jobs that had a surface or no longer have an open handoff action.

DELETE FROM events
WHERE kind = 'browser.handoff_epochs_reset'
  AND json_extract(detail_json, '$.migration') = '0045'
  AND (
    EXISTS (
      SELECT 1
      FROM browser_candidates c
      WHERE c.job_id = events.job_id
    )
    OR NOT EXISTS (
      SELECT 1
      FROM human_actions a
      WHERE a.job_id = events.job_id
        AND a.status = 'open'
        AND a.kind = 'openurl_handoff'
    )
  );
