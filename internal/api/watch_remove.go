// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/watch"
)

// WatchRemoveDigestPendingClass marks a watch.remove_v2 refusal: the watch
// still holds pending digest works and discard_digest was false. The CLI keys
// its guidance on it, so it is part of the method's contract.
const WatchRemoveDigestPendingClass = "watch_remove_digest_pending"

// WatchRemoveV2Params is watch.remove_v2: watch.remove plus the caller's
// digest decision. It is a new method rather than a flag on watch.remove
// because params decode strictly: a discard_digest an older daemon rejected
// as unknown would be indistinguishable from a malformed request, while an
// unknown method is a clean signal for the CLI to fall back to its two-step
// guard.
type WatchRemoveV2Params struct {
	ID            int64 `json:"id"`
	DiscardDigest bool  `json:"discard_digest,omitempty"`
}

// removeWatchV2 deletes a watch, refusing (rather than silently dropping
// newly recorded works) when pending digest entries exist and discard_digest
// is false. The pending check and the delete run in one store transaction, so
// an alert run recording between the CLI's digest read and this delete cannot
// lose works.
func removeWatchV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params WatchRemoveV2Params
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	if err := system.Watches.RemoveIfNoPendingDigest(ctx, params.ID, params.DiscardDigest); err != nil {
		if errors.Is(err, watch.ErrWatchDigestPending) {
			return nil, &ipc.RPCError{Code: "precondition_failed", Message: err.Error(), Detail: &ipc.ErrorDetail{ErrorClass: WatchRemoveDigestPendingClass}}
		}
		return failure(err)
	}
	return marshal(WatchRemoveResult{ID: params.ID, Removed: true})
}
