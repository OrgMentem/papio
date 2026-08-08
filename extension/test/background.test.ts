// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Behavioural tests for the MV3 bridge against a minimal fake chrome surface and
// a fake native port. No real chrome, and no wall-clock timers: every fake
// emitter awaits the handler promises it triggers, so the flow is deterministic.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";

import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape } from "../src/state";
import { capturePage, encodePageCapture, sanitizeFixture } from "../src/capture";
import { KeepaliveManager, type FreshSessionEvidence, type KeepaliveAPI } from "../src/keepalive";
import { interpret, type AdapterSpec, type PageVerdict } from "../src/adapters/types";
import {
  Bridge,
  findManagedTab,
  MIN_DAEMON_VERSION,
  hasDaemonUpdateHint,
  handleInboxRuntimeMessage,
  respondToRuntimePromise,
  INBOX_RUNTIME_MESSAGE_TYPES,
  needsVisibleWindow,
  normalizeManagedTabURL,
  isBotChallenge,
  isRedirectLoopPage,
  assessDrivenPage,
  registrableProviderHost,
  federatedLoginClaimKey,
  type BridgeDeps,
  type DownloadDeltaLike,
  type DownloadItemLike,
  type NativePort,
  type PdfGrabCorrelation,
  type TabChangeInfo,
  type TabInfo,
} from "../src/background";
import { routeResolverService } from "../src/resolver";

/** A hello_ack that must read as a healthy daemon has to sit at or above
 * background.ts's MIN_DAEMON_VERSION. Deriving it keeps a floor bump from
 * silently flipping every fixture below to "daemon_outdated" — the literal
 * "0.9.0" these used to carry was the old floor, and became outdated the
 * moment it moved. Tests that deliberately want an OLD daemon keep a literal.
 */
const CURRENT_DAEMON = MIN_DAEMON_VERSION;

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
  /** chrome.tabs.onActivated: the operator switched to a tab (ADR-0013 uses
   * this to privilege the focused tab over background navigation noise). */
  readonly onActivated = new FakeEmitter<[{ tabId: number; windowId: number }]>();
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
  runtimeMessages: object[];
  pdfGrabCorrelations: { current: Record<string, PdfGrabCorrelation> };
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
  const runtimeMessages: object[] = [];
  const pdfGrabCorrelations: { current: Record<string, PdfGrabCorrelation> } = { current: {} };
  const runtimeSendMessage = async (message: object): Promise<void> => {
    runtimeMessages.push(message);
  };
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
    runtimeSendMessage,
    pdfGrabCorrelations: {
      get: async () => pdfGrabCorrelations.current,
      set: async (value) => {
        pdfGrabCorrelations.current = value;
      },
    },
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
    runtimeMessages,
    pdfGrabCorrelations,
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

/** Build release-grade FreshSessionEvidence for a Commit C bridge call. The
 * generation is opaque to Bridge (KeepaliveManager-internal bookkeeping), so
 * tests never need a meaningful value here. */
function freshEvidence(h: Harness, origin: string, source: FreshSessionEvidence["source"] = "keepalive_tab"): FreshSessionEvidence {
  return { origin, observedAt: h.clock.now, generation: 1, source };
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

/** extractMetaURL and extractDownloadURL are self-contained page functions:
 * background.ts injects them verbatim via chrome.scripting.executeScript, so
 * they read the page's global `document`/`location` rather than taking a
 * Document parameter, and are deliberately not exported — executeScript
 * serializes the injected function alone, not its module scope, so a shared
 * helper or a test import would either break at runtime or let a test-local
 * reimplementation silently drift from the function actually shipped (see
 * the comment above extractDownloadURL in background.ts). Driving one job
 * through the real "meta"/"href" download path captures the exact function
 * reference background.ts hands to executeScript for that method, so every
 * case below calls production code directly. */
async function captureExtractor(method: "meta" | "href"): Promise<(...args: unknown[]) => unknown> {
  const h = makeHarness();
  const adapter: AdapterSpec = {
    id: `extractor-capture-${method}`,
    version: "1.0.0",
    hosts: [PROVIDER_HOST],
    classify: [],
    download:
      method === "meta"
        ? { selector: "a", requireKind: "article", method: "meta", metaName: "citation_pdf_url" }
        : { selector: "a.pdf", requireKind: "article", method: "href" },
  };
  h.deps.adapterSpecs.push(adapter);
  h.deps.permissions.contains = async () => true;
  let captured: ((...args: unknown[]) => unknown) | undefined;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (injection.func === interpret) {
      return [{ result: { kind: "article", adapter_id: adapter.id, adapter_version: adapter.version, evidence: [] } }];
    }
    // Whatever's left is the extractor under test for this method: capture
    // the real reference without invoking it here — a null result lets the
    // download flow finish cleanly without starting a chrome.downloads call.
    captured = injection.func as (...args: unknown[]) => unknown;
    return [{ result: null }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer(`job_capture_${method}`));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const url = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  if (captured === undefined) throw new Error(`extractor for method "${method}" was never invoked`);
  return captured;
}

/** Run `fn` with `globalThis.document`/`globalThis.location` set to a
 * happy-dom page at `url` — the environment executeScript actually provides
 * the extractor inside the tracked tab — then restore whatever was there
 * before. Mirrors the document/localStorage/sessionStorage swap
 * keepalive.test.ts uses for collectResolverMarkers; `location` joins the
 * swap here because extractMetaURL/extractDownloadURL read `location.href`
 * for the self-URL guard, which collectResolverMarkers never needs, and
 * HTMLMetaElement/HTMLAnchorElement join it because both extractors guard
 * their querySelector result with `instanceof` against those GLOBAL
 * constructors — bun's test environment has no DOM globals at all, so
 * without this swap every element happy-dom hands back fails the
 * instanceof check against an undefined constructor. */
function withPageGlobals<T>(url: string, html: string, fn: () => T): T {
  const window = new Window({ url });
  window.document.write(html);
  const previous = {
    document: globalThis.document,
    location: globalThis.location,
    HTMLMetaElement: globalThis.HTMLMetaElement,
    HTMLAnchorElement: globalThis.HTMLAnchorElement,
  };
  Object.assign(globalThis, {
    document: window.document,
    location: window.location,
    HTMLMetaElement: window.HTMLMetaElement,
    HTMLAnchorElement: window.HTMLAnchorElement,
  });
  try {
    return fn();
  } finally {
    Object.assign(globalThis, previous);
  }
}

test("extractMetaURL rejects every self-reference escape but still resolves a distinct download URL", async () => {
  const extractMetaURL = await captureExtractor("meta");
  const PAGE = "https://p.example.edu/a";
  const metaTag = (content: string): string =>
    `<html><head><meta name="citation_pdf_url" content="${content}"></head><body></body></html>`;
  const run = (html: string): unknown => withPageGlobals(PAGE, html, () => extractMetaURL("citation_pdf_url"));

  expect(run("<html><head></head><body></body></html>")).toBeNull(); // no meta tag at all
  expect(run(metaTag(""))).toBeNull(); // empty content
  expect(run(metaTag("   "))).toBeNull(); // whitespace-only content
  // The two escapes a literal href === href comparison missed: WHATWG
  // serializes a non-null empty query as a trailing "?", one character away
  // from the page's own href.
  expect(run(metaTag("?"))).toBeNull();
  // Userinfo survives href serialization unchanged while addressing the
  // identical page — rejected outright, not folded into the equality check.
  expect(run(metaTag("https://x@p.example.edu/a"))).toBeNull();
  expect(run(metaTag("#frag"))).toBeNull();
  expect(run(metaTag("//p.example.edu/a"))).toBeNull(); // protocol-relative self-reference
  expect(run(metaTag("javascript:alert(1)"))).toBeNull();
  expect(run(metaTag("data:text/html,hi"))).toBeNull();
  expect(run(metaTag("blob:https://p.example.edu/xyz"))).toBeNull();
  // Must still work: a real, distinct download URL on the same path.
  expect(run(metaTag("?download=true"))).toBe("https://p.example.edu/a?download=true");
  expect(run(metaTag("https://p.example.edu/other.pdf"))).toBe("https://p.example.edu/other.pdf");
});

test("extractDownloadURL rejects every self-reference escape but still resolves a distinct download URL", async () => {
  const extractDownloadURL = await captureExtractor("href");
  const PAGE = "https://p.example.edu/a";
  const anchor = (href: string): string => `<html><body><a class="pdf" href="${href}">Download</a></body></html>`;
  const run = (html: string): unknown => withPageGlobals(PAGE, html, () => extractDownloadURL("a.pdf"));

  expect(run("<html><body></body></html>")).toBeNull(); // no matching anchor at all
  expect(run(anchor(""))).toBeNull(); // empty href
  expect(run(anchor("   "))).toBeNull(); // whitespace-only href
  expect(run(anchor("?"))).toBeNull();
  expect(run(anchor("https://x@p.example.edu/a"))).toBeNull();
  expect(run(anchor("#frag"))).toBeNull();
  expect(run(anchor("//p.example.edu/a"))).toBeNull();
  expect(run(anchor("javascript:alert(1)"))).toBeNull();
  expect(run(anchor("data:text/html,hi"))).toBeNull();
  expect(run(anchor("blob:https://p.example.edu/xyz"))).toBeNull();
  expect(run(anchor("?download=true"))).toBe("https://p.example.edu/a?download=true");
  expect(run(anchor("https://p.example.edu/other.pdf"))).toBe("https://p.example.edu/other.pdf");
});

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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["browser-v1", "direct-download"] }));

  expect(h.backend.store).toMatchObject({
    connectionStatus: "connected",
    daemonVersion: CURRENT_DAEMON,
    daemonFeatures: ["browser-v1", "direct-download"],
    daemonUpdateHint: false,
  });
  expect(h.action.texts.at(-1)).toBe("");
});

test("a restarted worker clears persisted page-acquire capability before hello_ack", async () => {
  const h = makeHarness({
    ...emptyStore(),
    connectionStatus: "connected",
    daemonVersion: CURRENT_DAEMON,
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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }));

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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }));

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
    await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON }));

    expect(h.backend.store).toMatchObject({
      connectionStatus: "connected",
      daemonVersion: CURRENT_DAEMON,
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

test("an unregistered provider captures evidence and exits with a missing-adapter outcome", async () => {
  const h = makeHarness();
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
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === capturePage) {
      return [{
        result: {
          html: "<main class=\"article\">unsupported provider shape</main>",
          origin: `https://${PROVIDER_HOST}`,
          path: "/stable/article",
        },
      }];
    }
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  await h.port.inbound(jobOfferForHosts("job_missing_adapter", ["resolver.example.edu"]));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  h.tabs.live.set(tabID, { id: tabID, url: articleURL });
  let nextTimer = h.timers.length;
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );
  
  for (let retry = 0; retry < 2; retry += 1) {
    const relative = h.timers.slice(nextTimer).findIndex((timer) => timer.ms === 2_500);
    expect(relative).toBeGreaterThanOrEqual(0);
    nextTimer += relative;
    const timer = h.timers[nextTimer]!;
    nextTimer += 1;
    h.clock.now += 2_500;
    await timer.fn();
  }

  expect(h.frames().filter((frame) => frame.type === "page_capture")).toHaveLength(1);
  const outcomes = h.frames().filter((frame) => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toMatchObject({
    outcome: "ui_changed",
    detail:
      "No source-controlled adapter matched this provider page. " +
      "A sanitized diagnostic was saved locally for adapter development.",
  });
  expect(h.backend.store.activeJobs).toHaveLength(0);
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

test("missing provider access stays actionable and resumes the exact tab after grant", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] = [];
  let granted = false;
  h.deps.permissions.contains = async () => granted;
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_missing_provider_access"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  h.tabs.live.set(tabID, { id: tabID, url: articleURL });
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );

  expect(h.frames().filter((frame) => frame.type === "provider_outcome")).toHaveLength(0);
  expect(h.backend.store.activeJobs).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.blocked_provider_host).toBe(PROVIDER_HOST);
  expect(injections).toEqual([]);
  expect(h.backend.store.blockedProviderHosts).toEqual([PROVIDER_HOST]);
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toContain(PROVIDER_HOST);

  granted = true;
  await h.bridge.onPermissionsChanged();
  expect(injections.some((injection) => injection.func === interpret)).toBe(true);
  expect(h.backend.store.blockedProviderHosts).toEqual([]);
  expect(h.backend.store.activeJobs[0]?.blocked_provider_host).toBeUndefined();
});

test("a classify retry whose handoff tab closed settles instead of rejecting", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async () => [{ result: { kind: "unknown" } }];

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_retry_after_tab_close"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  h.tabs.live.set(tabID, { id: tabID, url: articleURL });
  await h.tabs.onUpdated.emit(
    tabID,
    { url: articleURL, status: "complete" },
    { id: tabID, url: articleURL },
  );

  const scheduled = h.timers.findIndex((timer) => timer.ms === 2_500);
  expect(scheduled).toBeGreaterThanOrEqual(0);

  // The operator closes the handoff before the retry fires. The retry is a bare
  // timer callback, so a rejection here escapes as an unhandled rejection.
  h.tabs.live.delete(tabID);
  h.clock.now += 2_500;
  await h.timers[scheduled]?.fn();

  expect(h.frames().filter((frame) => frame.type === "provider_outcome")).toHaveLength(0);
  expect(h.backend.store.activeJobs).toHaveLength(1);
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
  expect(h.frames().filter((frame) => frame.type === "provider_outcome")).toHaveLength(0);
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
  expect(h.action.titles.at(-1)).toBe("papio: connected");
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
test("challenge resume queues without a governor slot before classifying", async () => {
  let challenge = true;
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => challenge);
  const challengeTabID = await classifyProviderUnknown(h, "job_challenge_resume_queued");
  await h.port.inbound(jobOfferForHosts("job_resume_slot_a", ["link.springer.com"]));
  await h.port.inbound(jobOfferForHosts("job_resume_slot_b", ["link.springer.com"]));
  expect(h.tabs.live.size).toBe(3);

  const spec = h.deps.adapterSpecs[0]!;
  spec.download = {
    selector: "a",
    requireKind: "article",
    method: "url",
    idPattern: ".*",
    urlTemplate: "https://download.example/paper.pdf",
  };
  h.deps.settings.getTermsConsent = async () => "accept";
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (injection.func === interpret) {
      return [{ result: { kind: "article", adapter_id: spec.id, adapter_version: spec.version, evidence: [] } }];
    }
    return [{ result: "https://download.example/paper.pdf" }];
  };

  challenge = false;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  await h.tabs.onUpdated.emit(challengeTabID, { url, status: "complete" }, { id: challengeTabID, url });

  expect(h.downloads.started).toHaveLength(0);
  const resumed = h.backend.store.activeJobs.find((job) => job.job_id === "job_challenge_resume_queued");
  expect(resumed).toMatchObject({ tab_id: challengeTabID, status: "accepted" });
  expect(resumed?.challenge_blocked).toBeUndefined();
  expect(h.tabs.live.size).toBe(3);
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
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));

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
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));
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
test("concurrent classifications after accepted terms initiate exactly one download", async () => {
  const h = makeHarness();
  const adapter: AdapterSpec = {
    id: "terms-race",
    version: "1.0.0",
    hosts: [PROVIDER_HOST],
    classify: [{ kind: "article", any: ["article"] }],
    download: {
      selector: "a.download",
      requireKind: "article",
      method: "url",
      idPattern: "stable/([^/]+)",
      urlTemplate: "https://download.example/{1}.pdf",
      requiresTermsConsent: true,
    },
  };
  h.deps.adapterSpecs.push(adapter);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (injection.func === interpret) {
      return [{ result: { kind: "article", adapter_id: adapter.id, adapter_version: adapter.version, evidence: [] } }];
    }
    return [{ result: "https://download.example/paper.pdf" }];
  };
  let consentCalls = 0;
  let gateEnabled = false;
  let releaseConsent!: () => void;
  const consentGate = new Promise<"accept">((resolve) => {
    releaseConsent = () => resolve("accept");
  });
  h.deps.settings.getTermsConsent = async () => {
    if (!gateEnabled) return "accept";
    consentCalls += 1;
    return consentGate;
  };

  await h.bridge.start();
  gateEnabled = true;
  await h.port.inbound(jobOffer("job_terms_race"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const url = `https://${PROVIDER_HOST}/stable/article`;
  const first = h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  const second = h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  for (let attempt = 0; attempt < 20 && consentCalls < 2; attempt += 1) await Promise.resolve();
  releaseConsent();
  await Promise.all([first, second]);

  expect(consentCalls).toBe(2);
  expect(h.downloads.started).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
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
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
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
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));

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

test("session evidence sends the exact origin observed for that evidence", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["session_evidence_v1"] }));

  const sent = h.bridge.emitSessionEvidence("warm_verified", "https://resolver.example.edu");

  expect(sent).toBe(true);
  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({
    evidence: "warm_verified",
    origin_hint: "https://resolver.example.edu",
    at: new Date(h.clock.now).toISOString(),
  });
});

test("session evidence with no observed origin omits origin_hint instead of guessing a granted host or the latest offer", async () => {
  // papio-7d7a0ae96ca5726e: a hint that resolves to the WRONG institution is
  // indistinguishable from a correct one to the daemon and releases that
  // institution's parked handoffs without its session having been verified.
  // Omitting is strictly safer (the daemon's no-hint path scopes to the
  // default profile), so neither decoy below may leak into the frame.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["session_evidence_v1"] }));
  const decoyOfferURL = "https://decoy.example.edu/openurl?ctx=zzz";
  const offer = jobOffer("job_evidence_decoy", decoyOfferURL) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  await h.port.inbound(offer); // populates latestResolverOrigin()'s decoy candidate
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ resolverOrigin: "https://granted-host.example.edu", pausedForReauth: false }),
  } as unknown as KeepaliveManager); // decoy for the keepalive-snapshot fallback

  const sent = h.bridge.emitSessionEvidence("warm_verified", undefined);

  expect(sent).toBe(true);
  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({ evidence: "warm_verified", at: new Date(h.clock.now).toISOString() });
});

test("an auth-flagged resolver hostname omits origin_hint on auth_returned rather than inventing a fallback", async () => {
  // resolverOriginHint returns undefined for any offer URL isAuthenticationURL
  // flags (sso/idp/login/auth/shibboleth labels), so an institution whose own
  // resolver hostname contains one of those must fail closed to "no hint" —
  // not fall back to the keepalive snapshot's granted-host decoy below.
  const ssoOpenURL = "https://sso.resolver.example.edu/openurl?ctx=abc";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["session_evidence_v1"] }));
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ resolverOrigin: "https://granted-host.example.edu", pausedForReauth: false }),
    // Commit B's onTabUpdated now tells the manager about every navigation
    // synchronously (see the "notified before ready resolves" test below);
    // this stub only cares about the resolver-origin fallback, so the
    // notification itself is a no-op here.
    noteResolverNavigation: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_sso", ssoOpenURL));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: `https://${PROVIDER_HOST}/stable/x` }, { id: tabID });

  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({ evidence: "auth_returned", at: new Date(h.clock.now).toISOString() });
});

test("auth_returned origin_hint omits an unconfigured provider origin, even though it is a bare non-auth-flagged URL", async () => {
  // issue G: resolverOriginHint alone only rejects authentication-shaped and
  // non-bare origins — it does not require the daemon's configured set, so an
  // offer's own provider origin (bare, no sso/idp/login segment) could ride
  // along as the hint unless auth_returned is held to the same
  // knownResolverOrigins() bar recordFreshSessionEvidence and
  // recordInstitutionalSession already use. www.jstor.org here is the
  // offer's provider host, never advertised as a resolver_origin below.
  const providerOpenURL = `https://${PROVIDER_HOST}/openurl?ctx=providerhint`;
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["session_evidence_v1"], resolver_origins: ["https://resolver.example.edu"] }),
  );
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ resolverOrigin: "https://resolver.example.edu", pausedForReauth: false }),
    noteResolverNavigation: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_provider_hint", providerOpenURL));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: `https://${PROVIDER_HOST}/stable/x` }, { id: tabID });

  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({ evidence: "auth_returned", at: new Date(h.clock.now).toISOString() });
});

test("auth_returned origin_hint carries a configured resolver origin", async () => {
  // Contrast case for the test above: an offer resolving to an origin the
  // daemon actually advertised in hello_ack's resolver_origins still gets
  // the hint — the fix is the membership check, not a blanket omission.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["session_evidence_v1"], resolver_origins: ["https://resolver.example.edu"] }),
  );
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ resolverOrigin: "https://resolver.example.edu", pausedForReauth: false }),
    noteResolverNavigation: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_configured_hint")); // default OPENURL is resolver.example.edu
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: `https://${PROVIDER_HOST}/stable/x` }, { id: tabID });

  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({
    evidence: "auth_returned",
    origin_hint: "https://resolver.example.edu",
    at: new Date(h.clock.now).toISOString(),
  });
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

test("Wiley epdf viewer route downloads the declared direct endpoint", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push({
    id: "wiley",
    version: "0.2.0",
    hosts: ["onlinelibrary.wiley.com"],
    classify: [],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      method: "url",
      viewerPathPattern: "/doi/epdf/",
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate: "https://onlinelibrary.wiley.com/doi/pdfdirect/{1}?download=true",
    },
  });
  const viewerURL = "https://onlinelibrary.wiley.com/doi/epdf/10.1111/rego.12568";
  await h.bridge.start();
  await h.port.inbound(jobOfferForHosts("job_wiley_epdf_viewer", ["onlinelibrary.wiley.com"], viewerURL));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.onUpdated.emit(tabID, { url: viewerURL, status: "complete" }, { id: tabID, url: viewerURL });
  expect(h.downloads.started).toEqual([
    {
      url: "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/rego.12568?download=true",
      filename: "papio/job_wiley_epdf_viewer/paper.pdf",
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
  // Startup schedules two owned-surface reconcile passes; only the reconnect
  // backoff timer belongs to this disconnect.
  const reconnects = h.timers.filter((timer) => timer.ms === 1000);
  expect(reconnects).toHaveLength(1);
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");

  await reconnects[0]?.fn();
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

  expect(h.timers.filter((timer) => timer.ms !== 12_000 && timer.ms !== 90_000)).toHaveLength(8);
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
    { ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } },
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
    { ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } },
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
    { ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } },
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
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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

// The popup withholds the `terms` option, but it decides that when the panel
// is populated and the daemon underneath can be swapped between then and the
// click (the two-binary skew AGENTS.md documents). Emitting `terms` to a
// daemon that cannot validate it does not merely fail that capture — the
// decode error tears down the whole native-messaging session — so the
// boundary that actually sends the frame refuses it independently of the UI.
test("popup capture relay withholds a terms capture until the daemon advertises the gate", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const sanitized = sanitizeFixture(`<main class="terms">Consent wall</main>`, {
    provider: "jstor",
    scenario: "terms",
    originNoQuery: "https://www.jstor.org/terms",
    capturedISO: "2026-07-27T10:11:12.000Z",
  });
  const encoded = await encodePageCapture(sanitized, {
    host: "www.jstor.org",
    scenario: "terms",
    adapterID: "jstor",
  });
  if (!encoded.ok) throw new Error(encoded.error);

  await h.bridge.start();
  // page_capture_v1 alone is what every older daemon already advertises, and
  // it must not be enough to let `terms` onto the wire.
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.page_capture", payload: encoded.payload },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "capture_failed",
      message: "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
    },
  });
  expect(h.frames().some((frame) => frame.type === "page_capture")).toBe(false);

  await h.port.inbound(helloAck({ features: ["page_capture_v1", "page_capture_terms_v1"] }));
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
});

test("page capture request visibly opens a ledgered managed tab, captures, reports, and closes", async () => {
  const h = makeHarness(undefined, { windows: true, handoffSurface: "work-window" });
  let ledger: Record<string, number> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== capturePage) return [];
    return [{
      result: {
        html: `<html><body><main class="article">Captured structure</main></body></html>`,
        origin: "https://www.jstor.org",
        path: "/stable/123",
      },
    }];
  };
  await h.bridge.start();
  await h.port.inbound(helloAck({
    features: ["page_capture_v1", "page_capture_request_v1"],
  }));
  const pending = h.port.inbound({
    protocol: "papio-browser/1",
    type: "page_capture_request",
    msg_id: "capture-request-frame",
    seq: 2,
    payload: {
      request_id: "capture-request-001",
      url: "https://www.jstor.org/stable/123",
      provider: "jstor",
      scenario: "success",
    },
  });
  for (let attempt = 0; attempt < 20 && h.tabs.created.length === 0; attempt += 1) {
    await Promise.resolve();
  }
  for (let attempt = 0; attempt < 100 && (h.windows?.updated.length ?? 0) === 0; attempt += 1) {
    await Promise.resolve();
  }
  expect(h.windows?.created).toEqual([
    { url: "https://www.jstor.org/stable/123", focused: false, state: "minimized" },
  ]);
  expect(h.tabs.created).toEqual([
    { url: "https://www.jstor.org/stable/123", active: false, windowId: 500 },
  ]);
  expect(h.windows?.updated).toContainEqual({
    windowID: 500,
    props: { focused: true, drawAttention: true, state: "normal" },
  });
  const tabID = h.tabs.live.keys().next().value as number;
  for (let attempt = 0; attempt < 20 && ledger[String(tabID)] === undefined; attempt += 1) {
    await Promise.resolve();
  }
  expect(ledger[String(tabID)]).toBeDefined();
  for (let attempt = 0; attempt < 20 && !h.timers.some((timer) => timer.ms === 30_000); attempt += 1) {
    await Promise.resolve();
  }
  await h.tabs.onUpdated.emit(
    tabID,
    { status: "complete" },
    { id: tabID, url: "https://www.jstor.org/stable/123" },
  );
  for (let attempt = 0; attempt < 20 && !h.timers.some((timer) => timer.ms === 3_000); attempt += 1) {
    await Promise.resolve();
  }
  await h.timers.find((timer) => timer.ms === 3_000)?.fn();
  await pending;

  const capture = h.frames().find((frame) => frame.type === "page_capture");
  expect(capture?.payload).toMatchObject({
    host: "www.jstor.org",
    scenario: "success",
    adapter_id: "jstor",
    encoding: "gzip+base64",
  });
  const result = h.frames().find((frame) => frame.type === "page_capture_request_result");
  expect(result?.payload).toEqual({
    request_id: "capture-request-001",
    outcome: "captured",
  });
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.live.has(tabID)).toBe(false);
  expect(ledger[String(tabID)]).toBeUndefined();
  expect(h.downloads.started).toHaveLength(0);
});

test("page capture request respects the two-drive handoff governor", async () => {
  const h = makeHarness(
    { ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } },
    { handoffSurface: "in-window" },
  );
  h.deps.permissions.contains = async () => true;
  await h.bridge.start();
  await h.port.inbound(helloAck({
    features: ["page_capture_v1", "page_capture_request_v1"],
  }));
  await h.port.inbound(jobOffer("job_capture_governor_1"));
  await h.port.inbound(jobOffer("job_capture_governor_2"));
  expect(h.tabs.created).toHaveLength(2);

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "page_capture_request",
    msg_id: "capture-request-busy",
    seq: 3,
    payload: {
      request_id: "capture-request-busy",
      url: "https://www.jstor.org/stable/999",
      provider: "jstor",
      scenario: "drift",
      settle_ms: 0,
    },
  });
  expect(h.tabs.created).toHaveLength(2);
  const result = h.frames().find(
    (frame) =>
      frame.type === "page_capture_request_result" &&
      frame.payload["request_id"] === "capture-request-busy",
  );
  expect(result?.payload["outcome"]).toBe("busy");
});

test("inbox runtime messages validate the exact extension sender", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: [] }));
  await expect(handleInboxRuntimeMessage(h.bridge, message, { id: urls.runtimeID, url: urls.inboxURL }, urls)).resolves
    .toEqual({
      ok: false,
      error: {
        code: "feature_unavailable",
        message: "This daemon does not support the requested inbox feature",
      },
    });
});
test("papio.activity accepts popup senders while triage remains inbox-only", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const reply = {
    ok: true as const,
    feature: true,
    entries: [{ seq: 1, at: "2026-08-03T00:00:00Z", kind: "download_complete", text: "Ready" }],
  };
  let requestedLimit: number | undefined;
  h.bridge.requestActivity = async (limit) => {
    requestedLimit = limit;
    return reply;
  };

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.activity", request: { limit: 10 } },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toBe(reply);
  expect(requestedLimit).toBe(10);
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.triage.counts", request: {} },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toMatchObject({ ok: false, error: { code: "unauthorized" } });
});

test("papio.stats from any papio page routes to the bridge stats request", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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

// --- papio.pageBulk.* sender validation, malformed-request rejection, and
// happy dispatch (ADR-0019 Decision 4/7) — mirrors the papio.stats pattern
// above for the seven message types isPageBulkSender/isPopupSender gate. ---

const pageBulkTestURLs = {
  runtimeID: "papio-test-id",
  inboxURL: "chrome-extension://papio-test-id/inbox.html",
  popupURL: "chrome-extension://papio-test-id/popup.html",
  historyURL: "chrome-extension://papio-test-id/history.html",
  pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
};

const pageBulkRequestFixtures: {
  type: string;
  request: Record<string, unknown>;
  unauthorizedMessage: string;
  invalidMessage: string;
  /** isPopupSender-gated (papio.pageBulk.scan) vs isPageBulkSender-gated
   * (every other pageBulk type). */
  gate: "popup" | "pageBulk";
}[] = [
  {
    type: "papio.pageBulk.load",
    request: { scan_id: "scan-1" },
    unauthorizedMessage: "This sender cannot load a page-bulk scan",
    invalidMessage: "Invalid page-bulk load request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.scan",
    request: { tab_id: 5 },
    unauthorizedMessage: "This sender cannot start a page scan",
    invalidMessage: "Invalid page scan request",
    gate: "popup",
  },
  {
    type: "papio.pageBulk.rescan",
    request: { scan_id: "scan-1" },
    unauthorizedMessage: "This sender cannot rescan a page",
    invalidMessage: "Invalid rescan request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.status",
    request: { scan_id: "scan-1", identifiers: [{ local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" }] },
    unauthorizedMessage: "This sender cannot look up page-bulk status",
    invalidMessage: "Invalid page-bulk status request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.submit",
    request: {
      scan_id: "scan-1",
      canonical_keys: ["work:1"],
      source: { kind: "browser_page", origin: "https://scholar.example.edu", detector: "generic-identifiers/1" },
    },
    unauthorizedMessage: "This sender cannot submit a page-bulk batch",
    invalidMessage: "Invalid page-bulk submit request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.allowlist.get",
    request: { origin: "https://scholar.example.edu" },
    unauthorizedMessage: "This sender cannot read the scanner allowlist",
    invalidMessage: "Invalid scanner allowlist request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.allowlist.set",
    request: { origin: "https://scholar.example.edu", allowed: true },
    unauthorizedMessage: "This sender cannot change the scanner allowlist",
    invalidMessage: "Invalid scanner allowlist request",
    gate: "pageBulk",
  },
];

function stubPageBulkBridge(h: Harness): { calls: number } {
  const counter = { calls: 0 };
  const unreached = { ok: false as const, error: { code: "unexpected", message: "the bridge must not be reached" } };
  h.bridge.getPageBulkSnapshot = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.startPageBulkScan = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.requestPageBulkRescan = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.requestPageBulkStatus = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.requestPageBulkSubmit = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.pageBulkAllowlistContains = async () => {
    counter.calls += 1;
    return unreached;
  };
  h.bridge.setPageBulkAllowlist = async () => {
    counter.calls += 1;
    return unreached;
  };
  return counter;
}

test("papio.pageBulk.* rejects a foreign extension id and a foreign origin across the whole family", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const counter = stubPageBulkBridge(h);

  for (const { type, request, unauthorizedMessage, gate } of pageBulkRequestFixtures) {
    const ownPageURL = gate === "popup" ? urls.popupURL : urls.pageBulkURL;
    for (const sender of [
      // Foreign extension id — same URL shape, different id.
      { id: "other-extension", url: ownPageURL },
      // Correct id, but a URL that is not papio's own gated page (a content
      // script running on an arbitrary page, or — for pageBulk-gated types —
      // the popup, which is not the workspace page).
      { id: urls.runtimeID, url: "https://provider.example/article" },
    ]) {
      await expect(handleInboxRuntimeMessage(h.bridge, { type, request }, sender, urls)).resolves.toEqual({
        ok: false,
        error: { code: "unauthorized", message: unauthorizedMessage },
      });
    }
  }
  expect(counter.calls).toBe(0);
});

test("papio.pageBulk.* rejects a malformed request (extra key) via hasOnlyKeys without touching the bridge", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const counter = stubPageBulkBridge(h);

  for (const { type, request, invalidMessage, gate } of pageBulkRequestFixtures) {
    const sender = { id: urls.runtimeID, url: gate === "popup" ? urls.popupURL : urls.pageBulkURL };
    await expect(
      handleInboxRuntimeMessage(h.bridge, { type, request: { ...request, unexpected: true } }, sender, urls),
    ).resolves.toEqual({ ok: false, error: { code: "invalid_request", message: invalidMessage } });
  }
  expect(counter.calls).toBe(0);
});

test("isPageBulkSender strips the ?scan=<id> query when matching the workspace page", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  h.bridge.requestPageBulkStatus = async () => ({
    ok: true,
    items: [],
    truncated: false,
  });
  const sender = { id: urls.runtimeID, url: `${urls.pageBulkURL}?scan=scan-42` };
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.pageBulk.status",
        request: { scan_id: "scan-42", identifiers: [{ local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" }] },
      },
      sender,
      urls,
    ),
  ).resolves.toEqual({ ok: true, items: [], truncated: false });
});

test("papio.pageBulk.load happy dispatch forwards scan_id to the bridge and returns its snapshot", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const snapshot = {
    scanId: "scan-1",
    sourceTabId: 42,
    sourceOrigin: "https://scholar.example.edu",
    sourceTitle: "Reading list",
    scannedAt: "2026-08-07T12:00:00.000Z",
    documentGeneration: 1,
    items: [],
    truncated: false,
    renderedRecordCountHint: null,
  };
  let loadedScanID: string | undefined;
  h.bridge.getPageBulkSnapshot = async (scanID: string) => {
    loadedScanID = scanID;
    return { ok: true, snapshot };
  };
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL };
  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.pageBulk.load", request: { scan_id: "scan-1" } }, sender, urls),
  ).resolves.toEqual({ ok: true, snapshot });
  expect(loadedScanID).toBe("scan-1");
});

test("papio.pageBulk.status happy dispatch forwards the request to the bridge and returns its items", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  let received: unknown;
  h.bridge.requestPageBulkStatus = async (request) => {
    received = request;
    return {
      ok: true,
      items: [{ local_id: "id-1", canonical_key: "work:1", status: "eligible", ownership_complete: true }],
      truncated: false,
    };
  };
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL };
  const request = { scan_id: "scan-1", identifiers: [{ local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" }] };
  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.pageBulk.status", request }, sender, urls),
  ).resolves.toEqual({
    ok: true,
    items: [{ local_id: "id-1", canonical_key: "work:1", status: "eligible", ownership_complete: true }],
    truncated: false,
  });
  expect(received).toEqual(request);
});

test("papio.pageBulk.submit happy dispatch forwards the request to the bridge and returns its result", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  let received: unknown;
  h.bridge.requestPageBulkSubmit = async (request) => {
    received = request;
    return { ok: true, submitted: 1, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_1" };
  };
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL };
  const request = {
    scan_id: "scan-1",
    canonical_keys: ["work:1"],
    source: { kind: "browser_page", origin: "https://scholar.example.edu", detector: "generic-identifiers/1" },
  };
  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.pageBulk.submit", request }, sender, urls),
  ).resolves.toEqual({ ok: true, submitted: 1, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_1" });
  expect(received).toEqual(request);
});

test("papio.pageBulk.grabPdf routes a steering grab through the native port and identifies its download", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } };
  const request = {
    tab_id: 42,
    url: "https://resolver.example.edu/content/paper.pdf",
    title: "A paper",
    scan_id: "scan-grab-1",
  };

  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }));
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({ host: new URL(request.url).hostname, title: request.title });
  const requestID = requestFrame.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestID as string,
      outcome: "steering",
      grab_id: "grab-00000001",
      steering_path: "papio/grabs/grabpath/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({ ok: true, grab_id: "grab-00000001" });
  expect(h.downloads.started[0]).toMatchObject({
    url: request.url,
    conflictAction: "uniquify",
    saveAs: false,
  });

  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    { id: 901, url: request.url, filename: "/tmp/received-paper.pdf", state: "in_progress" },
    (suggestion) => suggestions.push(suggestion),
  );
  expect(suggestions).toEqual([{ filename: "papio/grabs/grabpath/received-paper.pdf", conflictAction: "uniquify" }]);

  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  expect(h.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-00000001",
    state: "identifying",
  });
});

test("papio.pageBulk.grabPdf reconciles an interrupted download after a service-worker restart", async () => {
  const first = makeHarness();
  const urls = pageBulkTestURLs;
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } };
  const request = {
    tab_id: 42,
    url: "https://resolver.example.edu/content/restart-paper.pdf",
    scan_id: "scan-grab-restart",
  };

  await first.bridge.start();
  await first.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }));
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    first.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await first.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({ host: new URL(request.url).hostname });
  await first.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-restart",
      steering_path: "papio/grabs/grabpath/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({ ok: true, grab_id: "grab-restart" });
  const persisted = JSON.parse(JSON.stringify(first.pdfGrabCorrelations.current)) as Record<string, PdfGrabCorrelation>;

  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  restarted.pdfGrabCorrelations.current = persisted;
  restarted.downloads.items.set(901, {
    id: 901,
    url: request.url,
    state: "interrupted",
  });
  await restarted.bridge.start();
  expect(restarted.runtimeMessages).not.toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-restart",
    state: "abandoned",
    detail: "The PDF grab download was interrupted",
  });
  expect(restarted.pdfGrabCorrelations.current["grab-restart"]).toMatchObject({ abandonPending: true });
});

test("papio.pageBulk.grabPdf clears a conflict acknowledgment with its settled state", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }));
  const request = { tab_id: 42, url: "https://resolver.example.edu/conflict.pdf", title: "Conflict", scan_id: "scan-grab-conflict" };
  const replyPromise = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } },
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(nativeResult("pdf_grab_result", {
    request_id: requestFrame.payload["request_id"] as string,
    outcome: "steering",
    grab_id: "grab-conflict-0001",
    steering_path: "papio/grabs/conflict/",
  }));
  await expect(replyPromise).resolves.toEqual({ ok: true, grab_id: "grab-conflict-0001" });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "interrupted" } });
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  await h.port.inbound(nativeResult("pdf_grab_abandon_result", {
    request_id: abandonFrame.payload["request_id"] as string,
    grab_id: "grab-conflict-0001",
    state: "quarantined",
    outcome: "conflict",
    detail: "pdf grab is already settled",
  }));
  expect(h.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-conflict-0001",
    state: "identifying",
    detail: "pdf grab is already settled",
  });
  expect(h.pdfGrabCorrelations.current).toEqual({});
});

test("papio.pageBulk.grabPdf delivers an unsolicited terminal result after a service-worker restart", async () => {
  const first = makeHarness();
  const urls = pageBulkTestURLs;
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } };
  const request = {
    tab_id: 42,
    url: "https://resolver.example.edu/content/terminal-paper.pdf",
    scan_id: "scan-grab-terminal",
  };

  await first.bridge.start();
  await first.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }));
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    first.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await first.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({ host: new URL(request.url).hostname });
  await first.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-terminal",
      steering_path: "papio/grabs/grabpath/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({ ok: true, grab_id: "grab-terminal" });
  const persisted = JSON.parse(JSON.stringify(first.pdfGrabCorrelations.current)) as Record<string, PdfGrabCorrelation>;

  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  restarted.pdfGrabCorrelations.current = persisted;
  await restarted.bridge.start();
  await restarted.port.inbound(
    nativeResult("pdf_grab_result", {
      grab_id: "grab-terminal",
      outcome: "job_created",
      detail: "Queued for adoption",
    }),
  );
  expect(restarted.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-terminal",
    state: "job_created",
    detail: "Queued for adoption",
  });
  expect(restarted.pdfGrabCorrelations.current).toEqual({});
});

test("open inbox runtime request focuses the singleton or creates it from the popup", async () => {
  const h = makeHarness(undefined, { windows: true });
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
test("session probe inspects a live resolver tab before claiming signed out, and a snapshot read never does", async () => {
  const h = makeHarness();
  let injections = 0;
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
      executeScript: async () => {
        injections += 1;
        return [{ result: [{ text: "Sign out", label: "" }] }];
      },
    },
    timers: { setTimeout: () => 0, clearTimeout: () => {} },
  };
  const manager = new KeepaliveManager(keepaliveAPI, {
    trackedJobCount: () => 0,
    latestOpenURL: () => undefined,
    onFreshSessionEvidence: (evidence) => {
      void h.bridge.recordFreshSessionEvidence(evidence);
    },
  });
  await manager.init();
  h.tabs.live.set(777, { id: 777, url: "https://resolver.example.edu/account" });
  h.bridge.attachKeepalive(manager);

  // papio.session.state is a pure read: it must not inject into the operator's
  // library tab. Only papio.session.probe may, and the popup sends that once
  // when it opens.
  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.session.state" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toMatchObject({ ok: true });
  expect(injections).toBe(0);

  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.session.probe" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toMatchObject({
    ok: true,
    state: {
      authenticated: true,
      checking: false,
      resolverOrigin: "https://resolver.example.edu",
    },
  });
  expect(injections).toBeGreaterThan(0);
});
test("session state reports each known resolver and sign-in targets its origin", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const collegeOrigin = "https://onesearch.library.example-college.edu";
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://resolver.example.edu", collegeOrigin] }));

  await expect(
    handleInboxRuntimeMessage(h.bridge, { type: "papio.session.state" }, { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toMatchObject({
    ok: true,
    origins: [
      { origin: "https://resolver.example.edu", verdict: "unknown" },
      { origin: collegeOrigin, verdict: "unknown" },
    ],
  });
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.signin", origin: collegeOrigin },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toContainEqual({ url: collegeOrigin, active: true });
});

test("provider and OA offer URLs never mint institution session rows", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://resolver.example.edu"] }));
  // Direct/OA offers land on provider hosts; each used to become a phantom
  // "institution" in the popup session card.
  await h.port.inbound(jobOffer("job_provider_a", "https://www.sciencedirect.com/science/article/pii/1"));
  await h.port.inbound(jobOffer("job_provider_b", "https://direct.mit.edu/reco/article/1"));

  expect(h.bridge.knownResolverOrigins()).toEqual(["https://resolver.example.edu"]);
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.state" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toMatchObject({
    ok: true,
    origins: [{ origin: "https://resolver.example.edu" }],
  });
});

test("session sign-in reports why it cannot open without a resolver", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
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
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["triage_mutations_v1"] }));

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

test("an inbox delivery reconciliation relays confirm_request_exists/absent through the native delivery_reconcile frame", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["triage_snapshot_schema_v3"] }));

  const pending = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.delivery.reconcile", request: { job_id: "job_delivery_0001", operation: "confirm_request_exists", provider_reference: "TN-42" } },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  const frame = await h.port.waitForFrame("delivery_reconcile_request");
  expect(frame.payload["job_id"]).toBe("job_delivery_0001");
  expect(frame.payload["operation"]).toBe("confirm_request_exists");
  expect(frame.payload["provider_reference"]).toBe("TN-42");

  await h.port.inbound(nativeResult("delivery_reconcile_result", {
    request_id: frame.payload["request_id"],
    outcome: "applied",
  }));
  await expect(pending).resolves.toEqual({ ok: true, outcome: "applied" });

  const pendingAbsent = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.delivery.reconcile", request: { job_id: "job_delivery_0002", operation: "confirm_request_absent" } },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  await Promise.resolve();
  await Promise.resolve();
  const absentFrame = h.frames().filter((f) => f.type === "delivery_reconcile_request").at(-1);
  expect(absentFrame?.payload["job_id"]).toBe("job_delivery_0002");
  expect(absentFrame?.payload["operation"]).toBe("confirm_request_absent");
  expect(absentFrame?.payload["provider_reference"]).toBeUndefined();
  await h.port.inbound(nativeResult("delivery_reconcile_result", {
    request_id: absentFrame?.payload["request_id"],
    outcome: "applied",
  }));
  await expect(pendingAbsent).resolves.toEqual({ ok: true, outcome: "applied" });

  // Shape validation and sender authorization must both fail closed.
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.delivery.reconcile", request: { job_id: "job_delivery_0001", operation: "confirm_request_absent", provider_reference: "TN-42" } },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: { code: "invalid_request", message: "Invalid delivery reconciliation request" },
  });
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.delivery.reconcile", request: { job_id: "job_delivery_0001", operation: "confirm_request_absent" } },
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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["triage_snapshot_v1"] }));

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
      daemon_version: CURRENT_DAEMON,
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
      daemon_version: CURRENT_DAEMON,
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
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["triage_snapshot_v1"] }));
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
  await reconnected?.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["triage_snapshot_v1"] }));
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
      daemon_version: CURRENT_DAEMON,
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
  expect(h.action.titles.at(-1)).toBe("papio: 1 paper waiting on your institution sign-in");

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
  expect(h.action.titles.at(-1)).toBe("papio: 1 paper waiting on your institution sign-in");

  h.deps.permissions.contains = async () => false;
  await h.bridge.syncConnectionBadge();
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe("papio: 1 paper waiting on your institution sign-in");

  h.deps.permissions.contains = async () => true;
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(tabID, { url: providerURL, status: "complete" }, { id: tabID, url: providerURL });
  expect(h.action.texts.at(-1)).toBe("4");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
  expect(h.action.titles.at(-1)).toBe("papio: 4 pending triage items");

  await h.port.emitDisconnect();
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.titles.at(-1)).toBe("papio: daemon disconnected");
});

test("the sign-in badge clears when a handoff returns to its provider", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async () => true;
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
  expect(h.action.titles.at(-1)).toBe("papio: connected");
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
const OPENATHENS_ERROR_URL = "https://login.openathens.net/saml/2/sso/example.edu/redirect";
const OPENATHENS_ERROR_TITLE = "Error | OpenAthens";

function openAthensDocument(body: string, title = OPENATHENS_ERROR_TITLE): Document {
  const window = new Window({ url: OPENATHENS_ERROR_URL });
  window.document.write(`<html><head><title>${title}</title></head><body>${body}</body></html>`);
  return window.document as unknown as Document;
}

const OPENATHENS_REDIRECT_BODY = `
  <main>
    <h1>Too many redirects</h1>
    <p>Service provider redirecting to OpenAthens too many times</p>
    <dl>
      <dt>Entity ID</dt><dd>https://www.tandfonline.com/shibboleth</dd>
      <dt>Error code</dt><dd>GA-AP-4021-06</dd>
    </dl>
  </main>
`;

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
test("OpenAthens redirect-loop error parks the job provider and retains its tab", async () => {
  const h = makeHarness();
  for (const marker of [
    "Too many redirects",
    "Service provider redirecting to OpenAthens",
    "GA-AP-4021-06",
    "OA-AP-4031-05",
  ]) {
    expect(assessDrivenPage(openAthensDocument(`<p>${marker}</p>`), true)).toEqual({
      kind: "redirect_loop",
    });
  }
  expect(
    assessDrivenPage(openAthensDocument("<p>GA-AP-4021-06</p>", "Sign in | OpenAthens"), true),
  ).toEqual({ kind: "normal" });
  expect(assessDrivenPage(openAthensDocument("<p>GA-AP-4021-06</p>"), false)).toEqual({
    kind: "normal",
  });
  const document = openAthensDocument(OPENATHENS_REDIRECT_BODY);
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== assessDrivenPage) return [];
    return [{ result: assessDrivenPage(document, injection.args?.[1] === true) }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOfferForHosts("job_openathens_loop", ["www.tandfonline.com"]));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = { id: tabID, url: OPENATHENS_ERROR_URL, title: OPENATHENS_ERROR_TITLE };
  h.tabs.live.set(tabID, tab);

  await h.tabs.onUpdated.emit(tabID, { title: OPENATHENS_ERROR_TITLE }, tab);

  const relevantFrames = h.frames().filter(
    (frame) =>
      (frame.type === "handoff_outcome" && frame.payload["outcome"] === "auth_error") ||
      (frame.type === "error" && frame.payload["code"] === "challenge_blocked"),
  );
  expect(
    relevantFrames.map((frame) =>
      frame.type === "handoff_outcome" ? frame.payload["outcome"] : frame.payload["code"],
    ),
  ).toEqual(["auth_error", "challenge_blocked"]);
  expect(relevantFrames.filter((frame) => frame.type === "error")).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "tandfonline.com",
    challenge_kind: "redirect_loop",
  });
  expect(h.backend.store.challengeCooldowns).toEqual({
    "tandfonline.com": h.clock.now + 600_000,
  });
  expect(h.backend.store.providerDrainLeases?.["www.tandfonline.com"]?.parkedReason).toBe("challenge");
  expect(h.backend.store.challengeCooldowns?.["login.openathens.net"]).toBeUndefined();
  expect(h.tabs.live.has(tabID)).toBe(true);
  expect(h.tabs.navigations).toEqual([]);
  expect(h.timers.filter((timer) => timer.ms === 1_500)).toHaveLength(1);
});

test("OpenAthens title-only error catches a late body in one bounded recheck", async () => {
  const h = makeHarness();
  let document = openAthensDocument("<main>Loading error details…</main>");
  let assessments = 0;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== assessDrivenPage) return [];
    assessments += 1;
    return [{ result: assessDrivenPage(document, injection.args?.[1] === true) }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOfferForHosts("job_openathens_late_body", ["www.tandfonline.com"]));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = { id: tabID, url: OPENATHENS_ERROR_URL, title: OPENATHENS_ERROR_TITLE };
  h.tabs.live.set(tabID, tab);

  await h.tabs.onUpdated.emit(tabID, { title: OPENATHENS_ERROR_TITLE }, tab);
  expect(h.frames().some((frame) => frame.type === "error" && frame.payload["code"] === "challenge_blocked")).toBe(false);
  const rechecks = h.timers.filter((timer) => timer.ms === 1_500);
  expect(rechecks).toHaveLength(1);

  document = openAthensDocument(OPENATHENS_REDIRECT_BODY);
  await rechecks[0]!.fn();
  await rechecks[0]!.fn();

  expect(assessments).toBe(2);
  expect(
    h.frames().filter((frame) => frame.type === "error" && frame.payload["code"] === "challenge_blocked"),
  ).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "tandfonline.com",
    challenge_kind: "redirect_loop",
  });
  expect(h.backend.store.challengeCooldowns?.["tandfonline.com"]).toBe(h.clock.now + 600_000);
  expect(h.tabs.live.has(tabID)).toBe(true);
  expect(h.tabs.navigations).toEqual([]);
});

test("a normal OpenAthens sign-in page remains untouched", async () => {
  const h = makeHarness();
  const title = "Sign in | OpenAthens";
  const document = openAthensDocument("<main><h1>Sign in</h1><form></form></main>", title);
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== assessDrivenPage) return [];
    return [{ result: assessDrivenPage(document, injection.args?.[1] === true) }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOfferForHosts("job_openathens_sign_in", ["www.tandfonline.com"]));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = { id: tabID, url: OPENATHENS_ERROR_URL, title };
  h.tabs.live.set(tabID, tab);

  await h.tabs.onUpdated.emit(tabID, { title }, tab);

  expect(h.frames().some((frame) => frame.type === "handoff_outcome")).toBe(false);
  expect(h.frames().some((frame) => frame.type === "error" && frame.payload["code"] === "challenge_blocked")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.challenge_blocked).toBeUndefined();
  expect(h.backend.store.challengeCooldowns ?? {}).toEqual({});
  expect(h.timers.filter((timer) => timer.ms === 1_500)).toHaveLength(0);
  expect(h.tabs.live.has(tabID)).toBe(true);
  expect(h.tabs.navigations).toEqual([]);
});


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
  const h = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
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

test("a drive timing out on an authentication page leaves the tab open and frees the governor slot", async () => {
  // papio-63955092613c7e9c: closing the tab here would destroy whatever the
  // operator has already done on a slow institutional login — a half-filled
  // IdP form, entered credentials, or an in-flight 2FA challenge — at the
  // 3-minute mark with no warning. parkHandoffForManual's own contract is to
  // leave that page for the operator; only the governor slot is freed.
  const h = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await h.bridge.start();
  const jobIDs = Array.from({ length: 3 }, (_, index) => `job_auth_timeout_${index}`);
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.live.size).toBe(2);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id < 0)).toHaveLength(1);
  const first = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  const firstTabID = first?.tab_id ?? -1;
  expect(firstTabID).toBeGreaterThanOrEqual(0);

  // The operator is mid-login on the resolver's IdP when the governor timer fires.
  const idpURL = "https://idp.example.edu/sso";
  h.tabs.live.set(firstTabID, { id: firstTabID, url: idpURL });

  const authTimeouts = h.timers.filter((t) => t.ms === 180_000);
  expect(authTimeouts).toHaveLength(2);
  h.clock.now += 180_000;
  await authTimeouts[0]?.fn();

  expect(h.tabs.removed).not.toContain(firstTabID);
  expect(h.tabs.live.has(firstTabID)).toBe(true);
  const after = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(after?.status).toBe("auth_pending");
  expect(after?.tab_id).toBe(firstTabID);
  expect(h.frames().filter((frame) => frame.type === "auth_pending")).toHaveLength(1);

  // The slot the parked job held is freed: the third, queued job now drives.
  expect(h.tabs.created).toHaveLength(3);
  const third = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[2]);
  expect(third?.tab_id).toBeGreaterThanOrEqual(0);
});

test("a drive timing out on an ordinary provider page still closes the tab as before", async () => {
  const h = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await h.bridge.start();
  const jobIDs = Array.from({ length: 3 }, (_, index) => `job_provider_timeout_${index}`);
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  const first = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  const firstTabID = first?.tab_id ?? -1;
  expect(firstTabID).toBeGreaterThanOrEqual(0);
  // Tab never left the resolver/provider page — no IdP navigation happened.

  const providerTimeouts = h.timers.filter((t) => t.ms === 180_000);
  expect(providerTimeouts).toHaveLength(2);
  h.clock.now += 180_000;
  await providerTimeouts[0]?.fn();

  expect(h.tabs.removed).toContain(firstTabID);
  expect(h.tabs.live.has(firstTabID)).toBe(false);
  const after = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(after?.status).toBe("auth_pending");
  expect(after?.tab_id).toBe(-1);
  expect(h.tabs.created).toHaveLength(3);
});

test("registerHandoffDrive refuses to exceed HANDOFF_DRIVE_LIMIT even when called directly at capacity", async () => {
  // papio-c3f6c091b017eb0c: every caller is expected to check
  // handoffDrives.size >= HANDOFF_DRIVE_LIMIT before calling, but the check
  // and the call are separated by awaits at several call sites, so two racing
  // entry points can both pass their own check. registerHandoffDrive must
  // enforce the cap itself rather than trust the caller.
  const h = makeHarness();
  await h.bridge.start();
  const bridgeInternal = h.bridge as unknown as {
    update: (fn: (s: StoreShape) => StoreShape) => Promise<void>;
    registerHandoffDrive: (jobID: string, tabID: number) => void;
    handoffDrives: Map<string, { tabID: number }>;
    handoffDriveQueue: { jobID: string }[];
  };
  const jobIDs = ["job_cap_direct_0", "job_cap_direct_1", "job_cap_direct_2"];
  const tabIDs: number[] = [];
  for (const jobID of jobIDs) {
    const tabID = h.tabs.nextId++;
    tabIDs.push(tabID);
    h.tabs.live.set(tabID, { id: tabID, url: OPENURL });
    // Mirror what every real caller already does: the job is committed to the
    // store with its live tab_id before registerHandoffDrive is ever called.
    await bridgeInternal.update((s) => ({
      ...s,
      activeJobs: [
        ...s.activeJobs,
        {
          job_id: jobID,
          tab_id: tabID,
          offered_at: h.clock.now,
          expires_at: h.clock.now + 1_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
        },
      ],
    }));
  }

  // Three direct calls at once — simulating callers that raced past their own
  // (separately await-fenced) cap check.
  bridgeInternal.registerHandoffDrive(jobIDs[0]!, tabIDs[0]!);
  bridgeInternal.registerHandoffDrive(jobIDs[1]!, tabIDs[1]!);
  bridgeInternal.registerHandoffDrive(jobIDs[2]!, tabIDs[2]!);

  expect(bridgeInternal.handoffDrives.size).toBe(2);
  expect(bridgeInternal.handoffDrives.has(jobIDs[0]!)).toBe(true);
  expect(bridgeInternal.handoffDrives.has(jobIDs[1]!)).toBe(true);
  expect(bridgeInternal.handoffDrives.has(jobIDs[2]!)).toBe(false);
  // Refused, not dropped: it is queued so the next drain reuses the tab it
  // already has rather than stranding the job.
  expect(bridgeInternal.handoffDriveQueue.some((request) => request.jobID === jobIDs[2])).toBe(true);
});

test("governor-queued handoffs are re-driven after a service-worker restart", async () => {
  const first = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await first.bridge.start();
  const jobIDs = Array.from({ length: 5 }, (_, index) => `job_restart_governor_${index}`);
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));
  expect(first.tabs.live.size).toBe(2);
  const parked = first.backend.store.activeJobs.filter((job) => job.tab_id < 0);
  expect(parked).toHaveLength(3);
  expect(parked.every((job) => job.status === "accepted")).toBe(true);

  // Only the persisted store survives a suspend; the FIFO holding those three
  // accepted-but-undriven jobs is worker memory. They used to strand forever:
  // the startup scan skipped tab_id < 0, the queued-release pass only handles
  // status "queued", and a daemon re-offer on the same URL merely re-acks.
  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  await restarted.bridge.start();
  // The restored worker must give the governor's two slots to the jobs that
  // were waiting for them, not leave them stranded at tab_id -1 forever.
  const parkedIDs = parked.map((job) => job.job_id);
  const drivenAfterRestart = restarted.backend.store.activeJobs.filter(
    (job) => parkedIDs.includes(job.job_id) && job.tab_id >= 0,
  );
  expect(drivenAfterRestart).toHaveLength(2);
});

test("a parked auth job with a preserved tab survives a restart without re-consuming its governor slot", async () => {
  // papio: the 3-minute timeout preserves the tab (and frees the governor
  // slot) when it lands on a recognized auth page instead of closing it —
  // see "a drive timing out on an authentication page..." above. Before
  // parked_with_tab existed, auth_pending + tab_id >= 0 was indistinguishable
  // from a job still mid-drive, so a restart between the park and the
  // operator finishing auth silently re-registered the parked job and
  // re-consumed its already-freed slot — and MV3's ~30s idle teardown made
  // that re-establish on every restart during a slow institutional SSO,
  // halving effective governor capacity for every other queued job.
  const first = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await first.bridge.start();
  const jobIDs = Array.from({ length: 3 }, (_, index) => `job_park_restart_${index}`);
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));

  const parkedTabID = first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])?.tab_id ?? -1;
  expect(parkedTabID).toBeGreaterThanOrEqual(0);
  const activeTabID = first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[1])?.tab_id ?? -1;
  expect(activeTabID).toBeGreaterThanOrEqual(0);

  // job[0] is mid-login on the resolver's IdP when its governor timer fires;
  // job[1] never navigates, so its own drive stays genuinely active.
  first.tabs.live.set(parkedTabID, { id: parkedTabID, url: "https://idp.example.edu/sso" });
  const authTimeouts = first.timers.filter((t) => t.ms === 180_000);
  expect(authTimeouts).toHaveLength(2);
  first.clock.now += 180_000;
  await authTimeouts[0]?.fn();

  const parkedAfterTimeout = first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(parkedAfterTimeout?.status).toBe("auth_pending");
  expect(parkedAfterTimeout?.tab_id).toBe(parkedTabID);
  expect(parkedAfterTimeout?.parked_with_tab).toBe(true);
  // The slot it freed went straight to the third, previously-queued job.
  expect(first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[2])?.tab_id).toBeGreaterThanOrEqual(0);

  // A service-worker restart over the exact persisted snapshot. MV3 tears
  // down the worker, NOT the browser's tabs, so every tab these jobs are
  // tracking is still open afterwards — including the one the timeout
  // deliberately preserved. Carrying them across is what makes this a
  // restart rather than a browser relaunch; without it reconcileTabs
  // correctly requeues everything and the parked state is never exercised.
  const survivingTabs = first.backend.store.activeJobs
    .filter((job) => job.tab_id >= 0)
    .map((job) => [job.job_id, job.tab_id] as const);
  expect(survivingTabs).toHaveLength(3);
  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  for (const [, tabID] of survivingTabs) {
    restarted.tabs.live.set(tabID, {
      id: tabID,
      url: tabID === parkedTabID ? "https://idp.example.edu/sso" : OPENURL,
    });
  }
  await restarted.bridge.start();

  const restartedInternal = restarted.bridge as unknown as {
    handoffDrives: Map<string, { tabID: number }>;
  };
  expect(restartedInternal.handoffDrives.has(jobIDs[0]!)).toBe(false);
  const restartedParked = restarted.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(restartedParked?.tab_id).toBe(parkedTabID);
  expect(restartedParked?.status).toBe("auth_pending");
  expect(restartedParked?.parked_with_tab).toBe(true);

  // The two genuinely active drives (job[1], never timed out, and job[2],
  // which claimed the freed slot before the restart) still restore as
  // before — exactly two slots, exactly two fresh 3-minute timeouts, and
  // neither is the parked job.
  expect(restartedInternal.handoffDrives.has(jobIDs[1]!)).toBe(true);
  expect(restartedInternal.handoffDrives.has(jobIDs[2]!)).toBe(true);
  expect(restartedInternal.handoffDrives.size).toBe(2);
  expect(restarted.timers.filter((t) => t.ms === 180_000)).toHaveLength(2);
});

test("finishing auth in a parked, preserved tab clears parked_with_tab so a later restart still restores it", async () => {
  // The tab-update handler is the only way a parked_with_tab job leaves the
  // parked state without going through registerHandoffDrive again: the
  // operator just keeps using the same preserved tab until it lands back on
  // the provider. If that transition left the marker stale, a restart after
  // auth completed but before any fresh drive would wrongly skip
  // re-registering a now-legitimate awaiting_download job — the same
  // stranding bug this campaign already fixed once, just moved one step later.
  const h = makeHarness({ ...emptyStore(), lastAuthReturnedAt: 1_700_000_000_000 });
  await h.bridge.start();
  const jobIDs = ["job_park_clear_0", "job_park_clear_1"];
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  const parkedTabID = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])?.tab_id ?? -1;
  expect(parkedTabID).toBeGreaterThanOrEqual(0);
  h.tabs.live.set(parkedTabID, { id: parkedTabID, url: "https://idp.example.edu/sso" });
  const timeout = h.timers.find((t) => t.ms === 180_000);
  h.clock.now += 180_000;
  await timeout?.fn();
  expect(h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])?.parked_with_tab).toBe(true);

  // The operator finishes authenticating in that same preserved tab.
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.onUpdated.emit(parkedTabID, { url: providerURL }, { id: parkedTabID, url: providerURL });

  const resumed = h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(resumed?.status).toBe("awaiting_download");
  expect(resumed?.parked_with_tab).toBe(false);

  // A restart now must treat it as the active, recoverable job it is again.
  // The worker restarts; the tabs do not close (see the sibling test above).
  const survivingTabs = h.backend.store.activeJobs
    .filter((job) => job.tab_id >= 0)
    .map((job) => [job.job_id, job.tab_id] as const);
  const restarted = makeHarness(JSON.parse(JSON.stringify(h.backend.store)) as StoreShape);
  for (const [, tabID] of survivingTabs) {
    restarted.tabs.live.set(tabID, {
      id: tabID,
      url: tabID === parkedTabID ? providerURL : OPENURL,
    });
  }
  await restarted.bridge.start();
  const restartedInternal = restarted.bridge as unknown as {
    handoffDrives: Map<string, { tabID: number }>;
  };
  expect(restartedInternal.handoffDrives.has(jobIDs[0]!)).toBe(true);
});

test("resuming a parked job while the governor is at capacity clears parked_with_tab so a restart mid-queue still recovers it", async () => {
  // papio: resumeHandoffAfterManual only enqueues (never patches the job)
  // whenever both governor slots are already full — the normal steady state,
  // since parkHandoffForManual drains its freed slot straight into the next
  // queued job. Before clearParkedMarker existed, that left parked_with_tab
  // true on a job with a live status and a live tab for as long as it sat in
  // the worker-local queue; a restart landing in that window (MV3 tears the
  // worker down after ~30s idle) saw the stale marker and skipped
  // re-registering it forever — stranded outside governor supervision, with
  // no timeout and no capacity accounting.
  const first = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await first.bridge.start();
  const jobIDs = Array.from({ length: 3 }, (_, index) => `job_resume_capacity_${index}`);
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));

  const parkedTabID = first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])?.tab_id ?? -1;
  expect(parkedTabID).toBeGreaterThanOrEqual(0);

  // job[0] stalls on the resolver's IdP when its governor timer fires,
  // freeing its slot straight to the third, previously-queued job — the
  // steady state in which any later resume has to go through the queue
  // instead of registering directly.
  first.tabs.live.set(parkedTabID, { id: parkedTabID, url: "https://idp.example.edu/sso" });
  const authTimeouts = first.timers.filter((t) => t.ms === 180_000);
  expect(authTimeouts).toHaveLength(2);
  first.clock.now += 180_000;
  await authTimeouts[0]?.fn();
  expect(first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])?.parked_with_tab).toBe(true);
  const beforeResume = first.bridge as unknown as { handoffDrives: Map<string, unknown> };
  expect(beforeResume.handoffDrives.size).toBe(2);
  expect(beforeResume.handoffDrives.has(jobIDs[0]!)).toBe(false);

  // The operator clears the challenge on the preserved tab. Both governor
  // slots are already full (job[1]'s original drive, job[2]'s backfilled
  // one), so resumeHandoffAfterManual — the un-park path — can only enqueue.
  const resumed = await first.bridge.resumeHandoffAfterManual(jobIDs[0]!);
  expect(resumed).toBe(false);
  const afterResume = first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0]);
  expect(afterResume?.tab_id).toBe(parkedTabID);
  expect(afterResume?.status).toBe("auth_pending");
  expect(afterResume?.parked_with_tab).toBe(false);

  // A service-worker restart over the exact persisted snapshot, mid-queue —
  // MV3 tears down the worker, NOT the browser's tabs, so every tracked tab
  // is still open afterwards; carrying them across is what makes this a
  // restart rather than a browser relaunch, and is what actually exercises
  // the stale-marker window (an in-process drain would never lose the queue).
  const survivingTabs = first.backend.store.activeJobs
    .filter((job) => job.tab_id >= 0)
    .map((job) => [job.job_id, job.tab_id] as const);
  expect(survivingTabs).toHaveLength(3);
  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  for (const [, tabID] of survivingTabs) {
    restarted.tabs.live.set(tabID, {
      id: tabID,
      url: tabID === parkedTabID ? "https://idp.example.edu/sso" : OPENURL,
    });
  }
  await restarted.bridge.start();

  // Recovered, not skipped: the persisted marker was already false, so the
  // restore loop's parked_with_tab check — which exists to skip a job that
  // is genuinely still parked — does not apply, and the job gets its
  // governor slot back instead of being stranded with no timeout and no
  // capacity accounting.
  const restartedInternal = restarted.bridge as unknown as {
    handoffDrives: Map<string, { tabID: number }>;
  };
  expect(restartedInternal.handoffDrives.has(jobIDs[0]!)).toBe(true);
});

// isDirectFileOffer is a URL-SHAPE heuristic, and an institutional handoff's
// offer URL is the operator's configured OpenURL base, whose path papio does
// not constrain. So a pdf-shaped base on a requires_auth offer never took the
// direct-download shortcut (that gate is shape AND requires_auth !== true) —
// it is a real handoff. Excluding it from restore on shape alone stranded
// exactly the jobs this restore exists to recover.
test("restore recovers an auth-required handoff whose OpenURL base looks like a file", async () => {
  const pdfShapedBase = "https://resolver.example.edu/openurl/content/pdf/resolve";
  const first = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 } });
  await first.bridge.start();
  const jobIDs = Array.from({ length: 4 }, (_, index) => `job_pdfbase_governor_${index}`);
  for (const jobID of jobIDs) {
    const offer = jobOffer(jobID, pdfShapedBase) as { payload: Record<string, unknown> };
    offer.payload["requires_auth"] = true;
    await first.port.inbound(offer);
  }
  // The direct-download shortcut must not have fired: requires_auth true keeps
  // every one of these on the handoff path despite the file-shaped URL.
  expect(first.downloads.started).toHaveLength(0);
  const parked = first.backend.store.activeJobs.filter((job) => job.tab_id < 0);
  expect(parked.length).toBeGreaterThan(0);
  expect(parked.every((job) => job.status === "accepted")).toBe(true);

  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  await restarted.bridge.start();
  const parkedIDs = parked.map((job) => job.job_id);
  const drivenAfterRestart = restarted.backend.store.activeJobs.filter(
    (job) => parkedIDs.includes(job.job_id) && job.tab_id >= 0,
  );
  expect(drivenAfterRestart.length).toBeGreaterThan(0);
});

test("per-origin sign-in hands the tab to the keepalive manager", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const reauthOrigins: (string | undefined)[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    openReauth: async (originHint?: string) => {
      reauthOrigins.push(originHint);
      return true;
    },
  } as unknown as KeepaliveManager);

  const reply = await h.bridge.requestSessionSignIn("https://beta.example.edu");
  expect(reply).toEqual({ ok: true, opened: true });
  // The manager must own the sign-in tab: it pauses its own reload cycle for
  // the duration, so a scheduled reload cannot destroy an in-flight SAML
  // exchange, and its tab is never ledgered for orphan reconciliation.
  expect(reauthOrigins).toEqual(["https://beta.example.edu"]);
  expect(h.tabs.created).toHaveLength(0);
});

test("per-origin sign-in falls back to a managed tab when the manager declines", async () => {
  const h = makeHarness();
  await h.bridge.start();
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    openReauth: async () => false,
  } as unknown as KeepaliveManager);

  const reply = await h.bridge.requestSessionSignIn("https://beta.example.edu");
  expect(reply).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toHaveLength(1);
});

test("delivery provenance keeps the host that requested the download", async () => {
  const jobID = "job_delivery_provenance";
  const pdfURL = "https://provider.example.edu/article/10.1000-x.pdf";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 100,
        offered_at: 1_700_000_000_000,
        expires_at: 1_700_000_600_000,
        status: "accepted",
        provider_hosts: [],
      },
    ],
    offerURLs: { [jobID]: OPENURL },
  });
  h.tabs.live.set(100, { id: 100, url: pdfURL });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  const reply = await h.bridge.startPDFDelivery({ tab_id: 100, url: pdfURL });
  expect(reply.ok).toBe(true);
  const downloadID = 900 + h.downloads.started.length;

  // The tab stays interactive for the whole download. Re-reading it at
  // completion attached institutional provenance to whatever the operator
  // navigated to instead of the page that produced the bytes.
  h.tabs.live.set(100, { id: 100, url: "https://unrelated.example.org/elsewhere" });
  h.downloads.items.set(downloadID, {
    id: downloadID,
    tabId: 100,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 1000,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: downloadID, state: { current: "complete" } });

  const context = h.frames().find((frame) => frame.type === "delivery_context");
  expect(context?.payload).toMatchObject({ page_host: "provider.example.edu" });
});

test("delivery session evidence is frozen at request time, not completion time", async () => {
  const jobID = "job_delivery_evidence_frozen";
  const pdfURL = "https://public.example.org/article/10.2000-y.pdf";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 100,
        offered_at: 1_700_000_000_000,
        expires_at: 1_700_000_600_000,
        status: "accepted",
        provider_hosts: [],
      },
    ],
    offerURLs: { [jobID]: OPENURL },
  });
  h.tabs.live.set(100, { id: 100, url: pdfURL });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  // Request the delivery with no warm session anywhere: keepaliveAuthenticated,
  // authReturnedThisWorker, and lastAuthReturnedAt are all unset.
  const reply = await h.bridge.startPDFDelivery({ tab_id: 100, url: pdfURL });
  expect(reply.ok).toBe(true);
  const downloadID = 900 + h.downloads.started.length;

  // An institutional sign-in lands elsewhere in the browser while this
  // non-institutional download is still in flight. A live read at
  // completion would credit this delivery with it; the frozen value must not.
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));

  h.downloads.items.set(downloadID, {
    id: downloadID,
    tabId: 100,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 1000,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: downloadID, state: { current: "complete" } });

  const context = h.frames().find((frame) => frame.type === "delivery_context");
  expect(context?.payload).toMatchObject({
    route: "direct",
    session_evidence: "none",
    page_host: "public.example.org",
  });
});

test("failed delivery frozen host does not poison a later non-delivery download", async () => {
  const jobID = "job_failed_delivery_poison";
  const deliveryURL = "https://provider.example.edu/article/10.1000-x.pdf";
  const secondHost = "platform.example.edu";
  const secondURL = `https://${secondHost}/pdf/10.1000-x.pdf`;
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 100,
        offered_at: 1_700_000_000_000,
        expires_at: 1_700_000_600_000,
        status: "accepted",
        provider_hosts: [],
      },
    ],
    offerURLs: { [jobID]: OPENURL },
  });
  h.tabs.live.set(100, { id: 100, url: deliveryURL });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  // Start a delivery — freezes page_host: "provider.example.edu".
  const reply = await h.bridge.startPDFDelivery({ tab_id: 100, url: deliveryURL });
  expect(reply.ok).toBe(true);
  const deliveryDownloadID = 900 + h.downloads.started.length;

  // Complete the delivery download with HTML mime — triggers failDelivery,
  // which leaves pendingDelivery with status: "failed" and page_host intact.
  h.downloads.items.set(deliveryDownloadID, {
    id: deliveryDownloadID,
    tabId: 100,
    filename: "/tmp/wrapper.html",
    mime: "text/html",
    fileSize: 500,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: deliveryDownloadID, state: { current: "complete" } });

  // Now simulate a resolver-routed download for the same job from a different host.
  // This is a non-delivery download (track.delivery !== true), so the frozen host
  // must NOT be applied.
  const secondDownloadID = 900 + h.downloads.started.length;
  // Set up the tab with the second host URL.
  h.tabs.live.set(100, { id: 100, url: secondURL });
  // Create the download item.
  h.downloads.items.set(secondDownloadID, {
    id: secondDownloadID,
    tabId: 100,
    filename: "/tmp/real_paper.pdf",
    mime: "application/pdf",
    fileSize: 2000,
    state: "complete",
  });
  // Emit onCreated first to set up the track (non-delivery).
  await h.downloads.onCreated.emit(h.downloads.items.get(secondDownloadID)!);
  // Now emit onChanged to complete it.
  await h.downloads.onChanged.emit({ id: secondDownloadID, state: { current: "complete" } });

  // The delivery_context frame should report the SECOND host, not the stale frozen one.
  const context = h.frames().find((frame) => frame.type === "delivery_context");
  expect(context?.payload).toMatchObject({ page_host: secondHost });
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

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));
  expect(h.tabs.live.size).toBe(2);
  expect(h.tabs.created).toHaveLength(2);
  expect(h.backend.store.activeJobs.filter((job) => job.tab_id < 0)).toHaveLength(3);
});
test("keepalive warmth releases only the matching resolver queue", async () => {
  const h = makeHarness();
  const collegeOrigin = "https://onesearch.library.example-college.edu";
  const collegeOpenURL = `${collegeOrigin}/openurl?ctx=college`;
  const defaultOffer = jobOfferForHosts("job_default_origin_queue", [PROVIDER_HOST]) as {
    payload: Record<string, unknown>;
  };
  defaultOffer.payload["requires_auth"] = true;
  const uwaOffer = jobOfferForHosts("job_uwa_origin_queue", ["link.springer.com"], collegeOpenURL) as {
    payload: Record<string, unknown>;
  };
  uwaOffer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(defaultOffer);
  await h.port.inbound(uwaOffer);
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, collegeOrigin));

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_default_origin_queue")).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_uwa_origin_queue")).toMatchObject({
    status: "accepted",
    tab_id: 100,
  });
  expect(h.tabs.created).toEqual([{ url: collegeOpenURL, active: false }]);
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

test("papio reconciles its own surfaces automatically and asks only about strays", async () => {
  const h = makeHarness(undefined, { tabGroups: true });
  let ledger: Record<string, number> = { "300": 1, "301": 1, "304": 1, "999": 1 };
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
  // 300: ledgered stray in the user's window (in-window fallback) — ask.
  h.tabs.live.set(300, { id: 300, url: "https://provider.example.org/a" });
  // 301: ledgered but the user is looking at it — never a candidate.
  h.tabs.live.set(301, { id: 301, url: "https://provider.example.org/b", active: true });
  // 302: an unledgered tab in a papio-titled group — papio never created it.
  h.tabs.live.set(302, { id: 302, url: "https://provider.example.org/c", groupId: 700 });
  // 304: ledgered AND still in papio's group — papio's surface, auto-closed.
  h.tabs.live.set(304, { id: 304, url: "https://provider.example.org/d", groupId: 700 });
  h.tabGroups!.live.set(700, { id: 700, collapsed: true, title: "papio", windowId: 1 });
  // 303: the pinned keepalive resolver tab folded into the papio group —
  // papio's own session anchor, never an orphan.
  h.tabs.live.set(303, { id: 303, url: "https://resolver.example.edu/keepalive", groupId: 700, pinned: true });

  // The popup card offers ONLY the ambiguous stray, not the group member.
  const status = await h.bridge.orphanTabStatus();
  expect(status).toEqual({ count: 1, tab_ids: [300] });
  // The dead entry is pruned from the durable ledger as a scan side effect.
  expect(ledger["999"]).toBeUndefined();

  // Startup reconciliation closes the owned-surface leftover on its own.
  const { closed: reconciled } = await h.bridge.reconcileOwnedTabs();
  expect(reconciled).toBe(1);
  expect(h.tabs.removed).toEqual([304]);
  expect(ledger["304"]).toBeUndefined();

  const { closed } = await h.bridge.cleanupOrphanTabs();
  expect(closed).toBe(1);
  expect(h.tabs.removed).toEqual([304, 300]);
  expect(h.tabs.live.has(302)).toBe(true);
  expect(h.tabs.live.has(301)).toBe(true);
  expect(h.tabs.live.has(trackedTab)).toBe(true);
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

// Commit B: the browser tells papio when a resolver page changed (so the
// keepalive origin state can be marked dirty instead of relying solely on
// the bounded 4-minute reload cycle), and the keepalive alarm's wake must
// reach the manager even when the native port is down. These tests pin the
// wiring in Bridge; KeepaliveManager's own dirty/probe bookkeeping is
// covered in keepalive.test.ts.

test("a completed navigation on an untracked tab reaches noteResolverNavigation", async () => {
  // Before Commit B, onTabUpdated returns at findByTab for any untracked
  // tab — so the operator's own library tab, which is NEVER a tracked job
  // tab, was invisible to the keepalive manager entirely.
  const h = makeHarness();
  const navigations: [number, string | undefined][] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();

  const libraryTabID = 555;
  const libraryURL = "https://onesearch.library.example-college.edu/discovery/search";
  h.tabs.live.set(libraryTabID, { id: libraryTabID, url: libraryURL });
  await h.tabs.onUpdated.emit(
    libraryTabID,
    { status: "complete", url: libraryURL },
    h.tabs.live.get(libraryTabID)!,
  );

  expect(navigations).toEqual([[libraryTabID, libraryURL]]);
});

test("a navigation on a tracked handoff tab also reaches noteResolverNavigation without losing existing tracked-job handling", async () => {
  const h = makeHarness();
  const navigations: [number, string | undefined][] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tracked_nav"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.onUpdated.emit(tabID, { url: idpURL, status: "complete" }, { id: tabID, url: idpURL });

  expect(navigations).toEqual([[tabID, idpURL]]);
  // The tracked-job side effect (leaving every provider host for an IdP
  // starts human authentication) must still fire; the manager notification
  // is additive, not a replacement.
  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(true);
});

test("keepalive manager is notified of a navigation synchronously, before bridge hydration resolves", async () => {
  // This is the wake-navigation case that motivated moving the attach: a
  // navigation that arrives while the worker is still hydrating (backend.load()
  // still pending) must not be lost. Do not await start() — `ready` is still
  // pending when the tab event is emitted below.
  const h = makeHarness();
  const navigations: [number, string | undefined][] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);

  void h.bridge.start();
  const wakeURL = "https://onesearch.library.example-college.edu/discovery/search";
  void h.tabs.onUpdated.emit(501, { status: "complete", url: wakeURL }, { id: 501, url: wakeURL });

  // No await/microtask has elapsed since start() and emit() were called: if
  // the call was already recorded, it happened synchronously, ahead of the
  // `await this.ready` that still gates the rest of onTabUpdated.
  expect(navigations).toEqual([[501, wakeURL]]);
});

test("chrome.tabs.onActivated routes to noteResolverActivated with the activated tab's URL", async () => {
  // ADR-0013 privileges the focused tab: switching to a resolver tab is as
  // strong a "look now" signal as a navigation, but nothing observed it
  // before Commit B — the extension holds no onActivated listener at all.
  const h = makeHarness();
  const activations: [number, string | undefined][] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: (tabID: number, rawURL: string | undefined) => {
      activations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();

  const tabID = 888;
  const url = "https://onesearch.library.example-college.edu/discovery/search";
  h.tabs.live.set(tabID, { id: tabID, url });
  await h.tabs.onActivated.emit({ tabId: tabID, windowId: 1 });

  expect(activations).toEqual([[tabID, url]]);
});

test("tab removal routes to noteTabRemoved and still cancels the job as before", async () => {
  const h = makeHarness();
  const removed: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteTabRemoved: (tabID: number) => {
      removed.push(tabID);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tab_removed"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await h.tabs.onRemoved.emit(tabID, { isWindowClosing: false });

  expect(removed).toEqual([tabID]);
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(true);
  expect(h.backend.store.activeJobs.length).toBe(0);
});

test("the keepalive alarm calls onWake even while the native port is down", async () => {
  // onKeepaliveAlarm's port-down branch short-circuits with `this.connect();
  // return;` before ever reaching the (pre-Commit-B) triage-counts refresh.
  // onWake must run independently of that branch, not behind it, or a
  // worker that wakes with a dead port never re-checks dirty origins.
  const h = makeHarness();
  const wakes: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    onWake: async () => {
      wakes.push(wakes.length);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  await h.port.emitDisconnect();
  expect(wakes.length).toBe(0); // sanity: disconnecting alone must not wake it

  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });

  expect(wakes.length).toBe(1);
});

// Commit C: an observed sign-in must release only its own institution's
// parked handoffs, and every path that can release/reload/label evidence is
// scoped to the origin that actually produced it. recordFreshSessionEvidence
// is the sole release-authorizing entry point (fired from keepalive's
// onFreshSessionEvidence after a committed, decisive probe verdict); a mere
// OpenURL landing may only ask keepalive to look again.

test("fresh evidence for one resolver releases only that resolver's queued handoffs", async () => {
  const h = makeHarness();
  const originB = "https://example.primo.exlibrisgroup.com";
  const bOpenURL = `${originB}/openurl?ctx=b`;
  const offerA = jobOffer("job_evidence_scope_a") as { payload: Record<string, unknown> };
  offerA.payload["requires_auth"] = true;
  const offerB = jobOffer("job_evidence_scope_b", bOpenURL) as { payload: Record<string, unknown> };
  offerB.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(offerA);
  await h.port.inbound(offerB);
  expect(h.backend.store.activeJobs.filter((job) => job.status === "queued")).toHaveLength(2);

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_evidence_scope_a")).toMatchObject({
    status: "accepted",
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_evidence_scope_b")).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);
});

test("release authority expires with the evidence that granted it", async () => {
  // A worker-local Set used to short-circuit hasAuthEvidence ahead of the TTL
  // check, so once an origin went release-grade it stayed release-grade for
  // the rest of the worker's life. Evidence now lives only in the persisted,
  // timestamped map, so it ages out.
  const h = makeHarness();
  const offer = jobOffer("job_evidence_expiry") as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));
  h.clock.now += 30 * 60_000 + 1;

  await h.port.inbound(offer);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_evidence_expiry")).toMatchObject({
    status: "queued",
  });
});

test("a committed sign-out revokes that origin's release authority immediately", async () => {
  // onOriginAuthenticationChanged was wired to a no-op, so nothing could ever
  // retract evidence: papio kept opening queued handoffs into a session the
  // operator had signed out of, until the TTL or the worker expired.
  const h = makeHarness();
  const origin = "https://resolver.example.edu";
  const offer = jobOffer("job_evidence_revoked") as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, origin));
  await h.bridge.revokeAuthEvidence(origin);

  await h.port.inbound(offer);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_evidence_revoked")).toMatchObject({
    status: "queued",
  });
  expect(h.backend.store.authEvidenceByOrigin?.[origin]).toBeUndefined();

  // Signing back in re-grants it, so revocation is not a one-way latch.
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, origin));
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_evidence_revoked")).toMatchObject({
    status: "accepted",
  });
});

test("fresh evidence for one resolver cannot be laundered into another's queue by a later unscoped release call", async () => {
  // recordOpenAccessLanding's own releaseQueuedHandoffs() call carries no
  // origin at all — the exact ambient-global call site Commit C closes. Its
  // safety now comes from every queued job being admitted only through its
  // OWN origin's evidence (hasHandoffReleaseEvidence), not from the caller.
  const h = makeHarness();
  const originB = "https://onesearch.library.example-college.edu";
  const bOpenURL = `${originB}/openurl?ctx=b`;
  const institutionalA = jobOffer("job_leak_guard_a") as { payload: Record<string, unknown> };
  institutionalA.payload["requires_auth"] = true;
  const institutionalB = jobOffer("job_leak_guard_b", bOpenURL) as { payload: Record<string, unknown> };
  institutionalB.payload["requires_auth"] = true;
  const openAccessURL = "https://oa.example.edu/article/leak-guard";
  const openAccess = jobOffer("job_leak_guard_oa", openAccessURL) as { payload: Record<string, unknown> };
  openAccess.payload["requires_auth"] = false;

  await h.bridge.start();
  await h.port.inbound(institutionalA);
  await h.port.inbound(institutionalB);
  await h.port.inbound(openAccess);

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://resolver.example.edu"));
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_a")).toMatchObject({
    status: "accepted",
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_b")).toMatchObject({
    status: "queued",
  });

  const openAccessTabID = h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_oa")?.tab_id ?? -1;
  const providerURL = `https://${PROVIDER_HOST}/stable/leak-guard`;
  await h.tabs.onUpdated.emit(
    openAccessTabID,
    { url: providerURL, status: "complete" },
    { id: openAccessTabID, url: providerURL },
  );

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_b")).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
});

test("session_evidence throttling is scoped per origin, not global", async () => {
  const h = makeHarness();
  const originA = "https://resolver.example.edu";
  const originB = "https://onesearch.library.example-college.edu";
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["session_evidence_v1"] }));

  expect(h.bridge.emitSessionEvidence("warm_verified", originA)).toBe(true);
  expect(h.bridge.emitSessionEvidence("warm_verified", originB)).toBe(true);
  // Each origin is still inside its OWN throttle window; neither may borrow
  // the other's cadence.
  expect(h.bridge.emitSessionEvidence("warm_verified", originA)).toBe(false);
  expect(h.bridge.emitSessionEvidence("warm_verified", originB)).toBe(false);

  const evidenceFrames = h.frames().filter((frame) => frame.type === "session_evidence");
  expect(evidenceFrames.map((frame) => frame.payload)).toEqual([
    { evidence: "warm_verified", origin_hint: originA, at: new Date(h.clock.now).toISOString() },
    { evidence: "warm_verified", origin_hint: originB, at: new Date(h.clock.now).toISOString() },
  ]);
});

test("currentSessionEvidence for a job on B is not labelled warm or fresh by A's evidence", async () => {
  const jobA = "job_session_evidence_scope_a";
  const jobB = "job_session_evidence_scope_b";
  const originA = "https://resolver.example.edu";
  const originB = "https://onesearch.library.example-college.edu";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      { job_id: jobA, tab_id: 100, offered_at: 1, expires_at: 2, status: "accepted", provider_hosts: [] },
      { job_id: jobB, tab_id: 101, offered_at: 1, expires_at: 2, status: "accepted", provider_hosts: [] },
    ],
    offerURLs: { [jobA]: `${originA}/openurl?ctx=a`, [jobB]: `${originB}/openurl?ctx=b` },
  });
  h.tabs.live.set(100, { id: 100, url: "https://provider.example.edu/article/a.pdf" });
  h.tabs.live.set(101, { id: 101, url: "https://provider.example.edu/article/b.pdf" });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, originA, "live_tab"));

  await h.downloads.onCreated.emit({ id: 1, tabId: 100, state: "in_progress" });
  h.downloads.items.set(1, {
    id: 1,
    tabId: 100,
    filename: "/tmp/a.pdf",
    mime: "application/pdf",
    fileSize: 10,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 1, state: { current: "complete" } });

  await h.downloads.onCreated.emit({ id: 2, tabId: 101, state: "in_progress" });
  h.downloads.items.set(2, {
    id: 2,
    tabId: 101,
    filename: "/tmp/b.pdf",
    mime: "application/pdf",
    fileSize: 20,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 2, state: { current: "complete" } });

  const contexts = h.frames().filter((frame) => frame.type === "delivery_context");
  expect(contexts.find((frame) => frame.job_id === jobA)?.payload).toMatchObject({ session_evidence: "fresh_auth" });
  expect(contexts.find((frame) => frame.job_id === jobB)?.payload).toMatchObject({ session_evidence: "none" });
});

test("an institutional landing marks its origin dirty, releases and reloads its own origin, and never touches another's", async () => {
  const landingJobID = "job_institutional_landing";
  const peerJobID = "job_institutional_landing_peer";
  const otherOriginJobID = "job_institutional_landing_other_origin";
  const landingTabID = 150;
  const otherOrigin = "https://onesearch.library.example-college.edu";
  const otherOpenURL = `${otherOrigin}/openurl?ctx=other`;
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: landingJobID,
        tab_id: landingTabID,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        requires_auth: true,
        provider_hosts: [PROVIDER_HOST],
      },
      {
        job_id: peerJobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        requires_auth: true,
        provider_hosts: [PROVIDER_HOST],
      },
      {
        job_id: otherOriginJobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        requires_auth: true,
        provider_hosts: ["link.springer.com"],
      },
    ],
    offerURLs: { [landingJobID]: OPENURL, [peerJobID]: OPENURL, [otherOriginJobID]: otherOpenURL },
  });
  h.tabs.live.set(landingTabID, { id: landingTabID, url: OPENURL });
  const dirtied: string[] = [];
  const probedForeground: (string | undefined)[] = [];
  const probedAutomatically: string[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    markDirty: async (origin: string) => {
      dirtied.push(origin);
    },
    // issue C: recordInstitutionalSession fires from a tab NAVIGATION, an
    // automatic path, not an operator action, so it must not take the 2s
    // operator floor (MIN_FOREGROUND_PROBE_SPACING_MS in keepalive.ts). This
    // spy still defines probeForeground so a regression back to the
    // operator entry point fails loudly instead of silently no-op'ing.
    probeForeground: async (origin?: string) => {
      probedForeground.push(origin);
    },
    probeOriginAutomatically: async (origin: string) => {
      probedAutomatically.push(origin);
    },
    notifyConfiguredOriginsChanged: () => {},
    noteResolverNavigation: () => {},
    noteResolverActivated: () => {},
    noteTabRemoved: () => {},
    onWake: async () => {},
  } as unknown as KeepaliveManager);

  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://resolver.example.edu", otherOrigin] }));

  await h.tabs.onUpdated.emit(landingTabID, { url: OPENURL, status: "complete" }, { id: landingTabID, url: OPENURL });

  // A landing is still only a reason to look again, never itself a verdict:
  // exactly one dirty-mark and one probe, scoped to the landing's own
  // origin, and routed through the automatic entry point — never the
  // operator-floor probeForeground (issue C).
  expect(dirtied).toEqual(["https://resolver.example.edu"]);
  expect(probedAutomatically).toEqual(["https://resolver.example.edu"]);
  expect(probedForeground).toEqual([]);

  // But papio itself drove this tab past authentication onto a page resolving
  // to a configured origin — first-hand evidence for THAT origin, so its own
  // queued sibling opens immediately, the same as any other release.
  expect(h.backend.store.activeJobs.find((job) => job.job_id === peerJobID)).toMatchObject({
    status: "accepted",
  });
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);

  // A different institution's queued handoff is never touched by this.
  expect(h.backend.store.activeJobs.find((job) => job.job_id === otherOriginJobID)).toMatchObject({
    status: "queued",
    tab_id: -1,
  });

  // The landing tab itself was never on an authentication page, so
  // reloadAuthenticationHandoffs found nothing to reload.
  expect(h.tabs.reloaded).toEqual([]);
});

test("reloadAuthenticationHandoffs reloads only the evidenced origin's tabs, leaving another institution's IdP tab alone", async () => {
  const jobA = "job_reload_scope_a";
  const jobB = "job_reload_scope_b";
  const originA = "https://resolver.example.edu";
  const originB = "https://onesearch.library.example-college.edu";
  const idpA = "https://idp.example.edu/sso";
  const idpB = "https://shibboleth.example-college.edu/idp/sso";
  const tabA = 201;
  const tabB = 202;
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      { job_id: jobA, tab_id: tabA, offered_at: 1, expires_at: 2, status: "accepted", provider_hosts: [PROVIDER_HOST] },
      { job_id: jobB, tab_id: tabB, offered_at: 1, expires_at: 2, status: "accepted", provider_hosts: ["link.springer.com"] },
    ],
    offerURLs: { [jobA]: `${originA}/openurl?ctx=a`, [jobB]: `${originB}/openurl?ctx=b` },
  });
  h.tabs.live.set(tabA, { id: tabA, url: idpA });
  h.tabs.live.set(tabB, { id: tabB, url: idpB });
  await h.bridge.start();

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, originA, "live_tab"));

  expect(h.tabs.reloaded).toEqual([tabA]);
});

test("requestSessionSignIn rejects a non-configured origin once hello_ack has landed, and accepts a configured one", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: ["https://resolver.example.edu"] }));

  await expect(h.bridge.requestSessionSignIn("https://unknown.example-college.edu")).resolves.toEqual({
    ok: false,
    error: { code: "resolver_unavailable", message: "This institution is not currently configured" },
  });
  expect(h.tabs.created).toEqual([]);

  const reply = await h.bridge.requestSessionSignIn("https://resolver.example.edu");
  expect(reply).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([{ url: "https://resolver.example.edu", active: true }]);
});

test("the 45-second forced-release fallback still opens a queued handoff with zero authentication evidence", async () => {
  // ADR-0009's autonomous-retry fallback is ratified and deliberately bypasses
  // evidence for exactly one forced job; Commit C must not regress it.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_forced_release_active"));
  await h.port.inbound(jobOffer("job_forced_release_queued"));
  const fallback = h.timers.find((timer) => timer.ms === 45_000);
  expect(fallback).toBeDefined();

  await fallback?.fn();

  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_forced_release_queued")).toMatchObject({
    status: "accepted",
  });
  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
  ]);
});

test("a hello_ack immediately notifies the keepalive manager of configured-origin membership", async () => {
  const h = makeHarness();
  const notifications: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    notifyConfiguredOriginsChanged: () => {
      notifications.push(notifications.length);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  expect(notifications).toEqual([]);

  await h.port.inbound(helloAck({ resolver_origins: ["https://resolver.example.edu"] }));

  expect(notifications).toEqual([0]);
});

// One login tab per institution: a cross-job federated-login registry keyed
// by a CLAIM KEY — the federated-login destination (IdP/DS) origin PLUS the
// offer's entityID — so three papers needing the same institution share a
// single sign-in tab instead of each opening its own. The origin alone is
// not enough: a shared WAYF/Discovery-Service host serving many
// institutions exposes exactly one origin, distinguished only by entityID
// (the real ProQuest adapter is exactly this shape).
const FED_ENTITY_ID = "https://idp.example.edu/entity";
const FED_LOGIN_TEMPLATE =
  "https://login.idp.example.edu/Shibboleth.sso/DS?entityID={entityID}&target=https://login.idp.example.edu/home";
const FED_LOGIN_URL = FED_LOGIN_TEMPLATE.replace("{entityID}", encodeURIComponent(FED_ENTITY_ID));
const FED_IDP_ORIGIN = new URL(FED_LOGIN_URL).origin;
let FED_CLAIM_KEY = "";
const FED_PROVIDER_LOGIN_URL = `https://${PROVIDER_HOST}/login-wall`;
const RESOLVER_ORIGIN = "https://resolver.example.edu";
// A second institution sharing the exact same DS host as FED_ENTITY_ID above
// — only entityID and the offer's own resolver origin differ.
const FED_ENTITY_ID_B = "https://idp-b.example.edu/entity";
const RESOLVER_ORIGIN_B = "https://resolver-b.example.edu";
const OPENURL_B = "https://resolver-b.example.edu/openurl?ctx=xyz";
let FED_CLAIM_KEY_B = "";
async function ensureFedClaimKeys(): Promise<void> {
  if (FED_CLAIM_KEY !== "" && FED_CLAIM_KEY_B !== "") return;
  FED_CLAIM_KEY = await federatedLoginClaimKey(FED_IDP_ORIGIN, FED_ENTITY_ID);
  FED_CLAIM_KEY_B = await federatedLoginClaimKey(FED_IDP_ORIGIN, FED_ENTITY_ID_B);
}
const FED_LOGIN_SPEC: AdapterSpec = {
  id: "fedlogin",
  version: "1.0.0",
  hosts: [PROVIDER_HOST],
  classify: [{ kind: "login", all: ["#login-form"] }],
  federatedLogin: FED_LOGIN_TEMPLATE,
};
const FED_LOGIN_VERDICT: PageVerdict = {
  kind: "login",
  adapter_id: "fedlogin",
  adapter_version: "1.0.0",
  evidence: [],
};

function fedLoginOffer(jobID: string, entityID: string = FED_ENTITY_ID, openurl: string = OPENURL): unknown {
  const offer = jobOffer(jobID, openurl) as { payload: Record<string, unknown> };
  offer.payload["login_entity_id"] = entityID;
  return offer;
}

function makeFedLoginHarness(seed?: StoreShape): Harness {
  const h = makeHarness(
    seed ?? { ...emptyStore(), authEvidenceByOrigin: { [RESOLVER_ORIGIN]: 1_700_000_000_000 } },
  );
  h.deps.adapterSpecs.push(FED_LOGIN_SPEC);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (injection.func === interpret) return [{ result: FED_LOGIN_VERDICT }];
    return [];
  };
  return h;
}

/** Keeps the fake tab's live URL in sync with a simulated landing on the
 * shared provider login wall — FakeTabs never updates `.live` on its own
 * from an emitted onUpdated event, only from a real `.update()` call. */
async function landOnFedProviderWall(h: Harness, tabID: number): Promise<void> {
  h.tabs.live.set(tabID, { id: tabID, url: FED_PROVIDER_LOGIN_URL });
  await h.tabs.onUpdated.emit(
    tabID,
    { url: FED_PROVIDER_LOGIN_URL, status: "complete" },
    { id: tabID, url: FED_PROVIDER_LOGIN_URL },
  );
}

/** Offers three jobs for the same institution (evidence pre-seeded so all
 * three open real handoff tabs, HANDOFF_DRIVE_LIMIT capping two concurrent),
 * then lands every one of them on the shared provider login wall. Returns
 * the harness with job_fed_a as the federated-login owner and job_fed_b /
 * job_fed_c parked in waiting_for_session. */
async function primeFedLoginTriad(): Promise<{ h: Harness; tabA: number; tabB: number; tabC: number }> {
  await ensureFedClaimKeys();
  const h = makeFedLoginHarness();
  await h.bridge.start();
  for (const jobID of ["job_fed_a", "job_fed_b", "job_fed_c"]) {
    await h.port.inbound(fedLoginOffer(jobID));
  }
  const tabA = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_a")?.tab_id ?? -1;
  const tabB = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA);
  await landOnFedProviderWall(h, tabB);
  // job_fed_c was queued behind the governor; parking job_fed_b just freed
  // its slot, so it now has a real tab too.
  const tabC = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabC);
  return { h, tabA, tabB, tabC };
}

test("one login tab per institution: two siblings park in waiting_for_session instead of opening their own IdP tab", async () => {
  const { h, tabA, tabB, tabC } = await primeFedLoginTriad();

  expect(tabA).toBeGreaterThanOrEqual(0);
  expect(tabB).toBeGreaterThanOrEqual(0);
  expect(tabC).toBeGreaterThanOrEqual(0);

  // Exactly one tab ever navigates to the IdP.
  const idpNavs = h.tabs.navigations.filter((n) => n.url === FED_LOGIN_URL);
  expect(idpNavs).toEqual([{ tabID: tabA, url: FED_LOGIN_URL }]);

  const a = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_a");
  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  expect(a?.waiting_for_session).toBeUndefined();
  expect(b).toMatchObject({
    waiting_for_session: true,
    waiting_for_session_key: FED_CLAIM_KEY,
    parked_with_tab: true,
    tab_id: tabB,
    status: "auth_pending",
  });
  expect(c).toMatchObject({
    waiting_for_session: true,
    waiting_for_session_key: FED_CLAIM_KEY,
    parked_with_tab: true,
    tab_id: tabC,
    status: "auth_pending",
  });

  // The other two tabs are left exactly where they were — the provider's
  // login wall, never navigated to the IdP.
  expect(h.tabs.live.get(tabB)?.url).toBe(FED_PROVIDER_LOGIN_URL);
  expect(h.tabs.live.get(tabC)?.url).toBe(FED_PROVIDER_LOGIN_URL);

  expect(h.backend.store.federatedLoginOwners).toEqual({
    [FED_CLAIM_KEY]: { jobID: "job_fed_a", tabID: tabA },
  });
});
test("federated claim storage is opaque and one owner supplies the only sign-in badge blocker", async () => {
  const { h } = await primeFedLoginTriad();
  const persisted = JSON.stringify(h.backend.store);
  expect(persisted).not.toContain(FED_IDP_ORIGIN);
  expect(persisted).not.toContain(FED_ENTITY_ID);
  h.backend.store.connectionStatus = "connected";
  await h.bridge.syncConnectionBadge("connected");
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe("papio: 1 paper waiting on your institution sign-in");
});

test("startup drops legacy raw claim keys and resumes their waiters ownerlessly", async () => {
  const { h, tabA, tabB, tabC } = await primeFedLoginTriad();
  const legacyKey = JSON.stringify([FED_IDP_ORIGIN, FED_ENTITY_ID]);
  const seed = JSON.parse(JSON.stringify(h.backend.store)) as StoreShape;
  seed.federatedLoginOwners = { [legacyKey]: { jobID: "job_fed_a", tabID: tabA } };
  for (const jobID of ["job_fed_b", "job_fed_c"]) {
    const job = seed.activeJobs.find((candidate) => candidate.job_id === jobID);
    if (job !== undefined) job.waiting_for_session_key = legacyKey;
  }
  const restarted = makeFedLoginHarness(seed);
  restarted.tabs.live.set(tabA, { id: tabA, url: FED_LOGIN_URL });
  restarted.tabs.live.set(tabB, { id: tabB, url: FED_PROVIDER_LOGIN_URL });
  restarted.tabs.live.set(tabC, { id: tabC, url: FED_PROVIDER_LOGIN_URL });
  await restarted.bridge.start();
  expect(restarted.backend.store.federatedLoginOwners).toEqual({});
  expect(restarted.backend.store.activeJobs.filter((job) => job.waiting_for_session === true)).toHaveLength(0);
  const persisted = JSON.stringify(restarted.backend.store);
  expect(persisted).not.toContain(FED_IDP_ORIGIN);
  expect(persisted).not.toContain(FED_ENTITY_ID);
});

test("repeated decisive evidence events cost zero navigations for a waiter whose claim owner is still live", async () => {
  const { h, tabA, tabB, tabC } = await primeFedLoginTriad();
  const originalDeadline = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b")?.waiting_deadline;
  expect(originalDeadline).toBeDefined();
  h.tabs.navigations.splice(0);
  const createdBefore = h.tabs.created.length;

  // Repeated probes fire the same decisive evidence multiple times (probes
  // fire repeatedly in practice) while job_fed_a — the claim's owner — is
  // still, for all this evidence proves, mid sign-in on the IdP. None of it
  // may resume job_fed_b or job_fed_c: resuming would only re-park them a
  // moment later, at the cost of a real navigation and a governor slot.
  for (let i = 0; i < 3; i += 1) {
    await h.bridge.recordFreshSessionEvidence(freshEvidence(h, RESOLVER_ORIGIN));
  }

  expect(h.tabs.navigations).toEqual([]);
  expect(h.tabs.created.length).toBe(createdBefore);
  expect(h.tabs.activated).toEqual([]);
  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  expect(b?.waiting_for_session).toBe(true);
  expect(b?.waiting_deadline).toBe(originalDeadline);
  expect(c?.waiting_for_session).toBe(true);
  // The claim itself is untouched too — still exactly the owner it started
  // with.
  expect(h.backend.store.federatedLoginOwners).toEqual({
    [FED_CLAIM_KEY]: { jobID: "job_fed_a", tabID: tabA },
  });
});

test("the claim owner leaving the IdP resumes its waiters exactly once, through the retirement chokepoint — never the prior evidence events", async () => {
  const { h, tabA, tabB, tabC } = await primeFedLoginTriad();
  // Three prior evidence events, all no-ops per the test above.
  for (let i = 0; i < 3; i += 1) {
    await h.bridge.recordFreshSessionEvidence(freshEvidence(h, RESOLVER_ORIGIN));
  }
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b")?.waiting_for_session).toBe(true);
  h.tabs.navigations.splice(0);

  // job_fed_a finally leaves the IdP and lands back on the provider.
  // clearFederatedLoginOwnerForTab retires the claim, which resumes every
  // one of that exact claim's waiters — the ONLY thing that actually moved
  // job_fed_b and job_fed_c, not the three evidence events before it.
  h.tabs.live.set(tabA, { id: tabA, url: FED_PROVIDER_LOGIN_URL });
  await h.tabs.onUpdated.emit(
    tabA,
    { url: FED_PROVIDER_LOGIN_URL, status: "complete" },
    { id: tabA, url: FED_PROVIDER_LOGIN_URL },
  );

  // a1 itself never frees a governor slot merely by landing back on the
  // provider (it keeps driving toward its own resolution — the same job
  // that owned the claim, not resumed by it), so exactly ONE of its two
  // waiters claims the one slot this retirement made available; the other
  // drops its park markers too (intent to resume expressed) but stays
  // queued behind the governor until a slot genuinely frees.
  expect(h.backend.store.federatedLoginOwners).toEqual({});
  const resumedNav = h.tabs.navigations.find((n) => n.tabID === tabB || n.tabID === tabC);
  expect(resumedNav?.url).toBe(OPENURL);
  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  expect(b?.waiting_for_session).toBe(false);
  expect(c?.waiting_for_session).toBe(false);
  // Exactly one navigation total: the retirement event fired once, and a
  // second, redundant onProvider re-entry for the same already-cleared
  // claim must never re-navigate either waiter again.
  expect(h.tabs.navigations).toHaveLength(1);
});

test("a service-worker restart with a LIVE claim owner re-arms the wait timer from the persisted deadline, not a fresh one", async () => {
  const h = makeFedLoginHarness();
  await h.bridge.start();
  await h.port.inbound(fedLoginOffer("job_wait_a"));
  await h.port.inbound(fedLoginOffer("job_wait_b"));
  const tabA = h.backend.store.activeJobs.find((j) => j.job_id === "job_wait_a")?.tab_id ?? -1;
  const tabB = h.backend.store.activeJobs.find((j) => j.job_id === "job_wait_b")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA);
  await landOnFedProviderWall(h, tabB);
  const parkedAt = h.clock.now;
  const bBeforeRestart = h.backend.store.activeJobs.find((j) => j.job_id === "job_wait_b");
  expect(bBeforeRestart?.waiting_for_session).toBe(true);
  expect(bBeforeRestart?.waiting_deadline).toBe(parkedAt + 600_000);

  // Two minutes pass before a service-worker restart. Both tabs survive —
  // job_wait_a (the claim owner) is still live, still on the IdP.
  h.clock.now += 120_000;
  const restarted = makeHarness(JSON.parse(JSON.stringify(h.backend.store)) as StoreShape);
  restarted.clock.now = h.clock.now;
  restarted.deps.adapterSpecs.push(FED_LOGIN_SPEC);
  restarted.deps.permissions.contains = async () => true;
  restarted.deps.scripting.executeScript = h.deps.scripting.executeScript;
  restarted.tabs.live.set(tabA, { id: tabA, url: FED_LOGIN_URL });
  restarted.tabs.live.set(tabB, { id: tabB, url: FED_PROVIDER_LOGIN_URL });

  await restarted.bridge.start();

  // The claim survives the restart too (its owner's tab is still live and
  // still on the IdP), and the persisted deadline is untouched.
  expect(restarted.backend.store.federatedLoginOwners?.[FED_CLAIM_KEY]?.jobID).toBe("job_wait_a");
  const bAfterRestart = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_wait_b");
  expect(bAfterRestart?.waiting_for_session).toBe(true);
  expect(bAfterRestart?.waiting_deadline).toBe(parkedAt + 600_000);

  const remaining = parkedAt + 600_000 - restarted.clock.now;
  expect(remaining).toBe(480_000);
  const rearmed = restarted.timers.find((t) => t.ms === remaining);
  expect(rearmed).toBeDefined();

  restarted.clock.now += remaining;
  await rearmed?.fn();

  const bDemoted = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_wait_b");
  expect(bDemoted).toMatchObject({ waiting_for_session: false, parked_with_tab: true, tab_id: tabB });
  expect(bDemoted?.waiting_for_session_key).toBeUndefined();
  expect(bDemoted?.waiting_deadline).toBeUndefined();
});

test("re-parking a resumed waiter keeps its original wait deadline instead of restarting the budget", async () => {
  const h = makeFedLoginHarness();
  await h.bridge.start();
  await h.port.inbound(fedLoginOffer("job_redeadline_a"));
  await h.port.inbound(fedLoginOffer("job_redeadline_b"));
  const tabA = h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_a")?.tab_id ?? -1;
  const tabB = h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_b")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA); // owns the claim
  await landOnFedProviderWall(h, tabB); // parks
  const originalDeadline = h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_b")?.waiting_deadline;
  expect(originalDeadline).toBe(h.clock.now + 600_000);

  // job_redeadline_a's tab closes (abandoned mid sign-in): retires its
  // claim, which resumes job_redeadline_b through the chokepoint — its own
  // tab is reused and redriven back to its offer URL.
  await h.tabs.onRemoved.emit(tabA, { isWindowClosing: false });
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_b")?.waiting_for_session).toBe(false);
  expect(h.tabs.navigations).toContainEqual({ tabID: tabB, url: OPENURL });

  // A third job claims the now-empty claim BEFORE job_redeadline_b's
  // redrive lands, so b's re-classify finds a live owner again and parks a
  // second time, under the SAME claim key.
  await h.port.inbound(fedLoginOffer("job_redeadline_c"));
  const tabC = h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_c")?.tab_id ?? -1;
  expect(tabC).toBeGreaterThanOrEqual(0);
  await landOnFedProviderWall(h, tabC);
  expect(h.backend.store.federatedLoginOwners?.[FED_CLAIM_KEY]?.jobID).toBe("job_redeadline_c");

  await landOnFedProviderWall(h, tabB);
  const bReparked = h.backend.store.activeJobs.find((j) => j.job_id === "job_redeadline_b");
  expect(bReparked?.waiting_for_session).toBe(true);
  expect(bReparked?.waiting_for_session_key).toBe(FED_CLAIM_KEY);
  // The original deadline survives the entire resume-then-re-park cycle
  // untouched — re-parking never grants a fresh budget.
  expect(bReparked?.waiting_deadline).toBe(originalDeadline);
});

test("fresh session evidence for a different institution resumes no waiting_for_session park", async () => {
  const { h, tabB, tabC } = await primeFedLoginTriad();
  h.tabs.navigations.splice(0);

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, "https://other-library.example.edu"));

  expect(h.tabs.navigations.some((n) => n.tabID === tabB || n.tabID === tabC)).toBe(false);
  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  expect(b?.waiting_for_session).toBe(true);
  expect(c?.waiting_for_session).toBe(true);
  // The registry claim for the ACTUAL institution is untouched too — the
  // scope guard cuts both ways.
  expect(h.backend.store.federatedLoginOwners).not.toEqual({});
});

test("two institutions sharing a federated-login (DS) host never collide: distinct claim keys, and evidence for one never resumes the other's waiter", async () => {
  await ensureFedClaimKeys();
  const h = makeFedLoginHarness({
    ...emptyStore(),
    authEvidenceByOrigin: {
      [RESOLVER_ORIGIN]: 1_700_000_000_000,
      [RESOLVER_ORIGIN_B]: 1_700_000_000_000,
    },
  });
  await h.bridge.start();

  // Institution A: three jobs — a1 owns claim A, a2 and a3 park under it.
  for (const jobID of ["job_x_a1", "job_x_a2", "job_x_a3"]) {
    await h.port.inbound(fedLoginOffer(jobID, FED_ENTITY_ID, OPENURL));
  }
  const tabA1 = h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a1")?.tab_id ?? -1;
  const tabA2 = h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a2")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA1);
  await landOnFedProviderWall(h, tabA2);
  const tabA3 = h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a3")?.tab_id ?? -1;
  expect(tabA3).toBeGreaterThanOrEqual(0); // a2 parking freed the slot a3 needed
  await landOnFedProviderWall(h, tabA3);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a2")?.waiting_for_session).toBe(true);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a3")?.waiting_for_session).toBe(true);

  // Institution B: the exact same DS host, a DIFFERENT entityID and offer
  // origin. Its first job takes the slot a3's park just freed.
  await h.port.inbound(fedLoginOffer("job_x_b1", FED_ENTITY_ID_B, OPENURL_B));
  const tabB1 = h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b1")?.tab_id ?? -1;
  expect(tabB1).toBeGreaterThanOrEqual(0);
  await landOnFedProviderWall(h, tabB1);

  // B never blocked on A's claim: b1 became the OWNER of its OWN claim
  // (navigated straight to the IdP), not a waiter behind a1's.
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b1")?.waiting_for_session).toBeUndefined();
  expect(h.tabs.navigations.filter((n) => n.tabID === tabB1)).toHaveLength(1);

  // Two distinct claims held simultaneously despite sharing one DS host.
  expect(Object.keys(h.backend.store.federatedLoginOwners ?? {}).sort()).toEqual(
    [FED_CLAIM_KEY, FED_CLAIM_KEY_B].sort(),
  );

  // Free a slot for institution B's second job via a1's own pre-existing
  // 3-minute drive timeout — a1 simply never came back, unrelated to the
  // federated-login feature itself.
  await h.port.inbound(fedLoginOffer("job_x_b2", FED_ENTITY_ID_B, OPENURL_B));
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b2")?.tab_id).toBe(-1);
  const a1Timeout = h.timers.find((t) => t.ms === 180_000);
  expect(a1Timeout).toBeDefined();
  await a1Timeout?.fn();
  const tabB2 = h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b2")?.tab_id ?? -1;
  expect(tabB2).toBeGreaterThanOrEqual(0);
  await landOnFedProviderWall(h, tabB2);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b2")?.waiting_for_session).toBe(true);

  // The scope guard AND the live-owner guard together: evidence for
  // institution A costs zero navigations for EITHER institution's waiter
  // while both claim owners (job_x_a1, job_x_b1) are still live — A's own
  // waiters would only re-park behind job_x_a1, and institution B's waiter
  // was never in scope for institution A's evidence to begin with.
  h.tabs.navigations.splice(0);
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, RESOLVER_ORIGIN));
  expect(h.tabs.navigations).toEqual([]);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a2")?.waiting_for_session).toBe(true);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_a3")?.waiting_for_session).toBe(true);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_x_b2")?.waiting_for_session).toBe(true);
});

test("the federated-login registry clears when the owning tab closes, so the next login attempt navigates normally", async () => {
  const h = makeFedLoginHarness();
  await h.bridge.start();
  await h.port.inbound(fedLoginOffer("job_fed_close_a"));
  const tabA = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA);
  expect(h.backend.store.federatedLoginOwners?.[FED_CLAIM_KEY]?.jobID).toBe("job_fed_close_a");

  // The tab at the login page closes (operator gives up, browser reclaims it —
  // the mechanism does not care which).
  await h.tabs.onRemoved.emit(tabA, { isWindowClosing: false });
  expect(h.backend.store.federatedLoginOwners ?? {}).toEqual({});

  // A second job needing the same institution opens its own tab and, on
  // hitting the same login wall, navigates straight to the IdP: nothing is
  // left to defer to.
  await h.port.inbound(fedLoginOffer("job_fed_close_b"));
  const tabB = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_close_b")?.tab_id ?? -1;
  expect(tabB).toBeGreaterThanOrEqual(0);
  await landOnFedProviderWall(h, tabB);

  expect(h.tabs.navigations).toContainEqual({ tabID: tabB, url: FED_LOGIN_URL });
  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_close_b");
  expect(b?.waiting_for_session).toBeUndefined();
  expect(h.backend.store.federatedLoginOwners).toEqual({
    [FED_CLAIM_KEY]: { jobID: "job_fed_close_b", tabID: tabB },
  });
});

test("a service-worker restart with a dead claim owner requeues its waiters instead of leaving them parked forever", async () => {
  const { h: first, tabB, tabC } = await primeFedLoginTriad();
  // job_fed_a (the claim owner) is NOT re-added to the restarted harness's
  // live tabs — its tab closed while this worker was asleep, the one case
  // onTabRemoved can never observe directly (MV3 tears the worker down
  // without firing it for tabs that close while the browser itself stays
  // open). job_fed_b and job_fed_c's tabs DO survive.
  const restarted = makeHarness(JSON.parse(JSON.stringify(first.backend.store)) as StoreShape);
  restarted.deps.adapterSpecs.push(FED_LOGIN_SPEC);
  restarted.deps.permissions.contains = async () => true;
  restarted.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "normal" } }];
    if (injection.func === interpret) return [{ result: FED_LOGIN_VERDICT }];
    return [];
  };
  restarted.tabs.live.set(tabB, { id: tabB, url: FED_PROVIDER_LOGIN_URL });
  restarted.tabs.live.set(tabC, { id: tabC, url: FED_PROVIDER_LOGIN_URL });

  await restarted.bridge.start();

  // job_fed_a's dead tab reconciles like any other pre-download job whose
  // tab vanished: back to an ordinary queued handoff, park markers cleared —
  // never left stale.
  const a = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_fed_a");
  expect(a).toMatchObject({ status: "queued", tab_id: -1, waiting_for_session: false, parked_with_tab: false });
  expect(a?.waiting_for_session_key).toBeUndefined();

  // Its now-dead claim retired at startup (reconcileFederatedLoginOwners),
  // which itself resumed every waiter of that exact claim — never left
  // ownerless.
  expect(restarted.backend.store.federatedLoginOwners ?? {}).toEqual({});
  const b = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  expect(b?.waiting_for_session).toBe(false);
  expect(c?.waiting_for_session).toBe(false);
  expect(restarted.tabs.navigations).toContainEqual({ tabID: tabB, url: OPENURL });
  expect(restarted.tabs.navigations).toContainEqual({ tabID: tabC, url: OPENURL });
  const restartedInternals = restarted.bridge as unknown as { handoffDrives: Map<string, unknown> };
  expect(restartedInternals.handoffDrives.size).toBe(2);
  expect([...restartedInternals.handoffDrives.keys()].sort()).toEqual(["job_fed_b", "job_fed_c"]);
});

test("a restart with a dead claim owner never resumes a waiter whose own wait deadline already expired — it demotes instead", async () => {
  const h = makeFedLoginHarness();
  await h.bridge.start();
  await h.port.inbound(fedLoginOffer("job_race_a"));
  await h.port.inbound(fedLoginOffer("job_race_expired"));
  const tabA = h.backend.store.activeJobs.find((j) => j.job_id === "job_race_a")?.tab_id ?? -1;
  const tabExpired = h.backend.store.activeJobs.find((j) => j.job_id === "job_race_expired")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA); // owns the claim
  await landOnFedProviderWall(h, tabExpired); // parks — deadline = T0 + 600_000
  const expiredDeadline = h.backend.store.activeJobs.find(
    (j) => j.job_id === "job_race_expired",
  )?.waiting_deadline;
  expect(expiredDeadline).toBeDefined();

  // Much later, a second job for the same institution arrives and also
  // parks behind the still-live owner — its own wait budget starts fresh,
  // well after job_race_expired's has already elapsed.
  h.clock.now += 900_000;
  await h.port.inbound(fedLoginOffer("job_race_live"));
  const tabLive = h.backend.store.activeJobs.find((j) => j.job_id === "job_race_live")?.tab_id ?? -1;
  expect(tabLive).toBeGreaterThanOrEqual(0);
  await landOnFedProviderWall(h, tabLive);
  const liveDeadline = h.backend.store.activeJobs.find((j) => j.job_id === "job_race_live")?.waiting_deadline;
  expect(liveDeadline).toBe(h.clock.now + 600_000);
  expect(h.clock.now).toBeGreaterThan(expiredDeadline!);

  // Restart. job_race_a's tab (the claim owner) is dead — closed while this
  // worker was asleep. Both waiters' tabs survive: job_race_expired's own
  // deadline has ALREADY passed by restart time; job_race_live's has not.
  const restarted = makeHarness(JSON.parse(JSON.stringify(h.backend.store)) as StoreShape);
  restarted.clock.now = h.clock.now;
  restarted.deps.adapterSpecs.push(FED_LOGIN_SPEC);
  restarted.deps.permissions.contains = async () => true;
  restarted.deps.scripting.executeScript = h.deps.scripting.executeScript;
  restarted.tabs.live.set(tabExpired, { id: tabExpired, url: FED_PROVIDER_LOGIN_URL });
  restarted.tabs.live.set(tabLive, { id: tabLive, url: FED_PROVIDER_LOGIN_URL });

  await restarted.bridge.start();

  // reconcileFederatedLoginOwners retires job_race_a's dead-owner claim
  // BEFORE reconcileSessionWaitTimeouts ever runs — that retirement's own
  // resume pass must not hand job_race_expired the navigation and drive
  // slot its already-past deadline promised it would never get, nor leave
  // it holding a stale, already-spent deadline.
  const expiredAfter = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_race_expired");
  expect(expiredAfter).toMatchObject({ waiting_for_session: false, parked_with_tab: true, tab_id: tabExpired });
  expect(expiredAfter?.waiting_deadline).toBeUndefined();
  expect(restarted.tabs.navigations.some((n) => n.tabID === tabExpired)).toBe(false);

  // job_race_live's own deadline had not passed: the same claim retirement
  // resumes it normally.
  const liveAfter = restarted.backend.store.activeJobs.find((j) => j.job_id === "job_race_live");
  expect(liveAfter?.waiting_for_session).toBe(false);
  expect(liveAfter?.waiting_deadline).toBe(liveDeadline);
  expect(restarted.tabs.navigations).toContainEqual({ tabID: tabLive, url: OPENURL });
});

test("the owner completing its own login resumes waiting siblings even when institution evidence is already warm", async () => {
  const h = makeFedLoginHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { [RESOLVER_ORIGIN]: 1_700_000_000_000 },
    resolverOrigins: [RESOLVER_ORIGIN],
  });
  await h.bridge.start();
  const offerA = fedLoginOffer("job_fed_owner_a") as { payload: Record<string, unknown> };
  offerA.payload["requires_auth"] = true;
  const offerB = fedLoginOffer("job_fed_owner_b") as { payload: Record<string, unknown> };
  offerB.payload["requires_auth"] = true;
  await h.port.inbound(offerA);
  await h.port.inbound(offerB);
  const tabA = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_owner_a")?.tab_id ?? -1;
  const tabB = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_owner_b")?.tab_id ?? -1;
  await landOnFedProviderWall(h, tabA);
  await landOnFedProviderWall(h, tabB);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_owner_b")?.waiting_for_session).toBe(true);

  // job_fed_a's tab, already navigated to the IdP by maybeRouteFederatedLogin,
  // now reports its own follow-up navigation landing there — Chrome's real
  // onUpdated firing after the programmatic tabs.update papio issued.
  await h.tabs.onUpdated.emit(tabA, { url: FED_LOGIN_URL, status: "complete" }, { id: tabA, url: FED_LOGIN_URL });
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_owner_a")?.status).toBe("auth_pending");

  h.tabs.navigations.splice(0);
  // job_fed_a completes sign-in and lands back on the provider. No
  // recordFreshSessionEvidence call ever fires in this test — evidence for
  // RESOLVER_ORIGIN was already warm from t=0, so ONLY job_fed_a finishing
  // its own login (recordInstitutionalSession) can resume job_fed_owner_b.
  const returnURL = `https://${PROVIDER_HOST}/stable/returned`;
  h.tabs.live.set(tabA, { id: tabA, url: returnURL });
  await h.tabs.onUpdated.emit(tabA, { url: returnURL, status: "complete" }, { id: tabA, url: returnURL });

  expect(h.tabs.navigations).toContainEqual({ tabID: tabB, url: OPENURL });
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_owner_b")?.waiting_for_session).toBe(false);
});

test("a waiting_for_session park past its wait budget demotes to an ordinary parked_with_tab park, not an invisible indefinite wait", async () => {
  const { h, tabB, tabC } = await primeFedLoginTriad();
  const navigationsBefore = h.tabs.navigations.length;
  const waitTimers = h.timers.filter((t) => t.ms === 600_000);
  expect(waitTimers.length).toBe(2); // one per waiting sibling (job_fed_b, job_fed_c)
  for (const timer of waitTimers) await timer.fn();

  const b = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_b");
  const c = h.backend.store.activeJobs.find((j) => j.job_id === "job_fed_c");
  // Demoted to the pre-feature presentation: still parked with its tab
  // preserved and operator-actionable, just no longer framed as "waiting on
  // another paper's sign-in" — that framing would now be a lie.
  expect(b).toMatchObject({ waiting_for_session: false, parked_with_tab: true, tab_id: tabB });
  expect(c).toMatchObject({ waiting_for_session: false, parked_with_tab: true, tab_id: tabC });
  expect(b?.waiting_for_session_key).toBeUndefined();
  expect(c?.waiting_for_session_key).toBeUndefined();
  // Demotion only relabels the existing park — it never drives anything.
  expect(h.tabs.navigations.length).toBe(navigationsBefore);
});

test("a challenge-parked handoff stays parked through fresh session evidence for its own institution (scope guard)", async () => {
  let challenge = true;
  const h = makeHarness({ ...emptyStore(), authEvidenceByOrigin: { [RESOLVER_ORIGIN]: 1_700_000_000_000 } });
  useUnknownProviderClassifier(h, () => challenge);
  const tabID = await classifyProviderUnknown(h, "job_challenge_evidence");

  expect(h.backend.store.activeJobs[0]).toMatchObject({ challenge_blocked: true, parked_with_tab: true });
  expect(h.backend.store.activeJobs[0]?.waiting_for_session).toBeUndefined();
  h.tabs.navigations.splice(0);

  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, RESOLVER_ORIGIN));

  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_challenge_evidence");
  expect(job?.challenge_blocked).toBe(true);
  expect(job?.parked_with_tab).toBe(true);
  expect(job?.waiting_for_session).toBeUndefined();
  expect(h.tabs.navigations).toEqual([]);
  expect(h.tabs.live.has(tabID)).toBe(true);
});

test("papio.pageBulk.grabStatus pulls a structured durable result", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } };
  await h.bridge.start();
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }));
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabStatus", request: { grab_id: "grab-status-0001" } },
    sender,
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_status_request");
  await h.port.inbound(
    nativeResult("pdf_grab_status_result", {
      request_id: requestFrame.payload["request_id"] as string,
      grab_id: "grab-status-0001",
      state: "job_created",
      outcome: "job_created",
      job_id: "job_00000001",
    }),
  );
  await expect(replyPromise).resolves.toEqual({
    ok: true,
    grab_id: "grab-status-0001",
    state: "job_created",
    outcome: "job_created",
    job_id: "job_00000001",
  });
});

test("runtime dispatcher rejection always sends a structured connection_lost reply", async () => {
  let called = false;
  let response: unknown;
  respondToRuntimePromise(
    Promise.reject({ code: "connection_lost" }),
    (reply) => {
      called = true;
      response = reply;
    },
  );
  await Promise.resolve();
  await Promise.resolve();
  expect(called).toBe(true);
  expect(response).toEqual({
    ok: false,
    error: "connection_lost",
    message: "papio lost its connection to the daemon and is retrying…",
  });
});

test("runtime message registry stays equal to the handler type chain", () => {
  const source = readFileSync(new URL("../src/background.ts", import.meta.url), "utf8");
  const handlerStart = source.indexOf("export async function handleInboxRuntimeMessage");
  const dispatcherStart = source.indexOf("chrome.runtime.onMessage.addListener", handlerStart);
  expect(handlerStart).toBeGreaterThanOrEqual(0);
  expect(dispatcherStart).toBeGreaterThan(handlerStart);
  const handlerSource = source.slice(handlerStart, dispatcherStart);
  const handlerTypes = new Set<string>(
    [...handlerSource.matchAll(/type\s*(?:===|!==)\s*"(papio\.[^"]+)"/g)]
      .map((match) => match[1])
      .filter((type): type is string => type !== undefined),
  );
  expect(handlerTypes).toContain("papio.pageBulk.grabStatus");
  expect(new Set<string>(INBOX_RUNTIME_MESSAGE_TYPES)).toEqual(handlerTypes);
});