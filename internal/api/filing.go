// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"papio/internal/app"
	"papio/internal/bootstrap"
	"papio/internal/ipc"
)

// UnfiledPage is the bounded jobs.unfiled filing read model.
type UnfiledPage struct {
	Jobs      []app.UnfiledJob `json:"jobs"`
	Truncated bool             `json:"truncated"`
}

func unfiledJobs(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		Filter string `json:"filter,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	filter := app.UnfiledFilter(strings.TrimSpace(params.Filter))
	if filter == "" {
		filter = app.UnfiledAll
	}
	switch filter {
	case app.UnfiledFailed, app.UnfiledMissing, app.UnfiledAll:
	default:
		return badParams(errors.New("filter must be failed, missing, or all"))
	}
	if system == nil || system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "application service is not configured"}
	}
	jobs, truncated, err := system.App.UnfiledJobs(ctx, filter, params.Limit)
	if errors.Is(err, app.ErrReadyHookNotConfigured) {
		return badParams(app.ErrReadyHookNotConfigured)
	}
	if err != nil {
		return failure(err)
	}
	return marshal(UnfiledPage{Jobs: jobs, Truncated: truncated})
}

func refileJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	params.JobID = strings.TrimSpace(params.JobID)
	if params.JobID == "" {
		return badParams(errors.New("job_id is required"))
	}
	if system == nil || system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "application service is not configured"}
	}
	result, err := system.App.RefileJob(ctx, params.JobID)
	if errors.Is(err, app.ErrReadyHookNotConfigured) {
		return badParams(app.ErrReadyHookNotConfigured)
	}
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}
