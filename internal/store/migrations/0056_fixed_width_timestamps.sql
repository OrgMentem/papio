-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- Rewrites every stored timestamp in the fixed-width form store.TimeLayout.
--
-- SQLite compares TEXT byte by byte, and papio compares timestamps as text:
-- lease and retry checks, [since, until) counts, keyset cursors, ORDER BY,
-- MIN and MAX. Every writer used time.RFC3339Nano, which trims trailing zeros
-- from the fraction, so a value was 20 to 27 bytes wide and text order was not
-- time order inside one second: '...:05Z' sorts after '...:05.1Z' because
-- 'Z' > '.', and '...:05.59561Z' sorts after the later '...:05.595612Z'.
-- Measured on a live store 2026-09-24: every one of 72,594 events.at values
-- was trimmed, 463 of the last 4,803 were not 27 bytes wide, and 18 seconds
-- held a pair of events whose text order was the reverse of their time order.
--
-- Writers now use store.FormatTime: UTC, exactly nine fractional digits,
-- always 30 bytes. A value written before this migration would still compare
-- wrongly against one written after it inside the same second -- a lease or
-- retry time written by the old binary, a [since, until) bound over history --
-- so the old values are rewritten here instead of being left to age out.
--
-- Part 1 touches only values in the old RFC 3339 UTC form: 'YYYY-MM-DDTHH:MM:SSZ'
-- or the same with a '.' and one to eight digits before the 'Z'. The digits
-- are padded with zeros to nine, which keeps the instant exactly. The rewrite
-- maps distinct instants to distinct text, so no unique index can collide.
-- Parsers accept both forms. source_budgets.window_start is a month key
-- ('2026-09') and source_credit_fuse.utc_day a day key, not instants, so they
-- are not listed.
--
-- Part 2 repairs the one other form found: grab.Service.Allocate bound a Go
-- time.Time directly, and the driver stored its String() form in local time,
-- '2026-08-13 14:23:49.227869 +0800 AWST m=+21436.124126501'. That text sorts
-- before every 'T' value of its date whatever its hour, and time.RFC3339Nano
-- cannot parse it. Its numeric offset gives the instant exactly. A value that
-- does not match that form, or does not convert, is left as it is.

-- Part 1: trimmed RFC 3339 UTC.

UPDATE acquisition_batch_chunks SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE acquisition_batch_members SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE acquisition_batches SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE acquisition_batches SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE acquisition_batches SET closed_at = substr(closed_at, 1, 19) || '.' || substr(substr(closed_at, 21, max(length(closed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE closed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(closed_at) = 20 OR (length(closed_at) BETWEEN 22 AND 29 AND substr(closed_at, 20, 1) = '.' AND substr(closed_at, 21, length(closed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE artifact_publications SET prepared_at = substr(prepared_at, 1, 19) || '.' || substr(substr(prepared_at, 21, max(length(prepared_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE prepared_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(prepared_at) = 20 OR (length(prepared_at) BETWEEN 22 AND 29 AND substr(prepared_at, 20, 1) = '.' AND substr(prepared_at, 21, length(prepared_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE artifact_winners SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE artifacts SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE attempts SET started_at = substr(started_at, 1, 19) || '.' || substr(substr(started_at, 21, max(length(started_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(started_at) = 20 OR (length(started_at) BETWEEN 22 AND 29 AND substr(started_at, 20, 1) = '.' AND substr(started_at, 21, length(started_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE attempts SET ended_at = substr(ended_at, 1, 19) || '.' || substr(substr(ended_at, 21, max(length(ended_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(ended_at) = 20 OR (length(ended_at) BETWEEN 22 AND 29 AND substr(ended_at, 20, 1) = '.' AND substr(ended_at, 21, length(ended_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE authentication_entry_leases SET lease_until = substr(lease_until, 1, 19) || '.' || substr(substr(lease_until, 21, max(length(lease_until) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(lease_until) = 20 OR (length(lease_until) BETWEEN 22 AND 29 AND substr(lease_until, 20, 1) = '.' AND substr(lease_until, 21, length(lease_until) - 21) NOT GLOB '*[^0-9]*'));

UPDATE authentication_entry_leases SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE authentication_entry_leases SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE authentication_entry_leases SET entitled_at = substr(entitled_at, 1, 19) || '.' || substr(substr(entitled_at, 21, max(length(entitled_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE entitled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(entitled_at) = 20 OR (length(entitled_at) BETWEEN 22 AND 29 AND substr(entitled_at, 20, 1) = '.' AND substr(entitled_at, 21, length(entitled_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE browser_candidates SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE browser_candidates SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE candidates SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE claim_observation_journal SET applied_at = substr(applied_at, 1, 19) || '.' || substr(substr(applied_at, 21, max(length(applied_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(applied_at) = 20 OR (length(applied_at) BETWEEN 22 AND 29 AND substr(applied_at, 20, 1) = '.' AND substr(applied_at, 21, length(applied_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE close_authorizations SET issued_at = substr(issued_at, 1, 19) || '.' || substr(substr(issued_at, 21, max(length(issued_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE issued_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(issued_at) = 20 OR (length(issued_at) BETWEEN 22 AND 29 AND substr(issued_at, 20, 1) = '.' AND substr(issued_at, 21, length(issued_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE close_authorizations SET consumed_at = substr(consumed_at, 1, 19) || '.' || substr(substr(consumed_at, 21, max(length(consumed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE consumed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(consumed_at) = 20 OR (length(consumed_at) BETWEEN 22 AND 29 AND substr(consumed_at, 20, 1) = '.' AND substr(consumed_at, 21, length(consumed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE daemon_authority_key SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET submitted_at = substr(submitted_at, 1, 19) || '.' || substr(substr(submitted_at, 21, max(length(submitted_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE submitted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(submitted_at) = 20 OR (length(submitted_at) BETWEEN 22 AND 29 AND substr(submitted_at, 20, 1) = '.' AND substr(submitted_at, 21, length(submitted_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET last_checked_at = substr(last_checked_at, 1, 19) || '.' || substr(substr(last_checked_at, 21, max(length(last_checked_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE last_checked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(last_checked_at) = 20 OR (length(last_checked_at) BETWEEN 22 AND 29 AND substr(last_checked_at, 20, 1) = '.' AND substr(last_checked_at, 21, length(last_checked_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET next_check_at = substr(next_check_at, 1, 19) || '.' || substr(substr(next_check_at, 21, max(length(next_check_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE next_check_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(next_check_at) = 20 OR (length(next_check_at) BETWEEN 22 AND 29 AND substr(next_check_at, 20, 1) = '.' AND substr(next_check_at, 21, length(next_check_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET last_poll_at = substr(last_poll_at, 1, 19) || '.' || substr(substr(last_poll_at, 21, max(length(last_poll_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE last_poll_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(last_poll_at) = 20 OR (length(last_poll_at) BETWEEN 22 AND 29 AND substr(last_poll_at, 20, 1) = '.' AND substr(last_poll_at, 21, length(last_poll_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE delivery_requests SET last_successful_poll_at = substr(last_successful_poll_at, 1, 19) || '.' || substr(substr(last_successful_poll_at, 21, max(length(last_successful_poll_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE last_successful_poll_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(last_successful_poll_at) = 20 OR (length(last_successful_poll_at) BETWEEN 22 AND 29 AND substr(last_successful_poll_at, 20, 1) = '.' AND substr(last_successful_poll_at, 21, length(last_successful_poll_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE effect_permits SET lease_until = substr(lease_until, 1, 19) || '.' || substr(substr(lease_until, 21, max(length(lease_until) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(lease_until) = 20 OR (length(lease_until) BETWEEN 22 AND 29 AND substr(lease_until, 20, 1) = '.' AND substr(lease_until, 21, length(lease_until) - 21) NOT GLOB '*[^0-9]*'));

UPDATE effect_permits SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE effect_permits SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE events SET at = substr(at, 1, 19) || '.' || substr(substr(at, 21, max(length(at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(at) = 20 OR (length(at) BETWEEN 22 AND 29 AND substr(at, 20, 1) = '.' AND substr(at, 21, length(at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE exports SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE human_actions SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE human_actions SET resolved_at = substr(resolved_at, 1, 19) || '.' || substr(substr(resolved_at, 21, max(length(resolved_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE resolved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(resolved_at) = 20 OR (length(resolved_at) BETWEEN 22 AND 29 AND substr(resolved_at, 20, 1) = '.' AND substr(resolved_at, 21, length(resolved_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE human_actions SET expires_at = substr(expires_at, 1, 19) || '.' || substr(substr(expires_at, 21, max(length(expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(expires_at) = 20 OR (length(expires_at) BETWEEN 22 AND 29 AND substr(expires_at, 20, 1) = '.' AND substr(expires_at, 21, length(expires_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE human_gate_observations SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE human_gate_observations SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE institution_profiles SET tombstoned_at = substr(tombstoned_at, 1, 19) || '.' || substr(substr(tombstoned_at, 21, max(length(tombstoned_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE tombstoned_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(tombstoned_at) = 20 OR (length(tombstoned_at) BETWEEN 22 AND 29 AND substr(tombstoned_at, 20, 1) = '.' AND substr(tombstoned_at, 21, length(tombstoned_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE institution_profiles SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE institution_profiles SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE job_artifacts SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE jobs SET lease_expires_at = substr(lease_expires_at, 1, 19) || '.' || substr(substr(lease_expires_at, 21, max(length(lease_expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE lease_expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(lease_expires_at) = 20 OR (length(lease_expires_at) BETWEEN 22 AND 29 AND substr(lease_expires_at, 20, 1) = '.' AND substr(lease_expires_at, 21, length(lease_expires_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE jobs SET retry_at = substr(retry_at, 1, 19) || '.' || substr(substr(retry_at, 21, max(length(retry_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE retry_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(retry_at) = 20 OR (length(retry_at) BETWEEN 22 AND 29 AND substr(retry_at, 20, 1) = '.' AND substr(retry_at, 21, length(retry_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE jobs SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE jobs SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE legacy_effect_blockers SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE legacy_effect_blockers SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE materialization_claims SET lease_until = substr(lease_until, 1, 19) || '.' || substr(substr(lease_until, 21, max(length(lease_until) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(lease_until) = 20 OR (length(lease_until) BETWEEN 22 AND 29 AND substr(lease_until, 20, 1) = '.' AND substr(lease_until, 21, length(lease_until) - 21) NOT GLOB '*[^0-9]*'));

UPDATE materialization_claims SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE materialization_claims SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET window_start = substr(window_start, 1, 19) || '.' || substr(substr(window_start, 21, max(length(window_start) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE window_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(window_start) = 20 OR (length(window_start) BETWEEN 22 AND 29 AND substr(window_start, 20, 1) = '.' AND substr(window_start, 21, length(window_start) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET first_at = substr(first_at, 1, 19) || '.' || substr(substr(first_at, 21, max(length(first_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE first_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(first_at) = 20 OR (length(first_at) BETWEEN 22 AND 29 AND substr(first_at, 20, 1) = '.' AND substr(first_at, 21, length(first_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET last_at = substr(last_at, 1, 19) || '.' || substr(substr(last_at, 21, max(length(last_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE last_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(last_at) = 20 OR (length(last_at) BETWEEN 22 AND 29 AND substr(last_at, 20, 1) = '.' AND substr(last_at, 21, length(last_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET available_at = substr(available_at, 1, 19) || '.' || substr(substr(available_at, 21, max(length(available_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE available_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(available_at) = 20 OR (length(available_at) BETWEEN 22 AND 29 AND substr(available_at, 20, 1) = '.' AND substr(available_at, 21, length(available_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET desktop_reserved_at = substr(desktop_reserved_at, 1, 19) || '.' || substr(substr(desktop_reserved_at, 21, max(length(desktop_reserved_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE desktop_reserved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(desktop_reserved_at) = 20 OR (length(desktop_reserved_at) BETWEEN 22 AND 29 AND substr(desktop_reserved_at, 20, 1) = '.' AND substr(desktop_reserved_at, 21, length(desktop_reserved_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET desktop_attempted_at = substr(desktop_attempted_at, 1, 19) || '.' || substr(substr(desktop_attempted_at, 21, max(length(desktop_attempted_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE desktop_attempted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(desktop_attempted_at) = 20 OR (length(desktop_attempted_at) BETWEEN 22 AND 29 AND substr(desktop_attempted_at, 20, 1) = '.' AND substr(desktop_attempted_at, 21, length(desktop_attempted_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE notification_intents SET webhook_attempted_at = substr(webhook_attempted_at, 1, 19) || '.' || substr(substr(webhook_attempted_at, 21, max(length(webhook_attempted_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE webhook_attempted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(webhook_attempted_at) = 20 OR (length(webhook_attempted_at) BETWEEN 22 AND 29 AND substr(webhook_attempted_at, 20, 1) = '.' AND substr(webhook_attempted_at, 21, length(webhook_attempted_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE page_bulk_runs SET opened_at = substr(opened_at, 1, 19) || '.' || substr(substr(opened_at, 21, max(length(opened_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE opened_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(opened_at) = 20 OR (length(opened_at) BETWEEN 22 AND 29 AND substr(opened_at, 20, 1) = '.' AND substr(opened_at, 21, length(opened_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE page_bulk_runs SET submitted_at = substr(submitted_at, 1, 19) || '.' || substr(substr(submitted_at, 21, max(length(submitted_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE submitted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(submitted_at) = 20 OR (length(submitted_at) BETWEEN 22 AND 29 AND substr(submitted_at, 20, 1) = '.' AND substr(submitted_at, 21, length(submitted_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE pdf_grab_eligibility_snapshots SET recorded_at = substr(recorded_at, 1, 19) || '.' || substr(substr(recorded_at, 21, max(length(recorded_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE recorded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(recorded_at) = 20 OR (length(recorded_at) BETWEEN 22 AND 29 AND substr(recorded_at, 20, 1) = '.' AND substr(recorded_at, 21, length(recorded_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE pdf_grabs SET notified_at = substr(notified_at, 1, 19) || '.' || substr(substr(notified_at, 21, max(length(notified_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE notified_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(notified_at) = 20 OR (length(notified_at) BETWEEN 22 AND 29 AND substr(notified_at, 20, 1) = '.' AND substr(notified_at, 21, length(notified_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE pdf_grabs SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE pdf_grabs SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE profile_evidence SET producer_observed_at = substr(producer_observed_at, 1, 19) || '.' || substr(substr(producer_observed_at, 21, max(length(producer_observed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE producer_observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(producer_observed_at) = 20 OR (length(producer_observed_at) BETWEEN 22 AND 29 AND substr(producer_observed_at, 20, 1) = '.' AND substr(producer_observed_at, 21, length(producer_observed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE profile_evidence SET daemon_received_at = substr(daemon_received_at, 1, 19) || '.' || substr(substr(daemon_received_at, 21, max(length(daemon_received_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE daemon_received_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(daemon_received_at) = 20 OR (length(daemon_received_at) BETWEEN 22 AND 29 AND substr(daemon_received_at, 20, 1) = '.' AND substr(daemon_received_at, 21, length(daemon_received_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE profile_evidence SET expires_at = substr(expires_at, 1, 19) || '.' || substr(substr(expires_at, 21, max(length(expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(expires_at) = 20 OR (length(expires_at) BETWEEN 22 AND 29 AND substr(expires_at, 20, 1) = '.' AND substr(expires_at, 21, length(expires_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE retraction_acks SET acked_at = substr(acked_at, 1, 19) || '.' || substr(substr(acked_at, 21, max(length(acked_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE acked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(acked_at) = 20 OR (length(acked_at) BETWEEN 22 AND 29 AND substr(acked_at, 20, 1) = '.' AND substr(acked_at, 21, length(acked_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE route_suppressions SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE route_suppressions SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE source_budgets SET next_allowed_at = substr(next_allowed_at, 1, 19) || '.' || substr(substr(next_allowed_at, 21, max(length(next_allowed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE next_allowed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(next_allowed_at) = 20 OR (length(next_allowed_at) BETWEEN 22 AND 29 AND substr(next_allowed_at, 20, 1) = '.' AND substr(next_allowed_at, 21, length(next_allowed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE source_credit_fuse SET drift_closed_at = substr(drift_closed_at, 1, 19) || '.' || substr(substr(drift_closed_at, 21, max(length(drift_closed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE drift_closed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(drift_closed_at) = 20 OR (length(drift_closed_at) BETWEEN 22 AND 29 AND substr(drift_closed_at, 20, 1) = '.' AND substr(drift_closed_at, 21, length(drift_closed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE validation_reports SET recorded_at = substr(recorded_at, 1, 19) || '.' || substr(substr(recorded_at, 21, max(length(recorded_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE recorded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(recorded_at) = 20 OR (length(recorded_at) BETWEEN 22 AND 29 AND substr(recorded_at, 20, 1) = '.' AND substr(recorded_at, 21, length(recorded_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE watch_digest_entries SET first_seen_at = substr(first_seen_at, 1, 19) || '.' || substr(substr(first_seen_at, 21, max(length(first_seen_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE first_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(first_seen_at) = 20 OR (length(first_seen_at) BETWEEN 22 AND 29 AND substr(first_seen_at, 20, 1) = '.' AND substr(first_seen_at, 21, length(first_seen_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE watches SET last_run_at = substr(last_run_at, 1, 19) || '.' || substr(substr(last_run_at, 21, max(length(last_run_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE last_run_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(last_run_at) = 20 OR (length(last_run_at) BETWEEN 22 AND 29 AND substr(last_run_at, 20, 1) = '.' AND substr(last_run_at, 21, length(last_run_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE watches SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE work_requests SET created_at = substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(created_at) = 20 OR (length(created_at) BETWEEN 22 AND 29 AND substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE zotio_item_scope SET observed_at = substr(observed_at, 1, 19) || '.' || substr(substr(observed_at, 21, max(length(observed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(observed_at) = 20 OR (length(observed_at) BETWEEN 22 AND 29 AND substr(observed_at, 20, 1) = '.' AND substr(observed_at, 21, length(observed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE zotio_tag_state SET updated_at = substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
   AND (length(updated_at) = 20 OR (length(updated_at) BETWEEN 22 AND 29 AND substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

-- Part 2: Go time.Time String() form in pdf_grabs, written by Allocate.

UPDATE pdf_grabs SET created_at = fixed.value
  FROM (
    SELECT id, strftime('%Y-%m-%dT%H:%M:%S', substr(v, 1, 19) || substr(tail, 1, 3) || ':' || substr(tail, 4, 2))
             || '.' || substr(frac || '000000000', 1, 9) || 'Z' AS value
      FROM (
        SELECT id, created_at AS v,
               CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, instr(substr(created_at, 20), ' ') - 2) ELSE '' END AS frac,
               CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 20 + instr(substr(created_at, 20), ' ')) ELSE substr(created_at, 21) END AS tail
          FROM pdf_grabs
         WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9][. ]*'
      )
     WHERE length(frac) <= 9 AND frac NOT GLOB '*[^0-9]*'
       AND tail GLOB '[+-][0-9][0-9][0-9][0-9]*'
       AND strftime('%Y-%m-%dT%H:%M:%S', substr(v, 1, 19) || substr(tail, 1, 3) || ':' || substr(tail, 4, 2)) IS NOT NULL
  ) AS fixed
 WHERE pdf_grabs.id = fixed.id;

UPDATE pdf_grabs SET updated_at = fixed.value
  FROM (
    SELECT id, strftime('%Y-%m-%dT%H:%M:%S', substr(v, 1, 19) || substr(tail, 1, 3) || ':' || substr(tail, 4, 2))
             || '.' || substr(frac || '000000000', 1, 9) || 'Z' AS value
      FROM (
        SELECT id, updated_at AS v,
               CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, instr(substr(updated_at, 20), ' ') - 2) ELSE '' END AS frac,
               CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 20 + instr(substr(updated_at, 20), ' ')) ELSE substr(updated_at, 21) END AS tail
          FROM pdf_grabs
         WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9][. ]*'
      )
     WHERE length(frac) <= 9 AND frac NOT GLOB '*[^0-9]*'
       AND tail GLOB '[+-][0-9][0-9][0-9][0-9]*'
       AND strftime('%Y-%m-%dT%H:%M:%S', substr(v, 1, 19) || substr(tail, 1, 3) || ':' || substr(tail, 4, 2)) IS NOT NULL
  ) AS fixed
 WHERE pdf_grabs.id = fixed.id;
