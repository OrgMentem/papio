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
	"papio/internal/work"
)

// PublisherHandoffDetail selects a DOI route without storing a caller-supplied
// URL or asserting open access. The URL is rebuilt from the job's identifier.
const PublisherHandoffDetail = "publisher retry via DOI"

// IsPublisherHandoff also recognizes a manual-download diagnosis after the
// publisher retry fails. Open must keep that route, not return to the resolver.
func IsPublisherHandoff(action HumanAction) bool {
	return action.Detail == PublisherHandoffDetail || strings.HasPrefix(action.Detail, PublisherHandoffDetail+"\n")
}

// RetryPublisherHandoff replaces one observed route failure with one explicit
// publisher attempt. It preserves the failed action, events and safety latches.
// The revision, action set, effect occupancy and retry limit share a transaction.
func (js *Store) RetryPublisherHandoff(ctx context.Context, actionID, revision int64) (string, error) {
	if actionID <= 0 || revision <= 0 {
		return "", errors.New("action id and revision must be positive")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	// Acquire the write transaction before reading the eligibility snapshot.
	res, err := tx.ExecContext(ctx, `UPDATE human_actions SET revision=revision
		WHERE id=? AND revision=? AND status='open'`, actionID, revision)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", fmt.Errorf("%w: action changed; list actions again", ErrConflict)
	}
	var jobID, doi string
	var requiresAuth bool
	err = tx.QueryRowContext(ctx, `SELECT j.id, i.value, a.requires_auth
		FROM human_actions a JOIN jobs j ON j.id=a.job_id
		JOIN identifiers i ON i.work_request_id=j.work_request_id AND i.kind='doi'
		WHERE a.id=? AND a.kind='manual_download' AND a.blocked_by='landing_page'
		AND a.diagnosis IN ('wrong_work','provider_adapter_drift','provider_adapter_missing')
		AND j.state='awaiting_human' AND (j.lease_owner IS NULL OR j.lease_expires_at < ?)
		AND j.artifact_sha256 IS NULL
		AND NOT EXISTS (SELECT 1 FROM human_actions other WHERE other.job_id=j.id AND other.status='open' AND other.id<>a.id)
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.job_id=j.id AND e.kind='browser.publisher_retry_requested')
		AND NOT EXISTS (SELECT 1 FROM artifact_winners w WHERE w.job_id=j.id)
		AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.status IN ('held','unknown_completion'))
		AND NOT EXISTS (SELECT 1 FROM legacy_effect_blockers l WHERE l.status='unresolved')
		AND NOT EXISTS (SELECT 1 FROM materialization_claims m JOIN browser_candidates c ON c.id=m.candidate_id
		  WHERE c.job_id=j.id AND m.phase IN ('claimed','bound','route_issued','navigated') AND (m.lease_until IS NULL OR m.lease_until>?))
		AND NOT EXISTS (SELECT 1 FROM human_gate_observations g JOIN institution_profiles p ON p.id=g.institution_profile_id
		  WHERE g.status='open' AND g.gate_type IN (?,?,?)
		  AND p.configured_name=COALESCE(NULLIF(json_extract(j.policy_json,'$.resolver'),''),'default'))
		AND NOT EXISTS (SELECT 1 FROM human_gate_observations g WHERE g.status='open' AND (
		  g.scope_class='platform'
		  OR EXISTS (SELECT 1 FROM json_each(g.detail_json,'$.dependent_job_ids') WHERE value=j.id)
		  OR EXISTS (SELECT 1 FROM json_each(g.detail_json,'$.claim_member_job_ids') WHERE value=j.id)))
		AND NOT EXISTS (SELECT 1 FROM route_suppressions s WHERE s.job_id=j.id AND s.active=1 AND s.reason IN ('provider_challenge','rate_limited'))`,
		actionID, now, now, HumanGateLogin, HumanGateMFA, HumanGateCaptchaOrSecurity).Scan(&jobID, &doi, &requiresAuth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: publisher retry requires one DOI route failure, no pending gate or effect, and no earlier publisher retry", ErrConflict)
	}
	if err != nil {
		return "", err
	}
	if _, err := work.NormalizeDOI(doi); err != nil {
		return "", fmt.Errorf("%w: job has no valid DOI", ErrConflict)
	}
	// The eligibility check above excludes live claims. Retire only the spent
	// bindings so an expired authentication reservation cannot block the retry.
	rows, err := tx.QueryContext(ctx, `SELECT m.binding_id FROM materialization_claims m JOIN browser_candidates c ON c.id=m.candidate_id
		WHERE c.job_id=? AND m.phase IN ('claimed','bound','route_issued','navigated')`, jobID)
	if err != nil {
		return "", err
	}
	var bindings []string
	for rows.Next() {
		var binding sql.NullString
		if err := rows.Scan(&binding); err != nil {
			_ = rows.Close()
			return "", err
		}
		if binding.String != "" {
			bindings = append(bindings, binding.String)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	for _, binding := range bindings {
		if _, err := abandonMaterializationClaimByBindingTx(ctx, tx, binding, now); err != nil {
			return "", err
		}
	}
	if err := releaseAuthenticationEntryLeasesForBindingsTx(ctx, tx, bindings, now); err != nil {
		return "", err
	}
	if err := consumeCloseAuthorizationsTx(ctx, tx, bindings, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE human_actions SET status='resolved', resolved_at=? WHERE id=?`, now, actionID); err != nil {
		return "", err
	}
	if _, err := js.OpenHumanActionTx(ctx, tx, jobID, "openurl_handoff", PublisherHandoffDetail, now, Access(requiresAuth, "landing_page")); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{"reason": "explicit_publisher_retry", "action_id": actionID, "action_revision": revision})
	for _, kind := range []string{"browser.publisher_retry_requested", "job.retry_requested"} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`, jobID, now, kind, string(detail)); err != nil {
			return "", err
		}
	}
	return jobID, tx.Commit()
}
