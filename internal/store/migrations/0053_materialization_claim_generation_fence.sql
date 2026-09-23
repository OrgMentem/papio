-- Record the highest browser holder generation retired by a reconciliation sweep.
-- NULL means no sweep has yet fenced a holder's claims.
ALTER TABLE daemon_authority_key
  ADD COLUMN fenced_through_generation INTEGER
  CHECK (fenced_through_generation IS NULL OR
         (fenced_through_generation >= 0 AND fenced_through_generation <= 9007199254740991));
