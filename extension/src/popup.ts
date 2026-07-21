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
import { chromeBackend, type ActiveJob, type StoreShape, TERMS_CONSENT_KEY } from "./state";
import { renderPapio } from "./dom";


/** Render daemon problems near the actions and keep the connected daemon
 * version in the muted diagnostic footer. */
export function renderDaemonStatus(
  doc: Document,
  status: Pick<StoreShape, "connectionStatus" | "daemonVersion" | "daemonUpdateHint">,
): void {
  const card = doc.getElementById("daemon-status");
  const message = doc.getElementById("daemon-status-message");
  const hint = doc.getElementById("daemon-status-hint");
  const footer = doc.getElementById("daemon-footer");
  if (!card || !message || !hint || !footer) return;

  let line = "";
  let action = "";
  let diagnostic = "";
  switch (status.connectionStatus ?? "disconnected") {
    case "connected": {
      const stampedVersion =
        typeof __PAPIO_DAEMON_VERSION__ === "string" ? __PAPIO_DAEMON_VERSION__ : "";
      if (typeof status.daemonVersion === "string" && status.daemonVersion.length > 0) {
        diagnostic = `papio daemon v${status.daemonVersion}`;
      }
      if (
        status.daemonUpdateHint === true &&
        stampedVersion !== "" &&
        stampedVersion !== "0.0.0-dev" &&
        typeof status.daemonVersion === "string" &&
        status.daemonVersion.length > 0
      ) {
        line = `papio ${stampedVersion} is available — daemon is v${status.daemonVersion}`;
        action = "brew upgrade papio, then: papio daemon stop";
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
  footer.hidden = diagnostic.length === 0;
  renderPapio(footer, diagnostic);
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
  error?: string;
}

interface PageMetadata {
  url: string;
  doi?: string;
  title?: string;
}

const NO_DOI_FOUND = "no DOI found on this page";

/**
 * Runs INSIDE the page via scripting.executeScript — must stay fully
 * self-contained (no outer-scope references survive serialization).
 *
 * DOI sources, in trust order: Google Scholar's citation_doi (Wiley,
 * Springer, most publishers), SAGE's publication_doi, Dublin Core
 * dc.Identifier[scheme=doi] (Atypon platforms omit citation_doi on
 * abstract pages), then a DOI-shaped match in the URL path or canonical
 * link (journals.sagepub.com/doi/abs/10.1177/... carries it verbatim).
 * The daemon re-validates and normalizes whatever we send.
 */
export function collectPageMetadata(): PageMetadata {
  const clean = (value: string | null | undefined): string => (value ?? "").trim();
  const meta = (name: string): string =>
    clean(document.querySelector(`meta[name="${name}"]`)?.getAttribute("content"));
  let doi = meta("citation_doi") || meta("publication_doi");
  if (!doi) {
    for (const el of Array.from(
      document.querySelectorAll('meta[name="dc.Identifier"], meta[name="DC.Identifier"], meta[name="dc.identifier"]'),
    )) {
      const scheme = clean(el.getAttribute("scheme")).toLowerCase();
      const content = clean(el.getAttribute("content"));
      if (!content) continue;
      if (scheme === "doi") { doi = content; break; }
      if (!scheme && content.toLowerCase().startsWith("doi:")) { doi = content.slice(4).trim(); break; }
      if (!scheme && /^10\.\d{4,9}\//.test(content)) { doi = content; break; }
    }
  }
  if (!doi) {
    const doiInPath = (value: string): string => {
      let decoded = value;
      try { decoded = decodeURIComponent(value); } catch { /* keep raw */ }
      const match = decoded.match(/10\.\d{4,9}\/[^\s?#]+/);
      return match ? match[0] : "";
    };
    doi =
      doiInPath(location.pathname) ||
      doiInPath(clean(document.querySelector('link[rel="canonical"]')?.getAttribute("href"))) ||
      doiInPath(clean(document.querySelector('meta[property="og:url"]')?.getAttribute("content")));
  }
  const title = meta("citation_title") || document.title.trim();
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
  const [injected] = await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    func: collectPageMetadata,
  });
  const metadata = injected?.result;
  if (
    typeof metadata !== "object" ||
    metadata === null ||
    typeof (metadata as PageMetadata).url !== "string"
  ) {
    throw new Error("Could not read the current page");
  }
  return metadata as PageMetadata;
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
    void onOpen().catch(
      (error: unknown) => {
        if (status) status.textContent = error instanceof Error ? error.message : "Could not open inbox";
      },
    ).finally(() => {
      button.disabled = false;
    });
  });
}

function pageAcquireStatus(response: PageAcquireResponse): string {
  if (typeof response.error === "string" && response.error.length > 0) return response.error;
  if (typeof response.job_id === "string" && response.job_id.length > 0) {
    return response.duplicate === true ? `Already queued: ${response.job_id}` : `Queued: ${response.job_id}`;
  }
  return "The daemon did not acknowledge this page.";
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

/** Render a page-aware acquisition launcher. It remains available while the
 * daemon is down so its established error path stays actionable. */
export function renderPageAcquire(
  doc: Document,
  onAcquire: () => Promise<PageAcquireResponse> = acquireCurrentPage,
): void {
  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn");
  const status = doc.getElementById("page-acquire-status");
  if (!(section instanceof HTMLElement) || !(button instanceof HTMLButtonElement) || !status) return;
  section.hidden = false;
  if (button.dataset.wired) return;
  button.dataset.wired = "1";
  let noDOIFound = false;
  button.addEventListener("click", () => {
    button.disabled = true;
    status.textContent = "Acquiring…";
    void onAcquire().then(
      (response) => {
        noDOIFound = response.error === NO_DOI_FOUND;
        status.textContent = pageAcquireStatus(response);
      },
      (error: unknown) => {
        status.textContent = error instanceof Error ? error.message : "Could not acquire this page";
      },
    ).finally(() => {
      button.disabled = noDOIFound;
    });
  });
}

export function renderPageContext(doc: Document, page: PageMetadata | undefined, jobs: ActiveJob[]): void {
  const detected = doc.getElementById("page-acquire-doi");
  const state = doc.getElementById("page-acquire-context");
  const button = doc.getElementById("page-acquire-btn");
  if (!detected || !state || !(button instanceof HTMLButtonElement)) return;
  if (!page?.doi) {
    detected.textContent = "No DOI detected on this page";
    state.textContent = "";
    button.disabled = true;
    return;
  }
  detected.textContent = `Detected DOI: ${page.doi}`;
  button.disabled = false;
  const normalizedDOI = page.doi.trim().toLowerCase().replace(/^doi:\s*/, "");
  const inFlight = jobs.some(
    (job) => job.expected?.doi?.trim().toLowerCase().replace(/^doi:\s*/, "") === normalizedDOI,
  );
  state.textContent = inFlight ? "An acquisition for this DOI is already in progress." : "";
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
  try {
    renderPageContext(document, await readCurrentPageMetadata(), store.activeJobs);
  } catch {
    renderPageContext(document, undefined, store.activeJobs);
  }
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
  downloads: { download: (options) => chrome.downloads.download(options) },
};

export function wireCapture(doc: Document = document): void {
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
      statusEl.textContent = result.ok ? `Saved ${result.filename}` : result.error;
      button.disabled = false;
    });
  });
}

export function wireSettings(doc: Document = document): void {
  const button = doc.getElementById("settings-btn");
  if (!(button instanceof HTMLButtonElement)) {
    return;
  }
  button.addEventListener("click", () => {
    void chrome.runtime.openOptionsPage();
    window.close();
  });
}

// The capture-fixture panel is a Chrome-only developer tool. Chrome adds an
// update_url to store installs; unpacked manifests omit it. Firefox manifests
// carry browser_specific_settings and must not expose the Chrome capture tool.
export function wireDevTools(doc: Document = document): void {
  let manifest: chrome.runtime.Manifest & Record<string, unknown>;
  try {
    manifest = chrome.runtime.getManifest() as chrome.runtime.Manifest & Record<string, unknown>;
  } catch {
    return;
  }
  if ("browser_specific_settings" in manifest || "update_url" in manifest) return;
  const section = doc.querySelector<HTMLElement>(".capture");
  if (section) section.hidden = false;
  wireCapture(doc);
}

if (typeof document !== "undefined" && typeof chrome !== "undefined") {
  renderPageAcquire(document);
  wireDevTools();
  wireSettings();
  wireInboxLauncher();
  wirePrimaryShortcut();
  // The initial refresh must not float: a popup opened before storage is
  // reachable (or a test importing this module) would otherwise surface an
  // unhandled rejection. Later refreshes re-render; this one is best-effort.
  refresh().catch((e) => console.debug("papio: initial popup refresh failed", e));
}
