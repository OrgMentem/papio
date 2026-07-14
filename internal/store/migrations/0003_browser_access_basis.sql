-- Browser-adopted files are institutionally accessed, matching the locked
-- acquisition-bundle access_basis vocabulary.
UPDATE candidates
SET access_basis = 'institutional'
WHERE source = 'browser' AND access_basis = 'subscription';
