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
  parseBrowserMessage,
  type BrowserMessage,
  type BrowserMessageType,
  type PageAcquireAckPayload,
  type PageAcquirePayload,
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
  TERMS_CONSENT_KEY,
  WORK_WINDOW_KEY,
} from "./state";
import {
  adapters,
  interpret,
  type AdapterContext,
  type AdapterSpec,
  type PageVerdict,
} from "./adapters/types";
import { takePendingFixtureFilename } from "./capture";
import { observeUnknown } from "./observe";
import { chromeKeepaliveAPI, initKeepalive, isAuthenticationURL } from "./keepalive";
import { routeResolverService, type ResolverRoute } from "./resolver";

export const NATIVE_HOST = "com.orgmentem.papio";
const CHROME_PDF_VIEWER_HOST = "mhjfbmdgcfjbbpaeojofohoefgiehjai";
/** Lowest native daemon that can service this extension. */
const MIN_DAEMON_VERSION = "0.1.0";


const AUTH_EVIDENCE_TTL_MS = 30 * 60_000;
const QUEUED_HANDOFF_RELEASE_MS = 45_000;
// A provider page can classify `unknown` transiently: its adapter selectors
// (custom elements, React roots) upgrade after the tab reports complete and
// after the SSO landing. Re-drive the idempotent classify path on a bounded
// schedule so a slow render still reaches a decisive verdict.
const CLASSIFY_RETRY_MS = 2_500;
const MAX_CLASSIFY_RETRIES = 8;
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

/** Whether this adapter's SPA must render outside the minimized work window. */
export function needsVisibleWindow(spec: AdapterSpec | undefined): boolean {
  return spec?.requiresVisible === true;
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
  /** Chrome sets this on a tab opened by another tab (e.g. a provider's
   * "download" that opens the PDF in a new viewer tab). Correlates the viewer
   * tab back to the tracked handoff tab that spawned it. */
  openerTabId?: number | undefined;
}

export interface TabChangeInfo {
  url?: string | undefined;
  status?: string | undefined;
}

export interface WindowInfo {
  id?: number | undefined;
  /** "minimized" | "normal" | ... — used only to avoid un-maximizing a normal
   * window when surfacing. */
  state?: string | undefined;
  /** Populated by windows.create when the window is created with a URL. */
  tabs?: TabInfo[] | undefined;
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
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
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
      props: { focused?: boolean; state?: "normal" | "minimized" },
    ): Promise<unknown>;
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
  /** chrome.scripting seam. Only ever used to inject the single self-contained
   * `interpret` function (and the one-line download click) on the tracked tab
   * of a granted provider host. */
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      // `never[]` accepts concrete injected signatures without disabling type
      // checking at this serialization boundary.
      func: (...args: never[]) => unknown;
      args?: unknown[];
    }): Promise<{ result?: unknown }[]>;
  };
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
    /** Optional; absent means enabled. Only consulted when `windows` exists. */
    getWorkWindowEnabled?(): Promise<boolean>;
  };
  /** Toolbar badge for connection health. Kept injectable so bridge logic has
   * no dependency on a particular browser global. */
  action: {
    setBadgeText(details: { text: string }): Promise<void>;
    setBadgeBackgroundColor(details: { color: string }): Promise<void>;
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
  /** Authentication observed during this service-worker lifetime. */
  private authReturnedThisWorker = false;
  /** Keepalive has observed its resolver tab return from authentication. */
  private keepaliveAuthenticated = false;
  /** Atomically reserves the one visible handoff while tabs.create is in flight. */
  private handoffOpening = false;
  private drainingQueuedHandoffs = false;
  /** Latest resolver URL from an offer, retained for the keepalive manager. */
  private latestOfferOpenURL: string | undefined;
  /** Pending fallback-release timers, keyed by queued job. Worker-local only. */
  private readonly queuedHandoffTimers = new Map<string, object>();
  /** Forced job IDs awaiting release; consumed by the single active drain so
   * overlapping fallback timers cannot drop each other's requests. */
  private readonly pendingForcedReleases = new Set<string>();
  /** Per-job bounded reclassification attempts while a provider page renders.
   * Worker-local; cleared on a decisive verdict, download, or job removal. */
  private readonly classifyRetries = new Map<string, number>();
  /** Broker-tab ids whose auth attempt is already counted, so the SSO redirect
   * dance within one drive increments the budget only once. Worker-local. */
  private readonly authCountedTabs = new Set<number>();
  /** Jobs already reported human_auth_required this worker lifetime, so a capped
   * job refreshes the daemon's human action at most once per spin-up. */
  private readonly authStalledReported = new Set<string>();
  /** Serializes work-window creation so concurrent offers cannot race two
   * dedicated windows into existence. Worker-local only. */
  private workTabChain: Promise<unknown> = Promise.resolve();

  constructor(private readonly deps: BridgeDeps) {}
  trackedJobCount(): number {
    return this.store.activeJobs.length;
  }

  /** Work-window mode is on when the platform has a windows API and the user
   * has not disabled the setting (absent getter or absent value = enabled). */
  private async workWindowActive(): Promise<boolean> {
    if (this.deps.windows === undefined) return false;
    const get = this.deps.settings.getWorkWindowEnabled;
    if (get === undefined) return true;
    try {
      return await get();
    } catch {
      return true;
    }
  }

  /** Open a broker tab. Work-window tabs stay unfocused and minimized unless a
   * directly matched adapter requires its SPA to render visibly; otherwise the
   * legacy rule applies and `surfaceFallback` decides whether the tab takes
   * focus. Never throws — returns undefined on failure, matching callers. */
  private async openBrokerTab(url: string, surfaceFallback: boolean): Promise<number | undefined> {
    if (await this.workWindowActive()) {
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
    try {
      return (await this.deps.tabs.create({ url, active: surfaceFallback })).id;
    } catch (e) {
      console.error("papio: tab creation failed", e);
      return undefined;
    }
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

  /** Bring a work-window tab to the human: activate it and restore/focus the
   * work window. No-op outside work-window mode (legacy tabs are already
   * visible) and best-effort throughout — auth proceeds regardless. */
  private async surfaceWorkTab(tabID: number): Promise<void> {
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
        ...(win.state === "minimized" ? { state: "normal" as const } : {}),
      });
    } catch {
      // Window gone; the popup badge and notification remain the signal.
    }
  }

  latestOpenURL(): string | undefined {
    return this.latestOfferOpenURL;
  }

  /** The keepalive manager pins its resolver tab inside the work window when
   * one exists, keeping papio's whole footprint out of the user's tab strip. */
  workWindowIDForKeepalive(): number | undefined {
    return this.store.workWindowID;
  }

  /** Keep the persistent daemon-health state visible without interrupting the
   * user. A badge failure is non-fatal: native bridging must keep recovering. */
  async syncConnectionBadge(status = this.store.connectionStatus): Promise<void> {
    try {
      if (status !== "connected") {
        await Promise.all([
          this.deps.action.setBadgeText({ text: "!" }),
          this.deps.action.setBadgeBackgroundColor({ color: "#777777" }),
        ]);
        return;
      }
      let ungranted = 0;
      for (const origin of this.store.resolverOrigins ?? []) {
        try {
          if (!(await this.deps.permissions.contains({ origins: [`${origin}/*`] }))) ungranted += 1;
        } catch {
          ungranted += 1;
        }
      }
      // The contains() calls above are async; if the port dropped meanwhile,
      // onPortDisconnect already painted "!" — don't overwrite it with a stale
      // connected-state badge.
      if (this.store.connectionStatus !== "connected") return;
      if (ungranted > 0) {
        await Promise.all([
          this.deps.action.setBadgeText({ text: String(ungranted) }),
          this.deps.action.setBadgeBackgroundColor({ color: "#1a73e8" }),
        ]);
        return;
      }
      await this.deps.action.setBadgeText({ text: "" });
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
        this.latestOfferOpenURL = url;
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
      await this.update((s) =>
        patchJob(s, job.job_id, { tab_id: -1, status: "queued", download_initiated: false }),
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
      // Fixture writes are data: URLs whose `filename` Chrome ignores; relocate
      // them to the enqueued papio-fixtures path before any job correlation.
      const fixtureName = takePendingFixtureFilename(item.url ?? "");
      if (fixtureName) {
        suggest({ filename: fixtureName, conflictAction: "uniquify" });
        return;
      }
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
  private async onKeepaliveAlarm(): Promise<void> {
    await this.ready;
    if (this.port === null && !this.closingDeliberately) {
      this.reconnectAttempts = 0;
      this.connect();
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
    port.onMessage.addListener((msg) => {
      if (this.port !== port) return;
      this.reconnectAttempts = 0;
      return this.onInbound(msg);
    });
    port.onDisconnect.addListener(() => this.onPortDisconnect(port));
    // hello is the mandatory first frame after connect (seq 0).
    const adapterVersions: Record<string, string> = {};
    for (const spec of this.deps.adapterSpecs) adapterVersions[spec.id] = spec.version;
    this.send("hello", {
      extension_version: this.deps.manifestVersion,
      adapter_versions: adapterVersions,
    });
  }

  private async onPortDisconnect(port: NativePort): Promise<void> {
    // A stale port may report its close after recovery opened a replacement.
    if (this.port !== port) return;
    this.port = null;
    this.failPageAcquireWaiters("The daemon disconnected before acknowledging this page");
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
    // Apply the transform synchronously so in-memory state stays in event order.
    this.store = fn(this.store);
    // Persist after any in-flight save settles, writing the latest snapshot so
    // reordered chrome.storage writes cannot resurrect an older one.
    const save = this.saveChain.then(() => this.deps.backend.save(this.store));
    // Keep the chain alive across a failed save without unhandled rejections;
    // this caller still observes the real error below.
    this.saveChain = save.catch(() => {});
    await save;
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
    this.offerURLs.delete(jobID);
    this.queuedHandoffTimers.delete(jobID);
    this.classifyRetries.delete(jobID);
    this.loginEntityIDs.delete(jobID);
    this.federatedLoginRouted.delete(jobID);
    this.federatedReDriven.delete(jobID);
    this.proquestAccountIDs.delete(jobID);
    this.accountIdAppended.delete(jobID);
    await this.update((s) => {
      const offerURLs = { ...(s.offerURLs ?? {}) };
      delete offerURLs[jobID];
      return { ...removeJob(s, jobID), offerURLs };
    });
  }

  /** Count at most one authentication attempt per broker-tab drive. The SSO
   * redirect dance can toggle auth_pending several times within one tab, so the
   * budget debounces on tab id; each fresh drive (a new tab from a re-offer or a
   * reconcile re-queue) is a distinct attempt. Persisted so attempts accumulate
   * across service-worker restarts within a browser session. */
  private async noteAuthAttempt(jobID: string, tabID: number): Promise<void> {
    if (this.authCountedTabs.has(tabID)) return;
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

  /** Persist evidence from a usable resolver landing in the same durable stamp
   * used by an auth return. The first observed signal also releases any cold
   * queue and refreshes tabs still parked at an IdP. */
  private async recordUsableSession(now: number): Promise<void> {
    const firstAuthEvidence = !this.authReturnedThisWorker;
    this.authReturnedThisWorker = true;
    await this.update((s) => ({ ...s, lastAuthReturnedAt: now }));
    if (firstAuthEvidence) {
      await this.releaseQueuedHandoffs();
      await this.reloadAuthenticationHandoffs();
    }
  }

  /** A queue must not become an invisible permanent sink when an already-warm
   * SSO session never produces an IdP round trip. Timers are deliberately
   * worker-local: startup independently checks durable evidence and live tabs. */
  private scheduleQueuedHandoffRelease(jobID: string): void {
    if (this.queuedHandoffTimers.has(jobID)) return;
    const token = {};
    this.queuedHandoffTimers.set(jobID, token);
    this.deps.setTimeout(async () => {
      if (this.queuedHandoffTimers.get(jobID) !== token) return;
      this.queuedHandoffTimers.delete(jobID);
      await this.ready;
      await this.releaseQueuedHandoffs(jobID);
    }, QUEUED_HANDOFF_RELEASE_MS);
  }

  /** Startup has no worker-local timer state. A tracked tab already settled
   * away from an IdP is the same usable-session evidence as a warm landing. */
  private async releaseQueuedHandoffsForLiveLanding(): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (job.tab_id < 0 || job.status === "queued") continue;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        if (typeof tab.url === "string" && !isAuthenticationURL(tab.url)) {
          await this.recordUsableSession(this.deps.now());
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

  private async releaseQueuedHandoffs(fallbackJobID?: string): Promise<void> {
    if (fallbackJobID !== undefined) this.pendingForcedReleases.add(fallbackJobID);
    if (
      (!this.hasAuthEvidence() && this.pendingForcedReleases.size === 0) ||
      this.drainingQueuedHandoffs
    ) {
      return;
    }
    this.drainingQueuedHandoffs = true;
    try {
      // One drain loop owns both auth-driven draining and every forced release,
      // including those queued by overlapping fallback timers while it runs.
      while (this.hasAuthEvidence() || this.pendingForcedReleases.size > 0) {
        let selected = this.hasAuthEvidence()
          ? this.store.activeJobs.find((job) => job.status === "queued")
          : undefined;
        const forcedJobID =
          selected === undefined ? this.pendingForcedReleases.values().next().value : undefined;
        if (forcedJobID !== undefined) {
          this.pendingForcedReleases.delete(forcedJobID);
          selected = this.store.activeJobs.find(
            (job) => job.job_id === forcedJobID && job.status === "queued",
          );
        }
        if (selected === undefined) {
          // Auth evidence releases every queued job, so none remaining means
          // any leftover forced IDs are moot; drop them and stop.
          if (this.hasAuthEvidence()) {
            this.pendingForcedReleases.clear();
            return;
          }
          continue;
        }
        const queued = selected; // const so narrowing survives the update closure.
        this.queuedHandoffTimers.delete(queued.job_id);
        const url = this.offerURLs.get(queued.job_id);
        if (url === undefined) {
          this.send("job_reject", {}, queued.job_id);
          await this.removeJobWithOffer(queued.job_id);
          continue;
        }
        let tabID: number | undefined;
        try {
          tabID = await this.openBrokerTab(url, false);
        } catch (e) {
          console.error("papio: queued handoff tab creation failed", e);
        }
        if (tabID === undefined) {
          this.send("job_reject", {}, queued.job_id);
          await this.removeJobWithOffer(queued.job_id);
          continue;
        }
        await this.update((s) =>
          patchJob(s, queued.job_id, {
            tab_id: tabID,
            status: "accepted",
            download_initiated: false,
          }),
        );
      }
    } finally {
      this.drainingQueuedHandoffs = false;
    }
  }

  private async reloadAuthenticationHandoffs(): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (job.tab_id < 0 || job.status === "queued") continue;
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

  /** Build, self-validate, and post one outbound frame. Validation is a safety
   * net: a frame that would not survive the shared parser is dropped, never
   * emitted. */
  private send(type: BrowserMessageType, payload: Record<string, unknown>, jobID?: string): boolean {
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
      parseBrowserMessage(env);
    } catch (e) {
      console.error("papio: refusing to send invalid frame", type, e);
      return false;
    }
    port.postMessage(env);
    return true;
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
    switch (msg.type) {
      case "job_offer":
        await this.onJobOffer(msg);
        return;
      case "cancel":
        await this.onCancel(msg);
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
    this.latestOfferOpenURL = openurl;
    const providerHosts = hostsRaw.filter((h): h is string => typeof h === "string");
    const expected = parseExpected(p["expected"]);
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
    if (existing) {
      if (existing.tab_id < 0) {
        if (priorOfferURL === undefined) {
          this.downloads.delete(jobID);
          await this.removeJobWithOffer(jobID);
        } else if (priorOfferURL === openurl) {
          this.send("job_accept", {}, jobID);
          if (existing.status === "queued") await this.releaseQueuedHandoffs();
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
          this.send("job_accept", {}, jobID);
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
    });
    if (isDirectFileOffer(openurl)) {
      await this.upsertJobWithOffer(makeJob(-1), openurl);
      this.send("job_accept", {}, jobID);
      await this.startDirectOfferDownload(jobID, openurl);
      return;
    }

    const queueHandoff =
      !this.hasAuthEvidence() &&
      (this.handoffOpening ||
        this.store.activeJobs.some((job) => job.tab_id >= 0 && job.status !== "queued"));
    if (queueHandoff) {
      await this.upsertJobWithOffer(makeJob(-1, "queued"), openurl);
      this.scheduleQueuedHandoffRelease(jobID);
      this.send("job_accept", {}, jobID);
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
      !this.hasAuthEvidence() &&
      (this.handoffOpening ||
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
    await this.update((s) =>
      patchJob(s, jobID, { tab_id: tabID, status: "accepted", download_initiated: false }),
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
    let host: string;
    try {
      host = new URL(url).hostname;
    } catch {
      return;
    }
    const successfulLanding = change.status === "complete" && !isAuthenticationURL(url);
    if (successfulLanding) await this.recordUsableSession(this.deps.now());
    if (change.status === "complete" && (await this.maybeRouteResolver(job, url))) return;
    // The offer's provider_hosts list is capped by the protocol (20 entries);
    // the adapter registry is the authoritative host source for classification,
    // so a tracked handoff landing on any registered family is on-provider.
    const onProvider =
      hostMatches(host, job.provider_hosts) ||
      this.deps.adapterSpecs.some((candidate) => hostMatches(host, candidate.hosts));
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
      const firstAuthReturn = !this.authReturnedThisWorker;
      this.authReturnedThisWorker = true;
      await this.update((s) => ({
        ...patchJob(s, job.job_id, { status: "awaiting_download" }),
        lastAuthReturnedAt: now,
      }));
      this.send("auth_returned", { elapsed_ms: elapsed }, job.job_id);
      if (firstAuthReturn) {
        await this.releaseQueuedHandoffs();
        await this.reloadAuthenticationHandoffs();
      }
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

  /** Download a tracked PDF-viewer navigation through Chrome's download API.
   * The persisted latch and in-memory correlation jointly ensure that a
   * content-disposition download or repeated completion event cannot start a
   * second download for the same job. Page classification stays exclusively in
   * the declarative adapter path; this method accepts only a PDF URL or the
   * recognized Chrome PDF viewer. */
  private async maybeDownloadPDFViewer(jobID: string, url: string, knownPDFViewer = false): Promise<void> {
    let job = findByJob(this.store, jobID);
    if (!job || (job.status !== "accepted" && job.status !== "awaiting_download")) return;
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
      return result?.kind === "routed";
    } catch (e) {
      console.error("papio: resolver routing failed; staying assisted", e);
      return false;
    }
  }

  /**
   * Classify the tracked tab's current provider page with the single injected
   * `interpret` function, then act on the verdict. No-ops (staying assisted)
   * when there is no registered adapter for the host or the host is not granted
   * via optional_host_permissions. Adapter execution never touches a tab we do
   * not own for this job.
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
    let granted = false;
    try {
      granted = await this.deps.permissions.contains({ origins: [`https://${host}/*`] });
    } catch {
      granted = false;
    }
    if (!granted) return; // host not granted -> stay assisted

    const ctx: AdapterContext = { expected: { ...(job.expected ?? {}) } };
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

  private scheduleClassifyRetry(jobID: string): void {
    const attempts = this.classifyRetries.get(jobID) ?? 0;
    if (attempts >= MAX_CLASSIFY_RETRIES) {
      this.classifyRetries.delete(jobID);
      return;
    }
    this.classifyRetries.set(jobID, attempts + 1);
    this.deps.setTimeout(() => this.retryClassify(jobID), CLASSIFY_RETRY_MS);
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

  private async retryClassify(jobID: string): Promise<void> {
    await this.ready;
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
   * assisted handoff semantics when this provider has no adapter at all. */
  private async recordUnknown(job: ActiveJob, host: string, adapterVersion?: string): Promise<void> {
    if (typeof chrome !== "undefined") await observeUnknown({ scripting: chrome.scripting, downloads: chrome.downloads, storage: chrome.storage }, job, host, () => new Date(this.deps.now()));
    if (adapterVersion === undefined) return;
    const now = this.deps.now();
    const count = job.unknown_count ?? 0;
    const last = job.last_unknown_ms ?? 0;
    if (count >= 1 && now - last >= 5000) {
      // Second unknown, at least 5s after the first: the UI has changed.
      this.send("provider_outcome", { outcome: "ui_changed", adapter_version: adapterVersion }, job.job_id);
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
        if (dl && job.download_initiated !== true) {
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
        await this.recordUnknown(job, host, av);
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
    if (typeof item.tabId === "number") {
      const byTab = findByTab(this.store, item.tabId);
      if (byTab) return byTab;
      // Fall through: provider viewers often download from a child tab the
      // extension did not create; host matching below still requires the
      // download to originate from an advertised provider host.
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
      if (job.download_initiated !== true || job.adapter_id === undefined) return false;
      const spec = this.deps.adapterSpecs.find((candidate) => candidate.id === job.adapter_id);
      return spec !== undefined && hostMatches(host, spec.hosts);
    });
    if (initiated.length === 1) return initiated[0];
    if (initiated.length > 1) return undefined;
    const matches = this.store.activeJobs.filter((job) => hostMatches(host, job.provider_hosts));
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
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
    },
    // chrome.windows is present in every Chromium; guarded for other runtimes.
    ...(typeof chrome.windows !== "undefined"
      ? {
          windows: {
            create: (props: { url: string; focused: boolean; state: "minimized" | "normal" }) =>
              chrome.windows.create(props) as Promise<WindowInfo>,
            get: (windowID: number) => chrome.windows.get(windowID) as Promise<WindowInfo>,
            update: (windowID: number, props: { focused?: boolean; state?: "normal" }) =>
              chrome.windows.update(windowID, props),
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
      async getWorkWindowEnabled() {
        try {
          const got = await chrome.storage.local.get(WORK_WINDOW_KEY);
          return got[WORK_WINDOW_KEY] !== false;
        } catch {
          return true;
        }
      },
    },
    action: {
      setBadgeText: (details) => chrome.action.setBadgeText(details),
      setBadgeBackgroundColor: (details) => chrome.action.setBadgeBackgroundColor(details),
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
  // Top-level registrations give Chrome a reason to start this worker at
  // browser launch and after install/update. Without them a cold-started
  // Chrome leaves the worker dead (and the daemon unreachable) until an
  // unrelated tab or download event happens to fire. bridge.start() already
  // ran at module top level by then; the callbacks need no body.
  chrome.runtime.onStartup.addListener(() => {});
  chrome.runtime.onInstalled.addListener(() => {});
  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
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
  // A grant/revoke from the popup or options page changes what papio can steer;
  // reflect it in the toolbar badge without waiting for the next hello_ack.
  chrome.permissions?.onAdded?.addListener(() => {
    void bridge.syncConnectionBadge();
  });
  chrome.permissions?.onRemoved?.addListener(() => {
    void bridge.syncConnectionBadge();
  });
  // KEEPALIVE INTEGRATION
  void bridge.start().then(() =>
    initKeepalive(chromeKeepaliveAPI(chrome), {
      trackedJobCount: () => bridge.trackedJobCount(),
      latestOpenURL: () => bridge.latestOpenURL(),
      workWindowID: () => bridge.workWindowIDForKeepalive(),
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
