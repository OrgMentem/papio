// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Deterministic manager tests: all scheduling is a fake one-shot timer, never
// a wall-clock interval or a Chrome API.

import { expect, test } from "bun:test";
import { Window } from "happy-dom";

import {
  classifyResolverJWTIdentity,
  classifyResolverMarkers,
  clampKeepaliveInterval,
  collectResolverMarkers,
  isAuthenticationURL,
  KeepaliveManager,
  SESSION_STALE_MS,
  type KeepaliveAPI,
  type KeepaliveTab,
  type KeepaliveTimers,
  type ResolverMarker,
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
  focusedTab: KeepaliveTab | undefined;
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
    active?: boolean;
    lastFocusedWindow?: boolean;
  }): Promise<KeepaliveTab[]> {
    this.queryCount += 1;
    if (query.active === true) return this.focusedTab === undefined ? [] : [this.focusedTab];
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
  warmDemand?: () => boolean,
): {
  manager: KeepaliveManager;
  api: KeepaliveAPI;
  jobs: { count: number };
  tabs: FakeTabs;
  timers: FakeTimers;
  badge: string[];
  reauths: { count: number };
  reauthState: boolean[];
  storageValues: Record<string, unknown>;
  resolverMarkers: ResolverMarker[];
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
  const resolverMarkers: ResolverMarker[] = [{ text: "Sign out", label: "" }];
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
    scripting: {
      executeScript: async () => [{ result: resolverMarkers }],
    },
    action: { setBadgeText: async ({ text }) => void badge.push(text) },
  };
  const manager = new KeepaliveManager(api, {
    trackedJobCount: () => jobs.count,
    latestOpenURL: () => latestOpenURL,
    ...(warmDemand !== undefined ? { warmDemand } : {}),
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
  return {
    manager,
    api,
    jobs,
    tabs,
    timers,
    badge,
    reauths,
    reauthState,
    storageValues,
    resolverMarkers,
  };
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
test("daemon warm demand keeps the resolver tab and closes after demand expires", async () => {
  let demand = true;
  const h = makeHarness(4, undefined, undefined, () => demand);
  h.jobs.count = 0;
  await h.manager.init();
  expect(h.tabs.created).toHaveLength(1);

  demand = false;
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
test("authentication URL detection ignores query strings and uses exact hostname segments", () => {
  expect(
    isAuthenticationURL(
      "https://example.primo.exlibrisgroup.com/nde/account/overview?vid=61EXU_INST:61EXU_NDE&lang=en&fromLogin=true",
    ),
  ).toBe(false);
  expect(isAuthenticationURL("https://resolver.example.edu/account?next=login")).toBe(false);
  expect(isAuthenticationURL("https://idp.example.edu/saml/return")).toBe(true);
  expect(isAuthenticationURL("https://login.example.edu/")).toBe(true);
  expect(isAuthenticationURL("https://example.edu/auth/callback")).toBe(true);
  expect(isAuthenticationURL("https://notidp.example.edu/account")).toBe(false);
});

test("resolver marker classifier prioritizes sign-out and handles Primo-shaped account markup", () => {
  expect(
    classifyResolverMarkers([
      { text: "Jane Doe", label: "" },
      { text: "My account", label: "" },
      { text: "Sign out", label: "Account menu" },
    ]),
  ).toBe("in");
  expect(classifyResolverMarkers([{ text: "Search", label: "" }, { text: "", label: "Sign in" }])).toBe("out");
  expect(classifyResolverMarkers([{ text: "Search", label: "" }])).toBe("unknown");
  expect(
    classifyResolverMarkers([
      { text: "Sign in", label: "" },
      { text: "Sign out", label: "" },
    ]),
  ).toBe("in");
});
test("resolver-origin marker verdicts are evidence-based and do not pause a live user tab", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign in", label: "" });
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(liveTab.id, liveTab);
  h.tabs.resolverTabs.push(liveTab);

  await h.manager.checkNow(100);

  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: false,
    verdict: "out",
    probeSource: "live_tab",
  });
  expect(h.reauthState).toEqual([]);
});

test("no resolver tab or probe evidence remains unknown instead of signed out", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  await h.manager.checkNow(100);

  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: false,
    verdict: "unknown",
    probeSource: "none",
  });
  expect(h.manager.getSnapshot().lastVerdictAt).toEqual(expect.any(Number));
});

test("the focused resolver tab is inspected even when the URL-pattern query misses", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign out", label: "" });
  const focused = { id: 77, url: "https://resolver.example.edu/account?fromLogin=true" };
  h.tabs.live.set(focused.id, focused);
  h.tabs.focusedTab = focused;
  // Field report 12:43pm: pattern query returned nothing while the operator
  // was looking at the signed-in library page in the active tab.

  await h.manager.checkNow(100);

  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
  });
});

test("an evidence-free probe does not latch: the next popup open re-probes", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();
  await h.manager.checkNow(100);
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "unknown", probeSource: "none" });

  // The operator focuses the library page and reopens the popup well within
  // SESSION_STALE_MS. The empty verdict must not be served from the latch.
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign out", label: "" });
  const focused = { id: 78, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(focused.id, focused);
  h.tabs.focusedTab = focused;

  await h.manager.checkNow(100);

  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in", probeSource: "live_tab" });
});

function syntheticJWT(payload: Record<string, unknown>): string {
  const encode = (value: string): string =>
    btoa(value).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  return `${encode('{"alg":"none"}')}.${encode(JSON.stringify(payload))}.${encode("sig")}`;
}

test("JWT identity classifier ignores guests, malformed values, and oversized storage", () => {
  const cases: readonly [string, readonly string[], "in" | "unknown"][] = [
    ["guest group", [syntheticJWT({ userName: "Jane Doe", userGroup: "GUEST" })], "unknown"],
    ["named user", [syntheticJWT({ userName: "Jane Doe", userGroup: "STUDENT" })], "in"],
    ["snake case identity", [syntheticJWT({ user_name: "jane", user_group: "STAFF" })], "in"],
    ["malformed segments", ["not.a.jwt!"], "unknown"],
    ["oversized value", ["a".repeat(8 * 1024)], "unknown"],
    // Anonymous session tokens carry opaque subs on many platforms — a bare
    // sub without a group claim must NOT read as signed in (cross-institution
    // safety; only the Ex Libris-style non-guest group disambiguates a sub).
    ["bare sub, no group claim", [syntheticJWT({ sub: "a81bc81b-dead-4e5d" })], "unknown"],
    ["sub with non-guest group", [syntheticJWT({ sub: "u123", userGroup: "STAFF" })], "in"],
    ["sub with empty group", [syntheticJWT({ sub: "u123", userGroup: "" })], "unknown"],
    ["named user without group claim", [syntheticJWT({ preferred_username: "jane" })], "in"],
  ];
  for (const [name, values, expected] of cases) {
    expect(classifyResolverJWTIdentity(values), name).toBe(expected);
  }
});

test("resolver marker classifier recognizes logout hrefs and form actions", () => {
  expect(classifyResolverMarkers([{ text: "", label: "", href: "/account/signout" }])).toBe("in");
  expect(classifyResolverMarkers([{ text: "", label: "", formAction: "/log-out" }])).toBe("in");
  expect(
    classifyResolverMarkers([
      { text: "Sign in", label: "", storageIdentity: "in" },
    ]),
  ).toBe("in");
});

test("marker collection scans logout links inside closed and hidden menus", () => {
  const window = new Window({ url: "https://resolver.example.edu/account" });
  window.document.write(
    "<html><body><details><div hidden><span><a href='/logout'>Exit</a></span></div></details></body></html>",
  );
  const previous = {
    document: globalThis.document,
    localStorage: globalThis.localStorage,
    sessionStorage: globalThis.sessionStorage,
  };
  Object.assign(globalThis, {
    document: window.document,
    localStorage: window.localStorage,
    sessionStorage: window.sessionStorage,
  });
  try {
    expect(classifyResolverMarkers(collectResolverMarkers())).toBe("in");
  } finally {
    Object.assign(globalThis, previous);
  }
});

test("Primo-shaped account page with a non-guest session JWT is signed in without sign-out UI", () => {
  const window = new Window({ url: "https://example.primo.exlibrisgroup.com/nde/account/overview" });
  window.document.write(
    "<html><body><main><h1>Jane Doe</h1><details><summary>Account</summary><div hidden>Profile</div></details></main></body></html>",
  );
  window.sessionStorage.setItem(
    "primo-session",
    syntheticJWT({ preferred_username: "jane", userGroup: "STUDENT" }),
  );
  const previous = {
    document: globalThis.document,
    localStorage: globalThis.localStorage,
    sessionStorage: globalThis.sessionStorage,
  };
  Object.assign(globalThis, {
    document: window.document,
    localStorage: window.localStorage,
    sessionStorage: window.sessionStorage,
  });
  try {
    const markers = collectResolverMarkers();
    expect(classifyResolverMarkers(markers)).toBe("in");
    expect(markers.some((marker) => marker.storageIdentity === "in")).toBe(true);
  } finally {
    Object.assign(globalThis, previous);
  }
});

test("marker scan outcome separates markers, no markers, and injection failure", async () => {
  const markers = makeHarness();
  await markers.manager.init();
  await markers.manager.checkNow(100);
  expect(markers.manager.getSnapshot().scanOutcome).toBe("markers");

  const noMarkers = makeHarness();
  noMarkers.resolverMarkers.splice(0, noMarkers.resolverMarkers.length);
  await noMarkers.manager.init();
  await noMarkers.manager.checkNow(100);
  expect(noMarkers.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    scanOutcome: "no_markers",
  });

  const failed = makeHarness();
  failed.api.scripting = {
    executeScript: async () => {
      throw new Error("missing host grant");
    },
  };
  await failed.manager.init();
  await failed.manager.checkNow(100);
  expect(failed.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    scanOutcome: "scan_failed",
  });
});
