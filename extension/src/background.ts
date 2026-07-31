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
  parseBrowserMessage,
  type BrowserMessage,
  type BrowserMessageType,
  type PageAcquireAckPayload,
  type PageAcquirePayload,
  type PageCapturePayload,
} from "./protocol";
import {
  chromeBackend,
  findByJob,
  findByTab,
  patchJob,
  removeJob,
  upsertJob,
  emptyStore,
  type ActiveJob,
  type StateBackend,
  type StoreShape,
  type TermsConsent,
  type ProviderDrainLease,
  TERMS_CONSENT_KEY,
  WORK_WINDOW_KEY,
  HANDOFF_SURFACE_KEY,
  type HandoffSurface,
} from "./state";
import {
  adapters,
  interpret,
  type AdapterContext,
  type AdapterSpec,
  type PageVerdict,
} from "./adapters/types";
import { observeUnknown, type ObserveChromeApi } from "./observe";
import { chromeKeepaliveAPI, initKeepalive, isAuthenticationURL } from "./keepalive";
import { routeResolverService, type ResolverRoute } from "./resolver";
import { detectAuthFailure } from "./authfail";

export const NATIVE_HOST = "com.orgmentem.papio";
const CHROME_PDF_VIEWER_HOST = "mhjfbmdgcfjbbpaeojofohoefgiehjai";
/** Lowest native daemon that can service this extension. 0.9.0 renamed the
 * wire access mode to "delegated"; older daemons emit "maximal", which this
 * extension rejects fail-closed. */
const MIN_DAEMON_VERSION = "0.9.0";


const AUTH_EVIDENCE_TTL_MS = 30 * 60_000;
const QUEUED_HANDOFF_RELEASE_MS = 45_000;
// A provider page can classify `unknown` transiently: its adapter selectors
// (custom elements, React roots) upgrade after the tab reports complete and
// after the SSO landing. Re-drive the idempotent classify path on a bounded
// schedule so a slow render still reaches a decisive verdict.
const CLASSIFY_RETRY_MS = 2_500;
const MAX_CLASSIFY_RETRIES = 8;
// A challenge holds only its provider's queue for the same one minute the old
// bounded challenge probe used, then a fresh drain can reclaim it.
const PROVIDER_DRAIN_LEASE_MS = 24 * CLASSIFY_RETRY_MS;
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
const TRIAGE_MUTATIONS_FEATURE = "triage_mutations_v1";
const REVIEW_PREVIEW_FEATURE = "review_preview_v1";
const STATS_FEATURE = "browser_stats_v1";
const PAGE_CAPTURE_FEATURE = "page_capture_v1";
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


/** Whether this adapter's SPA must render outside the minimized work window. */
export function needsVisibleWindow(spec: AdapterSpec | undefined): boolean {
  return spec?.requiresVisible === true;
}

/**
 * Keep bot-check detection structural: Cloudflare localizes its copy, while
 * these live-page markers survive both translation and fixture sanitization.
 *
 * SERIALIZATION CONTRACT: this must remain self-contained because
 * chrome.scripting serializes it into the provider page.
 */
export function isBotChallenge(doc: Document | null): boolean {
  const root: Document = doc ?? document;
  return (
    root.querySelector(
      'script[src*="/cdn-cgi/challenge-platform/"], ' +
        'script[src*="challenges.cloudflare.com/turnstile/"], ' +
        'input[name="cf-turnstile-response"], ' +
        '[id^="cf-chl-"], ' +
        '#captcha-box .main-wrapper[role="main"]',
    ) !== null
  );
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
    query?(query: { url?: string; groupId?: number }): Promise<TabInfo[]>;
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
    /** Optional (Chrome): add tabs to a group, creating one when groupId is
     * omitted. Returns the group id. Absent on platforms without tab groups. */
    group?(opts: { tabIds: number[]; groupId?: number }): Promise<number>;
  };
  /** chrome.windows seam. When present (and the user setting allows), broker
   * tabs use one dedicated minimized "work window" instead of the user's tab
   * strip, except an adapter whose SPA needs a visible window. A tab otherwise
   * surfaces only when the human is needed (IdP authentication). Absent on
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
  /** chrome.tabGroups seam. Present when tab-group handoff mode is available:
   * Chrome, and Firefox 139+ with the tabGroups permission. Absent on Firefox
   * < 139 and older Chromium, where tab-group mode falls back to the work
   * window. */
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
     * local to the extension/browser and is never put in a native frame. The
     * returned ID is the exact job correlation. */
    download(options: {
      url: string;
      filename: string;
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
  /** True only for a direct-file offer attempted before any broker tab opens. */
  directOffer: boolean;
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
 * chrome.downloads.download; it never crosses native messaging or storage. */
function extractDownloadURL(selector: string): string | null {
  const el = document.querySelector(selector);
  if (!(el instanceof HTMLAnchorElement)) return null;
  try {
    const u = new URL(el.href, location.href);
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** Self-contained meta-tag PDF-URL extractor, injected verbatim into the tracked
 * page. Returns only an HTTPS URL from the named meta tag's content. The URL
 * stays in extension memory and is handed directly to chrome.downloads.download;
 * it never crosses native messaging or storage. */
function extractMetaURL(metaName: string): string | null {
  const el = document.querySelector(`meta[name="${metaName}"]`);
  if (!(el instanceof HTMLMetaElement)) return null;
  try {
    const u = new URL(el.content, location.href);
    return u.protocol === "https:" ? u.href : null;
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

/** Self-contained click of a terms-and-conditions accept control, found by
 * accessible text inside an open modal (piercing shadow roots). Runs ONLY when
 * the user has recorded informed consent; the extension never guesses terms
 * controls otherwise. Returns whether a matching control was clicked. */
export function clickTermsAccept(modalSelector: string, textAny: string[]): boolean {
  const modal = document.querySelector(modalSelector);
  if (!modal) return false;
  const needles = textAny.map((t) => t.toLowerCase());
  const walk = (root: ParentNode): boolean => {
    for (const el of Array.from(root.querySelectorAll("*"))) {
      // Click only a genuine control, never a wrapping container whose text
      // merely includes the accept label: a modal footer <div> holds both
      // "Cancel" and "Accept and download", and clicking it is a no-op. The
      // real control is button-like (JSTOR's is an mfe-*-button with a shadow
      // #button-element).
      const tag = el.tagName.toLowerCase();
      const actionable =
        tag === "button" ||
        tag === "a" ||
        el.getAttribute?.("role") === "button" ||
        tag.endsWith("-button");
      if (actionable) {
        const label = ((el as HTMLElement).innerText ?? "") + " " + (el.getAttribute?.("aria-label") ?? "");
        if (needles.some((n) => label.toLowerCase().includes(n))) {
          const shadow = (el as HTMLElement & { shadowRoot?: ShadowRoot | null }).shadowRoot;
          const inner = shadow?.querySelector<HTMLElement>("#button-element");
          (inner ?? (el as HTMLElement)).click();
          return true;
        }
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
  private seq = 0;
  private store: StoreShape = emptyStore();
  private ready: Promise<void> = Promise.resolve();
  /** Serializes full-snapshot persistence. Concurrent Chrome events apply their
   * state transforms synchronously in event order, but chrome.storage gives no
   * write-ordering guarantee, so saves are chained: each runs after the prior
   * settles and persists the latest snapshot, so a stale write never wins. */
  private saveChain: Promise<void> = Promise.resolve();
  private listenersBound = false;
  /** Per-job in-progress download correlation (in-memory; transient). */
  private readonly downloads = new Map<string, DownloadTrack>();
  /** Tabs we are intentionally closing, so onRemoved does not emit a spurious
   * cancelled outcome for a programmatic close. */
  private readonly closingTabs = new Set<number>();
  /** A finished download keeps its broker tab open until the daemon has
   * acknowledged the adoption attempt for that job. */
  private readonly completedDownloadTabs = new Map<string, number>();
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
  /** Jobs that already reported a given terminal handoff or provider outcome,
   * so retries of one drive do not spam the daemon. Cleared for a fresh drive
   * and on job removal. */
  private readonly handoffOutcomeSent = new Set<string>();
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
  /** Resolver pages that conclusively show zero electronic holdings are terminal
   * for this offer. Keep this worker-local debounce until the job is removed so
   * reloads and SPA completion events cannot report the same outcome repeatedly. */
  private readonly resolverNoEntitlementSent = new Set<string>();
  /** Authentication observed during this service-worker lifetime. */
  private authReturnedThisWorker = false;
  /** A completed OA landing can release only OA concurrency queues; it is never
   * evidence that an institutional SSO session exists. */
  private openAccessLandingObserved = false;
  /** Keepalive has observed its resolver tab return from authentication. */
  private keepaliveAuthenticated = false;
  /** Atomically reserves the one visible handoff while tabs.create is in flight. */
  private handoffOpening = false;
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

  constructor(private readonly deps: BridgeDeps) {}
  trackedJobCount(): number {
    return this.store.activeJobs.length;
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

  /** Remember the standing host-level blocker separately from the per-job
   * daemon transition, so repeated pages do not create duplicate attention. */
  private async reportBlockedProviderHost(jobID: string, host: string): Promise<void> {
    if (!this.currentBlockedProviderHosts().includes(host)) {
      await this.update((store) => ({
        ...store,
        blockedProviderHosts: [...new Set([...(store.blockedProviderHosts ?? []), host])],
      }));
      await this.syncConnectionBadge();
    }

    const job = findByJob(this.store, jobID);
    const outcomeKey = `${jobID}:ui_changed`;
    if (job === undefined || this.handoffOutcomeSent.has(outcomeKey)) return;
    this.handoffOutcomeSent.add(outcomeKey);
    // The protocol has no browser-permission outcome. `ui_changed` is the
    // existing terminal provider path that preserves manual recovery; this
    // explicit detail prevents it from diagnosing an adapter that never ran.
    if (
      !this.send(
        "provider_outcome",
        {
          outcome: "ui_changed",
          detail: `Papio cannot read ${host}: browser access for this provider is not enabled. Open Papio Options and grant this source, or use Grant all sources.`,
        },
        jobID,
      )
    ) {
      this.handoffOutcomeSent.delete(outcomeKey);
      return;
    }
    await this.update((store) => patchJob(store, jobID, { blocked_provider_host: host }));
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
    for (const host of this.currentBlockedProviderHosts()) {
      if ((await this.hasEffectiveProviderAccess(host)) === true) await this.clearBlockedProviderHost(host);
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
      try {
        await this.deps.tabGroups.update(groupID, { title: this.handoffGroupTitle(tabID), collapsed: false });
      } catch {
        // Group gone; activating the tab below still surfaces it.
      }
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
      await this.deps.tabGroups.update(groupID, { title: HANDOFF_GROUP_TITLE, collapsed: true });
      return true;
    } catch {
      // Group gone; its stored id must not be reused.
      return false;
    }
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
      if (status !== "connected") {
        await Promise.all([
          this.deps.action.setBadgeText({ text: "!" }),
          this.deps.action.setBadgeBackgroundColor({ color: "#777777" }),
          this.deps.action.setTitle?.({ title: "Papio: daemon disconnected" }),
        ]);
        return;
      }
      // A cold institutional offer is deliberately queued before opening an
      // unproven SAML exchange. Count it here so disabled keepalive cannot hide
      // the only sign-in signal while that preflight waits.
      const signInBlockersBeforePermissions = this.signInBlockerCount();
      const blockedProviderHosts = this.currentBlockedProviderHosts();
      let ungrantedResolverOrigins = 0;
      if (signInBlockersBeforePermissions === 0 && blockedProviderHosts.length === 0) {
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
      // The contains() calls above are async; if the port dropped meanwhile,
      // onPortDisconnect already painted "!" — don't overwrite it with a stale
      // connected-state badge.
      if (this.store.connectionStatus !== "connected") return;
      const signInBlockers = this.signInBlockerCount();
      if (signInBlockers > 0) {
        await Promise.all([
          this.deps.action.setBadgeText({ text: String(signInBlockers) }),
          this.deps.action.setBadgeBackgroundColor({ color: "#b06000" }),
          this.deps.action.setTitle?.({
            title: `Papio: ${signInBlockers} paper${signInBlockers === 1 ? "" : "s"} waiting on your institution sign-in`,
          }),
        ]);
        return;
      }
      if (blockedProviderHosts.length > 0) {
        const hostLabel =
          blockedProviderHosts.length === 1 ? blockedProviderHosts[0] : `${blockedProviderHosts.length} provider hosts`;
        await Promise.all([
          this.deps.action.setBadgeText({ text: String(blockedProviderHosts.length) }),
          this.deps.action.setBadgeBackgroundColor({ color: "#b06000" }),
          this.deps.action.setTitle?.({ title: `Papio: ${hostLabel} need${blockedProviderHosts.length === 1 ? "s" : ""} browser access` }),
        ]);
        return;
      }
      if (ungrantedResolverOrigins > 0) {
        await Promise.all([
          this.deps.action.setBadgeText({ text: String(ungrantedResolverOrigins) }),
          this.deps.action.setBadgeBackgroundColor({ color: "#1a73e8" }),
          this.deps.action.setTitle?.({
            title: `Papio: ${ungrantedResolverOrigins} library resolver permission${ungrantedResolverOrigins === 1 ? "" : "s"} need attention`,
          }),
        ]);
        return;
      }
      const pending = this.triagePendingCount;
      await Promise.all([
        this.deps.action.setBadgeText({ text: pending !== undefined && pending > 0 ? String(pending) : "" }),
        this.deps.action.setBadgeBackgroundColor({ color: "#1a73e8" }),
        this.deps.action.setTitle?.({
          title:
            pending === undefined
              ? "Papio: connected"
              : pending === 0
                ? "Papio: no pending triage items"
                : `Papio: ${pending} pending triage item${pending === 1 ? "" : "s"}`,
        }),
      ]);
    } catch {
      // Browser action APIs are advisory; do not make a healthy bridge fail.
    }
  }


  /** Bind browser listeners (once), open the native connection, send hello, and
   * hydrate persisted job/tab correlation. Safe to call on every SW spin-up.
   * The synchronous prefix (listener bind + connect) runs before the first
   * await, satisfying MV3's top-level-registration expectation. */
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
      this.hydrated = true;
      await this.update((current) => current);
    });
    this.connect();
    // Wake this worker even when idle so queued daemon offers reach it (the
    // native connection originates here, so the daemon cannot wake a dormant
    // worker itself). Idempotent: re-creating the same alarm just resets it.
    this.deps.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: KEEPALIVE_ALARM_MINUTES });
    await this.ready;
    await this.restoreProviderDrainLeaseTimers();
    await this.reconcileHandoffGroups();
    await this.syncConnectionBadge();
    await this.reconcileTabs();
    await this.redrivePendingTermsGates();
    for (const job of this.store.activeJobs) {
      if (job.status === "queued") this.scheduleQueuedHandoffRelease(job.job_id);
    }
    await this.releaseQueuedHandoffs();
    await this.releaseQueuedHandoffsForLiveLanding();
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
        this.reportAuthStalled(job.job_id);
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      this.beginProviderDrive(job.job_id);
      await this.update((s) =>
        patchJob(s, job.job_id, {
          tab_id: -1,
          status: "queued",
          download_initiated: false,
          unknown_count: 0,
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
      this.closingTabs.add(job.tab_id);
      try {
        await this.deps.tabs.remove(job.tab_id);
      } catch {
        // Tab may already be gone; the outcome frame is what matters.
      }
    }
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
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

  /**
   * Surface the browser-owned handoff already offered for an inbox row. This
   * boundary accepts only a job id: provider/resolver URLs remain local to the
   * extension and are never returned to the caller.
   */
  async openHandoff(jobID: string): Promise<BrokerReply<{ opened: true }>> {
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
    }
    if (job === undefined || !this.offerURLs.has(jobID) || job.tab_id < 0) {
      return this.failure("handoff_open_failed", "The offered handoff could not be opened");
    }

    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(job.tab_id);
    } catch {
      return this.failure("handoff_open_failed", "The offered handoff tab is no longer available");
    }
    if (tab.id !== job.tab_id || this.deps.tabs.update === undefined) {
      return this.failure("handoff_open_failed", "The offered handoff tab could not be focused");
    }

    try {
      await this.deps.tabs.update(tab.id, { active: true });
      if (this.deps.windows !== undefined && tab.windowId !== undefined) {
        const win = await this.deps.windows.get(tab.windowId);
        await this.deps.windows.update(tab.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      }
    } catch {
      return this.failure("handoff_open_failed", "The offered handoff tab could not be focused");
    }
    return { ok: true, opened: true };
  }

  /** A daemon-directed retry may refresh an expired authentication exchange;
   * the inbox and popup retain focus-only behavior so they cannot disrupt a
   * provider page that is already downloading. */
  private async focusDaemonHandoff(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    const openurl = this.offerURLs.get(jobID);
    if (job !== undefined && job.tab_id >= 0 && openurl !== undefined && this.deps.tabs.update !== undefined) {
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        const needsFreshResolver =
          job.status === "auth_pending" || (typeof tab.url === "string" && isAuthenticationURL(tab.url));
        if (tab.id === job.tab_id && needsFreshResolver) {
          // This is an explicit human retry, not an autonomous loop; charging it
          // would let prior stale failures consume the fresh link they requested.
          await this.deps.tabs.update(job.tab_id, { url: openurl });
        }
      } catch {
        // The focus path below still returns the local missing-tab outcome.
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
  private hasCurrentHello(): boolean {
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
    const result = await this.requestNative(
      "triage_counts_request",
      {},
      "triage_counts_response",
      TRIAGE_SNAPSHOT_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined) return this.nativeFailure(result);
    if (result.code !== undefined) return this.failure(result.code, result.message ?? "The request is unavailable");
    const counts = result.payload["counts"];
    if (typeof counts !== "object" || counts === null) return this.failure("invalid_response", "The daemon returned invalid counts");
    const pending = (counts as Record<string, unknown>)["pending_total"];
    if (typeof pending === "number") {
      this.triagePendingCount = pending;
      await this.syncConnectionBadge();
    }
    return {
      ok: true,
      counts: counts as Record<string, unknown>,
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
   * "accept": clear the one-time prompt flag and re-run classification on the
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
  private async acceptTerms(jobID: string, rule: { modalSelector: string; textAny: string[] }): Promise<boolean> {
    const job = findByJob(this.store, jobID);
    if (!job || job.tab_id < 0) return false;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: clickTermsAccept,
        args: [rule.modalSelector, rule.textAny],
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

  private bindListeners(): void {
    if (this.listenersBound) return;
    this.listenersBound = true;
    this.deps.tabs.onUpdated.addListener((tabID, change, tab) => {
      return this.onTabUpdated(tabID, change, tab);
    });
    this.deps.tabs.onRemoved.addListener((tabID) => {
      return this.onTabRemoved(tabID);
    });
    this.deps.downloads.onCreated.addListener((item) => {
      return this.onDownloadCreated(item);
    });
    this.deps.downloads.onChanged.addListener((delta) => {
      return this.onDownloadChanged(delta);
    });
    this.deps.downloads.onDeterminingFilename?.addListener((item, suggest) => {
      // The event can race on either side of downloads.download resolving:
      // use its exact returned ID after resolution, or the pending URL before.
      // Host fallback remains fail-closed when several jobs share a provider.
      const exactJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
      const job = exactJobID ? findByJob(this.store, exactJobID) : this.correlate(item);
      if (!job) return;
      const base = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
      if (base.length === 0) return;
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
    this.offerURLs.delete(jobID);
    this.queuedHandoffTimers.delete(jobID);
    this.classifyRetries.delete(jobID);
    this.loginEntityIDs.delete(jobID);
    this.federatedLoginRouted.delete(jobID);
    this.federatedReDriven.delete(jobID);
    this.handoffOutcomeSent.delete(`${jobID}:stale_sso`);
    this.handoffOutcomeSent.delete(`${jobID}:auth_error`);
    this.handoffOutcomeSent.delete(`${jobID}:ui_changed`);
    this.authFailureSurfaced.delete(jobID);
    this.staleRecoveryEpochs.delete(jobID);
    this.staleRecoveryAttemptedEpochs.delete(jobID);
    this.staleRecoveryInFlightEpochs.delete(jobID);
    this.resolverNoEntitlementSent.delete(jobID);
    this.proquestAccountIDs.delete(jobID);
    this.accountIdAppended.delete(jobID);
    await this.update((s) => {
      const offerURLs = { ...(s.offerURLs ?? {}) };
      delete offerURLs[jobID];
      return { ...removeJob(s, jobID), offerURLs };
    });
    if (providerKey !== undefined) await this.releaseProviderDrainWhenUnused(providerKey);
    await this.closeWorkWindowIfIdle();
    await this.dropStaleHandoffGroup();
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

  /** Report the human authentication step for a capped job, at most once per
   * worker lifetime. human_auth_required is non-terminal daemon-side: the job
   * stays parked (awaiting_human) and is re-offered on a future warm launch. */
  private reportAuthStalled(jobID: string): void {
    if (this.authStalledReported.has(jobID)) return;
    this.authStalledReported.add(jobID);
    this.send("provider_outcome", { outcome: "human_auth_required" }, jobID);
  }

  /** Clear a job's auth-failure budget once a real download proves the session
   * works, so an earlier expired-session streak cannot cap a now-valid job. */
  private clearAuthAttempts(store: StoreShape, jobID: string): StoreShape {
    if (store.authAttempts?.[jobID] === undefined) return store;
    const authAttempts = { ...store.authAttempts };
    delete authAttempts[jobID];
    return { ...store, authAttempts };
  }

  private hasRecentAuthEvidence(): boolean {
    const at = this.store.lastAuthReturnedAt;
    const age = typeof at === "number" ? this.deps.now() - at : Number.POSITIVE_INFINITY;
    return age >= 0 && age <= AUTH_EVIDENCE_TTL_MS;
  }

  private hasAuthEvidence(): boolean {
    return this.authReturnedThisWorker || this.keepaliveAuthenticated || this.hasRecentAuthEvidence();
  }

  /** Keeps an OA landing from opening an institutional queue while preserving
   * the existing one-visible-tab flow for ordinary offers. */
  private hasHandoffReleaseEvidence(requiresAuth: boolean | undefined): boolean {
    return this.hasAuthEvidence() || (requiresAuth !== true && this.openAccessLandingObserved);
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

  /** Persist only an institutional session proof, because an OA completion can
   * otherwise mint an unattended SAML request for every waiting handoff. */
  private async recordInstitutionalSession(job: ActiveJob, rawURL: string, now: number): Promise<boolean> {
    if (!this.isInstitutionalSessionLanding(job, rawURL)) return false;
    const firstAuthEvidence = !this.authReturnedThisWorker;
    this.authReturnedThisWorker = true;
    await this.update((s) => ({ ...s, lastAuthReturnedAt: now }));
    if (firstAuthEvidence) {
      await this.releaseQueuedHandoffs();
      await this.reloadAuthenticationHandoffs();
    }
    return true;
  }

  /** OA completions retain the ordinary queue flow without becoming evidence
   * that it is safe to reload or open an institutional sign-in. */
  private async recordOpenAccessLanding(job: ActiveJob): Promise<void> {
    if (job.requires_auth === true) return;
    const firstOpenAccessLanding = !this.openAccessLandingObserved;
    this.openAccessLandingObserved = true;
    await this.releaseQueuedHandoffs();
    if (firstOpenAccessLanding) await this.reloadAuthenticationHandoffs(false);
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

  /** Called by keepalive only after its resolver tab has returned from login. */
  async setKeepaliveAuthenticated(authenticated: boolean): Promise<void> {
    this.keepaliveAuthenticated = authenticated;
    if (!authenticated) return;
    await this.ready;
    await this.releaseQueuedHandoffs();
  }

  private async releaseQueuedHandoffs(fallbackJobID?: string, forceProvider = false): Promise<void> {
    if (fallbackJobID !== undefined) this.pendingForcedReleases.add(fallbackJobID);
    if (forceProvider && fallbackJobID !== undefined) {
      const forced = findByJob(this.store, fallbackJobID);
      if (forced !== undefined) await this.clearProviderDrainPark(this.providerKeyForJob(forced));
    }
    if (!this.hasAuthEvidence() && !this.openAccessLandingObserved && this.pendingForcedReleases.size === 0) {
      return;
    }
    if (this.drainingQueuedHandoffs) {
      await new Promise<void>((resolve) => this.queuedHandoffDrainWaiters.add(resolve));
      return;
    }
    this.drainingQueuedHandoffs = true;
    try {
      await this.expireProviderDrainLeases();
      // One loop opens at most one unclassified handoff per provider. A lease
      // stays with that tab until it proves normal, becomes a challenge park,
      // or expires, while other provider groups keep making progress.
      while (this.hasAuthEvidence() || this.openAccessLandingObserved || this.pendingForcedReleases.size > 0) {
        let selected = this.store.activeJobs.find(
          (job) =>
            job.status === "queued" &&
            this.hasHandoffReleaseEvidence(job.requires_auth) &&
            !this.hasActiveProviderDrainLease(job),
        );
        let forcedJobID: string | undefined;
        if (selected === undefined) {
          for (const jobID of this.pendingForcedReleases) {
            const candidate = this.store.activeJobs.find((job) => job.job_id === jobID && job.status === "queued");
            if (candidate === undefined) {
              this.pendingForcedReleases.delete(jobID);
              continue;
            }
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
            tabID = await this.openBrokerTab(url, forceSurface);
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

  private async reloadAuthenticationHandoffs(includeInstitutional = true): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (job.tab_id < 0 || job.status === "queued" || (!includeInstitutional && job.requires_auth === true)) {
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
      console.error("papio: inbound handler failed", e);
    });
    return dispatched;
  }

  private resolveNativeResponse(msg: BrowserMessage): void {
    const requestID = msg.payload["request_id"];
    if (typeof requestID !== "string") return;
    const pending = this.pendingNativeRequests.get(requestID);
    if (pending === undefined || pending.expectedType !== msg.type) {
      console.debug("papio: dropping unknown or late triage response", msg.type, requestID);
      return;
    }
    this.pendingNativeRequests.delete(requestID);
    pending.resolve({ kind: "response", payload: msg.payload });
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
    if (CORRELATED_RESULT_TYPES.has(msg.type)) {
      this.resolveNativeResponse(msg);
      return;
    }
    switch (msg.type) {
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
          if (providerParked) {
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
          if (providerParked) {
            await this.update((s) => patchJob(s, jobID, { handoffAckPending: true }));
            return;
          }
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
      this.reportAuthStalled(jobID);
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
    if (isDirectFileOffer(openurl)) {
      await this.upsertJobWithOffer(makeJob(-1), openurl);
      this.send("job_accept", {}, jobID);
      await this.startDirectOfferDownload(jobID, openurl);
      return;
    }

    const queueHandoff =
      providerParked ||
      (!this.hasHandoffReleaseEvidence(requiresAuth) &&
        (requiresAuth === true ||
          this.handoffOpening ||
          this.store.activeJobs.some((job) => job.tab_id >= 0 && job.status !== "queued")));
    if (queueHandoff) {
      const queued = makeJob(-1, "queued");
      await this.upsertJobWithOffer(
        providerParked ? { ...queued, handoffAckPending: true } : queued,
        openurl,
      );
      this.scheduleQueuedHandoffRelease(jobID);
      if (!providerParked) this.send("job_accept", {}, jobID);
      return;
    }

    this.handoffOpening = true;
    let tabID: number | undefined;
    try {
      tabID = await this.openBrokerTab(openurl, true);
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
    const queueHandoff =
      !this.hasHandoffReleaseEvidence(job.requires_auth) &&
      (job.requires_auth === true ||
        this.handoffOpening ||
        this.store.activeJobs.some((candidate) => candidate.tab_id >= 0 && candidate.status !== "queued"));
    if (queueHandoff) {
      await this.update((s) =>
        patchJob(s, jobID, {
          status: "queued",
          tab_id: -1,
          download_initiated: false,
        }),
      );
      this.scheduleQueuedHandoffRelease(jobID);
      return;
    }

    this.handoffOpening = true;
    let tabID: number | undefined;
    try {
      tabID = await this.openBrokerTab(url, true);
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
  }

  private async onCancel(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    if (job.tab_id >= 0) {
      // Broker-owned by construction (we only track tabs we created).
      this.closingTabs.add(job.tab_id);
      try {
        await this.deps.tabs.remove(job.tab_id);
      } catch {
        // Tab already closed.
      }
    }
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    await this.removeJobWithOffer(jobID);
  }

  /** The daemon acknowledges download_complete only after it has attempted
   * adoption. Close the broker-owned viewer then, never on a raw tab event. */
  private async closeAfterAdoption(jobID: string | undefined): Promise<void> {
    if (jobID === undefined) return;
    const tabID = this.completedDownloadTabs.get(jobID);
    if (tabID === undefined) return;
    this.completedDownloadTabs.delete(jobID);
    if (tabID >= 0) {
      this.closingTabs.add(tabID);
      try {
        await this.deps.tabs.remove(tabID);
      } catch {
        // The viewer may already have closed itself after the download completed.
      }
    }
    await this.removeJobWithOffer(jobID);
  }

  private async onTabUpdated(tabID: number, change: TabChangeInfo, tab: TabInfo): Promise<void> {
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
    // A title-only update counts: the UNE Shibboleth stale page is classifiable
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
    if (!onProvider) {
      // Chrome exposes its built-in PDF viewer as an internal extension URL.
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
      await this.update((s) => patchJob(s, job.job_id, { status: "awaiting_download" }));
      this.send("auth_returned", { elapsed_ms: elapsed }, job.job_id);
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
        if (openurl !== undefined && this.deps.tabs.update !== undefined) {
          this.federatedReDriven.add(job.job_id);
          await this.deps.tabs.update(job.tab_id, { url: openurl });
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
    if (openurl === undefined || job.tab_id < 0 || this.deps.tabs.update === undefined) return false;
    this.staleRecoveryAttemptedEpochs.set(job.job_id, recoveryEpoch);
    this.staleRecoveryInFlightEpochs.set(job.job_id, recoveryEpoch);
    try {
      if (this.authAttemptsFor(job.job_id) >= MAX_AUTH_ATTEMPTS) {
        this.reportAuthStalled(job.job_id);
        return false;
      }
      await this.chargeAuthAttempt(job.job_id, job.tab_id);
      if ((this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== recoveryEpoch) return true;
      await this.deps.tabs.update(job.tab_id, { url: openurl });
      return true;
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
    if (
      offerURL.origin !== landingURL.origin ||
      !/(?:openurl|uresolver)/i.test(offerURL.pathname)
    ) {
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
      if (result?.kind === "routed") return true;
      if (result?.kind === "no_entitlement") {
        if (!this.resolverNoEntitlementSent.has(job.job_id)) {
          // Deliberately omit adapter metadata and all page/URL data: the
          // resolver's exact zero-holdings marker is sufficient to terminate
          // this institutional attempt.
          if (this.send("provider_outcome", { outcome: "no_entitlement" }, job.job_id)) {
            this.resolverNoEntitlementSent.add(job.job_id);
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
      await this.recordUnknown(job, host);
      return; // no declarative adapter for this verified host
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
      try {
        const results = await this.deps.scripting.executeScript({
          target: { tabId: job.tab_id },
          func: isBotChallenge,
          args: [null],
        });
        if (results[0]?.result === true) {
          await this.waitForBotChallenge(job);
          return;
        }
      } catch (e) {
        // A failed probe must retain the existing stale-adapter path rather
        // than silently make an unreadable provider page immortal.
        console.error("papio: bot-challenge detection failed; classifying normally", e);
      }
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
  private async waitForBotChallenge(job: ActiveJob): Promise<void> {
    if (this.isProviderDrainParked(job)) return;
    if ((job.unknown_count ?? 0) !== 0) {
      await this.update((s) => patchJob(s, job.job_id, { unknown_count: 0 }));
    }
    this.classifyRetries.delete(job.job_id);
    await this.parkProviderDrain(job);
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


  /** Auto-select the institution on a provider login wall: navigate the handoff
   * tab to the adapter's federated-login entry with the offer's entityID, once
   * per drive. Institution selection is deterministic config, not a secret; the
   * human still enters credentials at the IdP. No-op without a configured route,
   * a known entityID, or a `tabs.update` seam, and never re-navigates mid
   * sign-in (latched, cleared on job removal). */
  private async maybeRouteFederatedLogin(jobID: string, job: ActiveJob, spec: AdapterSpec): Promise<void> {
    const template = spec.federatedLogin;
    const entityID = this.loginEntityIDs.get(jobID);
    if (template === undefined || entityID === undefined) return;
    if (this.federatedLoginRouted.has(jobID)) return;
    if (this.deps.tabs.update === undefined) return;
    const url = template.replace("{entityID}", encodeURIComponent(entityID));
    if (!url.startsWith("https://")) return;
    this.federatedLoginRouted.add(jobID);
    try {
      await this.deps.tabs.update(job.tab_id, { url });
    } catch (e) {
      // Let a later classify retry route again if this navigation failed.
      this.federatedLoginRouted.delete(jobID);
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


  private async reclassifyCurrentProviderPage(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (!job) return;
    const tab = await this.deps.tabs.get(job.tab_id);
    if (tab.url === undefined) return;
    let host: string;
    try {
      host = new URL(tab.url).hostname;
    } catch {
      return;
    }
    const onRegisteredProvider =
      hostMatches(host, job.provider_hosts) ||
      this.deps.adapterSpecs.some((candidate) => hostMatches(host, candidate.hosts));
    if (!onRegisteredProvider) return;
    await this.maybeClassify(jobID, host);
  }

  /** Record a development capture for an unknown page without changing the
   * assisted handoff semantics. */
  private async recordUnknown(job: ActiveJob, host: string, adapter?: AdapterSpec): Promise<void> {
    const captureStorage = this.deps.captureStorage;
    if (captureStorage !== undefined && this.pageCaptureAvailable()) {
      await observeUnknown(
        {
          scripting: this.deps.scripting as ObserveChromeApi["scripting"],
          storage: captureStorage,
          sendPageCapture: (payload, jobID) => this.sendPageCapture(payload, jobID),
        },
        job,
        host,
        {
          verifiedHosts: adapter === undefined ? job.provider_hosts : [...job.provider_hosts, ...adapter.hosts],
          ...(adapter === undefined
            ? {}
            : { adapterID: adapter.id, adapterVersion: adapter.version }),
        },
        () => new Date(this.deps.now()),
      );
    }
    if (adapter === undefined) return;
    const now = this.deps.now();
    const count = job.unknown_count ?? 0;
    const last = job.last_unknown_ms ?? 0;
    if (count >= 1 && now - last >= 5000) {
      // Retries wait for one document to render; they are not independent
      // provider failures, so one broker drive gets one terminal observation.
      const outcomeKey = `${job.job_id}:ui_changed`;
      if (!this.handoffOutcomeSent.has(outcomeKey)) {
        this.handoffOutcomeSent.add(outcomeKey);
        if (!this.send("provider_outcome", { outcome: "ui_changed", adapter_version: adapter.version }, job.job_id)) {
          this.handoffOutcomeSent.delete(outcomeKey);
        }
      }
      await this.update((s) => patchJob(s, job.job_id, { unknown_count: 0 }));
    } else if (count === 0) {
      await this.update((s) => patchJob(s, job.job_id, { unknown_count: 1, last_unknown_ms: now }));
    }
  }

  /** Map a page verdict to a bridge action. See the safety contract: at most one
   * download initiation per job, ever; unknown only escalates after two spaced
   * observations; every other unknown keeps assisted behaviour. */
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
        this.send("provider_outcome", { outcome: "no_entitlement", adapter_version: av }, jobID);
        return;
      case "wrong_work":
      case "wrong_work_check":
        this.send("provider_outcome", { outcome: "wrong_work", adapter_version: av }, jobID);
        return;
      case "unknown":
        await this.recordUnknown(job, host, spec);
        return;
      }
  }

  private async onTabRemoved(tabID: number): Promise<void> {
    await this.ready;
    this.authCountedTabs.delete(tabID);
    if (this.closingTabs.delete(tabID)) return; // programmatic close, not a user cancel
    const job = findByTab(this.store, tabID);
    if (!job) return;
    // Once the user is past authentication (awaiting_download), a closed tab is
    // NOT a cancel: a download may be in flight or already saved into the job's
    // adoption directory, where the daemon's poll-time scan will adopt it. We
    // drop our local tab correlation but leave the job parked daemon-side.
    // Cancelling only stands while the handoff has not yet reached download.
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
    // Bind the download's ID to its job synchronously — before any await — so
    // the onDeterminingFilename event that fires right after onCreated (and must
    // call suggest() synchronously) can relocate the file into papio/<job>/ by
    // ID. This is what lets a cross-origin api/url download land correctly:
    // its provider redirect changes the URL before onDeterminingFilename, but at
    // creation no redirect has occurred yet, so the pending-offer URL matches
    // here and the ID is tracked in time.
    const earlyJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    if (earlyJobID !== undefined) {
      const early = this.downloads.get(earlyJobID) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
      early.ids.add(item.id);
      if (early.ids.size > 1) early.ambiguous = true;
      this.downloads.set(earlyJobID, early);
    }
    await this.ready;
    // API-started downloads usually have no tabId. Match the exact pending
    // offer URL before applying broad tab/provider correlation.
    const exactJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    const job = exactJobID === undefined ? this.correlate(item) : findByJob(this.store, exactJobID);
    if (!job) return; // unrelated tab / unknown origin: ignore entirely
    if (job.download_initiated !== true) {
      // Native browser downloads (not just adapter/viewer API requests) must
      // latch before a later completed-tab event can see a PDF viewer.
      await this.update((s) => patchJob(s, job.job_id, { download_initiated: true }));
    }
    const track = this.downloads.get(job.job_id) ?? { ids: new Set<number>(), ambiguous: false, directOffer: false };
    track.ids.add(item.id);
    if (track.ids.size > 1) track.ambiguous = true; // simultaneous candidates: user decides
    this.downloads.set(job.job_id, track);
  }

  private async onDownloadChanged(delta: DownloadDeltaLike): Promise<void> {
    await this.ready;
    const state = delta.state?.current;
    if (state !== "complete") {
      if (state === "interrupted") {
        for (const job of this.store.activeJobs) {
          const track = this.downloads.get(job.job_id);
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
    if (track.directOffer) {
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

    await this.update((s) =>
      this.clearAuthAttempts(patchJob(s, owner.job_id, { status: "awaiting_download" }), owner.job_id),
    );
    this.authStalledReported.delete(owner.job_id);
    this.send("download_started", { download_id: delta.id, filename }, owner.job_id);
    this.send("download_complete", { download_id: delta.id, filename, size_bytes: size }, owner.job_id);
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

interface InboxRuntimeSender {
  id?: string;
  url?: string;
}

interface InboxRuntimeURLs {
  runtimeID: string;
  inboxURL: string;
  popupURL: string;
  historyURL: string;
}

type InboxRuntimeReply =
  | BrokerFailure
  | { opened: true }
  | { captured: true }
  | BrokerReply<{ snapshot: Record<string, unknown> }>
  | BrokerReply<{ counts: Record<string, unknown>; generated_at: string }>
  | BrokerReply<{ outcome: string; detail?: string }>
  | BrokerReply<{ opened: true }>
  | BrokerReply<{ stats: Record<string, unknown> }>;

function isObjectRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
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

function isInboxOrPopupSender(sender: InboxRuntimeSender, urls: InboxRuntimeURLs): boolean {
  return sender.id === urls.runtimeID && (sender.url === urls.inboxURL || sender.url === urls.popupURL);
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

function isHandoffOpenRuntimeRequest(value: unknown): value is { job_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["job_id"]) &&
    typeof value["job_id"] === "string" &&
    value["job_id"].length > 0 &&
    value["job_id"].length <= 1024
  );
}

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
    if (!hasOnlyKeys(message, ["type", "payload"]) || !isPageCaptureRuntimeRequest(message["payload"])) {
      return runtimeFailure("invalid_request", "Invalid page capture request");
    }
    if (!bridge.pageCaptureAvailable()) return { captured: true };
    return bridge.sendPageCapture(message["payload"])
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
  if (
    type !== "papio.triage.snapshot" &&
    type !== "papio.triage.counts" &&
    type !== "papio.triage.decide" &&
    type !== "papio.action.resolve" &&
    type !== "papio.preview"
  ) {
    return undefined;
  }
  if (!isInboxSender(sender, urls)) {
    return runtimeFailure("unauthorized", "This sender cannot access the inbox broker");
  }
  if (!hasOnlyKeys(message, ["type", "request"])) {
    return runtimeFailure("invalid_request", "Invalid inbox broker request");
  }
  const request = message["request"];
  switch (type) {
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
    case "papio.preview":
      return isPreviewRuntimeRequest(request)
        ? bridge.requestPreview(request)
        : runtimeFailure("invalid_request", "Invalid preview request");
    default:
      return undefined;
  }
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
    backend: chromeBackend(chrome.storage),
    tabs: {
      create: (props) => chrome.tabs.create(props),
      get: (tabID) => chrome.tabs.get(tabID),
      reload: (tabID) => chrome.tabs.reload(tabID),
      remove: (tabID) => chrome.tabs.remove(tabID),
      update: (tabID, props) => chrome.tabs.update(tabID, props),
      query: (query) => chrome.tabs.query(query),
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
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
        message["type"] === "papio.triage.snapshot" ||
        message["type"] === "papio.triage.counts" ||
        message["type"] === "papio.triage.decide" ||
        message["type"] === "papio.action.resolve" ||
        message["type"] === "papio.preview" ||
        message["type"] === "papio.handoff.open" ||
        message["type"] === "papio.stats")
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
  void bridge.start().then(() =>
    initKeepalive(chromeKeepaliveAPI(chrome), {
      trackedJobCount: () => bridge.trackedJobCount(),
      latestOpenURL: () => bridge.latestOpenURL(),
      workWindowID: () => bridge.workWindowIDForKeepalive(),
      onTabPlaced: (tabID) => bridge.foldKeepaliveTab(tabID),
      onAuthenticationChanged: (authenticated) => {
        void bridge.setKeepaliveAuthenticated(authenticated);
      },
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
    }),
  );
}
