import type { TabChangeInfo, TabInfo } from "../src/background";

export class FakeEmitter<A extends unknown[]> {
  private readonly listeners: ((...args: A) => unknown)[] = [];

  addListener(listener: (...args: A) => void): void {
    this.listeners.push(listener);
  }

  removeListener(listener: (...args: A) => void): void {
    const index = this.listeners.indexOf(listener);
    if (index >= 0) this.listeners.splice(index, 1);
  }

  /** Test-only restart seam: capture how many listeners exist right now
   * (typically zero, or however many a fixture's own constructor installed)
   * and return a closure that truncates back to exactly that count. Models
   * an MV3 worker death/update: the dead worker's closures — never a
   * fixture's own internal bookkeeping listeners, which are always
   * installed before any Bridge exists — must stop being invoked once a
   * fresh Bridge takes over the same fake. */
  snapshot(): () => void {
    const keep = this.listeners.length;
    return () => {
      this.listeners.length = keep;
    };
  }

  async emit(...args: A): Promise<void> {
    await Promise.all(this.listeners.map((listener) => listener(...args)));
  }
}

export class FakeWebNavigation {
  readonly onCommitted = new FakeEmitter<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>();
  readonly onHistoryStateUpdated = new FakeEmitter<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>();
  readonly onReferenceFragmentUpdated = new FakeEmitter<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>();
  /** chrome.webNavigation.onTabReplaced delivers `{tabId, replacedTabId}`.
   * `{addedTabId, removedTabId}` is the *separate* tabs.onReplaced event; a
   * harness that emits those names tests a fiction the browser never sends. */
  readonly onTabReplaced = new FakeEmitter<[{ tabId: number; replacedTabId: number }]>();
  /** chrome.webNavigation.onErrorOccurred — a top-frame navigation that
   * failed to commit. Deliberately carries no URL beyond what a real caller
   * supplies: the seam exists only to test navigation-error ordering. */
  readonly onErrorOccurred = new FakeEmitter<[{ tabId: number; frameId: number; error?: string }]>();
  /** The browser's own top-frame document epochs — the state `getFrame`
   * reports. It lives in the browser, so it outlives a service-worker
   * restart; that is precisely why the background reads it instead of
   * trusting a worker-local map that MV3 empties without warning.
   *
   * `null` models a tab (or a platform) the browser reports no epoch for.
   * An untouched tab reports a stable id derived from the tab number, so a
   * restarted worker looking at the same undisturbed tab sees the same
   * document — exactly what Chrome does. */
  private readonly liveFrames = new Map<number, string | null>();
  private commits = 0;

  constructor() {
    // A committed top-frame navigation is a new document. When a test does not
    // name the id, mint a fresh one anyway: pretending the epoch survived a
    // real navigation is the fiction this harness exists to prevent.
    this.onCommitted.addListener((d) => {
      if (d.frameId !== 0) return;
      this.commits += 1;
      this.liveFrames.set(d.tabId, d.documentId ?? `doc-auto-${d.tabId}-c${this.commits}`);
    });
    this.onTabReplaced.addListener((d) => {
      this.liveFrames.delete(d.replacedTabId);
    });
  }

  /** State the browser's epoch without pretending an event was delivered —
   * models a tab that already existed before this worker started listening. */
  setFrame(tabId: number, documentId: string): void {
    this.liveFrames.set(tabId, documentId);
  }

  /** Model a browser that reports no document epoch for this tab at all. */
  clearFrame(tabId: number): void {
    this.liveFrames.set(tabId, null);
  }

  async getFrame(details: { tabId: number; frameId: number }): Promise<{ documentId?: string } | null> {
    if (details.frameId !== 0) return null;
    const known = this.liveFrames.get(details.tabId);
    if (known === null) return null;
    return { documentId: known ?? `doc-auto-${details.tabId}` };
  }

  /** Convenience: emit a top-frame navigation error for a tab. */
  async emitError(tabId: number, error?: string): Promise<void> {
    await this.onErrorOccurred.emit({
      tabId,
      frameId: 0,
      ...(error === undefined ? {} : { error }),
    });
  }
}

export type FakeTab = TabInfo & {
  muted?: boolean;
};

type TabCreateProperties = {
  url: string;
  active: boolean;
  windowId?: number;
  pinned?: boolean;
  muted?: boolean;
  [key: string]: unknown;
};

type TabUpdateProperties = {
  active?: boolean;
  url?: string;
  pinned?: boolean;
  muted?: boolean;
  [key: string]: unknown;
};

type TabQuery = {
  url?: string | string[];
  groupId?: number;
  active?: boolean;
  pinned?: boolean;
  muted?: boolean;
  lastFocusedWindow?: boolean;
  windowId?: number;
};

function cloneTab(tab: FakeTab): FakeTab {
  return { ...tab };
}

function matchesURL(url: string | undefined, pattern: string): boolean {
  if (url === undefined) return false;
  if (!pattern.includes("*")) return url === pattern;
  const escaped = pattern.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*");
  return new RegExp(`^${escaped}$`).test(url);
}

/** A deliberately small, stateful model of the tab lifecycle Chrome exposes. */
export class ChromeTabsFake {
  readonly onUpdated = new FakeEmitter<[number, TabChangeInfo, FakeTab]>();
  readonly onRemoved = new FakeEmitter<[number, { isWindowClosing: boolean }]>();
  readonly onActivated = new FakeEmitter<[{ tabId: number; windowId: number }]>();
  readonly created: TabCreateProperties[] = [];
  readonly removed: number[] = [];
  readonly grouped: {
    tabIds: number[];
    groupId?: number;
    createProperties?: { windowId?: number };
  }[] = [];
  group?: (options: {
    tabIds: number[];
    groupId?: number;
    createProperties?: { windowId?: number };
  }) => Promise<number>;
  readonly activated: number[] = [];
  readonly navigations: { tabID: number; url: string }[] = [];
  readonly reloaded: number[] = [];
  readonly updates: { id: number; properties: TabUpdateProperties }[] = [];
  readonly resolverTabs: FakeTab[] = [];
  private focusedTabID: number | undefined;
  queryCount = 0;
  nextURL: string | undefined;
  readonly live = new Map<number, FakeTab>();
  nextId = 200;
  currentWindowID = 1;
  failCreate = false;
  failWindowCreate = false;
  private readonly focusedWindowIDs = new Set<number>();
  get focusedTab(): FakeTab | undefined {
    return this.focusedTabID === undefined ? undefined : this.snapshot(this.focusedTabID);
  }

  set focusedTab(tab: FakeTab | undefined) {
    this.focusedTabID = tab?.id;
    if (tab?.id !== undefined && this.live.has(tab.id)) {
      this.activateState(tab.id, tab.windowId ?? this.currentWindowID);
    }
  }

  /** Insert setup state without pretending that a user or Chrome emitted it. */
  seed(tab: FakeTab): void {
    if (tab.id === undefined) throw new Error("seed requires a tab id");
    const copy = cloneTab(tab);
    const id = copy.id;
    if (id === undefined) throw new Error("seed requires a tab id");
    if (copy.windowId === undefined) copy.windowId = this.currentWindowID;
    this.live.set(id, copy);
    this.nextId = Math.max(this.nextId, id + 1);
    if (copy.active === true) this.activateState(id, copy.windowId!);
  }
  snapshot(tabId: number): FakeTab | undefined {
    const tab = this.live.get(tabId);
    return tab === undefined ? undefined : cloneTab(tab);
  }

  list(): FakeTab[] {
    return [...this.live.values()].map(cloneTab);
  }

  async create(properties: TabCreateProperties): Promise<FakeTab> {
    this.created.push({ ...properties });
    if (this.failCreate || (this.failWindowCreate && properties.windowId !== undefined)) throw new Error("tab creation blocked");
    const id = this.nextId++;
    const windowId = properties.windowId ?? this.currentWindowID;
    const tab: FakeTab = {
      id,
      url: properties.url,
      windowId,
      active: properties.active,
      ...(properties.pinned === undefined ? {} : { pinned: properties.pinned }),
      ...(properties.muted === undefined ? {} : { muted: properties.muted }),
    };
    for (const [key, value] of Object.entries(properties)) {
      if (!(key in tab) && key !== "url" && key !== "active" && key !== "windowId") {
        (tab as Record<string, unknown>)[key] = value;
      }
    }
    this.live.set(id, tab);
    if (tab.active === true) {
      tab.active = false;
      await this.activate(id, windowId);
    }
    return cloneTab(tab);
  }

  async get(tabId: number): Promise<FakeTab> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    return cloneTab(tab);
  }

  async query(query: TabQuery): Promise<FakeTab[]> {
    this.queryCount += 1;
    const patterns = query.url === undefined ? undefined : Array.isArray(query.url) ? query.url : [query.url];
    return this.list().filter((tab) =>
      (patterns === undefined || patterns.some((pattern) => matchesURL(tab.url, pattern))) &&
      (query.groupId === undefined || tab.groupId === query.groupId) &&
      (query.windowId === undefined || tab.windowId === query.windowId) &&
      (query.active === undefined || tab.active === query.active) &&
      (query.pinned === undefined || tab.pinned === query.pinned) &&
      (query.muted === undefined || tab.muted === query.muted),
    );
  }

  async update(tabId: number, properties: TabUpdateProperties): Promise<FakeTab> {
    this.updates.push({ id: tabId, properties: { ...properties } });
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    if (properties.url !== undefined) {
      this.navigations.push({ tabID: tabId, url: properties.url });
      tab.url = properties.url;
      tab.status = "loading";
      await this.onUpdated.emit(tabId, { url: properties.url, status: "loading" }, cloneTab(tab));
    }
    const change: TabChangeInfo = {};
    for (const [key, value] of Object.entries(properties)) {
      if (key !== "active" && key !== "url") {
        (tab as Record<string, unknown>)[key] = value;
        if (key === "status" || key === "title") {
          (change as Record<string, unknown>)[key] = value;
        }
      }
    }
    if (properties.active === true) await this.activate(tabId, tab.windowId ?? this.currentWindowID);
    if (Object.keys(change).length > 0) await this.onUpdated.emit(tabId, change, cloneTab(tab));
    return cloneTab(tab);
  }

  async completeNavigation(tabId: number, finalURL?: string): Promise<FakeTab> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    if (finalURL !== undefined) tab.url = finalURL;
    tab.status = "complete";
    await this.onUpdated.emit(
      tabId,
      { ...(finalURL === undefined ? {} : { url: finalURL }), status: "complete" },
      cloneTab(tab),
    );
    return cloneTab(tab);
  }

  async userNavigate(tabId: number, requestedURL: string): Promise<FakeTab> {
    return this.navigate(tabId, requestedURL);
  }

  async userActivate(tabId: number): Promise<void> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    await this.activate(tabId, tab.windowId ?? this.currentWindowID);
  }

  async userClose(tabId: number): Promise<void> {
    await this.removeInternal(tabId, false);
  }

  async remove(tabId: number): Promise<void> {
    await this.removeInternal(tabId, false);
  }

  async removeFromWindow(tabId: number): Promise<void> {
    await this.removeInternal(tabId, true);
  }

  async reload(tabId: number): Promise<void> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    this.reloaded.push(tabId);
    if (this.nextURL !== undefined) tab.url = this.nextURL;
  }

  /** Test-only setup mutation for metadata such as openerTabId/groupId. */
  patch(tabId: number, changes: Partial<FakeTab>): void {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    Object.assign(tab, changes);
  }

  /** Simulate state discovered absent after a worker restart, without replaying a missed event. */
  forget(tabId: number): void {
    this.live.delete(tabId);
  }

  clear(): void {
    this.live.clear();
  }
  private async navigate(tabId: number, requestedURL: string): Promise<FakeTab> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    this.navigations.push({ tabID: tabId, url: requestedURL });
    tab.url = requestedURL;
    tab.status = "loading";
    await this.onUpdated.emit(tabId, { url: requestedURL, status: "loading" }, cloneTab(tab));
    return cloneTab(tab);
  }

  private activateState(tabId: number, windowId: number): void {
    for (const tab of this.live.values()) {
      if (tab.windowId === windowId) tab.active = tab.id === tabId;
    }
    this.focusedWindowIDs.add(windowId);
  }

  private async activate(tabId: number, windowId: number): Promise<void> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    const changed = tab.active !== true;
    this.activateState(tabId, windowId);
    if (!changed) return;
    this.activated.push(tabId);
    await this.onActivated.emit({ tabId, windowId });
  }

  private async removeInternal(tabId: number, isWindowClosing: boolean): Promise<void> {
    const tab = this.live.get(tabId);
    if (tab === undefined) throw new Error(`No tab with id: ${tabId}.`);
    const wasActive = tab.active === true;
    const windowId = tab.windowId;
    this.live.delete(tabId);
    this.removed.push(tabId);
    await this.onRemoved.emit(tabId, { isWindowClosing });
    if (!wasActive || windowId === undefined) return;
    const neighbour = [...this.live.values()].find((candidate) => candidate.windowId === windowId);
    if (neighbour !== undefined) await this.activate(neighbour.id!, windowId);
  }

}
export const SAML_JOURNEY_ORIGINS = {
  sp: "https://sp.example.test",
  discovery: "https://discovery.example.test",
  idp: "https://idp.example.test",
  callback: "https://callback.example.test",
} as const;

export type SAMLJourneyEngagement = Readonly<{
  token: string;
  spURL: string;
  discoveryURL: string;
  idpURL: string;
  callbackURL: string;
}>;

export type SAMLReplayResponse = Readonly<{ status: "expired-token"; token: string }>;

/** Cross-origin SAML flow with a one-use execution token and explicit redirects. */
export class SAMLJourneyFixture {
  private sequence = 0;
  private readonly issued = new Set<string>();

  constructor(private readonly tabs: Pick<ChromeTabsFake, "completeNavigation">) {}

  engage(): SAMLJourneyEngagement {
    this.sequence += 1;
    const token = `eNsM${this.sequence.toString(36)}`;
    this.issued.add(token);
    return {
      token,
      spURL: `${SAML_JOURNEY_ORIGINS.sp}/login`,
      discoveryURL: `${SAML_JOURNEY_ORIGINS.discovery}/start`,
      idpURL: `${SAML_JOURNEY_ORIGINS.idp}/sso?execution=${token}`,
      callbackURL: `${SAML_JOURNEY_ORIGINS.callback}/saml/acs`,
    };
  }

  replay(token: string): SAMLReplayResponse {
    if (!this.issued.has(token)) throw new Error("cannot replay an unknown execution token");
    return { status: "expired-token", token };
  }

  async driveRedirectChain(tabId: number, engagement: SAMLJourneyEngagement): Promise<void> {
    await this.tabs.completeNavigation(tabId, engagement.discoveryURL);
    await this.tabs.completeNavigation(tabId, engagement.idpURL);
    await this.tabs.completeNavigation(tabId, engagement.callbackURL);
  }
}

export function createSAMLJourney(tabs: Pick<ChromeTabsFake, "completeNavigation">): SAMLJourneyFixture {
  return new SAMLJourneyFixture(tabs);
}