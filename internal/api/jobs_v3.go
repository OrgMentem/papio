// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"

	"papio/internal/bootstrap"
	"papio/internal/delivery"
	"papio/internal/ipc"
)

// DeliverySummary is jobs.get_v3's redaction-safe view of one job's
// delivery_requests row plus its latest gate verdict (ADR-0017 Decision
// 1/3B/5). It never carries patron_ref, api_key, or any other configured
// secret — those stay daemon-side, as Decision 2 requires.
type DeliverySummary struct {
	Provider      string `json:"provider"`
	Reference     string `json:"reference,omitempty"`
	State         string `json:"state"`
	SubmittedAt   string `json:"submitted_at,omitempty"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	NextCheckAt   string `json:"next_check_at,omitempty"`
	// GateClass and GateBlockers describe the LATEST recorded gate
	// evaluation, not a live recompute — see Service.LatestGateEvent. Both
	// are empty when this job was gated before any decision was recorded.
	GateClass    string   `json:"gate_class,omitempty"`
	GateBlockers []string `json:"gate_blockers,omitempty"`
}

// JobDetailV3 is jobs.get_v2's result plus a delivery section. It restates
// JobDetailV2's fields verbatim (rather than embedding it) so a reader that
// already parses JobDetailV2 parses this too and finds Delivery where it
// expects it — the same convention JobDetailV2 itself follows over
// JobDetail.
type JobDetailV3 struct {
	Job     *JobRow          `json:"job"`
	Events  []map[string]any `json:"events"`
	Actions []ActionRow      `json:"actions"`
	// Delivery is present only when this job has a delivery_requests row
	// (ADR-0017 Decision 5); absent for every job that never routed through
	// document delivery, which is most of them.
	Delivery *DeliverySummary `json:"delivery,omitempty"`
}

// getJobV3 is jobs.get_v2 plus a delivery section. New method, not a widened
// jobs.get_v2, for the reason jobs.get_v2 was new over the original jobs.get
// (ADR-0014 Decision 3): jobs.get_v2 is ratified and closed, and internal/ipc
// decodes every result with DisallowUnknownFields, so widening it would make
// an already-installed papio reject a newer daemon's reply.
func getJobV3(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	jobRow, events, actions, err := jobDetailParts(ctx, system, jobID)
	if err != nil {
		return failure(err)
	}
	detail := JobDetailV3{Job: jobRow, Events: events, Actions: actions}
	if system.App != nil && system.App.Delivery != nil {
		summary, err := jobDeliverySummary(ctx, system.App.Delivery, jobID)
		if err != nil {
			return failure(err)
		}
		detail.Delivery = summary
	}
	return marshal(detail)
}

// jobDeliverySummary loads jobID's delivery_requests row and its latest gate
// verdict, or returns (nil, nil) when the job was never routed through
// document delivery at all.
func jobDeliverySummary(ctx context.Context, svc *delivery.Service, jobID string) (*DeliverySummary, error) {
	request, err := svc.GetByJobID(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, nil
	}
	summary := &DeliverySummary{
		Provider:      request.Provider,
		Reference:     request.ProviderReference,
		State:         string(request.State),
		SubmittedAt:   request.SubmittedAt,
		LastCheckedAt: request.LastCheckedAt,
		NextCheckAt:   request.NextCheckAt,
	}
	verdict, err := svc.LatestGateEvent(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if verdict != nil {
		summary.GateClass = string(verdict.ProfileClass)
		summary.GateBlockers = verdict.Decision.Blockers
	}
	return summary, nil
}
