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

export const NATIVE_HOST = "com.orgmentem.papio";

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
  backend: StateBackend;
  tabs: {
    create(props: { url: string; active: boolean }): Promise<TabInfo>;
    get(tabID: number): Promise<TabInfo>;
    remove(tabID: number): Promise<void>;
    onUpdated: Listenable<[number, TabChangeInfo, TabInfo]>;
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
  };
  downloads: {
    search(query: { id: number }): Promise<DownloadItemLike[]>;
    onCreated: Listenable<[DownloadItemLike]>;
    onChanged: Listenable<[DownloadDeltaLike]>;
    /** chrome.downloads.onDeterminingFilename — Chrome-only; absent elsewhere.
     * The listener may call suggest() synchronously to relocate a download to
     * a relative path under the browser's Downloads directory. */
    onDeterminingFilename?: Listenable<
      [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
    >;
  };
}

interface DownloadTrack {
  ids: Set<number>;
  ambiguous: boolean;
}

function hostMatches(host: string, providerHosts: string[]): boolean {
  return providerHosts.some((h) => host === h || host.endsWith("." + h));
}

export class Bridge {
  private port: NativePort | null = null;
  private seq = 0;
  private store: StoreShape = emptyStore();
  private ready: Promise<void> = Promise.resolve();
  private listenersBound = false;
  /** Per-job in-progress download correlation (in-memory; transient). */
  private readonly downloads = new Map<string, DownloadTrack>();
  /** Tabs we are intentionally closing, so onRemoved does not emit a spurious
   * cancelled outcome for a programmatic close. */
  private readonly closingTabs = new Set<number>();

  constructor(private readonly deps: BridgeDeps) {}

  /** Bind browser listeners (once), open the native connection, send hello, and
   * hydrate persisted job/tab correlation. Safe to call on every SW spin-up.
   * The synchronous prefix (listener bind + connect) runs before the first
   * await, satisfying MV3's top-level-registration expectation. */
  async start(): Promise<void> {
    this.bindListeners();
    this.connect();
    this.ready = this.deps.backend.load().then((s) => {
      this.store = s;
    });
    await this.ready;
  }

  /** Cancel an active job on user request (popup cancel button). */
  async requestCancel(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    this.send("provider_outcome", { outcome: "cancelled" }, jobID);
    this.closingTabs.add(job.tab_id);
    try {
      await this.deps.tabs.remove(job.tab_id);
    } catch {
      // Tab may already be gone; the outcome frame is what matters.
    }
    this.downloads.delete(jobID);
    await this.update((s) => removeJob(s, jobID));
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
      // Steer the one job-correlated download into the job's adoption
      // directory under Downloads (papio/<job_id>/<name>) so no manual
      // Save As or file move is ever needed. Unrelated downloads are
      // untouched. suggest() must be called synchronously; correlation
      // uses only already-loaded state.
      const job = this.correlate(item);
      if (!job) return;
      const base = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
      if (base.length === 0) return;
      suggest({ filename: `papio/${job.job_id}/${base}`, conflictAction: "uniquify" });
    });
  }

  private connect(): void {
    const port = this.deps.connectNative(NATIVE_HOST);
    this.port = port;
    this.seq = 0;
    port.onMessage.addListener((msg) => {
      return this.onInbound(msg);
    });
    port.onDisconnect.addListener(() => {
      this.port = null;
    });
    // hello is the mandatory first frame after connect (seq 0).
    this.send("hello", { extension_version: this.deps.manifestVersion, adapter_versions: {} });
  }

  private disconnect(): void {
    const port = this.port;
    this.port = null;
    if (!port) return;
    try {
      port.disconnect();
    } catch {
      // Already torn down.
    }
  }

  private async update(fn: (store: StoreShape) => StoreShape): Promise<void> {
    this.store = fn(this.store);
    await this.deps.backend.save(this.store);
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
      case "ack":
        return;
      case "error":
        console.warn("papio: daemon reported error", msg.payload);
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
    const providerHosts = hostsRaw.filter((h): h is string => typeof h === "string");

    // Restart/re-offer dedup: if we already track this job with a live tab, do
    // not open a second tab — just re-accept using the existing tab.
    const existing = findByJob(this.store, jobID);
    if (existing) {
      let live = false;
      try {
        const tab = await this.deps.tabs.get(existing.tab_id);
        live = tab.id === existing.tab_id;
      } catch {
        live = false;
      }
      if (live) {
        this.send("job_accept", {}, jobID);
        return;
      }
      await this.update((s) => removeJob(s, jobID));
    }

    let tabID: number | undefined;
    try {
      const tab = await this.deps.tabs.create({ url: openurl, active: true });
      tabID = tab.id;
    } catch (e) {
      console.error("papio: tab creation failed; rejecting job", e);
      this.send("job_reject", {}, jobID);
      return;
    }
    if (tabID === undefined) {
      this.send("job_reject", {}, jobID);
      return;
    }

    const now = this.deps.now();
    const expiresMs = Date.parse(expiresAt);
    const job: ActiveJob = {
      job_id: jobID,
      tab_id: tabID,
      offered_at: now,
      expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
      status: "accepted",
      provider_hosts: providerHosts,
    };
    await this.update((s) => upsertJob(s, job));
    this.send("job_accept", {}, jobID);
  }

  private async onCancel(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    // Broker-owned by construction (we only track tabs we created).
    this.closingTabs.add(job.tab_id);
    try {
      await this.deps.tabs.remove(job.tab_id);
    } catch {
      // Tab already closed.
    }
    this.downloads.delete(jobID);
    await this.update((s) => removeJob(s, jobID));
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
    const onProvider = hostMatches(host, job.provider_hosts);
    if (!onProvider) {
      if (job.status !== "auth_pending") {
        // Left every provider host — human authentication has begun. Emit timing
        // only; the URL/host is never serialised.
        await this.update((s) =>
          patchJob(s, job.job_id, { status: "auth_pending", auth_started_ms: this.deps.now() }),
        );
        this.send("auth_pending", {}, job.job_id);
      }
      return;
    }
    if (job.status === "auth_pending") {
      const started = job.auth_started_ms ?? this.deps.now();
      const elapsed = Math.max(0, this.deps.now() - started);
      await this.update((s) => patchJob(s, job.job_id, { status: "awaiting_download" }));
      this.send("auth_returned", { elapsed_ms: elapsed }, job.job_id);
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
      await this.update((s) => removeJob(s, job.job_id));
      return;
    }
    this.send("provider_outcome", { outcome: "cancelled" }, job.job_id);
    this.downloads.delete(job.job_id);
    await this.update((s) => removeJob(s, job.job_id));
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
    const matches = this.store.activeJobs.filter((j) => hostMatches(host, j.provider_hosts));
    return matches.length === 1 ? matches[0] : undefined;
  }

  private async onDownloadCreated(item: DownloadItemLike): Promise<void> {
    await this.ready;
    const job = this.correlate(item);
    if (!job) return; // unrelated tab / unknown origin: ignore entirely
    const track = this.downloads.get(job.job_id) ?? { ids: new Set<number>(), ambiguous: false };
    track.ids.add(item.id);
    if (track.ids.size > 1) track.ambiguous = true; // simultaneous candidates: user decides
    this.downloads.set(job.job_id, track);
  }

  private async onDownloadChanged(delta: DownloadDeltaLike): Promise<void> {
    await this.ready;
    if (delta.state?.current !== "complete") return;
    let owner: ActiveJob | undefined;
    let track: DownloadTrack | undefined;
    for (const j of this.store.activeJobs) {
      const t = this.downloads.get(j.job_id);
      if (t && t.ids.has(delta.id)) {
        owner = j;
        track = t;
        break;
      }
    }
    if (!owner || !track) return;
    if (track.ambiguous || track.ids.size !== 1) return; // zero or multiple matches: stay with the user

    const found = await this.deps.downloads.search({ id: delta.id });
    const item = found[0];
    if (!item) return;
    const rawName = item.filename ?? delta.filename?.current ?? "";
    const filename = rawName.split(/[\\/]/).pop() ?? "";
    const size = item.fileSize ?? item.totalBytes ?? item.bytesReceived ?? 0;
    if (filename.length === 0 || size < 1) return; // cannot form a valid frame; leave to the user

    this.send("download_started", { download_id: delta.id, filename }, owner.job_id);
    this.send("download_complete", { download_id: delta.id, filename, size_bytes: size }, owner.job_id);
    const jobID = owner.job_id;
    this.downloads.delete(jobID);
    await this.update((s) => removeJob(s, jobID)); // extension's work is done; a later tab close is not a cancel
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
    backend: chromeBackend(chrome.storage),
    tabs: {
      create: (props) => chrome.tabs.create(props),
      get: (tabID) => chrome.tabs.get(tabID),
      remove: (tabID) => chrome.tabs.remove(tabID),
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
    },
    downloads: {
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
  };
}

// Wiring runs only inside a real extension service worker, never under bun test.
if (typeof chrome !== "undefined" && chrome.runtime?.id) {
  const bridge = new Bridge(realDeps());
  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (isCancelRequest(message)) {
      void bridge.requestCancel(message.job_id).then(() => sendResponse({ ok: true }));
      return true; // async sendResponse
    }
    return false;
  });
  void bridge.start();
}
