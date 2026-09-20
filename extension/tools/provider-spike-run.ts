// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Explicit development trial in one daemon-owned provider tab; no debugger.
import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync, readFileSync, appendFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { parseArgs } from "node:util";
import { createInterface } from "node:readline";
import { Readable } from "node:stream";
import { runTrial } from "./jev-trial";
import { runNativeLoop, observationHash, type NativeObservation, type NativeDriver } from "./native-spike-loop";
const options = Object.fromEntries(["run-dir", "extension-id", "job-id", "entry-url", "doi", "adoption-root", "monitor-helper"].map(name => [name, { type: "string" as const }]));
const allOptions: Record<string, { type: "string" }> = { ...options, "drift-evidence": { type: "string" } };
const { values } = parseArgs({ args: Bun.argv.slice(2), options: allOptions, strict: true });
for (const name of Object.keys(options)) if (!values[name]) throw new Error(`Required --${name}`);
if (!/^[a-p]{32}$/.test(values["extension-id"]!) || !/^job_[a-f0-9]{24,64}$/.test(values["job-id"]!) || !/^10\.\d{4,9}\/\S+$/.test(values.doi!)) throw new Error("Invalid experiment identity");
const entry = new URL(values["entry-url"]!);
if (entry.protocol !== "https:" || entry.username || entry.password || entry.search || entry.hash) throw new Error("Entry must be a credential-free HTTPS article URL");
// Optional private `jobs show --json` receipt for a developer-controlled failure
// experiment. It attributes the trial; it is not a production authority token.
let afterDrift = false;
if (values["drift-evidence"]) {
  const evidence = JSON.parse(readFileSync(values["drift-evidence"], "utf8"));
  if (evidence.job?.id !== values["job-id"] || evidence.job?.state !== "awaiting_human" || evidence.job?.work?.doi?.toLowerCase() !== values.doi!.toLowerCase() ||
      !evidence.events?.some((event: any) => event.kind === "browser.provider_outcome" && event.detail?.outcome === "ui_changed" && event.detail?.host === entry.hostname))
    throw new Error("A matching parked job and daemon drift outcome are required");
  afterDrift = true;
}
const dir = resolve(values["run-dir"]!), nonce = randomUUID(), origin = `chrome-extension://${values["extension-id"]}`;
mkdirSync(dir, { mode: 0o700 });
const record = (value: unknown) => appendFileSync(`${dir}/events.jsonl`, JSON.stringify({ at: new Date().toISOString(), ...value as object }) + "\n", { mode: 0o600 });
const save = (name: string, value: unknown) => writeFileSync(`${dir}/${name}.json`, JSON.stringify(value, null, 2) + "\n", { mode: 0o600, flag: "wx" });
const bundleDir = resolve(import.meta.dir, `../dist/provider-spike-${nonce}`); mkdirSync(bundleDir);
let socket: Bun.ServerWebSocket<unknown> | undefined, sequence = 0, startRequested = false;
const pending = new Map<number, { resolve: (value: any) => void; reject: (error: Error) => void }>();
let readyResolve: () => void;
const ready = new Promise<void>(resolve => { readyResolve = resolve; });
const server = Bun.serve<unknown>({ hostname: "127.0.0.1", port: 0,
  fetch(request, server) {
    const url = new URL(request.url);
    if (url.pathname === `/${nonce}/socket` && request.headers.get("origin") === origin && !socket && server.upgrade(request, { data: {} })) return;
    if (url.pathname === `/${nonce}/start` && request.method === "POST" && !request.headers.has("origin")) { startRequested = true; return Response.json({ started: true }); }
    return new Response("Not found", { status: 404 });
  }, websocket: {
    open(ws) { socket = ws; },
    message(_ws, raw) {
      const value = JSON.parse(String(raw)); if (value.ready === true) { readyResolve(); return; }
      const waiter = pending.get(value.id); if (!waiter) return; pending.delete(value.id);
      value.error ? waiter.reject(new Error(value.error)) : waiter.resolve(value.result);
    },
    close() { socket = undefined; for (const waiter of pending.values()) waiter.reject(new Error("Browser disconnected")); pending.clear(); },
  },
});
async function rpc(method: string, input: object = {}): Promise<any> {
  if (!socket) throw new Error("Browser disconnected");
  const id = ++sequence; let timer: ReturnType<typeof setTimeout>;
  try { return await new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject }); timer = setTimeout(() => { pending.delete(id); reject(new Error(`Browser request timed out: ${method}`)); }, 15000);
    socket!.send(JSON.stringify({ id, method, input }));
  }); } finally { clearTimeout(timer!); }
}
const config = { socket: `ws://127.0.0.1:${server.port}/${nonce}/socket`, jobID: values["job-id"]!, entryURL: entry.href, doi: values.doi!, afterDrift };
const built = await Bun.build({ entrypoints: [new URL("./provider-spike-browser.ts", import.meta.url).pathname], outdir: bundleDir, target: "browser", format: "esm", define: { PROVIDER_SPIKE: JSON.stringify(config) } });
if (!built.success) { server.stop(true); throw new Error(built.logs.join("\n")); }
writeFileSync(`${bundleDir}/run.html`, '<!doctype html><meta charset="utf-8"><title>Papio provider experiment</title><h1>Papio provider experiment</h1><p>One isolated job and its existing provider tab. Developer-authorized Jev experiment.</p><pre>Connecting…</pre><script type="module" src="provider-spike-browser.js"></script>');
const receipt = { url: `${origin}/dist/provider-spike-${nonce}/run.html`, startURL: `http://127.0.0.1:${server.port}/${nonce}/start`, bundleDir, jobID: config.jobID, entryURL: entry.href, pid: process.pid };
save("setup", receipt); console.log(JSON.stringify(receipt));
const abort = new AbortController(), timeout = setTimeout(() => abort.abort(new Error("Provider setup deadline elapsed")), 600000);
for (const name of ["SIGINT", "SIGTERM"] as const) process.on(name, () => abort.abort(new Error("Provider experiment cancelled")));
const aborted = new Promise<never>((_resolve, reject) => abort.signal.addEventListener("abort", () => reject(abort.signal.reason), { once: true }));
let monitor: ReturnType<typeof Bun.spawn> | undefined, lines: AsyncIterator<string> | undefined;
async function monitorLine() {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const line = await Promise.race([lines!.next(), new Promise<never>((_resolve, reject) => { timer = setTimeout(() => { monitor?.kill(); reject(new Error("Monitor timed out")); }, 5000); })]);
    if (line.done) throw new Error("Monitor ended early"); return JSON.parse(line.value);
  } finally { clearTimeout(timer); }
}
try {
  await Promise.race([ready, aborted]);
  const setup = await rpc("setup"); record({ kind: "setup", ...setup }); console.log(JSON.stringify({ ready: true, tabID: setup.tabID }));
  while (!startRequested) { abort.signal.throwIfAborted(); await Bun.sleep(100); }
  clearTimeout(timeout);
  monitor = Bun.spawn([resolve(values["monitor-helper"]!), "--passive-monitor"], { stdin: "pipe", stdout: "pipe", stderr: "ignore" });
  lines = createInterface({ input: Readable.fromWeb(monitor.stdout as ReadableStream<Uint8Array> as never), crlfDelay: Infinity })[Symbol.asyncIterator]();
  record({ kind: "monitor_start", ...await monitorLine() });
  const start = performance.now(), signal = AbortSignal.any([AbortSignal.timeout(90000), abort.signal]);
  let current: NativeObservation | undefined, count = 0;
  const driver: NativeDriver = {
    observe: async () => current = await rpc("observe"),
    pendingArtifact: async () => { const transfer = await rpc("artifact"); return transfer.pending || transfer.item?.state === "in_progress"; },
    act: async (choice, hash) => {
      if (!current || observationHash(current) !== hash) return { status: "stale", focusChanged: false, pointerMoved: false };
      const result = await rpc("act", { choice, revision: current.provenance.revision }); record({ kind: "browser_effect", ...result });
      return { status: result.status, focusChanged: false, pointerMoved: false };
    },
    artifact: async () => {
      const evidence = await rpc("artifact"), item = evidence.item;
      if (!item || item.state !== "complete") return null;
      record({ kind: "download", ...evidence });
      if (dirname(item.filename) !== resolve(values["adoption-root"]!, config.jobID)) throw new Error("Wrong adoption destination");
      const bytes = readFileSync(item.filename);
      if (bytes.length !== item.fileSize || bytes.length > 104857600 || bytes.subarray(0, 5).toString() !== "%PDF-") throw new Error("Downloaded bytes are not the completed PDF");
      return { path: item.filename, sha256: createHash("sha256").update(bytes).digest("hex"), bytes: bytes.length };
    },
  };
  const result = await runNativeLoop(driver, { decide: async (observation, signal) => {
    const path = `${dir}/snapshot-${++count}.json`; writeFileSync(path, JSON.stringify(observation), { mode: 0o600, flag: "wx" });
    const decision = await runTrial({ snapshot: path, runDir: `${dir}/calls`, maxCalls: 10 }, { key: () => {
      const key = Bun.spawnSync(["/usr/bin/security", "find-generic-password", "-s", "typesafe-api-key", "-w"], { stdout: "pipe", stderr: "pipe" });
      if (key.exitCode !== 0) throw new Error("Credential unavailable"); return key.stdout.toString().trim();
    }, fetch: (url, init) => fetch(url, { ...init, signal: AbortSignal.any([signal, init.signal!]) }) });
    return { choice: decision.choice, observationHash: observationHash(observation) };
  } }, { signal, maxDecisions: 10, noProgressMs: 15000, wait: () => Bun.sleep(250), record });
  const report = { ...result, durationMs: performance.now() - start }; save("result", report); console.log(JSON.stringify(report));
} catch (error) { save("failure", { error: error instanceof Error ? error.message : String(error) }); throw error; }
finally {
  clearTimeout(timeout);
  if (socket) {
    try { record({ kind: "final_download_evidence", ...await rpc("artifact") }); } catch {}
    try { record({ kind: "cleanup", ...await rpc("cleanup") }); } catch (error) { record({ kind: "cleanup_failed", error: String(error) }); }
  }
  if (monitor) {
    try { (monitor.stdin as import("bun").FileSink).end(); save("monitor", await monitorLine()); }
    catch (error) { record({ kind: "monitor_failed", error: String(error) }); }
    finally { monitor.kill(); }
  }
  socket?.close(); server.stop(true);
}
