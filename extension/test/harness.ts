// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixture harness for the declarative provider adapters. Renders a captured
// fixture HTML file (see fixtures/README.md) into a happy-dom Document so the
// production planner `planExecution` — the same function the browser injects —
// can be exercised exactly as it runs in the page.
//
// Skip-when-missing is the whole point: real provider fixtures are captured by
// the user later (Phase 3), so before any exist the suite must stay GREEN.
// loadFixture returns null when a file is absent; call sites gate on it with
// bun's test.skipIf / test.if so nothing fails for want of a capture.

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { Window } from "happy-dom";

import type { AdapterSpec, PageVerdict } from "../src/adapters/types";
import { planExecution, type ExpectedWork, type PlanPolicy, type PlanResult } from "../src/plan";

/** Repo-relative read-root for committed fixtures. Captures now travel over the
 * native-messaging bridge into the daemon's data directory; retrieve one with
 * `papio adapter captures` and commit it here as `<provider>/<scenario>.html`. */
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
 * comment node and ignored by `planExecution`, the production planner the
 * browser injects.
 */
export function parseHTML(html: string, baseURL = "https://fixture.local/"): Document {
  const window = new Window({ url: baseURL });
  window.document.write(html);
  return window.document as unknown as Document;
}

/** The scheme+host+path a capture was taken from, read back out of its
 * `papio-fixture` header. A relative `href` resolves against the document's
 * URL, so a tool that prints a resolved download URL has to parse with the
 * real origin or it reports a fixture-local address that never existed. */
export function captureOrigin(html: string): string | null {
  const header = /^<!--\s*papio-fixture\b[^>]*?\borigin="([^"]+)"/.exec(html);
  return header === null ? null : (header[1] ?? null);
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

/** The verdict `planExecution` already computed, projected out of its result.
 * Both result shapes carry it — a `Plan` and an `AssistedPlan` alike — so this
 * reads one field and classifies nothing. It never computes, infers, or
 * defaults a verdict: a result the planner did not verdict is a real failure,
 * not something to paper over. `assisted` remains the only authority
 * discriminator; a retained verdict authorizes no effect. */
export function verdictOf(result: PlanResult): PageVerdict {
  return result.verdict;
}

/** Run the production planner over a fixture Document and project its verdict.
 *
 * Synchronous `doc` path ONLY. The asynchronous live path is
 * `planExecution(null, spec, …)`, which returns a promise and reads the page's
 * global `document`; tests that drive it must call `planExecution` directly
 * rather than route it through this helper. */
export function classifyFixture(
  doc: Document,
  spec: AdapterSpec,
  expected?: ExpectedWork,
  policy?: PlanPolicy,
): PageVerdict {
  return verdictOf(planExecution(doc, spec, expected ?? {}, policy ?? {}));
}
