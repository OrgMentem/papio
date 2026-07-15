// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Behavioural tests for the MV3 bridge against a minimal fake chrome surface and
// a fake native port. No real chrome, and no wall-clock timers: every fake
// emitter awaits the handler promises it triggers, so the flow is deterministic.

import { expect, test } from "bun:test";

import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape } from "../src/state";
import {
  Bridge,
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
  disconnected = false;
  postMessage(msg: object): void {
    this.posted.push(msg);
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

class FakeTabs {
  readonly onUpdated = new FakeEmitter<[number, TabChangeInfo, TabInfo]>();
  readonly onRemoved = new FakeEmitter<[number, { isWindowClosing: boolean }]>();
  readonly created: { url: string; active: boolean }[] = [];
  readonly removed: number[] = [];
  readonly reloaded: number[] = [];
  readonly live = new Map<number, TabInfo>();
  nextId = 100;
  failCreate = false;
  async create(props: { url: string; active: boolean }): Promise<TabInfo> {
    this.created.push(props);
    if (this.failCreate) throw new Error("tab creation blocked");
    const id = this.nextId++;
    const tab: TabInfo = { id, url: props.url };
    this.live.set(id, tab);
    return tab;
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

interface Harness {
  bridge: Bridge;
  deps: BridgeDeps;
  port: FakePort;
  ports: FakePort[];
  backend: FakeBackend;
  tabs: FakeTabs;
  downloads: FakeDownloads;
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  frames(): BrowserMessage[];
  postedStrings(): string[];
}

function makeHarness(seed?: StoreShape): Harness {
  const port = new FakePort();
  const ports = [port];
  let connects = 0;
  const backend = new FakeBackend();
  if (seed) backend.store = seed;
  const tabs = new FakeTabs();
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const timers: { fn: () => void | Promise<void>; ms: number }[] = [];
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
  };
  return {
    bridge: new Bridge(deps),
    deps,
    port,
    ports,
    backend,
    tabs,
    downloads,
    clock,
    timers,
    frames: () => ports.flatMap((p) => p.posted.map(parseBrowserMessage)),
    postedStrings: () => ports.flatMap((p) => p.posted.map((f) => JSON.stringify(f))),
  };
}

function jobOffer(jobID: string): unknown {
  return {
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000001",
    job_id: jobID,
    seq: 0,
    payload: {
      openurl: OPENURL,
      provider_hosts: [PROVIDER_HOST],
      access_mode: "assisted",
      expires_at: EXPIRES,
    },
  };
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

test("hello is the first outgoing frame with a valid msg_id and seq 0", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const first = h.frames()[0];
  expect(first?.type).toBe("hello");
  expect(first?.seq).toBe(0);
  expect(first?.msg_id).toMatch(/^[A-Za-z0-9_-]{8,64}$/);
  expect(first?.payload["extension_version"]).toBe("0.1.0");
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

test("unplanned port death reconnects with backoff; deliberate disconnect stays down", async () => {
  // Unplanned: daemon restarted -> onDisconnect without a protocol error.
  const h = makeHarness();
  await h.bridge.start();
  const hellosBefore = h.frames().filter((f) => f.type === "hello").length;
  await h.port.emitDisconnect();
  expect(h.timers.length).toBe(1);
  expect(h.timers[0]?.ms).toBe(1000);
  h.timers[0]?.fn();
  const hellosAfter = h.frames().filter((f) => f.type === "hello").length;
  expect(hellosAfter).toBe(hellosBefore + 1); // re-hello on reconnect

  // Deliberate: malformed frame -> fail-closed disconnect, no timer scheduled.
  const bad = makeHarness();
  await bad.bridge.start();
  const timersBefore = bad.timers.length;
  await bad.port.inbound({ protocol: "papio-browser/1", type: "not_a_type", msg_id: "x", seq: 0, payload: {} });
  expect(bad.port.disconnected).toBe(true);
  expect(bad.timers.length).toBe(timersBefore);
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
