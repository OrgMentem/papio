// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Bundles the Chrome MV3 extension into dist/ and produces a Firefox MV3
// extension root in firefox/. Bun is a build tool here only — both shipped
// artifacts are plain browser JavaScript with zero runtime dependencies.
//   dist/{history,inbox,options,popup}.{js,html} Chrome extension pages
//   firefox/dist/{history,inbox,options,popup}.{js,html} Firefox extension pages
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


const extensionPageNames = ["history", "inbox", "options", "popup"] as const;

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
    cp("src/options.html", `${outdir}/options.html`),
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
    ["src/background.ts", "src/history.ts", "src/inbox.ts", "src/options.ts", "src/popup.ts"],
    "dist",
    "esm",
  );
  await copyExtensionPages("dist");
  await assertExtensionPages("dist");
  console.log(`built Chrome: ${chromeBundles} bundles + 4 html shells into dist/`);

  await mkdir(firefoxDist, { recursive: true });
  const firefoxBackgroundBundles = await build(["src/background.ts"], firefoxDist, "iife");
  const firefoxPageBundles = await build(
    ["src/history.ts", "src/inbox.ts", "src/options.ts", "src/popup.ts"],
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
      // papio's extension has no backend and collects no data; declare that
      // explicitly. AMO requires data_collection_permissions on new add-ons.
      data_collection_permissions: { required: ["none"] },
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
    `built Firefox: ${firefoxBackgroundBundles + firefoxPageBundles} bundles + 4 html shells + icons into firefox/`,
  );
}

const watching = process.argv.includes("--watch");

try {
  await buildAll();
} catch (error) {
  console.error(error);
  if (!watching) process.exit(1);
}

if (watching) {
  // Rebuild on source changes only; outputs (dist/, firefox/) are never watched,
  // so a rebuild cannot retrigger itself. Debounced to coalesce editor bursts.
  let timer: Timer | undefined;
  const schedule = (): void => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      void buildAll().catch((error) => console.error(error));
    }, 150);
  };
  for (const target of ["src", "icons"]) {
    fsWatch(target, { recursive: true }, schedule);
  }
  fsWatch("manifest.json", schedule);
  console.log("watching src/, icons/, manifest.json — rebuilding on change (Ctrl-C to stop)");
}
