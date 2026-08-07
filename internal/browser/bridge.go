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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/app"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/preview"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
	"papio/internal/zotio"
)

const (
	handoffActionKind = "openurl_handoff"
	// MinExtensionVersion: 0.5.0 renamed the wire access mode to "delegated";
	// older extensions fail-closed on offers carrying it.
	MinExtensionVersion          = "0.5.0"
	pageAcquireFeature           = "page_acquire"
	triageSnapshotFeature        = "triage_snapshot_v1"
	triageSnapshotSchema2Feature = "triage_snapshot_schema_v2"
	triageMutationsFeature       = "triage_mutations_v1"
	reviewPreviewFeature         = "review_preview_v1"
	statsFeature                 = "browser_stats_v1"
	pageCaptureFeature           = "page_capture_v1"
	pageCaptureRequestFeature    = "page_capture_request_v1"
	activityFeedFeature          = "activity_feed_v1"
	triageCountsSchema2Feature   = "triage_counts_schema_v2"
	sessionEvidenceFeature       = "session_evidence_v1"
	deliveryContextFeature       = "delivery_context_v1"
	pageCaptureTermsFeature      = "page_capture_terms_v1"
	pageBulkAcquireFeature       = "page_bulk_acquire_v1"
	triageSnapshotSchema3Feature = "triage_snapshot_schema_v3"
	// pageBulkConsumer is the sole daemon-assigned consumer for every job
	// created through page_bulk_submit_request (ADR-0019 Decision 6). The
	// extension never supplies it.
	pageBulkConsumer         = "browser-page"
	previewCapabilityTTL     = 10 * time.Minute
	sessionEvidenceThrottle  = 60 * time.Second
	deliveryContextTTL       = 60 * time.Second
	maxOutstandingOffers     = 4
	maxInstitutionalReoffers = 4
	handoffPageLimit         = 500
	// maxFocusFramesPerPoll bounds the focus batch in ONE sync response. Focus
	// requests accumulate from a caller-supplied job-id list, so an unbounded
	// drain is the only term that can push a response past ipc.MaxResultBytes —
	// and an oversized response fails the host's browser.sync, which the host
	// treats as fatal and answers with goodbye, killing the live session. The
	// remainder stays queued for the next ordinary poll. Pinned by
	// TestSyncResponseFitsResultCap.
	maxFocusFramesPerPoll = 32
)

// HandoffFocusMinExtensionVersion is the first extension that parses
// handoff_focus. An older extension fails closed on an unknown type and
// disconnects the whole session, so the frame is withheld below this floor.
const HandoffFocusMinExtensionVersion = "0.8.0"

// CaptureRequestIDMinExtensionVersion is the first extension that echoes
// page_capture.request_id. Below it the daemon cannot bind a content frame to
// the request that asked for it, so pageCapture leaves pending.path empty and
// the page_capture_request_result handler rewrites a genuine "captured" into
// "nav_failed: capture content was not stored" — reporting failure for a page
// that was captured and stored. Refuse the capture up front instead: a named
// refusal beats a false failure, and this is a per-feature floor rather than a
// MinExtensionVersion bump on purpose, because an older extension still runs
// every offer and handoff correctly. Only adapter capture is version-coupled.
const CaptureRequestIDMinExtensionVersion = "0.10.0"

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
type browserDownloadKey struct {
	JobID      string
	DownloadID int64
}

type pendingBrowserDownload struct {
	Filename    string
	CandidateID int64
	ReceivedAt  time.Time
}
type pendingDeliveryContext struct {
	Payload    protocol.DeliveryContextPayload
	ReceivedAt time.Time
}

// CaptureRequest is the daemon RPC input for one browser-driven fixture
// capture. RequestID is generated by the bridge and never accepted from IPC.
type CaptureRequest struct {
	URL      string
	Provider string
	Scenario string
	SettleMS *int64
}

// CaptureResult is a routine, structured capture outcome. Path is populated
// only after the existing page_capture store path succeeds.
type CaptureResult struct {
	RequestID string `json:"request_id,omitempty"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	Path      string `json:"path,omitempty"`
}

type pendingPageCapture struct {
	payload   protocol.PageCaptureRequestPayload
	sessionID string
	delivered bool
	path      string
	result    chan CaptureResult
}

// holdingsProvider is the ownership seam page-bulk acquisition needs. The
// concrete ownership.Registry satisfies it; Enabled distinguishes an active
// authority from nil/empty configuration without exposing registry details.
type holdingsProvider interface {
	Enabled() bool
	Lookup(context.Context, []ownership.Query) ownership.Result
}

// Bridge is the per-daemon-run browser bridge. Sessions are tracked in
// memory: each native-host process carries a session_id, exactly one session
// holds the offer/handoff flow, and later hellos from other browsers wait as
// pending instead of silently stealing the session. A fresh hello from the
// holder still resets the offered/cancelled bookkeeping, which is exactly the
// recovery an MV3 service-worker restart needs.
type Bridge struct {
	jobs     *job.Store
	svc      *app.Service
	holdings holdingsProvider
	// zotio answers library ownership for page_bulk_status when configured.
	// nil is a supported, first-class mode (ADR-0008: zotio and the generic
	// holdings registry never mix) — page_bulk_status then never calls
	// zotio and behaves exactly as it did before ownership was wired in.
	zotio        *zotio.Service
	triage       *triage.Service
	watchRunner  *watch.Runner
	preview      *preview.Server
	captureStore *captures.Store
	cfg          config.Config

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
	// a concurrent claim/takeover must not let a resumed poll send offers to a
	// demoted session or pollute the new holder's bookkeeping.
	epoch      uint64
	offered    map[string]bool // handoff jobs offered to the current holder
	cancelSent map[string]bool // jobs a daemon-side cancel was already announced for
	// A replayed auth return must not make the same holder open duplicate tabs.
	authReleased map[int64]bool
	// reofferPending prioritizes jobs released by the institutional-session
	// sweep when poll turns them back into job_offer frames.
	reofferPending map[string]bool
	// reofferSourceJobID and reofferProfile keep an authenticated institutional
	// session alive across ordinary sync ticks, even after its source settles.
	reofferSourceJobID string
	reofferProfile     string
	// Session evidence is timing-only and throttled per holder.
	lastSessionEvidenceAt time.Time
	// One reoffer sweep per sync cycle; the handler that triggered it lets poll
	// finish the same cycle without immediately running a second burst.
	reofferRanThisSync bool
	// lastPacedHeld deduplicates the operator-visible pacing event while a
	// backlog remains unchanged across the native host's two-second polls.
	lastPacedHeld int
	// Completion metadata and delivery context arrive as adjacent extension
	// frames. Keep both briefly so either frame order is safe across a native
	// host RPC boundary; values are never durable and expire quickly.
	pendingDownloads map[browserDownloadKey]pendingBrowserDownload
	deliveryContexts map[browserDownloadKey]pendingDeliveryContext
	// Focus requests survive a holder change so the replacement holder can
	// receive its offer before it is asked to surface the handoff.
	focusPending map[string]bool
	// Capture directives ride the holder's ordinary poll. One request per
	// session keeps the unchanged page_capture content frame unambiguous.
	pendingCaptures map[string]*pendingPageCapture
	now             func() time.Time
	// readDir is the adoption-directory ReadDir seam; nil in production,
	// where readAdoptionDir falls back to os.ReadDir. Tests substitute a
	// blocking or error-returning func to exercise adoptionScanSuspended
	// below without a real TCC-protected filesystem.
	readDir func(string) ([]os.DirEntry, error)
	// adoptionScanMu guards the field below. It is deliberately its own
	// lock, never b.mu: a ReadDir call that hangs behind a TCC consent wall
	// (see scanAdoptionDir) must never hold the session lock, or every other
	// bridge RPC wedges behind it — the exact incident this latch exists to
	// prevent.
	adoptionScanMu sync.Mutex
	// adoptionScanSuspended latches true the instant one adoption-directory
	// ReadDir call misses adoptionScanDeadline. A hung syscall can block
	// forever, so every scan while this is true short-circuits to "nothing
	// adoptable" without spawning another goroutine — at most one hung call
	// is ever outstanding per bridge. The goroutine that tripped it clears
	// the flag, and logs the recovery, the moment it finally returns.
	adoptionScanSuspended bool
}

// browserSession is one native-host connection that said hello.
type browserSession struct {
	ID               string
	ExtensionVersion string
	AdapterVersions  map[string]string
	HelloAt          time.Time
	LastSyncAt       time.Time
	Outdated         bool
	// adapterUpgradeRepairPending lets a newly live holder repair parks once
	// without turning each two-second browser poll into a maintenance sweep.
	adapterUpgradeRepairPending bool
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
// zotioService is nil when zotio is not configured (ADR-0008 exclusivity
// with holdings) — page_bulk_status then never consults it.
func NewBridge(jobs *job.Store, svc *app.Service, triageService *triage.Service, watchRunner *watch.Runner, previewServer *preview.Server, captureStore *captures.Store, holdings holdingsProvider, zotioService *zotio.Service, cfg config.Config, version string) *Bridge {
	required := []string{
		pageAcquireFeature, triageSnapshotFeature, triageSnapshotSchema2Feature, triageMutationsFeature, reviewPreviewFeature, statsFeature, pageCaptureFeature, pageCaptureRequestFeature, activityFeedFeature, triageCountsSchema2Feature, sessionEvidenceFeature, deliveryContextFeature, pageCaptureTermsFeature, pageBulkAcquireFeature, triageSnapshotSchema3Feature,
	}
	return &Bridge{
		jobs: jobs, svc: svc, triage: triageService, watchRunner: watchRunner, preview: previewServer, captureStore: captureStore, holdings: holdings, zotio: zotioService, cfg: cfg,
		Version:          version,
		Features:         required,
		offered:          map[string]bool{},
		cancelSent:       map[string]bool{},
		authReleased:     map[int64]bool{},
		reofferPending:   map[string]bool{},
		pendingDownloads: map[browserDownloadKey]pendingBrowserDownload{},
		deliveryContexts: map[browserDownloadKey]pendingDeliveryContext{},
		focusPending:     map[string]bool{},
		pendingCaptures:  map[string]*pendingPageCapture{},
		pending:          map[string]*browserSession{},
		now:              time.Now,
		readDir:          os.ReadDir,
	}
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

// FocusHandoffs queues compatible holder sessions to surface tracked handoffs.
// A missing, stale, legacy, or pre-handoff_focus holder is a normal fallback
// condition: callers must use the OS launcher rather than treating a routine
// browser absence as a bridge failure.
func (b *Bridge) FocusHandoffs(ctx context.Context, jobIDs []string) (queued int, sessionLive bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.holder == nil ||
		b.holder.ID == legacySessionID ||
		b.now().Sub(b.holder.LastSyncAt) > sessionStaleAfter ||
		compareVersion(b.holder.ExtensionVersion, HandoffFocusMinExtensionVersion) < 0 {
		return 0, false, nil
	}

	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return 0, true, err
	}
	handoff := make(map[string]bool, len(actions))
	for _, action := range actions {
		if action.Kind == handoffActionKind {
			handoff[action.JobID] = true
		}
	}
	for _, jobID := range jobIDs {
		if jobID == "" || b.focusPending[jobID] || !handoff[jobID] {
			continue
		}
		row, getErr := b.jobs.Get(ctx, jobID)
		switch {
		case errors.Is(getErr, sql.ErrNoRows):
			continue
		case getErr != nil:
			return queued, true, getErr
		case row.State != job.StateAwaitingHuman:
			continue
		}
		// A job the offer loop will skip must not be counted as queued. Doing so
		// reports success for a focus that never happens — no frame, no event,
		// nothing on screen — and suppresses the CLI's own explicit-open
		// fallback, so the user is told papio acted and it did not.
		if _, offerable := b.offerableAccessMode(*row); !offerable {
			continue
		}
		b.focusPending[jobID] = true
		queued++
	}
	return queued, true, nil
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
		if capture := b.pendingCaptures[b.holder.ID]; capture != nil {
			delete(b.pendingCaptures, b.holder.ID)
			capture.result <- CaptureResult{
				RequestID: capture.payload.RequestID,
				Outcome:   "nav_failed",
				Detail:    "browser session changed during page capture",
			}
		}
	}
	delete(b.pending, session.ID)
	session.needsAck = true
	// A pending browser has not been allowed to offer work, so it must check
	// upgrade repairs when it becomes the live holder.
	session.adapterUpgradeRepairPending = len(session.AdapterVersions) != 0
	b.holder = session
	b.epoch++
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	b.authReleased = map[int64]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = ""
	b.reofferProfile = ""
	b.lastSessionEvidenceAt = time.Time{}
	b.lastPacedHeld = 0
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
	b.reofferRanThisSync = false
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
	b.repairAdapterUpgradeParks(ctx)
	polled, err := b.poll(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, polled...), nil
}

// repairAdapterUpgradeParks keeps maintenance failures local to the daemon:
// a repair read or write must not disconnect the user's native browser session.
// The caller holds b.mu.
func (b *Bridge) repairAdapterUpgradeParks(ctx context.Context) {
	if b.holder == nil ||
		b.holder.Outdated ||
		!b.holder.adapterUpgradeRepairPending ||
		len(b.holder.AdapterVersions) == 0 ||
		b.now().Sub(b.holder.LastSyncAt) > sessionStaleAfter {
		return
	}
	b.holder.adapterUpgradeRepairPending = false
	if b.svc == nil {
		return
	}
	if err := b.svc.HandoffRepairer().RepairAdapterUpgrade(ctx, b.holder.ExtensionVersion, extensionVersionNewer); err != nil {
		log.Printf("papio: repairing provider parks after extension upgrade: %v", err)
	}
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

// Capture queues one directive for the current holder and waits for its
// correlated result. Routine absence, contention, and timeout stay structured.
func (b *Bridge) Capture(ctx context.Context, request CaptureRequest) CaptureResult {
	b.mu.Lock()
	now := b.now()
	if b.holder == nil || b.holder.Outdated || now.Sub(b.holder.LastSyncAt) > sessionStaleAfter {
		b.mu.Unlock()
		return CaptureResult{Outcome: "not_permitted", Detail: "no compatible browser session is connected"}
	}
	if compareVersion(b.holder.ExtensionVersion, CaptureRequestIDMinExtensionVersion) < 0 {
		version := b.holder.ExtensionVersion
		b.mu.Unlock()
		return CaptureResult{
			Outcome: "not_permitted",
			Detail: fmt.Sprintf("browser extension %s cannot correlate a capture; update to %s or newer",
				version, CaptureRequestIDMinExtensionVersion),
		}
	}
	sessionID := b.holder.ID
	if _, exists := b.pendingCaptures[sessionID]; exists {
		b.mu.Unlock()
		return CaptureResult{Outcome: "busy", Detail: "a page capture is already outstanding for this browser session"}
	}
	requestID := newMsgID()
	pending := &pendingPageCapture{
		payload: protocol.PageCaptureRequestPayload{
			RequestID: requestID,
			URL:       request.URL,
			Provider:  request.Provider,
			Scenario:  request.Scenario,
			SettleMS:  request.SettleMS,
		},
		sessionID: sessionID,
		result:    make(chan CaptureResult, 1),
	}
	b.pendingCaptures[sessionID] = pending
	b.mu.Unlock()

	select {
	case result := <-pending.result:
		return result
	case <-ctx.Done():
		// The result and the timeout can become ready in the same instant:
		// pending.result is buffered (cap 1), and a deliverer (handle's
		// page_capture_request_result case, release, or promote) may have
		// already sent into it right as ctx expires. Go's select then
		// chooses pseudo-randomly between the two ready cases, so this arm
		// can run even though a result is already sitting in the channel —
		// reporting "timeout" for a capture that actually succeeded, while
		// the file it already stored is silently orphaned (on disk, never
		// reported). Re-check the channel in the same b.mu critical section
		// that would otherwise clear the pending entry: whichever side (the
		// deliverer's delete+send, or this recheck) reaches the lock first
		// wins outright, so a delivered result is never dropped and the
		// pending/busy entry is still cleared exactly once on every path.
		b.mu.Lock()
		select {
		case result := <-pending.result:
			b.mu.Unlock()
			return result
		default:
		}
		if b.pendingCaptures[sessionID] == pending {
			delete(b.pendingCaptures, sessionID)
		}
		b.mu.Unlock()
		return CaptureResult{RequestID: requestID, Outcome: "timeout", Detail: "browser page capture timed out"}
	}
}

// release forgets a departing session. The caller holds b.mu.
func (b *Bridge) release(sessionID string) {
	delete(b.pending, sessionID)
	if b.holder != nil && b.holder.ID == sessionID {
		log.Printf("papio: browser session %s (v%s) disconnected", shortSession(sessionID), b.holder.ExtensionVersion)
		b.holder = nil
		b.epoch++
		b.reofferSourceJobID = ""
		b.reofferProfile = ""
		b.reofferPending = map[string]bool{}
	}
	if pending := b.pendingCaptures[sessionID]; pending != nil {
		delete(b.pendingCaptures, sessionID)
		pending.result <- CaptureResult{
			RequestID: pending.payload.RequestID,
			Outcome:   "nav_failed",
			Detail:    "browser session disconnected during page capture",
		}
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
			protocol.MsgTriageDecide, protocol.MsgHumanActionResolve, protocol.MsgDeliveryReconcileRequest, protocol.MsgReviewPreviewRequest, protocol.MsgStatsRequest, protocol.MsgActivityRequest, protocol.MsgPageCapture,
			protocol.MsgPageBulkStatusRequest, protocol.MsgPageBulkSubmitRequest:
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

	case protocol.MsgDeliveryReconcileRequest:
		return b.deliveryReconcile(ctx, msg.Payload.(*protocol.DeliveryReconcilePayload))

	case protocol.MsgReviewPreviewRequest:
		return b.reviewPreview(ctx, msg.Payload.(*protocol.ReviewPreviewRequestPayload))

	case protocol.MsgStatsRequest:
		return b.stats(ctx, msg.Payload.(*protocol.StatsRequestPayload))
	case protocol.MsgActivityRequest:
		return b.activity(ctx, msg.Payload.(*protocol.ActivityRequestPayload))

	case protocol.MsgPageBulkStatusRequest:
		return b.pageBulkStatus(ctx, msg.Payload.(*protocol.PageBulkStatusRequestPayload))

	case protocol.MsgPageBulkSubmitRequest:
		return b.pageBulkSubmit(ctx, msg.Payload.(*protocol.PageBulkSubmitRequestPayload))

	case protocol.MsgPageCapture:
		b.pageCapture(ctx, sessionID, msg.JobID, msg.Payload.(*protocol.PageCapturePayload))
		return nil, nil

	case protocol.MsgPageCaptureRequestResult:
		p := msg.Payload.(*protocol.PageCaptureRequestResultPayload)
		pending := b.pendingCaptures[sessionID]
		if pending == nil || pending.payload.RequestID != p.RequestID {
			return nil, nil
		}
		outcome := p.Outcome
		detail := p.Detail
		if outcome == "captured" && pending.path == "" {
			outcome = "nav_failed"
			detail = "capture content was not stored"
		}
		delete(b.pendingCaptures, sessionID)
		pending.result <- CaptureResult{
			RequestID: p.RequestID,
			Outcome:   outcome,
			Detail:    detail,
			Path:      pending.path,
		}
		return nil, nil

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
		return nil, b.leaveHandoff(ctx, msg.JobID, job.StateUnavailable, string(job.TerminalReasonBrowserRejected))

	case protocol.MsgAuthPending, protocol.MsgAuthReturned:
		return nil, b.recordAuth(ctx, msg)

	case protocol.MsgSessionEvidence:
		return nil, b.sessionEvidence(ctx, msg.Payload.(*protocol.SessionEvidencePayload))
	case protocol.MsgDeliveryContext:
		return nil, b.deliveryContext(ctx, msg.JobID, msg.Payload.(*protocol.DeliveryContextPayload))
	case protocol.MsgDownloadStarted:
		p := msg.Payload.(*protocol.DownloadStartedPayload)
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_started",
			map[string]any{"download_id": p.DownloadID, "filename": p.Filename})

	case protocol.MsgDownloadComplete:
		p := msg.Payload.(*protocol.DownloadCompletePayload)
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_complete",
			map[string]any{"download_id": p.DownloadID, "filename": p.Filename, "size_bytes": p.SizeBytes}); err != nil {
			return nil, err
		}
		b.pruneDeliveryMetadata(b.now())
		key := browserDownloadKey{JobID: msg.JobID, DownloadID: p.DownloadID}
		pendingDownload := pendingBrowserDownload{Filename: p.Filename, ReceivedAt: b.now()}
		b.pendingDownloads[key] = pendingDownload
		var context *app.BrowserDeliveryContext
		if pending, ok := b.deliveryContexts[key]; ok {
			context = browserDeliveryContext(&pending.Payload)
		}
		candidateID, err := b.adoptOutsideSessionLock(ctx, msg.JobID, p.Filename, context)
		if err != nil {
			// Environmental failure (file not there yet, Chrome rename race,
			// user saved elsewhere) must not sever the bridge: record it, keep
			// the job parked, and let the poll-time directory scan pick the
			// file up when it appears. Confinement violations land here too —
			// the report is ignored and the job simply stays awaiting_human.
			if evErr := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.adoption_deferred",
				map[string]any{"filename": p.Filename, "reason": truncate(err.Error(), 200)}); evErr != nil {
				return nil, evErr
			}
		} else {
			pendingDownload.CandidateID = candidateID
			b.pendingDownloads[key] = pendingDownload
			if context != nil {
				delete(b.deliveryContexts, key)
				delete(b.pendingDownloads, key)
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
		return nil, b.jobs.Cancel(ctx, msg.JobID, job.TerminalReasonBrowserCancelled)

	case protocol.MsgError:
		// Only the normalized code is durable; the free-text message is
		// extension-supplied and never persisted.
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.error",
			map[string]any{"code": msg.Payload.(*protocol.ErrorPayload).Code})

	default:
		return nil, fmt.Errorf("%w: unexpected inbound frame type %q", ErrInvalidFrame, msg.Type)
	}
}

// pageCapture treats diagnostic content failures as local losses: disconnecting
// the native session over a bad fixture would discard the handoff it was meant
// to help diagnose.
func (b *Bridge) pageCapture(ctx context.Context, sessionID, jobID string, payload *protocol.PageCapturePayload) {
	if !b.cfg.Captures.Enabled || b.captureStore == nil {
		return
	}
	html, err := decodePageCapture(payload)
	if err != nil {
		log.Printf("papio: ignoring page capture from %s: %v", payload.Host, err)
		return
	}
	path, err := b.captureStore.Store(ctx, payload.Host, payload.Scenario, payload.AdapterID, payload.AdapterVersion, html)
	if err != nil {
		log.Printf("papio: storing page capture from %s: %v", payload.Host, err)
		return
	}
	// Correlate strictly on the echoed request id. A requested capture carries
	// the RequestID of the page_capture_request it answers; an UNSOLICITED one
	// (the developer capture panel's captureFixture, which answers no pending
	// request at all) carries none, so it can no longer satisfy a
	// CLI-initiated `papio adapter capture` waiting on the same session for
	// the same provider/scenario and hand that caller the wrong file path
	// (papio-85a7420f4cd2564f). This is the same key page_capture_request_result
	// already correlates on, so both halves of one requested capture now bind
	// through one identity.
	//
	// No provider+scenario fallback: that pair is exactly the ambiguous match
	// this replaces, and keeping it as a backstop would reopen the bug for
	// every frame that omits the id. Matching the pending request's
	// REQUESTED-URL host against payload.Host is not an alternative either —
	// it was tried and reverted (bc3f4b2). payload.Host comes from the content
	// frame's location.origin, the host the tab actually LANDED on, so any
	// ordinary cross-host redirect (www canonicalization, a CDN host swap, an
	// SSO round-trip) makes the two differ and turns a real "captured" into a
	// reported "nav_failed" with no path.
	if pending := b.pendingCaptures[sessionID]; pending != nil &&
		payload.RequestID != "" &&
		pending.payload.RequestID == payload.RequestID {
		pending.path = path
	}
	if jobID == "" || b.jobs == nil {
		return
	}
	if err := b.jobs.RecordEvent(ctx, jobID, "browser.page_capture", map[string]any{
		"host":            payload.Host,
		"scenario":        payload.Scenario,
		"adapter_id":      payload.AdapterID,
		"adapter_version": payload.AdapterVersion,
		"path":            path,
		"size_bytes":      len(html),
	}); err != nil {
		log.Printf("papio: recording page capture for job %s: %v", jobID, err)
	}
}

func decodePageCapture(payload *protocol.PageCapturePayload) ([]byte, error) {
	const maxPageCaptureBytes int64 = 2 << 20
	if payload.Bytes < 1 || payload.Bytes > maxPageCaptureBytes {
		return nil, fmt.Errorf("declared page capture size %d is out of range", payload.Bytes)
	}
	compressed, err := base64.StdEncoding.Strict().DecodeString(payload.Body)
	if err != nil {
		return nil, fmt.Errorf("decoding page capture body: %w", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("opening page capture gzip body: %w", err)
	}
	html, err := io.ReadAll(io.LimitReader(reader, payload.Bytes+1))
	if err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("reading page capture gzip body: %w", err)
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("closing page capture gzip body: %w", err)
	}
	if int64(len(html)) != payload.Bytes {
		return nil, fmt.Errorf("decoded page capture size %d does not match declared %d", len(html), payload.Bytes)
	}
	return html, nil
}

// handleHello arbitrates a hello from sessionID. The holder keeps the
// session; a hello from another browser waits as pending unless the holder
// has gone silent. Legacy hosts (no session_id) cannot be arbitrated and keep
// the historical last-hello-wins behavior in both directions.
func (b *Bridge) handleHello(sessionID string, p *protocol.HelloPayload) ([]json.RawMessage, error) {
	now := b.now()
	session := &browserSession{
		ID:                          sessionID,
		ExtensionVersion:            p.ExtensionVersion,
		AdapterVersions:             p.AdapterVersions,
		HelloAt:                     now,
		LastSyncAt:                  now,
		Outdated:                    compareVersion(p.ExtensionVersion, MinExtensionVersion) < 0,
		adapterUpgradeRepairPending: len(p.AdapterVersions) != 0,
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
	holderChanged := b.holder == nil || b.holder.ID != session.ID
	if holderChanged {
		b.epoch++
		b.lastSessionEvidenceAt = time.Time{}
	}
	b.holder = session
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	b.authReleased = map[int64]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = ""
	b.reofferProfile = ""
	b.lastPacedHeld = 0
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
		payload, err := b.triageSnapshotPayload(ctx, request.RequestID, request.SchemaVersions[0], snapshot)
		if err != nil {
			return nil, err
		}
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

// triageSnapshotPayload builds one schema's wire shape from a triage
// snapshot. Schema 3 additionally populates attention/route_class/
// auth_requirement (dev/post-build-followups.md item 7) and, for a
// document_delivery human_action, looks up its live delivery_requests row —
// the only reason this needs ctx and can fail; the v1/v2 emission path
// below stays byte-identical to what it produced before triage-snapshot/3.
func (b *Bridge) triageSnapshotPayload(ctx context.Context, requestID string, schema int64, snapshot triage.Snapshot) (protocol.TriageSnapshotResponsePayload, error) {
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
			if schema == 3 {
				// A new watch hit is informational — nothing is blocked, and
				// papio is not doing anything on the operator's behalf yet.
				payload.Attention = "advisory"
			}
		case triage.KindHumanAction:
			action := item.HumanAction
			if action != nil {
				payload.ActionID, payload.JobID = action.ActionID, action.JobID
				payload.ActionKind, payload.JobState = action.ActionKind, action.JobState
				payload.Revision, payload.SHA256, payload.SizeBytes = action.Revision, action.SHA256, action.SizeBytes
				if schema >= 2 && action.BlockedBy != "" {
					payload.RequiresAuth, payload.BlockedBy = action.RequiresAuth, action.BlockedBy
				}
				if schema == 3 {
					payload.RouteClass = action.ActionKind
					payload.AuthRequirement = triageAuthRequirement(action.RequiresAuth)
					if action.ActionKind == job.ActionKindDocumentDelivery {
						payload.Delivery = b.triageDeliveryFor(ctx, action.JobID)
						// Decision 4's three named reconciliation operations
						// replace the generic dismiss/open ops a document_delivery
						// action would otherwise inherit — the delivery
						// sub-object already carries the request to display,
						// and there is no generic "open" destination.
						payload.Ops = []string{"open_request_history", "confirm_request_exists", "confirm_request_absent"}
					}
					payload.Attention = triageHumanActionAttention(action, payload.Delivery)
				}
			}
		case triage.KindRetraction:
			retraction := item.Retraction
			if retraction != nil {
				payload.DOI, payload.Nature = retraction.DOI, retraction.Nature
				payload.NoticedAt = retraction.NoticedAt.UTC().Format(time.RFC3339Nano)
				payload.NoticeDOI = retraction.NoticeDOI
			}
			if schema == 3 {
				// A retraction/correction/concern notice: informational, not
				// blocking anything (r5's "integrity notice = advisory").
				payload.Attention = "advisory"
			}
		}
		items = append(items, payload)
	}
	return protocol.TriageSnapshotResponsePayload{
		RequestID: requestID, Schema: schema, GeneratedAt: snapshot.GeneratedAt,
		Counts: triageCountsPayload(snapshot.Counts), Items: items, Cursor: snapshot.Cursor,
		HasMore: snapshot.HasMore, UnsupportedItemsCount: int64(snapshot.UnsupportedItemsCount),
	}, nil
}

// triageAuthRequirement is ADR-0016 Decision 4's tri-state carrier, derived
// from the existing RequiresAuth pointer without changing what that pointer
// means: nil is "unknown" (no auth classification has been observed for
// this action yet), and a non-nil value is the classified read.
func triageAuthRequirement(requiresAuth *bool) string {
	if requiresAuth == nil {
		return "unknown"
	}
	if *requiresAuth {
		return "true"
	}
	return "false"
}

// triageHumanActionAttention implements the settled attention mapping from
// dev/post-build-followups.md item 7: an unknown-auth openurl handoff (a
// library-link route papio has not yet observed a session state for) and a
// document_delivery item the provider already fulfilled proceed on their
// own (working); a document_delivery item still needing reconciliation, or
// any other action, needs a human decision (required). A known login/MFA
// boundary (RequiresAuth == true) is covered by the same "required" default.
func triageHumanActionAttention(action *triage.HumanAction, delivery *protocol.TriageDelivery) string {
	if action.ActionKind == job.ActionKindDocumentDelivery {
		if delivery != nil && delivery.State == "fulfilled" {
			return "working"
		}
		return "required"
	}
	if action.ActionKind == handoffActionKind && action.RequiresAuth == nil {
		return "working"
	}
	return "required"
}

// triageDeliveryFor looks up the live delivery_requests row for a
// document_delivery human action's job and maps it onto the wire's closed
// delivery sub-object. Both a missing row (nil, nil from GetByJobID — the
// action closed between the snapshot query and this lookup) and a lookup
// error degrade to an absent Delivery rather than failing the whole
// snapshot: propagating a routine SQLite hiccup out of triageSnapshot would
// abort the whole native-messaging session over one item, the exact
// reviewPreview-class footgun AGENTS.md warns against (a non-nil error from
// a browser-bridge RPC handler kills the session, not just the request).
func (b *Bridge) triageDeliveryFor(ctx context.Context, jobID string) *protocol.TriageDelivery {
	if b.svc == nil || b.svc.Delivery == nil {
		return nil
	}
	req, err := b.svc.Delivery.GetByJobID(ctx, jobID)
	if err != nil {
		log.Printf("papio: looking up delivery request for job %s: %v", jobID, err)
		return nil
	}
	if req == nil {
		return nil
	}
	return &protocol.TriageDelivery{
		Provider: req.Provider, ProviderReference: req.ProviderReference, State: string(req.State),
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

func triageCountsPayloadV2(counts triage.Counts) protocol.TriageCounts {
	payload := triageCountsPayload(counts)
	auth := int64(counts.ActionsRequiresAuth)
	payload.ActionsRequiresAuth = &auth
	return payload
}

func (b *Bridge) triageCounts(ctx context.Context, request *protocol.TriageCountsRequestPayload) ([]json.RawMessage, error) {
	if b.triage == nil {
		return b.triageUnavailable(nil)
	}
	counts, err := b.triage.Counts(ctx)
	if err != nil {
		return b.triageUnavailable(err)
	}
	payload := triageCountsPayload(counts)
	if len(request.SchemaVersions) == 1 && request.SchemaVersions[0] == 2 {
		payload = triageCountsPayloadV2(counts)
	}
	frame, err := b.frame(protocol.MsgTriageCountsResponse, "", protocol.TriageCountsResponsePayload{
		RequestID: request.RequestID, Counts: payload,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// unavailable reports a routine read-model failure (no triage service
// configured, or the query failed) as an ordinary error frame instead of a
// raw Go error. Neither TriageCountsResponsePayload nor StatsResponsePayload
// has an outcome/detail field and neither may gain one: both protocol
// decoders fail closed on unknown fields, so a new field on an existing
// message breaks every already-shipped extension. This mirrors
// sessionBusy/helloRequired/extensionOutdatedError instead. A raw Go error
// here would reach the native host's fatal path (internal/nativehost/host.go
// treats any browser.sync error as a dead connection), turning one failed
// query into a disconnect that also kills page_acquire and the handoff flow.
// The cause is logged rather than sent: the extension gets a stable code it
// can render as "temporarily unavailable", the operator keeps the diagnosis.
func (b *Bridge) unavailable(code, message, surface string, cause error) ([]json.RawMessage, error) {
	if cause != nil {
		log.Printf("papio: %s unavailable: %v", surface, cause)
	} else {
		log.Printf("papio: %s unavailable: no triage service configured", surface)
	}
	frame, err := b.frame(protocol.MsgError, "", protocol.ErrorPayload{Code: code, Message: message})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) triageUnavailable(cause error) ([]json.RawMessage, error) {
	return b.unavailable("triage_unavailable", "the triage inbox is temporarily unavailable", "triage counts", cause)
}

func statsPayload(requestID string, generatedAt string, stats triage.Stats) protocol.StatsResponsePayload {
	series := make([]protocol.StatsBucket, len(stats.Series))
	for i, bucket := range stats.Series {
		series[i] = protocol.StatsBucket{
			PeriodStart: bucket.PeriodStart.UTC().Format(time.RFC3339), Acquired: int64(bucket.Acquired),
		}
	}
	return protocol.StatsResponsePayload{
		RequestID: requestID, GeneratedAt: generatedAt,
		AcquiredTotal: int64(stats.AcquiredTotal), FailedTotal: int64(stats.FailedTotal),
		HandoffsRequired: int64(stats.HandoffsRequired),
		Access: protocol.StatsAccess{
			OpenAccess: int64(stats.Access.OpenAccess), Institutional: int64(stats.Access.Institutional),
			LicensedAPI: int64(stats.Access.LicensedAPI), Other: int64(stats.Access.Other),
		},
		Series: series,
	}
}

func (b *Bridge) stats(ctx context.Context, request *protocol.StatsRequestPayload) ([]json.RawMessage, error) {
	if b.triage == nil {
		return b.statsUnavailable(nil)
	}
	stats, err := b.triage.Stats(ctx)
	if err != nil {
		return b.statsUnavailable(err)
	}
	frame, err := b.frame(protocol.MsgStatsResponse, "",
		statsPayload(request.RequestID, b.now().UTC().Format(time.RFC3339), stats))
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) statsUnavailable(cause error) ([]json.RawMessage, error) {
	return b.unavailable("stats_unavailable", "acquisition stats are temporarily unavailable", "acquisition stats", cause)
}

// activity returns a bounded, display-only read model. A store read failure
// is routine from the browser's point of view, so it is logged and represented
// as an empty page rather than tearing down the native-messaging session.
func (b *Bridge) activity(ctx context.Context, request *protocol.ActivityRequestPayload) ([]json.RawMessage, error) {
	limit := request.Limit
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	entries := make([]protocol.ActivityEntryPayload, 0, limit)
	if b.jobs != nil && b.jobs.S != nil {
		recent, err := b.jobs.S.RecentEvents(int(limit), 0)
		if err != nil {
			log.Printf("papio: activity feed unavailable: %v", err)
		} else {
			for _, event := range recent {
				entry := protocol.ActivityEntryPayload{
					Seq:  event.Seq,
					At:   event.At.UTC().Format(time.RFC3339),
					Kind: activityKind(event.Kind),
					Text: store.ActivityText(event.Kind, event.Detail),
				}
				if event.JobID != "" {
					entry.JobID = event.JobID
				}
				if event.JobTitle != "" {
					entry.Title = activityTitle(event.JobTitle)
				}
				entries = append(entries, entry)
				if len(entries) == 50 {
					break
				}
			}
		}
	}
	frame, err := b.frame(protocol.MsgActivityResponse, "", protocol.ActivityResponsePayload{
		RequestID:   request.RequestID,
		GeneratedAt: b.now().UTC().Format(time.RFC3339),
		Entries:     entries,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func activityKind(value string) string {
	value = store.StripTerminalControls(value)
	runes := []rune(value)
	if len(runes) == 0 {
		return "unknown"
	}
	if len(runes) > 100 {
		return string(runes[:100])
	}
	return value
}

// activityTitle feeds the browser activity feed; it shares
// store.StripTerminalControls with the CLI/store paths so a title carrying
// ESC, BEL, or a raw C1 byte cannot inject escape sequences into whatever
// terminal later renders this feed — NUL-only stripping was the same gap the
// CLI row had before it was closed.
func activityTitle(value string) string {
	value = store.StripTerminalControls(value)
	runes := []rune(value)
	if len(runes) <= 500 {
		return value
	}
	return string(runes[:500])
}

func (b *Bridge) triageDecide(ctx context.Context, request *protocol.TriageDecidePayload) ([]json.RawMessage, error) {
	if b.triage == nil || b.watchRunner == nil {
		return b.triageDecisionResult(request.RequestID, "error", "triage mutations are not configured")
	}
	if strings.HasPrefix(request.ItemID, triage.RetractionIDPrefix) {
		return b.acknowledgeRetraction(ctx, request)
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

// acknowledgeRetraction clears one Crossref update notice. A retraction notice
// carries no watch digest to consume, so watch_scope is not consulted: the
// notice itself is the unit of dismissal.
func (b *Bridge) acknowledgeRetraction(ctx context.Context, request *protocol.TriageDecidePayload) ([]json.RawMessage, error) {
	if request.Op != "dismiss" {
		return b.triageDecisionResult(request.RequestID, "error", "retraction notices support only the dismiss operation")
	}
	applied, err := b.triage.AcknowledgeRetraction(ctx, request.ItemID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return b.triageDecisionResult(request.RequestID, "conflict", "")
	case err != nil:
		return b.triageDecisionResult(request.RequestID, "error", err.Error())
	case applied:
		return b.triageDecisionResult(request.RequestID, "applied", "")
	default:
		return b.triageDecisionResult(request.RequestID, "already_applied", "")
	}
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

// deliveryReconcile executes one of Decision 4's two document_delivery
// reconciliation mutations (confirm_request_exists/confirm_request_absent)
// against a job's open document_delivery human action. It is a new message
// rather than a widened human_action_resolve — see
// protocol.DeliveryReconcilePayload's doc comment — and mirrors
// internal/api/delivery.go's deliveryAction so the two surfaces (CLI/MCP and
// browser) leave a job in the exact same state for the same operator
// decision. Every failure encodes into the result frame's outcome/detail
// (never a raw Go error), matching every other bridge handler.
func (b *Bridge) deliveryReconcile(ctx context.Context, request *protocol.DeliveryReconcilePayload) ([]json.RawMessage, error) {
	if b.jobs == nil || b.svc == nil || b.svc.Delivery == nil {
		return b.deliveryReconcileResult(request.RequestID, "error", "document delivery is not configured")
	}
	action, err := b.openDocumentDeliveryAction(ctx, request.JobID)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if action == nil {
		return b.deliveryReconcileResult(request.RequestID, "already_applied", "no open document_delivery action for this job")
	}
	row, err := b.svc.Delivery.GetByJobID(ctx, request.JobID)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if row == nil {
		return b.deliveryReconcileResult(request.RequestID, "error", "no delivery request for this job")
	}
	switch request.Operation {
	case "confirm_request_exists":
		return b.deliveryConfirmRequestExists(ctx, request, action, row)
	case "confirm_request_absent":
		return b.deliveryConfirmRequestAbsent(ctx, request, action, row)
	default:
		return b.deliveryReconcileResult(request.RequestID, "error", "unknown operation "+request.Operation)
	}
}

// openDocumentDeliveryAction finds the one open document_delivery human
// action Decision 4 says a job in this reconciliation state must have.
// (nil, nil) means none is open — a routine race with a concurrent
// resolution, not an error.
func (b *Bridge) openDocumentDeliveryAction(ctx context.Context, jobID string) (*job.HumanAction, error) {
	actions, err := b.jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	for _, a := range actions {
		if a.Action.Kind == job.ActionKindDocumentDelivery && a.Action.Status == "open" {
			action := a.Action
			return &action, nil
		}
	}
	return nil, nil
}

// deliveryConfirmRequestExists mirrors internal/api's deliveryConfirmRequestExists:
// the row moves to pending with the human-supplied provider reference, the
// document_delivery action closes, and the job resumes as an ordinary
// pending delivery poll (StateRetryWait, RetryReasonDocumentDeliveryPending)
// — never retry_submission.
func (b *Bridge) deliveryConfirmRequestExists(ctx context.Context, request *protocol.DeliveryReconcilePayload, action *job.HumanAction, row *delivery.Request) ([]json.RawMessage, error) {
	if err := b.jobs.RepairAwaitingHuman(ctx, request.JobID, []int64{action.ID},
		map[string]any{"reason": "document_delivery_confirmed_exists"}); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	profile, err := b.svc.Delivery.ResolveGateProfileFor(ctx, row.InstitutionProfile)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.svc.Delivery.UpdateState(ctx, row.ID, delivery.StatePending); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	next := delivery.NextCheck(b.now(), 0, profile.StatusPollMinutes)
	if err := b.svc.Delivery.RecordPoll(ctx, row.ID, request.ProviderReference, next); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.jobs.Transition(ctx, request.JobID, job.StateResolving, job.StateRetryWait,
		map[string]any{"reason": job.RetryReasonDocumentDeliveryPending, "provider_reference": request.ProviderReference},
		job.WithRetryAt(next)); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	return b.deliveryReconcileResult(request.RequestID, "applied", "")
}

// deliveryConfirmRequestAbsent mirrors internal/api's
// deliveryConfirmRequestAbsent: the stale row is cancelled, the
// document_delivery action closes, and the job re-enters the shared
// Branch/gate seam (app.Service.SubmitDelivery) — never a duplicated policy
// implementation, and never a second live request for this idempotency key.
func (b *Bridge) deliveryConfirmRequestAbsent(ctx context.Context, request *protocol.DeliveryReconcilePayload, action *job.HumanAction, row *delivery.Request) ([]json.RawMessage, error) {
	if err := b.svc.Delivery.UpdateState(ctx, row.ID, delivery.StateCancelled); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.jobs.RepairAwaitingHuman(ctx, request.JobID, []int64{action.ID},
		map[string]any{"reason": "document_delivery_confirmed_absent"}); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if _, err := b.svc.SubmitDelivery(ctx, request.JobID); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	return b.deliveryReconcileResult(request.RequestID, "applied", "")
}

func (b *Bridge) deliveryReconcileResult(requestID, outcome, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgDeliveryReconcileResult, "", protocol.DeliveryReconcileResultPayload{
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
	row, err := b.jobs.Get(ctx, action.JobID)
	if err != nil {
		return b.reviewPreviewError(request.RequestID, fmt.Sprintf("review action %d preview is unavailable", request.ActionID))
	}
	url, err := b.preview.Issue(preview.IssueInput{
		ActionID: action.ID, Path: action.QuarantinePath, SHA256: action.QuarantineSHA256, Size: info.Size(),
		ExpectedRevision: action.Revision,
		Citation:         preview.Citation{Title: row.Work.Title, Authors: row.Work.Authors, Year: row.Work.Year},
		TTL:              previewCapabilityTTL,
	})
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
	// The browser session ID is in-memory transport state, not durable user
	// identity. Persist the explicit unknown principal until a durable,
	// non-secret multi-user identity is available at this boundary.
	jobID, err := b.svc.SubmitAs(ctx, job.PrincipalUnknown, request)
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

// pageBulkStatus resolves the ownership/job status of up to 200 page-detected
// identifiers (ADR-0019 Decision 5). It keeps no daemon-side scan-state: scan_id
// is opaque correlation supplied by the caller and is only ever echoed back
// (Decision 4 — the selection sheet lives in the extension, never the daemon).
// A positive zotio claim (see pageBulkStatusItem) is consulted before a live
// or previously-unavailable job verdict; every other case follows the prior
// job-history-first order. A nil provider, partial lookup, or failed lookup
// is unknown rather than not-owned (ADR-0008 invariant 2).
func (b *Bridge) pageBulkStatus(ctx context.Context, request *protocol.PageBulkStatusRequestPayload) ([]json.RawMessage, error) {
	zotioLookup := b.pageBulkZotioLookup(ctx, request.Identifiers)
	items := make([]protocol.PageBulkStatusItem, 0, len(request.Identifiers))
	for _, id := range request.Identifiers {
		items = append(items, b.pageBulkStatusItem(ctx, id, zotioLookup))
	}
	if err := b.recordPageBulkScan(ctx, request, items, b.now().UTC()); err != nil {
		// The measurement row is local-only funnel telemetry (ADR-0019
		// Decision 10), not part of the status contract: losing it must
		// never turn an otherwise-successful lookup into a bridge error.
		log.Printf("papio: recording page-bulk scan: %v", err)
	}
	truncated := false
	for {
		payload := protocol.PageBulkStatusResultPayload{
			RequestID: request.RequestID, ScanID: request.ScanID, Items: items, Truncated: truncated,
		}
		if b.frameFits(protocol.MsgPageBulkStatusResult, payload) {
			frame, err := b.frame(protocol.MsgPageBulkStatusResult, "", payload)
			if err != nil {
				return nil, err
			}
			return []json.RawMessage{frame}, nil
		}
		if len(items) <= 1 {
			return nil, fmt.Errorf("page bulk status item exceeds browser frame cap %d", protocol.MaxBrowserMessageBytes)
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

// recordPageBulkScan writes the local-only measurement row for one
// page_bulk_status_request (ADR-0019 Decision 10; dev/post-build-followups.md
// item 3). Unlike recordPageBulkRun (one row per submit attempt, keyed by a
// real batch_id), this row's batch_id stays "": pageBulkStatus and
// pageBulkSubmit still keep no daemon-side scan-state to correlate a status
// call with a later submit call (their own doc comments), so this is a
// genuinely separate row, not a two-phase update of one. That lets it
// honestly report the funnel counts a status call actually computed —
// detected_raw, canonical_unique, and the per-status breakdown — instead of
// leaving them at the schema's zero default, which recordPageBulkRun's own
// doc comment already accepts for submit-only rows. detector_id and
// source_origin stay "": page_bulk_status_request carries neither (ADR-0019
// keeps origin extension-side until submit), so this is an honest "unknown",
// never fabricated. RenderedRecordCountHint (nil when the page's structure
// was not recognized) is stored as-is — a null denominator, never a guess.
func (b *Bridge) recordPageBulkScan(ctx context.Context, request *protocol.PageBulkStatusRequestPayload, items []protocol.PageBulkStatusItem, at time.Time) error {
	canonicalKeys := make(map[string]bool, len(items))
	var eligible, ownedWithPDF, ownedMissingPDF, queued, ownershipIncomplete, invalid int
	for _, item := range items {
		if item.CanonicalKey != "" {
			canonicalKeys[item.CanonicalKey] = true
		}
		switch item.Status {
		case "eligible", "previously_unavailable":
			// previously_unavailable is historical, not a durable exclusion
			// (ADR-0019 Decision 5) and is not one of the funnel's fixed
			// columns; it folds into eligible, the bucket that already
			// means "still selectable".
			eligible++
		case "owned_with_pdf":
			ownedWithPDF++
		case "owned_missing_pdf":
			ownedMissingPDF++
		case "queued":
			queued++
		case "invalid":
			invalid++
		default:
			// ownership_incomplete and ownership_unknown (ADR-0008: an
			// unavailable/stale lookup is never a plain negative fact)
			// share one bucket here: neither is a confident classification,
			// and folding any future status into it fails closed rather
			// than silently dropping counts.
			ownershipIncomplete++
		}
	}
	ts := at.Format(time.RFC3339Nano)
	_, err := b.jobs.S.DB().ExecContext(ctx, `
		INSERT INTO page_bulk_runs
			(detector_id, source_origin, detected_raw, canonical_unique, eligible, owned_with_pdf, owned_missing_pdf, queued, ownership_incomplete, invalid, batch_id, opened_at, rendered_record_count_hint)
		VALUES ('', '', ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		len(request.Identifiers), len(canonicalKeys), eligible, ownedWithPDF, ownedMissingPDF, queued, ownershipIncomplete, invalid, ts,
		request.RenderedRecordCountHint)
	return err
}

// zotioLookupChunkSize is zotio.Service.LookupWorks' batch bound (1-50). A
// page can report up to 200 identifiers, so a full page chunks into up to 4
// zotio calls.
const zotioLookupChunkSize = 50

// zotioOwnership is one identifier's zotio classification, keyed by
// canonical key ("<kind>:<value>", matching PageBulkStatusItem.CanonicalKey).
// stale marks a classification that must never be painted as a plain
// negative: either its whole batch call failed, or zotio's mirror sync
// could not complete this round (status is then a best-effort, possibly
// zero, value).
type zotioOwnership struct {
	status  string
	itemKey string
	stale   bool
}

// pageBulkZotioLookup classifies every DOI/arXiv/PMID identifier from one
// status request against the user's Zotero library via zotio.Service,
// chunked to LookupWorks' bound, and logs the round's latency once. A nil
// service is the supported "not configured" mode (ADR-0008: zotio and the
// generic holdings registry never mix) and short-circuits without ever
// calling zotio, so page_bulk_status behaves exactly as it did before zotio
// ownership was wired in.
func (b *Bridge) pageBulkZotioLookup(ctx context.Context, ids []protocol.PageBulkIdentifier) map[string]zotioOwnership {
	if b.zotio == nil {
		return nil
	}
	type entry struct {
		key  string
		work zotio.LookupWork
	}
	entries := make([]entry, 0, len(ids))
	for _, id := range ids {
		kind, value, err := normalizePageBulkIdentifier(id.Kind, id.Value)
		if err != nil {
			continue
		}
		var work zotio.LookupWork
		switch kind {
		case ownership.KindDOI:
			work.DOI = value
		case ownership.KindArXiv:
			work.ArXiv = value
		case ownership.KindPMID:
			work.PMID = value
		default:
			continue
		}
		entries = append(entries, entry{key: kind + ":" + value, work: work})
	}
	if len(entries) == 0 {
		return nil
	}
	start := b.now()
	result := make(map[string]zotioOwnership, len(entries))
	for offset := 0; offset < len(entries); offset += zotioLookupChunkSize {
		end := offset + zotioLookupChunkSize
		if end > len(entries) {
			end = len(entries)
		}
		chunk := entries[offset:end]
		request := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(chunk))}
		for i, e := range chunk {
			request.Works[i] = e.work
		}
		lookup, err := b.zotio.LookupWorks(ctx, request)
		stale := err != nil || (lookup != nil && strings.TrimSpace(lookup.StalenessWarning) != "")
		for i, e := range chunk {
			if err != nil || lookup == nil || i >= len(lookup.Works) {
				result[e.key] = zotioOwnership{stale: true}
				continue
			}
			work := lookup.Works[i]
			result[e.key] = zotioOwnership{status: work.Status, itemKey: work.ItemKey, stale: stale}
		}
	}
	log.Printf("papio: page-bulk zotio lookup: %d works in %s", len(entries), b.now().Sub(start).Round(time.Millisecond))
	return result
}

// pageBulkStatusItem resolves one identifier. Job-store or holdings failures
// are reported as ownership_incomplete rather than propagated as Go errors:
// routine read-model failures must not tear down the native-messaging
// session. zotioLookup (nil when zotio is unconfigured) is merged with
// papio's own job/holdings state under a fixed precedence: papio ready
// bundle, then zotio owned_with_pdf, then zotio owned_missing_pdf (carrying
// the Zotero item key), then a live canonical job (queued), then a past
// terminal-unavailable verdict, then a complete negative lookup (eligible);
// an unavailable/stale zotio round is reported as ownership_unknown, never
// collapsed into a plain not-owned/"eligible" claim (ADR-0008 invariant 2).
func (b *Bridge) pageBulkStatusItem(ctx context.Context, id protocol.PageBulkIdentifier, zotioLookup map[string]zotioOwnership) protocol.PageBulkStatusItem {
	kind, value, err := normalizePageBulkIdentifier(id.Kind, id.Value)
	if err != nil {
		return protocol.PageBulkStatusItem{LocalID: id.LocalID, Status: "invalid"}
	}
	item := protocol.PageBulkStatusItem{LocalID: id.LocalID, CanonicalKey: kind + ":" + value}
	liveJobID, readyJobID, previouslyUnavailable, lookupErr := b.canonicalJobStatus(ctx, kind, value)
	zotioResult, hasZotio := zotioLookup[item.CanonicalKey]
	switch {
	case lookupErr != nil:
		item.Status = "ownership_incomplete"
	case readyJobID != "":
		// papio already holds a validated bundle for this work: the freshest
		// possible PDF-present claim, complete by construction. The wire
		// contract reserves job_id for queued rows (the one a user can
		// watch), so the ready job's id stays daemon-side.
		item.Status = "owned_with_pdf"
		item.OwnershipComplete = true
	case hasZotio && zotioResult.status == zotio.OwnershipOwnedWithPDF:
		item.Status = "owned_with_pdf"
		item.OwnershipComplete = true
	case hasZotio && zotioResult.status == zotio.OwnershipOwnedMissingPDF:
		item.Status = "owned_missing_pdf"
		item.OwnershipComplete = true
		item.ZotioItemKey = zotioResult.itemKey
	case liveJobID != "":
		item.Status, item.JobID = "queued", liveJobID
	case previouslyUnavailable:
		item.Status = "previously_unavailable"
	default:
		decision, complete := b.pageBulkOwnership(ctx, pageBulkOwnershipQuery(kind, value))
		// zotioConfident is true only when this round actually reached zotio
		// and got back a trustworthy (non-stale) answer, which — having
		// fallen through the owned_with_pdf/owned_missing_pdf cases above —
		// can only be a confident not-owned verdict here.
		zotioConfident := hasZotio && !zotioResult.stale
		item.OwnershipComplete = complete || zotioConfident
		switch {
		case decision.Suppress:
			item.Status = "owned_with_pdf"
		case decision.RecordPresent:
			item.Status = "owned_missing_pdf"
		case hasZotio && zotioResult.stale:
			item.Status = "ownership_unknown"
		case complete || zotioConfident:
			item.Status = "eligible"
		default:
			item.Status = "ownership_incomplete"
		}
	}
	return item
}

// pageBulkOwnershipQuery preserves the ownership package's deliberately
// different identity normalization while asking about the canonical
// acquisition identifier.
func pageBulkOwnershipQuery(kind, value string) ownership.Query {
	switch kind {
	case ownership.KindDOI:
		return ownership.QueryFor(value, "", "", ownership.VersionAny, ownership.EntityUnknown)
	case ownership.KindArXiv:
		return ownership.QueryFor("", value, "", ownership.VersionAny, ownership.EntityUnknown)
	case ownership.KindPMID:
		return ownership.QueryFor("", "", value, ownership.VersionAny, ownership.EntityUnknown)
	default:
		return ownership.Query{}
	}
}

// pageBulkOwnership applies the shared ADR-0008 suppression rules to one
// lookup. complete is false for nil/disabled providers, partial results, and
// malformed implementations. Positive claims remain usable even when another
// source failed, but absence licenses "eligible" only when complete is true.
func (b *Bridge) pageBulkOwnership(ctx context.Context, query ownership.Query) (ownership.Decision, bool) {
	if b.holdings == nil || !b.holdings.Enabled() {
		return ownership.Decision{}, false
	}
	result := b.holdings.Lookup(ctx, []ownership.Query{query})
	if len(result.Works) != 1 {
		return ownership.Decision{}, false
	}
	return ownership.Decide(query, result.Works[0]), result.Complete()
}

// normalizePageBulkIdentifier canonicalizes one browser-reported identifier
// with the daemon's existing normalizers — the only canonical validators
// (ADR-0019 Decision 3) — and reports the identifiers-table kind alongside it.
func normalizePageBulkIdentifier(kind, value string) (string, string, error) {
	switch kind {
	case "doi":
		normalized, err := work.NormalizeDOI(value)
		return kind, normalized, err
	case "pmid":
		normalized, err := work.NormalizePMID(value)
		return kind, normalized, err
	case "arxiv":
		normalized, err := work.NormalizeArXiv(value)
		return kind, normalized, err
	default:
		return "", "", fmt.Errorf("unsupported identifier kind %q", kind)
	}
}

// canonicalJobStatus looks up the given canonical identity directly against
// the jobs/identifiers tables the bridge already reaches (the same join
// liveJobForCanonicalWork uses internally in internal/job, unexported there).
// The most recent live job wins; failing that, the most recent READY job —
// papio's own validated bundle is the strongest artifact-present claim there
// is, and the only one that exists under a zotio configuration, where the
// generic holdings registry is deliberately empty (ADR-0008: zotio and
// generic sources never mix) and every external lookup honestly reports
// incomplete; failing that, any past terminal "unavailable" verdict
// (ADR-0019 Decision 5).
func (b *Bridge) canonicalJobStatus(ctx context.Context, kind, value string) (liveJobID, readyJobID string, previouslyUnavailable bool, err error) {
	rows, err := b.jobs.S.DB().QueryContext(ctx, `
		SELECT j.id, j.state FROM jobs j
		JOIN identifiers i ON i.work_request_id = j.work_request_id
		WHERE i.kind = ? AND i.value = ?
		ORDER BY j.created_at DESC`, kind, value)
	if err != nil {
		return "", "", false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return "", "", false, err
		}
		if !job.Terminal(state) {
			return id, "", false, nil
		}
		if state == job.StateReady && readyJobID == "" {
			readyJobID = id
		}
		if state == job.StateUnavailable {
			previouslyUnavailable = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", false, err
	}
	return "", readyJobID, previouslyUnavailable, nil
}

// pageBulkSubmit creates one ordinary batch of jobs from up to 50 canonical
// keys a prior page_bulk_status_request already resolved. Like pageBulkStatus
// it keeps no daemon-side scan-state: a stale or unknown scan_id is accepted
// and simply echoed back, and a canonical_key from any source — even one this
// daemon run never issued — is honoured as long as it still decodes to a valid
// identifier (ADR-0019 Decision 7).
//
// Each key becomes one ordinary SubmitWithOptionsAs call — the same
// application-service entry point acquire.submit_v3 uses (internal/api) —
// carrying the daemon-assigned consumer. This is not a second acquisition
// policy surface: a job created here enters the same waterfall as any other
// submission, including LibKey-routed institutional handoff where configured
// (ADR-0016 Decision 1).
//
// Every valid key rechecks holdings immediately before submission. Only a
// fresh artifact-present claim suppresses the ordinary app submission; the
// result is counted as already_owned (ADR-0008 invariant 2).
func (b *Bridge) pageBulkSubmit(ctx context.Context, request *protocol.PageBulkSubmitRequestPayload) ([]json.RawMessage, error) {
	var submitted, joined, alreadyOwned, invalid int64
	for _, key := range request.CanonicalKeys {
		wr, ok := pageBulkWorkRequest(key)
		if !ok {
			invalid++
			continue
		}
		// papio's own ready bundle suppresses first: it is the freshest
		// artifact-present claim and exists under every configuration,
		// including zotio setups where the holdings registry is empty.
		if kind, value, ok := pageBulkIdentifierOf(wr); ok {
			if _, readyJobID, _, statusErr := b.canonicalJobStatus(ctx, kind, value); statusErr == nil && readyJobID != "" {
				alreadyOwned++
				continue
			}
		}
		query := ownership.QueryFor(wr.Identifiers.DOI, wr.Identifiers.ArXiv, wr.Identifiers.PMID, wr.DesiredVersion, ownership.EntityUnknown)
		decision, _ := b.pageBulkOwnership(ctx, query)
		if decision.Suppress {
			alreadyOwned++
			continue
		}
		result, err := b.svc.SubmitWithOptionsAs(ctx, job.PrincipalUnknown, wr, app.SubmitOptions{Consumer: pageBulkConsumer})
		if err != nil {
			// A routine per-key submission failure (a resolver override
			// naming an unconfigured profile, a since-broken invariant on
			// the request) never fails the whole batch: it counts as one
			// invalid key among however many others succeeded.
			invalid++
			continue
		}
		if result.Existing {
			joined++
		} else {
			submitted++
		}
	}
	batchID := newMsgID()
	at := b.now().UTC()
	if err := b.recordPageBulkRun(ctx, request.Source, len(request.CanonicalKeys), submitted, invalid, batchID, at); err != nil {
		// The measurement row is local-only funnel telemetry (ADR-0019
		// Decision 10), not part of the acquisition contract: losing it must
		// never turn an otherwise-successful submission into a bridge error.
		log.Printf("papio: recording page-bulk run: %v", err)
	}
	frame, err := b.frame(protocol.MsgPageBulkSubmitResult, "", protocol.PageBulkSubmitResultPayload{
		RequestID: request.RequestID, ScanID: request.ScanID,
		Submitted: submitted, Joined: joined, AlreadyOwned: alreadyOwned, Invalid: invalid,
		BatchID: batchID,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// pageBulkIdentifierOf extracts the identifiers-table (kind, value) pair a
// canonical-key-derived work request names, mirroring pageBulkWorkRequest's
// own key vocabulary.
func pageBulkIdentifierOf(wr protocol.WorkRequest) (string, string, bool) {
	ids := wr.Identifiers
	if ids == nil {
		return "", "", false
	}
	switch {
	case ids.DOI != "":
		return "doi", ids.DOI, true
	case ids.ArXiv != "":
		return "arxiv", ids.ArXiv, true
	case ids.PMID != "":
		return "pmid", ids.PMID, true
	}
	return "", "", false
}

// pageBulkWorkRequest maps one status-issued canonical key back to a work
// request. false means the key no longer resolves — either it is stale or it
// never came from this daemon — which the caller counts as invalid rather than
// treating as a Go error (ADR-0019 Decision 7: page-bulk submit is a thin
// transport adapter, not a second validation surface).
func pageBulkWorkRequest(canonicalKey string) (protocol.WorkRequest, bool) {
	kind, value, ok := strings.Cut(canonicalKey, ":")
	if !ok || value == "" {
		return protocol.WorkRequest{}, false
	}
	normKind, normValue, err := normalizePageBulkIdentifier(kind, value)
	if err != nil {
		return protocol.WorkRequest{}, false
	}
	ids := &protocol.Identifiers{}
	switch normKind {
	case "doi":
		ids.DOI = normValue
	case "pmid":
		ids.PMID = normValue
	case "arxiv":
		ids.ArXiv = normValue
	}
	sum := sha256.Sum256([]byte(canonicalKey))
	return protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      "page_bulk_" + hex.EncodeToString(sum[:]),
		DesiredVersion: "any",
		Identifiers:    ids,
	}, true
}

// recordPageBulkRun writes the local-only measurement row for one submitted
// batch (ADR-0019 Decision 10). Funnel counts upstream of submit — detected,
// canonical_unique, eligible, owned_with_pdf, owned_missing_pdf, queued,
// ownership_incomplete — are left at their schema default of 0: the daemon
// keeps no scan-side state to pair them with (see pageBulkStatus and
// pageBulkSubmit's doc comments), so this row honestly reports only what the
// submit call itself observed.
func (b *Bridge) recordPageBulkRun(ctx context.Context, source protocol.PageBulkSubmitSource, selected int, submitted, invalid int64, batchID string, at time.Time) error {
	ts := at.Format(time.RFC3339Nano)
	_, err := b.jobs.S.DB().ExecContext(ctx, `
		INSERT INTO page_bulk_runs (detector_id, source_origin, selected, submitted, invalid, batch_id, opened_at, submitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		source.Detector, source.Origin, selected, submitted, invalid, batchID, ts, ts)
	return err
}

// pruneDeliveryMetadata drops unpaired frames after the short handoff window.
// The maps carry only job/download correlation and bounded route evidence.
func (b *Bridge) pruneDeliveryMetadata(now time.Time) {
	for key, pending := range b.pendingDownloads {
		if now.Sub(pending.ReceivedAt) > deliveryContextTTL {
			delete(b.pendingDownloads, key)
		}
	}
	for key, pending := range b.deliveryContexts {
		if now.Sub(pending.ReceivedAt) > deliveryContextTTL {
			delete(b.deliveryContexts, key)
		}
	}
}

func browserDeliveryContext(payload *protocol.DeliveryContextPayload) *app.BrowserDeliveryContext {
	if payload == nil {
		return nil
	}
	return &app.BrowserDeliveryContext{
		Route:           payload.Route,
		PageHost:        payload.PageHost,
		SessionEvidence: payload.SessionEvidence,
	}
}

// deliveryContext records the job/download-scoped route and, when the
// completion frame has already arrived, applies it to the browser candidate.
func (b *Bridge) deliveryContext(ctx context.Context, jobID string, payload *protocol.DeliveryContextPayload) error {
	b.pruneDeliveryMetadata(b.now())
	key := browserDownloadKey{JobID: jobID, DownloadID: payload.DownloadID}
	detail := map[string]any{
		"download_id":      payload.DownloadID,
		"route":            payload.Route,
		"session_evidence": payload.SessionEvidence,
	}
	if payload.PageHost != "" {
		detail["page_host"] = payload.PageHost
	}
	if err := b.jobs.S.AppendEvent(ctx, jobID, "browser.delivery_context", detail); err != nil {
		return err
	}
	b.deliveryContexts[key] = pendingDeliveryContext{Payload: *payload, ReceivedAt: b.now()}
	pending, ok := b.pendingDownloads[key]
	if !ok {
		return nil
	}
	provenance := browserDeliveryContext(payload)
	if pending.CandidateID != 0 {
		applied, err := b.jobs.ApplyBrowserDeliveryContextToCandidate(ctx, jobID, pending.CandidateID, payload.Route, payload.SessionEvidence, pageHostURL(payload.PageHost))
		if err != nil {
			return err
		}
		if !applied {
			// The candidate binding disappeared; keep both frames briefly so a
			// directory sweep can retry adoption instead of annotating a
			// different browser candidate.
			return nil
		}
		delete(b.deliveryContexts, key)
		delete(b.pendingDownloads, key)
		return nil
	}
	if _, err := b.adoptOutsideSessionLock(ctx, jobID, pending.Filename, provenance); err != nil {
		_ = b.jobs.S.AppendEvent(ctx, jobID, "browser.adoption_deferred",
			map[string]any{"filename": pending.Filename, "reason": truncate(err.Error(), 200)})
		return nil
	}
	delete(b.deliveryContexts, key)
	delete(b.pendingDownloads, key)
	return nil
}

func pageHostURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

// adoptOutsideSessionLock runs validation without blocking unrelated browser
// syncs. The adoption service leases the durable job state before validation,
// so releasing the in-memory session lock cannot admit a competing adoption.
// The caller must hold b.mu; it is held again before this method returns.
func (b *Bridge) adoptOutsideSessionLock(ctx context.Context, jobID, filename string, provenance ...*app.BrowserDeliveryContext) (int64, error) {
	b.mu.Unlock()
	defer b.mu.Lock()
	if len(provenance) > 0 && provenance[0] != nil {
		return b.adoptWithContext(ctx, jobID, filename, provenance[0])
	}
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

// extensionVersionNewer rejects malformed versions before using the legacy
// comparison helper: an unparseable hello must never be treated as evidence
// that a previously parked job's provider failure has been fixed.
func extensionVersionNewer(previous, current string) bool {
	return validExtensionVersion(previous) &&
		validExtensionVersion(current) &&
		compareVersion(previous, current) < 0
}

func validExtensionVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// recordAuth appends a timing-only auth event. The AuthPayload structurally
// cannot carry a URL, host, title, query, or fragment, so an identity-provider
// address cannot enter the event stream through this path.
// sessionEvidence records one timing-only institutional-session signal and
// reuses the auth-return sibling reoffer path. Replays within the throttle
// window are ignored before touching durable activity or job state.
func (b *Bridge) sessionEvidence(ctx context.Context, p *protocol.SessionEvidencePayload) error {
	now := b.now()
	if !b.lastSessionEvidenceAt.IsZero() {
		age := now.Sub(b.lastSessionEvidenceAt)
		if age >= 0 && age < sessionEvidenceThrottle {
			return nil
		}
	}
	b.lastSessionEvidenceAt = now
	if err := b.jobs.S.AppendEvent(ctx, "", "browser.session_evidence", nil); err != nil {
		return err
	}
	if b.reofferRanThisSync {
		return nil
	}
	err := b.reofferInstitutionalSiblingsForEvidence(ctx, p.OriginHint)
	b.reofferRanThisSync = true
	return err
}

// reofferInstitutionalSiblingsForEvidence chooses an open institutional
// handoff as the source for the existing sibling reoffer routine. When the
// extension supplies an origin hint, both the source and every candidate must
// belong to the matching configured resolver profile; an unknown hint fails
// closed.
//
// An absent hint is NOT a wildcard. Evidence that cannot be attributed to an
// institution may only release the default profile's queue: matching any
// profile would let a second institution's sign-in reopen the first
// institution's parked tabs, which is precisely the isolation this scoping
// exists to guarantee. Single-institution setups leave Policy.Resolver empty,
// which is the default profile, so their historical behavior is unchanged.
//
// A live pin may be retired only by attributable evidence for another
// profile. An unhinted frame must never clear or repoint it: an older extension
// or origin-less keepalive would otherwise replace a named-profile pin with a
// default-profile handoff, and the next genuine auth_returned for the named
// profile would hit the mismatched-pin branch and release nothing. That silently
// starves the institution's queue for at least the 60-second evidence-throttle
// window, or for the whole session if the extension never sends a hint.
func (b *Bridge) reofferInstitutionalSiblingsForEvidence(ctx context.Context, originHint string) error {
	hinted := strings.TrimSpace(originHint) != ""
	wantedProfile := resolverProfileKey("")
	if hinted {
		profile, ok := b.cfg.ResolverProfileForOrigin(originHint)
		if !ok {
			return nil
		}
		wantedProfile = resolverProfileKey(profile)
	}
	if b.reofferSourceJobID != "" && resolverProfileKey(b.reofferProfile) != wantedProfile {
		if !hinted {
			// An unattributable frame proves neither that the named pin is
			// stale nor that the default queue is authenticated. Falling
			// through would release the default queue while leaving the
			// named institution unable to use its genuine auth return.
			return nil
		}
		b.reofferSourceJobID = ""
		b.reofferProfile = ""
	}
	if b.reofferSourceJobID != "" {
		return b.reofferInstitutionalSiblings(ctx, b.reofferSourceJobID)
	}
	handoffs, _, err := b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	if err != nil {
		return err
	}
	var fallback string
	for _, item := range handoffs {
		// A quiesced handoff must not seed a re-offer sweep: riding someone
		// else's fresh login is exactly how a week-old dead action keeps
		// producing a tab per session. `papio actions open` still drives it.
		if !item.Action.RequiresAuth ||
			item.Action.Quiesced(b.now()) ||
			resolverProfileKey(item.Row.Policy.Resolver) != wantedProfile {
			continue
		}
		if fallback == "" {
			fallback = item.Row.ID
		}
		if b.offered[item.Row.ID] {
			return b.reofferInstitutionalSiblings(ctx, item.Row.ID)
		}
	}
	if fallback == "" {
		return nil
	}
	return b.reofferInstitutionalSiblings(ctx, fallback)
}

func (b *Bridge) recordAuth(ctx context.Context, msg *protocol.BrowserMessage) error {
	kind := "browser.auth_pending"
	if msg.Type == protocol.MsgAuthReturned {
		kind = "browser.auth_returned"
	}

	detail := map[string]any{}
	if p := msg.Payload.(*protocol.AuthPayload); p.ElapsedMS != nil {
		detail["elapsed_ms"] = *p.ElapsedMS
	}
	if err := b.jobs.S.AppendEvent(ctx, msg.JobID, kind, detail); err != nil {
		return err
	}
	if msg.Type != protocol.MsgAuthReturned {
		return nil
	}
	if b.reofferRanThisSync {
		return nil
	}
	err := b.reofferInstitutionalSiblings(ctx, msg.JobID)
	b.reofferRanThisSync = true
	return err
}

// reofferInstitutionalSiblings lets poll reopen only the handoffs that a
// returned institutional session can actually unlock. The caller holds b.mu.
func (b *Bridge) reofferInstitutionalSiblings(ctx context.Context, sourceJobID string) error {
	if b.holder == nil || b.now().Sub(b.holder.LastSyncAt) > sessionStaleAfter {
		return nil
	}

	handoffJobs, _, err := b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	if err != nil {
		return err
	}
	var source *job.Row
	var sourceActionID int64
	for i := range handoffJobs {
		item := &handoffJobs[i]
		if item.Row.ID == sourceJobID {
			row := item.Row
			source = &row
			if item.Action.RequiresAuth {
				sourceActionID = item.Action.ID
			}
			break
		}
	}
	if source == nil {
		if b.reofferSourceJobID != sourceJobID {
			return nil
		}
	} else {
		if b.reofferSourceJobID != "" && b.reofferSourceJobID != sourceJobID {
			return nil
		}
		if b.reofferSourceJobID == "" && sourceActionID != 0 {
			b.reofferSourceJobID = sourceJobID
			b.reofferProfile = resolverProfileKey(source.Policy.Resolver)
		}
		if sourceActionID != 0 {
			b.authReleased[sourceActionID] = true
		}
	}
	profile := b.reofferProfile
	if b.reofferSourceJobID != sourceJobID || profile == "" {
		return nil
	}

	handoff := make(map[string]job.HumanAction, len(handoffJobs))
	rows := make(map[string]job.Row, len(handoffJobs))
	for _, item := range handoffJobs {
		handoff[item.Row.ID] = item.Action
		rows[item.Row.ID] = item.Row
	}

	outstanding := outstandingOfferCount(b.offered, handoff, rows, b.pendingDownloads)
	available := maxOutstandingOffers - outstanding
	if available < 0 {
		available = 0
	}
	type candidate struct {
		action job.HumanAction
		row    job.Row
	}
	candidates := make([]candidate, 0, len(handoffJobs))
	for _, item := range handoffJobs {
		action := item.Action
		// Quiesced siblings are excluded for the same reason the seed is: a
		// fresh institutional login must not resurrect a week-old handoff
		// nobody has completed. An explicit `papio actions open` still does.
		if item.Row.ID == sourceJobID ||
			!action.RequiresAuth ||
			action.Quiesced(b.now()) ||
			b.authReleased[action.ID] {
			continue
		}
		row := item.Row
		if resolverProfileKey(row.Policy.Resolver) != profile ||
			row.LeaseActive(b.now()) ||
			hasSettledDownload(b.pendingDownloads, row.ID) {
			continue
		}
		if _, offerable := b.offerableAccessMode(row); !offerable {
			delete(b.reofferPending, row.ID)
			continue
		}
		requeued, err := b.institutionalRouteRequeued(ctx, row.ID)
		if err != nil {
			return err
		}
		if requeued {
			continue
		}
		candidates = append(candidates, candidate{action: action, row: row})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].row.CreatedAt == candidates[j].row.CreatedAt {
			return candidates[i].row.ID < candidates[j].row.ID
		}
		return candidates[i].row.CreatedAt < candidates[j].row.CreatedAt
	})
	released := 0
	for _, candidate := range candidates {
		if released >= maxInstitutionalReoffers ||
			b.holder == nil ||
			b.now().Sub(b.holder.LastSyncAt) > sessionStaleAfter {
			break
		}
		if !b.offered[candidate.row.ID] && available <= 0 {
			break
		}
		if b.offered[candidate.row.ID] {
			delete(b.offered, candidate.row.ID)
			delete(b.cancelSent, candidate.row.ID)
		} else {
			available--
		}
		b.reofferPending[candidate.row.ID] = true
		b.authReleased[candidate.action.ID] = true
		if err := b.jobs.RecordEvent(ctx, candidate.row.ID, "browser.handoff_reoffered",
			map[string]any{"reason": "institutional_session_live"}); err != nil {
			return err
		}
		released++
	}
	return nil
}

func outstandingOfferCount(
	offered map[string]bool,
	actions map[string]job.HumanAction,
	rows map[string]job.Row,
	settled map[browserDownloadKey]pendingBrowserDownload,
) int {
	count := 0
	for jobID := range offered {
		action, ok := actions[jobID]
		if !ok || action.Kind != handoffActionKind || hasSettledDownload(settled, jobID) {
			continue
		}
		row, ok := rows[jobID]
		if ok && row.State == job.StateAwaitingHuman {
			count++
		}
	}
	return count
}

func hasSettledDownload(settled map[browserDownloadKey]pendingBrowserDownload, jobID string) bool {
	for key := range settled {
		if key.JobID == jobID {
			return true
		}
	}
	return false
}

// resolverProfileKey keeps the two configured spellings of the default
// institution from being treated as separate authenticated sessions.
func resolverProfileKey(profile string) string {
	if profile == "" || profile == "default" {
		return "default"
	}
	return profile
}

// outcome maps a terminal provider observation onto a policy-legal transition.
func (b *Bridge) outcome(ctx context.Context, jobID string, p *protocol.ProviderOutcomePayload) error {
	sourceExtensionVersion := ""
	if b.holder != nil {
		sourceExtensionVersion = b.holder.ExtensionVersion
	}
	if err := b.jobs.RecordEvent(ctx, jobID, "browser.provider_outcome", map[string]any{
		"outcome":           p.Outcome,
		"adapter_version":   p.AdapterVersion,
		"detail":            p.Detail,
		"extension_version": sourceExtensionVersion,
	}); err != nil {
		return err
	}
	switch p.Outcome {
	case "cancelled":
		if err := b.resolveHandoff(ctx, jobID, "cancelled"); err != nil {
			return err
		}
		b.cancelSent[jobID] = true
		return b.jobs.Cancel(ctx, jobID, job.TerminalReasonBrowserCancelled)

	case "no_entitlement", "document_delivery_available":
		requeued, err := b.institutionalRouteRequeued(ctx, jobID)
		if err != nil {
			return err
		}
		if !requeued {
			// The institutional route has not been disproven yet: an OA
			// browser action still earns its one institutional fallback,
			// and an institutional action earns one rediscovery pass.
			if fellBack, err := b.fallbackOAHandoff(ctx, jobID, p.Outcome); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			} else if fellBack {
				return nil
			}
			if err := b.jobs.RecordEvent(ctx, jobID, "browser.no_entitlement_requeue", map[string]any{"outcome": p.Outcome}); err != nil {
				return err
			}
			if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
				return err
			}
			return b.leaveHandoff(ctx, jobID, job.StateResolving, "no_entitlement_rediscovery")
		}
		// The route already proved empty. Never convert a rediscovered OA
		// action back to that institutional route and never requeue again:
		// resolve whatever handoff reported this and park terminally.
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateUnavailable, p.Outcome)

	case "wrong_work", "ui_changed":
		actions, err := b.jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
		if err != nil {
			return err
		}
		// A missing handoff must not promise access: the user may still need to
		// sign in, and a false open-access claim sends them to a paywall.
		requiresAuth := true
		for _, action := range actions {
			if action.Kind == handoffActionKind {
				requiresAuth = action.RequiresAuth
				break
			}
		}
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		detail := "papio reached a different work; find and download the requested PDF yourself"
		if p.Outcome == "ui_changed" {
			detail = "papio could not drive the provider page; download the PDF yourself and papio will adopt it"
			if strings.HasPrefix(p.Detail, "No source-controlled adapter matched this provider page.") {
				detail = "papio has no adapter for this provider yet; download the PDF yourself for now"
				if strings.Contains(p.Detail, "A sanitized diagnostic was saved locally") {
					detail += "; a sanitized page diagnostic is saved locally; run 'papio adapter captures' to inspect it"
				}
			}
		}
		// The page, rather than the original paywall, now blocks papio; whether
		// that page needs a sign-in remains the resolved handoff's classification.
		_, err = b.jobs.OpenHumanAction(ctx, jobID, "manual_download", detail,
			job.Access(requiresAuth, "landing_page"))
		return err

	case "rate_limited":
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateRetryWait, p.Outcome)

	case "human_auth_required", "terms_acceptance_required":
		row, err := b.jobs.Get(ctx, jobID)
		if err != nil {
			return err
		}
		if !row.Work.HasFetchableIdentifier() {
			// An OA anti-bot offer can still download a title-matched work, but
			// an auth wall cannot turn a missing identifier into fetchable work.
			if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
				return err
			}
			return b.leaveHandoff(ctx, jobID, job.StateUnavailable, string(job.TerminalReasonNoIdentifier))
		}
		// Still legitimately in progress: keep the job parked and add the
		// specific human action the extension observed.
		//
		// Both outcomes classify as requiring authentication, but only one of
		// them is knowable. human_auth_required is explicit — the extension has
		// observed an auth wall — and recording it unclassified left the field
		// meaning "no sign-in needed" for the one action that most certainly
		// needs one. terms_acceptance_required is genuinely ambiguous: terms can
		// sit behind institutional auth, on a public page, or behind a free
		// account, and the payload carries nothing to tell them apart. It fails
		// closed rather than becoming a third stored value, because wrongly
		// asking a human to sign in costs a prompt while wrongly asserting none
		// is needed is what this field exists to prevent.
		_, err = b.jobs.OpenHumanAction(ctx, jobID, p.Outcome,
			"the provider requires a human step before the download can proceed",
			job.Access(true, "paywall"))
		return err

	default:
		return fmt.Errorf("unknown provider outcome %q", p.Outcome)
	}
}

// institutionalRouteRequeued reports whether this job already left an
// institutional handoff for an OA rediscovery pass. The event is the durable
// one-shot guard shared with app.exhaustedCandidates.
func (b *Bridge) institutionalRouteRequeued(ctx context.Context, jobID string) (bool, error) {
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind == "browser.no_entitlement_requeue" {
			return true, nil
		}
	}
	return false, nil
}

// leaveHandoff transitions a parked handoff job out of awaiting_human. It is
// idempotent: if the job already left awaiting_human, it does nothing.
func (b *Bridge) leaveHandoff(ctx context.Context, jobID, to, reason string) error {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if row.State != job.StateAwaitingHuman {
		return nil
	}
	detail := map[string]any{"reason": reason}
	var opts []job.TransitionOpt
	switch to {
	case job.StateUnavailable:
		// Provider outcomes are dynamic; classify at this boundary while the
		// original observation remains in the transition detail.
		opts = append(opts, job.WithTerminalReason(job.NormalizeTerminalReason(reason)))
	case job.StateRetryWait:
		opts = append(opts, job.WithRetryAt(b.now().Add(b.actionExpiry())))
	}
	if err := b.jobs.Transition(ctx, jobID, job.StateAwaitingHuman, to, detail, opts...); err != nil {
		if errors.Is(err, job.ErrConflict) {
			return nil
		}
		return err
	}
	return nil
}

// adopt resolves the reported download strictly under the job's adoption
// directory and hands it to the app for validation. The filename has already
// passed protocol validation (no path separators); this adds IsLocal and a
// symlink-resolved prefix guard before app-side confinement.
func (b *Bridge) adopt(ctx context.Context, jobID, filename string) (int64, error) {
	if !filepath.IsLocal(filename) {
		return 0, fmt.Errorf("adoption filename %q is not a local name", filename)
	}
	root := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0, fmt.Errorf("adoption root unavailable: %w", err)
	}
	full := filepath.Join(realRoot, filename)
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel != filename || strings.Contains(rel, "..") {
		return 0, fmt.Errorf("adoption path escapes %s", realRoot)
	}
	return b.svc.AdoptDownloadCandidate(ctx, jobID, full)
}

func (b *Bridge) adoptWithContext(ctx context.Context, jobID, filename string, provenance *app.BrowserDeliveryContext) (int64, error) {
	if !filepath.IsLocal(filename) {
		return 0, fmt.Errorf("adoption filename %q is not a local name", filename)
	}
	root := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0, fmt.Errorf("adoption root unavailable: %w", err)
	}
	full := filepath.Join(realRoot, filename)
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel != filename || strings.Contains(rel, "..") {
		return 0, fmt.Errorf("adoption path escapes %s", realRoot)
	}
	return b.svc.AdoptDownloadWithContextCandidate(ctx, jobID, full, provenance)
}

// adoptionScanDeadline bounds one adoption-directory ReadDir syscall. A
// TCC-protected root (for example a download_adoption_root under
// ~/Downloads on macOS) can make open(2) block in-kernel indefinitely: tccd
// is waiting on a consent decision only an interactive process can supply,
// and papio is a background daemon. 2s is far past any real filesystem
// latency but short enough that one hung scan costs at most one poll tick.
const adoptionScanDeadline = 2 * time.Second

// ErrAdoptionScanTimeout marks a ReadDir call that did not return within
// adoptionScanDeadline — the signature of the TCC consent wall described on
// scanAdoptionDir. Never wrapped, so callers compare it with errors.Is.
var ErrAdoptionScanTimeout = errors.New("adoption directory scan timed out")

// BoundedReadDir runs readDir(dir) — os.ReadDir when readDir is nil — on its
// own goroutine and returns ErrAdoptionScanTimeout if it has not completed
// within adoptionScanDeadline. Go cannot cancel a syscall already blocked
// in-kernel, so on timeout the goroutine is left running; when afterTimeout
// is non-nil, a second goroutine (which only waits on a channel, never on
// the syscall itself) reports its eventual real result to afterTimeout
// exactly once. This is the seam Bridge.scanAdoptionDir (which also latches
// further scans off while a call is outstanding — see adoptionScanSuspended)
// and doctor's adoption-root health check both build on, so a TCC-hung
// filesystem can never wedge the daemon or a one-shot doctor run.
func BoundedReadDir(dir string, readDir func(string) ([]os.DirEntry, error), afterTimeout func([]os.DirEntry, error)) ([]os.DirEntry, error) {
	if readDir == nil {
		readDir = os.ReadDir
	}
	type result struct {
		entries []os.DirEntry
		err     error
	}
	done := make(chan result, 1)
	go func() {
		entries, err := readDir(dir)
		done <- result{entries, err}
	}()
	select {
	case r := <-done:
		return r.entries, r.err
	case <-time.After(adoptionScanDeadline):
		if afterTimeout != nil {
			go func() {
				r := <-done
				afterTimeout(r.entries, r.err)
			}()
		}
		return nil, ErrAdoptionScanTimeout
	}
}

// readAdoptionDir bounds one adoption-directory listing against
// adoptionScanDeadline and, on a timeout, latches adoption scanning off for
// the whole bridge — not just this job — until the hung call eventually
// returns. A short-circuited or timed-out call reports the same shape of
// error a missing directory does, which every caller here already treats as
// "not adoptable": a scan papio could not complete must never be read as
// evidence a settled file is present (fail-closed adoption semantics). The
// two log lines fire exactly once per transition.
func (b *Bridge) readAdoptionDir(dir string) ([]os.DirEntry, error) {
	b.adoptionScanMu.Lock()
	suspended := b.adoptionScanSuspended
	b.adoptionScanMu.Unlock()
	if suspended {
		return nil, ErrAdoptionScanTimeout // a prior call is still hung; never stack another
	}

	entries, err := BoundedReadDir(dir, b.readDir, func([]os.DirEntry, error) {
		b.adoptionScanMu.Lock()
		b.adoptionScanSuspended = false
		b.adoptionScanMu.Unlock()
		log.Printf("papio: adoption scans resumed")
	})
	if errors.Is(err, ErrAdoptionScanTimeout) {
		b.adoptionScanMu.Lock()
		b.adoptionScanSuspended = true
		b.adoptionScanMu.Unlock()
		log.Printf("papio: adoption scans suspended: %s not responding (macOS privacy consent?)", b.cfg.EffectiveAdoptionRoot())
	}
	return entries, err
}

// scanAdoptionDir looks for exactly one settled candidate file in an
// adoptable job's adoption directory. Dotfiles (.DS_Store) are invisible; any
// .crdownload/.download marks an in-progress Chrome write and .part a
// Firefox one; either defers the whole scan. A zero-byte file is the browser's
// placeholder target (Firefox creates the final name empty while streaming
// into name.part), never a settled download, so it defers the scan too. More
// than one visible file is ambiguous and adopts nothing. The returned name
// feeds adopt(), which re-applies full confinement checks.
func (b *Bridge) scanAdoptionDir(_ context.Context, jobID string) (string, bool) {
	dir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	entries, err := b.readAdoptionDir(dir)
	if err != nil {
		return "", false // no directory yet, scan suspended/timed out, or unreadable
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

// SweepAdoptions adopts any settled file sitting in an adoptable job's
// adoption directory, independently of whether the extension is connected,
// has said hello, or has an open handoff action. A browser download can arrive
// while a page-acquired job is still live; the adoption path parks that job at
// awaiting_human and this job-scoped directory is the durable pickup signal.
// This makes directory adoption self-driving: the daemon owns completion, the
// browser plane is only a delivery hint. It scans the adoption root directly
// rather than the newest-N job list, so a settled download is never missed
// behind a large handoff backlog. It never emits frames or opens offers. Safe
// to call on a timer.
func (b *Bridge) SweepAdoptions(ctx context.Context) error {
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
		jobID := e.Name()
		row, err := b.jobs.Get(ctx, jobID)
		if err != nil || row == nil {
			continue
		}
		switch row.State {
		case job.StateQueued, job.StateResolving, job.StateFetching, job.StateAwaitingHuman:
		default:
			continue
		}
		name, ok := b.scanAdoptionDir(ctx, jobID)
		if !ok {
			continue
		}
		if _, err := b.adopt(ctx, jobID, name); err != nil {
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

// poll offers outstanding handoff jobs (once per hello-session), announces
// daemon-side cancels, and drains focus requests for the current holder.
func (b *Bridge) poll(ctx context.Context) ([]json.RawMessage, error) {
	// Auth-return and session-evidence reoffers are deliberately bounded. Keep
	// walking that queue on ordinary browser ticks once capacity is available;
	// this is what drains a parked backlog without requiring another evidence
	// frame.
	if b.reofferSourceJobID != "" && !b.reofferRanThisSync {
		if err := b.reofferInstitutionalSiblings(ctx, b.reofferSourceJobID); err != nil {
			return nil, err
		}
		b.reofferRanThisSync = true
	}
	awaiting, err := b.jobs.List(ctx, job.StateAwaitingHuman, 200)
	if err != nil {
		return nil, err
	}
	handoffJobs, _, err := b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	if err != nil {
		return nil, err
	}
	handoff := make(map[string]job.HumanAction, len(handoffJobs))
	present := map[string]bool{}
	for _, item := range handoffJobs {
		handoff[item.Row.ID] = item.Action
		present[item.Row.ID] = true
	}
	// adoptOutsideSessionLock releases b.mu; a concurrent claim/takeover in
	// that window must abort this poll — its offers would go to a demoted
	// session and its bookkeeping would pollute the new holder's maps.
	epoch := b.epoch
	var out []json.RawMessage
	for i := range awaiting {
		row := awaiting[i]
		present[row.ID] = true
		// Directory-scan adoption: a file the user (or a steered Chrome
		// download) placed in the job's adoption directory is the strongest
		// job-scoped gesture available. Exactly one settled regular file
		// adopts; zero or several (or an in-progress .crdownload) waits —
		// ambiguity stays with the user, per the fail-closed rule.
		if name, ok := b.scanAdoptionDir(ctx, row.ID); ok {
			_, err := b.adoptOutsideSessionLock(ctx, row.ID, name)
			if b.epoch != epoch {
				return out, nil
			}
			if err != nil {
				if evErr := b.jobs.S.AppendEvent(ctx, row.ID, "browser.adoption_deferred",
					map[string]any{"filename": name, "reason": truncate(err.Error(), 200)}); evErr != nil {
					return nil, evErr
				}
			} else {
				delete(b.offered, row.ID)
				delete(b.reofferPending, row.ID)
				delete(handoff, row.ID)
				continue // adopted; the job has left awaiting_human
			}
		}
	}

	// The ordinary job list is bounded for directory scans. Open handoff
	// actions are joined to their awaiting jobs in one query, so offer pacing
	// can drain backlogs larger than that page without one Get per action.
	rows := make(map[string]job.Row, len(handoffJobs))
	candidateIDs := make([]string, 0, len(handoffJobs))
	seen := make(map[string]bool, len(handoffJobs))
	addCandidate := func(row job.Row) {
		if row.State != job.StateAwaitingHuman {
			return
		}
		if _, ok := handoff[row.ID]; !ok || seen[row.ID] {
			return
		}
		seen[row.ID] = true
		rows[row.ID] = row
		candidateIDs = append(candidateIDs, row.ID)
	}
	for i := range awaiting {
		addCandidate(awaiting[i])
	}
	for _, item := range handoffJobs {
		addCandidate(item.Row)
	}
	for id := range b.reofferPending {
		if !seen[id] {
			delete(b.reofferPending, id)
		}
	}
	sort.SliceStable(candidateIDs, func(i, j int) bool {
		priority := func(id string) int {
			switch {
			case b.reofferPending[id]:
				return 0
			case b.focusPending[id]:
				return 1
			default:
				return 2
			}
		}
		iPriority := priority(candidateIDs[i])
		jPriority := priority(candidateIDs[j])
		if iPriority != jPriority {
			return iPriority < jPriority
		}
		if rows[candidateIDs[i]].CreatedAt != rows[candidateIDs[j]].CreatedAt {
			return rows[candidateIDs[i]].CreatedAt < rows[candidateIDs[j]].CreatedAt
		}
		return candidateIDs[i] < candidateIDs[j]
	})
	outstanding := outstandingOfferCount(b.offered, handoff, rows, b.pendingDownloads)
	slots := maxOutstandingOffers - outstanding
	if slots < 0 {
		slots = 0
	}
	held := 0
	heldIDs := make(map[string]bool)
	for _, id := range candidateIDs {
		if hasSettledDownload(b.pendingDownloads, id) || b.offered[id] {
			continue
		}
		row := rows[id]
		action := handoff[id]
		// The main auto-offer gate. focusPending is an explicit
		// `papio actions open`, and reofferPending was already filtered when it
		// was set, so both are honoured; a plain session-live tick is not.
		//
		// Age alone is not enough: the verified field incident aged only 3.07
		// days into QuiesceAfter's seven-day fence while being offered 38
		// times with zero terminal outcomes. ProjectHandoffOfferState reads
		// what each accepted drive actually did and quiesces on fruitless
		// epochs regardless of how young the action still is.
		overridden := b.focusPending[id] || b.reofferPending[id]
		quiescedByEvidence := false
		if !overridden {
			events, err := b.jobs.Events(ctx, id)
			if err != nil {
				return nil, err
			}
			state := job.ProjectHandoffOfferState(events, action.CreatedAt, b.now())
			quiescedByEvidence = state.Quiesced
			if quiescedByEvidence {
				audited := false
				for _, ev := range events {
					if kind, _ := ev["kind"].(string); kind == "browser.handoff_quiesced" {
						audited = true
						break
					}
				}
				if !audited {
					if err := b.jobs.S.AppendEvent(ctx, id, "browser.handoff_quiesced",
						map[string]any{"reason": "fruitless_drive_limit", "drive_epochs": state.FruitlessEpochs}); err != nil {
						return nil, err
					}
				}
			}
		}
		if (action.Quiesced(b.now()) || quiescedByEvidence) && !overridden {
			continue
		}
		accessMode, offerable := b.offerableAccessMode(row)
		if !offerable {
			delete(b.reofferPending, id)
			continue
		}
		if slots <= 0 {
			held++
			heldIDs[id] = true
			continue
		}
		offer, err := b.offer(row, action, accessMode)
		if err != nil {
			return nil, err
		}
		if err := b.jobs.S.AppendEvent(ctx, row.ID, "browser.handoff_offered",
			map[string]any{"requires_auth": action.RequiresAuth}); err != nil {
			return nil, err
		}
		out = append(out, offer)
		b.offered[row.ID] = true
		delete(b.reofferPending, row.ID)
		slots--
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
	if b.epoch != epoch {
		return out, nil
	}
	if b.holder != nil && b.holder.ID != legacySessionID && compareVersion(b.holder.ExtensionVersion, HandoffFocusMinExtensionVersion) >= 0 {
		ids := make([]string, 0, len(b.focusPending))
		for id := range b.focusPending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		focused := 0
		for _, id := range ids {
			if _, ok := handoff[id]; !ok {
				delete(b.focusPending, id)
				continue
			}
			row, getErr := b.jobs.Get(ctx, id)
			switch {
			case errors.Is(getErr, sql.ErrNoRows):
				delete(b.focusPending, id)
				continue
			case getErr != nil:
				return nil, getErr
			case row.State != job.StateAwaitingHuman:
				delete(b.focusPending, id)
				continue
			}
			if hasSettledDownload(b.pendingDownloads, id) {
				delete(b.focusPending, id)
				continue
			}
			accessMode, offerable := b.offerableAccessMode(*row)
			if !offerable {
				delete(b.focusPending, id)
				continue
			}
			if !b.offered[id] {
				if slots <= 0 {
					if !heldIDs[id] {
						held++
						heldIDs[id] = true
					}
					continue
				}
				offer, offerErr := b.offer(*row, handoff[id], accessMode)
				if offerErr != nil {
					return nil, offerErr
				}
				if err := b.jobs.S.AppendEvent(ctx, id, "browser.handoff_offered",
					map[string]any{"requires_auth": handoff[id].RequiresAuth}); err != nil {
					return nil, err
				}
				out = append(out, offer)
				b.offered[id] = true
				delete(b.reofferPending, id)
				slots--
			}
			frame, frameErr := b.frame(protocol.MsgHandoffFocus, id, protocol.EmptyPayload{})
			if frameErr != nil {
				return nil, frameErr
			}
			out = append(out, frame)
			delete(b.focusPending, id)
			focused++
			if focused >= maxFocusFramesPerPoll {
				break
			}
		}
	}
	if held == 0 {
		// A future backlog is a new pacing episode and deserves a fresh event,
		// even when it happens to hold the same number of jobs.
		b.lastPacedHeld = 0
	} else if held != b.lastPacedHeld {
		if err := b.jobs.S.AppendEvent(ctx, "", "browser.offers_paced", map[string]any{"held": held}); err != nil {
			return nil, err
		}
		b.lastPacedHeld = held
	}
	if b.holder != nil {
		if pending := b.pendingCaptures[b.holder.ID]; pending != nil && !pending.delivered {
			frame, err := b.frame(protocol.MsgPageCaptureRequest, "", pending.payload)
			if err != nil {
				return nil, err
			}
			out = append(out, frame)
			pending.delivered = true
		}
	}
	return out, nil
}

// offerableAccessMode resolves the access mode to advertise for one handoff
// offer, and reports whether the job may be offered at all.
//
// papio-browser/1 permits only assisted and delegated in a job_offer, because
// conservative never opens an institutional handoff. A parked job that resolves
// to conservative is therefore stale state: a row parked before the operator
// tightened the daemon mode, or before per-request overrides were honoured.
// Skipping it is the honest answer. Emitting it would fail the outbound
// self-validation, and a non-nil error out of Sync tears down the entire
// native-messaging session rather than dropping one offer.
func (b *Bridge) offerableAccessMode(row job.Row) (string, bool) {
	switch mode := b.cfg.EffectiveAccessMode(row.Policy.AccessMode); mode {
	case config.ModeAssisted, config.ModeDelegated:
		return mode, true
	default:
		return "", false
	}
}

// offer builds a job_offer for one parked handoff job. OA browser handoffs
// reuse the frozen OpenURL field with the candidate's public URL;
// institutional handoffs take the profile's route — the LibKey.io link when
// configured (ADR-0016), else the plain OpenURL resolver link. A LibKey
// route opens on libkey.io and then forwards through the institution's
// resolver, so the resolver host must stay on the offer's host list or the
// extension goes blind exactly at the redirect. accessMode comes from
// offerableAccessMode, so this never has to re-derive it.
func (b *Bridge) offer(row job.Row, action job.HumanAction, accessMode string) (json.RawMessage, error) {
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	libKeyURL := LibKeyURL(inst, row.Work)
	offerURL := RouteURL(inst, row.Work)
	if oaURL, ok := app.OABrowserHandoffURL(action.Detail); ok {
		offerURL = oaURL
	}
	if retrievalURL, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
		// ADR-0017's 2026-08-07 amendment: a fulfilled document-delivery
		// request's form-75 "View PDF" URL, not the institution's
		// ordinary resolver route.
		offerURL = retrievalURL
	}
	hosts := []string{}
	if h := resolverHost(offerURL); h != "" {
		hosts = append(hosts, h)
	}
	if libKeyURL != "" && offerURL == libKeyURL {
		if h := resolverHost(inst.OpenURLBase); h != "" && h != libKeyHost {
			hosts = append(hosts, h)
		}
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
		AccessMode:    accessMode,
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
		if !row.Work.HasFetchableIdentifier() {
			// The OA URL can still be offered directly, but a failed OA fetch
			// cannot become an institutional request without identity the
			// resolver can consume.
			if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
				return false, err
			}
			if err := b.leaveHandoff(ctx, jobID, job.StateUnavailable, string(job.TerminalReasonNoIdentifier)); err != nil {
				return false, err
			}
			return true, nil
		}
		if _, err := b.jobs.OpenHumanAction(ctx, jobID, handoffActionKind, app.InstitutionalOpenURLHandoffDetail, job.Access(true, "paywall")); err != nil {
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
