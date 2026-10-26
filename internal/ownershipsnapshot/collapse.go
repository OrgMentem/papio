// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"time"

	"papio/internal/store"
)

// CollapseObservation distinguishes a transient export truncation from an
// intentional reduction. Consecutive complete reads must report the same
// collapsed count over at least 72 hours before callers accept the new size.
// A different count, a failed read, or a restored export resets the window.
// Persist this alongside the last good count so restarts do not reset it.
type CollapseObservation struct {
	Count int    `json:"count"`
	Since string `json:"since,omitempty"`
}

// Accept observes one already-detected collapse and reports whether it has
// remained unchanged for the full window. Callers clear the observation on
// every non-collapsed or failed read, and after accepting the new baseline.
func (o *CollapseObservation) Accept(count int, now time.Time) bool {
	since, err := time.Parse(time.RFC3339Nano, o.Since)
	if err != nil || o.Count != count || now.Before(since) {
		o.Count, o.Since = count, store.FormatTime(now)
		return false
	}
	return now.Sub(since) >= 72*time.Hour
}
