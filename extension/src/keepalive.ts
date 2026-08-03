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
  /** Whether the control is actually rendered for the user. Sign-OUT
   * affordances legitimately hide inside closed account menus, so hidden
   * markers may prove "in" — but a signed-out page puts its sign-in prompt
   * front and center, so only a VISIBLE sign-in affordance may assert "out".
   * A page whose only sign-in markers are buried in drawer templates (Primo
   * NDE keeps one there permanently, signed in or not) stays unknown. */
  visible?: boolean;
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
  query(query: {
    pinned?: boolean;
    muted?: boolean;
    url?: string[];
    active?: boolean;
    lastFocusedWindow?: boolean;
  }): Promise<KeepaliveTab[]>;
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

export interface KeepaliveOriginSnapshot {
  /** The resolver origin this state belongs to; never an IdP URL. */
  origin: string;
  authenticated: boolean;
  verdict: SessionVerdict;
  probeSource: KeepaliveProbeSource;
  scanOutcome?: ScanOutcome;
  lastVerdictAt: number | null;
  checking: boolean;
  likelyAuthenticated: boolean;
  pausedForReauth: boolean;
  lastCheckAt: number | null;
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
  /** Durable daemon-side institutional work demand. */
  warmDemand?(): boolean;
  /** OpenURL from the most recently received job offer, kept in bridge memory. */
  latestOpenURL(): string | undefined;
  /** Configured resolver origins from the daemon hello exchange. */
  knownResolverOrigins?(): readonly string[];
  /** Number of queued institutional handoffs waiting for auth evidence. */
  queuedAuthJobs?(): number;
  /** Job ids parked after the bounded authentication-drive budget. */
  stalledAuthJobs?(): readonly string[];
  /** Latest persisted institutional-session evidence timestamp. */
  lastAuthReturnedAt?(): number | undefined;
  /** Reports a completed unknown/out -> in probe so the bridge can emit
   * session_evidence without exposing page details. */
  onSessionEvidence?(source: "live_tab" | "keepalive_tab", origin?: string): void;
  /** Reports when the keepalive tab has verified an authenticated resolver
   * return, or when that evidence is lost. */
  onAuthenticationChanged?(authenticated: boolean, origin?: string): void;
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


/** A JWT's identity claims are evidence only while an explicit expiration is
 * still in the future. Tokens without exp retain the resolver's legacy
 * session semantics; malformed or expired exp claims are rejected. */
function jwtPayloadIsUnexpired(payload: Record<string, unknown>): boolean {
  const exp = payload["exp"];
  if (exp === undefined) return true;
  return typeof exp === "number" && Number.isFinite(exp) && exp > Date.now() / 1_000;
}

/** Classify JWT-shaped storage values without touching browser state.
 *
 * A bare `sub` claim is NOT identity: anonymous session tokens carry opaque
 * subs on many platforms. Signed-in requires either a named-user claim, or
 * an Ex Libris-style explicit group claim that is not GUEST alongside a sub. */
export function classifyResolverJWTIdentity(values: readonly string[]): "in" | "unknown" {
  for (const value of values) {
    const payload = decodeJWTPayload(value);
    if (payload === undefined || !jwtPayloadIsUnexpired(payload)) continue;
    const named = ["userName", "user_name", "preferred_username", "name", "email"].some((claim) => {
      const identity = payload[claim];
      return (
        (typeof identity === "string" && identity.trim().length > 0) ||
        (typeof identity === "number" && Number.isFinite(identity))
      );
    });
    const group = payload["userGroup"] ?? payload["user_group"];
    if (group === "GUEST") continue;
    if (named) return "in";
    const sub = payload["sub"];
    const subPresent =
      (typeof sub === "string" && sub.trim().length > 0) ||
      (typeof sub === "number" && Number.isFinite(sub));
    if (subPresent && typeof group === "string" && group.trim().length > 0) return "in";
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
    if (SIGN_IN_MARKER.test(value) && marker.visible !== false) signIn = true;
  }
  if (storageIdentity) return "in";
  return signIn ? "out" : "unknown";
}

/** Serializable page function used by chrome.scripting.executeScript. */
export function collectResolverMarkers(): ResolverMarker[] {
  const maxStorageValueLength = 8 * 1024;
  const elements = Array.from(
    // Read only user-facing controls and their targets. Scanning every node
    // would include script/style/template source and ancestor textContent that
    // aggregates unrelated descendant labels.
    document.querySelectorAll<HTMLElement>(
      "a,button,input,select,textarea,form,summary,label,[role='button'],[role='link']",
    ),
  );
  const ignoredTextTags = new Set(["SCRIPT", "STYLE", "TEMPLATE"]);
  const controlText = (node: Node): string => {
    if (node.nodeType === 3) return node.nodeValue ?? "";
    if (node.nodeType !== 1) return "";
    const child = node as Element;
    if (ignoredTextTags.has(child.tagName)) return "";
    return Array.from(child.childNodes).map(controlText).join(" ");
  };
  const markers: ResolverMarker[] = elements.map((element) => {
    const value = element.getAttribute("value")?.trim() ?? "";
    // A form's action is a target, not a page-sized text aggregate. Its
    // controls are scanned independently below.
    const text = element.tagName === "FORM" ? "" : controlText(element).trim();
    const rect = element.getClientRects();
    const marker: ResolverMarker = {
      text: `${text} ${value}`.trim(),
      label: element.getAttribute("aria-label")?.trim() ?? "",
      visible: rect.length > 0 && element.checkVisibility?.() !== false,
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
        const record = payload as Record<string, unknown>;
        const exp = record["exp"];
        if (exp !== undefined && (typeof exp !== "number" || !Number.isFinite(exp) || exp <= Date.now() / 1_000)) {
          continue;
        }
        const named = ["userName", "user_name", "preferred_username", "name", "email"].some(
          (claim) => {
            const identity = record[claim];
            return (
              (typeof identity === "string" && identity.trim().length > 0) ||
              (typeof identity === "number" && Number.isFinite(identity))
            );
          },
        );
        const group = record["userGroup"] ?? record["user_group"];
        if (group === "GUEST") continue;
        if (named) return true;
        const sub = record["sub"];
        const subPresent =
          (typeof sub === "string" && sub.trim().length > 0) ||
          (typeof sub === "number" && Number.isFinite(sub));
        if (subPresent && typeof group === "string" && group.trim().length > 0) {
          return true;
        }
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
/** Per-origin session snapshots survive service-worker naps here: an origin's
 * warm verdict must not decay to "unknown" just because the worker slept. */
export const KEEPALIVE_ORIGIN_STATES_KEY = "keepalive.originStates";
/** Session probes are needed after two minutes without a completed check. */
export const SESSION_STALE_MS = 2 * 60_000;
const ON_DEMAND_PROBE_BUDGET_MS = 1_400;

function normalizeHttpsOrigin(raw: unknown, allowWildcardHost = false): string | undefined {
  if (typeof raw !== "string" || raw.length === 0) return undefined;
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || url.username !== "" || url.password !== "") return undefined;
    const hostname = url.hostname.toLowerCase();
    // Chrome percent-encodes `*` when a permission PATTERN is parsed as a
    // URL; an encoded wildcard is still a pattern, never an origin.
    if (hostname === "*" || hostname === "%2a") return undefined;
    if ((hostname.includes("*") || hostname.includes("%2a")) && !allowWildcardHost) return undefined;
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
  private grantedResolverOrigins: string[] = [];
  private readonly originStates = new Map<string, KeepaliveOriginSnapshot>();
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

  private originCandidates(): string[] {
    const candidates: unknown[] = [];
    try {
      candidates.push(...(this.options.knownResolverOrigins?.() ?? []));
    } catch {
      // The bridge's negotiated-origin cache is advisory.
    }
    // The row universe is the CONFIGURED institutions (daemon hello), with
    // the persisted/current resolver as pre-hello fallbacks. Permission
    // grants are deliberately excluded: "Grant all sources" hands papio
    // dozens of provider-host patterns, and every one of them rendered as a
    // phantom institution row.
    candidates.push(
      this.persistedResolverOrigin,
      this.resolver?.protocol === "https:" ? this.resolver.origin : undefined,
    );
    return [...new Set(
      candidates
        .map((candidate) => normalizeHttpsOrigin(candidate))
        .filter((origin): origin is string => origin !== undefined),
    )];
  }

  private defaultOriginSnapshot(origin: string): KeepaliveOriginSnapshot {
    return {
      origin,
      authenticated: false,
      verdict: "unknown",
      probeSource: "none",
      lastVerdictAt: null,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      lastCheckAt: null,
    };
  }

  private syncOriginStates(): void {
    const origins = this.originCandidates();
    const known = new Set(origins);
    for (const origin of origins) {
      if (!this.originStates.has(origin)) this.originStates.set(origin, this.defaultOriginSnapshot(origin));
    }
    for (const origin of this.originStates.keys()) {
      if (!known.has(origin)) this.originStates.delete(origin);
    }
  }

  /** Return one independently tracked verdict for every configured resolver. */
  getOriginSnapshots(): KeepaliveOriginSnapshot[] {
    this.syncOriginStates();
    return [...this.originStates.values()].map((snapshot) => ({ ...snapshot }));
  }
  private updateOriginSnapshot(
    origin: string | undefined,
    patch: Partial<KeepaliveOriginSnapshot>,
    clearScanOutcome = false,
  ): void {
    if (origin === undefined) return;
    const normalized = normalizeHttpsOrigin(origin);
    if (normalized === undefined) return;
    const current = this.originStates.get(normalized) ?? this.defaultOriginSnapshot(normalized);
    const next = { ...current, ...patch, origin: normalized };
    if (clearScanOutcome) delete next.scanOutcome;
    this.originStates.set(normalized, next);
    this.persistOriginStates();
  }

  /** Guards the persist path during startup: an early snapshot update must
   * not overwrite stored evidence before loadPreferences has restored it. */
  private originStatesRestored = false;

  private persistOriginStates(): void {
    if (!this.originStatesRestored) return;
    const save = this.api.storage.set?.({
      [KEEPALIVE_ORIGIN_STATES_KEY]: [...this.originStates.values()],
    });
    if (save !== undefined) void save.catch(() => {});
  }

  private restoreOriginStates(raw: unknown): void {
    if (!Array.isArray(raw)) return;
    for (const entry of raw) {
      if (typeof entry !== "object" || entry === null) continue;
      const snapshot = entry as KeepaliveOriginSnapshot;
      const origin = normalizeHttpsOrigin(snapshot.origin);
      if (origin === undefined) continue;
      // A pre-seeded default (no completed check) is not evidence — restored
      // state wins over it, but never over a live probe's result.
      const existing = this.originStates.get(origin);
      if (existing !== undefined && existing.lastCheckAt !== null) continue;
      // Restored evidence keeps its original timestamps: freshness gates in
      // the popup decide how much to trust it, never a worker restart.
      this.originStates.set(origin, { ...snapshot, origin, checking: false });
    }
  }


  /** Browser-local session state for privileged extension surfaces. */
  getSnapshot(): KeepaliveSnapshot {
    this.syncOriginStates();
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
      this.syncOriginStates();
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

  private hasStaleOrigin(now = Date.now()): boolean {
    this.syncOriginStates();
    for (const snapshot of this.originStates.values()) {
      if (snapshot.lastCheckAt === null || now - snapshot.lastCheckAt > SESSION_STALE_MS) return true;
    }
    return false;
  }

  /** Probe the known resolver origins immediately. A slow browser API is
   * bounded so a foreground popup request never waits beyond the MV3 budget. */
  async checkNow(budgetMs = ON_DEMAND_PROBE_BUDGET_MS): Promise<void> {
    // A completed probe that produced NO evidence (no tab inspected) must not
    // latch: the operator may have just focused the library page, and serving
    // the empty verdict for SESSION_STALE_MS reads as papio being blind.
    const latchedEmpty = this.verdict === "unknown" && this.probeSource === "none";
    if (
      this.probePromise === undefined &&
      !this.isSessionStale() &&
      !this.hasStaleOrigin() &&
      !latchedEmpty
    ) {
      return;
    }
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
    this.syncOriginStates();
    this.checking = true;
    this.likelyAuthenticated = false;
    for (const origin of this.originStates.keys()) {
      this.updateOriginSnapshot(origin, { checking: true, likelyAuthenticated: false });
    }
    const probe = this.probeResolver();
    const settled = probe.finally(() => {
      this.checking = false;
      this.likelyAuthenticated = false;
      for (const origin of this.originStates.keys()) {
        this.updateOriginSnapshot(origin, { checking: false, likelyAuthenticated: false });
      }
      this.probePromise = undefined;
    });
    this.probePromise = settled;
    return settled;
  }

  private async probeResolver(): Promise<void> {
    await this.loadPreferences();
    const configured =
      this.resolverFromLatestOffer() ?? this.configuredResolver() ?? this.resolver;
    if (configured !== undefined && configured.protocol === "https:") {
      await this.selectResolver(configured);
    }

    const origins = this.originCandidates();
    if (origins.length === 0) {
      const checkedAt = Date.now();
      this.lastCheckAt = checkedAt;
      this.setVerdict("unknown", "none", checkedAt);
      return;
    }
    if (this.resolver === undefined) {
      try {
        this.resolver = new URL(origins[0]!);
      } catch {
        const checkedAt = Date.now();
        this.lastCheckAt = checkedAt;
        this.setVerdict("unknown", "none", checkedAt);
        return;
      }
    }
    this.syncOriginStates();

    // The focused tab is checked directly: when the operator is looking at
    // the library page itself, the verdict must not depend on a URL-pattern
    // query that can miss (12:43pm field report: active resolver tab, probe
    // returned "no probe evidence").
    const queriedTabs = new Map<number, KeepaliveTab>();
    try {
      for (const tab of await this.api.tabs.query({ active: true, lastFocusedWindow: true })) {
        if (tab.id !== undefined) queriedTabs.set(tab.id, tab);
      }
    } catch {
      // Fall through to the URL-pattern queries.
    }
    for (const origin of origins) {
      try {
        for (const tab of await this.api.tabs.query({ url: [`${origin}/*`] })) {
          if (tab.id !== undefined) queriedTabs.set(tab.id, tab);
        }
      } catch {
        // A revoked host permission affects only this origin's scan.
      }
    }

    for (const origin of origins) {
      const resolver = new URL(origin);
      const resolverTabs = [...queriedTabs.values()].filter(
        (tab) => typeof tab.url === "string" && resolverURLMatches(tab.url, resolver),
      );
      // A user's visible resolver tab carries the strongest, freshest
      // evidence. The manager-owned tab is eligible only for the current
      // origin; a secondary origin is always live-tab evidence.
      const liveTab = resolverTabs.find((tab) => tab.id !== this.tabID);
      const isCurrent = this.resolver?.origin === origin;
      if (isCurrent) this.likelyAuthenticated = liveTab !== undefined;
      if (liveTab?.id !== undefined) {
        await this.inspectTab(liveTab.id, resolver);
        continue;
      }
      if (isCurrent && this.tabID !== undefined) {
        await this.inspectTab(this.tabID, resolver);
        continue;
      }
      const checkedAt = Date.now();
      if (isCurrent) {
        this.setVerdict("unknown", "none", checkedAt, undefined, origin);
      } else {
        this.updateOriginSnapshot(origin, {
          verdict: "unknown",
          authenticated: false,
          probeSource: "none",
          lastVerdictAt: checkedAt,
          lastCheckAt: checkedAt,
        }, true);
      }
    }
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
        KEEPALIVE_ORIGIN_STATES_KEY,
      ]);
      storageReadSucceeded = true;
    } catch {
      // Storage is advisory. A temporary failure must not stop an active batch.
    }
    this.intervalMinutes = clampKeepaliveInterval(values["keepalive.interval"]);
    this.enabled = values["keepalive.enabled"] !== false;

    if (storageReadSucceeded) {
      this.persistedResolverOrigin = normalizeHttpsOrigin(values[KEEPALIVE_RESOLVER_ORIGIN_KEY]);
      this.restoreOriginStates(values[KEEPALIVE_ORIGIN_STATES_KEY]);
    }
    this.originStatesRestored = true;
    this.grantedResolverOrigins = [];
    this.grantedResolverOrigin = undefined;
    if (this.api.permissions !== undefined) {
      try {
        const granted = await this.api.permissions.getAll();
        this.grantedResolverOrigins = resolverOriginsFromPermissionPatterns(granted.origins);
        this.grantedResolverOrigin = this.grantedResolverOrigins[0];
      } catch {
        // Optional permissions are advisory; the popup can still explain the
        // missing resolver and an incoming handoff can seed storage later.
      }
    }
    this.syncOriginStates();
  }

  private configuredResolver(): URL | undefined {
    const origin = this.persistedResolverOrigin ?? this.grantedResolverOrigin ?? this.originCandidates()[0];
    if (origin === undefined) return undefined;
    try {
      return new URL(origin);
    } catch {
      return undefined;
    }
  }

  private hasWarmDemand(): boolean {
    return this.options.trackedJobCount() > 0 || this.options.warmDemand?.() === true;
  }

  private async selectResolver(resolver: URL): Promise<void> {
    if (this.resolver?.origin !== resolver.origin && this.tabID !== undefined) {
      await this.closeTab();
    }
    this.resolver = resolver;
    this.syncOriginStates();
  }

  private async reconcile(): Promise<void> {
    const warmDemand = this.hasWarmDemand();
    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    if (!this.enabled || !warmDemand || resolver === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    await this.selectResolver(resolver);
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
  async openReauth(originHint?: string): Promise<boolean> {
    await this.loadPreferences();
    let requested: URL | undefined;
    if (originHint !== undefined) {
      const normalized = normalizeHttpsOrigin(originHint);
      if (normalized === undefined) return false;
      try {
        requested = new URL(normalized);
      } catch {
        return false;
      }
    }
    const target = requested ?? this.resolverFromLatestOffer() ?? this.configuredResolver() ?? this.resolver;
    if (target?.protocol !== "https:") return false;
    if (this.resolver?.origin !== target.origin && this.tabID !== undefined) {
      const oldTabID = this.tabID;
      const wasPaused = this.reauthPaused;
      this.tabID = undefined;
      this.reauthPaused = false;
      this.updateOriginSnapshot(this.resolver?.origin, { pausedForReauth: false });
      if (wasPaused) this.options.onReauthStateChanged?.(false);
      try {
        await this.api.tabs.remove(oldTabID);
      } catch {
        // A manually closed tab is already in the desired state.
      }
    }
    this.resolver = target;
    this.syncOriginStates();
    if (this.tabID === undefined) await this.createTab();
    const tabID = this.tabID;
    if (tabID === undefined) return false;
    const targetState = this.originStates.get(target.origin);
    if (targetState?.authenticated === true && !this.reauthPaused) {
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
    resolverOrigin: string | undefined = this.resolver?.origin,
  ): void {
    const normalizedOrigin = normalizeHttpsOrigin(resolverOrigin);
    const prior = normalizedOrigin === undefined
      ? this.authenticated
      : this.originStates.get(normalizedOrigin)?.authenticated ?? false;
    const authenticated = verdict === "in";
    const authenticationChanged = prior !== authenticated;
    const isCurrent = normalizedOrigin === undefined || normalizedOrigin === this.resolver?.origin;
    if (isCurrent) {
      this.verdict = verdict;
      this.probeSource = source;
      this.scanOutcome = scanOutcome;
      this.lastVerdictAt = completedAt === null ? undefined : completedAt;
      this.authenticated = authenticated;
    }
    this.updateOriginSnapshot(normalizedOrigin, {
      authenticated,
      verdict,
      probeSource: source,
      ...(scanOutcome === undefined ? {} : { scanOutcome }),
      lastVerdictAt: completedAt,
      lastCheckAt: completedAt,
      checking: false,
      likelyAuthenticated: isCurrent ? this.likelyAuthenticated : false,
    }, scanOutcome === undefined);
    if (
      !prior &&
      authenticated &&
      (source === "live_tab" || source === "keepalive_tab")
    ) {
      this.options.onSessionEvidence?.(source, normalizedOrigin);
    }
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
      // Opening the probe tab is not evidence of anything: the reset that
      // lived here erased restored or previously earned session state before
      // the first inspection could run. A genuinely new origin already sits
      // at "unknown"; the next completed inspection updates the verdict on
      // its own authority.
      await this.options.onTabPlaced?.(tab.id);
    } catch {
      // Browser policy may reject background tabs. Observe and try again later.
    }
  }

  private async onObserve(): Promise<void> {
    await this.loadPreferences();
    const warmDemand = this.hasWarmDemand();
    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    if (!this.enabled || !warmDemand || resolver === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    await this.selectResolver(resolver);
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
    if (!this.enabled || !this.hasWarmDemand()) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }

    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    if (resolver === undefined) {
      await this.closeTab();
      this.schedule(this.observeMs, () => this.onObserve());
      return;
    }
    await this.selectResolver(resolver);
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

  private async inspectTab(
    tabID = this.tabID,
    resolverOverride: URL | undefined = this.resolver,
  ): Promise<void> {
    if (tabID === undefined || resolverOverride === undefined) return;
    const resolver = resolverOverride;
    const origin = resolver.origin;
    const owned = tabID === this.tabID && this.resolver?.origin === origin;
    const source: KeepaliveProbeSource = owned ? "keepalive_tab" : "live_tab";
    let tab: KeepaliveTab;
    try {
      tab = await this.api.tabs.get(tabID);
    } catch {
      const checkedAt = Date.now();
      if (this.resolver?.origin === origin) this.lastCheckAt = checkedAt;
      if (!owned) {
        this.setVerdict("unknown", source, checkedAt, undefined, origin);
        return;
      }
      this.tabID = undefined;
      const wasPaused = this.reauthPaused;
      this.reauthPaused = false;
      if (wasPaused) this.options.onReauthStateChanged?.(false);
      this.setVerdict("unknown", "none", checkedAt, undefined, origin);
      return;
    }
    const checkedAt = Date.now();
    if (this.resolver?.origin === origin) this.lastCheckAt = checkedAt;
    if (typeof tab.url !== "string") {
      this.setVerdict("unknown", source, checkedAt, undefined, origin);
      return;
    }

    if (resolverURLMatches(tab.url, resolver)) {
      const markerResult = await this.resolverMarkerVerdict(tabID);
      // A URL-shaped auth redirect carries no session verdict by itself.
      // Keep unknown until marker inspection supplies affirmative evidence.
      const verdict = markerResult.verdict;
      this.setVerdict(verdict, source, Date.now(), markerResult.scanOutcome, origin);
      if (verdict === "in" && owned && this.reauthPaused) await this.resumeAfterReauth();
      if (
        owned &&
        (verdict === "out" || (verdict === "unknown" && isAuthenticationURL(tab.url)))
      ) {
        await this.pauseForReauth();
      }
      return;
    }
    if (isAuthenticationURL(tab.url)) {
      // The IdP URL is intentionally not scanned and therefore cannot assert
      // signed-out; it only drives the visible reauthentication affordance.
      this.setVerdict("unknown", source, checkedAt, undefined, origin);
      if (owned) await this.pauseForReauth();
      return;
    }
    this.setVerdict("unknown", source, checkedAt, undefined, origin);
  }

  private async pauseForReauth(): Promise<void> {
    if (this.reauthPaused || this.tabID === undefined) return;
    this.reauthPaused = true;
    this.updateOriginSnapshot(this.resolver?.origin, { pausedForReauth: true });
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
    this.updateOriginSnapshot(this.resolver?.origin, { pausedForReauth: false });
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
    const origin = this.resolver?.origin;
    const wasAwaitingReauth = this.reauthPaused;
    this.tabID = undefined;
    this.reauthPaused = false;
    this.updateOriginSnapshot(origin, { pausedForReauth: false });
    if (wasAwaitingReauth) this.options.onReauthStateChanged?.(false);
    this.setVerdict("unknown", "none", null, undefined, origin);
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
