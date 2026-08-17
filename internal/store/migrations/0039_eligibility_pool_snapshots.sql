-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Event-time eligibility pool snapshots for DOI-less grab settlement (ADR-0020
-- candidate-admission branch). One row per grab records which manual_download
-- jobs were on the table when the grab parked or bound, so backlog-replay
-- measurement can join historical settlements without re-enumerating live
-- eligibility.
--
-- PRIVACY / EXPOSURE (load-bearing):
--   snapshot JSON holds internal job_id values and bibliographic keys from the
--   operator's own acquisition backlog — more sensitive than aggregate counts,
--   less sensitive than PDF text. It deliberately omits tab URLs, signed CDN
--   parameters, zotio_item_key, quarantine paths, and excerpt text.
--   Default surfaces MUST NOT expose this payload:
--     - browser extension / pdf_grab_result wire: never (vocabulary unchanged)
--     - papio pulse / daemon info logs: never (pool_size + grab_id only if needed)
--     - papio doctor / --json health: never (schema version only)
--     - MCP resources: never unless a future explicit opt-in export tool is added
--   Permitted readers: identity-corpus / backlog replay via mode=ro joins only.
--   Measurement must use job.ListCandidateEligibleJobsTx on a read-only handle;
--   store.Open on an operator directory migrates as a side effect and is forbidden
--   for reporting.
CREATE TABLE pdf_grab_eligibility_snapshots (
  grab_id     TEXT PRIMARY KEY REFERENCES pdf_grabs(id) ON DELETE CASCADE,
  recorded_at TEXT NOT NULL,
  phase       TEXT NOT NULL CHECK (phase IN ('pre_bind','fenced_commit')),
  snapshot    TEXT NOT NULL  -- JSON, eligibility_pool_snapshot/1
);
CREATE INDEX pdf_grab_eligibility_snapshots_recorded
  ON pdf_grab_eligibility_snapshots(recorded_at);
