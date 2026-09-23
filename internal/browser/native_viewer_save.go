// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"papio/internal/job"
	"papio/internal/nativeviewer"
	"papio/internal/protocol"
)

const nativeViewerLifetime = 120 * time.Second

type nativeViewerReceipt struct {
	fingerprint     string
	outcome, reason string
}

// All fields are protected by b.mu. A busy worker exclusively owns the root
// and helper; retirement cancels it without closing handles under its feet.
type nativeViewerSave struct {
	lastDispatch                    time.Time
	record                          job.NativeViewerSaveReservation
	sessionID, binding              string
	ctx                             context.Context
	cancel                          context.CancelFunc
	root                            *nativeDownloadRoot
	helper                          nativeviewer.Session
	busy, saved, admitted, terminal bool
	digest, outcome, reason         string
	stable                          os.FileInfo
	receipts                        map[string]nativeViewerReceipt
}

func (b *Bridge) SetNativeViewerDriver(driver nativeviewer.Driver) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.nativeViewerSaves {
		b.stopNativeViewer(r, "stale", "authority_lost")
	}
	b.nativeViewerDriver = driver
}

func nativeViewerBinding(sessionID, jobID string, p *protocol.NativeViewerSaveRequestV1Payload) string {
	return nativeDigest(struct {
		Session, Job         string
		Action, Revision     int64
		Epoch, Document, URL string
	}{
		sessionID, jobID, p.ActionID, p.ActionRevision, p.BrowserEpoch, p.DocumentID, p.SourceURL,
	})
}

func (b *Bridge) nativeViewerAvailable(sessionID string) bool {
	h := b.arbitration.holderSession()
	return !b.agentClosed && b.nativeViewerDriver != nil && h != nil && h.ID == sessionID && !h.Outdated &&
		b.now().Sub(h.LastSyncAt) <= sessionStaleAfter && slices.Contains(h.Features, protocol.NativeViewerSaveFeature) && slices.Contains(h.Features, nativeViewerDownloadFeature)
}
func (b *Bridge) nativeViewerCurrent(ctx context.Context, r *nativeViewerSave) bool {
	return r.ctx.Err() == nil && !r.terminal && b.nativeViewerSaves[r.record.JobID] == r &&
		b.nativeViewerAvailable(r.sessionID) && r.record.HolderGeneration == b.arbitration.generation() &&
		b.jobs.CheckNativeViewerSave(ctx, r.record, b.now()) == nil
}

// stopNativeViewer is called under b.mu. Cleanup never touches arbitrary native
// surfaces: only the session created for this operation can close its own dialog.
func (b *Bridge) stopNativeViewer(r *nativeViewerSave, outcome, reason string) {
	r.cancel()
	r.terminal, r.outcome, r.reason = true, outcome, reason
	// Cancellation and Begin serialize in the authority transaction. A worker
	// that has begun anything retains unknown occupancy; zero steps settle.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = b.jobs.CancelNativeViewerSave(cleanupCtx, r.record, b.now())
	cleanupCancel()
	if !r.busy {
		root, helper := r.root, r.helper
		r.root, r.helper = nil, nil
		go func() {
			if helper != nil {
				_ = helper.Close()
			}
			root.close()
		}()
	}
}
func (b *Bridge) retireNativeViewerSaves(sessionID string) {
	for _, r := range b.nativeViewerSaves {
		if b.agentClosed || r.sessionID == sessionID || r.record.HolderGeneration != b.arbitration.generation() {
			b.stopNativeViewer(r, "stale", "authority_lost")
		}
	}
}

func (b *Bridge) watchNativeViewer(r *nativeViewerSave) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			b.mu.Lock()
			if !r.terminal && !r.admitted {
				b.stopNativeViewer(r, "stale", "expired")
			}
			b.mu.Unlock()
			return
		case <-ticker.C:
			b.mu.Lock()
			if r.admitted || r.terminal {
				b.mu.Unlock()
				return
			}
			current := b.nativeViewerCurrent(r.ctx, r)
			if !current {
				b.stopNativeViewer(r, "stale", "authority_lost")
			}
			b.mu.Unlock()
			if !current {
				return
			}
		}
	}
}

// Sync enters with b.mu held. Every helper/filesystem wait releases it. Stable
// request IDs are receipts, never authorization to dispatch a second effect.
func (b *Bridge) nativeViewerSaveRequest(ctx context.Context, sessionID, jobID string, p *protocol.NativeViewerSaveRequestV1Payload) ([]json.RawMessage, error) {
	result := protocol.NativeViewerSaveResultV1Payload{RequestID: p.RequestID, OperationID: p.OperationID, Outcome: "stale", Reason: "authority_lost"}
	reply := func() ([]json.RawMessage, error) {
		frame, err := b.frame(protocol.MsgNativeViewerSaveResultV1, jobID, result)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{frame}, nil
	}
	if !b.nativeViewerAvailable(sessionID) {
		result.Outcome, result.Reason = "unavailable", "unavailable"
		return reply()
	}
	binding, fingerprint := nativeViewerBinding(sessionID, jobID, p), nativeDigest(p)
	r := b.nativeViewerSaves[jobID]
	if r != nil && p.Step == "prepare" && r.terminal && !r.busy && (r.record.ActionID != p.ActionID || r.record.ActionRevision != p.ActionRevision) {
		delete(b.nativeViewerSaves, jobID)
		r = nil
	}
	if r != nil {
		if r.binding != binding || r.sessionID != sessionID || (p.Step != "prepare" && r.record.OperationID != p.OperationID) {
			if r.sessionID == sessionID && r.record.OperationID == p.OperationID && !r.admitted {
				b.stopNativeViewer(r, "stale", "document_changed")
			}
			result.Reason = "document_changed"
			return reply()
		}
		result.OperationID = r.record.OperationID
		if r.record.HolderGeneration != b.arbitration.generation() {
			return reply()
		}
		if p.Step == "cancel" {
			b.stopNativeViewer(r, "stale", "authority_lost")
			return reply()
		}
		if r.terminal {
			result.Outcome, result.Reason = r.outcome, r.reason
			return reply()
		}
		if cached, ok := r.receipts[p.RequestID]; ok {
			if cached.fingerprint != fingerprint {
				return reply()
			}
			result.Outcome, result.Reason = cached.outcome, cached.reason
			return reply()
		}
		if p.Step == "prepare" {
			result.Outcome, result.Reason = "refused", "already_started"
			return reply()
		}
		if r.busy {
			result.Outcome, result.Reason = "pending", "source_busy"
			return reply()
		}
		if r.admitted {
			r.busy = true
			b.mu.Unlock()
			outcome, reason := b.nativeViewerOutcome(ctx, r, false)
			b.mu.Lock()
			r.busy = false
			if outcome != "" {
				r.outcome, r.reason = outcome, reason
				r.terminal = outcome != "pending"
			}
			result.Outcome, result.Reason = r.outcome, r.reason
			return reply()
		}
		if !b.nativeViewerCurrent(ctx, r) {
			b.stopNativeViewer(r, "stale", "authority_lost")
			return reply()
		}
	} else {
		if p.Step != "prepare" {
			return reply()
		}
		// Only terminal receipts can be discarded. The durable action-revision latch
		// still refuses a second native operation after eviction or daemon restart.
		for id, old := range b.nativeViewerSaves {
			if !old.busy && old.terminal && old.record.ExpiresAtMS <= b.now().UnixMilli() {
				delete(b.nativeViewerSaves, id)
			}
		}
		if len(b.nativeViewerSaves) >= maxOutstandingOffers {
			result.Outcome, result.Reason = "unavailable", "source_busy"
			return reply()
		}
		generation, driver, cfg, gate := b.arbitration.generation(), b.nativeViewerDriver, b.cfg, b.nativeGate()
		operationID := job.NewID("viewer")
		deadline := b.now().Add(nativeViewerLifetime)
		// Prepare is observation only. Failure here must not consume the
		// action revision; the durable latch is committed before first Advance.
		b.mu.Unlock()
		prepared, err := boundedNativeIO(ctx, gate, 15*time.Second, func(ctx context.Context) (nativeViewerPrepared, error) {
			root, err := snapshotNativeDownloadRoot(ctx, cfg)
			if err != nil {
				return nativeViewerPrepared{}, err
			}
			helper, err := driver.Prepare(ctx, nativeviewer.Request{URL: p.SourceURL, Filename: "papio-viewer-" + operationID + ".pdf", Deadline: deadline})
			if err != nil {
				root.close()
				if helper != nil {
					_ = helper.Close()
				}
				return nativeViewerPrepared{}, err
			}
			return nativeViewerPrepared{root, helper}, nil
		}, cleanupNativeViewerPrepared)
		b.mu.Lock()
		if err != nil {
			result.Outcome, result.Reason = "unavailable", "native_failed"
			return reply()
		}
		reject := func() { go cleanupNativeViewerPrepared(prepared) }
		if !b.nativeViewerAvailable(sessionID) || generation != b.arbitration.generation() || b.nativeViewerSaves[jobID] != nil {
			reject()
			return reply()
		}
		attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
		if err != nil {
			reject()
			return reply()
		}
		// The native helper just checked this exact retained viewer. Its host
		// names the safety domain; no stale provider epoch is revived.
		domain := nativeViewerSafetyDomain(p.SourceURL)
		now := b.now()
		record, err := b.jobs.ReserveNativeViewerSave(ctx, job.NativeViewerSaveInput{ExplicitSelection: p.Selection == "explicit", OperationID: operationID, RequestID: p.RequestID, JobID: jobID, ActionID: p.ActionID, ActionRevision: p.ActionRevision, JobAttemptRevision: attempt, HolderGeneration: generation, BindingSHA256: binding, SafetyDomainID: domain, ExpiresAtMS: deadline.UnixMilli()}, now)
		if err != nil {
			reject()
			switch {
			case errors.Is(err, job.ErrNativeViewerSaveConsumed):
				result.Outcome, result.Reason = "refused", "already_started"
			case errors.Is(err, job.ErrEffectPermitBusy):
				result.Outcome, result.Reason = "unavailable", "source_busy"
			case errors.Is(err, job.ErrEffectPermitStale):
				result.Outcome, result.Reason = "stale", "authority_lost"
			default:
				result.Outcome, result.Reason = "unavailable", "unavailable"
			}
			return reply()
		}
		opctx, cancel := context.WithTimeout(context.Background(), deadline.Sub(now))
		r = &nativeViewerSave{record: record, sessionID: sessionID, binding: binding, ctx: opctx, cancel: cancel, root: prepared.root, helper: prepared.helper, outcome: "prepared", receipts: map[string]nativeViewerReceipt{p.RequestID: {fingerprint, "prepared", ""}}}
		if b.nativeViewerSaves == nil {
			b.nativeViewerSaves = map[string]*nativeViewerSave{}
		}
		b.nativeViewerSaves[jobID] = r
		result.OperationID, result.Outcome, result.Reason = record.OperationID, "prepared", ""
		go b.watchNativeViewer(r)
		return reply()
	}
	if len(r.receipts) >= 512 {
		b.stopNativeViewer(r, "refused", "expired")
		result.Outcome, result.Reason = r.outcome, r.reason
		return reply()
	}
	if !r.saved && b.now().Sub(r.lastDispatch) < 250*time.Millisecond {
		result.Outcome, result.Reason = "pending", "source_busy"
		return reply()
	}
	r.lastDispatch = b.now()
	r.busy = true
	r.receipts[p.RequestID] = nativeViewerReceipt{fingerprint, "pending", "source_busy"}
	root, helper := r.root, r.helper
	r.root, r.helper = nil, nil
	done := make(chan struct{})
	request := *p // raw URL exists only for this native dispatch, never in receipts
	// #nosec G118 -- the native save outlives this request by design: the
	// handler waits at most five seconds, and r.ctx bounds the operation to
	// nativeViewerLifetime.
	go b.runNativeViewer(r, request, root, helper, done)
	b.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	timer.Stop()
	b.mu.Lock()
	cached := r.receipts[p.RequestID]
	result.Outcome, result.Reason = cached.outcome, cached.reason
	if r.terminal {
		result.Outcome, result.Reason = r.outcome, r.reason
	}
	return reply()
}

func (b *Bridge) runNativeViewer(r *nativeViewerSave, p protocol.NativeViewerSaveRequestV1Payload, root *nativeDownloadRoot, helper nativeviewer.Session, done chan struct{}) {
	outcome, reason := "pending", ""
	saved, admitted, digest, stable := r.saved, r.admitted, r.digest, r.stable // handed off by the lock before worker start
	var staged nativeStagedFile
	published := false
	defer func() {
		b.mu.Lock()
		r.busy = false
		if !r.terminal {
			r.saved, r.admitted, r.digest, r.stable = saved, admitted, digest, stable
			r.outcome, r.reason = outcome, reason
			if outcome != "pending" && outcome != "prepared" {
				b.stopNativeViewer(r, outcome, reason)
			}
		}
		receipt := r.receipts[p.RequestID]
		receipt.outcome, receipt.reason = r.outcome, r.reason
		r.receipts[p.RequestID] = receipt
		if !r.terminal && !admitted {
			r.root, r.helper = root, helper
			root, helper = nil, nil
		}
		b.mu.Unlock()
		if helper != nil {
			_ = helper.Close()
		}
		if root != nil {
			if staged.name != "" && (!admitted || published) {
				_ = root.landing.Remove(staged.name)
			}
			root.close()
		}
		close(done)
	}()
	current := func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.nativeViewerCurrent(r.ctx, r) }
	if !current() {
		outcome, reason = "stale", "authority_lost"
		return
	}

	if !saved {
		b.mu.Lock()
		err := b.jobs.BeginNativeViewerSaveStep(r.ctx, r.record, p.RequestID, b.now())
		allowed := err == nil && b.nativeViewerCurrent(r.ctx, r)
		b.mu.Unlock()
		if !allowed {
			outcome, reason = "stale", "authority_lost"
			return
		}
		status, err := helper.Advance(r.ctx)
		if err != nil {
			outcome, reason = "unavailable", "native_failed"
			return
		}
		if !current() {
			outcome, reason = "stale", "authority_lost"
			return
		}
		if status != nativeviewer.Saved {
			return
		}
		saved = true
		_ = helper.Close()
		helper = nil
	}
	if !root.unchanged() {
		outcome, reason = "refused", "source_rejected"
		return
	}
	filename := r.record.Filename()
	if _, err := root.source.Lstat(filename + ".part"); err == nil {
		stable = nil
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		outcome, reason = "refused", "source_rejected"
		return
	}
	info, err := root.source.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		stable = nil
		return
	}
	if err != nil || !info.Mode().IsRegular() {
		outcome, reason = "refused", "source_rejected"
		return
	}
	if stable == nil || !sameAdoptionFile(stable, info) {
		stable = info
		return
	}
	staged, err = root.stage(r.ctx, filepath.Join(root.sourcePath, filename), r.record.OperationID, info.Size(), b.cfg.Fetch.MaxBytes)
	if err != nil {
		if !current() {
			outcome, reason = "stale", "authority_lost"
		}
		stable = nil
		return
	}
	b.mu.Lock()
	allowed := b.nativeViewerCurrent(r.ctx, r)
	if allowed {
		err = b.jobs.AdmitNativeViewerSave(r.ctx, r.record, filename, staged.digest, staged.size, b.now())
	} else {
		err = job.ErrEffectPermitStale
	}
	if err == nil {
		r.admitted = true
		r.digest = staged.digest
	}
	b.mu.Unlock()
	if err != nil {
		outcome, reason = "stale", "authority_lost"
		return
	}
	admitted, digest = true, staged.digest
	outcome, reason = "pending", "validation_pending"
	// Admission is the durable boundary. Cancellation of native authority
	// cannot destroy these daemon-owned bytes halfway through publication.
	// Failure preserves the exact stage (and its admission receipt), reports a
	// terminal failure, and keeps uncertain occupancy for explicit recovery.
	publishCtx, publishCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer publishCancel()
	err = root.publish(publishCtx, r.record.JobID, filename, staged)
	if err != nil {
		outcome, reason = "unavailable", "source_rejected"
		return
	}
	published = true
	_, err = b.ingestAdoptedFile(publishCtx, r.record.JobID, filename, nil, &r.record.Producer)
	if final, why := b.nativeViewerOutcome(publishCtx, r, err == nil); final != "" {
		outcome, reason = final, why
	}

}

func (b *Bridge) nativeViewerOutcome(ctx context.Context, r *nativeViewerSave, validated bool) (string, string) {
	if candidate, err := b.completedAdoption(ctx, r.record.JobID, r.record.Filename(), &artifactFence{digest: r.digest}, nil, &r.record.Producer); err == nil && candidate != 0 {
		return "ready", ""
	}
	outcome, reason := b.nativeImportOutcome(ctx, r.record.JobID, r.digest, validated)
	if outcome == "ready" {
		return "pending", "validation_pending"
	} // completedAdoption is the authoritative exact producer proof.
	return outcome, reason
}

type nativeViewerPrepared struct {
	root   *nativeDownloadRoot
	helper nativeviewer.Session
}

func cleanupNativeViewerPrepared(p nativeViewerPrepared) {
	if p.helper != nil {
		_ = p.helper.Close()
	}
	p.root.close()
}

func nativeViewerSafetyDomain(source string) string {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return ""
	}
	return "native-viewer:" + strings.ToLower(parsed.Hostname())
}
