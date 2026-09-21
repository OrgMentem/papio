// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development seam: decision backends never receive OS handles or execute UI.
import { sha256 } from "./jev-trial";

export interface NativeObservation {
  goal: string;
  page: { url: string; title: string; text: string };
  controls: { id: string; role: string; label: string; disabled?: boolean }[];
  provenance: { kind: string; revision: string };
  native_surface?: "document" | "save-dialog";
}
export interface NativeDecision { choice: string; observationHash: string }
export interface DecisionBackend {
  decide(observation: NativeObservation, signal: AbortSignal): Promise<NativeDecision>;
}
export interface NativeDriver {
  observe(): Promise<NativeObservation>;
  // The driver revalidates its retained target and ownership immediately before
  // invocation. A matching hash alone cannot fence a changing native UI.
  act(choice: string, observationHash: string): Promise<{ status: "dispatched" | "stale" | "needs_foreground"; focusChanged: boolean; pointerMoved: boolean; focusWithinOwnedSurface?: boolean }>;
  artifact(): Promise<{ path: string; sha256: string; bytes: number } | null>;
  // A correlated browser transfer owns progress until completion/interruption.
  // A temporarily disabled page control is not evidence of acquisition failure.
  pendingArtifact?(): Promise<boolean>;
}
export type NativeRunResult = { status: "downloaded"; artifact: NonNullable<Awaited<ReturnType<NativeDriver["artifact"]>>>; decisions: number }
  | { status: "blocked" | "needs_foreground" | "interference" | "no_progress" | "budget_exhausted"; decisions: number };
export interface NativeLoopOptions {
  maxDecisions: number;
  noProgressMs: number;
  signal: AbortSignal;
  wait: () => Promise<void>;
  now?: () => number;
  record: (event: Record<string, unknown>) => void;
  allowOwnedFocusChanges?: boolean;
}
export const observationHash = (observation: NativeObservation) => sha256(JSON.stringify(observation));

export async function runNativeLoop(driver: NativeDriver, backend: DecisionBackend, options: NativeLoopOptions): Promise<NativeRunResult> {
  if (!Number.isSafeInteger(options.maxDecisions) || options.maxDecisions < 1 || !Number.isFinite(options.noProgressMs) || options.noProgressMs <= 0) throw new Error("Explicit positive resource budgets required");
  const now = options.now ?? performance.now.bind(performance);
  let decisions = 0, changedAt = now(), previous = "", waitingForChange: string | null = null;
  const effects = new Set<string>();
  while (true) {
    options.signal.throwIfAborted();
    const artifact = await driver.artifact();
    if (artifact) return { status: "downloaded", artifact, decisions }; // Adoption is independently checked by the caller.
    if (await driver.pendingArtifact?.()) { await options.wait(); continue; }
    const observed = await driver.observe(), hash = observationHash(observed);
    // Ephemeral handles/revisions must not masquerade as progress.
    const state = sha256(JSON.stringify({ page: observed.page, controls: observed.controls.map(({ role, label, disabled }) => ({ role, label, disabled: disabled === true })), native_surface: observed.native_surface }));
    if (state !== previous) { changedAt = now(); previous = state; }
    if (now() - changedAt >= options.noProgressMs) return { status: "no_progress", decisions };
    if (waitingForChange === state) { await options.wait(); continue; }
    waitingForChange = null;
    if (decisions >= options.maxDecisions) return { status: "budget_exhausted", decisions };
    const decision = await backend.decide(observed, options.signal);
    decisions++;
    options.signal.throwIfAborted();
    if (decision.observationHash !== hash) throw new Error("Decision belongs to another observation");
    options.record({ kind: "decision", observationHash: hash, choice: decision.choice, decisions });
    if (decision.choice === "BLOCKED") {
      // Downloads can begin during inference. Transport evidence outranks a
      // model's interpretation of the now-disabled download button.
      const completed = await driver.artifact();
      if (completed) return { status: "downloaded", artifact: completed, decisions };
      if (await driver.pendingArtifact?.()) continue;
      return { status: "blocked", decisions };
    }
    if (decision.choice === "WAIT") { waitingForChange = state; await options.wait(); continue; }
    const target = observed.controls.find(c => c.id === decision.choice && !c.disabled);
    if (!target) throw new Error("Decision selected an absent or disabled control");
    const effect = `${state}:${target.role}:${target.label}`;
    if (effects.has(effect)) { waitingForChange = state; await options.wait(); continue; }
    // A second observation catches changes during inference; the driver also
    // checks its live native target during act, closing the remaining seam.
    if (observationHash(await driver.observe()) !== hash) { options.record({ kind: "stale", observationHash: hash }); continue; }
    options.signal.throwIfAborted();
    const result = await driver.act(target.id, hash);
    options.record({ kind: "effect", ...result, observationHash: hash, choice: target.id });
    if (result.status === "needs_foreground") return { status: "needs_foreground", decisions };
    if (result.pointerMoved || (result.focusChanged && !(options.allowOwnedFocusChanges && result.focusWithinOwnedSurface))) return { status: "interference", decisions };
    if (result.status === "dispatched") { effects.add(effect); waitingForChange = state; }
    await options.wait();
  }
}
