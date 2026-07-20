-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

ALTER TABLE watch_digest_entries ADD COLUMN identifiers_json TEXT NOT NULL DEFAULT '';
