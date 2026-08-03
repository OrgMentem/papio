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
import { classifyPage, isPDFPage, pdfSourceURL, sniffDOI, type PageKind } from "./deliver";
import type { KeepaliveSnapshot } from "./keepalive";
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
};

export const SESSION_STATE_MESSAGE = "papio.session.state";
export const SESSION_SIGNIN_MESSAGE = "papio.session.signin";
export const SESSION_RETRY_MESSAGE = "papio.session.retry";

function isSessionState(value: unknown): value is PopupSessionState {
  if (typeof value !== "object" || value === null) return false;
  const state = value as Record<string, unknown>;
  const resolverOrigin = state["resolverOrigin"];
  return (
    typeof state["enabled"] === "boolean" &&
    typeof state["intervalMinutes"] === "number" &&
    typeof state["authenticated"] === "boolean" &&
    typeof state["pausedForReauth"] === "boolean" &&
    (state["lastCheckAt"] === null || typeof state["lastCheckAt"] === "number") &&
    (resolverOrigin === null ||
      (typeof resolverOrigin === "string" && /^https:\/\/[^/]+$/.test(resolverOrigin))) &&
    (state["lastAuthReturnedAt"] === null || typeof state["lastAuthReturnedAt"] === "number") &&
    typeof state["queuedAuthJobs"] === "number" &&
    Array.isArray(state["stalledAuthJobs"]) &&
    state["stalledAuthJobs"].every((jobID) => typeof jobID === "string") &&
    typeof state["releasedAuthJobs"] === "number"
  );
}

export async function requestSessionState(): Promise<PopupSessionState | undefined> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({ type: SESSION_STATE_MESSAGE });
    if (typeof response !== "object" || response === null) return undefined;
    const state = (response as Record<string, unknown>)["state"];
    return isSessionState(state) ? state : undefined;
  } catch {
    return undefined;
  }
}

export async function openInstitutionSignIn(): Promise<void> {
  const response: unknown = await chrome.runtime.sendMessage({ type: SESSION_SIGNIN_MESSAGE });
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

function formatLastCheck(lastCheckAt: number | null): string {
  if (lastCheckAt === null || !Number.isFinite(lastCheckAt)) return "just now";
  const elapsed = Math.max(0, Date.now() - lastCheckAt);
  const minutes = Math.floor(elapsed / 60_000);
  if (minutes < 1) return "just now";
  return `${minutes} min ago`;
}

let sessionNoticeFadeTimer: ReturnType<typeof setTimeout> | undefined;
let sessionNoticeHideTimer: ReturnType<typeof setTimeout> | undefined;

function clearSessionNoticeTimers(): void {
  clearTimeout(sessionNoticeFadeTimer);
  clearTimeout(sessionNoticeHideTimer);
  sessionNoticeFadeTimer = undefined;
  sessionNoticeHideTimer = undefined;
}

export function renderInstitutionSession(
  doc: Document,
  state: PopupSessionState | undefined,
  onSignIn: () => Promise<void> = openInstitutionSignIn,
): void {
  const card = doc.getElementById("institution-session");
  const status = doc.getElementById("institution-session-status");
  const origin = doc.getElementById("institution-session-origin");
  const signIn = doc.getElementById("institution-session-signin");
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
  card.hidden = state === undefined;
  if (state === undefined) {
    clearSessionNoticeTimers();
    return;
  }
  status.textContent = !state.enabled
    ? "Keep-warm off"
    : state.pausedForReauth
      ? "Sign-in needed - papio paused"
      : state.authenticated
        ? `Session warm · last verified ${formatLastCheck(state.lastCheckAt)}`
        : "Signed out or expired";
  origin.textContent = state.resolverOrigin
    ? `Applies to ${state.resolverOrigin}`
    : "No resolver configured yet — open a paper first";
  signIn.disabled = state.resolverOrigin === null;
  if (!signIn.dataset.wired) {
    signIn.dataset.wired = "1";
    signIn.addEventListener("click", () => {
      signIn.disabled = true;
      signIn.textContent = "Opening…";
      void onSignIn().then(
        () => {
          signIn.disabled = false;
          signIn.textContent = "Sign in now";
        },
        (error: unknown) => {
          signIn.disabled = state.resolverOrigin === null;
          signIn.textContent = "Sign in now";
          status.textContent =
            error instanceof Error && error.message.length > 0
              ? error.message
              : "Could not open the institution sign-in";
        },
      );
    });
  }
  const released = Math.max(0, Math.trunc(state.releasedAuthJobs));
  if (released === 0) {
    clearSessionNoticeTimers();
    notice.classList.remove("is-expiring");
    notice.hidden = true;
    return;
  }
  const noticeText = `Sign-in unblocked ${released} item${released === 1 ? "" : "s"}`;
  if (!notice.hidden && notice.textContent === noticeText) return;
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
  const pending = jobs.filter((job) => job.status === "auth_pending");
  const blocked = [
    ...new Set(blockedProviderHosts.map((host) => host.trim().toLowerCase()).filter((host) => host.length > 0)),
  ];
  const stalled = [
    ...new Set(authStalledJobs.filter((jobID) => typeof jobID === "string" && jobID.length > 0)),
  ];
  section.hidden = pending.length === 0 && blocked.length === 0 && stalled.length === 0;
  list.replaceChildren();
  if (section.hidden) return;

  if (pending.length > 0 && blocked.length > 0) {
    heading.textContent = "Needs your attention";
    message.textContent = "Finish your institutional sign-in and allow browser access for the listed provider pages.";
  } else if (stalled.length > 0 && pending.length === 0 && blocked.length === 0) {
    heading.textContent = "Sign in, then retry";
    message.textContent = "Sign-in didn't stick - sign in, then retry these papers.";
  } else if (pending.length > 0) {
    heading.textContent = "Sign in to continue";
    message.textContent = "Finish your institutional sign-in to continue these papers.";
  } else {
    heading.textContent = "Allow provider access";
    message.textContent = "Papio cannot read the listed provider pages. Open Options and enable a source, or use Grant all sources.";
  }

  for (const job of pending) {
    const row = doc.createElement("div");
    row.className = "needs-you-item";
    const paper = doc.createElement("p");
    paper.className = "needs-you-paper";
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

function shortJobID(jobID: string): string {
  return jobID.length > 12 ? jobID.slice(0, 12) : jobID;
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
      return {
        job_id: jobID,
        url: fallback?.url ?? "",
        initiated_at: fallback?.initiated_at ?? 0,
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

/** Render a page-aware acquisition launcher. It remains available while the
 * daemon is down so its established error path stays actionable. */
export function renderPageAcquire(
  doc: Document,
  onAcquire: () => Promise<PageAcquireResponse> = acquireCurrentPage,
  onSendPDF: () => Promise<PageAcquireResponse> = sendCurrentPDF,
): void {
  const button = doc.getElementById("page-acquire-btn");
  const status = doc.getElementById("page-acquire-status");
  if (!(button instanceof HTMLButtonElement) || !status) return;
  if (button.dataset.wired) return;
  button.dataset.wired = "1";
  let noDOIFound = false;
  let queued = false;
  let deliveryPending = false;
  button.addEventListener("click", () => {
    const isPDF = button.dataset.mode === "pdf";
    button.disabled = true;
    button.textContent = isPDF ? "Sending…" : "Acquiring…";
    status.textContent = isPDF ? "Sending PDF to papio…" : "Acquiring…";
    void (isPDF ? onSendPDF() : onAcquire()).then(
      (response) => {
        if (isPDF) {
          const state = response.state;
          deliveryPending = state === "sending" || state === "downloaded";
          button.textContent = deliveryPending
            ? response.duplicate === true
              ? "Send PDF for the existing job"
              : "Sent to papio"
            : "Send PDF to papio";
          status.textContent =
            state === "sending"
              ? response.duplicate === true
                ? "Sending PDF for the existing job"
                : "Sending PDF to papio…"
              : state === "downloaded"
                ? "papio adopted v (validating)"
                : responseErrorMessage(response) || response.message || "PDF delivery did not start.";
          return;
        }
        noDOIFound = responseErrorMessage(response) === NO_DOI_FOUND;
        queued = typeof response.job_id === "string" && response.job_id.length > 0;
        button.textContent = queued
          ? response.duplicate === true
            ? "Already queued"
            : "Queued"
          : "Acquire this page";
        status.textContent = pageAcquireStatus(response);
      },
      (error: unknown) => {
        queued = false;
        deliveryPending = false;
        button.textContent = isPDF ? "Send PDF to papio" : "Acquire this page";
        status.textContent = error instanceof Error ? error.message : isPDF ? "Could not send PDF to papio" : "Could not acquire this page";
      },
    ).finally(() => {
      button.disabled = isPDF ? deliveryPending : noDOIFound || queued;
    });
  });
}

export function renderPageContext(
  doc: Document,
  page: PageMetadata | undefined,
  jobs: ActiveJob[],
  pendingDelivery?: PendingDelivery,
): void {
  const section = doc.getElementById("page-acquire");
  const detected = doc.getElementById("page-acquire-doi");
  const state = doc.getElementById("page-acquire-context");
  const status = doc.getElementById("page-acquire-status");
  const button = doc.getElementById("page-acquire-btn");
  if (
    !(section instanceof HTMLElement) ||
    !detected ||
    !state ||
    !status ||
    !(button instanceof HTMLButtonElement)
  ) {
    return;
  }
  section.hidden = false;
  const kind = page?.kind ?? (page ? classifyPage(page.url, page.doi ? { doi: page.doi } : {}).kind : "none");
  if (kind === "pdf") {
    const knownJob =
      (page?.tab_id === undefined ? undefined : jobs.find((job) => job.tab_id === page.tab_id)) ??
      (page?.doi === undefined
        ? undefined
        : jobs.find((job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === page.doi?.trim().toLowerCase().replace(/^doi:\s*/, "")));
    const delivery = pendingDelivery?.status === "failed" || pendingDelivery?.job_id === knownJob?.job_id
      ? pendingDelivery
      : undefined;
    detected.textContent = "";
    detected.hidden = true;
    state.textContent = "";
    status.textContent = deliveryStatusText(delivery);
    button.dataset.mode = "pdf";
    button.hidden = false;
    button.textContent = `Send PDF to papio${knownJob ? ` (job ${shortJobID(knownJob.job_id)})` : ""}`;
    button.disabled = delivery?.status === "sending" || delivery?.status === "downloaded";
    return;
  }
  if (!page?.doi) {
    detected.textContent = "No paper detected on this page";
    detected.hidden = false;
    state.textContent = "";
    status.textContent = "";
    button.dataset.mode = "doi";
    button.textContent = "Acquire this page";
    button.disabled = true;
    button.hidden = true;
    return;
  }
  detected.textContent = "";
  detected.hidden = true;
  status.textContent = "";
  button.dataset.mode = "doi";
  button.hidden = false;
  const normalizedDOI = page.doi.trim().toLowerCase().replace(/^doi:\s*/, "");
  const inFlight = jobs.some(
    (job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === normalizedDOI,
  );
  state.textContent = inFlight ? "An acquisition for this DOI is already in progress." : "";
  button.textContent = inFlight ? "Acquisition in progress" : "Acquire this page";
  button.disabled = inFlight;
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
  const store = await chromeBackend(chrome.storage).load();
  renderDaemonStatus(document, store);
  renderPageAcquire(document);
  const delivery = await readDeliveryFeedback(store.pendingDelivery);
  try {
    renderPageContext(document, await readCurrentPageMetadata(), store.activeJobs, delivery);
  } catch {
    renderPageContext(document, undefined, store.activeJobs, delivery);
  }
  const session = await requestSessionState();
  renderInstitutionSession(document, session);
  renderNeedsAttention(
    document,
    store.activeJobs,
    store.blockedProviderHosts,
    openHandoff,
    openOptions,
    session?.stalledAuthJobs ?? [],
    retryAuthStalled,
  );
  let consent: "accept" | "manual" | undefined;
  try {
    const got = await chrome.storage.local.get(TERMS_CONSENT_KEY);
    const v = got[TERMS_CONSENT_KEY];
    consent = v === "accept" || v === "manual" ? v : undefined;
  } catch {
    consent = undefined;
  }
  renderTermsConsent(document, store.activeJobs, consent, (value) => {
    void sendTermsConsent(value).then(() => refresh());
  });
  const ungranted: string[] = [];
  for (const origin of store.resolverOrigins ?? []) {
    try {
      if (!(await chrome.permissions.contains({ origins: [`${origin}/*`] }))) ungranted.push(origin);
    } catch {
      ungranted.push(origin);
    }
  }
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

// The build removes this panel from shipped HTML; keep the bundle fail-closed
// if extension assets from different builds are briefly mixed during an update.
export function wireDevTools(doc: Document = document): void {
  if (typeof __PAPIO_DEV_CAPTURE__ === "boolean" && !__PAPIO_DEV_CAPTURE__) return;
  const section = doc.querySelector<HTMLElement>(".capture");
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
}
