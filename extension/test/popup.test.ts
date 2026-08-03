// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import {
  acquireCurrentPage,
  collectPageMetadata,
  OPEN_HANDOFF_MESSAGE,
  OPEN_INBOX_MESSAGE,
  openInbox,
  openInstitutionSignIn,
  deriveSessionCardState,
  deriveSessionRows,
  readCurrentPageMetadata,
  refreshImpactSummary,
  renderInstitutionSession,
  renderNeedsAttention,
  renderDaemonStatus,
  renderImpactSummary,
  renderPageAcquire,
  renderPageContext,
  renderResolverGrants,
  renderTermsConsent,
  wireCapture,
  wireDevTools,
  wireHistoryLauncher,
  wireInboxLauncher,
  wirePrimaryShortcut,
  wireSettings,
  renderLeftoverTabs,
} from "../src/popup";
import type { ActiveJob } from "../src/state";
import { PROVIDERS, SCENARIOS } from "../src/capture";

function popupDocument(): Document {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/popup.html", import.meta.url), "utf8"));
  Object.assign(globalThis, {
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLSelectElement: window.HTMLSelectElement,
  });
  return window.document as unknown as Document;
}

function job(overrides: Partial<ActiveJob> = {}): ActiveJob {
  return {
    job_id: "job-1",
    tab_id: 17,
    offered_at: 1,
    expires_at: 2,
    status: "accepted",
    provider_hosts: ["www.jstor.org"],
    ...overrides,
  };
}

test("places the acquire icon before inbox and keeps idle feedback hidden", () => {
  const doc = popupDocument();
  const launcher = doc.querySelector(".launcher");
  const headerActions = doc.querySelector(".header-actions");

  expect(doc.querySelector("h1")).toBeNull();
  expect(launcher?.querySelectorAll(".launcher-action")).toHaveLength(0);
  expect(launcher?.querySelector("h2")).toBeNull();
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);
  expect(doc.getElementById("page-acquire-doi")).toBeNull();
  expect(doc.getElementById("page-acquire-context")).toBeNull();
  expect(headerActions?.children[0]?.id).toBe("page-acquire-btn");
  expect(headerActions?.children[1]?.id).toBe("open-inbox-btn");
  expect(doc.getElementById("page-acquire-btn")?.closest("header")).not.toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.querySelector("svg")).not.toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.hidden).toBe(true);
  expect(doc.getElementById("daemon-footer")).toBeNull();
  expect(doc.getElementById("open-inbox-btn")?.getAttribute("aria-label")).toBe("Open inbox");
  expect(doc.getElementById("needs-you-section")).not.toBeNull();
  expect(doc.getElementById("needs-you-section")?.hidden).toBe(true);
  expect(doc.getElementById("terms-consent")).not.toBeNull();
  expect(doc.getElementById("resolver-grant")).not.toBeNull();
});

test("capture selects offer every registered provider and scenario", () => {
  const doc = popupDocument();
  wireCapture(doc);
  const values = (id: string): string[] =>
    Array.from(doc.querySelectorAll<HTMLOptionElement>(`#${id} option`)).map((o) => o.value);
  expect(values("capture-provider")).toEqual([...PROVIDERS]);
  expect(values("capture-scenario")).toEqual([...SCENARIOS]);
});

test("capture tools require a developer build and an unpacked manifest", () => {
  const flag = "__PAPIO_DEV_CAPTURE__";
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, flag);
  try {
    Object.defineProperty(globalThis, flag, { configurable: true, value: false });
    const releaseBuild = popupDocument();
    wireDevTools(releaseBuild, {});
    expect(releaseBuild.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
    expect(releaseBuild.querySelectorAll("#capture-provider option")).toHaveLength(0);

    Object.defineProperty(globalThis, flag, { configurable: true, value: true });
    const packed = popupDocument();
    wireDevTools(packed, { update_url: "https://clients2.google.com/service/update2/crx" });
    expect(packed.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
    expect(packed.querySelectorAll("#capture-provider option")).toHaveLength(0);

    const unpacked = popupDocument();
    wireDevTools(unpacked, {});
    expect(unpacked.querySelector<HTMLElement>(".capture")?.hidden).toBe(false);
    expect(unpacked.querySelectorAll("#capture-provider option")).toHaveLength(PROVIDERS.length);
    const capture = unpacked.querySelector<HTMLElement>(".capture");
    expect(capture?.tagName).toBe("DETAILS");
    expect(capture?.hasAttribute("open")).toBe(false);
  } finally {
    if (descriptor !== undefined) {
      Object.defineProperty(globalThis, flag, descriptor);
    } else {
      Reflect.deleteProperty(globalThis, flag);
    }
  }
});

test("renders actionable daemon problems without routine version diagnostics", () => {
  const doc = popupDocument();
  renderDaemonStatus(doc, { connectionStatus: "connected", daemonVersion: "0.1.0" });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(true);

  Object.assign(globalThis, { __PAPIO_DAEMON_VERSION__: "0.2.0" });
  renderDaemonStatus(doc, {
    connectionStatus: "connected",
    daemonVersion: "0.1.0",
    daemonUpdateHint: true,
  });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(false);
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe(
    "papio 0.2.0 is available — daemon is v0.1.0",
  );

  renderDaemonStatus(doc, {
    connectionStatus: "connected",
    daemonVersion: "0.1.0-dev.abc123",
    daemonUpdateHint: true,
  });
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe(
    "papio 0.2.0 is available — your daemon is a development build (v0.1.0-dev.abc123)",
  );
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe(
    "Update the source checkout, then run: make dev-deploy",
  );
  delete (globalThis as Record<string, unknown>).__PAPIO_DAEMON_VERSION__;

  renderDaemonStatus(doc, { connectionStatus: "disconnected" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain("papio daemon isn't reachable");
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe("run: papio daemon status");
  expect(doc.getElementById("daemon-footer")).toBeNull();
});

test("shows the DOI acquire icon with its tooltip even without a negotiated daemon", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    throw new Error("papio daemon isn't reachable");
  });
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(section?.hidden).toBe(true);
  expect(button.disabled).toBe(false);
  expect(button.hidden).toBe(false);
  expect(button.title).toBe("Acquire this page · 10.1000/example");
  expect(button.getAttribute("aria-label")).toBe("Acquire this page · 10.1000/example");
  expect(button.getAttribute("aria-disabled")).toBe("false");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
  expect(button.disabled).toBe(false);
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("papio daemon isn't reachable");
});

test("keeps a successfully queued acquisition disabled", async () => {
  const doc = popupDocument();
  renderPageAcquire(doc, async () => ({ job_id: "job_page_acquire_001" }));
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();

  expect(button.disabled).toBe(true);
  expect(button.title).toBe("Queued");
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Queued: job_page_acquire_001");
});

test("hides the header acquire action when the current page has no paper", () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  });
  renderPageContext(doc, undefined, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  expect(button.hidden).toBe(true);
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);
  button.click();
  expect(calls).toBe(0);
});

test("shows the PDF acquire icon with the PDF tooltip", () => {
  const doc = popupDocument();
  renderPageAcquire(doc, async () => ({ error: "unused" }), async () => ({ state: "sending", job_id: "job_1234567890abcdef" }));
  renderPageContext(
    doc,
    { url: "https://papers.example.edu/download/paper.pdf?download=1", kind: "pdf", tab_id: 17 },
    [job({ job_id: "job_1234567890abcdef", tab_id: 17 })],
  );
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.hidden).toBe(false);
  expect(button.title).toBe("Send this PDF to papio");
  expect(button.getAttribute("aria-label")).toBe("Send this PDF to papio");
  expect(button.disabled).toBe(false);
  button.click();
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

test("renders a live, honest status card for a local in-flight acquisition", () => {
  const doc = popupDocument();
  const now = Date.now();
  let openedInbox = 0;
  let openedTab = 0;
  renderPageContext(
    doc,
    { url: "https://doi.org/10.1000/example", doi: "10.1000/example" },
    [job({ expected: { title: "A paper in progress", doi: "doi:10.1000/example" }, status: "auth_pending" })],
    undefined,
    [{
      seq: 2,
      at: new Date(now - 11 * 60_000).toISOString(),
      job_id: "job-1",
      kind: "browser.handoff_offered",
      text: "Institution access handoff offered",
      title: "A paper in progress",
    }],
    {
      openInbox: async () => { openedInbox += 1; },
      goToTab: async () => { openedTab += 1; },
    },
  );

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.hidden).toBe(false);
  expect(button.disabled).toBe(true);
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire")?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-live")?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-live-title")?.textContent).toBe("A paper in progress");
  expect(doc.getElementById("page-acquire-live-status")?.textContent).toContain("No progress for 11m");
  expect(doc.getElementById("page-acquire-live-status")?.textContent).toContain("Institution access handoff offered");
  const inbox = doc.getElementById("page-acquire-open-inbox") as HTMLButtonElement;
  const tab = doc.getElementById("page-acquire-go-tab") as HTMLButtonElement;
  expect(tab.hidden).toBe(false);
  inbox.click();
  tab.click();
  expect(openedInbox).toBe(1);
  expect(openedTab).toBe(1);
});

test("merges auth-pending paper rows into the institution session card", async () => {
  const doc = popupDocument();
  const requests: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (message: unknown) => {
          requests.push(message);
          return { ok: true, opened: true };
        },
      },
    },
  });

  renderNeedsAttention(doc, [
    job({
      status: "auth_pending",
      expected: { title: "A paper awaiting institutional access", doi: "10.1000/example" },
    }),
  ]);

  const session = doc.getElementById("institution-session");
  const waiting = doc.getElementById("institution-session-waiting");
  expect(session?.hidden).toBe(false);
  expect(waiting?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-waiting-heading")?.textContent).toBe(
    "Waiting on your sign-in",
  );
  expect(waiting?.querySelector(".institution-session-waiting-title")?.textContent).toBe(
    "A paper awaiting institutional access",
  );
  expect(doc.getElementById("needs-you-section")?.hidden).toBe(true);
  const button = waiting?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Focus");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(requests).toEqual([{ type: OPEN_HANDOFF_MESSAGE, request: { job_id: "job-1" } }]);
  expect(button.textContent).toBe("Focus");
  renderNeedsAttention(doc, []);
  expect(waiting?.hidden).toBe(true);
  expect(session?.hidden).toBe(true);
});

test("uses a DOI then job id when an awaiting sign-in has no paper title", () => {
  const doc = popupDocument();
  renderNeedsAttention(
    doc,
    [
      job({ job_id: "job-with-doi", status: "auth_pending", expected: { doi: "10.1000/fallback" } }),
      job({ job_id: "job-without-identity", status: "auth_pending" }),
    ],
    [],
    async () => {},
  );

  const labels = Array.from(doc.querySelectorAll(".institution-session-waiting-title")).map(
    (paper) => paper.textContent,
  );
  expect(labels).toEqual(["10.1000/fallback", "job-without-identity"]);
});
test("surfaces a blocked security check with a go-to-tab action", async () => {
  const doc = popupDocument();
  const focused: string[] = [];
  renderNeedsAttention(
    doc,
    [job({ job_id: "job-challenge", status: "auth_pending", challenge_blocked: true, challenge_host: "ScienceDirect.com" })],
    [],
    async (jobID) => {
      focused.push(jobID);
    },
  );

  const section = doc.getElementById("needs-you-section");
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-waiting")?.hidden).toBe(true);
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Security check needs you");
  expect(section?.querySelector(".needs-you-paper")?.textContent).toBe(
    "Security check needs you - sciencedirect.com",
  );
  const button = section?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Go-to-tab");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(focused).toEqual(["job-challenge"]);
});


test("surfaces each blocked provider host once and routes the remedy to Options", async () => {
  const doc = popupDocument();
  let openedOptions = 0;
  renderNeedsAttention(
    doc,
    [],
    ["journals.sagepub.com", "JOURNALS.SAGEPUB.COM", "www.sciencedirect.com"],
    async () => {},
    async () => {
      openedOptions += 1;
    },
  );

  const section = doc.getElementById("needs-you-section");
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Allow provider access");
  expect(doc.getElementById("needs-you-message")?.textContent).toContain("Grant all sources");
  expect(Array.from(section?.querySelectorAll(".needs-you-paper") ?? []).map((item) => item.textContent)).toEqual([
    "journals.sagepub.com",
    "www.sciencedirect.com",
  ]);
  const optionsButton = Array.from(section?.querySelectorAll("button") ?? []).find(
    (button) => button.textContent === "Open Options",
  ) as HTMLButtonElement;
  optionsButton.click();
  await Promise.resolve();
  expect(openedOptions).toBe(1);
});

test("opens the singleton inbox through the broker when it acknowledges", async () => {
  const requests: unknown[] = [];
  const created: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: { sendMessage: async (message: unknown) => { requests.push(message); return { opened: true }; } },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
  });

  await openInbox();
  expect(requests).toEqual([{ type: OPEN_INBOX_MESSAGE }]);
  expect(created).toEqual([]);
});

test("falls back to a direct inbox tab when the broker does not answer", async () => {
  const doc = popupDocument();
  const created: unknown[] = [];
  let closed = 0;
  const { promise: dismissed, resolve: onClose } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      runtime: { sendMessage: async () => undefined },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
    window: { close: () => { closed += 1; onClose(); } },
  });
  wireInboxLauncher(doc);

  (doc.getElementById("open-inbox-btn") as HTMLButtonElement).click();
  await dismissed;
  expect(created).toEqual([{ url: "dist/inbox.html" }]);
  // The popup dismisses itself once the inbox is open (Firefox keeps it open otherwise).
  expect(closed).toBe(1);
});

test("renderImpactSummary fills the impact card with real values", () => {
  const doc = popupDocument();
  renderImpactSummary(doc, { acquired_total: 42, failed_total: 14 });

  expect(doc.getElementById("impact-summary")?.hidden).toBe(false);
  // 42 acquired x 5 min ~= 3.5 h; 42 of 56 finished jobs succeeded.
  expect(doc.getElementById("impact-acquired")?.textContent).toBe("42");
  expect(doc.getElementById("impact-time-saved")?.textContent).toBe("3.5 h");
  expect(doc.getElementById("impact-success-rate")?.textContent).toBe("75%");
});

test("keeps the impact title and history link in one header row", () => {
  const doc = popupDocument();
  const header = doc.getElementById("impact-header");
  expect(header?.classList.contains("impact-header")).toBe(true);
  expect(header?.querySelector("h2")?.textContent).toBe("Your papio impact");
  expect(doc.getElementById("view-history-btn")?.parentElement).toBe(header);
  expect(doc.getElementById("impact-summary")?.querySelector(":scope > #view-history-btn")).toBeNull();
});

test("renderImpactSummary hides the impact card when stats are unavailable", () => {
  const doc = popupDocument();
  // Force it visible first so hiding it is a real assertion, not a no-op
  // against popup.html's default hidden state.
  (doc.getElementById("impact-summary") as HTMLElement).hidden = false;

  renderImpactSummary(doc, null);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(true);
});

test("refreshImpactSummary populates the impact card from a daemon stats reply", async () => {
  const doc = popupDocument();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({
          ok: true,
          stats: {
            generated_at: "2026-07-25T08:00:00Z",
            acquired_total: 42,
            failed_total: 14,
            handoffs_required: 9,
            access: { open_access: 18, institutional: 20, licensed_api: 3, other: 1 },
            series: [],
          },
        }),
      },
    },
  });

  await refreshImpactSummary(doc);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(false);
  expect(doc.getElementById("impact-acquired")?.textContent).toBe("42");
  expect(doc.getElementById("impact-success-rate")?.textContent).toBe("75%");
});

test("refreshImpactSummary hides the impact card when the daemon cannot serve stats", async () => {
  const doc = popupDocument();
  (doc.getElementById("impact-summary") as HTMLElement).hidden = false;
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({ ok: false, error: { code: "timeout", message: "no reply" } }),
      },
    },
  });

  await refreshImpactSummary(doc);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(true);
});

test("history launcher opens the manifest-derived history page and closes the popup", async () => {
  const doc = popupDocument();
  const created: unknown[] = [];
  let closed = 0;
  const { promise: dismissed, resolve: onClose } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        // A relocated popup page (not the dist/popup.html default) proves the
        // history URL is derived from the manifest and not a hardcoded
        // sibling literal: pre-fix code always opened "dist/history.html"
        // regardless of where the manifest actually declares the popup.
        getManifest: () => ({ action: { default_popup: "dist/ui/popup.html" } }),
        getURL: (path: string) => path,
      },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
    window: { close: () => { closed += 1; onClose(); } },
  });
  wireHistoryLauncher(doc);

  (doc.getElementById("view-history-btn") as HTMLButtonElement).click();
  await dismissed;
  expect(created).toEqual([{ url: "dist/ui/history.html" }]);
  expect(closed).toBe(1);
});

test("Enter invokes the primary acquisition action", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  });
  wirePrimaryShortcut(doc);
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  doc.dispatchEvent(new doc.defaultView!.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
});

test("keeps the informed-consent guidance available", () => {
  const doc = popupDocument();
  const choices: string[] = [];
  renderTermsConsent(doc, [job({ needs_terms_consent: true })], undefined, (choice) => choices.push(choice));

  expect(doc.getElementById("terms-consent")?.hidden).toBe(false);
  (doc.getElementById("terms-consent-enable") as HTMLButtonElement).click();
  expect(choices).toEqual(["accept"]);
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

// --- collectPageMetadata DOI fallback chain -------------------------------
// SAGE (Atypon) abstract pages carry no citation_doi; the scraper must fall
// back through publication_doi, dc.Identifier[scheme=doi], and the URL path.

function pageDocument(html: string, href: string): void {
  const window = new Window({ url: href });
  window.document.write(html);
  Object.assign(globalThis, { document: window.document, location: new URL(href) });
}

test("collectPageMetadata prefers citation_doi when present", () => {
  pageDocument(
    `<html><head><meta name="citation_doi" content=" 10.1002/prefer "><meta name="publication_doi" content="10.9999/wrong"><meta name="citation_title" content="Preferred"></head></html>`,
    "https://onlinelibrary.wiley.com/doi/10.1002/prefer",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBe("10.1002/prefer");
  expect(page.title).toBe("Preferred");
});

test("collectPageMetadata reads SAGE publication_doi and dc.Identifier", () => {
  pageDocument(
    `<html><head><meta name="dc.Identifier" scheme="publisher-id" content="10.1177_1071181319631264"><meta name="dc.Identifier" scheme="doi" content="10.1177/1071181319631264"><title>Trust Engineering</title></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264",
  );
  expect(collectPageMetadata().doi).toBe("10.1177/1071181319631264");

  pageDocument(
    `<html><head><meta name="publication_doi" content="10.1177/1071181319631264"></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264",
  );
  expect(collectPageMetadata().doi).toBe("10.1177/1071181319631264");
});

test("collectPageMetadata falls back to a DOI-shaped URL path", () => {
  pageDocument(
    `<html><head><title>Bare page</title></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264?journalCode=pro",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBe("10.1177/1071181319631264");
});

test("collectPageMetadata reports no DOI on DOI-less pages", () => {
  pageDocument(
    `<html><head><title>News article</title></head></html>`,
    "https://example.com/news/story-42",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBeUndefined();
  expect(page.title).toBe("News article");
});

test("collectPageMetadata classifies a JSTOR stable landing as its documented DOI", () => {
  const fixture = readFileSync(new URL("../fixtures/jstor/success.html", import.meta.url), "utf8")
    .replaceAll("2095101", "20183234");
  pageDocument(fixture, "https://www.jstor.org/stable/20183234");
  expect(collectPageMetadata().doi).toBe("10.2307/20183234");
});

test("collectPageMetadata finds a DOI in visible body text after metadata and links", () => {
  pageDocument(
    `<html><head><title>Visible paper</title></head><body><p>The DOI is 10.1000/body-layer.</p></body></html>`,
    "https://publisher.example/article",
  );
  expect(collectPageMetadata().doi).toBe("10.1000/body-layer");
});

test("openInstitutionSignIn surfaces the background failure reason", async () => {
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({
          ok: false,
          error: { code: "resolver_unavailable", message: "No resolver configured yet — open a paper first" },
        }),
      },
    },
  });
  await expect(openInstitutionSignIn()).rejects.toThrow("No resolver configured yet — open a paper first");
});

test("readCurrentPageMetadata keeps JSTOR detection when page scripting is unavailable", async () => {
  popupDocument();
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        query: async () => [{ id: 7, url: "https://www.jstor.org/stable/20183234" }],
      },
      scripting: {
        executeScript: async () => {
          throw new Error("script unavailable");
        },
      },
    },
  });
  await expect(readCurrentPageMetadata()).resolves.toMatchObject({
    doi: "10.2307/20183234",
    kind: "doi",
  });
});

test("institution session uses the shared card/button styles and explains missing resolver", () => {
  const doc = popupDocument();
  renderInstitutionSession(doc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    pausedForReauth: false,
    lastCheckAt: null,
    resolverOrigin: null,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  });
  expect(doc.getElementById("institution-session")?.classList.contains("launcher-action")).toBe(true);
  expect(doc.getElementById("institution-session-signin")?.classList.contains("primary")).toBe(true);
  // No resolver: the host slot stays empty and the status line carries the
  // label plus the one actionable hint.
  expect(doc.getElementById("institution-session-origin")?.textContent).toBe("");
  expect(doc.getElementById("institution-session-status")?.textContent).toBe(
    "No resolver configured yet · Open a paper first",
  );
  expect(doc.getElementById("institution-session-dismiss")).toBeNull();
});

test("unblocked notice shows once per release stamp and does not resurrect on polls", async () => {
  const doc = popupDocument();
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    pausedForReauth: false,
    lastCheckAt: Date.now(),
    resolverOrigin: "https://example.primo.exlibrisgroup.com",
    lastAuthReturnedAt: Date.now(),
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 1,
    releasedAuthJobsAt: 1_754_200_000_000,
  };
  renderInstitutionSession(doc, state);
  const notice = doc.getElementById("institution-session-unblocked");
  expect(notice?.hidden).toBe(false);
  expect(notice?.textContent).toBe("Sign-in unblocked 1 item");

  // Simulate the fade timer having hidden the notice, then a 5s session poll
  // re-delivering the same cumulative snapshot: it must stay hidden.
  if (notice instanceof HTMLElement) notice.hidden = true;
  renderInstitutionSession(doc, { ...state });
  expect(notice?.hidden).toBe(true);

  // A NEW release event (fresh stamp) re-announces.
  renderInstitutionSession(doc, { ...state, releasedAuthJobs: 2, releasedAuthJobsAt: 1_754_200_060_000 });
  expect(notice?.hidden).toBe(false);
  expect(notice?.textContent).toBe("Sign-in unblocked 2 items");
});

test("institution sign-in errors return to a working sign-in button with the reason", async () => {
  const doc = popupDocument();
  let attempts = 0;
  renderInstitutionSession(
    doc,
    {
      enabled: true,
      intervalMinutes: 4,
      authenticated: false,
      pausedForReauth: true,
      lastCheckAt: null,
      resolverOrigin: "https://resolver.example.edu",
      lastAuthReturnedAt: null,
      queuedAuthJobs: 0,
      stalledAuthJobs: [],
      releasedAuthJobs: 0,
    },
    async () => {
      attempts += 1;
      throw new Error("Could not open the institution sign-in");
    },
  );
  const button = doc.getElementById("institution-session-signin") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(attempts).toBe(1);
  expect(button.textContent).toBe("Sign in");
  expect(button.disabled).toBe(false);
  expect(doc.getElementById("institution-session-status")?.textContent).toBe(
    "Could not open the institution sign-in",
  );
});
test("session card matrix propagates marker scan outcomes", () => {
  const now = Date.now();
  const base = {
    enabled: true,
    intervalMinutes: 4,
    pausedForReauth: false,
    checking: false,
    likelyAuthenticated: false,
    lastCheckAt: now,
    resolverOrigin: "https://example.primo.exlibrisgroup.com",
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  };

  const noMarkers = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    scanOutcome: "no_markers",
    lastVerdictAt: now,
  });
  expect(noMarkers.label).toBe("Signed-in state unclear on this page");
  expect(noMarkers.detail).toBe("papio inspected your library tab but found no sign-in indicators");
  expect(noMarkers.action).toBe("signin");

  const failed = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    scanOutcome: "scan_failed",
    lastVerdictAt: now,
  });
  expect(failed.label).toBe("papio couldn't read the library page — check site access in Options");
  expect(failed.detail).toContain("via your library tab");

  const unknown = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "none",
    lastVerdictAt: now,
  });
  expect(unknown.label).toBe("Session unknown — open your library page to verify");
  expect(unknown.action).toBe("signin");
  expect(unknown.detail).toContain("no probe evidence");

  const signedOut = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "out",
    probeSource: "live_tab",
    scanOutcome: "markers",
    lastVerdictAt: now,
  });
  expect(signedOut.label).toBe("Signed out or expired");
  expect(signedOut.detail).toContain("via your library tab");

  const warm = deriveSessionCardState({
    ...base,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    scanOutcome: "markers",
    lastVerdictAt: now,
  });
  expect(warm.label).toContain("Session warm");
  expect(warm.detail).toMatch(/via your library tab · (just now|\d+m ago|\d+h ago)$/);
  // A warm session offers no sign-in action — the button is hidden, not dead.
  expect(warm.action).toBe("none");
});

test("session status lines omit degenerate probe detail and retain real evidence", () => {
  const now = Date.now();
  const defaultOrigin = "https://example.primo.exlibrisgroup.com";
  const uwaOrigin = "https://onesearch.library.example-college.edu";
  const base = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "unknown" as const,
    probeSource: "none" as const,
    lastVerdictAt: null,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastCheckAt: null,
    resolverOrigin: defaultOrigin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  };

  const single = popupDocument();
  renderInstitutionSession(single, {
    ...base,
    probeSource: "live_tab",
    lastVerdictAt: now,
  });
  expect(single.getElementById("institution-session-status")?.textContent).toBe("Checking session…");

  const multiple = popupDocument();
  renderInstitutionSession(multiple, {
    ...base,
    origins: [
      {
        origin: defaultOrigin,
        authenticated: false,
        verdict: "unknown",
        probeSource: "none",
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastCheckAt: now,
      },
      {
        origin: uwaOrigin,
        authenticated: false,
        verdict: "out",
        probeSource: "live_tab",
        scanOutcome: "markers",
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastCheckAt: now,
      },
    ],
  });
  const statuses = multiple.querySelectorAll<HTMLElement>(
    "#institution-session-rows .institution-session-status",
  );
  expect(statuses[0]?.textContent).toBe("Session unknown — open your library page to verify");
  expect(statuses[1]?.textContent).toMatch(/^Signed out or expired · via your library tab · /);
});

test("renders independent multi-origin session rows and targets each sign-in origin", async () => {
  const now = Date.now();
  const defaultOrigin = "https://example.primo.exlibrisgroup.com";
  const uwaOrigin = "https://onesearch.library.example-college.edu";
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "unknown" as const,
    probeSource: "none" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastCheckAt: now,
    resolverOrigin: defaultOrigin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    origins: [
      {
        origin: defaultOrigin,
        authenticated: true,
        verdict: "in" as const,
        probeSource: "live_tab" as const,
        scanOutcome: "markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastCheckAt: now,
      },
      {
        origin: uwaOrigin,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "live_tab" as const,
        scanOutcome: "markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastCheckAt: now,
      },
    ],
  };
  expect(deriveSessionRows(state)).toEqual([
    expect.objectContaining({ origin: defaultOrigin, action: "none" }),
    expect.objectContaining({ origin: uwaOrigin, label: "Signed out or expired", action: "signin" }),
  ]);

  const doc = popupDocument();
  const targets: string[] = [];
  renderInstitutionSession(doc, state, async (origin) => {
    if (origin !== undefined) targets.push(origin);
  });
  const rows = doc.querySelectorAll(".institution-session-origin-row");
  expect(rows).toHaveLength(2);
  expect(rows[0]?.textContent).toContain("example.primo.exlibrisgroup.com");
  expect(rows[1]?.textContent).toContain("onesearch.library.example-college.edu");
  expect(doc.getElementById("institution-session-row")).toBeNull();
  const buttons = Array.from(doc.querySelectorAll<HTMLButtonElement>(".institution-session-origin-row button"));
  expect(buttons).toHaveLength(2);
  expect(buttons[0]?.hidden).toBe(true);
  expect(buttons[1]?.hidden).toBe(false);
  expect(buttons[1]?.getAttribute("aria-describedby")).toBe("institution-session-status-1");
  buttons[1]?.click();
  await Promise.resolve();
  expect(targets).toEqual([uwaOrigin]);
});

test("one configured origin keeps the existing institution session card", () => {
  const now = Date.now();
  const origin = "https://example.primo.exlibrisgroup.com";
  const doc = popupDocument();
  renderInstitutionSession(doc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    scanOutcome: "markers",
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastCheckAt: now,
    resolverOrigin: origin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    origins: [{
      origin,
      authenticated: true,
      verdict: "in",
      probeSource: "live_tab",
      scanOutcome: "markers",
      lastVerdictAt: now,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      lastCheckAt: now,
    }],
  });
  expect(doc.getElementById("institution-session-rows")?.hidden).toBe(true);
  expect(doc.querySelector(".institution-session-row")?.hasAttribute("hidden")).toBe(false);
  expect(doc.getElementById("institution-session-origin")?.textContent).toBe(
    "example.primo.exlibrisgroup.com",
  );
  expect(doc.getElementById("institution-session-status")?.textContent).toContain("Session warm");
  expect(doc.getElementById("institution-session-signin")?.hidden).toBe(true);
});

test("leftover-tabs card stays hidden at zero and renders a pluralized count", () => {
  const doc = popupDocument();
  renderLeftoverTabs(doc, 0, async () => 0);
  const section = doc.getElementById("leftover-tabs");
  expect(section?.hasAttribute("hidden")).toBe(true);

  renderLeftoverTabs(doc, 3, async () => 3);
  expect(section?.hasAttribute("hidden")).toBe(false);
  expect(doc.getElementById("leftover-tabs-message")?.textContent).toContain("3 untracked tabs");

  renderLeftoverTabs(doc, 1, async () => 1);
  expect(doc.getElementById("leftover-tabs-message")?.textContent).toContain("1 untracked tab left");
});

test("leftover-tabs cleanup uses the latest callback after a rerender", async () => {
  const doc = popupDocument();
  const calls: string[] = [];
  renderLeftoverTabs(doc, 1, async () => {
    calls.push("old");
    return 1;
  });
  renderLeftoverTabs(doc, 1, async () => {
    calls.push("new");
    return 1;
  });
  (doc.getElementById("leftover-tabs-cleanup") as HTMLButtonElement).click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toEqual(["new"]);
});

test("leftover-tabs cleanup click closes the card and a failure re-arms the button", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderLeftoverTabs(doc, 2, async () => {
    calls += 1;
    if (calls === 1) throw new Error("cleanup blocked");
    return 2;
  });
  const section = doc.getElementById("leftover-tabs");
  const button = doc.getElementById("leftover-tabs-cleanup") as HTMLButtonElement;

  button.click();
  expect(button.disabled).toBe(true);
  await Promise.resolve();
  await Promise.resolve();
  // First attempt failed: the card persists and the button re-arms.
  expect(section?.hasAttribute("hidden")).toBe(false);
  expect(button.disabled).toBe(false);
  expect(button.textContent).toBe("Close them");

  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(2);
  expect(section?.hasAttribute("hidden")).toBe(true);
});
