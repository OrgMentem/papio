// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
)

// producerStats serves stats.producers_v1: the per-producer breakdown of
// artifact.producer events in a period, and how many of those acquisitions
// needed a person. It is a new method rather than a wider stats.get because
// the IPC envelope is decoded with DisallowUnknownFields, and stats.get's
// result is also the browser extension's stats view.
func producerStats(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		Since string `json:"since"`
		Until string `json:"until,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.Since == "" {
		return badParams(errors.New("since is required"))
	}
	since, err := time.Parse(time.RFC3339Nano, params.Since)
	if err != nil {
		return badParams(errors.New("since must be an RFC3339 instant"))
	}
	// An omitted until leaves the period open at its end (a zero time), so a
	// promotion recorded just before this call counts even when the clock
	// gives it the same reading as now.
	var until time.Time
	end := time.Now()
	if params.Until != "" {
		if until, err = time.Parse(time.RFC3339Nano, params.Until); err != nil {
			return badParams(errors.New("until must be an RFC3339 instant"))
		}
		end = until
	}
	if !end.After(since) {
		return badParams(errors.New("until must be after since"))
	}
	if system == nil || system.Jobs == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "job store is not configured"}
	}
	stats, err := system.Jobs.ProducerStats(ctx, since, until)
	if err != nil {
		return failure(err)
	}
	return marshal(stats)
}
