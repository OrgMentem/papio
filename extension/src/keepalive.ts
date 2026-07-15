// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Institutional resolver session keepalive. This is deliberately independent of
// the bridge: callers supply only current job count and the latest OpenURL.

export interface KeepaliveTab {
  id?: number | undefined;
  url?: string | undefined;
}

export interface KeepaliveTabs {
  create(properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
  }): Promise<KeepaliveTab>;
  reload(tabID: number): Promise<unknown>;
  get(tabID: number): Promise<KeepaliveTab>;
  query(query: { pinned?: boolean; muted?: boolean; url?: string[] }): Promise<KeepaliveTab[]>;
  remove(tabID: number): Promise<void>;
  update(
    tabID: number,
    properties: { active?: boolean; pinned?: boolean; muted?: boolean },
  ): Promise<KeepaliveTab>;
}

export interface KeepaliveStorage {
  get(keys: string[]): Promise<Record<string, unknown>>;
}

export interface KeepaliveAction {
  setBadgeText(details: { text: string }): Promise<void>;
}

export interface KeepaliveTimers {
  setTimeout(callback: () => void | Promise<void>, delayMs: number): unknown;
  clearTimeout(handle: unknown): void;
}

export interface KeepaliveAPI {
  tabs: KeepaliveTabs;
  storage: KeepaliveStorage;
  timers: KeepaliveTimers;
  action?: KeepaliveAction;
}

export interface KeepaliveOptions {
  /** Number of currently non-terminal handoff jobs. */
  trackedJobCount(): number;
  /** OpenURL from the most recently received job offer, kept in bridge memory. */
  latestOpenURL(): string | undefined;
  /** Reports when the keepalive tab has verified an authenticated resolver
   * return, or when that evidence is lost. */
  onAuthenticationChanged?(authenticated: boolean): void;
  /** Called once per detected login redirect, after the tab is made visible. */
  onReauthNeeded?(): void;
  /** Overrides the post-reload inspection delay for deterministic tests. */
  reloadSettleMs?: number;
  /** Overrides job/recovery observation cadence for deterministic tests. */
  observeMs?: number;
}

const DEFAULT_INTERVAL_MINUTES = 4;
const MIN_INTERVAL_MINUTES = 2;
const MAX_INTERVAL_MINUTES = 30;
const DEFAULT_OBSERVE_MS = 15_000;
const DEFAULT_RELOAD_SETTLE_MS = 1_000;
const LOGIN_ROUTE = /login|auth|sso|idp|shibboleth|signon/i;

/** Shared conservative detector for login/IdP routes. */
export function isAuthenticationURL(rawURL: string): boolean {
  try {
    const url = new URL(rawURL);
    return LOGIN_ROUTE.test(url.pathname + url.search);
  } catch {
    return false;
  }
}

/** Clamp an untrusted storage value to the supported interval range. */
export function clampKeepaliveInterval(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value)) return DEFAULT_INTERVAL_MINUTES;
  return Math.min(MAX_INTERVAL_MINUTES, Math.max(MIN_INTERVAL_MINUTES, Math.trunc(value)));
}

/**
 * Maintains at most one resolver-origin tab while active handoffs exist.
 *
 * A single injected one-shot timer is used instead of setInterval: it lets the
 * MV3 service worker schedule the next operation only after the previous one
 * has finished, and keeps every transition deterministic under test.
 */
export class KeepaliveManager {
  private timer: unknown | undefined;
  private tabID: number | undefined;
  private resolver: URL | undefined;
  private intervalMinutes = DEFAULT_INTERVAL_MINUTES;
  private enabled = true;
  private reauthPaused = false;
  private authenticated = false;
  private started = false;
  private readonly observeMs: number;
  private readonly reloadSettleMs: number;

  constructor(
    private readonly api: KeepaliveAPI,
    private readonly options: KeepaliveOptions,
  ) {
    this.observeMs = Math.max(0, options.observeMs ?? DEFAULT_OBSERVE_MS);
    this.reloadSettleMs = Math.max(0, options.reloadSettleMs ?? DEFAULT_RELOAD_SETTLE_MS);
  }

  /** Load preferences and reconcile immediately. Safe to call more than once. */
  async init(): Promise<void> {
    if (this.started) return;
    this.started = true;
    await this.sync();
  }

  /** Re-read preferences and reconcile with the current bridge-provided state. */
  async sync(): Promise<void> {
    await this.loadPreferences();
    await this.reconcile();
  }

  /** Stop scheduling and remove the manager-owned tab. */
  async dispose(): Promise<void> {
    this.clearTimer();
    await this.closeTab();
  }

  private async loadPreferences(): Promise<void> {
    try {
      const values = await this.api.storage.get(["keepalive.interval", "keepalive.enabled"]);
      this.intervalMinutes = clampKeepaliveInterval(values["keepalive.interval"]);
      this.enabled = values["keepalive.enabled"] !== false;
    } catch {
      // Storage is advisory. A temporary failure must not stop an active batch.
      this.intervalMinutes = DEFAULT_INTERVAL_MINUTES;
      this.enabled = true;
    }
  }

  private async reconcile(): Promise<void> {
    const activeJobs = this.options.trackedJobCount() > 0;
    const resolver = activeJobs ? this.resolverFromLatestOffer() : undefined;
    if (!this.enabled || !activeJobs || resolver === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    this.resolver = resolver;
    if (this.tabID === undefined) {
      await this.createTab();
      this.schedule(this.intervalMs(), () => this.onReload());
      return;
    }

    if (this.reauthPaused) {
      await this.inspectTab();
      this.schedule(this.reauthPaused ? this.observeMs : this.intervalMs(), () =>
        this.reauthPaused ? this.onObserve() : this.onReload(),
      );
      return;
    }

    this.schedule(this.intervalMs(), () => this.onReload());
  }

  private resolverFromLatestOffer(): URL | undefined {
    const openurl = this.options.latestOpenURL();
    if (openurl === undefined) return undefined;
    try {
      const url = new URL(openurl);
      if (url.protocol !== "https:" && url.protocol !== "http:") return undefined;
      return url;
    } catch {
      return undefined;
    }
  }
  private setAuthenticated(authenticated: boolean): void {
    if (this.authenticated === authenticated) return;
    this.authenticated = authenticated;
    this.options.onAuthenticationChanged?.(authenticated);
  }

  private async createTab(): Promise<void> {
    if (this.resolver === undefined) return;
    try {
      const existing = await this.api.tabs.query({
        pinned: true,
        muted: true,
        url: [`${this.resolver.protocol}//${this.resolver.host}/*`],
      });
      const tabID = existing.find((tab) => tab.id !== undefined)?.id;
      if (tabID !== undefined) {
        this.tabID = tabID;
        this.reauthPaused = false;
        this.setAuthenticated(false);
        return;
      }
    } catch {
      // Querying is a best-effort restart recovery; creation below remains safe.
    }
    try {
      const tab = await this.api.tabs.create({
        url: this.resolver.origin,
        active: false,
        pinned: true,
        muted: true,
      });
      if (tab.id === undefined) return;
      this.tabID = tab.id;
      this.reauthPaused = false;
      this.setAuthenticated(false);
    } catch {
      // Browser policy may reject background tabs. Observe and try again later.
    }
  }

  private async onObserve(): Promise<void> {
    await this.loadPreferences();
    const activeJobs = this.options.trackedJobCount() > 0;
    if (!this.enabled || !activeJobs || this.resolverFromLatestOffer() === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    this.resolver = this.resolverFromLatestOffer();
    if (this.tabID === undefined) {
      await this.createTab();
      this.schedule(this.intervalMs(), () => this.onReload());
      return;
    }

    if (this.reauthPaused) {
      await this.inspectTab();
      this.schedule(this.reauthPaused ? this.observeMs : this.intervalMs(), () =>
        this.reauthPaused ? this.onObserve() : this.onReload(),
      );
      return;
    }

    this.schedule(this.intervalMs(), () => this.onReload());
  }

  private async onReload(): Promise<void> {
    await this.loadPreferences();
    if (!this.enabled || this.options.trackedJobCount() <= 0) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    this.resolver = this.resolverFromLatestOffer();
    if (this.resolver === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }
    if (this.tabID === undefined) {
      await this.createTab();
      this.schedule(this.intervalMs(), () => this.onReload());
      return;
    }
    if (this.reauthPaused) {
      await this.inspectTab();
      this.schedule(this.reauthPaused ? this.observeMs : this.intervalMs(), () =>
        this.reauthPaused ? this.onObserve() : this.onReload(),
      );
      return;
    }

    try {
      await this.api.tabs.reload(this.tabID);
    } catch {
      this.tabID = undefined;
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }
    this.schedule(this.reloadSettleMs, () => this.inspectAfterReload());
  }

  private async inspectAfterReload(): Promise<void> {
    await this.inspectTab();
    this.schedule(this.reauthPaused ? this.observeMs : this.intervalMs(), () =>
      this.reauthPaused ? this.onObserve() : this.onReload(),
    );
  }

  private async inspectTab(): Promise<void> {
    if (this.tabID === undefined || this.resolver === undefined) return;
    let tab: KeepaliveTab;
    try {
      tab = await this.api.tabs.get(this.tabID);
    } catch {
      this.tabID = undefined;
      this.reauthPaused = false;
      this.setAuthenticated(false);
      return;
    }
    if (typeof tab.url !== "string") return;

    let finalURL: URL;
    try {
      finalURL = new URL(tab.url);
    } catch {
      return;
    }
    if (finalURL.hostname === this.resolver.hostname) {
      this.setAuthenticated(true);
      if (this.reauthPaused) await this.resumeAfterReauth();
      return;
    }
    if (isAuthenticationURL(tab.url)) {
      this.setAuthenticated(false);
      await this.pauseForReauth();
    }
  }

  private async pauseForReauth(): Promise<void> {
    this.setAuthenticated(false);
    if (this.reauthPaused || this.tabID === undefined) return;
    this.reauthPaused = true;
    try {
      await this.api.tabs.update(this.tabID, { active: true, pinned: false, muted: false });
    } catch {
      // The reauth callback/badge still gives the user a recoverable signal.
    }
    try {
      await this.api.action?.setBadgeText({ text: "!" });
    } catch {
      // A badge failure must not cause additional reloads during an auth flow.
    }
    this.options.onReauthNeeded?.();
  }

  private async resumeAfterReauth(): Promise<void> {
    if (this.tabID === undefined) return;
    this.reauthPaused = false;
    try {
      await this.api.tabs.update(this.tabID, { pinned: true, muted: true });
    } catch {
      // The tab is still usable; retry normal keepalive on the next cycle.
    }
    try {
      await this.api.action?.setBadgeText({ text: "" });
    } catch {
      // Badge state is cosmetic and must not block session recovery.
    }
  }

  private intervalMs(): number {
    return this.intervalMinutes * 60_000;
  }

  private schedule(delayMs: number, callback: () => Promise<void>): void {
    this.clearTimer();
    this.timer = this.api.timers.setTimeout(async () => {
      this.timer = undefined;
      await callback();
    }, delayMs);
  }

  private clearTimer(): void {
    if (this.timer !== undefined) this.api.timers.clearTimeout(this.timer);
    this.timer = undefined;
  }

  private async closeTab(): Promise<void> {
    const tabID = this.tabID;
    const wasAwaitingReauth = this.reauthPaused;
    this.tabID = undefined;
    this.reauthPaused = false;
    this.setAuthenticated(false);
    if (wasAwaitingReauth) {
      try {
        await this.api.action?.setBadgeText({ text: "" });
      } catch {
        // Badge state is cosmetic and must not block job cleanup.
      }
    }
    if (tabID === undefined) return;
    try {
      await this.api.tabs.remove(tabID);
    } catch {
      // A manually closed tab is already in the desired state.
    }
  }
}

/** Construct and start the production manager without exposing bridge internals. */
export function initKeepalive(api: KeepaliveAPI, options: KeepaliveOptions): KeepaliveManager {
  const manager = new KeepaliveManager(api, options);
  void manager.init();
  return manager;
}

/** Build the production API while keeping Chrome globals out of manager logic. */
export function chromeKeepaliveAPI(
  chromeAPI: Pick<typeof chrome, "action" | "storage" | "tabs">,
): KeepaliveAPI {
  return {
    tabs: {
      create: (properties) => chromeAPI.tabs.create(properties),
      reload: (tabID) => chromeAPI.tabs.reload(tabID),
      get: (tabID) => chromeAPI.tabs.get(tabID),
      query: (query) => chromeAPI.tabs.query(query),
      remove: (tabID) => chromeAPI.tabs.remove(tabID),
      update: async (tabID, properties) => (await chromeAPI.tabs.update(tabID, properties)) ?? {},
    },
    storage: {
      get: (keys) => chromeAPI.storage.local.get(keys),
    },
    timers: {
      setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
      clearTimeout: (handle) => clearTimeout(handle as number),
    },
    action: {
      setBadgeText: (details) => chromeAPI.action.setBadgeText(details),
    },
  };
}
