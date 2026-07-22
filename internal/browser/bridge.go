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
	"log"
	"os"
	"path/filepath"
	"sort"
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

// Bridge is the per-daemon-run browser bridge. Sessions are tracked in
// memory: each native-host process carries a session_id, exactly one session
// holds the offer/handoff flow, and later hellos from other browsers wait as
// pending instead of silently stealing the session. A fresh hello from the
// holder still resets the offered/cancelled bookkeeping, which is exactly the
// recovery an MV3 service-worker restart needs.
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

	mu           sync.Mutex
	seq          int64
	holder       *browserSession
	pending      map[string]*browserSession
	deniedHellos int
	takeovers    int
	// epoch increments whenever holder identity changes. Code that releases
	// b.mu mid-flight (adoption windows inside poll) re-checks it afterwards:
	// a concurrent claim/takeover must not let a resumed poll send offers to
	// a demoted session or pollute the new holder's bookkeeping.
	epoch      uint64
	offered    map[string]bool // handoff jobs offered to the current holder
	cancelSent map[string]bool // jobs a daemon-side cancel was already announced for
	now        func() time.Time
}

// browserSession is one native-host connection that said hello.
type browserSession struct {
	ID               string
	ExtensionVersion string
	AdapterVersions  map[string]string
	HelloAt          time.Time
	LastSyncAt       time.Time
	Outdated         bool
	// needsAck makes the next Sync from this session deliver a hello_ack:
	// a session promoted by claim or stale-takeover was denied its ack at
	// hello time and must still receive one before offers mean anything.
	needsAck bool
}

// legacySessionID stands in for native hosts older than the session_id field.
// It cannot collide with real ids (32 hex chars). Legacy hosts cannot be
// arbitrated, so a legacy hello always takes the session (and loses it to any
// later hello), preserving the historical last-hello-wins behavior.
const legacySessionID = "legacy"

// sessionStaleAfter is how long a holder may go without syncing before
// another browser may take over. Live native hosts poll every 2 seconds
// (nativehost.pollInterval); 5x that absorbs scheduling hiccups without
// making crash recovery feel slow.
const sessionStaleAfter = 10 * time.Second

// pendingExpireAfter prunes pending sessions whose native host stopped
// syncing without a goodbye (browser killed) so `papio browser sessions`
// reflects reality.
const pendingExpireAfter = 5 * time.Minute

// NewBridge constructs the bridge. It is cheap and always constructed; whether
// any job is ever offered depends on config (extension_id / openurl base).
func NewBridge(jobs *job.Store, svc *app.Service, triageService *triage.Service, watchRunner *watch.Runner, previewServer *preview.Server, cfg config.Config, version string, features []string) *Bridge {
	return &Bridge{
		jobs: jobs, svc: svc, triage: triageService, watchRunner: watchRunner, preview: previewServer, cfg: cfg,
		Version: version,
		Features: appendFeatures(features,
			pageAcquireFeature, triageSnapshotFeature, triageMutationsFeature, reviewPreviewFeature),
		offered: map[string]bool{}, cancelSent: map[string]bool{}, pending: map[string]*browserSession{},
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

// SessionInfo returns a consistent snapshot of the holder hello-session.
func (b *Bridge) SessionInfo() (extensionVersion string, adapterCount int, helloSeen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holder == nil {
		return "", 0, false
	}
	return b.holder.ExtensionVersion, len(b.holder.AdapterVersions), true
}

// SessionSummary is one connected browser session for status/CLI surfaces.
type SessionSummary struct {
	ID               string `json:"id"`
	ExtensionVersion string `json:"extension_version"`
	Holder           bool   `json:"holder"`
	HelloAt          string `json:"hello_at"`
	LastSyncAt       string `json:"last_sync_at"`
}

// Sessions lists the holder and every pending session, holder first, plus the
// arbitration counters accumulated since daemon start.
func (b *Bridge) Sessions() (sessions []SessionSummary, deniedHellos, takeovers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holder != nil {
		sessions = append(sessions, summarize(b.holder, true))
	}
	rest := make([]*browserSession, 0, len(b.pending))
	for _, session := range b.pending {
		rest = append(rest, session)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].LastSyncAt.After(rest[j].LastSyncAt) })
	for _, session := range rest {
		sessions = append(sessions, summarize(session, false))
	}
	return sessions, b.deniedHellos, b.takeovers
}

func summarize(session *browserSession, holder bool) SessionSummary {
	return SessionSummary{
		ID:               session.ID,
		ExtensionVersion: session.ExtensionVersion,
		Holder:           holder,
		HelloAt:          session.HelloAt.UTC().Format(time.RFC3339),
		LastSyncAt:       session.LastSyncAt.UTC().Format(time.RFC3339),
	}
}

// Claim promotes a pending session to holder (the `papio browser use` path)
// and returns the resolved full session id. The demoted holder stays pending
// — it is still alive and polling. sessionID may be an unambiguous prefix.
func (b *Bridge) Claim(sessionID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := strings.TrimSpace(sessionID)
	if prefix == "" {
		return "", errors.New("browser session id is required")
	}
	var matches []*browserSession
	if b.holder != nil && strings.HasPrefix(b.holder.ID, prefix) {
		matches = append(matches, b.holder)
	}
	for id, session := range b.pending {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, session)
		}
	}
	switch {
	case len(matches) == 0:
		return "", fmt.Errorf("unknown browser session %q (run 'papio browser sessions')", prefix)
	case len(matches) > 1:
		// Ambiguity includes the holder: a prefix matching both the holder
		// and a pending session must not report a silent no-op success.
		return "", fmt.Errorf("browser session prefix %q is ambiguous (run 'papio browser sessions')", prefix)
	case matches[0] == b.holder:
		return b.holder.ID, nil // already the holder
	}
	b.promote(matches[0], "claimed via papio browser use")
	return b.holder.ID, nil
}

// promote makes session the holder. The caller holds b.mu. The previous
// holder, when still present, is demoted to pending rather than dropped so an
// explicit claim can be reversed with another claim.
func (b *Bridge) promote(session *browserSession, reason string) {
	if b.holder != nil && b.holder.ID != session.ID {
		b.pending[b.holder.ID] = b.holder
	}
	delete(b.pending, session.ID)
	session.needsAck = true
	b.holder = session
	b.epoch++
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	b.takeovers++
	log.Printf("papio: browser session %s (v%s) now holds the bridge: %s", shortSession(session.ID), session.ExtensionVersion, reason)
}

func shortSession(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Sync processes a batch of inbound frames (possibly empty for a poll) from
// one native-host session and returns the outbound command frames. Every
// inbound frame is re-validated with protocol.DecodeBrowserMessage; malformed
// frames fail the whole call closed. A frame or poll from a session the
// daemon does not know (daemon restart, dropped stale holder) gets a protocol
// error frame instructing the extension to re-hello. Only the holder session
// receives offer/cancel traffic; goodbye releases the session immediately.
// Every outbound frame is self-validated by the same decoder before it leaves.
func (b *Bridge) Sync(ctx context.Context, sessionID string, goodbye bool, frames []json.RawMessage) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sessionID == "" {
		sessionID = legacySessionID
	}
	if goodbye {
		b.release(sessionID)
		return nil, nil
	}
	now := b.now()
	b.prunePending(now)
	if b.holder != nil && b.holder.ID == sessionID {
		b.holder.LastSyncAt = now
	} else if session, ok := b.pending[sessionID]; ok {
		session.LastSyncAt = now
		// Succession: a silent or departed holder yields to the session that
		// is demonstrably alive right now.
		if b.holder == nil {
			b.promote(session, "previous holder disconnected")
		} else if now.Sub(b.holder.LastSyncAt) > sessionStaleAfter {
			stale := b.holder
			delete(b.pending, stale.ID) // do not resurrect a silent holder
			b.promote(session, "holder "+shortSession(stale.ID)+" went silent")
			delete(b.pending, stale.ID)
		}
	}

	var out []json.RawMessage
	if b.holder != nil && b.holder.ID == sessionID && b.holder.needsAck {
		b.holder.needsAck = false
		if b.holder.Outdated {
			// A promoted session skipped the hello path where the outdated
			// gate normally answers; staying silent would leave an
			// incompatible extension holding the bridge unaware.
			outdated, err := b.extensionOutdatedError()
			if err != nil {
				return nil, err
			}
			out = append(out, outdated...)
		} else {
			ack, err := b.helloAck()
			if err != nil {
				return nil, err
			}
			out = append(out, ack)
		}
	}
	for _, raw := range frames {
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidFrame, err)
		}
		if msg.Type != protocol.MsgHello && !b.knownSession(sessionID) {
			return b.helloRequired()
		}
		replies, err := b.handle(ctx, sessionID, msg)
		if err != nil {
			return nil, err
		}
		out = append(out, replies...)
		if b.holder != nil && b.holder.ID == sessionID && b.holder.Outdated {
			return out, nil
		}
	}
	if !b.knownSession(sessionID) {
		required, err := b.helloRequired()
		if err != nil {
			return nil, err
		}
		return append(out, required...), nil
	}
	if b.holder == nil || b.holder.ID != sessionID || b.holder.Outdated {
		// Pending sessions poll but never receive offer/cancel traffic.
		return out, nil
	}
	polled, err := b.poll(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, polled...), nil
}

// knownSession reports whether the session already completed a hello this
// daemon run. The caller holds b.mu.
func (b *Bridge) knownSession(sessionID string) bool {
	if b.holder != nil && b.holder.ID == sessionID {
		return true
	}
	_, ok := b.pending[sessionID]
	return ok
}

// release forgets a departing session. The caller holds b.mu.
func (b *Bridge) release(sessionID string) {
	delete(b.pending, sessionID)
	if b.holder != nil && b.holder.ID == sessionID {
		log.Printf("papio: browser session %s (v%s) disconnected", shortSession(sessionID), b.holder.ExtensionVersion)
		b.holder = nil
		b.epoch++
	}
}

// prunePending drops pending sessions whose native host stopped syncing
// without a goodbye. The caller holds b.mu.
func (b *Bridge) prunePending(now time.Time) {
	for id, session := range b.pending {
		if now.Sub(session.LastSyncAt) > pendingExpireAfter {
			delete(b.pending, id)
		}
	}
}

// helloAck builds the capability acknowledgement frame. The caller holds b.mu.
func (b *Bridge) helloAck() (json.RawMessage, error) {
	return b.frame(protocol.MsgHelloAck, "", protocol.HelloAckPayload{
		DaemonVersion:   b.Version,
		Features:        b.Features,
		ResolverOrigins: b.cfg.ResolverOrigins(),
	})
}

// handle dispatches one decoded inbound frame from sessionID.
func (b *Bridge) handle(ctx context.Context, sessionID string, msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	if msg.Type == protocol.MsgHello {
		return b.handleHello(sessionID, msg.Payload.(*protocol.HelloPayload))
	}
	session := b.sessionByID(sessionID)
	if session == nil {
		return b.helloRequired()
	}
	if b.holder == nil || b.holder.ID != sessionID {
		switch msg.Type {
		case protocol.MsgPageAcquire, protocol.MsgTriageSnapshotRequest, protocol.MsgTriageCountsRequest,
			protocol.MsgTriageDecide, protocol.MsgHumanActionResolve, protocol.MsgReviewPreviewRequest:
			// Stateless request/response traffic works from any browser —
			// even an outdated one; every frame is protocol-validated
			// regardless of version. "Acquire this page" and the inbox must
			// not depend on who holds the handoff flow.
		default:
			// Offer/handoff frames from a non-holder are refused: acting on
			// them is exactly the silent session fight this arbitration
			// exists to prevent.
			return b.sessionBusy(msg.JobID)
		}
	} else if session.Outdated {
		// The outdated gate is holder-only: it protects the offer/handoff
		// flow, which is the only surface with version-coupled semantics.
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

// handleHello arbitrates a hello from sessionID. The holder keeps the
// session; a hello from another browser waits as pending unless the holder
// has gone silent. Legacy hosts (no session_id) cannot be arbitrated and keep
// the historical last-hello-wins behavior in both directions.
func (b *Bridge) handleHello(sessionID string, p *protocol.HelloPayload) ([]json.RawMessage, error) {
	now := b.now()
	session := &browserSession{
		ID:               sessionID,
		ExtensionVersion: p.ExtensionVersion,
		AdapterVersions:  p.AdapterVersions,
		HelloAt:          now,
		LastSyncAt:       now,
		Outdated:         compareVersion(p.ExtensionVersion, MinExtensionVersion) < 0,
	}
	holderAlive := b.holder != nil && now.Sub(b.holder.LastSyncAt) <= sessionStaleAfter
	sameSession := b.holder != nil && b.holder.ID == sessionID
	legacyInvolved := sessionID == legacySessionID || (b.holder != nil && b.holder.ID == legacySessionID)
	if b.holder != nil && holderAlive && !sameSession && !legacyInvolved {
		b.pending[sessionID] = session
		b.deniedHellos++
		log.Printf("papio: browser session %s (v%s) denied: session held by %s (v%s)",
			shortSession(sessionID), session.ExtensionVersion, shortSession(b.holder.ID), b.holder.ExtensionVersion)
		return b.sessionBusy("")
	}
	if b.holder != nil && !sameSession {
		b.takeovers++
		log.Printf("papio: browser session %s (v%s) took over from %s: holder silent or legacy",
			shortSession(sessionID), session.ExtensionVersion, shortSession(b.holder.ID))
	}
	delete(b.pending, sessionID)
	if b.holder == nil || b.holder.ID != session.ID {
		b.epoch++
	}
	b.holder = session
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	if session.Outdated {
		return b.extensionOutdatedError()
	}
	ack, err := b.helloAck()
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{ack}, nil
}

// sessionByID resolves holder or pending. The caller holds b.mu.
func (b *Bridge) sessionByID(sessionID string) *browserSession {
	if b.holder != nil && b.holder.ID == sessionID {
		return b.holder
	}
	return b.pending[sessionID]
}

// sessionBusy tells a non-holder browser who owns the bridge and how to
// switch. Delivered as an ordinary error frame so old extensions log it
// instead of breaking.
func (b *Bridge) sessionBusy(jobID string) ([]json.RawMessage, error) {
	holderVersion := ""
	if b.holder != nil {
		holderVersion = b.holder.ExtensionVersion
	}
	frame, err := b.frame(protocol.MsgError, jobID, protocol.ErrorPayload{
		Code:    "session_busy",
		Message: "another browser holds the papio session (v" + holderVersion + "); run 'papio browser sessions' then 'papio browser use' to switch",
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
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
				payload.RequiresAuth, payload.BlockedBy = action.RequiresAuth, action.BlockedBy
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
	if request.Verdict == "dismiss" {
		if _, err := b.jobs.DismissHumanAction(ctx, request.ActionID, request.ExpectedRevision); err != nil {
			if errors.Is(err, job.ErrConflict) {
				return b.humanActionResolveResult(request.RequestID, "conflict", "")
			}
			return b.humanActionResolveResult(request.RequestID, "error", err.Error())
		}
		if b.preview != nil {
			b.preview.Revoke(request.ActionID)
		}
		return b.humanActionResolveResult(request.RequestID, "applied", "")
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
		return b.reviewPreviewError(request.RequestID, "review preview is not configured")
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return b.reviewPreviewError(request.RequestID, "review preview is temporarily unavailable")
	}
	var action *job.HumanAction
	for i := range actions {
		if actions[i].ID == request.ActionID {
			action = &actions[i]
			break
		}
	}
	if action == nil || action.Kind != "verify_identity" {
		return b.reviewPreviewError(request.RequestID, fmt.Sprintf("review action %d is unavailable", request.ActionID))
	}
	info, err := os.Stat(action.QuarantinePath)
	if err != nil || !info.Mode().IsRegular() {
		return b.reviewPreviewError(request.RequestID, fmt.Sprintf("review action %d preview is unavailable", request.ActionID))
	}
	url, err := b.preview.Issue(action.ID, action.QuarantinePath, action.QuarantineSHA256, info.Size(), previewCapabilityTTL)
	if err != nil {
		return b.reviewPreviewError(request.RequestID, "preview could not be issued")
	}
	frame, err := b.frame(protocol.MsgReviewPreviewResult, "", protocol.ReviewPreviewResultPayload{
		RequestID: request.RequestID, Outcome: "ok", URL: url, SHA256: action.QuarantineSHA256, SizeBytes: info.Size(),
		ExpiresAt: b.now().UTC().Add(previewCapabilityTTL).Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// reviewPreviewError reports an ordinary, expected preview failure (not
// configured, action gone, file missing, issuance failure) as a structured
// review_preview_result frame instead of a raw Go error. A raw error here
// would propagate through Sync into the native host's fatal error path
// (internal/nativehost/host.go: any browser.sync error tears the whole
// native-messaging connection down), turning a routine "this PDF is no
// longer available" into a hard disconnect on every click.
func (b *Bridge) reviewPreviewError(requestID, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgReviewPreviewResult, "", protocol.ReviewPreviewResultPayload{
		RequestID: requestID, Outcome: "error", Detail: truncate(detail, 1000),
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
	// adoptOutsideSessionLock releases b.mu; a concurrent claim/takeover in
	// that window must abort this poll — its offers would go to a demoted
	// session and its bookkeeping would pollute the new holder's maps.
	epoch := b.epoch
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
			err := b.adoptOutsideSessionLock(ctx, row.ID, name)
			if b.epoch != epoch {
				return out, nil
			}
			if err != nil {
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
// handoff. The job stays awaiting_human and the action stays open: the human
// is mid-recovery, and the extension re-drives the handoff tab through the
// resolver itself (re-offering here would duplicate the frame — the offer URL
// is deterministic, and the extension never renavigates an already-tracked
// tab on a repeat job_offer). Unknown jobs or jobs without an open handoff
// are dropped fail-closed.
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
