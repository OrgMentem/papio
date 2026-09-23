// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package drive owns the one server-side path that puts a parked handoff on
// the operator's screen, and the paced drive that walks the backlog through it.
//
// `papio actions open` (the actions.open IPC method) and the pacer both call
// OpenHandoffs, so an unattended open is indistinguishable from an operator's
// except for the principal it records. A second open path would be a second
// place to forget the audit event ADR-0014 Decision 6 relies on.
package drive

import (
	"context"

	"papio/internal/job"
)

// HandoffOpenedEvent records that a human handoff was driven onto the
// operator's screen, with the owning consumer, the batch size and the
// principal that asked. It is the audit trail ADR-0014 Decision 6 relies on
// instead of a gate: "consumer X opened N human actions in M minutes" is
// answerable from the event stream, and a rate limit would have obstructed the
// operator it was meant to protect.
const HandoffOpenedEvent = "handoff.opened"

// Focuser surfaces tracked handoffs in the holder browser. *browser.Bridge
// satisfies it; the interface keeps this package below internal/browser.
type Focuser interface {
	FocusHandoffs(ctx context.Context, jobIDs []string) (queued int, sessionLive bool, err error)
}

// OpenHandoffs queues the named jobs' handoffs in the holder browser and
// records who asked. The browser's handoff surface settings (work window, tab
// group) decide where the tabs land; nothing here opens a URL itself.
//
// The audit event is written whenever the focus call succeeds, including when
// no session is live, exactly as actions.open always has.
func OpenHandoffs(ctx context.Context, focus Focuser, jobs *job.Store, jobIDs []string, principal job.Principal) (queued int, sessionLive bool, err error) {
	queued, sessionLive, err = focus.FocusHandoffs(ctx, jobIDs)
	if err != nil {
		return queued, sessionLive, err
	}
	RecordHandoffOpened(ctx, jobs, jobIDs, principal)
	return queued, sessionLive, nil
}

// RecordHandoffOpened leaves an auditable trace of who drove a human handoff
// onto the operator's screen.
//
// ADR-0014 Decision 6 declines to enforce ADR-0009's drain rule with a gate or
// an --operator-intent flag — scripts pass flags, and an agent driving the CLI
// is meant to get exactly what a human gets — so the rule rests on being
// auditable instead. The paced drive the operator authorized on 2026-09-23
// records principal "pacer" here, so its opens stay separable from a person's.
//
// It records the handoff's OWNER, not a self-declared opener. The owner was
// recorded at submit and is a fact papio holds; an opener label would be an
// unverifiable string supplied by the very caller under audit, and carrying it
// would mean a new param on a ratified method. A consumer looping its own ranked
// queue is opening its own jobs, so the burst is attributable either way — and
// when an operator opens someone else's handoff, naming the owner is still a true
// statement.
//
// A failed write is dropped: the tabs are already open, and losing an audit line
// must not turn a completed handoff into a reported error.
func RecordHandoffOpened(ctx context.Context, jobs *job.Store, jobIDs []string, principal job.Principal) {
	if jobs == nil || len(jobIDs) == 0 {
		return
	}
	consumers, err := jobs.ConsumersFor(ctx, jobIDs)
	if err != nil {
		return
	}
	for _, jobID := range jobIDs {
		detail := map[string]any{
			"principal": string(principal),
			// batch_size is what separates one deliberate selector call from a
			// loop of them: the drain pattern is many single-job opens in quick
			// succession, which looks nothing like an operator opening a queue.
			"batch_size": len(jobIDs),
		}
		if consumer, ok := consumers[jobID]; ok {
			detail["consumer"] = consumer
		}
		_ = jobs.RecordEvent(ctx, jobID, HandoffOpenedEvent, detail)
	}
}
