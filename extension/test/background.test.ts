// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Behavioural tests for the MV3 bridge against a minimal fake chrome surface and
// a fake native port. No real chrome, and no wall-clock timers: every fake
// emitter awaits the handler promises it triggers, so the flow is deterministic.

import { expect, test } from "bun:test";

import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape } from "../src/state";
import type { AdapterSpec } from "../src/adapters/types";
import { interpret } from "../src/adapters/types";
import {
  Bridge,
  hasDaemonUpdateHint,
  handleInboxRuntimeMessage,
  needsVisibleWindow,
  type BridgeDeps,
  type DownloadDeltaLike,
  type DownloadItemLike,
  type NativePort,
  type TabChangeInfo,
  type TabInfo,
} from "../src/background";
import { routeResolverService } from "../src/resolver";

const OPENURL = "https://resolver.example.edu/openurl?ctx=abc";
const PROVIDER_HOST = "www.jstor.org";
const EXPIRES = "2027-01-01T00:00:00Z";

// Listeners are registered as promise-returning callbacks; emit awaits them all,
// which makes handler completion observable without any timer.
class FakeEmitter<A extends unknown[]> {
  private readonly cbs: ((...a: A) => unknown)[] = [];
  addListener(cb: (...a: A) => void): void {
    this.cbs.push(cb);
  }
  async emit(...a: A): Promise<void> {
    await Promise.all(this.cbs.map((cb) => cb(...a)));
  }
}

class FakePort implements NativePort {
  readonly posted: object[] = [];
  readonly onMessage = new FakeEmitter<[unknown]>();
  readonly onDisconnect = new FakeEmitter<[]>();
  private readonly frameWaiters = new Set<(message: object) => void>();
  disconnected = false;
  postMessage(msg: object): void {
    this.posted.push(msg);
    for (const waiter of this.frameWaiters) waiter(msg);
  }
  async waitForFrame(type: BrowserMessage["type"]): Promise<BrowserMessage> {
    const existing = this.posted.map(parseBrowserMessage).find((frame) => frame.type === type);
    if (existing !== undefined) return existing;
    return new Promise<BrowserMessage>((resolve) => {
      const waiter = (message: object) => {
        const frame = parseBrowserMessage(message);
        if (frame.type !== type) return;
        this.frameWaiters.delete(waiter);
        resolve(frame);
      };
      this.frameWaiters.add(waiter);
    });
  }
  disconnect(): void {
    this.disconnected = true;
    void this.onDisconnect.emit();
  }
  async inbound(msg: unknown): Promise<void> {
    await this.onMessage.emit(msg);
  }
  /** Simulate unplanned port death (daemon restart) — Chrome fires
   * onDisconnect without the extension calling disconnect(). */
  async emitDisconnect(): Promise<void> {
    await this.onDisconnect.emit();
  }
}

class FakeBackend implements StateBackend {
  store: StoreShape = emptyStore();
  async load(): Promise<StoreShape> {
    return this.store;
  }
  async save(store: StoreShape): Promise<void> {
    this.store = store;
  }
}

class FakeAction {
  readonly texts: string[] = [];
  readonly backgroundColors: string[] = [];
  readonly titles: string[] = [];
  async setBadgeText(details: { text: string }): Promise<void> {
    this.texts.push(details.text);
  }
  async setBadgeBackgroundColor(details: { color: string }): Promise<void> {
    this.backgroundColors.push(details.color);
  }
  async setTitle(details: { title: string }): Promise<void> {
    this.titles.push(details.title);
  }
}

class FakeTabs {
  readonly onUpdated = new FakeEmitter<[number, TabChangeInfo, TabInfo]>();
  readonly onRemoved = new FakeEmitter<[number, { isWindowClosing: boolean }]>();
  readonly created: { url: string; active: boolean; windowId?: number }[] = [];
  readonly removed: number[] = [];
  readonly reloaded: number[] = [];
  /** Tab ids activated through tabs.update({active: true}). */
  readonly activated: number[] = [];
  readonly live = new Map<number, TabInfo>();
  nextId = 100;
  failCreate = false;
  async update(tabID: number, props: { active?: boolean; url?: string }): Promise<TabInfo> {
    if (props.active) this.activated.push(tabID);
    const tab = this.live.get(tabID);
    if (tab && props.url !== undefined) tab.url = props.url;
    return tab ?? {};
  }
  async create(props: { url: string; active: boolean; windowId?: number }): Promise<TabInfo> {
    this.created.push(props);
    if (this.failCreate) throw new Error("tab creation blocked");
    const id = this.nextId++;
    const tab: TabInfo = {
      id,
      url: props.url,
      ...(props.windowId !== undefined ? { windowId: props.windowId } : {}),
    };
    this.live.set(id, tab);
    return tab;
  }
  async query(query: { url: string }): Promise<TabInfo[]> {
    return [...this.live.values()].filter((tab) => tab.url === query.url);
  }
  async get(tabID: number): Promise<TabInfo> {
    const tab = this.live.get(tabID);
    if (!tab) throw new Error("no such tab");
    return tab;
  }
  async reload(tabID: number): Promise<void> {
    if (!this.live.has(tabID)) throw new Error("no such tab");
    this.reloaded.push(tabID);
  }
  async remove(tabID: number): Promise<void> {
    this.removed.push(tabID);
    this.live.delete(tabID);
  }
}

class FakeWindows {
  readonly created: { url: string; focused: boolean; state: string }[] = [];
  readonly updated: { windowID: number; props: { focused?: boolean; state?: string } }[] = [];
  readonly live = new Map<number, { id: number; state: string }>();
  nextId = 500;
  constructor(private readonly tabs: FakeTabs) {}
  async create(props: {
    url: string;
    focused: boolean;
    state: "minimized" | "normal";
  }): Promise<{ id: number; state: string; tabs: TabInfo[] }> {
    this.created.push(props);
    const id = this.nextId++;
    this.live.set(id, { id, state: props.state });
    const tab = await this.tabs.create({ url: props.url, active: false, windowId: id });
    return { id, state: props.state, tabs: [tab] };
  }
  async get(windowID: number): Promise<{ id: number; state: string }> {
    const win = this.live.get(windowID);
    if (!win) throw new Error("no such window");
    return win;
  }
  async update(
    windowID: number,
    props: { focused?: boolean; state?: "normal" },
  ): Promise<unknown> {
    this.updated.push({ windowID, props });
    const win = this.live.get(windowID);
    if (win && props.state !== undefined) win.state = props.state;
    return win ?? {};
  }
  /** Simulate the user closing the work window. */
  close(windowID: number): void {
    this.live.delete(windowID);
  }
}

class FakeDownloads {
  readonly onCreated = new FakeEmitter<[DownloadItemLike]>();
  readonly onChanged = new FakeEmitter<[DownloadDeltaLike]>();
  readonly onDeterminingFilename = new FakeEmitter<
    [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
  >();
  readonly items = new Map<number, DownloadItemLike>();
  readonly started: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }[] = [];
  readonly removedFiles: number[] = [];
  readonly erased: number[] = [];
  failDownload = false;
  async download(options: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }): Promise<number> {
    this.started.push(options);
    if (this.failDownload) throw new Error("download blocked");
    return 900 + this.started.length;
  }
  async removeFile(downloadID: number): Promise<void> {
    this.removedFiles.push(downloadID);
  }
  async erase(query: { id: number }): Promise<number[]> {
    this.erased.push(query.id);
    return [query.id];
  }
  async search(query: { id: number }): Promise<DownloadItemLike[]> {
    const item = this.items.get(query.id);
    return item ? [item] : [];
  }
}

class FakeAlarms {
  readonly onAlarm = new FakeEmitter<[{ name: string }]>();
  readonly created: string[] = [];
  create(name: string): void {
    this.created.push(name);
  }
}

interface Harness {
  bridge: Bridge;
  deps: BridgeDeps;
  port: FakePort;
  ports: FakePort[];
  backend: FakeBackend;
  tabs: FakeTabs;
  downloads: FakeDownloads;
  action: FakeAction;
  windows?: FakeWindows;
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  frames(): BrowserMessage[];
  alarms: FakeAlarms;
  postedStrings(): string[];
}

function makeHarness(
  seed?: StoreShape,
  opts?: { windows?: boolean; workWindowEnabled?: boolean; firefox?: boolean },
): Harness {
  const port = new FakePort();
  const ports = [port];
  let connects = 0;
  const backend = new FakeBackend();
  if (seed) backend.store = seed;
  const tabs = new FakeTabs();
  const downloads = new FakeDownloads();
  if (opts?.firefox === true) Reflect.deleteProperty(downloads, "onDeterminingFilename");
  const windows = opts?.windows === true ? new FakeWindows(tabs) : undefined;
  const clock = { now: 1_700_000_000_000 };
  const timers: { fn: () => void | Promise<void>; ms: number }[] = [];
  const action = new FakeAction();
  const alarms = new FakeAlarms();
  const deps: BridgeDeps = {
    connectNative: () => {
      if (connects++ === 0) return port;
      const next = new FakePort();
      ports.push(next);
      return next;
    },
    randomUUID: () => crypto.randomUUID(),
    manifestVersion: "0.1.0",
    now: () => clock.now,
    setTimeout: (fn, ms) => {
      timers.push({ fn, ms });
    },
    backend,
    tabs,
    downloads,
    // No registered adapters and no granted host: these behavioural tests stay
    // entirely in assisted mode, so the classifier never fires. Adapter mapping
    // is covered in adapters.test.ts.
    adapterSpecs: [],
    scripting: { executeScript: async () => [] },
    permissions: { contains: async () => false },
    settings: {
      getTermsConsent: async () => undefined,
      setTermsConsent: async () => {},
      ...(opts?.workWindowEnabled !== undefined
        ? { getWorkWindowEnabled: async () => opts.workWindowEnabled === true }
        : {}),
    },
    ...(windows !== undefined ? { windows } : {}),
    action,
    alarms: { create: (name) => alarms.create(name), onAlarm: alarms.onAlarm },
  };
  return {
    bridge: new Bridge(deps),
    deps,
    port,
    ports,
    backend,
    tabs,
    downloads,
    action,
    ...(windows !== undefined ? { windows } : {}),
    clock,
    timers,
    alarms,
    frames: () => ports.flatMap((p) => p.posted.map(parseBrowserMessage)),
    postedStrings: () => ports.flatMap((p) => p.posted.map((f) => JSON.stringify(f))),
  };
}

function jobOffer(jobID: string, openurl = OPENURL): unknown {
  return {
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000001",
    job_id: jobID,
    seq: 0,
    payload: {
      openurl,
      provider_hosts: [PROVIDER_HOST],
      access_mode: "assisted",
      expires_at: EXPIRES,
    },
  };
}

const PROVIDER_ADAPTER: AdapterSpec = {
  id: "provider",
  version: "1.0.0",
  hosts: [PROVIDER_HOST],
  classify: [],
};

function helloRequiredError(): unknown {
  return {
    protocol: "papio-browser/1",
    type: "error",
    msg_id: "error_00000001",
    seq: 1,
    payload: {
      code: "expected_hello",
      message: "hello required before browser session can resume",
    },
  };
}

function helloAck(
  payload: { daemon_version?: string; features?: string[]; resolver_origins?: string[] } = {},
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "hello_ack",
    msg_id: "hello_ack_000001",
    seq: 1,
    payload,
  };
}

function extensionOutdatedError(): unknown {
  return {
    protocol: "papio-browser/1",
    type: "error",
    msg_id: "error_00000002",
    seq: 1,
    payload: {
      code: "extension_outdated",
      message: "extension must be updated",
    },
  };
}

function triageCounts(pending = 0): Record<string, number> {
  return {
    pending_total: pending,
    watch_hits: pending,
    actions: 0,
    retractions: 0,
    jobs_working: 0,
    jobs_needs_review: 0,
    failure_groups_7d: 0,
  };
}

function nativeResult(type: string, payload: Record<string, unknown>): unknown {
  return {
    protocol: "papio-browser/1",
    type,
    msg_id: `result_${crypto.randomUUID().replace(/-/g, "")}`,
    seq: 9,
    payload,
  };
}

function snapshotResult(requestID: string, pending = 0, schema: 1 | 2 = 1): unknown {
  return nativeResult("triage_snapshot_response", {
    request_id: requestID,
    schema,
    generated_at: "2027-01-01T00:00:00Z",
    counts: triageCounts(pending),
    items: [],
    has_more: false,
    unsupported_items_count: 0,
  });
}

test("hello is the first outgoing frame with a valid msg_id and seq 0", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const first = h.frames()[0];
  expect(first?.type).toBe("hello");
  expect(first?.seq).toBe(0);
  expect(first?.msg_id).toMatch(/^[A-Za-z0-9_-]{8,64}$/);
  expect(first?.payload["extension_version"]).toBe("0.1.0");
});

test("startup clears a stale badge when persisted daemon health is connected", async () => {
  const h = makeHarness({ ...emptyStore(), connectionStatus: "connected" });
  await h.bridge.start();

  expect(h.action.texts).toEqual([""]);
});

test("hello acknowledgment persists daemon version, features, and connected status", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["browser-v1", "direct-download"] }));

  expect(h.backend.store).toMatchObject({
    connectionStatus: "connected",
    daemonVersion: "0.9.0",
    daemonFeatures: ["browser-v1", "direct-download"],
    daemonUpdateHint: false,
  });
  expect(h.action.texts.at(-1)).toBe("");
});

test("a restarted worker clears persisted page-acquire capability before hello_ack", async () => {
  const h = makeHarness({
    ...emptyStore(),
    connectionStatus: "connected",
    daemonVersion: "0.9.0",
    daemonUpdateHint: true,
    daemonFeatures: ["page_acquire"],
    resolverOrigins: ["https://onesearch.library.example.edu"],
  });
  await h.bridge.start();

  expect(h.backend.store).toMatchObject({
    daemonFeatures: [],
    resolverOrigins: [],
  });
  expect(h.bridge.pageAcquireAvailable()).toBe(false);
  let response: unknown;
  void h.bridge.requestPageAcquire({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
  }).then((value) => {
    response = value;
  });
  await Promise.resolve();
  await Promise.resolve();
  expect(response).toEqual({ error: "Page acquisition is not available from this daemon" });
  expect(h.frames().map((frame) => frame.type)).toEqual(["hello"]);
});


test("relays page acquisition and routes its acknowledgement to the popup", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["page_acquire"] }));

  const acknowledgement = h.bridge.requestPageAcquire({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  });
  await Promise.resolve();
  const request = h.frames().at(-1);
  expect(request?.type).toBe("page_acquire");
  expect(request?.payload).toEqual({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  });
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "page_acquire_ack",
    msg_id: "page-acquire-ack-001",
    seq: 2,
    payload: { job_id: "job_page_acquire_001", duplicate: true },
  });
  expect(await acknowledgement).toEqual({ job_id: "job_page_acquire_001", duplicate: true });
});

test("refuses a DOI-less page acquisition without sending a frame", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["page_acquire"] }));

  let response: unknown;
  void h.bridge.requestPageAcquire({
    url: "https://publisher.example.edu/article/42",
    title: "A DOI-less page",
    source: "popup",
  }).then((value) => {
    response = value;
  });
  await Promise.resolve();
  await Promise.resolve();
  expect(response).toEqual({ error: "page has no DOI" });
  expect(h.frames().map((frame) => frame.type)).toEqual(["hello"]);
});

test("hello_ack caches resolver origins and badges ungranted ones while connected", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async () => false;
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://onesearch.library.example.edu"] }));

  expect(h.backend.store.resolverOrigins).toEqual(["https://onesearch.library.example.edu"]);
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
});

test("a granted resolver origin leaves the connected badge clear", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async ({ origins }) =>
    origins.length === 1 && origins[0] === "https://onesearch.library.example.edu/*";
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://onesearch.library.example.edu"] }));

  expect(h.action.texts.at(-1)).toBe("");
});

test("a stale connected badge sync cannot mask a disconnected state", async () => {
  const h = makeHarness({
    ...emptyStore(),
    connectionStatus: "disconnected",
    resolverOrigins: ["https://onesearch.library.example.edu"],
  });
  h.deps.permissions.contains = async () => false;
  // Called as "connected", but the store already flipped to disconnected while
  // the permission checks were in flight: the guard must skip the count paint.
  await h.bridge.syncConnectionBadge("connected");

  expect(h.action.texts).not.toContain("1");
});

test("hello acknowledgment persists an informational update hint without changing health", async () => {
  Object.assign(globalThis, { __PAPIO_DAEMON_VERSION__: "1.0.0" });
  try {
    const h = makeHarness();
    await h.bridge.start();
    await h.port.inbound(helloAck({ daemon_version: "0.9.0" }));

    expect(h.backend.store).toMatchObject({
      connectionStatus: "connected",
      daemonVersion: "0.9.0",
      daemonUpdateHint: true,
    });
    expect(h.action.texts.at(-1)).toBe("");
  } finally {
    delete (globalThis as Record<string, unknown>).__PAPIO_DAEMON_VERSION__;
  }
});

test("an older daemon's empty hello acknowledgment remains connected", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck());

  expect(h.backend.store).toMatchObject({
    connectionStatus: "connected",
    daemonVersion: null,
    daemonFeatures: [],
  });
});

test("daemon update hints compare released semver cores against the build stamp", () => {
  expect(hasDaemonUpdateHint("0.1.0", "0.2.0")).toBe(true);
  expect(hasDaemonUpdateHint("0.2.0", "0.2.0")).toBe(false);
  expect(hasDaemonUpdateHint("0.3.0", "0.2.0")).toBe(false);
  expect(hasDaemonUpdateHint("0.2.0-dev", "0.2.0")).toBe(false);
  expect(hasDaemonUpdateHint("0.1.0", "0.0.0-dev")).toBe(false);
  expect(hasDaemonUpdateHint(null, "0.2.0")).toBe(false);
  expect(hasDaemonUpdateHint("unknown", "0.2.0")).toBe(false);
});

test("a daemon below the compatibility floor is marked outdated and badged", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.0.9" }));

  expect(h.backend.store.connectionStatus).toBe("daemon_outdated");
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");
});

test("extension-outdated daemon error is persisted and badged", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(extensionOutdatedError());

  expect(h.backend.store.connectionStatus).toBe("extension_outdated");
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");
});

test("job_offer opens exactly one tab and replies job_accept", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001_tyler"));

  expect(h.tabs.created.length).toBe(1);
  expect(h.tabs.created[0]?.url).toBe(OPENURL);
  const accept = h.frames().find((f) => f.type === "job_accept");
  expect(accept?.job_id).toBe("job_0001_tyler");
  expect(h.backend.store.activeJobs.length).toBe(1);
});

test("direct OA file offer downloads before opening a tab and adopts only PDF MIME", async () => {
  const h = makeHarness();
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658941";
  const offer = jobOffer("job_0001a_direct_pdf") as { payload: Record<string, unknown> };
  offer.payload["openurl"] = directURL;
  await h.bridge.start();
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.downloads.started).toEqual([
    {
      url: directURL,
      filename: "papio/job_0001a_direct_pdf/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/paper.pdf",
    fileSize: 64,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  expect(h.frames().some((f) => f.type === "download_complete" && f.job_id === "job_0001a_direct_pdf")).toBe(true);
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "ack",
    msg_id: "ack_00000002",
    job_id: "job_0001a_direct_pdf",
    seq: 1,
    payload: {},
  });
  expect(h.backend.store.activeJobs).toEqual([]);
  expect(h.tabs.removed).toEqual([]);
});

test("non-PDF direct offer removes junk and falls back to the broker tab", async () => {
  const h = makeHarness();
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942";
  const offer = jobOffer("job_0001a_direct_fallback") as { payload: Record<string, unknown> };
  offer.payload["openurl"] = directURL;
  await h.bridge.start();
  await h.port.inbound(offer);
  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/challenge.html",
    fileSize: 64,
    mime: "text/html",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });

  expect(h.downloads.removedFiles).toEqual([901]);
  expect(h.downloads.erased).toEqual([901]);
  expect(h.tabs.created).toEqual([{ url: directURL, active: true }]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(false);
});

test("direct download initiation errors fall back to the broker tab", async () => {
  const h = makeHarness();
  h.downloads.failDownload = true;
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658943";
  const offer = jobOffer("job_0001a_direct_error") as { payload: Record<string, unknown> };
  offer.payload["openurl"] = directURL;
  await h.bridge.start();
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([{ url: directURL, active: true }]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
});

test("tab-less re-offer without a durable offer URL recreates the direct download", async () => {
  const jobID = "job_0001a_stale_direct";
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658944";
  const seed: StoreShape = {
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
        download_initiated: true,
      },
    ],
  };
  const h = makeHarness(seed);
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["openurl"] = directURL;

  await h.bridge.start();
  await h.port.inbound(offer);

  expect(h.downloads.started).toHaveLength(1);
  expect(h.downloads.started[0]?.url).toBe(directURL);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
});

test("offer URLs round-trip through durable state for a worker restart", async () => {
  const jobID = "job_0001a_durable_direct";
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658945";
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["openurl"] = directURL;
  const first = makeHarness();

  await first.bridge.start();
  await first.port.inbound(offer);
  expect(first.backend.store.offerURLs).toEqual({ [jobID]: directURL });

  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  await restarted.bridge.start();
  await restarted.port.inbound(offer);

  expect(restarted.downloads.started).toEqual([]);
  expect(restarted.backend.store.offerURLs).toEqual({ [jobID]: directURL });
});

test("pre-auth handoffs queue behind one visible tab, then release after auth returns", async () => {
  const h = makeHarness();
  const jobIDs = Array.from({ length: 5 }, (_, index) => `job_0001a_queue_${index}`);

  await h.bridge.start();
  await Promise.all(jobIDs.map((jobID) => h.port.inbound(jobOffer(jobID))));

  expect(h.tabs.created.filter((tab) => tab.active)).toHaveLength(1);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(4);

  const activeJob = h.backend.store.activeJobs.find((job) => job.tab_id >= 0);
  expect(activeJob).toBeDefined();
  const activeTabID = activeJob?.tab_id ?? -1;
  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.onUpdated.emit(
    activeTabID,
    { url: idpURL, status: "complete" },
    { id: activeTabID, url: idpURL },
  );
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(4);

  // A pre-existing broker handoff can still be parked at IdP when another tab
  // returns first; auth release must force that stale redirect through cookies.
  const stuckTabID = 999;
  h.backend.store.activeJobs.push({
    job_id: "job_0001a_idp_stuck",
    tab_id: stuckTabID,
    offered_at: h.clock.now,
    expires_at: h.clock.now + 1,
    status: "accepted",
    provider_hosts: [PROVIDER_HOST],
  });
  h.tabs.live.set(stuckTabID, { id: stuckTabID, url: idpURL });

  h.clock.now += 1;
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(activeTabID, { url: providerURL }, { id: activeTabID, url: providerURL });

  expect(h.tabs.created).toHaveLength(5);
  expect(h.tabs.created.filter((tab) => !tab.active)).toHaveLength(4);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(0);
  expect(h.tabs.reloaded).toEqual([stuckTabID]);
});

test("a warm resolver landing releases queued handoffs without an auth event", async () => {
  const h = makeHarness();
  const jobIDs = ["job_0001a_warm_0", "job_0001a_warm_1", "job_0001a_warm_2"];

  await h.bridge.start();
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));
  const firstTabID = h.backend.store.activeJobs.find((job) => job.tab_id >= 0)?.tab_id ?? -1;

  await h.tabs.onUpdated.emit(firstTabID, { url: OPENURL, status: "complete" }, { id: firstTabID, url: OPENURL });

  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
    { url: OPENURL, active: false },
  ]);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toEqual([]);
  expect(h.backend.store.lastAuthReturnedAt).toBe(h.clock.now);
  expect(h.frames().some((frame) => frame.type === "auth_returned")).toBe(false);
});
test("a tracked resolver landing routes its electronic service only with origin permission", async () => {
  const h = makeHarness();
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.permissions.contains = async ({ origins }) =>
    origins.length === 1 && origins[0] === "https://resolver.example.edu/*";
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "routed", service: "JSTOR scholarly archive" } }];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_route"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.onUpdated.emit(tabID, { url: OPENURL, status: "complete" }, { id: tabID, url: OPENURL });

  expect(injections).toHaveLength(1);
  expect(injections[0]?.target).toEqual({ tabId: tabID });
  expect(injections[0]?.func).toBe(routeResolverService);
  expect(injections[0]?.args).toEqual([null]);
  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(false);

  const denied = makeHarness();
  let injectedWithoutPermission = false;
  denied.deps.scripting.executeScript = async () => {
    injectedWithoutPermission = true;
    return [];
  };
  await denied.bridge.start();
  await denied.port.inbound(jobOffer("job_0001a_resolver_denied"));
  const deniedTabID = denied.backend.store.activeJobs[0]?.tab_id ?? -1;
  await denied.tabs.onUpdated.emit(
    deniedTabID,
    { url: OPENURL, status: "complete" },
    { id: deniedTabID, url: OPENURL },
  );
  expect(injectedWithoutPermission).toBe(false);
});

test("a resolver no-entitlement route emits once and short-circuits provider classification", async () => {
  const h = makeHarness();
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.permissions.contains = async () => true;
  h.deps.adapterSpecs.push({ ...PROVIDER_ADAPTER, id: "resolver-provider", hosts: ["resolver.example.edu"] });
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "no_entitlement" } }];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_no_entitlement"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.onUpdated.emit(tabID, { url: OPENURL, status: "complete" }, { id: tabID, url: OPENURL });
  await h.tabs.onUpdated.emit(tabID, { url: OPENURL, status: "complete" }, { id: tabID, url: OPENURL });

  const outcomes = h.frames().filter(
    (frame) => frame.type === "provider_outcome" && frame.payload["outcome"] === "no_entitlement",
  );
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toEqual({ outcome: "no_entitlement" });
  expect(injections).toHaveLength(2);
  expect(injections.every((injection) => injection.func === routeResolverService)).toBe(true);
});

test("a resolver no-service route stays assisted without an outcome", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async () => [{ result: { kind: "no_service" } }];

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_no_service"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.onUpdated.emit(tabID, { url: OPENURL, status: "complete" }, { id: tabID, url: OPENURL });

  expect(h.frames().some((frame) => frame.type === "provider_outcome")).toBe(false);
  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(false);
});

test("a registered adapter host classifies even when absent from the offer's provider_hosts", async () => {
  // The protocol caps provider_hosts at 20 entries, so an offer cannot name
  // every adapter family; the registry is the authoritative host source.
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "unknown" } }];
  };

  await h.bridge.start();
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000002",
    job_id: "job_0001a_registry_host",
    seq: 0,
    payload: {
      openurl: OPENURL,
      provider_hosts: ["resolver.example.edu"],
      access_mode: "assisted",
      expires_at: EXPIRES,
    },
  });
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );

  expect(injections.some((i) => i.func === interpret && i.target.tabId === tabID)).toBe(true);
});

test("a unique manual Chrome download from a registry-only host is correlated", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  await h.bridge.start();
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000003",
    job_id: "job_0001a_registry_manual",
    seq: 0,
    payload: {
      openurl: OPENURL,
      provider_hosts: ["resolver.example.edu"],
      access_mode: "assisted",
      expires_at: EXPIRES,
    },
  });
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(h.backend.store.activeJobs[0]?.adapter_id).toBe(PROVIDER_ADAPTER.id);
  await h.downloads.onCreated.emit({
    id: 31,
    url: `https://${PROVIDER_HOST}/download/article.pdf`,
    state: "in_progress",
  });
  h.downloads.items.set(31, {
    id: 31,
    filename: "/Users/x/Downloads/article.pdf",
    fileSize: 91,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 31, state: { current: "complete" } });

  expect(h.frames().some((frame) => frame.type === "download_complete" && frame.job_id === "job_0001a_registry_manual")).toBe(true);
});

test("a manual registry-host download with two matching jobs remains unowned", async () => {
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: "job_0001a_registry_ambiguous_a",
        tab_id: 100,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: ["resolver.example.edu"],
        adapter_id: PROVIDER_ADAPTER.id,
      },
      {
        job_id: "job_0001a_registry_ambiguous_b",
        tab_id: 101,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: ["resolver.example.edu"],
        adapter_id: PROVIDER_ADAPTER.id,
      },
    ],
  });
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.tabs.live.set(100, { id: 100, url: `https://${PROVIDER_HOST}/stable/a` });
  h.tabs.live.set(101, { id: 101, url: `https://${PROVIDER_HOST}/stable/b` });
  await h.bridge.start();

  await h.downloads.onCreated.emit({
    id: 32,
    url: `https://${PROVIDER_HOST}/download/article.pdf`,
    state: "in_progress",
  });
  h.downloads.items.set(32, {
    id: 32,
    filename: "/Users/x/Downloads/article.pdf",
    fileSize: 91,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 32, state: { current: "complete" } });

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(false);
  expect(h.backend.store.activeJobs.every((job) => job.download_initiated !== true)).toBe(true);
});


test("a queued handoff falls back to a background tab after 45 seconds", async () => {
  const h = makeHarness();

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_timer_active"));
  await h.port.inbound(jobOffer("job_0001a_timer_queued"));
  const fallback = h.timers.find((timer) => timer.ms === 45_000);
  expect(fallback).toBeDefined();

  await fallback?.fn();

  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
  ]);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_0001a_timer_queued")?.status).toBe("accepted");
});

test("startup releases queued handoffs when a tracked tab is already on a non-IdP page", async () => {
  const activeID = 100;
  const queuedURL = "https://resolver.example.edu/openurl?queued=live";
  const h = makeHarness({
    activeJobs: [
      {
        job_id: "job_0001a_live_active",
        tab_id: activeID,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
      {
        job_id: "job_0001a_live_queued",
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: {
      job_0001a_live_active: OPENURL,
      job_0001a_live_queued: queuedURL,
    },
  });
  h.tabs.live.set(activeID, { id: activeID, url: OPENURL });

  await h.bridge.start();

  expect(h.tabs.created).toEqual([{ url: queuedURL, active: false }]);
  expect(h.backend.store.lastAuthReturnedAt).toBe(h.clock.now);
});

test("a recent auth return drains durable queued handoffs during startup", async () => {
  const jobID = "job_0001a_restart_queue";
  const queuedURL = "https://resolver.example.edu/openurl?queued=1";
  const h = makeHarness({
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: { [jobID]: queuedURL },
    lastAuthReturnedAt: 1_700_000_000_000,
  });

  await h.bridge.start();

  expect(h.tabs.created).toEqual([{ url: queuedURL, active: false }]);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
});

test("keepalive authentication evidence releases a restored queued handoff", async () => {
  const jobID = "job_0001a_keepalive_queue";
  const queuedURL = "https://resolver.example.edu/openurl?keepalive=1";
  const h = makeHarness({
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: { [jobID]: queuedURL },
  });

  await h.bridge.start();
  expect(h.tabs.created).toEqual([]);
  await h.bridge.setKeepaliveAuthenticated(true);

  expect(h.tabs.created).toEqual([{ url: queuedURL, active: false }]);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
});

test("a changed re-offer replaces an OA browser tab with the institutional fallback", async () => {
  const h = makeHarness();
  const oaURL = "https://oa.example.org/blocked-paper";
  const institutionalURL = "https://resolver.example.edu/openurl?fallback=1";
  const oaOffer = jobOffer("job_0001b_fallback") as { payload: Record<string, unknown> };
  oaOffer.payload["openurl"] = oaURL;
  const institutionalOffer = jobOffer("job_0001b_fallback") as { payload: Record<string, unknown> };
  institutionalOffer.payload["openurl"] = institutionalURL;

  await h.bridge.start();
  await h.port.inbound(oaOffer);
  await h.port.inbound(institutionalOffer);

  expect(h.tabs.created).toEqual([
    { url: oaURL, active: true },
    { url: institutionalURL, active: true },
  ]);
  expect(h.tabs.removed).toEqual([100]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(101);
  expect(h.frames().filter((f) => f.type === "job_accept" && f.job_id === "job_0001b_fallback")).toHaveLength(2);
});

test("job_reject is sent when tab creation fails", async () => {
  const h = makeHarness();
  h.tabs.failCreate = true;
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0002_fail"));

  expect(h.frames().some((f) => f.type === "job_accept")).toBe(false);
  const reject = h.frames().find((f) => f.type === "job_reject");
  expect(reject?.job_id).toBe("job_0002_fail");
});

test("IdP navigation emits auth_pending once and never leaks the URL/host", async () => {
  const secret = "SENTINEL_SECRET_hunter2_do_not_leak";
  const idpURL = `https://idp.example.edu/sso?SAMLRequest=${secret}#frag=${secret}`;
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0003_auth"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  // Leave the provider host to the IdP (twice — dedup must hold).
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });

  const authPending = h.frames().filter((f) => f.type === "auth_pending");
  expect(authPending.length).toBe(1);
  expect(authPending[0]?.payload).toEqual({});

  // Not one outgoing frame nor any stored state string may carry the sentinel.
  for (const s of h.postedStrings()) expect(s.includes(secret)).toBe(false);
  expect(JSON.stringify(h.backend.store).includes(secret)).toBe(false);

  // Returning to the provider host yields auth_returned with timing only.
  h.clock.now += 4200;
  await h.tabs.onUpdated.emit(tabID, { url: `https://${PROVIDER_HOST}/stable/x` }, { id: tabID });
  const authReturned = h.frames().find((f) => f.type === "auth_returned");
  expect(authReturned?.payload["elapsed_ms"]).toBe(4200);
  expect(Object.keys(authReturned?.payload ?? {})).toEqual(["elapsed_ms"]);
});

test("a job-tab download completes to a basename-only frame; unrelated tab ignored", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_dl"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  // Unrelated download on a different tab: must be ignored entirely.
  await h.downloads.onCreated.emit({ id: 2, tabId: 999, state: "in_progress" });
  h.downloads.items.set(2, { id: 2, tabId: 999, filename: "/tmp/other.pdf", fileSize: 10, state: "complete" });
  await h.downloads.onChanged.emit({ id: 2, state: { current: "complete" } });
  expect(h.frames().some((f) => f.type === "download_complete")).toBe(false);

  // Matching download on the job tab.
  await h.downloads.onCreated.emit({ id: 1, tabId: tabID, state: "in_progress" });
  h.downloads.items.set(1, {
    id: 1,
    tabId: tabID,
    filename: "/Users/x/Downloads/paper final.pdf",
    fileSize: 482913,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 1, state: { current: "complete" } });

  const started = h.frames().find((f) => f.type === "download_started");
  const complete = h.frames().find((f) => f.type === "download_complete");
  expect(started?.job_id).toBe("job_0004_dl");
  expect(complete?.payload["filename"]).toBe("paper final.pdf");
  expect(complete?.payload["size_bytes"]).toBe(482913);
  expect(complete?.payload["download_id"]).toBe(1);
});

test("Firefox keeps click adapters assisted and ignores their manual job-tab downloads", async () => {
  const h = makeHarness(undefined, { firefox: true });
  const clickAdapter: AdapterSpec = {
    ...PROVIDER_ADAPTER,
    download: { selector: "button.download", requireKind: "article", method: "click" },
  };
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.adapterSpecs.push(clickAdapter);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "article" } }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_click"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(injections).toHaveLength(1);
  expect(injections[0]?.func).toBe(interpret);
  expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
  await h.downloads.onCreated.emit({
    id: 41,
    tabId: tabID,
    url: `https://${PROVIDER_HOST}/download/article.pdf`,
    state: "in_progress",
  });
  h.downloads.items.set(41, {
    id: 41,
    tabId: tabID,
    filename: "/Users/x/Downloads/article.pdf",
    fileSize: 91,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 41, state: { current: "complete" } });

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
});

test("Firefox ignores manual downloads from non-click adapters without exact ownership", async () => {
  const h = makeHarness(undefined, { firefox: true });
  const hrefAdapter: AdapterSpec = {
    ...PROVIDER_ADAPTER,
    id: "firefox-href",
    download: { selector: "a.download", requireKind: "article", method: "href" },
  };
  h.deps.adapterSpecs.push(hrefAdapter);
  h.deps.permissions.contains = async () => false;
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_href"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(h.backend.store.activeJobs[0]?.adapter_id).toBe("firefox-href");
  await h.downloads.onCreated.emit({
    id: 42,
    tabId: tabID,
    url: `https://${PROVIDER_HOST}/download/article.pdf`,
    state: "in_progress",
  });
  h.downloads.items.set(42, {
    id: 42,
    tabId: tabID,
    filename: "/Users/x/Downloads/article.pdf",
    fileSize: 91,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 42, state: { current: "complete" } });

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
});

test("Firefox adapter API downloads remain filename-controlled and report normally", async () => {
  const h = makeHarness(undefined, { firefox: true });
  const apiAdapter: AdapterSpec = {
    ...PROVIDER_ADAPTER,
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      method: "api",
      urlTemplate: `https://${PROVIDER_HOST}/api/article`,
      jsonField: "pdf_url",
    },
  };
  h.deps.adapterSpecs.push(apiAdapter);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) =>
    injection.func === interpret
      ? [{ result: { kind: "article" } }]
      : [{ result: `https://${PROVIDER_HOST}/download/article.pdf` }];
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_api"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(h.downloads.started).toEqual([
    {
      url: `https://${PROVIDER_HOST}/download/article.pdf`,
      filename: "papio/job_0004_firefox_api/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
  await h.downloads.onCreated.emit({ id: 901, state: "in_progress" });
  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/article.pdf",
    fileSize: 91,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });

  expect(h.frames().some((frame) => frame.type === "download_complete" && frame.job_id === "job_0004_firefox_api")).toBe(true);
});

test("a PDF-viewer tab starts one download and closes after the adopted file completes", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0010_pdf_viewer"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const viewerURL = `https://${PROVIDER_HOST}/reader/blocked-paper.pdf`;

  await h.tabs.onUpdated.emit(tabID, { url: viewerURL, status: "complete" }, { id: tabID, url: viewerURL });
  await h.tabs.onUpdated.emit(tabID, { url: viewerURL, status: "complete" }, { id: tabID, url: viewerURL });
  expect(h.downloads.started).toEqual([
    {
      url: viewerURL,
      filename: "papio/job_0010_pdf_viewer/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);

  await h.downloads.onCreated.emit({ id: 901, tabId: tabID, state: "in_progress" });
  h.downloads.items.set(901, {
    id: 901,
    tabId: tabID,
    filename: "/Users/x/Downloads/paper.pdf",
    fileSize: 128,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  expect(h.tabs.removed).toEqual([]);
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "ack",
    msg_id: "ack_00000001",
    job_id: "job_0010_pdf_viewer",
    seq: 1,
    payload: {},
  });

  expect(h.frames().some((f) => f.type === "download_complete" && f.job_id === "job_0010_pdf_viewer")).toBe(true);
  expect(h.tabs.removed).toEqual([tabID]);
});

test("Chrome's built-in PDF viewer downloads the memory-only offered URL", async () => {
  const h = makeHarness();
  const offeredURL = "https://oa.example.org/opaque-download";
  const offer = jobOffer("job_0010b_chrome_viewer") as { payload: Record<string, unknown> };
  offer.payload["openurl"] = offeredURL;
  await h.bridge.start();
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const chromeViewerURL = "chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html";

  await h.tabs.onUpdated.emit(
    tabID,
    { url: chromeViewerURL, status: "complete" },
    { id: tabID, url: chromeViewerURL },
  );

  expect(h.downloads.started).toEqual([
    {
      url: offeredURL,
      filename: "papio/job_0010b_chrome_viewer/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
});

test("a pre-existing content-disposition download prevents PDF-viewer duplication", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0011_pdf_dedup"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await h.downloads.onCreated.emit({ id: 77, tabId: tabID, state: "in_progress" });
  const pdfURL = `https://${PROVIDER_HOST}/download/paper.pdf`;
  await h.tabs.onUpdated.emit(tabID, { url: pdfURL, status: "complete" }, { id: tabID, url: pdfURL });

  expect(h.downloads.started).toEqual([]);
});

test("a correlated download is steered into papio/<job_id>/; unrelated untouched", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0007_steer"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    { id: 5, tabId: tabID, filename: "Trust_in_Automation.pdf", state: "in_progress" },
    (s) => suggestions.push(s),
  );
  expect(suggestions).toEqual([
    { filename: "papio/job_0007_steer/Trust_in_Automation.pdf", conflictAction: "uniquify" },
  ]);

  // Unrelated download (different tab, unknown host): never steered.
  await h.downloads.onDeterminingFilename.emit(
    { id: 6, tabId: 999, url: "https://example.org/x.pdf", filename: "x.pdf", state: "in_progress" },
    (s) => suggestions.push(s),
  );
  expect(suggestions.length).toBe(1);
});
test("closing the tab before auth cancels; after auth (awaiting_download) does not", async () => {
  // Before auth return: tab close is a genuine user cancel.
  const pre = makeHarness();
  await pre.bridge.start();
  await pre.port.inbound(jobOffer("job_0008_precancel"));
  const preTab = pre.backend.store.activeJobs[0]?.tab_id ?? -1;
  await pre.tabs.onRemoved.emit(preTab, { isWindowClosing: false });
  expect(pre.frames().some((f) => f.type === "provider_outcome")).toBe(true);
  expect(pre.backend.store.activeJobs.length).toBe(0);

  // After auth return: job is awaiting_download; a closed tab must NOT cancel
  // (the download may be saved for daemon-side adoption).
  const post = makeHarness();
  await post.bridge.start();
  await post.port.inbound(jobOffer("job_0009_postauth"));
  const postTab = post.backend.store.activeJobs[0]?.tab_id ?? -1;
  post.tabs.live.set(postTab, { id: postTab, url: `https://${PROVIDER_HOST}/x` });
  await post.tabs.onUpdated.emit(postTab, { url: "https://idp.example.edu/sso" }, { id: postTab, url: "https://idp.example.edu/sso" });
  await post.tabs.onUpdated.emit(postTab, { url: `https://${PROVIDER_HOST}/y` }, { id: postTab, url: `https://${PROVIDER_HOST}/y` });
  expect(post.backend.store.activeJobs[0]?.status).toBe("awaiting_download");
  await post.tabs.onRemoved.emit(postTab, { isWindowClosing: false });
  expect(post.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(post.backend.store.activeJobs.length).toBe(0);
});


test("a malformed inbound frame fails closed by disconnecting", async () => {
  const h = makeHarness();
  await h.bridge.start();
  expect(h.port.disconnected).toBe(false);
  await h.port.inbound({ protocol: "papio-browser/1", type: "not_a_type", msg_id: "x", seq: 0, payload: {} });
  expect(h.port.disconnected).toBe(true);
});

test("restart recovery re-hellos and does not duplicate a live tab", async () => {
  const seed: StoreShape = {
    activeJobs: [
      {
        job_id: "job_0006_restart",
        tab_id: 100,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  };
  const h = makeHarness(seed);
  h.tabs.live.set(100, { id: 100, url: `https://${PROVIDER_HOST}/x` });
  await h.bridge.start();

  expect(h.frames()[0]?.type).toBe("hello");

  // Daemon re-offers the already-tracked job.
  await h.port.inbound(jobOffer("job_0006_restart"));

  expect(h.tabs.created.length).toBe(0);
  const accept = h.frames().find((f) => f.type === "job_accept");
  expect(accept?.job_id).toBe("job_0006_restart");
});

test("hello-required error reconnects once and does not duplicate a live tab", async () => {
  const seed: StoreShape = {
    activeJobs: [
      {
        job_id: "job_0007_session",
        tab_id: 100,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  };
  const h = makeHarness(seed);
  h.tabs.live.set(100, { id: 100, url: `https://${PROVIDER_HOST}/x` });
  await h.bridge.start();

  await h.port.inbound(helloRequiredError());

  expect(h.port.disconnected).toBe(true);
  expect(h.ports.length).toBe(2);
  expect(h.timers.length).toBe(0);
  expect(h.frames().filter((f) => f.type === "hello").length).toBe(2);

  // The fresh daemon re-offers the durable job; the tracked live tab is reused.
  await h.ports[1]?.inbound(jobOffer("job_0007_session"));
  expect(h.tabs.created.length).toBe(0);
  expect(h.frames().filter((f) => f.type === "job_accept" && f.job_id === "job_0007_session").length).toBe(1);
});

test("unplanned port death marks the badge unhealthy and reconnect clears it", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck());
  expect(h.action.texts.at(-1)).toBe("");

  await h.port.emitDisconnect();
  expect(h.timers.length).toBe(1);
  expect(h.timers[0]?.ms).toBe(1000);
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");

  await h.timers[0]?.fn();
  await h.ports[1]?.inbound(helloAck());
  expect(h.action.texts.at(-1)).toBe("");

  // Deliberate: malformed frame -> fail-closed disconnect, no timer scheduled.
  const bad = makeHarness();
  await bad.bridge.start();
  const timersBefore = bad.timers.length;
  await bad.port.inbound({ protocol: "papio-browser/1", type: "not_a_type", msg_id: "x", seq: 0, payload: {} });
  expect(bad.port.disconnected).toBe(true);
  expect(bad.timers.length).toBe(timersBefore);
});

test("backoff exhaustion leaves the daemon-unavailable badge set", async () => {
  const h = makeHarness();
  await h.bridge.start();
  for (let attempt = 0; attempt <= 8; attempt += 1) {
    await h.ports.at(-1)?.emitDisconnect();
    if (attempt < 8) await h.timers.at(-1)?.fn();
  }

  expect(h.timers).toHaveLength(8);
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");
});

test("concurrent fallback timers release every queued handoff", async () => {
  const h = makeHarness();
  // First offer opens the one visible tab; the rest queue behind it, each with
  // its own worker-local fallback timer.
  const jobIDs = ["job_conc_active", "job_conc_1", "job_conc_2", "job_conc_3"];

  await h.bridge.start();
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(3);
  const fallbacks = h.timers.filter((timer) => timer.ms === 45_000);
  expect(fallbacks).toHaveLength(3);

  // Fire every fallback timer at once. Their drains overlap, so a single-flight
  // guard that dropped concurrent forced releases would strand all but one job
  // durably queued with tab_id -1. Every queued handoff must instead release.
  await Promise.all(fallbacks.map((timer) => timer.fn()));

  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(0);
  expect(h.backend.store.activeJobs.every((job) => job.tab_id >= 0)).toBe(true);
  expect(h.tabs.created.filter((tab) => !tab.active)).toHaveLength(3);
});

// OrderBackend records the peak number of saves in flight at once. Each save
// yields on a microtask (never a real timer), so genuinely concurrent saves
// overlap deterministically while serialized ones never do.
class OrderBackend implements StateBackend {
  store: StoreShape = emptyStore();
  maxInFlight = 0;
  private inFlight = 0;
  async load(): Promise<StoreShape> {
    return this.store;
  }
  async save(store: StoreShape): Promise<void> {
    this.inFlight += 1;
    this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
    await Promise.resolve(); // microtask hop: overlapping writes stay in flight together.
    this.store = store;
    this.inFlight -= 1;
  }
}

test("overlapping state writes persist serially so no stale snapshot wins", async () => {
  const port = new FakePort();
  const tabs = new FakeTabs();
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const backend = new OrderBackend();
  // The seeded handoff needs a live tab so startup reconciliation keeps it.
  tabs.live.set(100, { id: 100, url: `https://${PROVIDER_HOST}/seed` });
  // A pre-existing visible handoff forces the two new offers to queue (a pure
  // state write with no intervening tab creation).
  backend.store = {
    activeJobs: [
      {
        job_id: "seed_visible",
        tab_id: 100,
        offered_at: clock.now,
        expires_at: clock.now + 1_000,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: {},
  };
  const deps: BridgeDeps = {
    connectNative: () => port,
    randomUUID: () => crypto.randomUUID(),
    manifestVersion: "0.1.0",
    now: () => clock.now,
    setTimeout: () => {},
    backend,
    tabs,
    downloads,
    adapterSpecs: [],
    scripting: { executeScript: async () => [] },
    permissions: { contains: async () => false },
    settings: { getTermsConsent: async () => undefined, setTermsConsent: async () => {} },
    action: {
      setBadgeText: async () => {},
      setBadgeBackgroundColor: async () => {},
    },
    alarms: { create: () => {}, onAlarm: { addListener: () => {} } },
  };
  const bridge = new Bridge(deps);
  await bridge.start();
  backend.maxInFlight = 0; // ignore any hydration write during start.

  // Two Chrome events land at once; each mutates state and persists a snapshot.
  await Promise.all([port.inbound(jobOffer("job_write_a")), port.inbound(jobOffer("job_write_b"))]);

  // Serialized persistence keeps at most one save in flight at any moment.
  // Without the save chain both writes overlap and a reordered chrome.storage
  // write could persist an older snapshot.
  expect(backend.maxInFlight).toBe(1);

  const ids = backend.store.activeJobs.map((job) => job.job_id).sort();
  expect(ids).toEqual(["job_write_a", "job_write_b", "seed_visible"]);
});

test("work-window mode routes the first handoff into one minimized unfocused window", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_first"));

  expect(h.windows?.created).toEqual([{ url: OPENURL, focused: false, state: "minimized" }]);
  // The tab was created by windows.create inside the work window, never
  // focused, and the job tracks it like any broker tab.
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false, windowId: 500 }]);
  expect(h.backend.store.workWindowID).toBe(500);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
  const accept = h.frames().find((f) => f.type === "job_accept");
  expect(accept?.job_id).toBe("job_ww_first");
});

test("requiresVisible is opt-in and fails closed for unmatched adapters", () => {
  const cases: { spec: AdapterSpec | undefined; wantsVisible: boolean }[] = [
    { spec: undefined, wantsVisible: false },
    { spec: PROVIDER_ADAPTER, wantsVisible: false },
    { spec: { ...PROVIDER_ADAPTER, requiresVisible: true }, wantsVisible: true },
  ];
  for (const { spec, wantsVisible } of cases) {
    expect(needsVisibleWindow(spec)).toBe(wantsVisible);
  }
});

test("work-window visibility follows the matched adapter requirement", async () => {
  const cases: {
    adapterSpecs: AdapterSpec[];
    expectedState: string;
    expectedUpdates: { windowID: number; props: { focused?: boolean; state?: string } }[];
  }[] = [
    {
      adapterSpecs: [{ ...PROVIDER_ADAPTER, requiresVisible: true }],
      expectedState: "normal",
      expectedUpdates: [{ windowID: 500, props: { focused: false, state: "normal" } }],
    },
    { adapterSpecs: [PROVIDER_ADAPTER], expectedState: "minimized", expectedUpdates: [] },
    { adapterSpecs: [], expectedState: "minimized", expectedUpdates: [] },
  ];
  for (const [index, c] of cases.entries()) {
    const h = makeHarness(undefined, { windows: true });
    h.deps.adapterSpecs = c.adapterSpecs;
    await h.bridge.start();
    await h.port.inbound(jobOffer(`job_ww_visibility_${index}`));
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    const url = `https://${PROVIDER_HOST}/stable/123`;

    await h.tabs.onUpdated.emit(tabID, { status: "complete", url }, { id: tabID, url });

    expect(h.windows?.live.get(500)?.state).toBe(c.expectedState);
    expect(h.windows?.updated).toEqual(c.expectedUpdates);
  }
});

test("a directly matched visible-required handoff opens a normal unfocused window", async () => {
  const h = makeHarness(undefined, { windows: true });
  h.deps.adapterSpecs = [{ ...PROVIDER_ADAPTER, requiresVisible: true }];
  const providerURL = `https://${PROVIDER_HOST}/stable/123`;
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_visible_target", providerURL));

  expect(h.windows?.created).toEqual([{ url: providerURL, focused: false, state: "normal" }]);
  expect(h.windows?.updated).toEqual([]);
});

test("work window is reused across offers and recreated after the user closes it", async () => {
  // Warm auth evidence so every offer opens immediately instead of queueing.
  const h = makeHarness(
    { ...emptyStore(), lastAuthReturnedAt: 1_700_000_000_000 },
    { windows: true },
  );
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_a"));
  await h.port.inbound(jobOffer("job_ww_b"));

  expect(h.windows?.created.length).toBe(1);
  expect(h.tabs.created.map((t) => t.windowId)).toEqual([500, 500]);

  // User closes the work window: the next offer recreates it.
  h.windows?.close(500);
  await h.port.inbound(jobOffer("job_ww_c"));
  expect(h.windows?.created.length).toBe(2);
  expect(h.backend.store.workWindowID).toBe(501);
});

test("IdP navigation surfaces the work-window tab: activate + restore + focus", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_auth"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });

  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(true);
  expect(h.tabs.activated).toEqual([tabID]);
  expect(h.windows?.updated).toEqual([
    { windowID: 500, props: { focused: true, state: "normal" } },
  ]);
});

test("disabling the work-window setting restores the legacy visible handoff", async () => {
  const h = makeHarness(undefined, { windows: true, workWindowEnabled: false });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_off"));

  expect(h.windows?.created).toEqual([]);
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  expect(h.backend.store.workWindowID).toBeUndefined();
});

test("an HTML adapter download is refused, discarded, and reported as download_not_pdf", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0020_html_trap"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  // The provider served its "get access" page where the PDF should be —
  // adopting it would only bounce off the daemon's %PDF validation.
  await h.downloads.onCreated.emit({ id: 7, tabId: tabID, state: "in_progress" });
  h.downloads.items.set(7, {
    id: 7,
    tabId: tabID,
    filename: "/Users/x/Downloads/1071181319631264.pdf",
    fileSize: 48210,
    mime: "text/html",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 7, state: { current: "complete" } });

  expect(h.frames().some((f) => f.type === "download_started")).toBe(false);
  expect(h.frames().some((f) => f.type === "download_complete")).toBe(false);
  const error = h.frames().find((f) => f.type === "error");
  expect(error?.job_id).toBe("job_0020_html_trap");
  expect(error?.payload["code"]).toBe("download_not_pdf");
  expect(h.downloads.removedFiles).toContain(7);

  // A genuine PDF on the same job afterwards still adopts normally.
  await h.downloads.onCreated.emit({ id: 8, tabId: tabID, state: "in_progress" });
  h.downloads.items.set(8, {
    id: 8,
    tabId: tabID,
    filename: "/Users/x/Downloads/real.pdf",
    fileSize: 91,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 8, state: { current: "complete" } });
  expect(h.frames().some((f) => f.type === "download_complete")).toBe(true);
});

test("inbox runtime messages validate the exact extension sender", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
  };
  const message = { type: "papio.triage.counts", request: {} };

  for (const sender of [
    { id: "papio-test-id", url: "chrome-extension://papio-test-id/options.html" },
    { id: "papio-test-id", url: "https://provider.example/article" },
    { id: "other-extension", url: urls.inboxURL },
  ]) {
    await expect(handleInboxRuntimeMessage(h.bridge, message, sender, urls)).resolves.toEqual({
      ok: false,
      error: { code: "unauthorized", message: "This sender cannot access the inbox broker" },
    });
  }

  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: [] }));
  await expect(handleInboxRuntimeMessage(h.bridge, message, { id: urls.runtimeID, url: urls.inboxURL }, urls)).resolves
    .toEqual({
      ok: false,
      error: {
        code: "feature_unavailable",
        message: "This daemon does not support the requested inbox feature",
      },
    });
});

test("open inbox runtime request focuses the singleton or creates it from the popup", async () => {
  const h = makeHarness(undefined, { windows: true });
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
  };
  h.tabs.live.set(88, { id: 88, url: urls.inboxURL, windowId: 600 });
  h.windows?.live.set(600, { id: 600, state: "minimized" });

  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.openInbox" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toEqual({ opened: true });
  expect(h.tabs.activated).toEqual([88]);
  expect(h.windows?.updated).toContainEqual({ windowID: 600, props: { focused: true } });
  expect(h.tabs.created).toEqual([]);

  h.tabs.live.clear();
  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.openInbox" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toEqual({ opened: true });
  expect(h.tabs.created).toEqual([{ url: urls.inboxURL, active: true }]);

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.openInbox" },
      { id: urls.runtimeID, url: "chrome-extension://papio-test-id/options.html" },
      urls,
    ),
  ).resolves.toMatchObject({ ok: false, error: { code: "unauthorized" } });
});

test("inbox handoff runtime opening focuses the live offered tab without returning its URL", async () => {
  const h = makeHarness(undefined, { windows: true });
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_inbox_open"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.handoff.open", request: { job_id: "job_0001a_inbox_open" } },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.activated).toEqual([tabID]);
  const liveTab = h.tabs.live.get(tabID);
  if (liveTab?.windowId === undefined) throw new Error("The offered tab has no live window");
  expect(h.windows?.updated).toContainEqual({
    windowID: liveTab.windowId,
    props: { focused: true, state: "normal" },
  });

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.handoff.open", request: { job_id: "", unexpected: true } },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "invalid_request", message: "Invalid handoff open request" },
  });
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.handoff.open", request: { job_id: "job_0001a_inbox_open" } },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "unauthorized", message: "This sender cannot access the inbox broker" },
  });
});

test("queued inbox handoff force-releases exactly one live tab under racing opens", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_handoff_active"));
  await h.port.inbound(jobOffer("job_0001a_handoff_queued"));

  const queuedID = "job_0001a_handoff_queued";
  expect(h.backend.store.activeJobs.find((job) => job.job_id === queuedID)?.status).toBe("queued");
  const [first, second] = await Promise.all([h.bridge.openHandoff(queuedID), h.bridge.openHandoff(queuedID)]);
  const released = h.backend.store.activeJobs.find((job) => job.job_id === queuedID);
  const releasedTabID = released?.tab_id ?? -1;

  expect(first).toEqual({ ok: true, opened: true });
  expect(second).toEqual({ ok: true, opened: true });
  expect(releasedTabID).toBeGreaterThanOrEqual(100);
  expect(h.tabs.created).toHaveLength(2);
  expect(h.tabs.activated).toContain(releasedTabID);
  expect(h.windows?.updated.some((update) => update.windowID === h.tabs.live.get(releasedTabID)?.windowId)).toBe(true);
});

test("an unknown inbox handoff makes one counts refresh before failing unavailable", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["triage_snapshot_v1"] }));

  const pending = h.bridge.openHandoff("job_0001a_not_offered");
  const refresh = await h.port.waitForFrame("triage_counts_request");
  const refreshes = h.frames().filter((frame) => frame.type === "triage_counts_request");
  const requestID = refresh.payload["request_id"];
  expect(refreshes).toHaveLength(1);
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("triage_counts_response", { request_id: requestID as string, counts: triageCounts() }),
  );

  await expect(pending).resolves.toEqual({
    ok: false,
    error: { code: "handoff_unavailable", message: "The requested handoff is not available" },
  });
  expect(h.frames().filter((frame) => frame.type === "triage_counts_request")).toHaveLength(1);
});

test("triage native replies correlate by request_id even when they arrive out of order", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: "0.9.0",
      features: ["triage_snapshot_v1", "triage_mutations_v1", "review_preview_v1"],
    }),
  );

  const first = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  const second = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  await Promise.resolve();
  await Promise.resolve();
  const requests = h.frames().filter((frame) => frame.type === "triage_snapshot_request");
  expect(requests.map((frame) => frame.payload["schema_versions"])).toEqual([[1], [1]]);
  const firstID = requests[0]?.payload["request_id"];
  const secondID = requests[1]?.payload["request_id"];
  expect(typeof firstID).toBe("string");
  expect(typeof secondID).toBe("string");

  await h.port.inbound(snapshotResult(secondID as string, 2));
  await h.port.inbound(snapshotResult(firstID as string, 1));
  await expect(first).resolves.toMatchObject({ ok: true, snapshot: { counts: { pending_total: 1 } } });
  await expect(second).resolves.toMatchObject({ ok: true, snapshot: { counts: { pending_total: 2 } } });
});

test("triage snapshot uses schema 2 only after the daemon advertises it", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: "0.9.0",
      features: ["triage_snapshot_v1", "triage_snapshot_schema_v2"],
    }),
  );

  const pending = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  await Promise.resolve();
  await Promise.resolve();
  const request = h.frames().find((frame) => frame.type === "triage_snapshot_request");
  expect(request?.payload["schema_versions"]).toEqual([2]);
  const requestID = request?.payload["request_id"];
  await h.port.inbound(snapshotResult(requestID as string, 1, 2));
  await expect(pending).resolves.toMatchObject({ ok: true });
});

test("triage requests time out and late echoes are dropped", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["triage_snapshot_v1"] }));
  const pending = h.bridge.requestTriageCounts();
  await Promise.resolve();
  await Promise.resolve();
  const request = h.frames().find((frame) => frame.type === "triage_counts_request");
  const requestID = request?.payload["request_id"];
  const timeout = h.timers.find((timer) => timer.ms === 15_000);
  expect(typeof requestID).toBe("string");
  expect(timeout).toBeDefined();

  const originalDebug = console.debug;
  const debugLines: unknown[][] = [];
  console.debug = (...args: unknown[]) => {
    debugLines.push(args);
  };
  try {
    await timeout?.fn();
    await expect(pending).resolves.toMatchObject({ ok: false, error: { code: "timeout" } });
    await h.port.inbound(
      nativeResult("triage_counts_response", { request_id: requestID as string, counts: triageCounts(3) }),
    );
  } finally {
    console.debug = originalDebug;
  }
  expect(debugLines.some((line) => line.join(" ").includes("unknown or late triage response"))).toBe(true);
});

test("a user-visible triage request forces reconnect and waits for a fresh hello", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.emitDisconnect();

  const pending = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  await Promise.resolve();
  expect(h.ports).toHaveLength(2);
  const reconnected = h.ports[1];
  expect(reconnected).toBeDefined();
  await reconnected?.inbound(helloAck({ daemon_version: "0.9.0", features: ["triage_snapshot_v1"] }));
  await Promise.resolve();
  const request = h.frames().find((frame) => frame.type === "triage_snapshot_request");
  const requestID = request?.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await reconnected?.inbound(snapshotResult(requestID as string, 1));
  await expect(pending).resolves.toMatchObject({ ok: true, snapshot: { counts: { pending_total: 1 } } });
});

test("heartbeat counts obey disconnected, permission, then pending badge precedence", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: "0.9.0",
      features: ["triage_snapshot_v1"],
      resolver_origins: ["https://resolver.example.edu"],
    }),
  );
  h.deps.permissions.contains = async () => true;

  const refresh = h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  const request = h.frames().find((frame) => frame.type === "triage_counts_request");
  const requestID = request?.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("triage_counts_response", { request_id: requestID as string, counts: triageCounts(4) }),
  );
  await refresh;
  expect(h.action.texts.at(-1)).toBe("4");
  expect(h.action.titles.at(-1)).toBe("Papio: 4 pending triage items");

  h.deps.permissions.contains = async () => false;
  await h.bridge.syncConnectionBadge();
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe("Papio: 1 provider permission need attention");

  await h.port.emitDisconnect();
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.titles.at(-1)).toBe("Papio: daemon disconnected");
});

test("inbound native handlers finish in receipt order across asynchronous awaits", async () => {
  const h = makeHarness();
  await h.bridge.start();

  const first = h.port.inbound(jobOffer("job_chain_first"));
  const second = h.port.inbound(jobOffer("job_chain_second"));
  await Promise.all([first, second]);

  expect(
    h
      .frames()
      .filter((frame) => frame.type === "job_accept")
      .map((frame) => frame.job_id),
  ).toEqual(["job_chain_first", "job_chain_second"]);
});
