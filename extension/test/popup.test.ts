// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import {
  acquireCurrentPage,
  collectPageMetadata,
  OPEN_INBOX_MESSAGE,
  openInbox,
  renderDaemonStatus,
  renderPageAcquire,
  renderPageContext,
  renderResolverGrants,
  renderTermsConsent,
  wireCapture,
  wireDevTools,
  wireInboxLauncher,
  wirePrimaryShortcut,
  wireSettings,
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

test("renders one card with two verbs and no redundant launcher headings", () => {
  const doc = popupDocument();
  const launcher = doc.querySelector(".launcher");

  expect(doc.querySelector("h1")).toBeNull();
  expect(launcher?.querySelectorAll(".launcher-action")).toHaveLength(2);
  expect(launcher?.querySelector("h2")).toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.textContent).toBe("Acquire this page");
  expect(doc.getElementById("page-acquire-doi")?.textContent).toBe("Detecting DOI…");
  expect(doc.getElementById("open-inbox-btn")?.textContent).toBe("Open inbox");
  expect(doc.getElementById("needs-you-section")).toBeNull();
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

test("shows capture tools only for unpacked Chrome manifests", () => {
  const production = popupDocument();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        getManifest: () => ({ manifest_version: 3, update_url: "https://clients2.google.com/service/update2/crx" }),
      },
    },
  });
  wireDevTools(production);
  expect(production.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
  expect(production.querySelectorAll("#capture-provider option")).toHaveLength(0);

  const unpacked = popupDocument();
  Object.assign(globalThis, {
    chrome: { runtime: { getManifest: () => ({ manifest_version: 3 }) } },
  });
  wireDevTools(unpacked);
  expect(unpacked.querySelector<HTMLElement>(".capture")?.hidden).toBe(false);
  expect(unpacked.querySelectorAll("#capture-provider option")).toHaveLength(PROVIDERS.length);

  const firefox = popupDocument();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        getManifest: () => ({ manifest_version: 3, browser_specific_settings: { gecko: { id: "papio@example.test" } } }),
      },
    },
  });
  wireDevTools(firefox);
  expect(firefox.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
});

test("renders daemon problems by the actions and the version in the footer", () => {
  const doc = popupDocument();
  renderDaemonStatus(doc, { connectionStatus: "connected", daemonVersion: "0.1.0" });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(true);
  expect(doc.getElementById("daemon-footer")?.hidden).toBe(false);
  expect(doc.getElementById("daemon-footer")?.textContent).toBe("papio daemon v0.1.0");

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
  expect(doc.getElementById("daemon-footer")?.textContent).toBe("papio daemon v0.1.0");
  delete (globalThis as Record<string, unknown>).__PAPIO_DAEMON_VERSION__;

  renderDaemonStatus(doc, { connectionStatus: "disconnected" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain("papio daemon isn't reachable");
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe("run: papio daemon status");
  expect(doc.getElementById("daemon-footer")?.hidden).toBe(true);
});

test("keeps acquisition available with a detected DOI even without a negotiated daemon", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    throw new Error("papio daemon isn't reachable");
  });
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(section?.hidden).toBe(false);
  expect(button.disabled).toBe(false);
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
  expect(button.disabled).toBe(false);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("papio daemon isn't reachable");
});

test("disables page acquisition when the current page has no DOI", () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  });
  renderPageContext(doc, undefined, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  expect(doc.getElementById("page-acquire-doi")?.textContent).toBe("No DOI detected on this page");
  button.click();
  expect(calls).toBe(0);
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

test("shows the detected DOI and a local in-flight acquisition", () => {
  const doc = popupDocument();
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, [
    job({ expected: { doi: "doi:10.1000/example" } }),
  ]);

  expect(doc.getElementById("page-acquire-doi")?.textContent).toBe("Detected DOI: 10.1000/example");
  expect(doc.getElementById("page-acquire-context")?.textContent).toBe(
    "An acquisition for this DOI is already in progress.",
  );
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
  Object.assign(globalThis, {
    chrome: {
      runtime: { sendMessage: async () => undefined },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
  });
  wireInboxLauncher(doc);

  (doc.getElementById("open-inbox-btn") as HTMLButtonElement).click();
  await Promise.resolve();
  await Promise.resolve();
  expect(created).toEqual([{ url: "dist/inbox.html" }]);
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
