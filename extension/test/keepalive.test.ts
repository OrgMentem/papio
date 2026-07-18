// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Deterministic manager tests: all scheduling is a fake one-shot timer, never
// a wall-clock interval or a Chrome API.

import { expect, test } from "bun:test";

import {
  clampKeepaliveInterval,
  KeepaliveManager,
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

  async query(_query: {
    pinned?: boolean;
    muted?: boolean;
    url?: string[];
  }): Promise<KeepaliveTab[]> {
    return [];
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

function makeHarness(interval: unknown = 4, workWindowID?: () => number | undefined): {
  manager: KeepaliveManager;
  jobs: { count: number };
  tabs: FakeTabs;
  timers: FakeTimers;
  badge: string[];
  reauths: { count: number };
} {
  const jobs = { count: 1 };
  const tabs = new FakeTabs();
  const timers = new FakeTimers();
  const badge: string[] = [];
  const reauths = { count: 0 };
  const api: KeepaliveAPI = {
    tabs,
    timers,
    storage: { get: async () => ({ "keepalive.interval": interval, "keepalive.enabled": true }) },
    action: { setBadgeText: async ({ text }) => void badge.push(text) },
  };
  const manager = new KeepaliveManager(api, {
    trackedJobCount: () => jobs.count,
    latestOpenURL: () => RESOLVER_OPENURL,
    ...(workWindowID !== undefined ? { workWindowID } : {}),
    onReauthNeeded: () => {
      reauths.count += 1;
    },
    observeMs: 10,
    reloadSettleMs: 1,
  });
  return { manager, jobs, tabs, timers, badge, reauths };
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
  expect(h.badge).toEqual(["!"]);
  expect(h.reauths.count).toBe(1);
  expect(h.tabs.updates).toEqual([{ id: 1, properties: { active: true, pinned: false, muted: false } }]);
  expect(h.timers.latestDelay()).toBe(10);

  h.tabs.nextURL = RESOLVER_OPENURL;
  h.tabs.live.get(1)!.url = RESOLVER_OPENURL; // Simulate the user's completed login.
  await h.timers.runNext();

  expect(h.badge).toEqual(["!", ""]);
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
