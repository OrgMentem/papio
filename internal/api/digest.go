// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
)

// WatchDigestAcquireResult reports how many pending digest entries were queued.
type WatchDigestAcquireResult struct {
	Queued int `json:"queued"`
}

// WatchDigestClearResult reports how many pending digest entries were removed.
type WatchDigestClearResult struct {
	Cleared int `json:"cleared"`
}

func acquireWatchDigest(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ID   int64    `json:"id"`
		Keys []string `json:"keys,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.WatchRunner == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	queued, err := system.WatchRunner.AcquireDigest(ctx, params.ID, params.Keys)
	if err != nil {
		return watchFailure(err)
	}
	return marshal(WatchDigestAcquireResult{Queued: queued})
}

func clearWatchDigest(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ID int64 `json:"id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	cleared, err := system.Watches.ClearDigest(ctx, params.ID)
	if err != nil {
		return failure(err)
	}
	return marshal(WatchDigestClearResult{Cleared: cleared})
}
