// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ADR-0019 Decision 4/5/6/8: the on-page bulk selection workspace. Loads the
// real page-bulk.html (mirrors inbox.test.ts's inboxDocument pattern) and
// drives page-bulk.ts through a mocked chrome.runtime/tabs/windows surface.

import { expect, test, vi } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";
import type { DetectedPaper } from "../src/page-scan";

interface RuntimeRequest {
  type: string;
  request: Record<string, unknown>;
}

type Reply = (message: RuntimeRequest) => unknown | Promise<unknown>;

interface StorageChangeLike {
  oldValue?: unknown;
  newValue?: unknown;
}

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
): Promise<{
  document: Document;
  window: Window;
  requests: RuntimeRequest[];
  tabsUpdated: number[];
  emitRuntimeMessage: (message: unknown) => Promise<void>;
  emitStorageChange: (changes: Record<string, StorageChangeLike>, areaName?: string) => Promise<void>;
}> {
  const search = scanId !== null ? `?scan=${encodeURIComponent(scanId)}` : "";
  const window = new Window({ url: `https://ext.test/page-bulk.html${search}` });
  window.document.write(readFileSync(new URL("../src/page-bulk.html", import.meta.url), "utf8"));
  const requests: RuntimeRequest[] = [];
  const tabsUpdated: number[] = [];
  const runtimeListeners: ((message: unknown) => void)[] = [];
  const storageListeners: ((changes: Record<string, StorageChangeLike>, areaName: string) => void)[] = [];
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
        onMessage: {
          addListener: (listener: (message: unknown) => void) => {
            runtimeListeners.push(listener);
          },
        },
        // A relocated popup page (not the dist/popup.html default) proves the
        // inbox link is derived from the manifest and not a hardcoded
        // sibling literal: pre-fix code always opened "inbox.html" at the
        // extension root regardless of where the manifest declares the
        // popup, a path build.ts never actually writes (mirrors popup.test.ts's
        // "history launcher opens the manifest-derived history page" test).
        getManifest: () => ({ action: { default_popup: "dist/ui/popup.html" } }),
        getURL: (path: string) => `chrome-extension://test-id/${path}`,
      },
      storage: {
        session: {},
        local: {},
        onChanged: {
          addListener: (listener: (changes: Record<string, StorageChangeLike>, areaName: string) => void) => {
            storageListeners.push(listener);
          },
        },
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
  return {
    document: window.document as unknown as Document,
    window,
    requests,
    tabsUpdated,
    emitRuntimeMessage: async (message: unknown) => {
      for (const listener of runtimeListeners) listener(message);
      await settle();
    },
    emitStorageChange: async (changes: Record<string, StorageChangeLike>, areaName = "session") => {
      for (const listener of storageListeners) listener(changes, areaName);
      await settle();
    },
  };
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
  renderedRecordCountHint: number | null;
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
    renderedRecordCountHint: null,
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

test("a snapshot with a rendered-record hint attaches it to the status request; a null hint sends nothing", async () => {
  const hinted = snapshot({ renderedRecordCountHint: 12 });
  const page = await pageBulkDocument("scan-1", standardReply(hinted));
  const statusRequest = page.requests.find((r) => r.type === "papio.pageBulk.status");
  expect(statusRequest?.request).toEqual({
    scan_id: "scan-1",
    identifiers: [{ local_id: "id-1", kind: "doi", value: "10.1234/abcd.5678" }],
    rendered_record_count_hint: 12,
  });
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

test("every identifier line links to its canonical resolver, opened safely in a new tab", async () => {
  const snap = snapshot({
    items: [
      paper({ localId: "id-1", identifier: { kind: "doi", value: "10.1234/abcd.5678" } }),
      paper({ localId: "id-2", identifier: { kind: "arxiv", value: "2101.00001" } }),
      paper({ localId: "id-3", identifier: { kind: "pmid", value: "31234567" } }),
    ],
  });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [eligibleStatus("id-1"), eligibleStatus("id-2"), eligibleStatus("id-3")]),
  );

  const expected: Record<string, string> = {
    "id-1": "https://doi.org/10.1234/abcd.5678",
    "id-2": "https://arxiv.org/abs/2101.00001",
    "id-3": "https://pubmed.ncbi.nlm.nih.gov/31234567/",
  };
  for (const [localId, href] of Object.entries(expected)) {
    const link = row(page.document, localId)?.querySelector("a.pb-row-link");
    expect(link?.getAttribute("href")).toBe(href);
    expect(link?.getAttribute("target")).toBe("_blank");
    expect(link?.getAttribute("rel")).toBe("noopener noreferrer");
  }
  // The link text is the bare identifier; the kind stays an unlinked prefix.
  expect(row(page.document, "id-1")?.querySelector("a.pb-row-link")?.textContent).toBe("10.1234/abcd.5678");
  expect(row(page.document, "id-1")?.querySelector(".pb-row-kind")?.textContent).toBe("DOI:");
});

test("a DOI that breaks URL structure is percent-encoded before it reaches the href", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1", identifier: { kind: "doi", value: "10.1234/a b#c?d" } })] });
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")]));
  expect(row(page.document, "id-1")?.querySelector("a.pb-row-link")?.getAttribute("href")).toBe(
    "https://doi.org/10.1234/a%20b%23c%3Fd",
  );
});

test("a label that already repeats its identifier shows it once, on the link", async () => {
  const snap = snapshot({
    items: [
      paper({ localId: "id-1", identifier: { kind: "doi", value: "10.1234/abcd.5678" }, label: "Trust in peer review doi:10.1234/abcd.5678" }),
      paper({ localId: "id-2", identifier: { kind: "doi", value: "10.1234/wxyz.1111" }, label: "Replication at scale (DOI: 10.1234/WXYZ.1111)" }),
      paper({ localId: "id-3", identifier: { kind: "arxiv", value: "2101.00001" }, label: "https://arxiv.org/abs/2101.00001 — Attention revisited" }),
    ],
  });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [eligibleStatus("id-1"), eligibleStatus("id-2"), eligibleStatus("id-3")]),
  );

  expect(row(page.document, "id-1")?.querySelector(".pb-row-label")?.textContent).toBe("Trust in peer review");
  expect(row(page.document, "id-2")?.querySelector(".pb-row-label")?.textContent).toBe("Replication at scale");
  expect(row(page.document, "id-3")?.querySelector(".pb-row-label")?.textContent).toBe("Attention revisited");
  // Display-only: the identifier itself is untouched and still submitted.
  expect(row(page.document, "id-2")?.querySelector("a.pb-row-link")?.textContent).toBe("10.1234/wxyz.1111");
  expect(row(page.document, "id-2")?.querySelector(".pb-row-identifier")?.textContent).toBe("DOI: 10.1234/wxyz.1111");
});

test("a label that is nothing but its identifier keeps its text rather than rendering an unnamed row", async () => {
  const snap = snapshot({
    items: [paper({ localId: "id-1", identifier: { kind: "doi", value: "10.1234/abcd.5678" }, label: "10.1234/abcd.5678" })],
  });
  const page = await pageBulkDocument("scan-1", standardReply(snap, [eligibleStatus("id-1")]));
  expect(row(page.document, "id-1")?.querySelector(".pb-row-label")?.textContent).toBe("10.1234/abcd.5678");
  expect(checkbox(page.document, "id-1")?.getAttribute("aria-label")).toBe("Select 10.1234/abcd.5678");
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

// --- collapsed ownership-unclear state ---------------------------------------

test("when every non-invalid row is ownership_incomplete the badges collapse into one note", async () => {
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
      { local_id: "id-1", canonical_key: "work:1", status: "ownership_incomplete", ownership_complete: false },
      { local_id: "id-2", canonical_key: "work:2", status: "ownership_incomplete", ownership_complete: false },
      // An invalid row never had ownership to check, so it does not break the collapse.
      { local_id: "id-3", status: "invalid", ownership_complete: false },
    ]),
  );

  const note = page.document.getElementById("ownership-unclear-note");
  expect(note?.hidden).toBe(false);
  expect(note?.textContent?.trim()).toBe(
    "papio can't check your library from this daemon configuration; duplicates are still prevented when you acquire",
  );
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")).toBeNull();
  expect(row(page.document, "id-2")?.querySelector(".pb-row-status")).toBeNull();
  // Suppressed for that state only — the invalid row keeps its own badge.
  expect(row(page.document, "id-3")?.querySelector(".pb-row-status")?.textContent).toBe("Not a recognized identifier");
  // Collapsing is display-only: the rows stay eligible and selectable.
  expect(checkbox(page.document, "id-1")?.disabled).toBe(false);
});

test("a mixed status result keeps per-row ownership badges and hides the collapsed note", async () => {
  const snap = snapshot({
    items: [paper({ localId: "id-1" }), paper({ localId: "id-2", identifier: { kind: "pmid", value: "111" } })],
  });
  const page = await pageBulkDocument(
    "scan-1",
    standardReply(snap, [
      { local_id: "id-1", canonical_key: "work:1", status: "ownership_incomplete", ownership_complete: false },
      eligibleStatus("id-2"),
    ]),
  );

  expect(page.document.getElementById("ownership-unclear-note")?.hidden).toBe(true);
  expect(row(page.document, "id-1")?.querySelector(".pb-row-status")?.textContent).toBe("Ownership unclear");
  expect(row(page.document, "id-2")?.querySelector(".pb-row-status")?.textContent).toBe("Eligible");
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

test("selecting more than 50 keeps the complete manifest and has no client-side cap", async () => {
  const items = Array.from({ length: 60 }, (_, i) => paper({ localId: `id-${i}`, identifier: { kind: "doi", value: `10.1/${i}` } }));
  const snap = snapshot({ items });
  const statusItems = items.map((item) => eligibleStatus(item.localId, `work:${item.localId}`));
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, mode: "v2", processed_count: 60, submitted: 60, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_v2" };
    }
    return standardReply(snap, statusItems)(message);
  });

  for (const item of items) {
    const box = checkbox(page.document, item.localId)!;
    box.checked = true;
    box.dispatchEvent(new Event("change", { bubbles: true }));
  }
  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();

  expect(submitted).toHaveLength(1);
  expect(submitted[0]?.request["canonical_keys"]).toEqual(items.map((item) => `work:${item.localId}`));
  expect(page.document.getElementById("result-summary")?.textContent).not.toContain("Progress covers");
  for (const item of items) expect(row(page.document, item.localId)?.dataset["status"]).toBe("submitted");
});

// --- submit payload, results, remainder retained ----------------------------

test("submitting Acquire all sends every eligible canonical key plus the source block", async () => {
  const snap = snapshot({ items: [paper({ localId: "id-1" }), paper({ localId: "id-2", identifier: { kind: "pmid", value: "555" } })] });
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, mode: "v2", processed_count: 2, submitted: 2, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_1" };
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

test("v1 fallback marks only the processed prefix and names the limited submission", async () => {
  const items = Array.from({ length: 55 }, (_, i) => paper({ localId: `id-${i}`, identifier: { kind: "doi", value: `10.1/${i}` } }));
  const snap = snapshot({ items });
  const statusItems = items.map((item) => eligibleStatus(item.localId, `work:${item.localId}`));
  const submitted: RuntimeRequest[] = [];
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.submit") {
      submitted.push(message);
      return { ok: true, mode: "v1", processed_count: 50, submitted: 50, joined: 0, already_owned: 0, invalid: 0, batch_id: "batch_v1" };
    }
    return standardReply(snap, statusItems)(message);
  });
  (page.document.getElementById("primary-btn") as HTMLButtonElement).click();
  await settle();
  const keys = submitted[0]?.request["canonical_keys"] as string[];
  expect(keys).toHaveLength(55);
  expect(keys).toEqual(items.map((item) => `work:${item.localId}`));
  expect(page.document.getElementById("result-summary")?.textContent).toContain("Progress covers this 50-item submission");
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
      return { ok: true, mode: "v2", processed_count: 1, submitted: 3, joined: 2, already_owned: 1, invalid: 1, batch_id: "batch_9" };
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
  expect(link?.getAttribute("href")).toBe("chrome-extension://test-id/dist/ui/inbox.html");
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

test("a stale generation-1 status reply that resolves after a rescan never overwrites the fresh generation-2 rows", async () => {
  const initial = snapshot({ documentGeneration: 1, items: [paper({ localId: "id-1" })] });
  const rescanned = snapshot({
    documentGeneration: 2,
    items: [paper({ localId: "id-1" }), paper({ localId: "id-9", identifier: { kind: "pmid", value: "999" } })],
  });
  const { promise: staleStatusReply, resolve: resolveStaleStatus } = Promise.withResolvers<unknown>();
  let statusAttempts = 0;
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: initial };
    if (message.type === "papio.pageBulk.rescan") return { ok: true, snapshot: rescanned };
    if (message.type === "papio.pageBulk.status") {
      statusAttempts += 1;
      // The generation-1 request (issued by the initial load) never
      // resolves on its own — it is resolved by hand, after the rescan
      // below has already applied generation-2 rows. The generation-2
      // request (issued by handleRescan's own loadStatus() call) resolves
      // immediately with real statuses.
      if (statusAttempts === 1) return staleStatusReply;
      return { ok: true, items: [eligibleStatus("id-1"), eligibleStatus("id-9")], truncated: false };
    }
    return { ok: true, allowed: false };
  });

  expect(statusAttempts).toBe(1);

  (page.document.getElementById("rescan-btn") as HTMLButtonElement).click();
  await settle();

  expect(statusAttempts).toBe(2);
  expect(page.document.querySelectorAll(".pb-row")).toHaveLength(2);
  expect(checkbox(page.document, "id-1")?.disabled).toBe(false);

  // Resolve the stale generation-1 reply late, claiming id-1 is already
  // owned (which would disable and mark it "Already in your library" if it
  // were wrongly applied to the current, generation-2 row).
  resolveStaleStatus({
    ok: true,
    items: [{ local_id: "id-1", canonical_key: "work:stale", status: "owned_with_pdf", ownership_complete: true }],
    truncated: false,
  });
  await settle();

  expect(checkbox(page.document, "id-1")?.disabled).toBe(false);
  expect(row(page.document, "id-1")?.dataset["status"]).toBe("eligible");
});

test("the status Retry button is disabled while a Rescan is in flight, like the Rescan button itself", async () => {
  const snap = snapshot();
  const { promise: rescanReply, resolve: resolveRescan } = Promise.withResolvers<unknown>();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.status" && message.request["scan_id"] === "scan-1") {
      return { ok: false, error: { code: "unavailable", message: "status failed" } };
    }
    if (message.type === "papio.pageBulk.rescan") return rescanReply;
    return standardReply(snap, [eligibleStatus("id-1")])(message);
  });

  expect(page.document.getElementById("status-error")?.hidden).toBe(false);
  const retryButton = page.document.getElementById("status-retry-btn") as HTMLButtonElement;
  expect(retryButton.disabled).toBe(false);

  (page.document.getElementById("rescan-btn") as HTMLButtonElement).click();
  await settle();
  expect(retryButton.disabled).toBe(true);

  resolveRescan({ ok: true, snapshot: snap });
  await settle();
  expect(retryButton.disabled).toBe(false);
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

test("a connection_lost status failure uses papio copy, reloads on storage reconnect, and keeps acquisition honest while unknown", async () => {
  vi.useFakeTimers();
  try {
    const snap = snapshot();
    let statusAttempts = 0;
    const page = await pageBulkDocument("scan-1", (message) => {
      if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
      if (message.type === "papio.pageBulk.status") {
        statusAttempts += 1;
        if (statusAttempts === 1) return { ok: false, error: "connection_lost", message: "raw channel closed text" };
        return { ok: true, items: [eligibleStatus("id-1")], truncated: false };
      }
      return { ok: true, allowed: false };
    });

    expect(page.document.getElementById("status-error-message")?.textContent).toBe(
      "papio lost its connection to the daemon and is retrying…",
    );
    expect(page.document.getElementById("primary-btn")?.textContent).toBe("Acquire papers — checking availability…");
    expect((page.document.getElementById("primary-btn") as HTMLButtonElement).disabled).toBe(true);

    await page.emitStorageChange({
      papio_state_v1: {
        oldValue: { connectionStatus: "disconnected" },
        newValue: { connectionStatus: "connected" },
      },
    });
    await settle();
    expect(statusAttempts).toBe(2);
    expect(page.document.getElementById("status-error")?.hidden).toBe(true);
    await page.emitStorageChange({
      papio_state_v1: {
        oldValue: { connectionStatus: "connected" },
        newValue: { connectionStatus: "connected", daemonVersion: "new" },
      },
    });
    expect(statusAttempts).toBe(2);
    vi.advanceTimersByTime(500);
    await settle();
    expect(statusAttempts).toBe(2);
    expect((page.document.getElementById("primary-btn") as HTMLButtonElement).textContent).toBe("Acquire all 1 eligible");
  } finally {
    vi.useRealTimers();
  }
});

test("a Chrome receiving-end rejection uses papio connection copy instead of raw runtime text", async () => {
  vi.useFakeTimers();
  try {
    const snap = snapshot();
    let statusAttempts = 0;
    const page = await pageBulkDocument("scan-1", (message) => {
      if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
      if (message.type === "papio.pageBulk.status") {
        statusAttempts += 1;
        if (statusAttempts === 1) throw new Error("Could not establish connection. Receiving end does not exist.");
        return { ok: true, items: [eligibleStatus("id-1")], truncated: false };
      }
      return { ok: true, allowed: false };
    });

    expect(page.document.getElementById("status-error-message")?.textContent).toBe(
      "papio lost its connection to the daemon and is retrying…",
    );
    vi.advanceTimersByTime(500);
    await settle();
    expect(statusAttempts).toBe(2);
  } finally {
    vi.useRealTimers();
  }
});

/** `thrownErrorMessage` used to compute a transport condition and then return
 * the connection copy on both branches, so every page-side throw claimed the
 * daemon connection had dropped. That fabricated cause misattributed a real
 * page-bulk failure (an unregistered correlated reply type) for several
 * debugging rounds. Freshness rule 12: a failure degrades explicitly, and an
 * invented cause is as dishonest as an invented zero. */
test("a non-transport status throw names the real failure and neither blames nor retries the connection", async () => {
  vi.useFakeTimers();
  try {
    const snap = snapshot();
    let statusAttempts = 0;
    const page = await pageBulkDocument("scan-1", (message) => {
      if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
      if (message.type === "papio.pageBulk.status") {
        statusAttempts += 1;
        if (statusAttempts === 1) throw new Error("Unregistered reply type page_bulk_status_v2");
        return { ok: true, items: [eligibleStatus("id-1")], truncated: false };
      }
      return { ok: true, allowed: false };
    });

    const shown = page.document.getElementById("status-error-message")?.textContent;
    expect(shown).toBe("papio could not complete that request: Unregistered reply type page_bulk_status_v2");
    expect(shown).not.toBe("papio lost its connection to the daemon and is retrying…");
    expect(page.document.getElementById("status-error")?.hidden).toBe(false);

    // Nothing transport-shaped failed, so nothing reschedules itself.
    vi.advanceTimersByTime(500);
    await settle();
    expect(statusAttempts).toBe(1);

    // The explicit Retry control still works for this class of failure.
    (page.document.getElementById("status-retry-btn") as HTMLButtonElement).click();
    await settle();
    expect(statusAttempts).toBe(2);
    expect(page.document.getElementById("status-error")?.hidden).toBe(true);
  } finally {
    vi.useRealTimers();
  }
});

test("a daemon-unavailable throw from background keeps the connection copy and still schedules the retry", async () => {
  vi.useFakeTimers();
  try {
    const snap = snapshot();
    let statusAttempts = 0;
    const page = await pageBulkDocument("scan-1", (message) => {
      if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
      if (message.type === "papio.pageBulk.status") {
        statusAttempts += 1;
        if (statusAttempts === 1) throw new Error("the daemon is unavailable");
        return { ok: true, items: [eligibleStatus("id-1")], truncated: false };
      }
      return { ok: true, allowed: false };
    });

    expect(page.document.getElementById("status-error-message")?.textContent).toBe(
      "papio lost its connection to the daemon and is retrying…",
    );
    vi.advanceTimersByTime(500);
    await settle();
    expect(statusAttempts).toBe(2);
    expect(page.document.getElementById("status-error")?.hidden).toBe(true);
  } finally {
    vi.useRealTimers();
  }
});

test("an over-long thrown message is whitespace-collapsed and bounded, never a raw multi-line dump", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.load") return { ok: true, snapshot: snap };
    if (message.type === "papio.pageBulk.status") {
      throw new Error(`runtime blew up\n   ${"x".repeat(400)} trailing-detail-that-must-not-render`);
    }
    return { ok: true, allowed: false };
  });

  const prefix = "papio could not complete that request: ";
  const shown = page.document.getElementById("status-error-message")?.textContent ?? "";
  expect(shown.startsWith(`${prefix}runtime blew up x`)).toBe(true);
  expect(shown).not.toContain("\n");
  expect(shown).not.toContain("trailing-detail-that-must-not-render");
  // Same bound as inbox.ts's boundedProse: 237 characters plus an ellipsis.
  expect(shown.length).toBe(prefix.length + 238);
  expect(shown.endsWith("…")).toBe(true);
});

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
  expect(box.checked).toBe(false);
});

test("the allowlist checkbox is disabled while its set request is pending", async () => {
  const snap = snapshot();
  let resolveSet: (value: unknown) => void = () => undefined;
  const setPending = new Promise<unknown>((resolve) => {
    resolveSet = resolve;
  });
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.allowlist.set") return setPending;
    return standardReply(snap, [eligibleStatus("id-1")], true)(message);
  });
  const box = page.document.getElementById("allowlist-checkbox") as HTMLInputElement;
  expect(box.checked).toBe(true);

  box.checked = false;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();

  expect(box.disabled).toBe(true);
  expect(box.checked).toBe(true);

  resolveSet({ ok: true, allowed: false });
  await settle();

  expect(box.disabled).toBe(false);
  expect(box.checked).toBe(false);
});

test("a failed allowlist set reverts the checkbox and shows a row-local error", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.allowlist.set") {
      return { ok: false, error: { code: "internal", message: "Could not save scanner consent" } };
    }
    return standardReply(snap, [eligibleStatus("id-1")], true)(message);
  });
  const box = page.document.getElementById("allowlist-checkbox") as HTMLInputElement;
  const message = page.document.getElementById("allowlist-message");

  box.checked = false;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();

  expect(box.checked).toBe(true);
  expect(message?.hidden).toBe(false);
  expect(message?.textContent).toBe("Could not save scanner consent");
});

test("a scanner_consent_required rescan keeps the snapshot and refuses without a second rescan until consent returns", async () => {
  const snap = snapshot();
  let rescanCalls = 0;
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.rescan") {
      rescanCalls += 1;
      if (rescanCalls === 1) {
        return {
          ok: false,
          error: {
            code: "scanner_consent_required",
            message: "Allow scanning on this site before papio reads the page",
          },
        };
      }
      return { ok: true, snapshot: { ...snap, documentGeneration: 2, items: snap.items } };
    }
    if (message.type === "papio.pageBulk.allowlist.set") {
      return { ok: true, allowed: message.request["allowed"] === true };
    }
    return standardReply(snap, [eligibleStatus("id-1")], false)(message);
  });

  expect(row(page.document, "id-1")).not.toBeNull();
  const rescanButton = page.document.getElementById("rescan-btn") as HTMLButtonElement;
  rescanButton.click();
  await settle();

  expect(rescanCalls).toBe(1);
  expect(row(page.document, "id-1")).not.toBeNull();
  expect(page.document.getElementById("status-error-message")?.textContent).toContain(
    "Allow scanning on this site before papio reads the page",
  );

  const box = page.document.getElementById("allowlist-checkbox") as HTMLInputElement;
  box.checked = true;
  box.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();

  rescanButton.click();
  await settle();

  expect(rescanCalls).toBe(2);
  expect(page.document.getElementById("status-error")?.hidden).toBe(true);
});

test("a source_changed rescan shows its detail and hides Rescan without rebinding", async () => {
  const snap = snapshot();
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.rescan") {
      return {
        ok: false,
        error: {
          code: "source_changed",
          message: "The source tab moved to another site — start a new scan",
        },
      };
    }
    return standardReply(snap)(message);
  });

  const rescanButton = page.document.getElementById("rescan-btn") as HTMLButtonElement;
  rescanButton.click();
  await settle();

  expect(row(page.document, "id-1")).not.toBeNull();
  expect(rescanButton.hidden).toBe(true);
  expect(page.document.getElementById("status-error-message")?.textContent).toBe(
    "The source tab moved to another site — start a new scan",
  );
  expect(page.requests.filter((r) => r.type === "papio.pageBulk.load").length).toBe(1);
});

test("reopening a workspace pulls settled grab state and never renders Ready to grab", async () => {
  const item = {
    ...paper({ kind: "pdf_grab", url: "https://resolver.example.edu/content/paper.pdf", title: "A paper" }),
    grab_id: "grab-settled-0001",
    grab_state: "quarantined",
  } as DetectedPaper & { grab_id: string; grab_state: string };
  const snap = snapshot({ items: [item] });
  const page = await pageBulkDocument("scan-1", (message) => {
    if (message.type === "papio.pageBulk.grabStatus") {
      expect(message.request).toEqual({ grab_id: "grab-settled-0001" });
      return { ok: true, grab_id: "grab-settled-0001", state: "job_created", outcome: "job_created" };
    }
    return standardReply(snap)(message);
  });
  const rendered = row(page.document, item.localId);
  expect(rendered?.textContent).toContain("Job created");
  expect(page.requests.some((request) => request.type === "papio.pageBulk.status")).toBe(false);
  expect(rendered?.textContent).not.toContain("Ready to grab");
  expect(rendered?.querySelector("button")?.disabled).toBe(true);
  await page.emitRuntimeMessage({
    type: "papio.pageBulk.grabState",
    scan_id: "scan-1",
    grab_id: "grab-settled-0001",
    state: "identifying",
    detail: "pdf grab is already settled",
  });
  expect(row(page.document, item.localId)?.textContent).toContain("Identifying");
});
