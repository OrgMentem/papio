// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only independent native runner. Requires an already-open local
// fixture tab. Never activates Chrome or falls back to pointer/keyboard input.
import { createHash } from "node:crypto";
import { appendFileSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { createInterface } from "node:readline";
import { Readable } from "node:stream";
import { resolve } from "node:path";
import { homedir } from "node:os";
import { parseArgs } from "node:util";
import { runTrial } from "./jev-trial";
import { observationHash, runNativeLoop, type DecisionBackend, type NativeDriver, type NativeObservation } from "./native-spike-loop";

const { values } = parseArgs({ args: Bun.argv.slice(2), options: {
  helper: { type: "string" }, fixture: { type: "string" }, "run-dir": { type: "string" },
  backend: { type: "string", default: "local-fixture" }, "max-decisions": { type: "string", default: "20" },
  delivery: { type: "string", default: "ax" },
  attention: { type: "string", default: "background" },
  "no-progress-seconds": { type: "string", default: "30" },
  "deadline-seconds": { type: "string", default: "300" }, inspect: { type: "boolean", default: false },
}, strict: true });
if (!values.helper || !values.fixture || !values["run-dir"] || !["local-fixture", "jev"].includes(values.backend!)) throw new Error("Required: --helper PATH --fixture FILE --run-dir NEW_PRIVATE_DIR [--backend local-fixture|jev]");
const dir = resolve(values["run-dir"]), fixture = JSON.parse(readFileSync(values.fixture, "utf8"));
const fixtureURL = new URL(fixture.url);
if (fixtureURL.hostname !== "127.0.0.1" || fixtureURL.protocol !== "http:" || !/^papio-native-[a-f0-9-]+\.pdf$/.test(fixture.filename)) throw new Error("Only the local native fixture is supported");
if (existsSync(resolve(homedir(), "Downloads", fixture.filename))) throw new Error("Fixture file already exists; use a fresh fixture to prove a new download");
if (!["background", "owned"].includes(values.attention!)) throw new Error("Invalid attention mode");
mkdirSync(dir, { mode: 0o700 });
const events = `${dir}/events.jsonl`;
writeFileSync(events, "", { mode: 0o600, flag: "wx" });
const record = (event: Record<string, unknown>) => appendFileSync(events, JSON.stringify({ at: new Date().toISOString(), ...event }) + "\n");
const proc = Bun.spawn([resolve(values.helper)], { stdin: "pipe", stdout: "pipe", stderr: Bun.file(`${dir}/helper-stderr.txt`) });
const lines = createInterface({ input: Readable.fromWeb(proc.stdout as never), crlfDelay: Infinity })[Symbol.asyncIterator]();
const deadline = AbortSignal.timeout(Number(values["deadline-seconds"]) * 1000);
deadline.addEventListener("abort", () => proc.kill());
async function rpc(input: Record<string, unknown>): Promise<any> {
  deadline.throwIfAborted();
  proc.stdin.write(JSON.stringify(input) + "\n"); await proc.stdin.flush();
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const line = await Promise.race([lines.next(), new Promise<never>((_, reject) => { timer = setTimeout(() => { proc.kill(); reject(new Error("Native helper timed out")); }, 15_000); })]);
    if (line.done) throw new Error("Native helper exited");
    const response = JSON.parse(line.value);
    if (!response.ok) throw new Error(`Native helper: ${response.error}`);
    return response.result;
  } finally { clearTimeout(timer); }
}
let monitorRunning = false;
let current: NativeObservation | undefined;
const driver: NativeDriver = {
  observe: async () => {
    current = await rpc({ method: "observe" }) as NativeObservation;
    return current;
  },
  act: async (choice, hash) => {
    if (!current || observationHash(current) !== hash) return { status: "stale", focusChanged: false, pointerMoved: false };
    const result = await rpc({ method: "act", choice, revision: current.provenance.revision });
    record({ kind: "native_delivery", ...result });
    return result;
  },
  artifact: async () => {
    const path = resolve(homedir(), "Downloads", fixture.filename);
    try {
      const stat = statSync(path);
      if (!stat.isFile() || stat.size !== fixture.bytes) return null;
      const hash = createHash("sha256").update(readFileSync(path)).digest("hex");
      return hash === fixture.sha256 ? { path, sha256: hash, bytes: stat.size } : null;
    } catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return null; throw error; }
  },
};
let count = 0;
const backend: DecisionBackend = {
  decide: async (observation, signal) => {
    signal.throwIfAborted();
    count++;
    writeFileSync(`${dir}/snapshot-${count}.json`, JSON.stringify(observation) + "\n", { mode: 0o600, flag: "wx" });
    if (values.backend === "local-fixture") {
      // Deterministic fixture oracle proves substitution, not model quality.
      const target = observation.controls.find(c => !c.disabled && /^(Read the main article|View article PDF|Download|Save)$/.test(c.label));
      return { choice: target?.id ?? "BLOCKED", observationHash: observationHash(observation) };
    }
    const result = await runTrial({ snapshot: `${dir}/snapshot-${count}.json`, runDir: `${dir}/calls`, maxCalls: Number(values["max-decisions"]) }, {
      key: () => {
        const value = Bun.spawnSync(["/usr/bin/security", "find-generic-password", "-s", "typesafe-api-key", "-w"], { stdout: "pipe", stderr: "pipe" });
        if (value.exitCode !== 0) throw new Error("Credential unavailable");
        return value.stdout.toString().trim();
      },
      fetch: (url, init) => fetch(url, { ...init, signal: AbortSignal.any([signal, init.signal!]) }),
    });
    return { choice: result.choice, observationHash: result.snapshotHash };
  },
};
try {
  const baseline = await rpc({ method: "configure", prefix: `${fixtureURL.origin}${fixture.prefix}`, delivery: values.delivery, attention: values.attention, goal: "Open the main article PDF in the browser viewer, then use Download and Save to save it to disk. Related reference PDFs are not the main article." });
  record({ kind: "baseline", ...baseline, backend: values.backend });
  record({ kind: "monitor_start", ...await rpc({ method: "start_monitor" }) }); monitorRunning = true;
  if (values.inspect) {
    const observation = await driver.observe();
    writeFileSync(`${dir}/observation.json`, JSON.stringify(observation, null, 2) + "\n", { mode: 0o600, flag: "wx" });
    console.log(JSON.stringify({ baseline, controls: observation.controls, page: observation.page }));
  } else {
    let result = await runNativeLoop(driver, backend, { maxDecisions: Number(values["max-decisions"]), noProgressMs: Number(values["no-progress-seconds"]) * 1000,
      signal: deadline, wait: () => new Promise(resolve => setTimeout(resolve, 500)), record, allowOwnedFocusChanges: values.attention === "owned" });
    const final = await rpc({ method: "status" });
    const monitor = await rpc({ method: "stop_monitor" }); monitorRunning = false;
    writeFileSync(`${dir}/monitor.json`, JSON.stringify(monitor, null, 2) + "\n", { mode: 0o600, flag: "wx" });
    record({ kind: "monitor_stop", ...monitor });
    // Also check settling/last-download time, after the action-level samples.
    const pointerChanged = !baseline.pointerObserved || !final.pointerObserved || baseline.pointerX !== final.pointerX || baseline.pointerY !== final.pointerY;
    const appChanged = !baseline.focusObserved || !final.focusObserved || baseline.frontPID !== final.frontPID;
    if (pointerChanged || appChanged || monitor.changeCounts.pointer > 0 || monitor.changeCounts.app > 0 ||
        (values.attention === "background" && (baseline.frontWindow !== final.frontWindow || monitor.changeCounts.window > 0))) {
      record({ kind: "attention_changed_by_end", baseline, final, downloadResult: result });
      result = { status: "interference", decisions: result.decisions };
    }
    writeFileSync(`${dir}/result.json`, JSON.stringify({ ...result, baseline, final, backend: values.backend }, null, 2) + "\n", { mode: 0o600, flag: "wx" });
    console.log(JSON.stringify({ ...result, baseline, final, backend: values.backend }));
  }
} catch (error) {
  writeFileSync(`${dir}/failure.json`, JSON.stringify({ status: "failed", error: error instanceof Error ? error.message : "Native run failed" }) + "\n", { mode: 0o600, flag: "wx" });
  throw error;
} finally {
  if (monitorRunning && !deadline.aborted) {
    try { writeFileSync(`${dir}/monitor.json`, JSON.stringify(await rpc({ method: "stop_monitor" }), null, 2) + "\n", { mode: 0o600, flag: "wx" }); }
    catch { /* Failure/deadline remains primary; absence of the report is not quietness. */ }
  }
  proc.stdin.end(); proc.kill();
}
