-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Durable review binding for verify_identity actions: the open action row
-- owns the exact candidate under review, the quarantined file it refers to,
-- and that file's SHA-256, so preview issuance and CAS acceptance read the
-- same fields instead of inferring via attempts or parsing free-text detail.
-- revision increments whenever the binding is refreshed; accept/reject carry
-- the expected revision (and SHA, for accept) and compare-and-set.
ALTER TABLE human_actions ADD COLUMN candidate_id INTEGER;
ALTER TABLE human_actions ADD COLUMN quarantine_path TEXT NOT NULL DEFAULT '';
ALTER TABLE human_actions ADD COLUMN quarantine_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE human_actions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
