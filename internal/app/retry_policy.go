// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"errors"
	"time"

	"papio/internal/budget"
	"papio/internal/job"
)

// retryKind labels why a pass ended without a verdict. It is recorded on the
// retry_wait transition so retryBudgetExhausted can tell the two apart.
const (
	retryKindTemporary  = "temporary"
	retryKindSourceGate = "source_gate"
	// retryKindAdvisory marks a pass that made no request because this
	// process's own token bucket turned every callable source away. Like a
	// closed source gate it consumes no attempt, but it stays distinct in the
	// durable log. Liveness comes from the retry-delay floor.
	retryKindAdvisory = "advisory"
	// retryKindExhaustedGate marks the single wait a job is allowed after its
	// retry budget is spent. It is not counted by retryBudgetExhausted.
	retryKindExhaustedGate = "exhausted_gate"
)

// retryPlan records observations from one acquisition pass. Its methods are
// the only way callers can describe retry facts. That keeps time precedence,
// chargeability, source gates, and cutover classification together.
type retryPlan struct {
	candidateTemporary   time.Time
	resolverTemporary    time.Time
	gate                 time.Time
	latestGate           time.Time
	advisory             time.Time
	openAccessCandidates int
	retryableCandidates  int
	temporaryResolvers   int
	closedSourceGates    int
	advisoryBackoffs     int
	stickyBudgetGate     bool
	sourcesCalled        int
}

func (p *retryPlan) observeOpenAccessCandidate() { p.openAccessCandidates++ }

func (p *retryPlan) observeSourceCalled() { p.sourcesCalled++ }

// observeResolverTemporary records a retryable resolver or sibling request and
// returns its wake time so the caller can defer the same budget identity.
func (p *retryPlan) observeResolverTemporary(now time.Time, delay, fallback time.Duration) time.Time {
	at := earlierRetry(time.Time{}, now, delay, fallback)
	p.resolverTemporary = earlierTime(p.resolverTemporary, at)
	p.temporaryResolvers++
	return at
}

// observeCandidateTemporary records a retryable candidate fetch and returns
// its wake time so the caller can defer the same budget identity.
func (p *retryPlan) observeCandidateTemporary(now time.Time, delay, fallback time.Duration) time.Time {
	at := earlierRetry(time.Time{}, now, delay, fallback)
	p.candidateTemporary = earlierTime(p.candidateTemporary, at)
	p.retryableCandidates++
	return at
}

// observeBudgetRefusal folds a budget admission refusal into the pass. It
// returns true only for a typed local refusal that the pass must park on.
func (p *retryPlan) observeBudgetRefusal(err error) bool {
	var exceeded *budget.ErrExceeded
	if errors.As(err, &exceeded) {
		p.recordExceeded(exceeded)
		return true
	}
	var deferred *budget.ErrDeferred
	if errors.As(err, &deferred) {
		p.recordDeferral(deferred)
		return true
	}
	return false
}

func (p *retryPlan) recordDeferral(deferred *budget.ErrDeferred) {
	if deferred == nil {
		return
	}
	if deferred.Advisory {
		p.advisory = earlierTime(p.advisory, deferred.Until)
		p.advisoryBackoffs++
		return
	}
	p.gate = earlierTime(p.gate, deferred.Until)
	p.latestGate = laterTime(p.latestGate, deferred.Until)
	p.closedSourceGates++
}

func (p *retryPlan) recordExceeded(exceeded *budget.ErrExceeded) {
	if exceeded == nil {
		return
	}
	if exceeded.Window == budget.WindowSticky {
		p.stickyBudgetGate = true
		p.closedSourceGates++
		return
	}
	until := exceeded.Until.UTC()
	if until.IsZero() {
		return
	}
	p.gate = earlierTime(p.gate, until)
	p.latestGate = laterTime(p.latestGate, until)
	p.closedSourceGates++
}

// merge folds another pass's observations in. It retains the earliest retry
// opportunity, the latest gate for the final exhaustion wait, and all counts.
func (p *retryPlan) merge(other retryPlan) {
	p.candidateTemporary = earlierTime(p.candidateTemporary, other.candidateTemporary)
	p.resolverTemporary = earlierTime(p.resolverTemporary, other.resolverTemporary)
	p.gate = earlierTime(p.gate, other.gate)
	p.latestGate = laterTime(p.latestGate, other.latestGate)
	p.advisory = earlierTime(p.advisory, other.advisory)
	p.openAccessCandidates += other.openAccessCandidates
	p.retryableCandidates += other.retryableCandidates
	p.temporaryResolvers += other.temporaryResolvers
	p.closedSourceGates += other.closedSourceGates
	p.advisoryBackoffs += other.advisoryBackoffs
	p.stickyBudgetGate = p.stickyBudgetGate || other.stickyBudgetGate
	p.sourcesCalled += other.sourcesCalled
}

func (p retryPlan) hasClosedSourceGate() bool { return p.closedSourceGates > 0 }

func (p retryPlan) atSiblingSearchBoundary(exhausted bool) bool {
	return p.temporary().IsZero() || exhausted || p.stickyBudgetGate
}

func (p retryPlan) empty() bool {
	return p.at().IsZero() && !p.stickyBudgetGate && p.advisoryBackoffs == 0
}

func (p retryPlan) temporary() time.Time {
	return earlierTime(p.candidateTemporary, p.resolverTemporary)
}

func (p retryPlan) at() time.Time { return earlierTime(p.temporary(), p.gate) }

func (p retryPlan) advisoryOnly() bool {
	return p.at().IsZero() && p.advisoryBackoffs > 0 && p.sourcesCalled == 0
}

func (p retryPlan) kind() string {
	if p.sourcesCalled > 0 || !p.temporary().IsZero() {
		return retryKindTemporary
	}
	if !p.gate.IsZero() || p.closedSourceGates > 0 {
		return retryKindSourceGate
	}
	if p.advisoryOnly() {
		return retryKindAdvisory
	}
	return retryKindTemporary
}

func (p retryPlan) gatePending(now time.Time) bool {
	return !p.latestGate.IsZero() && p.latestGate.After(now)
}

// retrySchedule is the policy result Service persists after its durable reads.
type retrySchedule struct {
	at        time.Time
	kind      string
	cutover   job.InstitutionCutoverDecision
	exhausted bool
}

// schedule applies retry-budget exhaustion after Service reads its durable
// history. It preserves the one final wait for the latest pending gate.
func (p retryPlan) schedule(now time.Time, retryDelay time.Duration, budgetExhausted, alreadyWaited bool) retrySchedule {
	at := p.at()
	kind := p.kind()
	if budgetExhausted {
		gatedOA := kind == retryKindSourceGate && p.openAccessCandidates > 0
		if gatedOA {
			at = p.latestGate
		} else if !p.gatePending(now) || alreadyWaited {
			return retrySchedule{exhausted: true}
		} else {
			at, kind = p.latestGate, retryKindExhaustedGate
		}
	}
	if at.IsZero() || !at.After(now) {
		at = now.Add(retryDelay)
	}
	cutover := p.cutoverDecision()
	if kind == retryKindExhaustedGate {
		cutover = job.InstitutionCutoverDecision{
			Blocker:                job.InstitutionCutoverBlockerSourceGateOnly,
			CanaryReadyRouteExists: false,
		}
	}
	return retrySchedule{at: at, kind: kind, cutover: cutover}
}

// cutoverDecision uses only current-pass observations. A pass that reached a
// source remains transient even if another source gate also appeared.
func (p retryPlan) cutoverDecision() job.InstitutionCutoverDecision {
	blocker := job.InstitutionCutoverBlockerNone
	switch {
	case !p.temporary().IsZero() || p.retryableCandidates > 0 || p.temporaryResolvers > 0 || p.sourcesCalled > 0:
		blocker = job.InstitutionCutoverBlockerTransientRetryRemaining
	case p.closedSourceGates > 0 || !p.gate.IsZero():
		blocker = job.InstitutionCutoverBlockerSourceGateOnly
	case p.advisoryOnly():
		blocker = job.InstitutionCutoverBlockerTransientRetryRemaining
	}
	return job.InstitutionCutoverDecision{
		Blocker:                blocker,
		CanaryReadyRouteExists: false,
	}
}

func (p retryPlan) fetchRetryDetail() map[string]any {
	return map[string]any{
		"reason":               "acquisition_inputs_temporarily_unavailable",
		"retryable_candidates": p.retryableCandidates,
		"temporary_resolvers":  p.temporaryResolvers,
		"closed_source_gates":  p.closedSourceGates,
	}
}

func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || (!candidate.IsZero() && candidate.Before(current)) {
		return candidate
	}
	return current
}

func laterTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.After(current) {
		return candidate
	}
	return current
}

func earlierRetry(current time.Time, now time.Time, delay, fallback time.Duration) time.Time {
	if delay <= 0 {
		delay = fallback
	}
	candidate := now.UTC().Add(delay)
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}
