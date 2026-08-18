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

// GrabSuggestResult, GrabSuggestionRow, and DocumentIdentifier are aliased
// from internal/browser the same way GrabIdentifyResult is above: Bridge is
// the only holder of the grab service and job store, so it owns the wire
// shape, and aliasing here lets the CLI decode api.GrabSuggestResult without
// a second declaration that could drift from the one the daemon encodes.
type GrabSuggestResult = browser.GrabSuggestResult
type GrabSuggestionRow = browser.GrabSuggestionRow
type DocumentIdentifier = browser.DocumentIdentifier

type grabsSuggestParams struct {
	GrabID string `json:"grab_id"`
	// Limit <= 0 means the daemon's own default (5); values above its hard
	// cap (25) clamp rather than error, same contract as grabsBindsParams
	// above and enforced by Bridge.SuggestGrabCandidates itself, since the
	// ranking and the bound it fits inside must never disagree.
	Limit int `json:"limit,omitempty"`
}

// listGrabSuggestions serves grabs.suggest, the read-only "which pending job
// is this?" ranking for a parked DOI-less grab. It is intentionally as thin
// as identifyGrab above: parameter validation and the unconfigured-daemon
// short-circuit live here, every other outcome (unknown grab, wrong state,
// re-validation failure, the ranking itself) is Bridge.SuggestGrabCandidates'
// job, because that is the one place production auto-bind's decision inputs
// are already built correctly and must not be rebuilt a second way.
func listGrabSuggestions(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params grabsSuggestParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.GrabID) == "" {
		return badParams(errors.New("grab_id is required"))
	}
	if system == nil || system.Browser == nil {
		return marshal(GrabSuggestResult{GrabID: params.GrabID, Outcome: "unavailable", Detail: "pdf grabs are not configured"})
	}
	return marshal(system.Browser.SuggestGrabCandidates(ctx, params.GrabID, params.Limit))
}

// GrabConfirmResult is aliased from internal/browser the same way
// GrabIdentifyResult and GrabSuggestResult are above: Bridge owns the wire
// shape because it is the only holder of the grab service and job store.
type GrabConfirmResult = browser.GrabConfirmResult

type grabConfirmParams struct {
	GrabID string `json:"grab_id"`
	JobID  string `json:"job_id"`
}

// confirmGrabCandidate serves grabs.confirm, the write counterpart to
// grabs.suggest: a human has picked one ranked candidate and this binds the
// parked grab to it. As thin as identifyGrab and listGrabSuggestions above —
// parameter validation and the unconfigured-daemon short-circuit live here,
// every other outcome (unknown grab/job, wrong state, the identity veto, the
// fenced bind itself) is Bridge.ConfirmGrabCandidate's job, so the decision
// stays built from the one place production auto-bind's decision inputs are
// already correct.
func confirmGrabCandidate(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params grabConfirmParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.GrabID) == "" || strings.TrimSpace(params.JobID) == "" {
		return badParams(errors.New("grab_id and job_id are required"))
	}
	if system == nil || system.Browser == nil {
		return marshal(GrabConfirmResult{GrabID: params.GrabID, JobID: params.JobID, Outcome: "unavailable", Detail: "pdf grabs are not configured"})
	}
	return marshal(system.Browser.ConfirmGrabCandidate(ctx, params.GrabID, params.JobID))
}
