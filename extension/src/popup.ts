// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Popup: show the active broker-owned handoffs as an acquisition batch. The
// popup never talks to the native host directly: cancelling is a message to the
// service worker, and focusing only activates the broker-owned tab.
//
// It also hosts a developer-only "Capture fixture" panel used during adapter
// work. Sanitization and the fail-closed residual-secret guard live in
// ./capture; the popup only wires the DOM.

import { captureFixture, type ChromeCaptureApi, type PageCapture, type Provider, type Scenario } from "./capture";
import { chromeBackend, type ActiveJob } from "./state";

type JobSection = "needs-you" | "in-flight" | "done" | "failed";

export interface PopupActions {
  cancel(jobID: string): Promise<void>;
  focus(job: ActiveJob): Promise<void>;
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
  if (tab?.windowId !== undefined) await chrome.windows.update(tab.windowId, { focused: true });
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

export async function refresh(): Promise<void> {
  const { activeJobs } = await chromeBackend(chrome.storage).load();
  renderJobs(document, activeJobs, realActions(), () => {
    void refresh();
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

if (typeof document !== "undefined" && typeof chrome !== "undefined") {
  wireCapture();
  void refresh();
}
