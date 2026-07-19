// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Bundles the Chrome MV3 extension into dist/ and produces a Firefox MV3
// extension root in firefox/. Bun is a build tool here only — both shipped
// artifacts are plain browser JavaScript with zero runtime dependencies.
//   dist/background.js                  Chrome module service worker
//   firefox/dist/background.js          Firefox classic event-page script
//   dist/{options,popup}.{js,html}      Chrome extension pages
//   firefox/dist/{options,popup}.{js,html} Firefox extension pages
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


async function build(entrypoints: string[], outdir: string, format: "esm" | "iife"): Promise<number> {
  const result = await Bun.build({
    entrypoints,
    outdir,
    target: "browser",
    format,
    sourcemap: "none",
    define: {
      __PAPIO_DAEMON_VERSION__: JSON.stringify(buildDaemonVersion),
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
    ["src/background.ts", "src/options.ts", "src/popup.ts"],
    "dist",
    "esm",
  );
  await cp("src/options.html", "dist/options.html");
  await cp("src/popup.html", "dist/popup.html");
  console.log(`built Chrome: ${chromeBundles} bundles + 2 html shells into dist/`);

  await mkdir(firefoxDist, { recursive: true });
  const firefoxBackgroundBundles = await build(["src/background.ts"], firefoxDist, "iife");
  const firefoxPageBundles = await build(["src/options.ts", "src/popup.ts"], firefoxDist, "esm");
  await Promise.all([
    cp("src/options.html", `${firefoxDist}/options.html`),
    cp("src/popup.html", `${firefoxDist}/popup.html`),
    cp("icons", `${firefoxRoot}/icons`, { recursive: true }),
  ]);

  const chromeManifest = JSON.parse(await readFile("manifest.json", "utf8")) as Record<string, unknown>;
  const { minimum_chrome_version: _, ...firefoxManifest } = chromeManifest;
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
  if (/^export /m.test(firefoxBackground)) {
    throw new Error("Firefox background bundle must be a classic script, not an ES module");
  }
  console.log(
    `built Firefox: ${firefoxBackgroundBundles + firefoxPageBundles} bundles + 2 html shells + icons into firefox/`,
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
