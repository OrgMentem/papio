// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Institutional resolver session keepalive. This is deliberately independent of
// the bridge: callers supply only current job count and the latest OpenURL.

export interface KeepaliveTab {
  id?: number | undefined;
  url?: string | undefined;
}

export type SessionVerdict = "in" | "out" | "unknown";
export type KeepaliveProbeSource = "live_tab" | "keepalive_tab" | "none";
export type ScanOutcome = "markers" | "no_markers" | "scan_failed";
export interface ResolverMarker {
  text: string;
  label: string;
  href?: string;
  formAction?: string;
  action?: string;
  storageIdentity?: "in";
}

export interface KeepaliveTabs {
  create(properties: {
    url: string;
    active: boolean;
    pinned: boolean;
    muted: boolean;
    windowId?: number;
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
  set?(values: Record<string, unknown>): Promise<void>;
}

export interface KeepalivePermissions {
  getAll(): Promise<{ origins?: string[] }>;
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
  permissions?: KeepalivePermissions;
  /** Optional in tests and browsers where page scripting is unavailable. */
  scripting?: {
    executeScript(injection: {
      target: { tabId: number };
      func: (...args: never[]) => unknown;
      args?: unknown[];
    }): Promise<{ result?: unknown }[]>;
  };
  timers: KeepaliveTimers;
  /** Retained for API compatibility with injected callers. Badge painting is
   * owned by the bridge, so the manager never writes it directly. */
  action?: KeepaliveAction;
}

export interface KeepaliveSnapshot {
  enabled: boolean;
  intervalMinutes: number;
  authenticated: boolean;
  /** Completed DOM/URL verdict. Older callers may omit this field. */
  verdict?: SessionVerdict;
  /** Branch that produced the current verdict. */
  probeSource?: KeepaliveProbeSource;
  /** Result of the most recent page marker scan, when one ran. */
  scanOutcome?: ScanOutcome;
  /** Epoch milliseconds when the current verdict completed. */
  lastVerdictAt?: number | null;
  /** True while an on-demand session probe is still in flight. */
  checking?: boolean;
  /** A resolver-origin tab was observed while the probe was in flight. This
   * is evidence only; `authenticated` remains the completed probe verdict. */
  likelyAuthenticated?: boolean;
  pausedForReauth: boolean;
  lastCheckAt: number | null;
  /** The configured resolver origin, never an authentication/IdP URL. */
  resolverOrigin: string | null;
  lastAuthReturnedAt: number | null;
  queuedAuthJobs: number;
  stalledAuthJobs: string[];
}


export interface KeepaliveOptions {
  /** Number of currently non-terminal handoff jobs. */
  trackedJobCount(): number;
  /** OpenURL from the most recently received job offer, kept in bridge memory. */
  latestOpenURL(): string | undefined;
  /** Number of queued institutional handoffs waiting for auth evidence. */
  queuedAuthJobs?(): number;
  /** Job ids parked after the bounded authentication-drive budget. */
  stalledAuthJobs?(): readonly string[];
  /** Latest persisted institutional-session evidence timestamp. */
  lastAuthReturnedAt?(): number | undefined;
  /** Reports when the keepalive tab has verified an authenticated resolver
   * return, or when that evidence is lost. */
  onAuthenticationChanged?(authenticated: boolean): void;
  /** Called once per detected login redirect, after the tab is made visible. */
  onReauthNeeded?(): void;
  /** Keeps the central bridge badge in sync with the paused state. */
  onReauthStateChanged?(paused: boolean): void;
  /** Id of papio's dedicated background work window, when one exists. The
   * keepalive tab is created there so it stays out of the user's tab strip. */
  workWindowID?(): number | undefined;
  /** Called after the keepalive tab is (re)created, so tab-group mode can fold
   * it into papio's collapsed group. Best-effort; keepalive proceeds regardless. */
  onTabPlaced?(tabID: number): Promise<void>;
  /** Brings a reauth-parked keepalive tab's window to the front. Needed when
   * the tab lives in the minimized work window; tabs.update alone cannot
   * surface it. Best-effort. */
  surfaceReauthTab?(tabID: number): Promise<void>;
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
const AUTH_HOST_SEGMENTS: Record<string, true> = {
  idp: true,
  sso: true,
  login: true,
  signin: true,
  auth: true,
  shibboleth: true,
  openathens: true,
};

/** Shared conservative detector for login/IdP routes. */
export function isAuthenticationURL(rawURL: string): boolean {
  try {
    const url = new URL(rawURL);
    const hostnameHasAuthSegment = url.hostname
      .toLowerCase()
      .split(".")
      .some((segment) => AUTH_HOST_SEGMENTS[segment] === true);
    return LOGIN_ROUTE.test(url.pathname) || hostnameHasAuthSegment;
  } catch {
    return false;
  }
}

const SIGN_OUT_MARKER = /sign\s*out|log\s*out|logout|signout|sign-out|log-out|my account/i;
const SIGN_IN_MARKER = /sign\s*in|log\s*in|login/i;
const MAX_STORAGE_VALUE_LENGTH = 8 * 1024;
const JWT_SEGMENT = /^[A-Za-z0-9_-]+$/;

function decodeJWTPart(part: string): string | undefined {
  if (part.length === 0 || !JWT_SEGMENT.test(part) || part.length % 4 === 1) return undefined;
  const normalized = part.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
  try {
    const binary = atob(padded);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return undefined;
  }
}

function decodeJWTPayload(raw: string): Record<string, unknown> | undefined {
  if (typeof raw !== "string" || raw.length >= MAX_STORAGE_VALUE_LENGTH) return undefined;
  const parts = raw.trim().split(".");
  if (
    parts.length !== 3 ||
    parts.some((part) => part.length === 0 || part.length % 4 === 1 || !JWT_SEGMENT.test(part))
  ) {
    return undefined;
  }
  const payloadPart = parts[1];
  if (payloadPart === undefined) return undefined;
  const payloadText = decodeJWTPart(payloadPart);
  if (payloadText === undefined) return undefined;
  try {
    const payload: unknown = JSON.parse(payloadText);
    return typeof payload === "object" && payload !== null && !Array.isArray(payload)
      ? payload as Record<string, unknown>
      : undefined;
  } catch {
    return undefined;
  }
}


/** Classify JWT-shaped storage values without touching browser state. */
export function classifyResolverJWTIdentity(values: readonly string[]): "in" | "unknown" {
  for (const value of values) {
    const payload = decodeJWTPayload(value);
    if (payload === undefined) continue;
    const hasIdentity = ["userName", "user_name", "preferred_username", "sub"].some((claim) => {
      const identity = payload[claim];
      return (
        (typeof identity === "string" && identity.trim().length > 0) ||
        (typeof identity === "number" && Number.isFinite(identity))
      );
    });
    const userGroup = payload["userGroup"] ?? payload["user_group"];
    if (hasIdentity && userGroup !== "GUEST") return "in";
  }
  return "unknown";
}

/** Classify bounded resolver-page marker data without touching browser state. */
export function classifyResolverMarkers(markers: readonly ResolverMarker[]): SessionVerdict {
  let signIn = false;
  let storageIdentity = false;
  for (const marker of markers) {
    if (
      typeof marker?.text !== "string" ||
      typeof marker?.label !== "string"
    ) {
      continue;
    }
    if (marker.storageIdentity === "in") storageIdentity = true;
    const value = [
      marker.text,
      marker.label,
      marker.href,
      marker.formAction,
      marker.action,
    ]
      .filter((part): part is string => typeof part === "string")
      .join(" ");
    if (SIGN_OUT_MARKER.test(value)) return "in";
    if (SIGN_IN_MARKER.test(value)) signIn = true;
  }
  if (storageIdentity) return "in";
  return signIn ? "out" : "unknown";
}

/** Serializable page function used by chrome.scripting.executeScript. */
export function collectResolverMarkers(): ResolverMarker[] {
  const maxStorageValueLength = 8 * 1024;
  const elements = Array.from(
    // Include every DOM element: closed details and hidden menu content is
    // still useful sign-in evidence even when it is not rendered.
    document.querySelectorAll("*"),
  );
  const markers: ResolverMarker[] = elements.map((element) => {
    const marker: ResolverMarker = {
      text: element.textContent?.trim() ?? "",
      label: element.getAttribute("aria-label")?.trim() ?? "",
    };
    const href = element.getAttribute("href")?.trim();
    if (href !== undefined && href.length > 0) marker.href = href;
    const formAction = element.getAttribute("formaction")?.trim();
    if (formAction !== undefined && formAction.length > 0) marker.formAction = formAction;
    const action = element.getAttribute("action")?.trim();
    if (action !== undefined && action.length > 0) marker.action = action;
    return marker;
  });

  const storageValues: string[] = [];
  const readStorage = (storage: Storage): void => {
    try {
      for (let index = 0; index < Math.min(storage.length, 50); index += 1) {
        const value = storage.getItem(storage.key(index) ?? "");
        if (typeof value === "string" && value.length < maxStorageValueLength) {
          storageValues.push(value);
        }
      }
    } catch {
      // Some privileged pages expose DOM but deny storage access.
    }
  };
  try {
    readStorage(localStorage);
  } catch {
    // Storage access can throw before the object is passed to readStorage.
  }
  try {
    readStorage(sessionStorage);
  } catch {
    // Storage access can throw before the object is passed to readStorage.
  }

  // Keep this helper self-contained: executeScript serializes only the
  // injected function, not its module-level dependencies.
  const hasStorageIdentity = (values: readonly string[]): boolean => {
    for (const value of values) {
      if (typeof value !== "string" || value.length >= maxStorageValueLength) continue;
      const parts = value.trim().split(".");
      if (
        parts.length !== 3 ||
        parts.some((part) => part.length === 0 || part.length % 4 === 1 || !/^[A-Za-z0-9_-]+$/.test(part))
      ) {
        continue;
      }
      try {
        const normalized = parts[1]!.replace(/-/g, "+").replace(/_/g, "/");
        const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
        const binary = atob(padded);
        const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
        const payload: unknown = JSON.parse(new TextDecoder().decode(bytes));
        if (typeof payload !== "object" || payload === null || Array.isArray(payload)) continue;
        const record = payload as Record<string, unknown>;
        const hasIdentity = ["userName", "user_name", "preferred_username", "sub"].some((claim) => {
          const identity = record[claim];
          return (
            (typeof identity === "string" && identity.trim().length > 0) ||
            (typeof identity === "number" && Number.isFinite(identity))
          );
        });
        if (hasIdentity && (record["userGroup"] ?? record["user_group"]) !== "GUEST") return true;
      } catch {
        // Ordinary storage values and malformed JWTs are not identity evidence.
      }
    }
    return false;
  };
  if (hasStorageIdentity(storageValues)) {
    markers.push({ text: "", label: "", storageIdentity: "in" });
  }
  return markers;
}


/** Clamp an untrusted storage value to the supported interval range. */
export function clampKeepaliveInterval(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value)) return DEFAULT_INTERVAL_MINUTES;
  return Math.min(MAX_INTERVAL_MINUTES, Math.max(MIN_INTERVAL_MINUTES, Math.trunc(value)));
}
/** Durable browser-local resolver origin. This is an origin only, never an
 * OpenURL path, query, fragment, or identity-provider URL. */
export const KEEPALIVE_RESOLVER_ORIGIN_KEY = "keepalive.resolverOrigin";
/** Session probes are needed after two minutes without a completed check. */
export const SESSION_STALE_MS = 2 * 60_000;
const ON_DEMAND_PROBE_BUDGET_MS = 1_400;

function normalizeHttpsOrigin(raw: unknown, allowWildcardHost = false): string | undefined {
  if (typeof raw !== "string" || raw.length === 0) return undefined;
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || url.username !== "" || url.password !== "") return undefined;
    if (url.hostname === "*" || (url.hostname.includes("*") && !allowWildcardHost)) return undefined;
    if (url.pathname !== "/" || url.search !== "" || url.hash !== "") return undefined;
    return url.origin;
  } catch {
    return undefined;
  }
}
function resolverURLMatches(raw: string, resolver: URL): boolean {
  try {
    const candidate = new URL(raw);
    if (candidate.protocol !== resolver.protocol || candidate.port !== resolver.port) return false;
    if (resolver.hostname.startsWith("*.")) {
      const suffix = resolver.hostname.slice(2);
      return candidate.hostname === suffix || candidate.hostname.endsWith(`.${suffix}`);
    }
    return candidate.origin === resolver.origin;
  } catch {
    return false;
  }
}


/** Extract one exact or host-wildcard HTTPS origin from a Chrome permission
 * pattern. Broad HTTPS all-host access is intentionally not a resolver. */
export function resolverOriginFromPermissionPattern(raw: unknown): string | undefined {
  if (typeof raw !== "string" || !/^https:\/\//i.test(raw)) return undefined;
  const withoutScheme = raw.slice("https://".length);
  const host = withoutScheme.split(/[/?#]/, 1)[0];
  if (host === undefined || host === "" || host === "*" || !/^[*a-z0-9.-]+(?::\d+)?$/i.test(host)) {
    return undefined;
  }
  return normalizeHttpsOrigin(`https://${host}`, true);
}

/** Normalize granted resolver permission patterns, preserving declaration
 * order while removing duplicates and broad all-host grants. */
export function resolverOriginsFromPermissionPatterns(
  patterns: readonly string[] | undefined,
): string[] {
  if (!Array.isArray(patterns)) return [];
  return [...new Set(patterns.map(resolverOriginFromPermissionPattern).filter((origin): origin is string => origin !== undefined))];
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
  private persistedResolverOrigin: string | undefined;
  private grantedResolverOrigin: string | undefined;
  private intervalMinutes = DEFAULT_INTERVAL_MINUTES;
  private enabled = true;
  private reauthPaused = false;
  private authenticated = false;
  private verdict: SessionVerdict = "unknown";
  private probeSource: KeepaliveProbeSource = "none";
  private scanOutcome: ScanOutcome | undefined;
  private lastVerdictAt: number | undefined;
  private lastCheckAt: number | undefined;
  private checking = false;
  private likelyAuthenticated = false;
  private probePromise: Promise<void> | undefined;

  /** Browser-local session state for privileged extension surfaces. */
  getSnapshot(): KeepaliveSnapshot {
    const resolverOrigin =
      this.resolver?.protocol === "https:"
        ? this.resolver.origin
        : this.persistedResolverOrigin ?? this.grantedResolverOrigin;
    const lastAuthReturnedAt = this.options.lastAuthReturnedAt?.();
    return {
      enabled: this.enabled,
      intervalMinutes: this.intervalMinutes,
      authenticated: this.authenticated,
      verdict: this.verdict,
      probeSource: this.probeSource,
      ...(this.scanOutcome === undefined ? {} : { scanOutcome: this.scanOutcome }),
      lastVerdictAt: this.lastVerdictAt ?? null,
      checking: this.checking,
      likelyAuthenticated: this.likelyAuthenticated,
      pausedForReauth: this.reauthPaused,
      lastCheckAt: this.lastCheckAt ?? null,
      resolverOrigin: resolverOrigin ?? null,
      lastAuthReturnedAt:
        typeof lastAuthReturnedAt === "number" && Number.isFinite(lastAuthReturnedAt)
          ? lastAuthReturnedAt
          : null,
      queuedAuthJobs: Math.max(0, Math.trunc(this.options.queuedAuthJobs?.() ?? 0)),
      stalledAuthJobs: [
        ...new Set(
          (this.options.stalledAuthJobs?.() ?? []).filter(
            (jobID): jobID is string => typeof jobID === "string" && jobID.length > 0,
          ),
        ),
      ],
    };
  }
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
  /** Record a resolver as soon as a handoff arrives, even if the periodic
   * keepalive observation has not run in this worker lifetime. */
  learnResolver(openURL: string): void {
    if (isAuthenticationURL(openURL)) return;
    try {
      const resolver = new URL(openURL);
      if (resolver.protocol !== "https:") return;
      this.resolver = resolver;
      this.rememberResolverOrigin(resolver);
    } catch {
      // Invalid offers are rejected by the normal handoff parser.
    }
  }
  /** True when no probe has completed or the completed result is older than
   * the popup freshness budget. */
  isSessionStale(now = Date.now()): boolean {
    if (this.lastCheckAt === undefined || !Number.isFinite(this.lastCheckAt)) return true;
    return now - this.lastCheckAt > SESSION_STALE_MS;
  }

  /** Probe the current resolver immediately. A slow browser API is bounded so
   * a foreground popup request never waits beyond the MV3 response budget. */
  async checkNow(budgetMs = ON_DEMAND_PROBE_BUDGET_MS): Promise<void> {
    if (this.probePromise === undefined && !this.isSessionStale()) return;
    const probe = this.probePromise ?? this.startProbe();
    const boundedBudget = Math.min(1_500, Math.max(0, Math.trunc(budgetMs)));
    let timeout: ReturnType<typeof setTimeout> | undefined;
    const deadline = new Promise<void>((resolve) => {
      timeout = setTimeout(resolve, boundedBudget);
    });
    try {
      await Promise.race([probe, deadline]);
    } finally {
      if (timeout !== undefined) clearTimeout(timeout);
    }
  }

  private startProbe(): Promise<void> {
    this.checking = true;
    this.likelyAuthenticated = false;
    const probe = this.probeResolver();
    const settled = probe.finally(() => {
      this.checking = false;
      this.likelyAuthenticated = false;
      this.probePromise = undefined;
    });
    this.probePromise = settled;
    return settled;
  }

  private async probeResolver(): Promise<void> {
    await this.loadPreferences();
    const resolver =
      this.resolverFromLatestOffer() ?? this.configuredResolver() ?? this.resolver;
    if (resolver === undefined || resolver.protocol !== "https:") {
      const checkedAt = Date.now();
      this.lastCheckAt = checkedAt;
      this.setVerdict("unknown", "none", checkedAt);
      return;
    }
    this.resolver = resolver;

    let liveTabs: KeepaliveTab[] = [];
    try {
      liveTabs = await this.api.tabs.query({ url: [`${resolver.origin}/*`] });
    } catch {
      // The permission may have been revoked while the popup was open. The
      // manager-owned probe below remains useful when it is still available.
    }
    const resolverTabs = liveTabs.filter(
      (tab) => tab.id !== undefined && typeof tab.url === "string" && resolverURLMatches(tab.url, resolver),
    );
    // A user's visible resolver tab carries the strongest, freshest evidence.
    // Only inspect the manager-owned tab when no user tab is available.
    const liveTab =
      resolverTabs.find((tab) => tab.id !== this.tabID) ?? resolverTabs[0];
    this.likelyAuthenticated = liveTab !== undefined;
    if (liveTab?.id !== undefined) {
      await this.inspectTab(liveTab.id);
      return;
    }
    if (this.tabID !== undefined) {
      await this.inspectTab(this.tabID);
      return;
    }
    const checkedAt = Date.now();
    this.lastCheckAt = checkedAt;
    this.setVerdict("unknown", "none", checkedAt);
  }


  /** Stop scheduling and remove the manager-owned tab. */
  async dispose(): Promise<void> {
    this.clearTimer();
    await this.closeTab();
  }

  private async loadPreferences(): Promise<void> {
    let values: Record<string, unknown> = {};
    let storageReadSucceeded = false;
    try {
      values = await this.api.storage.get([
        "keepalive.interval",
        "keepalive.enabled",
        KEEPALIVE_RESOLVER_ORIGIN_KEY,
      ]);
      storageReadSucceeded = true;
    } catch {
      // Storage is advisory. A temporary failure must not stop an active batch.
    }
    this.intervalMinutes = clampKeepaliveInterval(values["keepalive.interval"]);
    this.enabled = values["keepalive.enabled"] !== false;

    if (storageReadSucceeded) {
      this.persistedResolverOrigin = normalizeHttpsOrigin(values[KEEPALIVE_RESOLVER_ORIGIN_KEY]);
    }
    this.grantedResolverOrigin = undefined;
    if (this.persistedResolverOrigin === undefined && this.api.permissions !== undefined) {
      try {
        const granted = await this.api.permissions.getAll();
        this.grantedResolverOrigin = resolverOriginsFromPermissionPatterns(granted.origins)[0];
      } catch {
        // Optional permissions are advisory; the popup can still explain the
        // missing resolver and an incoming handoff can seed storage later.
      }
    }
  }

  private configuredResolver(): URL | undefined {
    const origin = this.persistedResolverOrigin ?? this.grantedResolverOrigin;
    if (origin === undefined) return undefined;
    try {
      return new URL(origin);
    } catch {
      return undefined;
    }
  }

  private async reconcile(): Promise<void> {
    const activeJobs = this.options.trackedJobCount() > 0;
    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
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

  /** Focus the reauthentication tab on an explicit operator request. If the
   * keepalive is disabled or has not observed a job yet, this still creates a
   * resolver-origin tab from the latest institutional offer when possible. */
  async openReauth(): Promise<boolean> {
    await this.loadPreferences();
    if (this.resolver === undefined) {
      this.resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    }
    if (this.resolver?.protocol !== "https:") return false;
    if (this.tabID === undefined) await this.createTab();
    const tabID = this.tabID;
    if (tabID === undefined) return false;
    if (this.authenticated && !this.reauthPaused) {
      try {
        await this.api.tabs.update(tabID, { active: true });
        await this.options.surfaceReauthTab?.(tabID);
      } catch {
        return false;
      }
      return true;
    }
    if (!this.reauthPaused) {
      await this.pauseForReauth();
    } else {
      try {
        await this.api.tabs.update(tabID, { active: true, pinned: false, muted: false });
        await this.options.surfaceReauthTab?.(tabID);
      } catch {
        return false;
      }
    }
    return true;
  }

  private resolverFromLatestOffer(): URL | undefined {
    const openurl = this.options.latestOpenURL();
    if (openurl === undefined || isAuthenticationURL(openurl)) return undefined;
    try {
      const url = new URL(openurl);
      if (url.protocol !== "https:") return undefined;
      this.rememberResolverOrigin(url);
      return url;
    } catch {
      return undefined;
    }
  }

  private rememberResolverOrigin(resolver: URL): void {
    const origin = normalizeHttpsOrigin(resolver.origin);
    if (origin === undefined || this.persistedResolverOrigin === origin) return;
    this.persistedResolverOrigin = origin;
    const save = this.api.storage.set?.({ [KEEPALIVE_RESOLVER_ORIGIN_KEY]: origin });
    if (save !== undefined) void save.catch(() => {});
  }

  private async resolverMarkerVerdict(
    tabID: number,
  ): Promise<{ verdict: SessionVerdict; scanOutcome: ScanOutcome }> {
    const executeScript = this.api.scripting?.executeScript;
    if (executeScript === undefined) {
      return { verdict: "unknown", scanOutcome: "scan_failed" };
    }
    try {
      const [injection] = await executeScript({
        target: { tabId: tabID },
        func: collectResolverMarkers,
      });
      const markers = injection?.result;
      if (!Array.isArray(markers)) {
        return { verdict: "unknown", scanOutcome: "scan_failed" };
      }
      const verdict = classifyResolverMarkers(markers as ResolverMarker[]);
      return {
        verdict,
        scanOutcome: verdict === "unknown" ? "no_markers" : "markers",
      };
    } catch {
      // Privileged pages, revoked host permission, and closed tabs expose a
      // distinct scan failure so the popup can explain the missing access.
      return { verdict: "unknown", scanOutcome: "scan_failed" };
    }
  }

  private setVerdict(
    verdict: SessionVerdict,
    source: KeepaliveProbeSource,
    completedAt: number | null = Date.now(),
    scanOutcome: ScanOutcome | undefined = undefined,
  ): void {
    const authenticated = verdict === "in";
    const authenticationChanged = this.authenticated !== authenticated;
    this.verdict = verdict;
    this.probeSource = source;
    this.scanOutcome = scanOutcome;
    this.lastVerdictAt = completedAt === null ? undefined : completedAt;
    this.authenticated = authenticated;
    if (authenticationChanged) this.options.onAuthenticationChanged?.(authenticated);
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
        this.setVerdict("unknown", "none", null);
        return;
      }
    } catch {
      // Querying is a best-effort restart recovery; creation below remains safe.
    }
    const base = {
      url: this.resolver.origin,
      active: false,
      pinned: true,
      muted: true,
    };
    const windowID = this.options.workWindowID?.();
    try {
      let tab: KeepaliveTab;
      try {
        tab = await this.api.tabs.create(
          windowID !== undefined ? { ...base, windowId: windowID } : base,
        );
      } catch (e) {
        // The work window may have been closed between lookup and create;
        // fall back to the user's current window rather than skip a cycle.
        if (windowID === undefined) throw e;
        tab = await this.api.tabs.create(base);
      }
      if (tab.id === undefined) return;
      this.tabID = tab.id;
      this.reauthPaused = false;
      this.setVerdict("unknown", "none", null);
      await this.options.onTabPlaced?.(tab.id);
    } catch {
      // Browser policy may reject background tabs. Observe and try again later.
    }
  }

  private async onObserve(): Promise<void> {
    await this.loadPreferences();
    const activeJobs = this.options.trackedJobCount() > 0;
    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
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

  private async onReload(): Promise<void> {
    await this.loadPreferences();
    if (!this.enabled || this.options.trackedJobCount() <= 0) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    this.resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
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

  private async inspectTab(tabID = this.tabID): Promise<void> {
    if (tabID === undefined || this.resolver === undefined) return;
    const owned = tabID === this.tabID;
    const source: KeepaliveProbeSource = owned ? "keepalive_tab" : "live_tab";
    let tab: KeepaliveTab;
    try {
      tab = await this.api.tabs.get(tabID);
    } catch {
      const checkedAt = Date.now();
      this.lastCheckAt = checkedAt;
      if (!owned) {
        this.setVerdict("unknown", source, checkedAt);
        return;
      }
      this.tabID = undefined;
      const wasPaused = this.reauthPaused;
      this.reauthPaused = false;
      if (wasPaused) this.options.onReauthStateChanged?.(false);
      this.setVerdict("unknown", "none", checkedAt);
      return;
    }
    const checkedAt = Date.now();
    this.lastCheckAt = checkedAt;
    if (typeof tab.url !== "string") {
      this.setVerdict("unknown", source, checkedAt);
      return;
    }

    if (resolverURLMatches(tab.url, this.resolver)) {
      const markerResult = await this.resolverMarkerVerdict(tabID);
      // An auth-shaped resolver path remains a conservative signed-out
      // fallback. A plain resolver URL has no affirmative URL evidence.
      const verdict =
        markerResult.verdict === "unknown"
          ? isAuthenticationURL(tab.url)
            ? "out"
            : "unknown"
          : markerResult.verdict;
      this.setVerdict(verdict, source, Date.now(), markerResult.scanOutcome);
      if (verdict === "in" && owned && this.reauthPaused) await this.resumeAfterReauth();
      if (verdict === "out" && owned) await this.pauseForReauth();
      return;
    }
    if (isAuthenticationURL(tab.url)) {
      this.setVerdict("out", source, checkedAt);
      if (owned) await this.pauseForReauth();
      return;
    }
    this.setVerdict("unknown", source, checkedAt);
  }

  private async pauseForReauth(): Promise<void> {
    if (this.reauthPaused || this.tabID === undefined) return;
    this.reauthPaused = true;
    try {
      await this.api.tabs.update(this.tabID, { active: true, pinned: false, muted: false });
      // In work-window mode the tab lives in a minimized window; bring it up.
      await this.options.surfaceReauthTab?.(this.tabID);
    } catch {
      // The reauth callback/badge still gives the user a recoverable signal.
    }
    this.options.onReauthStateChanged?.(true);
    this.options.onReauthNeeded?.();
  }

  private async resumeAfterReauth(): Promise<void> {
    if (this.tabID === undefined) return;
    this.reauthPaused = false;
    this.options.onReauthStateChanged?.(false);
    try {
      await this.api.tabs.update(this.tabID, { pinned: true, muted: true });
    } catch {
      // The tab is still usable; retry normal keepalive on the next cycle.
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
    if (wasAwaitingReauth) this.options.onReauthStateChanged?.(false);
    this.setVerdict("unknown", "none", null);
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
  chromeAPI: Pick<typeof chrome, "action" | "storage" | "tabs" | "permissions"> & {
    scripting?: Pick<typeof chrome.scripting, "executeScript">;
  },
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
      set: (values) => chromeAPI.storage.local.set(values),
    },
    permissions: {
      getAll: () => chromeAPI.permissions.getAll(),
    },
    ...(chromeAPI.scripting === undefined
      ? {}
      : {
          scripting: {
            executeScript: (injection: {
              target: { tabId: number };
              func: (...args: never[]) => unknown;
              args?: unknown[];
            }) => chromeAPI.scripting!.executeScript(injection),
          },
        }),
    timers: {
      setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
      clearTimeout: (handle) => clearTimeout(handle as number),
    },
    action: {
      setBadgeText: (details) => chromeAPI.action.setBadgeText(details),
    },
  };
}
