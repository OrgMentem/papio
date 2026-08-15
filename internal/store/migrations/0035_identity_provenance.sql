-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Identity provenance: durable anchor for submitted canonical work.
-- identifiers.provenance distinguishes submitted vs adopted vs unattested;
-- work_requests.submitted_fields records which bibliographic fields the
-- requester actually supplied (NULL = legacy/unattested).
ALTER TABLE identifiers ADD COLUMN provenance TEXT NOT NULL DEFAULT 'unattested' CHECK (provenance IN ('unattested','submitted','verified','adopted'));
ALTER TABLE work_requests ADD COLUMN submitted_fields TEXT;
