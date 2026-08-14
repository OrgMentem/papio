package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"papio/internal/store"
)

// LegacyEffectBlockerStatus is the durable state of a pre-0034 irreversible effect.
type LegacyEffectBlockerStatus string

const (
	LegacyEffectBlockerUnresolved LegacyEffectBlockerStatus = "unresolved"
	LegacyEffectBlockerSettled    LegacyEffectBlockerStatus = "settled"
)

// LegacyEffectBlocker preserves an irreversible effect that crossed the
// daemon/browser boundary before durable effect permits existed. It never
// consumes a permit slot, but every unresolved row globally refuses admission.
type LegacyEffectBlocker struct {
	ID                   string                    `json:"id"`
	Kind                 EffectKind                `json:"kind"`
	JobID                string                    `json:"job_id,omitempty"`
	SafetyDomainID       string                    `json:"safety_domain_id"`
	DriveAttemptID       string                    `json:"drive_attempt_id,omitempty"`
	Ordinal              int64                     `json:"ordinal,omitempty"`
	Strategy             string                    `json:"strategy,omitempty"`
	Revision             string                    `json:"revision,omitempty"`
	ClaimID              string                    `json:"claim_id,omitempty"`
	BindingID            string                    `json:"binding_id,omitempty"`
	EffectOrdinal        int64                     `json:"effect_ordinal,omitempty"`
	GrabID               string                    `json:"grab_id,omitempty"`
	ReconstructedAttempt *int64                    `json:"reconstructed_attempt,omitempty"`
	ReconstructedHolder  *int64                    `json:"reconstructed_holder,omitempty"`
	CleanupOnly          bool                      `json:"cleanup_only"`
	Status               LegacyEffectBlockerStatus `json:"status"`
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
}

// LegacyEffectBlockerInput identifies one exact imported effect to settle.
type LegacyEffectBlockerInput struct {
	Kind           EffectKind
	JobID          string
	DriveAttemptID string
	Ordinal        int64
	Strategy       string
	Revision       string
	ClaimID        string
	BindingID      string
	EffectOrdinal  int64
	GrabID         string
}

func (in LegacyEffectBlockerInput) validate() error {
	switch in.Kind {
	case GenericDrive, DirectGet:
		if !nonempty(in.JobID) || !nonempty(in.DriveAttemptID) ||
			in.Ordinal < 0 || !nonempty(in.Strategy) || !nonempty(in.Revision) {
			return errors.New("legacy drive blocker identity is incomplete")
		}
		if in.Kind == DirectGet && in.Strategy != "direct_get" {
			return errors.New("legacy direct blocker requires direct_get strategy")
		}
		if in.Kind == GenericDrive && in.Strategy == "direct_get" {
			return errors.New("legacy generic blocker cannot use direct_get strategy")
		}
	case PDFGrab:
		if !nonempty(in.GrabID) {
			return errors.New("legacy PDF blocker identity is incomplete")
		}
	case Institutional:
		if !nonempty(in.JobID) || !nonempty(in.ClaimID) ||
			!nonempty(in.BindingID) || in.EffectOrdinal < 1 {
			return errors.New("legacy institutional blocker identity is incomplete")
		}
	default:
		return errors.New("legacy effect blocker requires a supported kind")
	}
	return nil
}

// ImportLegacyStartedEpochs classifies every durable pre-permit indication that
// an irreversible browser effect may already have crossed the process boundary.
// Supersession and elapsed time are not completion evidence. Malformed start
// authority aborts startup rather than silently authorizing over ambiguity.
func (js *Store) ImportLegacyStartedEpochs(ctx context.Context) error {
	if js == nil || js.S == nil {
		return errors.New("job store is not initialized")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := importLegacyDriveEffects(ctx, tx); err != nil {
		return err
	}
	if err := importLegacyPDFGrabs(ctx, tx); err != nil {
		return err
	}
	if err := importLegacyInstitutionalClaims(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type legacyDriveEpoch struct {
	kind                                       EffectKind
	jobID, domain, attempt, strategy, revision string
	ordinal                                    int64
	started, result                            bool
}

func importLegacyDriveEffects(ctx context.Context, tx *sql.Tx) error {
	epochs := make(map[string]*legacyDriveEpoch)
	rows, err := tx.QueryContext(ctx, `
		SELECT e.job_id, j.id, e.kind, e.detail_json
		  FROM events e
		  LEFT JOIN jobs j ON j.id = e.job_id
		 WHERE e.kind IN ('browser.provider_drive_epoch_started',
		                  'browser.provider_drive_epoch_result',
		                  'browser.provider_drive_epoch_superseded',
		                  'browser.direct_route')
		 ORDER BY e.seq ASC`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var jobID, existingJobID, eventKind, raw sql.NullString
		if err := rows.Scan(&jobID, &existingJobID, &eventKind, &raw); err != nil {
			return err
		}
		detail, err := decodeLegacyEpochDetail(raw.String)
		if err != nil {
			return fmt.Errorf("decode legacy effect event for job %q: %w", jobID.String, err)
		}
		// An event with any relevant effect kind is evidence that may have
		// crossed the browser boundary. Do not let a missing/dangling job
		// attribution disappear through an inner join or a nullable scan.
		job := jobID.String
		jobExists := jobID.Valid && existingJobID.Valid && nonempty(existingJobID.String)

		if eventKind.String == "browser.direct_route" {
			attempt := legacyString(detail, "drive_attempt_id")
			revision := legacyString(detail, "route_revision")
			domain := legacyString(detail, "safety_domain")
			ordinal, ok := legacyInt(detail, "ordinal")
			if !jobExists || !nonempty(attempt) || !nonempty(revision) || !ok || ordinal < 0 {
				return fmt.Errorf("unclassifiable legacy direct effect for job %q", job)
			}
			phase := legacyString(detail, "phase")
			if phase != "offered" && phase != "result" {
				continue
			}
			if phase == "offered" && !nonempty(domain) {
				return fmt.Errorf("unclassifiable legacy direct effect for job %q", job)
			}
			key := legacyDriveBlockerKey(DirectGet, job, attempt, ordinal, "direct_get", revision)
			e := epochs[key]
			if e == nil {
				e = &legacyDriveEpoch{kind: DirectGet, jobID: job, attempt: attempt, ordinal: ordinal, strategy: "direct_get", revision: revision}
				epochs[key] = e
			}
			if nonempty(domain) {
				if nonempty(e.domain) && e.domain != domain {
					return fmt.Errorf("unclassifiable legacy direct effect for job %q: conflicting safety domains", job)
				}
				e.domain = domain
			}
			if phase == "offered" {
				e.started = true
			} else {
				e.result = true
			}
			continue
		}

		attempt := legacyString(detail, "drive_attempt_id")
		strategy := legacyString(detail, "strategy")
		revision := legacyString(detail, "revision")
		ordinal, ok := legacyInt(detail, "ordinal")
		domain := legacyString(detail, "safety_domain")
		if !jobExists || !nonempty(attempt) || !nonempty(strategy) || !nonempty(revision) || !ok || ordinal < 0 ||
			(eventKind.String == "browser.provider_drive_epoch_started" && !nonempty(domain)) {
			return fmt.Errorf("unclassifiable legacy provider drive effect for job %q", job)
		}
		key := legacyDriveBlockerKey(GenericDrive, job, attempt, ordinal, strategy, revision)
		e := epochs[key]
		if e == nil {
			e = &legacyDriveEpoch{kind: GenericDrive, jobID: job, attempt: attempt, ordinal: ordinal, strategy: strategy, revision: revision}
			epochs[key] = e
		}
		if nonempty(domain) {
			if nonempty(e.domain) && e.domain != domain {
				return fmt.Errorf("unclassifiable legacy provider drive effect for job %q: conflicting safety domains", job)
			}
			e.domain = domain
		}
		switch eventKind.String {
		case "browser.provider_drive_epoch_started":
			e.started = true
		case "browser.provider_drive_epoch_result":
			e.result = true
		case "browser.provider_drive_epoch_superseded":
			// Pre-permit supersession only minted a successor after a timeout. It
			// did not prove that the original effect had not crossed the boundary.
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, e := range epochs {
		if !e.started || e.result {
			continue
		}
		if err := insertLegacyDriveBlocker(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

func insertLegacyDriveBlocker(ctx context.Context, tx *sql.Tx, e *legacyDriveEpoch) error {
	now := store.Now()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal, strategy, revision,
		   reconstructed_attempt, reconstructed_holder, cleanup_only, status, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 1, 'unresolved', ?, ?
		 WHERE NOT EXISTS (
		       SELECT 1 FROM effect_permits
		        WHERE job_id=? AND effect_kind IN ('generic_drive','direct_get')
		          AND drive_attempt_id=? AND ordinal=? AND strategy=? AND revision=?
		       )
		ON CONFLICT DO NOTHING`,
		NewID("legacy_effect_blocker"), e.kind, e.jobID, e.domain, e.attempt, e.ordinal, e.strategy, e.revision,
		now, now, e.jobID, e.attempt, e.ordinal, e.strategy, e.revision)
	return err
}

func importLegacyPDFGrabs(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, url_host
		  FROM pdf_grabs
		 WHERE state IN ('awaiting_file','abandoned')
		   AND (job_id IS NULL OR job_id = '')
		   AND (state = 'awaiting_file' OR outcome IN ('','abandoned'))
		 ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var grabID, host string
		if err := rows.Scan(&grabID, &host); err != nil {
			return err
		}
		if !nonempty(grabID) || !nonempty(host) {
			return fmt.Errorf("unclassifiable legacy PDF grab %q", grabID)
		}
		now := store.Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO legacy_effect_blockers
			  (id, effect_kind, job_id, safety_domain_id, grab_id, reconstructed_attempt,
			   reconstructed_holder, cleanup_only, status, created_at, updated_at)
			SELECT ?, 'pdf_grab', NULL, ?, ?, NULL, NULL, 1, 'unresolved', ?, ?
			 WHERE NOT EXISTS (SELECT 1 FROM effect_permits WHERE effect_kind='pdf_grab' AND grab_id=?)
			ON CONFLICT DO NOTHING`,
			NewID("legacy_effect_blocker"), "pdf_grab:"+host, grabID, now, now, grabID); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func importLegacyInstitutionalClaims(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, c.job_id, m.binding_id,
		       CASE WHEN m.effect_ordinal >= 1 THEN m.effect_ordinal
		            ELSE m.route_issuance_ordinal END,
		       m.route_issuance_ordinal, c.safety_domain_id
		  FROM materialization_claims m
		  JOIN browser_candidates c ON c.id=m.candidate_id
		 WHERE m.phase IN ('route_issued','abandoned')
		   AND CASE WHEN m.effect_ordinal >= 1 THEN m.effect_ordinal
		            ELSE m.route_issuance_ordinal END >= 1
		   AND NOT EXISTS (
		       SELECT 1 FROM artifact_winners w
		        WHERE w.job_id=c.job_id
		          AND w.job_attempt_revision=c.job_attempt_revision
		          AND w.candidate_id=m.candidate_id)
		   AND NOT EXISTS (
		       SELECT 1 FROM events e
		        WHERE e.job_id=c.job_id
		          AND e.kind='browser.institutional_effect_result'
		          AND json_extract(e.detail_json, '$.claim_id')=m.id
		          AND json_extract(e.detail_json, '$.binding_id')=m.binding_id
		          AND CAST(json_extract(e.detail_json, '$.effect_ordinal') AS INTEGER) =
		              CASE WHEN m.effect_ordinal >= 1 THEN m.effect_ordinal
		                   ELSE m.route_issuance_ordinal END)
		 ORDER BY m.id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var claimID, jobID, bindingID, domain string
		var effectOrdinal, routeOrdinal int64
		if err := rows.Scan(&claimID, &jobID, &bindingID, &effectOrdinal, &routeOrdinal, &domain); err != nil {
			return err
		}
		if !nonempty(claimID) || !nonempty(jobID) || !nonempty(bindingID) || effectOrdinal < 1 || !nonempty(domain) {
			return fmt.Errorf("unclassifiable legacy institutional effect for claim %q", claimID)
		}
		now := store.Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO legacy_effect_blockers
			  (id, effect_kind, job_id, safety_domain_id, claim_id, binding_id, effect_ordinal,
			   reconstructed_attempt, reconstructed_holder, cleanup_only, status, created_at, updated_at)
			SELECT ?, 'institutional', ?, ?, ?, ?, ?, NULL, NULL, 1, 'unresolved', ?, ?
			 WHERE NOT EXISTS (
			       SELECT 1 FROM effect_permits
			        WHERE effect_kind='institutional' AND claim_id=? AND binding_id=? AND effect_ordinal=?
			       )
			ON CONFLICT DO NOTHING`,
			NewID("legacy_effect_blocker"), jobID, domain, claimID, bindingID, effectOrdinal, now, now,
			claimID, bindingID, effectOrdinal); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

// SettleLegacyEffectBlocker settles exactly one imported effect. It mutates
// only the blocker row; cleanup-only late evidence never touches current jobs.
func (js *Store) SettleLegacyEffectBlocker(ctx context.Context, in LegacyEffectBlockerInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	where, args := legacyBlockerIdentityWhere(in)
	all := append([]any{store.Now()}, args...)
	res, err := js.S.DB().ExecContext(ctx, `UPDATE legacy_effect_blockers SET status='settled', updated_at=? WHERE `+where+` AND status='unresolved'`, all...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	var status string
	err = js.S.DB().QueryRowContext(ctx, `SELECT status FROM legacy_effect_blockers WHERE `+where, args...).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEffectPermitStale
	}
	if err != nil {
		return err
	}
	if status == string(LegacyEffectBlockerSettled) {
		return nil
	}
	return ErrEffectPermitStale
}

// SettleLegacyInstitutionalNavigation resolves the route tuple carried by a
// pre-permit institutional navigation and settles exactly one imported
// blocker. The route ordinal is deliberately not part of the blocker identity:
// it is joined through the durable materialization claim, whose effect ordinal
// is the only value that may be derived. The whole lookup and mutation happen
// in one transaction; any ambiguity or current permit refuses cleanup.
func (js *Store) SettleLegacyInstitutionalNavigation(ctx context.Context, jobID, claimID, bindingID string, routeIssuanceOrdinal int64) error {
	if js == nil || js.S == nil || !nonempty(jobID) || !nonempty(claimID) ||
		!nonempty(bindingID) || routeIssuanceOrdinal < 1 || routeIssuanceOrdinal > 1<<53-1 {
		return ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var blockerID string
	var effectOrdinal int64
	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM legacy_effect_blockers b
		  JOIN materialization_claims c
		    ON c.id=b.claim_id AND c.binding_id=b.binding_id
		 WHERE b.effect_kind='institutional'
		   AND b.status='unresolved'
		   AND b.job_id=? AND b.claim_id=? AND b.binding_id=?
		   AND c.route_issuance_ordinal=?`,
		jobID, claimID, bindingID, routeIssuanceOrdinal).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrEffectPermitStale
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT b.id, b.effect_ordinal
		  FROM legacy_effect_blockers b
		  JOIN materialization_claims c
		    ON c.id=b.claim_id AND c.binding_id=b.binding_id
		 WHERE b.effect_kind='institutional'
		   AND b.status='unresolved'
		   AND b.job_id=? AND b.claim_id=? AND b.binding_id=?
		   AND c.route_issuance_ordinal=?`,
		jobID, claimID, bindingID, routeIssuanceOrdinal).Scan(&blockerID, &effectOrdinal); err != nil {
		return err
	}
	if effectOrdinal < 1 || effectOrdinal > 1<<53-1 {
		return ErrEffectPermitStale
	}
	var permitCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM effect_permits
		 WHERE effect_kind='institutional' AND job_id=? AND claim_id=? AND binding_id=?`,
		jobID, claimID, bindingID).Scan(&permitCount); err != nil {
		return err
	}
	if permitCount != 0 {
		return ErrEffectPermitStale
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE legacy_effect_blockers
		   SET status='settled', updated_at=?
		 WHERE id=? AND status='unresolved'
		   AND NOT EXISTS (
		       SELECT 1 FROM effect_permits
		        WHERE effect_kind='institutional' AND job_id=? AND claim_id=? AND binding_id=?
		   )`,
		store.Now(), blockerID, jobID, claimID, bindingID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrEffectPermitStale
	}
	return tx.Commit()
}

func legacyBlockerIdentityWhere(in LegacyEffectBlockerInput) (string, []any) {
	switch in.Kind {
	case GenericDrive, DirectGet:
		return `effect_kind=? AND job_id=? AND drive_attempt_id=? AND ordinal=? AND strategy=? AND revision=?`,
			[]any{in.Kind, in.JobID, in.DriveAttemptID, in.Ordinal, in.Strategy, in.Revision}
	case PDFGrab:
		return `effect_kind='pdf_grab' AND grab_id=?`, []any{in.GrabID}
	case Institutional:
		return `effect_kind='institutional' AND job_id=? AND claim_id=? AND binding_id=? AND effect_ordinal=?`,
			[]any{in.JobID, in.ClaimID, in.BindingID, in.EffectOrdinal}
	default:
		return `1=0`, nil
	}
}

func (b LegacyEffectBlocker) Identity() LegacyEffectBlockerInput {
	return LegacyEffectBlockerInput{
		Kind: b.Kind, JobID: b.JobID, DriveAttemptID: b.DriveAttemptID, Ordinal: b.Ordinal,
		Strategy: b.Strategy, Revision: b.Revision, ClaimID: b.ClaimID, BindingID: b.BindingID,
		EffectOrdinal: b.EffectOrdinal, GrabID: b.GrabID,
	}
}

// LegacyEffectBlockerRecovery is the only valid recovery path for an imported
// blocker. It is deliberately closed so this read model cannot leak provider
// detail or suggest an unsupported administrative cleanup.
type LegacyEffectBlockerRecovery string

const LegacyEffectBlockerRecoveryExactResultOrCorrelatedWinner LegacyEffectBlockerRecovery = "exact_result_or_correlated_winner"

// LegacyEffectBlockerRead is the bounded, privacy-safe projection used by
// operator surfaces. It excludes safety domains, grab hosts, claims, bindings,
// and every other provider/path-bearing field.
type LegacyEffectBlockerRead struct {
	ID             string
	Kind           EffectKind
	JobID          string
	DriveAttemptID string
	Ordinal        *int64
	Strategy       string
	Revision       string
	Since          time.Time
	Recovery       LegacyEffectBlockerRecovery
}

const LegacyEffectBlockerReadLimit = 16

// ReadUnresolvedLegacyEffectBlockers returns a stable, bounded view. The
// boolean reports that additional unresolved rows exist beyond the returned
// page; callers must render that fact rather than imply completeness.
func (js *Store) ReadUnresolvedLegacyEffectBlockers(ctx context.Context, limit int) ([]LegacyEffectBlockerRead, bool, error) {
	if limit <= 0 || limit > LegacyEffectBlockerReadLimit {
		limit = LegacyEffectBlockerReadLimit
	}
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT id, effect_kind, COALESCE(job_id,''), COALESCE(drive_attempt_id,''),
		       ordinal, COALESCE(strategy,''), COALESCE(revision,''), created_at
		FROM legacy_effect_blockers
		WHERE status='unresolved'
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]LegacyEffectBlockerRead, 0, limit)
	truncated := false
	for rows.Next() {
		if len(out) == limit {
			truncated = true
			break
		}
		var item LegacyEffectBlockerRead
		var ordinal sql.NullInt64
		var created string
		if err := rows.Scan(&item.ID, &item.Kind, &item.JobID, &item.DriveAttemptID,
			&ordinal, &item.Strategy, &item.Revision, &created); err != nil {
			return nil, false, err
		}
		if created != "" {
			item.Since, err = time.Parse(time.RFC3339Nano, created)
			if err != nil {
				return nil, false, err
			}
		}
		if ordinal.Valid {
			v := ordinal.Int64
			item.Ordinal = &v
		}
		item.Recovery = LegacyEffectBlockerRecoveryExactResultOrCorrelatedWinner
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}

// UnresolvedLegacyEffectBlockers returns all unresolved blockers in stable order.
func (js *Store) UnresolvedLegacyEffectBlockers(ctx context.Context) ([]LegacyEffectBlocker, error) {
	rows, err := js.S.DB().QueryContext(ctx, legacyBlockerSelect+`
		 WHERE status='unresolved' ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LegacyEffectBlocker
	for rows.Next() {
		b, err := scanLegacyEffectBlocker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (js *Store) UnresolvedLegacyEffectBlockerCount(ctx context.Context) (int, error) {
	var count int
	err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_effect_blockers WHERE status='unresolved'`).Scan(&count)
	return count, err
}

const legacyBlockerSelect = `SELECT id, effect_kind, job_id, safety_domain_id,
	drive_attempt_id, ordinal, strategy, revision, claim_id, binding_id, effect_ordinal, grab_id,
	reconstructed_attempt, reconstructed_holder, cleanup_only, status, created_at, updated_at
	FROM legacy_effect_blockers`

func scanLegacyEffectBlocker(s interface{ Scan(...any) error }) (*LegacyEffectBlocker, error) {
	var b LegacyEffectBlocker
	var jobID, driveAttemptID, strategy, revision, claimID, bindingID, grabID sql.NullString
	var ordinal, effectOrdinal, attempt, holder sql.NullInt64
	var cleanup int
	var created, updated string
	if err := s.Scan(&b.ID, &b.Kind, &jobID, &b.SafetyDomainID, &driveAttemptID, &ordinal,
		&strategy, &revision, &claimID, &bindingID, &effectOrdinal, &grabID,
		&attempt, &holder, &cleanup, &b.Status, &created, &updated); err != nil {
		return nil, err
	}
	b.JobID, b.DriveAttemptID, b.Strategy, b.Revision = jobID.String, driveAttemptID.String, strategy.String, revision.String
	b.ClaimID, b.BindingID, b.GrabID = claimID.String, bindingID.String, grabID.String
	if ordinal.Valid {
		b.Ordinal = ordinal.Int64
	}
	if effectOrdinal.Valid {
		b.EffectOrdinal = effectOrdinal.Int64
	}
	if attempt.Valid {
		v := attempt.Int64
		b.ReconstructedAttempt = &v
	}
	if holder.Valid {
		v := holder.Int64
		b.ReconstructedHolder = &v
	}
	b.CleanupOnly = cleanup != 0
	var err error
	if created != "" {
		b.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
	}
	if updated != "" {
		b.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
	}
	return &b, nil
}

func legacyDriveBlockerKey(kind EffectKind, jobID, attempt string, ordinal int64, strategy, revision string) string {
	return string(kind) + "\x00" + jobID + "\x00" + attempt + "\x00" + strconv.FormatInt(ordinal, 10) + "\x00" + strategy + "\x00" + revision
}

func decodeLegacyEpochDetail(raw string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var detail map[string]any
	if err := dec.Decode(&detail); err != nil {
		return nil, err
	}
	return detail, nil
}

func legacyString(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return strings.TrimSpace(value)
}

func legacyInt(detail map[string]any, key string) (int64, bool) {
	switch value := detail[key].(type) {
	case json.Number:
		n, err := strconv.ParseInt(string(value), 10, 64)
		return n, err == nil
	case float64:
		return int64(value), value == float64(int64(value))
	case int64:
		return value, true
	case int:
		return int64(value), true
	default:
		return 0, false
	}
}
