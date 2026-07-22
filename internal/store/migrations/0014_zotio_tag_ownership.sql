-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Add personal-library provenance and crash-safe ownership state without
-- rewriting migration 0013: databases that already applied v13 must roll
-- forward. Existing ledger rows represent tags papio believed it had applied.

ALTER TABLE zotio_tag_state
  ADD COLUMN status TEXT NOT NULL DEFAULT 'owned'
  CHECK (status IN ('pending','owned','foreign','missing'));

CREATE TABLE zotio_item_scope (
  item_key    TEXT PRIMARY KEY,
  scope       TEXT NOT NULL CHECK (scope IN ('personal')),
  observed_at TEXT NOT NULL
);
