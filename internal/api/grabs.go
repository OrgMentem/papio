// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"papio/internal/agentjson"
	"papio/internal/bootstrap"
	"papio/internal/browser"
	"papio/internal/grab"
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

// grabsBindsDefaultLimit and grabsBindsMaxLimit bound grabs.binds the same
// way jobs.list_v2 bounds its own page: unspecified gets a usable default,
// and an oversized request clamps down to the maximum rather than resetting
// to it, so a caller that asks for more never sees fewer rows than one that
// asked for less.
const (
	grabsBindsDefaultLimit = 50
	grabsBindsMaxLimit     = 200
)

type grabsBindsParams struct {
	Limit int `json:"limit,omitempty"`
}

// GrabBindRow is one grabs.binds row: the grab, the job an automatic bind
// filed it under, when that commit happened, and the full BindProvenance
// that justified the decision, carried verbatim so an operator never has to
// take the daemon's summary of its own reasoning on faith.
type GrabBindRow struct {
	GrabID     string              `json:"grab_id"`
	JobID      string              `json:"job_id"`
	BoundAt    time.Time           `json:"bound_at"`
	Provenance grab.BindProvenance `json:"provenance"`
}

// listAutonomousBinds serves grabs.binds, the read-only audit of every
// candidate_auto_bind decision the daemon has filed on its own. It exists
// because autoBindDecisionEnabled files jobs with nobody in the loop and
// there is no unbind command to reverse one — this listing, and rereading
// the provenance it carries, is the operator's only recourse after the
// fact.
//
// One extra row is requested from the store beyond the effective limit so
// truncation can be reported honestly rather than guessed; see
// grab.Service.ListAutonomousBinds.
func listAutonomousBinds(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params grabsBindsParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	limit := params.Limit
	switch {
	case limit <= 0:
		limit = grabsBindsDefaultLimit
	case limit > grabsBindsMaxLimit:
		limit = grabsBindsMaxLimit
	}
	if system == nil || system.Browser == nil {
		return marshal(agentjson.Envelope("binds", []GrabBindRow{}, false))
	}
	records, err := system.Browser.AutonomousBinds(ctx, limit+1)
	if err != nil {
		return failure(err)
	}
	truncated := false
	if len(records) > limit {
		records, truncated = records[:limit], true
	}
	rows := make([]GrabBindRow, len(records))
	for i, r := range records {
		rows[i] = GrabBindRow{GrabID: r.GrabID, JobID: r.JobID, BoundAt: r.BoundAt, Provenance: r.Provenance}
	}
	return marshal(agentjson.Envelope("binds", rows, truncated))
}
