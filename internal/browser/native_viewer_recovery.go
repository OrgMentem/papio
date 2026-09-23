// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"papio/internal/job"
)

const (
	// nativeViewerRecoveryBudget bounds durable deferred publication attempts
	// for one admitted stage before its occupancy is released with an
	// explanation. A hung adoption root (TCC) is skipped, not counted.
	nativeViewerRecoveryBudget = 20
	nativeViewerRecoveryIO     = 30 * time.Second
	nativeViewerRecoveryMax    = 5 * time.Minute
)

type nativeViewerRecovery struct {
	mu   sync.Mutex
	next map[string]nativeViewerRecoveryPace
}

type nativeViewerRecoveryPace struct {
	at   time.Time
	wait time.Duration
}

// recoverNativeViewerStages resumes publication of admitted native saves whose
// worker is gone: a publication failure, a crash after admission, or a crash
// inside publication. It works only from durable admission receipts and exact
// daemon-owned names. It never prepares, advances or closes a native helper,
// never reserves or begins a step, and never settles a permit it did not
// authenticate as this operation's own.
func (b *Bridge) recoverNativeViewerStages(ctx context.Context) {
	if b.jobs == nil || b.adoptionLatchUnhealthy() {
		return
	}
	if !b.nativeViewerRecovery.mu.TryLock() {
		return
	}
	defer b.nativeViewerRecovery.mu.Unlock()
	stages, err := b.jobs.NativeViewerStagedAdmissions(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("papio: listing admitted native viewer saves: %v", err)
		}
		return
	}
	for _, s := range stages {
		if ctx.Err() != nil {
			return
		}
		b.recoverNativeViewerStage(ctx, s)
	}
}

type nativeViewerRecoveryFS struct {
	retained string
}

func (b *Bridge) recoverNativeViewerStage(ctx context.Context, s job.NativeViewerStagedAdmission) {
	r := s.Reservation
	filename := r.Filename()
	b.mu.Lock()
	live := b.nativeViewerSaves[r.JobID]
	// Only a running worker owns the stage. An admitted record left idle and
	// non-terminal (published, validation pending) has no worker behind it.
	owned := live != nil && live.record.OperationID == r.OperationID && live.busy
	gate, cfg, now := b.nativeGate(), b.cfg, b.now()
	b.mu.Unlock()
	if owned || now.Before(b.nativeViewerRecovery.next[r.OperationID].at) || s.JobState == job.StateValidating {
		return
	}
	run := func(fn func(context.Context, *nativeDownloadRoot) (nativeViewerRecoveryFS, error)) (nativeViewerRecoveryFS, error) {
		return boundedNativeIO(ctx, gate, nativeViewerRecoveryIO, func(ctx context.Context) (nativeViewerRecoveryFS, error) {
			root, err := openNativeDownloadRoot(cfg)
			if err != nil {
				return nativeViewerRecoveryFS{}, err
			}
			defer root.close()
			result, err := fn(ctx, root)
			if _, lerr := root.landing.Lstat(s.StageName()); lerr == nil {
				result.retained = filepath.Join(root.landingPath, s.StageName())
			}
			return result, err
		}, func(nativeViewerRecoveryFS) {})
	}
	if !s.Adoptable || s.DeferredAttempts >= nativeViewerRecoveryBudget {
		reason := job.NativeViewerAuthorityLost
		if s.Adoptable {
			reason = job.NativeViewerPublicationFailed
		}
		// Look only, so the explanation names a stage that still exists. An
		// unreadable root must not strand occupancy: release without a name.
		fs, err := run(func(context.Context, *nativeDownloadRoot) (nativeViewerRecoveryFS, error) {
			return nativeViewerRecoveryFS{}, nil
		})
		if errors.Is(err, errNativeIOBusy) || ctx.Err() != nil {
			return
		}
		b.abandonNativeViewerStage(ctx, s, reason, fs.retained)
		return
	}
	fs, err := run(func(ctx context.Context, root *nativeDownloadRoot) (nativeViewerRecoveryFS, error) {
		// The stage stays until adoption reaches a conclusive outcome: the
		// final name can still be replaced before ingest reopens it.
		if err := root.resumePublication(ctx, r.JobID, filename, s.StageName(), s.SHA256, s.SizeBytes); err != nil {
			return nativeViewerRecoveryFS{}, err
		}
		return nativeViewerRecoveryFS{}, nil
	})
	switch {
	case errors.Is(err, errNativeStageMissing):
		b.abandonNativeViewerStage(ctx, s, job.NativeViewerStageMissing, fs.retained)
		return
	case errors.Is(err, errNativeStageRejected):
		b.abandonNativeViewerStage(ctx, s, job.NativeViewerStageRejected, fs.retained)
		return
	case errors.Is(err, errNativePublicationBlocked):
		b.abandonNativeViewerStage(ctx, s, job.NativeViewerPublicationBlocked, fs.retained)
		return
	case errors.Is(err, errNativeIOBusy) || ctx.Err() != nil:
		return // another native filesystem call owns the gate; not a failure
	case err != nil:
		b.deferNativeViewerStage(ctx, s, err)
		return
	}
	_, ingestErr := b.ingestAdoptedFile(ctx, r.JobID, filename, nil, &r.Producer)
	outcome, reason := b.nativeViewerOutcome(ctx, &nativeViewerSave{record: r, digest: s.SHA256}, ingestErr == nil)
	if ingestErr != nil && (outcome == "" || outcome == "pending") {
		b.deferNativeViewerStage(ctx, s, ingestErr)
		return
	}
	delete(b.nativeViewerRecovery.next, r.OperationID)
	if outcome == "" || outcome == "pending" {
		return
	}
	// Adoption now owns the admitted bytes, as after the original worker's
	// ingest; the stage is removed only while it still is their exact copy.
	if _, err := run(func(ctx context.Context, root *nativeDownloadRoot) (nativeViewerRecoveryFS, error) {
		if _, err := root.verifyStage(ctx, s.StageName(), s.SHA256, s.SizeBytes); err == nil {
			_ = root.landing.Remove(s.StageName())
		}
		return nativeViewerRecoveryFS{}, nil
	}); err != nil && ctx.Err() == nil {
		log.Printf("papio: removing adopted native viewer stage %s: %v", r.OperationID, err)
	}
	b.finishLiveNativeViewer(r, outcome, reason)
}

// finishLiveNativeViewer makes an idle in-process record of the same operation
// terminal, so later extension polls read durable truth and the record can be
// evicted. It is a reply cache, never authority; a running worker is untouched.
func (b *Bridge) finishLiveNativeViewer(r job.NativeViewerSaveReservation, outcome, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := b.nativeViewerSaves[r.JobID]
	if live == nil || live.record.OperationID != r.OperationID || live.busy {
		return
	}
	if live.cancel != nil {
		live.cancel()
	}
	live.terminal, live.outcome, live.reason = true, outcome, reason
}

// deferNativeViewerStage records the existing durable deferred-adoption event
// (and, under the TCC latch, its downloads_access_required action) and paces
// the next attempt. The durable count, not this pacing, bounds retries.
func (b *Bridge) deferNativeViewerStage(ctx context.Context, s job.NativeViewerStagedAdmission, cause error) {
	if err := b.recordAdoptionDeferred(ctx, s.Reservation.JobID, s.Reservation.Filename(), fmt.Errorf("admitted native viewer save not yet published: %w", cause)); err != nil {
		log.Printf("papio: recording deferred native viewer publication: %v", err)
	}
	if b.nativeViewerRecovery.next == nil {
		b.nativeViewerRecovery.next = map[string]nativeViewerRecoveryPace{}
	}
	pace := b.nativeViewerRecovery.next[s.Reservation.OperationID]
	pace.wait = min(max(2*pace.wait, 2*time.Second), nativeViewerRecoveryMax)
	pace.at = b.now().Add(pace.wait)
	b.nativeViewerRecovery.next[s.Reservation.OperationID] = pace
}

// abandonNativeViewerStage releases the exact operation's occupancy with a
// durable explanation. The stage file, the open action and the latch stay.
func (b *Bridge) abandonNativeViewerStage(ctx context.Context, s job.NativeViewerStagedAdmission, reason, retained string) {
	r := s.Reservation
	if err := b.jobs.AbandonNativeViewerStagedAdmission(ctx, r.JobID, r.OperationID, reason, retained); err != nil {
		if !errors.Is(err, job.ErrEffectPermitStale) {
			log.Printf("papio: releasing admitted native viewer save %s: %v", r.OperationID, err)
		}
		return
	}
	delete(b.nativeViewerRecovery.next, r.OperationID)
	b.finishLiveNativeViewer(r, "unavailable", "source_rejected")
	log.Printf("papio: admitted native viewer save %s for job %s cannot be published (%s); occupancy released, bytes kept", r.OperationID, r.JobID, reason)
}
