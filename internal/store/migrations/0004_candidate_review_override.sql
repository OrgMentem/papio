-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

ALTER TABLE candidates ADD COLUMN review_override INTEGER NOT NULL DEFAULT 0;
