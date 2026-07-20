// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import {
  acquireCurrentPage,
  cancelJob,
  livePageAcquireAvailable,
  focusJob,
  renderDaemonStatus,
  renderJobs,
  renderPageAcquire,
  refresh,
  renderResolverGrants,
  wireSettings,
  type PopupActions,
} from "../src/popup";
import type { ActiveJob } from "../src/state";

function popupDocument(): Document {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/popup.html", import.meta.url), "utf8"));
  Object.assign(globalThis, {
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
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

test("renders daemon connection and compatibility states", () => {
  const doc = popupDocument();
  renderDaemonStatus(doc, { connectionStatus: "connected", daemonVersion: "0.1.0" });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(false);
  expect(doc.getElementById("daemon-status")?.classList.contains("quiet")).toBe(true);
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe("papio daemon v0.1.0");

  Object.assign(globalThis, { __PAPIO_DAEMON_VERSION__: "0.2.0" });
  renderDaemonStatus(doc, {
    connectionStatus: "connected",
    daemonVersion: "0.1.0",
    daemonUpdateHint: true,
  });
  expect(doc.getElementById("daemon-status")?.classList.contains("quiet")).toBe(false);
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe(
    "papio 0.2.0 is available — daemon is v0.1.0",
  );
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe("brew upgrade papio, then: papio daemon stop");
  delete (globalThis as Record<string, unknown>).__PAPIO_DAEMON_VERSION__;

  renderDaemonStatus(doc, { connectionStatus: "disconnected" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain("papio daemon isn't reachable");
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe("run: papio daemon status");

  renderDaemonStatus(doc, { connectionStatus: "daemon_outdated", daemonVersion: "0.0.9" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain("papio daemon is out of date");
  expect(doc.getElementById("daemon-status-hint")?.textContent).toContain("update papio");

  renderDaemonStatus(doc, { connectionStatus: "extension_outdated" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain(
    "this extension is older than your papio daemon supports",
  );
});

test("gates page acquisition on the negotiated feature", async () => {
  const doc = popupDocument();
  let calls = 0;
  const acquire = async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  };

  renderPageAcquire(doc, false, acquire);
  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(section?.hidden).toBe(true);
  button.click();
  expect(calls).toBe(0);

  renderPageAcquire(doc, true, acquire);
  expect(section?.hidden).toBe(false);
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Queued: job_page_acquire_001");
});

test("reads page-acquire capability from the live worker", async () => {
  const requests: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (request: unknown) => {
          requests.push(request);
          return { page_acquire: false };
        },
      },
    },
  });

  expect(await livePageAcquireAvailable()).toBe(false);
  expect(requests).toEqual([{ channel: "papio", action: "get_capabilities" }]);
});

test("refreshes live capability when negotiation completes", async () => {
  const doc = popupDocument();
  let pageAcquireAvailable = false;
  let onChanged: ((changes: unknown, areaName: string) => unknown) | undefined;
  Object.assign(globalThis, {
    chrome: {
      storage: {
        session: {
          get: async () => ({
            papio_state_v1: {
              activeJobs: [],
              connectionStatus: "connected",
              daemonFeatures: ["page_acquire"],
            },
          }),
        },
        local: { get: async () => ({}) },
        onChanged: {
          addListener: (listener: (changes: unknown, areaName: string) => unknown) => {
            onChanged = listener;
          },
        },
      },
      runtime: { sendMessage: async () => ({ page_acquire: pageAcquireAvailable }) },
      permissions: { contains: async () => false },
    },
  });

  await refresh();
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);

  pageAcquireAvailable = true;
  await onChanged?.({ papio_state_v1: {} }, "session");
  expect(doc.getElementById("page-acquire")?.hidden).toBe(false);
});

test("disables page acquisition when the current page has no DOI", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, true, async () => {
    calls += 1;
    return { error: "no DOI found on this page" };
  });

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();

  expect(calls).toBe(1);
  expect(button.disabled).toBe(true);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("no DOI found on this page");
});

test("does not send a DOI-less scraped page to the daemon", async () => {
  popupDocument();
  let messages = 0;
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 1 }] },
      scripting: {
        executeScript: async () => [{
          result: { url: "https://publisher.example.edu/article/42", title: "A DOI-less page" },
        }],
      },
      runtime: {
        sendMessage: async () => {
          messages += 1;
          return { job_id: "job_page_acquire_001" };
        },
      },
    },
  });

  await expect(acquireCurrentPage()).resolves.toEqual({ error: "no DOI found on this page" });
  expect(messages).toBe(0);
});

test("focus button activates the correct broker-owned tab and then its window", async () => {
  const doc = popupDocument();
  const tabCalls: Array<[number, { active: boolean }]> = [];
  const windowCalls: Array<[number, { focused: boolean; state?: string }]> = [];
  let windowState = "normal";
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        update: async (tabID: number, properties: { active: boolean }) => {
          tabCalls.push([tabID, properties]);
          return { windowId: 42 };
        },
      },
      windows: {
        get: async (windowID: number) => ({ id: windowID, state: windowState }),
        update: async (windowID: number, properties: { focused: boolean; state?: string }) => {
          windowCalls.push([windowID, properties]);
        },
      },
    },
  });
  // Capture the wired action's promise so completion is awaited directly,
  // instead of guessing a scheduler delay.
  let focused: Promise<void> = Promise.resolve();
  const actions: PopupActions = {
    cancel: async () => {},
    focus: (target) => (focused = focusJob(target)),
  };
  renderJobs(doc, [job("auth_pending", { tab_id: 91 })], actions);

  const focus = Array.from(doc.querySelectorAll("button")).find((button) => button.textContent === "Focus");
  focus?.click();
  await focused;

  // A normal window keeps its state — focus only, never a forced resize.
  expect(tabCalls).toEqual([[91, { active: true }]]);
  expect(windowCalls).toEqual([[42, { focused: true }]]);

  // A minimized work window is restored while it is focused.
  windowState = "minimized";
  focus?.click();
  await focused;
  expect(windowCalls[1]).toEqual([42, { focused: true, state: "normal" }]);
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

test("settings cog opens the options page and closes the popup", () => {
  const doc = popupDocument();
  let opened = 0;
  let closed = 0;
  Object.assign(globalThis, {
    chrome: { runtime: { openOptionsPage: () => { opened += 1; return Promise.resolve(); } } },
    window: { close: () => { closed += 1; } },
  });
  wireSettings(doc);
  const button = doc.getElementById("settings-btn") as unknown as HTMLButtonElement;
  expect(button).not.toBeNull();
  button.click();
  expect(opened).toBe(1);
  expect(closed).toBe(1);
});

test("renders a one-click library grant for ungranted resolvers", () => {
  const doc = popupDocument();
  const grants: string[][] = [];
  renderResolverGrants(doc, ["https://onesearch.library.example.edu"], (origins) => grants.push(origins));

  const section = doc.getElementById("resolver-grant");
  expect(section?.hidden).toBe(false);
  expect(section?.textContent).toContain("onesearch.library.example.edu");
  const button = section?.querySelector("button") as HTMLButtonElement | null;
  expect(button?.textContent).toBe("Allow library access");

  button?.click();
  expect(grants).toEqual([["https://onesearch.library.example.edu"]]);
  expect(button?.disabled).toBe(true);
});

test("hides the library grant prompt when every resolver is granted", () => {
  const doc = popupDocument();
  renderResolverGrants(doc, [], () => {});
  const section = doc.getElementById("resolver-grant");
  expect(section?.hidden).toBe(true);
  expect(section?.children.length).toBe(0);
});
