// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Popup: show the active broker-owned handoffs and offer a cancel button. Cancel
// asks the background service worker to emit provider_outcome{cancelled} and
// close the tab; the popup never talks to the native host directly.
//
// It also hosts a developer-only "Capture fixture" panel used during Phase 3
// adapter work: on an explicit click (a user gesture, so `activeTab` is live) it
// serializes the active tab, sanitizes it here in the popup, and downloads the
// result as a versioned adapter fixture. Sanitization and the fail-closed
// residual-secret guard both live in ./capture; the popup only wires the DOM.

import { captureFixture, type ChromeCaptureApi, type PageCapture, type Provider, type Scenario } from "./capture";
import { chromeBackend, type ActiveJob } from "./state";

function jobItem(job: ActiveJob): HTMLLIElement {
  const item = document.createElement("li");

  const id = document.createElement("div");
  id.className = "job-id";
  id.textContent = job.job_id;

  const status = document.createElement("div");
  status.className = "job-status";
  status.textContent = job.status;

  const cancel = document.createElement("button");
  cancel.textContent = "Cancel";
  cancel.addEventListener("click", () => {
    void chrome.runtime.sendMessage({ channel: "papio", action: "cancel", job_id: job.job_id }).then(() => {
      item.remove();
      void refresh();
    });
  });

  item.append(id, status, cancel);
  return item;
}

async function refresh(): Promise<void> {
  const jobsEl = document.getElementById("jobs");
  const emptyEl = document.getElementById("empty");
  if (!(jobsEl instanceof HTMLUListElement) || !emptyEl) return;

  const { activeJobs } = await chromeBackend(chrome.storage).load();
  jobsEl.replaceChildren(...activeJobs.map(jobItem));
  emptyEl.hidden = activeJobs.length > 0;
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

function wireCapture(): void {
  const providerEl = document.getElementById("capture-provider");
  const scenarioEl = document.getElementById("capture-scenario");
  const button = document.getElementById("capture-btn");
  const statusEl = document.getElementById("capture-status");
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

wireCapture();
void refresh();
