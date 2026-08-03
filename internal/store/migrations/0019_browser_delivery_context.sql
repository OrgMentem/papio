-- Browser delivery provenance is nullable: legacy and manually adopted files
-- have no observed route/session evidence and remain conservative.
ALTER TABLE candidates ADD COLUMN browser_route TEXT
  CHECK (browser_route IS NULL OR browser_route IN ('resolver', 'direct', 'oa'));
ALTER TABLE candidates ADD COLUMN session_evidence TEXT
  CHECK (session_evidence IS NULL OR session_evidence IN ('fresh_auth', 'warm', 'none'));

-- Older browser rows were unconditionally labeled institutional. Without a
-- durable route/session observation that claim is invented, so normalize it.
UPDATE candidates
SET access_basis = 'manual'
WHERE source = 'browser'
  AND access_basis = 'institutional'
  AND browser_route IS NULL
  AND session_evidence IS NULL;
