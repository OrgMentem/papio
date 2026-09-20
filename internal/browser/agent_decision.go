// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"papio/internal/acquisitionagent"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

const (
	agentFallbackFeature = "agent_fallback_v1"
	agentDecisionLimit   = 60
	agentAttemptDuration = 10 * time.Minute
	agentRequestDuration = 30 * time.Second
)

// An in-memory reply mailbox, not acquisition authority. The existing durable
// effect permit authorizes the attempt; job events reserve its inference budget.
// A restart discards every decision, and a repeated durable request cannot spend
// again. No cloud credential or page text enters either storage tier.
type pendingAgentDecision struct {
	sessionID  string
	jobID      string
	generation int64
	permitID   string
	deadline   time.Time
	request    protocol.AgentDecideRequestV1Payload
	cancel     context.CancelFunc
	result     *protocol.AgentDecideResultV1Payload
}

// SetAcquisitionBackend is called during bootstrap, before serving browser IPC.
// A local backend implements the same interface without cloud credentials.
func (b *Bridge) SetAcquisitionBackend(backend acquisitionagent.Backend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, pending := range b.agentDecisions {
		pending.cancel()
	}
	b.agentDecisions = make(map[string]*pendingAgentDecision)
	b.agentBackend = backend
}

// CloseAcquisitionBackend fences completions before the owning System closes
// its database. Network cancellation cannot leave an asynchronous store writer.
func (b *Bridge) CloseAcquisitionBackend() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.agentClosed = true
	for _, pending := range b.agentDecisions {
		pending.cancel()
	}
	b.agentDecisions = nil
}

func agentResult(p *protocol.AgentDecideRequestV1Payload, outcome, detail string) protocol.AgentDecideResultV1Payload {
	return protocol.AgentDecideResultV1Payload{RequestID: p.RequestID, ObservationRevision: p.Observation.Revision, Outcome: outcome, Detail: detail}
}

func (b *Bridge) agentReply(jobID string, p protocol.AgentDecideResultV1Payload) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgAgentDecideResultV1, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// agentAuthority is always called under b.mu. It checks the existing permit,
// holder, current epoch, delegated handoff, safety domain, and work identity.
// Possession of a request ID or of a model answer grants none of these.
func (b *Bridge) agentAuthority(ctx context.Context, sessionID, jobID string, p *protocol.AgentDecideRequestV1Payload) (*job.EffectPermit, bool) {
	if b.agentClosed || b.agentBackend == nil || p == nil || p.Validate() != nil || p.Strategy != "generic" {
		return nil, false
	}
	holder := b.arbitration.holderSession()
	if holder == nil || holder.ID != sessionID || holder.Outdated || !slices.Contains(holder.Features, agentFallbackFeature) || !b.effectPermitAvailable() || !b.providerDriveEpochAvailable() {
		return nil, false
	}
	if b.now().Sub(holder.LastSyncAt) > sessionStaleAfter {
		return nil, false
	}
	permit, err := b.jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{JobID: jobID, Kind: job.EffectKindGenericDrive, DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal, Strategy: p.Strategy, Revision: p.Revision})
	if err != nil || permit == nil || permit.Status != job.EffectPermitHeld || permit.BrowserHolderGeneration != b.arbitration.generation() || permit.LeaseUntil == nil || !permit.LeaseUntil.After(b.now()) {
		return nil, false
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil || attempt != permit.JobAttemptRevision {
		return nil, false
	}
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return nil, false
	}
	doi, err := work.NormalizeDOI(row.Work.DOI)
	if err != nil || doi != p.Observation.DOI {
		return nil, false
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return nil, false
	}
	tuple := providerDriveEpochKey(p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision)
	current, started, applied, superseded, domain := providerDriveEpochState(events, tuple)
	if current != tuple || !started || applied || superseded || domain != permit.SafetyDomainID {
		return nil, false
	}
	authorized, err := b.providerDriveEpochAuthorized(ctx, jobID, domain)
	return permit, err == nil && authorized
}

// Session lifecycle calls this under b.mu, including for already completed
// replies. A browser that never polls again cannot occupy a decision slot.
func (b *Bridge) retireAgentDecisions(sessionID string) {
	for id, pending := range b.agentDecisions {
		if pending.sessionID == sessionID || pending.generation != b.arbitration.generation() {
			pending.cancel()
			delete(b.agentDecisions, id)
		}
	}
}

// Called by Sync under b.mu. It reserves paid work durably then returns at once;
// the ordinary native-host poll carries the eventual correlated response.
func (b *Bridge) agentDecide(ctx context.Context, sessionID, jobID string, p *protocol.AgentDecideRequestV1Payload) ([]json.RawMessage, error) {
	if b.agentBackend == nil || b.agentClosed {
		return b.agentReply(jobID, agentResult(p, "unavailable", "No acquisition decision backend is configured."))
	}
	permit, authorized := b.agentAuthority(ctx, sessionID, jobID, p)
	if !authorized {
		return b.agentReply(jobID, agentResult(p, "stale", "The acquisition attempt is no longer current."))
	}
	// A departed holder cannot consume a mailbox forever. Its durable call
	// reservation remains, but its response has no authority in this session.
	for oldJobID, pending := range b.agentDecisions {
		if pending.sessionID != sessionID || pending.generation != b.arbitration.generation() {
			pending.cancel()
			delete(b.agentDecisions, oldJobID)
		}
	}
	if pending := b.agentDecisions[jobID]; pending != nil {
		// An exact retransmission waits for the original response. Any other
		// concurrent observation must wait; it never starts a second call.
		a, _ := json.Marshal(pending.request)
		z, _ := json.Marshal(p)
		if pending.sessionID == sessionID && string(a) == string(z) {
			return nil, nil
		}
		return b.agentReply(jobID, agentResult(p, "stale", "Another decision is pending for this attempt."))
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return b.agentReply(jobID, agentResult(p, "unavailable", "The decision budget is unavailable."))
	}
	count := 0
	deadline := b.now().Add(agentAttemptDuration)
	for _, event := range events {
		if event["kind"] != "browser.agent_decision_requested" {
			continue
		}
		d, _ := event["detail"].(map[string]any)
		if stringDetail(d, "permit_id") != permit.ID {
			continue
		}
		count++
		if stringDetail(d, "request_id") == p.RequestID {
			return b.agentReply(jobID, agentResult(p, "stale", "This decision request was already consumed."))
		}
		firstDeadline, parseErr := time.Parse(time.RFC3339Nano, stringDetail(d, "deadline"))
		if parseErr != nil {
			return b.agentReply(jobID, agentResult(p, "exhausted", "The saved decision budget cannot be resumed."))
		}
		if firstDeadline.Before(deadline) {
			deadline = firstDeadline
		}
	}
	if count >= agentDecisionLimit || !b.now().Before(deadline) {
		return b.agentReply(jobID, agentResult(p, "exhausted", "The acquisition attempt reached its decision or time budget."))
	}
	if len(b.agentDecisions) >= maxOutstandingOffers {
		return b.agentReply(jobID, agentResult(p, "unavailable", "The decision backend is busy."))
	}
	err = b.jobs.S.AppendEvent(ctx, jobID, "browser.agent_decision_requested", map[string]any{
		"permit_id": permit.ID, "request_id": p.RequestID, "observation_revision": p.Observation.Revision,
		"decision_number": count + 1, "deadline": deadline.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return b.agentReply(jobID, agentResult(p, "unavailable", "The decision budget could not be reserved."))
	}
	requestContext, cancel := context.WithTimeout(context.Background(), min(agentRequestDuration, deadline.Sub(b.now())))
	pending := &pendingAgentDecision{sessionID: sessionID, jobID: jobID, generation: b.arbitration.generation(), permitID: permit.ID, deadline: deadline, request: *p, cancel: cancel}
	pending.request.Observation.Controls = slices.Clone(p.Observation.Controls)
	if b.agentDecisions == nil {
		b.agentDecisions = make(map[string]*pendingAgentDecision)
	}
	b.agentDecisions[jobID] = pending
	backend := b.agentBackend
	go b.runAgentDecision(requestContext, pending, backend)
	return nil, nil
}

func (b *Bridge) runAgentDecision(ctx context.Context, pending *pendingAgentDecision, backend acquisitionagent.Backend) {
	defer pending.cancel()
	finished := make(chan struct{})
	// Job cancellation can arrive through CLI/MCP, without a browser poll.
	// The short observer cancels inference; it never grants or settles effects.
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-finished:
				return
			case <-ticker.C:
				b.mu.Lock()
				_, current := b.agentAuthority(ctx, pending.sessionID, pending.jobID, &pending.request)
				current = current && b.agentDecisions[pending.jobID] == pending && pending.generation == b.arbitration.generation()
				b.mu.Unlock()
				if !current {
					pending.cancel()
					return
				}
			}
		}
	}()
	decision, err := backend.Decide(ctx, pending.request.Observation)
	close(finished)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.agentClosed || b.agentDecisions[pending.jobID] != pending {
		return
	}
	result := agentResult(&pending.request, "unavailable", "The decision backend did not return a usable answer.")
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, current := b.agentAuthority(checkCtx, pending.sessionID, pending.jobID, &pending.request)
	if !current || pending.generation != b.arbitration.generation() {
		result = agentResult(&pending.request, "stale", "The acquisition attempt changed while the decision was pending.")
	} else if !b.now().Before(pending.deadline) {
		result = agentResult(&pending.request, "exhausted", "The acquisition attempt reached its time budget.")
	} else if err == nil && ctx.Err() == nil && acquisitionagent.ValidateDecision(pending.request.Observation, decision) == nil {
		result = agentResult(&pending.request, "decision", "")
		result.Choice = decision.Choice
	}
	// Keep receipts useful for local repair and cost measurement without storing
	// the projection, raw model text, credentials, or a provider URL.
	detail := map[string]any{"permit_id": pending.permitID, "request_id": pending.request.RequestID, "observation_revision": pending.request.Observation.Revision, "outcome": result.Outcome}
	if err == nil {
		detail["input_tokens"], detail["output_tokens"] = decision.InputTokens, decision.OutputTokens
	}
	if result.Choice != "" {
		detail["choice"] = result.Choice
	}
	if b.jobs.S.AppendEvent(checkCtx, pending.jobID, "browser.agent_decision_completed", detail) != nil {
		result = agentResult(&pending.request, "unavailable", "The decision receipt could not be recorded.")
	}
	pending.result = &result
	if b.sessionByID(pending.sessionID) == nil {
		delete(b.agentDecisions, pending.jobID)
	}
}

func (b *Bridge) drainAgentDecisions(ctx context.Context, sessionID string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for jobID, pending := range b.agentDecisions {
		if pending.sessionID != sessionID || pending.result == nil {
			continue
		}
		result := *pending.result
		if result.Outcome == "decision" {
			_, current := b.agentAuthority(ctx, sessionID, jobID, &pending.request)
			if !current || pending.generation != b.arbitration.generation() {
				result = agentResult(&pending.request, "stale", "The acquisition attempt is no longer current.")
			} else if !b.now().Before(pending.deadline) {
				result = agentResult(&pending.request, "exhausted", "The acquisition attempt reached its time budget.")
			}
		}
		frames, err := b.agentReply(jobID, result)
		if err != nil {
			return nil, err
		}
		out = append(out, frames...)
		delete(b.agentDecisions, jobID)
	}
	return out, nil
}
