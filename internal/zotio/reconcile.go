// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"papio/internal/job"
)

// Ambiguous applies.
//
// An apply whose outcome papio did not see - a timeout, a daemon shutdown, a
// signal kill, unreadable output, or a claim that outlived its lease - may
// have changed Zotero. The exports row for it reads "ambiguous", the plan
// stays, and nothing runs the mutation again blind: a second connector save
// puts a duplicate paper in the library, and a second attachment add puts a
// duplicate PDF on the item.
//
// The row does not stay ambiguous forever, because a retry that only replays
// the stale error fails the job's whole import budget without ever asking
// Zotero. Each Apply of an ambiguous plan reconciles it first, against a
// freshly synced mirror:
//
//   - the library holds the paper with its PDF: the apply is recorded as a
//     duplicate and the job imports;
//   - a create saved the item but not its PDF: the item is recorded as the
//     job's committed item (committed.go) and the plan is dropped, so the next
//     plan attaches to that item instead of creating another;
//   - the library holds nothing, the mirror is fresh, and the settle window has
//     passed: the row reopens and the same plan runs once more. The window
//     outlasts the desktop-to-Web-API sync lag (webAPINotSyncedHint), so a save
//     that landed has reached the mirror by then; a connector plan also needs
//     Zotero desktop to answer (desktopEvidenceGap), since a mirror sync alone
//     does not show what the desktop saved;
//   - anything else, including a lookup that failed: the row stays ambiguous
//     and Apply reports zotio_apply_unconfirmed.

// ambiguousSettleWindow is how long an ambiguous apply may still be in flight
// on Zotero's side before an empty library is read as "it never landed".
const ambiguousSettleWindow = 2 * time.Minute

// interruptedApplyHint is the recorded cause of an apply whose claim expired
// without a result.
const interruptedApplyHint = "Zotio apply was interrupted before it recorded a result"

// errReplanRequired reports that reconciliation dropped a plan whose create
// had already saved the item; PlanAndApply plans once more, onto that item.
var errReplanRequired = errors.New("Zotio plan superseded by an earlier committed item; replan required")

// unconfirmedApplyError reports an apply that is still ambiguous. It carries
// its own class: an earlier timeout or cancel is not what the operator has to
// act on now.
func unconfirmedApplyError(planID string, recorded *ApplyResult, detail string) error {
	message := fmt.Sprintf("Zotio apply for plan %s is unconfirmed: an earlier attempt ended without a result", planID)
	if recorded != nil && recorded.Error != "" {
		message += " (" + recorded.Error + ")"
	}
	message += "; papio checks Zotero first and does not repeat the mutation until it can tell whether it landed"
	if detail != "" {
		message += ": " + detail
	}
	return &ClassifiedError{
		cause: errors.New(message),
		info:  safeErrorInfo(ErrorClassZotioApplyUnconfirmed, "Zotio apply outcome is unconfirmed; papio is checking Zotero before it retries", 0),
	}
}

// expireStaleClaim turns an in-progress claim that outlived its lease into an
// ambiguous apply. Its owner died or stalled somewhere between claiming and
// recording, so the mutation may have run; reclaiming the row for a blind
// re-run would repeat it.
func (s *Service) expireStaleClaim(ctx context.Context, key string, plan *Plan) error {
	now := s.now().UTC()
	// The mutation may have run any time after the claim, so the settle window
	// measures from the claim, not from this expiry pass: the lease alone
	// already outlasts the window.
	var claimedAt sql.NullInt64
	if err := s.Store.DB().QueryRowContext(ctx,
		`SELECT CAST(json_extract(result_json, '$.claimed_at') AS INTEGER) FROM exports
		 WHERE kind = 'zotio_apply' AND idempotency_key = ?
		   AND json_extract(result_json, '$.status') = 'in_progress'`, key).Scan(&claimedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	appliedAt := now
	if claimedAt.Valid && claimedAt.Int64 > 0 && claimedAt.Int64 < now.Unix() {
		appliedAt = time.Unix(claimedAt.Int64, 0).UTC()
	}
	raw, err := json.Marshal(&ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "ambiguous",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: appliedAt.Format(time.RFC3339),
		Error:     interruptedApplyHint,
	})
	if err != nil {
		return err
	}
	_, err = s.Store.DB().ExecContext(ctx,
		`UPDATE exports SET result_json = ?
		 WHERE kind = 'zotio_apply' AND idempotency_key = ?
		   AND json_extract(result_json, '$.status') = 'in_progress'
		   AND CAST(json_extract(result_json, '$.claimed_at') AS INTEGER) <= ?`,
		string(raw), key, now.Add(-applyClaimLease).Unix())
	return err
}

// replaceAmbiguousApply swaps the ambiguous record for result. Only the
// reconciler that read the ambiguous row may finish it: a concurrent
// reconciler or a reopened claim leaves nothing to replace.
func (s *Service) replaceAmbiguousApply(ctx context.Context, key string, result *ApplyResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	updated, err := s.Store.DB().ExecContext(ctx,
		`UPDATE exports SET result_json = ?
		 WHERE kind = 'zotio_apply' AND idempotency_key = ?
		   AND json_extract(result_json, '$.status') = 'ambiguous'`,
		string(raw), key)
	if err != nil {
		return err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return WithErrorInfo(fmt.Errorf("Zotio apply reservation was not finalized: %w", job.ErrConflict))
	}
	return nil
}

// settleAmbiguousApply reconciles an ambiguous apply against Zotero. It
// returns the apply's settled result, nil with a nil error when the row was
// reopened for one fresh run of the plan, or an error while the outcome is
// still unconfirmed.
func (s *Service) settleAmbiguousApply(ctx context.Context, plan *Plan, key string, recorded *ApplyResult) (*ApplyResult, error) {
	status, itemKey, fresh, err := s.planOwnership(ctx, plan)
	if err != nil {
		return nil, unconfirmedApplyError(plan.ID, recorded, "Zotero could not be read just now")
	}
	ledgerCtx := context.WithoutCancel(ctx)
	switch status {
	case OwnershipOwnedWithPDF:
		result := &ApplyResult{
			PlanID:    plan.ID,
			JobID:     plan.JobID,
			Status:    "duplicate",
			ParentKey: itemKey,
			AppliedAt: s.now().UTC().Format(time.RFC3339),
		}
		if err := s.replaceAmbiguousApply(ledgerCtx, key, result); err != nil {
			return nil, err
		}
		return result, nil
	case OwnershipOwnedMissingPDF:
		if plan.Route == "manifest_create" {
			return nil, s.supersedeCommittedCreate(ledgerCtx, key, plan, recorded, itemKey)
		}
	}
	if !fresh {
		return nil, unconfirmedApplyError(plan.ID, recorded, "the Zotio mirror could not be refreshed")
	}
	if recordedAt, err := time.Parse(time.RFC3339, recorded.AppliedAt); err == nil && s.now().Before(recordedAt.Add(ambiguousSettleWindow)) {
		return nil, unconfirmedApplyError(plan.ID, recorded, "Zotero may still be syncing the earlier attempt")
	}
	reopened := &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "failed",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Error:     "an earlier attempt left no trace in the refreshed Zotero library",
	}
	if planUsesConnector(plan) {
		if reason := s.desktopEvidenceGap(ctx); reason != "" {
			return nil, unconfirmedApplyError(plan.ID, recorded, reason)
		}
	}
	if err := s.replaceAmbiguousApply(ledgerCtx, key, reopened); err != nil {
		return nil, err
	}
	return nil, nil
}

// desktopStatusReader is the optional CLI capability that reports whether
// Zotero desktop runs and answers.
type desktopStatusReader interface {
	DesktopStatus(context.Context) (DesktopStatus, error)
}

// planUsesConnector reports whether the plan's mutation goes through Zotero
// desktop's connector, so that the desktop, not the Web API, is where a save
// that landed would first show.
func planUsesConnector(plan *Plan) bool {
	for i, arg := range plan.ApplyArgs {
		if arg == "--via" && i+1 < len(plan.ApplyArgs) && plan.ApplyArgs[i+1] == "connector" {
			return true
		}
	}
	return false
}

// desktopEvidenceGap returns why an empty mirror cannot yet prove that a
// connector save never landed, or "" when it can. A successful mirror sync
// proves nothing about the desktop: the desktop may be closed or may not have
// synced the item it saved. The mirror reads the desktop local API only
// while the desktop answers, so a desktop that answers now is the evidence;
// without it the apply stays ambiguous and a retry cannot duplicate the item.
func (s *Service) desktopEvidenceGap(ctx context.Context) string {
	reader, ok := s.CLI.(desktopStatusReader)
	if !ok {
		return "Zotero desktop cannot be asked whether it holds the earlier attempt"
	}
	status, err := reader.DesktopStatus(ctx)
	if err != nil {
		return "Zotero desktop cannot be asked whether it holds the earlier attempt"
	}
	if !status.Ready() {
		return "Zotero desktop is not answering, so the empty library does not show the earlier attempt"
	}
	return ""
}

// supersedeCommittedCreate records that an interrupted create saved the item
// but not its PDF, then retires the create plan. The committed record comes
// first: it is what keeps every later plan off the create route, so a crash
// between the steps still cannot duplicate the item.
func (s *Service) supersedeCommittedCreate(ctx context.Context, key string, plan *Plan, recorded *ApplyResult, itemKey string) error {
	detail := map[string]any{"status": importCommittedFound, "parent_key": itemKey}
	if err := s.Bundle.Jobs.RecordEvent(ctx, plan.JobID, importCommittedEvent, detail); err != nil {
		return unconfirmedApplyError(plan.ID, recorded, "recording the committed item failed")
	}
	retired := &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "failed",
		ParentKey: itemKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Error:     "an earlier attempt saved the item without its PDF",
	}
	if err := s.replaceAmbiguousApply(ctx, key, retired); err != nil {
		return err
	}
	if err := s.invalidatePlan(ctx, plan); err != nil {
		return err
	}
	return errReplanRequired
}

// planOwnership asks a freshly synced mirror what the plan's paper looks like
// in the library. fresh is false when the mirror could not be refreshed, in
// which case a not-owned answer proves nothing.
func (s *Service) planOwnership(ctx context.Context, plan *Plan) (status, itemKey string, fresh bool, err error) {
	if plan.Route == "existing_item" {
		key := plan.ExpectedParentKey
		fresh = s.CLI.Sync(ctx) == nil
		holding, known := s.itemsHoldingPDF(ctx, []string{key})
		if !known {
			return "", "", false, errors.New("Zotio could not read the missing-PDF queue")
		}
		if holding[key] {
			// The queue lists only items that exist and lack a PDF, so
			// absence from it proves a PDF only for an item that still
			// exists. A deleted parent is absent too.
			if _, err := s.CLI.GetItem(ctx, key); err != nil {
				return "", "", false, fmt.Errorf("Zotio could not confirm parent item %s still exists: %w", key, err)
			}
			return OwnershipOwnedWithPDF, key, fresh, nil
		}
		return OwnershipOwnedMissingPDF, key, fresh, nil
	}
	row, err := s.Bundle.Jobs.Get(ctx, plan.JobID)
	if err != nil {
		return "", "", false, err
	}
	lookup, err := s.LookupWorks(ctx, LookupWorksRequest{Works: []LookupWork{LookupWorkFrom(row.Work)}})
	if err != nil {
		return "", "", false, err
	}
	if len(lookup.Works) != 1 {
		return "", "", false, errors.New("Zotio lookup answered a different number of works")
	}
	return lookup.Works[0].Status, lookup.Works[0].ItemKey, lookup.StalenessWarning == "", nil
}
