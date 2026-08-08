// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio MV3 bridge service worker. Least-privilege handoff between the daemon
// (via the papio-native-host native-messaging host) and ordinary Chrome tabs.
//
// Invariants enforced here, not merely documented:
//   - Every inbound frame is re-parsed with parseBrowserMessage; a ProtocolError
//     drops the connection (fail closed).
//   - Outgoing frames are validated with the same parser before postMessage, so
//     the extension can never emit a malformed or privacy-violating frame.
//   - auth_pending/auth_returned carry timing only. URL/host/title are compared
//     locally and NEVER placed in any outgoing frame or persisted state.
//   - Exactly one broker-owned tab per job; downloads are adopted only when they
//     correlate to that tab, and only when a single candidate is unambiguous.
//
// The class is constructed with an injected BridgeDeps seam so the whole flow is
// unit-testable without a real chrome runtime.

import {
  BROWSER_PROTOCOL_VERSION,
  MAX_BROWSER_MESSAGE_BYTES,
  MsgPageCapture,
  MsgPageCaptureRequestResult,
  parseBrowserMessage,
  type ActivityEntryPayload,
  type BrowserMessage,
  type BrowserMessageType,
  type DeliveryRoute,
  type DeliverySessionEvidence,
  type PageAcquireAckPayload,
  type PageAcquirePayload,
  type PageBulkIdentifier,
  type PageBulkStatusItem,
  type PageBulkSubmitSource,
  type PageCapturePayload,
  type PageCaptureRequestPayload,
  type PageCaptureRequestResultPayload,
} from "./protocol";
import {
  emptyPageBulkScanStore,
  PAGE_BULK_SCAN_STORAGE_KEY,
  scanDocument,
  withPageBulkSnapshot,
  type DetectedPaper,
  type PageBulkScanStore,
  type PageBulkSnapshot,
  type ScanResult,
} from "./page-scan";
import {
  chromeBackend,
  clearPendingDelivery,
  emptyStore,
  findByJob,
  findByTab,
  patchJob,
  removeJob,
  startPendingDelivery,
  updatePendingDelivery,
  upsertJob,
  type ActiveJob,
  type PendingDelivery,
  type StateBackend,
  type StoreShape,
  type TermsConsent,
  type ProviderDrainLease,
  type FederatedLoginOwner,
  TERMS_CONSENT_KEY,
  WORK_WINDOW_KEY,
  HANDOFF_SURFACE_KEY,
  MANAGED_TAB_LEDGER_KEY,
  type HandoffSurface,
} from "./state";
import { isPDFPage, pdfSourceURL, sanitizePageHost } from "./deliver";
import {
  adapters,
  interpret,
  type AdapterContext,
  type AdapterSpec,
  type PageVerdict,
} from "./adapters/types";
import { observeUnknown, type ObserveChromeApi } from "./observe";
import {
  capturePage,
  encodePageCapture,
  residualLeak,
  sanitizeFixture,
  type PageCapture,
  type Provider,
  type Scenario,
} from "./capture";
import { chromeKeepaliveAPI, initKeepalive, isAuthenticationURL } from "./keepalive";
import type {
  FreshSessionEvidence,
  KeepaliveManager,
  KeepaliveOriginSnapshot,
  KeepaliveSnapshot,
} from "./keepalive";
import { routeResolverService, type ResolverRoute } from "./resolver";
import { detectAuthFailure } from "./authfail";

export const NATIVE_HOST = "com.orgmentem.papio";
const CHROME_PDF_VIEWER_HOST = "mhjfbmdgcfjbbpaeojofohoefgiehjai";
/** Lowest native daemon that can service this extension. 0.18.0 added the
 * optional `request_id` echo on `page_capture`; this extension always sends it
 * on a requested capture, and a daemon that predates the field rejects the
 * whole frame — which is fatal to the entire native-messaging session, not
 * just that capture. The floor cannot prevent the frame (it drives the popup's
 * "daemon is out of date" line, not emission), but it names the skew instead
 * of leaving the operator with an unexplained disconnect. 0.9.0, the previous
 * floor, renamed the wire access mode to "delegated"; older daemons emit
 * "maximal", which this extension rejects fail-closed. */
export const MIN_DAEMON_VERSION = "0.18.0";


const AUTH_EVIDENCE_TTL_MS = 30 * 60_000;
const QUEUED_HANDOFF_RELEASE_MS = 45_000;
/** At most two papio-created handoff tabs may be driving at once. The queue
 * below is intentionally worker-local: a service-worker restart may drop it,
 * and daemon re-offers recover any accepted job that was waiting for a tab. */
const HANDOFF_DRIVE_LIMIT = 2;
const HANDOFF_DRIVE_TIMEOUT_MS = 3 * 60_000;
/** Bounds how long a job may sit parked in waiting_for_session on another
 * job's shared federated-login claim. Past this, nothing has resumed it —
 * the owner may have simply walked away mid sign-in — so the marker clears
 * on its own and the job reverts to an ordinary parked_with_tab park: the
 * pre-feature presentation, visible and operator-actionable, instead of
 * waiting invisibly forever for a claim that may never retire. */
const SESSION_WAIT_TIMEOUT_MS = 10 * 60_000;
// (custom elements, React roots) upgrade after the tab reports complete and
// after the SSO landing. Re-drive the idempotent classify path on a bounded
// schedule so a slow render still reaches a decisive verdict.
const CLASSIFY_RETRY_MS = 2_500;
const MAX_CLASSIFY_RETRIES = 8;
// A challenge holds only its provider's queue for the same one minute the old
// bounded challenge probe used, then a fresh drain can reclaim it.
const PROVIDER_DRAIN_LEASE_MS = 24 * CLASSIFY_RETRY_MS;
/** Security checks and redirect-loop dead ends cool a provider for ten minutes
 * so an automated re-offer cannot immediately trip the same hardening again. */
const CHALLENGE_COOLDOWN_MS = 10 * 60_000;
/** A title-only OpenAthens error update can precede its body render. Recheck
 * exactly once, late enough for the bounded DOM marker probe to see it. */
const OPENATHENS_ERROR_RECHECK_MS = 1_500;
const OPENATHENS_LOGIN_HOST = "login.openathens.net";
const OPENATHENS_ERROR_TITLE = "Error | OpenAthens";
const PROVIDER_MULTI_LABEL_SUFFIXES: Record<string, true> = {
  "ac.uk": true,
  "co.uk": true,
  "com.au": true,
  "com.br": true,
  "com.cn": true,
  "com.mx": true,
  "co.jp": true,
  "co.nz": true,
  "edu.au": true,
  "gov.au": true,
  "gov.uk": true,
  "govt.nz": true,
  "net.au": true,
  "ne.jp": true,
  "org.au": true,
  "org.nz": true,
  "or.jp": true,
};

export type ChallengeBlockKind = "cloudflare" | "redirect_loop";

/** Canonical provider key: registrable hostname only, never a path or IdP. */
export function registrableProviderHost(host: string): string | undefined {
  const labels = host.toLowerCase().split(".").filter(Boolean);
  if (labels.length < 2 || labels.some((label) => !/^[a-z0-9-]+$/.test(label))) return undefined;
  const suffix = labels.slice(-2).join(".");
  const count = PROVIDER_MULTI_LABEL_SUFFIXES[suffix] === true ? 3 : 2;
  return labels.slice(-count).join(".");
}
// A job whose warm SSO session cannot complete human authentication would
// otherwise be re-driven on every daemon re-offer and worker spin-up forever,
// thrashing the provider (repeat navigations trip bot walls) and burning the
// resolver. Cap authentication drives per browser session; past it the job is
// reported human_auth_required (kept parked daemon-side, non-terminal) and no
// longer opens broker tabs until a fresh launch clears the budget.
const MAX_AUTH_ATTEMPTS = 3;
// The alarm that wakes an idle MV3 worker to re-establish the daemon connection
// so queued offers arrive without a keepalive tab or user activity. One minute
// is Chrome's reliable floor for a packed extension; it bounds delivery latency.
const KEEPALIVE_ALARM = "papio-keepalive";
const KEEPALIVE_ALARM_MINUTES = 1;
/** Bound a foreground runtime request without retaining it past the worker's
 * lifetime. Native frames themselves are bounded by the protocol parser. */
const TRIAGE_REQUEST_TIMEOUT_MS = 15_000;
const HELLO_WAIT_TIMEOUT_MS = 5_000;
const TRIAGE_SNAPSHOT_FEATURE = "triage_snapshot_v1";
const TRIAGE_SNAPSHOT_SCHEMA_2_FEATURE = "triage_snapshot_schema_v2";
const TRIAGE_SNAPSHOT_SCHEMA_3_FEATURE = "triage_snapshot_schema_v3";
const TRIAGE_COUNTS_SCHEMA_2_FEATURE = "triage_counts_schema_v2";
const SESSION_EVIDENCE_FEATURE = "session_evidence_v1";
const DELIVERY_CONTEXT_FEATURE = "delivery_context_v1";
const TRIAGE_MUTATIONS_FEATURE = "triage_mutations_v1";
const REVIEW_PREVIEW_FEATURE = "review_preview_v1";
const STATS_FEATURE = "browser_stats_v1";
const ACTIVITY_FEED_FEATURE = "activity_feed_v1";
const PAGE_CAPTURE_FEATURE = "page_capture_v1";
const PAGE_CAPTURE_REQUEST_FEATURE = "page_capture_request_v1";
const PAGE_CAPTURE_TERMS_FEATURE = "page_capture_terms_v1";
/** ADR-0019 Decision 7: page_bulk_status_request/page_bulk_submit_request. */
const PAGE_BULK_ACQUIRE_FEATURE = "page_bulk_acquire_v1";
const PDF_GRAB_FEATURE = "pdf_grab_v1";
const PDF_GRAB_CORRELATION_STORAGE_KEY = "papio_pdf_grab_correlations_v1";
/** ADR-0019 Decision 2: kept separate from acquisition/adapter host grants. */
const PAGE_BULK_ALLOWLIST_KEY = "papio_scanner_allowlist_v1";
const PAGE_CAPTURE_DEFAULT_SETTLE_MS = 3_000;
const PAGE_CAPTURE_NAV_TIMEOUT_MS = 30_000;
const TRIAGE_COUNTS_FRESH_MS = 3 * KEEPALIVE_ALARM_MINUTES * 60_000;
const SESSION_EVIDENCE_THROTTLE_MS = 60_000;
/** Daemon replies that settle a correlated `requestNative` call. `requestNative`
 * rejects any type outside this set before registering a wait, so wrappers and
 * variables cannot create a request that only fails later by timing out. */
const CORRELATED_RESULT_TYPES: ReadonlySet<BrowserMessageType> = new Set([
  "triage_snapshot_response",
  "triage_counts_response",
  "triage_decide_result",
  "human_action_resolve_result",
  "review_preview_result",
  "stats_response",
  "activity_response",
  "page_bulk_status_result",
  "page_bulk_submit_result",
  "delivery_reconcile_result",
  "pdf_grab_result",
]);
// Fallback for a manifest without an action popup; both build targets ship
// the pages under dist/ (see build.ts) and the manifest is the source of truth.
const POPUP_PAGE_PATH = "dist/popup.html";
/** Stable base title for papio's handoff group. A surfaced paper temporarily
 * appends its identity, but always returns to this title so a later worker can
 * rediscover the group without retaining page state. */
const HANDOFF_GROUP_TITLE = "papio";
const HANDOFF_GROUP_TITLE_MAX_TITLE_LENGTH = 72;
/** Paper labels are transient; the stable prefix is the ownership marker that
 * lets a reloaded worker find the physical group again. */
function isHandoffGroupTitle(title: string | undefined): boolean {
  return title === HANDOFF_GROUP_TITLE || title?.startsWith(`${HANDOFF_GROUP_TITLE} — `) === true;
}

export interface BadgeState {
  connectionStatus: StoreShape["connectionStatus"];
  reauthNeeded: boolean;
  authBlockers: number;
  /** Number of active jobs left on a provider security check/dead-end page. */
  challengeBlocked?: number;
  blockedHosts: number | readonly string[];
  ungrantedResolvers: number;
  triageCount: number | undefined;
}

export interface BadgeResult {
  text: string;
  color: string;
  tooltip: string;
}

/** Compute the one toolbar badge used by every background subsystem.
 * Precedence is documented and intentional: disconnected daemon (gray) >
 * reauthentication (orange) > sign-in blockers (orange) > challenge-blocked
 * jobs (orange) > blocked providers (orange) > resolver grants (blue) >
 * triage (blue) > blank. */
export function computeBadge(state: BadgeState): BadgeResult {
  const blockedHostCount =
    typeof state.blockedHosts === "number"
      ? Math.max(0, Math.trunc(state.blockedHosts))
      : state.blockedHosts.length;
  const challengeBlockedCount =
    typeof state.challengeBlocked === "number" ? Math.max(0, Math.trunc(state.challengeBlocked)) : 0;
  const authBlockerCount = Math.max(0, Math.trunc(state.authBlockers));
  const resolverCount = Math.max(0, Math.trunc(state.ungrantedResolvers));
  const triageCount =
    typeof state.triageCount === "number" && Number.isFinite(state.triageCount)
      ? Math.max(0, Math.trunc(state.triageCount))
      : undefined;
  if (state.connectionStatus !== "connected") {
    return { text: "!", color: "#777777", tooltip: "papio: daemon disconnected" };
  }
  if (state.reauthNeeded) {
    return { text: "!", color: "#b06000", tooltip: "papio: institution sign-in needed" };
  }
  if (authBlockerCount > 0) {
    return {
      text: String(authBlockerCount),
      color: "#b06000",
      tooltip: `papio: ${authBlockerCount} paper${authBlockerCount === 1 ? "" : "s"} waiting on your institution sign-in`,
    };
  }
  if (challengeBlockedCount > 0) {
    return {
      text: String(challengeBlockedCount),
      color: "#b06000",
      tooltip: `papio: ${challengeBlockedCount} security check${challengeBlockedCount === 1 ? "" : "s"} need your attention`,
    };
  }
  if (blockedHostCount > 0) {
    const host =
      typeof state.blockedHosts === "number" ? undefined : state.blockedHosts[0];
    const tooltip =
      blockedHostCount === 1 && typeof host === "string"
        ? `papio: ${host} needs browser access`
        : `papio: ${blockedHostCount} provider hosts need browser access`;
    return { text: String(blockedHostCount), color: "#b06000", tooltip };
  }
  if (resolverCount > 0) {
    return {
      text: String(resolverCount),
      color: "#1a73e8",
      tooltip: `papio: ${resolverCount} library resolver permission${resolverCount === 1 ? "" : "s"} need attention`,
    };
  }
  if (triageCount !== undefined && triageCount > 0) {
    return {
      text: String(triageCount),
      color: "#1a73e8",
      tooltip: `papio: ${triageCount} pending triage item${triageCount === 1 ? "" : "s"}`,
    };
  }
  return {
    text: "",
    color: "#1a73e8",
    tooltip: triageCount === 0 ? "papio: no pending triage items" : "papio: connected",
  };
}
export type BridgeSessionState = KeepaliveSnapshot & {
  releasedAuthJobs: number;
  /** Epoch ms of the most recent release; keys once-per-event popup notices. */
  releasedAuthJobsAt: number | null;
};



/** Whether this adapter's SPA must render outside the minimized work window. */
export function needsVisibleWindow(spec: AdapterSpec | undefined): boolean {
  return spec?.requiresVisible === true;
}

export type DrivenPageAssessmentKind = "normal" | "challenge" | "redirect_loop";
export interface DrivenPageAssessment {
  kind: DrivenPageAssessmentKind;
}

/**
 * Cloudflare/Turnstile marker probe. Keep this function self-contained:
 * chrome.scripting serializes it into the provider page's isolated world.
 * Only bounded structural markers and page-authored text are inspected; no
 * page text is returned to the extension.
 */
export function isBotChallenge(doc: Document | null): boolean {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  const structural =
    root.querySelector(
      'script[src*="/cdn-cgi/challenge-platform/"], ' +
        'script[src*="challenges.cloudflare.com/turnstile/"], ' +
        'input[name="cf-turnstile-response"], ' +
        '#challenge-form, .cf-turnstile, [id*="cf-chl-"], [class*="cf-chl-"], ' +
        '#captcha-box .main-wrapper[role="main"]',
    ) !== null;
  return (
    structural ||
    /are\s+you\s+a\s+robot\??/i.test(title) ||
    /verify\s+you\s+are\s+human/i.test(`${title}\n${text}`)
  );
}

/** OpenAthens and browser redirect-loop dead ends remain human-visible.
 * `openAthensHost` is supplied only after the tracked tab's origin is verified;
 * keeping it explicit prevents an OpenAthens-looking provider page from
 * triggering the origin-specific code/phrase markers. */
export function isRedirectLoopPage(doc: Document | null, openAthensHost = false): boolean {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  const genericLoop =
    /\btoo\s+many\s+redirects\b|\berr_too_many_redirects\b|\bredirect\s+loop\b/i.test(
      `${title}\n${text}`,
    );
  const openAthensLoop =
    openAthensHost &&
    title === "Error | OpenAthens" &&
    /\btoo\s+many\s+redirects\b|\bservice\s+provider\s+redirecting\b|\b(?:GA|OA)-AP-\d{4}-\d{2}\b/i.test(
      text,
    );
  return (!openAthensHost && genericLoop) || openAthensLoop;
}

/**
 * Single bounded assessment injected before adapter interpretation. It keeps
 * challenge/error pages from being mistaken for articles or login forms.
 * Do not reference outer functions: this body is serialized by Chrome.
 */
export function assessDrivenPage(doc: Document | null, openAthensHost = false): DrivenPageAssessment {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  const structural =
    root.querySelector(
      'script[src*="/cdn-cgi/challenge-platform/"], ' +
        'script[src*="challenges.cloudflare.com/turnstile/"], ' +
        'input[name="cf-turnstile-response"], ' +
        '#challenge-form, .cf-turnstile, [id*="cf-chl-"], [class*="cf-chl-"], ' +
        '#captcha-box .main-wrapper[role="main"]',
    ) !== null;
  if (
    structural ||
    /are\s+you\s+a\s+robot\??/i.test(title) ||
    /verify\s+you\s+are\s+human/i.test(`${title}\n${text}`)
  ) {
    return { kind: "challenge" };
  }
  const genericLoop =
    /\btoo\s+many\s+redirects\b|\berr_too_many_redirects\b|\bredirect\s+loop\b/i.test(
      `${title}\n${text}`,
    );
  const openAthensLoop =
    openAthensHost &&
    title === "Error | OpenAthens" &&
    /\btoo\s+many\s+redirects\b|\bservice\s+provider\s+redirecting\b|\b(?:GA|OA)-AP-\d{4}-\d{2}\b/i.test(
      text,
    );
  if ((!openAthensHost && genericLoop) || openAthensLoop) return { kind: "redirect_loop" };
  return { kind: "normal" };
}


export interface Listenable<A extends unknown[]> {
  addListener(cb: (...args: A) => void): void;
}

export interface NativePort {
  postMessage(msg: object): void;
  onMessage: Listenable<[unknown]>;
  onDisconnect: Listenable<[]>;
  disconnect(): void;
}


export interface TabInfo {
  id?: number | undefined;
  url?: string | undefined;
  status?: string | undefined;
  /** Page title when available; used only for local IdP failure-page
   * heuristics and never sent over the bridge. */
  title?: string | undefined;
  /** Chrome sets this on a tab opened by another tab (e.g. a provider's
   * "download" that opens the PDF in a new viewer tab). Correlates the viewer
   * tab back to the tracked handoff tab that spawned it. */
  windowId?: number | undefined;
  /** Chrome's group membership id; -1 means the tab is not grouped. */
  groupId?: number | undefined;
  openerTabId?: number | undefined;
  /** Chrome marks the keepalive resolver tab pinned; papio's broker tabs never
   * are. Lets the idle-close check keep a keepalive-pinned work window alive. */
  pinned?: boolean | undefined;
  /** Whether the tab is the selected tab in its window. Orphan cleanup never
   * closes a tab the user is actively looking at. */
  active?: boolean | undefined;
}
/** Normalize only the fragment component for managed-tab dedupe. Chrome may
 * canonicalize a URL while creating a tab, so use URL.href when possible and
 * retain a conservative string fallback for malformed values. */
export function normalizeManagedTabURL(rawURL: string): string {
  try {
    const url = new URL(rawURL);
    url.hash = "";
    return url.href;
  } catch {
    const fragment = rawURL.indexOf("#");
    return fragment < 0 ? rawURL : rawURL.slice(0, fragment);
  }
}

/** Return the live tab that should be reused for a managed open. A tracked job
 * wins even when its current document has navigated away; otherwise exact URL
 * equality (ignoring only fragments) prevents duplicate browser tabs. */
export function findManagedTab(
  candidates: readonly TabInfo[],
  url: string,
  trackedTabID?: number,
): TabInfo | undefined {
  if (trackedTabID !== undefined) {
    const tracked = candidates.find((candidate) => candidate.id === trackedTabID);
    if (tracked !== undefined) return tracked;
  }
  const normalized = normalizeManagedTabURL(url);
  return candidates.find(
    (candidate) => candidate.id !== undefined && candidate.url !== undefined && normalizeManagedTabURL(candidate.url) === normalized,
  );
}
export type ManagedTabPurpose = "handoff" | "inbox-open" | "session-signin" | "redrive" | "reoffer" | "capture";
export interface OpenManagedTabOptions {
  url: string;
  jobId?: string;
  purpose: ManagedTabPurpose;
  /** Legacy in-window visibility for a new handoff; work-window placement
   * still follows its adapter-driven visibility rules. */
  surfaceFallback?: boolean;
  /** Set false when the caller just surfaced the tab (e.g. stale-page
   * recovery) and only needs managed URL reuse/navigation. */
  focusExisting?: boolean;
}


export interface TabChangeInfo {
  url?: string | undefined;
  status?: string | undefined;
  /** Chrome fires a title-only update when a document's title resolves after
   * the load completes. Needed because some IdP failure pages are classifiable
   * only by title (see onTabUpdated). */
  title?: string | undefined;
}

export interface WindowInfo {
  id?: number | undefined;
  /** "minimized" | "normal" | ... — used only to avoid un-maximizing a normal
   * window when surfacing. */
  state?: string | undefined;
  /** Populated by windows.create when the window is created with a URL. */
  tabs?: TabInfo[] | undefined;
}

export interface TabGroupInfo {
  id: number;
  collapsed: boolean;
  title?: string | undefined;
  /** Groups are scoped to a browser window, so this must agree with every tab
   * moved into the group. */
  windowId?: number | undefined;
}


export interface DownloadItemLike {
  id: number;
  state?: string | undefined;
  filename?: string | undefined;
  fileSize?: number | undefined;
  totalBytes?: number | undefined;
  bytesReceived?: number | undefined;
  referrer?: string | undefined;
  finalUrl?: string | undefined;
  url?: string | undefined;
  mime?: string | undefined;
  /** Present in the test fake and some Chromium builds; absent in stable
   * chrome.downloads.DownloadItem, in which case we fall back to referrer. */
  tabId?: number | undefined;
}

export interface DownloadDeltaLike {
  id: number;
  state?: { current?: string | undefined } | undefined;
  filename?: { current?: string | undefined } | undefined;
}
export interface PdfGrabCorrelation {
  scanID: string;
  tabID: number;
  state: string;
  downloadID: number;
  steeringPath: string;
  url: string;
}

export interface BridgeDeps {
  connectNative(name: string): NativePort;
  manifestVersion: string;
  randomUUID(): string;
  now(): number;
  /** Injectable timers so tests control reconnect backoff and queue release. */
  setTimeout(fn: () => void | Promise<void>, ms: number): void;
  backend: StateBackend;
  tabs: {
    create(props: { url: string; active: boolean; windowId?: number }): Promise<TabInfo>;
    get(tabID: number): Promise<TabInfo>;
    reload(tabID: number): Promise<unknown>;
    remove(tabID: number): Promise<void>;
    /** Optional: surface a work-window tab on human auth ({active}), or
     * navigate the handoff tab to a federated-login route ({url}). */
    update?(tabID: number, props: { active?: boolean; url?: string }): Promise<unknown>;
    onUpdated: Listenable<[number, TabChangeInfo, TabInfo]>;
    /** Used only for the singleton inbox tab. */
    sendMessage?(tabID: number, message: object): Promise<unknown>;
    query?(query: { url?: string; groupId?: number }): Promise<TabInfo[]>;
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
    /** ADR-0013 privileges the focused tab: an activation with no matching
     * navigation event is still evidence the operator is looking at that
     * origin's resolver page. */
    onActivated: Listenable<[{ tabId: number; windowId: number }]>;
    /** Optional (Chrome): add tabs to a group, creating one when groupId is
     * omitted. Returns the group id. Absent on platforms without tab groups. */
    group?(opts: { tabIds: number[]; groupId?: number }): Promise<number>;
  };
  /** Extension-page broadcast channel (runtime.onMessage), distinct from tabs.sendMessage content-script delivery. */
  runtimeSendMessage?(message: object): Promise<unknown>;
  /** chrome.windows seam. When present (and the user setting allows), broker
   * tabs use one dedicated minimized "work window" instead of the user's tab
   * strip, except an adapter whose SPA needs a visible window. A tab otherwise
   * platforms without the API — tabs then open with the legacy visibility rules. */
  windows?: {
    create(props: { url: string; focused: boolean; state: "minimized" | "normal" }): Promise<WindowInfo>;
    get(windowID: number): Promise<WindowInfo>;
    update(
      windowID: number,
      props: { focused?: boolean; state?: "normal" | "minimized"; drawAttention?: boolean },
    ): Promise<unknown>;
    /** Close papio's dedicated work window once it holds no papio tabs, so the
     * background window never accumulates across handoffs. */
    remove(windowID: number): Promise<void>;
  };
  tabGroups?: {
    get(groupID: number): Promise<TabGroupInfo>;
    update(
      groupID: number,
      props: { collapsed?: boolean; title?: string; color?: string },
    ): Promise<unknown>;
    /** Find groups by title. Used to rediscover papio's orphaned handoff group
     * after an extension reload clears the in-memory id but leaves the group. */
    query(props: { title?: string }): Promise<TabGroupInfo[]>;
  };
  downloads: {
    search(query: { id: number }): Promise<DownloadItemLike[]>;
    /** Start a browser-managed download. The resolver-provided offer URL stays
     * local to the extension/browser and is never put in a native frame. */
    download(options: {
      url: string;
      filename?: string;
      conflictAction: "uniquify";
      saveAs: false;
    }): Promise<number>;
    removeFile(downloadID: number): Promise<void>;
    erase(query: { id: number }): Promise<number[]>;
    onCreated: Listenable<[DownloadItemLike]>;
    onChanged: Listenable<[DownloadDeltaLike]>;
    /** chrome.downloads.onDeterminingFilename — Chrome-only; absent elsewhere.
     * The listener may call suggest() synchronously to relocate a download to
     * a relative path under the browser's Downloads directory. */
    onDeterminingFilename?: Listenable<
      [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
    >;
  };
  /** Registered declarative provider adapters. Injected so hello's
   * adapter_versions map and the classifier are unit-testable. */
  adapterSpecs: AdapterSpec[];
  /** Inject only serializable DOM probes into tracked, granted provider tabs so
   * page inspection cannot escape the host-permission boundary. */
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      // `never[]` accepts concrete injected signatures without disabling type
      // checking at this serialization boundary.
      func: (...args: never[]) => unknown;
      args?: unknown[];
    }): Promise<{ result?: unknown }[]>;
  };
  /** The observation path needs durable quota state but must not depend on a
   * browser global, so tests can prove the capture frame reaches the bridge. */
  captureStorage?: ObserveChromeApi["storage"];
  /** chrome.permissions seam. Adapter execution is gated on an explicit
   * optional-host-permission grant for the provider origin. */
  permissions: {
    contains(perm: { origins: string[] }): Promise<boolean>;
  };
  /** Durable user settings (chrome.storage.local): informed consent for
   * auto-accepting publisher terms, and the background work-window toggle. */
  settings: {
    getTermsConsent(): Promise<TermsConsent>;
    setTermsConsent(value: Exclude<TermsConsent, undefined>): Promise<void>;
    /** Tri-state surface choice. `tab-group` degrades to `work-window` if
     * tabGroups is absent. */
    getHandoffSurface(): Promise<HandoffSurface>;
  };
  /** Durable managed-tab ledger (chrome.storage.local). The session store dies
   * with an extension reload, orphaning every tab papio opened in its previous
   * life; this ledger survives the reload so the popup can offer a one-click
   * cleanup instead of leaking those tabs forever. Optional: absent disables
   * durable orphan ownership tracking. */
  tabLedger?: {
    load(): Promise<Record<string, number>>;
    save(entries: Record<string, number>): Promise<void>;
  };
  /** Ephemeral scan snapshots (chrome.storage.session): never chrome.storage
   * local/sync, never persisted, never sent to the daemon (ADR-0019
   * Decision 4). Optional so callers that never exercise page-bulk scanning
   * can omit it; a missing dep degrades scanning to "not saved" rather than
   * throwing. */
  pageBulkScans?: {
    get(): Promise<PageBulkScanStore>;
    set(store: PageBulkScanStore): Promise<void>;
  };
  /** Session correlation for PDF-grab terminal pushes across SW restarts. */
  pdfGrabCorrelations?: {
    get(): Promise<Record<string, PdfGrabCorrelation>>;
    set(value: Record<string, PdfGrabCorrelation>): Promise<void>;
  };
  /** Scanner-scoped origin allowlist (chrome.storage.local), kept and
   * revocable separately from acquisition/adapter host-permission grants
   * (ADR-0019 Decision 2). An explicit scan click is v1's consent for that
   * one scan regardless of allowlist membership; this list only records an
   * "always allow on this site" choice for future ambient features. */
  scannerAllowlist?: {
    get(): Promise<string[]>;
    set(origins: string[]): Promise<void>;
  };
  /** Toolbar badge for connection health. Kept injectable so bridge logic has
   * no dependency on a particular browser global. */
  action: {
    setBadgeText(details: { text: string }): Promise<void>;
    setBadgeBackgroundColor(details: { color: string }): Promise<void>;
    setTitle?(details: { title: string }): Promise<void>;
  };
  /** chrome.alarms seam. An MV3 service worker sleeps after ~30s idle; a
   * periodic alarm is the only thing that wakes it, so pending daemon offers
   * reach an idle worker with no keepalive tab or user activity. */
  alarms: {
    create(name: string, info: { periodInMinutes: number }): void;
    onAlarm: Listenable<[{ name: string }]>;
  };
}

interface DownloadTrack {
  ids: Set<number>;
  ambiguous: boolean;
  directOffer: boolean;
  delivery?: boolean;
  route?: DeliveryRoute;
  sessionEvidence?: DeliverySessionEvidence;
}

interface PdfGrabTrack {
  ids: Set<number>;
  tabID: number;
  scanID: string;
  url: string;
  steeringPath: string;
}
interface StalledAuthHandoff {
  url: string;
  providerHosts: string[];
  expected?: { title?: string; doi?: string };
  requiresAuth?: boolean;
}
interface QueuedHandoffDrive {
  jobID: string;
  purpose: ManagedTabPurpose;
  surfaceFallback?: boolean;
  focusExisting?: boolean;
}

interface HandoffDrive {
  tabID: number;
  token: object;
}

type NativeRequestKind = "response" | "transport" | "timeout";

interface NativeRequestResult {
  kind: NativeRequestKind;
  payload?: Record<string, unknown>;
  code?: string;
  message?: string;
}

interface PendingNativeRequest {
  expectedType: BrowserMessageType;
  resolve(result: NativeRequestResult): void;
}

type ClassifyRetryKind = "unknown";

interface ClassifyRetry {
  kind: ClassifyRetryKind;
  attempts: number;
}

interface BrokerFailure {
  ok: false;
  error: { code: string; message: string };
}

interface BrokerSuccess<T extends Record<string, unknown>> {
  ok: true;
}

type BrokerReply<T extends Record<string, unknown>> = BrokerFailure | (BrokerSuccess<T> & T);

interface DeliveryStartPayload {
  tab_id: number;
  url: string;
  doi?: string;
  title?: string;
}

type DeliveryState = "sending" | "downloaded" | "failed" | "adopted" | "idle";

type DeliveryReply = BrokerReply<{
  state: DeliveryState;
  job_id?: string;
  duplicate?: boolean;
  message?: string;
}>;

function hostMatches(host: string, providerHosts: string[]): boolean {
  return providerHosts.some((h) => host === h || host.endsWith("." + h));
}

/** Parse a released semver (with an optional leading v) without retaining its
 * prerelease identifier: callers only need to distinguish release from pre-release. */
function parseSemver(version: string): [number, number, number, boolean] | null {
  const match = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$/.exec(version);
  if (match === null) return null;
  const [, major, minor, patch, prerelease] = match;
  return [Number(major), Number(minor), Number(patch), prerelease !== undefined];
}

/** True when a released semver (with an optional leading v) is older than the
 * bridge's compatibility floor. Unparseable daemon banners stay connected: the
 * daemon has already completed the protocol handshake. */
function isSemverLowerThan(version: string, minimum: string, includePrerelease = true): boolean {
  const actual = parseSemver(version);
  const floor = parseSemver(minimum);
  if (actual === null || floor === null) return false;
  for (let i = 0; i < 3; i += 1) {
    if (actual[i] !== floor[i]) return actual[i]! < floor[i]!;
  }
  return includePrerelease && actual[3] && !floor[3];
}

/** Whether a stamped extension release has a newer daemon version available.
 * Buildless development bundles deliberately carry the 0.0.0-dev sentinel. */
export function hasDaemonUpdateHint(daemonVersion: string | null, stampedVersion: string): boolean {
  if (daemonVersion === null || stampedVersion === "" || stampedVersion === "0.0.0-dev") return false;
  return isSemverLowerThan(daemonVersion, stampedVersion, false);
}

/** Capabilities are valid only for the hello exchange on the current port. */
function clearNegotiationState(store: StoreShape): StoreShape {
  return {
    ...store,
    daemonFeatures: [],
    resolverOrigins: [],
  };
}

/** Narrow a job_offer's optional `expected` block to the resolver-declared work
 * hints we persist for classification. Never carries an IdP value. */
function parseExpected(raw: unknown): { title?: string; doi?: string } | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const e = raw as Record<string, unknown>;
  const title = typeof e["title"] === "string" ? e["title"] : undefined;
  const doi = typeof e["doi"] === "string" ? e["doi"] : undefined;
  if (title === undefined && doi === undefined) return undefined;
  return {
    ...(title !== undefined ? { title } : {}),
    ...(doi !== undefined ? { doi } : {}),
  };
}

/** Compare only the stable, non-secret part of a provider download URL.
 * Chrome may normalize a signed query before onDeterminingFilename fires. */
function sameDownloadRoute(a: string, b: string): boolean {
  try {
    const left = new URL(a);
    const right = new URL(b);
    return left.origin === right.origin && left.pathname === right.pathname;
  } catch {
    return false;
  }
}

/** Recognize public direct-file routes without guessing from content. These
 * paths can be handed to chrome.downloads before a browser tab is needed. */
function isDirectFileOffer(raw: string): boolean {
  try {
    const path = new URL(raw).pathname.toLowerCase();
    return (
      path.endsWith(".pdf") ||
      path.includes("/content/pdf/") ||
      path.includes("/doi/pdf/") ||
      /(?:^|\/)pdf(?:\/|$)/.test(path)
    );
  } catch {
    return false;
  }
}

/** Self-contained provider-link extractor, injected verbatim into the tracked
 * page. It returns only an HTTPS href from the declared selector. The signed
 * URL remains in extension memory and is handed directly to
 * chrome.downloads.download; it never crosses native messaging or storage.
 *
 * Keep this function self-contained: executeScript serializes the injected
 * function alone, not its module-level dependencies, so the empty/self URL
 * guard below is deliberately duplicated in extractMetaURL rather than shared.
 *
 * The self-URL check compares origin + pathname + search rather than the raw
 * href string: WHATWG serializes a non-null empty query as a trailing "?", so
 * `content="?"` on https://p/a produced "https://p/a?" — one character away
 * from the page's own href and therefore NOT caught by a literal href ===
 * href comparison, while `URL#search` already normalizes both the "?" and
 * no-query forms to "". A URL carrying userinfo (`https://x@p/a`) survives
 * href serialization unchanged while still addressing the identical page, so
 * it is rejected outright rather than folded into the equality check. */
function extractDownloadURL(selector: string): string | null {
  const el = document.querySelector<HTMLAnchorElement>(selector);
  if (!(el instanceof HTMLAnchorElement)) return null;
  const raw = el.getAttribute("href")?.trim() ?? "";
  if (raw.length === 0) return null;
  try {
    const u = new URL(raw, location.href);
    if (u.protocol !== "https:") return null;
    if (u.username !== "" || u.password !== "") return null;
    const page = new URL(location.href);
    const isSelf = u.origin === page.origin && u.pathname === page.pathname && u.search === page.search;
    return isSelf ? null : u.href;
  } catch {
    return null;
  }
}

/** Self-contained meta-tag PDF-URL extractor, injected verbatim into the tracked
 * page. Returns only an HTTPS URL from the named meta tag's content. The URL
 * stays in extension memory and is handed directly to chrome.downloads.download;
 * it never crosses native messaging or storage.
 *
 * An empty or self-resolving content attribute is rejected, because
 * `new URL("", base)` resolves to BASE — which is https and truthy, so
 * `<meta name="citation_pdf_url" content="">` used to yield the landing page's
 * own URL. The adapter `article` rule only checks that the tag EXISTS, so such
 * a page classified as an article and then handed its own landing HTML to
 * chrome.downloads.download as if it were the PDF; payload validation caught
 * it downstream, but the fetch was wasted and the failure was misattributed to
 * the provider. A query string still differentiates: a provider's
 * `?download=true` form of the same path is a real, distinct download URL.
 *
 * The self-URL check compares origin + pathname + search rather than the raw
 * href string, and rejects userinfo outright, for the same reason documented
 * in extractDownloadURL above — see that comment for the two escapes this
 * closes. */
function extractMetaURL(metaName: string): string | null {
  const el = document.querySelector(`meta[name="${metaName}"]`);
  if (!(el instanceof HTMLMetaElement)) return null;
  const raw = el.getAttribute("content")?.trim() ?? "";
  if (raw.length === 0) return null;
  try {
    const u = new URL(raw, location.href);
    if (u.protocol !== "https:") return null;
    if (u.username !== "" || u.password !== "") return null;
    const page = new URL(location.href);
    const isSelf = u.origin === page.origin && u.pathname === page.pathname && u.search === page.search;
    return isSelf ? null : u.href;
  } catch {
    return null;
  }
}

/** Self-contained resolver for a provider's direct PDF endpoint, injected into
 * the tracked page. It fills {N}/{id} in urlTemplate from idPattern's capture
 * groups against the page URL, and only when the declared entitled download
 * control is present (the same signal the `article` verdict uses). For method
 * "api" the built URL returns JSON carrying the real download URL in jsonField
 * (fetched with the page's session cookies). The resolved URL is handed to
 * chrome.downloads.download; it never crosses native messaging or storage. */
export async function resolveDownloadURL(
  selector: string,
  idPattern: string | null,
  urlTemplate: string | null,
  jsonField: string | null,
): Promise<string | null> {
  if (!urlTemplate) return null;
  if (!document.querySelector(selector)) return null;
  let built = urlTemplate;
  if (idPattern) {
    const m = location.href.match(new RegExp(idPattern));
    if (!m) return null;
    built = built.replace(/\{(\d+|id)\}/g, (_, k: string) => m[k === "id" ? 1 : Number(k)] ?? "");
  }
  let target = built;
  if (jsonField) {
    try {
      const r = await fetch(built, { credentials: "include" });
      if (!r.ok) return null;
      const data = (await r.json()) as Record<string, unknown>;
      const raw = data[jsonField];
      if (typeof raw !== "string") return null;
      target = raw;
    } catch {
      return null;
    }
  }
  try {
    const u = new URL(target, location.href);
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** Self-contained click of a terms-and-conditions accept control, found by an
 * explicit fixture-backed selector or accessible text inside an open modal
 * (piercing shadow roots). Runs ONLY when the user has recorded informed
 * consent; the extension never guesses terms controls otherwise. Returns
 * whether a matching control was clicked. */
export function clickTermsAccept(
  modalSelector: string,
  textAny: string[],
  controlSelector: string | null = null,
): boolean {
  const modal = document.querySelector(modalSelector);
  if (!modal) return false;
  const click = (el: Element): boolean => {
    const target = el as HTMLElement;
    if (typeof target.click !== "function") return false;
    const shadow = (target as HTMLElement & { shadowRoot?: ShadowRoot | null }).shadowRoot;
    const inner = shadow?.querySelector<HTMLElement>("#button-element");
    (inner ?? target).click();
    return true;
  };
  if (controlSelector !== null) {
    const control = modal.matches(controlSelector) ? modal : modal.querySelector(controlSelector);
    return control === null ? false : click(control);
  }

  const needles = textAny.map((t) => t.toLowerCase());
  const walk = (root: ParentNode): boolean => {
    for (const el of Array.from(root.querySelectorAll("*"))) {
      // Click only a genuine control, never a wrapping container whose text
      // merely includes the accept label: a modal footer <div> holds both
      // "Cancel" and "Accept and download", and clicking it is a no-op. The
      // real control is button-like (JSTOR's is an mfe-*-button with a shadow
      // #button-element).
      const tag = el.tagName.toLowerCase();
      const submit = tag === "input" && el.getAttribute("type")?.toLowerCase() === "submit";
      const actionable =
        tag === "button" ||
        tag === "a" ||
        submit ||
        el.getAttribute?.("role") === "button" ||
        tag.endsWith("-button");
      if (actionable) {
        const value = submit ? (el.getAttribute("value") ?? "") : "";
        const formContext =
          submit && value.trim() === ""
            ? ((el.closest("form") as HTMLElement | null)?.innerText ?? "")
            : "";
        const label =
          ((el as HTMLElement).innerText ?? "") +
          " " +
          (el.getAttribute?.("aria-label") ?? "") +
          " " +
          value +
          " " +
          formContext;
        if (needles.some((n) => label.toLowerCase().includes(n)) && click(el)) return true;
      }
      const sub = (el as HTMLElement & { shadowRoot?: ShadowRoot | null }).shadowRoot;
      if (sub && walk(sub)) return true;
    }
    return false;
  };
  return walk(modal);
}



/** Self-contained declared click, optionally through one explicitly named
 * control in an open shadow root. It may then wait for one declared in-page
 * gate or click one declared provider download-modal control. No guessed
 * delay, selector, fallback, or terms/consent action. */
async function clickDeclaredDownload(
  selector: string,
  shadowSelector: string | null,
  waitForSelector: string | null,
  timeoutMs: number | null,
  followupSelector: string | null,
): Promise<boolean> {
  const host = document.querySelector(selector);
  let target: Element | null = host;
  if (shadowSelector !== null) {
    if (!(host instanceof HTMLElement) || host.shadowRoot === null) return false;
    target = host.shadowRoot.querySelector(shadowSelector);
  }
  if (!(target instanceof HTMLElement)) return false;
  target.click();

  const appearanceSelector = followupSelector ?? waitForSelector;
  if (appearanceSelector === null) return true;
  const findAppeared = (): Element | null => {
    try {
      return document.querySelector(appearanceSelector);
    } catch {
      return null;
    }
  };

  let appeared = findAppeared();
  if (appeared === null) {
    const boundedMs = Math.max(0, Math.min(timeoutMs ?? 0, 5000));
    appeared = await new Promise<Element | null>((resolve) => {
      let observer: MutationObserver | null = null;
      let timer: number | Timer | undefined;
      const finish = (element: Element | null): void => {
        observer?.disconnect();
        clearTimeout(timer);
        resolve(element);
      };
      observer = new MutationObserver(() => {
        const element = findAppeared();
        if (element !== null) finish(element);
      });
      observer.observe(document.documentElement, { childList: true, subtree: true, attributes: true });
      timer = setTimeout(() => finish(findAppeared()), boundedMs);
    });
  }

  if (followupSelector !== null) {
    if (!(appeared instanceof HTMLElement)) return false;
    appeared.click();
  }
  return true;
}

/** Bare `scheme://host` for a scanned tab's URL, or null when the page is
 * not an ordinary secure page (ADR-0019 Decision 6: source.origin is bare
 * scheme+host only, and the daemon's page_bulk_submit_request rejects
 * anything but https). */
function bareHTTPSOrigin(rawURL: string | undefined): string | null {
  if (typeof rawURL !== "string" || rawURL.length === 0) return null;
  try {
    const parsed = new URL(rawURL);
    return parsed.protocol === "https:" ? `${parsed.protocol}//${parsed.host}` : null;
  } catch {
    return null;
  }
}

/** ADR-0019 operator UX requirement: the selection workspace header names
 * the source page and when it was scanned. Both are strictly local UI
 * decoration, never sent to the daemon — page-scan.ts's PageBulkSnapshot,
 * the shape shared with the detector and the daemon-facing status/submit
 * round trip, deliberately excludes them (Decision 6: source.origin is bare
 * scheme+host only, never a page title) — so they travel as a background-local
 * intersection instead of widening that shared shape. */
export type PageBulkSnapshotView = PageBulkSnapshot & {
  sourceTitle: string;
  scannedAt: string;
  pdfGrabAvailable?: boolean;
};

export class Bridge {
  private hydrated = false;
  private port: NativePort | null = null;
  /** page_acquire acknowledgements carry no correlation id, so requests are
   * serialized in popup-message order and resolved FIFO. */
  private readonly pageAcquireWaiters: Array<(ack: PageAcquireAckPayload) => void> = [];
  /** Signed provider URL -> job for the narrow interval between calling
   * chrome.downloads.download and receiving its ID. Memory-only: never stored
   * or framed. This lets onDeterminingFilename steer the exact adapter-started
   * download even when stale provider tabs make host correlation ambiguous. */
  private readonly pendingDownloadURLs = new Map<string, string>();
  private readonly pendingGrabDownloadURLs = new Map<string, { grabID: string; tabID: number; steeringPath: string }>();
  private readonly pdfGrabCorrelations = new Map<string, PdfGrabCorrelation>();
  private seq = 0;
  private store: StoreShape = emptyStore();
  private ready: Promise<void> = Promise.resolve();
  /** Serializes full-snapshot persistence. Concurrent Chrome events apply their
   * state transforms synchronously in event order, but chrome.storage gives no
   * write-ordering guarantee, so saves are chained: each runs after the prior
   * settles and persists the latest snapshot, so a stale write never wins. */
  private saveChain: Promise<void> = Promise.resolve();
  private listenersBound = false;
  private readonly downloads = new Map<string, DownloadTrack>();
  private readonly grabDownloads = new Map<string, PdfGrabTrack>();
  /** Tabs we are intentionally closing, so onRemoved does not emit a spurious
   * cancelled outcome for a programmatic close. */
  private readonly closingTabs = new Set<number>();
  /** Browser-driven fixture capture shares the two-slot handoff governor. */
  private pageCaptureDriving = false;
  private readonly pageCaptureLoadWaiters = new Map<number, (loaded: boolean) => void>();
  /** Lazily-loaded durable ledger of broker tabs this and prior extension
   * lives created. Keys are stringified tab ids, values open timestamps. */
  /** Serializes every managed-tab ledger load/mutate/save transaction. */
  private tabLedgerChain: Promise<void> = Promise.resolve();
  private tabLedgerCache: Record<string, number> | undefined;
  /** A finished download keeps its broker tab open until the daemon has
   * acknowledged the adoption attempt for that job. */
  private readonly completedDownloadTabs = new Map<string, number>();
  /** Jobs currently owned by the operator's direct PDF delivery. This
   * worker-local marker prevents ack cleanup from closing the user's tab. */
  private readonly deliveryJobs = new Set<string>();
  private lastDeliveryState:
    | { job_id: string; state: "adopted"; message: string; at: number }
    | undefined;
  /** Resolver-provided offer URLs are cached here after storage hydration. */
  private readonly offerURLs = new Map<string, string>();
  /** Institution Shibboleth entityIDs from job offers (login_entity_id), used to
   * build an adapter's federated-login route on a `login` verdict. Worker-local;
   * re-offers repopulate it. */
  private readonly loginEntityIDs = new Map<string, string>();
  /** Provider account ids from job offers (proquest_account_id), appended to the
   * provider URL to unlock institutional access. Worker-local. */
  private readonly proquestAccountIDs = new Map<string, string>();
  /** Jobs whose provider URL was already account-id-appended this drive, so a
   * still-walled page doesn't loop. Cleared on job removal. */
  private readonly accountIdAppended = new Set<string>();
  /** Jobs whose handoff tab was already routed to federated login this drive, so
   * repeated `login` classifies do not re-navigate mid sign-in. Cleared on job
   * removal. */
  private readonly federatedLoginRouted = new Set<string>();
  /** Jobs whose openurl was re-driven once after federated login returned, so a
   * still-walled page doesn't loop. Cleared on job removal. */
  private readonly federatedReDriven = new Set<string>();
  /** Token-guarded SESSION_WAIT_TIMEOUT_MS timers for waiting_for_session
   * parks, same pattern as handoffDriveTimeouts: a stale timer that fires
   * after the job has already resumed is a harmless no-op (checked by
   * both map identity and the job's still being waiting_for_session). */
  private readonly waitingForSessionTimers = new Map<string, object>();
  /** Jobs that already reported a given terminal handoff or provider outcome,
   * so retries of one drive do not spam the daemon. Cleared for a fresh drive
   * and on job removal. */
  private readonly handoffOutcomeSent = new Set<string>();
  /** Challenge/dead-end browser.error reports are once per active drive. */
  private readonly challengeBlockedOutcomeSent = new Set<string>();
  /** Worker-local wakeups complement durable cooldown expiry timestamps. */
  private readonly challengeCooldownTimers = new Map<string, object>();
  /** Jobs whose work window was already raised for a detected IdP failure this
   * worker lifetime, so a bounded re-drive loop cannot yank focus repeatedly.
   * Cleared on job removal. */
  private readonly authFailureSurfaced = new Set<string>();
  /** Chrome can dispatch a document's `complete` and title updates without
   * awaiting either callback; their shared epoch prevents one stale page from
   * consuming multiple recovery attempts. */
  private readonly staleRecoveryEpochs = new Map<string, number>();
  private readonly staleRecoveryAttemptedEpochs = new Map<string, number>();
  private readonly staleRecoveryInFlightEpochs = new Map<string, number>();
  /** Document epoch already given its one late OpenAthens body probe. Retaining
   * the epoch after the timer fires prevents repeated title events from polling. */
  private readonly openAthensErrorRecheckEpochs = new Map<string, number>();
  /** Resolver pages that conclusively show zero electronic holdings are terminal
   * for this offer. Keep this worker-local debounce until the job is removed so
   * reloads and SPA completion events cannot report the same outcome repeatedly. */
  private readonly resolverNoEntitlementSent = new Set<string>();
  /** Route traversal evidence observed for each active handoff. */
  private readonly resolverRoutes = new Set<string>();
  /** Per-job auth evidence used for the next completed browser delivery. */
  private readonly deliverySessionEvidence = new Map<string, DeliverySessionEvidence>();
  /** A completed OA landing can release only OA concurrency queues; it is never
   * evidence that an institutional SSO session exists. */
  private openAccessLandingObserved = false;
  // Per-origin auth evidence lives in ONE place: store.authEvidenceByOrigin
  // (state.ts), timestamped and TTL'd. Three worker-local mirrors used to
  // shadow it — a release-grade Set, a landing Set, and a timestamp Map — and
  // every one of them was append-only. hasAuthEvidence() consulted the
  // release-grade Set first and returned true unconditionally, so signing out
  // revoked nothing: papio kept releasing that origin's queued handoffs for
  // the rest of the worker's life, past the TTL the persisted entry was
  // supposed to enforce. A single expiring source can be revoked with one
  // delete, and cannot disagree with itself.
  /** Current keepalive reauthentication pause, used by computeBadge. */
  private keepaliveReauthNeeded = false;
  /** Attached synchronously at worker startup, before bridge.start() binds
   * listeners, so a wake-triggered navigation can never observe it unset. */
  private keepaliveManager: KeepaliveManager | undefined;
  /** Human-auth stalls and their resolver offers remain worker-local so an
   * operator can explicitly reset and re-drive them without persistence. */
  private readonly stalledAuthHandoffs = new Map<string, StalledAuthHandoff>();
  private authUnblockedCount = 0;
  private authUnblockedAt: number | null = null;
  /** Atomically reserves the one visible handoff while tabs.create is in flight. */
  private handoffOpening = false;
  /** FIFO for accepted offers that are waiting only for a governor slot. */
  private readonly handoffDriveQueue: QueuedHandoffDrive[] = [];
  private readonly queuedDriveJobIDs = new Set<string>();
  private readonly handoffDrives = new Map<string, HandoffDrive>();
  private readonly handoffDriveTimeouts = new Map<string, object>();
  private handoffDriveDrainChain: Promise<void> = Promise.resolve();
  /** URLs papio has intentionally opened or navigated for each tracked job.
   * Used only as a best-effort guard against closing a tab the user reused. */
  private readonly managedTabURLs = new Map<string, Set<string>>();
  /** Single reducer state for the papio tab-group's human-attention surface. */
  private handoffGroupDesiredExpanded = false;
  private handoffGroupLastStateChangeAt: number | undefined;
  private handoffGroupUpdateToken: object | undefined;
  private drainingHandoffDriveQueue = false;
  private drainingQueuedHandoffs = false;
  /** Callers that arrive while the single queue drain is opening a tab wait for
   * that drain to settle before inspecting the job's resulting tab. */
  private readonly queuedHandoffDrainWaiters = new Set<() => void>();
  /** Pending fallback-release timers, keyed by queued job. Worker-local only. */
  private readonly queuedHandoffTimers = new Map<string, object>();
  /** Forced job IDs awaiting release; consumed by the single active drain so
   * overlapping fallback timers cannot drop each other's requests. */
  private readonly pendingForcedReleases = new Set<string>();
  /** Ownership tokens never leave this worker. Durable lease metadata omits
   * them, so a restarted worker waits only for the persisted expiry. */
  private readonly providerDrainLeaseOwners = new Map<string, string>();
  /** Lease-expiry wakeups are best-effort; startup and the keepalive alarm
   * re-derive expiry from session state after MV3 discards these timers. */
  private readonly providerDrainLeaseTimers = new Map<string, object>();
  /** A bounded retry budget tracks only ordinary provider render races. */
  private readonly classifyRetries = new Map<string, ClassifyRetry>();
  /** Effective provider access is stable between permission changes, so retries
   * and repeated tab updates do not repeatedly ask Chrome about the same host. */
  private readonly providerAccessByHost = new Map<string, boolean>();
  /** Broker-tab ids whose auth attempt is already counted, so the SSO redirect
   * dance within one drive increments the budget only once. Worker-local. */
  private readonly authCountedTabs = new Set<number>();
  /** Serializes the complete managed-tab reuse/create decision, so concurrent
   * inbox clicks or re-offers cannot both observe an empty candidate set. */
  private managedTabChain: Promise<unknown> = Promise.resolve();
  /** Coalesce concurrent inbox clicks for one job so one request owns the
   * managed-tab focus/open choreography and all callers receive its result. */
  private readonly openHandoffRequests = new Map<
    string,
    Promise<BrokerReply<{ opened: true }>>
  >();
  /** Jobs already reported human_auth_required this worker lifetime, so a capped
   * job refreshes the daemon's human action at most once per spin-up. */
  private readonly authStalledReported = new Set<string>();
  /** Serializes work-window creation so concurrent offers cannot race two
   * dedicated windows into existence. Worker-local only. */
  private workTabChain: Promise<unknown> = Promise.resolve();
  /** The broker-tab chain does not cover keepalive placement, so group adoption
   * needs its own gate to prevent two first folds from both creating a group. */
  private handoffGroupChain: Promise<void> = Promise.resolve();
  /** A persisted id can name only one window; retain the other live groups for
   * this worker so window-local handoffs do not overwrite each other. */
  private readonly handoffGroupIDsByWindow = new Map<number, number>();
  /** Native port messages may await storage, tabs, or downloads. Preserve wire
   * receipt order across those awaits so state transitions never interleave. */
  private inboundChain: Promise<void> = Promise.resolve();
  /** One resolver per correlated native triage request. It is intentionally
   * worker-memory only; daemon state remains the authority after a restart. */
  private readonly pendingNativeRequests = new Map<string, PendingNativeRequest>();
  private portGeneration = 0;
  private helloAckGeneration = -1;
  private helloSentGeneration = -1;
  private readonly helloWaiters = new Set<(acknowledged: boolean) => void>();
  private requestIDSequence = 0;
  /** Best-effort display cache only, refreshed from daemon counts or snapshots. */
  private triagePendingCount: number | undefined;
  /** Durable institutional demand from the most recent negotiated counts poll. */
  private triageActionsRequiresAuth: number | undefined;
  private triageActionsRequiresAuthAt: number | undefined;
  /** Per-origin session_evidence throttle. Keyed by origin (or "" when no
   * origin hint), so one institution's evidence can no longer suppress
   * another's for SESSION_EVIDENCE_THROTTLE_MS. */
  private readonly sessionEvidenceSentAt = new Map<string, number>();

  constructor(private readonly deps: BridgeDeps) {}
  trackedJobCount(): number {
    return this.store.activeJobs.length;
  }

  warmDemand(): boolean {
    const count = this.triageActionsRequiresAuth;
    const receivedAt = this.triageActionsRequiresAuthAt;
    if (count === undefined || receivedAt === undefined || count <= 0) return false;
    const age = this.deps.now() - receivedAt;
    return age >= 0 && age <= TRIAGE_COUNTS_FRESH_MS;
  }

  private queuedAuthJobCount(): number {
    return this.store.activeJobs.filter(
      (job) => job.status === "queued" && job.requires_auth === true,
    ).length;
  }
  queuedAuthJobs(): number {
    return this.queuedAuthJobCount();
  }

  lastAuthReturnedAt(): number | undefined {
    return this.store.lastAuthReturnedAt;
  }

  stalledAuthJobIDs(): string[] {
    return [...this.authStalledReported];
  }

  challengeBlockedJobCount(): number {
    return this.store.activeJobs.filter((job) => job.challenge_blocked === true).length;
  }

  attachKeepalive(manager: KeepaliveManager): void {
    this.keepaliveManager = manager;
    this.keepaliveReauthNeeded = manager.getSnapshot().pausedForReauth;
  }

  setKeepaliveReauthNeeded(paused: boolean): void {
    if (this.keepaliveReauthNeeded === paused) return;
    this.keepaliveReauthNeeded = paused;
    void this.syncConnectionBadge();
  }

  private resolverOriginHint(rawURL: string | undefined): string | undefined {
    if (rawURL === undefined || isAuthenticationURL(rawURL)) return undefined;
    try {
      const parsed = new URL(rawURL);
      const origin = `${parsed.protocol}//${parsed.host}`;
      return isBareHTTPSOrigin(origin) ? origin : undefined;
    } catch {
      return undefined;
    }
  }

  /** The institution origin for one job. The offer URL's own origin answers
   * when the daemon's config knows it; otherwise the first provider host the
   * config-derived resolver origins recognize does. A LibKey-routed offer
   * opens on libkey.io and forwards through the institution's resolver, so
   * the offer origin stops identifying the institution the moment LibKey
   * link mode is configured — the daemon deliberately keeps the resolver
   * host on provider_hosts for exactly this derivation (ADR-0016). Fails
   * closed to undefined: an origin outside the configured set never
   * becomes institutional bookkeeping. */
  private jobInstitutionOrigin(job: ActiveJob): string | undefined {
    const known = this.knownResolverOrigins();
    const hinted = this.resolverOriginHint(this.offerURLs.get(job.job_id));
    if (hinted !== undefined && known.includes(hinted)) return hinted;
    for (const host of job.provider_hosts ?? []) {
      const match = known.find((origin) => {
        try {
          return new URL(origin).hostname === host;
        } catch {
          return false;
        }
      });
      if (match !== undefined) return match;
    }
    return undefined;
  }

  knownResolverOrigins(): readonly string[] {
    const origins = new Set<string>();
    // Institutions are the daemon's CONFIG-derived resolver origins from the
    // hello ack — never offer traffic. OA/direct offers carry provider URLs,
    // and folding those in turned every provider that ever offered a job into
    // a phantom "institution" row in the popup session card.
    const candidates = [...(this.store.resolverOrigins ?? [])];
    for (const candidate of candidates) {
      const origin = this.resolverOriginHint(candidate);
      if (origin !== undefined) origins.add(origin);
    }
    return [...origins];
  }

  sessionOriginStates(): KeepaliveOriginSnapshot[] {
    const manager = this.keepaliveManager;
    if (manager !== undefined) return manager.getOriginSnapshots();
    const state = this.sessionState();
    return this.knownResolverOrigins().map((origin) => {
      const isDefault = origin === state.resolverOrigin;
      return {
        origin,
        authenticated: isDefault && state.authenticated,
        verdict: isDefault ? (state.verdict ?? "unknown") : "unknown",
        probeSource: isDefault ? (state.probeSource ?? "none") : "none",
        ...(isDefault && state.lastProbeOutcome !== undefined ? { lastProbeOutcome: state.lastProbeOutcome } : {}),
        lastVerdictAt: isDefault ? (state.lastVerdictAt ?? null) : null,
        checking: isDefault && state.checking === true,
        likelyAuthenticated: isDefault && state.likelyAuthenticated === true,
        pausedForReauth: isDefault && state.pausedForReauth,
        lastProbeAt: isDefault ? state.lastProbeAt : null,
        dirtySince: null,
      };
    });
  }

  /** Any configured origin whose persisted evidence is still inside the TTL.
   * Only reached before the keepalive manager has a snapshot of its own. */
  private anyOriginAuthenticated(): boolean {
    return this.knownResolverOrigins().some((origin) => this.hasAuthEvidence(origin));
  }

  sessionState(): BridgeSessionState {
    const fallback: KeepaliveSnapshot = {
      enabled: true,
      intervalMinutes: 4,
      authenticated: this.anyOriginAuthenticated(),
      verdict: this.anyOriginAuthenticated() ? "in" : "unknown",
      probeSource: this.anyOriginAuthenticated() ? "keepalive_tab" : "none",
      lastVerdictAt: null,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: this.keepaliveReauthNeeded,
      lastProbeAt: null,
      resolverOrigin: null,
      lastAuthReturnedAt: this.store.lastAuthReturnedAt ?? null,
      queuedAuthJobs: this.queuedAuthJobCount(),
      stalledAuthJobs: this.stalledAuthJobIDs(),
    };
    const snapshot = this.keepaliveManager?.getSnapshot() ?? fallback;
    return {
      ...snapshot,
      pausedForReauth: this.keepaliveReauthNeeded || snapshot.pausedForReauth,
      authenticated: snapshot.authenticated,
      lastAuthReturnedAt: this.store.lastAuthReturnedAt ?? snapshot.lastAuthReturnedAt,
      queuedAuthJobs: this.queuedAuthJobCount(),
      stalledAuthJobs: this.stalledAuthJobIDs(),
      releasedAuthJobs: this.authUnblockedCount,
      releasedAuthJobsAt: this.authUnblockedAt,
    };
  }

  /** Pure read for the popup's steady-state poll: never probes, never injects. */
  async sessionStateSnapshot(): Promise<BridgeSessionState> {
    await this.ready;
    return this.sessionState();
  }

  /** Refresh a stale/unknown keepalive verdict before replying to the popup.
   * The manager bounds the wait and leaves `checking` true when browser APIs
   * exceed the foreground budget. */
  async sessionStateWithProbe(): Promise<BridgeSessionState> {
    await this.ready;
    await this.keepaliveManager?.probeForeground();
    return this.sessionState();
  }

  async requestSessionSignIn(origin?: string): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    if (origin !== undefined) {
      if (!isBareHTTPSOrigin(origin)) {
        return this.failure("resolver_unavailable", "No resolver configured yet — open a paper first");
      }
      if (this.hasCurrentHello() && !this.knownResolverOrigins().includes(origin)) {
        return this.failure("resolver_unavailable", "This institution is not currently configured");
      }
      // Hand the tab to the keepalive manager exactly as the no-origin branch
      // below does. It owns the tab for the duration of the sign-in: its
      // reload cycle pauses, so a scheduled reload cannot destroy an
      // in-flight SAML exchange, and the tab is never entered in the managed
      // tab ledger, so startup orphan reconciliation cannot close it while
      // the operator is still signing in.
      const originManager = this.keepaliveManager;
      if (originManager !== undefined) {
        try {
          if (await originManager.openReauth(origin)) return { ok: true, opened: true };
        } catch {
          // Fall through to the unmanaged tab below.
        }
      }
      const tabID = await this.openManagedTab({
        url: origin,
        purpose: "session-signin",
      });
      return tabID === undefined
        ? this.failure("session_open_failed", "Could not open the institution sign-in")
        : { ok: true, opened: true };
    }
    const manager = this.keepaliveManager;
    let resolverOrigin = manager?.getSnapshot().resolverOrigin ?? this.latestResolverOrigin();
    if (manager !== undefined) {
      try {
        if (await manager.openReauth()) return { ok: true, opened: true };
      } catch {
        // Fall through to the explicit foreground-origin fallback below.
      }
      resolverOrigin = manager.getSnapshot().resolverOrigin ?? resolverOrigin;
    }
    if (resolverOrigin === undefined) {
      return this.failure("resolver_unavailable", "No resolver configured yet — open a paper first");
    }
    const tabID = await this.openManagedTab({
      url: resolverOrigin,
      purpose: "session-signin",
    });
    return tabID === undefined
      ? this.failure("session_open_failed", "Could not open the institution sign-in")
      : { ok: true, opened: true };
  }

  async retryAuthStalled(jobID: string): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    const current = findByJob(this.store, jobID);
    const saved =
      this.stalledAuthHandoffs.get(jobID) ??
      (current !== undefined && this.offerURLs.get(jobID) !== undefined
        ? {
            url: this.offerURLs.get(jobID)!,
            providerHosts: [...current.provider_hosts],
            ...(current.expected !== undefined ? { expected: current.expected } : {}),
            ...(current.requires_auth !== undefined ? { requiresAuth: current.requires_auth } : {}),
          }
        : undefined);
    if (saved === undefined || !this.authStalledReported.has(jobID)) {
      return this.failure("handoff_unavailable", "This authentication stall is no longer available");
    }
    await this.update((s) => this.clearAuthAttempts(s, jobID));
    if (!this.handoffDrives.has(jobID) && this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
      const now = this.deps.now();
      await this.upsertJobWithOffer(
        {
          job_id: jobID,
          tab_id: current?.tab_id ?? -1,
          offered_at: now,
          expires_at: now,
          status: "accepted",
          provider_hosts: [...saved.providerHosts],
          ...(saved.requiresAuth !== undefined ? { requires_auth: saved.requiresAuth } : {}),
        },
        saved.url,
      );
      this.enqueueHandoffDrive({ jobID, purpose: "redrive", focusExisting: false });
      this.authStalledReported.delete(jobID);
      this.stalledAuthHandoffs.delete(jobID);
      this.send("job_accept", {}, jobID);
      await this.drainHandoffDriveQueue();
      return { ok: true, opened: true };
    }
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: saved.url,
        jobId: jobID,
        purpose: "redrive",
      });
    } catch {
      tabID = undefined;
    }
    if (tabID === undefined) {
      return this.failure("handoff_open_failed", "Could not reopen the institutional handoff");
    }
    this.beginProviderDrive(jobID);
    const openedAt = this.deps.now();
    await this.upsertJobWithOffer(
      {
        job_id: jobID,
        tab_id: tabID,
        offered_at: openedAt,
        expires_at: openedAt,
        status: "accepted",
        provider_hosts: [...saved.providerHosts],
        ...(saved.requiresAuth !== undefined ? { requires_auth: saved.requiresAuth } : {}),
      },
      saved.url,
    );
    this.registerHandoffDrive(jobID, tabID);
    this.stalledAuthHandoffs.delete(jobID);
    this.send("job_accept", {}, jobID);
    return { ok: true, opened: true };
  }



  /** A cold preflight has no tab yet; excluding it would hide the only sign-in
   * signal when keepalive is disabled. */
  private signInBlockerCount(): number {
    return this.store.activeJobs.filter(
      (job) => job.status === "auth_pending" || (job.status === "queued" && job.requires_auth === true),
    ).length;
  }

  private currentBlockedProviderHosts(): string[] {
    return [...new Set(this.store.blockedProviderHosts ?? [])];
  }

  /** A new broker tab is a new provider attempt, so terminal classification
   * observations from its predecessor must not suppress this drive. */
  private beginProviderDrive(jobID: string): void {
    this.classifyRetries.delete(jobID);
    this.handoffOutcomeSent.delete(`${jobID}:ui_changed`);
    this.challengeBlockedOutcomeSent.delete(`${jobID}:challenge_blocked`);
  }

  /** Chrome answers this origin query from effective access: an all-sites grant
   * is sufficient to read a provider page even when no host-specific grant exists. */
  private async hasEffectiveProviderAccess(host: string): Promise<boolean | undefined> {
    const cached = this.providerAccessByHost.get(host);
    if (cached !== undefined) return cached;
    try {
      const allowed = await this.deps.permissions.contains({ origins: [`https://${host}/*`] });
      this.providerAccessByHost.set(host, allowed);
      return allowed;
    } catch (error) {
      // A failed permission query is not proof of a missing grant, so keep the
      // handoff assisted instead of claiming a diagnosis we cannot establish.
      console.error("papio: provider access check failed; staying assisted", error);
      return undefined;
    }
  }

  /** Remember the standing host-level blocker and the exact governed job so
   * repeated pages do not duplicate attention and a later grant can resume. */
  private async reportBlockedProviderHost(jobID: string, host: string): Promise<void> {
    if (!this.currentBlockedProviderHosts().includes(host)) {
      await this.update((store) => ({
        ...store,
        blockedProviderHosts: [...new Set([...(store.blockedProviderHosts ?? []), host])],
      }));
      await this.syncConnectionBadge();
    }

    const job = findByJob(this.store, jobID);
    if (job !== undefined && job.blocked_provider_host !== host) {
      // Keep the governed tab and job live. The popup's user-gesture-bound
      // permission grant can then resume this exact page instead of leaving a
      // terminal manual-download action behind.
      await this.update((store) => patchJob(store, jobID, { blocked_provider_host: host }));
    }
  }

  private async clearBlockedProviderHost(host: string): Promise<boolean> {
    const hasMarker = this.store.activeJobs.some((job) => job.blocked_provider_host === host);
    if (!hasMarker && !this.currentBlockedProviderHosts().includes(host)) return false;
    await this.update((store) => ({
      ...store,
      activeJobs: store.activeJobs.map((job) => {
        if (job.blocked_provider_host !== host) return job;
        const { blocked_provider_host: _blockedProviderHost, ...unblocked } = job;
        return unblocked;
      }),
      blockedProviderHosts: (store.blockedProviderHosts ?? []).filter((blockedHost) => blockedHost !== host),
    }));
    return true;
  }

  /** Permission changes invalidate the cache before repainting the durable
   * host-level signal, so an Options-page grant clears it without a page reload. */
  async onPermissionsChanged(): Promise<void> {
    this.providerAccessByHost.clear();
    const retryJobs = new Set<string>();
    for (const host of this.currentBlockedProviderHosts()) {
      if ((await this.hasEffectiveProviderAccess(host)) !== true) continue;
      for (const job of this.store.activeJobs) {
        if (job.blocked_provider_host === host) retryJobs.add(job.job_id);
      }
      await this.clearBlockedProviderHost(host);
    }
    for (const jobID of retryJobs) {
      try {
        await this.reclassifyCurrentProviderPage(jobID, true);
      } catch (error) {
        // A tab can disappear between the browser permission callback and the
        // retry. Normal tab-close recovery remains authoritative.
        console.error("papio: provider access granted after its tab closed", error);
      }
    }
    await this.syncConnectionBadge();
  }

  /** Resolve where handoffs open. `tab-group` degrades to `work-window` when
   * the platform lacks tab groups, and any window-backed mode degrades to
   * `in-window` without a windows API. */
  private async handoffSurface(): Promise<HandoffSurface> {
    let surface = await this.deps.settings.getHandoffSurface();
    if (surface === "tab-group" && this.deps.tabs.group === undefined) surface = "work-window";
    if (surface === "work-window" && this.deps.windows === undefined) surface = "in-window";
    return surface;
  }

  /** Open a broker tab. Work-window tabs stay unfocused and minimized unless a
   * directly matched adapter requires its SPA to render visibly; otherwise the
   * legacy rule applies and `surfaceFallback` decides whether the tab takes
   * focus. Never throws — returns undefined on failure, matching callers. */
  private async openBrokerTab(url: string, surfaceFallback: boolean): Promise<number | undefined> {
    const surface = await this.handoffSurface();
    if (surface === "work-window") {
      let targetAdapter: AdapterSpec | undefined;
      try {
        const host = new URL(url).hostname;
        targetAdapter = this.deps.adapterSpecs.find((candidate) => hostMatches(host, candidate.hosts));
      } catch {
        // The browser will reject malformed handoff URLs through the normal path.
      }
      const opened = this.workTabChain.then(() =>
        this.openWorkWindowTab(url, needsVisibleWindow(targetAdapter)),
      );
      this.workTabChain = opened.catch(() => undefined);
      try {
        return await opened;
      } catch (e) {
        console.error("papio: work-window tab creation failed", e);
        return undefined;
      }
    }
    if (surface === "tab-group") {
      // Serialize like the work window so concurrent offers share one group.
      const opened = this.workTabChain.then(() => this.openTabGroupTab(url));
      this.workTabChain = opened.catch(() => undefined);
      try {
        return await opened;
      } catch (e) {
        console.error("papio: tab-group creation failed", e);
        return undefined;
      }
    }
    try {
      return (await this.deps.tabs.create({ url, active: surfaceFallback })).id;
    } catch (e) {
      console.error("papio: tab creation failed", e);
      return undefined;
    }
  }
  private rememberManagedTabURL(jobID: string | undefined, url: string | undefined): void {
    if (jobID === undefined || url === undefined || url.length === 0) return;
    const known = this.managedTabURLs.get(jobID) ?? new Set<string>();
    known.add(normalizeManagedTabURL(url));
    this.managedTabURLs.set(jobID, known);
  }

  /** A URL is closable only when it still resembles a page papio opened for
   * this job. A different origin is treated as user navigation and left alone. */
  private isManagedTabURL(job: ActiveJob, rawURL: string | undefined): boolean {
    if (rawURL === undefined || rawURL.length === 0) return false;
    const normalized = normalizeManagedTabURL(rawURL);
    if (this.managedTabURLs.get(job.job_id)?.has(normalized)) return true;
    const offered = this.offerURLs.get(job.job_id);
    if (offered !== undefined && normalizeManagedTabURL(offered) === normalized) return true;
    try {
      const current = new URL(rawURL);
      if (current.hostname === CHROME_PDF_VIEWER_HOST || isAuthenticationURL(rawURL)) return true;
      if (offered !== undefined && new URL(offered).origin === current.origin) return true;
      return hostMatches(current.hostname, job.provider_hosts) ||
        this.deps.adapterSpecs.some((spec) => hostMatches(current.hostname, spec.hosts));
    } catch {
      return false;
    }
  }

  private async closeManagedHandoffTab(job: ActiveJob, tabID: number): Promise<boolean> {
    if (tabID < 0) return false;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(tabID);
    } catch {
      return false;
    }
    if (tab.id !== tabID || !this.isManagedTabURL(job, tab.url)) return false;
    const surface = await this.handoffSurface();
    if (surface === "work-window" && (this.store.workWindowID === undefined || tab.windowId !== this.store.workWindowID)) {
      return false;
    }
    if (surface === "tab-group") {
      const groupID = tab.groupId;
      if (groupID === undefined || groupID < 0 || this.deps.tabGroups === undefined) return false;
      if ((await this.knownHandoffGroup(groupID, tab.windowId)) === undefined) return false;
    }
    this.closingTabs.add(tabID);
    try {
      await this.deps.tabs.remove(tabID);
      return true;
    } catch {
      this.closingTabs.delete(tabID);
      return false;
    }
  }

  /** Route every resolver/provider open through the selected handoff surface,
   * reusing a live tracked job tab; jobless resolver opens use URL equality
   * modulo fragment. Distinct jobs may legitimately share a resolver URL.
   * The whole lookup/create sequence is serialized because Chrome can deliver
   * two inbox clicks before the first create resolves. */
  private async openManagedTab(options: OpenManagedTabOptions): Promise<number | undefined> {
    const queued = this.managedTabChain.then(() => this.openManagedTabUnlocked(options));
    this.managedTabChain = queued.then(
      () => undefined,
      () => undefined,
    );
    return queued;
  }

  private async openManagedTabUnlocked(options: OpenManagedTabOptions): Promise<number | undefined> {
    const job = options.jobId === undefined ? undefined : findByJob(this.store, options.jobId);
    const trackedTabID = job !== undefined && job.tab_id >= 0 ? job.tab_id : undefined;
    const candidates: TabInfo[] = [];
    const seen = new Set<number>();
    const addCandidate = (candidate: TabInfo): void => {
      if (candidate.id === undefined || seen.has(candidate.id)) return;
      seen.add(candidate.id);
      candidates.push(candidate);
    };
    if (trackedTabID !== undefined) {
      try {
        addCandidate(await this.deps.tabs.get(trackedTabID));
      } catch {
        // A stale persisted id is not proof that a matching tab is absent.
      }
    }
    if (this.deps.tabs.query !== undefined && options.jobId === undefined) {
      try {
        for (const candidate of await this.deps.tabs.query({})) addCandidate(candidate);
      } catch {
        // URL dedupe is best-effort when a browser rejects an all-tabs query.
      }
    }
    const reusable =
      trackedTabID !== undefined || options.jobId === undefined
        ? findManagedTab(candidates, options.url, trackedTabID)
        : undefined;
    if (reusable?.id !== undefined) {
      const shouldNavigate =
        (options.purpose === "redrive" || options.purpose === "reoffer") &&
        (reusable.url === undefined ||
          normalizeManagedTabURL(reusable.url) !== normalizeManagedTabURL(options.url));
      try {
        if (shouldNavigate && this.deps.tabs.update !== undefined) {
          await this.deps.tabs.update(reusable.id, { url: options.url });
        }
        if (options.focusExisting !== false) {
          await this.focusManagedTab(reusable.id, {
            ...reusable,
            ...(shouldNavigate ? { url: options.url } : {}),
          });
        }
      } catch {
        // A tab can disappear between lookup and focus; callers still retain
        // the live id and the browser removal path will recover the job.
      }
      this.rememberManagedTabURL(options.jobId, options.url);
      await this.recordManagedTab(options.jobId, reusable.id);
      return reusable.id;
    }
    const tabID = await this.openBrokerTab(options.url, options.surfaceFallback ?? true);
    if (tabID === undefined) return undefined;
    this.rememberManagedTabURL(options.jobId, options.url);
    await this.recordManagedTab(options.jobId, tabID);
    await this.ledgerManagedTab(tabID);
    if (options.purpose === "session-signin") {
      try {
        await this.focusManagedTab(tabID);
      } catch {
        // Sign-in remains available in the managed surface if focus is denied.
      }
    }
    return tabID;
  }

  /** Keep the durable active-job tab id aligned whenever a managed open finds
   * or creates a tab for an already-known job. Fresh offers upsert their full
   * job record immediately after this helper returns. */
  private async recordManagedTab(jobID: string | undefined, tabID: number): Promise<void> {
    if (jobID === undefined) return;
    const job = findByJob(this.store, jobID);
    if (job === undefined || job.tab_id === tabID) return;
    await this.update((s) => patchJob(s, jobID, { tab_id: tabID }));
  }

  private async saveTabLedger(ledger: Record<string, number>): Promise<void> {
    const snapshot = { ...ledger };
    try {
      await this.deps.tabLedger?.save(snapshot);
    } catch {
      // Best-effort durability: a failed write only degrades future cleanup.
    }
  }

  /** Load, mutate, and persist the managed-tab ledger as one serialized
   * transaction. Every cache and storage value is a fresh snapshot so a later
   * mutation cannot rewrite an earlier save's object in place. */
  private runTabLedgerTransaction<T>(
    transaction: (
      ledger: Record<string, number>,
    ) => Promise<{ value: T; changed: boolean }> | { value: T; changed: boolean },
  ): Promise<T> {
    const operation = this.tabLedgerChain.then(async () => {
      let cached = this.tabLedgerCache;
      if (cached === undefined) {
        try {
          cached = (await this.deps.tabLedger?.load()) ?? {};
        } catch {
          cached = {};
        }
      }
      const ledger = { ...cached };
      const result = await transaction(ledger);
      this.tabLedgerCache = { ...ledger };
      if (result.changed) await this.saveTabLedger(this.tabLedgerCache);
      return result.value;
    });
    this.tabLedgerChain = operation.then(
      () => undefined,
      () => undefined,
    );
    return operation;
  }

  /** Record a broker tab papio CREATED. Reused tabs are deliberately never
   * ledgered: a URL-matched reuse can be the user's own tab, and the ledger
   * exists to authorize closing — papio must never earn that authority over
   * a tab it did not open. */
  private async ledgerManagedTab(tabID: number): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    await this.runTabLedgerTransaction(async (ledger) => {
      const key = String(tabID);
      if (ledger[key] !== undefined) return { value: undefined, changed: false };
      ledger[key] = this.deps.now();
      return { value: undefined, changed: true };
    });
  }

  private async forgetLedgeredTab(tabID: number): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    await this.runTabLedgerTransaction(async (ledger) => {
      const key = String(tabID);
      if (ledger[key] === undefined) return { value: undefined, changed: false };
      delete ledger[key];
      return { value: undefined, changed: true };
    });
  }

  /** Classify ledgered, untracked tabs. Tabs sitting in papio's OWN surfaces
   * (the papio tab group or the dedicated work window) are unambiguously
   * papio's to manage and are reconciled automatically; ledgered strays
   * elsewhere (in-window fallbacks, tabs the user pulled out of the group)
   * only ever close through the operator's popup card. Tracked, active,
   * and pinned (keepalive) tabs are never candidates; dead ledger entries
   * are pruned as a side effect. */
  private async classifyLedgeredTabs(): Promise<{ auto: number[]; ask: number[] }> {
    await this.ready;
    if (this.deps.tabLedger === undefined) return { auto: [], ask: [] };
    const tracked = new Set<number>();
    for (const job of this.store.activeJobs) if (job.tab_id >= 0) tracked.add(job.tab_id);
    for (const id of this.completedDownloadTabs.values()) tracked.add(id);
    for (const id of this.closingTabs) tracked.add(id);
    return this.runTabLedgerTransaction(async (ledger) => {
      const auto = new Set<number>();
      const ask = new Set<number>();
      let changed = false;
      for (const key of Object.keys(ledger)) {
        const tabID = Number(key);
        if (!Number.isInteger(tabID) || tabID < 0) {
          delete ledger[key];
          changed = true;
          continue;
        }
        if (tracked.has(tabID)) continue;
        let tab: TabInfo;
        try {
          tab = await this.deps.tabs.get(tabID);
        } catch {
          delete ledger[key];
          changed = true;
          continue;
        }
        // Never the tab the user is looking at, and never the keepalive
        // resolver tab — Chrome marks it pinned, and it is papio's session.
        if (tab.active === true || tab.pinned === true) continue;
        let ownedSurface = tab.windowId !== undefined && tab.windowId === this.store.workWindowID;
        if (!ownedSurface && tab.groupId !== undefined && tab.groupId >= 0) {
          ownedSurface = (await this.knownHandoffGroup(tab.groupId, tab.windowId)) !== undefined;
        }
        (ownedSurface ? auto : ask).add(tabID);
      }
      return {
        value: {
          auto: [...auto].sort((a, b) => a - b),
          ask: [...ask].sort((a, b) => a - b),
        },
        changed,
      };
    });
  }

  /** Popup card contents: only the strays papio will not touch on its own. */
  async orphanTabStatus(): Promise<{ count: number; tab_ids: number[] }> {
    const { ask } = await this.classifyLedgeredTabs();
    return { count: ask.length, tab_ids: ask };
  }

  /** papio owns its surfaces: silently close ledgered tabs still sitting in
   * the papio group or work window once the daemon has had time to reclaim
   * live work. Runs shortly after startup — no operator step required. */
  async reconcileOwnedTabs(): Promise<{ closed: number }> {
    const { auto } = await this.classifyLedgeredTabs();
    return { closed: await this.closeLedgeredTabs(auto) };
  }

  private async closeLedgeredTabs(tabIDs: readonly number[]): Promise<number> {
    let closed = 0;
    for (const tabID of tabIDs) {
      this.closingTabs.add(tabID);
      try {
        await this.deps.tabs.remove(tabID);
        closed++;
      } catch {
        this.closingTabs.delete(tabID);
      }
      await this.forgetLedgeredTab(tabID);
    }
    return closed;
  }

  /** Operator-initiated: close every stray the popup card offered. */
  async cleanupOrphanTabs(): Promise<{ closed: number }> {
    const { tab_ids } = await this.orphanTabStatus();
    return { closed: await this.closeLedgeredTabs(tab_ids) };
  }
  private handoffNeedsHumanNow(): boolean {
    return this.store.activeJobs.some(
      (job) => job.status === "auth_pending" || job.challenge_blocked === true,
    );
  }

  /** Reduce all human-attention signals to one papio-group state. Updates are
   * trailing-edge debounced so an auth redirect storm cannot thrash expand /
   * collapse; the first transition is immediate and later transitions are
   * limited to one browser update per five seconds. */
  private async reduceHandoffGroupState(tabID?: number): Promise<void> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return;
    const desiredExpanded = this.handoffNeedsHumanNow();
    this.handoffGroupDesiredExpanded = desiredExpanded;
    const groupID = tabID === undefined ? this.store.handoffGroupID : await this.handoffGroupIDForTab(tabID);
    if (groupID === undefined) return;
    let current: TabGroupInfo;
    try {
      current = await tabGroups.get(groupID);
    } catch {
      return;
    }
    const desiredCollapsed = !desiredExpanded;
    if (current.collapsed === desiredCollapsed) return;
    const elapsed =
      this.handoffGroupLastStateChangeAt === undefined
        ? HANDOFF_DRIVE_TIMEOUT_MS
        : this.deps.now() - this.handoffGroupLastStateChangeAt;
    if (elapsed < 5_000) {
      if (this.handoffGroupUpdateToken !== undefined) return;
      const token = {};
      this.handoffGroupUpdateToken = token;
      this.deps.setTimeout(async () => {
        if (this.handoffGroupUpdateToken !== token) return;
        this.handoffGroupUpdateToken = undefined;
        await this.reduceHandoffGroupState(tabID);
      }, 5_000 - Math.max(0, elapsed));
      return;
    }
    try {
      await tabGroups.update(groupID, {
        title: desiredExpanded && tabID !== undefined ? this.handoffGroupTitle(tabID) : HANDOFF_GROUP_TITLE,
        collapsed: desiredCollapsed,
      });
      this.handoffGroupLastStateChangeAt = this.deps.now();
    } catch {
      // A vanished group is recreated by the next managed handoff.
    }
  }


  /** Focus a managed tab and, when available, its papio group and containing
   * window. This is intentionally best-effort: focusing is operator UX, not a
   * prerequisite for the daemon handoff. */
  private async focusManagedTab(tabID: number, knownTab?: TabInfo): Promise<void> {
    const tab = knownTab ?? (await this.deps.tabs.get(tabID));
    await this.reduceHandoffGroupState(tabID);
    await this.deps.tabs.update?.(tabID, { active: true });
    if (tab.windowId !== undefined && this.deps.windows !== undefined) {
      try {
        const win = await this.deps.windows.get(tab.windowId);
        await this.deps.windows.update(tab.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // A closed work window is handled by the normal tab-removal path.
      }
    }
  }


  /** Create the handoff tab in the user's current window and fold it into the
   * collapsed "papio" tab group, reusing the group across handoffs. The group
   * lives in the user's window; Chrome removes it automatically once empty. */
  private async openTabGroupTab(url: string): Promise<number | undefined> {
    const tab = await this.deps.tabs.create({ url, active: false });
    if (tab.id === undefined) return undefined;
    await this.foldIntoHandoffGroup(tab.id, tab.windowId);
    return tab.id;
  }

  /** Queue every create-or-adopt decision because keepalive placement can race
   * broker-tab creation outside the work-tab chain. */
  private async inHandoffGroupChain<T>(work: () => Promise<T>): Promise<T> {
    const queued = this.handoffGroupChain.then(work);
    this.handoffGroupChain = queued.then(
      () => undefined,
      () => undefined,
    );
    return queued;
  }


  private async windowIDForTab(tabID: number, knownWindowID?: number): Promise<number | undefined> {
    if (knownWindowID !== undefined) return knownWindowID;
    try {
      return (await this.deps.tabs.get(tabID)).windowId;
    } catch {
      return undefined;
    }
  }

  private async knownHandoffGroup(
    groupID: number,
    windowID: number | undefined,
  ): Promise<TabGroupInfo | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      const found = await tabGroups.get(groupID);
      return isHandoffGroupTitle(found.title) && (windowID === undefined || found.windowId === windowID)
        ? found
        : undefined;
    } catch {
      return undefined;
    }
  }

  private async findHandoffGroups(windowID?: number): Promise<TabGroupInfo[] | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      return (await tabGroups.query({})).filter(
        (candidate) =>
          isHandoffGroupTitle(candidate.title) && (windowID === undefined || candidate.windowId === windowID),
      );
    } catch {
      return undefined;
    }
  }

  private preferredHandoffGroup(
    candidates: TabGroupInfo[],
    windowID: number | undefined,
  ): TabGroupInfo | undefined {
    const remembered = windowID === undefined ? undefined : this.handoffGroupIDsByWindow.get(windowID);
    return (
      candidates.find((candidate) => candidate.id === remembered) ??
      candidates.find((candidate) => candidate.id === this.store.handoffGroupID) ??
      candidates.find((candidate) => candidate.collapsed === false) ??
      candidates[0]
    );
  }

  /** Merge legacy duplicates before another tab is added, so adoption repairs
   * old reload races instead of merely avoiding the next one. */
  private async foldDuplicateHandoffGroups(
    primary: TabGroupInfo,
    candidates: TabGroupInfo[],
    windowID: number | undefined,
  ): Promise<void> {
    const tabs = this.deps.tabs;
    if (tabs.group === undefined || tabs.query === undefined) return;
    for (const duplicate of candidates) {
      if (duplicate.id === primary.id) continue;
      try {
        const tabIDs = (await tabs.query({ groupId: duplicate.id }))
          .filter((tab) => tab.id !== undefined && (windowID === undefined || tab.windowId === windowID))
          .map((tab) => tab.id!);
        if (tabIDs.length > 0) await tabs.group({ tabIds: tabIDs, groupId: primary.id });
      } catch {
        // A user can close a tab or group while startup is repairing it; the
        // remaining groups are still safe to reconcile.
      }
    }
  }

  private async rememberHandoffGroup(groupID: number, windowID: number | undefined): Promise<void> {
    if (windowID !== undefined) this.handoffGroupIDsByWindow.set(windowID, groupID);
    if (this.store.handoffGroupID === groupID) return;
    await this.update((s) => ({ ...s, handoffGroupID: groupID }));
  }

  /** Add a tab to the collapsed "papio" group, reusing the group across
   * handoffs (and the keepalive tab) or creating it collapsed on first use.
   * No-op when the platform lacks tab grouping. */
  private async foldIntoHandoffGroup(tabID: number, knownWindowID?: number): Promise<void> {
    await this.inHandoffGroupChain(() => this.foldIntoHandoffGroupUnlocked(tabID, knownWindowID));
  }

  private async foldIntoHandoffGroupUnlocked(tabID: number, knownWindowID?: number): Promise<void> {
    const tabs = this.deps.tabs;
    const tabGroups = this.deps.tabGroups;
    if (tabs.group === undefined) return;
    const windowID = await this.windowIDForTab(tabID, knownWindowID);
    const remembered = windowID === undefined ? undefined : this.handoffGroupIDsByWindow.get(windowID);
    let reuse =
      remembered === undefined
        ? undefined
        : await this.knownHandoffGroup(remembered, windowID);
    if (reuse === undefined && this.store.handoffGroupID !== undefined) {
      reuse = await this.knownHandoffGroup(this.store.handoffGroupID, windowID);
    }
    const found = await this.findHandoffGroups(windowID);
    if (found !== undefined) {
      reuse = this.preferredHandoffGroup(found, windowID) ?? reuse;
    }
    if (reuse !== undefined) {
      if (found !== undefined) await this.foldDuplicateHandoffGroups(reuse, found, windowID);
      await tabs.group({ tabIds: [tabID], groupId: reuse.id });
      await this.rememberHandoffGroup(reuse.id, windowID);
      return;
    }
    const groupID = await tabs.group({ tabIds: [tabID] });
    if (tabGroups !== undefined) {
      try {
        await tabGroups.update(groupID, { title: HANDOFF_GROUP_TITLE, collapsed: true, color: "orange" });
      } catch {
        // A grouped tab remains usable even if the browser declines its display update.
      }
    }
    await this.rememberHandoffGroup(groupID, windowID);
  }

  private async handoffGroupWindowID(group: TabGroupInfo): Promise<number | undefined> {
    if (group.windowId !== undefined) return group.windowId;
    const tabs = this.deps.tabs;
    if (tabs.query === undefined) return undefined;
    try {
      return (await tabs.query({ groupId: group.id })).find((tab) => tab.windowId !== undefined)?.windowId;
    } catch {
      return undefined;
    }
  }

  /** Recover all groups left by prior worker lifetimes before a new fold can
   * multiply them again. */
  private async reconcileHandoffGroups(): Promise<void> {
    await this.inHandoffGroupChain(() => this.reconcileHandoffGroupsUnlocked());
  }

  private async reconcileHandoffGroupsUnlocked(): Promise<void> {
    const candidates = await this.findHandoffGroups();
    if (candidates === undefined || candidates.length === 0) return;
    const byWindow = new Map<number, TabGroupInfo[]>();
    for (const candidate of candidates) {
      const windowID = await this.handoffGroupWindowID(candidate);
      if (windowID === undefined) continue;
      const groups = byWindow.get(windowID);
      if (groups === undefined) {
        byWindow.set(windowID, [candidate]);
      } else {
        groups.push(candidate);
      }
    }
    const selected: { group: TabGroupInfo; windowID: number }[] = [];
    for (const [windowID, groups] of byWindow) {
      const primary = this.preferredHandoffGroup(groups, windowID);
      if (primary === undefined) continue;
      await this.foldDuplicateHandoffGroups(primary, groups, windowID);
      this.handoffGroupIDsByWindow.set(windowID, primary.id);
      selected.push({ group: primary, windowID });
    }
    const persisted =
      selected.find((candidate) => candidate.group.id === this.store.handoffGroupID) ?? selected[0];
    if (persisted !== undefined) {
      await this.rememberHandoffGroup(persisted.group.id, persisted.windowID);
    }
  }

  /** Fold the keepalive resolver tab into the "papio" group when tab-group mode
   * is active, keeping papio's whole footprint in one collapsed group. In
   * work-window mode keepalive already places its tab in the work window. */
  async foldKeepaliveTab(tabID: number): Promise<void> {
    await this.ready;
    if ((await this.handoffSurface()) !== "tab-group") return;
    await this.foldIntoHandoffGroup(tabID);
  }

  /** Create the tab inside the dedicated work window, keeping a directly
   * matched visible-required adapter out of the minimized state. */
  private async openWorkWindowTab(url: string, visible: boolean): Promise<number | undefined> {
    const windows = this.deps.windows;
    if (windows === undefined) return undefined;
    const existing = this.store.workWindowID;
    if (existing !== undefined) {
      try {
        const win = await windows.get(existing);
        if (visible && win.state === "minimized") {
          await windows.update(existing, { focused: false, state: "normal" });
        }
        return (await this.deps.tabs.create({ url, active: false, windowId: existing })).id;
      } catch {
        // Window closed by the user (or the tab create raced its closing):
        // fall through and recreate.
      }
    }
    const created = await windows.create({
      url,
      focused: false,
      state: visible ? "normal" : "minimized",
    });
    // macOS Firefox often ignores `state`/`focused` at creation time
    // (bugzilla 1271047): the "minimized" work window arrives front and
    // center. Re-asserting the state after creation is the reliable form.
    if (!visible && created.id !== undefined && created.state !== "minimized") {
      try {
        await windows.update(created.id, { focused: false, state: "minimized" });
      } catch {
        // Cosmetic only: a visible work window still brokers correctly.
      }
    }
    if (created.id !== undefined) {
      const windowID = created.id;
      await this.update((s) => ({ ...s, workWindowID: windowID }));
    }
    return created.tabs?.find((tab) => tab.id !== undefined)?.id;
  }

  /** Restore only adapters whose SPA cannot hydrate while the work window is hidden. */
  private async restoreWorkWindowForAdapter(spec: AdapterSpec): Promise<void> {
    if (!needsVisibleWindow(spec)) return;
    const windowID = this.store.workWindowID;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return;
    try {
      const win = await windows.get(windowID);
      if (win.state === "minimized") {
        await windows.update(windowID, { focused: false, state: "normal" });
      }
    } catch {
      // The handoff continues assisted if the dedicated window disappeared.
    }
  }

  private handoffGroupTitle(tabID: number): string {
    const jobs = this.store.activeJobs.filter((job) => job.tab_id >= 0);
    if (jobs.length !== 1 || jobs[0]?.tab_id !== tabID) return HANDOFF_GROUP_TITLE;
    const title = jobs[0].expected?.title?.replace(/\s+/g, " ").trim();
    if (!title) return HANDOFF_GROUP_TITLE;
    const shortTitle =
      title.length <= HANDOFF_GROUP_TITLE_MAX_TITLE_LENGTH
        ? title
        : `${title.slice(0, HANDOFF_GROUP_TITLE_MAX_TITLE_LENGTH - 1).trimEnd()}…`;
    return `${HANDOFF_GROUP_TITLE} — ${shortTitle}`;
  }

  /** The persisted singleton can name another window, so Chrome's membership
   * data is the authority when a handoff needs to be surfaced or folded away. */
  private async handoffGroupIDForTab(tabID: number): Promise<number | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      const windowID = tab.windowId;
      if (tab.groupId !== undefined && tab.groupId >= 0) {
        const group = await this.knownHandoffGroup(tab.groupId, windowID);
        if (group !== undefined) {
          if (windowID !== undefined) this.handoffGroupIDsByWindow.set(windowID, group.id);
          return group.id;
        }
      }
      const remembered = windowID === undefined ? undefined : this.handoffGroupIDsByWindow.get(windowID);
      if (remembered !== undefined) {
        const group = await this.knownHandoffGroup(remembered, windowID);
        if (group !== undefined) return group.id;
      }
      if (this.store.handoffGroupID !== undefined) {
        const group = await this.knownHandoffGroup(this.store.handoffGroupID, windowID);
        if (group !== undefined) return group.id;
      }
      const found = await this.findHandoffGroups(windowID);
      return found === undefined ? undefined : this.preferredHandoffGroup(found, windowID)?.id;
    } catch {
      // A disappearing tab must not prevent the native handoff from progressing.
      return undefined;
    }
  }

  /** Bring the handoff tab to the human for authentication. In work-window mode
   * this activates the tab and restores/focuses the window; in tab-group mode it
   * expands the collapsed "papio" group and activates the tab. No-op for legacy
   * in-window tabs (already visible). Best-effort — auth proceeds regardless. */
  private async surfaceWorkTab(tabID: number): Promise<void> {
    const groupID = await this.handoffGroupIDForTab(tabID);
    if (groupID !== undefined && this.deps.tabGroups !== undefined) {
      await this.reduceHandoffGroupState(tabID);
      try {
        await this.deps.tabs.update?.(tabID, { active: true });
      } catch {
        // The tab may already be gone; the badge/notification remain the signal.
      }
      return;
    }
    const windowID = this.store.workWindowID;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return;
    try {
      await this.deps.tabs.update?.(tabID, { active: true });
    } catch {
      // The tab may already be gone; window focus below still helps.
    }
    try {
      const win = await windows.get(windowID);
      await windows.update(windowID, {
        focused: true,
        drawAttention: true,
        ...(win.state === "minimized" ? { state: "normal" as const } : {}),
      });
    } catch {
      // Window gone; the popup badge and notification remain the signal.
    }
  }

  /** A missing group id must not be reused after the physical group disappeared. */
  private async recollapseHandoffGroup(tabID?: number): Promise<boolean> {
    const groupID = tabID === undefined ? this.store.handoffGroupID : await this.handoffGroupIDForTab(tabID);
    if (groupID === undefined || this.deps.tabGroups === undefined) return false;
    try {
      await this.deps.tabGroups.get(groupID);
      await this.reduceHandoffGroupState(tabID);
      return true;
    } catch {
      // Group gone; its stored id must not be reused.
      return false;
    }
  }

  /** Clear parked_with_tab at the point INTENT to drive is expressed — either
   * caller below — not only on registerHandoffDrive's eventual success. Both
   * are reachable whenever the governor's two slots are already full, which
   * is the normal steady state once parkHandoffForManual drains its freed
   * slot straight into the next queued job: resumeHandoffAfterManual and the
   * offer handler's re-offer branches then defer through enqueueHandoffDrive
   * instead of registering directly. Clearing only on success left every
   * deferred path writing a live status back to storage with
   * parked_with_tab still true; a worker restart during that open-ended
   * queue wait (MV3 tears the worker down after ~30s idle) would see a live
   * status plus the stale marker and skip re-registering forever — stranded
   * outside governor supervision, with no timeout and no capacity
   * accounting. Both callers route through this one helper so a future
   * third caller cannot reopen the gap.
   * void, not awaited: `update` mutates the in-memory store and chains its
   * persistence synchronously before this call returns, so the clear is
   * queued for disk before either caller's own next await could yield to a
   * teardown. It cannot race parkHandoffForManual's own awaited set either:
   * JS has no true concurrency, and intent to drive a job again is only ever
   * expressed after that same job has already been parked. */
  private clearParkedMarker(jobID: string): void {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    if (job.parked_with_tab === true || job.waiting_for_session === true) {
      this.waitingForSessionTimers.delete(jobID);
      void this.update((s) =>
        patchJob(s, jobID, { parked_with_tab: false, waiting_for_session: false, waiting_for_session_key: undefined }),
      );
    }
  }

  private enqueueHandoffDrive(request: QueuedHandoffDrive): void {
    if (this.handoffDrives.has(request.jobID) || this.queuedDriveJobIDs.has(request.jobID)) return;
    if (findByJob(this.store, request.jobID) === undefined) return;
    this.queuedDriveJobIDs.add(request.jobID);
    this.handoffDriveQueue.push(request);
    this.clearParkedMarker(request.jobID);
  }

  private releaseHandoffDrive(jobID: string): void {
    this.handoffDrives.delete(jobID);
    this.handoffDriveTimeouts.delete(jobID);
    if (!this.queuedDriveJobIDs.delete(jobID)) return;
    const index = this.handoffDriveQueue.findIndex((request) => request.jobID === jobID);
    if (index >= 0) this.handoffDriveQueue.splice(index, 1);
  }

  private registerHandoffDrive(jobID: string, tabID: number): void {
    if (this.handoffDrives.has(jobID)) return;
    // A caller's own `handoffDrives.size >= HANDOFF_DRIVE_LIMIT` check and this
    // call are separated by awaits (openManagedTab, upsertJobWithOffer/patchJob),
    // so two entry points that are not both on the serialized inbound-frame
    // chain — e.g. a popup RPC racing a native re-offer frame — can each pass
    // their own check and both land here, exceeding the cap. Re-check it here
    // so every call site is safe by construction. The caller has always already
    // upserted/patched the job with this tabID before calling, so queuing (not
    // dropping) it lets the next drain reuse that same live tab once a slot frees.
    if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
      this.enqueueHandoffDrive({ jobID, purpose: "handoff", focusExisting: false });
      return;
    }
    // This job may have been parked by the timeout callback below with its
    // tab deliberately preserved (see parked_with_tab's own doc comment in
    // state.ts); a fresh registration here means it is being driven again —
    // by the operator finishing auth and a re-offer/redrive claiming this
    // same tab, or any other caller. clearParkedMarker above documents why
    // this must happen at intent, not just here at success.
    this.clearParkedMarker(jobID);
    const token = {};
    this.handoffDrives.set(jobID, { tabID, token });
    this.handoffDriveTimeouts.set(jobID, token);
    this.deps.setTimeout(async () => {
      if (this.handoffDriveTimeouts.get(jobID) !== token) return;
      this.handoffDriveTimeouts.delete(jobID);
      const current = findByJob(this.store, jobID);
      if (current !== undefined && current.tab_id === tabID) {
        await this.update((s) =>
          patchJob(s, jobID, {
            status: "auth_pending",
            auth_started_ms: this.deps.now(),
          }),
        );
        this.send("auth_pending", {}, jobID);
        // parkHandoffForManual's own contract ("A challenge/auth stall leaves
        // the exact page available to the operator") is violated if we close
        // the tab here: a slow institutional SSO chain or an in-flight 2FA
        // prompt can still be live on the IdP page at the 3-minute mark, and
        // closing destroys the half-filled form with no warning. Only close
        // when the tab is NOT sitting on a recognized auth page. An unreadable
        // tab (already gone) falls through to the close path below — there is
        // nothing left on it to preserve, and removing an already-gone tab id
        // is a harmless no-op.
        let onAuthPage = false;
        try {
          const tab = await this.deps.tabs.get(tabID);
          onAuthPage = typeof tab.url === "string" && isAuthenticationURL(tab.url);
        } catch {
          // Tab already gone; closeManagedHandoffTab below is a no-op on it.
        }
        if (!onAuthPage) {
          // Nothing worth preserving on this page, so the tab goes and the
          // job drops its reference to it. parkHandoffForManual below records
          // the park for the auth-page case, where tab_id survives.
          await this.closeManagedHandoffTab(current, tabID);
          await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
        }
      }
      await this.parkHandoffForManual(jobID);
    }, HANDOFF_DRIVE_TIMEOUT_MS);
  }

  private async drainHandoffDriveQueueUnlocked(): Promise<void> {
    while (this.handoffDrives.size < HANDOFF_DRIVE_LIMIT && this.handoffDriveQueue.length > 0) {
      const request = this.handoffDriveQueue.shift();
      if (request === undefined) return;
      this.queuedDriveJobIDs.delete(request.jobID);
      const job = findByJob(this.store, request.jobID);
      if (job === undefined) continue;
      let tabID = job.tab_id >= 0 ? job.tab_id : undefined;
      if (tabID !== undefined) {
        try {
          const live = await this.deps.tabs.get(tabID);
          if (live.id !== tabID) tabID = undefined;
        } catch {
          tabID = undefined;
        }
      }
      const url = this.offerURLs.get(request.jobID);
      if (tabID !== undefined && request.purpose === "redrive" && url !== undefined && this.deps.tabs.update !== undefined) {
        try {
          await this.deps.tabs.update(tabID, { url });
        } catch {
          tabID = undefined;
        }
      }
      if (tabID === undefined && url === undefined) {
        this.send("job_reject", {}, request.jobID);
        await this.removeJobWithOffer(request.jobID);
        continue;
      }
      if (tabID === undefined && url !== undefined) {
        try {
          tabID = await this.openManagedTab({
            url,
            jobId: request.jobID,
            purpose: request.purpose,
            ...(request.surfaceFallback !== undefined ? { surfaceFallback: request.surfaceFallback } : {}),
            ...(request.focusExisting !== undefined ? { focusExisting: request.focusExisting } : {}),
          });
        } catch (error) {
          console.error("papio: queued handoff tab creation failed", error);
        }
      }
      if (tabID === undefined) {
        this.send("job_reject", {}, request.jobID);
        await this.removeJobWithOffer(request.jobID);
        continue;
      }
      this.beginProviderDrive(request.jobID);
      await this.update((s) =>
        patchJob(s, request.jobID, {
          tab_id: tabID,
          status: "accepted",
          download_initiated: false,
          unknown_count: 0,
        }),
      );
      this.registerHandoffDrive(request.jobID, tabID);
      if (request.surfaceFallback === true) await this.surfaceWorkTab(tabID);
    }
  }

  private async drainHandoffDriveQueue(): Promise<void> {
    const queued = this.handoffDriveDrainChain.then(async () => {
      this.drainingHandoffDriveQueue = true;
      try {
        await this.drainHandoffDriveQueueUnlocked();
      } finally {
        this.drainingHandoffDriveQueue = false;
      }
    });
    this.handoffDriveDrainChain = queued.catch(() => undefined);
    await queued;
  }

  /** A challenge/auth stall leaves the exact page available to the operator,
   * but it is no longer an automated drive and therefore frees one governor
   * slot.
   *
   * That combination — a live tab the job still references, with no entry in
   * handoffDrives — is indistinguishable on its own from a job that IS mid
   * drive, because the slot lives only in worker memory while tab_id is
   * persisted. A service-worker restart (MV3 tears the worker down after
   * ~30s idle) would otherwise see the surviving tab and re-register a fresh
   * drive, silently re-consuming the slot this park just released and
   * re-arming its timeout. Across a slow institutional SSO that repeats on
   * every restart, halving effective governor capacity for everyone else.
   * Recording the park here — rather than at each of the three callers —
   * keeps the marker and the slot release inseparable. registerHandoffDrive
   * clears it whenever the job is genuinely driven again. */
  async parkHandoffForManual(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    this.releaseHandoffDrive(jobID);
    if (job !== undefined && job.tab_id >= 0) {
      await this.update((s) => patchJob(s, jobID, { parked_with_tab: true }));
      await this.reduceHandoffGroupState(job.tab_id);
    }
    await this.drainHandoffDriveQueue();
    await this.releaseQueuedHandoffs();
  }

  /** Cross-job park: this handoff's classify verdict is "login" and its
   * federated-login claim key (the IdP/DS origin plus entityID
   * maybeRouteFederatedLogin resolved) already has a live sibling tab
   * driving that same sign-in (federatedLoginOwners). A second tab at the
   * same login page teaches the human nothing and doubles the sign-in
   * work, so this tab is deliberately left exactly where it is — the
   * provider's login wall — and the governor slot is released, same
   * bookkeeping as parkHandoffForManual's timeout park (parked_with_tab)
   * plus a distinct waiting_for_session marker (and the claim key it is
   * waiting on) so UI copy and the resume paths can tell the two parks
   * apart. Bounded by armSessionWaitTimeout against a PERSISTED deadline
   * (waiting_deadline): reused as-is if this job already carries one from
   * an earlier park in this same wait — re-parking (resumed, hit another
   * login wall, parked again) never grants a fresh budget — and minted
   * fresh only the very first time this job ever parks. */
  private async parkHandoffWaitingForSession(jobID: string, claimKey: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    this.releaseHandoffDrive(jobID);
    if (job !== undefined && job.tab_id >= 0) {
      const deadline = job.waiting_deadline ?? this.deps.now() + SESSION_WAIT_TIMEOUT_MS;
      await this.update((s) =>
        patchJob(s, jobID, {
          status: "auth_pending",
          auth_started_ms: findByJob(s, jobID)?.auth_started_ms ?? this.deps.now(),
          parked_with_tab: true,
          waiting_for_session: true,
          waiting_for_session_key: claimKey,
          waiting_deadline: deadline,
        }),
      );
      await this.reduceHandoffGroupState(job.tab_id);
      this.armSessionWaitTimeout(jobID, claimKey, deadline);
    }
    await this.drainHandoffDriveQueue();
    await this.releaseQueuedHandoffs();
  }

  /** Demotes a waiting_for_session park to an ordinary parked_with_tab park
   * (the pre-feature presentation) at the given absolute `deadline` — an
   * MV3 restart re-arms this from the SAME persisted deadline
   * (reconcileSessionWaitTimeouts), never a fresh SESSION_WAIT_TIMEOUT_MS
   * window, so a worker sleeping mid-wait cannot itself extend the budget.
   * A no-op if the job already resumed, re-parked under a different claim
   * (a fresh call already replaced this token), or is gone. Genuinely
   * spending the deadline also clears it: THIS wait attempt is over, so a
   * later, unrelated park earns a fresh budget rather than inheriting an
   * already-expired one. */
  private armSessionWaitTimeout(jobID: string, claimKey: string, deadline: number): void {
    const token = {};
    this.waitingForSessionTimers.set(jobID, token);
    const delay = Math.max(0, deadline - this.deps.now());
    this.deps.setTimeout(async () => {
      if (this.waitingForSessionTimers.get(jobID) !== token) return;
      this.waitingForSessionTimers.delete(jobID);
      const job = findByJob(this.store, jobID);
      if (job === undefined || job.waiting_for_session !== true || job.waiting_for_session_key !== claimKey) return;
      await this.update((s) =>
        patchJob(s, jobID, {
          waiting_for_session: false,
          waiting_for_session_key: undefined,
          waiting_deadline: undefined,
        }),
      );
    }, delay);
  }

  /** Startup re-arm for every live waiting_for_session job, mirroring how
   * parked_with_tab's own restart handling walks activeJobs: an MV3 restart
   * drops every worker-local setTimeout, so without this a waiter's bounded
   * wait would silently become unbounded the moment the worker slept. A
   * deadline already past due demotes immediately, synchronously, rather
   * than waiting for a timer that would fire with a negative delay anyway. */
  private async reconcileSessionWaitTimeouts(): Promise<void> {
    const now = this.deps.now();
    for (const job of this.store.activeJobs) {
      if (job.waiting_for_session !== true) continue;
      const claimKey = job.waiting_for_session_key;
      const deadline = job.waiting_deadline;
      if (claimKey === undefined || deadline === undefined) continue;
      if (now >= deadline) {
        await this.update((s) =>
          patchJob(s, job.job_id, {
            waiting_for_session: false,
            waiting_for_session_key: undefined,
            waiting_deadline: undefined,
          }),
        );
        continue;
      }
      this.armSessionWaitTimeout(job.job_id, claimKey, deadline);
    }
  }

  /** Reclaim a slot after the operator completes a challenge on the same tab.
   * No navigation occurs here; the next page update drives normal assessment.
   * Classification must wait when all governor slots remain occupied. */
  async resumeHandoffAfterManual(jobID: string): Promise<boolean> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    if (job === undefined || job.tab_id < 0) return false;
    if (!this.handoffDrives.has(jobID)) {
      if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
        this.enqueueHandoffDrive({ jobID, purpose: "handoff", focusExisting: false });
      } else {
        this.registerHandoffDrive(jobID, job.tab_id);
      }
    }
    await this.drainHandoffDriveQueue();
    return this.handoffDrives.has(jobID);
  }

  /** Keepalive must preserve an institutional session, not follow whichever
   * open-access offer happened to arrive last. */
  latestOpenURL(): string | undefined {
    for (let index = this.store.activeJobs.length - 1; index >= 0; index -= 1) {
      const job = this.store.activeJobs[index];
      if (job === undefined || job.requires_auth !== true) continue;
      const openurl = this.offerURLs.get(job.job_id);
      if (openurl !== undefined) return openurl;
    }
    return undefined;
  }

  /** Return only the configured resolver origin; signed offer paths and query
   * parameters never cross the popup reply or become persisted session state. */
  private latestResolverOrigin(): string | undefined {
    const openurl = this.latestOpenURL();
    if (openurl === undefined) return undefined;
    try {
      const url = new URL(openurl);
      return url.protocol === "https:" ? url.origin : undefined;
    } catch {
      return undefined;
    }
  }

  /** The keepalive manager pins its resolver tab inside the work window when
   * one exists, keeping papio's whole footprint out of the user's tab strip. */
  workWindowIDForKeepalive(): number | undefined {
    return this.store.workWindowID;
  }

  /** Keep the persistent daemon-health state visible without interrupting the
   * user. A badge failure is non-fatal: native bridging must keep recovering.
   * Precedence is disconnected, sign-in, a live provider-access block, resolver
   * setup, then triage: a blocked handoff outranks background work, but a dead
   * daemon or a sign-in the user can complete remains more immediate. */
  async syncConnectionBadge(status = this.store.connectionStatus): Promise<void> {
    try {
      const blockedProviderHosts = this.currentBlockedProviderHosts();
      const signInBlockersBeforePermissions = this.signInBlockerCount();
      let ungrantedResolverOrigins = 0;
      if (status === "connected" && signInBlockersBeforePermissions === 0 && blockedProviderHosts.length === 0) {
        for (const origin of this.store.resolverOrigins ?? []) {
          try {
            if (!(await this.deps.permissions.contains({ origins: [`${origin}/*`] }))) {
              ungrantedResolverOrigins += 1;
            }
          } catch {
            ungrantedResolverOrigins += 1;
          }
        }
      }
      // contains() is asynchronous; never paint a connected result after the
      // port has dropped while permission checks were in flight.
      if (status === "connected" && this.store.connectionStatus !== "connected") return;
      const badge = computeBadge({
        connectionStatus: status,
        reauthNeeded: this.keepaliveReauthNeeded,
        authBlockers: this.signInBlockerCount(),
        challengeBlocked: this.challengeBlockedJobCount(),
        blockedHosts: blockedProviderHosts,
        ungrantedResolvers: ungrantedResolverOrigins,
        triageCount: this.triagePendingCount,
      });
      await Promise.all([
        this.deps.action.setBadgeText({ text: badge.text }),
        this.deps.action.setBadgeBackgroundColor({ color: badge.color }),
        this.deps.action.setTitle?.({ title: badge.tooltip }),
      ]);
    } catch {
      // Browser action APIs are advisory; do not make a healthy bridge fail.
    }
  }


  /** Bind browser listeners (once), open the native connection, send hello, and
   * hydrate persisted job/tab correlation. Safe to call on every SW spin-up.
   * top-level-registration expectation. */
  async start(): Promise<void> {
    this.bindListeners();
    this.ready = this.deps.backend.load().then(async (s) => {
      // A service-worker restart may hydrate a prior connection's hello_ack.
      // Keep durable job correlation, but never revive its capabilities.
      this.store = clearNegotiationState(s);
      this.offerURLs.clear();
      for (const [jobID, url] of Object.entries(s.offerURLs ?? {})) {
        if (typeof url !== "string" || findByJob(s, jobID) === undefined) continue;
        this.offerURLs.set(jobID, url);
      }
      const correlations = this.deps.pdfGrabCorrelations === undefined ? {} : await this.deps.pdfGrabCorrelations.get();
      for (const [grabID, correlation] of Object.entries(correlations)) {
        if (
          typeof correlation.scanID === "string" &&
          typeof correlation.tabID === "number" &&
          typeof correlation.state === "string" &&
          typeof correlation.downloadID === "number" &&
          typeof correlation.steeringPath === "string" &&
          typeof correlation.url === "string"
        ) {
          this.pdfGrabCorrelations.set(grabID, correlation);
        }
      }
      this.hydrated = true;
      await this.update((current) => current);
    });
    this.connect();
    // Wake this worker even when idle so queued daemon offers reach it (the
    // native connection originates here, so the daemon cannot wake a dormant
    // worker itself). Idempotent: re-creating the same alarm just resets it.
    this.deps.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: KEEPALIVE_ALARM_MINUTES });
    await this.ready;
    await this.reconcilePdfGrabCorrelations();
    await this.restoreProviderDrainLeaseTimers();
    await this.restoreChallengeCooldownTimers();
    // Reconcile persisted papio groups before any new fold can race the
    // startup repair and multiply groups in the same browser window.
    await this.reconcileHandoffGroups();
    await this.syncConnectionBadge();
    await this.reconcileTabs();
    await this.reconcileFederatedLoginOwners();
    await this.reconcileSessionWaitTimeouts();
    const governorQueuedAtRestart: string[] = [];
    for (const job of this.store.activeJobs) {
      if (job.status !== "accepted" && job.status !== "auth_pending" && job.status !== "awaiting_download") {
        continue;
      }
      if (job.tab_id < 0) {
        // Governor-queued before this worker was suspended: the daemon
        // accepted it, but the FIFO holding it until a slot freed lives only
        // in memory. Nothing else recovers these — this scan used to skip
        // them, the queued-release pass below only handles status "queued",
        // and a daemon re-offer on the same URL merely re-acks. Left alone
        // they never open, never complete and never time out, which is worst
        // exactly under the flood the governor exists for.
        //
        // Deliveries and direct-file downloads also park at tab_id -1 and must
        // NOT be handed a broker tab. The direct-file test has to mirror the
        // offer path's gate exactly: isDirectFileOffer is shape-only, and an
        // institutional handoff's offer URL is the operator's OpenURL base,
        // whose path papio does not constrain — so a pdf-shaped base on a
        // requires_auth offer is a real handoff that was never eligible for
        // the direct-download shortcut. Excluding it on shape alone would
        // strand exactly the jobs this restore exists to recover.
        if (job.status !== "accepted") continue;
        if (this.store.pendingDelivery?.job_id === job.job_id) continue;
        const offerURL = this.offerURLs.get(job.job_id);
        if (offerURL === undefined) continue;
        if (isDirectFileOffer(offerURL) && job.requires_auth !== true) continue;
        governorQueuedAtRestart.push(job.job_id);
        continue;
      }
      if (job.parked_with_tab === true) {
        // Deliberately parked by the handoff-drive timeout with its tab
        // preserved for the operator (see parked_with_tab's doc comment in
        // state.ts): the governor slot was already released at park time,
        // not merely dropped by this restart. Re-registering here would
        // silently re-consume a slot and re-arm a fresh 3-minute timeout for
        // a job nobody asked to resume driving, halving effective governor
        // capacity for every other queued job across a slow institutional
        // SSO. The tab-update listener still recovers it the moment the
        // operator finishes authenticating in that same tab.
        continue;
      }
      if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) continue;
      this.registerHandoffDrive(job.job_id, job.tab_id);
    }
    for (const jobID of governorQueuedAtRestart) {
      this.enqueueHandoffDrive({ jobID, purpose: "handoff", focusExisting: false });
    }
    if (governorQueuedAtRestart.length > 0) await this.drainHandoffDriveQueue();
    await this.redrivePendingTermsGates();
    for (const job of this.store.activeJobs) {
      if (job.status === "queued") this.scheduleQueuedHandoffRelease(job.job_id);
    }
    await this.releaseQueuedHandoffs();
    await this.releaseQueuedHandoffsForLiveLanding();
    // papio owns its surfaces: after the daemon has had a moment to reclaim
    // live work through fresh offers, silently close ledgered leftovers still
    // sitting in the papio group or work window. Two passes: an early one for
    // the common case and a late one for offers that reclaim tabs slowly.
    for (const delay of [12_000, 90_000]) {
      this.deps.setTimeout(() => {
        void this.reconcileOwnedTabs();
      }, delay);
    }
  }

  /**
   * On spin-up the tracked tab_id can be stale: a tab closed while the MV3
   * worker slept (its onTabRemoved never fired), or session-restore reopened
   * provider tabs with fresh ids. Verify each tracked tab still exists and
   * recover the ones that don't, so a job never strands invisibly on a dead
   * tab (the "jobs stuck at auth_returned" failure).
   */
  private async reconcileTabs(): Promise<void> {
    for (const job of [...this.store.activeJobs]) {
      if (job.tab_id < 0) continue; // already queued / awaiting an open
      let alive = false;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        alive = tab?.id === job.tab_id;
      } catch {
        alive = false;
      }
      if (alive) continue;
      if (this.store.pendingDelivery?.job_id === job.job_id && this.store.pendingDelivery.status !== "failed") {
        await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
        continue;
      }
      if (job.status === "awaiting_download") {
        // Past auth: a download may have completed or be in flight into the
        // job's adoption dir, which the daemon's poll-scan adopts. Park it, as
        // onTabRemoved would have.
        this.completedDownloadTabs.delete(job.job_id);
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      // Pre-download tab vanished: re-queue so the handoff choreography reopens
      // it (one visible at a time, forced release within the fallback window)
      // instead of leaving the job pointed at a dead tab. Without a retained
      // offer URL there is nothing to reopen, so drop it.
      if (this.offerURLs.get(job.job_id) === undefined) {
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      if (this.authAttemptsFor(job.job_id) >= MAX_AUTH_ATTEMPTS) {
        // Already failed to authenticate this job MAX_AUTH_ATTEMPTS times this
        // session: surface the human step and leave it parked instead of
        // re-queueing it into another doomed drive.
        const offerURL = this.offerURLs.get(job.job_id);
        if (offerURL !== undefined) {
          this.rememberStalledAuthHandoff(job.job_id, {
            url: offerURL,
            providerHosts: job.provider_hosts,
            ...(job.expected !== undefined ? { expected: job.expected } : {}),
            ...(job.requires_auth !== undefined ? { requiresAuth: job.requires_auth } : {}),
          });
        }
        await this.reportAuthStalled(job.job_id);
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      this.beginProviderDrive(job.job_id);
      this.waitingForSessionTimers.delete(job.job_id);
      await this.update((s) =>
        patchJob(s, job.job_id, {
          tab_id: -1,
          status: "queued",
          download_initiated: false,
          unknown_count: 0,
          // A dead tab discovered only at restart (MV3 never fired
          // onRemoved while this worker slept) never went through
          // onTabRemoved's own waiting_for_session demotion — clear both
          // park markers here for the same reason: a stale marker on a
          // now-queued, tab-less job would misreport it as still parked.
          waiting_for_session: false,
          waiting_for_session_key: undefined,
          parked_with_tab: false,
        }),
      );
      this.scheduleQueuedHandoffRelease(job.job_id);
    }
  }

  /** Cancel an active job on user request (popup cancel button). */
  async requestCancel(jobID: string): Promise<void> {

    await this.ready;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    this.send("provider_outcome", { outcome: "cancelled" }, jobID);
    if (job.tab_id >= 0) {
      await this.closeManagedHandoffTab(job, job.tab_id);
    } else {
      this.releaseHandoffDrive(jobID);
    }
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    this.authStalledReported.delete(jobID);
    this.stalledAuthHandoffs.delete(jobID);
    await this.removeJobWithOffer(jobID);
  }

  /** True only after this port's hello_ack has advertised page acquisition. */
  pageAcquireAvailable(): boolean {
    return (
      this.store.connectionStatus === "connected" &&
      (this.store.daemonFeatures ?? []).includes("page_acquire")
    );
  }

  pageCaptureAvailable(): boolean {
    return (
      this.store.connectionStatus === "connected" &&
      (this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_FEATURE)
    );
  }

  /** Second, independent enforcement point for the `terms` capture scenario.
   * The popup withholds the option, but it decides that when the panel is
   * populated, and the daemon underneath can be swapped between then and the
   * click (the two-binary skew AGENTS.md documents). Emitting `terms` to a
   * daemon that cannot validate it does not merely fail that capture: the
   * decode error tears down the entire native-messaging session, so the
   * boundary that actually sends the frame refuses it too. */
  termsCaptureAvailable(): boolean {
    return (
      this.store.connectionStatus === "connected" &&
      (this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_TERMS_FEATURE)
    );
  }

  /** Forward an active-page acquisition request and await the daemon ack. */
  async requestPageAcquire(payload: PageAcquirePayload): Promise<PageAcquireAckPayload> {
    await this.ready;
    if (!this.pageAcquireAvailable()) {
      return { error: "Page acquisition is not available from this daemon" };
    }
    if (typeof payload.doi !== "string" || payload.doi.trim() === "") {
      return { error: "page has no DOI" };
    }
    return new Promise<PageAcquireAckPayload>((resolve) => {
      this.pageAcquireWaiters.push(resolve);
      const frame: Record<string, unknown> = {
        url: payload.url,
        ...(payload.doi !== undefined ? { doi: payload.doi } : {}),
        ...(payload.title !== undefined ? { title: payload.title } : {}),
        ...(payload.source !== undefined ? { source: payload.source } : {}),
      };
      if (!this.send("page_acquire", frame)) {
        this.pageAcquireWaiters.pop();
        resolve({ error: "Could not send page acquisition request" });
      }
    });
  }

  private deliveryJobForDOI(doi: string | undefined): ActiveJob | undefined {
    if (doi === undefined || doi.trim() === "") return undefined;
    const normalized = doi.trim().toLowerCase().replace(/^doi:\s*/, "");
    return this.store.activeJobs.find(
      (job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === normalized,
    );
  }

  private async startDeliveryDownload(jobID: string, url: string): Promise<boolean> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return false;
    this.deliveryJobs.add(jobID);
    await this.update((s) => patchJob(s, jobID, { download_initiated: true }));
    this.downloads.set(jobID, { ids: new Set<number>(), ambiguous: false, directOffer: false, delivery: true });
    this.pendingDownloadURLs.set(url, jobID);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: `papio/${jobID}/paper.pdf`,
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(jobID);
      if (track !== undefined) {
        track.ids.add(id);
        if (track.ids.size > 1) track.ambiguous = true;
        this.downloads.set(jobID, track);
      }
      return true;
    } catch {
      this.downloads.delete(jobID);
      this.deliveryJobs.delete(jobID);
      await this.update((s) =>
        updatePendingDelivery(
          patchJob(s, jobID, { download_initiated: false }),
          jobID,
          { status: "failed", error: "Could not start the browser download" },
        ),
      );
      return false;
    } finally {
      this.pendingDownloadURLs.delete(url);
    }
  }

  async startPDFDelivery(payload: DeliveryStartPayload): Promise<DeliveryReply> {
    await this.ready;
    if (!Number.isSafeInteger(payload.tab_id) || payload.tab_id < 0 || payload.url.length === 0) {
      return this.failure("invalid_request", "Invalid PDF delivery request");
    }
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(payload.tab_id);
    } catch {
      return this.failure("tab_unavailable", "The current PDF tab is no longer available");
    }
    const tabURL = typeof tab.url === "string" ? tab.url : payload.url;
    const url = pdfSourceURL(payload.url || tabURL);
    if (!isPDFPage(tabURL) && !isPDFPage(url)) {
      return this.failure("not_pdf", "No PDF detected on this page");
    }
    const doi = payload.doi;
    let job = findByTab(this.store, payload.tab_id) ?? this.deliveryJobForDOI(doi);
    let duplicate = false;
    if (job === undefined) {
      if (doi === undefined || doi.trim() === "") {
        return this.failure("no_doi", "This PDF has no DOI to queue");
      }
      const ack = await this.requestPageAcquire({
        url,
        doi,
        ...(payload.title ? { title: payload.title } : {}),
        source: "popup",
      });
      if (ack.error !== undefined) return this.failure("page_acquire", ack.error);
      if (ack.job_id === undefined) return this.failure("page_acquire", "The daemon did not return a job");
      duplicate = ack.duplicate === true;
      await this.inboundChain;
      job = findByJob(this.store, ack.job_id);
      if (job === undefined && duplicate) {
        return this.failure("duplicate_not_live", "That paper is already queued, but its job is not live in this browser");
      }
      if (job === undefined) {
        const now = this.deps.now();
        const synthetic: ActiveJob = {
          job_id: ack.job_id,
          tab_id: payload.tab_id,
          offered_at: now,
          expires_at: now + 24 * 60 * 60_000,
          status: "accepted",
          provider_hosts: [],
          ...(payload.title || doi ? { expected: { ...(payload.title ? { title: payload.title } : {}), ...(doi ? { doi } : {}) } } : {}),
        };
        await this.update((s) => upsertJob(s, synthetic));
        job = synthetic;
      }
    }
    const pending = this.store.pendingDelivery;
    if (pending !== undefined && pending.status !== "failed" && pending.job_id !== job.job_id) {
      return this.failure("delivery_busy", "Another PDF is already being sent to papio");
    }
    if (pending?.job_id === job.job_id && pending.status !== "failed") {
      return { ok: true, state: pending.status ?? "sending", job_id: job.job_id, ...(duplicate ? { duplicate: true } : {}) };
    }
    // Freeze the requesting page's host alongside the URL. The tab stays
    // interactive for the whole download, so this is the only moment the
    // page that actually produced these bytes is known for certain.
    const deliveryPageHostAtStart = sanitizePageHost(tabURL);
    // Freeze session evidence too, for the same reason:
    // store.authEvidenceByOrigin is live per-origin
    // state, not scoped to this tab or download, so an institutional probe
    // or sign-in landing anywhere in the browser during the multi-second
    // download must not retroactively credit this delivery.
    // deliveryEvidenceFor reads this frozen value back at completion instead
    // of re-reading live state.
    const sessionEvidenceAtStart = this.currentSessionEvidence(job);
    await this.update((s) =>
      startPendingDelivery(s, {
        job_id: job.job_id,
        url,
        initiated_at: this.deps.now(),
        status: "sending",
        ...(deliveryPageHostAtStart !== undefined ? { page_host: deliveryPageHostAtStart } : {}),
        session_evidence: sessionEvidenceAtStart,
      }),
    );
    this.lastDeliveryState = undefined;
    const started = await this.startDeliveryDownload(job.job_id, url);
    if (!started) {
      return this.failure("download_start", "Could not start the browser download");
    }
    return { ok: true, state: "sending", job_id: job.job_id, ...(duplicate ? { duplicate: true } : {}) };
  }

  deliveryState(): DeliveryReply {
    const pending = this.store.pendingDelivery;
    if (pending !== undefined) {
      return {
        ok: true,
        state: pending.status ?? "sending",
        job_id: pending.job_id,
        ...(pending.error ? { message: pending.error } : {}),
      };
    }
    if (this.lastDeliveryState !== undefined && this.deps.now() - this.lastDeliveryState.at < 10 * 60_000) {
      return {
        ok: true,
        state: this.lastDeliveryState.state,
        job_id: this.lastDeliveryState.job_id,
        message: this.lastDeliveryState.message,
      };
    }
    return { ok: true, state: "idle" };
  }

  private failPageAcquireWaiters(error: string): void {
    while (this.pageAcquireWaiters.length > 0) {
      this.pageAcquireWaiters.shift()?.({ error });
    }
  }

  /** Focus an existing inbox tab before creating one. This is browser-local UI
   * state only; no tab id is retained because a worker can disappear at will. */
  async openInbox(inboxURL: string): Promise<void> {
    const existing = (await this.deps.tabs.query?.({ url: inboxURL })) ?? [];
    const tab = existing.find((candidate) => candidate.id !== undefined);
    if (tab?.id !== undefined) {
      await this.deps.tabs.update?.(tab.id, { active: true });
      if (tab.windowId !== undefined && this.deps.windows !== undefined) {
        await this.deps.windows.update(tab.windowId, { focused: true });
      }
      return;
    }
    await this.deps.tabs.create({ url: inboxURL, active: true });
  }
  /** Surface the browser-owned handoff already offered for an inbox row. This
   * boundary accepts only a job id: provider/resolver URLs remain local to the
   * extension and are never returned to the caller. */
  async openHandoff(jobID: string): Promise<BrokerReply<{ opened: true }>> {
    const pending = this.openHandoffRequests.get(jobID);
    if (pending !== undefined) return pending;
    const request = this.openHandoffUnlocked(jobID);
    this.openHandoffRequests.set(jobID, request);
    try {
      return await request;
    } finally {
      if (this.openHandoffRequests.get(jobID) === request) {
        this.openHandoffRequests.delete(jobID);
      }
    }
  }

  private async openHandoffUnlocked(jobID: string): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    let job = findByJob(this.store, jobID);
    if (job === undefined || !this.offerURLs.has(jobID)) {
      // A just-acquired inbox item can race the native job_offer. Counts is a
      // safe read that prompts the daemon to flush its already-queued frames;
      // perform it at most once, then wait for the inbound FIFO before retrying.
      if (this.hasCurrentHello() && (this.store.daemonFeatures ?? []).includes(TRIAGE_SNAPSHOT_FEATURE)) {
        try {
          await this.requestTriageCounts();
        } catch {
          // A refresh failure is indistinguishable from no offer at this local
          // boundary; the bounded retry below returns the actionable result.
        }
      }
      await this.inboundChain;
      job = findByJob(this.store, jobID);
    }
    if (job === undefined || !this.offerURLs.has(jobID)) {
      return this.failure("handoff_unavailable", "The requested handoff is not available");
    }

    if (job.status === "queued") {
      // releaseQueuedHandoffs owns the cross-event drain latch. Calling any
      // lower-level opener here would let concurrent inbox clicks create two
      // tabs for the same queued offer.
      await this.releaseQueuedHandoffs(jobID, true);
      job = findByJob(this.store, jobID);
      if (job === undefined || !this.offerURLs.has(jobID) || job.tab_id < 0) {
        return this.failure("handoff_open_failed", "The offered handoff could not be opened");
      }
      await this.focusManagedTab(job.tab_id);
      return { ok: true, opened: true };
    }
    if (job.tab_id < 0) {
      this.enqueueHandoffDrive({ jobID, purpose: "inbox-open" });
      await this.drainHandoffDriveQueue();
      job = findByJob(this.store, jobID);
      if (job !== undefined && job.tab_id >= 0) {
        await this.focusManagedTab(job.tab_id);
        return { ok: true, opened: true };
      }
      return this.failure("handoff_queued", "The handoff is waiting for an available browser slot");
    }
    const openurl = this.offerURLs.get(jobID);
    if (openurl === undefined) {
      return this.failure("handoff_open_failed", "The offered handoff could not be opened");
    }
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: openurl,
        jobId: jobID,
        purpose: "inbox-open",
      });
    } catch {
      tabID = undefined;
    }
    return tabID === undefined
      ? this.failure("handoff_open_failed", "The offered handoff tab could not be focused")
      : { ok: true, opened: true };
  }


  /** A daemon-directed retry may refresh an expired authentication exchange;
   * the inbox and popup retain focus-only behavior so they cannot disrupt a
   * provider page that is already downloading. */
  private async focusDaemonHandoff(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    const openurl = this.offerURLs.get(jobID);
    if (job !== undefined && job.tab_id >= 0 && openurl !== undefined) {
      let needsFreshResolver = false;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        needsFreshResolver =
          job.status === "auth_pending" || (typeof tab.url === "string" && isAuthenticationURL(tab.url));
      } catch {
        // A missing tab is handled by the focus/open fallback below.
      }
      if (needsFreshResolver) {
        const reopened = await this.openManagedTab({
          url: openurl,
          jobId: jobID,
          purpose: "redrive",
        });
        if (reopened !== undefined) {
          if (!this.handoffDrives.has(jobID) && this.handoffDrives.size < HANDOFF_DRIVE_LIMIT) {
            this.registerHandoffDrive(jobID, reopened);
          }
          return;
        }
      }
    }
    await this.openHandoff(jobID);
  }

  private failure(code: string, message: string): BrokerFailure {
    return { ok: false, error: { code, message } };
  }

  private nativeFailure(result: NativeRequestResult): BrokerFailure {
    switch (result.kind) {
      case "timeout":
        return this.failure("timeout", "The daemon did not respond in time");
      case "transport":
        return this.failure(result.code ?? "connection_lost", result.message ?? "The daemon is unavailable");
      default:
        return this.failure(result.code ?? "daemon_error", result.message ?? "The daemon rejected the request");
    }
  }

  /** A hello acknowledgement belongs to exactly one native port. */
  hasCurrentHello(): boolean {
    return this.port !== null && this.helloAckGeneration === this.portGeneration;
  }

  private settleHelloWaiters(acknowledged: boolean): void {
    for (const waiter of this.helloWaiters) waiter(acknowledged);
    this.helloWaiters.clear();
  }

  private waitForCurrentHello(): Promise<boolean> {
    if (this.hasCurrentHello()) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      const waiter = (acknowledged: boolean) => {
        if (!this.helloWaiters.delete(waiter)) return;
        resolve(acknowledged);
      };
      this.helloWaiters.add(waiter);
      this.deps.setTimeout(() => waiter(false), HELLO_WAIT_TIMEOUT_MS);
    });
  }

  /** Foreground requests must never rely on the next passive reconnect tick. */
  private async ensureConnected(): Promise<boolean> {
    await this.ready;
    if (this.hasCurrentHello()) return true;
    this.reconnectAttempts = 0;
    // A freshly opened port already has a current hello in flight; coalesce
    // foreground callers on that acknowledgement rather than churning ports.
    if (this.port !== null && this.helloSentGeneration === this.portGeneration) {
      return this.waitForCurrentHello();
    }
    if (this.port === null) {
      this.closingDeliberately = false;
      this.connect();
    } else {
      this.reconnectForHello();
    }
    return this.waitForCurrentHello();
  }

  private nextRequestID(): string {
    // UUID text is already a valid msg-id once hyphens are removed. A local
    // sequence makes a deterministic test seam and a late echo unable to
    // collide with a later request in this worker lifetime.
    const random = this.deps.randomUUID().replace(/-/g, "");
    const suffix = `_${this.requestIDSequence++}`;
    return random.length + suffix.length <= 64 ? `${random}${suffix}` : random;
  }

  private failPendingNativeRequests(code: string, message: string): void {
    for (const pending of this.pendingNativeRequests.values()) {
      pending.resolve({ kind: "transport", code, message });
    }
    this.pendingNativeRequests.clear();
  }

  private sendCorrelated(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    expectedType: BrowserMessageType,
  ): Promise<NativeRequestResult> {
    const requestID = this.nextRequestID();
    return new Promise<NativeRequestResult>((resolve) => {
      const pending: PendingNativeRequest = { expectedType, resolve };
      this.pendingNativeRequests.set(requestID, pending);
      this.deps.setTimeout(() => {
        if (this.pendingNativeRequests.get(requestID) !== pending) return;
        this.pendingNativeRequests.delete(requestID);
        resolve({ kind: "timeout" });
      }, TRIAGE_REQUEST_TIMEOUT_MS);
      if (!this.send(type, { ...payload, request_id: requestID })) {
        this.pendingNativeRequests.delete(requestID);
        resolve({
          kind: "transport",
          code: "connection_lost",
          message: "The daemon connection was lost before the request was sent",
        });
        this.reconnectForHello();
      }
    });
  }

  private async requestNative(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    expectedType: BrowserMessageType,
    feature: string,
    mutation: boolean,
  ): Promise<NativeRequestResult> {
    if (!CORRELATED_RESULT_TYPES.has(expectedType)) {
      throw new Error(`papio: correlated request expects unrouted reply type ${expectedType}`);
    }
    const attempts = mutation ? 1 : 2;
    for (let attempt = 0; attempt < attempts; attempt += 1) {
      if (!(await this.ensureConnected())) {
        return {
          kind: "transport",
          code: "connection_timeout",
          message: "Could not establish a current daemon session",
        };
      }
      if (!(this.store.daemonFeatures ?? []).includes(feature)) {
        return {
          kind: "response",
          code: "feature_unavailable",
          message: "This daemon does not support the requested inbox feature",
        };
      }
      const result = await this.sendCorrelated(type, payload, expectedType);
      if (result.kind !== "transport" || mutation || attempt + 1 === attempts) return result;
      // Reads are safe to retry once after a confirmed transport failure;
      // mutations deliberately return their ambiguous status to the page.
    }
    return { kind: "transport", code: "connection_lost", message: "The daemon is unavailable" };
  }

  async requestTriageSnapshot(
    request: { schema_versions: [1]; limit?: number; cursor?: string },
  ): Promise<BrokerReply<{ snapshot: Record<string, unknown> }>> {
    const schemaVersions: [1] | [2] = (this.store.daemonFeatures ?? []).includes(TRIAGE_SNAPSHOT_SCHEMA_2_FEATURE)
      ? [2]
      : request.schema_versions;
    const result = await this.requestNative(
      "triage_snapshot_request",
      { ...request, schema_versions: schemaVersions },
      "triage_snapshot_response",
      TRIAGE_SNAPSHOT_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const { request_id: _requestID, ...snapshot } = result.payload;
    const counts = snapshot["counts"];
    if (typeof counts === "object" && counts !== null && typeof (counts as Record<string, unknown>)["pending_total"] === "number") {
      this.triagePendingCount = (counts as Record<string, number>)["pending_total"];
      await this.syncConnectionBadge();
    }
    return { ok: true, snapshot };
  }

  async requestTriageCounts(): Promise<BrokerReply<{ counts: Record<string, unknown>; generated_at: string }>> {
    const payload = (this.store.daemonFeatures ?? []).includes(TRIAGE_COUNTS_SCHEMA_2_FEATURE)
      ? { schema_versions: [2] }
      : {};
    const result = await this.requestNative(
      "triage_counts_request",
      payload,
      "triage_counts_response",
      TRIAGE_SNAPSHOT_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const counts = result.payload["counts"];
    if (typeof counts !== "object" || counts === null) return this.failure("invalid_response", "The daemon returned invalid counts");
    const record = counts as Record<string, unknown>;
    const pending = record["pending_total"];
    if (typeof pending === "number") {
      this.triagePendingCount = pending;
      await this.syncConnectionBadge();
    }
    const actionsRequiresAuth = record["actions_requires_auth"];
    if (typeof actionsRequiresAuth === "number") {
      this.triageActionsRequiresAuth = actionsRequiresAuth;
      this.triageActionsRequiresAuthAt = this.deps.now();
    } else {
      this.triageActionsRequiresAuth = undefined;
      this.triageActionsRequiresAuthAt = undefined;
    }
    await this.keepaliveManager?.sync();
    return {
      ok: true,
      counts: record,
      generated_at: new Date(this.deps.now()).toISOString(),
    };
  }

  async requestStats(): Promise<BrokerReply<{ stats: Record<string, unknown> }>> {
    const result = await this.requestNative(
      "stats_request",
      {},
      "stats_response",
      STATS_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const { request_id: _requestID, ...stats } = result.payload;
    return { ok: true, stats };
  }
  async requestActivity(limit?: number): Promise<BrokerReply<{ feature: boolean; entries: ActivityEntryPayload[] }>> {
    // Do not even open/reconnect the native path for a daemon that has already
    // advertised that it cannot serve the feed. The inbox treats this as a
    // normal, feature-gated empty result rather than an error.
    if (!(this.store.daemonFeatures ?? []).includes(ACTIVITY_FEED_FEATURE)) {
      return { ok: true, feature: false, entries: [] };
    }
    const result = await this.requestNative(
      "activity_request",
      limit === undefined ? {} : { limit },
      "activity_response",
      ACTIVITY_FEED_FEATURE,
      false,
    );
    if (result.kind !== "response") return this.nativeFailure(result);
    if (result.code === "feature_unavailable") return { ok: true, feature: false, entries: [] };
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    if (result.payload === undefined) return this.nativeFailure(result);
    const entries = result.payload["entries"];
    if (!Array.isArray(entries)) return this.failure("invalid_response", "The daemon returned invalid activity entries");
    return { ok: true, feature: true, entries: entries as ActivityEntryPayload[] };
  }

  // -------------------------------------------------------------------------
  // ADR-0019: on-page bulk acquisition. Scanning and the local snapshot store
  // are pure browser-local state (Decision 4); only the status/submit round
  // trips below touch the daemon.
  // -------------------------------------------------------------------------

  /** Tab-derived facts the snapshot needs beyond origin. `title` is
   * browser-local UI decoration only (ADR-0019 operator UX requirement:
   * the workspace header names the source page) — it is carried on
   * PageBulkSnapshotView, never on the daemon-facing PageBulkSubmitSource
   * (Decision 6: origin only, never page title). */
  private async pageBulkTabMeta(tabID: number): Promise<{ origin: string; title: string } | null> {
    try {
      const tab = await this.deps.tabs.get(tabID);
      const origin = bareHTTPSOrigin(tab.url);
      if (origin === null) return null;
      const title = tab.title?.trim();
      return { origin, title: title !== undefined && title !== "" ? title : origin };
    } catch {
      return null;
    }
  }

  /** Inject scanDocument into the tab's top frame (Decision 3: no iframes —
   * executeScript's default target is the top frame only) and validate the
   * shape of what comes back. `scanned` is cast to page-scan.ts's own
   * declared ScanResult, the same convention capturePage's caller uses for
   * its PageCapture result, then checked field-by-field before use. */
  private async executePageScan(
    tabID: number,
  ): Promise<
    { ok: true; items: DetectedPaper[]; truncated: boolean; renderedRecordCountHint: number | null } | BrokerFailure
  > {
    let tabURL: string | undefined;
    try {
      tabURL = (await this.deps.tabs.get(tabID)).url;
    } catch {
      return this.failure("scan_failed", "Could not scan the page");
    }
    let injected: { result?: unknown } | undefined;
    try {
      [injected] = await this.deps.scripting.executeScript({
        target: { tabId: tabID },
        func: scanDocument,
        args: [tabURL ?? ""],
      });
    } catch {
      return this.failure("scan_failed", "Could not scan the page");
    }
    const scanned = injected?.result as ScanResult | undefined;
    if (
      scanned === undefined ||
      !Array.isArray(scanned.papers) ||
      typeof scanned.truncated !== "boolean" ||
      (scanned.renderedRecordCountHint !== null && typeof scanned.renderedRecordCountHint !== "number")
    ) {
      return this.failure("scan_failed", "Could not scan the page");
    }
    return {
      ok: true,
      items: scanned.papers,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
  }

  private async loadPageBulkStore(): Promise<PageBulkScanStore> {
    if (this.deps.pageBulkScans === undefined) return emptyPageBulkScanStore();
    return this.deps.pageBulkScans.get();
  }

  private async savePageBulkSnapshot(snapshot: PageBulkSnapshot): Promise<void> {
    if (this.deps.pageBulkScans === undefined) return;
    const store = await this.deps.pageBulkScans.get();
    await this.deps.pageBulkScans.set(withPageBulkSnapshot(store, snapshot));
  }

  /** Scan tabID's top frame and persist a fresh snapshot (generation 1). The
   * explicit popup click that reaches this method IS v1's scan consent
   * (Decision 1, Decision 2) — no allowlist check gates it. */
  async runPageBulkScan(tabID: number): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const meta = await this.pageBulkTabMeta(tabID);
    if (meta === null) {
      return this.failure("invalid_page", "papio can only scan an ordinary secure (https) page");
    }
    const scanned = await this.executePageScan(tabID);
    if (!scanned.ok) return scanned;
    const snapshot: PageBulkSnapshotView = {
      scanId: this.deps.randomUUID(),
      sourceTabId: tabID,
      sourceOrigin: meta.origin,
      sourceTitle: meta.title,
      pdfGrabAvailable: this.pdfGrabAvailable(),
      scannedAt: new Date().toISOString(),
      documentGeneration: 1,
      items: scanned.items,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
    await this.savePageBulkSnapshot(snapshot);
    return { ok: true, snapshot };
  }

  /** Scan and open one selection workspace per active scan (Decision 4: a
   * new tab per scan, never a singleton like the inbox). */
  async startPageBulkScan(tabID: number, pageBulkBaseURL: string): Promise<BrokerReply<{ scan_id: string }>> {
    const scanned = await this.runPageBulkScan(tabID);
    if (!scanned.ok) return scanned;
    try {
      await this.deps.tabs.create({
        url: `${pageBulkBaseURL}?scan=${encodeURIComponent(scanned.snapshot.scanId)}`,
        active: true,
      });
    } catch {
      return this.failure("open_failed", "Could not open the selection workspace");
    }
    return { ok: true, scan_id: scanned.snapshot.scanId };
  }

  /** Re-run the scan for an already-open workspace's scanId (the Rescan
   * button), bumping documentGeneration so a superseded reply can be
   * detected client-side. Reuses the scanId — never a new storage slot. */
  async requestPageBulkRescan(scanID: string): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const store = await this.loadPageBulkStore();
    const existing = store.byId[scanID];
    if (existing === undefined) return this.failure("scan_not_found", "This scan is no longer open");
    const meta = await this.pageBulkTabMeta(existing.sourceTabId);
    if (meta === null) return this.failure("tab_unavailable", "The source tab is no longer available");
    const scanned = await this.executePageScan(existing.sourceTabId);
    if (!scanned.ok) return scanned;
    const snapshot: PageBulkSnapshotView = {
      scanId: scanID,
      sourceTabId: existing.sourceTabId,
      sourceOrigin: meta.origin,
      sourceTitle: meta.title,
      pdfGrabAvailable: this.pdfGrabAvailable(),
      scannedAt: new Date().toISOString(),
      documentGeneration: existing.documentGeneration + 1,
      items: scanned.items,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
    await this.savePageBulkSnapshot(snapshot);
    return { ok: true, snapshot };
  }

  /** Load an already-open workspace's snapshot without rescanning — the
   * page-bulk.ts route's initial `?scan=<id>` read, and the missing half of
   * the scan/rescan pair the predecessor landed (a workspace tab reloading
   * or a fresh tab opened at ?scan=<id> had no way to fetch its snapshot
   * without this). Returns scan_not_found once the snapshot has aged out of
   * the bounded PAGE_BULK_SNAPSHOT_LIMIT store or the browser session ended
   * (Decision 4: chrome.storage.session only, never persisted past the
   * session) — the operator-visible "scan expired" state. */
  async getPageBulkSnapshot(scanID: string): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const store = await this.loadPageBulkStore();
    const existing = store.byId[scanID] as PageBulkSnapshotView | undefined;
    if (existing === undefined) return this.failure("scan_not_found", "This scan is no longer open");
    return { ok: true, snapshot: existing };
  }
  pdfGrabAvailable(): boolean {
    return (
      this.store.connectionStatus === "connected" &&
      this.deps.downloads.onDeterminingFilename !== undefined &&
      (this.store.daemonFeatures ?? []).includes(PDF_GRAB_FEATURE)
    );
  }

  private persistPdfGrabCorrelations(): void {
    if (this.deps.pdfGrabCorrelations === undefined) return;
    void this.deps.pdfGrabCorrelations.set(Object.fromEntries(this.pdfGrabCorrelations.entries())).catch(() => {});
  }

  private notifyPdfGrab(scanID: string, grabID: string, state: string, detail?: string): void {
    const send = this.deps.runtimeSendMessage;
    if (send === undefined) return;
    void send({ type: "papio.pageBulk.grabState", scan_id: scanID, grab_id: grabID, state, ...(detail !== undefined ? { detail } : {}) }).catch(() => {});
  }
  private async reconcilePdfGrabCorrelations(): Promise<void> {
    for (const [grabID, correlation] of this.pdfGrabCorrelations) {
      let items: DownloadItemLike[];
      try {
        items = await this.deps.downloads.search({ id: correlation.downloadID });
      } catch {
        continue;
      }
      const item = items[0];
      if (item?.state === "interrupted") {
        this.notifyPdfGrab(correlation.scanID, grabID, "failed", "The PDF grab download was interrupted");
        this.pdfGrabCorrelations.delete(grabID);
        this.persistPdfGrabCorrelations();
        continue;
      }
      if (item?.state === "complete") {
        // The settled file is authoritative; the sweeper handles completed downloads.
        continue;
      }
      this.grabDownloads.set(grabID, {
        ids: new Set([correlation.downloadID]),
        tabID: correlation.tabID,
        scanID: correlation.scanID,
        url: correlation.url,
        steeringPath: correlation.steeringPath,
      });
    }
  }

  async requestPdfGrab(request: { tab_id: number; url: string; title?: string | undefined; workspace_tab_id?: number | undefined; scan_id?: string | undefined }): Promise<BrokerReply<{ grab_id: string }>> {
    if (!this.pdfGrabAvailable()) return this.failure("feature_unavailable", "PDF grabbing needs Chrome download steering and a compatible daemon");
    const result = await this.requestNative(
      "pdf_grab_request",
      { url: request.url, ...(request.title !== undefined ? { title: request.title } : {}) },
      "pdf_grab_result",
      PDF_GRAB_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The PDF grab is unavailable");
    const outcome = result.payload["outcome"];
    const grabID = result.payload["grab_id"];
    const steeringPath = result.payload["steering_path"];
    if (outcome !== "steering" || typeof grabID !== "string" || typeof steeringPath !== "string") {
      return this.failure("grab_failed", typeof result.payload["detail"] === "string" ? result.payload["detail"] : "The daemon could not start this PDF grab");
    }
    const workspaceTabID = request.workspace_tab_id ?? request.tab_id;
    const scanID = request.scan_id ?? "";
    this.grabDownloads.set(grabID, { ids: new Set<number>(), tabID: workspaceTabID, scanID, url: request.url, steeringPath });
    this.pendingGrabDownloadURLs.set(request.url, { grabID, tabID: workspaceTabID, steeringPath });
    try {
      const id = await this.deps.downloads.download({ url: request.url, conflictAction: "uniquify", saveAs: false });
      const track = this.grabDownloads.get(grabID);
      if (track !== undefined) track.ids.add(id);
      this.pdfGrabCorrelations.set(grabID, {
        scanID,
        tabID: workspaceTabID,
        state: "grabbed",
        downloadID: id,
        steeringPath,
        url: request.url,
      });
      this.persistPdfGrabCorrelations();
      this.notifyPdfGrab(scanID, grabID, "grabbed");
      return { ok: true, grab_id: grabID };
    } catch {
      this.grabDownloads.delete(grabID);
      this.pdfGrabCorrelations.delete(grabID);
      this.persistPdfGrabCorrelations();
      this.pendingGrabDownloadURLs.delete(request.url);
      return this.failure("grab_failed", "Could not start the browser download");
    } finally {
      this.pendingGrabDownloadURLs.delete(request.url);
    }
  }


  async requestPageBulkStatus(
    request: { scan_id: string; identifiers: PageBulkIdentifier[]; rendered_record_count_hint?: number },
  ): Promise<BrokerReply<{ items: PageBulkStatusItem[]; truncated: boolean }>> {
    const result = await this.requestNative(
      "page_bulk_status_request",
      request,
      "page_bulk_status_result",
      PAGE_BULK_ACQUIRE_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const items = result.payload["items"];
    const truncated = result.payload["truncated"];
    if (!Array.isArray(items) || typeof truncated !== "boolean") {
      return this.failure("invalid_response", "The daemon returned an invalid page-bulk status result");
    }
    return { ok: true, items: items as PageBulkStatusItem[], truncated };
  }

  /** A submit is a mutation: it creates a batch, so — like requestTriageDecision
   * — it gets exactly one attempt, never the read path's transport retry. */
  async requestPageBulkSubmit(
    request: { scan_id: string; canonical_keys: string[]; source: PageBulkSubmitSource },
  ): Promise<
    BrokerReply<{ submitted: number; joined: number; already_owned: number; invalid: number; batch_id: string }>
  > {
    const result = await this.requestNative(
      "page_bulk_submit_request",
      request,
      "page_bulk_submit_result",
      PAGE_BULK_ACQUIRE_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const payload = result.payload;
    const submitted = payload["submitted"];
    const joined = payload["joined"];
    const alreadyOwned = payload["already_owned"];
    const invalid = payload["invalid"];
    const batchID = payload["batch_id"];
    if (
      typeof submitted !== "number" ||
      typeof joined !== "number" ||
      typeof alreadyOwned !== "number" ||
      typeof invalid !== "number" ||
      typeof batchID !== "string"
    ) {
      return this.failure("invalid_response", "The daemon returned an invalid page-bulk submit result");
    }
    return { ok: true, submitted, joined, already_owned: alreadyOwned, invalid, batch_id: batchID };
  }

  /** Membership read/write for the scanner-scoped allowlist (Decision 2).
   * Absent dep degrades to "never allowlisted" rather than throwing. */
  async pageBulkAllowlistContains(origin: string): Promise<BrokerReply<{ allowed: boolean }>> {
    if (this.deps.scannerAllowlist === undefined) return { ok: true, allowed: false };
    const origins = await this.deps.scannerAllowlist.get();
    return { ok: true, allowed: origins.includes(origin) };
  }

  async setPageBulkAllowlist(origin: string, allowed: boolean): Promise<BrokerReply<{ allowed: boolean }>> {
    if (this.deps.scannerAllowlist === undefined) return { ok: true, allowed: false };
    const origins = await this.deps.scannerAllowlist.get();
    const next = allowed ? [...origins.filter((o) => o !== origin), origin] : origins.filter((o) => o !== origin);
    await this.deps.scannerAllowlist.set(next);
    return { ok: true, allowed };
  }


  async requestTriageDecision(
    request: { item_id: string; op: "acquire" | "dismiss"; watch_scope?: "all" | number[] },
  ): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "triage_decide",
      request,
      "triage_decide_result",
      TRIAGE_MUTATIONS_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string" ? { detail: result.payload["detail"] } : {}),
    };
  }

  async requestActionResolve(
    request: { action_id: number; verdict: "accept" | "reject" | "dismiss"; expected_revision: number; expected_sha256?: string },
  ): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "human_action_resolve",
      request,
      "human_action_resolve_result",
      TRIAGE_MUTATIONS_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string" ? { detail: result.payload["detail"] } : {}),
    };
  }

  // Decision 4's confirm_request_exists/confirm_request_absent mutations
  // (triage-snapshot/3). Gated on the v3 snapshot feature rather than
  // TRIAGE_MUTATIONS_FEATURE: a daemon that never emits document_delivery
  // items has nothing for this RPC to act on, and open_request_history is
  // deliberately not here — it never mutates anything and is handled
  // locally by the inbox page.
  async requestDeliveryReconcile(
    request: { job_id: string; operation: "confirm_request_exists" | "confirm_request_absent"; provider_reference?: string },
  ): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "delivery_reconcile_request",
      request,
      "delivery_reconcile_result",
      TRIAGE_SNAPSHOT_SCHEMA_3_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string" ? { detail: result.payload["detail"] } : {}),
    };
  }

  async requestPreview(
    request: { action_id: number },
  ): Promise<BrokerReply<{ outcome: string; detail?: string; preview?: Record<string, unknown> }>> {
    const result = await this.requestNative(
      "review_preview_request",
      request,
      "review_preview_result",
      REVIEW_PREVIEW_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const outcome = result.payload["outcome"] as string;
    if (outcome === "error") {
      return {
        ok: true,
        outcome,
        ...(typeof result.payload["detail"] === "string" ? { detail: result.payload["detail"] } : {}),
      };
    }
    const { request_id: _requestID, outcome: _outcome, ...preview } = result.payload;
    return { ok: true, outcome, preview };
  }

  /**
   * Record the user's informed terms-consent choice (popup first-use prompt),
   * clear the pending-prompt flags, and — when they consented — re-drive the
   * still-open terms gate on every flagged job so the current downloads
   * complete without a second visit. Idempotent and safe if jobs have moved on.
   */
  async requestTermsConsent(value: Exclude<TermsConsent, undefined>): Promise<void> {
    await this.ready;
    await this.deps.settings.setTermsConsent(value);
    if (value !== "accept") {
      // User declined auto-accept: clear the one-time prompt flag so the popup
      // stops asking; any open gate stays assisted.
      for (const jobID of this.store.activeJobs.filter((j) => j.needs_terms_consent === true).map((j) => j.job_id)) {
        await this.update((s) => patchJob(s, jobID, { needs_terms_consent: false }));
      }
      return;
    }
    await this.redrivePendingTermsGates();
  }

  /** Re-drive every job still parked at a terms gate now that consent is
   * accepted: clear the one-time prompt flag and re-run classification on the
   * live provider tab so an open terms modal is accepted and the download
   * completes without a second visit. Runs when the user grants consent AND on
   * worker startup, so a grant that landed while the worker was asleep (missing
   * the one-shot re-drive) still completes on the next connect. Idempotent: a
   * job with no live tab or an already-closed modal is a no-op. */
  private async redrivePendingTermsGates(): Promise<void> {
    if ((await this.deps.settings.getTermsConsent()) !== "accept") return;
    const flagged = this.store.activeJobs
      .filter((j) => j.needs_terms_consent === true && j.tab_id >= 0)
      .map((j) => j.job_id);
    for (const jobID of flagged) {
      await this.update((s) => patchJob(s, jobID, { needs_terms_consent: false }));
      try {
        await this.reclassifyCurrentProviderPage(jobID);
      } catch (e) {
        console.error("papio: terms re-drive failed; staying assisted", e);
      }
    }
  }

  /** Inject the consented terms-accept click on the tracked tab. Gated by the
   * caller on recorded consent; returns whether a control was clicked. */
  private async acceptTerms(
    jobID: string,
    rule: { modalSelector: string; control?: string; textAny: string[] },
  ): Promise<boolean> {
    const job = findByJob(this.store, jobID);
    if (!job || job.tab_id < 0) return false;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: clickTermsAccept,
        args: [rule.modalSelector, rule.textAny, rule.control ?? null],
      });
      return results[0]?.result === true;
    } catch (e) {
      console.error("papio: terms accept click failed; staying assisted", e);
      return false;
    }
  }

  private pendingJobFor(item: DownloadItemLike): string | undefined {
    const observed = [item.url, item.finalUrl].filter((v): v is string => typeof v === "string");
    const jobs = new Set<string>();
    for (const [pendingURL, jobID] of this.pendingDownloadURLs) {
      if (observed.some((url) => url === pendingURL || sameDownloadRoute(url, pendingURL))) {
        jobs.add(jobID);
      }
    }
    return jobs.size === 1 ? jobs.values().next().value : undefined;
  }

  /** downloads.download may resolve with the ID before Chrome asks extensions
   * to determine the filename. IDs are exact and contain no provider secret. */
  private trackedJobFor(downloadID: number): string | undefined {
    let matched: string | undefined;
    for (const [jobID, track] of this.downloads) {
      if (!track.ids.has(downloadID)) continue;
      if (matched !== undefined && matched !== jobID) return undefined;
      matched = jobID;
    }
    return matched;
  }
  private pendingGrabFor(item: DownloadItemLike): PdfGrabTrack | undefined {
    const observed = [item.url, item.finalUrl].filter((value): value is string => typeof value === "string");
    for (const [pendingURL, pending] of this.pendingGrabDownloadURLs) {
      if (observed.some((url) => url === pendingURL || sameDownloadRoute(url, pendingURL))) {
        return this.grabDownloads.get(pending.grabID);
      }
    }
    return undefined;
  }

  private trackedGrabFor(downloadID: number): string | undefined {
    for (const [grabID, track] of this.grabDownloads) {
      if (track.ids.has(downloadID)) return grabID;
    }
    return undefined;
  }

  private bindListeners(): void {
    if (this.listenersBound) return;
    this.listenersBound = true;
    this.deps.tabs.onUpdated.addListener((tabID, change, tab) => {
      return this.onTabUpdated(tabID, change, tab);
    });
    this.deps.tabs.onRemoved.addListener((tabID) => {
      // Synchronous before the async removal handler: per-tab epochs, settle
      // timers and origin associations must never accumulate against a dead
      // tab id even if onTabRemoved's own work is still pending on `ready`.
      this.keepaliveManager?.noteTabRemoved(tabID);
      return this.onTabRemoved(tabID);
    });
    this.deps.tabs.onActivated.addListener(({ tabId }) => {
      return this.onTabActivated(tabId);
    });
    this.deps.downloads.onCreated.addListener((item) => {
      return this.onDownloadCreated(item);
    });
    this.deps.downloads.onChanged.addListener((delta) => {
      return this.onDownloadChanged(delta);
    });
    this.deps.downloads.onDeterminingFilename?.addListener((item, suggest) => {
      const exactJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
      const job = exactJobID ? findByJob(this.store, exactJobID) : this.correlate(item);
      const grabID = this.trackedGrabFor(item.id);
      const grab = grabID === undefined ? this.pendingGrabFor(item) : this.grabDownloads.get(grabID);
      const base = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
      if (grab !== undefined && base.length > 0 && job === undefined) {
        suggest({ filename: `${grab.steeringPath}${base}`, conflictAction: "uniquify" });
        return;
      }
      if (!job || base.length === 0) return;
      suggest({ filename: `papio/${job.job_id}/${base}`, conflictAction: "uniquify" });
    });
    this.deps.alarms.onAlarm.addListener((alarm) => {
      if (alarm.name === KEEPALIVE_ALARM) return this.onKeepaliveAlarm();
    });
  }

  /** The keepalive alarm woke the worker. The top-level start() on this same
   * spin-up already reconnects; this is the safety net that re-establishes the
   * daemon connection if it is still down, so any queued offers arrive. */
  /** The keepalive alarm both reconnects the broker and refreshes the
   * non-authoritative pending count when the negotiated schema supports it. */
  private async onKeepaliveAlarm(): Promise<void> {
    await this.ready;
    await this.releaseExpiredQueuedHandoffs();
    // Recovery runs unconditionally on this wake, independent of the native
    // port: a service worker that died mid-probe still needs its dirty/paused
    // origins re-probed even while the daemon connection is also down. It is
    // deliberately NOT awaited — reconnecting the daemon and refreshing the
    // triage count are latency-sensitive on this one wake, and a session probe
    // can take seconds of browser API work with nothing here depending on its
    // result.
    void this.keepaliveManager?.onWake();
    if (this.port === null && !this.closingDeliberately) {
      this.reconnectAttempts = 0;
      this.connect();
      return;
    }
    if (this.hasCurrentHello() && (this.store.daemonFeatures ?? []).includes(TRIAGE_SNAPSHOT_FEATURE)) {
      await this.requestTriageCounts();
    }
  }

  /** Consecutive unplanned disconnects; resets on a healthy inbound frame. */
  private reconnectAttempts = 0;
  /** Set while disconnect() runs so the onDisconnect listener knows the
   * teardown was deliberate (protocol error / shutdown): deliberate
   * disconnects must NOT auto-reconnect — fail closed stays failed. */
  private closingDeliberately = false;

  private connect(): void {
    // A previous service-worker instance may have persisted a completed
    // handshake. Clear it before hello so no request can use stale features.
    this.store = clearNegotiationState(this.store);
    if (this.hydrated) void this.update((current) => current);
    const port = this.deps.connectNative(NATIVE_HOST);
    this.port = port;
    this.portGeneration += 1;
    this.helloAckGeneration = -1;
    this.helloSentGeneration = -1;
    this.triagePendingCount = undefined;
    this.triageActionsRequiresAuth = undefined;
    this.triageActionsRequiresAuthAt = undefined;
    this.sessionEvidenceSentAt.clear();
    port.onMessage.addListener((msg) => {
      if (this.port !== port) return;
      this.reconnectAttempts = 0;
      return this.enqueueInbound(msg, port);
    });
    port.onDisconnect.addListener(() => this.onPortDisconnect(port));
    // hello is the mandatory first frame after connect (seq 0).
    const adapterVersions: Record<string, string> = {};
    for (const spec of this.deps.adapterSpecs) adapterVersions[spec.id] = spec.version;
    this.helloSentGeneration = this.portGeneration;
    if (
      !this.send("hello", {
        extension_version: this.deps.manifestVersion,
        adapter_versions: adapterVersions,
      })
    ) {
      this.helloSentGeneration = -1;
    }
  }

  private async onPortDisconnect(port: NativePort): Promise<void> {
    // A stale port may report its close after recovery opened a replacement.
    if (this.port !== port) return;
    this.port = null;
    this.failPageAcquireWaiters("The daemon disconnected before acknowledging this page");
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon disconnected before acknowledging the request",
    );
    await this.update((s) => ({ ...s, connectionStatus: "disconnected" }));
    await this.syncConnectionBadge("disconnected");
    if (this.closingDeliberately) return;
    // Unplanned port death (daemon restart, host exit, Chrome nap): the daemon
    // owns all durable state, so reconnect + re-hello is always safe. Bounded
    // exponential backoff, capped at 60s, gives up after 8 attempts until the
    // next user-visible event restarts the cycle.
    if (this.reconnectAttempts >= 8) return;
    const delay = Math.min(60_000, 1_000 * 2 ** this.reconnectAttempts);
    this.reconnectAttempts += 1;
    this.deps.setTimeout(() => {
      if (this.port === null && !this.closingDeliberately) this.connect();
    }, delay);
  }

  private disconnect(): void {
    this.closingDeliberately = true;
    const port = this.port;
    this.port = null;
    this.failPageAcquireWaiters("The daemon disconnected before acknowledging this page");
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon disconnected before acknowledging the request",
    );
    if (!port) return;
    try {
      port.disconnect();
    } catch {
      // Already torn down.
    }
  }

  /** Replace a live native port whose daemon forgot this hello-session. */
  private reconnectForHello(): void {
    const port = this.port;
    if (!port) return;
    // Clear ownership before closing: onDisconnect for this stale port must not
    // schedule a second recovery after connect() has installed its replacement.
    this.closingDeliberately = true;
    this.port = null;
    this.failPageAcquireWaiters("The daemon restarted before acknowledging this page");
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon restarted before acknowledging the request",
    );
    try {
      port.disconnect();
    } catch {
      // Chrome can report an already-closed native port.
    } finally {
      this.closingDeliberately = false;
    }
    this.reconnectAttempts = 0;
    this.connect();
  }

  private async update(fn: (store: StoreShape) => StoreShape): Promise<void> {
    const signInBlockersBefore = this.signInBlockerCount();
    // Apply the transform synchronously so in-memory state stays in event order.
    this.store = fn(this.store);
    const signInBlockersChanged = signInBlockersBefore !== this.signInBlockerCount();
    // Persist after any in-flight save settles, writing the latest snapshot so
    // reordered chrome.storage writes cannot resurrect an older one.
    const save = this.saveChain.then(() => this.deps.backend.save(this.store));
    // Keep the chain alive across a failed save without unhandled rejections;
    // this caller still observes the real error below.
    this.saveChain = save.catch(() => {});
    await save;
    if (signInBlockersChanged) await this.syncConnectionBadge();
  }

  private async upsertJobWithOffer(job: ActiveJob, offerURL: string): Promise<void> {
    this.offerURLs.set(job.job_id, offerURL);
    if (job.requires_auth === true) this.keepaliveManager?.learnResolver(offerURL);
    await this.update((s) => {
      const withJob = upsertJob(s, job);
      return {
        ...withJob,
        offerURLs: { ...(s.offerURLs ?? {}), [job.job_id]: offerURL },
      };
    });
  }

  private async removeJobWithOffer(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    const providerKey = job === undefined ? undefined : this.providerKeyForJob(job);
    this.releaseHandoffDrive(jobID);
    this.managedTabURLs.delete(jobID);
    this.deliveryJobs.delete(jobID);
    this.resolverRoutes.delete(jobID);
    this.deliverySessionEvidence.delete(jobID);
    this.offerURLs.delete(jobID);
    this.queuedHandoffTimers.delete(jobID);
    this.classifyRetries.delete(jobID);
    this.loginEntityIDs.delete(jobID);
    this.federatedLoginRouted.delete(jobID);
    this.federatedReDriven.delete(jobID);
    this.waitingForSessionTimers.delete(jobID);
    this.handoffOutcomeSent.delete(`${jobID}:stale_sso`);
    this.handoffOutcomeSent.delete(`${jobID}:auth_error`);
    this.handoffOutcomeSent.delete(`${jobID}:ui_changed`);
    this.challengeBlockedOutcomeSent.delete(`${jobID}:challenge_blocked`);
    this.authFailureSurfaced.delete(jobID);
    this.staleRecoveryEpochs.delete(jobID);
    this.staleRecoveryAttemptedEpochs.delete(jobID);
    this.staleRecoveryInFlightEpochs.delete(jobID);
    this.openAthensErrorRecheckEpochs.delete(jobID);
    this.resolverNoEntitlementSent.delete(jobID);
    this.proquestAccountIDs.delete(jobID);
    this.accountIdAppended.delete(jobID);
    await this.clearFederatedLoginOwnerForJob(jobID);
    await this.update((s) => {
      const offerURLs = { ...(s.offerURLs ?? {}) };
      delete offerURLs[jobID];
      return { ...clearPendingDelivery(removeJob(s, jobID), jobID), offerURLs };
    });
    if (providerKey !== undefined) await this.releaseProviderDrainWhenUnused(providerKey);
    await this.closeWorkWindowIfIdle();
    await this.dropStaleHandoffGroup();
    if (!this.drainingHandoffDriveQueue) await this.drainHandoffDriveQueue();
  }

  /** Close papio's dedicated work window once no handoff owns a broker tab in
   * it, so the background window does not accumulate across handoffs. A pinned
   * tab (the keepalive resolver session) keeps the window alive; anything else
   * left over is an orphaned broker tab and closing the window reaps it too.
   * No-op when work-window mode is off or the platform lacks the windows API. */
  private async closeWorkWindowIfIdle(): Promise<void> {
    const windows = this.deps.windows;
    const windowID = this.store.workWindowID;
    if (windows === undefined || windowID === undefined) return;
    if (this.store.activeJobs.some((job) => job.tab_id >= 0)) return;
    let win: WindowInfo | undefined;
    try {
      win = await windows.get(windowID);
    } catch {
      // Already gone (user closed it): drop the stale id so the next handoff
      // creates a fresh window rather than reusing a dead one.
      await this.update((s) => {
        const next = { ...s };
        delete next.workWindowID;
        return next;
      });
      return;
    }
    if ((win.tabs ?? []).some((tab) => tab.pinned === true)) return;
    const trackedTabIDs = new Set(
      this.store.activeJobs.filter((job) => job.tab_id >= 0).map((job) => job.tab_id),
    );
    if ((win.tabs ?? []).some((tab) => tab.id !== undefined && tab.pinned !== true && !trackedTabIDs.has(tab.id))) {
      return;
    }
    try {
      await windows.remove(windowID);
    } catch {
      // Raced a manual close; the id is cleared below regardless.
    }
    await this.update((s) => {
      const next = { ...s };
      delete next.workWindowID;
      return next;
    });
  }

  /** A keepalive tab can outlive a cancellation, so clear its removed paper's
   * title before retaining the group for reuse. */
  private async dropStaleHandoffGroup(): Promise<void> {
    const groupID = this.store.handoffGroupID;
    if (groupID === undefined) return;
    if (this.store.activeJobs.some((job) => job.tab_id >= 0)) return;
    if (await this.recollapseHandoffGroup()) return;
    await this.update((s) => {
      const next = { ...s };
      delete next.handoffGroupID;
      return next;
    });
  }

  /** Count at most one authentication attempt per broker-tab drive. The SSO
   * redirect dance can toggle auth_pending several times within one tab, so the
   * budget debounces on tab id; each fresh drive (a new tab from a re-offer or a
   * reconcile re-queue) is a distinct attempt. Persisted so attempts accumulate
   * across service-worker restarts within a browser session. */
  private async noteAuthAttempt(jobID: string, tabID: number): Promise<void> {
    if (this.authCountedTabs.has(tabID)) return;
    await this.chargeAuthAttempt(jobID, tabID);
  }

  /** Spend one unit of a job's durable authentication budget and claim its tab.
   * Claiming matters for the stale-IdP re-drive, which charges explicitly
   * because it reuses the SAME tab (noteAuthAttempt's debounce would swallow
   * it): the claim stops the ordinary auth_pending path, which this tab update
   * falls through to next, from charging the same drive a second time. */
  private async chargeAuthAttempt(jobID: string, tabID: number): Promise<void> {
    this.authCountedTabs.add(tabID);
    await this.update((s) => {
      const authAttempts = { ...(s.authAttempts ?? {}) };
      authAttempts[jobID] = (authAttempts[jobID] ?? 0) + 1;
      return { ...s, authAttempts };
    });
  }

  private authAttemptsFor(jobID: string): number {
    return (this.store.authAttempts ?? {})[jobID] ?? 0;
  }
  private rememberStalledAuthHandoff(jobID: string, handoff: StalledAuthHandoff): void {
    this.stalledAuthHandoffs.set(jobID, {
      url: handoff.url,
      providerHosts: [...handoff.providerHosts],
      ...(handoff.expected !== undefined ? { expected: handoff.expected } : {}),
      ...(handoff.requiresAuth !== undefined ? { requiresAuth: handoff.requiresAuth } : {}),
    });
  }


  /** Report the human authentication step for a capped job, at most once per
   * worker lifetime. human_auth_required is non-terminal daemon-side: the job
   * stays parked (awaiting_human) and is re-offered on a future warm launch. */
  private async reportAuthStalled(jobID: string): Promise<void> {
    if (!this.authStalledReported.has(jobID)) {
      this.authStalledReported.add(jobID);
      this.send("provider_outcome", { outcome: "human_auth_required" }, jobID);
    }
    await this.parkHandoffForManual(jobID);
  }

  /** Clear a job's auth-failure budget once a real download proves the session
   * works, so an earlier expired-session streak cannot cap a now-valid job. */
  private clearAuthAttempts(store: StoreShape, jobID: string): StoreShape {
    if (store.authAttempts?.[jobID] === undefined) return store;
    const authAttempts = { ...store.authAttempts };
    delete authAttempts[jobID];
    return { ...store, authAttempts };
  }

  /** Release-grade evidence for `origin`: a persisted authEvidenceByOrigin
   * entry within AUTH_EVIDENCE_TTL_MS, written only by
   * recordFreshSessionEvidence and by a warm institutional landing
   * (recordInstitutionalSession), and dropped by revokeAuthEvidence when a
   * probe commits that the origin is no longer authenticated.
   *
   * Persisted rather than worker-local because an MV3 worker restarts
   * constantly, and expiring rather than sticky because a session ends: the
   * worker-local Set that used to short-circuit this check never expired and
   * was never deleted, so an operator who signed out kept authorizing
   * releases until the worker happened to die.
   *
   * The global lastAuthReturnedAt display field is still excluded on its own —
   * ADR-0013: a timing frame is not identity evidence; only an origin-scoped
   * observation is. */
  private hasAuthEvidence(origin: string): boolean {
    const evidencedAt = this.store.authEvidenceByOrigin?.[origin];
    if (typeof evidencedAt !== "number") return false;
    const age = this.deps.now() - evidencedAt;
    return age >= 0 && age <= AUTH_EVIDENCE_TTL_MS;
  }

  /** A committed probe says this origin is no longer authenticated, so its
   * evidence must stop authorizing releases immediately rather than idling
   * out over the TTL. Wired to keepalive's onOriginAuthenticationChanged,
   * which fires only on a real committed change — never on a preserved
   * verdict, so a closed tab or an unreadable page cannot revoke. */
  async revokeAuthEvidence(origin: string): Promise<void> {
    await this.ready;
    if (this.store.authEvidenceByOrigin?.[origin] === undefined) return;
    await this.update((s) => {
      const authEvidenceByOrigin = { ...s.authEvidenceByOrigin };
      delete authEvidenceByOrigin[origin];
      return { ...s, authEvidenceByOrigin };
    });
  }

  /** Keeps an OA landing from opening an institutional queue while preserving
   * the existing one-visible-tab flow for ordinary offers. `origin` may be
   * undefined for a job whose offer URL never resolved to a bare HTTPS
   * origin; such a job can still qualify through the OA branch. */
  private hasHandoffReleaseEvidence(origin: string | undefined, requiresAuth: boolean | undefined): boolean {
    return (origin !== undefined && this.hasAuthEvidence(origin)) || (requiresAuth !== true && this.openAccessLandingObserved);
  }
  /** A provider's declared host set is the only local grouping information the
   * bridge has. Canonicalizing it makes every re-offer use the same lease
   * without retaining a resolver or provider URL. */
  private providerKeyForHosts(providerHosts: string[]): string {
    const hosts = [...new Set(providerHosts.map((host) => host.trim().toLowerCase()).filter(Boolean))].sort();
    return hosts.length === 0 ? "unknown-provider" : hosts.join(",");
  }

  private providerKeyForJob(job: ActiveJob): string {
    return this.providerKeyForHosts(job.provider_hosts);
  }
  private challengeHostsFor(providerHosts: readonly string[]): string[] {
    return [
      ...new Set(
        providerHosts
          .map((host) => registrableProviderHost(host))
          .filter((host): host is string => host !== undefined),
      ),
    ];
  }

  private challengeCooldownActiveForHosts(providerHosts: readonly string[]): boolean {
    const now = this.deps.now();
    return this.challengeHostsFor(providerHosts).some((host) => {
      const expiresAt = this.store.challengeCooldowns?.[host];
      return typeof expiresAt === "number" && Number.isFinite(expiresAt) && expiresAt > now;
    });
  }

  private async expireChallengeCooldowns(): Promise<string[]> {
    const now = this.deps.now();
    const expired = Object.entries(this.store.challengeCooldowns ?? {})
      .filter(([host, expiresAt]) => registrableProviderHost(host) !== host || !Number.isFinite(expiresAt) || expiresAt <= now)
      .map(([host]) => host);
    if (expired.length === 0) return expired;
    await this.update((store) => {
      const challengeCooldowns = { ...(store.challengeCooldowns ?? {}) };
      for (const host of expired) delete challengeCooldowns[host];
      return { ...store, challengeCooldowns };
    });
    for (const host of expired) this.challengeCooldownTimers.delete(host);
    return expired;
  }

  private scheduleChallengeCooldownExpiry(host: string, expiresAt: number): void {
    const token = {};
    this.challengeCooldownTimers.set(host, token);
    this.deps.setTimeout(async () => {
      if (this.challengeCooldownTimers.get(host) !== token) return;
      await this.ready;
      const expired = await this.expireChallengeCooldowns();
      if (!expired.includes(host)) return;
      await this.releaseQueuedHandoffs();
      await this.syncConnectionBadge();
    }, Math.max(0, expiresAt - this.deps.now()));
  }

  private async restoreChallengeCooldownTimers(): Promise<void> {
    await this.expireChallengeCooldowns();
    for (const [host, expiresAt] of Object.entries(this.store.challengeCooldowns ?? {})) {
      if (registrableProviderHost(host) === host && expiresAt > this.deps.now()) {
        this.scheduleChallengeCooldownExpiry(host, expiresAt);
      }
    }
  }
  private challengeHostFor(job: ActiveJob, currentHost: string, currentURL?: string): string | undefined {
    const declaredHosts = this.challengeHostsFor(job.provider_hosts);
    if (currentURL !== undefined && isAuthenticationURL(currentURL)) return declaredHosts[0];
    const current = registrableProviderHost(currentHost);
    if (current === undefined) return declaredHosts[0];
    if (
      hostMatches(currentHost, job.provider_hosts) ||
      this.deps.adapterSpecs.some((spec) => hostMatches(currentHost, spec.hosts))
    ) {
      return current;
    }
    return declaredHosts[0];
  }

  private async blockChallenge(
    job: ActiveJob,
    currentHost: string,
    kind: ChallengeBlockKind,
    currentURL?: string,
  ): Promise<void> {
    const providerHost = this.challengeHostFor(job, currentHost, currentURL);
    if (providerHost === undefined) return;
    const now = this.deps.now();
    const alreadyBlocked =
      job.challenge_blocked === true &&
      job.challenge_host === providerHost &&
      job.challenge_kind === kind;
    const expiresAt = now + CHALLENGE_COOLDOWN_MS;
    await this.update((store) => ({
      ...patchJob(store, job.job_id, {
        challenge_blocked: true,
        challenge_host: providerHost,
        challenge_kind: kind,
        challenge_blocked_at: now,
        unknown_count: 0,
      }),
      challengeCooldowns: {
        ...(store.challengeCooldowns ?? {}),
        [providerHost]: expiresAt,
      },
    }));
    this.scheduleChallengeCooldownExpiry(providerHost, expiresAt);
    const outcomeKey = `${job.job_id}:challenge_blocked`;
    if (!alreadyBlocked && !this.challengeBlockedOutcomeSent.has(outcomeKey)) {
      if (
        this.send(
          "error",
          {
            code: "challenge_blocked",
            message: "Provider security check or redirect loop needs human attention",
          },
          job.job_id,
        )
      ) {
        this.challengeBlockedOutcomeSent.add(outcomeKey);
      }
    }
    if (!alreadyBlocked) await this.parkProviderDrain(job);
    await this.parkHandoffForManual(job.job_id);
    await this.syncConnectionBadge();
  }
  private async clearChallengeBlock(job: ActiveJob): Promise<boolean> {
    if (job.challenge_blocked !== true) return false;
    const providerHost = job.challenge_host;
    await this.update((store) => {
      const activeJobs = store.activeJobs.map((candidate) => {
        if (candidate.job_id !== job.job_id) return candidate;
        const next = { ...candidate };
        delete next.challenge_blocked;
        delete next.challenge_host;
        delete next.challenge_kind;
        delete next.challenge_blocked_at;
        return next;
      });
      const challengeCooldowns = { ...(store.challengeCooldowns ?? {}) };
      if (providerHost !== undefined) delete challengeCooldowns[providerHost];
      return { ...store, activeJobs, challengeCooldowns };
    });
    if (providerHost !== undefined) this.challengeCooldownTimers.delete(providerHost);
    this.challengeBlockedOutcomeSent.delete(`${job.job_id}:challenge_blocked`);
    await this.clearProviderDrainPark(this.providerKeyForJob(job));
    const resumed = await this.resumeHandoffAfterManual(job.job_id);
    await this.releaseQueuedHandoffs();
    await this.syncConnectionBadge();
    return resumed;
  }



  private currentProviderDrainLease(providerKey: string): ProviderDrainLease | undefined {
    const lease = this.store.providerDrainLeases?.[providerKey];
    if (
      lease === undefined ||
      lease.providerKey !== providerKey ||
      !Number.isFinite(lease.expiresAt) ||
      lease.expiresAt <= this.deps.now()
    ) {
      return undefined;
    }
    return lease;
  }

  private hasActiveProviderDrainLease(job: ActiveJob): boolean {
    return this.currentProviderDrainLease(this.providerKeyForJob(job)) !== undefined;
  }

  private isProviderDrainParked(job: ActiveJob): boolean {
    return this.currentProviderDrainLease(this.providerKeyForJob(job))?.parkedReason === "challenge";
  }

  /** Discard stale or malformed persisted leases before a drain chooses work.
   * The owner is intentionally absent from session storage, so an unexpired
   * lease from a prior service worker is respected until this bounded expiry. */
  private async expireProviderDrainLeases(): Promise<string[]> {
    const now = this.deps.now();
    const expired = Object.entries(this.store.providerDrainLeases ?? {})
      .filter(
        ([providerKey, lease]) =>
          lease.providerKey !== providerKey || !Number.isFinite(lease.expiresAt) || lease.expiresAt <= now,
      )
      .map(([providerKey]) => providerKey);
    if (expired.length === 0) return expired;
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      for (const providerKey of expired) delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    for (const providerKey of expired) {
      this.providerDrainLeaseOwners.delete(providerKey);
      this.providerDrainLeaseTimers.delete(providerKey);
    }
    return expired;
  }

  private scheduleProviderDrainLeaseExpiry(providerKey: string, expiresAt: number): void {
    const token = {};
    this.providerDrainLeaseTimers.set(providerKey, token);
    this.deps.setTimeout(async () => {
      if (this.providerDrainLeaseTimers.get(providerKey) !== token) return;
      this.providerDrainLeaseTimers.delete(providerKey);
      await this.ready;
      const expired = await this.expireProviderDrainLeases();
      if (!expired.includes(providerKey)) return;
      await this.acknowledgePendingProviderHandoffs(providerKey);
      await this.releaseQueuedHandoffs();
    }, Math.max(0, expiresAt - this.deps.now()));
  }

  private async restoreProviderDrainLeaseTimers(): Promise<void> {
    await this.expireProviderDrainLeases();
    for (const [providerKey, lease] of Object.entries(this.store.providerDrainLeases ?? {})) {
      if (this.currentProviderDrainLease(providerKey) !== undefined) {
        this.scheduleProviderDrainLeaseExpiry(providerKey, lease.expiresAt);
      }
    }
  }

  /** Claim one provider while opening its next queued tab. A live claim from
   * this or a prior worker blocks the candidate; callers continue with another
   * provider rather than starting a second drain. */
  private async claimProviderDrainLease(job: ActiveJob): Promise<string | undefined> {
    await this.expireProviderDrainLeases();
    const providerKey = this.providerKeyForJob(job);
    if (this.currentProviderDrainLease(providerKey) !== undefined) return undefined;
    const owner = this.deps.randomUUID();
    const expiresAt = this.deps.now() + PROVIDER_DRAIN_LEASE_MS;
    let claimed = false;
    await this.update((store) => {
      const current = store.providerDrainLeases?.[providerKey];
      if (
        current !== undefined &&
        current.providerKey === providerKey &&
        Number.isFinite(current.expiresAt) &&
        current.expiresAt > this.deps.now()
      ) {
        return store;
      }
      claimed = true;
      return {
        ...store,
        providerDrainLeases: {
          ...(store.providerDrainLeases ?? {}),
          [providerKey]: { providerKey, expiresAt },
        },
      };
    });
    if (!claimed) return undefined;
    this.providerDrainLeaseOwners.set(providerKey, owner);
    this.scheduleProviderDrainLeaseExpiry(providerKey, expiresAt);
    return owner;
  }

  private async releaseProviderDrainLease(providerKey: string, owner: string): Promise<void> {
    if (this.providerDrainLeaseOwners.get(providerKey) !== owner) return;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const lease = store.providerDrainLeases?.[providerKey];
      if (lease === undefined || lease.parkedReason !== undefined) return store;
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
  }

  /** A challenge remains human-visible in its existing tab. Its siblings stay
   * queued and every new re-offer remains unaccepted until the lease clears. */
  private async parkProviderDrain(job: ActiveJob): Promise<void> {
    const providerKey = this.providerKeyForJob(job);
    const owner = this.deps.randomUUID();
    const expiresAt = this.deps.now() + PROVIDER_DRAIN_LEASE_MS;
    this.providerDrainLeaseOwners.set(providerKey, owner);
    await this.update((store) => ({
      ...store,
      providerDrainLeases: {
        ...(store.providerDrainLeases ?? {}),
        [providerKey]: { providerKey, expiresAt, parkedReason: "challenge" },
      },
    }));
    this.scheduleProviderDrainLeaseExpiry(providerKey, expiresAt);
  }
  /** A non-challenge provider document proves this drain can advance. The next
   * queued sibling may claim a fresh lease; a challenge instead replaces it
   * with the parked form above. */
  private async completeProviderDrainLease(providerKey: string): Promise<boolean> {
    if (this.currentProviderDrainLease(providerKey) === undefined) return false;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    return true;
  }


  /** The only explicit resume is an existing human handoff-open request; a
   * cleared challenge also calls this when its provider document returns. */
  private async clearProviderDrainPark(providerKey: string): Promise<boolean> {
    const lease = this.currentProviderDrainLease(providerKey);
    if (lease?.parkedReason !== "challenge") return false;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    return true;
  }

  private async acknowledgePendingProviderHandoffs(providerKey: string): Promise<boolean> {
    const pending = this.store.activeJobs.filter(
      (job) => this.providerKeyForJob(job) === providerKey && job.handoffAckPending === true,
    );
    for (const job of pending) {
      if (!this.send("job_accept", {}, job.job_id)) return false;
    }
    if (pending.length === 0) return true;
    const acknowledged = new Set(pending.map((job) => job.job_id));
    await this.update((store) => ({
      ...store,
      activeJobs: store.activeJobs.map((job) => {
        if (!acknowledged.has(job.job_id)) return job;
        const { handoffAckPending: _handoffAckPending, ...resumed } = job;
        return resumed;
      }),
    }));
    return true;
  }

  private async releaseProviderDrainWhenUnused(providerKey: string): Promise<void> {
    if (this.store.activeJobs.some((job) => this.providerKeyForJob(job) === providerKey)) return;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      if (store.providerDrainLeases?.[providerKey] === undefined) return store;
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
  }

  /** A warm provider or resolver landing proves this job's institution has a
   * session; an unrelated completed page must not unlock its queued peers. */
  private isInstitutionalSessionLanding(job: ActiveJob, rawURL: string): boolean {
    if (job.requires_auth !== true || isAuthenticationURL(rawURL)) return false;
    const offered = this.offerURLs.get(job.job_id);
    if (offered === undefined) return false;
    try {
      const landing = new URL(rawURL);
      const offer = new URL(offered);
      return (
        landing.origin === offer.origin ||
        hostMatches(landing.hostname, job.provider_hosts) ||
        this.deps.adapterSpecs.some((adapter) => hostMatches(landing.hostname, adapter.hosts))
      );
    } catch {
      return false;
    }
  }

  /** Merge a fresh release-grade observation for `origin` into the persisted
   * evidence map, pruning any entry (including this same origin's prior
   * value) that has already aged past AUTH_EVIDENCE_TTL_MS. Keeps
   * store.authEvidenceByOrigin bounded across a long-lived profile instead
   * of accumulating one entry per resolver ever seen. */
  private withAuthEvidence(store: StoreShape, origin: string, now: number): Record<string, number> {
    const merged: Record<string, number> = {};
    for (const [existingOrigin, at] of Object.entries(store.authEvidenceByOrigin ?? {})) {
      const age = now - at;
      if (age >= 0 && age <= AUTH_EVIDENCE_TTL_MS) merged[existingOrigin] = at;
    }
    merged[origin] = now;
    return merged;
  }

  /** A landing is a reason to look again — ADR-0013 says a timing frame is
   * not identity evidence, so this never asserts an in/out session verdict.
   * But when papio itself drove the tab past authentication onto a page
   * resolving to a configured origin, that IS first-hand evidence for THAT
   * origin: it persists per-origin release evidence (surviving the MV3
   * restarts that wipe everything worker-local), releases only that origin's own
   * queued handoffs, and reloads only that origin's own stalled auth tabs —
   * reloadAuthenticationHandoffs still skips any job already reported to the
   * operator as authStalledReported. It never touches another origin's tabs
   * or queue. An offer origin outside the daemon's current configured set
   * fails closed (do nothing): this remains a best-effort, narrowly-scoped
   * release, never a source of truth beyond its own origin.
   *
   * Also resumes this origin's waiting_for_session siblings, UNCONDITIONALLY
   * — not gated behind firstAuthEvidence/warm-evidence like the keepalive
   * probe nudge above. THIS job finishing its own sign-in is the clearest
   * possible proof the shared session is real, regardless of whether some
   * other evidence for this origin already existed; gating it the same way
   * would silently drop exactly the case that motivated this whole feature —
   * a still-warm-looking origin whose real IdP session had actually expired,
   * so evidence never re-lands and only THIS landing ever proves it. */
  private async recordInstitutionalSession(job: ActiveJob, rawURL: string, now: number): Promise<boolean> {
    if (!this.isInstitutionalSessionLanding(job, rawURL)) return false;
    const origin = this.jobInstitutionOrigin(job);
    if (origin === undefined) return false;
    const firstAuthEvidence = !this.hasAuthEvidence(origin);
    await this.update((s) => ({
      ...s,
      lastAuthReturnedAt: now,
      authEvidenceByOrigin: this.withAuthEvidence(s, origin, now),
    }));
    if (firstAuthEvidence) {
      void this.keepaliveManager?.markDirty(origin);
      // Not probeForeground: this fires from a tab NAVIGATION, an automatic
      // path, not an operator action, so it must not take the 2s operator
      // floor (MIN_FOREGROUND_PROBE_SPACING_MS in keepalive.ts) — the
      // bounded rate this landing path relies on assumes the 10s automatic
      // floor instead. markDirty stays regardless as the wake-sweep fallback
      // if this probe never lands.
      void this.keepaliveManager?.probeOriginAutomatically(origin);
    }
    await this.drainQueuedHandoffs(origin, undefined, false);
    await this.reloadAuthenticationHandoffs(origin);
    await this.resumeWaitingForSessionHandoffs(origin);
    return true;
  }

  /** OA completions retain the ordinary queue flow without becoming evidence
   * that it is safe to reload or open an institutional sign-in. */
  private async recordOpenAccessLanding(job: ActiveJob): Promise<void> {
    if (job.requires_auth === true) return;
    const firstOpenAccessLanding = !this.openAccessLandingObserved;
    this.openAccessLandingObserved = true;
    await this.releaseQueuedHandoffs();
    if (firstOpenAccessLanding) await this.reloadAuthenticationHandoffs(undefined, false);
  }

  // A popup delivery's route is deliberately never classified "oa" here,
  // even when job.requires_auth === false would say the job is OA-routed.
  // Session evidence and OA classification are orthogonal: an operator can
  // send an OA-routed PDF while an unrelated institutional session is warm
  // elsewhere in the same browser, and frozen evidence (below) correctly
  // reports "warm" for that case. The daemon's wire validator rejects any
  // frame with route "oa" and session_evidence not "none" (BrowserAccessBasis),
  // and a rejected delivery_context is a fatal decode that tears down the
  // whole native-messaging session (AGENTS.md) — so claiming "oa" here would
  // turn a merely conservative access_basis into a session-ending crash the
  // moment evidence is honestly "warm". Staying on "direct" always decodes,
  // and "direct" + "none" already resolves to the conservative "manual"
  // basis (never "institutional", never an unverified "open_access") — the
  // same fallback BrowserAccessBasis documents for missing/incomplete
  // context.
  private deliveryRouteFor(job: ActiveJob, track: DownloadTrack): DeliveryRoute {
    if (track.delivery === true) return "direct";
    if (track.route !== undefined) return track.route;
    if (this.resolverRoutes.has(job.job_id)) return "resolver";
    return job.requires_auth === false ? "oa" : "direct";
  }

  /** Live session-evidence read, scoped to this job's own configured
   * resolver origin — never the global fallback (that was the leak: any
   * job's delivery could be credited by an unrelated origin's evidence). */
  private currentSessionEvidence(job: ActiveJob): DeliverySessionEvidence {
    const perJob = this.deliverySessionEvidence.get(job.job_id);
    if (perJob !== undefined) return perJob;
    const origin = this.resolverOriginHint(this.offerURLs.get(job.job_id));
    if (origin === undefined) return "none";
    // Tiered off the one persisted source: inside the TTL the session is
    // freshly evidenced, an expired-but-present entry is merely warm, and a
    // revoked or never-seen origin has nothing.
    const lastAuth = this.store.authEvidenceByOrigin?.[origin];
    if (typeof lastAuth !== "number") return "none";
    const age = this.deps.now() - lastAuth;
    return age >= 0 && age <= AUTH_EVIDENCE_TTL_MS ? "fresh_auth" : "warm";
  }

  private deliveryEvidenceFor(job: ActiveJob, track: DownloadTrack, route: DeliveryRoute): DeliverySessionEvidence {
    if (route === "oa") return "none";
    // Mirrors deliveryPageHost's frozen-host guard below: a popup delivery's
    // session evidence is captured once, in startPDFDelivery, at request
    // time. The download can take seconds with the tab still interactive, so
    // store.authEvidenceByOrigin is live per-origin state that can flip true
    // mid-download — reading it here would credit a public-page delivery with
    // an institutional probe or sign-in that happened to land elsewhere in
    // the browser while the bytes were still in flight.
    if (track.delivery === true) {
      const frozen = this.store.pendingDelivery;
      if (frozen?.job_id === job.job_id && frozen.status !== "failed" && frozen.session_evidence !== undefined) {
        return frozen.session_evidence;
      }
    }
    if (track.sessionEvidence !== undefined) return track.sessionEvidence;
    return this.currentSessionEvidence(job);
  }

  private async deliveryPageHost(
    owner: ActiveJob,
    item: DownloadItemLike,
    track: DownloadTrack,
  ): Promise<string | undefined> {
    // Provenance must name the page these bytes came from. For a popup
    // delivery that host was frozen when the download was requested: the
    // download can take seconds, the tab remains interactive throughout, and
    // re-reading it here would label the candidate with whatever the operator
    // navigated to in the meantime.
    //
    // The frozen host is only valid for the delivery download it was captured
    // for. A failed delivery (status: "failed") leaves page_host intact in
    // pendingDelivery, and clearPendingDelivery only runs on job
    // completion/removal — so a stale frozen host can poison a later
    // non-delivery download for the same job (sequence A: failed delivery
    // followed by a resolver-routed download from a different host).
    // Non-delivery downloads (handoff, directOffer) must never inherit a
    // delivery's frozen host (sequence B).
    const frozen = this.store.pendingDelivery;
    if (
      track.delivery === true &&
      frozen?.job_id === owner.job_id &&
      frozen.status !== "failed" &&
      frozen.page_host !== undefined
    ) {
      return frozen.page_host;
    }
    const tabID = typeof item.tabId === "number" && item.tabId >= 0 ? item.tabId : owner.tab_id;
    if (tabID < 0) return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      if (typeof tab.url !== "string") return undefined;
      return sanitizePageHost(tab.url);
    } catch {
      return undefined;
    }
  }

  /** Waiting briefly avoids an unattended SAML exchange; a bounded fallback
   * prevents that safety check from parking a cold handoff forever. */
  private scheduleQueuedHandoffRelease(jobID: string): void {
    const job = findByJob(this.store, jobID);
    if (job === undefined || job.status !== "queued") {
      this.queuedHandoffTimers.delete(jobID);
      this.pendingForcedReleases.delete(jobID);
      return;
    }
    if (this.queuedHandoffTimers.has(jobID)) return;
    const token = {};
    this.queuedHandoffTimers.set(jobID, token);
    const delay = Math.max(0, job.offered_at + QUEUED_HANDOFF_RELEASE_MS - this.deps.now());
    this.deps.setTimeout(async () => {
      if (this.queuedHandoffTimers.get(jobID) !== token) return;
      this.queuedHandoffTimers.delete(jobID);
      await this.ready;
      await this.releaseQueuedHandoffs(jobID);
    }, delay);
  }

  /** MV3 timers die with their worker. The periodic wake checks durable offer
   * times so a cold queue cannot restart its fallback window forever on sleep. */
  private async releaseExpiredQueuedHandoffs(): Promise<void> {
    const deadline = this.deps.now() - QUEUED_HANDOFF_RELEASE_MS;
    const dueJobIDs = this.store.activeJobs
      .filter((job) => job.status === "queued" && job.offered_at <= deadline)
      .map((job) => job.job_id);
    for (const jobID of dueJobIDs) await this.releaseQueuedHandoffs(jobID);
  }

  /** Startup has no worker-local timer state. A tracked tab already settled
   * away from an IdP is the same usable-session evidence as a warm landing. */
  private async releaseQueuedHandoffsForLiveLanding(): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (job.tab_id < 0 || job.status === "queued") continue;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        const institutionalSession =
          typeof tab.url === "string" &&
          (await this.recordInstitutionalSession(job, tab.url, this.deps.now()));
        if (institutionalSession) return;
        if (typeof tab.url === "string" && !isAuthenticationURL(tab.url)) {
          await this.recordOpenAccessLanding(job);
          return;
        }
      } catch {
        // A closed tab is handled by the normal tab-removal path.
      }
    }
  }

  /** origin_hint is authoritative on the daemon: an absent hint scopes the
   * release to the default profile (safe), but a hint that resolves to the
   * WRONG institution is indistinguishable from a correct one and releases
   * that institution's parked handoffs without its session being verified
   * (papio-7d7a0ae96ca5726e). So this only ever forwards the origin the
   * caller actually observed for THIS evidence — never a keepalive
   * snapshot's resolver (which itself degrades to an arbitrary granted
   * host) or the most recent offer's origin, which need not be the origin
   * that produced this evidence at all. */
  emitSessionEvidence(evidence: "warm_verified" | "auth_returned", originHint?: string): boolean {
    const now = this.deps.now();
    const throttleKey = originHint ?? "";
    const sentAt = this.sessionEvidenceSentAt.get(throttleKey);
    if (sentAt !== undefined) {
      const age = now - sentAt;
      if (age >= 0 && age < SESSION_EVIDENCE_THROTTLE_MS) return false;
    }
    if (!(this.store.daemonFeatures ?? []).includes(SESSION_EVIDENCE_FEATURE)) return false;
    const payload: Record<string, unknown> = {
      evidence,
      at: new Date(now).toISOString(),
    };
    if (isBareHTTPSOrigin(originHint)) payload.origin_hint = originHint;
    if (!this.send("session_evidence", payload)) return false;
    this.sessionEvidenceSentAt.set(throttleKey, now);
    return true;
  }

  /** Fires from keepalive's onFreshSessionEvidence — the ONLY callback that
   * may authorize work from a decisive probe verdict (see keepalive.ts's
   * KeepaliveOptions doc). Marks `origin` release-grade for this worker life
   * AND persists that evidence (withAuthEvidence) so it survives an MV3
   * restart, unblocks ONLY that origin's queue and stuck tabs, and forwards
   * the timing fact to the daemon. Never resets an authentication-attempt
   * budget, never reopens an auth-stalled human action, never touches
   * another origin's tabs — ADR-0009's autonomous-retry line, held by
   * drainQueuedHandoffs'/reloadAuthenticationHandoffs's own origin scoping
   * below. drainQueuedHandoffs is called directly with an exact origin
   * (never through releaseQueuedHandoffs, whose fallback-driven callers
   * below always pass no origin); recordInstitutionalSession's warm-landing
   * path does the same for its own, narrower kind of evidence.
   *
   * Also resumes every waiting_for_session sibling for this SAME institution
   * (resumeWaitingForSessionHandoffs) — a park caused by anything else
   * (challenge, manual download) is untouched: the helper looks at nothing
   * but the waiting_for_session marker and the job's own offer origin, so
   * ADR-0009's autonomous-retry line holds here too. Deliberately does NOT
   * retire any federated-login registry claim: evidence is proof the
   * institution's session is warm, never proof THIS SPECIFIC claim's owner
   * tab actually finished — an owner still genuinely mid-redirect on the IdP
   * survives even its own institution's evidence, so a resumed waiter's
   * re-classify can only park behind it again, never open a second tab at
   * the same login page. Idempotent under duplicate/repeated evidence: once
   * a waiter is enqueued it clears waiting_for_session (clearParkedMarker),
   * so a later call's scan simply finds nothing left to resume. */
  async recordFreshSessionEvidence(evidence: FreshSessionEvidence): Promise<void> {
    const { origin } = evidence;
    await this.ready;
    const now = this.deps.now();
    await this.update((s) => ({
      ...s,
      lastAuthReturnedAt: now,
      authEvidenceByOrigin: this.withAuthEvidence(s, origin, now),
    }));
    this.emitSessionEvidence("warm_verified", origin);
    await this.drainQueuedHandoffs(origin, undefined, false);
    await this.reloadAuthenticationHandoffs(origin);
    await this.resumeWaitingForSessionHandoffs(origin);
  }

  /** Bypasses evidence for exactly one forced job — the 45s
   * QUEUED_HANDOFF_RELEASE_MS fallback timer, releaseExpiredQueuedHandoffs,
   * operator actions, and provider-lease/challenge-cooldown expiry, all
   * already-ratified ADR-0009 autonomous-retry behaviour. With no
   * fallbackJobID this is a pure opportunistic re-drain: every queued job is
   * still admitted only through its OWN origin's release-grade evidence
   * (hasHandoffReleaseEvidence), so this can never launder one origin's
   * evidence into another's queue. */
  private async releaseQueuedHandoffs(fallbackJobID?: string, forceProvider = false): Promise<void> {
    await this.drainQueuedHandoffs(undefined, fallbackJobID, forceProvider);
  }

  private async drainQueuedHandoffs(
    originScope: string | undefined,
    fallbackJobID: string | undefined,
    forceProvider: boolean,
  ): Promise<void> {
    if (fallbackJobID !== undefined) this.pendingForcedReleases.add(fallbackJobID);
    if (forceProvider && fallbackJobID !== undefined) {
      const forced = findByJob(this.store, fallbackJobID);
      if (forced !== undefined) await this.clearProviderDrainPark(this.providerKeyForJob(forced));
    }
    const jobOrigin = (job: ActiveJob): string | undefined =>
      this.resolverOriginHint(this.offerURLs.get(job.job_id));
    const matchesOrigin =
      originScope === undefined ? (_job: ActiveJob) => true : (job: ActiveJob) => jobOrigin(job) === originScope;
    await this.drainHandoffDriveQueue();
    const anyQueuedEligible = this.store.activeJobs.some(
      (job) =>
        matchesOrigin(job) &&
        job.status === "queued" &&
        this.hasHandoffReleaseEvidence(jobOrigin(job), job.requires_auth),
    );
    if (!anyQueuedEligible && this.pendingForcedReleases.size === 0) {
      return;
    }
    if (this.drainingQueuedHandoffs) {
      await new Promise<void>((resolve) => this.queuedHandoffDrainWaiters.add(resolve));
      return;
    }
    this.drainingQueuedHandoffs = true;
    try {
      await this.expireProviderDrainLeases();
      await this.expireChallengeCooldowns();
      // One loop opens at most one unclassified handoff per provider. A lease
      // stays with that tab until it proves normal, becomes a challenge park,
      while (this.handoffDrives.size < HANDOFF_DRIVE_LIMIT) {
        let selected = this.store.activeJobs.find(
          (job) =>
            matchesOrigin(job) &&
            job.status === "queued" &&
            this.hasHandoffReleaseEvidence(jobOrigin(job), job.requires_auth) &&
            !this.challengeCooldownActiveForHosts(job.provider_hosts) &&
            !this.hasActiveProviderDrainLease(job),
        );
        let forcedJobID: string | undefined;
        if (selected === undefined) {
          for (const jobID of this.pendingForcedReleases) {
            const candidate = this.store.activeJobs.find(
              (job) => matchesOrigin(job) && job.job_id === jobID && job.status === "queued",
            );
            if (candidate === undefined) {
              this.pendingForcedReleases.delete(jobID);
              continue;
            }
            if (this.challengeCooldownActiveForHosts(candidate.provider_hosts)) continue;
            if (this.hasActiveProviderDrainLease(candidate)) continue;
            selected = candidate;
            forcedJobID = jobID;
            this.pendingForcedReleases.delete(jobID);
            break;
          }
        }
        if (selected === undefined) return;

        const queued = selected;
        const providerKey = this.providerKeyForJob(queued);
        const owner = await this.claimProviderDrainLease(queued);
        if (owner === undefined) continue;
        let opened = false;
        try {
          const forceSurface = forcedJobID === queued.job_id && queued.requires_auth === true;
          this.queuedHandoffTimers.delete(queued.job_id);
          const url = this.offerURLs.get(queued.job_id);
          if (url === undefined) {
            this.send("job_reject", {}, queued.job_id);
            await this.removeJobWithOffer(queued.job_id);
            continue;
          }
          if (!(await this.acknowledgePendingProviderHandoffs(providerKey))) return;
          let tabID: number | undefined;
          try {
            tabID = await this.openManagedTab({
              url,
              jobId: queued.job_id,
              purpose: "handoff",
              surfaceFallback: forceSurface,
            });
          } catch (e) {
            console.error("papio: queued handoff tab creation failed", e);
          }
          if (tabID === undefined) {
            this.send("job_reject", {}, queued.job_id);
            await this.removeJobWithOffer(queued.job_id);
            continue;
          }
          this.beginProviderDrive(queued.job_id);
          await this.update((s) =>
            patchJob(s, queued.job_id, {
              tab_id: tabID,
              status: "accepted",
              download_initiated: false,
              unknown_count: 0,
            }),
          );
          this.registerHandoffDrive(queued.job_id, tabID);
          if (queued.requires_auth === true) {
            this.authUnblockedCount += 1;
            this.authUnblockedAt = this.deps.now();
          }
          opened = true;
          if (forceSurface) await this.surfaceWorkTab(tabID);
        } finally {
          if (!opened) await this.releaseProviderDrainLease(providerKey, owner);
        }
      }
    } finally {
      this.drainingQueuedHandoffs = false;
      for (const resolve of this.queuedHandoffDrainWaiters) resolve();
      this.queuedHandoffDrainWaiters.clear();
    }
  }

  private async reloadAuthenticationHandoffs(origin: string | undefined, includeInstitutional = true): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (
        job.tab_id < 0 ||
        job.status === "queued" ||
        (!includeInstitutional && job.requires_auth === true) ||
        (origin !== undefined && this.resolverOriginHint(this.offerURLs.get(job.job_id)) !== origin) ||
        this.authStalledReported.has(job.job_id)
      ) {
        continue;
      }
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        if (typeof tab.url === "string" && isAuthenticationURL(tab.url)) {
          await this.deps.tabs.reload(job.tab_id);
        }
      } catch {
        // A closed handoff is handled by the normal tab-removal path.
      }
    }
  }

  public sendPageCapture(payload: PageCapturePayload, jobID?: string): boolean {
    return this.pageCaptureAvailable() && this.send(MsgPageCapture, payload, jobID);
  }

  private waitForPageCaptureLoad(tabID: number): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      let finished = false;
      const finish = (loaded: boolean): void => {
        if (finished) return;
        finished = true;
        if (this.pageCaptureLoadWaiters.get(tabID) === finish) {
          this.pageCaptureLoadWaiters.delete(tabID);
        }
        resolve(loaded);
      };
      this.pageCaptureLoadWaiters.set(tabID, finish);
      this.deps.setTimeout(() => finish(false), PAGE_CAPTURE_NAV_TIMEOUT_MS);
    });
  }

  private async onPageCaptureRequest(msg: BrowserMessage): Promise<void> {
    const request = msg.payload as unknown as PageCaptureRequestPayload;
    const reply = (outcome: PageCaptureRequestResultPayload["outcome"], detail?: string): void => {
      const payload: PageCaptureRequestResultPayload = {
        request_id: request.request_id,
        outcome,
        ...(detail === undefined ? {} : { detail }),
      };
      this.send(MsgPageCaptureRequestResult, payload);
    };
    if (
      !this.pageCaptureAvailable() ||
      !(this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_REQUEST_FEATURE)
    ) {
      reply("not_permitted", "page capture is not available");
      return;
    }
    let requested: URL;
    try {
      requested = new URL(request.url);
    } catch {
      reply("nav_failed", "the requested URL is invalid");
      return;
    }
    try {
      if (!(await this.deps.permissions.contains({ origins: [`${requested.origin}/*`] }))) {
        reply("not_permitted", "provider host permission is not granted");
        return;
      }
    } catch {
      reply("not_permitted", "provider host permission could not be checked");
      return;
    }
    if (this.pageCaptureDriving || this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
      reply("busy", "browser handoff slots are occupied");
      return;
    }

    this.pageCaptureDriving = true;
    const managedKey = `capture_${request.request_id}`;
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: request.url,
        jobId: managedKey,
        purpose: "capture",
        surfaceFallback: true,
        focusExisting: false,
      });
      if (tabID === undefined) {
        reply("nav_failed", "could not open the provider page");
        return;
      }
      // An explicit fixture-capture command may need a visible tab: heavy SPAs
      // and consent managers routinely stop rendering in the minimized work
      // window. The command itself is the operator's request to surface it.
      await this.surfaceWorkTab(tabID);
      const load = this.waitForPageCaptureLoad(tabID);
      try {
        if ((await this.deps.tabs.get(tabID)).status === "complete") {
          this.pageCaptureLoadWaiters.get(tabID)?.(true);
        }
      } catch {
        this.pageCaptureLoadWaiters.get(tabID)?.(false);
      }
      if (!(await load)) {
        reply("timeout", "provider page did not finish loading");
        return;
      }
      const settleMS = request.settle_ms ?? PAGE_CAPTURE_DEFAULT_SETTLE_MS;
      if (settleMS > 0) {
        await new Promise<void>((resolve) => this.deps.setTimeout(resolve, settleMS));
      }
      let injected: { result?: unknown } | undefined;
      try {
        [injected] = await this.deps.scripting.executeScript({
          target: { tabId: tabID },
          func: capturePage,
        });
      } catch {
        reply("not_permitted", "could not read the provider page");
        return;
      }
      const page = injected?.result as PageCapture | undefined;
      if (
        page === undefined ||
        typeof page.html !== "string" ||
        typeof page.origin !== "string" ||
        typeof page.path !== "string"
      ) {
        reply("nav_failed", "provider page capture returned no document");
        return;
      }
      let finalOrigin: URL;
      try {
        finalOrigin = new URL(page.origin);
      } catch {
        reply("nav_failed", "provider page has an invalid origin");
        return;
      }
      if (finalOrigin.protocol !== "https:" || finalOrigin.hostname === "") {
        reply("nav_failed", "provider page did not finish on https");
        return;
      }
      try {
        if (!(await this.deps.permissions.contains({ origins: [`${finalOrigin.origin}/*`] }))) {
          reply("not_permitted", "final provider host permission is not granted");
          return;
        }
      } catch {
        reply("not_permitted", "final provider host permission could not be checked");
        return;
      }
      const sanitized = sanitizeFixture(page.html, {
        provider: request.provider as Provider,
        scenario: request.scenario as Scenario,
        originNoQuery: `${page.origin}${page.path}`,
        capturedISO: new Date(this.deps.now()).toISOString(),
      });
      const leak = residualLeak(sanitized);
      if (leak !== null) {
        reply("nav_failed", "sanitized page did not pass the privacy check");
        return;
      }
      const encoded = await encodePageCapture(sanitized, {
        host: finalOrigin.hostname,
        scenario: request.scenario,
        adapterID: request.provider,
        // Binds this content frame to the request that asked for it. The
        // daemon used to match provider+scenario alone, so an unsolicited
        // capture of the same pair could satisfy this pending request and
        // hand its caller the other capture's file path
        // (papio-85a7420f4cd2564f).
        requestID: request.request_id,
      });
      if (!encoded.ok) {
        reply("nav_failed", encoded.error);
        return;
      }
      if (!this.sendPageCapture(encoded.payload)) {
        reply("nav_failed", "could not send the sanitized page capture");
        return;
      }
      reply("captured");
    } finally {
      this.pageCaptureDriving = false;
      this.managedTabURLs.delete(managedKey);
      if (tabID !== undefined) {
        this.closingTabs.add(tabID);
        try {
          await this.deps.tabs.remove(tabID);
        } catch {
          this.closingTabs.delete(tabID);
        }
        await this.forgetLedgeredTab(tabID);
      }
    }
  }

  /** Build, self-validate, and post one outbound frame. Validation is a safety
   * net: a frame that would not survive the shared parser is dropped, never
   * emitted. */
  private send(type: BrowserMessageType, payload: object, jobID?: string): boolean {
    const port = this.port;
    if (!port) return false;
    const env: Record<string, unknown> = {
      protocol: BROWSER_PROTOCOL_VERSION,
      type,
      msg_id: this.deps.randomUUID().replace(/-/g, ""),
      seq: this.seq++,

      payload,
    };
    if (jobID !== undefined) env.job_id = jobID;
    try {
      if (new TextEncoder().encode(JSON.stringify(env)).byteLength > MAX_BROWSER_MESSAGE_BYTES) {
        console.error("papio: refusing to send frame over native message cap", type);
        return false;
      }
    } catch (e) {
      console.error("papio: refusing to encode outbound frame", type, e);
      return false;
    }
    try {
      parseBrowserMessage(env);
    } catch (e) {
      console.error("papio: refusing to send invalid frame", type, e);
      return false;
    }
    try {
      port.postMessage(env);
      return true;
    } catch (e) {
      console.error("papio: native postMessage failed", e);
      return false;
    }
  }

  private enqueueInbound(raw: unknown, sourcePort: NativePort): Promise<void> {
    const dispatched = this.inboundChain.then(() => {
      if (this.port !== sourcePort) return;
      return this.onInbound(raw);
    });
    // Keep later frames progressing even if a single handler fails unexpectedly;
    // the returned promise still exposes that failure to the event emitter.
    this.inboundChain = dispatched.catch((e) => {
      console.error("papio: inbound frame handler failed", e);
    });
    return dispatched;
  }

  private resolveNativeResponse(msg: BrowserMessage): void {
    const requestID = msg.payload["request_id"];
    if (typeof requestID !== "string") return;
    const pending = this.pendingNativeRequests.get(requestID);
    if (pending === undefined || pending.expectedType !== msg.type) {
      console.debug("papio: dropping unknown or late correlated response", msg.type, requestID);
      return;
    }
    this.pendingNativeRequests.delete(requestID);
    pending.resolve({ kind: "response", payload: msg.payload });
  }
  private onUnsolicitedPdfGrab(msg: BrowserMessage): void {
    const grabID = msg.payload["grab_id"];
    const outcome = msg.payload["outcome"];
    if (typeof grabID !== "string" || typeof outcome !== "string") return;
    const track = this.grabDownloads.get(grabID);
    const persisted = this.pdfGrabCorrelations.get(grabID);
    const correlation = track === undefined ? persisted : { scanID: track.scanID, tabID: track.tabID, state: "identifying" };
    if (correlation === undefined) return;
    const detail = typeof msg.payload["detail"] === "string" ? msg.payload["detail"] : undefined;
    const terminal =
      outcome === "job_created" ? "job_created" :
      outcome === "already_owned" ? "already_owned" :
      outcome === "needs_identifier" ? "needs_identifier" :
      "failed";
    // Terminal pushes are deliberately at-most-once; a reopened workspace reads
    // the fresh snapshot rather than replaying an old outcome.
    this.notifyPdfGrab(correlation.scanID, grabID, terminal, detail);
    this.grabDownloads.delete(grabID);
    this.pdfGrabCorrelations.delete(grabID);
    this.persistPdfGrabCorrelations();
  }

  private async onInbound(raw: unknown): Promise<void> {
    let msg: BrowserMessage;
    try {
      msg = parseBrowserMessage(raw);
    } catch (e) {
      // Fail closed: a malformed frame means the peer is untrustworthy.
      console.error("papio: protocol error on inbound frame; disconnecting", e);
      this.disconnect();
      return;
    }
    await this.ready;
    // Every correlated daemon result is routed from ONE list. When the switch
    // below enumerated these case-by-case, review_preview_result was simply
    // absent: the daemon issued the preview capability, the frame fell through
    // to the ignore-echo default, and every "View PDF" click sat until its
    // request timed out reporting that the daemon had not responded. A reply
    // type can no longer be named as a requestNative expectation and go
    // unrouted here.
    const grabRequestID = msg.payload["request_id"];
    if (msg.type === "pdf_grab_result" && (grabRequestID === undefined || grabRequestID === "")) {
      this.onUnsolicitedPdfGrab(msg);
      return;
    }
    if (CORRELATED_RESULT_TYPES.has(msg.type)) {
      this.resolveNativeResponse(msg);
      return;
    }
    switch (msg.type) {
      case "page_capture_request":
        await this.onPageCaptureRequest(msg);
        return;
      case "job_offer":
        await this.onJobOffer(msg);
        return;
      case "cancel":
        await this.onCancel(msg);
        return;
      case "handoff_focus":
        if (msg.job_id !== undefined) {
          // A missing handoff may refresh counts, whose reply is serialized on
          // this FIFO; detach it so the correlated reply can be received.
          void this.focusDaemonHandoff(msg.job_id);
        }
        return;
      case "hello_ack": {
        const version = typeof msg.payload.daemon_version === "string" ? msg.payload.daemon_version : null;
        const features = Array.isArray(msg.payload.features)
          ? msg.payload.features.filter((feature): feature is string => typeof feature === "string")
          : [];
        const resolverOrigins = Array.isArray(msg.payload.resolver_origins)
          ? msg.payload.resolver_origins.filter((o): o is string => typeof o === "string")
          : [];
        const connectionStatus =
          version !== null && isSemverLowerThan(version, MIN_DAEMON_VERSION) ? "daemon_outdated" : "connected";
        const stampedVersion =
          typeof __PAPIO_DAEMON_VERSION__ === "string" ? __PAPIO_DAEMON_VERSION__ : "";
        await this.update((s) => ({
          ...s,
          connectionStatus,
          daemonVersion: version,
          daemonUpdateHint: hasDaemonUpdateHint(version, stampedVersion),
          daemonFeatures: features,
          resolverOrigins,
        }));
        await this.syncConnectionBadge(connectionStatus);
        this.helloAckGeneration = this.portGeneration;
        this.keepaliveManager?.notifyConfiguredOriginsChanged();
        this.settleHelloWaiters(true);
        return;
      }
      case "page_acquire_ack": {
        const waiter = this.pageAcquireWaiters.shift();
        if (waiter) {
          waiter({
            ...(typeof msg.payload.job_id === "string" ? { job_id: msg.payload.job_id } : {}),
            ...(typeof msg.payload.duplicate === "boolean" ? { duplicate: msg.payload.duplicate } : {}),
            ...(typeof msg.payload.error === "string" ? { error: msg.payload.error } : {}),
          });
        }
        return;
      }
      case "ack":
        await this.closeAfterAdoption(msg.job_id);
        return;
      case "error":
        console.warn("papio: daemon reported error", msg.payload);
        if (msg.payload.code === "expected_hello") this.reconnectForHello();
        if (msg.payload.code === "extension_outdated") {
          await this.update((s) => ({ ...s, connectionStatus: "extension_outdated" }));
          await this.syncConnectionBadge("extension_outdated");
        }
        return;
      default:
        // Extension->daemon-only types are ignored if echoed back.
        return;
    }
  }

  private async onJobOffer(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    const p = msg.payload;
    const openurl = p["openurl"];
    const hostsRaw = p["provider_hosts"];
    const expiresAt = p["expires_at"];
    // Shape is already guaranteed by parseBrowserMessage; these narrow for TS.
    if (typeof openurl !== "string" || !Array.isArray(hostsRaw) || typeof expiresAt !== "string") return;
    const priorOfferURL = this.offerURLs.get(jobID);
    const providerHosts = hostsRaw.filter((h): h is string => typeof h === "string");
    const providerKey = this.providerKeyForHosts(providerHosts);
    const providerParked = this.currentProviderDrainLease(providerKey)?.parkedReason === "challenge";
    const challengeCooldown = this.challengeCooldownActiveForHosts(providerHosts);
    const expected = parseExpected(p["expected"]);
    const requiresAuth = typeof p["requires_auth"] === "boolean" ? p["requires_auth"] : undefined;
    const loginEntityID = p["login_entity_id"];
    if (typeof loginEntityID === "string" && loginEntityID.length > 0) {
      this.loginEntityIDs.set(jobID, loginEntityID);
    }
    const proquestAccountID = p["proquest_account_id"];
    if (typeof proquestAccountID === "string" && proquestAccountID.length > 0) {
      this.proquestAccountIDs.set(jobID, proquestAccountID);
    }

    // Restart/re-offer dedup normally re-accepts a live tab. A tab-less job
    // without its durable offer URL cannot represent an in-flight download:
    // discard that stale record so this offer recreates the real browser work.
    const existing = findByJob(this.store, jobID);
    const pendingDelivery = this.store.pendingDelivery;
    if (pendingDelivery?.job_id === jobID && pendingDelivery.status !== "failed") {
      const now = this.deps.now();
      const expiresMs = Date.parse(expiresAt);
      const deliveryJob: ActiveJob = existing ?? {
        job_id: jobID,
        tab_id: -1,
        offered_at: now,
        expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
        status: "accepted",
        provider_hosts: providerHosts,
      };
      await this.upsertJobWithOffer(
        {
          ...deliveryJob,
          provider_hosts: providerHosts,
          ...(expected !== undefined ? { expected } : {}),
          ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
        },
        openurl,
      );
      this.send("job_accept", {}, jobID);
      return;
    }
    if (existing !== undefined && existing.requires_auth !== requiresAuth) {
      // A restored job can predate this field; its first re-offer must learn the
      // requirement before a fallback can recreate an expired sign-in request.
      if (requiresAuth === undefined) {
        await this.update((s) => ({
          ...s,
          activeJobs: s.activeJobs.map((job) => {
            if (job.job_id !== jobID) return job;
            const next = { ...job };
            delete next.requires_auth;
            return next;
          }),
        }));
      } else {
        await this.update((s) => patchJob(s, jobID, { requires_auth: requiresAuth }));
      }
    }
    if (existing) {
      if (existing.tab_id < 0) {
        if (priorOfferURL === undefined) {
          this.downloads.delete(jobID);
          await this.removeJobWithOffer(jobID);
        } else if (priorOfferURL === openurl) {
          if (providerParked || challengeCooldown) {
            await this.update((s) => patchJob(s, jobID, { handoffAckPending: true }));
            this.scheduleQueuedHandoffRelease(jobID);
            return;
          }
          if (existing.handoffAckPending === true) {
            if (!(await this.acknowledgePendingProviderHandoffs(providerKey))) return;
          } else {
            this.send("job_accept", {}, jobID);
          }
          if (existing.status === "queued") {
            this.scheduleQueuedHandoffRelease(jobID);
            await this.releaseQueuedHandoffs();
          }
          return;
        } else {
          this.downloads.delete(jobID);
          await this.removeJobWithOffer(jobID);
        }
      } else {
        let live = false;
        try {
          const tab = await this.deps.tabs.get(existing.tab_id);
          live = tab.id === existing.tab_id;
        } catch {
          live = false;
        }
        if (live && (priorOfferURL === undefined || priorOfferURL === openurl)) {
          if (providerParked || challengeCooldown) {
            await this.update((s) => patchJob(s, jobID, { handoffAckPending: true }));
            return;
          }
          if (!this.handoffDrives.has(jobID)) {
            if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
              this.enqueueHandoffDrive({ jobID, purpose: "reoffer", focusExisting: false });
            } else {
              this.registerHandoffDrive(jobID, existing.tab_id);
            }
          }
          if (existing.handoffAckPending === true) {
            await this.acknowledgePendingProviderHandoffs(providerKey);
          } else {
            this.send("job_accept", {}, jobID);
          }
          return;
        }
        if (
          live &&
          !providerParked &&
          !challengeCooldown &&
          !(isDirectFileOffer(openurl) && requiresAuth !== true)
        ) {
          if (this.authAttemptsFor(jobID) >= MAX_AUTH_ATTEMPTS) {
            this.rememberStalledAuthHandoff(jobID, {
              url: openurl,
              providerHosts,
              ...(expected !== undefined ? { expected } : {}),
              ...(requiresAuth !== undefined ? { requiresAuth } : {}),
            });
            await this.reportAuthStalled(jobID);
            return;
          }
          if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT && !this.handoffDrives.has(jobID)) {
            await this.upsertJobWithOffer(
              {
                ...existing,
                offered_at: this.deps.now(),
                status: "accepted",
                provider_hosts: providerHosts,
                // ...existing may still carry a stale parked_with_tab: true left
                // over from a prior timeout park. enqueueHandoffDrive just below
                // also clears it, but only after this write already lands — so
                // writing the stale value here first would still let a worker
                // restart between these two calls see a live status plus the
                // marker and skip re-registering the job forever.
                parked_with_tab: false,
              },
              openurl,
            );
            this.enqueueHandoffDrive({ jobID, purpose: "reoffer", focusExisting: false });
            if (existing.handoffAckPending === true) {
              await this.acknowledgePendingProviderHandoffs(providerKey);
            } else {
              this.send("job_accept", {}, jobID);
            }
            return;
          }
          const tabID = await this.openManagedTab({
            url: openurl,
            jobId: jobID,
            purpose: "reoffer",
          });
          if (tabID === undefined) {
            this.send("job_reject", {}, jobID);
            return;
          }
          this.beginProviderDrive(jobID);
          const expiresMs = Date.parse(expiresAt);
          const refreshed: ActiveJob = {
            ...existing,
            tab_id: tabID,
            offered_at: this.deps.now(),
            expires_at: Number.isNaN(expiresMs) ? this.deps.now() : expiresMs,
            status: "accepted",
            provider_hosts: providerHosts,
            ...(expected !== undefined ? { expected } : {}),
            ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
          };
          if (expected === undefined) delete refreshed.expected;
          if (requiresAuth === undefined) delete refreshed.requires_auth;
          delete refreshed.challenge_blocked;
          delete refreshed.challenge_host;
          delete refreshed.challenge_kind;
          delete refreshed.challenge_blocked_at;
          if (existing.handoffAckPending !== true) delete refreshed.handoffAckPending;
          await this.upsertJobWithOffer(refreshed, openurl);
          this.registerHandoffDrive(jobID, tabID);
          if (existing.handoffAckPending === true) {
            await this.acknowledgePendingProviderHandoffs(providerKey);
          } else {
            this.send("job_accept", {}, jobID);
          }
          return;
        }
        if (live) {
          this.closingTabs.add(existing.tab_id);
          try {
            await this.deps.tabs.remove(existing.tab_id);
          } catch (e) {
            console.error("papio: could not replace prior handoff tab", e);
            this.send("job_reject", {}, jobID);
            return;
          }
        }
        await this.removeJobWithOffer(jobID);
      }
    }

    if (this.authAttemptsFor(jobID) >= MAX_AUTH_ATTEMPTS) {
      // This browser session has driven the job through human authentication
      // MAX_AUTH_ATTEMPTS times without a download: the warm session cannot
      // complete it. Report the human step (once) and decline to open another
      // broker tab. No job_reject — that is terminal; the job stays parked and
      // is re-offered on a future launch with a fresh budget.
      this.rememberStalledAuthHandoff(jobID, {
        url: openurl,
        providerHosts,
        ...(expected !== undefined ? { expected } : {}),
        ...(requiresAuth !== undefined ? { requiresAuth } : {}),
      });
      await this.reportAuthStalled(jobID);
      return;
    }

    const now = this.deps.now();
    const expiresMs = Date.parse(expiresAt);
    const makeJob = (tabID: number, status: ActiveJob["status"] = "accepted"): ActiveJob => ({
      job_id: jobID,
      tab_id: tabID,
      offered_at: now,
      expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
      status,
      provider_hosts: providerHosts,
      ...(expected !== undefined ? { expected } : {}),
      ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
    });
    // A direct-file URL is not permission to download unattended. The
    // shape-matching above says only "this looks like a file"; whether a human
    // must sign in is a property of the ACTION, and the daemon has already
    // decided it. Checking it here rather than refusing to emit file-shaped
    // URLs daemon-side keeps isDirectFileOffer's heuristic in one component:
    // the offer URL for an institutional handoff is the operator's configured
    // OpenURL base, whose path papio does not constrain, so a pdf-shaped base
    // would otherwise route a sign-in-required offer straight to a download.
    // Only an explicit true refuses; absent and false behave as before.
    if (isDirectFileOffer(openurl) && requiresAuth !== true && !challengeCooldown) {
      await this.upsertJobWithOffer(makeJob(-1), openurl);
      this.send("job_accept", {}, jobID);
      await this.startDirectOfferDownload(jobID, openurl);
      return;
    }

    const governorQueued =
      !providerParked &&
      !challengeCooldown &&
      this.hasHandoffReleaseEvidence(this.resolverOriginHint(openurl), requiresAuth) &&
      (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT || this.handoffDriveQueue.length > 0);
    const queueHandoff =
      providerParked ||
      challengeCooldown ||
      governorQueued ||
      (!this.hasHandoffReleaseEvidence(this.resolverOriginHint(openurl), requiresAuth) &&
        (requiresAuth === true ||
          this.handoffOpening ||
          this.store.activeJobs.some((job) => job.tab_id >= 0 && job.status !== "queued")));
    if (queueHandoff) {
      const queued = makeJob(-1, governorQueued ? "accepted" : "queued");
      await this.upsertJobWithOffer(
        providerParked || challengeCooldown ? { ...queued, handoffAckPending: true } : queued,
        openurl,
      );
      if (governorQueued) {
        this.enqueueHandoffDrive({ jobID, purpose: "handoff" });
        this.send("job_accept", {}, jobID);
        await this.drainHandoffDriveQueue();
      } else {
        this.scheduleQueuedHandoffRelease(jobID);
        if (!providerParked && !challengeCooldown) this.send("job_accept", {}, jobID);
      }
      return;
    }

    this.handoffOpening = true;
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: openurl,
        jobId: jobID,
        purpose: "handoff",
      });
    } catch (e) {
      console.error("papio: tab creation failed; rejecting job", e);
    } finally {
      this.handoffOpening = false;
    }
    if (tabID === undefined) {
      this.send("job_reject", {}, jobID);
      return;
    }
    this.beginProviderDrive(jobID);
    await this.upsertJobWithOffer(makeJob(tabID), openurl);
    this.registerHandoffDrive(jobID, tabID);
    this.send("job_accept", {}, jobID);
  }

  /** Start the one download-first attempt for an unequivocal direct-file URL.
   * Any initiation error falls back to the normal broker-tab handoff. */
  private async startDirectOfferDownload(jobID: string, url: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (!job || job.tab_id >= 0 || job.download_initiated === true) return;
    await this.update((s) => patchJob(s, jobID, { download_initiated: true }));
    // Register the direct-offer classification before Chrome can emit
    // onCreated/onChanged for a small cached response.
    this.downloads.set(jobID, { ids: new Set<number>(), ambiguous: false, directOffer: true });
    this.pendingDownloadURLs.set(url, jobID);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: `papio/${jobID}/paper.pdf`,
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(jobID) ?? { ids: new Set<number>(), ambiguous: false, directOffer: true };
      track.ids.add(id);
      track.directOffer = true;
      if (track.ids.size > 1) track.ambiguous = true;
      this.downloads.set(jobID, track);
    } catch (e) {
      console.error("papio: direct-file download initiation failed; opening handoff tab", e);
      this.downloads.delete(jobID);
      await this.fallbackToOfferTab(jobID);
    } finally {
      this.pendingDownloadURLs.delete(url);
    }
  }

  /** Remove a non-PDF direct attempt and return to the established tab flow. */
  private async discardDirectOffer(jobID: string, downloadID: number): Promise<void> {
    await this.discardDownload(jobID, downloadID);
    await this.fallbackToOfferTab(jobID);
  }

  private async failDelivery(jobID: string, downloadID: number, reason: string): Promise<void> {
    await this.discardDownload(jobID, downloadID);
    this.deliveryJobs.delete(jobID);
    await this.update((s) =>
      updatePendingDelivery(
        patchJob(s, jobID, { download_initiated: false }),
        jobID,
        { status: "failed", error: reason },
      ),
    );
    this.send("error", { code: "download_not_pdf", message: reason }, jobID);
  }

  /** Erase a download we refuse to adopt: tracking, file, and history entry. */
  private async discardDownload(jobID: string, downloadID: number): Promise<void> {
    this.downloads.delete(jobID);
    try {
      await this.deps.downloads.removeFile(downloadID);
    } catch {
      // Interrupted downloads may not have produced a removable file.
    }
    try {
      await this.deps.downloads.erase({ id: downloadID });
    } catch {
      // Clearing history is best-effort; opening the human-visible fallback is not.
    }
  }

  /** Convert a failed download-first attempt into the normal handoff flow. */
  private async fallbackToOfferTab(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    const url = this.offerURLs.get(jobID);
    if (!job || job.tab_id >= 0 || url === undefined) return;
    const governorQueued =
      this.hasHandoffReleaseEvidence(this.resolverOriginHint(url), job.requires_auth) &&
      (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT || this.handoffDriveQueue.length > 0);
    const queueHandoff =
      governorQueued ||
      (!this.hasHandoffReleaseEvidence(this.resolverOriginHint(url), job.requires_auth) &&
        (job.requires_auth === true ||
          this.handoffOpening ||
          this.store.activeJobs.some((candidate) => candidate.tab_id >= 0 && candidate.status !== "queued")));
    if (queueHandoff) {
      await this.update((s) =>
        patchJob(s, jobID, {
          status: governorQueued ? "accepted" : "queued",
          tab_id: -1,
          download_initiated: false,
        }),
      );
      if (governorQueued) {
        this.enqueueHandoffDrive({ jobID, purpose: "handoff" });
        await this.drainHandoffDriveQueue();
      } else {
        this.scheduleQueuedHandoffRelease(jobID);
      }
      return;
    }

    this.handoffOpening = true;
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url,
        jobId: jobID,
        purpose: "handoff",
      });
    } catch (e) {
      console.error("papio: tab creation failed after direct-file download", e);
    } finally {
      this.handoffOpening = false;
    }
    if (tabID === undefined) {
      this.send("job_reject", {}, jobID);
      await this.removeJobWithOffer(jobID);
      return;
    }
    this.beginProviderDrive(jobID);
    await this.update((s) =>
      patchJob(s, jobID, {
        tab_id: tabID,
        status: "accepted",
        download_initiated: false,
        unknown_count: 0,
      }),
    );
    this.registerHandoffDrive(jobID, tabID);
  }

  private async onCancel(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    if (job.tab_id >= 0) {
      await this.closeManagedHandoffTab(job, job.tab_id);
    } else {
      this.releaseHandoffDrive(jobID);
    }
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    await this.removeJobWithOffer(jobID);
  }

  /** The daemon acknowledges download_complete only after it has attempted
   * adoption. Close the broker-owned viewer then, never on a raw tab event. */
  private async closeAfterAdoption(jobID: string | undefined): Promise<void> {
    if (jobID === undefined) return;
    const isDelivery = this.deliveryJobs.has(jobID) || this.store.pendingDelivery?.job_id === jobID;
    if (isDelivery) {
      this.completedDownloadTabs.delete(jobID);
      this.deliveryJobs.delete(jobID);
      this.lastDeliveryState = {
        job_id: jobID,
        state: "adopted",
        message: "papio adopted v (validating)",
        at: this.deps.now(),
      };
      await this.update((s) => clearPendingDelivery(s, jobID));
      await this.removeJobWithOffer(jobID);
      return;
    }
    const tabID = this.completedDownloadTabs.get(jobID);
    if (tabID === undefined) return;
    this.completedDownloadTabs.delete(jobID);
    const job = findByJob(this.store, jobID);
    if (job !== undefined && tabID >= 0) await this.closeManagedHandoffTab(job, tabID);
    this.releaseHandoffDrive(jobID);
    await this.removeJobWithOffer(jobID);
  }

  /** Run the bounded DOM probe for a tracked page and preserve the existing
   * challenge-blocked contract. The boolean argument is origin evidence for
   * OpenAthens-only body markers; page text never leaves the injected function. */
  private async assessTrackedDrivenPage(
    job: ActiveJob,
    host: string,
    url: string,
  ): Promise<boolean> {
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: assessDrivenPage,
        args: [null, host === OPENATHENS_LOGIN_HOST],
      });
      const assessment = results[0]?.result as DrivenPageAssessment | undefined;
      if (assessment?.kind === "challenge" || assessment?.kind === "redirect_loop") {
        await this.blockChallenge(
          job,
          host,
          assessment.kind === "challenge" ? "cloudflare" : "redirect_loop",
          url,
        );
        return true;
      }
      if (assessment?.kind === "normal" && job.challenge_blocked === true) {
        return !(await this.clearChallengeBlock(job));
      }
    } catch (e) {
      console.error("papio: driven-page assessment failed; continuing handoff", e);
    }
    return false;
  }

  /** Chrome may publish the terminal OpenAthens title before React/body text.
   * Give each document epoch exactly one late probe; it never navigates or
   * retries the page and retains the terminal tab when the marker appears. */
  private scheduleOpenAthensErrorRecheck(job: ActiveJob, epoch: number): void {
    if (this.openAthensErrorRecheckEpochs.get(job.job_id) === epoch) return;
    this.openAthensErrorRecheckEpochs.set(job.job_id, epoch);
    this.deps.setTimeout(async () => {
      await this.ready;
      if (this.openAthensErrorRecheckEpochs.get(job.job_id) !== epoch) return;
      if ((this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== epoch) return;
      const current = findByJob(this.store, job.job_id);
      if (current === undefined || current.tab_id !== job.tab_id || current.challenge_blocked === true) return;
      let tab: TabInfo;
      try {
        tab = await this.deps.tabs.get(current.tab_id);
      } catch {
        return;
      }
      if (tab.url === undefined || tab.title !== OPENATHENS_ERROR_TITLE) return;
      let host: string;
      try {
        host = new URL(tab.url).hostname.toLowerCase();
      } catch {
        return;
      }
      if (host !== OPENATHENS_LOGIN_HOST) return;
      await this.assessTrackedDrivenPage(current, host, tab.url);
    }, OPENATHENS_ERROR_RECHECK_MS);
  }

  private async onTabUpdated(tabID: number, change: TabChangeInfo, tab: TabInfo): Promise<void> {
    const pageCaptureWaiter = this.pageCaptureLoadWaiters.get(tabID);
    if (pageCaptureWaiter !== undefined && change.status === "complete") {
      pageCaptureWaiter(true);
    }
    // A tracked handoff tab landing on the resolver is the same evidence as
    // an untracked one — the manager parses to an origin and discards the
    // raw URL itself, so this runs unconditionally before the tracked-job
    // gate below (and before an in-flight injection could commit a result
    // against a document epoch this navigation just invalidated). SPA/history
    // navigation on the tracked path below can land without a "complete"
    // status, which is why "loading" is included here too.
    if (change.url !== undefined || change.status === "complete" || change.status === "loading") {
      this.keepaliveManager?.noteResolverNavigation(tabID, change.url ?? tab.url);
    }
    await this.ready;
    const job = findByTab(this.store, tabID);
    if (!job) {
      // A provider "download" that opens the PDF in a NEW viewer tab (e.g. JSTOR
      // navigates to /stable/pdf/<id>.pdf) is untracked here. Adopt it for the
      // tracked handoff tab that spawned it so the PDF still flows to the daemon.
      if (change.status === "complete") await this.maybeAdoptViewerTab(tabID, change.url ?? tab.url, tab.openerTabId);
      return;
    }
    const url = change.url ?? tab.url;
    if (url === undefined) return;
    if (change.status === "loading") this.advanceStaleRecoveryEpoch(job.job_id);
    const staleRecoveryEpoch = this.staleRecoveryEpochs.get(job.job_id) ?? 0;
    let host: string;
    try {
      host = new URL(url).hostname;
    } catch {
      return;
    }
    // A title-only update counts: the the default institution Shibboleth stale page is classifiable
    // ONLY by its title ("… Login Service - Stale Request"); its URL is byte-for-byte
    // the URL of the working login form. Chrome can deliver that title after the
    // `complete` event, and detection used to run on `complete` alone — so the one
    // page papio most needs to recognize was the one it could silently miss.
    if (change.status === "complete" || change.title !== undefined) {
      const failure = detectAuthFailure(url, tab.title);
      if (failure !== undefined) {
        // Surface every recognized IdP failure. Only a terminal stale-request
        // signature is safe to navigate away from; password recovery and retry
        // forms must remain where the human can use them.
        if (!this.authFailureSurfaced.has(job.job_id)) {
          this.authFailureSurfaced.add(job.job_id);
          await this.surfaceWorkTab(job.tab_id);
        }
        // Mark only after a successful send: a dropped native port must not
        // permanently swallow the one report this job gets for this outcome.
        if (
          !this.handoffOutcomeSent.has(`${job.job_id}:${failure}`) &&
          this.send("handoff_outcome", { outcome: failure, final_host: host }, job.job_id)
        ) {
          this.handoffOutcomeSent.add(`${job.job_id}:${failure}`);
        }
        if (
          failure === "stale_sso" &&
          /\bstale\s+request\b/i.test(tab.title ?? "") &&
          (await this.redriveStaleHandoff(job, staleRecoveryEpoch))
        ) {
          return;
        }
      }
    }
    if (
      host === OPENATHENS_LOGIN_HOST &&
      change.title === OPENATHENS_ERROR_TITLE
    ) {
      this.scheduleOpenAthensErrorRecheck(job, staleRecoveryEpoch);
    }
    const adapter = this.deps.adapterSpecs.find((candidate) => hostMatches(host, candidate.hosts));
    // The registry is source-controlled and may cover hosts omitted from the
    // capped offer list. Persist its identity before any permission-dependent
    // classification so later native download events can safely correlate it.
    if (adapter !== undefined && job.adapter_id !== adapter.id) {
      await this.update((s) => patchJob(s, job.job_id, { adapter_id: adapter.id }));
    }
    const successfulLanding = change.status === "complete" && !isAuthenticationURL(url);
    if (successfulLanding) {
      const institutionalSession = await this.recordInstitutionalSession(job, url, this.deps.now());
      if (!institutionalSession) await this.recordOpenAccessLanding(job);
    }
    if (change.status === "complete" && (await this.maybeRouteResolver(job, url))) return;
    // The offer's provider_hosts list is capped by the protocol (20 entries);
    // the adapter registry is the authoritative host source for classification,
    // so a tracked handoff landing on any registered family is on-provider.
    const onProvider = hostMatches(host, job.provider_hosts) || adapter !== undefined;
    // Back on the provider means this tab left the IdP (successfully or not);
    // either way it can no longer be the live sibling any waiting job is
    // deferring to for this origin.
    if (onProvider) await this.clearFederatedLoginOwnerForTab(tabID);
    const shouldAssessBeforeRouting =
      (change.status === "complete" || change.title !== undefined) &&
      (onProvider || isAuthenticationURL(url));
    if (
      shouldAssessBeforeRouting &&
      (change.title !== undefined || !onProvider) &&
      (await this.assessTrackedDrivenPage(job, host, url))
    ) {
      return;
    }
    if (!onProvider) {
      // Reuse the durable resolver-provided offer URL that produced that viewer.
      if (change.status === "complete" && host === CHROME_PDF_VIEWER_HOST) {
        const offeredURL = this.offerURLs.get(job.job_id);
        if (offeredURL !== undefined) {
          await this.maybeDownloadPDFViewer(job.job_id, offeredURL, true);
          return;
        }
      }
      // A direct PDF can legitimately land on a CDN outside the offer's
      // provider-host list. Its URL alone is sufficient to preserve the
      // browser download flow without treating that redirect as an IdP hop.
      if (change.status === "complete") {
        let directPDF = false;
        try {
          directPDF = new URL(url).pathname.toLowerCase().endsWith(".pdf");
        } catch {
          directPDF = false;
        }
        if (directPDF) {
          await this.maybeDownloadPDFViewer(job.job_id, url);
          return;
        }
      }
      // A stable non-authentication landing outside the capped offer list is
      // still the resolver's provider result. Give it the same bounded
      // no-adapter evidence window instead of leaving a permanent spinner.
      if (successfulLanding) {
        await this.maybeClassify(job.job_id, host);
        return;
      }
      if (job.status !== "auth_pending" && !successfulLanding) {
        // Leaving every provider host for an IdP starts human authentication.
        // A completed non-IdP page is instead a usable resolver landing.
        await this.update((s) =>
          patchJob(s, job.job_id, { status: "auth_pending", auth_started_ms: this.deps.now() }),
        );
        this.send("auth_pending", {}, job.job_id);
        await this.noteAuthAttempt(job.job_id, tabID);
        await this.surfaceWorkTab(tabID);
      }
      return;
    }
    if (job.status === "auth_pending") {
      const started = job.auth_started_ms ?? this.deps.now();
      const now = this.deps.now();
      const elapsed = Math.max(0, now - started);
      this.deliverySessionEvidence.set(job.job_id, "fresh_auth");
      // This transition is also how a job parked with its tab preserved
      // (parked_with_tab, state.ts) resumes: the operator finished auth in
      // that same tab without ever going through registerHandoffDrive again,
      // so nothing else clears the marker here. Leaving it stale would make
      // a later restart wrongly skip re-registering this now-legitimate
      // awaiting_download job, stranding it the same way the timeout park
      // itself used to before parked_with_tab existed.
      await this.update((s) => patchJob(s, job.job_id, { status: "awaiting_download", parked_with_tab: false }));
      this.send("auth_returned", { elapsed_ms: elapsed }, job.job_id);
      // jobInstitutionOrigin holds this producer to the same configured-origin
      // bar as recordFreshSessionEvidence and recordInstitutionalSession: an
      // origin outside the daemon's configured set never rides along as the
      // hint, and an absent hint stays the documented safe default. It also
      // survives LibKey-fronted offers, whose offer origin is libkey.io
      // rather than the institution's resolver.
      const authReturnedOriginHint = this.jobInstitutionOrigin(job);
      this.emitSessionEvidence("auth_returned", authReturnedOriginHint);
      // The human is past authentication; fold the "papio" group back away.
      await this.recollapseHandoffGroup(tabID);
      const institutionalSession = await this.recordInstitutionalSession(job, url, now);
      if (!institutionalSession) await this.recordOpenAccessLanding(job);
      // If we routed this job through federated login, the return lands on the
      // provider's generic post-login page (the DS target), not the article.
      // Re-drive the original openurl once so the now-warm session resolves the
      // entitled page; the fresh navigation triggers classify below.
      if (this.federatedLoginRouted.has(job.job_id) && !this.federatedReDriven.has(job.job_id)) {
        const openurl = this.offerURLs.get(job.job_id);
        if (openurl !== undefined) {
          this.federatedReDriven.add(job.job_id);
          await this.openManagedTab({
            url: openurl,
            jobId: job.job_id,
            purpose: "redrive",
            focusExisting: false,
          });
          return;
        }
      }
      // The provider landing that ends authentication frequently arrives
      // without a `status: "complete"` (SPA soft-nav, history push, or a
      // resolver/interstitial hop), so the complete-gated classify below never
      // runs. Classify now; interpret's settle waits for the provider's
      // late-upgrading controls, and the download latch keeps this idempotent
      // with any subsequent complete.
      await this.maybeClassify(job.job_id, host);
    }
    // Once the provider page has finished loading on the tracked tab (past any
    // human auth), run the declarative adapter — permission-gated, tracked-tab
    // only. Re-reads fresh job state; a stale local `job` here is fine.
    if (change.status === "complete") {
      await this.maybeDownloadPDFViewer(job.job_id, url);
      await this.maybeClassify(job.job_id, host);
    }
  }

  /** Chrome's loading signal is the boundary between stale documents: title and
   * complete notifications for that document can run concurrently. */

  private advanceStaleRecoveryEpoch(jobID: string): void {
    this.staleRecoveryEpochs.set(jobID, (this.staleRecoveryEpochs.get(jobID) ?? 0) + 1);
    this.staleRecoveryAttemptedEpochs.delete(jobID);
    this.staleRecoveryInFlightEpochs.delete(jobID);
  }

  /**
   * Re-drive a handoff tab stranded on a dead IdP page through its retained
   * resolver offer URL, so the resolver mints a fresh SAML exchange against the
   * now-warmer session. The daemon only records the failure; recovery lives here.
   *
   * Charged against the same durable per-job authentication budget as a
   * broker-tab drive. The worker-local report debounce cannot bound this: a
   * service-worker restart clears it while the dead tab, the parked job, and the
   * user's next sign-in attempt all survive, so the old "once per outcome" latch
   * degenerated into an unbounded resolver loop across restarts. Past the cap the
   * tab is deliberately LEFT on the failure page — the user needs to see it — and
   * the job is reported human_auth_required, which keeps it parked daemon-side.
   *
   * Returns true once this document is claimed, so another callback cannot
   * fall through and spend a second entry in the same recovery budget.
   */
  private async redriveStaleHandoff(job: ActiveJob, recoveryEpoch: number): Promise<boolean> {
    if ((this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== recoveryEpoch) return true;
    if (
      this.staleRecoveryAttemptedEpochs.get(job.job_id) === recoveryEpoch ||
      this.staleRecoveryInFlightEpochs.get(job.job_id) === recoveryEpoch
    ) {
      return true;
    }
    const openurl = this.offerURLs.get(job.job_id);
    if (openurl === undefined || job.tab_id < 0) return false;
    this.staleRecoveryAttemptedEpochs.set(job.job_id, recoveryEpoch);
    this.staleRecoveryInFlightEpochs.set(job.job_id, recoveryEpoch);
    try {
      if (this.authAttemptsFor(job.job_id) >= MAX_AUTH_ATTEMPTS) {
        this.rememberStalledAuthHandoff(job.job_id, {
          url: openurl,
          providerHosts: job.provider_hosts,
          ...(job.expected !== undefined ? { expected: job.expected } : {}),
          ...(job.requires_auth !== undefined ? { requiresAuth: job.requires_auth } : {}),
        });
        await this.reportAuthStalled(job.job_id);
        return false;
      }
      await this.chargeAuthAttempt(job.job_id, job.tab_id);
      if (!this.handoffDrives.has(job.job_id) && this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
        this.enqueueHandoffDrive({ jobID: job.job_id, purpose: "redrive", focusExisting: false });
        await this.drainHandoffDriveQueue();
        return true;
      }
      const tabID = await this.openManagedTab({
        url: openurl,
        jobId: job.job_id,
        purpose: "redrive",
        focusExisting: false,
      });
      if (tabID !== undefined && !this.handoffDrives.has(job.job_id)) {
        this.registerHandoffDrive(job.job_id, tabID);
      }
      return tabID !== undefined;
    } catch {
      // Tab vanished mid-recovery; the normal removal path re-queues.
      return false;
    } finally {
      if (this.staleRecoveryInFlightEpochs.get(job.job_id) === recoveryEpoch) {
        this.staleRecoveryInFlightEpochs.delete(job.job_id);
      }
    }
  }

  /** Download a tracked PDF-viewer navigation through Chrome's download API.
   * The persisted latch and in-memory correlation jointly ensure that a
   * content-disposition download or repeated completion event cannot start a
   * second download for the same job. Page classification stays exclusively in
   * the declarative adapter path; this method accepts only a PDF URL or the
   * recognized Chrome PDF viewer. */
  private async maybeDownloadPDFViewer(jobID: string, url: string, knownPDFViewer = false): Promise<void> {
    let job = findByJob(this.store, jobID);
    if (!job || (job.status !== "accepted" && job.status !== "awaiting_download")) return;
    if (this.isFirefoxClickDownload(job)) return;
    if (job.download_initiated === true || this.downloads.has(jobID)) return;

    let viewer = knownPDFViewer;
    if (!viewer) {
      try {
        viewer = new URL(url).pathname.toLowerCase().endsWith(".pdf");
      } catch {
        viewer = false;
      }
    }
    if (!viewer) return;

    // Re-read after the permission/probe awaits: a content-disposition
    // download may have been correlated while this probe was in flight.
    job = findByJob(this.store, jobID);
    if (!job || job.download_initiated === true || this.downloads.has(jobID)) return;
    await this.update((s) => patchJob(s, jobID, { download_initiated: true }));

    this.pendingDownloadURLs.set(url, jobID);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: `papio/${jobID}/paper.pdf`,
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(jobID) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
      track.ids.add(id);
      if (track.ids.size > 1) track.ambiguous = true;
      this.downloads.set(jobID, track);
    } catch (e) {
      console.error("papio: PDF-viewer download initiation failed; staying assisted", e);
    } finally {
      this.pendingDownloadURLs.delete(url);
    }
  }

  /**
   * Adopt a PDF that a provider opened in a NEW viewer tab (target=_blank
   * navigation to a `.pdf`), correlating it to the tracked handoff tab that
   * spawned it. The adapter's click set `download_initiated` but produced a
   * viewer, not a `chrome.downloads` item — so gate on "no download tracked
   * yet" (this.downloads) rather than the latch. Downloads the URL through the
   * browser cookie jar so the daemon's adoption/import path runs, then closes
   * the viewer tab. Falls back to leaving the tab (assisted) on any ambiguity.
   */
  private async maybeAdoptViewerTab(viewerTabId: number, url: string | undefined, openerTabId: number | undefined): Promise<void> {
    if (url === undefined) return;
    let isPDF = false;
    let host: string;
    try {
      const u = new URL(url);
      host = u.hostname;
      isPDF = u.pathname.toLowerCase().endsWith(".pdf");
    } catch {
      return;
    }
    if (!isPDF) return;

    // Prefer the opener correlation; fall back to a unique provider-host job
    // that clicked (download_initiated) but has no real download yet.
    const candidates = this.store.activeJobs.filter((j) => {
      if (this.downloads.has(j.job_id)) return false;
      if (this.isFirefoxClickDownload(j)) return false;
      if (j.status !== "accepted" && j.status !== "awaiting_download") return false;
      if (openerTabId !== undefined && j.tab_id === openerTabId) return true;
      return openerTabId === undefined && j.download_initiated === true && hostMatches(host, j.provider_hosts);
    });
    const job = candidates.length === 1 ? candidates[0] : candidates.find((j) => j.tab_id === openerTabId);
    if (!job) return;

    this.pendingDownloadURLs.set(url, job.job_id);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: `papio/${job.job_id}/paper.pdf`,
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(job.job_id) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
      track.ids.add(id);
      if (track.ids.size > 1) track.ambiguous = true;
      this.downloads.set(job.job_id, track);
      if (job.download_initiated !== true) {
        await this.update((s) => patchJob(s, job.job_id, { download_initiated: true }));
      }
      try {
        await this.deps.tabs.remove(viewerTabId);
      } catch {
        // Viewer tab already gone; adoption still proceeds.
      }
    } catch (e) {
      console.error("papio: viewer-tab PDF adoption failed; staying assisted", e);
    } finally {
      this.pendingDownloadURLs.delete(url);
    }
  }

  /**
   * Route a resolver's first electronic service in the same tracked tab.
   * The offer origin proves this is the institutional resolver for this job;
   * the injected function separately accepts only same-origin Alma service
   * links. Missing host permission or no electronic service stays assisted.
   */
  private async maybeRouteResolver(job: ActiveJob, currentURL: string): Promise<boolean> {
    const offered = this.offerURLs.get(job.job_id);
    if (offered === undefined) return false;
    let offerURL: URL;
    let landingURL: URL;
    try {
      offerURL = new URL(offered);
      landingURL = new URL(currentURL);
    } catch {
      return false;
    }
    // The offer origin proves this is the institutional resolver for this
    // job — unless LibKey fronts the route, in which case the offer origin
    // is libkey.io and the proof is the landing itself: the config-derived
    // institution origin plus a resolver-shaped path.
    const offerIsResolver =
      offerURL.origin === landingURL.origin && /(?:openurl|uresolver)/i.test(offerURL.pathname);
    const institution = this.jobInstitutionOrigin(job);
    const landedOnInstitutionResolver =
      institution !== undefined &&
      landingURL.origin === institution &&
      /(?:openurl|uresolver)/i.test(landingURL.pathname);
    if (!offerIsResolver && !landedOnInstitutionResolver) {
      return false;
    }

    let granted = false;
    try {
      granted = await this.deps.permissions.contains({ origins: [`${landingURL.origin}/*`] });
    } catch {
      return false;
    }
    if (!granted) return false;

    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: routeResolverService,
        args: [null],
      });
      const result = results[0]?.result as ResolverRoute | undefined;
      if (result?.kind === "routed") {
        this.resolverRoutes.add(job.job_id);
        return true;
      }
      if (result?.kind === "no_entitlement") {
        if (!this.resolverNoEntitlementSent.has(job.job_id)) {
          // Deliberately omit adapter metadata and all page/URL data: the
          // resolver's exact zero-holdings marker is sufficient to terminate
          // this institutional attempt.
          if (this.send("provider_outcome", { outcome: "no_entitlement" }, job.job_id)) {
            this.resolverNoEntitlementSent.add(job.job_id);
            await this.settleHandoffAfterOutcome(job.job_id);
          }
        }
        return true;
      }
      // `no_service` is inconclusive: retain the existing assisted behavior.
      return false;
    } catch (e) {
      console.error("papio: resolver routing failed; staying assisted", e);
      return false;
    }
  }
  /** Firefox cannot steer a native click download into papio/<job>, so a click
   * adapter must remain human-assisted there. Direct API downloads carry their
   * own filename and are unaffected. */
  private isFirefoxClickDownload(job: ActiveJob): boolean {
    if (this.deps.downloads.onDeterminingFilename !== undefined || job.adapter_id === undefined) return false;
    const spec = this.deps.adapterSpecs.find((candidate) => candidate.id === job.adapter_id);
    return spec?.download?.method === "click";
  }

  /** A manual browser download may originate from an offer host or from a
   * source-controlled adapter host that was recorded on the tracked landing. */
  private matchesManualDownloadHost(job: ActiveJob, host: string): boolean {
    if (hostMatches(host, job.provider_hosts)) return true;
    if (job.adapter_id === undefined) return false;
    const spec = this.deps.adapterSpecs.find((candidate) => candidate.id === job.adapter_id);
    return spec !== undefined && hostMatches(host, spec.hosts);
  }


  /**
   * Classify the tracked tab's current provider page with the single injected
   * `interpret` function, then act on the verdict. A registered provider is
   * diagnosed before injection when the browser cannot effectively read it;
   * all-sites access is effective access. Adapter execution never touches a tab
   * we do not own for this job.
   */
  private async maybeClassify(jobID: string, host: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (!job) return;
    if (job.status !== "accepted" && job.status !== "awaiting_download") return;
    const spec = this.deps.adapterSpecs.find((candidate) => hostMatches(host, candidate.hosts));
    if (!spec) {
      // Direct-PDF delivery does not need a page adapter. Otherwise verify that
      // the extension can inspect this host, then give the page one bounded
      // render window before declaring a durable coverage gap. Auth returns
      // and provider SPAs can replace their document after the first complete
      // event.
      if (job.download_initiated === true || this.downloads.has(job.job_id)) return;
      const access = await this.hasEffectiveProviderAccess(host);
      if (access !== true) {
        if (access === false) await this.reportBlockedProviderHost(jobID, host);
        return;
      }
      if (await this.clearBlockedProviderHost(host)) await this.syncConnectionBadge();
      const now = this.deps.now();
      const firstUnknownAt = job.last_unknown_ms;
      if (firstUnknownAt === undefined || now - firstUnknownAt < 5000) {
        if (firstUnknownAt === undefined) {
          await this.update((store) =>
            patchJob(store, job.job_id, { unknown_count: 1, last_unknown_ms: now }),
          );
        }
        this.scheduleClassifyRetry(job.job_id);
        return;
      }
      const currentJob = findByJob(this.store, job.job_id);
      if (currentJob === undefined) return;
      const captured = await this.recordUnknown(currentJob, host);
      const outcomeKey = `${job.job_id}:ui_changed`;
      if (!this.handoffOutcomeSent.has(outcomeKey)) {
        this.handoffOutcomeSent.add(outcomeKey);
        const detail =
          "No source-controlled adapter matched this provider page." +
          (captured ? " A sanitized diagnostic was saved locally for adapter development." : "");
        if (!this.send("provider_outcome", { outcome: "ui_changed", detail }, job.job_id)) {
          this.handoffOutcomeSent.delete(outcomeKey);
        } else {
          await this.settleHandoffAfterOutcome(job.job_id);
        }
      }
      return;
    }
    await this.restoreWorkWindowForAdapter(spec);
    const access = await this.hasEffectiveProviderAccess(host);
    if (access !== true) {
      if (access === false) await this.reportBlockedProviderHost(jobID, host);
      return;
    }
    if (await this.clearBlockedProviderHost(host)) await this.syncConnectionBadge();
    const currentJob = findByJob(this.store, jobID);
    if (!currentJob || (currentJob.status !== "accepted" && currentJob.status !== "awaiting_download")) return;

    let assessmentKind: DrivenPageAssessmentKind | undefined;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: currentJob.tab_id },
        func: assessDrivenPage,
        args: [null],
      });
      const result = results[0]?.result as DrivenPageAssessment | undefined;
      if (result?.kind === "challenge" || result?.kind === "redirect_loop" || result?.kind === "normal") {
        assessmentKind = result.kind;
      }
    } catch (e) {
      console.error("papio: driven-page assessment failed; classifying normally", e);
    }
    if (assessmentKind === "challenge" || assessmentKind === "redirect_loop") {
      await this.blockChallenge(
        currentJob,
        host,
        assessmentKind === "challenge" ? "cloudflare" : "redirect_loop",
      );
      return;
    }
    if (assessmentKind === "normal" && currentJob.challenge_blocked === true) {
      if (!(await this.clearChallengeBlock(currentJob))) return;
    }

    const ctx: AdapterContext = { expected: { ...(currentJob.expected ?? {}) } };
    let verdict: PageVerdict | undefined;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: interpret,
        // interpret(null, spec, ctx): doc arrives null, falls back to the page's
        // document; spec + ctx are the JSON args.
        args: [null, spec, ctx],
      });
      const first = results[0];
      verdict = first ? (first.result as PageVerdict | undefined) : undefined;
    } catch (e) {
      console.error("papio: adapter classification failed; staying assisted", e);
      return;
    }
    if (!verdict) return;
    if (verdict.kind === "unknown") {
      let fallbackKind: ChallengeBlockKind | undefined;
      try {
        const results = await this.deps.scripting.executeScript({
          target: { tabId: job.tab_id },
          func: isBotChallenge,
          args: [null],
        });
        if (results[0]?.result === true) {
          fallbackKind = "cloudflare";
        } else {
          const redirectResults = await this.deps.scripting.executeScript({
            target: { tabId: job.tab_id },
            func: isRedirectLoopPage,
            args: [null],
          });
          if (redirectResults[0]?.result === true) fallbackKind = "redirect_loop";
        }
      } catch (e) {
        // A failed probe must retain the existing stale-adapter path rather
        // than silently make an unreadable provider page immortal.
        console.error("papio: challenge detection failed; classifying normally", e);
      }
      if (fallbackKind !== undefined) {
        await this.waitForBotChallenge(currentJob, host, fallbackKind);
        return;
      }
      if (currentJob.challenge_blocked === true) await this.clearChallengeBlock(currentJob);
    }
    const providerKey = this.providerKeyForJob(currentJob);
    if (await this.completeProviderDrainLease(providerKey)) {
      if (await this.acknowledgePendingProviderHandoffs(providerKey)) {
        await this.releaseQueuedHandoffs();
      }
    }
    await this.applyVerdict(jobID, spec, verdict, host);
    // A decisive verdict ends the render race; `unknown` may just be an
    // un-upgraded page, so retry on a bounded schedule. A latched download-click
    // that opens a declared terms gate must ALSO keep retrying: providers like
    // JSTOR upgrade the terms modal (mfe-*) AFTER the click, so a single
    // post-click classify can miss it. A retry can never start a second
    // download — every download-initiation path bails on download_initiated —
    // so it only serves to catch the terms modal and accept it.
    const after = findByJob(this.store, jobID);
    const awaitingTermsGate =
      spec.termsAccept !== undefined &&
      (after?.status === "accepted" || after?.status === "awaiting_download") &&
      after?.download_initiated === true &&
      !this.downloads.has(jobID);
    if (verdict.kind === "unknown" || (verdict.kind !== "terms" && awaitingTermsGate)) {
      this.scheduleClassifyRetry(jobID);
    } else {
      this.classifyRetries.delete(jobID);
    }
  }

  /** A challenge is a provider-wide human step, not a page retry. Keep its
   * existing tab available and park only siblings with a bounded lease. */
  private async waitForBotChallenge(
    job: ActiveJob,
    host: string,
    kind: ChallengeBlockKind = "cloudflare",
  ): Promise<void> {
    this.classifyRetries.delete(job.job_id);
    await this.blockChallenge(job, host, kind);
  }

  private scheduleClassifyRetry(jobID: string): void {
    const retry = this.classifyRetries.get(jobID);
    const attempts = retry?.kind === "unknown" ? retry.attempts : 0;
    if (attempts >= MAX_CLASSIFY_RETRIES) {
      this.classifyRetries.delete(jobID);
      return;
    }
    const next: ClassifyRetry = { kind: "unknown", attempts: attempts + 1 };
    this.classifyRetries.set(jobID, next);
    this.deps.setTimeout(() => this.retryClassify(jobID, next), CLASSIFY_RETRY_MS);
  }


  /** Registry entries survive only as long as the tab they name; this is the
   * one place that trusts a present entry as proof of a live sibling tab (no
   * extra tabs.get liveness check), so every path that can end that claim's
   * federated-login drive — tab close, navigate off the claimed origin,
   * job removal, a dead restart-time owner — clears its entry through here
   * or one of the two helpers below. Deliberately narrow: nothing here ever
   * fires merely because session evidence landed (recordFreshSessionEvidence
   * does not call this) — an owner still genuinely on the IdP survives even
   * its own institution's evidence, so a resumed waiter can only park behind
   * it again, never open a second tab at the same login page. Successfully
   * clearing an entry always resumes that claim's own waiters
   * (resumeWaitingForSessionByClaim): no path may retire a claim and leave
   * its waiters ownerless. */
  private async clearFederatedLoginOwner(claimKey: string, jobID: string): Promise<void> {
    if (this.store.federatedLoginOwners?.[claimKey]?.jobID !== jobID) return;
    await this.update((s) => {
      const next = { ...(s.federatedLoginOwners ?? {}) };
      delete next[claimKey];
      return { ...s, federatedLoginOwners: next };
    });
    await this.resumeWaitingForSessionByClaim(claimKey);
  }

  /** A closed or navigated-away tab can no longer be the live sibling any
   * waiting job is deferring to. Called for every removed/updated tab; a
   * no-op unless that exact tab is a current registry owner. */
  private async clearFederatedLoginOwnerForTab(tabID: number): Promise<void> {
    const owners = this.store.federatedLoginOwners;
    if (owners === undefined) return;
    const claimKey = Object.keys(owners).find((key) => owners[key]?.tabID === tabID);
    if (claimKey === undefined) return;
    const jobID = owners[claimKey]?.jobID;
    if (jobID === undefined) return;
    await this.clearFederatedLoginOwner(claimKey, jobID);
  }

  /** Safety net for removeJobWithOffer: a job leaving tracking (cancel, reject,
   * cross-provider replacement) is over as a federated-login owner even when
   * its tab is still alive — onTabRemoved handles the tab-closed case, this
   * handles every other way a job stops existing. */
  private async clearFederatedLoginOwnerForJob(jobID: string): Promise<void> {
    const owners = this.store.federatedLoginOwners;
    if (owners === undefined) return;
    const claimKey = Object.keys(owners).find((key) => owners[key]?.jobID === jobID);
    if (claimKey === undefined) return;
    await this.clearFederatedLoginOwner(claimKey, jobID);
  }

  /** Startup validation for federatedLoginOwners, mirroring parked_with_tab's
   * own restart handling: the map is persisted (session storage, beside
   * activeJobs) so a restarted worker does not let every parked sibling race
   * to reclaim a claim a live owner already holds, but the owning job's tab
   * may have closed while this worker was asleep with no onRemoved to catch
   * it. Runs after reconcileTabs, so activeJobs.tab_id already reflects any
   * dead-tab recovery that ran this restart; clearing routes through
   * clearFederatedLoginOwner, so a dead owner's waiters requeue here too,
   * not just its own claim disappearing. */
  private async reconcileFederatedLoginOwners(): Promise<void> {
    const owners = this.store.federatedLoginOwners;
    if (owners === undefined) return;
    for (const [claimKey, owner] of Object.entries(owners)) {
      const ownerJob = findByJob(this.store, owner.jobID);
      if (ownerJob === undefined || ownerJob.tab_id !== owner.tabID) {
        await this.clearFederatedLoginOwner(claimKey, owner.jobID);
      }
    }
  }

  /** Shared drive-queue resume for every job `matches` selects out of the
   * current waiting_for_session parks: enqueue through the same
   * handoffDriveQueue resumeHandoffAfterManual and every re-offer use, so
   * HANDOFF_DRIVE_LIMIT still caps concurrency and no tab is ever
   * activated/focused. purpose "redrive" makes the queue drain navigate each
   * tab back to its own offer URL (drainHandoffDriveQueueUnlocked) before
   * re-registering the drive — the parked page is stale, unlike a manual
   * challenge resume where the human already moved it forward. A job whose
   * tab closed while parked never reaches here: onTabRemoved demotes it to
   * an ordinary queued handoff instead, released the same way any other
   * queued job is. enqueueHandoffDrive's own clearParkedMarker call drops
   * both park markers here too, the moment intent to resume is expressed —
   * a sibling still behind the governor is no longer marked parked, only
   * still tab_id-tracked and auth_pending, until drainHandoffDriveQueueUnlocked
   * actually claims its slot. Repeated/duplicate calls are idempotent: once
   * a job's intent to resume is expressed it drops out of every future
   * matches() scan (waiting_for_session is false), so a second call finds
   * nothing left to enqueue for it.
   *
   * A candidate whose claim key STILL has a live owner is skipped
   * regardless of `matches`: a present federatedLoginOwners entry IS the
   * proof that owner's tab has neither closed nor left the claimed origin
   * (clearFederatedLoginOwner/clearFederatedLoginOwnerForTab retire it the
   * instant either happens), so resuming here could only re-park the exact
   * same job a moment later — zero navigations and zero drive slots wasted
   * on churn from a keepalive probe repeating every few seconds while a
   * sign-in is genuinely still in progress. That job's real resume is the
   * retirement chokepoint (the moment the owner truly leaves), never a
   * repeated evidence/landing event.
   *
   * A candidate whose OWN waiting_deadline has already passed is likewise
   * never resumed here — demoted instead, the same synchronous transition
   * armSessionWaitTimeout's own expiry and reconcileSessionWaitTimeouts use.
   * Startup runs reconcileFederatedLoginOwners (which can retire a dead
   * owner's claim and land here) before reconcileSessionWaitTimeouts (which
   * would otherwise have demoted the same expired waiter moments later): a
   * claim retiring and a waiter's own deadline expiring can coincide at
   * exactly the same restart, and whichever runs first must not hand that
   * waiter a navigation and a drive slot its own timeout already promised
   * it would never get, nor leave it holding an already-spent deadline. */
  private async resumeWaitingForSessionJobs(matches: (job: ActiveJob) => boolean): Promise<void> {
    const now = this.deps.now();
    for (const job of this.store.activeJobs) {
      if (job.waiting_for_session !== true || job.tab_id < 0 || !matches(job)) continue;
      if (
        job.waiting_for_session_key !== undefined &&
        this.store.federatedLoginOwners?.[job.waiting_for_session_key] !== undefined
      ) {
        continue;
      }
      if (job.waiting_deadline !== undefined && now >= job.waiting_deadline) {
        this.waitingForSessionTimers.delete(job.job_id);
        await this.update((s) =>
          patchJob(s, job.job_id, {
            waiting_for_session: false,
            waiting_for_session_key: undefined,
            waiting_deadline: undefined,
          }),
        );
        continue;
      }
      this.enqueueHandoffDrive({
        jobID: job.job_id,
        purpose: "redrive",
        focusExisting: false,
        surfaceFallback: false,
      });
    }
    await this.drainHandoffDriveQueue();
  }

  /** Institution-scoped resume: every waiting_for_session job whose OWN
   * offer resolves to `origin`, regardless of which specific claim key it is
   * parked on. Used when the evidence for an origin is broader than any one
   * claim (recordFreshSessionEvidence, recordInstitutionalSession). */
  private async resumeWaitingForSessionHandoffs(origin: string): Promise<void> {
    await this.resumeWaitingForSessionJobs(
      (job) => this.resolverOriginHint(this.offerURLs.get(job.job_id)) === origin,
    );
  }

  /** Claim-scoped resume: every waiting_for_session job parked on this exact
   * claim key, regardless of its own offer origin resolving cleanly. Used
   * when a specific claim retires (clearFederatedLoginOwner) — the owner
   * job's own data may already be gone (removed, restart-dead), so this
   * never depends on it: the waiters carry their own claim key. */
  private async resumeWaitingForSessionByClaim(claimKey: string): Promise<void> {
    await this.resumeWaitingForSessionJobs((job) => job.waiting_for_session_key === claimKey);
  }

  /** Auto-select the institution on a provider login wall: navigate the handoff
   * tab to the adapter's federated-login entry with the offer's entityID, once
   * per drive. Institution selection is deterministic config, not a secret; the
   * human still enters credentials at the IdP. No-op without a configured route,
   * a known entityID, or a `tabs.update` seam, and never re-navigates mid
   * sign-in (latched, cleared on job removal).
   *
   * ONE login tab per institution: before navigating, check
   * federatedLoginOwners for the claim key this template+entityID resolves
   * to — the destination origin ALONE is not the institution: a shared
   * WAYF/Discovery-Service host serving many institutions exposes exactly
   * one origin for all of them, distinguished only by entityID in the query
   * (the real ProQuest adapter is exactly this shape), so the claim key is
   * origin+entityID, never origin alone. An already-live sibling tab holding
   * that claim means a second tab at the same login page would teach the
   * human nothing, so this job parks instead (waiting_for_session) and
   * resumes when that claim retires or fresh institution evidence lands.
   * Otherwise this job claims the key and becomes that live tab for any
   * siblings that arrive next. */
  private async maybeRouteFederatedLogin(jobID: string, job: ActiveJob, spec: AdapterSpec): Promise<void> {
    const template = spec.federatedLogin;
    const entityID = this.loginEntityIDs.get(jobID);
    if (template === undefined || entityID === undefined) return;
    if (this.federatedLoginRouted.has(jobID)) return;
    if (this.deps.tabs.update === undefined) return;
    const url = template.replace("{entityID}", encodeURIComponent(entityID));
    if (!url.startsWith("https://")) return;
    let idpOrigin: string;
    try {
      idpOrigin = new URL(url).origin;
    } catch {
      return;
    }
    // JSON-encoded pair, not a plain join: entityID is itself a URL and may
    // contain any separator a hand-picked delimiter could collide with.
    const claimKey = JSON.stringify([idpOrigin, entityID]);
    const owner = this.store.federatedLoginOwners?.[claimKey];
    if (owner !== undefined && owner.jobID !== jobID) {
      await this.parkHandoffWaitingForSession(jobID, claimKey);
      return;
    }
    this.federatedLoginRouted.add(jobID);
    await this.update((s) => ({
      ...s,
      federatedLoginOwners: { ...(s.federatedLoginOwners ?? {}), [claimKey]: { jobID, tabID: job.tab_id } },
    }));
    try {
      await this.deps.tabs.update(job.tab_id, { url });
    } catch (e) {
      // Let a later classify retry route again if this navigation failed.
      this.federatedLoginRouted.delete(jobID);
      await this.clearFederatedLoginOwner(claimKey, jobID);
      console.error("papio: federated login route failed", e);
    }
  }

  /** Unlock a provider's openurl link-resolver by appending its institutional
   * account id (ProQuest: ?accountid=<id>) to the current tab URL — fully
   * autonomous, no sign-in. Returns true if it navigated. No-op without a
   * configured param/account id or a `tabs.update` seam, if the current URL
   * already carries the param, or if already appended this drive (latched). */
  private async maybeAppendAccountId(jobID: string, job: ActiveJob, spec: AdapterSpec): Promise<boolean> {
    const param = spec.accountIdParam;
    const accountID = this.proquestAccountIDs.get(jobID);
    if (param === undefined || accountID === undefined) return false;
    if (this.accountIdAppended.has(jobID)) return false;
    if (this.deps.tabs.update === undefined) return false;
    let current: string;
    try {
      current = (await this.deps.tabs.get(job.tab_id)).url ?? "";
    } catch {
      return false;
    }
    if (!current.startsWith("https://")) return false;
    const url = new URL(current);
    if (url.searchParams.get(param) === accountID) return false;
    url.searchParams.set(param, accountID);
    this.accountIdAppended.add(jobID);
    try {
      await this.deps.tabs.update(job.tab_id, { url: url.toString() });
      return true;
    } catch (e) {
      this.accountIdAppended.delete(jobID);
      console.error("papio: account-id unlock failed", e);
      return false;
    }
  }

  private async retryClassify(jobID: string, expected?: ClassifyRetry): Promise<void> {
    await this.ready;
    if (expected !== undefined && this.classifyRetries.get(jobID) !== expected) return;
    const job = findByJob(this.store, jobID);
    // Stop once the job is gone or an actual download is tracked. The guard is
    // the tracked download, NOT download_initiated: a click that latched to open
    // a terms gate has download_initiated=true but no download yet, and the
    // retry must continue so a late-upgrading terms modal is caught. No download
    // can fire twice — every initiation path still bails on download_initiated.
    if (!job || this.downloads.has(jobID)) {
      this.classifyRetries.delete(jobID);
      return;
    }
    if (job.status !== "accepted" && job.status !== "awaiting_download") {
      this.classifyRetries.delete(jobID);
      return;
    }
    await this.reclassifyCurrentProviderPage(jobID);
  }


  private async reclassifyCurrentProviderPage(jobID: string, allowUnregistered = false): Promise<void> {
    const job = findByJob(this.store, jobID);
    // A queued handoff (tab_id -1) has no page yet, and a closed one never
    // will: normal tab-removal recovery stays authoritative. Callers include a
    // bare classify-retry timer, so a throw here would escape unhandled.
    if (!job || job.tab_id < 0) return;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(job.tab_id);
    } catch {
      return;
    }
    if (tab.url === undefined || isAuthenticationURL(tab.url)) return;
    let host: string;
    try {
      host = new URL(tab.url).hostname;
    } catch {
      return;
    }
    const onRegisteredProvider =
      hostMatches(host, job.provider_hosts) ||
      this.deps.adapterSpecs.some((candidate) => hostMatches(host, candidate.hosts));
    const continuingUnregisteredLanding = allowUnregistered || job.last_unknown_ms !== undefined;
    if (!onRegisteredProvider && !continuingUnregisteredLanding) return;
    await this.maybeClassify(jobID, host);
  }

  /** Record a development capture for an unknown page. The caller decides
   * whether to remain assisted or report a terminal coverage gap. */
  private async recordUnknown(job: ActiveJob, host: string, adapter?: AdapterSpec): Promise<boolean> {
    let captured = false;
    const captureStorage = this.deps.captureStorage;
    if (captureStorage !== undefined && this.pageCaptureAvailable()) {
      captured = await observeUnknown(
        {
          scripting: this.deps.scripting as ObserveChromeApi["scripting"],
          storage: captureStorage,
          sendPageCapture: (payload, jobID) => this.sendPageCapture(payload, jobID),
        },
        job,
        host,
        {
          verifiedHosts:
            adapter === undefined
              ? [...job.provider_hosts, host]
              : [...job.provider_hosts, ...adapter.hosts],
          ...(adapter === undefined
            ? {}
            : { adapterID: adapter.id, adapterVersion: adapter.version }),
        },
        () => new Date(this.deps.now()),
      );
    }
    if (adapter === undefined) return captured;
    const now = this.deps.now();
    const count = job.unknown_count ?? 0;
    const last = job.last_unknown_ms ?? 0;
    if (count >= 1 && now - last >= 5000) {
      // Retries wait for one document to render; they are not independent
      // provider failures, so one broker drive gets one terminal observation.
      const outcomeKey = `${job.job_id}:ui_changed`;
      if (!this.handoffOutcomeSent.has(outcomeKey)) {
        this.handoffOutcomeSent.add(outcomeKey);
        if (
          !this.send("provider_outcome", { outcome: "ui_changed", adapter_version: adapter.version }, job.job_id)
        ) {
          this.handoffOutcomeSent.delete(outcomeKey);
        } else {
          await this.settleHandoffAfterOutcome(job.job_id);
        }
      }
    } else if (count === 0) {
      await this.update((s) => patchJob(s, job.job_id, { unknown_count: 1, last_unknown_ms: now }));
    }
    return captured;
  }

  /** Map a page verdict to a bridge action. See the safety contract: at most one
   * download initiation per job, ever; unknown only escalates after two spaced
   * observations; every other unknown keeps assisted behaviour. */
  private async settleHandoffAfterOutcome(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    if (job.tab_id >= 0) await this.closeManagedHandoffTab(job, job.tab_id);
    this.releaseHandoffDrive(jobID);
    await this.removeJobWithOffer(jobID);
  }

  private async applyVerdict(jobID: string, spec: AdapterSpec, verdict: PageVerdict, host: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (!job) return;
    const av = spec.version;

    if (verdict.kind !== "unknown" && (job.unknown_count ?? 0) !== 0) {
      // Any decisive verdict breaks the unknown streak.
      await this.update((s) => patchJob(s, jobID, { unknown_count: 0 }));
    }

    switch (verdict.kind) {
      case "article": {
        const dl = spec.download;
        if (
          dl &&
          job.download_initiated !== true &&
          !(dl.method === "click" && this.deps.downloads.onDeterminingFilename === undefined)
        ) {
          if ((dl.method === "url" || dl.method === "api" || dl.method === "meta") && dl.requiresTermsConsent === true) {
            const consent = await this.deps.settings.getTermsConsent();
            if (consent !== "accept") {
              // The direct-endpoint fetch bypasses the publisher terms UI, so
              // gate it on recorded consent to auto-accept terms. Without
              // consent, prompt once and stay assisted — no fetch, no latch.
              this.send("provider_outcome", { outcome: "terms_acceptance_required", adapter_version: av }, jobID);
              if (consent === undefined) {
                await this.update((s) => patchJob(s, jobID, { needs_terms_consent: true }));
              }
              return;
            }
          }
          // Consent is an await boundary shared by concurrent classifications.
          // Re-read the durable latch before this synchronous update claims the
          // job, so only one classifier can initiate the download.
          const latestJob = findByJob(this.store, jobID);
          if (latestJob === undefined || latestJob.download_initiated === true) return;
          // Latch BEFORE resolving/downloading (persisted) so no
          // re-classification can ever initiate a second download for this
          // job. Failure falls back to assisted mode; the user can still use
          // the verified page control manually.
          await this.update((s) =>
            patchJob(s, jobID, { download_initiated: true, adapter_id: spec.id }),
          );
          try {
            if (dl.method === "click") {
              const results = await this.deps.scripting.executeScript({
                target: { tabId: job.tab_id },
                func: clickDeclaredDownload,
                args: [
                  dl.selector,
                  dl.shadowSelector ?? null,
                  dl.postClickWaitFor ?? null,
                  dl.postClickTimeoutMs ?? null,
                  dl.followupSelector ?? null,
                ],
              });
              const clicked = results[0]?.result === true;
              if (clicked && dl.postClickWaitFor !== undefined) {
                await this.reclassifyCurrentProviderPage(jobID);
              }
            } else if (dl.method === "url" || dl.method === "api") {
              const built = await this.deps.scripting.executeScript({
                target: { tabId: job.tab_id },
                func: resolveDownloadURL,
                args: [dl.selector, dl.idPattern ?? null, dl.urlTemplate ?? null, dl.jsonField ?? null],
              });
              const url = built[0]?.result;
              if (typeof url === "string" && url.startsWith("https://")) {
                this.pendingDownloadURLs.set(url, jobID);
                try {
                  const id = await this.deps.downloads.download({
                    url,
                    filename: `papio/${jobID}/paper.pdf`,
                    conflictAction: "uniquify",
                    saveAs: false,
                  });
                  this.downloads.set(jobID, { ids: new Set([id]), ambiguous: false, directOffer: false });
                } finally {
                  this.pendingDownloadURLs.delete(url);
                }
              }
            } else if (dl.method === "meta") {
              const metas = await this.deps.scripting.executeScript({
                target: { tabId: job.tab_id },
                func: extractMetaURL,
                args: [dl.metaName ?? "citation_pdf_url"],
              });
              const url = metas[0]?.result;
              if (typeof url === "string" && url.startsWith("https://")) {
                this.pendingDownloadURLs.set(url, jobID);
                try {
                  const id = await this.deps.downloads.download({
                    url,
                    filename: `papio/${jobID}/paper.pdf`,
                    conflictAction: "uniquify",
                    saveAs: false,
                  });
                  this.downloads.set(jobID, { ids: new Set([id]), ambiguous: false, directOffer: false });
                } finally {
                  this.pendingDownloadURLs.delete(url);
                }
              }
            } else {
              const links = await this.deps.scripting.executeScript({
                target: { tabId: job.tab_id },
                func: extractDownloadURL,
                args: [dl.selector],
              });
              const href = links[0]?.result;
              if (typeof href === "string" && href.startsWith("https://")) {
                this.pendingDownloadURLs.set(href, jobID);
                try {
                  const id = await this.deps.downloads.download({
                    url: href,
                    filename: `papio/${jobID}/paper.pdf`,
                    conflictAction: "uniquify",
                    saveAs: false,
                  });
                  // Correlate by Chrome's returned ID, not URL/referrer
                  // heuristics. onChanged can now complete even if onCreated
                  // raced the Promise.
                  this.downloads.set(jobID, { ids: new Set([id]), ambiguous: false, directOffer: false });
                } finally {
                  this.pendingDownloadURLs.delete(href);
                }
              }
            }
          } catch (e) {
            console.error("papio: adapter download initiation failed; staying assisted", e);
          }
          // No synthesized frames: the real Chrome download flows through the
          // onChanged listener, which emits download_started/complete.
        }
        return;
      }
      case "login":
        // A provider login wall. If the adapter has a federated-login route and
        // the offer carried the institution entityID, auto-select the institution
        // by navigating the handoff tab straight to the IdP (skipping the
        // provider's picker); the human still enters credentials there. Then stay
        // auth_pending, emit nothing.
        // Prefer the autonomous account-id unlock; fall back to federated login.
        if (!(await this.maybeAppendAccountId(jobID, job, spec))) {
          await this.maybeRouteFederatedLogin(jobID, job, spec);
        }
        return;
      case "terms": {
        const consent = await this.deps.settings.getTermsConsent();
        if (consent === "accept" && spec.termsAccept) {
          const accepted = await this.acceptTerms(job.job_id, spec.termsAccept);
          if (accepted) {
            // The accept click opens the provider PDF (often in a new viewer
            // tab), which the download / viewer-adoption path captures and
            // reports as download_started/complete. No extra frame: the
            // frozen protocol has no terms-accepted outcome, and the download
            // events are the audit trail.
            return;
          }
        }
        this.send("provider_outcome", { outcome: "terms_acceptance_required", adapter_version: av }, jobID);
        // First terms gate with no recorded choice: flag for the popup's
        // one-time informed-consent prompt.
        if (consent === undefined) {
          await this.update((s) => patchJob(s, jobID, { needs_terms_consent: true }));
        }
        return;
      }
      case "no_entitlement":
        if (this.send("provider_outcome", { outcome: "no_entitlement", adapter_version: av }, jobID)) {
          await this.settleHandoffAfterOutcome(jobID);
        }
        return;
      case "wrong_work":
      case "wrong_work_check":
        if (this.send("provider_outcome", { outcome: "wrong_work", adapter_version: av }, jobID)) {
          await this.settleHandoffAfterOutcome(jobID);
        }
        return;
      case "unknown":
        await this.recordUnknown(job, host, spec);
        return;
      }
  }

  /** Chrome may not have populated the tab's URL by the time onActivated
   * fires (e.g. a brand-new tab still loading its first document); look it
   * up rather than trust the event payload. A tab that vanished between the
   * event and the lookup is not evidence of anything — swallow and drop. */
  private async onTabActivated(tabID: number): Promise<void> {
    let url: string | undefined;
    try {
      url = (await this.deps.tabs.get(tabID)).url;
    } catch {
      return;
    }
    this.keepaliveManager?.noteResolverActivated(tabID, url);
  }

  private async onTabRemoved(tabID: number): Promise<void> {
    await this.ready;
    const pageCaptureWaiter = this.pageCaptureLoadWaiters.get(tabID);
    if (pageCaptureWaiter !== undefined) pageCaptureWaiter(false);
    this.authCountedTabs.delete(tabID);
    void this.forgetLedgeredTab(tabID);
    // A closed tab can no longer be the live sibling any waiting job is
    // deferring to — regardless of whether this was a programmatic close
    // (below) or a genuine user close (further down).
    await this.clearFederatedLoginOwnerForTab(tabID);
    if (this.closingTabs.delete(tabID)) {
      await this.drainHandoffDriveQueue();
      return;
    } // programmatic close, not a user cancel
    const job = findByTab(this.store, tabID);
    if (!job) return;
    this.releaseHandoffDrive(job.job_id);
    // A tab parked only because a SIBLING job owns the shared login tab
    // (waiting_for_session) never had a chance to sign in on its own — its
    // tab closing is not the operator abandoning the job, just losing the
    // page it was quietly waiting on. Re-enter it as an ordinary queued
    // drive (exactly reconcileTabs's dead-pre-download-tab recovery) so
    // recordFreshSessionEvidence's existing queued-release path — not a
    // second, redundant resume mechanism — is what reopens it.
    if (job.waiting_for_session === true) {
      this.waitingForSessionTimers.delete(job.job_id);
      this.beginProviderDrive(job.job_id);
      await this.update((s) =>
        patchJob(s, job.job_id, {
          tab_id: -1,
          status: "queued",
          waiting_for_session: false,
          waiting_for_session_key: undefined,
          parked_with_tab: false,
          download_initiated: false,
          unknown_count: 0,
        }),
      );
      this.scheduleQueuedHandoffRelease(job.job_id);
      return;
    }
    if (this.deliveryJobs.has(job.job_id) || this.store.pendingDelivery?.job_id === job.job_id) {
      // The browser download is independent of the source tab. Keep its exact
      // correlation and pending record alive when the operator closes the PDF.
      await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
      await this.drainHandoffDriveQueue();
      return;
    }
    // Once the user is past authentication (awaiting_download), a closed tab is
    // NOT a cancel: a download may be in flight or already saved into the job's
    // adoption directory, where the daemon's poll-scan adopts it. Park it, as
    // onTabRemoved would have.
    if (job.status === "awaiting_download") {
      this.completedDownloadTabs.delete(job.job_id);
      await this.removeJobWithOffer(job.job_id);
      return;
    }
    this.send("provider_outcome", { outcome: "cancelled" }, job.job_id);
    this.downloads.delete(job.job_id);
    this.completedDownloadTabs.delete(job.job_id);
    await this.removeJobWithOffer(job.job_id);
  }

  private correlate(item: DownloadItemLike): ActiveJob | undefined {
    // Firefox cannot relocate native/manual downloads into papio/<job>. Only
    // exact IDs/URLs registered by downloads.download are safe there; those
    // bypass this broad tab/host correlation path.
    if (this.deps.downloads.onDeterminingFilename === undefined) return undefined;
    if (typeof item.tabId === "number") {
      const byTab = findByTab(this.store, item.tabId);
      if (byTab) {
        if (this.isFirefoxClickDownload(byTab)) return undefined;
        return byTab;
      }
      // extension did not create; host matching below requires an advertised
      // provider host or the job's persisted registry adapter.
    }
    const src = item.referrer ?? item.finalUrl ?? item.url;
    if (src === undefined || src.length === 0) return undefined;
    let host: string;
    try {
      host = new URL(src).hostname;
    } catch {
      return undefined;
    }
    const initiated = this.store.activeJobs.filter((job) => {
      if (this.isFirefoxClickDownload(job) || job.download_initiated !== true || job.adapter_id === undefined) return false;
      const spec = this.deps.adapterSpecs.find((candidate) => candidate.id === job.adapter_id);
      return spec !== undefined && hostMatches(host, spec.hosts);
    });
    if (initiated.length === 1) return initiated[0];
    if (initiated.length > 1) return undefined;
    const matches = this.store.activeJobs.filter(
      (job) => !this.isFirefoxClickDownload(job) && this.matchesManualDownloadHost(job, host),
    );
    return matches.length === 1 ? matches[0] : undefined;
  }

  private async onDownloadCreated(item: DownloadItemLike): Promise<void> {
    const pendingGrab = this.pendingGrabFor(item);
    if (pendingGrab !== undefined) {
      pendingGrab.ids.add(item.id);
      this.grabDownloads.set(this.trackedGrabFor(item.id) ?? "", pendingGrab);
      return;
    }
    const earlyJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    if (earlyJobID !== undefined) {
      const early = this.downloads.get(earlyJobID) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
      early.ids.add(item.id);
      if (early.ids.size > 1) early.ambiguous = true;
      this.downloads.set(earlyJobID, early);
    }
    await this.ready;
    const exactJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    const job = exactJobID === undefined ? this.correlate(item) : findByJob(this.store, exactJobID);
    if (!job) return;
    if (job.download_initiated !== true) {
      await this.update((s) => patchJob(s, job.job_id, { download_initiated: true }));
    }
    const track = this.downloads.get(job.job_id) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
    track.ids.add(item.id);
    if (track.ids.size > 1) track.ambiguous = true;
    this.downloads.set(job.job_id, track);
  }

  private async onDownloadChanged(delta: DownloadDeltaLike): Promise<void> {
    await this.ready;
    const state = delta.state?.current;
    const grabID = this.trackedGrabFor(delta.id);
    if (grabID !== undefined) {
      const grab = this.grabDownloads.get(grabID);
      if (grab !== undefined) {
        if (state === "interrupted") {
          this.notifyPdfGrab(grab.scanID, grabID, "failed", "The PDF grab download was interrupted");
          this.grabDownloads.delete(grabID);
          this.pdfGrabCorrelations.delete(grabID);
          this.persistPdfGrabCorrelations();
          return;
        }
        if (state === "complete") {
          const correlation = this.pdfGrabCorrelations.get(grabID);
          if (correlation !== undefined) {
            correlation.state = "identifying";
            this.persistPdfGrabCorrelations();
          }
          this.notifyPdfGrab(grab.scanID, grabID, "identifying");
        }
      }
    }
    if (state !== "complete") {
      if (state === "interrupted") {
        for (const job of this.store.activeJobs) {
          const track = this.downloads.get(job.job_id);
          if (track?.delivery === true && track.ids.has(delta.id)) {
            await this.failDelivery(job.job_id, delta.id, "The PDF download was interrupted");
            return;
          }
          if (track?.directOffer === true && track.ids.has(delta.id)) {
            await this.discardDirectOffer(job.job_id, delta.id);
            return;
          }
        }
      }
      return;
    }
    let owner: ActiveJob | undefined;
    let track: DownloadTrack | undefined;
    for (const job of this.store.activeJobs) {
      const candidate = this.downloads.get(job.job_id);
      if (candidate && candidate.ids.has(delta.id)) {
        owner = job;
        track = candidate;
        break;
      }
    }
    if (!owner || !track) return;
    if (track.ambiguous || track.ids.size !== 1) return; // zero or multiple matches: stay with the user
    const found = await this.deps.downloads.search({ id: delta.id });
    const item = found[0];
    const mime = item?.mime?.split(";", 1)[0]?.trim().toLowerCase();
    if (track.delivery === true) {
      if (mime !== "application/pdf") {
        await this.failDelivery(owner.job_id, delta.id, "Downloaded file was not a PDF — job stays in your inbox");
        return;
      }
    } else if (track.directOffer) {
      if (mime !== "application/pdf") {
        await this.discardDirectOffer(owner.job_id, delta.id);
        return;
      }
    } else if (mime === "text/html" || mime === "application/xhtml+xml") {
      // The provider served a web page where the PDF should be — the classic
      // no-entitlement wrapper (SAGE "get access"). Adopting it would only
      // bounce off the daemon's %PDF validation and burn a round trip, so
      // refuse here, discard the file, and tell the daemon why. The job stays
      // parked with its human actions; the tab stays for the human.
      await this.discardDownload(owner.job_id, delta.id);
      this.send(
        "error",
        { code: "download_not_pdf", message: "provider served HTML where a PDF was expected (likely no entitlement)" },
        owner.job_id,
      );
      return;
    }
    if (!item) return;
    const rawName = item.filename ?? delta.filename?.current ?? "";
    const filename = rawName.split(/[\\/]/).pop() ?? "";
    const size = item.fileSize ?? item.totalBytes ?? item.bytesReceived ?? 0;
    if (filename.length === 0 || size < 1) return; // cannot form a valid frame; leave to the user

    await this.update((s) => {
      const next = this.clearAuthAttempts(patchJob(s, owner.job_id, { status: "awaiting_download" }), owner.job_id);
      return track.delivery === true
        ? updatePendingDelivery(next, owner.job_id, { status: "downloaded" })
        : next;
    });
    this.authStalledReported.delete(owner.job_id);
    this.stalledAuthHandoffs.delete(owner.job_id);
    const route = this.deliveryRouteFor(owner, track);
    const sessionEvidence = this.deliveryEvidenceFor(owner, track, route);
    const pageHost = await this.deliveryPageHost(owner, item, track);
    this.send("download_started", { download_id: delta.id, filename }, owner.job_id);
    this.send("download_complete", { download_id: delta.id, filename, size_bytes: size }, owner.job_id);
    if (
      this.store.connectionStatus === "connected" &&
      (this.store.daemonFeatures ?? []).includes(DELIVERY_CONTEXT_FEATURE)
    ) {
      this.send(
        "delivery_context",
        {
          download_id: delta.id,
          route,
          session_evidence: sessionEvidence,
          ...(pageHost !== undefined ? { page_host: pageHost } : {}),
        },
        owner.job_id,
      );
    }
    this.completedDownloadTabs.set(owner.job_id, owner.tab_id);
    this.downloads.delete(owner.job_id);
  }
}

interface CancelRequest {
  channel: "papio";
  action: "cancel";
  job_id: string;
}

function isCancelRequest(message: unknown): message is CancelRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "cancel" &&
    "job_id" in message &&
    typeof message.job_id === "string"
  );
}

interface PageAcquireRequest {
  channel: "papio";
  action: "page_acquire";
  payload: PageAcquirePayload;
}

function isPageAcquireRequest(message: unknown): message is PageAcquireRequest {
  if (
    typeof message !== "object" ||
    message === null ||
    !("channel" in message) ||
    message.channel !== "papio" ||
    !("action" in message) ||
    message.action !== "page_acquire" ||
    !("payload" in message) ||
    typeof message.payload !== "object" ||
    message.payload === null ||
    Array.isArray(message.payload)
  ) {
    return false;
  }
  const payload = message.payload as Record<string, unknown>;
  if (!Object.keys(payload).every((key) => key === "url" || key === "doi" || key === "title" || key === "source")) {
    return false;
  }
  return (
    typeof payload.url === "string" &&
    (payload.doi === undefined || typeof payload.doi === "string") &&
    (payload.title === undefined || typeof payload.title === "string") &&
    (payload.source === undefined || typeof payload.source === "string")
  );
}

interface CapabilitiesRequest {
  channel: "papio";
  action: "get_capabilities";
}

function isCapabilitiesRequest(message: unknown): message is CapabilitiesRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "get_capabilities"
  );
}

interface TermsConsentRequest {
  channel: "papio";
  action: "terms_consent";
  value: "accept" | "manual";
}

function isTermsConsentRequest(message: unknown): message is TermsConsentRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "terms_consent" &&
    "value" in message &&
    (message.value === "accept" || message.value === "manual")
  );
}

interface OrphanTabsRequest {
  channel: "papio";
  action: "orphan_tabs_status" | "orphan_tabs_cleanup";
}

function isOrphanTabsRequest(message: unknown): message is OrphanTabsRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    (message.action === "orphan_tabs_status" || message.action === "orphan_tabs_cleanup")
  );
}

interface InboxRuntimeSender {
  id?: string | undefined;
  url?: string | undefined;
  tab?: { id?: number | undefined } | undefined;
}

interface InboxRuntimeURLs {
  runtimeID: string;
  inboxURL: string;
  popupURL: string;
  historyURL: string;
  /** ADR-0019 Decision 4: addressed `?scan=<id>`, so exact-sender checks
   * compare origin+pathname only — never the full URL — for this one page. */
  pageBulkURL: string;
}

type InboxRuntimeReply =
  | BrokerFailure
  | { opened: true }
  | { captured: true }
  | { ok: true }
  | BrokerReply<{ snapshot: Record<string, unknown> }>
  | BrokerReply<{ counts: Record<string, unknown>; generated_at: string }>
  | BrokerReply<{ outcome: string; detail?: string }>
  | BrokerReply<{ opened: true }>
  | BrokerReply<{ stats: Record<string, unknown> }>
  | BrokerReply<{ feature: boolean; entries: ActivityEntryPayload[] }>
  | BrokerReply<{ state: BridgeSessionState; origins: KeepaliveOriginSnapshot[] }>
  | BrokerReply<{ scan_id: string }>
  | BrokerReply<{ snapshot: PageBulkSnapshot }>
  | BrokerReply<{ items: PageBulkStatusItem[]; truncated: boolean }>
  | BrokerReply<{ submitted: number; joined: number; already_owned: number; invalid: number; batch_id: string }>
  | BrokerReply<{ allowed: boolean }>
  | DeliveryReply;

function isObjectRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function isBareHTTPSOrigin(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > 300) return false;
  try {
    const parsed = new URL(value);
    return (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === "" &&
      parsed.pathname === "/" &&
      parsed.search === "" &&
      parsed.hash === "" &&
      parsed.host !== "" &&
      `${parsed.protocol}//${parsed.host}` === value
    );
  } catch {
    return false;
  }
}

function hasOnlyKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).every((key) => keys.includes(key));
}

function isPositiveSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 1;
}

function isInboxSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  return sender.id === urls.runtimeID && sender.url === urls.inboxURL;
}


function isPopupSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  return sender.id === urls.runtimeID && sender.url === urls.popupURL;
}
function isInboxOrPopupSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  return sender.id === urls.runtimeID && (sender.url === urls.inboxURL || sender.url === urls.popupURL);
}

/** ADR-0019 Decision 4: the workspace is addressed `?scan=<id>`, so the
 * exact-page check compares origin+pathname only, ignoring that query. */
function isPageBulkSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  if (sender.id !== urls.runtimeID || sender.url === undefined) return false;
  try {
    const senderURL = new URL(sender.url);
    const pageURL = new URL(urls.pageBulkURL);
    return senderURL.origin === pageURL.origin && senderURL.pathname === pageURL.pathname;
  } catch {
    return false;
  }
}

// Stats is a read consumed by the popup summary and the history page as well
// as the inbox, so it accepts any of papio's own extension pages — never a
// content script or a foreign extension.
function isStatsSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  return sender.id === urls.runtimeID &&
    (sender.url === urls.inboxURL || sender.url === urls.popupURL || sender.url === urls.historyURL);
}

function runtimeFailure(code: string, message: string): BrokerFailure {
  return { ok: false, error: { code, message } };
}

function isSnapshotRuntimeRequest(
  value: unknown,
): value is { schema_versions: [1]; limit?: number; cursor?: string } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["schema_versions", "limit", "cursor"])) return false;
  const versions = value["schema_versions"];
  return (
    Array.isArray(versions) &&
    versions.length === 1 &&
    versions[0] === 1 &&
    (value["limit"] === undefined || (isPositiveSafeInteger(value["limit"]) && value["limit"] <= 100)) &&
    (value["cursor"] === undefined || (typeof value["cursor"] === "string" && value["cursor"].length <= 256))
  );
}

function isCountsRuntimeRequest(value: unknown): value is Record<string, never> {
  return isObjectRecord(value) && Object.keys(value).length === 0;
}

function isActivityRuntimeRequest(value: unknown): value is { limit?: number } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["limit"])) return false;
  return value["limit"] === undefined || (isPositiveSafeInteger(value["limit"]) && value["limit"] <= 50);
}

function isDecisionRuntimeRequest(
  value: unknown,
): value is { item_id: string; op: "acquire" | "dismiss"; watch_scope?: "all" | number[] } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["item_id", "op", "watch_scope"])) return false;
  const itemID = value["item_id"];
  const op = value["op"];
  const watchScope = value["watch_scope"];
  if (typeof itemID !== "string" || itemID.length === 0 || itemID.length > 1024) return false;
  if (op !== "acquire" && op !== "dismiss") return false;
  if (op === "acquire") return watchScope === undefined;
  if (watchScope === "all") return true;
  if (!Array.isArray(watchScope) || watchScope.length < 1 || watchScope.length > 100) return false;
  const ids = new Set<number>();
  for (const id of watchScope) {
    if (!isPositiveSafeInteger(id) || ids.has(id)) return false;
    ids.add(id);
  }
  return true;
}

function isResolveRuntimeRequest(
  value: unknown,
): value is { action_id: number; verdict: "accept" | "reject" | "dismiss"; expected_revision: number; expected_sha256?: string } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["action_id", "verdict", "expected_revision", "expected_sha256"])) {
    return false;
  }
  const verdict = value["verdict"];
  const expectedSHA = value["expected_sha256"];
  if (
    !isPositiveSafeInteger(value["action_id"]) ||
    !isPositiveSafeInteger(value["expected_revision"]) ||
    (verdict !== "accept" && verdict !== "reject" && verdict !== "dismiss")
  ) {
    return false;
  }
  if (verdict === "accept" && typeof expectedSHA !== "string") return false;
  return expectedSHA === undefined || (typeof expectedSHA === "string" && /^[a-f0-9]{64}$/.test(expectedSHA));
}

function isPreviewRuntimeRequest(value: unknown): value is { action_id: number } {
  return isObjectRecord(value) && hasOnlyKeys(value, ["action_id"]) && isPositiveSafeInteger(value["action_id"]);
}

function isDeliveryReconcileRuntimeRequest(
  value: unknown,
): value is { job_id: string; operation: "confirm_request_exists" | "confirm_request_absent"; provider_reference?: string } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["job_id", "operation", "provider_reference"])) return false;
  const jobID = value["job_id"];
  const operation = value["operation"];
  const providerReference = value["provider_reference"];
  if (typeof jobID !== "string" || jobID.length === 0 || jobID.length > 128) return false;
  if (operation !== "confirm_request_exists" && operation !== "confirm_request_absent") return false;
  if (operation === "confirm_request_exists") {
    return typeof providerReference === "string" && providerReference.length > 0 && providerReference.length <= 300;
  }
  return providerReference === undefined;
}

function isPageBulkScanRuntimeRequest(value: unknown): value is { tab_id: number } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["tab_id"]) &&
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0
  );
}

function isPageBulkRescanRuntimeRequest(value: unknown): value is { scan_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["scan_id"]) &&
    typeof value["scan_id"] === "string" &&
    value["scan_id"].length > 0 &&
    value["scan_id"].length <= 128
  );
}

function isPageBulkGrabRuntimeRequest(value: unknown): value is { tab_id: number; url: string; title?: string; scan_id?: string } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["tab_id", "url", "title", "scan_id"])) return false;
  const scanID = value["scan_id"];
  if (scanID !== undefined && (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128)) return false;
  return (
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0 &&
    typeof value["url"] === "string" &&
    value["url"].startsWith("https://") &&
    value["url"].length <= 4000 &&
    (value["title"] === undefined || typeof value["title"] === "string")
  );
}

function isPageBulkIdentifier(value: unknown): value is PageBulkIdentifier {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["local_id", "kind", "value"])) return false;
  const kind = value["kind"];
  return (
    typeof value["local_id"] === "string" &&
    value["local_id"].length > 0 &&
    value["local_id"].length <= 128 &&
    (kind === "doi" || kind === "pmid" || kind === "arxiv" || kind === "openalex") &&
    typeof value["value"] === "string" &&
    value["value"].length > 0 &&
    value["value"].length <= 512
  );
}

function isPageBulkStatusRuntimeRequest(
  value: unknown,
): value is { scan_id: string; identifiers: PageBulkIdentifier[]; rendered_record_count_hint?: number } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["scan_id", "identifiers", "rendered_record_count_hint"])) return false;
  const scanID = value["scan_id"];
  const identifiers = value["identifiers"];
  if (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128) return false;
  if (!Array.isArray(identifiers) || identifiers.length < 1 || identifiers.length > 200) return false;
  if ("rendered_record_count_hint" in value) {
    const hint = value["rendered_record_count_hint"];
    if (typeof hint !== "number" || !Number.isInteger(hint) || hint < 0) return false;
  }
  return identifiers.every(isPageBulkIdentifier);
}

function isPageBulkSubmitSource(value: unknown): value is PageBulkSubmitSource {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["kind", "origin", "detector"]) &&
    value["kind"] === "browser_page" &&
    isBareHTTPSOrigin(value["origin"]) &&
    typeof value["detector"] === "string" &&
    value["detector"].length > 0 &&
    value["detector"].length <= 128
  );
}

function isPageBulkSubmitRuntimeRequest(
  value: unknown,
): value is { scan_id: string; canonical_keys: string[]; source: PageBulkSubmitSource } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["scan_id", "canonical_keys", "source"])) return false;
  const scanID = value["scan_id"];
  const keys = value["canonical_keys"];
  if (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128) return false;
  if (!Array.isArray(keys) || keys.length < 1 || keys.length > 50) return false;
  if (!keys.every((key) => typeof key === "string" && key.length > 0 && key.length <= 300)) return false;
  return isPageBulkSubmitSource(value["source"]);
}

function isPageBulkAllowlistGetRuntimeRequest(value: unknown): value is { origin: string } {
  return isObjectRecord(value) && hasOnlyKeys(value, ["origin"]) && isBareHTTPSOrigin(value["origin"]);
}

function isPageBulkAllowlistSetRuntimeRequest(value: unknown): value is { origin: string; allowed: boolean } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["origin", "allowed"]) &&
    isBareHTTPSOrigin(value["origin"]) &&
    typeof value["allowed"] === "boolean"
  );
}

function isHandoffOpenRuntimeRequest(value: unknown): value is { job_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["job_id"]) &&
    typeof value["job_id"] === "string" &&
    value["job_id"].length > 0 &&
    value["job_id"].length <= 1024
  );
}

function isSessionRetryRuntimeRequest(value: unknown): value is { job_id: string } {
  return isHandoffOpenRuntimeRequest(value);
}

function isDeliveryStartRuntimeRequest(value: unknown): value is DeliveryStartPayload {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["tab_id", "url", "doi", "title"])) return false;
  return (
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0 &&
    typeof value["url"] === "string" &&
    value["url"].length > 0 &&
    value["url"].length <= 4000 &&
    (value["doi"] === undefined || typeof value["doi"] === "string") &&
    (value["title"] === undefined || typeof value["title"] === "string")
  );
}

// The key whitelist deliberately omits request_id. This path is the popup's
// UNSOLICITED capture (capture.ts's captureFixture), which answers no
// page_capture_request; accepting a caller-supplied correlation id here would
// let an extension page forge a binding to whatever capture the CLI is
// currently waiting on — the exact cross-binding papio-85a7420f4cd2564f is
// about, just deliberate instead of accidental. A requested capture never
// travels this way: it is sent straight from onPageCaptureRequest.
function isPageCaptureRuntimeRequest(value: unknown): value is PageCapturePayload {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["host", "scenario", "adapter_id", "adapter_version", "encoding", "bytes", "body"])
  ) {
    return false;
  }
  return (
    typeof value["host"] === "string" &&
    typeof value["scenario"] === "string" &&
    (value["adapter_id"] === undefined || typeof value["adapter_id"] === "string") &&
    (value["adapter_version"] === undefined || typeof value["adapter_version"] === "string") &&
    typeof value["encoding"] === "string" &&
    isPositiveSafeInteger(value["bytes"]) &&
    typeof value["body"] === "string"
  );
}

/**
 * Exact extension-page authorization prevents a content script from sending
 * captured page material over native messaging.
 */
export async function handleInboxRuntimeMessage(
  bridge: Bridge,
  message: unknown,
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): Promise<InboxRuntimeReply | undefined> {
  if (!isObjectRecord(message) || typeof message["type"] !== "string") return undefined;
  const type = message["type"];
  if (type === "papio.page_capture") {
    if (sender.id !== urls.runtimeID || sender.url !== urls.popupURL) {
      return runtimeFailure("unauthorized", "This sender cannot send page captures");
    }
    const capturePayload = message["payload"];
    if (!hasOnlyKeys(message, ["type", "payload"]) || !isPageCaptureRuntimeRequest(capturePayload)) {
      return runtimeFailure("invalid_request", "Invalid page capture request");
    }
    if (!bridge.pageCaptureAvailable()) return { captured: true };
    // A refusal here is not a routine "diagnostic panel is closed" state:
    // the operator explicitly selected `terms`, so the popup's own filter
    // (which withholds `terms` from the scenario list unless the daemon
    // already advertised it) was correct when the panel loaded but the
    // daemon underneath was swapped before the click (the two-binary skew
    // AGENTS.md documents). Reporting `{ captured: true }` here previously
    // sent the operator hunting a daemon-side storage bug that does not
    // exist, when the real fix is upgrading the stale daemon.
    if (capturePayload.scenario === "terms" && !bridge.termsCaptureAvailable()) {
      return runtimeFailure(
        "capture_failed",
        "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
      );
    }
    return bridge.sendPageCapture(capturePayload)
      ? { captured: true }
      : runtimeFailure("capture_failed", "Could not send page capture");
  }
  if (type === "papio.openInbox") {
    if (!isInboxOrPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot open the inbox");
    if (!hasOnlyKeys(message, ["type"])) return runtimeFailure("invalid_request", "Invalid inbox open request");
    try {
      await bridge.openInbox(urls.inboxURL);
      return { opened: true };
    } catch {
      return runtimeFailure("open_failed", "Could not open the inbox");
    }
  }
  if (type === "papio.stats") {
    if (!isStatsSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot access papio stats");
    if (!hasOnlyKeys(message, ["type", "request"])) return runtimeFailure("invalid_request", "Invalid stats request");
    return isCountsRuntimeRequest(message["request"])
      ? bridge.requestStats()
      : runtimeFailure("invalid_request", "Invalid stats request");
  }
  if (type === "papio.handoff.open") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot access the inbox broker");
    }
    if (!hasOnlyKeys(message, ["type", "request"])) {
      return runtimeFailure("invalid_request", "Invalid handoff open request");
    }
    return isHandoffOpenRuntimeRequest(message["request"])
      ? bridge.openHandoff(message["request"].job_id)
      : runtimeFailure("invalid_request", "Invalid handoff open request");
  }
  if (type === "papio.delivery.start") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot start PDF delivery");
    }
    if (!hasOnlyKeys(message, ["type", "request"]) || !isDeliveryStartRuntimeRequest(message["request"])) {
      return runtimeFailure("invalid_request", "Invalid PDF delivery request");
    }
    return bridge.startPDFDelivery(message["request"]);
  }
  if (type === "papio.delivery.state") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot read PDF delivery state");
    }
    if (!hasOnlyKeys(message, ["type"])) return runtimeFailure("invalid_request", "Invalid PDF delivery state request");
    return bridge.deliveryState();
  }
  if (type === "papio.session.state") {
    if (!isPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot access institution session state");
    if (!hasOnlyKeys(message, ["type"])) return runtimeFailure("invalid_request", "Invalid institution session request");
    return {
      ok: true,
      state: await bridge.sessionStateSnapshot(),
      origins: bridge.sessionOriginStates(),
    };
  }
  if (type === "papio.session.probe") {
    if (!isPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot probe institution session state");
    if (!hasOnlyKeys(message, ["type"])) return runtimeFailure("invalid_request", "Invalid institution session request");
    return {
      ok: true,
      state: await bridge.sessionStateWithProbe(),
      origins: bridge.sessionOriginStates(),
    };
  }
  if (type === "papio.session.signin") {
    if (!isPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot control institution sign-in");
    if (!hasOnlyKeys(message, ["type", "origin"])) return runtimeFailure("invalid_request", "Invalid institution sign-in request");
    const origin = message["origin"];
    if (origin === undefined) return bridge.requestSessionSignIn();
    if (!isBareHTTPSOrigin(origin)) return runtimeFailure("invalid_request", "Invalid institution sign-in request");
    return bridge.requestSessionSignIn(origin);
  }
  if (type === "papio.session.retry") {
    if (!isPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot retry institution handoffs");
    if (!hasOnlyKeys(message, ["type", "request"]) || !isSessionRetryRuntimeRequest(message["request"])) {
      return runtimeFailure("invalid_request", "Invalid institution handoff retry request");
    }
    return bridge.retryAuthStalled(message["request"].job_id);
  }
  if (type === "papio.pageBulk.load") {
    if (!isPageBulkSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot load a page-bulk scan");
    }
    const request = message["request"];
    // Same { scan_id } shape as papio.pageBulk.rescan — a plain read of the
    // already-open workspace's snapshot, never a re-scan.
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkRescanRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid page-bulk load request");
    }
    return bridge.getPageBulkSnapshot(request.scan_id);
  }
  if (type === "papio.pageBulk.scan") {
    if (!isPopupSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot start a page scan");
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkScanRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid page scan request");
    }
    return bridge.startPageBulkScan(request.tab_id, urls.pageBulkURL);
  }
  if (type === "papio.pageBulk.rescan") {
    if (!isPageBulkSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot rescan a page");
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkRescanRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid rescan request");
    }
    return bridge.requestPageBulkRescan(request.scan_id);
  }
  if (type === "papio.pageBulk.status") {
    if (!isPageBulkSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot look up page-bulk status");
    }
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkStatusRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid page-bulk status request");
    }
    return bridge.requestPageBulkStatus(request);
  }
  if (type === "papio.pageBulk.submit") {
    if (!isPageBulkSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot submit a page-bulk batch");
    }
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkSubmitRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid page-bulk submit request");
    }
    return bridge.requestPageBulkSubmit(request);
  }
  if (type === "papio.pageBulk.allowlist.get") {
    if (!isPageBulkSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot read the scanner allowlist");
    }
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkAllowlistGetRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid scanner allowlist request");
    }
    return bridge.pageBulkAllowlistContains(request.origin);
  }
  if (type === "papio.pageBulk.allowlist.set") {
    if (!isPageBulkSender(sender, urls)) {
      return runtimeFailure("unauthorized", "This sender cannot change the scanner allowlist");
    }
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkAllowlistSetRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid scanner allowlist request");
    }
    return bridge.setPageBulkAllowlist(request.origin, request.allowed);
  }
  if (type === "papio.pageBulk.grabPdf") {
    if (!isPageBulkSender(sender, urls)) return runtimeFailure("unauthorized", "This sender cannot grab a PDF");
    const request = message["request"];
    if (!hasOnlyKeys(message, ["type", "request"]) || !isPageBulkGrabRuntimeRequest(request)) {
      return runtimeFailure("invalid_request", "Invalid PDF grab request");
    }
    return bridge.requestPdfGrab({ ...request, workspace_tab_id: sender.tab?.id });
  }
  if (
    type !== "papio.activity" &&
    type !== "papio.triage.snapshot" &&
    type !== "papio.triage.counts" &&
    type !== "papio.triage.decide" &&
    type !== "papio.action.resolve" &&
    type !== "papio.delivery.reconcile" &&
    type !== "papio.preview"
  ) {
    return undefined;
  }
  const senderAuthorized =
    type === "papio.activity" ? isInboxOrPopupSender(sender, urls) : isInboxSender(sender, urls);
  if (!senderAuthorized) {
    return runtimeFailure("unauthorized", "This sender cannot access the inbox broker");
  }
  if (!hasOnlyKeys(message, ["type", "request"])) {
    return runtimeFailure("invalid_request", "Invalid inbox broker request");
  }
  const request = message["request"];
  switch (type) {
    case "papio.activity":
      return isActivityRuntimeRequest(request)
        ? bridge.requestActivity(request.limit)
        : runtimeFailure("invalid_request", "Invalid activity request");
    case "papio.triage.snapshot":
      return isSnapshotRuntimeRequest(request)
        ? bridge.requestTriageSnapshot(request)
        : runtimeFailure("invalid_request", "Invalid triage snapshot request");
    case "papio.triage.counts":
      return isCountsRuntimeRequest(request)
        ? bridge.requestTriageCounts()
        : runtimeFailure("invalid_request", "Invalid triage counts request");
    case "papio.triage.decide":
      return isDecisionRuntimeRequest(request)
        ? bridge.requestTriageDecision(request)
        : runtimeFailure("invalid_request", "Invalid triage decision request");
    case "papio.action.resolve":
      return isResolveRuntimeRequest(request)
        ? bridge.requestActionResolve(request)
        : runtimeFailure("invalid_request", "Invalid action resolution request");
    case "papio.delivery.reconcile":
      return isDeliveryReconcileRuntimeRequest(request)
        ? bridge.requestDeliveryReconcile(request)
        : runtimeFailure("invalid_request", "Invalid delivery reconciliation request");
    case "papio.preview":
      return isPreviewRuntimeRequest(request)
        ? bridge.requestPreview(request)
        : runtimeFailure("invalid_request", "Invalid preview request");
    default:
      return undefined;
  }
}

/** Defensive shape check for whatever chrome.storage.session actually holds
 * (foreign extension write, a stale schema from a prior version, or nothing
 * yet) before trusting it as a PageBulkScanStore. */
function isPageBulkScanStore(value: unknown): value is PageBulkScanStore {
  if (typeof value !== "object" || value === null) return false;
  if (!("order" in value) || !("byId" in value)) return false;
  const order = value.order;
  const byId = value.byId;
  return Array.isArray(order) && order.every((id) => typeof id === "string") && typeof byId === "object" && byId !== null;
}

function realDeps(): BridgeDeps {
  return {
    connectNative: (name) => {
      const port = chrome.runtime.connectNative(name);
      return {
        postMessage: (msg) => port.postMessage(msg),
        onMessage: { addListener: (cb) => port.onMessage.addListener((m) => cb(m)) },
        onDisconnect: { addListener: (cb) => port.onDisconnect.addListener(() => cb()) },
        disconnect: () => port.disconnect(),
      };
    },
    manifestVersion: chrome.runtime.getManifest().version,
    randomUUID: () => crypto.randomUUID(),
    now: () => Date.now(),
    setTimeout: (fn, ms) => {
      setTimeout(fn, ms);
    },
    runtimeSendMessage: (message) => chrome.runtime.sendMessage(message),
    backend: chromeBackend(chrome.storage),
    tabs: {
      create: (props) => chrome.tabs.create(props),
      get: (tabID) => chrome.tabs.get(tabID),
      reload: (tabID) => chrome.tabs.reload(tabID),
      remove: (tabID) => chrome.tabs.remove(tabID),
      update: (tabID, props) => chrome.tabs.update(tabID, props),
      query: (query) => chrome.tabs.query(query),
      sendMessage: (tabID, message) => chrome.tabs.sendMessage(tabID, message),
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
      onActivated: { addListener: (cb) => chrome.tabs.onActivated.addListener(cb) },
      ...(typeof chrome.tabs.group === "function"
        ? { group: (opts: { tabIds: number[]; groupId?: number }) => chrome.tabs.group(opts as chrome.tabs.GroupOptions) }
        : {}),
    },
    // chrome.windows is present in every Chromium; guarded for other runtimes.
    ...(typeof chrome.windows !== "undefined"
      ? {
          windows: {
            create: (props: { url: string; focused: boolean; state: "minimized" | "normal" }) =>
              chrome.windows.create(props) as Promise<WindowInfo>,
            // populate:true so the idle-close check can see a keepalive-pinned tab.
            get: (windowID: number) =>
              chrome.windows.get(windowID, { populate: true }) as Promise<WindowInfo>,
            update: (
              windowID: number,
              props: { focused?: boolean; state?: "normal" | "minimized"; drawAttention?: boolean },
            ) => chrome.windows.update(windowID, props),
            remove: (windowID: number) => chrome.windows.remove(windowID),
          },
        }
      : {}),
    // chrome.tabGroups: Chrome and Firefox 139+ (with the tabGroups permission);
    // absent on Firefox < 139 and older Chromium. Runtime-detected either way.
    ...(typeof chrome.tabGroups !== "undefined"
      ? {
          tabGroups: {
            get: (groupID: number) => chrome.tabGroups.get(groupID) as Promise<TabGroupInfo>,
            update: (groupID: number, props: { collapsed?: boolean; title?: string; color?: string }) =>
              chrome.tabGroups.update(groupID, props as chrome.tabGroups.UpdateProperties),
            query: (props: { title?: string }) => chrome.tabGroups.query(props) as Promise<TabGroupInfo[]>,
          },
        }
      : {}),
    downloads: {
      download: (options) => chrome.downloads.download(options),
      removeFile: (downloadID) => chrome.downloads.removeFile(downloadID),
      erase: (query) => chrome.downloads.erase(query),
      search: (query) => chrome.downloads.search(query),
      onCreated: { addListener: (cb) => chrome.downloads.onCreated.addListener(cb) },
      onChanged: { addListener: (cb) => chrome.downloads.onChanged.addListener(cb) },
      ...(chrome.downloads.onDeterminingFilename
        ? {
            onDeterminingFilename: {
              addListener: (
                cb: (
                  item: DownloadItemLike,
                  suggest: (s: { filename: string; conflictAction: "uniquify" }) => void,
                ) => void,
              ) => chrome.downloads.onDeterminingFilename.addListener(cb),
            },
          }
        : {}),
    },
    adapterSpecs: adapters,
    scripting: {
      executeScript: (injection) =>
        chrome.scripting.executeScript(
          injection as unknown as chrome.scripting.ScriptInjection<unknown[], unknown>,
        ),
    },
    captureStorage: {
      local: {
        get: (key) => chrome.storage.local.get(key),
        set: (items) => chrome.storage.local.set(items),
      },
    },
    tabLedger: {
      load: async () => {
        const got = await chrome.storage.local.get(MANAGED_TAB_LEDGER_KEY);
        const v = got[MANAGED_TAB_LEDGER_KEY];
        if (typeof v !== "object" || v === null || Array.isArray(v)) return {};
        const entries: Record<string, number> = {};
        for (const [key, value] of Object.entries(v as Record<string, unknown>)) {
          if (typeof value === "number") entries[key] = value;
        }
        return entries;
      },
      save: async (entries) => {
        await chrome.storage.local.set({ [MANAGED_TAB_LEDGER_KEY]: entries });
      },
    },
    permissions: {
      contains: (perm) => chrome.permissions.contains(perm),
    },
    settings: {
      async getTermsConsent() {
        try {
          const got = await chrome.storage.local.get(TERMS_CONSENT_KEY);
          const v = got[TERMS_CONSENT_KEY];
          return v === "accept" || v === "manual" ? v : undefined;
        } catch {
          return undefined;
        }
      },
      async setTermsConsent(value) {
        await chrome.storage.local.set({ [TERMS_CONSENT_KEY]: value });
      },
      async getHandoffSurface(): Promise<HandoffSurface> {
        try {
          const got = await chrome.storage.local.get([HANDOFF_SURFACE_KEY, WORK_WINDOW_KEY]);
          const v = got[HANDOFF_SURFACE_KEY];
          if (v === "in-window" || v === "work-window" || v === "tab-group") return v;
          // No explicit choice: honor the legacy boolean so upgrades are seamless.
          return got[WORK_WINDOW_KEY] === false ? "in-window" : "work-window";
        } catch {
          return "work-window";
        }
      },
    },
    pageBulkScans: {
      async get() {
        try {
          const got = await chrome.storage.session.get(PAGE_BULK_SCAN_STORAGE_KEY);
          const stored = got[PAGE_BULK_SCAN_STORAGE_KEY];
          return isPageBulkScanStore(stored) ? stored : emptyPageBulkScanStore();
        } catch {
          return emptyPageBulkScanStore();
        }
      },
      async set(store) {
        await chrome.storage.session.set({ [PAGE_BULK_SCAN_STORAGE_KEY]: store });
      },
    },
    pdfGrabCorrelations: {
      async get() {
        try {
          const got = await chrome.storage.session.get(PDF_GRAB_CORRELATION_STORAGE_KEY);
          const stored = got[PDF_GRAB_CORRELATION_STORAGE_KEY];
          if (typeof stored !== "object" || stored === null) return {};
          return stored as Record<string, PdfGrabCorrelation>;
        } catch {
          return {};
        }
      },
      async set(value) {
        await chrome.storage.session.set({ [PDF_GRAB_CORRELATION_STORAGE_KEY]: value });
      },
    },
    scannerAllowlist: {
      async get() {
        try {
          const got = await chrome.storage.local.get(PAGE_BULK_ALLOWLIST_KEY);
          const stored = got[PAGE_BULK_ALLOWLIST_KEY];
          return Array.isArray(stored) ? stored.filter((origin): origin is string => typeof origin === "string") : [];
        } catch {
          return [];
        }
      },
      async set(origins) {
        await chrome.storage.local.set({ [PAGE_BULK_ALLOWLIST_KEY]: origins });
      },
    },
    action: {
      setBadgeText: (details) => chrome.action.setBadgeText(details),
      setBadgeBackgroundColor: (details) => chrome.action.setBadgeBackgroundColor(details),
      setTitle: (details) => chrome.action.setTitle(details),
    },
    alarms: {
      create: (name, info) => chrome.alarms?.create(name, info),
      onAlarm: {
        addListener: (cb) => chrome.alarms?.onAlarm?.addListener(cb),
      },
    },
  };
}

// Wiring runs only inside a real extension service worker, never under bun test.
if (typeof chrome !== "undefined" && chrome.runtime?.id) {
  const bridge = new Bridge(realDeps());
  // The broker authorizes senders by exact page URL. Derive the popup path
  // from the manifest and the inbox as its sibling so the authorized URLs
  // can never drift from the shipped page layout again.
  const declaredPopup = chrome.runtime.getManifest().action?.default_popup ?? POPUP_PAGE_PATH;
  const inboxRuntimeURLs: InboxRuntimeURLs = {
    runtimeID: chrome.runtime.id,
    inboxURL: chrome.runtime.getURL(declaredPopup.replace(/[^/]*$/, "inbox.html")),
    popupURL: chrome.runtime.getURL(declaredPopup),
    historyURL: chrome.runtime.getURL(declaredPopup.replace(/[^/]*$/, "history.html")),
    pageBulkURL: chrome.runtime.getURL(declaredPopup.replace(/[^/]*$/, "page-bulk.html")),
  };
  // Top-level registrations give Chrome a reason to start this worker at
  // browser launch and after install/update. Without them a cold-started
  // Chrome leaves the worker dead (and the daemon unreachable) until an
  // unrelated tab or download event happens to fire. bridge.start() already
  // ran at module top level by then; the callbacks need no body.
  chrome.runtime.onStartup.addListener(() => {});
  chrome.runtime.onInstalled.addListener(() => {});
  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (
      isObjectRecord(message) &&
      (message["type"] === "papio.openInbox" ||
        message["type"] === "papio.page_capture" ||
        message["type"] === "papio.activity" ||
        message["type"] === "papio.triage.snapshot" ||
        message["type"] === "papio.triage.counts" ||
        message["type"] === "papio.triage.decide" ||
        message["type"] === "papio.action.resolve" ||
        message["type"] === "papio.delivery.reconcile" ||
        message["type"] === "papio.preview" ||
        message["type"] === "papio.handoff.open" ||
        message["type"] === "papio.delivery.start" ||
        message["type"] === "papio.delivery.state" ||
        message["type"] === "papio.session.state" ||
        message["type"] === "papio.session.probe" ||
        message["type"] === "papio.session.signin" ||
        message["type"] === "papio.session.retry" ||
        message["type"] === "papio.stats" ||
        message["type"] === "papio.pageBulk.load" ||
        message["type"] === "papio.pageBulk.scan" ||
        message["type"] === "papio.pageBulk.rescan" ||
        message["type"] === "papio.pageBulk.status" ||
        message["type"] === "papio.pageBulk.submit" ||
        message["type"] === "papio.pageBulk.allowlist.get" ||
        message["type"] === "papio.pageBulk.allowlist.set" ||
        message["type"] === "papio.pageBulk.grabPdf")
    ) {
      void handleInboxRuntimeMessage(bridge, message, _sender, inboxRuntimeURLs).then((reply) => {
        sendResponse(reply);
      });
      return true;
    }
    if (isCapabilitiesRequest(message)) {
      sendResponse({ page_acquire: bridge.pageAcquireAvailable() });
      return false;
    }
    if (isPageAcquireRequest(message)) {
      void bridge.requestPageAcquire(message.payload).then(sendResponse);
      return true; // async native acknowledgement
    }
    if (isCancelRequest(message)) {
      void bridge.requestCancel(message.job_id).then(() => sendResponse({ ok: true }));
      return true; // async sendResponse
    }
    if (isTermsConsentRequest(message)) {
      void bridge.requestTermsConsent(message.value).then(() => sendResponse({ ok: true }));
      return true; // async sendResponse
    }
    if (isOrphanTabsRequest(message)) {
      if (message.action === "orphan_tabs_status") {
        void bridge.orphanTabStatus().then(sendResponse);
      } else {
        void bridge.cleanupOrphanTabs().then(sendResponse);
      }
      return true; // async sendResponse
    }
    return false;
  });
  // A grant/revoke changes both resolver setup and whether a recorded provider
  // blocker remains effective; clear the cached answer before repainting.
  chrome.permissions?.onAdded?.addListener(() => {
    void bridge.onPermissionsChanged();
  });
  chrome.permissions?.onRemoved?.addListener(() => {
    void bridge.onPermissionsChanged();
  });
  // KEEPALIVE INTEGRATION
  // Constructed and attached synchronously, before bridge.start() runs (and
  // therefore before bindListeners() binds any chrome.tabs/alarms listener):
  // a navigation that WAKES this worker must never reach onTabUpdated with
  // this.keepaliveManager still undefined. initKeepalive() itself only fires
  // manager.init() without awaiting it, so hydration continues concurrently
  // with the bridge's own async startup below — neither blocks the other.
  const keepaliveManager = initKeepalive(chromeKeepaliveAPI(chrome), {
    trackedJobCount: () => bridge.trackedJobCount(),
    warmDemand: () => bridge.warmDemand(),
    latestOpenURL: () => bridge.latestOpenURL(),
    knownResolverOrigins: () => bridge.knownResolverOrigins(),
    queuedAuthJobs: () => bridge.queuedAuthJobs(),
    stalledAuthJobs: () => bridge.stalledAuthJobIDs(),
    lastAuthReturnedAt: () => bridge.lastAuthReturnedAt(),
    workWindowID: () => bridge.workWindowIDForKeepalive(),
    onTabPlaced: (tabID) => bridge.foldKeepaliveTab(tabID),
    configuredOriginsReady: () => bridge.hasCurrentHello(),
    onFreshSessionEvidence: (evidence: FreshSessionEvidence) => {
      void bridge.recordFreshSessionEvidence(evidence);
    },
    onOriginAuthenticationChanged: (origin: string, authenticated: boolean) => {
      // A committed "no longer authenticated" must retract that origin's
      // release authority now, not let it idle out over AUTH_EVIDENCE_TTL_MS.
      // Signing out is exactly when papio must stop opening queued handoffs
      // into a session that will bounce them to a login wall.
      if (!authenticated) void bridge.revokeAuthEvidence(origin);
      void bridge.syncConnectionBadge();
    },
    onReauthStateChanged: (paused) => bridge.setKeepaliveReauthNeeded(paused),
    surfaceReauthTab: async (tabID) => {
      try {
        const tab = await chrome.tabs.get(tabID);
        if (tab.windowId === undefined) return;
        const win = await chrome.windows.get(tab.windowId);
        await chrome.windows.update(tab.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // Badge and popup remain the recoverable reauth signal.
      }
    },
  });
  bridge.attachKeepalive(keepaliveManager);
  void bridge.start();
}
