-- Copyright 2026 OrgMentem. Licensed under MIT.
-- Record what the desktop leg delivered so a still-pending webhook digest can
-- keep coalescing the shared row without rewriting the desktop audit trail.
-- Both columns stay NULL for a desktop leg that never sent anything.

ALTER TABLE notification_intents ADD COLUMN desktop_sent_count INTEGER;
ALTER TABLE notification_intents ADD COLUMN desktop_sent_payload_json TEXT;
