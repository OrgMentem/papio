// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import { adapters } from "../src/adapters/types";
import type { Source } from "../src/options";
import { PAGE_CAPTURE_CONSENT_KEY } from "../src/state";

const ALL_SITES_ORIGIN = "https://*/*";
let importSerial = 0;

interface OptionsPage {
  document: Document;
  providerOrigins: string[];
  permissionRequests: string[][];
  permissionRemovals: string[][];
  containsCalls: string[][];
  grantedOrigins: Set<string>;
  storageValues: Record<string, unknown>;
}

interface OptionsPageOptions {
  origins?: readonly string[];
  removeTakesEffect?: boolean;
  pageCaptureConsent?: boolean;
}

async function settle(): Promise<void> {
  for (let iteration = 0; iteration < 12; iteration += 1) await Promise.resolve();
}

function containsRequestedOrigins(origins: readonly string[], grantedOrigins: ReadonlySet<string>): boolean {
  for (const origin of origins) {
    if (!grantedOrigins.has(origin) && !grantedOrigins.has(ALL_SITES_ORIGIN)) return false;
  }
  return true;
}

function sourceRow(document: Document, listID: string, origin: string): HTMLLIElement | undefined {
  const list = document.getElementById(listID);
  if (!list) return undefined;
  return Array.from(list.querySelectorAll("li")).find(
    (item) => item.querySelector(".source-host")?.textContent === origin,
  );
}

async function optionsDocument(options: OptionsPageOptions = {}): Promise<OptionsPage> {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/options.html", import.meta.url), "utf8"));
  const permissionRequests: string[][] = [];
  const permissionRemovals: string[][] = [];
  const storageValues: Record<string, unknown> = {
    ...(options.pageCaptureConsent === undefined ? {} : { [PAGE_CAPTURE_CONSENT_KEY]: options.pageCaptureConsent }),
  };
  const containsCalls: string[][] = [];
  const grantedOrigins = new Set(options.origins ?? []);
  const removeTakesEffect = options.removeTakesEffect ?? true;
  Object.assign(globalThis, {
    window,
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLInputElement: window.HTMLInputElement,
    HTMLSelectElement: window.HTMLSelectElement,
    HTMLUListElement: window.HTMLUListElement,
    chrome: {
      permissions: {
        contains: async ({ origins }: { origins: string[] }) => {
          containsCalls.push([...origins]);
          return containsRequestedOrigins(origins, grantedOrigins);
        },
        getAll: async () => ({ origins: [...grantedOrigins] }),
        request: async ({ origins }: { origins: string[] }) => {
          permissionRequests.push([...origins]);
          for (const origin of origins) grantedOrigins.add(origin);
          return true;
        },
        remove: async ({ origins }: { origins: string[] }) => {
          permissionRemovals.push([...origins]);
          if (removeTakesEffect) {
            for (const origin of origins) grantedOrigins.delete(origin);
          }
          return true;
        },
      },
      runtime: {
        getManifest: () => ({ version: "0.0.0", host_permissions: [] }),
      },
      storage: {
        session: { get: async () => ({}) },
        local: {
          get: async (key: string | string[]) => {
            const keys = Array.isArray(key) ? key : [key];
            return Object.fromEntries(
              keys.filter((entry) => entry in storageValues).map((entry) => [entry, storageValues[entry]]),
            );
          },
          set: async (items: Record<string, unknown>) => {
            Object.assign(storageValues, items);
          },
        },
      },
    },
  });
  importSerial += 1;
  // Each case owns both permission state and the document the page captures at import time.
  const { ALL_SITES_ORIGIN: pageAllSitesOrigin, PROVIDER_SOURCES } = await import(
    `../src/options.ts?options-test=${importSerial}`
  );
  expect(pageAllSitesOrigin).toBe(ALL_SITES_ORIGIN);
  await settle();
  return {
    document: window.document as unknown as Document,
    providerOrigins: PROVIDER_SOURCES.map((source: Source) => source.origin),
    storageValues,
    permissionRequests,
    permissionRemovals,
    containsCalls,
    grantedOrigins,
  };
}

test("renders an ungranted switch for every registered adapter host", async () => {
  const page = await optionsDocument();

  for (const adapter of adapters) {
    for (const host of adapter.hosts) {
      const origin = `https://*.${host.toLowerCase()}/*`;
      const row = sourceRow(page.document, "sources", origin);
      expect(row).toBeDefined();
      const toggle = row?.querySelector("button[role='switch']");
      expect(toggle).toBeDefined();
      expect(toggle?.getAttribute("aria-checked")).toBe("false");
    }
  }
});

test("derives unique origins and retains every PsycNet host", async () => {
  const page = await optionsDocument();

  expect(new Set(page.providerOrigins).size).toBe(page.providerOrigins.length);
  expect(page.providerOrigins).toContain("https://*.psycnet.apa.org/*");
  expect(page.providerOrigins).toContain("https://*.doi.apa.org/*");
});

test("shows all-sites access as its own controllable permission", async () => {
  const page = await optionsDocument();
  const row = sourceRow(page.document, "all-sites-access", ALL_SITES_ORIGIN);
  expect(row).toBeDefined();
  expect(row?.textContent).toContain("covers every site");
  const toggle = row?.querySelector("button[role='switch']") as HTMLButtonElement | null | undefined;
  expect(toggle).not.toBeNull();
  expect(toggle?.getAttribute("aria-checked")).toBe("false");

  toggle?.click();
  await settle();
  expect(page.permissionRequests).toEqual([[ALL_SITES_ORIGIN]]);
  expect(page.grantedOrigins.has(ALL_SITES_ORIGIN)).toBe(true);
  const grantedRow = sourceRow(page.document, "all-sites-access", ALL_SITES_ORIGIN);
  expect(grantedRow?.querySelector("button[role='switch']")?.getAttribute("aria-checked")).toBe("true");

  (grantedRow?.querySelector("button[role='switch']") as HTMLButtonElement | null)?.click();
  await settle();
  expect(page.permissionRemovals).toEqual([[ALL_SITES_ORIGIN]]);
  expect(page.grantedOrigins.has(ALL_SITES_ORIGIN)).toBe(false);
  const revokedRow = sourceRow(page.document, "all-sites-access", ALL_SITES_ORIGIN);
  expect(revokedRow?.querySelector("button[role='switch']")?.getAttribute("aria-checked")).toBe("false");
});

test("renders sources covered only by all-sites access without a revocable switch", async () => {
  const page = await optionsDocument({ origins: [ALL_SITES_ORIGIN] });
  const origin = "https://*.journals.sagepub.com/*";
  const row = sourceRow(page.document, "sources", origin);

  expect(row).toBeDefined();
  expect(row?.textContent).toContain("Covered by all-sites access");
  expect(row?.querySelector("button[role='switch']")).toBeNull();
  expect(page.containsCalls).toEqual([]);
});

test("revoke all removes the broad grant with the provider origins", async () => {
  const page = await optionsDocument({ origins: [ALL_SITES_ORIGIN] });

  (page.document.getElementById("revoke-all") as HTMLButtonElement).click();
  await settle();
  expect(page.permissionRemovals).toEqual([[...page.providerOrigins, ALL_SITES_ORIGIN]]);
  expect(page.grantedOrigins.has(ALL_SITES_ORIGIN)).toBe(false);
  const row = sourceRow(page.document, "sources", "https://*.journals.sagepub.com/*");
  expect(row?.querySelector("button[role='switch']")?.getAttribute("aria-checked")).toBe("false");
  expect((page.document.getElementById("provider-permission-message") as HTMLElement).hidden).toBe(true);
});

test("explains when a bulk revoke cannot remove all-sites access", async () => {
  const page = await optionsDocument({
    origins: [ALL_SITES_ORIGIN],
    removeTakesEffect: false,
  });

  (page.document.getElementById("revoke-all") as HTMLButtonElement).click();
  await settle();
  expect(page.permissionRemovals[0]).toContain(ALL_SITES_ORIGIN);
  const message = page.document.getElementById("provider-permission-message") as HTMLElement;
  expect(message.hidden).toBe(false);
  expect(message.textContent).toContain("All-sites access is still active");
});

test("re-reads an individual grant after a remove reports success without changing state", async () => {
  const origin = "https://*.journals.sagepub.com/*";
  const page = await optionsDocument({
    origins: [origin],
    removeTakesEffect: false,
  });
  const row = sourceRow(page.document, "sources", origin);
  const toggle = row?.querySelector("button[role='switch']") as HTMLButtonElement | null | undefined;
  expect(toggle?.getAttribute("aria-checked")).toBe("true");

  toggle?.click();
  await settle();
  expect(page.permissionRemovals).toEqual([[origin]]);
  expect(page.grantedOrigins.has(origin)).toBe(true);
  const refreshedRow = sourceRow(page.document, "sources", origin);
  expect(refreshedRow?.querySelector("button[role='switch']")?.getAttribute("aria-checked")).toBe("true");
});

test("keeps version diagnostics collapsed in settings", async () => {
  const page = await optionsDocument();
  const diagnostics = page.document.querySelector("details.diagnostics");

  expect(diagnostics).not.toBeNull();
  expect(diagnostics?.hasAttribute("open")).toBe(false);
  expect(diagnostics?.contains(page.document.getElementById("daemon-footer"))).toBe(true);
});
test("persists the Firefox page-capture consent checkbox", async () => {
  const page = await optionsDocument();
  const checkbox = page.document.getElementById("page-capture-consent") as HTMLInputElement;
  expect(checkbox.checked).toBe(false);
  expect(checkbox.disabled).toBe(false);

  checkbox.checked = true;
  checkbox.dispatchEvent(new Event("change"));
  await settle();
  expect(page.storageValues[PAGE_CAPTURE_CONSENT_KEY]).toBe(true);

  const restored = await optionsDocument({ pageCaptureConsent: true });
  const restoredCheckbox = restored.document.getElementById("page-capture-consent") as HTMLInputElement;
  expect(restoredCheckbox.checked).toBe(true);
});

test("persists feedback and interruption settings", async () => {
  const page = await optionsDocument();
  const toolbar = page.document.getElementById("toolbar-count-mode") as HTMLSelectElement;
  const catchUp = page.document.getElementById("catch-up-enabled") as HTMLInputElement;
  const success = page.document.getElementById("success-ack-mode") as HTMLSelectElement;

  expect(toolbar.value).toBe("required");
  expect(catchUp.checked).toBe(true);
  expect(success.value).toBe("all");

  toolbar.value = "all";
  toolbar.dispatchEvent(new Event("change"));
  catchUp.checked = false;
  catchUp.dispatchEvent(new Event("change"));
  success.value = "errors";
  success.dispatchEvent(new Event("change"));
  await settle();

  expect(page.storageValues["papio_toolbar_count_mode_v1"]).toBe("all");
  expect(page.storageValues["papio_catch_up_enabled_v1"]).toBe(false);
  expect(page.storageValues["papio_success_ack_mode_v1"]).toBe("errors");
});
