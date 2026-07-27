// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"

	"papio/internal/job"
)

// HandoffRepairer heals awaiting_human jobs stranded by a crash between the
// browser bridge's non-transactional handoff mutations (requeue event, action
// resolution, state transition). It runs as bounded best-effort maintenance,
// including once at daemon startup.
type HandoffRepairer struct {
	svc *Service
}

// HandoffRepairer returns a maintenance runner that repairs stranded handoff
// parks. It satisfies daemon.MaintenanceRunner without importing that package.
func (s *Service) HandoffRepairer() *HandoffRepairer { return &HandoffRepairer{svc: s} }

// RunDue performs one repair pass over awaiting_human jobs.
//
// Rule 1 (orphaned park): a job in awaiting_human with no open human action
// can never be resolved by anyone — every legitimate park pairs with an open
// action. It is sent back to resolving for the scheduler to reclaim.
//
// Rule 2 (contradicted park): a job whose only open actions are institutional
// openurl_handoffs while a durable browser.no_entitlement_requeue event says
// that route already proved empty would re-offer a dead login loop. The
// actions are resolved and the job re-enters resolving, where exhaustion
// observes the event and parks it unavailable with terminal no_entitlement.
//
// Rule 3 (unfetchable park): a job whose only open actions are institutional
// handoffs but whose work carries no fetchable identifier is asking for a
// sign-in that cannot produce a PDF. Such parks predate the identifier gate in
// exhaustedCandidates and would otherwise sit in the queue forever, drawing an
// escalating reminder each time. Same shape as rule 2: resolve the actions and
// re-enter resolving, where the one gate that owns this decision classifies it.
//
// The transactional repair rejects a state/action snapshot that has gone
// stale, including an adoption lease acquired after its page read.
func (r *HandoffRepairer) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	s := r.svc
	rows, err := s.Jobs.ListOldest(ctx, []string{job.StateAwaitingHuman}, job.ListLimitMax)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(ctx, ids)
	if err != nil {
		return err
	}
	openByJob := make(map[string][]job.HumanAction, len(rows))
	for _, action := range actions {
		openByJob[action.JobID] = append(openByJob[action.JobID], action)
	}
	var firstErr error
	record := func(err error) {
		if err != nil && !errors.Is(err, job.ErrConflict) && firstErr == nil {
			firstErr = err
		}
	}
	for i := range rows {
		row := &rows[i]
		open := openByJob[row.ID]
		if len(open) == 0 {
			record(s.Jobs.RepairAwaitingHuman(ctx, row.ID, nil,
				map[string]any{"reason": "orphaned_handoff_repair"}))
			continue
		}
		if !allInstitutionalHandoffs(open) {
			continue
		}
		repair := "proven_empty_route_repair"
		switch {
		case !row.Work.HasFetchableIdentifier():
			repair = "unfetchable_handoff_repair"
		case !s.institutionalRouteExhausted(ctx, row.ID):
			continue
		}
		actionIDs := make([]int64, 0, len(open))
		for _, action := range open {
			actionIDs = append(actionIDs, action.ID)
		}
		record(s.Jobs.RepairAwaitingHuman(ctx, row.ID, actionIDs,
			map[string]any{"reason": repair}))
	}
	return firstErr
}

// allInstitutionalHandoffs reports whether every open action is an
// institutional OpenURL handoff. Any other open action (verify identity,
// manual download, an OA browser handoff, …) legitimately holds the park.
func allInstitutionalHandoffs(actions []job.HumanAction) bool {
	for _, action := range actions {
		if action.Kind != "openurl_handoff" || !action.RequiresAuth {
			return false
		}
	}
	return true
}
