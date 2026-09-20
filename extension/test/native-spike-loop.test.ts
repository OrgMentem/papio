// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { observationHash, runNativeLoop, type DecisionBackend, type NativeDriver, type NativeObservation } from "../tools/native-spike-loop";

const snapshot = (stage = 0): NativeObservation => ({ goal: "Acquire the main article PDF", page: { url: `http://127.0.0.1/article/${stage}`, title: "Article", text: "Full text" },
  controls: [{ id: "c1", role: "link", label: "View PDF" }], provenance: { kind: "offline", revision: String(stage) } });

function harness() {
  let stage = 0, calls = 0, effects = 0, time = 0;
  const events: Record<string, unknown>[] = [], controller = new AbortController();
  const driver: NativeDriver = {
    observe: async () => snapshot(stage),
    act: async () => { stage++; effects++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; },
    artifact: async () => stage === 2 ? { path: "/fixture.pdf", sha256: "fixture", bytes: 100 } : null,
  };
  const local: DecisionBackend = { decide: async observation => { calls++; return { choice: "c1", observationHash: observationHash(observation) }; } };
  const options = { maxDecisions: 10, noProgressMs: 100, signal: controller.signal, wait: async () => { time += 25; }, now: () => time, record: (event: Record<string, unknown>) => { events.push(event); } };
  return { driver, local, options, controller, events, calls: () => calls, effects: () => effects };
}

test("offline backend completes several native effects without credentials or network; download remains distinct from adoption", async () => {
  const h = harness();
  expect(await runNativeLoop(h.driver, h.local, h.options)).toMatchObject({ status: "downloaded", decisions: 2 });
  expect(h.effects()).toBe(2);
});

test("changing page during inference invalidates the selected native target before any effect", async () => {
  const h = harness(); let reads = 0;
  h.driver.observe = async () => snapshot(reads++);
  expect(await runNativeLoop(h.driver, h.local, { ...h.options, maxDecisions: 2 })).toMatchObject({ status: "budget_exhausted" });
  expect(h.effects()).toBe(0);
});

test("cancellation during inference prevents the effect", async () => {
  const h = harness();
  h.local.decide = async observation => { h.controller.abort(); return { choice: "c1", observationHash: observationHash(observation) }; };
  await expect(runNativeLoop(h.driver, h.local, h.options)).rejects.toThrow();
  expect(h.effects()).toBe(0);
});

test("an unresolved effect is not replayed and unchanged loading state does not spend more model calls", async () => {
  const h = harness(); let dispatched = 0;
  h.driver.act = async () => { dispatched++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; };
  expect(await runNativeLoop(h.driver, h.local, h.options)).toMatchObject({ status: "no_progress", decisions: 1 });
  expect(dispatched).toBe(1);
  expect(h.calls()).toBe(1);
});

test("model-selected unknown or stale decisions never execute", async () => {
  for (const decision of [{ choice: "invented", observationHash: observationHash(snapshot()) }, { choice: "c1", observationHash: "stale" }]) {
    const h = harness(); h.local.decide = async () => decision;
    await expect(runNativeLoop(h.driver, h.local, h.options)).rejects.toThrow();
    expect(h.effects()).toBe(0);
  }
});

test("quiet mode stops immediately when a native action needs foreground or changes focus/pointer", async () => {
  for (const result of [
    { status: "needs_foreground" as const, focusChanged: false, pointerMoved: false },
    { status: "dispatched" as const, focusChanged: true, pointerMoved: false },
    { status: "dispatched" as const, focusChanged: false, pointerMoved: true },
  ]) {
    const h = harness(); h.driver.act = async () => result;
    expect(await runNativeLoop(h.driver, h.local, h.options)).toMatchObject({ status: result.status === "needs_foreground" ? "needs_foreground" : "interference", decisions: 1 });
  }
});

test("foreground-owned mode permits its own Save dialog, but never unrelated focus or pointer movement", async () => {
  for (const owned of [true, false]) {
    const h = harness();
    const act = h.driver.act;
    h.driver.act = async (...args) => ({ ...await act(...args), focusChanged: true, focusWithinOwnedSurface: owned });
    expect(await runNativeLoop(h.driver, h.local, { ...h.options, allowOwnedFocusChanges: true })).toMatchObject({ status: owned ? "downloaded" : "interference" });
  }
});
