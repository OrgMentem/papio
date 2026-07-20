// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Popup: show the active broker-owned handoffs as an acquisition batch. The
// popup never talks to the native host directly: cancelling is a message to the
// service worker, and focusing only activates the broker-owned tab.
//
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

type JobSection = "needs-you" | "in-flight" | "done" | "failed";

export interface PopupActions {
  cancel(jobID: string): Promise<void>;
  focus(job: ActiveJob): Promise<void>;
}

/** Render the native-daemon compatibility state independently of job activity,
 * so a connection or version problem remains visible in an otherwise empty
 * batch. */
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
  let quiet = false;
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
        line = `papio ${stampedVersion} is available — daemon is v${status.daemonVersion}`;
        action = "brew upgrade papio, then: papio daemon stop";
      } else if (typeof status.daemonVersion === "string" && status.daemonVersion.length > 0) {
        line = `papio daemon v${status.daemonVersion}`;
        quiet = true;
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
  card.classList.toggle("quiet", quiet);
  renderPapio(message, line);
  hint.textContent = action;
}

interface ClassifiedJob {
  job: ActiveJob;
  section: JobSection;
  reason?: string;
  state: string;
}

function classifyJob(job: ActiveJob): ClassifiedJob {
  // Storage is durable across extension upgrades. Treat future worker states as
  // display-only rather than making the popup fail to render an older record.
  const status = job.status as string;
  switch (status) {
    case "auth_pending":
    case "login":
      return { job, section: "needs-you", reason: "Sign in to continue", state: "Awaiting sign-in" };
    case "terms":
      return { job, section: "needs-you", reason: "Accept the provider terms", state: "Terms need approval" };
    case "awaiting_download":
    case "manual_download":
      if (job.download_initiated) return { job, section: "in-flight", state: "Downloading…" };
      return { job, section: "needs-you", reason: "Download the paper", state: "Manual download needed" };
    case "downloading":
      return { job, section: "in-flight", state: "Downloading…" };
    case "imported":
    case "completed":
      return { job, section: "done", state: "Imported" };
    case "failed":
    case "cancelled":
      return { job, section: "failed", state: "Failed" };
    case "offered":
      return { job, section: "in-flight", state: "Starting…" };
    default:
      return { job, section: "in-flight", state: "Opening provider…" };
  }
}

function providerFor(job: ActiveJob): string {
  if (job.adapter_id) return job.adapter_id;
  return (job.provider_hosts[0] ?? "Provider").replace(/^www\./, "");
}

function titleFor(job: ActiveJob): string {
  const title = job.expected?.title?.trim();
  if (!title) return job.job_id;
  return title.length > 68 ? `${title.slice(0, 67)}…` : title;
}

export async function focusJob(job: ActiveJob): Promise<void> {
  const tab = await chrome.tabs.update(job.tab_id, { active: true });
  if (tab?.windowId === undefined) return;
  // Work-window tabs live in a minimized window; restore before focusing.
  // A normal/maximized window keeps its state — only focus changes.
  const win = await chrome.windows.get(tab.windowId);
  await chrome.windows.update(tab.windowId, {
    focused: true,
    ...(win.state === "minimized" ? { state: "normal" as const } : {}),
  });
}
export async function cancelJob(jobID: string): Promise<void> {
  await chrome.runtime.sendMessage({ channel: "papio", action: "cancel", job_id: jobID });
}


function realActions(): PopupActions {
  return { cancel: cancelJob, focus: focusJob };
}

function jobItem(doc: Document, classified: ClassifiedJob, actions: PopupActions, onCancelled?: () => void): HTMLLIElement {
  const { job, section, reason, state } = classified;
  const item = doc.createElement("li");
  item.className = "job";

  const provider = doc.createElement("div");
  provider.className = "job-provider";
  provider.textContent = providerFor(job);

  const title = doc.createElement("div");
  title.className = "job-title";
  title.textContent = titleFor(job);
  title.title = job.expected?.title ?? job.job_id;

  const stateEl = doc.createElement("div");
  stateEl.className = "job-state";
  stateEl.textContent = state;

  const details = doc.createElement("div");
  details.className = "job-details";
  details.append(provider, title, stateEl);

  if (reason) {
    const reasonEl = doc.createElement("div");
    reasonEl.className = "job-reason";
    reasonEl.textContent = reason;
    details.append(reasonEl);
  }

  const controls = doc.createElement("div");
  controls.className = "job-controls";
  if (section === "needs-you") {
    const focus = doc.createElement("button");
    focus.className = "primary";
    focus.type = "button";
    focus.textContent = "Focus";
    focus.addEventListener("click", () => {
      void actions.focus(job).catch(() => {});
    });
    controls.append(focus);
  }

  if (section === "needs-you" || section === "in-flight") {
    const cancel = doc.createElement("button");
    cancel.type = "button";
    cancel.textContent = "Cancel";
    cancel.addEventListener("click", () => {
      void actions.cancel(job.job_id).then(
        () => {
          item.remove();
          onCancelled?.();
        },
        () => {},
      );
    });
    controls.append(cancel);
  }

  item.append(details, controls);
  return item;
}

function listFor(doc: Document, id: string): HTMLUListElement | null {
  const list = doc.getElementById(id);
  return list instanceof HTMLUListElement ? list : null;
}

/**
 * Render a batch snapshot. Exported so the popup can be tested against a
 * happy-dom document without importing Chrome's real API surface.
 */
export function renderJobs(
  doc: Document,
  jobs: ActiveJob[],
  actions: PopupActions = realActions(),
  onCancelled?: () => void,
): void {
  const groups: Record<JobSection, ClassifiedJob[]> = {
    "needs-you": [],
    "in-flight": [],
    done: [],
    failed: [],
  };
  for (const job of jobs) {
    const classified = classifyJob(job);
    groups[classified.section].push(classified);
  }

  const counts = {
    needs: groups["needs-you"].length,
    inFlight: groups["in-flight"].length,
    done: groups.done.length,
    failed: groups.failed.length,
  };
  const summary = doc.getElementById("summary");
  if (summary) {
    summary.textContent = `${counts.needs} need you · ${counts.inFlight} downloading · ${counts.done} imported · ${counts.failed} failed`;
  }

  const sections: Array<[JobSection, string, string]> = [
    ["needs-you", "needs-you-list", "needs-you-section"],
    ["in-flight", "in-flight-list", "in-flight-section"],
    ["done", "done-list", "done-section"],
    ["failed", "failed-list", "failed-section"],
  ];
  for (const [section, listID, sectionID] of sections) {
    const list = listFor(doc, listID);
    if (list) list.replaceChildren(...groups[section].map((job) => jobItem(doc, job, actions, onCancelled)));
    const container = doc.getElementById(sectionID);
    if (container && (section === "needs-you" || section === "in-flight")) container.hidden = groups[section].length === 0;
  }

  for (const [id, count] of [
    ["needs-you-count", counts.needs],
    ["in-flight-count", counts.inFlight],
    ["done-count", counts.done],
    ["failed-count", counts.failed],
  ] as const) {
    const countEl = doc.getElementById(id);
    if (countEl) countEl.textContent = String(count);
  }

  const emptyEl = doc.getElementById("empty");
  if (emptyEl) emptyEl.hidden = jobs.length > 0;
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

/** Capability state belongs to the live worker, never a prior session snapshot. */
export async function livePageAcquireAvailable(): Promise<boolean> {
  try {
    const response: unknown = await chrome.runtime.sendMessage({
      channel: "papio",
      action: "get_capabilities",
    });
    return (
      typeof response === "object" &&
      response !== null &&
      (response as Record<string, unknown>)["page_acquire"] === true
    );
  } catch {
    return false;
  }
}

function pageAcquireStatus(response: PageAcquireResponse): string {
  if (typeof response.error === "string" && response.error.length > 0) return response.error;
  if (typeof response.job_id === "string" && response.job_id.length > 0) {
    return response.duplicate === true ? `Already queued: ${response.job_id}` : `Queued: ${response.job_id}`;
  }
  return "The daemon did not acknowledge this page.";
}

export async function acquireCurrentPage(): Promise<PageAcquireResponse> {
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
  const page = metadata as PageMetadata;
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

/** Show page acquisition only when this connected daemon negotiated support. */
export function renderPageAcquire(
  doc: Document,
  enabled: boolean,
  onAcquire: () => Promise<PageAcquireResponse> = acquireCurrentPage,
): void {
  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn");
  const status = doc.getElementById("page-acquire-status");
  if (!(section instanceof HTMLElement) || !(button instanceof HTMLButtonElement) || !status) return;
  section.hidden = !enabled;
  if (!enabled || button.dataset.wired) return;
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

let capabilityWatcherBound = false;

/** Re-query the live worker when persisted bridge state changes after hello_ack. */
function watchLiveCapability(): void {
  if (capabilityWatcherBound) return;
  const onChanged = chrome.storage.onChanged;
  if (onChanged === undefined) return;
  capabilityWatcherBound = true;
  onChanged.addListener((changes, areaName) => {
    if (
      (areaName !== "session" && areaName !== "local") ||
      !("papio_state_v1" in changes)
    ) {
      return;
    }
    return refresh();
  });
}

export async function refresh(): Promise<void> {
  watchLiveCapability();
  const [store, pageAcquireEnabled] = await Promise.all([
    chromeBackend(chrome.storage).load(),
    livePageAcquireAvailable(),
  ]);
  renderDaemonStatus(document, store);
  renderPageAcquire(document, pageAcquireEnabled);
  renderJobs(document, store.activeJobs, realActions(), () => {
    void refresh();
  });
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

// The capture-fixture panel is a developer-only tool. Show and wire it only for
// unpacked development installs; store-installed users never see it. getSelf()
// needs no "management" permission.
async function wireDevTools(doc: Document = document): Promise<void> {
  let development = false;
  try {
    development = (await chrome.management.getSelf()).installType === "development";
  } catch {
    development = false;
  }
  if (!development) return;
  const section = doc.querySelector<HTMLElement>(".capture");
  if (section) section.hidden = false;
  wireCapture(doc);
}

if (typeof document !== "undefined" && typeof chrome !== "undefined") {
  void wireDevTools();
  wireSettings();
  void refresh();
}
