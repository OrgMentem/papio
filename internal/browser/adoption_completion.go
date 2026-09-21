// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"papio/internal/app"
	"papio/internal/job"
	"papio/internal/store"
)

var errDeliveryProvenanceUnconfirmed = errors.New("artifact already stored; browser delivery provenance is unconfirmed")

const deliveryProvenanceUnconfirmedEvent = "browser.delivery_provenance_unconfirmed"

// completedAdoption recovers only a ready job's accepted browser candidate for
// the exact confined bytes weighed by ingestAdoptedFile. A sweep may have
// published them before the completion arrived, or won while adoption ran.
// Returning the candidate keeps delivery-context pairing and cleanup identical
// to an ordinary completion without reopening or revalidating the job.
func (b *Bridge) completedAdoption(ctx context.Context, jobID, filename string, fence *artifactFence, provenance *app.BrowserDeliveryContext, supplied *job.ArtifactProducerIdentity) (int64, error) {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if row.State != job.StateReady {
		return 0, nil
	}
	if fence.digest == "" {
		return 0, fmt.Errorf("completed browser download does not match ready artifact for job %s", jobID)
	}
	candidate, err := b.jobs.GetCandidate(ctx, row.SelectedCandidateID)
	if err != nil {
		return 0, err
	}
	if candidate == nil || candidate.JobID != jobID || candidate.Source != "browser" ||
		candidate.Status != job.CandidateAccepted || candidate.URLKey != "browser-adopt:sha256:"+fence.digest {
		return 0, fmt.Errorf("ready artifact has no matching accepted browser candidate for job %s", jobID)
	}
	if row.ArtifactSHA256 != fence.digest {
		// Sanitization keeps the candidate keyed to the confined source bytes,
		// but publishes a different artifact. Only this accepted candidate's
		// recorded rewrite can connect the two; a late frame supplies no such
		// authority. Producer recovery below must still use the source digest.
		var sanitized bool
		err := b.jobs.S.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM events WHERE job_id = ? AND kind = 'job.pdf_sanitized'
			AND json_extract(detail_json, '$.candidate_id') = ?
			AND json_extract(detail_json, '$.source_sha256') = ?
			AND json_extract(detail_json, '$.adopted_sha256') = ?
		)`, jobID, candidate.ID, fence.digest, row.ArtifactSHA256).Scan(&sanitized)
		if err != nil {
			return 0, err
		}
		if !sanitized {
			return 0, fmt.Errorf("completed browser download does not match ready artifact for job %s", jobID)
		}
	}
	if err := b.svc.Artifacts.Verify(row.ArtifactSHA256); err != nil {
		return 0, err
	}
	// A late frame cannot mint its own filename+SHA authority. Recover before
	// persistArtifactCorrelation, and settle only that exact historical effect;
	// do not commit a new winner or settle whichever claim is currently live.
	producer, err := b.recoverArtifactProducerExact(ctx, jobID, filename, fence.digest)
	if err != nil {
		return 0, err
	}
	if supplied != nil && producer == nil {
		// A sweep can publish before any filename+SHA producer observation
		// exists. The artifact is complete, but this frame cannot authorize
		// provenance or producer settlement. Report that distinction without
		// minting correlation from the late frame.
		return 0, errDeliveryProvenanceUnconfirmed
	}
	if supplied != nil && !artifactProducersMatch(supplied, producer) {
		return 0, fmt.Errorf("completed browser download has no matching durable producer for job %s", jobID)
	}
	if producer != nil {
		if _, err := b.jobs.SettleArtifactProducer(ctx, jobID, *producer); err != nil {
			return 0, err
		}
	}
	if provenance != nil {
		applied, err := b.jobs.ApplyBrowserDeliveryContextToCandidate(ctx, jobID, candidate.ID,
			provenance.Route, provenance.SessionEvidence, pageHostURL(provenance.PageHost))
		if err != nil {
			return 0, err
		}
		if !applied {
			return 0, fmt.Errorf("browser delivery candidate %d is unavailable for job %s", candidate.ID, jobID)
		}
	}
	return candidate.ID, nil
}

// deliveryProvenanceUnconfirmed recognizes an observed completion disposition,
// not artifact/producer authority. Its durable key prevents a late context from
// rebuilding pending metadata after the completion was consumed or a restart.
func (b *Bridge) deliveryProvenanceUnconfirmed(ctx context.Context, key browserDownloadKey) (bool, error) {
	var found bool
	err := b.jobs.S.DB().QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM events WHERE job_id = ? AND kind = ?
		AND json_extract(detail_json, '$.download_id') = ?
	)`, key.JobID, deliveryProvenanceUnconfirmedEvent, key.DownloadID).Scan(&found)
	return found, err
}

// finishUnconfirmedDelivery records the diagnostic once before dropping pending
// metadata. Call with b.mu held. A storage failure leaves the metadata retryable;
// neither this diagnostic nor a replay can settle a producer or bind a candidate.
func (b *Bridge) finishUnconfirmedDelivery(ctx context.Context, key browserDownloadKey, filename string) error {
	detail, err := json.Marshal(map[string]any{
		"download_id": key.DownloadID,
		"filename":    filename,
		"reason":      errDeliveryProvenanceUnconfirmed.Error(),
	})
	if err != nil {
		return err
	}
	_, err = b.jobs.S.DB().ExecContext(ctx, `
		INSERT INTO events (job_id, at, kind, detail_json)
		SELECT ?, ?, ?, ? WHERE NOT EXISTS (
			SELECT 1 FROM events WHERE job_id = ? AND kind = ?
			AND json_extract(detail_json, '$.download_id') = ?
		)`, key.JobID, store.Now(), deliveryProvenanceUnconfirmedEvent, string(detail),
		key.JobID, deliveryProvenanceUnconfirmedEvent, key.DownloadID)
	if err != nil {
		return err
	}
	delete(b.pendingDownloads, key)
	delete(b.deliveryContexts, key)
	return nil
}
