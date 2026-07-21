// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package browser implements the daemon side of the Phase 2 ordinary-Chrome
// institutional handoff. The bridge speaks locked papio-browser/1 as a
// pull loop over daemon-owned local IPC: the extension delivers observation
// frames and the bridge returns command frames. Browser messages are strictly
// re-validated here (fail closed) and treated as observations only — they never
// authorize a job transition the core policy would forbid. PDF bytes and secrets
// never cross this boundary; only metadata, timing, and local paths do.
package browser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/preview"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
)

const (
	handoffActionKind      = "openurl_handoff"
	MinExtensionVersion    = "0.1.0"
	pageAcquireFeature     = "page_acquire"
	triageSnapshotFeature  = "triage_snapshot_v1"
	triageMutationsFeature = "triage_mutations_v1"
	reviewPreviewFeature   = "review_preview_v1"
	previewCapabilityTTL   = 10 * time.Minute
)

// ErrInvalidFrame marks a client-side protocol violation (a frame that fails
// strict decode, arrives before hello, or is not a legal inbound type). The RPC
// layer maps it to invalid_argument; other Sync errors are internal.
var ErrInvalidFrame = errors.New("invalid browser frame")

// Bridge is the per-daemon-run browser session. hello is tracked in memory: a
// fresh hello resets the offered/cancelled bookkeeping, which is exactly the
// recovery an MV3 service-worker restart needs (the extension reconnects, says
// hello again, and re-receives outstanding offers).
type Bridge struct {
	jobs        *job.Store
	svc         *app.Service
	triage      *triage.Service
	watchRunner *watch.Runner
	preview     *preview.Server
	cfg         config.Config

	// Version and Features are daemon capabilities announced in hello_ack.
	Version  string
	Features []string

	// extensionVersion and adapterVersions describe the current hello-session.
	extensionVersion string
	adapterVersions  map[string]string

	mu                sync.Mutex
	seq               int64
	helloSeen         bool
	extensionOutdated bool
	offered           map[string]bool // handoff jobs offered this hello-session
	cancelSent        map[string]bool // jobs a daemon-side cancel was already announced for
	now               func() time.Time
}

// NewBridge constructs the bridge. It is cheap and always constructed; whether
// any job is ever offered depends on config (extension_id / openurl base).
func NewBridge(jobs *job.Store, svc *app.Service, triageService *triage.Service, watchRunner *watch.Runner, previewServer *preview.Server, cfg config.Config, version string, features []string) *Bridge {
	return &Bridge{
		jobs: jobs, svc: svc, triage: triageService, watchRunner: watchRunner, preview: previewServer, cfg: cfg,
		Version: version,
		Features: appendFeatures(features,
			pageAcquireFeature, triageSnapshotFeature, triageMutationsFeature, reviewPreviewFeature),
		offered: map[string]bool{}, cancelSent: map[string]bool{},
		now: time.Now,
	}
}

func appendFeatures(features []string, required ...string) []string {
	mandatory := make(map[string]bool, len(required))
	orderedRequired := make([]string, 0, len(required))
	for _, feature := range required {
		if feature != "" && !mandatory[feature] {
			mandatory[feature] = true
			orderedRequired = append(orderedRequired, feature)
		}
	}
	result := make([]string, 0, 32)
	for _, feature := range features {
		if len(result) == 32-len(orderedRequired) {
			break
		}
		if feature != "" && !mandatory[feature] {
			duplicate := false
			for _, existing := range result {
				if existing == feature {
					duplicate = true
					break
				}
			}
			if !duplicate {
				result = append(result, feature)
			}
		}
	}
	return append(result, orderedRequired...)
}

func appendFeature(features []string, feature string) []string {
	return appendFeatures(features, feature)
}

// SessionInfo returns a consistent snapshot of the current extension hello-session.
func (b *Bridge) SessionInfo() (extensionVersion string, adapterCount int, helloSeen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.extensionVersion, len(b.adapterVersions), b.helloSeen
}

// Sync processes a batch of inbound frames (possibly empty for a poll) and
// returns the outbound command frames. Every inbound frame is re-validated with
// protocol.DecodeBrowserMessage; malformed frames fail the whole call closed.
// A valid frame or poll arriving after a daemon restart but before hello gets a
// protocol error frame so the still-connected extension can reconnect and
// establish a fresh hello-session. Every outbound frame is self-validated by
// the same decoder before it leaves.
func (b *Bridge) Sync(ctx context.Context, frames []json.RawMessage) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []json.RawMessage
	for _, raw := range frames {
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidFrame, err)
		}
		if !b.helloSeen && msg.Type != protocol.MsgHello {
			return b.helloRequired()
		}
		replies, err := b.handle(ctx, msg)
		if err != nil {
			return nil, err
		}
		out = append(out, replies...)
		if b.extensionOutdated {
			return out, nil
		}
	}
	if !b.helloSeen {
		required, err := b.helloRequired()
		if err != nil {
			return nil, err
		}
		return append(out, required...), nil
	}
	polled, err := b.poll(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, polled...), nil
}

// handle dispatches one decoded inbound frame.
func (b *Bridge) handle(ctx context.Context, msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	if msg.Type == protocol.MsgHello {
		p := msg.Payload.(*protocol.HelloPayload)
		b.helloSeen = true
		b.extensionOutdated = compareVersion(p.ExtensionVersion, MinExtensionVersion) < 0
		b.extensionVersion = p.ExtensionVersion
		b.adapterVersions = p.AdapterVersions
		b.offered = map[string]bool{}
		b.cancelSent = map[string]bool{}
		if b.extensionOutdated {
			return b.extensionOutdatedError()
		}
		ack, err := b.frame(protocol.MsgHelloAck, "", protocol.HelloAckPayload{
			DaemonVersion:   b.Version,
			Features:        b.Features,
			ResolverOrigins: b.cfg.ResolverOrigins(),
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{ack}, nil
	}
	if !b.helloSeen {
		return b.helloRequired()
	}
	if b.extensionOutdated {
		return b.extensionOutdatedError()
	}

	switch msg.Type {
	case protocol.MsgPageAcquire:
		return b.pageAcquire(ctx, msg.Payload.(*protocol.PageAcquirePayload))

	case protocol.MsgTriageSnapshotRequest:
		return b.triageSnapshot(ctx, msg.Payload.(*protocol.TriageSnapshotRequestPayload))

	case protocol.MsgTriageCountsRequest:
		return b.triageCounts(ctx, msg.Payload.(*protocol.TriageCountsRequestPayload))

	case protocol.MsgTriageDecide:
		return b.triageDecide(ctx, msg.Payload.(*protocol.TriageDecidePayload))

	case protocol.MsgHumanActionResolve:
		return b.humanActionResolve(ctx, msg.Payload.(*protocol.HumanActionResolvePayload))

	case protocol.MsgReviewPreviewRequest:
		return b.reviewPreview(ctx, msg.Payload.(*protocol.ReviewPreviewRequestPayload))

	case protocol.MsgJobAccept:
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_accept", nil)

	case protocol.MsgHandoffOutcome:
		return nil, b.handoffOutcome(ctx, msg.JobID, msg.Payload.(*protocol.HandoffOutcomePayload))

	case protocol.MsgJobReject:
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_reject", nil); err != nil {
			return nil, err
		}
		if fellBack, err := b.fallbackOAHandoff(ctx, msg.JobID, "browser_rejected"); err != nil {
			return nil, err
		} else if fellBack {
			return nil, nil
		}
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			return nil, err
		}
		return nil, b.leaveHandoff(ctx, msg.JobID, job.StateUnavailable, "browser_rejected")

	case protocol.MsgAuthPending, protocol.MsgAuthReturned:
		return nil, b.recordAuth(ctx, msg)

	case protocol.MsgDownloadStarted:
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_started", nil)

	case protocol.MsgDownloadComplete:
		p := msg.Payload.(*protocol.DownloadCompletePayload)
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_complete",
			map[string]any{"filename": p.Filename, "size_bytes": p.SizeBytes}); err != nil {
			return nil, err
		}
		if err := b.adoptOutsideSessionLock(ctx, msg.JobID, p.Filename); err != nil {
			// Environmental failure (file not there yet, Chrome rename race,
			// user saved elsewhere) must not sever the bridge: record it, keep
			// the job parked, and let the poll-time directory scan pick the
			// file up when it appears. Confinement violations land here too —
			// the report is ignored and the job simply stays awaiting_human.
			if evErr := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.adoption_deferred",
				map[string]any{"filename": p.Filename, "reason": truncate(err.Error(), 200)}); evErr != nil {
				return nil, evErr
			}
		}
		ack, err := b.frame(protocol.MsgAck, msg.JobID, protocol.EmptyPayload{})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{ack}, nil

	case protocol.MsgProviderOutcome:
		return nil, b.outcome(ctx, msg.JobID, msg.Payload.(*protocol.ProviderOutcomePayload))

	case protocol.MsgCancel:
		// Extension -> daemon: the user closed the broker-owned tab. Treat as a
		// cancelled outcome.
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			return nil, err
		}
		b.cancelSent[msg.JobID] = true // we initiated nothing to echo back
		return nil, b.jobs.Cancel(ctx, msg.JobID, "browser_cancelled")

	case protocol.MsgError:
		// Only the normalized code is durable; the free-text message is
		// extension-supplied and never persisted.
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.error",
			map[string]any{"code": msg.Payload.(*protocol.ErrorPayload).Code})

	default:
		return nil, fmt.Errorf("%w: unexpected inbound frame type %q", ErrInvalidFrame, msg.Type)
	}
}

// triageSnapshot answers only an explicit request. It progressively lowers the
// requested page size before framing so a maximal page cannot breach the native
// messaging cap; each retry asks the service for a real page so its cursor
// remains valid for exactly the returned item count.
func (b *Bridge) triageSnapshot(ctx context.Context, request *protocol.TriageSnapshotRequestPayload) ([]json.RawMessage, error) {
	if b.triage == nil {
		return nil, errors.New("triage service is not configured")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	for {
		snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: int(limit), Cursor: request.Cursor})
		if err != nil {
			return nil, err
		}
		payload := triageSnapshotPayload(request.RequestID, snapshot)
		if b.frameFits(protocol.MsgTriageSnapshotResponse, payload) {
			frame, err := b.frame(protocol.MsgTriageSnapshotResponse, "", payload)
			if err != nil {
				return nil, err
			}
			return []json.RawMessage{frame}, nil
		}
		if len(snapshot.Items) <= 1 {
			return nil, fmt.Errorf("triage snapshot item exceeds browser frame cap %d", protocol.MaxBrowserMessageBytes)
		}
		limit = int64(len(snapshot.Items) - 1)
	}
}

func triageSnapshotPayload(requestID string, snapshot triage.Snapshot) protocol.TriageSnapshotResponsePayload {
	items := make([]protocol.TriageSnapshotItem, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		payload := protocol.TriageSnapshotItem{
			Kind: item.Kind, ID: item.ID, Rank: int64(item.Rank), Title: item.Title,
			Facts: triageFacts(item.Facts), Links: triageLinks(item.Links), Ops: append([]string(nil), item.Ops...),
		}
		switch item.Kind {
		case triage.KindWatchHit:
			hit := item.WatchHit
			if hit != nil {
				payload.Work = &protocol.TriageWork{
					DOI: hit.Work.DOI, Title: hit.Work.Title, Authors: hit.Work.Authors,
					Year: int64(hit.Work.Year), IsOA: hit.Work.IsOA,
				}
				payload.Abstract = hit.Abstract
				payload.Watches = make([]protocol.TriageWatch, 0, len(hit.Watches))
				for _, watched := range hit.Watches {
					payload.Watches = append(payload.Watches, protocol.TriageWatch{ID: watched.ID, Label: watched.Label})
				}
				payload.FirstSeenAt = hit.FirstSeenAt
			}
		case triage.KindHumanAction:
			action := item.HumanAction
			if action != nil {
				payload.ActionID, payload.JobID = action.ActionID, action.JobID
				payload.ActionKind, payload.JobState = action.ActionKind, action.JobState
				payload.Revision, payload.SHA256, payload.SizeBytes = action.Revision, action.SHA256, action.SizeBytes
			}
		case triage.KindRetraction:
			retraction := item.Retraction
			if retraction != nil {
				payload.DOI, payload.Nature = retraction.DOI, retraction.Nature
				payload.NoticedAt = retraction.NoticedAt.UTC().Format(time.RFC3339Nano)
				payload.NoticeDOI = retraction.NoticeDOI
			}
		}
		items = append(items, payload)
	}
	return protocol.TriageSnapshotResponsePayload{
		RequestID: requestID, Schema: int64(snapshot.Schema), GeneratedAt: snapshot.GeneratedAt,
		Counts: triageCountsPayload(snapshot.Counts), Items: items, Cursor: snapshot.Cursor,
		HasMore: snapshot.HasMore, UnsupportedItemsCount: int64(snapshot.UnsupportedItemsCount),
	}
}

func triageFacts(facts []triage.Fact) []protocol.TriageFact {
	result := make([]protocol.TriageFact, 0, len(facts))
	for _, fact := range facts {
		result = append(result, protocol.TriageFact{Label: fact.Label, Text: fact.Text})
	}
	return result
}

func triageLinks(links []triage.Link) []protocol.TriageLink {
	result := make([]protocol.TriageLink, 0, len(links))
	for _, link := range links {
		result = append(result, protocol.TriageLink{Rel: link.Rel, URL: link.URL})
	}
	return result
}

func triageCountsPayload(counts triage.Counts) protocol.TriageCounts {
	return protocol.TriageCounts{
		PendingTotal: int64(counts.PendingTotal), WatchHits: int64(counts.WatchHits), Actions: int64(counts.Actions),
		Retractions: int64(counts.Retractions), JobsWorking: int64(counts.JobsWorking),
		JobsNeedsReview: int64(counts.JobsNeedsReview), FailureGroups7d: int64(counts.FailureGroups7d),
	}
}

func (b *Bridge) triageCounts(ctx context.Context, request *protocol.TriageCountsRequestPayload) ([]json.RawMessage, error) {
	if b.triage == nil {
		return nil, errors.New("triage service is not configured")
	}
	counts, err := b.triage.Counts(ctx)
	if err != nil {
		return nil, err
	}
	frame, err := b.frame(protocol.MsgTriageCountsResponse, "", protocol.TriageCountsResponsePayload{
		RequestID: request.RequestID, Counts: triageCountsPayload(counts),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) triageDecide(ctx context.Context, request *protocol.TriageDecidePayload) ([]json.RawMessage, error) {
	if b.triage == nil || b.watchRunner == nil {
		return b.triageDecisionResult(request.RequestID, "error", "triage mutations are not configured")
	}
	hit, err := b.triage.FindWatchHit(ctx, request.ItemID)
	if errors.Is(err, sql.ErrNoRows) {
		return b.triageDecisionResult(request.RequestID, "conflict", "")
	}
	if err != nil {
		return b.triageDecisionResult(request.RequestID, "error", err.Error())
	}
	if request.Op == "acquire" {
		for _, watched := range hit.Watches {
			if _, err := b.watchRunner.AcquireDigest(ctx, watched.ID, []string{watched.WorkKey}); err != nil {
				if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
					return b.triageDecisionResult(request.RequestID, "conflict", "")
				}
				return b.triageDecisionResult(request.RequestID, "error", err.Error())
			}
		}
		return b.triageDecisionResult(request.RequestID, "applied", "")
	}
	selected, err := triageDismissScope(request.WatchScope, hit.Watches)
	if err != nil {
		return b.triageDecisionResult(request.RequestID, "error", err.Error())
	}
	for _, watched := range hit.Watches {
		if !selected[watched.ID] {
			continue
		}
		if _, err := b.watchRunner.ConsumeDigest(ctx, watched.ID, []string{watched.WorkKey}); err != nil {
			if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
				return b.triageDecisionResult(request.RequestID, "conflict", "")
			}
			return b.triageDecisionResult(request.RequestID, "error", err.Error())
		}
	}
	return b.triageDecisionResult(request.RequestID, "applied", "")
}

func triageDismissScope(raw json.RawMessage, watches []triage.Watch) (map[int64]bool, error) {
	var all string
	if err := json.Unmarshal(raw, &all); err == nil {
		if all != "all" {
			return nil, errors.New("watch_scope must be all or watch IDs")
		}
		selected := make(map[int64]bool, len(watches))
		for _, watched := range watches {
			selected[watched.ID] = true
		}
		return selected, nil
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, errors.New("watch_scope must be all or watch IDs")
	}
	available, selected := make(map[int64]bool, len(watches)), make(map[int64]bool, len(ids))
	for _, watched := range watches {
		available[watched.ID] = true
	}
	for _, id := range ids {
		if !available[id] || selected[id] {
			return nil, errors.New("watch_scope contains an invalid watch ID")
		}
		selected[id] = true
	}
	return selected, nil
}

func (b *Bridge) triageDecisionResult(requestID, outcome, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgTriageDecideResult, "", protocol.TriageDecideResultPayload{
		RequestID: requestID, Outcome: outcome, Detail: truncate(detail, 1000),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) humanActionResolve(ctx context.Context, request *protocol.HumanActionResolvePayload) ([]json.RawMessage, error) {
	if b.jobs == nil {
		return b.humanActionResolveResult(request.RequestID, "error", "jobs are not configured")
	}
	resolution, err := b.jobs.ResolveReviewCAS(ctx, job.ResolveReviewInput{
		ActionID: request.ActionID, Verdict: request.Verdict,
		ExpectedRevision: request.ExpectedRevision, ExpectedSHA256: request.ExpectedSHA256,
	})
	if err != nil {
		if errors.Is(err, job.ErrConflict) {
			return b.humanActionResolveResult(request.RequestID, "conflict", "")
		}
		return b.humanActionResolveResult(request.RequestID, "error", err.Error())
	}
	if b.preview != nil && (resolution.Outcome == job.ReviewApplied || resolution.Outcome == job.ReviewAlreadyApplied) {
		b.preview.Revoke(request.ActionID)
	}
	return b.humanActionResolveResult(request.RequestID, string(resolution.Outcome), "")
}

func (b *Bridge) humanActionResolveResult(requestID, outcome, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgHumanActionResolveResult, "", protocol.HumanActionResolveResultPayload{
		RequestID: requestID, Outcome: outcome, Detail: truncate(detail, 1000),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) reviewPreview(ctx context.Context, request *protocol.ReviewPreviewRequestPayload) ([]json.RawMessage, error) {
	if b.jobs == nil || b.preview == nil {
		return nil, errors.New("review preview is not configured")
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return nil, err
	}
	var action *job.HumanAction
	for i := range actions {
		if actions[i].ID == request.ActionID {
			action = &actions[i]
			break
		}
	}
	if action == nil || action.Kind != "verify_identity" {
		return nil, fmt.Errorf("review action %d is unavailable", request.ActionID)
	}
	info, err := os.Stat(action.QuarantinePath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("review action %d preview is unavailable", request.ActionID)
	}
	url, err := b.preview.Issue(action.ID, action.QuarantinePath, action.QuarantineSHA256, info.Size(), previewCapabilityTTL)
	if err != nil {
		return nil, err
	}
	frame, err := b.frame(protocol.MsgReviewPreviewResult, "", protocol.ReviewPreviewResultPayload{
		RequestID: request.RequestID, URL: url, SHA256: action.QuarantineSHA256, SizeBytes: info.Size(),
		ExpiresAt: b.now().UTC().Add(previewCapabilityTTL).Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) frameFits(msgType string, payload any) bool {
	raw, err := json.Marshal(map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     msgType,
		"msg_id":   "AAAAAAAAAAAAAAAAAAAAAA",
		"seq":      b.seq + 1,
		"payload":  payload,
	})
	return err == nil && len(raw) <= protocol.MaxBrowserMessageBytes
}
func (b *Bridge) pageAcquire(ctx context.Context, payload *protocol.PageAcquirePayload) ([]json.RawMessage, error) {
	request, err := pageAcquireRequest(payload)
	if err != nil {
		return b.pageAcquireError(err)
	}
	if existing, err := b.liveJobForRequest(ctx, request.RequestID); err != nil {
		return b.pageAcquireError(err)
	} else if existing != "" {
		ack, err := b.frame(protocol.MsgPageAcquireAck, "", protocol.PageAcquireAckPayload{
			JobID: existing, Duplicate: true,
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{ack}, nil
	}
	jobID, err := b.svc.Submit(ctx, request)
	if err != nil {
		return b.pageAcquireError(err)
	}
	ack, err := b.frame(protocol.MsgPageAcquireAck, "", protocol.PageAcquireAckPayload{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{ack}, nil
}

func pageAcquireRequest(payload *protocol.PageAcquirePayload) (protocol.WorkRequest, error) {
	doi := strings.TrimSpace(payload.DOI)
	if doi == "" {
		return protocol.WorkRequest{}, errors.New("page has no DOI")
	}
	normalizedDOI, err := work.NormalizeDOI(doi)
	if err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("invalid page DOI: %w", err)
	}
	request := protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		DesiredVersion: "any",
		Identifiers:    &protocol.Identifiers{DOI: normalizedDOI},
	}
	title := strings.TrimSpace(payload.Title)
	if len(title) >= 3 && len(title) <= 500 {
		request.Title = title
	}
	identity := "doi:" + normalizedDOI
	sum := sha256.Sum256([]byte(identity))
	request.RequestID = "page_acquire_" + hex.EncodeToString(sum[:])
	return request, nil
}

func (b *Bridge) liveJobForRequest(ctx context.Context, requestID string) (string, error) {
	var jobID string
	err := b.jobs.S.DB().QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE work_request_id = ? AND state NOT IN ('failed','cancelled','unavailable') ORDER BY created_at DESC LIMIT 1`,
		requestID,
	).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return jobID, nil
}

func (b *Bridge) pageAcquireError(err error) ([]json.RawMessage, error) {
	ack, frameErr := b.frame(protocol.MsgPageAcquireAck, "", protocol.PageAcquireAckPayload{
		Error: truncate(err.Error(), 1000),
	})
	if frameErr != nil {
		return nil, frameErr
	}
	return []json.RawMessage{ack}, nil
}

// adoptOutsideSessionLock runs validation without blocking unrelated browser
// syncs. The adoption service leases the durable job state before validation,
// so releasing the in-memory session lock cannot admit a competing adoption.
// The caller must hold b.mu; it is held again before this method returns.
func (b *Bridge) adoptOutsideSessionLock(ctx context.Context, jobID, filename string) error {
	b.mu.Unlock()
	defer b.mu.Lock()
	return b.adopt(ctx, jobID, filename)
}

// helloRequired tells a still-connected extension that the daemon lost its
// in-memory browser session (for example, after a daemon restart). The existing
// error frame keeps papio-browser/1 unchanged while instructing the extension
// to reconnect and send the mandatory first hello.
func (b *Bridge) helloRequired() ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgError, "", protocol.ErrorPayload{
		Code:    "expected_hello",
		Message: "hello required before browser session can resume",
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) extensionOutdatedError() ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgError, "", protocol.ErrorPayload{
		Code:    "extension_outdated",
		Message: "update the extension from the store, then reconnect",
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func compareVersion(left, right string) int {
	parse := func(value string) [3]int {
		var parts [3]int
		for i, raw := range strings.SplitN(value, ".", 3) {
			parts[i], _ = strconv.Atoi(raw)
		}
		return parts
	}
	a, b := parse(left), parse(right)
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// recordAuth appends a timing-only auth event. The AuthPayload structurally
// cannot carry a URL, host, title, query, or fragment, so an identity-provider
// address cannot enter the event stream through this path.
func (b *Bridge) recordAuth(ctx context.Context, msg *protocol.BrowserMessage) error {
	kind := "browser.auth_pending"
	if msg.Type == protocol.MsgAuthReturned {
		kind = "browser.auth_returned"
	}

	detail := map[string]any{}
	if p := msg.Payload.(*protocol.AuthPayload); p.ElapsedMS != nil {
		detail["elapsed_ms"] = *p.ElapsedMS
	}
	return b.jobs.S.AppendEvent(ctx, msg.JobID, kind, detail)
}

// outcome maps a terminal provider observation onto a policy-legal transition.
func (b *Bridge) outcome(ctx context.Context, jobID string, p *protocol.ProviderOutcomePayload) error {
	switch p.Outcome {
	case "cancelled":
		if err := b.resolveHandoff(ctx, jobID, "cancelled"); err != nil {
			return err
		}
		b.cancelSent[jobID] = true
		return b.jobs.Cancel(ctx, jobID, "browser_cancelled")

	case "no_entitlement", "document_delivery_available":
		if fellBack, err := b.fallbackOAHandoff(ctx, jobID, p.Outcome); err != nil {
			return err
		} else if fellBack {
			return nil
		}
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateUnavailable, p.Outcome)

	case "wrong_work", "ui_changed":
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateNeedsReview, p.Outcome)

	case "rate_limited":
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateRetryWait, p.Outcome)

	case "human_auth_required", "terms_acceptance_required":
		// Still legitimately in progress: keep the job parked and add the
		// specific human action the extension observed.
		_, err := b.jobs.OpenHumanAction(ctx, jobID, p.Outcome,
			"the provider requires a human step before the download can proceed")
		return err

	default:
		return fmt.Errorf("unknown provider outcome %q", p.Outcome)
	}
}

// leaveHandoff transitions a parked handoff job out of awaiting_human. It is
// idempotent: if the job already left awaiting_human, it does nothing.
func (b *Bridge) leaveHandoff(ctx context.Context, jobID, to, reason string) error {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if row.State != job.StateAwaitingHuman {
		return nil
	}
	detail := map[string]any{"reason": reason}
	var opts []job.TransitionOpt
	switch to {
	case job.StateUnavailable:
		opts = append(opts, job.WithTerminalReason(reason))
	case job.StateRetryWait:
		opts = append(opts, job.WithRetryAt(b.now().Add(b.actionExpiry())))
	}
	return b.jobs.Transition(ctx, jobID, job.StateAwaitingHuman, to, detail, opts...)
}

// adopt resolves the reported download strictly under the job's adoption
// directory and hands it to the app for validation. The filename has already
// passed protocol validation (no path separators); this adds IsLocal and a
// symlink-resolved prefix guard before app-side confinement.
func (b *Bridge) adopt(ctx context.Context, jobID, filename string) error {
	if !filepath.IsLocal(filename) {
		return fmt.Errorf("adoption filename %q is not a local name", filename)
	}
	root := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("adoption root unavailable: %w", err)
	}
	full := filepath.Join(realRoot, filename)
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel != filename || strings.Contains(rel, "..") {
		return fmt.Errorf("adoption path escapes %s", realRoot)
	}
	return b.svc.AdoptDownload(ctx, jobID, full)
}

// scanAdoptionDir looks for exactly one settled candidate file in a parked
// job's adoption directory. Dotfiles (.DS_Store) are invisible; any
// .crdownload/.download marks an in-progress Chrome write and .part a Firefox
// one; either defers the whole scan. A zero-byte file is the browser's
// placeholder target (Firefox creates the final name empty while streaming
// into name.part), never a settled download, so it defers the scan too. More
// than one visible file is ambiguous and adopts nothing. The returned name
// feeds adopt(), which re-applies full confinement checks.
func (b *Bridge) scanAdoptionDir(_ context.Context, jobID string) (string, bool) {
	dir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false // no directory yet: nothing placed
	}
	var name string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		if strings.HasSuffix(n, ".crdownload") || strings.HasSuffix(n, ".download") || strings.HasSuffix(n, ".part") {
			return "", false // the browser is still writing; wait for the rename
		}
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err != nil || info.Size() == 0 {
			return "", false // placeholder target of an in-progress write
		}
		if name != "" {
			return "", false // ambiguous: stays with the user
		}
		name = n
	}
	return name, name != ""
}

// SweepAdoptions adopts any settled file sitting in a parked handoff job's
// adoption directory, independently of whether the extension is connected or
// has said hello. This makes directory adoption self-driving: the daemon owns
// completion, the browser plane is only a delivery hint. It scans the adoption
// root directly rather than the newest-N job list, so a settled download is
// never missed behind a large handoff backlog. It never emits frames or opens
// offers. Safe to call on a timer.
func (b *Bridge) SweepAdoptions(ctx context.Context) error {
	root := b.cfg.EffectiveAdoptionRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return err
	}
	handoff := map[string]bool{}
	for _, a := range actions {
		if a.Kind == handoffActionKind {
			handoff[a.JobID] = true
		}
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" {
			continue
		}
		jobID := e.Name()
		if !handoff[jobID] {
			continue // no open handoff action: not an adoptable handoff job
		}
		row, err := b.jobs.Get(ctx, jobID)
		if err != nil || row == nil || row.State != job.StateAwaitingHuman {
			continue
		}
		name, ok := b.scanAdoptionDir(ctx, jobID)
		if !ok {
			continue
		}
		if err := b.adopt(ctx, jobID, name); err != nil {
			if evErr := b.jobs.S.AppendEvent(ctx, jobID, "browser.adoption_deferred",
				map[string]any{"filename": name, "reason": truncate(err.Error(), 200)}); evErr != nil {
				return evErr
			}
		}
	}
	return nil
}

// SweepTerminalAdoptions removes the per-job adoption landing directory of any
// terminal job. A ready job's PDF has already been promoted into the immutable
// artifact store (Zotero imports from there, never from the landing copy), and
// cancelled/failed/unavailable jobs never produced anything the user needs from
// this directory, so the landing bytes are pure disk growth. Non-terminal jobs
// are load-bearing and never swept: awaiting_human handoffs may still receive a
// download, and needs_review inspection files are referenced by open actions.
// The rejected/ sibling directory, which deliberately preserves files a human
// must re-supply, is left untouched. Best-effort, idempotent, and safe on a
// timer.
func (b *Bridge) SweepTerminalAdoptions(ctx context.Context) error {
	root := b.cfg.EffectiveAdoptionRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" {
			continue
		}
		row, err := b.jobs.Get(ctx, e.Name())
		if err != nil || row == nil {
			continue // unknown or unreadable dir: leave it for a human
		}
		if !job.Terminal(row.State) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	return nil
}

// RunSweeper calls SweepAdoptions and SweepTerminalAdoptions on an interval
// until ctx is cancelled.
// Cancellation is a normal shutdown and returns nil. Per-job adoption failures
// are recorded as durable browser.adoption_deferred events inside
// SweepAdoptions; a transient store-level sweep error is retried on the next
// tick rather than returned, because this goroutine is unsupervised and a dead
// sweeper silently strands every subsequently downloaded PDF.
func (b *Bridge) RunSweeper(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := b.SweepAdoptions(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Best-effort, idempotent scan: a transient store error (DB
				// busy, a momentary read failure) must NOT kill the only
				// directory-adoption loop. A dead sweeper silently strands
				// every PDF that lands afterward until a daemon restart, and
				// this goroutine is unsupervised, so its death is invisible.
				// Retry next tick; a genuinely fatal store failure also breaks
				// the supervised server and scheduler loops.
			}
			if err := b.SweepTerminalAdoptions(ctx); err != nil && ctx.Err() != nil {
				return nil
			}
		}
	}
}

// poll offers outstanding handoff jobs (once per hello-session) and announces a
// daemon-side cancel for any offered job the user has since cancelled.
func (b *Bridge) poll(ctx context.Context) ([]json.RawMessage, error) {
	awaiting, err := b.jobs.List(ctx, job.StateAwaitingHuman, 200)
	if err != nil {
		return nil, err
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return nil, err
	}
	handoff := map[string]job.HumanAction{}
	for _, a := range actions {
		if a.Kind == handoffActionKind {
			handoff[a.JobID] = a
		}
	}
	present := map[string]bool{}
	var out []json.RawMessage
	for i := range awaiting {
		row := awaiting[i]
		present[row.ID] = true
		action, ok := handoff[row.ID]
		if !ok {
			continue
		}
		// Directory-scan adoption: a file the user (or a steered Chrome
		// download) placed in the job's adoption directory is the strongest
		// job-scoped gesture available. Exactly one settled regular file
		// adopts; zero or several (or an in-progress .crdownload) waits —
		// ambiguity stays with the user, per the fail-closed rule.
		if name, ok := b.scanAdoptionDir(ctx, row.ID); ok {
			if err := b.adoptOutsideSessionLock(ctx, row.ID, name); err != nil {
				if evErr := b.jobs.S.AppendEvent(ctx, row.ID, "browser.adoption_deferred",
					map[string]any{"filename": name, "reason": truncate(err.Error(), 200)}); evErr != nil {
					return nil, evErr
				}
			} else {
				continue // adopted; the job has left awaiting_human
			}
		}
		if b.offered[row.ID] {
			continue
		}
		offer, err := b.offer(row, action)
		if err != nil {
			return nil, err
		}
		if err := b.jobs.S.AppendEvent(ctx, row.ID, "browser.handoff_offered",
			map[string]any{"requires_auth": action.RequiresAuth}); err != nil {
			return nil, err
		}
		out = append(out, offer)
		b.offered[row.ID] = true
	}
	// Announce a cancel for any offered job that left awaiting_human because it
	// was cancelled daemon-side (e.g. `papio jobs cancel`).
	for id := range b.offered {
		if present[id] || b.cancelSent[id] {
			continue
		}
		row, err := b.jobs.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if row.State != job.StateCancelled {
			continue
		}
		frame, err := b.frame(protocol.MsgCancel, id, protocol.EmptyPayload{})
		if err != nil {
			return nil, err
		}
		out = append(out, frame)
		b.cancelSent[id] = true
	}
	return out, nil
}

// offer builds a job_offer for one parked handoff job. OA browser handoffs
// reuse the frozen OpenURL field with the candidate's public URL; institutional
// handoffs still construct the regular OpenURL resolver link.
func (b *Bridge) offer(row job.Row, action job.HumanAction) (json.RawMessage, error) {
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	offerURL := OpenURL(inst.OpenURLBase, row.Work)
	if oaURL, ok := app.OABrowserHandoffURL(action.Detail); ok {
		offerURL = oaURL
	}
	hosts := []string{}
	if h := resolverHost(offerURL); h != "" {
		hosts = append(hosts, h)
	}
	hosts = append(hosts, verifiedProviderHosts...)
	expected := &protocol.JobOfferExpected{DOI: row.Work.DOI, Title: truncate(row.Work.Title, 500)}
	if expected.DOI == "" && expected.Title == "" {
		expected = nil
	}
	payload := protocol.JobOfferPayload{
		OpenURL:       offerURL,
		ProviderHosts: hosts,
		Expected:      expected,
		AccessMode:    b.cfg.AccessMode,
		RequiresAuth:  action.RequiresAuth,
		ExpiresAt:     b.now().Add(b.actionExpiry()).UTC().Format(time.RFC3339),
	}
	// Federated login-routing: hand this job's institution Shibboleth entityID
	// and ProQuest account id to the extension so it can auto-select the
	// institution on a provider login wall and unlock ProQuest's link-resolver.
	// Values are per-profile (InstitutionFor), so a named institution routes its
	// own login and never inherits the default institution's identity.
	payload.LoginEntityID = inst.ShibbolethEntityID
	payload.ProquestAccountID = inst.ProquestAccountID
	return b.frame(protocol.MsgJobOffer, row.ID, payload)
}

// handoffOutcome records an extension-reported IdP failure on a parked
// handoff and re-arms the offer so the next poll sends a freshly minted
// OpenURL. The job stays awaiting_human and the action stays open: the human
// is mid-recovery and the fresh link is the recovery path. Unknown jobs or
// jobs without an open handoff are dropped fail-closed.
func (b *Bridge) handoffOutcome(ctx context.Context, jobID string, p *protocol.HandoffOutcomePayload) error {
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return err
	}
	open := false
	for _, action := range actions {
		if action.JobID == jobID && action.Kind == handoffActionKind {
			open = true
			break
		}
	}
	if !open {
		return nil
	}
	if err := b.jobs.S.AppendEvent(ctx, jobID, "browser.handoff_failed",
		map[string]any{"outcome": p.Outcome, "final_host": p.FinalHost}); err != nil {
		return err
	}
	delete(b.offered, jobID)
	return nil
}

// fallbackOAHandoff replaces the one-time OA browser offer with the ordinary
// institutional resolver offer while keeping the job parked. The action's
// detail is the durable offer discriminator, so a restart cannot re-open the
// OA URL and alternate forever.
func (b *Bridge) fallbackOAHandoff(ctx context.Context, jobID, failure string) (bool, error) {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return false, err
	}
	if base, ok := b.cfg.OpenURLBaseFor(row.Policy.Resolver); !ok || base == "" {
		return false, nil
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		if action.JobID != jobID || action.Kind != handoffActionKind {
			continue
		}
		if _, ok := app.OABrowserHandoffURL(action.Detail); !ok {
			return false, nil
		}
		if _, err := b.jobs.OpenHumanAction(ctx, jobID, handoffActionKind, app.InstitutionalOpenURLHandoffDetail, job.WithAccessClassification(true, "paywall")); err != nil {
			return false, err
		}
		if err := b.jobs.RecordEvent(ctx, jobID, "browser.oa_handoff_fallback", map[string]any{"reason": failure}); err != nil {
			return false, err
		}
		delete(b.offered, jobID)
		return true, nil
	}
	return false, nil
}

// resolveHandoff closes the open openurl_handoff action for a job with the given
// terminal status ("resolved" or "cancelled").
func (b *Bridge) resolveHandoff(ctx context.Context, jobID, status string) error {
	_, err := b.jobs.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = ?, resolved_at = ?
		 WHERE job_id = ? AND kind = ? AND status = 'open'`,
		status, store.Now(), jobID, handoffActionKind)
	return err
}

// frame encodes one outbound envelope with a fresh msg_id and a monotonic seq,
// then self-validates it through the strict decoder so a malformed command can
// never leave the daemon.
func (b *Bridge) frame(msgType, jobID string, payload any) (json.RawMessage, error) {
	b.seq++
	env := map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     msgType,
		"msg_id":   newMsgID(),
		"seq":      b.seq,
		"payload":  payload,
	}
	if jobID != "" {
		env["job_id"] = jobID
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if _, err := protocol.DecodeBrowserMessage(raw); err != nil {
		return nil, fmt.Errorf("outbound %s failed self-validation: %w", msgType, err)
	}
	return raw, nil
}

func (b *Bridge) actionExpiry() time.Duration {
	secs := b.cfg.Browser.ActionExpirySeconds
	if secs <= 0 {
		secs = 1800
	}
	return time.Duration(secs) * time.Second
}

// newMsgID returns a random identifier matching ^[A-Za-z0-9_-]{8,64}$.
func newMsgID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
