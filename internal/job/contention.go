// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
)

// contendingStates are the job states whose occupants will make resolver
// requests: one is in a pass now, one is parked waiting to retry a pass, one has
// not started yet. Everything else is either past the resolve stage or waiting
// on a person.
//
// awaiting_human is deliberately absent, and it is the state most likely to be
// wrongly added back. Measured on the operator's store: of 139 jobs parked in
// awaiting_human, zero had made a single wire attempt since parking. A parked
// job is not competing for an allowance; counting it would make contention
// permanently true on any long-lived install and turn a share that binds under
// pressure into a share that always binds.
var contendingStates = []string{StateResolving, StateRetryWait, StateQueued}

// OtherWorkWaiting reports whether any job besides exceptJobID is waiting to
// make resolver requests. It is the signal that makes a per-job credit share
// bind: with nothing else waiting, deferring a job that has taken its share
// helps nobody, because an unspent allowance cannot be carried forward.
//
// The question is deliberately not per-source. A resolve pass calls every
// enabled source in turn, so a job waiting to resolve is contending for all of
// them at once; a per-source answer would need a per-source intent papio does
// not record and does not need.
func (s *Store) OtherWorkWaiting(ctx context.Context, exceptJobID string) (bool, error) {
	if s == nil || s.S == nil {
		return false, errors.New("job: store is not configured")
	}
	query := `SELECT EXISTS (SELECT 1 FROM jobs WHERE id <> ? AND state IN (?, ?, ?))`
	args := []any{exceptJobID}
	for _, state := range contendingStates {
		args = append(args, state)
	}
	var waiting bool
	if err := s.S.DB().QueryRowContext(ctx, query, args...).Scan(&waiting); err != nil {
		return false, err
	}
	return waiting, nil
}
