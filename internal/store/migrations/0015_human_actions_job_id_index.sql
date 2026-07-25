-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- The only existing index on human_actions is the partial
-- human_actions_open (status = 'open'), which cannot serve a per-job lookup.
-- triage.Stats' EXISTS(SELECT 1 FROM human_actions ha WHERE ha.job_id = j.id)
-- correlated subquery therefore ran a full table scan for every acquired
-- job, a cost that grows with the whole table's lifetime size and compounds
-- the stats query's context-deadline risk.

CREATE INDEX human_actions_by_job ON human_actions(job_id);
