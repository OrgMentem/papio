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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"papio/internal/app"
	"papio/internal/batch"
	"papio/internal/captures"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/grab"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/ownership"
	"papio/internal/pdf"
	"papio/internal/preview"
	"papio/internal/protocol"
	"papio/internal/pulse"
	"papio/internal/routes"
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
	MinExtensionVersion = "0.5.0"
	// DirectRouteMinExtensionVersion is the first extension that understands
	// direct provider-file URLs in a job_offer. Older extensions would open
	// these URLs in a broker tab and cannot participate in route sequencing.
	DirectRouteMinExtensionVersion = "0.13.0"
	pageAcquireFeature             = "page_acquire"
	triageSnapshotFeature          = "triage_snapshot_v1"
	triageSnapshotSchema2Feature   = "triage_snapshot_schema_v2"
	triageMutationsFeature         = "triage_mutations_v1"
	reviewPreviewFeature           = "review_preview_v1"
	statsFeature                   = "browser_stats_v1"
	pageCaptureFeature             = "page_capture_v1"
	pageCaptureRequestFeature      = "page_capture_request_v1"
	activityFeedFeature            = "activity_feed_v1"
	triageCountsSchema2Feature     = "triage_counts_schema_v2"
	sessionEvidenceFeature         = "session_evidence_v1"
	deliveryContextFeature         = "delivery_context_v1"
	pageCaptureTermsFeature        = "page_capture_terms_v1"
	triageSnapshotSchema3Feature   = "triage_snapshot_schema_v3"
	triageSnapshotSchema4Feature   = "triage_snapshot_schema_v4"
	pageBulkAcquireFeature         = "page_bulk_acquire_v1"
	// New negotiated read/presentation capabilities. Keep this order frozen
	// with the hello_ack feature assertion.
	surfacePresenceFeature       = "surface_presence_v1"
	workPulseFeature             = "work_pulse_v1"
	activityPageV2Feature        = "activity_page_v2"
	pageBulkCohortV2Feature      = "page_bulk_cohort_v2"
	triageCountsSchema3Feature   = "triage_counts_schema_v3"
	triageSnapshotSchema5Feature = "triage_snapshot_schema_v5"
	// sessionRolesFeature tells the extension this daemon acknowledges a
	// hello it denies holdership to, and labels every hello_ack with the
	// role it grants ("holder" or "pending"). Without it a pending session
	// learns no daemon features and locally refuses every capability, even
	// the holder-independent ones the dispatcher already admits from it.
	sessionRolesFeature = "session_roles_v1"
	// pdfGrabV1Feature is ADR-0020's PDF-grab capability flag: it gates both
	// the pdf_grab_request/pdf_grab_result message pair and, extension-side,
	// whether the workspace even renders the grab row.
	pdfGrabV1Feature                    = "pdf_grab_v1"
	handoffLinkV1Feature                = "handoff_link_v1"
	providerDirectGetV1Feature          = "provider_direct_get_v1"
	providerDriveEpochV1Feature         = "provider_drive_epoch_v1"
	effectPermitFeature                 = protocol.EffectPermitFeature
	institutionalMaterializationFeature = protocol.InstitutionalMaterializationFeature
	// surfaceCloseFeature gates the generic one-use close-authorization
	// request/response pair (surface_close_request/surface_close_response),
	// independent of institutional_authentication_claim_v1.
	surfaceCloseFeature = protocol.SurfaceCloseFeature
	// institutionalAuthenticationClaimFeature gates the full claim-request
	// and claim-observation family (authentication_claim_request/response,
	// claim_observation/claim_observation_ack). This is the 32nd and last
	// advertised feature before the fail-closed 32-feature cap
	// (protocol.go's hello.features/hello_ack.features bound, pinned by
	// protocol_test.go) — no further protocol feature may be added without
	// retiring or consolidating an existing one first
	// (dev/active/claim-observation-protocol.md §1).
	institutionalAuthenticationClaimFeature = protocol.InstitutionalAuthenticationClaimFeature
	// pdfGrabSuggestV1Feature gates the inbox's operator candidate picker:
	// the pdf_grab_suggest_request/response ranked "which pending job is
	// this?" answer and the pdf_grab_confirm_request/response fenced bind
	// on the operator's chosen job (both wire Bridge.SuggestGrabCandidates/
	// ConfirmGrabCandidate, the same methods `papio grabs suggest`/`grabs
	// confirm` already call locally). An extension below this feature keeps
	// showing the old provide_identifier guidance text instead of sending a
	// frame the daemon has not proven it can answer.
	pdfGrabSuggestV1Feature = protocol.PdfGrabSuggestFeature
	// ProviderDirectGetMinExtensionVersion gates the additive frame away from
	// released 0.13.x sessions whose strict parser cannot know this message.
	ProviderDirectGetMinExtensionVersion = "0.14.0"
	// pageBulkConsumer is the sole daemon-assigned consumer for every job
	// created through page_bulk_submit_request (ADR-0019 Decision 6). The
	// extension never supplies it.
	pageBulkConsumer = "browser-page"
	// pdfGrabConsumerPrefix names every job a PDF grab creates or joins
	// (ADR-0020 Decision 4): "browser-pdf:<host>", the bare tab host the
	// grab came from.
	pdfGrabConsumerPrefix = "browser-pdf:"
	// grabsDirName is the reserved subtree under the adoption root holding
	// one directory per grab id (SweepTerminalAdoptions's unknown-dir
	// hygiene must never treat it as a stray job directory).
	grabsDirName = "grabs"
	// staleAwaitingGrabBudget covers daemon downtime and slow links; the
	// extension's explicit abandon path remains primary.
	staleAwaitingGrabBudget  = 6 * time.Hour
	previewCapabilityTTL     = 10 * time.Minute
	sessionEvidenceThrottle  = 60 * time.Second
	deliveryContextTTL       = 60 * time.Second
	maxOutstandingOffers     = 4
	maxInstitutionalReoffers = 4
	handoffPageLimit         = 500
	// maxFocusFramesPerPoll bounds the focus batch in ONE sync response. Focus
	// requests accumulate from a caller-supplied job-id list, so an unbounded
	// drain is the only term that can push a response past ipc.MaxResultBytes —
	// and a transport-level oversized response is fatal at the host boundary.
	// The remainder stays queued for the next ordinary poll. Pinned by
	// TestSyncResponseFitsResultCap.
	maxFocusFramesPerPoll = 32
	// maxClaimObservationsPerPoll bounds how many queued claim_observation
	// frames one Sync poll answers with acks, mirroring
	// maxFocusFramesPerPoll's rationale: the extension MUST send at most
	// this many per poll (dev/active/claim-observation-protocol.md §5),
	// carrying any remainder to the next poll. Pinned by
	// TestSyncResponseFitsResultCap.
	maxClaimObservationsPerPoll = 32
)

// Session roles carried by hello_ack. Absent means "holder": an old daemon
// only ever acked the session it granted the bridge to.
const (
	sessionRoleHolder  = "holder"
	sessionRolePending = "pending"
)

// PDF-grab refusal reasons. The extension switches on these to pick user
// copy, so the set is closed and every member is produced by exactly one
// condition; Detail stays a human-readable companion, never the thing a UI
// parses. There is deliberately no "session_elsewhere": a grab is
// user-initiated and self-routed, so holdership never refuses one.
const (
	// grabReasonNoSession: the sender never completed a hello.
	grabReasonNoSession = "no_session"
	// grabReasonExtensionOutdated: the session lacks pdf_grab_v1 or the
	// effect-permit capability, or is below MinExtensionVersion.
	grabReasonExtensionOutdated = "extension_outdated"
	// grabReasonDaemonUnsupported: this daemon does not advertise pdf_grab_v1.
	grabReasonDaemonUnsupported = "daemon_unsupported"
	// grabReasonBusy: the single effect lane is occupied by another effect.
	grabReasonBusy = "busy"
	// grabReasonNotConfigured: grab storage is not configured.
	grabReasonNotConfigured = "not_configured"
	// grabReasonAdoptionUnhealthy: the adoption latch is unhealthy (on macOS,
	// usually withheld TCC consent for the downloads folder).
	grabReasonAdoptionUnhealthy = "adoption_unhealthy"
	// grabReasonTabUnusable: the requested tab/URL cannot be grabbed.
	grabReasonTabUnusable = "tab_unusable"
	// grabReasonInternal: an unexpected daemon-side failure.
	grabReasonInternal = "internal"
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

// beforeAutoBindFenceForTest, when non-nil, runs inside the fence transaction
// before eligibility is re-checked. Tests use it to make the fence window
// deterministic without relying on file-system timing. Nil in production.
var beforeAutoBindFenceForTest func() error

// beforeAutoBindTxForTest runs after the pre-transaction candidate decision
// and before the binding transaction opens. That is the window the fence
// exists to cover, and it is the only place a test can change the eligibility
// pool safely: inside the transaction the store's single connection is
// already held, so a pool write there would deadlock. Nil in production.
var beforeAutoBindTxForTest func() error

// afterAutoBindCommitForTest runs after MarkBoundToJobFenced commits but before
// the validated bytes are staged into the winner's adoption directory. Tests
// use it to simulate a concurrent cancel/terminal transition between commit
// and ingest.
var afterAutoBindCommitForTest func(grabID, jobID string) error

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
	Producer    *job.ArtifactProducerIdentity
	ReceivedAt  time.Time
}
type pendingDeliveryContext struct {
	Payload    protocol.DeliveryContextPayload
	ReceivedAt time.Time
}

// materializationOffer is the daemon's in-memory acknowledgement fence for a
// candidate offer. The candidate remains re-offerable while it is still
// eligible and unexpired; the map is keyed by job so cancellation can be
// announced even when no legacy handoff offer was sent.
type materializationOffer struct {
	CandidateID string
	ExpiresAt   time.Time
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

type pendingEffectPermitReconcile struct {
	permitID string
	jobID    string
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
	pulse        *pulse.Service
	cohorts      *batch.Cohorts
	// grabs owns pdf_grabs (ADR-0020); constructed internally in NewBridge
	// from jobs.S rather than threaded through the constructor signature —
	// it shares the same *store.Store every other job.Store-backed accessor
	// in this package does.
	grabs *grab.Service

	// Version and Features are daemon capabilities announced in hello_ack.
	Version  string
	Features []string

	mu                   sync.Mutex
	providerDriveEpochMu sync.Mutex
	seq                  int64
	holder               *browserSession
	pending              map[string]*browserSession
	deniedHellos         int
	takeovers            int
	// epoch increments whenever holder identity changes. Code that releases
	// b.mu mid-flight (adoption windows inside poll) re-checks it afterwards:
	// a concurrent claim/takeover must not let a resumed poll send offers to a
	// demoted session or pollute the new holder's bookkeeping.
	epoch      int64
	offered    map[string]bool // handoff jobs offered to the current holder
	cancelSent map[string]bool // jobs a daemon-side cancel was already announced for
	// A replayed auth return must not make the same holder open duplicate tabs.
	authReleased map[int64]bool
	// reofferPending prioritizes jobs released by the institutional-session
	// sweep when poll turns them back into job_offer frames.
	reofferPending map[string]bool
	// directRouteAttempts retains a never-acquired tuple across busy polls.
	// It is not authorization state; successful acquire is the durable source.
	directRouteAttempts map[string]string
	// effectPermitReconciles binds each browser observation to the exact
	// daemon request that solicited it. Permit IDs alone are not job-scoped
	// correlation and must never authorize an unsolicited settlement.
	effectPermitReconciles map[string]pendingEffectPermitReconcile
	// reofferSourceJobID keeps an authenticated institutional session alive
	// per configured resolver profile across ordinary sync ticks, even after
	// its source settles.
	reofferSourceJobID map[string]string
	// Session evidence is timing-only and throttled per exact configured
	// resolver profile.
	lastSessionEvidenceAt map[string]time.Time
	// Reoffer sweeps are accounted per profile within one sync cycle; a
	// successful sweep for one profile must not suppress another profile.
	reofferRanThisSync map[string]bool
	// lastPacedHeld deduplicates the operator-visible pacing event while a
	// backlog remains unchanged across the native host's two-second polls.
	lastPacedHeld int
	// Completion metadata and delivery context arrive as adjacent extension
	// frames. Keep both briefly so either frame order is safe across a native
	// host RPC boundary; values are never durable and expire quickly.
	pendingDownloads map[browserDownloadKey]pendingBrowserDownload
	deliveryContexts map[browserDownloadKey]pendingDeliveryContext
	// materializationOffered tracks the current candidate offer by job. It is
	// deliberately not a historical dedupe set: an offer is repeated on later
	// polls until the daemon observes a claim, a candidate/status change, or
	// expiry. Entries are removed with their focused job or on holder change.
	materializationOffered map[string]materializationOffer
	// materializationTracked keeps cancellation delivery alive after the
	// candidate offer is claimed and the reoffer state is cleared.
	materializationTracked map[string]bool
	// materializationRecoveryPending requests a durable scan after holder
	// promotion so explicit candidate work survives daemon/worker restart.
	materializationRecoveryPending bool
	// materializationClaimReconcileUnavailable fails closed after a transient
	// expired-claim reconciliation error, without masking profile/generation
	// authority uncertainty.
	materializationClaimReconcileUnavailable bool
	// materializationProfileAuthorityUnavailable keeps profile/key failures
	// distinct from claim-sweep failures; neither may clear the other.
	materializationProfileAuthorityUnavailable bool
	// materializationGenerationUnavailable retries durable holder-generation
	// allocation before exposing materialization after a transient store error.
	materializationGenerationUnavailable bool
	// materializationAuthorityUncertain fails closed after a holder generation
	// change whose stale-claim reconciliation could not be committed.
	materializationAuthorityUncertain bool
	// Focus requests survive a holder change so the replacement holder can
	// receive its offer before it is asked to surface the handoff.
	focusPending map[string]bool
	// materializationScheduleCursor is daemon-side keyset state. It is only
	// advanced after a scheduler query completes while this holder is still
	// current; DB scheduling itself never runs under b.mu.
	materializationScheduleCursor     job.CandidateScheduleCursor
	scheduleCursorPending             job.CandidateScheduleCursor
	materializationScheduleInFlight   bool
	materializationScheduleVersion    uint64
	scheduleHasMorePending            bool
	scheduleEligibleCandidates        func(context.Context, int, job.CandidateScheduleCursor) (job.CandidateSchedulePage, error)
	materializationScheduleBlocked    bool
	materializationScheduleProcessed  bool
	prepareMaterializationCandidateFn func(context.Context, job.Row) (*job.BrowserCandidate, error)
	listAwaitingHuman                 func(context.Context, int) ([]job.Row, error)
	listOpenHandoffs                  func(context.Context, int) ([]job.OpenHandoffJob, bool, error)
	openHandoffForJobFn               func(context.Context, string) (*job.HumanAction, error)
	// session keeps the unchanged page_capture content frame unambiguous.
	pendingCaptures map[string]*pendingPageCapture
	now             func() time.Time
	// readDir is the adoption-directory ReadDir seam; nil in production,
	// where readAdoptionDir falls back to os.ReadDir. Tests substitute a
	// blocking or error-returning func to exercise adoptionScanSuspended
	// below without a real TCC-protected filesystem.
	readDir func(string) ([]os.DirEntry, error)
	// renameFile is the local-file move seam; nil uses os.Rename. Tests may
	// force EXDEV to exercise the copy-and-remove fallback.
	renameFile func(string, string) error
	// adoptionScanMu guards the field below. It is deliberately its own
	// lock, never b.mu: a ReadDir call that hangs behind a TCC consent wall
	// (see scanAdoptionDir) must never hold the session lock, or every other
	// bridge RPC wedges behind it — the exact incident this latch exists to
	// prevent.
	adoptionScanMu sync.Mutex
	// adoptionScanSuspended latches true the instant one adoption-directory
	// ReadDir call misses AdoptionScanDeadline. A hung syscall can block
	// forever, so every scan while this is true short-circuits to "nothing
	// adoptable" without spawning another goroutine — at most one hung call
	// is ever outstanding per bridge. The goroutine that tripped it clears
	// the flag, and logs the recovery, the moment it finally returns.
	// presence carries only focused surface leases; it is bounded and
	// holder-independent because pending sessions may report their own focus.
	presence              map[string]presenceLease
	presenceOrder         []string
	adoptionScanSuspended bool
	// unavailableLogMu guards unavailableLog. Repeated identical read-model
	// failures (same surface and cause) log once per window instead of every
	// UI poll.
	unavailableLogMu sync.Mutex
	unavailableLog   map[string]unavailableLogState
	// adoptionScanGate is a capacity-1 semaphore held for the full lifetime
	// of one underlying root/dir listing.
	adoptionScanGate chan struct{}
}

type presenceLease struct {
	surface  string
	focused  bool
	received time.Time
	clientAt time.Time
}

// PresenceProvider returns the daemon-owned focused-surface hint used by the
// notification router. It carries no browser metadata beyond the lease type.
func (b *Bridge) PresenceProvider() notify.PresenceProvider { return b }

// AnyFocused reports whether any focused popup/inbox lease was received within
// the bounded TTL. Receipt time, not the client timestamp, controls expiry.
func (b *Bridge) AnyFocused(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.anyFocusedLocked(now)
}

func (b *Bridge) anyFocusedLocked(now time.Time) bool {
	expired := false
	focused := false
	for id, lease := range b.presence {
		if now.Sub(lease.received) >= surfacePresenceTTL {
			delete(b.presence, id)
			expired = true
			continue
		}
		if lease.focused {
			focused = true
		}
	}
	if expired {
		b.compactPresenceOrderLocked()
	}
	return focused
}

// compactPresenceOrderLocked keeps the FIFO eviction index in one-to-one
// correspondence with the live lease map. The bounded map is tiny, so doing
// this after expiry is preferable to allowing stale ids to accumulate.
func (b *Bridge) compactPresenceOrderLocked() {
	if len(b.presenceOrder) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(b.presenceOrder))
	compact := b.presenceOrder[:0]
	for _, id := range b.presenceOrder {
		if _, live := b.presence[id]; !live {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		compact = append(compact, id)
	}
	b.presenceOrder = compact
}

// browserSession is one native-host connection that said hello.
type browserSession struct {
	ID               string
	ExtensionVersion string
	AdapterVersions  map[string]string
	Features         []string
	HelloAt          time.Time
	LastSyncAt       time.Time
	Outdated         bool
	// adapterUpgradeRepairPending lets a newly live holder repair parks once
	// without turning each two-second browser poll into a maintenance sweep.
	adapterUpgradeRepairPending bool
	// needsAck makes the next Sync from this session deliver a hello_ack:
	// a session promoted by claim or stale-takeover was only acked as
	// sessionRolePending at hello time and must hear it now holds the
	// bridge before offers mean anything.
	needsAck bool
	// demotedNotice makes the next Sync from a claim-demoted holder deliver
	// one session_busy frame. `papio browser use` moves the bridge without
	// the old holder's extension ever hearing about it, so that browser kept
	// reporting a live connection while receiving no offers. A hello-time
	// takeover needs no flag: it drops the previous holder outright, whose
	// next poll is answered with expected_hello.
	demotedNotice bool
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
const (
	surfacePresenceTTL = 120 * time.Second
	maxPresenceLeases  = 256
)

// pendingExpireAfter prunes pending sessions whose native host stopped
// syncing without a goodbye (browser killed) so `papio browser sessions`
// reflects reality.
const pendingExpireAfter = 5 * time.Minute

type parkedGrabItemSource struct {
	store *store.Store
}

func (source parkedGrabItemSource) SnapshotItems(ctx context.Context, tx *sql.Tx) ([]triage.Item, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(NULLIF(title, ''), url_host), state
		FROM pdf_grabs
		WHERE state = 'parked_no_identifier'
		ORDER BY updated_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]triage.Item, 0)
	for rows.Next() {
		var id, title, state string
		if err := rows.Scan(&id, &title, &state); err != nil {
			return nil, err
		}
		items = append(items, triage.Item{
			Kind:  triage.KindPdfGrab,
			ID:    triage.PdfGrabIDPrefix + id,
			Title: title,
			Ops:   []string{"provide_identifier", "dismiss"},
			PdfGrab: &triage.PdfGrab{
				GrabID: id, State: state,
			},
		})
	}
	return items, rows.Err()
}
func NewBridge(jobs *job.Store, svc *app.Service, triageService *triage.Service, watchRunner *watch.Runner, previewServer *preview.Server, captureStore *captures.Store, holdings holdingsProvider, zotioService *zotio.Service, cfg config.Config, version string) *Bridge {
	required := []string{
		pageAcquireFeature, triageSnapshotFeature, triageSnapshotSchema2Feature, triageMutationsFeature, reviewPreviewFeature, statsFeature, pageCaptureFeature, pageCaptureRequestFeature, activityFeedFeature, triageCountsSchema2Feature, sessionEvidenceFeature, deliveryContextFeature, pageCaptureTermsFeature, pageBulkAcquireFeature, triageSnapshotSchema3Feature, triageSnapshotSchema4Feature, pdfGrabV1Feature, handoffLinkV1Feature, providerDirectGetV1Feature, providerDriveEpochV1Feature, effectPermitFeature, institutionalMaterializationFeature,
		surfacePresenceFeature, workPulseFeature, activityPageV2Feature, pageBulkCohortV2Feature, triageCountsSchema3Feature, triageSnapshotSchema5Feature, sessionRolesFeature, pdfGrabSuggestV1Feature, surfaceCloseFeature,
		// institutionalAuthenticationClaimFeature is the 32nd and final slot
		// under the fail-closed cap (see its own doc comment); do not add
		// another feature after it without retiring one first.
		institutionalAuthenticationClaimFeature,
	}
	var grabs *grab.Service
	var cohorts *batch.Cohorts
	if jobs != nil {
		grabs = grab.New(jobs.S, nil)
		if jobs.S != nil {
			cohorts = batch.New(jobs.S)
		}
		if triageService != nil {
			triageService.RegisterSource(parkedGrabItemSource{store: jobs.S})
		}
	}
	return &Bridge{
		jobs: jobs, svc: svc, triage: triageService, watchRunner: watchRunner, preview: previewServer, captureStore: captureStore, holdings: holdings, zotio: zotioService, cfg: cfg, cohorts: cohorts,
		grabs:                  grabs,
		Version:                version,
		Features:               required,
		offered:                map[string]bool{},
		cancelSent:             map[string]bool{},
		pending:                map[string]*browserSession{},
		authReleased:           map[int64]bool{},
		reofferPending:         map[string]bool{},
		directRouteAttempts:    map[string]string{},
		effectPermitReconciles: map[string]pendingEffectPermitReconcile{},
		reofferSourceJobID:     map[string]string{},
		lastSessionEvidenceAt:  map[string]time.Time{},
		reofferRanThisSync:     map[string]bool{},
		pendingDownloads:       map[browserDownloadKey]pendingBrowserDownload{},
		deliveryContexts:       map[browserDownloadKey]pendingDeliveryContext{},
		focusPending:           map[string]bool{},
		pendingCaptures:        map[string]*pendingPageCapture{},
		materializationOffered: map[string]materializationOffer{},
		presence:               map[string]presenceLease{},
		unavailableLog:         map[string]unavailableLogState{},
		materializationTracked: map[string]bool{},
		now:                    time.Now,
		readDir:                os.ReadDir,
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
	holderID := b.holder.ID
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
		if jobID == "" || !handoff[jobID] {
			continue
		}
		// A focus already owed is not a refusal, and it must not skip the work
		// either: the marker is a sticky priority flag that outlives the poll
		// which delivered its frame, so short-circuiting on it meant an
		// operator asking again never reached the attempt decision below - the
		// exact case of a paper whose sign-in stalled. Re-marking an owed job
		// is idempotent; the paths that bail out below now decline to count a
		// focus that will not happen, which is what the count is for.
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
		if b.institutionalMaterializationAvailable() {
			epoch := b.epoch
			b.mu.Unlock()
			// An operator asking again for a paper whose attempt already
			// navigated and never delivered is the retry decision the
			// one-navigation-per-attempt invariant waits for; without it
			// prepareMaterializationCandidate below returns the same pinned
			// candidate forever and every claim answers 'busy'. A no-op unless
			// that attempt is provably spent.
			if started, startErr := b.jobs.StartNextMaterializationAttemptForSpentCandidate(ctx, row.ID); startErr != nil {
				log.Printf("papio: starting the next materialization attempt for %s: %v", row.ID, startErr)
			} else if started {
				log.Printf("papio: %s asked again for a spent institutional attempt; starting the next one", row.ID)
			}
			candidate, candidateErr := b.prepareMaterializationCandidate(ctx, *row)
			b.mu.Lock()
			if b.epoch != epoch || b.holder == nil || b.holder.ID != holderID {
				if b.holder != nil && b.holder.ID != legacySessionID && b.institutionalMaterializationAvailable() {
					b.focusPending[row.ID] = true
					b.materializationTracked[row.ID] = false
					queued++
					continue
				}
				return queued, false, nil
			}
			if candidateErr != nil {
				return queued, true, candidateErr
			}
			if candidate == nil {
				continue
			}
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
	b.reconcileMaterializationGeneration(context.Background())
	return b.holder.ID, nil
}

// promote makes session the holder. The caller holds b.mu. The previous
// holder, when still present, is demoted to pending rather than dropped so an
// explicit claim can be reversed with another claim.
func (b *Bridge) promote(session *browserSession, reason string) {
	if b.holder != nil && b.holder.ID != session.ID {
		b.holder.demotedNotice = true
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
	if b.jobs != nil {
		generation, err := b.jobs.NextMaterializationHolderGeneration(context.Background())
		if err != nil {
			b.materializationAuthorityUncertain = true
			b.materializationGenerationUnavailable = true
			log.Printf("papio: materialization holder generation unavailable: %v", err)
		} else {
			b.epoch = generation
			b.materializationAuthorityUncertain = false
			b.materializationGenerationUnavailable = false
		}
	}
	delete(b.pending, session.ID)
	session.needsAck = true
	// A pending browser has not been allowed to offer work, so it must check
	// upgrade repairs when it becomes the live holder.
	b.holder = session
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	b.authReleased = map[int64]bool{}
	b.reofferPending = map[string]bool{}
	b.materializationOffered = map[string]materializationOffer{}
	b.materializationTracked = map[string]bool{}
	b.reofferRanThisSync = map[string]bool{}
	b.effectPermitReconciles = map[string]pendingEffectPermitReconcile{}

	b.materializationScheduleCursor = job.CandidateScheduleCursor{}
	b.scheduleCursorPending = job.CandidateScheduleCursor{}
	b.scheduleHasMorePending = false
	b.materializationScheduleBlocked = false
	b.materializationScheduleProcessed = false
	b.materializationScheduleInFlight = false
	b.materializationScheduleVersion++
	b.materializationRecoveryPending = true
	b.takeovers++
	log.Printf("papio: browser session %s (v%s) now holds the bridge: %s", shortSession(session.ID), session.ExtensionVersion, reason)
}

// reconcileMaterializationGeneration abandons claims fenced to older holder
// generations before the replacement holder can receive any candidate offer.
// A store failure is fail-closed: materialization offers stay disabled until a
// later holder promotion successfully reconciles the durable fence.
func (b *Bridge) reconcileMaterializationGeneration(ctx context.Context) {
	if b.jobs == nil || b.materializationGenerationUnavailable || b.materializationProfileAuthorityUnavailable {
		return
	}
	count, err := b.jobs.AbandonStaleMaterializations(ctx, b.epoch)
	if err != nil {
		b.materializationAuthorityUncertain = true
		log.Printf("papio: abandoning stale materialization claims failed: %v", err)
		return
	}
	b.materializationAuthorityUncertain = false
	if count > 0 {
		log.Printf("papio: abandoned %d stale materialization claims for holder generation %d", count, b.epoch)
	}
}

// recoverMaterializationFocus reconstructs only explicit, durable candidate
// work after a holder/daemon restart. Candidate rows are never created here;
// they must already be attached to an open institutional handoff action.
func (b *Bridge) recoverMaterializationFocus(ctx context.Context) error {
	if b.materializationGenerationUnavailable || b.materializationProfileAuthorityUnavailable || b.materializationAuthorityUncertain || b.materializationClaimReconcileUnavailable {
		return nil
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return err
	}
	for _, action := range actions {
		if action.Kind != handoffActionKind || action.Status != "open" || action.JobID == "" {
			continue
		}
		row, rowErr := b.jobs.Get(ctx, action.JobID)
		if errors.Is(rowErr, sql.ErrNoRows) || rowErr != nil || row.State != job.StateAwaitingHuman {
			continue
		}
		attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, row.ID)
		if attemptErr != nil || attempt < 1 {
			continue
		}
		candidate, candidateErr := b.jobs.CurrentBrowserCandidateForJob(ctx, row.ID, attempt)
		if candidateErr != nil || candidate == nil {
			continue
		}
		if candidate.Status != "eligible" && candidate.Status != "claimed" && candidate.Status != "materializing" {
			continue
		}
		b.focusPending[row.ID] = true
		b.materializationTracked[row.ID] = true
		b.materializationOffered[row.ID] = materializationOffer{CandidateID: candidate.ID}
	}
	return nil
}
func shortSession(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ErrOutboundFrame marks a daemon-side contract failure while constructing an
// outbound browser frame. It is transport-fatal at the API/native-host
// boundary: silently converting it to an application refusal would hide a
// broken protocol implementation.
var ErrOutboundFrame = errors.New("outbound frame self-validation failed")

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
	b.reofferRanThisSync = map[string]bool{}
	generationAtStart := b.epoch
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
			ack, err := b.helloAck(sessionRoleHolder)
			if err != nil {
				return nil, err
			}
			out = append(out, ack)
		}
	}
	// A claim moved the bridge out from under this session. Tell it once, with
	// the same frame a denied hello gets, so its UI can stop claiming a live
	// papio connection and name the browser that now holds one.
	if demoted, ok := b.pending[sessionID]; ok && demoted.demotedNotice {
		demoted.demotedNotice = false
		busy, err := b.sessionBusy("")
		if err != nil {
			return nil, err
		}
		out = append(out, busy...)
	}
	for _, raw := range frames {
		var msg *protocol.BrowserMessage
		var err error
		if b.legacyInstitutionalNavigationAllowed(sessionID) {
			msg, err = protocol.DecodeBrowserMessageWithLegacyInstitutionalNavigation(raw)
		} else {
			msg, err = protocol.DecodeBrowserMessage(raw)
		}
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
			if b.epoch != generationAtStart || b.materializationAuthorityUncertain {
				b.reconcileMaterializationGeneration(ctx)
			}
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
	if b.epoch != generationAtStart || b.materializationAuthorityUncertain {
		b.reconcileMaterializationGeneration(ctx)
	}
	b.repairAdapterUpgradeParks(ctx)
	// Candidate selection is durable scheduler work, not session arbitration.
	// Release b.mu while the indexed query runs so a stalled database cannot
	// prevent a live replacement holder from taking over. Revalidate both
	// holder identity and generation before using any returned descriptors.
	var scheduled []job.BrowserCandidateDescriptor
	schedulingUnavailable := false
	scheduleRan := false
	scheduleEpoch := b.epoch
	scheduleVersion := b.materializationScheduleVersion
	if b.jobs != nil && b.institutionalMaterializationAvailable() && !b.materializationScheduleInFlight {
		cursor := b.materializationScheduleCursor
		b.materializationScheduleInFlight = true
		b.materializationScheduleVersion++
		scheduleVersion = b.materializationScheduleVersion
		scheduleRan = true
		schedule := b.scheduleEligibleCandidates
		if schedule == nil {
			schedule = func(sctx context.Context, slimit int, scursor job.CandidateScheduleCursor) (job.CandidateSchedulePage, error) {
				return b.jobs.ScheduleEligibleBrowserCandidates(sctx, slimit, scursor)
			}
		}
		b.mu.Unlock()
		page, scheduleErr := schedule(ctx, maxOutstandingOffers, cursor)
		b.mu.Lock()
		if b.materializationScheduleVersion != scheduleVersion {
			return out, nil
		}
		b.materializationScheduleInFlight = false
		if b.epoch != scheduleEpoch || b.holder == nil || b.holder.ID != sessionID {
			return out, nil
		}
		if scheduleErr != nil {
			schedulingUnavailable = true
			log.Printf("papio: materialization scheduler unavailable: %v", scheduleErr)
		} else {
			scheduled = page.Candidates
			b.scheduleCursorPending = page.Cursor
			b.scheduleHasMorePending = page.HasMore
		}
	}
	b.materializationScheduleBlocked = false
	b.materializationScheduleProcessed = false
	offeredBefore := maps.Clone(b.offered)
	materializationOfferedBefore := maps.Clone(b.materializationOffered)
	materializationTrackedBefore := maps.Clone(b.materializationTracked)
	deliveredBefore := make(map[string]bool, len(b.pendingCaptures))
	for id, pending := range b.pendingCaptures {
		if pending != nil {
			deliveredBefore[id] = pending.delivered
		}
	}
	polled, err := b.poll(ctx, scheduled, schedulingUnavailable)
	if err != nil {
		// poll may stage offers/cancels or mark a pending capture delivered
		// before a later frame-construction failure. The API classifies routine
		// failures as non-fatal and discards this reply, so roll back all
		// in-memory staging that the browser never observed.
		b.offered = offeredBefore
		b.materializationOffered = materializationOfferedBefore
		b.materializationTracked = materializationTrackedBefore
		for id, pending := range b.pendingCaptures {
			if pending != nil {
				pending.delivered = deliveredBefore[id]
			}
		}
		return nil, err
	}
	if scheduleRan && !schedulingUnavailable && !b.materializationScheduleBlocked &&
		b.materializationScheduleProcessed && b.materializationScheduleVersion == scheduleVersion &&
		b.epoch == scheduleEpoch && b.holder != nil && b.holder.ID == sessionID {
		b.materializationScheduleCursor = b.scheduleCursorPending
		if !b.scheduleHasMorePending {
			b.materializationScheduleCursor = job.CandidateScheduleCursor{}
		}
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
	if err := b.svc.HandoffRepairer().RepairAdapterUpgrade(
		ctx,
		b.holder.ExtensionVersion,
		b.holder.AdapterVersions,
		extensionVersionNewer,
	); err != nil {
		log.Printf("papio: repairing provider parks after adapter upgrade: %v", err)
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
		b.reofferSourceJobID = map[string]string{}
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

// helloAck builds the capability acknowledgement frame for the role this
// session was granted: sessionRoleHolder, or sessionRolePending for a hello
// that lost arbitration. A pending session is acked too — it still drives
// the holder-independent surfaces the dispatcher admits from a non-holder,
// and it can only know they exist from the feature list carried here.
// The caller holds b.mu.
func (b *Bridge) helloAck(role string) (json.RawMessage, error) {
	payload := protocol.HelloAckPayload{
		DaemonVersion:   b.Version,
		Features:        b.Features,
		ResolverOrigins: b.cfg.ResolverOrigins(),
		Role:            role,
	}
	if role == sessionRoleHolder {
		generation := b.epoch
		payload.BrowserHolderGeneration = &generation
	}
	return b.frame(protocol.MsgHelloAck, "", payload)
}

func institutionalMaterializationMessage(msgType string) bool {
	switch msgType {
	case protocol.MsgInstitutionalClaimRequest, protocol.MsgInstitutionalBindRequest,
		protocol.MsgInstitutionalRouteRequest, protocol.MsgInstitutionalNavigatedRequest,
		protocol.MsgInstitutionalReconcileRequest:
		return true
	}
	return false
}

// institutionalAuthenticationClaimMessage reports whether msgType is part
// of the claim-observation protocol family
// (dev/active/claim-observation-protocol.md §2.1/§2.2).
// authentication_claim_request precedes a requires_auth tab and is
// current-holder-only; claim_observation is a historical-report frame,
// admitted from any known session per Decision 4's "an old holder's
// browser is still allowed to report what already happened locally" —
// every reducer path checks browser_holder_generation before mutating, so a
// non-holder or demoted session cannot revive a stale claim, it can only
// report it (which acks stale).
func institutionalAuthenticationClaimMessage(msgType string) bool {
	switch msgType {
	case protocol.MsgAuthenticationClaimRequest, protocol.MsgClaimObservation:
		return true
	}
	return false
}

func (b *Bridge) institutionalMaterializationAvailable() bool {
	return b != nil && b.holder != nil && b.holder.ID != legacySessionID &&
		!b.materializationAuthorityUncertain &&
		!b.materializationProfileAuthorityUnavailable &&
		!b.materializationGenerationUnavailable &&
		!b.materializationClaimReconcileUnavailable &&
		slices.Contains(b.holder.Features, institutionalMaterializationFeature) &&
		slices.Contains(b.holder.Features, effectPermitFeature)
}
func (b *Bridge) legacyInstitutionalNavigationAllowed(sessionID string) bool {
	if b == nil {
		return false
	}
	var session *browserSession
	if b.holder != nil && b.holder.ID == sessionID {
		session = b.holder
	} else {
		session = b.pending[sessionID]
	}
	return session != nil &&
		slices.Contains(session.Features, institutionalMaterializationFeature) &&
		!slices.Contains(session.Features, effectPermitFeature)
}

func materializationMAC(key []byte, label string, values ...string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(label))
	for _, value := range values {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(value))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func materializationRevision(key []byte, values ...string) int64 {
	sum := materializationMAC(key, "route_revision", values...)
	raw, _ := hex.DecodeString(sum[:16])
	n := int64(binary.BigEndian.Uint64(raw) & uint64(^uint64(0)>>1))
	if n < 1 {
		return 1
	}
	return n
}

// authenticationEntryIdentity derives the human sign-in entry each configured
// profile shares, which is what an authentication claim is (ADR-0022; the
// plan's corrected cardinality rule: "a claim may group profiles sharing one
// human entry"). The federated entity ID names that entry when it is
// declared. When it is not, the resolver ORIGIN names it - a library with no
// federated entity is still signed into at one origin - and a profile sharing
// that origin with exactly one declared entity adopts it.
//
// The config name is never the entry identity, which is what this replaces.
// Naming your own institution under [browser.resolvers.<name>] while the same
// resolver is already the top-level default is ordinary configuration, and it
// minted a SECOND claim for one library: two sign-in slots where the
// cardinality invariant promises one, and evidence for one that proves
// nothing for the other. Measured live 2026-08-20 on the operator's own
// config: `default` keyed on its entity, `une` keyed on the string "une".
func authenticationEntryIdentity(cfg *config.Config, name string, inst config.Institution) string {
	if inst.ShibbolethEntityID != "" {
		return inst.ShibbolethEntityID
	}
	origins := cfg.ResolverProfilesForOrigin(resolverOrigin(inst.OpenURLBase))
	declared := ""
	for _, peer := range origins {
		peerInst, ok := cfg.InstitutionFor(peer)
		if !ok || peerInst.ShibbolethEntityID == "" {
			continue
		}
		if declared != "" && declared != peerInst.ShibbolethEntityID {
			// Two federated entities behind one origin are two entries; an
			// entity-less profile cannot be assigned to either.
			declared = ""
			break
		}
		declared = peerInst.ShibbolethEntityID
	}
	if declared != "" {
		return declared
	}
	if origin := resolverOrigin(inst.OpenURLBase); origin != "" {
		return origin
	}
	return name
}

// resolverOrigin reduces a configured OpenURL base to its bare https origin,
// the form ResolverProfilesForOrigin accepts.
func resolverOrigin(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ""
	}
	origin := "https://" + strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		origin += ":" + port
	}
	return origin
}

func (b *Bridge) reconcileMaterializationProfiles(ctx context.Context, key []byte) error {
	specs := make([]job.InstitutionProfileSpec, 0, len(b.cfg.ResolverNames()))
	for _, name := range b.cfg.ResolverNames() {
		inst, ok := b.cfg.InstitutionFor(name)
		if !ok {
			continue
		}
		name = resolverProfileKey(name)
		specs = append(specs, job.InstitutionProfileSpec{
			ConfiguredName: name,
			AuthorityDigest: materializationMAC(key, "profile_authority", name,
				inst.OpenURLBase, inst.ShibbolethEntityID, inst.ProquestAccountID,
				inst.LibKeyMode, strconv.FormatInt(inst.LibKeyLibraryID, 10)),
			AuthenticationClaimID: materializationMAC(key, "authentication_claim",
				authenticationEntryIdentity(&b.cfg, name, inst)),
		})
	}
	_, err := b.jobs.ReconcileInstitutionProfiles(ctx, specs)
	return err
}

func (b *Bridge) openHandoffForJob(ctx context.Context, jobID string) (*job.HumanAction, error) {
	if b.openHandoffForJobFn != nil {
		return b.openHandoffForJobFn(ctx, jobID)
	}
	actions, err := b.jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil {
		return nil, err
	}
	for i := range actions {
		if actions[i].JobID == jobID && actions[i].Kind == handoffActionKind && actions[i].Status == "open" {
			return &actions[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func (b *Bridge) prepareMaterializationCandidate(ctx context.Context, row job.Row) (*job.BrowserCandidate, error) {
	if b.prepareMaterializationCandidateFn != nil {
		return b.prepareMaterializationCandidateFn(ctx, row)
	}
	if b.jobs == nil || row.ID == "" {
		return nil, job.ErrMaterializationStale
	}
	key, err := b.jobs.InstitutionAuthorityKey(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.reconcileMaterializationProfiles(ctx, key); err != nil {
		return nil, err
	}
	profileName := resolverProfileKey(row.Policy.Resolver)
	profile, err := b.jobs.InstitutionProfileByConfiguredName(ctx, profileName)
	if err != nil {
		return nil, err
	}
	if profile == nil || profile.TombstonedAt != "" {
		return nil, job.ErrMaterializationStale
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	if existing, getErr := b.jobs.CurrentBrowserCandidateForJob(ctx, row.ID, attempt); getErr != nil {
		return nil, getErr
	} else if existing != nil {
		return existing, nil
	}
	strategy := "title"
	switch {
	case row.Work.DOI != "":
		strategy = "doi"
	case row.Work.PMID != "":
		strategy = "pmid"
	case row.Work.ArXiv != "":
		strategy = "arxiv"
	case row.Work.ISBN != "":
		strategy = "isbn"
	case row.Work.OpenAlex != "":
		strategy = "openalex"
	}
	routeRevision := materializationRevision(key, profile.ID, strategy, row.ID, strconv.FormatInt(attempt, 10), row.Work.DOI, row.Work.PMID, row.Work.ArXiv, row.Work.ISBN, row.Work.OpenAlex)
	input := job.BrowserCandidateInput{
		ID:    materializationMAC(key, "browser_candidate", profile.ID, strconv.FormatInt(profile.Revision, 10), profile.AuthorityDigest, strconv.FormatInt(routeRevision, 10), strategy, row.ID, strconv.FormatInt(attempt, 10)),
		JobID: row.ID, JobAttemptRevision: attempt,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: routeRevision, RouteClass: "institutional", IdentifierStrategy: strategy,
		PreRouteSafetyKey: materializationMAC(key, "pre_route_safety", profile.ID, row.ID, strconv.FormatInt(attempt, 10)),
		// The safety domain is a PROVIDER fence, so it must be shared by every
		// job that will reach the same provider; the scheduler's sibling
		// anti-join is the only thing serializing irreversible effects across
		// jobs, and it compares this value. Mixing row.ID in gave every job a
		// private domain, so that anti-join could never match a sibling and
		// cross-job serialization silently did nothing. Pre-route, the honest
		// shared axis is the institution profile: all of its traffic leaves
		// through the same resolver and proxy. A landed provider domain, once
		// there is one, is the stronger key and belongs here instead.
		SafetyDomainID:   materializationMAC(key, "safety_domain", profile.ID),
		AdapterRevision:  "packaged:institutional_materialization/1",
		EffectContractID: "browser_tab:institutional_materialization/1", Status: "eligible",
	}
	candidate, createErr := b.jobs.CreateBrowserCandidate(ctx, input)
	if createErr != nil {
		if existing, getErr := b.jobs.CurrentBrowserCandidateForJob(ctx, row.ID, attempt); getErr == nil && existing != nil {
			return existing, nil
		}
	}
	return candidate, createErr
}

// currentMaterializationEligibility is the shared fence for every
// browser-tab materialization effect. A candidate may remain in the durable
// store after its job is cancelled, its handoff action is resolved, or an
// explicit retry starts a new attempt; none of those late frames may mutate
// the old attempt.
func (b *Bridge) currentMaterializationEligibility(ctx context.Context, candidate *job.BrowserCandidate) (*job.Row, *job.HumanAction, bool, error) {
	if candidate == nil || candidate.JobID == "" {
		return nil, nil, false, nil
	}
	row, err := b.jobs.Get(ctx, candidate.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if row == nil || row.State != job.StateAwaitingHuman || job.Terminal(row.State) {
		return row, nil, false, nil
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, candidate.JobID)
	if err != nil {
		return row, nil, false, err
	}
	if attempt != candidate.JobAttemptRevision {
		return row, nil, false, nil
	}
	action, err := b.openHandoffForJob(ctx, candidate.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil, false, nil
	}
	if err != nil {
		return row, nil, false, err
	}
	return row, action, action != nil, nil
}

func (b *Bridge) clearMaterializationOffer(jobID string) {
	if jobID == "" {
		return
	}
	delete(b.materializationOffered, jobID)
	delete(b.focusPending, jobID)
}
func (b *Bridge) clearMaterializationTracking(jobID string) {
	b.clearMaterializationOffer(jobID)
	delete(b.materializationTracked, jobID)
}

func (b *Bridge) institutionalMaterializationDisabled(msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	detail := "institutional materialization is not negotiated by the current holder"
	var responseType string
	var payload any
	switch msg.Type {
	case protocol.MsgInstitutionalClaimRequest:
		p := msg.Payload.(*protocol.InstitutionalClaimRequestPayload)
		responseType, payload = protocol.MsgInstitutionalClaimResponse, protocol.InstitutionalClaimResponsePayload{RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail}
	case protocol.MsgInstitutionalBindRequest:
		p := msg.Payload.(*protocol.InstitutionalBindRequestPayload)
		responseType, payload = protocol.MsgInstitutionalBindResponse, protocol.InstitutionalBindResponsePayload{RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail}
	case protocol.MsgInstitutionalRouteRequest:
		p := msg.Payload.(*protocol.InstitutionalRouteRequestPayload)
		responseType, payload = protocol.MsgInstitutionalRouteResponse, protocol.InstitutionalRouteResponsePayload{RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail}
	case protocol.MsgInstitutionalNavigatedRequest:
		p := msg.Payload.(*protocol.InstitutionalNavigatedRequestPayload)
		responseType, payload = protocol.MsgInstitutionalNavigatedResponse, protocol.InstitutionalNavigatedResponsePayload{RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail}
	case protocol.MsgInstitutionalReconcileRequest:
		p := msg.Payload.(*protocol.InstitutionalReconcileRequestPayload)
		responseType, payload = protocol.MsgInstitutionalReconcileResponse, protocol.InstitutionalReconcileResponsePayload{RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail}
	default:
		return nil, nil
	}
	frame, err := b.frame(responseType, msg.JobID, payload)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func liveMaterializationClaim(c *job.MaterializationClaim, now time.Time) bool {
	if c == nil || c.BrowserHolderGeneration < 0 {
		return false
	}
	switch c.Phase {
	case "claimed", "bound", "route_issued", "navigated":
	default:
		return false
	}
	if c.LeaseUntil == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339Nano, c.LeaseUntil)
	return err == nil && expires.After(now)
}

// logMaterializationRefusal records a materialization frame the daemon did not
// grant. Every refusal in this pipeline is otherwise invisible from the daemon
// side: the extension either retries locally with backoff or stalls, so a paper
// that never reaches a surface leaves no trace at all beyond a claim row that
// sits at its birth phase. Same gap the claim-observation ack had, and the same
// remedy - out of band in the log, never widened into a wire result.
func logMaterializationRefusal(frameType, jobID, outcome, detail string) {
	log.Printf("papio: materialization %s for %s not granted: %s (%s)",
		frameType, jobID, outcome, detail)
}

func (b *Bridge) materializationClaimResponse(jobID string, p protocol.InstitutionalClaimResponsePayload) ([]json.RawMessage, error) {
	if p.Outcome != "claimed" {
		logMaterializationRefusal(protocol.MsgInstitutionalClaimResponse, jobID, p.Outcome, p.Detail)
	}
	frame, err := b.frame(protocol.MsgInstitutionalClaimResponse, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) institutionalClaim(ctx context.Context, jobID string, p *protocol.InstitutionalClaimRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	response := func(outcome, detail string) ([]json.RawMessage, error) {
		return b.materializationClaimResponse(jobID, protocol.InstitutionalClaimResponsePayload{
			RequestID: requestID, Outcome: outcome, Detail: detail,
		})
	}
	if p == nil || p.MaterializationKind != "browser_tab" {
		return response("not_eligible", "only browser-tab materialization is supported")
	}
	candidate, err := b.jobs.GetBrowserCandidate(ctx, p.CandidateID)
	if err != nil || candidate == nil || candidate.JobID != jobID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return response("error", "candidate state is unavailable")
		}
		return response("stale", "candidate is not current")
	}
	eligible, _, eligibleNow, eligibilityErr := b.currentMaterializationEligibility(ctx, candidate)
	if eligibilityErr != nil {
		return response("error", "handoff state is unavailable")
	}
	if !eligibleNow || eligible == nil || eligible.ID != jobID {
		return response("not_eligible", "the handoff is no longer open")
	}
	if candidate.Status != "eligible" && candidate.Status != "claimed" && candidate.Status != "materializing" {
		return response("stale", "candidate is no longer eligible")
	}
	if offer, offered := b.materializationOffered[jobID]; offered &&
		!offer.ExpiresAt.IsZero() && !offer.ExpiresAt.After(b.now()) {
		b.clearMaterializationOffer(jobID)
		if candidate.Status != "claimed" && candidate.Status != "materializing" {
			delete(b.materializationTracked, jobID)
		}
		return response("stale", "candidate offer has expired")
	}
	leaseUntil := b.now().Add(b.actionExpiry())
	claim, err := b.jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: b.epoch,
		JobAttemptRevision:         candidate.JobAttemptRevision,
		InstitutionProfileRevision: candidate.InstitutionProfileRevision,
		RouteRevision:              candidate.RouteRevision, MaterializationKind: p.MaterializationKind,
		LeaseUntil: leaseUntil,
	})
	if err != nil {
		outcome := "error"
		switch {
		case errors.Is(err, job.ErrMaterializationStale):
			outcome = "stale"
		case errors.Is(err, job.ErrMaterializationBusy), errors.Is(err, job.ErrMaterializationConflict):
			outcome = "busy"
			// "busy" means try again shortly, and the extension believes it:
			// it keeps the correlation and re-drives its bounded claim ladder
			// on every keepalive tick. When the conflict is the candidate's own
			// finished attempt that is never going to clear, so the honest
			// answer is that the candidate is stale - the outcome the extension
			// answers by dropping the workflow. Measured live 2026-08-20: three
			// papers burning a four-attempt ladder every ~60s indefinitely.
			if spent, spentErr := b.jobs.SpentMaterializationCandidate(ctx, jobID); spentErr != nil {
				log.Printf("papio: reading spent materialization attempt for %s: %v", jobID, spentErr)
			} else if spent {
				return response("stale", "this attempt is finished; ask again to start another")
			}
		}
		return response(outcome, "candidate claim was not accepted")
	}
	generation := claim.BrowserHolderGeneration
	return b.materializationClaimResponse(jobID, protocol.InstitutionalClaimResponsePayload{
		RequestID: requestID, Outcome: "claimed", CandidateID: candidate.ID, ClaimID: claim.ID, BindingID: claim.BindingID,
		BrowserHolderGeneration: &generation, LeaseUntil: claim.LeaseUntil,
	})
}

func (b *Bridge) institutionalBind(ctx context.Context, jobID string, p *protocol.InstitutionalBindRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	result := protocol.InstitutionalBindResponsePayload{RequestID: requestID}
	if p == nil {
		result.Outcome, result.Detail = "stale", "binding request is missing"
		return b.frameInstitutionalBind(jobID, result)
	}
	claim, err := b.jobs.GetMaterializationClaim(ctx, p.ClaimID)
	if err != nil || claim == nil {
		result.Outcome, result.Detail = "stale", "claim is not current"
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			result.Outcome, result.Detail = "error", "claim state is unavailable"
		}
		return b.frameInstitutionalBind(jobID, result)
	}
	candidate, candidateErr := b.jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if candidateErr != nil {
		result.Outcome, result.Detail = "error", "candidate state is unavailable"
		return b.frameInstitutionalBind(jobID, result)
	}
	if candidate == nil || candidate.JobID != jobID || p.TabID < 0 ||
		!liveMaterializationClaim(claim, b.now()) ||
		claim.BrowserHolderGeneration != b.epoch || claim.MaterializationKind != "browser_tab" {
		result.Outcome, result.Detail = "stale", "claim is fenced to another holder"
		return b.frameInstitutionalBind(jobID, result)
	}
	_, _, eligibleNow, eligibilityErr := b.currentMaterializationEligibility(ctx, candidate)
	if eligibilityErr != nil {
		result.Outcome, result.Detail = "error", "handoff state is unavailable"
		return b.frameInstitutionalBind(jobID, result)
	}
	if !eligibleNow {
		result.Outcome, result.Detail = "not_eligible", "the handoff is no longer open"
		return b.frameInstitutionalBind(jobID, result)
	}
	// The candidate's institution profile decides whether this bind also
	// owes the authentication-entry lease its owner-binding side channel
	// (claim-observation-protocol.md §4.1). Resolved before the bind itself
	// so a lookup failure fails closed instead of leaving a bound scaffold
	// whose lease ownership was never even attempted.
	authenticationClaimID := ""
	if profile, profileErr := b.jobs.GetInstitutionProfile(ctx, candidate.InstitutionProfileID); profileErr != nil {
		result.Outcome, result.Detail = "error", "institution profile state is unavailable"
		return b.frameInstitutionalBind(jobID, result)
	} else if profile != nil {
		authenticationClaimID = profile.AuthenticationClaimID
	}
	// The bind records this job as the entry's owner-binding (§4.1) and fails
	// closed when that write does not fence-match, so a paper that never
	// reserved the entry can never bind. That is reachable and was live: the
	// daemon-orchestrated pipeline (candidate offer -> claim -> scaffold ->
	// bind) has no consult in it, so any paper whose institution carried
	// another job's lease row hammered bind every ~2s forever, minting and
	// removing a scaffold each pass. Acquire the slot here through the same
	// arbitration the consult uses - it is the single gate, so one sign-in per
	// institution still holds - and refuse quietly when a live owner holds it.
	if authenticationClaimID != "" {
		leaseID := evidenceObservationID("authentication_claim_lease", authenticationClaimID, jobID, strconv.FormatInt(b.epoch, 10))
		if _, reserveErr := b.jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
			AuthenticationClaimID: authenticationClaimID, LeaseID: leaseID, OwnerID: jobID,
			BrowserHolderGeneration: b.epoch, LeaseUntil: b.now().Add(b.actionExpiry()),
		}); reserveErr != nil {
			if !errors.Is(reserveErr, job.ErrAuthenticationEntryLeaseBusy) {
				result.Outcome, result.Detail = "error", "authentication entry lease is unavailable"
				return b.frameInstitutionalBind(jobID, result)
			}
			// not_eligible is the outcome the extension answers by retiring
			// the scaffold and clearing the workflow, which is exactly right
			// here: no surface, no retry storm, and the scheduler re-offers
			// this candidate once the institution is free.
			result.Outcome, result.Detail = "not_eligible", "another sign-in for this institution is in progress"
			return b.frameInstitutionalBind(jobID, result)
		}
	}
	err = b.jobs.BindMaterializationWithLeaseOwner(ctx, claim.ID, p.BindingID, b.epoch,
		candidate.InstitutionProfileRevision, p.TabID, authenticationClaimID, jobID)
	if err != nil {
		// Every other refusal here names itself; this one answered a bare
		// "stale" with no detail, which is what made a live stall
		// undiagnosable from the daemon side. The lease-owner side channel
		// (claim-observation-protocol.md §4.1) is the likely half: the bind
		// records this job as the institution entry's owner-binding and
		// fails closed when that write is a fenced no-op, which is exactly
		// what happens when the job never reserved the entry in the first
		// place.
		result.Outcome, result.Detail = "stale",
			"the claim fence or the institution's sign-in slot was lost"
		if !errors.Is(err, job.ErrMaterializationStale) && !errors.Is(err, job.ErrMaterializationConflict) {
			result.Outcome, result.Detail = "error", "binding could not be recorded"
		}
		return b.frameInstitutionalBind(jobID, result)
	}
	result.Outcome = "bound"
	result.ClaimID, result.BindingID = claim.ID, p.BindingID
	return b.frameInstitutionalBind(jobID, result)
}

func (b *Bridge) frameInstitutionalBind(jobID string, p protocol.InstitutionalBindResponsePayload) ([]json.RawMessage, error) {
	if p.Outcome != "bound" {
		logMaterializationRefusal(protocol.MsgInstitutionalBindResponse, jobID, p.Outcome, p.Detail)
	}
	frame, err := b.frame(protocol.MsgInstitutionalBindResponse, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}
func (b *Bridge) institutionalRoute(ctx context.Context, jobID string, p *protocol.InstitutionalRouteRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	result := protocol.InstitutionalRouteResponsePayload{RequestID: requestID}
	if p == nil {
		result.Outcome, result.Detail = "stale", "route request is missing"
		return b.frameInstitutionalRoute(jobID, result)
	}
	claim, err := b.jobs.GetMaterializationClaim(ctx, p.ClaimID)
	if err != nil || claim == nil {
		result.Outcome, result.Detail = "stale", "claim is not current"
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			result.Outcome, result.Detail = "error", "claim state is unavailable"
		}
		return b.frameInstitutionalRoute(jobID, result)
	}
	if claim.BindingID != p.BindingID || claim.BrowserHolderGeneration != b.epoch ||
		claim.MaterializationKind != "browser_tab" ||
		(claim.Phase != "bound" && claim.Phase != "route_issued" && claim.Phase != "navigated") {
		result.Outcome, result.Detail = "stale", "claim is fenced to another holder"
		return b.frameInstitutionalRoute(jobID, result)
	}
	candidate, candidateErr := b.jobs.GetBrowserCandidate(ctx, claim.CandidateID)
	if candidateErr != nil {
		result.Outcome, result.Detail = "error", "candidate state is unavailable"
		return b.frameInstitutionalRoute(jobID, result)
	}
	if candidate == nil || candidate.JobID != jobID {
		result.Outcome, result.Detail = "stale", "candidate is not current"
		return b.frameInstitutionalRoute(jobID, result)
	}
	profile, profileErr := b.jobs.GetInstitutionProfile(ctx, candidate.InstitutionProfileID)
	if profileErr != nil {
		result.Outcome, result.Detail = "error", "institution profile state is unavailable"
		return b.frameInstitutionalRoute(jobID, result)
	}
	if profile == nil || profile.TombstonedAt != "" || profile.Revision != candidate.InstitutionProfileRevision {
		result.Outcome, result.Detail = "stale", "institution profile fence was lost"
		return b.frameInstitutionalRoute(jobID, result)
	}
	row, action, eligibleNow, eligibilityErr := b.currentMaterializationEligibility(ctx, candidate)
	if eligibilityErr != nil {
		result.Outcome, result.Detail = "error", "handoff state is unavailable"
		return b.frameInstitutionalRoute(jobID, result)
	}
	if !eligibleNow || row == nil || action == nil {
		result.Outcome, result.Detail = "not_eligible", "the handoff is no longer open"
		return b.frameInstitutionalRoute(jobID, result)
	}
	// Resolve the route before acquiring, but never return it unless the
	// daemon-durable permit transaction commits.
	routeURL, ok := app.ResolveHumanActionURL(*action, *row, b.cfg.InstitutionFor)
	if !ok || routeURL == "" {
		result.Outcome, result.Detail = "not_eligible", "no current institutional route is available"
		return b.frameInstitutionalRoute(jobID, result)
	}
	leaseUntil := b.now().Add(b.actionExpiry())
	if claim.LeaseUntil != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, claim.LeaseUntil)
		if parseErr != nil {
			result.Outcome, result.Detail = "error", "claim lease is invalid"
			return b.frameInstitutionalRoute(jobID, result)
		}
		leaseUntil = parsed
	}
	if !b.effectPermitAvailable() {
		result.Outcome, result.Detail = "feature_disabled", "durable effect permits are unavailable"
		return b.frameInstitutionalRoute(jobID, result)
	}
	permit, acquireOutcome, acquireErr := b.jobs.AcquireInstitutionalEffectPermit(ctx, job.InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: p.BindingID,
		SafetyDomainID:          candidate.SafetyDomainID,
		InstitutionalRequestID:  p.InstitutionalRequestID,
		JobAttemptRevision:      candidate.JobAttemptRevision,
		BrowserHolderGeneration: b.epoch,
		ExpectedEffectOrdinal:   p.ExpectedEffectOrdinal,
		LeaseUntil:              leaseUntil,
		Authorization: job.EffectPermitEvent{Kind: "browser.institutional_effect_authorized", Detail: map[string]any{
			"claim_id": claim.ID, "binding_id": p.BindingID,
			"institutional_request_id": p.InstitutionalRequestID,
			"expected_effect_ordinal":  p.ExpectedEffectOrdinal,
			"safety_domain":            candidate.SafetyDomainID,
		}},
	})
	if acquireErr != nil {
		switch {
		case errors.Is(acquireErr, job.ErrEffectPermitBusy):
			result.Outcome, result.Detail = "busy", "the effect lane is occupied"
		case errors.Is(acquireErr, job.ErrEffectPermitStale):
			result.Outcome, result.Detail = "stale", "the effect authorization fence was lost"
		default:
			result.Outcome, result.Detail = "error", "effect authorization is unavailable"
		}
		return b.frameInstitutionalRoute(jobID, result)
	}
	if permit == nil || permit.EffectOrdinal == nil {
		result.Outcome, result.Detail = "error", "effect authorization is incomplete"
		return b.frameInstitutionalRoute(jobID, result)
	}
	if acquireOutcome == job.EffectPermitDuplicate && permit.Status != job.EffectPermitHeld {
		result.Outcome, result.Detail = "stale", "the institutional effect was already resolved"
		return b.frameInstitutionalRoute(jobID, result)
	}
	issuedClaim, claimErr := b.jobs.GetMaterializationClaim(ctx, claim.ID)
	if claimErr != nil || issuedClaim == nil || issuedClaim.RouteIssuanceOrdinal < 1 ||
		issuedClaim.BindingID != p.BindingID || issuedClaim.EffectOrdinal != *permit.EffectOrdinal {
		result.Outcome, result.Detail = "error", "atomic route authorization is incomplete"
		return b.frameInstitutionalRoute(jobID, result)
	}
	ordinal := issuedClaim.RouteIssuanceOrdinal
	result.Outcome, result.RouteIssuanceOrdinal, result.EffectOrdinal, result.URL =
		"issued", ordinal, *permit.EffectOrdinal, routeURL
	result.ClaimID, result.BindingID = claim.ID, p.BindingID
	result.InstitutionalRequestID = p.InstitutionalRequestID
	return b.frameInstitutionalRoute(jobID, result)
}

func (b *Bridge) frameInstitutionalRoute(jobID string, p protocol.InstitutionalRouteResponsePayload) ([]json.RawMessage, error) {
	if p.Outcome != "issued" {
		logMaterializationRefusal(protocol.MsgInstitutionalRouteResponse, jobID, p.Outcome, p.Detail)
	}
	frame, err := b.frame(protocol.MsgInstitutionalRouteResponse, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) institutionalNavigated(ctx context.Context, jobID string, p *protocol.InstitutionalNavigatedRequestPayload) ([]json.RawMessage, error) {
	return b.institutionalNavigatedForSession(ctx, jobID, p, true)
}

func (b *Bridge) institutionalNavigatedForSession(ctx context.Context, jobID string, p *protocol.InstitutionalNavigatedRequestPayload, currentHolder bool) ([]json.RawMessage, error) {
	requestID := ""
	result := protocol.InstitutionalNavigatedResponsePayload{}
	if p != nil {
		requestID = p.RequestID
	}
	result.RequestID = requestID
	if p != nil && p.EffectOrdinal == 0 && p.InstitutionalRequestID == "" &&
		p.ClaimID != "" && p.BindingID != "" && p.RouteIssuanceOrdinal >= 1 && p.TabID >= 0 {
		legacyErr := b.jobs.SettleLegacyInstitutionalNavigation(ctx, jobID, p.ClaimID, p.BindingID, p.RouteIssuanceOrdinal)
		switch {
		case legacyErr == nil:
			result.Outcome, result.Detail = "stale", "navigation was settled as historical cleanup"
		case errors.Is(legacyErr, job.ErrEffectPermitStale):
			result.Outcome, result.Detail = "stale", "institutional effect does not match an occupying permit"
		default:
			result.Outcome, result.Detail = "error", "legacy institutional effect could not be recorded"
		}
		return b.frameInstitutionalNavigated(jobID, result)
	}
	if p == nil || p.ClaimID == "" || p.BindingID == "" ||
		p.InstitutionalRequestID == "" || p.EffectOrdinal < 1 ||
		p.RouteIssuanceOrdinal < 1 || p.TabID < 0 {
		result.Outcome, result.Detail = "stale", "navigation request is missing"
		return b.frameInstitutionalNavigated(jobID, result)
	}

	// The exact request identity is the settlement fence. Do not pre-read the
	// claim and reject here: the claim may have been superseded after the
	// browser navigation, but the historical permit still owns the cleanup
	// result. Current claim acknowledgement is performed by the permit
	// transaction only when all current fences still match.
	identity := job.EffectPermitIdentity{
		JobID: jobID, Kind: job.EffectKindInstitutional,
		ClaimID: p.ClaimID, BindingID: p.BindingID,
		EffectOrdinal: p.EffectOrdinal, InstitutionalRequestID: p.InstitutionalRequestID,
	}
	permit, lookupErr := b.jobs.GetEffectPermitByIdentity(ctx, identity)
	if lookupErr != nil {
		result.Outcome, result.Detail = "error", "institutional effect state is unavailable"
		return b.frameInstitutionalNavigated(jobID, result)
	}
	if permit == nil {
		// Historical pre-permit navigation is cleanup-only.  The exact
		// blocker tuple is the sole durable mutation; do not acknowledge or
		// otherwise touch the current claim/job generation.
		legacyErr := b.jobs.SettleLegacyEffectBlocker(ctx, job.LegacyEffectBlockerInput{
			Kind:          job.EffectKindInstitutional,
			JobID:         jobID,
			ClaimID:       p.ClaimID,
			BindingID:     p.BindingID,
			EffectOrdinal: p.EffectOrdinal,
		})
		switch {
		case legacyErr == nil:
			result.Outcome, result.Detail = "stale", "navigation was settled as historical cleanup"
		case errors.Is(legacyErr, job.ErrEffectPermitStale):
			result.Outcome, result.Detail = "stale", "institutional effect does not match an occupying permit"
		default:
			result.Outcome, result.Detail = "error", "legacy institutional effect could not be recorded"
		}
		return b.frameInstitutionalNavigated(jobID, result)
	}
	currentHolderGeneration := b.epoch
	if !currentHolder {
		// A negotiated non-holder may settle its exact historical navigation,
		// but it can never pass the current-holder projection fence.
		currentHolderGeneration = -1
	}
	permit, _, settleErr := b.jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
		Identity: identity,
		RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
			"claim_id": p.ClaimID, "binding_id": p.BindingID,
			"effect_ordinal": p.EffectOrdinal, "institutional_request_id": p.InstitutionalRequestID,
			"route_issuance_ordinal": p.RouteIssuanceOrdinal, "tab_id": p.TabID,
			"outcome": "acknowledged",
		}}},
		CurrentBrowserHolderGeneration: currentHolderGeneration,
		Navigation: &job.EffectPermitNavigationFence{
			ClaimID: p.ClaimID, BindingID: p.BindingID,
			RouteIssuanceOrdinal: p.RouteIssuanceOrdinal, TabID: p.TabID,
		},
	})
	if settleErr != nil {
		result.Outcome, result.Detail = "stale", "institutional effect does not match an occupying permit"
		if !errors.Is(settleErr, job.ErrEffectPermitStale) {
			result.Outcome, result.Detail = "error", "institutional effect settlement is unavailable"
		}
		return b.frameInstitutionalNavigated(jobID, result)
	}
	if permit != nil && permit.OperatorOverridden {
		result.Outcome, result.Detail = "overridden", "operator resolved the institutional effect"
		return b.frameInstitutionalNavigated(jobID, result)
	}
	if permit == nil || !permit.CurrentAtSettlement {
		result.Outcome, result.Detail = "stale", "navigation was settled as historical cleanup"
		return b.frameInstitutionalNavigated(jobID, result)
	}
	result.Outcome, result.Detail = "acknowledged", ""
	result.ClaimID, result.BindingID = p.ClaimID, p.BindingID
	return b.frameInstitutionalNavigated(jobID, result)
}

func (b *Bridge) frameInstitutionalNavigated(jobID string, p protocol.InstitutionalNavigatedResponsePayload) ([]json.RawMessage, error) {
	if p.Outcome != "acknowledged" {
		logMaterializationRefusal(protocol.MsgInstitutionalNavigatedResponse, jobID, p.Outcome, p.Detail)
	}
	frame, err := b.frame(protocol.MsgInstitutionalNavigatedResponse, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) institutionalReconcile(ctx context.Context, p *protocol.InstitutionalReconcileRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	result := protocol.InstitutionalReconcileResponsePayload{RequestID: requestID, Outcome: "reconciled"}
	fail := func(detail string) {
		result.Outcome = "error"
		result.Detail = detail
		result.Claims = nil
	}
	if p == nil {
		fail("reconcile request is missing")
	} else {
		for _, binding := range p.Bindings {
			if binding.BindingID == "" || binding.TabID < 0 {
				continue
			}
			claim, err := b.jobs.MaterializationClaimByBindingID(ctx, binding.BindingID)
			if err != nil {
				fail("reconcile claim state is unavailable")
				break
			}
			if claim == nil {
				continue
			}
			if !liveMaterializationClaim(claim, b.now()) ||
				claim.BrowserHolderGeneration != b.epoch || claim.MaterializationKind != "browser_tab" {
				continue
			}
			candidate, err := b.jobs.GetBrowserCandidate(ctx, claim.CandidateID)
			if err != nil {
				fail("reconcile candidate state is unavailable")
				break
			}
			if candidate == nil {
				continue
			}
			_, _, eligibleNow, eligibilityErr := b.currentMaterializationEligibility(ctx, candidate)
			if eligibilityErr != nil {
				fail("reconcile handoff state is unavailable")
				break
			}
			if !eligibleNow {
				continue
			}
			// The reported tab must be the one the claim is durably bound to.
			// Echoing the extension's number back would let a reconcile assert
			// any tab as this materialization's tab, which is the one fact the
			// daemon is supposed to be the authority on.
			//
			// A claim is inserted with tab_id 0 in phase "claimed" and only
			// gains a real tab when BindMaterialization lands, while the
			// extension opens the scaffold tab BEFORE it sends the bind. So
			// "claimed with a live tab the daemon has not recorded" is the
			// normal transient state, and it is exactly what reconcile exists
			// to recover after a worker death. Rejecting it would omit the
			// claim, and the extension treats an omitted binding as dead: it
			// closes the tab and clears the workflow while the durable claim
			// stays live to its lease, blocking the candidate. Confirm such a
			// claim without asserting a tab, and enforce the match only once
			// the daemon actually has one.
			// Phase, not the tab value, says whether a tab is bound: tab 0 is
			// a legitimate bound tab here, so it cannot double as "unbound".
			var tabID *int64
			if claim.Phase != "claimed" {
				if claim.TabID != binding.TabID {
					continue
				}
				bound := claim.TabID
				tabID = &bound
			}
			result.Claims = append(result.Claims, protocol.InstitutionalReconcileClaim{
				ClaimID: claim.ID, BindingID: claim.BindingID, CandidateID: claim.CandidateID,
				Phase: claim.Phase, TabID: tabID,
			})

		}
	}
	frame, err := b.frame(protocol.MsgInstitutionalReconcileResponse, "", result)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// surfaceClose answers surface_close_request (surface_close_v1,
// dev/active/claim-observation-protocol.md §2.3). Generic, not job-scoped:
// a scaffold being closed may have no live job. A binding_id with no
// materialization_claims row at all is never daemon-closable — that covers
// both an unknown binding and an extension-minted pre-cutover scaffold,
// which the plan's narrowed migration promise deliberately leaves to a
// bounded operator review rather than daemon inference.
func (b *Bridge) surfaceClose(ctx context.Context, p *protocol.SurfaceCloseRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	result := protocol.SurfaceCloseResponsePayload{RequestID: requestID}
	frame := func() ([]json.RawMessage, error) {
		f, err := b.frame(protocol.MsgSurfaceCloseResponse, "", result)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{f}, nil
	}
	if p == nil {
		result.Outcome, result.Detail = "error", "surface close request is missing"
		return frame()
	}
	claim, err := b.jobs.MaterializationClaimByBindingID(ctx, p.BindingID)
	if err != nil {
		result.Outcome, result.Detail = "error", "binding state is unavailable"
		return frame()
	}
	if claim == nil {
		result.Outcome, result.Detail = "not_eligible", "binding has no live materialization claim"
		return frame()
	}
	if p.BrowserHolderGeneration != b.epoch {
		result.Outcome, result.Detail = "stale", "browser holder generation is not current"
		return frame()
	}
	eligible := false
	switch p.Disposition {
	case "materialization_settled":
		eligible = claim.Phase == "settled"
	case "claim_abandoned":
		eligible = claim.Phase == "abandoned"
	case "scaffold_idle":
		if claim.Phase == "claimed" || claim.Phase == "bound" {
			permit, permitErr := b.jobs.LiveEffectPermit(ctx)
			if permitErr != nil {
				result.Outcome, result.Detail = "error", "effect permit state is unavailable"
				return frame()
			}
			eligible = permit == nil || permit.ClaimID != claim.ID
		}
	case "job_inactive":
		// A navigated surface is not closable merely because it looks old.
		// The daemon must prove the browser handoff itself is no longer live:
		// terminal job, or no open openurl_handoff action after the same direct
		// lookup poll() uses before emitting cancel. The binding remains the
		// resource identity, and an unsettled effect for this exact claim still
		// vetoes closure.
		candidate, candidateErr := b.jobs.GetBrowserCandidate(ctx, claim.CandidateID)
		if candidateErr != nil || candidate == nil {
			result.Outcome, result.Detail = "error", "binding candidate state is unavailable"
			return frame()
		}
		row, jobErr := b.jobs.Get(ctx, candidate.JobID)
		if jobErr != nil || row == nil {
			result.Outcome, result.Detail = "error", "binding job state is unavailable"
			return frame()
		}
		inactive := job.Terminal(row.State)
		if !inactive {
			_, actionErr := b.openHandoffForJob(ctx, row.ID)
			switch {
			case errors.Is(actionErr, sql.ErrNoRows):
				inactive = true
			case actionErr != nil:
				result.Outcome, result.Detail = "error", "browser handoff state is unavailable"
				return frame()
			}
		}
		if inactive {
			permit, permitErr := b.jobs.LiveEffectPermit(ctx)
			if permitErr != nil {
				result.Outcome, result.Detail = "error", "effect permit state is unavailable"
				return frame()
			}
			eligible = permit == nil || permit.ClaimID != claim.ID
			if !eligible {
				result.Detail = "the binding's provider effect is not settled"
			}
		} else {
			result.Detail = "the binding still has an active browser handoff"
		}
	}
	if !eligible {
		result.Outcome = "not_eligible"
		if result.Detail == "" {
			result.Detail = "disposition does not match the binding's current phase"
		}
		return frame()
	}
	id, nonce, issueErr := b.jobs.IssueCloseAuthorization(ctx, p.BindingID, p.BrowserHolderGeneration, p.Disposition, b.now())
	if issueErr != nil {
		if errors.Is(issueErr, job.ErrCloseAuthorizationConflict) {
			result.Outcome, result.Detail = "busy", "a live close authorization already exists for this binding"
		} else {
			result.Outcome, result.Detail = "error", "close authorization could not be recorded"
		}
		return frame()
	}
	generation := p.BrowserHolderGeneration
	result.Outcome, result.CloseAuthorizationID, result.Nonce, result.BrowserHolderGeneration =
		"authorized", id, nonce, &generation
	return frame()
}

// institutionalAuthenticationClaimDisabled answers the two claim-observation
// message types when the negotiating session has not advertised
// institutional_authentication_claim_v1. claim_observation_ack's closed
// outcome vocabulary (§2.2) has no "feature_disabled" member, so an
// unnegotiated observation is acked "rejected" instead — an ordinary,
// expected failure per this file's structured-outcome rule, never a raw
// error or a disconnect.
func (b *Bridge) institutionalAuthenticationClaimDisabled(msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	detail := "institutional authentication claim is not negotiated by the current holder"
	switch msg.Type {
	case protocol.MsgAuthenticationClaimRequest:
		p := msg.Payload.(*protocol.AuthenticationClaimRequestPayload)
		frame, err := b.frame(protocol.MsgAuthenticationClaimResponse, msg.JobID, protocol.AuthenticationClaimResponsePayload{
			RequestID: p.RequestID, Outcome: "feature_disabled", Detail: detail,
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{frame}, nil
	case protocol.MsgClaimObservation:
		p := msg.Payload.(*protocol.ClaimObservationPayload)
		frame, err := b.frame(protocol.MsgClaimObservationAck, msg.JobID, protocol.ClaimObservationAckPayload{
			RequestID: p.RequestID, Outcome: "rejected", Detail: detail,
			GateOccurrenceID: p.GateOccurrenceID, BrowserHolderGeneration: b.epoch,
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{frame}, nil
	}
	return nil, nil
}

// authenticationClaimCurrentGateOccurrence reads the current human_gate.login
// occurrence scoped to one authentication claim, whatever its status
// (open or resolved) — the read-only half claim_observation's ack needs to
// always echo "the daemon's current occurrence id" (§2.2).
func (b *Bridge) authenticationClaimCurrentGateOccurrence(ctx context.Context, claimID string) (job.HumanGateObservation, bool, error) {
	rows, err := b.jobs.CurrentHumanGateObservations(ctx, string(job.HumanGateScopeAuthenticationClaim), claimID)
	if err != nil {
		return job.HumanGateObservation{}, false, err
	}
	for _, row := range rows {
		if row.GateType == job.HumanGateLogin {
			return row, true, nil
		}
	}
	return job.HumanGateObservation{}, false, nil
}

// ensureAuthenticationClaimGateOccurrence returns the id of the currently
// OPEN human_gate.login occurrence for claimID, minting one via the same
// occurrence-minting machinery institutional handlers already use
// (upsertProfileGate) whenever none is open — either because none exists
// yet (first grant) or because a prior cycle resolved it (a fresh grant
// reopens with a new occurrence id: "the gate id carries the frame's
// msg_id" rollover rule, ADR-0022 "Human gates are keyed to the
// occurrence"). Only the arbitration reducer (authenticationClaim) calls
// this; the observation reducer only ever reads the current occurrence.
func (b *Bridge) ensureAuthenticationClaimGateOccurrence(ctx context.Context, profile *job.InstitutionProfile, claimID, jobID, requestID string) (string, error) {
	if row, ok, err := b.authenticationClaimCurrentGateOccurrence(ctx, claimID); err != nil {
		return "", err
	} else if ok && row.Status == job.HumanGateOpen {
		return row.ID, nil
	}
	observationKey := evidenceObservationID("authentication_claim_grant", requestID, claimID)
	if err := b.upsertProfileGate(ctx, observationKey, profile.ConfiguredName, jobID, job.HumanGateLogin, job.HumanGateOpen,
		`{"source":"authentication_claim_request"}`); err != nil {
		return "", err
	}
	row, ok, err := b.authenticationClaimCurrentGateOccurrence(ctx, claimID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("authentication claim gate occurrence could not be established")
	}
	return row.ID, nil
}

func (b *Bridge) authenticationClaimResponse(jobID string, p protocol.AuthenticationClaimResponsePayload) ([]json.RawMessage, error) {
	if p.Outcome != "open_new" && p.Outcome != "navigate_existing" {
		logMaterializationRefusal(protocol.MsgAuthenticationClaimResponse, jobID, p.Outcome, p.Detail)
	}
	frame, err := b.frame(protocol.MsgAuthenticationClaimResponse, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// authenticationClaim answers authentication_claim_request: the §2.1.1
// arbitration reducer. authentication_claim_id is always resolved here from
// candidate_id (Decision 2), never accepted from the wire. The lease id is
// deterministic per (claim, owning job, holder generation) — mirroring the
// legacy reserveAuthenticationEntry's derivation — so a genuine retry from
// the same owner replays into the same reservation instead of racing a
// fresh one.
func (b *Bridge) authenticationClaim(ctx context.Context, jobID string, p *protocol.AuthenticationClaimRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	if p != nil {
		requestID = p.RequestID
	}
	response := func(outcome, detail string) ([]json.RawMessage, error) {
		return b.authenticationClaimResponse(jobID, protocol.AuthenticationClaimResponsePayload{
			RequestID: requestID, Outcome: outcome, Detail: detail,
		})
	}
	if p == nil {
		return response("error", "authentication claim request is missing")
	}
	candidate, err := b.jobs.GetBrowserCandidate(ctx, p.CandidateID)
	if err != nil || candidate == nil || candidate.JobID != jobID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return response("error", "candidate state is unavailable")
		}
		return response("not_eligible", "candidate is not current")
	}
	if candidate.Status != "eligible" && candidate.Status != "claimed" && candidate.Status != "materializing" {
		return response("not_eligible", "candidate is no longer eligible")
	}
	profile, err := b.jobs.GetInstitutionProfile(ctx, candidate.InstitutionProfileID)
	if err != nil {
		return response("error", "institution profile state is unavailable")
	}
	if profile == nil || profile.TombstonedAt != "" || profile.Revision != candidate.InstitutionProfileRevision || profile.AuthenticationClaimID == "" {
		return response("not_eligible", "institution profile authority was lost")
	}
	claimID := profile.AuthenticationClaimID
	leaseID := evidenceObservationID("authentication_claim_lease", claimID, jobID, strconv.FormatInt(b.epoch, 10))
	lease, reserveErr := b.jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
		AuthenticationClaimID: claimID, LeaseID: leaseID, OwnerID: jobID,
		BrowserHolderGeneration: b.epoch, LeaseUntil: b.now().Add(b.actionExpiry()),
	})
	generation := b.epoch
	if reserveErr != nil {
		if !errors.Is(reserveErr, job.ErrAuthenticationEntryLeaseBusy) {
			return response("error", "authentication entry lease is unavailable")
		}
		owner, ok, ownerErr := b.jobs.GetAuthenticationEntryLease(ctx, claimID)
		if ownerErr != nil || !ok {
			return response("error", "authentication entry lease state is unavailable")
		}
		occurrenceID, occErr := b.ensureAuthenticationClaimGateOccurrence(ctx, profile, claimID, jobID, requestID)
		if occErr != nil {
			return response("error", "gate occurrence state is unavailable")
		}
		// This job is not getting a surface of its own, so give back what it
		// took to ask. Claiming the candidate precedes this arbitration and
		// flips it to 'claimed' — the state the scheduler reads as "in
		// progress" — so a refusal used to strand an unconsumed claim (tab 0,
		// no route, no effect) until its lease expired: the paper could not
		// retry, the scheduler could not re-offer it when the entry freed, and
		// nothing said so. Only an unconsumed claim is released; a surface or an
		// irreversible provider effect keeps it.
		if released, releaseErr := b.jobs.ReleaseUnconsumedMaterializationClaim(ctx, p.CandidateID); releaseErr != nil {
			log.Printf("papio: unconsumed materialization claim for %s could not be released: %v", jobID, releaseErr)
		} else if released {
			b.materializationScheduleVersion++
		}
		// focus_owner requires a real surface to focus (owner_binding_id is
		// wire-required on it); a busy lease whose owner has not bound a
		// surface yet (a race between its own open_new grant and its
		// institutional_bind_request) has nothing to focus, so it parks
		// exactly like the automatic-trigger case regardless of trigger.
		if p.Trigger == "explicit" && owner.OwnerBindingID != "" {
			return b.authenticationClaimResponse(jobID, protocol.AuthenticationClaimResponsePayload{
				RequestID: requestID, Outcome: "focus_owner",
				AuthenticationClaimID: claimID, BrowserHolderGeneration: &generation,
				GateOccurrenceID: occurrenceID, LeaseUntil: owner.LeaseUntil,
				OwnerBindingID: owner.OwnerBindingID, OwnerTabHint: owner.OwnerTabHint,
			})
		}
		dependents, depErr := b.jobs.EligibleAuthenticationClaimDependents(ctx, claimID)
		if depErr != nil {
			return response("error", "dependent count is unavailable")
		}
		return b.authenticationClaimResponse(jobID, protocol.AuthenticationClaimResponsePayload{
			RequestID: requestID, Outcome: "park",
			AuthenticationClaimID: claimID, BrowserHolderGeneration: &generation,
			GateOccurrenceID: occurrenceID, DependentCount: &dependents,
		})
	}
	occurrenceID, occErr := b.ensureAuthenticationClaimGateOccurrence(ctx, profile, claimID, jobID, requestID)
	if occErr != nil {
		return response("error", "gate occurrence state is unavailable")
	}
	// A live owner_binding_id means a surface already exists to navigate;
	// its absence — whether this reservation is brand new, or a replay of
	// one that has not reached institutional_bind_response yet — means
	// there is nothing to navigate to, so the extension is granted
	// permission to open one exactly as a fresh grant would (§2.1.1 case
	// 2/3 collapse to the same wire outcome here).
	if lease.OwnerBindingID != "" {
		return b.authenticationClaimResponse(jobID, protocol.AuthenticationClaimResponsePayload{
			RequestID: requestID, Outcome: "navigate_existing",
			AuthenticationClaimID: claimID, BrowserHolderGeneration: &generation,
			GateOccurrenceID: occurrenceID, LeaseUntil: lease.LeaseUntil,
			OwnerBindingID: lease.OwnerBindingID, OwnerTabHint: lease.OwnerTabHint,
		})
	}
	return b.authenticationClaimResponse(jobID, protocol.AuthenticationClaimResponsePayload{
		RequestID: requestID, Outcome: "open_new",
		AuthenticationClaimID: claimID, BrowserHolderGeneration: &generation,
		GateOccurrenceID: occurrenceID, LeaseUntil: lease.LeaseUntil,
	})
}

func (b *Bridge) claimObservationAck(jobID string, p protocol.ClaimObservationAckPayload) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgClaimObservationAck, jobID, p)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// claimObservation answers claim_observation: the §2.2.1 observation
// reducer, fenced by §3's idempotency/ordering rule. gate_occurrence_id and
// browser_holder_generation are populated on every path, including the
// earliest possible returns, because the ack requires both unconditionally.
func (b *Bridge) claimObservation(ctx context.Context, jobID string, p *protocol.ClaimObservationPayload) ([]json.RawMessage, error) {
	generation := b.epoch
	if p == nil {
		return b.claimObservationAck(jobID, protocol.ClaimObservationAckPayload{
			Outcome: "error", Detail: "claim observation is missing",
			GateOccurrenceID: "claim-observation-missing-payload", BrowserHolderGeneration: generation,
		})
	}
	applied, err := b.jobs.ApplyClaimObservation(ctx, job.ApplyClaimObservationInput{
		JobID: jobID, AuthenticationClaimID: p.AuthenticationClaimID, BindingID: p.BindingID,
		ObservationID: p.ObservationID, GateOccurrenceID: p.GateOccurrenceID,
		EventKind: p.EventKind, EventOrdinal: p.EventOrdinal,
		FrameGeneration: p.BrowserHolderGeneration, Generation: generation,
		AuthReturnedEvidenceObservationID: evidenceObservationID("claim_observation_auth_returned", p.ObservationID),
		LeaseUntil:                        b.now().Add(b.actionExpiry()),
		Now:                               b.now(),
	})
	if err != nil {
		return b.claimObservationAck(jobID, protocol.ClaimObservationAckPayload{
			RequestID: p.RequestID, Outcome: "error", Detail: "claim observation could not be applied",
			GateOccurrenceID: p.GateOccurrenceID, BrowserHolderGeneration: generation,
		})
	}
	if applied.EntitledLanding {
		// Nudge the existing materialization scheduler
		// (Bridge.Sync -> ScheduleEligibleBrowserCandidates, already run
		// unconditionally every Sync) and the legacy federated-login
		// reoffer path (reofferInstitutionalSiblings) — both already
		// poll/reopen on exactly this signal. A dependent that already
		// carries a durable browser_candidates row (Slice 4,
		// dev/active/surface-lifecycle-plan.md) is picked up by
		// admitAutomaticMaterializationCandidates on the next poll purely
		// from the now-entitled lease state — no legacy reoffer needed for
		// it, and reofferInstitutionalSiblings is a harmless no-op for a
		// materialization-tracked job (poll's jobLoop skips tracked,
		// non-focused ids ahead of any legacy offer). A dependent with no
		// candidate at all has nothing to connect to yet and still needs
		// this call to resume. Run only after ApplyClaimObservation's own
		// transaction has committed: reofferInstitutionalSiblings opens its
		// own store transactions and would deadlock the single-writer
		// connection if nested inside that one.
		ownerJobID := jobID
		if claim, claimErr := b.jobs.MaterializationClaimByBindingID(ctx, p.BindingID); claimErr == nil && claim != nil {
			if candidate, candErr := b.jobs.GetBrowserCandidate(ctx, claim.CandidateID); candErr == nil && candidate != nil && candidate.JobID != "" {
				ownerJobID = candidate.JobID
			}
		}
		b.materializationScheduleVersion++
		if err := b.reofferInstitutionalSiblings(ctx, ownerJobID); err != nil {
			log.Printf("papio: claim observation entitled_landing reoffer unavailable: %v", err)
		}
	}
	// An observation the daemon declines is otherwise invisible on both sides:
	// the extension's outbox drain retires the entry on `rejected`/`stale`
	// (its own comment says a rejection "is still logged server-side", which
	// until now it was not), and nothing persists the ack outcome. So a
	// permanently refused login journal — measured live 2026-08-19, zero rows
	// across weeks of real sign-ins — looked exactly like a login nobody had
	// ever attempted. Log the disposition out of band rather than widening the
	// ack: the IPC result shape is fail-closed for older peers (AGENTS.md).
	if applied.Outcome != "applied" {
		log.Printf("papio: claim observation %s for %s not applied: %s (%s)",
			p.EventKind, jobID, applied.Outcome, applied.Detail)
	}
	return b.claimObservationAck(jobID, protocol.ClaimObservationAckPayload{
		RequestID: p.RequestID, Outcome: applied.Outcome, Detail: applied.Detail,
		GateOccurrenceID: applied.GateOccurrenceID, BrowserHolderGeneration: generation,
		LeaseUntil: applied.LeaseUntil,
	})
}

// handle dispatches one decoded inbound frame from sessionID.
func (b *Bridge) handle(ctx context.Context, sessionID string, msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	if msg.Type == protocol.MsgHello {
		return b.handleHello(sessionID, msg.Payload.(*protocol.HelloPayload))
	}
	session := b.sessionByID(sessionID)
	historicalEffectResult := msg.Type == protocol.MsgProviderDriveEpochResultRequest ||
		msg.Type == protocol.MsgProviderDirectGetResult ||
		msg.Type == protocol.MsgTermsEffectResultRequest ||
		msg.Type == protocol.MsgInstitutionalNavigatedRequest
	if historicalEffectResult && session == nil {
		return b.helloRequired()
	}
	legacyInstitutionalNavigation := msg.Type == protocol.MsgInstitutionalNavigatedRequest &&
		session != nil &&
		slices.Contains(session.Features, institutionalMaterializationFeature) &&
		!slices.Contains(session.Features, effectPermitFeature)
	if historicalEffectResult && (b.holder == nil || b.holder.ID != sessionID) &&
		!slices.Contains(session.Features, effectPermitFeature) && !legacyInstitutionalNavigation {
		// A non-holder may settle only the negotiated durable effect tuple;
		// featureless peers cannot establish that historical authority.
		return b.sessionBusy(msg.JobID)
	}
	if msg.Type == protocol.MsgTermsEffectStartRequest {
		if session == nil {
			return b.helloRequired()
		}
		if b.holder == nil || b.holder.ID != sessionID {
			return b.sessionBusy(msg.JobID)
		}
		if session.Outdated {
			return b.extensionOutdatedError()
		}
	}
	if msg.Type == protocol.MsgSurfaceCloseRequest {
		if session == nil {
			return b.helloRequired()
		}
		if b.holder == nil || b.holder.ID != sessionID {
			return b.sessionBusy(msg.JobID)
		}
		if session.Outdated {
			return b.extensionOutdatedError()
		}
	}
	if institutionalMaterializationMessage(msg.Type) {
		if session == nil {
			return b.helloRequired()
		}
		historicalResult := msg.Type == protocol.MsgInstitutionalNavigatedRequest
		if !historicalResult {
			if b.holder == nil || b.holder.ID != sessionID {
				return b.sessionBusy(msg.JobID)
			}
			if session.Outdated {
				return b.extensionOutdatedError()
			}
		}
		// A negotiated pre-permit peer may submit only the old navigation
		// result shape, which is admitted as cleanup-only below.
		if !slices.Contains(session.Features, institutionalMaterializationFeature) ||
			(!slices.Contains(session.Features, effectPermitFeature) && !legacyInstitutionalNavigation) {
			return b.institutionalMaterializationDisabled(msg)
		}
		if !historicalResult && !b.institutionalMaterializationAvailable() {
			return b.institutionalMaterializationDisabled(msg)
		}
		switch msg.Type {
		case protocol.MsgInstitutionalClaimRequest:
			return b.institutionalClaim(ctx, msg.JobID, msg.Payload.(*protocol.InstitutionalClaimRequestPayload))
		case protocol.MsgInstitutionalBindRequest:
			return b.institutionalBind(ctx, msg.JobID, msg.Payload.(*protocol.InstitutionalBindRequestPayload))
		case protocol.MsgInstitutionalRouteRequest:
			return b.institutionalRoute(ctx, msg.JobID, msg.Payload.(*protocol.InstitutionalRouteRequestPayload))
		case protocol.MsgInstitutionalNavigatedRequest:
			return b.institutionalNavigatedForSession(ctx, msg.JobID, msg.Payload.(*protocol.InstitutionalNavigatedRequestPayload), b.holder != nil && b.holder.ID == sessionID)
		case protocol.MsgInstitutionalReconcileRequest:
			// Omitting this case did not make reconcile unsupported: the frame
			// is protocol-defined, validated, gated as an institutional
			// materialization message above, and answered when the feature is
			// disabled. Falling out of this switch dropped it into the generic
			// unknown-frame default, which is classified ErrInvalidFrame and is
			// therefore transport-fatal — so the extension's post-restart
			// binding re-sync tore down the very session it was repairing.
			return b.institutionalReconcile(ctx, msg.Payload.(*protocol.InstitutionalReconcileRequestPayload))
		}
	}
	if institutionalAuthenticationClaimMessage(msg.Type) {
		if session == nil {
			return b.helloRequired()
		}
		historicalResult := msg.Type == protocol.MsgClaimObservation
		if !historicalResult {
			if b.holder == nil || b.holder.ID != sessionID {
				return b.sessionBusy(msg.JobID)
			}
			if session.Outdated {
				return b.extensionOutdatedError()
			}
		}
		// Unlike institutional_materialization_v1 (which negotiates a wire
		// shape old extensions cannot parse), authentication_claim_request
		// and claim_observation are brand-new message types with no legacy
		// shape to disambiguate: the mere fact that a session sent one
		// proves it understands it. This gate checks the DAEMON's own
		// advertised capability, not the session's negotiated list, so
		// landing it daemon-side needs zero extension change (§6 rollout
		// point 1) — an old extension never sends these types at all, and a
		// new one only sends them after its OWN client-side gate
		// (background.ts's AUTHENTICATION_CLAIM_FEATURE) already saw this
		// daemon advertise it in hello_ack. b.Features is the required list
		// NewBridge hardcodes, so this is unreachable in practice — defense
		// in depth, exactly as the design doc's §2.1.1 rule 5 says.
		if !slices.Contains(b.Features, institutionalAuthenticationClaimFeature) {
			return b.institutionalAuthenticationClaimDisabled(msg)
		}
		switch msg.Type {
		case protocol.MsgAuthenticationClaimRequest:
			return b.authenticationClaim(ctx, msg.JobID, msg.Payload.(*protocol.AuthenticationClaimRequestPayload))
		case protocol.MsgClaimObservation:
			return b.claimObservation(ctx, msg.JobID, msg.Payload.(*protocol.ClaimObservationPayload))
		}
	}
	if b.holder == nil || b.holder.ID != sessionID {
		switch msg.Type {
		case protocol.MsgPageAcquire, protocol.MsgTriageSnapshotRequest, protocol.MsgTriageCountsRequest,
			protocol.MsgPageBulkStatusRequest, protocol.MsgPageBulkSubmitRequest, protocol.MsgPageBulkSubmitV2Request,
			protocol.MsgPdfGrabRequest, protocol.MsgPdfGrabStatusRequest, protocol.MsgPdfGrabAbandonRequest,
			protocol.MsgPdfGrabSuggestRequest, protocol.MsgPdfGrabConfirmRequest,
			protocol.MsgSurfacePresence, protocol.MsgWorkPulseRequest, protocol.MsgActivityPageRequest,
			protocol.MsgProviderDriveEpochResultRequest, protocol.MsgProviderDirectGetResult,
			protocol.MsgTermsEffectResultRequest,
			protocol.MsgStatsRequest, protocol.MsgActivityRequest, protocol.MsgTriageDecide,
			protocol.MsgHumanActionResolve, protocol.MsgDeliveryReconcileRequest,
			protocol.MsgReviewPreviewRequest:
			// A frame is holder-independent when the human initiated it in
			// this browser, it starts no browser-side effect — no tab opened,
			// focused, grouped or closed; no offer, handoff, cancel or focus
			// frame emitted; no effect permit allocated or held; no provider
			// drive, direct get, terms acceptance or institutional navigation
			// started — and it claims no bridge-routed authority: it neither
			// depends on being the session the daemon would route
			// daemon-initiated work to, nor mutates holder-scoped bridge
			// state (materialization generation/epoch, offer bookkeeping,
			// schedule cursors, session evidence). Holdership is not a
			// concurrency fence, so mutating the daemon's own records is not
			// disqualifying: dismissing a triage item, resolving a parked
			// human action, reconciling a delivery request, ranking a parked
			// grab's candidates, or binding one to the operator's pick are
			// human decisions about daemon state, and the browser the human
			// clicked in is the right browser by construction. Confirm still
			// runs through the same fenced bind autonomous binding uses, so
			// holdership buys no extra safety a non-holder confirm lacks.
			// Exact historical result frames are also accepted from a
			// recognized old session: identity settlement is cleanup-only
			// unless the transaction independently proves the current
			// attempt and generation.
		default:
			// Offer/handoff frames from a non-holder are refused: acting on
			// them is exactly the silent session fight this arbitration
			// exists to prevent.
			return b.sessionBusy(msg.JobID)
		}
	} else if session.Outdated && !historicalEffectResult {
		// Version gates protect starts and mutable offer/handoff flow. An
		// exact historical result remains cleanup authority for its own permit.
		return b.extensionOutdatedError()
	}

	switch msg.Type {
	case protocol.MsgSurfacePresence:
		return b.surfacePresence(ctx, msg.Payload.(*protocol.SurfacePresencePayload))
	case protocol.MsgWorkPulseRequest:
		return b.workPulse(ctx, msg.Payload.(*protocol.WorkPulseRequestPayload))
	case protocol.MsgActivityPageRequest:
		return b.activityPage(ctx, msg.Payload.(*protocol.ActivityPageRequestPayload))
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
	case protocol.MsgProviderDriveEpochStartRequest:
		return b.providerDriveEpochStart(ctx, msg.JobID, msg.Payload.(*protocol.ProviderDriveEpochStartRequestPayload))
	case protocol.MsgProviderDriveEpochResultRequest:
		return b.providerDriveEpochResultForSession(ctx, msg.JobID, msg.Payload.(*protocol.ProviderDriveEpochResultRequestPayload), b.holder != nil && b.holder.ID == sessionID)
	case protocol.MsgTermsEffectStartRequest:
		return b.termsEffectStart(ctx, msg.JobID, msg.Payload.(*protocol.TermsEffectStartRequestPayload))
	case protocol.MsgTermsEffectResultRequest:
		return b.termsEffectResult(ctx, msg.JobID, msg.Payload.(*protocol.TermsEffectResultRequestPayload))

	case protocol.MsgSurfaceCloseRequest:
		return b.surfaceClose(ctx, msg.Payload.(*protocol.SurfaceCloseRequestPayload))

	case protocol.MsgHandoffLinkRequest:
		return b.handoffLink(ctx, msg.Payload.(*protocol.HandoffLinkRequestPayload))

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
	case protocol.MsgPageBulkSubmitV2Request:
		return b.pageBulkSubmitV2(ctx, msg.Payload.(*protocol.PageBulkSubmitV2RequestPayload))

	case protocol.MsgPdfGrabRequest:
		return b.pdfGrab(ctx, sessionID, msg.Payload.(*protocol.PdfGrabRequestPayload))

	case protocol.MsgPdfGrabStatusRequest:
		return b.pdfGrabStatus(ctx, msg.Payload.(*protocol.PdfGrabStatusRequestPayload))

	case protocol.MsgPdfGrabAbandonRequest:
		return b.pdfGrabAbandonSession(ctx, sessionID, msg.Payload.(*protocol.PdfGrabAbandonRequestPayload))

	case protocol.MsgPdfGrabSuggestRequest:
		return b.pdfGrabSuggest(ctx, msg.Payload.(*protocol.PdfGrabSuggestRequestPayload))

	case protocol.MsgPdfGrabConfirmRequest:
		return b.pdfGrabConfirm(ctx, msg.Payload.(*protocol.PdfGrabConfirmRequestPayload))

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
		// The disposition is recorded only when the peer named one, so an
		// older extension's rows keep their existing shape and read as
		// driving — which is what its acks have always meant.
		var detail map[string]any
		if p, ok := msg.Payload.(*protocol.JobAcceptPayload); ok && p.Disposition != "" {
			detail = map[string]any{"disposition": p.Disposition}
		}
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_accept", detail); err != nil {
			log.Printf("papio: recording browser.job_accept: %v", err)
		}
		return nil, nil

	case protocol.MsgHandoffOutcome:
		if err := b.handoffOutcome(ctx, msg.JobID, msg.Payload.(*protocol.HandoffOutcomePayload)); err != nil {
			log.Printf("papio: recording browser.handoff_outcome: %v", err)
		}
		return nil, nil

	case protocol.MsgJobReject:
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_reject", nil); err != nil {
			log.Printf("papio: recording browser.job_reject: %v", err)
			return nil, nil
		}
		if fellBack, err := b.fallbackOAHandoff(ctx, msg.JobID, "browser_rejected"); err != nil {
			log.Printf("papio: applying browser rejection fallback: %v", err)
			return nil, nil
		} else if fellBack {
			return nil, nil
		}
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			log.Printf("papio: resolving rejected handoff: %v", err)
			return nil, nil
		}
		if err := b.leaveHandoff(ctx, msg.JobID, job.StateUnavailable, string(job.TerminalReasonBrowserRejected)); err != nil {
			log.Printf("papio: closing rejected handoff: %v", err)
		}
		return nil, nil

	case protocol.MsgAuthPending, protocol.MsgAuthReturned:
		if err := b.recordAuth(ctx, msg); err != nil {
			log.Printf("papio: recording browser auth event: %v", err)
		}
		return nil, nil

	case protocol.MsgSessionEvidence:
		if err := b.sessionEvidence(ctx, msg.Payload.(*protocol.SessionEvidencePayload), msg.MsgID); err != nil {
			log.Printf("papio: recording browser session evidence: %v", err)
		}
		return nil, nil
	case protocol.MsgDeliveryContext:
		if err := b.deliveryContext(ctx, msg.JobID, msg.Payload.(*protocol.DeliveryContextPayload)); err != nil {
			log.Printf("papio: recording browser delivery context: %v", err)
		}
		return nil, nil
	case protocol.MsgDownloadStarted:
		p := msg.Payload.(*protocol.DownloadStartedPayload)
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_started",
			map[string]any{"download_id": p.DownloadID, "filename": p.Filename}); err != nil {
			log.Printf("papio: recording browser.download_started: %v", err)
		}
		if row, err := b.jobs.Get(ctx, msg.JobID); err == nil {
			if err := b.resolveProfileGatesForJob(ctx,
				evidenceObservationID("download_started", msg.JobID, strconv.FormatInt(p.DownloadID, 10)),
				row.Policy.Resolver, msg.JobID, job.HumanGateLogin, job.HumanGateMFA,
				job.HumanGateCaptchaOrSecurity, job.HumanGateTermsRequired); err != nil {
				log.Printf("papio: resolving browser gates after download start: %v", err)
			}
		}
		return nil, nil

	case protocol.MsgDownloadComplete:
		p := msg.Payload.(*protocol.DownloadCompletePayload)
		key := browserDownloadKey{JobID: msg.JobID, DownloadID: p.DownloadID}
		detail := map[string]any{
			"download_id": p.DownloadID, "filename": p.Filename,
			"size_bytes": p.SizeBytes,
		}
		producer := artifactProducerIdentity(p.Producer)
		if producer != nil {
			// Persist the complete producer tuple before looking for the file
			// or attempting adoption. Chrome may deliver download_complete
			// before its rename lands in the adoption directory; a later
			// pre-adoption retry records the digest alongside this tuple.
			detail["producer"] = producer
		}
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_complete", detail); err != nil {
			// The producer tuple is the only artifact-to-effect correlation
			// authority. Never adopt bytes when its exact record failed to
			// reach durable storage.
			return nil, err
		}
		b.pruneDeliveryMetadata(b.now())
		pendingDownload := pendingBrowserDownload{
			Filename: p.Filename, Producer: producer, ReceivedAt: b.now(),
		}
		b.pendingDownloads[key] = pendingDownload
		var context *app.BrowserDeliveryContext
		if pending, ok := b.deliveryContexts[key]; ok {
			context = browserDeliveryContext(&pending.Payload)
		}
		candidateID, err := b.adoptOutsideSessionLock(ctx, msg.JobID, p.Filename, context, producer)
		switch {
		case errors.Is(err, errArtifactSuperseded):
			// Another materialization already won this attempt. Retrying would
			// re-deliver bytes that must never be attached, so the pending
			// download is dropped instead of deferred.
			delete(b.pendingDownloads, key)
			delete(b.deliveryContexts, key)
		case errors.Is(err, job.ErrAdoptNotAwaiting), errors.Is(err, job.ErrCandidateNotEligible):
			// The picked job stopped awaiting this download before the bytes
			// arrived (the human action was resolved or dismissed elsewhere).
			// That is permanent, not environmental: deferring it would make
			// the directory sweep retry an adoption the eligibility fence
			// must refuse every time. Drop the pending frames and record the
			// refusal so the reason is visible rather than silent.
			if evErr := b.recordAdoptionDeferred(ctx, msg.JobID, p.Filename, err); evErr != nil {
				log.Printf("papio: recording refused browser adoption: %v", evErr)
			}
			delete(b.pendingDownloads, key)
			delete(b.deliveryContexts, key)
		case err != nil:
			// Environmental failure (file not there yet, Chrome rename race,
			// user saved elsewhere) must not sever the bridge.
			if evErr := b.recordAdoptionDeferred(ctx, msg.JobID, p.Filename, err); evErr != nil {
				log.Printf("papio: recording deferred browser adoption: %v", evErr)
			}
		default:
			pendingDownload.CandidateID = candidateID
			b.pendingDownloads[key] = pendingDownload
			if err := b.recordAdoptionConclusiveLatch(ctx, msg.JobID); err != nil {
				log.Printf("papio: recording adoption safety latch: %v", err)
			}
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

	case protocol.MsgProviderDirectGetResult:
		if err := b.providerDirectGetResultForSession(ctx, msg.JobID, msg.Payload.(*protocol.ProviderDirectGetResultPayload), b.holder != nil && b.holder.ID == sessionID); err != nil {
			log.Printf("papio: recording provider direct result: %v", err)
		}
		return nil, nil

	case protocol.MsgProviderOutcome:
		if err := b.outcome(ctx, msg.JobID, msg.MsgID, msg.Payload.(*protocol.ProviderOutcomePayload)); err != nil {
			log.Printf("papio: recording browser provider outcome: %v", err)
		}
		return nil, nil

	case protocol.MsgCancel:
		// Extension -> daemon: the user closed the broker-owned tab. Treat as a
		// cancelled outcome.
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			log.Printf("papio: resolving cancelled handoff: %v", err)
			return nil, nil
		}
		b.cancelSent[msg.JobID] = true // we initiated nothing to echo back
		b.clearMaterializationTracking(msg.JobID)
		if err := b.jobs.Cancel(ctx, msg.JobID, job.TerminalReasonBrowserCancelled); err != nil {
			log.Printf("papio: cancelling browser job: %v", err)
		}
		return nil, nil
	case protocol.MsgEffectPermitReconcileResponse:
		p := msg.Payload.(*protocol.EffectPermitReconcileResponsePayload)
		if p == nil {
			return nil, nil
		}
		pending, requested := b.effectPermitReconciles[p.RequestID]
		if requested {
			delete(b.effectPermitReconciles, p.RequestID)
		}
		if !requested || pending.permitID != p.PermitID || pending.jobID != msg.JobID {
			return nil, nil
		}
		// Stale or error observations are ordinary no-ops; they must not
		// tear down the browser session.
		if p.Outcome == "stale" || p.Outcome == "error" {
			return nil, nil
		}
		permit, err := b.jobs.GetEffectPermit(ctx, p.PermitID)
		if err != nil || permit == nil || permit.JobID != msg.JobID {
			return nil, nil
		}
		// This response was correlated to a request emitted to the current
		// holder. The permit's stored generation is intentionally used for
		// historical classification after holder replacement.
		obs := job.EffectPermitObservation{
			PermitID:                p.PermitID,
			BrowserHolderGeneration: permit.BrowserHolderGeneration,
		}
		switch p.Outcome {
		case "settled":
			// Only a recorded terms acknowledgement is conclusive without a
			// daemon result event. Download/direct flags are local projections:
			// releasing on one can strand the durable route in-flight.
			if permit.Kind != job.EffectKindTerms {
				return nil, nil
			}
			obs.SettledProof = true
		case "recorded":
			obs.Dispatched = p.Dispatched
			obs.CorrelatedDownload = p.DownloadPresent
			obs.Acknowledged = p.Acknowledged
		default:
			return nil, nil
		}
		if _, err := b.jobs.ReconcileEffectPermit(ctx, obs); err != nil {
			// Stale is ordinary; never map to ErrInvalidFrame.
			if errors.Is(err, job.ErrEffectPermitStale) {
				return nil, nil
			}
			log.Printf("papio: effect permit reconcile: %v", err)
		}
		return nil, nil

	case protocol.MsgError:
		p := msg.Payload.(*protocol.ErrorPayload)
		// Legacy job-level error frames are intentionally uncorrelated. They
		// cannot mutate a direct route; only provider_direct_get_result carries
		// the daemon-minted route tuple.
		// Only the normalized code is durable; the free-text message is
		// extension-supplied and never persisted.
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.error",
			map[string]any{"code": p.Code}); err != nil {
			log.Printf("papio: recording browser error: %v", err)
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("%w: unexpected inbound frame type %q", ErrInvalidFrame, msg.Type)
	}
}
func (b *Bridge) providerDriveEpochAuthorized(ctx context.Context, jobID, safetyDomain string) (bool, error) {
	if b == nil || b.jobs == nil || strings.TrimSpace(jobID) == "" {
		return false, nil
	}
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return false, err
	}
	if row.State != job.StateAwaitingHuman || job.Terminal(row.State) {
		return false, nil
	}
	mode, ok := b.offerableAccessMode(*row)
	if !ok || mode != config.ModeDelegated {
		return false, nil
	}
	actions, err := b.jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		if action.JobID != jobID || action.Kind != handoffActionKind || action.Status != "open" {
			continue
		}
		// A start is authorized only for the currently offered provider
		// safety domain. Holder/attempt checks alone are insufficient after
		// promotion can replace the route while retaining the job handoff.
		if strings.TrimSpace(safetyDomain) != "" {
			currentDomain, domainErr := b.providerDriveSafetyDomain(ctx, jobID)
			if domainErr != nil {
				return false, domainErr
			}
			if currentDomain != strings.TrimSpace(safetyDomain) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}

func (b *Bridge) providerDriveEpochStart(ctx context.Context, jobID string, p *protocol.ProviderDriveEpochStartRequestPayload) ([]json.RawMessage, error) {
	var requestID, driveAttemptID, strategy, revision string
	var ordinal int64
	var outcome, detail string
	if p == nil {
		outcome, detail = "error", "malformed provider drive epoch tuple"
	} else {
		requestID, driveAttemptID, ordinal, strategy, revision = p.RequestID, p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision
		if !validProviderDriveEpochTuple(driveAttemptID, ordinal, strategy, revision) {
			outcome, detail = "error", "malformed provider drive epoch tuple"
		} else if !b.effectPermitAvailable() {
			outcome, detail = "unsupported", "durable effect permits are not negotiated by the current holder"
		} else if !b.providerDriveEpochAvailable() {
			outcome, detail = "unsupported", "provider drive epochs are not negotiated by the current holder"
		} else {
			b.providerDriveEpochMu.Lock()
			events, err := b.jobs.Events(ctx, jobID)
			if err != nil {
				outcome, detail = "error", "provider drive epoch state is unavailable"
			} else {
				tuple := providerDriveEpochKey(driveAttemptID, ordinal, strategy, revision)
				current, _, applied, superseded, domain := providerDriveEpochState(events, tuple)
				if applied {
					outcome, detail = "duplicate", "drive epoch was already resolved"
				} else if current != tuple || domain == "" || superseded {
					outcome, detail = "stale", "drive epoch is not the current offered tuple"
				} else {
					authorized, authErr := b.providerDriveEpochAuthorized(ctx, jobID, domain)
					if authErr != nil {
						outcome, detail = "error", "provider drive epoch authorization is unavailable"
					} else if !authorized {
						outcome, detail = "stale", "drive epoch job is no longer awaiting this handoff"
					} else {
						attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, jobID)
						if attemptErr != nil {
							outcome, detail = "error", "provider drive attempt state is unavailable"
						} else {
							permit, permitOutcome, acquireErr := b.jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
								Identity: job.EffectPermitIdentity{
									JobID: jobID, Kind: job.EffectKindGenericDrive,
									DriveAttemptID: driveAttemptID, Ordinal: ordinal,
									Strategy: strategy, Revision: revision,
								},
								JobAttemptRevision: attempt, BrowserHolderGeneration: b.epoch,
								SafetyDomainID: domain, LeaseUntil: b.now().UTC().Add(b.actionExpiry()),
								Authorization: job.EffectPermitEvent{Kind: "browser.provider_drive_epoch_started", Detail: map[string]any{
									"drive_attempt_id": driveAttemptID, "ordinal": ordinal,
									"strategy": strategy, "revision": revision, "safety_domain": domain,
								}},
							})
							exactReplay := permit != nil && permit.Status == job.EffectPermitHeld &&
								permit.JobAttemptRevision == attempt &&
								permit.BrowserHolderGeneration == b.epoch &&
								permit.SafetyDomainID == domain
							switch {
							case acquireErr != nil && errors.Is(acquireErr, job.ErrEffectPermitBusy):
								outcome, detail = "stale", "effect lane is occupied"
							case acquireErr != nil && errors.Is(acquireErr, job.ErrEffectPermitStale):
								outcome, detail = "stale", "drive epoch authorization is stale"
							case acquireErr != nil:
								outcome, detail = "error", "provider drive epoch start could not be recorded"
							case permitOutcome == job.EffectPermitAcquired || (permitOutcome == job.EffectPermitDuplicate && exactReplay):
								// A lost response may be replayed exactly once:
								// same tuple, attempt, holder generation, and
								// safety domain, with the permit still held.
								outcome, detail = "started", ""
							case permitOutcome == job.EffectPermitDuplicate:
								outcome, detail = "duplicate", "drive epoch was already resolved or fenced"
							case permitOutcome == job.EffectPermitBusyOutcome:
								outcome, detail = "stale", "effect lane is occupied"
							default:
								outcome, detail = "stale", "drive epoch is stale"
							}
						}
					}
				}
			}
			b.providerDriveEpochMu.Unlock()
		}
	}
	frame, err := b.frame(protocol.MsgProviderDriveEpochStartResult, jobID, protocol.ProviderDriveEpochStartResultPayload{
		RequestID: requestID, DriveAttemptID: driveAttemptID, Ordinal: ordinal, Strategy: strategy, Revision: revision, Outcome: outcome, Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) providerDriveEpochResult(ctx context.Context, jobID string, p *protocol.ProviderDriveEpochResultRequestPayload) ([]json.RawMessage, error) {
	return b.providerDriveEpochResultForSession(ctx, jobID, p, true)
}

func (b *Bridge) providerDriveEpochResultForSession(ctx context.Context, jobID string, p *protocol.ProviderDriveEpochResultRequestPayload, currentHolder bool) ([]json.RawMessage, error) {
	var requestID, driveAttemptID, strategy, revision string
	var ordinal int64
	var outcome, detail string
	if p == nil {
		outcome, detail = "error", "malformed provider drive epoch tuple"
	} else {
		requestID, driveAttemptID, ordinal, strategy, revision = p.RequestID, p.DriveAttemptID, p.Ordinal, p.Strategy, p.Revision
		if !validProviderDriveEpochTuple(driveAttemptID, ordinal, strategy, revision) || p.Outcome == "" {
			outcome, detail = "error", "malformed provider drive epoch result"
		} else {
			b.providerDriveEpochMu.Lock()
			identity := job.EffectPermitIdentity{
				JobID: jobID, Kind: job.EffectKindGenericDrive,
				DriveAttemptID: driveAttemptID, Ordinal: ordinal,
				Strategy: strategy, Revision: revision,
			}
			permit, lookupErr := b.jobs.GetEffectPermitByIdentity(ctx, identity)
			if lookupErr != nil {
				outcome, detail = "error", "provider drive permit state is unavailable"
			} else if permit == nil {
				legacyErr := b.jobs.SettleLegacyEffectBlocker(ctx, job.LegacyEffectBlockerInput{
					Kind:  job.EffectKindGenericDrive,
					JobID: jobID, DriveAttemptID: driveAttemptID, Ordinal: ordinal,
					Strategy: strategy, Revision: revision,
				})
				switch {
				case legacyErr == nil:
					outcome, detail = "applied", ""
				case errors.Is(legacyErr, job.ErrEffectPermitStale):
					outcome, detail = "stale", "drive epoch result does not match a started tuple"
				default:
					outcome, detail = "error", "legacy drive epoch result could not be recorded"
				}
			} else {
				resultDetail := map[string]any{
					"drive_attempt_id": driveAttemptID, "ordinal": ordinal,
					"strategy": strategy, "revision": revision,
					"outcome": p.Outcome, "safety_domain": permit.SafetyDomainID,
				}
				if p.Detail != "" {
					resultDetail["detail"] = redactProviderDetail(p.Detail)
				}
				effectOutcome, effectDetail := p.Outcome, p.Detail
				var resultStateErr error
				if permit.Status == job.EffectPermitSettled {
					var events []map[string]any
					events, resultStateErr = b.jobs.Events(ctx, jobID)
					if durable, ok := providerDriveEpochResultDetail(events, providerDriveEpochKey(driveAttemptID, ordinal, strategy, revision)); ok {
						resultDetail = durable
						effectOutcome = stringDetail(durable, "outcome")
						effectDetail = stringDetail(durable, "detail")
					}
				}
				if resultStateErr != nil {
					outcome, detail = "error", "provider drive result state is unavailable"
				} else {
					required := []job.EffectPermitEvent{{Kind: "browser.provider_drive_epoch_result", Detail: resultDetail}}
					var currentAttempt int64
					var attemptErr error
					if currentHolder {
						currentAttempt, attemptErr = b.jobs.MaterializationAttemptRevision(ctx, jobID)
					}
					current := currentHolder && attemptErr == nil && permit.JobAttemptRevision == currentAttempt && permit.BrowserHolderGeneration == b.epoch
					if attemptErr != nil {
						outcome, detail = "error", "provider drive attempt state is unavailable"
					} else {
						currentEvents := make([]job.EffectPermitEvent, 0, 2)
						if current && (providerDriveStrongOutcome(effectOutcome) || providerDriveStrongDetail(effectDetail)) {
							currentEvents = append(currentEvents, job.EffectPermitEvent{Kind: providerLatchEventKind, Detail: map[string]any{
								"kind": "no_positive_effects", "safety_domain": permit.SafetyDomainID,
							}})
						}
						if current && (effectOutcome == "not_pdf" || effectOutcome == "cancelled") {
							currentEvents = append(currentEvents, job.EffectPermitEvent{Kind: "browser.provider_drive_epoch_offered", Detail: map[string]any{
								"drive_attempt_id": driveAttemptID, "ordinal": ordinal + 1,
								"strategy": strategy, "revision": revision, "safety_domain": permit.SafetyDomainID,
							}})
						}
						settleGeneration := b.epoch
						if !currentHolder {
							settleGeneration = -1
						}
						settledPermit, settleOutcome, settleErr := b.jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
							Identity:                       identity,
							RequiredEvents:                 required,
							CurrentAttemptRevision:         permit.JobAttemptRevision,
							CurrentBrowserHolderGeneration: settleGeneration,
							CurrentEvents:                  currentEvents,
						})
						switch {
						case settleErr != nil && errors.Is(settleErr, job.ErrEffectPermitStale):
							outcome, detail = "stale", "drive epoch result does not match a started tuple"
						case settleErr != nil:
							outcome, detail = "error", "provider drive epoch result could not be recorded"
						case settleOutcome == job.EffectPermitSettleDuplicate:
							outcome, detail = "duplicate", ""
						default:
							outcome, detail = "applied", ""
						}
						if currentHolder && settleErr == nil && settledPermit != nil && settledPermit.CurrentAtSettlement &&
							!settledPermit.OperatorOverridden &&
							(settleOutcome == job.EffectPermitApplied || settleOutcome == job.EffectPermitSettleDuplicate) {
							b.reofferPending[jobID] = true
						}
					}
				}
			}
			b.providerDriveEpochMu.Unlock()
		}
	}
	frame, err := b.frame(protocol.MsgProviderDriveEpochResult, jobID, protocol.ProviderDriveEpochResultPayload{
		RequestID: requestID, DriveAttemptID: driveAttemptID, Ordinal: ordinal, Strategy: strategy, Revision: revision, Outcome: outcome, Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func providerDriveEpochState(events []map[string]any, tuple string) (current string, started, applied, superseded bool, domain string) {
	for _, event := range events {
		d, _ := event["detail"].(map[string]any)
		eventTuple := providerDriveEpochKey(stringDetail(d, "drive_attempt_id"), int64(intDetail(d, "ordinal")), stringDetail(d, "strategy"), stringDetail(d, "revision"))
		switch event["kind"] {
		case "browser.provider_drive_epoch_offered":
			if validProviderDriveEpochTuple(stringDetail(d, "drive_attempt_id"), int64(intDetail(d, "ordinal")), stringDetail(d, "strategy"), stringDetail(d, "revision")) {
				current = eventTuple
				if eventTuple == tuple {
					domain = strings.TrimSpace(stringDetail(d, "safety_domain"))
				}
			}
		case "browser.provider_drive_epoch_superseded":
			if eventTuple == tuple {
				superseded = true
			}
		case "browser.provider_drive_epoch_started":
			if eventTuple == tuple {

				started = true
			}
		case "browser.provider_drive_epoch_result":
			if eventTuple == tuple {
				applied = true
			}
		}
	}
	return
}

func providerDriveEpochResultDetail(events []map[string]any, tuple string) (map[string]any, bool) {
	for _, event := range events {
		if event["kind"] != "browser.provider_drive_epoch_result" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		strategy := stringDetail(detail, "strategy")
		revision := stringDetail(detail, "revision")
		if strategy == "" || revision == "" {
			continue
		}
		if providerDriveEpochKey(
			stringDetail(detail, "drive_attempt_id"),
			int64(intDetail(detail, "ordinal")),
			strategy,
			revision,
		) == tuple {
			return detail, true
		}
	}
	return nil, false
}

// providerDriveSafetyDomain derives the current provider safety domain only
// from durable provider-drive offer history. Client-supplied terms metadata is
// never a safety-domain authority.
func (b *Bridge) providerDriveSafetyDomain(ctx context.Context, jobID string) (string, error) {
	if b == nil || b.jobs == nil || strings.TrimSpace(jobID) == "" {
		return "", job.ErrMaterializationStale
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.provider_drive_epoch_offered" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		if domain := strings.TrimSpace(stringDetail(detail, "safety_domain")); domain != "" {
			return domain, nil
		}
	}
	return "", job.ErrMaterializationStale
}

func (b *Bridge) termsEffectStart(ctx context.Context, jobID string, p *protocol.TermsEffectStartRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	var outcome, detail string
	var permitID, occurrenceID string
	if p != nil {
		requestID = p.RequestID
	}
	if !b.effectPermitAvailable() {
		outcome, detail = "unsupported", "durable effect permits are not negotiated by the current holder"
	} else if p == nil {
		outcome, detail = "error", "terms effect request is missing"
	} else if authorized, authErr := b.providerDriveEpochAuthorized(ctx, jobID, ""); authErr != nil {
		outcome, detail = "error", "terms effect handoff state is unavailable"
	} else if !authorized {
		outcome, detail = "stale", "terms effect handoff is no longer live"
	} else {
		attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, jobID)
		if attemptErr != nil {
			outcome, detail = "error", "terms effect attempt state is unavailable"
		} else {
			domain, domainErr := b.providerDriveSafetyDomain(ctx, jobID)
			if domainErr != nil {
				if errors.Is(domainErr, job.ErrMaterializationStale) {
					outcome, detail = "stale", "terms effect has no current provider safety domain"
				} else {
					outcome, detail = "error", "terms effect safety domain is unavailable"
				}
			} else {
				key, keyErr := b.jobs.InstitutionAuthorityKey(ctx)
				if keyErr != nil {
					outcome, detail = "error", "terms effect authority is unavailable"
				} else {
					occurrenceID = materializationMAC(key, "terms_occurrence", jobID, p.AdapterID, p.AdapterVersion, p.AuthorityDigest)
					identity := job.EffectPermitIdentity{
						JobID: jobID, Kind: job.EffectKindTerms,
						TermsOccurrenceID: occurrenceID,
					}
					permit, acquireOutcome, acquireErr := b.jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
						Identity: identity, JobAttemptRevision: attempt,
						BrowserHolderGeneration: b.epoch, SafetyDomainID: domain,
						LeaseUntil: b.now().UTC().Add(b.actionExpiry()),
						Authorization: job.EffectPermitEvent{Kind: "browser.terms_effect_authorized", Detail: map[string]any{
							"terms_occurrence_id": occurrenceID,
							"adapter_id":          p.AdapterID, "adapter_version": p.AdapterVersion,
							"authority_digest": p.AuthorityDigest, "safety_domain": domain,
						}},
					})
					switch {
					case acquireErr != nil && errors.Is(acquireErr, job.ErrEffectPermitBusy):
						outcome, detail = "busy", "the effect lane is occupied"
					case acquireErr != nil && errors.Is(acquireErr, job.ErrEffectPermitStale):
						outcome, detail = "stale", "the terms effect authorization fence was lost"
					case acquireErr != nil:
						outcome, detail = "error", "terms effect authorization is unavailable"
					case acquireOutcome == job.EffectPermitAcquired:
						outcome, detail, permitID = "started", "", permit.ID
					case acquireOutcome == job.EffectPermitDuplicate &&
						permit.Status == job.EffectPermitHeld &&
						permit.JobAttemptRevision == attempt &&
						permit.BrowserHolderGeneration == b.epoch &&
						permit.SafetyDomainID == domain:
						// The start response may have been lost before the
						// extension persisted the tuple. Re-issue the same
						// authorization only to its original holder and
						// attempt; the extension's effect governor serializes
						// live callers, and no new permit is minted.
						outcome, detail, permitID = "started", "", permit.ID
					case acquireOutcome == job.EffectPermitDuplicate:
						outcome, detail = "duplicate", "terms effect was already resolved or fenced"
					case acquireOutcome == job.EffectPermitBusyOutcome:
						outcome, detail = "busy", "the effect lane is occupied"
					default:
						outcome, detail = "stale", "terms effect authorization is stale"
					}
				}
			}
		}
	}
	result := protocol.TermsEffectStartResultPayload{
		RequestID: requestID, Outcome: outcome, Detail: truncate(detail, 500),
	}
	if outcome == "started" {
		result.PermitID, result.TermsOccurrenceID = permitID, occurrenceID
	}
	frame, err := b.frame(protocol.MsgTermsEffectStartResult, jobID, result)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) termsEffectResult(ctx context.Context, jobID string, p *protocol.TermsEffectResultRequestPayload) ([]json.RawMessage, error) {
	requestID := ""
	permitID, occurrenceID := "", ""
	var outcome, detail string
	if p != nil {
		requestID, permitID, occurrenceID = p.RequestID, p.PermitID, p.TermsOccurrenceID
	}
	if p == nil {
		outcome, detail = "error", "terms effect result is missing"
	} else {
		permit, lookupErr := b.jobs.GetEffectPermit(ctx, p.PermitID)
		switch {
		case lookupErr != nil:
			outcome, detail = "error", "terms effect permit state is unavailable"
		case permit == nil || permit.Kind != job.EffectKindTerms || permit.JobID != jobID ||
			permit.TermsOccurrenceID != p.TermsOccurrenceID:
			outcome, detail = "stale", "terms effect result does not match a permit"
		default:
			_, settleOutcome, settleErr := b.jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
				Identity: job.EffectPermitIdentity{
					JobID: jobID, Kind: job.EffectKindTerms,
					TermsOccurrenceID: p.TermsOccurrenceID,
				},
				RequiredEvents: []job.EffectPermitEvent{{Kind: "browser.terms_effect_result", Detail: map[string]any{
					"permit_id": permit.ID, "terms_occurrence_id": permit.TermsOccurrenceID,
					"outcome": p.Outcome,
				}}},
			})
			switch {
			case settleErr != nil && errors.Is(settleErr, job.ErrEffectPermitStale):
				outcome, detail = "stale", "terms effect result does not match a permit"
			case settleErr != nil:
				outcome, detail = "error", "terms effect result could not be recorded"
			case settleOutcome == job.EffectPermitSettleDuplicate:
				outcome, detail = "duplicate", ""
			default:
				outcome, detail = "applied", ""
			}
		}
	}
	frame, err := b.frame(protocol.MsgTermsEffectResult, jobID, protocol.TermsEffectResultPayload{
		RequestID: requestID, PermitID: permitID, TermsOccurrenceID: occurrenceID,
		Outcome: outcome, Detail: truncate(detail, 500),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func providerDriveStrongOutcome(outcome string) bool {
	switch outcome {
	case "wrong_work", "validation_failed", "failed_validation", "unexpected_effect", "envelope_invalid", "invalid_envelope":
		return true
	default:
		return false
	}
}

func providerDriveStrongDetail(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "validation") ||
		strings.Contains(lower, "envelope") ||
		strings.Contains(lower, "unexpected effect")
}

func providerDriveEpochKey(driveAttemptID string, ordinal int64, strategy, revision string) string {
	return driveAttemptID + "\x00" + strconv.FormatInt(ordinal, 10) + "\x00" + strategy + "\x00" + revision
}
func validProviderDriveEpochTuple(driveAttemptID string, ordinal int64, strategy, revision string) bool {
	return driveAttemptID != "" && ordinal >= 0 && strategy != "" && revision != ""
}

// handoffLink mints the same fresh route used by `papio actions open`.
// Routine misses are returned as structured outcomes; only frame construction
// can return a transport error.
func (b *Bridge) handoffLink(ctx context.Context, request *protocol.HandoffLinkRequestPayload) ([]json.RawMessage, error) {
	result := func(outcome, detail, target string) ([]json.RawMessage, error) {
		frame, err := b.frame(protocol.MsgHandoffLinkResult, "", protocol.HandoffLinkResultPayload{
			RequestID: request.RequestID,
			Outcome:   outcome,
			URL:       target,
			Detail:    truncate(detail, 1000),
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{frame}, nil
	}
	if b.jobs == nil {
		return result("unavailable", "handoff links are temporarily unavailable", "")
	}
	var authorized []json.RawMessage
	var frameErr error
	var frameOutcome, frameDetail, frameTarget string
	err := b.jobs.WithOpenHandoffJob(ctx, request.JobID, func(open job.OpenHandoffJob) error {
		setResult := func(outcome, detail, target string) error {
			frameOutcome, frameDetail, frameTarget = outcome, detail, target
			authorized, frameErr = result(outcome, detail, target)
			return frameErr
		}
		target, ok := app.ResolveHumanActionURL(open.Action, open.Row, b.cfg.InstitutionFor)
		if !ok {
			return setResult("not_openurl", "no usable handoff URL is configured for this job", "")
		}
		if len(target) == 0 || len(target) > 4000 {
			return setResult("unavailable", "the generated handoff URL is unavailable", "")
		}
		parsed, parseErr := url.ParseRequestURI(target)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return setResult("unavailable", "the generated handoff URL is unavailable", "")
		}
		return setResult("opened", "", target)
	})
	if err == nil {
		return authorized, nil
	}
	if frameErr != nil {
		// Keep frame construction inside the authorization transaction so an
		// action cannot close before an "opened" response exists. Rebuilding
		// only the already-failed frame here preserves the direct frame-builder
		// error shape pinned by failclosed_test; no response can escape.
		return result(frameOutcome, frameDetail, frameTarget)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// A second read exists only to make routine refusal copy precise; it
		// can never mint a URL.
		row, getErr := b.jobs.Get(ctx, request.JobID)
		if errors.Is(getErr, sql.ErrNoRows) {
			return result("job_gone", "the requested job is no longer available", "")
		}
		if getErr != nil {
			return result("unavailable", "handoff links are temporarily unavailable", "")
		}
		if row.State != job.StateAwaitingHuman {
			return result("not_open_action", "the job is no longer awaiting a handoff", "")
		}
		return result("not_open_action", "the job has no open handoff action", "")
	}
	return result("unavailable", "handoff links are temporarily unavailable", "")
}

// pageCapHostRE validates the page-capture ingress host as a bare origin
// (hostname + optional :port, no scheme/path/query/spaces). It must stay in
// lock-step with captures.captureHostRE — the same grammar the store uses to
// guarantee injective bucketing — so validated hosts round-trip faithfully.
var pageCapHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)

func isValidPageCaptureHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, " /?#") || strings.Contains(host, "://") {
		return false
	}
	return pageCapHostRE.MatchString(host)
}

// pageCapture treats diagnostic content failures as local losses: disconnecting
// the native session over a bad fixture would discard the handoff it was meant
// to help diagnose.
func (b *Bridge) pageCapture(ctx context.Context, sessionID, jobID string, payload *protocol.PageCapturePayload) {
	if !b.cfg.Captures.Enabled || b.captureStore == nil {
		return
	}
	if !isValidPageCaptureHost(payload.Host) {
		log.Printf("papio: ignoring page capture with invalid host %q", payload.Host)
		if pending := b.pendingCaptures[sessionID]; pending != nil && payload.RequestID != "" && pending.payload.RequestID == payload.RequestID {
			delete(b.pendingCaptures, sessionID)
			pending.result <- CaptureResult{
				RequestID: pending.payload.RequestID,
				Outcome:   "not_permitted",
				Detail:    "page capture host must be a bare origin (hostname, optional port)",
			}
		}
		return
	}
	html, err := decodePageCapture(payload)
	if err != nil {
		log.Printf("papio: ignoring page capture from %s: %v", payload.Host, err)
		return
	}
	path, err := b.captureStore.StoreSanitizedPinned(ctx, jobID, payload.Host, payload.Scenario, payload.AdapterID, payload.AdapterVersion, html)
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
		Features:                    slices.Clone(p.Features),
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
		// Ack first, then refuse holdership. The denial is about who receives
		// daemon-initiated offers and handoffs — it is not a rejection of the
		// session. A pending browser still serves user-initiated,
		// holder-independent requests (the dispatcher's non-holder whitelist),
		// and without an ack it never learns which of them this daemon
		// supports, so its own UI fails closed on every one of them.
		ack, err := b.helloAck(sessionRolePending)
		if err != nil {
			return nil, err
		}
		busy, err := b.sessionBusy("")
		if err != nil {
			return nil, err
		}
		return append([]json.RawMessage{ack}, busy...), nil
	}
	if b.holder != nil && !sameSession {
		b.takeovers++
		log.Printf("papio: browser session %s (v%s) took over from previous holder",
			shortSession(sessionID), session.ExtensionVersion)
	}
	delete(b.pending, sessionID)
	holderChanged := b.holder == nil || b.holder.ID != session.ID
	if b.jobs != nil && (holderChanged || b.materializationGenerationUnavailable) {
		generation, genErr := b.jobs.NextMaterializationHolderGeneration(context.Background())
		if genErr != nil {
			b.materializationAuthorityUncertain = true
			b.materializationGenerationUnavailable = true
			log.Printf("papio: materialization holder generation unavailable: %v", genErr)
		} else {
			b.epoch = generation
			b.materializationGenerationUnavailable = false
		}
	}
	if holderChanged {
		b.lastSessionEvidenceAt = map[string]time.Time{}
		b.materializationScheduleCursor = job.CandidateScheduleCursor{}
		b.scheduleCursorPending = job.CandidateScheduleCursor{}
		b.scheduleHasMorePending = false
		b.materializationScheduleBlocked = false
		b.materializationScheduleProcessed = false
		b.materializationScheduleInFlight = false
		b.materializationScheduleVersion++
	}
	b.materializationRecoveryPending = true
	b.holder = session
	b.offered = map[string]bool{}
	b.cancelSent = map[string]bool{}
	b.materializationTracked = map[string]bool{}
	b.authReleased = map[int64]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = map[string]string{}
	b.lastPacedHeld = 0
	if b.jobs != nil {
		key, keyErr := b.jobs.InstitutionAuthorityKey(context.Background())
		if keyErr != nil {
			b.materializationProfileAuthorityUnavailable = true
			b.materializationAuthorityUncertain = true
			log.Printf("papio: institution materialization authority key unavailable: %v", keyErr)
		} else if profileErr := b.reconcileMaterializationProfiles(context.Background(), key); profileErr != nil {
			b.materializationProfileAuthorityUnavailable = true
			b.materializationAuthorityUncertain = true
			log.Printf("papio: institution profile reconciliation failed: %v", profileErr)
		} else {
			b.materializationProfileAuthorityUnavailable = false
			b.reconcileMaterializationGeneration(context.Background())
		}
	}
	if session.Outdated {
		return b.extensionOutdatedError()
	}
	ack, err := b.helloAck(sessionRoleHolder)
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
		return b.unavailable(request.RequestID, "triage_unavailable", "the triage inbox is temporarily unavailable", "triage snapshot", nil)
	}
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	for {
		snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: int(limit), Cursor: request.Cursor, Schema: int(request.SchemaVersions[0])})
		if err != nil {
			return b.unavailable(request.RequestID, "triage_unavailable", "the triage inbox is temporarily unavailable", "triage snapshot", err)
		}
		payload := b.triageSnapshotPayload(ctx, request.RequestID, request.SchemaVersions[0], snapshot)
		size, fits := b.frameSize(protocol.MsgTriageSnapshotResponse, payload)
		if fits {
			frame, err := b.frame(protocol.MsgTriageSnapshotResponse, "", payload)
			if err != nil {
				return nil, err
			}
			return []json.RawMessage{frame}, nil
		}
		if len(snapshot.Items) <= 1 {
			return b.unavailable(request.RequestID, "triage_snapshot_too_large", "the triage inbox item is too large to display", "triage snapshot",
				fmt.Errorf("triage snapshot item exceeds browser frame cap %d", protocol.MaxBrowserMessageBytes))
		}
		limit = int64(nextSnapshotLimit(len(snapshot.Items), size))
	}
}

// nextSnapshotLimit proposes the page size to retry an overflowing snapshot
// with. Retiring one item per pass is what this replaces, and it was quadratic
// in the worst case: every pass re-queries the service AND re-validates every
// surviving item through a full protocol round trip, so a page three times over
// the cap paid ~2/3 of len(items)^2 validations to walk down one item at a time.
// Scaling by the observed overage lands on a fitting page in a pass or two.
//
// This changes only how fast the search converges, never what ships: the caller
// still asks the service for a real page at the proposed size and still
// re-validates and re-measures it, so an estimate that is too generous simply
// costs one more pass. The floor of one guarantees progress, and the caller
// refuses a single item that cannot fit on its own.
func nextSnapshotLimit(items, size int) int {
	if items <= 1 || size <= 0 {
		return 1
	}
	scaled := items * protocol.MaxBrowserMessageBytes / size
	if scaled >= items {
		scaled = items - 1
	}
	if scaled < 1 {
		scaled = 1
	}
	return scaled
}

// triageSnapshotPayload builds one schema's wire shape from a triage
// snapshot. Schema 3 additionally populates attention/route_class/
// auth_requirement (dev/post-build-followups.md item 7) and, for a
// document_delivery human_action, looks up its live delivery_requests row.
// Lookup failures are intentionally represented by an absent optional
// delivery field; outbound frame-construction errors remain fatal to the
// request.
func (b *Bridge) triageSnapshotPayload(ctx context.Context, requestID string, schema int64, snapshot triage.Snapshot) protocol.TriageSnapshotResponsePayload {
	items := make([]protocol.TriageSnapshotItem, 0, len(snapshot.Items))
	counts := triageCountsPayload(snapshot.Counts)
	if schema < 5 {
		counts.TurnsRequired, counts.TurnsWorking = nil, nil
		counts.FamilyBreakdownComplete, counts.RequiredTurnsComplete = nil, nil
		counts.FamilyRuns, counts.RequiredTurns = nil, nil
	}
	omit := func(item protocol.TriageSnapshotItem, reason error) {
		log.Printf("papio: omitting triage snapshot item %s: %v",
			triageSnapshotItemIdentity(item), reason)
		// v4 counts are global and floor-bound, while legacy counts are
		// frame-identity counts, so omission decrements only the latter.
		counts = triageCountsAfterOmission(counts, item, schema)
	}
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
			if schema >= 3 {
				// A new watch hit is informational — nothing is blocked, and
				// papio is not doing anything on the operator's behalf yet.
				payload.Attention = "advisory"
			}
		case triage.KindHumanAction:
			action := item.HumanAction
			if action != nil {
				payload.ActionID, payload.JobID = action.ActionID, action.JobID
				payload.ActionKind, payload.JobState = action.ActionKind, action.JobState
			}
			// Schema 3's route_class vocabulary is closed. An unrepresentable
			// action kind must be omitted rather than invalidating the whole
			// snapshot frame.
			if action != nil && schema == 3 && !slices.Contains(protocol.TriageRouteClasses(), action.ActionKind) {
				omit(payload, fmt.Errorf("human_action.route_class %q is not representable in schema 3", action.ActionKind))
				continue
			}
			if action != nil && schema >= 5 && !slices.Contains(protocol.TriageRouteClassesV5(), action.ActionKind) {
				omit(payload, fmt.Errorf("human_action.route_class %q is not representable in schema 5", action.ActionKind))
				continue
			}
			if action != nil {
				payload.ActionID, payload.JobID = action.ActionID, action.JobID
				payload.ActionKind, payload.JobState = action.ActionKind, action.JobState
				payload.Revision, payload.SHA256, payload.SizeBytes = action.Revision, action.SHA256, action.SizeBytes
				if schema >= 2 && action.BlockedBy != "" {
					payload.RequiresAuth, payload.BlockedBy = action.RequiresAuth, action.BlockedBy
				}
				if schema >= 3 {
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
					if schema >= 5 && item.Family != nil {
						payload.RunKey, payload.NextActor = item.Family.RunKey, item.Family.NextActor
						payload.GuidanceVariant, payload.OperationVariant = item.Family.GuidanceVariant, item.Family.OperationVariant
					}
				}
			}
		case triage.KindRetraction:
			retraction := item.Retraction
			if retraction != nil {
				payload.DOI, payload.Nature = retraction.DOI, retraction.Nature
				payload.NoticedAt = retraction.NoticedAt.UTC().Format(time.RFC3339Nano)
				payload.NoticeDOI = retraction.NoticeDOI
			}
			if schema >= 3 {
				// A retraction/correction/concern notice: informational, not
				// blocking anything (r5's "integrity notice = advisory").
				payload.Attention = "advisory"
			}
		case triage.KindPdfGrab:
			if schema < 4 || item.PdfGrab == nil {
				continue
			}
			payload.Kind = triage.KindPdfGrab
			payload.Label = item.Title
			payload.Grab = &protocol.TriageGrab{GrabID: item.PdfGrab.GrabID, State: item.PdfGrab.State}
			payload.RouteClass = "pdf_identifier_needed"
			payload.BlockedBy = "identifier_missing"
			payload.Attention = "required"
			payload.Ops = []string{"provide_identifier", "dismiss"}
			if schema >= 5 && item.Family != nil {
				payload.RunKey, payload.NextActor = item.Family.RunKey, item.Family.NextActor
				payload.GuidanceVariant, payload.OperationVariant = item.Family.GuidanceVariant, item.Family.OperationVariant
			}
		}
		if reason := triageSnapshotItemValidationError(schema, payload); reason != nil {
			omit(payload, reason)
			continue
		}
		items = append(items, payload)
	}
	return protocol.TriageSnapshotResponsePayload{
		RequestID: requestID, Schema: schema, GeneratedAt: snapshot.GeneratedAt,
		Counts: counts, Items: items, Cursor: snapshot.Cursor,
		HasMore: snapshot.HasMore, UnsupportedItemsCount: int64(snapshot.UnsupportedItemsCount),
	}
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

func triageSnapshotItemIdentity(item protocol.TriageSnapshotItem) string {
	if item.ActionID > 0 {
		return fmt.Sprintf("action_id=%d job_id=%s", item.ActionID, item.JobID)
	}
	if item.ID != "" {
		return "item_id=" + item.ID
	}
	if item.Grab != nil {
		return "grab_id=" + item.Grab.GrabID
	}
	return "kind=" + item.Kind
}

func triageCountsAfterOmission(counts protocol.TriageCounts, item protocol.TriageSnapshotItem, schema int64) protocol.TriageCounts {
	if schema >= 4 {
		return counts
	}
	switch item.Kind {
	case triage.KindWatchHit:
		if counts.WatchHits > 0 {
			counts.WatchHits--
		}
	case triage.KindHumanAction:
		if counts.Actions > 0 {
			counts.Actions--
		}
		if counts.ActionsRequiresAuth != nil && item.RequiresAuth != nil && *item.RequiresAuth && *counts.ActionsRequiresAuth > 0 {
			(*counts.ActionsRequiresAuth)--
		}
	case triage.KindRetraction:
		if counts.Retractions > 0 {
			counts.Retractions--
		}
	}
	if counts.PendingTotal > 0 {
		counts.PendingTotal--
	}
	return counts
}

func triageSnapshotItemValidationError(schema int64, item protocol.TriageSnapshotItem) error {
	counts := protocol.TriageCounts{PendingTotal: 1}
	switch item.Kind {
	case triage.KindWatchHit:
		counts.WatchHits = 1
	case triage.KindHumanAction:
		counts.Actions = 1
	case triage.KindRetraction:
		counts.Retractions = 1
	}
	return validateTriageSnapshotPayload(protocol.TriageSnapshotResponsePayload{
		RequestID: "snapshot-item-validate", Schema: schema, GeneratedAt: "2026-01-01T00:00:00Z",
		Counts: counts, Items: []protocol.TriageSnapshotItem{item}, HasMore: false,
	})
}

func validateTriageSnapshotPayload(payload protocol.TriageSnapshotResponsePayload) error {
	raw, err := json.Marshal(map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     protocol.MsgTriageSnapshotResponse,
		"msg_id":   "snapshot-validate",
		"seq":      1,
		"payload":  payload,
	})
	if err != nil {
		return err
	}
	_, err = protocol.DecodeBrowserMessage(raw)
	return err
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
		log.Printf("papio: delivery lookup unavailable (job_id=%s): %s",
			truncate(jobID, 128), truncate(err.Error(), 256))
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
func (b *Bridge) surfacePresence(_ context.Context, request *protocol.SurfacePresencePayload) ([]json.RawMessage, error) {
	now := b.now()
	clientAt, err := time.Parse(time.RFC3339, request.At)
	if err != nil {
		clientAt = time.Time{}
	}
	if b.presence == nil {
		b.presence = make(map[string]presenceLease)
	}
	b.anyFocusedLocked(now)
	b.compactPresenceOrderLocked()
	if _, exists := b.presence[request.InstanceID]; !exists {
		for len(b.presence) >= maxPresenceLeases {
			if len(b.presenceOrder) == 0 {
				for id := range b.presence {
					delete(b.presence, id)
					break
				}
				break
			}
			delete(b.presence, b.presenceOrder[0])
			b.presenceOrder = b.presenceOrder[1:]
		}
		b.presenceOrder = append(b.presenceOrder, request.InstanceID)
	}
	b.presence[request.InstanceID] = presenceLease{
		surface: request.Surface, focused: request.Focused, received: now, clientAt: clientAt,
	}
	frame, err := b.frame(protocol.MsgSurfacePresenceAck, "", protocol.SurfacePresenceAckPayload{
		RequestID: request.RequestID, Accepted: true,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func triageLinks(links []triage.Link) []protocol.TriageLink {
	result := make([]protocol.TriageLink, 0, len(links))
	for _, link := range links {
		result = append(result, protocol.TriageLink{Rel: link.Rel, URL: link.URL})
	}
	return result
}

func triageCountsPayload(counts triage.Counts) protocol.TriageCounts {
	payload := protocol.TriageCounts{
		PendingTotal: int64(counts.PendingTotal), WatchHits: int64(counts.WatchHits), Actions: int64(counts.Actions),
		Retractions: int64(counts.Retractions), JobsWorking: int64(counts.JobsWorking),
		JobsNeedsReview: int64(counts.JobsNeedsReview), FailureGroups7d: int64(counts.FailureGroups7d),
	}
	if counts.TurnsRequired != nil {
		payload.TurnsRequired = new(int64(*counts.TurnsRequired))
	}
	if counts.TurnsWorking != nil {
		payload.TurnsWorking = new(int64(*counts.TurnsWorking))
	}
	if counts.FamilyBreakdownComplete != nil {
		payload.FamilyBreakdownComplete = new(*counts.FamilyBreakdownComplete)
	}
	if counts.RequiredTurnsComplete != nil {
		payload.RequiredTurnsComplete = new(*counts.RequiredTurnsComplete)
	}
	for _, run := range counts.FamilyRuns {
		payload.FamilyRuns = append(payload.FamilyRuns, protocol.TriageFamilyRun{
			RunKey: run.RunKey, FirstRank: int64(run.FirstRank), RouteClass: run.RouteClass, ActionKind: run.ActionKind,
			NextActor: run.NextActor, GuidanceVariant: run.GuidanceVariant, OperationVariant: run.OperationVariant, Count: int64(run.Count),
		})
	}
	for _, turn := range counts.RequiredTurns {
		wire := protocol.TriageRequiredTurn{ItemID: turn.ItemID, ItemKind: turn.ItemKind, RouteClass: turn.RouteClass, GateClaimID: turn.GateClaimID, DependentJobs: int64(turn.DependentJobs)}
		if turn.ActionID > 0 {
			wire.ActionID, wire.JobID = new(int64(turn.ActionID)), turn.JobID
		} else {
			wire.GrabID = turn.GrabID
		}
		payload.RequiredTurns = append(payload.RequiredTurns, wire)
	}
	return payload
}

func triageCountsPayloadV2(counts triage.Counts) protocol.TriageCounts {
	payload := triageCountsPayload(counts)
	auth := int64(counts.ActionsRequiresAuth)
	payload.ActionsRequiresAuth = &auth
	return payload
}
func (b *Bridge) triageCounts(ctx context.Context, request *protocol.TriageCountsRequestPayload) ([]json.RawMessage, error) {
	if b.triage == nil {
		return b.triageUnavailable(request.RequestID, nil)
	}
	schema := 0
	if len(request.SchemaVersions) == 1 {
		schema = int(request.SchemaVersions[0])
	}
	counts, err := b.triage.Counts(ctx, schema)
	if err != nil {
		return b.triageUnavailable(request.RequestID, err)
	}
	payload := triageCountsPayload(counts)
	if schema == 2 {
		payload = triageCountsPayloadV2(counts)
	}
	if schema != 3 {
		payload.TurnsRequired, payload.TurnsWorking = nil, nil
		payload.FamilyBreakdownComplete, payload.RequiredTurnsComplete = nil, nil
		payload.FamilyRuns, payload.RequiredTurns = nil, nil
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
// sessionBusy/helloRequired/extensionOutdatedError instead. The cause is
// logged rather than sent: the extension gets a stable code it can render as
// "temporarily unavailable", the operator keeps the diagnosis.
const unavailableLogSummaryWindow = 5 * time.Minute

type unavailableLogState struct {
	firstAt    time.Time
	suppressed int
}

const unavailableLogKeySep = "\x1e"

func unavailableLogKey(surface string, cause error) string {
	if cause == nil {
		return surface + unavailableLogKeySep + "no-triage-service"
	}
	return surface + unavailableLogKeySep + cause.Error()
}

func (b *Bridge) logUnavailable(surface string, cause error) {
	key := unavailableLogKey(surface, cause)
	now := time.Now()
	b.unavailableLogMu.Lock()
	defer b.unavailableLogMu.Unlock()
	prev, seen := b.unavailableLog[key]
	if seen && now.Sub(prev.firstAt) < unavailableLogSummaryWindow {
		prev.suppressed++
		b.unavailableLog[key] = prev
		return
	}
	if seen && prev.suppressed > 0 {
		log.Printf("papio: %s unavailable: suppressed %d identical errors in 5m", surface, prev.suppressed)
	}
	if cause != nil {
		log.Printf("papio: %s unavailable: %v", surface, cause)
	} else {
		log.Printf("papio: %s unavailable: no triage service configured", surface)
	}
	b.unavailableLog[key] = unavailableLogState{firstAt: now, suppressed: 0}
}

func (b *Bridge) unavailable(requestID, code, message, surface string, cause error) ([]json.RawMessage, error) {
	b.logUnavailable(surface, cause)
	frame, err := b.frame(protocol.MsgError, "", protocol.ErrorPayload{
		Code: code, Message: message, RequestID: requestID,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) triageUnavailable(requestID string, cause error) ([]json.RawMessage, error) {
	return b.unavailable(requestID, "triage_unavailable", "the triage inbox is temporarily unavailable", "triage counts", cause)
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
		return b.statsUnavailable(request.RequestID, nil)
	}
	stats, err := b.triage.Stats(ctx)
	if err != nil {
		return b.statsUnavailable(request.RequestID, err)
	}
	frame, err := b.frame(protocol.MsgStatsResponse, "",
		statsPayload(request.RequestID, b.now().UTC().Format(time.RFC3339), stats))
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}
func (b *Bridge) statsUnavailable(requestID string, cause error) ([]json.RawMessage, error) {
	return b.unavailable(requestID, "stats_unavailable", "acquisition stats are temporarily unavailable", "acquisition stats", cause)
}

// activity returns a bounded, display-only read model. A store read failure
// is routine from the browser's point of view, so it is logged and represented
// as a structured unavailable error rather than tearing down the session.
func (b *Bridge) activity(ctx context.Context, request *protocol.ActivityRequestPayload) ([]json.RawMessage, error) {
	limit := request.Limit
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if b.jobs == nil || b.jobs.S == nil {
		return b.unavailable(request.RequestID, "activity_unavailable", "activity history is temporarily unavailable", "activity feed", nil)
	}
	recent, err := b.jobs.S.RecentEvents(int(limit), 0)
	if err != nil {
		return b.unavailable(request.RequestID, "activity_unavailable", "activity history is temporarily unavailable", "activity feed", err)
	}
	entries := make([]protocol.ActivityEntryPayload, 0, limit)
	for _, event := range recent {
		entry := protocol.ActivityEntryPayload{
			Seq: event.Seq, At: event.At.UTC().Format(time.RFC3339),
			Kind: activityKind(event.Kind), Text: store.ActivityText(event.Kind, event.Detail),
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
	frame, err := b.frame(protocol.MsgActivityResponse, "", protocol.ActivityResponsePayload{
		RequestID: request.RequestID, GeneratedAt: b.now().UTC().Format(time.RFC3339), Entries: entries,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// SetPulseService wires the daemon's authoritative pulse read model after
// construction, preserving the existing NewBridge call signature.
func (b *Bridge) SetPulseService(service *pulse.Service) { b.pulse = service }

func (b *Bridge) workPulse(ctx context.Context, request *protocol.WorkPulseRequestPayload) ([]json.RawMessage, error) {
	if b.pulse == nil {
		return b.unavailable(request.RequestID, "pulse_unavailable", "live progress is temporarily unavailable", "work pulse", nil)
	}
	snapshot, err := b.pulse.Read(ctx)
	if err != nil {
		return b.unavailable(request.RequestID, "pulse_unavailable", "live progress is temporarily unavailable", "work pulse", err)
	}
	snapshot.RequestID = request.RequestID
	if !b.effectPermitAvailable() {
		// effect_permits was added to the strict work_pulse_v1 payload with
		// effect_permit_v1 as its compatibility gate. Older peers must never
		// see the additive field.
		snapshot.EffectPermits = nil
	}
	frame, err := b.frame(protocol.MsgWorkPulseResponse, "", snapshot)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) activityPage(ctx context.Context, request *protocol.ActivityPageRequestPayload) ([]json.RawMessage, error) {
	limit := int(request.Limit)
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	before := int64(0)
	if request.BeforeSeq != "" {
		if parsed, err := strconv.ParseInt(request.BeforeSeq, 10, 64); err == nil && parsed > 0 {
			before = parsed
		}
	}
	entries := make([]protocol.ActivityEntryPayload, 0, limit)
	var hasMore, gap bool
	var latest, earliest, newCount int64
	if b.jobs == nil || b.jobs.S == nil {
		return b.unavailable(request.RequestID, "activity_page_unavailable", "activity history is temporarily unavailable", "activity page", nil)
	}
	if err := b.jobs.S.DB().QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0), COALESCE(MIN(seq),0) FROM events").Scan(&latest, &earliest); err != nil {
		return b.unavailable(request.RequestID, "activity_page_unavailable", "activity history is temporarily unavailable", "activity page", err)
	}
	if request.SeenThroughSeq != "" {
		if seen, err := strconv.ParseInt(request.SeenThroughSeq, 10, 64); err == nil && seen > 0 {
			if earliest > 0 && seen < earliest-1 {
				gap = true
			} else if err := b.jobs.S.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE seq > ?", seen).Scan(&newCount); err != nil {
				return b.unavailable(request.RequestID, "activity_page_unavailable", "activity history is temporarily unavailable", "activity page", err)
			}
		}
	}
	recent, truncated, err := b.jobs.S.RecentEventsPage(limit, before, "")
	if err != nil {
		return b.unavailable(request.RequestID, "activity_page_unavailable", "activity history is temporarily unavailable", "activity page", err)
	}
	hasMore = truncated
	for _, event := range recent {
		entry := protocol.ActivityEntryPayload{
			Seq: event.Seq, At: event.At.UTC().Format(time.RFC3339),
			Kind: activityKind(event.Kind), Text: store.ActivityText(event.Kind, event.Detail),
		}
		if event.JobID != "" {
			entry.JobID = event.JobID
		}
		if event.JobTitle != "" {
			entry.Title = activityTitle(event.JobTitle)
		}
		entries = append(entries, entry)
	}
	payload := protocol.ActivityPageResponsePayload{
		RequestID: request.RequestID, GeneratedAt: b.now().UTC().Format(time.RFC3339),
		Entries: entries, HasMore: hasMore, LatestSeq: latest,
	}
	if len(entries) > 0 && hasMore {
		payload.Cursor = strconv.FormatInt(entries[len(entries)-1].Seq, 10)
	}
	zero := int64(0)
	payload.NewCountSince = &zero
	if request.SeenThroughSeq != "" {
		if gap {
			payload.NewCountSince = nil
			payload.Gap = &gap
		} else {
			payload.NewCountSince = &newCount
		}
	}
	frame, err := b.frame(protocol.MsgActivityPageResponse, "", payload)
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
	if strings.HasPrefix(request.ItemID, triage.PdfGrabIDPrefix) {
		return b.dismissPdfGrab(ctx, request)
	}
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
		targets := make([]watch.DigestTarget, 0, len(hit.Watches))
		for _, watched := range hit.Watches {
			targets = append(targets, watch.DigestTarget{WatchID: watched.ID, WorkKey: watched.WorkKey})
		}
		if err := b.watchRunner.AcquireDigests(ctx, targets); err != nil {
			if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
				return b.triageDecisionResult(request.RequestID, "conflict", "")
			}
			return b.triageDecisionResult(request.RequestID, "error", err.Error())
		}
		return b.triageDecisionResult(request.RequestID, "applied", "")
	}
	selected, err := triageDismissScope(request.WatchScope, hit.Watches)
	if err != nil {
		return b.triageDecisionResult(request.RequestID, "error", err.Error())
	}
	targets := make([]watch.DigestTarget, 0, len(hit.Watches))
	for _, watched := range hit.Watches {
		if !selected[watched.ID] {
			continue
		}
		targets = append(targets, watch.DigestTarget{WatchID: watched.ID, WorkKey: watched.WorkKey})
	}
	if err := b.watchRunner.ConsumeDigests(ctx, targets); err != nil {
		if errors.Is(err, watch.ErrDigestEntryNotFound) || errors.Is(err, sql.ErrNoRows) {
			return b.triageDecisionResult(request.RequestID, "conflict", "")
		}
		return b.triageDecisionResult(request.RequestID, "error", err.Error())
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
func (b *Bridge) dismissPdfGrab(ctx context.Context, request *protocol.TriageDecidePayload) ([]json.RawMessage, error) {
	if request.Op != "dismiss" {
		return b.triageDecisionResult(request.RequestID, "error", "pdf grabs support only the dismiss operation")
	}
	if b.grabs == nil {
		return b.triageDecisionResult(request.RequestID, "error", "pdf grabs are not configured")
	}
	id := strings.TrimPrefix(request.ItemID, triage.PdfGrabIDPrefix)
	if id == "" {
		return b.triageDecisionResult(request.RequestID, "conflict", "")
	}
	g, err := b.grabs.Get(ctx, id)
	if err != nil {
		return b.triageDecisionResult(request.RequestID, "error", "pdf grab is temporarily unavailable")
	}
	if g == nil {
		return b.triageDecisionResult(request.RequestID, "conflict", "")
	}
	quarantinePath := g.QuarantinePath
	if err := b.grabs.Delete(ctx, id); err != nil {
		return b.triageDecisionResult(request.RequestID, "error", "pdf grab could not be dismissed")
	}
	if quarantinePath != "" {
		if err := os.RemoveAll(filepath.Dir(quarantinePath)); err != nil {
			log.Printf("papio: removing dismissed grab quarantine %s: %v", filepath.Dir(quarantinePath), err)
		}
	}
	return b.triageDecisionResult(request.RequestID, "applied", "")
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
		var dismissedKind string
		actions, listErr := b.jobs.ListHumanActions(ctx, true)
		if listErr != nil {
			return b.humanActionResolveResult(request.RequestID, "error", listErr.Error())
		}
		for _, action := range actions {
			if action.ID == request.ActionID {
				dismissedKind = action.Kind
				break
			}
		}
		if dismissedKind == "" {
			return b.humanActionResolveResult(request.RequestID, "error", "human action not found")
		}
		_, err := b.jobs.DismissHumanAction(ctx, request.ActionID, request.ExpectedRevision)
		if err != nil {
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
	profile, err := b.svc.Delivery.ResolveGateProfileFor(ctx, row.InstitutionProfile)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	next := delivery.NextCheck(b.now(), 0, profile.StatusPollMinutes)
	repairDetail := map[string]any{"reason": "document_delivery_confirmed_exists"}
	repairDetail["from"], repairDetail["to"] = job.StateAwaitingHuman, job.StateResolving
	repairJSON, err := marshalJobDetail(repairDetail)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	retryDetail := map[string]any{"reason": job.RetryReasonDocumentDeliveryPending, "provider_reference": request.ProviderReference}
	retryDetail["from"], retryDetail["to"] = job.StateResolving, job.StateRetryWait
	retryJSON, err := marshalJobDetail(retryDetail)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	// One threaded tx: delivery row mutations + job transitions atomically.
	// Human action closed LAST via RepairAwaitingHumanTx.
	db := b.svc.Delivery.DB()
	if db == nil {
		// Fallback to ordered apply if DB accessor unavailable.
		if err := b.svc.Delivery.UpdateState(ctx, row.ID, delivery.StatePending); err != nil {
			return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
		}
		if err := b.svc.Delivery.RecordPoll(ctx, row.ID, request.ProviderReference, next); err != nil {
			return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
		}
		if err := b.jobs.RepairAwaitingHuman(ctx, request.JobID, []int64{action.ID}, map[string]any{"reason": "document_delivery_confirmed_exists"}); err != nil {
			return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
		}
		if err := b.jobs.Transition(ctx, request.JobID, job.StateResolving, job.StateRetryWait, map[string]any{"reason": job.RetryReasonDocumentDeliveryPending, "provider_reference": request.ProviderReference}, job.WithRetryAt(next)); err != nil {
			return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
		}
		return b.deliveryReconcileResult(request.RequestID, "applied", "")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	defer func() { _ = tx.Rollback() }()
	if err := b.svc.Delivery.UpdateStateTx(ctx, tx, row.ID, delivery.StatePending); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.svc.Delivery.RecordPollTx(ctx, tx, row.ID, request.ProviderReference, next); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	now := bridgeNow()
	if err := b.jobs.RepairAwaitingHumanTx(ctx, tx, request.JobID, []int64{action.ID}, string(repairJSON), now); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.jobs.TransitionTx(ctx, tx, request.JobID, job.StateResolving, job.StateRetryWait, string(retryJSON), job.TransitionTxConfig{RetryAt: next.UTC().Format(time.RFC3339Nano)}, now); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := tx.Commit(); err != nil {
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
	if _, err := b.svc.SubmitDelivery(ctx, request.JobID); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	if err := b.jobs.RepairAwaitingHuman(ctx, request.JobID, []int64{action.ID}, map[string]any{"reason": "document_delivery_confirmed_absent"}); err != nil {
		return b.deliveryReconcileResult(request.RequestID, "error", err.Error())
	}
	return b.deliveryReconcileResult(request.RequestID, "applied", "")
}

func marshalJobDetail(detail map[string]any) (string, error) {
	data, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func bridgeNow() string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Use store.Now() format; time.Now is fine for bridge context.
	return now
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
// review_preview_result frame instead of a raw Go error. The host's explicit
// application-failure disposition keeps even an unexpected daemon error from
// tearing down the native-messaging connection.
func (b *Bridge) reviewPreviewError(requestID, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgReviewPreviewResult, "", protocol.ReviewPreviewResultPayload{
		RequestID: requestID, Outcome: "error", Detail: truncate(detail, 1000),
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// frameSize reports the encoded size of one outbound frame and whether it fits
// the native-messaging cap. The size is what lets an overflowing page be
// resized in proportion to its overage instead of one item at a time.
func (b *Bridge) frameSize(msgType string, payload any) (int, bool) {
	raw, err := json.Marshal(map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     msgType,
		"msg_id":   "AAAAAAAAAAAAAAAAAAAAAA",
		"seq":      b.seq + 1,
		"payload":  payload,
	})
	if err != nil {
		return 0, false
	}
	return len(raw), len(raw) <= protocol.MaxBrowserMessageBytes
}

func (b *Bridge) frameFits(msgType string, payload any) bool {
	_, fits := b.frameSize(msgType, payload)
	return fits
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
				// b.frame performs the final protocol self-validation. A
				// malformed item must still become a structured refusal, not
				// an RPC error that can take down the browser session.
				log.Printf("papio: page-bulk status frame self-validation failed: %v", err)
				fallback := protocol.PageBulkStatusResultPayload{
					RequestID: request.RequestID, ScanID: request.ScanID, Truncated: true,
				}
				frame, fallbackErr := b.frame(protocol.MsgPageBulkStatusResult, "", fallback)
				if fallbackErr != nil {
					return nil, fallbackErr
				}
				return []json.RawMessage{frame}, nil
			}
			return []json.RawMessage{frame}, nil
		}
		// Detect an item that cannot fit even by itself before dropping valid
		// neighbors to satisfy the aggregate frame cap. "invalid" is the
		// existing renderable refusal state; clearing the canonical identity
		// prevents the workspace from offering it for submission.
		refused := false
		for i, item := range items {
			if b.frameFits(protocol.MsgPageBulkStatusResult, protocol.PageBulkStatusResultPayload{
				RequestID: request.RequestID, ScanID: request.ScanID,
				Items: []protocol.PageBulkStatusItem{item}, Truncated: true,
			}) {
				continue
			}
			log.Printf("papio: refusing oversized page-bulk status item %s", item.LocalID)
			items[i] = protocol.PageBulkStatusItem{LocalID: item.LocalID, Status: "frame_too_large"}
			refused = true
		}
		if refused {
			truncated = true
			continue
		}
		if len(items) <= 1 {
			// This is defensive: all current protocol fields are bounded, so
			// a single valid item should fit. Return a structured refusal if
			// a future field violates that invariant.
			if len(items) == 1 {
				log.Printf("papio: refusing page-bulk status item %s after frame-cap exhaustion", items[0].LocalID)
				items[0] = protocol.PageBulkStatusItem{LocalID: items[0].LocalID, Status: "invalid"}
				truncated = true
				continue
			}
			return b.unavailable(request.RequestID, "page_bulk_status_unavailable",
				"page status is temporarily unavailable", "page bulk status", nil)
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
			// zotio.LookupWork has no W-id field — DOI/ArXiv/PMID only — so an
			// "openalex" row (a real, reachable kind since normalizePageBulk
			// Identifier learned it) is deliberately never entered here. It
			// never reaches zotio at all; pageBulkStatusItem's default
			// ownership branch skips the generic holdings registry for it
			// too, so its completeness follows neither source (bridge_test.go's
			// TestPageBulkStatusOpenAlexWOnlyRowSkipsZotioAndFollowsLedger).
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
		// LocalOnly: the workspace privacy line promises this check makes
		// no network request; zotio's refresh sync would be one.
		request := zotio.LookupWorksRequest{LocalOnly: true, Works: make([]zotio.LookupWork, len(chunk))}
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
		if kind != ownership.KindDOI && kind != ownership.KindArXiv && kind != ownership.KindPMID {
			// Neither zotio (pageBulkZotioLookup's LookupWork carries only
			// DOI/ArXiv/PMID — an openalex row never reaches zotioLookup at
			// all) nor the generic holdings registry (pageBulkOwnershipQuery
			// below answers the same three kinds only) can classify this
			// identifier — an OpenAlex-only row today. Report the ordinary
			// not-yet-checked state without ever calling either source: its
			// completeness follows the same unchecked-source semantics as any
			// other identifier no configured source covers, and this can
			// never manufacture a false eligible-and-complete claim.
			item.Status = "ownership_incomplete"
			break
		}
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
	case "openalex":
		normalized, err := work.NormalizeOpenAlex(value)
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
func (b *Bridge) submitPageBulkMembers(ctx context.Context, keys []string) ([]batch.MemberOutcome, error) {
	outcomes := make([]batch.MemberOutcome, 0, len(keys))
	for _, key := range keys {
		outcome := batch.MemberOutcome{CanonicalKey: key}
		wr, ok := pageBulkWorkRequest(key)
		if !ok {
			outcome.Outcome = "invalid"
			outcomes = append(outcomes, outcome)
			continue
		}
		if kind, value, ok := pageBulkIdentifierOf(wr); ok {
			if _, readyJobID, _, statusErr := b.canonicalJobStatus(ctx, kind, value); statusErr == nil && readyJobID != "" {
				outcome.Outcome = "already_owned"
				outcomes = append(outcomes, outcome)
				continue
			}
		}
		query := ownership.QueryFor(wr.Identifiers.DOI, wr.Identifiers.ArXiv, wr.Identifiers.PMID, wr.DesiredVersion, ownership.EntityUnknown)
		decision, _ := b.pageBulkOwnership(ctx, query)
		if decision.Suppress {
			outcome.Outcome = "already_owned"
			outcomes = append(outcomes, outcome)
			continue
		}
		if b.svc == nil {
			outcome.Outcome = "invalid"
			outcomes = append(outcomes, outcome)
			continue
		}
		result, err := b.svc.SubmitWithOptionsAs(ctx, job.PrincipalUnknown, wr, app.SubmitOptions{Consumer: pageBulkConsumer})
		if err != nil {
			outcome.Outcome = "invalid"
			outcomes = append(outcomes, outcome)
			continue
		}
		outcome.JobID = result.JobID
		if result.Existing {
			outcome.Outcome = "joined"
		} else {
			outcome.Outcome = "submitted"
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func memberCounts(outcomes []batch.MemberOutcome) (submitted, joined, owned, invalid int64) {
	for _, outcome := range outcomes {
		switch outcome.Outcome {
		case "submitted":
			submitted++
		case "joined":
			joined++
		case "already_owned":
			owned++
		default:
			invalid++
		}
	}
	return
}

func (b *Bridge) pageBulkSubmit(ctx context.Context, request *protocol.PageBulkSubmitRequestPayload) ([]json.RawMessage, error) {
	outcomes, _ := b.submitPageBulkMembers(ctx, request.CanonicalKeys)
	submitted, joined, alreadyOwned, invalid := memberCounts(outcomes)
	batchID := newMsgID()
	at := b.now().UTC()
	if err := b.recordPageBulkRun(ctx, request.Source, len(request.CanonicalKeys), submitted, invalid, batchID, at); err != nil {
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

func (b *Bridge) pageBulkSubmitV2(ctx context.Context, request *protocol.PageBulkSubmitV2RequestPayload) ([]json.RawMessage, error) {
	if b.cohorts == nil {
		return b.unavailable(request.RequestID, "page_bulk_cohort_unavailable", "page-bulk cohorts are temporarily unavailable", "page bulk cohort", nil)
	}
	result, err := b.cohorts.SubmitChunk(ctx, batch.ChunkRequest{
		RequestID: request.RequestID, CohortID: request.CohortID,
		Source:      batch.Source{Kind: request.Source.Kind, Label: request.Source.Origin, Detector: request.Source.Detector, ScanID: request.ScanID},
		CohortTotal: int(request.CohortTotal), ChunkIndex: int(request.ChunkIndex),
		FinalChunk: request.FinalChunk, CanonicalKeys: request.CanonicalKeys,
	}, func(submitCtx context.Context, keys []string) ([]batch.MemberOutcome, error) {
		return b.submitPageBulkMembers(submitCtx, keys)
	})
	if err != nil {
		conflict := &batch.ConflictError{}
		if errors.As(err, &conflict) {
			return b.unavailable(request.RequestID, "page_bulk_cohort_conflict", conflict.Error(), "page bulk cohort", err)
		}
		return b.unavailable(request.RequestID, "page_bulk_cohort_unavailable", "page-bulk cohort submission is temporarily unavailable", "page bulk cohort", err)
	}
	payload := protocol.PageBulkSubmitV2ResultPayload{
		RequestID: request.RequestID, ScanID: request.ScanID, CohortID: request.CohortID,
		ChunkIndex: request.ChunkIndex, FinalChunk: request.FinalChunk, BatchID: result.BatchID,
		Membership: result.Membership, PersistedMembers: int64(result.PersistedMembers),
		Submitted: int64(result.Submitted), Joined: int64(result.Joined),
		AlreadyOwned: int64(result.AlreadyOwned), Invalid: int64(result.Invalid),
	}
	if result.CohortTotal != nil {
		v := int64(*result.CohortTotal)
		payload.CohortTotal = &v
	}
	frame, err := b.frame(protocol.MsgPageBulkSubmitV2Result, "", payload)
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
	case ids.OpenAlex != "":
		return "openalex", ids.OpenAlex, true
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
	case "openalex":
		ids.OpenAlex = normValue
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

// pdfGrabRefusalReason reports why sessionID may not start a PDF grab, or ""
// when it may. It is the whole admission test for the grab path.
//
// It deliberately does NOT compare sessionID against the holder, and the
// conjunct that used to (`b.holder.ID != sessionID`) was wrong three times
// over:
//
//   - handle's non-holder whitelist already admits MsgPdfGrabRequest (the
//     `case protocol.MsgPdfGrabRequest, ...` arm around bridge.go:2026), so
//     the dispatcher and this function contradicted each other and the
//     stricter one silently won.
//   - A grab is user-initiated and self-routed: the requesting session gets
//     its own steering_path and runs its own chrome.downloads.download into
//     it, and the sweep adopts by directory, not by session. Nothing in the
//     flow needs the daemon to know how to reach this browser unprompted,
//     which is the only thing holdership buys.
//   - Concurrency is fenced by the single effect-permit lane (AllocateEffect
//     below), not by holdership. Refusing a non-holder narrowed nothing and
//     merely made the user's second browser useless.
//
// Holdership still routes DAEMON-initiated work — offers and handoffs — and
// the whitelist's default arm still refuses those from a non-holder.
//
// This is NOT the same as pdfGrabAbandonSession's originator scoping, which
// is correct and stays: "the session that created this grab" is a per-grab
// fact carried by the effect request id, not a claim on the bridge. Do not
// collapse the two — dropping holdership here says nothing about who may
// cancel a grab already in flight.
func (b *Bridge) pdfGrabRefusalReason(sessionID string) string {
	if b == nil {
		return grabReasonInternal
	}
	session := b.sessionByID(sessionID)
	if session == nil {
		return grabReasonNoSession
	}
	if session.Outdated ||
		!slices.Contains(session.Features, pdfGrabV1Feature) ||
		!slices.Contains(session.Features, effectPermitFeature) {
		return grabReasonExtensionOutdated
	}
	return ""
}

// pdfGrab allocates a PDF grab (ADR-0020 Decision 3): a grab id and a
// steering directory under the reserved grabs/ namespace. It never touches
// the requested URL itself — only chrome.downloads.download, steered by the
// extension, ever fetches those bytes, riding the user's own session.
// Allocation is a single daemon-durable transaction: grab row + held PDF
// permit + URL-free authorization event, with the landing directory prepared
// before commit. Steering is returned only for a newly acquired allocation;
// an existing active grab returns "existing" with no steering, and anything
// that cannot be served returns structured "unavailable" carrying both a
// closed-enum reason the UI switches on and a human-readable detail.
// Structured refusal only, never a raw error: an unhandled outcome here
// would decode fine on the extension side but defeat the whole point of a
// closed outcome enum a UI can safely switch on.
func (b *Bridge) pdfGrab(ctx context.Context, sessionID string, request *protocol.PdfGrabRequestPayload) ([]json.RawMessage, error) {
	if reason := b.pdfGrabRefusalReason(sessionID); reason != "" {
		detail := "papio is not connected to this browser yet"
		if reason != grabReasonNoSession {
			detail = "this browser extension cannot save PDFs yet; reload it to finish updating"
		}
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", reason, detail)
	}
	if b.grabs == nil {
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonNotConfigured, "papio is not set up to save PDFs")
	}
	if !slices.Contains(b.Features, pdfGrabV1Feature) {
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonDaemonUnsupported, "this papio cannot save PDFs")
	}
	if b.adoptionLatchUnhealthy() {
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonAdoptionUnhealthy,
			"the downloads folder is not responding (macOS privacy consent?); try again after granting access")
	}
	normalizedHost := strings.ToLower(strings.TrimSpace(request.Host))
	normalizedHost = strings.TrimSuffix(normalizedHost, ".")
	safetyDomain := hostSafetyDomain("pdf_grab", "https://"+normalizedHost)
	if safetyDomain == "" {
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonTabUnusable, "this tab is not a PDF papio can save")
	}
	title := truncate(request.Title, 500)
	var preparedDir string
	prepare := func(grabID string) error {
		dir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), grabsDirName, grabID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		preparedDir = dir
		return nil
	}
	g, err := b.grabs.AllocateEffect(ctx, normalizedHost, title, b.epoch, safetyDomain, b.now().Add(b.actionExpiry()), prepare, request.RequestID)
	if err != nil {
		if preparedDir != "" {
			_ = os.RemoveAll(preparedDir)
		}
		if errors.Is(err, grab.ErrBusy) || errors.Is(err, job.ErrEffectPermitBusy) {
			return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonBusy, "papio is already busy with another download; try again in a moment")
		}
		log.Printf("papio: pdf grab allocation failed: %v", err)
		return b.pdfGrabRefusal(request.RequestID, "", "unavailable", grabReasonInternal, "could not start this download")
	}
	if g.Outcome == "existing" {
		return b.pdfGrabRefusal(request.RequestID, g.ID, "existing", "", "")
	}
	return b.pdfGrabSteeringResult(request.RequestID, g.ID, preparedDir)
}

// pdfGrabRefusal answers one grab request without allocating anything.
// reason is the closed-enum machine tag; the protocol permits it only on the
// "unavailable" and "not_supported" outcomes, so "existing" passes "".
func (b *Bridge) pdfGrabRefusal(requestID, grabID, outcome, reason, detail string) ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgPdfGrabResult, "", protocol.PdfGrabResultPayload{
		RequestID: requestID, GrabID: grabID, Outcome: outcome, Reason: reason, Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) pdfGrabSteeringResult(requestID, grabID, preparedDir string) ([]json.RawMessage, error) {
	cleanup := true
	defer func() {
		if cleanup && preparedDir != "" {
			_ = os.RemoveAll(preparedDir)
		}
	}()
	frame, err := b.frame(protocol.MsgPdfGrabResult, "", protocol.PdfGrabResultPayload{
		RequestID: requestID, GrabID: grabID, Outcome: "steering",
		SteeringPath: "papio/" + grabsDirName + "/" + grabID + "/",
	})
	if err != nil {
		return nil, err
	}
	cleanup = false
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) pdfGrabStatus(ctx context.Context, request *protocol.PdfGrabStatusRequestPayload) ([]json.RawMessage, error) {
	result := protocol.PdfGrabStatusResultPayload{
		RequestID: request.RequestID,
		GrabID:    request.GrabID,
	}
	if b.grabs == nil {
		result.Outcome = "unavailable"
		result.Detail = "papio is not set up to save PDFs"
	} else {
		g, err := b.grabs.Get(ctx, request.GrabID)
		if err != nil {
			log.Printf("papio: pdf grab status lookup failed: %v", err)
			result.Outcome = "unavailable"
			result.Detail = "could not read grab status"
		} else if g == nil {
			result.Outcome = "not_found"
		} else {
			result.State = string(g.State)
			result.Outcome = g.Outcome
			result.Detail = g.Detail
			result.JobID = g.JobID
		}
	}
	frame, err := b.frame(protocol.MsgPdfGrabStatusResult, "", result)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

func (b *Bridge) pdfGrabAbandon(ctx context.Context, request *protocol.PdfGrabAbandonRequestPayload) ([]json.RawMessage, error) {
	return b.pdfGrabAbandonWith(ctx, request, "", func() error {
		return b.grabs.MarkAbandoned(ctx, request.GrabID, "The PDF grab download was interrupted")
	})
}

// pdfGrabAbandonSession cancels a grab on behalf of the browser that started
// it. The admission test is the same capability check the grab path uses —
// holdership is irrelevant here too — and the originator fence lives where it
// belongs, in MarkAbandonedForRequest: only the effect request id that
// allocated a capture may release its occupancy.
func (b *Bridge) pdfGrabAbandonSession(ctx context.Context, sessionID string, request *protocol.PdfGrabAbandonRequestPayload) ([]json.RawMessage, error) {
	if b.pdfGrabRefusalReason(sessionID) != "" {
		// "unavailable", not "conflict": a conflict names a durable grab
		// state, and this refusal reads no grab row at all. The old code
		// emitted conflict with an empty state, which fails the outbound
		// self-validation the frame builder runs — so the refusal did not
		// refuse one request, it errored out of Sync and disconnected the
		// browser.
		frame, err := b.frame(protocol.MsgPdfGrabAbandonResult, "", protocol.PdfGrabAbandonResultPayload{
			RequestID: request.RequestID, GrabID: request.GrabID,
			Outcome: "unavailable", Detail: "only the browser that started this download can cancel it",
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{frame}, nil
	}
	return b.pdfGrabAbandonWith(ctx, request, request.RequestID, func() error {
		err := b.grabs.MarkAbandonedForRequest(ctx, request.GrabID, request.RequestID, b.epoch, "The PDF grab download was interrupted")
		if err == nil {
			return nil
		}
		// The fenced path deliberately covers only occupying captures, so a
		// capture whose permit is already settled falls through to here with
		// nothing in flight. Refusing it would leave the row — and every retry
		// for that paper, allocation being idempotent per host and title — waiting on
		// AbandonStaleAwaiting's cutoff hours later for no safety gain.
		return b.grabs.MarkAbandonedUnoccupied(ctx, request.GrabID, "The PDF grab download was interrupted")
	})
}

func (b *Bridge) pdfGrabAbandonWith(ctx context.Context, request *protocol.PdfGrabAbandonRequestPayload, expectedRequestID string, abandon func() error) ([]json.RawMessage, error) {
	result := protocol.PdfGrabAbandonResultPayload{
		RequestID: request.RequestID,
		GrabID:    request.GrabID,
		State:     string(grab.StateAbandoned),
		Outcome:   "abandoned",
	}
	if b.grabs == nil {
		result.State = ""
		result.Outcome = "unavailable"
		result.Detail = "papio is not set up to save PDFs"
	} else if err := abandon(); err != nil {
		// A lost acknowledgment is ambiguous: inspect the durable row before
		// deciding whether the retry succeeded, found nothing, or conflicts
		// with a later grab lifecycle transition.
		g, getErr := b.grabs.Get(ctx, request.GrabID)
		if getErr != nil {
			log.Printf("papio: pdf grab abandon lookup failed: %v", getErr)
			result.State = ""
			result.Outcome = "unavailable"
			result.Detail = "could not inspect grab"
		} else if g == nil {
			result.State = ""
			result.Outcome = "not_found"
		} else if g.State == grab.StateAbandoned && (expectedRequestID == "" || g.EffectRequestID == expectedRequestID) {
			result.State = string(grab.StateAbandoned)
			result.Outcome = "abandoned"
			result.Detail = g.Detail
		} else {
			result.State = string(g.State)
			result.Outcome = "conflict"
			result.Detail = "this download has already finished, or a different browser started it"
		}
	}
	frame, err := b.frame(protocol.MsgPdfGrabAbandonResult, "", result)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// pdfGrabSuggest serves pdf_grab_suggest_request, the inbox's read-only
// "which pending job is this?" ranking for one parked, DOI-less grab. It is
// a thin wire adapter over Bridge.SuggestGrabCandidates — the same method
// `papio grabs suggest` and the grabs.suggest RPC already call — so the
// ranking is built from exactly one decision path, never redecided per
// surface.
//
// SuggestGrabCandidates re-validates the quarantined PDF through the worker
// on every call (see its own doc comment for why a cached report would be
// wrong), which is materially slower than an ordinary frame handler. This
// needs no concurrency cap of its own: handle() runs inside Sync's frame
// loop with b.mu held for the whole call — the one exception, the
// materialization scheduler, explicitly releases and re-acquires it around
// its own query — so at most one frame, across every session the bridge
// holds and not just this one, is ever being processed at a time. A burst
// of suggest_request polls cannot pile up concurrent validations; each one
// queues behind the mutex like every other bridge RPC.
//
// Every routine failure SuggestGrabCandidates reports through its closed
// Outcome enum is carried straight to the wire outcome; nothing here may
// become a raw Go error, or a stale inbox click would tear down the whole
// browser session (the reviewPreview footgun AGENTS.md documents).
//
// Frame size: a maximal legal payload — 25 suggestions each at their field
// caps (64-char job_id, 500-char title, 213-char DOI, 500-char reason, 16
// evidence strings at 300 chars) plus 8 document identifiers at their own
// caps — encodes to roughly 156 KiB, leaving real headroom under
// MaxBrowserMessageBytes (256 KiB). b.frame self-validates against that cap
// on every call, so this never needs its own truncation path the way
// triageSnapshot's frameFits/item-count backoff does; TestSyncResponseFitsResultCap's
// existing worst-case bound already treats "one solicited response" as a
// full 256 KiB regardless of type, so it covers this frame type without a
// type-specific addition.
func (b *Bridge) pdfGrabSuggest(ctx context.Context, request *protocol.PdfGrabSuggestRequestPayload) ([]json.RawMessage, error) {
	result := b.SuggestGrabCandidates(ctx, request.GrabID, int(request.Limit))
	identifiers := make([]protocol.PdfGrabDocumentIdentifier, 0, len(result.DocumentIdentifiers))
	for _, id := range result.DocumentIdentifiers {
		identifiers = append(identifiers, protocol.PdfGrabDocumentIdentifier{
			Kind: id.Kind, Value: id.Value, Source: id.Source,
		})
	}
	// Authors deliberately does not travel on this wire (see
	// PdfGrabSuggestionRow's doc comment): title/year/DOI plus the verdict
	// and its evidence are the minimum a human needs to tell candidates
	// apart, not a mirror of the internal RPC shape.
	suggestions := make([]protocol.PdfGrabSuggestionRow, 0, len(result.Suggestions))
	for _, row := range result.Suggestions {
		suggestions = append(suggestions, protocol.PdfGrabSuggestionRow{
			JobID: row.JobID, Title: row.Title, Year: int64(row.Year), DOI: row.DOI,
			Verdict: row.Verdict, Reason: row.Reason, Evidence: row.Evidence,
		})
	}
	frame, err := b.frame(protocol.MsgPdfGrabSuggestResponse, "", protocol.PdfGrabSuggestResponsePayload{
		RequestID: request.RequestID, GrabID: result.GrabID, Outcome: result.Outcome, Detail: result.Detail,
		DocumentIdentifiers: identifiers, Suggestions: suggestions, Truncated: result.Truncated,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// pdfGrabConfirm serves pdf_grab_confirm_request, the write counterpart to
// pdf_grab_suggest_request: the operator has picked one ranked candidate in
// the inbox and this binds the parked grab to it. It is a thin wire adapter
// over Bridge.ConfirmGrabCandidate — the same method `papio grabs confirm`
// and the grabs.confirm RPC already call, through the same
// MarkBoundToJobFenced fence autonomous binding uses — so there is exactly
// one place that decision is made.
//
// See pdfGrabSuggest's doc comment for why this needs no concurrency cap of
// its own: it runs under the same Sync-held b.mu.
//
// Every routine failure ConfirmGrabCandidate reports, including the
// refused_identity veto, is carried straight to the wire outcome; nothing
// here may become a raw Go error.
func (b *Bridge) pdfGrabConfirm(ctx context.Context, request *protocol.PdfGrabConfirmRequestPayload) ([]json.RawMessage, error) {
	result := b.ConfirmGrabCandidate(ctx, request.GrabID, request.JobID)
	frame, err := b.frame(protocol.MsgPdfGrabConfirmResponse, "", protocol.PdfGrabConfirmResponsePayload{
		RequestID: request.RequestID, GrabID: result.GrabID, JobID: result.JobID, Outcome: result.Outcome, Detail: result.Detail,
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
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
	if _, err := b.adoptOutsideSessionLock(ctx, jobID, pending.Filename, provenance, pending.Producer); err != nil {
		_ = b.recordAdoptionDeferred(ctx, jobID, pending.Filename, err)
		if errors.Is(err, job.ErrAdoptNotAwaiting) || errors.Is(err, job.ErrCandidateNotEligible) {
			// Permanent, not environmental — see the same case in the
			// download-complete handler. Retrying can only be refused again.
			delete(b.deliveryContexts, key)
			delete(b.pendingDownloads, key)
		}
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

// errArtifactSuperseded reports bytes that lost the insert-only artifact-winner
// decision for the job attempt. The delivery is not a transport failure and not
// a deferrable environmental failure: another materialization already produced
// the artifact for this attempt, so these bytes must never be attached.
var errArtifactSuperseded = errors.New("artifact winner already decided for this job attempt")

// adoptionPath resolves the reported download strictly under the job's adoption
// directory. The filename has already passed protocol validation (no path
// separators); this adds IsLocal and a symlink-resolved prefix guard before
// app-side confinement. It walks cfg.AdoptionRoots so a file that landed in
// the superseded <data_dir>/adoptions root before the default moved under the
// browser's download directory is still adoptable.
func (b *Bridge) adoptionPath(jobID, filename string) (string, error) {
	if !filepath.IsLocal(filename) {
		return "", fmt.Errorf("adoption filename %q is not a local name", filename)
	}
	var rootErr error
	for _, base := range b.cfg.AdoptionRoots() {
		realRoot, err := filepath.EvalSymlinks(filepath.Join(base, jobID))
		if err != nil {
			rootErr = err
			continue
		}
		full := filepath.Join(realRoot, filename)
		rel, err := filepath.Rel(realRoot, full)
		if err != nil || rel != filename || strings.Contains(rel, "..") {
			return "", fmt.Errorf("adoption path escapes %s", realRoot)
		}
		return full, nil
	}
	return "", fmt.Errorf("adoption root unavailable: %w", rootErr)
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// artifactFence carries the winner decision for one delivery across
// validation. digest is the exact bytes weighed; claim/candidate are nil when
// the attempt has no institutional history and the legacy path applies.
type artifactFence struct {
	attempt   int64
	digest    string
	claim     *job.MaterializationClaim
	candidate *job.BrowserCandidate
	governed  bool // the attempt is institutional; winning is mandatory
	replay    bool // this exact artifact already won; do not CAS again
}

// weighArtifact decides whether these bytes may be adopted, WITHOUT committing
// the winner. Committing before validation was a real defect: rejected bytes
// (an HTML interstitial, a wrong-work PDF) permanently won the attempt, and the
// correct file that landed afterwards hashed differently and was refused as
// superseded. The winner must describe bytes that passed validation, so the CAS
// moved to commitArtifact and only the refusal decision happens here.
//
// "Institutional" is any attempt with materialization history, not merely a
// currently live claim: a claim that expired or was abandoned before the file
// landed must not silently fall back to unfenced legacy adoption.
func (b *Bridge) weighArtifact(ctx context.Context, jobID, filename string) (*artifactFence, error) {
	if b.jobs == nil {
		return &artifactFence{}, nil
	}
	full, err := b.adoptionPath(jobID, filename)
	if err != nil {
		return nil, err
	}
	digest, err := fileDigest(full)
	if err != nil {
		return nil, err
	}
	if b.materializationGenerationUnavailable {
		return &artifactFence{digest: digest}, nil
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		return nil, err
	}
	decided, hasWinner, err := b.jobs.ArtifactWinner(ctx, jobID, attempt)
	if err != nil {
		return nil, err
	}
	claim, candidate, err := b.jobs.LiveMaterializationClaimForJob(ctx, jobID, attempt, b.epoch)
	if err != nil {
		return nil, err
	}
	governed := hasWinner || claim != nil
	if candidate == nil {
		// No live claim: fall back to the attempt's candidate history, which
		// both decides that the attempt is institutional and attributes the
		// winner when the claim that produced the bytes has already expired.
		historic, histErr := b.jobs.CandidateForAttempt(ctx, jobID, attempt)
		if histErr != nil {
			return nil, histErr
		}
		if historic != nil {
			candidate = historic
			governed = true
		}
	}
	// Hash every file before adoption, including ordinary generic/direct
	// effects. Their permits are still exact producer correlations even
	// though they do not create an institutional artifact winner.
	// Hashing is intentionally performed before any adoption. The digest is
	// the only byte-level key that can authorize producer recovery.
	if !governed {
		return &artifactFence{attempt: attempt, digest: digest}, nil
	}
	fence := &artifactFence{
		attempt: attempt, digest: digest,
		claim: claim, candidate: candidate, governed: true,
	}
	if hasWinner {
		// Replaying the artifact this attempt already committed is idempotent;
		// different bytes are a late producer and must not be attached.
		if decided.SHA256 != digest {
			if eventErr := b.jobs.RecordEvent(ctx, jobID, "browser.artifact_superseded", nil); eventErr != nil {
				log.Printf("papio: recording superseded artifact: %v", eventErr)
			}
			return nil, errArtifactSuperseded
		}
		fence.replay = true
	}
	return fence, nil
}

func artifactProducerIdentity(p *protocol.ArtifactProducerPayload) *job.ArtifactProducerIdentity {
	if p == nil {
		return nil
	}
	producer := &job.ArtifactProducerIdentity{
		Kind:           job.EffectKind(p.EffectKind),
		DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal,
		Strategy: p.Strategy, Revision: p.Revision,
		ClaimID: p.ClaimID, BindingID: p.BindingID,
		EffectOrdinal:          p.EffectOrdinal,
		InstitutionalRequestID: p.InstitutionalRequestID,
	}
	return producer
}

func artifactProducersMatch(a, b *job.ArtifactProducerIdentity) bool {
	if a == nil || b == nil || a.Kind != b.Kind ||
		a.DriveAttemptID != b.DriveAttemptID || a.Strategy != b.Strategy ||
		a.Revision != b.Revision || a.ClaimID != b.ClaimID ||
		a.BindingID != b.BindingID ||
		a.InstitutionalRequestID != b.InstitutionalRequestID {
		return false
	}
	if (a.Ordinal == nil) != (b.Ordinal == nil) ||
		(a.EffectOrdinal == nil) != (b.EffectOrdinal == nil) {
		return false
	}
	if a.Ordinal != nil && *a.Ordinal != *b.Ordinal {
		return false
	}
	return a.EffectOrdinal == nil || *a.EffectOrdinal == *b.EffectOrdinal
}

func (b *Bridge) recoverArtifactProducerExact(ctx context.Context, jobID, filename, digest string) (*job.ArtifactProducerIdentity, error) {
	// Durable filename+SHA correlation is authoritative, including after a
	// daemon restart. Callers that need cleanup must see ambiguity rather than
	// silently treating it as an unrelated artifact.
	return b.jobs.ArtifactProducerForArtifact(ctx, jobID, filename, digest)
}

func (b *Bridge) recoverArtifactProducer(ctx context.Context, jobID, filename, digest string, supplied *job.ArtifactProducerIdentity) *job.ArtifactProducerIdentity {
	producer, err := b.recoverArtifactProducerExact(ctx, jobID, filename, digest)
	if err != nil {
		log.Printf("papio: recovering artifact producer correlation: %v", err)
		return nil
	}
	if producer == nil || (supplied != nil && !artifactProducersMatch(supplied, producer)) {
		return nil
	}
	return producer
}

// persistArtifactCorrelation records the exact bytes and producer tuple
// before adoption. The initial download_complete event may have been emitted
// before Chrome's file rename, so this second durable observation is what
// allows a restart sweep to repair a settled winner without guessing from a
// job or filename.
func (b *Bridge) persistArtifactCorrelation(ctx context.Context, jobID, filename, digest string, producer *job.ArtifactProducerIdentity) error {
	if producer == nil || digest == "" {
		return nil
	}
	return b.jobs.S.AppendEvent(ctx, jobID, "browser.download_complete", map[string]any{
		"filename": filename,
		"sha256":   digest,
		"producer": producer,
	})
}

// commitArtifact records the winner for bytes that have just passed validation,
// and settles only the exact effect producer. The winner and occupancy release
// share one SQLite transaction; a restart can recover the producer from the
// pre-adoption download event and repair an already committed winner.
func (b *Bridge) commitArtifact(
	ctx context.Context,
	jobID, filename string,
	fence *artifactFence,
	supplied *job.ArtifactProducerIdentity,
) error {
	if fence == nil {
		return nil
	}
	producer := b.recoverArtifactProducer(ctx, jobID, filename, fence.digest, supplied)
	if !fence.governed {
		if producer != nil {
			settled, err := b.jobs.SettleArtifactProducer(ctx, jobID, *producer)
			if err != nil {
				log.Printf("papio: settling exact ungoverned artifact producer: %v", err)
			} else if settled {
				b.reofferPending[jobID] = true
			}
		}
		return nil
	}
	if fence.candidate != nil {
		_, won, settled, err := b.jobs.CommitArtifactWinnerAndProducer(ctx, job.ArtifactWinner{
			JobID: jobID, JobAttemptRevision: fence.attempt, CandidateID: fence.candidate.ID,
			BrowserHolderGeneration: b.epoch, SHA256: fence.digest,
		}, producer)
		switch {
		case errors.Is(err, job.ErrMaterializationStale):
			// An uncorrelated artifact still needs a live claim to establish a
			// winner. If exact filename+SHA producer evidence appeared between
			// recovery and commit, release only that historical occupancy;
			// never mutate the stale claim or current offer state.
			lateProducer, recoveryErr := b.recoverArtifactProducerExact(ctx, jobID, filename, fence.digest)
			if recoveryErr != nil {
				log.Printf("papio: late artifact producer recovery refused: %v", recoveryErr)
				return nil
			}
			if lateProducer == nil {
				return nil
			}
			if _, settleErr := b.jobs.SettleArtifactProducer(ctx, jobID, *lateProducer); settleErr != nil {
				log.Printf("papio: late artifact producer settlement refused: %v", settleErr)
			}
			return nil
		case err != nil:
			log.Printf("papio: recording atomic artifact winner for %s: %v", jobID, err)
			if eventErr := b.jobs.RecordEvent(ctx, jobID, "browser.artifact_unfenced",
				map[string]any{"job_attempt_revision": fence.attempt}); eventErr != nil {
				log.Printf("papio: recording unfenced adoption: %v", eventErr)
			}
		case !won:
			if eventErr := b.jobs.RecordEvent(ctx, jobID, "browser.artifact_superseded", nil); eventErr != nil {
				log.Printf("papio: recording superseded artifact: %v", eventErr)
			}
		case settled:
			b.reofferPending[jobID] = true
		}
	} else if producer != nil {
		settled, err := b.jobs.SettleArtifactProducer(ctx, jobID, *producer)
		if err != nil {
			log.Printf("papio: settling exact artifact producer: %v", err)
		} else if settled {
			b.reofferPending[jobID] = true
		}
	}
	if fence.claim == nil || fence.candidate == nil {
		return nil
	}
	if err := b.jobs.SettleMaterialization(ctx, fence.claim.ID, fence.claim.BindingID,
		int64(b.epoch), fence.candidate.InstitutionProfileRevision); err != nil &&
		!errors.Is(err, job.ErrMaterializationStale) &&
		!errors.Is(err, job.ErrMaterializationConflict) {
		log.Printf("papio: settling materialization after adoption: %v", err)
	}
	return nil
}

// ingestAdoptedFile is the single institutional-aware entry point for every
// browser-delivered file, correlated or swept. SweepAdoptions used to call
// b.adopt directly, so a file that landed during daemon downtime or after a
// lost download_complete was adopted with no winner at all — the timer path
// is precisely the one that runs when correlation was missed.
func (b *Bridge) ingestAdoptedFile(
	ctx context.Context,
	jobID, filename string,
	provenance *app.BrowserDeliveryContext,
	producer *job.ArtifactProducerIdentity,
) (int64, error) {
	fence, err := b.weighArtifact(ctx, jobID, filename)
	if err != nil {
		return 0, err
	}
	if err := b.persistArtifactCorrelation(ctx, jobID, filename, fence.digest, producer); err != nil {
		return 0, err
	}
	var candidateID int64
	if provenance != nil {
		candidateID, err = b.adoptWithContext(ctx, jobID, filename, provenance)
	} else {
		candidateID, err = b.adopt(ctx, jobID, filename)
	}
	if err != nil {
		return candidateID, err
	}
	if commitErr := b.commitArtifact(ctx, jobID, filename, fence, producer); commitErr != nil {
		return candidateID, commitErr
	}
	return candidateID, nil
}

// adoptOutsideSessionLock runs validation without blocking unrelated browser
// syncs. The adoption service leases the durable job state before validation,
// so releasing the in-memory session lock cannot admit a competing adoption.
// The caller must hold b.mu; it is held again before this method returns.
func (b *Bridge) adoptOutsideSessionLock(
	ctx context.Context,
	jobID, filename string,
	provenance *app.BrowserDeliveryContext,
	producer *job.ArtifactProducerIdentity,
) (int64, error) {
	b.mu.Unlock()
	defer b.mu.Lock()
	return b.ingestAdoptedFile(ctx, jobID, filename, provenance, producer)
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

const profileEvidenceTTL = 30 * time.Minute

// uncorrelatedEvidenceQuarantine is how long after a profile revision changes
// an uncorrelated observation is refused. A frame with no job correlation
// carries no proof of which revision produced it, so immediately after an
// authority edit there is no way to tell a fresh observation of the new
// identity from a buffered one describing the old. Refusing briefly is the
// fail-closed reading; correlated frames are unaffected because they carry
// their candidate's revision.
const uncorrelatedEvidenceQuarantine = 2 * time.Minute

// recordProfileEvidence persists one institutional-session observation and
// reports whether it was accepted. The observation is fenced to the current
// browser holder generation and to the exact profile revision it was produced
// under; receipt time, not producer time, controls expiry.
//
// For a job-correlated observation the produced-under revision is the revision
// snapshotted on that job's browser candidate, looked up through the attempt's
// candidate HISTORY. Using the "current candidate" query here was a silent
// hole: that query hides a candidate whose profile revision is no longer
// current, so a frame buffered across a profile edit found nothing, fell back
// to the revision live at receipt, and was stored as though the new identity
// had been observed. The whole point of the fence is that it must not be.
//
// Callers that mutate gates or authentication leases MUST check the accepted
// result: acting on a rejected observation reintroduces the same promotion one
// layer up.
// The returned storedID is the durable profile_evidence key, which differs
// from the caller's observationID because it is hashed with the profile,
// revision and holder. Anything that references the evidence row by foreign
// key must use this value, not the input.
func (b *Bridge) recordProfileEvidence(ctx context.Context, observationID, resolverName, jobID string, verdict job.ProfileEvidenceVerdict, source job.ProfileEvidenceSource, producerObservedAt string) (accepted bool, storedID string, err error) {
	if b.jobs == nil {
		return false, "", nil
	}
	if b.materializationGenerationUnavailable {
		return false, "", errors.New("browser holder generation is unavailable")
	}
	profile, profileErr := b.jobs.InstitutionProfileByConfiguredName(ctx, resolverProfileKey(resolverName))
	if profileErr != nil || profile == nil || profile.TombstonedAt != "" {
		return false, "", profileErr
	}
	observedRevision := profile.Revision
	correlated := strings.TrimSpace(jobID) != ""
	if correlated {
		attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, jobID)
		if attemptErr != nil {
			return false, "", attemptErr
		}
		candidate, candidateErr := b.jobs.CandidateForAttempt(ctx, jobID, attempt)
		if candidateErr != nil {
			return false, "", candidateErr
		}
		if candidate != nil {
			observedRevision = candidate.InstitutionProfileRevision
		}
	} else if profile.Revision > 1 {
		// Only an authority CHANGE is ambiguous. A profile at its first
		// revision has no superseded identity for a buffered frame to be
		// describing, and quarantining that case would refuse warm evidence
		// for the whole window after every daemon start, when profiles are
		// reconciled fresh.
		if changed, parseErr := time.Parse(time.RFC3339Nano, profile.UpdatedAt); parseErr == nil &&
			b.now().UTC().Sub(changed.UTC()) < uncorrelatedEvidenceQuarantine {
			return false, "", nil
		}
	}
	received := b.now().UTC()
	if strings.TrimSpace(producerObservedAt) == "" {
		producerObservedAt = received.Format(time.RFC3339Nano)
	}
	idParts := []string{
		profile.ID, strconv.FormatInt(observedRevision, 10),
		strconv.FormatInt(b.epoch, 10), string(verdict), string(source),
	}
	if strings.TrimSpace(observationID) == "" {
		idParts = append(idParts, producerObservedAt)
	} else {
		idParts = append(idParts, observationID)
	}
	sum := sha256.Sum256([]byte(strings.Join(idParts, "\x00")))
	observationID = hex.EncodeToString(sum[:])
	recordErr := b.jobs.RecordProfileEvidence(ctx, job.ProfileEvidenceObservation{
		ObservationID: observationID, BrowserHolderGeneration: b.epoch,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: observedRevision,
		Verdict: verdict, Source: source, ProducerObservedAt: producerObservedAt,
		DaemonReceivedAt: received.Format(time.RFC3339Nano),
		ExpiresAt:        received.Add(profileEvidenceTTL).Format(time.RFC3339Nano),
	})
	if errors.Is(recordErr, job.ErrProfileEvidenceStale) {
		// The observation describes a superseded profile identity. Discarding
		// it is the fence, not a transport failure — but the caller must not
		// then act on it, so this is reported as not accepted rather than as
		// success.
		return false, "", nil
	}
	if recordErr != nil {
		return false, "", recordErr
	}
	return true, observationID, nil
}

func evidenceVerdict(value string) job.ProfileEvidenceVerdict {

	switch strings.TrimSpace(value) {
	case "warm_verified", "warm", "fresh_auth":
		return job.ProfileEvidenceWarmVerified
	case "auth_returned":
		return job.ProfileEvidenceAuthReturned
	case "signed_out", "signed-out", "logged_out":
		return job.ProfileEvidenceSignedOut
	case "unknown":
		return job.ProfileEvidenceUnknown
	default:
		return job.ProfileEvidenceInconclusive
	}
}
func (b *Bridge) reserveAuthenticationEntry(ctx context.Context, resolverName, jobID string) error {
	profile, err := b.jobs.InstitutionProfileByConfiguredName(ctx, resolverProfileKey(resolverName))
	if err != nil || profile == nil || profile.TombstonedAt != "" || profile.AuthenticationClaimID == "" {
		return err
	}
	leaseID := evidenceObservationID("authentication_entry", profile.AuthenticationClaimID,
		jobID, strconv.FormatInt(b.epoch, 10))
	_, err = b.jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
		AuthenticationClaimID:   profile.AuthenticationClaimID,
		LeaseID:                 leaseID,
		OwnerID:                 jobID,
		BrowserHolderGeneration: b.epoch,
		LeaseUntil:              b.now().UTC().Add(profileEvidenceTTL),
	})
	if errors.Is(err, job.ErrAuthenticationEntryLeaseBusy) {
		return nil
	}
	return err
}

func evidenceObservationID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// recordAuth appends a timing-only auth event. The AuthPayload structurally
// cannot carry a URL, host, title, query, or fragment, so an identity-provider
// address cannot enter the event stream through this path.
// profilesSharingOneClaim keeps the origin-scoped attribution set only when
// every configured profile on that origin belongs to the SAME authentication
// claim. One claim is one human sign-in entry, so a session observed at that
// origin is a fact about that entry and holds for each profile grouped under
// it. Two claims behind one origin are two entries, and evidence for one is
// never evidence for the other: that case returns nothing and the caller
// fails closed. A profile with no live (untombstoned) row is dropped rather
// than treated as a second claim.
func (b *Bridge) profilesSharingOneClaim(ctx context.Context, names []string) ([]string, error) {
	claim := ""
	var profiles []string
	for _, name := range names {
		key := resolverProfileKey(name)
		profile, err := b.jobs.InstitutionProfileByConfiguredName(ctx, key)
		if err != nil {
			return nil, err
		}
		if profile == nil || profile.TombstonedAt != "" || profile.AuthenticationClaimID == "" {
			continue
		}
		if claim == "" {
			claim = profile.AuthenticationClaimID
		} else if claim != profile.AuthenticationClaimID {
			return nil, nil
		}
		profiles = append(profiles, key)
	}
	return profiles, nil
}

// sessionEvidence records the first attributable profile observation within
// each throttle window, then applies activity/reoffer side effects. Distinct
// profiles remain independently attributable within one sync.
//
// An origin hint attributes to every profile that origin serves, but ONLY
// while those profiles share one authentication claim - the daemon's identity
// for one human sign-in entry (surface-lifecycle-plan.md's corrected
// cardinality rule: "a claim may group profiles sharing one human entry",
// and resolving one claim never asserts evidence for another). Two distinct
// claims behind one origin are two human entries, so that frame stays
// unattributable and fails closed exactly as before.
//
// Resolving the hint to a single profile instead made a shared origin
// ambiguous, and the operator's own institution is routinely configured twice
// - once at the top level, once named so a job can request it - so every
// uncorrelated frame for their own library was dropped: a real sign-in
// recorded nothing and released nothing (measured live 2026-08-20).
func (b *Bridge) sessionEvidence(ctx context.Context, p *protocol.SessionEvidencePayload, msgIDs ...string) error {
	if b.lastSessionEvidenceAt == nil {
		b.lastSessionEvidenceAt = map[string]time.Time{}
	}
	if b.reofferRanThisSync == nil {
		b.reofferRanThisSync = map[string]bool{}
	}
	hinted := strings.TrimSpace(p.OriginHint) != ""
	wantedProfiles := []string{resolverProfileKey("")}
	if hinted {
		profiles, err := b.profilesSharingOneClaim(ctx, b.cfg.ResolverProfilesForOrigin(p.OriginHint))
		if err != nil {
			return err
		}
		wantedProfiles = profiles
	}
	now := b.now()
	msgID := ""
	if len(msgIDs) > 0 {
		msgID = msgIDs[0]
	}
	// Recorded profiles drive the side effects. A throttled or unprovable
	// profile is not an error and does not veto its siblings: each profile
	// carries its own evidence row, throttle window and reoffer pin.
	var recorded []string
	for _, profile := range wantedProfiles {
		if last := b.lastSessionEvidenceAt[profile]; !last.IsZero() {
			age := now.Sub(last)
			if age >= 0 && age < sessionEvidenceThrottle {
				continue
			}
		}
		obsID := evidenceObservationID("session_evidence", msgID, profile, p.Evidence, p.At)
		accepted, _, err := b.recordProfileEvidence(ctx, obsID, profile, "", evidenceVerdict(p.Evidence), job.ProfileEvidenceProbe, p.At)
		if err != nil {
			return err
		}
		if !accepted {
			// Uncorrelated and unprovable against the current revision. It is
			// not recorded, so it must not release parked work either.
			continue
		}
		b.lastSessionEvidenceAt[profile] = now
		recorded = append(recorded, profile)
	}
	if len(recorded) == 0 {
		return nil
	}
	if err := b.jobs.S.AppendEvent(ctx, "", "browser.session_evidence", nil); err != nil {
		return err
	}
	for _, profile := range recorded {
		if b.reofferRanThisSync[profile] {
			continue
		}
		if err := b.reofferInstitutionalSiblingsForEvidence(ctx, profile, hinted); err != nil {
			return err
		}
		b.reofferRanThisSync[profile] = true
	}
	return nil
}

// reofferInstitutionalSiblingsForEvidence chooses an open institutional
// handoff as the source for the existing sibling reoffer routine. The caller
// has already attributed the observation, so this takes the resolved profile
// key: both the source and every candidate must belong to it. An origin-less
// frame keeps its narrower authority below.
func (b *Bridge) reofferInstitutionalSiblingsForEvidence(ctx context.Context, wantedProfile string, hinted bool) error {
	if !hinted {
		for profile, sourceJobID := range b.reofferSourceJobID {
			if profile != resolverProfileKey("") && sourceJobID != "" {
				// An origin-less frame cannot retire a named-profile pin or
				// prove that the default institution is the authenticated one.
				return nil
			}
		}
	}
	if sourceJobID := b.reofferSourceJobID[wantedProfile]; sourceJobID != "" {
		return b.reofferInstitutionalSiblings(ctx, sourceJobID)
	}
	handoffs, _, err := b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	if err != nil {
		return err
	}
	var fallback string
	for _, item := range handoffs {
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
func (b *Bridge) reofferProfileForSource(ctx context.Context, sourceJobID string) (string, bool, error) {
	for profile, pinned := range b.reofferSourceJobID {
		if pinned == sourceJobID {
			return profile, true, nil
		}
	}
	handoffs, _, err := b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	if err != nil {
		return "", false, err
	}
	for _, item := range handoffs {
		if item.Row.ID == sourceJobID && item.Action.RequiresAuth {
			return resolverProfileKey(item.Row.Policy.Resolver), true, nil
		}
	}
	return "", false, nil
}

func (b *Bridge) upsertProfileGate(ctx context.Context, observationKey, resolverName, jobID string, gateType job.HumanGateType, status job.HumanGateStatus, detail string) error {
	if b.jobs == nil {
		return nil
	}
	profile, err := b.jobs.InstitutionProfileByConfiguredName(ctx, resolverProfileKey(resolverName))
	if err != nil || profile == nil || profile.TombstonedAt != "" {
		return err
	}
	scopeClass := string(job.HumanGateScopeInstitutionProfile)
	profileFence := strconv.FormatInt(profile.Revision, 10)
	scopeKey := profile.ID + "\x00" + profileFence
	claimID := ""
	if gateType == job.HumanGateLogin || gateType == job.HumanGateMFA || gateType == job.HumanGateCaptchaOrSecurity {
		claimID = profile.AuthenticationClaimID
		if claimID != "" {
			scopeClass = string(job.HumanGateScopeAuthenticationClaim)
			// Authentication ownership and its one attention surface are
			// shared by claim across profile revisions.
			scopeKey = claimID
		}
	}
	current, err := b.jobs.CurrentHumanGateObservations(ctx, scopeClass, scopeKey)
	if err != nil {
		return err
	}
	if status == job.HumanGateResolved {
		for _, currentGate := range current {
			if currentGate.GateType != gateType || currentGate.Status != job.HumanGateOpen ||
				(!slices.Contains(currentGate.DependentJobIDs, jobID) && !slices.Contains(currentGate.ClaimMemberJobIDs, jobID)) {
				continue
			}
			currentGate.Status = job.HumanGateResolved
			if err := b.jobs.ResolveHumanGateObservation(ctx, currentGate); err != nil && !errors.Is(err, job.ErrConflict) {
				return err
			}
			return nil
		}
		return nil
	}
	// The observation key must identify the OCCURRENCE, not just the job.
	// This early return is what makes exact replay idempotent, and it fires on
	// the id alone regardless of the row's status — so any two genuinely
	// distinct occurrences that hash alike collapse into one, and the second
	// silently fails to reopen a resolved gate. That happened: auth_pending
	// frames without elapsed_ms all keyed on (kind, job) alone, so after a
	// login resolved the gate, the next sign-out for that job was discarded
	// and every sibling stayed parked with no attention surface. Callers now
	// mix the per-frame msg_id in; keep it that way.
	id := evidenceObservationID("human_gate", observationKey, string(gateType), scopeClass, scopeKey)
	for _, row := range current {
		if row.GateType == gateType && row.ID == id {
			return nil
		}
	}
	var revision int64 = 1
	for _, row := range current {
		if row.GateType == gateType && row.ObservationRevision >= revision {
			revision = row.ObservationRevision + 1
		}
	}
	if detail == "" {
		detail = "{}"
	}
	return b.jobs.UpsertHumanGateObservation(ctx, job.HumanGateObservation{
		ID: id, GateType: gateType, ScopeClass: scopeClass, ScopeKey: scopeKey,
		InstitutionProfileID: profile.ID, AuthenticationClaimID: claimID,
		// ClaimMemberJobIDs identifies the full claim and its deterministic
		// owner; dependent siblings are derived by the gate authority.
		ClaimMemberJobIDs:   []string{jobID},
		ObservationRevision: revision, Status: status, DetailJSON: detail,
		CreatedAt: b.now().UTC().Format(time.RFC3339Nano), UpdatedAt: b.now().UTC().Format(time.RFC3339Nano),
	})
}

func (b *Bridge) resolveProfileGatesForJob(ctx context.Context, observationKey, resolverName, jobID string, gateTypes ...job.HumanGateType) error {
	for _, gateType := range gateTypes {
		if err := b.upsertProfileGate(ctx, observationKey, resolverName, jobID,
			gateType, job.HumanGateResolved, `{"source":"browser_progress"}`); err != nil {
			return err
		}
	}
	return nil
}

// renewMaterializationLease extends the live claim for a job the human is
// demonstrably still working. RenewMaterializationClaim had no production
// caller, so a login, MFA prompt or CAPTCHA that outlived the action expiry
// let reconciliation abandon the claim underneath the user: the eventual
// callback arrived stale, the candidate went back to eligible, and the bytes
// that finally landed could not be fenced because the store refuses a winner
// to a holder that can no longer prove it owns the effect.
//
// Authentication traffic is the precise signal — it is exactly the slow,
// human-paced interaction that outruns the lease.
func (b *Bridge) renewMaterializationLease(ctx context.Context, jobID string) {
	if b.jobs == nil || strings.TrimSpace(jobID) == "" || b.materializationGenerationUnavailable {
		return
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		return
	}
	claim, _, err := b.jobs.LiveMaterializationClaimForJob(ctx, jobID, attempt, int64(b.epoch))
	if err != nil || claim == nil {
		return
	}
	renewErr := b.jobs.RenewMaterializationClaim(ctx, claim.ID, int64(b.epoch), b.now().Add(b.actionExpiry()))
	if renewErr != nil && !errors.Is(renewErr, job.ErrMaterializationStale) {
		log.Printf("papio: renewing materialization lease for %s: %v", jobID, renewErr)
	}
}

func (b *Bridge) recordAuth(ctx context.Context, msg *protocol.BrowserMessage) error {

	kind := "browser.auth_pending"
	if msg.Type == protocol.MsgAuthReturned {
		kind = "browser.auth_returned"
	}
	if b.reofferRanThisSync == nil {
		b.reofferRanThisSync = map[string]bool{}
	}

	detail := map[string]any{}
	elapsed := ""
	if p := msg.Payload.(*protocol.AuthPayload); p.ElapsedMS != nil {
		detail["elapsed_ms"] = *p.ElapsedMS
		elapsed = strconv.FormatInt(*p.ElapsedMS, 10)
	}
	if err := b.jobs.S.AppendEvent(ctx, msg.JobID, kind, detail); err != nil {
		return err
	}
	row, err := b.jobs.Get(ctx, msg.JobID)
	if err != nil {
		return err
	}
	if job.Terminal(row.State) {
		return nil
	}
	// Authentication traffic proves the human is still on this job, so the
	// materialization lease must not expire underneath them.
	b.renewMaterializationLease(ctx, msg.JobID)
	resolverName := resolverProfileKey(row.Policy.Resolver)
	if msg.Type == protocol.MsgAuthReturned {
		// The GATE id must identify the occurrence, so it carries msg_id. The
		// EVIDENCE id must not: profile_evidence is append-only with no pruning,
		// and auth_pending arrives with an empty payload and can toggle several
		// times per tab, so keying it per frame grows the table with browsing
		// activity instead of with distinct facts.
		accepted, _, err := b.recordProfileEvidence(ctx, evidenceObservationID("auth_returned", msg.JobID, elapsed), resolverName, msg.JobID, job.ProfileEvidenceAuthReturned, job.ProfileEvidenceAuthReturn, b.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		if !accepted {
			// The observation describes a superseded profile identity. Resolving
			// the login gate or promoting the authentication lease on it would
			// assert that the CURRENT identity is authenticated, which is the
			// promotion the evidence fence exists to prevent.
			return nil
		}
		if err := b.upsertProfileGate(ctx, evidenceObservationID("auth_returned", msg.MsgID, msg.JobID, elapsed), resolverName, msg.JobID, job.HumanGateLogin, job.HumanGateResolved, `{"source":"auth_returned"}`); err != nil {
			return err
		}
		if profile, profileErr := b.jobs.InstitutionProfileByConfiguredName(ctx, resolverName); profileErr == nil && profile != nil && profile.AuthenticationClaimID != "" {
			if lease, found, leaseErr := b.jobs.GetAuthenticationEntryLease(ctx, profile.AuthenticationClaimID); leaseErr == nil && found &&
				lease.State == job.AuthenticationEntryLeaseReserved && lease.OwnerID == msg.JobID &&
				lease.BrowserHolderGeneration == int64(b.epoch) {
				if evidence, evidenceFound, evidenceErr := b.jobs.CurrentProfileEvidence(ctx, profile.ID, profile.Revision, int64(b.epoch)); evidenceErr == nil && evidenceFound {
					if convertErr := b.jobs.ConvertAuthenticationEntryLeaseToHuman(ctx, profile.AuthenticationClaimID, lease.LeaseID, msg.JobID, int64(b.epoch), evidence); convertErr != nil &&
						!errors.Is(convertErr, job.ErrAuthenticationEntryLeaseDenied) && !errors.Is(convertErr, job.ErrAuthenticationEntryLeaseStale) {
						return convertErr
					}
				}
			}
		}
		if err := b.resolveProfileGatesForJob(ctx,
			evidenceObservationID("auth_returned", msg.MsgID, msg.JobID, elapsed), resolverName, msg.JobID,
			job.HumanGateMFA, job.HumanGateCaptchaOrSecurity); err != nil {
			return err
		}
	} else {
		accepted, _, err := b.recordProfileEvidence(ctx, evidenceObservationID("auth_pending", msg.JobID, elapsed), resolverName, msg.JobID, job.ProfileEvidenceUnknown, job.ProfileEvidenceAuthReturn, b.now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
		if err := b.upsertProfileGate(ctx, evidenceObservationID("auth_pending", msg.MsgID, msg.JobID, elapsed), resolverName, msg.JobID, job.HumanGateLogin, job.HumanGateOpen, `{"source":"auth_pending"}`); err != nil {
			return err
		}
		if err := b.reserveAuthenticationEntry(ctx, resolverName, msg.JobID); err != nil {
			return err
		}
	}
	if msg.Type != protocol.MsgAuthReturned {
		return nil
	}
	if b.reofferSourceJobID == nil {
		b.reofferSourceJobID = map[string]string{}
	}
	profile, ok, err := b.reofferProfileForSource(ctx, msg.JobID)
	if err != nil {
		return err
	}
	if !ok || b.reofferRanThisSync[profile] {
		return nil
	}
	if err := b.reofferInstitutionalSiblings(ctx, msg.JobID); err != nil {
		return err
	}
	b.reofferRanThisSync[profile] = true
	return nil
}

// handoffQuiescedByEvidence reads what each accepted drive of this action
// actually did and reports whether it has burned its fruitless-epoch budget,
// appending the one audit event that makes the decision visible.
//
// Both the ordinary offer drain and the institutional reoffer release MUST
// consult this, not `HumanAction.Quiesced` alone. Age and fruitlessness are
// different failures: the verified field incident aged only 3.07 days into
// QuiesceAfter's seven-day fence while being offered 38 times with zero
// terminal outcomes. A path that filters on age and then overrides the
// fruitless gate launders a permanently-dead action past it — measured live
// 2026-08-21, where four already-quiesced papers held the entire four-slot
// reoffer budget across 424 releases while 58 healthy papers behind them were
// never volunteered once.
func (b *Bridge) handoffQuiescedByEvidence(
	ctx context.Context,
	id string,
	action job.HumanAction,
) (bool, int, error) {
	events, err := b.jobs.Events(ctx, id)
	if err != nil {
		return false, 0, err
	}
	state := job.ProjectHandoffOfferState(events, action.CreatedAt, b.now())
	if !state.Quiesced {
		return false, state.FruitlessEpochs, nil
	}
	// Only an audit newer than the last repair counts as already-recorded: a
	// paper whose streak was reset and then genuinely re-quiesced must say so
	// again, or the repair would silence every future verdict for it.
	audited := false
	for _, ev := range events {
		switch kind, _ := ev["kind"].(string); kind {
		case job.HandoffEpochsResetEvent:
			audited = false
		case "browser.handoff_quiesced":
			audited = true
		}
	}
	if audited {
		return true, state.FruitlessEpochs, nil
	}
	if err := b.jobs.S.AppendEvent(ctx, id, "browser.handoff_quiesced",
		map[string]any{"reason": "fruitless_drive_limit", "drive_epochs": state.FruitlessEpochs}); err != nil {
		return false, state.FruitlessEpochs, err
	}
	return true, state.FruitlessEpochs, nil
}

// reofferInstitutionalSiblings lets poll reopen only the handoffs that a
// returned institutional session can actually unlock. The caller holds b.mu.
func (b *Bridge) reofferInstitutionalSiblings(ctx context.Context, sourceJobID string) error {
	if b.holder == nil || b.now().Sub(b.holder.LastSyncAt) > sessionStaleAfter {
		return nil
	}
	if b.reofferSourceJobID == nil {
		b.reofferSourceJobID = map[string]string{}
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
	sourceProfile := ""
	if source != nil {
		sourceProfile = resolverProfileKey(source.Policy.Resolver)
	}
	profile := sourceProfile
	if profile == "" {
		for candidateProfile, pinned := range b.reofferSourceJobID {
			if pinned == sourceJobID {
				profile = candidateProfile
				break
			}
		}
	}
	if profile == "" {
		return nil
	}
	pinnedSource := b.reofferSourceJobID[profile]
	if source == nil {
		if pinnedSource != sourceJobID {
			return nil
		}
	} else {
		if pinnedSource != "" && pinnedSource != sourceJobID {
			return nil
		}
		if pinnedSource == "" && sourceActionID != 0 {
			b.reofferSourceJobID[profile] = sourceJobID
		}
		if sourceActionID != 0 {
			b.authReleased[sourceActionID] = true
		}
	}
	if b.reofferSourceJobID[profile] != sourceJobID {
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
		// fresh institutional login must not resurrect a handoff nobody has
		// completed. An explicit `papio actions open` still does. Both
		// quiescence rules apply — the cheap age one here, the evidence one
		// below once the cheap filters have narrowed the set.
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
		quiesced, _, err := b.handoffQuiescedByEvidence(ctx, row.ID, action)
		if err != nil {
			return err
		}
		if quiesced {
			delete(b.reofferPending, row.ID)
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
		wasOffered := b.offered[candidate.row.ID]
		if err := b.jobs.RecordEvent(ctx, candidate.row.ID, "browser.handoff_reoffered",
			map[string]any{"reason": "institutional_session_live"}); err != nil {
			return err
		}
		if wasOffered {
			delete(b.offered, candidate.row.ID)
			delete(b.cancelSent, candidate.row.ID)
		} else {
			available--
		}
		b.reofferPending[candidate.row.ID] = true
		b.authReleased[candidate.action.ID] = true
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

func (b *Bridge) updateCaptureLease(ctx context.Context, jobID string) error {
	if b.captureStore == nil || strings.TrimSpace(jobID) == "" {
		return nil
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return err
	}
	first, latest := "", ""
	for _, event := range events {
		if event["kind"] != "browser.page_capture" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if strings.TrimSpace(stringDetail(detail, "scenario")) == "observed" {
			continue
		}
		path := strings.TrimSpace(stringDetail(detail, "path"))
		if path == "" {
			continue
		}
		if first == "" {
			first = path
		}
		latest = path
	}
	if first == "" || latest == "" {
		return nil
	}
	return b.captureStore.UpdateJob(ctx, jobID, first, latest)
}

// providerDetailRedaction removes URL-shaped and credential-shaped substrings
// from extension-supplied free text before it reaches durable storage.
var providerDetailRedaction = regexp.MustCompile(
	`(?i)([a-z][a-z0-9+.-]*://\S+|[?&][^\s=]{1,64}=[^\s&]+|\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?:/\S*)?|\b[A-Za-z0-9_-]{24,}\.[A-Za-z0-9_-]{8,}\b|\b[A-Fa-f0-9]{32,}\b)`)

// redactProviderDetail keeps a provider's free text diagnosable without making
// the durable event a place where a URL, query token, or credential can land.
// The text is adapter-authored and travels through the extension, so it is
// untrusted input: truncation alone does not sanitise it. Closed outcome codes
// carry the meaning; this string is only ever a human hint.
func redactProviderDetail(detail string) string {
	cleaned := providerDetailRedaction.ReplaceAllString(strings.TrimSpace(detail), "[redacted]")
	if len(cleaned) > 200 {
		cleaned = cleaned[:200]
	}
	return cleaned
}

// suppressCurrentRoute records a durable, exactly-keyed suppression for the
// route this job attempt is currently using, so the scheduler's suppression
// anti-join stops re-selecting the identical tuple. The producer side of
// route_suppressions had no caller at all, which left that anti-join with no
// rows to consume: a route that proved it had no entitlement, or that answered
// with a challenge, was re-offered on the next pass exactly as before.
//
// The key is the candidate's own tuple, so a rediscovery pass that mints a new
// route revision is unaffected — only the route that actually failed is fenced.
// A job with no institutional candidate is not institutional and is skipped.
func (b *Bridge) suppressCurrentRoute(ctx context.Context, jobID string, reason job.RouteSuppressionReason, observationID string) error {
	if b.jobs == nil {
		return nil
	}
	attempt, err := b.jobs.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		return err
	}
	candidate, err := b.jobs.CandidateForAttempt(ctx, jobID, attempt)
	if err != nil || candidate == nil {
		return err
	}
	return b.jobs.AddRouteSuppression(ctx, job.RouteSuppression{
		RouteSuppressionKey: job.RouteSuppressionKey{
			JobID: jobID, JobAttemptRevision: candidate.JobAttemptRevision,
			InstitutionProfileID:       candidate.InstitutionProfileID,
			InstitutionProfileRevision: candidate.InstitutionProfileRevision,
			RouteRevision:              candidate.RouteRevision,
			SafetyDomainID:             candidate.SafetyDomainID,
			AdapterRevision:            candidate.AdapterRevision,
			IdentifierStrategy:         candidate.IdentifierStrategy,
		},
		EvidenceObservationID: observationID,
		Reason:                reason,
	})
}

// outcome maps a terminal provider observation onto a policy-legal transition.
func (b *Bridge) outcome(ctx context.Context, jobID, msgID string, p *protocol.ProviderOutcomePayload) (err error) {
	defer func() {
		if b.captureStore == nil {
			return
		}
		row, getErr := b.jobs.Get(ctx, jobID)
		if getErr != nil || row.State == job.StateAwaitingHuman {
			return
		}
		if releaseErr := b.captureStore.ReleaseJob(ctx, jobID); err == nil {
			err = releaseErr
		}
	}()
	sourceExtensionVersion := ""
	if b.holder != nil {
		sourceExtensionVersion = b.holder.ExtensionVersion
	}
	detail := map[string]any{
		"outcome":           p.Outcome,
		"adapter_version":   p.AdapterVersion,
		"detail":            redactProviderDetail(p.Detail),
		"extension_version": sourceExtensionVersion,
	}
	if p.AdapterID != "" {
		detail["adapter_id"] = p.AdapterID
	}
	if err := b.jobs.RecordEvent(ctx, jobID, "browser.provider_outcome", detail); err != nil {
		return err
	}
	rowForEvidence, rowErr := b.jobs.Get(ctx, jobID)
	if rowErr != nil {
		return rowErr
	}
	if job.Terminal(rowForEvidence.State) {
		return nil
	}
	if (p.Outcome == "human_auth_required" || p.Outcome == "terms_acceptance_required") &&
		!rowForEvidence.Work.HasFetchableIdentifier() {
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateUnavailable, string(job.TerminalReasonNoIdentifier))
	}
	verdict := job.ProfileEvidenceInconclusive
	if p.Outcome == "human_auth_required" {
		verdict = job.ProfileEvidenceSignedOut
	}
	outcomeObservationKey := evidenceObservationID("provider_outcome", msgID, jobID, p.Outcome, p.AdapterID, p.AdapterVersion)
	progressObservationKey := evidenceObservationID("provider_progress", msgID, jobID, p.Outcome)
	evidenceAccepted, storedEvidenceID, err := b.recordProfileEvidence(ctx,
		outcomeObservationKey,
		rowForEvidence.Policy.Resolver, jobID, verdict, job.ProfileEvidenceProviderOutcome,
		b.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	// A rejected observation describes a superseded identity, so it must not
	// move gates or the authentication lease on the current one. The job's own
	// routing below still runs: refusing to route would strand the job over an
	// authority question that says nothing about whether the work can proceed.
	if evidenceAccepted {
		switch p.Outcome {
		case "human_auth_required":
			// The explicit login gate remains current.
		case "terms_acceptance_required":
			if err := b.resolveProfileGatesForJob(ctx, progressObservationKey,
				rowForEvidence.Policy.Resolver, jobID, job.HumanGateLogin, job.HumanGateMFA, job.HumanGateCaptchaOrSecurity); err != nil {
				return err
			}
		default:
			if err := b.resolveProfileGatesForJob(ctx, progressObservationKey,
				rowForEvidence.Policy.Resolver, jobID, job.HumanGateLogin, job.HumanGateMFA,
				job.HumanGateCaptchaOrSecurity, job.HumanGateTermsRequired); err != nil {
				return err
			}
		}
		switch p.Outcome {
		case "human_auth_required":
			if err := b.upsertProfileGate(ctx, outcomeObservationKey,
				rowForEvidence.Policy.Resolver, jobID, job.HumanGateLogin, job.HumanGateOpen, `{"source":"provider_outcome"}`); err != nil {
				return err
			}
			if err := b.reserveAuthenticationEntry(ctx, rowForEvidence.Policy.Resolver, jobID); err != nil {
				return err
			}
		case "terms_acceptance_required":
			if err := b.upsertProfileGate(ctx, outcomeObservationKey,
				rowForEvidence.Policy.Resolver, jobID, job.HumanGateTermsRequired, job.HumanGateOpen, `{"source":"provider_outcome"}`); err != nil {
				return err
			}
		case "ui_changed":
			lower := strings.ToLower(p.Detail)
			if strings.Contains(lower, "captcha") || strings.Contains(lower, "security") {
				if err := b.upsertProfileGate(ctx, outcomeObservationKey,
					rowForEvidence.Policy.Resolver, jobID, job.HumanGateCaptchaOrSecurity, job.HumanGateOpen, `{"source":"provider_outcome"}`); err != nil {
					return err
				}
				if err := b.reserveAuthenticationEntry(ctx, rowForEvidence.Policy.Resolver, jobID); err != nil {
					return err
				}
				// A challenge is a property of this route, not of the work, so
				// fence the tuple as well as opening the human gate. Without a
				// suppression the scheduler re-selects the identical route the
				// moment the gate resolves.
				if err := b.suppressCurrentRoute(ctx, jobID, job.RouteSuppressionProviderChallenge, storedEvidenceID); err != nil {
					log.Printf("papio: suppressing challenged route for %s: %v", jobID, err)
				}
			}
		}
	}
	if err := b.updateCaptureLease(ctx, jobID); err != nil {
		return err
	}
	if err := b.recordProviderLatch(ctx, jobID, p); err != nil {
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
		// This exact route proved it cannot serve this work. Fence it before
		// any requeue so a rediscovery pass cannot re-select the same tuple.
		if err := b.suppressCurrentRoute(ctx, jobID, job.RouteSuppressionNoEntitlement, storedEvidenceID); err != nil {
			log.Printf("papio: suppressing route after %s for %s: %v", p.Outcome, jobID, err)
		}
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
		requiresAuth := true
		actions, err := b.jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
		if err != nil {
			return err
		}
		// A missing handoff must not promise access: the user may still need to
		for _, action := range actions {
			if action.Kind == handoffActionKind {
				requiresAuth = action.RequiresAuth
				break
			}
		}
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		// Three genuinely different reasons reach this one action kind, and
		// which one it is decides the inbox task family. Take the answer from
		// structure, never prose: the extension emits ui_changed WITH an
		// adapter_id when a known adapter stopped matching its page (drift),
		// and WITHOUT one when no adapter claimed the page at all. Reading the
		// English sentence instead is how all 27 live manual downloads came to
		// share one instruction.
		detail, diagnosis := "papio reached a different work; find and download the requested PDF yourself", job.DiagnosisReasonWrongWork
		if p.Outcome == "ui_changed" {
			if p.AdapterID != "" {
				detail, diagnosis = "papio could not drive the provider page; download the PDF yourself and papio will adopt it", job.DiagnosisReasonProviderAdapterDrift
			} else {
				detail, diagnosis = "papio has no adapter for this provider yet; download the PDF yourself for now", job.DiagnosisReasonProviderAdapterMissing
				// Copy only: whether a capture was kept decorates the
				// sentence and never selects the diagnosis above.
				if strings.Contains(p.Detail, "A sanitized diagnostic was saved locally") {
					detail += "; a sanitized page diagnostic is saved locally; run 'papio adapter captures' to inspect it"
				}
			}
		}
		// The page, rather than the original paywall, now blocks papio; whether
		// that page needs a sign-in remains the resolved handoff's classification.
		_, err = b.jobs.OpenHumanAction(ctx, jobID, "manual_download", detail,
			job.Access(requiresAuth, "landing_page"), job.WithHumanActionDiagnosis(diagnosis))
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

const providerLatchEventKind = "job.latch"

func routeSafetyDomain(routeRevision string) string {
	routeRevision = strings.TrimSpace(routeRevision)
	if routeRevision == "" {
		return ""
	}
	family, _, ok := strings.Cut(routeRevision, "/")
	if !ok || family == "" {
		return ""
	}
	return "route:" + family
}

func hostSafetyDomain(prefix, target string) string {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return ""
	}
	return prefix + ":" + strings.ToLower(parsed.Hostname())
}

func actionSafetyDomain(cfg config.Config, row job.Row, action job.HumanAction) string {
	if target, ok := app.OABrowserHandoffURL(action.Detail); ok {
		return hostSafetyDomain("oa", target)
	}
	if target, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
		return hostSafetyDomain("delivery", target)
	}
	inst, ok := cfg.InstitutionFor(row.Policy.Resolver)
	if !ok {
		return ""
	}
	return hostSafetyDomain("institution", RouteURL(inst, row.Work))
}

// recordProviderLatch turns provider observations into durable, job-scoped
// circuit-breaker evidence. The provider outcome frame deliberately has no
// host field; when available, the preceding page-capture event supplies the
// landed provider host without widening the strict wire contract.
func (b *Bridge) recordProviderLatch(ctx context.Context, jobID string, p *protocol.ProviderOutcomePayload) error {
	if b == nil || b.jobs == nil || p == nil {
		return nil
	}
	kind := ""
	switch p.Outcome {
	case "wrong_work", "unexpected_effect", "validation_failed", "failed_validation":
		kind = "no_positive_effects"
	case "ui_changed", "unknown":
		kind = "drift"
	default:
		return nil
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return err
	}
	domain := ""
	for i := len(events) - 1; i >= 0; i-- {
		detail, _ := events[i]["detail"].(map[string]any)
		switch events[i]["kind"] {
		case "browser.direct_route":
			if stringDetail(detail, "phase") == "offered" || stringDetail(detail, "phase") == "result" {
				domain = stringDetail(detail, "safety_domain")
				if domain == "" {
					domain = routeSafetyDomain(stringDetail(detail, "route_revision"))
				}
			}
		case "browser.provider_drive_epoch_offered", "browser.provider_drive_epoch_started", "browser.provider_drive_epoch_result":
			domain = stringDetail(detail, "safety_domain")
		case "browser.handoff_offered":
			domain = stringDetail(detail, "safety_domain")
		}
		if domain != "" {
			break
		}
	}
	if domain == "" {
		row, rowErr := b.jobs.Get(ctx, jobID)
		if rowErr != nil {
			return rowErr
		}
		actions, actionErr := b.jobs.ListOpenHumanActionsForJobs(ctx, []string{jobID})
		if actionErr != nil {
			return actionErr
		}
		for _, action := range actions {
			if action.Kind == handoffActionKind {
				domain = actionSafetyDomain(b.cfg, *row, action)
				break
			}
		}
		// An observation without an offered route cannot identify a safety
		// domain. Keep the ordinary provider outcome durable, but do not create
		// a latch that would accidentally suspend unrelated browser routes.
		if domain == "" {
			return nil
		}
	}
	host := providerOutcomeHost(events, p.AdapterID, p.AdapterVersion)
	for _, event := range events {
		if event["kind"] != providerLatchEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["kind"] != kind || stringDetail(detail, "safety_domain") != domain {
			continue
		}
		if kind == "no_positive_effects" ||
			(stringDetail(detail, "adapter_id") == p.AdapterID &&
				stringDetail(detail, "adapter_version") == p.AdapterVersion &&
				stringDetail(detail, "host") == host) {
			return nil
		}
	}
	detail := map[string]any{
		"kind":          kind,
		"safety_domain": domain,
	}
	if kind == "drift" {
		detail["adapter_id"] = p.AdapterID
		detail["adapter_version"] = p.AdapterVersion
		detail["host"] = host
	}
	return b.jobs.RecordEvent(ctx, jobID, providerLatchEventKind, detail)
}

func (b *Bridge) appendProviderNoPositiveLatch(ctx context.Context, jobID, domain string) error {
	if b == nil || b.jobs == nil || strings.TrimSpace(domain) == "" {
		return nil
	}
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event["kind"] != providerLatchEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if stringDetail(detail, "kind") == "no_positive_effects" &&
			stringDetail(detail, "safety_domain") == domain {
			return nil
		}
	}
	return b.jobs.RecordEvent(ctx, jobID, providerLatchEventKind, map[string]any{
		"kind": "no_positive_effects", "safety_domain": domain,
	})
}

// recordAdoptionConclusiveLatch distinguishes a rejected adopted file from
// environmental adoption deferral. The app parks an identity rejection with a
// durable transition reason; only that conclusive reason trips the current
// handoff domain, never an unreadable or still-renaming file.
func (b *Bridge) recordAdoptionConclusiveLatch(ctx context.Context, jobID string) error {
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return err
	}
	domain := ""
	conclusive := false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if event["kind"] == "browser.handoff_offered" {
			domain = stringDetail(detail, "safety_domain")
		}
		if event["kind"] == "job.transition" && stringDetail(detail, "reason") == "wrong_work" {
			conclusive = true
		}
	}
	if !conclusive || domain == "" {
		return nil
	}
	return b.appendProviderNoPositiveLatch(ctx, jobID, domain)
}
func providerOutcomeHost(events []map[string]any, adapterID, adapterVersion string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.page_capture" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		if id := stringDetail(detail, "adapter_id"); id != "" && id != adapterID {
			continue
		}
		if adapterVersion != "" {
			if version := stringDetail(detail, "adapter_version"); version != "" && version != adapterVersion {
				continue
			}
		}
		return strings.ToLower(strings.TrimSpace(stringDetail(detail, "host")))
	}
	return ""
}

func stringDetail(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return value
}
func intDetail(detail map[string]any, key string) int {
	switch value := detail[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return -1
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
// directory and hands it to the app for validation.
func (b *Bridge) adopt(ctx context.Context, jobID, filename string) (int64, error) {
	full, err := b.adoptionPath(jobID, filename)
	if err != nil {
		return 0, err
	}
	return b.svc.AdoptDownloadCandidate(ctx, jobID, full)
}

func (b *Bridge) adoptWithContext(ctx context.Context, jobID, filename string, provenance *app.BrowserDeliveryContext) (int64, error) {
	full, err := b.adoptionPath(jobID, filename)
	if err != nil {
		return 0, err
	}
	return b.svc.AdoptDownloadWithContextCandidate(ctx, jobID, full, provenance)
}

// AdoptionScanDeadline bounds one adoption-directory ReadDir syscall. A
// TCC-protected root (for example a download_adoption_root under
// ~/Downloads on macOS) can make open(2) block in-kernel indefinitely: tccd
// is waiting on a consent decision only an interactive process can supply,
// and papio is a background daemon. 2s is far past any real filesystem
// latency but short enough that one hung scan costs at most one poll tick.
//
// It is a var, not a const, solely so the tests that prove the hung-syscall
// behaviour can compress it: they block a ReadDir seam forever, so at the
// production value each one costs a real 2s of wall clock. Production never
// assigns it.
var AdoptionScanDeadline = 2 * time.Second

// ErrAdoptionScanTimeout marks a ReadDir call that did not return within
// AdoptionScanDeadline — the signature of the TCC consent wall described on
// scanAdoptionDir. Never wrapped, so callers compare it with errors.Is.
var ErrAdoptionScanTimeout = errors.New("adoption directory scan timed out")

// BoundedReadDir runs readDir(dir) — os.ReadDir when readDir is nil — on its
// own goroutine and returns ErrAdoptionScanTimeout if it has not completed
// within AdoptionScanDeadline. Go cannot cancel a syscall already blocked
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
	case <-time.After(AdoptionScanDeadline):
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
// AdoptionScanDeadline and, on a timeout, latches adoption scanning off for
// the whole bridge — not just this job — until the hung call eventually
// returns. A short-circuited or timed-out call reports the same shape of
// error a missing directory does, which every caller here already treats as
// "not adoptable": a scan papio could not complete must never be read as
// evidence a settled file is present (fail-closed adoption semantics). The
// two log lines fire exactly once per transition.
func (b *Bridge) readAdoptionDir(dir string) ([]os.DirEntry, error) {
	b.adoptionScanMu.Lock()
	if b.adoptionScanGate == nil {
		b.adoptionScanGate = make(chan struct{}, 1)
	}
	gate := b.adoptionScanGate
	suspended := b.adoptionScanSuspended
	b.adoptionScanMu.Unlock()
	if suspended {
		return nil, ErrAdoptionScanTimeout // a prior call is still hung; never stack another
	}

	// Single-flight: acquire the gate for the whole lifetime of the
	// underlying call. If the current holder is itself hung, waiting the
	// full deadline here is pointless — it will latch the bridge; report
	// the same fail-closed timeout without touching the latch.
	select {
	case gate <- struct{}{}:
	case <-time.After(AdoptionScanDeadline):
		return nil, ErrAdoptionScanTimeout
	}

	fn := b.readDir
	if fn == nil {
		fn = os.ReadDir
	}
	held := func(d string) ([]os.DirEntry, error) {
		defer func() { <-gate }() // released when the real call returns — timely or hours late
		return fn(d)
	}
	entries, err := BoundedReadDir(dir, held, func([]os.DirEntry, error) {
		b.adoptionScanMu.Lock()
		b.adoptionScanSuspended = false
		b.adoptionScanMu.Unlock()
		log.Printf("papio: adoption scans resumed")
	})
	if errors.Is(err, ErrAdoptionScanTimeout) {
		b.adoptionScanMu.Lock()
		b.adoptionScanSuspended = true
		b.adoptionScanMu.Unlock()
		log.Printf("papio: adoption scans suspended: %s not responding (macOS privacy consent?)", dir)
	}
	return entries, err
}

// adoptionLatchUnhealthy reports whether the adoption-scan latch
// (adoptionScanSuspended) is currently tripped: a prior ReadDir under the
// adoption root missed AdoptionScanDeadline and has not yet returned. It is
// the daemon-side signature of the macOS TCC consent wall AGENTS.md
// documents, and recordAdoptionDeferred uses it to decide whether a failed
// adoption is plausibly that wall (worth a human grant prompt) rather than
// an ordinary transient miss.
func (b *Bridge) adoptionLatchUnhealthy() bool {
	b.adoptionScanMu.Lock()
	defer b.adoptionScanMu.Unlock()
	return b.adoptionScanSuspended
}

// recordAdoptionDeferred appends the durable browser.adoption_deferred event
// for one failed adoption attempt (a completed download whose bytes are not
// yet — or never — going to land in the artifact store) and, only when the
// adoption-scan latch is currently unhealthy, opens or refreshes a
// downloads_access_required human action naming the adoption root.
//
// An ordinary transient defer — the file has not finished writing, a Chrome
// rename race, a confinement violation — leaves the latch healthy and opens
// nothing: those are not fixed by a Full Disk/Files-and-Folders grant, and a
// user should never be sent chasing a permission that was never the
// problem. OpenHumanAction is itself idempotent per (job, kind), so polling
// this on every deferred adoption — download_complete, the periodic
// directory sweep, or the poll-time scan — never opens a second action; it
// just refreshes the one already open. The action resolves the same way
// every other non-advisory action does, on the job's next terminal
// transition (job.Store's transition), which fires the moment adoption
// eventually succeeds.
func (b *Bridge) recordAdoptionDeferred(ctx context.Context, jobID, filename string, cause error) error {
	if err := b.jobs.S.AppendEvent(ctx, jobID, "browser.adoption_deferred",
		map[string]any{"filename": filename, "reason": truncate(cause.Error(), 200)}); err != nil {
		return err
	}
	if !b.adoptionLatchUnhealthy() {
		return nil
	}
	if _, err := b.jobs.OpenHumanAction(ctx, jobID, job.ActionKindDownloadsAccessRequired,
		b.cfg.EffectiveAdoptionRoot(), job.Access(false, "")); err != nil {
		return err
	}
	return nil
}

func (b *Bridge) preserveDeferredAdoption(jobID, filename, stagedPath string) error {
	rejectDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), "rejected", jobID)
	if err := os.MkdirAll(rejectDir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(rejectDir, filename)
	if _, err := os.Stat(dest); err == nil {
		dest = uniqueAdoptionDest(rejectDir, filename)
	}
	return b.moveGrabFile(stagedPath, dest)
}

func (b *Bridge) recoverDeferredAutoBind(ctx context.Context, grabID, jobID string) error {
	if b.jobs == nil {
		return nil
	}
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil || row == nil {
		return nil
	}
	if job.Terminal(row.State) {
		return nil
	}
	// Find the latest deferred filename for this job.
	events, err := b.jobs.Events(ctx, jobID)
	if err != nil {
		return nil
	}
	var target string
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.adoption_deferred" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		if f, _ := detail["filename"].(string); f != "" {
			target = f
			break
		}
	}
	if target == "" {
		return nil
	}
	// Prefer the staged file in the adoption directory; fall back to the
	// rejected/ copy written for terminal jobs.
	dirs := []string{
		filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID),
		filepath.Join(b.cfg.EffectiveAdoptionRoot(), "rejected", jobID),
	}
	var staged string
	for _, d := range dirs {
		candidate := filepath.Join(d, target)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			staged = candidate
			break
		}
		if matches, _ := filepath.Glob(filepath.Join(d, target+"*")); len(matches) > 0 {
			for _, m := range matches {
				if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
					staged = m
					break
				}
			}
			if staged != "" {
				break
			}
		}
	}
	if staged == "" {
		return nil
	}
	// If the file is in rejected/, move it back to the adoption directory
	// before ingestion (adoption requires confinement under the job's dir).
	adoptDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	if filepath.Dir(staged) != adoptDir {
		if err := os.MkdirAll(adoptDir, 0o700); err != nil {
			return nil
		}
		dest := uniqueAdoptionDest(adoptDir, target)
		if err := b.moveGrabFile(staged, dest); err != nil {
			if err := b.copyGrabFile(staged, dest); err != nil {
				return nil
			}
		}
		target = filepath.Base(dest)
	}
	if _, err := b.ingestAdoptedFile(ctx, jobID, target, nil, nil); err != nil {
		_ = b.recordAdoptionDeferred(ctx, jobID, target, err)
		return nil
	}
	// Clean up grab staging if still present; grab row already job_created.
	if grabRow, _ := b.grabs.Get(ctx, grabID); grabRow != nil {
		_ = os.RemoveAll(filepath.Join(b.cfg.EffectiveAdoptionRoot(), "grabs", grabID))
		if grabRow.QuarantinePath != "" {
			_ = os.Remove(grabRow.QuarantinePath)
		}
	}
	return nil
}

// scanAdoptionDir looks for exactly one settled candidate file in an
// adoptable job's adoption directory. Dotfiles (.DS_Store) are invisible; any
// .crdownload/.download marks an in-progress Chrome write and .part a
// Firefox one; either defers the whole scan. A zero-byte file is the browser's
// placeholder target (Firefox creates the final name empty while streaming
// into name.part), never a settled download, so it defers the scan too. More
// than one visible file is ambiguous and adopts nothing. The returned name
// feeds adopt(), which re-applies full confinement checks. Roots are tried in
// cfg.AdoptionRoots order, so the effective root wins and the drain-only
// legacy root is only consulted when it holds the job's directory.
func (b *Bridge) scanAdoptionDir(_ context.Context, jobID string) (string, bool) {
	for _, root := range b.cfg.AdoptionRoots() {
		if name, ok := b.settledFileIn(filepath.Join(root, jobID)); ok {
			return name, true
		}
	}
	return "", false
}

// settledFileIn is scanAdoptionDir's directory-scan rule, factored out so
// SweepGrabs's per-grab landing directory (ADR-0020) shares the exact same
// settled-file heuristic as ordinary job adoption.
func (b *Bridge) settledFileIn(dir string) (string, bool) {
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
//
// It walks every root in cfg.AdoptionRoots, not just the effective one, so an
// install that was adopting into the superseded <data_dir>/adoptions root
// before the default moved under the browser's download directory keeps
// draining. A per-root error is fatal to the tick the same way it always was.
func (b *Bridge) SweepAdoptions(ctx context.Context) error {
	for _, root := range b.cfg.AdoptionRoots() {
		if err := b.sweepAdoptionsIn(ctx, root); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) sweepAdoptionsIn(ctx context.Context, root string) error {
	entries, err := b.readAdoptionDir(root)
	if errors.Is(err, ErrAdoptionScanTimeout) {
		return nil // root not responding (TCC); latch already logged, skip this tick
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" || e.Name() == grabsDirName {
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
			// The directory is ambiguous (e.g. contains a pre-existing file
			// alongside a deferred auto-bind staging). Try the filename-keyed
			// deferred recovery, which names the exact file the committed claim
			// meant to adopt and bypasses the single-file heuristic.
			if g, _ := b.grabs.ByJobID(ctx, jobID); g != nil && g.State == grab.StateJobCreated {
				_ = b.recoverDeferredAutoBind(ctx, g.ID, jobID)
			}
			continue
		}
		if _, err := b.ingestAdoptedFile(ctx, jobID, name, nil, nil); err != nil {
			if errors.Is(err, errArtifactSuperseded) {
				// Another delivery already won this attempt. Re-scanning would
				// refuse the same bytes every tick, so the job is latched out
				// of the sweep rather than deferred.
				if latchErr := b.recordAdoptionConclusiveLatch(ctx, jobID); latchErr != nil {
					return latchErr
				}
				continue
			}
			if evErr := b.recordAdoptionDeferred(ctx, jobID, name, err); evErr != nil {
				return evErr
			}
		} else if latchErr := b.recordAdoptionConclusiveLatch(ctx, jobID); latchErr != nil {
			return latchErr
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
// must re-supply, is left untouched. So is the grabs/ sibling (ADR-0020): it
// is a reserved namespace of its own, holding one directory per grab id
// rather than per job id, and SweepGrabs owns its lifecycle — treating it as
// an unknown job directory here would try (and, since it is never empty
// while a grab is live, harmlessly fail to) rmdir a live grab every tick.
// Best-effort, idempotent, and safe on a timer.
//
// It runs over every root in cfg.AdoptionRoots, but the drain-only legacy
// root gets a deliberately narrower rule: only a job whose bytes are provably
// in the artifact store (ready or imported) has its directory collected
// there. That root predates the current default and was never swept while it
// was unreachable, so it can hold a file a human downloaded by hand for a job
// that then failed — and an upgrade is the worst possible moment to delete
// the only copy of something on the strength of a state transition made
// before this directory was in scope. Those husks stay, and doctor's
// adoption_root_legacy check tells the operator the folder is theirs to
// remove.
func (b *Bridge) SweepTerminalAdoptions(ctx context.Context) error {
	effective := b.cfg.EffectiveAdoptionRoot()
	for _, root := range b.cfg.AdoptionRoots() {
		collectible := job.Terminal
		if root != effective {
			collectible = artifactSafelyStored
		}
		if err := b.sweepTerminalAdoptionsIn(ctx, root, collectible); err != nil {
			return err
		}
	}
	return nil
}

// artifactSafelyStored reports whether a job's landing bytes are provably
// redundant: the content-addressed artifact store already holds them.
func artifactSafelyStored(state string) bool {
	return state == job.StateReady || state == job.StateImported
}

func (b *Bridge) sweepTerminalAdoptionsIn(ctx context.Context, root string, collectible func(string) bool) error {
	entries, err := b.readAdoptionDir(root)
	if errors.Is(err, ErrAdoptionScanTimeout) {
		return nil // root not responding (TCC); latch already logged, skip this tick
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" || e.Name() == grabsDirName {
			continue
		}
		row, err := b.jobs.Get(ctx, e.Name())
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			continue // store hiccup: not evidence the job is unknown — leave it
		}
		if err != nil || row == nil {
			// Confirmed-unknown dir (prior database era, crashed run): never
			// delete contents a human may need, but an EMPTY stray is pure
			// clutter. os.Remove has rmdir semantics — it fails atomically if
			// a file lands concurrently, so this can never eat a real
			// download.
			_ = os.Remove(filepath.Join(root, e.Name()))
			continue
		}
		if !collectible(row.State) {
			continue
		}
		if g, _ := b.grabs.ByJobID(ctx, e.Name()); g != nil && g.State == grab.StateJobCreated && !artifactSafelyStored(row.State) {
			// Committed auto-bind whose job never reached ready: bytes are
			// in adoption/ or rejected/ and recovery keys on the exact filename.
			// Ready/imported bytes are already in the immutable artifact store.
			if entries, err := b.readAdoptionDir(filepath.Join(root, e.Name())); err == nil && len(entries) > 0 {
				continue
			}
			if entries, err := b.readAdoptionDir(filepath.Join(filepath.Dir(root), "rejected", e.Name())); err == nil && len(entries) > 0 {
				continue
			}
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	return nil
}

// SweepGrabs advances every PDF grab whose settled file has landed in its
// own grabs/<grab-id>/ landing directory (ADR-0020 Decision 3/4),
// independently of whether the extension is connected. It shares
// readAdoptionDir's bounded, latch-aware reader with
// SweepAdoptions/SweepTerminalAdoptions, so a TCC consent wall on the
// adoption root defers grab sweeping exactly the way it defers ordinary
// adoption — a latch-hung root skips this tick entirely, same as those. Like
// those sweeps it walks every root in cfg.AdoptionRoots so a grab whose
// landing directory was minted under the superseded <data_dir>/adoptions root
// still settles; the stale backstop runs once, after every root.
func (b *Bridge) SweepGrabs(ctx context.Context) error {
	if b.grabs == nil {
		return nil
	}
	for _, base := range b.cfg.AdoptionRoots() {
		if err := b.sweepGrabsIn(ctx, filepath.Join(base, grabsDirName)); err != nil {
			return err
		}
	}
	// Run the stale backstop only after scanning/processing landing files so
	// daemon downtime cannot abandon bytes that arrived while it was offline.
	if err := b.grabs.AbandonStaleAwaiting(ctx, time.Now().Add(-staleAwaitingGrabBudget)); err != nil {
		log.Printf("papio: stale PDF grab sweep failed: %v", err)
	}
	return nil
}

func (b *Bridge) sweepGrabsIn(ctx context.Context, root string) error {
	entries, err := b.readAdoptionDir(root)
	if errors.Is(err, ErrAdoptionScanTimeout) {
		return nil // root not responding (TCC); latch already logged, skip this tick
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		g, err := b.grabs.Get(ctx, id)
		if err != nil {
			continue // store hiccup: not evidence the grab is unknown — leave it
		}
		if g == nil {
			// Confirmed-unknown dir: the same hygiene SweepTerminalAdoptions
			// applies to a stray job directory — remove only if empty, never
			// touch contents a human may need. os.Remove has rmdir
			// semantics, so a concurrently landing file can never be eaten.
			_ = os.Remove(filepath.Join(root, id))
			continue
		}
		if g.State != grab.StateAwaitingFile && g.State != grab.StateQuarantined {
			continue // already terminal; nothing left to do here
		}
		var name string
		if g.State == grab.StateAwaitingFile {
			var ok bool
			name, ok = b.settledFileIn(filepath.Join(root, id))
			if !ok {
				continue
			}
		} else if g.State == grab.StateQuarantined {
			// Retry: validated bytes are in temp, not in the landing dir.
			// Leave name empty — processSettledGrab stages temp directly.
			name = ""
		}
		if err := b.processSettledGrab(ctx, g, filepath.Join(root, id), name); err != nil {
			log.Printf("papio: pdf grab %s processing failed: %v", id, err)
		}
	}
	return nil
}

// processSettledGrab runs ADR-0020 Decision 4's identification pipeline on
// one settled grab file: quarantine, structural validation, then
// front-matter DOI extraction. No network fetch is ever needed for the
// artifact itself — MatchIdentity and documentDOIs both read local text.
func (b *Bridge) processSettledGrab(ctx context.Context, g *grab.Grab, dir, name string) error {
	if b.svc == nil || b.svc.Artifacts == nil || b.svc.Validate == nil {
		return errors.New("acquisition service is not configured")
	}
	path := ""
	if name != "" {
		path = filepath.Join(dir, name)
	}
	temp := g.QuarantinePath
	if g.State != grab.StateQuarantined || temp == "" {
		qdir, err := b.svc.Artifacts.QuarantineDir(g.ID)
		if err != nil {
			return err
		}
		temp = filepath.Join(qdir, "grab.pdf")
		if err := copyFile(path, temp); err != nil {
			_ = os.Remove(temp)
			return err
		}
		if err := b.grabs.MarkQuarantined(ctx, g.ID, temp); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	report, err := b.svc.Validate(ctx, temp, "application/pdf", work.Work{})
	if err != nil {
		// Infrastructure failure (worker unavailable, deadline): leave the
		// row in quarantined for the next tick to retry, rather than
		// declaring the file invalid on a bounds failure that says nothing
		// about the PDF itself.
		return err
	}
	active := report.Structural.Encrypted || report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles
	// Encrypted/JS/embedded publisher PDFs are held for review after a job
	// exists (validateCandidate parks unsafe_pdf). Deleting them here as
	// invalid_pdf made Send PDF's grab fallback unusable for the SAGE/T&F
	// files that motivated that park.
	if !active && (!report.Payload.OK || !report.Structural.Valid) {
		_ = os.Remove(temp)
		_ = os.RemoveAll(dir)
		return b.grabs.MarkFailedValidation(ctx, g.ID, "the captured file is not a valid PDF")
	}
	// No target work.Work exists yet, so only the front-matter DOI pattern
	// applies — corroboratingIdentifier's arXiv/PMID markers compare against
	// a KNOWN identifier and do not apply here (see pdf.FrontMatterDOIs).
	dois := pdf.FrontMatterDOIs(report.Text.Excerpt)
	if len(dois) == 0 {
		// AUTONOMOUS CANDIDATE BINDING IS ENABLED (2026-08-18, operator's
		// decision). A settled DOI-less grab is bound to the single pending
		// job it qualifies against, and parks when none does.
		//
		// WHY it was off, and what changed. The identifier gate reads a
		// page-one occurrence of the candidate's identifier as the document
		// identifying ITSELF, and corroboratingIdentifier accepts a CITED DOI
		// exactly as readily. So a journal expansion printing "Extended from
		// DOI <target>" corroborates the target while its own DOI sits past
		// the 1 KiB blind window — nothing contradicts, every gate passes,
		// and a DIFFERENT work is filed under a right citation. That is this
		// project's cardinal failure, silent and permanent, against a park
		// that costs one human decision.
		//
		// Two things now bound that risk, and neither is an argument.
		//
		// First, measurement against the population this path actually sees.
		// A grab exists because a researcher clicked Send PDF with the
		// document open in front of them, so the human has already excluded
		// the wrong-kind-of-document families (supplements, cover sheets,
		// obvious errata) that no predicate reliably catches. That is what
		// makes the operator's own library a representative sample here
		// rather than a convenient one. Over ~9,800 trials at pool sizes
		// 2/5/10/25 across the random, same-author, same-year, title-superset
		// and same-venue-year arms: ZERO wrong binds, per-document one-sided
		// 95% bound 0.94%, and 65 of 318 documents (20.4%) correctly bound —
		// above the 10% viability floor. Pool size is not a risk axis: N=2
		// and N=25 are identical, because a randomly drawn distractor
		// essentially never clears title AND author AND year.
		//
		// Second, the one family that does fail is now named rather than
		// hypothetical. A document printing another work's title, authors,
		// year AND identifier with no correction word is bound as that work
		// 311 times out of 311 in the measurement's synthetic "conjunction"
		// arm. Adversarial review found real instances — an Oxford Academic
		// Editor's Note, an eNeuro "See related article" commentary — and
		// both phrases are now correctionMarkers, so labelled instances park.
		// An UNLABELLED instance remains the one way this rule can still file
		// the wrong paper, and no vocabulary closes it; only the structural
		// work in dev/active/structural-front-matter-parser.md does.
		//
		// What makes that survivable rather than reckless: every bind is
		// committed inside the eligibility fence with a provenance row
		// recording all candidates in order, each one's verdict, the winner's
		// evidence, the rule version, and a digest of exactly what the
		// predicate read — so a wrong bind can be found and reconstructed
		// afterwards instead of being indistinguishable from a right one.
		candidates, err := b.jobs.ListCandidateEligibleJobs(ctx)
		if err != nil {
			return err
		}
		logAbnormalEligibilityPoolSize(g.ID, len(candidates))
		autoBindAttempted := autoBindDecisionEnabled
		autoBindOutcome := grab.AutoBindOutcomeNotAttempted()
		if autoBindDecisionEnabled {
			bound, err := b.attemptAutoBind(ctx, g, dir, name, temp, pdf.BindDocument{Excerpt: report.Text.Excerpt, Metadata: report.Metadata})
			if err != nil {
				return err
			}
			if bound {
				return nil
			}
			autoBindOutcome = grab.AutoBindOutcomeAbstained()
		}
		snap := grab.NewEligibilityPoolSnapshot(candidates, grab.SnapshotPhasePreBind, autoBindDecisionEnabled, autoBindAttempted, autoBindOutcome)
		_ = os.RemoveAll(dir)
		return b.grabs.MarkParkedNoIdentifierWithEligibilitySnapshot(ctx, g.ID, snap)
	}
	var mismatch error
	for _, doi := range dois {
		err := b.createGrabJob(ctx, g, doi, temp, dir, name, report.Text.Excerpt)
		if errors.Is(err, errGrabIdentityMismatch) {
			mismatch = err
			continue
		}
		return err
	}
	if mismatch != nil {
		return b.grabs.MarkParkedNoIdentifier(ctx, g.ID)
	}
	return nil
}

func logAbnormalEligibilityPoolSize(grabID string, poolSize int) {
	if poolSize > 1000 {
		log.Printf("papio: eligibility pool size %d for grab %s exceeds normal bounds", poolSize, grabID)
	}
}

// autoBindDecisionEnabled gates the autonomous candidate-binding decision — see
// the WHY at the call site in processSettledGrab for the measurement that
// authorised turning it on, and for the one failure family it does not close.
//
// It stays a plain var rather than a config knob. The original reason was that
// an unsafe rule must not be switchable back ON from a TOML file; the reason it
// is still not config is the mirror of that, and weaker: nobody has asked for an
// off switch, and a `[browser]` field cannot be added without the strict-config
// deploy dance (AGENTS.md). If an operator ever needs to disable this without a
// rebuild, that is the change to make, and it is a config addition rather than a
// new mechanism. Tests flip it in both directions through the helpers in
// grab_autobind_test.go, so both paths stay exercised.
var autoBindDecisionEnabled = true

// attemptAutoBind runs the candidate-binding decision for one settled DOI-less
// grab and, on success, stages and ingests its validated bytes. It reports
// whether the grab was bound and fully handled; (false, nil) is abstention and
// the caller must park.
//
// Abstention is the default: an empty pool, a tie, a Review verdict, or a
// fence rejection all return (false, nil). The only path that returns true is
// a committed bind.
func (b *Bridge) attemptAutoBind(ctx context.Context, g *grab.Grab, dir, name, temp string, doc pdf.BindDocument) (bool, error) {
	candidates, err := b.jobs.ListCandidateEligibleJobs(ctx)
	if err != nil {
		return false, err
	}
	if len(candidates) == 0 {
		log.Printf("papio: auto-bind abstained for grab %s: no candidates", g.ID)
		return false, nil
	}
	bindCandidates := make([]pdf.BindCandidate, 0, len(candidates))
	for _, c := range candidates {
		bindCandidates = append(bindCandidates, pdf.BindCandidate{
			Key:   c.JobID,
			Work:  c.Work,
			Bound: c.BoundDOIs,
		})
	}
	winner, ok, abstainReason := pdf.SelectAutoBindCandidate(doc, bindCandidates)
	if !ok {
		if abstainReason != "" {
			log.Printf("papio: auto-bind abstained for grab %s: %s (candidates %d)", g.ID, abstainReason, len(bindCandidates))
		}
		return false, nil
	}
	if beforeAutoBindTxForTest != nil {
		if err := beforeAutoBindTxForTest(); err != nil {
			return false, err
		}
	}
	// The provenance is built by the decision that runs INSIDE the binding
	// transaction, never by this pre-transaction one. Carrying provenance
	// across the fence and comparing only the winner's KEY let a pool that
	// changed under an unchanged key commit stale evidence, a stale loser set
	// and a stale candidate count — an audit trail describing a decision that
	// did not happen.
	decide := func(ctx context.Context, tx *sql.Tx) (grab.BindProvenance, error) {
		if beforeAutoBindFenceForTest != nil {
			if err := beforeAutoBindFenceForTest(); err != nil {
				return grab.BindProvenance{}, err
			}
		}
		// Re-read through the transaction's connection only — the pool is a
		// single connection the transaction already holds, so a pool read
		// here would deadlock. A recompute outside the serialization point
		// fences nothing, because another writer can change eligibility
		// between the decision and the CAS.
		fresh, err := job.ListCandidateEligibleJobsTx(ctx, tx)
		if err != nil {
			return grab.BindProvenance{}, err
		}
		freshCandidates := make([]pdf.BindCandidate, 0, len(fresh))
		for _, c := range fresh {
			freshCandidates = append(freshCandidates, pdf.BindCandidate{
				Key:   c.JobID,
				Work:  c.Work,
				Bound: c.BoundDOIs,
			})
		}
		freshWinner, freshOK, _ := pdf.SelectAutoBindCandidate(doc, freshCandidates)
		if !freshOK || freshWinner.Key != winner.Key {
			return grab.BindProvenance{}, grab.ErrFenceRejected
		}
		fencedSnap := grab.NewEligibilityPoolSnapshot(fresh, grab.SnapshotPhaseFencedCommit, true, true, grab.AutoBindOutcomeBound())
		logAbnormalEligibilityPoolSize(g.ID, fencedSnap.PoolSize)
		if err := grab.RecordEligibilitySnapshotTx(ctx, tx, g.ID, grab.SnapshotPhaseFencedCommit, fencedSnap); err != nil {
			return grab.BindProvenance{}, err
		}
		return autoBindProvenance(doc, freshCandidates, freshWinner), nil
	}
	if err := b.grabs.MarkBoundToJobFenced(ctx, g.ID, winner.Key, "job_created", decide); err != nil {
		if errors.Is(err, grab.ErrFenceRejected) {
			log.Printf("papio: auto-bind abstained for grab %s: fence rejected (winner %s)", g.ID, winner.Key)
			return false, nil
		}
		return false, err
	}
	if afterAutoBindCommitForTest != nil {
		if err := afterAutoBindCommitForTest(g.ID, winner.Key); err != nil {
			log.Printf("papio: afterAutoBindCommit hook: %v", err)
		}
	}
	// Private staging is the validated immutable quarantine copy (temp).
	// It is the only source that reaches the winner's adoption directory,
	// and only after the claim is durable. This closes TOCTOU (mutable
	// landing file), the name=="" directory-copy bug, and the pre-commit
	// orphan-adoption window in one: no bytes are visible to
	// SweepAdoptions until the bind has committed.
	jobDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), winner.Key)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		// Claim is durable but staging failed — keep temp for recovery
		// and record a deferred adoption with the intended filename.
		fallback := name
		if fallback == "" || !filepath.IsLocal(fallback) || fallback == "." || fallback == string(filepath.Separator) {
			fallback = "grab.pdf"
		}
		_ = b.recordAdoptionDeferred(ctx, winner.Key, fallback, err)
		return true, nil
	}
	fallbackName := name
	if fallbackName == "" || !filepath.IsLocal(fallbackName) || fallbackName == "." || fallbackName == string(filepath.Separator) {
		fallbackName = "grab.pdf"
	}
	dest := uniqueAdoptionDest(jobDir, fallbackName)
	if err := b.copyGrabFile(temp, dest); err != nil {
		_ = b.recordAdoptionDeferred(ctx, winner.Key, fallbackName, err)
		return true, nil
	}
	boundName := filepath.Base(dest)
	if _, err := b.ingestAdoptedFile(ctx, winner.Key, boundName, nil, nil); err != nil {
		if evErr := b.recordAdoptionDeferred(ctx, winner.Key, boundName, err); evErr != nil {
			return true, evErr
		}
		// Keep temp and the staged dest for filename-keyed recovery;
		// do NOT RemoveAll(dir) yet — the orphan would be cleaned on
		// success only. A terminal winner's directory is normally
		// collectible, so move the staged copy to rejected/ to keep it
		// human-visible when the job cannot be adopted.
		if row, getErr := b.jobs.Get(ctx, winner.Key); getErr == nil && row != nil && job.Terminal(row.State) {
			_ = b.preserveDeferredAdoption(winner.Key, boundName, dest)
		}
		return true, nil
	}
	_ = os.Remove(temp)
	_ = os.RemoveAll(dir)
	return true, nil
}

// autoBindProvenance serialises one 1-of-N binding decision into an audit
// record a human can reconstruct: the ordered candidates that were on the
// table with each one's terminal verdict, the winner's evidence, the exact
// rule version, and a hash pinning the bytes the predicate read.
//
// It stores no scholarly text. The document is identified by digest, not
// copied: the document itself is already durable in the artifact store, and a
// second copy in an audit column would be content nothing manages the lifetime
// of. Losing candidates contribute their machine reason code only — the
// winner's evidence is the one place document-derived strings belong, because
// it is the justification for the row that was written.
//
// The digest covers everything the predicate read, which under
// candidate_auto_bind/3 is the excerpt AND the file's allowlisted embedded
// metadata — see pdf.BindDocument.Digest, which keeps a document with no
// metadata digesting to exactly its excerpt hash so the common case stays
// comparable with rows written by earlier rules.
func autoBindProvenance(doc pdf.BindDocument, candidates []pdf.BindCandidate, winner pdf.CandidateQualification) grab.BindProvenance {
	verdicts := make([]grab.CandidateVerdict, 0, len(candidates))
	for _, c := range candidates {
		q := pdf.QualifyCandidate(doc, c)
		v := grab.CandidateVerdict{JobID: c.Key}
		switch {
		case q.Qualifies:
			v.Verdict = "qualifies"
		case q.Review:
			v.Verdict = "review"
			v.Reason = q.Reason
		default:
			v.Verdict = "rejected"
			v.Reason = q.Reason
		}
		verdicts = append(verdicts, v)
	}
	return grab.BindProvenance{
		Method:               "candidate_auto_bind",
		Rule:                 pdf.CandidateBindingRule,
		Winner:               winner.Key,
		CandidatesConsidered: len(candidates),
		Evidence:             winner.Evidence,
		Candidates:           verdicts,
		ExcerptSHA256:        doc.Digest(),
	}
}

// errGrabIdentityMismatch means this front-matter DOI names a ready bundle
// that MatchIdentity says is a different work. The caller tries the next DOI
// rather than claiming already_owned or creating a duplicate job.
var errGrabIdentityMismatch = errors.New("grab front-matter DOI does not match the ready work")

func uniqueAdoptionDest(dir, filename string) string {
	if filename == "" || !filepath.IsLocal(filename) || filename == "." || filename == string(filepath.Separator) {
		filename = "grab.pdf"
	}
	dest := filepath.Join(dir, filename)
	if _, err := os.Stat(dest); err != nil {
		return dest
	}
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for n := 2; n < 1000; n++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, n, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext))
}

// createGrabJob implements ADR-0020 Decision 4's "identifier found" branch.
// Ledger dedupe (ADR-0010) applies naturally through the same
// CreateRequestForWork every other submission path uses, but ready is terminal
// and therefore invisible to that dedupe. Mirror IdentifyGrab: a ready bundle
// is already_owned and discards the captured bytes, while a live (non-terminal)
// job reuses those bytes via its own adoption directory (job_created).
func (b *Bridge) createGrabJob(ctx context.Context, g *grab.Grab, doi, quarantinePath, landingDir, filename, excerpt string) error {
	if _, readyJobID, _, err := b.canonicalJobStatus(ctx, "doi", doi); err != nil {
		return err
	} else if readyJobID != "" {
		row, err := b.jobs.Get(ctx, readyJobID)
		if err != nil {
			return err
		}
		switch pdf.MatchIdentity(excerpt, row.Work).Result {
		case pdf.IdentityPass:
			// Keep the only copy until the grab row is durably terminal.
			if err := b.grabs.MarkJobCreated(ctx, g.ID, readyJobID, "already_owned"); err != nil {
				return err
			}
			_ = os.Remove(quarantinePath)
			_ = os.RemoveAll(landingDir)
			return nil
		case pdf.IdentityReview:
			// An erratum/comment can print the ready paper's DOI. Do not
			// claim already_owned or discard the capture.
			return b.grabs.MarkParkedNoIdentifier(ctx, g.ID)
		default:
			return errGrabIdentityMismatch
		}
	}
	mode, err := b.cfg.RequireAccessMode()
	if err != nil {
		return err
	}
	result, err := b.jobs.CreateRequestForWork(ctx, job.NewID("wr"), work.Work{DOI: doi}, "", "",
		job.Policy{AccessMode: mode, DesiredVersion: "any", FetchMaxBytes: b.cfg.Fetch.MaxBytes},
		nil, job.Attribution{Principal: job.PrincipalUnknown, Consumer: pdfGrabConsumerPrefix + g.URLHost}, false)
	if err != nil {
		return err
	}
	jobDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), result.JobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return err
	}
	src := filepath.Join(landingDir, filename)
	if _, err := os.Stat(src); err != nil {
		src = quarantinePath
	}
	dest := uniqueAdoptionDest(jobDir, filename)
	if err := b.copyGrabFile(src, dest); err != nil {
		return err
	}
	boundName := filepath.Base(dest)
	if err := b.grabs.MarkJobCreated(ctx, g.ID, result.JobID, "job_created"); err != nil {
		_ = os.Remove(dest)
		return err
	}
	_ = os.Remove(quarantinePath)
	_ = os.RemoveAll(landingDir)
	if _, err := b.ingestAdoptedFile(ctx, result.JobID, boundName, nil, nil); err != nil {
		if evErr := b.recordAdoptionDeferred(ctx, result.JobID, boundName, err); evErr != nil {
			return evErr
		}
	}
	return nil
}

// GrabIdentifyResult is the structured outcome of binding an operator-supplied
// identifier to a quarantined browser grab. Routine refusals stay on this wire
// as outcomes rather than becoming local-RPC failures.
type GrabIdentifyResult struct {
	GrabID string `json:"grab_id"`
	JobID  string `json:"job_id,omitempty"`

	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// GrabConfirmResult is the structured outcome of binding an operator-chosen
// pending job to a parked, DOI-less grab. It sits beside GrabIdentifyResult
// on the same closed wire vocabulary — routine refusals stay outcomes, never
// local-RPC failures — but where IdentifyGrab keys on an IDENTIFIER,
// ConfirmGrabCandidate keys on a JOB the human already picked from
// grabs.suggest, which is required because a pending job need not carry any
// identifier at all.
type GrabConfirmResult struct {
	GrabID string `json:"grab_id"`
	JobID  string `json:"job_id,omitempty"`

	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// GrabSuggestResult is the structured outcome of grabs.suggest: a ranked
// "which pending job is this?" answer for a parked DOI-less grab, built by
// scoring every candidate-eligible job with the SAME predicate and the SAME
// document construction attemptAutoBind uses. It is read-only by
// construction — there is no field here that binds anything — which is what
// lets ranking use signals the acceptance rule itself is deliberately too
// strict to accept on: a bad rank only wastes a human's glance, where a bad
// accept would misfile a paper.
type GrabSuggestResult struct {
	GrabID string `json:"grab_id"`

	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`

	// DocumentIdentifiers is what the FILE says about itself, surfaced so a
	// human is not asked to retype an identifier papio already read out of
	// the PDF's own embedded metadata. This is the "target-aware only,
	// never mint" metadata rule (see internal/pdf/metadata.go) applied to
	// DISPLAY rather than to acceptance: nothing here is compared against a
	// candidate or fed back into QualifyCandidate, so a wrong or aggregator-
	// mangled value costs a glance, not a misfile.
	DocumentIdentifiers []DocumentIdentifier `json:"document_identifiers,omitempty"`

	// Suggestions is the ranked candidate pool. Empty and Outcome=="ok"
	// together mean the grab is genuinely parked with nobody to offer —
	// distinct from every non-ok outcome, which means the ranking could not
	// be computed at all.
	Suggestions []GrabSuggestionRow `json:"suggestions,omitempty"`
	// Truncated reports whether the eligible pool was larger than the
	// effective limit, exactly like grabs.binds' own Truncated: a caller
	// asking for 5 must be able to tell "these are the only 5" from "these
	// are the top 5 of more".
	Truncated bool `json:"truncated,omitempty"`
}

// DocumentIdentifier is one allowlisted embedded-metadata value the
// quarantined file carries about itself, spelled the same way
// work.NormalizeDOI/NormalizeArXiv/NormalizePMID would accept it back from a
// human, so a value shown here is exactly what `grabs identify` needs typed.
type DocumentIdentifier struct {
	Kind   string `json:"kind"`   // "doi", "arxiv", or "pmid"
	Value  string `json:"value"`  // normalized, ready to retype
	Source string `json:"source"` // allowlisted field name, e.g. "xmp/prism:doi"
}

// GrabSuggestionRow is one candidate-eligible job scored against a parked
// grab's bytes. Verdict/Reason/Evidence are CandidateQualification carried
// through verbatim (see pdf.QualifyCandidate) — this row makes no claim
// QualifyCandidate itself did not already make.
type GrabSuggestionRow struct {
	JobID   string   `json:"job_id"`
	Title   string   `json:"title,omitempty"`
	Authors []string `json:"authors,omitempty"`
	Year    int      `json:"year,omitempty"`
	DOI     string   `json:"doi,omitempty"`

	Verdict  string   `json:"verdict"` // "qualifies", "review", or "rejected"
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// grabSuggestDefaultLimit and grabSuggestMaxLimit bound grabs.suggest's
// ranked list the same way grabsBindsDefaultLimit/grabsBindsMaxLimit (see
// internal/api/grabs.go) bound grabs.binds: an unspecified limit still
// returns a usable page, and an oversized one clamps down rather than
// resetting to the default, so asking for more never returns fewer rows
// than asking for less.
const (
	grabSuggestDefaultLimit = 5
	grabSuggestMaxLimit     = 25
)

func clampSuggestLimit(limit int) int {
	switch {
	case limit <= 0:
		return grabSuggestDefaultLimit
	case limit > grabSuggestMaxLimit:
		return grabSuggestMaxLimit
	default:
		return limit
	}
}

// SuggestGrabCandidates answers "which pending job is this?" for one parked
// DOI-less grab: it scores every candidate-eligible job against the parked
// bytes with the production predicate and returns a ranked, human-readable
// list. It is read-only end to end — no state transition, no file movement,
// no bind — because a bad RANK only costs a wasted glance while a bad ACCEPT
// misfiles a paper; see the ranking-vs-acceptance split at the top of
// pdf/candidate_select.go.
//
// A suggestion list is computed fresh on every call rather than persisted,
// because the pending-job pool it scores against changes independently of
// this grab: a job that qualified an hour ago may since have been filed by
// another grab or abandoned by the operator, and a stored suggestion could
// not know that. The parked grab keeps its bytes at QuarantinePath for
// exactly this reason — recomputing is always possible and always current.
//
// The guard order and outcome vocabulary mirror IdentifyGrab, which this
// suggestion is a preview for: the same grab must be found, in the same
// state, with the same bytes on disk, before either RPC can do anything.
func (b *Bridge) SuggestGrabCandidates(ctx context.Context, grabID string, limit int) GrabSuggestResult {
	result := GrabSuggestResult{GrabID: grabID}
	if b == nil || b.grabs == nil || b.jobs == nil || b.svc == nil || b.svc.Validate == nil {
		result.Outcome, result.Detail = "unavailable", "pdf grabs are not configured"
		return result
	}
	g, err := b.grabs.Get(ctx, grabID)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "pdf grab is temporarily unavailable"
		return result
	}
	if g == nil {
		result.Outcome, result.Detail = "unknown_grab", "pdf grab not found"
		return result
	}
	if g.State != grab.StateParkedNoIdentifier {
		result.Outcome, result.Detail = "wrong_state", "pdf grab is not parked awaiting an identifier"
		return result
	}
	if g.QuarantinePath == "" {
		result.Outcome, result.Detail = "failed", "pdf grab has no quarantined file"
		return result
	}
	// Re-run the same structural validation processSettledGrab already ran
	// over these exact bytes, rather than trusting a cached report: nothing
	// persists that report, and the daemon may have restarted since the
	// park. This reproduces the identical pdf.ValidationReport the
	// production decision would build were this grab settling right now —
	// the whole point of reusing it instead of drifting a second decision
	// path that scores different inputs than a real bind would.
	report, err := b.svc.Validate(ctx, g.QuarantinePath, "application/pdf", work.Work{})
	if err != nil {
		// Infrastructure failure (worker unavailable, deadline), exactly
		// like processSettledGrab's own Validate call: it says nothing
		// about whether the file is valid, so "failed" — a terminal
		// verdict on this grab — would be wrong. The caller should retry.
		result.Outcome, result.Detail = "unavailable", "pdf grab could not be re-validated"
		return result
	}
	candidates, err := b.jobs.ListCandidateEligibleJobs(ctx)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "eligible job pool is temporarily unavailable"
		return result
	}
	doc := pdf.BindDocument{Excerpt: report.Text.Excerpt, Metadata: report.Metadata}
	rows := make([]GrabSuggestionRow, 0, len(candidates))
	for _, c := range candidates {
		// QualifyCandidate, deliberately never SelectAutoBindCandidate:
		// SelectAutoBindCandidate answers "may I bind unattended" and
		// abstains the moment more than one candidate qualifies or the
		// pool is ambiguous — exactly the situation a ranked list exists to
		// help a human resolve. QualifyCandidate scores every candidate
		// independently, which a ranking needs and a bind decision must not
		// have (see the file header's 1-of-1-vs-1-of-N distinction).
		q := pdf.QualifyCandidate(doc, pdf.BindCandidate{Key: c.JobID, Work: c.Work, Bound: c.BoundDOIs})
		verdict := "rejected"
		switch {
		case q.Qualifies:
			verdict = "qualifies"
		case q.Review:
			verdict = "review"
		}
		rows = append(rows, GrabSuggestionRow{
			JobID:    c.JobID,
			Title:    c.Work.Title,
			Authors:  c.Work.Authors,
			Year:     c.Work.Year,
			DOI:      c.Work.DOI,
			Verdict:  verdict,
			Reason:   q.Reason,
			Evidence: q.Evidence,
		})
	}
	// Deterministic per the grabs.suggest contract: qualifies before review
	// before rejected; within a tier, more evidence first (a candidate a
	// human can confirm faster belongs first); ties broken by job id so two
	// calls against an unchanged pool always render identically.
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := suggestVerdictRank(rows[i].Verdict), suggestVerdictRank(rows[j].Verdict)
		if ri != rj {
			return ri < rj
		}
		if len(rows[i].Evidence) != len(rows[j].Evidence) {
			return len(rows[i].Evidence) > len(rows[j].Evidence)
		}
		return rows[i].JobID < rows[j].JobID
	})
	limit = clampSuggestLimit(limit)
	if len(rows) > limit {
		rows, result.Truncated = rows[:limit], true
	}
	result.Suggestions = rows
	result.DocumentIdentifiers = extractDocumentIdentifiers(report.Metadata)
	result.Outcome = "ok"
	return result
}

func suggestVerdictRank(verdict string) int {
	switch verdict {
	case "qualifies":
		return 0
	case "review":
		return 1
	default:
		return 2
	}
}

// extractDocumentIdentifiers renders a file's allowlisted embedded metadata
// (pdf.MetadataFields — the same fields QualifyCandidate's gate 5 reads for
// corroboration) as a list of identifiers for DISPLAY.
//
// This is deliberately the one place in this codebase that reads an
// identifier OUT of metadata rather than checking metadata AGAINST one: see
// "TARGET-AWARE ONLY" in internal/pdf/metadata.go, which forbids exactly that
// for acceptance, because a template error or aggregator rewrite would then
// mint a wrong identity with nothing to catch it — the same hazard
// FrontMatterDOIs carries for page-one text. Showing the raw value to a human
// who is about to pick among a list of named candidates is a different
// operation with a different failure mode: at worst they see a value that
// is not theirs and ignore it. Nothing this returns may be compared against a
// candidate or threaded into QualifyCandidate/SelectAutoBindCandidate.
func extractDocumentIdentifiers(fields pdf.MetadataFields) []DocumentIdentifier {
	ordered := make(pdf.MetadataFields, len(fields))
	copy(ordered, fields)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Field != ordered[j].Field {
			return ordered[i].Field < ordered[j].Field
		}
		return ordered[i].Value < ordered[j].Value
	})
	out := make([]DocumentIdentifier, 0, len(ordered))
	for _, f := range ordered {
		kind, value := classifyMetadataIdentifier(f.Value)
		if kind == "" {
			continue
		}
		out = append(out, DocumentIdentifier{Kind: kind, Value: value, Source: f.Field})
	}
	return out
}

// classifyMetadataIdentifier normalizes one raw metadata value the same way
// IdentifyGrab normalizes a human-typed one, so a value surfaced here is
// spelled exactly as `grabs identify`/`grabs confirm` would need it retyped.
// DOI is tried first because every field in identifierFields (metadata.go)
// is a documented DOI container in production practice; arXiv and PMID are
// checked after because dc:identifier/dcterms:identifier are generic Dublin
// Core slots some publishers populate with either instead.
func classifyMetadataIdentifier(raw string) (kind, value string) {
	if doi, err := work.NormalizeDOI(raw); err == nil {
		return "doi", doi
	}
	if id, err := work.NormalizeArXiv(raw); err == nil {
		return "arxiv", id
	}
	if pmid, err := work.NormalizePMID(raw); err == nil {
		return "pmid", pmid
	}
	return "", ""
}

func (b *Bridge) copyGrabFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeOutErr := out.Close()
	closeInErr := in.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeOutErr != nil {
		_ = os.Remove(dst)
		return closeOutErr
	}
	if closeInErr != nil {
		_ = os.Remove(dst)
		return closeInErr
	}
	return nil
}

func (b *Bridge) moveGrabFile(src, dst string) error {
	rename := b.renameFile
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := b.copyGrabFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// IdentifyGrab validates one explicitly typed identifier, checks the ready
// bundle projection before creating any job, then joins or creates the
// canonical job while reusing the quarantined bytes.
func (b *Bridge) IdentifyGrab(ctx context.Context, grabID, kind, raw string) GrabIdentifyResult {
	result := GrabIdentifyResult{GrabID: grabID}
	var target work.Work
	var canonicalKind, canonicalValue string
	var err error
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "doi":
		target.DOI, err = work.NormalizeDOI(raw)
		canonicalKind, canonicalValue = "doi", target.DOI
	case "pmid":
		target.PMID, err = work.NormalizePMID(raw)
		canonicalKind, canonicalValue = "pmid", target.PMID
	case "arxiv":
		target.ArXiv, err = work.NormalizeArXiv(raw)
		canonicalKind, canonicalValue = "arxiv", target.ArXiv
	default:
		result.Outcome, result.Detail = "invalid_identifier", "identifier kind must be doi, pmid, or arxiv"
		return result
	}
	if err != nil {
		result.Outcome, result.Detail = "invalid_identifier", "identifier is malformed"
		return result
	}
	if b == nil || b.grabs == nil || b.jobs == nil {
		result.Outcome, result.Detail = "unavailable", "pdf grabs are not configured"
		return result
	}
	g, err := b.grabs.Get(ctx, grabID)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "pdf grab is temporarily unavailable"
		return result
	}
	if g == nil {
		result.Outcome, result.Detail = "unknown_grab", "pdf grab not found"
		return result
	}
	if g.State != grab.StateParkedNoIdentifier {
		result.Outcome, result.Detail = "wrong_state", "pdf grab is not parked awaiting an identifier"
		return result
	}
	if g.QuarantinePath == "" {
		result.Outcome, result.Detail = "failed", "pdf grab has no quarantined file"
		return result
	}
	_, readyJobID, _, err := b.canonicalJobStatus(ctx, canonicalKind, canonicalValue)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "canonical ownership is temporarily unavailable"
		return result
	}
	if readyJobID != "" {
		if err := os.RemoveAll(filepath.Dir(g.QuarantinePath)); err != nil {
			result.Outcome, result.Detail = "failed", "captured bytes could not be discarded after ready ownership"
			return result
		}
		if err := b.grabs.MarkIdentified(ctx, grabID); err != nil {
			result.Outcome, result.Detail = "conflict", "pdf grab changed before identification"
			return result
		}
		if err := b.grabs.MarkJobCreated(ctx, grabID, readyJobID, "already_owned"); err != nil {
			result.Outcome, result.Detail = "failed", "pdf grab could not be finalized"
			return result
		}
		return GrabIdentifyResult{GrabID: grabID, JobID: readyJobID, Outcome: "already_owned"}
	}
	mode, err := b.cfg.RequireAccessMode()
	if err != nil {
		result.Outcome, result.Detail = "configuration_required", "access mode is not configured"
		return result
	}
	created, err := b.jobs.CreateRequestForWork(ctx, job.NewID("wr"), target, "", "",
		job.Policy{AccessMode: mode, DesiredVersion: "any", FetchMaxBytes: b.cfg.Fetch.MaxBytes},
		nil, job.Attribution{Principal: job.PrincipalUnknown, Consumer: pdfGrabConsumerPrefix + g.URLHost}, false)
	if err != nil {
		result.Outcome, result.Detail = "failed", "canonical job could not be created"
		return result
	}
	result.JobID = created.JobID
	filename := filepath.Base(g.QuarantinePath)
	if !filepath.IsLocal(filename) || filename == "." || filename == string(filepath.Separator) {
		result.Outcome, result.Detail = "failed", "quarantined file name is invalid"
		return result
	}
	jobDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), created.JobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		result.Outcome, result.Detail = "failed", "job adoption directory could not be created"
		return result
	}
	if err := b.moveGrabFile(g.QuarantinePath, filepath.Join(jobDir, filename)); err != nil {
		result.Outcome, result.Detail = "failed", "quarantined file could not be bound to the job"
		return result
	}
	_ = os.Remove(filepath.Dir(g.QuarantinePath))
	// A grab can join an already-live job, including one holding a live
	// institutional claim with a route already issued, so these bytes owe the
	// same winner decision as any other browser-delivered file.
	if _, err := b.ingestAdoptedFile(ctx, created.JobID, filename, nil, nil); err != nil {
		_ = b.recordAdoptionDeferred(ctx, created.JobID, filename, err)
	}
	if err := b.grabs.MarkIdentified(ctx, grabID); err != nil {
		result.Outcome, result.Detail = "conflict", "pdf grab changed before identification"
		return result
	}
	if err := b.grabs.MarkJobCreated(ctx, grabID, created.JobID, "job_created"); err != nil {
		result.Outcome, result.Detail = "failed", "pdf grab could not be finalized"
		return result
	}
	result.Outcome = "job_created"
	return result
}

// ConfirmGrabCandidate binds one parked, DOI-less grab to a job the human
// chose from the grabs.suggest ranking. It is IdentifyGrab's sibling for the
// captures an identifier cannot reach — a pending job need not carry any
// identifier at all — so the human names a JOB instead, and this method
// re-derives the SAME candidate-binding decision attemptAutoBind makes for
// that one job and commits it through the SAME fence (MarkBoundToJobFenced)
// rather than opening a second door to the bind. See the file header split
// between 1-of-1 verification and 1-of-N selection: this is still 1-of-N in
// shape, just with the human doing the selecting instead of
// SelectAutoBindCandidate.
//
// The one thing a human pick cannot override is document identity. The
// bytes' own front matter is re-read and checked with
// pdf.CheckConclusiveIdentity against the CHOSEN job's bound DOIs; a
// blocking verdict refuses with refused_identity and changes nothing. A
// human is authority about WHICH pending paper they meant to click; they are
// not authority about what the bytes actually are, and papio's shipped
// precedence rule — unchanged from the autonomous path — is that extracted
// identity outranks a human pick.
func (b *Bridge) ConfirmGrabCandidate(ctx context.Context, grabID, jobID string) GrabConfirmResult {
	result := GrabConfirmResult{GrabID: grabID, JobID: jobID}
	if b == nil || b.grabs == nil || b.jobs == nil || b.svc == nil || b.svc.Validate == nil {
		result.Outcome, result.Detail = "unavailable", "pdf grabs are not configured"
		return result
	}
	g, err := b.grabs.Get(ctx, grabID)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "pdf grab is temporarily unavailable"
		return result
	}
	if g == nil {
		result.Outcome, result.Detail = "unknown_grab", "pdf grab not found"
		return result
	}
	if g.State != grab.StateParkedNoIdentifier {
		result.Outcome, result.Detail = "wrong_state", "pdf grab is not parked awaiting an identifier"
		return result
	}
	if g.QuarantinePath == "" {
		result.Outcome, result.Detail = "failed", "pdf grab has no quarantined file"
		return result
	}
	// Scope the verb to exactly the pool grabs.suggest ranked: a stale UI
	// showing a job that was since filed, cancelled, or closed must not be
	// able to bind bytes to an arbitrary job id.
	candidates, err := b.jobs.ListCandidateEligibleJobs(ctx)
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "candidate pool is temporarily unavailable"
		return result
	}
	var boundDOIs []string
	found := false
	for _, c := range candidates {
		if c.JobID == jobID {
			boundDOIs, found = c.BoundDOIs, true
			break
		}
	}
	if !found {
		result.Outcome, result.Detail = "unknown_job", "job is not in the candidate-eligible pool"
		return result
	}
	// Re-run the same structural validation processSettledGrab already ran
	// over these exact bytes (see SuggestGrabCandidates just above, which
	// reads this identically for the same reason: nothing persists that
	// report, and the daemon may have restarted since the park).
	report, err := b.svc.Validate(ctx, g.QuarantinePath, "application/pdf", work.Work{})
	if err != nil {
		result.Outcome, result.Detail = "unavailable", "pdf grab could not be re-validated"
		return result
	}
	active := report.Structural.Encrypted || report.Structural.HasJavaScript || report.Structural.HasEmbeddedFiles
	if !active && (!report.Payload.OK || !report.Structural.Valid) {
		result.Outcome, result.Detail = "failed", "the captured file is not a valid PDF"
		return result
	}
	doc := pdf.BindDocument{Excerpt: report.Text.Excerpt, Metadata: report.Metadata}

	// THE ONE REFUSAL THAT MATTERS. The veto is job-scoped (bound to the
	// CHOSEN job's own DOIs, not the whole pool): it is answering "is this
	// the work I said it was", not "is this any work in the pool". A
	// blocking verdict means the document conclusively names a different
	// work than the human picked, and that outranks the pick.
	if veto := pdf.CheckConclusiveIdentity(report.Text.Excerpt, boundDOIs); veto.Blocks() {
		result.Outcome, result.Detail = "refused_identity", veto.Verdict
		return result
	}

	// The decision is recomputed INSIDE the transaction, exactly as
	// attemptAutoBind's decide does, and provenance comes only from this
	// closure — MarkBoundToJobFenced enforces that as its only door.
	// Re-checking the winner against a TX-scoped read (rather than trusting
	// the pre-transaction `found` above) is what makes this a fence and not
	// a check-then-act race: the job could leave the pool between the
	// preflight read and this commit.
	decide := func(ctx context.Context, tx *sql.Tx) (grab.BindProvenance, error) {
		fresh, err := job.ListCandidateEligibleJobsTx(ctx, tx)
		if err != nil {
			return grab.BindProvenance{}, err
		}
		freshCandidates := make([]pdf.BindCandidate, 0, len(fresh))
		winnerIdx := -1
		for i, c := range fresh {
			freshCandidates = append(freshCandidates, pdf.BindCandidate{Key: c.JobID, Work: c.Work, Bound: c.BoundDOIs})
			if c.JobID == jobID {
				winnerIdx = i
			}
		}
		if winnerIdx == -1 {
			return grab.BindProvenance{}, grab.ErrFenceRejected
		}
		return operatorConfirmProvenance(doc, freshCandidates, winnerIdx), nil
	}
	// A parked grab is in parked_no_identifier, which MarkBoundToJobFenced's
	// CAS does not accept — it binds from awaiting_file, quarantined or
	// identified, the states an unparked settlement passes through. Identifying
	// the grab first is exactly what IdentifyGrab does for the same reason, and
	// the ordering is the same: this transition says "a human has answered the
	// identity question", which is true the moment the pick clears the veto,
	// and the fenced bind below is what makes the answer durable.
	if err := b.grabs.MarkIdentified(ctx, grabID); err != nil {
		result.Outcome, result.Detail = "conflict", "pdf grab changed before the pick was applied"
		return result
	}
	if err := b.grabs.MarkBoundToJobFenced(ctx, grabID, jobID, "job_created", decide); err != nil {
		if errors.Is(err, grab.ErrFenceRejected) {
			result.Outcome, result.Detail = "conflict", "job left the candidate pool before the bind committed"
			return result
		}
		result.Outcome, result.Detail = "failed", "pdf grab could not be finalized"
		return result
	}
	// Stage and ingest exactly as attemptAutoBind does after its own
	// commit: the validated quarantine copy is the only source that
	// reaches the winner's adoption directory, and only after the claim is
	// durable. This is the ordering that survives a crash between commit
	// and staging; reusing it here rather than reinventing it is the
	// point. Every failure branch below still reports job_created, because
	// the bind itself already committed — recordAdoptionDeferred is how a
	// staging failure stays visible and recoverable instead of silently
	// losing bytes the row now claims to own.
	jobDir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	fallbackName := filepath.Base(g.QuarantinePath)
	if fallbackName == "" || !filepath.IsLocal(fallbackName) || fallbackName == "." || fallbackName == string(filepath.Separator) {
		fallbackName = "grab.pdf"
	}
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		_ = b.recordAdoptionDeferred(ctx, jobID, fallbackName, err)
		result.Outcome = "job_created"
		return result
	}
	dest := uniqueAdoptionDest(jobDir, fallbackName)
	if err := b.copyGrabFile(g.QuarantinePath, dest); err != nil {
		_ = b.recordAdoptionDeferred(ctx, jobID, fallbackName, err)
		result.Outcome = "job_created"
		return result
	}
	boundName := filepath.Base(dest)
	if _, err := b.ingestAdoptedFile(ctx, jobID, boundName, nil, nil); err != nil {
		if evErr := b.recordAdoptionDeferred(ctx, jobID, boundName, err); evErr != nil {
			result.Outcome, result.Detail = "failed", "pdf grab bound but bytes could not be adopted"
			return result
		}
		if row, getErr := b.jobs.Get(ctx, jobID); getErr == nil && row != nil && job.Terminal(row.State) {
			_ = b.preserveDeferredAdoption(jobID, boundName, dest)
		}
		result.Outcome = "job_created"
		return result
	}
	_ = os.Remove(g.QuarantinePath)
	_ = os.RemoveAll(filepath.Dir(g.QuarantinePath))
	result.Outcome = "job_created"
	return result
}

// operatorConfirmProvenance mirrors autoBindProvenance's shape exactly — the
// same ordered per-candidate verdicts, the same rule and digest — with two
// deliberate differences. Method is "operator_confirm", never
// "candidate_auto_bind": grab.Service.ListAutonomousBinds filters on that
// exact string, and its whole claim is "no human was involved in this
// filing" — a row this function writes would falsify that claim if it used
// the same method name. And Evidence is taken from the CHOSEN candidate even
// when QualifyCandidate did not qualify it, because the audit value of this
// row is precisely that a human overrode a machine that declined or
// abstained; recording the predicate's own reasoning for that candidate is
// what makes the override reconstructable later.
func operatorConfirmProvenance(doc pdf.BindDocument, candidates []pdf.BindCandidate, winnerIdx int) grab.BindProvenance {
	verdicts := make([]grab.CandidateVerdict, 0, len(candidates))
	var winnerQual pdf.CandidateQualification
	for i, c := range candidates {
		q := pdf.QualifyCandidate(doc, c)
		if i == winnerIdx {
			winnerQual = q
		}
		v := grab.CandidateVerdict{JobID: c.Key}
		switch {
		case q.Qualifies:
			v.Verdict = "qualifies"
		case q.Review:
			v.Verdict = "review"
			v.Reason = q.Reason
		default:
			v.Verdict = "rejected"
			v.Reason = q.Reason
		}
		verdicts = append(verdicts, v)
	}
	return grab.BindProvenance{
		Method:               "operator_confirm",
		Rule:                 pdf.CandidateBindingRule,
		Winner:               candidates[winnerIdx].Key,
		CandidatesConsidered: len(candidates),
		Evidence:             winnerQual.Evidence,
		Candidates:           verdicts,
		ExcerptSHA256:        doc.Digest(),
	}
}

// AutonomousBinds forwards to the grab store's grabs.binds audit query. It
// exists because Bridge is the only place that holds the *grab.Service —
// bootstrap.System has no field for it — the same reason IdentifyGrab, just
// above, is the entry point api.identifyGrab calls rather than the api
// package reaching into internal/grab on its own. b.grabs unset (PDF grabs
// not configured) reports no binds rather than an error: an operator asking
// what a machine has filed on an install with grabs off should see "none",
// not a fault.
func (b *Bridge) AutonomousBinds(ctx context.Context, limit int) ([]grab.BindRecord, error) {
	if b == nil || b.grabs == nil {
		return nil, nil
	}
	return b.grabs.ListAutonomousBinds(ctx, limit)
}

// copyFile streams src into a freshly created dst. Unlike copyHashed
// (internal/app), grabs need no content hash at this stage: the copy exists
// only for structural validation and DOI extraction, ahead of any job or
// candidate that would key on it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// RunSweeper calls SweepGrabs, SweepAdoptions, and SweepTerminalAdoptions on
// an interval until ctx is cancelled. SweepGrabs runs first so a grab that
// creates a job and places its file in that job's own adoption directory is
// claimed by SweepAdoptions in the very same tick, never waiting a full
// interval for a second pass.
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
			if err := b.SweepGrabs(ctx); err != nil && ctx.Err() != nil {
				return nil
			}
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
func (b *Bridge) poll(ctx context.Context, scheduled []job.BrowserCandidateDescriptor, schedulingUnavailable bool) ([]json.RawMessage, error) {
	if b.jobs != nil {
		if _, err := b.jobs.ReconcileMaterializationClaims(ctx, b.now()); err != nil {
			b.materializationClaimReconcileUnavailable = true
			b.materializationScheduleBlocked = true
			log.Printf("papio: reconciling expired materialization claims: %v", err)
			return nil, nil
		}
		b.materializationClaimReconcileUnavailable = false
		// close_authorizations §4.3: a token the extension never explicitly
		// consumes or acks (the daemon-driven settle/abandon paths now
		// consume theirs directly; this sweeps whatever is left — a token
		// issued for a tab the daemon never heard a removal for) must not
		// stay 'issued' forever. Best-effort, like the reconcile pass above:
		// a transient failure here must never block ordinary polling.
		if _, err := b.jobs.ExpireCloseAuthorizations(ctx, b.now().Add(-b.actionExpiry())); err != nil {
			log.Printf("papio: expiring stale close authorizations: %v", err)
		}
		// §4.5: a terminal owner already counts as an expired lease at
		// reservation time, but nothing forced that evaluation, so a dead
		// paper's institution slot stayed held until some other candidate
		// happened to contend for it — over ten hours on the operator's own
		// machine, with every read of that institution reporting a sign-in in
		// progress. Best-effort like the passes above; an unresolved
		// institutional effect permit still keeps its entry occupied.
		if _, err := b.jobs.RetireTerminalAuthenticationEntryLeases(ctx, b.now()); err != nil {
			log.Printf("papio: retiring terminal authentication entry leases: %v", err)
		}
		// Same class, other half: an entry that never bound a surface cannot
		// be a sign-in in progress, and one such entry refused 71 binds on
		// the operator's own machine while reporting exactly that.
		if _, err := b.jobs.ExpireUnboundAuthenticationEntryLeases(ctx, b.now()); err != nil {
			log.Printf("papio: expiring unbound authentication entry leases: %v", err)
		}
	}
	if b.materializationRecoveryPending {
		if err := b.recoverMaterializationFocus(ctx); err != nil {
			b.materializationScheduleBlocked = true
			b.materializationScheduleProcessed = false
			log.Printf("papio: recovering durable materialization focus: %v", err)
		} else {
			b.materializationRecoveryPending = false
		}
	}
	// Auth-return and session-evidence reoffers are deliberately bounded. Keep
	// walking that queue on ordinary browser ticks once capacity is available;
	// this is what drains a parked backlog without requiring another evidence
	// frame.
	for profile, sourceJobID := range b.reofferSourceJobID {
		if sourceJobID == "" || b.reofferRanThisSync[profile] {
			continue
		}
		if err := b.reofferInstitutionalSiblings(ctx, sourceJobID); err != nil {
			log.Printf("papio: browser reoffer poll unavailable: %v", err)
			continue
		}
		b.reofferRanThisSync[profile] = true
	}
	var awaiting []job.Row
	var err error
	if b.listAwaitingHuman != nil {
		awaiting, err = b.listAwaitingHuman(ctx, 200)
	} else {
		awaiting, err = b.jobs.List(ctx, job.StateAwaitingHuman, 200)
	}
	if err != nil {
		b.materializationScheduleBlocked = true
		log.Printf("papio: browser offer poll unavailable: %v", err)
		return nil, nil
	}
	var handoffJobs []job.OpenHandoffJob
	if b.listOpenHandoffs != nil {
		handoffJobs, _, err = b.listOpenHandoffs(ctx, handoffPageLimit)
	} else {
		handoffJobs, _, err = b.jobs.ListOpenHandoffJobsPage(ctx, handoffPageLimit)
	}
	if err != nil {
		b.materializationScheduleBlocked = true
		log.Printf("papio: browser handoff poll unavailable: %v", err)
		return nil, nil
	}
	b.materializationScheduleProcessed = true
	if b.captureStore != nil {
		if pending, pendingErr := b.captureStore.PendingJobs(ctx); pendingErr == nil {
			awaitingIDs := make(map[string]struct{}, len(awaiting))
			for _, row := range awaiting {
				awaitingIDs[row.ID] = struct{}{}
			}
			for _, jobID := range pending {
				if _, ok := awaitingIDs[jobID]; ok {
					continue
				}
				row, getErr := b.jobs.Get(ctx, jobID)
				if getErr != nil || row.State != job.StateAwaitingHuman {
					if releaseErr := b.captureStore.ReleaseJob(ctx, jobID); releaseErr != nil {
						log.Printf("papio: capture retention release for %s: %v", jobID, releaseErr)
					}
				}
			}
		}
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
		// Directory-scan adoption: a file the user (or a steered Chrome
		// download) placed in the job's adoption directory is the strongest
		// job-scoped gesture available. Exactly one settled regular file
		// adopts; zero or several (or an in-progress .crdownload) waits —
		// ambiguity stays with the user, per the fail-closed rule.
		if name, ok := b.scanAdoptionDir(ctx, row.ID); ok {
			_, err := b.adoptOutsideSessionLock(ctx, row.ID, name, nil, nil)
			if b.epoch != epoch {
				return out, nil
			}
			if err != nil {
				if evErr := b.recordAdoptionDeferred(ctx, row.ID, name, err); evErr != nil {
					log.Printf("papio: recording deferred browser adoption: %v", evErr)
				}
			} else {
				if latchErr := b.recordAdoptionConclusiveLatch(ctx, row.ID); latchErr != nil {
					log.Printf("papio: recording adoption safety latch: %v", latchErr)
				}
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
	// Slice 4 (dev/active/surface-lifecycle-plan.md): claim-paced automatic
	// candidate offers ride the same maxOutstandingOffers transport budget
	// as legacy/direct-route offers, so their admission is computed and
	// reserved before the legacy loop below spends the remainder. Their
	// share is capped at half of maxOutstandingOffers (never all of it, and
	// never zero when automatic work exists) so a mixed poll — automatic
	// claim-paced candidates alongside legacy/direct-route offers — always
	// leaves the legacy loop room instead of a same-poll flood of distinct
	// automatic claims spending the whole transport budget.
	automaticAdmitted := map[string]bool{}
	if b.claimBoundAutomaticMaterializationEnabled() {
		automaticCap := maxOutstandingOffers / 2
		if automaticCap < 1 {
			automaticCap = 1
		}
		if automaticCap > slots {
			automaticCap = slots
		}
		automaticAdmitted = b.admitAutomaticMaterializationCandidates(ctx, scheduled, handoff, automaticCap)
		slots -= len(automaticAdmitted)
		if slots < 0 {
			slots = 0
		}
	}
	held := 0
	heldIDs := make(map[string]bool)
jobLoop:
	for _, id := range candidateIDs {
		if hasSettledDownload(b.pendingDownloads, id) || b.offered[id] {
			continue
		}
		row := rows[id]
		if automaticAdmitted[id] {
			// Serviced by the claim-paced materialization path below; a
			// capable holder must never fall back to a URL-bearing legacy
			// offer for it.
			continue
		}
		if b.materializationTracked[id] && !b.focusPending[id] && b.institutionalMaterializationAvailable() {
			// A capable holder must never fall back to the URL-bearing legacy
			// offer after an explicit candidate has been observed. Claim expiry
			// only makes the durable candidate eligible again; a fresh explicit
			// focus/redrive is still required to surface it.
			continue
		}
		if b.focusPending[id] && b.institutionalMaterializationAvailable() {
			continue
		}
		overridden := b.focusPending[id] || b.reofferPending[id]
		quiescedByEvidence := false
		action := handoff[id]
		// The main auto-offer gate. focusPending is an explicit
		// `papio actions open`; reofferPending has already been through the
		// SAME evidence check in reofferInstitutionalSiblings, so both are
		// honoured here. A plain session-live tick is not.
		//
		// Age alone is not enough — see handoffQuiescedByEvidence.
		if !overridden {
			quiesced, _, err := b.handoffQuiescedByEvidence(ctx, id, action)
			if err != nil {
				log.Printf("papio: reading handoff history for %s: %v", id, err)
				continue
			}
			quiescedByEvidence = quiesced
		}
		if (action.Quiesced(b.now()) || quiescedByEvidence) && !overridden {
			continue
		}
		accessMode, offerable := b.offerableAccessMode(row)
		if !offerable {
			delete(b.reofferPending, id)
			continue
		}
		latched, latchErr := b.browserOfferLatched(ctx, row, action)
		if latchErr != nil {
			log.Printf("papio: reading browser safety latches for %s: %v", id, latchErr)
			delete(b.reofferPending, id)
			continue
		}
		if latched {
			delete(b.reofferPending, id)
			continue
		}
		if b.providerDriveEpochAvailable() {
			events, epochErr := b.jobs.Events(ctx, id)
			if epochErr != nil {
				log.Printf("papio: reading provider drive epoch history for %s: %v", id, epochErr)
				continue
			}
			if providerDriveEpochSuppressed(events, actionSafetyDomain(b.cfg, row, action)) {
				continue
			}
		}
		if slots <= 0 {
			held++
			heldIDs[id] = true
			continue
		}
		directAction := true
		if _, ok := app.OABrowserHandoffURL(action.Detail); ok {
			directAction = false
		}
		if _, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
			directAction = false
		}
		if directAction && b.directRouteEligible(row, accessMode) {
			candidates, candidateErr := b.directRouteCandidates(ctx, row)
			if candidateErr != nil {
				log.Printf("papio: reading direct-route identifiers for %s: %v", id, candidateErr)
			} else if len(candidates) != 0 {
				events, eventErr := b.jobs.Events(ctx, id)
				if eventErr != nil {
					log.Printf("papio: reading direct-route history for %s: %v", id, eventErr)
				} else {
					ordinal, inFlight, pendingAttempt := directRouteProgress(events, candidates, b.now())
					if inFlight || directRouteSucceeded(events) {
						continue
					}
					for ordinal < len(candidates) {
						candidate := candidates[ordinal]
						if !validateDirectRouteEnvelope(candidate) {
							break
						}
						candidateDomain := routeSafetyDomain(candidate.RouteRevision)
						latched, latchErr := b.browserOfferLatched(ctx, row, action,
							candidateDomain, resolverHost(candidate.URL))
						if latchErr != nil {
							log.Printf("papio: reading direct-route safety latches for %s: %v", id, latchErr)
							break
						}
						if latched {
							ordinal++
							continue
						}
						if !b.providerDirectGetAvailable() || !b.effectPermitAvailable() {
							// A direct route is an explicit effect path. Do not
							// fall through to a URL-bearing alternative offer.
							continue jobLoop
						}
						attemptKey := directRouteAttemptKey(id, ordinal, candidate.RouteRevision)
						attemptID := pendingAttempt
						if attemptID == "" && b.directRouteAttempts != nil {
							attemptID = b.directRouteAttempts[attemptKey]
						}
						if attemptID == "" {
							attemptID = newMsgID()
						}
						attemptRevision, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, id)
						if attemptErr != nil || attemptRevision < 1 {
							continue jobLoop
						}
						identity := job.EffectPermitIdentity{
							JobID: id, Kind: job.EffectKindDirectGet,
							DriveAttemptID: attemptID, Ordinal: int64(ordinal),
							Strategy: "direct_get", Revision: candidate.RouteRevision,
						}
						permit, permitOutcome, permitErr := b.jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
							Identity:                identity,
							JobAttemptRevision:      attemptRevision,
							BrowserHolderGeneration: int64(b.epoch),
							SafetyDomainID:          candidateDomain,
							LeaseUntil:              b.now().UTC().Add(b.actionExpiry()),
							Authorization: job.EffectPermitEvent{Kind: "browser.direct_route", Detail: map[string]any{
								"route_revision":   candidate.RouteRevision,
								"safety_domain":    candidateDomain,
								"ordinal":          ordinal,
								"drive_attempt_id": attemptID,
								"phase":            "offered",
							}},
						})
						exactReplay := permit != nil && permit.Status == job.EffectPermitHeld &&
							permit.JobAttemptRevision == attemptRevision &&
							permit.BrowserHolderGeneration == int64(b.epoch) &&
							permit.SafetyDomainID == candidateDomain
						if permitErr != nil || (permitOutcome != job.EffectPermitAcquired &&
							(permitOutcome != job.EffectPermitDuplicate || !exactReplay)) {
							if !errors.Is(permitErr, job.ErrEffectPermitBusy) &&
								permitOutcome != job.EffectPermitBusyOutcome {
								delete(b.directRouteAttempts, attemptKey)
							}
							continue jobLoop
						}
						frame, frameErr := b.frame(protocol.MsgProviderDirectGetRequest, id,
							protocol.ProviderDirectGetRequestPayload{
								DriveAttemptID:     attemptID,
								Ordinal:            int64(ordinal),
								RouteRevision:      candidate.RouteRevision,
								ExpectedIdentifier: candidate.Identifier,
								URL:                candidate.URL,
								AllowedOrigin:      candidate.AllowedOrigin,
								PathFamily:         candidate.PathFamily,
								TermsPolicy:        candidate.TermsPolicy,
							})
						if frameErr != nil {
							return nil, frameErr
						}
						delete(b.directRouteAttempts, attemptKey)
						out = append(out, frame)
						b.offered[id] = true
						delete(b.reofferPending, id)
						slots--
						continue jobLoop
					}
				}
			}
		}
		offer, err := b.offer(row, action, accessMode)
		if err != nil {
			return nil, err
		}
		if err := b.jobs.S.AppendEvent(ctx, row.ID, "browser.handoff_offered",
			map[string]any{
				"requires_auth": action.RequiresAuth,
				"safety_domain": actionSafetyDomain(b.cfg, row, action),
			}); err != nil {
			log.Printf("papio: recording browser.handoff_offered for %s: %v", row.ID, err)
			continue
		}
		out = append(out, offer)
		b.offered[row.ID] = true
		delete(b.reofferPending, row.ID)
		slots--
	}
	// Announce cancellation for legacy handoff offers, materialization-only
	// candidate offers, and terminal jobs which still own a durable live claim.
	// The first two are worker-memory indexes; the third repairs their loss
	// across a daemon restart, when the extension can still hold both its local
	// job state and the tab but no in-memory map names either.
	cancelIDs := make(map[string]bool, len(b.offered)+len(b.materializationOffered)+len(b.materializationTracked))
	for id := range b.offered {
		cancelIDs[id] = true
	}
	for id := range b.materializationOffered {
		cancelIDs[id] = true
	}
	for id := range b.materializationTracked {
		cancelIDs[id] = true
	}
	// Terminal jobs whose cancel this session has ALREADY delivered. Read before
	// the loop below sets cancelSent, so a job appears here one poll after it was
	// told — never in the same poll that emits its frame.
	var terminalCancelled []string
	if terminalIDs, terminalErr := b.jobs.TerminalMaterializationJobIDs(ctx); terminalErr != nil {
		log.Printf("papio: reading terminal materialization jobs for browser cancellation: %v", terminalErr)
	} else {
		for _, id := range terminalIDs {
			if b.cancelSent[id] {
				terminalCancelled = append(terminalCancelled, id)
			}
			cancelIDs[id] = true
		}
	}
	for id := range cancelIDs {
		row, err := b.jobs.Get(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			delete(b.offered, id)
			b.clearMaterializationTracking(id)
			continue
		}
		if err != nil {
			log.Printf("papio: checking cancelled browser job %s: %v", id, err)
			continue
		}
		trackedMaterialization := b.materializationTracked[id] ||
			b.materializationOffered[id].CandidateID != ""
		actionPresent := present[id]
		if trackedMaterialization && row.State == job.StateAwaitingHuman && !actionPresent {
			// present is only the bounded poll page; a tracked job may be
			// beyond that page. Confirm its action directly before treating
			// omission as closure.
			action, actionErr := b.openHandoffForJob(ctx, id)
			switch {
			case actionErr == nil && action != nil:
				actionPresent = true
			case actionErr != nil && !errors.Is(actionErr, sql.ErrNoRows):
				log.Printf("papio: checking tracked materialization action %s: %v", id, actionErr)
				continue
			}
		}
		if trackedMaterialization && (row.State != job.StateAwaitingHuman || !actionPresent) {
			if !b.cancelSent[id] {
				frame, frameErr := b.frame(protocol.MsgCancel, id, protocol.EmptyPayload{})
				if frameErr != nil {
					return nil, frameErr
				}
				out = append(out, frame)
				b.cancelSent[id] = true
			}
			b.clearMaterializationTracking(id)
			continue
		}
		if row.State != job.StateCancelled {
			if row.State != job.StateAwaitingHuman {
				b.clearMaterializationTracking(id)
			}
			continue
		}
		if !b.cancelSent[id] {
			frame, frameErr := b.frame(protocol.MsgCancel, id, protocol.EmptyPayload{})
			if frameErr != nil {
				return nil, frameErr
			}
			out = append(out, frame)
			b.cancelSent[id] = true
		}
		b.clearMaterializationTracking(id)
	}
	// The row itself, last. Both reconcile paths decline to touch a claim
	// carrying any effect permit, which for a terminal job protects nothing and
	// makes the row immortal: eleven claims on cancelled jobs, tab ids days
	// dead, sat `navigated` on the operator's machine while this very loop
	// re-sent their cancel on every daemon restart. Retiring only what was
	// already told keeps the frame above as the browser's notice, and keeps this
	// best-effort like the reconcile passes: a failure must never block polling.
	if len(terminalCancelled) > 0 {
		if _, err := b.jobs.AbandonTerminalMaterializations(ctx, b.now(), terminalCancelled); err != nil {
			log.Printf("papio: abandoning terminal materialization claims: %v", err)
		}
	}
	if b.epoch != epoch {
		return out, nil
	}
	scheduledByJob := make(map[string]job.BrowserCandidateDescriptor, len(scheduled))
	scheduledRawByJob := make(map[string]job.BrowserCandidateDescriptor, len(scheduled))
	scheduledDomainOwner := make(map[string]string, len(scheduled))
	for _, descriptor := range scheduled {
		if descriptor.JobID == "" {
			continue
		}
		scheduledRawByJob[descriptor.JobID] = descriptor
		domain := descriptor.SafetyDomainID
		if domain == "" {
			domain = descriptor.InstitutionProfileID + "\x00" + descriptor.PreRouteSafetyKey
		}
		if b.focusPending[descriptor.JobID] || automaticAdmitted[descriptor.JobID] {
			if _, exists := scheduledDomainOwner[domain]; !exists {
				scheduledDomainOwner[domain] = descriptor.JobID
				scheduledByJob[descriptor.JobID] = descriptor
			}
		}
	}
	if b.holder != nil && b.holder.ID != legacySessionID && compareVersion(b.holder.ExtensionVersion, HandoffFocusMinExtensionVersion) >= 0 {
		ids := make([]string, 0, len(b.focusPending))
		ordered := make(map[string]bool, len(b.focusPending))
		for _, descriptor := range scheduled {
			_, hasHandoff := handoff[descriptor.JobID]
			if descriptor.JobID != "" && b.focusPending[descriptor.JobID] && hasHandoff {
				ids = append(ids, descriptor.JobID)
				ordered[descriptor.JobID] = true
			}
		}
		var rest []string
		for id := range b.focusPending {
			if !ordered[id] {
				rest = append(rest, id)
			}
		}
		sort.Strings(rest)
		ids = append(ids, rest...)
		focused := 0
		for _, id := range ids {
			if _, ok := handoff[id]; !ok {
				action, actionErr := b.openHandoffForJob(ctx, id)
				switch {
				case errors.Is(actionErr, sql.ErrNoRows):
					delete(b.focusPending, id)
					continue
				case actionErr != nil:
					log.Printf("papio: checking focused browser action %s: %v", id, actionErr)
					continue
				case action == nil:
					delete(b.focusPending, id)
					continue
				default:
					handoff[id] = *action
				}
			}
			row, getErr := b.jobs.Get(ctx, id)
			switch {
			case errors.Is(getErr, sql.ErrNoRows):
				delete(b.focusPending, id)
				continue
			case getErr != nil:
				log.Printf("papio: checking focused browser job %s: %v", id, getErr)
				continue
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
			latched, latchErr := b.browserOfferLatched(ctx, *row, handoff[id])
			if latchErr != nil {
				log.Printf("papio: reading focused browser safety latch for %s: %v", id, latchErr)
				continue
			}
			if latched {
				delete(b.focusPending, id)
				continue
			}
			if b.focusPending[id] && b.institutionalMaterializationAvailable() {
				frame, err := b.serviceMaterializationCandidate(ctx, id, row, handoff[id], accessMode, scheduledByJob, scheduledRawByJob)
				if err != nil {
					return nil, err
				}
				if frame != nil {
					out = append(out, frame)
				}
				continue
			}
			if !b.offered[id] && b.providerDriveEpochAvailable() {
				events, epochErr := b.jobs.Events(ctx, id)
				if epochErr != nil {
					log.Printf("papio: reading focused provider epoch for %s: %v", id, epochErr)
					continue
				}
				if providerDriveEpochSuppressed(events, actionSafetyDomain(b.cfg, *row, handoff[id])) {
					continue
				}
			}
			if !b.offered[id] {
				if slots <= 0 {
					if !heldIDs[id] {
						held++
						heldIDs[id] = true
					}
					continue
				}
				offer, offerErr := b.offerAtURL(*row, handoff[id], accessMode, "", true)
				if offerErr != nil {
					return nil, offerErr
				}
				if err := b.jobs.S.AppendEvent(ctx, id, "browser.handoff_offered",
					map[string]any{
						"requires_auth": handoff[id].RequiresAuth,
						"safety_domain": actionSafetyDomain(b.cfg, *row, handoff[id]),
					}); err != nil {
					log.Printf("papio: recording focused handoff offer for %s: %v", id, err)
					continue
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
	if len(automaticAdmitted) > 0 {
		// Slice 4: service claim-paced automatic candidates in a
		// deterministic order, sharing serviceMaterializationCandidate with
		// the explicit-focus loop above. No MsgHandoffFocus/legacy URL
		// fallback applies here: a capable holder never falls back to a
		// URL-bearing offer, and nothing here was an operator gesture.
		autoIDs := make([]string, 0, len(automaticAdmitted))
		for id := range automaticAdmitted {
			autoIDs = append(autoIDs, id)
		}
		sort.Strings(autoIDs)
		for _, id := range autoIDs {
			if hasSettledDownload(b.pendingDownloads, id) {
				continue
			}
			row, ok := rows[id]
			if !ok {
				continue
			}
			action := handoff[id]
			accessMode, offerable := b.offerableAccessMode(row)
			if !offerable {
				continue
			}
			latched, latchErr := b.browserOfferLatched(ctx, row, action)
			if latchErr != nil {
				log.Printf("papio: reading automatic browser safety latch for %s: %v", id, latchErr)
				continue
			}
			if latched {
				continue
			}
			frame, err := b.serviceMaterializationCandidate(ctx, id, &row, action, accessMode, scheduledByJob, scheduledRawByJob)
			if err != nil {
				return nil, err
			}
			if frame != nil {
				out = append(out, frame)
			}
		}
	}
	if held == 0 {
	} else if held != b.lastPacedHeld {
		if err := b.jobs.S.AppendEvent(ctx, "", "browser.offers_paced", map[string]any{"held": held}); err != nil {

			log.Printf("papio: recording offer pacing: %v", err)
		} else {
			b.lastPacedHeld = held
		}
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
	if b.grabs != nil {
		pending, err := b.grabs.PendingNotifications(ctx, 10)
		if err != nil {
			log.Printf("papio: reading pending PDF grab notifications: %v", err)
			return out, nil
		}
		for _, g := range pending {
			frame, err := b.frame(protocol.MsgPdfGrabResult, "", protocol.PdfGrabResultPayload{
				GrabID: g.ID, Outcome: g.Outcome, Detail: g.Detail,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, frame)
			if err := b.grabs.MarkNotified(ctx, g.ID); err != nil {
				log.Printf("papio: marking PDF grab notification %s: %v", g.ID, err)
			}
		}
	}
	if b.effectPermitAvailable() && b.jobs != nil {
		if permit, _ := b.jobs.LiveEffectPermit(ctx); permit != nil {
			payload := protocol.EffectPermitReconcileRequestPayload{
				RequestID:  newMsgID(),
				PermitID:   permit.ID,
				EffectKind: string(permit.Kind),
			}
			switch permit.Kind {
			case job.EffectKindGenericDrive, job.EffectKindDirectGet:
				if permit.Ordinal != nil {
					payload.DriveAttemptID = permit.DriveAttemptID
					payload.Ordinal = permit.Ordinal
					payload.Strategy = permit.Strategy
					payload.Revision = permit.Revision
				}
			case job.EffectKindPDFGrab:
				payload.GrabID = permit.GrabID
			case job.EffectKindTerms:
				payload.TermsOccurrenceID = permit.TermsOccurrenceID
			case job.EffectKindInstitutional:
				payload.ClaimID = permit.ClaimID
				payload.BindingID = permit.BindingID
				payload.EffectOrdinal = permit.EffectOrdinal
				payload.InstitutionalRequestID = permit.InstitutionalRequestID
			}
			// Only emit when kind-specific identity is complete; otherwise
			// self-validation would fail and the whole poll would abort.
			complete := false
			switch permit.Kind {
			case job.EffectKindGenericDrive, job.EffectKindDirectGet:
				complete = payload.DriveAttemptID != "" && payload.Ordinal != nil && payload.Strategy != "" && payload.Revision != ""
			case job.EffectKindPDFGrab:
				complete = payload.GrabID != ""
			case job.EffectKindTerms:
				complete = payload.TermsOccurrenceID != ""
			case job.EffectKindInstitutional:
				complete = payload.ClaimID != "" && payload.BindingID != "" && payload.EffectOrdinal != nil && payload.InstitutionalRequestID != ""
			}
			if complete {
				if frame, err := b.frame(protocol.MsgEffectPermitReconcileRequest, permit.JobID, payload); err == nil {
					for requestID, pending := range b.effectPermitReconciles {
						if pending.permitID == permit.ID {
							delete(b.effectPermitReconciles, requestID)
						}
					}
					b.effectPermitReconciles[payload.RequestID] = pendingEffectPermitReconcile{
						permitID: permit.ID,
						jobID:    permit.JobID,
					}
					out = append(out, frame)
				} else {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

// serviceMaterializationCandidate resolves, refreshes, or clears one
// MsgInstitutionalCandidateOffer for id against the current scheduler
// snapshot. It is shared by poll's explicit-focus loop and Slice 4's
// automatic admission loop (dev/active/surface-lifecycle-plan.md Slice 4):
// both loops fully own materialization for the ids they hand it, so a nil
// frame with a nil error means id is still pending (or its tracking was
// just cleared) and the caller must never fall back to a legacy URL offer
// for it. The caller holds b.mu.
func (b *Bridge) serviceMaterializationCandidate(
	ctx context.Context, id string, row *job.Row, action job.HumanAction, accessMode string,
	scheduledByJob, scheduledRawByJob map[string]job.BrowserCandidateDescriptor,
) (json.RawMessage, error) {
	candidate, candidateOK := scheduledByJob[id]
	if !candidateOK {
		if _, domainBlocked := scheduledRawByJob[id]; domainBlocked {
			return nil, nil
		}
		attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, id)
		if attemptErr != nil {
			log.Printf("papio: reading materialization attempt for %s: %v", id, attemptErr)
			return nil, nil
		}
		durable, candidateErr := b.jobs.CurrentBrowserCandidateForJob(ctx, id, attempt)
		if candidateErr != nil {
			log.Printf("papio: reading materialization candidate for %s: %v", id, candidateErr)
			return nil, nil
		}
		if durable == nil {
			b.clearMaterializationTracking(id)
			delete(b.focusPending, id)
			return nil, nil
		}
		switch durable.Status {
		case "claimed", "materializing":
			candidate = job.BrowserCandidateDescriptor{
				CandidateID: durable.ID, JobID: durable.JobID,
				JobAttemptRevision:         durable.JobAttemptRevision,
				InstitutionProfileID:       durable.InstitutionProfileID,
				InstitutionProfileRevision: durable.InstitutionProfileRevision,
				RouteRevision:              durable.RouteRevision, RouteClass: durable.RouteClass,
				IdentifierStrategy: durable.IdentifierStrategy,
				PreRouteSafetyKey:  durable.PreRouteSafetyKey,
				SafetyDomainID:     durable.SafetyDomainID,
				AdapterRevision:    durable.AdapterRevision,
				EffectContractID:   durable.EffectContractID,
				Status:             durable.Status, CreatedAt: durable.CreatedAt,
			}
			candidateOK = candidate.CandidateID != ""
		case "eligible":
			// An eligible row the scheduler did not hand us is not ours to offer,
			// and in-memory tracking is the proof it did. This holds for an
			// explicitly focused job too: the scheduler is the authority that
			// admits at most one candidate per safety domain, so offering a
			// durable row behind its back can put two institutional surfaces on
			// one provider - the invariant
			// TestMaterializationSchedulerKeepsOneSafetyDomainScaffold and
			// TestMaterializationSchedulerErrorRetainsFocusUntilRecovery pin.
			// A focused candidate that never reaches a schedule page is starvation
			// upstream, in ScheduleEligibleBrowserCandidates, and is fixed there.
			trackedOffer, tracked := b.materializationOffered[id]
			if !tracked || trackedOffer.CandidateID != durable.ID {
				return nil, nil
			}
			candidate = job.BrowserCandidateDescriptor{
				CandidateID: durable.ID, JobID: durable.JobID,
				JobAttemptRevision:         durable.JobAttemptRevision,
				InstitutionProfileID:       durable.InstitutionProfileID,
				InstitutionProfileRevision: durable.InstitutionProfileRevision,
				RouteRevision:              durable.RouteRevision, RouteClass: durable.RouteClass,
				IdentifierStrategy: durable.IdentifierStrategy,
				PreRouteSafetyKey:  durable.PreRouteSafetyKey,
				SafetyDomainID:     durable.SafetyDomainID,
				AdapterRevision:    durable.AdapterRevision,
				EffectContractID:   durable.EffectContractID,
				Status:             durable.Status, CreatedAt: durable.CreatedAt,
			}
			candidateOK = true
		default:
			b.clearMaterializationTracking(id)
			delete(b.focusPending, id)
			return nil, nil
		}
	}
	if !candidateOK {
		return nil, nil
	}
	attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, id)
	if attemptErr != nil {
		return nil, nil
	}
	current, currentErr := b.jobs.CurrentBrowserCandidateForJob(ctx, id, attempt)
	if currentErr != nil {
		return nil, nil
	}
	if current == nil {
		b.clearMaterializationTracking(id)
		delete(b.focusPending, id)
		return nil, nil
	}
	if current.ID != candidate.CandidateID ||
		current.JobAttemptRevision != candidate.JobAttemptRevision ||
		current.InstitutionProfileID != candidate.InstitutionProfileID ||
		current.InstitutionProfileRevision != candidate.InstitutionProfileRevision ||
		current.RouteRevision != candidate.RouteRevision ||
		current.PreRouteSafetyKey != candidate.PreRouteSafetyKey ||
		current.SafetyDomainID != candidate.SafetyDomainID ||
		current.AdapterRevision != candidate.AdapterRevision ||
		current.EffectContractID != candidate.EffectContractID ||
		(candidate.Status == "eligible" && current.Status != "eligible") ||
		(candidate.Status != "eligible" && current.Status != "claimed" && current.Status != "materializing") {
		return nil, nil
	}
	// A candidate owned by a finished attempt cannot be advanced by any offer:
	// its claim's holder generation is gone, its lease is over, and a claim
	// request against it can only answer busy. Offering it every poll is what
	// produced a sustained claim/busy round trip about once a second per stuck
	// paper, measured live 2026-08-20 on four of them, indefinitely. Checked
	// here rather than in the durable branch above because the scheduler
	// snapshot is cached and can still describe the candidate as eligible.
	// Only an explicit operator ask starts the next attempt
	// (StartNextMaterializationAttemptForSpentCandidate).
	if current.Status != "eligible" {
		if spent, spentErr := b.jobs.SpentMaterializationCandidate(ctx, id); spentErr != nil {
			log.Printf("papio: reading spent materialization attempt for %s: %v", id, spentErr)
		} else if spent {
			b.clearMaterializationTracking(id)
			delete(b.focusPending, id)
			return nil, nil
		}
	}
	now := b.now()
	offerState, tracked := b.materializationOffered[id]
	newCandidate := !tracked
	if tracked && offerState.CandidateID != candidate.CandidateID {
		delete(b.materializationOffered, id)
		tracked = false
		newCandidate = true
	}
	if newCandidate {
		delete(b.cancelSent, id)
	}
	if tracked && !offerState.ExpiresAt.IsZero() && !offerState.ExpiresAt.After(now) {
		delete(b.materializationOffered, id)
		delete(b.materializationTracked, id)
		tracked = false
		delete(b.cancelSent, id)
	}
	expiresAt := now.Add(b.actionExpiry())
	if tracked && !offerState.ExpiresAt.IsZero() {
		expiresAt = offerState.ExpiresAt
	}
	events, eventsErr := b.jobs.Events(ctx, id)
	if eventsErr != nil {
		log.Printf("papio: reading candidate offer context for %s: %v", id, eventsErr)
		return nil, nil
	}
	hosts := b.browserOfferHosts(*row, action, events)
	if len(hosts) == 0 {
		return nil, nil
	}
	expected := &protocol.JobOfferExpected{DOI: row.Work.DOI, Title: truncate(row.Work.Title, 500)}
	if expected.DOI == "" && expected.Title == "" {
		expected = nil
	}
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	offerPayload := protocol.InstitutionalCandidateOfferPayload{
		CandidateID: candidate.CandidateID, MaterializationKind: "browser_tab",
		ExpiresAt:     expiresAt.UTC().Format(time.RFC3339),
		ProviderHosts: hosts, Expected: expected, AccessMode: accessMode,
		LoginEntityID: inst.ShibbolethEntityID, ProquestAccountID: inst.ProquestAccountID,
		RequiresAuth: action.RequiresAuth,
	}
	if attemptID, ordinal, ok := b.latestProviderDriveEpoch(id); ok {
		offerPayload.DriveAttemptID = attemptID
		offerPayload.DriveOrdinal = &ordinal
		offerPayload.DriveStrategy = "generic"
		offerPayload.DriveRevision = "1"
	}
	offerFrame, frameErr := b.frame(protocol.MsgInstitutionalCandidateOffer, id, offerPayload)
	if frameErr != nil {
		return nil, frameErr
	}
	b.materializationOffered[id] = materializationOffer{CandidateID: candidate.CandidateID, ExpiresAt: expiresAt}
	b.materializationTracked[id] = true
	return offerFrame, nil
}

// claimBoundAutomaticMaterializationEnabled is Slice 4's enablement gate
// (dev/active/surface-lifecycle-plan.md Slice 4, "Rollout order"): a session
// that negotiated institutional_materialization_v1 (and effect_permit_v1,
// via institutionalMaterializationAvailable) gets automatic (non-focus)
// materialization candidate offers by default — no operator toggle. A
// session missing that negotiation keeps today's behavior exactly (legacy
// URL re-offers, explicit-focus-only candidates); mixed-version safety
// comes from that hello_ack feature negotiation, not from leaving the
// automatic path dark.
//
// institutional_authentication_claim_v1 is checked against b.Features (the
// daemon's own advertised list), not the session's: per
// institutionalAuthenticationClaimMessage's dispatch gate above,
// authentication_claim_request/claim_observation are brand-new message
// types with no legacy shape to disambiguate, so the extension never
// advertises this one in its hello at all — it self-attests capability by
// sending the message, after seeing the daemon advertise it in hello_ack.
// Gating this on the session's list would therefore always be false and
// would silently leave the automatic path dark for every current pair.
//
// This is deliberately independent of ADR-0022 Decision 10's Phase 4/5
// readiness gate (job.InstitutionCutoverDecision.CanaryReadyRouteExists,
// computed in internal/app and unread here): that gate governs whether an
// automatic SIGNED-OUT/unknown-evidence FIRST route may be canaried after
// ordinary OA exhaustion, which stays off until Phase 5 qualifies exact
// route tuples. This gate only lets already-scaffolded, claim-admitted
// BROWSER-TAB materialization — warm or human-paced-in-progress work that
// Phase 1-3 already shipped behind explicit focus — ride automatically
// instead of waiting for an operator click; it never bypasses OA, a source
// gate, or provider-readiness qualification.
func (b *Bridge) claimBoundAutomaticMaterializationEnabled() bool {
	return b.institutionalMaterializationAvailable() &&
		slices.Contains(b.Features, institutionalAuthenticationClaimFeature)
}

// admitAutomaticMaterializationCandidates is Slice 4's claim-paced admission
// on top of the scheduler's fair, per-domain batch: scheduled work that does
// not currently require authentication is admitted immediately (no
// authentication-claim contention). Work that does require authentication
// reuses the Slice 3 authentication-entry lease
// (dev/active/claim-observation-protocol.md §4) rather than re-deriving
// claim ownership:
//
//   - no lease yet for the claim: the daemon reserves one for exactly this
//     job right here — the architecture ruling's "consult → open_new means
//     the lease is reserved for the job" — so the single-admission fence is
//     durable across polls instead of a per-call in-memory map (the
//     scheduler can return several same-claim, different-domain descriptors
//     in one pass, and a fresh reservation each poll would let each of them
//     win a separate poll in turn). A reservation failure (raced busy, or a
//     genuine store error) parks this descriptor for a later poll;
//   - the lease is reserved to this exact job: stays admitted, so its offer
//     keeps refreshing until bound or resolved. At most one descriptor for
//     that job is admitted per poll even here — a job with several eligible
//     candidates across revisions/safety domains must not scaffold more
//     than one at a time while its claim is still unresolved;
//   - the lease is human AND durably entitled (a fenced entitled_landing
//     observation applied — not merely auth_returned's state='human', which
//     only proves an IdP round trip happened, not that it reached entitled
//     content): every eligible dependent is admitted, since each proceeds
//     on its own materialization binding without a fresh login wall
//     (ADR-0022 Decision 6: "successful resolution resumes eligible
//     siblings through normal daemon scheduling");
//   - any other state (reserved by a different job, human-but-not-yet-
//     entitled, expired/retired after owner_closed, or a lookup failure):
//     parked — this job is a dependent of an unresolved (or just-retired)
//     claim and stays tabless until the daemon observes entitled_landing or
//     grants a fresh arbitration.
//
// A descriptor already carrying a live legacy handoff offer (b.offered) is
// skipped outright: a job migrates from the legacy URL-bearing offer to the
// claim-paced candidate path only after that offer is retired (cancelled or
// timed out), never both at once — otherwise the extension would receive two
// drives for the same job and could retain the old tab while also creating
// the institutional scaffold.
//
// limit bounds total admissions to the remaining maxOutstandingOffers
// budget shared with legacy/direct-route offers. The caller holds b.mu.
func (b *Bridge) admitAutomaticMaterializationCandidates(
	ctx context.Context, scheduled []job.BrowserCandidateDescriptor,
	handoff map[string]job.HumanAction, limit int,
) map[string]bool {
	admitted := map[string]bool{}
	if limit <= 0 {
		return admitted
	}
	claimSlotUsed := map[string]bool{}
	for _, descriptor := range scheduled {
		if len(admitted) >= limit {
			break
		}
		if descriptor.JobID == "" || b.focusPending[descriptor.JobID] || b.offered[descriptor.JobID] {
			continue
		}
		action, hasAction := handoff[descriptor.JobID]
		if !hasAction {
			continue
		}
		if !action.RequiresAuth {
			admitted[descriptor.JobID] = true
			continue
		}
		profile, err := b.jobs.GetInstitutionProfile(ctx, descriptor.InstitutionProfileID)
		if err != nil || profile == nil || profile.TombstonedAt != "" ||
			profile.AuthenticationClaimID == "" || profile.Revision != descriptor.InstitutionProfileRevision {
			continue
		}
		claimID := profile.AuthenticationClaimID
		lease, found, leaseErr := b.jobs.GetAuthenticationEntryLease(ctx, claimID)
		if leaseErr != nil {
			continue
		}
		switch {
		case !found:
			if claimSlotUsed[claimID] {
				continue
			}
			leaseID := evidenceObservationID("authentication_claim_lease", claimID, descriptor.JobID, strconv.FormatInt(b.epoch, 10))
			if _, reserveErr := b.jobs.ReserveAuthenticationEntryLease(ctx, job.AuthenticationEntryLeaseInput{
				AuthenticationClaimID: claimID, LeaseID: leaseID, OwnerID: descriptor.JobID,
				BrowserHolderGeneration: b.epoch, LeaseUntil: b.now().Add(b.actionExpiry()),
			}); reserveErr != nil {
				continue
			}
			claimSlotUsed[claimID] = true
		case lease.State == job.AuthenticationEntryLeaseHuman && lease.EntitledAt != "":
			// Landed and entitled: every eligible dependent proceeds on its
			// own binding.
		case lease.State == job.AuthenticationEntryLeaseReserved && lease.OwnerID == descriptor.JobID:
			if claimSlotUsed[claimID] {
				continue
			}
			claimSlotUsed[claimID] = true
		default:
			// Reserved by a different job, human-but-not-entitled, expired,
			// or unknown state: parked.
			continue
		}
		admitted[descriptor.JobID] = true
	}
	return admitted
}
func (b *Bridge) providerDirectGetAvailable() bool {
	if b == nil || b.holder == nil || !slices.Contains(b.Features, providerDirectGetV1Feature) {
		return false
	}
	return compareVersion(b.holder.ExtensionVersion, ProviderDirectGetMinExtensionVersion) >= 0
}
func (b *Bridge) effectPermitAvailable() bool {
	if b == nil || b.holder == nil || !slices.Contains(b.Features, effectPermitFeature) {
		return false
	}
	return slices.Contains(b.holder.Features, effectPermitFeature)
}
func (b *Bridge) providerDriveEpochAvailable() bool {
	if b == nil || b.holder == nil || !slices.Contains(b.Features, providerDriveEpochV1Feature) || !b.effectPermitAvailable() {
		return false
	}
	return compareVersion(b.holder.ExtensionVersion, ProviderDirectGetMinExtensionVersion) >= 0
}

// offerableAccessMode resolves the access mode to advertise for one handoff.
func (b *Bridge) offerableAccessMode(row job.Row) (string, bool) {
	mode := b.cfg.EffectiveAccessMode(row.Policy.AccessMode)
	// ISBN catalogue/ebook routes are deliberately human-assisted even when
	// the global policy is delegated: papio cannot automatically fetch or
	// validate a book PDF.
	actions, err := b.jobs.ListOpenHumanActionsForJobs(context.Background(), []string{row.ID})
	if err == nil {
		for _, action := range actions {
			if action.Kind == handoffActionKind &&
				strings.HasPrefix(action.Detail, app.InstitutionalBookOpenURLHandoffDetail) {
				mode = config.ModeAssisted
				break
			}
		}
	}
	switch mode {
	case config.ModeAssisted, config.ModeDelegated:
		return mode, true
	default:
		return "", false
	}
}

// directRouteCandidates loads the public identifiers that the compiled route
// table can consume. Direct routes are provider-scoped: a missing provider
// hint is deliberately not treated as "all providers", because that would
// turn an arbitrary DOI into an unrelated provider navigation.
func (b *Bridge) directRouteCandidates(ctx context.Context, row job.Row) ([]routes.Candidate, error) {
	identifiers := make(map[string]string, 2)
	if row.Work.DOI != "" {
		identifiers["doi"] = row.Work.DOI
	}
	if row.WorkRequestID != "" {
		var pii string
		err := b.jobs.S.DB().QueryRowContext(ctx,
			`SELECT value FROM identifiers WHERE work_request_id = ? AND kind = 'pii'`,
			row.WorkRequestID).Scan(&pii)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if pii != "" {
			identifiers["pii"] = pii
		}
	}
	all := routes.CandidatesForIdentifiers(identifiers, "")

	// A resolver policy may itself name a packaged provider family or route
	// revision. Ordinary institution profile names simply produce no match and
	// fall through to the institution URL / durable provider evidence below.
	if hint := strings.TrimSpace(row.Policy.Resolver); hint != "" {
		if candidates := routes.CandidatesForIdentifiers(identifiers, hint); len(candidates) != 0 {
			return candidates, nil
		}
	}
	if inst, ok := b.cfg.InstitutionFor(row.Policy.Resolver); ok {
		if family := directRouteFamilyForHost(all, resolverHost(RouteURL(inst, row.Work))); family != "" {
			if candidates := routes.CandidatesForIdentifiers(identifiers, family); len(candidates) != 0 {
				return candidates, nil
			}
		}
	}

	events, err := b.jobs.Events(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	// Only use the all-routes expansion as an internal lookup against durable
	// evidence. No candidate from it is emitted; the matching route family is
	// pinned and expanded again below.
	for i := len(events) - 1; i >= 0; i-- {
		kind, _ := events[i]["kind"].(string)
		detail, _ := events[i]["detail"].(map[string]any)
		if revision := stringDetail(detail, "route_revision"); kind == "browser.direct_route" && revision != "" {
			family := revision
			if slash := strings.LastIndexByte(family, '/'); slash > 0 {
				family = family[:slash]
			}
			if candidates := routes.CandidatesForIdentifiers(identifiers, family); len(candidates) != 0 {
				return candidates, nil
			}
		}
		if kind == "browser.provider_outcome" {
			if adapter := strings.TrimSpace(stringDetail(detail, "adapter_id")); adapter != "" {
				for _, candidate := range all {
					family := candidate.RouteRevision
					if slash := strings.LastIndexByte(family, '/'); slash > 0 {
						family = family[:slash]
					}
					if adapter == family || strings.HasPrefix(family, adapter+"-") {
						if candidates := routes.CandidatesForIdentifiers(identifiers, family); len(candidates) != 0 {
							return candidates, nil
						}
					}
				}
			}
		}
		host := strings.TrimSpace(stringDetail(detail, "host"))
		if host == "" || (kind != "browser.page_capture" && kind != "browser.provider_outcome") {
			continue
		}
		for _, candidate := range all {
			if !directRouteHostMatches(host, candidate.AllowedOrigin) {
				continue
			}
			family := candidate.RouteRevision
			if slash := strings.LastIndexByte(family, '/'); slash > 0 {
				family = family[:slash]
			}
			if candidates := routes.CandidatesForIdentifiers(identifiers, family); len(candidates) != 0 {
				return candidates, nil
			}
		}
	}
	return nil, nil
}

func directRouteFamilyForHost(candidates []routes.Candidate, host string) string {
	for _, candidate := range candidates {
		if directRouteHostMatches(host, candidate.AllowedOrigin) {
			family := candidate.RouteRevision
			if slash := strings.LastIndexByte(family, '/'); slash > 0 {
				family = family[:slash]
			}
			return family
		}
	}
	return ""
}

func directRouteHostMatches(host, origin string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	originHost := strings.TrimSuffix(strings.ToLower(resolverHost(origin)), ".")
	return host != "" && originHost != "" &&
		(host == originHost || strings.HasSuffix(host, "."+originHost) || strings.HasSuffix(originHost, "."+host))
}

func directRouteOrdinal(detail map[string]any) (int, bool) {
	switch value := detail["ordinal"].(type) {
	case int:
		return value, value >= 0
	case int64:
		return int(value), value >= 0
	case float64:
		return int(value), value >= 0 && value == float64(int(value))
	default:
		return 0, false
	}
}

func directRouteAttemptKey(jobID string, ordinal int, revision string) string {
	return jobID + "\x00" + strconv.Itoa(ordinal) + "\x00" + revision
}

// directRouteOfferLease is retained for compatibility with projection tests.
// It is not an authorization lease and never permits a repeat.
const directRouteOfferLease = 10 * time.Minute

func directRouteProgress(events []map[string]any, candidates []routes.Candidate, _ time.Time) (next int, inFlight bool, pendingAttempt string) {
	attempt := ""
	revision := ""
	ordinal := -1
	answered := false
	for _, event := range events {
		if event["kind"] != "browser.direct_route" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if cleanup, _ := detail["cleanup_only"].(bool); cleanup {
			continue
		}
		eventOrdinal, ok := directRouteOrdinal(detail)
		if !ok || eventOrdinal < 0 || eventOrdinal >= len(candidates) {
			continue
		}
		eventRevision := stringDetail(detail, "route_revision")
		eventAttempt := stringDetail(detail, "drive_attempt_id")
		if eventAttempt == "" || eventRevision == "" || eventRevision != candidates[eventOrdinal].RouteRevision {
			continue
		}
		switch stringDetail(detail, "phase") {
		case "offered":
			if eventOrdinal == next {
				attempt, revision, ordinal, inFlight, answered = eventAttempt, eventRevision, eventOrdinal, true, false
			}
		case "result":
			if eventOrdinal != ordinal || eventAttempt != attempt || eventRevision != revision || !inFlight {
				continue
			}
			answered = true
			switch stringDetail(detail, "outcome") {
			case "success", "not_pdf":
				next++
				inFlight = false
				attempt, revision, ordinal = "", "", -1
			default:
				// foreign/login/terms/challenge and every unknown result are
				// terminal observations for this tuple but never advance.
				inFlight = true
			}
		}
	}
	if inFlight && !answered {
		// An elapsed offer lease is diagnostic only. It never authorizes a
		// repeat; the exact permit remains the at-most-once authority.
		return next, true, attempt
	}
	return next, inFlight, ""
}
func directRouteSucceeded(events []map[string]any) bool {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.direct_route" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		if stringDetail(detail, "phase") == "result" && stringDetail(detail, "outcome") == "success" {
			return true
		}
	}
	return false
}

// validateDirectRouteEnvelope checks the candidate at the emission boundary.
// The canonical path/escaping rules live in internal/routes so this bridge
// cannot drift into a second URL envelope implementation.
func validateDirectRouteEnvelope(candidate routes.Candidate) bool {
	return routes.ValidateCandidate(candidate) == nil
}

func (b *Bridge) directRouteEligible(row job.Row, accessMode string) bool {
	return accessMode == config.ModeDelegated &&
		b.cfg.Browser.DirectRoutesEnabled &&
		b.holder != nil &&
		b.holder.ID != legacySessionID &&
		compareVersion(b.holder.ExtensionVersion, DirectRouteMinExtensionVersion) >= 0
}

// browserOfferLatched projects durable job.latch events into the browser-only
// offer gate. It intentionally does not participate in resolver/API candidate
// selection. Focus is view-only: it cannot bypass this latch or mint a new
// drive epoch; only an explicit retry may reset that authority.
func (b *Bridge) browserOfferLatched(
	ctx context.Context,
	row job.Row,
	action job.HumanAction,
	override ...string,
) (bool, error) {
	events, err := b.jobs.Events(ctx, row.ID)
	if err != nil {
		return true, err
	}
	domain := actionSafetyDomain(b.cfg, row, action)
	offerHosts := b.browserOfferHosts(row, action, events)
	landingHost := ""
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	offerURL := RouteURL(inst, row.Work)
	if oaURL, ok := app.OABrowserHandoffURL(action.Detail); ok {
		offerURL = oaURL
	}
	if retrievalURL, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
		offerURL = retrievalURL
	}
	landingHost = strings.ToLower(strings.TrimSpace(resolverHost(offerURL)))
	if len(override) > 0 && override[0] != "" {
		domain = override[0]
	}
	if len(override) > 1 && override[1] != "" {
		landingHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(override[1]), "."))
		offerHosts = append(offerHosts, landingHost)
	}
	if domain == "" {
		return false, nil
	}
	for _, event := range events {
		if event["kind"] != providerLatchEventKind {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if stringDetail(detail, "safety_domain") != domain {
			continue
		}
		switch stringDetail(detail, "kind") {
		case "no_positive_effects":
			return true, nil
		case "drift":
			adapterID := stringDetail(detail, "adapter_id")
			adapterVersion := stringDetail(detail, "adapter_version")
			if adapterID == "" || adapterVersion == "" {
				continue
			}
			liveVersion := ""
			if b.holder != nil {
				liveVersion = b.holder.AdapterVersions[adapterID]
			}
			if extensionVersionNewer(adapterVersion, liveVersion) {
				continue
			}
			host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(stringDetail(detail, "host")), "."))
			if host == "" {
				// A drift observation may arrive without a page capture (for
				// example after a bridge restart). The durable safety domain is
				// still authoritative; missing host evidence must not turn a
				// latched route into an automatic re-offer.
				return true, nil
			}
			if (landingHost != "" && host == landingHost) || browserOfferHostMatches(offerHosts, host) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (b *Bridge) browserOfferHosts(row job.Row, action job.HumanAction, events []map[string]any, directURL ...string) []string {
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	offerURL := ""
	if len(directURL) > 0 {
		offerURL = strings.TrimSpace(directURL[0])
	}
	if offerURL == "" {
		offerURL = RouteURL(inst, row.Work)
		if target, ok := app.OABrowserHandoffURL(action.Detail); ok {
			offerURL = target
		}
		if target, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
			offerURL = target
		}
	}
	hosts := make([]string, 0, 4)
	offerHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(resolverHost(offerURL))), ".")
	offerHostIsProvider := browserOfferHostMatches(verifiedProviderHosts, offerHost)
	appendHost := func(raw string) {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if host == "" || slices.Contains(hosts, host) {
			return
		}
		hosts = append(hosts, host)
	}
	appendHost(offerHost)
	libKeyURL := LibKeyURL(inst, row.Work)
	if libKeyURL != "" && offerURL == libKeyURL {
		if host := resolverHost(inst.OpenURLBase); host != "" && host != libKeyHost {
			appendHost(host)
		}
	}
	// A resolver may redirect to a provider host that is not the resolver
	// landing host. Only durable page-capture evidence that belongs to the
	// declarative provider set is action-specific; do not attach the entire
	// global provider list to every institution route.
	for _, event := range events {
		if event["kind"] != "browser.page_capture" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(stringDetail(detail, "host"))), ".")
		if host == "" || !browserOfferHostMatches(verifiedProviderHosts, host) {
			continue
		}
		// When the actual route is already a known provider, evidence from
		// another provider cannot justify this offer. Resolver/institution
		// routes remain open to their reviewed provider landing evidence.
		if offerHostIsProvider && !browserOfferHostMatches([]string{offerHost}, host) {
			continue
		}
		appendHost(host)
	}
	return hosts
}

func browserOfferHostMatches(offerHosts []string, latchHost string) bool {
	if latchHost == "" {
		return false
	}
	for _, offerHost := range offerHosts {
		offerHost = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(offerHost)), ".")
		if offerHost == "" {
			continue
		}
		if offerHost == latchHost || strings.HasSuffix(offerHost, "."+latchHost) ||
			strings.HasSuffix(latchHost, "."+offerHost) {
			return true
		}
	}
	return false
}

// providerDriveEpochSuppressed reports whether a generic epoch is already
// started or terminal. A merely offered tuple still needs its job_offer frame
// after a daemon/extension hello; suppressing that frame would strand legacy
// ordinary handoffs.
func providerDriveEpochSuppressed(events []map[string]any, domain string) bool {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return true
	}
	current := ""
	started, result, successor := false, false, false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		tuple := providerDriveEpochKey(
			stringDetail(detail, "drive_attempt_id"),
			int64(intDetail(detail, "ordinal")),
			stringDetail(detail, "strategy"),
			stringDetail(detail, "revision"),
		)
		switch event["kind"] {
		case "browser.provider_drive_epoch_offered":
			if stringDetail(detail, "safety_domain") != domain ||
				!validProviderDriveEpochTuple(stringDetail(detail, "drive_attempt_id"), int64(intDetail(detail, "ordinal")), stringDetail(detail, "strategy"), stringDetail(detail, "revision")) {
				continue
			}
			if result {
				successor = true
			}
			current, started, result = tuple, false, false
		case "browser.provider_drive_epoch_started":
			if tuple == current {
				started = true
			}
		case "browser.provider_drive_epoch_result":
			if tuple == current {
				result = true
			}
		}
	}
	return current != "" && (started || result || successor)
}

// providerDriveEpochLease is retained only as a diagnostic age threshold.
// Lease expiry never changes permit status or authorizes a successor.
const providerDriveEpochLease = 10 * time.Minute

// driveEpochStalled is retained as a compatibility query for older callers.
// It is deliberately inert: elapsed time never releases a permit or mints a successor.
func (b *Bridge) driveEpochStalled(jobID, attempt string, ordinal int64) bool {
	return false
}

// latestProviderDriveEpoch derives the generic tuple from durable events.
func (b *Bridge) latestProviderDriveEpoch(jobID string) (string, int64, bool) {
	if b == nil || b.jobs == nil {
		return "", 0, false
	}
	events, err := b.jobs.Events(context.Background(), jobID)
	if err != nil {
		return "", 0, false
	}
	attempt, ordinal := "", int64(0)
	for _, event := range events {
		d, _ := event["detail"].(map[string]any)
		if event["kind"] == "browser.provider_drive_epoch_offered" {
			attempt, ordinal = stringDetail(d, "drive_attempt_id"), int64(intDetail(d, "ordinal"))
		}
	}
	return attempt, ordinal, attempt != ""
}

func (b *Bridge) latestHandoffSafetyDomain(jobID string) string {
	if b == nil || b.jobs == nil {
		return ""
	}
	events, err := b.jobs.Events(context.Background(), jobID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "browser.handoff_offered" {
			continue
		}
		detail, _ := events[i]["detail"].(map[string]any)
		if domain := strings.TrimSpace(stringDetail(detail, "safety_domain")); domain != "" {
			return domain
		}
	}
	return ""
}

func (b *Bridge) offer(row job.Row, action job.HumanAction, accessMode string) (json.RawMessage, error) {
	return b.offerAtURL(row, action, accessMode, "", false)
}

func (b *Bridge) offerAtURL(row job.Row, action job.HumanAction, accessMode, directURL string, forceNewEpoch bool) (json.RawMessage, error) {
	inst, _ := b.cfg.InstitutionFor(row.Policy.Resolver)
	offerURL := directURL
	if offerURL == "" {
		offerURL = RouteURL(inst, row.Work)
		if oaURL, ok := app.OABrowserHandoffURL(action.Detail); ok {
			offerURL = oaURL
		}
		if retrievalURL, ok := app.DocumentDeliveryRetrievalHandoffURL(action.Detail); ok {
			// ADR-0017's 2026-08-07 amendment: a fulfilled document-delivery
			// request's form-75 "View PDF" URL, not the institution's
			// ordinary resolver route.
			offerURL = retrievalURL
		}
	}
	var events []map[string]any
	if b.jobs != nil && row.ID != "" {
		var err error
		events, err = b.jobs.Events(context.Background(), row.ID)
		if err != nil {
			return nil, fmt.Errorf("read browser offer evidence: %w", err)
		}
	}
	hosts := b.browserOfferHosts(row, action, events, offerURL)
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
	driveAllowed := b.providerDriveEpochAvailable()
	if driveAllowed && b.jobs != nil {
		if live, err := b.jobs.LiveEffectPermit(context.Background()); err != nil {
			return nil, fmt.Errorf("read effect permit occupancy: %w", err)
		} else if live != nil {
			driveAllowed = false
		}
		if blockers, err := b.jobs.UnresolvedLegacyEffectBlockerCount(context.Background()); err != nil {
			return nil, fmt.Errorf("read legacy effect blockers: %w", err)
		} else if blockers > 0 {
			driveAllowed = false
		}
	}
	if driveAllowed {
		b.providerDriveEpochMu.Lock()
		attempt, ordinal, ok := b.latestProviderDriveEpoch(row.ID)
		domain := actionSafetyDomain(b.cfg, row, action)
		if b.jobs != nil {
			if durableDomain := b.latestHandoffSafetyDomain(row.ID); durableDomain != "" {
				domain = durableDomain
			}
		}
		if forceNewEpoch && ok && b.jobs != nil {
			if err := b.jobs.S.AppendEvent(context.Background(), row.ID, "browser.provider_drive_epoch_superseded", map[string]any{
				"drive_attempt_id": attempt, "ordinal": ordinal, "strategy": "generic", "revision": "1", "safety_domain": domain,
			}); err != nil {
				b.providerDriveEpochMu.Unlock()
				return nil, fmt.Errorf("record provider drive epoch supersession: %w", err)
			}
		}
		if forceNewEpoch || !ok {
			attempt, ordinal = newMsgID(), 0
			if b.jobs != nil {
				if err := b.jobs.S.AppendEvent(context.Background(), row.ID, "browser.provider_drive_epoch_offered", map[string]any{
					"drive_attempt_id": attempt, "ordinal": ordinal, "strategy": "generic", "revision": "1", "safety_domain": domain,
				}); err != nil {
					b.providerDriveEpochMu.Unlock()
					return nil, fmt.Errorf("record provider drive epoch offer: %w", err)
				}
			}
		}
		payload.DriveAttemptID = attempt
		payload.DriveOrdinal = &ordinal
		payload.DriveStrategy = "generic"
		payload.DriveRevision = "1"
		b.providerDriveEpochMu.Unlock()
	}

	// Federated login-routing: hand this job's institution Shibboleth entityID
	// and ProQuest account id to the extension so it can auto-select the
	// institution on a provider login wall and unlock ProQuest's link-resolver.
	payload.LoginEntityID = inst.ShibbolethEntityID
	payload.ProquestAccountID = inst.ProquestAccountID
	return b.frame(protocol.MsgJobOffer, row.ID, payload)
}

// providerDirectGetResult applies one strict direct observation only when its
// complete daemon-minted tuple still names the current in-flight route.
// providerDirectGetResult settles the exact durable direct_get permit first.
// Projection fences only decide whether the visible browser.direct_route result
// and safety latch are current; they never prevent cleanup of a held permit.
func (b *Bridge) providerDirectGetResult(ctx context.Context, jobID string, p *protocol.ProviderDirectGetResultPayload) error {
	return b.providerDirectGetResultForSession(ctx, jobID, p, true)
}

func (b *Bridge) providerDirectGetResultForSession(ctx context.Context, jobID string, p *protocol.ProviderDirectGetResultPayload, currentHolder bool) error {
	if p == nil || !validDirectResultTuple(p) {
		return nil
	}
	identity := job.EffectPermitIdentity{
		JobID: jobID, Kind: job.EffectKindDirectGet,
		DriveAttemptID: p.DriveAttemptID, Ordinal: p.Ordinal,
		Strategy: "direct_get", Revision: p.RouteRevision,
	}
	permit, err := b.jobs.GetEffectPermitByIdentity(ctx, identity)
	if err != nil {
		return err
	}
	if permit == nil {
		// A pre-permit result can arrive after the legacy epoch was imported
		// and the current permit generation disappeared.  Settle only the
		// exact blocker tuple; never append a result, latch, or successor.
		if legacyErr := b.jobs.SettleLegacyEffectBlocker(ctx, job.LegacyEffectBlockerInput{
			Kind:           job.EffectKindDirectGet,
			JobID:          jobID,
			DriveAttemptID: p.DriveAttemptID,
			Ordinal:        p.Ordinal,
			Strategy:       "direct_get",
			Revision:       p.RouteRevision,
		}); legacyErr != nil && !errors.Is(legacyErr, job.ErrEffectPermitStale) {
			return legacyErr
		}
		return nil
	}
	if permit.Status == job.EffectPermitSettled {
		// A duplicate result must preserve the existing visible projection and
		// must not append another cleanup audit.
		return nil
	}

	current := false
	envelopeInvalid := false
	var candidate routes.Candidate
	var events []map[string]any
	row, rowErr := b.jobs.Get(ctx, jobID)
	if rowErr == nil {
		candidates, candidateErr := b.directRouteCandidates(ctx, *row)
		if candidateErr == nil && p.Ordinal >= 0 && p.Ordinal < int64(len(candidates)) &&
			candidates[p.Ordinal].RouteRevision == p.RouteRevision {
			candidate = candidates[p.Ordinal]
			events, rowErr = b.jobs.Events(ctx, jobID)
			if rowErr == nil {
				attempt, attemptErr := b.jobs.MaterializationAttemptRevision(ctx, jobID)
				if attemptErr == nil && currentHolder && permit.JobAttemptRevision == attempt &&
					permit.BrowserHolderGeneration == int64(b.epoch) {
					ordinal, inFlight, _ := directRouteProgress(events, candidates, b.now())
					current = inFlight && int64(ordinal) == p.Ordinal &&
						candidate.RouteRevision == p.RouteRevision
					if current {
						// directRouteProgress has already established that this
						// tuple is the current unanswered projection. Keep the
						// exact offered event check here to reject malformed or
						// foreign history.
						offered := false
						for i := len(events) - 1; i >= 0; i-- {
							if events[i]["kind"] != "browser.direct_route" {
								continue
							}
							detail, _ := events[i]["detail"].(map[string]any)
							if intDetail(detail, "ordinal") != int(p.Ordinal) ||
								stringDetail(detail, "route_revision") != p.RouteRevision ||
								stringDetail(detail, "drive_attempt_id") != p.DriveAttemptID {
								continue
							}
							if stringDetail(detail, "phase") == "offered" {
								offered = true
							}
							break
						}
						current = offered
					}
				}
			}
		}
	}

	outcome := p.Outcome
	landing := p.LandingClass
	finalHost := ""
	finalPath := ""
	if current {
		finalHost = strings.ToLower(strings.TrimSpace(p.FinalHost))
		finalPath = p.FinalPath
		if outcome == "success" {
			expectedOrigin, originErr := url.Parse(candidate.AllowedOrigin)
			expectedURL, urlErr := url.Parse(candidate.URL)
			if originErr != nil || urlErr != nil || finalHost == "" ||
				finalHost != strings.ToLower(expectedOrigin.Hostname()) ||
				landing != "pdf" || finalPath == "" || strings.ContainsAny(finalPath, "?#") ||
				finalPath != expectedURL.EscapedPath() {
				outcome = "unknown"
				landing = "unknown"
				finalHost, finalPath = "", ""
				envelopeInvalid = true
			}
		}
	}
	// This event is always URL-free and is the atomic settlement audit. Mark
	// historical cleanup-only observations so the direct-route projection does
	// not mistake them for current visible results.
	resultDetail := map[string]any{
		"route_revision": p.RouteRevision, "ordinal": p.Ordinal,
		"drive_attempt_id": p.DriveAttemptID, "safety_domain": permit.SafetyDomainID,
		"outcome": outcome, "landing_class": landing,
	}
	if redacted := redactProviderDetail(p.Detail); redacted != "" {
		resultDetail["detail"] = redacted
	}
	required := []job.EffectPermitEvent{{Kind: "browser.provider_direct_get_result", Detail: resultDetail}}
	currentEvents := make([]job.EffectPermitEvent, 0, 2)
	if current {
		visible := map[string]any{
			"route_revision": p.RouteRevision, "ordinal": p.Ordinal,
			"drive_attempt_id": p.DriveAttemptID, "safety_domain": permit.SafetyDomainID,
			"phase": "result", "outcome": outcome, "landing_class": landing,
		}
		if finalHost != "" {
			visible["final_host"] = finalHost
		}
		if finalPath != "" {
			visible["final_path"] = finalPath
		}
		if redacted := redactProviderDetail(p.Detail); redacted != "" {
			visible["detail"] = redacted
		}
		if permit.Status != job.EffectPermitSettled {
			currentEvents = append(currentEvents, job.EffectPermitEvent{Kind: "browser.direct_route", Detail: visible})
		}
		if envelopeInvalid && permit.Status != job.EffectPermitSettled {
			currentEvents = append(currentEvents, job.EffectPermitEvent{Kind: providerLatchEventKind, Detail: map[string]any{
				"kind": "no_positive_effects", "safety_domain": permit.SafetyDomainID,
			}})
		}
	} else {
		cleanup := map[string]any{
			"route_revision": p.RouteRevision, "ordinal": p.Ordinal,
			"drive_attempt_id": p.DriveAttemptID, "safety_domain": permit.SafetyDomainID,
			"phase": "result", "outcome": outcome, "landing_class": landing,
			"cleanup_only": true,
		}
		if redacted := redactProviderDetail(p.Detail); redacted != "" {
			cleanup["detail"] = redacted
		}
		required = append(required, job.EffectPermitEvent{Kind: "browser.direct_route", Detail: cleanup})
	}
	settleGeneration := int64(b.epoch)
	if !currentHolder {
		settleGeneration = -1
	}
	_, _, err = b.jobs.SettleEffectPermit(ctx, job.EffectPermitSettleInput{
		Identity:                       identity,
		RequiredEvents:                 required,
		CurrentAttemptRevision:         permit.JobAttemptRevision,
		CurrentBrowserHolderGeneration: settleGeneration,
		CurrentEvents:                  currentEvents,
	})
	return err
}

func validDirectResultTuple(p *protocol.ProviderDirectGetResultPayload) bool {
	return p.DriveAttemptID != "" && p.Ordinal >= 0 && p.RouteRevision != ""
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
		return nil, fmt.Errorf("%w: outbound %s failed self-validation: %w", ErrOutboundFrame, msgType, err)
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
