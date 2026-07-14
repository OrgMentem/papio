// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture-capture tests: sanitizeFixture must strip scripts/queries/fragments/
// form values and mask token-shaped secrets deterministically, and captureFixture
// must wire scripting -> sanitize -> downloads while failing closed on a dirty
// result. No real chrome, no DOM — pure string checks and an injected fake API.

import { expect, test } from "bun:test";

import {
  captureFixture,
  MAX_CAPTURE_BYTES,
  sanitizeFixture,
  type ChromeCaptureApi,
  type FixtureMeta,
  type PageCapture,
} from "../src/capture";

const TOKEN = "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"; // 32 URL-safe chars → token-shaped

const META: FixtureMeta = {
  provider: "proquest",
  scenario: "success",
  originNoQuery: "https://www.proquest.com/docview/1",
  capturedISO: "2026-07-14T00:00:00.000Z",
};

test("script/style/textarea bodies are emptied but tags survive", () => {
  const out = sanitizeFixture(
    `<div><script>fetch('/steal?c='+document.cookie)</script><style>.x{color:red}</style><textarea>typed secret</textarea></div>`,
    META,
  );
  expect(out).not.toContain("document.cookie");
  expect(out).not.toContain("color:red");
  expect(out).not.toContain("typed secret");
  expect(out).toContain("<script></script>");
  expect(out).toContain("<textarea></textarea>");
});

test("query strings and fragments are stripped from url attributes", () => {
  const out = sanitizeFixture(
    `<a href="https://www.proquest.com/pdf/1?tk=SENTINELabc#frag">x</a><img src="/img/logo.png?v=3">`,
    META,
  );
  expect(out).toContain(`href="https://www.proquest.com/pdf/1"`);
  expect(out).toContain(`src="/img/logo.png"`);
  expect(out).not.toContain("SENTINEL");
  expect(out).not.toContain("#frag");
  expect(out).not.toContain("v=3");
});

test("input values and value attributes are blanked", () => {
  const out = sanitizeFixture(
    `<input type="password" name="pw" value="hunter2" autocomplete="current-password">`,
    META,
  );
  expect(out).toContain(`value=""`);
  expect(out).not.toContain("hunter2");
  expect(out).not.toContain("autocomplete");
  expect(out).toContain(`name="pw"`); // structural attrs survive
});

test("token-shaped runs are masked in BOTH attributes and text", () => {
  const out = sanitizeFixture(`<span data-token="${TOKEN}">${TOKEN}</span>`, META);
  expect(out).toContain(`data-token="TOKEN"`);
  expect(out).toContain(`<span data-token="TOKEN">TOKEN</span>`);
  expect(out).not.toContain(TOKEN);
});

test("meta tags carrying a token-shaped content are dropped", () => {
  const out = sanitizeFixture(`<meta name="csrf-token" content="${TOKEN}"><title>ok</title>`, META);
  expect(out).not.toContain("csrf-token");
  expect(out).not.toContain(TOKEN);
  expect(out).toContain("<title>ok</title>");
});

test("class/id selectors stay verbatim", () => {
  const out = sanitizeFixture(`<div class="login-form panel" id="main">hi</div>`, META);
  expect(out).toContain(`class="login-form panel"`);
  expect(out).toContain(`id="main"`);
});

test("the papio-fixture header is the first line and exactly formatted", () => {
  const out = sanitizeFixture(`<p>hi</p>`, META);
  const firstLine = out.slice(0, out.indexOf("\n"));
  expect(firstLine).toBe(
    `<!-- papio-fixture provider="proquest" scenario="success" origin="https://www.proquest.com/docview/1" captured="2026-07-14T00:00:00.000Z" -->`,
  );
});

test("sanitization is deterministic", () => {
  const html = `<a href="https://x/y?z=1"><input value="${TOKEN}">text ${TOKEN}</a>`;
  expect(sanitizeFixture(html, META)).toBe(sanitizeFixture(html, META));
});

test("a sentinel secret hidden in a query string never survives", () => {
  const secret = "SENTINEL_" + TOKEN;
  const out = sanitizeFixture(`<a href="https://sso.example/cb?ticket=${secret}">go</a>`, META);
  expect(out).not.toContain("SENTINEL");
  expect(out).not.toContain(secret);
});

// --- popup wiring against a fake chrome ------------------------------------

const CLEAN_PAGE: PageCapture = {
  html: `<html><head><title>ProQuest</title></head><body><h1 class="doc-title" id="t">Trust</h1><a class="dl" href="https://www.proquest.com/pdf/1?tk=x">PDF</a><script>var s="secret";</script><input name="q" value="typed"></body></html>`,
  origin: "https://www.proquest.com",
  path: "/docview/1",
};

function fakeChrome(page: PageCapture | undefined, tabId: number | null = 7) {
  const downloads: Array<{ url: string; filename: string; conflictAction: string; saveAs: boolean }> = [];
  const api: ChromeCaptureApi = {
    tabs: { query: async () => [{ id: tabId ?? undefined }] },
    scripting: { executeScript: async () => [{ result: page }] },
    downloads: {
      download: async (options) => {
        downloads.push(options);
        return 42;
      },
    },
  };
  return { api, downloads };
}

const FIXED_NOW = (): Date => new Date("2026-07-14T00:00:00.000Z");

test("capture downloads sanitized HTML at the versioned fixture path", async () => {
  const { api, downloads } = fakeChrome(CLEAN_PAGE);
  const result = await captureFixture(api, "proquest", "success", FIXED_NOW);

  expect(result).toEqual({ ok: true, downloadId: 42, filename: "papio-fixtures/proquest/success.html" });
  expect(downloads).toHaveLength(1);
  const dl = downloads[0]!;
  expect(dl.conflictAction).toBe("uniquify");
  expect(dl.saveAs).toBe(false);

  const comma = dl.url.indexOf(",");
  const decoded = decodeURIComponent(dl.url.slice(comma + 1));
  const expected = sanitizeFixture(CLEAN_PAGE.html, {
    provider: "proquest",
    scenario: "success",
    originNoQuery: "https://www.proquest.com/docview/1",
    capturedISO: "2026-07-14T00:00:00.000Z",
  });
  expect(decoded).toBe(expected);
  expect(decoded).not.toContain("?tk="); // the download link's query is gone
  expect(decoded.startsWith("<!-- papio-fixture ")).toBe(true);
});

test("capture refuses to write a fixture that still carries a token", async () => {
  // A token-shaped run lodged in a structural class attr is NOT scrubbed, so the
  // fail-closed residual check must catch it and abort before any download.
  const dirty: PageCapture = {
    html: `<div class="${TOKEN}">x</div>`,
    origin: "https://www.jstor.org",
    path: "/stable/1",
  };
  const { api, downloads } = fakeChrome(dirty);
  const result = await captureFixture(api, "jstor", "drift", FIXED_NOW);

  expect(result.ok).toBe(false);
  if (!result.ok) expect(result.error).toContain("dirty fixture");
  expect(downloads).toHaveLength(0);
});

test("capture fails closed on a missing active tab", async () => {
  const { api, downloads } = fakeChrome(CLEAN_PAGE, null);
  const result = await captureFixture(api, "ebsco", "terms", FIXED_NOW);
  expect(result.ok).toBe(false);
  expect(downloads).toHaveLength(0);
});

test("capture rejects an over-cap payload before serializing", async () => {
  const huge: PageCapture = { html: "x".repeat(MAX_CAPTURE_BYTES + 1), origin: "https://x", path: "/" };
  const { api, downloads } = fakeChrome(huge);
  const result = await captureFixture(api, "springer", "no-entitlement", FIXED_NOW);
  expect(result.ok).toBe(false);
  if (!result.ok) expect(result.error).toContain("cap");
  expect(downloads).toHaveLength(0);
});
