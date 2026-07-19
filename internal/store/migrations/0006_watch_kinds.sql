-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

ALTER TABLE watches ADD COLUMN kind TEXT NOT NULL DEFAULT 'discovery';
ALTER TABLE watches ADD COLUMN mode TEXT NOT NULL DEFAULT 'acquire';

CREATE TABLE watch_digest_entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  work_key TEXT NOT NULL,
  title TEXT NOT NULL,
  authors TEXT NOT NULL DEFAULT '',
  year INTEGER NOT NULL DEFAULT 0,
  doi TEXT NOT NULL DEFAULT '',
  is_oa INTEGER NOT NULL DEFAULT 0,
  first_seen_at TEXT NOT NULL,
  UNIQUE(watch_id, work_key)
);
CREATE INDEX idx_watch_digest_watch ON watch_digest_entries(watch_id, id);
