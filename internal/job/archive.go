// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/store"
)

const DispositionActive = "active"
const DispositionArchived = "archived"

var ErrArchiveConfirmation = errors.New("archive requires explicit confirmation")

// DispositionResult keeps consumption intent separate from the acquisition state.
type DispositionResult struct {
	JobID          string `json:"job_id"`
	State          string `json:"state"`
	Disposition    string `json:"disposition"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Changed        bool   `json:"changed"`
}

func (js *Store) Disposition(ctx context.Context, jobID string) (DispositionResult, error) {
	var result DispositionResult
	err := js.S.DB().QueryRowContext(ctx, `SELECT id, state, acquisition_disposition, COALESCE(artifact_sha256, '') FROM jobs WHERE id = ?`, jobID).Scan(&result.JobID, &result.State, &result.Disposition, &result.ArtifactSHA256)
	return result, err
}

func (js *Store) Archive(ctx context.Context, jobID string, confirmed bool) (DispositionResult, error) {
	if !confirmed {
		return DispositionResult{}, ErrArchiveConfirmation
	}
	return js.setDisposition(ctx, jobID, DispositionArchived)
}

func (js *Store) Restore(ctx context.Context, jobID string) (DispositionResult, error) {
	return js.setDisposition(ctx, jobID, DispositionActive)
}

func (js *Store) setDisposition(ctx context.Context, jobID, disposition string) (DispositionResult, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return DispositionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Reserve the writer before checking intent, including concurrent filing.
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET acquisition_disposition = acquisition_disposition WHERE id = ?`, jobID); err != nil {
		return DispositionResult{}, err
	}
	var result DispositionResult
	if err = tx.QueryRowContext(ctx, `SELECT id, state, acquisition_disposition, COALESCE(artifact_sha256, '') FROM jobs WHERE id = ?`, jobID).Scan(&result.JobID, &result.State, &result.Disposition, &result.ArtifactSHA256); err != nil {
		return result, err
	}
	if result.State != StateReady {
		return result, ErrConflict
	}
	if disposition == DispositionArchived {
		var hookRunning bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM events reserved
			WHERE reserved.job_id = ? AND reserved.kind = 'hook.filing_reserved'
			AND NOT EXISTS (
				SELECT 1 FROM events released
				WHERE released.job_id = reserved.job_id
				AND released.kind = 'hook.filing_released'
				AND json_extract(released.detail_json, '$.reservation_id') =
					json_extract(reserved.detail_json, '$.reservation_id')
			)
		)`, jobID).Scan(&hookRunning); err != nil {
			return result, err
		}
		if hookRunning {
			return result, ErrConflict
		}
		// An external mutation can outlive its lease or report failure after
		// committing. Any apply receipt therefore prevents archival.
		var filing bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM exports WHERE job_id = ? AND kind = 'zotio_apply'
		)`, jobID).Scan(&filing); err != nil {
			return result, err
		}
		if filing {
			return result, ErrConflict
		}
		var validated bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_artifacts WHERE job_id = ? AND artifact_sha256 = ? AND role = 'main' AND identity_result IN ('pass', 'user_confirmed'))`, jobID, result.ArtifactSHA256).Scan(&validated); err != nil {
			return result, err
		}
		if !validated {
			return result, ErrConflict
		}
	}
	if result.Disposition == disposition {
		return result, tx.Commit()
	}
	now := store.Now()
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET acquisition_disposition = ? WHERE id = ?`, disposition, jobID); err != nil {
		return result, err
	}
	kind := "acquisition.archived"
	if disposition == DispositionActive {
		kind = "acquisition.restored"
	}
	detail, err := json.Marshal(map[string]any{"from": result.Disposition, "to": disposition, "artifact_sha256": result.ArtifactSHA256, "artifact_retained": true})
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, ?, ?)`, jobID, now, kind, string(detail)); err != nil {
		return result, err
	}
	result.Disposition, result.Changed = disposition, true
	return result, tx.Commit()
}
