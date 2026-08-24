-- Copyright 2026 OrgMentem. Licensed under MIT.
-- Bound canonical page-acquire probes independently for live and terminal rows.

CREATE INDEX jobs_by_work_request_created_at
  ON jobs(work_request_id, created_at DESC);
