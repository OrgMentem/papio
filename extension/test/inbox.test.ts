// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test, vi } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";
import type { TriageCounts, TriageSnapshotItem } from "../src/protocol";

// happy-dom's SVG namespace constructor is used by the details disclosure
// renderer; keep the test DOM whitelist explicit as the page gains controls.
interface FixtureSnapshot {
  schema: 1 | 2 | 3 | 4 | 5;
  generated_at: string;
  counts: TriageCounts;
  items: TriageSnapshotItem[];
  cursor?: string;
  has_more: boolean;
  unsupported_items_count: number;
}

interface RuntimeRequest {
  type: string;
  request: Record<string, unknown>;
}

// happy-dom's Window has no prompt(); confirm_request_exists uses
// window.prompt to collect the operator-supplied provider reference, so
// tests stub it through this narrow, named augmentation rather than an
// inline cast.
type WindowWithPrompt = Window & { prompt: (message?: string, defaultValue?: string) => string | null };

let importSerial = 0;

async function settle(): Promise<void> {
  for (let iteration = 0; iteration < 12; iteration += 1) await Promise.resolve();
}

async function inboxDocument(
  reply: (message: RuntimeRequest) => unknown | Promise<unknown>,
  options: { downloadSteering?: boolean; daemonFeatures?: string[] } = {},
): Promise<{ document: Document; window: Window; requests: RuntimeRequest[]; opened: string[] }> {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/inbox.html", import.meta.url), "utf8"));
  const requests: RuntimeRequest[] = [];
  const opened: string[] = [];
  window.open = ((url?: string | URL) => {
    if (typeof url === "string") opened.push(url);
    return null;
  }) as typeof window.open;
  Object.assign(globalThis, {
    window,
    document: window.document,
    Event: window.Event,
    KeyboardEvent: window.KeyboardEvent,
    Element: window.Element,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLInputElement: window.HTMLInputElement,
    HTMLTimeElement: window.HTMLTimeElement,
    HTMLSelectElement: window.HTMLSelectElement,
    chrome: {
      runtime: {
        sendMessage: async (message: RuntimeRequest) => {
          requests.push(message);
          return reply(message);
        },
      },
      // Chrome exposes chrome.downloads.onDeterminingFilename; Firefox has no
      // equivalent. The page reads that capability, never the user agent, so
      // the two platform routes are exercised by presence alone.
      downloads: options.downloadSteering === true
        ? { onDeterminingFilename: { addListener: () => undefined } }
        : {},
      // chrome.storage backs loadDaemonFeatures' read of the persisted
      // BrokerStore (the same chromeBackend(chrome.storage).load() popup.ts
      // already uses) — present only when a test needs the picker gated
      // open. Its absence is itself a fixture: refreshInbox's
      // loadDaemonFeatures then no-ops, daemonFeatures stays [], and the
      // picker stays gated closed, exercising the old-daemon fallback with
      // no extra setup.
      ...(options.daemonFeatures !== undefined
        ? {
            storage: {
              local: {
                get: async () => ({
                  papio_state_v1: { activeJobs: [], daemonFeatures: options.daemonFeatures },
                }),
                set: async () => undefined,
              },
            },
          }
        : {}),
    },
  });
  importSerial += 1;
  // Each fixture needs a fresh page module because its UI state is intentionally module-local.
  await import(`../src/inbox.ts?inbox-test=${importSerial}`);
  await settle();
  return {
    document: window.document as unknown as Document,
    window,
    requests,
    opened,
  };
}

function counts(overrides: Partial<TriageCounts> = {}): TriageCounts {
  return {
    pending_total: 4,
    watch_hits: 1,
    actions: 2,
    retractions: 1,
    jobs_working: 0,
    jobs_needs_review: 2,
    failure_groups_7d: 0,
    ...overrides,
  };
}

const sha256 = "a".repeat(64);
const previewURL = "http://127.0.0.1:43123/p/capability";

function watchHit(id: string, rank: number, title: string, links: TriageSnapshotItem["links"] = [{ rel: "doi", url: "https://doi.org/10.1/example" }]): TriageSnapshotItem {
  return {
    kind: "watch_hit",
    id,
    rank,
    title,
    facts: [{ label: "Watch", text: "Focused reading" }],
    links,
    ops: ["acquire", "dismiss", "open"],
    work: { doi: "10.1/example", title, authors: "Researcher", year: 2026, is_oa: true },
    abstract: "A useful abstract.",
    watches: [{ id: 1, label: "Focused reading" }],
    first_seen_at: "2026-07-21T10:00:00Z",
  };
}

function verifyIdentity(id = "action:1", rank = 1): TriageSnapshotItem {
  return {
    kind: "human_action",
    id,
    rank,
    title: "Verified PDF title",
    facts: [{ label: "Reason", text: "Identity needs review" }],
    links: [{ rel: "landing", url: "https://example.test/paper" }],
    ops: ["accept", "reject"],
    action_id: 17,
    job_id: "job-17",
    action_kind: "verify_identity",
    job_state: "needs_review",
    revision: 4,
    sha256,
    size_bytes: 99,
  };
}

function manualAction(id: string, rank: number, title: string): TriageSnapshotItem {
  return {
    kind: "human_action",
    id,
    rank,
    title,
    facts: [{ label: "Action", text: "manual download" }],
    links: [],
    ops: ["dismiss"],
    action_id: 18,
    job_id: "job-18",
    action_kind: "manual_download",
    job_state: "needs_review",
    revision: 1,
    sha256: "",
    size_bytes: 0,
  };
}

function handoffAction(id: string, rank: number, requiresAuth: boolean): TriageSnapshotItem {
  return {
    kind: "human_action",
    id,
    rank,
    title: "Browser handoff article",
    facts: [{ label: "Action", text: "browser handoff ready" }],
    links: [{ rel: "doi", url: "https://doi.org/10.1234/handoff" }],
    ops: ["open"],
    action_id: 19,
    job_id: `job_handoff_${id.replace("action:", "")}`,
    action_kind: "openurl_handoff",
    job_state: "needs_review",
    revision: 1,
    sha256: "",
    size_bytes: 0,
    requires_auth: requiresAuth,
    blocked_by: requiresAuth ? "paywall" : "anti_bot",
  };
}

function documentDeliveryAction(
  id: string,
  rank: number,
  attention: "working" | "required",
  state: "offered" | "unknown_outcome" | "fulfilled" | "declined" | "cancelled",
): TriageSnapshotItem {
  return {
    kind: "human_action",
    id,
    rank,
    title: "document delivery",
    facts: [{ label: "Action", text: "document delivery" }],
    links: [],
    ops: ["open_request_history", "confirm_request_exists", "confirm_request_absent"],
    attention,
    action_id: 21,
    job_id: "job-delivery-21",
    action_kind: "document_delivery",
    job_state: "awaiting_human",
    revision: 1,
    sha256: "",
    size_bytes: 0,
    route_class: "document_delivery",
    auth_requirement: "unknown",
    delivery: { provider: "illiad", provider_reference: "TN-42", state },
  };
}


function downloadsAccessAction(id: string, rank: number, jobState = "awaiting_human"): TriageSnapshotItem {
  return {
    kind: "human_action",
    id,
    rank,
    title: "downloads access required",
    facts: [
      { label: "Action", text: "downloads access required" },
      { label: "Detail", text: "/Users/example/Downloads/papio" },
    ],
    links: [],
    ops: ["dismiss"],
    attention: "required",
    action_id: 22,
    job_id: "job-downloads-access-22",
    action_kind: "downloads_access_required",
    job_state: jobState,
    revision: 1,
    sha256: "",
    size_bytes: 0,
    route_class: "downloads_access_required",
    auth_requirement: "unknown",
  };
}

function retraction(id: string, rank: number, title: string): TriageSnapshotItem {
  return {
    kind: "retraction",
    id,
    rank,
    title,
    facts: [{ label: "Nature", text: "Retraction" }],
    links: [{ rel: "doi", url: "https://doi.org/10.1/retracted" }],
    ops: ["open"],
    doi: "10.1/retracted",
    nature: "retraction",
    noticed_at: "2026-07-21T10:00:00Z",
  };
}
function pdfGrab(grabID = "grab_test_1", label = "Reading copy"): TriageSnapshotItem {
  return {
    kind: "pdf_grab",
    label,
    grab: { grab_id: grabID, state: "parked_no_identifier" },
    route_class: "pdf_identifier_needed",
    blocked_by: "identifier_missing",
    attention: "required",
    ops: ["provide_identifier", "dismiss"],
  } as TriageSnapshotItem;
}

function snapshot(items: TriageSnapshotItem[], options: Partial<FixtureSnapshot> = {}): FixtureSnapshot {
  return {
    schema: 1,
    generated_at: "2026-07-21T10:00:00Z",
    counts: counts(),
    items,
    has_more: false,
    unsupported_items_count: 0,
    ...options,
  };
}

function snapshotReply(fixture: FixtureSnapshot, message: RuntimeRequest): unknown {
  if (message.type === "papio.triage.snapshot") return { ok: true, snapshot: fixture };
  if (message.type === "papio.triage.counts") return { ok: true, counts: fixture.counts, generated_at: fixture.generated_at };
  if (message.type === "papio.activity") return { ok: true, feature: false, entries: [] };
  return { ok: false, error: { code: "unexpected", message: "Unexpected message" } };
}

function key(document: Document, value: string): void {
  document.dispatchEvent(new KeyboardEvent("keydown", { key: value, bubbles: true }));
}

// Leaving the page commits whatever is still inside its undo window; tests use
// it to reach the daemon call without waiting out the real timer.
function flush(window: Window): void {
  window.dispatchEvent(new window.Event("pagehide"));
}

test("keeps the turn count on the counts line, inventory on the tabs, and liveness on the pulse line", async () => {
  const fixture = snapshot([], {
    counts: counts({ pending_total: 116, turns_required: 34, actions: 0, jobs_working: 116, watch_hits: 0, retractions: 0 }),
  });
  const pulse = (inFlight: number, continuing: number) => ({
    ok: true,
    available: true,
    worker_epoch: "worker-live",
    received_at: Date.now(),
    pulse: {
      request_id: "pulse-live",
      schema: 1,
      generated_at: new Date().toISOString(),
      projection_complete: true,
      nonterminal_total: 116,
      in_flight: inFlight,
      continuing,
      scheduled: 82,
      waiting_required: 34,
      stalled: 0,
    },
  });
  const idlePage = await inboxDocument((message) => {
    if (message.type === "papio.work.pulse") return pulse(0, 0);
    return snapshotReply(fixture, message);
  });
  const idleCounts = idlePage.document.getElementById("inbox-counts")?.textContent ?? "";
  // The tabs already carry pending inventory, so the header must not repeat it.
  expect(idleCounts).toBe("34 need you");
  expect(idleCounts).not.toContain("116");
  expect(idleCounts).not.toMatch(/open|for reference/);
  expect(idleCounts).not.toMatch(/working on|working through/);
  expect(idlePage.document.getElementById("inbox-pulse")?.hidden).toBe(true);

  const movingPage = await inboxDocument((message) => {
    if (message.type === "papio.work.pulse") return pulse(2, 5);
    return snapshotReply(fixture, message);
  });
  expect(movingPage.document.getElementById("inbox-counts")?.textContent).not.toContain("working on");
  expect(movingPage.document.getElementById("inbox-pulse")?.textContent).toContain("papio is working on 7");
  expect(movingPage.document.getElementById("inbox-pulse")?.textContent).not.toContain("116");
});

// The counts line is the only place the effective turn total appears, so every
// way the daemon can withhold or qualify it has to reach the operator intact.
test("names an unreported turn total instead of rendering it as zero", async () => {
  // A daemon that never negotiated counts-v3 sends inventory without
  // turns_required. There are five open actions; "Nothing needs you" would be
  // a lie and "0 need you" would be an invented number.
  const fixture = snapshot([], {
    counts: counts({ pending_total: 5, actions: 5, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const line = page.document.getElementById("inbox-counts")?.textContent ?? "";
  expect(line).toBe("papio hasn't reported how many need you");
  expect(line).not.toMatch(/\d/);
  // The inventory the daemon did send still reaches the operator on the tabs.
  expect(page.document.getElementById("actions-tab")?.textContent).toBe("Actions (5)");
});

test("keeps the exact turn total when the daemon omits the per-turn projection", async () => {
  // required_turns_complete=false drops the item-level list only; the daemon
  // keeps the attention count exact (internal/triage/triage.go). Only the
  // toolbar badge suppresses the number there, and for its own reason.
  const fixture = snapshot([], {
    counts: counts({
      pending_total: 1300,
      turns_required: 1290,
      required_turns_complete: false,
      actions: 1300,
      watch_hits: 0,
      retractions: 0,
    }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("1290 need you");
});

test("says nothing needs you when the daemon reports zero turns beside open references", async () => {
  // Zero turns is not an empty inbox: four watch hits stay on their tab. The
  // old "No open items" line claimed the whole inventory was clear.
  const fixture = snapshot([], {
    counts: counts({ pending_total: 4, turns_required: 0, actions: 0, watch_hits: 4, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("Nothing needs you");
  expect(page.document.getElementById("watch-tab")?.textContent).toBe("Watch hits (4)");
});

test("renders rank-ordered bands, label:text facts, and only safe HTTPS links", async () => {
  const unsafe = watchHit("hit:unsafe", 3, "Watch hit", [
    { rel: "doi", url: "javascript:alert(1)" },
    { rel: "landing", url: "https://example.test/safe" },
  ]);
  const fixture = snapshot([
    unsafe,
    manualAction("action:manual", 2, "Manual action"),
    verifyIdentity("action:verify", 8),
    retraction("retraction:doi", 4, "Retraction notice"),
  ], { has_more: true, cursor: "next-page" });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(Array.from(page.document.querySelectorAll(".triage-group > h2"), (heading) => heading.textContent)).toEqual([
    "Retractions (1)",
  ]);
  expect(Array.from(page.document.querySelectorAll(".triage-item h3"), (heading) => heading.firstChild?.textContent)).toEqual([
    "Retraction notice",
    "Manual action",
    "Verified PDF title",
    "Watch hit",
  ]);
  expect(page.document.querySelector(".item-facts dt")?.textContent).toBe("Nature");
  expect(page.document.querySelector(".item-facts dd")?.textContent).toBe("Retraction");
  expect(Array.from(page.document.querySelectorAll<HTMLAnchorElement>("a"), (anchor) => anchor.href)).toContain("https://example.test/safe");
  expect(Array.from(page.document.querySelectorAll<HTMLAnchorElement>("a"), (anchor) => anchor.href)).not.toContain("javascript:alert(1)");
  expect(page.document.querySelector("time")?.textContent).toBe("generated at 2026-07-21T10:00:00Z");
  expect(page.document.getElementById("load-more")?.hidden).toBe(false);
  page.document.getElementById("load-more")?.dispatchEvent(new Event("click", { bubbles: true }));
  await settle();
  expect(page.requests.filter((request) => request.type === "papio.triage.snapshot")[1]?.request.cursor).toBe("next-page");
});

test("keyboard navigation moves rows and verify_identity is preview-gated and confirmed", async () => {
  const fixture = snapshot([
    verifyIdentity(),
    manualAction("action:later", 2, "Later manual action"),
  ], { counts: counts({ pending_total: 2, actions: 2, watch_hits: 0, retractions: 0 }) });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.preview") {
      return { ok: true, outcome: "ok", preview: { url: previewURL, sha256, size_bytes: 99, expires_at: "2026-07-21T10:10:00Z" } };
    }
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  const itemRows = page.document.querySelectorAll<HTMLElement>("[data-triage-item-id]");
  expect(itemRows[0]?.dataset.triageItemId).toBe("action:1");
  itemRows[0]?.focus();
  key(page.document, "j");
  expect(page.document.activeElement?.getAttribute("data-triage-item-id")).toBe("action:later");
  key(page.document, "k");
  expect(page.document.activeElement?.getAttribute("data-triage-item-id")).toBe("action:1");
  const input = page.document.createElement("input");
  page.document.body.append(input);
  input.focus();
  input.dispatchEvent(new KeyboardEvent("keydown", { key: "j", bubbles: true }));
  expect(itemRows[0]?.tabIndex).toBe(0);
  input.remove();

  const accept = page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:1'] [data-operation='accept']");
  expect(accept?.disabled).toBe(true);
  page.document.querySelector<HTMLButtonElement>("[data-operation='preview']")?.click();
  await settle();
  expect(page.opened).toEqual([previewURL]);
  expect(page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:1'] [data-operation='accept']")?.disabled).toBe(false);

  key(page.document, "a");
  expect(page.document.getElementById("confirm-dialog")?.hidden).toBe(false);
  expect(page.requests.filter((request) => request.type === "papio.action.resolve")).toHaveLength(0);
  key(page.document, "j");
  expect(page.document.activeElement?.getAttribute("data-triage-item-id")).not.toBe("action:later");

  page.document.getElementById("confirm-submit")?.dispatchEvent(new Event("click", { bubbles: true }));
  await settle();
  expect(page.requests.find((request) => request.type === "papio.action.resolve")?.request).toEqual({
    action_id: 17,
    verdict: "accept",
    expected_revision: 4,
    expected_sha256: sha256,
  });
});

test("institutional handoff Open uses the broker rather than its canonical DOI", async () => {
  const item = handoffAction("action:institutional", 1, true);
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.handoff.open") return { ok: true, opened: true };
    return snapshotReply(fixture, message);
  });

  expect(page.document.querySelector("[data-triage-item-id='action:institutional'] .item-citation a")?.getAttribute("href"))
    .toBe("https://doi.org/10.1234/handoff");
  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:institutional'] [data-operation='open']")?.click();
  await settle();

  expect(page.requests.filter((request) => request.type === "papio.handoff.open")).toEqual([{
    type: "papio.handoff.open",
    request: { job_id: "job_handoff_institutional" },
  }]);
  expect(page.opened).toEqual([]);
  expect(page.document.getElementById("operation-status")?.textContent).toBe("Browser handoff opened.");
});
test("waiting sibling overlay is browser-local, suppresses primary focus, and lapses at deadline", async () => {
  vi.useFakeTimers();
  try {
    const item = handoffAction("action:waiting", 1, true);
    item.attention = "required";
    const fixture = snapshot([item], {
      counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    });
    const deadline = Date.now() + 30;
    const page = await inboxDocument((message) => {
      if (message.type === "papio.triage.waiting") {
        return { ok: true, waiting_jobs: [{ job_id: "job_handoff_waiting", deadline }] };
      }
      return snapshotReply(fixture, message);
    });
    const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:waiting']");
    expect(row?.dataset.attention).toBe("working");
    expect(row?.querySelector(".item-guidance")?.textContent).toBe(
      "papio is continuing — waiting for the institution sign-in already open in another tab",
    );
    expect(row?.querySelector("[data-operation='open']")).toBeNull();
    vi.advanceTimersByTime(50);
    page.document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
    await settle();
    const expiredRow = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:waiting']");
    expect(expiredRow?.dataset.attention).toBe("required");
    expect(expiredRow?.querySelector(".item-guidance")?.textContent).toBe("Sign in to your institution");
  } finally {
    vi.useRealTimers();
  }
});
test("hoists one byte-identical family instruction above its ranked rows", async () => {
  const items = [1, 2, 3].map((rank) => {
    const item = manualAction(`action:family-${rank}`, rank, `Family paper ${rank}`);
    item.run_key = "run_family_manual";
    item.next_actor = "researcher";
    item.guidance_variant = "manual_download";
    item.operation_variant = "dismiss_only";
    return item;
  });
  const fixture = snapshot(items, {
    schema: 5,
    counts: counts({
      pending_total: 3,
      actions: 3,
      watch_hits: 0,
      retractions: 0,
      turns_required: 3,
      turns_working: 0,
      family_breakdown_complete: true,
      family_runs: [{
        run_key: "run_family_manual",
        first_rank: 1,
        route_class: "manual_download",
        action_kind: "manual_download",
        next_actor: "researcher",
        guidance_variant: "manual_download",
        operation_variant: "dismiss_only",
        count: 3,
      }],
    }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.querySelectorAll(".family-heading")).toHaveLength(1);
  expect(page.document.querySelectorAll(".family-guidance")).toHaveLength(1);
  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(3);

  // Card corners must follow the family block, not tag position. Before this was
  // fixed the CSS keyed on `:first-of-type`/`:last-of-type`, which are scoped to
  // element type, so with headings interleaved only the section's first and last
  // <article> were marked and every interior boundary rendered square.
  const rows = Array.from(page.document.querySelectorAll<HTMLElement>("[data-triage-item-id]"));
  expect(rows.map((row) => row.dataset.cardStart ?? "")).toEqual(["true", "", ""]);
  expect(rows.map((row) => row.dataset.cardEnd ?? "")).toEqual(["", "", "true"]);
  for (const row of rows) {
    const startsCard = row.previousElementSibling?.classList.contains("triage-item") !== true;
    const endsCard = row.nextElementSibling?.classList.contains("triage-item") !== true;
    expect(row.dataset.cardStart === "true").toBe(startsCard);
    expect(row.dataset.cardEnd === "true").toBe(endsCard);
  }
});

// Reproduces the live defect: a two-row family followed by two singleton
// families. Keying card edges on sibling type swept the singletons into the
// pair's card, where a heading reading "2 papers" sat above four rows of three
// different kinds. Each block now heads its own card, singleton or not, so the
// count above a card is always the count of that card.
test("a singleton family starts its own card under its own heading", async () => {
  const family = [1, 2].map((rank) => {
    const item = manualAction(`action:pair-${rank}`, rank, `Paired paper ${rank}`);
    item.run_key = "run_pair";
    item.next_actor = "researcher";
    item.guidance_variant = "manual_download";
    item.operation_variant = "dismiss_only";
    return item;
  });
  const loners = [3, 4].map((rank) => {
    const item = manualAction(`action:lone-${rank}`, rank, `Lone paper ${rank}`);
    item.run_key = `run_lone_${rank}`;
    item.next_actor = "researcher";
    item.guidance_variant = "manual_download";
    item.operation_variant = rank === 3 ? "dismiss_only" : "open_and_dismiss";
    return item;
  });
  const run = (key: string, operation: string, count: number, firstRank: number) => ({
    run_key: key,
    first_rank: firstRank,
    route_class: "manual_download",
    action_kind: "manual_download",
    next_actor: "researcher",
    guidance_variant: "manual_download",
    operation_variant: operation,
    count,
  });
  const fixture = snapshot([...family, ...loners], {
    schema: 5,
    counts: counts({
      pending_total: 4,
      actions: 4,
      watch_hits: 0,
      retractions: 0,
      turns_required: 4,
      turns_working: 0,
      family_breakdown_complete: true,
      family_runs: [
        run("run_pair", "dismiss_only", 2, 1),
        run("run_lone_3", "dismiss_only", 1, 3),
        run("run_lone_4", "open_and_dismiss", 1, 4),
      ],
    }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  const rows = Array.from(page.document.querySelectorAll<HTMLElement>("[data-triage-item-id]"));
  expect(rows).toHaveLength(4);
  // Each block is one card, and each card is headed by its own count.
  expect(rows.map((row) => row.dataset.cardStart ?? "")).toEqual(["true", "", "true", "true"]);
  expect(rows.map((row) => row.dataset.cardEnd ?? "")).toEqual(["", "true", "true", "true"]);
  expect(Array.from(page.document.querySelectorAll(".family-heading"), (node) => node.textContent)).toEqual([
    "Manual downloads · 2 papers",
    "Manual downloads · 1 paper",
    "Manual downloads · 1 paper",
  ]);
  // No heading may claim rows that belong to the block below it: every header
  // is immediately followed by the first row of its own card.
  for (const header of Array.from(page.document.querySelectorAll(".family-header"))) {
    let sibling = header.nextElementSibling;
    while (sibling !== null && !sibling.classList.contains("triage-item")) sibling = sibling.nextElementSibling;
    expect((sibling as HTMLElement | null)?.dataset.cardStart).toBe("true");
  }
});

type GuidanceVariant = NonNullable<TriageSnapshotItem["guidance_variant"]>;

// One manual-download family of `details.length` rows. A null detail means the
// daemon shipped no reason prose for that row.
function manualFamilyFixture(variant: GuidanceVariant, details: ReadonlyArray<string | null>): FixtureSnapshot {
  const runKey = `run_${variant}`;
  const items = details.map((detail, index) => {
    const item = manualAction(`action:${variant}-${index + 1}`, index + 1, `Paper ${index + 1}`);
    item.run_key = runKey;
    item.next_actor = "researcher";
    item.guidance_variant = variant;
    item.operation_variant = "dismiss_only";
    if (detail !== null) item.facts = [...item.facts, { label: "Detail", text: detail }];
    return item;
  });
  return snapshot(items, {
    schema: 5,
    counts: counts({
      pending_total: items.length,
      actions: items.length,
      watch_hits: 0,
      retractions: 0,
      turns_required: items.length,
      turns_working: 0,
      family_breakdown_complete: true,
      family_runs: [{
        run_key: runKey,
        first_rank: 1,
        route_class: "manual_download",
        action_kind: "manual_download",
        next_actor: "researcher",
        guidance_variant: variant,
        operation_variant: "dismiss_only",
        count: items.length,
      }],
    }),
  });
}

// Five genuinely different situations mint a manual download. Collapsing them
// into one family told six researchers to re-download the exact file papio had
// already rejected, so each one owns its heading and its imperative.
const MANUAL_FAMILY_COPY: ReadonlyArray<[GuidanceVariant, string, string]> = [
  ["manual_download", "Manual downloads · 2 papers", "Download each PDF — papio takes it from there."],
  [
    "manual_download_adapter_missing",
    "Manual downloads · no adapter yet · 2 papers",
    "papio has no adapter for these providers yet. Download each PDF — papio takes it from there.",
  ],
  [
    "manual_download_page_undriveable",
    "Manual downloads · page changed · 2 papers",
    "papio could not drive these provider pages. Download each PDF — papio takes it from there.",
  ],
  [
    "manual_download_rejected_file",
    "Replace rejected files · 2 papers",
    "The file papio adopted was not the paper. Download a different PDF for each.",
  ],
  [
    "manual_download_wrong_work",
    "Wrong paper reached · 2 papers",
    "papio landed on a different work. Find and download the requested PDF.",
  ],
];

test("each manual-download variant renders its own heading and instruction", async () => {
  const rendered: string[] = [];
  for (const [variant, heading, instruction] of MANUAL_FAMILY_COPY) {
    const page = await inboxDocument((message) => snapshotReply(manualFamilyFixture(variant, [null, null]), message));
    expect(page.document.querySelector(".family-heading")?.textContent).toBe(heading);
    expect(page.document.querySelector(".family-guidance")?.textContent).toBe(instruction);
    rendered.push(`${heading}|${instruction}`);
  }
  // No two situations may read alike.
  expect(new Set(rendered).size).toBe(MANUAL_FAMILY_COPY.length);
});

test("a family block prints its instruction exactly once however many rows it holds", async () => {
  const fixture = manualFamilyFixture("manual_download", Array.from({ length: 12 }, () => null));
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const list = page.document.getElementById("item-list");

  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(12);
  expect((list?.textContent ?? "").split("Download each PDF — papio takes it from there.")).toHaveLength(2);
  // The hoisted paragraph is the only guidance line in the block; no row
  // repeats it as its own instruction.
  expect(page.document.querySelectorAll(".item-guidance")).toHaveLength(1);
});

test("a rejected file cannot be mistaken for an ordinary manual download", async () => {
  const plain = await inboxDocument((message) =>
    snapshotReply(manualFamilyFixture("manual_download", [null, null]), message));
  const rejected = await inboxDocument((message) =>
    snapshotReply(manualFamilyFixture("manual_download_rejected_file", [null, null]), message));

  const plainBadge = plain.document.querySelector<HTMLElement>(".item-status");
  const rejectedBadge = rejected.document.querySelector<HTMLElement>(".item-status");
  expect(plainBadge?.textContent).toBe("↓");
  expect(rejectedBadge?.textContent).toBe("↺");
  expect(rejectedBadge?.dataset.status).toBe("manual_download_rejected_file");
  expect(rejectedBadge?.getAttribute("aria-label")).toBe("Rejected file — download a different PDF");

  // The trap this closes: sending the researcher back for the same file papio
  // already refused.
  const copy = rejected.document.querySelector(".family-guidance")?.textContent ?? "";
  expect(copy).toContain("Download a different PDF");
  expect(copy).not.toContain("papio takes it from there");
});

// A lone gate row used to arrive with no instruction at all: the per-row path
// has no case for a security check, publisher terms or an institution sign-in,
// and the block path refused to render under two rows. The header now renders
// at any count, and its one-row wording never claims a plural.
test("a lone family row is headed and instructed at one paper", async () => {
  const page = await inboxDocument((message) =>
    snapshotReply(manualFamilyFixture("manual_download_rejected_file", [null]), message));

  expect(page.document.querySelector(".family-heading")?.textContent).toBe("Replace rejected files · 1 paper");
  expect(page.document.querySelector(".family-guidance")?.textContent).toBe(
    "The file papio adopted was not the paper. Download a different PDF.",
  );
  // One instruction, hoisted — the row does not restate it.
  expect(page.document.querySelectorAll(".item-guidance")).toHaveLength(1);
  expect(page.document.querySelector(".triage-item .item-guidance")).toBeNull();
});

// The three kinds the review named: a lone one of each reaches the operator
// with a route, where before it reached them with a title and nothing else.
test("a lone security check, terms gate and institution sign-in each carry an instruction", async () => {
  const cases: ReadonlyArray<[GuidanceVariant, string, string]> = [
    ["security_challenge", "Security checks · 1 paper", "Solve each security check in its tab."],
    ["terms_acceptance", "Publisher terms · 1 paper", "Review and accept the publisher terms for each source."],
    [
      "institution_sign_in",
      "Institution sign-in · 1 paper",
      "Sign in to your institution once — papio continues the waiting papers.",
    ],
  ];
  for (const [variant, heading, instruction] of cases) {
    const item = handoffAction(`action:${variant}`, 1, true);
    item.run_key = `run_${variant}`;
    item.next_actor = "researcher";
    item.guidance_variant = variant;
    item.operation_variant = "open_and_dismiss";
    const fixture = snapshot([item], {
      schema: 5,
      counts: counts({
        pending_total: 1,
        actions: 1,
        watch_hits: 0,
        retractions: 0,
        turns_required: 1,
        turns_working: 0,
        family_breakdown_complete: true,
        family_runs: [{
          run_key: `run_${variant}`,
          first_rank: 1,
          route_class: "openurl_handoff",
          action_kind: "openurl_handoff",
          next_actor: "researcher",
          guidance_variant: variant,
          operation_variant: "open_and_dismiss",
          count: 1,
        }],
      }),
    });
    const page = await inboxDocument((message) => snapshotReply(fixture, message));
    expect(page.document.querySelector(".family-heading")?.textContent).toBe(heading);
    expect(page.document.querySelector(".family-guidance")?.textContent).toBe(instruction);
    // The instruction is the block's, and the row's controls name it.
    const described = page.document.querySelector("button[data-operation]")?.getAttribute("aria-describedby") ?? "";
    expect(described.split(" ")).toContain(page.document.querySelector(".family-guidance")?.id ?? "");
  }
});

test("a row whose reason differs from its family's shows one bounded reason line", async () => {
  const fixture = manualFamilyFixture("manual_download_page_undriveable", [
    "the publisher page changed shape; download the requested PDF yourself and papio will adopt it",
    "the download control never responded; download the requested PDF yourself and papio will adopt it",
  ]);
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  const reasons = Array.from(page.document.querySelectorAll(".item-reason")).map((node) => node.textContent);
  expect(reasons).toEqual(["the publisher page changed shape", "the download control never responded"]);
  // A reason is the part the heading cannot carry — never a second copy of the
  // hoisted instruction, and never the whole prose blob.
  for (const reason of reasons) {
    expect(reason).not.toContain("papio takes it from there");
    expect(reason).not.toContain("download the requested PDF yourself");
  }
  // The full prose stays recoverable behind the row's disclosure.
  const debug = page.document.querySelector<HTMLDListElement>(".item-debug");
  expect(debug?.hidden).toBe(true);
  expect(debug?.textContent).toContain("download the requested PDF yourself and papio will adopt it");

  // Rule 13: the row's controls name the hoisted instruction, the platform
  // route, and this row's own reason.
  const row = page.document.querySelector("[data-triage-item-id='action:manual_download_page_undriveable-1']");
  const described = row?.querySelector("button[data-operation]")?.getAttribute("aria-describedby") ?? "";
  const familyID = page.document.querySelector(".family-guidance")?.id ?? "";
  const reasonID = page.document.querySelector(".item-reason")?.id ?? "";
  expect(familyID).not.toBe("");
  expect(reasonID).not.toBe("");
  expect(described.split(" ")).toContain(familyID);
  expect(described.split(" ")).toContain(reasonID);
});

test("a family whose rows share one reason prints no per-row reason line", async () => {
  const shared = "no source-controlled adapter matched this provider";
  const fixture = manualFamilyFixture("manual_download_adapter_missing", [shared, shared, shared]);
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(3);
  expect(page.document.querySelectorAll(".item-reason")).toHaveLength(0);
  expect(page.document.querySelectorAll(".family-guidance")).toHaveLength(1);
});

test("an over-long or multi-line row reason is bounded to one line", async () => {
  const long = `${"unbounded provider prose ".repeat(40)}tail`;
  const fixture = manualFamilyFixture("manual_download_page_undriveable", [
    long,
    "a\n\n  short   second\treason",
  ]);
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const reasons = Array.from(page.document.querySelectorAll(".item-reason")).map((node) => node.textContent ?? "");

  expect(long.length).toBeGreaterThan(400);
  expect(reasons[0]?.length).toBeLessThanOrEqual(120);
  expect(reasons[0]?.endsWith("…")).toBe(true);
  // Whitespace is collapsed exactly as every other untrusted string is.
  expect(reasons[1]).toBe("a short second reason");
});

test("the manual-download route is honest on each platform and names no path", async () => {
  const withoutSteering = await inboxDocument((message) =>
    snapshotReply(manualFamilyFixture("manual_download", [null, null]), message));
  const route = withoutSteering.document.querySelector(".family-mechanism")?.textContent ?? "";
  expect(route).toBe(
    "This browser cannot pass a saved download to papio. Open the PDF and use Send PDF in the papio toolbar popup instead.",
  );
  // One block, one route line — not one per row.
  expect(withoutSteering.document.querySelectorAll(".family-mechanism")).toHaveLength(1);

  const withSteering = await inboxDocument(
    (message) => snapshotReply(manualFamilyFixture("manual_download", [null, null]), message),
    { downloadSteering: true },
  );
  expect(withSteering.document.querySelector(".family-mechanism")).toBeNull();
  const mechanism = withSteering.document.querySelector(".item-mechanism")?.textContent ?? "";
  expect(mechanism).toContain("papio picks the download up from your browser's downloads");

  // No surface may promise a folder: the adoption root is a daemon setting the
  // browser cannot see, and a job id is never shown to the researcher.
  for (const page of [withoutSteering, withSteering]) {
    const text = page.document.body.textContent ?? "";
    expect(text).not.toContain("Downloads/papio");
    expect(text).not.toContain("<job>");
    expect(text).not.toMatch(/papio\/[a-z<]/);
  }
});

// The daemon's own manual-download details, verbatim from
// internal/browser/bridge.go. Each one restates, in the daemon's words, the
// exact fact this page's family copy already hoists — and a literal comparison
// caught none of them, so every live manual-download family printed its reason
// twice.
const DAEMON_MANUAL_DETAILS: ReadonlyArray<[GuidanceVariant, string]> = [
  ["manual_download_wrong_work", "papio reached a different work; find and download the requested PDF yourself"],
  [
    "manual_download_page_undriveable",
    "papio could not drive the provider page; download the PDF yourself and papio will adopt it",
  ],
  ["manual_download_adapter_missing", "papio has no adapter for this provider yet; download the PDF yourself for now"],
];

test("a reason the hoisted instruction already states in other words prints once", async () => {
  for (const [variant, detail] of DAEMON_MANUAL_DETAILS) {
    const page = await inboxDocument((message) =>
      snapshotReply(manualFamilyFixture(variant, [detail, detail, detail]), message));

    expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(3);
    expect(page.document.querySelectorAll(".family-guidance")).toHaveLength(1);
    expect(page.document.querySelectorAll(".item-reason")).toHaveLength(0);
    // Still recoverable in the row's disclosure; only the second visible copy
    // is gone.
    expect(page.document.querySelector(".item-debug")?.textContent).toContain(detail.split(";")[0]);
  }
});

// The suppression is a closed table of known equivalences, not a similarity
// score: a reason the block does not already state must survive it.
test("a reason the block does not state survives the equivalence table", async () => {
  const page = await inboxDocument((message) =>
    snapshotReply(
      manualFamilyFixture("manual_download_wrong_work", [
        "papio reached a different work; find and download the requested PDF yourself",
        "the resolver returned a preprint of another paper",
      ]),
      message,
    ));

  expect(Array.from(page.document.querySelectorAll(".item-reason"), (node) => node.textContent)).toEqual([
    "the resolver returned a preprint of another paper",
  ]);
});

// The cliff risk 4 named: one action the daemon could not map anywhere in the
// snapshot makes it report its family breakdown incomplete. The surface used to
// answer that by dropping every block header and printing one instruction per
// row. Blocks are derived from the loaded rows now, so only the run's total
// count depends on the daemon's breakdown.
test("an incomplete family breakdown keeps the header and prints the instruction once", async () => {
  const fixture = manualFamilyFixture("manual_download_page_undriveable", [null, null, null, null, null]);
  const page = await inboxDocument((message) =>
    snapshotReply({ ...fixture, counts: { ...fixture.counts, family_breakdown_complete: false, family_runs: [] } }, message));
  const list = page.document.getElementById("item-list");

  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(5);
  expect(page.document.querySelector(".family-heading")?.textContent).toBe("Manual downloads · page changed · 5 papers");
  expect(page.document.querySelectorAll(".family-guidance")).toHaveLength(1);
  expect(page.document.querySelectorAll(".triage-item .item-guidance")).toHaveLength(0);
  expect((list?.textContent ?? "").split("papio could not drive these provider pages")).toHaveLength(2);
});

// An older daemon ships no guidance quartet at all, so no authored copy exists
// for these rows. The row's own imperative is hoisted verbatim under the
// heading its action kind earns, instead of being repeated five times.
test("rows with no guidance variant hoist their own imperative under a kind heading", async () => {
  const items = [1, 2, 3, 4, 5].map((rank) => manualAction(`action:legacy-${rank}`, rank, `Legacy paper ${rank}`));
  const fixture = snapshot(items, {
    counts: counts({ pending_total: 5, actions: 5, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const list = page.document.getElementById("item-list");

  expect(page.document.querySelector(".family-heading")?.textContent).toBe("Manual downloads · 5 papers");
  expect(page.document.querySelector(".family-guidance")?.textContent).toBe("Download the PDF yourself - papio adopts it");
  expect(page.document.querySelectorAll(".triage-item .item-guidance")).toHaveLength(0);
  expect((list?.textContent ?? "").split("Download the PDF yourself - papio adopts it")).toHaveLength(2);
  // The Firefox caveat still reaches this block, once and unshortened.
  expect(Array.from(page.document.querySelectorAll(".family-mechanism"), (node) => node.textContent)).toEqual([
    "This browser cannot pass a saved download to papio. Open the PDF and use Send PDF in the papio toolbar popup instead.",
  ]);
});

// A lone row with no authored copy has nothing to repeat, so a header would
// only cost it a line: it keeps its instruction inline.
test("a lone row with no guidance variant keeps its instruction inline", async () => {
  const fixture = snapshot([manualAction("action:legacy", 1, "Legacy paper")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelectorAll(".family-heading")).toHaveLength(0);
  expect(page.document.querySelector(".triage-item .item-guidance")?.firstChild?.textContent)
    .toBe("Download the PDF yourself - papio adopts it");
});

// A sign-in already open in another tab is the browser's own knowledge, which
// the block's copy cannot describe. That row says so for itself and stays in
// the block, so one waiting row can no longer collapse a whole block back into
// an instruction per row.
test("a waiting row keeps its own line inside the block it still belongs to", async () => {
  vi.useFakeTimers();
  try {
    const items = [1, 2, 3].map((rank) => {
      const item = handoffAction(`action:waiting-${rank}`, rank, true);
      item.job_id = `job_waiting_${rank}`;
      item.attention = "required";
      item.run_key = "run_sign_in";
      item.next_actor = "researcher";
      item.guidance_variant = "institution_sign_in";
      item.operation_variant = "open_and_dismiss";
      item.ops = ["open", "dismiss"];
      return item;
    });
    const fixture = snapshot(items, {
      schema: 5,
      counts: counts({
        pending_total: 3,
        actions: 3,
        watch_hits: 0,
        retractions: 0,
        turns_required: 3,
        turns_working: 0,
        family_breakdown_complete: true,
        family_runs: [{
          run_key: "run_sign_in",
          first_rank: 1,
          route_class: "openurl_handoff",
          action_kind: "openurl_handoff",
          next_actor: "researcher",
          guidance_variant: "institution_sign_in",
          operation_variant: "open_and_dismiss",
          count: 3,
        }],
      }),
    });
    const page = await inboxDocument((message) => {
      if (message.type === "papio.triage.waiting") {
        return { ok: true, waiting_jobs: [{ job_id: "job_waiting_2", deadline: Date.now() + 60_000 }] };
      }
      return snapshotReply(fixture, message);
    });

    expect(page.document.querySelector(".family-heading")?.textContent).toBe("Institution sign-in · 3 papers");
    expect(page.document.querySelectorAll(".family-guidance")).toHaveLength(1);
    expect(Array.from(page.document.querySelectorAll(".triage-item .item-guidance"), (node) => node.textContent)).toEqual([
      "papio is continuing — waiting for the institution sign-in already open in another tab",
    ]);
    // One card, three rows: the block is not split by the row that differs.
    const rows = Array.from(page.document.querySelectorAll<HTMLElement>("[data-triage-item-id]"));
    expect(rows.map((row) => row.dataset.cardStart ?? "")).toEqual(["true", "", ""]);
    expect(rows.map((row) => row.dataset.cardEnd ?? "")).toEqual(["", "", "true"]);
    // The waiting row owns no decision, so it exposes no control; its line is
    // still addressable, and its siblings' controls still name the block.
    const waitingRow = page.document.querySelector("[data-triage-item-id='action:waiting-2']");
    expect(waitingRow?.querySelector("button[data-operation]")).toBeNull();
    expect(waitingRow?.querySelector(".item-guidance")?.id).not.toBe("");
    const sibling = page.document.querySelector("[data-triage-item-id='action:waiting-1']");
    const described = sibling?.querySelector("button[data-operation]")?.getAttribute("aria-describedby") ?? "";
    expect(described.split(" ")).toContain(page.document.querySelector(".family-guidance")?.id ?? "");
  } finally {
    vi.useRealTimers();
  }
});

test("keyboard o sends an OA browser handoff through the broker", async () => {
  const item = handoffAction("action:open-access", 1, false);
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.handoff.open") return { ok: true, opened: true };
    return snapshotReply(fixture, message);
  });

  page.document.querySelector<HTMLElement>("[data-triage-item-id='action:open-access']")?.focus();
  key(page.document, "o");
  await settle();

  expect(page.requests.filter((request) => request.type === "papio.handoff.open")).toEqual([{
    type: "papio.handoff.open",
    request: { job_id: "job_handoff_open-access" },
  }]);
  expect(page.opened).toEqual([]);
});

test("a handoff missing its job identifier is disabled and never falls back to its DOI", async () => {
  const item = handoffAction("action:missing-job", 1, true);
  delete item.job_id;
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const open = page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:missing-job'] [data-operation='open']");

  expect(open?.disabled).toBe(true);
  page.document.querySelector<HTMLElement>("[data-triage-item-id='action:missing-job']")?.focus();
  key(page.document, "o");
  await settle();

  expect(page.requests.filter((request) => request.type === "papio.handoff.open")).toHaveLength(0);
  expect(page.opened).toEqual([]);
  expect(page.document.querySelector("[data-triage-item-id='action:missing-job'] .item-result")?.textContent)
    .toBe("This browser handoff is missing its job identifier.");
});

test("a broker handoff rejection stays inline and leaves the action available", async () => {
  const item = handoffAction("action:rejected", 1, true);
  const failure = "Handoff could not be opened ".repeat(12);
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.handoff.open") return { ok: false, error: { code: "handoff_unavailable", message: failure } };
    return snapshotReply(fixture, message);
  });

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:rejected'] [data-operation='open']")?.click();
  await settle();

  const result = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:rejected'] .item-result");
  expect(result?.textContent).toBe(`${failure.slice(0, 237)}…`);
  expect(result?.dataset.tone).toBe("error");
  expect(page.document.querySelector("[data-triage-item-id='action:rejected']")).not.toBeNull();
  expect(page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:rejected'] [data-operation='open']")?.disabled).toBe(false);
});

test("a handoff opens while the inbox believes the daemon is reconnecting", async () => {
  // Regression: requestHandoffOpen used to run through beginMutation, whose
  // !state.connected gate blocked broker opens outright. The broker call is
  // local to the extension — the background owns the native session — so a
  // lagging inbox connectivity view must not stop it.
  const item = handoffAction("action:offline-open", 1, true);
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  let available = true;
  const page = await inboxDocument((message) => {
    if (message.type === "papio.handoff.open") return { ok: true, opened: true };
    if (!available) return { ok: false, error: { code: "disconnected", message: "Native host is down" } };
    return snapshotReply(fixture, message);
  });

  available = false;
  page.document.getElementById("refresh-inbox")?.click();
  await settle();
  expect(page.document.getElementById("connection-status")?.textContent).toContain("Disconnected");

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:offline-open'] [data-operation='open']")?.click();
  await settle();

  expect(page.requests.filter((request) => request.type === "papio.handoff.open")).toEqual([{
    type: "papio.handoff.open",
    request: { job_id: "job_handoff_offline-open" },
  }]);
  expect(page.opened).toEqual([]);
  expect(page.document.getElementById("operation-status")?.textContent).toBe("Browser handoff opened.");
});

test("a successful handoff retry clears the prior inline failure", async () => {
  const item = handoffAction("action:retry-clear", 1, true);
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  let attempts = 0;
  const page = await inboxDocument((message) => {
    if (message.type === "papio.handoff.open") {
      attempts += 1;
      if (attempts === 1) return { ok: false, error: { code: "handoff_unavailable", message: "No live tab for this job." } };
      return { ok: true, opened: true };
    }
    return snapshotReply(fixture, message);
  });

  const open = (): void =>
    page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:retry-clear'] [data-operation='open']")?.click();
  open();
  await settle();
  const failed = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:retry-clear'] .item-result");
  expect(failed?.textContent).toBe("No live tab for this job.");
  expect(failed?.dataset.tone).toBe("error");

  open();
  await settle();
  const result = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:retry-clear'] .item-result");
  expect(result?.textContent).toBe("Browser handoff opened.");
  expect(result?.dataset.tone).toBe("info");
  expect(attempts).toBe(2);
});

test("manual Open is a plain tab open that binds nothing, while watch links stay direct", async () => {
  const manual = manualAction("action:manual-open", 1, "Manual link");
  manual.job_id = "job_manual_open_0001";
  manual.links = [{ rel: "landing", url: "https://example.test/manual" }];
  manual.ops = ["open"];
  const hit = watchHit("hit:open", 2, "Watch link", [{ rel: "doi", url: "https://doi.org/10.1234/watch" }]);
  const fixture = snapshot([manual, hit], {
    counts: counts({ pending_total: 2, actions: 1, watch_hits: 1, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:manual-open'] [data-operation='open']")?.click();
  await settle();
  // The pre-pin is gone: Open no longer claims delivery authority for a job,
  // so it sends no runtime message at all. The popup's own picker carries that
  // intent at Send PDF time instead, where the researcher is actually looking
  // at the PDF.
  expect(page.requests.filter((request) => request.type === "papio.manual.open")).toEqual([]);
  expect(page.opened).toContain("https://example.test/manual");

  page.document.querySelector<HTMLElement>("[data-triage-item-id='hit:open']")?.focus();
  key(page.document, "o");
  expect(page.opened).toEqual(["https://example.test/manual", "https://doi.org/10.1234/watch"]);
});

test("a conflict leaves an inline refresh result and re-requests the snapshot", async () => {
  const fixture = snapshot([watchHit("hit:one", 1, "Conflict watch hit")], {
    counts: counts({ pending_total: 1, watch_hits: 1, actions: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.triage.decide") return { ok: true, outcome: "conflict" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLElement>("[data-triage-item-id='hit:one']")?.focus();
  key(page.document, "d");
  await settle();
  flush(page.window);
  await settle();
  expect(page.document.querySelector(".item-result")?.textContent).toBe("changed elsewhere — refreshed");
  expect(page.requests.filter((request) => request.type === "papio.triage.snapshot")).toHaveLength(2);
});
test("returning to the tab re-requests the snapshot so the inbox stays fresh", async () => {
  const fixture = snapshot([watchHit("hit:one", 1, "Fresh on return")], {
    counts: counts({ pending_total: 1, watch_hits: 1, actions: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.requests.filter((request) => request.type === "papio.triage.snapshot")).toHaveLength(1);
  page.document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
  await settle();
  expect(page.requests.filter((request) => request.type === "papio.triage.snapshot")).toHaveLength(2);
});
test("an automatic snapshot refresh preserves expanded item details", async () => {
  const originalSetTimeout = globalThis.setTimeout;
  const polls: Array<() => void> = [];
  globalThis.setTimeout = ((callback: () => void, delay?: number) => {
    if (delay === 15_000) {
      polls.push(callback);
      return 0;
    }
    return originalSetTimeout(callback, delay);
  }) as typeof globalThis.setTimeout;
  try {
    let fixture = snapshot([manualAction("action:expanded", 1, "Expanded details")], {
      counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    });
    const page = await inboxDocument((message) => snapshotReply(fixture, message));
    const toggle = page.document.querySelector<HTMLButtonElement>(
      "[data-triage-item-id='action:expanded'] .item-debug-toggle",
    );
    toggle?.click();
    expect(toggle?.getAttribute("aria-expanded")).toBe("true");
    expect(polls).toHaveLength(1);

    fixture = snapshot([manualAction("action:expanded", 1, "Expanded details")], {
      counts: counts({ pending_total: 2, actions: 1, watch_hits: 0, retractions: 0 }),
    });
    polls.shift()?.();
    await settle();

    const refreshedRow = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:expanded']");
    expect(refreshedRow?.querySelector(".item-debug-toggle")?.getAttribute("aria-expanded")).toBe("true");
    expect(refreshedRow?.querySelector<HTMLDListElement>(".item-debug")?.hidden).toBe(false);
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});


test("a daemon-down refresh leaves the page rendered, shows reconnect, and disables mutations", async () => {
  const fixture = snapshot([watchHit("hit:one", 1, "Still visible")], {
    counts: counts({ pending_total: 1, watch_hits: 1, actions: 0, retractions: 0 }),
  });
  let available = true;
  const page = await inboxDocument((message) => {
    if (!available) return { ok: false, error: { code: "disconnected", message: "Native host is down" } };
    return snapshotReply(fixture, message);
  });
  available = false;
  page.document.getElementById("refresh-inbox")?.dispatchEvent(new Event("click", { bubbles: true }));
  await settle();
  expect(page.document.getElementById("connection-status")?.textContent).toContain("Disconnected");
  expect(page.document.getElementById("reconnect-daemon")?.hidden).toBe(false);
  expect(page.document.querySelector<HTMLButtonElement>("[data-operation='acquire']")?.disabled).toBe(true);
  expect(page.document.querySelector("[data-triage-item-id='hit:one']")?.textContent).toContain("Still visible");
});

test("a rejected preview (business error) stays connected and does not disconnect", async () => {
  // Regression: the daemon rejecting one specific preview request (action
  // already resolved, quarantine file gone, …) used to come back from the
  // native bridge as a raw transport failure and got treated exactly like a
  // dead connection — the banner flipped to Disconnected on every click even
  // though nothing else was actually wrong. It must now surface as an
  // ordinary per-item error with the connection left alone.
  const fixture = snapshot([verifyIdentity()], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.preview") {
      return { ok: true, outcome: "error", detail: "review action 17 is unavailable" };
    }
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='preview']")?.click();
  await settle();
  expect(page.document.querySelector(".item-result")?.textContent).toBe("review action 17 is unavailable");
  expect(page.document.querySelector(".item-result")?.getAttribute("data-tone")).toBe("error");
  expect(page.document.getElementById("connection-status")?.hidden).toBe(true);
  expect(page.document.getElementById("reconnect-daemon")?.hidden).toBe(true);
  expect(page.opened).toEqual([]);
});

test("an acknowledged removal focuses the next triage row", async () => {
  const fixture = snapshot([
    watchHit("hit:first", 1, "First hit"),
    watchHit("hit:second", 2, "Second hit"),
  ], { counts: counts({ pending_total: 2, watch_hits: 2, actions: 0, retractions: 0 }) });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.triage.decide") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='hit:first'] [data-operation='acquire']")?.click();
  await settle();
  expect(page.document.querySelector("[data-triage-item-id='hit:first']")).toBeNull();
  expect(page.document.activeElement?.getAttribute("data-triage-item-id")).toBe("hit:second");
});

test("a dismissal removes the row at once, holds the daemon call, and undo puts it back", async () => {
  // Dismissal is deferred rather than confirmed: the modal protected nothing
  // (the daemon cannot un-cancel a job) while costing a click per row, so the
  // undo window is the safety net and no dialog may appear.
  const action = manualAction("action:manual", 1, "Manual action");
  action.attention = "required";
  const fixture = snapshot([action], {
    counts: counts({ pending_total: 1, turns_required: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  expect(page.document.getElementById("confirm-dialog")?.hidden).toBe(true);
  expect(page.document.querySelector("[data-triage-item-id='action:manual']")).toBeNull();
  expect(page.document.getElementById("undo-bar")?.hidden).toBe(false);
  // The header carries turns_required alone, so a local removal has to move it.
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("Nothing needs you");
  expect(page.requests.filter((request) => request.type === "papio.action.resolve")).toHaveLength(0);

  page.document.getElementById("undo-dismiss")?.dispatchEvent(new Event("click", { bubbles: true }));
  await settle();
  expect(page.document.querySelector("[data-triage-item-id='action:manual']")).not.toBeNull();
  expect(page.document.getElementById("undo-bar")?.hidden).toBe(true);
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("1 needs you");
  expect(page.requests.filter((request) => request.type === "papio.action.resolve")).toHaveLength(0);
});

test("a committed human_action dismissal calls papio.action.resolve, not triage.decide", async () => {
  // Regression: humanActionItems offers "dismiss" for non-review kinds
  // (manual_download, openurl_handoff), but the client's dismiss handler was
  // written only for watch_hit's papio.triage.decide RPC. Routing a
  // human_action dismiss through that path would silently no-op (the
  // server-side triage_decide handler can never find a "action:N" id in the
  // watch-hit table) and report a confusing "changed elsewhere" conflict.
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  flush(page.window);
  await settle();
  expect(page.requests.filter((request) => request.type === "papio.triage.decide")).toHaveLength(0);
  expect(page.requests.find((request) => request.type === "papio.action.resolve")?.request).toEqual({
    action_id: 18,
    verdict: "dismiss",
    expected_revision: 1,
  });
  expect(page.document.querySelector("[data-triage-item-id='action:manual']")).toBeNull();
  expect(page.document.getElementById("undo-bar")?.hidden).toBe(false);
});

test("the undo bar names a cancelled acquisition only when the job is parked on the action", async () => {
  // Mirrors dismissalCancelsParkedJob in internal/job/job.go: dismissing an
  // action a job is parked on cancels that job, while a leftover action on a
  // job that moved on just closes a dead row. The wording must not threaten
  // cancellation for the second case — that is the whole reason the blanket
  // confirmation was wrong.
  const parked = manualAction("action:parked", 1, "Parked download");
  parked.job_state = "awaiting_human";
  const stale = manualAction("action:stale", 2, "Stale residue");
  stale.action_id = 20;
  stale.job_state = "resolving";
  const fixture = snapshot([parked, stale], {
    counts: counts({ pending_total: 2, actions: 2, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:parked'] [data-operation='dismiss']")?.click();
  await settle();
  expect(page.document.getElementById("undo-message")?.textContent).toContain("cancelled its acquisition");

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='action:stale'] [data-operation='dismiss']")?.click();
  await settle();
  expect(page.document.getElementById("undo-message")?.textContent).toBe("Dismissed 2 items — 1 cancelled an acquisition.");

  flush(page.window);
  await settle();
  expect(page.requests.filter((request) => request.type === "papio.action.resolve").map((request) => request.request["action_id"]))
    .toEqual([18, 20]);
});

test("downloads_access_required renders the blocked-Downloads label and the adoption root, attention-required styled", async () => {
  const item = downloadsAccessAction("action:downloads-access", 1);
  const fixture = snapshot([item], {
    schema: 3,
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  const card = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:downloads-access']");
  expect(card?.dataset.attention).toBe("required");
  expect(card?.querySelector(".item-status")?.getAttribute("aria-label")).toBe("Downloads folder access needed");
  const instruction = card?.querySelector(".item-instruction")?.textContent ?? "";
  expect(instruction).toContain("papio can't read /Users/example/Downloads/papio");
  expect(instruction).toContain("System Settings");
});

// Mirrors dismissalCancelsParkedJob's deliberate exclusion in
// internal/job/job.go: the pending download is fine, only the Downloads
// folder grant is missing, so dismissing this kind must never threaten (or
// perform) cancellation — unlike manual_download parked on the same state.
test("dismissing downloads_access_required never cancels the parked job", async () => {
  const item = downloadsAccessAction("action:downloads-access-dismiss", 1, "awaiting_human");
  const fixture = snapshot([item], {
    schema: 3,
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>(
    "[data-triage-item-id='action:downloads-access-dismiss'] [data-operation='dismiss']",
  )?.click();
  await settle();
  expect(page.document.getElementById("undo-message")?.textContent).toBe("Dismissed “downloads access required”.");

  flush(page.window);
  await settle();
  expect(page.requests.find((request) => request.type === "papio.action.resolve")?.request).toEqual({
    action_id: 22,
    verdict: "dismiss",
    expected_revision: 1,
  });
});

test("a structured broker rejection renders inline and never fakes a disconnect", async () => {
  // Regression: a stale worker rejected dismisses with invalid_request and
  // the inbox rendered it as "Disconnected: … Reconnecting automatically",
  // hiding the real error and lying about connectivity. A structured reply
  // proves the messaging path is alive; only a thrown call is a disconnect.
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  let throwNow = false;
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve") {
      if (throwNow) throw new Error("The message port closed before a response was received.");
      return { ok: false, error: { code: "invalid_request", message: "Invalid action resolution request" } };
    }
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  flush(page.window);
  await settle();

  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:manual'] .item-result");
  expect(row?.textContent).toBe("Invalid action resolution request");
  expect(row?.dataset.tone).toBe("error");
  expect(page.document.querySelector("[data-triage-item-id='action:manual']")).not.toBeNull();
  expect(page.document.getElementById("connection-status")?.hidden).toBe(true);
  expect(page.document.getElementById("reconnect-daemon")?.hidden).toBe(true);

  // A thrown runtime call is a real transport failure and does disconnect.
  throwNow = true;
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  flush(page.window);
  await settle();
  expect(page.document.getElementById("connection-status")?.textContent).toContain("Disconnected");
});

test("the inbox paints no connection verdict before the daemon answers", async () => {
  // The reported flash: `connected: false` is the initial value, not a verdict,
  // and the first paint read it as one — a red "Disconnected … run papio
  // status" on every open, before anything had been asked.
  vi.useFakeTimers();
  try {
    const unanswered = Promise.withResolvers<unknown>();
    const page = await inboxDocument((message) =>
      message.type === "papio.triage.snapshot" ? unanswered.promise : snapshotReply(snapshot([]), message),
    );
    const banner = page.document.getElementById("connection-status");
    expect(banner?.hidden).toBe(true);
    expect(banner?.dataset.state).toBeUndefined();
    expect(page.document.getElementById("reconnect-daemon")?.hidden).toBe(true);

    // A daemon that really is slow must not leave the page silent forever — but
    // it still must not be called disconnected before it has been given a turn.
    vi.advanceTimersByTime(1_000);
    expect(banner?.hidden).toBe(false);
    expect(banner?.textContent).toBe("Connecting to daemon…");
    expect(banner?.dataset.state).toBeUndefined();
  } finally {
    vi.useRealTimers();
  }
});

test("a session held by another browser is not reported as a lost connection", async () => {
  // The snapshot read is holder-independent in the daemon, but this browser is
  // negotiated for nothing until it is claimed. Calling that "Disconnected …
  // Reconnecting automatically — run papio status" sent the researcher hunting
  // a daemon that was answering the whole time.
  const fixture = snapshot([manualAction("action:busy", 1, "Manual action")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  let refuse = false;
  const page = await inboxDocument((message) => {
    if (refuse && message.type === "papio.triage.snapshot") {
      return {
        ok: false,
        error: {
          code: "session_busy",
          message: "Another browser holds the papio session; run 'papio browser use --latest' to move it here",
        },
      };
    }
    return snapshotReply(fixture, message);
  });

  refuse = true;
  page.document.getElementById("refresh-inbox")?.click();
  await settle();

  const banner = page.document.getElementById("connection-status");
  expect(banner?.hidden).toBe(false);
  expect(banner?.textContent).toBe(
    "Not this browser: Another browser holds the papio session; run 'papio browser use --latest' to move it here The inbox reconnects by itself once it does.",
  );
  expect(banner?.textContent).not.toContain("papio status");
});

test("the filter narrows visible items, keeps counts intact, and reports a distinct empty state", async () => {
  const fixture = snapshot([
    watchHit("hit:one", 1, "Attention and memory"),
    manualAction("action:manual", 2, "Cognitive load review"),
  ], { counts: counts({ pending_total: 2, turns_required: 1, actions: 1, watch_hits: 1, retractions: 0 }) });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const filterInput = page.document.getElementById("item-filter") as HTMLInputElement;

  filterInput.value = "memory";
  filterInput.dispatchEvent(new Event("input", { bubbles: true }));
  await settle();
  expect(Array.from(page.document.querySelectorAll("[data-triage-item-id]"), (row) => row.getAttribute("data-triage-item-id"))).toEqual(["hit:one"]);
  // Filtering narrows the view only: the daemon's turn count and the tab
  // labels that own inventory both stay whole.
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("1 needs you");
  expect(page.document.getElementById("actions-tab")?.textContent).toBe("Actions (1)");
  expect(page.document.getElementById("watch-tab")?.textContent).toBe("Watch hits (1)");


  filterInput.value = "no such paper exists";
  filterInput.dispatchEvent(new Event("input", { bubbles: true }));
  await settle();
  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(0);
  expect(page.document.querySelector("#item-list > p")?.textContent).toContain("No items match");

  filterInput.value = "";
  filterInput.dispatchEvent(new Event("input", { bubbles: true }));
  await settle();
  expect(page.document.querySelectorAll("[data-triage-item-id]")).toHaveLength(2);
});
test("renders v4 PDF grabs, guides identifier entry, and dismisses by grab identity", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.triage.decide") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1']");
  expect(row).not.toBeNull();
  expect(row?.dataset.attention).toBe("required");
  expect(row?.querySelector(".item-guidance")?.textContent).toContain(
    "papio grabs identify grab_test_1 --doi <value>",
  );

  row?.querySelector<HTMLButtonElement>("[data-operation='provide_identifier']")?.click();
  await settle();
  expect(page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1'] .item-result")?.textContent ?? "").toContain("papio grabs identify grab_test_1");

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] [data-operation='dismiss']")?.click();
  await settle();
  flush(page.window);
  await settle();
  expect(page.requests.find((request) => request.type === "papio.triage.decide")?.request).toEqual({
    item_id: "pdf_grab:grab_test_1",
    op: "dismiss",
    watch_scope: "all",
  });
});

test("the parked-capture picker renders the daemon's ranked suggestions with their evidence", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.grab.suggest") {
      return {
        ok: true,
        grab_id: "grab_test_1",
        outcome: "ok",
        document_identifiers: [],
        suggestions: [
          {
            job_id: "job_qualifies_1",
            title: "Attention Is All You Need",
            year: 2017,
            verdict: "qualifies",
            reason: "excerpt title matches the pending job",
            evidence: ["front-matter title matches", "DOI corroborated by excerpt"],
          },
          {
            job_id: "job_review_1",
            title: "A Different Paper",
            verdict: "review",
            evidence: ["partial title overlap only"],
          },
        ],
        truncated: false,
      };
    }
    return snapshotReply(fixture, message);
  }, { daemonFeatures: ["pdf_grab_suggest_v1"] });

  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1']");
  const button = row?.querySelector<HTMLButtonElement>("[data-operation='provide_identifier']");
  expect(button?.textContent).toBe("Which paper is this?");
  button?.click();
  await settle();

  expect(page.requests.find((request) => request.type === "papio.grab.suggest")?.request).toEqual({ grab_id: "grab_test_1" });
  const suggestions = page.document.querySelectorAll<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1'] .grab-picker-suggestion");
  expect(suggestions.length).toBe(2);
  expect(suggestions[0]?.querySelector(".grab-picker-suggestion-title")?.textContent).toBe("Attention Is All You Need (2017)");
  expect(suggestions[0]?.querySelector(".grab-picker-verdict")?.textContent).toBe("Qualifies");
  const evidence = Array.from(suggestions[0]?.querySelectorAll(".grab-picker-evidence li") ?? []).map((li) => li.textContent);
  expect(evidence).toEqual(["front-matter title matches", "DOI corroborated by excerpt"]);
  expect(suggestions[1]?.querySelector(".grab-picker-verdict")?.textContent).toBe("Needs review");
});

test("a document_identifiers-only suggest result shows what the file itself declares", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.grab.suggest") {
      return {
        ok: true,
        grab_id: "grab_test_1",
        outcome: "ok",
        document_identifiers: [{ kind: "doi", value: "10.1234/example", source: "xmp/prism:doi" }],
        suggestions: [],
        truncated: false,
      };
    }
    return snapshotReply(fixture, message);
  }, { daemonFeatures: ["pdf_grab_suggest_v1"] });

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] [data-operation='provide_identifier']")?.click();
  await settle();

  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1']");
  expect(row?.querySelector(".grab-picker-identifier-value")?.textContent).toBe("DOI: 10.1234/example");
  expect(row?.querySelector(".grab-picker-identifier-source")?.textContent).toBe("found in xmp/prism:doi");
  expect(row?.querySelector(".grab-picker-identifiers code")?.textContent).toBe(
    "papio grabs identify grab_test_1 --doi 10.1234/example",
  );
  expect(row?.querySelector(".grab-picker-suggestions")).toBeNull();
});

test("confirming a suggested candidate files the capture and settles the row", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.grab.suggest") {
      return {
        ok: true,
        grab_id: "grab_test_1",
        outcome: "ok",
        document_identifiers: [],
        suggestions: [
          { job_id: "job_qualifies_1", title: "Attention Is All You Need", verdict: "qualifies", evidence: ["front-matter title matches"] },
        ],
        truncated: false,
      };
    }
    if (message.type === "papio.grab.confirm") {
      return { ok: true, grab_id: "grab_test_1", job_id: "job_qualifies_1", outcome: "job_created" };
    }
    return snapshotReply(fixture, message);
  }, { daemonFeatures: ["pdf_grab_suggest_v1"] });

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] [data-operation='provide_identifier']")?.click();
  await settle();
  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] .grab-picker-confirm")?.click();
  await settle();

  expect(page.requests.find((request) => request.type === "papio.grab.confirm")?.request).toEqual({
    grab_id: "grab_test_1",
    job_id: "job_qualifies_1",
  });
  expect(page.document.querySelector("[data-triage-item-id='pdf_grab:grab_test_1']")).toBeNull();
});

test("a refused_identity confirm leaves the row present with the refusal visible", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.grab.suggest") {
      return {
        ok: true,
        grab_id: "grab_test_1",
        outcome: "ok",
        document_identifiers: [],
        suggestions: [
          { job_id: "job_review_1", title: "A Different Paper", verdict: "review", evidence: ["partial title overlap only"] },
        ],
        truncated: false,
      };
    }
    if (message.type === "papio.grab.confirm") {
      return {
        ok: true,
        grab_id: "grab_test_1",
        outcome: "refused_identity",
        detail: "excerpt names DOI 10.9/other, not the picked job's",
      };
    }
    return snapshotReply(fixture, message);
  }, { daemonFeatures: ["pdf_grab_suggest_v1"] });

  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] [data-operation='provide_identifier']")?.click();
  await settle();
  page.document.querySelector<HTMLButtonElement>("[data-triage-item-id='pdf_grab:grab_test_1'] .grab-picker-confirm")?.click();
  await settle();

  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1']");
  expect(row).not.toBeNull();
  const notice = row?.querySelector(".grab-picker-confirm-notice");
  expect(notice?.getAttribute("data-tone")).toBe("error");
  expect(notice?.textContent).toContain("papio refused this pick");
  expect(notice?.textContent).toContain("excerpt names DOI 10.9/other, not the picked job's");
  // The picker is still showing the same ranked candidate — nothing changed.
  expect(row?.querySelector(".grab-picker-suggestion")).not.toBeNull();
});

test("without the daemon's pdf_grab_suggest_v1 feature, provide_identifier keeps its guidance-only fallback", async () => {
  const fixture = snapshot([pdfGrab()], {
    schema: 4,
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='pdf_grab:grab_test_1']");
  const button = row?.querySelector<HTMLButtonElement>("[data-operation='provide_identifier']");
  expect(button?.textContent).toBe("Provide identifier");
  button?.click();
  await settle();

  expect(page.requests.some((request) => request.type === "papio.grab.suggest")).toBe(false);
  expect(row?.querySelector(".grab-picker")).toBeNull();
  expect(page.document.querySelector("[data-triage-item-id='pdf_grab:grab_test_1'] .item-result")?.textContent ?? "").toContain(
    "papio grabs identify grab_test_1",
  );
});

// Every other kind hoists one instruction for its whole block. A grab cannot:
// its imperative names its own grab id, so a block sentence would replace two
// distinct routes with one summary that acts on neither.
test("pdf grabs keep their own identify command instead of hoisting a summary", async () => {
  const grabs = [pdfGrab("grab_test_1", "Reading copy"), pdfGrab("grab_test_2", "Second copy")];
  for (const grab of grabs) grab.guidance_variant = "pdf_identifier";
  const fixture = snapshot(grabs, {
    schema: 5,
    counts: counts({ pending_total: 2, actions: 0, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelectorAll(".family-heading")).toHaveLength(0);
  expect(page.document.querySelector(".triage-group-pdf_grab > h2")?.textContent).toBe("PDF grabs (2)");
  expect(Array.from(page.document.querySelectorAll(".triage-item .item-guidance"), (node) => node.textContent)).toEqual([
    "Provide an identifier: papio grabs identify grab_test_1 --doi <value> (or --pmid/--arxiv <value>)",
    "Provide an identifier: papio grabs identify grab_test_2 --doi <value> (or --pmid/--arxiv <value>)",
  ]);
});

test("the action kind renders as a status glyph with an accessible label, not a fact row", async () => {
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const badge = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:manual'] .item-status");
  expect(badge?.dataset.status).toBe("manual_download");
  expect(badge?.getAttribute("aria-label")).toBe("Manual download needed");
  expect(badge?.dataset.label).toBe("Manual download needed");
  expect(page.document.querySelector("[data-triage-item-id='action:manual'] dd[data-fact='action']")).toBeNull();
});

test("expandable details carry mechanism copy and backend identifiers", async () => {
  const item = manualAction("action:manual", 1, "Manual action");

  item.facts = [
    { label: "Action", text: "manual download" },
    { label: "Authors", text: "Yann LeCun, Yoshua Bengio, Geoffrey Hinton" },
    { label: "Year", text: "2015" },
    { label: "Detail", text: "a resolver returned a landing page but no verified direct PDF" },
    { label: "Job", text: "job-18" },
  ];
  item.links = [{ rel: "doi", url: "https://doi.org/10.1038/nature14539" }];
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const row = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:manual']");

  // APA (default): inverted initials, parenthesized year, and the link shown
  // as the host it names with the full URL demoted onto the anchor.
  const citation = row?.querySelector(".item-citation");
  expect(citation?.textContent).toBe("LeCun, Y., Bengio, Y., & Hinton, G. (2015). doi.org");
  const anchor = citation?.querySelector("a");
  expect(anchor?.href).toBe("https://doi.org/10.1038/nature14539");
  expect(anchor?.title).toBe("https://doi.org/10.1038/nature14539");
  expect(anchor?.getAttribute("aria-label")).toBe("DOI: https://doi.org/10.1038/nature14539");

  const debug = row?.querySelector<HTMLDListElement>(".item-debug");
  const debugToggle = row?.querySelector<HTMLButtonElement>(".item-guidance .item-debug-toggle");
  expect(debugToggle?.querySelector("svg")).not.toBeNull();
  expect(debugToggle?.getAttribute("aria-label")).toBe("More details");
  expect(debugToggle?.getAttribute("aria-expanded")).toBe("false");
  expect(debug?.hidden).toBe(true);
  expect(row?.querySelector(".item-guidance")?.textContent?.startsWith("Download the PDF yourself")).toBe(true);
  expect(row?.querySelector(".item-mechanism")?.textContent).toContain(
    "a resolver returned a landing page but no verified direct PDF",
  );
  // The mechanism copy names the route, never a folder: the daemon's adoption
  // root is invisible to the browser and the researcher never learns a job id.
  expect(row?.querySelector(".item-mechanism")?.textContent).toContain(
    "Send PDF in the papio toolbar popup",
  );
  expect(row?.querySelector(".item-mechanism")?.textContent).not.toContain("Downloads/papio");
  debugToggle?.click();
  expect(debugToggle?.getAttribute("aria-expanded")).toBe("true");
  expect(debug?.hidden).toBe(false);
  const debugFields = debug === null || debug === undefined ? [] : Array.from(debug.querySelectorAll(".item-debug-field"));
  expect(debugFields.slice(1).map((field) => field.textContent)).toEqual([
    "itemaction:manual",
    "jobjob-18",
    "revision1",
  ]);
  expect(row?.querySelector(".item-debug")?.previousElementSibling).toBe(row?.querySelector(".item-guidance"));
  expect(row?.querySelector(".item-facts")).toBeNull();

  // Switching the style re-renders the citation in MLA.
  const select = page.document.getElementById("citation-style") as HTMLSelectElement;
  select.value = "mla";
  select.dispatchEvent(new Event("change", { bubbles: true }));
  await settle();
  expect(page.document.querySelector("[data-triage-item-id='action:manual'] .item-citation")?.textContent).toBe(
    "LeCun, Yann, et al. 2015, doi.org",
  );
});

// The host is a compression of an already-identified row, never a substitute
// for identity. A retraction notice carries no author and no year, so a bare
// `doi.org` would name every paper in the library equally.
test("a row with no author or year keeps its link's locator, not just its host", async () => {
  const fixture = snapshot([retraction("retraction:doi", 1, "Library update notice")], {
    counts: counts({ pending_total: 1, actions: 0, watch_hits: 0, retractions: 1 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const citation = page.document.querySelector("[data-triage-item-id='retraction:doi'] .item-citation");

  expect(citation?.textContent).toBe("doi.org/10.1/retracted");
  expect(citation?.querySelector("a")?.getAttribute("aria-label")).toBe("DOI: https://doi.org/10.1/retracted");
});

test("a missing adapter is named and its local diagnostic is explained", async () => {
  const item = manualAction("action:missing-adapter", 1, "Unsupported provider article");
  item.facts.push({
    label: "Detail",
    text:
      "papio has no adapter for this provider yet; download the PDF yourself for now; " +
      "a sanitized page diagnostic is saved locally; run 'papio adapter captures' to inspect it",
  });
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const row = page.document.querySelector<HTMLElement>(
    "[data-triage-item-id='action:missing-adapter']",
  );

  expect(row?.querySelector(".item-guidance")?.textContent).toContain(
    "No adapter yet - download this PDF manually",
  );
  expect(row?.querySelector(".item-mechanism")?.textContent).toContain(
    "run papio adapter captures to find it",
  );
});

test("access classification chooses one concise next action", async () => {
  const openAccess = handoffAction("action:open-access", 1, false);
  const institutional = handoffAction("action:institutional", 2, true);
  const manual = manualAction("action:manual-auth", 3, "Paywalled manual download");
  manual.requires_auth = true;
  manual.blocked_by = "paywall";
  const fixture = snapshot([openAccess, institutional, manual], {
    counts: counts({ pending_total: 3, actions: 3, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelector("[data-triage-item-id='action:open-access'] .item-guidance")?.textContent)
    .toContain("Open the page");
  expect(page.document.querySelector("[data-triage-item-id='action:institutional'] .item-guidance")?.textContent)
    .toContain("Sign in to your institution");
  expect(page.document.querySelector("[data-triage-item-id='action:manual-auth'] .item-guidance")?.textContent)
    .toContain("Download the PDF yourself - papio adopts it");
  expect(page.document.querySelector(".access-hint")).toBeNull();

  for (const guidance of Array.from(page.document.querySelectorAll<HTMLElement>(".item-guidance"))) {
    const copy = guidance.firstChild?.textContent ?? "";
    expect(copy.length).toBeLessThanOrEqual(60);
    expect(copy.trim().split(/\s+/).length).toBeLessThanOrEqual(8);
  }
});

test("manual download guidance does not compete with authentication metadata", async () => {
  const item = manualAction("action:manual-auth", 1, "Paywalled manual download");
  item.requires_auth = true;
  item.blocked_by = "paywall";
  item.facts = [{ label: "Detail", text: "the browser handoff did not produce a file; download the requested PDF yourself and papio will adopt it" }];
  const page = await inboxDocument((message) =>
    snapshotReply(snapshot([item], { counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }) }), message));

  const row = page.document.querySelector("[data-triage-item-id='action:manual-auth']");
  expect(row?.querySelector(".item-guidance")?.textContent).toContain(
    "Download the PDF yourself - papio adopts it",
  );
  expect(row?.querySelector(".item-guidance")?.textContent).not.toContain("sign in");
  expect(row?.querySelector<HTMLDListElement>(".item-debug")?.hidden).toBe(true);
  expect(row?.querySelector(".item-mechanism")?.textContent).toContain(
    "the browser handoff did not produce a file",
  );
});

test("keeps daemon detail and mechanism copy out of the always-visible row", async () => {
  const item = manualAction("action:dedup-guidance", 1, "Manual download with daemon detail");
  item.facts = [
    { label: "Action", text: "manual download" },
    { label: "Detail", text: "Download the requested PDF and papio will adopt it." },
  ];
  const page = await inboxDocument((message) =>
    snapshotReply(snapshot([item], { counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }) }), message));

  const row = page.document.querySelector("[data-triage-item-id='action:dedup-guidance']");
  expect(row?.querySelectorAll(".item-instruction")).toHaveLength(1);
  expect(row?.querySelector(".item-guidance")?.firstChild?.textContent).toBe(
    "Download the PDF yourself - papio adopts it",
  );
  expect(row?.querySelector(".item-detail")).toBeNull();
  const details = row?.querySelector<HTMLDListElement>(".item-debug");
  expect(details?.hidden).toBe(true);
  expect(details?.textContent).toContain("Download the requested PDF and papio will adopt it.");
  expect(details?.textContent).toContain("Send PDF in the papio toolbar popup");
  expect(details?.textContent).not.toContain("Downloads/papio");
});

test("an author suffix duplicated in the title is stripped for display", async () => {
  const item = manualAction("action:manual", 1, "Trust Engineering for Human-AI Teams - Neta Ezer, Sylvain Bruni");
  item.facts = [
    { label: "Action", text: "manual download" },
    { label: "Authors", text: "Neta Ezer, Sylvain Bruni, Yang Cai" },
  ];
  const fixture = snapshot([item], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.querySelector("[data-triage-item-id='action:manual'] h3")?.textContent).toBe(
    "Trust Engineering for Human-AI Teams",
  );
});

test("tabs default to Actions, separate watch hits, and support keyboard navigation", async () => {
  const fixture = snapshot([
    retraction("retraction:tab", 1, "Tab retraction"),
    manualAction("action:tab", 2, "Tab action"),
    watchHit("hit:tab", 3, "Tab watch hit"),
  ], { counts: counts({ pending_total: 3, actions: 1, watch_hits: 1, retractions: 1 }) });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.activity") {
      return { ok: true, feature: true, entries: [{ seq: 1, at: "2026-08-03T11:59:00Z", kind: "system", text: "Ready" }] };
    }
    return snapshotReply(fixture, message);
  });

  const actionsTab = page.document.getElementById("actions-tab") as HTMLButtonElement;
  const watchTab = page.document.getElementById("watch-tab") as HTMLButtonElement;
  const activityTab = page.document.getElementById("activity-tab") as HTMLButtonElement;
  expect(actionsTab.getAttribute("role")).toBe("tab");
  expect(actionsTab.getAttribute("aria-selected")).toBe("true");
  expect(page.document.getElementById("actions-panel")?.hidden).toBe(false);
  expect(page.document.getElementById("watch-panel")?.hidden).toBe(true);
  expect(page.document.getElementById("activity-panel")?.hidden).toBe(true);
  expect(actionsTab.textContent).toBe("Actions (2)");
  expect(watchTab.textContent).toBe("Watch hits (1)");
  expect(page.document.querySelectorAll("#item-list [data-triage-item-id]")).toHaveLength(2);
  expect(page.document.querySelectorAll("#watch-list [data-triage-item-id]")).toHaveLength(1);

  watchTab.click();
  expect(watchTab.getAttribute("aria-selected")).toBe("true");
  expect(page.document.getElementById("watch-panel")?.hidden).toBe(false);
  expect(page.document.getElementById("actions-panel")?.hidden).toBe(true);
  watchTab.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }));
  expect(activityTab.getAttribute("aria-selected")).toBe("true");
  expect(page.document.getElementById("activity-panel")?.hidden).toBe(false);
  activityTab.dispatchEvent(new KeyboardEvent("keydown", { key: "Home", bubbles: true }));
  expect(actionsTab.getAttribute("aria-selected")).toBe("true");
  expect(actionsTab.tabIndex).toBe(0);
});

test("activity groups by job, collapses duplicate rows, and only links Actions jobs", async () => {
  const fixture = snapshot([manualAction("action:activity", 1, "Activity download")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const entries = [
    { seq: 7, at: "2026-08-03T11:59:45Z", job_id: "job-18", kind: "browser.download_started", text: "same text", title: "Download activity" },
    { seq: 6, at: "2026-08-03T11:59:44Z", job_id: "job-18", kind: "job.transition", text: "same text", title: "Download activity" },
    { seq: 5, at: "2026-08-03T11:59:43Z", job_id: "job-18", kind: "job.transition", text: "next step", title: "Download activity" },
    { seq: 4, at: "2026-08-03T11:59:42Z", job_id: "job-other", kind: "job.transition", text: "other job", title: "Other activity" },
    { seq: 3, at: "2026-08-03T11:59:41Z", kind: "system", text: "system event" },
  ];
  const page = await inboxDocument((message) => {
    if (message.type === "papio.activity") return { ok: true, feature: true, entries };
    return snapshotReply(fixture, message);
  });

  page.document.getElementById("activity-tab")?.dispatchEvent(new Event("click", { bubbles: true }));
  const panel = page.document.getElementById("activity-panel");
  expect(panel?.hidden).toBe(false);
  expect(Array.from(panel?.querySelectorAll(".activity-group h3") ?? [], (heading) => heading.firstChild?.textContent))
    .toEqual(["Download activity", "Other activity", "System"]);
  expect(panel?.querySelectorAll(".activity-entry")).toHaveLength(4);
  expect(panel?.querySelector(".activity-count")?.textContent).toBe("×2");
  expect(panel?.textContent).not.toContain("Job job-");
  expect(panel?.querySelector(".activity-job-link")?.textContent).toBe("job-18");

  panel?.querySelector<HTMLButtonElement>(".activity-job-link")?.click();
  expect(page.document.getElementById("actions-panel")?.hidden).toBe(false);
  expect(page.document.querySelector("[data-triage-item-id='action:activity']")?.classList.contains("activity-highlight")).toBe(true);
});

test("activity initially shows at most five groups and fifteen rows, then reveals the batch", async () => {
  const fixture = snapshot([], { counts: counts({ pending_total: 0, actions: 0, watch_hits: 0, retractions: 0 }) });
  const entries = Array.from({ length: 18 }, (_, index) => ({
    seq: 18 - index,
    at: "2026-08-03T11:59:00Z",
    job_id: `job-${Math.floor(index / 3)}`,
    kind: "job.transition",
    text: `event ${index}`,
    title: `Job ${Math.floor(index / 3)}`,
  }));
  const page = await inboxDocument((message) => {
    if (message.type === "papio.activity") return { ok: true, feature: true, entries };
    return snapshotReply(fixture, message);
  });
  page.document.getElementById("activity-tab")?.dispatchEvent(new Event("click", { bubbles: true }));

  const panel = page.document.getElementById("activity-panel");
  const showMore = page.document.getElementById("activity-show-more") as HTMLButtonElement;
  expect(panel?.querySelectorAll(".activity-group")).toHaveLength(5);
  expect(panel?.querySelectorAll(".activity-entry")).toHaveLength(15);
  expect(showMore.hidden).toBe(false);
  showMore.click();
  expect(panel?.querySelectorAll(".activity-group")).toHaveLength(6);
  expect(panel?.querySelectorAll(".activity-entry")).toHaveLength(18);
  expect(showMore.hidden).toBe(true);
});

test("activity tab explains when the daemon does not advertise the feed", async () => {
  const fixture = snapshot([watchHit("hit:no-activity", 1, "No activity")]);
  const page = await inboxDocument((message) => {
    if (message.type === "papio.activity") return { ok: true, feature: false, entries: [] };
    return snapshotReply(fixture, message);
  });

  const activityTab = page.document.getElementById("activity-tab");
  expect(activityTab?.hidden).toBe(false);
  expect(page.document.getElementById("activity-panel")?.hidden).toBe(true);
  activityTab?.click();
  expect(page.document.getElementById("activity-panel")?.hidden).toBe(false);
  expect(page.document.getElementById("activity-panel")?.textContent).toContain("Activity is unavailable");
  expect(page.document.querySelectorAll(".activity-entry")).toHaveLength(0);
});
test("the Watch panel reports both a clear list and a filter miss", async () => {
  const emptyPage = await inboxDocument((message) =>
    snapshotReply(snapshot([manualAction("action:no-watch", 1, "Action only")], {
      counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    }), message));
  emptyPage.document.getElementById("watch-tab")?.click();
  expect(emptyPage.document.getElementById("watch-list")?.textContent).toContain("Your watch list is clear.");

  const filteredPage = await inboxDocument((message) =>
    snapshotReply(snapshot([watchHit("hit:filtered", 1, "Visible watch hit")], {
      counts: counts({ pending_total: 1, actions: 0, watch_hits: 1, retractions: 0 }),
    }), message));
  const filter = filteredPage.document.getElementById("item-filter") as HTMLInputElement;
  filter.value = "not-found";
  filter.dispatchEvent(new Event("input", { bubbles: true }));
  filteredPage.document.getElementById("watch-tab")?.click();
  expect(filteredPage.document.getElementById("watch-list")?.textContent).toContain("No watch hits match");
});
test("an unchanged counts poll preserves the focused inbox control", async () => {
  const originalSetTimeout = globalThis.setTimeout;
  const polls: Array<() => void> = [];
  globalThis.setTimeout = ((callback: () => void, delay?: number) => {
    if (delay === 15_000) {
      polls.push(callback);
      return 0;
    }
    return originalSetTimeout(callback, delay);
  }) as typeof globalThis.setTimeout;
  try {
    const fixture = snapshot([manualAction("action:poll-focus", 1, "Poll focus")], {
      counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    });
    const page = await inboxDocument((message) => snapshotReply(fixture, message));
    const dismiss = page.document.querySelector<HTMLButtonElement>(
      "[data-triage-item-id='action:poll-focus'] [data-operation='dismiss']",
    );
    expect(dismiss).not.toBeNull();
    dismiss?.focus();
    expect(polls).toHaveLength(1);
    polls.shift()?.();
    await settle();
    expect(page.document.activeElement?.getAttribute("data-operation")).toBe("dismiss");
    expect(page.document.activeElement?.closest("[data-triage-item-id]")?.getAttribute("data-triage-item-id"))
      .toBe("action:poll-focus");
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});

test("human action guidance names only the next step", async () => {
  const manual = manualAction("action:manual-guidance", 1, "Manual guidance");
  const authHandoff = handoffAction("action:auth-guidance", 2, true);
  authHandoff.facts.push({
    label: "Detail",
    text: "A fresh link is generated on each open so an expired resolver URL is never reused.",
  });
  const openHandoff = handoffAction("action:open-guidance", 3, false);
  const verify = verifyIdentity("action:verify-guidance", 4);
  const unknown = manualAction("action:unknown-guidance", 5, "Unknown guidance");
  unknown.action_kind = "future_action";
  const fixture = snapshot([manual, authHandoff, openHandoff, verify, unknown], {
    counts: counts({ pending_total: 5, actions: 5, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelector("[data-triage-item-id='action:manual-guidance'] .item-guidance")?.firstChild?.textContent)
    .toBe("Download the PDF yourself - papio adopts it");
  expect(page.document.querySelector("[data-triage-item-id='action:auth-guidance'] .item-guidance")?.firstChild?.textContent)
    .toBe("Sign in to your institution");
  expect(page.document.querySelector("[data-triage-item-id='action:open-guidance'] .item-guidance")?.firstChild?.textContent)
    .toBe("Open the page");
  expect(page.document.querySelector("[data-triage-item-id='action:verify-guidance'] .item-guidance")?.firstChild?.textContent)
    .toBe("Review the PDF, then accept or reject");
  expect(page.document.querySelector("[data-triage-item-id='action:unknown-guidance'] .item-instruction")).toBeNull();

  const authDetails = page.document.querySelector("[data-triage-item-id='action:auth-guidance'] .item-debug");
  expect(authDetails?.textContent).toContain("A fresh link is generated on each open");
  expect(authDetails?.textContent).toContain("papio continues automatically after the handoff");
});

test("retry operations declared by the daemon are not rendered", async () => {
  const item = watchHit("hit:retry-placeholder", 1, "Retry placeholder");
  item.ops = ["retry"];
  const fixture = snapshot([item]);
  const page = await inboxDocument((message) => snapshotReply(fixture, message));

  expect(page.document.querySelector("[data-triage-item-id='hit:retry-placeholder'] [data-operation='retry']")).toBeNull();
  expect(page.document.querySelector("[data-triage-item-id='hit:retry-placeholder'] .item-controls")).toBeNull();
});

test("challenge-blocked rows show one concise action and hide the mechanism", async () => {
  const fixture = snapshot([manualAction("action:challenge", 1, "Challenge paper")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const entries = [
    {
      seq: 3,
      at: "2026-08-03T12:00:00Z",
      job_id: "job-18",
      kind: "browser.error",
      text: "challenge_blocked: Provider security check needs human attention",
    },
  ];
  const page = await inboxDocument((message) => {
    if (message.type === "papio.activity") return { ok: true, feature: true, entries };
    return snapshotReply(fixture, message);
  });

  const row = page.document.querySelector("[data-triage-item-id='action:challenge']");
  const annotation = row?.querySelector(".challenge-annotation");
  expect(annotation?.firstChild?.textContent).toBe("Solve the security check in its tab");
  expect(row?.querySelectorAll(".item-guidance")).toHaveLength(1);
  const details = row?.querySelector<HTMLDListElement>(".item-debug");
  expect(details?.hidden).toBe(true);
  expect(details?.textContent).toContain(
    "papio resumes automatically after you solve the security check.",
  );
});

test("tab labels carry daemon totals, not the loaded page size", async () => {
  const fixture = snapshot([manualAction("action:paged", 1, "First of many")], {
    counts: counts({ pending_total: 119, actions: 109, watch_hits: 5, retractions: 5 }),
    has_more: true,
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.getElementById("actions-tab")?.textContent).toBe("Actions (114)");
  expect(page.document.getElementById("watch-tab")?.textContent).toBe("Watch hits (5)");
});

test("an empty watch panel renders without the snapshot's Load more control", async () => {
  const fixture = snapshot([manualAction("action:only", 1, "Only actions here")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    has_more: true,
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  expect(page.document.getElementById("load-more")?.hidden).toBe(false);
  (page.document.getElementById("watch-tab") as HTMLButtonElement).click();
  expect(page.document.getElementById("watch-panel")?.hidden).toBe(false);
  expect(page.document.getElementById("load-more")?.hidden).toBe(true);
});

test("watch hits load themselves when the count says they exist", async () => {
  const first = snapshot([manualAction("action:first", 1, "First page action")], {
    counts: counts({ pending_total: 2, actions: 1, watch_hits: 1, retractions: 0 }),
    has_more: true,
    cursor: "page-2",
  });
  const second = snapshot([watchHit("watch:auto", 2, "Automatically loaded hit")], {
    counts: counts({ pending_total: 2, actions: 1, watch_hits: 1, retractions: 0 }),
    has_more: false,
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.triage.snapshot") {
      return snapshotReply(message.request["cursor"] === "page-2" ? second : first, message);
    }
    return snapshotReply(first, message);
  });
  await settle();
  await settle();
  // Nobody clicked Load more: the second page arrived on its own and the
  // watch row renders.
  expect(page.document.querySelector("[data-triage-item-id='watch:auto']")).not.toBeNull();
  expect(page.document.getElementById("watch-panel")?.textContent).not.toContain("Loading 1 watch hit");
});

test("renders a document_delivery item's delivery detail, attention styling, and the three reconciliation ops", async () => {
  const unresolved = documentDeliveryAction("action:delivery-required", 1, "required", "unknown_outcome");
  const fulfilled = documentDeliveryAction("action:delivery-working", 2, "working", "fulfilled");
  fulfilled.job_id = "job-delivery-22";
  fulfilled.action_id = 22;
  const fixture = snapshot([unresolved, fulfilled], {
    schema: 3,
    counts: counts({ pending_total: 2, actions: 2, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.delivery.reconcile") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  const win = page.window as WindowWithPrompt;
  win.prompt = () => "TN-42";

  const requiredRow = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:delivery-required']");
  expect(requiredRow?.dataset.attention).toBe("required");
  const workingRow = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:delivery-working']");
  expect(workingRow?.dataset.attention).toBe("working");
  expect(requiredRow?.querySelector(".item-status")?.getAttribute("data-status")).toBe("document_delivery");

  const delivery = requiredRow?.querySelector(".item-delivery");
  expect(delivery?.textContent).toContain("illiad");
  expect(delivery?.textContent).toContain("TN-42");
  expect(delivery?.textContent).toContain("unknown outcome");

  const historyButton = requiredRow?.querySelector<HTMLButtonElement>("[data-operation='open_request_history']");
  const existsButton = requiredRow?.querySelector<HTMLButtonElement>("[data-operation='confirm_request_exists']");
  const requiredAbsentButton = requiredRow?.querySelector<HTMLButtonElement>("[data-operation='confirm_request_absent']");
  expect(historyButton?.textContent).toBe("History");
  expect(existsButton?.textContent).toBe("Confirm exists");
  expect(requiredAbsentButton?.textContent).toBe("Confirm absent");

  const debugToggle = requiredRow?.querySelector<HTMLButtonElement>(".item-debug-toggle");
  expect(debugToggle?.getAttribute("aria-expanded")).toBe("false");
  historyButton?.click();
  expect(debugToggle?.getAttribute("aria-expanded")).toBe("true");

  existsButton?.click();
  await settle();
  expect(page.requests.find((request) => request.type === "papio.delivery.reconcile")?.request).toEqual({
    job_id: "job-delivery-21",
    operation: "confirm_request_exists",
    provider_reference: "TN-42",
  });
  expect(page.document.querySelector("[data-triage-item-id='action:delivery-required']")).toBeNull();

  expect(workingRow?.querySelector<HTMLButtonElement>("[data-operation='confirm_request_absent']")).toBeNull();
});

test("renders an offered delivery as a request created but not submitted", async () => {
  const item = documentDeliveryAction("action:delivery-offered", 1, "required", "offered");
  const fixture = snapshot([item], {
    schema: 3,
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const delivery = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:delivery-offered'] .item-delivery");
  const status = Array.from(delivery?.querySelectorAll("dt") ?? [])
    .find((label) => label.textContent === "Status")
    ?.nextElementSibling?.textContent;
  expect(status).toBe("request created but not submitted");
});

test("hides waiting-on-you pulse while counts own the turn and deduplicates connection bands", async () => {
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
    counts: counts({ pending_total: 1, turns_required: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const pulse = {
    ok: true,
    available: true,
    worker_epoch: "worker-live",
    received_at: Date.now(),
    pulse: {
      request_id: "pulse-wait",
      schema: 1,
      generated_at: new Date().toISOString(),
      projection_complete: true,
      nonterminal_total: 1,
      in_flight: 0,
      continuing: 0,
      scheduled: 0,
      waiting_required: 1,
      stalled: 0,
    },
  };
  const page = await inboxDocument((message) => {
    if (message.type === "papio.work.pulse") return pulse;
    return snapshotReply(fixture, message);
  });
  expect(page.document.getElementById("inbox-counts")?.textContent).toBe("1 needs you");
  expect(page.document.getElementById("inbox-pulse")?.hidden).toBe(true);
  expect(page.document.getElementById("connection-status")?.hidden).toBe(true);
});

test("styles status meaning with data-tone independent of attention", async () => {
  const rejected = manualAction("action:rejected", 1, "Rejected file");
  rejected.guidance_variant = "manual_download_rejected_file";
  rejected.attention = "required";
  const working = manualAction("action:working", 2, "Working download");
  working.attention = "working";
  const fixture = snapshot([rejected, working], {
    schema: 3,
    counts: counts({ pending_total: 2, actions: 2, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  const rejectedBadge = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:rejected'] .item-status");
  const workingBadge = page.document.querySelector<HTMLElement>("[data-triage-item-id='action:working'] .item-status");
  expect(rejectedBadge?.dataset.tone).toBe("degraded");
  expect(workingBadge?.dataset.tone).toBe("continuing");
  expect(rejectedBadge?.dataset.attention).toBeUndefined();
});

test("uses contextual Open labels for manual, auth handoff, and watch hits", async () => {
  const manual = manualAction("action:manual", 1, "Manual");
  manual.ops = ["open", "dismiss"];
  manual.links = [{ rel: "landing", url: "https://example.test/manual" }];
  const authHandoff = handoffAction("action:auth", 2, true);
  const plainHandoff = handoffAction("action:plain", 3, false);
  const hit = watchHit("hit:one", 4, "Hit");
  hit.ops = ["acquire", "open", "dismiss"];
  const fixture = snapshot([manual, authHandoff, plainHandoff, hit], {
    counts: counts({ pending_total: 4, actions: 3, watch_hits: 1, retractions: 0 }),
  });
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  // "Open source" is barred: it reads as open-source/open-access rather than
  // "open this paper's link". Each label must also stay distinct per kind.
  const label = (id: string): string | undefined =>
    page.document.querySelector(`[data-triage-item-id='${id}'] [data-operation='open']`)?.textContent ?? undefined;
  expect(label("action:manual")).toBe("Open link");
  expect(label("action:auth")).toBe("Sign in");
  expect(label("action:plain")).toBe("Open page");
  expect(label("hit:one")).toBe("Open result");
  expect(page.document.body.textContent).not.toMatch(/open source/i);
});

test("exposes row-focus status tooltips and a 32px debug control", async () => {
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")]);
  const page = await inboxDocument((message) => snapshotReply(fixture, message));
  // The static legend is gone, so every one of a glyph's meanings must be
  // reachable from the row itself.
  expect(page.document.querySelector("#status-legend")).toBeNull();
  const toggle = page.document.querySelector(".item-debug-toggle") as unknown as HTMLElement | null;
  expect(toggle).not.toBeNull();
  const styles = page.window.getComputedStyle(toggle as never);
  expect(Number.parseFloat(styles.minHeight)).toBe(32);
  expect(Number.parseFloat(styles.minWidth)).toBe(32);
  const status = page.document.querySelector("[data-triage-item-id='action:manual'] .item-status");
  // Non-colour carriers: the glyph has an accessible name, and its visible
  // label rides data-label for the hover/focus tooltip rather than colour.
  expect(status?.getAttribute("role")).toBe("img");
  expect((status?.getAttribute("aria-label") ?? "").length).toBeGreaterThan(0);
  expect((status?.getAttribute("data-label") ?? "").length).toBeGreaterThan(0);
  // Roving keyboard focus reveals the same label the pointer does, without
  // adding a second tab stop per row. The rule is CSS-only, so the stylesheet
  // declaring it beside the hover selector is the observable contract here.
  const css = Array.from(page.document.querySelectorAll("style"))
    .map((node) => node.textContent ?? "")
    .join("\n");
  expect(css).toContain(".triage-item:focus-visible > .item-status::after");
});

test("announces persistent row errors once across rerenders", async () => {
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")]);
  let fail = true;
  const page = await inboxDocument((message) => {
    if (message.type === "papio.action.resolve" && fail) {
      return { ok: false, error: { code: "invalid_request", message: "Still invalid" } };
    }
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  flush(page.window);
  await settle();
  const announcer = page.document.getElementById("operation-status");
  expect(announcer?.textContent).toBe("Still invalid");
  expect(page.document.querySelector(".item-result")?.getAttribute("role")).toBeNull();
  page.document.getElementById("refresh-inbox")?.dispatchEvent(new Event("click", { bubbles: true }));
  await settle();
  expect(announcer?.textContent).toBe("Still invalid");
});

test("feedback strip kinds, undo priority, and success lifecycle", async () => {
  const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
    counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
  });
  const page = await inboxDocument((message) => {
    if (message.type === "papio.settings.successAck") return { ok: true, mode: "all" };
    if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
    return snapshotReply(fixture, message);
  });
  page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
  await settle();
  const bar = page.document.getElementById("undo-bar");
  expect(bar?.dataset.kind).toBe("undo");
  expect(page.document.getElementById("undo-dismiss")?.textContent).toBe("Undo");
  expect(page.document.getElementById("undo-dismiss")?.textContent).not.toMatch(/\(\d+\)/);
  flush(page.window);
  await settle();
  expect(bar?.dataset.kind).toBe("success");
  expect(page.document.getElementById("undo-dismiss")?.hidden).toBe(true);
});

test("aggregates more than three queued success notices", async () => {
  vi.useFakeTimers();
  try {
    const fixture = snapshot([
      watchHit("hit:1", 1, "One"),
      watchHit("hit:2", 2, "Two"),
      watchHit("hit:3", 3, "Three"),
      watchHit("hit:4", 4, "Four"),
      watchHit("hit:5", 5, "Five"),
    ], { counts: counts({ pending_total: 5, watch_hits: 5, actions: 0, retractions: 0 }) });
    const page = await inboxDocument((message) => {
      if (message.type === "papio.settings.successAck") return { ok: true, mode: "all" };
      if (message.type === "papio.triage.decide") return { ok: true, outcome: "applied" };
      return snapshotReply(fixture, message);
    });
    for (const id of ["hit:1", "hit:2", "hit:3", "hit:4", "hit:5"]) {
      page.document.querySelector<HTMLButtonElement>(`[data-triage-item-id='${id}'] [data-operation='acquire']`)?.click();
      for (let i = 0; i < 4; i += 1) await settle();
    }
    const bar = page.document.getElementById("undo-bar");
    vi.advanceTimersByTime(20_000);
    for (let i = 0; i < 12; i += 1) await settle();
    expect(page.document.getElementById("undo-message")?.textContent).toContain("4 actions completed.");
  } finally {
    vi.useRealTimers();
  }
});

test("undo deadline transfer blocks keyboard undo at the boundary", async () => {
  vi.useFakeTimers();
  try {
    const fixture = snapshot([manualAction("action:manual", 1, "Manual action")], {
      counts: counts({ pending_total: 1, actions: 1, watch_hits: 0, retractions: 0 }),
    });
    const page = await inboxDocument((message) => {
      if (message.type === "papio.action.resolve") return { ok: true, outcome: "applied" };
      return snapshotReply(fixture, message);
    });
    page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
    await settle();
    vi.advanceTimersByTime(5999);
    key(page.document, "u");
    await settle();
    expect(page.document.querySelector("[data-triage-item-id='action:manual']")).not.toBeNull();
    expect(page.requests.filter((request) => request.type === "papio.action.resolve")).toHaveLength(0);

    page.document.querySelector<HTMLButtonElement>("[data-operation='dismiss']")?.click();
    await settle();
    vi.advanceTimersByTime(6000);
    await settle();
    key(page.document, "u");
    await settle();
    expect(page.document.querySelector("[data-triage-item-id='action:manual']")).toBeNull();
    expect(page.requests.filter((request) => request.type === "papio.action.resolve")).toHaveLength(1);
  } finally {
    vi.useRealTimers();
  }
});

