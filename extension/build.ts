// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Bundles the MV3 extension into dist/. Bun is a build tool here only — the
// shipped artifact is plain browser JavaScript with zero runtime dependencies.
//   dist/background.js  service worker
//   dist/options.js + dist/options.html
//   dist/popup.js    + dist/popup.html

import { cp } from "node:fs/promises";

const result = await Bun.build({
  entrypoints: ["src/background.ts", "src/options.ts", "src/popup.ts"],
  outdir: "dist",
  target: "browser",
  format: "esm",
  sourcemap: "none",
});

if (!result.success) {
  for (const log of result.logs) console.error(log);
  process.exit(1);
}

// HTML shells reference the sibling bundles by relative path (./options.js).
await cp("src/options.html", "dist/options.html");
await cp("src/popup.html", "dist/popup.html");

console.log(`built ${result.outputs.length} bundles + 2 html shells into dist/`);
