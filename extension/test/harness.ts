// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture harness for the declarative provider adapters. Renders a captured
// fixture HTML file (see fixtures/README.md) into a happy-dom Document so the
// pure `interpret` function can be exercised exactly as it runs in the page.
//
// Skip-when-missing is the whole point: real provider fixtures are captured by
// the user later (Phase 3), so before any exist the suite must stay GREEN.
// loadFixture returns null when a file is absent; call sites gate on it with
// bun's test.skipIf / test.if so nothing fails for want of a capture.

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { Window } from "happy-dom";

/** Repo-relative read-root for captured fixtures. Capture downloads land in the
 * browser's Downloads folder under `papio-fixtures/`; the user moves the
 * sanitized files here under `<provider>/<scenario>.html`. */
export const FIXTURE_ROOT = join(import.meta.dir, "..", "fixtures");

export function fixturePath(provider: string, scenario: string): string {
  return join(FIXTURE_ROOT, provider, `${scenario}.html`);
}

export function fixtureExists(provider: string, scenario: string): boolean {
  return existsSync(fixturePath(provider, scenario));
}

/**
 * Parse a full HTML document string into a happy-dom Document. The captured
 * fixture's `<!-- papio-fixture ... -->` header comment is parsed as a harmless
 * comment node and ignored by `interpret`.
 */
export function parseHTML(html: string): Document {
  const window = new Window({ url: "https://fixture.local/" });
  window.document.write(html);
  return window.document as unknown as Document;
}

/**
 * Load a captured fixture into a Document, or return null when the fixture file
 * is absent so suites can skip cleanly before capture.
 */
export function loadFixture(provider: string, scenario: string): Document | null {
  const path = fixturePath(provider, scenario);
  if (!existsSync(path)) return null;
  return parseHTML(readFileSync(path, "utf8"));
}
