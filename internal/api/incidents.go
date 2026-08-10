// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/incident"
	"papio/internal/ipc"
)

type incidentsParams struct {
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type incidentsResult struct {
	Incidents []incident.Group `json:"incidents"`
}

func listJobIncidents(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params incidentsParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.Limit < 0 {
		return badParams(errors.New("limit must not be negative"))
	}
	since, err := parseFailuresSince(params.Since, time.Now().UTC())
	if err != nil {
		return badParams(err)
	}
	rows, err := system.Jobs.IncidentFailures(ctx, since, params.Limit)
	if err != nil {
		return failure(err)
	}
	if rows == nil {
		rows = []incident.Group{}
	}
	return marshal(incidentsResult{Incidents: rows})
}
