// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"papio/internal/bootstrap"
	"papio/internal/browser"
	"papio/internal/ipc"
)

type grabIdentifyParams struct {
	GrabID string `json:"grab_id"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

type GrabIdentifyResult = browser.GrabIdentifyResult

func identifyGrab(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params grabIdentifyParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.GrabID) == "" || strings.TrimSpace(params.Kind) == "" || strings.TrimSpace(params.Value) == "" {
		return badParams(errors.New("grab_id, kind, and value are required"))
	}
	if system == nil || system.Browser == nil {
		return marshal(GrabIdentifyResult{GrabID: params.GrabID, Outcome: "unavailable", Detail: "pdf grabs are not configured"})
	}
	return marshal(system.Browser.IdentifyGrab(ctx, params.GrabID, params.Kind, params.Value))
}
