// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

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

// downloadAlreadyPromoted reports whether a completion whose landing bytes are
// gone names the download the job's current ready artifact was adopted from.
// The directory sweep can promote a landed file before the browser reports
// completion (Firefox stream captures reported it 14s later), and
// SweepTerminalAdoptions then collects the ready job's landing directory, so
// the late frame has nothing left to weigh. Such a frame is not a deferred
// adoption. Without bytes it proves nothing about provenance or producers, so
// the caller only records the unconfirmed-delivery disposition.
//
// The durable record must show this exact download (id and filename) started
// before a browser adoption began, and that the first validation to finish
// after it is the promotion the job still stands on. A download that started
// later, or one whose bytes a rejected validation already consumed, keeps
// deferring. Call with b.mu held; it reads only the store.
func (b *Bridge) downloadAlreadyPromoted(ctx context.Context, key browserDownloadKey, filename string, adoptErr error) bool {
	if !errors.Is(adoptErr, fs.ErrNotExist) {
		return false
	}
	row, err := b.jobs.Get(ctx, key.JobID)
	if err != nil || row == nil || (row.State != job.StateReady && row.State != job.StateImported) {
		return false
	}
	candidate, err := b.jobs.GetCandidate(ctx, row.SelectedCandidateID)
	if err != nil || candidate == nil || candidate.JobID != key.JobID || candidate.Source != "browser" ||
		candidate.Status != job.CandidateAccepted || !strings.HasPrefix(candidate.URLKey, "browser-adopt:sha256:") {
		return false
	}
	artifactPath, err := b.svc.Artifacts.ArtifactPath(row.ArtifactSHA256)
	if err != nil {
		return false
	}
	if info, err := os.Stat(artifactPath); err != nil || !info.Mode().IsRegular() {
		return false
	}
	var promoted bool
	err = b.jobs.S.DB().QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM events s
		WHERE s.job_id = ?1 AND s.kind = 'browser.download_started'
		AND json_extract(s.detail_json, '$.download_id') = ?2
		AND json_extract(s.detail_json, '$.filename') = ?3
		AND EXISTS (
			SELECT 1 FROM events a WHERE a.job_id = s.job_id AND a.kind = 'job.transition'
			AND a.seq > s.seq AND json_extract(a.detail_json, '$.reason') = 'adopt_browser_download'
		)
		AND (
			SELECT r.seq FROM events r WHERE r.job_id = s.job_id AND r.kind = 'job.transition'
			AND r.seq > s.seq AND json_extract(r.detail_json, '$.from') = ?4
			ORDER BY r.seq LIMIT 1
		) = (
			SELECT MAX(r.seq) FROM events r WHERE r.job_id = s.job_id AND r.kind = 'job.transition'
			AND json_extract(r.detail_json, '$.from') = ?4 AND json_extract(r.detail_json, '$.to') = ?5
		)
	)`, key.JobID, key.DownloadID, filename, job.StateValidating, job.StateReady).Scan(&promoted)
	if err != nil {
		log.Printf("papio: checking promoted browser download: %v", err)
		return false
	}
	return promoted
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
