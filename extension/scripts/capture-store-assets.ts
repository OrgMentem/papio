// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Regenerate every Chrome Web Store visual asset from the *real* extension UI in
// one command:
//
//   bun run capture:store        # build dist/ + render all assets
//   bun run capture:store --skip-build
//
// Outputs (git-ignored) land in extension/web-ext-artifacts/store-assets/:
//   screenshot-popup.png     1280x800  toolbar popup, connected + DOI detected
//   screenshot-options.png   1280x800  options page, provider access controls
//   screenshot-inbox.png     1280x800  triage inbox, actions + watch hits
//   promo-small.png            440x280  small promo tile (24-bit, no alpha)
//   promo-marquee.png         1400x560  marquee promo tile (24-bit, no alpha)
//
// How it stays faithful: the shipped dist/{popup,options,inbox}.html pages are
// served over http (file:// blocks ES-module imports) and rendered in real
// headless Chrome. A `chrome.*` shim is injected *before* page scripts run, so
// every pixel is the actual UI render — only the daemon *data* is stubbed. Promo
// tiles are branded marketing graphics rendered from a generated template.
//
// Requires: system Google Chrome (or $PAPIO_CHROME) and ImageMagick (`magick`).
// Chrome Web Store caps screenshots at 1280x800 and promo tiles at their exact
// canvas sizes, so we render at 2x and Lanczos-downscale for crisp text.

import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import puppeteer, { type Browser, type Page } from "puppeteer-core";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const extensionDir = resolve(scriptDir, "..");
const distDir = join(extensionDir, "dist");
const outDir = join(extensionDir, "web-ext-artifacts", "store-assets");

const manifest = JSON.parse(await readFile(join(extensionDir, "manifest.json"), "utf8")) as {
  version: string;
  host_permissions?: string[];
};
const VERSION = manifest.version;
const HOST_PERMISSIONS = manifest.host_permissions ?? [];

// ---------------------------------------------------------------------------
// External tooling discovery
// ---------------------------------------------------------------------------

function findChrome(): string {
  const fromEnv = process.env.PAPIO_CHROME;
  if (fromEnv && existsSync(fromEnv)) return fromEnv;
  const candidates = [
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
  ];
  for (const c of candidates) if (existsSync(c)) return c;
  throw new Error("Chrome not found. Set $PAPIO_CHROME to the browser executable path.");
}

function findMagick(): string {
  for (const bin of ["magick", "convert"]) {
    try {
      execFileSync(bin, ["-version"], { stdio: "ignore" });
      return bin;
    } catch {
      // try next
    }
  }
  throw new Error("ImageMagick not found. Install it (`brew install imagemagick`).");
}

const CHROME = findChrome();
const MAGICK = findMagick();

// ---------------------------------------------------------------------------
// Static file server for dist/ (file:// blocks ES-module imports)
// ---------------------------------------------------------------------------

const contentType = (path: string): string => {
  if (path.endsWith(".html")) return "text/html; charset=utf-8";
  if (path.endsWith(".js")) return "text/javascript; charset=utf-8";
  if (path.endsWith(".css")) return "text/css; charset=utf-8";
  if (path.endsWith(".json")) return "application/json";
  if (path.endsWith(".png")) return "image/png";
  return "application/octet-stream";
};

function startServer() {
  return Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(req) {
      const path = new URL(req.url).pathname.replace(/^\/+/, "");
      const file = Bun.file(join(distDir, path || "index.html"));
      if (!(await file.exists())) return new Response("not found", { status: 404 });
      return new Response(file, { headers: { "content-type": contentType(path) } });
    },
  });
}

// ---------------------------------------------------------------------------
// chrome.* shims — injected before page scripts so the UI renders faithfully
// ---------------------------------------------------------------------------

const popupState = {
  activeJobs: [],
  connectionStatus: "connected",
  daemonVersion: VERSION,
  daemonUpdateHint: false,
  daemonFeatures: [],
  resolverOrigins: [],
};

const popupPage = {
  url: "https://journals.sagepub.com/doi/10.1177/01461672241234567",
  doi: "10.1177/01461672241234567",
  title: "Belonging and Motivation in First-Year Students",
};

const makePopupMock = (state: Record<string, unknown>) => `(() => {
  const store = ${JSON.stringify(state)};
  const page = ${JSON.stringify(popupPage)};
  globalThis.chrome = {
    runtime: { getManifest: () => ({ version: ${JSON.stringify(VERSION)}, update_url: "https://clients2.google.com/service/update2/crx" }), sendMessage: async () => ({}), openOptionsPage: () => {} },
    storage: { local: { get: async (k) => (k === "papio_state_v1" ? { papio_state_v1: store } : {}), set: async () => {} } },
    permissions: { contains: async () => true, request: async () => true },
    tabs: { query: async () => [{ id: 1 }], create: async () => {} },
    scripting: { executeScript: async () => [{ result: page }] },
  };
})();`;

const popupMock = makePopupMock(popupState);

// Attention state: daemon unreachable — the popup's built-in warning card.
const popupAttentionMock = makePopupMock({ ...popupState, connectionStatus: "disconnected" });

const optionsMock = `(() => {
  const store = ${JSON.stringify({ ...popupState })};
  const manifest = { version: ${JSON.stringify(VERSION)}, update_url: "https://clients2.google.com/service/update2/crx", host_permissions: ${JSON.stringify(HOST_PERMISSIONS)} };
  globalThis.chrome = {
    runtime: {
      getManifest: () => manifest,
      sendMessage: async (message) => {
        if (message && message.type === "papio.pageBulk.allowlist.list") {
          return { ok: true, origins: ["https://journals.example"] };
        }
        return {};
      },
      openOptionsPage: () => {},
    },
    storage: { local: { get: async (k) => (k === "papio_state_v1" ? { papio_state_v1: store } : {}), set: async () => {} } },
    permissions: { contains: async () => true, request: async () => true, remove: async () => true },
  };
})();`;

const inboxItems = [
  {
    kind: "human_action",
    id: "act-1",
    rank: 1,
    action_kind: "openurl_handoff",
    job_id: "7fA3kQ9mZ2bLpX4n",
    title: "Bilingual working memory and cognitive control in school-age children",
    requires_auth: true,
    facts: [
      { label: "Authors", text: "Thanh Nguyen, Soojin Park" },
      { label: "Year", text: "2024" },
      { label: "Detail", text: "Sign in to your institution in the handoff tab; papio finishes the download." },
    ],
    links: [{ rel: "doi", url: "https://doi.org/10.1017/S1366728924000112" }],
    ops: ["open", "dismiss"],
    work: { doi: "10.1017/S1366728924000112", title: "Bilingual working memory and cognitive control in school-age children", authors: "Thanh Nguyen, Soojin Park", year: 2024, is_oa: false },
  },
  {
    kind: "human_action",
    id: "act-2",
    rank: 2,
    action_kind: "verify_identity",
    title: "Sleep quality and academic performance: a longitudinal study",
    revision: 1,
    sha256: "ab12cd34ef56",
    facts: [
      { label: "Authors", text: "Maria Alvarez, Liam OBrien" },
      { label: "Year", text: "2023" },
      { label: "Detail", text: "papio downloaded a PDF but could not confirm it matches. Preview it, then accept or reject." },
    ],
    links: [{ rel: "doi", url: "https://doi.org/10.1037/edu0000789" }],
    ops: ["accept", "reject"],
  },
  {
    kind: "human_action",
    id: "act-3",
    rank: 3,
    action_kind: "manual_download",
    title: "Emotion regulation strategies across adulthood",
    requires_auth: false,
    facts: [
      { label: "Authors", text: "Yuki Tanaka, Grace Miller" },
      { label: "Year", text: "2022" },
      { label: "Detail", text: "Download the PDF from the open tab; papio will file it in your library." },
    ],
    links: [{ rel: "doi", url: "https://doi.org/10.1093/geronb/gbac015" }],
    ops: ["open", "dismiss"],
  },
  {
    kind: "watch_hit",
    id: "wh-1",
    rank: 4,
    title: "Attention networks and mind-wandering during reading",
    facts: [
      { label: "Authors", text: "Chen Wei, Ana Rodrigues" },
      { label: "Year", text: "2025" },
    ],
    links: [
      { rel: "doi", url: "https://doi.org/10.1016/j.cognition.2025.105678" },
      { rel: "openalex", url: "https://openalex.org/W4399123456" },
    ],
    ops: ["acquire", "dismiss"],
    watches: [{ id: 1, label: "attention & memory" }],
    work: { doi: "10.1016/j.cognition.2025.105678", title: "Attention networks and mind-wandering during reading", authors: "Chen Wei, Ana Rodrigues", year: 2025, is_oa: true },
  },
  {
    kind: "watch_hit",
    id: "wh-2",
    rank: 5,
    title: "Neural correlates of second-language acquisition in adults",
    facts: [
      { label: "Authors", text: "Omar Haddad, Elena Petrova" },
      { label: "Year", text: "2025" },
    ],
    links: [{ rel: "doi", url: "https://doi.org/10.1093/cercor/bhaf021" }],
    ops: ["acquire", "dismiss"],
    watches: [{ id: 2, label: "language & the brain" }],
    work: { doi: "10.1093/cercor/bhaf021", title: "Neural correlates of second-language acquisition in adults", authors: "Omar Haddad, Elena Petrova", year: 2025, is_oa: false },
  },
];

const inboxCounts = {
  pending_total: 5,
  watch_hits: 2,
  actions: 3,
  retractions: 0,
  jobs_working: 1,
  jobs_needs_review: 1,
  failure_groups_7d: 0,
};

const inboxMock = `(() => {
  const items = ${JSON.stringify(inboxItems)};
  const counts = ${JSON.stringify(inboxCounts)};
  const snapshot = { schema: 1, generated_at: new Date().toISOString(), counts, items, has_more: false, unsupported_items_count: 0 };
  globalThis.chrome = {
    runtime: {
      getManifest: () => ({ version: ${JSON.stringify(VERSION)} }),
      sendMessage: async (m) => {
        if (m && m.type === "papio.triage.snapshot") return { ok: true, snapshot };
        if (m && m.type === "papio.triage.counts") return { ok: true, counts };
        return { ok: true };
      },
      openOptionsPage: () => {},
    },
    storage: { local: { get: async () => ({}), set: async () => {} } },
    tabs: { create: async () => {} },
    permissions: { contains: async () => true },
  };
})();`;

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

const SCALE = 2;

async function downscale(rawPng: Buffer, name: string, width: number, height: number, opaqueBg?: string) {
  const tmp = join(outDir, `.${name}.raw.png`);
  const finalPath = join(outDir, `${name}.png`);
  await writeFile(tmp, rawPng);
  const args = [tmp, "-filter", "Lanczos", "-resize", `${width}x${height}`];
  if (opaqueBg) args.push("-background", opaqueBg, "-flatten");
  args.push("-strip", `PNG24:${finalPath}`);
  execFileSync(MAGICK, args, { stdio: "ignore" });
  await rm(tmp, { force: true });
  return finalPath;
}

async function newPage(browser: Browser, width: number, height: number, mock?: string): Promise<Page> {
  const page = await browser.newPage();
  await page.setViewport({ width, height, deviceScaleFactor: SCALE });
  await page.emulateMediaFeatures([{ name: "prefers-color-scheme", value: "light" }]);
  if (mock) await page.evaluateOnNewDocument(mock);
  return page;
}

async function capturePopup(browser: Browser, base: string, name: string, mock: string) {
  const page = await newPage(browser, 1280, 800, mock);
  await page.goto(`${base}/popup.html`, { waitUntil: "load" });
  // Wait until the async refresh() has painted its state (acquire section or the
  // daemon-status warning card, depending on the variant).
  await page.waitForFunction(() => {
    const acq = document.getElementById("page-acquire-btn") as HTMLButtonElement | null;
    const card = document.getElementById("daemon-status");
    return (acq !== null && !acq.disabled) || (card !== null && !card.hidden);
  }, { timeout: 10000 });
  await page.addStyleTag({
    content:
      "html{min-height:100vh;display:flex!important;align-items:center;justify-content:center;background:linear-gradient(135deg,#eaf3ff 0%,#dce8f5 55%,#cdd9e8 100%)!important}" +
      "body{transform:scale(1.7);box-shadow:0 30px 80px rgba(20,40,80,.28);border-radius:14px;overflow:hidden;border:1px solid #dce3ea}",
  });
  await Bun.sleep(250);
  const raw = (await page.screenshot({ type: "png" })) as Buffer;
  await downscale(raw, name, 1280, 800);
  await page.close();
  console.log(`  ${name}.png`);
}

async function captureScreenshots(browser: Browser, base: string) {
  // Popup — healthy (connected + DOI detected) and attention (daemon unreachable).
  await capturePopup(browser, base, "screenshot-popup", popupMock);
  await capturePopup(browser, base, "screenshot-popup-attention", popupAttentionMock);

  // Options — settings page, top viewport (header through provider access).
  {
    const page = await newPage(browser, 1280, 800, optionsMock);
    await page.goto(`${base}/options.html`, { waitUntil: "load" });
    await page.waitForFunction(() => document.querySelectorAll("button").length > 3, { timeout: 10000 });
    await Bun.sleep(300);
    const raw = (await page.screenshot({ type: "png" })) as Buffer;
    await downscale(raw, "screenshot-options", 1280, 800);
    await page.close();
    console.log("  screenshot-options.png");
  }

  // Inbox — triage list with human actions and watch hits.
  {
    const page = await newPage(browser, 1280, 800, inboxMock);
    await page.goto(`${base}/inbox.html`, { waitUntil: "load" });
    await page.waitForFunction(() => document.querySelectorAll(".triage-item").length >= 5, { timeout: 10000 });
    await Bun.sleep(300);
    const raw = (await page.screenshot({ type: "png" })) as Buffer;
    await downscale(raw, "screenshot-inbox", 1280, 800);
    await page.close();
    console.log("  screenshot-inbox.png");
  }
}

// ---------------------------------------------------------------------------
// Promo tiles (branded marketing graphics)
// ---------------------------------------------------------------------------

const BG = "#f4f1e8";

// Extract the peeking-baboon cameo from the animated docs wordmark and freeze it
// into a static, standalone SVG (drop the animation; force its settled pose).
async function buildBaboonSvg(): Promise<string> {
  const src = await readFile(resolve(extensionDir, "..", "docs", "assets", "logo-wordmark.svg"), "utf8");
  const open = src.indexOf('<g class="baboon-motion"');
  if (open < 0) throw new Error("baboon-motion group not found in logo-wordmark.svg");
  // Walk from the opening tag, balancing <g>/</g> to find its matching close.
  let depth = 0;
  let i = src.indexOf(">", open) + 1;
  const innerStart = i;
  for (; i < src.length; ) {
    const g = src.indexOf("<g", i);
    const c = src.indexOf("</g>", i);
    if (c < 0) throw new Error("unterminated baboon-motion group");
    if (g >= 0 && g < c) { depth++; i = g + 2; continue; }
    if (depth === 0) break;
    depth--;
    i = c + 4;
  }
  // Drop the standalone chest rect: it exists only so the docs porthole has a
  // flat block to clip. Unclipped it reads as a peg under the round mantle,
  // whose own path already carries rounded shoulders. Papers/paws stay.
  const inner = src.slice(innerStart, i).replace(/<rect class="baboon-mantle"[^>]*\/>/, "");
  // viewBox wraps the measured baboon bbox (x[-132,136] y[-90,71]) + ~8px pad,
  // so the full mane fits with no side/bottom clipping.
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="-140 -98 284 177">` +
    `<style>` +
    `.baboon-mantle{fill:#2B2D42}.baboon-face{fill:#E85D4A}` +
    `.baboon-line{stroke:#2B2D42;fill:none}.baboon-feature{fill:#2B2D42;stroke:#2B2D42}` +
    `.paper-sheet{fill:#F5F1E8;stroke:#2B2D42}.paper-line{stroke:#E85D4A;fill:none}` +
    `.baboon-paw{fill:#E85D4A}.eyes-shut{opacity:0}` +
    `</style>${inner}</svg>`;
}

function promoHtml(iconDataUrl: string, variant: "small" | "marquee", baboonDataUrl: string): string {
  const marquee = variant === "marquee";
  const iconSize = marquee ? 176 : 96;
  const wordmark = marquee ? 108 : 60;
  const tagline = marquee ? 30 : 18;
  const layout = marquee
    ? "flex-direction:row;gap:56px;text-align:left;padding:0 120px 0 96px;justify-content:center"
    : "flex-direction:column;gap:18px;text-align:center;padding:26px";
  const features = marquee
    ? `<div class="chips">
         <span>Open-access first</span>
         <span>Every PDF validated</span>
         <span>Your real session — no bot</span>
       </div>`
    : "";
  // Baboon cameo peeking up in the bottom-right corner, holding its papers.
  const baboon = marquee ? `<img class="baboon" src="${baboonDataUrl}" alt="">` : "";
  return `<!doctype html><html><head><meta charset="utf-8"><style>
    *{margin:0;box-sizing:border-box;-webkit-font-smoothing:antialiased}
    html,body{width:100%;height:100%}
    body{
      position:relative;display:flex;align-items:center;${layout};
      font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
      background:radial-gradient(120% 140% at 12% 0%, #ffffff 0%, ${BG} 46%, #ece7da 100%);
      color:#1f2b45;overflow:hidden;
    }
    .icon{width:${iconSize}px;height:${iconSize}px;border-radius:${Math.round(iconSize * 0.22)}px;
      box-shadow:0 18px 44px rgba(31,43,69,.20);flex:0 0 auto;background:#f5f2ea}
    .copy{display:flex;flex-direction:column;gap:${marquee ? 16 : 8}px;align-items:${marquee ? "flex-start" : "center"}}
    .wordmark{font-size:${wordmark}px;font-weight:800;font-style:italic;letter-spacing:-.02em;line-height:1;color:#1f2b45}
    .wordmark b{color:#e8734a;font-weight:800}
    .rule{width:${marquee ? 72 : 48}px;height:4px;border-radius:2px;background:#e8734a}
    .tagline{font-size:${tagline}px;font-weight:600;color:#39435c;line-height:1.28;max-width:${marquee ? 560 : 360}px}
    .chips{display:flex;gap:12px;margin-top:8px;flex-wrap:wrap}
    .chips span{font-size:15px;font-weight:600;color:#2f3a54;background:rgba(31,43,69,.06);
      border:1px solid rgba(31,43,69,.10);border-radius:999px;padding:7px 14px}
    .baboon{position:absolute;right:70px;bottom:18px;width:272px;height:auto;
      filter:drop-shadow(0 12px 22px rgba(31,43,69,.16))}
  </style></head><body>
    <img class="icon" src="${iconDataUrl}" alt="papio">
    <div class="copy">
      <div class="wordmark">papio</div>
      <div class="rule"></div>
      <div class="tagline">Scholarly PDFs — fetched, validated, and filed into your library.</div>
      ${features}
    </div>
    ${baboon}
  </body></html>`;
}

async function capturePromoTiles(browser: Browser) {
  const iconBytes = await readFile(join(extensionDir, "icons", "icon128.png"));
  const iconDataUrl = `data:image/png;base64,${iconBytes.toString("base64")}`;
  const baboonDataUrl = `data:image/svg+xml;base64,${Buffer.from(await buildBaboonSvg()).toString("base64")}`;

  const tiles: Array<{ name: string; variant: "small" | "marquee"; w: number; h: number }> = [
    { name: "promo-small", variant: "small", w: 440, h: 280 },
    { name: "promo-marquee", variant: "marquee", w: 1400, h: 560 },
  ];

  for (const t of tiles) {
    const page = await newPage(browser, t.w, t.h);
    await page.setContent(promoHtml(iconDataUrl, t.variant, baboonDataUrl), { waitUntil: "load" });
    await page.evaluate(async () => {
      // ensure the embedded icon has decoded before we snapshot
      await Promise.all(Array.from(document.images).map((i) => (i.complete ? null : i.decode().catch(() => {}))));
    });
    await Bun.sleep(150);
    const raw = (await page.screenshot({ type: "png" })) as Buffer;
    await downscale(raw, t.name, t.w, t.h, BG); // flatten -> guaranteed no alpha
    await page.close();
    console.log(`  ${t.name}.png`);
  }
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

async function ensureBuild() {
  if (process.argv.includes("--skip-build") && existsSync(join(distDir, "popup.html"))) {
    console.log("==> skipping build (--skip-build)");
    return;
  }
  console.log("==> building extension");
  const proc = Bun.spawnSync(["bun", "run", "build.ts"], { cwd: extensionDir, stdout: "inherit", stderr: "inherit" });
  if (proc.exitCode !== 0) throw new Error("extension build failed");
}

await ensureBuild();
await mkdir(outDir, { recursive: true });

const server = startServer();
const base = `http://127.0.0.1:${server.port}`;
const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  args: ["--no-sandbox", "--hide-scrollbars", "--force-color-profile=srgb"],
});

try {
  console.log(`==> rendering store assets for papio v${VERSION}`);
  await captureScreenshots(browser, base);
  await capturePromoTiles(browser);
  console.log(`==> done -> ${outDir}`);
} finally {
  await browser.close();
  server.stop(true);
}
