// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 4/5/6/8: the on-page bulk selection workspace. Loads the
// real page-bulk.html (mirrors inbox.test.ts's inboxDocument pattern) and
// drives page-bulk.ts through a mocked chrome.runtime/tabs/windows surface.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";
import type { DetectedPaper } from "../src/page-scan";

interface RuntimeRequest {
  type: string;
  request: Record<string, unknown>;
}

type Reply = (message: RuntimeRequest) => unknown | Promise<unknown>;

let importSerial = 0;

async function settle(): Promise<void> {
  for (let iteration = 0; iteration < 16; iteration += 1) await Promise.resolve();
}

interface ChromeTestOptions {
  tabsUpdateFails?: boolean;
}

async function pageBulkDocument(
  scanId: string | null,
  reply: Reply,
  options: ChromeTestOptions = {},
): Promise<{ document: Document; window: Window; requests: RuntimeRequest[]; tabsUpdated: number[] }> {
  const search = scanId !== null ? `?scan=${encodeURIComponent(scanId)}` : "";
  const window = new Window({ url: `https://ext.test/page-bulk.html${search}` });
  window.document.write(readFileSync(new URL("../src/page-bulk.html", import.meta.url), "utf8"));
  const requests: RuntimeRequest[] = [];
  const tabsUpdated: number[] = [];
  Object.assign(globalThis, {
    window,
    document: window.document,
    Event: window.Event,
    Element: window.Element,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLInputElement: window.HTMLInputElement,
    chrome: {
      runtime: {
        sendMessage: async (message: RuntimeRequest) => {
          requests.push(message);
          return reply(message);
        },
        getURL: (path: string) => `chrome-extension://test-id/${path}`,
      },
      tabs: {
        update: async (tabId: number, _props: Record<string, unknown>) => {
          if (options.tabsUpdateFails === true) throw new Error("No tab with id");
          tabsUpdated.push(tabId);
          return { id: tabId, windowId: 900 };
        },
      },
      windows: {
        get: async (windowId: number) => ({ id: windowId, state: "normal" }),
        update: async (windowId: number, _props: Record<string, unknown>) => ({ id: windowId }),
      },
    },
  });
  importSerial += 1;
  // Each fixture needs a fresh page module because its state is module-local.
  await import(`../src/page-bulk.ts?page-bulk-test=${importSerial}`);
  await settle();
  return { document: window.document as unknown as Document, window, requests, tabsUpdated };
}

function paper(overrides: Partial<DetectedPaper> = {}): DetectedPaper {
  return {
    localId: "id-1",
    detector: "generic-identifiers/1",
    identifier: { kind: "doi", value: "10.1234/abcd.5678" },
    label: "A paper about testing bulk selection",
    occurrences: 1,
    ...overrides,
  };
}

interface FixtureSnapshot {
  scanId: string;
  sourceTabId: number;
  sourceOrigin: string;
  sourceTitle: string;
  scannedAt: string;
  documentGeneration: number;
  items: DetectedPaper[];
  truncated: boolean;
}

function snapshot(overrides: Partial<FixtureSnapshot> = {}): FixtureSnapshot {
  return {
    scanId: "scan-1",
    sourceTabId: 42,
    sourceOrigin: "https://scholar.example.edu",
    sourceTitle: "Reading list \u2014 Fall 2026",
    scannedAt: "2026-08-07T12:00:00.000Z",
    documentGeneration: 1,
    items: [paper()],
    truncated: false,
    ...overrides,
  };
}

interface FixtureStatusItem {
  local_id: string;
  canonical_key?: string;
  status: string;
  ownership_complete: boolean;
  job_id?: string;
}

function eligibleStatus(localId: string, canonicalKey?: string): FixtureStatusItem {
  return { local_id: localId, canonical_key: canonicalKey ?? `work:${localId}`, status: "eligible", ownership_complete: true };
}

/** Standard reply router: load returns `snap`, status returns `items`
 * (defaulting to "eligible" for every item in `snap` when omitted),
 * allowlist reads back `allowed`. */
function standardReply(snap: FixtureSnapshot, items?: FixtureStatusItem[], allowed = false): Reply {
  const statusItems = items ?? snap.items.map((item) => eligibleStatus(item.localId));
  return (message) => {
    if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
    if (message.type === "papio.pageBulk.status") return { ok: true, items: statusItems, truncated: false };
    if (message.type === "papio.pageBulk.allowlist.get") return { ok: true, allowed };
    if (message.type === "papio.pageBulk.allowlist.set") return { ok: true, allowed: message.request["allowed"] };
    return { ok: false, error: { code: "unexpected", message: `unexpected message ${message.type}` } };
  };
}

function row(doc: Document, localId: string): HTMLElement | null {
  return doc.querySelector<HTMLElement>(`.pb-row[data-local-id='${localId}']`);
}

function checkbox(doc: Document, localId: string): HTMLInputElement | null {
  return row(doc, localId)?.querySelector<HTMLInputElement>("input[type='checkbox']") ?? null;
}

// --- header binding, load/status wiring -------------------------------------

test("loads the snapshot, binds the header (title/origin/timestamp), and sets document.title", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", standardReply(snap));

  expect(page.window.document.title).toBe("papio \u2014 select papers: Reading list \u2014 Fall 2026");
  expect(page.document.getElementById("scan-title")?.textContent).toBe("Select papers: Reading list \u2014 Fall 2026");
  expect(page.document.getElementById("scan-meta")?.textContent).toContain("https://scholar.example.edu");
  expect(page.document.getElementById("scan-summary")?.textContent).toBe("1 identified paper found \u2014 1 eligible");

  expect(page.requests[0]).toEqual({ type: "papio.pageBulk.load", request: { scan_id: "scan-1" } });
  const statusRequest = page.requests.find((r) => r.type === "papio.pageBulk.status");
  expect(statusRequest?.request).toEqual({
    scan_id: "scan-1",
    identifiers: [{ local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" }],
  });
  const allowlistRequest = page.requests.find((r) => r.type === "papio.pageBulk.allowlist.get");
  expect(allowlistRequest?.request).toEqual({ origin: "https://scholar.example.edu" });
});

test("no ?scan= in the URL shows a load error and never calls the runtime", async () => {
  const page = await pageBulkDocument(null, () => ({ ok: false }));
  expect(page.requests).toHaveLength(0);
  expect(page.document.getElementById("scan-error")?.hidden).toBe(false);
  expect(page.document.getElementById("scan-error")?.textContent).toContain("No scan specified");
});

// --- row rendering, disabled-state rules ------------------------------------

test("renders one row per paper with label, identifier, and status text", async () => {
  const snap = snapshot({
    items: [
      paper({ localId: "id-1", identifier: { kind: "doi", value: "10.1234/abcd.5678" }, label: "Paper one" }),
      paper({ localId: "id-2", identifier: { kind: "arxiv", value: "2101.00001" }, label: "Paper two" }),
    ],
  });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [eligibleStatus("id-1"), eligibleStatus("id-2")]),
  );

  expect(page.document.querySelectorAll(".pb-row")).toHaveLength(2);
  expect(row(page.document, "id-1")?.querySelector(".pb-row-label")?.textContent).toBe("Paper one");
  expect(row(page.document, "id-1")?.querySelector(".pb-row-identifier")?.textContent).toBe("DOI: 10.1234/abcd.5678");
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")?.textContent).toBe("Eligible");
  expect(row(page.document, "id-2")?.querySelector(".pb-row-identifier")?.textContent).toBe("arXiv: 2101.00001");
});

test("owned_with_pdf rows are unchecked and disabled", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [{ local_id: "id-1", canonical_key: "work:1", status: "owned_with_pdf", ownership_complete: true }]),
  );
  const box = checkbox(page.document, "id-1");
  expect(box?.checked).toBe(false);
  expect(box?.disabled).toBe(true);
  expect(row(page.document, "id-1")?.dataset["disabled"]).toBe("true");
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")?.textContent).toBe("Already in your library");
});

test("queued rows are unchecked, disabled, and show the job id", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [{ local_id: "id-1", canonical_key: "work:1", status: "queued", ownership_complete: true, job_id: "job_00000042" }]),
  );
  const box = checkbox(page.document, "id-1");
  expect(box?.checked).toBe(false);
  expect(box?.disabled).toBe(true);
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")?.textContent).toBe("Queued (job job_00000042)");
});

test("invalid rows are marked not-selectable and never carry a canonical key", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [{ local_id: "id-1", status: "invalid", ownership_complete: false }]),
  );
  const box = checkbox(page.document, "id-1");
  expect(box?.disabled).toBe(true);
  expect(row(page.document, "id-1")?.dataset["status"]).toBe("invalid");
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")?.textContent).toBe("Not a recognized identifier");
});

test("owned_missing_pdf, ownership_incomplete, and previously_unavailable rows stay eligible and checkable", async () => {
  const snap = snapshot({
    items: [
      paper({ localId: "id-1" }),
      paper({ localId: "id-2", identifier: { kind: "pmid", value: "111" } }),
      paper({ localId: "id-3", identifier: { kind: "pmid", value: "222" } }),
    ],
  });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [
      { local_id: "id-1", canonical_key: "work:1", status: "owned_missing_pdf", ownership_complete: true },
      { local_id: "id-2", canonical_key: "work:2", status: "ownership_incomplete", ownership_complete: false },
      { local_id: "id-3", canonical_key: "work:3", status: "previously_unavailable", ownership_complete: true },
    ]),
  );
  for (const id of ["id-1", "id-2", "id-3"]) {
    expect(checkbox(page.document, id)?.disabled).toBe(false);
  }
  expect(row(page.document, "id-3")?.querySelector(".pb-row-status")?.textContent).toBe("No route previously");
});

// --- selection morphing, 50-cap ---------------------------------------------

test("rows start unselected and the primary button reads Acquire all N eligible", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1" }), paper({ localId: "id-2" })] });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [eligibleStatus("id-1"), eligibleStatus("id-2")]),
  );
  expect(checkbox(page.document, "id-1")?.checked).toBe(false);
  expect(checkbox(page.document, "id-2")?.checked).toBe(false);
  const primary = page.document.getElementById("primary-btn") as HTMLButtonElement;
  expect(primary.textContent).toBe("Acquire all 2 eligible");
  expect(primary.disabled).toBe(false);
});

test("checking a row morphs the primary button to Acquire N selected, and unchecking reverts it", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1" }), paper({ localId: "id-2" })] });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [eligibleStatus("id-1"), eligibleStatus("id-2")]),
  );
  const primary = page.document.getElementById("primary-btn") as HTMLButtonElement;
  const box = checkbox(page.document, "id-1");
  box!.checked = true;
  box!.dispatchEvent(new Event("change", { bubbles: true }));
  expect(primary.textContent).toBe("Acquire 1 selected");
  box!.checked = false;
  box!.dispatchEvent(new Event("change", { bubbles: true }));
  expect(primary.textContent).toBe("Acquire all 2 eligible");
});

test("50-cap: selecting more than 50 shows the cap note without hiding the true selected count", async () => {
  const items = Array.from({ length: 60 }, (_, i) => paper({ localId: `id-${i}`, identifier: { kind: "doi", value: `10.1/${i}` } }));
  const snap = snapshot({ items });
  const statusItems = items.map((item) => eligibleStatus(item.localId));
  const page = await pageBulkDocument("scan-1", standardReply(snap, statusItems));

  for (const item of items) {
    const box = checkbox(page.document, item.localId)!;
    box.checked = true;
    box.dispatchEvent(new Event("change", { bubbles: true }));
  }
  const primary = page.document.getElementById("primary-btn") as HTMLButtonElement;
  expect(primary.textContent).toBe("Acquire 60 selected");
  expect(page.document.getElementById("submit-status")?.textContent).toBe(
    "50 selected \u00b7 papio batches are limited to 50",
  );
});

// --- submit payload, results, remainder retained ----------------------------

test("submitting Acquire all sends every eligible canonical key plus the source block", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1" }), paper({ localId: "id-2", identifier: { kind: "pmid", value: "555" } })] });
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, submitted: 2, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_1" };
    }
    return standardReply(snap, [eligibleStatus("id-1", "work:a"), eligibleStatus("id-2", "work:b")])(message);
  });

  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();

  expect(submitted).toHaveLength(1);
  expect(submitted[0]?.request).toEqual({
    scan_id: "scan-1",
    canonical_keys: ["work:a", "work:b"],
    source: { kind: "browser_page", origin: "https://scholar.example.edu", detector: "generic-identifiers/1" },
  });
});

test("submitting a manual selection sends only the checked canonical keys", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1" }), paper({ localId: "id-2", identifier: { kind: "pmid", value: "555" } })] });
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, submitted: 1, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_2" };
    }
    return standardReply(snap, [eligibleStatus("id-1", "work:a"), eligibleStatus("id-2", "work:b")])(message);
  });

  const box = checkbox(page.document, "id-1")!;
  box.checked = true;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();

  expect(submitted[0]?.request["canonical_keys"]).toEqual(["work:a"]);
});

test("submit caps the batch at 50 and retains the remainder for the next submit", async () => {
  const items = Array.from({ length: 55 }, (_, i) => paper({ localId: `id-${i}`, identifier: { kind: "doi", value: `10.1/${i}` } }));
  const snap = snapshot({ items });
  const statusItems = items.map((item) => eligibleStatus(item.localId, `work:${item.localId}`));
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, submitted: 50, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_3" };
    }
    return standardReply(snap, statusItems)(message);
  });

  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();

  const keys = submitted[0]?.request["canonical_keys"] as string[];
  expect(keys).toHaveLength(50);
  expect(keys).toEqual(items.slice(0, 50).map((item) => `work:${item.localId}`));
  // The remainder (the last 5) stay eligible, unsubmitted, and checkable.
  for (const item of items.slice(50)) {
    expect(row(page.document, item.localId)?.dataset["status"]).not.toBe("submitted");
    expect(checkbox(page.document, item.localId)?.disabled).toBe(false);
  }
  for (const item of items.slice(0, 50)) {
    expect(row(page.document, item.localId)?.dataset["status"]).toBe("submitted");
    expect(checkbox(page.document, item.localId)?.disabled).toBe(true);
  }
});

test("the result panel renders submitted/joined/already-owned/invalid counts and an open-inbox link", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      return { ok: true, submitted: 3, joined: 2, already_owned: 1, invalid: 1, batch_id: "batch_9" };
    }
    return standardReply(snap, [eligibleStatus("id-1", "work:a")])(message);
  });

  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();

  const summary = page.document.getElementById("result-summary");
  expect(summary?.hidden).toBe(false);
  expect(summary?.textContent).toContain("3 submitted");
  expect(summary?.textContent).toContain("2 joined");
  expect(summary?.textContent).toContain("1 already owned");
  expect(summary?.textContent).toContain("1 invalid");
  const link = summary?.querySelector("a");
  expect(link?.getAttribute("href")).toBe("chrome-extension://test-id/inbox.html");
  expect(link?.textContent).toBe("Open inbox");
});

// --- rescan --------------------------------------------------------------

test("Rescan loads a fresh snapshot with rows unselected again", async () => {
  const initial = snapshot({ items: [paper({ localId: "id-1" })] });
  const rescanned = snapshot({
    documentGeneration: 2,
    items: [paper({ localId: "id-1" }), paper({ localId: "id-9", identifier: { kind: "pmid", value: "999" } })],
  });
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.rescan") return { ok: true, snapshot: rescanned };
    return standardReply(initial, [eligibleStatus("id-1")])(message);
  });

  const box = checkbox(page.document, "id-1")!;
  box.checked = true;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  expect((page.document.getElementById("primary-btn") as HTMLButtonElement).textContent).toBe("Acquire 1 selected");

  (page.document.getElementById("rescan-btn") as HTMLButtonElement).click();
  await settle();

  expect(page.document.querySelectorAll(".pb-row")).toHaveLength(2);
  expect(checkbox(page.document, "id-1")?.checked).toBe(false);
});

// --- source-tab-closed state -------------------------------------------------

test("a tab_unavailable Rescan failure shows the source-tab-closed state and hides Rescan", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.rescan") {
      return { ok: false, error: { code: "tab_unavailable", message: "The source tab is no longer available" } };
    }
    return standardReply(snap, [eligibleStatus("id-1")])(message);
  });

  (page.document.getElementById("rescan-btn") as HTMLButtonElement).click();
  await settle();

  expect(page.document.getElementById("source-closed-note")?.hidden).toBe(false);
  expect(page.document.getElementById("source-closed-note")?.textContent).toBe(
    "source tab closed \u2014 this snapshot remains usable",
  );
  expect(page.document.getElementById("rescan-btn")?.hidden).toBe(true);
  expect(page.document.getElementById("return-to-source-btn")?.hidden).toBe(true);
});

test("Return to source page focuses the source tab directly via chrome.tabs.update", async () => {
  const snap = snapshot({ sourceTabId: 777 });
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")]));

  (page.document.getElementById("return-to-source-btn") as HTMLButtonElement).click();
  await settle();

  expect(page.tabsUpdated).toEqual([777]);
  expect(page.document.getElementById("source-closed-note")?.hidden).toBe(true);
  expect(page.document.getElementById("rescan-btn")?.hidden).toBe(false);
});

test("Return to source page degrades to the source-tab-closed state when the tab is gone", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")]), { tabsUpdateFails: true });

  (page.document.getElementById("return-to-source-btn") as HTMLButtonElement).click();
  await settle();

  expect(page.document.getElementById("source-closed-note")?.hidden).toBe(false);
  expect(page.document.getElementById("rescan-btn")?.hidden).toBe(true);
});

// --- scan expired ------------------------------------------------------------

test("a scan_not_found load reply shows the expired banner and hides the workspace", async () => {
  const page = await pageBulkDocument("scan-1", () => ({
    ok: false,
    error: { code: "scan_not_found", message: "This scan is no longer open" },
  }));

  expect(page.document.getElementById("scan-expired")?.hidden).toBe(false);
  expect(page.document.getElementById("scan-expired")?.textContent).toBe("scan expired \u2014 rescan the source page");
  expect(page.document.getElementById("workspace-main")?.hidden).toBe(true);
  expect(page.document.getElementById("action-bar")?.hidden).toBe(true);
});

// --- status errors -------------------------------------------------------

test("a status lookup failure shows the retry banner; Retry re-sends the request", async () => {
  const snap = snapshot();
  let statusAttempts = 0;
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
    if (message.type === "papio.pageBulk.status") {
      statusAttempts += 1;
      if (statusAttempts === 1) return { ok: false, error: { code: "unavailable", message: "status failed" } };
      return { ok: true, items: [eligibleStatus("id-1")], truncated: false };
    }
    return { ok: true, allowed: false };
  });

  expect(page.document.getElementById("status-error")?.hidden).toBe(false);
  expect(page.document.getElementById("status-error-message")?.textContent).toBe("status failed");

  (page.document.getElementById("status-retry-btn") as HTMLButtonElement).click();
  await settle();

  expect(statusAttempts).toBe(2);
  expect(page.document.getElementById("status-error")?.hidden).toBe(true);
  expect(checkbox(page.document, "id-1")?.disabled).toBe(false);
});

// --- truncated / empty states ------------------------------------------------

test("a truncated snapshot shows the truncated note", async () => {
  const snap = snapshot({ truncated: true });
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")]));
  expect(page.document.getElementById("truncated-note")?.hidden).toBe(false);
});

test("an empty snapshot shows the empty state and disables the primary button", async () => {
  const snap = snapshot({ items: [] });
  const page = await pageBulkDocument("scan-1", standardReply(snap, []));
  expect(page.document.getElementById("empty-state")?.hidden).toBe(false);
  expect(page.document.querySelectorAll(".pb-row")).toHaveLength(0);
  const primary = page.document.getElementById("primary-btn") as HTMLButtonElement;
  expect(primary.textContent).toBe("Acquire all 0 eligible");
  expect(primary.disabled).toBe(true);
});

// --- scanner allowlist -----------------------------------------------------

test("the allowlist checkbox reflects background state and persists a change", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")], true));
  const box = page.document.getElementById("allowlist-checkbox") as HTMLInputElement;
  expect(box.checked).toBe(true);

  box.checked = false;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();

  const setRequest = page.requests.find((r) => r.type === "papio.pageBulk.allowlist.set");
  expect(setRequest?.request).toEqual({ origin: "https://scholar.example.edu", allowed: false });
});
