// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Deterministic manager tests: all scheduling is a fake one-shot timer, never
// a wall-clock interval or a Chrome API.

import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";

import {
  classifyResolverJWTIdentity,
  classifyResolverMarkers,
  clampKeepaliveInterval,
  collectResolverMarkers,
  isAuthenticationURL,
  KeepaliveManager,
  MAX_OBSERVED_TABS_PER_ORIGIN,
  SESSION_STALE_MS,
  type KeepaliveAPI,
  type KeepaliveOriginSnapshot,
  type KeepaliveProbeSource,
  type KeepaliveTab,
  type KeepaliveTimers,
  type ResolverMarker,
  type SessionVerdict,
} from "../src/keepalive";

const RESOLVER_OPENURL = "https://resolver.example.edu/openurl?genre=article";

// KeepaliveManager has no clock seam of its own — it reads the real
// Date.now() the same way production code does. Every harness patches the
// global once, through this single mechanism, instead of individual tests
// saving/restoring Date.now by hand: makeHarness() owns the override and
// afterEach() always undoes it, so a forgotten restore in one test can never
// leak a frozen clock into the next.
const REAL_DATE_NOW = Date.now.bind(Date);
let restoreDateNow: (() => void) | undefined;
afterEach(() => {
  restoreDateNow?.();
  restoreDateNow = undefined;
});

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

/** Controls chrome.scripting.executeScript per tab: a default resolves
 * immediately from markersByTab/defaultMarkers exactly like before, but
 * hold() lets a test keep one tab's scan open while a sibling's completes,
 * so an intermediate reducer result would be observable if the
 * implementation ever leaked one. injectionCounts lets a test assert a tab
 * was — or was never — inspected at all. */
class FakeScripting {
  readonly injectionCounts = new Map<number, number>();
  private readonly held = new Map<number, ReturnType<typeof Promise.withResolvers<ResolverMarker[]>>>();

  constructor(
    private readonly markersByTab: Map<number, ResolverMarker[]>,
    private readonly defaultMarkers: ResolverMarker[],
  ) {}

  hold(tabId: number): { release: (markers?: ResolverMarker[]) => void } {
    const gate = Promise.withResolvers<ResolverMarker[]>();
    this.held.set(tabId, gate);
    return {
      release: (markers) => {
        gate.resolve(markers ?? this.markersByTab.get(tabId) ?? this.defaultMarkers);
        this.held.delete(tabId);
      },
    };
  }

  executeScript = async (injection?: { target?: { tabId?: number } }): Promise<{ result?: unknown }[]> => {
    const tabId = injection?.target?.tabId ?? -1;
    this.injectionCounts.set(tabId, (this.injectionCounts.get(tabId) ?? 0) + 1);
    const gate = this.held.get(tabId);
    const markers = gate !== undefined ? await gate.promise : this.markersByTab.get(tabId) ?? this.defaultMarkers;
    return [{ result: markers }];
  };
}

interface HarnessResolver {
  latestOpenURL?: string | undefined;
  storedOrigin?: unknown;
  grantedOrigins?: string[];
  knownOrigins?: string[];
  storageValues?: Record<string, unknown>;
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
  scripting: FakeScripting;
  clock: { now: () => number; advanceBy: (ms: number) => void };
  badge: string[];
  reauths: { count: number };
  reauthState: boolean[];
  sessionEvidence: { source: KeepaliveProbeSource; origin: string | undefined }[];
  storageValues: Record<string, unknown>;
  resolverMarkers: ResolverMarker[];
  markersByTab: Map<number, ResolverMarker[]>;
  /** Every "keepalive.originStates" payload ever persisted, in write order —
   * lets a test prove a probe committed a verdict exactly once instead of
   * leaking an intermediate per-tab result. */
  originStateWrites: KeepaliveOriginSnapshot[][];
} {
  const jobs = { count: 1 };
  const tabs = new FakeTabs();
  const timers = new FakeTimers();
  const badge: string[] = [];
  const reauths = { count: 0 };
  const reauthState: boolean[] = [];
  const sessionEvidence: { source: KeepaliveProbeSource; origin: string | undefined }[] = [];
  const originStateWrites: KeepaliveOriginSnapshot[][] = [];
  const storageValues: Record<string, unknown> = {
    "keepalive.interval": interval,
    "keepalive.enabled": true,
    "keepalive.resolverOrigin": resolverConfig?.storedOrigin,
    ...(resolverConfig?.storageValues ?? {}),
  };
  const latestOpenURL = resolverConfig === undefined ? RESOLVER_OPENURL : resolverConfig.latestOpenURL;
  const resolverMarkers: ResolverMarker[] = [{ text: "Sign out", label: "" }];
  /** Per-tab marker overrides: tabs render sessions independently. */
  const markersByTab = new Map<number, ResolverMarker[]>();
  const scripting = new FakeScripting(markersByTab, resolverMarkers);
  const clockState = { now: REAL_DATE_NOW() };
  Date.now = () => clockState.now;
  restoreDateNow = () => {
    Date.now = REAL_DATE_NOW;
  };
  const api: KeepaliveAPI = {
    tabs,
    timers,
    storage: {
      get: async () => ({ ...storageValues }),
      set: async (values) => {
        Object.assign(storageValues, values);
        const states = values["keepalive.originStates"];
        if (Array.isArray(states)) {
          originStateWrites.push(states.map((entry) => ({ ...(entry as KeepaliveOriginSnapshot) })));
        }
      },
    },
    permissions: {
      getAll: async () => ({ origins: resolverConfig?.grantedOrigins ?? [] }),
    },
    scripting: { executeScript: scripting.executeScript },
    action: { setBadgeText: async ({ text }) => void badge.push(text) },
  };
  const manager = new KeepaliveManager(api, {
    trackedJobCount: () => jobs.count,
    latestOpenURL: () => latestOpenURL,
    ...(resolverConfig?.knownOrigins === undefined
      ? {}
      : { knownResolverOrigins: () => resolverConfig.knownOrigins ?? [] }),
    ...(warmDemand !== undefined ? { warmDemand } : {}),
    ...(workWindowID !== undefined ? { workWindowID } : {}),
    onReauthNeeded: () => {
      reauths.count += 1;
    },
    onReauthStateChanged: (paused) => {
      reauthState.push(paused);
    },
    onSessionEvidence: (source, origin) => {
      sessionEvidence.push({ source, origin });
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
    scripting,
    clock: {
      now: () => clockState.now,
      advanceBy: (ms) => {
        clockState.now += ms;
      },
    },
    badge,
    reauths,
    reauthState,
    sessionEvidence,
    storageValues,
    resolverMarkers,
    markersByTab,
    originStateWrites,
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

/** Drains the microtask queue without a real timer or macrotask: nothing
 * else in these tests is pending, so repeated `await Promise.resolve()`
 * ticks are sufficient to run every chain forward to whatever deferred
 * promise it is actually blocked on. Extra rounds beyond what a chain
 * needs are harmless no-ops — they can never unblock a promise this file
 * hasn't resolved yet. */
async function flushMicrotasks(rounds = 25): Promise<void> {
  for (let i = 0; i < rounds; i++) await Promise.resolve();
}

/** Distinct verdict values actually WRITTEN for `origin`, in commit order,
 * starting from `initial`. A correct atomic reducer commits at most once
 * per probe; the bug this batch fixes was each inspected tab calling
 * setVerdict() as soon as it resolved, so an intermediate wrong verdict
 * could be persisted — and observed — before the reducer finished. */
function verdictCommits(
  writes: readonly KeepaliveOriginSnapshot[][],
  origin: string,
  initial: SessionVerdict,
): SessionVerdict[] {
  const commits: SessionVerdict[] = [];
  let prev: SessionVerdict = initial;
  for (const snapshot of writes) {
    const row = snapshot.find((entry) => entry.origin === origin);
    if (row === undefined || row.verdict === prev) continue;
    commits.push(row.verdict);
    prev = row.verdict;
  }
  return commits;
}


test("papio-8f79b6ba67bdbdaa: concurrent creation attempts across sync() calls produce exactly one tab", async () => {
  // sync() is now invoked from every successful triage-counts response, in
  // addition to the existing timer-driven onReload/onObserve callbacks, so
  // two callers can both observe this.tabID === undefined and race into
  // createTab() across its awaited tabs.query()/tabs.create() calls. A.query
  // -> B.query -> A.create -> B.create used to leave two live tabs with the
  // later assignment overwriting this.tabID — orphaning the first, pinned
  // tab forever, since the tab governor deliberately skips pinned tabs.
  const h = makeHarness();
  let queryCalls = 0;
  let createCalls = 0;
  const query = Promise.withResolvers<KeepaliveTab[]>();
  const create = Promise.withResolvers<KeepaliveTab>();
  h.tabs.query = async (_query: {
    pinned?: boolean;
    muted?: boolean;
    url?: string[];
    active?: boolean;
    lastFocusedWindow?: boolean;
  }): Promise<KeepaliveTab[]> => {
    queryCalls += 1;
    return query.promise;
  };
  h.tabs.create = async (properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
  }): Promise<KeepaliveTab> => {
    createCalls += 1;
    h.tabs.created.push(properties);
    return create.promise;
  };

  // One caller mirrors the timer-driven start; the other mirrors an inbound
  // triage-counts frame arriving mid-cycle. Neither is awaited individually
  // so both chains are free to interleave.
  const first = h.manager.init();
  const second = h.manager.sync();
  await flushMicrotasks();

  // Both chains have run forward to whatever they are blocked on. Only the
  // FIRST to reach createTab() ever calls tabs.query(); the second sees the
  // shared in-flight promise and never repeats the query or the create.
  expect(queryCalls).toBe(1);
  expect(createCalls).toBe(0);

  query.resolve([]);
  await flushMicrotasks();
  expect(createCalls).toBe(1);

  create.resolve({ id: 1, url: "https://resolver.example.edu" });
  await first;
  await second;

  expect(queryCalls).toBe(1);
  expect(createCalls).toBe(1);
  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true },
  ]);

  // Same-origin callers only ever share one creation — nothing here should
  // ever reach removeStaleTab's teardown path.
  expect(h.tabs.removed).toEqual([]);

  // The single created tab is the one the manager actually owns: it is
  // reloaded (not recreated) on the next cycle and removed once demand ends.
  h.jobs.count = 0;
  await h.manager.sync();
  expect(h.tabs.removed).toEqual([1]);
});

test("openReauth requesting a different origin mid-creation never rides another institution's tab", async () => {
  // The concurrency test above shares one resolver for every caller, so it
  // never exercises openReauth switching institutions. openReauth exists
  // specifically to do that, and unlike reconcile/onObserve/onReload it can
  // fire while this.tabID is still undefined (the very race window the
  // shared in-flight promise targets) — so its own "close the stale tab"
  // teardown (gated on this.tabID !== undefined) never runs, and riding the
  // in-flight promise used to hand it a tab for the WRONG institution.
  const h = makeHarness();
  const otherOrigin = "https://otherlib.example.edu";

  interface QueryCall {
    url: string[] | undefined;
    resolve: (tabs: KeepaliveTab[]) => void;
  }
  interface CreateCall {
    url: string;
    resolve: (tab: KeepaliveTab) => void;
  }
  const queries: QueryCall[] = [];
  const creates: CreateCall[] = [];
  h.tabs.query = (query: { url?: string[] }): Promise<KeepaliveTab[]> => {
    const { promise, resolve } = Promise.withResolvers<KeepaliveTab[]>();
    queries.push({ url: query.url, resolve });
    return promise;
  };
  h.tabs.create = (properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
  }): Promise<KeepaliveTab> => {
    h.tabs.created.push(properties);
    const { promise, resolve } = Promise.withResolvers<KeepaliveTab>();
    creates.push({ url: properties.url, resolve });
    return promise.then((tab) => {
      if (tab.id !== undefined) h.tabs.live.set(tab.id, tab);
      return tab;
    });
  };

  // Caller A: the ordinary timer-driven path claims the configured
  // resolver origin and blocks on its query.
  const first = h.manager.init();
  await flushMicrotasks();
  expect(queries).toHaveLength(1);
  expect(queries[0]?.url).toEqual(["https://resolver.example.edu/*"]);
  expect(creates).toHaveLength(0);

  // Caller B: the operator explicitly asks to reauthenticate a DIFFERENT
  // institution while A's creation is still in flight and this.tabID is
  // still undefined — exactly the window the old dedupe mishandled.
  const second = h.manager.openReauth(otherOrigin);
  await flushMicrotasks();
  // B must not join A's in-flight attempt: no second query yet, and
  // definitely nothing created for either origin so far.
  expect(queries).toHaveLength(1);
  expect(creates).toHaveLength(0);

  // A's query settles: no existing resolver.example.edu tab, so A creates one.
  queries[0]?.resolve([]);
  await flushMicrotasks();
  expect(creates).toHaveLength(1);
  expect(creates[0]?.url).toBe("https://resolver.example.edu");

  // A's create settles. Only now can B stop waiting and drive its OWN,
  // origin-correct creation — never reusing A's query result or tab.
  creates[0]?.resolve({ id: 1, url: "https://resolver.example.edu" });
  await flushMicrotasks();
  expect(queries).toHaveLength(2);
  expect(queries[1]?.url).toEqual(["https://otherlib.example.edu/*"]);
  // The FIRST attempt's tab (id 1, resolver.example.edu) is about to be
  // orphaned: this.tabID is about to be overwritten by B's own creation
  // below, and the tab governor deliberately skips pinned tabs, so nothing
  // else would ever close it. B must remove it before starting its own
  // creation, and it must do so before issuing its own query above.
  expect(h.tabs.removed).toEqual([1]);
  expect(creates).toHaveLength(1);

  queries[1]?.resolve([]);
  await flushMicrotasks();
  expect(creates).toHaveLength(2);
  // The tab created for B's origin must be requested under B's origin, never
  // A's — this is the exact hijack the bug allowed.
  expect(creates[1]?.url).toBe("https://otherlib.example.edu");

  creates[1]?.resolve({ id: 2, url: "https://otherlib.example.edu" });
  await first;
  await second;

  // The operator's explicit reauth request lands on ITS OWN tab (id 2, the
  // one created for otherlib.example.edu), never on A's resolver.example.edu
  // tab (id 1): pauseForReauth() acts on this.tabID, so an update targeting
  // tab 1 here would mean the operator was handed the wrong institution.
  expect(h.tabs.updates.some((u) => u.id === 1)).toBe(false);
  expect(h.tabs.updates.some((u) => u.id === 2 && u.properties.active === true)).toBe(true);
  expect(h.manager.getSnapshot().resolverOrigin).toBe(otherOrigin);
  // Exactly the orphan is gone: B's own tab was never also swept up.
  expect(h.tabs.removed).toEqual([1]);
});

test("papio-fd8a4fcae897e58d: concurrent openReauth calls for different origins both report honestly", async () => {
  // The test above drives caller A through init() — the timer path, whose
  // boolean nobody reads. The popup renders one Sign-in button per
  // institution row and disables only the clicked one, so the real operator
  // race is TWO openReauth calls, and openReauth's return value decides
  // whether background.ts falls through to an unmanaged session-signin tab.
  //
  // Unserialised, B's wait-then-retry continuation ran before A's openReauth
  // continuation and cleared this.tabID (removeStaleTab is synchronous up to
  // its tabs.remove), so A returned false for a tab its own attempt had just
  // created. Serialising openReauth makes A finish — create, focus, return
  // true — before B supersedes it.
  const h = makeHarness();
  const originA = "https://a.example.edu";
  const originB = "https://b.example.edu";

  const [okA, okB] = await Promise.all([
    h.manager.openReauth(originA),
    h.manager.openReauth(originB),
  ]);

  // A genuinely opened its institution's tab; saying otherwise sends its
  // caller down the unmanaged-tab fallback for a tab that already existed.
  expect(okA).toBe(true);
  expect(okB).toBe(true);
  expect(h.tabs.created.map((tab) => tab.url)).toEqual([originA, originB]);
  // A focused its own tab before B superseded it, and B focused only its own.
  expect(h.tabs.updates.map((update) => update.id)).toEqual([1, 2]);
  // Exactly one tab survives, for the origin that won.
  expect(h.tabs.removed).toEqual([1]);
  expect([...h.tabs.live.keys()]).toEqual([2]);
  expect(h.manager.getSnapshot().resolverOrigin).toBe(originB);
});

test("papio-8f79b6ba67bdbdaa: a failed tab creation does not wedge later creation attempts", async () => {
  const h = makeHarness();
  let createAttempts = 0;
  const realCreate = h.tabs.create.bind(h.tabs);
  h.tabs.create = async (properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
  }): Promise<KeepaliveTab> => {
    createAttempts += 1;
    // Simulate browser policy rejecting the very first background-tab
    // creation (the failure mode createTabOnce's own catch comment names).
    if (createAttempts === 1) throw new Error("background tab creation blocked by browser policy");
    return realCreate(properties);
  };

  await h.manager.init();
  // createTabOnce() swallows the rejection, so no tab exists yet — but the
  // shared in-flight promise from that failed cycle MUST have been cleared
  // by createTab()'s `.finally()`. If it were only cleared on success, this
  // stale never-resolving-to-a-tab promise would wedge every later caller
  // that checks this.tabID === undefined, permanently starving the
  // resolver of a keepalive tab.
  expect(h.tabs.created.length).toBe(0);
  expect(createAttempts).toBe(1);

  // reconcile() schedules onReload after createTab() regardless of outcome;
  // running it retries creation on a manager that must not be wedged.
  await h.timers.runNext();

  expect(createAttempts).toBe(2);
  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: false, pinned: true, muted: true },
  ]);

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

test("probeForeground always runs a real scan, even moments after the previous one completed", async () => {
  // probeForeground() replaces checkNow(): SESSION_STALE_MS is a
  // display-trust budget for the popup only, never a gate on whether a
  // probe actually runs.
  const h = makeHarness();
  await h.manager.init();

  await h.manager.probeForeground();
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in", lastProbeOutcome: "markers" });
  const injectedAfterFirst = h.scripting.injectionCounts.get(1) ?? 0;
  expect(injectedAfterFirst).toBeGreaterThan(0);
  const queriesAfterFirst = h.tabs.queryCount;

  // Well within SESSION_STALE_MS — the deleted freshness gate would have
  // skipped this call entirely.
  h.clock.advanceBy(1_000);
  expect(h.clock.now()).toBeLessThan((h.manager.getSnapshot().lastVerdictAt ?? 0) + SESSION_STALE_MS);
  await h.manager.probeForeground();

  expect(h.tabs.queryCount).toBeGreaterThan(queriesAfterFirst);
  expect(h.scripting.injectionCounts.get(1) ?? 0).toBeGreaterThan(injectedAfterFirst);
  expect(h.manager.getSnapshot().lastProbeAt).toBe(h.clock.now());
});

test("getSnapshot() and getOriginSnapshots() are pure reads that never probe", async () => {
  const h = makeHarness();
  await h.manager.init();
  await h.manager.probeForeground();
  const injectionsAfterProbe = [...h.scripting.injectionCounts.values()].reduce((a, b) => a + b, 0);
  const queriesAfterProbe = h.tabs.queryCount;

  for (let i = 0; i < 5; i++) {
    h.manager.getSnapshot();
    h.manager.getOriginSnapshots();
  }

  const injectionsAfterReads = [...h.scripting.injectionCounts.values()].reduce((a, b) => a + b, 0);
  expect(injectionsAfterReads).toBe(injectionsAfterProbe);
  expect(h.tabs.queryCount).toBe(queriesAfterProbe);
});

test("a live resolver tab is evidence while its probe determines the verdict", async () => {
  const h = makeHarness();
  await h.manager.init();
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(liveTab.id, liveTab);
  h.tabs.resolverTabs.push(liveTab);
  const gate = h.scripting.hold(liveTab.id);
  const pending = h.manager.probeForeground();
  await flushMicrotasks();
  const during = h.manager.getSnapshot();
  expect(during.checking).toBe(true);
  // The fake browser resolves once released, so the completed state is the
  // authoritative resolver-origin verdict rather than the interim evidence.
  gate.release();
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

test("a lone 'My account' marker is not sign-in evidence", () => {
  // "My account" is present on plenty of signed-out landing pages too, so on
  // its own it can no longer assert "in" — only an explicit sign-out
  // affordance (or a qualifying JWT identity) counts.
  expect(classifyResolverMarkers([{ text: "My account", label: "" }])).toBe("unknown");
});

test("resolver-origin marker verdicts are evidence-based and do not pause a live user tab", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  // Closes the owned keepalive tab: the manager's own tab is always a probe
  // candidate for its origin, so leaving it live would let its default
  // "Sign out" marker join this origin's decisive readings and mask what
  // this test is actually pinning — a single non-owned tab's evidence.
  await h.manager.sync();
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign in", label: "" });
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(liveTab.id, liveTab);
  h.tabs.resolverTabs.push(liveTab);

  await h.manager.probeForeground();

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

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: false,
    verdict: "unknown",
    probeSource: "none",
    lastProbeOutcome: "no_tab",
  });
  // A no_tab outcome is not itself an inspection: it must never manufacture
  // a fake completed-verdict timestamp the popup could mistake for evidence.
  expect(h.manager.getSnapshot().lastVerdictAt).toBeNull();
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

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
  });
});

test("a preferred/triggering tab decides the verdict despite three stale tabs sorting first in query order", async () => {
  // The manager's own resolver tab is always priority-ordered ahead of
  // plain query-order siblings (probeOrigin: preferred, then focused, then
  // owned, then the rest) — so during the reload cycle it is the causal
  // observation regardless of where tabs.query() happens to place it.
  const h = makeHarness();
  await h.manager.init(); // creates the owned tab, id 1
  h.markersByTab.set(1, [{ text: "Sign out", label: "" }]); // owned tab: decisive "in"

  // Three stale siblings sort BEFORE the owned tab in tabs.query() results
  // and each disagree with it — under the old liveTabs.slice(0, 3) cap the
  // owned tab would never even have been observed.
  const stale1 = { id: 90, url: "https://resolver.example.edu/discovery/a" };
  const stale2 = { id: 91, url: "https://resolver.example.edu/discovery/b" };
  const stale3 = { id: 92, url: "https://resolver.example.edu/discovery/c" };
  h.tabs.live.set(90, stale1);
  h.tabs.live.set(91, stale2);
  h.tabs.live.set(92, stale3);
  h.tabs.resolverTabs.push(stale1, stale2, stale3);
  h.markersByTab.set(90, [{ text: "Sign in", label: "" }]);
  h.markersByTab.set(91, [{ text: "Sign in", label: "" }]);
  h.markersByTab.set(92, [{ text: "Sign in", label: "" }]);

  const origin = "https://resolver.example.edu";
  h.originStateWrites.length = 0; // Scope the write log to this reload cycle only.
  await h.timers.runNext(); // reload
  await h.timers.runNext(); // inspectAfterReload -> probeOwnedTab("cycle")

  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in", probeSource: "keepalive_tab" });
  // Exactly one commit: the three disagreeing siblings never got to author
  // an intermediate or final "out"/"unknown" verdict of their own.
  expect(verdictCommits(h.originStateWrites, origin, "unknown")).toEqual(["in"]);
});

test("more matching tabs than the observation cap never commits a decisive verdict from siblings alone", async () => {
  const h = makeHarness();
  await h.manager.init();
  // Earn a warm "in" verdict first, from the owned tab alone.
  await h.manager.probeForeground();
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in", authenticated: true });
  const earnedAt = h.manager.getSnapshot().lastVerdictAt;

  // Now flood the origin with more matching tabs than the cap, none of
  // them focused or owned, every one reading "out".
  expect(MAX_OBSERVED_TABS_PER_ORIGIN).toBe(5);
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign in", label: "" });
  const flood: KeepaliveTab[] = [];
  for (let id = 200; id < 200 + MAX_OBSERVED_TABS_PER_ORIGIN + 1; id++) {
    const tab = { id, url: `https://resolver.example.edu/discovery/${id}` };
    h.tabs.live.set(id, tab);
    flood.push(tab);
  }
  h.tabs.resolverTabs.push(...flood);

  h.clock.advanceBy(1_000);
  await h.manager.probeForeground();

  const origin = "https://resolver.example.edu";
  const snapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(snapshot).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "partial_scan",
  });
  expect(snapshot?.lastVerdictAt).toBe(earnedAt);
  expect(snapshot?.lastProbeAt).not.toBe(earnedAt);
});

test("a decisive causal out from the focused tab is not overruled by a stale sibling reading in", async () => {
  const h = makeHarness();
  await h.manager.init();
  // The focused tab is the causal observation and reads a decisive "out";
  // cookies are browser-global, so an "in"-reading sibling is real
  // evidence too — but per-tab caches disagree, and the operator is
  // looking at the "out" page right now.
  const focused = { id: 42, url: "https://resolver.example.edu/discovery/search" };
  const sibling = { id: 43, url: "https://resolver.example.edu/account/overview" };
  h.tabs.live.set(42, focused);
  h.tabs.live.set(43, sibling);
  h.tabs.focusedTab = focused;
  h.tabs.resolverTabs.push(focused, sibling);
  h.markersByTab.set(42, [{ text: "Sign in", label: "" }]);
  h.markersByTab.set(43, [{ text: "Sign out", label: "" }]);

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "out",
    authenticated: false,
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
  });
});

test("decisive in and out with no causal preference commits unknown as a conflict", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync(); // Close the owned tab: it must not act as a tiebreaker here.

  // Neither tab is focused and neither is preferred, so there is no causal
  // observation to arbitrate between two tabs that disagree decisively.
  const a = { id: 51, url: "https://resolver.example.edu/discovery/a" };
  const b = { id: 52, url: "https://resolver.example.edu/discovery/b" };
  h.tabs.live.set(51, a);
  h.tabs.live.set(52, b);
  h.tabs.resolverTabs.push(a, b);
  h.markersByTab.set(51, [{ text: "Sign in", label: "" }]);
  h.markersByTab.set(52, [{ text: "Sign out", label: "" }]);

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    authenticated: false,
    lastProbeOutcome: "conflict",
  });
});

test("a sibling scan held open cannot publish an intermediate verdict", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  const a = { id: 61, url: "https://resolver.example.edu/discovery/a" };
  const b = { id: 62, url: "https://resolver.example.edu/discovery/b" };
  h.tabs.live.set(61, a);
  h.tabs.live.set(62, b);
  h.tabs.resolverTabs.push(a, b);
  h.markersByTab.set(61, [{ text: "Sign in", label: "" }]); // resolves immediately: "out"
  h.markersByTab.set(62, [{ text: "Sign out", label: "" }]); // held open: would-be "in"
  const gate = h.scripting.hold(62);

  const origin = "https://resolver.example.edu";
  const pending = h.manager.probeForeground();
  await flushMicrotasks();

  // Tab A has already resolved to "out", but the reducer cannot commit
  // anything until tab B's observation is in too — otherwise it cannot
  // tell a genuine single-polarity commit from a conflict it hasn't
  // finished discovering yet.
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "unknown", authenticated: false });
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "unknown",
    authenticated: false,
  });
  expect(h.sessionEvidence).toEqual([]);

  gate.release();
  await pending;

  // Both decisive and disagreeing, with no causal tab: conflict, not a
  // leaked "out".
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "unknown", lastProbeOutcome: "conflict" });
  expect(h.sessionEvidence).toEqual([]);
});

test("scan_failed preserves a previously earned in verdict", async () => {
  const h = makeHarness();
  await h.manager.init();
  await h.manager.probeForeground();
  const earned = h.manager.getSnapshot();
  expect(earned).toMatchObject({ verdict: "in", authenticated: true });

  h.api.scripting = {
    executeScript: async () => {
      throw new Error("host permission revoked");
    },
  };
  h.clock.advanceBy(1_000);
  await h.manager.probeForeground();

  const after = h.manager.getSnapshot();
  expect(after).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "scan_failed",
  });
  expect(after.lastVerdictAt).toBe(earned.lastVerdictAt);
  expect(after.lastProbeAt).not.toBe(earned.lastProbeAt);
});

test("an owned tab parked on an authentication URL still pauses for reauth even though a live user tab reads in", async () => {
  // Owned-tab disposition (pause/resume) is computed from the owned tab's
  // OWN observation only — never from the committed origin verdict, which
  // may come from an entirely different, non-owned tab.
  const h = makeHarness();
  await h.manager.init();
  h.tabs.live.get(1)!.url = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver";

  const sibling = { id: 88, url: "https://resolver.example.edu/account/overview" };
  h.tabs.live.set(88, sibling);
  h.tabs.resolverTabs.push(sibling); // Default markers: "Sign out" -> decisive "in".

  await h.manager.probeForeground();

  const snapshot = h.manager.getSnapshot();
  expect(snapshot.pausedForReauth).toBe(true);
  expect(snapshot.verdict).toBe("in");
  expect(h.reauthState).toEqual([true]);
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

test("probe outcome separates markers, no markers, and injection failure", async () => {
  const markers = makeHarness();
  await markers.manager.init();
  await markers.manager.probeForeground();
  expect(markers.manager.getSnapshot().lastProbeOutcome).toBe("markers");

  const noMarkers = makeHarness();
  noMarkers.resolverMarkers.splice(0, noMarkers.resolverMarkers.length);
  await noMarkers.manager.init();
  await noMarkers.manager.probeForeground();
  expect(noMarkers.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    lastProbeOutcome: "no_markers",
  });

  const failed = makeHarness();
  failed.api.scripting = {
    executeScript: async () => {
      throw new Error("missing host grant");
    },
  };
  await failed.manager.init();
  await failed.manager.probeForeground();
  expect(failed.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    lastProbeOutcome: "scan_failed",
  });
});
test("auth-shaped URL evidence stays unknown when marker inspection is empty", async () => {
  const h = makeHarness();
  h.resolverMarkers.splice(0, h.resolverMarkers.length);
  await h.manager.init();
  const liveTab = { id: 91, url: "https://resolver.example.edu/auth/login" };
  h.tabs.live.set(liveTab.id, liveTab);
  h.tabs.resolverTabs.push(liveTab);

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    authenticated: false,
    lastProbeOutcome: "no_markers",
  });
});

test("an IdP URL surfaces reauthentication without asserting signed out", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.tabs.nextURL = "https://idp.example.edu/sso/login";

  await h.timers.runNext();
  await h.timers.runNext();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    authenticated: false,
    pausedForReauth: true,
  });
});

test("JWT identity requires an unexpired exp claim", () => {
  expect(
    classifyResolverJWTIdentity([
      syntheticJWT({ preferred_username: "jane", exp: Math.floor(Date.now() / 1_000) - 1 }),
    ]),
  ).toBe("unknown");
  expect(
    classifyResolverJWTIdentity([
      syntheticJWT({ preferred_username: "jane", exp: Math.floor(Date.now() / 1_000) + 60 }),
    ]),
  ).toBe("in");
});

test("injected marker collection rejects an expired JWT identity", () => {
  const window = new Window({ url: "https://resolver.example.edu/account" });
  window.sessionStorage.setItem(
    "primo-session",
    syntheticJWT({ preferred_username: "jane", exp: Math.floor(Date.now() / 1_000) - 1 }),
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
    expect(classifyResolverMarkers(markers)).toBe("unknown");
    expect(markers.some((marker) => marker.storageIdentity === "in")).toBe(false);
  } finally {
    Object.assign(globalThis, previous);
  }
});

test("marker collection ignores logout text in scripts, styles, templates, and ancestors", () => {
  const window = new Window({ url: "https://resolver.example.edu/account" });
  window.document.write(
    "<html><head><style>.logout { content: 'logout'; }</style></head><body><script>const label = 'logout';</script><template>logout</template><main>logout</main></body></html>",
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
    expect(classifyResolverMarkers(collectResolverMarkers())).toBe("unknown");
  } finally {
    Object.assign(globalThis, previous);
  }
});

test("popup check probes a live tab for a second known resolver origin", async () => {
  const defaultOrigin = "https://resolver.example.edu";
  const uwaOrigin = "https://onesearch.library.example-college.edu";
  const h = makeHarness(4, undefined, {
    latestOpenURL: `${defaultOrigin}/openurl`,
    knownOrigins: [defaultOrigin, uwaOrigin],
  });
  await h.manager.init();
  const uwaTab = { id: 43, url: `${uwaOrigin}/account` };
  h.tabs.live.set(uwaTab.id, uwaTab);
  h.tabs.resolverTabs.push(uwaTab);
  const inspected: number[] = [];
  h.api.scripting = {
    executeScript: async ({ target }) => {
      inspected.push(target.tabId);
      return [{
        result: target.tabId === uwaTab.id
          ? [{ text: "Sign out", label: "" }]
          : [],
      }];
    },
  };

  await h.manager.probeForeground();

  expect(inspected).toContain(uwaTab.id);
  expect(h.manager.getOriginSnapshots().find((snapshot) => snapshot.origin === uwaOrigin)).toMatchObject({
    verdict: "in",
    authenticated: true,
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
  });
});

test("granted provider permission patterns never mint institution rows", async () => {
  const defaultOrigin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, {
    latestOpenURL: `${defaultOrigin}/openurl`,
    knownOrigins: [defaultOrigin, "https://onesearch.library.example-college.edu"],
    grantedOrigins: [
      "https://*.academic.oup.com/*",
      "https://*.cambridge.org/*",
      "https://www.jstor.org/*",
      `${defaultOrigin}/*`,
    ],
  });
  await h.manager.init();

  const origins = h.manager.getOriginSnapshots().map((snapshot) => snapshot.origin);
  expect(origins.sort()).toEqual([
    "https://onesearch.library.example-college.edu",
    "https://resolver.example.edu",
  ]);
});

test("hidden sign-in markers never assert signed out; visible ones do", () => {
  // Primo NDE keeps a sign-in href in a closed drawer permanently — signed in
  // or not. A hidden prompt is not evidence of a signed-out session.
  expect(
    classifyResolverMarkers([
      { text: "Sign in", label: "", href: "/nde/login", visible: false },
    ]),
  ).toBe("unknown");
  // A signed-out page puts the prompt front and center.
  expect(
    classifyResolverMarkers([
      { text: "Sign in", label: "", href: "/nde/login", visible: true },
    ]),
  ).toBe("out");
  // Markers predating the visibility flag keep their old meaning.
  expect(classifyResolverMarkers([{ text: "Sign in", label: "" }])).toBe("out");
  // Sign-out affordances count from inside closed menus, hidden or not.
  expect(
    classifyResolverMarkers([
      { text: "Sign out", label: "", href: "/logout", visible: false },
      { text: "Sign in", label: "", href: "/nde/login", visible: true },
    ]),
  ).toBe("in");
});

test("per-origin verdicts survive a service-worker restart", async () => {
  const origin = "https://onesearch.library.example-college.edu";
  const stored: Record<string, unknown> = {
    "keepalive.originStates": [
      {
        origin,
        authenticated: true,
        verdict: "in",
        probeSource: "live_tab",
        lastProbeOutcome: "markers",
        lastVerdictAt: Date.now() - 60_000,
        checking: true,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: Date.now() - 60_000,
      },
    ],
  };
  const h = makeHarness(4, undefined, {
    knownOrigins: [origin],
    storageValues: stored,
  });
  await h.manager.init();
  const snapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(snapshot?.verdict).toBe("in");
  expect(snapshot?.authenticated).toBe(true);
  // Restored evidence is settled state, never a stuck in-flight probe.
  expect(snapshot?.checking).toBe(false);
});

test("all tabs signed out keeps the focused tab's verdict authoritative", async () => {
  const h = makeHarness();
  await h.manager.init();
  const first = { id: 42, url: "https://resolver.example.edu/discovery/search" };
  const second = { id: 43, url: "https://resolver.example.edu/discovery/dbsearch" };
  h.tabs.live.set(42, first);
  h.tabs.live.set(43, second);
  h.tabs.focusedTab = first;
  h.tabs.resolverTabs.push(first, second);
  h.markersByTab.set(42, [{ text: "Sign in", label: "", href: "/nde/login", visible: true }]);
  h.markersByTab.set(43, []);

  await h.manager.probeForeground();
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "out", probeSource: "live_tab" });
});

test("closing the library tab never erases an earned warm verdict, but the next probe still runs", async () => {
  const h = makeHarness();
  await h.manager.init();
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.live.set(42, liveTab);
  h.tabs.resolverTabs.push(liveTab);
  await h.manager.probeForeground();
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in" });
  const queriesAfterFirst = h.tabs.queryCount;

  // Every resolver tab closes — including papio's own pinned keepalive tab,
  // which is a probe candidate in its own right (preferred -> focused ->
  // owned -> rest). Leaving it live would exercise the "keepalive_tab still
  // answers" path instead of the no-tab path this test exists for.
  //
  // Unlike checkNow(), probeForeground() has no freshness gate, so the second
  // call genuinely queries again rather than being swallowed — the old test
  // here never actually reached the no-tab branch.
  for (const id of [...h.tabs.live.keys()]) h.tabs.live.delete(id);
  h.tabs.resolverTabs.length = 0;
  await h.manager.probeForeground();

  expect(h.tabs.queryCount).toBeGreaterThan(queriesAfterFirst);
  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "in",
    lastProbeOutcome: "no_tab",
  });
});