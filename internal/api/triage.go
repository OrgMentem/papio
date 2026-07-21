// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/triage"
	"papio/internal/watch"
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
	hit, err := system.Triage.FindWatchHit(ctx, params.ItemID)
	if errors.Is(err, sql.ErrNoRows) {
		return marshal(triageDecideResult{Outcome: "conflict"})
	}
	if err != nil {
		return failure(err)
	}
	if params.Op == "acquire" {
		for _, watched := range hit.Watches {
			if _, err := system.WatchRunner.AcquireDigest(ctx, watched.ID, []string{watched.WorkKey}); err != nil {
				if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
					return marshal(triageDecideResult{Outcome: "conflict"})
				}
				return watchFailure(err)
			}
		}
		return marshal(triageDecideResult{Outcome: "applied"})
	}

	watchIDs, err := triageDismissScope(params.WatchScope, hit.Watches)
	if err != nil {
		return badParams(err)
	}
	for _, watched := range hit.Watches {
		if !watchIDs[watched.ID] {
			continue
		}
		if _, err := system.WatchRunner.ConsumeDigest(ctx, watched.ID, []string{watched.WorkKey}); err != nil {
			if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
				return marshal(triageDecideResult{Outcome: "conflict"})
			}
			return watchFailure(err)
		}
	}
	return marshal(triageDecideResult{Outcome: "applied"})
}

func triageDismissScope(raw json.RawMessage, watches []triage.Watch) (map[int64]bool, error) {
	if len(raw) == 0 {
		return nil, errors.New("watch_scope is required for dismiss")
	}
	var all string
	if err := json.Unmarshal(raw, &all); err == nil {
		if all != "all" {
			return nil, errors.New("watch_scope must be all or watch IDs")
		}
		selected := make(map[int64]bool, len(watches))
		for _, watched := range watches {
			selected[watched.ID] = true
		}
		return selected, nil
	}
	var ids []int64
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&ids); err != nil || len(ids) == 0 || len(ids) > 100 {
		return nil, errors.New("watch_scope must be all or 1 to 100 watch IDs")
	}
	available := make(map[int64]bool, len(watches))
	for _, watched := range watches {
		available[watched.ID] = true
	}
	selected := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || !available[id] || selected[id] {
			return nil, errors.New("watch_scope contains an invalid watch ID")
		}
		selected[id] = true
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("watch_scope must be all or 1 to 100 watch IDs")
	}
	return selected, nil
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
	if system.Preview != nil && (resolution.Outcome == job.ReviewApplied || resolution.Outcome == job.ReviewAlreadyApplied) {
		system.Preview.Revoke(params.ActionID)
	}
	return marshal(struct {
		Outcome string `json:"outcome"`
		JobID   string `json:"job_id,omitempty"`
		State   string `json:"state,omitempty"`
	}{Outcome: string(resolution.Outcome), JobID: resolution.JobID, State: resolution.State})
}
