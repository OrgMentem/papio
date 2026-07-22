-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Applied-state ledger for the Zotero exception-tag reconciler: what papio
-- believes is currently written on each linked Zotero item ('' means no tag).
-- Desired state is always recomputed from jobs; this table only bounds the
-- reconciler's zotio invocations to genuine deltas.

CREATE TABLE zotio_tag_state (
  item_key   TEXT PRIMARY KEY,
  tag        TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
