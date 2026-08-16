-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Grab bind provenance: durable audit for automatic candidate binding.
-- pdf_grabs.bind_provenance records why an automatic binding was made
-- (method, rule version, winner, candidates considered, and evidence) so a
-- human reading the ledger later can reconstruct the decision. NULL means no
-- automatic binding decision has been recorded — every row predating this
-- column, and any grab not bound via candidate_auto_bind, is honestly
-- absent rather than guessed.
ALTER TABLE pdf_grabs ADD COLUMN bind_provenance TEXT;
