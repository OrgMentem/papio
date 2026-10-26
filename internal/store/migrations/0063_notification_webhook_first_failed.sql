-- Copyright 2026 OrgMentem. Licensed under MIT.
-- Record when a webhook leg first failed with a provable non-delivery, so the
-- retry window is measured from a webhook-owned time. available_at belongs to
-- the desktop leg too: quiet hours and reminder upserts move it. The column
-- stays NULL for a leg that never needed a retry.

ALTER TABLE notification_intents ADD COLUMN webhook_first_failed_at TEXT;
