-- Copyright 2026 OrgMentem. Licensed under MIT.
-- One daemon-private authority key. The key is never projected into events,
-- diagnostics, protocol payloads, or serialized configuration.
CREATE TABLE daemon_authority_key (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  hmac_key BLOB NOT NULL CHECK (length(hmac_key) = 32),
  created_at TEXT NOT NULL
);

-- Monotonic daemon holder generations fence claims across daemon restarts.
ALTER TABLE daemon_authority_key
  ADD COLUMN holder_generation INTEGER NOT NULL DEFAULT 0
  CHECK (holder_generation >= 0 AND holder_generation <= 9007199254740991);

-- Bindings retain the exact browser tab they were acknowledged against.
-- Tab zero is a valid Chrome tab id; negative values are the only sentinel.
ALTER TABLE materialization_claims
  ADD COLUMN tab_id INTEGER NOT NULL DEFAULT 0 CHECK (tab_id >= 0);
