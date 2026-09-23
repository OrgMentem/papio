// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// RetryPublisherHandoff replaces one observed failure with one explicit DOI
// attempt. After that attempt, a newer refusing adapter can re-offer the
// original institutional route once. All checks share the write transaction.
func (js *Store) RetryPublisherHandoff(ctx context.Context, actionID, revision int64, liveVersions ...map[string]string) (string, error) {
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
	var jobID, doi, actionDetail string
	var requiresAuth, publisherRetried bool
	err = tx.QueryRowContext(ctx, `SELECT j.id, i.value, a.requires_auth, a.detail,
		EXISTS (SELECT 1 FROM events e WHERE e.job_id=j.id AND e.kind='browser.publisher_retry_requested')
		FROM human_actions a JOIN jobs j ON j.id=a.job_id
		JOIN identifiers i ON i.work_request_id=j.work_request_id AND i.kind='doi'
		WHERE a.id=? AND a.kind='manual_download' AND a.blocked_by='landing_page'
		AND a.diagnosis IN ('wrong_work','provider_adapter_drift','provider_adapter_missing')
		AND j.state='awaiting_human' AND (j.lease_owner IS NULL OR j.lease_expires_at < ?)
		AND j.artifact_sha256 IS NULL
		AND NOT EXISTS (SELECT 1 FROM human_actions other WHERE other.job_id=j.id AND other.status='open' AND other.id<>a.id)
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.job_id=j.id AND e.kind='job.retry_requested'
			AND json_extract(e.detail_json,'$.reason')='adapter_upgraded')
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
		  OR (g.gate_type<>? AND (
		    EXISTS (SELECT 1 FROM json_each(g.detail_json,'$.dependent_job_ids') WHERE value=j.id)
		    OR EXISTS (SELECT 1 FROM json_each(g.detail_json,'$.claim_member_job_ids') WHERE value=j.id)))))
		AND NOT EXISTS (SELECT 1 FROM route_suppressions s WHERE s.job_id=j.id AND s.active=1 AND s.reason IN ('provider_challenge','rate_limited'))`,
		actionID, now, now, HumanGateLogin, HumanGateMFA, HumanGateCaptchaOrSecurity, HumanGateTermsRequired).
		Scan(&jobID, &doi, &requiresAuth, &actionDetail, &publisherRetried)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: publisher retry requires one DOI route failure, no pending gate or effect, and no earlier publisher retry", ErrConflict)
	}
	if err != nil {
		return "", err
	}
	if _, err := work.NormalizeDOI(doi); err != nil {
		return "", fmt.Errorf("%w: job has no valid DOI", ErrConflict)
	}
	if publisherRetried && !IsPublisherHandoff(HumanAction{Detail: actionDetail}) {
		return "", fmt.Errorf("%w: institutional retry requires a park from the DOI attempt", ErrConflict)
	}
	adapterID, previousVersion, currentVersion := "", "", ""
	if publisherRetried {
		var recorded string
		err := tx.QueryRowContext(ctx, `SELECT e.detail_json FROM events e
			WHERE e.job_id=? AND e.kind='browser.provider_outcome'
			AND e.seq < (SELECT MIN(p.seq) FROM events p WHERE p.job_id=e.job_id AND p.kind='browser.publisher_retry_requested')
			ORDER BY e.seq DESC LIMIT 1`, jobID).Scan(&recorded)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: original institutional refusal has no adapter evidence", ErrConflict)
		}
		if err != nil {
			return "", err
		}
		var outcome struct {
			Outcome        string `json:"outcome"`
			AdapterID      string `json:"adapter_id"`
			AdapterVersion string `json:"adapter_version"`
		}
		if err := json.Unmarshal([]byte(recorded), &outcome); err != nil {
			return "", err
		}
		adapterID, previousVersion = outcome.AdapterID, outcome.AdapterVersion
		if len(liveVersions) != 0 {
			currentVersion = liveVersions[0][adapterID]
		}
		if outcome.Outcome != DiagnosisReasonWrongWork || adapterID == "" ||
			!adapterVersionNewer(previousVersion, currentVersion) {
			return "", fmt.Errorf("%w: institutional retry requires a newer version of the refusing adapter", ErrConflict)
		}
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
	handoffDetail := PublisherHandoffDetail
	if publisherRetried {
		handoffDetail = "institutional retry after adapter upgrade"
	}
	if _, err := js.OpenHumanActionTx(ctx, tx, jobID, "openurl_handoff", handoffDetail, now, Access(requiresAuth, "landing_page")); err != nil {
		return "", err
	}
	reason := "explicit_publisher_retry"
	if publisherRetried {
		reason = "adapter_upgraded"
	}
	detail, _ := json.Marshal(map[string]any{
		"reason": reason, "action_id": actionID, "action_revision": revision,
		"adapter_id": adapterID, "old_adapter_version": previousVersion, "new_adapter_version": currentVersion,
	})
	if !publisherRetried {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`,
			jobID, now, "browser.publisher_retry_requested", string(detail)); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`,
		jobID, now, "job.retry_requested", string(detail)); err != nil {
		return "", err
	}
	return jobID, tx.Commit()
}

// adapterVersionNewer refuses missing or malformed adapter evidence.
func adapterVersionNewer(previous, current string) bool {
	parse := func(version string) ([3]int, bool) {
		var numbers [3]int
		parts := strings.Split(version, ".")
		if len(parts) != len(numbers) {
			return numbers, false
		}
		for i, part := range parts {
			if part == "" {
				return numbers, false
			}
			for _, digit := range part {
				if digit < '0' || digit > '9' {
					return numbers, false
				}
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				return numbers, false
			}
			numbers[i] = n
		}
		return numbers, true
	}
	old, ok := parse(previous)
	if !ok {
		return false
	}
	now, ok := parse(current)
	if !ok {
		return false
	}
	for i := range old {
		if now[i] != old[i] {
			return now[i] > old[i]
		}
	}
	return false
}
