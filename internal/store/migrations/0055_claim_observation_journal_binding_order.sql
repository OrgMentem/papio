-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- claim_observation ordering is per surface, not per login occurrence.
--
-- The extension numbers the observations of one surface (one binding) from 0.
-- It cannot know what other surfaces reported under the same occurrence, and
-- the daemon itself assigns owner_closed a position. But the ordering fence
-- compared each frame with the highest ordinal of the whole occurrence, and
-- the unique index below enforced that one sequence. One login occurrence
-- stays open across many papers, so every observation on a new surface was
-- acked `stale` until its ordinal passed the occurrence's maximum. Measured
-- live 2026-09-24: occurrence 180cae2d... reached ordinal 10, and each
-- auth_returned, wall_observed and navigation_error from the next two
-- surfaces was refused.
--
-- Uniqueness moves to (occurrence, binding, ordinal). Every existing row
-- satisfies the narrower rule because it satisfied the wider one.

DROP INDEX claim_observation_journal_ordinal;
CREATE UNIQUE INDEX claim_observation_journal_ordinal
  ON claim_observation_journal(gate_occurrence_id, binding_id, event_ordinal);
