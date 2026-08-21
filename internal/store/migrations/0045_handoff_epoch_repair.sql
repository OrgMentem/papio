-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Repairs papers retired by the queued-accept accounting bug.
--
-- A `browser.job_accept` used to mean both "I am driving this" and "I have it
-- queued behind my one drive slot", and the daemon charged a fruitless drive
-- epoch either way -- quiescing a paper after three. Measured on a live store
-- 2026-08-21: 78 papers permanently retired on 438 accepted handoffs, of which
-- 77 papers never had a single `browser_candidates` row, because one stuck
-- sign-in held the extension's only drive slot and every sibling was queued,
-- acked, and dropped at QUEUED_HANDOFF_RELEASE_MS.
--
-- The verdict is DERIVED from event history, not stored, so fixing the rule
-- does not un-charge those accepts: a pre-fix ack carries no disposition and
-- still reads as a drive. Guessing retrospectively which historical acks were
-- really queue waits is exactly the heuristic that would resurrect the silent-
-- drive incident the fence exists for, so the repair states itself instead.
--
-- One `browser.handoff_epochs_reset` per affected job zeroes the streak from
-- here. It claims nothing about what any drive did. A paper that is genuinely
-- dead simply re-quiesces on its next three real drives; nothing is dismissed,
-- and `papio actions open` drove any of them on demand throughout.

INSERT INTO events (job_id, at, kind, detail_json)
SELECT DISTINCT
  a.job_id,
  strftime('%Y-%m-%dT%H:%M:%f000Z', 'now'),
  'browser.handoff_epochs_reset',
  json_object(
    'reason', 'queued_accepts_charged_as_drives',
    'migration', '0045'
  )
FROM human_actions a
WHERE a.status = 'open'
  AND EXISTS (
    SELECT 1 FROM events e
    WHERE e.job_id = a.job_id
      AND e.kind = 'browser.handoff_quiesced'
  );
