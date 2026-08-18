// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Formalizes the two MV3 restart shapes background.test.ts exercises ad hoc
// throughout: a worker restart (SW death — durable storage AND every
// browser-side fake survive; only the in-memory Bridge and its native port
// are gone) and an extension update (chrome.storage.session, the durable job
// ledger, is wiped; local storage and the browser-side fakes survive). Both
// hand back the SAME fakes with a fresh Bridge, so the caller starts it and
// re-sends hello exactly as it would after a real restart.

import { expect } from "bun:test";

import { Bridge, type BridgeDeps } from "../src/background";
import { emptyStore, type StoreShape } from "../src/state";

/** The minimal shape either helper needs from a test harness. Structural,
 * not imported from background.test.ts's own (unexported) Harness type:
 * every concrete harness there satisfies this by construction. `bridge` and
 * `port` are typed `unknown` here deliberately — this module only ever
 * overwrites them, never reads their harness-specific shape. */
export interface RestartableHarness {
  readonly deps: BridgeDeps;
  readonly backend: { store: StoreShape };
  bridge: unknown;
  readonly port: unknown;
  /** The harness's live native-port list. A restart pushes a fresh port here
   * only once the caller awaits the returned bridge's `start()`; the `port`
   * accessor installed below reads whatever is latest at access time. */
  readonly ports: readonly unknown[];
}

/** Redefine `port` on a harness clone as a live accessor instead of a value
 * copied at clone time: the fresh port does not exist until the caller
 * awaits `bridge.start()`, which both restart helpers deliberately leave to
 * the caller (starting it here would race the caller's own pre-start setup,
 * e.g. installing a durable-ledger fixture before hydration reads it). */
function installLivePortAccessor<H extends RestartableHarness>(
  next: H,
  ports: H["ports"],
): void {
  Object.defineProperty(next, "port", {
    enumerable: true,
    configurable: true,
    get: () => {
      const latest = ports[ports.length - 1];
      if (latest === undefined) {
        throw new Error(
          "lifecycle harness has no port yet — await bridge.start() before reading .port",
        );
      }
      return latest;
    },
  });
}

/** Swap in a fresh Bridge over the harness's existing deps/fakes and backend
 * store: an MV3 service-worker death. Durable storage (chrome.storage.local
 * AND session) and every browser-side fake (tabs, windows, downloads) are
 * untouched — only the dead worker's in-memory Bridge and native port are
 * gone. The caller awaits `bridge.start()` and sends hello on the returned
 * harness's `port`. */
export function restartWorker<H extends RestartableHarness>(h: H): H {
  const next: H = { ...h };
  next.bridge = new Bridge(h.deps);
  installLivePortAccessor(next, h.ports);
  return next;
}

/** Swap in a fresh Bridge with chrome.storage.session (the durable job
 * ledger this Bridge hydrates from) wiped, modelling an extension update:
 * browser-side state (tabs, windows, the durable managed-tab ledger in
 * chrome.storage.local) survives, but every in-memory job the worker was
 * tracking does not. The caller awaits `bridge.start()` and sends hello on
 * the returned harness's `port`. */
export function simulateExtensionUpdate<H extends RestartableHarness>(
  h: H,
): H {
  h.backend.store = emptyStore();
  const next: H = { ...h };
  next.bridge = new Bridge(h.deps);
  installLivePortAccessor(next, h.ports);
  return next;
}

/** Assert a durable ledger entry for `tabID` was retained (surface-
 * lifecycle-plan.md's "absence retains" invariant: closing requires a
 * positive daemon authorization, never mere absence from a ledger). */
export function expectLedgerRetains(
  ledger: Record<string, unknown>,
  tabID: number,
): void {
  expect(ledger[String(tabID)]).toBeDefined();
}
