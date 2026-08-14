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
  MAX_AFFORDANCE_LENGTH,
  MAX_CONTROL_TEXT_DEPTH,
  MAX_MARKER_TEXT_LENGTH,
  MAX_OBSERVED_TABS_PER_ORIGIN,
  MAX_SCANNED_CONTROLS,
  MIN_FOREGROUND_PROBE_SPACING_MS,
  MIN_PROBE_START_SPACING_MS,
  SESSION_STALE_MS,
  type FreshSessionEvidence,
  type KeepaliveAPI,
  type KeepaliveOriginSnapshot,
  type KeepaliveTab,
  type KeepaliveTimers,
  type ResolverMarker,
  type SessionVerdict,
} from "../src/keepalive";
import { ChromeTabsFake } from "./fake-tabs";

const withResolverDom = <T,>(html: string, run: () => T, url = "https://resolver.example.edu/discovery/search"): T => {
  const window = new Window({ url });
  window.document.write(html);
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
    return run();
  } finally {
    Object.assign(globalThis, previous);
  }
};

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
    { callback: () => void | Promise<void>; delayMs: number; dueAt: number }
  >();
  readonly delays: number[] = [];

  /** `now` is the SAME shared clock the harness hands out as `clock.now` —
   * Commit B schedules three independent timer kinds (cycle/reauth/settle)
   * that can be pending simultaneously, so a test drives `clock.advanceBy()`
   * exactly the way production computes delays and lets `runDue()` fire
   * whatever has actually elapsed, instead of guessing which of several
   * pending delay values belongs to which kind. */
  constructor(private readonly now: () => number = () => Date.now()) {}

  setTimeout(callback: () => void | Promise<void>, delayMs: number): number {
    const id = this.nextID++;
    this.pending.set(id, { callback, delayMs, dueAt: this.now() + delayMs });
    this.delays.push(delayMs);
    return id;
  }

  clearTimeout(handle: unknown): void {
    if (typeof handle === "number") this.pending.delete(handle);
  }

  async runNext(): Promise<void> {
    const entry = this.pending.entries().next().value as
      | [number, { callback: () => void | Promise<void>; delayMs: number; dueAt: number }]
      | undefined;
    if (entry === undefined) throw new Error("no scheduled timer");
    this.pending.delete(entry[0]);
    await entry[1].callback();
  }

  /** Run every pending timer whose computed fire time has elapsed at the
   * clock's current position, due-time-then-insertion order, looping so a
   * callback that reschedules another already-due timer is also drained.
   * This is what lets a test simulate real elapsed time across several
   * concurrently pending timer kinds without knowing their exact delays. */
  async runDue(maxRounds = 50): Promise<void> {
    for (let round = 0; round < maxRounds; round++) {
      const due = [...this.pending.entries()]
        .filter(([, entry]) => entry.dueAt <= this.now())
        .sort((a, b) => a[1].dueAt - b[1].dueAt || a[0] - b[0]);
      if (due.length === 0) return;
      for (const [id, entry] of due) {
        if (!this.pending.has(id)) continue; // cleared earlier this round
        this.pending.delete(id);
        await entry.callback();
      }
    }
    throw new Error("runDue exceeded maxRounds — possible timer rescheduling loop");
  }

  /** Run the oldest pending timer scheduled with exactly `delayMs`, so a
   * test can pick out one fixed-delay kind (e.g. the `reloadSettleMs`
   * settle timer) from several concurrently pending kinds without
   * advancing the clock. */
  async runByDelay(delayMs: number): Promise<void> {
    const entry = [...this.pending.entries()].find(([, e]) => e.delayMs === delayMs);
    if (entry === undefined) {
      throw new Error(`no pending timer with delay ${delayMs}; pending: ${this.pendingDelays().join(", ")}`);
    }
    this.pending.delete(entry[0]);
    await entry[1].callback();
  }

  /** Delays of every CURRENTLY pending timer — unlike `delays` (a
   * cumulative log), this reflects only what has not yet fired or been
   * cleared. */
  pendingDelays(): number[] {
    return [...this.pending.values()].map((entry) => entry.delayMs);
  }

  pendingCount(): number {
    return this.pending.size;
  }

  latestDelay(): number | undefined {
    return this.delays.at(-1);
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

/** Controls persistence of `keepalive.originStates`: by default every
 * `storage.set()` call resolves immediately, exactly like a real browser
 * write. `arm()` queues up holds for the next N calls so a test can freeze
 * an in-flight write and observe whether the manager attempts a SECOND one
 * before the first settles — proving the manager's own serialized persist
 * chain (never a lucky ordering in this fake) is what keeps a newer write
 * from ever being clobbered by an older one settling late. */
class StorageWriteGate {
  private armed = 0;
  private readonly held: { values: Record<string, unknown>; release: () => void }[] = [];

  arm(count = 1): void {
    this.armed += count;
  }

  async gate(values: Record<string, unknown>): Promise<void> {
    if (this.armed <= 0) return;
    this.armed -= 1;
    const resolvers = Promise.withResolvers<void>();
    this.held.push({ values, release: () => resolvers.resolve() });
    await resolvers.promise;
  }

  /** Release the oldest still-held write, letting its `storage.set()` call
   * resolve and apply. */
  releaseOldest(): void {
    this.held.shift()?.release();
  }

  get pendingCount(): number {
    return this.held.length;
  }
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
  tabs: ChromeTabsFake;
  timers: FakeTimers;
  scripting: FakeScripting;
  clock: { now: () => number; advanceBy: (ms: number) => void };
  badge: string[];
  reauths: { count: number };
  reauthState: boolean[];
  freshEvidence: FreshSessionEvidence[];
  authChanges: { origin: string; authenticated: boolean }[];
  /** Mutable "hello_ack landed" toggle read live by configuredOriginsReady():
   * defaults to false, matching production before the daemon handshake, so
   * every existing test's behavior is unchanged unless a test opts in. */
  configuredReady: { value: boolean };
  storageValues: Record<string, unknown>;
  resolverMarkers: ResolverMarker[];
  markersByTab: Map<number, ResolverMarker[]>;
  /** Every "keepalive.originStates" payload ever persisted, in write order —
   * lets a test prove a probe committed a verdict exactly once instead of
   * leaking an intermediate per-tab result. */
  originStateWrites: KeepaliveOriginSnapshot[][];
  /** Lets a test freeze one `keepalive.originStates` write mid-flight and
   * prove the manager never issues a second one until it settles. */
  storageGate: StorageWriteGate;
} {
  const jobs = { count: 1 };
  const tabs = new ChromeTabsFake();
  tabs.nextId = 1;
  const clockState = { now: REAL_DATE_NOW() };
  Date.now = () => clockState.now;
  restoreDateNow = () => {
    Date.now = REAL_DATE_NOW;
  };
  const timers = new FakeTimers(() => clockState.now);
  const storageGate = new StorageWriteGate();
  const badge: string[] = [];
  const reauths = { count: 0 };
  const reauthState: boolean[] = [];
  const freshEvidence: FreshSessionEvidence[] = [];
  const authChanges: { origin: string; authenticated: boolean }[] = [];
  const configuredReady = { value: false };
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
  const api: KeepaliveAPI = {
    tabs,
    timers,
    storage: {
      get: async () => ({ ...storageValues }),
      set: async (values) => {
        await storageGate.gate(values);
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
    configuredOriginsReady: () => configuredReady.value,
    onFreshSessionEvidence: (evidence) => {
      freshEvidence.push(evidence);
    },
    onOriginAuthenticationChanged: (origin, authenticated) => {
      authChanges.push({ origin, authenticated });
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
    freshEvidence,
    authChanges,
    configuredReady,
    storageValues,
    resolverMarkers,
    markersByTab,
    originStateWrites,
    storageGate,
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
  h.tabs.seed({ id: 1, url: "https://resolver.example.edu", active: false, pinned: true, muted: true });
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
      if (tab.id !== undefined) h.tabs.seed(tab);
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
  expect(h.tabs.list().flatMap((tab) => tab.id === undefined ? [] : [tab.id])).toEqual([2]);
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

  await h.timers.runByDelay(4 * 60_000); // cycleTimer: the interval reload fires
  await h.timers.runByDelay(1); // cycleTimer: reloadSettleMs bounded final-URL inspection

  expect(h.tabs.reloaded).toEqual([1]);
  expect(h.reauthState).toEqual([true]);
  expect(h.reauths.count).toBe(1);
  expect(h.tabs.updates).toEqual([{ id: 1, properties: { active: true, pinned: false, muted: false } }]);
  expect(h.timers.latestDelay()).toBe(10);

  h.tabs.nextURL = RESOLVER_OPENURL;
  h.tabs.patch(1, { url: RESOLVER_OPENURL }); // Simulate the user's completed login.
  // Clear the spacing window opened by the pause-triggering probe above, so
  // the recovery probe the reauth watch fires below is admitted immediately
  // instead of deferred.
  h.clock.advanceBy(MIN_PROBE_START_SPACING_MS);
  await h.timers.runByDelay(10); // reauthTimer: the recovery probe runs and resumes
  await flushMicrotasks(); // armReauthTimer's callback is fire-and-forget, unlike scheduleCycle's

  expect(h.reauthState).toEqual([true, false]);
  expect(h.tabs.updates).toEqual([
    { id: 1, properties: { active: true, pinned: false, muted: false } },
    { id: 1, properties: { pinned: true, muted: true } },
  ]);

  // Resuming clears the logical pause but does not itself reschedule the
  // reload cycle — that is the next heartbeat's job: reconcile() sees
  // !reauthPaused and re-arms the (still-absolute) cycle deadline, exactly
  // the sync()-driven model this commit's starvation fix relies on.
  await h.manager.sync();
  // scheduleCycle() computes delay from the CURRENT clock, which already
  // advanced MIN_PROBE_START_SPACING_MS above to clear the recovery
  // probe's spacing window — so the re-armed cycle timer is the full
  // interval minus that elapsed time, not a fresh 4 minutes.
  await h.timers.runByDelay(4 * 60_000 - MIN_PROBE_START_SPACING_MS); // cycleTimer: reload after resume
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

  // Past MIN_FOREGROUND_PROBE_SPACING_MS — the operator floor a foreground
  // request obeys — but still well within SESSION_STALE_MS; the deleted
  // freshness gate would have skipped this call entirely. A throttle-
  // deferred foreground request now settles the instant the defer is
  // recorded, so driving runDue() afterward picks up the trailing probe
  // whether this call was admitted outright or briefly deferred.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  expect(h.clock.now()).toBeLessThan((h.manager.getSnapshot().lastVerdictAt ?? 0) + SESSION_STALE_MS);
  await h.manager.probeForeground();
  await h.timers.runDue();
  await flushMicrotasks();
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
  h.tabs.seed(liveTab);
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
  h.tabs.seed(liveTab);
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
  h.tabs.seed(focused);
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
  h.tabs.seed(stale1);
  h.tabs.seed(stale2);
  h.tabs.seed(stale3);
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
    h.tabs.seed(tab);
    flood.push(tab);
  }
  h.tabs.resolverTabs.push(...flood);

  // Past the operator floor a foreground request obeys; a throttle-deferred
  // call now settles immediately, so runDue() below drives the trailing
  // probe whether this landed admitted or briefly deferred.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  await h.timers.runDue();
  await flushMicrotasks();

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
  h.tabs.seed(focused);
  h.tabs.seed(sibling);
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
  h.tabs.seed(a);
  h.tabs.seed(b);
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
  h.tabs.seed(a);
  h.tabs.seed(b);
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
  expect(h.freshEvidence).toEqual([]);

  gate.release();
  await pending;

  // Both decisive and disagreeing, with no causal tab: conflict, not a
  // leaked "out".
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "unknown", lastProbeOutcome: "conflict" });
  expect(h.freshEvidence).toEqual([]);
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
  // See the observation-cap test above: past the operator floor, and
  // runDue() below drives the trailing probe regardless of admit/defer.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  await h.timers.runDue();
  await flushMicrotasks();

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
  h.tabs.patch(1, { url: "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver" });

  const sibling = { id: 88, url: "https://resolver.example.edu/account/overview" };
  h.tabs.seed(sibling);
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

test("a URL is routing, not an affordance: neither direction is decided by a link target", () => {
  // Verified against a real signed-in capture of a Primo NDE resolver: a
  // navigation link reading "AI Assisted Search" points at /nde/login, and
  // matching that href was single-handedly classifying an authenticated page
  // as signed out. Feature links routed through a login path survive signing
  // in, so no amount of probing at the right moment could have corrected it.
  expect(
    classifyResolverMarkers([{ text: "AI Assisted Search", label: "", visible: true }]),
  ).toBe("unknown");
  // The same rule in the harmful direction: a bare logout target with nothing
  // the operator can read is not proof of a session either.
  expect(classifyResolverMarkers([{ text: "", label: "" }])).toBe("unknown");
  // An accessible label still counts — an icon-only control is a real
  // affordance even with no text node.
  expect(classifyResolverMarkers([{ text: "", label: "Sign out of your account" }])).toBe("in");
  expect(classifyResolverMarkers([{ text: "Sign in", label: "", storageIdentity: "in" }])).toBe("in");
});

test("marker collection scans sign-out affordances inside closed and hidden menus", () => {
  // A closed account menu is where a real sign-out affordance lives, and it is
  // legitimate evidence of a session. What makes it evidence is the label the
  // operator would read on opening the menu — not the /logout target, which a
  // signed-out page can carry just as easily.
  expect(
    withResolverDom(
      "<html><body><details><div hidden><span><a href='/logout'>Sign out</a></span></div></details></body></html>",
      () => classifyResolverMarkers(collectResolverMarkers()),
    ),
  ).toBe("in");
});

test("Primo-shaped account page with a non-guest session JWT is signed in without sign-out UI", () => {
  const token = syntheticJWT({ preferred_username: "jane", userGroup: "STUDENT" });
  const markers = withResolverDom(
    "<html><body><main><h1>Jane Doe</h1><details><summary>Account</summary><div hidden>Profile</div></details></main></body></html>",
    () => {
      sessionStorage.setItem("primo-session", token);
      return collectResolverMarkers();
    },
    "https://example.primo.exlibrisgroup.com/nde/account/overview",
  );
  expect(classifyResolverMarkers(markers)).toBe("in");
  expect(markers.some((marker) => marker.storageIdentity === "in")).toBe(true);
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
  h.tabs.seed(liveTab);
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
  const token = syntheticJWT({ preferred_username: "jane", exp: Math.floor(Date.now() / 1_000) - 1 });
  const markers = withResolverDom(
    "<html><body></body></html>",
    () => {
      sessionStorage.setItem("primo-session", token);
      return collectResolverMarkers();
    },
    "https://resolver.example.edu/account",
  );
  expect(classifyResolverMarkers(markers)).toBe("unknown");
  expect(markers.some((marker) => marker.storageIdentity === "in")).toBe(false);
});

test("marker collection ignores logout text in scripts, styles, templates, and ancestors", () => {
  expect(
    withResolverDom(
      "<html><head><style>.logout { content: 'logout'; }</style></head><body><script>const label = 'logout';</script><template>logout</template><main>logout</main></body></html>",
      () => classifyResolverMarkers(collectResolverMarkers()),
      "https://resolver.example.edu/account",
    ),
  ).toBe("unknown");
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
  h.tabs.seed(uwaTab);
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
  // Primo NDE keeps a sign-in control in a closed drawer permanently — signed
  // in or not. A hidden prompt is not evidence of a signed-out session.
  expect(classifyResolverMarkers([{ text: "Sign in", label: "", visible: false }])).toBe("unknown");
  // A signed-out page puts the prompt front and center. This is the exact
  // marker the real resolver captures produce.
  expect(classifyResolverMarkers([{ text: "Sign in", label: "Sign In", visible: true }])).toBe("out");
  // Markers predating the visibility flag keep their old meaning.
  expect(classifyResolverMarkers([{ text: "Sign in", label: "" }])).toBe("out");
  // Sign-out affordances count from inside closed menus, hidden or not.
  expect(
    classifyResolverMarkers([
      { text: "Sign out", label: "", visible: false },
      { text: "Sign in", label: "", visible: true },
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
        dirtySince: null,
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
  expect(snapshot?.dirtySince).toBeNull();
});

test("all tabs signed out keeps the focused tab's verdict authoritative", async () => {
  const h = makeHarness();
  await h.manager.init();
  const first = { id: 42, url: "https://resolver.example.edu/discovery/search" };
  const second = { id: 43, url: "https://resolver.example.edu/discovery/dbsearch" };
  h.tabs.seed(first);
  h.tabs.seed(second);
  h.tabs.focusedTab = first;
  h.tabs.resolverTabs.push(first, second);
  h.markersByTab.set(42, [{ text: "Sign in", label: "Sign In", visible: true }]);
  h.markersByTab.set(43, []);

  await h.manager.probeForeground();
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "out", probeSource: "live_tab" });
});

test("closing the library tab never erases an earned warm verdict, but the next probe still runs", async () => {
  const h = makeHarness();
  await h.manager.init();
  const liveTab = { id: 42, url: "https://resolver.example.edu/account" };
  h.tabs.seed(liveTab);
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
  for (const id of h.tabs.list().flatMap((tab) => tab.id === undefined ? [] : [tab.id])) await h.tabs.userClose(id);
  h.tabs.resolverTabs.length = 0;
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS); // past the operator floor from the first probe
  await h.manager.probeForeground();
  await h.timers.runDue();
  await flushMicrotasks();

  expect(h.tabs.queryCount).toBeGreaterThan(queriesAfterFirst);
  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "in",
    lastProbeOutcome: "no_tab",
  });
});

// --- Commit B: navigation/activation events, MV3 recovery -----------------
// These exercise noteResolverNavigation/noteResolverActivated/noteTabRemoved/
// onWake() against the SAME fake browser above. `jobs.count = 0` is used
// freely below to keep a test's candidate-tab set to exactly the tabs it
// sets up itself, uncomplicated by the manager's own owned keepalive tab.

test("a completed navigation on a known resolver origin schedules exactly one trailing probe, and a url change plus complete for the same document coalesces into it", async () => {
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tab = { id: 501, url: `${origin}/discovery` };
  h.tabs.seed(tab);

  h.manager.noteResolverNavigation(tab.id, tab.url); // "url" changed event
  h.manager.noteResolverNavigation(tab.id, tab.url); // "complete" event, same document
  await flushMicrotasks();

  // Still settling — a resolver SPA renders its header and mints its
  // session token after the load event, so the probe deliberately waits.
  expect(h.scripting.injectionCounts.get(tab.id) ?? 0).toBe(0);
  // Two triggers for the SAME document must coalesce into one settle
  // timer: each call re-arms (clears+resets) the origin's settle timer, so
  // only the LAST one survives to fire. The cumulative log still shows
  // both arm attempts even though just one is left pending.
  expect(h.timers.delays.filter((delay) => delay === 1)).toHaveLength(2);
  expect(h.timers.pendingDelays().filter((delay) => delay === 1)).toHaveLength(1);

  await h.timers.runByDelay(1);
  await flushMicrotasks();

  expect(h.scripting.injectionCounts.get(tab.id)).toBe(1);
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.lastProbeAt).not.toBeNull();
});

test("a navigation to an origin outside the manager's known set creates no origin state and persists nothing", async () => {
  const h = makeHarness();
  await h.manager.init();
  const knownBefore = h.manager.getOriginSnapshots().map((s) => s.origin).sort();
  const writesBefore = h.originStateWrites.length;

  h.manager.noteResolverNavigation(999, "https://unrelated-vendor.example.com/dashboard");
  await flushMicrotasks();

  expect(h.manager.getOriginSnapshots().map((s) => s.origin).sort()).toEqual(knownBefore);
  expect(h.originStateWrites.length).toBe(writesBefore);
});

test("navigating away from a known resolver to an unrelated IdP-shaped URL marks the departed origin dirty, without ever recording the new origin", async () => {
  const h = makeHarness();
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tab = { id: 501, url: `${origin}/discovery` };
  h.tabs.seed(tab);
  h.manager.noteResolverNavigation(tab.id, tab.url);
  await h.timers.runByDelay(1);
  await flushMicrotasks();
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBeNull();
  const knownBefore = h.manager.getOriginSnapshots().map((s) => s.origin).sort();

  h.tabs.patch(tab.id, { url: "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO" });
  h.manager.noteResolverNavigation(tab.id, "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO");

  const snapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(snapshot?.dirtySince).not.toBeNull();
  // The IdP host is never recorded as a resolver origin.
  expect(h.manager.getOriginSnapshots().map((s) => s.origin).sort()).toEqual(knownBefore);
  expect(h.manager.getOriginSnapshots().some((s) => s.origin.includes("idp.example.edu"))).toBe(false);
});

test("noteTabRemoved drops the tab's epoch and settle timer, and a probe that preferred it completes without committing evidence for the dead tab", async () => {
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tab = { id: 501, url: `${origin}/discovery` };
  h.tabs.seed(tab);
  h.markersByTab.set(tab.id, [{ text: "Sign out", label: "" }]); // would-be decisive "in"
  const gate = h.scripting.hold(tab.id);

  h.manager.noteResolverActivated(tab.id, tab.url); // no settle delay: admits immediately
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(tab.id)).toBe(1); // scan in flight

  await h.tabs.userClose(tab.id); // the tab closes while its scan is still pending
  h.manager.noteTabRemoved(tab.id);
  expect(h.timers.pendingDelays()).not.toContain(1); // no settle timer survives it

  gate.release(); // the stale scan resolves after the tab is already gone
  await flushMicrotasks();

  const snapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(snapshot?.verdict).toBe("unknown");
  expect(snapshot?.authenticated).toBe(false);
  // Discarded, not "no evidence found": the scan never got to report
  // anything decisive because the tab it read from was already gone.
  expect(snapshot?.lastProbeOutcome).toBe("no_tab");
});

test("a tab that navigates while its scan is still pending cannot commit the stale document's result", async () => {
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tab = { id: 501, url: `${origin}/discovery` };
  h.tabs.seed(tab);
  h.markersByTab.set(tab.id, [{ text: "Sign out", label: "" }]); // stale document: would-be decisive "in"
  const gate = h.scripting.hold(tab.id);

  h.manager.noteResolverActivated(tab.id, tab.url);
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(tab.id)).toBe(1);

  // The operator navigates the SAME tab onward before the scan returns —
  // the epoch this bumps is what the stale scan gets checked against.
  h.tabs.patch(tab.id, { url: `${origin}/checkout` });
  h.manager.noteResolverNavigation(tab.id, `${origin}/checkout`);

  gate.release(); // the STALE document's markers resolve after the navigation
  await flushMicrotasks();

  const snapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(snapshot?.authenticated).toBe(false);
  expect(snapshot?.verdict).not.toBe("in");
  // The stale read is discarded outright, not merely outvoted.
  expect(snapshot?.lastProbeOutcome).toBe("no_tab");
});

test("probe starts for one origin are spaced by MIN_PROBE_START_SPACING_MS, coalescing superseded triggers into one trailing probe", async () => {
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tabA = { id: 501, url: `${origin}/a` };
  const tabB = { id: 502, url: `${origin}/b` };
  const tabC = { id: 503, url: `${origin}/c` };
  for (const tab of [tabA, tabB, tabC]) h.tabs.seed(tab);

  h.manager.noteResolverActivated(tabA.id, tabA.url); // nothing yet probed for this origin: runs immediately
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(tabA.id)).toBe(1);

  h.clock.advanceBy(2_000); // well inside the spacing window
  h.manager.noteResolverActivated(tabB.id, tabB.url);
  await flushMicrotasks();
  h.clock.advanceBy(2_000);
  h.manager.noteResolverActivated(tabC.id, tabC.url); // supersedes B as the newest pending trigger
  await flushMicrotasks();

  expect(h.scripting.injectionCounts.get(tabB.id) ?? 0).toBe(0);
  expect(h.scripting.injectionCounts.get(tabC.id) ?? 0).toBe(0);

  h.clock.advanceBy(MIN_PROBE_START_SPACING_MS);
  await h.timers.runDue();
  await flushMicrotasks();

  // Exactly one trailing probe ran, and it carried the NEWEST generation's
  // preferred tab — B's own trigger never got a standalone probe of its own.
  expect(h.scripting.injectionCounts.get(tabC.id)).toBe(1);
  expect(h.scripting.injectionCounts.get(tabB.id) ?? 0).toBe(0);
});

test("probeForeground does not bypass the spacing limit, and popup-style open/close cycling cannot exceed it", async () => {
  const h = makeHarness();
  await h.manager.init(); // owned tab id 1
  await h.manager.probeForeground(); // first probe for this origin: runs immediately
  const afterFirst = h.scripting.injectionCounts.get(1) ?? 0;
  expect(afterFirst).toBeGreaterThan(0);

  // Rapid popup open/close, each well inside MIN_FOREGROUND_PROBE_SPACING_MS
  // of the first start. The caller-settlement fix under test resolves a
  // throttle-deferred foreground call the instant the defer is recorded —
  // never waiting on the eventual trailing probe — so all five can be
  // awaited directly with no real or fake time spent.
  for (let i = 0; i < 5; i++) {
    h.clock.advanceBy(300); // 5 * 300ms = 1_500ms, still inside the 2s floor
    await h.manager.probeForeground();
  }
  // None of these five calls could have started a genuinely new probe:
  // every one landed inside the operator floor and coalesced into the
  // same still-pending trailing request.
  expect(h.scripting.injectionCounts.get(1) ?? 0).toBe(afterFirst);

  // Cross the operator floor: exactly one coalesced trailing probe runs.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.timers.runDue();
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(1) ?? 0).toBe(afterFirst + 1);

  // Companion: an AUTOMATIC trigger (navigation, on a second tab at the
  // same origin) is still held to the full MIN_PROBE_START_SPACING_MS
  // floor — the shorter operator floor above governs foreground requests
  // only.
  const liveTab = { id: 900, url: RESOLVER_OPENURL };
  h.tabs.seed(liveTab);
  h.manager.noteResolverNavigation(liveTab.id, liveTab.url);
  await h.timers.runByDelay(1); // reloadSettleMs: the settle timer requests the probe
  await flushMicrotasks();

  // MIN_FOREGROUND_PROBE_SPACING_MS elapsed is enough for the operator
  // floor but nowhere near the automatic one: the navigation trigger must
  // still be deferred, not admitted.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.timers.runDue();
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(liveTab.id) ?? 0).toBe(0);

  // The remaining time to complete the full automatic floor: now it runs.
  h.clock.advanceBy(MIN_PROBE_START_SPACING_MS - MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.timers.runDue();
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(liveTab.id)).toBe(1);
});

test("a dirty origin survives a simulated worker restart and is probed by onWake(); a clean, not-yet-due origin is not", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const tab = { id: 701, url: `${origin}/discovery` };
  h.tabs.seed(tab);
  h.manager.noteResolverNavigation(tab.id, tab.url);
  await h.timers.runByDelay(1);
  await flushMicrotasks();
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBeNull();

  // The operator leaves for an IdP: dirtySince is written through the
  // serialized persist chain.
  h.tabs.patch(tab.id, { url: "https://idp.example.edu/idp/sso" });
  h.manager.noteResolverNavigation(tab.id, "https://idp.example.edu/idp/sso");
  await flushMicrotasks();
  const dirtyAt = h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince;
  expect(dirtyAt).not.toBeNull();
  const persisted = h.storageValues["keepalive.originStates"] as KeepaliveOriginSnapshot[];
  expect(persisted.find((s) => s.origin === origin)?.dirtySince).toBe(dirtyAt);

  // The worker "dies": a fresh manager restores from the SAME persisted storage.
  const restarted = makeHarness(4, undefined, { storageValues: { ...h.storageValues } });
  await restarted.manager.init();
  expect(restarted.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBe(dirtyAt);

  const liveTab = { id: 702, url: `${origin}/account` };
  restarted.tabs.seed(liveTab);
  restarted.tabs.resolverTabs.push(liveTab);
  const queriesBeforeWake = restarted.tabs.queryCount;
  await restarted.manager.onWake();
  await flushMicrotasks();
  expect(restarted.tabs.queryCount).toBeGreaterThan(queriesBeforeWake);
  expect(restarted.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBeNull();

  // Clean, not paused, nowhere near due: the very next wake must not touch
  // the browser at all.
  const queriesAfterCommit = restarted.tabs.queryCount;
  await restarted.manager.onWake();
  expect(restarted.tabs.queryCount).toBe(queriesAfterCommit);
});

test("onWake() probes purely from dirty/paused/due origin state, with no daemon-port involvement", async () => {
  const h = makeHarness();
  h.jobs.count = 0;
  await h.manager.init();
  const origin = "https://resolver.example.edu";
  const tab = { id: 501, url: `${origin}/discovery` };
  h.tabs.seed(tab);
  h.manager.noteResolverNavigation(tab.id, tab.url); // establishes the tab<->origin association
  await h.timers.runByDelay(1);
  await flushMicrotasks();

  // KeepaliveManager has no port/message concept at all — onWake() is
  // callable with nothing but the manager itself, and a clean, committed
  // origin means there is nothing for it to do.
  const queriesBefore = h.tabs.queryCount;
  await h.manager.onWake();
  expect(h.tabs.queryCount).toBe(queriesBefore);

  // Dirty it via a departure-shaped navigation, which marks dirty but does
  // not itself probe…
  h.tabs.patch(tab.id, { url: "https://idp.example.edu/idp/sso" });
  h.manager.noteResolverNavigation(tab.id, "https://idp.example.edu/idp/sso");
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).not.toBeNull();
  expect(h.tabs.queryCount).toBe(queriesBefore);

  // …and onWake() picks it up purely from that state. "wake" is an
  // automatic trigger: a throttle-deferred call resolves only once its
  // trailing probe runs (unlike a foreground request's immediate defer
  // settlement), so it is fired without an await and any fake spacing
  // timer it might arm (only possible if this landed exactly on the
  // floor and was deferred) is driven explicitly rather than raced
  // against real time.
  h.clock.advanceBy(MIN_PROBE_START_SPACING_MS);
  void h.manager.onWake();
  await flushMicrotasks();
  if (h.timers.pendingDelays().includes(0)) await h.timers.runByDelay(0);
  await flushMicrotasks();
  expect(h.tabs.queryCount).toBeGreaterThan(queriesBefore);
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBeNull();
});

test("repeated sync() calls cannot postpone the cycle: the absolute deadline still fires the owned-tab reload", async () => {
  const h = makeHarness(); // interval=4 minutes -> intervalMs = 240_000
  await h.manager.init(); // owned tab id 1 created, cycle timer armed
  expect(h.tabs.reloaded).toEqual([]);

  // Heartbeats every 60s, well inside the 4-minute interval — the historical
  // bug re-armed a fresh 4-minute reload timer on every single one of these,
  // so the owned tab was never reloaded.
  for (let i = 0; i < 3; i++) {
    h.clock.advanceBy(60_000);
    await h.manager.sync();
  }
  expect(h.tabs.reloaded).toEqual([]); // not due yet

  // Cross the ORIGINAL deadline (4 minutes after init — one more 60s tick).
  h.clock.advanceBy(60_000);
  await h.timers.runDue();
  await flushMicrotasks();

  expect(h.tabs.reloaded).toEqual([1]);
});

test("a settle timer firing does not cancel the reauth watch armed for a paused origin", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.tabs.nextURL = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver";
  await h.timers.runByDelay(4 * 60_000); // cycleTimer: the interval reload fires
  await h.timers.runByDelay(1); // cycleTimer: reloadSettleMs bounded final-URL inspection -> pauses for reauth, arms the reauthTimer
  await flushMicrotasks();
  expect(h.manager.getSnapshot().pausedForReauth).toBe(true);
  expect(h.timers.pendingDelays()).toContain(10); // observeMs, configured by the harness

  // Clear the spacing window opened by the pause-triggering probe so the
  // settle-triggered probe below actually RUNS (admitted, not merely
  // deferred) — this test is about a running probe leaving the reauth
  // watch alone, not about a deferred one trivially doing so.
  h.clock.advanceBy(MIN_PROBE_START_SPACING_MS);

  // A live user tab visits the SAME known resolver origin independently of
  // the paused keepalive tab (still parked on the IdP); its navigation
  // arms a settle timer. Its markers read decisively "out" so this probe
  // cannot itself legitimately resume the pause — the owned tab's own
  // auth_url observation would only re-affirm "pause", a no-op while
  // already paused.
  const liveTab = { id: 777, url: "https://resolver.example.edu/discovery" };
  h.tabs.seed(liveTab);
  h.markersByTab.set(liveTab.id, [{ text: "Sign in", label: "" }]); // decisive "out"
  h.manager.noteResolverNavigation(liveTab.id, liveTab.url);
  expect(h.timers.pendingDelays()).toContain(1);

  await h.timers.runByDelay(1); // the settle timer fires, and its probe runs immediately
  await flushMicrotasks();

  // The reauth watch is still armed — an unrelated settle timer, and the
  // real probe it triggered, must never cancel it, or the paused operator
  // would silently stop being polled for recovery.
  expect(h.timers.pendingDelays()).toContain(10);
  expect(h.manager.getSnapshot().pausedForReauth).toBe(true);
});

test("openReauth's pauseForReauth arms the observeMs watch immediately, and does not cancel the pending cycle timer", async () => {
  const h = makeHarness();
  await h.manager.init(); // cycle timer armed at 240_000
  // Advance most of the way through the interval so any "inherited" cycle
  // timer would fire far later than observeMs.
  h.clock.advanceBy(200_000);

  await h.manager.openReauth(); // explicit operator sign-in request
  expect(h.manager.getSnapshot().pausedForReauth).toBe(true);

  // A fresh observeMs-delay watch is pending RIGHT NOW — not the leftover
  // ~40s of an inherited cycle timer, and not a full interval — and the
  // pre-existing cycle timer is untouched: one timer kind can never cancel
  // another.
  expect(h.timers.pendingDelays().slice().sort((a, b) => a - b)).toEqual([10, 240_000]);
});

test("resumeAfterReauth clears the logical pause even when the owned tab has closed, without re-pinning a gone tab", async () => {
  const h = makeHarness();
  await h.manager.init(); // owned tab id 1
  h.tabs.nextURL = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver";
  await h.timers.runNext(); // reload
  await h.timers.runNext(); // bounded final-URL inspection -> pauses for reauth
  await flushMicrotasks();
  expect(h.manager.getSnapshot().pausedForReauth).toBe(true);

  // The operator completes sign-in — the tab now reads a decisive "in" —
  // but the scan is held open while the manager independently decides to
  // close the owned tab (job queue drained), racing the resume disposition
  // against the tab's own removal. closeTab() never touches the per-tab
  // document epoch (only noteResolverNavigation/noteTabRemoved do), so the
  // in-flight read stays valid — it is resumeAfterReauth's OWN handling of
  // a since-vanished owned tab that is under test here, not the read.
  const origin = "https://resolver.example.edu";
  h.tabs.patch(1, { url: RESOLVER_OPENURL });
  h.markersByTab.set(1, [{ text: "Sign out", label: "" }]);
  const gate = h.scripting.hold(1);
  const probe = h.manager.probeForeground();
  await flushMicrotasks(); // the scan is in flight, held

  h.jobs.count = 0;
  await h.manager.sync(); // closeTab(): tab 1 removed, this.tabID cleared
  expect(h.tabs.removed).toContain(1);

  gate.release(); // the (still valid, same-document) scan resolves after removal
  await probe;
  await flushMicrotasks();

  expect(h.manager.getSnapshot().pausedForReauth).toBe(false);
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.pausedForReauth).toBe(false);
  // Nothing attempted to re-pin/unmute a tab id that no longer exists.
  expect(
    h.tabs.updates.some((u) => u.id === 1 && u.properties.pinned === true && u.properties.muted === true),
  ).toBe(false);
});

test("an older persisted write can never land after a newer one: the serialized chain keeps writes single-flight", async () => {
  const defaultOrigin = "https://resolver.example.edu";
  const secondOrigin = "https://onesearch.library.example-college.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [defaultOrigin, secondOrigin] });
  h.jobs.count = 0;
  await h.manager.init();

  const tabA = { id: 601, url: `${defaultOrigin}/discovery` };
  const tabB = { id: 602, url: `${secondOrigin}/discovery` };
  h.tabs.seed(tabA);
  h.tabs.seed(tabB);
  h.manager.noteResolverNavigation(tabA.id, tabA.url);
  h.manager.noteResolverNavigation(tabB.id, tabB.url);
  await h.timers.runByDelay(1);
  await h.timers.runByDelay(1);
  await flushMicrotasks();
  // Both origins hold committed, non-dirty state — a clean baseline.
  expect(h.manager.getOriginSnapshots().every((s) => s.dirtySince === null)).toBe(true);

  h.storageGate.arm(1); // freeze the NEXT persisted write only
  h.tabs.patch(tabA.id, { url: "https://idp.example.edu/idp/sso" }); // A departs for an IdP
  h.manager.noteResolverNavigation(tabA.id, "https://idp.example.edu/idp/sso");
  await flushMicrotasks();
  expect(h.storageGate.pendingCount).toBe(1); // A's dirty-mark write is held open

  h.tabs.patch(tabB.id, { url: "https://idp.example.edu/idp/sso" }); // B departs too, moments later
  h.manager.noteResolverNavigation(tabB.id, "https://idp.example.edu/idp/sso");
  await flushMicrotasks();
  // If the manager issued a second, concurrent storage.set() here instead of
  // queuing behind the serialized chain, this fake would happily let it
  // resolve before the first — the exact race an unawaited full-array write
  // used to allow. It must not: still exactly one write outstanding.
  expect(h.storageGate.pendingCount).toBe(1);

  h.storageGate.releaseOldest();
  await flushMicrotasks();
  h.storageGate.releaseOldest(); // release whatever the chain queued next, if anything
  await flushMicrotasks();

  const finalStates = h.storageValues["keepalive.originStates"] as KeepaliveOriginSnapshot[];
  // The last thing written reflects BOTH departures — never a stale
  // snapshot missing one because an earlier write's late completion
  // clobbered a later one.
  expect(finalStates.find((s) => s.origin === defaultOrigin)?.dirtySince).not.toBeNull();
  expect(finalStates.find((s) => s.origin === secondOrigin)?.dirtySince).not.toBeNull();
});

// --- Commit C: origin-scoped unblocking -----------------------------------
// onFreshSessionEvidence/onOriginAuthenticationChanged replace
// onSessionEvidence/onAuthenticationChanged. Evidence is release-grade only
// for a configured member origin once configuredOriginsReady() is true;
// h.configuredReady defaults to false (today's pre-hello_ack union
// behavior), so every test above this banner is unaffected.

test("a fresh release-grade probe after a warm-restored verdict still emits onFreshSessionEvidence, even though authenticated does not transition", async () => {
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
        dirtySince: null,
      },
    ],
  };
  const h = makeHarness(4, undefined, { knownOrigins: [origin], storageValues: stored });
  h.configuredReady.value = true;
  await h.manager.init();
  // Restoring a warm verdict is not an observation — it must never itself
  // be treated as fresh evidence.
  expect(h.freshEvidence).toEqual([]);

  const tab = { id: 70, url: `${origin}/account` };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab); // default markers "Sign out" -> decisive "in"
  // The manager's own owned tab is ALSO pinned at this origin (jobs.count
  // defaults to 1, so init() maintains warm demand here) and would agree
  // just as decisively — without a causal preference, an owned tab in the
  // decisive set wins the source label even when a live tab was also
  // decisive. Focusing this tab makes it the causal observation, so the
  // evidence this test cares about is unambiguously attributed to a real
  // USER tab, not the manager's own keepalive tab.
  h.tabs.focusedTab = tab;

  await h.manager.probeForeground(origin);

  // authenticated was already true before and after this probe — a
  // transition-only callback would stay silent here, which is exactly the
  // bug a worker restart used to trigger.
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "markers",
  });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.freshEvidence[0]).toMatchObject({ origin, source: "live_tab" });
  expect(typeof h.freshEvidence[0]?.generation).toBe("number");
});

test("onOriginAuthenticationChanged fires only when the committed authenticated value actually changes", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync(); // close the owned tab: only the tab set up below counts

  const tab = { id: 40, url: `${origin}/account` };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab);

  // First probe: default markers ("Sign out") commit authenticated=true —
  // a real change from the initial default (false).
  await h.manager.probeForeground(origin);
  expect(h.authChanges).toEqual([{ origin, authenticated: true }]);

  // Second probe, same decisive "in": authenticated stays true, no change,
  // no second call.
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground(origin);
  expect(h.authChanges).toEqual([{ origin, authenticated: true }]);

  // Third probe flips to "out": a genuine change back to false.
  h.resolverMarkers.splice(0, h.resolverMarkers.length, { text: "Sign in", label: "" });
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground(origin);
  expect(h.authChanges).toEqual([
    { origin, authenticated: true },
    { origin, authenticated: false },
  ]);

  // A preserved-verdict commit (scan_failed) must not fire even though
  // nothing changed to trigger it accidentally either.
  h.api.scripting = {
    executeScript: async () => {
      throw new Error("host permission revoked");
    },
  };
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground(origin);
  expect(h.authChanges).toEqual([
    { origin, authenticated: true },
    { origin, authenticated: false },
  ]);
});

test("a preserved-verdict commit (no_tab, scan_failed, partial_scan) fires neither callback", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync(); // close the owned tab: no candidate tab exists yet

  // no_tab: nothing to observe, nothing to preserve — still no callback.
  await h.manager.probeForeground();
  expect(h.manager.getSnapshot().lastProbeOutcome).toBe("no_tab");
  expect(h.freshEvidence).toEqual([]);
  expect(h.authChanges).toEqual([]);

  // Earn a real "in" first, so later preserved branches have something at
  // stake — proving they truly preserve rather than trivially having
  // nothing to report either way.
  const tab = { id: 40, url: `${origin}/account` };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab);
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  const earned = h.manager.getSnapshot();
  expect(earned).toMatchObject({ verdict: "in", authenticated: true, probeSource: "live_tab" });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toHaveLength(1);

  // scan_failed: an injection failure preserves the earned verdict, exactly
  // as the reducer documents — verdict/authenticated/probeSource/
  // lastVerdictAt are left alone; only lastProbeAt/lastProbeOutcome advance.
  h.api.scripting = {
    executeScript: async () => {
      throw new Error("host permission revoked");
    },
  };
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  const afterScanFailed = h.manager.getSnapshot();
  expect(afterScanFailed).toMatchObject({
    verdict: earned.verdict,
    authenticated: earned.authenticated,
    probeSource: earned.probeSource,
    lastVerdictAt: earned.lastVerdictAt,
    lastProbeOutcome: "scan_failed",
  });
  expect(afterScanFailed.lastProbeAt).not.toBe(earned.lastProbeAt);
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toHaveLength(1);

  // partial_scan: too many candidate tabs to trust siblings alone — same
  // preserve-only contract as scan_failed. Every flood tab reads no markers
  // at all (not the harness's decisive-"Sign out" default): a decisive "in"
  // among truncated siblings is now allowed to commit (see reduceObservations
  // — D-fix), so this branch is exercised only when nothing among the
  // truncated set is decisive either way. No trailing drain here: nothing
  // this assertion cares about is scheduled behind a timer, and running one
  // would also fire the unrelated warm-demand-lapsed housekeeping tick this
  // test's own `h.jobs.count = 0` armed earlier, which independently resets
  // the origin once no tab exists to observe — a real but orthogonal path,
  // not the preserved-commit behavior under test here.
  h.api.scripting = { executeScript: h.scripting.executeScript };
  h.markersByTab.set(tab.id, []); // neutralize: this section tests truncation with NO decisive tab, not tab 40's earlier "in" carrying it.
  expect(MAX_OBSERVED_TABS_PER_ORIGIN).toBe(5);
  const flood: KeepaliveTab[] = [];
  for (let id = 200; id < 200 + MAX_OBSERVED_TABS_PER_ORIGIN + 1; id++) {
    const floodTab = { id, url: `${origin}/discovery/${id}` };
    h.tabs.seed(floodTab);
    h.markersByTab.set(id, []);
    flood.push(floodTab);
  }
  h.tabs.resolverTabs.push(...flood);
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  const afterPartialScan = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(afterPartialScan).toMatchObject({
    verdict: earned.verdict,
    authenticated: earned.authenticated,
    probeSource: earned.probeSource,
    lastVerdictAt: earned.lastVerdictAt,
    lastProbeOutcome: "partial_scan",
  });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toHaveLength(1);
});

test("a conflict reduction fires neither callback", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync(); // close the owned tab: it must not act as a tiebreaker here.

  // Neither tab is focused and neither is preferred, so there is no causal
  // observation to arbitrate between two tabs that disagree decisively.
  const a = { id: 51, url: `${origin}/discovery/a` };
  const b = { id: 52, url: `${origin}/discovery/b` };
  h.tabs.seed(a);
  h.tabs.seed(b);
  h.tabs.resolverTabs.push(a, b);
  h.markersByTab.set(51, [{ text: "Sign in", label: "" }]);
  h.markersByTab.set(52, [{ text: "Sign out", label: "" }]);

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "unknown",
    authenticated: false,
    lastProbeOutcome: "conflict",
  });
  expect(h.freshEvidence).toEqual([]);
  expect(h.authChanges).toEqual([]);
});

test("exactly one onFreshSessionEvidence fires per committing probe, even when several observed tabs read in", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  const siblings = [61, 62, 63].map((id) => ({ id, url: `${origin}/discovery/${id}` }));
  for (const tab of siblings) h.tabs.seed(tab);
  h.tabs.resolverTabs.push(...siblings); // default markers "Sign out" -> every one decisive "in"

  await h.manager.probeForeground();

  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "markers",
  });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.freshEvidence[0]).toMatchObject({ origin, source: "live_tab" });
});

test("neither callback fires before configuredOriginsReady() returns true, even for a release-grade in", async () => {
  const h = makeHarness(); // configuredReady defaults to false: pre-hello_ack
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  const tab = { id: 40, url: "https://resolver.example.edu/account" };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab); // default markers "Sign out" -> decisive "in"

  await h.manager.probeForeground();

  // The origin still earns its own verdict locally...
  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "markers",
  });
  // ...but nothing is release-grade before the daemon handshake has landed.
  expect(h.freshEvidence).toEqual([]);
  expect(h.authChanges).toEqual([]);
});

test("once ready, an origin present only in offer traffic is not a configured member", async () => {
  const offerOrigin = "https://resolver.example.edu"; // from the default OpenURL, never in knownOrigins
  const memberOrigin = "https://onesearch.library.example-college.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [memberOrigin] });
  h.configuredReady.value = true;
  await h.manager.init();

  // No row for the offer-only origin: membership is exactly
  // knownResolverOrigins() once ready, never the owned tab's target.
  expect(h.manager.getOriginSnapshots().map((s) => s.origin)).toEqual([memberOrigin]);

  h.jobs.count = 0;
  await h.manager.sync();
  const tab = { id: 40, url: `${offerOrigin}/account` };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab); // default markers "Sign out" -> would-be decisive "in"

  // Even an explicit-origin probe (which bypasses candidate selection)
  // cannot manufacture release authority for a non-member.
  await h.manager.probeForeground(offerOrigin);
  expect(h.freshEvidence).toEqual([]);
  expect(h.authChanges).toEqual([]);
  expect(h.manager.getOriginSnapshots().map((s) => s.origin)).toEqual([memberOrigin]);

  expect(await h.manager.openReauth(offerOrigin)).toBe(false);
});

test("while not ready, restored origin states survive syncOriginStates() when the configured set is momentarily empty", async () => {
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
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: Date.now() - 60_000,
        dirtySince: null,
      },
    ],
  };
  // knownOrigins: [] mirrors clearNegotiationState zeroing resolverOrigins on
  // every connect/reconnect — the configured set is momentarily empty, and
  // configuredReady stays at its default false throughout (never "ready").
  const h = makeHarness(4, undefined, { knownOrigins: [], storageValues: stored });
  await h.manager.init();

  expect(h.manager.getOriginSnapshots().map((s) => s.origin)).toContain(origin);

  // A second reconcile (another reconnect tick, still not ready) must not
  // delete it either.
  await h.manager.sync();
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "in",
    authenticated: true,
  });
});

test("notifyConfiguredOriginsChanged() picks up a newly configured origin immediately and probes it if dirty", async () => {
  const origin = "https://onesearch.library.example-college.edu";
  const stored: Record<string, unknown> = {
    "keepalive.originStates": [
      {
        origin,
        authenticated: false,
        verdict: "unknown",
        probeSource: "none",
        lastVerdictAt: null,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: null,
        dirtySince: Date.now() - 5_000,
      },
    ],
  };
  const resolverConfig: HarnessResolver = { knownOrigins: [], storageValues: stored };
  const h = makeHarness(4, undefined, resolverConfig);
  await h.manager.init(); // pre-hello_ack: not ready, empty configured set

  const tab = { id: 90, url: `${origin}/account` };
  h.tabs.seed(tab);
  h.tabs.resolverTabs.push(tab); // default markers "Sign out" -> decisive "in"

  // hello_ack lands: the configured set now includes the origin, and the
  // bridge tells the manager directly.
  resolverConfig.knownOrigins = [origin];
  h.configuredReady.value = true;
  h.manager.notifyConfiguredOriginsChanged();
  await flushMicrotasks();

  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "in",
    authenticated: true,
    dirtySince: null,
  });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.freshEvidence[0]).toMatchObject({ origin, source: "live_tab" });
});

test("a superseded generation's result never produces evidence", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  // First attempt: tab A's scan is held open past the probe-admission
  // deadline (20s), so its in-flight slot frees while the observation
  // itself is still pending in the background — the only way a second,
  // later-generation probe for the same origin can start at all.
  const tabA = { id: 71, url: `${origin}/account` };
  h.tabs.seed(tabA);
  h.tabs.focusedTab = tabA;
  const gateA = h.scripting.hold(tabA.id);

  const first = h.manager.probeForeground(origin);
  await flushMicrotasks();
  h.clock.advanceBy(20_000);
  await h.timers.runByDelay(20_000); // admission deadline frees the slot
  await first;

  // No commit yet: tab A's own observation has not resolved.
  expect(h.freshEvidence).toEqual([]);

  // Second attempt, a fresh generation, admitted now that the slot is
  // free: a different tab answers immediately and commits "in".
  const tabB = { id: 72, url: `${origin}/account` };
  h.tabs.seed(tabB);
  h.tabs.focusedTab = tabB; // default markers "Sign out" -> decisive "in"

  await h.manager.probeForeground(origin);
  expect(h.freshEvidence).toHaveLength(1);
  const [firstEvidence] = h.freshEvidence;

  // The stale first attempt finally resolves — reading the same decisive
  // "in" it would have committed at the time — but its generation was
  // superseded by the second attempt's, so it must not produce a second
  // evidence event.
  gateA.release();
  await flushMicrotasks();

  expect(h.freshEvidence).toHaveLength(1);
  expect(h.freshEvidence[0]).toBe(firstEvidence);
});

test("a superseded generation cannot overwrite a fresher committed verdict, nor clear a dirtySince set after it committed", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();

  // --- Phase 1: the stale attempt reads a DIFFERENT, decisive polarity
  // than the fresher generation that already committed. The scaffold this
  // builds on deliberately has the stale read agree with the fresh one,
  // so it never exercises the overwrite this guards against.

  const tabA = { id: 71, url: `${origin}/account` };
  h.tabs.seed(tabA);
  h.tabs.focusedTab = tabA;
  const gateA = h.scripting.hold(tabA.id);

  const first = h.manager.probeForeground(origin);
  await flushMicrotasks();
  h.clock.advanceBy(20_000);
  await h.timers.runByDelay(20_000); // admission deadline frees the slot
  await first;
  expect(h.freshEvidence).toEqual([]);

  const tabB = { id: 72, url: `${origin}/account` };
  h.tabs.seed(tabB);
  h.tabs.focusedTab = tabB; // default markers "Sign out" -> decisive "in"
  await h.manager.probeForeground(origin);

  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toEqual([{ origin, authenticated: true }]);
  const committedAt = h.manager.getSnapshot().lastVerdictAt;
  expect(h.manager.getSnapshot()).toMatchObject({ verdict: "in", authenticated: true });

  // Tab A's held scan finally resolves to a decisive "out" — a document
  // this manager stopped caring about several probes ago. Its generation
  // was superseded, so it must not be written AT ALL: gating only the
  // callbacks would let it silently flip the card back to "Signed out".
  gateA.release([{ text: "Sign in", label: "" }]);
  await flushMicrotasks();

  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toEqual([{ origin, authenticated: true }]); // no flip back to false
  expect(h.manager.getSnapshot()).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastVerdictAt: committedAt,
  });
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastVerdictAt: committedAt,
  });

  // --- Phase 2 (mirror, preserved-verdict branch): a stale no_tab result
  // must not clear a dirtySince that a genuinely later event set AFTER
  // the fresher probe already committed.

  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  const tabD = { id: 73, url: `${origin}/account` };
  h.tabs.seed(tabD);
  h.tabs.focusedTab = tabD;
  const gateD = h.scripting.hold(tabD.id);

  const stale2 = h.manager.probeForeground(origin);
  await flushMicrotasks();
  h.clock.advanceBy(20_000);
  await h.timers.runByDelay(20_000); // admission deadline frees the slot
  await stale2;

  const tabE = { id: 74, url: `${origin}/account` };
  h.tabs.seed(tabE);
  h.tabs.focusedTab = tabE; // default markers "Sign out" -> decisive "in" again
  await h.manager.probeForeground(origin);
  const freshCommitAt = h.manager.getSnapshot().lastVerdictAt;
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince).toBeNull();

  // A genuinely new dirty signal arrives AFTER that fresher commit.
  await h.manager.markDirty(origin);
  const dirtyAt = h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.dirtySince;
  expect(dirtyAt).not.toBeNull();

  // Tab D is gone by the time its held scan finally settles: the stale
  // attempt's own observation degrades to "stale", which reduces to a
  // "no_tab" outcome — the preserved-verdict branch.
  await h.tabs.userClose(tabD.id);
  gateD.release();
  await flushMicrotasks();

  const finalSnapshot = h.manager.getOriginSnapshots().find((s) => s.origin === origin);
  expect(finalSnapshot?.dirtySince).toBe(dirtyAt);
  expect(finalSnapshot?.verdict).toBe("in");
  expect(finalSnapshot?.lastVerdictAt).toBe(freshCommitAt);
});

test("an ownership-skipped sibling makes a fresh signed-out scan incomplete", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  h.jobs.count = 0;
  await h.manager.init();
  await h.manager.sync();

  // Establish warm signed-in evidence, then remove the warm tab so the
  // superseded scan below has exactly the held candidate plus its sibling.
  const warm = { id: 70, url: `${origin}/account` };
  h.tabs.seed(warm);
  h.tabs.focusedTab = warm;
  await h.manager.probeForeground(origin);
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.tabs.userClose(warm.id);
  h.manager.noteTabRemoved(warm.id);

  const held = { id: 71, url: `${origin}/account` };
  h.tabs.seed(held);
  h.tabs.focusedTab = held;
  const gate = h.scripting.hold(held.id);
  const stale = h.manager.probeForeground(origin);
  await flushMicrotasks();
  h.clock.advanceBy(20_000);
  await h.timers.runByDelay(20_000);
  await stale;

  const signedOut = { id: 72, url: `${origin}/account` };
  h.tabs.seed(signedOut);
  h.tabs.focusedTab = signedOut;
  h.markersByTab.set(signedOut.id, [{ text: "Sign in", label: "" }]);
  await h.manager.probeForeground(origin);

  // The held tab was omitted after the stale generation's lease expired.
  // Its sibling's decisive "out" cannot revoke the already earned session.
  expect(h.manager.getSnapshot()).toMatchObject({ authenticated: true, verdict: "in" });
  expect(h.freshEvidence).toHaveLength(1);
  expect(h.authChanges).toEqual([{ origin, authenticated: true }]);

  gate.release([{ text: "Sign out", label: "" }]);
  await flushMicrotasks();
});

test("an expired sole-candidate ownership lease permits a later probe to recover evidence", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness(4, undefined, { knownOrigins: [origin] });
  h.configuredReady.value = true;
  h.jobs.count = 0;
  await h.manager.init();
  await h.manager.sync();

  const held = { id: 73, url: `${origin}/account` };
  h.tabs.seed(held);
  h.tabs.focusedTab = held;
  const gate = h.scripting.hold(held.id);
  const originalQuery = h.tabs.query.bind(h.tabs);
  const queryGate = Promise.withResolvers<void>();
  let delayFirstQuery = true;
  h.tabs.query = async (query) => {
    if (delayFirstQuery) {
      delayFirstQuery = false;
      await queryGate.promise;
    }
    return originalQuery(query);
  };

  // Delay candidate registration itself until after the admission deadline.
  const stale = h.manager.probeForeground(origin);
  await flushMicrotasks();
  h.clock.advanceBy(20_000);
  await h.timers.runByDelay(20_000);
  await stale;
  queryGate.resolve();
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(held.id)).toBe(1);

  // The old executeScript is still wedged, but its late-registered lease is
  // already expired by data. A later generation must observe the sole
  // candidate again rather than reducing it to permanent no_tab.
  const later = h.manager.probeForeground(origin);
  await flushMicrotasks();
  expect(h.scripting.injectionCounts.get(held.id)).toBe(2);

  gate.release();
  await later;
  await flushMicrotasks();
  expect(h.manager.getSnapshot()).toMatchObject({ authenticated: true, verdict: "in" });
  expect(h.freshEvidence).toHaveLength(1);
});

test("a pause that lands after the resolver moved on to a healthy origin does not raise reauth for the new origin", async () => {
  const originA = "https://resolver.example.edu";
  const originB = "https://onesearch.library.example-college.edu";
  const stored: Record<string, unknown> = {
    "keepalive.originStates": [
      {
        origin: originB,
        authenticated: true,
        verdict: "in",
        probeSource: "live_tab",
        lastProbeOutcome: "markers",
        lastVerdictAt: Date.now(),
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: Date.now(),
        dirtySince: null,
      },
    ],
  };
  const h = makeHarness(4, undefined, { latestOpenURL: RESOLVER_OPENURL, storageValues: stored });
  await h.manager.init(); // owned tab id 1 for origin A

  // Gate only pauseForReauth's own tabs.update shape (pinned explicitly
  // false) so every other tabs.update call — e.g. openReauth's
  // healthy-tab reactivation below — is unaffected and free to settle.
  const gate = Promise.withResolvers<void>();
  const originalUpdate = h.tabs.update.bind(h.tabs);
  h.tabs.update = async (
    id: number,
    properties: { active?: boolean; pinned?: boolean; muted?: boolean },
  ) => {
    if (properties.pinned === false) await gate.promise;
    return originalUpdate(id, properties);
  };

  // The owned tab parks on an IdP redirect: commitOriginProbe's
  // disposition is "pause", which calls pauseForReauth(A) directly — NOT
  // through openReauth's serialized reauthChain.
  h.tabs.patch(1, { url: "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?service=resolver" });
  const pausing = h.manager.probeForeground();
  await flushMicrotasks();

  // pauseForReauth(A) is stuck inside its tabs.update await; nothing has
  // fired yet.
  expect(h.reauthState).toEqual([]);
  expect(h.reauths.count).toBe(0);

  // The operator opens a healthy, different origin directly. This moves
  // this.resolver/this.tabID to B well before A's stuck pause resolves —
  // openReauth is serialized only against itself, never against a
  // probe's own disposition call.
  await h.manager.openReauth(originB);
  expect(h.manager.getSnapshot().resolverOrigin).toBe(originB);
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === originB)?.pausedForReauth).toBe(false);

  // Now let A's stuck pause resolve.
  gate.resolve();
  await pausing;
  await flushMicrotasks();

  // B must never have been told it needs reauth because of A's stale,
  // late-landing pause.
  expect(h.reauthState).toEqual([false]); // only removeStaleTab's own unpause push
  expect(h.reauths.count).toBe(0);
  expect(h.timers.pendingDelays()).not.toContain(10); // no reauth watch armed
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === originB)?.pausedForReauth).toBe(false);
});

test("a restart mid-pause does not strand the persisted pause, whether the paused tab is adopted or freshly created", async () => {
  const origin = "https://resolver.example.edu";
  const pausedSnapshot = (): Record<string, unknown> => ({
    "keepalive.originStates": [
      {
        origin,
        authenticated: false,
        verdict: "unknown",
        probeSource: "none",
        lastVerdictAt: null,
        lastProbeAt: null,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: true,
        dirtySince: null,
      },
    ],
  });

  // Branch 1: the paused tab (unpinned/unmuted by the real pauseForReauth
  // before the restart) still satisfies the fake adoption query, so
  // createTabOnce() takes the ADOPT branch.
  {
    const h = makeHarness(4, undefined, {
      latestOpenURL: RESOLVER_OPENURL,
      storageValues: pausedSnapshot(),
    });
    const adopted = { id: 501, url: `${origin}/account`, active: false, pinned: true, muted: true };
    h.tabs.seed(adopted);
    h.tabs.resolverTabs.push(adopted);

    await h.manager.init();
    await flushMicrotasks();

    expect(h.tabs.created).toHaveLength(0); // adopted, not created
    expect(h.manager.getSnapshot().pausedForReauth).toBe(false);
    expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.pausedForReauth).toBe(false);
    expect(h.timers.pendingDelays()).not.toContain(10); // no reauth watch left armed
    const persisted = h.storageValues["keepalive.originStates"] as KeepaliveOriginSnapshot[];
    expect(persisted.find((s) => s.origin === origin)?.pausedForReauth).toBe(false);
  }

  // Branch 2: nothing adoptable exists — createTabOnce() takes the CREATE
  // branch instead.
  {
    const h = makeHarness(4, undefined, {
      latestOpenURL: RESOLVER_OPENURL,
      storageValues: pausedSnapshot(),
    });

    await h.manager.init();
    await flushMicrotasks();

    expect(h.tabs.created).toHaveLength(1); // freshly created, not adopted
    expect(h.manager.getSnapshot().pausedForReauth).toBe(false);
    expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)?.pausedForReauth).toBe(false);
    expect(h.timers.pendingDelays()).not.toContain(10);
    const persisted = h.storageValues["keepalive.originStates"] as KeepaliveOriginSnapshot[];
    expect(persisted.find((s) => s.origin === origin)?.pausedForReauth).toBe(false);
  }
});

test("a non-decisive causal tab cedes the verdict to a decisive sibling, and probeSource reflects whichever tab actually decided", async () => {
  // Polarity 1: the causal (focused) tab is a live, non-owned tab reading
  // no markers at all; the manager's OWNED tab is a non-causal sibling
  // that reads decisively "in". Precedence must fall through past the
  // non-decisive causal tab to the sibling, and probeSource must credit
  // the owned tab that actually decided — not the non-owned causal one.
  {
    const h = makeHarness();
    await h.manager.init(); // owned tab id 1, default markers "Sign out" -> decisive "in"

    const focused = { id: 50, url: "https://resolver.example.edu/discovery/search" };
    h.tabs.seed(focused);
    h.tabs.focusedTab = focused;
    h.markersByTab.set(focused.id, []); // no markers at all

    await h.manager.probeForeground();

    expect(h.manager.getSnapshot()).toMatchObject({
      verdict: "in",
      authenticated: true,
      probeSource: "keepalive_tab",
      lastProbeOutcome: "markers",
    });
  }

  // Polarity 2: the causal (focused) tab is now the manager's OWNED tab,
  // reading no markers; a non-owned live sibling reads decisively "out".
  // probeSource must credit the non-owned sibling it ceded to, not the
  // owned causal tab.
  {
    const h = makeHarness();
    await h.manager.init(); // owned tab id 1
    h.markersByTab.set(1, []); // owned tab: no markers at all
    h.tabs.focusedTab = h.tabs.snapshot(1)!;

    const sibling = { id: 60, url: "https://resolver.example.edu/account/overview" };
    h.tabs.seed(sibling);
    h.tabs.resolverTabs.push(sibling);
    h.markersByTab.set(sibling.id, [{ text: "Sign in", label: "" }]); // decisive "out"

    await h.manager.probeForeground();

    expect(h.manager.getSnapshot()).toMatchObject({
      verdict: "out",
      authenticated: false,
      probeSource: "live_tab",
      lastProbeOutcome: "markers",
    });
  }
});

// --- Security review follow-ups: prose-as-affordance, unbounded collector
// payloads, unreachable verdicts under tab flooding, and the injected JWT
// closure's agreement with its tested spec (issues A/B/D/F). Every test
// below is grounded in the current classifyResolverMarkers/
// collectResolverMarkers/reduceObservations/resolverMarkerVerdict source,
// not the pre-fix behavior it replaces.

test("prose containing a sign-out phrase is not an affordance, pinned exactly at MAX_AFFORDANCE_LENGTH", () => {
  // A discovery layer renders result/article titles inside plain anchors.
  // "log out" appearing inside a headline-length string must not forge a
  // sign-out affordance — only a control-label-length string can.
  const articleProse = "Why students log out of surveillance platforms";
  expect(articleProse.length).toBeGreaterThan(MAX_AFFORDANCE_LENGTH);
  expect(classifyResolverMarkers([{ text: articleProse, label: "" }])).toBe("unknown");
  // A real control label still counts.
  expect(classifyResolverMarkers([{ text: "Sign out", label: "" }])).toBe("in");
  // Pin the boundary itself, from both sides: a normalized text of exactly
  // MAX_AFFORDANCE_LENGTH characters still qualifies; one character past it
  // does not.
  const atLimit = "Sign out of your library account here".padEnd(MAX_AFFORDANCE_LENGTH, "!");
  expect(atLimit.length).toBe(MAX_AFFORDANCE_LENGTH);
  expect(classifyResolverMarkers([{ text: atLimit, label: "" }])).toBe("in");
  const overLimit = atLimit + "!";
  expect(classifyResolverMarkers([{ text: overLimit, label: "" }])).toBe("unknown");
});

test("the same MAX_AFFORDANCE_LENGTH bound applies to the sign-in direction", () => {
  // The harmful direction is a forged "in", but the classifier applies one
  // rule to both marker types — prose must not assert "out" either.
  const articleProse = "Students explain why they sign in to campus portals daily";
  expect(articleProse.length).toBeGreaterThan(MAX_AFFORDANCE_LENGTH);
  expect(classifyResolverMarkers([{ text: articleProse, label: "", visible: true }])).toBe("unknown");
  const atLimit = "Sign in to your library account here".padEnd(MAX_AFFORDANCE_LENGTH, "!");
  expect(atLimit.length).toBe(MAX_AFFORDANCE_LENGTH);
  expect(classifyResolverMarkers([{ text: atLimit, label: "", visible: true }])).toBe("out");
  const overLimit = atLimit + "!";
  expect(classifyResolverMarkers([{ text: overLimit, label: "", visible: true }])).toBe("unknown");
});

test("whitespace formatting cannot defeat or trip the affordance length bound", () => {
  // Newlines and runs of spaces around a real control label must not push
  // its RAW length over the bound: the length that matters is the
  // normalized (whitespace-collapsed, trimmed) one.
  const paddedLabel = "\n\n\n\n   \t  \n Sign     \n\n   out   \n\n\n\n   \t\t \n\n  ";
  expect(paddedLabel.length).toBeGreaterThan(MAX_AFFORDANCE_LENGTH); // raw length alone would wrongly exclude it
  expect(classifyResolverMarkers([{ text: "", label: paddedLabel }])).toBe("in");
});

test("the injected collector caps element count, per-marker text, and deeply nested control text", () => {
  // More matching controls than the element cap: the whole array is
  // structured-cloned back to the service worker, so the cap must bound the
  // RETURNED count, not merely how much is read.
  {
    const controls = Array.from(
      { length: MAX_SCANNED_CONTROLS + 50 },
      (_unused, index) => `<button>Sign out ${index}</button>`,
    ).join("");
    const markers = withResolverDom(`<html><body>${controls}</body></html>`, () => collectResolverMarkers());
    expect(markers.length).toBe(MAX_SCANNED_CONTROLS);
  }

  // One control whose own text is far longer than the per-marker cap.
  {
    const longText = "L".repeat(MAX_MARKER_TEXT_LENGTH * 25);
    const markers = withResolverDom(
      `<html><body><button>${longText}</button></body></html>`,
      () => collectResolverMarkers(),
    );
    expect(markers).toHaveLength(1);
    expect(markers[0]?.text.length).toBe(MAX_MARKER_TEXT_LENGTH);
  }

  // Deeply nested matched controls (a <label> wrapping a <label> wrapping a
  // <label>...), each level carrying its own substantial text: controlText()
  // re-serializes every descendant at every ancestor level, so this shape is
  // the quadratic-blowup case, not just a single oversized control.
  {
    const depth = 30; // well past MAX_CONTROL_TEXT_DEPTH
    const perLevelText = "X".repeat(500);
    let inner = "leaf";
    for (let level = 0; level < depth; level++) inner = `<label>${perLevelText}${inner}</label>`;
    const markers = withResolverDom(`<html><body>${inner}</body></html>`, () => collectResolverMarkers());
    expect(markers).toHaveLength(depth);
    const totalChars = markers.reduce((sum, marker) => sum + marker.text.length + marker.label.length, 0);
    // Pre-fix (no per-marker truncation, no depth bound), this shape's
    // outermost markers alone would each re-serialize up to depth *
    // perLevelText.length characters, summing to roughly depth*(depth+1)/2 *
    // perLevelText.length ≈ 232,500 total characters. Every marker is
    // truncated independently now, so the total is bounded by count * cap
    // regardless of nesting depth.
    expect(totalChars).toBeGreaterThan(0);
    expect(totalChars).toBeLessThanOrEqual(depth * MAX_MARKER_TEXT_LENGTH * 2);
  }
});


test("the receiver rejects an oversized marker array as a failed scan, preserving the prior verdict", async () => {
  const h = makeHarness();
  await h.manager.init(); // owned tab id 1, default "Sign out" marker -> earns "in"
  await h.manager.probeForeground();
  const earned = h.manager.getSnapshot();
  expect(earned).toMatchObject({ verdict: "in", authenticated: true });

  // An array longer than collectResolverMarkers()'s own element cap cannot
  // be a genuine result of the current collector — only a page bypassing it
  // (or a compromised channel) could produce one. Every entry is still
  // decisive "Sign out" content: if the receiver's length check were
  // missing, this would classify "in" too, via a fresh "markers" commit —
  // the ONLY thing this test can tell apart is whether the array was
  // rejected outright (outcome "scan_failed", timestamp preserved) or
  // accepted and reclassified (outcome "markers", fresh timestamp).
  const oversized: ResolverMarker[] = Array.from({ length: MAX_SCANNED_CONTROLS + 1 }, () => ({
    text: "Sign out",
    label: "",
  }));
  h.markersByTab.set(1, oversized);
  h.clock.advanceBy(MIN_FOREGROUND_PROBE_SPACING_MS);
  await h.manager.probeForeground();
  await h.timers.runDue();
  await flushMicrotasks();

  const after = h.manager.getSnapshot();
  expect(after).toMatchObject({
    verdict: "in",
    authenticated: true,
    lastProbeOutcome: "scan_failed",
  });
  expect(after.lastVerdictAt).toBe(earned.lastVerdictAt);
  expect(after.lastProbeAt).not.toBe(earned.lastProbeAt);
});

test("six or more resolver tabs no longer deadlock the verdict: a decisive in among truncated siblings commits", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync(); // close the owned tab: it must not supply a causal/owned bias here.
  h.resolverMarkers.splice(0, h.resolverMarkers.length); // default fallback reads nothing decisive.

  expect(MAX_OBSERVED_TABS_PER_ORIGIN).toBe(5);
  const flood: KeepaliveTab[] = [];
  for (let id = 200; id < 200 + MAX_OBSERVED_TABS_PER_ORIGIN + 1; id++) {
    const tab = { id, url: `https://resolver.example.edu/discovery/${id}` };
    h.tabs.seed(tab);
    flood.push(tab);
  }
  h.tabs.resolverTabs.push(...flood);
  // Exactly one of the six candidate tabs (more than the observation cap)
  // carries a decisive sign-out affordance; the rest read nothing at all.
  // None is focused or preferred, so there is no causal tiebreaker — the
  // decisive tab sorts first in query order, landing inside the cap.
  h.markersByTab.set(200, [{ text: "Sign out", label: "" }]);

  await h.manager.probeForeground();

  const origin = "https://resolver.example.edu";
  // A researcher with six library tabs open must not be stuck on "unknown"
  // forever just because one more tab than the cap exists: truncation
  // cannot manufacture a decisive "in", so a real one still commits.
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "in",
    authenticated: true,
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
  });
});

test("companion: the same truncated shape with only decisive out observations still refuses to commit out", async () => {
  const h = makeHarness();
  await h.manager.init();
  h.jobs.count = 0;
  await h.manager.sync();
  h.resolverMarkers.splice(0, h.resolverMarkers.length);

  const flood: KeepaliveTab[] = [];
  for (let id = 300; id < 300 + MAX_OBSERVED_TABS_PER_ORIGIN + 1; id++) {
    const tab = { id, url: `https://resolver.example.edu/discovery/${id}` };
    h.tabs.seed(tab);
    flood.push(tab);
  }
  h.tabs.resolverTabs.push(...flood);
  // Sign-in affordances must be prominent, so "out" demands having actually
  // looked everywhere — the asymmetry the classifier already relies on. A
  // truncated scan must never manufacture "out" the way it may now
  // manufacture "in".
  h.markersByTab.set(300, [{ text: "Sign in", label: "", visible: true }]);

  await h.manager.probeForeground();

  const origin = "https://resolver.example.edu";
  expect(h.manager.getOriginSnapshots().find((s) => s.origin === origin)).toMatchObject({
    verdict: "unknown",
    authenticated: false,
    lastProbeOutcome: "partial_scan",
  });
});

test("collectResolverMarkers' injected storage-identity closure agrees with classifyResolverJWTIdentity across a shared corpus", () => {
  // hasStorageIdentity (injected, self-contained for executeScript) must
  // stay byte-for-byte equivalent to classifyResolverJWTIdentity (the
  // exported, tested spec) — the injected copy is what actually grants a
  // release-grade "in", and the exported one is the only one the test suite
  // can reach directly.
  const corpus: readonly [string, string, "in" | "unknown"][] = [
    ["named-claim token", syntheticJWT({ userName: "Jane Doe", userGroup: "STUDENT" }), "in"],
    ["GUEST-group token", syntheticJWT({ userName: "Jane Doe", userGroup: "GUEST" }), "unknown"],
    ["sub-plus-group token", syntheticJWT({ sub: "u123", userGroup: "STAFF" }), "in"],
    ["bare-sub token", syntheticJWT({ sub: "a81bc81b-dead-4e5d" }), "unknown"],
    [
      "expired token",
      syntheticJWT({ preferred_username: "jane", exp: Math.floor(Date.now() / 1_000) - 1 }),
      "unknown",
    ],
    ["malformed token", "not.a.jwt!", "unknown"],
  ];

  for (const [name, token, expected] of corpus) {
    const specResult = classifyResolverJWTIdentity([token]);
    expect(specResult, `${name}: classifyResolverJWTIdentity`).toBe(expected);

    const collectorResult = withResolverDom(
      "<html><body></body></html>",
      () => {
        sessionStorage.setItem("token", token);
        const markers = collectResolverMarkers();
        return markers.some((marker) => marker.storageIdentity === "in") ? ("in" as const) : ("unknown" as const);
      },
      "https://resolver.example.edu/account",
    );
    expect(collectorResult, `${name}: collectResolverMarkers`).toBe(expected);
  }
});