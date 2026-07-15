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
} from "./state";
import {
  adapters,
  interpret,
  type AdapterContext,
  type AdapterSpec,
  type PageVerdict,
} from "./adapters/types";
import { observeUnknown } from "./observe";
import { chromeKeepaliveAPI, initKeepalive, isAuthenticationURL } from "./keepalive";
import { routeResolverService, type ResolverRoute } from "./resolver";

export const NATIVE_HOST = "com.orgmentem.papio";
const CHROME_PDF_VIEWER_HOST = "mhjfbmdgcfjbbpaeojofohoefgiehjai";
const AUTH_EVIDENCE_TTL_MS = 30 * 60_000;
const QUEUED_HANDOFF_RELEASE_MS = 45_000;


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
}

export interface TabChangeInfo {
  url?: string | undefined;
  status?: string | undefined;
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
    create(props: { url: string; active: boolean }): Promise<TabInfo>;
    get(tabID: number): Promise<TabInfo>;
    reload(tabID: number): Promise<unknown>;
    remove(tabID: number): Promise<void>;
    onUpdated: Listenable<[number, TabChangeInfo, TabInfo]>;
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
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
  private port: NativePort | null = null;
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

  constructor(private readonly deps: BridgeDeps) {}
  trackedJobCount(): number {
    return this.store.activeJobs.length;
  }

  latestOpenURL(): string | undefined {
    return this.latestOfferOpenURL;
  }


  /** Bind browser listeners (once), open the native connection, send hello, and
   * hydrate persisted job/tab correlation. Safe to call on every SW spin-up.
   * The synchronous prefix (listener bind + connect) runs before the first
   * await, satisfying MV3's top-level-registration expectation. */
  async start(): Promise<void> {
    this.bindListeners();
    this.connect();
    this.ready = this.deps.backend.load().then((s) => {
      this.store = s;
      this.offerURLs.clear();
      for (const [jobID, url] of Object.entries(s.offerURLs ?? {})) {
        if (typeof url !== "string" || findByJob(s, jobID) === undefined) continue;
        this.offerURLs.set(jobID, url);
        this.latestOfferOpenURL = url;
      }
    });
    await this.ready;
    for (const job of this.store.activeJobs) {
      if (job.status === "queued") this.scheduleQueuedHandoffRelease(job.job_id);
    }
    await this.releaseQueuedHandoffs();
    await this.releaseQueuedHandoffsForLiveLanding();
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
  }

  /** Consecutive unplanned disconnects; resets on a healthy inbound frame. */
  private reconnectAttempts = 0;
  /** Set while disconnect() runs so the onDisconnect listener knows the
   * teardown was deliberate (protocol error / shutdown): deliberate
   * disconnects must NOT auto-reconnect — fail closed stays failed. */
  private closingDeliberately = false;

  private connect(): void {
    const port = this.deps.connectNative(NATIVE_HOST);
    this.port = port;
    this.seq = 0;
    port.onMessage.addListener((msg) => {
      if (this.port !== port) return;
      this.reconnectAttempts = 0;
      return this.onInbound(msg);
    });
    port.onDisconnect.addListener(() => {
      // A stale port may report its close after recovery opened a replacement.
      if (this.port !== port) return;
      this.port = null;
      if (this.closingDeliberately) return;
      // Unplanned port death (daemon restart, host exit, Chrome nap): the
      // daemon owns all durable state, so reconnect + re-hello is always
      // safe. Bounded exponential backoff, capped at 60s, gives up after
      // 8 attempts until the next user-visible event restarts the cycle.
      if (this.reconnectAttempts >= 8) return;
      const delay = Math.min(60_000, 1_000 * 2 ** this.reconnectAttempts);
      this.reconnectAttempts += 1;
      this.deps.setTimeout(() => {
        if (this.port === null && !this.closingDeliberately) this.connect();
      }, delay);
    });
    // hello is the mandatory first frame after connect (seq 0).
    const adapterVersions: Record<string, string> = {};
    for (const spec of this.deps.adapterSpecs) adapterVersions[spec.id] = spec.version;
    this.send("hello", {
      extension_version: this.deps.manifestVersion,
      adapter_versions: adapterVersions,
    });
  }

  private disconnect(): void {
    this.closingDeliberately = true;
    const port = this.port;
    this.port = null;
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
    await this.update((s) => {
      const offerURLs = { ...(s.offerURLs ?? {}) };
      delete offerURLs[jobID];
      return { ...removeJob(s, jobID), offerURLs };
    });
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
          tabID = (await this.deps.tabs.create({ url, active: false })).id;
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
  private send(type: BrowserMessageType, payload: Record<string, unknown>, jobID?: string): void {
    const port = this.port;
    if (!port) return;
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
      return;
    }
    port.postMessage(env);
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
      case "hello_ack":
        return;
      case "ack":
        await this.closeAfterAdoption(msg.job_id);
        return;
      case "error":
        console.warn("papio: daemon reported error", msg.payload);
        if (msg.payload.code === "expected_hello") this.reconnectForHello();
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
      tabID = (await this.deps.tabs.create({ url: openurl, active: true })).id;
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
    await this.fallbackToOfferTab(jobID);
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
      tabID = (await this.deps.tabs.create({ url, active: true })).id;
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
    if (!job) return;
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
    const onProvider = hostMatches(host, job.provider_hosts);
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
    if (!hostMatches(host, job.provider_hosts)) return;
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
        // Human is still authenticating: stay auth_pending, emit nothing.
        return;
      case "terms":
        this.send("provider_outcome", { outcome: "terms_acceptance_required", adapter_version: av }, jobID);
        return;
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
    if (track.directOffer) {
      const mime = item?.mime?.split(";", 1)[0]?.trim().toLowerCase();
      if (mime !== "application/pdf") {
        await this.discardDirectOffer(owner.job_id, delta.id);
        return;
      }
    }
    if (!item) return;
    const rawName = item.filename ?? delta.filename?.current ?? "";
    const filename = rawName.split(/[\\/]/).pop() ?? "";
    const size = item.fileSize ?? item.totalBytes ?? item.bytesReceived ?? 0;
    if (filename.length === 0 || size < 1) return; // cannot form a valid frame; leave to the user

    await this.update((s) => patchJob(s, owner.job_id, { status: "awaiting_download" }));
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
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
    },
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
    if (isCancelRequest(message)) {
      void bridge.requestCancel(message.job_id).then(() => sendResponse({ ok: true }));
      return true; // async sendResponse
    }
    return false;
  });
  // KEEPALIVE INTEGRATION
  void bridge.start().then(() =>
    initKeepalive(chromeKeepaliveAPI(chrome), {
      trackedJobCount: () => bridge.trackedJobCount(),
      latestOpenURL: () => bridge.latestOpenURL(),
      onAuthenticationChanged: (authenticated) => {
        void bridge.setKeepaliveAuthenticated(authenticated);
      },
    }),
  );
}
