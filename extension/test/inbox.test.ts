// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";
import type { TriageCounts, TriageSnapshotItem } from "../src/protocol";

interface FixtureSnapshot {
  schema: 1;
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

let importSerial = 0;

async function settle(): Promise<void> {
  for (let iteration = 0; iteration < 12; iteration += 1) await Promise.resolve();
}

async function inboxDocument(
  reply: (message: RuntimeRequest) => unknown | Promise<unknown>,
): Promise<{ document: Document; requests: RuntimeRequest[]; opened: string[] }> {
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
    HTMLTimeElement: window.HTMLTimeElement,
    chrome: {
      runtime: {
        sendMessage: async (message: RuntimeRequest) => {
          requests.push(message);
          return reply(message);
        },
      },
    },
  });
  importSerial += 1;
  // Each fixture needs a fresh page module because its UI state is intentionally module-local.
  await import(`../src/inbox.ts?inbox-test=${importSerial}`);
  await settle();
  return { document: window.document as unknown as Document, requests, opened };
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
    facts: [],
    links: [],
    ops: ["reject"],
    action_id: 18,
    job_id: "job-18",
    action_kind: "manual_download",
    job_state: "needs_review",
    revision: 1,
    sha256: "",
    size_bytes: 0,
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
  return { ok: false, error: { code: "unexpected", message: "Unexpected message" } };
}

function key(document: Document, value: string): void {
  document.dispatchEvent(new KeyboardEvent("keydown", { key: value, bubbles: true }));
}

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
    "Human actions (2)",
    "Watch hits (1)",
  ]);
  expect(Array.from(page.document.querySelectorAll(".triage-item h3"), (heading) => heading.textContent)).toEqual([
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
      return { ok: true, preview: { url: previewURL, sha256, size_bytes: 99, expires_at: "2026-07-21T10:10:00Z" } };
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
  expect(page.document.querySelector(".item-result")?.textContent).toBe("changed elsewhere — refreshed");
  expect(page.requests.filter((request) => request.type === "papio.triage.snapshot")).toHaveLength(2);
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
