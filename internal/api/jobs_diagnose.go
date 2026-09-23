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

// JobDiagnosisV2 adds the observation-only institutional cutover projection.
// The v1 alias above remains unchanged for strict older clients.
type JobDiagnosisV2 = job.DiagnosisV2

func diagnoseJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return diagnoseJobWithProjection(ctx, raw, system, false)
}

func diagnoseJobV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return diagnoseJobWithProjection(ctx, raw, system, true)
}

func diagnoseJobWithProjection(ctx context.Context, raw json.RawMessage, system *bootstrap.System, v2 bool) ([]byte, *ipc.RPCError) {
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
	// An opened handoff that is parked behind a sibling's institutional surface
	// names that sibling instead of repeating advice the operator already took.
	queued := func(d job.Diagnosis) (job.Diagnosis, error) {
		handoff, opened := job.OpenedHandoff(row, actions, events)
		if !opened {
			return d, nil
		}
		blocker, err := system.Jobs.HandoffQueueBlocker(ctx, row.ID, handoff.RequiresAuth)
		if err != nil || blocker == nil {
			return d, err
		}
		return d.QueuedBehind(*blocker), nil
	}
	if v2 {
		diagnosis := job.DiagnoseV2(row, actions, events)
		if diagnosis.Diagnosis, err = queued(diagnosis.Diagnosis); err != nil {
			return failure(err)
		}
		return marshal(diagnosis)
	}
	diagnosis, err := queued(job.Diagnose(row, actions, events))
	if err != nil {
		return failure(err)
	}
	return marshal(diagnosis)
}
