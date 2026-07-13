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
  async remove(tabID: number): Promise<void> {
    this.removed.push(tabID);
    this.live.delete(tabID);
  }
}

class FakeDownloads {
  readonly onCreated = new FakeEmitter<[DownloadItemLike]>();
  readonly onChanged = new FakeEmitter<[DownloadDeltaLike]>();
  readonly items = new Map<number, DownloadItemLike>();
  async search(query: { id: number }): Promise<DownloadItemLike[]> {
    const item = this.items.get(query.id);
    return item ? [item] : [];
  }
}

interface Harness {
  bridge: Bridge;
  deps: BridgeDeps;
  port: FakePort;
  backend: FakeBackend;
  tabs: FakeTabs;
  downloads: FakeDownloads;
  clock: { now: number };
  frames(): BrowserMessage[];
  postedStrings(): string[];
}

function makeHarness(seed?: StoreShape): Harness {
  const port = new FakePort();
  const backend = new FakeBackend();
  if (seed) backend.store = seed;
  const tabs = new FakeTabs();
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const deps: BridgeDeps = {
    connectNative: () => port,
    manifestVersion: "0.1.0",
    randomUUID: () => crypto.randomUUID(),
    now: () => clock.now,
    backend,
    tabs,
    downloads,
  };
  return {
    bridge: new Bridge(deps),
    deps,
    port,
    backend,
    tabs,
    downloads,
    clock,
    frames: () => port.posted.map(parseBrowserMessage),
    postedStrings: () => port.posted.map((f) => JSON.stringify(f)),
  };
}

function jobOffer(jobID: string): unknown {
  return {
    protocol: "papio-browser/0.1",
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

test("a malformed inbound frame fails closed by disconnecting", async () => {
  const h = makeHarness();
  await h.bridge.start();
  expect(h.port.disconnected).toBe(false);
  await h.port.inbound({ protocol: "papio-browser/0.1", type: "not_a_type", msg_id: "x", seq: 0, payload: {} });
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
