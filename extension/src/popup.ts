// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Popup: a minimal launcher for acquiring the active page or opening the
// full-tab inbox. It never talks to the native host directly; actions route
// through the background broker.

// It also hosts a developer-only "Capture fixture" panel used during adapter
// work. Sanitization and the fail-closed residual-secret guard live in
// ./capture; the popup only wires the DOM.

import {
  captureFixture,
  PROVIDERS,
  SCENARIOS,
  type ChromeCaptureApi,
  type PageCapture,
  type Provider,
  type Scenario,
} from "./capture";
import { chromeBackend, type ActiveJob, type PendingDelivery, type StoreShape, TERMS_CONSENT_KEY } from "./state";
import type { ActivityEntryPayload } from "./protocol";
import { classifyPage, isPDFPage, pdfSourceURL, sniffDOI, type PageKind } from "./deliver";
import {
  SESSION_STALE_MS,
  type KeepaliveOriginSnapshot,
  type KeepaliveSnapshot,
} from "./keepalive";
import { renderPapio } from "./dom";
import {
  EST_MINUTES_SAVED_PER_PAPER,
  formatHoursSaved,
  formatShare,
  parseStatsReply,
  type AcquisitionStats,
} from "./stats";


declare const __PAPIO_DEV_CAPTURE__: boolean;

/** Render actionable daemon problems near the popup actions. Routine version
 * diagnostics live behind the settings page's collapsed disclosure. */
export function renderDaemonStatus(
  doc: Document,
  status: Pick<StoreShape, "connectionStatus" | "daemonVersion" | "daemonUpdateHint">,
): void {
  const card = doc.getElementById("daemon-status");
  const message = doc.getElementById("daemon-status-message");
  const hint = doc.getElementById("daemon-status-hint");
  if (!card || !message || !hint) return;

  let line = "";
  let action = "";
  switch (status.connectionStatus ?? "disconnected") {
    case "connected": {
      const stampedVersion =
        typeof __PAPIO_DAEMON_VERSION__ === "string" ? __PAPIO_DAEMON_VERSION__ : "";
      if (
        status.daemonUpdateHint === true &&
        stampedVersion !== "" &&
        stampedVersion !== "0.0.0-dev" &&
        typeof status.daemonVersion === "string" &&
        status.daemonVersion.length > 0
      ) {
        const developmentDaemon = /(?:^|[-.])dev(?:[.-]|$)/i.test(status.daemonVersion);
        line = `papio ${stampedVersion} is available — ${
          developmentDaemon
            ? `your daemon is a development build (v${status.daemonVersion})`
            : `daemon is v${status.daemonVersion}`
        }`;
        action = developmentDaemon
          ? "Update the source checkout, then run: make dev-deploy"
          : "brew upgrade papio, then: papio daemon stop";
      }
      break;
    }
    case "daemon_outdated":
      line = "papio daemon is out of date — update papio to keep downloads working";
      action = "update papio, then restart the daemon";
      break;
    case "extension_outdated":
      line = "this extension is older than your papio daemon supports — update it from your browser's extension store";
      break;
    case "disconnected":
      line = "papio daemon isn't reachable";
      action = "run: papio daemon status";
      break;
  }
  card.hidden = line.length === 0;
  renderPapio(message, line);
  hint.textContent = action;
}


export async function sendTermsConsent(value: "accept" | "manual"): Promise<void> {
  await chrome.runtime.sendMessage({ channel: "papio", action: "terms_consent", value });
}

/**
 * Show the one-time informed-consent prompt when a job hit a publisher
 * terms-and-conditions gate and the user has not yet chosen. Pure over the
 * document so it is testable; the caller supplies the current consent value and
 * the choice handler. Hidden once a choice exists or no terms gate is pending.
 */
export function renderTermsConsent(
  doc: Document,
  jobs: ActiveJob[],
  consent: "accept" | "manual" | undefined,
  onChoice: (value: "accept" | "manual") => void,
): void {
  const card = doc.getElementById("terms-consent");
  if (!card) return;
  const pending = consent === undefined && jobs.some((j) => j.needs_terms_consent === true);
  card.hidden = !pending;
  if (!pending) return;
  const enable = doc.getElementById("terms-consent-enable");
  const decline = doc.getElementById("terms-consent-decline");
  if (enable instanceof HTMLButtonElement && !enable.dataset.wired) {
    enable.dataset.wired = "1";
    enable.addEventListener("click", () => onChoice("accept"));
  }
  if (decline instanceof HTMLButtonElement && !decline.dataset.wired) {
    decline.dataset.wired = "1";
    decline.addEventListener("click", () => onChoice("manual"));
  }
}

/**
 * Surface a one-click grant for library resolvers papio cannot yet steer. The
 * origins come from the daemon's config (via hello_ack), so the user never needs
 * to know or type a URL. `onAllow` must reach chrome.permissions.request inside
 * the button's click gesture, so the ungranted set is computed by the caller.
 */
export function renderResolverGrants(
  doc: Document,
  ungrantedOrigins: string[],
  onAllow: (origins: string[]) => void,
): void {
  const container = doc.getElementById("resolver-grant");
  if (!(container instanceof HTMLElement)) return;
  container.replaceChildren();
  container.hidden = ungrantedOrigins.length === 0;
  if (ungrantedOrigins.length === 0) return;

  const hosts = ungrantedOrigins.map((origin) => origin.replace(/^https:\/\//, "")).join(", ");
  const heading = doc.createElement("h2");
  heading.textContent = "Library access";
  const lede = doc.createElement("p");
  renderPapio(lede, `Allow papio to use your library resolver so it can finish downloads without a manual click: ${hosts}`);
  const button = doc.createElement("button");
  button.className = "primary";
  button.type = "button";
  button.textContent = "Allow library access";
  button.addEventListener("click", () => {
    button.disabled = true;
    onAllow(ungrantedOrigins);
  });
  container.append(heading, lede, button);
}
interface PageAcquireResponse {
  job_id?: string;
  duplicate?: boolean;
  error?: string | { code?: string; message?: string };
  state?: "sending" | "downloaded" | "failed" | "adopted";
  message?: string;
}

interface PageMetadata {
  url: string;
  doi?: string;
  title?: string;
  kind?: PageKind;
  tab_id?: number;
}

const NO_DOI_FOUND = "no DOI found on this page";

/**
 * Runs INSIDE the page via scripting.executeScript — must stay fully
 * self-contained (no outer-scope references survive serialization).
 *
 * DOI sources are deliberately ordered from page-authored metadata through
 * stable links and finally visible text. The daemon re-validates and
 * normalizes whatever we send.
 */
export function collectPageMetadata(): PageMetadata {
  const clean = (value: string | null | undefined): string => (value ?? "").trim();
  const firstDOI = (value: string): string => {
    let decoded = value;
    try { decoded = decodeURIComponent(value); } catch { /* keep raw */ }
    const match = decoded.match(/\b10\.\d{4,9}\/[^\s"'<>?#]+/);
    return match === null ? "" : match[0].replace(/[.,;:!?\]}>'"]+$/g, "");
  };
  const metaTags = Array.from(document.querySelectorAll("meta[name]"));
  const metaValues = (name: string): string[] =>
    metaTags
      .filter((element) => clean(element.getAttribute("name")).toLowerCase() === name)
      .map((element) => clean(element.getAttribute("content")))
      .filter((value) => value.length > 0);
  let doi = "";
  // Specific standards win over broad fallbacks; publication_doi retains
  // support for older SAGE/Atypon pages.
  for (const name of [
    "citation_doi",
    "dc.identifier",
    "prism.doi",
    "publication_doi",
    "citation_pdf_url",
  ]) {
    for (const value of metaValues(name)) {
      doi = firstDOI(value);
      if (doi) break;
    }
    if (doi) break;
  }
  if (!doi) {
    const canonical = clean(document.querySelector('link[rel="canonical"]')?.getAttribute("href"));
    const ogURL = clean(document.querySelector('meta[property="og:url"]')?.getAttribute("content"));
    doi = firstDOI(canonical) || firstDOI(ogURL) || firstDOI(location.href);
  }
  if (!doi) {
    for (const link of Array.from(document.querySelectorAll("a[href]")).slice(0, 1000)) {
      const href = clean(link.getAttribute("href"));
      try {
        const hostname = new URL(href, location.href).hostname.toLowerCase();
        if (hostname !== "doi.org" && !hostname.endsWith(".doi.org")) continue;
      } catch {
        continue;
      }
      doi = firstDOI(href);
      if (doi) break;
    }
  }
  if (!doi) {
    doi = firstDOI((document.body?.innerText ?? "").slice(0, 200_000));
  }
  if (!doi) {
    try {
      const pageURL = new URL(location.href);
      if (
        pageURL.hostname.toLowerCase() === "jstor.org" ||
        pageURL.hostname.toLowerCase().endsWith(".jstor.org")
      ) {
        const stableID = pageURL.pathname.match(/^\/stable\/([^/]+)\/?$/i)?.[1];
        if (stableID) {
          let decoded = stableID;
          try { decoded = decodeURIComponent(stableID); } catch { /* keep raw */ }
          doi = `10.2307/${decoded}`;
        }
      }
    } catch {
      // A malformed location cannot produce a safe stable-page identifier.
    }
  }
  const title = metaValues("citation_title")[0] || document.title.trim();
  return {
    url: location.href,
    ...(doi ? { doi } : {}),
    ...(title ? { title } : {}),
  };
}

/** Read the active page under the popup's transient activeTab grant. */
export async function readCurrentPageMetadata(): Promise<PageMetadata> {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab?.id === undefined) throw new Error("No active tab");
  const tabURL = typeof tab.url === "string" ? tab.url : "";
  const contentType =
    typeof (tab as unknown as Record<string, unknown>)["contentType"] === "string"
      ? (tab as unknown as Record<string, unknown>)["contentType"] as string
      : undefined;
  const tabPDF = isPDFPage(tabURL, contentType);
  let metadata: PageMetadata | undefined;
  try {
    const [injected] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: collectPageMetadata,
    });
    const result = injected?.result;
    if (typeof result === "object" && result !== null && typeof (result as PageMetadata).url === "string") {
      metadata = result as PageMetadata;
    }
  } catch {
    // PDF viewers and privileged pages reject scripting; their tab URL is
    // enough to classify the page and start a browser-managed download.
  }
  const url = tabURL || metadata?.url;
  if (url === undefined || url.length === 0) throw new Error("Could not read the current page");
  const inferredDOI = metadata?.doi ?? sniffDOI(url);
  const classification = tabPDF
    ? { kind: "pdf" as const, ...(inferredDOI ? { doi: inferredDOI } : {}) }
    : classifyPage(url, {
        ...(inferredDOI ? { doi: inferredDOI } : {}),
        ...(contentType ? { contentType } : {}),
      });
  const pageDOI = inferredDOI ?? classification.doi;
  return {
    url,
    ...(pageDOI ? { doi: pageDOI } : {}),
    ...(metadata?.title || tab.title ? { title: metadata?.title || tab.title! } : {}),
    kind: classification.kind,
    tab_id: tab.id,
  };
}

export const OPEN_INBOX_MESSAGE = "papio.openInbox";

/** Ask the broker to focus its singleton inbox, with a direct-page fallback
 * while the broker rollout is still skewed. */
export async function openInbox(): Promise<void> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({ type: OPEN_INBOX_MESSAGE });
    if (
      typeof response === "object" &&
      response !== null &&
      (response as Record<string, unknown>).opened === true
    ) {
      return;
    }
  } catch {
    // Older workers do not know this launcher request. Open the page directly.
  }
  await chrome.tabs.create({ url: "dist/inbox.html" });
}

export const OPEN_HANDOFF_MESSAGE = "papio.handoff.open";

/** Ask the background broker to surface the tab it already owns for this job.
 * The job id is the only handoff detail a popup may send; resolver URLs remain
 * inside the extension so a popup cannot accidentally disclose a signed link. */
export async function openHandoff(jobID: string): Promise<void> {
  const response: unknown = await chrome.runtime.sendMessage({
    type: OPEN_HANDOFF_MESSAGE,
    request: { job_id: jobID },
  });
  if (
    typeof response === "object" &&
    response !== null &&
    (response as Record<string, unknown>)["ok"] === true &&
    (response as Record<string, unknown>)["opened"] === true
  ) {
    return;
  }
  throw new Error("Could not focus the institutional sign-in");
}

export type PopupSessionState = KeepaliveSnapshot & {
  releasedAuthJobs: number;
  /** Epoch ms of the latest release event; the notice shows once per stamp. */
  releasedAuthJobsAt?: number | null;
  /** One independently probed state for every configured resolver origin. */
  origins?: KeepaliveOriginSnapshot[];
};

export const SESSION_STATE_MESSAGE = "papio.session.state";
export const SESSION_SIGNIN_MESSAGE = "papio.session.signin";
export const SESSION_RETRY_MESSAGE = "papio.session.retry";

function isOriginSnapshot(value: unknown): value is KeepaliveOriginSnapshot {
  if (typeof value !== "object" || value === null) return false;
  const snapshot = value as Record<string, unknown>;
  return (
    typeof snapshot["origin"] === "string" &&
    /^https:\/\/[^/]+$/.test(snapshot["origin"]) &&
    typeof snapshot["authenticated"] === "boolean" &&
    (snapshot["verdict"] === "in" || snapshot["verdict"] === "out" || snapshot["verdict"] === "unknown") &&
    (snapshot["probeSource"] === "live_tab" ||
      snapshot["probeSource"] === "keepalive_tab" ||
      snapshot["probeSource"] === "none") &&
    (snapshot["scanOutcome"] === undefined ||
      snapshot["scanOutcome"] === "markers" ||
      snapshot["scanOutcome"] === "no_markers" ||
      snapshot["scanOutcome"] === "scan_failed") &&
    (snapshot["lastVerdictAt"] === null || typeof snapshot["lastVerdictAt"] === "number") &&
    typeof snapshot["checking"] === "boolean" &&
    typeof snapshot["likelyAuthenticated"] === "boolean" &&
    typeof snapshot["pausedForReauth"] === "boolean" &&
    (snapshot["lastCheckAt"] === null || typeof snapshot["lastCheckAt"] === "number")
  );
}

function isSessionState(value: unknown): value is PopupSessionState {
  if (typeof value !== "object" || value === null) return false;
  const state = value as Record<string, unknown>;
  const resolverOrigin = state["resolverOrigin"];
  const origins = state["origins"];
  return (
    typeof state["enabled"] === "boolean" &&
    typeof state["authenticated"] === "boolean" &&
    (state["verdict"] === undefined ||
      state["verdict"] === "in" ||
      state["verdict"] === "out" ||
      state["verdict"] === "unknown") &&
    (state["probeSource"] === undefined ||
      state["probeSource"] === "live_tab" ||
      state["probeSource"] === "keepalive_tab" ||
      state["probeSource"] === "none") &&
    (state["scanOutcome"] === undefined ||
      state["scanOutcome"] === "markers" ||
      state["scanOutcome"] === "no_markers" ||
      state["scanOutcome"] === "scan_failed") &&
    (state["lastVerdictAt"] === undefined ||
      state["lastVerdictAt"] === null ||
      typeof state["lastVerdictAt"] === "number") &&
    (state["checking"] === undefined || typeof state["checking"] === "boolean") &&
    (state["likelyAuthenticated"] === undefined || typeof state["likelyAuthenticated"] === "boolean") &&
    typeof state["pausedForReauth"] === "boolean" &&
    (state["lastCheckAt"] === null || typeof state["lastCheckAt"] === "number") &&
    (resolverOrigin === null ||
      (typeof resolverOrigin === "string" && /^https:\/\/[^/]+$/.test(resolverOrigin))) &&
    (state["lastAuthReturnedAt"] === null || typeof state["lastAuthReturnedAt"] === "number") &&
    typeof state["queuedAuthJobs"] === "number" &&
    Array.isArray(state["stalledAuthJobs"]) &&
    state["stalledAuthJobs"].every((jobID) => typeof jobID === "string") &&
    typeof state["releasedAuthJobs"] === "number" &&
    (state["releasedAuthJobsAt"] === undefined ||
      state["releasedAuthJobsAt"] === null ||
      typeof state["releasedAuthJobsAt"] === "number") &&
    (origins === undefined || (Array.isArray(origins) && origins.every(isOriginSnapshot)))
  );
}

export async function requestSessionState(): Promise<PopupSessionState | undefined> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({ type: SESSION_STATE_MESSAGE });
    if (typeof response !== "object" || response === null) return undefined;
    const envelope = response as Record<string, unknown>;
    const state = envelope["state"];
    if (!isSessionState(state)) return undefined;
    const origins = envelope["origins"];
    if (origins === undefined) return state;
    return Array.isArray(origins) && origins.every(isOriginSnapshot)
      ? { ...state, origins }
      : state;
  } catch {
    return undefined;
  }
}

export async function requestOrphanTabCount(): Promise<number> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({
      channel: "papio",
      action: "orphan_tabs_status",
    });
    if (typeof response !== "object" || response === null) return 0;
    const count = (response as Record<string, unknown>)["count"];
    return typeof count === "number" && count > 0 ? count : 0;
  } catch {
    return 0;
  }
}

export async function cleanupOrphanTabs(): Promise<number> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({
      channel: "papio",
      action: "orphan_tabs_cleanup",
    });
    if (typeof response !== "object" || response === null) return 0;
    const closed = (response as Record<string, unknown>)["closed"];
    return typeof closed === "number" ? closed : 0;
  } catch {
    return 0;
  }
}

const leftoverCleanupHandlers = new WeakMap<HTMLButtonElement, () => Promise<number>>();

/** Offer a one-click close of tabs papio opened in a previous extension life
 * and can no longer track (reloads wipe the session store; the durable ledger
 * and the papio tab-group sweep still recognize them). Hidden at zero. */
export function renderLeftoverTabs(
  doc: Document,
  count: number,
  onCleanup: () => Promise<number> = cleanupOrphanTabs,
): void {
  const section = doc.getElementById("leftover-tabs");
  const message = doc.getElementById("leftover-tabs-message");
  const button = doc.getElementById("leftover-tabs-cleanup");
  if (
    !(section instanceof HTMLElement) ||
    !(message instanceof HTMLElement) ||
    !(button instanceof HTMLButtonElement)
  ) {
    return;
  }
  if (count <= 0) {
    section.hidden = true;
    return;
  }
  section.hidden = false;
  message.textContent =
    count === 1
      ? "1 untracked tab left from an earlier session."
      : `${count} untracked tabs left from an earlier session.`;
  button.textContent = "Close them";
  leftoverCleanupHandlers.set(button, onCleanup);
  if (!button.dataset.wired) {
    button.dataset.wired = "1";
    button.addEventListener("click", () => {
      const cleanup = leftoverCleanupHandlers.get(button);
      if (cleanup === undefined) return;
      button.disabled = true;
      button.textContent = "Closing…";
      void cleanup().then(
        () => {
          section.hidden = true;
        },
        () => {
          button.disabled = false;
          button.textContent = "Close them";
        },
      );
    });
  }
}
export async function openInstitutionSignIn(origin?: string): Promise<void> {
  const response: unknown = await chrome.runtime.sendMessage({
    type: SESSION_SIGNIN_MESSAGE,
    ...(origin === undefined ? {} : { origin }),
  });
  if (
    typeof response === "object" &&
    response !== null &&
    (response as Record<string, unknown>)["ok"] === true &&
    (response as Record<string, unknown>)["opened"] === true
  ) {
    return;
  }
  const responseError =
    typeof response === "object" && response !== null
      ? (response as Record<string, unknown>)["error"]
      : undefined;
  const reason =
    typeof responseError === "object" &&
    responseError !== null &&
    typeof (responseError as Record<string, unknown>)["message"] === "string"
      ? (responseError as Record<string, unknown>)["message"] as string
      : "Could not open the institution sign-in";
  throw new Error(reason);
}

export async function retryAuthStalled(jobID: string): Promise<void> {
  const response: unknown = await chrome.runtime.sendMessage({
    type: SESSION_RETRY_MESSAGE,
    request: { job_id: jobID },
  });
  if (
    typeof response !== "object" ||
    response === null ||
    (response as Record<string, unknown>)["ok"] !== true
  ) {
    throw new Error("Could not retry this handoff");
  }
}

/** Short relative age: freshness at a glance beats a wall-clock time the
 * reader must subtract from "now" themselves. */
function formatAgo(timestamp: number | null): string {
  if (timestamp === null || !Number.isFinite(timestamp)) return "just now";
  const elapsed = Math.max(0, Date.now() - timestamp);
  const minutes = Math.floor(elapsed / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  return `${Math.floor(minutes / 60)}h ago`;
}

/** Bare host for naming which institution a session (and its sign-in button)
 * belongs to — the full origin URL is protocol noise in a 360px card. */
export function resolverHost(origin: string | null): string {
  if (origin === null) return "";
  try {
    return new URL(origin).host;
  } catch {
    return origin;
  }
}

function sessionEvidenceDetail(state: PopupSessionState): string {
  const source =
    state.probeSource === "live_tab"
      ? "via your library tab"
      : state.probeSource === "keepalive_tab"
        ? "via keepalive tab"
        : "no probe evidence";
  const rawTimestamp = state.lastVerdictAt;
  if (typeof rawTimestamp !== "number" || !Number.isFinite(rawTimestamp)) {
    return source;
  }
  return `${source} · ${formatAgo(rawTimestamp)}`;
}

export interface SessionCardState {
  label: string;
  detail: string;
  action: "none" | "signin";
}

/** Convert a session snapshot into mutually exclusive card copy. In
 * particular, a missing resolver never shares a card with signed-out copy. */
export function deriveSessionCardState(state: PopupSessionState | undefined): SessionCardState {
  if (state === undefined) {
    return { label: "Checking session…", detail: "", action: "none" };
  }
  if (state.resolverOrigin === null) {
    return {
      label: "No resolver configured yet",
      detail: "Open a paper first",
      action: "none",
    };
  }
  const detail = sessionEvidenceDetail(state);
  if (!state.enabled) {
    return {
      label: "Keep-warm off",
      detail,
      action: "signin",
    };
  }
  const checking = state.checking === true;
  const lastCheckAt = state.lastCheckAt;
  const stale =
    lastCheckAt === null ||
    !Number.isFinite(lastCheckAt) ||
    Date.now() - lastCheckAt > SESSION_STALE_MS;
  if (checking) {
    return {
      label: state.likelyAuthenticated === true ? "Likely signed in — verifying" : "Checking session…",
      detail,
      action: "signin",
    };
  }
  if (state.pausedForReauth) {
    return {
      label: "Sign-in needed - papio paused",
      detail,
      action: "signin",
    };
  }
  const verdict = state.verdict ?? (state.authenticated ? "in" : "unknown");
  if (stale) {
    return {
      label: "Checking session…",
      detail,
      action: "signin",
    };
  }
  if (state.scanOutcome === "no_markers") {
    return {
      label: "Signed-in state unclear on this page",
      detail: "papio inspected your library tab but found no sign-in indicators",
      action: "signin",
    };
  }
  if (state.scanOutcome === "scan_failed") {
    return {
      label: "papio couldn't read the library page — check site access in Options",
      detail,
      action: "signin",
    };
  }
  if (verdict === "unknown") {
    return {
      label: "Session unknown — open your library page to verify",
      detail,
      action: "signin",
    };
  }
  const completedVerdict =
    typeof state.lastVerdictAt === "number" && Number.isFinite(state.lastVerdictAt);
  if (verdict === "out" && !state.authenticated && completedVerdict) {
    return {
      label: "Signed out or expired",
      detail,
      action: "signin",
    };
  }
  if (verdict === "in" && state.authenticated) {
    return {
      label: `Session warm · verified ${formatAgo(lastCheckAt)}`,
      detail,
      action: "none",
    };
  }
  return {
    label: "Session unknown — open your library page to verify",
    detail,
    action: "signin",
  };
}

export interface SessionRowState extends SessionCardState {
  origin: string;
}

/** Convert every configured resolver snapshot into independent, pure row copy. */
export function deriveSessionRows(state: PopupSessionState | undefined): SessionRowState[] {
  if (state === undefined || state.origins === undefined) return [];
  return state.origins.map((originState) => {
    const card = deriveSessionCardState({
      ...state,
      ...originState,
      resolverOrigin: originState.origin,
    });
    return { origin: originState.origin, ...card };
  });
}

let sessionNoticeFadeTimer: ReturnType<typeof setTimeout> | undefined;
let sessionNoticeHideTimer: ReturnType<typeof setTimeout> | undefined;
let sessionProbeRetryTimer: ReturnType<typeof setTimeout> | undefined;
/** The release-event stamp the notice last animated for. A cumulative count
 * re-delivered by every session poll must not resurrect a faded notice. */
let sessionNoticeShownKey: string | undefined;

const sessionSignInHandlers = new WeakMap<HTMLButtonElement, () => Promise<void>>();

function clearSessionNoticeTimers(): void {
  clearTimeout(sessionNoticeFadeTimer);
  clearTimeout(sessionNoticeHideTimer);
  sessionNoticeFadeTimer = undefined;
  sessionNoticeHideTimer = undefined;
}
function sessionRowText(row: Pick<SessionCardState, "label" | "detail">): string {
  const labelEndsWithEllipsis = /(?:…|\.{3})$/.test(row.label);
  const hasNoProbeEvidence = row.detail === "no probe evidence" ||
    row.detail.startsWith("no probe evidence · ");
  return row.detail === "" || labelEndsWithEllipsis || hasNoProbeEvidence
    ? row.label
    : `${row.label} · ${row.detail}`;
}

function renderSessionRows(
  doc: Document,
  container: HTMLElement,
  rows: readonly SessionRowState[],
  onSignIn: (origin?: string) => Promise<void>,
): void {
  container.replaceChildren();
  rows.forEach((row, index) => {
    const item = doc.createElement("div");
    item.className = "action-row institution-session-origin-row";
    const copy = doc.createElement("div");
    copy.className = "institution-session-origin-copy";
    const host = doc.createElement("span");
    host.className = "institution-session-origin";
    host.textContent = resolverHost(row.origin);
    const status = doc.createElement("p");
    status.className = "institution-session-status";
    status.id = `institution-session-status-${index}`;
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    status.textContent = sessionRowText(row);
    copy.append(host, status);
    const signIn = doc.createElement("button");
    signIn.className = "primary";
    signIn.type = "button";
    signIn.textContent = "Sign in";
    signIn.setAttribute("aria-label", `Sign in to ${resolverHost(row.origin)}`);
    signIn.setAttribute("aria-describedby", status.id);
    signIn.hidden = row.action === "none";
    signIn.disabled = row.action === "none";
    sessionSignInHandlers.set(signIn, () => onSignIn(row.origin));
    signIn.dataset.origin = row.origin;
    if (!signIn.dataset.wired) {
      signIn.dataset.wired = "1";
      signIn.addEventListener("click", () => {
        const action = sessionSignInHandlers.get(signIn);
        if (action === undefined) return;
        signIn.disabled = true;
        signIn.textContent = "Opening…";
        void action().then(
          () => {
            signIn.disabled = false;
            signIn.textContent = "Sign in";
          },
          (error: unknown) => {
            signIn.disabled = false;
            signIn.textContent = "Sign in";
            status.textContent =
              error instanceof Error && error.message.length > 0
                ? error.message
                : "Could not open the institution sign-in";
          },
        );
      });
    }
    item.append(copy, signIn);
    container.append(item);
  });
}

function scheduleSessionProbeRetry(state: PopupSessionState | undefined): void {
  clearTimeout(sessionProbeRetryTimer);
  sessionProbeRetryTimer = undefined;
  if (state?.checking !== true) return;
  sessionProbeRetryTimer = setTimeout(() => {
    sessionProbeRetryTimer = undefined;
    void requestSessionState().then((next) => {
      if (next !== undefined) renderInstitutionSession(document, next);
    });
  }, 2_000);
  (sessionProbeRetryTimer as unknown as { unref?: () => void }).unref?.();
}


export function renderInstitutionSession(
  doc: Document,
  state: PopupSessionState | undefined,
  onSignIn: (origin?: string) => Promise<void> = openInstitutionSignIn,
): void {
  const card = doc.getElementById("institution-session");
  const status = doc.getElementById("institution-session-status");
  const origin = doc.getElementById("institution-session-origin");
  const signIn = doc.getElementById("institution-session-signin");
  const legacyRow = doc.querySelector<HTMLElement>("#institution-session .institution-session-row");
  const rowsContainer = doc.getElementById("institution-session-rows");
  const waiting = doc.getElementById("institution-session-waiting");
  const notice = doc.getElementById("institution-session-unblocked");
  if (
    !(card instanceof HTMLElement) ||
    !(status instanceof HTMLElement) ||
    !(origin instanceof HTMLElement) ||
    !(signIn instanceof HTMLButtonElement) ||
    !(notice instanceof HTMLElement)
  ) {
    return;
  }
  card.dataset.hasSession = state === undefined ? "false" : "true";
  card.hidden = state === undefined && waiting?.hidden !== false;
  if (state === undefined) {
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = true;
    origin.textContent = "";
    status.textContent = "";
    signIn.hidden = true;
    signIn.disabled = true;
    rowsContainer?.replaceChildren();
    if (rowsContainer instanceof HTMLElement) rowsContainer.hidden = true;
    clearSessionNoticeTimers();
    return;
  }

  const rows = deriveSessionRows(state);
  const multiOrigin = rows.length > 1 && rowsContainer instanceof HTMLElement;
  if (multiOrigin) {
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = true;
    origin.textContent = "";
    rowsContainer.hidden = false;
    renderSessionRows(doc, rowsContainer, rows, onSignIn);
  } else {
    if (rowsContainer instanceof HTMLElement) {
      rowsContainer.hidden = true;
      rowsContainer.replaceChildren();
    }
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = false;
    const singleOrigin = rows.length === 1 ? rows[0]?.origin : undefined;
    const displayState =
      state.resolverOrigin === null && singleOrigin !== undefined && state.origins?.[0] !== undefined
        ? { ...state, ...state.origins[0], resolverOrigin: singleOrigin }
        : state;
    const cardState = deriveSessionCardState(displayState);
    status.textContent = sessionRowText(cardState);
    origin.textContent = resolverHost(displayState.resolverOrigin);
    signIn.disabled = cardState.action === "none";
    signIn.hidden = cardState.action === "none";
    signIn.setAttribute("aria-describedby", status.id);
    sessionSignInHandlers.set(signIn, () => onSignIn(displayState.resolverOrigin ?? undefined));
    if (!signIn.dataset.wired) {
      signIn.dataset.wired = "1";
      signIn.addEventListener("click", () => {
        const action = sessionSignInHandlers.get(signIn);
        if (action === undefined) return;
        signIn.disabled = true;
        signIn.textContent = "Opening…";
        void action().then(
          () => {
            signIn.disabled = false;
            signIn.textContent = "Sign in";
          },
          (error: unknown) => {
            signIn.disabled = false;
            signIn.textContent = "Sign in";
            status.textContent =
              error instanceof Error && error.message.length > 0
                ? error.message
                : "Could not open the institution sign-in";
          },
        );
      });
    }
  }

  const released = Math.max(0, Math.trunc(state.releasedAuthJobs));
  if (released === 0) {
    clearSessionNoticeTimers();
    sessionNoticeShownKey = undefined;
    notice.classList.remove("is-expiring");
    notice.hidden = true;
    return;
  }
  const noticeKey = `${state.releasedAuthJobsAt ?? 0}:${released}`;
  if (noticeKey === sessionNoticeShownKey) return;
  sessionNoticeShownKey = noticeKey;
  const noticeText = `Sign-in unblocked ${released} item${released === 1 ? "" : "s"}`;
  clearSessionNoticeTimers();
  notice.classList.remove("is-expiring");
  notice.hidden = false;
  const message = doc.createElement("span");
  message.textContent = noticeText;
  notice.replaceChildren(message);
  sessionNoticeFadeTimer = setTimeout(() => {
    notice.classList.add("is-expiring");
    sessionNoticeHideTimer = setTimeout(() => {
      notice.hidden = true;
      notice.classList.remove("is-expiring");
    }, 260);
    (sessionNoticeHideTimer as unknown as { unref?: () => void }).unref?.();
  }, 5_000);
  (sessionNoticeFadeTimer as unknown as { unref?: () => void }).unref?.();
}

function handoffPaperLabel(job: ActiveJob): string {
  const title = job.expected?.title?.trim();
  if (title) return title;
  const doi = job.expected?.doi?.trim();
  return doi || job.job_id;
}

function renderWaitingOnSignIn(
  doc: Document,
  jobs: readonly ActiveJob[],
  onFocus: (jobID: string) => Promise<void>,
): void {
  const card = doc.getElementById("institution-session");
  const waiting = doc.getElementById("institution-session-waiting");
  const list = doc.getElementById("institution-session-waiting-list");
  if (
    !(card instanceof HTMLElement) ||
    !(waiting instanceof HTMLElement) ||
    !(list instanceof HTMLElement)
  ) {
    return;
  }

  list.replaceChildren();
  waiting.hidden = jobs.length === 0;
  if (jobs.length === 0) {
    if (card.dataset.hasSession !== "true") card.hidden = true;
    return;
  }
  card.hidden = false;
  if (card.dataset.hasSession !== "true") {
    const legacyRow = card.querySelector<HTMLElement>(".institution-session-row");
    const rows = doc.getElementById("institution-session-rows");
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = true;
    if (rows instanceof HTMLElement) rows.hidden = true;
  }

  for (const job of jobs) {
    const row = doc.createElement("div");
    row.className = "action-row institution-session-waiting-row";
    const paper = doc.createElement("p");
    paper.className = "institution-session-waiting-title";
    paper.textContent = handoffPaperLabel(job);
    const button = doc.createElement("button");
    button.className = "ghost";
    button.type = "button";
    button.textContent = "Focus";
    button.addEventListener("click", () => {
      button.disabled = true;
      button.textContent = "Focusing…";
      void onFocus(job.job_id).then(
        () => {
          button.disabled = false;
          button.textContent = "Focus";
        },
        () => {
          button.disabled = false;
          button.textContent = "Try again";
        },
      );
    });
    row.append(paper, button);
    list.append(row);
  }
}

/** Render the durable browser actions that cannot safely be completed from the
 * service worker: institutional sign-in and granting provider access in Options. */
export function renderNeedsAttention(
  doc: Document,
  jobs: ActiveJob[],
  blockedProviderHosts: readonly string[] = [],
  onFocus: (jobID: string) => Promise<void> = openHandoff,
  onOpenOptions: () => Promise<void> = openOptions,
  authStalledJobs: readonly string[] = [],
  onRetry: (jobID: string) => Promise<void> = retryAuthStalled,
): void {
  const section = doc.getElementById("needs-you-section");
  const heading = doc.getElementById("needs-you-heading");
  const message = doc.getElementById("needs-you-message");
  const list = doc.getElementById("needs-you-list");
  if (
    !(section instanceof HTMLElement) ||
    !(heading instanceof HTMLElement) ||
    !(message instanceof HTMLElement) ||
    !(list instanceof HTMLElement)
  ) {
    return;
  }
  const pending = jobs.filter(
    (job) => job.status === "auth_pending" && job.challenge_blocked !== true,
  );
  renderWaitingOnSignIn(doc, pending, onFocus);
  const challengeJobs = jobs.filter(
    (job) =>
      job.challenge_blocked === true &&
      typeof job.challenge_host === "string" &&
      job.challenge_host.trim().length > 0 &&
      job.tab_id >= 0,
  );
  const blocked = [
    ...new Set(blockedProviderHosts.map((host) => host.trim().toLowerCase()).filter((host) => host.length > 0)),
  ];
  const stalled = [
    ...new Set(authStalledJobs.filter((jobID) => typeof jobID === "string" && jobID.length > 0)),
  ];
  section.hidden = challengeJobs.length === 0 && blocked.length === 0 && stalled.length === 0;
  list.replaceChildren();
  if (section.hidden) return;

  if (challengeJobs.length > 0) {
    heading.textContent = "Security check needs you";
    message.textContent = "Solve it in the open tab — papio resumes automatically.";
  } else if (stalled.length > 0 && blocked.length > 0) {
    heading.textContent = "Needs your attention";
    message.textContent = "Sign in again and allow provider access below.";
  } else if (stalled.length > 0) {
    heading.textContent = "Sign in, then retry";
    message.textContent = "Sign-in didn't stick — retry these papers.";
  } else {
    heading.textContent = "Allow provider access";
    message.textContent = "Enable these sources in Options, or use Grant all sources.";
  }
  message.hidden = message.textContent === "";

  for (const job of challengeJobs) {
    const row = doc.createElement("div");
    row.className = "needs-you-item";
    const copy = doc.createElement("div");
    copy.className = "needs-you-copy";
    const provider = doc.createElement("p");
    provider.className = "needs-you-paper";
    provider.textContent = `Security check needs you - ${job.challenge_host!.trim().toLowerCase()}`;
    const reason = doc.createElement("p");
    reason.className = "needs-you-reason";
    reason.textContent = "Complete it in the open tab; papio will resume without retrying the provider.";
    copy.append(provider, reason);
    const button = doc.createElement("button");
    button.className = "ghost";
    button.type = "button";
    button.textContent = "Go-to-tab";
    button.addEventListener("click", () => {
      button.disabled = true;
      button.textContent = "Opening…";
      void onFocus(job.job_id).then(
        () => {
          button.disabled = false;
          button.textContent = "Go-to-tab";
        },
        () => {
          button.disabled = false;
          button.textContent = "Try again";
        },
      );
    });
    row.append(copy, button);
    list.append(row);
  }


  for (const jobID of stalled) {
    const row = doc.createElement("div");
    row.className = "needs-you-item";
    const copy = doc.createElement("div");
    copy.className = "needs-you-copy";
    const paper = doc.createElement("p");
    paper.className = "needs-you-paper";
    const knownJob = jobs.find((job) => job.job_id === jobID);
    paper.textContent = knownJob === undefined ? jobID : handoffPaperLabel(knownJob);
    const reason = doc.createElement("p");
    reason.textContent = "Sign-in didn't stick - sign in, then retry";
    const button = doc.createElement("button");
    button.className = "ghost";
    button.type = "button";
    button.textContent = "Retry now";
    button.addEventListener("click", () => {
      button.disabled = true;
      button.textContent = "Retrying…";
      void onRetry(jobID).then(
        () => {
          button.disabled = false;
          button.textContent = "Retry now";
        },
        () => {
          button.disabled = false;
          button.textContent = "Try again";
        },
      );
    });
    row.append(copy, button);
    list.append(row);
  }

  for (const host of blocked) {
    const row = doc.createElement("div");
    row.className = "needs-you-item";
    const provider = doc.createElement("p");
    provider.className = "needs-you-paper";
    provider.textContent = host;
    const button = doc.createElement("button");
    button.className = "ghost";
    button.type = "button";
    button.textContent = "Open Options";
    button.addEventListener("click", () => {
      button.disabled = true;
      button.textContent = "Opening…";
      void onOpenOptions().catch(() => {
        button.disabled = false;
        button.textContent = "Try again";
      });
    });
    row.append(provider, button);
    list.append(row);
  }
}

export function wireInboxLauncher(
  doc: Document = document,
  onOpen: () => Promise<void> = openInbox,
): void {
  const button = doc.getElementById("open-inbox-btn");
  const status = doc.getElementById("open-inbox-status");
  if (!(button instanceof HTMLButtonElement) || button.dataset.wired) return;
  button.dataset.wired = "1";
  button.addEventListener("click", () => {
    button.disabled = true;
    if (status) status.textContent = "Opening inbox…";
    void onOpen()
      .then(() => {
        // Chrome dismisses the popup when the new tab takes focus; Firefox
        // keeps it open, so close it explicitly once the inbox is open.
        window.close();
      })
      .catch((error: unknown) => {
        if (status) status.textContent = error instanceof Error ? error.message : "Could not open inbox";
        button.disabled = false;
      });
  });
}

/**
 * The history page ships as dist/history.html, beside the popup (see
 * build.ts). Derived from the manifest's declared popup path — mirroring
 * background.ts's own inboxRuntimeURLs derivation, which authorizes this
 * same URL as a stats sender — rather than hardcoded, so a future page
 * relocation cannot silently desync the two. Computed lazily (not at module
 * load) since chrome.runtime is only available once a real popup document,
 * or a test that mocks it, is in play.
 */
function historyPagePath(): string {
  const declaredPopup = chrome.runtime.getManifest().action?.default_popup ?? "dist/popup.html";
  return declaredPopup.replace(/[^/]*$/, "history.html");
}

/**
 * Fill the "Your papio impact" summary, or hide the whole section when stats
 * are unavailable (daemon offline or too old) — the popup stays a launcher,
 * never an error surface.
 */
export function renderImpactSummary(
  doc: Document,
  stats: Pick<AcquisitionStats, "acquired_total" | "failed_total"> | null,
): void {
  const section = doc.getElementById("impact-summary");
  const acquired = doc.getElementById("impact-acquired");
  const timeSaved = doc.getElementById("impact-time-saved");
  const successRate = doc.getElementById("impact-success-rate");
  if (!section || !acquired || !timeSaved || !successRate) return;
  if (stats === null) {
    section.hidden = true;
    return;
  }
  acquired.textContent = String(stats.acquired_total);
  timeSaved.textContent = formatHoursSaved(stats.acquired_total * EST_MINUTES_SAVED_PER_PAPER);
  successRate.textContent = formatShare(stats.acquired_total, stats.acquired_total + stats.failed_total);
  section.hidden = false;
}

export async function refreshImpactSummary(doc: Document = document): Promise<void> {
  let stats: AcquisitionStats | null;
  try {
    const reply = parseStatsReply(await chrome.runtime.sendMessage({ type: "papio.stats", request: {} }));
    stats = reply.ok ? reply.stats : null;
  } catch {
    stats = null;
  }
  renderImpactSummary(doc, stats);
}

export function wireHistoryLauncher(doc: Document = document): void {
  const button = doc.getElementById("view-history-btn");
  if (!(button instanceof HTMLButtonElement) || button.dataset.wired) return;
  button.dataset.wired = "1";
  button.addEventListener("click", () => {
    void chrome.tabs.create({ url: chrome.runtime.getURL(historyPagePath()) }).then(() => {
      // Chrome dismisses the popup when the new tab takes focus; Firefox
      // keeps it open, so close it explicitly once the tab exists.
      window.close();
    });
  });
}

function responseErrorMessage(response: PageAcquireResponse): string {
  if (typeof response.error === "string" && response.error.length > 0) return response.error;
  if (typeof response.error === "object" && response.error !== null && typeof response.error.message === "string") {
    return response.error.message;
  }
  return "";
}

function pageAcquireStatus(response: PageAcquireResponse): string {
  const error = responseErrorMessage(response);
  if (error) return error;
  if (typeof response.message === "string" && response.message.length > 0) return response.message;
  if (typeof response.job_id === "string" && response.job_id.length > 0) {
    return response.duplicate === true ? `Already queued: ${response.job_id}` : `Queued: ${response.job_id}`;
  }
  return "The daemon did not acknowledge this page.";
}

function deliveryStatusText(delivery: PendingDelivery | undefined): string {
  if (delivery?.status === "failed") return delivery.error || "Could not deliver this PDF";
  if (delivery?.status === "downloaded") return "papio adopted v (validating)";
  if (delivery?.status === "sending") return "Sending PDF to papio…";
  return "";
}

const POPUP_REFRESH_INTERVAL_MS = 5_000;
const STALL_THRESHOLD_MS = 10 * 60_000;

/** Keep the activity feed best-effort: an older daemon may not advertise it,
 * and an asleep worker may reject the request while the page remains useful. */
function isPopupActivityEntry(value: unknown): value is ActivityEntryPayload {
  if (typeof value !== "object" || value === null) return false;
  const entry = value as Record<string, unknown>;
  return (
    typeof entry.seq === "number" &&
    Number.isSafeInteger(entry.seq) &&
    typeof entry.at === "string" &&
    typeof entry.kind === "string" &&
    typeof entry.text === "string" &&
    (entry.job_id === undefined || typeof entry.job_id === "string") &&
    (entry.title === undefined || typeof entry.title === "string")
  );
}

function popupActivityEntries(value: unknown): ActivityEntryPayload[] | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const response = value as Record<string, unknown>;
  if (response.ok !== true || typeof response.feature !== "boolean") return undefined;
  if (response.feature === false) return [];
  return Array.isArray(response.entries) ? response.entries.filter(isPopupActivityEntry) : undefined;
}

async function readPopupActivity(): Promise<ActivityEntryPayload[] | undefined> {
  try {
    const response = await chrome.runtime.sendMessage({
      type: "papio.activity",
      request: { limit: 50 },
    });
    return popupActivityEntries(response);
  } catch {
    return undefined;
  }
}

function relativeAgeParts(timestamp: number, now = Date.now()): { display: string; compact: string; stale: boolean } {
  if (!Number.isFinite(timestamp)) return { display: "just now", compact: "0m", stale: false };
  const elapsed = Math.max(0, now - timestamp);
  const seconds = Math.floor(elapsed / 1_000);
  if (seconds < 45) return { display: "just now", compact: "0m", stale: false };
  if (seconds < 90) return { display: "1m ago", compact: "1m", stale: false };
  if (seconds < 3_600) {
    const minutes = Math.round(seconds / 60);
    return { display: `${minutes}m ago`, compact: `${minutes}m`, stale: elapsed > STALL_THRESHOLD_MS };
  }
  if (seconds < 86_400) {
    const hours = Math.round(seconds / 3_600);
    return { display: `${hours}h ago`, compact: `${hours}h`, stale: elapsed > STALL_THRESHOLD_MS };
  }
  const days = Math.round(seconds / 86_400);
  return { display: `${days}d ago`, compact: `${days}d`, stale: elapsed > STALL_THRESHOLD_MS };
}

function popupActivityForJob(entries: readonly ActivityEntryPayload[], jobID: string): ActivityEntryPayload | undefined {
  let latest: ActivityEntryPayload | undefined;
  for (const entry of entries) {
    if (entry.job_id !== jobID) continue;
    if (
      latest === undefined ||
      entry.seq > latest.seq ||
      (entry.seq === latest.seq && entry.at > latest.at)
    ) {
      latest = entry;
    }
  }
  return latest;
}

function fallbackJobStatus(job: ActiveJob): string {
  switch (String(job.status)) {
    case "offered":
      return "Institution access handoff offered";
    case "queued":
      return "Queued for acquisition";
    case "accepted":
      return "Acquisition accepted";
    case "auth_pending":
      return "Waiting on you to sign in";
    case "awaiting_download":
      return "Waiting for your PDF download";
    case "awaiting_human":
      return "Waiting on you to continue";
    default:
      return "Acquisition is active";
  }
}

function liveStatusText(
  job: ActiveJob,
  activity: ActivityEntryPayload | undefined,
  pendingDelivery: PendingDelivery | undefined,
): { text: string; timestamp: number; stale: boolean } {
  const now = Date.now();
  const activityTimestamp = activity === undefined ? NaN : Date.parse(activity.at);
  const fallbackTimestamp = Number.isFinite(job.offered_at) && job.offered_at > 0 ? job.offered_at : now;
  let text = activity?.text.trim() || fallbackJobStatus(job);
  let timestamp = Number.isFinite(activityTimestamp) ? activityTimestamp : fallbackTimestamp;

  if (pendingDelivery !== undefined) {
    const pendingText =
      pendingDelivery.status === "failed"
        ? pendingDelivery.error?.trim() || "PDF delivery needs attention"
        : pendingDelivery.status === "downloaded"
          ? "PDF received by papio"
          : pendingDelivery.status === "sending"
            ? "Sending PDF to papio"
            : "";
    if (pendingText !== "") {
      const pendingTimestamp = Number.isFinite(pendingDelivery.initiated_at) && pendingDelivery.initiated_at > 0
        ? pendingDelivery.initiated_at
        : now;
      if (!Number.isFinite(activityTimestamp) || pendingTimestamp >= activityTimestamp) {
        text = pendingText;
        timestamp = pendingTimestamp;
      }
    }
  }

  const waitingOnOperator = String(job.status) === "awaiting_human" ||
    /awaiting[_ ]human|waiting on (the )?operator|still waiting on you/i.test(text);
  if (waitingOnOperator && !/waiting on you|waiting for your/i.test(text)) {
    text = `Waiting on you · ${text}`;
  }
  const age = relativeAgeParts(timestamp, now);
  const activityAge = Number.isFinite(activityTimestamp) ? relativeAgeParts(activityTimestamp, now) : undefined;
  const stalled = activityAge?.stale === true;
  const stallAge = activityAge?.compact ?? age.compact;
  return {
    text: `${stalled ? `No progress for ${stallAge} · ` : ""}${text} · ${age.display}`,
    timestamp,
    stale: stalled,
  };
}

export interface PopupLiveActions {
  openInbox?: () => Promise<void>;
  goToTab?: (jobID: string) => Promise<void>;
}

const liveActionHandlers = new WeakMap<HTMLButtonElement, () => Promise<void>>();

function wireLiveAction(
  button: HTMLButtonElement,
  handler: () => Promise<void>,
  label: string,
): void {
  liveActionHandlers.set(button, handler);
  if (button.dataset.wired) return;
  button.dataset.wired = "1";
  button.addEventListener("click", () => {
    const action = liveActionHandlers.get(button);
    if (action === undefined) return;
    button.disabled = true;
    button.textContent = "Opening…";
    void action().then(
      () => {
        button.disabled = false;
        button.textContent = label;
        if (typeof window !== "undefined") window.close();
      },
      (error: unknown) => {
        button.disabled = false;
        button.textContent = "Try again";
        const status = button.closest("#page-acquire")?.querySelector<HTMLElement>(".page-acquire-live-status");
        if (status !== null && status !== undefined) {
          status.textContent = error instanceof Error ? error.message : "Could not open the requested papio page";
        }
      },
    );
  });
}

function renderLiveAcquisition(
  doc: Document,
  job: ActiveJob,
  activityEntries: readonly ActivityEntryPayload[],
  pendingDelivery: PendingDelivery | undefined,
  actions: PopupLiveActions,
): void {
  const card = doc.getElementById("page-acquire-live");
  const title = doc.getElementById("page-acquire-live-title");
  const status = doc.getElementById("page-acquire-live-status");
  const inbox = doc.getElementById("page-acquire-open-inbox");
  const tab = doc.getElementById("page-acquire-go-tab");
  if (
    !(card instanceof HTMLElement) ||
    !(title instanceof HTMLElement) ||
    !(status instanceof HTMLElement) ||
    !(inbox instanceof HTMLButtonElement) ||
    !(tab instanceof HTMLButtonElement)
  ) {
    return;
  }

  const delivery = pendingDelivery?.job_id === job.job_id ? pendingDelivery : undefined;
  const latest = popupActivityForJob(activityEntries, job.job_id);
  const knownTitle = job.expected?.title?.trim() || latest?.title?.trim() || "";
  title.textContent = knownTitle;
  title.hidden = knownTitle === "";
  const live = liveStatusText(job, latest, delivery);
  status.textContent = live.text;
  status.dataset.stalled = live.stale ? "true" : "false";
  card.hidden = false;

  wireLiveAction(inbox, actions.openInbox ?? openInbox, "Open inbox item");
  inbox.dataset.jobId = job.job_id;

  const handoffKind = latest?.kind === "browser.handoff_offered" ||
    latest?.kind === "browser.handoff_reoffered" ||
    latest?.kind === "browser.auth_pending";
  const hasLiveHandoffTab = Number.isFinite(job.tab_id) && job.tab_id >= 0 &&
    (job.requires_auth === true || String(job.status) === "auth_pending" || handoffKind);
  tab.hidden = !hasLiveHandoffTab;
  if (hasLiveHandoffTab) {
    wireLiveAction(tab, () => (actions.goToTab ?? openHandoff)(job.job_id), "Go to tab");
    tab.dataset.jobId = job.job_id;
  } else {
    tab.dataset.jobId = "";
  }
}

export async function acquireCurrentPage(): Promise<PageAcquireResponse> {
  const page = await readCurrentPageMetadata();
  if (typeof page.doi !== "string" || !page.doi) {
    return { error: NO_DOI_FOUND };
  }
  const result: unknown = await chrome.runtime.sendMessage({
    channel: "papio",
    action: "page_acquire",
    payload: {
      url: page.url,
      doi: page.doi,
      ...(page.title ? { title: page.title } : {}),
      source: "popup",
    },
  });
  if (typeof result !== "object" || result === null) {
    throw new Error("The daemon did not acknowledge this page");
  }
  return result as PageAcquireResponse;
}

/** Ask the broker to deliver the current PDF without opening another tab. */
export async function sendCurrentPDF(): Promise<PageAcquireResponse> {
  const page = await readCurrentPageMetadata();
  if (page.kind !== "pdf" && !isPDFPage(page.url)) {
    return { error: "No PDF detected on this page" };
  }
  const result: unknown = await chrome.runtime.sendMessage({

    type: "papio.delivery.start",
    request: {
      tab_id: page.tab_id,
      url: pdfSourceURL(page.url),
      ...(page.doi ? { doi: page.doi } : {}),
      ...(page.title ? { title: page.title } : {}),
    },
  });
  if (typeof result !== "object" || result === null) {
    throw new Error("Could not start PDF delivery");
  }
  return result as PageAcquireResponse;
}

type DeliveryFeedback = PendingDelivery & {
  status: "sending" | "downloaded" | "failed";
};

async function readDeliveryFeedback(fallback: PendingDelivery | undefined): Promise<PendingDelivery | undefined> {
  try {
    const reply: unknown = await chrome.runtime.sendMessage({ type: "papio.delivery.state" });
    if (
      typeof reply === "object" &&
      reply !== null &&
      typeof (reply as Record<string, unknown>)["job_id"] === "string" &&
      ((reply as Record<string, unknown>)["state"] === "sending" ||
        (reply as Record<string, unknown>)["state"] === "downloaded" ||
        (reply as Record<string, unknown>)["state"] === "failed")
    ) {
      const state = (reply as Record<string, unknown>)["state"] as DeliveryFeedback["status"];
      const jobID = (reply as Record<string, unknown>)["job_id"] as string;
      const message = (reply as Record<string, unknown>)["message"];
      const sameJob = fallback?.job_id === jobID ? fallback : undefined;
      return {
        job_id: jobID,
        url: sameJob?.url ?? "",
        initiated_at: sameJob?.initiated_at ?? 0,
        status: state,
        ...(typeof message === "string" ? { error: message } : {}),
      };
    }
  } catch {
    // The storage snapshot remains the fallback when the worker is asleep or
    // an older worker does not know the delivery query.
  }
  return fallback;
}

/** The header pill shows a compact verb; the full label rides title/aria so
 * the DOI and richer state stay one hover away. */
function shortAcquireLabel(label: string): string {
  if (label.startsWith("Acquiring")) return "Acquiring…";
  if (label.startsWith("Acquire this page")) return "Acquire";
  if (label.startsWith("Sending")) return "Sending…";
  if (label.startsWith("Send this PDF")) return "Send PDF";
  if (label.startsWith("PDF sent")) return "Sent";
  if (label.startsWith("Already queued") || label.startsWith("Queued")) return "Queued";
  return label.length > 12 ? `${label.slice(0, 11)}…` : label;
}

function setAcquireButton(
  button: HTMLButtonElement,
  label: string,
  disabled: boolean,
  hidden = false,
): void {
  button.textContent = shortAcquireLabel(label);
  button.title = label;
  button.setAttribute("aria-label", label);
  button.setAttribute("aria-disabled", String(disabled));
  button.disabled = disabled;
  button.hidden = hidden;
}

function showAcquireFeedback(section: HTMLElement, status: HTMLElement, text: string): void {
  status.textContent = text;
  section.hidden = false;
}

/** Render a page-aware acquisition launcher. It remains available while the
 * daemon is down so its established error path stays actionable. */
export function renderPageAcquire(
  doc: Document,
  onAcquire: () => Promise<PageAcquireResponse> = acquireCurrentPage,
  onSendPDF: () => Promise<PageAcquireResponse> = sendCurrentPDF,
): void {
  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn");
  const status = doc.getElementById("page-acquire-status");
  if (
    !(section instanceof HTMLElement) ||
    !(button instanceof HTMLButtonElement) ||
    !(status instanceof HTMLElement)
  ) {
    return;
  }
  if (button.dataset.wired) return;
  button.dataset.wired = "1";
  let queued = false;
  let deliveryPending = false;
  button.addEventListener("click", () => {
    const isPDF = button.dataset.mode === "pdf";
    setAcquireButton(button, isPDF ? "Sending PDF to papio…" : "Acquiring this page…", true);
    showAcquireFeedback(section, status, isPDF ? "Sending PDF to papio…" : "Acquiring…");
    void (isPDF ? onSendPDF() : onAcquire()).then(
      (response) => {
        if (isPDF) {
          const state = response.state;
          deliveryPending = state === "sending" || state === "downloaded";
          const label = deliveryPending
            ? response.duplicate === true
              ? "Sending PDF for the existing job"
              : "PDF sent to papio"
            : "Send this PDF to papio";
          setAcquireButton(button, label, deliveryPending);
          showAcquireFeedback(
            section,
            status,
            state === "sending"
              ? response.duplicate === true
                ? "Sending PDF for the existing job"
                : "Sending PDF to papio…"
              : state === "downloaded"
                ? "papio adopted v (validating)"
                : responseErrorMessage(response) || response.message || "PDF delivery did not start.",
          );
          return;
        }
        queued = typeof response.job_id === "string" && response.job_id.length > 0;
        const label = queued
          ? response.duplicate === true
            ? "Already queued"
            : "Queued"
          : button.dataset.idleLabel ?? "Acquire this page";
        setAcquireButton(button, label, queued);
        showAcquireFeedback(section, status, pageAcquireStatus(response));
      },
      (error: unknown) => {
        queued = false;
        deliveryPending = false;
        setAcquireButton(
          button,
          isPDF ? "Send this PDF to papio" : button.dataset.idleLabel ?? "Acquire this page",
          false,
        );
        showAcquireFeedback(
          section,
          status,
          error instanceof Error
            ? error.message
            : isPDF
              ? "Could not send PDF to papio"
              : "Could not acquire this page",
        );
      },
    ).finally(() => {
      const disabled = isPDF ? deliveryPending : queued;
      button.disabled = disabled;
      button.setAttribute("aria-disabled", String(disabled));
    });
  });
}

function normalizedPDFURL(value: string | undefined): string | undefined {
  if (typeof value !== "string" || value.trim() === "") return undefined;
  try {
    const url = new URL(pdfSourceURL(value));
    url.hash = "";
    return url.href;
  } catch {
    return value.trim();
  }
}

export function renderPageContext(
  doc: Document,
  page: PageMetadata | undefined,
  jobs: ActiveJob[],
  pendingDelivery?: PendingDelivery,
  activityEntries: readonly ActivityEntryPayload[] = [],
  liveActions: PopupLiveActions = {},
): void {
  const section = doc.getElementById("page-acquire");
  const status = doc.getElementById("page-acquire-status");
  const button = doc.getElementById("page-acquire-btn");
  const liveCard = doc.getElementById("page-acquire-live");
  if (
    !(section instanceof HTMLElement) ||
    !(status instanceof HTMLElement) ||
    !(button instanceof HTMLButtonElement)
  ) {
    return;
  }
  if (liveCard instanceof HTMLElement) liveCard.hidden = true;
  section.hidden = true;
  status.textContent = "";
  const kind = page?.kind ?? (page ? classifyPage(page.url, page.doi ? { doi: page.doi } : {}).kind : "none");
  if (kind === "pdf") {
    const knownJob =
      (page?.tab_id === undefined ? undefined : jobs.find((job) => job.tab_id === page.tab_id)) ??
      (page?.doi === undefined
        ? undefined
        : jobs.find((job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === page.doi?.trim().toLowerCase().replace(/^doi:\s*/, "")));
    const currentPDFURL = normalizedPDFURL(page?.url);
    const pendingPDFURL = normalizedPDFURL(pendingDelivery?.url);
    const deliveryMatchesJob = pendingDelivery?.job_id === knownJob?.job_id;
    const deliveryMatchesURL =
      currentPDFURL !== undefined && pendingPDFURL !== undefined && currentPDFURL === pendingPDFURL;
    const delivery = deliveryMatchesJob || deliveryMatchesURL ? pendingDelivery : undefined;
    button.dataset.mode = "pdf";
    button.dataset.idleLabel = "Send this PDF to papio";
    const disabled = delivery?.status === "sending" || delivery?.status === "downloaded";
    setAcquireButton(button, "Send this PDF to papio", disabled);
    if (knownJob !== undefined) {
      renderLiveAcquisition(doc, knownJob, activityEntries, delivery, liveActions);
      section.hidden = false;
    } else {
      const deliveryStatus = deliveryStatusText(delivery);
      if (deliveryStatus !== "") showAcquireFeedback(section, status, deliveryStatus);
    }
    return;
  }
  if (!page?.doi) {
    button.dataset.mode = "doi";
    button.dataset.idleLabel = "Acquire this page";
    setAcquireButton(button, "Acquire this page", true, true);
    return;
  }

  button.dataset.mode = "doi";
  const normalizedDOI = page.doi.trim().toLowerCase().replace(/^doi:\s*/, "");
  const idleLabel = `Acquire this page · ${normalizedDOI}`;
  button.dataset.idleLabel = idleLabel;
  const inFlightJob = jobs.find(
    (job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === normalizedDOI,
  );
  if (inFlightJob === undefined) {
    setAcquireButton(button, idleLabel, false);
    return;
  }

  setAcquireButton(button, `Acquisition in progress · ${normalizedDOI}`, true);
  renderLiveAcquisition(doc, inFlightJob, activityEntries, pendingDelivery, liveActions);
  section.hidden = false;
}

let popupActivity: ActivityEntryPayload[] = [];
let popupRefreshTimer: ReturnType<typeof setInterval> | undefined;

function startPopupRefresh(): void {
  if (popupRefreshTimer !== undefined) return;
  popupRefreshTimer = setInterval(() => {
    void refresh().catch((error: unknown) => {
      console.debug("papio: popup refresh failed", error);
    });
  }, POPUP_REFRESH_INTERVAL_MS);
}

export function wirePrimaryShortcut(doc: Document = document): void {
  if (doc.documentElement.dataset.primaryShortcutWired) return;
  doc.documentElement.dataset.primaryShortcutWired = "1";
  doc.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" || event.defaultPrevented) return;
    const target = event.target;
    if (target instanceof HTMLElement && target.closest("button, input, select, textarea, a")) return;
    const primary = doc.getElementById("page-acquire-btn");
    if (primary instanceof HTMLButtonElement && !primary.disabled) {
      event.preventDefault();
      primary.click();
    }
  });
}


export async function refresh(): Promise<void> {
  // Wave 1: store-derived sections paint immediately (one storage read),
  // before the user can aim at anything.
  const store = await chromeBackend(chrome.storage).load();
  renderDaemonStatus(document, store);
  renderPageAcquire(document);
  // Wave 2: every slow input is gathered in parallel and painted in ONE
  // synchronous pass. Sections revealing one by one over the next seconds
  // shift later cards mid-aim — a live mis-click hit "Focus" where
  // "Close them" had been a moment earlier.
  const [freshActivity, delivery, pageMetadata, session, orphanCount, consent, ungranted] =
    await Promise.all([
      readPopupActivity(),
      readDeliveryFeedback(store.pendingDelivery),
      readCurrentPageMetadata().catch(() => undefined),
      requestSessionState(),
      requestOrphanTabCount(),
      chrome.storage.local.get(TERMS_CONSENT_KEY).then(
        (got) => {
          const v = got[TERMS_CONSENT_KEY];
          return v === "accept" || v === "manual" ? v : undefined;
        },
        () => undefined,
      ),
      (async () => {
        const pending: string[] = [];
        for (const origin of store.resolverOrigins ?? []) {
          try {
            if (!(await chrome.permissions.contains({ origins: [`${origin}/*`] }))) pending.push(origin);
          } catch {
            pending.push(origin);
          }
        }
        return pending;
      })(),
    ]);
  if (freshActivity !== undefined) popupActivity = freshActivity;
  renderPageContext(document, pageMetadata, store.activeJobs, delivery, popupActivity);
  renderInstitutionSession(document, session);
  scheduleSessionProbeRetry(session);
  renderLeftoverTabs(document, orphanCount);
  renderNeedsAttention(
    document,
    store.activeJobs,
    store.blockedProviderHosts,
    openHandoff,
    openOptions,
    session?.stalledAuthJobs ?? [],
    retryAuthStalled,
  );
  renderTermsConsent(document, store.activeJobs, consent, (value) => {
    void sendTermsConsent(value).then(() => refresh());
  });
  renderResolverGrants(document, ungranted, (toGrant) => {
    void chrome.permissions.request({ origins: toGrant.map((origin) => `${origin}/*`) }).then(() => refresh());
  });
}

// Thin adapter over the real chrome surface so captureFixture stays testable
// against a fake. The injected result is normalized to `result?: PageCapture`.
const captureApi: ChromeCaptureApi = {
  tabs: { query: (info) => chrome.tabs.query(info) },
  scripting: {
    executeScript: async (injection): Promise<Array<{ result?: PageCapture | undefined }>> => {
      const results = await chrome.scripting.executeScript(injection);
      return results.map((r) => ({ result: r.result ?? undefined }));
    },
  },
  sendPageCapture: async (payload) => {
    const response: unknown = await chrome.runtime.sendMessage({ type: "papio.page_capture", payload });
    return (
      typeof response === "object" &&
      response !== null &&
      (response as Record<string, unknown>)["captured"] === true
    );
  },
};

export function wireCapture(doc: Document = document): void {
  // The dev-only capture panel needs the element constructors for its
  // instanceof narrowing; a DOM environment without them (some test DOMs)
  // simply has no panel to wire.
  if (typeof HTMLSelectElement === "undefined" || typeof HTMLButtonElement === "undefined") {
    return;
  }
  const providerEl = doc.getElementById("capture-provider");
  const scenarioEl = doc.getElementById("capture-scenario");
  const button = doc.getElementById("capture-btn");
  const statusEl = doc.getElementById("capture-status");
  if (
    !(providerEl instanceof HTMLSelectElement) ||
    !(scenarioEl instanceof HTMLSelectElement) ||
    !(button instanceof HTMLButtonElement) ||
    !statusEl
  ) {
    return;
  }

  // The registry is the single source of truth: a newly registered adapter is
  // capturable without touching popup markup.
  for (const [select, values] of [
    [providerEl, PROVIDERS],
    [scenarioEl, SCENARIOS],
  ] as const) {
    select.replaceChildren(
      ...values.map((value) => {
        const option = doc.createElement("option");
        option.value = value;
        option.textContent = value;
        return option;
      }),
    );
  }

  button.addEventListener("click", () => {
    const provider = providerEl.value as Provider;
    const scenario = scenarioEl.value as Scenario;
    button.disabled = true;
    statusEl.textContent = "Capturing…";
    void captureFixture(captureApi, provider, scenario, () => new Date()).then((result) => {
      statusEl.textContent = result.ok ? `Sent ${result.bytes}-byte capture` : result.error;
      button.disabled = false;
    });
  });
}

export async function openOptions(): Promise<void> {
  const opened = chrome.runtime.openOptionsPage();
  window.close();
  await opened;
}

export function wireSettings(doc: Document = document): void {
  const button = doc.getElementById("settings-btn");
  if (!(button instanceof HTMLButtonElement)) {
    return;
  }
  button.addEventListener("click", () => {
    void openOptions();
  });
}

// Release builds remove this panel from their HTML. The runtime manifest check
// also keeps a packed developer build from exposing fixture capture tools.
export function wireDevTools(
  doc: Document = document,
  manifest?: { update_url?: string | undefined },
): void {
  const section = doc.querySelector<HTMLElement>(".capture");
  // Resolved lazily and defensively: test environments import this module
  // with a partial chrome global, and a default-parameter call would throw
  // at wiring time.
  const resolved =
    manifest ??
    (typeof chrome !== "undefined" && typeof chrome.runtime?.getManifest === "function"
      ? chrome.runtime.getManifest()
      : {});
  if (
    resolved.update_url !== undefined ||
    (typeof __PAPIO_DEV_CAPTURE__ === "boolean" && !__PAPIO_DEV_CAPTURE__)
  ) {
    if (section) section.hidden = true;
    return;
  }
  if (section) section.hidden = false;
  wireCapture(doc);
}

if (typeof document !== "undefined" && typeof chrome !== "undefined") {
  renderPageAcquire(document);
  wireDevTools();
  wireSettings();
  wireInboxLauncher();
  wireHistoryLauncher();
  wirePrimaryShortcut();
  // The initial refresh must not float: a popup opened before storage is
  // reachable (or a test importing this module) would otherwise surface an
  // unhandled rejection. Later refreshes re-render; this one is best-effort.
  refresh().catch((e) => console.debug("papio: initial popup refresh failed", e));
  // Stats are additive: refreshImpactSummary resolves to a hidden section on
  // any failure, so the launcher renders identically with or without a daemon.
  void refreshImpactSummary();
  // A popup can remain open while the daemon advances the duplicate job.
  // Keep the live card honest without making the static launcher poll forever.
  startPopupRefresh();
}
