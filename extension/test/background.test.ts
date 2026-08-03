// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Behavioural tests for the MV3 bridge against a minimal fake chrome surface and
// a fake native port. No real chrome, and no wall-clock timers: every fake
// emitter awaits the handler promises it triggers, so the flow is deterministic.

import { expect, test } from "bun:test";
import { Window } from "happy-dom";


import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape } from "../src/state";
import { capturePage, encodePageCapture, sanitizeFixture } from "../src/capture";
import { KeepaliveManager, type KeepaliveAPI } from "../src/keepalive";
import { interpret, type AdapterSpec } from "../src/adapters/types";
import {
  Bridge,
  findManagedTab,
  hasDaemonUpdateHint,
  handleInboxRuntimeMessage,
  needsVisibleWindow,
  normalizeManagedTabURL,
  isBotChallenge,
  isRedirectLoopPage,
  assessDrivenPage,
  registrableProviderHost,
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
test("managed tab dedupe ignores URL fragments and prioritizes a tracked tab", () => {
  const candidates: TabInfo[] = [
    { id: 100, url: "https://sage.example/article?download=1#section-a" },
    { id: 101, url: "https://sage.example/other" },
  ];
  expect(normalizeManagedTabURL("https://sage.example/article?download=1#section-a")).toBe(
    "https://sage.example/article?download=1",
  );
  expect(findManagedTab(candidates, "https://sage.example/article?download=1#section-b")?.id).toBe(100);
  expect(findManagedTab(candidates, "https://sage.example/new#fragment", 101)?.id).toBe(101);
});

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
  /** URL navigations are distinct from focus updates, so tests can prove that
   * a handoff refreshes only when its current document is an auth page. */
  readonly navigations: { tabID: number; url: string }[] = [];
  readonly live = new Map<number, TabInfo>();
  nextId = 100;
  failCreate = false;
  currentWindowID = 1;
  /** Records tabs.group calls; assigned by makeHarness only in tab-group mode. */
  readonly grouped: { tabIds: number[]; groupId?: number }[] = [];
  group?: (opts: { tabIds: number[]; groupId?: number }) => Promise<number>;
  async update(tabID: number, props: { active?: boolean; url?: string }): Promise<TabInfo> {
    if (props.active) this.activated.push(tabID);
    const tab = this.live.get(tabID);
    if (props.url !== undefined) {
      this.navigations.push({ tabID, url: props.url });
      if (tab) tab.url = props.url;
    }
    return tab ?? {};
  }
  async create(props: { url: string; active: boolean; windowId?: number }): Promise<TabInfo> {
    this.created.push(props);
    if (this.failCreate) throw new Error("tab creation blocked");
    const id = this.nextId++;
    const tab: TabInfo = { id, url: props.url, windowId: props.windowId ?? this.currentWindowID };
    this.live.set(id, tab);
    return tab;
  }
  async query(query: { url?: string; groupId?: number }): Promise<TabInfo[]> {
    return [...this.live.values()].filter(
      (tab) =>
        (query.url === undefined || tab.url === query.url) &&
        (query.groupId === undefined || tab.groupId === query.groupId),
    );
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
  readonly updated: {
    windowID: number;
    props: { focused?: boolean; state?: "normal" | "minimized"; drawAttention?: boolean };
  }[] = [];
  readonly removed: number[] = [];
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
  async get(windowID: number): Promise<{ id: number; state: string; tabs: TabInfo[] }> {
    const win = this.live.get(windowID);
    if (!win) throw new Error("no such window");
    return { ...win, tabs: [...this.tabs.live.values()].filter((tab) => tab.windowId === windowID) };
  }
  async update(
    windowID: number,
    props: { focused?: boolean; state?: "normal" | "minimized"; drawAttention?: boolean },
  ): Promise<unknown> {
    this.updated.push({ windowID, props });
    const win = this.live.get(windowID);
    if (win && props.state !== undefined) win.state = props.state;
    return win ?? {};
  }
  /** Programmatic close: drops the window and any tabs still parked in it. */
  async remove(windowID: number): Promise<void> {
    this.removed.push(windowID);
    for (const tab of [...this.tabs.live.values()].filter((tab) => tab.windowId === windowID)) {
      if (tab.id !== undefined) this.tabs.live.delete(tab.id);
    }
    this.live.delete(windowID);
  }
  /** Simulate the user closing the work window. */
  close(windowID: number): void {
    this.live.delete(windowID);
  }
}

class FakeTabGroups {
  readonly updated: { groupID: number; props: { collapsed?: boolean; title?: string; color?: string } }[] =
    [];
  readonly live = new Map<number, { id: number; collapsed: boolean; title?: string; windowId?: number }>();
  nextID = 700;
  async get(groupID: number): Promise<{ id: number; collapsed: boolean; title?: string; windowId?: number }> {
    const group = this.live.get(groupID);
    if (!group) throw new Error("no such group");
    return group;
  }
  async query(props: {
    title?: string;
  }): Promise<{ id: number; collapsed: boolean; title?: string; windowId?: number }[]> {
    return [...this.live.values()].filter((g) => props.title === undefined || g.title === props.title);
  }
  async update(
    groupID: number,
    props: { collapsed?: boolean; title?: string; color?: string },
  ): Promise<unknown> {
    this.updated.push({ groupID, props });
    const group = this.live.get(groupID);
    if (group) {
      if (props.collapsed !== undefined) group.collapsed = props.collapsed;
      if (props.title !== undefined) group.title = props.title;
    }
    return group ?? {};
  }
  /** Simulate Chrome removing an emptied group (or the user closing it). */
  close(groupID: number): void {
    this.live.delete(groupID);
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
  tabGroups?: FakeTabGroups;
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  frames(): BrowserMessage[];
  alarms: FakeAlarms;
  postedStrings(): string[];
}

function makeHarness(
  seed?: StoreShape,
  opts?: {
    windows?: boolean;
    workWindowEnabled?: boolean;
    firefox?: boolean;
    tabGroups?: boolean;
    handoffSurface?: "in-window" | "work-window" | "tab-group";
  },
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
  const tabGroups = opts?.tabGroups === true ? new FakeTabGroups() : undefined;
  if (tabGroups !== undefined) {
    tabs.group = async ({ tabIds, groupId }) => {
      const id = groupId ?? tabGroups.nextID++;
      const target = tabGroups.live.get(id);
      const windowID = target?.windowId ?? tabs.live.get(tabIds[0]!)?.windowId;
      if (
        windowID !== undefined &&
        tabIds.some((tabID) => tabs.live.get(tabID)?.windowId !== windowID)
      ) {
        throw new Error("tabs from different windows cannot share a group");
      }
      if (target === undefined) {
        tabGroups.live.set(id, {
          id,
          collapsed: false,
          ...(windowID === undefined ? {} : { windowId: windowID }),
        });
      }
      tabs.grouped.push(groupId === undefined ? { tabIds } : { tabIds, groupId });
      const emptied = new Set<number>();
      for (const tabID of tabIds) {
        const tab = tabs.live.get(tabID);
        if (tab === undefined) throw new Error("no such tab");
        if (tab.groupId !== undefined && tab.groupId >= 0 && tab.groupId !== id) emptied.add(tab.groupId);
        tab.groupId = id;
      }
      for (const formerGroupID of emptied) {
        if (![...tabs.live.values()].some((tab) => tab.groupId === formerGroupID)) {
          tabGroups.close(formerGroupID);
        }
      }
      return id;
    };
  }
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
      getHandoffSurface: async () =>
        opts?.handoffSurface ?? (opts?.workWindowEnabled === false ? "in-window" : "work-window"),
    },
    ...(windows !== undefined ? { windows } : {}),
    ...(tabGroups !== undefined ? { tabGroups } : {}),
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
    ...(tabGroups !== undefined ? { tabGroups } : {}),
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
function jobOfferForHosts(jobID: string, providerHosts: string[], openurl = OPENURL): unknown {
  const offer = jobOffer(jobID, openurl) as { payload: Record<string, unknown> };
  offer.payload["provider_hosts"] = providerHosts;
  return offer;
}


const PROVIDER_ADAPTER: AdapterSpec = {
  id: "provider",
  version: "1.0.0",
  hosts: [PROVIDER_HOST],
  classify: [],
};

function sanitizedObservedChallenge(html: string): Document {
  const window = new Window({ url: "https://fixture.local/" });
  window.document.write(
    sanitizeFixture(html, {
      provider: "observed",
      scenario: "observed",
      originNoQuery: "https://fixture.local/challenge",
      capturedISO: "2026-07-22T07:18:34.092Z",
    }),
  );
  return window.document as unknown as Document;
}

function useUnknownProviderClassifier(h: Harness, challenge: () => boolean): void {
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === interpret) return [{ result: { kind: "unknown" } }];
    if (injection.func === assessDrivenPage) return [{ result: { kind: challenge() ? "challenge" : "normal" } }];
    if (injection.func === isBotChallenge) return [{ result: challenge() }];
    return [];
  };
}

async function classifyProviderUnknown(h: Harness, jobID: string): Promise<number> {
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.live.set(tabID, { id: tabID, url });
  await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  return tabID;
}

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

  expect(h.tabs.created).toHaveLength(2);
  expect(h.tabs.created.filter((tab) => !tab.active)).toHaveLength(1);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(3);
  expect(h.tabs.reloaded).toEqual([stuckTabID]);
});

test("a cold requires-auth handoff is signalled while queued and opens after its bounded fallback", async () => {
  // No KeepaliveManager reports an authenticated session in this harness: this
  // is the disabled-keepalive, no-evidence path that must still reach the user.
  const h = makeHarness();
  const offer = jobOffer("job_0001a_requires_auth") as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.bridge.setKeepaliveAuthenticated(false);
  await h.port.inbound(helloAck());
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.bridge.trackedJobCount()).toBe(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: "job_0001a_requires_auth",
    tab_id: -1,
    status: "queued",
    requires_auth: true,
  });
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");

  // Treat the worker-local timer as lost with MV3 worker suspension. The
  // periodic wake must use the durable offer time to release the cold queue.
  expect(h.timers.some((timer) => timer.ms === 45_000)).toBe(true);
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });

  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.backend.store.activeJobs[0]).toMatchObject({ tab_id: tabID, status: "accepted" });

  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(true);
});

test("a re-offer records its auth requirement on a restored queued handoff", async () => {
  const jobID = "job_0001a_restored_requires_auth";
  const h = makeHarness({
    ...emptyStore(),
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
    offerURLs: { [jobID]: OPENURL },
  });
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]?.requires_auth).toBe(true);
});

test("open-access offers open immediately without institutional session evidence", async () => {
  for (const requiresAuth of [undefined, false] as const) {
    const h = makeHarness();
    const offer = jobOffer(`job_0001a_open_access_${String(requiresAuth)}`) as {
      payload: Record<string, unknown>;
    };
    if (requiresAuth !== undefined) offer.payload["requires_auth"] = requiresAuth;

    await h.bridge.start();
    await h.port.inbound(offer);

    expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
    expect(h.backend.store.activeJobs[0]).toMatchObject({ tab_id: 100, status: "accepted" });
  }
});

test("an OA completion cannot release a queued institutional handoff", async () => {
  const h = makeHarness();
  const institutionalURL = "https://resolver.example.edu/openurl?ctx=institutional";
  const openAccessURL = "https://oa.example.edu/article/123";
  const institutional = jobOffer("job_0001a_institutional", institutionalURL) as {
    payload: Record<string, unknown>;
  };
  institutional.payload["requires_auth"] = true;
  const openAccess = jobOffer("job_0001a_open_access", openAccessURL) as {
    payload: Record<string, unknown>;
  };
  openAccess.payload["requires_auth"] = false;

  await h.bridge.start();
  await h.port.inbound(institutional);
  await h.port.inbound(openAccess);
  const openAccessTabID = h.backend.store.activeJobs.find((job) => job.job_id === "job_0001a_open_access")?.tab_id ?? -1;
  const providerURL = `https://${PROVIDER_HOST}/stable/open-access`;
  await h.tabs.onUpdated.emit(
    openAccessTabID,
    { url: providerURL, status: "complete" },
    { id: openAccessTabID, url: providerURL },
  );

  expect(h.tabs.created).toEqual([{ url: openAccessURL, active: true }]);
  expect(h.bridge.latestOpenURL()).toBe(institutionalURL);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_0001a_institutional")).toMatchObject({
    tab_id: -1,
    status: "queued",
  });
  expect(h.backend.store.lastAuthReturnedAt).toBeUndefined();
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
  ]);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(1);
  expect(h.backend.store.lastAuthReturnedAt).toBeUndefined();
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
  expect(injections).toHaveLength(1);
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

test("a registry-only adapter host classifies and emits an observed capture", async () => {
  // The offer list is capped while the source-controlled adapter registry is not;
  // capture must use the same verified-host decision as classification.
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  const stored: Record<string, unknown> = {};
  h.deps.captureStorage = {
    local: {
      get: async (key) => ({ [key]: stored[key] }),
      set: async (items) => {
        Object.assign(stored, items);
      },
    },
  };
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    if (injection.func === interpret) return [{ result: { kind: "unknown" } }];
    if (injection.func === capturePage) {
      return [{ result: { html: `<main class="article">new provider shape</main>`, origin: `https://${PROVIDER_HOST}`, path: "/stable/article" } }];
    }
    return [{ result: false }];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
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
  const captures = h.frames().filter((frame) => frame.type === "page_capture");
  expect(captures).toHaveLength(1);
  expect(captures[0]?.job_id).toBe("job_0001a_registry_host");
  expect(captures[0]?.payload).toMatchObject({
    host: PROVIDER_HOST,
    scenario: "observed",
    adapter_id: PROVIDER_ADAPTER.id,
  });
  expect(h.downloads.started).toHaveLength(0);
});

test("all-sites browser access counts as effective provider access", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  const permissionQueries: string[][] = [];
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.permissions.contains = async ({ origins }) => {
    permissionQueries.push(origins);
    // Chrome answers true for this exact-origin query when the user granted
    // optional https://*/* access; no host-specific grant is required to read it.
    return true;
  };
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    if (injection.func === interpret) return [{ result: { kind: "unknown" } }];
    if (injection.func === isBotChallenge) return [{ result: false }];
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_all_sites_provider_access"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );

  expect(permissionQueries).toEqual([[`https://${PROVIDER_HOST}/*`]]);
  expect(injections.some((injection) => injection.func === interpret)).toBe(true);
  expect(h.backend.store.blockedProviderHosts).toBeUndefined();
});

test("missing provider access reports the exact host instead of a bare adapter failure", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  h.deps.permissions.contains = async () => false;
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_missing_provider_access"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );

  const outcomes = h.frames().filter((frame) => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload["outcome"]).toBe("ui_changed");
  expect(outcomes[0]?.payload["detail"]).toContain(PROVIDER_HOST);
  expect(outcomes[0]?.payload["detail"]).toContain("Open Papio Options");
  expect(outcomes[0]?.payload["adapter_version"]).toBeUndefined();
  expect(injections).toEqual([]);
  expect(h.backend.store.blockedProviderHosts).toEqual([PROVIDER_HOST]);
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toContain(PROVIDER_HOST);
});

test("one blocked provider host stays a single indication across repeated updates", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  let permissionChecks = 0;
  h.deps.permissions.contains = async () => {
    permissionChecks += 1;
    return false;
  };

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_deduplicated_provider_access"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  for (let update = 0; update < 3; update += 1) {
    await h.tabs.onUpdated.emit(
      tabID,
      { url: articleURL, status: "complete" },
      { id: tabID, url: articleURL },
    );
  }

  expect(permissionChecks).toBe(1);
  expect(h.frames().filter((frame) => frame.type === "provider_outcome")).toHaveLength(1);
  expect(h.backend.store.blockedProviderHosts).toEqual([PROVIDER_HOST]);
  expect(h.action.titles.filter((title) => title.includes(PROVIDER_HOST))).toHaveLength(1);
});

test("a stored provider blocker remains visible until effective access changes", async () => {
  const h = makeHarness({
    ...emptyStore(),
    connectionStatus: "connected",
    blockedProviderHosts: [PROVIDER_HOST],
  });
  let granted = false;
  h.deps.permissions.contains = async () => granted;

  await h.bridge.start();
  expect(h.action.titles.at(-1)).toContain(PROVIDER_HOST);

  granted = true;
  await h.bridge.onPermissionsChanged();
  expect(h.backend.store.blockedProviderHosts).toEqual([]);
  expect(h.action.titles.at(-1)).toBe("Papio: connected");
});

test("Cloudflare challenge detection survives the observed marker sanitization", () => {
  // SAGE, ACM, and the newer ScienceDirect captures share these widget markers;
  // their script bodies are intentionally absent from committed fixtures.
  const widgetChallenge = sanitizedObservedChallenge(`
    <html><head>
      <script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1">discarded body</script>
      <script src="https://challenges.cloudflare.com/turnstile/v0/b/3104729c556c/api.js">discarded body</script>
    </head><body>
      <input type="hidden" name="cf-turnstile-response" id="cf-chl-widget-TOKEN_response">
    </body></html>
  `);
  // The older 21 KiB ScienceDirect capture has no surviving cf-chl/script
  // marker, but its non-script captcha stage remains after sanitization.
  const legacyScienceDirectChallenge = sanitizedObservedChallenge(`
    <html><head><title>Verificación en curso</title></head><body>
      <div id="captcha-box"><div class="main-wrapper" role="main"></div></div>
    </body></html>
  `);
  const translatedTitleOnly = sanitizedObservedChallenge(`
    <html><head><title>Un momento...</title></head><body><main></main></body></html>
  `);

  expect(isBotChallenge(widgetChallenge)).toBe(true);
  expect(isBotChallenge(legacyScienceDirectChallenge)).toBe(true);
  expect(isBotChallenge(translatedTitleOnly)).toBe(false);
});

test("a Cloudflare challenge clears an earlier unknown streak and parks its provider", async () => {
  let challenge = false;
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => challenge);
  const tabID = await classifyProviderUnknown(h, "job_challenge_clears_unknown");

  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);
  await h.port.inbound(helloAck());
  challenge = true;
  h.clock.now += 5_000;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });

  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "jstor.org",
    challenge_kind: "cloudflare",
  });
  expect(h.backend.store.challengeCooldowns).toEqual({
    "jstor.org": h.clock.now + 600_000,
  });
  expect(h.tabs.live.has(tabID)).toBe(true);
  expect(
    h.frames().find((frame) => frame.type === "error" && frame.payload["code"] === "challenge_blocked"),
  ).toBeDefined();
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.backend.store.activeJobs[0]?.unknown_count ?? 0).toBe(0);
  expect(h.backend.store.providerDrainLeases).toEqual({
    [PROVIDER_HOST]: {
      providerKey: PROVIDER_HOST,
      expiresAt: h.clock.now + 60_000,
      parkedReason: "challenge",
    },
  });
  expect(h.timers.at(-1)?.ms).toBe(60_000);
});

test("a cleared Cloudflare challenge returns to the ordinary stale-adapter path", async () => {
  let challenge = true;
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => challenge);
  const tabID = await classifyProviderUnknown(h, "job_challenge_clears");

  challenge = false;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  for (let step = 0; step < 3; step += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }

  const outcomes = h.frames().filter((frame) => frame.type === "provider_outcome");

  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toMatchObject({
    outcome: "ui_changed",
    adapter_version: PROVIDER_ADAPTER.version,
  });
  expect(h.backend.store.challengeCooldowns).toEqual({});
  expect(h.backend.store.activeJobs[0]?.challenge_blocked).toBeUndefined();
});
test("driven-page assessment classifies challenge, redirect-loop, and normal fixtures", () => {
  const fixture = (html: string): Document => {
    const window = new Window({ url: "https://www.jstor.org/stable/paper" });
    window.document.write(html);
    return window.document as unknown as Document;
  };
  const challengeFixtures = [
    "<html><head><title>Are you a robot?</title></head><body></body></html>",
    "<form id=\"challenge-form\"></form>",
    "<div class=\"cf-turnstile\"></div>",
    "<div id=\"data-cf-chl-token\"></div>",
    "<main>Verify you are human to continue</main>",
  ];
  for (const html of challengeFixtures) {
    const doc = fixture(html);
    expect(assessDrivenPage(doc).kind).toBe("challenge");
    expect(isBotChallenge(doc)).toBe(true);
  }
  const loop = fixture("<html><head><title>OpenAthens</title></head><body>Too many redirects</body></html>");
  expect(assessDrivenPage(loop).kind).toBe("redirect_loop");
  expect(isRedirectLoopPage(loop)).toBe(true);
  const normal = fixture("<html><head><title>Article</title></head><body><main>Abstract</main></body></html>");
  expect(assessDrivenPage(normal)).toEqual({ kind: "normal" });
  expect(isBotChallenge(normal)).toBe(false);
  expect(isRedirectLoopPage(normal)).toBe(false);
  expect(registrableProviderHost("www.sciencedirect.com")).toBe("sciencedirect.com");
  expect(registrableProviderHost("journals.example.co.uk")).toBe("example.co.uk");
});

test("unknown retries report ui_changed once per drive and again for a re-offered tab", async () => {
  const jobID = "job_unknown_outcome_drive";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => false);
  const firstTabID = await classifyProviderUnknown(h, jobID);

  // The bounded retries revisit one document while it renders. Before the
  // latch, every second retry pair emitted another identical terminal outcome.
  for (let retry = 0; retry < 8; retry += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }
  expect(
    h.frames().filter((frame) => frame.type === "provider_outcome" && frame.payload["outcome"] === "ui_changed"),
  ).toHaveLength(1);

  // A re-offer whose previous tab is gone creates a genuinely new provider
  // drive, which may legitimately produce its own terminal observation.
  h.tabs.live.delete(firstTabID);
  await h.port.inbound(jobOffer(jobID));
  const secondTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(secondTabID).not.toBe(firstTabID);
  const articleURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.live.set(secondTabID, { id: secondTabID, url: articleURL });
  await h.tabs.onUpdated.emit(
    secondTabID,
    { url: articleURL, status: "complete" },
    { id: secondTabID, url: articleURL },
  );
  for (let retry = 0; retry < 2; retry += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }

  expect(
    h.frames().filter((frame) => frame.type === "provider_outcome" && frame.payload["outcome"] === "ui_changed"),
  ).toHaveLength(2);
});

test("a challenge parks only its provider and leaves another provider draining", async () => {
  const otherProvider = "link.springer.com";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => true);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_challenge_source"));
  const sourceTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const challengeURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.live.set(sourceTabID, { id: sourceTabID, url: challengeURL });
  await h.tabs.onUpdated.emit(
    sourceTabID,
    { url: challengeURL, status: "complete" },
    { id: sourceTabID, url: challengeURL },
  );

  const parked = jobOffer("job_challenge_parked") as { payload: Record<string, unknown> };
  parked.payload["requires_auth"] = true;
  const other = jobOfferForHosts("job_challenge_other", [otherProvider]) as { payload: Record<string, unknown> };
  other.payload["requires_auth"] = true;
  await h.port.inbound(parked);
  await h.port.inbound(other);
  await h.bridge.setKeepaliveAuthenticated(true);

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_challenge_parked")).toMatchObject({
    tab_id: -1,
    status: "queued",
    handoffAckPending: true,
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_challenge_other")).toMatchObject({
    status: "accepted",
  });
  expect(
    h.frames().filter((frame) => frame.type === "job_accept" && frame.job_id === "job_challenge_parked"),
  ).toHaveLength(0);
});

test("an expired provider lease reclaims its queued handoff without a new offer", async () => {
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => true);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_lease_source"));
  const sourceTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const challengeURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.live.set(sourceTabID, { id: sourceTabID, url: challengeURL });
  await h.tabs.onUpdated.emit(
    sourceTabID,
    { url: challengeURL, status: "complete" },
    { id: sourceTabID, url: challengeURL },
  );

  const queued = jobOffer("job_lease_reclaim") as { payload: Record<string, unknown> };
  queued.payload["requires_auth"] = true;
  await h.port.inbound(queued);
  await h.bridge.setKeepaliveAuthenticated(true);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_lease_reclaim")).toMatchObject({
    tab_id: -1,
    status: "queued",
    handoffAckPending: true,
  });

  const leaseExpiry = h.timers.find((timer) => timer.ms === 60_000);
  expect(leaseExpiry).toBeDefined();
  h.clock.now += 60_000;
  await leaseExpiry!.fn();

  // A provider challenge now has a durable cooldown in addition to its short
  // drain lease; expiry of the lease alone must not immediately re-drive it.
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_lease_reclaim")).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(
    h.frames().filter((frame) => frame.type === "job_accept" && frame.job_id === "job_lease_reclaim"),
  ).toHaveLength(1);
  expect(h.backend.store.providerDrainLeases).toEqual({});

  const cooldownExpiry = h.timers.find((timer) => timer.ms === 600_000);
  expect(cooldownExpiry).toBeDefined();
  h.clock.now += 540_000;
  await cooldownExpiry!.fn();

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_lease_reclaim")).toMatchObject({
    status: "accepted",
  });
  expect(
    h.frames().filter((frame) => frame.type === "job_accept" && frame.job_id === "job_lease_reclaim"),
  ).toHaveLength(1);
  expect(h.backend.store.providerDrainLeases).toEqual({
    [PROVIDER_HOST]: {
      providerKey: PROVIDER_HOST,
      expiresAt: h.clock.now + 60_000,
    },
  });
});

test("a unique manual Chrome download from a registry-only host is correlated", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
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
  expect(h.backend.store.lastAuthReturnedAt).toBeUndefined();
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

test("a changed re-offer reuses the live job tab for the institutional fallback", async () => {
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

  expect(h.tabs.created).toEqual([{ url: oaURL, active: true }]);
  expect(h.tabs.navigations).toEqual([{ tabID: 100, url: institutionalURL }]);
  expect(h.tabs.removed).toEqual([]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
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
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    return [{ result: { kind: "article" } }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_click"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(injections).toHaveLength(2);
  expect(injections.filter((injection) => injection.func === interpret)).toHaveLength(1);
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
  h.deps.permissions.contains = async () => true;
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

test("a cross-origin api download with a content-disposition rename steers into papio/<job>/ by ID", async () => {
  // Regression for the EBSCO shape (research.ebsco.com -> content.ebscohost.com):
  // the provider redirect changes origin before onDeterminingFilename, the item
  // carries no tabId, and the server renames the file (retrieve.pdf). Steering
  // must come from the creation-bound download ID, never tab/URL heuristics.
  const h = makeHarness();
  const crossOriginPDF = "https://content.aggregator.example/cds/retrieve?db=a9h";
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
    injection.func === interpret ? [{ result: { kind: "article" } }] : [{ result: crossOriginPDF }];
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004b_xorigin_api"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url: articleURL, status: "complete" }, { id: tabID, url: articleURL });

  expect(h.downloads.started).toEqual([
    {
      url: crossOriginPDF,
      filename: "papio/job_0004b_xorigin_api/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);

  // Chrome fires onCreated (still the requested URL), then onDeterminingFilename
  // with the server's content-disposition name and no tab correlation.
  await h.downloads.onCreated.emit({ id: 901, url: crossOriginPDF, state: "in_progress" });
  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    { id: 901, url: crossOriginPDF, filename: "retrieve.pdf", state: "in_progress" },
    (s) => suggestions.push(s),
  );
  expect(suggestions).toEqual([
    { filename: "papio/job_0004b_xorigin_api/retrieve.pdf", conflictAction: "uniquify" },
  ]);

  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/papio/job_0004b_xorigin_api/retrieve.pdf",
    fileSize: 2_100_000,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });

  const complete = h.frames().find(
    (frame) => frame.type === "download_complete" && frame.job_id === "job_0004b_xorigin_api",
  );
  expect(complete?.payload["filename"]).toBe("retrieve.pdf");
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
  expect(h.timers.filter((timer) => timer.ms === 1_000)).toHaveLength(0);
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

test("concurrent handoff triggers cannot double-drain one provider", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_provider_active"));
  await h.port.inbound(jobOffer("job_provider_first"));
  await h.port.inbound(jobOffer("job_provider_second"));

  const [first, second] = await Promise.all([
    h.bridge.openHandoff("job_provider_first"),
    h.bridge.openHandoff("job_provider_second"),
  ]);

  expect(first).toEqual({ ok: true, opened: true });
  expect(second).toMatchObject({ ok: false });
  expect(h.tabs.created).toHaveLength(2);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_provider_second")).toMatchObject({
    tab_id: -1,
    status: "queued",
  });
});

test("concurrent fallback timers retain same-provider handoffs behind one lease", async () => {
  const h = makeHarness();
  // First offer opens the one visible tab; the rest queue behind it, each with
  // its own worker-local fallback timer.
  const jobIDs = ["job_conc_active", "job_conc_1", "job_conc_2", "job_conc_3"];

  await h.bridge.start();
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(3);
  const fallbacks = h.timers.filter((timer) => timer.ms === 45_000);
  expect(fallbacks).toHaveLength(3);

  // Racing fallback callbacks claim one provider lease. The remaining jobs
  // stay queued instead of opening concurrent handoff tabs for the same host.
  await Promise.all(fallbacks.map((timer) => timer.fn()));

  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(2);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id >= 0)).toHaveLength(2);
  expect(h.tabs.created.filter((tab) => !tab.active)).toHaveLength(1);
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
    settings: {
      getTermsConsent: async () => undefined,
      setTermsConsent: async () => {},
      getHandoffSurface: async () => "work-window",
    },
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
    expectedUpdates: {
      windowID: number;
      props: { focused?: boolean; state?: "normal" | "minimized"; drawAttention?: boolean };
    }[];
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
    h.deps.permissions.contains = async () => c.adapterSpecs.length > 0;
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

  // The governor queues a third drive while both warm slots are occupied.
  h.windows?.close(500);
  await h.port.inbound(jobOffer("job_ww_c"));
  expect(h.windows?.created.length).toBe(1);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_ww_c")).toMatchObject({
    status: "accepted",
    tab_id: -1,
  });

  // Releasing one live drive lets the queued offer recreate the user-closed
  // work window instead of trying to reuse its stale id.
  await h.bridge.requestCancel("job_ww_a");
  expect(h.windows?.created.length).toBe(2);
  expect(h.backend.store.workWindowID).toBe(501);
});

test("the work window closes once the last handoff releases its tab", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_idle"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.backend.store.workWindowID).toBe(500);
  // The user closes the handoff tab before download: the job cancels and, with
  // no papio tab left in the work window, the window is reaped rather than left
  // to accumulate across handoffs.
  h.tabs.live.delete(tabID);
  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.windows?.removed).toEqual([500]);
  expect(h.backend.store.workWindowID).toBeUndefined();
});

test("a keepalive-pinned tab keeps the work window alive when handoffs drain", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_keepalive"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  // Keepalive pins its resolver session tab in the shared work window; draining
  // the last handoff must not evict it.
  h.tabs.live.set(9001, {
    id: 9001,
    url: "https://resolver.example.edu/keepalive",
    windowId: 500,
    pinned: true,
  });
  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.windows?.removed).toEqual([]);
  expect(h.backend.store.workWindowID).toBe(500);
});

test("tab-group mode opens the handoff in a collapsed papio group in the current window", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_first"));

  // The tab opens in the user's current window (no windowId), not a new window.
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }]);
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(true);
  expect(h.tabGroups?.updated).toEqual([
    { groupID: groupID!, props: { title: "papio", collapsed: true, color: "orange" } },
  ]);
});

test("tab-group handoff works on Firefox 139+ (no onDeterminingFilename)", async () => {
  // Firefox has no downloads.onDeterminingFilename, but Firefox 139+ ships the
  // tabGroups API. Tab-group handoff is dep-driven, never gated on Chrome.
  const h = makeHarness(undefined, { firefox: true, tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_firefox"));

  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }]);
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(true);
});

test("tab-group handoffs reuse one papio group", async () => {
  const h = makeHarness(
    { ...emptyStore(), lastAuthReturnedAt: 1_700_000_000_000 },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_a"));
  const groupID = h.backend.store.handoffGroupID!;
  await h.port.inbound(jobOffer("job_tg_b"));

  // Second handoff joins the same group rather than creating another.
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }, { tabIds: [101], groupId: groupID }]);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabGroups?.live.size).toBe(1);
});

test("concurrent tab-group folds create exactly one papio group", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  const first = await h.tabs.create({ url: `${OPENURL}&keepalive=first`, active: false });
  const second = await h.tabs.create({ url: `${OPENURL}&keepalive=second`, active: false });

  await Promise.all([h.bridge.foldKeepaliveTab(first.id!), h.bridge.foldKeepaliveTab(second.id!)]);

  expect(h.tabGroups?.live.size).toBe(1);
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }, { tabIds: [101], groupId: 700 }]);
});

test("tab-group folds do not reuse a stored group from another window", async () => {
  const h = makeHarness(
    { ...emptyStore(), handoffGroupID: 700 },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  const existing = await h.tabs.create({ url: `${OPENURL}&window=one`, active: false, windowId: 1 });
  h.tabs.live.get(existing.id!)!.groupId = 700;
  h.tabGroups?.live.set(700, { id: 700, collapsed: true, title: "papio", windowId: 1 });
  h.tabGroups!.nextID = 701;
  h.tabs.currentWindowID = 2;

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_second_window"));

  expect(h.tabGroups?.live.size).toBe(2);
  expect(h.tabGroups?.live.get(700)?.windowId).toBe(1);
  expect(h.tabGroups?.live.get(701)?.windowId).toBe(2);
  expect(h.tabs.grouped).toEqual([{ tabIds: [101] }]);
});

test("tab-group mode rediscovers a renamed papio group after session storage clears", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_reload_a"));
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  expect(h.tabGroups?.live.size).toBe(1);
  await h.tabGroups!.update(groupID!, {
    title: "papio — A paper still awaiting institutional access",
    collapsed: false,
  });

  // Simulate an extension reload: chrome.storage.session is wiped while the
  // physical group remains labeled with the paper that needs attention.
  h.backend.store = emptyStore();
  const reloaded = new Bridge(h.deps);
  await reloaded.start();
  await h.ports[h.ports.length - 1]!.inbound(jobOffer("job_tg_reload_b"));

  expect(h.tabGroups?.live.size).toBe(1);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }, { tabIds: [101], groupId: groupID! }]);
  expect(h.tabGroups?.live.get(groupID!)?.title).toBe("papio — A paper still awaiting institutional access");
});

test("startup consolidates three papio groups in one window", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  for (const groupID of [700, 701, 702]) {
    const tab = await h.tabs.create({ url: `${OPENURL}&group=${groupID}`, active: false });
    h.tabs.live.get(tab.id!)!.groupId = groupID;
    h.tabGroups!.live.set(groupID, {
      id: groupID,
      collapsed: groupID !== 700,
      title: "papio",
      windowId: 1,
    });
  }
  h.tabGroups!.nextID = 703;

  await h.bridge.start();

  expect(h.tabs.grouped).toEqual([{ tabIds: [101], groupId: 700 }, { tabIds: [102], groupId: 700 }]);
  expect([...h.tabs.live.values()].map((tab) => tab.groupId)).toEqual([700, 700, 700]);
  expect(h.tabGroups?.live.size).toBe(1);
});

test("IdP auth expands the papio group and re-collapses when auth returns", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  const offer = jobOffer("job_tg_auth") as { payload: Record<string, unknown> };
  offer.payload["expected"] = { title: "A paper awaiting institutional access" };
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(true);
  expect(h.tabs.activated).toEqual([tabID]);
  expect(h.tabGroups?.live.get(groupID)?.collapsed).toBe(false);
  expect(h.tabGroups?.live.get(groupID)?.title).toBe("papio — A paper awaiting institutional access");

  // Auth returns to a provider host: the job advances and the group folds away.
  const providerURL = `https://${PROVIDER_HOST}/stable/123`;
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });
  const recollapse = h.timers.find((timer) => timer.ms === 5_000);
  expect(recollapse).toBeDefined();
  h.clock.now += 5_000;
  await recollapse!.fn();
  expect(h.frames().some((f) => f.type === "auth_returned")).toBe(true);
  expect(h.tabGroups?.live.get(groupID)?.collapsed).toBe(true);
  expect(h.tabGroups?.live.get(groupID)?.title).toBe("papio");
});

test("tab-group surfaces use the generic title when multiple jobs share the group", async () => {
  const h = makeHarness(
    { ...emptyStore(), lastAuthReturnedAt: 1_700_000_000_000 },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  const first = jobOffer("job_tg_multiple_first") as { payload: Record<string, unknown> };
  first.payload["expected"] = { title: "A paper that should not claim a shared group" };

  await h.bridge.start();
  await h.port.inbound(first);
  await h.port.inbound(jobOffer("job_tg_multiple_second"));
  const firstTabID = h.backend.store.activeJobs.find((job) => job.job_id === "job_tg_multiple_first")?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;
  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";

  await h.tabs.onUpdated.emit(firstTabID, { url: idpURL }, { id: firstTabID, url: idpURL });

  expect(h.tabGroups?.live.get(groupID)?.title).toBe("papio");
});

test("an emptied papio group's id is dropped so the next handoff regroups", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_idle"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;
  // Chrome removes the group once its last tab is gone.
  h.tabGroups?.close(groupID);
  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.backend.store.handoffGroupID).toBeUndefined();
});

test("a live papio group keeps its generic collapsed title when the last handoff closes", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  const offer = jobOffer("job_tg_keepalive") as { payload: Record<string, unknown> };
  offer.payload["expected"] = { title: "Paper metadata must disappear on cancellation" };
  await h.bridge.start();
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.tabGroups?.live.get(groupID)).toMatchObject({
    collapsed: false,
    title: "papio — Paper metadata must disappear on cancellation",
  });

  // The group still exists because a keepalive tab remains folded into it.
  h.tabs.live.delete(tabID);
  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });
  const collapse = h.timers.find((timer) => timer.ms === 5_000);
  expect(collapse).toBeDefined();
  h.clock.now += 5_000;
  await collapse!.fn();
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabGroups?.live.get(groupID)).toMatchObject({ collapsed: true, title: "papio" });
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
    { windowID: 500, props: { focused: true, drawAttention: true, state: "normal" } },
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

test("popup capture relay emits page_capture only after the daemon advertises it", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  const sanitized = sanitizeFixture(`<main class="article">Known structure</main>`, {
    provider: "jstor",
    scenario: "success",
    originNoQuery: "https://www.jstor.org/stable/123",
    capturedISO: "2026-07-27T10:11:12.000Z",
  });
  const encoded = await encodePageCapture(sanitized, {
    host: "www.jstor.org",
    scenario: "success",
    adapterID: "jstor",
  });
  if (!encoded.ok) throw new Error(encoded.error);

  await h.bridge.start();
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.page_capture", payload: encoded.payload },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "unauthorized", message: "This sender cannot send page captures" },
  });
  expect(h.frames().some((frame) => frame.type === "page_capture")).toBe(false);
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.page_capture", payload: encoded.payload },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ captured: true });
  expect(h.frames().some((frame) => frame.type === "page_capture")).toBe(false);
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.page_capture", payload: encoded.payload },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ captured: true });

  const captures = h.frames().filter((frame) => frame.type === "page_capture");
  expect(captures).toHaveLength(1);
  expect(captures[0]?.payload).toEqual({ ...encoded.payload });
  expect(captures[0]?.job_id).toBeUndefined();
  expect(h.downloads.started).toHaveLength(0);
});

test("inbox runtime messages validate the exact extension sender", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
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

test("papio.stats from any papio page routes to the bridge stats request", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  const statsReply: Awaited<ReturnType<Bridge["requestStats"]>> = {
    ok: true,
    stats: {
      generated_at: "2026-07-25T08:00:00Z",
      acquired_total: 3,
      failed_total: 1,
      handoffs_required: 2,
      access: { open_access: 1, institutional: 2, licensed_api: 0, other: 0 },
      series: [{ period_start: "2026-07-20T00:00:00Z", acquired: 3 }],
    },
  };
  let statsCalls = 0;
  h.bridge.requestStats = async () => {
    statsCalls += 1;
    return statsReply;
  };

  // The popup summary, the history page, and the inbox all read stats.
  for (const url of [urls.popupURL, urls.historyURL, urls.inboxURL]) {
    await expect(
      handleInboxRuntimeMessage(h.bridge, { type: "papio.stats", request: {} }, { id: urls.runtimeID, url }, urls),
    ).resolves.toBe(statsReply);
  }
  expect(statsCalls).toBe(3);
});

test("papio.stats rejects foreign senders and malformed requests without touching the bridge", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  let statsCalls = 0;
  h.bridge.requestStats = async (): ReturnType<Bridge["requestStats"]> => {
    statsCalls += 1;
    return { ok: false, error: { code: "unexpected", message: "the bridge must not be reached" } };
  };

  for (const sender of [
    { id: urls.runtimeID, url: "chrome-extension://papio-test-id/options.html" },
    { id: urls.runtimeID, url: "https://provider.example/article" },
    { id: "other-extension", url: urls.historyURL },
  ]) {
    await expect(
      handleInboxRuntimeMessage(h.bridge, { type: "papio.stats", request: {} }, sender, urls),
    ).resolves.toEqual({
      ok: false,
      error: { code: "unauthorized", message: "This sender cannot access papio stats" },
    });
  }

  const sender = { id: urls.runtimeID, url: urls.popupURL };
  for (const message of [
    { type: "papio.stats", request: { unexpected: true } },
    { type: "papio.stats" },
    { type: "papio.stats", request: {}, extra: 1 },
  ]) {
    await expect(handleInboxRuntimeMessage(h.bridge, message, sender, urls)).resolves.toEqual({
      ok: false,
      error: { code: "invalid_request", message: "Invalid stats request" },
    });
  }
  expect(statsCalls).toBe(0);
});

test("open inbox runtime request focuses the singleton or creates it from the popup", async () => {
  const h = makeHarness(undefined, { windows: true });
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
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
    historyURL: "chrome-extension://papio-test-id/history.html",
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
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.activated).toEqual([tabID, tabID]);

  for (const message of [
    { type: "papio.triage.decide", request: { item_id: "item_001", op: "acquire" } },
    { type: "papio.action.resolve", request: { action_id: 1, verdict: "reject", expected_revision: 1 } },
    { type: "papio.preview", request: { action_id: 1 } },
  ]) {
    await expect(
      handleInboxRuntimeMessage(h.bridge, message, { id: urls.runtimeID, url: urls.popupURL }, urls),
    ).resolves.toEqual({
      ok: false,
      error: { code: "unauthorized", message: "This sender cannot access the inbox broker" },
    });
  }


  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.handoff.open", request: { job_id: "job_0001a_inbox_open" } },
      { id: urls.runtimeID, url: "chrome-extension://papio-test-id/options.html" },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "unauthorized", message: "This sender cannot access the inbox broker" },
  });
});
test("concurrent inbox opens reuse one managed tab and focus it once", async () => {
  const jobID = "job_open_dedupe";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: { [jobID]: OPENURL },
  });
  await h.bridge.start();
  const [first, second] = await Promise.all([h.bridge.openHandoff(jobID), h.bridge.openHandoff(jobID)]);

  expect(first).toEqual({ ok: true, opened: true });
  expect(second).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  expect(h.tabs.activated).toEqual([100]);
});
test("session state probes a live resolver tab before claiming signed out", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  await h.bridge.start();
  const keepaliveAPI: KeepaliveAPI = {
    tabs: {
      create: async (properties) =>
        h.tabs.create({
          url: properties.url,
          active: properties.active,
          ...(properties.windowId === undefined ? {} : { windowId: properties.windowId }),
        }),
      reload: (tabID) => h.tabs.reload(tabID),
      get: (tabID) => h.tabs.get(tabID),
      query: async (query) => {
        const pattern = query.url?.[0];
        if (pattern === undefined) return [];
        const origin = pattern.endsWith("/*") ? pattern.slice(0, -2) : pattern;
        return [...h.tabs.live.values()].filter((tab) => {
          if (typeof tab.url !== "string") return false;
          try {
            return new URL(tab.url).origin === origin;
          } catch {
            return false;
          }
        });
      },
      remove: (tabID) => h.tabs.remove(tabID),
      update: async (tabID) => h.tabs.get(tabID),
    },
    storage: {
      get: async () => ({
        "keepalive.interval": 4,
        "keepalive.enabled": true,
        "keepalive.resolverOrigin": "https://resolver.example.edu",
      }),
    },
    permissions: { getAll: async () => ({ origins: [] }) },
    scripting: {
      executeScript: async () => [{ result: [{ text: "Sign out", label: "" }] }],
    },
    timers: { setTimeout: () => 0, clearTimeout: () => {} },
  };
  const manager = new KeepaliveManager(keepaliveAPI, {
    trackedJobCount: () => 0,
    latestOpenURL: () => undefined,
    onAuthenticationChanged: (authenticated) => {
      void h.bridge.setKeepaliveAuthenticated(authenticated);
    },
  });
  await manager.init();
  h.tabs.live.set(777, { id: 777, url: "https://resolver.example.edu/account" });
  h.bridge.attachKeepalive(manager);

  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.session.state" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toMatchObject({
    ok: true,
    state: {
      authenticated: true,
      checking: false,
      resolverOrigin: "https://resolver.example.edu",
    },
  });
});

test("session sign-in reports why it cannot open without a resolver", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  await h.bridge.start();
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.signin" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "resolver_unavailable", message: "No resolver configured yet — open a paper first" },
  });
});

test("session sign-in opens the resolver origin in a foreground tab as fallback", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  const offer = jobOffer("job_session_signin") as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(offer);
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.signin" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([{ url: "https://resolver.example.edu", active: true }]);
});

test("handoff_focus surfaces the tracked work-window tab without creating another", async () => {
  const h = makeHarness(undefined, { windows: true });
  const jobID = "job_0001a_focus";
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  expect(h.tabs.created).toHaveLength(1);

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "handoff_focus",
    msg_id: "focus_00000001",
    job_id: jobID,
    seq: 1,
    payload: {},
  });
  for (
    let i = 0;
    i < 10 && !h.windows?.updated.some((update) => update.windowID === 500 && update.props.focused === true);
    i += 1
  ) {
    await Promise.resolve();
  }

  expect(h.tabs.created).toHaveLength(1);
  expect(h.tabs.activated).toContain(tabID);
  expect(h.windows?.updated).toContainEqual({
    windowID: 500,
    props: { focused: true, state: "normal" },
  });
  expect(h.tabs.navigations).toEqual([]);
});

test("handoff_focus re-drives an auth-pending tab without charging the automatic retry budget", async () => {
  const h = makeHarness(undefined, { windows: true });
  const jobID = "job_0001a_focus_auth";
  const idpURL = "https://idp.example.edu/sso";
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id ?? -1;
  const authTab = { id: tabID, url: idpURL, windowId: 500 };
  h.tabs.live.set(tabID, authTab);
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, authTab);
  const attemptsBefore = h.backend.store.authAttempts?.[jobID];
  h.tabs.navigations.splice(0);

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "handoff_focus",
    msg_id: "focus_00000002",
    job_id: jobID,
    seq: 2,
    payload: {},
  });
  for (let i = 0; i < 10 && h.tabs.navigations.length === 0; i += 1) {
    await Promise.resolve();
  }

  expect(h.tabs.navigations).toEqual([{ tabID, url: OPENURL }]);
  expect(h.backend.store.authAttempts?.[jobID]).toBe(attemptsBefore);
  expect(h.tabs.activated).toContain(tabID);
});

test("an inbox dismiss relays verdict dismiss through the native resolve", async () => {
  // Regression: the inbox and the native protocol both speak verdict
  // "dismiss" (Go enumRequired and protocol.ts both allow it), but the
  // broker's isResolveRuntimeRequest guard only accepted accept/reject, so
  // every human-action dismiss died as "Invalid action resolution request".
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
  };
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.9.0", features: ["triage_mutations_v1"] }));

  const pending = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.action.resolve", request: { action_id: 142, verdict: "dismiss", expected_revision: 1 } },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  const frame = await h.port.waitForFrame("human_action_resolve");
  expect(frame.payload["verdict"]).toBe("dismiss");
  expect(frame.payload["action_id"]).toBe(142);
  expect(frame.payload["expected_revision"]).toBe(1);
  expect(frame.payload["expected_sha256"]).toBeUndefined();

  await h.port.inbound(nativeResult("human_action_resolve_result", {
    request_id: frame.payload["request_id"],
    outcome: "applied",
  }));
  await expect(pending).resolves.toEqual({ ok: true, outcome: "applied" });

  // A genuinely unknown verdict must still be rejected with the exact error.
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.action.resolve", request: { action_id: 142, verdict: "cancel", expected_revision: 1 } },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "invalid_request", message: "Invalid action resolution request" },
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
  expect(debugLines.some((line) => line.join(" ").includes("unknown or late correlated response"))).toBe(true);
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

test("heartbeat counts obey disconnected, sign-in, permission, then pending badge precedence", async () => {
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
  await h.port.inbound(jobOffer("job_badge_auth"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toBe("Papio: 1 paper waiting on your institution sign-in");

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
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe("Papio: 1 paper waiting on your institution sign-in");

  h.deps.permissions.contains = async () => false;
  await h.bridge.syncConnectionBadge();
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe("Papio: 1 paper waiting on your institution sign-in");

  h.deps.permissions.contains = async () => true;
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });
  expect(h.action.texts.at(-1)).toBe("4");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
  expect(h.action.titles.at(-1)).toBe("Papio: 4 pending triage items");

  await h.port.emitDisconnect();
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.titles.at(-1)).toBe("Papio: daemon disconnected");
});

test("the sign-in badge clears when a handoff returns to its provider", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_badge_auth_return"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.action.texts.at(-1)).toBe("1");

  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });

  expect(h.action.texts.at(-1)).toBe("");
  expect(h.action.titles.at(-1)).toBe("Papio: connected");
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

// The the default institution Shibboleth dead end (idp.example.edu/idp/profile/SAML2/Redirect/SSO?execution=…)
// is classifiable ONLY by its title: that exact URL also serves the working
// login form. Chrome can deliver the title in a separate update after `complete`.
const STALE_IDP_URL = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?execution=e1s2";
const STALE_IDP_TITLE = "Example University Login Service - Stale Request";

/** Offer a job into a work window and return its broker tab id. */
async function offerIntoWorkWindow(h: Harness, jobID: string): Promise<number> {
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === jobID)?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  return tabID;
}

/** Land the broker tab on the dead Shibboleth page via a title-only update.
 * Later failures include their loading boundary so they are distinct documents. */
async function emitStaleIdPTitle(h: Harness, tabID: number, newDocument = false): Promise<void> {
  const tab = { id: tabID, url: STALE_IDP_URL, title: STALE_IDP_TITLE, windowId: 500 };
  h.tabs.live.set(tabID, tab);
  if (newDocument) {
    await h.tabs.onUpdated.emit(tabID, { url: STALE_IDP_URL }, tab);
    await h.tabs.onUpdated.emit(tabID, { status: "loading" }, tab);
  }
  await h.tabs.onUpdated.emit(tabID, { title: STALE_IDP_TITLE }, tab);
}

test("a title-only stale IdP page is detected, raises the work window, and re-drives", async () => {
  // The failure this pins: the handoff sat minimized on a dead sign-in page for
  // hours. Detection ran only on `status: complete` (missing the late title),
  // and the recovery re-drive returned before the surfacing code, so the one
  // moment papio knew a human was required was the one moment it stayed hidden.
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const tabID = await offerIntoWorkWindow(h, "job_stale_surface");
  expect(h.windows?.live.get(500)?.state).toBe("minimized");

  await emitStaleIdPTitle(h, tabID);

  const outcome = h.frames().find((f) => f.type === "handoff_outcome");
  expect(outcome?.job_id).toBe("job_stale_surface");
  expect(outcome?.payload).toMatchObject({ outcome: "stale_sso", final_host: "idp.example.edu" });
  // Surfaced: tab activated and the minimized window restored and focused.
  expect(h.tabs.activated).toContain(tabID);
  expect(h.windows?.updated).toContainEqual({
    windowID: 500,
    props: { focused: true, drawAttention: true, state: "normal" },
  });
  // ...and only then re-driven through the retained resolver link.
  expect(h.tabs.live.get(tabID)?.url).toBe(OPENURL);
  expect(h.backend.store.authAttempts?.["job_stale_surface"]).toBe(1);
});

test("recoverable IdP errors surface without navigating away from their forms", async () => {
  for (const [title, outcome] of [
    ["Password expired", "stale_sso"],
    ["Login error", "auth_error"],
  ] as const) {
    const h = makeHarness(undefined, { windows: true });
    await h.bridge.start();
    const tabID = await offerIntoWorkWindow(h, `job_recoverable_${outcome}`);
    const loginTab = { id: tabID, url: STALE_IDP_URL, title: "Institution sign-in", windowId: 500 };
    h.tabs.live.set(tabID, loginTab);
    await h.tabs.onUpdated.emit(tabID, { url: STALE_IDP_URL }, loginTab);
    expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");

    const errorTab = { ...loginTab, title };
    h.tabs.live.set(tabID, errorTab);
    await h.tabs.onUpdated.emit(tabID, { title }, errorTab);

    expect(h.frames().find((frame) => frame.type === "handoff_outcome")?.payload).toMatchObject({ outcome });
    expect(h.tabs.navigations).toEqual([]);
    expect(h.tabs.live.get(tabID)?.url).toBe(STALE_IDP_URL);
    expect(h.tabs.activated).toContain(tabID);
    expect(h.windows?.updated).toContainEqual({
      windowID: 500,
      props: { focused: true, drawAttention: true, state: "normal" },
    });
  }
});

test("concurrent stale-page title and complete events spend one recovery attempt", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const tabID = await offerIntoWorkWindow(h, "job_stale_concurrent");
  const tab = { id: tabID, url: STALE_IDP_URL, title: STALE_IDP_TITLE, windowId: 500 };
  h.tabs.live.set(tabID, tab);

  await Promise.all([
    h.tabs.onUpdated.emit(tabID, { url: STALE_IDP_URL, status: "complete" }, tab),
    h.tabs.onUpdated.emit(tabID, { title: STALE_IDP_TITLE }, tab),
  ]);

  expect(h.backend.store.authAttempts?.["job_stale_concurrent"]).toBe(1);
  expect(h.tabs.navigations).toEqual([{ tabID, url: OPENURL }]);
});

test("the stale re-drive is charged to the durable budget, not a worker-local latch", async () => {
  // The re-drive reuses the SAME tab, so noteAuthAttempt's tab-id debounce
  // cannot bound it, and the old worker-local `handoffOutcomeSent` latch was
  // cleared by every service-worker restart — leaving the resolver loop
  // unbounded across restarts. At the cap the tab must be LEFT on the failure
  // page (the user needs to see it) and the job reported human_auth_required.
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const tabID = await offerIntoWorkWindow(h, "job_stale_budget");

  for (let attempt = 1; attempt <= 3; attempt++) {
    await emitStaleIdPTitle(h, tabID, attempt > 1);
    expect(h.backend.store.authAttempts?.["job_stale_budget"]).toBe(attempt);
    expect(h.tabs.live.get(tabID)?.url).toBe(OPENURL); // re-driven
  }

  await emitStaleIdPTitle(h, tabID, true);
  expect(h.backend.store.authAttempts?.["job_stale_budget"]).toBe(3); // capped
  expect(h.tabs.live.get(tabID)?.url).toBe(STALE_IDP_URL); // left for the human
  const stalled = h.frames().filter((f) => f.type === "provider_outcome");
  expect(stalled.length).toBe(1);
  expect(stalled[0]?.payload).toMatchObject({ outcome: "human_auth_required" });
});

test("one dead IdP page reports once but a repeat page still re-raises nothing new", async () => {
  // The daemon audit trail must not be spammed with identical handoff_failed
  // events, and focus must not be yanked once per re-drive in a loop.
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const tabID = await offerIntoWorkWindow(h, "job_stale_debounce");

  await emitStaleIdPTitle(h, tabID);
  await emitStaleIdPTitle(h, tabID);

  expect(h.frames().filter((f) => f.type === "handoff_outcome").length).toBe(1);
  expect(
    h.windows?.updated.filter((u) => u.props.focused === true).length,
  ).toBe(1);
});


// The inbox "View PDF" control on a verify_identity action. The daemon issues a
// loopback capability URL promptly (confirmed against a live daemon), but the
// reply type was absent from onInbound's correlation routing, so the frame was
// dropped as an echo and every click failed with "The daemon did not respond in
// time" after the full request timeout. This drives the whole path.
test("a review preview capability reaches the caller that asked for it", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: "0.12.0", features: ["review_preview_v1"] }),
  );

  const pending = h.bridge.requestPreview({ action_id: 213 });
  await Promise.resolve();
  await Promise.resolve();
  const request = h.frames().filter((frame) => frame.type === "review_preview_request");
  expect(request).toHaveLength(1);

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "review_preview_result",
    msg_id: "previewresult000000000000000001",
    seq: 99,
    payload: {
      request_id: request[0]?.payload["request_id"],
      outcome: "ok",
      url: "http://127.0.0.1:50795/p/YD5r_87qRLi0Ut9XMwt8E4Cq-Bv7N4VvSoEyMsH7-Fs",
      sha256: "434376057dbe6fc3e44d0fdb7268b56126e9b47ca9d6aa9e05cf2e738f72e81d",
      size_bytes: 2570670,
      expires_at: "2036-07-27T02:39:46.877077Z",
    },
  } as never);

  await expect(pending).resolves.toMatchObject({
    ok: true,
    preview: { url: "http://127.0.0.1:50795/p/YD5r_87qRLi0Ut9XMwt8E4Cq-Bv7N4VvSoEyMsH7-Fs" },
  });
});

// A structured refusal must surface as a named reason, not as another silent
// timeout. The transport succeeded, so ok stays true and the inbox branches on
// outcome — see the comment at inbox.ts around the preview click handler.
test("a refused review preview reports the daemon's reason", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: "0.12.0", features: ["review_preview_v1"] }));

  const pending = h.bridge.requestPreview({ action_id: 9999 });
  await Promise.resolve();
  await Promise.resolve();
  const request = h.frames().filter((frame) => frame.type === "review_preview_request");

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "review_preview_result",
    msg_id: "previewresult000000000000000002",
    seq: 100,
    payload: {
      request_id: request[0]?.payload["request_id"],
      outcome: "error",
      detail: "review action 9999 is unavailable",
    },
  } as never);

  await expect(pending).resolves.toEqual({
    ok: true,
    outcome: "error",
    detail: "review action 9999 is unavailable",
  });
});

test("a direct-file offer that requires a sign-in is queued, never downloaded", async () => {
  // isDirectFileOffer matches on URL shape alone. An institutional offer's URL
  // is the operator's configured OpenURL base, whose path papio does not
  // constrain, so a pdf-shaped base would otherwise route a sign-in-required
  // offer straight into chrome.downloads with no human present.
  const h = makeHarness();
  const offer = jobOffer("job_0091_auth_direct") as { payload: Record<string, unknown> };
  offer.payload["openurl"] = "https://library.example.edu/openurl/pdf/10.1000-x";
  offer.payload["requires_auth"] = true;
  await h.bridge.start();
  await h.port.inbound(offer);

  expect(h.downloads.started).toEqual([]);
  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_0091_auth_direct");
  expect(job?.status).toBe("queued");
});


test("handoff governor keeps two drives, drains FIFO on settle and timeout", async () => {
  const h = makeHarness({ ...emptyStore(), lastAuthReturnedAt: 1_700_000_000_000 });
  await h.bridge.start();
  const jobIDs = Array.from({ length: 5 }, (_, index) => `job_governor_${index}`);
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.live.size).toBe(2);
  expect(h.tabs.created).toHaveLength(2);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id < 0)).toHaveLength(3);
  expect(h.frames().filter((frame) => frame.type === "job_accept")).toHaveLength(5);

  const first = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(first?.tab_id).toBeGreaterThanOrEqual(0);
  await h.bridge.requestCancel(jobIDs[0]!);
  expect(h.tabs.live.size).toBe(2);
  expect(h.tabs.created).toHaveLength(3);

  const timeout = h.timers.at(-1);
  expect(timeout?.ms).toBe(180_000);
  h.clock.now += 180_000;
  await timeout?.fn();
  expect(h.tabs.live.size).toBe(2);
  expect(h.tabs.created).toHaveLength(4);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[2])?.status).toBe("auth_pending");
});

test("successful adoption closes a managed handoff while auth keeps its page", async () => {
  const success = makeHarness();
  await success.bridge.start();

  await success.port.inbound(jobOffer("job_close_success"));
  const successTab = success.backend.store.activeJobs[0]?.tab_id ?? -1;
  await success.downloads.onCreated.emit({ id: 901, tabId: successTab, state: "in_progress" });
  success.downloads.items.set(901, {
    id: 901,
    tabId: successTab,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 100,
    state: "complete",
  });
  await success.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  await success.port.inbound({
    protocol: "papio-browser/1",
    type: "ack",
    msg_id: "ack_close_success",
    job_id: "job_close_success",
    seq: 1,
    payload: {},
  });
  expect(success.tabs.removed).toEqual([successTab]);

  const human = makeHarness();
  await human.bridge.start();
  await human.port.inbound(jobOffer("job_close_human"));
  const humanTab = human.backend.store.activeJobs[0]?.tab_id ?? -1;
  await human.tabs.onUpdated.emit(
    humanTab,
    { url: "https://idp.example.edu/login", status: "complete" },
    { id: humanTab, url: "https://idp.example.edu/login" },
  );
  expect(human.tabs.removed).toEqual([]);
  expect(human.tabs.live.has(humanTab)).toBe(true);
});
test("cold handoffs consolidate to one tab before warm evidence fills the second slot", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const jobIDs = Array.from({ length: 5 }, (_, index) => `job_cold_governor_${index}`);
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.live.size).toBe(1);
  expect(h.tabs.created).toHaveLength(1);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id < 0)).toHaveLength(4);

  await h.bridge.setKeepaliveAuthenticated(true);
  expect(h.tabs.live.size).toBe(2);
  expect(h.tabs.created).toHaveLength(2);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id < 0)).toHaveLength(3);
});

test("handoff group reducer trails auth recollapse and stays quiet when collapsed", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_group_debounce"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  const idpURL = "https://idp.example.edu/login";
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(false);
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(false);
  const collapse = h.timers.find((timer) => timer.ms === 5_000);
  expect(collapse).toBeDefined();
  h.clock.now += 5_000;
  await collapse?.fn();
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(true);
  const updates = h.tabGroups?.updated.length ?? 0;
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });
  expect(h.tabGroups?.updated.length).toBe(updates);
});

test("orphan scan flags ledgered and papio-group leftovers but never tracked or active tabs", async () => {
  const h = makeHarness(undefined, { tabGroups: true });
  let ledger: Record<string, number> = { "300": 1, "301": 1, "999": 1 };
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_orphan_live"));
  const trackedTab = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(trackedTab).toBeGreaterThanOrEqual(0);
  // 300: ledgered orphan from a prior life. 301: ledgered but the user is
  // looking at it right now. 999: ledger entry whose tab is already gone.
  h.tabs.live.set(300, { id: 300, url: "https://provider.example.org/a" });
  h.tabs.live.set(301, { id: 301, url: "https://provider.example.org/b", active: true });
  // 302: pre-ledger leftover still sitting in a papio-titled group.
  h.tabs.live.set(302, { id: 302, url: "https://provider.example.org/c", groupId: 700 });
  h.tabGroups!.live.set(700, { id: 700, collapsed: true, title: "papio", windowId: 1 });
  // 303: the pinned keepalive resolver tab folded into the papio group —
  // papio's own session anchor, never an orphan.
  h.tabs.live.set(303, { id: 303, url: "https://resolver.example.edu/keepalive", groupId: 700, pinned: true });

  const status = await h.bridge.orphanTabStatus();
  expect(status).toEqual({ count: 2, tab_ids: [300, 302] });
  // The dead entry is pruned from the durable ledger as a scan side effect.
  expect(ledger["999"]).toBeUndefined();

  const { closed } = await h.bridge.cleanupOrphanTabs();
  expect(closed).toBe(2);
  expect(h.tabs.removed).toEqual([300, 302]);
  expect(h.tabs.live.has(trackedTab)).toBe(true);
  expect(h.tabs.live.has(301)).toBe(true);
  expect(ledger["300"]).toBeUndefined();
  // Closing an orphan is programmatic: no cancel frame may reach the daemon.
  expect(h.frames().filter((frame) => frame.type === "cancel")).toHaveLength(0);
});

test("created broker tabs are ledgered durably and forgotten once they close", async () => {
  const h = makeHarness();
  let ledger: Record<string, number> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ledgered"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  expect(ledger[String(tabID)]).toBeDefined();

  h.tabs.live.delete(tabID);
  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });
  expect(ledger[String(tabID)]).toBeUndefined();
});