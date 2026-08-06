// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Institutional resolver session keepalive. This is deliberately independent of
// the bridge: callers supply only current job count and the latest OpenURL.

export interface KeepaliveTab {
  id?: number | undefined;
  url?: string | undefined;
}

export type SessionVerdict = "in" | "out" | "unknown";
export type KeepaliveProbeSource = "live_tab" | "keepalive_tab" | "none";
/** Outcome of one completed probe ATTEMPT. Distinct from the verdict: an
 * attempt that learned nothing must not overwrite what an earlier attempt
 * learned. */
export type ProbeOutcome =
  | "markers" // decisive sign-in/sign-out evidence was committed
  | "no_markers" // a resolver page was read and carried no indicators
  | "scan_failed" // injection or host access failed
  | "no_tab" // no inspectable resolver tab existed
  | "partial_scan" // more matching tabs than the observation cap
  | "conflict"; // decisive "in" and "out" both present, no causal preference
export interface ResolverMarker {
  text: string;
  label: string;
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
  /** Always exactly `verdict === "in"`. */
  authenticated: boolean;
  verdict: SessionVerdict;
  probeSource: KeepaliveProbeSource;
  /** Epoch ms when the current VERDICT was committed. The display-trust clock. */
  lastVerdictAt: number | null;
  /** Epoch ms of the last completed probe ATTEMPT, whatever it learned. */
  lastProbeAt: number | null;
  lastProbeOutcome?: ProbeOutcome;
  /** Ephemeral. Always false when persisted and when restored. */
  checking: boolean;
  likelyAuthenticated: boolean;
  pausedForReauth: boolean;
  /** Epoch ms when an external signal said this origin's evidence may be
   * obsolete, cleared when a probe commits. Durable so a worker that dies
   * before probing still knows to look on the next wake. */
  dirtySince: number | null;
}

export interface KeepaliveSnapshot {
  enabled: boolean;
  intervalMinutes: number;
  authenticated: boolean;
  /** Completed DOM/URL verdict. Older callers may omit this field. */
  verdict?: SessionVerdict;
  /** Branch that produced the current verdict. */
  probeSource?: KeepaliveProbeSource;
  /** Outcome of the most recent completed probe attempt, when one ran. */
  lastProbeOutcome?: ProbeOutcome;
  /** Epoch milliseconds when the current verdict completed. */
  lastVerdictAt?: number | null;
  /** True while an on-demand session probe is still in flight. */
  checking?: boolean;
  /** A resolver-origin tab was observed while the probe was in flight. This
   * is evidence only; `authenticated` remains the completed probe verdict. */
  likelyAuthenticated?: boolean;
  pausedForReauth: boolean;
  /** Epoch milliseconds of the last completed probe attempt, whatever it learned. */
  lastProbeAt: number | null;
  /** The configured resolver origin, never an authentication/IdP URL. */
  resolverOrigin: string | null;
  lastAuthReturnedAt: number | null;
  queuedAuthJobs: number;
  stalledAuthJobs: string[];
}

export interface FreshSessionEvidence {
  /** Always a configured resolver origin. Never an IdP or provider host. */
  origin: string;
  observedAt: number;
  /** Per-origin probe generation the evidence was committed for. */
  generation: number;
  source: "live_tab" | "keepalive_tab";
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
  /** True once a hello_ack has landed on the current daemon port. Before
   * that the configured set is UNKNOWN, not empty — see
   * KeepaliveManager.configuredOriginsReady(). */
  configuredOriginsReady?(): boolean;
  /** Number of queued institutional handoffs waiting for auth evidence. */
  queuedAuthJobs?(): number;
  /** Job ids parked after the bounded authentication-drive budget. */
  stalledAuthJobs?(): readonly string[];
  /** Latest persisted institutional-session evidence timestamp. */
  lastAuthReturnedAt?(): number | undefined;
  /** Fires ONCE per newly committed release-grade "in" probe for a
   * configured origin, whether or not the stored verdict was already "in".
   * A transition-only signal would stay silent after a worker restart
   * restores a warm verdict — exactly when queued work needs releasing.
   * This is the only callback that may authorize releasing queued work. */
  onFreshSessionEvidence?(evidence: FreshSessionEvidence): void;
  /** Committed authentication state changed for one configured origin.
   * Badge and UI state only — never a release trigger. */
  onOriginAuthenticationChanged?(origin: string, authenticated: boolean): void;
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

// ADR-0013: only an explicit sign-out affordance or a qualifying JWT identity
// counts as signed-in evidence. "my account" is a false-"in" risk (present on
// plenty of signed-out landing pages) and a later commit lets a verdict open
// tabs, so a false "in" here is no longer merely cosmetic.
const SIGN_OUT_MARKER = /sign\s*out|log\s*out|logout|signout|sign-out|log-out/i;
const SIGN_IN_MARKER = /sign\s*in|log\s*in|login/i;
/** A discovery layer renders result/article titles inside anchors, so a page
 * a session-holder never signed in to can carry prose like "Why students log
 * out of surveillance platforms" inside a plain `<a>` — text a naive regex
 * match treats identically to a real "Sign out" button. An affordance is a
 * CONTROL LABEL: short by construction. Prose is not. Only text/label whose
 * normalized (whitespace-collapsed, trimmed) form is at most this long counts
 * as evidence, in either direction. */
export const MAX_AFFORDANCE_LENGTH = 40;
const MAX_STORAGE_VALUE_LENGTH = 8 * 1024;
/** collectResolverMarkers() is injected verbatim into a page papio does not
 * control and its whole return value is structured-cloned back to the
 * service worker — mirror the discipline its own storage scan already
 * applies (50 keys / 8 KiB per value): cap how many controls a single scan
 * queries, how deep a single control's text walk recurses, and how long any
 * one marker's text/label can be. These three are duplicated as LOCAL
 * literals inside collectResolverMarkers itself (it must stay self-contained
 * for executeScript) — keep the literals there equal to these. */
export const MAX_SCANNED_CONTROLS = 400;
export const MAX_MARKER_TEXT_LENGTH = 200;
export const MAX_CONTROL_TEXT_DEPTH = 8;
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
 * an Ex Libris-style explicit group claim that is not GUEST alongside a sub.
 *
 * THIS IS THE SPECIFICATION for storage-identity claim handling. It has no
 * `src` caller of its own — the code that actually runs is
 * collectResolverMarkers()'s `hasStorageIdentity` closure below, duplicated
 * because an executeScript injection cannot import this module. The two
 * MUST agree; `hasStorageIdentity` carries a matching comment pointing back
 * here. This export exists so the claim logic has one tested definition
 * instead of only the untested, unimportable copy. */
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

/** Classify bounded resolver-page marker data without touching browser state.
 *
 * Only what the operator can READ counts as an affordance: the control's text
 * and its accessible label, never its target URL. A URL is routing, not an
 * invitation — verified against a real signed-in capture of a Primo NDE
 * resolver, where a navigation link labelled "AI Assisted Search" points at
 * /nde/login and was single-handedly classifying an authenticated page as
 * signed out. Feature links routed through a login path are common, and they
 * survive signing in, so URL matching produced a false "out" that no amount of
 * probing at the right moment could correct. */
export function classifyResolverMarkers(markers: readonly ResolverMarker[]): SessionVerdict {
  let signIn = false;
  let storageIdentity = false;
  for (const marker of markers) {
    if (typeof marker?.text !== "string" || typeof marker?.label !== "string") continue;
    if (marker.storageIdentity === "in") storageIdentity = true;
    // Only a normalized field short enough to BE a control label — not a
    // headline that happens to contain one — contributes to the match.
    const text = marker.text.replace(/\s+/g, " ").trim();
    const label = marker.label.replace(/\s+/g, " ").trim();
    const affordance = [
      text.length <= MAX_AFFORDANCE_LENGTH ? text : "",
      label.length <= MAX_AFFORDANCE_LENGTH ? label : "",
    ].join(" ");
    if (SIGN_OUT_MARKER.test(affordance)) return "in";
    if (SIGN_IN_MARKER.test(affordance) && marker.visible !== false) signIn = true;
  }
  if (storageIdentity) return "in";
  return signIn ? "out" : "unknown";
}

/** Serializable page function used by chrome.scripting.executeScript. */
export function collectResolverMarkers(): ResolverMarker[] {
  const maxStorageValueLength = 8 * 1024;
  // Duplicated literals for MAX_SCANNED_CONTROLS / MAX_MARKER_TEXT_LENGTH /
  // MAX_CONTROL_TEXT_DEPTH (see the exported constants above): executeScript
  // serializes only this function, not its module scope, so a shared
  // reference throws ReferenceError once injected into the page. Keep these
  // three equal to the exported values.
  const maxScannedControls = 400;
  const maxMarkerTextLength = 200;
  const maxControlTextDepth = 8;
  const elements = Array.from(
    // Read only user-facing controls and their targets. Scanning every node
    // would include script/style/template source and ancestor textContent that
    // aggregates unrelated descendant labels.
    document.querySelectorAll<HTMLElement>(
      "a,button,input,select,textarea,form,summary,label,[role='button'],[role='link']",
    ),
    // A page can render arbitrarily many candidate controls. The array built
    // below is structured-cloned whole back to the service worker, so an
    // unbounded element list is an unbounded payload from a page papio does
    // not control — cap it the same way the storage scan below is capped.
  ).slice(0, maxScannedControls);
  const ignoredTextTags: Record<string, true> = { SCRIPT: true, STYLE: true, TEMPLATE: true };
  // Depth-bounded: an unbounded walk is worst-case quadratic once a matched
  // control nests another (a <label> wrapping a <button> wrapping a <span>
  // re-serializes the same descendants at every level, once per ancestor),
  // and a pathologically deep DOM should not make the injected scan itself
  // expensive.
  const controlText = (node: Node, depth = 0): string => {
    if (node.nodeType === 3) return node.nodeValue ?? "";
    if (node.nodeType !== 1) return "";
    const child = node as Element;
    if (ignoredTextTags[child.tagName] === true) return "";
    if (depth >= maxControlTextDepth) return "";
    return Array.from(child.childNodes)
      .map((descendant) => controlText(descendant, depth + 1))
      .join(" ");
  };
  // A control's `value` is its rendered label only on push buttons. On a text
  // or search input it is whatever the operator last typed, and a discovery
  // layer echoes the query back into the box — so treating it as an affordance
  // lets a search for "sign out" forge proof of a session on a page the
  // operator is not signed in to.
  const buttonValueTypes: Record<string, true> = { submit: true, button: true, reset: true };
  const markers: ResolverMarker[] = elements.map((element) => {
    const isButtonValue =
      element.tagName === "INPUT" &&
      buttonValueTypes[element.getAttribute("type")?.trim().toLowerCase() ?? ""] === true;
    const value = isButtonValue ? (element.getAttribute("value")?.trim() ?? "") : "";
    // A form's action is a target, not a page-sized text aggregate. Its
    // controls are scanned independently below.
    const text = element.tagName === "FORM" ? "" : controlText(element).trim();
    const rect = element.getClientRects();
    // Deliberately no href/formaction/action. The classifier judges
    // affordances, not routing, so collecting targets would only give it a way
    // to be wrong — and it would pull URLs out of the operator's page for no
    // remaining purpose.
    return {
      // Truncated, not just capped in count: a single control's own text
      // (a hidden <label> wrapping a whole article body, say) can still be
      // page-sized even after the depth bound above.
      text: `${text} ${value}`.trim().slice(0, maxMarkerTextLength),
      label: (element.getAttribute("aria-label")?.trim() ?? "").slice(0, maxMarkerTextLength),
      visible: rect.length > 0 && element.checkVisibility?.() !== false,
    };
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
  // injected function, not its module-level dependencies, so it cannot import
  // classifyResolverJWTIdentity() and must duplicate its claim handling
  // instead. THE TWO MUST AGREE: classifyResolverJWTIdentity() above is the
  // specification (it is what the tests exercise, since this closure cannot
  // be imported into a test file either) — named claim list, userGroup/
  // user_group, the GUEST rejection, sub+group, and the exp check must stay
  // byte-for-byte identical to it. Any change to one belongs in both.
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
        // Matches decodeJWTPayload()'s shape guard above: a JWT payload is a
        // claims object, never an array or a bare primitive.
        if (typeof payload !== "object" || payload === null || Array.isArray(payload)) continue;
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
/** Display-trust budget ONLY: how long a completed verdict is shown as fresh
 * in the popup. Must never gate whether a probe runs — probeForeground()
 * always probes regardless of this value. */
export const SESSION_STALE_MS = 2 * 60_000;
/** Caps how many candidate tabs a single probeOrigin() call inspects. Beyond
 * this the scan is reported as "partial_scan" rather than silently guessing. */
export const MAX_OBSERVED_TABS_PER_ORIGIN = 5;
const ON_DEMAND_PROBE_BUDGET_MS = 1_400;
/** Minimum spacing between probe STARTS for one origin. Not a freshness gate:
 * it never asks whether the previous verdict is good enough, it only limits
 * how often papio may inject into the operator's library tab. The newest
 * pending generation always runs. */
export const MIN_PROBE_START_SPACING_MS = 10_000;
/** Operator-initiated probes (popup open, the Sign in button) get a shorter
 * floor than automatic triggers. The throttle exists to bound how often papio
 * injects into the operator's library page, and an explicit request is already
 * rate-limited by a human hand; holding one behind the full automatic floor
 * would put a ten-second staleness window on exactly the interaction this work
 * exists to fix — the operator signs in, reopens the popup, and expects papio
 * to have noticed. This floor bounds probe STARTS, not injections, and each
 * start injects into up to MAX_OBSERVED_TABS_PER_ORIGIN (5) tabs: a
 * pathologically cycled popup can reach 30 starts/minute and up to 150
 * injections/minute here, against 6 starts/minute and up to 30
 * injections/minute on the automatic MIN_PROBE_START_SPACING_MS floor — five
 * times the ceiling, not the same one. That gap is accepted, not hidden: an
 * operator with the popup open is still bounded by how fast a human can
 * reopen it, and missing a just-completed sign-in for up to ten seconds is
 * the worse failure. */
export const MIN_FOREGROUND_PROBE_SPACING_MS = 2_000;
/** Bounds one admitted probeOrigin() attempt so a wedged tabs./scripting
 * call cannot hold an origin's in-flight slot open forever — that would
 * starve every later trigger's trailing probe indefinitely. Independent of
 * probeForeground()'s budgetMs, which only bounds the CALLER's wait. */
const PROBE_ADMISSION_DEADLINE_MS = 20_000;

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

/** Result of one pure inspection of one tab. Never written anywhere:
 * probeOrigin() reduces a batch of these into exactly one committed
 * verdict. `kind` "off_origin"/"auth_url" cover the URL-shape checks that
 * precede marker scanning; `verdict` is present only for kind "verdict".
 * "stale" means the tab's document changed under the scan (its per-tab
 * epoch moved, or its URL no longer matches once re-checked afterward) —
 * reduceObservations must treat it as absent evidence, never as
 * "no_markers": a document we can no longer vouch for said nothing. */
interface TabObservation {
  tabID: number;
  /** True when this is the manager's own resolver-origin tab. */
  owned: boolean;
  kind: "verdict" | "no_markers" | "scan_failed" | "no_tab" | "off_origin" | "auth_url" | "stale";
  verdict?: "in" | "out";
}

/** Every place a probe can originate. Commit C gates release authority on
 * this; here it only picks the log-worthy label and travels unchanged
 * through requestProbe() -> probeOrigin(). */
type ProbeReason = "foreground" | "cycle" | "reauth" | "navigation" | "activation" | "wake";

/** Keys purely off the ProbeReason VALUE, never off who actually called
 * requestProbe() — the type carries no "came from an operator action" fact
 * for this to check. Today only probeForeground() constructs "foreground",
 * and every automatic trigger — including background.ts's own
 * institutional-landing detection, which uses probeOriginAutomatically()'s
 * "navigation" reason instead of calling probeForeground() directly — uses
 * one of the other five reasons, so "foreground" does mean operator-
 * initiated in practice. That correspondence is a calling convention every
 * caller of probeForeground() must uphold, not something this function or
 * the ProbeReason type enforces: a future caller of probeForeground() from
 * an automatic path would silently inherit the shorter, operator-only
 * floor. */
function spacingFloorFor(reason: ProbeReason): number {
  return reason === "foreground" ? MIN_FOREGROUND_PROBE_SPACING_MS : MIN_PROBE_START_SPACING_MS;
}

/** One coalesced, not-yet-started probe request for an origin: the newest
 * reason/preferredTabID always overwrites the previous ones, and every
 * caller that piled on while waiting resolves off the SAME promise once the
 * eventual trailing probe (whichever one actually runs) settles. */
interface PendingProbeState {
  reason: ProbeReason;
  preferredTabID: number | undefined;
  promise: Promise<void>;
  resolve: () => void;
}


/**
 * Maintains at most one resolver-origin tab while active handoffs exist.
 *
 * Four independent timer kinds replace the single one-shot timer this class
 * used to multiplex: `cycleTimer` (the periodic owned-tab reload/observe
 * loop, driven by an absolute due time so repeated external sync() calls can
 * never postpone it), `reauthTimer` (the paused-tab recheck loop),
 * `settleTimers` (one per origin, letting a just-navigated resolver SPA
 * render before it is probed), and `spacingTimers` (one per origin, the
 * trailing-probe delay a throttled requestProbe() defers behind). Firing or
 * clearing any one of the four must never touch another — each owns its own
 * field/Map and only the methods named after it may write to it. Every
 * probe — foreground, navigation, activation, cycle, reauth, or a recovery
 * wake — funnels through requestProbe(), which admits at most one in-flight
 * probe per origin and throttles probe STARTS to MIN_PROBE_START_SPACING_MS
 * apart for automatic triggers, or MIN_FOREGROUND_PROBE_SPACING_MS for an
 * operator-initiated one.
 */
export class KeepaliveManager {
  private cycleTimer: unknown | undefined;
  /** Absolute epoch ms the pending cycleTimer is armed for. Undefined
   * exactly when cycleTimer is undefined. */
  private nextCycleDueAt: number | undefined;
  /** Epoch ms the owned-tab cycle last actually ran — a cycleTimer firing,
   * or a fresh tab creation/adoption — never "now" at the moment some
   * unrelated caller happens to invoke reconcile()/onObserve()/onReload().
   * Those three compute the NEXT due time as `lastCycleRunAt + intervalMs()`
   * so a heartbeat re-entering reconcile() between two real cycle steps
   * recomputes the SAME due time, and scheduleCycle()'s "no later than"
   * check leaves the pending timer alone instead of pushing the reload out
   * another full interval. This is the fix for the papio-keepalive alarm
   * (running every ~60s) starving the 4-minute reload forever. */
  private lastCycleRunAt = 0;
  /** Single slot for "recheck the paused owned tab again soon". Armed by
   * pauseForReauth() itself so an explicit openReauth() pause takes effect
   * within observeMs instead of inheriting whatever cycleTimer happened to
   * already be pending — up to a full interval away. */
  private reauthTimer: unknown | undefined;
  /** Post-navigation "let the SPA render its header and mint a session
   * token" delay, one per origin so two origins' navigations can never
   * cancel each other. */
  private readonly settleTimers = new Map<string, unknown>();
  /** One per origin: the trailing-probe delay a throttled requestProbe()
   * defers behind. `dueAt` is the absolute epoch ms the timer is armed for,
   * tracked alongside the handle so a later deferral for the SAME origin
   * but a shorter floor (an operator request arriving after an automatic
   * one was already deferred) can pull the timer earlier — never later,
   * never cancel it outright. Kept in its own Map rather than reusing
   * settleTimers: by the time a spacing defer needs a timer, whatever
   * settle timer led here has already fired (a settle timer's whole job is
   * to call requestProbe(), which is what triggers a spacing defer), so
   * the two never actually contend for the same slot — but a dedicated Map
   * means a future caller can never accidentally clobber one while
   * intending the other. */
  private readonly spacingTimers = new Map<string, { timer: unknown; dueAt: number }>();
  private tabID: number | undefined;
  private resolver: URL | undefined;
  private persistedResolverOrigin: string | undefined;
  private grantedResolverOrigin: string | undefined;
  private grantedResolverOrigins: string[] = [];
  private readonly originStates = new Map<string, KeepaliveOriginSnapshot>();
  /** Per-tab "which document is this" counter. Bumped on every navigation
   * noteResolverNavigation() is told about, dropped on tab removal. An
   * observeTab() scan spans an awaited executeScript(); without this, an
   * injected result can resolve against a document the tab has already
   * navigated away from, and nothing about the shape of the result reveals
   * that — a stale sign-out page and a stale sign-in page look identical to
   * a scan that only ever sees the markers it was handed back. */
  private readonly tabDocumentEpochs = new Map<number, number>();
  /** Last known-resolver-origin a tab was observed on. Lets a navigation
   * AWAY from a resolver (to an IdP, most commonly) mark that origin dirty
   * without ever recording the IdP's own origin anywhere. */
  private readonly tabResolverOrigins = new Map<number, string>();
  private intervalMinutes = DEFAULT_INTERVAL_MINUTES;
  private enabled = true;
  private reauthPaused = false;
  private authenticated = false;
  private verdict: SessionVerdict = "unknown";
  private probeSource: KeepaliveProbeSource = "none";
  private lastProbeOutcome: ProbeOutcome | undefined;
  private lastVerdictAt: number | undefined;
  private lastProbeAt: number | undefined;
  private checking = false;
  private likelyAuthenticated = false;
  /** Per-origin admission control for requestProbe(): every probe request —
   * foreground, navigation, activation, cycle, reauth, wake — funnels
   * through requestProbe(), which uses these three to decide whether to
   * start immediately, queue behind an in-flight attempt, or defer behind
   * the MIN_PROBE_START_SPACING_MS throttle. */
  private readonly probeInFlight = new Map<string, Promise<void>>();
  private readonly pendingProbes = new Map<string, PendingProbeState>();
  private readonly lastProbeStartedAt = new Map<string, number>();
  /** Per-origin counter, bumped once per admitted probe start
   * (startProbe()) and threaded through probeOrigin() into
   * commitOriginProbe(). Identifies which probe attempt produced a given
   * committed FreshSessionEvidence — never used for admission itself. */
  private readonly probeGenerations = new Map<string, number>();
  /** Every origin-state write is chained after the previous one and reads
   * the CURRENT map only once it is actually its turn to run, so a burst of
   * synchronous patches (a probe's likelyAuthenticated flip, then its
   * verdict commit) coalesces into whichever write runs last — never an
   * interleaved, reordered chrome.storage.set that could resurrect an older
   * snapshot over a newer one. Mirrors BrowserBridge.saveChain. */
  private persistChain: Promise<void> = Promise.resolve();
  /** Shared in-flight promise for createTabOnce(). sync() now runs from
   * every triage-counts response as well as the onObserve/onReload timers,
   * so reconcile/onObserve/onReload can each independently see
   * this.tabID === undefined and race into createTab() across its awaited
   * tabs.query()/tabs.create() calls. Without this, two interleaved callers
   * both query, both find nothing, and both create — the second assignment
   * to this.tabID orphans the first tab, and the tab governor deliberately
   * skips pinned tabs, so the orphan is never reconciled or closed.
   *
   * Joining this promise is only safe for a caller that wants the SAME
   * origin the in-flight attempt was started for (tabCreationOrigin,
   * below): openReauth exists specifically to switch origins, and this.
   * resolver can already have moved on to a different institution by the
   * time a second caller reaches createTab(). Riding the promise then would
   * hand that caller (via this.tabID) a tab for the wrong institution. createTab()
   * compares the wanted origin against tabCreationOrigin and, on a
   * mismatch, waits for the stale attempt to settle (so it can never be
   * starved), tears down any tab that settled attempt produced (via
   * removeStaleTab — the settled attempt may have created one for the
   * origin nobody wants anymore before it could be stopped, and that tab
   * would otherwise be pinned+muted with nothing referencing it once this
   * loop overwrites this.tabID; the tab governor skips pinned tabs on
   * purpose, so it would never be reconciled or closed), and then drives
   * its own, origin-correct creation instead of joining. */
  private tabCreationInFlight: Promise<void> | undefined;
  /** Origin the in-flight tabCreationInFlight attempt was started for.
   * Undefined whenever no creation is in flight. */
  private tabCreationOrigin: string | undefined;

  /** True once a hello_ack has landed on the current port. Before that the
   * configured set is UNKNOWN, not empty (verified defect #6): treating an
   * empty/advisory candidate set as "no institutions" would wipe restored
   * display state, and nothing may be release-grade while this is false. */
  private configuredOriginsReady(): boolean {
    try {
      return this.options.configuredOriginsReady?.() === true;
    } catch {
      return false;
    }
  }

  /** `origin` is exactly one of the daemon's configured resolver origins.
   * Always false before hello_ack — see configuredOriginsReady() above.
   * The one thing every release-grade signal (onFreshSessionEvidence,
   * onOriginAuthenticationChanged) and openReauth(origin) trust: an offer,
   * a persisted origin, or a granted permission pattern may SELECT among
   * these, but none of them may WIDEN this set (verified defect #5). */
  private isConfiguredMember(origin: string): boolean {
    return this.configuredOriginsReady() && this.originCandidates().includes(origin);
  }

  private originCandidates(): string[] {
    const known: unknown[] = [];
    try {
      known.push(...(this.options.knownResolverOrigins?.() ?? []));
    } catch {
      // The bridge's negotiated-origin cache is advisory.
    }
    if (this.configuredOriginsReady()) {
      // Once hello_ack has landed the row universe is EXACTLY the daemon's
      // configured institutions. persistedResolverOrigin/this.resolver may
      // still SELECT which configured origin has current demand
      // (configuredResolver(), openReauthOnce()) but never WIDEN
      // membership — an offer or stale persisted origin for an
      // unconfigured institution must never create a phantom row.
      return [...new Set(
        known
          .map((candidate) => normalizeHttpsOrigin(candidate))
          .filter((origin): origin is string => origin !== undefined),
      )];
    }
    // Pre-hello (verified defect #6): the configured set is UNKNOWN, not
    // empty. Fold in the persisted/current resolver as display-only
    // fallbacks so the popup still has a row before the daemon confirms
    // anything. Permission grants stay excluded even here: "Grant all
    // sources" hands papio dozens of provider-host patterns, and every one
    // of them would otherwise render as a phantom institution row.
    const candidates = [
      ...known,
      this.persistedResolverOrigin,
      this.resolver?.protocol === "https:" ? this.resolver.origin : undefined,
    ];
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
      lastProbeAt: null,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      dirtySince: null,
    };
  }

  private syncOriginStates(): void {
    const origins = this.originCandidates();
    for (const origin of origins) {
      if (!this.originStates.has(origin)) this.originStates.set(origin, this.defaultOriginSnapshot(origin));
    }
    // Only prune once membership is authoritative. Pre-hello, `origins` is
    // a display-only union that clearNegotiationState shrinks to nothing on
    // every connect/reconnect attempt — deleting absent states here would
    // wipe restored rows several times a session before hello_ack ever lands.
    if (!this.configuredOriginsReady()) return;
    const known = new Set(origins);
    for (const origin of this.originStates.keys()) {
      if (!known.has(origin)) this.originStates.delete(origin);
    }
  }

  /** Return one independently tracked verdict for every configured resolver. */
  getOriginSnapshots(): KeepaliveOriginSnapshot[] {
    this.syncOriginStates();
    return [...this.originStates.values()].map((snapshot) => ({ ...snapshot }));
  }

  /** Called by the bridge the instant a hello_ack lands on the current
   * port, so origin membership updates apply immediately instead of
   * waiting for the next minute's alarm/cycle to call syncOriginStates()
   * incidentally. Re-syncs the row universe against the now-authoritative
   * knownResolverOrigins() and, mirroring onWake()'s dirty-driven recovery
   * probing, requests a probe for every origin still dirty. */
  notifyConfiguredOriginsChanged(): void {
    this.syncOriginStates();
    for (const [origin, snapshot] of this.originStates) {
      if (snapshot.dirtySince !== null) void this.requestProbe(origin, "wake");
    }
  }
  private updateOriginSnapshot(
    origin: string | undefined,
    patch: Partial<KeepaliveOriginSnapshot>,
    clearProbeOutcome = false,
  ): Promise<void> {
    if (origin === undefined) return Promise.resolve();
    const normalized = normalizeHttpsOrigin(origin);
    if (normalized === undefined) return Promise.resolve();
    const current = this.originStates.get(normalized) ?? this.defaultOriginSnapshot(normalized);
    const next = { ...current, ...patch, origin: normalized };
    if (clearProbeOutcome) delete next.lastProbeOutcome;
    this.originStates.set(normalized, next);
    return this.persistOriginStates();
  }

  /** Guards the persist path during startup: an early snapshot update must
   * not overwrite stored evidence before loadPreferences has restored it. */
  private originStatesRestored = false;

  private persistOriginStates(): Promise<void> {
    if (!this.originStatesRestored) return Promise.resolve();
    const write = this.persistChain.then(() => {
      const values = {
        [KEEPALIVE_ORIGIN_STATES_KEY]: [...this.originStates.values()].map((snapshot) => ({
          ...snapshot,
          checking: false,
        })),
      };
      return this.api.storage.set?.(values) ?? Promise.resolve();
    });
    this.persistChain = write.catch(() => {});
    return write;
  }

  private restoreOriginStates(raw: unknown): void {
    if (!Array.isArray(raw)) return;
    for (const entry of raw) {
      if (typeof entry !== "object" || entry === null) continue;
      const snapshot = entry as KeepaliveOriginSnapshot;
      const origin = normalizeHttpsOrigin(snapshot.origin);
      if (origin === undefined) continue;
      // A pre-seeded default (no completed probe) is not evidence — restored
      // state wins over it, but never over a live probe's result.
      const existing = this.originStates.get(origin);
      if (existing !== undefined && existing.lastProbeAt !== null) continue;
      // Restored evidence keeps its original timestamps: freshness gates in
      // the popup decide how much to trust it, never a worker restart.
      const dirtySince = typeof snapshot.dirtySince === "number" ? snapshot.dirtySince : null;
      this.originStates.set(origin, { ...snapshot, origin, checking: false, dirtySince });
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
      ...(this.lastProbeOutcome === undefined ? {} : { lastProbeOutcome: this.lastProbeOutcome }),
      lastVerdictAt: this.lastVerdictAt ?? null,
      checking: this.checking,
      likelyAuthenticated: this.likelyAuthenticated,
      pausedForReauth: this.reauthPaused,
      lastProbeAt: this.lastProbeAt ?? null,
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
   * keepalive observation has not run in this worker lifetime. Persists via
   * rememberResolverOrigin() below, which is a pre-hello DISPLAY fallback
   * only — it never contributes to configured-origin membership once
   * hello_ack has landed; see originCandidates(). */
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
  /** Probe every known resolver origin, or one specific origin, right now.
   * No freshness gate: this always (re)probes, unlike the prior on-demand
   * check — SESSION_STALE_MS is a display-trust budget for the popup,
   * never a probe gate. Every request — including this one — funnels
   * through requestProbe()'s per-origin admission control, so a foreground
   * call moments after another probe for the same origin may be deferred
   * behind MIN_PROBE_START_SPACING_MS rather than starting immediately; the
   * browser-API work always runs to completion, only the CALLER's wait is
   * bounded by budgetMs so a foreground popup request never blocks past the
   * MV3 budget. */
  async probeForeground(origin?: string, budgetMs = ON_DEMAND_PROBE_BUDGET_MS): Promise<void> {
    await this.loadPreferences();
    const configured =
      this.resolverFromLatestOffer() ?? this.configuredResolver() ?? this.resolver;
    if (configured !== undefined && configured.protocol === "https:") {
      await this.selectResolver(configured);
    }
    const targets =
      origin !== undefined
        ? [normalizeHttpsOrigin(origin)].filter(
            (candidate): candidate is string => candidate !== undefined,
          )
        : this.originCandidates();

    const work = Promise.all(targets.map((target) => this.foregroundProbe(target))).then(() => {});
    const boundedBudget = Math.min(1_500, Math.max(0, Math.trunc(budgetMs)));
    const { promise: deadline, resolve: resolveDeadline } = Promise.withResolvers<void>();
    const timeout = setTimeout(resolveDeadline, boundedBudget);
    try {
      await Promise.race([work, deadline]);
    } finally {
      clearTimeout(timeout);
    }
  }

  /** Automatic-path probe for callers outside the tab-tracking pipeline
   * (background.ts's own institutional-landing detection, on tab
   * navigation) that need an immediate re-probe of one origin without
   * claiming probeForeground()'s "foreground" reason — that reason carries
   * the shorter, operator-only MIN_FOREGROUND_PROBE_SPACING_MS floor (see
   * spacingFloorFor()), and this call site is not an operator action.
   * Routes through requestProbe() with "navigation", an already-automatic
   * reason, so it gets the full MIN_PROBE_START_SPACING_MS floor like every
   * other automatic trigger. */
  async probeOriginAutomatically(origin: string): Promise<void> {
    await this.loadPreferences();
    const target = normalizeHttpsOrigin(origin);
    if (target === undefined) return;
    await this.requestProbe(target, "navigation");
  }

  /** `checking` is owned entirely here: requestProbe()/probeOrigin() never
   * touch it, so a "cycle"/"reauth"/"navigation"/etc. request never flips it
   * and never collides with a concurrent foreground probe's flag. */
  private async foregroundProbe(origin: string): Promise<void> {
    if (origin === this.resolver?.origin) this.checking = true;
    void this.updateOriginSnapshot(origin, { checking: true });
    try {
      await this.requestProbe(origin, "foreground");
    } finally {
      if (origin === this.resolver?.origin) {
        this.checking = false;
        this.likelyAuthenticated = false;
      }
      void this.updateOriginSnapshot(origin, { checking: false, likelyAuthenticated: false });
    }
  }

  /** Single funnel for every probe trigger — foreground, navigation,
   * activation, cycle, reauth, wake. An in-flight probe for the origin
   * queues this request (newest reason/preferredTabID wins) and resolves
   * once the eventual trailing probe settles; otherwise, if the last probe
   * for this origin STARTED less than its reason's spacing floor ago (the
   * shorter MIN_FOREGROUND_PROBE_SPACING_MS for an operator-initiated
   * "foreground" request, MIN_PROBE_START_SPACING_MS for every automatic
   * one) the origin is marked dirty and the request is deferred to the
   * earliest permitted start; otherwise it starts immediately. A deferred
   * request never touches verdict/lastVerdictAt/lastProbeAt/
   * lastProbeOutcome — deferral is not an observation.
   *
   * Caller settlement differs by how the request was admitted: joining an
   * in-flight probe, or starting one outright, resolves when that actual
   * probe attempt settles. A THROTTLE-deferred "foreground" request is
   * different — the caller only wants to know its request landed, not to
   * sit through however long another trigger's spacing floor takes; it
   * resolves the instant the defer is recorded, and the eventual trailing
   * probe becomes purely the manager's own business from there. A
   * throttle-deferred automatic request keeps the old behavior: it
   * resolves only once the trailing probe it piggybacks on has run. */
  private requestProbe(origin: string, reason: ProbeReason, preferredTabID?: number): Promise<void> {
    if (this.probeInFlight.has(origin)) {
      return this.deferProbe(origin, reason, preferredTabID);
    }
    const now = Date.now();
    const lastStart = this.lastProbeStartedAt.get(origin);
    const floor = spacingFloorFor(reason);
    if (lastStart !== undefined && now - lastStart < floor) {
      void this.markDirty(origin);
      const trailing = this.deferProbe(origin, reason, preferredTabID);
      this.armSpacingTimer(origin, lastStart + floor);
      return reason === "foreground" ? Promise.resolve() : trailing;
    }
    return this.startProbe(origin, reason, preferredTabID);
  }

  /** Arms, or re-arms, the origin's spacing timer for `dueAt` — never to a
   * LATER time than whatever is already pending. A foreground defer's 2s
   * floor can pull an already-armed automatic 10s deferral earlier; an
   * automatic defer arriving after a foreground one was already armed must
   * never push it back out, or an operator-initiated request would end up
   * waiting on a timer sized for a trigger that merely arrived first. */
  private armSpacingTimer(origin: string, dueAt: number): void {
    const existing = this.spacingTimers.get(origin);
    if (existing !== undefined && existing.dueAt <= dueAt) return;
    if (existing !== undefined) this.api.timers.clearTimeout(existing.timer);
    const timer = this.api.timers.setTimeout(() => {
      this.spacingTimers.delete(origin);
      void this.runPendingProbe(origin);
    }, Math.max(0, dueAt - Date.now()));
    this.spacingTimers.set(origin, { timer, dueAt });
  }

  private deferProbe(origin: string, reason: ProbeReason, preferredTabID: number | undefined): Promise<void> {
    const existing = this.pendingProbes.get(origin);
    if (existing !== undefined) {
      existing.reason = reason;
      existing.preferredTabID = preferredTabID;
      return existing.promise;
    }
    const { promise, resolve } = Promise.withResolvers<void>();
    this.pendingProbes.set(origin, { reason, preferredTabID, promise, resolve });
    return promise;
  }

  private async runPendingProbe(origin: string): Promise<void> {
    const pending = this.pendingProbes.get(origin);
    if (pending === undefined) return;
    this.pendingProbes.delete(origin);
    // Re-enter requestProbe() rather than starting directly: the in-flight
    // probe that unblocked this trailing run may have finished well inside
    // the spacing window measured from its OWN start, so this still needs
    // its own admission check to keep every start at least its reason's
    // spacing floor apart.
    await this.requestProbe(origin, pending.reason, pending.preferredTabID);
    pending.resolve();
  }

  private async startProbe(
    origin: string,
    reason: ProbeReason,
    preferredTabID: number | undefined,
  ): Promise<void> {
    this.lastProbeStartedAt.set(origin, Date.now());
    const generation = (this.probeGenerations.get(origin) ?? 0) + 1;
    this.probeGenerations.set(origin, generation);
    const attempt = this.boundedProbe(origin, reason, preferredTabID, generation).finally(() => {
      this.probeInFlight.delete(origin);
      if (this.pendingProbes.has(origin)) void this.runPendingProbe(origin);
    });
    this.probeInFlight.set(origin, attempt);
    await attempt;
  }

  /** Bounds one admitted probeOrigin() attempt: the in-flight slot always
   * frees within PROBE_ADMISSION_DEADLINE_MS even if the underlying browser
   * API call is wedged, so one hung origin can never starve every later
   * trigger's trailing probe. The hung attempt itself is left running (or
   * failing) on its own; this only stops it from blocking admission. */
  private boundedProbe(
    origin: string,
    reason: ProbeReason,
    preferredTabID: number | undefined,
    generation: number,
  ): Promise<void> {
    const { promise, resolve } = Promise.withResolvers<void>();
    let settled = false;
    const finish = (): void => {
      if (settled) return;
      settled = true;
      resolve();
    };
    const timer = this.api.timers.setTimeout(finish, PROBE_ADMISSION_DEADLINE_MS);
    void this.probeOrigin(origin, reason, preferredTabID, generation)
      .catch(() => {})
      .finally(() => {
        this.api.timers.clearTimeout(timer);
        finish();
      });
    return promise;
  }

  /** Marks an origin's evidence possibly-obsolete, moving dirtySince from
   * null to now. A later signal while already dirty leaves the original
   * "obsolete since" timestamp alone. The in-memory move happens
   * synchronously (via updateOriginSnapshot); the returned promise is only
   * the durable persist, for callers (note*'s asynchronous tail, and the
   * bridge's recordInstitutionalSession) that need it to have landed before
   * a worker might sleep again. Public: a landing is not itself evidence
   * (ADR-0013 — a timing frame is not an identity assertion), so the bridge
   * marks dirty and lets a real probe decide, instead of asserting "in". */
  markDirty(origin: string): Promise<void> {
    const current = this.originStates.get(origin);
    if (current !== undefined && current.dirtySince !== null) return Promise.resolve();
    return this.updateOriginSnapshot(origin, { dirtySince: Date.now() });
  }

  /** One atomic probe-and-commit cycle for a single origin: observes a
   * bounded, deduplicated, priority-ordered set of candidate tabs (pure —
   * nothing is written until every observation has returned), reduces them
   * to exactly one outcome, and commits exactly once. `preferredTabID`
   * (passed by requestProbe()'s callers for the manager's own tab, or a
   * navigation/activation's causal tab) is the causal tab when present;
   * otherwise the focused tab is. `reason` is unused by this commit — it
   * exists so a later commit can gate release authority (which callers may
   * pause/resume the owned tab) without another signature change.
   * `generation` is startProbe()'s per-origin counter for this admitted
   * attempt, passed through unchanged so commitOriginProbe() can stamp it
   * on any FreshSessionEvidence this attempt commits. */
  private async probeOrigin(
    origin: string,
    reason: ProbeReason,
    preferredTabID: number | undefined,
    generation: number,
  ): Promise<void> {
    let resolver: URL;
    try {
      resolver = new URL(origin);
    } catch {
      return;
    }

    // The focused tab is checked directly: when the operator is looking at
    // the library page itself, the verdict must not depend on a URL-pattern
    // query that can miss (12:43pm field report: active resolver tab, probe
    // returned "no probe evidence").
    let focusedTabID: number | undefined;
    try {
      for (const tab of await this.api.tabs.query({ active: true, lastFocusedWindow: true })) {
        if (tab.id !== undefined && typeof tab.url === "string" && resolverURLMatches(tab.url, resolver)) {
          focusedTabID = tab.id;
          break;
        }
      }
    } catch {
      // Fall through to the URL-pattern query below.
    }

    const matchedIDs: number[] = [];
    try {
      for (const tab of await this.api.tabs.query({ url: [`${origin}/*`] })) {
        if (tab.id !== undefined && typeof tab.url === "string" && resolverURLMatches(tab.url, resolver)) {
          matchedIDs.push(tab.id);
        }
      }
    } catch {
      // A revoked host permission affects only this origin's scan.
    }

    const ownedTabID =
      this.tabID !== undefined && this.resolver?.origin === origin ? this.tabID : undefined;

    // Priority order, deduplicated by id — never raw query order: a
    // preferred/focused/owned tab's evidence must not be diluted by
    // whichever tab happened to sort first out of tabs.query().
    const seen = new Set<number>();
    const ordered: number[] = [];
    const push = (id: number | undefined): void => {
      if (id !== undefined && !seen.has(id)) {
        seen.add(id);
        ordered.push(id);
      }
    };
    push(preferredTabID);
    push(focusedTabID);
    push(ownedTabID);
    for (const id of matchedIDs) push(id);

    // "A resolver-origin tab exists" is itself evidence-in-progress, shown
    // to the popup while the individual scans below are still running.
    const likelyAuthenticated = ordered.length > 0;
    if (origin === this.resolver?.origin) this.likelyAuthenticated = likelyAuthenticated;
    void this.updateOriginSnapshot(origin, { likelyAuthenticated });

    const truncated = ordered.length > MAX_OBSERVED_TABS_PER_ORIGIN;
    const toObserve = ordered.slice(0, MAX_OBSERVED_TABS_PER_ORIGIN);
    const observations = await Promise.all(toObserve.map((tabID) => this.observeTab(tabID, resolver)));

    const causalTabID = preferredTabID ?? focusedTabID;
    const reduction = this.reduceObservations(observations, causalTabID, truncated);
    const ownedObservation = observations.find((observation) => observation.owned);
    await this.commitOriginProbe(origin, reduction, ownedObservation, generation);
  }

  /** Pure. Never writes a field, persists anything, calls an options.*
   * callback, or touches reauth state — probeOrigin() collects a whole
   * batch of these before anything is committed. */
  private async observeTab(tabID: number, resolver: URL): Promise<TabObservation> {
    const owned = tabID === this.tabID && this.resolver?.origin === resolver.origin;
    let tab: KeepaliveTab;
    try {
      tab = await this.api.tabs.get(tabID);
    } catch {
      return { tabID, owned, kind: "no_tab" };
    }
    if (typeof tab.url !== "string" || !resolverURLMatches(tab.url, resolver)) {
      if (typeof tab.url === "string" && isAuthenticationURL(tab.url)) {
        return { tabID, owned, kind: "auth_url" };
      }
      return { tabID, owned, kind: "off_origin" };
    }
    // Snapshot the document epoch immediately before the injected scan: a
    // per-tab generation counter cannot prove the result that comes back
    // still describes the document sitting in this tab right now.
    const epochAtScan = this.tabDocumentEpochs.get(tabID);
    const scan = await this.resolverMarkerVerdict(tabID);
    if (this.tabDocumentEpochs.get(tabID) !== epochAtScan) {
      return { tabID, owned, kind: "stale" };
    }
    // The epoch only advances once noteResolverNavigation() has actually
    // run; a navigation event the listener has not been delivered yet would
    // pass the check above. Re-reading the tab directly catches that gap.
    try {
      const after = await this.api.tabs.get(tabID);
      if (typeof after.url !== "string" || !resolverURLMatches(after.url, resolver)) {
        return { tabID, owned, kind: "stale" };
      }
    } catch {
      return { tabID, owned, kind: "stale" };
    }
    if (scan.outcome === "markers") return { tabID, owned, kind: "verdict", verdict: scan.verdict };
    return { tabID, owned, kind: scan.outcome };
  }

  /** Reduce one origin's batch of observations to exactly one outcome, in
   * the precedence documented on ProbeOutcome: a decisive causal tab always
   * wins outright; failing that, disagreeing siblings are a conflict;
   * failing that, a decisive "in" commits even from a TRUNCATED scan —
   * forging "in" requires a decisive sign-out affordance or storage
   * identity on the origin, and truncation (more candidate tabs than
   * MAX_OBSERVED_TABS_PER_ORIGIN) cannot manufacture one, so refusing to
   * ever commit here would be strictly worse: six-plus open resolver tabs
   * with the operator focused elsewhere would leave the origin unable to
   * advance past "unknown" for as long as those tabs stay open, wedging
   * every queued handoff behind the 45s fallback timer forever. A decisive
   * "out" still commits only from a COMPLETE scan — the asymmetry the
   * classifier already relies on (sign-out affordances may hide, so a
   * lone "in" is trusted; sign-in affordances must be prominent, so "out"
   * demands having actually looked everywhere). Anything short of a
   * decisive "in" commit LEAVES THE VERDICT ALONE — an incomplete, failed,
   * or stale scan must never manufacture "out" for an origin that earned
   * "in" from an earlier, complete probe; a truncated scan that
   * suppressed the only chance to learn anything new instead reports
   * "partial_scan". */
  private reduceObservations(
    observations: readonly TabObservation[],
    causalTabID: number | undefined,
    truncated: boolean,
  ): { outcome: ProbeOutcome; verdict?: SessionVerdict; source?: KeepaliveProbeSource } {
    const causal =
      causalTabID === undefined
        ? undefined
        : observations.find((observation) => observation.tabID === causalTabID);
    if (causal?.kind === "verdict" && causal.verdict !== undefined) {
      return {
        outcome: "markers",
        verdict: causal.verdict,
        source: causal.owned ? "keepalive_tab" : "live_tab",
      };
    }

    const decisiveIn = observations.filter(
      (observation) => observation.kind === "verdict" && observation.verdict === "in",
    );
    const decisiveOut = observations.filter(
      (observation) => observation.kind === "verdict" && observation.verdict === "out",
    );
    if (decisiveIn.length > 0 && decisiveOut.length > 0) {
      return { outcome: "conflict", verdict: "unknown", source: "none" };
    }
    if (decisiveIn.length > 0) {
      return {
        outcome: "markers",
        verdict: "in",
        source: decisiveIn.some((observation) => observation.owned) ? "keepalive_tab" : "live_tab",
      };
    }
    if (!truncated && decisiveOut.length > 0) {
      return {
        outcome: "markers",
        verdict: "out",
        source: decisiveOut.some((observation) => observation.owned) ? "keepalive_tab" : "live_tab",
      };
    }
    if (truncated) return { outcome: "partial_scan" };
    if (observations.some((observation) => observation.kind === "scan_failed")) {
      return { outcome: "scan_failed" };
    }
    if (observations.some((observation) => observation.kind === "no_markers")) {
      return { outcome: "no_markers", verdict: "unknown", source: "none" };
    }
    return { outcome: "no_tab" };
  }

  /** The single write for one probeOrigin() call. `reduction.verdict`
   * present means a NEW verdict was decided; absent means "verdict
   * preserved" — only lastProbeAt/lastProbeOutcome advance. Either way,
   * dirtySince clears: commitOriginProbe() running at all IS the "we
   * looked" event dirtySince exists to demand, even for a "stale"-only
   * batch that collapses to no_tab — whatever caused the staleness (an
   * epoch bump) came from its own noteResolverNavigation() call, which
   * independently re-marked dirty and scheduled its own settle-probe, so
   * clearing here never loses a pending recheck. The owned tab's
   * pause/resume disposition is a SECOND, independent result computed from
   * its own observation only, applied after the snapshot write so a user
   * tab's "in"/"out" never repins or pauses a keepalive tab sitting on a
   * different page.
   *
   * Release authority lives here too: onFreshSessionEvidence fires only for
   * a decisive "markers" commit of "in" from a real tab (live_tab/
   * keepalive_tab), for a CONFIGURED origin (isConfiguredMember) —
   * regardless of whether `authenticated` transitioned, so a restored warm
   * "in" still releases queued work on the next confirming probe.
   * onOriginAuthenticationChanged fires only on an actual committed
   * authenticated flip, also gated to configured origins; it is display-only
   * and must never be read as authorization. */
  private async commitOriginProbe(
    origin: string,
    reduction: { outcome: ProbeOutcome; verdict?: SessionVerdict; source?: KeepaliveProbeSource },
    ownedObservation: TabObservation | undefined,
    generation: number,
  ): Promise<void> {
    const now = Date.now();
    // PROBE_ADMISSION_DEADLINE_MS can free an origin's admission slot while
    // the wedged attempt that held it is still running in the background (see
    // boundedProbe()); a fresher probe can then start, finish and commit
    // BEFORE that stale attempt finally resolves and reaches here. Its result
    // describes a document this manager stopped caring about several probes
    // ago, so it must not be written AT ALL — gating only the callbacks would
    // let it silently overwrite the fresher verdict, flipping the card back to
    // "Signed out" for a session that is fine and firing nothing to say so.
    if (generation !== this.probeGenerations.get(origin)) return;
    const isCurrent = origin === this.resolver?.origin;
    if (reduction.verdict !== undefined) {
      const authenticated = reduction.verdict === "in";
      const source = reduction.source ?? "none";
      const prior = this.originStates.get(origin)?.authenticated ?? false;
      if (isCurrent) {
        this.verdict = reduction.verdict;
        this.probeSource = source;
        this.authenticated = authenticated;
        this.lastVerdictAt = now;
        this.lastProbeAt = now;
        this.lastProbeOutcome = reduction.outcome;
      }
      void this.updateOriginSnapshot(origin, {
        authenticated,
        verdict: reduction.verdict,
        probeSource: source,
        lastVerdictAt: now,
        lastProbeAt: now,
        lastProbeOutcome: reduction.outcome,
        dirtySince: null,
      });
      if (
        reduction.outcome === "markers" &&
        authenticated &&
        (source === "live_tab" || source === "keepalive_tab") &&
        this.isConfiguredMember(origin)
      ) {
        this.options.onFreshSessionEvidence?.({ origin, observedAt: now, generation, source });
      }
      if (authenticated !== prior && this.isConfiguredMember(origin)) {
        this.options.onOriginAuthenticationChanged?.(origin, authenticated);
      }
    } else {
      // Verdict preserved: an incomplete/failed/empty/stale scan is not evidence.
      if (isCurrent) {
        this.lastProbeAt = now;
        this.lastProbeOutcome = reduction.outcome;
      }
      void this.updateOriginSnapshot(origin, {
        lastProbeAt: now,
        lastProbeOutcome: reduction.outcome,
        dirtySince: null,
      });
    }

    const disposition = this.ownedTabDisposition(ownedObservation);
    if (disposition === "pause") await this.pauseForReauth(origin);
    else if (disposition === "resume") await this.resumeAfterReauth(origin);
  }

  /** Computed from the OWNED tab's own observation ONLY — never from the
   * committed origin verdict, which may have come from a different tab
   * entirely. A user tab reading "in" must not repin an owned tab still
   * parked on an IdP; a user tab reading "out" must not pause an owned tab
   * sitting on a good resolver page. */
  private ownedTabDisposition(
    observation: TabObservation | undefined,
  ): "pause" | "resume" | "unchanged" {
    if (observation === undefined || !observation.owned) return "unchanged";
    if (observation.kind === "verdict" && observation.verdict === "in") return "resume";
    if (observation.kind === "auth_url" || (observation.kind === "verdict" && observation.verdict === "out")) {
      return "pause";
    }
    return "unchanged";
  }

  /** Reset an origin to "no evidence yet" WITHOUT recording a completed
   * probe attempt — used only when the manager gives up or adopts a tab,
   * neither of which is itself an inspection. */
  private resetVerdict(origin: string | undefined): void {
    const isCurrent = origin === undefined || origin === this.resolver?.origin;
    if (isCurrent) {
      this.verdict = "unknown";
      this.probeSource = "none";
      this.authenticated = false;
      this.lastVerdictAt = undefined;
      this.lastProbeAt = undefined;
      this.lastProbeOutcome = undefined;
    }
    void this.updateOriginSnapshot(
      origin,
      {
        authenticated: false,
        verdict: "unknown",
        probeSource: "none",
        lastVerdictAt: null,
        lastProbeAt: null,
        likelyAuthenticated: isCurrent ? this.likelyAuthenticated : false,
      },
      true,
    );
  }


  /** Stop scheduling and remove the manager-owned tab. */
  async dispose(): Promise<void> {
    this.clearCycleTimer();
    this.clearReauthTimer();
    for (const timer of this.settleTimers.values()) this.api.timers.clearTimeout(timer);
    this.settleTimers.clear();
    for (const entry of this.spacingTimers.values()) this.api.timers.clearTimeout(entry.timer);
    this.spacingTimers.clear();
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

  /** Shared by reconcile/onObserve/onReload/inspectAfterReload/onReauthTick
   * — the only places that probe the manager's OWN tab as the causal
   * observation. Routes through requestProbe() like every other trigger. */
  private async probeOwnedTab(reason: "cycle" | "reauth"): Promise<void> {
    const origin = this.resolver?.origin;
    if (origin === undefined || this.tabID === undefined) return;
    await this.requestProbe(origin, reason, this.tabID);
  }

  private async reconcile(): Promise<void> {
    const warmDemand = this.hasWarmDemand();
    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    if (!this.enabled || !warmDemand || resolver === undefined) {
      await this.closeTab();
      this.scheduleCycle(this.lastCycleRunAt + this.observeMs, () => this.onObserve());
      return;
    }

    await this.selectResolver(resolver);
    if (this.tabID === undefined) {
      await this.createTab();
      this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
      return;
    }

    if (this.reauthPaused) {
      // reauthTimer already owns the paused recheck loop (armed by
      // pauseForReauth); the cycle timer is left untouched until it resolves.
      return;
    }

    this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
  }

  /** Tail of the serialised openReauth chain. The popup renders one Sign-in
   * button per institution row and each button disables only ITSELF, so two
   * clicks a second apart reach requestSessionSignIn concurrently — and
   * chrome.runtime.onMessage dispatches papio.session.signin outside the
   * serialized inbound native-frame chain, so nothing else orders them.
   *
   * Concurrently is the one way this manager cannot serve them: it holds a
   * SINGLE tabID and a single resolver, so the second request necessarily
   * supersedes the first. Unserialised, both callers ended up waiting on the
   * same tabCreationInFlight promise and their continuations ran in
   * subscription order: A's resolved createTab and DEFERRED its own
   * openReauth continuation to a fresh microtask, so B's continuation ran
   * first, saw this.tabID still holding the tab A's own attempt had just
   * created, and cleared it (removeStaleTab, synchronously) before A could
   * read it. A then returned false for a tab that had genuinely been
   * created, and background.ts's requestSessionSignIn fell through to an
   * UNMANAGED session-signin tab — the tab the keepalive path exists to
   * avoid, since startup orphan reconciliation can close it mid-SAML.
   *
   * Serialising is what makes the boolean honest: A creates, focuses, and
   * returns true; B then supersedes it exactly as an explicit second request
   * should. Note that resolving the shared creation to a tab id instead does
   * NOT fix this — A would hold a concrete id for a tab B had already
   * closed, and tabs.update would throw straight into the same `return
   * false`. createTab()'s own wait-then-retry loop is deliberately left
   * alone: its callers (reconcile/onObserve/onReload) discard the result and
   * only need the origin-correctness the loop already guarantees. */
  private reauthChain: Promise<void> = Promise.resolve();

  /** Focus the reauthentication tab on an explicit operator request. If the
   * keepalive is disabled or has not observed a job yet, this still creates a
   * resolver-origin tab from the latest institutional offer when possible.
   *
   * Serialised against every other openReauth call on this manager; see
   * reauthChain above. */
  async openReauth(originHint?: string): Promise<boolean> {
    const run = this.reauthChain.then(() => this.openReauthOnce(originHint));
    // A rejected request must not poison the queue: requestSessionSignIn
    // catches the throw and opens its own fallback tab, and the operator's
    // NEXT click still has to run.
    this.reauthChain = run.then(
      () => {},
      () => {},
    );
    return run;
  }

  private async openReauthOnce(originHint?: string): Promise<boolean> {
    await this.loadPreferences();
    let requested: URL | undefined;
    if (originHint !== undefined) {
      const normalized = normalizeHttpsOrigin(originHint);
      if (normalized === undefined) return false;
      // Fail closed once hello_ack has landed: an origin outside the
      // daemon's configured set must never get a reauth tab opened for it
      // (verified defect #5 — offer/permission traffic must not create an
      // institution the operator never configured).
      if (this.configuredOriginsReady() && !this.isConfiguredMember(normalized)) return false;
      try {
        requested = new URL(normalized);
      } catch {
        return false;
      }
    }
    const target = requested ?? this.resolverFromLatestOffer() ?? this.configuredResolver() ?? this.resolver;
    if (target?.protocol !== "https:") return false;
    if (this.resolver?.origin !== target.origin && this.tabID !== undefined) {
      await this.removeStaleTab(this.tabID, this.resolver?.origin);
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
      await this.pauseForReauth(target.origin);
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

  /** Persists the current resolver as browser-local display state: the
   * pre-hello fallback row shown before the daemon has confirmed anything,
   * and the last-known origin remembered across restarts. This is NEVER
   * membership — once configuredOriginsReady() is true, only
   * knownResolverOrigins() decides which origins exist; see
   * originCandidates(). */
  private rememberResolverOrigin(resolver: URL): void {
    const origin = normalizeHttpsOrigin(resolver.origin);
    if (origin === undefined || this.persistedResolverOrigin === origin) return;
    this.persistedResolverOrigin = origin;
    const save = this.api.storage.set?.({ [KEEPALIVE_RESOLVER_ORIGIN_KEY]: origin });
    if (save !== undefined) void save.catch(() => {});
  }

  private async resolverMarkerVerdict(
    tabID: number,
  ): Promise<
    | { outcome: "markers"; verdict: "in" | "out" }
    | { outcome: "no_markers" | "scan_failed" }
  > {
    const executeScript = this.api.scripting?.executeScript;
    if (executeScript === undefined) {
      return { outcome: "scan_failed" };
    }
    try {
      const [injection] = await executeScript({
        target: { tabId: tabID },
        func: collectResolverMarkers,
      });
      const markers = injection?.result;
      // Mirror the 50-key/8-KiB storage discipline collectResolverMarkers()
      // applies to itself: the array crosses a structured-clone trust
      // boundary from a page papio does not control, and MAX_SCANNED_CONTROLS
      // is the collector's own element cap, so anything longer than that
      // cannot be a genuine result of the current collector — treat it as a
      // failed scan rather than handing an oversized, unbounded array to the
      // classifier.
      if (!Array.isArray(markers) || markers.length > MAX_SCANNED_CONTROLS) {
        return { outcome: "scan_failed" };
      }
      const verdict = classifyResolverMarkers(markers as ResolverMarker[]);
      if (verdict === "unknown") return { outcome: "no_markers" };
      return { outcome: "markers", verdict };
    } catch {
      // Privileged pages, revoked host permission, and closed tabs expose a
      // distinct scan failure so the popup can explain the missing access.
      return { outcome: "scan_failed" };
    }
  }

  /** Remove a tab whose origin the manager no longer wants, because the
   * resolver moved on while the tab was still live. Shared by openReauth's
   * ordinary origin-switch teardown and by createTab()'s wait-then-retry
   * path (see the tabCreationInFlight doc comment above): without this,
   * the wait-then-retry path would overwrite this.tabID with the NEW
   * origin's tab and leave the settled first attempt's tab pinned+muted
   * with nothing referencing it — the tab governor skips pinned tabs on
   * purpose, so that orphan is never reconciled or closed. `origin` is
   * the origin the removed tab was opened for, which the caller must pass
   * explicitly: by the time this runs, this.resolver may already have
   * moved on to a different one. */
  private async removeStaleTab(tabID: number, origin: string | undefined): Promise<void> {
    const wasPaused = this.reauthPaused;
    if (this.tabID === tabID) {
      this.tabID = undefined;
      this.reauthPaused = false;
      this.clearReauthTimer();
    }
    void this.updateOriginSnapshot(origin, { pausedForReauth: false });
    if (wasPaused) this.options.onReauthStateChanged?.(false);
    try {
      await this.api.tabs.remove(tabID);
    } catch {
      // A manually closed tab is already in the desired state.
    }
  }

  private async createTab(): Promise<void> {
    for (;;) {
      const resolver = this.resolver;
      const wantedOrigin = resolver?.protocol === "https:" ? resolver.origin : undefined;
      if (this.tabCreationInFlight === undefined) {
        this.tabCreationOrigin = wantedOrigin;
        const attempt = this.createTabOnce();
        this.tabCreationInFlight = attempt.finally(() => {
          this.tabCreationInFlight = undefined;
          this.tabCreationOrigin = undefined;
        });
        await this.tabCreationInFlight;
        return;
      }
      if (wantedOrigin === this.tabCreationOrigin) {
        await this.tabCreationInFlight;
        return;
      }
      // Wanted a different origin than the creation already in flight
      // (e.g. openReauth switching institutions mid-race). Wait for the
      // stale attempt to settle first, so it can never be starved, then
      // tear down any tab it produced for the origin nobody wants anymore
      // (a failed attempt leaves this.tabID undefined, so there is nothing
      // to close) before looping to drive our own origin-correct creation
      // instead of joining a promise that would hand back the wrong
      // institution's tab.
      const staleOrigin = this.tabCreationOrigin;
      await this.tabCreationInFlight;
      if (this.tabID !== undefined) await this.removeStaleTab(this.tabID, staleOrigin);
    }
  }

  private async createTabOnce(): Promise<void> {
    // Snapshot once: all four callers (reconcile, onObserve, onReload,
    // openReauth) can mutate this.resolver synchronously before calling
    // createTab, and this method itself awaits across tabs.query()/
    // tabs.create(). Reading this.resolver again after either await let a
    // racing caller's origin switch leak into an in-progress creation —
    // querying for one origin's existing tab but creating (or claiming) a
    // tab under a different one. A local snapshot keeps this call
    // consistent with whichever origin it actually started for; createTab()
    // above is what keeps a DIFFERENT origin from riding this call instead
    // of starting its own.
    const resolver = this.resolver;
    if (resolver === undefined) return;
    try {
      const existing = await this.api.tabs.query({
        pinned: true,
        muted: true,
        url: [`${resolver.protocol}//${resolver.host}/*`],
      });
      const tabID = existing.find((tab) => tab.id !== undefined)?.id;
      if (tabID !== undefined) {
        this.tabID = tabID;
        this.clearReauthPause(resolver.origin);
        // The owned-tab cycle "just ran" the moment we take ownership of a
        // tab, whether adopted here or freshly created below — otherwise
        // the very first scheduleCycle(lastCycleRunAt + intervalMs()) call
        // would compute a due time still stuck at epoch 0 and fire almost
        // immediately instead of a full interval from now.
        this.lastCycleRunAt = Date.now();
        this.resetVerdict(resolver.origin);
        return;
      }
    } catch {
      // Querying is a best-effort restart recovery; creation below remains safe.
    }
    const base = {
      url: resolver.origin,
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
      this.clearReauthPause(resolver.origin);
      this.lastCycleRunAt = Date.now();
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
      this.scheduleCycle(this.lastCycleRunAt + this.observeMs, () => this.onObserve());
      return;
    }

    await this.selectResolver(resolver);
    if (this.tabID === undefined) {
      await this.createTab();
      this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
      return;
    }

    if (this.reauthPaused) return;

    this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
  }

  private async onReload(): Promise<void> {
    await this.loadPreferences();
    if (!this.enabled || !this.hasWarmDemand()) {
      await this.closeTab();
      this.scheduleCycle(this.lastCycleRunAt + this.observeMs, () => this.onObserve());
      return;
    }

    const resolver = this.resolverFromLatestOffer() ?? this.configuredResolver();
    if (resolver === undefined) {
      await this.closeTab();
      this.scheduleCycle(this.lastCycleRunAt + this.observeMs, () => this.onObserve());
      return;
    }
    await this.selectResolver(resolver);
    if (this.tabID === undefined) {
      await this.createTab();
      this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
      return;
    }
    if (this.reauthPaused) return;

    try {
      await this.api.tabs.reload(this.tabID);
    } catch {
      this.tabID = undefined;
      this.scheduleCycle(this.lastCycleRunAt + this.observeMs, () => this.onObserve());
      return;
    }
    this.scheduleCycle(Date.now() + this.reloadSettleMs, () => this.inspectAfterReload());
  }

  private async inspectAfterReload(): Promise<void> {
    await this.probeOwnedTab("cycle");
    if (this.reauthPaused) return;
    this.scheduleCycle(this.lastCycleRunAt + this.intervalMs(), () => this.onReload());
  }


  private async pauseForReauth(origin: string): Promise<void> {
    if (this.reauthPaused || this.tabID === undefined) return;
    if (this.resolver?.origin !== origin) return;
    this.reauthPaused = true;
    void this.updateOriginSnapshot(origin, { pausedForReauth: true });
    const pausedTabID = this.tabID;
    try {
      await this.api.tabs.update(pausedTabID, { active: true, pinned: false, muted: false });
      // In work-window mode the tab lives in a minimized window; bring it up.
      await this.options.surfaceReauthTab?.(pausedTabID);
    } catch {
      // The reauth callback/badge still gives the user a recoverable signal.
    }
    // The resolver can move to another institution while this call is in
    // flight — openReauth() is serialized only against itself, not against a
    // probe's disposition — and a late-landing pause for the OLD origin must
    // never raise "needs sign-in" over the NEW one's healthy session, nor
    // surface a tab id that now belongs to it. resumeAfterReauth() re-checks
    // for the mirror-image reason; this is the same rule on the way in.
    if (this.resolver?.origin !== origin || this.tabID !== pausedTabID) return;
    this.options.onReauthStateChanged?.(true);
    this.options.onReauthNeeded?.();
    // Guarantee a prompt recheck regardless of caller: an explicit
    // openReauth() call reaches here OUTSIDE the reconcile/onObserve/
    // onReload chain, so without arming our own timer this would otherwise
    // inherit whatever cycleTimer already happened to be pending — up to a
    // full interval away.
    this.armReauthTimer();
  }

  /** Drop a reauthentication pause, in memory AND in the persisted snapshot.
   *
   * Taking ownership of a fresh or adopted tab must clear both. pauseForReauth
   * unpins and unmutes the tab it parks, so a service worker suspended
   * mid-pause comes back with `tabID`/`reauthPaused` at their in-memory
   * defaults while the origin snapshot still says pausedForReauth — and the
   * adoption query, which looks for a pinned+muted tab, cannot find the very
   * tab the operator may be signing in on. Without this the popup kept
   * reporting "Waiting on your sign-in" for a session nothing was waiting on,
   * with no reauth timer armed to ever re-check it. */
  private clearReauthPause(origin: string | undefined): void {
    this.reauthPaused = false;
    this.clearReauthTimer();
    if (origin !== undefined) void this.updateOriginSnapshot(origin, { pausedForReauth: false });
  }

  private armReauthTimer(): void {
    this.clearReauthTimer();
    this.reauthTimer = this.api.timers.setTimeout(() => {
      this.reauthTimer = undefined;
      void this.onReauthTick();
    }, this.observeMs);
  }

  private clearReauthTimer(): void {
    if (this.reauthTimer !== undefined) this.api.timers.clearTimeout(this.reauthTimer);
    this.reauthTimer = undefined;
  }

  private async onReauthTick(): Promise<void> {
    await this.loadPreferences();
    if (!this.reauthPaused || this.tabID === undefined) return;
    await this.probeOwnedTab("reauth");
    if (this.reauthPaused) this.armReauthTimer();
  }

  /** Clears the LOGICAL pause for `origin` even when the owned tab that
   * earned this "in" reading is already gone — a dead tab must never leave
   * an origin stuck "paused" forever. Only touches this.reauthPaused/
   * reauthTimer/onReauthStateChanged (the CURRENT resolver's live state)
   * when `origin` still IS the current resolver, checked both before and
   * after the tabs.update() await: the resolver can move to a different
   * institution while this call is in flight, and a late-resolving resume
   * for the OLD origin must never clear the NEW one's pause. Re-pinning is
   * likewise conditional on the owned tab still existing and still
   * belonging to `origin`. */
  private async resumeAfterReauth(origin: string): Promise<void> {
    void this.updateOriginSnapshot(origin, { pausedForReauth: false });
    const tabID = this.tabID;
    if (tabID === undefined || this.resolver?.origin !== origin) {
      if (this.resolver?.origin === origin) {
        this.reauthPaused = false;
        this.clearReauthTimer();
        this.options.onReauthStateChanged?.(false);
      }
      return;
    }
    try {
      await this.api.tabs.update(tabID, { pinned: true, muted: true });
    } catch {
      // The tab is still usable; retry normal keepalive on the next cycle.
    }
    if (this.resolver?.origin !== origin) return;
    this.reauthPaused = false;
    this.clearReauthTimer();
    this.options.onReauthStateChanged?.(false);
  }


  private intervalMs(): number {
    return this.intervalMinutes * 60_000;
  }

  /** "No later than": if a cycle timer is already pending for a due time at
   * or before `dueAt`, it is left alone — a later request must never push
   * an already-correct deadline out. reconcile()/onObserve()/onReload() are
   * idempotent, state-driven re-evaluations, so even the one case this can
   * briefly leave a "wrong-purpose" timer pending (e.g. an idle poll still
   * armed just after a tab was created) is harmless: whichever callback
   * fires next re-derives the correct next step from current state and
   * reschedules correctly on its own. */
  private scheduleCycle(dueAt: number, callback: () => Promise<void>): void {
    if (this.cycleTimer !== undefined && this.nextCycleDueAt !== undefined && this.nextCycleDueAt <= dueAt) {
      return;
    }
    if (this.cycleTimer !== undefined) this.api.timers.clearTimeout(this.cycleTimer);
    this.nextCycleDueAt = dueAt;
    const delay = Math.max(0, dueAt - Date.now());
    this.cycleTimer = this.api.timers.setTimeout(async () => {
      this.cycleTimer = undefined;
      this.nextCycleDueAt = undefined;
      this.lastCycleRunAt = Date.now();
      await callback();
    }, delay);
  }

  private clearCycleTimer(): void {
    if (this.cycleTimer !== undefined) this.api.timers.clearTimeout(this.cycleTimer);
    this.cycleTimer = undefined;
    this.nextCycleDueAt = undefined;
  }

  private originFromURL(rawURL: string | undefined): string | undefined {
    if (rawURL === undefined) return undefined;
    try {
      const url = new URL(rawURL);
      if (url.protocol !== "https:") return undefined;
      return normalizeHttpsOrigin(url.origin);
    } catch {
      return undefined;
    }
  }

  /** Debounced per-origin "let the SPA render" delay: each call for the
   * same origin replaces the previous timer, so only the LAST navigation
   * event in a burst (url-change, then complete) actually triggers a
   * probe. */
  private armSettleTimer(origin: string, tabID: number): void {
    const existing = this.settleTimers.get(origin);
    if (existing !== undefined) this.api.timers.clearTimeout(existing);
    const timer = this.api.timers.setTimeout(() => {
      this.settleTimers.delete(origin);
      void this.requestProbe(origin, "navigation", tabID);
    }, this.reloadSettleMs);
    this.settleTimers.set(origin, timer);
  }

  /** A tab finished loading, or changed URL. Cheap and synchronous up
   * front — epoch bump, origin-membership check, and dirty/settle
   * bookkeeping all happen before any await, so a wake event can never be
   * reordered or lost while this manager is still hydrating. */
  noteResolverNavigation(tabID: number, rawURL: string | undefined): void {
    this.tabDocumentEpochs.set(tabID, (this.tabDocumentEpochs.get(tabID) ?? 0) + 1);
    this.syncOriginStates();
    const origin = this.originFromURL(rawURL);
    const previousOrigin = this.tabResolverOrigins.get(tabID);
    const known = origin !== undefined && this.originStates.has(origin);
    const leftOrigin = previousOrigin !== undefined && previousOrigin !== origin ? previousOrigin : undefined;

    if (!known || origin === undefined) {
      this.tabResolverOrigins.delete(tabID);
    } else {
      this.tabResolverOrigins.set(tabID, origin);
      // A resolver SPA renders its header and mints its session token after
      // the load event, not at navigation-start — probing immediately would
      // race the page itself.
      this.armSettleTimer(origin, tabID);
    }

    // The synchronous bookkeeping above is already done; only the durable
    // persist of dirtySince needs to land before a worker might sleep again.
    void (async () => {
      if (leftOrigin !== undefined) await this.markDirty(leftOrigin);
      if (known && origin !== undefined) await this.markDirty(origin);
    })();
  }

  /** The operator switched to a tab. ADR-0013 privileges the focused tab,
   * so this probes immediately (no settle delay) instead of waiting for a
   * navigation that may never come — the tab could already be sitting on a
   * fully rendered resolver page. Never commits a verdict directly; it only
   * requests one through the same admission-controlled path as everything
   * else. */
  noteResolverActivated(tabID: number, rawURL: string | undefined): void {
    this.syncOriginStates();
    const origin = this.originFromURL(rawURL);
    const previousOrigin = this.tabResolverOrigins.get(tabID);
    const known = origin !== undefined && this.originStates.has(origin);
    const leftOrigin = previousOrigin !== undefined && previousOrigin !== origin ? previousOrigin : undefined;

    if (!known || origin === undefined) {
      this.tabResolverOrigins.delete(tabID);
    } else {
      this.tabResolverOrigins.set(tabID, origin);
    }

    void (async () => {
      if (leftOrigin !== undefined) await this.markDirty(leftOrigin);
      if (known && origin !== undefined) {
        await this.markDirty(origin);
        await this.requestProbe(origin, "activation", tabID);
      }
    })();
  }

  /** A tab is gone: drop its epoch, settle timer, and origin association.
   * If it was the manager-owned tab, clear tabID and the logical reauth
   * pause exactly as closeTab() does — a dead tab must never leave an
   * origin stuck "paused" with nothing left to resume it. */
  noteTabRemoved(tabID: number): void {
    this.tabDocumentEpochs.delete(tabID);
    const origin = this.tabResolverOrigins.get(tabID);
    this.tabResolverOrigins.delete(tabID);
    if (origin !== undefined) {
      const settleTimer = this.settleTimers.get(origin);
      if (settleTimer !== undefined) {
        this.api.timers.clearTimeout(settleTimer);
        this.settleTimers.delete(origin);
      }
    }
    if (this.tabID !== tabID) return;
    const ownedOrigin = this.resolver?.origin;
    const wasPaused = this.reauthPaused;
    this.tabID = undefined;
    this.reauthPaused = false;
    this.clearReauthTimer();
    void this.updateOriginSnapshot(ownedOrigin, { pausedForReauth: false });
    if (wasPaused) this.options.onReauthStateChanged?.(false);
    this.resetVerdict(ownedOrigin);
  }

  /** Periodic wake, and the durable recovery path for events lost to a
   * suspended worker: setTimeout-based timers (cycleTimer/reauthTimer/
   * settleTimers/spacingTimers) never survive a service-worker restart, but
   * dirtySince does. Probes only origins that are dirty or paused for
   * reauth, and runs the owned-tab cycle work when its absolute deadline
   * has passed — pure local state, no daemon-port/message involvement. */
  async onWake(): Promise<void> {
    await this.loadPreferences();
    const now = Date.now();
    const due: string[] = [];
    for (const [origin, snapshot] of this.originStates) {
      if (snapshot.dirtySince !== null || snapshot.pausedForReauth) due.push(origin);
    }
    await Promise.all(due.map((origin) => this.requestProbe(origin, "wake")));

    if (this.nextCycleDueAt !== undefined && this.nextCycleDueAt <= now) {
      await this.reconcile();
    }
  }

  private async closeTab(): Promise<void> {
    const tabID = this.tabID;
    const origin = this.resolver?.origin;
    const wasAwaitingReauth = this.reauthPaused;
    this.tabID = undefined;
    this.reauthPaused = false;
    this.clearReauthTimer();
    void this.updateOriginSnapshot(origin, { pausedForReauth: false });
    if (wasAwaitingReauth) this.options.onReauthStateChanged?.(false);
    this.resetVerdict(origin);
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
