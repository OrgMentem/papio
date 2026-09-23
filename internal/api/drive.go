// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"

	"papio/internal/bootstrap"
	"papio/internal/drive"
	"papio/internal/ipc"
)

// driveStatus, drivePause and driveResume serve `papio drive`. They are new
// method names with a result of their own (drive.Status), so no existing
// result widens.
func driveStatus(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return driveCall(ctx, raw, system, (*drive.Pacer).Status)
}

func drivePause(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return driveCall(ctx, raw, system, (*drive.Pacer).Pause)
}

func driveResume(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return driveCall(ctx, raw, system, (*drive.Pacer).Resume)
}

func driveCall(ctx context.Context, raw json.RawMessage, system *bootstrap.System, call func(*drive.Pacer, context.Context) (drive.Status, error)) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Drive == nil {
		return nil, &ipc.RPCError{Code: "unavailable", Message: "the paced drive is unavailable with this daemon"}
	}
	status, err := call(system.Drive, ctx)
	if err != nil {
		return failure(err)
	}
	return marshal(status)
}
