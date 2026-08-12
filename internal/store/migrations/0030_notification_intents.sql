-- Copyright 2026 OrgMentem. Licensed under MIT.
CREATE TABLE notification_intents(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  aggregate_key TEXT NOT NULL,
  phase TEXT NOT NULL,
  window_start TEXT NOT NULL,
  job_id TEXT,
  batch_id TEXT,
  scan_id TEXT,
  payload_json TEXT NOT NULL,
  first_at TEXT NOT NULL,
  last_at TEXT NOT NULL,
  count INTEGER NOT NULL,
  available_at TEXT NOT NULL,
  desktop_state TEXT NOT NULL,
  desktop_reserved_at TEXT,
  desktop_attempted_at TEXT,
  webhook_state TEXT NOT NULL,
  webhook_attempted_at TEXT
);
CREATE UNIQUE INDEX notification_intents_identity
  ON notification_intents(category, event_kind, aggregate_key, phase, window_start);
CREATE INDEX notification_intents_desktop_due
  ON notification_intents(desktop_state, available_at);
