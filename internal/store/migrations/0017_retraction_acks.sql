-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Acknowledged Crossref update notices.
--
-- A retraction notice is derived state: the sentinel recomputes it from
-- Crossref every day for as long as the work stays in the library, so nothing
-- the user does can make the notice go away. Without a record of "I have seen
-- this", every notice is permanent inbox clutter ranked above the actions that
-- still need work.
--
-- The acknowledgement is keyed on the notice identity rather than the work, so
-- a nature that escalates (concern -> retraction) or a newly issued notice DOI
-- is a different row and surfaces the work again. The daily sweep prunes rows
-- whose notice is no longer current, which also means a withdrawn-then-reissued
-- notice is shown afresh.

CREATE TABLE retraction_acks (
  doi        TEXT NOT NULL,
  nature     TEXT NOT NULL CHECK (nature IN ('retraction','correction','concern')),
  notice_doi TEXT NOT NULL DEFAULT '',
  acked_at   TEXT NOT NULL,
  PRIMARY KEY (doi, nature, notice_doi)
);
