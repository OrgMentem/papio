// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"fmt"

	"papio/internal/app"
	"papio/internal/job"
)

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
	if fence.digest == "" || row.ArtifactSHA256 != fence.digest {
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
	if err := b.svc.Artifacts.Verify(fence.digest); err != nil {
		return 0, err
	}
	// A late frame cannot mint its own filename+SHA authority. Recover before
	// persistArtifactCorrelation, and settle only that exact historical effect;
	// do not commit a new winner or settle whichever claim is currently live.
	producer, err := b.recoverArtifactProducerExact(ctx, jobID, filename, fence.digest)
	if err != nil {
		return 0, err
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
