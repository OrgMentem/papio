-- papio schema v1: durable acquisition core (stack plan "Durable state model").
-- No credentials, cookies, raw DOM, screenshots, or signed URL query values
-- are ever stored; candidate URLs are persisted redacted only.

CREATE TABLE work_requests (
  id                   TEXT PRIMARY KEY,
  created_at           TEXT NOT NULL,
  requester            TEXT NOT NULL DEFAULT 'cli',
  zotio_item_key       TEXT,
  collection_key       TEXT,
  title                TEXT,
  authors_json         TEXT,
  year                 INTEGER,
  desired_version      TEXT NOT NULL DEFAULT 'any',
  access_mode_override TEXT,
  max_cost_usd         REAL,
  sources_allow_json   TEXT,
  sources_deny_json    TEXT
);

CREATE TABLE identifiers (
  work_request_id TEXT NOT NULL REFERENCES work_requests(id) ON DELETE CASCADE,
  kind            TEXT NOT NULL CHECK (kind IN ('doi','pmid','arxiv','isbn','openalex')),
  value           TEXT NOT NULL,
  raw             TEXT NOT NULL,
  PRIMARY KEY (work_request_id, kind)
);
CREATE INDEX identifiers_by_value ON identifiers(kind, value);

CREATE TABLE jobs (
  id                    TEXT PRIMARY KEY,
  work_request_id       TEXT NOT NULL REFERENCES work_requests(id),
  state                 TEXT NOT NULL,
  policy_json           TEXT NOT NULL,
  lease_owner           TEXT,
  lease_expires_at      TEXT,
  selected_candidate_id INTEGER,
  artifact_sha256       TEXT,
  terminal_reason       TEXT,
  retry_at              TEXT,
  created_at            TEXT NOT NULL,
  updated_at            TEXT NOT NULL
);
CREATE INDEX jobs_by_state ON jobs(state);
-- One live job per work request; terminal jobs stay for history.
CREATE UNIQUE INDEX jobs_active_per_request ON jobs(work_request_id)
  WHERE state NOT IN ('ready','failed','cancelled','unavailable');

CREATE TABLE candidates (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id              TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  source              TEXT NOT NULL,
  url_redacted        TEXT NOT NULL,
  url_key             TEXT NOT NULL,
  landing_redacted    TEXT,
  version             TEXT NOT NULL,
  access_basis        TEXT NOT NULL,
  reuse_license       TEXT NOT NULL,
  expected_mime       TEXT,
  cost_usd            REAL NOT NULL DEFAULT 0,
  direct              INTEGER NOT NULL DEFAULT 0,
  identity_confidence REAL NOT NULL DEFAULT 0.5,
  rank_evidence       TEXT,
  rank                INTEGER,
  status              TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','fetching','invalid','retryable','accepted','skipped')),
  created_at          TEXT NOT NULL
);
CREATE INDEX candidates_by_job ON candidates(job_id, rank);
CREATE UNIQUE INDEX candidates_dedupe ON candidates(job_id, url_key);

CREATE TABLE attempts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id       TEXT NOT NULL,
  candidate_id INTEGER,
  stage        TEXT NOT NULL CHECK (stage IN ('resolve','fetch','validate')),
  source       TEXT,
  started_at   TEXT NOT NULL,
  ended_at     TEXT,
  outcome      TEXT,
  http_status  INTEGER,
  detail       TEXT
);
CREATE INDEX attempts_by_job ON attempts(job_id);

CREATE TABLE artifacts (
  sha256             TEXT PRIMARY KEY,
  size_bytes         INTEGER NOT NULL,
  mime               TEXT NOT NULL,
  page_count         INTEGER,
  text_chars         INTEGER,
  ocr_used           INTEGER NOT NULL DEFAULT 0,
  encrypted          INTEGER NOT NULL DEFAULT 0,
  has_active_content INTEGER NOT NULL DEFAULT 0,
  identity_result    TEXT,
  path               TEXT NOT NULL,
  created_at         TEXT NOT NULL
);

CREATE TABLE human_actions (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL,
  kind        TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','expired','cancelled')),
  detail      TEXT,
  created_at  TEXT NOT NULL,
  resolved_at TEXT,
  expires_at  TEXT
);
CREATE INDEX human_actions_open ON human_actions(status) WHERE status = 'open';

CREATE TABLE events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT,
  at          TEXT NOT NULL,
  kind        TEXT NOT NULL,
  detail_json TEXT NOT NULL
);
CREATE INDEX events_by_job ON events(job_id, seq);

CREATE TABLE exports (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id          TEXT NOT NULL,
  kind            TEXT NOT NULL CHECK (kind IN ('bundle','zotio_plan','zotio_apply')),
  idempotency_key TEXT NOT NULL UNIQUE,
  path            TEXT,
  result_json     TEXT,
  created_at      TEXT NOT NULL
);

CREATE TABLE source_budgets (
  source             TEXT PRIMARY KEY,
  window_start       TEXT,
  requests_in_window INTEGER NOT NULL DEFAULT 0,
  spent_usd          REAL NOT NULL DEFAULT 0,
  next_allowed_at    TEXT
);
