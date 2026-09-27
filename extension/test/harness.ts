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

import { afterEach, beforeEach } from "bun:test";
import { Window } from "happy-dom";

import { hermeticWindowSettings } from "./hermetic-dom";

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
 * Windows opened by `parseHTML` that the current test has not closed itself.
 * Fixture documents must stay alive for the whole test, so each window is
 * aborted and closed in `afterEach` — never during the test. Documents built
 * at module scope (shared across tests in one file) predate the first
 * `beforeEach` seal and are left alone.
 */
const openWindows: Window[] = [];
let sealedWindows = 0;
function closeOwnedWindows(): Promise<void> {
  const owned = openWindows.splice(sealedWindows);
  sealedWindows = openWindows.length;
  return (async () => {
    for (const win of owned) {
      try {
        await win.happyDOM.abort();
      } catch {
        // The test already aborted or closed this window; closing is idempotent.
      }
      try {
        await win.happyDOM.close();
      } catch {
        // Same as above: a test-owned close must not fail the suite.
      }
    }
  })();
}
try {
  beforeEach(() => {
    sealedWindows = openWindows.length;
  });
  afterEach(() => closeOwnedWindows());
} catch {
  // Imported outside `bun test` (the adapter tools under `bun run`): no
  // per-test lifecycle exists, so windows stay open until the process exits.
}

/** Close every window the current test opened via `parseHTML`. */
export function closeHarnessWindows(): Promise<void> {
  return closeOwnedWindows();
}

/** URLs the refusing fetch interceptor has blocked since the last drain. */
export { drainRefusedRequests } from "./hermetic-dom";

/**
 * Parse a full HTML document string into a happy-dom Document. The captured
 * fixture's `<!-- papio-fixture ... -->` header comment is parsed as a harmless
 * comment node and ignored by `planExecution`, the production planner the
 * browser injects.
 *
 * Hermetic: happy-dom's stylesheet/script/iframe loaders are off and a
 * refusing fetch interceptor blocks the rest (./hermetic-dom.ts), so parsing
 * a fixture with its real capture origin never touches the network. The
 * settings are passed explicitly, not left to the test preload, because the
 * adapter tools import this module under `bun run`. Refused URLs are counted
 * in `drainRefusedRequests`. Under `bun test` the window is aborted and
 * closed in `afterEach`.
 */
export function parseHTML(html: string, baseURL = "https://fixture.local/"): Document {
  const window = new Window({
    url: baseURL,
    settings: { ...hermeticWindowSettings, disableJavaScriptEvaluation: true },
  });
  openWindows.push(window);
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
