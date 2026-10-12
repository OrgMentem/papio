// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
)

func acquisitionDisposition(ctx context.Context, raw json.RawMessage, system *bootstrap.System, operation string) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID     string `json:"job_id"`
		Confirmed bool   `json:"confirmed,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	params.JobID = strings.TrimSpace(params.JobID)
	if params.JobID == "" {
		return badParams(errors.New("job_id is required"))
	}
	if operation == "archive" && !params.Confirmed {
		return badParams(errors.New("archive requires confirmed=true; artifacts remain retained"))
	}
	if system == nil || system.Jobs == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "job store is not configured"}
	}
	switch operation {
	case "archive":
		result, err := system.Jobs.Archive(ctx, params.JobID, params.Confirmed)
		if err != nil {
			return failure(err)
		}
		return marshal(result)
	case "restore":
		result, err := system.Jobs.Restore(ctx, params.JobID)
		if err != nil {
			return failure(err)
		}
		return marshal(result)
	default:
		result, err := system.Jobs.Disposition(ctx, params.JobID)
		if err != nil {
			return failure(err)
		}
		return marshal(result)
	}
}
