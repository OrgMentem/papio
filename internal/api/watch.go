// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/publicationwatch"
	"papio/internal/watch"
)

func editWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.EditInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	updated, err := system.Watches.Edit(ctx, input)
	if err != nil {
		return failure(err)
	}
	return marshal(updated)
}

func addGenericBackfill(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.GenericBackfillInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if system == nil || system.WatchRunner == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	created, err := system.WatchRunner.AddGenericBackfill(ctx, input)
	if err != nil {
		return failure(err)
	}
	return marshal(created)
}

func addPublicationWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input publicationwatch.AddInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	result, err := system.PublicationWatches.Add(ctx, input)
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}

func listPublicationWatches(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input struct{}
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	result, err := system.PublicationWatches.List(ctx)
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}

func publicationNotices(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.IDInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	result, err := system.PublicationWatches.Notices(ctx, input.ID)
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}

func runPublicationWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.IDInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	result, err := system.PublicationWatches.Run(ctx, input.ID)
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}

func pausePublicationWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.IDInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	if err := system.PublicationWatches.Pause(ctx, input.ID); err != nil {
		return failure(err)
	}
	return marshal(map[string]any{"id": input.ID, "paused": true})
}

func acquirePublicationNotice(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input struct {
		NoticeID int64 `json:"notice_id"`
	}
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if err := publicationConfigured(system); err != nil {
		return nil, err
	}
	result, err := system.PublicationWatches.Acquire(ctx, input.NoticeID)
	if err != nil {
		return failure(err)
	}
	return marshal(result)
}

func publicationConfigured(system *bootstrap.System) *ipc.RPCError {
	if system == nil || system.PublicationWatches == nil {
		return &ipc.RPCError{Code: "precondition_failed", Message: "publication watches are not configured"}
	}
	return nil
}
