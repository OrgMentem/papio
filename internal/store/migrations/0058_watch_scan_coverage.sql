-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

-- Per-watch, per-source deep-scan position for discovery watches. A watch used
-- to read one page of results per run, so a mature query whose first page was
-- all owned or already-reported works stopped finding anything while every run
-- reported success. Each run still reads the first page, then walks a bounded
-- number of deeper pages; next_token is the opaque discovery continuation the
-- next run resumes from ('' restarts after the first page), and state is the
-- discovery page state the walk last stopped on. See internal/watch/store.go
-- (ScanCoverage, SaveScanCoverage).
CREATE TABLE watch_scan_coverage (
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  next_token TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (watch_id, source)
);
