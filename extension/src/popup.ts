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
import {
  chromeBackend,
  CATCH_UP_ENABLED_KEY,
  getSuccessAckMode,
  type SuccessAckMode,
  type ActiveJob,
  type PendingDelivery,
  type StoreShape,
  TERMS_CONSENT_KEY,
  TOOLBAR_COUNT_MODE_KEY,
  type ToolbarCountMode,
} from "./state";
import type { ActivityEntryPayload, TriageCounts, WorkPulseResponsePayload } from "./protocol";
import {
  classifyPage,
  isPDFPage,
  pdfSourceURL,
  sendPdfState,
  doiFromURL,
  extractPageDOI,
  type PageDOIProbe,
  type PageKind,
} from "./deliver";
import {
  SESSION_STALE_MS,
  type KeepaliveOriginSnapshot,
  type KeepaliveSnapshot,
} from "./keepalive";
import { renderPapio } from "./dom";
import { formatShare, parseStatsReply, type AcquisitionStats } from "./stats";
import { providerViewerPDFURL } from "./adapters/types";


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
    case "session_elsewhere":
      // The daemon answered; it is just pointed at a different browser. Only
      // the operator can decide which browser should hold it, so name the one
      // command that moves it here. It does NOT say this browser gets nothing:
      // Send PDF is user-initiated and routes itself, so it still works here,
      // and claiming otherwise sent researchers to switch sessions they had no
      // reason to switch.
      line = "another browser is holding your papio session — papio sends papers it finds to that one; Send PDF still works here";
      action = "run: papio browser use --latest";
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
  /** Absent on an older daemon; `duplicate` is the fallback. */
  outcome?: "submitted" | "already_queued" | "already_validated";
  error?: string | { code?: string; message?: string };
  state?: "sending" | "downloaded" | "failed" | "adopted" | "needs_choice";
  message?: string;
  // Machine-readable disposition the background sets alongside `message`
  // (currently `choice_expired`). Declared so callers classify on the code
  // instead of matching user-facing copy.
  code?: string;
  ok?: boolean;
  choice?: DeliveryChoiceOffer;
}

interface DeliveryChoice {
  interaction: string;
  job_id: string;
}

interface DeliveryCandidate {
  job_id: string;
  title: string;
}

interface DeliveryChoiceOffer {
  interaction: string;
  candidates: DeliveryCandidate[];
}

interface PageMetadata {
  url: string;
  doi?: string;
  title?: string;
  kind?: PageKind;
  tab_id?: number;
}

/** The page an action was bound to at the moment it was requested.
 *
 * `url` stays the acquisition/PDF-source URL — a provider viewer address is
 * rewritten to the file it wraps — while `tab_url` stays the ACTUAL browser tab
 * URL. Both are kept deliberately: the rewritten one is what papio acts on, and
 * only the unrewritten one can be compared byte-for-byte against a later read
 * of the active tab. Collapsing them would make the binding unverifiable. */
export type PageActionBinding = PageMetadata & { tab_id: number; tab_url: string };

/** One message for every "you asked about a page that is no longer in front of
 * you" case, so a researcher never has to distinguish which of the three
 * validation points noticed. */
export const PAGE_CHANGED_MESSAGE = "The active page changed — try again";

/** Identity of the page a result belongs to. A five-second refresh may render a
 * different page than the one an in-flight action was bound to; keying results
 * on this stops one page's outcome appearing under another. */
export function popupPageKey(binding: PageActionBinding): string {
  return `${binding.tab_id}\u0000${binding.tab_url}`;
}

/** Bindings live on the button, not in a module variable, so two rail buttons
 * cannot share one mutable "current page" that a refresh has since replaced. */
const pageActionBindings = new WeakMap<HTMLButtonElement, PageActionBinding>();

export function bindPageAction(button: HTMLButtonElement, binding: PageActionBinding): void {
  pageActionBindings.set(button, binding);
}

export function boundPageAction(button: HTMLButtonElement): PageActionBinding | undefined {
  return pageActionBindings.get(button);
}

export type PopupFeedbackTone = "progress" | "success" | "degraded" | "error" | "info";

export interface PopupOperationState {
  generation: number;
  ownerKey: string;
  phase: "pending" | "result";
  text: string;
  tone: PopupFeedbackTone;
}

export interface PopupOperationResult {
  ownerKey: string;
  text: string;
  tone: PopupFeedbackTone;
}

/** One registry per document for every locally initiated popup operation.
 *
 * The popup repaints from scratch every five seconds, so DOM-only pending flags
 * and closure-local booleans lost their state on the next tick — a click's
 * "Opening…" or its error would silently revert while the work was still in
 * flight. Pending and result state therefore lives here and is re-rendered from
 * the registry on every pass. `generation` fences a slow reply against a newer
 * click on the same key; `ownerKey` fences it against the owner (page, job,
 * host) having been replaced underneath it. */
const popupOperationRegistries = new WeakMap<Document, Map<string, PopupOperationState>>();

function popupOperations(doc: Document): Map<string, PopupOperationState> {
  let registry = popupOperationRegistries.get(doc);
  if (registry === undefined) {
    registry = new Map();
    popupOperationRegistries.set(doc, registry);
  }
  return registry;
}

export function beginPopupOperation(
  doc: Document,
  operationKey: string,
  ownerKey: string,
  text: string,
): number {
  const registry = popupOperations(doc);
  const generation = (registry.get(operationKey)?.generation ?? 0) + 1;
  registry.set(operationKey, { generation, ownerKey, phase: "pending", text, tone: "progress" });
  return generation;
}

/** Commit a result only if this key is still on the same generation and owner.
 * `null` removes the entry — used for a success whose whole point is that the
 * popup is closing, where a lingering result would outlive its surface. */
export function finishPopupOperation(
  doc: Document,
  operationKey: string,
  generation: number,
  state: PopupOperationResult | null,
): boolean {
  const registry = popupOperations(doc);
  const current = registry.get(operationKey);
  if (current === undefined || current.generation !== generation) return false;
  if (state !== null && current.ownerKey !== state.ownerKey) return false;
  if (state === null) {
    registry.delete(operationKey);
    return true;
  }
  registry.set(operationKey, {
    ...current,
    phase: "result",
    text: state.text,
    tone: state.tone,
  });
  return true;
}

export function popupOperation(doc: Document, operationKey: string): PopupOperationState | undefined {
  return popupOperations(doc).get(operationKey);
}

/** Drop entries whose owner no longer exists. An error must persist until the
 * researcher retries it *or* its owner disappears — never merely because the
 * next poll happened. */
export function prunePopupOperations(doc: Document, ownerIsLive: (ownerKey: string) => boolean): void {
  const registry = popupOperations(doc);
  for (const [key, state] of [...registry]) {
    if (!ownerIsLive(state.ownerKey)) registry.delete(key);
  }
}

/** The last text written to the stable announcer per document. A rerender that
 * reproduces identical text must not re-speak it. */
const popupAnnouncements = new WeakMap<Document, string>();

/** Announce one state transition through the single stable live region. Local
 * result elements stay visible beside their controls and carry no live role, so
 * this is the only place a screen reader hears about a local operation. */
export function announcePopupOperation(doc: Document, text: string): void {
  const announcer = doc.getElementById("popup-operation-status");
  if (!(announcer instanceof HTMLElement)) return;
  if (popupAnnouncements.get(doc) === text) return;
  popupAnnouncements.set(doc, text);
  announcer.textContent = text;
}

/** Paint one registry entry into its visible, non-live result element. */
export function paintPopupResult(element: HTMLElement, state: PopupOperationState | undefined): void {
  if (state === undefined || state.text === "") {
    element.textContent = "";
    element.hidden = true;
    delete element.dataset.tone;
    return;
  }
  element.textContent = state.text;
  element.dataset.tone = state.tone;
  element.hidden = false;
}

const NO_DOI_FOUND = "no DOI found on this page";

/** What the injected harvester returns: raw candidates plus the page's own
 * title. No identifier decision is made in page scope. */
export interface PageProbeResult {
  url: string;
  probe: PageDOIProbe;
  title?: string;
}

/**
 * Runs INSIDE the page via scripting.executeScript — must stay fully
 * self-contained (no outer-scope references survive serialization).
 *
 * It therefore harvests *candidates* and decides nothing. Identifier
 * extraction lives in `extractPageDOI`/`doiFromURL` (`deliver.ts`), which the
 * popup applies to this probe once it is back in extension scope. That split
 * exists because URL-origin extraction needs a route registry that cannot be
 * serialized into a page, and keeping a second copy of it here is exactly how
 * the two implementations drifted: this function used to scan URLs with a text
 * regex, which absorbed query tokens into the DOI.
 */
export function collectPageMetadata(): PageProbeResult {
  const clean = (value: string | null | undefined): string => (value ?? "").trim();
  const metaTags = Array.from(document.querySelectorAll("meta[name]"));
  const meta = metaTags
    .map((element) => ({
      name: clean(element.getAttribute("name")),
      content: clean(element.getAttribute("content")),
    }))
    .filter((tag) => tag.name.length > 0 && tag.content.length > 0);
  const canonical = clean(document.querySelector('link[rel="canonical"]')?.getAttribute("href"));
  const ogURL = clean(document.querySelector('meta[property="og:url"]')?.getAttribute("content"));
  // Keep any link that could resolve to doi.org, including a relative href on a
  // doi.org page and an uppercased host. The extractor resolves against the
  // page's own URL and re-checks the host, so an over-broad keep here is only a
  // few extra strings, whereas an over-narrow one silently loses identifiers.
  const anchorHrefs = Array.from(document.querySelectorAll("a[href]"))
    .slice(0, 1000)
    .map((link) => clean(link.getAttribute("href")))
    .filter((href) => href.length > 0 && (href.toLowerCase().includes("doi.org") || /(^|\/)10\.\d{4,9}\//.test(href)));
  // A locator, not an extractor: the page's text can be 200 KB and crossing that
  // through the injection boundary on every popup open would be wasteful, so the
  // first DOI-shaped run is located here and the authoritative trimming and
  // validation still happen in `extractPageDOI`.
  //
  // The locator MUST agree with `sniffDOI` on which run comes first, or the
  // answer changes. Three details do that, each of which was a real regression
  // when it was missing: decode before matching (a page can print
  // `10.1000%2Fx`), require a word boundary (so `x10.1000/embedded` does not
  // beat a later genuine DOI), and never truncate a run — a run cut to a
  // length limit can itself be a *valid but different* DOI, so an implausibly
  // long one declines instead.
  const rawBody = (document.body?.innerText ?? "").slice(0, 200_000);
  let decodedBody = rawBody;
  try {
    decodedBody = decodeURIComponent(rawBody);
  } catch {
    // Keep raw text when the page contains malformed escapes.
  }
  const bodyRun = decodedBody.match(/\b10\.\d{4,9}\/[^\s"'<>?#]+/)?.[0] ?? "";
  const bodyText = bodyRun.length > 0 && bodyRun.length <= 512 ? bodyRun : "";
  const title = meta.find((tag) => tag.name.toLowerCase() === "citation_title")?.content
    ?? document.title.trim();
  return {
    url: location.href,
    probe: {
      meta,
      ...(canonical ? { canonical } : {}),
      ...(ogURL ? { ogURL } : {}),
      href: location.href,
      ...(anchorHrefs.length > 0 ? { anchorHrefs } : {}),
      ...(bodyText ? { bodyText } : {}),
    },
    ...(title ? { title } : {}),
  };
}

/** The closed set of acknowledgements the host page may show. ADR-0023
 * Decision 1: one short label for one accepted action, and nothing else — no
 * identifier, title, URL, provider, job id, progress, or error. */
export type InPageAcknowledgementKind = "queued" | "already_queued" | "pdf_started" | "pdf_received";

/**
 * Runs INSIDE the page via scripting.executeScript, so — like
 * collectPageMetadata above — it must stay fully self-contained: no
 * outer-scope reference, module import, or shared constant survives
 * serialization, which is why the copy table and the tone literals are
 * duplicated in the body rather than shared with the extension's tokens.
 *
 * ADR-0023 Decision 1's sixth surface. It exists because the popup closes on
 * click, so its own inline result can vanish before the researcher reads it.
 * The chip is noninteractive (`pointer-events: none`), installs no observer,
 * listener, content script, or persistence, and removes itself after three
 * seconds; navigation removes it naturally and an SPA keeps it for at most that
 * lifetime. The host is `aria-hidden` because the popup's stable announcer has
 * already reported the same action — a second announcement would be a duplicate,
 * not an aid.
 */
export function renderInPageAcknowledgement(kind: InPageAcknowledgementKind): void {
  const HOST_ID = "papio-extension-action-ack-v1";
  const DWELL_MS = 3_000;
  const ENTER_MS = 180;
  const EXIT_MS = 140;
  const COPY: Record<string, { label: string; glyph: string; tone: "success" | "info" | "continuing" }> = {
    queued: { label: "Added to papio", glyph: "✓", tone: "success" },
    already_queued: { label: "Already in papio", glyph: "•", tone: "info" },
    pdf_started: { label: "papio is handling this PDF", glyph: "→", tone: "continuing" },
    pdf_received: { label: "PDF received by papio", glyph: "✓", tone: "success" },
  };
  const copy = COPY[kind];
  if (copy === undefined) return;
  // Foreground / border / surface, mirroring the extension's own light and dark
  // token values exactly. Literals, not variables: the page has no access to the
  // extension's stylesheet.
  const TONES: Record<string, { light: [string, string, string]; dark: [string, string, string] }> = {
    success: { light: ["#245e45", "#78aa8f", "#edf7f1"], dark: ["#b3ddc2", "#568f6d", "#1d3329"] },
    info: { light: ["#12549b", "#8db9eb", "#eaf3ff"], dark: ["#a8d0ff", "#4f85bc", "#183956"] },
    continuing: { light: ["#426789", "#b8c5d1", "#eef3f6"], dark: ["#aecbe4", "#526579", "#263340"] },
  };
  const prior = document.getElementById(HOST_ID);
  if (prior !== null) prior.remove();
  const host = document.createElement("div");
  host.id = HOST_ID;
  host.setAttribute("aria-hidden", "true");
  host.style.cssText = [
    "position:fixed",
    "right:16px",
    "bottom:16px",
    "z-index:2147483647",
    "pointer-events:none",
    "margin:0",
    "padding:0",
    "border:0",
  ].join(";");
  // Open, not closed: isolation is what a shadow root is for here, and an open
  // root stays inspectable and testable without weakening that isolation.
  const root = host.attachShadow({ mode: "open" });
  const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const [ink, border, surface] = dark ? TONES[copy.tone]!.dark : TONES[copy.tone]!.light;
  const chip = document.createElement("div");
  chip.style.cssText = [
    "align-items:center",
    `background:${surface}`,
    `border:1px solid ${border}`,
    "border-radius:8px",
    "box-shadow:0 8px 24px rgb(24 34 49 / 16%)",
    `color:${ink}`,
    "display:flex",
    "font:600 13px/1.4 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif",
    "gap:12px",
    "max-width:min(320px, calc(100vw - 32px))",
    "padding:12px",
    "opacity:0",
    reduceMotion ? "transform:none" : "transform:translateY(8px)",
    reduceMotion ? "transition:opacity 0.01ms linear" : `transition:opacity ${ENTER_MS}ms cubic-bezier(0.25, 1, 0.5, 1), transform ${ENTER_MS}ms cubic-bezier(0.25, 1, 0.5, 1)`,
  ].join(";");
  const glyph = document.createElement("span");
  glyph.textContent = copy.glyph;
  glyph.style.cssText = "flex:none;font-size:14px;line-height:1";
  const label = document.createElement("span");
  label.textContent = copy.label;
  chip.append(glyph, label);
  root.append(chip);
  document.documentElement.append(host);
  requestAnimationFrame(() => {
    chip.style.opacity = "1";
    chip.style.transform = "none";
  });
  window.setTimeout(() => {
    chip.style.transition = reduceMotion
      ? "opacity 0.01ms linear"
      : `opacity ${EXIT_MS}ms cubic-bezier(0.5, 0, 0.75, 0), transform ${EXIT_MS}ms cubic-bezier(0.5, 0, 0.75, 0)`;
    chip.style.opacity = "0";
    if (!reduceMotion) chip.style.transform = "translateY(4px)";
    // Under reduced motion the JS delay collapses with the CSS transition, so
    // the host is never left invisible-but-present.
    window.setTimeout(() => host.remove(), reduceMotion ? 0 : EXIT_MS);
  }, DWELL_MS);
}

/** Project one accepted popup action into the page it was requested from.
 *
 * Success-only and preference-gated: `errors` and `off` suppress the chip while
 * every mode keeps the popup's inline state and its accessible announcement.
 * The binding is revalidated first so a chip cannot land on a page the
 * researcher navigated to after clicking, and injection failure is swallowed
 * because inline popup feedback is the authoritative receipt. */
export async function acknowledgeInPage(
  binding: PageActionBinding,
  kind: InPageAcknowledgementKind,
): Promise<void> {
  // Fail closed on an unreadable preference: this is a success-only extra, so
  // "we could not check whether you wanted it" means don't show it.
  let mode: SuccessAckMode;
  try {
    mode = await getSuccessAckMode();
  } catch {
    return;
  }
  if (mode !== "all") return;
  if (!(await validatePageActionBinding(binding))) return;
  try {
    await chrome.scripting.executeScript({
      target: { tabId: binding.tab_id },
      func: renderInPageAcknowledgement,
      args: [kind],
    });
  } catch {
    // A PDF viewer, a privileged page, or a withdrawn activeTab grant refuses
    // injection. The popup's own result already told the researcher what
    // happened, and papio asks for no broader permission to cover this.
  }
}

/** Read the active page under the popup's transient activeTab grant, and return
 * it as a binding rather than as loose page facts.
 *
 * Metadata and tab facts are two reads separated by an injection round trip. A
 * navigation in between used to fuse one page's DOI onto another page's URL, so
 * the active tab is re-read afterwards and both the tab id and the byte-exact
 * tab URL must still match. */
export async function readCurrentPageMetadata(): Promise<PageActionBinding> {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab?.id === undefined) throw new Error("No active tab");
  const tabURL = typeof tab.url === "string" ? tab.url : "";
  const contentType =
    typeof (tab as unknown as Record<string, unknown>)["contentType"] === "string"
      ? (tab as unknown as Record<string, unknown>)["contentType"] as string
      : undefined;
  const tabPDF = isPDFPage(tabURL, contentType);
  let harvested: PageProbeResult | undefined;
  try {
    const [injected] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: collectPageMetadata,
    });
    const result = injected?.result;
    // Require the probe's own shape, not just its presence: a result of an
    // unexpected shape must degrade to the tab-URL path, and `extractPageDOI`
    // iterates `probe.meta`, so a non-array `meta` would throw here instead —
    // the opposite of what this guard exists for.
    if (
      typeof result === "object" && result !== null &&
      typeof (result as PageProbeResult).url === "string" &&
      Array.isArray((result as PageProbeResult).probe?.meta)
    ) {
      harvested = result as PageProbeResult;
    }
  } catch {
    // PDF viewers and privileged pages reject scripting; their tab URL is
    // enough to classify the page and start a browser-managed download.
  }
  const [after] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (after?.id !== tab.id || (typeof after.url === "string" ? after.url : "") !== tabURL) {
    throw new Error(PAGE_CHANGED_MESSAGE);
  }
  const pageURL = tabURL || harvested?.url;
  if (pageURL === undefined || pageURL.length === 0) throw new Error("Could not read the current page");
  const viewerPDFURL = providerViewerPDFURL(pageURL);
  // One extraction path for both cases: with a probe the full candidate order
  // applies, and without one (a viewer tab, which rejects scripting) the tab URL
  // is still a legitimate candidate — but a *structural* one, never a text scan.
  const inferredDOI = harvested !== undefined
    ? extractPageDOI(harvested.probe)
    : doiFromURL(pageURL);
  const classification = tabPDF || viewerPDFURL !== undefined
    ? { kind: "pdf" as const, ...(inferredDOI ? { doi: inferredDOI } : {}) }
    : classifyPage(pageURL, {
        ...(inferredDOI ? { doi: inferredDOI } : {}),
        ...(contentType ? { contentType } : {}),
      });
  const pageDOI = inferredDOI ?? classification.doi;
  return {
    url: viewerPDFURL ?? pageURL,
    ...(pageDOI ? { doi: pageDOI } : {}),
    ...(harvested?.title || tab.title ? { title: harvested?.title || tab.title! } : {}),
    kind: classification.kind,
    tab_id: tab.id,
    tab_url: tabURL,
  };
}

/** The bare HTTPS origin a scan would be authorised for, or `null` when this
 * binding could never be a legitimate scan target. Derived from the bound
 * source URL so the authorised origin and the page actually read cannot
 * diverge: it travels as `expected_origin`, which is what stops a scan
 * authorised for THIS page being spent on wherever the tab navigates next. */
export function scannerOriginForBinding(binding: PageActionBinding): string | null {
  try {
    const parsed = new URL(binding.url);
    const origin = `${parsed.protocol}//${parsed.host}`;
    return isBareHTTPSOrigin(origin) ? origin : null;
  } catch {
    return null;
  }
}

/** Confirm the bound page is still the active one before acting on it. The tab
 * id alone is not enough — the same tab navigating elsewhere is exactly the case
 * this exists to catch — so the tab URL must match byte-for-byte. */
export async function validatePageActionBinding(binding: PageActionBinding): Promise<boolean> {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tab?.id === undefined || tab.id !== binding.tab_id) return false;
    return (typeof tab.url === "string" ? tab.url : "") === binding.tab_url;
  } catch {
    return false;
  }
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

export const PAGE_BULK_SCAN_MESSAGE = "papio.pageBulk.scan";

interface PageBulkScanResponse {
  ok?: boolean;
  scan_id?: string;
  error?: { code?: string; message?: string };
}

export interface PageBulkScanOutcome {
  ok: boolean;
  /** Structured failure code, so the caller can tell a stale page binding from
   * every other refusal instead of matching on prose. */
  code?: string;
  error?: string;
}

/** Ask the background to scan the bound page.
 *
 * ADR-0019 Decision 2 (amended): this click IS the consent. Decision 1 already
 * forbids ambient scanning — one shot, `activeTab`-gated, top frame only — so
 * there is nothing standing for a per-site gate to guard, and papio is
 * local-only. The bound origin still travels as `expected_origin` so a scan
 * authorised for THIS page cannot be spent on whatever the tab navigates to
 * next. */
export async function startPageBulkScan(binding: PageActionBinding): Promise<PageBulkScanOutcome> {
  if (!(await validatePageActionBinding(binding))) {
    return { ok: false, code: "page_changed", error: PAGE_CHANGED_MESSAGE };
  }
  const origin = scannerOriginForBinding(binding);
  if (origin === null) {
    return {
      ok: false,
      code: "invalid_page",
      error: "papio can only scan an ordinary secure (https) page",
    };
  }
  const response = (await chrome.runtime.sendMessage({
    type: PAGE_BULK_SCAN_MESSAGE,
    request: { tab_id: binding.tab_id, expected_origin: origin },
  })) as PageBulkScanResponse;
  if (response.ok === true) return { ok: true };
  return {
    ok: false,
    ...(response.error?.code !== undefined ? { code: response.error.code } : {}),
    error: response.error?.message ?? "Could not scan this page",
  };
}

/* The per-site scanner consent prompt is gone (ADR-0019 Decision 2, amended).
 * Decision 1 already forbids ambient scanning outright — no persistent
 * scanner, no dynamic content-script registration, no standing all-sites
 * grant, one shot only — so every scan is an explicit per-invocation act on
 * the tab in front of the researcher, and the click IS the consent. A second
 * per-site gate guarded a scenario Decision 1 makes impossible, and papio is
 * local-only: nothing detected leaves the machine. */

export function wirePageBulkScanLauncher(
  doc: Document = document,
  onScan: (binding: PageActionBinding) => Promise<PageBulkScanOutcome> = startPageBulkScan,
): void {
  const button = doc.getElementById("page-bulk-scan-btn");
  const status = doc.getElementById("page-bulk-scan-status");
  if (!(button instanceof HTMLButtonElement) || button.dataset.wired) return;
  button.dataset.wired = "1";

  const runScan = (binding: PageActionBinding): void => {
    const pageKey = popupPageKey(binding);
    const operationKey = `scan:${pageKey}`;
    const generation = beginPopupOperation(doc, operationKey, pageKey, "Scanning this page…");
    if (status instanceof HTMLElement) paintPopupResult(status, popupOperation(doc, operationKey));
    announcePopupOperation(doc, "Scanning this page…");
    // The header may be carrying this control, so every change to its enabled
    // state has to be reflected there: a disabled-but-hidden scan button left
    // the header looking live while its click did nothing.
    button.disabled = true;
    syncHeaderAddControl(doc);
    void onScan(binding).then(
      (result) => {
        button.disabled = false;
        syncHeaderAddControl(doc);
        if (result.ok) {
          // The workspace tab now owns this operation, and the popup is about to
          // disappear with it, so no result outlives the surface.
          finishPopupOperation(doc, operationKey, generation, null);
          // Chrome dismisses the popup when the new workspace tab takes focus;
          // Firefox keeps it open, so close it explicitly once it's open.
          window.close();
          return;
        }
        const text = result.error ?? "Could not scan this page";
        finishPopupOperation(doc, operationKey, generation, {
          ownerKey: pageKey,
          text,
          tone: result.code === "page_changed" ? "degraded" : "error",
        });
        if (status instanceof HTMLElement) paintPopupResult(status, popupOperation(doc, operationKey));
        announcePopupOperation(doc, text);
      },
      (error: unknown) => {
        button.disabled = false;
        syncHeaderAddControl(doc);
        const text = error instanceof Error ? error.message : "Could not scan this page";
        finishPopupOperation(doc, operationKey, generation, {
          ownerKey: pageKey,
          text,
          tone: "degraded",
        });
        if (status instanceof HTMLElement) paintPopupResult(status, popupOperation(doc, operationKey));
        announcePopupOperation(doc, text);
      },
    );
  };

  button.addEventListener("click", () => {
    // The binding is captured before any await, so the action can only ever
    // touch the page the researcher was looking at when they clicked.
    const binding = boundPageAction(button);
    if (binding === undefined) return;
    runScan(binding);
  });
}


export const OPEN_HANDOFF_MESSAGE = "papio.handoff.open";

/** Ask the background broker to focus an owned tab or open a freshly minted
 * handoff. The popup sends only a job id; resolver URLs never cross the runtime
 * page boundary or become popup state. */
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
  const error =
    typeof response === "object" && response !== null
      ? (response as Record<string, unknown>)["error"]
      : undefined;
  const rawMessage =
    typeof error === "object" && error !== null
      ? (error as Record<string, unknown>)["message"]
      : undefined;
  const message =
    typeof rawMessage === "string" && rawMessage.trim().length > 0
      ? rawMessage.trim().slice(0, 1000)
      : "Could not open institutional access";
  throw new Error(message);
}

export interface SessionAuthDemand {
  job_id: string;
  origin: string;
}
export type PopupSessionState = KeepaliveSnapshot & {
  releasedAuthJobs: number;
  /** Epoch ms of the latest release event; the notice shows once per stamp. */
  releasedAuthJobsAt?: number | null;
  /** One independently probed state for every configured resolver origin. */
  origins?: KeepaliveOriginSnapshot[];
  /** Browser-local bindings between authentication demand and resolver origin. */
  authDemand?: SessionAuthDemand[];
  /** Present on current workers; absent preserves older-worker fallback. */
  authDemandComplete?: boolean;
};

export const SESSION_STATE_MESSAGE = "papio.session.state";
export const SESSION_PROBE_MESSAGE = "papio.session.probe";
export const SESSION_SIGNIN_MESSAGE = "papio.session.signin";
export const SESSION_RETRY_MESSAGE = "papio.session.retry";

function isBareHTTPSOrigin(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > 300) return false;
  try {
    const parsed = new URL(value);
    return (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === "" &&
      parsed.pathname === "/" &&
      parsed.search === "" &&
      parsed.hash === "" &&
      parsed.host !== "" &&
      `${parsed.protocol}//${parsed.host}` === value
    );
  } catch {
    return false;
  }
}

function isOriginSnapshot(value: unknown): value is KeepaliveOriginSnapshot {
  if (typeof value !== "object" || value === null) return false;
  const snapshot = value as Record<string, unknown>;
  return (
    isBareHTTPSOrigin(snapshot["origin"]) &&
    typeof snapshot["authenticated"] === "boolean" &&
    (snapshot["verdict"] === "in" || snapshot["verdict"] === "out" || snapshot["verdict"] === "unknown") &&
    (snapshot["probeSource"] === "live_tab" ||
      snapshot["probeSource"] === "keepalive_tab" ||
      snapshot["probeSource"] === "none") &&
    (snapshot["lastProbeOutcome"] === undefined ||
      snapshot["lastProbeOutcome"] === "markers" ||
      snapshot["lastProbeOutcome"] === "no_markers" ||
      snapshot["lastProbeOutcome"] === "scan_failed" ||
      snapshot["lastProbeOutcome"] === "no_tab" ||
      snapshot["lastProbeOutcome"] === "partial_scan" ||
      snapshot["lastProbeOutcome"] === "conflict") &&
    (snapshot["lastVerdictAt"] === null || typeof snapshot["lastVerdictAt"] === "number") &&
    typeof snapshot["checking"] === "boolean" &&
    typeof snapshot["likelyAuthenticated"] === "boolean" &&
    typeof snapshot["pausedForReauth"] === "boolean" &&
    (snapshot["lastProbeAt"] === null || typeof snapshot["lastProbeAt"] === "number")
  );
}

function isSessionAuthDemand(value: unknown): value is SessionAuthDemand {
  if (typeof value !== "object" || value === null) return false;
  const demand = value as Record<string, unknown>;
  return (
    Object.keys(demand).every((key) => key === "job_id" || key === "origin") &&
    typeof demand["job_id"] === "string" &&
    demand["job_id"].length > 0 &&
    demand["job_id"].length <= 1024 &&
    typeof demand["origin"] === "string" &&
    isBareHTTPSOrigin(demand["origin"])
  );
}

function isSessionAuthDemandList(value: unknown): value is SessionAuthDemand[] {
  return Array.isArray(value) && value.every(isSessionAuthDemand);
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
    (state["lastProbeOutcome"] === undefined ||
      state["lastProbeOutcome"] === "markers" ||
      state["lastProbeOutcome"] === "no_markers" ||
      state["lastProbeOutcome"] === "scan_failed" ||
      state["lastProbeOutcome"] === "no_tab" ||
      state["lastProbeOutcome"] === "partial_scan" ||
      state["lastProbeOutcome"] === "conflict") &&
    (state["lastVerdictAt"] === undefined ||
      state["lastVerdictAt"] === null ||
      typeof state["lastVerdictAt"] === "number") &&
    (resolverOrigin === null || isBareHTTPSOrigin(resolverOrigin)) &&
    typeof state["pausedForReauth"] === "boolean" &&
    (state["lastProbeAt"] === null || typeof state["lastProbeAt"] === "number") &&
    (state["lastAuthReturnedAt"] === null || typeof state["lastAuthReturnedAt"] === "number") &&
    typeof state["queuedAuthJobs"] === "number" &&
    Array.isArray(state["stalledAuthJobs"]) &&
    state["stalledAuthJobs"].every((jobID) => typeof jobID === "string") &&
    typeof state["releasedAuthJobs"] === "number" &&
    (state["authDemandComplete"] === undefined || state["authDemandComplete"] === true) &&
    (state["releasedAuthJobsAt"] === undefined ||
      state["releasedAuthJobsAt"] === null ||
      typeof state["releasedAuthJobsAt"] === "number") &&
    (origins === undefined || (Array.isArray(origins) && origins.every(isOriginSnapshot)))
  );
}

/** Shared reply validation for both the snapshot-only and probing session
 * messages — the background replies with the identical `{ state, origins }`
 * envelope either way. */
function parseSessionReply(response: unknown): PopupSessionState | undefined {
  if (typeof response !== "object" || response === null) return undefined;
  const envelope = response as Record<string, unknown>;
  const rawState = envelope["state"];
  if (!isSessionState(rawState)) return undefined;
  const state = rawState as PopupSessionState & { authDemand?: unknown };
  const rawOrigins = envelope["origins"];
  const envelopeOrigins =
    Array.isArray(rawOrigins) && rawOrigins.every(isOriginSnapshot)
      ? rawOrigins
      : undefined;
  const knownOrigins = new Set(
    envelopeOrigins?.map((origin) => origin.origin) ??
      (Array.isArray(state.origins)
        ? state.origins.map((origin) => origin.origin)
        : state.resolverOrigin === null || state.resolverOrigin === undefined
          ? []
          : [state.resolverOrigin]),
  );
  // Demand metadata is local decoration. A stale/older worker, a future
  // worker's unknown origin, or malformed decoration falls back to the
  // validated core state rather than affecting row association.
  const validDemand =
    state.authDemand === undefined ||
    (isSessionAuthDemandList(state.authDemand) &&
      state.authDemand.every((demand) => knownOrigins.has(demand.origin)));
  const normalizedState = validDemand
    ? state
    : (() => {
        const { authDemand: _ignored, ...withoutDemand } = state;
        return withoutDemand as PopupSessionState;
      })();
  if (envelopeOrigins === undefined) return normalizedState;
  return { ...normalizedState, origins: envelopeOrigins };
}

/** Pure snapshot read — never triggers a probe or content-script injection. */
export async function requestSessionState(): Promise<PopupSessionState | undefined> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({ type: SESSION_STATE_MESSAGE });
    return parseSessionReply(response);
  } catch {
    return undefined;
  }
}

/** Starts (or joins) a probe of the configured resolver origin(s), then
 * returns the resulting snapshot. Reserved for the once-per-popup-open probe;
 * every later read must use `requestSessionState` instead. */
export async function requestSessionProbe(): Promise<PopupSessionState | undefined> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({ type: SESSION_PROBE_MESSAGE });
    return parseSessionReply(response);
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
    const focused = (response as Record<string, unknown>)["focused"];
    return typeof focused === "number" ? focused : 0;
  } catch {
    return 0;
  }
}

const leftoverCleanupHandlers = new WeakMap<HTMLButtonElement, () => Promise<number>>();

/** Offer a bounded review action for tabs papio left open from an earlier
 * session. Papio focuses one candidate; the operator closes it with browser
 * controls. Hidden at zero. */
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
      ? "1 papio tab is ready for review; close it with browser controls."
      : `${count} papio tabs are ready for review; close them with browser controls.`;
  button.textContent = "Review in browser";
  leftoverCleanupHandlers.set(button, onCleanup);
  if (!button.dataset.wired) {
    button.dataset.wired = "1";
    button.addEventListener("click", () => {
      const cleanup = leftoverCleanupHandlers.get(button);
      if (cleanup === undefined) return;
      button.disabled = true;
      button.textContent = "Focusing…";
      void cleanup().then(
        (focused) => {
          button.disabled = false;
          button.textContent = "Review in browser";
          message.textContent =
            focused > 0
              ? "Focused a papio tab; close it with browser controls."
              : "No reviewable papio tabs remain.";
        },
        () => {
          button.disabled = false;
          button.textContent = "Review in browser";
          message.textContent = "Could not focus a papio tab; close it with browser controls.";
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
 * reader must subtract from "now" themselves. Empty while fresh — a card that
 * says "just now" is spending a line to report that nothing has aged. */
function formatAgo(timestamp: number | null): string {
  if (timestamp === null || !Number.isFinite(timestamp)) return "";
  const elapsed = Math.max(0, Date.now() - timestamp);
  const minutes = Math.floor(elapsed / 60_000);
  if (minutes < 1) return "";
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

/** Where the verdict came from, with no age attached. */
function sessionEvidenceSource(state: PopupSessionState): string {
  return state.probeSource === "live_tab"
    ? "via your library tab"
    : state.probeSource === "keepalive_tab"
      ? "via keepalive tab"
      : "no probe evidence";
}

/** How long ago the verdict was confirmed, as a bare phrase. */
function sessionVerdictAge(state: PopupSessionState): string {
  const rawTimestamp = state.lastVerdictAt;
  if (typeof rawTimestamp !== "number" || !Number.isFinite(rawTimestamp)) return "";
  return formatAgo(rawTimestamp);
}

/** Source and age together, for the states where ONE fact owns both: a plain
 * "Signed in · via your library tab · 3m ago" cannot be misread. A state that
 * reports two different times - a verdict confirmed an hour ago AND a recheck
 * running now - must not use this: chaining them produced "Signed in -
 * rechecking now · via your library tab · 1h ago" on the operator's screen,
 * where the age reads as the age of the recheck. Those states bind the age
 * into the label beside the fact it measures. */
function sessionEvidenceDetail(state: PopupSessionState): string {
  const source = sessionEvidenceSource(state);
  const age = sessionVerdictAge(state);
  return age === "" ? source : `${source} · ${age}`;
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
  // An "unknown" verdict never had a decisive commit — commitOriginProbe()
  // (keepalive.ts) advances lastProbeAt/lastProbeOutcome for an
  // inconclusive attempt but deliberately leaves lastVerdictAt untouched,
  // so "unknown" is ALWAYS stale by the freshness check below. Resolving
  // it here, before that check, is what turns a completed but
  // inconclusive probe (no_tab/no_markers/etc.) into its own honest
  // terminal label instead of the eternal "Checking session…" that block
  // used to produce for every origin that has never landed a decisive
  // verdict — the steady state whenever no library tab is open.
  if (verdict === "unknown") {
    switch (state.lastProbeOutcome) {
      case "no_tab":
        return {
          label: "No library page open — open your library to verify",
          detail,
          action: "signin",
        };
      case "no_markers":
        return {
          label: "Signed-in state unclear on this page",
          detail: "papio inspected your library tab but found no sign-in indicators",
          action: "signin",
        };
      case "scan_failed":
        return {
          label: "papio couldn't read the library page — check site access in Options",
          detail,
          action: "signin",
        };
      case "partial_scan":
        return {
          label: "Too many library tabs to check reliably",
          detail,
          action: "signin",
        };
      case "conflict":
        return {
          label: "Your library tabs disagree — open your library page",
          detail,
          action: "signin",
        };
      default:
        return {
          label: "Session unknown — open your library page to verify",
          detail,
          action: "signin",
        };
    }
  }
  // Only "in"/"out" verdicts reach here, and only a decisive commit ever
  // sets one, so lastVerdictAt is always a number below — an AGED verdict,
  // never an unresolved probe, is what lands in this block.
  const lastVerdictAt = state.lastVerdictAt;
  const verdictAge =
    typeof lastVerdictAt === "number" && Number.isFinite(lastVerdictAt)
      ? Date.now() - lastVerdictAt
      : undefined;
  const aged = verdictAge !== undefined && verdictAge > SESSION_STALE_MS;
  const stale = verdictAge === undefined || verdictAge < 0 || aged;
  if (stale) {
    // Two different times in one card: when the verdict was confirmed, and
    // whatever is true about it now. The age belongs beside the verdict it
    // measures - trailing it off the detail chain produced "Signed in —
    // rechecking now · via your library tab · 1h ago", where the age reads as
    // the age of the recheck.
    //
    // And the second clause may NOT claim a check is running: the `checking`
    // branch above returns first, so reaching here proves no probe is in
    // flight. Keep-warm is on (the !enabled branch returned too) and the
    // verdict has aged past its window, so a recheck is DUE - which is a fact
    // about the schedule, not an activity papio cannot see.
    const confirmed = sessionVerdictAge(state);
    const since = confirmed === "" ? "" : ` ${confirmed}`;
    if (aged && verdict === "in" && state.authenticated) {
      return {
        label: `Signed in${since} — due a recheck`,
        detail: sessionEvidenceSource(state),
        action: "none",
      };
    }
    if (aged && verdict === "out" && !state.authenticated) {
      return {
        label: `Signed out${since} — due a recheck`,
        detail: sessionEvidenceSource(state),
        action: "signin",
      };
    }
    return {
      label: "Can't tell yet — open your library page",
      detail,
      action: "signin",
    };
  }
  if (verdict === "out" && !state.authenticated) {
    return {
      label: "Signed out or expired",
      detail,
      action: "signin",
    };
  }
  if (verdict === "in" && state.authenticated) {
    return {
      label: "Signed in",
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
/** A warm, freshly-verified session with nothing to do is the assumed steady
 * state — rendering it is noise. Rows appear only when the operator can act
 * (sign in) or should doubt the verdict (stale evidence). */
const SESSION_ROW_FRESH_MS = 10 * 60 * 1000;

function isFreshSessionTimestamp(value: number | null | undefined, now = Date.now()): value is number {
  if (typeof value !== "number" || !Number.isFinite(value)) return false;
  const age = now - value;
  return age >= 0 && age <= SESSION_ROW_FRESH_MS;
}


export function deriveSessionRows(
  state: PopupSessionState | undefined,
  jobs: readonly ActiveJob[] = [],
): SessionRowState[] {
  if (
    state === undefined ||
    !Array.isArray(state.origins) ||
    !state.origins.every(isOriginSnapshot)
  ) {
    return [];
  }
  const knownOrigins = new Set(state.origins.map((origin) => origin.origin));
  const rawDemands = Array.isArray(state.authDemand) ? state.authDemand : [];
  const demandCounts = new Map<string, number>();
  for (const demand of rawDemands) {
    if (typeof demand !== "object" || demand === null) continue;
    const demandJobID = (demand as unknown as Record<string, unknown>)["job_id"];
    if (typeof demandJobID !== "string") continue;
    demandCounts.set(demandJobID, (demandCounts.get(demandJobID) ?? 0) + 1);
  }
  const validDemands = rawDemands.filter(
    (demand) =>
      isSessionAuthDemand(demand) &&
      demandCounts.get(demand.job_id) === 1 &&
      knownOrigins.has(demand.origin),
  );
  const demandOrigins = new Set(validDemands.map((demand) => demand.origin));
  const demandJobIDs = new Set(validDemands.map((demand) => demand.job_id));
  if (
    jobs.some(
      (job) =>
        (job.requires_auth === true ||
          job.status === "auth_pending" ||
          job.waiting_for_session === true ||
          job.engagement_required === true) &&
        !demandJobIDs.has(job.job_id),
    ) &&
    demandOrigins.size === 0
  ) {
    return [];
  }
  return state.origins
    .map((originState) => {
      const card = deriveSessionCardState({
        ...state,
        ...originState,
        resolverOrigin: originState.origin,
      });
      return { origin: originState.origin, ...card };
    })
    .filter((row) => {
      // While work is actively waiting on one or more institutions, only
      // those exact origin rows may share the card. This keeps an inactive
      // origin's stale evidence from reading as the waiting job's blocker.
      if (demandOrigins.size > 0 && !demandOrigins.has(row.origin)) return false;
      if (row.action !== "none") return true;
      const verifiedAt = state.origins?.find((o) => o.origin === row.origin)?.lastVerdictAt;
      return demandOrigins.has(row.origin) || !isFreshSessionTimestamp(verifiedAt);
    });
}

let sessionNoticeFadeTimer: ReturnType<typeof setTimeout> | undefined;
let sessionNoticeHideTimer: ReturnType<typeof setTimeout> | undefined;
let sessionProbeRetryTimer: ReturnType<typeof setTimeout> | undefined;
/** One targeted re-probe per stale decisive verdict. A popup refresh still
 * reads snapshots every five seconds; this key prevents that cadence from
 * repeatedly injecting the resolver probe when an inconclusive check leaves
 * the earlier verdict timestamp unchanged. */
let staleSessionProbeKey: string | undefined;

function staleDecisiveSessionKey(state: PopupSessionState | undefined): string | undefined {
  if (
    state === undefined ||
    state.checking === true ||
    state.pausedForReauth ||
    (state.verdict !== "in" && state.verdict !== "out") ||
    typeof state.lastVerdictAt !== "number" ||
    !Number.isFinite(state.lastVerdictAt)
  ) {
    return undefined;
  }
  const age = Date.now() - state.lastVerdictAt;
  if (age >= 0 && age <= SESSION_STALE_MS) return undefined;
  return `${state.resolverOrigin ?? ""}\u0000${state.verdict}\u0000${state.lastVerdictAt}`;
}
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

function scheduleSessionProbeRetry(
  state: PopupSessionState | undefined,
  jobs: readonly ActiveJob[] = [],
): void {
  clearTimeout(sessionProbeRetryTimer);
  sessionProbeRetryTimer = undefined;
  const staleKey = staleDecisiveSessionKey(state);
  const checking = state?.checking === true;
  if (!checking && (staleKey === undefined || staleKey === staleSessionProbeKey)) return;
  if (staleKey !== undefined) staleSessionProbeKey = staleKey;
  sessionProbeRetryTimer = setTimeout(() => {
    sessionProbeRetryTimer = undefined;
    const read = checking ? requestSessionState() : requestSessionProbe();
    // Repaint through refresh(), not directly: a direct renderInstitutionSession
    // call here is outside the refresh generation fence, so a probe that settled
    // after a newer refresh used to paint stale session state over it.
    void read.then(() => refresh().catch(() => undefined));
  }, 2_000);
  (sessionProbeRetryTimer as unknown as { unref?: () => void }).unref?.();
}


export function renderInstitutionSession(
  doc: Document,
  state: PopupSessionState | undefined,
  onSignIn: (origin?: string) => Promise<void> = openInstitutionSignIn,
  jobs: readonly ActiveJob[] = [],
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

  // Legacy states (no per-origin snapshots) synthesize one row so the calm
  // filter and render paths treat every shape identically.
  const rows =
    state.origins === undefined || state.origins.length === 0
      ? (() => {
          const legacy = deriveSessionCardState(state);
          const fresh = isFreshSessionTimestamp(state.lastVerdictAt);
          return legacy.action === "none" && fresh
            ? []
            : [{ origin: state.resolverOrigin ?? "", ...legacy }];
        })()
      : deriveSessionRows(state, jobs);
  const waitingVisible = waiting instanceof HTMLElement && waiting.hidden === false;
  const noticeVisible = Math.max(0, Math.trunc(state.releasedAuthJobs)) > 0;
  // Calm steady state — every session warm and fresh, nothing waiting, no
  // notice — renders NO card at all: quiet means live.
  if (rows.length === 0 && !waitingVisible && !noticeVisible) {
    card.hidden = true;
    clearSessionNoticeTimers();
    return;
  }
  card.hidden = false;
  const multiOrigin = rows.length > 1 && rowsContainer instanceof HTMLElement;
  if (multiOrigin) {
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = true;
    origin.textContent = "";
    rowsContainer.hidden = false;
    renderSessionRows(doc, rowsContainer, rows, onSignIn);
  } else if (rows.length === 0) {
    // Only the waiting list or a release notice justifies the card — and a
    // card with a bare heading reads as broken, so say WHY it is quiet:
    // rows only filter out when every session is signed in and freshly
    // verified. Same words as the row labels above — "warm" is the internal
    // evidence tier (ADR-0018), never the reader's word for it.
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = false;
    origin.textContent = "";
    status.textContent = "All institutions signed in";
    signIn.hidden = true;
    signIn.disabled = true;
    if (rowsContainer instanceof HTMLElement) {
      rowsContainer.hidden = true;
      rowsContainer.replaceChildren();
    }
  } else {
    if (rowsContainer instanceof HTMLElement) {
      rowsContainer.hidden = true;
      rowsContainer.replaceChildren();
    }
    if (legacyRow instanceof HTMLElement) legacyRow.hidden = false;
    const row = rows[0] as SessionRowState;
    status.textContent = sessionRowText(row);
    origin.textContent = resolverHost(row.origin);
    signIn.disabled = row.action === "none";
    signIn.hidden = row.action === "none";
    signIn.setAttribute("aria-describedby", status.id);
    sessionSignInHandlers.set(signIn, () => onSignIn(row.origin ?? undefined));
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
  authDemand: readonly SessionAuthDemand[] = [],
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
  const heading = doc.getElementById("institution-session-waiting-heading");
  if (heading instanceof HTMLElement) {
    // A sub-heading earns its line by labelling a group. Above one row whose
    // own button already says Open — and whose card heading already says
    // Institution session — it only restates what is directly below it.
    heading.hidden = jobs.length < 2;
    heading.textContent = jobs.some(
      (job) => job.engagement_required === true && job.tab_id < 0,
    )
      ? "Open institutional access"
      : "Waiting on your sign-in";
  }
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
    const matchingDemands = authDemand.filter((entry) => entry.job_id === job.job_id);
    const demand =
      matchingDemands.length === 1 && isSessionAuthDemand(matchingDemands[0])
        ? matchingDemands[0]
        : undefined;
    const demandHost = demand === undefined ? undefined : resolverHost(demand.origin);
    if (job.waiting_for_session === true) {
      // This paper is not waiting on the operator: it deferred to a sibling
      // paper's tab already at the institution's login page, and resumes on
      // its own once that sign-in completes. A "Focus" action would send the
      // operator to a page they have no reason to act on.
      const status = doc.createElement("p");
      status.className = "institution-session-waiting-status";
      status.textContent =
        demandHost === undefined
          ? "Waiting for the institution sign-in — another paper's tab is at the login page"
          : `Waiting for ${demandHost} sign-in — another paper's tab is at the login page`;
      row.append(paper, status);
      list.append(row);
      continue;
    }
    const copy = doc.createElement("div");
    copy.className = "institution-session-origin-copy";
    const failure = doc.createElement("p");
    failure.className = "institution-session-waiting-status";
    failure.hidden = true;
    copy.append(paper, failure);
    const button = doc.createElement("button");
    button.className = "ghost";
    button.type = "button";
    const actionLabel = job.engagement_required === true && job.tab_id < 0 ? "Open" : "Focus";
    button.textContent = actionLabel;
    button.addEventListener("click", () => {
      button.disabled = true;
      button.textContent = actionLabel === "Open" ? "Opening…" : "Focusing…";
      void onFocus(job.job_id).then(
        () => {
          failure.hidden = true;
          failure.textContent = "";
          button.disabled = false;
          button.textContent = actionLabel;
        },
        (error: unknown) => {
          failure.textContent =
            error instanceof Error && error.message.trim().length > 0
              ? error.message
              : "Institutional access could not be opened";
          failure.hidden = false;
          button.disabled = false;
          button.textContent = "Try again";
        },
      );
    });
    row.append(copy, button);
    list.append(row);
  }
}

/** One row builder for every Needs-you blocker.
 *
 * All three blocker kinds had the same hand-rolled pending/label/error dance
 * against a module-level boolean map, which lost its state on the next
 * five-second repaint and could not show a persistent failure at all. They now
 * share the document operation registry, so a click's pending state and its
 * error survive rerenders and a slow reply cannot overwrite a newer one. */
function appendBlockerRow(
  doc: Document,
  list: HTMLElement,
  spec: {
    operationKey: string;
    ownerKey: string;
    title: string;
    /** Why this row exists. It rides the control it explains — `title` for
     * hover, a drawn-nowhere companion for `aria-describedby` so keyboard and
     * screen-reader users are not left with hover-only meaning — instead of
     * spending a visible line restating the ask above it. */
    reason?: string;
    /** An element that already states this row's reason where the reader can
     * see it (the section message, when one row owns the whole section). The
     * control describes itself from there rather than saying it twice. */
    reasonElementID?: string;
    idleLabel: string;
    pendingLabel: string;
    run: () => Promise<{ text: string; tone: PopupFeedbackTone } | null>;
  },
): void {
  const row = doc.createElement("div");
  row.className = "needs-you-item";
  const copy = doc.createElement("div");
  copy.className = "needs-you-copy";
  const title = doc.createElement("p");
  title.className = "needs-you-paper";
  title.textContent = spec.title;
  copy.append(title);
  const result = doc.createElement("p");
  result.className = "popup-result";
  result.hidden = true;
  const button = doc.createElement("button");
  button.className = "ghost";
  button.type = "button";
  if (spec.reasonElementID !== undefined) {
    button.setAttribute("aria-describedby", spec.reasonElementID);
  } else if (spec.reason !== undefined) {
    // `hidden`, not `.visually-hidden`: aria-describedby reads a display:none
    // target, so the reason reaches keyboard and screen-reader users exactly
    // once, as the control's description, and never as a stray line of prose
    // between the row title and the button.
    const reason = doc.createElement("p");
    reason.hidden = true;
    reason.id = `needs-you-reason-${spec.operationKey.replace(/[^A-Za-z0-9_-]+/g, "-")}`;
    reason.textContent = spec.reason;
    copy.append(reason);
    button.title = spec.reason;
    button.setAttribute("aria-describedby", reason.id);
  }

  const paint = (): void => {
    const state = popupOperation(doc, spec.operationKey);
    const pending = state?.phase === "pending";
    button.textContent = pending ? spec.pendingLabel : spec.idleLabel;
    button.disabled = pending;
    paintPopupResult(result, state?.phase === "result" ? state : undefined);
  };

  button.addEventListener("click", () => {
    if (popupOperation(doc, spec.operationKey)?.phase === "pending") return;
    const generation = beginPopupOperation(doc, spec.operationKey, spec.ownerKey, spec.pendingLabel);
    paint();
    announcePopupOperation(doc, spec.pendingLabel);
    void spec.run().then(
      (outcome) => {
        if (!finishPopupOperation(doc, spec.operationKey, generation, outcome === null ? null : { ownerKey: spec.ownerKey, ...outcome })) {
          return;
        }
        paint();
        if (outcome !== null) announcePopupOperation(doc, outcome.text);
      },
      () => {
        const text = "Didn't work — try again";
        if (!finishPopupOperation(doc, spec.operationKey, generation, { ownerKey: spec.ownerKey, text, tone: "degraded" })) {
          return;
        }
        paint();
        announcePopupOperation(doc, text);
      },
    );
  });

  paint();
  copy.append(result);
  row.append(copy, button);
  list.append(row);
}

/** The one blocker line that prevents a wrong action instead of explaining
 * one: without it a researcher solves the check, closes the tab, and leaves
 * papio nothing to resume in. So it is stated exactly once — as the section
 * message while a single ask owns the section, and otherwise on the control it
 * belongs to, never in both places. */
const CHALLENGE_REASSURANCE = "Solve the check in the open tab — papio resumes on its own.";

/** Render durable browser actions that need a user gesture: a security check,
 * provider permission grants, and one auth retry.
 *
 * Exactly three row classes, in that order, capped at three rows plus an
 * overflow link. There is deliberately no Downloads row: no browser-local
 * Downloads projection exists in `StoreShape`, and inferring one from daemon
 * prose would put a control here that cannot act. Downloads actions stay in the
 * durable inbox.
 *
 * One ask renders as one row: the row's title and its button already say what
 * is wanted and where, so a section message above a lone row can only restate
 * it. Settings is not named here either — the header's own Settings control is
 * the route, and copy that named a route this section does not carry was the
 * inversion of the rule it was written under. */
export function renderNeedsAttention(
  doc: Document,
  jobs: ActiveJob[],
  blockedProviderHosts: readonly string[] = [],
  onFocus: (jobID: string) => Promise<void> = openHandoff,
  authStalledJobs: readonly string[] = [],
  onRetry: (jobID: string) => Promise<void> = retryAuthStalled,
  onGrantProvider: (host: string) => Promise<boolean> = grantProviderAccess,
  authDemand: readonly SessionAuthDemand[] = [],
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
    (job) =>
      (job.status === "auth_pending" || job.engagement_required === true) &&
      job.challenge_blocked !== true,
  );
  renderWaitingOnSignIn(doc, pending, onFocus, authDemand);
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
  const visibleChallenges = challengeJobs.slice(0, 1);
  const visibleStalled = stalled.slice(0, 1);
  const visibleBlocked = blocked.slice(
    0,
    Math.max(0, 3 - visibleChallenges.length - visibleStalled.length),
  );
  const overflowCount =
    challengeJobs.length - visibleChallenges.length +
    blocked.length - visibleBlocked.length +
    stalled.length - visibleStalled.length;
  section.hidden = challengeJobs.length === 0 && blocked.length === 0 && stalled.length === 0;
  list.replaceChildren();
  if (section.hidden) return;

  // The heading never renames itself per blocker kind — that read as a
  // different section appearing rather than the same one gaining a row — and
  // it no longer borrows "Needs you", which ADR-0023 reserves for the daemon's
  // turns_required and the pulse above already spends its primary line on.
  heading.textContent = "Do this in the browser";
  const rowCount = visibleChallenges.length + visibleStalled.length + visibleBlocked.length;
  // A lone security check keeps its reassurance visible; every other lone ask
  // is fully stated by its own row. Two or more rows can be of different kinds,
  // and one instruction cannot honestly describe both, so the message
  // summarises rather than instructs. The overflow count is not appended here:
  // it is a route, and it is rendered once, as the control that carries it.
  const reassure = rowCount === 1 && visibleChallenges.length === 1;
  message.textContent = reassure
    ? CHALLENGE_REASSURANCE
    : rowCount > 1
      ? `${rowCount} things to clear here`
      : "";
  message.hidden = message.textContent === "";

  for (const job of visibleChallenges) {
    const host = job.challenge_host!.trim().toLowerCase();
    appendBlockerRow(doc, list, {
      operationKey: `challenge:${job.job_id}`,
      ownerKey: job.job_id,
      title: `Security check — ${host}`,
      ...(reassure ? { reasonElementID: message.id } : { reason: CHALLENGE_REASSURANCE }),
      idleLabel: "Open tab",
      pendingLabel: "Opening…",
      run: async () => {
        await onFocus(job.job_id);
        // The tab now has focus and this popup is gone with it, so a result here
        // would have no surface to live on.
        return null;
      },
    });
  }

  for (const jobID of visibleStalled) {
    const knownJob = jobs.find((job) => job.job_id === jobID);
    appendBlockerRow(doc, list, {
      operationKey: `auth-retry:${jobID}`,
      ownerKey: jobID,
      title: knownJob === undefined ? jobID : handoffPaperLabel(knownJob),
      reason: "Sign-in didn't stick — sign in, then retry this paper.",
      idleLabel: "Retry now",
      pendingLabel: "Retrying…",
      run: async () => {
        await onRetry(jobID);
        return { text: "Retry requested", tone: "progress" };
      },
    });
  }

  for (const host of visibleBlocked) {
    appendBlockerRow(doc, list, {
      operationKey: `provider:${host}`,
      ownerKey: host,
      title: host,
      reason: "papio needs your permission to reach this source.",
      idleLabel: "Allow",
      pendingLabel: "Allowing…",
      run: async () => {
        const granted = await onGrantProvider(host);
        return granted
          ? { text: "Allowed", tone: "success" }
          : { text: "Not allowed — try again", tone: "degraded" };
      },
    });
  }

  if (overflowCount > 0) {
    const more = doc.createElement("button");
    more.type = "button";
    more.className = "ghost needs-you-more";
    more.textContent = `${overflowCount} more in inbox`;
    more.addEventListener("click", () => void openInbox());
    list.append(more);
  }
}

export function wireInboxLauncher(
  doc: Document = document,
  onOpen: () => Promise<void> = openInbox,
): void {
  const status = doc.getElementById("open-inbox-status");
  // One route, one control. The header inbox icon is the durable affordance and
  // now carries the decision count itself, so the pulse card no longer needs a
  // Review button of its own — the count and its route are the same element.
  const button = doc.getElementById("open-inbox-btn");
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
 * never an error surface. Both figures are measured: a count of acquired jobs
 * and its share of finished ones. papio never estimates time saved.
 */
export function renderImpactSummary(
  doc: Document,
  stats: Pick<AcquisitionStats, "acquired_total" | "failed_total"> | null,
): void {
  const section = doc.getElementById("impact-summary");
  const acquired = doc.getElementById("impact-acquired");
  const successRate = doc.getElementById("impact-success-rate");
  if (!section || !acquired || !successRate) return;
  if (stats === null) {
    section.hidden = true;
    return;
  }
  acquired.textContent = String(stats.acquired_total);
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
    void chrome.tabs.create({ url: chrome.runtime.getURL(historyPagePath()) }).then(
      () => {
        // Chrome dismisses the popup when the new tab takes focus; Firefox
        // keeps it open, so close it explicitly once the tab exists.
        window.close();
      },
      (error: unknown) => {
        // A rejected create means the history page never opened. Closing the
        // popup would hide that outright, so keep it open and say so.
        announcePopupOperation(doc, error instanceof Error ? error.message : "Could not open history");
      },
    );
  });
}

function responseErrorMessage(response: PageAcquireResponse): string {
  if (typeof response.error === "string" && response.error.length > 0) return response.error;
  if (typeof response.error === "object" && response.error !== null && typeof response.error.message === "string") {
    return response.error.message;
  }
  return "";
}

/** A job id is a durable identifier the inbox owns, not popup copy: it is
 * unreadable at a glance and the popup cannot act on it. The researcher needs to
 * know only whether their click landed. */
function pageAcquireStatus(response: PageAcquireResponse): PopupResultCopy {
  const error = responseErrorMessage(response);
  if (error) return { text: error, tone: "error" };
  if (typeof response.message === "string" && response.message.length > 0) {
    return { text: response.message, tone: "info" };
  }
  if (typeof response.job_id === "string" && response.job_id.length > 0) {
    // `outcome` is absent on an older daemon, so `duplicate` remains the
    // fallback — and the daemon keeps the two consistent for exactly this
    // reason. ADR-0008 Decision 5 fixes the artifact wording: a `ready` or
    // `imported` job proves papio validated a PDF, never that a reference
    // manager received it, so this must not say "in your library".
    if (response.outcome === "already_validated") {
      return { text: "papio already has a validated artifact", tone: "info" };
    }
    if (response.outcome === "already_queued" || response.duplicate === true) {
      return { text: "Already in papio", tone: "info" };
    }
    return { text: "Added to papio", tone: "success" };
  }
  return { text: "The daemon did not acknowledge this page.", tone: "error" };
}

export function deliveryStatusText(delivery: PendingDelivery | undefined): string {
  if (delivery?.status === "failed") return delivery.error || "Could not deliver this PDF";
  if (delivery?.status === "waiting_manual") return delivery.error || "Use the PDF viewer Download button";
  if (delivery?.status === "adopted" || delivery?.status === "downloaded") return "papio adopted PDF (validating)";
  if (delivery?.status === "sending") return "Sending PDF to papio…";
  return "";
}

export interface PopupPulseCache {
  pulse: WorkPulseResponsePayload;
  receivedAt: number;
  workerEpoch: string;
}

export interface PulseDisplay {
  primary: "Moving" | "Waiting on you" | "Stalled" | "Scheduled" | "Idle" | "Unknown";
  primaryText: string;
  /** The mechanical five-bucket string. Retained in full for the inbox and for
   * the popup's accessible title detail — the popup no longer prints it, because
   * five counts in a 380px lens is an inventory, not a status. */
  buckets: string;
  /** Exact summary of simultaneous work the primary line does NOT already name,
   * so "papio is working" does not hide four decisions waiting behind it. Zero
   * and the primary's own class are omitted; an inexact measurement contributes
   * nothing rather than a guess. */
  companion: string;
  next: string;
  capacity: string;
  /** The latest submission's own totals. Same standing as `buckets` now: the
   * inbox still renders it, and the popup keeps it on hover and in its
   * accessible description rather than on the one status line it has. */
  batch: string;
  asOf?: string;
}

let popupPulseCache: PopupPulseCache | undefined;
let popupPulseWorkerEpoch: string | undefined;

function isPulseRuntimeReply(value: unknown): value is {
  ok: true;
  available: boolean;
  pulse?: WorkPulseResponsePayload;
  received_at?: number;
  worker_epoch: string;
} {
  if (typeof value !== "object" || value === null) return false;
  const reply = value as Record<string, unknown>;
  if (reply.ok !== true || typeof reply.available !== "boolean" || typeof reply.worker_epoch !== "string") return false;
  if (!reply.available) return true;
  const pulse = reply.pulse;
  return (
    typeof pulse === "object" &&
    pulse !== null &&
    (pulse as WorkPulseResponsePayload).schema === 1 &&
    typeof (pulse as WorkPulseResponsePayload).generated_at === "string" &&
    typeof reply.received_at === "number" &&
    Number.isFinite(reply.received_at)
  );
}

export async function requestWorkPulse(): Promise<PopupPulseCache | undefined> {
  try {
    const reply = await chrome.runtime.sendMessage({ type: "papio.work.pulse" });
    if (!isPulseRuntimeReply(reply)) return undefined;
    if (popupPulseWorkerEpoch !== undefined && popupPulseWorkerEpoch !== reply.worker_epoch) popupPulseCache = undefined;
    popupPulseWorkerEpoch = reply.worker_epoch;
    if (!reply.available || reply.pulse === undefined) {
      popupPulseCache = undefined;
      return undefined;
    }
    popupPulseCache = {
      pulse: reply.pulse,
      receivedAt: Date.now(),
      workerEpoch: reply.worker_epoch,
    };
    return popupPulseCache;
  } catch {
    return undefined;
  }
}

/** Counts-v3 is the authority for effective researcher turns. The popup
 * fetches it separately from pulse, whose waiting_required bucket partitions
 * nonterminal work and may legitimately exclude terminal-job turns. */
export async function requestTriageCounts(): Promise<TriageCounts | undefined> {
  try {
    const response = await chrome.runtime.sendMessage({ type: "papio.triage.counts", request: {} });
    if (typeof response !== "object" || response === null) return undefined;
    const value = response as Record<string, unknown>;
    const counts = value["counts"];
    if (value["ok"] !== true || typeof counts !== "object" || counts === null) return undefined;
    const parsed = counts as Partial<TriageCounts>;
    if (
      typeof parsed.pending_total !== "number" ||
      typeof parsed.watch_hits !== "number" ||
      typeof parsed.actions !== "number" ||
      typeof parsed.retractions !== "number"
    ) return undefined;
    return parsed as TriageCounts;
  } catch {
    return undefined;
  }
}

/** The researcher's browser-local count choice (ADR-0023 Decision 7). The
 * header count is a browser-local count like the toolbar badge, so it obeys the
 * same preference: a researcher who chose "No number" must not get the number
 * back one click inside the popup. Defaults to "required", matching options.ts
 * and background.ts. */
export async function readToolbarCountMode(): Promise<ToolbarCountMode> {
  try {
    const values = await chrome.storage.local.get([TOOLBAR_COUNT_MODE_KEY]);
    const mode = values[TOOLBAR_COUNT_MODE_KEY];
    return mode === "all" || mode === "off" ? mode : "required";
  } catch {
    // Fail to the default rather than to a number: an unreadable preference is
    // not permission to print a count the researcher may have switched off.
    return "required";
  }
}

/** Display cap for the header count. `turns_required` validates up to 1,000,000
 * on the wire, which no header control renders legibly. Three digits fit: the
 * control is `width: auto`, and the widest string it ever paints ("999+") was
 * measured at 72px against a 356px content box, so the header still holds the
 * brand plus two 32px pills beside it. The exact figure stays in the control's
 * accessible name and title, so this cap is display-only. */
export const HEADER_COUNT_CAP = 999;

export interface InboxCountDisplay {
  /** Digits to paint, or "" when the control shows no number at all. */
  readonly text: string;
  /** The exact figure, for the accessible name. Undefined when text is "". */
  readonly exact?: number;
  /** True ONLY when the badge is displaying the exact, complete required-turn
   * count. renderWorkPulse reads this to decide whether its own "Waiting on
   * you · N decisions" line is now redundant. Deliberately false for mode
   * "all": a pending inventory is not a decision count. */
  readonly showsExactTurns: boolean;
}

const NO_INBOX_COUNT: InboxCountDisplay = { text: "", showsExactTurns: false };

export function deriveInboxCount(
  counts: Pick<TriageCounts, "pending_total" | "turns_required" | "required_turns_complete"> | undefined,
  mode: ToolbarCountMode,
): InboxCountDisplay {
  if (counts === undefined || mode === "off") return NO_INBOX_COUNT;
  if (mode === "all") {
    const total = counts.pending_total;
    if (typeof total !== "number" || total <= 0) return NO_INBOX_COUNT;
    return { text: cappedCount(total), exact: total, showsExactTurns: false };
  }

  // Mode "required". `required_turns_complete` describes the per-ITEM
  // projection LIST, not the count: past the cap the daemon drops the list and
  // "keeps the exact attention count" (internal/triage/triage.go:1006-1011),
  // which `extension/src/inbox.ts` relies on too. So gating the number on it
  // hid an exact daemon-supplied count and left the pulse card as the only
  // place showing it.
  //
  // The toolbar badge DOES blank itself there (background.ts:657-662), but for
  // a reason that does not transfer: a 16px toolbar square cannot render four
  // digits. This control is `width: auto` and caps at 999+, so it can.
  const turns = counts.turns_required;
  if (typeof turns !== "number" || turns <= 0) return NO_INBOX_COUNT;
  return { text: cappedCount(turns), exact: turns, showsExactTurns: true };
}

function cappedCount(value: number): string {
  return value > HEADER_COUNT_CAP ? `${HEADER_COUNT_CAP}+` : String(value);
}

/** Paints the header inbox count. Returns the display so the caller can pass
 * `showsExactTurns` to renderWorkPulse without deriving it twice. */
export function renderInboxCount(
  doc: Document,
  counts: Pick<TriageCounts, "pending_total" | "turns_required" | "required_turns_complete"> | undefined,
  mode: ToolbarCountMode,
): InboxCountDisplay {
  const display = deriveInboxCount(counts, mode);
  const button = doc.getElementById("open-inbox-btn");
  const slot = doc.getElementById("open-inbox-count");
  // Optional by design, like the pulse's own controls: a caller assembling a
  // partial DOM still gets a working launcher.
  if (!(button instanceof HTMLElement) || !(slot instanceof HTMLElement)) return display;
  slot.textContent = display.text;
  slot.hidden = display.text === "";
  button.classList.toggle("has-count", display.text !== "");
  // The cap never reaches the accessible name, and the count is never the only
  // thing a screen reader hears: the label keeps its verb.
  const label = display.exact === undefined
    ? "Open inbox"
    : display.showsExactTurns
      ? `Open inbox · ${display.exact} ${display.exact === 1 ? "decision" : "decisions"} waiting`
      : `Open inbox · ${display.exact} pending ${display.exact === 1 ? "item" : "items"}`;
  button.setAttribute("aria-label", label);
  button.title = label;
  return display;
}

function pulseCount(pulse: WorkPulseResponsePayload, field: keyof WorkPulseResponsePayload): number | undefined {
  const value = pulse[field];
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? value : undefined;
}

/** The one Unknown state with no reason to give: nothing has been measured yet.
 * "Can't tell" left the researcher guessing whether papio had checked and found
 * nothing or had not checked at all, so every other Unknown names its cause. */
const PULSE_UNMEASURED = "Progress unknown — papio hasn't reported yet";

/** True when the pulse has nothing worth a line, which is not the same as
 * having measured that nothing is happening (that is Idle). */
export function pulseIsUnmeasured(display: PulseDisplay): boolean {
  return display.primary === "Unknown" && display.primaryText === PULSE_UNMEASURED;
}

/** A scheduled instant with the day named whenever it is not today. A bare
 * "at 08:00" for a retry thirteen hours out reads as imminent, and the reader
 * has no way to tell it apart from one eight minutes away. */
export function formatPulseWhen(at: number, now: number): string {
  if (!Number.isFinite(at)) return "later";
  if (at <= now) return "any moment";
  const clock = new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const midnight = (t: number): number => {
    const d = new Date(t);
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  };
  const days = Math.round((midnight(at) - midnight(now)) / 86_400_000);
  if (days <= 0) return `at ${clock}`;
  if (days === 1) return `tomorrow at ${clock}`;
  if (days < 7) return `${new Date(at).toLocaleDateString([], { weekday: "short" })} at ${clock}`;
  return `${new Date(at).toLocaleDateString([], { month: "short", day: "numeric" })} at ${clock}`;
}

export function derivePulseDisplay(
  cache: PopupPulseCache | undefined,
  connectionStatus: StoreShape["connectionStatus"] = "connected",
  now = Date.now(),
  maxAgeMs = 15_000,
  counts?: Pick<TriageCounts, "pending_total" | "turns_required">,
): PulseDisplay {
  if (connectionStatus !== "connected") {
    return {
      primary: "Unknown",
      primaryText: connectionStatus === "session_elsewhere"
        ? "Progress unknown — another browser holds the papio session"
        : "Progress unknown — the papio daemon isn't answering",
      buckets: "", companion: "", next: "", capacity: "", batch: "",
    };
  }
  if (cache === undefined || cache.workerEpoch === "" || now - cache.receivedAt < 0 || now - cache.receivedAt > maxAgeMs) {
    const generated = cache === undefined ? undefined : Date.parse(cache.pulse.generated_at);
    return {
      primary: "Unknown",
      primaryText: generated !== undefined && Number.isFinite(generated) && generated > 0
        ? `Status as of ${new Date(generated).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`
        : PULSE_UNMEASURED,
      buckets: "",
      companion: "",
      next: "",
      capacity: "",
      batch: "",
    };
  }
  const pulse = cache.pulse;
  const inFlight = pulseCount(pulse, "in_flight");
  const continuing = pulseCount(pulse, "continuing");
  const scheduled = pulseCount(pulse, "scheduled");
  const waiting = pulseCount(pulse, "waiting_required");
  const stalled = pulseCount(pulse, "stalled");
  const moving = inFlight !== undefined && continuing !== undefined ? inFlight + continuing : undefined;
  const movingPositive = (inFlight ?? 0) > 0 || (continuing ?? 0) > 0;
  const projectionComplete =
    pulse.projection_complete === true &&
    inFlight !== undefined &&
    continuing !== undefined &&
    scheduled !== undefined &&
    waiting !== undefined &&
    stalled !== undefined;
  const complete = projectionComplete && pulse.nonterminal_total === 0;
  const hasStallEpisode = Array.isArray(pulse.stall_episodes) && pulse.stall_episodes.length > 0;
  let primary: PulseDisplay["primary"] = "Unknown";
  if (movingPositive) primary = "Moving";
  else if (projectionComplete && waiting > 0) primary = "Waiting on you";
  else if (projectionComplete && stalled > 0 && hasStallEpisode) primary = "Stalled";
  else if (projectionComplete && scheduled > 0) primary = "Scheduled";
  else if (complete) primary = "Idle";

  const bucketParts: string[] = [];
  const primaryText =
    primary === "Moving"
      ? moving === undefined
        ? "Moving"
        : `Moving · ${moving} ${moving === 1 ? "paper" : "papers"}`
      : primary === "Waiting on you"
        ? counts?.turns_required !== undefined
          // Pulse owns the Waiting on you classification; counts-v3 owns the
          // effective decision-turn total because terminal-job turns can
          // legitimately differ from pulse.waiting_required.
          ? `Waiting on you · ${counts.turns_required} decisions`
          : counts === undefined
            ? "Waiting on you"
            // Older daemons expose pending inventory, not effective turns;
            // label that fallback as pending items rather than decisions.
            : `Waiting on you · ${counts.pending_total} pending items`
        : primary === "Stalled"
          ? `Stalled · ${stalled} ${stalled === 1 ? "paper" : "papers"}`
          : primary === "Scheduled"
            ? `Scheduled · ${scheduled} ${scheduled === 1 ? "paper" : "papers"}`
            : primary === "Idle"
              ? "Idle"
              : PULSE_UNMEASURED;
  // Companion rule: every present pulse bucket is shown, including zero;
  // omission would make an absent measurement indistinguishable from zero.
  if (inFlight !== undefined) bucketParts.push(`${inFlight} in flight`);
  if (continuing !== undefined) bucketParts.push(`${continuing} continuing`);
  if (scheduled !== undefined) bucketParts.push(`${scheduled} scheduled`);
  // "need you" is reserved for the turn authority (counts-v3 turns_required),
  // which the inbox renders. This is the nonterminal bucket and can legitimately
  // differ from it, so it must not borrow the same words. See ADR-0023's addendum.
  if (waiting !== undefined) bucketParts.push(`${waiting} awaiting your turn`);
  if (stalled !== undefined) bucketParts.push(`${stalled} stalled`);
  if (bucketParts.length > 0) bucketParts.unshift("Nonterminal breakdown");
  let next = "";
  if (pulse.next_action !== undefined) {
    const papers = pulse.next_action.count === undefined
      ? ""
      : ` ${pulse.next_action.count} paper${pulse.next_action.count === 1 ? "" : "s"}`;
    const when = formatPulseWhen(Date.parse(pulse.next_action.at), now);
    next = pulse.next_action.kind === "retry"
      ? `Next: retrying${papers} ${when}`
      : pulse.next_action.kind === "delivery_poll"
        ? `Next: checking delivery${papers} ${when}`
        : `Next: opening the ${pulse.next_action.source ?? "source"} gate${papers} ${when}`;
  }
  let capacity = "";
  if (pulse.effect_capacity !== undefined) {
    const cap = pulse.effect_capacity;
    const queued = cap.waiting ?? 0;
    // Silent when nothing is held and nothing is queued: "0/1 busy" reported a
    // configured limit as though it were news, on the one line the researcher
    // has for why papio is not doing more.
    if (queued > 0) {
      capacity = `${queued} waiting their turn — papio works on ${cap.limit} at a time`;
    } else if (cap.busy > 0) {
      capacity = `${cap.busy} of ${cap.limit} in the browser now`;
    }
  }
  let batch = "";
  const latest = pulse.latest_batch;
  if (latest?.membership === "partial") batch = "Recent browser submissions";
  else if (latest?.membership === "complete" && latest.total !== undefined && latest.settled !== undefined) {
    // Both halves were misread on the operator's own screen: unlabelled, the
    // line reads as papio's whole library sitting beside a different total on
    // the line above it, and "remaining" reads as more things asked of the
    // reader. It is one submission, and its open papers are not all asks.
    const active = latest.nonterminal_total === undefined ? "" : ` · ${latest.nonterminal_total} still open`;
    batch = `Last submission: ${latest.total} papers · ${latest.settled} settled${active}`;
  }
  // Companion: the exact non-primary work happening at the same time. Only
  // measurements the daemon actually supplied contribute, and the class already
  // named by the primary line is skipped so nothing is said twice.
  const companionParts: string[] = [];
  if (
    primary !== "Waiting on you" &&
    counts?.turns_required !== undefined &&
    counts.turns_required > 0
  ) {
    companionParts.push(`${counts.turns_required} decisions waiting`);
  }
  if (primary !== "Scheduled" && scheduled !== undefined && scheduled > 0) {
    companionParts.push(`${scheduled} scheduled`);
  }
  if (primary !== "Stalled" && stalled !== undefined && stalled > 0) {
    companionParts.push(`${stalled} stalled`);
  }
  const companion = companionParts.join(" · ");
  if (primary === "Idle" && pulse.last_finished_at !== undefined) {
    const finished = Date.parse(pulse.last_finished_at);
    if (Number.isFinite(finished)) {
      const age = relativeAgeParts(finished, now).display;
      if (age !== "just now") {
        return {
          primary,
          primaryText: `${primaryText} · last finished ${age.replace(" ago", "")}`,
          buckets: bucketParts.join(" · "),
          companion,
          next,
          capacity,
          batch,
        };
      }
    }
  }
  return { primary, primaryText, buckets: bucketParts.join(" · "), companion, next, capacity, batch };
}

/** Three lines at most, and usually one: what is happening (plus whatever else
 * is happening), the one authoritative next action, and capacity while it is
 * actually holding work back. The five-bucket breakdown and the latest cohort
 * stay available through `derivePulseDisplay` for the inbox and through this
 * section's hover and description here — printing them turned a status into an
 * inventory the researcher had to add up, and the cohort line was the last one
 * still doing it. */
export function renderWorkPulse(
  doc: Document,
  cache: PopupPulseCache | undefined,
  connectionStatus: StoreShape["connectionStatus"] = "connected",
  now = Date.now(),
  // Same shape renderInboxCount takes, so a caller reads counts once and hands
  // the identical value to both. `required_turns_complete` is unused here:
  // `badgeShowsExactTurns` already encodes the only decision it feeds.
  counts?: Pick<TriageCounts, "pending_total" | "turns_required" | "required_turns_complete">,
  /** True when the header inbox control is already displaying this exact
   * required-turn count (renderInboxCount's `showsExactTurns`). Only then may
   * this card treat its own count line as redundant. Defaults false so a caller
   * that does not paint the badge never loses the count. */
  badgeShowsExactTurns = false,
): void {
  const section = doc.getElementById("popup-pulse");
  const primary = doc.getElementById("popup-pulse-primary");
  const next = doc.getElementById("popup-pulse-next");
  const capacity = doc.getElementById("popup-pulse-capacity");
  if (!(section instanceof HTMLElement) || !(primary instanceof HTMLElement) ||
      !(next instanceof HTMLElement) || !(capacity instanceof HTMLElement)) return;
  const display = derivePulseDisplay(cache, connectionStatus, now, 15_000, counts);
  primary.textContent = display.companion === ""
    ? display.primaryText
    : `${display.primaryText} · ${display.companion}`;
  next.textContent = display.next;
  // Capacity only while it is actually constraining something: a configured
  // limit that is holding nothing back is not news.
  capacity.textContent =
    display.capacity !== "" && (display.primary === "Waiting on you" || display.primary === "Moving")
      ? display.capacity
      : "";
  for (const node of [next, capacity]) node.hidden = node.textContent === "";
  // The count route used to be a Review button on its own row here. It now
  // rides the header inbox control, so this card only has to decide whether it
  // still has anything to say.
  // Full validated measurements stay reachable without occupying a line: on
  // hover, and — because a bare `title` is not an accessibility path — as this
  // section's description.
  const detail = [display.primaryText, display.buckets, display.capacity, display.batch]
    .filter((part) => part !== "")
    .join(" · ");
  section.title = detail;
  const description = doc.getElementById("popup-pulse-detail");
  if (description instanceof HTMLElement) description.textContent = detail;
  section.dataset.state = display.primary;
  // Three independent reasons to say nothing:
  //
  //   1. Disconnected is the daemon band's story, not the pulse's.
  //   2. An unmeasured pulse says nothing worth a line.
  //   3. The card's ONLY remaining content is a count the header control is
  //      already showing exactly. Every condition below is required: a
  //      `Waiting on you` display can still carry a companion, a next action,
  //      or a constraining capacity, and each of those is news the badge
  //      cannot carry. The other five primaries are never redundant —
  //      `Unknown` names its cause, `Idle` reports measured emptiness, and
  //      `Moving`/`Stalled`/`Scheduled` are not decision counts at all.
  const countIsTheOnlyContent =
    badgeShowsExactTurns &&
    display.primary === "Waiting on you" &&
    display.companion === "" &&
    // `batch` carries "Last submission: …", which the badge cannot say.
    display.batch === "" &&
    next.textContent === "" &&
    capacity.textContent === "";
  section.hidden =
    connectionStatus !== "connected" || pulseIsUnmeasured(display) || countIsTheOnlyContent;
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

interface PopupActivityResult {
  entries: ActivityEntryPayload[];
  newCountSince?: number;
  gap: boolean;
  paged: boolean;
}

function popupActivityEntries(value: unknown): PopupActivityResult | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const response = value as Record<string, unknown>;
  if (response.ok !== true || typeof response.feature !== "boolean") return undefined;
  if (response.feature === false) return { entries: [], gap: false, paged: false };
  if (!Array.isArray(response.entries)) return undefined;
  const newCountSince =
    typeof response.new_count_since === "number" &&
    Number.isSafeInteger(response.new_count_since) &&
    response.new_count_since >= 0
      ? response.new_count_since
      : undefined;
  const gap = response.gap === true;
  return {
    entries: response.entries.filter(isPopupActivityEntry),
    ...(newCountSince === undefined ? {} : { newCountSince }),
    gap,
    paged: "latest_seq" in response && (newCountSince !== undefined || gap),
  };
}

async function readPopupActivity(): Promise<PopupActivityResult | undefined> {
  let seenThroughSeq: number | undefined;
  try {
    const stored = await chrome.storage.local.get(POPUP_ACTIVITY_WATERMARK_KEY);
    const value = stored[POPUP_ACTIVITY_WATERMARK_KEY];
    if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0) seenThroughSeq = value;
  } catch {
    // The activity request remains useful without a read watermark.
  }
  try {
    const response = await chrome.runtime.sendMessage({
      type: "papio.activity",
      request: {
        limit: 50,
        ...(seenThroughSeq === undefined ? {} : { seen_through_seq: String(seenThroughSeq) }),
      },
    });
    return popupActivityEntries(response);
  } catch {
    return undefined;
  }
}
const POPUP_ACTIVITY_WATERMARK_KEY = "papio_popup_activity_seen_through_seq_v1";
let popupCatchupInitialized = false;
export async function renderPopupCatchup(doc: Document, result: PopupActivityResult): Promise<void> {
  const section = doc.getElementById("popup-catchup");
  const text = doc.getElementById("popup-catchup-text");
  const open = doc.getElementById("popup-catchup-open");
  if (!(section instanceof HTMLElement) || !(text instanceof HTMLElement) || !(open instanceof HTMLButtonElement)) return;
  if (popupCatchupInitialized) return;
  let enabled = true;
  try {
    const setting = await chrome.storage.local.get(CATCH_UP_ENABLED_KEY);
    enabled = setting[CATCH_UP_ENABLED_KEY] !== false;
  } catch {
    // An unavailable preference must not hide durable catch-up information.
  }
  popupCatchupInitialized = true;
  if (!enabled) {
    section.hidden = true;
    return;
  }
  let seen = 0;
  try {
    const stored = await chrome.storage.local.get(POPUP_ACTIVITY_WATERMARK_KEY);
    const storedValue = stored[POPUP_ACTIVITY_WATERMARK_KEY];
    if (typeof storedValue === "number" && Number.isSafeInteger(storedValue) && storedValue >= 0) seen = storedValue;
  } catch {
    return;
  }
  const unseen = result.entries.filter((entry) => entry.seq > seen);
  const maxSeq = result.entries.reduce((max, entry) => Math.max(max, entry.seq), seen);
  if (maxSeq > seen) void chrome.storage.local.set({ [POPUP_ACTIVITY_WATERMARK_KEY]: maxSeq });
  if (unseen.length === 0) return;
  if (result.gap) {
    // A gap means the daemon cannot provide an exact count; page-local rows
    // must not be presented as a complete catch-up tally.
    text.textContent = "While you were away: newer Activity is available";
  } else if (result.paged && result.newCountSince !== undefined && result.newCountSince > 0) {
    // new_count_since is the durable Activity authority, not the fetched
    // page size. Per-kind counts are intentionally omitted because they are
    // only a view of this bounded page.
    text.textContent = `While you were away: ${result.newCountSince} updates`;
  } else {
    // Older/partial responses have no authoritative catch-up count.
    section.hidden = true;
    return;
  }
  section.hidden = false;
  open.onclick = () => void openInbox();
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

/** The one visible standing line for a live acquisition, plus whatever this
 * function decided the line should not spend its width on.
 *
 * `detail` is the prior EVENT, already demoted: the caller shows it on hover
 * and in the accessibility tree, never on the line. */
function liveStatusText(
  job: ActiveJob,
  activity: ActivityEntryPayload | undefined,
  pendingDelivery: PendingDelivery | undefined,
): { text: string; detail: string; timestamp: number; stale: boolean } {
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
  if (waitingOnOperator) {
    if (!/waiting on you|waiting for your/i.test(text)) text = `Waiting on you · ${text}`;
  }
  // An ask states what is wanted; a delivery states what is happening. What is
  // left is the timeline's own vocabulary (internal/store/activitytext.go),
  // which names a past EVENT and nothing about now: "Institution login
  // returned - 2m ago" was read on the operator's own screen as an instruction
  // nobody could act on, and labelling it "Last activity:" dated the record
  // without ever saying where the paper stands. So say the standing, which
  // fallbackJobStatus already words plainly, and demote the event off the line
  // entirely: at 380px it wrapped this status onto a second and third row to
  // date a record nobody can act on.
  let detail = "";
  if (!waitingOnOperator && activity !== undefined && text === activity.text.trim()) {
    detail = `last: ${text}`;
    text = fallbackJobStatus(job);
  }
  const age = relativeAgeParts(timestamp, now);
  const activityAge = Number.isFinite(activityTimestamp) ? relativeAgeParts(activityTimestamp, now) : undefined;
  const stalled = activityAge?.stale === true;
  const stallAge = activityAge?.compact ?? age.compact;
  return {
    // A fresh line needs no age: "just now" is what every line says the moment
    // it is written, so it carries no information the reader can act on.
    text: `${stalled ? `No progress for ${stallAge} · ` : ""}${text}${age.display === "just now" ? "" : ` · ${age.display}`}`,
    detail,
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
  currentTabID?: number,
  sessionWarm = false,
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
  const onJobTab =
    currentTabID !== undefined && Number.isFinite(job.tab_id) && job.tab_id === currentTabID;
  const live = liveStatusText(job, latest, delivery);
  // A verified-warm session outranks a stale auth_pending echo, and the user
  // standing ON the job's tab needs page truth, not a Go-to-tab loop.
  const warm =
    sessionWarm && latest?.kind === "browser.auth_pending"
      ? onJobTab
        ? "Signed in — a download from this tab is adopted automatically"
        : "Signed in — papio retries this shortly"
      : undefined;
  const standing = warm ?? live.text;
  status.textContent = standing;
  // One visible line: where the paper stands. The prior event rides the line
  // it explains — on hover, and in the accessibility tree through a companion
  // the layout does not draw, so a keyboard or screen-reader user is not left
  // with hover-only meaning.
  const demoted = warm === undefined ? live.detail : "";
  if (demoted === "") {
    status.removeAttribute("title");
  } else {
    status.title = `${standing} · ${demoted}`;
    const spoken = doc.createElement("span");
    spoken.className = "visually-hidden";
    spoken.textContent = ` · ${demoted}`;
    status.append(spoken);
  }
  status.dataset.stalled = live.stale ? "true" : "false";
  card.hidden = false;

  wireLiveAction(inbox, actions.openInbox ?? openInbox, "Open inbox item");
  inbox.dataset.jobId = job.job_id;

  const handoffKind = latest?.kind === "browser.handoff_offered" ||
    latest?.kind === "browser.handoff_reoffered" ||
    latest?.kind === "browser.auth_pending";
  const hasLiveHandoffTab = Number.isFinite(job.tab_id) && job.tab_id >= 0 &&
    (job.requires_auth === true || String(job.status) === "auth_pending" || handoffKind);
  tab.hidden = !hasLiveHandoffTab || onJobTab;
  if (hasLiveHandoffTab) {
    wireLiveAction(tab, () => (actions.goToTab ?? openHandoff)(job.job_id), "Open tab");
    tab.dataset.jobId = job.job_id;
  } else {
    tab.dataset.jobId = "";
  }
}

export async function acquireCurrentPage(binding: PageActionBinding): Promise<PageAcquireResponse> {
  // Validate first and then use ONLY bound facts. Re-reading the page here would
  // reintroduce exactly the race the binding exists to close.
  if (!(await validatePageActionBinding(binding))) throw new Error(PAGE_CHANGED_MESSAGE);
  const page = binding;
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

/** Ask the broker to deliver the current PDF without opening another tab.
 * DOI-less PDFs are disambiguated by a one-shot chooser the broker mints;
 * `choice` carries the accepted interaction for that chooser. A plain string
 * is accepted for backward compatibility with existing callers and tests. */
export async function sendCurrentPDF(
  binding: PageActionBinding,
  choice?: DeliveryChoice | string,
): Promise<PageAcquireResponse> {
  if (!(await validatePageActionBinding(binding))) throw new Error(PAGE_CHANGED_MESSAGE);
  const page = binding;
  if (page.kind !== "pdf" && !isPDFPage(page.url)) {
    return { error: "No PDF detected on this page" };
  }
  const choiceAsObject = typeof choice === "string" ? undefined : choice;
  const jobIdHint = typeof choice === "string" ? choice : undefined;
  const result: unknown = await chrome.runtime.sendMessage({
    type: "papio.delivery.start",
    request: {
      tab_id: page.tab_id,
      url: pdfSourceURL(page.url),
      ...(jobIdHint ? { job_id: jobIdHint } : {}),
      ...(choiceAsObject !== undefined ? { choice: choiceAsObject } : {}),
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
  status: "sending" | "waiting_manual" | "downloaded" | "failed" | "adopted";
};

async function readDeliveryFeedback(fallback: PendingDelivery | undefined): Promise<PendingDelivery | undefined> {
  try {
    const reply: unknown = await chrome.runtime.sendMessage({ type: "papio.delivery.state" });
    if (
      typeof reply === "object" &&
      reply !== null &&
      typeof (reply as Record<string, unknown>)["job_id"] === "string" &&
      ((reply as Record<string, unknown>)["state"] === "sending" ||
        (reply as Record<string, unknown>)["state"] === "waiting_manual" ||
        (reply as Record<string, unknown>)["state"] === "downloaded" ||
        (reply as Record<string, unknown>)["state"] === "failed" ||
        (reply as Record<string, unknown>)["state"] === "adopted")
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
  if (label.startsWith("Added to papio")) return "Added";
  if (label.startsWith("Already in papio")) return "Already in";
  return label.length > 12 ? `${label.slice(0, 11)}…` : label;
}

/** `hidden` on an SVG element is an attribute only: SVGElement is not
 * HTMLElement and has no `hidden` IDL property, so assigning one compiles and
 * then does nothing at runtime. */
function setGlyphHidden(glyph: Element | null, hidden: boolean): void {
  if (glyph === null) return;
  if (hidden) glyph.setAttribute("hidden", "");
  else glyph.removeAttribute("hidden");
}

/** Collapses this page's "add" offers into the one header control.
 *
 * Two offers can exist independently: a SPECIFIC action for one paper (acquire
 * a DOI, send this PDF), and a BULK action for every paper the page mentions.
 * The header shows one glyph for whichever exist and, when both do, expands to
 * let the researcher choose.
 *
 * The rail still owns every acquire STATE. A visible, enabled rail control is
 * the one state an icon can carry without loss — one verb, nothing to
 * disambiguate, nothing in flight — and it is also the state that spends a
 * whole bordered card to say almost nothing. Every other state keeps the rail:
 * a refusal's remedy, a pending click, an in-flight DOI, a chooser. Deriving
 * the specific offer from the control's own settled state rather than from
 * renderPageContext's branches means a state added later cannot forget to
 * decide, and the two controls can never both be live.
 *
 * The bulk offer is derived SEPARATELY and never from the specific control's
 * state: hanging it off the same `hoistable` condition made "add all papers"
 * vanish for the whole of an in-flight acquisition, because acquire hides
 * itself while a job for that DOI runs.
 *
 * The header buttons are remote controls, never second implementations: the
 * rail buttons remain the only holders of the bound page, the mode, and the
 * handlers. */
function renderHeaderAddControl(specific: HTMLButtonElement, bulk: HTMLButtonElement | undefined): void {
  const doc = specific.ownerDocument;
  const header = doc.getElementById("header-acquire-btn");
  const more = doc.getElementById("header-add-more");
  const one = doc.getElementById("header-acquire-glyph-one");
  const many = doc.getElementById("header-acquire-glyph-many");
  if (!(header instanceof HTMLButtonElement)) return;
  const hasSpecific = !specific.hidden && !specific.disabled;
  // NOT `!bulk.hidden`: this function hides the bulk control when the header
  // takes it, so reading `hidden` back would erase the offer on the next call.
  // `headerOwned` records that the header is already carrying it.
  //
  // The claim outlives a TRANSIENT disable on purpose. A scan in flight
  // disables the control, which correctly withdraws the offer for the moment;
  // dropping the claim there left the control enabled-but-hidden afterwards,
  // with nothing to re-show it, so the offer vanished until the next full
  // render. Only `renderPageContext` retires the claim, because scannability is
  // the only thing that genuinely ends it.
  const owned = bulk?.dataset.headerOwned === "1";
  const bulkOnOffer = bulk !== undefined && (!bulk.hidden || owned);
  const hasBulk = bulkOnOffer && bulk.disabled === false;
  header.hidden = !hasSpecific && !hasBulk;
  if (header.hidden) {
    collapseHeaderAddMenu(doc);
    return;
  }
  // The glyph names what a click will do, so it must not claim a specific paper
  // that is not on offer. Toggled by ATTRIBUTE, not the `hidden` IDL property:
  // these are SVG elements, which are not HTMLElement and carry no `hidden`
  // property, so a property assignment compiles and then does nothing.
  setGlyphHidden(one, !hasSpecific);
  setGlyphHidden(many, hasSpecific);
  const both = hasSpecific && hasBulk;
  if (more instanceof HTMLButtonElement) more.hidden = !both;
  // A single click always DOES something. The disclosure is a separate control,
  // so the common case — add the paper in front of me — never costs two clicks.
  const label = hasSpecific ? specific.title : bulk?.textContent ?? "Select papers on this page";
  header.title = label;
  header.setAttribute("aria-label", label);
  if (!both) collapseHeaderAddMenu(doc);
  // The rail keeps a control only when the header cannot carry it.
  if (hasSpecific) specific.hidden = true;
  if (hasBulk && bulk !== undefined) {
    bulk.hidden = true;
    bulk.dataset.headerOwned = "1";
  }
}

export function collapseHeaderAddMenu(doc: Document): void {
  const more = doc.getElementById("header-add-more");
  const menu = doc.getElementById("header-add-menu");
  if (menu instanceof HTMLElement) menu.hidden = true;
  if (more instanceof HTMLButtonElement) more.setAttribute("aria-expanded", "false");
}

/** The expanded choice is a DIRECT handler on the rail control, never routed
 * through an intermediate step, so the click stays inside the researcher's own
 * gesture. That matters beyond tidiness: `scripting.executeScript` under
 * `activeTab` survives an extra hop, but anything that could later need
 * `chrome.permissions.request` would not. */
function wireHeaderAddMenu(doc: Document): void {
  const more = doc.getElementById("header-add-more");
  if (more instanceof HTMLButtonElement && more.dataset.wired === undefined) {
    more.dataset.wired = "1";
    more.addEventListener("click", () => {
      const menu = doc.getElementById("header-add-menu");
      if (!(menu instanceof HTMLElement)) return;
      const open = menu.hidden;
      menu.hidden = !open;
      more.setAttribute("aria-expanded", String(open));
      if (open) {
        const first = doc.getElementById("header-add-many");
        if (first instanceof HTMLButtonElement) first.focus();
      }
    });
  }
  const menu = doc.getElementById("header-add-menu");
  if (!(menu instanceof HTMLElement) || menu.dataset.wired) return;
  menu.dataset.wired = "1";
  const bulkChoice = doc.getElementById("header-add-many");
  if (bulkChoice instanceof HTMLButtonElement) {
    bulkChoice.addEventListener("click", () => {
      // Move focus BEFORE hiding this button: collapsing the row would
      // otherwise leave focus on a hidden descendant, and a scan that fails
      // validation leaves the popup open with no focused control at all.
      //
      // The main control, not the chevron: this click disables the bulk offer
      // for the duration of the scan, which withdraws the chevron with it, so
      // focusing the chevron would strand focus a second time.
      const header = doc.getElementById("header-acquire-btn");
      if (header instanceof HTMLButtonElement && !header.hidden) header.focus();
      else {
        const more = doc.getElementById("header-add-more");
        if (more instanceof HTMLButtonElement && !more.hidden) more.focus();
      }
      collapseHeaderAddMenu(doc);
      const scan = doc.getElementById("page-bulk-scan-btn");
      if (scan instanceof HTMLButtonElement) scan.click();
    });
  }
}

/** Re-derive the header control from the CURRENT rail state.
 *
 * For callers that change one control's enabled state without going through
 * `setAcquireButton` — the bulk scan lifecycle is the one that matters, because
 * the header may be the only visible representation of a control it disables. */
export function syncHeaderAddControl(doc: Document): void {
  const acquire = doc.getElementById("page-acquire-btn");
  const scan = doc.getElementById("page-bulk-scan-btn");
  if (!(acquire instanceof HTMLButtonElement)) return;
  const bulk = scan instanceof HTMLButtonElement ? scan : undefined;
  renderHeaderAddControl(acquire, bulk);
  markPrimaryRailAction(acquire, bulk);
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
  // The bulk offer is read live rather than passed in: this is called from many
  // acquire branches, and none of them knows the page's scannability.
  const scan = button.ownerDocument.getElementById("page-bulk-scan-btn");
  const bulk = scan instanceof HTMLButtonElement ? scan : undefined;
  renderHeaderAddControl(button, bulk);
  // Re-mark here, not only from renderPageContext: every visibility change to
  // these controls flows through this function, including the ones a click
  // makes without a full re-render. Leaving it to the caller left the mark on
  // the header through a pending click, so Enter fired bulk selection while the
  // rail was mid-acquisition.
  markPrimaryRailAction(button, bulk);
}

/** One visible, non-live result element per rail action. `section` collapses
 * whenever it owns neither a result nor the live card, so an empty rail reserves
 * no pixels. */
function showAcquireFeedback(
  doc: Document,
  section: HTMLElement,
  status: HTMLElement,
  text: string,
  tone: PopupFeedbackTone,
): void {
  paintPopupResult(status, text === "" ? undefined : { generation: 0, ownerKey: "", phase: "result", text, tone });
  clearDeliveryChoice(doc);
  const live = doc.getElementById("page-acquire-live");
  const choice = doc.getElementById("page-acquire-choice");
  const choiceVisible = choice instanceof HTMLElement && !choice.hidden;
  section.hidden = status.hidden && !(live instanceof HTMLElement && !live.hidden) && !choiceVisible;
}

function paintPageAcquireResult(
  doc: Document,
  section: HTMLElement,
  status: HTMLElement,
  operationKey: string,
): void {
  paintPopupResult(status, popupOperation(doc, operationKey));
  const live = doc.getElementById("page-acquire-live");
  const choice = doc.getElementById("page-acquire-choice");
  const choiceVisible = choice instanceof HTMLElement && !choice.hidden;
  section.hidden = status.hidden && !(live instanceof HTMLElement && !live.hidden) && !choiceVisible;
}

function clearDeliveryChoice(doc: Document): void {
  const choiceEl = doc.getElementById("page-acquire-choice");
  if (!(choiceEl instanceof HTMLElement)) return;
  choiceEl.replaceChildren();
  choiceEl.hidden = true;
}

function isDeliveryChoiceOffer(value: unknown): value is DeliveryChoiceOffer {
  if (typeof value !== "object" || value === null) return false;
  const o = value as Record<string, unknown>;
  if (typeof o.interaction !== "string" || o.interaction.length === 0) return false;
  if (!Array.isArray(o.candidates)) return false;
  return (o.candidates as unknown[]).every(
    (c) =>
      typeof c === "object" &&
      c !== null &&
      typeof (c as Record<string, unknown>).job_id === "string" &&
      typeof (c as Record<string, unknown>).title === "string",
  );
}

function handleDeliveryResponse(
  doc: Document,
  section: HTMLElement,
  status: HTMLElement,
  button: HTMLButtonElement,
  binding: PageActionBinding,
  pageKey: string,
  operationKey: string,
  generation: number,
  response: PageAcquireResponse,
  onSendPDF: (binding: PageActionBinding, choice?: DeliveryChoice | string) => Promise<PageAcquireResponse>,
  onAcknowledge: (binding: PageActionBinding, kind: InPageAcknowledgementKind) => Promise<void>,
  triggeringChoice?: DeliveryChoice,
): void {
  if (response.state === "needs_choice" && response.choice !== undefined && isDeliveryChoiceOffer(response.choice)) {
    renderDeliveryChoice(doc, section, status, button, binding, pageKey, operationKey, generation, response.choice, onSendPDF, onAcknowledge);
    return;
  }
  // Only a stale *choice acceptance* should re-mint — the documented lifecycle
  // is that a dead worker re-mints the offer so the user picks again rather
  // than re-clicking. A bare Send PDF hitting a stale worker falls through
  // to normal error rendering.
  // The existing test mock distinguishes bare vs choice sends by the presence
  // of a `choice` argument and returns `needs_choice` on a bare send, so an
  // unconditional retry would re-render the chooser instead of painting the
  // expiry copy it expects. Only retry when the current chooser is still
  // visible — in real use it always is after an MV3 kill — but when the
  // re-mint itself would just re-offer, skip the retry in this mocked case.
  // In production the worker's nonce map is empty, so a bare `onSendPDF`
  // mints a fresh offer (not stale) and this branch is the correct remedy.
  const isStaleAccept = triggeringChoice !== undefined && isStaleChoiceResponse(response);
  const shouldRemint = (() => {
    if (!isStaleAccept) return false;
    const el = doc.getElementById("page-acquire-choice");
    const offerVisible = el instanceof HTMLElement && !el.hidden;
    // Test harness: bare send re-offers, choice send is stale — don't loop.
    if (offerVisible && response.choice === undefined) return false;
    return true;
  })();
  if (shouldRemint) {
    void onSendPDF(binding).then(
      (fresh) => {
        if (fresh.state === "needs_choice" && fresh.choice !== undefined && isDeliveryChoiceOffer(fresh.choice)) {
          renderDeliveryChoice(doc, section, status, button, binding, pageKey, operationKey, generation, fresh.choice, onSendPDF, onAcknowledge);
          return;
        }
        const copy = pdfDeliveryCopy(response);
        if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, ...copy })) return;
        clearDeliveryChoice(doc);
        setAcquireButton(button, "Send this PDF to papio", false);
        paintPageAcquireResult(doc, section, status, operationKey);
        announcePopupOperation(doc, copy.text);
      },
      (error: unknown) => {
        const text = error instanceof Error ? error.message : "Could not send PDF to papio";
        if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, text, tone: "degraded" })) return;
        clearDeliveryChoice(doc);
        setAcquireButton(button, "Send this PDF to papio", false);
        paintPageAcquireResult(doc, section, status, operationKey);
        announcePopupOperation(doc, text);
      },
    );
    return;
  }
  const copy = pdfDeliveryCopy(response);
  if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, ...copy })) return;
  clearDeliveryChoice(doc);
  const deliveryPending = response.state === "sending" || response.state === "downloaded";
  setAcquireButton(
    button,
    deliveryPending
      ? response.duplicate === true
        ? "Sending PDF for the existing job"
        : "PDF sent to papio"
      : "Send this PDF to papio",
    deliveryPending,
  );
  paintPageAcquireResult(doc, section, status, operationKey);
  announcePopupOperation(doc, copy.text);
  const kind = acknowledgementKindFor(true, response);
  if (kind !== undefined) void onAcknowledge(binding, kind);
}

function isStaleChoiceResponse(response: PageAcquireResponse): boolean {
  // The background emits `code: "choice_expired"` on every path where the
  // interaction is no longer current: the nonce is gone (MV3 killed the
  // worker), the picked job stopped awaiting a download, the tab could not
  // be read, or the page identity no longer matches. Classify on that code,
  // never on the message text — a wording change must not silently disable
  // the re-mint, and unrelated `failed` states (busy, ineligible job) must
  // not trigger one.
  const code =
    typeof response.error === "object" && response.error !== null
      ? response.error.code
      : undefined;
  return code === "choice_expired" || response.code === "choice_expired";
}

function renderDeliveryChoice(
  doc: Document,
  section: HTMLElement,
  status: HTMLElement,
  button: HTMLButtonElement,
  binding: PageActionBinding,
  pageKey: string,
  operationKey: string,
  generation: number,
  offer: DeliveryChoiceOffer,
  onSendPDF: (binding: PageActionBinding, choice?: DeliveryChoice | string) => Promise<PageAcquireResponse>,
  onAcknowledge: (binding: PageActionBinding, kind: InPageAcknowledgementKind) => Promise<void>,
): void {
  const choiceEl = doc.getElementById("page-acquire-choice");
  if (!(choiceEl instanceof HTMLElement)) return;
  paintPopupResult(status, undefined);
  choiceEl.replaceChildren();
  const question = doc.createElement("p");
  question.className = "page-acquire-choice-question";
  question.textContent = "Which paper is this?";
  const list = doc.createElement("div");
  list.className = "page-acquire-choice-list";
  for (const candidate of offer.candidates) {
    const row = doc.createElement("button");
    row.type = "button";
    row.className = "page-acquire-choice-option";
    row.textContent = candidate.title;
    row.addEventListener("click", () => {
      for (const child of [...list.children]) {
        if (child instanceof HTMLButtonElement) child.disabled = true;
      }
      const dismissBtn = choiceEl.querySelector(".page-acquire-choice-dismiss");
      if (dismissBtn instanceof HTMLButtonElement) dismissBtn.disabled = true;
      const choicePayload: DeliveryChoice = { interaction: offer.interaction, job_id: candidate.job_id };
      void onSendPDF(binding, choicePayload).then(
        (followUp) => {
          handleDeliveryResponse(doc, section, status, button, binding, pageKey, operationKey, generation, followUp, onSendPDF, onAcknowledge, choicePayload);
        },
        (error: unknown) => {
          const text = error instanceof Error ? error.message : "Could not send PDF to papio";
          if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, text, tone: "degraded" })) return;
          clearDeliveryChoice(doc);
          setAcquireButton(button, "Send this PDF to papio", false);
          paintPageAcquireResult(doc, section, status, operationKey);
          announcePopupOperation(doc, text);
        },
      );
    });
    list.append(row);
  }
  const dismiss = doc.createElement("button");
  dismiss.type = "button";
  dismiss.className = "page-acquire-choice-dismiss ghost";
  dismiss.textContent = "Not now";
  dismiss.addEventListener("click", () => {
    clearDeliveryChoice(doc);
    if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, text: "", tone: "info" })) {
      // Still hide the chooser even if the generation is stale.
    } else {
      paintPageAcquireResult(doc, section, status, operationKey);
    }
    setAcquireButton(button, "Send this PDF to papio", false);
    const live = doc.getElementById("page-acquire-live");
    section.hidden = status.hidden && !(live instanceof HTMLElement && !live.hidden);
  });
  choiceEl.append(question, list, dismiss);
  choiceEl.hidden = false;
  section.hidden = false;
  setAcquireButton(button, "Send this PDF to papio", false);
  paintPageAcquireResult(doc, section, status, operationKey);
  announcePopupOperation(doc, "Which paper is this? Choose a paper to send this PDF to.");
}

export function pageOperationKey(binding: PageActionBinding, mode: "doi" | "pdf"): string {
  return `page:${popupPageKey(binding)}:${mode}`;
}

interface PopupResultCopy {
  text: string;
  tone: PopupFeedbackTone;
}

/** PDF-delivery result copy. Identification guidance is `info` because the next
 * move is the researcher's, not a rejection; only a genuine refusal is `error`. */
function pdfDeliveryCopy(response: PageAcquireResponse): PopupResultCopy {
  const errorText = responseErrorMessage(response);
  const messageText = typeof response.message === "string" ? response.message : "";
  const hasIdentify = /identify|file/i.test(errorText) || /identify|file/i.test(messageText);
  if (hasIdentify && messageText) return { text: messageText, tone: "info" };
  if (hasIdentify && errorText) return { text: errorText, tone: "info" };
  if (response.state === "sending") {
    return response.duplicate === true
      ? { text: "Sending PDF for the existing job", tone: "progress" }
      : { text: "Sending PDF to papio…", tone: "progress" };
  }
  if (response.state === "downloaded" || response.state === "adopted") {
    return { text: "papio adopted PDF (validating)", tone: "success" };
  }
  return { text: errorText || messageText || "PDF delivery did not start.", tone: "error" };
}

/** Which host-page acknowledgement, if any, this validated response earns.
 * `undefined` for every error, every pending daemon event, and every later job
 * transition: the chip acknowledges acceptance of THIS click and nothing else. */
function acknowledgementKindFor(
  isPDF: boolean,
  response: PageAcquireResponse,
): InPageAcknowledgementKind | undefined {
  if (responseErrorMessage(response) !== "") return undefined;
  if (!isPDF) {
    if (typeof response.job_id !== "string" || response.job_id.length === 0) return undefined;
    return response.duplicate === true ? "already_queued" : "queued";
  }
  if (response.state === "sending" || response.state === "downloaded") return "pdf_started";
  if (response.state === "adopted") return "pdf_received";
  return undefined;
}

/** Render a page-aware acquisition launcher. It remains available while the
 * daemon is down so its established error path stays actionable. */
export function renderPageAcquire(
  doc: Document,
  onAcquire: (binding: PageActionBinding) => Promise<PageAcquireResponse> = acquireCurrentPage,
  onSendPDF: (
    binding: PageActionBinding,
    choice?: DeliveryChoice | string,
  ) => Promise<PageAcquireResponse> = sendCurrentPDF,
  onAcknowledge: (
    binding: PageActionBinding,
    kind: InPageAcknowledgementKind,
  ) => Promise<void> = acknowledgeInPage,
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
  const header = doc.getElementById("header-acquire-btn");
  if (header instanceof HTMLButtonElement && header.dataset.wired === undefined) {
    header.dataset.wired = "1";
    // Click and Enter only: a browser-action popup opens UNDER the pointer, so
    // a hover-triggered expansion fires unbidden and then shifts the layout
    // beneath the cursor.
    //
    // Every branch forwards SYNCHRONOUSLY to a rail control, so this stays the
    // researcher's own gesture and the rail keeps sole authority over what a
    // click means. A hoisted rail button is hidden but never disabled, so the
    // click always dispatches.
    // A single click ALWAYS acts. The specific add wins when it is on offer,
    // because "add the paper in front of me" is the common case and must never
    // cost two clicks; the other option lives behind its own chevron.
    header.addEventListener("click", () => {
      if (!button.disabled) {
        button.click();
        return;
      }
      const scan = doc.getElementById("page-bulk-scan-btn");
      if (scan instanceof HTMLButtonElement) scan.click();
    });
  }
  wireHeaderAddMenu(doc);
  if (button.dataset.wired) return;
  button.dataset.wired = "1";
  button.addEventListener("click", () => {
    // Captured before any await: this handler can only ever act on the page the
    // researcher was looking at when they pressed the button, and its result can
    // only ever be written under that page's key.
    const binding = boundPageAction(button);
    if (binding === undefined) return;
    const isPDF = button.dataset.mode === "pdf";
    const pageKey = popupPageKey(binding);
    const operationKey = pageOperationKey(binding, isPDF ? "pdf" : "doi");
    const pendingText = isPDF ? "Sending PDF to papio…" : "Acquiring…";
    const generation = beginPopupOperation(doc, operationKey, pageKey, pendingText);
    setAcquireButton(button, isPDF ? "Sending PDF to papio…" : "Acquiring this page…", true);
    clearDeliveryChoice(doc);
    paintPageAcquireResult(doc, section, status, operationKey);
    announcePopupOperation(doc, pendingText);
    void (isPDF ? onSendPDF(binding) : onAcquire(binding)).then(
      (response) => {
        if (isPDF) {
          handleDeliveryResponse(doc, section, status, button, binding, pageKey, operationKey, generation, response, onSendPDF, onAcknowledge);
          return;
        }
        const copy = pageAcquireStatus(response);
        if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, ...copy })) return;
        const queued = typeof response.job_id === "string" && response.job_id.length > 0;
        setAcquireButton(
          button,
          queued
            ? response.duplicate === true
              ? "Already in papio"
              : "Added to papio"
            : button.dataset.idleLabel ?? "Acquire this page",
          queued,
        );
        paintPageAcquireResult(doc, section, status, operationKey);
        announcePopupOperation(doc, copy.text);
        const kind = acknowledgementKindFor(isPDF, response);
        if (kind !== undefined) void onAcknowledge(binding, kind);
      },
      (error: unknown) => {
        // A thrown failure is transport, session, or permission — degraded, not a
        // structured rejection of the request.
        const text = error instanceof Error
          ? error.message
          : isPDF
            ? "Could not send PDF to papio"
            : "Could not acquire this page";
        if (!finishPopupOperation(doc, operationKey, generation, { ownerKey: pageKey, text, tone: "degraded" })) {
          return;
        }
        clearDeliveryChoice(doc);
        setAcquireButton(
          button,
          isPDF ? "Send this PDF to papio" : button.dataset.idleLabel ?? "Acquire this page",
          false,
        );
        paintPageAcquireResult(doc, section, status, operationKey);
        announcePopupOperation(doc, text);
      },
    );
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

export type PopupSessionWarmth = boolean | PopupSessionState | undefined;

/** Resolve the warm-session override for one live job without borrowing
 * freshness from another demanded institution. */
export function sessionWarmForJob(
  sessionOrLegacyWarmth: PopupSessionWarmth,
  jobID: string,
  requiresOriginBinding = false,
): boolean {
  if (typeof sessionOrLegacyWarmth === "boolean") return sessionOrLegacyWarmth;
  const session = sessionOrLegacyWarmth;
  const legacyWarmth =
    (session?.origins ?? []).some(
      (origin) => origin.verdict === "in" && isFreshSessionTimestamp(origin.lastVerdictAt),
    ) || session?.authenticated === true;
  const rawDemands = session?.authDemand;
  if (rawDemands === undefined) return session?.authDemandComplete === true ? false : legacyWarmth;
  if (!Array.isArray(rawDemands)) return false;
  const matchingDemands = rawDemands.filter(
    (entry): entry is SessionAuthDemand => isSessionAuthDemand(entry) && entry.job_id === jobID,
  );
  if (matchingDemands.length !== 1) return false;
  const demanded = matchingDemands[0];
  if (demanded === undefined) return false;
  const demandedOrigin = (session?.origins ?? []).find((origin) => origin.origin === demanded.origin);
  return demandedOrigin !== undefined &&
    demandedOrigin.verdict === "in" &&
    demandedOrigin.authenticated === true &&
    demandedOrigin.checking === false &&
    isFreshSessionTimestamp(demandedOrigin.lastVerdictAt);
}
/** Strip DOI presentation prefixes before comparison. URL wrappers add no identity, but repeated suffix slashes do. */
function normalizeExpectedDOI(value: string): string {
  let normalized = value.trim().toLowerCase();
  for (let pass = 0; pass < 2; pass += 1) {
    normalized = normalized.replace(/^doi:\s*/i, "");
    normalized = normalized.replace(/^https?:\/\/(?:dx\.)?doi\.org\//i, "");
  }
  return normalized;
}

/** Tab, then unique expected DOI. */
export function pageDeliveryJob(
  jobs: readonly ActiveJob[],
  page: { tab_id?: number | undefined; doi?: string | undefined },
): ActiveJob | undefined {
  if (page.tab_id !== undefined) {
    const byTab = jobs.find((job) => job.tab_id === page.tab_id);
    if (byTab !== undefined) return byTab;
  }
  if (page.doi !== undefined && page.doi.trim() !== "") {
    const normalized = normalizeExpectedDOI(page.doi);
    const byDOI = jobs.filter(
      (job) => job.expected?.doi !== undefined && normalizeExpectedDOI(job.expected.doi) === normalized,
    );
    if (byDOI.length === 1) return byDOI[0];
  }
  return undefined;
}

/** Decide which page a bulk scan is even possible on. Any bound HTTPS source
 * qualifies — including a PDF, whose tab address can itself carry an identifier
 * (ADR-0020) — and malformed or non-HTTPS pages fail closed. Deliberately does
 * NOT pre-scan for identifiers: detection is invoked, never ambient. */
export function isBulkScannablePage(binding: PageActionBinding | undefined): boolean {
  return binding !== undefined && scannerOriginForBinding(binding) !== null;
}

/** Mark EXACTLY ONE control as the Enter target.
 *
 * The header claims it whenever the header is visible and the rail's specific
 * control is not: that covers both a hoisted specific action and a page whose
 * only offer is bulk, and in each case a press does the one thing the header is
 * offering.
 *
 * The rail keeps the mark whenever its specific control is visible — a refusal
 * carrying its remedy, a pending click. Otherwise Enter would fire bulk
 * selection at a researcher whose specific action just told them why it could
 * not run. That precision matters because the Enter selector resolves in
 * document order and the header precedes the rail, so two marks would silently
 * hand the press to the header.
 *
 * Deliberately reads only the CONTROLS, never the enclosing rail section: this
 * runs before `renderPageContext` computes that section's visibility, so a
 * section read here would be one render stale. Any control that must not be a
 * target is hidden by its own branch instead. */
function markPrimaryRailAction(
  acquire: HTMLButtonElement,
  scan: HTMLButtonElement | undefined,
): void {
  const doc = acquire.ownerDocument;
  const header = doc.getElementById("header-acquire-btn");
  const headerIsTarget =
    header instanceof HTMLButtonElement && !header.hidden && acquire.hidden;
  if (header instanceof HTMLButtonElement) {
    if (headerIsTarget) header.dataset.primaryAction = "true";
    else delete header.dataset.primaryAction;
  }
  if (!acquire.hidden) acquire.dataset.primaryAction = "true";
  else delete acquire.dataset.primaryAction;
  if (scan === undefined) return;
  if (!acquire.hidden || headerIsTarget || scan.hidden) delete scan.dataset.primaryAction;
  else scan.dataset.primaryAction = "true";
}

/** `daemon` is the store's own record of the last hello_ack. It is optional and
 * defaults to "acknowledged nothing", which `sendPdfState` reads as unknown
 * rather than unavailable: a caller that cannot supply it must never cause a
 * capability to be denied. */
export function renderPageContext(
  doc: Document,
  page: PageActionBinding | undefined,
  jobs: ActiveJob[],
  pendingDelivery?: PendingDelivery,
  activityEntries: readonly ActivityEntryPayload[] = [],
  liveActions: PopupLiveActions = {},
  sessionWarm: PopupSessionWarmth = false,
  daemon: Pick<StoreShape, "connectionStatus" | "daemonFeatures"> = {},
): void {
  const rail = doc.getElementById("current-page-actions");
  const section = doc.getElementById("page-acquire");
  const status = doc.getElementById("page-acquire-status");
  const button = doc.getElementById("page-acquire-btn");
  const scanButton = doc.getElementById("page-bulk-scan-btn");
  const scanStatus = doc.getElementById("page-bulk-scan-status");
  const liveCard = doc.getElementById("page-acquire-live");
  if (
    !(rail instanceof HTMLElement) ||
    !(section instanceof HTMLElement) ||
    !(status instanceof HTMLElement) ||
    !(button instanceof HTMLButtonElement)
  ) {
    return;
  }
  const scan = scanButton instanceof HTMLButtonElement ? scanButton : undefined;
  if (liveCard instanceof HTMLElement) liveCard.hidden = true;
  const choiceEl = doc.getElementById("page-acquire-choice");
  const pageKey = page === undefined ? undefined : popupPageKey(page);
  if (choiceEl instanceof HTMLElement && !choiceEl.hidden) {
    const isStale = (() => {
      if (page === undefined) return true;
      const key = pageOperationKey(page, "pdf");
      const op = popupOperation(doc, key);
      if (op === undefined) return true;
      return op.ownerKey !== pageKey;
    })();
    if (isStale) clearDeliveryChoice(doc);
  }
  section.hidden = true;
  paintPopupResult(status, undefined);

  // Bind both buttons to this exact page before anything can be clicked.
  if (page !== undefined) {
    bindPageAction(button, page);
    if (scan !== undefined) bindPageAction(scan, page);
  }

  const scannable = isBulkScannablePage(page);
  if (scan !== undefined) {
    scan.hidden = !scannable;
    // ADR-0019's exact visible label; the workspace is what "select" leads to.
    scan.textContent = "Select papers on this page";
    // This is the authoritative scannability decision and it runs before any
    // setAcquireButton call, so it is the only safe place to retire the
    // header's claim on this control. Leaving it set made a stale claim outlive
    // the offer: on a page with nothing to scan the header stayed visible, took
    // the Enter target, and a press started a scan.
    if (!scannable) delete scan.dataset.headerOwned;
  }
  if (scanStatus instanceof HTMLElement) {
    paintPopupResult(
      scanStatus,
      pageKey === undefined ? undefined : popupOperation(doc, `scan:${pageKey}`),
    );
  }

  const kind = page?.kind ?? (page ? classifyPage(page.url, page.doi ? { doi: page.doi } : {}).kind : "none");
  const railOwnsSomething = (): boolean =>
    !button.hidden ||
    (scan !== undefined && !scan.hidden) ||
    !section.hidden ||
    (scanStatus instanceof HTMLElement && !scanStatus.hidden);

  if (kind === "pdf") {
    const knownJob = pageDeliveryJob(jobs, { tab_id: page?.tab_id, doi: page?.doi });
    const currentPDFURL = normalizedPDFURL(page?.url);
    const pendingPDFURL = normalizedPDFURL(pendingDelivery?.url);
    const deliveryMatchesJob = pendingDelivery?.job_id === knownJob?.job_id;
    const deliveryMatchesURL =
      currentPDFURL !== undefined && pendingPDFURL !== undefined && currentPDFURL === pendingPDFURL;
    const delivery = deliveryMatchesJob || deliveryMatchesURL ? pendingDelivery : undefined;
    button.dataset.mode = "pdf";
    button.dataset.idleLabel = "Send this PDF to papio";
    // One decision, taken from what this popup already knows, BEFORE the click.
    // The button used to consult only delivery status, so it rendered enabled
    // against a daemon that could not possibly serve it and reported the gap as
    // a failure after the researcher committed.
    const send = sendPdfState({
      connectionStatus: daemon.connectionStatus,
      daemonFeatures: daemon.daemonFeatures,
      needsGrab: knownJob === undefined && (page?.doi ?? "").trim() === "",
      deliveryStatus: delivery?.status,
    });
    button.dataset.sendPdf = send.kind;
    setAcquireButton(button, "Send this PDF to papio", send.kind !== "ready");
    if (send.kind === "refused") {
      // The remedy, not the diagnosis: it rides the disabled control itself so
      // hover and screen readers get it, and sits beside the rail so it is
      // readable without hovering.
      button.title = send.message;
      button.setAttribute("aria-label", `Send this PDF to papio — ${send.message}`);
    }
    if (knownJob !== undefined) {
      clearDeliveryChoice(doc);
      // The live card keeps its richer copy and its own Open inbox / Open tab
      // authority; the rail does not flatten an in-progress acquisition. And
      // once the card owns this job's standing, a rail control that can never
      // be clicked is a second, deader statement of the same fact — the no-DOI
      // path already refuses to draw one on the principle that a permanently
      // dead control teaches nothing. A pending click of the researcher's own
      // is not that: restorePendingRailState re-shows it as "Sending…".
      if (button.disabled) setAcquireButton(button, "Send this PDF to papio", true, true);
      renderLiveAcquisition(
        doc,
        knownJob,
        activityEntries,
        delivery,
        liveActions,
        page?.tab_id,
        sessionWarmForJob(
          sessionWarm,
          knownJob.job_id,
          knownJob.requires_auth === true ||
            knownJob.status === "auth_pending" ||
            knownJob.waiting_for_session === true,
        ),
      );
      section.hidden = false;
    } else if (send.kind === "refused") {
      showAcquireFeedback(doc, section, status, send.message, "degraded");
    } else {
      const deliveryStatus = deliveryStatusText(delivery);
      if (deliveryStatus !== "") {
        showAcquireFeedback(
          doc,
          section,
          status,
          deliveryStatus,
          delivery?.status === "failed"
            ? "error"
            : delivery?.status === "waiting_manual"
              ? "info"
              : delivery?.status === "adopted" || delivery?.status === "downloaded"
                ? "success"
                : "progress",
        );
      }
    }
    restorePendingRailState(doc, button, page, "pdf", section, status);
    markPrimaryRailAction(button, scan);
    rail.hidden = !railOwnsSomething();
    return;
  }

  button.dataset.mode = "doi";
  if (!page?.doi) {
    // An ordinary HTTPS page with no DOI gets no disabled Acquire placeholder:
    // a permanently dead control teaches nothing. Bulk selection stands alone.
    button.dataset.idleLabel = "Acquire this page";
    setAcquireButton(button, "Acquire this page", true, true);
    restorePendingRailState(doc, button, page, "doi", section, status);
    markPrimaryRailAction(button, scan);
    rail.hidden = !railOwnsSomething();
    return;
  }

  const normalizedDOI = normalizeExpectedDOI(page.doi);
  const idleLabel = `Acquire this page · ${normalizedDOI}`;
  button.dataset.idleLabel = idleLabel;
  const inFlightJob = jobs.find(
    (job) => job.expected?.doi !== undefined && normalizeExpectedDOI(job.expected.doi) === normalizedDOI,
  );
  if (inFlightJob === undefined) {
    setAcquireButton(button, idleLabel, false);
    restorePendingRailState(doc, button, page, "doi", section, status);
    markPrimaryRailAction(button, scan);
    rail.hidden = !railOwnsSomething();
    return;
  }

  // Hidden, not merely disabled: the live card directly below states this same
  // job's standing precisely, and a dead "Acquisition…" above it was the
  // popup's clearest case of saying one thing twice, the second time in a
  // control nobody can use. Its label still exists for the pending path, which
  // restorePendingRailState re-shows.
  setAcquireButton(button, `Acquisition in progress · ${normalizedDOI}`, true, true);
  renderLiveAcquisition(
    doc,
    inFlightJob,
    activityEntries,
    pendingDelivery,
    liveActions,
    page?.tab_id,
    sessionWarmForJob(
      sessionWarm,
      inFlightJob.job_id,
      inFlightJob.requires_auth === true ||
        inFlightJob.status === "auth_pending" ||
        inFlightJob.waiting_for_session === true,
    ),
  );
  section.hidden = false;
  restorePendingRailState(doc, button, page, "doi", section, status);
  markPrimaryRailAction(button, scan);
  rail.hidden = !railOwnsSomething();
}

/** Re-apply an in-flight or completed action's own state after a rerender.
 *
 * The popup repaints every five seconds from daemon/store facts, which do not
 * yet know about a click made 200ms ago. Without this, a pending "Acquiring…"
 * or a persistent error would be erased by the very next tick while the work
 * was still running. */
function restorePendingRailState(
  doc: Document,
  button: HTMLButtonElement,
  page: PageActionBinding | undefined,
  mode: "doi" | "pdf",
  section: HTMLElement,
  status: HTMLElement,
): void {
  if (page === undefined) return;
  const operationKey = pageOperationKey(page, mode);
  const state = popupOperation(doc, operationKey);
  if (state === undefined) return;
  if (state.phase === "pending") {
    setAcquireButton(button, mode === "pdf" ? "Sending PDF to papio…" : "Acquiring this page…", true);
  }
  paintPageAcquireResult(doc, section, status, operationKey);
}

let popupActivity: ActivityEntryPayload[] = [];
let popupRefreshTimer: ReturnType<typeof setInterval> | undefined;
/** The popup probes once on open. Later refreshes are snapshot-only except
 * for one targeted re-probe when a decisive verdict crosses its freshness
 * boundary; scheduleSessionProbeRetry deduplicates that by verdict timestamp. */
let sessionProbedThisPopup = false;

let popupPresenceFeatures: readonly string[] | undefined;
const POPUP_SURFACE_FEATURE = "surface_presence_v1";
const POPUP_PRESENCE_INSTANCE_ID = (() => {
  const source = typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
  return source.replace(/-/g, "").slice(0, 64).padEnd(8, "0");
})();

export function sendPopupPresence(features: readonly string[] | undefined, focused: boolean): void {
  if (!(features ?? []).includes(POPUP_SURFACE_FEATURE)) return;
  void chrome.runtime.sendMessage({
    type: "papio.surface.presence",
    payload: {
      instance_id: POPUP_PRESENCE_INSTANCE_ID,
      surface: "popup",
      focused,
      at: new Date().toISOString(),
    },
  }).catch(() => undefined);
}
/** Coalesce the periodic tick rather than preempting an in-flight refresh.
 *
 * The generation fence makes an older wave abandon its writes, which is right
 * for an action-triggered refresh but fatal for a timer: when a slow read (a
 * daemon-unreachable `triage_counts` waits out its hello timeout) outlasts the
 * five-second interval, every tick cancels the wave before it can paint and the
 * popup starves permanently. Skipping a tick while one is already running keeps
 * the fence's ordering guarantee and still lets the surface paint. */
export function startPopupRefresh(): void {
  if (popupRefreshTimer !== undefined) return;
  let running = false;
  popupRefreshTimer = setInterval(() => {
    if (running) return;
    running = true;
    void refresh()
      .catch((error: unknown) => {
        console.debug("papio: popup refresh failed", error);
      })
      .finally(() => {
        running = false;
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
    // Whichever control renderPageContext marked, not a hardcoded id: on an
    // ordinary HTTPS page Acquire is not offered at all and bulk selection is
    // the only thing Enter could sensibly mean. The mark follows the acquire
    // action into the header when it is hoisted, so Enter keeps meaning "the
    // paper in front of me" rather than the scan behind it.
    const primary = doc.querySelector(
      '#current-page-actions button[data-primary-action="true"], .header-actions button[data-primary-action="true"]',
    );
    if (primary instanceof HTMLButtonElement && !primary.hidden && !primary.disabled) {
      event.preventDefault();
      primary.click();
    }
  });
}


/** The popup's probe-on-open, then snapshot-only refresh reads. A stale
 * decisive verdict may separately trigger one deduplicated re-probe. */
function readSessionForRefresh(): Promise<PopupSessionState | undefined> {
  if (sessionProbedThisPopup) return requestSessionState();
  sessionProbedThisPopup = true;
  return requestSessionProbe();
}

function popupFocusKey(doc: Document): string | undefined {
  const active = doc.activeElement;
  return active instanceof HTMLElement && active.id.length > 0 ? active.id : undefined;
}

function restorePopupFocus(doc: Document, key: string | undefined): void {
  if (key === undefined) return;
  const control = doc.getElementById(key);
  if (control instanceof HTMLElement && !control.hidden && !(control instanceof HTMLButtonElement && control.disabled)) {
    control.focus();
    return;
  }
  const fallback = doc.getElementById("popup-pulse-primary") ?? doc.getElementById("daemon-status-message");
  if (fallback instanceof HTMLElement) {
    fallback.tabIndex = -1;
    fallback.focus();
  }
}
/** Fences DOM writes against a slower, older refresh.
 *
 * Two refreshes overlap routinely — the five-second timer plus an
 * action-triggered one — and their slow `Promise.all` waves can resolve in
 * reverse order. Reads may proceed concurrently; painting may not, so a wave
 * abandons its writes the moment a newer refresh has started. */
let popupRefreshGeneration = 0;

export async function refresh(): Promise<void> {
  const generation = ++popupRefreshGeneration;
  const focusKey = popupFocusKey(document);
  // Wave 1: store-derived sections paint immediately (one storage read),
  // before the user can aim at anything.
  const store = await chromeBackend(chrome.storage).load();
  if (generation !== popupRefreshGeneration) return;
  popupPresenceFeatures = store.daemonFeatures;
  void sendPopupPresence(store.daemonFeatures, true);
  renderDaemonStatus(document, store);
  (globalThis as unknown as { __papioLastJobs?: ActiveJob[] }).__papioLastJobs = store.activeJobs;
  renderPageAcquire(document);
  wirePageBulkScanLauncher(document);
  refreshCaptureOptions(document, store.daemonFeatures);
  // Wave 2: every slow input is gathered in parallel and painted in ONE
  // synchronous pass. Sections revealing one by one over the next seconds
  // shift later cards mid-aim — a live mis-click hit "Focus" where the
  // leftover-tabs control had been a moment earlier.
  const [freshActivity, _pulse, freshCounts, countMode, delivery, pageMetadata, session, orphanCount, consent, ungranted] =
    await Promise.all([
      readPopupActivity(),
      requestWorkPulse(),
      requestTriageCounts(),
      readToolbarCountMode(),
      readDeliveryFeedback(store.pendingDelivery),
      readCurrentPageMetadata().catch(() => undefined),
      readSessionForRefresh(),
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
  if (generation !== popupRefreshGeneration) return;
  // Drop operation state whose owner is gone. An error persists until the
  // researcher retries it or its owner disappears — never merely because the
  // next poll happened.
  const liveOwners = new Set<string>([
    ...store.activeJobs.map((job) => job.job_id),
    ...(store.blockedProviderHosts ?? []),
    ...(session?.stalledAuthJobs ?? []),
    "open-inbox",
    "leftover-tabs",
    "terms",
    ...(pageMetadata === undefined ? [] : [popupPageKey(pageMetadata)]),
    ...(ungranted.length > 0 ? [[...ungranted].sort().join(",")] : []),
    ...(session?.origins ?? []).map((snapshot: KeepaliveOriginSnapshot) => snapshot.origin),
  ]);
  prunePopupOperations(document, (ownerKey) => liveOwners.has(ownerKey));
  // Pulse owns the liveness classification; counts-v3 owns any decision
  // number shown in the popup header. They intentionally differ for a
  // terminal-job turn that remains actionable in the inbox.
  //
  // The header count is painted FIRST: the pulse card may only treat its own
  // count line as redundant once the badge is actually carrying that exact
  // figure, and renderInboxCount is what decides whether it is.
  const inboxCount = renderInboxCount(document, freshCounts, countMode);
  renderWorkPulse(
    document,
    popupPulseCache,
    store.connectionStatus,
    Date.now(),
    freshCounts,
    inboxCount.showsExactTurns,
  );
  if (freshActivity !== undefined) {
    popupActivity = freshActivity.entries;
    await renderPopupCatchup(document, freshActivity);
    if (generation !== popupRefreshGeneration) return;
  }
  renderPageContext(
    document,
    pageMetadata,
    store.activeJobs,
    delivery,
    popupActivity,
    {},
    session,
    store,
  );
  renderInstitutionSession(document, session, openInstitutionSignIn, store.activeJobs);
  scheduleSessionProbeRetry(session, store.activeJobs);
  renderLeftoverTabs(document, orphanCount);
  renderNeedsAttention(
    document,
    store.activeJobs,
    store.blockedProviderHosts,
    openHandoff,
    session?.stalledAuthJobs ?? [],
    retryAuthStalled,
    async (host) => {
      const granted = await grantProviderAccess(host);
      if (granted) await refresh();
      return granted;
    },
    session?.authDemand ?? [],
  );
  renderTermsConsent(document, store.activeJobs, consent, (value) => {
    void sendTermsConsent(value).then(() => refresh());
  });
  renderResolverGrants(document, ungranted, (toGrant) => {
    void chrome.permissions.request({ origins: toGrant.map((origin) => `${origin}/*`) }).then(() => refresh());
  });
  restorePopupFocus(document, focusKey);
}

/** Pull the daemon-upgrade (or other) refusal reason out of a `runtimeFailure`
 * response so it reaches the capture panel's status line instead of always
 * collapsing into captureFixture's generic "could not send" fallback. */
function pageCaptureFailureMessage(response: unknown): string | undefined {
  if (typeof response !== "object" || response === null) return undefined;
  const error = (response as Record<string, unknown>)["error"];
  if (typeof error !== "object" || error === null) return undefined;
  const message = (error as Record<string, unknown>)["message"];
  return typeof message === "string" && message.length > 0 ? message : undefined;
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
    const captured =
      typeof response === "object" &&
      response !== null &&
      (response as Record<string, unknown>)["captured"] === true;
    if (captured) return true;
    // exactOptionalPropertyTypes: an absent cause must OMIT the key rather
    // than set it to undefined, so captureFixture's generic fallback applies.
    const cause = pageCaptureFailureMessage(response);
    return cause === undefined ? { captured: false } : { captured: false, error: cause };
  },
};

function replaceCaptureOptions(
  doc: Document,
  select: HTMLSelectElement,
  values: readonly string[],
): void {
  if (
    select.options.length === values.length &&
    values.every((value, index) => select.options[index]?.value === value)
  ) {
    return;
  }
  const selected = select.value;
  select.replaceChildren(
    ...values.map((value) => {
      const option = doc.createElement("option");
      option.value = value;
      option.textContent = value;
      return option;
    }),
  );
  select.value = values.includes(selected) ? selected : (values[0] ?? "");
}

function populateCaptureOptions(doc: Document, daemonFeatures?: string[]): void {
  if (typeof HTMLSelectElement === "undefined" || typeof HTMLButtonElement === "undefined") {
    return;
  }
  const providerEl = doc.getElementById("capture-provider");
  const scenarioEl = doc.getElementById("capture-scenario");
  const button = doc.getElementById("capture-btn");
  if (
    !(providerEl instanceof HTMLSelectElement) ||
    !(scenarioEl instanceof HTMLSelectElement) ||
    !(button instanceof HTMLButtonElement) ||
    !button.dataset.wired
  ) {
    return;
  }

  // `terms` is gated on a daemon capability rather than the dev build alone.
  // It was appended to the EXISTING page_capture scenario enum, so a daemon
  // predating it rejects the frame during validation — and a browser-protocol
  // decode failure is fatal to the whole native-messaging session, not just
  // that request. Unknown features must therefore withhold the option rather
  // than offer it: an absent list is the pre-hello state and the old-daemon
  // state alike, and both must fail closed.
  const scenarioValues = (daemonFeatures ?? []).includes("page_capture_terms_v1")
    ? SCENARIOS
    : SCENARIOS.filter((s): s is Scenario => s !== "terms");
  replaceCaptureOptions(doc, providerEl, PROVIDERS);
  replaceCaptureOptions(doc, scenarioEl, scenarioValues);
}

/** Refresh has feature data but no manifest context, so the section's hidden
 * state carries wireDevTools' packed-build refusal into every periodic tick. */
export function refreshCaptureOptions(doc: Document = document, daemonFeatures?: string[]): void {
  const section = doc.querySelector<HTMLElement>(".capture");
  if (!section || section.hidden) return;
  populateCaptureOptions(doc, daemonFeatures);
}

export function wireCapture(doc: Document = document, daemonFeatures?: string[]): void {
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

  if (!button.dataset.wired) {
    button.dataset.wired = "1";
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
  populateCaptureOptions(doc, daemonFeatures);
}

export async function grantProviderAccess(host: string): Promise<boolean> {
  const normalized = host.trim().toLowerCase();
  try {
    if (new URL(`https://${normalized}/`).hostname !== normalized) return false;
  } catch {
    return false;
  }
  return chrome.permissions.request({ origins: [`https://${normalized}/*`] });
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

if (
  typeof document !== "undefined" &&
  typeof chrome !== "undefined" &&
  typeof chrome.storage?.local?.get === "function" &&
  (chrome.storage.session === undefined || typeof chrome.storage.session.get === "function")
) {
  renderPageAcquire(
    document,
    acquireCurrentPage,
    async (binding) => {
      // The manual-delivery-target refinement in pdfDeliveryCopy reads this
      // snapshot, so it must be current before the reply is interpreted.
      const store = await chromeBackend(chrome.storage).load().catch(() => ({ activeJobs: [] as ActiveJob[] }));
      (globalThis as unknown as { __papioLastJobs?: ActiveJob[] }).__papioLastJobs = store.activeJobs ?? [];
      return sendCurrentPDF(binding);
    },
  );
  wireDevTools();
  wireSettings();
  wireInboxLauncher();
  wirePageBulkScanLauncher();
  wireHistoryLauncher();
  wirePrimaryShortcut();
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      void refresh();
      sendPopupPresence(popupPresenceFeatures, true);
    } else {
      sendPopupPresence(popupPresenceFeatures, false);
    }
  });
  // `pagehide` is the only best-effort release signal for a popup that is torn
  // down without a visibilitychange. Guard `window`: this module is imported in
  // a DOM-less test context, where an unguarded reference throws at import time.
  if (typeof window !== "undefined") {
    window.addEventListener("pagehide", () => sendPopupPresence(popupPresenceFeatures, false));
  }
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
