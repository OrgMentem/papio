// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Re-run committed fixtures through the *current* sanitizeFixture pipeline,
// preserving each file's provenance header (provider, scenario, origin,
// captured). Needed whenever the sanitizer is hardened after the original raw
// captures are gone: the committed fixture is the only surviving input.
//
//   bun run scripts/resanitize-fixtures.ts fixtures/*/*.html
//
// Same fail-closed rule as capture: a file whose output still trips
// residualLeak is left untouched and the run exits non-zero.

import { readFileSync, writeFileSync } from "node:fs";

import { residualLeak, sanitizeFixture } from "../src/capture";
import type { FixtureMeta } from "../src/capture";

const HEADER_RE =
  /^<!-- papio-fixture provider="([^"]+)" scenario="([^"]+)" origin="([^"]+)" captured="([^"]+)" -->\n?/;

let failed = 0;
for (const path of process.argv.slice(2)) {
  const input = readFileSync(path, "utf8");
  const header = HEADER_RE.exec(input);
  if (!header) {
    console.error(`${path}: no provenance header; skipping`);
    failed += 1;
    continue;
  }
  const [, provider, scenario, origin, captured] = header;
  const meta = {
    provider,
    scenario,
    originNoQuery: origin,
    capturedISO: captured,
  } as FixtureMeta;
  const sanitized = sanitizeFixture(input.slice(header[0].length), meta);
  const leak = residualLeak(sanitized);
  if (leak !== null) {
    console.error(`${path}: refusing to write (${leak})`);
    failed += 1;
    continue;
  }
  if (sanitized === input) {
    console.log(`${path}: unchanged`);
    continue;
  }
  writeFileSync(path, sanitized);
  console.log(`${path}: rewritten (${sanitized.length} bytes)`);
}
process.exit(failed > 0 ? 1 : 0);
