-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0019 Decision 10 follow-up: `canonical_unique` alone is not a yield —
-- a yield needs a denominator. A known page-class detector can count visible
-- result cards structurally (definition-list rows, repeated card
-- containers, reference-list items) without extracting their contents, so
-- `identifier_yield = canonical_unique / rendered_record_count_hint` is
-- honest where the hint exists, and honestly absent (NULL, never a guess)
-- on every page whose structure the detector does not recognize.
ALTER TABLE page_bulk_runs ADD COLUMN rendered_record_count_hint INTEGER NULL;
