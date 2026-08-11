// Copyright 2026 OrgMentem. Licensed under MIT.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
)

// JobDiagnosis is a versioned read model for the CLI's agent-facing job
// diagnosis. It is a new RPC result rather than an addition to jobs.get:
// existing IPC decoders reject widened responses.
type JobDiagnosis = job.Diagnosis

func diagnoseJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	row, err := system.Jobs.Get(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	events, err := system.Jobs.Events(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	attributed, err := system.Jobs.ListHumanActionsForJob(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	actions := make([]job.HumanAction, 0, len(attributed))
	for _, item := range attributed {
		actions = append(actions, item.Action)
	}
	return marshal(job.Diagnose(row, actions, events))
}
