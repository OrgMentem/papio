// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture-capture tests: sanitizeFixture must strip scripts/queries/fragments/
// form values and mask token-shaped secrets deterministically, and captureFixture
// must wire scripting -> sanitize -> downloads while failing closed on a dirty
// result. No real chrome, no DOM — pure string checks and an injected fake API.

import { expect, test } from "bun:test";

import {
  captureFixture,
  downloadFixture,
  MAX_CAPTURE_BYTES,
  sanitizeFixture,
  residualLeak,
  takePendingFixtureFilename,
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

test("script/style/textarea/SVG bodies are emptied but tags survive", () => {
  const out = sanitizeFixture(
    `<script>document.cookie</script><style>body{color:red}</style><textarea>typed secret</textarea>` +
      `<svg aria-label="icon"><style>.secret{}</style><path d="${TOKEN}"></path></svg>`,
    META,
  );
  expect(out).not.toContain("document.cookie");
  expect(out).not.toContain("color:red");
  expect(out).not.toContain("typed secret");
  expect(out).toContain("<script></script>");
  expect(out).toContain("<textarea></textarea>");
  expect(out).toContain(`<svg aria-label="icon"></svg>`);
});

test("query strings and fragments are stripped from url attributes", () => {
  const out = sanitizeFixture(
    `<a href="https://www.proquest.com/pdf/1?tk=SENTINELabc#frag">x</a>` +
      `<img src="/img/logo.png?v=3"><button data-href="?id=doi:10.1000/example&sid=provider">Lookup</button>`,
    META,
  );
  expect(out).toContain(`href="https://www.proquest.com/pdf/1"`);
  expect(out).toContain(`src="/img/logo.png"`);
  expect(out).toContain(`data-href=""`);
  expect(out).not.toContain("SENTINEL");
  expect(out).not.toContain("#frag");
  expect(out).not.toContain("v=3");
});

test("semantic URL paths survive while opaque path segments and queries are masked", () => {
  const out = sanitizeFixture(
    `<a href="/products/ejournals/pdf/10.1055/a-2821-8219.pdf?ticket=${TOKEN}">PDF</a>` +
      `<a href="/download/56ec3ea3-966b-4a98-9584-f8f51fe6f1d0/file.pdf">opaque</a>`,
    META,
  );
  expect(out).toContain(`href="/products/ejournals/pdf/10.1055/a-2821-8219.pdf"`);
  expect(out).toContain(`href="/download/TOKEN/file.pdf"`);
  expect(out).not.toContain("ticket=");
  expect(residualLeak(out)).toBeNull();
});

test("provider URL metas remain queryless selector evidence while token metas are dropped", () => {
  const out = sanitizeFixture(
    `<meta name="citation_pdf_url" content="https://hal.science/hal-04206682/document?token=${TOKEN}">` +
      `<meta name="wkhealth_pdf_url" content="https://journals.lww.com/downloadpdf.aspx?an=${TOKEN}">` +
      `<meta name="csrf-token" content="${TOKEN}">`,
    META,
  );
  expect(out).toContain(
    `name="citation_pdf_url" content="https://hal.science/hal-04206682/document"`,
  );
  expect(out).toContain(
    `name="wkhealth_pdf_url" content="https://journals.lww.com/downloadpdf.aspx"`,
  );
  expect(out).not.toContain("csrf-token");
  expect(out).not.toContain("token=");
  expect(residualLeak(out)).toBeNull();
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

test("short request ids and CSP nonces are blanked and guarded", () => {
  const out = sanitizeFixture(
    `<script src="/bundle.js" nonce="9ab35b571eb2fec3"></script>` +
      `<div data-request-id="9ab35b571eb2fec3-MNL" class="page">x</div>` +
      `<meta name="request-id" content="95054841648dcefe-SJC">`,
    META,
  );
  expect(out).toContain(`nonce=""`);
  expect(out).toContain(`data-request-id=""`);
  expect(out).not.toContain("9ab35b571eb2fec3");
  expect(out).not.toContain("95054841648dcefe");
  expect(residualLeak(out)).toBeNull();
  // The guard itself rejects a survivor independently of the rewriter.
  expect(residualLeak(`<div data-request-id="9ab35b571eb2fec3-MNL">x</div>`)).toContain(
    "per-request identifier",
  );
  expect(residualLeak(`<meta name="request-id" content="95054841648dcefe-SJC">`)).toContain(
    "per-request identifier",
  );
});

test("email addresses are masked in text, attributes, and mailto links", () => {
  const out = sanitizeFixture(
    `<a href="mailto:masayoshi.mase.mh@hitachi.com">Contact</a>` +
      `<meta name="citation_author_email" content="owen@stanford.edu">` +
      `<span>reach bbseiler@stanford.edu for reprints</span>`,
    META,
  );
  expect(out).not.toContain("hitachi.com");
  expect(out).not.toContain("stanford.edu");
  expect(out).not.toContain("@");
  expect(residualLeak(out)).toBeNull();
  expect(residualLeak(`<span>owen@stanford.edu</span>`)).toContain("email address");
});

test("provider-specific URL attributes lose queries like href/src do", () => {
  const out = sanitizeFixture(
    `<div data-fullTexturl="/content/journals/10.1146/x.html?itemId=/content/journals/10.1146/x&mimeType=html">a</div>` +
      `<comp institution-log-in-url="https://www.cambridge.org/core/shibboleth?app=core&ref=/core/product" register-url="/core/register?ref=/core/product">b</comp>`,
    META,
  );
  expect(out).toContain(`data-fullTexturl="/content/journals/10.1146/x.html"`);
  expect(out).toContain(`institution-log-in-url="https://www.cambridge.org/core/shibboleth"`);
  expect(out).toContain(`register-url="/core/register"`);
  expect(out).not.toContain("itemId=");
  expect(out).not.toContain("ref=");
  expect(residualLeak(out)).toBeNull();
  expect(residualLeak(`<div data-fullTexturl="/x.html?itemId=1">a</div>`)).toContain(
    "query string",
  );
});

test("stable selector prefixes survive while per-record id suffixes are masked", () => {
  const out = sanitizeFixture(
    `<a id="downloadPDFLink_MSTAR_216440925" href="/pdf/1">PDF</a>` +
      `<div id="fulltext_translation2_MSTAR_216440925">x</div>`,
    META,
  );
  expect(out).toContain(`id="downloadPDFLink_MSTAR_TOKEN"`);
  expect(out).toContain(`id="fulltext_TOKEN_MSTAR_TOKEN"`);
  expect(out).not.toContain("216440925");
  expect(residualLeak(out)).toBeNull();
});

test("inline styles are blanked because they are not selector evidence", () => {
  const out = sanitizeFixture(
    `<html style="--viewer-container-height: 1070px"><div style="background:url(${TOKEN})">x</div></html>`,
    META,
  );
  expect(out).toContain(`<html style="">`);
  expect(out).toContain(`<div style="">x</div>`);
  expect(out).not.toContain("--viewer-container-height");
  expect(out).not.toContain(TOKEN);
});

test("residual guard ignores framework tag and attribute names", () => {
  const clean = sanitizeFixture(
    `<mfe-content-details-pharos-button data-sveltekit-preload-data="off" id="mfeSupportChatMountPoint" class="value-propositions__icon">ok</mfe-content-details-pharos-button>`,
    META,
  );
  expect(residualLeak(clean)).toBeNull();

});
test("semantic provider test hooks remain usable as selector evidence", () => {
  const out = sanitizeFixture(
    `<button data-auto="card-call-to-action-download-button">Download</button>`,
    META,
  );
  expect(out).toContain(`data-auto="card-call-to-action-download-button"`);
  expect(residualLeak(out)).toBeNull();
});


test("opaque selector identifiers are masked instead of becoming adapter dependencies", () => {
  const out = sanitizeFixture(`<div id="56ec3ea3-966b-4a98-9584-f8f51fe6f1d0">x</div>`, META);
  expect(out).toContain(`id="TOKEN"`);
  expect(out).not.toContain("56ec3ea3");
  expect(residualLeak(out)).toBeNull();
});

test("token-shaped runs are masked in BOTH attributes and text", () => {
  const out = sanitizeFixture(`<span data-token="${TOKEN}">${TOKEN}</span>`, META);
  expect(out).toContain(`data-token="TOKEN"`);
  expect(out).toContain(`<span data-token="TOKEN">TOKEN</span>`);
  expect(out).not.toContain(TOKEN);
});

test("comments are emptied without merging adjacent markup", () => {
  const out = sanitizeFixture(
    `before<!--<a href="/account?token=${TOKEN}">private</a>-->` +
      `<mfe-content-details-pharos-button>ok</mfe-content-details-pharos-button><!--eslint-enable-->after`,
    META,
  );
  expect(out).toContain(
    `before<!----><mfe-content-details-pharos-button>ok</mfe-content-details-pharos-button><!---->after`,
  );
  expect(out).not.toContain("private");
  expect(residualLeak(out)).toBeNull();
});

test("meta tags carrying a token-shaped content are dropped", () => {
  const out = sanitizeFixture(`<meta name="csrf-token" content="${TOKEN}"><title>ok</title>`, META);
  expect(out).not.toContain("csrf-token");
  expect(out).not.toContain(TOKEN);
  expect(out).toContain("<title>ok</title>");
});

test("class/id selectors and explicit provider test hooks stay verbatim", () => {
  const out = sanitizeFixture(
    `<a class="download-link" id="main" data-test="pdf-link">Download</a>`,
    META,
  );
  expect(out).toContain(`class="download-link"`);
  expect(out).toContain(`id="main"`);
  expect(out).toContain(`data-test="pdf-link"`);
});

test("the papio-fixture header is the first line and exactly formatted", () => {
  const out = sanitizeFixture(`<p>hi</p>`, META);
  const firstLine = out.slice(0, out.indexOf("\n"));
  expect(firstLine).toBe(
    `<!-- papio-fixture provider="proquest" scenario="success" origin="https://www.proquest.com/docview/1" captured="2026-07-14T00:00:00.000Z" -->`,
  );
});

test("residual guard accepts the validated provenance header with a semantic long origin", () => {
  const out = sanitizeFixture(`<p>paywall</p>`, {
    ...META,
    provider: "nature",
    scenario: "no-entitlement",
    originNoQuery: "https://www.nature.com/articles/nature14539",
  });
  expect(residualLeak(out)).toBeNull();
});

test("residual guard still rejects token-shaped values in non-provenance comments", () => {
  const out =
    `<!-- papio-fixture provider="nature" scenario="success" origin="https://www.nature.com/articles/TOKEN" captured="2026-07-14T00:00:00.000Z" -->\n` +
    `<!-- ${TOKEN} -->`;
  expect(residualLeak(out)).toContain("token-shaped value");
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

test("fixture writes enqueue their path for onDeterminingFilename (data: URLs ignore filename)", async () => {
  // Chrome ignores downloads.download's filename for data: URLs, so the intended
  // path must be recoverable by the onDeterminingFilename listener.
  // Drain any residue enqueued by other tests (module-level FIFO queue).
  while (takePendingFixtureFilename("data:text/html") !== undefined) {
    /* drain */
  }
  const { api } = fakeChrome(CLEAN_PAGE);
  await downloadFixture(api, "papio-fixtures/observed/www.jstor.org/2026.html", "<p>x</p>");
  const dataUrl = "data:text/html;charset=utf-8,%3Cp%3Ex%3C%2Fp%3E";

  // A non-fixture (real) download is never relocated by the fixture queue.
  expect(takePendingFixtureFilename("https://www.jstor.org/stable/pdf/1.pdf")).toBeUndefined();
  // The fixture download's intended path is dequeued once, then drained.
  expect(takePendingFixtureFilename(dataUrl)).toBe("papio-fixtures/observed/www.jstor.org/2026.html");
  expect(takePendingFixtureFilename(dataUrl)).toBeUndefined();
});

test("a rejected fixture download does not leave a filename for the next download", async () => {
  while (takePendingFixtureFilename("data:text/html") !== undefined) {
    /* drain */
  }
  const rejected: Pick<ChromeCaptureApi, "downloads"> = {
    downloads: {
      download: async () => {
        throw new Error("download rejected");
      },
    },
  };
  await expect(downloadFixture(rejected, "papio-fixtures/stale.html", "<p>stale</p>")).rejects.toThrow(
    "download rejected",
  );

  const { api } = fakeChrome(CLEAN_PAGE);
  await downloadFixture(api, "papio-fixtures/current.html", "<p>current</p>");
  expect(takePendingFixtureFilename("data:text/html;charset=utf-8,%3Cp%3Ecurrent%3C%2Fp%3E")).toBe(
    "papio-fixtures/current.html",
  );
});

test("capture masks a token-shaped provider path before writing", async () => {
  const dynamicPath: PageCapture = {
    html: `<div class="record-details">x</div>`,
    origin: "https://www.jstor.org",
    path: `/stable/${TOKEN}`,
  };
  const { api, downloads } = fakeChrome(dynamicPath);
  const result = await captureFixture(api, "jstor", "drift", FIXED_NOW);

  expect(result.ok).toBe(true);
  expect(downloads).toHaveLength(1);
  const encoded = downloads[0]?.url.split(",", 2)[1];
  expect(encoded).toBeDefined();
  const decoded = decodeURIComponent(encoded ?? "");
  expect(decoded).not.toContain(TOKEN);
  expect(decoded).toContain(`origin="https://www.jstor.org/stable/TOKEN"`);
  expect(residualLeak(decoded)).toBeNull();
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
