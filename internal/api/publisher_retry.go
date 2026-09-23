// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/drive"
	"papio/internal/ipc"
)

func retryPublisher(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ActionID int64 `json:"action_id"`
		Revision int64 `json:"expected_revision"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.ActionID <= 0 || params.Revision <= 0 {
		return badParams(errors.New("action_id and expected_revision must be positive"))
	}
	id, err := system.Browser.RetryPublisher(ctx, params.ActionID, params.Revision)
	if err != nil {
		return failure(err)
	}
	drive.RecordHandoffOpened(ctx, system.Jobs, []string{id}, PrincipalFrom(ctx))
	return marshal(SubmitResult{JobID: id})
}
