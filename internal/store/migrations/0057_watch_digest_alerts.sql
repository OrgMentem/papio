-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

-- Durable alert receipts for watch digest entries. An alert-mode watch used to
-- lose its new-work alert when the process died (or the notifier failed)
-- between RecordDigest and the notification route: the next run deduplicates
-- the already-recorded works to reported=0 and never routes. The runner now
-- re-reads pending entries without a receipt, routes one catch-up alert, and
-- records receipts only after the route succeeds; see
-- internal/watch/store.go (UnalertedDigestEntries, MarkDigestAlerted).
CREATE TABLE watch_digest_alerts (
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  work_key TEXT NOT NULL,
  alerted_at TEXT NOT NULL,
  PRIMARY KEY (watch_id, work_key)
);
