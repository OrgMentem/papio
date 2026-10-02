// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/protocol"
)

func submitOnce(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input struct {
		Request    protocol.WorkRequest `json:"request"`
		AutoImport *bool                `json:"auto_import,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if system == nil || system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "acquisition is not configured"}
	}
	id, err := system.App.SubmitOnceWithAutoImport(ctx, input.Request, input.AutoImport)
	if err != nil {
		return failure(err)
	}
	return marshal(map[string]string{"job_id": id})
}
