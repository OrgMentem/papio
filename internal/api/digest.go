// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/watch"
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
		var partial *watch.DigestPartialError
		if errors.As(err, &partial) {
			// The count of works that already left the digest is the one
			// fact the operator needs, and the message is the only place it
			// can travel without widening the result.
			_, rpcErr := watchFailure(err)
			rpcErr.Message = safeMessage(partial, rpcErr.Message)
			rpcErr.Detail.ErrorHint = ""
			return nil, rpcErr
		}
		switch {
		case errors.Is(err, watch.ErrDigestEntryNotFound):
			return nil, &ipc.RPCError{Code: "not_found", Message: safeMessage(err, "watch digest entry not found") + " is not pending in watch " + strconv.FormatInt(params.ID, 10)}
		case errors.Is(err, sql.ErrNoRows):
			return nil, &ipc.RPCError{Code: "not_found", Message: "watch " + strconv.FormatInt(params.ID, 10) + " not found; run `papio watch list` to see watch ids"}
		}
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
	if system == nil || system.WatchRunner == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	cleared, err := system.WatchRunner.ClearDigest(ctx, params.ID)
	if err != nil {
		return failure(err)
	}
	return marshal(WatchDigestClearResult{Cleared: cleared})
}
