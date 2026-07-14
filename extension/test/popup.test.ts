// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import { cancelJob, focusJob, renderJobs, type PopupActions } from "../src/popup";
import type { ActiveJob } from "../src/state";

function popupDocument(): Document {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/popup.html", import.meta.url), "utf8"));
  Object.assign(globalThis, {
    document: window.document,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLSelectElement: window.HTMLSelectElement,
    HTMLUListElement: window.HTMLUListElement,
  });
  return window.document as unknown as Document;
}

function job(status: ActiveJob["status"] | "imported" | "failed", overrides: Partial<ActiveJob> = {}): ActiveJob {
  return {
    job_id: `job-${status}`,
    tab_id: 17,
    offered_at: 1,
    expires_at: 2,
    status: status as unknown as ActiveJob["status"],
    provider_hosts: ["www.jstor.org"],
    expected: { title: `A ${status} paper` },
    ...overrides,
  };
}

function fakeActions() {
  const focused: ActiveJob[] = [];
  const cancelled: string[] = [];
  const actions: PopupActions = {
    focus: async (activeJob) => {
      focused.push(activeJob);
    },
    cancel: async (jobID) => {
      cancelled.push(jobID);
    },
  };
  return { actions, focused, cancelled };
}

test("renders a batch with needs-you first and correct phase counts", () => {
  const doc = popupDocument();
  const { actions } = fakeActions();
  renderJobs(
    doc,
    [
      job("accepted", { job_id: "in-flight" }),
      job("auth_pending", { job_id: "sign-in", tab_id: 9 }),
      job("awaiting_download", { job_id: "manual-download", tab_id: 10 }),
      job("imported", { job_id: "imported" }),
      job("failed", { job_id: "failed" }),
    ],
    actions,
  );

  expect(doc.getElementById("summary")?.textContent).toBe("2 need you · 1 downloading · 1 imported · 1 failed");
  expect(doc.getElementById("needs-you-count")?.textContent).toBe("2");
  expect(doc.getElementById("in-flight-count")?.textContent).toBe("1");
  expect(doc.getElementById("done-count")?.textContent).toBe("1");
  expect(doc.getElementById("failed-count")?.textContent).toBe("1");
  expect(doc.querySelector("#needs-you-list .job-title")?.textContent).toBe("A auth_pending paper");
  expect(doc.querySelector("#needs-you-list")?.textContent).toContain("Sign in to continue");
  expect(doc.querySelector("#needs-you-list")?.textContent).toContain("Download the paper");
  expect(doc.querySelector("#in-flight-list")?.textContent).toContain("Opening provider");
  expect(doc.querySelector("#in-flight-list .job-provider")?.textContent).toBe("jstor.org");
  expect(doc.querySelector("#done-section")?.hasAttribute("open")).toBe(false);
  expect(doc.querySelector("#failed-section")?.hasAttribute("open")).toBe(false);
  expect(doc.getElementById("capture-btn")).not.toBeNull();
});

test("focus button activates the correct broker-owned tab and then its window", async () => {
  const doc = popupDocument();
  const tabCalls: Array<[number, { active: boolean }]> = [];
  const windowCalls: Array<[number, { focused: boolean }]> = [];
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        update: async (tabID: number, properties: { active: boolean }) => {
          tabCalls.push([tabID, properties]);
          return { windowId: 42 };
        },
      },
      windows: {
        update: async (windowID: number, properties: { focused: boolean }) => {
          windowCalls.push([windowID, properties]);
        },
      },
    },
  });
  const actions: PopupActions = { cancel: async () => {}, focus: focusJob };
  renderJobs(doc, [job("auth_pending", { tab_id: 91 })], actions);

  const focus = Array.from(doc.querySelectorAll("button")).find((button) => button.textContent === "Focus");
  focus?.click();
  await Promise.resolve();

  expect(tabCalls).toEqual([[91, { active: true }]]);
  expect(windowCalls).toEqual([[42, { focused: true }]]);
});

test("cancel remains wired for an in-flight handoff", async () => {
  const doc = popupDocument();
  const messages: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (message: unknown) => {
          messages.push(message);
        },
      },
    },
  });
  const actions: PopupActions = { cancel: cancelJob, focus: async () => {} };
  renderJobs(doc, [job("accepted", { job_id: "cancel-me" })], actions);

  const cancel = Array.from(doc.querySelectorAll("button")).find((button) => button.textContent === "Cancel");
  cancel?.click();
  await Promise.resolve();
  await Promise.resolve();

  expect(messages).toEqual([{ channel: "papio", action: "cancel", job_id: "cancel-me" }]);
  expect(doc.querySelector("#in-flight-list .job")).toBeNull();
});
