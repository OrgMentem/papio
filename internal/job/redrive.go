// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"papio/internal/store"
)

// RedriveInstitutionalHandoff replaces one spent manual route with the ordinary
// institutional handoff. The resolver lookup and handoff detail come from the
// daemon's configuration and ordinary park path, not from the operator.
// revision is zero only for a parked job with no open action.
//
// The spent route is a manual download, or an open handoff whose detail
// oaHandoff recognizes as an open-access browser route: an OA URL that
// answered HTML (Wiley's pdfdirect without an entitlement) leaves the job
// parked on that handoff, and the institutional route is the one left to try.
// The third shape is an open terms_acceptance_required action with no live
// browser claim: the provider parked the drive on its consent step, and the
// resolved terms action's reason travels in the job.retry_requested event.
//
// The fourth shape is an unavailable job whose terminal reason is
// browser_rejected. Until 2026-09-23 the bridge turned an empty job_reject,
// sent when the extension had lost its own worker-local offer URL, into that
// terminal state. The reject carried no evidence about the paper, so the job
// returns to awaiting_human with a fresh institutional handoff (revision 0).
//
// The fifth shape is a needs_review job on one open manual_download with the
// adopted_pdf_failed_validation diagnosis: an adopted file failed validation
// and could not be moved to rejected/, so papio parked it out of the adoption
// sweep's reach and asked the operator to remove it. adoptedFileGone is the
// caller's observation that the job's adoption directories now hold no file;
// without it the job stays parked, because awaiting_human would let the sweep
// re-adopt and re-reject that same file every tick.
//
// Two awaiting_human shapes return the job to resolving instead and yield no
// new action, so the returned id is zero: a manual download that the
// open-access browser route left behind (oaHandoff recognizes the handoff the
// bridge resolved before it), and any spent route of a job whose
// institutional route has already reported no entitlement.
func (js *Store) RedriveInstitutionalHandoff(ctx context.Context, jobID string, revision int64,
	openURLBaseFor func(string) (string, bool), oaHandoff func(detail string) bool, adoptedFileGone bool,
	handoffDetail string) (int64, error) {
	return js.redriveInstitutionalHandoff(ctx, jobID, revision, openURLBaseFor, oaHandoff, adoptedFileGone, handoffDetail, true)
}

// CheckRedriveInstitutionalHandoff answers whether RedriveInstitutionalHandoff
// would accept the same request, without changing anything: it runs the very
// same preconditions inside a transaction it always rolls back. The paced
// drive uses it to rank a manual download without replacing it, so its status
// view never mutates and its eligibility can never drift from the verb's.
func (js *Store) CheckRedriveInstitutionalHandoff(ctx context.Context, jobID string, revision int64,
	openURLBaseFor func(string) (string, bool), oaHandoff func(detail string) bool) error {
	_, err := js.redriveInstitutionalHandoff(ctx, jobID, revision, openURLBaseFor, oaHandoff, false, "", false)
	return err
}

func (js *Store) redriveInstitutionalHandoff(ctx context.Context, jobID string, revision int64,
	openURLBaseFor func(string) (string, bool), oaHandoff func(detail string) bool, adoptedFileGone bool,
	handoffDetail string, apply bool) (int64, error) {
	if strings.TrimSpace(jobID) == "" || revision < 0 {
		return 0, errors.New("job_id and non-negative revision are required")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET updated_at=updated_at WHERE id=?`, jobID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, sql.ErrNoRows
	}
	var state, terminalReason, resolver, doi, pmid, isbn string
	var leased, won, blocked int
	err = tx.QueryRowContext(ctx, `SELECT j.state, COALESCE(j.terminal_reason,''),
		COALESCE(json_extract(j.policy_json,'$.resolver'),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='doi' LIMIT 1),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='pmid' LIMIT 1),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='isbn' LIMIT 1),''),
		(j.lease_owner IS NOT NULL AND (j.lease_expires_at IS NULL OR j.lease_expires_at>=?)),
		(j.artifact_sha256 IS NOT NULL OR EXISTS (SELECT 1 FROM artifact_winners w WHERE w.job_id=j.id)),
		(SELECT COUNT(*) FROM effect_permits p WHERE p.status IN ('held','unknown_completion'))+
		(SELECT COUNT(*) FROM legacy_effect_blockers l WHERE l.status='unresolved')
		FROM jobs j WHERE j.id=?`, now, jobID).Scan(&state, &terminalReason, &resolver, &doi, &pmid, &isbn, &leased, &won, &blocked)
	if err != nil {
		return 0, err
	}
	rejected := state == StateUnavailable && terminalReason == string(TerminalReasonBrowserRejected)
	unquarantined := state == StateNeedsReview
	if state != StateAwaitingHuman && !rejected && !unquarantined {
		return 0, fmt.Errorf("%w: job is %s, not awaiting_human, needs_review on an adopted file, or unavailable after browser_rejected", ErrConflict, state)
	}
	if leased != 0 || won != 0 || blocked != 0 {
		return 0, fmt.Errorf("%w: job has a lease, artifact, or unresolved effect permit", ErrConflict)
	}
	base, configured := openURLBaseFor(resolver)
	if !configured || base == "" || (doi == "" && pmid == "" && isbn == "") {
		return 0, fmt.Errorf("%w: job has no usable institutional resolver route", ErrConflict)
	}
	var latestRedrive, latestOutcome int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(MAX(CASE WHEN kind='job.retry_requested' AND json_extract(detail_json,'$.reason')='operator_redrive' THEN seq END),0),
		COALESCE(MAX(CASE WHEN kind IN ('browser.provider_outcome','browser.download_complete') THEN seq END),0)
		FROM events WHERE job_id=?`, jobID).Scan(&latestRedrive, &latestOutcome); err != nil {
		return 0, err
	}
	if latestRedrive > latestOutcome {
		return 0, fmt.Errorf("%w: redrive already requested since the last browser outcome", ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,COALESCE(detail,''),COALESCE(diagnosis,''),revision FROM human_actions WHERE job_id=? AND status='open'`, jobID)
	if err != nil {
		return 0, err
	}
	var actionID, actionRevision int64
	var actionKind, actionDetail, actionDiagnosis string
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&actionID, &actionKind, &actionDetail, &actionDiagnosis, &actionRevision); err != nil {
			_ = rows.Close()
			return 0, err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	if unquarantined {
		if count != 1 || actionKind != "manual_download" || actionDiagnosis != DiagnosisReasonAdoptedPDFInvalid || revision != actionRevision {
			return 0, fmt.Errorf("%w: needs_review redrive expects one unchanged manual_download for an adopted file that failed validation; list actions again", ErrConflict)
		}
		if !adoptedFileGone {
			return 0, fmt.Errorf("%w: the job's adoption directory still holds a file, or could not be read; remove the file, then redrive", ErrConflict)
		}
	}
	// A terms action is the third spent shape: the provider parked the drive
	// on its consent step, and the extension's own consent setting decides it
	// again when the fresh handoff is driven. Nothing is accepted here; a
	// profile without consent simply parks on terms again. A live claim means
	// a drive is still on that surface, so only a parked or retired one yields.
	terms := actionKind == "terms_acceptance_required"
	spent := actionKind == "manual_download" || terms || (actionKind == "openurl_handoff" && oaHandoff(actionDetail))
	if count > 1 || (count == 1 && (!spent || revision != actionRevision)) || (count == 0 && revision != 0) {
		return 0, fmt.Errorf("%w: expected one unchanged manual_download, open-access handoff, or terms_acceptance_required action, or no open action with revision 0; list actions again", ErrConflict)
	}
	if count == 1 && terms {
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM materialization_claims m
			JOIN browser_candidates c ON c.id=m.candidate_id WHERE c.job_id=?
			AND m.phase IN ('claimed','bound','route_issued','navigated'))`, jobID).Scan(&live); err != nil {
			return 0, err
		}
		if live != 0 {
			return 0, fmt.Errorf("%w: job still has a live browser claim on its terms step", ErrConflict)
		}
	}
	if count == 0 {
		// A browser_rejected job's handoff was cancelled by that terminal
		// transition; any other park names the action it last resolved.
		query := `SELECT id,revision FROM human_actions
			WHERE job_id=? AND status='resolved' ORDER BY id DESC LIMIT 1`
		if rejected {
			query = `SELECT id,revision FROM human_actions
			WHERE job_id=? AND kind='openurl_handoff' AND status='cancelled' ORDER BY id DESC LIMIT 1`
		}
		err := tx.QueryRowContext(ctx, query, jobID).Scan(&actionID, &actionRevision)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: no resolved action on parked job", ErrConflict)
		}
		if err != nil {
			return 0, err
		}
	}
	// Two spent routes are redriven through rediscovery, not a fresh
	// institutional handoff, because that handoff is not the route left to
	// try. A manual download the open-access browser route left behind asks
	// for that route again: the page was reachable, only papio's drive of it
	// failed. And an institutional route that already reported no entitlement
	// (browser.no_entitlement_requeue) can only report it again. Live
	// 2026-09-24, a paced redrive of a PMC manual download opened exactly that
	// route, and its second no_entitlement ended the job unavailable.
	// Resolving hands the choice to exhaustion, its one owner: it re-derives a
	// live open-access URL, falls back to an untried institutional route, or
	// settles the job when neither remains. A stored open-access URL is never
	// reused, because it can be a bearer link that has since expired.
	rediscover := false
	if state == StateAwaitingHuman {
		var provenEmpty int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events
			WHERE job_id=? AND kind='browser.no_entitlement_requeue')`, jobID).Scan(&provenEmpty); err != nil {
			return 0, err
		}
		rediscover = provenEmpty != 0
		if !rediscover && count == 1 && actionKind == "manual_download" {
			// The bridge resolves the handoff it drove before it opens the
			// manual download, so the newest earlier handoff is its route.
			var route string
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(detail,'') FROM human_actions
				WHERE job_id=? AND kind='openurl_handoff' AND id<? ORDER BY id DESC LIMIT 1`, jobID, actionID).Scan(&route)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			rediscover = oaHandoff(route)
		}
	}
	if !apply {
		return 0, nil
	}
	if rejected || unquarantined {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?, terminal_reason=NULL, updated_at=?,
			retry_at=NULL, lease_owner=NULL, lease_expires_at=NULL WHERE id=? AND state=?`,
			StateAwaitingHuman, now, jobID, state)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return 0, fmt.Errorf("%w: job changed; list jobs again", ErrConflict)
		}
		transition, err := json.Marshal(map[string]any{"from": state, "to": StateAwaitingHuman, "reason": "operator_redrive"})
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,'job.transition',?)`, jobID, now, string(transition)); err != nil {
			return 0, err
		}
	}
	bindings, err := tx.QueryContext(ctx, `SELECT DISTINCT m.binding_id FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id WHERE c.job_id=?
		AND m.phase IN ('claimed','bound','route_issued','navigated','parked') AND m.binding_id IS NOT NULL AND m.binding_id<>''`, jobID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for bindings.Next() {
		var id string
		if err := bindings.Scan(&id); err != nil {
			_ = bindings.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = bindings.Err()
	_ = bindings.Close()
	if err != nil {
		return 0, err
	}
	for _, binding := range ids {
		if _, err := abandonMaterializationClaimByBindingTx(ctx, tx, binding, now); err != nil {
			return 0, err
		}
	}
	if err := releaseAuthenticationEntryLeasesForBindingsTx(ctx, tx, ids, now); err != nil {
		return 0, err
	}
	if err := consumeCloseAuthorizationsTx(ctx, tx, ids, now); err != nil {
		return 0, err
	}
	var fresh int64
	if rediscover {
		var resolve []int64
		if count == 1 {
			resolve = []int64{actionID}
		}
		transition, err := json.Marshal(map[string]any{"from": StateAwaitingHuman, "to": StateResolving, "reason": "operator_redrive"})
		if err != nil {
			return 0, err
		}
		if err := js.RepairAwaitingHumanTx(ctx, tx, jobID, resolve, string(transition), now); err != nil {
			return 0, err
		}
	} else {
		if count == 1 {
			res, err := tx.ExecContext(ctx, `UPDATE human_actions SET status='resolved',resolved_at=? WHERE id=? AND status='open' AND revision=?`, now, actionID, revision)
			if err != nil {
				return 0, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return 0, fmt.Errorf("%w: action changed; list actions again", ErrConflict)
			}
		}
		if fresh, err = js.OpenHumanActionTx(ctx, tx, jobID, "openurl_handoff", handoffDetail, now, Access(true, "paywall")); err != nil {
			return 0, err
		}
	}
	detail, err := json.Marshal(map[string]any{"reason": "operator_redrive", "action_id": actionID, "action_revision": actionRevision})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`, jobID, now, "job.retry_requested", string(detail)); err != nil {
		return 0, err
	}
	return fresh, tx.Commit()
}
