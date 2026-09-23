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
func (js *Store) RedriveInstitutionalHandoff(ctx context.Context, jobID string, revision int64,
	openURLBaseFor func(string) (string, bool), oaHandoff func(detail string) bool, handoffDetail string) (int64, error) {
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
	var state, resolver, doi, pmid, isbn string
	var leased, won, blocked int
	err = tx.QueryRowContext(ctx, `SELECT j.state,
		COALESCE(json_extract(j.policy_json,'$.resolver'),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='doi' LIMIT 1),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='pmid' LIMIT 1),''),
		COALESCE((SELECT value FROM identifiers WHERE work_request_id=j.work_request_id AND kind='isbn' LIMIT 1),''),
		(j.lease_owner IS NOT NULL AND (j.lease_expires_at IS NULL OR j.lease_expires_at>=?)),
		(j.artifact_sha256 IS NOT NULL OR EXISTS (SELECT 1 FROM artifact_winners w WHERE w.job_id=j.id)),
		(SELECT COUNT(*) FROM effect_permits p WHERE p.status IN ('held','unknown_completion'))+
		(SELECT COUNT(*) FROM legacy_effect_blockers l WHERE l.status='unresolved')
		FROM jobs j WHERE j.id=?`, now, jobID).Scan(&state, &resolver, &doi, &pmid, &isbn, &leased, &won, &blocked)
	if err != nil {
		return 0, err
	}
	if state != StateAwaitingHuman {
		return 0, fmt.Errorf("%w: job is %s, not awaiting_human", ErrConflict, state)
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
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,COALESCE(detail,''),revision FROM human_actions WHERE job_id=? AND status='open'`, jobID)
	if err != nil {
		return 0, err
	}
	var actionID, actionRevision int64
	var actionKind, actionDetail string
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&actionID, &actionKind, &actionDetail, &actionRevision); err != nil {
			_ = rows.Close()
			return 0, err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	spent := actionKind == "manual_download" || (actionKind == "openurl_handoff" && oaHandoff(actionDetail))
	if count > 1 || (count == 1 && (!spent || revision != actionRevision)) || (count == 0 && revision != 0) {
		return 0, fmt.Errorf("%w: expected one unchanged manual_download or open-access handoff action, or no open action with revision 0; list actions again", ErrConflict)
	}
	if count == 0 {
		err := tx.QueryRowContext(ctx, `SELECT id,revision FROM human_actions
			WHERE job_id=? AND status='resolved' ORDER BY id DESC LIMIT 1`, jobID).Scan(&actionID, &actionRevision)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: no resolved action on parked job", ErrConflict)
		}
		if err != nil {
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
	if count == 1 {
		res, err := tx.ExecContext(ctx, `UPDATE human_actions SET status='resolved',resolved_at=? WHERE id=? AND status='open' AND revision=?`, now, actionID, revision)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return 0, fmt.Errorf("%w: action changed; list actions again", ErrConflict)
		}
	}
	fresh, err := js.OpenHumanActionTx(ctx, tx, jobID, "openurl_handoff", handoffDetail, now, Access(true, "paywall"))
	if err != nil {
		return 0, err
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
