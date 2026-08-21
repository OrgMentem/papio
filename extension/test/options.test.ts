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
  scannerAllowlistOrigins: Set<string>;
  runtimeMessages: Array<{ type: string; request: Record<string, unknown> }>;
  storageGetKeys: string[];
  storageSetKeys: string[];
}

interface OptionsPageOptions {
  origins?: readonly string[];
  removeTakesEffect?: boolean;
  pageCaptureConsent?: boolean;
  scannerAllowlistOrigins?: readonly string[];
  allowlistSetFails?: Record<string, boolean>;
  /** Hold an `allowlist.set` open so a genuinely pending row can be observed. */
  allowlistSetGate?: (origin: string) => Promise<void>;
  storageSetFails?: Set<string>;
  storageGetFails?: Set<string>;
  resolverOrigins?: string[];
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
    ...(options.resolverOrigins === undefined ? {} : { papio_state_v1: { resolverOrigins: options.resolverOrigins, version: 5 } }),
  };
  const containsCalls: string[][] = [];
  const grantedOrigins = new Set(options.origins ?? []);
  const removeTakesEffect = options.removeTakesEffect ?? true;
  const scannerAllowlistOrigins = new Set(options.scannerAllowlistOrigins ?? []);
  const runtimeMessages: Array<{ type: string; request: Record<string, unknown> }> = [];
  const storageGetKeys: string[] = [];
  const storageSetKeys: string[] = [];

  Object.assign(globalThis, {
    window,
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLInputElement: window.HTMLInputElement,
    HTMLSelectElement: window.HTMLSelectElement,
    HTMLLIElement: window.HTMLLIElement,
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
        sendMessage: async (message: { type: string; request?: Record<string, unknown> }) => {
          runtimeMessages.push({ type: message.type, request: message.request ?? {} });
          if (message.type === "papio.pageBulk.allowlist.list") {
            return { ok: true, origins: [...scannerAllowlistOrigins].sort() };
          }
          if (message.type === "papio.pageBulk.allowlist.set") {
            const gate = options.allowlistSetGate;
            if (gate !== undefined) await gate(message.request?.origin as string);
            const origin = message.request?.origin as string;
            const allowed = message.request?.allowed as boolean;
            if (options.allowlistSetFails?.[origin]) {
              return {
                ok: false,
                error: { code: "unavailable", message: "could not revoke page scanning for this site" },
              };
            }
            if (allowed) scannerAllowlistOrigins.add(origin);
            else scannerAllowlistOrigins.delete(origin);
            return { ok: true, allowed };
          }
          return {};
        },
      },
      storage: {
        session: {
          get: async (key?: unknown) => {
            if (options.storageGetFails?.has("__any__") || options.storageGetFails?.has("papio_state_v1")) throw new Error("storage unavailable");
            const k = typeof key === "string" ? key : Array.isArray(key) ? (key[0] as string) : key ? Object.keys(key as Record<string, unknown>)[0] : undefined;
            if (k && k in storageValues) return { [k]: (storageValues as Record<string, unknown>)[k] };
            if (k === undefined) return { ...storageValues };
            return {};
          },
        },
        local: {
          get: async (key: string | string[] | Record<string, unknown> | null | undefined) => {
            const keys = Array.isArray(key) ? key : typeof key === "string" ? [key] : key ? Object.keys(key as Record<string, unknown>) : [];
            for (const entry of keys) storageGetKeys.push(entry);
            if (options.storageGetFails && keys.some((k) => options.storageGetFails!.has(k))) throw new Error("storage unavailable");
            if (options.storageGetFails?.has("__any__")) throw new Error("storage unavailable");
            return Object.fromEntries(
              keys.filter((entry) => entry in storageValues).map((entry) => [entry, storageValues[entry]]),
            );
          },
          set: async (items: Record<string, unknown>) => {
            for (const entry of Object.keys(items)) storageSetKeys.push(entry);
            if (options.storageSetFails && Object.keys(items).some((k) => options.storageSetFails!.has(k))) throw new Error("quota exceeded");
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
    scannerAllowlistOrigins,
    runtimeMessages,
    storageGetKeys,
    storageSetKeys,
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

test("a permission row spends one visible line and keeps the exact pattern reachable", async () => {
  const page = await optionsDocument();
  const origin = "https://*.journals.sagepub.com/*";
  const row = sourceRow(page.document, "sources", origin);
  expect(row).toBeDefined();

  // Hover and keyboard focus both read the pattern off the row itself.
  expect(row?.getAttribute("data-tip")).toBe(origin);

  // The friendly label is the only line that costs pixels.
  const label = row?.querySelector(".source-label") as HTMLElement;
  expect(label.textContent).toBe("SAGE Journals");
  const host = row?.querySelector(".source-host") as HTMLElement;
  expect(host.textContent).toBe(origin);
  const hostStyle = page.document.defaultView!.getComputedStyle(host);
  expect(hostStyle.position).toBe("absolute");
  expect(hostStyle.height).toBe("1px");
  expect(hostStyle.clipPath).toBe("inset(50%)");

  // Off-screen, never out of the accessibility tree.
  expect(host.getAttribute("aria-hidden")).toBeNull();
  expect(host.hidden).toBe(false);

  // The switch's accessible name still carries friendly name and pattern.
  const name = row?.querySelector("button[role='switch']")?.getAttribute("aria-label");
  expect(name).toBe(`Access to SAGE Journals (${origin})`);
});

test("a row with no switch still exposes its pattern to hover and assistive tech", async () => {
  const page = await optionsDocument({ origins: [ALL_SITES_ORIGIN] });
  const origin = "https://*.journals.sagepub.com/*";
  const row = sourceRow(page.document, "sources", origin);

  expect(row?.querySelector("button[role='switch']")).toBeNull();
  expect(row?.getAttribute("data-tip")).toBe(origin);
  expect(row?.querySelector(".source-host")?.getAttribute("aria-hidden")).toBeNull();
  // The consequence copy is not a pattern restatement, so it stays visible.
  expect(row?.querySelector(".hint")?.textContent).toContain("Covered by all-sites access");
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
const SCANNER_ALLOWLIST_STORAGE_KEY = "papio_scanner_allowlist_v1";

function scannerRow(document: Document, origin: string): HTMLLIElement | undefined {
  return sourceRow(document, "scanner-allowlist", origin);
}

test("scanner allowlist empty state shows the exact sentence and hides the list", async () => {
  const page = await optionsDocument();
  const list = page.document.getElementById("scanner-allowlist");
  const empty = page.document.getElementById("scanner-allowlist-empty") as HTMLElement;
  expect(list?.hidden).toBe(true);
  expect(empty.hidden).toBe(false);
  expect(empty.textContent).toBe("No sites are allowed for page scanning.");
});

test("scanner allowlist lists each origin with its own Stop allowing control", async () => {
  const origin = "https://journals.example";
  const page = await optionsDocument({ scannerAllowlistOrigins: [origin] });
  const row = scannerRow(page.document, origin);
  expect(row).toBeDefined();
  const button = row?.querySelector("button");
  expect(button?.textContent).toBe("Stop allowing");
});

test("a scanner allowlist row shows the host once and keeps the exact origin reachable", async () => {
  const origin = "https://journals.example";
  const page = await optionsDocument({ scannerAllowlistOrigins: [origin] });
  const row = scannerRow(page.document, origin);

  expect(row?.querySelector(".source-label")?.textContent).toBe("journals.example");
  expect(row?.getAttribute("data-tip")).toBe(origin);
  expect(row?.querySelector(".source-host")?.textContent).toBe(origin);
  expect(row?.querySelector(".source-host")?.getAttribute("aria-hidden")).toBeNull();
  expect(row?.querySelector("button")?.getAttribute("aria-label")).toBe(
    `Stop allowing page scanning on ${origin}`,
  );
});

test("successful scanner revocation removes only that row", async () => {
  const keep = "https://keep.example";
  const drop = "https://drop.example";
  const page = await optionsDocument({ scannerAllowlistOrigins: [keep, drop] });
  const dropRow = scannerRow(page.document, drop);
  (dropRow?.querySelector("button") as HTMLButtonElement).click();
  await settle();
  expect(page.scannerAllowlistOrigins.has(drop)).toBe(false);
  expect(scannerRow(page.document, drop)).toBeUndefined();
  expect(scannerRow(page.document, keep)).toBeDefined();
});

test("failed scanner allowlist.set keeps the row and shows local feedback", async () => {
  const origin = "https://fail.example";
  const page = await optionsDocument({
    scannerAllowlistOrigins: [origin],
    allowlistSetFails: { [origin]: true },
  });
  (scannerRow(page.document, origin)?.querySelector("button") as HTMLButtonElement).click();
  await settle();
  expect(scannerRow(page.document, origin)).toBeDefined();
  const message = page.document.getElementById("scanner-allowlist-message") as HTMLElement;
  expect(message.hidden).toBe(false);
  expect(message.getAttribute("data-tone")).toBe("degraded");
  expect(message.textContent).toContain("could not revoke");
});

test("a pending scanner revoke disables its own control and leaves the others usable", async () => {
  const first = "https://first.example";
  const second = "https://second.example";
  const held = Promise.withResolvers<void>();
  const page = await optionsDocument({
    scannerAllowlistOrigins: [first, second],
    allowlistSetGate: async (origin) => {
      if (origin === first) await held.promise;
    },
  });
  const firstButton = scannerRow(page.document, first)?.querySelector("button") as HTMLButtonElement;
  const secondButton = scannerRow(page.document, second)?.querySelector("button") as HTMLButtonElement;
  firstButton.click();
  await settle();
  // The first row is genuinely in flight and says so.
  expect(firstButton.disabled).toBe(true);
  expect(scannerRow(page.document, first)).toBeDefined();
  expect(secondButton.disabled).toBe(false);

  // The second row must actually work, not merely look enabled: a control that
  // silently ignores a click is worse than a disabled one.
  secondButton.click();
  await settle();
  expect(scannerRow(page.document, second)).toBeUndefined();
  expect(scannerRow(page.document, first)).toBeDefined();

  held.resolve();
  await settle();
  expect(scannerRow(page.document, first)).toBeUndefined();
});

test("scanner allowlist management never reads scanner storage directly", async () => {
  const page = await optionsDocument({ scannerAllowlistOrigins: ["https://journals.example"] });
  expect(page.storageGetKeys).not.toContain(SCANNER_ALLOWLIST_STORAGE_KEY);
  expect(page.storageSetKeys).not.toContain(SCANNER_ALLOWLIST_STORAGE_KEY);
  expect(page.runtimeMessages.some((entry) => entry.type === "papio.pageBulk.allowlist.list")).toBe(true);
});

test("feedback settings revert and show an error when storage.set rejects", async () => {
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on("unhandledRejection", onRejection);
  const page = await optionsDocument({ storageSetFails: new Set(["papio_toolbar_count_mode_v1"]) });
  const toolbar = page.document.getElementById("toolbar-count-mode") as HTMLSelectElement;
  const msg = page.document.getElementById("feedback-settings-message") as HTMLElement;
  expect(toolbar.value).toBe("required");
  toolbar.value = "all";
  toolbar.dispatchEvent(new Event("change"));
  await settle();
  expect(rejections).toHaveLength(0);
  expect(toolbar.value).toBe("required");
  expect(toolbar.disabled).toBe(false);
  expect(msg.hidden).toBe(false);
  expect(msg.textContent).toContain("could not save");
  process.off("unhandledRejection", onRejection);
});

test("catch-up toggle reverts and shows an error when storage.set rejects", async () => {
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on("unhandledRejection", onRejection);
  const page = await optionsDocument({ storageSetFails: new Set(["papio_catch_up_enabled_v1"]) });
  const catchUp = page.document.getElementById("catch-up-enabled") as HTMLInputElement;
  const msg = page.document.getElementById("feedback-settings-message") as HTMLElement;
  expect(catchUp.checked).toBe(true);
  catchUp.checked = false;
  catchUp.dispatchEvent(new Event("change"));
  await settle();
  expect(rejections).toHaveLength(0);
  expect(catchUp.checked).toBe(true);
  expect(catchUp.disabled).toBe(false);
  expect(msg.hidden).toBe(false);
  expect(msg.textContent).toContain("could not save");
  process.off("unhandledRejection", onRejection);
});

test("success-ack select reverts and shows an error when storage.set rejects", async () => {
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on("unhandledRejection", onRejection);
  const page = await optionsDocument({ storageSetFails: new Set(["papio_success_ack_mode_v1"]) });
  const success = page.document.getElementById("success-ack-mode") as HTMLSelectElement;
  const msg = page.document.getElementById("feedback-settings-message") as HTMLElement;
  expect(success.value).toBe("all");
  success.value = "errors";
  success.dispatchEvent(new Event("change"));
  await settle();
  expect(rejections).toHaveLength(0);
  expect(success.value).toBe("all");
  expect(success.disabled).toBe(false);
  expect(msg.hidden).toBe(false);
  expect(msg.textContent).toContain("could not save");
  process.off("unhandledRejection", onRejection);
});

test("handoff surface reverts and shows an error when storage.set rejects", async () => {
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on("unhandledRejection", onRejection);
  const page = await optionsDocument({ storageSetFails: new Set(["papio_handoff_surface_v1"]) });
  const tabGroup = page.document.getElementById("handoff-tab-group") as HTMLButtonElement;
  const work = page.document.getElementById("handoff-work-window") as HTMLButtonElement;
  const msg = page.document.getElementById("handoff-surface-message") as HTMLElement;
  // Initial render: work-window is active
  expect(work.getAttribute("aria-pressed")).toBe("true");
  tabGroup.click();
  await settle();
  expect(rejections).toHaveLength(0);
  expect(tabGroup.getAttribute("aria-pressed")).toBe("false");
  expect(work.getAttribute("aria-pressed")).toBe("true");
  expect(tabGroup.disabled).toBe(false);
  expect(msg.hidden).toBe(false);
  expect(msg.textContent).toContain("could not save");
  process.off("unhandledRejection", onRejection);
});

test("configured resolvers renders unavailable state when storage load rejects", async () => {
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on("unhandledRejection", onRejection);
  const page = await optionsDocument({
    resolverOrigins: ["https://resolver.example"],
    storageGetFails: new Set(["__any__"]),
  });
  await settle();
  expect(rejections).toHaveLength(0);
  const section = page.document.getElementById("configured-resolvers-section") as HTMLElement;
  const list = page.document.getElementById("configured-resolvers") as HTMLUListElement;
  const msg = page.document.getElementById("configured-resolvers-message") as HTMLElement;
  expect(section.hidden).toBe(false);
  expect(list.textContent).toContain("unavailable");
  const li = list.querySelector("li");
  expect(li).toBeDefined();
  expect(li instanceof HTMLLIElement).toBe(true);
  expect(msg.hidden).toBe(false);
  expect(msg.textContent).toContain("could not load");
  process.off("unhandledRejection", onRejection);
});
