// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"time"

	"papio/internal/job"
)

// strandedNeedsReviewMinAge leaves five normal maintenance passes for a
// legitimate action-resolution/state transition to settle before maintenance
// treats its empty-action gap as durable.
const strandedNeedsReviewMinAge = 5 * time.Minute

const adapterUpgradeRepairReason = "adapter_upgrade_repair"

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

// RunDue performs one repair pass over human-park jobs.
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
// handoffs but whose work carries nothing a sign-in could act on is asking for
// a login that cannot produce a PDF. That means no fetchable identifier at
// all, and equally a lone DOI the registry has never heard of — the reported
// incident was a one-digit typo, whose park rule 2 can never reach, because
// browser.no_entitlement_requeue requires the browser to have got as far as
// the institutional resolver and a dead DOI never does. Such parks predate the
// gate in exhaustedCandidates and would otherwise sit in the queue forever,
// drawing an escalating reminder each time. Same shape as rule 2: resolve the
// actions and re-enter resolving, where the one gate that owns this decision
// classifies it. handoffGate memoizes, so sweeping the same parked jobs every
// maintenance tick does not become a registry request per job per minute.
//
// Rule 4 (unactionable review): an old needs_review job with no open action
// cannot be approved, rejected, or retried. The nine observed strands each
// recorded job.transition {"from":"awaiting_human","reason":"ui_changed",
// "to":"needs_review"} after their openurl_handoff had resolved, leaving no
// artifact or action. Move it to awaiting_human with manual_download so the
// user can supply a browser download for adoption.
//
// Rule 5 (provider upgrade): a manual-download park created by an older
// provider adapter can retry when the live browser registry proves that exact
// adapter has been upgraded. Captures bind the outcome to its adapter id; old
// events without that evidence conservatively fall back to a newer extension
// bundle. The bridge supplies both live-only signals to RepairAdapterUpgrade.
//
// The transactional repair rejects a state/action snapshot that has gone
// stale, including an adoption lease acquired after its page read.
func (r *HandoffRepairer) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	s := r.svc
	rows, err := s.Jobs.ListOldest(ctx, []string{job.StateAwaitingHuman, job.StateNeedsReview}, job.ListLimitMax)
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
	now := r.now()
	var firstErr error
	record := func(err error) {
		if err != nil && !errors.Is(err, job.ErrConflict) && firstErr == nil {
			firstErr = err
		}
	}
	for i := range rows {
		row := &rows[i]
		open := openByJob[row.ID]
		if row.State == job.StateNeedsReview {
			if len(open) != 0 || !strandedNeedsReview(row, now) {
				continue
			}
			// A page papio could not drive is the immediate blocker, but only the
			// resolved handoff can tell us whether its institutional login remains needed.
			record(s.Jobs.RepairParkWithAction(ctx, row.ID, job.StateNeedsReview, job.StateAwaitingHuman, nil,
				"manual_download", "the browser handoff did not produce a file; download the requested PDF yourself and papio will adopt it",
				map[string]any{"reason": "stranded_handoff_repair"},
				job.AccessInheritedFromResolvedHandoff("landing_page"),
				// The handoff simply produced nothing; no provider-specific
				// fault was observed, so this is the plain manual download.
				job.WithHumanActionDiagnosis(job.DiagnosisReasonInstitutionalHandoff)))
			continue
		}
		if len(open) == 0 {
			record(s.Jobs.RepairAwaitingHuman(ctx, row.ID, nil,
				map[string]any{"reason": "orphaned_handoff_repair"}))
			continue
		}
		if !allInstitutionalHandoffs(open) {
			continue
		}
		// After allInstitutionalHandoffs, so the registry is never probed for a
		// park this rule would skip anyway.
		routeable, _, _ := s.handoffGate(ctx, row.Work, row.Policy.Resolver)
		repair := "proven_empty_route_repair"
		switch {
		case !routeable:
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

// RepairAdapterUpgrade returns manual-download parks to resolving when a live
// browser session proves that the adapter which stranded them is newer. A
// captured adapter id scopes the comparison to that registry entry, so changing
// an unrelated adapter cannot churn the park. Events predating adapter-id
// capture fall back to the extension bundle version.
//
// The bridge owns version comparison because app must not depend on the browser
// package; newer must decline malformed versions rather than guessing. The
// transition event is both the audit record and the durable one-shot latch: a
// re-park without a fresh provider outcome cannot loop on the same upgrade.
func (r *HandoffRepairer) RepairAdapterUpgrade(
	ctx context.Context,
	liveExtensionVersion string,
	liveAdapterVersions map[string]string,
	newer func(previous, current string) bool,
) error {
	if r == nil || r.svc == nil || r.svc.Jobs == nil || liveExtensionVersion == "" || newer == nil {
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
		if !allManualDownloads(open) {
			continue
		}
		events, err := s.Jobs.Events(ctx, row.ID)
		if err != nil {
			record(err)
			continue
		}
		previousExtensionVersion, adapterID, previousAdapterVersion, adapterOutcome :=
			providerAdapterUpgradeSource(events)
		if !adapterOutcome || providerRouteProvenEmpty(events) {
			continue
		}

		liveAdapterVersion := ""
		upgradeProven := false
		if adapterID != "" {
			liveAdapterVersion = liveAdapterVersions[adapterID]
			upgradeProven = newer(previousAdapterVersion, liveAdapterVersion)
		} else {
			upgradeProven = newer(previousExtensionVersion, liveExtensionVersion)
		}
		if !upgradeProven ||
			adapterUpgradeAlreadyRepaired(
				events,
				previousExtensionVersion,
				liveExtensionVersion,
				adapterID,
				previousAdapterVersion,
				liveAdapterVersion,
			) {
			continue
		}

		actionIDs := make([]int64, 0, len(open))
		for _, action := range open {
			actionIDs = append(actionIDs, action.ID)
		}
		detail := map[string]any{
			"reason":                adapterUpgradeRepairReason,
			"adapter_version":       previousAdapterVersion,
			"old_extension_version": previousExtensionVersion,
			"new_extension_version": liveExtensionVersion,
		}
		if adapterID != "" {
			detail["adapter_id"] = adapterID
			detail["old_adapter_version"] = previousAdapterVersion
			detail["new_adapter_version"] = liveAdapterVersion
		}
		record(s.Jobs.RepairAwaitingHuman(ctx, row.ID, actionIDs, detail))
	}
	return firstErr
}

// allManualDownloads excludes every other human decision from an automated
// retry: only a provider-driven manual download is invalidated by an upgrade.
func allManualDownloads(actions []job.HumanAction) bool {
	if len(actions) == 0 {
		return false
	}
	for _, action := range actions {
		if action.Kind != "manual_download" {
			return false
		}
	}
	return true
}

// providerAdapterUpgradeSource reads the latest provider outcome. Current
// frames identify their adapter directly; historical events fall back to the
// nearest preceding capture from the same adapter version. A capture never
// crosses an earlier provider outcome.
func providerAdapterUpgradeSource(events []map[string]any) (
	extensionVersion, adapterID, adapterVersion string,
	ok bool,
) {
	outcomeIndex := -1
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if kind, _ := event["kind"].(string); kind != "browser.provider_outcome" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		extensionVersion, _ = detail["extension_version"].(string)
		adapterVersion, _ = detail["adapter_version"].(string)
		adapterID, _ = detail["adapter_id"].(string)
		outcomeIndex = i
		break
	}
	if extensionVersion == "" || adapterVersion == "" {
		return extensionVersion, "", adapterVersion, false
	}
	if adapterID != "" {
		return extensionVersion, adapterID, adapterVersion, true
	}
	for i := outcomeIndex - 1; i >= 0; i-- {
		event := events[i]
		kind, _ := event["kind"].(string)
		if kind == "browser.provider_outcome" {
			break
		}
		if kind != "browser.page_capture" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		capturedVersion, _ := detail["adapter_version"].(string)
		capturedID, _ := detail["adapter_id"].(string)
		if capturedID != "" && capturedVersion == adapterVersion {
			return extensionVersion, capturedID, adapterVersion, true
		}
	}
	return extensionVersion, "", adapterVersion, true
}

func providerRouteProvenEmpty(events []map[string]any) bool {
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind == "browser.no_entitlement_requeue" {
			return true
		}
	}
	return false
}

func adapterUpgradeAlreadyRepaired(
	events []map[string]any,
	previousExtensionVersion, liveExtensionVersion, adapterID, previousAdapterVersion, liveAdapterVersion string,
) bool {
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "job.transition" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if reason, _ := detail["reason"].(string); reason != adapterUpgradeRepairReason {
			continue
		}
		if adapterID != "" {
			previous, _ := detail["old_adapter_version"].(string)
			current, _ := detail["new_adapter_version"].(string)
			capturedID, _ := detail["adapter_id"].(string)
			if capturedID == adapterID && previous == previousAdapterVersion && current == liveAdapterVersion {
				return true
			}
			continue
		}
		previous, _ := detail["old_extension_version"].(string)
		current, _ := detail["new_extension_version"].(string)
		if previous == previousExtensionVersion && current == liveExtensionVersion {
			return true
		}
	}
	return false
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

// strandedNeedsReview declines malformed timestamps because their age cannot
// be established safely.
func strandedNeedsReview(row *job.Row, now time.Time) bool {
	updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	return err == nil && !updated.Add(strandedNeedsReviewMinAge).After(now)
}

func (r *HandoffRepairer) now() time.Time {
	if r != nil && r.svc != nil && r.svc.Now != nil {
		return r.svc.Now()
	}
	return time.Now()
}
