// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Bundles the Chrome MV3 extension into dist/ and produces a Firefox MV3
// extension root in firefox/. Bun is a build tool here only — both shipped
// artifacts are plain browser JavaScript with zero runtime dependencies.
//   dist/{history,inbox,materialize,options,page-bulk,popup,toast}.{js,html} Chrome pages
//   firefox/dist/{...same...}.{js,html} Firefox extension pages
//
// Pass --watch to rebuild on changes to src/, icons/, or manifest.json — the
// dev loop (see `bun run dev`) pairs this with `web-ext run`, which reloads the
// Firefox add-on whenever firefox/ changes. Nothing here uses WebDriver/CDP, so
// the dev browser never sets navigator.webdriver.

import { cp, mkdir, readFile, writeFile } from "node:fs/promises";
import { watch as fsWatch } from "node:fs";

const firefoxRoot = "firefox";
const firefoxDist = `${firefoxRoot}/dist`;
const buildDaemonVersion = process.env.PAPIO_DAEMON_VERSION ?? "0.0.0-dev";
// AMO manifests cannot distinguish an unpacked extension from a shipped one,
// so the developer-panel boundary is fixed before either browser bundle exists.
const captureToolsInDevBuild =
  process.env.PAPIO_CAPTURE_TOOLS === "1" ||
  (process.env.PAPIO_CAPTURE_TOOLS === undefined && buildDaemonVersion === "0.0.0-dev");
const capturePanel = /\n      <details class="capture" hidden>[\s\S]*?\n      <\/details>/;


const extensionPageNames = ["history", "inbox", "materialize", "options", "page-bulk", "popup", "toast"] as const;

async function assertExtensionPages(outdir: string): Promise<void> {
  await Promise.all(
    extensionPageNames.map(async (pageName) => {
      await readFile(`${outdir}/${pageName}.html`, "utf8");
      const bundle = await readFile(`${outdir}/${pageName}.js`, "utf8");
      if (bundle.includes("innerHTML")) {
        throw new Error(`${outdir}/${pageName}.js must not contain innerHTML; use textContent instead`);
      }
    }),
  );
  const popup = await readFile(`${outdir}/popup.html`, "utf8");
  if (popup.includes('class="capture"') !== captureToolsInDevBuild) {
    throw new Error(`${outdir}/popup.html has the wrong developer capture panel for this build`);
  }
}

async function copyExtensionPages(outdir: string): Promise<void> {
  const popup = await readFile("src/popup.html", "utf8");
  const builtPopup = captureToolsInDevBuild ? popup : popup.replace(capturePanel, "");
  if (!captureToolsInDevBuild && builtPopup === popup) {
    throw new Error("release build could not remove the developer capture panel");
  }
  await Promise.all([
    cp("src/history.html", `${outdir}/history.html`),
    cp("src/inbox.html", `${outdir}/inbox.html`),
    cp("src/materialize.html", `${outdir}/materialize.html`),
    cp("src/options.html", `${outdir}/options.html`),
    cp("src/page-bulk.html", `${outdir}/page-bulk.html`),
    cp("src/toast.html", `${outdir}/toast.html`),
    writeFile(`${outdir}/popup.html`, builtPopup),
  ]);
}


async function build(entrypoints: string[], outdir: string, format: "esm" | "iife"): Promise<number> {
  const result = await Bun.build({
    entrypoints,
    outdir,
    target: "browser",
    format,
    sourcemap: "none",
    define: {
      __PAPIO_DAEMON_VERSION__: JSON.stringify(buildDaemonVersion),
      __PAPIO_DEV_CAPTURE__: JSON.stringify(captureToolsInDevBuild),
    },
  });
  if (!result.success) {
    for (const log of result.logs) console.error(log);
    throw new Error("bundle failed");
  }
  return result.outputs.length;
}

async function buildAll(): Promise<void> {
  const chromeBundles = await build(
    ["src/background.ts", "src/history.ts", "src/inbox.ts", "src/materialize.ts", "src/options.ts", "src/page-bulk.ts", "src/popup.ts", "src/toast.ts"],
    "dist",
    "esm",
  );
  await copyExtensionPages("dist");
  await assertExtensionPages("dist");
  console.log(`built Chrome: ${chromeBundles} bundles + ${extensionPageNames.length} html shells into dist/`);

  await mkdir(firefoxDist, { recursive: true });
  const firefoxBackgroundBundles = await build(["src/background.ts"], firefoxDist, "iife");
  const firefoxPageBundles = await build(
    ["src/history.ts", "src/inbox.ts", "src/materialize.ts", "src/options.ts", "src/page-bulk.ts", "src/popup.ts", "src/toast.ts"],
    firefoxDist,
    "esm",
  );
  await Promise.all([
    copyExtensionPages(firefoxDist),
    cp("icons", `${firefoxRoot}/icons`, { recursive: true }),
  ]);
  await assertExtensionPages(firefoxDist);

  // manifest.json deliberately omits content_security_policy: MV3's default
  // extension_pages CSP already prohibits inline and remotely hosted scripts.
  const chromeManifest = JSON.parse(await readFile("manifest.json", "utf8")) as Record<string, unknown>;
  const { minimum_chrome_version: _, ...firefoxManifest } = chromeManifest;
  // tabGroups (Firefox 139+) enables the same collapsed "papio" tab-group
  // handoff on Firefox as on Chrome. Keep the permission; on Firefox < 139 the
  // API is absent and handoffSurface() degrades tab-group mode to the work
  // window at runtime, so a lower strict_min_version stays compatible.
  firefoxManifest.background = { scripts: ["dist/background.js"] };
  firefoxManifest.browser_specific_settings = {
    gecko: {
      id: "papio@orgmentem.com",
      strict_min_version: "128.0",
      // Firefox data collection consent covers sanitized page captures sent
      // from the extension to the local native app. Keep this Firefox-only;
      // manifest.json remains valid for Chrome without the declaration.
      data_collection_permissions: { required: ["websiteContent"] },
    },
  };
  await writeFile(`${firefoxRoot}/manifest.json`, `${JSON.stringify(firefoxManifest, null, 2)}\n`);

  const firefoxBackground = await readFile(`${firefoxDist}/background.js`, "utf8");
  const firefoxInbox = await readFile(`${firefoxDist}/inbox.js`, "utf8");
  const firefoxHistory = await readFile(`${firefoxDist}/history.js`, "utf8");
  if ([firefoxBackground, firefoxInbox, firefoxHistory].some((bundle) => /^export /m.test(bundle))) {
    throw new Error("Firefox background, inbox, and history bundles must not contain top-level exports");
  }
  console.log(
    `built Firefox: ${firefoxBackgroundBundles + firefoxPageBundles} bundles + ${extensionPageNames.length} html shells + icons into firefox/`,
  );
}

const watching = process.argv.includes("--watch");
const reloading = process.argv.includes("--reload");

let buildSucceeded = false;
try {
  await buildAll();
  buildSucceeded = true;
} catch (error) {
  console.error(error);
  if (!watching) process.exit(1);
}
if (buildSucceeded && reloading) {
  await requestExtensionReload();
}

/** Tell a connected development-mode extension to reload itself from disk.
 * Replaces the manual chrome://extensions Reload click: the daemon relays one
 * dev_reload command over the native-messaging connection the browser already
 * holds, and the native host delivers it within its 2s poll. Never fatal — a
 * watcher must survive a stopped daemon or a browser that is not connected. */
async function requestExtensionReload(): Promise<void> {
  try {
    const proc = Bun.spawn(["papio", "browser", "reload"], { stderr: "inherit" });
    const exitCode = await proc.exited;
    if (exitCode !== 0) {
      console.error("papio browser reload failed — is the daemon running and a development extension connected?");
    }
  } catch {
    console.error("papio browser reload spawn failed — is papio on PATH?");
  }
}

if (watching) {
  // Rebuild on source changes only; outputs (dist/, firefox/) are never watched,
  // so a rebuild cannot retrigger itself. Debounced to coalesce editor bursts.
  // One build at a time. The 150ms debounce coalesces editor bursts, but it
  // only clears the pending timer: once a build is running, the next change
  // used to start a SECOND concurrent buildAll() over the same dist/ and
  // firefox/ trees, so the outputs could interleave and --reload could then
  // load a torn bundle. Changes arriving mid-build collapse into exactly one
  // follow-up build, and the reload is skipped when another build is already
  // queued, since that bundle is about to be replaced.
  let timer: Timer | undefined;
  let building = false;
  let queued = false;
  const runBuild = async (): Promise<void> => {
    if (building) {
      queued = true;
      return;
    }
    building = true;
    try {
      await buildAll();
      if (reloading && !queued) await requestExtensionReload();
    } catch (error) {
      console.error(error);
    } finally {
      building = false;
      if (queued) {
        queued = false;
        void runBuild();
      }
    }
  };
  const schedule = (): void => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      void runBuild();
    }, 150);
  };
  for (const target of ["src", "icons"]) {
    fsWatch(target, { recursive: true }, schedule);
  }
  fsWatch("manifest.json", schedule);
  console.log(
    reloading
      ? "watching src/, icons/, manifest.json — rebuilding and reloading extension on change (Ctrl-C to stop)"
      : "watching src/, icons/, manifest.json — rebuilding on change (Ctrl-C to stop)",
  );
}
