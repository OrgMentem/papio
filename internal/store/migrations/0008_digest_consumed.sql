-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

ALTER TABLE watch_digest_entries ADD COLUMN consumed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE watch_digest_entries ADD COLUMN authors_json TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_watch_digest_pending ON watch_digest_entries(watch_id, consumed, id);
