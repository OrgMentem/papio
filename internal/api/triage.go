// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/triage"
)

type triageDecideResult struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

func triageSnapshot(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var request triage.SnapshotRequest
	if err := ipc.DecodeParams(raw, &request); err != nil {
		return badParams(err)
	}
	if system == nil || system.Triage == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "triage inbox is not configured"}
	}
	snapshot, err := system.Triage.Snapshot(ctx, request)
	if err != nil {
		return failure(err)
	}
	return marshal(snapshot)
}

func triageCounts(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Triage == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "triage inbox is not configured"}
	}
	counts, err := system.Triage.Counts(ctx)
	if err != nil {
		return failure(err)
	}
	return marshal(counts)
}

// triageStats exposes the acquisition value read model the browser extension
// already reads over stats_request. Without it the CLI - and so the derived
// MCP surface - is the only interface that cannot see what papio's
// institutional access actually bought.
func triageStats(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Triage == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "triage inbox is not configured"}
	}
	stats, err := system.Triage.Stats(ctx)
	if err != nil {
		return failure(err)
	}
	return marshal(stats)
}

func triageDecide(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ItemID     string          `json:"item_id"`
		Op         string          `json:"op"`
		WatchScope json.RawMessage `json:"watch_scope,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ItemID == "" || (params.Op != "acquire" && params.Op != "dismiss") {
		if err == nil {
			err = errors.New("item_id and operation (acquire or dismiss) are required")
		}
		return badParams(err)
	}
	if system == nil || system.Triage == nil || system.WatchRunner == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "triage inbox is not configured"}
	}
	scope := triage.WatchScope{}
	if params.Op == string(triage.DecisionDismiss) {
		var err error
		scope, err = decodeTriageDismissScope(params.WatchScope)
		if err != nil {
			return badParams(err)
		}
	}
	result, err := system.Triage.Decide(ctx, triage.DecisionInput{
		ItemID: params.ItemID, Operation: triage.DecisionOperation(params.Op), Scope: scope,
	}, system.WatchRunner)
	if err != nil {
		var watchErr *triage.WatchMutationError
		if errors.As(err, &watchErr) {
			return watchFailure(err)
		}
		return failure(err)
	}
	if result.Outcome == triage.DecisionInvalid {
		return badParams(errors.New(result.Detail))
	}
	return marshal(triageDecideResult{Outcome: string(result.Outcome), Detail: result.Detail})
}

// decodeTriageDismissScope owns IPC JSON decoding. The triage service receives
// the normalized scope and validates it against the current hit.
func decodeTriageDismissScope(raw json.RawMessage) (triage.WatchScope, error) {
	if len(raw) == 0 {
		return triage.WatchScope{}, errors.New("watch_scope is required for dismiss")
	}
	var all string
	if err := json.Unmarshal(raw, &all); err == nil {
		if all != "all" {
			return triage.WatchScope{}, errors.New("watch_scope must be all or watch IDs")
		}
		return triage.WatchScope{All: true}, nil
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil || len(ids) == 0 || len(ids) > 100 {
		return triage.WatchScope{}, errors.New("watch_scope must be all or 1 to 100 watch IDs")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return triage.WatchScope{}, errors.New("watch_scope contains an invalid watch ID")
		}
		seen[id] = true
	}
	return triage.WatchScope{WatchIDs: ids}, nil
}

func resolveActionCAS(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ActionID         int64  `json:"action_id"`
		Verdict          string `json:"verdict"`
		ExpectedRevision *int64 `json:"expected_revision,omitempty"`
		ExpectedSHA256   string `json:"expected_sha256,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ActionID <= 0 || (params.Verdict != "accept" && params.Verdict != "reject") {
		if err == nil {
			err = errors.New("action_id and verdict (accept or reject) are required")
		}
		return badParams(err)
	}
	if system == nil || system.Jobs == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "jobs are not configured"}
	}
	if params.ExpectedRevision == nil {
		jobID, state, err := system.Jobs.ResolveReview(ctx, params.ActionID, params.Verdict)
		if err != nil {
			return failure(err)
		}
		if system.Captures != nil {
			if err := system.Captures.ReleaseJob(ctx, jobID); err != nil {
				return failure(err)
			}
		}
		if system.Preview != nil {
			system.Preview.Revoke(params.ActionID)
		}
		return marshal(map[string]any{"job_id": jobID, "state": state})
	}
	resolution, err := system.Jobs.ResolveReviewCAS(ctx, job.ResolveReviewInput{
		ActionID: params.ActionID, Verdict: params.Verdict,
		ExpectedRevision: *params.ExpectedRevision, ExpectedSHA256: params.ExpectedSHA256,
	})
	if err != nil {
		if errors.Is(err, job.ErrConflict) {
			return marshal(struct {
				Outcome string `json:"outcome"`
			}{Outcome: string(job.ReviewConflict)})
		}
		return badParams(err)
	}
	if system.Captures != nil && (resolution.Outcome == job.ReviewApplied || resolution.Outcome == job.ReviewAlreadyApplied) {
		if err := system.Captures.ReleaseJob(ctx, resolution.JobID); err != nil {
			return failure(err)
		}
	}
	if system.Preview != nil && (resolution.Outcome == job.ReviewApplied || resolution.Outcome == job.ReviewAlreadyApplied) {
		system.Preview.Revoke(params.ActionID)
	}
	return marshal(struct {
		Outcome string `json:"outcome"`
		JobID   string `json:"job_id,omitempty"`
		State   string `json:"state,omitempty"`
	}{Outcome: string(resolution.Outcome), JobID: resolution.JobID, State: resolution.State})
}
