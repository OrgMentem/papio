// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import { adapters } from "../src/adapters/types";

const window = new Window();
window.document.write(readFileSync(new URL("../src/options.html", import.meta.url), "utf8"));
Object.assign(globalThis, {
  document: window.document,
  Event: window.Event,
  HTMLElement: window.HTMLElement,
  HTMLButtonElement: window.HTMLButtonElement,
  HTMLUListElement: window.HTMLUListElement,
});

const permissionRequests: string[][] = [];
const permissionRemovals: string[][] = [];
Object.assign(globalThis, {
  chrome: {
    permissions: {
      contains: async () => false,
      request: async ({ origins }: { origins: string[] }) => {
        permissionRequests.push(origins);
        return true;
      },
      remove: async ({ origins }: { origins: string[] }) => {
        permissionRemovals.push(origins);
        return true;
      },
    },
    runtime: {
      getManifest: () => ({ version: "0.0.0", host_permissions: [] }),
    },
    storage: {
      session: { get: async () => ({}) },
      local: { get: async () => ({}), set: async () => {} },
    },
  },
});

// options.ts initializes against chrome and document at import time, so load it
// only after this test installs the options-page environment.
const { PROVIDER_SOURCES } = await import("../src/options");
const sourceList = document.getElementById("sources") as HTMLUListElement;
const providerOrigins = PROVIDER_SOURCES.map((source) => source.origin);

test("renders a grant control for every registered adapter host", async () => {
  await Promise.resolve();

  for (const adapter of adapters) {
    for (const host of adapter.hosts) {
      const origin = `https://*.${host.toLowerCase()}/*`;
      const row = Array.from(sourceList.querySelectorAll("li")).find((item) =>
        item.textContent?.includes(origin),
      );
      expect(row).toBeDefined();
      expect(row?.querySelector("button")?.textContent).toBe("Grant");
    }
  }
});

test("derives unique origins and retains every PsycNet host", () => {
  expect(new Set(providerOrigins).size).toBe(providerOrigins.length);
  expect(providerOrigins).toContain("https://*.psycnet.apa.org/*");
  expect(providerOrigins).toContain("https://*.doi.apa.org/*");
});

test("bulk provider grant and revoke use the derived origin set", async () => {
  permissionRequests.length = 0;
  permissionRemovals.length = 0;

  (document.getElementById("grant-all") as HTMLButtonElement).click();
  await Promise.resolve();
  expect(permissionRequests).toEqual([providerOrigins]);

  (document.getElementById("revoke-all") as HTMLButtonElement).click();
  await Promise.resolve();
  expect(permissionRemovals).toEqual([providerOrigins]);
});
