// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"papio/internal/store"
)

// HumanAttentionProjection is the daemon-owned attention read model. A gate
// scope contributes one surface regardless of how many jobs or claim members
// depend on it.
type HumanAttentionProjection struct {
	Gates []HumanGateObservation `json:"gates"`
	Count int                    `json:"count"`
}

func scanHumanGateRows(rows *sql.Rows) ([]HumanGateObservation, error) {
	defer func() { _ = rows.Close() }()
	var out []HumanGateObservation
	for rows.Next() {
		var o HumanGateObservation
		var detail string
		if err := rows.Scan(&o.ID, &o.GateType, &o.ScopeClass, &o.ScopeKey,
			&o.InstitutionProfileID, &o.BindingID, &o.ObservationRevision,
			&o.Status, &detail, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		if err := decodeHumanGateDetail(detail, &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CurrentHumanGateProjection returns every current gate row, including closed
// rows retained as the latest fact. Callers that need operator attention should
// use CurrentHumanAttention, which filters to open rows.
func (js *Store) CurrentHumanGateProjection(ctx context.Context) ([]HumanGateObservation, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT id, gate_type, scope_class, scope_key,
		       COALESCE(institution_profile_id,''), COALESCE(binding_id,''),
		       observation_revision, status, detail_json, created_at, updated_at
		FROM human_gate_observations
		ORDER BY scope_class, scope_key, gate_type`)
	if err != nil {
		return nil, err
	}
	return scanHumanGateRows(rows)
}

// CurrentHumanAttention returns one live surface per exact gate scope. Sibling
// jobs and claim members are represented by the row's dependent sets, not by
// additional attention rows.
func (js *Store) CurrentHumanAttention(ctx context.Context) (HumanAttentionProjection, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT h.id, h.gate_type, h.scope_class, h.scope_key,
		       COALESCE(h.institution_profile_id,''), COALESCE(h.binding_id,''),
		       h.observation_revision, h.status, h.detail_json, h.created_at, h.updated_at
		FROM human_gate_observations h
		WHERE h.status = ?
		  AND (h.scope_class <> ? OR h.institution_profile_id IS NULL OR EXISTS (
		    SELECT 1 FROM institution_profiles p
		    WHERE p.id=h.institution_profile_id AND p.tombstoned_at IS NULL
		      AND h.scope_key=p.id || char(0) || CAST(p.revision AS TEXT)
		  ))
		  AND (h.scope_class <> ? OR h.institution_profile_id IS NULL OR EXISTS (
		    SELECT 1 FROM institution_profiles claim_profile
		    WHERE claim_profile.authentication_claim_id=h.scope_key
		      AND claim_profile.tombstoned_at IS NULL
		  ))
		ORDER BY h.scope_class, h.scope_key, h.gate_type`,
		string(HumanGateOpen), string(HumanGateScopeInstitutionProfile),
		string(HumanGateScopeAuthenticationClaim))
	if err != nil {
		return HumanAttentionProjection{}, err
	}
	gates, err := scanHumanGateRows(rows)
	if err != nil {
		return HumanAttentionProjection{}, err
	}
	live := gates[:0]
	for _, gate := range gates {
		hadDependents := len(gate.DependentJobIDs) > 0
		dependents := gate.DependentJobIDs[:0]
		for _, jobID := range gate.DependentJobIDs {
			row, err := js.Get(ctx, jobID)
			if errors.Is(err, sql.ErrNoRows) {
				dependents = append(dependents, jobID)
				continue
			}
			if err != nil {
				return HumanAttentionProjection{}, err
			}
			if !Terminal(row.State) {
				dependents = append(dependents, jobID)
			}
		}
		gate.DependentJobIDs = dependents
		if hadDependents && len(dependents) == 0 {
			continue
		}
		live = append(live, gate)
	}
	return HumanAttentionProjection{Gates: live, Count: len(live)}, nil
}

// HumanGateAttentionCount reports the number of live gate surfaces. It is
// deliberately a row count, never a job or dependent-sibling count.
func (js *Store) HumanGateAttentionCount(ctx context.Context) (int, error) {
	projection, err := js.CurrentHumanAttention(ctx)
	return projection.Count, err
}

// CountHumanGateAttention is a descriptive alias for HumanGateAttentionCount.
func (js *Store) CountHumanGateAttention(ctx context.Context) (int, error) {
	return js.HumanGateAttentionCount(ctx)
}

// ResolveHumanGate closes exactly the matching current gate. The expected
// revision fences a late success response; no other gate or legacy
// job-scoped human action is touched.
func (js *Store) ResolveHumanGate(ctx context.Context, gateType HumanGateType, scopeClass, scopeKey string, expectedRevision int64) error {
	if !gateType.valid() || !validHumanGateScopeClass(scopeClass) || scopeKey == "" || expectedRevision < 1 {
		return errors.New("human gate resolution requires an exact typed scope and revision")
	}
	result, err := js.S.DB().ExecContext(ctx, `
		UPDATE human_gate_observations
		SET status = ?, observation_revision = observation_revision + 1, updated_at = ?
		WHERE gate_type = ? AND scope_class = ? AND scope_key = ?
		  AND status = ? AND observation_revision = ?`,
		string(HumanGateResolved), store.Now(), string(gateType), scopeClass, scopeKey,
		string(HumanGateOpen), expectedRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: human gate scope is stale, closed, or missing", ErrConflict)
	}
	return nil
}

// CloseHumanGate is the explicit matching-gate spelling used by callers that
// treat a successful browser/platform observation as closure.
func (js *Store) CloseHumanGate(ctx context.Context, gateType HumanGateType, scopeClass, scopeKey string, expectedRevision int64) error {
	return js.ResolveHumanGate(ctx, gateType, scopeClass, scopeKey, expectedRevision)
}

// ResolveHumanGateObservation is the ID-fenced form for callbacks that carry
// the observed gate row. It is stricter than ResolveHumanGate: a success from
// a replaced gate observation cannot close the replacement.
func (js *Store) ResolveHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
	if err := observation.validate(); err != nil {
		return err
	}
	if observation.Status != HumanGateResolved {
		return errors.New("human gate closure requires resolved status")
	}
	result, err := js.S.DB().ExecContext(ctx, `
		UPDATE human_gate_observations
		SET status = ?, observation_revision = observation_revision + 1, updated_at = ?
		WHERE id = ? AND gate_type = ? AND scope_class = ? AND scope_key = ?
		  AND status = ? AND observation_revision = ?`,
		string(HumanGateResolved), store.Now(), observation.ID, string(observation.GateType),
		observation.ScopeClass, observation.ScopeKey, string(HumanGateOpen),
		observation.ObservationRevision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: human gate observation is stale, closed, or missing", ErrConflict)
	}
	return nil
}

// CloseHumanGateObservation names the same ID-fenced successful-gate operation
// for callers that use close terminology.
func (js *Store) CloseHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
	return js.ResolveHumanGateObservation(ctx, observation)
}
