-- Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
-- ADR-0017 Decision 4: the delivery poll executor's bookkeeping columns.
-- provider_status_raw always carries the exact TransactionStatus string the
-- provider last returned, independent of whether internal/delivery's
-- hard-coded state map understood it (ILLiad statuses are institution-
-- customizable, so there is no exhaustive enum — see internal/delivery/
-- poll.go). provider_display_status is the last "meaningful" status the
-- poll executor observed: it is left untouched on an unmapped/custom read,
-- so the Request Finished tri-branch can still find fulfilled/cancelled
-- evidence recorded by an earlier poll even if this row's most recent read
-- came back Request Finished. last_poll_at/last_successful_poll_at and
-- consecutive_poll_failures/last_poll_error_class implement the failure
-- discipline the ADR requires: a failed poll only ever advances these
-- bookkeeping columns, never delivery_requests.state.
ALTER TABLE delivery_requests ADD COLUMN provider_status_raw TEXT;
ALTER TABLE delivery_requests ADD COLUMN provider_display_status TEXT;
ALTER TABLE delivery_requests ADD COLUMN last_poll_at TEXT;
ALTER TABLE delivery_requests ADD COLUMN last_successful_poll_at TEXT;
ALTER TABLE delivery_requests ADD COLUMN consecutive_poll_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE delivery_requests ADD COLUMN last_poll_error_class TEXT;
