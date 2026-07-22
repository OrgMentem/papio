-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Rename the delegated browser-download policy from its former access-mode label.

UPDATE jobs SET policy_json = replace(policy_json, '"access_mode":"maximal"', '"access_mode":"delegated"') WHERE policy_json LIKE '%"access_mode":"maximal"%';
UPDATE work_requests SET access_mode_override = 'delegated' WHERE access_mode_override = 'maximal';
