-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Abstracts captured at digest time let the triage inbox present enough
-- context to decide acquire/dismiss without opening the landing page.
ALTER TABLE watch_digest_entries ADD COLUMN abstract TEXT NOT NULL DEFAULT '';
