// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Behavioural tests for the MV3 bridge against a minimal fake chrome surface and
// a fake native port. No real chrome, and no wall-clock timers: every fake
// emitter awaits the handler promises it triggers, so the flow is deterministic.

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";

import {
  parseBrowserMessage,
  parseBrowserMessageWithLegacyInstitutionalNavigation,
  PDF_GRAB_REFUSAL_REASONS,
  type BrowserMessage,
  type BrowserSessionRole,
  type PageCapturePayload,
  type PdfGrabRefusalReason,
} from "../src/protocol";
import { isSurfaceBirthRecord, type SurfaceBirthRecord } from "../src/ledger";
import {
  emptyStore,
  findByJob,
  jobDownloadFilename,
  migrateManagedState,
  startPendingDelivery,
  type ActiveJob,
  type MaterializationCorrelation,
  type ProviderDriveEpoch,
  type StateBackend,
  type StoreShape,
} from "../src/state";
import { pdfGrabRefusalText } from "../src/deliver";
import {
  capturePage,
  encodePageCapture,
  sanitizeFixture,
} from "../src/capture";
import {
  KeepaliveManager,
  type FreshSessionEvidence,
  type KeepaliveAPI,
} from "../src/keepalive";
import { type AdapterSpec, type PageVerdict } from "../src/adapters/types";
import {
  emptyPageBulkScanStore,
  scanDocument,
  withPageBulkSnapshot,
  type PageBulkScanStore,
  type PageBulkSnapshot,
} from "../src/page-scan";
import {
  planExecution,
  planGeneric,
  type GenericCandidate,
  type GenericPlan,
  type Plan,
} from "../src/plan";
import {
  Bridge,
  assessDrivenPage,
  findManagedTab,
  MIN_DAEMON_VERSION,
  hasDaemonUpdateHint,
  handleInboxRuntimeMessage,
  respondToRuntimePromise,
  INBOX_RUNTIME_MESSAGE_TYPES,
  needsVisibleWindow,
  normalizeManagedTabURL,
  normalizeExpectedDOI,
  isBotChallenge,
  isRedirectLoopPage,
  executePlannedPageEffect,
  registrableProviderHost,
  type BridgeDeps,
  type ClaimObservationOutboxEntry,
  type DownloadDeltaLike,
  type DownloadItemLike,
  type NativePort,
  type NavigationErrorMarkerEntry,
  type PageBulkSnapshotView,
  type PdfGrabCorrelation,
  type TabChangeInfo,
  type TabInfo,
} from "../src/background";
import { routeResolverService } from "../src/resolver";
import { ChromeTabsFake, FakeWebNavigation } from "./fake-tabs";
import { FakeDownloads } from "./fake-downloads";
import {
  expectLedgerRetains,
  restartWorker,
  simulateExtensionUpdate,
} from "./lifecycle";

/** A hello_ack that must read as a healthy daemon has to sit at or above
 * background.ts's MIN_DAEMON_VERSION. Deriving it keeps a floor bump from
 * silently flipping every fixture below to "daemon_outdated" — the literal
 * "0.9.0" these used to carry was the old floor, and became outdated the
 * moment it moved. Tests that deliberately want an OLD daemon keep a literal.
 */
const CURRENT_DAEMON = MIN_DAEMON_VERSION;

const OPENURL = "https://resolver.example.edu/openurl?ctx=abc";
const PROVIDER_HOST = "www.jstor.org";
/** Simulates the ADR-0022 Phase 4 authentication-claim daemon (surface-
 * lifecycle plan, Slice 3). Tests exercising the AUTONOMOUS requires_auth
 * open/release machinery negotiate it; without it the Slice 0 containment
 * gate parks that work tabless as engagement_required. */
const AUTH_CLAIM = "institutional_authentication_claim_v1";
test("managed tab dedupe ignores URL fragments and prioritizes a tracked tab", () => {
  const candidates: TabInfo[] = [
    { id: 100, url: "https://sage.example/article?download=1#section-a" },
    { id: 101, url: "https://sage.example/other" },
  ];
  expect(
    normalizeManagedTabURL("https://sage.example/article?download=1#section-a"),
  ).toBe("https://sage.example/article?download=1");
  expect(
    findManagedTab(
      candidates,
      "https://sage.example/article?download=1#section-b",
    )?.id,
  ).toBe(100);
  expect(
    findManagedTab(candidates, "https://sage.example/new#fragment", 101)?.id,
  ).toBe(101);
});

const EXPIRES = "2027-01-01T00:00:00Z";

// Listeners are registered as promise-returning callbacks; emit awaits them all,
// which makes handler completion observable without any timer.
class FakeEmitter<A extends unknown[]> {
  private readonly cbs: ((...a: A) => unknown)[] = [];
  addListener(cb: (...a: A) => void): void {
    this.cbs.push(cb);
  }
  /** Test-only restart seam: see fake-tabs.ts's FakeEmitter.snapshot for the
   * exact contract this mirrors. */
  snapshot(): () => void {
    const keep = this.cbs.length;
    return () => {
      this.cbs.length = keep;
    };
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
  async waitForFrame(
    type: BrowserMessage["type"],
    afterPostedCount = 0,
  ): Promise<BrowserMessage> {
    const existing = this.posted
      .slice(afterPostedCount)
      .map(parseBrowserMessage)
      .find((frame) => frame.type === type);
    if (existing !== undefined) return existing;
    return new Promise<BrowserMessage>((resolve) => {
      const waiter = (message: object) => {
        if (this.posted.indexOf(message) < afterPostedCount) return;
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
  failNextSave = false;
  /** Every store this backend was ever asked to persist, in order — not
   * just the latest. A leak that lands in storage transiently and is later
   * overwritten is still a leak; checking only the final `store` snapshot
   * would miss it. */
  readonly saveHistory: StoreShape[] = [];
  async load(): Promise<StoreShape> {
    return this.store;
  }
  async save(store: StoreShape): Promise<void> {
    if (this.failNextSave) {
      this.failNextSave = false;
      throw new Error("storage unavailable");
    }
    this.store = store;
    this.saveHistory.push(JSON.parse(JSON.stringify(store)) as StoreShape);
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

class FakeWindows {
  readonly created: { url: string; focused: boolean; state: string }[] = [];
  readonly updated: {
    windowID: number;
    props: {
      focused?: boolean;
      state?: "normal" | "minimized";
      drawAttention?: boolean;
    };
  }[] = [];
  readonly removed: number[] = [];
  readonly live = new Map<number, { id: number; state: string }>();
  nextId = 500;
  constructor(private readonly tabs: ChromeTabsFake) {}
  async create(props: {
    url: string;
    focused: boolean;
    state: "minimized" | "normal";
  }): Promise<{ id: number; state: string; tabs: TabInfo[] }> {
    this.created.push(props);
    const id = this.nextId++;
    this.live.set(id, { id, state: props.state });
    const tab = await this.tabs.create({
      url: props.url,
      active: false,
      windowId: id,
    });
    return { id, state: props.state, tabs: [tab] };
  }
  async get(
    windowID: number,
  ): Promise<{ id: number; state: string; tabs: TabInfo[] }> {
    const win = this.live.get(windowID);
    if (!win) throw new Error("no such window");
    return {
      ...win,
      tabs: this.tabs.list().filter((tab) => tab.windowId === windowID),
    };
  }
  async update(
    windowID: number,
    props: {
      focused?: boolean;
      state?: "normal" | "minimized";
      drawAttention?: boolean;
    },
  ): Promise<unknown> {
    this.updated.push({ windowID, props });
    const win = this.live.get(windowID);
    if (win && props.state !== undefined) win.state = props.state;
    return win ?? {};
  }
  /** Programmatic close: drops the window and any tabs still parked in it. */
  async remove(windowID: number): Promise<void> {
    this.removed.push(windowID);
    for (const tab of this.tabs
      .list()
      .filter((candidate) => candidate.windowId === windowID)) {
      if (tab.id !== undefined) await this.tabs.removeFromWindow(tab.id);
    }
    this.live.delete(windowID);
  }
  /** Simulate the user closing the work window. */
  close(windowID: number): void {
    this.live.delete(windowID);
  }
}

class FakeTabGroups {
  readonly updated: {
    groupID: number;
    props: { collapsed?: boolean; title?: string; color?: string };
  }[] = [];
  readonly live = new Map<
    number,
    { id: number; collapsed: boolean; title?: string; windowId?: number }
  >();
  nextID = 700;
  async get(groupID: number): Promise<{
    id: number;
    collapsed: boolean;
    title?: string;
    windowId?: number;
  }> {
    const group = this.live.get(groupID);
    if (!group) throw new Error("no such group");
    return group;
  }
  async query(props: {
    title?: string;
  }): Promise<
    { id: number; collapsed: boolean; title?: string; windowId?: number }[]
  > {
    return [...this.live.values()].filter(
      (g) => props.title === undefined || g.title === props.title,
    );
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

class FakeAlarms {
  readonly onAlarm = new FakeEmitter<[{ name: string }]>();
  readonly created: { name: string; info?: { periodInMinutes: number } }[] =
    [];
  private readonly scheduled = new Set<string>();
  create(name: string, info?: { periodInMinutes: number }): void {
    if (this.scheduled.has(name)) return;
    this.scheduled.add(name);
    this.created.push({
      name,
      ...(info === undefined ? {} : { info }),
    });
  }
  async get(name: string): Promise<{ name: string } | undefined> {
    return this.scheduled.has(name) ? { name } : undefined;
  }
}

interface Harness {
  bridge: Bridge;
  deps: BridgeDeps;
  port: FakePort;
  ports: FakePort[];
  backend: FakeBackend;
  tabs: ChromeTabsFake;
  downloads: FakeDownloads;
  action: FakeAction;
  windows?: FakeWindows;
  tabGroups?: FakeTabGroups;
  webNavigation: FakeWebNavigation;
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  runtimeMessages: object[];
  pdfGrabCorrelations: { current: Record<string, PdfGrabCorrelation> };
  claimObservationOutbox: {
    current: Record<string, ClaimObservationOutboxEntry>;
  };
  navigationErrorMarkers: {
    current: Record<string, NavigationErrorMarkerEntry>;
  };
  frames(): BrowserMessage[];
  alarms: FakeAlarms;
  postedStrings(): string[];
  scannerAllowlist: { origins: string[] };
  pageBulkScans: { current: PageBulkScanStore };
  sessionStorage: { clear(): void };
  detachBridgeListeners(): void;
}

function makeHarness(
  seed?: StoreShape,
  opts?: {
    windows?: boolean;
    workWindowEnabled?: boolean;
    firefox?: boolean;
    firefoxVersion?: string;
    captureConsent?: boolean;
    tabGroups?: boolean;
    handoffSurface?: "in-window" | "work-window" | "tab-group";
  },
): Harness {
  const port = new FakePort();
  const ports = [port];
  let connects = 0;
  const backend = new FakeBackend();
  if (seed) backend.store = seed;
  const webNavigation = new FakeWebNavigation();
  const tabs = new ChromeTabsFake();
  tabs.nextId = 100;
  const downloads = new FakeDownloads();
  if (opts?.firefox === true)
    Reflect.deleteProperty(downloads, "onDeterminingFilename");
  const windows = opts?.windows === true ? new FakeWindows(tabs) : undefined;
  const tabGroups = opts?.tabGroups === true ? new FakeTabGroups() : undefined;
  if (tabGroups !== undefined) {
    tabs.group = async ({ tabIds, groupId }) => {
      const id = groupId ?? tabGroups.nextID++;
      const target = tabGroups.live.get(id);
      const windowID = target?.windowId ?? tabs.snapshot(tabIds[0]!)?.windowId;
      if (
        windowID !== undefined &&
        tabIds.some((tabID) => tabs.snapshot(tabID)?.windowId !== windowID)
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
      tabs.grouped.push(
        groupId === undefined ? { tabIds } : { tabIds, groupId },
      );
      const emptied = new Set<number>();
      for (const tabID of tabIds) {
        const tab = tabs.snapshot(tabID);
        if (tab === undefined) throw new Error("no such tab");
        if (tab.groupId !== undefined && tab.groupId >= 0 && tab.groupId !== id)
          emptied.add(tab.groupId);
        tabs.patch(tabID, { groupId: id });
      }
      for (const formerGroupID of emptied) {
        if (!tabs.list().some((tab) => tab.groupId === formerGroupID)) {
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
  // Restart seam (lifecycle.ts): snapshot every listener each emitter holds
  // right now — before any Bridge exists — so a later restartWorker/
  // simulateExtensionUpdate can drop exactly what a dead worker's own
  // bindListeners() added, without touching a fixture's own permanent
  // internal listeners (e.g. FakeWebNavigation's onCommitted bookkeeping,
  // installed in its own constructor, above and before this point).
  const listenerRestores = [
    tabs.onUpdated.snapshot(),
    tabs.onRemoved.snapshot(),
    tabs.onActivated.snapshot(),
    downloads.onCreated.snapshot(),
    downloads.onChanged.snapshot(),
    downloads.onDeterminingFilename?.snapshot(),
    webNavigation.onCommitted.snapshot(),
    webNavigation.onHistoryStateUpdated.snapshot(),
    webNavigation.onReferenceFragmentUpdated.snapshot(),
    webNavigation.onTabReplaced.snapshot(),
    webNavigation.onErrorOccurred.snapshot(),
    alarms.onAlarm.snapshot(),
  ].filter((restore): restore is () => void => restore !== undefined);
  const detachBridgeListeners = (): void => {
    for (const restore of listenerRestores) restore();
  };
  const runtimeMessages: object[] = [];
  const runtimeSendMessage = async (message: object): Promise<void> => {
    runtimeMessages.push(message);
  };
  const pdfGrabCorrelations: { current: Record<string, PdfGrabCorrelation> } = {
    current: {},
  };
  const claimObservationOutbox: {
    current: Record<string, ClaimObservationOutboxEntry>;
  } = { current: {} };
  const navigationErrorMarkers: {
    current: Record<string, NavigationErrorMarkerEntry>;
  } = { current: {} };
  // The scanner-consent record ADR-0019 Decision 2 gates DOM reads on. Real
  // storage semantics (validated, deduplicated, sorted) live in realDeps; this
  // is a bare cell so a test can seed or observe exactly what it means to.
  const scannerAllowlist: { origins: string[] } = { origins: [] };
  const pageBulkScans: { current: PageBulkScanStore } = {
    current: emptyPageBulkScanStore(),
  };
  // Slice 2b browser-session epoch: mirrors chrome.storage.session (wiped by
  // simulateExtensionUpdate below) paired with chrome.storage.local (never
  // wiped) — restartWorker leaves both cells alone, exactly like a real SW
  // restart.
  const epochSession: { value: string | undefined } = { value: undefined };
  const epochLocal: { value: string | undefined } = { value: undefined };
  const deps: BridgeDeps = {
    connectNative: () => {
      if (connects++ === 0) return port;
      const next = new FakePort();
      ports.push(next);
      return next;
    },
    randomUUID: () => crypto.randomUUID(),
    manifestVersion: "0.1.0",
    runtimeGetURL: (path) => `chrome-extension://test/${path}`,
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
    claimObservationOutbox: {
      get: async () => claimObservationOutbox.current,
      set: async (value) => {
        claimObservationOutbox.current = value;
      },
    },
    navigationErrorMarkers: {
      get: async () => navigationErrorMarkers.current,
      set: async (value) => {
        navigationErrorMarkers.current = value;
      },
    },
    downloads,
    scannerAllowlist: {
      get: async () => scannerAllowlist.origins,
      set: async (origins) => {
        scannerAllowlist.origins = origins;
      },
    },
    epoch: {
      getSession: async () => epochSession.value,
      setSession: async (value) => {
        epochSession.value = value;
      },
      getLocal: async () => epochLocal.value,
      setLocal: async (value) => {
        epochLocal.value = value;
      },
    },
    pageBulkScans: {
      get: async () => pageBulkScans.current,
      set: async (store) => {
        pageBulkScans.current = store;
      },
    },
    // No registered adapters and no granted host: these behavioural tests stay
    // out of the adapter action path. Adapter mapping is covered in
    // adapters.test.ts.
    adapterSpecs: [],
    scripting: { executeScript: async () => [] },
    permissions: { contains: async () => false },
    settings: {
      getTermsConsent: async () => undefined,
      setTermsConsent: async () => {},
      getHandoffSurface: async () =>
        opts?.handoffSurface ??
        (opts?.workWindowEnabled === false ? "in-window" : "work-window"),
    },
    ...(opts?.firefox === true
      ? {
          browserInfo: async () => ({
            name: "Firefox",
            version: opts.firefoxVersion ?? "128.0",
          }),
        }
      : {}),
    captureConsent: {
      get: async () => opts?.captureConsent === true,
    },
    ...(windows !== undefined ? { windows } : {}),
    ...(tabGroups !== undefined ? { tabGroups } : {}),
    action,
    alarms: {
      create: (name, info) => alarms.create(name, info),
      get: (name) => alarms.get(name),
      onAlarm: alarms.onAlarm,
    },
    webNavigation,
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
    webNavigation,
    ...(windows !== undefined ? { windows } : {}),
    ...(tabGroups !== undefined ? { tabGroups } : {}),
    clock,
    timers,
    runtimeMessages,
    pdfGrabCorrelations,
    claimObservationOutbox,
    navigationErrorMarkers,
    alarms,
    frames: () => ports.flatMap((p) => p.posted.map(parseBrowserMessage)),
    postedStrings: () =>
      ports.flatMap((p) => p.posted.map((f) => JSON.stringify(f))),
    scannerAllowlist,
    pageBulkScans,
    sessionStorage: {
      clear: () => {
        epochSession.value = undefined;
      },
    },
    detachBridgeListeners,
  };
}

function jobOffer(
  jobID: string,
  openurl = OPENURL,
  accessMode: "assisted" | "delegated" = "delegated",
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000001",
    job_id: jobID,
    seq: 0,
    payload: {
      openurl,
      provider_hosts: [PROVIDER_HOST],
      access_mode: accessMode,
      expires_at: EXPIRES,
    },
  };
}
function candidateOffer(
  jobID: string,
  candidateID = "cand_0001",
  expiresAt = "2030-01-01T00:00:00Z",
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "institutional_candidate_offer",
    msg_id: "candidate_offer_000001",
    job_id: jobID,
    seq: 2,
    payload: {
      candidate_id: candidateID,
      materialization_kind: "browser_tab",
      expires_at: expiresAt,
      provider_hosts: [PROVIDER_HOST],
      expected: { doi: "10.1234/example", title: "Example work" },
      access_mode: "delegated",
      login_entity_id: "https://idp.example/entity",
      proquest_account_id: "12345",
      requires_auth: true,
      drive_attempt_id: "attempt-001",
      drive_ordinal: 0,
      drive_strategy: "generic",
      drive_revision: "rev-1",
    },
  };
}
function materializationActiveJob(jobID: string): ActiveJob {
  return {
    job_id: jobID,
    tab_id: -1,
    offered_at: 1_700_000_000_000,
    expires_at: 1_900_000_000_000,
    status: "accepted",
    provider_hosts: [PROVIDER_HOST],
    access_mode: "delegated",
  };
}

function institutionalClaimResponse(
  jobID: string,
  candidateID: string,
  claimID = "claim_0001",
  bindingID = "bind_0001",
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "institutional_claim_response",
    msg_id: "claim_response_000001",
    job_id: jobID,
    seq: 3,
    payload: {
      outcome: "claimed",
      candidate_id: candidateID,
      claim_id: claimID,
      binding_id: bindingID,
      browser_holder_generation: 1,
      lease_until: "2030-01-01T00:00:00Z",
    },
  };
}
function institutionalBindResponse(
  jobID: string,
  claimID = "claim_0001",
  bindingID = "bind_0001",
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "institutional_bind_response",
    msg_id: "bind_response_000001",
    job_id: jobID,
    seq: 4,
    payload: { outcome: "bound", claim_id: claimID, binding_id: bindingID },
  };
}
function jobOfferForHosts(
  jobID: string,
  providerHosts: string[],
  openurl = OPENURL,
): unknown {
  const offer = jobOffer(jobID, openurl) as {
    payload: Record<string, unknown>;
  };
  offer.payload["provider_hosts"] = providerHosts;
  return offer;
}
/** Minimal valid SurfaceBirthRecord for tests that poke tabLedgerCache
 * directly (bypassing migration, which only legacy raw-storage fixtures go
 * through). Field values are placeholders unless a test overrides one it
 * actually exercises. */
function fakeBirthRecord(overrides: Partial<SurfaceBirthRecord> = {}): SurfaceBirthRecord {
  return {
    binding_id: "test-binding-0001",
    tab_hint: 0,
    purpose: "handoff",
    browser_epoch: "test-epoch",
    extension_generation: "test",
    created_at: 1,
    ...overrides,
  };
}
/** Wires a raw, pre-migration-shaped ledger fixture (legacy or already-
 * migrated, tests may seed either) behind deps.tabLedger. migrateTabLedger
 * runs over it on first touch, exactly as production does, so `current()`
 * reflects birth certificates once the bridge has written anything back. */
function installManagedTabLedger(
  h: Harness,
  initial: Record<string, unknown>,
): { current: () => Record<string, unknown> } {
  let ledger: Record<string, unknown> = { ...initial };
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  return { current: () => ({ ...ledger }) };
}

/** Seed the worker-local offerURLs cache directly (Slice 4: offer URLs are
 * transient worker memory only, never persisted — see state.ts). Safe
 * before or after `h.bridge.start()` (hydration no longer touches this
 * cache); represents a job offered earlier in the SAME worker lifetime,
 * never a value restored from storage. */
function seedOfferURLs(h: Harness, urls: Record<string, string>): void {
  const internals = h.bridge as unknown as { offerURLs: Map<string, string> };
  for (const [jobID, url] of Object.entries(urls)) internals.offerURLs.set(jobID, url);
}

/** Build release-grade FreshSessionEvidence for a Commit C bridge call. The
 * generation is opaque to Bridge (KeepaliveManager-internal bookkeeping), so
 * tests never need a meaningful value here. */
function freshEvidence(
  h: Harness,
  origin: string,
  source: FreshSessionEvidence["source"] = "keepalive_tab",
): FreshSessionEvidence {
  return { origin, observedAt: h.clock.now, generation: 1, source };
}

const PROVIDER_ADAPTER: AdapterSpec = {
  id: "provider",
  version: "1.0.0",
  hosts: [PROVIDER_HOST],
  classify: [],
};
function plannerResult(
  injection: Parameters<BridgeDeps["scripting"]["executeScript"]>[0],
  verdict: Partial<PageVerdict> & { kind: PageVerdict["kind"] },
): { result: Plan }[] {
  const args = injection.args ?? [];
  const spec = (args[1] as AdapterSpec | undefined) ?? PROVIDER_ADAPTER;
  const download = spec.download;
  const expected = (args[2] ?? {}) as {
    title?: string;
    doi?: string;
    year?: number;
  };
  const policy = (args[3] ?? {}) as {
    access_mode?: "assisted" | "delegated" | "conservative";
    terms_consent?: "accept" | "decline";
  };
  const win = new Window({ url: "https://www.jstor.org/stable/4093878" });
  const terms = verdict.kind === "terms" ? spec.termsAccept : undefined;
  const planningSpec =
    terms !== undefined
      ? {
          ...spec,
          classify: [{ kind: "terms" as const, all: [terms.modalSelector] }],
        }
      : spec;
  if (terms !== undefined) {
    const modalFirst = terms.modalSelector.split(/[.#\[]/u)[0]?.trim() ?? "div";
    const modalTag = /^[a-z][a-z0-9-]*/iu.exec(modalFirst)?.[0] ?? "div";
    const modal = win.document.createElement(modalTag);
    const modalID = /#([A-Za-z0-9_-]+)/u.exec(terms.modalSelector)?.[1];
    const modalClass = /\.([A-Za-z0-9_-]+)/u.exec(terms.modalSelector)?.[1];
    if (modalID !== undefined) modal.id = modalID;
    if (modalClass !== undefined) modal.className = modalClass;
    if (/\[open\]/u.test(terms.modalSelector)) modal.setAttribute("open", "");
    const controlSelector = terms.control ?? "button";
    const controlFirst =
      controlSelector.split(/[.#\[]/u)[0]?.trim() ?? "button";
    const controlTag = /^[a-z][a-z0-9-]*/iu.exec(controlFirst)?.[0] ?? "button";
    const control = win.document.createElement(controlTag);
    const controlID = /#([A-Za-z0-9_-]+)/u.exec(controlSelector)?.[1];
    const controlClass = /\.([A-Za-z0-9_-]+)/u.exec(controlSelector)?.[1];
    if (controlID !== undefined) control.id = controlID;
    if (controlClass !== undefined) control.className = controlClass;
    if (controlTag.toLowerCase() === "input") {
      control.setAttribute("type", "submit");
      control.setAttribute("value", terms.textAny[0] ?? "Accept");
    } else {
      control.textContent = terms.textAny[0] ?? "Accept";
    }
    modal.appendChild(control);
    win.document.body.appendChild(modal);
  }
  const selector = download?.selector;
  if (selector !== undefined && terms === undefined) {
    const first = selector.split(",")[0]?.trim() ?? "div";
    const tag = /^[a-z][a-z0-9-]*/i.exec(first)?.[0] ?? "div";
    const element = win.document.createElement(tag);
    const id = /#([A-Za-z0-9_-]+)/.exec(first)?.[1];
    const className = /\.([A-Za-z0-9_-]+)/.exec(first)?.[1];
    if (id !== undefined) element.id = id;
    if (className !== undefined) element.className = className;
    if (download?.method === "meta") {
      element.setAttribute("name", download.metaName ?? "citation_pdf_url");
      element.setAttribute("content", "https://download.example/paper.pdf");
    } else if (tag.toLowerCase() === "a") {
      element.setAttribute("href", "https://download.example/paper.pdf");
    }
    (tag.toLowerCase() === "meta"
      ? win.document.head
      : win.document.body
    ).appendChild(element);
  }
  const actual = planExecution(
    win.document as unknown as Document,
    planningSpec,
    expected,
    policy,
  );
  const fullVerdict: PageVerdict = {
    kind: verdict.kind,
    adapter_id: verdict.adapter_id ?? spec.id,
    adapter_version: verdict.adapter_version ?? spec.version,
    evidence: verdict.evidence ?? [],
  };
  const base: Plan =
    "assisted" in actual
      ? {
          adapter_id: spec.id,
          adapter_version: spec.version,
          verdict: fullVerdict,
          decisive_rule:
            fullVerdict.kind === "unknown"
              ? null
              : `rule:${fullVerdict.kind} matched`,
          target_ref: null,
          method: null,
          url: null,
          required_consequence: "none",
          access_mode: policy.access_mode,
          terms_consent: policy.terms_consent ?? null,
          expected_work: {
            requested_doi: expected.doi?.trim().toLowerCase() ?? null,
            requested_title:
              expected.title?.trim().toLowerCase().replace(/\s+/g, " ") ?? null,
            doi: null,
            title: null,
          },
          effect_graph: {
            primary_target: null,
            followup_target: null,
            terms_target: null,
            api: null,
            consequence: "none",
            route: null,
          },
          route_origin: null,
          revalidation: {
            target_cardinality: 1,
            max_selector_length: 512,
            max_wait_ms: 0,
          },
        }
      : actual;
  const termsPlan =
    fullVerdict.kind === "terms" && base.effect_graph.terms_target !== null;
  const article = fullVerdict.kind === "article" && spec.download !== undefined;
  const articleTarget = article
    ? (base.target_ref ?? {
        selector: spec.download!.selector,
        shadow_selector: spec.download!.shadowSelector ?? null,
        fingerprint: "synthetic",
      })
    : null;
  return [
    {
      result: {
        ...base,
        adapter_id: fullVerdict.adapter_id,
        adapter_version: fullVerdict.adapter_version,
        verdict: fullVerdict,
        decisive_rule:
          fullVerdict.kind === "unknown"
            ? null
            : `rule:${fullVerdict.kind} matched`,
        target_ref: termsPlan ? null : articleTarget,
        method: termsPlan ? null : article ? spec.download!.method : null,
        url: termsPlan
          ? null
          : article
            ? "https://download.example/paper.pdf"
            : null,
        required_consequence: termsPlan
          ? "none"
          : article
            ? "download"
            : "none",
        effect_graph: termsPlan
          ? base.effect_graph
          : {
              primary_target: articleTarget,
              followup_target: null,
              terms_target: null,
              api: null,
              consequence:
                article && spec.download!.method === "click"
                  ? "modal"
                  : article
                    ? "download"
                    : "none",
              route: {
                origin: "https://download.example",
                pathname: "/paper.pdf",
              },
            },
        route_origin: termsPlan
          ? base.route_origin
          : "https://download.example",
        revalidation: termsPlan
          ? base.revalidation
          : { target_cardinality: 1, max_selector_length: 512, max_wait_ms: 0 },
      },
    },
  ];
}
function plannedEffectResult(
  injection: Parameters<BridgeDeps["scripting"]["executeScript"]>[0],
): { result: { ok: boolean; url?: string } }[] {
  const plan = (injection.args?.[0] ?? {}) as Plan;
  const rule = (injection.args?.[1] ?? {}) as { method?: string };
  if (rule.method === "click") return [{ result: { ok: true } }];
  return [
    {
      result: {
        ok: plan.url !== null,
        ...(plan.url !== null ? { url: plan.url } : {}),
      },
    },
  ];
}

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

function useUnknownProviderClassifier(
  h: Harness,
  challenge: () => boolean,
): void {
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === planExecution)
      return plannerResult(injection, { kind: "unknown" });
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: challenge() ? "challenge" : "normal" } }];
    if (injection.func === isBotChallenge) return [{ result: challenge() }];
    return [];
  };
}

async function classifyProviderUnknown(
  h: Harness,
  jobID: string,
): Promise<number> {
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.seed({ id: tabID, url });
  await h.tabs.completeNavigation(tabID, url);
  return tabID;
}
/** Exercise the planner's URL resolution directly. This mirror used to call
 * an injected background helper that no longer exists; the planner is now the
 * single source of truth for href/meta URL safety. */
function plannerURL(
  method: "meta" | "href",
  pageURL: string,
  html: string,
  selector: string,
): string | null {
  const window = new Window({ url: pageURL });
  window.document.write(html);
  const adapter: AdapterSpec = {
    id: `planner-url-${method}`,
    version: "1.0.0",
    hosts: [PROVIDER_HOST],
    classify: [
      {
        kind: "article",
        all: [method === "meta" ? `meta[name="${selector}"]` : selector],
      },
    ],
    download:
      method === "meta"
        ? {
            selector: `meta[name="${selector}"]`,
            requireKind: "article",
            method: "meta",
            metaName: selector,
          }
        : { selector, requireKind: "article", method: "href" },
  };
  const result = planExecution(
    window.document as unknown as Document,
    adapter,
    {},
    {},
  );
  return "assisted" in result ? null : result.url;
}

test("planner meta URL resolution rejects every self-reference escape but still resolves a distinct download URL", () => {
  const PAGE = "https://p.example.edu/a";
  const metaTag = (content: string): string =>
    `<html><head><meta name="citation_pdf_url" content="${content}"></head><body></body></html>`;
  const run = (html: string): string | null =>
    plannerURL("meta", PAGE, html, "citation_pdf_url");

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
  expect(run(metaTag("?download=true"))).toBe(
    "https://p.example.edu/a?download=true",
  );
  expect(run(metaTag("https://p.example.edu/other.pdf"))).toBe(
    "https://p.example.edu/other.pdf",
  );
});

test("planner href URL resolution rejects every self-reference escape but still resolves a distinct download URL", () => {
  const PAGE = "https://p.example.edu/a";
  const anchor = (href: string): string =>
    `<html><body><a class="pdf" href="${href}">Download</a></body></html>`;
  const run = (html: string): string | null =>
    plannerURL("href", PAGE, html, "a.pdf");

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
  expect(run(anchor("?download=true"))).toBe(
    "https://p.example.edu/a?download=true",
  );
  expect(run(anchor("https://p.example.edu/other.pdf"))).toBe(
    "https://p.example.edu/other.pdf",
  );
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
  payload: {
    daemon_version?: string;
    features?: string[];
    resolver_origins?: string[];
    role?: BrowserSessionRole;
    browser_holder_generation?: number;
  } = {},
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

function sessionBusyError(): unknown {
  return {
    protocol: "papio-browser/1",
    type: "error",
    msg_id: "error_00000003",
    seq: 1,
    payload: {
      code: "session_busy",
      message:
        "another browser holds the papio session (v0.14.0); run 'papio browser sessions' then 'papio browser use' to switch",
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

function snapshotResult(
  requestID: string,
  pending = 0,
  schema: 1 | 2 = 1,
): unknown {
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

/** One open `manual_download` row exactly as triage-snapshot/1 emits it. */
function manualDownloadItem(
  jobID: string,
  actionID = 1,
): Record<string, unknown> {
  return {
    kind: "human_action",
    id: `action:${actionID}`,
    rank: actionID,
    title: "Example work",
    facts: [],
    links: [],
    ops: ["open", "dismiss"],
    action_id: actionID,
    job_id: jobID,
    action_kind: "manual_download",
    job_state: "awaiting_human",
    revision: 1,
    sha256: "",
    size_bytes: 0,
  };
}

/** Issue one triage snapshot request and settle it with exactly this page. */
async function settleSnapshot(
  h: Harness,
  items: Record<string, unknown>[],
  opts?: { hasMore?: boolean; unsupported?: number; cursor?: string },
): Promise<void> {
  const before = h
    .frames()
    .filter((frame) => frame.type === "triage_snapshot_request").length;
  const pending = h.bridge.requestTriageSnapshot({
    schema_versions: [1],
    ...(opts?.cursor === undefined ? {} : { cursor: opts.cursor }),
  });
  await Promise.resolve();
  await Promise.resolve();
  const request = h
    .frames()
    .filter((frame) => frame.type === "triage_snapshot_request")[before];
  expect(request).toBeDefined();
  const hasMore = opts?.hasMore === true;
  await h.port.inbound(
    nativeResult("triage_snapshot_response", {
      request_id: request!.payload["request_id"],
      schema: 1,
      generated_at: "2027-01-01T00:00:00Z",
      counts: {
        ...triageCounts(0),
        pending_total: items.length,
        actions: items.length,
      },
      items,
      ...(hasMore ? { cursor: "next-page" } : {}),
      has_more: hasMore,
      unsupported_items_count: opts?.unsupported ?? 0,
    }),
  );
  await expect(pending).resolves.toMatchObject({ ok: true });
}

test("hello is the first outgoing frame with a valid msg_id and seq 0", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const first = h.frames()[0];
  expect(first?.type).toBe("hello");
  expect(first?.seq).toBe(0);
  expect(first?.msg_id).toMatch(/^[A-Za-z0-9_-]{8,64}$/);
  expect(first?.payload["extension_version"]).toBe("0.1.0");
  // Exact list, not a superset check: an entry missing here is a feature the
  // daemon silently refuses rather than a parse error. `pdf_grab_v1` was
  // absent while `pdfGrabRefusalReason` required it, so "Send PDF" answered
  // `extension_outdated` in every browser. The Go tests could not catch it —
  // `grabCapableHello` builds its own hello containing the feature.
  expect(first?.payload["features"]).toEqual([
    "effect_permit_v1",
    "pdf_grab_v1",
    "institutional_materialization_v1",
    "surface_presence_v1",
    "work_pulse_v1",
  ]);
});

test("startup clears a stale badge when persisted daemon health is connected", async () => {
  const h = makeHarness({ ...emptyStore(), connectionStatus: "connected" });
  await h.bridge.start();

  expect(h.action.texts).toEqual([""]);
});

test("hello acknowledgment persists daemon version, features, and connected status", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["browser-v1", "direct-download"],
    }),
  );

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
  void h.bridge
    .requestPageAcquire({
      url: "https://publisher.example.edu/article/42",
      doi: "10.1000/example.42",
    })
    .then((value) => {
      response = value;
    });
  await Promise.resolve();
  await Promise.resolve();
  expect(response).toEqual({
    error: "Page acquisition is not available from this daemon",
  });
  expect(h.frames().map((frame) => frame.type)).toEqual(["hello"]);
});

test("relays page acquisition and routes its acknowledgement to the popup", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }),
  );

  const acknowledgement = h.bridge.requestPageAcquire({
    // A landing-page URL routinely carries bearer-grade values — a Springer
    // content-sharing token, an EZproxy ticket — and the daemon never reads this
    // field, so the wire boundary reduces it to an origin for EVERY caller. It
    // used to be forwarded verbatim.
    url: "https://publisher.example.edu/article/42?sharing_token=SECRET123#s2",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  });
  await Promise.resolve();
  const request = h.frames().at(-1);
  expect(request?.type).toBe("page_acquire");
  expect(request?.payload).toEqual({
    url: "https://publisher.example.edu",
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
  expect(await acknowledgement).toEqual({
    job_id: "job_page_acquire_001",
    duplicate: true,
  });
});

test("page acquisition drops a URL-shaped title and refuses an unrepresentable address", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }),
  );

  // The daemon PERSISTS title into the job row, unlike url which it discards, so
  // a title that is really an address must not smuggle one past the reduction.
  void h.bridge.requestPageAcquire({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "https://publisher.example.edu/article/42?ticket=ST-9f8e7d",
    source: "popup",
  });
  await Promise.resolve();
  expect(h.frames().at(-1)?.payload).toEqual({
    url: "https://publisher.example.edu",
    doi: "10.1000/example.42",
    source: "popup",
  });

  // No safe address exists, so nothing is sent at all: refusing is correct, and
  // falling back to the original would defeat the reduction.
  const before = h.frames().length;
  expect(
    await h.bridge.requestPageAcquire({ url: "about:blank", doi: "10.1000/x", source: "popup" }),
  ).toEqual({ error: "papio could not read this page's address" });
  expect(h.frames().length).toBe(before);
});

test("refuses a DOI-less page acquisition without sending a frame", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }),
  );

  let response: unknown;
  void h.bridge
    .requestPageAcquire({
      url: "https://publisher.example.edu/article/42",
      title: "A DOI-less page",
      source: "popup",
    })
    .then((value) => {
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
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://onesearch.library.example.edu"] }),
  );

  expect(h.backend.store.resolverOrigins).toEqual([
    "https://onesearch.library.example.edu",
  ]);
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
});

test("a granted resolver origin leaves the connected badge clear", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async ({ origins }) =>
    origins.length === 1 &&
    origins[0] === "https://onesearch.library.example.edu/*";
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://onesearch.library.example.edu"] }),
  );

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

test("a hello refused by the holder browser is not reported as an unreachable daemon", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(sessionBusyError());

  expect(h.backend.store.connectionStatus).toBe("session_elsewhere");
  expect(h.action.titles.at(-1)).toBe(
    "papio: another browser holds the papio session",
  );
  // The harness never fires a queued timer, so this resolves only because a
  // refusal is treated as a settled answer. Waiting out the hello timeout made
  // every popup read five seconds late in a browser that was merely pending.
  const stats = await h.bridge.requestStats();
  expect(stats).toMatchObject({ ok: false, error: { code: "session_busy" } });
  // The port stays open and polling: a later `papio browser use` seats this
  // session with a hello_ack on the same connection.
  expect(h.ports.length).toBe(1);
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON }));
  expect(h.backend.store.connectionStatus).toBe("connected");
});

test("a demoted browser keeps every capability it negotiated", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: ["page_acquire"] }),
  );
  expect(h.bridge.pageAcquireAvailable()).toBe(true);

  // `papio browser use` moved the session elsewhere and the daemon says so on
  // the next poll. Holdership gates offers and handoffs, never the features
  // this port negotiated: the daemon still accepts page acquisition from any
  // session it acknowledged, so the button must not go dark.
  await h.port.inbound(sessionBusyError());
  expect(h.backend.store.connectionStatus).toBe("session_elsewhere");
  expect(h.bridge.pageAcquireAvailable()).toBe(true);
});

test("a pending session sends a PDF: holdership never refuses a user-initiated grab", async () => {
  const h = makeHarness();
  await h.bridge.start();
  // The daemon now acknowledges a session it could not slot, ack FIRST, so the
  // session learns every feature before being told it is not the holder.
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      role: "pending",
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  await h.port.inbound(sessionBusyError());
  expect(h.backend.store.connectionStatus).toBe("session_elsewhere");
  expect(h.backend.store.daemonFeatures).toEqual([
    "pdf_grab_v1",
    "effect_permit_v1",
  ]);
  // The reported bug: this returned false, so Send PDF failed at click time.
  expect(h.bridge.pdfGrabAvailable()).toBe(true);

  const pdfURL = "https://provider.example.edu/pending.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });
  const reply = h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL });
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-pending-01",
      steering_path: "papio/grabs/pendingpath/",
    }),
  );
  await expect(reply).resolves.toMatchObject({ ok: true, state: "sending" });
  expect(h.downloads.started[0]).toMatchObject({ url: pdfURL });
});

test("a pending session attempts no holder-only work", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      role: "pending",
      features: [
        "institutional_materialization_v1",
        "effect_permit_v1",
        "session_evidence_v1",
        "page_capture_v1",
      ],
    }),
  );
  const before = h.frames().length;
  // An offer is daemon-initiated work: every reply it earns is holder-only.
  await h.port.inbound(jobOffer("job_0001_pending"));
  // A handoff focus would fetch a fresh link, which is holder-only too.
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "handoff_focus",
    msg_id: "handoff_focus_0001",
    seq: 4,
    job_id: "job_0001_pending",
    payload: {},
  });
  expect(h.bridge.emitSessionEvidence("warm_verified")).toBe(false);
  expect(h.bridge.pageCaptureAvailable()).toBe(false);
  expect(h.tabs.created).toEqual([]);
  expect(
    h
      .frames()
      .slice(before)
      .map((frame) => frame.type),
  ).toEqual([]);
});

test("an absent hello_ack role behaves as holder", async () => {
  const h = makeHarness();
  await h.bridge.start();
  // A daemon predating session roles only ever acknowledged the session it had
  // just slotted, so its silence is not ambiguous.
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["session_evidence_v1", "page_capture_v1"],
    }),
  );
  await h.port.inbound(jobOffer("job_0001_legacy"));

  expect(h.tabs.created.length).toBe(1);
  expect(h.frames().find((f) => f.type === "job_accept")?.job_id).toBe(
    "job_0001_legacy",
  );
  expect(h.bridge.emitSessionEvidence("warm_verified")).toBe(true);
  expect(h.bridge.pageCaptureAvailable()).toBe(true);
});

test("an explicit holder role does holder-only work", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, role: "holder" }),
  );
  await h.port.inbound(jobOffer("job_0001_holder"));

  expect(h.tabs.created.length).toBe(1);
  expect(h.frames().find((f) => f.type === "job_accept")?.job_id).toBe(
    "job_0001_holder",
  );
});

test("a reclaimed session resumes holder-only work on its next ack", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, role: "pending" }),
  );
  await h.port.inbound(jobOffer("job_0001_reclaim"));
  expect(h.tabs.created).toEqual([]);

  // `papio browser use` seats this session; the ack arrives on the same port.
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, role: "holder" }),
  );
  await h.port.inbound(jobOffer("job_0001_reclaim"));
  expect(h.tabs.created.length).toBe(1);
});

test("every pdf_grab_result refusal reason reaches the popup as its own copy", async () => {
  const expected: Record<PdfGrabRefusalReason, string> = {
    busy: "papio is busy with another download — try again in a moment.",
    daemon_unsupported: "papio needs an update — run papio doctor.",
    extension_outdated: "Reload the papio extension to finish updating.",
    not_configured: "papio isn't set up to save PDFs yet — run papio doctor.",
    adoption_unhealthy:
      "papio can't reach its downloads folder — run papio doctor.",
    tab_unusable: "This tab isn't a PDF papio can save.",
    no_session: "papio couldn't save this PDF — try again.",
    internal: "papio couldn't save this PDF — try again.",
  };
  // Every reason in the closed wire vocabulary must be covered, so adding one
  // to protocol.ts without deciding its copy fails here rather than shipping
  // the daemon's log prose to a researcher.
  expect(Object.keys(expected).sort()).toEqual(
    [...PDF_GRAB_REFUSAL_REASONS].sort(),
  );

  for (const reason of PDF_GRAB_REFUSAL_REASONS) {
    const h = makeHarness();
    await h.bridge.start();
    await h.port.inbound(
      helloAck({
        daemon_version: CURRENT_DAEMON,
        role: "pending",
        features: ["pdf_grab_v1", "effect_permit_v1"],
      }),
    );
    const pdfURL = `https://provider.example.edu/${reason}.pdf`;
    h.tabs.seed({ id: 42, url: pdfURL });
    const reply = h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL });
    const requestFrame = await h.port.waitForFrame("pdf_grab_request");
    await h.port.inbound(
      nativeResult("pdf_grab_result", {
        request_id: requestFrame.payload["request_id"] as string,
        outcome: "unavailable",
        reason,
        detail: "pdf grab requires the current holder with negotiated permits",
      }),
    );
    await expect(reply).resolves.toEqual({
      ok: false,
      error: { code: "grab_failed", message: expected[reason] },
    });
  }
});

test("a refusal with no reason falls back to the daemon's plain detail", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const pdfURL = "https://provider.example.edu/legacy-refusal.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });
  const reply = h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL });
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "unavailable",
      detail: "the download folder is full",
    }),
  );
  await expect(reply).resolves.toEqual({
    ok: false,
    error: { code: "grab_failed", message: "the download folder is full" },
  });
});

test("a reasonless refusal never surfaces daemon-internal detail prose", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const pdfURL = "https://provider.example.edu/jargon.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });
  const reply = h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL });
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "unavailable",
      // The exact string the reported bug put in front of a researcher.
      detail:
        "pdf grab requires the current holder with negotiated effect permits",
    }),
  );
  await expect(reply).resolves.toEqual({
    ok: false,
    error: {
      code: "grab_failed",
      message: "papio couldn't save this PDF — try again.",
    },
  });
});

test("no PDF-send refusal this browser can produce names holdership or permits", async () => {
  const jargon = /holder|permit|negotiat/i;
  const h = makeHarness();
  await h.bridge.start();
  const pdfURL = "https://provider.example.edu/no-daemon.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });

  const messages: string[] = [];
  // No daemon at all.
  const offline = (await h.bridge.startPDFDelivery({
    tab_id: 42,
    url: pdfURL,
  })) as unknown as { error?: { message?: string } };
  messages.push(offline.error?.message ?? "");
  // A daemon that acknowledged nothing useful.
  await h.port.inbound(helloAck({ daemon_version: CURRENT_DAEMON }));
  const unsupported = (await h.bridge.startPDFDelivery({
    tab_id: 42,
    url: pdfURL,
  })) as unknown as { error?: { message?: string } };
  messages.push(unsupported.error?.message ?? "");
  // A browser with no download steering to hand papio a file with.
  const firefox = makeHarness(undefined, { firefox: true });
  await firefox.bridge.start();
  firefox.tabs.seed({ id: 42, url: pdfURL });
  const noSteering = (await firefox.bridge.requestPdfGrab({
    tab_id: 42,
    url: pdfURL,
  })) as unknown as { error?: { message?: string } };
  messages.push(noSteering.error?.message ?? "");
  // Every daemon-classified refusal.
  for (const reason of PDF_GRAB_REFUSAL_REASONS) {
    messages.push(pdfGrabRefusalText(reason));
  }

  expect(messages.every((message) => message.length > 0)).toBe(true);
  for (const message of messages) expect(message).not.toMatch(jargon);
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
test("a re-offer after a simulated worker restart recovers the durable ledger tab", async () => {
  const h = makeHarness();
  installManagedTabLedger(h, {});
  await h.bridge.start();
  const offer = jobOffer("job_ledger_restart");
  await h.port.inbound(offer);
  const originalTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.tabs.created).toHaveLength(1);

  const internal = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
  };
  await internal.update((store) => ({
    ...store,
    activeJobs: store.activeJobs.map((job) =>
      job.job_id === "job_ledger_restart" ? { ...job, tab_id: 999 } : job,
    ),
  }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(originalTabID);
});

test("a stale tab id with no ledger match mints once and records the replacement", async () => {
  const jobID = "job_ledger_miss";
  const offerURL = "https://resolver.example.edu/openurl?ledger=miss";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 999,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  });
  const ledger = installManagedTabLedger(h, {});
  const get = h.tabs.get.bind(h.tabs);
  h.deps.tabs.get = async (tabID) => {
    if (tabID === 999) throw new Error("stale tab");
    return get(tabID);
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID, offerURL));

  expect(h.tabs.created).toHaveLength(1);
  const replacement = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobID,
  )?.tab_id;
  expect(replacement).toBe(100);
  expect(ledger.current()["100"]).toMatchObject({ job_id: jobID });
});

test("repeated reoffers reuse one ledger tab without growing the browser surface", async () => {
  const h = makeHarness();
  installManagedTabLedger(h, {});
  await h.bridge.start();
  const offer = jobOffer("job_ledger_repeat");
  await h.port.inbound(offer);
  await h.port.inbound(offer);
  await h.port.inbound(offer);

  expect(h.tabs.created).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
});
test("assisted direct-file offers stay parked for either auth requirement", async () => {
  for (const requiresAuth of [false, true]) {
    const h = makeHarness();
    const offer = jobOffer(`job_assisted_direct_${requiresAuth}`) as {
      payload: Record<string, unknown>;
    };
    offer.payload["openurl"] =
      "https://dl.acm.org/doi/pdf/10.1145/3630106.3660000";
    offer.payload["access_mode"] = "assisted";
    offer.payload["requires_auth"] = requiresAuth;
    await h.bridge.start();
    await h.port.inbound(offer);
    expect(h.tabs.created).toHaveLength(0);
    expect(h.downloads.started).toHaveLength(0);
    expect(h.backend.store.activeJobs[0]).toMatchObject({
      tab_id: -1,
      status: "queued",
      engagement_required: true,
      access_mode: "assisted",
    });
  }
});

test("a legacy offer without access authority stays parked", async () => {
  const h = makeHarness();
  const offer = jobOffer("job_legacy_missing_mode") as {
    payload: Record<string, unknown>;
  };
  delete offer.payload["access_mode"];
  await h.bridge.start();
  await h.port.inbound(offer);
  expect(h.tabs.created).toHaveLength(0);
  expect(h.downloads.started).toHaveLength(0);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: -1,
    status: "queued",
    engagement_required: true,
  });
});

test("a same-family orphan from another job is replaced rather than reused", async () => {
  const h = makeHarness();
  installManagedTabLedger(h, {
    "100": {
      openedAt: 1,
      url: "https://resolver.example.edu/openurl?job=old",
      jobID: "job_other",
    },
  });
  h.tabs.seed({ id: 100, url: "https://resolver.example.edu/openurl?job=old" });
  await h.bridge.start();
  const offer = jobOffer(
    "job_new_exact_url",
    "https://resolver.example.edu/openurl?job=new",
  );
  await h.port.inbound(offer);
  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu/openurl?job=new", active: true },
  ]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(101);
});

test("direct JSON landing is parked as unknown rather than a candidate-local miss", async () => {
  const h = makeHarness();
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942.pdf";
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_get_000001",
    job_id: "job_0001a_direct_json",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-attempt-0001",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942.pdf",
      url: directURL,
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/response.json",
    fileSize: 64,
    mime: "application/json",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  const result = h
    .frames()
    .find((frame) => frame.type === "provider_direct_get_result");
  expect(result?.payload.outcome).toBe("unknown");
  expect(h.downloads.removedFiles).toEqual([901]);
});

test("a direct route requiring durable terms consent does not download without consent", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_get_terms_0001",
    job_id: "job_0001a_direct_terms",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-terms-attempt-0001",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "durable_consent",
    },
  });

  expect(h.downloads.started).toEqual([]);
  const result = h
    .frames()
    .find((frame) => frame.type === "provider_direct_get_result");
  expect(result?.payload).toMatchObject({
    drive_attempt_id: "direct-terms-attempt-0001",
    outcome: "terms",
    landing_class: "terms",
  });
  expect(h.backend.store.activeJobs).toEqual([]);
});

test("mixed-version direct request without effect permit never downloads and emits no result or state", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["provider_direct_get_v1"] }));
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942";
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_mixed_version_0001",
    job_id: "job_direct_mixed_version",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-mixed-version-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: directURL,
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  expect(h.downloads.started).toHaveLength(0);
  expect(
    h.frames().some((frame) => frame.type === "provider_direct_get_result"),
  ).toBe(false);
  expect(h.backend.store.activeJobs).toEqual([]);
  expect(JSON.stringify(h.backend.store)).not.toContain(directURL);
  expect(JSON.stringify(h.backend.store)).not.toContain(
    "direct-mixed-version-attempt",
  );
  // No direct epoch or download state was persisted; the request was ignored before any
  // irreversible browser work.
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_direct_mixed_version",
    ),
  ).toBeUndefined();
});

test("pre-auth handoffs queue behind one visible tab, then release after auth returns", async () => {
  const h = makeHarness();
  const jobIDs = Array.from(
    { length: 5 },
    (_, index) => `job_0001a_queue_${index}`,
  );

  await h.bridge.start();
  await Promise.all(jobIDs.map((jobID) => h.port.inbound(jobOffer(jobID))));

  expect(h.tabs.created.filter((tab) => tab.active)).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(4);

  const activeJob = h.backend.store.activeJobs.find((job) => job.tab_id >= 0);
  expect(activeJob).toBeDefined();
  const activeTabID = activeJob?.tab_id ?? -1;
  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.completeNavigation(activeTabID, idpURL);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(4);

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
    access_mode: "delegated",
  });
  h.tabs.seed({ id: stuckTabID, url: idpURL });

  h.clock.now += 1;
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.userNavigate(activeTabID, providerURL);

  expect(h.tabs.created).toHaveLength(1);
  expect(h.tabs.created.filter((tab) => !tab.active)).toHaveLength(0);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(4);
  expect(h.tabs.reloaded).toEqual([stuckTabID]);
});

test("a cold requires-auth handoff is signalled while queued and opens after its bounded fallback", async () => {
  // No KeepaliveManager reports an authenticated session in this harness: this
  // is the disabled-keepalive, no-evidence path that must still reach the user.
  const h = makeHarness();
  const offer = jobOffer("job_0001a_requires_auth") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.bridge.trackedJobCount()).toBe(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: "job_0001a_requires_auth",
    tab_id: -1,
    status: "queued",
    requires_auth: true,
  });
  // Queued is papio's own work, not an ask: no count, no amber. (This harness
  // paints in the operator's quiet `off` count mode, so the queued clause
  // itself is pinned in badge.test.ts, where the mode is explicit.)
  expect(h.action.texts.at(-1)).toBe("");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
  expect(h.action.titles.at(-1)).not.toContain("sign-in");

  // Treat the worker-local timer as lost with MV3 worker suspension. The
  // periodic wake must use the durable offer time to release the cold queue.
  expect(h.timers.some((timer) => timer.ms === 45_000)).toBe(true);
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });

  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: tabID,
    status: "accepted",
  });

  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(true);
  // Now there is a login page in front of the operator: escalate.
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toBe(
    "papio: 1 paper needs your institution sign-in",
  );
});

test("handoff_link_v1 keeps a cold auth offer tabless until explicit engagement", async () => {
  const jobID = "job_fresh_link_cold";
  const entityID = "https://idp.example.edu/entity";
  const freshURL = `https://${PROVIDER_HOST}/openurl?fresh=1`;
  const h = makeHarness();
  let ledger: Record<string, unknown> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = entityID;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: jobID,
    tab_id: -1,
    status: "queued",
    requires_auth: true,
    engagement_required: true,
  });
  expect(h.timers.some((timer) => timer.ms === 45_000)).toBe(false);
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: jobID,
    tab_id: -1,
    status: "queued",
    engagement_required: true,
  });

  const opening = h.bridge.openHandoff(jobID);
  const request = await h.port.waitForFrame("handoff_link_request");
  expect(request.payload["job_id"]).toBe(jobID);
  await h.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: request.payload["request_id"],
      outcome: "opened",
      url: freshURL,
    }),
  );

  await expect(opening).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([{ url: freshURL, active: true }]);
  expect(ledger["100"]).toMatchObject({
    purpose: "federated-login",
    job_id: jobID,
  });
  expect(JSON.stringify(ledger)).not.toContain(freshURL);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: 100,
    status: "accepted",
    engagement_required: false,
  });
  await expect(h.bridge.openHandoff(jobID)).resolves.toEqual({
    ok: true,
    opened: true,
  });
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toHaveLength(1);
  const driveTimeout = h.timers.find((timer) => timer.ms === 3 * 60_000);
  expect(driveTimeout).toBeDefined();

  await driveTimeout?.fn();
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: 100,
    status: "auth_pending",
    parked_with_tab: true,
  });
  expect(h.tabs.removed).not.toContain(100);
  const internals = h.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
  };
  const driveTimerCount = h.timers.filter(
    (timer) => timer.ms === 3 * 60_000,
  ).length;
  expect(internals.handoffDrives.has(jobID)).toBe(false);
  await h.port.inbound(offer);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: 100,
    status: "auth_pending",
    parked_with_tab: true,
  });
  expect(internals.handoffDrives.has(jobID)).toBe(false);
  expect(h.timers.filter((timer) => timer.ms === 3 * 60_000)).toHaveLength(
    driveTimerCount,
  );
  await expect(h.bridge.openHandoff(jobID)).resolves.toEqual({
    ok: true,
    opened: true,
  });
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toHaveLength(1);
});

test("handoff_link_v1 keeps a warm requires-auth offer on the eager path", async () => {
  const jobID = "job_fresh_link_warm";
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = "https://idp.example.edu/entity";

  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }),
  );
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: jobID,
    tab_id: 100,
    status: "accepted",
    requires_auth: true,
  });
  expect(h.backend.store.activeJobs[0]?.engagement_required).toBeUndefined();
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toEqual([]);
});

// --- Slice 0 containment (dev/active/surface-lifecycle-plan.md) ---
// Without the daemon-side authentication-claim feature, NO autonomous path
// may create a requires_auth surface; the work parks tabless for explicit
// engagement. These pin the gate closed; the machinery itself stays covered
// by the AUTH_CLAIM-negotiating tests above.

test("Slice 0: a warm legacy requires-auth offer parks for engagement without the claim feature", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  const offer = jobOffer("job_gate_warm_legacy") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: "job_gate_warm_legacy",
    tab_id: -1,
    status: "queued",
    engagement_required: true,
    requires_auth: true,
  });
  // A legacy offer keeps its URL, worker-local only, so the operator's
  // explicit open can use it within the same worker lifetime.
  const warmInternals = h.bridge as unknown as {
    offerURLs: Map<string, string>;
  };
  expect(warmInternals.offerURLs.get("job_gate_warm_legacy")).toBe(OPENURL);

  // The 45s fallback wake never admits engagement-gated work.
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  expect(h.tabs.created).toEqual([]);

  // The operator's explicit open drives the RETAINED legacy URL — it must
  // not dead-end in openFreshHandoff (missing_claim) on an old daemon.
  const opened = await h.bridge.openHandoff("job_gate_warm_legacy");
  expect(opened.ok).toBe(true);
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
});

test("Slice 0: a warm fresh-link requires-auth offer stays tabless without the claim feature", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  const offer = jobOffer("job_gate_warm_fresh") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: "job_gate_warm_fresh",
    tab_id: -1,
    status: "queued",
    engagement_required: true,
    fresh_handoff: true,
  });
  // The fresh-link privacy cutover holds: no persisted route.
});

test("Slice 0: the claim feature alone does not open into a dead network", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  h.deps.online = () => false;
  const offer = jobOffer("job_gate_offline") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: -1,
    status: "queued",
    engagement_required: true,
  });
});

test("Slice 0: an offline wake releases nothing; the next online wake releases", async () => {
  const jobID = "job_gate_wake";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 9_999_999_999_999,
        status: "queued",
        requires_auth: true,
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  });
  let online = false;
  h.deps.online = () => online;
  // Pins probe-before-release ordering: onWake must observe zero created
  // tabs on every wake, including the online one that releases below.
  const tabsSeenAtWake: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
    markDirty: async () => {},
    probeForeground: async () => {},
    probeOriginAutomatically: async () => {},
    notifyConfiguredOriginsChanged: () => {},
    noteResolverNavigation: () => {},
    noteTabRemoved: () => {},
    onWake: async () => {
      tabsSeenAtWake.push(h.tabs.created.length);
    },
  } as unknown as KeepaliveManager);

  await h.bridge.start();
  seedOfferURLs(h, { [jobID]: OPENURL });
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  // Probe-before-release: a dead-network wake must not navigate anything.
  expect(h.tabs.created).toEqual([]);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobID),
  ).toMatchObject({ status: "queued", tab_id: -1 });

  online = true;
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  // The bounded fallback release deliberately surfaces auth work (active).
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
  expect(tabsSeenAtWake).toEqual([0, 0]);
});

test("Slice 0: an online wake paces exactly one release even with four queued requires_auth jobs on the same provider host", async () => {
  const jobIDs = [
    "job_gate_wake_flood_a",
    "job_gate_wake_flood_b",
    "job_gate_wake_flood_c",
    "job_gate_wake_flood_d",
  ];
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: jobIDs.map((jobID) => ({
      job_id: jobID,
      tab_id: -1,
      offered_at: 1,
      expires_at: 9_999_999_999_999,
      status: "queued" as const,
      requires_auth: true,
      provider_hosts: [PROVIDER_HOST],
    })),
  });
  let online = false;
  h.deps.online = () => online;
  const tabsSeenAtWake: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
    markDirty: async () => {},
    probeForeground: async () => {},
    probeOriginAutomatically: async () => {},
    notifyConfiguredOriginsChanged: () => {},
    noteResolverNavigation: () => {},
    noteTabRemoved: () => {},
    onWake: async () => {
      tabsSeenAtWake.push(h.tabs.created.length);
    },
  } as unknown as KeepaliveManager);

  await h.bridge.start();
  seedOfferURLs(
    h,
    Object.fromEntries(jobIDs.map((jobID) => [jobID, `${OPENURL}?job=${jobID}`])),
  );
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  expect(h.tabs.created).toEqual([]);
  for (const jobID of jobIDs) {
    expect(
      h.backend.store.activeJobs.find((job) => job.job_id === jobID),
    ).toMatchObject({ status: "queued", tab_id: -1 });
  }

  online = true;
  h.clock.now += 60_000;
  await h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  // drainQueuedHandoffs's HANDOFF_DRIVE_LIMIT (=1) paces releases to exactly
  // one tab per wake regardless of how many jobs are simultaneously due —
  // proving the pacing itself, not merely a single-job coincidence.
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(jobIDs.length - 1);
  expect(tabsSeenAtWake).toEqual([0, 0]);
});

test("Slice 0: a legacy engagement park's offer URL is worker-local only — a genuine restart drops it, and the operator's explicit open needs the daemon's natural re-offer to recover it", async () => {
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  const offer = jobOffer("job_gate_legacy_restart") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;
  await first.bridge.start();
  await first.port.inbound(helloAck());
  await first.port.inbound(offer);
  const firstInternals = first.bridge as unknown as {
    offerURLs: Map<string, string>;
  };
  expect(firstInternals.offerURLs.get("job_gate_legacy_restart")).toBe(
    OPENURL,
  );

  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  await restarted.bridge.start();
  await restarted.port.inbound(helloAck());
  // The job record survives (it is a durable, URL-free part of managed
  // state); the offer URL itself never does — no persisted-URL path
  // remains for a genuine restart to expose.
  const restartedInternals = restarted.bridge as unknown as {
    offerURLs: Map<string, string>;
  };
  expect(restartedInternals.offerURLs.has("job_gate_legacy_restart")).toBe(
    false,
  );
  expect(
    (await restarted.bridge.openHandoff("job_gate_legacy_restart")).ok,
  ).toBe(false);
  expect(restarted.tabs.created).toEqual([]);

  // The daemon still tracks the job and naturally re-offers it on
  // reconnect (the same wire delivery the first session used) — that is
  // what recovers the surface, never a stored URL.
  await restarted.port.inbound(offer);
  expect(restartedInternals.offerURLs.get("job_gate_legacy_restart")).toBe(
    OPENURL,
  );
  const opened = await restarted.bridge.openHandoff("job_gate_legacy_restart");
  expect(opened.ok).toBe(true);
  expect(restarted.tabs.created).toEqual([{ url: OPENURL, active: true }]);
});

test("Slice 0: hydration still drops a persisted minted fresh link", () => {
  const jobID = "job_gate_fresh_restart";
  const sentinelURL = "https://resolver.example.edu/minted?one-use=1";
  // migrateManagedState (state.ts) is the actual scrub point — the real
  // chromeBackend runs it on every load, before a Bridge ever sees the
  // result (background.test.ts's FakeBackend deliberately skips this step,
  // so driving it through makeHarness/bridge.start() would test nothing).
  // The seeded top-level `offerURLs` key mirrors exactly where a
  // pre-Slice-2b persisted blob (dev history: 06892c7's StoreShape) carried
  // this one-use minted URL; migratedState's allowlist reconstruction
  // (state.ts) must never read or forward an unrecognized key like this
  // one forward into a live store — the "no persisted URL anywhere"
  // invariant is structural, not a special-cased scrub.
  const raw = {
    version: 1,
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 9_999_999_999_999,
        status: "queued",
        requires_auth: true,
        engagement_required: true,
        fresh_handoff: true,
        provider_hosts: [PROVIDER_HOST],
      },
    ],
    offerURLs: { [jobID]: sentinelURL },
  };
  const migrated = migrateManagedState(raw);
  const serialized = JSON.stringify(migrated);
  expect(serialized).not.toContain(sentinelURL);
  expect(serialized).not.toContain("offerURLs");
  expect(migrated.activeJobs[0]).toMatchObject({
    job_id: jobID,
    tab_id: -1,
    fresh_handoff: true,
  });
});

test("Slice 0: an operator open of a tabless legacy job bypasses the drive gate", async () => {
  const jobID = "job_gate_operator_open";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 9_999_999_999_999,
        status: "accepted",
        requires_auth: true,
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  });
  await h.bridge.start();
  seedOfferURLs(h, { [jobID]: OPENURL });
  await h.port.inbound(helloAck());
  const opened = await h.bridge.openHandoff(jobID);
  expect(opened.ok).toBe(true);
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
});

test("Slice 0: a port disconnect closes the gate despite negotiated features", async () => {
  const jobID = "job_gate_disconnect";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1,
        expires_at: 9_999_999_999_999,
        status: "queued",
        requires_auth: true,
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.emitDisconnect();
  // Fresh session evidence arriving in the reconnect gap must not spend the
  // stale feature list on creating a sign-in surface with no daemon.
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );
  expect(h.tabs.created).toEqual([]);
});

test("Slice 0: a stale-IdP recovery with a dead tab parks instead of minting", async () => {
  const jobID = "job_gate_stale_redrive";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 777,
        offered_at: 1,
        expires_at: 9_999_999_999_999,
        status: "auth_pending",
        requires_auth: true,
        access_mode: "delegated",
        provider_hosts: [PROVIDER_HOST],
      },
    ],
  });
  seedOfferURLs(h, { [jobID]: OPENURL });
  await h.bridge.start();
  await h.port.inbound(helloAck());
  const job = h.backend.store.activeJobs[0];
  const internals = h.bridge as unknown as {
    redriveStaleHandoff(job: unknown, epoch: number): Promise<boolean>;
  };
  // Tab 777 was never seeded: the tracked tab is gone, so this autonomous
  // recovery would CREATE a replacement sign-in surface. Gate closed ⇒ park.
  expect(await internals.redriveStaleHandoff(job, 0)).toBe(false);
  expect(h.tabs.created).toEqual([]);
  expect(
    h.backend.store.activeJobs.find((candidate) => candidate.job_id === jobID),
  ).toMatchObject({
    tab_id: -1,
    status: "queued",
    engagement_required: true,
  });
});

test("Slice 0: session evidence reloads parked auth tabs but never the operator-active one", async () => {
  const idpURL = "https://idp.example.edu/sso";
  const jobs = ["job_fence_reload_a", "job_fence_reload_b"].map(
    (jobID, index) => ({
      job_id: jobID,
      tab_id: 61 + index,
      offered_at: 1,
      expires_at: 9_999_999_999_999,
      status: "auth_pending" as const,
      requires_auth: true,
      access_mode: "delegated" as const,
      provider_hosts: [PROVIDER_HOST],
    }),
  );
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: jobs,
  });
  h.tabs.seed({ id: 61, url: idpURL });
  // The operator is typing credentials into tab 62 RIGHT NOW: a reload
  // would wipe the form.
  h.tabs.seed({ id: 62, url: idpURL, active: true });

  await h.bridge.start();
  seedOfferURLs(h, {
    job_fence_reload_a: OPENURL,
    job_fence_reload_b: `${OPENURL}&sibling=1`,
  });
  await h.port.inbound(helloAck());
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  expect(h.tabs.reloaded).toContain(61);
  expect(h.tabs.reloaded).not.toContain(62);
});

test("Slice 0: repeated sign-in requests reuse the live papio sign-in tab", async () => {
  const origin = "https://resolver.example.edu";
  const h = makeHarness();
  let ledger: Record<string, unknown> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };

  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: [origin] }));
  const first = await h.bridge.requestSessionSignIn(origin);
  expect(first.ok).toBe(true);
  expect(h.tabs.created).toHaveLength(1);
  const tabID =
    [...Object.keys(ledger)].map(Number).find((id) => id >= 0) ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  // The operator is mid-SAML on the IdP: same sign-in surface, no new tab.
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/sso");
  const second = await h.bridge.requestSessionSignIn(origin);
  expect(second.ok).toBe(true);
  expect(h.tabs.created).toHaveLength(1);
  expect(h.tabs.activated).toContain(tabID);
});

test("a fresh tab that loses its binding to cancellation is closed", async () => {
  const jobID = "job_fresh_link_cancel_race";
  const entityID = "https://idp.example.edu/entity";
  const freshURL = `https://${PROVIDER_HOST}/openurl?fresh=cancelled`;
  const h = makeHarness();
  let ledger: Record<string, unknown> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  const createStarted = Promise.withResolvers<void>();
  const releaseCreate = Promise.withResolvers<void>();
  const createTab = h.tabs.create.bind(h.tabs);
  h.tabs.create = async (properties) => {
    createStarted.resolve();
    await releaseCreate.promise;
    return createTab(properties);
  };
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = entityID;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await h.port.inbound(offer);
  const opening = h.bridge.openHandoff(jobID);
  const request = await h.port.waitForFrame("handoff_link_request");
  await h.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: request.payload["request_id"],
      outcome: "opened",
      url: freshURL,
    }),
  );
  await createStarted.promise;
  await h.bridge.requestCancel(jobID);
  releaseCreate.resolve();

  await expect(opening).resolves.toEqual({
    ok: false,
    error: {
      code: "tab_creation_failed",
      message: "The handoff tab could not be created",
    },
  });
  expect(h.tabs.removed).toContain(100);
  expect(h.tabs.snapshot(100)).toBeUndefined();
  expect(h.backend.store.activeJobs).toEqual([]);
});

test("hello migration marks restored tabless auth jobs as fresh handoffs", async () => {
  const jobID = "job_fresh_link_migrated";
  const seed: StoreShape = {
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "auth_pending",
        provider_hosts: [PROVIDER_HOST],
        requires_auth: true,
      },
    ],
  };
  const h = makeHarness(seed);
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));

  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: -1,
    engagement_required: true,
    fresh_handoff: true,
  });
});

test("missing institution metadata is a structured engagement failure and never pre-opens", async () => {
  const jobID = "job_fresh_link_missing_claim";
  const h = makeHarness();
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await h.port.inbound(offer);

  expect(h.tabs.created).toEqual([]);
  await expect(h.bridge.openHandoff(jobID)).resolves.toEqual({
    ok: false,
    error: {
      code: "missing_claim",
      message: "The handoff is missing institution identity metadata",
    },
  });
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toEqual([]);
  expect(h.tabs.created).toEqual([]);
});

// --- Slice 3: claim-consult scenarios (dev/active/claim-observation-protocol.md) ---
// Shared fixtures: a cold, tabless, engagement_required institutional job
// with a live materialization candidate — the state openHandoff needs to
// reach openFreshHandoff's consult (materializationCorrelation supplies the
// candidate id; the daemon-driven materialization workflow itself is a
// separate, unrelated mechanism these tests never negotiate the feature
// for — hello_ack wipes any *seeded* materializations.candidate_id the
// moment it does not see institutional_materialization_v1 negotiated, so
// the candidate is applied afterward instead, straight into the reducer).
function coldClaimJob(jobID: string): ActiveJob {
  return {
    job_id: jobID,
    tab_id: -1,
    offered_at: 1_700_000_000_000,
    expires_at: 1_800_000_000_000,
    status: "queued",
    provider_hosts: [PROVIDER_HOST],
    requires_auth: true,
    access_mode: "delegated",
    engagement_required: true,
    fresh_handoff: true,
  };
}
function claimCandidate(
  jobID: string,
  candidateID: string,
): MaterializationCorrelation {
  return {
    job_id: jobID,
    candidate_id: candidateID,
    materialization_kind: "browser_tab",
    candidate_expires_at: "2030-01-01T00:00:00Z",
    phase: "claimed",
    tab_id: -1,
  };
}
/** Gives `jobID` a live materialization candidate without negotiating
 * institutional_materialization_v1 (which would drive the unrelated
 * daemon-orchestrated materialization workflow this candidate id is only
 * ever read as a signal from). Must run after bridge.start(). */
async function seedClaimCandidate(
  h: Harness,
  jobID: string,
  candidateID: string,
): Promise<void> {
  const internals = h.bridge as unknown as {
    applyMaterialization(jobID: string, event: unknown): Promise<void>;
  };
  await internals.applyMaterialization(jobID, {
    type: "offer",
    correlation: claimCandidate(jobID, candidateID),
  });
}
/** authentication_claim_request/response are JOB_SCOPED: the daemon's
 * reply must carry `job_id` at the message envelope, not just the request
 * id inside `payload` — `nativeResult` alone omits it. */
function claimResponse(
  jobID: string,
  requestID: unknown,
  fields: Record<string, unknown>,
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "authentication_claim_response",
    msg_id: `claim_response_${crypto.randomUUID().replace(/-/g, "")}`,
    job_id: jobID,
    seq: 9,
    payload: { request_id: requestID, ...fields },
  };
}
/** claim_observation_ack is likewise JOB_SCOPED. */
function observationAck(
  jobID: string,
  requestID: unknown,
  fields: Record<string, unknown>,
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "claim_observation_ack",
    msg_id: `observation_ack_${crypto.randomUUID().replace(/-/g, "")}`,
    job_id: jobID,
    seq: 9,
    payload: { request_id: requestID, ...fields },
  };
}
/** Drives `jobID`'s explicit open through an `open_new` consult outcome and
 * answers the resulting handoff_link_request, leaving a live, ledgered
 * directly-minted tab and an established claimGrants entry — the state
 * every later claim_observation-emitting event (entitled_landing,
 * owner_closed, navigation_error) requires. Per the architecture ruling,
 * an explicit operator gesture on a granted claim mints the fresh link
 * directly (no materialize.html scaffold, no daemon materialization
 * binding — that path is reserved for the daemon's own automatic Slice 4
 * pipeline). Assumes helloAck already negotiated handoff_link_v1 +
 * AUTH_CLAIM, `jobID` is already a cold candidate job (coldClaimJob,
 * seeded before bridge.start()), and seedClaimCandidate has already run
 * for it. */
async function openClaimWithNewSurface(
  h: Harness,
  jobID: string,
  candidateID: string,
  url: string,
  claimID = `claim_${candidateID}`,
): Promise<number> {
  const tabsCreatedBefore = h.tabs.created.length;
  const framesBefore = h.port.posted.length;
  const opening = h.bridge.openHandoff(jobID);
  const claimRequest = await h.port.waitForFrame(
    "authentication_claim_request",
    framesBefore,
  );
  expect(claimRequest.job_id).toBe(jobID);
  expect(claimRequest.payload["candidate_id"]).toBe(candidateID);
  expect(claimRequest.payload["trigger"]).toBe("explicit");
  // Claim-before-surface pin: no tab exists yet even before the daemon has
  // answered the claim request at all — the request itself must never be
  // preceded or raced by a locally-minted surface.
  expect(h.tabs.created).toHaveLength(tabsCreatedBefore);
  await h.port.inbound(
    claimResponse(jobID, claimRequest.payload["request_id"], {
      outcome: "open_new",
      authentication_claim_id: claimID,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${candidateID}`,
      lease_until: "2030-01-01T00:00:00Z",
    }),
  );
  const linkRequest = await h.port.waitForFrame(
    "handoff_link_request",
    framesBefore,
  );
  // No tab exists yet: the explicit direct mint never self-mints a
  // scaffold — the daemon's mint RPC is requested before any tab exists,
  // exactly as before Slice 3/4.
  expect(h.tabs.created).toHaveLength(tabsCreatedBefore);
  await h.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: linkRequest.payload["request_id"],
      outcome: "opened",
      url,
    }),
  );
  await expect(opening).resolves.toEqual({ ok: true, opened: true });
  const tabID =
    h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id ??
    -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  // One tab, created directly with the final URL — no separate scaffold
  // navigation step.
  expect(h.tabs.created).toHaveLength(tabsCreatedBefore + 1);
  expect(h.tabs.created[tabsCreatedBefore]?.url).toBe(url);
  return tabID;
}

test("Slice 3: N cold institutional jobs with live candidates consult independently — first open_new, rest park with a surfaced dependent count", async () => {
  const jobA = "job_claim_three_a";
  const jobB = "job_claim_three_b";
  const jobC = "job_claim_three_c";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA), coldClaimJob(jobB), coldClaimJob(jobC)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, "cand_three_a001");
  await seedClaimCandidate(h, jobB, "cand_three_b001");
  await seedClaimCandidate(h, jobC, "cand_three_c001");

  await openClaimWithNewSurface(
    h,
    jobA,
    "cand_three_a001",
    `https://${PROVIDER_HOST}/fresh?a=1`,
  );
  expect(h.tabs.created).toHaveLength(1);

  const parkCases: { jobID: string; candidateID: string; dependentCount: number }[] = [
    { jobID: jobB, candidateID: "cand_three_b001", dependentCount: 1 },
    { jobID: jobC, candidateID: "cand_three_c001", dependentCount: 2 },
  ];
  for (const { jobID, candidateID, dependentCount } of parkCases) {
    const framesBeforeThis = h.port.posted.length;
    const opening = h.bridge.openHandoff(jobID);
    const claimRequest = await h.port.waitForFrame(
      "authentication_claim_request",
      framesBeforeThis,
    );
    expect(claimRequest.job_id).toBe(jobID);
    expect(claimRequest.payload["candidate_id"]).toBe(candidateID);
    await h.port.inbound(
      claimResponse(jobID, claimRequest.payload["request_id"], {
        outcome: "park",
        authentication_claim_id: `claim_${candidateID}`,
        browser_holder_generation: 1,
        gate_occurrence_id: `occ_${candidateID}`,
        dependent_count: dependentCount,
      }),
    );
    await expect(opening).resolves.toEqual({
      ok: false,
      error: {
        code: "handoff_busy",
        message: "Another sign-in for this institution is already in progress",
      },
    });
  }

  // Exactly one sign-in surface across all three jobs.
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toHaveLength(1);
  expect(h.backend.store.activeJobs.find((job) => job.job_id === jobB)).toMatchObject(
    { tab_id: -1, status: "queued" },
  );
  expect(h.backend.store.activeJobs.find((job) => job.job_id === jobC)).toMatchObject(
    { tab_id: -1, status: "queued" },
  );
  const internals = h.bridge as unknown as {
    claimDependentCounts: Map<string, number>;
  };
  expect(internals.claimDependentCounts.get(jobB)).toBe(1);
  expect(internals.claimDependentCounts.get(jobC)).toBe(2);
});

test("Scenario 3 upgrade: siblings resumed after entitled_landing arrive as fresh institutional candidate offers sharing ONE authentication claim, each binds its OWN daemon-granted scaffold and retires it on delivery — no local binding synthesis, no persisted URL in any snapshot", async () => {
  const jobA = "job_claim_scenario3_a";
  const jobB = "job_claim_scenario3_b";
  const jobC = "job_claim_scenario3_c";
  const SHARED_CLAIM_ID = "claim_scenario3_shared";
  const SHARED_OCC_ID = "occ_scenario3_shared";
  const h = makeHarness(
    {
      ...emptyStore(),
      activeJobs: [coldClaimJob(jobA), coldClaimJob(jobB), coldClaimJob(jobC)],
    },
    { windows: true },
  );
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: [
        "handoff_link_v1",
        AUTH_CLAIM,
        "institutional_materialization_v1",
        "effect_permit_v1",
        "surface_close_v1",
      ],
    }),
  );
  await seedClaimCandidate(h, jobA, "cand_s3up_a001");
  await seedClaimCandidate(h, jobB, "cand_s3up_b001");
  await seedClaimCandidate(h, jobC, "cand_s3up_c001");

  // All three jobs contend for the SAME institutional claim gate: jobA's
  // consult grants it directly, jobB/jobC's park responses carry the
  // identical authentication_claim_id/gate_occurrence_id — never a
  // per-candidate id, which would misrepresent three independent gates.
  const tabA = await openClaimWithNewSurface(
    h,
    jobA,
    "cand_s3up_a001",
    `https://${PROVIDER_HOST}/fresh?s3up=a`,
    SHARED_CLAIM_ID,
  );
  for (const [jobID, candidateID, dependentCount] of [
    [jobB, "cand_s3up_b001", 1],
    [jobC, "cand_s3up_c001", 2],
  ] as const) {
    const framesBefore = h.port.posted.length;
    const opening = h.bridge.openHandoff(jobID);
    const req = await h.port.waitForFrame(
      "authentication_claim_request",
      framesBefore,
    );
    await h.port.inbound(
      claimResponse(jobID, req.payload["request_id"], {
        outcome: "park",
        authentication_claim_id: SHARED_CLAIM_ID,
        browser_holder_generation: 1,
        gate_occurrence_id: SHARED_OCC_ID,
        dependent_count: dependentCount,
      }),
    );
    await opening;
  }
  // Zero sign-in surfaces beyond jobA's across the whole three-job run.
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toHaveLength(1);

  // jobA authenticates and lands on entitled content.
  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };
  await internals.emitClaimObservation(jobA, tabA, "entitled_landing", true);
  const landed = await h.port.waitForFrame("claim_observation");
  expect(landed.payload["event_kind"]).toBe("entitled_landing");
  await h.port.inbound(
    observationAck(jobA, landed.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_s3up_landed",
      browser_holder_generation: 1,
    }),
  );

  // The daemon resolves the claim and admits each eligible sibling through
  // a FRESH institutional_candidate_offer (Slice 4's real automatic
  // pipeline) — never a locally-synthesized binding. Each sibling must run
  // its OWN institutional_claim_request/institutional_bind_request round
  // trip, land on its own daemon-issued binding_id and scaffold tab (never
  // reusing jobA's tab or claim), then complete route/navigate/delivery and
  // retire its scaffold through the real materialization_settled close
  // transaction once the daemon's generic ack settles delivery.
  const siblingTabs: number[] = [];
  const siblingBindings: string[] = [];
  for (const [jobID, candidateID, bindingID] of [
    [jobB, "cand_s3up_b_resume", "bind_s3up_b01"],
    [jobC, "cand_s3up_c_resume", "bind_s3up_c01"],
  ] as const) {
    const framesBefore = h.port.posted.length;
    const tabsCreatedBefore = h.tabs.created.length;
    await h.port.inbound(candidateOffer(jobID, candidateID));
    const claim = await h.port.waitForFrame(
      "institutional_claim_request",
      framesBefore,
    );
    expect(claim.job_id).toBe(jobID);
    expect(claim.payload["candidate_id"]).toBe(candidateID);
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_claim_response",
      msg_id: `claim_response_${bindingID}`,
      job_id: jobID,
      seq: 3,
      payload: {
        request_id: claim.payload["request_id"],
        outcome: "claimed",
        candidate_id: candidateID,
        claim_id: `claim_${bindingID}`,
        binding_id: bindingID,
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
      },
    });
    const bind = await h.port.waitForFrame(
      "institutional_bind_request",
      framesBefore,
    );
    expect(bind.payload["binding_id"]).toBe(bindingID);
    const scaffoldCreated = h.tabs.created[tabsCreatedBefore];
    expect(scaffoldCreated?.active).toBe(false);
    expect(scaffoldCreated?.url).toBe(
      `chrome-extension://test/dist/materialize.html#${bindingID}`,
    );
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_bind_response",
      msg_id: `bind_response_${bindingID}`,
      job_id: jobID,
      seq: 4,
      payload: {
        request_id: bind.payload["request_id"],
        outcome: "bound",
        claim_id: `claim_${bindingID}`,
        binding_id: bindingID,
      },
    });
    const correlation = h.backend.store.materializations?.[jobID];
    expect(correlation).toMatchObject({ phase: "bound", binding_id: bindingID });
    const tabID = correlation?.tab_id ?? -1;
    expect(tabID).toBeGreaterThanOrEqual(0);
    siblingTabs.push(tabID);
    siblingBindings.push(bindingID);

    const route = await h.port.waitForFrame(
      "institutional_route_request",
      framesBefore,
    );
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_route_response",
      msg_id: `route_response_${bindingID}`,
      job_id: jobID,
      seq: 5,
      payload: {
        request_id: route.payload["request_id"],
        outcome: "issued",
        claim_id: `claim_${bindingID}`,
        binding_id: bindingID,
        route_issuance_ordinal: 1,
        effect_ordinal:
          (route.payload["expected_effect_ordinal"] as number) + 1,
        institutional_request_id: route.payload["institutional_request_id"],
        url: `https://${PROVIDER_HOST}/${bindingID}`,
      },
    });
    const navigated = await h.port.waitForFrame(
      "institutional_navigated_request",
      framesBefore,
    );
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_navigated_response",
      msg_id: `navigated_response_${bindingID}`,
      job_id: jobID,
      seq: 6,
      payload: {
        request_id: navigated.payload["request_id"],
        outcome: "acknowledged",
        claim_id: `claim_${bindingID}`,
        binding_id: bindingID,
      },
    });
    for (let i = 0; i < 20; i += 1) await Promise.resolve();
    expect(h.backend.store.materializations?.[jobID]).toMatchObject({
      phase: "navigated",
    });

    // Delivery settles: the daemon's generic ack retires the scaffold
    // through the real materialization_settled close transaction (fix B).
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "ack",
      msg_id: `ack_${bindingID}`,
      job_id: jobID,
      seq: 7,
      payload: {},
    });
    const closeRequest = await h.port.waitForFrame(
      "surface_close_request",
      framesBefore,
    );
    expect(closeRequest.payload["disposition"]).toBe("materialization_settled");
    expect(closeRequest.payload["binding_id"]).toBe(bindingID);
    await h.port.inbound(
      nativeResult("surface_close_response", {
        request_id: closeRequest.payload["request_id"],
        outcome: "authorized",
        close_authorization_id: `auth_${bindingID}`,
        nonce: `nonce_${bindingID}`,
        browser_holder_generation: 1,
      }),
    );
    for (let i = 0; i < 20; i += 1) await Promise.resolve();
    // Fix A (materializeScaffold now routes through openBrokerTab, landing
    // the scaffold in the work window like every other papio tab) plus the
    // job-tab detach in closeAfterAdoption's materialization_settled path
    // (mirroring the existing navigation_error/scaffold_idle close sites):
    // closeOwnedTab's ownership gate (inWorkWindow || inPapioGroup) and its
    // still-tracked-by-a-job guard both admit the authorized removal below
    // instead of silently refusing it, and the tombstone (along with the
    // rest of the ledger entry) is consumed by forgetLedgeredTab once
    // onTabRemoved fires.
    expect(h.tabs.removed).toContain(tabID);
    expect(h.tabs.snapshot(tabID)).toBeUndefined();
    const closeInternalsAfter = h.bridge as unknown as {
      tabLedgerCache: Record<string, SurfaceBirthRecord>;
    };
    expect(closeInternalsAfter.tabLedgerCache[String(tabID)]).toBeUndefined();
  }

  expect(h.tabs.created).toHaveLength(3);
  expect(new Set([tabA, ...siblingTabs]).size).toBe(3);
  expect(new Set(siblingBindings).size).toBe(2);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobA),
  ).toMatchObject({ status: "accepted" });
  // Siblings resumed through the real pipeline are driven by their own
  // materialization correlation (asserted per-sibling above, phase
  // "bound" with a distinct daemon-issued binding_id) — their ActiveJob
  // status is whatever it already was from the earlier park, unchanged
  // by the candidate offer itself.
  // Structural privacy invariant: no provider origin, and no offer-URL
  // spill, in ANY individual persisted snapshot across the whole resume
  // sequence — not merely the final store.
  expect(h.backend.saveHistory.length).toBeGreaterThan(0);
  for (const snapshot of h.backend.saveHistory) {
    const serialized = JSON.stringify(snapshot);
    expect(serialized).not.toContain(`https://${PROVIDER_HOST}`);
    expect(serialized).not.toContain("offerURLs");
  }
});

test("Slice 3: auth_returned/entitled_landing acks applied trigger no local resume — a live-tab re-offer is accepted without a second consult", async () => {
  const jobID = "job_claim_resume";
  const candidateID = "cand_resume_0001";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobID)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobID, candidateID);
  const tabID = await openClaimWithNewSurface(
    h,
    jobID,
    candidateID,
    `https://${PROVIDER_HOST}/fresh?resume=1`,
  );
  expect(h.tabs.created).toHaveLength(1);

  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };
  const framesBeforeAuthReturned = h.port.posted.length;
  await internals.emitClaimObservation(jobID, tabID, "auth_returned", true);
  const authReturned = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeAuthReturned,
  );
  expect(authReturned.payload["event_kind"]).toBe("auth_returned");
  await h.port.inbound(
    observationAck(jobID, authReturned.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_resume_ack01",
      browser_holder_generation: 1,
    }),
  );

  const framesBeforeLanding = h.port.posted.length;
  await internals.emitClaimObservation(jobID, tabID, "entitled_landing", true);
  const landed = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeLanding,
  );
  expect(landed.payload["event_kind"]).toBe("entitled_landing");
  await h.port.inbound(
    observationAck(jobID, landed.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_resume_ack02",
      browser_holder_generation: 1,
    }),
  );

  // The acks mutate nothing locally: no new tab, no job-state change.
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobID),
  ).toMatchObject({ tab_id: tabID, status: "accepted" });

  // A fresh daemon re-offer for the same, still-live job is accepted as-is —
  // no second consult mints a second sign-in surface.
  const framesBeforeReoffer = h.port.posted.length;
  const reoffer = jobOffer(jobID, `https://${PROVIDER_HOST}/reoffer`) as {
    payload: Record<string, unknown>;
  };
  reoffer.payload["requires_auth"] = true;
  await h.port.inbound(reoffer);
  const afterReoffer = h.frames().slice(framesBeforeReoffer);
  expect(h.tabs.created).toHaveLength(1);
  expect(afterReoffer.filter((frame) => frame.type === "job_accept")).toHaveLength(1);
  expect(
    afterReoffer.filter(
      (frame) =>
        frame.type === "authentication_claim_request" ||
        frame.type === "handoff_link_request",
    ),
  ).toEqual([]);
});

test("Slice 3: an owner tab closed without success emits owner_closed with the correct ordinal and leaves dependents tabless", async () => {
  const jobA = "job_claim_owner";
  const jobB = "job_claim_dependent";
  const candidateA = "cand_owner_0001";
  const candidateB = "cand_dependent001";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA), coldClaimJob(jobB)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, candidateA);
  await seedClaimCandidate(h, jobB, candidateB);
  const tabID = await openClaimWithNewSurface(
    h,
    jobA,
    candidateA,
    `https://${PROVIDER_HOST}/fresh?owner=1`,
  );

  // jobB independently parks behind the same institution's claim gate.
  const parkFramesBefore = h.port.posted.length;
  const parking = h.bridge.openHandoff(jobB);
  const parkRequest = await h.port.waitForFrame(
    "authentication_claim_request",
    parkFramesBefore,
  );
  await h.port.inbound(
    claimResponse(jobB, parkRequest.payload["request_id"], {
      outcome: "park",
      authentication_claim_id: `claim_${candidateB}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${candidateB}`,
      dependent_count: 1,
    }),
  );
  await parking;
  const dependentSnapshot = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobB,
  );

  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };
  const framesBeforeFirst = h.port.posted.length;
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  const first = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeFirst,
  );
  expect(first.payload["event_ordinal"]).toBe(0);
  await h.port.inbound(
    observationAck(jobA, first.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_owner_ack001",
      browser_holder_generation: 1,
    }),
  );

  const framesBeforeClose = h.port.posted.length;
  await h.tabs.userClose(tabID);
  const closed = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeClose,
  );
  expect(closed.payload["event_kind"]).toBe("owner_closed");
  expect(closed.payload["event_ordinal"]).toBe(1);

  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobB),
  ).toEqual(dependentSnapshot);
  expect(h.tabs.created).toHaveLength(1);
});

// Live-smoke regression (2026-08-19): reproduced on the operator's own
// browser. The grant that authorizes owner_closed is worker memory, and MV3
// sleeps the worker after ~30s idle, so a sign-in tab abandoned minutes later
// emitted NOTHING: the daemon kept the claim in `navigated` until its 30-minute
// lease expired, and every sibling paper at that institution parked tablessly
// and silently in the meantime (verified against the live store:
// materialization_claims held a claim whose tab_id no longer existed, while
// claim_observation_journal was empty). The birth record is the durable proof
// that survives, so it now carries the claim identity.
test("Slice 3 (live smoke): an owner tab closed after a worker restart still reports owner_closed from its birth record", async () => {
  const jobA = "job_claim_owner_restart";
  const candidateA = "cand_owner_restart";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA)],
  });
  const ledger = installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, candidateA);
  const tabID = await openClaimWithNewSurface(
    h,
    jobA,
    candidateA,
    `https://${PROVIDER_HOST}/fresh?owner=restart`,
  );
  const born = ledger.current()[String(tabID)];
  expect(isSurfaceBirthRecord(born)).toBe(true);
  const bindingID = isSurfaceBirthRecord(born) ? born.binding_id : "";
  expect(bindingID).not.toBe("");
  // The identity the restart will depend on is durable, not worker memory.
  expect(isSurfaceBirthRecord(born) ? born.claim : undefined).toMatchObject({
    authentication_claim_id: `claim_${candidateA}`,
    gate_occurrence_id: `occ_${candidateA}`,
    browser_holder_generation: 1,
  });

  // The worker dies. Durable storage and the tab both survive; claimGrants
  // does not.
  const restarted = restartWorker(h);
  await restarted.bridge.start();
  // A real daemon's ack carries its live holder generation (the oracle
  // round's P0), which is what re-arms the close-authorization path after a
  // restart. The observation above does not depend on it — the birth record
  // carries the generation the claim was granted under.
  await restarted.port.inbound(
    helloAck({
      features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"],
      browser_holder_generation: 1,
    }),
  );

  const framesBefore = restarted.port.posted.length;
  await restarted.tabs.userClose(tabID);
  const closed = await restarted.port.waitForFrame(
    "claim_observation",
    framesBefore,
  );
  expect(closed.payload["event_kind"]).toBe("owner_closed");
  // Identity comes from the birth record, not a guess: the same claim and
  // gate occurrence the daemon granted before the restart.
  expect(closed.payload["authentication_claim_id"]).toBe(`claim_${candidateA}`);
  expect(closed.payload["gate_occurrence_id"]).toBe(`occ_${candidateA}`);
  expect(closed.payload["binding_id"]).toBe(bindingID);
  expect(closed.payload["browser_holder_generation"]).toBe(1);

  // And the abandonment is authorized under its own disposition, so the
  // daemon can retire the claim now instead of waiting out the lease.
  const closeRequest = restarted
    .frames()
    .find(
      (frame) =>
        frame.type === "surface_close_request" &&
        frame.payload["binding_id"] === bindingID,
    );
  expect(closeRequest?.payload["disposition"]).toBe("claim_abandoned");
});

test("Slice 3 (oracle finding 4): an ack's fresh gate_occurrence_id rotates the SAME still-owned job's grant in place — a later wall_observed for the new occurrence is not suppressed by the prior occurrence's latch", async () => {
  const jobA = "job_claim_occ_rotate";
  const candidateA = "cand_occ_rotate01";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, candidateA);
  const tabID = await openClaimWithNewSurface(
    h,
    jobA,
    candidateA,
    `https://${PROVIDER_HOST}/fresh?occ=1`,
  );
  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };

  // G1 (openClaimWithNewSurface's own `occ_${candidateA}`): the first
  // wall_observed for this occurrence, latched.
  const framesBeforeFirst = h.port.posted.length;
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  const first = await h.port.waitForFrame("claim_observation", framesBeforeFirst);
  expect(first.payload["gate_occurrence_id"]).toBe(`occ_${candidateA}`);

  // The daemon acks it applied but rotates the occurrence on this SAME
  // still-owned grant — no tab close, no re-consult, no job removal in
  // between (oracle finding 4's "the first arbitration otherwise ends
  // without removing the job"). drainObservationOutboxBatch's own ack
  // handling applies a fresh gate_occurrence_id to every grant sharing
  // this authentication_claim_id in place.
  await h.port.inbound(
    observationAck(jobA, first.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_rotate_g2",
      browser_holder_generation: 1,
    }),
  );

  // A further wall_observed must still reach the daemon for the NEW
  // occurrence: the pre-fix job-level latch (`${jobID}:wall_observed`)
  // would suppress this as already-emitted even though it belongs to a
  // completely different occurrence than the one that set the latch.
  const framesBeforeSecond = h.port.posted.length;
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  const second = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeSecond,
  );
  expect(second.payload["gate_occurrence_id"]).toBe("occ_rotate_g2");
  expect(second.payload["event_kind"]).toBe("wall_observed");
  expect(second.payload["observation_id"]).not.toBe(first.payload["observation_id"]);

  // A THIRD call for the SAME (now current) occurrence is still
  // suppressed: the fix re-scopes the latch, it does not remove it.
  const framesBeforeThird = h.port.posted.length;
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  expect(h.port.posted.length).toBe(framesBeforeThird);
});

test("Slice 3: a claim_observation outbox entry replays before any lease-renewing action after a restart, and a duplicate ack clears it without local mutation", async () => {
  const jobA = "job_claim_replay_owner";
  const jobB = "job_claim_replay_cold";
  const candidateA = "cand_replay_a0001";
  const candidateB = "cand_replay_b0001";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA), coldClaimJob(jobB)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, candidateA);
  await seedClaimCandidate(h, jobB, candidateB);
  const tabID = await openClaimWithNewSurface(
    h,
    jobA,
    candidateA,
    `https://${PROVIDER_HOST}/fresh?replay=1`,
  );
  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  const originalRequest = await h.port.waitForFrame("claim_observation");
  const observationID = originalRequest.payload["observation_id"] as string;
  expect(typeof observationID).toBe("string");
  // Never acked here: it survives, unresolved, into the restart below.
  const jobASnapshot = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobA,
  );
  const restarted = restartWorker(h);
  // Neither of these is awaited yet: bridge.start() itself awaits
  // surfaceReady (which drains the outbox first), and openHandoff awaits it
  // too — so job B's own consult cannot send a frame before the replay does.
  const starting = restarted.bridge.start();
  const openingB = restarted.bridge.openHandoff(jobB);
  await restarted.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }),
  );
  // hello_ack without institutional_materialization_v1 wipes every
  // materialization (this bridge never negotiated the daemon-orchestrated
  // materialization workflow); job B's candidate must be re-seeded for
  // this worker lifetime exactly like the first one was.
  await seedClaimCandidate(restarted, jobB, candidateB);

  const replay = await restarted.port.waitForFrame("claim_observation");
  expect(replay.payload["observation_id"]).toBe(observationID);
  expect(replay.payload["event_kind"]).toBe("wall_observed");
  expect(
    restarted.port.posted
      .map(parseBrowserMessage)
      .filter((frame) => frame.type === "authentication_claim_request"),
  ).toEqual([]);

  await restarted.port.inbound(
    observationAck(jobA, replay.payload["request_id"], {
      outcome: "duplicate",
      gate_occurrence_id: "occ_replay_ack01",
      browser_holder_generation: 1,
    }),
  );

  expect(restarted.claimObservationOutbox.current[observationID]).toBeUndefined();
  expect(
    restarted.backend.store.activeJobs.find((job) => job.job_id === jobA),
  ).toEqual(jobASnapshot);

  // Now unblocked: job B's own consult proceeds.
  const claimRequest = await restarted.port.waitForFrame(
    "authentication_claim_request",
  );
  expect(claimRequest.job_id).toBe(jobB);
  await restarted.port.inbound(
    claimResponse(jobB, claimRequest.payload["request_id"], {
      outcome: "park",
      authentication_claim_id: `claim_${candidateB}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${candidateB}`,
      dependent_count: 0,
    }),
  );
  await openingB;
  await starting;
});

test("Slice 3: a job_offer delivered before the startup outbox replay's ack does not stall the inbound chain", async () => {
  const jobA = "job_claim_deadlock_owner";
  const jobD = "job_claim_deadlock_offer";
  const candidateA = "cand_deadlock_a001";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobA)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobA, candidateA);
  const tabID = await openClaimWithNewSurface(
    h,
    jobA,
    candidateA,
    `https://${PROVIDER_HOST}/fresh?deadlock=1`,
  );
  const internals = h.bridge as unknown as {
    emitClaimObservation(
      jobID: string,
      tabID: number,
      eventKind: string,
      latch?: boolean,
    ): Promise<void>;
  };
  await internals.emitClaimObservation(jobA, tabID, "wall_observed", true);
  await h.port.waitForFrame("claim_observation");
  // Never acked: this observation survives, unresolved, into the restart.

  const restarted = restartWorker(h);
  const starting = restarted.bridge.start();
  await restarted.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }),
  );
  // The startup replay's own claim_observation is now in flight, unacked.
  // A job_offer delivered on the SAME serialized inbound FIFO before that
  // ack must still be accepted promptly: the pre-fix bug awaited the
  // replay inside the surfaceReady barrier every job_offer handler also
  // awaits, so the ack — which can only arrive back through this same FIFO
  // — queued behind the offer and neither one ever resolved. If this
  // regresses, the awaited `inbound` call below hangs and the test times
  // out.
  await restarted.port.inbound(jobOffer(jobD));
  expect(
    restarted
      .frames()
      .filter((frame) => frame.type === "job_accept" && frame.job_id === jobD),
  ).toHaveLength(1);

  // The outbox replay itself still completes normally afterward.
  const replay = await restarted.port.waitForFrame("claim_observation");
  expect(replay.job_id).toBe(jobA);
  const observationID = replay.payload["observation_id"] as string;
  await restarted.port.inbound(
    observationAck(jobA, replay.payload["request_id"], {
      outcome: "duplicate",
      gate_occurrence_id: "occ_deadlock_ack01",
      browser_holder_generation: 1,
    }),
  );
  expect(
    restarted.claimObservationOutbox.current[observationID],
  ).toBeUndefined();
  await starting;
});

test("Slice 3: two concurrent automatic drives for the same job produce exactly one consult", async () => {
  const jobID = "job_claim_mint_latch";
  const candidateID = "cand_mint_latch01";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobID)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobID, candidateID);
  const internals = h.bridge as unknown as {
    openFreshHandoff(
      jobID: string,
      job: ActiveJob,
      trigger: "automatic" | "explicit",
    ): Promise<{
      ok: boolean;
      opened?: true;
      error?: { code: string; message: string };
    }>;
  };
  const job = h.backend.store.activeJobs.find((j) => j.job_id === jobID)!;
  const first = internals.openFreshHandoff(jobID, job, "automatic");

  // A second automatic drive races in while the first is still mid-consult
  // (before the daemon's authentication_claim_response arrives): the
  // mintingFreshHandoffs latch is reserved BEFORE consultAuthenticationClaim
  // runs, so it — not the consult outcome — is what makes two racing
  // wakes produce exactly one authentication_claim_request.
  const second = internals.openFreshHandoff(jobID, job, "automatic");
  await expect(second).resolves.toEqual({
    ok: false,
    error: {
      code: "handoff_opening",
      message: "A sign-in surface for this job is already being created",
    },
  });

  const claimRequest = await h.port.waitForFrame(
    "authentication_claim_request",
  );
  expect(claimRequest.payload["trigger"]).toBe("automatic");
  await h.port.inbound(
    claimResponse(jobID, claimRequest.payload["request_id"], {
      outcome: "open_new",
      authentication_claim_id: `claim_${candidateID}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${candidateID}`,
      lease_until: "2030-01-01T00:00:00Z",
    }),
  );
  // Architecture ruling: an automatic drive never self-mints a
  // materialization binding on a granted claim — it stays tabless and
  // holds the grant for the daemon's own Slice 4 pipeline to drive.
  await expect(first).resolves.toEqual({
    ok: false,
    error: {
      code: "handoff_pending",
      message: "The sign-in surface will open once the daemon materializes it",
    },
  });

  expect(h.tabs.created).toHaveLength(0);
  expect(
    h.frames().filter((frame) => frame.type === "authentication_claim_request"),
  ).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toHaveLength(0);
  expect(
    h.backend.store.activeJobs.find((j) => j.job_id === jobID),
  ).toMatchObject({ status: "queued", tab_id: -1 });
});

test("Slice 3: trigger routing — explicit open on a claim owned elsewhere focuses the owner tab, automatic parks", async () => {
  const explicitJobID = "job_claim_trigger_ex";
  const explicitCandidate = "cand_trigger_ex001";
  const ownerTabID = 900;
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(explicitJobID)],
  });
  h.tabs.seed({ id: ownerTabID, url: "https://idp.example.edu/sso" });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, explicitJobID, explicitCandidate);
  const opening = h.bridge.openHandoff(explicitJobID);
  const claimRequest = await h.port.waitForFrame(
    "authentication_claim_request",
  );
  expect(claimRequest.payload["trigger"]).toBe("explicit");
  await h.port.inbound(
    claimResponse(explicitJobID, claimRequest.payload["request_id"], {
      outcome: "focus_owner",
      authentication_claim_id: `claim_${explicitCandidate}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${explicitCandidate}`,
      lease_until: "2030-01-01T00:00:00Z",
      owner_binding_id: "bind_owner_trig01",
      owner_tab_hint: ownerTabID,
    }),
  );
  await expect(opening).resolves.toEqual({ ok: true, opened: true });
  expect(
    h.tabs.updates.some(
      (update) => update.id === ownerTabID && update.properties.active === true,
    ),
  ).toBe(true);
  expect(h.tabs.created).toEqual([]);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === explicitJobID),
  ).toMatchObject({ tab_id: -1, status: "queued" });

  // Automatic: the same situation, driven off an automatic trigger, never
  // disturbs the operator by focusing anything — it parks instead.
  const autoJobID = "job_claim_trigger_auto";
  const autoCandidate = "cand_trigger_auto01";
  const h2 = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(autoJobID)],
  });
  installManagedTabLedger(h2, {});
  await h2.bridge.start();
  await h2.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h2, autoJobID, autoCandidate);
  const internals = h2.bridge as unknown as {
    openFreshHandoff(
      jobID: string,
      job: ActiveJob,
      trigger: "automatic" | "explicit",
    ): Promise<{
      ok: boolean;
      opened?: true;
      error?: { code: string; message: string };
    }>;
  };
  const autoJob = h2.backend.store.activeJobs.find(
    (job) => job.job_id === autoJobID,
  )!;
  const autoOpening = internals.openFreshHandoff(autoJobID, autoJob, "automatic");
  const autoClaimRequest = await h2.port.waitForFrame(
    "authentication_claim_request",
  );
  expect(autoClaimRequest.payload["trigger"]).toBe("automatic");
  await h2.port.inbound(
    claimResponse(autoJobID, autoClaimRequest.payload["request_id"], {
      outcome: "park",
      authentication_claim_id: `claim_${autoCandidate}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${autoCandidate}`,
      dependent_count: 0,
    }),
  );
  await expect(autoOpening).resolves.toEqual({
    ok: false,
    error: {
      code: "handoff_busy",
      message: "Another sign-in for this institution is already in progress",
    },
  });
  expect(h2.tabs.created).toEqual([]);
  expect(h2.tabs.updates).toEqual([]);
});

test("Slice 3: navigation_error on a claim-owned handoff tab emits the observation and charges no local auth attempt", async () => {
  const jobID = "job_claim_naverr";
  const candidateID = "cand_naverr_0001";
  const idpURL = "https://idp.example.edu/sso";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(jobID)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobID, candidateID);
  const tabID = await openClaimWithNewSurface(
    h,
    jobID,
    candidateID,
    `https://${PROVIDER_HOST}/fresh?naverr=1`,
  );

  const framesBeforeError = h.port.posted.length;
  await h.webNavigation.emitError(tabID);
  await h.tabs.completeNavigation(tabID, idpURL);

  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(false);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.status,
  ).toBe("accepted");
  const observation = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeError,
  );
  expect(observation.payload["event_kind"]).toBe("navigation_error");
});

test("Scenario 5: navigation_error observation, once the daemon acks it applied, auto-closes the exhausted scaffold through the real close-authorization transaction and detaches the job via onTabRemoved — no local park response, no re-mint after restart", async () => {
  const jobID = "job_scenario5_naverr";
  const candidateID = "cand_scenario5_0001";
  const idpURL = "https://idp.example.edu/sso";
  const h = makeHarness(
    { ...emptyStore(), activeJobs: [coldClaimJob(jobID)] },
    { windows: true },
  );
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"] }),
  );
  await seedClaimCandidate(h, jobID, candidateID);
  const tabID = await openClaimWithNewSurface(
    h,
    jobID,
    candidateID,
    `https://${PROVIDER_HOST}/fresh?s5=1`,
  );

  // Observed before generic auth detection, no auth charge, no cooldown.
  const framesBeforeError = h.port.posted.length;
  await h.webNavigation.emitError(tabID);
  await h.tabs.completeNavigation(tabID, idpURL);
  expect(h.frames().some((frame) => frame.type === "auth_pending")).toBe(false);
  const observation = await h.port.waitForFrame(
    "claim_observation",
    framesBeforeError,
  );
  expect(observation.payload["event_kind"]).toBe("navigation_error");

  // The operator has moved on (focus elsewhere) by the time the daemon
  // commits this; an operator-active tab is never auto-closed (Slice 2b).
  h.tabs.patch(tabID, { active: false });
  const framesBeforeAck = h.port.posted.length;
  await h.port.inbound(
    observationAck(jobID, observation.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_s5_naverr",
      browser_holder_generation: 1,
    }),
  );

  // The ack itself — not a separate park response — is the trigger: §2.3's
  // auto-close wiring fires the standard 2b close transaction on this now-
  // exhausted scaffold, disposed claim_abandoned, entirely off the wire.
  const closeRequest = await h.port.waitForFrame(
    "surface_close_request",
    framesBeforeAck,
  );
  expect(closeRequest.payload["disposition"]).toBe("claim_abandoned");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: closeRequest.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-s5-0001",
      nonce: "nonce-s5-0001",
      browser_holder_generation: 1,
    }),
  );
  // The authorization's own removal runs off the wire response, not this
  // await: let its fire-and-forget continuation settle before asserting.
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
  // The job detaches from its now-gone tab through the real onTabRemoved
  // path — never a manually poked tab_id.
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobID),
  ).toMatchObject({ tab_id: -1 });
  // Tombstone consumed, no job-cancel emitted.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.frames().some((f) => f.type === "job_reject")).toBe(false);

  // Classify exhaustion survives a worker restart: the worker-local retry
  // map is allowed to reset (a fresh Bridge instance has none), but the
  // daemon-committed close is durable — the extension never re-mints a
  // replacement scaffold for this job on its own after restart.
  const tabsCreatedBeforeRestart = h.tabs.created.length;
  const restarted = restartWorker(h);
  await restarted.bridge.start();
  await restarted.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"] }),
  );
  expect(restarted.tabs.created).toHaveLength(tabsCreatedBeforeRestart);
});

test("Scenario 5 at the teardown boundary (oracle finding 5): the worker dies right after onErrorOccurred, before the document ever settles — restart still charges no auth attempt and still reaches a durable terminal disposition", async () => {
  const jobID = "job_scenario5_teardown";
  const candidateID = "cand_scenario5_td01";
  const idpURL = "https://idp.example.edu/sso";
  const h = makeHarness(
    { ...emptyStore(), activeJobs: [coldClaimJob(jobID)] },
    { windows: true },
  );
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"] }),
  );
  await seedClaimCandidate(h, jobID, candidateID);
  const tabID = await openClaimWithNewSurface(
    h,
    jobID,
    candidateID,
    `https://${PROVIDER_HOST}/fresh?s5td=1`,
  );

  // The top-frame load fails — the ONLY signal Chrome delivers before this
  // worker dies. In real Chrome the document goes on to settle at the IdP's
  // error page almost immediately, but that onUpdated "complete" event is
  // delivered to whichever listener is alive AT THAT MOMENT: a dead worker
  // never receives it, and Chrome never replays it later. `patch` (not
  // `completeNavigation`) models exactly that — the browser-side tab state
  // moves on without ever firing the event this now-dead worker would have
  // consumed.
  await h.webNavigation.emitError(tabID);
  h.tabs.patch(tabID, { url: idpURL, status: "complete" });

  const restarted = restartWorker(h);
  await restarted.bridge.start();
  await restarted.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"] }),
  );

  // The daemon-visible auth-attempt count is unchanged: no auth_pending (or
  // any other attempt-charging frame) was ever sent for this dead end, even
  // though the new worker's own navigationErrors map started out empty —
  // the durable marker, not worker memory, is what excluded it.
  expect(restarted.frames().some((f) => f.type === "auth_pending")).toBe(false);

  const observation = await restarted.port.waitForFrame("claim_observation");
  expect(observation.payload["event_kind"]).toBe("navigation_error");
  expect(observation.payload["binding_id"]).toBeDefined();
  expect(observation.job_id).toBe(jobID);

  // The operator has moved on by the time the daemon commits this; an
  // operator-active tab is never auto-closed (Slice 2b) — same guard as the
  // same-worker path exercises.
  restarted.tabs.patch(tabID, { active: false });
  const framesBeforeAck = restarted.port.posted.length;
  await restarted.port.inbound(
    observationAck(jobID, observation.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "occ_s5td_naverr",
      browser_holder_generation: 1,
    }),
  );

  // The scaffold reaches a durable terminal disposition: the standard
  // close-authorization transaction fires for the now-exhausted scaffold,
  // exactly as it would if the same-worker path had classified this
  // navigation error itself — proving the exclusion never depended on this
  // worker's memory surviving the teardown.
  const closeRequest = await restarted.port.waitForFrame(
    "surface_close_request",
    framesBeforeAck,
  );
  expect(closeRequest.payload["disposition"]).toBe("claim_abandoned");
  await restarted.port.inbound(
    nativeResult("surface_close_response", {
      request_id: closeRequest.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-s5td-0001",
      nonce: "nonce-s5td-0001",
      browser_holder_generation: 1,
    }),
  );
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  expect(restarted.tabs.removed).toContain(tabID);
  expect(restarted.tabs.snapshot(tabID)).toBeUndefined();
  expect(
    restarted.backend.store.activeJobs.find((job) => job.job_id === jobID),
  ).toMatchObject({ tab_id: -1 });
});

test("Scenario 7: Firefox <139 (no tabGroups) — the scaffold lands in work-window mode, ownership stays binding-based, and full event-page teardown at each protocol boundary still resumes from durable state", async () => {
  const jobID = "job_scenario7_ff138";
  const candidateID = "cand_scenario7_0001";
  const h = makeHarness(
    { ...emptyStore(), activeJobs: [coldClaimJob(jobID)] },
    { windows: true },
  );
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h, jobID, candidateID);
  const tabID = await openClaimWithNewSurface(
    h,
    jobID,
    candidateID,
    `https://${PROVIDER_HOST}/fresh?s7=1`,
  );

  // No tabGroups API at all (pre-139 Firefox): group identity degrades to
  // the dedicated work window, never a bare in-window tab.
  expect(h.tabGroups).toBeUndefined();
  expect(h.windows?.created).toHaveLength(1);
  const workWindowID = h.backend.store.workWindowID;
  expect(workWindowID).toBeDefined();
  expect(h.tabs.snapshot(tabID)?.windowId).toBe(workWindowID);

  // Full teardown AFTER the claim -> scaffold -> navigate sequence: the
  // still-live, still-tracked tab needs no reconciliation action.
  const tabsCreatedBeforeRestart = h.tabs.created.length;
  const windowsCreatedBeforeRestart = h.windows?.created.length ?? 0;
  const restarted = restartWorker(h);
  await restarted.bridge.start();
  await restarted.port.inbound(
    helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }),
  );
  expect(restarted.tabs.created).toHaveLength(tabsCreatedBeforeRestart);
  expect(restarted.windows?.created).toHaveLength(windowsCreatedBeforeRestart);
  expect(
    restarted.backend.store.activeJobs.find((j) => j.job_id === jobID),
  ).toMatchObject({ tab_id: tabID, status: "accepted" });

  // A second, unrelated institutional job after the teardown reuses the
  // SAME work window by its durable id — group/window identity degrades
  // structurally (there is no title-based work-window lookup to begin
  // with, unlike tab groups' own title-matching risk already fenced in
  // Slice 2b), never becoming an implicit "ownership" claim over some
  // other window that merely happens to still be alive.
  const jobID2 = "job_scenario7_ff138_second";
  const candidateID2 = "cand_scenario7_0002";
  const offer2 = jobOffer(jobID2) as { payload: Record<string, unknown> };
  offer2.payload["requires_auth"] = true;
  await restarted.port.inbound(offer2);
  await seedClaimCandidate(restarted, jobID2, candidateID2);
  const tabID2 = await openClaimWithNewSurface(
    restarted,
    jobID2,
    candidateID2,
    `https://${PROVIDER_HOST}/fresh?s7=2`,
  );
  expect(restarted.tabs.snapshot(tabID2)?.windowId).toBe(workWindowID);
  expect(restarted.windows?.created).toHaveLength(windowsCreatedBeforeRestart);
});

/** hello_ack's institutional-materialization resume path awaits
 * reconcileMaterializationTabs() before ever calling scheduleMaterialization
 * again (background.ts's hello_ack handler) — every job with a live
 * binding_id gets folded into an institutional_reconcile_request the
 * daemon must answer first. Left unanswered, that await never settles and
 * the resumed worker never re-drives its pending protocol step. Drains
 * whatever is currently posted (a no-op when no materialization has a
 * binding_id yet, e.g. right after a claim-boundary restart) — asserting
 * along the way that each live binding was folded into exactly ONE
 * reconcile request this handshake (background.ts's hello_ack handler used
 * to call reconcileMaterializationTabs() twice per handshake, doubling
 * every institutional_reconcile_request; now deterministic). */
async function drainReconcileRequests(h: Harness): Promise<void> {
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  const requests = h
    .frames()
    .filter((frame) => frame.type === "institutional_reconcile_request");
  const bindingRequestCounts = new Map<string, number>();
  for (const req of requests) {
    const bindings = req.payload["bindings"];
    if (!Array.isArray(bindings)) continue;
    for (const binding of bindings) {
      const bindingID = (binding as Record<string, unknown>)["binding_id"];
      if (typeof bindingID !== "string") continue;
      bindingRequestCounts.set(
        bindingID,
        (bindingRequestCounts.get(bindingID) ?? 0) + 1,
      );
    }
  }
  for (const count of bindingRequestCounts.values()) expect(count).toBe(1);
  for (const req of requests) {
    await h.port.inbound(
      nativeResult("institutional_reconcile_response", {
        request_id: req.payload["request_id"],
        outcome: "reconciled",
      }),
    );
  }
}

test("Scenario 7 continued: institutional materialization survives a worker restart at each of the five protocol boundaries (claim, bind, route, navigate, ack), Firefox seam throughout, ownership never title-based", async () => {
  // A durable-store baseline: an explicit direct-mint job proves the
  // work-window fallback still holds by its numeric id, never a
  // title-based lookup — the same regression Scenario 7's original body
  // pins, retained here as a sibling check alongside the new per-boundary
  // coverage below.
  const jobID0 = "job_scenario7b_explicit";
  const candidateID0 = "cand_scenario7b_0000";
  const h0 = makeHarness(
    { ...emptyStore(), activeJobs: [coldClaimJob(jobID0)] },
    { windows: true },
  );
  installManagedTabLedger(h0, {});
  await h0.bridge.start();
  await h0.port.inbound(helloAck({ features: ["handoff_link_v1", AUTH_CLAIM] }));
  await seedClaimCandidate(h0, jobID0, candidateID0);
  const tab0 = await openClaimWithNewSurface(
    h0,
    jobID0,
    candidateID0,
    `https://${PROVIDER_HOST}/fresh?s7b=explicit`,
  );
  expect(h0.tabGroups).toBeUndefined();
  const workWindowID = h0.backend.store.workWindowID;
  expect(workWindowID).toBeDefined();
  expect(h0.tabs.snapshot(tab0)?.windowId).toBe(workWindowID);

  // Five independent institutional materialization pipelines, each
  // restarted at a different protocol boundary before any daemon reply
  // for that boundary's outstanding request ever arrives — durable
  // correlation state (never the dead worker's own in-flight promise)
  // must drive resumption, and delivery must still complete.
  const FEATURES = [
    "institutional_materialization_v1",
    "effect_permit_v1",
    "surface_close_v1",
  ];
  const BOUNDARIES = ["claim", "bind", "route", "navigate", "ack"] as const;
  for (const restartAt of BOUNDARIES) {
    const jobID = `job_scenario7b_${restartAt}`;
    const candidateID = `cand_scenario7b_${restartAt}`;
    const bindingID = `bind_scenario7b_${restartAt}`;
    const claimID = `claim_scenario7b_${restartAt}`;
    let h = makeHarness(
      { ...emptyStore(), activeJobs: [materializationActiveJob(jobID)] },
      { windows: true },
    );
    installManagedTabLedger(h, {});
    await h.bridge.start();
    await h.port.inbound(helloAck({ features: FEATURES }));
    expect(h.tabGroups).toBeUndefined();

    await h.port.inbound(candidateOffer(jobID, candidateID));
    let claimReq = await h.port.waitForFrame("institutional_claim_request");
    if (restartAt === "claim") {
      h = restartWorker(h);
      await h.bridge.start();
      await h.port.inbound(helloAck({ features: FEATURES }));
      await drainReconcileRequests(h);
      claimReq = await h.port.waitForFrame("institutional_claim_request");
    }
    expect(claimReq.payload["candidate_id"]).toBe(candidateID);
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_claim_response",
      msg_id: `claim_response_${restartAt}`,
      job_id: jobID,
      seq: 3,
      payload: {
        request_id: claimReq.payload["request_id"],
        outcome: "claimed",
        candidate_id: candidateID,
        claim_id: claimID,
        binding_id: bindingID,
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
      },
    });

    let bindReq = await h.port.waitForFrame("institutional_bind_request");
    if (restartAt === "bind") {
      h = restartWorker(h);
      await h.bridge.start();
      await h.port.inbound(helloAck({ features: FEATURES }));
      await drainReconcileRequests(h);
      bindReq = await h.port.waitForFrame("institutional_bind_request");
    }
    expect(bindReq.payload["binding_id"]).toBe(bindingID);
    const scaffoldMatches = h.tabs
      .list()
      .filter(
        (tab) =>
          tab.url === `chrome-extension://test/dist/materialize.html#${bindingID}`,
      );
    expect(scaffoldMatches).toHaveLength(1);
    const scaffoldTabID = scaffoldMatches[0]!.id!;
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_bind_response",
      msg_id: `bind_response_${restartAt}`,
      job_id: jobID,
      seq: 4,
      payload: {
        request_id: bindReq.payload["request_id"],
        outcome: "bound",
        claim_id: claimID,
        binding_id: bindingID,
      },
    });

    let routeReq = await h.port.waitForFrame("institutional_route_request");
    if (restartAt === "route") {
      h = restartWorker(h);
      await h.bridge.start();
      await h.port.inbound(helloAck({ features: FEATURES }));
      await drainReconcileRequests(h);
      routeReq = await h.port.waitForFrame("institutional_route_request");
    }
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_route_response",
      msg_id: `route_response_${restartAt}`,
      job_id: jobID,
      seq: 5,
      payload: {
        request_id: routeReq.payload["request_id"],
        outcome: "issued",
        claim_id: claimID,
        binding_id: bindingID,
        route_issuance_ordinal: 1,
        effect_ordinal:
          (routeReq.payload["expected_effect_ordinal"] as number) + 1,
        institutional_request_id: routeReq.payload["institutional_request_id"],
        url: `https://${PROVIDER_HOST}/${restartAt}`,
      },
    });

    let navigatedReq = await h.port.waitForFrame(
      "institutional_navigated_request",
    );
    if (restartAt === "navigate") {
      h = restartWorker(h);
      await h.bridge.start();
      await h.port.inbound(helloAck({ features: FEATURES }));
      await drainReconcileRequests(h);
      navigatedReq = await h.port.waitForFrame(
        "institutional_navigated_request",
      );
    }
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "institutional_navigated_response",
      msg_id: `navigated_response_${restartAt}`,
      job_id: jobID,
      seq: 6,
      payload: {
        request_id: navigatedReq.payload["request_id"],
        outcome: "acknowledged",
        claim_id: claimID,
        binding_id: bindingID,
      },
    });
    for (let i = 0; i < 20; i += 1) await Promise.resolve();
    expect(h.backend.store.materializations?.[jobID]).toMatchObject({
      phase: "navigated",
    });

    // The flow completes at every restart point: delivery reaches the
    // researcher's scaffold tab regardless of which boundary a worker
    // death lands on, and no duplicate scaffold was ever minted — the
    // SAME tab that started as the scaffold now carries the resolver URL.
    expect(h.tabs.snapshot(scaffoldTabID)?.url).toBe(
      `https://${PROVIDER_HOST}/${restartAt}`,
    );
    expect(
      h.tabs
        .list()
        .filter((tab) => tab.url?.includes(`materialize.html#${bindingID}`)),
    ).toHaveLength(0);
    // Fix A (materializeScaffold now routes through openBrokerTab): the
    // scaffold's own work window is created exactly once for this job —
    // never re-created across however many restarts this boundary
    // exercises, and never duplicated.
    expect(h.windows?.created ?? []).toHaveLength(1);
    expect(h.tabGroups).toBeUndefined();

    if (restartAt === "ack") {
      h = restartWorker(h);
      await h.bridge.start();
      await h.port.inbound(helloAck({ features: FEATURES }));
      await drainReconcileRequests(h);
      // Fix B (lastKnownBrowserHolderGeneration is now rehydrated at
      // start() from the surviving materialization correlation's own
      // browser_holder_generation, background.ts ~5321-5449): a worker
      // death after the claim step, with no further claim consult in this
      // worker's remaining lifetime, no longer permanently blocks close
      // authorization — the ack below still drives materialization_
      // settled retirement through to a real close, exactly as it does at
      // every other boundary.
      await h.port.inbound({
        protocol: "papio-browser/1",
        type: "ack",
        msg_id: `ack_frame_${restartAt}`,
        job_id: jobID,
        seq: 7,
        payload: {},
      });
      const closeRequest = await h.port.waitForFrame("surface_close_request");
      expect(closeRequest.payload["disposition"]).toBe(
        "materialization_settled",
      );
      expect(closeRequest.payload["binding_id"]).toBe(bindingID);
      await h.port.inbound(
        nativeResult("surface_close_response", {
          request_id: closeRequest.payload["request_id"],
          outcome: "authorized",
          close_authorization_id: `auth_${restartAt}`,
          nonce: `nonce_${restartAt}`,
          browser_holder_generation: 1,
        }),
      );
      for (let i = 0; i < 20; i += 1) await Promise.resolve();
      expect(h.tabs.removed).toContain(scaffoldTabID);
      expect(h.tabs.snapshot(scaffoldTabID)).toBeUndefined();
    }
  }
});


test("a cold engagement re-requests its URL after a service-worker restart", async () => {
  const jobID = "job_fresh_link_restart";
  const first = makeHarness();
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = "https://idp.example.edu/entity";
  await first.bridge.start();
  await first.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await first.port.inbound(offer);
  const seed = JSON.parse(JSON.stringify(first.backend.store)) as StoreShape;

  const restarted = makeHarness(seed);
  await restarted.bridge.start();
  await restarted.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  const opening = restarted.bridge.openHandoff(jobID);
  const request = await restarted.port.waitForFrame("handoff_link_request");
  expect(request.payload["job_id"]).toBe(jobID);
  await restarted.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: request.payload["request_id"],
      outcome: "unavailable",
      detail: "resolver action disappeared",
    }),
  );

  await expect(opening).resolves.toMatchObject({
    ok: false,
    error: { code: "unavailable" },
  });
  expect(restarted.tabs.created).toEqual([]);
});

test("a failed fresh-link mint rolls back its institution claim without opening a tab", async () => {
  const jobID = "job_fresh_link_failure";
  const entityID = "https://idp.example.edu/entity";
  const h = makeHarness();
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = entityID;
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["handoff_link_v1"] }));
  await h.port.inbound(offer);

  const opening = h.bridge.openHandoff(jobID);
  const request = await h.port.waitForFrame("handoff_link_request");
  await h.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: request.payload["request_id"],
      outcome: "unavailable",
      detail: "resolver action disappeared",
    }),
  );

  await expect(opening).resolves.toEqual({
    ok: false,
    error: {
      code: "unavailable",
      message: "The daemon could not mint a fresh handoff URL",
    },
  });
  expect(h.tabs.created).toEqual([]);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    tab_id: -1,
    engagement_required: true,
  });
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
    if (requiresAuth !== undefined)
      offer.payload["requires_auth"] = requiresAuth;

    await h.bridge.start();
    await h.port.inbound(offer);

    expect(h.tabs.created).toEqual([{ url: OPENURL, active: true }]);
    expect(h.backend.store.activeJobs[0]).toMatchObject({
      tab_id: 100,
      status: "accepted",
    });
  }
});

test("an OA completion cannot release a queued institutional handoff", async () => {
  const h = makeHarness();
  const institutionalURL =
    "https://resolver.example.edu/openurl?ctx=institutional";
  const openAccessURL = "https://oa.example.edu/article/123";
  const institutional = jobOffer(
    "job_0001a_institutional",
    institutionalURL,
  ) as {
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
  const openAccessTabID =
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_0001a_open_access",
    )?.tab_id ?? -1;
  const providerURL = `https://${PROVIDER_HOST}/stable/open-access`;
  await h.tabs.completeNavigation(openAccessTabID, providerURL);

  expect(h.tabs.created).toEqual([{ url: openAccessURL, active: true }]);
  expect(h.bridge.latestOpenURL()).toBe(institutionalURL);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_0001a_institutional",
    ),
  ).toMatchObject({
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
  const firstTabID =
    h.backend.store.activeJobs.find((job) => job.tab_id >= 0)?.tab_id ?? -1;

  await h.tabs.completeNavigation(firstTabID, OPENURL);
  // The landing is warm evidence for this resolver, but the first drive still
  // owns the sole effect slot. Settle that owner before asserting the FIFO
  // successor, rather than inventing a concurrent second drive.
  await h.bridge.requestCancel(jobIDs[0]!);
  const internals = h.bridge as unknown as {
    releaseQueuedHandoffs: () => Promise<void>;
  };
  await internals.releaseQueuedHandoffs.call(h.bridge);

  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
  ]);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(1);
  expect(h.backend.store.lastAuthReturnedAt).toBeUndefined();
  expect(h.frames().some((frame) => frame.type === "auth_returned")).toBe(
    false,
  );
});
test("a tracked resolver landing routes its electronic service only with origin permission", async () => {
  const h = makeHarness();
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.permissions.contains = async ({ origins }) =>
    origins.length === 1 && origins[0] === "https://resolver.example.edu/*";
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "routed", service: "JSTOR scholarly archive" } }];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_route"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, OPENURL);

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
  await denied.tabs.completeNavigation(deniedTabID, OPENURL);
  expect(injectedWithoutPermission).toBe(false);
});

test("a fresh resolver link routes after its one-use URL is retired", async () => {
  const jobID = "job_fresh_resolver_route";
  const entityID = "https://idp.example.edu/entity";
  const freshURL = "https://resolver.example.edu/openurl?fresh=private";
  const h = makeHarness();
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.permissions.contains = async ({ origins }) =>
    origins.length === 1 && origins[0] === "https://resolver.example.edu/*";
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "routed", service: "JSTOR scholarly archive" } }];
  };
  const offer = jobOffer(jobID) as { payload: Record<string, unknown> };
  offer.payload["requires_auth"] = true;
  offer.payload["login_entity_id"] = entityID;

  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["handoff_link_v1"],
      resolver_origins: ["https://resolver.example.edu"],
    }),
  );
  await h.port.inbound(offer);
  const opening = h.bridge.openHandoff(jobID);
  const request = await h.port.waitForFrame("handoff_link_request");
  await h.port.inbound(
    nativeResult("handoff_link_result", {
      request_id: request.payload["request_id"],
      outcome: "opened",
      url: freshURL,
    }),
  );
  await expect(opening).resolves.toEqual({ ok: true, opened: true });

  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, freshURL);
  expect(injections).toHaveLength(1);
  expect(injections[0]?.func).toBe(routeResolverService);
});

test("a resolver no-entitlement route emits once and short-circuits provider classification", async () => {
  const h = makeHarness();
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.permissions.contains = async () => true;
  h.deps.adapterSpecs.push({
    ...PROVIDER_ADAPTER,
    id: "resolver-provider",
    hosts: ["resolver.example.edu"],
  });
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    return [{ result: { kind: "no_entitlement" } }];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_no_entitlement"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, OPENURL);
  await h.tabs.completeNavigation(tabID, OPENURL);

  const outcomes = h
    .frames()
    .filter(
      (frame) =>
        frame.type === "provider_outcome" &&
        frame.payload["outcome"] === "no_entitlement",
    );
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toEqual({ outcome: "no_entitlement" });
  expect(injections).toHaveLength(1);
  expect(
    injections.every((injection) => injection.func === routeResolverService),
  ).toBe(true);
});

test("a resolver no-service route stays assisted without an outcome", async () => {
  const h = makeHarness();
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async () => [
    { result: { kind: "no_service" } },
  ];

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_resolver_no_service"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, OPENURL);

  expect(h.frames().some((frame) => frame.type === "provider_outcome")).toBe(
    false,
  );
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
      return [
        {
          result: {
            html: '<main class="article">unsupported provider shape</main>',
            origin: `https://${PROVIDER_HOST}`,
            path: "/stable/article",
          },
        },
      ];
    }
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  await h.port.inbound(
    jobOfferForHosts("job_missing_adapter", ["resolver.example.edu"]),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  h.tabs.seed({ id: tabID, url: articleURL });
  let nextTimer = h.timers.length;
  await h.tabs.completeNavigation(tabID, articleURL);

  for (let retry = 0; retry < 2; retry += 1) {
    const relative = h.timers
      .slice(nextTimer)
      .findIndex((timer) => timer.ms === 2_500);
    expect(relative).toBeGreaterThanOrEqual(0);
    nextTimer += relative;
    const timer = h.timers[nextTimer]!;
    nextTimer += 1;
    h.clock.now += 2_500;
    await timer.fn();
  }

  expect(
    h.frames().filter((frame) => frame.type === "page_capture"),
  ).toHaveLength(1);
  const outcomes = h
    .frames()
    .filter((frame) => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toMatchObject({
    outcome: "ui_changed",
    detail:
      "No source-controlled adapter matched this provider page. " +
      "A sanitized diagnostic was saved locally for adapter development.",
  });
  // The daemon opens a manual_download action from exactly this outcome, so the
  // job survives as an inert correlation window rather than being deleted: it
  // detaches from its tab, holds no drive authority, and keeps only the hosts
  // correlate() needs to claim the researcher's own download.
  expect(h.backend.store.activeJobs).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: "job_missing_adapter",
    tab_id: -1,
    status: "awaiting_download",
  });
  expect(h.backend.store.activeJobs[0]?.access_mode).toBeUndefined();
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
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    if (injection.func === planExecution)
      return plannerResult(injection, { kind: "unknown" });
    if (injection.func === capturePage) {
      return [
        {
          result: {
            html: `<main class="article">new provider shape</main>`,
            origin: `https://${PROVIDER_HOST}`,
            path: "/stable/article",
          },
        },
      ];
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
  await expect(
    h.bridge.openHandoff("job_0001a_registry_host"),
  ).resolves.toEqual({ ok: true, opened: true });
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

  expect(
    injections.some(
      (i) => i.func === planExecution && i.target.tabId === tabID,
    ),
  ).toBe(true);
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
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.permissions.contains = async ({ origins }) => {
    permissionQueries.push(origins);
    // Chrome answers true for this exact-origin query when the user granted
    // optional https://*/* access; no host-specific grant is required to read it.
    return true;
  };
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    if (injection.func === planExecution)
      return plannerResult(injection, { kind: "unknown" });
    if (injection.func === isBotChallenge) return [{ result: false }];
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_all_sites_provider_access"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

  expect(permissionQueries).toEqual([[`https://${PROVIDER_HOST}/*`]]);
  expect(injections.some((injection) => injection.func === planExecution)).toBe(
    true,
  );
  expect(h.backend.store.blockedProviderHosts).toBeUndefined();
});

test("missing provider access stays actionable and resumes the exact tab after grant", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
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
  h.tabs.seed({ id: tabID, url: articleURL });
  await h.tabs.completeNavigation(tabID, articleURL);

  expect(
    h.frames().filter((frame) => frame.type === "provider_outcome"),
  ).toHaveLength(0);
  expect(h.backend.store.activeJobs).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.blocked_provider_host).toBe(
    PROVIDER_HOST,
  );
  expect(injections).toEqual([]);
  expect(h.backend.store.blockedProviderHosts).toEqual([PROVIDER_HOST]);
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toContain(PROVIDER_HOST);

  granted = true;
  await h.bridge.onPermissionsChanged();
  expect(injections.some((injection) => injection.func === planExecution)).toBe(
    true,
  );
  expect(h.backend.store.blockedProviderHosts).toEqual([]);
  expect(h.backend.store.activeJobs[0]?.blocked_provider_host).toBeUndefined();
});

test("a classify retry whose handoff tab closed settles instead of rejecting", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push(PROVIDER_ADAPTER);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === planExecution) {
      return plannerResult(injection, { kind: "unknown" });
    }
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: "normal" } }];
    return [];
  };

  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(jobOffer("job_retry_after_tab_close"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  h.tabs.seed({ id: tabID, url: articleURL });
  await h.tabs.completeNavigation(tabID, articleURL);

  const scheduled = h.timers.findIndex((timer) => timer.ms === 2_500);
  expect(scheduled).toBeGreaterThanOrEqual(0);

  // The operator closes the handoff before the retry fires. The retry is a bare
  // timer callback, so a rejection here escapes as an unhandled rejection.
  await h.tabs.userClose(tabID);
  h.clock.now += 2_500;
  await h.timers[scheduled]?.fn();

  expect(
    h.frames().filter((frame) => frame.type === "provider_outcome"),
  ).toHaveLength(0);
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
    await h.tabs.completeNavigation(tabID, articleURL);
  }

  expect(permissionChecks).toBe(1);
  expect(
    h.frames().filter((frame) => frame.type === "provider_outcome"),
  ).toHaveLength(0);
  expect(h.backend.store.blockedProviderHosts).toEqual([PROVIDER_HOST]);
  expect(
    h.action.titles.filter((title) => title.includes(PROVIDER_HOST)),
  ).toHaveLength(1);
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
  const tabID = await classifyProviderUnknown(
    h,
    "job_challenge_clears_unknown",
  );

  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);
  await h.port.inbound(helloAck());
  challenge = true;
  h.clock.now += 5_000;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  await h.tabs.completeNavigation(tabID, url);

  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "jstor.org",
    challenge_kind: "cloudflare",
  });
  expect(h.backend.store.challengeCooldowns).toEqual({
    "jstor.org": h.clock.now + 600_000,
  });
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
  expect(
    h
      .frames()
      .find(
        (frame) =>
          frame.type === "error" &&
          frame.payload["code"] === "challenge_blocked",
      ),
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
  await h.tabs.completeNavigation(tabID, url);
  for (let step = 0; step < 3; step += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }

  const outcomes = h
    .frames()
    .filter((frame) => frame.type === "provider_outcome");

  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]?.payload).toMatchObject({
    outcome: "ui_changed",
    adapter_version: PROVIDER_ADAPTER.version,
    adapter_id: PROVIDER_ADAPTER.id,
  });
  expect(h.backend.store.challengeCooldowns).toEqual({});
  expect(h.backend.store.activeJobs[0]?.challenge_blocked).toBeUndefined();
});
test("challenge resume queues without a governor slot before classifying", async () => {
  let challenge = true;
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => challenge);
  const challengeTabID = await classifyProviderUnknown(
    h,
    "job_challenge_resume_queued",
  );
  const ownerOffer = jobOfferForHosts("job_resume_slot_a", [
    "link.springer.com",
  ]) as {
    payload: Record<string, unknown>;
  };
  ownerOffer.payload["requires_auth"] = false;
  await h.port.inbound(ownerOffer);
  const queuedOffer = jobOfferForHosts("job_resume_slot_b", [
    "link.springer.com",
  ]) as {
    payload: Record<string, unknown>;
  };
  queuedOffer.payload["requires_auth"] = true;
  await h.port.inbound(queuedOffer);
  // The challenge parked its own tab and released its slot, so the first
  // unrelated offer may legitimately claim it; the second stays FIFO-queued.
  expect(h.tabs.list().length).toBe(2);

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
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: "normal" } }];
    if (injection.func === planExecution) {
      return plannerResult(injection, {
        kind: "article",
        adapter_id: spec.id,
        adapter_version: spec.version,
        evidence: [],
      });
    }
    if (injection.func === executePlannedPageEffect)
      return plannedEffectResult(injection);
    return [{ result: "https://download.example/paper.pdf" }];
  };

  challenge = false;
  const url = `https://${PROVIDER_HOST}/stable/challenge`;
  await h.tabs.completeNavigation(challengeTabID, url);

  expect(h.downloads.started).toHaveLength(0);
  const resumed = h.backend.store.activeJobs.find(
    (job) => job.job_id === "job_challenge_resume_queued",
  );
  expect(resumed).toMatchObject({ tab_id: challengeTabID, status: "accepted" });
  expect(resumed?.challenge_blocked).toBeUndefined();
  const resumedInternals = h.bridge as unknown as {
    handoffDriveQueue: Array<{ jobID: string }>;
    handoffDrives: Map<string, unknown>;
  };
  expect(
    resumedInternals.handoffDrives.has("job_challenge_resume_queued"),
  ).toBe(false);
  expect(
    resumedInternals.handoffDriveQueue.some(
      (request) => request.jobID === "job_challenge_resume_queued",
    ),
  ).toBe(true);
  // The unrelated owner remains effectful while the resumed challenge waits
  // for a governor slot; no second classification or download is allowed.
  expect(h.tabs.list().length).toBe(2);
});
test("driven-page assessment classifies challenge, redirect-loop, and normal fixtures", () => {
  const fixture = (html: string): Document => {
    const window = new Window({ url: "https://www.jstor.org/stable/paper" });
    window.document.write(html);
    return window.document as unknown as Document;
  };
  const challengeFixtures = [
    "<html><head><title>Are you a robot?</title></head><body></body></html>",
    '<form id="challenge-form"></form>',
    '<div class="cf-turnstile"></div>',
    '<div id="data-cf-chl-token"></div>',
    "<main>Verify you are human to continue</main>",
  ];
  for (const html of challengeFixtures) {
    const doc = fixture(html);
    expect(assessDrivenPage(doc).kind).toBe("challenge");
    expect(isBotChallenge(doc)).toBe(true);
  }
  const loop = fixture(
    "<html><head><title>OpenAthens</title></head><body>Too many redirects</body></html>",
  );
  expect(assessDrivenPage(loop).kind).toBe("redirect_loop");
  expect(isRedirectLoopPage(loop)).toBe(true);
  const normal = fixture(
    "<html><head><title>Article</title></head><body><main>Abstract</main></body></html>",
  );
  expect(assessDrivenPage(normal)).toEqual({ kind: "normal" });
  expect(isBotChallenge(normal)).toBe(false);
  expect(isRedirectLoopPage(normal)).toBe(false);
  expect(registrableProviderHost("www.sciencedirect.com")).toBe(
    "sciencedirect.com",
  );
  expect(registrableProviderHost("journals.example.co.uk")).toBe(
    "example.co.uk",
  );
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
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "provider_outcome" &&
          frame.payload["outcome"] === "ui_changed",
      ),
  ).toHaveLength(1);

  // A re-offer whose previous tab is gone creates a genuinely new provider
  // drive, which may legitimately produce its own terminal observation.
  await h.tabs.userClose(firstTabID);
  await h.port.inbound(jobOffer(jobID));
  const secondTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(secondTabID).not.toBe(firstTabID);
  const articleURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.seed({ id: secondTabID, url: articleURL });
  await h.tabs.completeNavigation(secondTabID, articleURL);
  for (let retry = 0; retry < 2; retry += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }

  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "provider_outcome" &&
          frame.payload["outcome"] === "ui_changed",
      ),
  ).toHaveLength(2);
});

/** Drive one handoff all the way to the `ui_changed` provider outcome — the
 * outcome from which the daemon opens a `manual_download` human action. Two
 * spaced unknown observations are what escalates; the retry timers are how the
 * fake clock reaches the second one. */
async function driveToManualDownloadOutcome(
  h: Harness,
  jobID: string,
): Promise<number> {
  const tabID = await classifyProviderUnknown(h, jobID);
  for (let retry = 0; retry < 2; retry += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }
  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "provider_outcome" &&
          frame.payload["outcome"] === "ui_changed",
      ),
  ).toHaveLength(1);
  return tabID;
}

/** The researcher's own click download: it comes from a tab papio never
 * opened, so only the referrer host can correlate it. */
function clickDownload(id: number, filename = "1234567.pdf"): DownloadItemLike {
  return {
    id,
    tabId: 4242,
    referrer: `https://${PROVIDER_HOST}/stable/challenge`,
    url: `https://${PROVIDER_HOST}/stable/${filename}`,
    filename,
    state: "in_progress",
  };
}

async function suggestFilenameFor(
  h: Harness,
  item: DownloadItemLike,
): Promise<string[]> {
  const suggestions: string[] = [];
  await h.downloads.onDeterminingFilename.emit(item, (s) =>
    suggestions.push(s.filename),
  );
  return suggestions;
}

test("a ui_changed outcome retains a steering window for the researcher's own download", async () => {
  // The daemon mints manual_download from this exact outcome. Deleting the job
  // here used to switch Chrome steering off at the moment the action asking the
  // researcher to download was created, so their file landed outside
  // papio/<job_id>/ and was never adopted.
  const jobID = "job_manual_window_steer";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => false);
  await driveToManualDownloadOutcome(h, jobID);

  const retained = h.backend.store.activeJobs;
  expect(retained).toHaveLength(1);
  expect(retained[0]).toMatchObject({
    job_id: jobID,
    tab_id: -1,
    status: "awaiting_download",
  });
  expect(retained[0]?.provider_hosts).toContain(PROVIDER_HOST);
  expect(retained[0]?.adapter_id).toBe("provider");
  // No delegated authority and no retained offer URL: nothing autonomous can
  // pick this record up and drive it again.
  expect(retained[0]?.access_mode).toBeUndefined();

  expect(await suggestFilenameFor(h, clickDownload(950))).toEqual([
    `papio/${jobID}/1234567.pdf`,
  ]);

  h.downloads.items.set(950, {
    id: 950,
    filename: `/Users/x/Downloads/papio/${jobID}/1234567.pdf`,
    fileSize: 1_500_000,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onCreated.emit(clickDownload(950));
  await h.downloads.onChanged.emit({ id: 950, state: { current: "complete" } });
  const complete = h
    .frames()
    .find(
      (frame) => frame.type === "download_complete" && frame.job_id === jobID,
    );
  expect(complete?.payload["filename"]).toBe("1234567.pdf");
});

test("a retained steering window frees the governor slot and disarms its drive timeout", async () => {
  const jobID = "job_manual_window_governor";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => false);
  await driveToManualDownloadOutcome(h, jobID);
  const internals = h.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
    handoffDriveTimeouts: Map<string, unknown>;
    handoffDriveQueue: Array<{ jobID: string }>;
  };
  // The daemon's "busy" refusal semantics assume the extension never holds a
  // drive slot for a job it is not driving. A parked window drives nothing.
  expect(internals.handoffDrives.has(jobID)).toBe(false);
  expect(internals.handoffDrives.size).toBe(0);
  expect(internals.handoffDriveQueue).toHaveLength(0);
  expect(internals.handoffDriveTimeouts.has(jobID)).toBe(false);

  // 180_000 is HANDOFF_DRIVE_TIMEOUT_MS. The timer object still exists — the
  // fake clock cannot unschedule — so firing it must be inert: a parked window
  // has no drive to stall, and an auth_pending frame here would tell the
  // daemon the researcher is mid sign-in on work already handed back to them.
  const framesBefore = h.frames().length;
  const driveTimeout = h.timers.find((timer) => timer.ms === 180_000);
  expect(driveTimeout).toBeDefined();
  h.clock.now += 180_000;
  await driveTimeout!.fn();
  expect(h.frames().slice(framesBefore)).toHaveLength(0);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    job_id: jobID,
    tab_id: -1,
    status: "awaiting_download",
  });

  // The freed slot is real capacity, not bookkeeping: the next offer drives
  // immediately instead of queueing behind the parked window.
  await h.port.inbound(
    jobOffer(
      "job_manual_window_next",
      "https://resolver.example.edu/openurl?ctx=next",
    ),
  );
  expect(internals.handoffDrives.has("job_manual_window_next")).toBe(true);
  expect(internals.handoffDriveQueue).toHaveLength(0);
  expect(h.tabs.created).toHaveLength(2);
});

test("two retained windows on one provider host refuse to claim a download", async () => {
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => false);
  await driveToManualDownloadOutcome(h, "job_manual_window_one");

  const secondURL = "https://resolver.example.edu/openurl?ctx=second";
  await h.port.inbound(jobOffer("job_manual_window_two", secondURL));
  const secondTabID =
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_manual_window_two",
    )?.tab_id ?? -1;
  expect(secondTabID).toBeGreaterThanOrEqual(0);
  const articleURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.seed({ id: secondTabID, url: articleURL });
  await h.tabs.completeNavigation(secondTabID, articleURL);
  for (let retry = 0; retry < 2; retry += 1) {
    const timer = h.timers.at(-1);
    expect(timer).toBeDefined();
    h.clock.now += 2_500;
    await timer!.fn();
  }

  expect(h.backend.store.activeJobs.map((job) => job.job_id).sort()).toEqual([
    "job_manual_window_one",
    "job_manual_window_two",
  ]);
  // Retaining more jobs raises ambiguity, and a wrong guess files the
  // researcher's PDF under someone else's paper. No suggestion at all.
  expect(await suggestFilenameFor(h, clickDownload(951))).toEqual([]);
  await h.downloads.onCreated.emit(clickDownload(951));
  for (const job of h.backend.store.activeJobs)
    expect(job.download_initiated).toBeUndefined();
});

test("a worker restart rehydrates the retained window from the daemon's snapshot", async () => {
  const jobID = "job_manual_window_restart";
  const first = makeHarness();
  useUnknownProviderClassifier(first, () => false);
  await driveToManualDownloadOutcome(first, jobID);

  // The MV3 worker dies; only what survives the real managed-state migration
  // comes back. Worker-memory bookkeeping does not.
  const restarted = makeHarness(
    migrateManagedState(JSON.parse(JSON.stringify(first.backend.store))),
  );
  useUnknownProviderClassifier(restarted, () => false);
  await restarted.bridge.start();
  expect(restarted.backend.store.activeJobs.map((job) => job.job_id)).toEqual([
    jobID,
  ]);
  // The startup drive scan must not resurrect it: no tab, no governor slot.
  expect(restarted.tabs.created).toHaveLength(0);
  const internals = restarted.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
  };
  expect(internals.handoffDrives.size).toBe(0);

  await restarted.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1"],
    }),
  );
  await settleSnapshot(restarted, [manualDownloadItem(jobID)]);

  expect(restarted.backend.store.activeJobs.map((job) => job.job_id)).toEqual([
    jobID,
  ]);
  expect(await suggestFilenameFor(restarted, clickDownload(952))).toEqual([
    `papio/${jobID}/1234567.pdf`,
  ]);
});

test("a complete snapshot without the action retires the retained window", async () => {
  const jobID = "job_manual_window_retire";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => false);
  await driveToManualDownloadOutcome(h, jobID);
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1"],
    }),
  );

  // A partial page describes a subset of the open actions and is never
  // authority to retire anything.
  await settleSnapshot(h, [], { hasMore: true });
  expect(h.backend.store.activeJobs.map((job) => job.job_id)).toEqual([jobID]);
  await settleSnapshot(h, [], { unsupported: 1 });
  expect(h.backend.store.activeJobs.map((job) => job.job_id)).toEqual([jobID]);

  // A complete page that no longer reports the action is the daemon saying the
  // window is closed — adopted, dismissed, or otherwise resolved.
  await settleSnapshot(h, []);
  expect(h.backend.store.activeJobs).toHaveLength(0);
  expect(await suggestFilenameFor(h, clickDownload(953))).toEqual([]);
});

test("Firefox refuses to claim a retained window's download at all", async () => {
  // Firefox has no downloads.onDeterminingFilename, so a native download can
  // never be relocated into papio/<job_id>/. Acknowledging one would tell the
  // daemon to adopt a file it will never find, so correlate() refuses before
  // any host or adapter reasoning — and isFirefoxClickDownload keeps excluding
  // click adapters behind it. Retaining the job must not reach past either.
  for (const clickAdapter of [true, false]) {
    const jobID = `job_manual_window_firefox_${clickAdapter ? "click" : "plain"}`;
    const h = makeHarness(undefined, { firefox: true });
    h.deps.adapterSpecs.push(
      clickAdapter
        ? {
            ...PROVIDER_ADAPTER,
            download: {
              selector: "a#pdf",
              requireKind: "article",
              method: "click",
            },
          }
        : PROVIDER_ADAPTER,
    );
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) =>
      injection.func === planExecution
        ? plannerResult(injection, { kind: "unknown" })
        : [];
    await driveToManualDownloadOutcome(h, jobID);

    expect(h.backend.store.activeJobs[0]).toMatchObject({
      job_id: jobID,
      tab_id: -1,
      status: "awaiting_download",
    });
    expect(h.deps.downloads.onDeterminingFilename).toBeUndefined();
    h.downloads.items.set(954, {
      id: 954,
      filename: "/Users/x/Downloads/1234567.pdf",
      fileSize: 1_500_000,
      mime: "application/pdf",
      state: "complete",
    });
    await h.downloads.onCreated.emit(clickDownload(954));
    await h.downloads.onChanged.emit({
      id: 954,
      state: { current: "complete" },
    });
    expect(h.backend.store.activeJobs[0]?.download_initiated).toBeUndefined();
    expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(
      false,
    );
  }
});

test("a challenge parks only its provider and leaves another provider draining", async () => {
  const otherProvider = "link.springer.com";
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => true);
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(jobOffer("job_challenge_source"));
  const sourceTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const challengeURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.seed({ id: sourceTabID, url: challengeURL });
  await h.tabs.completeNavigation(sourceTabID, challengeURL);

  const parked = jobOffer("job_challenge_parked") as {
    payload: Record<string, unknown>;
  };
  parked.payload["requires_auth"] = true;
  const other = jobOfferForHosts("job_challenge_other", [otherProvider]) as {
    payload: Record<string, unknown>;
  };
  other.payload["requires_auth"] = true;
  await h.port.inbound(parked);
  await h.port.inbound(other);
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_challenge_parked",
    ),
  ).toMatchObject({
    tab_id: -1,
    status: "queued",
    handoffAckPending: true,
  });
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_challenge_other",
    ),
  ).toMatchObject({
    status: "accepted",
  });
  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "job_accept" &&
          frame.job_id === "job_challenge_parked",
      ),
  ).toHaveLength(0);
});

test("an expired provider lease reclaims its queued handoff without a new offer", async () => {
  const h = makeHarness();
  useUnknownProviderClassifier(h, () => true);
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(jobOffer("job_lease_source"));
  const sourceTabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const challengeURL = `https://${PROVIDER_HOST}/stable/challenge`;
  h.tabs.seed({ id: sourceTabID, url: challengeURL });
  await h.tabs.completeNavigation(sourceTabID, challengeURL);

  const queued = jobOffer("job_lease_reclaim") as {
    payload: Record<string, unknown>;
  };
  queued.payload["requires_auth"] = true;
  await h.port.inbound(queued);
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_lease_reclaim",
    ),
  ).toMatchObject({
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
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_lease_reclaim",
    ),
  ).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "job_accept" && frame.job_id === "job_lease_reclaim",
      ),
  ).toHaveLength(1);
  expect(h.backend.store.providerDrainLeases).toEqual({});

  const cooldownExpiry = h.timers.find((timer) => timer.ms === 600_000);
  expect(cooldownExpiry).toBeDefined();
  h.clock.now += 540_000;
  await cooldownExpiry!.fn();

  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_lease_reclaim",
    ),
  ).toMatchObject({
    status: "accepted",
  });
  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "job_accept" && frame.job_id === "job_lease_reclaim",
      ),
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
  await h.port.inbound(helloAck({ features: ["effect_permit_v1"] }));
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
  await expect(
    h.bridge.openHandoff("job_0001a_registry_manual"),
  ).resolves.toEqual({ ok: true, opened: true });
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

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
  const complete = h
    .frames()
    .find((frame) => frame.type === "download_complete");
  expect(complete?.payload["producer"]).toBeUndefined();
  expect(
    h
      .frames()
      .some(
        (frame) =>
          frame.type === "download_complete" &&
          frame.job_id === "job_0001a_registry_manual",
      ),
  ).toBe(true);
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
  h.tabs.seed({ id: 100, url: `https://${PROVIDER_HOST}/stable/a` });
  h.tabs.seed({ id: 101, url: `https://${PROVIDER_HOST}/stable/b` });
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

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(
    false,
  );
  expect(
    h.backend.store.activeJobs.every((job) => job.download_initiated !== true),
  ).toBe(true);
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
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: "normal" } }];
    if (injection.func === planExecution) {
      return plannerResult(injection, {
        kind: "article",
        adapter_id: adapter.id,
        adapter_version: adapter.version,
        evidence: [],
      });
    }
    if (injection.func === executePlannedPageEffect)
      return plannedEffectResult(injection);
    return [{ result: "https://download.example/paper.pdf" }];
  };
  let consentCalls = 0;
  let effectCalls = 0;
  let signalEffectStarted!: () => void;
  let releaseEffect!: () => void;
  const effectStarted = new Promise<void>((resolve) => {
    signalEffectStarted = resolve;
  });
  const effectGate = new Promise<void>((resolve) => {
    releaseEffect = () => resolve();
  });
  h.deps.settings.getTermsConsent = async () => {
    consentCalls += 1;
    return "accept";
  };
  const originalExecuteScript = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === executePlannedPageEffect) {
      effectCalls += 1;
      signalEffectStarted();
      if (effectCalls === 1) await effectGate;
    }
    return originalExecuteScript(injection);
  };

  await h.bridge.start();
  await h.port.inbound(jobOffer("job_terms_race"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const url = `https://${PROVIDER_HOST}/stable/article`;
  const first = h.tabs.completeNavigation(tabID, url);
  const second = h.tabs.completeNavigation(tabID, url);
  await effectStarted;
  // The first classification owns the provider/effect lease; the concurrent
  // observation is queued rather than entering a second terms/download effect.
  expect(effectCalls).toBe(1);
  expect(consentCalls).toBeGreaterThan(0);
  expect(h.downloads.started).toHaveLength(0);
  releaseEffect();
  await Promise.all([first, second]);

  const retry = h.timers.find((timer) => timer.ms === 2_500);
  expect(retry).toBeDefined();
  await retry?.fn();
  expect(effectCalls).toBe(1);
  expect(consentCalls).toBeGreaterThan(0);
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

  // Model the first effect settling without letting the ordinary release
  // drain consume the queued job before its durable 45-second fallback fires.
  const internals = h.bridge as unknown as {
    releaseHandoffDrive(jobID: string): void;
  };
  internals.releaseHandoffDrive("job_0001a_timer_active");
  await fallback?.fn();

  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
  ]);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_0001a_timer_queued",
    )?.status,
  ).toBe("accepted");
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
  });
  h.tabs.seed({ id: activeID, url: OPENURL });
  seedOfferURLs(h, { job_0001a_live_queued: queuedURL });

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
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });

  seedOfferURLs(h, { [jobID]: queuedURL });
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
  });

  seedOfferURLs(h, { [jobID]: queuedURL });
  await h.bridge.start();
  expect(h.tabs.created).toEqual([]);
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  expect(h.tabs.created).toEqual([{ url: queuedURL, active: false }]);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
});

test("a changed re-offer reuses the live job tab for the institutional fallback", async () => {
  const h = makeHarness();
  const oaURL = "https://oa.example.org/blocked-paper";
  const institutionalURL = "https://resolver.example.edu/openurl?fallback=1";
  const oaOffer = jobOffer("job_0001b_fallback") as {
    payload: Record<string, unknown>;
  };
  oaOffer.payload["openurl"] = oaURL;
  const institutionalOffer = jobOffer("job_0001b_fallback") as {
    payload: Record<string, unknown>;
  };
  institutionalOffer.payload["openurl"] = institutionalURL;

  await h.bridge.start();
  await h.port.inbound(oaOffer);
  await h.port.inbound(institutionalOffer);

  expect(h.tabs.created).toEqual([{ url: oaURL, active: true }]);
  expect(h.tabs.navigations).toEqual([{ tabID: 100, url: institutionalURL }]);
  expect(h.tabs.removed).toEqual([]);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
  expect(
    h
      .frames()
      .filter(
        (f) => f.type === "job_accept" && f.job_id === "job_0001b_fallback",
      ),
  ).toHaveLength(2);
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
  await h.tabs.userNavigate(tabID, idpURL);
  await h.tabs.completeNavigation(tabID, idpURL);

  const authPending = h.frames().filter((f) => f.type === "auth_pending");
  expect(authPending.length).toBe(1);
  expect(authPending[0]?.payload).toEqual({});

  // Not one outgoing frame nor any stored state string may carry the sentinel.
  for (const s of h.postedStrings()) expect(s.includes(secret)).toBe(false);
  expect(JSON.stringify(h.backend.store).includes(secret)).toBe(false);

  // Returning to the provider host yields auth_returned with timing only.
  h.clock.now += 4200;
  await h.tabs.userNavigate(tabID, `https://${PROVIDER_HOST}/stable/x`);
  const authReturned = h.frames().find((f) => f.type === "auth_returned");
  expect(authReturned?.payload["elapsed_ms"]).toBe(4200);
  expect(Object.keys(authReturned?.payload ?? {})).toEqual(["elapsed_ms"]);
});

test("session evidence sends the exact origin observed for that evidence", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["session_evidence_v1"] }));

  const sent = h.bridge.emitSessionEvidence(
    "warm_verified",
    "https://resolver.example.edu",
  );

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
  const offer = jobOffer("job_evidence_decoy", decoyOfferURL) as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;
  await h.port.inbound(offer); // populates latestResolverOrigin()'s decoy candidate
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: "https://granted-host.example.edu",
      pausedForReauth: false,
    }),
  } as unknown as KeepaliveManager); // decoy for the keepalive-snapshot fallback

  const sent = h.bridge.emitSessionEvidence("warm_verified", undefined);

  expect(sent).toBe(true);
  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({
    evidence: "warm_verified",
    at: new Date(h.clock.now).toISOString(),
  });
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
    getSnapshot: () => ({
      resolverOrigin: "https://granted-host.example.edu",
      pausedForReauth: false,
    }),
    // Commit B's onTabUpdated now tells the manager about every navigation
    // synchronously (see the "notified before ready resolves" test below);
    // this stub only cares about the resolver-origin fallback, so the
    // notification itself is a no-op here.
    noteResolverNavigation: () => {},
    noteResolverActivated: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_sso", ssoOpenURL));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  await h.tabs.completeNavigation(tabID, idpURL);
  await h.tabs.userNavigate(tabID, `https://${PROVIDER_HOST}/stable/x`);

  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({
    evidence: "auth_returned",
    at: new Date(h.clock.now).toISOString(),
  });
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
    helloAck({
      features: ["session_evidence_v1"],
      resolver_origins: ["https://resolver.example.edu"],
    }),
  );
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: "https://resolver.example.edu",
      pausedForReauth: false,
    }),
    noteResolverNavigation: () => {},
    noteResolverActivated: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_provider_hint", providerOpenURL));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  await h.tabs.completeNavigation(tabID, idpURL);
  await h.tabs.userNavigate(tabID, `https://${PROVIDER_HOST}/stable/x`);

  const frame = h.frames().find((f) => f.type === "session_evidence");
  expect(frame?.payload).toEqual({
    evidence: "auth_returned",
    at: new Date(h.clock.now).toISOString(),
  });
});

test("auth_returned origin_hint carries a configured resolver origin", async () => {
  // Contrast case for the test above: an offer resolving to an origin the
  // daemon actually advertised in hello_ack's resolver_origins still gets
  // the hint — the fix is the membership check, not a blanket omission.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["session_evidence_v1"],
      resolver_origins: ["https://resolver.example.edu"],
    }),
  );
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: "https://resolver.example.edu",
      pausedForReauth: false,
    }),
    noteResolverNavigation: () => {},
    noteResolverActivated: () => {},
  } as unknown as KeepaliveManager);
  await h.port.inbound(jobOffer("job_evidence_configured_hint")); // default OPENURL is resolver.example.edu
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  await h.tabs.completeNavigation(tabID, idpURL);
  await h.tabs.userNavigate(tabID, `https://${PROVIDER_HOST}/stable/x`);

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
  h.downloads.items.set(2, {
    id: 2,
    tabId: 999,
    filename: "/tmp/other.pdf",
    fileSize: 10,
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 2, state: { current: "complete" } });
  expect(h.frames().some((f) => f.type === "download_complete")).toBe(false);

  // Matching download on the job tab.
  await h.downloads.onCreated.emit({
    id: 1,
    tabId: tabID,
    state: "in_progress",
  });
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
    download: {
      selector: "button.download",
      requireKind: "article",
      method: "click",
    },
  };
  const injections: Parameters<BridgeDeps["scripting"]["executeScript"]>[0][] =
    [];
  h.deps.adapterSpecs.push(clickAdapter);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    injections.push(injection);
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: "normal" } }];
    if (injection.func === planExecution)
      return plannerResult(injection, { kind: "article" });
    return [];
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_click"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

  expect(injections).toHaveLength(2);
  expect(
    injections.filter((injection) => injection.func === planExecution),
  ).toHaveLength(1);
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

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(
    false,
  );
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
});

test("Firefox ignores manual downloads from non-click adapters without exact ownership", async () => {
  const h = makeHarness(undefined, { firefox: true });
  const hrefAdapter: AdapterSpec = {
    ...PROVIDER_ADAPTER,
    id: "firefox-href",
    download: {
      selector: "a.download",
      requireKind: "article",
      method: "href",
    },
  };
  h.deps.adapterSpecs.push(hrefAdapter);
  h.deps.permissions.contains = async () => true;
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_href"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

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

  expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(
    false,
  );
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
    injection.func === planExecution
      ? plannerResult(injection, { kind: "article" })
      : injection.func === executePlannedPageEffect
        ? [
            {
              result: {
                ok: true,
                url: `https://${PROVIDER_HOST}/download/article.pdf`,
              },
            },
          ]
        : [];
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004_firefox_api"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

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

  expect(
    h
      .frames()
      .some(
        (frame) =>
          frame.type === "download_complete" &&
          frame.job_id === "job_0004_firefox_api",
      ),
  ).toBe(true);
});

test("a cross-origin api download with a content-disposition rename steers into papio/<job>/ by ID", async () => {
  // Regression for the EBSCO shape (research.ebsco.com -> content.ebscohost.com):
  // the provider redirect changes origin before onDeterminingFilename, the item
  // carries no tabId, and the server renames the file (retrieve.pdf). Steering
  // must come from the creation-bound download ID, never tab/URL heuristics.
  const h = makeHarness();
  const crossOriginPDF =
    "https://content.aggregator.example/cds/retrieve?db=a9h";
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
    injection.func === planExecution
      ? plannerResult(injection, { kind: "article" })
      : injection.func === executePlannedPageEffect
        ? [{ result: { ok: true, url: crossOriginPDF } }]
        : [{ result: crossOriginPDF }];
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0004b_xorigin_api"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const articleURL = `https://${PROVIDER_HOST}/stable/article`;
  await h.tabs.completeNavigation(tabID, articleURL);

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
  await h.downloads.onCreated.emit({
    id: 901,
    url: crossOriginPDF,
    state: "in_progress",
  });
  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    {
      id: 901,
      url: crossOriginPDF,
      filename: "retrieve.pdf",
      state: "in_progress",
    },
    (s) => suggestions.push(s),
  );
  expect(suggestions).toEqual([
    {
      filename: "papio/job_0004b_xorigin_api/retrieve.pdf",
      conflictAction: "uniquify",
    },
  ]);

  h.downloads.items.set(901, {
    id: 901,
    filename: "/Users/x/Downloads/papio/job_0004b_xorigin_api/retrieve.pdf",
    fileSize: 2_100_000,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });

  const complete = h
    .frames()
    .find(
      (frame) =>
        frame.type === "download_complete" &&
        frame.job_id === "job_0004b_xorigin_api",
    );
  expect(complete?.payload["filename"]).toBe("retrieve.pdf");
});

test("a CDN PDF viewer is adopted for one uniquely driven accepted job", async () => {
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: "job_cdn_single",
        tab_id: 100,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: ["www.sciencedirect.com"],
        download_initiated: true,
        access_mode: "delegated",
      },
    ],
  });
  h.tabs.seed({ id: 100, url: OPENURL });
  await h.bridge.start();
  h.tabs.seed({ id: 101, url: "about:blank" });
  await h.tabs.completeNavigation(
    101,
    "https://pdf.sciencedirectassets.com/77/paper.pdf",
  );

  expect(h.downloads.started).toEqual([
    {
      url: "https://pdf.sciencedirectassets.com/77/paper.pdf",
      filename: "papio/job_cdn_single/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
});
test("an unrelated openerless PDF viewer is rejected without provider provenance", async () => {
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: "job_unrelated_viewer",
        tab_id: 100,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
        access_mode: "delegated",
        download_initiated: true,
      },
    ],
  });
  h.tabs.seed({ id: 100, url: OPENURL });
  await h.bridge.start();
  h.tabs.seed({ id: 101, url: "about:blank" });
  await h.tabs.completeNavigation(101, "https://unrelated.example/paper.pdf");
  expect(h.downloads.started).toEqual([]);
});

test("a CDN PDF viewer remains unadopted when two driven jobs are candidates", async () => {
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [0, 1].map((index) => ({
      job_id: `job_cdn_ambiguous_${index}`,
      tab_id: 100 + index,
      offered_at: 1_700_000_000_000,
      expires_at: 1_800_000_000_000,
      status: "accepted" as const,
      provider_hosts: ["www.sciencedirect.com"],
      download_initiated: true,
    })),
  });
  h.tabs.seed({ id: 100, url: OPENURL });
  h.tabs.seed({ id: 101, url: OPENURL });
  await h.bridge.start();
  h.tabs.seed({ id: 102, url: "about:blank" });
  await h.tabs.completeNavigation(
    102,
    "https://pdf.sciencedirectassets.com/77/paper.pdf",
  );

  expect(h.downloads.started).toEqual([]);
});

test("Firefox keeps the CDN viewer adoption path disabled for click adapters", async () => {
  const jobID = "job_cdn_firefox";
  const h = makeHarness(
    {
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 100,
          offered_at: 1_700_000_000_000,
          expires_at: 1_800_000_000_000,
          status: "accepted",
          provider_hosts: ["www.sciencedirect.com"],
          adapter_id: "firefox-click",
          download_initiated: true,
        },
      ],
    },
    { firefox: true },
  );
  h.deps.adapterSpecs.push({
    ...PROVIDER_ADAPTER,
    id: "firefox-click",
    download: {
      selector: "button.download",
      requireKind: "article",
      method: "click",
    },
  });
  h.tabs.seed({ id: 100, url: OPENURL });
  await h.bridge.start();
  h.tabs.seed({ id: 101, url: "about:blank" });
  await h.tabs.completeNavigation(
    101,
    "https://pdf.sciencedirectassets.com/77/paper.pdf",
  );

  expect(h.downloads.started).toEqual([]);
});

test("a PDF-viewer tab starts one download and leaves the adopted viewer open", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0010_pdf_viewer"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const viewerURL = `https://${PROVIDER_HOST}/reader/blocked-paper.pdf`;

  await h.tabs.completeNavigation(tabID, viewerURL);
  await h.tabs.completeNavigation(tabID, viewerURL);
  expect(h.downloads.started).toEqual([
    {
      url: viewerURL,
      filename: "papio/job_0010_pdf_viewer/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);

  await h.downloads.onCreated.emit({
    id: 901,
    tabId: tabID,
    state: "in_progress",
  });
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

  expect(
    h
      .frames()
      .some(
        (f) =>
          f.type === "download_complete" && f.job_id === "job_0010_pdf_viewer",
      ),
  ).toBe(true);
  expect(h.tabs.removed).toEqual([]);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});

test("Chrome's built-in PDF viewer downloads the memory-only offered URL", async () => {
  const h = makeHarness();
  const offeredURL = "https://oa.example.org/opaque-download";
  const offer = jobOffer("job_0010b_chrome_viewer") as {
    payload: Record<string, unknown>;
  };
  offer.payload["openurl"] = offeredURL;
  await h.bridge.start();
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const chromeViewerURL =
    "chrome-extension://mhjfbmdgcfjbbpaeojofohoefgiehjai/index.html";

  await h.tabs.completeNavigation(tabID, chromeViewerURL);

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
      viewerRoutes: [{ pathPrefix: "/doi/epdf/" }],
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate:
        "https://onlinelibrary.wiley.com/doi/pdfdirect/{1}?download=true",
    },
  });
  const viewerURL =
    "https://onlinelibrary.wiley.com/doi/epdf/10.1111/rego.12568";
  await h.bridge.start();
  await h.port.inbound(
    jobOfferForHosts(
      "job_wiley_epdf_viewer",
      ["onlinelibrary.wiley.com"],
      viewerURL,
    ),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, viewerURL);
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

  await h.downloads.onCreated.emit({
    id: 77,
    tabId: tabID,
    state: "in_progress",
  });
  const pdfURL = `https://${PROVIDER_HOST}/download/paper.pdf`;
  await h.tabs.completeNavigation(tabID, pdfURL);

  expect(h.downloads.started).toEqual([]);
});
test("an MDPI PDF route without a .pdf suffix is adopted", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const pdfURL = "https://mdpi.com/2227-7102/9/3/181/pdf?version=1563177761";
  await h.port.inbound(
    jobOfferForHosts("job_mdpi_pdf_route", ["mdpi.com"], pdfURL),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(tabID, pdfURL);

  expect(h.downloads.started).toEqual([
    {
      url: pdfURL,
      filename: "papio/job_mdpi_pdf_route/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
});

test("a correlated download is steered into papio/<job_id>/; unrelated untouched", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0007_steer"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    {
      id: 5,
      tabId: tabID,
      filename: "Trust_in_Automation.pdf",
      state: "in_progress",
    },
    (s) => suggestions.push(s),
  );
  expect(suggestions).toEqual([
    {
      filename: "papio/job_0007_steer/Trust_in_Automation.pdf",
      conflictAction: "uniquify",
    },
  ]);

  // Unrelated download (different tab, unknown host): never steered.
  await h.downloads.onDeterminingFilename.emit(
    {
      id: 6,
      tabId: 999,
      url: "https://example.org/x.pdf",
      filename: "x.pdf",
      state: "in_progress",
    },
    (s) => suggestions.push(s),
  );
  expect(suggestions.length).toBe(1);
});

/** Live Silverchair watermark delivery: bearer-grade `token` query (~850 chars). */
const SILVERCHAIR_SIGNED_HOST = "watermark02.silverchair.com";

async function helloPdfGrabCapable(h: Harness): Promise<void> {
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
}

function silverchairSignedPDFURL(
  pathname = "/paper.pdf",
  tokenChar = "a",
): string {
  return `https://${SILVERCHAIR_SIGNED_HOST}${pathname}?token=${tokenChar.repeat(852)}`;
}

async function armSignedViewerGrab(
  h: Harness,
  opts?: {
    tabID?: number;
    pathname?: string;
    grabID?: string;
    steeringPath?: string;
    tokenChar?: string;
  },
): Promise<{
  tabID: number;
  pdfURL: string;
  route: string;
  grabID: string;
  steeringPath: string;
}> {
  const tabID = opts?.tabID ?? 81;
  const pathname = opts?.pathname ?? "/paper.pdf";
  const grabID = opts?.grabID ?? "grab-signed-comp";
  const steeringPath = opts?.steeringPath ?? "papio/grabs/signedcomp/";
  const pdfURL = silverchairSignedPDFURL(pathname, opts?.tokenChar ?? "a");
  const route = `https://${SILVERCHAIR_SIGNED_HOST}${pathname}`;
  h.tabs.seed({ id: tabID, url: pdfURL });
  const reply = h.bridge.requestPdfGrab({ tab_id: tabID, url: pdfURL });
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: grabID,
      steering_path: steeringPath,
    }),
  );
  await expect(reply).resolves.toMatchObject({
    ok: true,
    awaiting_viewer: true,
  });
  return { tabID, pdfURL, route, grabID, steeringPath };
}

describe("single-use signed delivery URL composition", () => {
  test("an armed grab starts no download and replies awaiting_viewer", async () => {
    // Removing `requiresNativeViewerDownload` → `downloads.download` on Send PDF
    // would break this: a doomed refetch of the one-time link.
    const h = makeHarness();
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      grabID: "grab-no-fetch",
      steeringPath: "papio/grabs/nofetch01/",
    });
    expect(h.downloads.started).toEqual([]);
    const correlation = h.pdfGrabCorrelations.current[armed.grabID];
    expect(correlation).toMatchObject({
      state: "awaiting_viewer",
      route: armed.route,
    });
    // No download id at all — papio started nothing, so there is nothing to
    // carry one. An id here would mean the doomed refetch had run.
    expect(correlation).not.toHaveProperty("downloadID");
  });

  test("the researcher's own download is steered into the armed grab on the same route", async () => {
    // Removing `onDeterminingFilename` grab steering (pendingGrabFor) would
    // break this: the viewer click would not land in the grab directory.
    const h = makeHarness();
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      grabID: "grab-same-route",
      steeringPath: "papio/grabs/sameroute/",
    });
    const suggestions: { filename: string; conflictAction: string }[] = [];
    await h.downloads.onDeterminingFilename.emit(
      {
        id: 881,
        tabId: armed.tabID,
        url: armed.pdfURL,
        filename: "paper.pdf",
        state: "in_progress",
      },
      (s) => suggestions.push(s),
    );
    expect(suggestions).toEqual([
      {
        filename: `${armed.steeringPath}paper.pdf`,
        conflictAction: "uniquify",
      },
    ]);
  });

  test("a download on a different route is not claimed by the armed grab", async () => {
    // Removing route-keyed `pendingGrabDownloadURLs` matching would break this:
    // every signed URL on the host would steer into one grab.
    const h = makeHarness();
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      pathname: "/paper-a.pdf",
      grabID: "grab-route-a",
      steeringPath: "papio/grabs/routea001/",
      tokenChar: "a",
    });
    const otherURL = silverchairSignedPDFURL("/paper-b.pdf", "b");
    expect(
      await suggestFilenameFor(h, {
        id: 882,
        tabId: armed.tabID,
        url: otherURL,
        filename: "paper-b.pdf",
        state: "in_progress",
      }),
    ).toEqual([]);
  });

  test("an awaiting-viewer grab survives worker death and remains steerable after hydration", async () => {
    // Removing hydration `reconcilePdfGrabCorrelations` re-registration would
    // break this: MV3 recycles the worker before the researcher's click.
    const first = makeHarness();
    await helloPdfGrabCapable(first);
    const armed = await armSignedViewerGrab(first, {
      grabID: "grab-sw-hydrate",
      steeringPath: "papio/grabs/swhydrate/",
    });
    const persisted = JSON.parse(
      JSON.stringify(first.pdfGrabCorrelations.current),
    ) as Record<string, PdfGrabCorrelation>;

    const restarted = makeHarness();
    restarted.pdfGrabCorrelations.current = persisted;
    await helloPdfGrabCapable(restarted);
    expect(restarted.pdfGrabCorrelations.current[armed.grabID]).toMatchObject({
      state: "awaiting_viewer",
      route: armed.route,
    });
    expect(
      await suggestFilenameFor(restarted, {
        id: 883,
        tabId: armed.tabID,
        url: armed.pdfURL,
        filename: "paper.pdf",
        state: "in_progress",
      }),
    ).toEqual([`${armed.steeringPath}paper.pdf`]);
  });

  test("Firefox declines a signed link without promising to file the download", async () => {
    // Removing the Firefox `requiresNativeViewerDownload` refusal in
    // `startPDFDelivery` would break this: the old copy promised filing.
    const h = makeHarness(undefined, { firefox: true });
    await helloPdfGrabCapable(h);
    const pdfURL = silverchairSignedPDFURL();
    h.tabs.seed({ id: 90, url: pdfURL });
    const reply = await h.bridge.startPDFDelivery({
      tab_id: 90,
      url: pdfURL,
    });
    expect(reply.ok).toBe(false);
    const message = reply.ok ? "" : reply.error.message;
    expect(message).toMatch(/Chrome/);
    expect(message).not.toMatch(/papio will file/i);
    expect(h.downloads.started).toEqual([]);
    expect(h.frames().some((f) => f.type === "pdf_grab_request")).toBe(false);
  });
});

describe("click-adapter job vs armed grab ambiguity", () => {
  function downloadOwnership(h: Harness): {
    jobTrack: (jobID: string) => Set<number> | undefined;
    grabTrack: (grabID: string) => Set<number> | undefined;
  } {
    const internals = h.bridge as unknown as {
      downloads: Map<string, { ids: Set<number> }>;
      grabDownloads: Map<string, { ids: Set<number> }>;
    };
    return {
      jobTrack: (jobID) => internals.downloads.get(jobID)?.ids,
      grabTrack: (grabID) => internals.grabDownloads.get(grabID)?.ids,
    };
  }

  async function awaitBridgeReady(h: Harness): Promise<void> {
    await (h.bridge as unknown as { ready: Promise<void> }).ready;
  }

  test("a live same-tab delegated click job and armed grab both match: park, steer nothing", async () => {
    const tabID = 100;
    const jobID = "job_click_grab_ambig";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: tabID,
          offered_at: 1_700_000_000_000,
          expires_at: 1_800_000_000_000,
          status: "accepted",
          access_mode: "delegated",
          provider_hosts: [SILVERCHAIR_SIGNED_HOST],
          download_initiated: true,
          adapter_id: "provider",
        },
      ],
    });
    h.tabs.seed({ id: tabID, url: OPENURL });
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      tabID,
      grabID: "grab-ambig",
      steeringPath: "papio/grabs/grambig01/",
    });
    const item: DownloadItemLike = {
      id: 991,
      tabId: tabID,
      url: armed.pdfURL,
      filename: "paper.pdf",
      state: "in_progress",
    };

    await awaitBridgeReady(h);
    expect(await suggestFilenameFor(h, item)).toEqual([]);
    await h.downloads.onCreated.emit(item);

    const { jobTrack, grabTrack } = downloadOwnership(h);
    expect(jobTrack(jobID)?.has(item.id)).not.toBe(true);
    expect(grabTrack(armed.grabID)?.has(item.id)).not.toBe(true);

    h.downloads.items.set(item.id, {
      ...item,
      filename: "/Users/x/Downloads/paper.pdf",
      fileSize: 1_500_000,
      mime: "application/pdf",
      state: "complete",
    });
    await h.downloads.onChanged.emit({
      id: item.id,
      state: { current: "complete" },
    });
    expect(h.frames().some((frame) => frame.type === "download_complete")).toBe(
      false,
    );
    expect(
      h.runtimeMessages.some(
        (message) =>
          (message as { type?: string; state?: string }).type ===
            "papio.pageBulk.grabState" &&
          (message as { state?: string }).state === "identifying",
      ),
    ).toBe(false);
  });

  test("a live grab still beats a stale same-tab job correlation without download_initiated", async () => {
    const tabID = 101;
    const jobID = "job_stale_tab_grab";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: tabID,
          offered_at: 1_700_000_000_000,
          expires_at: 1_800_000_000_000,
          status: "accepted",
          access_mode: "delegated",
          provider_hosts: [SILVERCHAIR_SIGNED_HOST],
          download_initiated: false,
        },
      ],
    });
    h.tabs.seed({ id: tabID, url: OPENURL });
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      tabID,
      grabID: "grab-stale-tab",
      steeringPath: "papio/grabs/staletab/",
    });
    const item: DownloadItemLike = {
      id: 992,
      tabId: tabID,
      url: armed.pdfURL,
      filename: "paper.pdf",
      state: "in_progress",
    };

    await awaitBridgeReady(h);
    expect(await suggestFilenameFor(h, item)).toEqual([
      `${armed.steeringPath}paper.pdf`,
    ]);
    await h.downloads.onCreated.emit(item);

    const { jobTrack, grabTrack } = downloadOwnership(h);
    expect(grabTrack(armed.grabID)?.has(item.id)).toBe(true);
    expect(jobTrack(jobID)?.has(item.id)).not.toBe(true);
    expect(
      h.runtimeMessages.some(
        (message) =>
          (message as { type?: string; state?: string }).type ===
            "papio.pageBulk.grabState" &&
          (message as { state?: string }).state === "failed",
      ),
    ).toBe(false);
  });

  test("the ambiguity conflict message names both claimants and does not claim the file was sent", async () => {
    const tabID = 102;
    const jobID = "job_conflict_copy";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: tabID,
          offered_at: 1_700_000_000_000,
          expires_at: 1_800_000_000_000,
          status: "accepted",
          access_mode: "delegated",
          provider_hosts: [SILVERCHAIR_SIGNED_HOST],
          download_initiated: true,
        },
      ],
    });
    h.tabs.seed({ id: tabID, url: OPENURL });
    await helloPdfGrabCapable(h);
    const armed = await armSignedViewerGrab(h, {
      tabID,
      grabID: "grab-conflict-copy",
      steeringPath: "papio/grabs/conflictcopy/",
    });
    await awaitBridgeReady(h);
    await h.downloads.onCreated.emit({
      id: 993,
      tabId: tabID,
      url: armed.pdfURL,
      filename: "paper.pdf",
      state: "in_progress",
    });

    const conflict = h.runtimeMessages.find(
      (message) =>
        (message as { type?: string; grab_id?: string }).type ===
          "papio.pageBulk.grabState" &&
        (message as { grab_id?: string }).grab_id === armed.grabID,
    ) as { state?: string; detail?: string } | undefined;
    expect(conflict).toMatchObject({
      state: "failed",
    });
    expect(conflict?.detail).toMatch(/handoff acquisition/i);
    expect(conflict?.detail).toMatch(/Send PDF grab/i);
    expect(conflict?.detail).toMatch(/finish or cancel the handoff job/i);
    expect(conflict?.detail).toMatch(/cancel the grab/i);
    expect(conflict?.detail?.toLowerCase()).not.toMatch(
      /sent to papio|saved to papio|filed it|pdf sent/,
    );
    expect(conflict?.state).not.toBe("grabbed");
    expect(conflict?.state).not.toBe("identifying");
  });
});

test("a download interrupted before its id is known settles the grab instead of stalling", async () => {
  // Chrome interrupts an instantly-failing download before `download()`
  // resolves, so the onChanged delta carrying that failure arrives while the id
  // is still untracked and `trackedGrabFor` drops it. The grab then held the
  // single effect lane as `unknown_completion` until the next worker start.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const pdfURL = "https://provider.example.edu/dead-link.pdf";
  h.tabs.seed({ id: 61, url: pdfURL });
  h.downloads.afterCreate = (downloadID) => {
    const item = h.downloads.items.get(downloadID);
    if (item !== undefined) {
      h.downloads.items.set(downloadID, { ...item, state: "interrupted" });
    }
  };

  const reply = h.bridge.requestPdfGrab({ tab_id: 61, url: pdfURL });
  const frame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: frame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-dead-link-01",
      steering_path: "papio/grabs/deadlink/",
    }),
  );

  // The daemon is told, so it can settle the permit on definite evidence
  // rather than being left to guess whether the fetch happened — and it is told
  // under the *allocation's* request id. Cancellation is fenced on the id the
  // daemon stored as `effect_request_id`, so a freshly minted correlation id is
  // answered `conflict` and the capture keeps occupying the effect lane.
  const abandon = await h.port.waitForFrame("pdf_grab_abandon_request");
  expect(abandon.payload["grab_id"]).toBe("grab-dead-link-01");
  expect(abandon.payload["request_id"]).toBe(frame.payload["request_id"]);
  // Answer it, so the reply proves the settled cancellation rather than the
  // two-second timeout the request falls back to when nothing responds.
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandon.payload["request_id"] as string,
      grab_id: "grab-dead-link-01",
      state: "abandoned",
      outcome: "abandoned",
    }),
  );
  await expect(reply).resolves.toMatchObject({ ok: false });
  // The correlation is gone, so a later Send PDF starts clean instead of
  // finding a stale record for a download that failed — and no signing token is
  // left behind in storage.
  expect(h.pdfGrabCorrelations.current["grab-dead-link-01"]).toBeUndefined();
});
test("closing the tab before auth cancels; after auth (awaiting_download) does not", async () => {
  // Before auth return: tab close is a genuine user cancel.
  const pre = makeHarness();
  await pre.bridge.start();
  await pre.port.inbound(jobOffer("job_0008_precancel"));
  const preTab = pre.backend.store.activeJobs[0]?.tab_id ?? -1;
  await pre.tabs.userClose(preTab);
  expect(pre.frames().some((f) => f.type === "provider_outcome")).toBe(true);
  expect(pre.backend.store.activeJobs.length).toBe(0);

  // After auth return: job is awaiting_download; a closed tab must NOT cancel
  // (the download may be saved for daemon-side adoption).
  const post = makeHarness();
  await post.bridge.start();
  await post.port.inbound(jobOffer("job_0009_postauth"));
  const postTab = post.backend.store.activeJobs[0]?.tab_id ?? -1;
  post.tabs.seed({ id: postTab, url: `https://${PROVIDER_HOST}/x` });
  await post.tabs.userNavigate(postTab, "https://idp.example.edu/sso");
  await post.tabs.userNavigate(postTab, `https://${PROVIDER_HOST}/y`);
  expect(post.backend.store.activeJobs[0]?.status).toBe("awaiting_download");
  await post.tabs.userClose(postTab);
  expect(post.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(post.backend.store.activeJobs.length).toBe(0);
});

test("a malformed inbound frame fails closed by disconnecting", async () => {
  const h = makeHarness();
  await h.bridge.start();
  expect(h.port.disconnected).toBe(false);
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "not_a_type",
    msg_id: "x",
    seq: 0,
    payload: {},
  });
  expect(h.port.disconnected).toBe(true);
});


test("an existing armed grab is resumed for the tab's current URL, never reported as sending", async () => {
  // Observed live on watermark02.silverchair.com: a second Send PDF for a tab
  // whose grab was already armed answered `existing`, and the popup said "PDF
  // sent to papio" for a grab waiting on a download nobody was performing.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const signed = `https://watermark02.silverchair.com/paper.pdf?token=${"z".repeat(120)}`;
  h.tabs.seed({ id: 71, url: signed });

  // First click arms the grab and asks for the viewer's own download.
  const armed = h.bridge.requestPdfGrab({ tab_id: 71, url: signed });
  const first = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: first.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-existing-01",
      steering_path: "papio/grabs/existing01/",
    }),
  );
  await expect(armed).resolves.toMatchObject({ ok: true, awaiting_viewer: true });

  // Second click: the daemon answers `existing` with no steering of its own.
  const seen = h.port.posted.length;
  const again = h.bridge.requestPdfGrab({ tab_id: 71, url: signed });
  const second = await h.port.waitForFrame("pdf_grab_request", seen);
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: second.payload["request_id"] as string,
      outcome: "existing",
      grab_id: "grab-existing-01",
    }),
  );
  const statusFrame = await h.port.waitForFrame("pdf_grab_status_request");
  await h.port.inbound(
    nativeResult("pdf_grab_status_result", {
      request_id: statusFrame.payload["request_id"] as string,
      grab_id: "grab-existing-01",
      state: "awaiting_file",
    }),
  );
  await expect(again).resolves.toMatchObject({ ok: true, awaiting_viewer: true });

  // Steering survives, so the researcher's own Download click still lands in
  // the grab directory rather than being adopted as an unrelated file.
  const suggestions: { filename: string }[] = [];
  h.downloads.onDeterminingFilename.emit(
    { id: 900, url: signed, filename: "paper.pdf", state: "in_progress" },
    (s) => suggestions.push(s),
  );
  expect(suggestions[0]?.filename).toBe("papio/grabs/existing01/paper.pdf");
});

test("an existing grab this browser cannot steer is cleared, and the retry allocates a fresh one", async () => {
  // Session-scoped correlations do not survive an extension reload, so after one
  // the daemon still answers `existing` for a grab nothing can steer into. It
  // would otherwise hold the tab for the daemon's six-hour stale bound.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const signed = `https://watermark02.silverchair.com/orphan.pdf?token=${"q".repeat(120)}`;
  h.tabs.seed({ id: 72, url: signed });
  const reply = h.bridge.requestPdfGrab({ tab_id: 72, url: signed });
  const frame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: frame.payload["request_id"] as string,
      outcome: "existing",
      grab_id: "grab-orphan-01",
    }),
  );
  const statusFrame = await h.port.waitForFrame("pdf_grab_status_request");
  await h.port.inbound(
    nativeResult("pdf_grab_status_result", {
      request_id: statusFrame.payload["request_id"] as string,
      grab_id: "grab-orphan-01",
      state: "awaiting_file",
    }),
  );
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  expect(abandonFrame.payload["grab_id"]).toBe("grab-orphan-01");
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandonFrame.payload["request_id"] as string,
      grab_id: "grab-orphan-01",
      state: "abandoned",
      outcome: "abandoned",
    }),
  );

  // One retry, and only one: the fresh allocation is armed for the viewer.
  const seen = h.port.posted.length;
  const retryFrame = await h.port.waitForFrame("pdf_grab_request", seen - 1);
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: retryFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-orphan-02",
      steering_path: "papio/grabs/orphan02/",
    }),
  );
  await expect(reply).resolves.toMatchObject({
    ok: true,
    grab_id: "grab-orphan-02",
    awaiting_viewer: true,
  });
});

test("an existing grab the daemon refuses to cancel is reported, not retried", async () => {
  // The daemon answers `conflict` inside a successful reply when the capture
  // still occupies its permit — held, or lost with completion unknown — which is
  // the deliberate rule that only the request identity which allocated an
  // occupying capture may release it. Treating that as clearance and retrying
  // just repeats the refusal, and reporting it as sent is the lie this whole
  // sequence exists to remove.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const signed = `https://watermark02.silverchair.com/held.pdf?token=${"h".repeat(120)}`;
  h.tabs.seed({ id: 73, url: signed });
  const reply = h.bridge.requestPdfGrab({ tab_id: 73, url: signed });
  const frame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: frame.payload["request_id"] as string,
      outcome: "existing",
      grab_id: "grab-held-01",
    }),
  );
  const statusFrame = await h.port.waitForFrame("pdf_grab_status_request");
  await h.port.inbound(
    nativeResult("pdf_grab_status_result", {
      request_id: statusFrame.payload["request_id"] as string,
      grab_id: "grab-held-01",
      state: "awaiting_file",
    }),
  );
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  const postedBeforeRefusal = h.port.posted.length;
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandonFrame.payload["request_id"] as string,
      grab_id: "grab-held-01",
      state: "awaiting_file",
      outcome: "conflict",
      detail: "this download has already finished, or a different browser started it",
    }),
  );
  const settled = await reply;
  expect(settled.ok).toBe(false);
  // No second allocation: a refused cancellation is final for this attempt.
  const laterGrabRequests = h.port.posted
    .slice(postedBeforeRefusal)
    .map(parseBrowserMessage)
    .filter((f) => f.type === "pdf_grab_request");
  expect(laterGrabRequests).toEqual([]);
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
  h.tabs.seed({ id: 100, url: `https://${PROVIDER_HOST}/x` });
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
  h.tabs.seed({ id: 100, url: `https://${PROVIDER_HOST}/x` });
  await h.bridge.start();

  await h.port.inbound(helloRequiredError());

  expect(h.port.disconnected).toBe(true);
  expect(h.ports.length).toBe(2);
  expect(h.timers.filter((timer) => timer.ms === 1_000)).toHaveLength(0);
  expect(h.frames().filter((f) => f.type === "hello").length).toBe(2);

  // The fresh daemon re-offers the durable job; the tracked live tab is reused.
  await h.ports[1]?.inbound(jobOffer("job_0007_session"));
  expect(h.tabs.created.length).toBe(0);
  expect(
    h
      .frames()
      .filter((f) => f.type === "job_accept" && f.job_id === "job_0007_session")
      .length,
  ).toBe(1);
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
  await bad.port.inbound({
    protocol: "papio-browser/1",
    type: "not_a_type",
    msg_id: "x",
    seq: 0,
    payload: {},
  });
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

  expect(
    h.timers.filter((timer) => timer.ms !== 12_000 && timer.ms !== 90_000),
  ).toHaveLength(8);
  expect(h.action.texts.at(-1)).toBe("!");
  expect(h.action.backgroundColors.at(-1)).toBe("#777777");
});

test("concurrent handoff triggers cannot double-drain one provider", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_provider_active"));
  await h.port.inbound(jobOffer("job_provider_first"));
  await h.port.inbound(jobOffer("job_provider_second"));
  // The single governor slot is released before the racing inbox opens; both
  // requests must still target their exact queued jobs, not double-drain.
  await h.bridge.requestCancel("job_provider_active");
  const [first, second] = await Promise.all([
    h.bridge.openHandoff("job_provider_first"),
    h.bridge.openHandoff("job_provider_second"),
  ]);

  expect(first).toMatchObject({ ok: true, opened: true });
  expect(second).toMatchObject({ ok: false });
  expect(h.tabs.created).toHaveLength(2);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_provider_first",
    ),
  ).toMatchObject({
    tab_id: expect.any(Number),
    status: "accepted",
  });
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_provider_second",
    ),
  ).toMatchObject({
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
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(3);
  const fallbacks = h.timers.filter((timer) => timer.ms === 45_000);
  expect(fallbacks).toHaveLength(3);
  await h.bridge.requestCancel("job_conc_active");
  // Racing fallback callbacks claim one provider lease. The remaining jobs
  // stay queued instead of opening concurrent handoff tabs for the same host.
  await Promise.all(fallbacks.map((timer) => timer.fn()));

  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(2);
  expect(
    h.backend.store.activeJobs.filter((job) => job.tab_id >= 0),
  ).toHaveLength(1);
  // One fallback owns the sole provider lease and is surfaced in the
  // background; the remaining callbacks retain their queued jobs.
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
  const tabs = new ChromeTabsFake();
  tabs.nextId = 100;
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const backend = new OrderBackend();
  // The seeded handoff needs a live tab so startup reconciliation keeps it.
  tabs.seed({ id: 100, url: `https://${PROVIDER_HOST}/seed` });
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
  await Promise.all([
    port.inbound(jobOffer("job_write_a")),
    port.inbound(jobOffer("job_write_b")),
  ]);

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

  expect(h.windows?.created).toEqual([
    { url: OPENURL, focused: false, state: "minimized" },
  ]);
  // The tab was created by windows.create inside the work window, never
  // focused, and the job tracks it like any broker tab.
  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: false, windowId: 500 },
  ]);
  expect(h.backend.store.workWindowID).toBe(500);
  expect(h.backend.store.activeJobs[0]?.tab_id).toBe(100);
  const accept = h.frames().find((f) => f.type === "job_accept");
  expect(accept?.job_id).toBe("job_ww_first");
});

test("requiresVisible is opt-in and fails closed for unmatched adapters", () => {
  const cases: { spec: AdapterSpec | undefined; wantsVisible: boolean }[] = [
    { spec: undefined, wantsVisible: false },
    { spec: PROVIDER_ADAPTER, wantsVisible: false },
    {
      spec: { ...PROVIDER_ADAPTER, requiresVisible: true },
      wantsVisible: true,
    },
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
      props: {
        focused?: boolean;
        state?: "normal" | "minimized";
        drawAttention?: boolean;
      };
    }[];
  }[] = [
    {
      adapterSpecs: [{ ...PROVIDER_ADAPTER, requiresVisible: true }],
      expectedState: "normal",
      expectedUpdates: [
        { windowID: 500, props: { focused: false, state: "normal" } },
      ],
    },
    {
      adapterSpecs: [PROVIDER_ADAPTER],
      expectedState: "minimized",
      expectedUpdates: [],
    },
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

    await h.tabs.completeNavigation(tabID, url);

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

  expect(h.windows?.created).toEqual([
    { url: providerURL, focused: false, state: "normal" },
  ]);
  expect(h.windows?.updated).toEqual([]);
});

test("work window is reused across offers and recreated after the user closes it", async () => {
  // Warm auth evidence makes the first offer immediately occupy the single
  // governor slot; later offers remain queued until it releases.
  const h = makeHarness(
    {
      ...emptyStore(),
      authEvidenceByOrigin: {
        "https://resolver.example.edu": 1_700_000_000_000,
      },
    },
    { windows: true },
  );
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_a"));
  await h.port.inbound(jobOffer("job_ww_b"));

  expect(h.windows?.created.length).toBe(1);
  expect(h.tabs.created.map((t) => t.windowId)).toEqual([500]);

  // The governor queues further drives while the single warm slot is occupied.
  h.windows?.close(500);
  await h.port.inbound(jobOffer("job_ww_c"));
  expect(h.windows?.created.length).toBe(1);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === "job_ww_c"),
  ).toMatchObject({
    status: "accepted",
    tab_id: -1,
  });

  // Releasing one live drive lets the queued offer recreate the user-closed
  // work window instead of trying to reuse its stale id.
  await h.bridge.requestCancel("job_ww_a");
  expect(h.windows?.created.length).toBe(2);
  expect(h.backend.store.workWindowID).toBe(501);
});

test("work window remains open once the last handoff releases its tab", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_idle"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.backend.store.workWindowID).toBe(500);
  // The user closes the handoff tab before download: the job cancels, but
  // papio leaves the work window and its remaining browser state open.
  await h.tabs.userClose(tabID);
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.windows?.removed).toEqual([]);
  expect(h.backend.store.workWindowID).toBe(500);
});

test("a keepalive-pinned tab keeps the work window alive when handoffs drain", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_keepalive"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  // Keepalive pins its resolver session tab in the shared work window; draining
  // the last handoff must not evict it.
  h.tabs.seed({
    id: 9001,
    url: "https://resolver.example.edu/keepalive",
    windowId: 500,
    pinned: true,
  });
  await h.tabs.userClose(tabID);
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.windows?.removed).toEqual([]);
  expect(h.backend.store.workWindowID).toBe(500);
});

test("tab-group mode opens the handoff in a collapsed papio group in the current window", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_first"));

  // The tab opens in the user's current window (no windowId), not a new window.
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);
  expect(h.tabs.grouped).toEqual([{ tabIds: [100] }]);
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(true);
  expect(h.tabGroups?.updated).toEqual([
    {
      groupID: groupID!,
      props: { title: "papio", collapsed: true, color: "orange" },
    },
  ]);
});

test("tab-group handoff works on Firefox 139+ (no onDeterminingFilename)", async () => {
  // Firefox has no downloads.onDeterminingFilename, but Firefox 139+ ships the
  // tabGroups API. Tab-group handoff is dep-driven, never gated on Chrome.
  const h = makeHarness(undefined, {
    firefox: true,
    tabGroups: true,
    handoffSurface: "tab-group",
  });
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
    {
      ...emptyStore(),
      authEvidenceByOrigin: {
        "https://resolver.example.edu": 1_700_000_000_000,
      },
    },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_a"));
  const groupID = h.backend.store.handoffGroupID!;
  await h.bridge.requestCancel("job_tg_a");
  await h.port.inbound(jobOffer("job_tg_b"));

  // The second handoff opens after the first releases and joins the same
  // papio group rather than creating another group.
  expect(h.tabs.grouped).toEqual([
    { tabIds: [100] },
    { tabIds: [101], groupId: groupID },
  ]);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabGroups?.live.size).toBe(1);
});

test("concurrent tab-group folds create exactly one papio group", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  await h.bridge.start();
  const first = await h.tabs.create({
    url: `${OPENURL}&keepalive=first`,
    active: false,
  });
  const second = await h.tabs.create({
    url: `${OPENURL}&keepalive=second`,
    active: false,
  });

  await Promise.all([
    h.bridge.foldKeepaliveTab(first.id!),
    h.bridge.foldKeepaliveTab(second.id!),
  ]);

  expect(h.tabGroups?.live.size).toBe(1);
  expect(h.tabs.grouped).toEqual([
    { tabIds: [100] },
    { tabIds: [101], groupId: 700 },
  ]);
});

test("tab-group folds do not reuse a stored group from another window", async () => {
  const h = makeHarness(
    { ...emptyStore(), handoffGroupID: 700 },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  const existing = await h.tabs.create({
    url: `${OPENURL}&window=one`,
    active: false,
    windowId: 1,
  });
  h.tabs.patch(existing.id!, { groupId: 700 });
  h.tabGroups?.live.set(700, {
    id: 700,
    collapsed: true,
    title: "papio",
    windowId: 1,
  });
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
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  installManagedTabLedger(h, {});
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
  expect(h.tabs.grouped).toEqual([
    { tabIds: [100] },
    { tabIds: [101], groupId: groupID! },
  ]);
  expect(h.tabGroups?.live.get(groupID!)?.title).toBe(
    "papio — A paper still awaiting institutional access",
  );
});

test("startup consolidates three papio groups in one window", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  const fixedEpoch = "epoch-fixed-consolidate";
  h.deps.randomUUID = () => fixedEpoch;
  const seededLedger: Record<string, unknown> = {};
  for (const groupID of [700, 701, 702]) {
    const tab = await h.tabs.create({
      url: `${OPENURL}&group=${groupID}`,
      active: false,
    });
    h.tabs.patch(tab.id!, { groupId: groupID });
    h.tabGroups!.live.set(groupID, {
      id: groupID,
      collapsed: groupID !== 700,
      title: "papio",
      windowId: 1,
    });
    seededLedger[String(tab.id)] = fakeBirthRecord({
      tab_hint: tab.id!,
      browser_epoch: fixedEpoch,
    });
  }
  h.tabGroups!.nextID = 703;
  installManagedTabLedger(h, seededLedger);
  await h.bridge.start();

  expect(h.tabs.grouped).toEqual([
    { tabIds: [101], groupId: 700 },
    { tabIds: [102], groupId: 700 },
  ]);
  expect(h.tabs.list().map((tab) => tab.groupId)).toEqual([700, 700, 700]);
  expect(h.tabGroups?.live.size).toBe(1);
});

test("IdP auth expands the papio group and re-collapses when auth returns", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  await h.bridge.start();
  const offer = jobOffer("job_tg_auth") as { payload: Record<string, unknown> };
  offer.payload["expected"] = {
    title: "A paper awaiting institutional access",
  };
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(true);
  expect(h.tabs.activated).toEqual([tabID]);
  expect(h.tabGroups?.live.get(groupID)?.collapsed).toBe(false);
  expect(h.tabGroups?.live.get(groupID)?.title).toBe(
    "papio — A paper awaiting institutional access",
  );

  // Auth returns to a provider host: the job advances and the group folds away.
  const providerURL = `https://${PROVIDER_HOST}/stable/123`;
  await h.tabs.completeNavigation(tabID, providerURL);
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
    {
      ...emptyStore(),
      authEvidenceByOrigin: {
        "https://resolver.example.edu": 1_700_000_000_000,
      },
    },
    { tabGroups: true, handoffSurface: "tab-group" },
  );
  const first = jobOffer("job_tg_multiple_first") as {
    payload: Record<string, unknown>;
  };
  first.payload["expected"] = {
    title: "A paper that should not claim a shared group",
  };

  await h.bridge.start();
  await h.port.inbound(first);
  await h.port.inbound(jobOffer("job_tg_multiple_second"));
  const firstTabID =
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_tg_multiple_first",
    )?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;
  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";

  await h.tabs.userNavigate(firstTabID, idpURL);

  expect(h.tabGroups?.live.get(groupID)?.title).toBe("papio");
});

test("an emptied papio group's id is dropped so the next handoff regroups", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tg_idle"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;
  // Chrome removes the group once its last tab is gone.
  h.tabGroups?.close(groupID);
  await h.tabs.userClose(tabID);
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.backend.store.handoffGroupID).toBeUndefined();
});

test("a live papio group keeps its generic collapsed title when the last handoff closes", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  const offer = jobOffer("job_tg_keepalive") as {
    payload: Record<string, unknown>;
  };
  offer.payload["expected"] = {
    title: "Paper metadata must disappear on cancellation",
  };
  await h.bridge.start();
  await h.port.inbound(offer);
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID!;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.tabGroups?.live.get(groupID)).toMatchObject({
    collapsed: false,
    title: "papio — Paper metadata must disappear on cancellation",
  });

  // The group still exists because a keepalive tab remains folded into it.
  await h.tabs.userClose(tabID);
  const collapse = h.timers.find((timer) => timer.ms === 5_000);
  expect(collapse).toBeDefined();
  h.clock.now += 5_000;
  await collapse!.fn();
  expect(h.backend.store.activeJobs.length).toBe(0);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabGroups?.live.get(groupID)).toMatchObject({
    collapsed: true,
    title: "papio",
  });
});

test("IdP navigation surfaces the work-window tab: activate + restore + focus", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_ww_auth"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.userNavigate(tabID, idpURL);

  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(true);
  expect(h.tabs.activated).toEqual([tabID]);
  expect(h.windows?.updated).toEqual([
    {
      windowID: 500,
      props: { focused: true, drawAttention: true, state: "normal" },
    },
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
  await h.downloads.onCreated.emit({
    id: 7,
    tabId: tabID,
    state: "in_progress",
  });
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
  await h.downloads.onCreated.emit({
    id: 8,
    tabId: tabID,
    state: "in_progress",
  });
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const sanitized = sanitizeFixture(
    `<main class="article">Known structure</main>`,
    {
      provider: "jstor",
      scenario: "success",
      originNoQuery: "https://www.jstor.org/stable/123",
      capturedISO: "2026-07-27T10:11:12.000Z",
    },
  );
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
    error: {
      code: "unauthorized",
      message: "This sender cannot send page captures",
    },
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
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
      message:
        "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
    },
  });
  expect(h.frames().some((frame) => frame.type === "page_capture")).toBe(false);

  await h.port.inbound(
    helloAck({ features: ["page_capture_v1", "page_capture_terms_v1"] }),
  );
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

test("capture consent refreshes live false-to-true and true-to-false", async () => {
  const h = makeHarness(undefined, { firefox: true, captureConsent: false });
  let allowed = false;
  h.deps.captureConsent = { get: async () => allowed };
  const sanitized = sanitizeFixture(
    `<main class="article">Consent transition</main>`,
    {
      provider: "jstor",
      scenario: "success",
      originNoQuery: "https://www.jstor.org/stable/consent",
      capturedISO: "2026-08-10T00:00:00.000Z",
    },
  );
  const encoded = await encodePageCapture(sanitized, {
    host: "www.jstor.org",
    scenario: "success",
    adapterID: "jstor",
    adapterVersion: "1",
  });
  if (!encoded.ok) throw new Error(encoded.error);
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  await expect(h.bridge.sendPageCapture(encoded.payload)).resolves.toBe(false);
  allowed = true;
  await expect(h.bridge.sendPageCapture(encoded.payload)).resolves.toBe(true);
  allowed = false;
  await expect(h.bridge.sendPageCapture(encoded.payload)).resolves.toBe(false);
  expect(
    h.frames().filter((frame) => frame.type === "page_capture"),
  ).toHaveLength(1);
});

// The ledger's origin_digest (Slice 2b) is computed with crypto.subtle.digest,
// which Bun never settles across a pure microtask (`await Promise.resolve()`)
// spin — deterministic fake timers cannot help either, since nothing here
// waits on a timer. `setTimeout(r, 0)` is the minimal real yield that lets
// the pending digest's native completion callback run.
test("page capture request closes an inactive ledgered managed tab after focus moves away", async () => {
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  let ledger: Record<string, unknown> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== capturePage) return [];
    return [
      {
        result: {
          html: `<html><body><main class="article">Captured structure</main></body></html>`,
          origin: "https://www.jstor.org",
          path: "/stable/123",
        },
      },
    ];
  };
  h.deps.tabs.query = async () => h.tabs.list();
  expect(h.deps.tabs.query).toBeDefined();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["page_capture_v1", "page_capture_request_v1"],
    }),
  );
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
  for (
    let attempt = 0;
    attempt < 20 && h.tabs.created.length === 0;
    attempt += 1
  ) {
    await new Promise((r) => setTimeout(r, 0));
  }
  for (
    let attempt = 0;
    attempt < 100 && (h.windows?.updated.length ?? 0) === 0;
    attempt += 1
  ) {
    await new Promise((r) => setTimeout(r, 0));
  }
  expect(h.windows?.created).toEqual([
    {
      url: "https://www.jstor.org/stable/123",
      focused: false,
      state: "minimized",
    },
  ]);
  expect(h.tabs.created).toEqual([
    { url: "https://www.jstor.org/stable/123", active: false, windowId: 500 },
  ]);
  expect(h.windows?.updated).toContainEqual({
    windowID: 500,
    props: { focused: true, drawAttention: true, state: "normal" },
  });
  const tabID = h.tabs.list()[0]?.id ?? -1;
  await h.tabs.userActivate(tabID);
  for (
    let attempt = 0;
    attempt < 20 && ledger[String(tabID)] === undefined;
    attempt += 1
  ) {
    await new Promise((r) => setTimeout(r, 0));
  }
  expect(ledger[String(tabID)]).toBeDefined();
  h.tabs.seed({
    id: 999,
    url: "https://operator.example.test/away",
    active: false,
    windowId: 500,
  });
  await h.tabs.userActivate(999); // the operator switches away before capture cleanup
  for (
    let attempt = 0;
    attempt < 20 && !h.timers.some((timer) => timer.ms === 30_000);
    attempt += 1
  ) {
    await new Promise((r) => setTimeout(r, 0));
  }
  await h.tabs.completeNavigation(tabID);
  for (
    let attempt = 0;
    attempt < 20 && !h.timers.some((timer) => timer.ms === 3_000);
    attempt += 1
  ) {
    await new Promise((r) => setTimeout(r, 0));
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
  const result = h
    .frames()
    .find((frame) => frame.type === "page_capture_request_result");
  expect(result?.payload).toEqual({
    request_id: "capture-request-001",
    outcome: "captured",
  });
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(false);
  expect(h.downloads.started).toHaveLength(0);
});

test("page capture request respects the single-drive handoff governor", async () => {
  const h = makeHarness(
    {
      ...emptyStore(),
      authEvidenceByOrigin: {
        "https://resolver.example.edu": 1_700_000_000_000,
      },
    },
    { handoffSurface: "in-window" },
  );
  h.deps.permissions.contains = async () => true;
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["page_capture_v1", "page_capture_request_v1"],
    }),
  );
  await h.port.inbound(jobOffer("job_capture_governor_1"));
  await h.port.inbound(jobOffer("job_capture_governor_2"));
  expect(h.tabs.created).toHaveLength(1);

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
  expect(h.tabs.created).toHaveLength(1);
  const result = h
    .frames()
    .find(
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const message = { type: "papio.triage.counts", request: {} };

  for (const sender of [
    {
      id: "papio-test-id",
      url: "chrome-extension://papio-test-id/options.html",
    },
    { id: "papio-test-id", url: "https://provider.example/article" },
    { id: "other-extension", url: urls.inboxURL },
  ]) {
    await expect(
      handleInboxRuntimeMessage(h.bridge, message, sender, urls),
    ).resolves.toEqual({
      ok: false,
      error: {
        code: "unauthorized",
        message: "This sender cannot access the inbox broker",
      },
    });
  }

  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: [] }),
  );
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      message,
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "feature_unavailable",
      message: "This daemon does not support the requested inbox feature",
    },
  });
});
// The popup may read AGGREGATES it renders itself (Activity for catch-up, counts
// for the pulse header's decision count). The triage SNAPSHOT carries citations
// and identifiers, and every mutation owns a decision, so both stay inbox-only:
// the popup closes on focus loss and must not own a result it cannot show.
test("papio.activity and counts accept popup senders while snapshot and mutations stay inbox-only", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const reply = {
    ok: true as const,
    feature: true,
    request_id: "request-activity-1",
    generated_at: "2026-08-03T00:00:01Z",
    entries: [
      {
        seq: 1,
        at: "2026-08-03T00:00:00Z",
        kind: "download_complete",
        text: "Ready",
      },
    ],
    has_more: false,
    latest_seq: 1,
  };
  let requestedRequest:
    | { limit?: number; before_seq?: string; seen_through_seq?: string }
    | undefined;
  h.bridge.requestActivity = async (request) => {
    requestedRequest = request;
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
  expect(requestedRequest).toEqual({ limit: 10 });
  const stubbed = {
    ok: true as const,
    counts: { pending_total: 1 },
    generated_at: "2026-08-03T00:00:01Z",
  };
  h.bridge.requestTriageCounts = async () => stubbed;
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.triage.counts", request: {} },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toBe(stubbed);

  for (const type of [
    "papio.triage.snapshot",
    "papio.triage.decide",
    "papio.action.resolve",
    "papio.delivery.reconcile",
  ]) {
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        { type, request: {} },
        { id: urls.runtimeID, url: urls.popupURL },
        urls,
      ),
    ).resolves.toMatchObject({ ok: false, error: { code: "unauthorized" } });
  }
});

test("papio.stats from any papio page routes to the bridge stats request", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
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
      handleInboxRuntimeMessage(
        h.bridge,
        { type: "papio.stats", request: {} },
        { id: urls.runtimeID, url },
        urls,
      ),
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  let statsCalls = 0;
  h.bridge.requestStats = async (): ReturnType<Bridge["requestStats"]> => {
    statsCalls += 1;
    return {
      ok: false,
      error: { code: "unexpected", message: "the bridge must not be reached" },
    };
  };

  for (const sender of [
    {
      id: urls.runtimeID,
      url: "chrome-extension://papio-test-id/options.html",
    },
    { id: urls.runtimeID, url: "https://provider.example/article" },
    { id: "other-extension", url: urls.historyURL },
  ]) {
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        { type: "papio.stats", request: {} },
        sender,
        urls,
      ),
    ).resolves.toEqual({
      ok: false,
      error: {
        code: "unauthorized",
        message: "This sender cannot access papio stats",
      },
    });
  }

  const sender = { id: urls.runtimeID, url: urls.popupURL };
  for (const message of [
    { type: "papio.stats", request: { unexpected: true } },
    { type: "papio.stats" },
    { type: "papio.stats", request: {}, extra: 1 },
  ]) {
    await expect(
      handleInboxRuntimeMessage(h.bridge, message, sender, urls),
    ).resolves.toEqual({
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
  optionsURL: "chrome-extension://papio-test-id/options.html",
  pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
};

const pageBulkRequestFixtures: {
  type: string;
  request: Record<string, unknown>;
  unauthorizedMessage: string;
  invalidMessage: string;
  /** Which *papio* page(s) may send this type. `scan` is popup-only, most of
   * the family is workspace-only, scanner-consent reads/writes accept the
   * three consent surfaces, and the whole-list read is Options-only. */
  gate: "popup" | "pageBulk" | "consent" | "options";
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
    request: { tab_id: 5, expected_origin: "https://scholar.example.edu" },
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
    request: {
      scan_id: "scan-1",
      identifiers: [
        { local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" },
      ],
    },
    unauthorizedMessage: "This sender cannot look up page-bulk status",
    invalidMessage: "Invalid page-bulk status request",
    gate: "pageBulk",
  },
  {
    type: "papio.pageBulk.submit",
    request: {
      scan_id: "scan-1",
      canonical_keys: ["work:1"],
      source: {
        kind: "browser_page",
        origin: "https://scholar.example.edu",
        detector: "generic-identifiers/1",
      },
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
    gate: "consent",
  },
  {
    type: "papio.pageBulk.allowlist.set",
    request: { origin: "https://scholar.example.edu", allowed: true },
    unauthorizedMessage: "This sender cannot change the scanner allowlist",
    invalidMessage: "Invalid scanner allowlist request",
    gate: "consent",
  },
  {
    type: "papio.pageBulk.allowlist.list",
    request: {},
    unauthorizedMessage: "This sender cannot list the scanner allowlist",
    invalidMessage: "Invalid scanner allowlist request",
    gate: "options",
  },
];

/** The pages a gate accepts, and *papio*'s own pages it must still refuse. */
function gatePageURLs(
  gate: "popup" | "pageBulk" | "consent" | "options",
  urls: typeof pageBulkTestURLs,
): { authorized: string[]; refusedOwnPages: string[] } {
  switch (gate) {
    case "popup":
      return {
        authorized: [urls.popupURL],
        refusedOwnPages: [urls.pageBulkURL, urls.optionsURL, urls.inboxURL],
      };
    case "pageBulk":
      return {
        authorized: [urls.pageBulkURL],
        refusedOwnPages: [urls.popupURL, urls.optionsURL, urls.inboxURL],
      };
    case "consent":
      return {
        authorized: [urls.popupURL, urls.pageBulkURL, urls.optionsURL],
        refusedOwnPages: [urls.inboxURL, urls.historyURL],
      };
    case "options":
      return {
        authorized: [urls.optionsURL],
        refusedOwnPages: [
          urls.popupURL,
          urls.pageBulkURL,
          urls.inboxURL,
          urls.historyURL,
        ],
      };
  }
}

function stubPageBulkBridge(h: Harness): { calls: number } {
  const counter = { calls: 0 };
  const unreached = {
    ok: false as const,
    error: { code: "unexpected", message: "the bridge must not be reached" },
  };
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
  h.bridge.pageBulkAllowlistList = async () => {
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

  for (const {
    type,
    request,
    unauthorizedMessage,
    gate,
  } of pageBulkRequestFixtures) {
    const { authorized, refusedOwnPages } = gatePageURLs(gate, urls);
    for (const sender of [
      // Foreign extension id — an authorized URL shape, different id.
      ...authorized.map((url) => ({ id: "other-extension", url })),
      // Correct id, but a content script on an arbitrary page.
      { id: urls.runtimeID, url: "https://provider.example/article" },
      // Correct id on one of papio's OWN pages that this gate still refuses —
      // the scanner-consent split is only real if the inbox cannot write it and
      // the popup cannot enumerate it.
      ...refusedOwnPages.map((url) => ({ id: urls.runtimeID, url })),
    ]) {
      await expect(
        handleInboxRuntimeMessage(h.bridge, { type, request }, sender, urls),
      ).resolves.toEqual({
        ok: false,
        error: { code: "unauthorized", message: unauthorizedMessage },
      });
    }
  }
  expect(counter.calls).toBe(0);
});

test("papio.pageBulk.* accepts every authorized papio page for its gate", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const counter = stubPageBulkBridge(h);

  for (const { type, request, gate } of pageBulkRequestFixtures) {
    for (const url of gatePageURLs(gate, urls).authorized) {
      const before = counter.calls;
      await handleInboxRuntimeMessage(
        h.bridge,
        { type, request },
        { id: urls.runtimeID, url },
        urls,
      );
      // Reaching the (stubbed) bridge is the observable proof the sender and
      // request shape both passed; the stub's reply itself is meaningless.
      expect(counter.calls).toBe(before + 1);
    }
  }
});

test("papio.pageBulk.* rejects a malformed request (extra key) via hasOnlyKeys without touching the bridge", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const counter = stubPageBulkBridge(h);

  for (const {
    type,
    request,
    invalidMessage,
    gate,
  } of pageBulkRequestFixtures) {
    const sender = {
      id: urls.runtimeID,
      url: gatePageURLs(gate, urls).authorized[0],
    };
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        { type, request: { ...request, unexpected: true } },
        sender,
        urls,
      ),
    ).resolves.toEqual({
      ok: false,
      error: { code: "invalid_request", message: invalidMessage },
    });
  }
  expect(counter.calls).toBe(0);
});

test("papio.pageBulk.scan requires a bare-HTTPS expected_origin", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const counter = stubPageBulkBridge(h);
  const sender = { id: urls.runtimeID, url: urls.popupURL };

  for (const request of [
    // Missing entirely — the pre-consent shape. It must not degrade to an
    // unchecked scan of whatever the tab currently shows.
    { tab_id: 5 },
    { tab_id: 5, expected_origin: "" },
    { tab_id: 5, expected_origin: "http://scholar.example.edu" },
    { tab_id: 5, expected_origin: "https://scholar.example.edu/list" },
    { tab_id: 5, expected_origin: "https://scholar.example.edu?q=1" },
    { tab_id: 5, expected_origin: 42 },
  ]) {
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        { type: "papio.pageBulk.scan", request },
        sender,
        urls,
      ),
    ).resolves.toEqual({
      ok: false,
      error: { code: "invalid_request", message: "Invalid page scan request" },
    });
  }
  expect(counter.calls).toBe(0);
});

test("papio.pageBulk.scan forwards the bound tab and origin to the bridge", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  let received: unknown;
  h.bridge.startPageBulkScan = async (
    tabID: number,
    expectedOrigin: string,
    pageBulkBaseURL: string,
  ) => {
    received = { tabID, expectedOrigin, pageBulkBaseURL };
    return { ok: true, scan_id: "scan-9" };
  };
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.pageBulk.scan",
        request: { tab_id: 7, expected_origin: "https://scholar.example.edu" },
      },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, scan_id: "scan-9" });
  expect(received).toEqual({
    tabID: 7,
    expectedOrigin: "https://scholar.example.edu",
    pageBulkBaseURL: urls.pageBulkURL,
  });
});

// --- ADR-0019 Decision 2: the click invokes the scan, the allowlist consents
// to it. Every test here asserts on injection COUNT, because the whole point is
// that no DOM is read before consent exists — a refusal that happens after
// executeScript would be a privacy failure that still returns the right code.

/** Count only scans (scanDocument injections); adapter/planner injections from
 * unrelated code paths must not be mistaken for a page read. */
function countScanInjections(h: Harness): () => number {
  let scans = 0;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === scanDocument) scans += 1;
    return [
      { result: { papers: [], truncated: false, renderedRecordCountHint: null } },
    ];
  };
  return () => scans;
}

test("a first scan of an unapproved origin is refused before the page is read", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://scholar.example.edu/refs" });

  await expect(
    h.bridge.runPageBulkScan(42, "https://scholar.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "scanner_consent_required",
      message: "Allow scanning on this site before papio reads the page",
    },
  });
  expect(scans()).toBe(0);
  expect(Object.keys(h.pageBulkScans.current.byId)).toEqual([]);
});

test("a scan of an allowlisted origin reads the page and persists a snapshot", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://scholar.example.edu/refs" });
  h.scannerAllowlist.origins = ["https://scholar.example.edu"];

  const result = await h.bridge.runPageBulkScan(42, "https://scholar.example.edu");
  expect(result.ok).toBe(true);
  expect(scans()).toBe(1);
  if (!result.ok) throw new Error(result.error.message);
  expect(result.snapshot.sourceOrigin).toBe("https://scholar.example.edu");
  expect(result.snapshot.documentGeneration).toBe(1);
});

test("an allowlist grant for one origin never authorizes scanning a different origin", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://other.example.edu/refs" });
  h.scannerAllowlist.origins = ["https://scholar.example.edu"];

  await expect(
    h.bridge.runPageBulkScan(42, "https://other.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "scanner_consent_required",
      message: "Allow scanning on this site before papio reads the page",
    },
  });
  expect(scans()).toBe(0);
});

test("a tab that navigated away from the bound origin is refused as page_changed, not scanned under the bound consent", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  // Both origins are allowlisted, so the ONLY thing that can refuse this is the
  // binding check: consent for site A must not read site B just because the
  // researcher happens to have allowed B too — they clicked on A.
  h.tabs.seed({ id: 42, url: "https://elsewhere.example.edu/article" });
  h.scannerAllowlist.origins = [
    "https://scholar.example.edu",
    "https://elsewhere.example.edu",
  ];

  await expect(
    h.bridge.runPageBulkScan(42, "https://scholar.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "page_changed",
      message: "The source page changed — try again",
    },
  });
  expect(scans()).toBe(0);
});

test("scanner consent fails closed when there is no allowlist storage at all", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://scholar.example.edu/refs" });
  delete h.deps.scannerAllowlist;

  await expect(
    h.bridge.runPageBulkScan(42, "https://scholar.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "scanner_consent_required",
      message: "Allow scanning on this site before papio reads the page",
    },
  });
  expect(scans()).toBe(0);
});

function seedSnapshot(
  h: Harness,
  overrides?: Partial<PageBulkSnapshotView>,
): PageBulkSnapshotView {
  const snapshot: PageBulkSnapshotView = {
    scanId: "scan-1",
    sourceTabId: 42,
    sourceOrigin: "https://scholar.example.edu",
    sourceTitle: "References",
    pdfGrabAvailable: false,
    scannedAt: "2026-08-14T00:00:00.000Z",
    documentGeneration: 1,
    items: [],
    truncated: false,
    renderedRecordCountHint: null,
    ...overrides,
  };
  h.pageBulkScans.current = withPageBulkSnapshot(
    emptyPageBulkScanStore(),
    snapshot,
  );
  return snapshot;
}

test("Rescan is refused before reading the page once scanning consent is revoked", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://scholar.example.edu/refs" });
  seedSnapshot(h);
  // Consent was granted when the workspace opened and revoked since. The open
  // workspace must not keep reading the page on the strength of a stale grant.
  h.scannerAllowlist.origins = [];

  await expect(h.bridge.requestPageBulkRescan("scan-1")).resolves.toEqual({
    ok: false,
    error: {
      code: "scanner_consent_required",
      message: "Allow scanning on this site before papio reads the page",
    },
  });
  expect(scans()).toBe(0);
});

test("Rescan succeeds again once the origin is re-allowed, bumping the generation", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://scholar.example.edu/refs" });
  seedSnapshot(h);
  h.scannerAllowlist.origins = ["https://scholar.example.edu"];

  const result = await h.bridge.requestPageBulkRescan("scan-1");
  expect(result.ok).toBe(true);
  expect(scans()).toBe(1);
  if (!result.ok) throw new Error(result.error.message);
  expect(result.snapshot.documentGeneration).toBe(2);
  expect(result.snapshot.scanId).toBe("scan-1");
});

test("Rescan after the source tab moved to another site refuses instead of rebinding the snapshot", async () => {
  const h = makeHarness();
  const scans = countScanInjections(h);
  h.tabs.seed({ id: 42, url: "https://elsewhere.example.edu/article" });
  seedSnapshot(h);
  h.scannerAllowlist.origins = [
    "https://scholar.example.edu",
    "https://elsewhere.example.edu",
  ];

  await expect(h.bridge.requestPageBulkRescan("scan-1")).resolves.toEqual({
    ok: false,
    error: {
      code: "source_changed",
      message: "The source tab moved to another site — start a new scan",
    },
  });
  expect(scans()).toBe(0);
  // The stored snapshot still names the origin it was scanned from.
  expect(h.pageBulkScans.current.byId["scan-1"]?.sourceOrigin).toBe(
    "https://scholar.example.edu",
  );
});

test("the scanner allowlist list read is validated, deduplicated, and sorted", async () => {
  const h = makeHarness();
  h.scannerAllowlist.origins = [
    "https://zeta.example.edu",
    "https://alpha.example.edu",
    "https://zeta.example.edu",
    "http://insecure.example.edu",
    "https://alpha.example.edu/path",
    "not a url",
  ];

  await expect(h.bridge.pageBulkAllowlistList()).resolves.toEqual({
    ok: true,
    origins: ["https://alpha.example.edu", "https://zeta.example.edu"],
  });
});

test("the scanner allowlist list read is empty, not absent, with no storage", async () => {
  const h = makeHarness();
  delete h.deps.scannerAllowlist;
  await expect(h.bridge.pageBulkAllowlistList()).resolves.toEqual({
    ok: true,
    origins: [],
  });
});

test("allowing then revoking one origin leaves the others untouched", async () => {
  const h = makeHarness();
  h.scannerAllowlist.origins = ["https://kept.example.edu"];

  await expect(
    h.bridge.setPageBulkAllowlist("https://scholar.example.edu", true),
  ).resolves.toEqual({ ok: true, allowed: true });
  expect(h.scannerAllowlist.origins).toContain("https://scholar.example.edu");
  await expect(
    h.bridge.pageBulkAllowlistContains("https://scholar.example.edu"),
  ).resolves.toEqual({ ok: true, allowed: true });

  await expect(
    h.bridge.setPageBulkAllowlist("https://scholar.example.edu", false),
  ).resolves.toEqual({ ok: true, allowed: false });
  expect(h.scannerAllowlist.origins).toEqual(["https://kept.example.edu"]);
});

test("isPageBulkSender strips the ?scan=<id> query when matching the workspace page", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  h.bridge.requestPageBulkStatus = async () => ({
    ok: true,
    items: [],
    truncated: false,
  });
  const sender = {
    id: urls.runtimeID,
    url: `${urls.pageBulkURL}?scan=scan-42`,
  };
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.pageBulk.status",
        request: {
          scan_id: "scan-42",
          identifiers: [
            { local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" },
          ],
        },
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
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.pageBulk.load", request: { scan_id: "scan-1" } },
      sender,
      urls,
    ),
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
      items: [
        {
          local_id: "id-1",
          canonical_key: "work:1",
          status: "eligible",
          ownership_complete: true,
        },
      ],
      truncated: false,
    };
  };
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL };
  const request = {
    scan_id: "scan-1",
    identifiers: [
      { local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" },
    ],
  };
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.pageBulk.status", request },
      sender,
      urls,
    ),
  ).resolves.toEqual({
    ok: true,
    items: [
      {
        local_id: "id-1",
        canonical_key: "work:1",
        status: "eligible",
        ownership_complete: true,
      },
    ],
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
    return {
      ok: true,
      mode: "v2",
      processed_count: 1,
      submitted: 1,
      joined: 0,
      already_owned: 0,
      invalid: 0,
      batch_id: "batch_1",
    };
  };
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL };
  const request = {
    scan_id: "scan-1",
    canonical_keys: ["work:1"],
    source: {
      kind: "browser_page",
      origin: "https://scholar.example.edu",
      detector: "generic-identifiers/1",
    },
  };
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.pageBulk.submit", request },
      sender,
      urls,
    ),
  ).resolves.toEqual({
    ok: true,
    mode: "v2",
    processed_count: 1,
    submitted: 1,
    joined: 0,
    already_owned: 0,
    invalid: 0,
    batch_id: "batch_1",
  });
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
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({
    host: new URL(request.url).hostname,
    title: request.title,
  });
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
  await expect(replyPromise).resolves.toEqual({
    ok: true,
    grab_id: "grab-00000001",
  });
  expect(h.downloads.started[0]).toMatchObject({
    url: request.url,
    conflictAction: "uniquify",
    saveAs: false,
  });

  const suggestions: { filename: string; conflictAction: string }[] = [];
  await h.downloads.onDeterminingFilename.emit(
    {
      id: 901,
      url: request.url,
      filename: "/tmp/received-paper.pdf",
      state: "in_progress",
    },
    (suggestion) => suggestions.push(suggestion),
  );
  expect(suggestions).toEqual([
    {
      filename: "papio/grabs/grabpath/received-paper.pdf",
      conflictAction: "uniquify",
    },
  ]);

  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  expect(h.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-00000001",
    state: "identifying",
  });
});
test("papio.pageBulk.grabPdf fails closed before native request when effect permits are not negotiated", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: CURRENT_DAEMON, features: ["pdf_grab_v1"] }),
  );
  const before = h.frames().length;
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.pageBulk.grabPdf",
        request: {
          tab_id: 42,
          url: "https://resolver.example.edu/content/no-permit.pdf",
          scan_id: "scan-no-permit",
        },
      },
      { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "feature_unavailable",
      message: "papio needs an update — run papio doctor.",
    },
  });
  expect(
    h
      .frames()
      .slice(before)
      .filter((frame) => frame.type === "pdf_grab_request"),
  ).toHaveLength(0);
  expect(h.downloads.started).toHaveLength(0);
  expect(h.pdfGrabCorrelations.current).toEqual({});
});

test("papio.pageBulk.grabPdf abandons the exact grab when download initiation fails", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  h.downloads.failDownload = true;
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const replyPromise = handleInboxRuntimeMessage(
    h.bridge,
    {
      type: "papio.pageBulk.grabPdf",
      request: {
        tab_id: 42,
        url: "https://resolver.example.edu/content/fails.pdf",
        scan_id: "scan-grab-fails",
      },
    },
    { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } },
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-fails-0001",
      steering_path: "papio/grabs/grab-fails-0001/",
    }),
  );
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  expect(abandonFrame.payload).toMatchObject({ grab_id: "grab-fails-0001" });
  expect(typeof abandonFrame.payload["request_id"]).toBe("string");
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandonFrame.payload["request_id"] as string,
      grab_id: "grab-fails-0001",
      state: "abandoned",
      outcome: "abandoned",
    }),
  );
  await expect(replyPromise).resolves.toEqual({
    ok: false,
    error: {
      code: "grab_failed",
      message: "Could not start the browser download",
    },
  });
  expect(h.downloads.started).toHaveLength(1);
  expect(h.pdfGrabCorrelations.current).toEqual({});
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
  await first.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    first.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await first.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({
    host: new URL(request.url).hostname,
  });
  await first.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-restart",
      steering_path: "papio/grabs/grabpath/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({
    ok: true,
    grab_id: "grab-restart",
  });
  const persisted = JSON.parse(
    JSON.stringify(first.pdfGrabCorrelations.current),
  ) as Record<string, PdfGrabCorrelation>;

  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
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
  expect(restarted.pdfGrabCorrelations.current["grab-restart"]).toMatchObject({
    abandonPending: true,
  });
});

test("papio.pageBulk.grabPdf clears a conflict acknowledgment with its settled state", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const request = {
    tab_id: 42,
    url: "https://resolver.example.edu/conflict.pdf",
    title: "Conflict",
    scan_id: "scan-grab-conflict",
  };
  const replyPromise = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } },
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-conflict-0001",
      steering_path: "papio/grabs/conflict/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({
    ok: true,
    grab_id: "grab-conflict-0001",
  });
  await h.downloads.onChanged.emit({
    id: 901,
    state: { current: "interrupted" },
  });
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandonFrame.payload["request_id"] as string,
      grab_id: "grab-conflict-0001",
      state: "quarantined",
      outcome: "conflict",
      detail: "pdf grab is already settled",
    }),
  );
  expect(h.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-conflict-0001",
    state: "identifying",
    detail: "pdf grab is already settled",
  });
  expect(h.pdfGrabCorrelations.current).toEqual({});
});

test("a refused abandon is not reported as a cancellation", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const request = {
    tab_id: 42,
    url: "https://resolver.example.edu/refused-abandon.pdf",
    scan_id: "scan-grab-refused-abandon",
  };
  const replyPromise = handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } },
    urls,
  );
  const requestFrame = await h.port.waitForFrame("pdf_grab_request");
  await h.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-refused-abandon",
      steering_path: "papio/grabs/refusedabandon/",
    }),
  );
  await expect(replyPromise).resolves.toMatchObject({ ok: true });
  await h.downloads.onChanged.emit({
    id: 901,
    state: { current: "interrupted" },
  });
  const abandonFrame = await h.port.waitForFrame("pdf_grab_abandon_request");
  // The daemon declines to cancel and carries no state at all. Defaulting this
  // to "abandoned" told the researcher their download had been cancelled while
  // the daemon still owned whatever the grab became.
  await h.port.inbound(
    nativeResult("pdf_grab_abandon_result", {
      request_id: abandonFrame.payload["request_id"] as string,
      grab_id: "grab-refused-abandon",
      state: "",
      outcome: "unavailable",
      detail: "only the browser that started this download can cancel it",
    }),
  );
  expect(h.runtimeMessages).toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-refused-abandon",
    state: "failed",
    detail: "only the browser that started this download can cancel it",
  });
  expect(h.runtimeMessages).not.toContainEqual({
    type: "papio.pageBulk.grabState",
    scan_id: request.scan_id,
    grab_id: "grab-refused-abandon",
    state: "abandoned",
    detail: "The PDF grab download was interrupted",
  });
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
  await first.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    first.bridge,
    { type: "papio.pageBulk.grabPdf", request },
    sender,
    urls,
  );
  const requestFrame = await first.port.waitForFrame("pdf_grab_request");
  expect(requestFrame.payload).toMatchObject({
    host: new URL(request.url).hostname,
  });
  await first.port.inbound(
    nativeResult("pdf_grab_result", {
      request_id: requestFrame.payload["request_id"] as string,
      outcome: "steering",
      grab_id: "grab-terminal",
      steering_path: "papio/grabs/grabpath/",
    }),
  );
  await expect(replyPromise).resolves.toEqual({
    ok: true,
    grab_id: "grab-terminal",
  });
  const persisted = JSON.parse(
    JSON.stringify(first.pdfGrabCorrelations.current),
  ) as Record<string, PdfGrabCorrelation>;

  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  h.tabs.seed({ id: 88, url: urls.inboxURL, windowId: 600 });
  h.windows?.live.set(600, { id: 600, state: "minimized" });

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.openInbox" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ opened: true });
  expect(h.tabs.activated).toEqual([88]);
  expect(h.windows?.updated).toContainEqual({
    windowID: 600,
    props: { focused: true },
  });
  expect(h.tabs.created).toEqual([]);

  h.tabs.clear();
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.openInbox" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ opened: true });
  expect(h.tabs.created).toEqual([{ url: urls.inboxURL, active: true }]);

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.openInbox" },
      {
        id: urls.runtimeID,
        url: "chrome-extension://papio-test-id/options.html",
      },
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_inbox_open"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.handoff.open",
        request: { job_id: "job_0001a_inbox_open" },
      },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.activated).toEqual([tabID]);
  const liveTab = h.tabs.snapshot(tabID);
  if (liveTab?.windowId === undefined)
    throw new Error("The offered tab has no live window");
  expect(
    h.windows?.updated.some(
      (update) =>
        update.windowID === liveTab.windowId && update.props.focused === true,
    ),
  ).toBe(true);

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
      {
        type: "papio.handoff.open",
        request: { job_id: "job_0001a_inbox_open" },
      },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({ ok: true, opened: true });
  expect(h.tabs.activated).toEqual([tabID]);

  for (const message of [
    {
      type: "papio.triage.decide",
      request: { item_id: "item_001", op: "acquire" },
    },
    {
      type: "papio.action.resolve",
      request: { action_id: 1, verdict: "reject", expected_revision: 1 },
    },
    { type: "papio.preview", request: { action_id: 1 } },
  ]) {
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        message,
        { id: urls.runtimeID, url: urls.popupURL },
        urls,
      ),
    ).resolves.toEqual({
      ok: false,
      error: {
        code: "unauthorized",
        message: "This sender cannot access the inbox broker",
      },
    });
  }

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.handoff.open",
        request: { job_id: "job_0001a_inbox_open" },
      },
      {
        id: urls.runtimeID,
        url: "chrome-extension://papio-test-id/options.html",
      },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "unauthorized",
      message: "This sender cannot access the inbox broker",
    },
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
  });
  await h.bridge.start();
  seedOfferURLs(h, { [jobID]: OPENURL });
  const [first, second] = await Promise.all([
    h.bridge.openHandoff(jobID),
    h.bridge.openHandoff(jobID),
  ]);

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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  const keepaliveAPI: KeepaliveAPI = {
    tabs: {
      create: async (properties) =>
        h.tabs.create({
          url: properties.url,
          active: properties.active,
          ...(properties.windowId === undefined
            ? {}
            : { windowId: properties.windowId }),
        }),
      reload: (tabID) => h.tabs.reload(tabID),
      get: (tabID) => h.tabs.get(tabID),
      query: async (query) => {
        const pattern = query.url?.[0];
        if (pattern === undefined) return [];
        const origin = pattern.endsWith("/*") ? pattern.slice(0, -2) : pattern;
        return h.tabs.list().filter((tab) => {
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
  h.tabs.seed({ id: 777, url: "https://resolver.example.edu/account" });
  h.bridge.attachKeepalive(manager);

  // papio.session.state is a pure read: it must not inject into the operator's
  // library tab. Only papio.session.probe may, and the popup sends that once
  // when it opens.
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.state" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toMatchObject({ ok: true });
  expect(injections).toBe(0);

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.probe" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const collegeOrigin = "https://onesearch.library.example-college.edu";
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      resolver_origins: ["https://resolver.example.edu", collegeOrigin],
    }),
  );

  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.state" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
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

test("configured auth-like resolver origins remain in strict membership", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://sso.resolver.example.edu"] }),
  );

  expect(h.bridge.knownResolverOrigins()).toEqual([
    "https://sso.resolver.example.edu",
  ]);
});

test("generic engagement alone is omitted from session authentication demand", async () => {
  const h = makeHarness({
    ...emptyStore(),
    resolverOrigins: ["https://resolver.example.edu"],
    activeJobs: [
      {
        job_id: "job_generic_engagement",
        tab_id: -1,
        offered_at: 1,
        expires_at: 2,
        status: "queued",
        provider_hosts: [],
        engagement_required: true,
      },
    ],
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://resolver.example.edu"] }),
  );

  expect(h.bridge.sessionAuthDemand()).toEqual([]);
});

test("origin-specific sign-in rejects an arbitrary origin before a current hello", async () => {
  const h = makeHarness();
  await h.bridge.start();

  await expect(
    h.bridge.requestSessionSignIn("https://arbitrary.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "resolver_unavailable",
      message: "The daemon has not confirmed configured institutions",
    },
  });
  expect(h.tabs.created).toEqual([]);
});

test("old-daemon empty resolver sets allow only the keepalive current origin", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck());
  const currentOrigin = "https://legacy-resolver.example.edu";
  const opened: (string | undefined)[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: currentOrigin,
      pausedForReauth: false,
    }),
    openReauth: async (originHint?: string) => {
      opened.push(originHint);
      return true;
    },
  } as unknown as KeepaliveManager);

  await expect(h.bridge.requestSessionSignIn(currentOrigin)).resolves.toEqual({
    ok: true,
    opened: true,
  });
  await expect(
    h.bridge.requestSessionSignIn("https://unknown-legacy.example.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "resolver_unavailable",
      message: "This institution is not currently configured",
    },
  });
  expect(opened).toEqual([currentOrigin]);
});

test("session state binds active authentication demand to the exact resolver origin", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const originA = "https://resolver.example.edu";
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: [originA, "https://stale.other.example"] }),
  );
  await h.port.inbound(jobOffer("job_demand_a", OPENURL));
  const tabID =
    h.backend.store.activeJobs.find((job) => job.job_id === "job_demand_a")
      ?.tab_id ?? -1;
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/sso");
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      { type: "papio.session.state" },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toMatchObject({
    ok: true,
    state: {
      authDemand: [{ job_id: "job_demand_a", origin: originA }],
    },
  });
});

test("provider and OA offer URLs never mint institution session rows", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://resolver.example.edu"] }),
  );
  // Direct/OA offers land on provider hosts; each used to become a phantom
  // "institution" in the popup session card.
  await h.port.inbound(
    jobOffer(
      "job_provider_a",
      "https://www.sciencedirect.com/science/article/pii/1",
    ),
  );
  await h.port.inbound(
    jobOffer("job_provider_b", "https://direct.mit.edu/reco/article/1"),
  );

  expect(h.bridge.knownResolverOrigins()).toEqual([
    "https://resolver.example.edu",
  ]);
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
    optionsURL: "chrome-extension://papio-test-id/options.html",
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
    error: {
      code: "resolver_unavailable",
      message: "No resolver configured yet — open a paper first",
    },
  });
});

test("session sign-in opens the resolver origin in a foreground tab as fallback", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const offer = jobOffer("job_session_signin") as {
    payload: Record<string, unknown>;
  };
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
  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: true },
  ]);
});

test("handoff_focus surfaces the tracked work-window tab without creating another", async () => {
  const h = makeHarness(undefined, { windows: true });
  const jobID = "job_0001a_focus";
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID =
    h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id ??
    -1;
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
    i < 10 &&
    !h.windows?.updated.some(
      (update) => update.windowID === 500 && update.props.focused === true,
    );
    i += 1
  ) {
    await Promise.resolve();
  }

  expect(h.tabs.created).toHaveLength(1);
  expect(h.tabs.activated).toContain(tabID);
  expect(
    h.windows?.updated.some(
      (update) => update.windowID === 500 && update.props.focused === true,
    ),
  ).toBe(true);
  expect(h.tabs.navigations).toEqual([]);
});

test("handoff_focus re-drives an auth-pending tab without charging the automatic retry budget", async () => {
  const h = makeHarness(undefined, { windows: true });
  const jobID = "job_0001a_focus_auth";
  const idpURL = "https://idp.example.edu/sso";
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID =
    h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id ??
    -1;
  const authTab = { id: tabID, url: idpURL, windowId: 500 };
  h.tabs.seed(authTab);
  await h.tabs.userNavigate(tabID, idpURL);
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
});

test("an inbox dismiss relays verdict dismiss through the native resolve", async () => {
  // Regression: the inbox and native protocol both speak verdict "dismiss"
  // (Go enumRequired and protocol.ts both allow it), but the runtime dispatch
  // switch omitted papio.action.resolve, so every human-action dismissal
  // stalled without sending a native frame.
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_mutations_v1"],
    }),
  );

  const pending = handleInboxRuntimeMessage(
    h.bridge,
    {
      type: "papio.action.resolve",
      request: { action_id: 142, verdict: "dismiss", expected_revision: 1 },
    },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  const frame = await h.port.waitForFrame("human_action_resolve");
  expect(frame.payload["verdict"]).toBe("dismiss");
  expect(frame.payload["action_id"]).toBe(142);
  expect(frame.payload["expected_revision"]).toBe(1);
  expect(frame.payload["expected_sha256"]).toBeUndefined();

  await h.port.inbound(
    nativeResult("human_action_resolve_result", {
      request_id: frame.payload["request_id"],
      outcome: "applied",
    }),
  );
  await expect(pending).resolves.toEqual({ ok: true, outcome: "applied" });

  // A genuinely unknown verdict must still be rejected with the exact error.
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.action.resolve",
        request: { action_id: 142, verdict: "cancel", expected_revision: 1 },
      },
      { id: urls.runtimeID, url: urls.inboxURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "invalid_request",
      message: "Invalid action resolution request",
    },
  });
});

test("an inbox delivery reconciliation relays confirm_request_exists/absent through the native delivery_reconcile frame", async () => {
  const h = makeHarness();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_schema_v3"],
    }),
  );

  const pending = handleInboxRuntimeMessage(
    h.bridge,
    {
      type: "papio.delivery.reconcile",
      request: {
        job_id: "job_delivery_0001",
        operation: "confirm_request_exists",
        provider_reference: "TN-42",
      },
    },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  const frame = await h.port.waitForFrame("delivery_reconcile_request");
  expect(frame.payload["job_id"]).toBe("job_delivery_0001");
  expect(frame.payload["operation"]).toBe("confirm_request_exists");
  expect(frame.payload["provider_reference"]).toBe("TN-42");

  await h.port.inbound(
    nativeResult("delivery_reconcile_result", {
      request_id: frame.payload["request_id"],
      outcome: "applied",
      detail: "delivery request confirmed",
    }),
  );
  const postedBeforeAbsent = h.port.posted.length;
  const pendingAbsent = handleInboxRuntimeMessage(
    h.bridge,
    {
      type: "papio.delivery.reconcile",
      request: {
        job_id: "job_delivery_0002",
        operation: "confirm_request_absent",
      },
    },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  const absentFrame = await h.port.waitForFrame(
    "delivery_reconcile_request",
    postedBeforeAbsent,
  );
  expect(absentFrame.payload["job_id"]).toBe("job_delivery_0002");
  expect(absentFrame.payload["operation"]).toBe("confirm_request_absent");
  expect(absentFrame.payload["provider_reference"]).toBeUndefined();
  await h.port.inbound(
    nativeResult("delivery_reconcile_result", {
      request_id: absentFrame?.payload["request_id"],
      outcome: "applied",
    }),
  );
  await expect(pendingAbsent).resolves.toEqual({
    ok: true,
    outcome: "applied",
  });

  // Shape validation and sender authorization must both fail closed without
  // reaching native messaging. Missing/empty references are invalid for
  // confirm_request_exists; even an explicit undefined provider_reference key
  // is forbidden for confirm_request_absent.
  const reconcileFrameCount = () =>
    h.frames().filter((f) => f.type === "delivery_reconcile_request").length;
  const beforeMalformed = reconcileFrameCount();
  const malformedRequests = [
    { job_id: "job_delivery_0003", operation: "confirm_request_exists" },
    {
      job_id: "job_delivery_0003",
      operation: "confirm_request_exists",
      provider_reference: "",
    },
    {
      job_id: "job_delivery_0003",
      operation: "confirm_request_absent",
      provider_reference: undefined,
    },
    {
      job_id: "job_delivery_0003",
      operation: "confirm_request_absent",
      extra: true,
    },
    {
      job_id: "bad!",
      operation: "confirm_request_exists",
      provider_reference: "TN-43",
    },
    {
      job_id: "job_delivery_0003",
      operation: "other",
      provider_reference: "TN-43",
    },
  ];
  for (const request of malformedRequests) {
    await expect(
      handleInboxRuntimeMessage(
        h.bridge,
        { type: "papio.delivery.reconcile", request },
        { id: urls.runtimeID, url: urls.inboxURL },
        urls,
      ),
    ).resolves.toEqual({
      ok: false,
      error: {
        code: "invalid_request",
        message: "Invalid delivery reconciliation request",
      },
    });
  }
  expect(reconcileFrameCount()).toBe(beforeMalformed);
  await expect(
    handleInboxRuntimeMessage(
      h.bridge,
      {
        type: "papio.delivery.reconcile",
        request: {
          job_id: "job_delivery_0003",
          operation: "confirm_request_absent",
        },
      },
      { id: urls.runtimeID, url: urls.popupURL },
      urls,
    ),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "unauthorized",
      message: "This sender cannot access the inbox broker",
    },
  });
  expect(reconcileFrameCount()).toBe(beforeMalformed);
});

test("queued inbox handoff force-releases exactly one live tab under racing opens", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_0001a_handoff_active"));
  await h.port.inbound(jobOffer("job_0001a_handoff_queued"));

  const queuedID = "job_0001a_handoff_queued";
  await h.bridge.requestCancel("job_0001a_handoff_active");
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === queuedID)?.status,
  ).toBe("queued");
  const [first, second] = await Promise.all([
    h.bridge.openHandoff(queuedID),
    h.bridge.openHandoff(queuedID),
  ]);
  const released = h.backend.store.activeJobs.find(
    (job) => job.job_id === queuedID,
  );
  const releasedTabID = released?.tab_id ?? -1;

  expect(first).toEqual({ ok: true, opened: true });
  expect(second).toEqual({ ok: true, opened: true });
  expect(releasedTabID).toBeGreaterThanOrEqual(100);
  expect(h.tabs.created).toHaveLength(2);
  expect(h.tabs.activated).toContain(releasedTabID);
  expect(
    h.windows?.updated.some(
      (update) => update.windowID === h.tabs.snapshot(releasedTabID)?.windowId,
    ),
  ).toBe(true);
});

test("an unknown inbox handoff makes one counts refresh before failing unavailable", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1"],
    }),
  );

  const pending = h.bridge.openHandoff("job_0001a_not_offered");
  const refresh = await h.port.waitForFrame("triage_counts_request");
  const refreshes = h
    .frames()
    .filter((frame) => frame.type === "triage_counts_request");
  const requestID = refresh.payload["request_id"];
  expect(refreshes).toHaveLength(1);
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("triage_counts_response", {
      request_id: requestID as string,
      counts: triageCounts(),
    }),
  );

  await expect(pending).resolves.toEqual({
    ok: false,
    error: {
      code: "handoff_unavailable",
      message: "The requested handoff is not available",
    },
  });
  expect(
    h.frames().filter((frame) => frame.type === "triage_counts_request"),
  ).toHaveLength(1);
});

test("triage native replies correlate by request_id even when they arrive out of order", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: [
        "triage_snapshot_v1",
        "triage_mutations_v1",
        "review_preview_v1",
      ],
    }),
  );

  const first = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  const second = h.bridge.requestTriageSnapshot({ schema_versions: [1] });
  await Promise.resolve();
  await Promise.resolve();
  const requests = h
    .frames()
    .filter((frame) => frame.type === "triage_snapshot_request");
  expect(requests.map((frame) => frame.payload["schema_versions"])).toEqual([
    [1],
    [1],
  ]);
  const firstID = requests[0]?.payload["request_id"];
  const secondID = requests[1]?.payload["request_id"];
  expect(typeof firstID).toBe("string");
  expect(typeof secondID).toBe("string");

  await h.port.inbound(snapshotResult(secondID as string, 2));
  await h.port.inbound(snapshotResult(firstID as string, 1));
  await expect(first).resolves.toMatchObject({
    ok: true,
    snapshot: { counts: { pending_total: 1 } },
  });
  await expect(second).resolves.toMatchObject({
    ok: true,
    snapshot: { counts: { pending_total: 2 } },
  });
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
  const request = h
    .frames()
    .find((frame) => frame.type === "triage_snapshot_request");
  expect(request?.payload["schema_versions"]).toEqual([2]);
  const requestID = request?.payload["request_id"];
  await h.port.inbound(snapshotResult(requestID as string, 1, 2));
  await expect(pending).resolves.toMatchObject({ ok: true });
});

test("triage counts negotiate independently of snapshot schema 4", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: [
        "triage_snapshot_v1",
        "triage_snapshot_schema_v4",
        "triage_counts_schema_v2",
      ],
    }),
  );

  const pending = h.bridge.requestTriageCounts();
  await Promise.resolve();
  await Promise.resolve();
  const request = h
    .frames()
    .find((frame) => frame.type === "triage_counts_request");
  expect(request?.payload["schema_versions"]).toEqual([2]);
  const requestID = request?.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("triage_counts_response", {
      request_id: requestID as string,
      counts: triageCounts(3),
    }),
  );
  await expect(pending).resolves.toMatchObject({
    ok: true,
    counts: { pending_total: 3 },
  });
});

test("triage requests time out and late echoes are dropped", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1"],
    }),
  );
  const pending = h.bridge.requestTriageCounts();
  await Promise.resolve();
  await Promise.resolve();
  const request = h
    .frames()
    .find((frame) => frame.type === "triage_counts_request");
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
    await expect(pending).resolves.toMatchObject({
      ok: false,
      error: { code: "timeout" },
    });
    await h.port.inbound(
      nativeResult("triage_counts_response", {
        request_id: requestID as string,
        counts: triageCounts(3),
      }),
    );
  } finally {
    console.debug = originalDebug;
  }
  expect(
    debugLines.some((line) =>
      line.join(" ").includes("unknown or late correlated response"),
    ),
  ).toBe(true);
});
test("surface presence does not retry on a transport failure within one poll", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["surface_presence_v1"],
    }),
  );
  const pending = h.bridge.sendSurfacePresence({
    instance_id: "instance-1",
    surface: "popup",
    focused: true,
    at: "2027-01-01T00:00:00Z",
  });
  await h.port.waitForFrame("surface_presence");
  await h.port.emitDisconnect();
  await expect(pending).resolves.toMatchObject({ ok: false });
  expect(
    h.frames().filter((frame) => frame.type === "surface_presence"),
  ).toHaveLength(1);
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
  await reconnected?.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1"],
    }),
  );
  await Promise.resolve();
  const request = h
    .frames()
    .find((frame) => frame.type === "triage_snapshot_request");
  const requestID = request?.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await reconnected?.inbound(snapshotResult(requestID as string, 1));
  await expect(pending).resolves.toMatchObject({
    ok: true,
    snapshot: { counts: { pending_total: 1 } },
  });
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
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(h.action.titles.at(-1)).toBe(
    "papio: 1 paper needs your institution sign-in",
  );

  const refresh = h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  const request = h
    .frames()
    .find((frame) => frame.type === "triage_counts_request");
  const requestID = request?.payload["request_id"];
  expect(typeof requestID).toBe("string");
  await h.port.inbound(
    nativeResult("triage_counts_response", {
      request_id: requestID as string,
      counts: triageCounts(4),
    }),
  );
  await refresh;
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe(
    "papio: 1 paper needs your institution sign-in",
  );

  h.deps.permissions.contains = async () => false;
  await h.bridge.syncConnectionBadge();
  expect(h.action.texts.at(-1)).toBe("1");
  expect(h.action.titles.at(-1)).toBe(
    "papio: 1 paper needs your institution sign-in",
  );

  h.deps.permissions.contains = async () => true;
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.completeNavigation(tabID, providerURL);
  expect(h.action.texts.at(-1)).toBe("4");
  expect(h.action.backgroundColors.at(-1)).toBe("#1a73e8");
  expect(h.action.titles.at(-1)).toBe("papio: 4 pending items");

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
  await h.tabs.userNavigate(tabID, idpURL);
  expect(h.action.texts.at(-1)).toBe("1");

  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.completeNavigation(tabID, providerURL);

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
const STALE_IDP_URL =
  "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO?execution=e1s2";
const STALE_IDP_TITLE = "Example University Login Service - Stale Request";
const OPENATHENS_ERROR_URL =
  "https://login.openathens.net/saml/2/sso/example.edu/redirect";
const OPENATHENS_ERROR_TITLE = "Error | OpenAthens";

function openAthensDocument(
  body: string,
  title = OPENATHENS_ERROR_TITLE,
): Document {
  const window = new Window({ url: OPENATHENS_ERROR_URL });
  window.document.write(
    `<html><head><title>${title}</title></head><body>${body}</body></html>`,
  );
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
  const tabID =
    h.backend.store.activeJobs.find((j) => j.job_id === jobID)?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  return tabID;
}

/** Land the broker tab on the dead Shibboleth page via a title-only update.
 * Later failures include their loading boundary so they are distinct documents. */
async function emitStaleIdPTitle(
  h: Harness,
  tabID: number,
  newDocument = false,
): Promise<void> {
  const tab = {
    id: tabID,
    url: STALE_IDP_URL,
    title: STALE_IDP_TITLE,
    windowId: 500,
  };
  h.tabs.seed(tab);
  if (newDocument) {
    await h.tabs.userNavigate(tabID, STALE_IDP_URL);
    await h.tabs.update(tabID, { status: "loading" });
  }
  await h.tabs.update(tabID, { title: STALE_IDP_TITLE });
}
test("OpenAthens redirect-loop error parks the job provider and retains its tab", async () => {
  const h = makeHarness();
  for (const marker of [
    "Too many redirects",
    "Service provider redirecting to OpenAthens",
    "GA-AP-4021-06",
    "OA-AP-4031-05",
  ]) {
    expect(
      assessDrivenPage(openAthensDocument(`<p>${marker}</p>`), true),
    ).toEqual({
      kind: "redirect_loop",
    });
  }
  expect(
    assessDrivenPage(
      openAthensDocument("<p>GA-AP-4021-06</p>", "Sign in | OpenAthens"),
      true,
    ),
  ).toEqual({ kind: "normal" });
  expect(
    assessDrivenPage(openAthensDocument("<p>GA-AP-4021-06</p>"), false),
  ).toEqual({
    kind: "normal",
  });
  const document = openAthensDocument(OPENATHENS_REDIRECT_BODY);
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== assessDrivenPage) return [];
    return [
      { result: assessDrivenPage(document, injection.args?.[1] === true) },
    ];
  };
  await h.bridge.start();
  await h.port.inbound(
    jobOfferForHosts("job_openathens_loop", ["www.tandfonline.com"]),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = {
    id: tabID,
    url: OPENATHENS_ERROR_URL,
    title: OPENATHENS_ERROR_TITLE,
    windowId: 1,
    active: true,
  };
  h.tabs.seed(tab);

  await h.tabs.update(tabID, { title: OPENATHENS_ERROR_TITLE });

  const relevantFrames = h
    .frames()
    .filter(
      (frame) =>
        (frame.type === "handoff_outcome" &&
          frame.payload["outcome"] === "auth_error") ||
        (frame.type === "error" &&
          frame.payload["code"] === "challenge_blocked"),
    );
  expect(
    relevantFrames.map((frame) =>
      frame.type === "handoff_outcome"
        ? frame.payload["outcome"]
        : frame.payload["code"],
    ),
  ).toEqual(["auth_error", "challenge_blocked"]);
  expect(relevantFrames.filter((frame) => frame.type === "error")).toHaveLength(
    1,
  );
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "tandfonline.com",
    challenge_kind: "redirect_loop",
  });
  expect(h.backend.store.challengeCooldowns).toEqual({
    "tandfonline.com": h.clock.now + 600_000,
  });
  expect(
    h.backend.store.providerDrainLeases?.["www.tandfonline.com"]?.parkedReason,
  ).toBe("challenge");
  expect(
    h.backend.store.challengeCooldowns?.["login.openathens.net"],
  ).toBeUndefined();
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
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
    return [
      { result: assessDrivenPage(document, injection.args?.[1] === true) },
    ];
  };
  await h.bridge.start();
  await h.port.inbound(
    jobOfferForHosts("job_openathens_late_body", ["www.tandfonline.com"]),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = {
    id: tabID,
    url: OPENATHENS_ERROR_URL,
    title: OPENATHENS_ERROR_TITLE,
    windowId: 1,
    active: true,
  };
  h.tabs.seed(tab);

  await h.tabs.update(tabID, { title: OPENATHENS_ERROR_TITLE });
  expect(
    h
      .frames()
      .some(
        (frame) =>
          frame.type === "error" &&
          frame.payload["code"] === "challenge_blocked",
      ),
  ).toBe(false);
  const rechecks = h.timers.filter((timer) => timer.ms === 1_500);
  expect(rechecks).toHaveLength(1);

  document = openAthensDocument(OPENATHENS_REDIRECT_BODY);
  await rechecks[0]!.fn();
  await rechecks[0]!.fn();

  expect(assessments).toBe(2);
  expect(
    h
      .frames()
      .filter(
        (frame) =>
          frame.type === "error" &&
          frame.payload["code"] === "challenge_blocked",
      ),
  ).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    challenge_host: "tandfonline.com",
    challenge_kind: "redirect_loop",
  });
  expect(h.backend.store.challengeCooldowns?.["tandfonline.com"]).toBe(
    h.clock.now + 600_000,
  );
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
  expect(h.tabs.navigations).toEqual([]);
});

test("a normal OpenAthens sign-in page remains untouched", async () => {
  const h = makeHarness();
  const title = "Sign in | OpenAthens";
  const document = openAthensDocument(
    "<main><h1>Sign in</h1><form></form></main>",
    title,
  );
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func !== assessDrivenPage) return [];
    return [
      { result: assessDrivenPage(document, injection.args?.[1] === true) },
    ];
  };
  await h.bridge.start();
  await h.port.inbound(
    jobOfferForHosts("job_openathens_sign_in", ["www.tandfonline.com"]),
  );
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = {
    id: tabID,
    url: OPENATHENS_ERROR_URL,
    title,
    windowId: 1,
    active: true,
  };
  h.tabs.seed(tab);

  await h.tabs.update(tabID, { title });

  expect(h.frames().some((frame) => frame.type === "handoff_outcome")).toBe(
    false,
  );
  expect(
    h
      .frames()
      .some(
        (frame) =>
          frame.type === "error" &&
          frame.payload["code"] === "challenge_blocked",
      ),
  ).toBe(false);
  expect(h.backend.store.activeJobs[0]?.challenge_blocked).toBeUndefined();
  expect(h.backend.store.challengeCooldowns ?? {}).toEqual({});
  expect(h.timers.filter((timer) => timer.ms === 1_500)).toHaveLength(0);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
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
  expect(outcome?.payload).toMatchObject({
    outcome: "stale_sso",
    final_host: "idp.example.edu",
  });
  // Surfaced: tab activated and the minimized window restored and focused.
  expect(h.tabs.activated).toContain(tabID);
  expect(h.windows?.updated).toContainEqual({
    windowID: 500,
    props: { focused: true, drawAttention: true, state: "normal" },
  });
  // ...and only then re-driven through the retained resolver link.
  expect(h.tabs.snapshot(tabID)?.url).toBe(OPENURL);
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
    const loginTab = {
      id: tabID,
      url: STALE_IDP_URL,
      title: "Institution sign-in",
      windowId: 500,
    };
    h.tabs.seed(loginTab);
    await h.tabs.userNavigate(tabID, STALE_IDP_URL);
    expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");

    const navigationsBeforeError = h.tabs.navigations.length;
    const errorTab = { ...loginTab, title };
    h.tabs.seed(errorTab);
    await h.tabs.update(tabID, { title });

    expect(
      h.frames().find((frame) => frame.type === "handoff_outcome")?.payload,
    ).toMatchObject({ outcome });
    expect(h.tabs.navigations).toHaveLength(navigationsBeforeError);
    expect(h.tabs.snapshot(tabID)?.url).toBe(STALE_IDP_URL);
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
  const tab = {
    id: tabID,
    url: STALE_IDP_URL,
    title: STALE_IDP_TITLE,
    windowId: 500,
  };
  h.tabs.seed(tab);

  await Promise.all([
    h.tabs.completeNavigation(tabID, STALE_IDP_URL),
    h.tabs.update(tabID, { title: STALE_IDP_TITLE }),
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
    expect(h.tabs.snapshot(tabID)?.url).toBe(OPENURL); // re-driven
  }

  await emitStaleIdPTitle(h, tabID, true);
  expect(h.backend.store.authAttempts?.["job_stale_budget"]).toBe(3); // capped
  expect(h.tabs.snapshot(tabID)?.url).toBe(STALE_IDP_URL); // left for the human
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
  h.windows?.updated.splice(0);

  // Two title callbacks can race for the same still-loaded document; keep both
  // updates concurrent so the fake does not invent a second navigation after
  // the first callback has legitimately re-driven the tab.
  await Promise.all([emitStaleIdPTitle(h, tabID), emitStaleIdPTitle(h, tabID)]);

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
  const request = h
    .frames()
    .filter((frame) => frame.type === "review_preview_request");
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
      sha256:
        "434376057dbe6fc3e44d0fdb7268b56126e9b47ca9d6aa9e05cf2e738f72e81d",
      size_bytes: 2570670,
      expires_at: "2036-07-27T02:39:46.877077Z",
    },
  } as never);

  await expect(pending).resolves.toMatchObject({
    ok: true,
    preview: {
      url: "http://127.0.0.1:50795/p/YD5r_87qRLi0Ut9XMwt8E4Cq-Bv7N4VvSoEyMsH7-Fs",
    },
  });
});

// A structured refusal must surface as a named reason, not as another silent
// timeout. The transport succeeded, so ok stays true and the inbox branches on
// outcome — see the comment at inbox.ts around the preview click handler.
test("a refused review preview reports the daemon's reason", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ daemon_version: "0.12.0", features: ["review_preview_v1"] }),
  );

  const pending = h.bridge.requestPreview({ action_id: 9999 });
  await Promise.resolve();
  await Promise.resolve();
  const request = h
    .frames()
    .filter((frame) => frame.type === "review_preview_request");

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

test("handoff governor keeps one drive, drains FIFO on settle and timeout", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await h.bridge.start();
  const jobIDs = Array.from(
    { length: 5 },
    (_, index) => `job_governor_${index}`,
  );
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.list().length).toBe(1);
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.tab_id < 0),
  ).toHaveLength(4);
  expect(
    h.frames().filter((frame) => frame.type === "job_accept"),
  ).toHaveLength(5);

  const first = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(first?.tab_id).toBeGreaterThanOrEqual(0);
  await h.bridge.requestCancel(jobIDs[0]!);
  expect(h.tabs.list().length).toBe(2);
  expect(h.tabs.created).toHaveLength(2);

  const timeout = h.timers.at(-1);
  expect(timeout?.ms).toBe(180_000);
  h.clock.now += 180_000;
  await timeout?.fn();
  expect(h.tabs.list().length).toBe(3);
  expect(h.tabs.created).toHaveLength(3);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[1])?.status,
  ).toBe("auth_pending");
});

test("a drive timing out on an authentication page leaves the tab open and frees the governor slot", async () => {
  // papio-63955092613c7e9c: closing the tab here would destroy whatever the
  // operator has already done on a slow institutional login — a half-filled
  // IdP form, entered credentials, or an in-flight 2FA challenge — at the
  // 3-minute mark with no warning. parkHandoffForManual's own contract is to
  // leave that page for the operator; only the governor slot is freed.
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await h.bridge.start();
  const jobIDs = Array.from(
    { length: 3 },
    (_, index) => `job_auth_timeout_${index}`,
  );
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.list().length).toBe(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.tab_id < 0),
  ).toHaveLength(2);
  const first = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  const firstTabID = first?.tab_id ?? -1;
  expect(firstTabID).toBeGreaterThanOrEqual(0);

  // The operator is mid-login on the resolver's IdP when the governor timer fires.
  const idpURL = "https://idp.example.edu/sso";
  h.tabs.seed({ id: firstTabID, url: idpURL });

  const authTimeouts = h.timers.filter((t) => t.ms === 180_000);
  expect(authTimeouts).toHaveLength(1);

  h.clock.now += 180_000;
  await authTimeouts[0]?.fn();

  expect(h.tabs.removed).not.toContain(firstTabID);
  expect(h.tabs.snapshot(firstTabID) !== undefined).toBe(true);
  const after = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(after?.status).toBe("auth_pending");
  expect(after?.tab_id).toBe(-1);
  expect(
    h.frames().filter((frame) => frame.type === "auth_pending"),
  ).toHaveLength(1);

  // The next FIFO job now drives after the parked job frees its slot.
  expect(h.tabs.created).toHaveLength(2);
  const second = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[1],
  );
  expect(second?.tab_id).toBeGreaterThanOrEqual(0);
});

test("a drive timing out on an ordinary provider page leaves the tab open and frees the governor slot", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await h.bridge.start();
  const jobIDs = Array.from(
    { length: 3 },
    (_, index) => `job_provider_timeout_${index}`,
  );
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  const first = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  const firstTabID = first?.tab_id ?? -1;
  expect(firstTabID).toBeGreaterThanOrEqual(0);
  // Tab never left the resolver/provider page — no IdP navigation happened.

  const providerTimeouts = h.timers.filter((t) => t.ms === 180_000);
  expect(providerTimeouts).toHaveLength(1);
  h.clock.now += 180_000;
  await providerTimeouts[0]?.fn();

  expect(h.tabs.removed).not.toContain(firstTabID);
  expect(h.tabs.snapshot(firstTabID) !== undefined).toBe(true);
  const after = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(after?.status).toBe("auth_pending");
  expect(after?.tab_id).toBe(-1);
  expect(h.tabs.created).toHaveLength(2);
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
    h.tabs.seed({ id: tabID, url: OPENURL });
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

  expect(bridgeInternal.handoffDrives.size).toBe(1);
  expect(bridgeInternal.handoffDrives.has(jobIDs[0]!)).toBe(true);
  expect(bridgeInternal.handoffDrives.has(jobIDs[1]!)).toBe(false);
  expect(bridgeInternal.handoffDrives.has(jobIDs[2]!)).toBe(false);
  // Refused, not dropped: both excess drives are queued so the next drain
  // reuses their tabs rather than stranding either job.
  expect(
    bridgeInternal.handoffDriveQueue.some(
      (request) => request.jobID === jobIDs[1],
    ),
  ).toBe(true);
  expect(
    bridgeInternal.handoffDriveQueue.some(
      (request) => request.jobID === jobIDs[2],
    ),
  ).toBe(true);
});

test("governor-queued handoffs stay stranded after a service-worker restart — offerURLs never persists, so the startup governor scan has nothing to redrive with", async () => {
  // Pre-Slice-4, this scenario only appeared to work because the test
  // FakeBackend round-tripped StoreShape.offerURLs verbatim; the real
  // chromeBackend's serializer has always stripped it before every save
  // (state.ts's migratedState). So in production this governor-queued
  // backlog was ALREADY stranded across a genuine restart — Slice 4 (plan
  // lines 341-360) removes the illusion instead of leaving it in place.
  // Recovering these jobs needs a daemon-driven resync distinct from a
  // bare re-offer (which merely re-acks, per the FIFO note below) — out of
  // this slice's scope; tracked as a follow-up, not fixed here.
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await first.bridge.start();
  const jobIDs = Array.from(
    { length: 5 },
    (_, index) => `job_restart_governor_${index}`,
  );
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));
  expect(first.tabs.list().length).toBe(1);
  const parked = first.backend.store.activeJobs.filter((job) => job.tab_id < 0);
  expect(parked).toHaveLength(4);
  expect(parked.every((job) => job.status === "accepted")).toBe(true);

  // Only the persisted store survives a suspend; the FIFO holding those
  // accepted-but-undriven jobs is worker memory. A daemon re-offer on the
  // same URL merely re-acks it, never re-attempts the governor slot.
  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  await restarted.bridge.start();
  const parkedIDs = parked.map((job) => job.job_id);
  const drivenAfterRestart = restarted.backend.store.activeJobs.filter(
    (job) => parkedIDs.includes(job.job_id) && job.tab_id >= 0,
  );
  expect(drivenAfterRestart).toHaveLength(0);
  for (const jobID of jobIDs) await restarted.port.inbound(jobOffer(jobID));
  expect(
    restarted.backend.store.activeJobs.filter(
      (job) => parkedIDs.includes(job.job_id) && job.tab_id >= 0,
    ),
  ).toHaveLength(0);
});

test("live accepted handoffs queue behind the restored governor owner after restart", async () => {
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await first.bridge.start();
  await first.port.inbound(jobOffer("job_restart_live_a"));
  await first.port.inbound(jobOffer("job_restart_live_b"));

  const persisted = JSON.parse(
    JSON.stringify(first.backend.store),
  ) as StoreShape;
  const a = persisted.activeJobs.find(
    (job) => job.job_id === "job_restart_live_a",
  )!;
  const b = persisted.activeJobs.find(
    (job) => job.job_id === "job_restart_live_b",
  )!;
  a.tab_id = 200;
  a.status = "accepted";
  a.parked_with_tab = false;
  b.tab_id = 201;
  b.status = "accepted";
  b.parked_with_tab = false;

  const restarted = makeHarness(persisted);
  restarted.tabs.seed({ id: 200, url: OPENURL });
  restarted.tabs.seed({ id: 201, url: OPENURL });
  await restarted.bridge.start();
  const internals = restarted.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
    handoffDriveQueue: Array<{ jobID: string }>;
    releaseHandoffDrive(jobID: string): void;
    drainHandoffDriveQueue(): Promise<void>;
  };
  expect([...internals.handoffDrives.keys()]).toEqual(["job_restart_live_a"]);
  expect(internals.handoffDriveQueue.map((request) => request.jobID)).toEqual([
    "job_restart_live_b",
  ]);

  internals.releaseHandoffDrive("job_restart_live_a");
  await internals.drainHandoffDriveQueue();
  expect([...internals.handoffDrives.keys()]).toEqual(["job_restart_live_b"]);
});

test("a timeout-detached auth job survives restart without re-consuming its governor slot", async () => {
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await first.bridge.start();
  const jobIDs = Array.from(
    { length: 3 },
    (_, index) => `job_park_restart_${index}`,
  );
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));

  const parkedTabID =
    first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])
      ?.tab_id ?? -1;
  expect(parkedTabID).toBeGreaterThanOrEqual(0);
  const activeTabID =
    first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[1])
      ?.tab_id ?? -1;
  expect(activeTabID).toBe(-1);
  first.tabs.seed({ id: parkedTabID, url: "https://idp.example.edu/sso" });
  await first.tabs.userActivate(parkedTabID);
  const authTimeouts = first.timers.filter((t) => t.ms === 180_000);
  expect(authTimeouts).toHaveLength(1);
  first.clock.now += 180_000;
  await authTimeouts[0]?.fn();

  const parkedAfterTimeout = first.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(parkedAfterTimeout?.status).toBe("auth_pending");
  expect(parkedAfterTimeout?.tab_id).toBe(-1);
  expect(parkedAfterTimeout?.parked_with_tab).toBeUndefined();
  expect(first.tabs.snapshot(parkedTabID) !== undefined).toBe(true);
  expect(
    first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[1])
      ?.tab_id,
  ).toBeGreaterThanOrEqual(0);

  const survivingTabs = first.backend.store.activeJobs
    .filter((job) => job.tab_id >= 0)
    .map((job) => [job.job_id, job.tab_id] as const);
  expect(survivingTabs).toHaveLength(1);
  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  for (const [, tabID] of survivingTabs)
    restarted.tabs.seed({ id: tabID, url: OPENURL });
  await restarted.bridge.start();
  const restartedInternal = restarted.bridge as unknown as {
    handoffDrives: Map<string, { tabID: number }>;
  };
  const restartedParked = restarted.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(restartedInternal.handoffDrives.has(jobIDs[0]!)).toBe(false);
  expect(restartedParked?.tab_id).toBe(-1);
  expect(restartedParked?.status).toBe("auth_pending");
  expect(restartedParked?.parked_with_tab).toBeUndefined();
  expect(restartedInternal.handoffDrives.has(jobIDs[1]!)).toBe(true);
  expect(restartedInternal.handoffDrives.has(jobIDs[2]!)).toBe(false);
  expect(restartedInternal.handoffDrives.size).toBe(1);
  expect(restarted.timers.filter((t) => t.ms === 180_000)).toHaveLength(1);
});

test("a timeout-detached job does not re-associate an operator tab", async () => {
  const h = makeHarness({
    ...emptyStore(),
    lastAuthReturnedAt: 1_700_000_000_000,
  });
  await h.bridge.start();
  const jobIDs = ["job_park_clear_0", "job_park_clear_1"];
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  const detachedTabID =
    h.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])
      ?.tab_id ?? -1;
  expect(detachedTabID).toBeGreaterThanOrEqual(0);
  h.tabs.seed({ id: detachedTabID, url: "https://idp.example.edu/sso" });
  const timeout = h.timers.find((t) => t.ms === 180_000);
  h.clock.now += 180_000;
  await timeout?.fn();
  const detached = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(detached?.status).toBe("auth_pending");
  expect(detached?.tab_id).toBe(-1);
  expect(detached?.parked_with_tab).toBeUndefined();
  expect(h.tabs.snapshot(detachedTabID) !== undefined).toBe(true);

  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.userNavigate(detachedTabID, providerURL);
  const unchanged = h.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(unchanged?.status).toBe("auth_pending");
  expect(unchanged?.tab_id).toBe(-1);
});
test("a timeout-detached job stays outside the governor until a fresh drive", async () => {
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await first.bridge.start();
  const jobIDs = Array.from(
    { length: 3 },
    (_, index) => `job_resume_capacity_${index}`,
  );
  for (const jobID of jobIDs) await first.port.inbound(jobOffer(jobID));

  const detachedTabID =
    first.backend.store.activeJobs.find((job) => job.job_id === jobIDs[0])
      ?.tab_id ?? -1;
  expect(detachedTabID).toBeGreaterThanOrEqual(0);
  first.tabs.seed({ id: detachedTabID, url: "https://idp.example.edu/sso" });
  const authTimeouts = first.timers.filter((t) => t.ms === 180_000);

  expect(authTimeouts).toHaveLength(1);
  first.clock.now += 180_000;
  await authTimeouts[0]?.fn();

  const beforeResume = first.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
  };
  expect(beforeResume.handoffDrives.size).toBe(1);
  expect(beforeResume.handoffDrives.has(jobIDs[0]!)).toBe(false);
  const resumed = await first.bridge.resumeHandoffAfterManual(jobIDs[0]!);
  expect(resumed).toBe(false);
  const detached = first.backend.store.activeJobs.find(
    (job) => job.job_id === jobIDs[0],
  );
  expect(detached?.tab_id).toBe(-1);
  expect(detached?.status).toBe("auth_pending");
  expect(detached?.parked_with_tab).toBeUndefined();
  expect(first.tabs.snapshot(detachedTabID) !== undefined).toBe(true);

  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  await restarted.bridge.start();
  const restartedInternal = restarted.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
  };
  expect(restartedInternal.handoffDrives.has(jobIDs[0]!)).toBe(false);
});

// isDirectFileOffer is a URL-SHAPE heuristic, and an institutional handoff's
// offer URL is the operator's configured OpenURL base, whose path papio does
// not constrain. So a pdf-shaped base on a requires_auth offer never took the
// direct-download shortcut (that gate is shape AND requires_auth !== true) —
// it is a real handoff. Excluding it from restore on shape alone stranded
// exactly the jobs this restore exists to recover.
test("restore recovers an auth-required handoff whose OpenURL base looks like a file", async () => {
  const pdfShapedBase =
    "https://resolver.example.edu/openurl/content/pdf/resolve";
  const first = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await first.bridge.start();
  await first.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  const jobIDs = Array.from(
    { length: 4 },
    (_, index) => `job_pdfbase_governor_${index}`,
  );
  for (const jobID of jobIDs) {
    const offer = jobOffer(jobID, pdfShapedBase) as {
      payload: Record<string, unknown>;
    };
    offer.payload["requires_auth"] = true;
    await first.port.inbound(offer);
  }
  // The direct-download shortcut must not have fired: requires_auth true keeps
  // every one of these on the handoff path despite the file-shaped URL.
  expect(first.downloads.started).toHaveLength(0);
  const parked = first.backend.store.activeJobs.filter((job) => job.tab_id < 0);
  expect(parked.length).toBeGreaterThan(0);
  expect(parked.every((job) => job.status === "accepted")).toBe(true);

  // A restarted worker has no negotiated features until its own hello_ack
  // lands, and reconciliation runs first — so the Slice 0 containment gate
  // parks the requires_auth backlog for explicit engagement instead of
  // re-driving it into surfaces (surface-lifecycle plan: restore must not
  // multiply sign-in tabs). The direct-download shortcut still never fires.
  const restarted = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  await restarted.bridge.start();
  // this worker's offerURLs cache starts empty (never persisted); re-deliver
  // the daemon's natural reconnect re-offer for the backlog before checking
  // the Slice 0 gate demoted it to queued/engagement_required.
  for (const jobID of jobIDs) {
    const offer = jobOffer(jobID, pdfShapedBase) as {
      payload: Record<string, unknown>;
    };
    offer.payload["requires_auth"] = true;
    await restarted.port.inbound(offer);
  }
  expect(restarted.downloads.started).toHaveLength(0);
  const parkedIDs = parked.map((job) => job.job_id);
  const afterRestart = restarted.backend.store.activeJobs.filter((job) =>
    parkedIDs.includes(job.job_id),
  );
  expect(afterRestart.length).toBeGreaterThan(0);
  for (const job of afterRestart) {
    expect(job.tab_id).toBe(-1);
    expect(job.status).toBe("queued");
    expect(job.engagement_required).toBe(true);
  }
});

test("per-origin sign-in hands the tab to the keepalive manager", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck());
  const reauthOrigins: (string | undefined)[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: "https://beta.example.edu",
      pausedForReauth: false,
    }),
    noteResolverActivated: () => {},
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
  await h.port.inbound(helloAck());
  h.bridge.attachKeepalive({
    getSnapshot: () => ({
      resolverOrigin: "https://beta.example.edu",
      pausedForReauth: false,
    }),
    noteResolverActivated: () => {},
    openReauth: async () => false,
  } as unknown as KeepaliveManager);

  const reply = await h.bridge.requestSessionSignIn("https://beta.example.edu");
  expect(reply).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toHaveLength(1);
});

// The durable inbox-prefined manual_delivery_target pin has been deleted.
// The volatile one-shot DeliveryChoice nonce (interaction + candidates) supersedes it.
// These tests cover the new mechanism: needs_choice offer, one-shot consumption,
// page-identity revalidation, tab-close destruction, SW-surviving waiting_manual
// continuation, and the remaining inbox Open (now a plain tab open with no pin).

test("inbox Open is now a plain tab open with no pin side-effect", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const landingURL = "https://link.springer.com/article/10.1007/BF03392100";
  const jobID = "job_open_plain_new_0001";
  await ((h.bridge as unknown as { update: (fn: (s: unknown)=>unknown)=>Promise<void> }).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [{ job_id: jobID, tab_id: -1, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["link.springer.com"] }] };
  });
  const opened = await handleInboxRuntimeMessage(
    h.bridge,
    { type: "papio.manual.open", request: { job_id: jobID, url: landingURL } },
    { id: urls.runtimeID, url: urls.inboxURL },
    urls,
  );
  expect(opened).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([{ url: landingURL, active: true }]);
  expect((findByJob(h.backend.store, jobID) as unknown as Record<string, unknown>)?.["manual_delivery_target"]).toBeUndefined();
  expect(findByJob(h.backend.store, jobID)?.tab_id).toBe(-1);
});

test("inbox Open remains inbox-only and HTTPS-only", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const urls = {
    runtimeID: "papio-test-id",
    inboxURL: "chrome-extension://papio-test-id/inbox.html",
    popupURL: "chrome-extension://papio-test-id/popup.html",
    historyURL: "chrome-extension://papio-test-id/history.html",
    optionsURL: "chrome-extension://papio-test-id/options.html",
    pageBulkURL: "chrome-extension://papio-test-id/page-bulk.html",
  };
  const request = (jobID: string, url = "https://publisher.example.edu/article") => ({
    type: "papio.manual.open",
    request: { job_id: jobID, url, title: "Paper" },
  });
  await expect(
    handleInboxRuntimeMessage(h.bridge, request("job_manual_popup_forbidden"), { id: urls.runtimeID, url: urls.popupURL }, urls),
  ).resolves.toMatchObject({ ok: false, error: { code: "unauthorized" } });
  await expect(
    handleInboxRuntimeMessage(h.bridge, request("job_manual_http_forbidden", "http://publisher.example.edu/article"), { id: urls.runtimeID, url: urls.inboxURL }, urls),
  ).resolves.toMatchObject({ ok: false, error: { code: "invalid_request" } });
});

function awaitingJob(jobID: string, offeredAt: number, title?: string): ActiveJob {
  return {
    job_id: jobID,
    tab_id: -1,
    offered_at: offeredAt,
    expires_at: offeredAt + 86400000,
    status: "awaiting_download",
    provider_hosts: [],
    ...(title ? { expected: { title } } : {}),
  } as ActiveJob;
}

test("DOI-less uncorrelated PDF with non-empty advisory list replies needs_choice with cap 12 and newest-first ordering", async () => {
  const h = makeHarness();
  const jobs: ActiveJob[] = [];
  for (let i = 0; i < 14; i++) {
    jobs.push(awaitingJob("job_adv_" + String(i).padStart(4, "0"), 1_700_000_000_000 + i, "Title " + i));
  }
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: jobs };
  });
  await h.bridge.start();
  const pdfURL = "https://provider.example.edu/paper.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(reply["ok"]).toBe(true);
  expect(reply["state"]).toBe("needs_choice");
  const offer = reply["choice"] as { interaction: string; candidates: { job_id: string; title: string }[] };
  expect(typeof offer.interaction).toBe("string");
  expect(offer.candidates.length).toBe(12);
  expect(offer.candidates[0]!.job_id).toBe("job_adv_0013");
  expect(offer.candidates[11]!.job_id).toBe("job_adv_0002");
  expect(offer.candidates.some((c)=>c.job_id==="job_adv_0000")).toBe(false);
  expect(offer.candidates.some((c)=>c.job_id==="job_adv_0001")).toBe(false);
});

test("DOI-less uncorrelated PDF with EMPTY advisory list falls through to grab / no_doi (blind path untouched)", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const pdfURL = "https://provider.example.edu/paper.pdf";
  h.tabs.seed({ id: 42, url: pdfURL });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 42, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(reply["state"]).not.toBe("needs_choice");
  expect(reply).toMatchObject({ ok: false, error: { code: "no_doi" } });
  const h2 = makeHarness();
  await h2.bridge.start();
  await h2.port.inbound(helloAck({ features: ["pdf_grab_v1", "effect_permit_v1"] }));
  h2.tabs.seed({ id: 43, url: pdfURL });
  let grabCalled = false;
  (h2.bridge as unknown as Record<string, unknown>).requestPdfGrab = async ()=>{ grabCalled = true; return { ok: true, grab_id: "grab-001" } as unknown as never; };
  const reply2 = await h2.bridge.startPDFDelivery({ tab_id: 43, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(grabCalled).toBe(true);
  expect(reply2).toMatchObject({ ok: true, state: "sending" });
});

test("a pick with a valid choice starts delivery for exactly that job", async () => {
  const h = makeHarness();
  const jobs = [awaitingJob("job_pick_a", 1_700_000_000_001, "Alpha"), awaitingJob("job_pick_b", 1_700_000_000_002, "Beta")];
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: jobs };
  });
  await h.bridge.start();
  const pdfURL = "https://provider.example.edu/paper.pdf";
  h.tabs.seed({ id: 50, url: pdfURL });
  const first = await h.bridge.startPDFDelivery({ tab_id: 50, url: pdfURL }) as unknown as Record<string, unknown>;
  const offer = (first["choice"] as { interaction: string; candidates: { job_id: string }[] });
  const chosen = offer.candidates.find((c)=>c.job_id==="job_pick_a")!;
  expect(chosen).toBeDefined();
  const second = await h.bridge.startPDFDelivery({ tab_id: 50, url: pdfURL, choice: { interaction: offer.interaction, job_id: chosen.job_id } }) as unknown as Record<string, unknown>;
  expect(second).toMatchObject({ ok: true, job_id: "job_pick_a" });
  expect(["sending", "waiting_manual"]).toContain(String(second["state"]));
  expect(h.downloads.started.length).toBeGreaterThanOrEqual(1);
});

test("replaying the SAME interaction a second time fails (one-shot)", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [awaitingJob("job_replay_a", 1_700_000_000_001), awaitingJob("job_replay_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  const pdfURL = "https://provider.example.edu/paper.pdf";
  h.tabs.seed({ id: 51, url: pdfURL });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 51, url: pdfURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const first = await h.bridge.startPDFDelivery({ tab_id: 51, url: pdfURL, choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(first["ok"]).toBe(true);
  const replay = await h.bridge.startPDFDelivery({ tab_id: 51, url: pdfURL, choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(replay).toMatchObject({ ok: false });
  expect(String((replay as { message?: string }).message ?? (replay as { error?: { message?: string } }).error?.message ?? "")).toMatch(/expired/i);
});

test("an unknown/expired interaction fails with the expired-choice message", async () => {
  const h = makeHarness();
  await h.bridge.start();
  h.tabs.seed({ id: 52, url: "https://provider.example.edu/paper.pdf" });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 52, url: "https://provider.example.edu/paper.pdf", choice: { interaction: "deadbeefdeadbeefdeadbeefdeadbeef", job_id: "job_ghost_0001" } }) as unknown as Record<string, unknown>;
  expect(reply).toMatchObject({ ok: false });
  expect(String((reply as { message?: string }).message ?? "")).toMatch(/expired/i);
});

test("a pick naming a job_id NOT among the offered candidates fails", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [awaitingJob("job_cand_a", 1_700_000_000_001)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 53, url: "https://provider.example.edu/paper.pdf" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 53, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string };
  const outsider = await h.bridge.startPDFDelivery({ tab_id: 53, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: "job_not_offered_9999" } }) as unknown as Record<string, unknown>;
  expect(outsider).toMatchObject({ ok: false });
  expect(String((outsider as { message?: string }).message ?? "")).toMatch(/expired/i);
});

test("a pick after the tab navigated fails on page-identity mismatch", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [awaitingJob("job_nav_a", 1_700_000_000_001), awaitingJob("job_nav_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 54, url: "https://provider.example.edu/paper.pdf" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 54, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await h.webNavigation.onHistoryStateUpdated.emit({ tabId: 54, frameId: 0, url: "https://provider.example.edu/paper.pdf#page=2" });
  const afterNav = await h.bridge.startPDFDelivery({ tab_id: 54, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(afterNav).toMatchObject({ ok: false });
  expect(String((afterNav as { message?: string }).message ?? "")).toMatch(/expired/i);
});

test("a pick for a job that is no longer awaiting_download fails", async () => {
  const h = makeHarness();
  const j = awaitingJob("job_gone_a", 1_700_000_000_001);
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [j, awaitingJob("job_gone_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 55, url: "https://provider.example.edu/paper.pdf" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 55, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: s2.activeJobs.map((x)=> x.job_id==="job_gone_a" ? { ...x, status: "accepted" as const } : x) };
  });
  const gone = await h.bridge.startPDFDelivery({ tab_id: 55, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: "job_gone_a" } }) as unknown as Record<string, unknown>;
  expect(gone).toMatchObject({ ok: false });
  expect(String((gone as { message?: string }).message ?? "")).toMatch(/expired/i);
});

test("delivery_busy destroys the offer rather than leaving it consumable", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const store = st as StoreShape;
    return { ...store, activeJobs: [awaitingJob("job_busy_a", 1_700_000_000_001), awaitingJob("job_busy_b", 1_700_000_000_002), awaitingJob("job_busy_c", 1_700_000_000_003)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 56, url: "https://provider.example.edu/a.pdf" });
  h.tabs.seed({ id: 57, url: "https://provider.example.edu/b.pdf" });
  const offerA = ((await h.bridge.startPDFDelivery({ tab_id: 56, url: "https://provider.example.edu/a.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const firstPick = await h.bridge.startPDFDelivery({ tab_id: 56, url: "https://provider.example.edu/a.pdf", choice: { interaction: offerA.interaction, job_id: "job_busy_a" } }) as unknown as Record<string, unknown>;
  expect(firstPick["ok"]).toBe(true);
  const offerB = ((await h.bridge.startPDFDelivery({ tab_id: 57, url: "https://provider.example.edu/b.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const busy = await h.bridge.startPDFDelivery({ tab_id: 57, url: "https://provider.example.edu/b.pdf", choice: { interaction: offerB.interaction, job_id: "job_busy_b" } }) as unknown as Record<string, unknown>;
  expect(busy).toMatchObject({ ok: false, error: { code: "delivery_busy" } });
  const replay = await h.bridge.startPDFDelivery({ tab_id: 57, url: "https://provider.example.edu/b.pdf", choice: { interaction: offerB.interaction, job_id: "job_busy_b" } }) as unknown as Record<string, unknown>;
  expect(String((replay as { message?: string }).message ?? "")).toMatch(/expired/i);
});

test("waiting_manual continuation survives a simulated SW death and then adopts the viewer download, revalidating page identity", async () => {
  const viewerURL = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [
      { job_id: "job_wait_cont_0001", tab_id: -1, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["sciencedirect.com","pdf.sciencedirectassets.com"], adapter_id: "sciencedirect", access_mode: "delegated" as const },
      awaitingJob("job_wait_cont_0002", 1_700_000_000_010),
    ]};
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 80, url: viewerURL });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 80, url: viewerURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const pickJob = offer.candidates.find((c)=>c.job_id==="job_wait_cont_0001")?.job_id ?? offer.candidates[0]!.job_id;
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 80, url: viewerURL, choice: { interaction: offer.interaction, job_id: pickJob } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  expect(h1.backend.store.pendingDelivery?.status).toBe("waiting_manual");
  expect(h1.backend.store.pendingDelivery?.page_identity).toBeDefined();
  expect(h1.backend.store.pendingDelivery!.page_identity!.tab_id).toBe(80);
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  h2.tabs.seed({ id: 80, url: viewerURL });
  await h2.bridge.start();
  expect(h2.backend.store.pendingDelivery?.status).toBe("waiting_manual");
  const item: DownloadItemLike = { id: 9001, tabId: 80, url: viewerURL, filename: "main.pdf", state: "in_progress" };
  await h2.downloads.onCreated.emit(item);
  expect(h2.backend.store.pendingDelivery?.status).toBe("waiting_manual");
});

test("a continuation whose tab was closed or navigated does NOT adopt", async () => {
  const viewerURL = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [
      { job_id: "job_nadopt_0001", tab_id: -1, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["sciencedirect.com","pdf.sciencedirectassets.com"] },
      awaitingJob("job_nadopt_0002", 1_700_000_000_010),
    ]};
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 81, url: viewerURL });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 81, url: viewerURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 81, url: viewerURL, choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  h2.tabs.seed({ id: 82, url: viewerURL });
  await h2.bridge.start();
  await h2.webNavigation.onHistoryStateUpdated.emit({ tabId: 81, frameId: 0, url: viewerURL });
  const item: DownloadItemLike = { id: 9002, tabId: 82, url: viewerURL, filename: "main.pdf", state: "in_progress" };
  await h2.downloads.onCreated.emit(item);
  expect(h2.backend.store.pendingDelivery).toBeUndefined();
});

test("closing the tab destroys that tab's offers and any waiting_manual continuation bound to it", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_close_a", 1_700_000_000_001), awaitingJob("job_close_b", 1_700_000_000_002), awaitingJob("job_close_c", 1_700_000_000_003)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 90, url: "https://provider.example.edu/a.pdf" });
  const offerUnconsumed = ((await h.bridge.startPDFDelivery({ tab_id: 90, url: "https://provider.example.edu/a.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return startPendingDelivery(s2, { job_id: "job_close_a", url: "https://provider.example.edu/a.pdf", initiated_at: 1_700_000_000_000, status: "waiting_manual", page_identity: { tab_id: 90, nav_seq: 0, source_url: "https://provider.example.edu/a.pdf" } });
  });
  expect(h.backend.store.pendingDelivery?.page_identity?.tab_id).toBe(90);
  h.tabs.seed({ id: 91, url: "https://provider.example.edu/b.pdf" });
  const offerOther = ((await h.bridge.startPDFDelivery({ tab_id: 91, url: "https://provider.example.edu/b.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await h.tabs.onRemoved.emit(90, { isWindowClosing: false });
  expect(h.backend.store.pendingDelivery).toBeUndefined();
  const expired = await h.bridge.startPDFDelivery({ tab_id: 90, url: "https://provider.example.edu/a.pdf", choice: { interaction: offerUnconsumed.interaction, job_id: offerUnconsumed.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(String((expired as { message?: string }).message ?? "")).toMatch(/expired/i);
  const stillLive = await h.bridge.startPDFDelivery({ tab_id: 91, url: "https://provider.example.edu/b.pdf", choice: { interaction: offerOther.interaction, job_id: offerOther.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(stillLive["ok"]).toBe(true);
});

test("SAGE journal viewer downloads directly without a pin", async () => {
  const h = makeHarness();
  h.deps.adapterSpecs.push({
    id: "sage",
    version: "0.2.0",
    hosts: ["journals.sagepub.com"],
    classify: [],
    download: {
      selector: "section.format--pdf_epub",
      requireKind: "article",
      method: "url",
      viewerRoutes: [{ pathPrefix: "/doi/epdf/" }],
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate: "https://journals.sagepub.com/doi/pdf/{1}?download=true",
    },
  });
  await h.bridge.start();
  const viewerURL = "https://journals.sagepub.com/doi/epdf/10.1177/0146167207301014";
  const directURL = "https://journals.sagepub.com/doi/pdf/10.1177/0146167207301014?download=true";
  // Correlation now via tab/DOI — not a durable pin. Use a tab-bound job.
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [{ job_id: "job_sage_nopin_0001", tab_id: 200, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["journals.sagepub.com"], adapter_id: "sage", access_mode: "delegated" as const }] };
  });
  h.tabs.seed({ id: 200, url: viewerURL });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 200, url: directURL, doi: "10.1177/0146167207301014" }) as unknown as Record<string, unknown>;
  expect(reply).toMatchObject({ ok: true, state: "sending", job_id: "job_sage_nopin_0001" });
  expect(h.downloads.started[0]?.url).toBe(directURL);
});

test("Cell PII PDF response downloads directly without a pin", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const viewerURL = "https://www.cell.com/action/showPdf?pii=S2405-8440%2817%2930127-5";
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [{ job_id: "job_cell_nopin_0001", tab_id: 201, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["cell.com"] }] };
  });
  h.tabs.seed({ id: 201, url: viewerURL });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 201, url: viewerURL }) as unknown as Record<string, unknown>;
  expect(reply).toMatchObject({ ok: true, state: "sending", job_id: "job_cell_nopin_0001" });
  expect(h.downloads.started[0]?.url).toBe(viewerURL);
});


test("A->B click race does not bind B under A's choice (live tab URL)", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_race_a", 1_700_000_000_001), awaitingJob("job_race_b", 1_700_000_000_002), awaitingJob("job_race_c", 1_700_000_000_003)] };
  });
  await h.bridge.start();
  const urlA = "https://provider.example.edu/a.pdf?sig=AAA";
  const urlB = "https://provider.example.edu/b.pdf?sig=BBB";
  h.tabs.seed({ id: 70, url: urlA });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 70, url: urlA }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  h.tabs.seed({ id: 70, url: urlB });
  const race = await h.bridge.startPDFDelivery({ tab_id: 70, url: urlA, choice: { interaction: offer.interaction, job_id: "job_race_a" } }) as unknown as Record<string, unknown>;
  expect(race).toMatchObject({ ok: false, code: "choice_expired" });
});

test("same-URL full reload between mint and accept fails closed (onCommitted epoch)", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_reload_a", 1_700_000_000_001), awaitingJob("job_reload_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 71, url: "https://provider.example.edu/paper.pdf" });
  await h.webNavigation.onCommitted!.emit({ tabId: 71, frameId: 0, documentId: "doc-1" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 71, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await h.webNavigation.onCommitted!.emit({ tabId: 71, frameId: 0, documentId: "doc-2" });
  h.tabs.seed({ id: 71, url: "https://provider.example.edu/paper.pdf" });
  const afterReload = await h.bridge.startPDFDelivery({ tab_id: 71, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(afterReload).toMatchObject({ ok: false, code: "choice_expired" });
});

test("expired nonce beyond TTL fails at consume (not just sweep)", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_ttl_a", 1_700_000_000_001), awaitingJob("job_ttl_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 72, url: "https://provider.example.edu/paper.pdf" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 72, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  h.clock.now += 10 * 60_000 + 1000;
  const expired = await h.bridge.startPDFDelivery({ tab_id: 72, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(expired).toMatchObject({ ok: false, code: "choice_expired" });
  const expired2 = await h.bridge.startPDFDelivery({ tab_id: 72, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(expired2).toMatchObject({ ok: false, code: "choice_expired" });
});

test("waiting_manual revalidation fails closed after worker death with empty volatile maps (live token mismatch)", async () => {
  const viewerURL = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const otherURL = "https://pdf.sciencedirectassets.com/271849/other.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [
      { job_id: "job_wmanual_a", tab_id: -1, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: [] },
      awaitingJob("job_wmanual_b", 1_700_000_000_010),
    ]};
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 73, url: viewerURL });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 73, url: viewerURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 73, url: viewerURL, choice: { interaction: offer.interaction, job_id: offer.candidates[0]!.job_id } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  h2.tabs.seed({ id: 73, url: otherURL });
  await h2.bridge.start();
  const item: DownloadItemLike = { id: 9101, tabId: 73, url: otherURL, filename: "other.pdf", state: "in_progress" };
  await h2.downloads.onCreated.emit(item);
  expect(h2.backend.store.pendingDelivery).toBeUndefined();
});

test("onTabReplaced prerender activation clears durable continuation and allows fresh pick", async () => {
  const viewer80 = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_replace_a", 1_700_000_000_001), awaitingJob("job_replace_b", 1_700_000_000_002)] };
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 80, url: viewer80 });
  await h1.webNavigation.onCommitted!.emit({ tabId: 80, frameId: 0, documentId: "doc-80" });
  const offer80 = ((await h1.bridge.startPDFDelivery({ tab_id: 80, url: viewer80 }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 80, url: viewer80, choice: { interaction: offer80.interaction, job_id: "job_replace_a" } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  expect(h1.backend.store.pendingDelivery?.page_identity?.tab_id).toBe(80);
  await h1.webNavigation.onTabReplaced.emit({ tabId: 81, replacedTabId: 80 });
  expect(h1.backend.store.pendingDelivery).toBeUndefined();
  h1.tabs.seed({ id: 81, url: viewer80 });
  await h1.webNavigation.onCommitted!.emit({ tabId: 81, frameId: 0, documentId: "doc-81" });
  const offer81 = await h1.bridge.startPDFDelivery({ tab_id: 81, url: viewer80 }) as unknown as Record<string, unknown>;
  expect(offer81["state"]).toBe("needs_choice");
  const offer81Choice = (offer81["choice"] as { interaction: string; candidates: { job_id: string }[] });
  const repick = await h1.bridge.startPDFDelivery({ tab_id: 81, url: viewer80, choice: { interaction: offer81Choice.interaction, job_id: "job_replace_b" } }) as unknown as Record<string, unknown>;
  expect(repick["ok"]).toBe(true);
  expect(repick["job_id"]).toBe("job_replace_b");
});

/** chrome.webNavigation.onTabReplaced delivers `{tabId, replacedTabId}`; the
 * `{addedTabId, removedTabId}` pair belongs to the separate tabs.onReplaced
 * API. Reading the wrong names yields undefined on both sides, which disarms
 * the revocation entirely while every assertion written against the wrong
 * shape still passes. Both assertions below fail on the wrong field names:
 * the first needs `replacedTabId`, the second needs `tabId`. */
test("onTabReplaced revokes on the real {tabId, replacedTabId} field names", async () => {
  const viewer = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_swap_a", 1_700_000_000_001), awaitingJob("job_swap_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 300, url: viewer });
  await h.webNavigation.onCommitted.emit({ tabId: 300, frameId: 0, documentId: "doc-300" });
  const offer300 = ((await h.bridge.startPDFDelivery({ tab_id: 300, url: viewer }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h.bridge.startPDFDelivery({ tab_id: 300, url: viewer, choice: { interaction: offer300.interaction, job_id: "job_swap_a" } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  // A second, still-unspent offer minted against the prerendered tab that is
  // about to take over.
  h.tabs.seed({ id: 301, url: viewer });
  await h.webNavigation.onCommitted.emit({ tabId: 301, frameId: 0, documentId: "doc-301" });
  const offer301 = ((await h.bridge.startPDFDelivery({ tab_id: 301, url: viewer }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await h.webNavigation.onTabReplaced.emit({ tabId: 301, replacedTabId: 300 });
  // `replacedTabId` side: the continuation granted against the tab that went away.
  expect(h.backend.store.pendingDelivery).toBeUndefined();
  // `tabId` side: the offer held against the tab that took over.
  const spendNew = await h.bridge.startPDFDelivery({ tab_id: 301, url: viewer, choice: { interaction: offer301.interaction, job_id: "job_swap_b" } }) as unknown as Record<string, unknown>;
  expect(spendNew).toMatchObject({ ok: false, code: "choice_expired" });
});

/** The dangerous composition finding 10 names: a picker minted for a tab the
 * worker never saw commit, so it holds no epoch of its own. The epoch must come
 * from the browser, because the source token strips the query and two signed
 * documents at one provider path are otherwise indistinguishable. */
test("a pre-existing tab with no observed epoch still binds to the browser's document, and a same-path swap is refused", async () => {
  const pdfPath = "https://provider.example.edu/paper.pdf";
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_pre_a", 1_700_000_000_001), awaitingJob("job_pre_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  // Seeded, never committed: the worker's own navigation map knows nothing.
  h.tabs.seed({ id: 310, url: `${pdfPath}?sig=AAA` });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 310, url: `${pdfPath}?sig=AAA` }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  expect(offer).toBeDefined();
  // Same origin and path, freshly signed query, genuinely different document.
  h.tabs.seed({ id: 310, url: `${pdfPath}?sig=BBB` });
  h.webNavigation.setFrame(310, "doc-310-second");
  const spend = await h.bridge.startPDFDelivery({ tab_id: 310, url: `${pdfPath}?sig=AAA`, choice: { interaction: offer.interaction, job_id: "job_pre_a" } }) as unknown as Record<string, unknown>;
  expect(spend).toMatchObject({ ok: false, code: "choice_expired" });
  expect(h.downloads.started).toHaveLength(0);
});

/** Worker restart empties `pageNavSeq` and the epoch map both, so neither can
 * carry weight. Only the browser's own epoch can, and it must be re-read. */
test("a continuation does not adopt after a query-only document swap across a worker restart", async () => {
  const base = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_qswap_a", 1_700_000_000_001), awaitingJob("job_qswap_b", 1_700_000_000_002)] };
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 320, url: `${base}?sig=AAA` });
  await h1.webNavigation.onCommitted.emit({ tabId: 320, frameId: 0, documentId: "doc-320" });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 320, url: `${base}?sig=AAA` }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 320, url: `${base}?sig=AAA`, choice: { interaction: offer.interaction, job_id: "job_qswap_a" } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  expect(h1.backend.store.pendingDelivery?.page_identity?.document_id).toBe("doc-320");
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  // Same tab, same path, different signed document. The token cannot see it.
  h2.tabs.seed({ id: 320, url: `${base}?sig=BBB` });
  h2.webNavigation.setFrame(320, "doc-320-other");
  await h2.bridge.start();
  await h2.downloads.onCreated.emit({ id: 9301, tabId: 320, url: `${base}?sig=BBB`, filename: "main.pdf", state: "in_progress" } as DownloadItemLike);
  expect(h2.backend.store.pendingDelivery).toBeUndefined();
});

/** Same URL start to finish: only the document epoch reveals that the page was
 * navigated (or reloaded) while MV3 had the worker suspended. */
test("a continuation does not adopt after a full navigation while the worker was suspended", async () => {
  const viewer = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_susp_a", 1_700_000_000_001), awaitingJob("job_susp_b", 1_700_000_000_002)] };
  });
  await h1.bridge.start();
  h1.tabs.seed({ id: 330, url: viewer });
  await h1.webNavigation.onCommitted.emit({ tabId: 330, frameId: 0, documentId: "doc-330" });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 330, url: viewer }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  const picked = await h1.bridge.startPDFDelivery({ tab_id: 330, url: viewer, choice: { interaction: offer.interaction, job_id: "job_susp_a" } }) as unknown as Record<string, unknown>;
  expect(picked["state"]).toBe("waiting_manual");
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  h2.tabs.seed({ id: 330, url: viewer });
  // The worker slept through the navigation; the browser did not.
  h2.webNavigation.setFrame(330, "doc-330-reloaded");
  await h2.bridge.start();
  await h2.downloads.onCreated.emit({ id: 9302, tabId: 330, url: viewer, filename: "main.pdf", state: "in_progress" } as DownloadItemLike);
  expect(h2.backend.store.pendingDelivery).toBeUndefined();
});

/** `Tab.url` is optional by API contract. Absence is a refusal, never a licence
 * to fall back on whatever URL the popup said it was looking at. */
test("an absent live Tab.url refuses instead of reusing the caller-supplied URL", async () => {
  const pdfURL = "https://provider.example.edu/paper.pdf";
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_nourl_a", 1_700_000_000_001), awaitingJob("job_nourl_b", 1_700_000_000_002)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 340, url: pdfURL });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 340, url: pdfURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  // Chrome now reports the tab without a URL.
  h.tabs.seed({ id: 340 });
  const spend = await h.bridge.startPDFDelivery({ tab_id: 340, url: pdfURL, choice: { interaction: offer.interaction, job_id: "job_nourl_a" } }) as unknown as Record<string, unknown>;
  expect(spend).toMatchObject({ ok: false, code: "choice_expired" });
  expect(h.downloads.started).toHaveLength(0);
  const bare = await h.bridge.startPDFDelivery({ tab_id: 340, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(bare).toMatchObject({ ok: false, error: { code: "tab_unavailable" } });
});

/** No epoch from the platform means no way to tell two documents at one path
 * apart, so the picker and the durable manual continuation are unavailable
 * rather than approximate. */
test("a browser that reports no document epoch mints no offer and no continuation", async () => {
  const pdfURL = "https://provider.example.edu/paper.pdf";
  const viewer = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [
      awaitingJob("job_noepoch_a", 1_700_000_000_001),
      { job_id: "job_noepoch_doi", tab_id: 361, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["pdf.sciencedirectassets.com"] },
    ]};
  });
  await h.bridge.start();
  h.tabs.seed({ id: 360, url: pdfURL });
  h.webNavigation.clearFrame(360);
  const noOffer = await h.bridge.startPDFDelivery({ tab_id: 360, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(noOffer).toMatchObject({ ok: false, error: { code: "page_unverified" } });
  h.tabs.seed({ id: 361, url: viewer });
  h.webNavigation.clearFrame(361);
  const noContinuation = await h.bridge.startPDFDelivery({ tab_id: 361, url: viewer }) as unknown as Record<string, unknown>;
  expect(noContinuation).toMatchObject({ ok: false, error: { code: "page_unverified" } });
  expect(h.backend.store.pendingDelivery).toBeUndefined();
});

/** Both epochs absent used to read as "nothing changed" and adopt. A
 * continuation with no recorded epoch cannot be revalidated at all. */
test("a continuation recorded without a document epoch never adopts", async () => {
  const pdfURL = "https://provider.example.edu/paper.pdf";
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return startPendingDelivery(
      { ...s2, activeJobs: [awaitingJob("job_noepoch_cont", 1_700_000_000_001)] },
      {
        job_id: "job_noepoch_cont",
        url: pdfURL,
        initiated_at: 1_700_000_000_000,
        status: "waiting_manual",
        page_identity: { tab_id: 370, nav_seq: 0, source_url: pdfURL },
      },
    );
  });
  await h.bridge.start();
  // Everything else lines up: same tab, same token, worker map untouched.
  h.tabs.seed({ id: 370, url: pdfURL });
  expect(h.backend.store.pendingDelivery?.status).toBe("waiting_manual");
  await h.downloads.onCreated.emit({ id: 9303, tabId: 370, url: pdfURL, filename: "paper.pdf", state: "in_progress" } as DownloadItemLike);
  expect(h.backend.store.pendingDelivery).toBeUndefined();
});

test("death between sending persistence and downloads.download is recoverable via fresh pick", async () => {
  const h1 = makeHarness();
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_send_a", 1_700_000_000_001), awaitingJob("job_send_b", 1_700_000_000_002)] };
  });
  await h1.bridge.start();
  const pdfURL = "https://provider.example.edu/paper.pdf";
  h1.tabs.seed({ id: 90, url: pdfURL });
  await h1.webNavigation.onCommitted!.emit({ tabId: 90, frameId: 0, documentId: "doc-90" });
  const offer = ((await h1.bridge.startPDFDelivery({ tab_id: 90, url: pdfURL }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await ((h1.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, pendingDelivery: { job_id: offer.candidates[0]!.job_id, initiated_at: 1_700_000_000_000, status: "sending" as const } };
  });
  (h1.bridge as unknown as { deliveryJobs: Set<string> }).deliveryJobs.clear();
  const persisted = JSON.parse(JSON.stringify(h1.backend.store)) as StoreShape;
  const h2 = makeHarness(persisted);
  h2.tabs.seed({ id: 90, url: pdfURL });
  await h2.bridge.start();
  expect(h2.backend.store.pendingDelivery).toBeUndefined();
  const freshOffer = await h2.bridge.startPDFDelivery({ tab_id: 90, url: pdfURL }) as unknown as Record<string, unknown>;
  expect(freshOffer["state"]).toBe("needs_choice");
});

test("terminal delivery destroys outstanding choices so same-page resend cannot reuse old nonce", async () => {
  const h = makeHarness();
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_term_a", 1_700_000_000_001), awaitingJob("job_term_b", 1_700_000_000_002), awaitingJob("job_term_c", 1_700_000_000_003)] };
  });
  await h.bridge.start();
  h.tabs.seed({ id: 110, url: "https://provider.example.edu/paper.pdf" });
  const offer = ((await h.bridge.startPDFDelivery({ tab_id: 110, url: "https://provider.example.edu/paper.pdf" }) as unknown as Record<string, unknown>)["choice"]) as { interaction: string; candidates: { job_id: string }[] };
  await (h.bridge as unknown as { removeJobWithOffer: (id:string)=>Promise<void> }).removeJobWithOffer("job_term_b");
  const reuse = await h.bridge.startPDFDelivery({ tab_id: 110, url: "https://provider.example.edu/paper.pdf", choice: { interaction: offer.interaction, job_id: "job_term_a" } }) as unknown as Record<string, unknown>;
  expect(reuse).toMatchObject({ ok: false, code: "choice_expired" });
});

test("ScienceDirect assets Send PDF arms the native viewer download (waiting_manual) via tab/DOI correlation", async () => {
  const store = { ...emptyStore(), activeJobs: [
    { job_id: "job_sciencedirect_native_viewer", tab_id: 42, offered_at: 1_700_000_000_000, expires_at: 1_800_000_000_000, status: "awaiting_download" as const, provider_hosts: ["sciencedirect.com", "pdf.sciencedirectassets.com"], adapter_id: "sciencedirect", access_mode: "delegated" as const },
  ] } as StoreShape;
  const h = makeHarness(store);
  const articleURL = "https://www.sciencedirect.com/science/article/pii/S0360131510000527";
  const viewerURL = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  h.tabs.seed({ id: 42, url: articleURL });
  await h.bridge.start();
  h.tabs.seed({ id: 99, url: viewerURL, openerTabId: 42 });
  await expect(h.bridge.startPDFDelivery({ tab_id: 99, url: viewerURL })).resolves.toMatchObject({ ok: true, state: "waiting_manual", job_id: "job_sciencedirect_native_viewer", message: expect.stringMatching(/viewer Download button/i) });
  expect(h.downloads.started).toEqual([]);
  expect(findByJob(h.backend.store, "job_sciencedirect_native_viewer")).toMatchObject({ tab_id: 99, status: "awaiting_download" });
  expect(h.backend.store.pendingDelivery).toMatchObject({ job_id: "job_sciencedirect_native_viewer", status: "waiting_manual" });
  const suggestions: string[] = [];
  await h.downloads.onDeterminingFilename.emit({ id: 779, tabId: 99, url: viewerURL, filename: "1-s2.0-S0360131510000527-main.pdf", state: "in_progress" }, (s)=>suggestions.push(s.filename));
  expect(suggestions).toEqual(["papio/job_sciencedirect_native_viewer/1-s2.0-S0360131510000527-main.pdf"]);
});


test("Firefox refuses a single-use PDF link instead of promising to file it", async () => {
  // Firefox has no onDeterminingFilename, so papio cannot steer or adopt a
  // download it did not start. The refusal used to say "use the viewer Download
  // button — papio will file that download for you", which the platform cannot
  // do: the researcher would download the paper and papio would never see it.
  const h = makeHarness(undefined, { firefox: true });
  await ((h.bridge as unknown as { update: (fn:(s:unknown)=>unknown)=>Promise<void>}).update)((st: unknown)=>{
    const s2 = st as StoreShape;
    return { ...s2, activeJobs: [awaitingJob("job_ff_a", 1_700_000_000_001)] };
  });
  await h.bridge.start();
  const viewerURL = "https://pdf.sciencedirectassets.com/271849/1-s2.0-S0360131510X00057/1-s2.0-S0360131510000527/main.pdf";
  h.tabs.seed({ id: 100, url: viewerURL });
  const reply = await h.bridge.startPDFDelivery({ tab_id: 100, url: viewerURL });
  expect(reply.ok).toBe(false);
  const message = reply.ok ? "" : reply.error.message;
  expect(message).toMatch(/Chrome/);
  expect(message).not.toMatch(/papio will file/i);
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
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: [],
      },
    ],
  });
  h.tabs.seed({ id: 100, url: pdfURL });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  const reply = await h.bridge.startPDFDelivery({ tab_id: 100, url: pdfURL });
  expect(reply.ok).toBe(true);
  const downloadID = 900 + h.downloads.started.length;

  // The tab stays interactive for the whole download. Re-reading it at
  // completion attached institutional provenance to whatever the operator
  // navigated to instead of the page that produced the bytes.
  h.tabs.seed({ id: 100, url: "https://unrelated.example.org/elsewhere" });
  h.downloads.items.set(downloadID, {
    id: downloadID,
    tabId: 100,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 1000,
    state: "complete",
  });
  await h.downloads.onChanged.emit({
    id: downloadID,
    state: { current: "complete" },
  });

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
  });
  h.tabs.seed({ id: 100, url: pdfURL });
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
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  h.downloads.items.set(downloadID, {
    id: downloadID,
    tabId: 100,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 1000,
    state: "complete",
  });
  await h.downloads.onChanged.emit({
    id: downloadID,
    state: { current: "complete" },
  });

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
  });
  h.tabs.seed({ id: 100, url: deliveryURL });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  // Start a delivery — freezes page_host: "provider.example.edu".
  const reply = await h.bridge.startPDFDelivery({
    tab_id: 100,
    url: deliveryURL,
  });
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
  await h.downloads.onChanged.emit({
    id: deliveryDownloadID,
    state: { current: "complete" },
  });

  // Now simulate a resolver-routed download for the same job from a different host.
  // This is a non-delivery download (track.delivery !== true), so the frozen host
  // must NOT be applied.
  const secondDownloadID = 900 + h.downloads.started.length;
  // Set up the tab with the second host URL.
  h.tabs.seed({ id: 100, url: secondURL });
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
  await h.downloads.onChanged.emit({
    id: secondDownloadID,
    state: { current: "complete" },
  });

  // The delivery_context frame should report the SECOND host, not the stale frozen one.
  const context = h.frames().find((frame) => frame.type === "delivery_context");
  expect(context?.payload).toMatchObject({ page_host: secondHost });
});

test("successful adoption removes the job while leaving its managed handoff open", async () => {
  const success = makeHarness();
  await success.bridge.start();

  await success.port.inbound(jobOffer("job_close_success"));
  const successTab = success.backend.store.activeJobs[0]?.tab_id ?? -1;
  const successTabInfo = success.tabs.snapshot(successTab);
  (
    success.bridge as unknown as { tabLedgerCache: Record<string, unknown> }
  ).tabLedgerCache = {
    [String(successTab)]: fakeBirthRecord({ tab_hint: successTab }),
  };
  await success.tabs.userActivate(successTab);
  await success.downloads.onCreated.emit({
    id: 901,
    tabId: successTab,
    state: "in_progress",
  });
  success.downloads.items.set(901, {
    id: 901,
    tabId: successTab,
    filename: "/tmp/paper.pdf",
    mime: "application/pdf",
    fileSize: 100,
    state: "complete",
  });
  await success.downloads.onChanged.emit({
    id: 901,
    state: { current: "complete" },
  });
  await success.port.inbound({
    protocol: "papio-browser/1",
    type: "ack",
    msg_id: "ack_close_success",
    job_id: "job_close_success",
    seq: 1,
    payload: {},
  });
  expect(success.tabs.removed).toEqual([]);
  expect(success.tabs.snapshot(successTab) !== undefined).toBe(true);
  expect(
    success.backend.store.activeJobs.find(
      (job) => job.job_id === "job_close_success",
    ),
  ).toBeUndefined();
  const successInternal = success.bridge as unknown as {
    handoffDrives: Map<string, unknown>;
  };
  expect(successInternal.handoffDrives.has("job_close_success")).toBe(false);

  const human = makeHarness();
  await human.bridge.start();
  await human.port.inbound(jobOffer("job_close_human"));
  const humanTab = human.backend.store.activeJobs[0]?.tab_id ?? -1;
  await human.tabs.completeNavigation(
    humanTab,
    "https://idp.example.edu/login",
  );
  expect(human.tabs.removed).toEqual([]);
  expect(human.tabs.snapshot(humanTab) !== undefined).toBe(true);
});
test("cold handoffs keep one tab until warm evidence releases the queue", async () => {
  const h = makeHarness();
  await h.bridge.start();
  const jobIDs = Array.from(
    { length: 5 },
    (_, index) => `job_cold_governor_${index}`,
  );
  for (const jobID of jobIDs) await h.port.inbound(jobOffer(jobID));

  expect(h.tabs.list().length).toBe(1);
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.tab_id < 0),
  ).toHaveLength(4);

  // Evidence is scoped and release-grade, but cannot exceed the single
  // effectful drive while the existing tab is still active.
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );
  expect(h.tabs.list().length).toBe(1);
  expect(h.tabs.created).toHaveLength(1);
  expect(
    h.backend.store.activeJobs.filter((job) => job.tab_id < 0),
  ).toHaveLength(4);
});
test("keepalive warmth releases only the matching resolver queue", async () => {
  const h = makeHarness();
  const collegeOrigin = "https://onesearch.library.example-college.edu";
  const collegeOpenURL = `${collegeOrigin}/openurl?ctx=college`;
  const defaultOffer = jobOfferForHosts("job_default_origin_queue", [
    PROVIDER_HOST,
  ]) as {
    payload: Record<string, unknown>;
  };
  defaultOffer.payload["requires_auth"] = true;
  const uwaOffer = jobOfferForHosts(
    "job_uwa_origin_queue",
    ["link.springer.com"],
    collegeOpenURL,
  ) as {
    payload: Record<string, unknown>;
  };
  uwaOffer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(defaultOffer);
  await h.port.inbound(uwaOffer);
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, collegeOrigin));

  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_default_origin_queue",
    ),
  ).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_uwa_origin_queue",
    ),
  ).toMatchObject({
    status: "accepted",
    tab_id: 100,
  });
  expect(h.tabs.created).toEqual([{ url: collegeOpenURL, active: false }]);
});

test("handoff group reducer trails auth recollapse and stays quiet when collapsed", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_group_debounce"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  const idpURL = "https://idp.example.edu/login";
  const providerURL = `https://${PROVIDER_HOST}/stable/returned`;
  await h.tabs.completeNavigation(tabID, idpURL);
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(false);
  await h.tabs.completeNavigation(tabID, providerURL);
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(false);
  const collapse = h.timers.find((timer) => timer.ms === 5_000);
  expect(collapse).toBeDefined();
  h.clock.now += 5_000;
  await collapse?.fn();
  expect(h.tabGroups?.live.get(groupID!)?.collapsed).toBe(true);
  const updates = h.tabGroups?.updated.length ?? 0;
  await h.tabs.completeNavigation(tabID, providerURL);
  expect(h.tabGroups?.updated.length).toBe(updates);
});

test("papio classifies its own surfaces and asks only about strays without closing tabs", async () => {
  const h = makeHarness(undefined, { tabGroups: true });
  let ledger: Record<string, unknown> = {
    "300": { openedAt: 1, url: "https://provider.example.org/a", jobID: "job_other_a" },
    "301": { openedAt: 1, url: "https://provider.example.org/b", jobID: "job_other_b" },
    "304": {
      openedAt: 1,
      url: "https://provider.example.org/d",
      groupId: 700,
      windowId: 1,
      jobID: "job_other_d",
    },
    "999": {
      openedAt: 1,
      url: "https://provider.example.org/missing",
      jobID: "job_other_missing",
    },
  };
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
  h.tabs.seed({ id: 300, url: "https://provider.example.org/a" });
  // 301: ledgered but the user is looking at it — never a candidate.
  h.tabs.seed({ id: 301, url: "https://provider.example.org/b", active: true });
  // 302: an unledgered tab in a papio-titled group — papio never created it.
  h.tabs.seed({ id: 302, url: "https://provider.example.org/c", groupId: 700 });
  // 304: ledgered AND still in papio's group — papio leaves it open for review.
  h.tabs.seed({ id: 304, url: "https://provider.example.org/d", groupId: 700 });
  h.tabGroups!.live.set(700, {
    id: 700,
    collapsed: true,
    title: "papio",
    windowId: 1,
  });
  // 303: the pinned keepalive resolver tab folded into the papio group —
  // papio's own session anchor, never an orphan.
  h.tabs.seed({
    id: 303,
    url: "https://resolver.example.edu/keepalive",
    groupId: 700,
    pinned: true,
  });

  // The popup card offers ONLY the ambiguous stray, not the group member.
  const status = await h.bridge.orphanTabStatus();
  expect(status).toEqual({ count: 1, tab_ids: [300] });
  // The dead entry is pruned from the durable ledger as a scan side effect.
  expect(ledger["999"]).toBeUndefined();

  // Startup reconciliation classifies the owned-surface leftover but leaves it open.
  const { closed: reconciled } = await h.bridge.reconcileOwnedTabs();
  expect(reconciled).toBe(0);
  expect(h.tabs.removed).toEqual([]);
  expect(ledger["304"]).toBeDefined();

  const cleanup = await h.bridge.cleanupOrphanTabs();
  expect(cleanup).toEqual({ closed: 0, focused: 1 });
  expect(h.tabs.removed).toEqual([]);
  expect(h.tabs.activated).toContain(300);
  expect(h.tabs.snapshot(304) !== undefined).toBe(true);
  expect(h.tabs.snapshot(302) !== undefined).toBe(true);
  expect(h.tabs.snapshot(301) !== undefined).toBe(true);
  expect(h.tabs.snapshot(trackedTab) !== undefined).toBe(true);
  expect(ledger["300"]).toBeDefined();
  // No lifecycle path closes an orphan; cancellation remains a job transition.
  expect(h.frames().filter((frame) => frame.type === "cancel")).toHaveLength(0);
});

test("startup reconciliation retires a modern same-epoch inactive orphan through job_inactive", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: "job_already_removed_0001",
  };

  const reconciling = h.bridge.reconcileOwnedTabs();
  const request = await h.port.waitForFrame("surface_close_request");
  expect(request.payload).toMatchObject({
    binding_id: bindingID,
    disposition: "job_inactive",
  });
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-reconcile-inactive",
      nonce: "nonce-reconcile-inactive",
      browser_holder_generation: 1,
    }),
  );
  await expect(reconciling).resolves.toEqual({ closed: 1 });
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
});

test("startup reconciliation retains an active orphan rather than ceding or closing it", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: "job_active_orphan_0001",
  };
  h.tabs.patch(tabID, { active: true });

  await expect(h.bridge.reconcileOwnedTabs()).resolves.toEqual({ closed: 0 });
  expect(h.tabs.removed).not.toContain(tabID);
  expect(
    h.frames().filter((frame) => frame.type === "surface_close_request"),
  ).toHaveLength(0);
  // Retained, NOT ceded: papio focuses its own surfaces, so an active tab of
  // unknown cause is ambiguous and ambiguity retains. Ceding here made every
  // tab papio opened permanently unretirable.
  expect(internals.tabLedgerCache[String(tabID)]?.ceded).toBeUndefined();
});

// The retry that makes retaining safe: a surface retained because it was the
// foreground tab retires the moment it stops being one. Without it, "retain
// instead of cede" would simply postpone the litter forever, since nothing
// else revisits an owned tab between worker starts.
test("an owned surface papio focused retires when the operator moves to another tab", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    focusOwnedTab: (tabID: number) => Promise<void>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: "job_focused_then_left_0001",
  };
  await internals.focusOwnedTab(tabID);
  expect(internals.tabLedgerCache[String(tabID)]?.ceded).toBeUndefined();

  const otherTabID = await h.tabs.create({
    url: "https://example.edu/reading",
    active: false,
  });
  const framesBefore = h.port.posted.length;
  const switching = h.tabs.userActivate(otherTabID.id ?? -1);
  const request = await h.port.waitForFrame("surface_close_request", framesBefore);
  expect(request.payload).toMatchObject({
    binding_id: bindingID,
    disposition: "job_inactive",
  });
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-deactivated",
      nonce: "nonce-deactivated",
      browser_holder_generation: 1,
    }),
  );
  await switching;
  expect(h.tabs.removed).toContain(tabID);
});

test("created broker tabs are ledgered durably and forgotten once they close", async () => {
  const h = makeHarness();
  let ledger: Record<string, unknown> = {};
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

  await h.tabs.userClose(tabID);
  expect(ledger[String(tabID)]).toBeUndefined();
});
test("review never focuses a stale ledger id collision; the unproven entry is retained", async () => {
  const h = makeHarness();
  let ledger: Record<string, unknown> = {
    "300": { openedAt: 1, url: "https://papio.example.edu/old" },
  };
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  h.tabs.seed({ id: 300, url: "https://foreign.example.edu/current" });
  await h.bridge.start();
  // By URL alone a navigated papio tab and a recycled tab id naming a
  // foreign tab are indistinguishable, so the entry authorizes no action —
  // but it is NOT deleted: ordinary redirects must not erase ownership
  // evidence (surface-lifecycle plan, Slice 0; identity proof is Slice 2).
  expect(await h.bridge.cleanupOrphanTabs()).toEqual({ closed: 0, focused: 0 });
  expect(h.tabs.activated).not.toContain(300);
  expect(ledger["300"]).toBeDefined();
});

// Commit B: the browser tells papio when a resolver page changed (so the
test("close gate leaves an active managed tab open", async () => {
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_close_gate_active"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = h.tabs.snapshot(tabID);
  if (tab !== undefined) {
    tab.active = true;
    (
      h.bridge as unknown as { tabLedgerCache: Record<string, unknown> }
    ).tabLedgerCache = { [String(tabID)]: fakeBirthRecord({ tab_hint: tabID }) };
  }
  expect(tabID).toBeGreaterThanOrEqual(0);
  await h.tabs.userActivate(tabID);
  await h.bridge.requestCancel("job_close_gate_active");
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});
test("cancellation removes an inactive ledgered scaffold tab", async () => {
  const jobID = "job_close_positive";
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    update: (fn: (s: StoreShape) => StoreShape) => Promise<void>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: jobID,
  };
  await internals.update((store) => ({
    ...store,
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: tabID }],
  }));

  await h.bridge.requestCancel(jobID);
  const request = await h.port.waitForFrame("surface_close_request");
  expect(request.payload["disposition"]).toBe("job_inactive");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-cancel-inactive",
      nonce: "nonce-cancel-inactive",
      browser_holder_generation: 1,
    }),
  );
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(false);
});

test("cancellation leaves current PDF content in papio's surface", async () => {
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_current_pdf_content"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = h.tabs.snapshot(tabID);
  if (tab !== undefined) {
    h.tabs.patch(tabID, {
      active: false,
      url: "https://provider.example.edu/paper.pdf",
    });
  }
  (
    h.bridge as unknown as { tabLedgerCache: Record<string, unknown> }
  ).tabLedgerCache = {
    [String(tabID)]: fakeBirthRecord({ tab_hint: tabID }),
  };
  await h.bridge.requestCancel("job_current_pdf_content");
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});
test("cancellation leaves a scaffold tab dragged out of papio's surface", async () => {
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  let ledger: Record<string, unknown> = {};
  h.deps.tabLedger = {
    load: async () => ({ ...ledger }),
    save: async (entries) => {
      ledger = { ...entries };
    },
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_close_dragged"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  const tab = h.tabs.snapshot(tabID);
  if (tab !== undefined)
    h.tabs.patch(tabID, { active: false, windowId: 999, groupId: undefined });
  await h.bridge.requestCancel("job_close_dragged");
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});

test("cancellation refuses an un-ledgered inactive tab", async () => {
  const h = makeHarness(undefined, {
    windows: true,
    handoffSurface: "work-window",
  });
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_unledgered_refusal"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  h.tabs.patch(tabID, { active: false });
  (
    h.bridge as unknown as { tabLedgerCache: Record<string, unknown> }
  ).tabLedgerCache = {};
  await h.bridge.requestCancel("job_unledgered_refusal");
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});
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
    noteResolverActivated: () => {},
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();

  const libraryTabID = 555;
  const libraryURL =
    "https://onesearch.library.example-college.edu/discovery/search";
  h.tabs.seed({ id: libraryTabID, url: libraryURL });
  await h.tabs.completeNavigation(libraryTabID, libraryURL);

  expect(navigations).toEqual([[libraryTabID, libraryURL]]);
});

test("a navigation on a tracked handoff tab also reaches noteResolverNavigation without losing existing tracked-job handling", async () => {
  const h = makeHarness();
  const navigations: [number, string | undefined][] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tracked_nav"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  await h.tabs.completeNavigation(tabID, idpURL);

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
    noteResolverActivated: () => {},
    noteResolverNavigation: (tabID: number, rawURL: string | undefined) => {
      navigations.push([tabID, rawURL]);
    },
  } as unknown as KeepaliveManager);

  void h.bridge.start();
  const wakeURL =
    "https://onesearch.library.example-college.edu/discovery/search";
  h.tabs.seed({ id: 501, url: wakeURL });
  void h.tabs.userNavigate(501, wakeURL);

  // No await/microtask has elapsed since start() and emit() were called: if
  // the call was already recorded, it happened synchronously, ahead of the
  // `await this.ready` that still gates the rest of onTabUpdated.
  expect(navigations).toEqual([[501, wakeURL]]);
});

test("chrome.tabs.onActivated routes to noteResolverActivated with the activated tab's URL", async () => {
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
  h.tabs.seed({ id: tabID, url });
  await h.tabs.userActivate(tabID);

  expect(activations).toEqual([[tabID, url]]);
});

test("tab removal routes to noteTabRemoved and still cancels the job as before", async () => {
  const h = makeHarness();
  const removed: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
    noteTabRemoved: (tabID: number) => {
      removed.push(tabID);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_tab_removed"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await h.tabs.userClose(tabID);

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
    noteResolverActivated: () => {},
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

/** Answer the two correlated requests a keepalive wake issues, in the order it
 * issues them. Two harness facts make the obvious version deadlock:
 *
 * `waitForFrame` replays from the start, so `offset` MUST be the posted-frame
 * count from before the wake — otherwise this answers a stale request id from
 * hello and the wake's own request is never replied to at all.
 *
 * `inbound` resolves only when the whole inbound chain for that frame has run,
 * and the triage reply's continuation issues the pulse request and waits for ITS
 * reply. So awaiting the triage delivery before answering the pulse blocks the
 * answer it is waiting for. Deliver, do not await, then settle at the end. */
async function respondToKeepaliveRefresh(
  port: FakePort,
  offset: number,
): Promise<void> {
  const triage = await port.waitForFrame("triage_counts_request", offset);
  const triageDelivered = port.inbound(
    nativeResult("triage_counts_response", {
      request_id: triage.payload["request_id"],
      counts: triageCounts(0),
    }),
  );
  const pulse = await port.waitForFrame("work_pulse_request", offset);
  await port.inbound(
    nativeResult("work_pulse_response", {
      request_id: pulse.payload["request_id"],
      schema: 1,
      generated_at: "2027-01-01T00:00:00Z",
      nonterminal_total: 0,
      projection_complete: true,
      in_flight: 0,
      continuing: 0,
      scheduled: 0,
      waiting_required: 0,
      stalled: 0,
    }),
  );
  await triageDelivered;
}

test("duplicate keepalive alarm delivery issues one work pulse request per interval", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1", "work_pulse_v1"],
    }),
  );

  // Both deliveries are in flight before either settles, which is the shape
  // Chrome produced when start() re-created the persisted alarm on every worker
  // spin-up: two wake callbacks in the same second.
  const postedBeforeWake = h.port.posted.length;
  const duplicateWake = Promise.all([
    h.alarms.onAlarm.emit({ name: "papio-keepalive" }),
    h.alarms.onAlarm.emit({ name: "papio-keepalive" }),
  ]);
  await respondToKeepaliveRefresh(h.port, postedBeforeWake);
  await duplicateWake;

  expect(h.frames().filter((frame) => frame.type === "work_pulse_request")).toHaveLength(
    1,
  );

  // Past the dedupe window the next wake is a real interval, not a duplicate.
  h.clock.now += 55_001;
  const postedBeforeSecondWake = h.port.posted.length;
  // The wake cannot settle until its own requests are answered, so start it,
  // answer, then settle it.
  const secondWake = h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  await respondToKeepaliveRefresh(h.port, postedBeforeSecondWake);
  await secondWake;
  expect(h.frames().filter((frame) => frame.type === "work_pulse_request")).toHaveLength(
    2,
  );
});

test("worker restart does not re-create an existing keepalive alarm", async () => {
  const h = makeHarness();
  await h.bridge.start();
  expect(h.alarms.created.filter((alarm) => alarm.name === "papio-keepalive")).toHaveLength(
    1,
  );
  await h.bridge.start();
  expect(h.alarms.created.filter((alarm) => alarm.name === "papio-keepalive")).toHaveLength(
    1,
  );
});

test("fresh worker generation issues keepalive work pulse inside dedupe window", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1", "work_pulse_v1"],
    }),
  );

  const postedBeforeFirstWake = h.port.posted.length;
  const firstWake = h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  await respondToKeepaliveRefresh(h.port, postedBeforeFirstWake);
  await firstWake;
  expect(h.frames().filter((frame) => frame.type === "work_pulse_request")).toHaveLength(
    1,
  );

  const reloaded = new Bridge(h.deps);
  await reloaded.start();
  const restartedPort = h.ports[h.ports.length - 1]!;
  await restartedPort.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["triage_snapshot_v1", "work_pulse_v1"],
    }),
  );

  // Same alarm, inside the previous generation's 55s dedupe window. The dedupe
  // state lives on the Bridge instance, so worker death clears it: a fresh
  // generation must still get its pulse, or MV3's ~30s idle kill would silence
  // the surface the dedupe exists to protect.
  const postedBeforeReload = restartedPort.posted.length;
  const reloadWake = h.alarms.onAlarm.emit({ name: "papio-keepalive" });
  await respondToKeepaliveRefresh(restartedPort, postedBeforeReload);
  await reloadWake;
  expect(
    restartedPort.posted
      .map(parseBrowserMessage)
      .filter((frame) => frame.type === "work_pulse_request"),
  ).toHaveLength(1);
  void reloaded;
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
  const offerA = jobOffer("job_evidence_scope_a") as {
    payload: Record<string, unknown>;
  };
  offerA.payload["requires_auth"] = true;
  const offerB = jobOffer("job_evidence_scope_b", bOpenURL) as {
    payload: Record<string, unknown>;
  };
  offerB.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(offerA);
  await h.port.inbound(offerB);
  expect(
    h.backend.store.activeJobs.filter((job) => job.status === "queued"),
  ).toHaveLength(2);

  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_evidence_scope_a",
    ),
  ).toMatchObject({
    status: "accepted",
  });
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_evidence_scope_b",
    ),
  ).toMatchObject({
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
  const offer = jobOffer("job_evidence_expiry") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );
  h.clock.now += 30 * 60_000 + 1;

  await h.port.inbound(offer);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_evidence_expiry",
    ),
  ).toMatchObject({
    status: "queued",
  });
});

test("a committed sign-out revokes that origin's release authority immediately", async () => {
  // onOriginAuthenticationChanged was wired to a no-op, so nothing could ever
  // retract evidence: papio kept opening queued handoffs into a session the
  // operator had signed out of, until the TTL or the worker expired.
  const h = makeHarness();
  const origin = "https://resolver.example.edu";
  const offer = jobOffer("job_evidence_revoked") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;

  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, origin));
  await h.bridge.revokeAuthEvidence(origin);

  await h.port.inbound(offer);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_evidence_revoked",
    ),
  ).toMatchObject({
    status: "queued",
  });
  expect(h.backend.store.authEvidenceByOrigin?.[origin]).toBeUndefined();

  // Signing back in re-grants it, so revocation is not a one-way latch.
  await h.bridge.recordFreshSessionEvidence(freshEvidence(h, origin));
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_evidence_revoked",
    ),
  ).toMatchObject({
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
  const institutionalA = jobOffer("job_leak_guard_a") as {
    payload: Record<string, unknown>;
  };
  institutionalA.payload["requires_auth"] = true;
  const institutionalB = jobOffer("job_leak_guard_b", bOpenURL) as {
    payload: Record<string, unknown>;
  };
  institutionalB.payload["requires_auth"] = true;
  const openAccessURL = "https://oa.example.edu/article/leak-guard";
  const openAccess = jobOffer("job_leak_guard_oa", openAccessURL) as {
    payload: Record<string, unknown>;
  };
  openAccess.payload["requires_auth"] = false;
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: [AUTH_CLAIM] }));
  await h.port.inbound(institutionalA);
  await h.port.inbound(institutionalB);
  await h.port.inbound(openAccess);
  // The open-access owner must settle before resolver evidence can admit A;
  // otherwise a second handoff would overlap the sole governor effect.
  const openAccessJob = h.backend.store.activeJobs.find(
    (job) => job.job_id === "job_leak_guard_oa",
  );
  if (openAccessJob?.tab_id !== undefined && openAccessJob.tab_id >= 0) {
    await h.bridge.parkHandoffForManual(openAccessJob.job_id);
  }

  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_a"),
  ).toMatchObject({
    status: "accepted",
  });
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_b"),
  ).toMatchObject({
    status: "queued",
  });

  const openAccessTabID =
    h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_oa")
      ?.tab_id ?? -1;
  const providerURL = `https://${PROVIDER_HOST}/stable/leak-guard`;
  await h.tabs.completeNavigation(openAccessTabID, providerURL);

  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === "job_leak_guard_b"),
  ).toMatchObject({
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

  const evidenceFrames = h
    .frames()
    .filter((frame) => frame.type === "session_evidence");
  expect(evidenceFrames.map((frame) => frame.payload)).toEqual([
    {
      evidence: "warm_verified",
      origin_hint: originA,
      at: new Date(h.clock.now).toISOString(),
    },
    {
      evidence: "warm_verified",
      origin_hint: originB,
      at: new Date(h.clock.now).toISOString(),
    },
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
      {
        job_id: jobA,
        tab_id: 100,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [],
      },
      {
        job_id: jobB,
        tab_id: 101,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [],
      },
    ],
  });
  h.tabs.seed({ id: 100, url: "https://provider.example.edu/article/a.pdf" });
  h.tabs.seed({ id: 101, url: "https://provider.example.edu/article/b.pdf" });
  await h.bridge.start();
  seedOfferURLs(h, {
    [jobA]: `${originA}/openurl?ctx=a`,
    [jobB]: `${originB}/openurl?ctx=b`,
  });
  await h.port.inbound(helloAck({ features: ["delivery_context_v1"] }));

  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, originA, "live_tab"),
  );

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

  const contexts = h
    .frames()
    .filter((frame) => frame.type === "delivery_context");
  expect(
    contexts.find((frame) => frame.job_id === jobA)?.payload,
  ).toMatchObject({ session_evidence: "fresh_auth" });
  expect(
    contexts.find((frame) => frame.job_id === jobB)?.payload,
  ).toMatchObject({ session_evidence: "none" });
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
  });
  h.tabs.seed({ id: landingTabID, url: OPENURL });
  const dirtied: string[] = [];
  const probedForeground: (string | undefined)[] = [];
  const probedAutomatically: string[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
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
    noteTabRemoved: () => {},
    onWake: async () => {},
  } as unknown as KeepaliveManager);

  await h.bridge.start();
  seedOfferURLs(h, {
    [landingJobID]: OPENURL,
    [peerJobID]: OPENURL,
    [otherOriginJobID]: otherOpenURL,
  });
  await h.port.inbound(
    helloAck({
      features: [AUTH_CLAIM],
      resolver_origins: ["https://resolver.example.edu", otherOrigin],
    }),
  );

  await h.tabs.completeNavigation(landingTabID, OPENURL);

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
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === peerJobID),
  ).toMatchObject({
    status: "accepted",
  });
  expect(h.tabs.created).toEqual([{ url: OPENURL, active: false }]);

  // A different institution's queued handoff is never touched by this.
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === otherOriginJobID),
  ).toMatchObject({
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
      {
        job_id: jobA,
        tab_id: tabA,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
        access_mode: "delegated",
      },
      {
        job_id: jobB,
        tab_id: tabB,
        offered_at: 1,
        expires_at: 2,
        status: "accepted",
        provider_hosts: ["link.springer.com"],
        access_mode: "delegated",
      },
    ],
  });
  h.tabs.seed({ id: tabA, url: idpA });
  h.tabs.seed({ id: tabB, url: idpB });
  await h.bridge.start();
  seedOfferURLs(h, {
    [jobA]: `${originA}/openurl?ctx=a`,
    [jobB]: `${originB}/openurl?ctx=b`,
  });

  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, originA, "live_tab"),
  );

  expect(h.tabs.reloaded).toEqual([tabA]);
});

test("requestSessionSignIn rejects a non-configured origin once hello_ack has landed, and accepts a configured one", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ resolver_origins: ["https://resolver.example.edu"] }),
  );

  await expect(
    h.bridge.requestSessionSignIn("https://unknown.example-college.edu"),
  ).resolves.toEqual({
    ok: false,
    error: {
      code: "resolver_unavailable",
      message: "This institution is not currently configured",
    },
  });
  expect(h.tabs.created).toEqual([]);

  const reply = await h.bridge.requestSessionSignIn(
    "https://resolver.example.edu",
  );
  expect(reply).toEqual({ ok: true, opened: true });
  expect(h.tabs.created).toEqual([
    { url: "https://resolver.example.edu", active: true },
  ]);
});

test("the 45-second forced-release fallback still opens a queued handoff with zero authentication evidence", async () => {
  // ADR-0009's autonomous-retry fallback is ratified and deliberately bypasses
  // evidence for exactly one forced job; it still waits for the sole slot.
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_forced_release_active"));
  await h.port.inbound(jobOffer("job_forced_release_queued"));

  const fallback = h.timers.find((timer) => timer.ms === 45_000);
  expect(fallback).toBeDefined();
  const internals = h.bridge as unknown as {
    releaseHandoffDrive(jobID: string): void;
  };
  internals.releaseHandoffDrive("job_forced_release_active");
  await fallback?.fn();

  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_forced_release_queued",
    ),
  ).toMatchObject({
    status: "accepted",
  });
  expect(h.tabs.created).toEqual([
    { url: OPENURL, active: true },
    { url: OPENURL, active: false },
  ]);
});

test("an explicit queued open keeps its exact forced job ahead of unrelated queued work", async () => {
  const h = makeHarness();
  h.backend.store.challengeCooldowns = {
    "www.jstor.org": h.clock.now + 600_000,
  };
  await h.bridge.start();
  await h.port.inbound(
    jobOfferForHosts("job_forced_priority_active_a", ["nature.com"]),
  );

  const jobA = "job_forced_priority_a";
  const jobB = "job_forced_priority_b";
  const jobC = "job_forced_priority_c";
  const originA = "https://resolver-a.example.edu/openurl?a";
  const originB = "https://resolver-b.example.edu/openurl?b";
  const originC = "https://resolver-c.example.edu/openurl?c";
  await h.port.inbound(jobOfferForHosts(jobA, ["www.jstor.org"], originA));
  await h.port.inbound(jobOfferForHosts(jobB, ["link.springer.com"], originB));
  await h.port.inbound(jobOfferForHosts(jobC, ["sciencedirect.com"], originC));
  await h.bridge.requestCancel("job_forced_priority_active_a");
  // Establish A/B/C as queued work before any evidence makes B eligible.
  // The active owner is settled explicitly so the forced request is the only
  // candidate considered by the subsequent release.
  // The owner is settled; clear only the temporary provider cooldown while
  // retaining A's queued marker for the explicit exact-job request.
  const bridgeStore = (h.bridge as unknown as { store: StoreShape }).store;
  bridgeStore.challengeCooldowns = {};
  h.backend.store.challengeCooldowns = {};
  // Explicit force may bypass that temporary block, but it must never
  // substitute B or C.
  await expect(h.bridge.openHandoff(jobA)).resolves.toEqual({
    ok: true,
    opened: true,
  });
  expect(h.tabs.created.map((tab) => tab.url)).toEqual([OPENURL, originA]);
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobA),
  ).toMatchObject({
    status: "accepted",
    tab_id: expect.any(Number),
  });
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobB),
  ).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobC),
  ).toMatchObject({
    status: "queued",
    tab_id: -1,
  });
});

test("a hello_ack immediately notifies the keepalive manager of configured-origin membership", async () => {
  const h = makeHarness();
  const notifications: number[] = [];
  h.bridge.attachKeepalive({
    getSnapshot: () => ({ pausedForReauth: false }),
    noteResolverActivated: () => {},
    notifyConfiguredOriginsChanged: () => {
      notifications.push(notifications.length);
    },
  } as unknown as KeepaliveManager);
  await h.bridge.start();
  expect(notifications).toEqual([]);

  await h.port.inbound(
    helloAck({ resolver_origins: ["https://resolver.example.edu"] }),
  );

  expect(notifications).toEqual([0]);
});

test("a challenge-parked handoff stays parked through fresh session evidence for its own institution (scope guard)", async () => {
  let challenge = true;
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  useUnknownProviderClassifier(h, () => challenge);
  const tabID = await classifyProviderUnknown(h, "job_challenge_evidence");

  expect(h.backend.store.activeJobs[0]).toMatchObject({
    challenge_blocked: true,
    parked_with_tab: true,
  });
  expect(h.backend.store.activeJobs[0]?.waiting_for_session).toBeUndefined();
  h.tabs.navigations.splice(0);

  await h.bridge.recordFreshSessionEvidence(
    freshEvidence(h, "https://resolver.example.edu"),
  );

  const job = h.backend.store.activeJobs.find(
    (j) => j.job_id === "job_challenge_evidence",
  );
  expect(job?.challenge_blocked).toBe(true);
  expect(job?.parked_with_tab).toBe(true);
  expect(job?.waiting_for_session).toBeUndefined();
  expect(h.tabs.navigations).toEqual([]);
  expect(h.tabs.snapshot(tabID) !== undefined).toBe(true);
});

test("papio.pageBulk.grabStatus pulls a structured durable result", async () => {
  const h = makeHarness();
  const urls = pageBulkTestURLs;
  const sender = { id: urls.runtimeID, url: urls.pageBulkURL, tab: { id: 42 } };
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      daemon_version: CURRENT_DAEMON,
      features: ["pdf_grab_v1", "effect_permit_v1"],
    }),
  );
  const replyPromise: Promise<unknown> = handleInboxRuntimeMessage(
    h.bridge,
    {
      type: "papio.pageBulk.grabStatus",
      request: { grab_id: "grab-status-0001" },
    },
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
  const source = readFileSync(
    new URL("../src/background.ts", import.meta.url),
    "utf8",
  );
  const handlerStart = source.indexOf(
    "export async function handleInboxRuntimeMessage",
  );
  const dispatcherStart = source.indexOf(
    "chrome.runtime.onMessage.addListener",
    handlerStart,
  );
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

async function pageCapturePayload(): Promise<PageCapturePayload> {
  const encoded = await encodePageCapture(
    sanitizeFixture("<html><body>provider failure</body></html>", {
      provider: "example",
      scenario: "observed",
      originNoQuery: "https://provider.example/failure",
      capturedISO: "2026-01-01T00:00:00.000Z",
    }),
    { host: "provider.example", scenario: "observed" },
  );
  if (!encoded.ok) throw new Error(encoded.error);
  return encoded.payload;
}

test("Firefox 128 suppresses page_capture transmission until consent", async () => {
  const h = makeHarness(undefined, { firefox: true, captureConsent: false });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  expect(await h.bridge.sendPageCapture(await pageCapturePayload())).toBe(
    false,
  );
  expect(
    h.frames().filter((frame) => frame.type === "page_capture"),
  ).toHaveLength(0);
});

test("Firefox 128 transmits page_capture after stored consent", async () => {
  const h = makeHarness(undefined, { firefox: true, captureConsent: true });
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  expect(await h.bridge.sendPageCapture(await pageCapturePayload())).toBe(true);
  expect(
    h.frames().filter((frame) => frame.type === "page_capture"),
  ).toHaveLength(1);
});

test("Chrome keeps page_capture transmission always on", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["page_capture_v1"] }));
  expect(await h.bridge.sendPageCapture(await pageCapturePayload())).toBe(true);
  expect(
    h.frames().filter((frame) => frame.type === "page_capture"),
  ).toHaveLength(1);
});
test("assisted offers block every papio-initiated download path", async () => {
  for (const method of ["click", "url", "meta", "href"] as const) {
    const h = makeHarness();
    const adapter: AdapterSpec = {
      id: `assisted-${method}`,
      version: "1.0.0",
      hosts: [PROVIDER_HOST],
      classify: [{ kind: "article", any: ["article"] }],
      download:
        method === "click"
          ? { selector: "button.download", requireKind: "article", method }
          : method === "url"
            ? {
                selector: "a.download",
                requireKind: "article",
                method,
                idPattern: "stable/([^/]+)",
                urlTemplate: "https://download.example/{1}.pdf",
              }
            : {
                selector: "a.download",
                requireKind: "article",
                method,
                ...(method === "meta" ? { metaName: "citation_pdf_url" } : {}),
              },
    };
    h.deps.adapterSpecs.push(adapter);
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) => {
      if (injection.func === assessDrivenPage)
        return [{ result: { kind: "normal" } }];
      if (injection.func === planExecution) {
        return plannerResult(injection, {
          kind: "article",
          adapter_id: adapter.id,
          adapter_version: adapter.version,
          evidence: [],
        });
      }
      return [
        {
          result:
            method === "click"
              ? true
              : `https://${PROVIDER_HOST}/download/paper.pdf`,
        },
      ];
    };
    await h.bridge.start();
    await h.port.inbound(
      jobOffer(`job_assisted_${method}`, OPENURL, "assisted"),
    );
    await expect(
      h.bridge.openHandoff(`job_assisted_${method}`),
    ).resolves.toEqual({ ok: true, opened: true });
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(
      tabID,
      `https://${PROVIDER_HOST}/stable/article`,
    );
    expect(h.downloads.started).toHaveLength(0);
    expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
  }
});

test("delegated article offers retain automatic download behavior", async () => {
  const h = makeHarness();
  const adapter: AdapterSpec = {
    id: "delegated-url",
    version: "1.0.0",
    hosts: [PROVIDER_HOST],
    classify: [{ kind: "article", any: ["article"] }],
    download: {
      selector: "a.download",
      requireKind: "article",
      method: "url",
      idPattern: "stable/([^/]+)",
      urlTemplate: "https://download.example/{1}.pdf",
    },
  };
  h.deps.adapterSpecs.push(adapter);
  h.deps.permissions.contains = async () => true;
  h.deps.scripting.executeScript = async (injection) => {
    if (injection.func === assessDrivenPage)
      return [{ result: { kind: "normal" } }];
    if (injection.func === planExecution) {
      return plannerResult(injection, {
        kind: "article",
        adapter_id: adapter.id,
        adapter_version: adapter.version,
        evidence: [],
      });
    }
    if (injection.func === executePlannedPageEffect)
      return plannedEffectResult(injection);
    return [{ result: "https://download.example/paper.pdf" }];
  };
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_delegated_url", OPENURL, "delegated"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.tabs.completeNavigation(
    tabID,
    `https://${PROVIDER_HOST}/stable/article`,
  );
  expect(h.downloads.started).toHaveLength(1);
});

test("assisted jobs still adopt a human-initiated download", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_assisted_human", OPENURL, "assisted"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  await h.downloads.onCreated.emit({
    id: 81,
    tabId: tabID,
    url: `https://${PROVIDER_HOST}/download/paper.pdf`,
    state: "in_progress",
  });
  h.downloads.items.set(81, {
    id: 81,
    tabId: tabID,
    filename: "/Users/x/Downloads/paper.pdf",
    fileSize: 91,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 81, state: { current: "complete" } });
  expect(
    h
      .frames()
      .some(
        (frame) =>
          frame.type === "download_complete" &&
          frame.job_id === "job_assisted_human",
      ),
  ).toBe(true);
});

test("DOI normalization strips presentation prefixes but preserves repeated slashes", () => {
  expect(normalizeExpectedDOI(" DOI: https://doi.org/10.1000//ABC ")).toBe(
    "10.1000//abc",
  );
  expect(normalizeExpectedDOI("https://doi.org/10.1000//ABC")).toBe(
    "10.1000//abc",
  );
  expect(normalizeExpectedDOI("10.1000/abc")).not.toBe(
    normalizeExpectedDOI("10.1000//abc"),
  );
});

describe("generic settled-unknown acquisition", () => {
  async function reachUnknown(
    h: Harness,
    jobID: string,
    accessMode: "assisted" | "delegated",
    planned: GenericPlan,
    expectedDOI = "10.1000/generic",
    driveEpoch = true,
  ): Promise<void> {
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) => {
      if (injection.func === planGeneric) return [{ result: planned }];
      return [];
    };
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => ({
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome:
          args[2] === "provider_drive_epoch_start_result"
            ? "started"
            : "applied",
      },
    }));
    const offer = jobOffer(jobID, OPENURL, accessMode) as {
      payload: Record<string, unknown>;
    };
    await h.bridge.start();
    await h.port.inbound(
      helloAck({
        features: ["provider_drive_epoch_v1", "effect_permit_v1"],
      }),
    );
    // requestNative above models the feature-negotiated daemon ACK.
    offer.payload["expected"] = { doi: expectedDOI };
    if (driveEpoch) {
      offer.payload["drive_attempt_id"] = "generic-test-epoch-1";
      offer.payload["drive_ordinal"] = 0;
      offer.payload["drive_strategy"] = "generic";
      offer.payload["drive_revision"] = "1";
    }
    await h.port.inbound(offer);
    if (accessMode === "assisted") {
      await expect(h.bridge.openHandoff(jobID)).resolves.toEqual({
        ok: true,
        opened: true,
      });
    }
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    h.clock.now += 6_000;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
  }
  async function genericStartOutcome(
    h: Harness,
    jobID: string,
    outcome:
      | "started"
      | "duplicate"
      | "stale"
      | "error"
      | readonly ("started" | "duplicate" | "stale" | "error")[],
  ): Promise<unknown[][]> {
    const requests: unknown[][] = [];
    const outcomes = Array.isArray(outcome) ? [...outcome] : [outcome];
    const candidate: GenericCandidate = {
      strategy_id: "generic-citation-pdf/1",
      strategy_version: "1",
      url: `https://www.jstor.org/download/${jobID}.pdf`,
    };
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) => {
      if (injection.func === planGeneric) {
        return [
          {
            result: {
              evidence: ["e0:citation-doi=exact"],
              candidates: [candidate],
            },
          },
        ];
      }
      return [];
    };
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      requests.push(args);
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? (outcomes.shift() ?? "stale")
              : "applied",
        },
      };
    });
    await h.bridge.start();
    await h.port.inbound(
      helloAck({ features: ["provider_drive_epoch_v1", "effect_permit_v1"] }),
    );
    const offer = jobOffer(jobID, OPENURL, "delegated") as {
      payload: Record<string, unknown>;
    };
    offer.payload["expected"] = { doi: "10.1000/generic" };
    offer.payload["drive_attempt_id"] = `${jobID}-attempt`;
    offer.payload["drive_ordinal"] = 0;
    offer.payload["drive_strategy"] = "generic";
    offer.payload["drive_revision"] = "1";
    await h.port.inbound(offer);
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    h.clock.now += 6_000;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    return requests;
  }
  test.each(["duplicate", "stale", "error"] as const)(
    "generic %s start result never downloads and retains the exact candidate tuple",
    async (outcome) => {
      const h = makeHarness();
      const requests = await genericStartOutcome(
        h,
        `job_generic_${outcome}`,
        outcome,
      );
      expect(h.downloads.started).toHaveLength(0);
      expect(
        requests.filter(
          (args) => args[0] === "provider_drive_epoch_start_request",
        ),
      ).toHaveLength(1);
      expect(requests[0]?.[1]).toMatchObject({
        drive_attempt_id: `job_generic_${outcome}-attempt`,
        ordinal: 0,
        strategy: "generic",
        revision: "1",
      });
      expect(h.backend.store.activeJobs[0]?.generic_drive_epoch).toMatchObject({
        drive_attempt_id: `job_generic_${outcome}-attempt`,
        ordinal: 0,
        strategy: "generic",
        revision: "1",
      });
      expect(
        h.backend.store.activeJobs[0]?.generic_drive_epoch?.ordinal,
      ).not.toBe(1);
      if (outcome === "stale") {
        expect(h.backend.store.activeJobs[0]?.generic_terminal).toBe(false);
        expect(h.backend.store.activeJobs[0]?.generic_deferred).toBe(true);
      } else {
        expect(h.backend.store.activeJobs[0]?.generic_terminal).toBe(true);
        expect(h.backend.store.activeJobs[0]?.generic_deferred).not.toBe(true);
      }
    },
  );

  test("duplicate generic start remains exactly reconcilable as unknown completion", async () => {
    const h = makeHarness();
    const jobID = "job_generic_duplicate_reconcile";
    await genericStartOutcome(h, jobID, "duplicate");
    await h.port.inbound({
      protocol: "papio-browser/1",
      type: "effect_permit_reconcile_request",
      msg_id: "generic-duplicate-reconcile-msg",
      job_id: jobID,
      seq: 9,
      payload: {
        request_id: "generic-duplicate-reconcile-request",
        permit_id: "generic-duplicate-permit",
        effect_kind: "generic_drive",
        drive_attempt_id: `${jobID}-attempt`,
        ordinal: 0,
        strategy: "generic",
        revision: "1",
      },
    });
    const response = h
      .frames()
      .find(
        (frame) =>
          frame.type === "effect_permit_reconcile_response" &&
          frame.payload["request_id"] === "generic-duplicate-reconcile-request",
      );
    expect(response?.payload).toMatchObject({
      outcome: "recorded",
      dispatched: false,
      download_present: false,
      acknowledged: false,
      tab_present: false,
    });
  });

  test("mixed-version generic start is unsupported before authorization and cannot download", async () => {
    const h = makeHarness();
    const candidate: GenericCandidate = {
      strategy_id: "generic-citation-pdf/1",
      strategy_version: "1",
      url: "https://www.jstor.org/download/mixed-version.pdf",
    };
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) =>
      injection.func === planGeneric
        ? [
            {
              result: {
                evidence: ["e0:citation-doi=exact"],
                candidates: [candidate],
              },
            },
          ]
        : [];
    await h.bridge.start();
    // The daemon speaks provider_drive_epoch_v1 but not the permit feature.
    await h.port.inbound(helloAck({ features: ["provider_drive_epoch_v1"] }));
    const offer = jobOffer(
      "job_generic_mixed_version",
      OPENURL,
      "delegated",
    ) as { payload: Record<string, unknown> };
    offer.payload["expected"] = { doi: "10.1000/generic" };
    offer.payload["drive_attempt_id"] = "mixed-version-attempt";
    offer.payload["drive_ordinal"] = 0;
    offer.payload["drive_strategy"] = "generic";
    offer.payload["drive_revision"] = "1";
    await h.port.inbound(offer);
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    h.clock.now += 6_000;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);

    expect(h.downloads.started).toHaveLength(0);
    expect(h.backend.store.activeJobs[0]?.generic_drive_epoch).toMatchObject({
      drive_attempt_id: "mixed-version-attempt",
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(
      h.backend.store.activeJobs[0]?.generic_drive_epoch?.ordinal,
    ).not.toBe(1);
    expect(
      h
        .frames()
        .some((frame) => frame.type === "provider_drive_epoch_start_request"),
    ).toBe(false);
  });
  test("busy stale then exact re-offer reacquires the same never-acquired tuple", async () => {
    const h = makeHarness();
    const jobID = "job_generic_busy_reoffer";
    const requests = await genericStartOutcome(h, jobID, ["stale", "started"]);
    const firstStartCount = requests.filter(
      (args) => args[0] === "provider_drive_epoch_start_request",
    ).length;
    const exactOffer = jobOffer(jobID, OPENURL, "delegated") as {
      payload: Record<string, unknown>;
    };
    exactOffer.payload["expected"] = { doi: "10.1000/generic" };
    exactOffer.payload["drive_attempt_id"] = `${jobID}-attempt`;
    exactOffer.payload["drive_ordinal"] = 0;
    exactOffer.payload["drive_strategy"] = "generic";
    exactOffer.payload["drive_revision"] = "1";

    // Settlement/wake is daemon-owned. A later offer retries exact identity X;
    // the extension must never synthesize ordinal N+1.
    await h.port.inbound(exactOffer);
    // The stale defer clears on the exact same-tuple offer; the next settled-unknown drives the retry.
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    h.clock.now += 6_000;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    expect(h.downloads.started).toHaveLength(1);
    expect(
      requests.filter(
        (args) => args[0] === "provider_drive_epoch_start_request",
      ),
    ).toHaveLength(firstStartCount + 1);
    expect(requests.at(-1)?.[1]).toMatchObject({
      drive_attempt_id: `${jobID}-attempt`,
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(h.backend.store.activeJobs[0]?.generic_drive_epoch).toMatchObject({
      drive_attempt_id: `${jobID}-attempt`,
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(
      h.backend.store.activeJobs[0]?.generic_drive_epoch?.ordinal,
    ).not.toBe(1);
  });

  test("assisted generic execution records E0 evidence without downloading", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_assisted", "assisted", {
      evidence: ["e0:citation-doi=exact"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/a.pdf",
        },
      ],
    });
    expect(h.downloads.started).toHaveLength(0);
    const outcome = h
      .frames()
      .find(
        (frame) =>
          frame.type === "provider_outcome" &&
          frame.payload.outcome === "ui_changed",
      );
    expect(outcome?.payload.detail).toContain("e0:citation-doi=exact");
  });

  test("a delegated generic candidate is revoked before epoch start completes", async () => {
    const h = makeHarness();
    const jobID = "job_generic_downgrade_race";
    const candidate: GenericCandidate = {
      strategy_id: "generic-citation-pdf/1",
      strategy_version: "1",
      url: "https://www.jstor.org/download/revoked.pdf",
    };
    let release!: () => void;
    let entered!: () => void;
    const epochStartEntered = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const epochStartGate = new Promise<void>((resolve) => {
      release = resolve;
    });
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) => {
      if (injection.func === planGeneric) {
        return [
          {
            result: {
              evidence: ["e0:citation-doi=exact"],
              candidates: [candidate],
            },
          },
        ];
      }
      return [];
    };
    await h.bridge.start();
    await h.port.inbound(
      helloAck({ features: ["provider_drive_epoch_v1", "effect_permit_v1"] }),
    );
    const offer = jobOffer(jobID, OPENURL, "delegated") as {
      payload: Record<string, unknown>;
    };
    offer.payload["expected"] = { doi: "10.1000/revoked" };
    offer.payload["drive_attempt_id"] = "generic-revoked-epoch-1";
    offer.payload["drive_ordinal"] = 0;
    offer.payload["drive_strategy"] = "generic";
    offer.payload["drive_revision"] = "1";
    await h.port.inbound(offer);
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      if (args[0] === "provider_drive_epoch_start_request") {
        entered();
        await epochStartGate;
      }
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? "started"
              : "applied",
        },
      };
    });
    const startGenericCandidate = (
      h.bridge as unknown as {
        startGenericCandidate(
          jobID: string,
          candidates: GenericCandidate[],
          requestedIndex: number,
        ): Promise<void>;
      }
    ).startGenericCandidate.bind(h.bridge);
    const attempt = startGenericCandidate(jobID, [candidate], 0);
    await epochStartEntered;
    await h.port.inbound(jobOffer(jobID, OPENURL, "assisted"));
    expect(h.backend.store.activeJobs[0]?.access_mode).toBe("assisted");
    release();
    await attempt;
    expect(h.downloads.started).toHaveLength(0);
    expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
    expect(h.backend.store.activeJobs[0]?.adapter_id).toBeUndefined();
  });

  test("a declared citation URL downloads once and records its strategy", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_citation", "delegated", {
      evidence: ["e0:citation-doi=exact", "e0:citation-pdf=unique"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/download/citation.pdf",
        },
      ],
    });
    expect(h.downloads.started).toHaveLength(1);
    expect(h.downloads.started[0]?.url).toBe(
      "https://www.jstor.org/download/citation.pdf",
    );
    expect(h.backend.store.activeJobs[0]?.adapter_id).toBe(
      "generic-citation-pdf/1",
    );
  });

  test("an interrupted citation download stays parked without advancing", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_ordinary_failure", "delegated", {
      evidence: ["e0:citation-doi=exact"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/download/missing.pdf",
        },
        {
          strategy_id: "generic-article-pdf-link/1",
          strategy_version: "1",
          url: "https://www.jstor.org/pdf/article.pdf",
        },
      ],
    });
    expect(h.downloads.started).toHaveLength(1);
    await h.downloads.onChanged.emit({
      id: 901,
      state: { current: "interrupted" },
      error: { current: "NETWORK_FAILED" },
    });
    expect(h.downloads.started.map((entry) => entry.url)).toEqual([
      "https://www.jstor.org/download/missing.pdf",
    ]);
  });
  test("identity revalidation failure stops generic execution without a download", async () => {
    const h = makeHarness();
    let plans = 0;
    const requests: unknown[][] = [];
    h.deps.permissions.contains = async () => true;
    h.deps.scripting.executeScript = async (injection) => {
      if (injection.func === planGeneric) {
        plans += 1;
        return [
          {
            result:
              plans === 1
                ? {
                    evidence: ["e0:citation-doi=exact"],
                    candidates: [
                      {
                        strategy_id: "generic-citation-pdf/1",
                        strategy_version: "1",
                        url: "https://www.jstor.org/download/identity.pdf",
                      },
                    ],
                  }
                : { evidence: ["e0:citation-doi=mismatch"], candidates: [] },
          },
        ];
      }
      return [];
    };
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      requests.push(args);
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? "started"
              : "applied",
        },
      };
    });
    await h.bridge.start();
    await h.port.inbound(
      helloAck({ features: ["provider_drive_epoch_v1", "effect_permit_v1"] }),
    );
    // requestNative above models the feature-negotiated daemon ACK.
    const offer = jobOffer("job_generic_identity", OPENURL, "delegated") as {
      payload: Record<string, unknown>;
    };
    offer.payload["expected"] = { doi: "10.1000/generic" };
    offer.payload["drive_attempt_id"] = "generic-identity-epoch-1";
    offer.payload["drive_ordinal"] = 0;
    offer.payload["drive_strategy"] = "generic";
    offer.payload["drive_revision"] = "1";
    await h.port.inbound(offer);
    const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    h.clock.now += 6_000;
    await h.tabs.completeNavigation(tabID, `https://${PROVIDER_HOST}/article`);
    expect(h.downloads.started).toHaveLength(0);
    expect(
      requests.some(
        (args) =>
          args[0] === "provider_drive_epoch_result_request" &&
          (args[1] as Record<string, unknown>)?.outcome === "wrong_work" &&
          (args[1] as Record<string, unknown>)?.drive_attempt_id ===
            "generic-identity-epoch-1",
      ),
    ).toBe(true);
    expect(
      requests
        .filter((args) => args[0] === "provider_drive_epoch_result_request")
        .every((args) => args[5] === "job_generic_identity"),
    ).toBe(true);
    expect(
      h
        .frames()
        .some(
          (frame) =>
            frame.type === "provider_outcome" &&
            frame.payload.outcome === "wrong_work",
        ),
    ).toBe(false);
  });

  test("generic completed download is settled after worker restart", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_restart_complete", "delegated", {
      evidence: ["e0:citation-doi=exact"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/download/restart-complete.pdf",
        },
      ],
    });
    // Startup reconciliation may observe the exact in-flight download, but
    // producer identity is feature-gated on the newly negotiated port. Keep
    // the browser item in flight until that handshake has completed, then
    // deliver the terminal event through the reloaded worker.
    h.downloads.items.set(901, {
      id: 901,
      filename: "/tmp/restart-complete.pdf",
      fileSize: 12,
      mime: "application/pdf",
      state: "in_progress",
    });
    const reloaded = new Bridge(h.deps);
    Reflect.set(reloaded, "requestNative", async (...args: unknown[]) => ({
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome:
          args[2] === "provider_drive_epoch_start_result"
            ? "started"
            : "applied",
      },
    }));
    await reloaded.start();
    const reloadedPort = h.ports[1];
    expect(reloadedPort).toBeDefined();
    await reloadedPort!.inbound(
      helloAck({ features: ["provider_drive_epoch_v1", "effect_permit_v1"] }),
    );
    // The old worker's in-memory track dies with the worker; the persisted
    // epoch is what the reloaded bridge reconstructs.
    (Reflect.get(h.bridge, "downloads") as Map<string, unknown>).delete(
      "job_generic_restart_complete",
    );
    h.downloads.items.set(901, {
      id: 901,
      filename: "/tmp/restart-complete.pdf",
      fileSize: 12,
      mime: "application/pdf",
      state: "complete",
    });
    await h.downloads.onChanged.emit({
      id: 901,
      state: { current: "complete" },
    });
    const complete = h
      .frames()
      .filter((frame) => frame.type === "download_complete");
    expect(complete[0]?.payload["producer"]).toEqual({
      effect_kind: "generic_drive",
      drive_attempt_id: "generic-test-epoch-1",
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(complete).toHaveLength(1);
    expect(
      h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id,
    ).toBe(901);
  });

  test("generic interrupted download settles its exact tuple after worker restart", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_restart_interrupted", "delegated", {
      evidence: ["e0:citation-doi=exact"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/download/restart-interrupted.pdf",
        },
      ],
    });
    h.downloads.items.set(901, {
      id: 901,
      state: "interrupted",
      mime: "application/pdf",
    });
    const reloaded = new Bridge(h.deps);
    const requests: unknown[][] = [];
    Reflect.set(reloaded, "requestNative", async (...args: unknown[]) => {
      requests.push(args);
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? "started"
              : "applied",
        },
      };
    });
    await reloaded.start();
    const cancelled = requests.filter(
      (args) =>
        args[0] === "provider_drive_epoch_result_request" &&
        (args[1] as Record<string, unknown>)?.outcome === "cancelled",
    );
    await h.downloads.onChanged.emit({
      id: 901,
      state: { current: "interrupted" },
    });
    expect(
      requests.filter(
        (args) =>
          args[0] === "provider_drive_epoch_result_request" &&
          (args[1] as Record<string, unknown>)?.outcome === "cancelled",
      ),
    ).toHaveLength(1);
  });

  test("the generic attempt latch survives a worker restart", async () => {
    const h = makeHarness();
    await reachUnknown(h, "job_generic_restart", "delegated", {
      evidence: ["e0:citation-doi=exact"],
      candidates: [
        {
          strategy_id: "generic-citation-pdf/1",
          strategy_version: "1",
          url: "https://www.jstor.org/download/restart.pdf",
        },
      ],
    });
    const before = h.downloads.started.length;
    const persisted = JSON.parse(JSON.stringify(h.backend.store)) as StoreShape;
    expect(JSON.stringify(persisted)).not.toContain("restart.pdf");
    const reloaded = new Bridge(h.deps);
    let restartedStartRequests = 0;
    Reflect.set(reloaded, "requestNative", async (...args: unknown[]) => {
      if (args[0] === "provider_drive_epoch_start_request")
        restartedStartRequests += 1;
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? "started"
              : "applied",
        },
      };
    });
    await reloaded.start();
    expect(restartedStartRequests).toBe(0);
    expect(h.downloads.started).toHaveLength(before);
    expect(
      (persisted.activeJobs[0] as ActiveJob & { generic_evaluated?: boolean })
        ?.generic_evaluated,
    ).toBe(true);
  });
  test("generic browser-document MIME parks the current candidate without advancing", async () => {
    const h = makeHarness();
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => ({
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome:
          args[2] === "provider_drive_epoch_start_result"
            ? "started"
            : "applied",
      },
    }));
    await reachUnknown(
      h,
      "job_generic_html",
      "delegated",
      {
        evidence: ["e0:citation-doi=exact"],
        candidates: [
          {
            strategy_id: "generic-citation-pdf/1",
            strategy_version: "1",
            url: "https://www.jstor.org/download/html.pdf",
          },
          {
            strategy_id: "generic-article-pdf-link/1",
            strategy_version: "1",
            url: "https://www.jstor.org/download/second.pdf",
          },
        ],
      },
      "10.1000/generic",
      true,
    );
    h.downloads.items.set(901, {
      id: 901,
      filename: "/tmp/wrapper.html",
      fileSize: 10,
      mime: "text/html",
      state: "complete",
    });
    await h.downloads.onChanged.emit({
      id: 901,
      state: { current: "complete" },
    });
    expect(h.downloads.started.map((entry) => entry.url)).toEqual([
      "https://www.jstor.org/download/html.pdf",
    ]);
  });

  test("generic clean non-browser MIME retains exact tuple and awaits daemon successor", async () => {
    const h = makeHarness();
    const requests: unknown[][] = [];
    const makeResponse = async (...args: unknown[]) => {
      requests.push(args);
      return {
        kind: "response",
        payload: {
          ...((args[1] ?? {}) as Record<string, unknown>),
          outcome:
            args[2] === "provider_drive_epoch_start_result"
              ? "started"
              : "applied",
        },
      };
    };
    Reflect.set(h.bridge, "requestNative", makeResponse);
    await reachUnknown(
      h,
      "job_generic_clean",
      "delegated",
      {
        evidence: ["e0:citation-doi=exact"],
        candidates: [
          {
            strategy_id: "generic-citation-pdf/1",
            strategy_version: "1",
            url: "https://www.jstor.org/download/image.pdf",
          },
          {
            strategy_id: "generic-article-pdf-link/1",
            strategy_version: "1",
            url: "https://www.jstor.org/download/second.pdf",
          },
        ],
      },
      "10.1000/generic",
      true,
    );
    // reachUnknown overwrites requestNative; reinstall our tracker for the completion phase
    Reflect.set(h.bridge, "requestNative", makeResponse);
    expect(h.downloads.started.map((entry) => entry.url)).toEqual([
      "https://www.jstor.org/download/image.pdf",
    ]);
    h.downloads.items.set(901, {
      id: 901,
      filename: "/tmp/image.png",
      fileSize: 10,
      mime: "image/png",
      state: "complete",
    });
    await h.downloads.onChanged.emit({
      id: 901,
      state: { current: "complete" },
    });
    expect(h.downloads.started.map((entry) => entry.url)).toEqual([
      "https://www.jstor.org/download/image.pdf",
    ]);
    const resultRequests = requests.filter(
      (args) => args[0] === "provider_drive_epoch_result_request",
    );
    expect(resultRequests).toHaveLength(1);
    expect(resultRequests[0]?.[1]).toMatchObject({
      drive_attempt_id: "generic-test-epoch-1",
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(h.backend.store.activeJobs[0]?.generic_drive_epoch).toMatchObject({
      drive_attempt_id: "generic-test-epoch-1",
      ordinal: 0,
      strategy: "generic",
      revision: "1",
    });
    expect(h.backend.store.activeJobs[0]?.generic_terminal).toBe(true);
    expect(h.backend.store.activeJobs[0]?.generic_deferred).not.toBe(true);
  });
});

test("direct named DOI route completes a valid PDF without persisting its URL", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942";
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_valid_0001",
    job_id: "job_direct_valid",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-valid-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: directURL,
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  h.downloads.items.set(901, {
    id: 901,
    filename: "/tmp/direct-valid.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: directURL,
    url: directURL,
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  const complete = h
    .frames()
    .find((frame) => frame.type === "download_complete");
  expect(complete?.payload["producer"]).toEqual({
    effect_kind: "direct_get",
    drive_attempt_id: "direct-valid-attempt",
    ordinal: 0,
    strategy: "direct_get",
    revision: "acm-doi-pdf/1",
  });
  const result = h
    .frames()
    .find((frame) => frame.type === "provider_direct_get_result");
  expect(result?.payload).toMatchObject({
    drive_attempt_id: "direct-valid-attempt",
    outcome: "success",
    landing_class: "pdf",
  });
  expect(
    h.frames().filter((frame) => frame.type === "download_started"),
  ).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "download_complete"),
  ).toHaveLength(1);
  expect(JSON.stringify(h.backend.store)).not.toContain(directURL);
});

test("an authorized institutional download carries only its exact producer tuple", async () => {
  const jobID = "job_institutional_download_producer";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: 301,
        offered_at: 1_700_000_000_000,
        expires_at: 1_900_000_000_000,
        status: "accepted",
        provider_hosts: [PROVIDER_HOST],
        access_mode: "delegated",
      },
    ],
    materializations: {
      [jobID]: {
        job_id: jobID,
        candidate_id: "cand-producer-0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim-producer-0001",
        binding_id: "binding-producer-0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "navigated",
        tab_id: 301,
        effect_ordinal: 7,
        institutional_request_id: "institutional-request-0001",
      },
    },
  });
  h.tabs.seed({
    id: 301,
    url: "https://provider.example.edu/article",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["effect_permit_v1", "institutional_materialization_v1"],
    }),
  );
  // A same-job download from another tab may still be correlated by provider
  // host, but it is not the materialization effect and must not inherit its
  // producer tuple.
  await h.downloads.onCreated.emit({
    id: 904,
    tabId: 302,
    url: "https://provider.example.edu/download",
    state: "in_progress",
  });
  h.downloads.items.set(904, {
    id: 904,
    tabId: 302,
    filename: "/tmp/wrong-tab.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 904, state: { current: "complete" } });
  const wrongTabProducer = h
    .frames()
    .find(
      (frame) =>
        frame.type === "download_complete" &&
        frame.payload["download_id"] === 904 &&
        frame.payload["producer"] !== undefined,
    );
  expect(wrongTabProducer).toBeUndefined();
  await h.downloads.onCreated.emit({
    id: 905,
    tabId: 301,
    url: "https://provider.example.edu/download",
    state: "in_progress",
  });
  h.downloads.items.set(905, {
    id: 905,
    tabId: 301,
    filename: "/tmp/institutional.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
  });
  await h.downloads.onChanged.emit({ id: 905, state: { current: "complete" } });
  const complete = h
    .frames()
    .find(
      (frame) =>
        frame.type === "download_complete" &&
        frame.payload["download_id"] === 905,
    );
  expect(complete?.payload["producer"]).toEqual({
    effect_kind: "institutional",
    claim_id: "claim-producer-0001",
    binding_id: "binding-producer-0001",
    effect_ordinal: 7,
    institutional_request_id: "institutional-request-0001",
  });
});
test("tabless direct downloads share the single effect governor and release after initiation", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  let unblock!: () => void;
  const blocked = new Promise<void>((resolve) => {
    unblock = resolve;
  });
  h.downloads.afterCreate = async () => blocked;
  const onDirect = Reflect.get(h.bridge, "onProviderDirectGetRequest") as (
    message: unknown,
  ) => Promise<void>;
  const direct = (jobID: string, msgID: string) =>
    onDirect.call(h.bridge, {
      protocol: "papio-browser/1",
      type: "provider_direct_get_request",
      msg_id: msgID,
      job_id: jobID,
      seq: 2,
      payload: {
        drive_attempt_id: `${jobID}-attempt`,
        ordinal: 0,
        route_revision: "acm-doi-pdf/1",
        expected_identifier: "doi:10.1145/3630106.3658942",
        url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
        allowed_origin: "https://dl.acm.org",
        path_family: "/doi/pdf/{doi}",
        terms_policy: "none",
      },
    });
  const first = direct("job_direct_governor_a", "direct_governor_a");
  for (
    let attempt = 0;
    attempt < 20 && h.downloads.started.length < 1;
    attempt += 1
  ) {
    await Promise.resolve();
  }
  expect(h.downloads.started).toHaveLength(1);
  await direct("job_direct_governor_b", "direct_governor_b");
  expect(h.downloads.started).toHaveLength(1);
  unblock();
  await first;
  for (
    let attempt = 0;
    attempt < 200 &&
    (h.downloads.started.length < 2 ||
      Reflect.get(h.bridge, "effectGovernorOwner") !== undefined);
    attempt += 1
  ) {
    await Promise.resolve();
  }
  expect(h.downloads.started).toHaveLength(2);
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
  expect(Reflect.get(h.bridge, "providerDrainLeaseOwners")).toEqual(new Map());
});
test("provider tab navigation waits behind an unlike direct effect and resumes after its consequence", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  let unblock!: () => void;
  const blocked = new Promise<void>((resolve) => {
    unblock = resolve;
  });
  h.downloads.afterCreate = async () => blocked;
  const onDirect = Reflect.get(h.bridge, "onProviderDirectGetRequest") as (
    message: unknown,
  ) => Promise<void>;
  const first = onDirect.call(h.bridge, {
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "cross_kind_direct",
    job_id: "job_cross_kind_direct",
    seq: 2,
    payload: {
      drive_attempt_id: "cross-kind-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  for (
    let attempt = 0;
    attempt < 20 && h.downloads.started.length < 1;
    attempt += 1
  ) {
    await Promise.resolve();
  }
  expect(h.downloads.started).toHaveLength(1);
  await h.port.inbound(jobOffer("job_cross_kind_tab"));
  expect(h.tabs.created).toHaveLength(0);
  unblock();
  await first;
  for (
    let attempt = 0;
    attempt < 20 && h.tabs.created.length < 1;
    attempt += 1
  ) {
    await Promise.resolve();
  }
  expect(h.tabs.created).toHaveLength(1);
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toMatchObject({
    jobID: "job_cross_kind_tab",
  });
});
test("tabless direct download failure releases both governors for a retry", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  h.downloads.failDownload = true;
  const onDirect = Reflect.get(h.bridge, "onProviderDirectGetRequest") as (
    message: unknown,
  ) => Promise<void>;
  await onDirect.call(h.bridge, {
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_failure_0001",
    job_id: "job_direct_failure",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-failure-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
  expect(Reflect.get(h.bridge, "providerDrainLeaseOwners")).toEqual(new Map());
});

test("direct download persistence failure does not report a false network result", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  h.downloads.afterCreate = () => {
    h.backend.failNextSave = true;
  };
  const onDirect = Reflect.get(h.bridge, "onProviderDirectGetRequest") as (
    message: unknown,
  ) => Promise<void>;
  await onDirect.call(h.bridge, {
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_persist_failure_0001",
    job_id: "job_direct_persist_failure",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-persist-failure-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  expect(h.downloads.started).toHaveLength(1);
  expect(
    h.frames().filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(0);
  expect(
    Reflect.get(h.bridge, "downloads").get("job_direct_persist_failure")?.ids,
  ).toEqual(new Set([901]));
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
  expect(Reflect.get(h.bridge, "providerDrainLeaseOwners")).toEqual(new Map());
});

test("direct PDF with a foreign final envelope never emits adoption events", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_foreign_0001",
    job_id: "job_direct_foreign",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-foreign-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  h.downloads.items.set(901, {
    id: 901,
    filename: "/tmp/direct-foreign.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: "https://evil.example/download.pdf",
    url: "https://evil.example/download.pdf",
  });

  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  const result = h
    .frames()
    .find((frame) => frame.type === "provider_direct_get_result");
  expect(result?.payload).toMatchObject({
    drive_attempt_id: "direct-foreign-attempt",
    outcome: "foreign",
    landing_class: "foreign",
  });
  expect(
    h.frames().filter((frame) => frame.type === "download_started"),
  ).toHaveLength(0);
  expect(
    h.frames().filter((frame) => frame.type === "download_complete"),
  ).toHaveLength(0);
  expect(h.downloads.removedFiles).toEqual([901]);
});
test("direct named-marker mismatch is rejected at completion", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["provider_direct_get_v1", "effect_permit_v1"] }),
  );
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "provider_direct_get_request",
    msg_id: "direct_marker_mismatch_0001",
    job_id: "job_direct_marker_mismatch",
    seq: 2,
    payload: {
      drive_attempt_id: "direct-marker-mismatch-attempt",
      ordinal: 0,
      route_revision: "acm-doi-pdf/1",
      expected_identifier: "doi:10.1145/3630106.3658942",
      url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
      allowed_origin: "https://dl.acm.org",
      path_family: "/doi/pdf/{doi}",
      terms_policy: "none",
    },
  });
  h.downloads.items.set(901, {
    id: 901,
    filename: "/tmp/direct-marker-mismatch.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: "https://dl.acm.org/doi/pdf/10.1145/3630106.other",
    url: "https://dl.acm.org/doi/pdf/10.1145/3630106.other",
  });
  await h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  const result = h
    .frames()
    .find((frame) => frame.type === "provider_direct_get_result");
  expect(result?.payload).toMatchObject({
    drive_attempt_id: "direct-marker-mismatch-attempt",
    outcome: "foreign",
    landing_class: "foreign",
  });
  expect(h.downloads.started).toHaveLength(1);
  expect(h.downloads.removedFiles).toEqual([901]);
  expect(
    h.frames().filter((frame) => frame.type === "download_started"),
  ).toHaveLength(0);
  expect(
    h.frames().filter((frame) => frame.type === "download_complete"),
  ).toHaveLength(0);
});

test("direct completed download is recovered once across worker restarts", async () => {
  const directURL = "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942";
  const seed: StoreShape = {
    ...emptyStore(),
    activeJobs: [
      {
        job_id: "job_direct_restart",
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: ["dl.acm.org"],
        access_mode: "delegated",
        download_initiated: true,
        direct_terminal: false,
        drive_epoch: {
          drive_attempt_id: "direct-restart-attempt",
          ordinal: 0,
          strategy: "direct",
          route_revision: "acm-doi-pdf/1",
          in_flight_download_id: 901,
          attempt_count: 1,
        },
        direct_envelope: {
          allowed_origin: "https://dl.acm.org",
          path_family: "/doi/pdf/{doi}",
          expected_identifier: "doi:10.1145/3630106.3658942",
        },
      },
    ],
  };
  const first = makeHarness(seed);
  first.downloads.items.set(901, {
    id: 901,
    filename: "/tmp/direct-restart.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: directURL,
    url: directURL,
  });
  await first.bridge.start();
  expect(
    first
      .frames()
      .filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(1);
  expect(first.backend.store.activeJobs[0]?.direct_terminal).toBe(true);
  expect(JSON.stringify(first.backend.store)).not.toContain(directURL);

  const second = makeHarness(
    JSON.parse(JSON.stringify(first.backend.store)) as StoreShape,
  );
  second.downloads.items.set(901, {
    id: 901,
    filename: "/tmp/direct-restart.pdf",
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: directURL,
    url: directURL,
  });
  await second.bridge.start();
  expect(
    second
      .frames()
      .filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(0);
});
test("direct pre-ID restart does not infer correlation from a matching filename", async () => {
  const jobID = "job_direct_pre_id";
  const seed: StoreShape = {
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: ["dl.acm.org"],
        access_mode: "delegated",
        download_initiated: true,
        drive_epoch: {
          drive_attempt_id: "direct-pre-id-attempt",
          ordinal: 0,
          strategy: "direct",
          route_revision: "acm-doi-pdf/1",
          attempt_count: 1,
        },
        direct_envelope: {
          allowed_origin: "https://dl.acm.org",
          path_family: "/doi/pdf/{doi}",
          expected_identifier: "doi:10.1145/3630106.3658942",
        },
      },
    ],
  };
  const h = makeHarness(seed);
  h.downloads.items.set(901, {
    id: 901,
    filename: jobDownloadFilename(jobID),
    fileSize: 12,
    mime: "application/pdf",
    state: "complete",
    finalUrl: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
    url: "https://dl.acm.org/doi/pdf/10.1145/3630106.3658942",
  });
  await h.bridge.start();
  expect(
    h.frames().filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(0);
  expect(h.backend.store.activeJobs[0]?.direct_terminal).not.toBe(true);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  const second = new Bridge(h.deps);
  await second.start();
  expect(
    h.frames().filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(0);
});
test("direct pre-ID restart with no exact id keeps the effect unresolved", async () => {
  const jobID = "job_direct_pre_id_missing";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: ["dl.acm.org"],
        access_mode: "delegated",
        download_initiated: true,
        drive_epoch: {
          drive_attempt_id: "direct-pre-id-missing",
          ordinal: 0,
          strategy: "direct",
          route_revision: "acm-doi-pdf/1",
          attempt_count: 1,
        },
        direct_envelope: {
          allowed_origin: "https://dl.acm.org",
          path_family: "/doi/pdf/{doi}",
          expected_identifier: "doi:10.1145/3630106.3658942",
        },
      },
    ],
  });
  await h.bridge.start();
  expect(
    h.frames().filter((frame) => frame.type === "provider_direct_get_result"),
  ).toHaveLength(0);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(h.backend.store.activeJobs[0]?.direct_terminal).not.toBe(true);
});

test("generic exact-ID interruption settles once and missing exact ID remains unresolved", async () => {
  const jobID = "job_generic_pre_id";
  const requests: unknown[][] = [];
  const seed: StoreShape = {
    ...emptyStore(),
    activeJobs: [
      {
        job_id: jobID,
        tab_id: -1,
        offered_at: 1_700_000_000_000,
        expires_at: 1_800_000_000_000,
        status: "accepted",
        provider_hosts: ["www.jstor.org"],
        access_mode: "delegated",
        download_initiated: true,
        generic_drive_epoch: {
          drive_attempt_id: "generic-pre-id-attempt",
          ordinal: 0,
          strategy: "generic",
          revision: "1",
          strategy_id: "generic-citation-pdf/1",
          in_flight_download_id: 901,
          attempt_count: 1,
        },
      },
    ],
  };
  const h = makeHarness(seed);
  Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
    requests.push(args);
    return {
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome: "applied",
      },
    };
  });
  h.downloads.items.set(901, {
    id: 901,
    filename: jobDownloadFilename(jobID),
    mime: "application/pdf",
    state: "interrupted",
  });
  await h.bridge.start();
  expect(
    requests.filter(
      (args) => args[0] === "provider_drive_epoch_result_request",
    ),
  ).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.generic_terminal).toBe(true);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(false);
  const second = new Bridge(h.deps);
  Reflect.set(second, "requestNative", async (...args: unknown[]) => {
    requests.push(args);
    return {
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome: "applied",
      },
    };
  });
  await second.start();
  expect(
    requests.filter(
      (args) => args[0] === "provider_drive_epoch_result_request",
    ),
  ).toHaveLength(1);
  expect(requests[0]?.[5]).toBe(jobID);

  const missing = makeHarness(seed);
  const missingRequests: unknown[][] = [];
  Reflect.set(missing.bridge, "requestNative", async (...args: unknown[]) => {
    missingRequests.push(args);
    return {
      kind: "response",
      payload: {
        ...((args[1] ?? {}) as Record<string, unknown>),
        outcome: "applied",
      },
    };
  });
  missing.downloads.items.clear();
  await missing.bridge.start();
  expect(
    missingRequests.filter(
      (args) => args[0] === "provider_drive_epoch_result_request",
    ),
  ).toHaveLength(0);
  expect(missing.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(missing.backend.store.activeJobs[0]?.generic_terminal).not.toBe(true);
});
test("institutional candidate offer dispatches claim without awaiting the correlated response", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const inbound = h.port.inbound(candidateOffer("job_mat_0001"));
  await inbound;
  const claim = await h.port.waitForFrame("institutional_claim_request");
  expect(claim.job_id).toBe("job_mat_0001");
  expect(claim.payload).toMatchObject({
    candidate_id: "cand_0001",
    materialization_kind: "browser_tab",
  });
  expect(typeof claim.payload.request_id).toBe("string");
  expect(claim.payload.request_id).not.toBe("");
  expect(JSON.stringify(h.backend.store)).not.toContain("https://");
  expect(h.backend.store.materializations?.["job_mat_0001"]).toMatchObject({
    candidate_id: "cand_0001",
    phase: "claiming",
    tab_id: -1,
  });
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === "job_mat_0001"),
  ).toMatchObject({
    job_id: "job_mat_0001",
    tab_id: -1,
    status: "accepted",
    provider_hosts: [PROVIDER_HOST],
    expected: { doi: "10.1234/example", title: "Example work" },
    requires_auth: true,
    access_mode: "delegated",
    generic_drive_epoch: {
      drive_attempt_id: "attempt-001",
      ordinal: 0,
      strategy: "generic",
      revision: "rev-1",
    },
  });
});

test("candidate materialization waits behind the global browser effect permit", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    claimEffectGovernor: (jobID: string) => string | undefined;
    releaseEffectGovernor: (
      jobID: string,
      token: string,
      wake?: boolean,
    ) => void;
  };
  const token = internals.claimEffectGovernor.call(h.bridge, "blocking-effect");
  expect(token).toBeDefined();
  await h.port.inbound(candidateOffer("job_mat_effect_wait"));
  await Promise.resolve();
  expect(
    h.frames().some((frame) => frame.type === "institutional_claim_request"),
  ).toBe(false);

  internals.releaseEffectGovernor.call(
    h.bridge,
    "blocking-effect",
    token!,
    true,
  );
  const claim = await h.port.waitForFrame("institutional_claim_request");
  expect(claim.job_id).toBe("job_mat_effect_wait");
});

test("persisted materialization waits for capability negotiation and clears on daemon downgrade", async () => {
  const h = makeHarness({
    activeJobs: [materializationActiveJob("job_mat_downgrade")],
    workWindowID: 500,
    materializations: {
      job_mat_downgrade: {
        job_id: "job_mat_downgrade",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        binding_id: "bind_0001",
        phase: "bound",
        tab_id: 902,
      },
    },
  });
  h.tabs.seed({
    id: 902,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  expect(h.tabs.removed).not.toContain(902);
  expect(
    h.frames().some((frame) => frame.type.startsWith("institutional_")),
  ).toBe(false);

  await h.port.inbound(helloAck());
  expect(h.tabs.removed).toContain(902);
  expect(h.backend.store.materializations).toBeUndefined();
});

test("candidate-only materialization binds ActiveJob tab before provider navigation", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  await h.port.inbound(candidateOffer("job_mat_candidate_only"));
  await h.port.waitForFrame("institutional_claim_request");
  const internals = h.bridge as unknown as {
    cancelMaterializationWorkflow: (jobID: string) => void;
    applyMaterialization: (jobID: string, event: unknown) => Promise<void>;
    onTabUpdated: (
      tabID: number,
      change: Record<string, string>,
      tab: TabInfo,
    ) => Promise<void>;
  };
  internals.cancelMaterializationWorkflow("job_mat_candidate_only");
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "claimed",
    claim_id: "claim_0001",
    binding_id: "bind_0001",
    browser_holder_generation: 1,
    lease_until: "2030-01-01T00:00:00Z",
  });
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "scaffolded",
    tab_id: 777,
  });
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "bound",
  });
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "route_issued",
    route_issuance_ordinal: 1,
  });
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "navigating",
  });
  await internals.applyMaterialization("job_mat_candidate_only", {
    type: "navigated",
  });
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_mat_candidate_only",
    ),
  ).toMatchObject({
    tab_id: 777,
    provider_hosts: [PROVIDER_HOST],
  });
  h.tabs.seed({
    id: 777,
    url: "https://www.jstor.org/stable/example",
    active: false,
    windowId: 500,
  });
  let classified: [string, string] | undefined;
  Reflect.set(h.bridge, "assessTrackedDrivenPage", async () => false);
  Reflect.set(h.bridge, "maybeDownloadPDFViewer", async () => {});
  Reflect.set(
    h.bridge,
    "maybeClassify",
    async (jobID: string, host: string) => {
      classified = [jobID, host];
      return undefined;
    },
  );
  await internals.onTabUpdated(
    777,
    { url: "https://www.jstor.org/stable/example", status: "complete" },
    {
      id: 777,
      url: "https://www.jstor.org/stable/example",
      active: false,
      windowId: 500,
    },
  );
  expect(classified).toEqual(["job_mat_candidate_only", "www.jstor.org"]);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_mat_candidate_only",
    )?.tab_id,
  ).toBe(777);
  expect(JSON.stringify(h.backend.store)).not.toContain(
    "https://resolver.example.edu",
  );
});
test("institutional claim busy uses bounded retry instead of a dead failed phase", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  await h.port.inbound(candidateOffer("job_mat_busy"));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_claim_response",
    msg_id: "claim_response_busy",
    job_id: "job_mat_busy",
    seq: 3,
    payload: { request_id: claim.payload["request_id"], outcome: "busy" },
  });
  await new Promise<void>((resolve) => queueMicrotask(resolve));
  await new Promise<void>((resolve) => queueMicrotask(resolve));
  await new Promise<void>((resolve) => queueMicrotask(resolve));
  expect(h.backend.store.materializations?.["job_mat_busy"]).toMatchObject({
    phase: "offered",
    retry_attempts: 1,
  });
  const internals = h.bridge as unknown as {
    materializationRetryTimers: Map<string, object>;
  };
  expect(internals.materializationRetryTimers.has("job_mat_busy")).toBe(true);
});
test("same candidate fresh expiry revives an expired pre-claim offer", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  await h.port.inbound(candidateOffer("job_mat_reoffer"));
  const firstClaim = await h.port.waitForFrame("institutional_claim_request");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_claim_response",
    msg_id: "claim_response_reoffer_busy",
    job_id: "job_mat_reoffer",
    seq: 3,
    payload: { request_id: firstClaim.payload["request_id"], outcome: "busy" },
  });
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  const internals = h.bridge as unknown as {
    materializationRuns: Map<string, Promise<void>>;
    materializationRetryTimers: Map<string, object>;
  };
  expect(internals.materializationRuns.has("job_mat_reoffer")).toBe(false);
  const retryTimer = h.timers.find((timer) => timer.ms === 1_000);
  expect(retryTimer).toBeDefined();
  h.clock.now = Date.parse("2031-01-01T00:00:00Z");
  const fresh = candidateOffer("job_mat_reoffer") as {
    msg_id: string;
    seq: number;
    payload: Record<string, unknown>;
  };
  fresh.msg_id = "candidate_offer_000002";
  fresh.seq = 4;
  fresh.payload["expires_at"] = "2032-01-01T00:00:00Z";
  const afterFirstClaim = h.port.posted.length;
  await h.port.inbound(fresh);
  const secondClaim = await h.port.waitForFrame(
    "institutional_claim_request",
    afterFirstClaim,
  );
  expect(secondClaim.payload["candidate_id"]).toBe("cand_0001");
  expect(h.backend.store.materializations?.["job_mat_reoffer"]).toMatchObject({
    phase: "claiming",
    candidate_id: "cand_0001",
    candidate_expires_at: "2032-01-01T00:00:00Z",
  });
});

test("institutional bind error uses bounded retry instead of a dead failed phase", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
    runMaterialization: (jobID: string) => Promise<void>;
    materializationRetryTimers: Map<string, object>;
    cancelledMaterializationJobs: Set<string>;
    pendingMaterializationRequests: unknown[];
  };
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [materializationActiveJob("job_mat_bind_error")],
    materializations: {
      job_mat_bind_error: {
        job_id: "job_mat_bind_error",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "claimed",
        tab_id: -1,
      },
    },
  }));
  const current = h.backend.store.materializations?.["job_mat_bind_error"];
  expect(current).toMatchObject({
    phase: "claimed",
    candidate_id: "cand_0001",
    binding_id: "bind_0001",
    tab_id: -1,
  });
  expect(internals.cancelledMaterializationJobs.has("job_mat_bind_error")).toBe(
    false,
  );
  const bindPromise = h.port.waitForFrame("institutional_bind_request");
  let runSettled = false;
  const runPromise = internals.runMaterialization
    .call(h.bridge, "job_mat_bind_error")
    .finally(() => {
      runSettled = true;
    });
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  const after = h.backend.store.materializations?.["job_mat_bind_error"];
  const frameTypes = h.frames().map((frame) => frame.type);
  if (!frameTypes.includes("institutional_bind_request")) {
    throw new Error(
      JSON.stringify({
        correlation: after,
        created: h.tabs.created.length,
        queries: h.tabs.queryCount,
        pending: internals.pendingMaterializationRequests.length,
        runSettled,
        cancelled:
          internals.cancelledMaterializationJobs.has("job_mat_bind_error"),
        features: h.backend.store.daemonFeatures,
      }),
    );
  }
  const bind = await bindPromise;
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_bind_response",
    msg_id: "bind_response_error",
    job_id: "job_mat_bind_error",
    seq: 4,
    payload: { request_id: bind.payload["request_id"], outcome: "error" },
  });
  await runPromise;
  await Promise.resolve();
  await Promise.resolve();
  expect(
    h.backend.store.materializations?.["job_mat_bind_error"],
  ).toMatchObject({
    phase: "claimed",
    retry_attempts: 1,
  });
  expect(internals.materializationRetryTimers.has("job_mat_bind_error")).toBe(
    true,
  );
});

test("scaffold creation failure retries and converges to one replacement", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
    runMaterialization: (jobID: string) => Promise<void>;
    materializationRetryTimers: Map<string, object>;
    cancelledMaterializationJobs: Set<string>;
  };
  h.tabs.seed({ id: 999, url: OPENURL, active: false, windowId: 500 });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [
      { ...materializationActiveJob("job_mat_scaffold_retry"), tab_id: 999 },
    ],
    workWindowID: 500,
    materializations: {
      job_mat_scaffold_retry: {
        job_id: "job_mat_scaffold_retry",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "claimed",
        tab_id: -1,
      },
    },
  }));
  const current = h.backend.store.materializations?.["job_mat_scaffold_retry"];
  expect(current).toMatchObject({
    phase: "claimed",
    candidate_id: "cand_0001",
    binding_id: "bind_0001",
    tab_id: -1,
  });
  expect(Date.parse(current?.candidate_expires_at ?? "")).toBeGreaterThan(
    h.clock.now,
  );
  expect(
    internals.cancelledMaterializationJobs.has("job_mat_scaffold_retry"),
  ).toBe(false);
  h.tabs.failCreate = true;
  await internals.runMaterialization.call(h.bridge, "job_mat_scaffold_retry");
  expect(
    internals.materializationRetryTimers.has("job_mat_scaffold_retry"),
  ).toBe(true);
  const retryTimer = h.timers
    .slice()
    .reverse()
    .find((timer) => timer.ms === 1_000);
  expect(retryTimer).toBeDefined();
  h.tabs.failCreate = false;
  const bindPromise = h.port.waitForFrame("institutional_bind_request");
  await retryTimer!.fn();
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  expect(h.frames().map((frame) => frame.type)).toContain(
    "institutional_bind_request",
  );
  const bind = await bindPromise;
  expect(bind.payload["tab_id"]).toBe(
    h.backend.store.materializations?.["job_mat_scaffold_retry"]?.tab_id,
  );
  expect(
    h.tabs
      .list()
      .filter(
        (tab) => tab.url?.includes("materialize.html#bind_0001") === true,
      ),
  ).toHaveLength(1);
});

test("institutional claim stale clears local correlation for a later offer", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  await h.port.inbound(candidateOffer("job_mat_stale"));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_claim_response",
    msg_id: "claim_response_stale",
    job_id: "job_mat_stale",
    seq: 3,
    payload: { request_id: claim.payload["request_id"], outcome: "stale" },
  });
  await Promise.resolve();
  await Promise.resolve();
  const internals = h.bridge as unknown as {
    materializationRetryTimers: Map<string, object>;
  };
  expect(h.backend.store.materializations?.["job_mat_stale"]).toBeUndefined();
  expect(internals.materializationRetryTimers.has("job_mat_stale")).toBe(false);
});

test("new institutional candidate supersedes old correlation and closes its scaffold", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
  };
  h.tabs.seed({
    id: 901,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [materializationActiveJob("job_mat_supersede")],
    workWindowID: 500,
    materializations: {
      job_mat_supersede: {
        job_id: "job_mat_supersede",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "bound",
        tab_id: 901,
      },
    },
  }));
  await h.port.inbound(candidateOffer("job_mat_supersede", "cand_0002"));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  expect(claim.payload["candidate_id"]).toBe("cand_0002");
  expect(h.backend.store.materializations?.["job_mat_supersede"]).toMatchObject(
    {
      candidate_id: "cand_0002",
      phase: "claiming",
      tab_id: -1,
    },
  );
  expect(h.tabs.removed).toContain(901);
});

// Live-smoke reproduction (2026-08-19): one institutional open left TWO papio
// surfaces for one job - the navigated tab sitting on the operator's login
// wall, plus a second scaffold announcing "Materialization binding ready" that
// could never bind. The daemon pins a re-offered candidate's expiry in memory
// (internal/browser/bridge.go:10181-10183), so a daemon restart - or its own
// lapse check at 10174 - re-offers the SAME candidate with a FRESH expiry.
// That is `sameCandidateRefresh`, which resets the correlation to
// `offered`/`tab_id: -1` and re-runs materialization against a live, already
// navigated surface: the navigated tab no longer looks like a scaffold, so a
// replacement is minted and the operator's real sign-in tab is orphaned from
// its job.
test("a re-offer of the same candidate never mints a second surface for a navigated claim", async () => {
  const jobID = "job_mat_reoffer_navigated";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
  };
  // The state after a successful open: the scaffold was navigated to the route,
  // so this tab is the operator's login wall, not a materialize.html page.
  h.tabs.seed({
    id: 950,
    url: `https://${PROVIDER_HOST}/idp/profile/SAML2/Redirect/SSO?x=1`,
    active: false,
    windowId: 500,
  });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: 950 }],
    workWindowID: 500,
    materializations: {
      [jobID]: {
        job_id: jobID,
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "navigated",
        tab_id: 950,
        route_issuance_ordinal: 1,
        effect_ordinal: 1,
      },
    },
  }));
  const createdBefore = h.tabs.created.length;

  // Same candidate, fresher expiry - what a restarted daemon sends.
  await h.port.inbound(
    candidateOffer(jobID, "cand_0001", "2030-06-01T00:00:00Z"),
  );
  for (let i = 0; i < 40; i += 1) await Promise.resolve();

  // No second surface, and the live one is still owned by its job.
  expect(h.tabs.created.length).toBe(createdBefore);
  expect(
    h.frames().filter((frame) => frame.type === "institutional_claim_request"),
  ).toEqual([]);
  expect(h.backend.store.materializations?.[jobID]).toMatchObject({
    phase: "navigated",
    tab_id: 950,
    binding_id: "bind_0001",
  });
  expect(
    h.backend.store.activeJobs.find((job) => job.job_id === jobID)?.tab_id,
  ).toBe(950);
});

// Live-smoke reproduction (2026-08-19), the other half: a leftover scaffold for
// a binding whose surface has already been navigated. reconcileMaterializationTabs
// only sees tabs still sitting at `materialize.html#<binding>`, so the real
// surface - the operator's login wall - is never among its candidates, while the
// leftover is. `candidates.find(...) ?? candidates[0]` therefore chose the
// placeholder, repointed the correlation at it, and reported IT to the daemon as
// the job's surface, orphaning the tab the operator actually has to sign in on.
test("startup reconcile keeps the navigated surface and retires a leftover scaffold", async () => {
  const jobID = "job_mat_leftover_scaffold";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: 950 }],
    workWindowID: 500,
    materializations: {
      [jobID]: {
        job_id: jobID,
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "navigated",
        tab_id: 950,
        route_issuance_ordinal: 1,
        effect_ordinal: 1,
      },
    },
  });
  // The navigated surface: the operator's institutional login wall.
  h.tabs.seed({
    id: 950,
    url: `https://${PROVIDER_HOST}/idp/profile/SAML2/Redirect/SSO`,
    active: false,
    windowId: 500,
  });
  // The leftover papio minted for the same binding and never retired.
  h.tabs.seed({
    id: 951,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  for (let i = 0; i < 40; i += 1) await Promise.resolve();

  expect(h.tabs.removed).toContain(951);
  expect(h.tabs.removed).not.toContain(950);
  expect(h.backend.store.materializations?.[jobID]).toMatchObject({
    phase: "navigated",
    tab_id: 950,
  });
  const request = h
    .frames()
    .find((frame) => frame.type === "institutional_reconcile_request");
  expect(request?.payload["bindings"]).toEqual([
    { binding_id: "bind_0001", tab_id: 950 },
  ]);
});

// Live-smoke reproduction (2026-08-19), the creator: the daemon's explicit open
// queues BOTH a materialization candidate offer (bridge.go:9866-9871) and a
// handoff_focus (bridge.go:9911). focusDaemonHandoff falls through to
// openHandoff for a URL-free candidate, and openHandoffUnlocked's
// engagement_required branch mints a LEGACY fresh handoff link - a second tab
// for the same paper, racing the scaffold materialization is already building.
// The focus frame arrives while the correlation is still pre-bind, exactly when
// job.tab_id is legitimately -1, so neither path can see the other. Verified
// live twice: one open, two papio tabs, one of them stranded at
// materialize.html for the same binding the daemon had already navigated.
test("an explicit focus never mints a legacy handoff beside an in-flight materialization", async () => {
  const jobID = "job_mat_focus_no_duplicate";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      // The live daemon's set: the legacy fresh-link mint and the claim
      // consult are both feature-gated, so a narrower list would gate the
      // very collision under test out of existence.
      features: [
        "institutional_materialization_v1",
        "effect_permit_v1",
        "handoff_link_v1",
        AUTH_CLAIM,
      ],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
  };
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    // A parked institutional job: engagement_required, no surface yet.
    activeJobs: [
      {
        ...materializationActiveJob(jobID),
        tab_id: -1,
        requires_auth: true,
        engagement_required: true,
      },
    ],
    workWindowID: 500,
  }));
  // The daemon's explicit open queues a candidate offer AND a focus frame. The
  // offer arrives first and starts the pipeline for real - hand-seeding a
  // correlation instead would leave nothing actually driving the job, which is
  // the one state where an explicit mint is still the only way to a surface.
  await h.port.inbound(candidateOffer(jobID, "cand_0001"));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  expect(claim.payload["candidate_id"]).toBe("cand_0001");
  const createdBefore = h.tabs.created.length;

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "handoff_focus",
    msg_id: "focus_dup_0001",
    job_id: jobID,
    seq: 3,
    payload: {},
  });
  for (let i = 0; i < 40; i += 1) await Promise.resolve();

  // The daemon grants the claim: its own materialization is pre-bind, so no
  // owner surface exists to navigate yet (bridge.go's §2.1.1 case 3).
  const consult = h
    .frames()
    .find((frame) => frame.type === "authentication_claim_request");
  expect(consult).toBeDefined();
  await h.port.inbound(
    claimResponse(jobID, consult?.payload["request_id"], {
      outcome: "open_new",
      authentication_claim_id: "auth_claim_dup_0001",
      gate_occurrence_id: "occ_dup_0001",
      browser_holder_generation: 1,
      lease_until: "2030-01-01T00:05:00Z",
    }),
  );
  for (let i = 0; i < 40; i += 1) await Promise.resolve();

  // Granted or not, surface creation belongs to the materialization pipeline
  // the same candidate is already driving: no legacy mint, no second tab.
  expect(
    h.frames().filter((frame) => frame.type === "handoff_link_request"),
  ).toEqual([]);
  expect(h.tabs.created.length).toBe(createdBefore);
});

// Live-smoke regression (2026-08-19): two papers were cancelled on the
// operator's own library by papio's own housekeeping. A reconcile removal of a
// surface the job still pointed at fell through onTabRemoved to the operator-
// cancel branch: `provider_outcome {outcome:"cancelled"}` + removeJobWithOffer,
// and the daemon cancelled the paper (traces in the open-defect table in
// dev/active/surface-lifecycle-plan.md). Every existing materialization test
// missed it because `materializationActiveJob` sets `tab_id: -1`, so
// `findByTab` never matched and the branch was unreachable. These two pin the
// distinction the marker draws: papio's own removal is silent, the operator's
// is still a cancellation.
test("live smoke regression: an internal removal of a surface the job still owns never reports cancellation", async () => {
  const jobID = "job_mat_no_self_cancel";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  // Driven at the chokepoint every internal removal shares (fourteen callers:
  // reconcile dedupe, superseded correlation, response-loss cleanup, …) rather
  // than through one caller. Some callers detach the job before removing and
  // are safe by accident; the invariant belongs to the removal itself, and
  // this is the seam that decides it for all of them.
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
    removeMaterializationTab: (tabID: number) => Promise<void>;
  };
  h.tabs.seed({
    id: 901,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    // The production state the existing tests could not reach:
    // materializationActiveJob sets tab_id -1, so findByTab never matched and
    // onTabRemoved's job-bearing branches were unreachable from any of them.
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: 901 }],
    workWindowID: 500,
  }));
  const framesBefore = h.port.posted.length;
  await internals.removeMaterializationTab.call(h.bridge, 901);
  expect(h.tabs.removed).toContain(901);

  // Papio removed it, so nothing is reported as an outcome and the paper lives.
  expect(
    h
      .frames()
      .slice(framesBefore)
      .filter((frame) => frame.type === "provider_outcome"),
  ).toEqual([]);
  const job = h.backend.store.activeJobs.find(
    (candidate) => candidate.job_id === jobID,
  );
  expect(job).toBeDefined();
  // Detached from the dead tab, retained for the reconcile round trip.
  expect(job?.tab_id).toBe(-1);
});

test("live smoke regression: the operator closing that same surface IS still a cancellation", async () => {
  const jobID = "job_mat_operator_cancel";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
  };
  h.tabs.seed({
    id: 902,
    url: "chrome-extension://test/dist/materialize.html#bind_0002",
    active: false,
    windowId: 500,
  });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: 902 }],
    workWindowID: 500,
  }));
  const framesBefore = h.port.posted.length;
  await h.tabs.userClose(902);
  const outcome = await h.port.waitForFrame("provider_outcome", framesBefore);
  expect(outcome.payload["outcome"]).toBe("cancelled");
  expect(
    h.backend.store.activeJobs.find((candidate) => candidate.job_id === jobID),
  ).toBeUndefined();
});

test("route update loss removes dead scaffold and next run rebinds a replacement", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const originalUpdate = h.tabs.update.bind(h.tabs);
  let failNextRoute = true;
  h.tabs.update = async (tabID, properties) => {
    if (properties.url !== undefined && failNextRoute) {
      failNextRoute = false;
      throw new Error("scaffold disappeared");
    }
    return originalUpdate(tabID, properties);
  };
  const internals = h.bridge as unknown as {
    update: (fn: (store: StoreShape) => StoreShape) => Promise<void>;
    runMaterialization: (jobID: string) => Promise<void>;
    scheduleMaterialization: (jobID: string, immediate?: boolean) => void;
    cancelledMaterializationJobs: Set<string>;
    pendingMaterializationRequests: unknown[];
  };
  h.tabs.seed({
    id: 903,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await internals.update.call(h.bridge, (store) => ({
    ...store,
    activeJobs: [materializationActiveJob("job_mat_dead_tab")],
    workWindowID: 500,
    materializations: {
      job_mat_dead_tab: {
        job_id: "job_mat_dead_tab",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "bound",
        tab_id: 903,
        route_issuance_ordinal: 7,
      },
    },
  }));
  const current = h.backend.store.materializations?.["job_mat_dead_tab"];
  expect(current).toMatchObject({
    phase: "bound",
    candidate_id: "cand_0001",
    binding_id: "bind_0001",
    tab_id: 903,
  });
  expect(Date.parse(current?.candidate_expires_at ?? "")).toBeGreaterThan(
    h.clock.now,
  );
  expect(internals.cancelledMaterializationJobs.has("job_mat_dead_tab")).toBe(
    false,
  );
  let runSettled = false;
  const firstRun = internals.runMaterialization
    .call(h.bridge, "job_mat_dead_tab")
    .finally(() => {
      runSettled = true;
    });
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  const after = h.backend.store.materializations?.["job_mat_dead_tab"];
  const frameTypes = h.frames().map((frame) => frame.type);
  if (!frameTypes.includes("institutional_route_request")) {
    throw new Error(
      JSON.stringify({
        correlation: after,
        created: h.tabs.created.length,
        queries: h.tabs.queryCount,
        pending: internals.pendingMaterializationRequests.length,
        runSettled,
        cancelled:
          internals.cancelledMaterializationJobs.has("job_mat_dead_tab"),
        features: h.backend.store.daemonFeatures,
      }),
    );
  }
  const firstRoute = h
    .frames()
    .find((frame) => frame.type === "institutional_route_request");
  if (firstRoute === undefined)
    throw new Error("institutional route request is missing");
  expect(internals.pendingMaterializationRequests.length).toBe(1);
  expect(typeof firstRoute.payload["request_id"]).toBe("string");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_route_response",
    msg_id: "route_response_dead",
    job_id: "job_mat_dead_tab",
    seq: 4,
    payload: {
      request_id: firstRoute.payload["request_id"],
      outcome: "issued",
      claim_id: "claim_0001",
      binding_id: "bind_0001",
      route_issuance_ordinal: 8,
      effect_ordinal:
        (firstRoute.payload["expected_effect_ordinal"] as number) + 1,
      institutional_request_id: firstRoute.payload["institutional_request_id"],
      url: "https://resolver.example.edu/fresh/8",
    },
  });
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  const afterRoute = h.backend.store.materializations?.["job_mat_dead_tab"];
  if (afterRoute?.phase === "bound") {
    throw new Error(
      JSON.stringify({
        correlation: afterRoute,
        pending: internals.pendingMaterializationRequests.length,
        frames: h.frames().map((frame) => frame.type),
        runSettled,
      }),
    );
  }
  await firstRun;
  expect(h.backend.store.materializations?.["job_mat_dead_tab"]).toMatchObject({
    phase: "claimed",
    tab_id: -1,
    claim_id: "claim_0001",
    binding_id: "bind_0001",
    route_issuance_ordinal: 8,
  });

  const postedBeforeReplacement = h.port.posted.length;
  internals.scheduleMaterialization.call(h.bridge, "job_mat_dead_tab", true);
  const bind = await h.port.waitForFrame(
    "institutional_bind_request",
    postedBeforeReplacement,
  );
  expect(bind.payload["tab_id"]).not.toBe(903);
  const replacementTabID = bind.payload["tab_id"];
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_bind_response",
    msg_id: "bind_response_replacement",
    job_id: "job_mat_dead_tab",
    seq: 5,
    payload: {
      request_id: bind.payload["request_id"],
      outcome: "bound",
      claim_id: "claim_0001",
      binding_id: "bind_0001",
    },
  });
  const secondRoute = await h.port.waitForFrame(
    "institutional_route_request",
    postedBeforeReplacement,
  );
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_route_response",
    msg_id: "route_response_replacement",
    job_id: "job_mat_dead_tab",
    seq: 6,
    payload: {
      request_id: secondRoute.payload["request_id"],
      outcome: "issued",
      claim_id: "claim_0001",
      binding_id: "bind_0001",
      route_issuance_ordinal: 9,
      effect_ordinal:
        (secondRoute.payload["expected_effect_ordinal"] as number) + 1,
      institutional_request_id: secondRoute.payload["institutional_request_id"],
      url: "https://resolver.example.edu/fresh/9",
    },
  });
  const navigated = await h.port.waitForFrame(
    "institutional_navigated_request",
    postedBeforeReplacement,
  );
  expect(navigated.payload["tab_id"]).toBe(replacementTabID);
});

test("restart replays a pre-permit navigating correlation with cleanup-only shape", async () => {
  const h = makeHarness({
    activeJobs: [materializationActiveJob("job_mat_legacy_restart")],
    workWindowID: 500,
    materializations: {
      job_mat_legacy_restart: {
        job_id: "job_mat_legacy_restart",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        phase: "navigating",
        tab_id: 905,
        route_issuance_ordinal: 7,
      },
    },
  });
  h.tabs.seed({
    id: 905,
    url: "https://resolver.example.edu/open",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["institutional_materialization_v1"] }),
  );
  const internals = h.bridge as unknown as {
    runMaterialization: (jobID: string) => Promise<void>;
  };
  const run = internals.runMaterialization.call(
    h.bridge,
    "job_mat_legacy_restart",
  );
  let navigation: BrowserMessage | undefined;
  for (let i = 0; i < 100 && navigation === undefined; i += 1) {
    const raw = h.port.posted.find(
      (frame) =>
        (frame as Record<string, unknown>)["type"] ===
        "institutional_navigated_request",
    );
    if (raw !== undefined)
      navigation = parseBrowserMessageWithLegacyInstitutionalNavigation(
        raw,
        true,
      );
    else await Promise.resolve();
  }
  if (navigation === undefined)
    throw new Error("legacy navigation frame is missing");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_navigated_response",
    msg_id: "legacy_restart_response",
    job_id: "job_mat_legacy_restart",
    seq: 4,
    payload: {
      request_id: navigation.payload["request_id"],
      outcome: "stale",
    },
  });
  await run;
});

test("cancelling materialization removes scaffold, retry timer, and correlation", async () => {
  const h = makeHarness({
    activeJobs: [materializationActiveJob("job_mat_cancel")],
    workWindowID: 500,
    materializations: {
      job_mat_cancel: {
        job_id: "job_mat_cancel",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        binding_id: "bind_0001",
        phase: "bound",
        tab_id: 902,
      },
    },
  });
  h.tabs.seed({
    id: 902,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  const internals = h.bridge as unknown as {
    materializationRetryTimers: Map<string, object>;
    removeJobWithOffer: (jobID: string) => Promise<void>;
  };
  internals.materializationRetryTimers.set("job_mat_cancel", {});
  await internals.removeJobWithOffer.call(h.bridge, "job_mat_cancel");
  expect(h.backend.store.materializations?.["job_mat_cancel"]).toBeUndefined();
  expect(h.tabs.removed).toContain(902);
  expect(internals.materializationRetryTimers.has("job_mat_cancel")).toBe(
    false,
  );
});
test("deferred route response from superseded candidate cannot navigate replacement", async () => {
  const h = makeHarness({
    activeJobs: [materializationActiveJob("job_mat_route_supersede")],
    workWindowID: 500,
    materializations: {
      job_mat_route_supersede: {
        job_id: "job_mat_route_supersede",
        candidate_id: "cand_0001",
        materialization_kind: "browser_tab",
        candidate_expires_at: "2030-01-01T00:00:00Z",
        claim_id: "claim_0001",
        binding_id: "bind_0001",
        browser_holder_generation: 1,
        lease_until: "2030-01-01T00:05:00Z",
        phase: "bound",
        tab_id: 904,
      },
    },
  });
  h.tabs.seed({
    id: 904,
    url: "chrome-extension://test/dist/materialize.html#bind_0001",
    active: false,
    windowId: 500,
  });
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["institutional_materialization_v1", "effect_permit_v1"],
    }),
  );
  const internals = h.bridge as unknown as {
    runMaterialization: (jobID: string) => Promise<void>;
  };
  const oldRun = internals.runMaterialization.call(
    h.bridge,
    "job_mat_route_supersede",
  );
  const oldRoute = await h.port.waitForFrame("institutional_route_request");
  await h.port.inbound(candidateOffer("job_mat_route_supersede", "cand_0002"));
  const replacementClaim = await h.port.waitForFrame(
    "institutional_claim_request",
  );
  expect(replacementClaim.payload["candidate_id"]).toBe("cand_0002");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_route_response",
    msg_id: "route_response_old_after_supersede",
    job_id: "job_mat_route_supersede",
    seq: 4,
    payload: {
      request_id: oldRoute.payload["request_id"],
      outcome: "issued",
      claim_id: "claim_0001",
      binding_id: "bind_0001",
      route_issuance_ordinal: 1,
      effect_ordinal:
        (oldRoute.payload["expected_effect_ordinal"] as number) + 1,
      institutional_request_id: oldRoute.payload["institutional_request_id"],
      url: "https://resolver.example.edu/old-route",
    },
  });
  await oldRun;
  expect(
    h.tabs.navigations.some((entry) => entry.url.includes("old-route")),
  ).toBe(false);
  expect(
    h.backend.store.materializations?.["job_mat_route_supersede"]?.candidate_id,
  ).toBe("cand_0002");
});
describe("effect_permit reconcile inbound", () => {
  function reconcileRequest(
    jobID: string | undefined,
    payload: Record<string, unknown>,
  ): unknown {
    const jobless = payload["effect_kind"] === "pdf_grab";
    return {
      protocol: "papio-browser/1",
      type: "effect_permit_reconcile_request",
      msg_id: `reconcile_${String(payload["request_id"])}`,
      ...(jobID !== undefined && !jobless ? { job_id: jobID } : {}),
      seq: 5,
      payload,
    };
  }
  function assertURLFree(frame: unknown): void {
    const raw = JSON.stringify(frame);
    expect(raw).not.toContain("https://");
    expect(raw).not.toContain("http://");
    expect(raw.toLowerCase()).not.toContain("provider_text");
    expect(raw).not.toContain("resolver.example.edu/openurl");
    expect(raw).not.toMatch(/"url"\s*:/);
    expect(raw).not.toMatch(/"path"\s*:/);
    expect(raw).not.toMatch(/"provider"/);
    expect(raw).not.toMatch(/"origin"/);
  }
  function assertResponseShape(frame: BrowserMessage): void {
    const rawPayload = frame.payload;
    if (rawPayload === null || typeof rawPayload !== "object")
      throw new Error("payload not object");
    const p: Record<string, unknown> = rawPayload as Record<string, unknown>;
    const allowed = [
      "request_id",
      "permit_id",
      "outcome",
      "dispatched",
      "download_present",
      "acknowledged",
      "tab_present",
      "detail",
    ] as const;
    for (const key of Object.keys(p)) {
      expect((allowed as readonly string[]).includes(key)).toBe(true);
    }
    expect(typeof p["request_id"]).toBe("string");
    expect(typeof p["permit_id"]).toBe("string");
    expect(typeof p["outcome"]).toBe("string");
    const outcome = p["outcome"];
    expect(
      typeof outcome === "string" &&
        ["recorded", "settled", "stale", "duplicate", "error"].includes(
          outcome,
        ),
    ).toBe(true);
    for (const k of [
      "dispatched",
      "download_present",
      "acknowledged",
      "tab_present",
    ] as const) {
      expect(typeof p[k]).toBe("boolean");
    }
    if ("detail" in p) {
      const detail = p["detail"];
      expect(typeof detail).toBe("string");
      if (typeof detail === "string") {
        expect(detail.length).toBeLessThanOrEqual(1000);
        expect(detail.includes("\u0000")).toBe(false);
      }
    }
    assertURLFree(frame);
  }

  test("generic no-dispatch restart emits all-false recorded response deterministically and URL-free", async () => {
    const jobID = "job_generic_reconcile_no_dispatch";
    const driveAttemptID = "drive-attempt-generic-no-dispatch";
    const seed: StoreShape = {
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 101,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
          access_mode: "delegated",
          // Persisted pre-dispatch intent is not evidence that the browser
          // allocated an irreversible download effect.
          download_initiated: true,
          generic_drive_epoch: {
            drive_attempt_id: driveAttemptID,
            ordinal: 0,
            strategy: "generic",
            revision: "rev-1",
            attempt_count: 0,
          },
        },
      ],
    };
    const h = makeHarness(seed);
    h.tabs.seed({
      id: 101,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: { outcome: "started" } };
    });
    const payload = {
      request_id: "request-generic-no-dispatch-0001",
      permit_id: "permit-generic-no-dispatch-0001",
      effect_kind: "generic_drive",
      drive_attempt_id: driveAttemptID,
      ordinal: 0,
      strategy: "generic",
      revision: "rev-1",
    };
    await h.port.inbound(reconcileRequest(jobID, payload));
    // second inbound proves the serialized queue was not deadlocked by awaiting requestNative
    await h.port.inbound(
      reconcileRequest(jobID, {
        ...payload,
        request_id: "request-generic-no-dispatch-0002",
        permit_id: "permit-generic-no-dispatch-0002",
      }),
    );
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames).toHaveLength(2);
    for (const frame of frames) {
      expect(frame.job_id).toBe(jobID);
      expect(frame.payload["dispatched"]).toBe(false);
      expect(frame.payload["download_present"]).toBe(false);
      expect(frame.payload["acknowledged"]).toBe(false);
      expect(frame.payload["tab_present"]).toBe(false);
      expect(frame.payload["outcome"]).toBe("recorded");
      assertResponseShape(frame);
    }
    expect(spy).toHaveLength(0);
    const persisted = JSON.parse(JSON.stringify(h.backend.store)) as StoreShape;
    const restarted = makeHarness(persisted);
    restarted.tabs.seed({
      id: 101,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    await restarted.bridge.start();
    const spy2: unknown[][] = [];
    Reflect.set(
      restarted.bridge,
      "requestNative",
      async (...args: unknown[]) => {
        spy2.push(args);
        return { kind: "response", payload: {} };
      },
    );
    await restarted.port.inbound(reconcileRequest(jobID, payload));
    const restartedFrames = restarted
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(restartedFrames).toHaveLength(1);
    expect(restartedFrames[0]!.payload).toEqual(frames[0]!.payload);
    assertResponseShape(restartedFrames[0]!);
    expect(spy2).toHaveLength(0);
  });

  test("generic exact in-flight download reports dispatched and download_present true without deadlocking and without requestNative", async () => {
    const jobID = "job_generic_reconcile_download";
    const driveAttemptID = "drive-attempt-generic-download";
    const downloadID = 901;
    const seed: StoreShape = {
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 102,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
          access_mode: "delegated",
          download_initiated: true,
          generic_drive_epoch: {
            drive_attempt_id: driveAttemptID,
            ordinal: 0,
            strategy: "generic",
            revision: "rev-1",
            attempt_count: 0,
            in_flight_download_id: downloadID,
          },
        },
      ],
    };
    const h = makeHarness(seed);
    h.tabs.seed({
      id: 102,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    h.downloads.items.set(downloadID, {
      id: downloadID,
      url: "https://www.jstor.org/download/paper.pdf",
      state: "in_progress",
    });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const payload = {
      request_id: "request-generic-download-0001",
      permit_id: "permit-generic-download-0001",
      effect_kind: "generic_drive",
      drive_attempt_id: driveAttemptID,
      ordinal: 0,
      strategy: "generic",
      revision: "rev-1",
    };
    const before = h.port.posted.length;
    await h.port.inbound(reconcileRequest(jobID, payload));
    // queue a second reconcile on same tuple but different request_id to prove FIFO liveness
    await h.port.inbound(
      reconcileRequest(jobID, {
        ...payload,
        request_id: "request-generic-download-0002",
        permit_id: "permit-generic-download-0002",
      }),
    );
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames).toHaveLength(2);
    for (const frame of frames) {
      expect(frame.payload["dispatched"]).toBe(true);
      expect(frame.payload["download_present"]).toBe(true);
      expect(frame.payload["acknowledged"]).toBe(false);
      expect(frame.payload["tab_present"]).toBe(false);
      expect(frame.payload["outcome"]).toBe("recorded");
      assertResponseShape(frame);
    }
    expect(spy).toHaveLength(0);
    // ensure no URL leaked even though download URL exists only in Downloads map, not in payload
    for (const frame of frames) assertURLFree(frame);
    expect(before).toBeLessThan(h.port.posted.length);
  });

  test("generic missing local tuple records unknown completion with no URL", async () => {
    const jobID = "job_generic_reconcile_wrong";
    const driveAttemptID = "drive-attempt-generic-wrong";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 103,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
          access_mode: "delegated",
          generic_drive_epoch: {
            drive_attempt_id: driveAttemptID,
            ordinal: 0,
            strategy: "generic",
            revision: "rev-1",
            attempt_count: 0,
          },
        },
      ],
    });
    h.tabs.seed({
      id: 103,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    h.downloads.items.set(901, { id: 901, state: "in_progress" });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const wrong = {
      request_id: "request-generic-wrong-0001",
      permit_id: "permit-generic-wrong-0001",
      effect_kind: "generic_drive",
      drive_attempt_id: driveAttemptID,
      ordinal: 1,
      strategy: "generic",
      revision: "rev-1",
    };
    await h.port.inbound(reconcileRequest(jobID, wrong));
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames).toHaveLength(1);
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    expect(frames[0]!.payload["dispatched"]).toBe(false);
    expect(frames[0]!.payload["download_present"]).toBe(false);
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[0]!);
    expect(spy).toHaveLength(0);
  });

  test("direct_get exact tuple reports evidence and missing local tuple records unknown completion", async () => {
    const jobID = "job_direct_reconcile";
    const driveAttemptID = "drive-attempt-direct-reconcile";
    const downloadID = 902;
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 104,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
          access_mode: "delegated",
          download_initiated: true,
          direct_terminal: true,
          drive_epoch: {
            drive_attempt_id: driveAttemptID,
            ordinal: 2,
            strategy: "direct",
            route_revision: "rev-direct-1",
            attempt_count: 0,
            in_flight_download_id: downloadID,
          } as ProviderDriveEpoch,
        } as ActiveJob,
      ],
    });
    h.tabs.seed({
      id: 104,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    h.downloads.items.set(downloadID, { id: downloadID, state: "in_progress" });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const exact = {
      request_id: "request-direct-exact-0001",
      permit_id: "permit-direct-exact-0001",
      effect_kind: "direct_get",
      drive_attempt_id: driveAttemptID,
      ordinal: 2,
      strategy: "direct_get",
      revision: "rev-direct-1",
    };
    await h.port.inbound(reconcileRequest(jobID, exact));
    let frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    expect(frames[0]!.payload["dispatched"]).toBe(true);
    expect(frames[0]!.payload["download_present"]).toBe(true);
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[0]!);
    const wrong = {
      ...exact,
      request_id: "request-direct-wrong-0001",
      permit_id: "permit-direct-wrong-0001",
      revision: "rev-wrong",
    };
    await h.port.inbound(reconcileRequest(jobID, wrong));
    frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[1]!.payload["outcome"]).toBe("recorded");
    expect(frames[1]!.payload["dispatched"]).toBe(false);
    expect(frames[1]!.payload["download_present"]).toBe(false);
    assertResponseShape(frames[1]!);
    expect(spy).toHaveLength(0);
  });

  test("pdf_grab exact correlation reports browser evidence; missing correlation records unknown completion", async () => {
    const jobID = "job_pdf_grab_reconcile";
    const grabID = "grab-reconcile-0001";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: -1,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
        },
      ],
    });
    await h.bridge.start();
    const corr = {
      scanID: "scan-pdf-0001",
      tabID: 201,
      downloadID: 903,
      steeringPath: "papio/grabs/grabpath/",
      state: "quarantined",
    } as PdfGrabCorrelation;
    (
      h.bridge as unknown as {
        pdfGrabCorrelations: Map<string, PdfGrabCorrelation>;
      }
    ).pdfGrabCorrelations.set(grabID, corr);
    h.tabs.seed({
      id: 201,
      url: "https://example.com/page",
      active: false,
      windowId: 500,
    });
    h.downloads.items.set(903, { id: 903, state: "in_progress" });
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const exact = {
      request_id: "request-pdf-exact-0001",
      permit_id: "permit-pdf-exact-0001",
      effect_kind: "pdf_grab",
      grab_id: grabID,
    };
    await h.port.inbound(reconcileRequest(undefined, exact));
    let frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[0]!.payload["dispatched"]).toBe(true);
    expect(frames[0]!.payload["download_present"]).toBe(true);
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(true);
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    assertResponseShape(frames[0]!);
    const unknown = {
      request_id: "request-pdf-unknown-0001",
      permit_id: "permit-pdf-unknown-0001",
      effect_kind: "pdf_grab",
      grab_id: "grab-unknown-9999",
    };
    await h.port.inbound(reconcileRequest(undefined, unknown));
    frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[1]!.payload["outcome"]).toBe("recorded");
    expect(frames[1]!.payload["dispatched"]).toBe(false);
    expect(frames[1]!.payload["download_present"]).toBe(false);
    expect(frames[1]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[1]!);
    expect(spy).toHaveLength(0);
  });

  test("institutional bound reports acknowledged false with live tab, navigated reports acknowledged true", async () => {
    const jobID = "job_institutional_reconcile";
    const claimID = "claim-reconcile-0001";
    const bindingID = "binding-reconcile-0001";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: -1,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
        },
      ],
      materializations: {
        [jobID]: {
          job_id: jobID,
          candidate_id: "cand-0001",
          materialization_kind: "browser_tab",
          candidate_expires_at: "2030-01-01T00:00:00Z",
          claim_id: claimID,
          binding_id: bindingID,
          browser_holder_generation: 1,
          lease_until: "2030-01-01T00:05:00Z",
          phase: "bound",
          tab_id: 301,
        },
      },
    });
    h.tabs.seed({
      id: 301,
      url: "chrome-extension://test/dist/materialize.html#bind_0001",
      active: false,
      windowId: 500,
    });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const boundPayload = {
      request_id: "request-inst-bound-0001",
      permit_id: "permit-inst-bound-0001",
      effect_kind: "institutional",
      claim_id: claimID,
      binding_id: bindingID,
      effect_ordinal: 1,
      institutional_request_id: "inst-req-0001",
      tab_id: 301,
    };
    await h.port.inbound(reconcileRequest(jobID, boundPayload));
    let frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(true);
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    assertResponseShape(frames[0]!);
    // navigated phase
    await (
      h.bridge as unknown as {
        update: (fn: (s: StoreShape) => StoreShape) => Promise<void>;
      }
    ).update.call(h.bridge, (s) => ({
      ...s,
      materializations: {
        ...s.materializations,
        [jobID]: {
          ...s.materializations![jobID]!,
          phase: "navigated" as const,
        },
      },
    }));
    const navigatedPayload = {
      ...boundPayload,
      request_id: "request-inst-nav-0001",
      permit_id: "permit-inst-nav-0001",
      effect_ordinal: 2,
      institutional_request_id: "inst-req-0002",
    };
    await h.port.inbound(reconcileRequest(jobID, navigatedPayload));
    frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[1]!.payload["acknowledged"]).toBe(true);
    expect(frames[1]!.payload["tab_present"]).toBe(true);
    expect(frames[1]!.payload["outcome"]).toBe("recorded");
    assertResponseShape(frames[1]!);
    // closed tab => tab_present false
    await h.tabs.remove(301);
    await h.port.inbound(
      reconcileRequest(jobID, {
        ...boundPayload,
        request_id: "request-inst-closed-0001",
        permit_id: "permit-inst-closed-0001",
      }),
    );
    frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[2]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[2]!);
    expect(spy).toHaveLength(0);
  });

  test("institutional mismatched local claim records unknown completion", async () => {
    const jobID = "job_inst_wrong";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: -1,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
        },
      ],
      materializations: {
        [jobID]: {
          job_id: jobID,
          candidate_id: "cand-0001",
          materialization_kind: "browser_tab",
          candidate_expires_at: "2030-01-01T00:00:00Z",
          claim_id: "claim-correct",
          binding_id: "binding-correct",
          browser_holder_generation: 1,
          lease_until: "2030-01-01T00:05:00Z",
          phase: "bound",
          tab_id: 311,
        },
      },
    });
    h.tabs.seed({
      id: 311,
      url: "chrome-extension://test/dist/materialize.html#bind",
      active: false,
      windowId: 500,
    });
    await h.bridge.start();
    const payload = {
      request_id: "request-inst-wrong-0001",
      permit_id: "permit-inst-wrong-0001",
      effect_kind: "institutional",
      claim_id: "claim-wrong",
      binding_id: "binding-correct",
      effect_ordinal: 1,
      institutional_request_id: "inst-req-wrong",
      tab_id: 311,
    };
    await h.port.inbound(reconcileRequest(jobID, payload));
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    expect(frames[0]!.payload["dispatched"]).toBe(false);
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[0]!);
  });

  test("terms without persisted acknowledgement records unknown completion URL-free", async () => {
    const jobID = "job_terms_reconcile";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: -1,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
        },
      ],
    });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const payload = {
      request_id: "request-terms-0001",
      permit_id: "permit-terms-0001",
      effect_kind: "terms",
      terms_occurrence_id: "terms-occurrence-0001",
    };
    await h.port.inbound(reconcileRequest(jobID, payload));
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames[0]!.payload["outcome"]).toBe("recorded");
    expect(frames[0]!.payload["dispatched"]).toBe(false);
    expect(frames[0]!.payload["download_present"]).toBe(false);
    expect(frames[0]!.payload["acknowledged"]).toBe(false);
    expect(frames[0]!.payload["tab_present"]).toBe(false);
    assertResponseShape(frames[0]!);
    expect(spy).toHaveLength(0);
    // deterministic across restart
    const persisted = JSON.parse(JSON.stringify(h.backend.store)) as StoreShape;
    const restarted = makeHarness(persisted);
    await restarted.bridge.start();
    await restarted.port.inbound(reconcileRequest(jobID, payload));
    const restartedFrames = restarted
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(restartedFrames[0]!.payload).toEqual(frames[0]!.payload);
  });

  test("reconcile response never carries URL/path/provider and deadlock-free concurrent inbound", async () => {
    const jobID = "job_reconcile_privacy";
    const driveAttemptID = "drive-attempt-privacy-0001";
    const h = makeHarness({
      ...emptyStore(),
      activeJobs: [
        {
          job_id: jobID,
          tab_id: 401,
          offered_at: 1_700_000_000_000,
          expires_at: 1_900_000_000_000,
          status: "accepted",
          provider_hosts: [PROVIDER_HOST],
          access_mode: "delegated",
          generic_drive_epoch: {
            drive_attempt_id: driveAttemptID,
            ordinal: 0,
            strategy: "generic",
            revision: "rev-privacy-1",
            attempt_count: 0,
          },
        },
      ],
    });
    h.tabs.seed({
      id: 401,
      url: `https://${PROVIDER_HOST}/article`,
      active: false,
      windowId: 500,
    });
    await h.bridge.start();
    const spy: unknown[][] = [];
    Reflect.set(h.bridge, "requestNative", async (...args: unknown[]) => {
      spy.push(args);
      return { kind: "response", payload: {} };
    });
    const payload = {
      request_id: "request-privacy-0001",
      permit_id: "permit-privacy-0001",
      effect_kind: "generic_drive",
      drive_attempt_id: driveAttemptID,
      ordinal: 0,
      strategy: "generic",
      revision: "rev-privacy-1",
    };
    // fire reconcile and immediately queue a hello_ack behind it to prove FIFO liveness
    const p1 = h.port.inbound(reconcileRequest(jobID, payload));
    const p2 = h.port.inbound(helloAck({ features: ["effect_permit_v1"] }));
    await Promise.all([p1, p2]);
    const frames = h
      .frames()
      .filter((f) => f.type === "effect_permit_reconcile_response");
    expect(frames).toHaveLength(1);
    assertResponseShape(frames[0]!);
    // hello_ack must have been processed (daemonFeatures set)
    expect(h.backend.store.daemonFeatures).toContain("effect_permit_v1");
    expect(spy).toHaveLength(0);
  });
});

// Slice 1 (surface-lifecycle-plan.md): navigation-error ordering ahead of
// generic auth-wall detection, and the lifecycle.ts harness seams
// (restartWorker/simulateExtensionUpdate/expectLedgerRetains).
test("Slice 1: a navigation error on a managed handoff tab never charges an auth attempt", async () => {
  const idpURL = "https://idp.example.edu/sso";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_naverr_managed"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  await h.webNavigation.emitError(tabID);
  // The failed provider load settles at the IdP URL as an error document —
  // Chrome delivers that as a single "complete" landing, with no separate
  // "loading" transition (that would itself model a fresh operator
  // navigation and prematurely start authentication before the marker is
  // ever consulted). Mirrors the already-covered recovery sibling below.
  await h.tabs.completeNavigation(tabID, idpURL);

  expect(h.frames().some((f) => f.type === "auth_pending")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
  expect(h.backend.store.activeJobs[0]?.challenge_blocked).toBeUndefined();

  // Ordering pin: the same IdP landing, without a preceding navigation
  // error, still starts human authentication normally.
  const control = makeHarness();
  await control.bridge.start();
  await control.port.inbound(jobOffer("job_naverr_control"));
  const controlTabID = control.backend.store.activeJobs[0]?.tab_id ?? -1;
  await control.tabs.userNavigate(controlTabID, idpURL);
  await control.tabs.completeNavigation(controlTabID, idpURL);
  expect(
    control.frames().filter((f) => f.type === "auth_pending"),
  ).toHaveLength(1);
  expect(control.backend.store.activeJobs[0]?.status).toBe("auth_pending");
});

test("Slice 1: the navigation-error marker clears on the next successful navigation", async () => {
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_naverr_clears"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;

  await h.webNavigation.emitError(tabID);
  // The tab recovers and lands back on the provider — a genuinely
  // successful navigation, not the erroring document.
  await h.tabs.completeNavigation(
    tabID,
    `https://${PROVIDER_HOST}/stable/recovered`,
  );
  expect(h.backend.store.activeJobs[0]?.status).not.toBe("auth_pending");

  // A later, unrelated IdP landing on the same tab classifies normally —
  // proof the marker did not survive the successful landing above.
  const idpURL = "https://idp.example.edu/sso";
  await h.tabs.userNavigate(tabID, idpURL);
  await h.tabs.completeNavigation(tabID, idpURL);
  expect(
    h.frames().filter((f) => f.type === "auth_pending"),
  ).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
});

test("Slice 1: simulateExtensionUpdate reproduces the session-storage-clears tab-group reload", async () => {
  const h = makeHarness(undefined, {
    tabGroups: true,
    handoffSurface: "tab-group",
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_navlife_tg_a"));
  const groupID = h.backend.store.handoffGroupID;
  expect(groupID).toBeDefined();
  expect(h.tabGroups?.live.size).toBe(1);
  await h.tabGroups!.update(groupID!, {
    title: "papio — A paper still awaiting institutional access",
    collapsed: false,
  });

  // Simulate an extension reload: chrome.storage.session is wiped while the
  // physical group remains labeled with the paper that needs attention —
  // same scenario as the hand-rolled reload above, rebuilt on the helper to
  // prove it is truthful (assertions kept identical).
  const updated = simulateExtensionUpdate(h);
  await updated.bridge.start();
  await updated.port.inbound(jobOffer("job_navlife_tg_b"));

  expect(h.tabGroups?.live.size).toBe(1);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabs.grouped).toEqual([
    { tabIds: [100] },
    { tabIds: [101], groupId: groupID! },
  ]);
  expect(h.tabGroups?.live.get(groupID!)?.title).toBe(
    "papio — A paper still awaiting institutional access",
  );
});

test("Slice 1: restartWorker detaches the dead bridge's own listeners — a tab event after restart is handled by the new bridge only", async () => {
  const jobID = "job_restart_isolation";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(jobOffer(jobID));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  const restarted = restartWorker(h);
  await restarted.bridge.start();
  await h.tabs.userClose(tabID);

  // Exactly one provider_outcome/cancelled across every port this harness
  // ever created: the dead bridge's own tabs.onRemoved listener must never
  // fire again once a fresh Bridge has taken over the same fakes — a
  // second, stale listener would double-report the same close (and could
  // stomp the new bridge's own save with a dead in-memory snapshot).
  const cancelled = restarted
    .frames()
    .filter((frame) => frame.type === "provider_outcome");
  expect(cancelled).toHaveLength(1);
  expect(restarted.backend.store.activeJobs).toEqual([]);
});

test("Slice 1: restartWorker keeps the durable tab ledger across a worker restart, but a claim-gate park's offer URL is worker-local only and needs the daemon's re-offer to recover", async () => {
  const h = makeHarness({
    ...emptyStore(),
    authEvidenceByOrigin: { "https://resolver.example.edu": 1_700_000_000_000 },
  });
  const ledger = installManagedTabLedger(h, {});
  const offer = jobOffer("job_navlife_gate_restart") as {
    payload: Record<string, unknown>;
  };
  offer.payload["requires_auth"] = true;
  await h.bridge.start();
  await h.port.inbound(helloAck());
  await h.port.inbound(offer);
  const internals = h.bridge as unknown as { offerURLs: Map<string, string> };
  expect(internals.offerURLs.get("job_navlife_gate_restart")).toBe(OPENURL);
  expect(
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_navlife_gate_restart",
    ),
  ).toMatchObject({ tab_id: -1, status: "queued", engagement_required: true });

  // A second, ordinary handoff proves the durable ledger survives the
  // restart.
  await h.port.inbound(jobOffer("job_navlife_ledger_restart"));
  const ledgeredTabID =
    h.backend.store.activeJobs.find(
      (job) => job.job_id === "job_navlife_ledger_restart",
    )?.tab_id ?? -1;
  expect(ledgeredTabID).toBeGreaterThanOrEqual(0);
  expectLedgerRetains(ledger.current(), ledgeredTabID);
  // Free the single governor slot this ordinary handoff still holds: the
  // claim-gate job's explicit open below must not be blocked by an
  // unrelated handoff's live tab. parkHandoffForManual's own restart-safety
  // contract (see its doc comment) is exactly this — releasing the drive
  // while retaining tab_id, so reconciliation on restart does not
  // re-consume the slot for a tab that is no longer actively driving.
  await h.bridge.parkHandoffForManual("job_navlife_ledger_restart");
  const createdBeforeRestart = h.tabs.created.length;

  const restarted = restartWorker(h);
  await restarted.bridge.start();
  await restarted.port.inbound(helloAck());
  // The claim-gate job's record survives (durable, URL-free managed
  // state); its offer URL — worker-local only — does not.
  const restartedInternals = restarted.bridge as unknown as {
    offerURLs: Map<string, string>;
  };
  expect(restartedInternals.offerURLs.has("job_navlife_gate_restart")).toBe(
    false,
  );
  expect(
    (await restarted.bridge.openHandoff("job_navlife_gate_restart")).ok,
  ).toBe(false);
  expect(restarted.tabs.created.length).toBe(createdBeforeRestart);

  // The daemon still tracks the job and naturally re-offers it on
  // reconnect; that re-offer — never stored state — is what recovers it.
  await restarted.port.inbound(offer);
  const opened = await restarted.bridge.openHandoff(
    "job_navlife_gate_restart",
  );
  expect(opened.ok).toBe(true);
  expect(restarted.tabs.created.slice(createdBeforeRestart)).toEqual([
    { url: OPENURL, active: true },
  ]);
  expectLedgerRetains(ledger.current(), ledgeredTabID);
});

// Slice 2b: durable identity, adoption, and the close transaction
// (surface-lifecycle-plan.md lines 271-303; claim-observation-protocol.md
// §2.3/§4.3). Helpers build a scaffold tab already recorded as an owned
// SurfaceBirthRecord under the bridge's own browser_epoch, with a known
// daemon holder generation, so closeOwnedSurface's local eligibility check
// passes and only the wire round trip is exercised per test.
async function seedOwnedScaffold(
  h: Harness,
  opts: { active?: boolean; pinned?: boolean; windowId?: number } = {},
): Promise<{ tabID: number; bindingID: string }> {
  if (h.deps.tabLedger === undefined) installManagedTabLedger(h, {});
  await h.port.inbound(helloAck({ features: ["surface_close_v1"] }));
  const windowId = opts.windowId ?? 1;
  const tab = await h.tabs.create({
    url: "https://materialize.example/scaffold",
    active: opts.active ?? false,
    windowId,
  });
  const tabID = tab.id!;
  if (opts.pinned === true) h.tabs.patch(tabID, { pinned: true });
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    browserEpoch: string | undefined;
    lastKnownBrowserHolderGeneration: number | undefined;
    update: (fn: (s: StoreShape) => StoreShape) => Promise<void>;
    closeOwnedSurface: (
      tabID: number,
      disposition:
        | "scaffold_idle"
        | "materialization_settled"
        | "claim_abandoned"
        | "job_inactive",
    ) => Promise<{ closed: boolean }>;
  };
  const bindingID = "binding-owned-0001";
  internals.tabLedgerCache = {
    [String(tabID)]: fakeBirthRecord({
      binding_id: bindingID,
      tab_hint: tabID,
      browser_epoch: internals.browserEpoch ?? "test-epoch",
    }),
  };
  internals.lastKnownBrowserHolderGeneration = 1;
  await internals.update((s) => ({ ...s, workWindowID: windowId }));
  return { tabID, bindingID };
}
function closeInternals(h: Harness): {
  closeOwnedSurface: (
    tabID: number,
    disposition:
      | "scaffold_idle"
      | "materialization_settled"
      | "claim_abandoned"
      | "job_inactive",
  ) => Promise<{ closed: boolean }>;
  tabLedgerCache: Record<string, SurfaceBirthRecord>;
} {
  return h.bridge as unknown as {
    closeOwnedSurface: (
      tabID: number,
      disposition:
        | "scaffold_idle"
        | "materialization_settled"
        | "claim_abandoned"
        | "job_inactive",
    ) => Promise<{ closed: boolean }>;
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
  };
}

test("Slice 2b: close transaction happy path authorizes, tombstones, removes, and suppresses cancellation", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const closing = closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle");
  const request = await h.port.waitForFrame("surface_close_request");
  expect(request.payload["binding_id"]).toBe(bindingID);
  expect(request.payload["disposition"]).toBe("scaffold_idle");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-0001",
      nonce: "nonce-0001",
      browser_holder_generation: 1,
    }),
  );
  await expect(closing).resolves.toEqual({ closed: true });
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
  // A daemon-authorized close is not an operator cancel: no provider_outcome.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("job removal retires its inactive owned surface through daemon authorization", async () => {
  const jobID = "job_inactive_close_0001";
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    update: (fn: (s: StoreShape) => StoreShape) => Promise<void>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: jobID,
  };
  await internals.update((store) => ({
    ...store,
    activeJobs: [{ ...materializationActiveJob(jobID), tab_id: tabID }],
  }));

  // `cancel` is an inbound native frame. The close request must be launched
  // off-chain: awaiting its response inside this handler would deadlock the
  // serialized inbound FIFO against the response itself.
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "cancel",
    msg_id: "cancel-inactive-0001",
    job_id: jobID,
    seq: 10,
    payload: {},
  });
  expect(findByJob(h.backend.store, jobID)).toBeUndefined();
  const request = await h.port.waitForFrame("surface_close_request");
  expect(request.payload).toMatchObject({
    binding_id: bindingID,
    disposition: "job_inactive",
  });
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-job-inactive-0001",
      nonce: "nonce-job-inactive-0001",
      browser_holder_generation: 1,
    }),
  );
  for (let i = 0; i < 20; i += 1) await Promise.resolve();

  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
  // Papio retired it; this is not the operator cancelling the paper.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("a durable cancel retires a ledger-only surface after browser-local job state is gone", async () => {
  const jobID = "job_ledger_only_cancel_0001";
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  await h.port.inbound(
    helloAck({
      features: [
        "surface_close_v1",
        "institutional_authentication_claim_v1",
      ],
    }),
  );
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
  };
  internals.tabLedgerCache[String(tabID)] = {
    ...internals.tabLedgerCache[String(tabID)]!,
    binding_id: bindingID,
    job_id: jobID,
    claim: {
      authentication_claim_id: "auth-claim-ledger-only-0001",
      gate_occurrence_id: "gate-occ-ledger-only-0001",
      browser_holder_generation: 1,
    },
  };
  expect(findByJob(h.backend.store, jobID)).toBeUndefined();

  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "cancel",
    msg_id: "cancel-ledger-only-0001",
    job_id: jobID,
    seq: 11,
    payload: {},
  });
  const request = await h.port.waitForFrame("surface_close_request");
  expect(request.payload).toMatchObject({
    binding_id: bindingID,
    disposition: "job_inactive",
  });
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-ledger-only-0001",
      nonce: "nonce-ledger-only-0001",
      browser_holder_generation: 1,
    }),
  );
  const observation = await h.port.waitForFrame("claim_observation");
  expect(observation.job_id).toBe(jobID);
  expect(observation.payload).toMatchObject({
    binding_id: bindingID,
    event_kind: "owner_closed",
  });
  await h.port.inbound(
    observationAck(jobID, observation.payload["request_id"], {
      outcome: "applied",
      gate_occurrence_id: "gate-occ-ledger-only-0001",
      browser_holder_generation: 1,
      lease_until: "2026-08-20T04:30:00Z",
    }),
  );
  expect(h.tabs.removed).toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("Slice 2b: a non-authorized outcome retains the surface", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID } = await seedOwnedScaffold(h);
  const closing = closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle");
  const request = await h.port.waitForFrame("surface_close_request");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "not_eligible",
    }),
  );
  await expect(closing).resolves.toEqual({ closed: false });
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeDefined();
});

test("Slice 2b: an operator activation cedes the surface before any close is requested", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  await h.tabs.userActivate(tabID);
  // Causal cession: the takeover is recorded at the activation papio did not
  // cause, so the close is refused locally and no one-use authorization is
  // spent proving what the extension already knows.
  const record = closeInternals(h).tabLedgerCache[String(tabID)];
  expect(record?.ceded).toBe(true);
  expect(record?.binding_id).toBe(bindingID);
  expect(record?.pending_close).toBeUndefined();
  await expect(
    closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle"),
  ).resolves.toEqual({ closed: false });
  expect(
    h.frames().filter((frame) => frame.type === "surface_close_request"),
  ).toHaveLength(0);
  expect(h.tabs.removed).not.toContain(tabID);
});

test("Review round finding 3: an operator activation landing inside the close window (after consumeCloseTombstone's own check, strictly before tabs.remove is issued) cedes instead of closing", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const rawGet = h.tabs.get.bind(h.tabs);
  let getCalls = 0;
  h.deps.tabs.get = async (id) => {
    const snapshot = await rawGet(id);
    if (id === tabID) {
      getCalls += 1;
      // Call #1 is consumeCloseTombstone's own required check (sees
      // inactive, proceeds). Call #2 is closeOwnedTab's own fresh get,
      // issued immediately after it captures the touch epoch at entry.
      // Land a genuine operator activation strictly inside that async
      // round trip, then let it revert before the snapshot is handed
      // back — so the returned tab still reads inactive, exactly what a
      // single before/after tabs.get cannot see, and exactly why the
      // fix compares a touch epoch instead of trusting one snapshot.
      if (getCalls === 2) {
        await h.tabs.userActivate(tabID);
        h.tabs.patch(tabID, { active: false });
      }
    }
    return snapshot;
  };
  const closing = closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle");
  const request = await h.port.waitForFrame("surface_close_request");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-toctou-0001",
      nonce: "nonce-toctou-0001",
      browser_holder_generation: 1,
    }),
  );
  // The cession, not merely an earlier activity check the operator's
  // later touch could still outrun.
  await expect(closing).resolves.toEqual({ closed: false });
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeDefined();
  const record = closeInternals(h).tabLedgerCache[String(tabID)];
  expect(record?.ceded).toBe(true);
  expect(record?.binding_id).toBe(bindingID);
  expect(record?.pending_close).toBeUndefined();
});

test("Scenario 6: papio's own focus_owner activation leaves no cession trace, but a genuine operator activation cedes and that survives a worker restart", async () => {
  // Part (a): papio-initiated focus_owner (the daemon routing an explicit
  // dependent to an already-live owner tab) activates that tab itself —
  // this must never be conflated with operator engagement. A later
  // authorized close on the SAME tab still runs the normal wire round trip
  // (never refused as ceded), proving no cession trace was left behind.
  const ownerJobID = "job_scenario6_owner";
  const ownerCandidateID = "cand_scenario6_owner01";
  const dependentJobID = "job_scenario6_dependent";
  const dependentCandidateID = "cand_scenario6_dep001";
  const h = makeHarness({
    ...emptyStore(),
    activeJobs: [coldClaimJob(ownerJobID), coldClaimJob(dependentJobID)],
  });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({
      features: ["handoff_link_v1", AUTH_CLAIM, "surface_close_v1"],
    }),
  );
  await seedClaimCandidate(h, ownerJobID, ownerCandidateID);
  const ownerTabID = await openClaimWithNewSurface(
    h,
    ownerJobID,
    ownerCandidateID,
    `https://${PROVIDER_HOST}/fresh?s6=owner`,
  );

  await seedClaimCandidate(h, dependentJobID, dependentCandidateID);
  const dependentFramesBefore = h.port.posted.length;
  const dependentOpening = h.bridge.openHandoff(dependentJobID);
  const dependentClaim = await h.port.waitForFrame(
    "authentication_claim_request",
    dependentFramesBefore,
  );
  await h.port.inbound(
    claimResponse(dependentJobID, dependentClaim.payload["request_id"], {
      outcome: "focus_owner",
      authentication_claim_id: `claim_${dependentCandidateID}`,
      browser_holder_generation: 1,
      gate_occurrence_id: `occ_${dependentCandidateID}`,
      lease_until: "2030-01-01T00:00:00Z",
      owner_binding_id: "bind_s6_owner01",
      owner_tab_hint: ownerTabID,
    }),
  );
  await expect(dependentOpening).resolves.toEqual({ ok: true, opened: true });
  expect(
    h.tabs.updates.some(
      (update) =>
        update.id === ownerTabID && update.properties.active === true,
    ),
  ).toBe(true);

  // No cession trace: papio's own focus_owner-driven activation never
  // touches the tab ledger's `ceded` field by itself — that field is only
  // ever written inside a close attempt's own tab.active check (the
  // structural discriminator gap: focusClaimOwnerTab's tabs.update
  // (active: true) and a genuine operator click are indistinguishable at
  // that later check — out of scope here). Proven by inspecting the
  // ledger record directly, before any close is ever attempted against
  // this tab.
  const internals = h.bridge as unknown as {
    closeOwnedSurface: (
      tabID: number,
      disposition:
        | "scaffold_idle"
        | "materialization_settled"
        | "claim_abandoned"
        | "job_inactive",
    ) => Promise<{ closed: boolean }>;
    snapshotTabLedger: () => Promise<Record<string, SurfaceBirthRecord>>;
  };
  const ledgerAfterFocus = await internals.snapshotTabLedger();
  expect(ledgerAfterFocus[String(ownerTabID)]?.ceded).toBeUndefined();

  // Part (b): papio's own focus leaves the surface retirable, which is the
  // half the discriminator gap used to forfeit — the cession it wrote made
  // every tab papio focused permanently unretirable. A fresh harness with no
  // live job owns the surface, so the close can reach the wire.
  const h1 = makeHarness(undefined, { windows: true });
  await h1.bridge.start();
  const { tabID: focusedTabID } = await seedOwnedScaffold(h1);
  const focusInternals = h1.bridge as unknown as {
    focusOwnedTab: (tabID: number) => Promise<void>;
  };
  await focusInternals.focusOwnedTab(focusedTabID);
  expect(
    closeInternals(h1).tabLedgerCache[String(focusedTabID)]?.ceded,
  ).toBeUndefined();
  h1.tabs.patch(focusedTabID, { active: false });
  const retiring = closeInternals(h1).closeOwnedSurface(
    focusedTabID,
    "scaffold_idle",
  );
  const ownerRequest = await h1.port.waitForFrame("surface_close_request");
  await h1.port.inbound(
    nativeResult("surface_close_response", {
      request_id: ownerRequest.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-s6-owner",
      nonce: "nonce-s6-owner",
      browser_holder_generation: 1,
    }),
  );
  await expect(retiring).resolves.toEqual({ closed: true });
  expect(h1.tabs.removed).toContain(focusedTabID);

  // Part (c): a genuine operator activation cedes at the activation itself,
  // and that cession survives a worker restart — no close is ever attempted.
  const h2 = makeHarness(undefined, { windows: true });
  await h2.bridge.start();
  const { tabID: scaffoldTabID } = await seedOwnedScaffold(h2);
  await h2.tabs.userActivate(scaffoldTabID);
  const ledgerBeforeRestart = closeInternals(h2).tabLedgerCache;
  expect(ledgerBeforeRestart[String(scaffoldTabID)]?.ceded).toBe(true);
  await expect(
    closeInternals(h2).closeOwnedSurface(scaffoldTabID, "scaffold_idle"),
  ).resolves.toEqual({ closed: false });
  expect(h2.tabs.removed).not.toContain(scaffoldTabID);

  const restarted = restartWorker(h2);
  await restarted.bridge.start();
  const framesBeforeRestart = restarted.frames().length;
  const closing3 = closeInternals(restarted).closeOwnedSurface(
    scaffoldTabID,
    "scaffold_idle",
  );
  await expect(closing3).resolves.toEqual({ closed: false });
  expect(restarted.tabs.removed).not.toContain(scaffoldTabID);
  expect(restarted.tabs.snapshot(scaffoldTabID)).toBeDefined();
  // Refused at the ledger-entry gate before even attempting the wire round
  // trip: no renavigation, no close, no surface_close_request at all.
  expect(
    restarted
      .frames()
      .slice(framesBeforeRestart)
      .some((f) => f.type === "surface_close_request"),
  ).toBe(false);
});

test("Slice 2b: a failed remove leaves the tombstone; startup replay completes it", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID } = await seedOwnedScaffold(h);
  h.deps.tabs.remove = async () => {
    throw new Error("remove failed");
  };
  const closing = closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle");
  const request = await h.port.waitForFrame("surface_close_request");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-0003",
      nonce: "nonce-0003",
      browser_holder_generation: 1,
    }),
  );
  await closing;
  // closeOwnedTab swallows the remove failure; the tab and its tombstone survive.
  expect(h.tabs.snapshot(tabID)).toBeDefined();
  expect(closeInternals(h).tabLedgerCache[String(tabID)]?.pending_close).toBeDefined();

  const restarted = restartWorker(h);
  (
    restarted.bridge as unknown as { lastKnownBrowserHolderGeneration: number }
  ).lastKnownBrowserHolderGeneration = 1;
  const startPromise = restarted.bridge.start();
  await restarted.port.inbound(helloAck({ features: ["surface_close_v1"] }));
  const replay = await restarted.port.waitForFrame("surface_close_request");
  expect(replay.payload["binding_id"]).toBeDefined();
  restarted.deps.tabs.remove = async (id) => {
    h.tabs.live.delete(id);
  };
  await restarted.port.inbound(
    nativeResult("surface_close_response", {
      request_id: replay.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-0004",
      nonce: "nonce-0004",
      browser_holder_generation: 1,
    }),
  );
  await startPromise;
  expect(h.tabs.snapshot(tabID)).toBeUndefined();
});

test("P0 (oracle finding 2): a persisted close tombstone survives a hello_ack race — local bootstrap alone cannot replay it, but the SAME holder ack that finally supplies browser_holder_generation closes the tab in the SAME worker lifetime", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID } = await seedOwnedScaffold(h);
  h.deps.tabs.remove = async () => {
    throw new Error("remove failed");
  };
  const closing = closeInternals(h).closeOwnedSurface(tabID, "scaffold_idle");
  const request = await h.port.waitForFrame("surface_close_request");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-p0-0001",
      nonce: "nonce-p0-0001",
      browser_holder_generation: 7,
    }),
  );
  await closing;
  // Same fixture as the sibling "failed remove" test: the tombstone
  // (durably persisted, not merely in worker memory) survives.
  expect(h.tabs.snapshot(tabID)).toBeDefined();
  expect(closeInternals(h).tabLedgerCache[String(tabID)]?.pending_close).toBeDefined();

  // A worker restart wipes lastKnownBrowserHolderGeneration (worker-memory
  // only, per its own doc comment) but keeps the persisted tombstone and
  // the browser epoch it was minted under — deliberately NOT pre-seeding
  // it the way the sibling test does, to actually exercise the race.
  const restarted = restartWorker(h);
  restarted.deps.tabs.remove = async (id) => {
    h.tabs.live.delete(id);
  };
  await restarted.bridge.start();
  // hello_ack has not arrived yet: local bootstrap's own replay pass ran
  // during start() and lost the race exactly as the oracle finding
  // describes — no live browser_holder_generation to send, and
  // requestCloseAuthorization refuses locally rather than guess one. Before
  // the P0 fix this was the end of the story: nothing else ever retried,
  // and the tombstone (and the tab) would strand forever. Scoped to this
  // restarted worker's OWN port — the pre-restart port's own successful
  // surface_close_request from above is still in `frames()`'s aggregate.
  expect(
    restarted.port.posted
      .map(parseBrowserMessage)
      .some((f) => f.type === "surface_close_request"),
  ).toBe(false);
  expect(restarted.tabs.snapshot(tabID)).toBeDefined();

  // hello_ack finally arrives, carrying surface_close_v1 AND the live
  // holder generation together — the fix's trigger. No second restart.
  // waitForFrame is started BEFORE the trigger: scheduleCloseTombstoneReplay
  // is fire-and-forget from the hello_ack handler, so the request can post
  // after port.inbound's own promise has already resolved.
  const replay = restarted.port.waitForFrame("surface_close_request");
  await restarted.port.inbound(
    helloAck({ features: ["surface_close_v1"], browser_holder_generation: 7 }),
  );
  const replayFrame = await replay;
  expect(replayFrame.payload["binding_id"]).toBeDefined();
  expect(replayFrame.payload["browser_holder_generation"]).toBe(7);
  await restarted.port.inbound(
    nativeResult("surface_close_response", {
      request_id: replayFrame.payload["request_id"],
      outcome: "authorized",
      close_authorization_id: "auth-p0-0002",
      nonce: "nonce-p0-0002",
      browser_holder_generation: 7,
    }),
  );
  // scheduleCloseTombstoneReplay's inLifecycleChain work is fire-and-forget
  // from the hello_ack handler; let its continuation (closeAuthorizedRecord
  // through closeOwnedTab) settle before asserting the removal it performs.
  // The tab closes in THIS worker lifetime — no second restart required,
  // closing the gap the oracle's finding 2 and test-gap 5 describe.
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
  expect(restarted.tabs.snapshot(tabID)).toBeUndefined();
});

test("Slice 2b: a browser restart invalidates tab-ID authority; records survive only for review", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_epoch_a"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  const internals = h.bridge as unknown as {
    browserEpoch: string | undefined;
    ownedMemberTab: (
      tabID: number,
      ledger: Record<string, SurfaceBirthRecord>,
    ) => Promise<TabInfo | undefined>;
    snapshotTabLedger: () => Promise<Record<string, SurfaceBirthRecord>>;
  };
  const ledgerBefore = await internals.snapshotTabLedger();
  expect(ledgerBefore[String(tabID)]?.browser_epoch).toBe(internals.browserEpoch);
  expect(await internals.ownedMemberTab(tabID, ledgerBefore)).toBeDefined();

  // A genuine browser restart mints a fresh epoch no existing record can
  // re-prove against (classifyRestart's "browser" branch) — simulated here
  // directly on the same worker lifetime's epoch field, since a real restart
  // spins up an entirely separate process with no shared harness state.
  internals.browserEpoch = "post-restart-epoch";
  const ledgerAfter = await internals.snapshotTabLedger();
  expect(await internals.ownedMemberTab(tabID, ledgerAfter)).toBeUndefined();
  // The record itself is never deleted — it survives, review-only.
  expect(ledgerAfter[String(tabID)]).toBeDefined();
});

test("Slice 2b: a browser restart's fresh epoch retains a stale pending tombstone's tab instead of closing it", async () => {
  const h = makeHarness(undefined, { windows: true });
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    browserEpoch: string | undefined;
    replayPendingCloseTombstones: () => Promise<void>;
  };
  const preRestartEpoch = internals.tabLedgerCache[String(tabID)]?.browser_epoch;
  internals.tabLedgerCache = {
    [String(tabID)]: {
      ...internals.tabLedgerCache[String(tabID)]!,
      pending_close: {
        authorization_id: "auth-stale-0001",
        nonce: "nonce-stale-0001",
        holder_generation: 1,
        recorded_at: 1_700_000_000_000,
        disposition: "scaffold_idle",
      },
    },
  };
  // A genuine browser restart mints a fresh epoch no existing record can
  // re-prove against (classifyRestart's "browser" branch) — simulated
  // directly on the same worker lifetime's epoch field, matching the
  // adjacent epoch-invalidation test's technique, since a real restart
  // spins up an entirely separate process with no shared harness state.
  expect(preRestartEpoch).toBeDefined();
  internals.browserEpoch = "post-restart-epoch";

  const framesBefore = h.port.posted.length;
  await internals.replayPendingCloseTombstones();

  // No close authorization was ever requested for a tab-ID whose ledger
  // authority died with the old epoch — never authorize or remove
  // against it.
  expect(
    h.frames().slice(framesBefore).some((f) => f.type === "surface_close_request"),
  ).toBe(false);
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeDefined();

  // The record itself survives — ceded for the standing operator-review
  // path, its pending tombstone and job binding cleared so nothing acts on
  // it again.
  const record = internals.tabLedgerCache[String(tabID)];
  expect(record?.ceded).toBe(true);
  expect(record?.binding_id).toBe(bindingID);
  expect(record?.pending_close).toBeUndefined();
});

test("Slice 2b: adoption ignores a papio-titled group with no owned member", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  const foreign = await h.tabs.create({
    url: "https://foreign.example.edu/current",
    active: false,
  });
  h.tabs.patch(foreign.id!, { groupId: 700 });
  h.tabGroups!.live.set(700, { id: 700, collapsed: true, title: "papio", windowId: 1 });
  h.tabGroups!.nextID = 701;
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_no_owned_member"));
  // A brand new group was created; the foreign "papio"-titled group was
  // never merged into or adopted as the persisted group.
  expect(h.backend.store.handoffGroupID).not.toBe(700);
  expect(h.tabs.grouped.some((g) => g.groupId === 700)).toBe(false);
});

test("Slice 2b: jobless sign-in reuse still works via origin-digest match", async () => {
  const h = makeHarness();
  const origin = "https://resolver.example.edu";
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ resolver_origins: [origin] }));
  const first = await h.bridge.requestSessionSignIn(origin);
  expect(first.ok).toBe(true);
  expect(h.tabs.created).toHaveLength(1);
  const tabID = h.tabs.created.length > 0 ? h.tabs.list()[0]!.id! : -1;
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/sso");
  const second = await h.bridge.requestSessionSignIn(origin);
  expect(second.ok).toBe(true);
  expect(h.tabs.created).toHaveLength(1);
  expect(h.tabs.activated).toContain(tabID);
});

test("Slice 2b: legacy timeout park detaches and retains when the daemon answers not_eligible", async () => {
  const h = makeHarness(undefined, { windows: true });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(helloAck({ features: ["surface_close_v1"] }));
  await h.port.inbound(jobOffer("job_timeout_not_eligible"));
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);
  const internals = h.bridge as unknown as {
    lastKnownBrowserHolderGeneration: number | undefined;
  };
  internals.lastKnownBrowserHolderGeneration = 1;
  const timeout = h.timers.find((timer) => timer.ms === 3 * 60_000);
  expect(timeout).toBeDefined();
  const fired = timeout!.fn();
  const request = await h.port.waitForFrame("surface_close_request");
  await h.port.inbound(
    nativeResult("surface_close_response", {
      request_id: request.payload["request_id"],
      outcome: "not_eligible",
    }),
  );
  await fired;
  // Detached (job no longer tracks the tab) and retained (never closed).
  expect(
    h.backend.store.activeJobs.find((j) => j.job_id === "job_timeout_not_eligible")?.tab_id,
  ).toBe(-1);
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeDefined();
});

test("Slice 2b: update simulation adopts zero new surfaces and closes nothing without authorization", async () => {
  const h = makeHarness(undefined, { tabGroups: true, handoffSurface: "tab-group" });
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(jobOffer("job_scenario2_a"));
  const groupID = h.backend.store.handoffGroupID;
  const tabID = h.backend.store.activeJobs[0]?.tab_id ?? -1;
  expect(h.tabGroups?.live.size).toBe(1);

  const updated = simulateExtensionUpdate(h);
  await updated.bridge.start();
  // Session-scoped job state is gone (a fresh Bridge, no jobs), but the
  // birth certificate and its group are revalidated and reused untouched:
  // no new group, no new window, and nothing was closed to get there.
  expect(h.tabGroups?.live.size).toBe(1);
  expect(h.backend.store.handoffGroupID).toBe(groupID);
  expect(h.tabs.removed).not.toContain(tabID);
  expect(h.tabs.snapshot(tabID)).toBeDefined();
});

test("Scenario 2: a live institutional materialize.html scaffold survives simulateExtensionUpdate untouched — birth record revalidated across the update, and a post-update re-offer never leaves two live scaffolds for the same binding", async () => {
  const jobID = "job_scenario2_materialize";
  const candidateID = "cand_scenario2_0001";
  const bindingID = "bind_scenario2_0001";
  const claimID = "claim_scenario2_0001";
  const h = makeHarness();
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["institutional_materialization_v1"] }),
  );
  await h.port.inbound(candidateOffer(jobID, candidateID));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  const claimResponsePayload = institutionalClaimResponse(
    jobID,
    candidateID,
    claimID,
    bindingID,
  ) as { payload: Record<string, unknown> };
  claimResponsePayload.payload["request_id"] = claim.payload["request_id"];
  await h.port.inbound(claimResponsePayload);
  const bind = await h.port.waitForFrame("institutional_bind_request");
  expect(bind.payload["binding_id"]).toBe(bindingID);
  const scaffoldTab = h.tabs
    .list()
    .find(
      (tab) =>
        tab.url === `chrome-extension://test/dist/materialize.html#${bindingID}`,
    );
  expect(scaffoldTab).toBeDefined();
  const scaffoldTabID = scaffoldTab!.id!;
  const bindResponsePayload = institutionalBindResponse(
    jobID,
    claimID,
    bindingID,
  ) as { payload: Record<string, unknown> };
  bindResponsePayload.payload["request_id"] = bind.payload["request_id"];
  await h.port.inbound(bindResponsePayload);

  const internals = h.bridge as unknown as {
    snapshotTabLedger: () => Promise<Record<string, SurfaceBirthRecord>>;
  };
  const ledgerBefore = await internals.snapshotTabLedger();
  expect(ledgerBefore[String(scaffoldTabID)]).toMatchObject({
    binding_id: bindingID,
  });

  const updated = simulateExtensionUpdate(h);
  await updated.bridge.start();
  // Session-scoped materialization state is gone (a fresh Bridge, no jobs),
  // but the scaffold tab and its birth certificate survive the update
  // untouched: nothing was closed to get there.
  expect(Object.keys(h.backend.store.materializations ?? {})).toHaveLength(0);
  expect(h.tabs.removed).not.toContain(scaffoldTabID);
  expect(h.tabs.snapshot(scaffoldTabID)).toBeDefined();
  const updatedInternals = updated.bridge as unknown as {
    snapshotTabLedger: () => Promise<Record<string, SurfaceBirthRecord>>;
  };
  const ledgerAfter = await updatedInternals.snapshotTabLedger();
  expect(ledgerAfter[String(scaffoldTabID)]).toMatchObject({
    binding_id: bindingID,
  });

  // A fresh daemon re-offer/re-claim for the same binding after the update
  // must never leave TWO simultaneously live scaffolds for it — whichever
  // tab materializeScaffold ends up driving (reused or freshly minted),
  // there is exactly one live scaffold for this binding at any time.
  await updated.port.inbound(
    helloAck({ features: ["institutional_materialization_v1"] }),
  );
  await updated.port.inbound(candidateOffer(jobID, candidateID));
  const claim2 = await updated.port.waitForFrame("institutional_claim_request");
  const claimResponsePayload2 = institutionalClaimResponse(
    jobID,
    candidateID,
    claimID,
    bindingID,
  ) as { payload: Record<string, unknown> };
  claimResponsePayload2.payload["request_id"] = claim2.payload["request_id"];
  await updated.port.inbound(claimResponsePayload2);
  const bind2 = await updated.port.waitForFrame("institutional_bind_request");
  expect(bind2.payload["binding_id"]).toBe(bindingID);
  const liveScaffoldsForBinding = updated.tabs
    .list()
    .filter(
      (tab) =>
        tab.url === `chrome-extension://test/dist/materialize.html#${bindingID}`,
    );
  expect(liveScaffoldsForBinding).toHaveLength(1);
});

test("Review round finding 1: reconciliation facing an operator-active scaffold plus an unledgered duplicate cedes the active one and retains both, never adopting, deactivating, or deleting either", async () => {
  const jobID = "job_scenario_duplicate_reconcile";
  const candidateID = "cand_duplicate_0001";
  const bindingID = "bind_duplicate_0001";
  const claimID = "claim_duplicate_0001";
  const h = makeHarness();
  installManagedTabLedger(h, {});
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["institutional_materialization_v1"] }),
  );
  const scaffoldURL = `chrome-extension://test/dist/materialize.html#${bindingID}`;
  // The active scaffold is papio's own prior work (ledgered, under this
  // exact binding and browser epoch) that the operator has since claimed
  // by activating it. The duplicate is a second live tab that merely
  // matches the URL pattern — never ledgered by this worker lifetime, the
  // review round's finding 1 scenario for "a restored or user-duplicated
  // scaffold".
  const activeTab = await h.tabs.create({ url: scaffoldURL, active: true });
  const duplicateTab = await h.tabs.create({
    url: scaffoldURL,
    active: false,
  });
  const internals = h.bridge as unknown as {
    tabLedgerCache: Record<string, SurfaceBirthRecord>;
    browserEpoch: string | undefined;
  };
  internals.tabLedgerCache = {
    ...internals.tabLedgerCache,
    [String(activeTab.id)]: fakeBirthRecord({
      binding_id: bindingID,
      tab_hint: activeTab.id!,
      browser_epoch: internals.browserEpoch ?? "test-epoch",
    }),
  };

  await h.port.inbound(candidateOffer(jobID, candidateID));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  const claimResponsePayload = institutionalClaimResponse(
    jobID,
    candidateID,
    claimID,
    bindingID,
  ) as { payload: Record<string, unknown> };
  claimResponsePayload.payload["request_id"] = claim.payload["request_id"];
  await h.port.inbound(claimResponsePayload);
  // materializeScaffold's own ceding/adoption pass runs synchronously
  // before either the reused branch's ledgerManagedTab or the create-new
  // branch's institutional_bind_request — waiting for the eventual
  // bind request (for whichever tab it ends up driving) proves that pass
  // has already completed.
  await h.port.waitForFrame("institutional_bind_request");
  // Never deactivated, never adopted as the working scaffold: the
  // operator's activation is permanent cession, not a papio-overridable
  // hint — persisted on the ledger record, not merely inferred.
  expect(
    h.tabs.updates.some(
      (update) =>
        update.id === activeTab.id && update.properties.active === false,
    ),
  ).toBe(false);
  expect(h.tabs.snapshot(activeTab.id!)?.active).toBe(true);
  expect(h.tabs.removed).not.toContain(activeTab.id);
  expect(internals.tabLedgerCache[String(activeTab.id)]?.ceded).toBe(true);
  // Never silently deleted: an unledgered duplicate is retained, never
  // routed through closeOwnedTab's materialization-reconcile bypass
  // (which needs no birth record at all) just because its URL matches.
  expect(h.tabs.removed).not.toContain(duplicateTab.id);
  expect(h.tabs.snapshot(duplicateTab.id!)).toBeDefined();
});

// Live-smoke regression (2026-08-19): the scaffold path was a root-relative
// "materialize.html" while every shipped manifest declares pages under dist/,
// so every automatically-owned institutional tab navigated to a page that
// exists nowhere and rendered Chrome's ERR_FILE_NOT_FOUND. The old literal
// assertions could not catch it: they pinned the same wrong string the source
// built. This one is anchored to the shipped manifest instead, so the scaffold
// URL and the declared page layout cannot drift apart again.
test("the scaffold tab navigates to a page where the shipped manifest declares pages live", async () => {
  const jobID = "job_scaffold_page_layout";
  const candidateID = "cand_scaffold_layout";
  const bindingID = "bind_scaffold_layout";
  const h = makeHarness();
  await h.bridge.start();
  await h.port.inbound(
    helloAck({ features: ["institutional_materialization_v1"] }),
  );
  await h.port.inbound(candidateOffer(jobID, candidateID));
  const claim = await h.port.waitForFrame("institutional_claim_request");
  await h.port.inbound({
    protocol: "papio-browser/1",
    type: "institutional_claim_response",
    msg_id: `claim_response_${bindingID}`,
    job_id: jobID,
    seq: 3,
    payload: {
      request_id: claim.payload["request_id"],
      outcome: "claimed",
      candidate_id: candidateID,
      claim_id: `claim_${bindingID}`,
      binding_id: bindingID,
      browser_holder_generation: 1,
      lease_until: "2030-01-01T00:05:00Z",
    },
  });
  await h.port.waitForFrame("institutional_bind_request");
  const scaffoldURL = h.tabs.created.at(0)?.url;
  expect(scaffoldURL).toBeDefined();

  // The extension root is whatever directory holds the manifest; pages are
  // wherever the manifest says the popup is. Both shipped manifests
  // (Chrome's hand-authored one and the generated Firefox one) declare
  // dist/, and dev-unpacked/ mirrors it.
  const manifest = JSON.parse(
    readFileSync(new URL("../manifest.json", import.meta.url), "utf8"),
  ) as { action?: { default_popup?: string } };
  const declaredPopup = manifest.action?.default_popup;
  expect(declaredPopup).toBeDefined();
  const pageDir = declaredPopup!.replace(/[^/]*$/, "");
  expect(pageDir).not.toBe("");

  const scaffoldPath = new URL(scaffoldURL!).pathname.replace(/^\//, "");
  expect(scaffoldPath).toBe(`${pageDir}materialize.html`);
  // Build-independent existence proof: the page ships from src/, and the
  // bundler emits it into pageDir under its own name.
  const shell = readFileSync(
    new URL("../src/materialize.html", import.meta.url),
    "utf8",
  );
  expect(shell).toContain("<html");
});

// An extension reload wipes chrome.storage.session, so a paper mid-sign-in
// loses its auth_pending status - and the daemon cannot re-offer it, because a
// job owning a live claim is not a scheduler-eligible candidate. Measured live
// 2026-08-20: right after a reload the badge read "papio: connected" while a
// real login page sat in papio's own tab group, so the one paper the operator
// could finish was the one paper papio stopped mentioning. The durable birth
// ledger survives the wipe and is the recovery path.
test("a reload keeps reporting a paper stopped at a login page", async () => {
  const h = makeHarness(undefined, { windows: true });
  const ledger = installManagedTabLedger(h, {});
  await h.bridge.start();
  const { tabID, bindingID } = await seedOwnedScaffold(h);
  // Persist the birth certificate the way a real mint does, so it is present
  // in storage.local (not just worker memory) across the reload.
  const internals = h.bridge as unknown as {
    browserEpoch: string | undefined;
  };
  await h.deps.tabLedger?.save({
    [String(tabID)]: fakeBirthRecord({
      binding_id: bindingID,
      tab_hint: tabID,
      browser_epoch: internals.browserEpoch ?? "test-epoch",
    }),
  });
  await h.tabs.userNavigate(tabID, "https://idp.example.edu/sso");

  const reloaded = simulateExtensionUpdate(h);
  await reloaded.bridge.start();
  await reloaded.port.inbound(helloAck({ features: ["surface_close_v1"] }));
  // The reload really did lose the job, and the ledger really did survive.
  expect(reloaded.backend.store.activeJobs).toHaveLength(0);
  expect(Object.keys(ledger.current())).toContain(String(tabID));

  await reloaded.bridge.syncConnectionBadge();
  expect(reloaded.action.texts.at(-1)).toBe("1");
  expect(reloaded.action.backgroundColors.at(-1)).toBe("#b06000");
  expect(reloaded.action.titles.at(-1)).toBe(
    "papio: 1 paper needs your institution sign-in",
  );

  // Off the wall, no ask: the same surface on a provider page reports nothing.
  await reloaded.tabs.userNavigate(tabID, `https://${PROVIDER_HOST}/article`);
  await reloaded.bridge.syncConnectionBadge();
  expect(reloaded.action.titles.at(-1)).not.toContain("sign-in");
});
