// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Deterministic manager tests: all scheduling is a fake one-shot timer, never
// a wall-clock interval or a Chrome API.

import { expect, test } from "bun:test";

import {
  clampKeepaliveInterval,
  KeepaliveManager,
  SESSION_STALE_MS,
  type KeepaliveAPI,
  type KeepaliveTab,
  type KeepaliveTimers,
} from "../src/keepalive";

const RESOLVER_OPENURL = "https://resolver.example.edu/openurl?genre=article";

class FakeTimers implements KeepaliveTimers {
  private nextID = 1;
  private readonly pending = new Map<
    number,
    { callback: () => void | Promise<void>; delayMs: number }
  >();
  readonly delays: number[] = [];

  setTimeout(callback: () => void | Promise<void>, delayMs: number): number {
    const id = this.nextID++;
    this.pending.set(id, { callback, delayMs });
    this.delays.push(delayMs);
    return id;
  }

  clearTimeout(handle: unknown): void {
    if (typeof handle === "number") this.pending.delete(handle);
  }

  async runNext(): Promise<void> {
    const entry = this.pending.entries().next().value as
      | [number, { callback: () => void | Promise<void>; delayMs: number }]
      | undefined;
    if (entry === undefined) throw new Error("no scheduled timer");
    this.pending.delete(entry[0]);
    await entry[1].callback();
  }

  latestDelay(): number | undefined {
    return this.delays.at(-1);
  }
}

class FakeTabs {
  readonly created: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
  }[] = [];
  /** When set, creation into any windowId throws (window closed race). */
  failWindowCreate = false;
  readonly reloaded: number[] = [];
  readonly removed: number[] = [];
  readonly updates: { id: number; properties: { active?: boolean; pinned?: boolean; muted?: boolean } }[] = [];
  readonly resolverTabs: KeepaliveTab[] = [];
  queryCount = 0;
  readonly live = new Map<number, KeepaliveTab>();
  nextURL: string | undefined;

  async create(properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
  }): Promise<KeepaliveTab> {
    if (this.failWindowCreate && properties.windowId !== undefined) {
      this.created.push(properties);
      throw new Error("no such window");
    }
    const id = this.created.length + 1;
    this.created.push(properties);
    this.live.set(id, { id, url: properties.url });
    return { id, url: properties.url };
  }

  async reload(id: number): Promise<void> {
    this.reloaded.push(id);
    const tab = this.live.get(id);
    if (tab !== undefined && this.nextURL !== undefined) tab.url = this.nextURL;
  }

  async query(query: {
    pinned?: boolean;
    muted?: boolean;
    url?: string[];
  }): Promise<KeepaliveTab[]> {
    this.queryCount += 1;
    return query.url === undefined ? [] : [...this.resolverTabs];
  }

  async get(id: number): Promise<KeepaliveTab> {
    const tab = this.live.get(id);
    if (tab === undefined) throw new Error("tab is gone");
    return tab;
  }

  async remove(id: number): Promise<void> {
    this.removed.push(id);
    this.live.delete(id);
  }

  async update(
    id: number,
    properties: { active?: boolean; pinned?: boolean; muted?: boolean },
  ): Promise<KeepaliveTab> {
    this.updates.push({ id, properties });
    return this.get(id);
  }
}

interface HarnessResolver {
  latestOpenURL?: string | undefined;
  storedOrigin?: unknown;
  grantedOrigins?: string[];
}

function makeHarness(
  interval: unknown = 4,
  workWindowID?: () => number | undefined,
  resolverConfig?: HarnessResolver,
): {
  manager: KeepaliveManager;
  jobs: { count: number };
  tabs: FakeTabs;
  timers: FakeTimers;
  badge: string[];
  reauths: { count: number };
  reauthState: boolean[];
  storageValues: Record<string, unknown>;
} {
  const jobs = { count: 1 };
  const tabs = new FakeTabs();
  const timers = new FakeTimers();
  const badge: string[] = [];
  const reauths = { count: 0 };
  const reauthState: boolean[] = [];
  const storageValues: Record<string, unknown> = {
    "keepalive.interval": interval,
    "keepalive.enabled": true,
    "keepalive.resolverOrigin": resolverConfig?.storedOrigin,
  };
  const latestOpenURL = resolverConfig === undefined ? RESOLVER_OPENURL : resolverConfig.latestOpenURL;
  const api: KeepaliveAPI = {
    tabs,
    timers,
    storage: {
      get: async () => ({ ...storageValues }),
      set: async (values) => {
        Object.assign(storageValues, values);
      },
    },
    permissions: {
      getAll: async () => ({ origins: resolverConfig?.grantedOrigins ?? [] }),
    },
    action: { setBadgeText: async ({ text }) => void badge.push(text) },
  };
  const manager = new KeepaliveManager(api, {
    trackedJobCount: () => jobs.count,
    latestOpenURL: () => latestOpenURL,
    ...(workWindowID !== undefined ? { workWindowID } : {}),
    onReauthNeeded: () => {
      reauths.count += 1;
    },
    onReauthStateChanged: (paused) => {
      reauthState.push(paused);
    },
    observeMs: 10,
    reloadSettleMs: 1,
  });
  return { manager, jobs, tabs, timers, badge, reauths, reauthState, storageValues };
}

test("creates one pinned resolver tab, reloads it, and closes it when jobs finish", async () => {
  const h = makeHarness();
  await h.manager.init();

  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true },
  ]);
  expect(h.timers.latestDelay()).toBe(4 * 60_000);

  await h.timers.runNext();
  expect(h.tabs.reloaded).toEqual([1]);
  expect(h.timers.latestDelay()).toBe(1);

  await h.timers.runNext();
  expect(h.timers.latestDelay()).toBe(4 * 60_000);

  h.jobs.count = 0;
  await h.manager.sync();
  expect(h.tabs.removed).toEqual([1]);
});

test("pauses after an IdP redirect, notifies the user, and resumes on resolver recovery", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.tabs.nextURL = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver";

  await h.timers.runNext(); // reload
  await h.timers.runNext(); // bounded final-URL inspection

  expect(h.tabs.reloaded).toEqual([1]);
  expect(h.reauthState).toEqual([true]);
  expect(h.reauths.count).toBe(1);
  expect(h.tabs.updates).toEqual([{ id: 1, properties: { active: true, pinned: false, muted: false } }]);
  expect(h.timers.latestDelay()).toBe(10);

  h.tabs.nextURL = RESOLVER_OPENURL;
  h.tabs.live.get(1)!.url = RESOLVER_OPENURL; // Simulate the user's completed login.
  await h.timers.runNext();

  expect(h.reauthState).toEqual([true, false]);
  expect(h.tabs.updates).toEqual([
    { id: 1, properties: { active: true, pinned: false, muted: false } },
    { id: 1, properties: { pinned: true, muted: true } },
  ]);
  expect(h.timers.latestDelay()).toBe(4 * 60_000);

  await h.timers.runNext();
  expect(h.tabs.reloaded).toEqual([1, 1]);
});

test("clamps configured intervals to supported bounds", async () => {
  expect(clampKeepaliveInterval(undefined)).toBe(4);
  expect(clampKeepaliveInterval(0)).toBe(2);
  expect(clampKeepaliveInterval(99)).toBe(30);
  expect(clampKeepaliveInterval(4.8)).toBe(4);

  const low = makeHarness(0);
  await low.manager.init();
  expect(low.timers.latestDelay()).toBe(2 * 60_000);

  const high = makeHarness(99);
  await high.manager.init();
  expect(high.timers.latestDelay()).toBe(30 * 60_000);
});

test("creates the keepalive tab inside the work window, falling back when it is gone", async () => {
  const routed = makeHarness(4, () => 500);
  await routed.manager.init();
  expect(routed.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true, windowId: 500 },
  ]);

  // The work window closed between lookup and create: retry lands the tab in
  // the user's current window instead of skipping the keepalive cycle.
  const fallback = makeHarness(4, () => 500);
  fallback.tabs.failWindowCreate = true;
  await fallback.manager.init();
  expect(fallback.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true, windowId: 500 },
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true },
  ]);
});

test("snapshot exposes session state without leaking an IdP host", async () => {
  const h = makeHarness();
  await h.manager.init();
  let snapshot = h.manager.getSnapshot();
  expect(snapshot).toMatchObject({
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    pausedForReauth: false,
    resolverOrigin: "https://resolver.example.edu",
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
  });
  h.tabs.nextURL = "https://idp.example.edu/sso/login";
  await h.timers.runNext();
  await h.timers.runNext();
  snapshot = h.manager.getSnapshot();
  expect(snapshot.pausedForReauth).toBe(true);
  expect(snapshot.resolverOrigin).toBe("https://resolver.example.edu");
  expect(snapshot.resolverOrigin).not.toContain("idp.example.edu");
});
test("snapshot resolves a durable resolver before granted permission fallback", async () => {
  const stored = makeHarness(4, undefined, {
    latestOpenURL: undefined,
    storedOrigin: "https://stored.resolver.example",
    grantedOrigins: ["https://granted.resolver.example/*"],
  });
  await stored.manager.init();
  expect(stored.manager.getSnapshot().resolverOrigin).toBe("https://stored.resolver.example");

  const fallback = makeHarness(4, undefined, {
    latestOpenURL: undefined,
    grantedOrigins: ["https://granted.resolver.example/*"],
  });
  await fallback.manager.init();
  expect(fallback.manager.getSnapshot().resolverOrigin).toBe("https://granted.resolver.example");
});

test("on-demand session checks run when unknown and after the freshness window", async () => {
  const originalNow = Date.now;
  let now = originalNow();
  Date.now = () => now;
  try {
    const h = makeHarness();
    await h.manager.init();
    const initialQueries = h.tabs.queryCount;

    await h.manager.checkNow(100);
    expect(h.tabs.queryCount).toBeGreaterThan(initialQueries);
    const firstCheck = h.manager.getSnapshot();
    expect(firstCheck.lastCheckAt).toBe(now);
    expect(firstCheck.authenticated).toBe(true);

    const freshQueries = h.tabs.queryCount;
    await h.manager.checkNow(100);
    expect(h.tabs.queryCount).toBe(freshQueries);

    now += SESSION_STALE_MS + 1;
    h.tabs.live.get(1)!.url = "https://resolver.example.edu/account";
    await h.manager.checkNow(100);
    expect(h.tabs.queryCount).toBeGreaterThan(freshQueries);
    expect(h.manager.getSnapshot().lastCheckAt).toBe(now);
  } finally {
    Date.now = originalNow;
  }
});

test("a live resolver tab is evidence while its probe determines the verdict", async () => {
  const h = makeHarness();
  await h.manager.init();
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(liveTab.id, liveTab);
  h.tabs.resolverTabs.push(liveTab);
  const pending = h.manager.checkNow(100);
  const during = h.manager.getSnapshot();
  expect(during.checking).toBe(true);
  // The fake browser resolves immediately, so the completed state is the
  // authoritative resolver-origin verdict rather than the interim evidence.
  await pending;
  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: true,
    checking: false,
    resolverOrigin: "https://resolver.example.edu",
  });
});
