// Copyright 2026 OrgMentem. Licensed under MIT.

package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/protocol"
)

// PulseResult is the purpose-built work.pulse_v1 result. It is intentionally a
// new result type rather than an extension of an existing stats or triage
// response.
type PulseResult = protocol.WorkPulseResponsePayload

func workPulse(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params protocol.WorkPulseRequestPayload
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.RequestID == "" || len(params.RequestID) > 64 || len(params.SchemaVersions) != 1 || params.SchemaVersions[0] != 1 {
		return badParams(errors.New("work pulse request must specify request_id and schema_versions [1]"))
	}
	if system == nil || system.Pulse == nil {
		return nil, &ipc.RPCError{Code: "unavailable", Message: "work pulse is unavailable with this daemon"}
	}
	snapshot, err := system.Pulse.Read(ctx)
	if err != nil {
		return failure(err)
	}
	snapshot.RequestID = params.RequestID
	return marshal(PulseResult(snapshot))
}
