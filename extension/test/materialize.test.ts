// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

const shell = readFileSync(new URL("../src/materialize.html", import.meta.url), "utf8");
const source = readFileSync(new URL("../src/materialize.ts", import.meta.url), "utf8");
let importSerial = 0;

async function materializeDocument(binding?: string): Promise<Document> {
  const url = new URL("https://extension.test/materialize.html");
  if (binding !== undefined) url.hash = binding;
  const window = new Window({ url: url.href });
  window.document.write(shell);

  const previous = {
    document: globalThis.document,
    HTMLElement: globalThis.HTMLElement,
    location: globalThis.location,
    window: globalThis.window,
  };
  Object.assign(globalThis, {
    document: window.document,
    HTMLElement: window.HTMLElement,
    location: window.location,
    window,
  });
  try {
    importSerial += 1;
    await import(`../src/materialize.ts?materialize-test=${importSerial}`);
    return window.document as unknown as Document;
  } finally {
    Object.assign(globalThis, previous);
  }
}

test("valid fragment renders one inert binding marker", async () => {
  const binding = "Abc_123-";
  const document = await materializeDocument(binding);
  const status = document.querySelector("#materialize-status");

  expect(document.querySelectorAll("#materialize-status")).toHaveLength(1);
  expect(status?.tagName).toBe("OUTPUT");
  expect(status?.getAttribute("role")).toBe("status");
  expect(status?.getAttribute("data-state")).toBe("valid");
  expect(status?.getAttribute("data-binding-id")).toBe(binding);
  expect(status?.textContent).toBe("Materialization binding ready");
  expect(document.querySelectorAll("a,button,input,select,textarea,form")).toHaveLength(0);
});

test.each([
  [undefined, "missing fragment"],
  ["short7", "too short"],
  ["a".repeat(129), "too long"],
  ["valid.binding", "punctuation"],
  ["valid%20id", "encoded content"],
])("%s fragment renders closed invalid state (%s)", async (binding) => {
  const document = await materializeDocument(binding);
  const status = document.querySelector("#materialize-status");

  expect(status?.getAttribute("data-state")).toBe("invalid");
  expect(status?.hasAttribute("data-binding-id")).toBe(false);
  expect(status?.textContent).toBe("Invalid materialization binding");
});

test("shell and script expose no query data or active work", () => {
  expect(shell).not.toContain("materialize.html?");
  expect(shell).not.toMatch(/<(?:a|button|input|select|textarea|form)\b/i);
  expect(source).not.toMatch(/(?:fetch|XMLHttpRequest|chrome\.|browser\.|localStorage|sessionStorage|history\.|location\.(?:assign|replace)|runtime\.|storage\.)/);
  expect(source).not.toMatch(/\b(?:work|profile|provider|route|url)\b/i);
});
