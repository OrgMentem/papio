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
-- Part 1 rewrites each RFC 3339 value, in UTC ('Z') or with a numeric offset
-- ('+08:00', '-05:30'), to UTC with nine fractional digits:
--   * The date and clock must be real: month 01-12, a day that exists in that
--     month, hour 00-23, minute and second 00-59. strftime() normalizes an
--     impossible date ('2026-02-30' becomes '2026-03-02'), so a value whose
--     date and clock do not survive strftime() unchanged is not a timestamp.
--   * An offset moves the clock by whole minutes, so strftime() converts the
--     seconds part to UTC (across a day, month or year) and the fraction is
--     kept as it is. SQLite reads offsets up to +/-14:00, the range of every
--     real zone; a larger one is left as it is.
--   * The fraction is padded with zeros to nine digits, or cut at nine, as
--     Go's parser reads it, so the instant stays exact to the nanosecond.
-- Any other text -- a month key such as source_budgets.window_start
-- ('2026-09'), '2026-13-99T88:77:66Z', a fraction with no digits -- is left
-- exactly as it is: guessing an instant is not this migration's business.
-- A value already in the fixed form is not touched. Distinct instants map to
-- distinct text, so no unique index can collide. Parsers accept both forms.
--
-- Part 2 repairs the one other form found: grab.Service.Allocate bound a Go
-- time.Time directly, and the driver stored its String() form in local time,
-- '2026-08-13 14:23:49.227869 +0800 AWST m=+21436.124126501'. That text sorts
-- before every 'T' value of its date whatever its hour, and time.RFC3339Nano
-- cannot parse it. Its numeric offset gives the instant exactly. A value that
-- does not match that form, or does not convert, is left as it is.

-- Part 1: RFC 3339 in UTC or with a numeric offset.

UPDATE acquisition_batch_chunks SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE acquisition_batch_members SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE acquisition_batches SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE acquisition_batches SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE acquisition_batches SET closed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(closed_at, 1, 19) || CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END)
       || '.' || substr(substr(substr(closed_at, 20, length(closed_at) - 19 - length(CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE closed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (closed_at GLOB '*Z' OR closed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(closed_at) = 30 AND closed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(closed_at, 6, 2) BETWEEN '01' AND '12' AND substr(closed_at, 12, 2) <= '23'
   AND substr(closed_at, 15, 2) <= '59' AND substr(closed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(closed_at, 1, 19)) = substr(closed_at, 1, 19)
   AND (substr(closed_at, 20, length(closed_at) - 19 - length(CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END)) = '' OR (substr(closed_at, 20, length(closed_at) - 19 - length(CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(closed_at, 20, length(closed_at) - 19 - length(CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(closed_at, 1, 19) || CASE WHEN substr(closed_at, -1) = 'Z' THEN 'Z' ELSE substr(closed_at, -6) END) IS NOT NULL;
UPDATE artifact_publications SET prepared_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(prepared_at, 1, 19) || CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END)
       || '.' || substr(substr(substr(prepared_at, 20, length(prepared_at) - 19 - length(CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE prepared_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (prepared_at GLOB '*Z' OR prepared_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(prepared_at) = 30 AND prepared_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(prepared_at, 6, 2) BETWEEN '01' AND '12' AND substr(prepared_at, 12, 2) <= '23'
   AND substr(prepared_at, 15, 2) <= '59' AND substr(prepared_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(prepared_at, 1, 19)) = substr(prepared_at, 1, 19)
   AND (substr(prepared_at, 20, length(prepared_at) - 19 - length(CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END)) = '' OR (substr(prepared_at, 20, length(prepared_at) - 19 - length(CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(prepared_at, 20, length(prepared_at) - 19 - length(CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(prepared_at, 1, 19) || CASE WHEN substr(prepared_at, -1) = 'Z' THEN 'Z' ELSE substr(prepared_at, -6) END) IS NOT NULL;
UPDATE artifact_winners SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE artifacts SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE attempts SET started_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(started_at, 1, 19) || CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END)
       || '.' || substr(substr(substr(started_at, 20, length(started_at) - 19 - length(CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (started_at GLOB '*Z' OR started_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(started_at) = 30 AND started_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(started_at, 6, 2) BETWEEN '01' AND '12' AND substr(started_at, 12, 2) <= '23'
   AND substr(started_at, 15, 2) <= '59' AND substr(started_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(started_at, 1, 19)) = substr(started_at, 1, 19)
   AND (substr(started_at, 20, length(started_at) - 19 - length(CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END)) = '' OR (substr(started_at, 20, length(started_at) - 19 - length(CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(started_at, 20, length(started_at) - 19 - length(CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(started_at, 1, 19) || CASE WHEN substr(started_at, -1) = 'Z' THEN 'Z' ELSE substr(started_at, -6) END) IS NOT NULL;
UPDATE attempts SET ended_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(ended_at, 1, 19) || CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END)
       || '.' || substr(substr(substr(ended_at, 20, length(ended_at) - 19 - length(CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (ended_at GLOB '*Z' OR ended_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(ended_at) = 30 AND ended_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(ended_at, 6, 2) BETWEEN '01' AND '12' AND substr(ended_at, 12, 2) <= '23'
   AND substr(ended_at, 15, 2) <= '59' AND substr(ended_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(ended_at, 1, 19)) = substr(ended_at, 1, 19)
   AND (substr(ended_at, 20, length(ended_at) - 19 - length(CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END)) = '' OR (substr(ended_at, 20, length(ended_at) - 19 - length(CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(ended_at, 20, length(ended_at) - 19 - length(CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(ended_at, 1, 19) || CASE WHEN substr(ended_at, -1) = 'Z' THEN 'Z' ELSE substr(ended_at, -6) END) IS NOT NULL;
UPDATE authentication_entry_leases SET lease_until =
       strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)
       || '.' || substr(substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (lease_until GLOB '*Z' OR lease_until GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(lease_until) = 30 AND lease_until GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(lease_until, 6, 2) BETWEEN '01' AND '12' AND substr(lease_until, 12, 2) <= '23'
   AND substr(lease_until, 15, 2) <= '59' AND substr(lease_until, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19)) = substr(lease_until, 1, 19)
   AND (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) = '' OR (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) GLOB '.[0-9]*' AND substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END) IS NOT NULL;
UPDATE authentication_entry_leases SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE authentication_entry_leases SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE authentication_entry_leases SET entitled_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(entitled_at, 1, 19) || CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END)
       || '.' || substr(substr(substr(entitled_at, 20, length(entitled_at) - 19 - length(CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE entitled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (entitled_at GLOB '*Z' OR entitled_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(entitled_at) = 30 AND entitled_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(entitled_at, 6, 2) BETWEEN '01' AND '12' AND substr(entitled_at, 12, 2) <= '23'
   AND substr(entitled_at, 15, 2) <= '59' AND substr(entitled_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(entitled_at, 1, 19)) = substr(entitled_at, 1, 19)
   AND (substr(entitled_at, 20, length(entitled_at) - 19 - length(CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END)) = '' OR (substr(entitled_at, 20, length(entitled_at) - 19 - length(CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(entitled_at, 20, length(entitled_at) - 19 - length(CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(entitled_at, 1, 19) || CASE WHEN substr(entitled_at, -1) = 'Z' THEN 'Z' ELSE substr(entitled_at, -6) END) IS NOT NULL;
UPDATE browser_candidates SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE browser_candidates SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE candidates SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE claim_observation_journal SET applied_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(applied_at, 1, 19) || CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END)
       || '.' || substr(substr(substr(applied_at, 20, length(applied_at) - 19 - length(CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (applied_at GLOB '*Z' OR applied_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(applied_at) = 30 AND applied_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(applied_at, 6, 2) BETWEEN '01' AND '12' AND substr(applied_at, 12, 2) <= '23'
   AND substr(applied_at, 15, 2) <= '59' AND substr(applied_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(applied_at, 1, 19)) = substr(applied_at, 1, 19)
   AND (substr(applied_at, 20, length(applied_at) - 19 - length(CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END)) = '' OR (substr(applied_at, 20, length(applied_at) - 19 - length(CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(applied_at, 20, length(applied_at) - 19 - length(CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(applied_at, 1, 19) || CASE WHEN substr(applied_at, -1) = 'Z' THEN 'Z' ELSE substr(applied_at, -6) END) IS NOT NULL;
UPDATE close_authorizations SET issued_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(issued_at, 1, 19) || CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END)
       || '.' || substr(substr(substr(issued_at, 20, length(issued_at) - 19 - length(CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE issued_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (issued_at GLOB '*Z' OR issued_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(issued_at) = 30 AND issued_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(issued_at, 6, 2) BETWEEN '01' AND '12' AND substr(issued_at, 12, 2) <= '23'
   AND substr(issued_at, 15, 2) <= '59' AND substr(issued_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(issued_at, 1, 19)) = substr(issued_at, 1, 19)
   AND (substr(issued_at, 20, length(issued_at) - 19 - length(CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END)) = '' OR (substr(issued_at, 20, length(issued_at) - 19 - length(CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(issued_at, 20, length(issued_at) - 19 - length(CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(issued_at, 1, 19) || CASE WHEN substr(issued_at, -1) = 'Z' THEN 'Z' ELSE substr(issued_at, -6) END) IS NOT NULL;
UPDATE close_authorizations SET consumed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(consumed_at, 1, 19) || CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END)
       || '.' || substr(substr(substr(consumed_at, 20, length(consumed_at) - 19 - length(CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE consumed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (consumed_at GLOB '*Z' OR consumed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(consumed_at) = 30 AND consumed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(consumed_at, 6, 2) BETWEEN '01' AND '12' AND substr(consumed_at, 12, 2) <= '23'
   AND substr(consumed_at, 15, 2) <= '59' AND substr(consumed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(consumed_at, 1, 19)) = substr(consumed_at, 1, 19)
   AND (substr(consumed_at, 20, length(consumed_at) - 19 - length(CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END)) = '' OR (substr(consumed_at, 20, length(consumed_at) - 19 - length(CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(consumed_at, 20, length(consumed_at) - 19 - length(CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(consumed_at, 1, 19) || CASE WHEN substr(consumed_at, -1) = 'Z' THEN 'Z' ELSE substr(consumed_at, -6) END) IS NOT NULL;
UPDATE daemon_authority_key SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET submitted_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19) || CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)
       || '.' || substr(substr(substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE submitted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (submitted_at GLOB '*Z' OR submitted_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(submitted_at) = 30 AND submitted_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(submitted_at, 6, 2) BETWEEN '01' AND '12' AND substr(submitted_at, 12, 2) <= '23'
   AND substr(submitted_at, 15, 2) <= '59' AND substr(submitted_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19)) = substr(submitted_at, 1, 19)
   AND (substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)) = '' OR (substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19) || CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET last_checked_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(last_checked_at, 1, 19) || CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END)
       || '.' || substr(substr(substr(last_checked_at, 20, length(last_checked_at) - 19 - length(CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE last_checked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (last_checked_at GLOB '*Z' OR last_checked_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(last_checked_at) = 30 AND last_checked_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(last_checked_at, 6, 2) BETWEEN '01' AND '12' AND substr(last_checked_at, 12, 2) <= '23'
   AND substr(last_checked_at, 15, 2) <= '59' AND substr(last_checked_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_checked_at, 1, 19)) = substr(last_checked_at, 1, 19)
   AND (substr(last_checked_at, 20, length(last_checked_at) - 19 - length(CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END)) = '' OR (substr(last_checked_at, 20, length(last_checked_at) - 19 - length(CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(last_checked_at, 20, length(last_checked_at) - 19 - length(CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_checked_at, 1, 19) || CASE WHEN substr(last_checked_at, -1) = 'Z' THEN 'Z' ELSE substr(last_checked_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET next_check_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(next_check_at, 1, 19) || CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END)
       || '.' || substr(substr(substr(next_check_at, 20, length(next_check_at) - 19 - length(CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE next_check_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (next_check_at GLOB '*Z' OR next_check_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(next_check_at) = 30 AND next_check_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(next_check_at, 6, 2) BETWEEN '01' AND '12' AND substr(next_check_at, 12, 2) <= '23'
   AND substr(next_check_at, 15, 2) <= '59' AND substr(next_check_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(next_check_at, 1, 19)) = substr(next_check_at, 1, 19)
   AND (substr(next_check_at, 20, length(next_check_at) - 19 - length(CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END)) = '' OR (substr(next_check_at, 20, length(next_check_at) - 19 - length(CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(next_check_at, 20, length(next_check_at) - 19 - length(CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(next_check_at, 1, 19) || CASE WHEN substr(next_check_at, -1) = 'Z' THEN 'Z' ELSE substr(next_check_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET last_poll_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(last_poll_at, 1, 19) || CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END)
       || '.' || substr(substr(substr(last_poll_at, 20, length(last_poll_at) - 19 - length(CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE last_poll_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (last_poll_at GLOB '*Z' OR last_poll_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(last_poll_at) = 30 AND last_poll_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(last_poll_at, 6, 2) BETWEEN '01' AND '12' AND substr(last_poll_at, 12, 2) <= '23'
   AND substr(last_poll_at, 15, 2) <= '59' AND substr(last_poll_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_poll_at, 1, 19)) = substr(last_poll_at, 1, 19)
   AND (substr(last_poll_at, 20, length(last_poll_at) - 19 - length(CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END)) = '' OR (substr(last_poll_at, 20, length(last_poll_at) - 19 - length(CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(last_poll_at, 20, length(last_poll_at) - 19 - length(CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_poll_at, 1, 19) || CASE WHEN substr(last_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_poll_at, -6) END) IS NOT NULL;
UPDATE delivery_requests SET last_successful_poll_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(last_successful_poll_at, 1, 19) || CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END)
       || '.' || substr(substr(substr(last_successful_poll_at, 20, length(last_successful_poll_at) - 19 - length(CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE last_successful_poll_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (last_successful_poll_at GLOB '*Z' OR last_successful_poll_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(last_successful_poll_at) = 30 AND last_successful_poll_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(last_successful_poll_at, 6, 2) BETWEEN '01' AND '12' AND substr(last_successful_poll_at, 12, 2) <= '23'
   AND substr(last_successful_poll_at, 15, 2) <= '59' AND substr(last_successful_poll_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_successful_poll_at, 1, 19)) = substr(last_successful_poll_at, 1, 19)
   AND (substr(last_successful_poll_at, 20, length(last_successful_poll_at) - 19 - length(CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END)) = '' OR (substr(last_successful_poll_at, 20, length(last_successful_poll_at) - 19 - length(CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(last_successful_poll_at, 20, length(last_successful_poll_at) - 19 - length(CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_successful_poll_at, 1, 19) || CASE WHEN substr(last_successful_poll_at, -1) = 'Z' THEN 'Z' ELSE substr(last_successful_poll_at, -6) END) IS NOT NULL;
UPDATE effect_permits SET lease_until =
       strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)
       || '.' || substr(substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (lease_until GLOB '*Z' OR lease_until GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(lease_until) = 30 AND lease_until GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(lease_until, 6, 2) BETWEEN '01' AND '12' AND substr(lease_until, 12, 2) <= '23'
   AND substr(lease_until, 15, 2) <= '59' AND substr(lease_until, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19)) = substr(lease_until, 1, 19)
   AND (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) = '' OR (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) GLOB '.[0-9]*' AND substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END) IS NOT NULL;
UPDATE effect_permits SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE effect_permits SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE events SET at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(at, 1, 19) || CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END)
       || '.' || substr(substr(substr(at, 20, length(at) - 19 - length(CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (at GLOB '*Z' OR at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(at) = 30 AND at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(at, 6, 2) BETWEEN '01' AND '12' AND substr(at, 12, 2) <= '23'
   AND substr(at, 15, 2) <= '59' AND substr(at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(at, 1, 19)) = substr(at, 1, 19)
   AND (substr(at, 20, length(at) - 19 - length(CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END)) = '' OR (substr(at, 20, length(at) - 19 - length(CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END)) GLOB '.[0-9]*' AND substr(substr(at, 20, length(at) - 19 - length(CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(at, 1, 19) || CASE WHEN substr(at, -1) = 'Z' THEN 'Z' ELSE substr(at, -6) END) IS NOT NULL;
UPDATE exports SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE human_actions SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE human_actions SET resolved_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(resolved_at, 1, 19) || CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END)
       || '.' || substr(substr(substr(resolved_at, 20, length(resolved_at) - 19 - length(CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE resolved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (resolved_at GLOB '*Z' OR resolved_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(resolved_at) = 30 AND resolved_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(resolved_at, 6, 2) BETWEEN '01' AND '12' AND substr(resolved_at, 12, 2) <= '23'
   AND substr(resolved_at, 15, 2) <= '59' AND substr(resolved_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(resolved_at, 1, 19)) = substr(resolved_at, 1, 19)
   AND (substr(resolved_at, 20, length(resolved_at) - 19 - length(CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END)) = '' OR (substr(resolved_at, 20, length(resolved_at) - 19 - length(CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(resolved_at, 20, length(resolved_at) - 19 - length(CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(resolved_at, 1, 19) || CASE WHEN substr(resolved_at, -1) = 'Z' THEN 'Z' ELSE substr(resolved_at, -6) END) IS NOT NULL;
UPDATE human_actions SET expires_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19) || CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)
       || '.' || substr(substr(substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (expires_at GLOB '*Z' OR expires_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(expires_at) = 30 AND expires_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(expires_at, 6, 2) BETWEEN '01' AND '12' AND substr(expires_at, 12, 2) <= '23'
   AND substr(expires_at, 15, 2) <= '59' AND substr(expires_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19)) = substr(expires_at, 1, 19)
   AND (substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)) = '' OR (substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19) || CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END) IS NOT NULL;
UPDATE human_gate_observations SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE human_gate_observations SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE institution_profiles SET tombstoned_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(tombstoned_at, 1, 19) || CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END)
       || '.' || substr(substr(substr(tombstoned_at, 20, length(tombstoned_at) - 19 - length(CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE tombstoned_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (tombstoned_at GLOB '*Z' OR tombstoned_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(tombstoned_at) = 30 AND tombstoned_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(tombstoned_at, 6, 2) BETWEEN '01' AND '12' AND substr(tombstoned_at, 12, 2) <= '23'
   AND substr(tombstoned_at, 15, 2) <= '59' AND substr(tombstoned_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(tombstoned_at, 1, 19)) = substr(tombstoned_at, 1, 19)
   AND (substr(tombstoned_at, 20, length(tombstoned_at) - 19 - length(CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END)) = '' OR (substr(tombstoned_at, 20, length(tombstoned_at) - 19 - length(CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(tombstoned_at, 20, length(tombstoned_at) - 19 - length(CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(tombstoned_at, 1, 19) || CASE WHEN substr(tombstoned_at, -1) = 'Z' THEN 'Z' ELSE substr(tombstoned_at, -6) END) IS NOT NULL;
UPDATE institution_profiles SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE institution_profiles SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE job_artifacts SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE jobs SET lease_expires_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(lease_expires_at, 1, 19) || CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END)
       || '.' || substr(substr(substr(lease_expires_at, 20, length(lease_expires_at) - 19 - length(CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE lease_expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (lease_expires_at GLOB '*Z' OR lease_expires_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(lease_expires_at) = 30 AND lease_expires_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(lease_expires_at, 6, 2) BETWEEN '01' AND '12' AND substr(lease_expires_at, 12, 2) <= '23'
   AND substr(lease_expires_at, 15, 2) <= '59' AND substr(lease_expires_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_expires_at, 1, 19)) = substr(lease_expires_at, 1, 19)
   AND (substr(lease_expires_at, 20, length(lease_expires_at) - 19 - length(CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END)) = '' OR (substr(lease_expires_at, 20, length(lease_expires_at) - 19 - length(CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(lease_expires_at, 20, length(lease_expires_at) - 19 - length(CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_expires_at, 1, 19) || CASE WHEN substr(lease_expires_at, -1) = 'Z' THEN 'Z' ELSE substr(lease_expires_at, -6) END) IS NOT NULL;
UPDATE jobs SET retry_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(retry_at, 1, 19) || CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END)
       || '.' || substr(substr(substr(retry_at, 20, length(retry_at) - 19 - length(CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE retry_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (retry_at GLOB '*Z' OR retry_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(retry_at) = 30 AND retry_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(retry_at, 6, 2) BETWEEN '01' AND '12' AND substr(retry_at, 12, 2) <= '23'
   AND substr(retry_at, 15, 2) <= '59' AND substr(retry_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(retry_at, 1, 19)) = substr(retry_at, 1, 19)
   AND (substr(retry_at, 20, length(retry_at) - 19 - length(CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END)) = '' OR (substr(retry_at, 20, length(retry_at) - 19 - length(CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(retry_at, 20, length(retry_at) - 19 - length(CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(retry_at, 1, 19) || CASE WHEN substr(retry_at, -1) = 'Z' THEN 'Z' ELSE substr(retry_at, -6) END) IS NOT NULL;
UPDATE jobs SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE jobs SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE legacy_effect_blockers SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE legacy_effect_blockers SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE materialization_claims SET lease_until =
       strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)
       || '.' || substr(substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE lease_until GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (lease_until GLOB '*Z' OR lease_until GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(lease_until) = 30 AND lease_until GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(lease_until, 6, 2) BETWEEN '01' AND '12' AND substr(lease_until, 12, 2) <= '23'
   AND substr(lease_until, 15, 2) <= '59' AND substr(lease_until, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19)) = substr(lease_until, 1, 19)
   AND (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) = '' OR (substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)) GLOB '.[0-9]*' AND substr(substr(lease_until, 20, length(lease_until) - 19 - length(CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(lease_until, 1, 19) || CASE WHEN substr(lease_until, -1) = 'Z' THEN 'Z' ELSE substr(lease_until, -6) END) IS NOT NULL;
UPDATE materialization_claims SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE materialization_claims SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET window_start =
       strftime('%Y-%m-%dT%H:%M:%S', substr(window_start, 1, 19) || CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END)
       || '.' || substr(substr(substr(window_start, 20, length(window_start) - 19 - length(CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE window_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (window_start GLOB '*Z' OR window_start GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(window_start) = 30 AND window_start GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(window_start, 6, 2) BETWEEN '01' AND '12' AND substr(window_start, 12, 2) <= '23'
   AND substr(window_start, 15, 2) <= '59' AND substr(window_start, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(window_start, 1, 19)) = substr(window_start, 1, 19)
   AND (substr(window_start, 20, length(window_start) - 19 - length(CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END)) = '' OR (substr(window_start, 20, length(window_start) - 19 - length(CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END)) GLOB '.[0-9]*' AND substr(substr(window_start, 20, length(window_start) - 19 - length(CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(window_start, 1, 19) || CASE WHEN substr(window_start, -1) = 'Z' THEN 'Z' ELSE substr(window_start, -6) END) IS NOT NULL;
UPDATE notification_intents SET first_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(first_at, 1, 19) || CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END)
       || '.' || substr(substr(substr(first_at, 20, length(first_at) - 19 - length(CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE first_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (first_at GLOB '*Z' OR first_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(first_at) = 30 AND first_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(first_at, 6, 2) BETWEEN '01' AND '12' AND substr(first_at, 12, 2) <= '23'
   AND substr(first_at, 15, 2) <= '59' AND substr(first_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(first_at, 1, 19)) = substr(first_at, 1, 19)
   AND (substr(first_at, 20, length(first_at) - 19 - length(CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END)) = '' OR (substr(first_at, 20, length(first_at) - 19 - length(CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(first_at, 20, length(first_at) - 19 - length(CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(first_at, 1, 19) || CASE WHEN substr(first_at, -1) = 'Z' THEN 'Z' ELSE substr(first_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET last_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(last_at, 1, 19) || CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END)
       || '.' || substr(substr(substr(last_at, 20, length(last_at) - 19 - length(CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE last_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (last_at GLOB '*Z' OR last_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(last_at) = 30 AND last_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(last_at, 6, 2) BETWEEN '01' AND '12' AND substr(last_at, 12, 2) <= '23'
   AND substr(last_at, 15, 2) <= '59' AND substr(last_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_at, 1, 19)) = substr(last_at, 1, 19)
   AND (substr(last_at, 20, length(last_at) - 19 - length(CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END)) = '' OR (substr(last_at, 20, length(last_at) - 19 - length(CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(last_at, 20, length(last_at) - 19 - length(CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_at, 1, 19) || CASE WHEN substr(last_at, -1) = 'Z' THEN 'Z' ELSE substr(last_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET available_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(available_at, 1, 19) || CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END)
       || '.' || substr(substr(substr(available_at, 20, length(available_at) - 19 - length(CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE available_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (available_at GLOB '*Z' OR available_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(available_at) = 30 AND available_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(available_at, 6, 2) BETWEEN '01' AND '12' AND substr(available_at, 12, 2) <= '23'
   AND substr(available_at, 15, 2) <= '59' AND substr(available_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(available_at, 1, 19)) = substr(available_at, 1, 19)
   AND (substr(available_at, 20, length(available_at) - 19 - length(CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END)) = '' OR (substr(available_at, 20, length(available_at) - 19 - length(CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(available_at, 20, length(available_at) - 19 - length(CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(available_at, 1, 19) || CASE WHEN substr(available_at, -1) = 'Z' THEN 'Z' ELSE substr(available_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET desktop_reserved_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_reserved_at, 1, 19) || CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END)
       || '.' || substr(substr(substr(desktop_reserved_at, 20, length(desktop_reserved_at) - 19 - length(CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE desktop_reserved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (desktop_reserved_at GLOB '*Z' OR desktop_reserved_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(desktop_reserved_at) = 30 AND desktop_reserved_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(desktop_reserved_at, 6, 2) BETWEEN '01' AND '12' AND substr(desktop_reserved_at, 12, 2) <= '23'
   AND substr(desktop_reserved_at, 15, 2) <= '59' AND substr(desktop_reserved_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_reserved_at, 1, 19)) = substr(desktop_reserved_at, 1, 19)
   AND (substr(desktop_reserved_at, 20, length(desktop_reserved_at) - 19 - length(CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END)) = '' OR (substr(desktop_reserved_at, 20, length(desktop_reserved_at) - 19 - length(CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(desktop_reserved_at, 20, length(desktop_reserved_at) - 19 - length(CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_reserved_at, 1, 19) || CASE WHEN substr(desktop_reserved_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_reserved_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET desktop_attempted_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_attempted_at, 1, 19) || CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END)
       || '.' || substr(substr(substr(desktop_attempted_at, 20, length(desktop_attempted_at) - 19 - length(CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE desktop_attempted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (desktop_attempted_at GLOB '*Z' OR desktop_attempted_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(desktop_attempted_at) = 30 AND desktop_attempted_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(desktop_attempted_at, 6, 2) BETWEEN '01' AND '12' AND substr(desktop_attempted_at, 12, 2) <= '23'
   AND substr(desktop_attempted_at, 15, 2) <= '59' AND substr(desktop_attempted_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_attempted_at, 1, 19)) = substr(desktop_attempted_at, 1, 19)
   AND (substr(desktop_attempted_at, 20, length(desktop_attempted_at) - 19 - length(CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END)) = '' OR (substr(desktop_attempted_at, 20, length(desktop_attempted_at) - 19 - length(CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(desktop_attempted_at, 20, length(desktop_attempted_at) - 19 - length(CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(desktop_attempted_at, 1, 19) || CASE WHEN substr(desktop_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(desktop_attempted_at, -6) END) IS NOT NULL;
UPDATE notification_intents SET webhook_attempted_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(webhook_attempted_at, 1, 19) || CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END)
       || '.' || substr(substr(substr(webhook_attempted_at, 20, length(webhook_attempted_at) - 19 - length(CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE webhook_attempted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (webhook_attempted_at GLOB '*Z' OR webhook_attempted_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(webhook_attempted_at) = 30 AND webhook_attempted_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(webhook_attempted_at, 6, 2) BETWEEN '01' AND '12' AND substr(webhook_attempted_at, 12, 2) <= '23'
   AND substr(webhook_attempted_at, 15, 2) <= '59' AND substr(webhook_attempted_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(webhook_attempted_at, 1, 19)) = substr(webhook_attempted_at, 1, 19)
   AND (substr(webhook_attempted_at, 20, length(webhook_attempted_at) - 19 - length(CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END)) = '' OR (substr(webhook_attempted_at, 20, length(webhook_attempted_at) - 19 - length(CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(webhook_attempted_at, 20, length(webhook_attempted_at) - 19 - length(CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(webhook_attempted_at, 1, 19) || CASE WHEN substr(webhook_attempted_at, -1) = 'Z' THEN 'Z' ELSE substr(webhook_attempted_at, -6) END) IS NOT NULL;
UPDATE page_bulk_runs SET opened_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(opened_at, 1, 19) || CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END)
       || '.' || substr(substr(substr(opened_at, 20, length(opened_at) - 19 - length(CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE opened_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (opened_at GLOB '*Z' OR opened_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(opened_at) = 30 AND opened_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(opened_at, 6, 2) BETWEEN '01' AND '12' AND substr(opened_at, 12, 2) <= '23'
   AND substr(opened_at, 15, 2) <= '59' AND substr(opened_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(opened_at, 1, 19)) = substr(opened_at, 1, 19)
   AND (substr(opened_at, 20, length(opened_at) - 19 - length(CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END)) = '' OR (substr(opened_at, 20, length(opened_at) - 19 - length(CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(opened_at, 20, length(opened_at) - 19 - length(CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(opened_at, 1, 19) || CASE WHEN substr(opened_at, -1) = 'Z' THEN 'Z' ELSE substr(opened_at, -6) END) IS NOT NULL;
UPDATE page_bulk_runs SET submitted_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19) || CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)
       || '.' || substr(substr(substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE submitted_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (submitted_at GLOB '*Z' OR submitted_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(submitted_at) = 30 AND submitted_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(submitted_at, 6, 2) BETWEEN '01' AND '12' AND substr(submitted_at, 12, 2) <= '23'
   AND substr(submitted_at, 15, 2) <= '59' AND substr(submitted_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19)) = substr(submitted_at, 1, 19)
   AND (substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)) = '' OR (substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(submitted_at, 20, length(submitted_at) - 19 - length(CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(submitted_at, 1, 19) || CASE WHEN substr(submitted_at, -1) = 'Z' THEN 'Z' ELSE substr(submitted_at, -6) END) IS NOT NULL;
UPDATE pdf_grab_eligibility_snapshots SET recorded_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19) || CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)
       || '.' || substr(substr(substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE recorded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (recorded_at GLOB '*Z' OR recorded_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(recorded_at) = 30 AND recorded_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(recorded_at, 6, 2) BETWEEN '01' AND '12' AND substr(recorded_at, 12, 2) <= '23'
   AND substr(recorded_at, 15, 2) <= '59' AND substr(recorded_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19)) = substr(recorded_at, 1, 19)
   AND (substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)) = '' OR (substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19) || CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END) IS NOT NULL;
UPDATE pdf_grabs SET notified_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(notified_at, 1, 19) || CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END)
       || '.' || substr(substr(substr(notified_at, 20, length(notified_at) - 19 - length(CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE notified_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (notified_at GLOB '*Z' OR notified_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(notified_at) = 30 AND notified_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(notified_at, 6, 2) BETWEEN '01' AND '12' AND substr(notified_at, 12, 2) <= '23'
   AND substr(notified_at, 15, 2) <= '59' AND substr(notified_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(notified_at, 1, 19)) = substr(notified_at, 1, 19)
   AND (substr(notified_at, 20, length(notified_at) - 19 - length(CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END)) = '' OR (substr(notified_at, 20, length(notified_at) - 19 - length(CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(notified_at, 20, length(notified_at) - 19 - length(CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(notified_at, 1, 19) || CASE WHEN substr(notified_at, -1) = 'Z' THEN 'Z' ELSE substr(notified_at, -6) END) IS NOT NULL;
UPDATE pdf_grabs SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE pdf_grabs SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE profile_evidence SET producer_observed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(producer_observed_at, 1, 19) || CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END)
       || '.' || substr(substr(substr(producer_observed_at, 20, length(producer_observed_at) - 19 - length(CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE producer_observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (producer_observed_at GLOB '*Z' OR producer_observed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(producer_observed_at) = 30 AND producer_observed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(producer_observed_at, 6, 2) BETWEEN '01' AND '12' AND substr(producer_observed_at, 12, 2) <= '23'
   AND substr(producer_observed_at, 15, 2) <= '59' AND substr(producer_observed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(producer_observed_at, 1, 19)) = substr(producer_observed_at, 1, 19)
   AND (substr(producer_observed_at, 20, length(producer_observed_at) - 19 - length(CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END)) = '' OR (substr(producer_observed_at, 20, length(producer_observed_at) - 19 - length(CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(producer_observed_at, 20, length(producer_observed_at) - 19 - length(CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(producer_observed_at, 1, 19) || CASE WHEN substr(producer_observed_at, -1) = 'Z' THEN 'Z' ELSE substr(producer_observed_at, -6) END) IS NOT NULL;
UPDATE profile_evidence SET daemon_received_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(daemon_received_at, 1, 19) || CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END)
       || '.' || substr(substr(substr(daemon_received_at, 20, length(daemon_received_at) - 19 - length(CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE daemon_received_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (daemon_received_at GLOB '*Z' OR daemon_received_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(daemon_received_at) = 30 AND daemon_received_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(daemon_received_at, 6, 2) BETWEEN '01' AND '12' AND substr(daemon_received_at, 12, 2) <= '23'
   AND substr(daemon_received_at, 15, 2) <= '59' AND substr(daemon_received_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(daemon_received_at, 1, 19)) = substr(daemon_received_at, 1, 19)
   AND (substr(daemon_received_at, 20, length(daemon_received_at) - 19 - length(CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END)) = '' OR (substr(daemon_received_at, 20, length(daemon_received_at) - 19 - length(CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(daemon_received_at, 20, length(daemon_received_at) - 19 - length(CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(daemon_received_at, 1, 19) || CASE WHEN substr(daemon_received_at, -1) = 'Z' THEN 'Z' ELSE substr(daemon_received_at, -6) END) IS NOT NULL;
UPDATE profile_evidence SET expires_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19) || CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)
       || '.' || substr(substr(substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (expires_at GLOB '*Z' OR expires_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(expires_at) = 30 AND expires_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(expires_at, 6, 2) BETWEEN '01' AND '12' AND substr(expires_at, 12, 2) <= '23'
   AND substr(expires_at, 15, 2) <= '59' AND substr(expires_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19)) = substr(expires_at, 1, 19)
   AND (substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)) = '' OR (substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(expires_at, 20, length(expires_at) - 19 - length(CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(expires_at, 1, 19) || CASE WHEN substr(expires_at, -1) = 'Z' THEN 'Z' ELSE substr(expires_at, -6) END) IS NOT NULL;
UPDATE retraction_acks SET acked_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(acked_at, 1, 19) || CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END)
       || '.' || substr(substr(substr(acked_at, 20, length(acked_at) - 19 - length(CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE acked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (acked_at GLOB '*Z' OR acked_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(acked_at) = 30 AND acked_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(acked_at, 6, 2) BETWEEN '01' AND '12' AND substr(acked_at, 12, 2) <= '23'
   AND substr(acked_at, 15, 2) <= '59' AND substr(acked_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(acked_at, 1, 19)) = substr(acked_at, 1, 19)
   AND (substr(acked_at, 20, length(acked_at) - 19 - length(CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END)) = '' OR (substr(acked_at, 20, length(acked_at) - 19 - length(CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(acked_at, 20, length(acked_at) - 19 - length(CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(acked_at, 1, 19) || CASE WHEN substr(acked_at, -1) = 'Z' THEN 'Z' ELSE substr(acked_at, -6) END) IS NOT NULL;
UPDATE route_suppressions SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE route_suppressions SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;
UPDATE source_budgets SET next_allowed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(next_allowed_at, 1, 19) || CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END)
       || '.' || substr(substr(substr(next_allowed_at, 20, length(next_allowed_at) - 19 - length(CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE next_allowed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (next_allowed_at GLOB '*Z' OR next_allowed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(next_allowed_at) = 30 AND next_allowed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(next_allowed_at, 6, 2) BETWEEN '01' AND '12' AND substr(next_allowed_at, 12, 2) <= '23'
   AND substr(next_allowed_at, 15, 2) <= '59' AND substr(next_allowed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(next_allowed_at, 1, 19)) = substr(next_allowed_at, 1, 19)
   AND (substr(next_allowed_at, 20, length(next_allowed_at) - 19 - length(CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END)) = '' OR (substr(next_allowed_at, 20, length(next_allowed_at) - 19 - length(CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(next_allowed_at, 20, length(next_allowed_at) - 19 - length(CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(next_allowed_at, 1, 19) || CASE WHEN substr(next_allowed_at, -1) = 'Z' THEN 'Z' ELSE substr(next_allowed_at, -6) END) IS NOT NULL;
UPDATE source_credit_fuse SET drift_closed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(drift_closed_at, 1, 19) || CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END)
       || '.' || substr(substr(substr(drift_closed_at, 20, length(drift_closed_at) - 19 - length(CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE drift_closed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (drift_closed_at GLOB '*Z' OR drift_closed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(drift_closed_at) = 30 AND drift_closed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(drift_closed_at, 6, 2) BETWEEN '01' AND '12' AND substr(drift_closed_at, 12, 2) <= '23'
   AND substr(drift_closed_at, 15, 2) <= '59' AND substr(drift_closed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(drift_closed_at, 1, 19)) = substr(drift_closed_at, 1, 19)
   AND (substr(drift_closed_at, 20, length(drift_closed_at) - 19 - length(CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END)) = '' OR (substr(drift_closed_at, 20, length(drift_closed_at) - 19 - length(CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(drift_closed_at, 20, length(drift_closed_at) - 19 - length(CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(drift_closed_at, 1, 19) || CASE WHEN substr(drift_closed_at, -1) = 'Z' THEN 'Z' ELSE substr(drift_closed_at, -6) END) IS NOT NULL;
UPDATE validation_reports SET recorded_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19) || CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)
       || '.' || substr(substr(substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE recorded_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (recorded_at GLOB '*Z' OR recorded_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(recorded_at) = 30 AND recorded_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(recorded_at, 6, 2) BETWEEN '01' AND '12' AND substr(recorded_at, 12, 2) <= '23'
   AND substr(recorded_at, 15, 2) <= '59' AND substr(recorded_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19)) = substr(recorded_at, 1, 19)
   AND (substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)) = '' OR (substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(recorded_at, 20, length(recorded_at) - 19 - length(CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(recorded_at, 1, 19) || CASE WHEN substr(recorded_at, -1) = 'Z' THEN 'Z' ELSE substr(recorded_at, -6) END) IS NOT NULL;
UPDATE watch_digest_entries SET first_seen_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(first_seen_at, 1, 19) || CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END)
       || '.' || substr(substr(substr(first_seen_at, 20, length(first_seen_at) - 19 - length(CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE first_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (first_seen_at GLOB '*Z' OR first_seen_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(first_seen_at) = 30 AND first_seen_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(first_seen_at, 6, 2) BETWEEN '01' AND '12' AND substr(first_seen_at, 12, 2) <= '23'
   AND substr(first_seen_at, 15, 2) <= '59' AND substr(first_seen_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(first_seen_at, 1, 19)) = substr(first_seen_at, 1, 19)
   AND (substr(first_seen_at, 20, length(first_seen_at) - 19 - length(CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END)) = '' OR (substr(first_seen_at, 20, length(first_seen_at) - 19 - length(CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(first_seen_at, 20, length(first_seen_at) - 19 - length(CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(first_seen_at, 1, 19) || CASE WHEN substr(first_seen_at, -1) = 'Z' THEN 'Z' ELSE substr(first_seen_at, -6) END) IS NOT NULL;
UPDATE watches SET last_run_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(last_run_at, 1, 19) || CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END)
       || '.' || substr(substr(substr(last_run_at, 20, length(last_run_at) - 19 - length(CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE last_run_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (last_run_at GLOB '*Z' OR last_run_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(last_run_at) = 30 AND last_run_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(last_run_at, 6, 2) BETWEEN '01' AND '12' AND substr(last_run_at, 12, 2) <= '23'
   AND substr(last_run_at, 15, 2) <= '59' AND substr(last_run_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_run_at, 1, 19)) = substr(last_run_at, 1, 19)
   AND (substr(last_run_at, 20, length(last_run_at) - 19 - length(CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END)) = '' OR (substr(last_run_at, 20, length(last_run_at) - 19 - length(CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(last_run_at, 20, length(last_run_at) - 19 - length(CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(last_run_at, 1, 19) || CASE WHEN substr(last_run_at, -1) = 'Z' THEN 'Z' ELSE substr(last_run_at, -6) END) IS NOT NULL;
UPDATE watches SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE work_requests SET created_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)
       || '.' || substr(substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (created_at GLOB '*Z' OR created_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(created_at) = 30 AND created_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(created_at, 6, 2) BETWEEN '01' AND '12' AND substr(created_at, 12, 2) <= '23'
   AND substr(created_at, 15, 2) <= '59' AND substr(created_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19)) = substr(created_at, 1, 19)
   AND (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) = '' OR (substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(created_at, 20, length(created_at) - 19 - length(CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(created_at, 1, 19) || CASE WHEN substr(created_at, -1) = 'Z' THEN 'Z' ELSE substr(created_at, -6) END) IS NOT NULL;
UPDATE zotio_item_scope SET observed_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(observed_at, 1, 19) || CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END)
       || '.' || substr(substr(substr(observed_at, 20, length(observed_at) - 19 - length(CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE observed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (observed_at GLOB '*Z' OR observed_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(observed_at) = 30 AND observed_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(observed_at, 6, 2) BETWEEN '01' AND '12' AND substr(observed_at, 12, 2) <= '23'
   AND substr(observed_at, 15, 2) <= '59' AND substr(observed_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(observed_at, 1, 19)) = substr(observed_at, 1, 19)
   AND (substr(observed_at, 20, length(observed_at) - 19 - length(CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END)) = '' OR (substr(observed_at, 20, length(observed_at) - 19 - length(CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(observed_at, 20, length(observed_at) - 19 - length(CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(observed_at, 1, 19) || CASE WHEN substr(observed_at, -1) = 'Z' THEN 'Z' ELSE substr(observed_at, -6) END) IS NOT NULL;
UPDATE zotio_tag_state SET updated_at =
       strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)
       || '.' || substr(substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) || '000000000', 1, 9) || 'Z'
 WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]?*'
   AND (updated_at GLOB '*Z' OR updated_at GLOB '*[+-][0-9][0-9]:[0-9][0-9]')
   AND NOT (length(updated_at) = 30 AND updated_at GLOB '*.[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z')
   AND substr(updated_at, 6, 2) BETWEEN '01' AND '12' AND substr(updated_at, 12, 2) <= '23'
   AND substr(updated_at, 15, 2) <= '59' AND substr(updated_at, 18, 2) <= '59'
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19)) = substr(updated_at, 1, 19)
   AND (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) = '' OR (substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)) GLOB '.[0-9]*' AND substr(substr(updated_at, 20, length(updated_at) - 19 - length(CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END)), 2) NOT GLOB '*[^0-9]*'))
   AND strftime('%Y-%m-%dT%H:%M:%S', substr(updated_at, 1, 19) || CASE WHEN substr(updated_at, -1) = 'Z' THEN 'Z' ELSE substr(updated_at, -6) END) IS NOT NULL;

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
