-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

CREATE TABLE watches (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  label                TEXT NOT NULL,
  query                TEXT NOT NULL,
  filters_json         TEXT NOT NULL,
  collection           TEXT NOT NULL DEFAULT '',
  cadence_hours        INTEGER NOT NULL CHECK (cadence_hours > 0),
  per_run_cap          INTEGER NOT NULL CHECK (per_run_cap BETWEEN 1 AND 50),
  enabled              INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  last_run_at          TEXT,
  created_at           TEXT NOT NULL,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  last_error           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX watches_due ON watches(enabled, last_run_at);
