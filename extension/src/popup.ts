// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Popup: show the active broker-owned handoffs and offer a cancel button. Cancel
// asks the background service worker to emit provider_outcome{cancelled} and
// close the tab; the popup never talks to the native host directly.

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

void refresh();
