// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// CLI fixture sanitizer: the same sanitizeFixture pipeline the popup capture
// tool uses, runnable over HTML fetched by any means (curl, browser save,
// agent capture). Exists because the popup capture tool is Chrome-only and
// fixture capture must also work from raw page HTML during adapter
// development.
//
//   bun run scripts/sanitize-fixture.ts <provider> <scenario> <url> <in.html> <out.html>
//
// Refuses to write when the sanitized output still carries a token-shaped
// residue (same fail-closed rule as the popup).

import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

import { residualLeak, sanitizeFixture } from "../src/capture";
import type { FixtureMeta } from "../src/capture";

const [provider, scenario, rawURL, inPath, outPath] = process.argv.slice(2);
if (!provider || !scenario || !rawURL || !inPath || !outPath) {
  console.error("usage: sanitize-fixture.ts <provider> <scenario> <url> <in.html> <out.html>");
  process.exit(2);
}

const url = new URL(rawURL);
const meta = {
  provider,
  scenario,
  originNoQuery: `${url.origin}${url.pathname}`,
  capturedISO: new Date().toISOString(),
} as FixtureMeta;

const sanitized = sanitizeFixture(readFileSync(inPath, "utf8"), meta);
const leak = residualLeak(sanitized);
if (leak !== null) {
  console.error(`refusing to write fixture: residual secret-shaped content (${leak})`);
  process.exit(1);
}
mkdirSync(dirname(outPath), { recursive: true });
writeFileSync(outPath, sanitized);
console.log(`wrote ${outPath} (${sanitized.length} bytes)`);
