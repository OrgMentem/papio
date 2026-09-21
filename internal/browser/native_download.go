// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// A reservation retains local observations, not acquisition authority. Its
// one-shot arm/admission is recorded in existing job events. No baseline can
// be reconstructed after restart, so loss is conservative and never replayed.
type nativeDownloadReservation struct {
	record          job.NativeDownloadReservation
	arm             protocol.NativeDownloadArmRequestV1Payload
	sessionID       string
	root            *nativeDownloadRoot
	observation     string
	digest          string
	outcome, reason string
	busy            bool
	rebindRequest   string // fingerprint of the still-current successful transfer
}

func (b *Bridge) currentGenericDriveAuthority(ctx context.Context, sessionID, jobID, attemptID string, ordinal int64, strategy, revision string) (*job.EffectPermit, bool) {
	holder := b.arbitration.holderSession()
	if strategy != "generic" || holder == nil || holder.ID != sessionID || holder.Outdated || b.now().Sub(holder.LastSyncAt) > sessionStaleAfter || !b.effectPermitAvailable() || !b.providerDriveEpochAvailable() {
		return nil, false
	}
	permit, err := b.jobs.GetEffectPermitByIdentity(ctx, job.EffectPermitIdentity{JobID: jobID, Kind: job.GenericDrive, DriveAttemptID: attemptID, Ordinal: ordinal, Strategy: strategy, Revision: revision})
	if err != nil || permit == nil || permit.Status != job.Held || permit.BrowserHolderGeneration != b.arbitration.generation() || permit.LeaseUntil == nil || !permit.LeaseUntil.After(b.now()) {
		return nil, false
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil || attempt != permit.JobAttemptRevision {
		return nil, false
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return nil, false
	}
	tuple := providerDriveEpochKey(attemptID, ordinal, strategy, revision)
	current, started, applied, superseded, domain := providerDriveEpochState(events, tuple)
	if current != tuple || !started || applied || superseded || domain != permit.SafetyDomainID {
		return nil, false
	}
	authorized, err := b.providerDriveEpochAuthorized(ctx, jobID, domain)
	return permit, err == nil && authorized
}

var errNativeIOBusy = errors.New("native download filesystem busy")

func nativeDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func nativeBinding(sessionID, jobID string, p *protocol.NativeDownloadArmRequestV1Payload) string {
	copy := *p
	copy.RequestID = ""
	return nativeDigest(struct {
		Session, Job string
		Binding      protocol.NativeDownloadArmRequestV1Payload
	}{sessionID, jobID, copy})
}
func (b *Bridge) nativeAvailable(sessionID string) bool {
	h := b.arbitration.holderSession()
	return !b.agentClosed && h != nil && h.ID == sessionID && !h.Outdated && slices.Contains(h.Features, protocol.NativeClickAdoptionFeature)
}
func (b *Bridge) nativeAuthority(ctx context.Context, sessionID, jobID string, p protocol.ArtifactProducerPayload) (*job.EffectPermit, bool) {
	if !b.nativeAvailable(sessionID) || p.Ordinal == nil || p.EffectKind != "generic_drive" {
		return nil, false
	}
	return b.currentGenericDriveAuthority(ctx, sessionID, jobID, p.DriveAttemptID, *p.Ordinal, p.Strategy, p.Revision)
}

// boundedNativeIO is single-flight for the lifetime of the underlying syscall,
// not merely for the caller's wait. A timed-out worker can clean up its result,
// but never admits source bytes. Only previously admitted staging may be
// published by a worker. cleanup owns late handles.
func boundedNativeIO[T any](parent context.Context, gate chan struct{}, budget time.Duration, run func(context.Context) (T, error), cleanup func(T)) (T, error) {
	var zero T
	select {
	case gate <- struct{}{}:
	default:
		return zero, errNativeIOBusy
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	done := make(chan result)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { <-gate }()
		value, err := run(ctx)
		select {
		case done <- result{value, err}:
		case <-ctx.Done():
			cleanup(value)
		}
	}()
	select {
	case r := <-done:
		<-finished // the next stage may acquire the same gate immediately
		return r.value, r.err
	case <-ctx.Done():
		return zero, ErrAdoptionScanTimeout
	}
}
func (b *Bridge) nativeGate() chan struct{} {
	if b.nativeIOGate == nil {
		b.nativeIOGate = make(chan struct{}, 1)
	}
	return b.nativeIOGate
}

func (b *Bridge) armNativeDownload(ctx context.Context, sessionID, jobID string, p *protocol.NativeDownloadArmRequestV1Payload) ([]json.RawMessage, error) {
	result := protocol.NativeDownloadArmResultV1Payload{RequestID: p.RequestID, Outcome: "stale", Reason: "authority_lost"}
	reply := func() ([]json.RawMessage, error) {
		f, e := b.frame(protocol.MsgNativeDownloadArmResultV1, jobID, result)
		if e != nil {
			return nil, e
		}
		return []json.RawMessage{f}, nil
	}
	permit, ok := b.nativeAuthority(ctx, sessionID, jobID, p.Producer)
	if !ok {
		return reply()
	}
	// Evict only idle observations. Expired events remain permanent arm latches.
	for id, r := range b.nativeDownloads {
		if !r.busy && (r.record.ExpiresAtMS <= b.now().UnixMilli() || r.record.HolderGeneration != b.arbitration.generation()) {
			root := r.root
			delete(b.nativeDownloads, id)
			if root != nil {
				go root.close()
			}
		}
	}
	binding := nativeBinding(sessionID, jobID, p)
	if prior := b.nativeDownloads[jobID]; prior != nil {
		if prior.arm.RequestID == p.RequestID && nativeBinding(sessionID, jobID, &prior.arm) == binding && prior.record.BindingSHA256 == binding && prior.root != nil && prior.observation == "" && !prior.busy {
			result.Outcome = "armed"
			result.Reason = ""
			result.ReservationID = prior.record.ReservationID
			result.ExpiresAtMS = prior.record.ExpiresAtMS
		} else {
			result.Outcome = "refused"
			result.Reason = "already_armed"
		}
		return reply()
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		result.Outcome = "unavailable"
		result.Reason = "unavailable"
		return reply()
	}
	for _, ev := range events {
		d, _ := ev["detail"].(map[string]any)
		if ev["kind"] == "browser.native_download_reserved" && stringDetail(d, "permit_id") == permit.ID {
			result.Outcome = "refused"
			result.Reason = "already_armed"
			return reply()
		}
	}
	// Keep duplicate receipts while space remains, but do not let completed
	// work occupy every live slot. Eviction removes only the reply cache;
	// durable reservation/admission events still prohibit replay.
	if len(b.nativeDownloads) >= maxOutstandingOffers {
		for id, r := range b.nativeDownloads {
			if r.busy || r.root != nil || r.observation == "" {
				continue
			}
			switch r.outcome {
			case "ready", "review", "rejected", "refused", "stale":
				delete(b.nativeDownloads, id)
			default:
				continue
			}
			break // retain all but the single slot needed by this arm
		}
	}
	if len(b.nativeDownloads) >= maxOutstandingOffers {
		result.Outcome = "unavailable"
		result.Reason = "source_busy"
		return reply()
	}
	gate, cfg := b.nativeGate(), b.cfg
	b.mu.Unlock()
	root, err := boundedNativeIO(ctx, gate, AdoptionScanDeadline, func(ctx context.Context) (*nativeDownloadRoot, error) { return snapshotNativeDownloadRoot(ctx, cfg) }, func(r *nativeDownloadRoot) { r.close() })
	b.mu.Lock()
	if err != nil {
		result.Outcome = "unavailable"
		result.Reason = "source_rejected"
		if errors.Is(err, ErrAdoptionScanTimeout) || errors.Is(err, errNativeIOBusy) {
			result.Reason = "source_busy"
		}
		return reply()
	}
	current, ok := b.nativeAuthority(ctx, sessionID, jobID, p.Producer)
	if !ok || current.ID != permit.ID {
		go root.close()
		return reply()
	}
	now := b.now()
	expires := now.Add(agentAttemptDuration)
	if permit.LeaseUntil.Before(expires) {
		expires = *permit.LeaseUntil
	}
	record := job.NativeDownloadReservation{ReservationID: job.NewID("native"), PermitID: permit.ID, JobID: jobID, Producer: *artifactProducerIdentity(&p.Producer), JobAttemptRevision: permit.JobAttemptRevision, HolderGeneration: permit.BrowserHolderGeneration, BindingSHA256: binding, ArmedAtMS: now.UnixMilli(), ExpiresAtMS: expires.UnixMilli()}
	if err := b.jobs.ReserveNativeDownload(ctx, record, now); err != nil {
		go root.close()
		result.Outcome = "refused"
		result.Reason = "already_armed"
		return reply()
	}
	if b.nativeDownloads == nil {
		b.nativeDownloads = map[string]*nativeDownloadReservation{}
	}
	b.nativeDownloads[jobID] = &nativeDownloadReservation{record: record, arm: *p, sessionID: sessionID, root: root}
	result.Outcome = "armed"
	result.Reason = ""
	result.ReservationID = record.ReservationID
	result.ExpiresAtMS = record.ExpiresAtMS
	return reply()
}

// Called under b.mu, like arm/import. No filesystem work or unlock occurs: an
// import either consumes its observation first, or sees the transferred binding.
func (b *Bridge) rebindNativeDownload(ctx context.Context, sessionID, jobID string, p *protocol.NativeDownloadRebindRequestV1Payload) ([]json.RawMessage, error) {
	result := protocol.NativeDownloadRebindResultV1Payload{RequestID: p.RequestID, ReservationID: p.ReservationID, Outcome: "stale", Reason: "authority_lost"}
	reply := func() ([]json.RawMessage, error) {
		f, err := b.frame(protocol.MsgNativeDownloadRebindResultV1, jobID, result)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{f}, nil
	}
	if !b.nativeAvailable(sessionID) {
		return reply()
	}
	peerFeatures := b.arbitration.holderSession().Features
	if b.agentBackend == nil || !slices.Contains(peerFeatures, agentFallbackFeature) || !slices.Contains(peerFeatures, protocol.AgentNavigationFeature) {
		result.Outcome, result.Reason = "unavailable", "unavailable"
		return reply()
	}
	permit, ok := b.nativeAuthority(ctx, sessionID, jobID, p.Producer)
	r := b.nativeDownloads[jobID]
	if !ok || r == nil || r.sessionID != sessionID || r.record.ReservationID != p.ReservationID || r.record.PermitID != permit.ID || r.record.HolderGeneration != b.arbitration.generation() {
		return reply()
	}
	if r.record.ExpiresAtMS <= b.now().UnixMilli() {
		result.Reason = "expired"
		return reply()
	}
	if r.busy || r.observation != "" || r.root == nil {
		result.Outcome, result.Reason = "refused", "already_armed"
		if r.busy {
			result.Reason = "source_busy"
		}
		return reply()
	}
	old := protocol.NativeDownloadArmRequestV1Payload{Producer: p.Producer, BrowserEpoch: p.BrowserEpoch, DocumentID: p.DocumentID}
	next := old
	next.DocumentID = p.NextDocumentID
	oldBinding, nextBinding := nativeBinding(sessionID, jobID, &old), nativeBinding(sessionID, jobID, &next)
	fingerprint := nativeDigest(p)
	if oldBinding == nextBinding || (r.record.BindingSHA256 != oldBinding && (r.record.BindingSHA256 != nextBinding || r.rebindRequest != fingerprint)) {
		return reply()
	}
	previous := r.record
	previous.BindingSHA256 = oldBinding
	if err := b.jobs.RebindNativeDownload(ctx, previous, nextBinding, p.RequestID, b.now()); err != nil {
		switch {
		case errors.Is(err, job.ErrNativeDownloadReserved):
			result.Outcome, result.Reason = "refused", "already_armed"
		case errors.Is(err, job.ErrEffectPermitStale):
		default:
			result.Outcome, result.Reason = "unavailable", "unavailable"
		}
		return reply()
	}
	r.record.BindingSHA256 = nextBinding
	r.rebindRequest = fingerprint
	result.Outcome, result.Reason = "rebound", ""
	return reply()
}

type nativeStageResult struct {
	root *nativeDownloadRoot
	file nativeStagedFile
}

func cleanupNativeStage(r nativeStageResult) {
	if r.root != nil {
		if r.file.name != "" {
			_ = r.root.landing.Remove(r.file.name)
		}
		r.root.close()
	}
}

func (b *Bridge) importNativeDownload(ctx context.Context, sessionID, jobID string, p *protocol.NativeDownloadImportRequestV1Payload) ([]json.RawMessage, error) {
	result := protocol.NativeDownloadImportResultV1Payload{RequestID: p.RequestID, ReservationID: p.ReservationID, DownloadID: p.DownloadID, Outcome: "stale", Reason: "authority_lost"}
	reply := func() ([]json.RawMessage, error) {
		f, e := b.frame(protocol.MsgNativeDownloadImportResultV1, jobID, result)
		if e != nil {
			return nil, e
		}
		return []json.RawMessage{f}, nil
	}
	r := b.nativeDownloads[jobID]
	arm := protocol.NativeDownloadArmRequestV1Payload{Producer: p.Producer, BrowserEpoch: p.BrowserEpoch, DocumentID: p.DocumentID}
	if r == nil || r.record.ReservationID != p.ReservationID || r.sessionID != sessionID || r.record.BindingSHA256 != nativeBinding(sessionID, jobID, &arm) || !b.nativeAvailable(sessionID) || r.record.HolderGeneration != b.arbitration.generation() {
		return reply()
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil || attempt != r.record.JobAttemptRevision {
		return reply()
	}
	observed := *p
	observed.RequestID = ""
	fingerprint := nativeDigest(observed)
	if r.observation != "" {
		if r.observation == fingerprint {
			if !r.busy && r.outcome == "deferred" && r.digest != "" {
				b.mu.Unlock()
				outcome, reason := b.nativeImportOutcome(ctx, jobID, r.digest, false)
				b.mu.Lock()
				if !b.nativeAvailable(sessionID) || r.record.HolderGeneration != b.arbitration.generation() {
					return reply()
				}
				if outcome != "" {
					r.outcome, r.reason = outcome, reason
				}
			}
			result.Outcome = r.outcome
			result.Reason = r.reason
			if r.busy {
				result.Outcome = "deferred"
				result.Reason = "source_busy"
			}
		}
		return reply()
	}
	permit, ok := b.nativeAuthority(ctx, sessionID, jobID, p.Producer)
	if !ok || permit.ID != r.record.PermitID || r.root == nil || r.record.ExpiresAtMS <= b.now().UnixMilli() || p.StartedAtMS < r.record.ArmedAtMS || p.StartedAtMS > b.now().UnixMilli() {
		return reply()
	}
	r.observation = fingerprint
	r.busy = true
	r.outcome = "refused"
	r.reason = "source_rejected"
	root := r.root
	r.root = nil // worker exclusively owns the handles until it returns
	gate, maxBytes := b.nativeGate(), b.cfg.Fetch.MaxBytes
	b.mu.Unlock()
	stage, err := boundedNativeIO(ctx, gate, 5*time.Second, func(ctx context.Context) (nativeStageResult, error) {
		f, e := root.stage(ctx, p.SourcePath, r.record.ReservationID, p.SizeBytes, maxBytes)
		if e != nil {
			root.close()
			return nativeStageResult{}, e
		}
		return nativeStageResult{root, f}, nil
	}, cleanupNativeStage)
	b.mu.Lock()
	r.busy = false
	if err != nil {
		if errors.Is(err, errNativeIOBusy) {
			go root.close()
		}
		result.Outcome = r.outcome
		result.Reason = r.reason
		return reply()
	}
	defer func() { go cleanupNativeStage(stage) }()
	permit, ok = b.nativeAuthority(ctx, sessionID, jobID, p.Producer)
	if !ok || permit.ID != r.record.PermitID {
		r.outcome = "stale"
		r.reason = "authority_lost"
		return reply()
	}
	filename := r.record.ReservationID + ".pdf"
	admission := job.NativeDownloadAdmission{NativeDownloadReservation: r.record, DownloadID: p.DownloadID, StartedAtMS: p.StartedAtMS, ObservationSHA256: fingerprint, Filename: filename, SHA256: stage.file.digest, SizeBytes: stage.file.size}
	if err := b.jobs.AdmitNativeDownload(ctx, admission, b.now()); err != nil {
		r.outcome = "stale"
		r.reason = "authority_lost"
		return reply()
	}
	// This is the durable admission boundary. From here only daemon-created,
	// exact digest-correlated staging bytes are used; never re-open the source.
	r.digest = stage.file.digest
	r.busy = true
	r.outcome = "deferred"
	r.reason = "validation_pending"
	b.mu.Unlock()
	publishing := stage
	stage = nativeStageResult{} // the bounded worker owns cleanup on timeout
	stage, err = boundedNativeIO(ctx, gate, 5*time.Second, func(ctx context.Context) (nativeStageResult, error) {
		return publishing, publishing.root.publish(ctx, jobID, filename, publishing.file)
	}, cleanupNativeStage)
	if errors.Is(err, errNativeIOBusy) {
		cleanupNativeStage(publishing)
	}
	if err == nil {
		_, err = b.ingestAdoptedFile(ctx, jobID, filename, nil, &r.record.Producer)
	}
	finalOutcome, finalReason := b.nativeImportOutcome(ctx, jobID, r.digest, err == nil)
	b.mu.Lock()
	r.busy = false
	if finalOutcome != "" {
		r.outcome = finalOutcome
		r.reason = finalReason
	}
	result.Outcome = r.outcome
	result.Reason = r.reason
	return reply()
}

// A deferred receipt only reads daemon-owned evidence; it never reopens the
// browser source. Exact artifact/candidate identity is required for success.
func (b *Bridge) nativeImportOutcome(ctx context.Context, jobID, digest string, validated bool) (string, string) {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return "", ""
	}
	switch row.State {
	case job.StateReady:
		candidate, err := b.jobs.GetCandidate(ctx, row.SelectedCandidateID)
		if err == nil && candidate != nil && candidate.Source == "browser" && candidate.Status == job.CandidateAccepted && candidate.URLKey == "browser-adopt:sha256:"+digest && row.ArtifactSHA256 == digest && b.svc.Artifacts.Verify(digest) == nil {
			return "ready", ""
		}
	case job.StateNeedsReview:
		candidate, err := b.jobs.GetCandidate(ctx, row.SelectedCandidateID)
		if validated || (err == nil && candidate != nil && candidate.Source == "browser" && candidate.URLKey == "browser-adopt:sha256:"+digest) {
			return "review", "invalid_pdf"
		}
	case job.StateAwaitingHuman:
		if validated {
			return "rejected", "invalid_pdf"
		}
	case job.StateCancelled, job.StateFailed:
		return "stale", "authority_lost"
	}
	return "", ""
}

// Called under the session lock. Busy workers exclusively own their handles;
// their post-copy authority check fences any departed holder before admission.
func (b *Bridge) retireNativeDownloads(sessionID string) {
	for id, r := range b.nativeDownloads {
		if b.agentClosed || r.sessionID == sessionID || r.record.HolderGeneration != b.arbitration.generation() {
			delete(b.nativeDownloads, id)
			if r.root != nil {
				go r.root.close()
			}
		}
	}
}
