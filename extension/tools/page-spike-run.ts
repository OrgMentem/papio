// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Explicit development runner. One owned inactive fixture tab, existing extension
// APIs only. No daemon, provider, debugger, runtime permissions or global input.
import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync, readFileSync, existsSync, appendFileSync } from "node:fs";
import { basename, resolve } from "node:path";
import { parseArgs } from "node:util";
import { createInterface } from "node:readline";
import { Readable } from "node:stream";
import { runTrial } from "./jev-trial";
import { runNativeLoop, observationHash, type NativeObservation, type NativeDriver } from "./native-spike-loop";
const { values } = parseArgs({ args: Bun.argv.slice(2), options: { "run-dir": { type: "string" }, "extension-id": { type: "string" }, "monitor-helper": { type: "string" }, backend: { type: "string", default: "local-fixture" } }, strict: true });
if (!values["run-dir"] || !/^[a-p]{32}$/.test(values["extension-id"] ?? "") || !["local-fixture", "jev"].includes(values.backend!)) throw new Error("Required --run-dir NEW_DIR --extension-id ID [--backend local-fixture|jev]");
const dir = resolve(values["run-dir"]), nonce = randomUUID(), origin = `chrome-extension://${values["extension-id"]}`;
mkdirSync(dir, { mode: 0o700 });
const record = (value: unknown) => appendFileSync(`${dir}/events.jsonl`, JSON.stringify({ at: new Date().toISOString(), ...value as object }) + "\n", { mode: 0o600 });
const bundleDir = resolve(import.meta.dir, `../dist/page-spike-${nonce}`), filename = `papio-page-spike-${nonce}.pdf`;
mkdirSync(`${bundleDir}/fixture`, { recursive: true });
const sourcePDF = readFileSync(new URL("../../internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf", import.meta.url));
const digest = createHash("sha256").update(sourcePDF).digest("hex");
let socket: Bun.ServerWebSocket<unknown> | undefined, sequence = 0, startRequested = false;
const pending = new Map<number, { resolve: (value: any) => void; reject: (error: Error) => void }>();
let readyResolve: () => void;
const ready = new Promise<void>(resolve => { readyResolve = resolve; });
const server = Bun.serve<unknown>({ hostname: "127.0.0.1", port: 0,
  fetch(request, server) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname.startsWith(`/${nonce}/fixture/`)) {
      const name = url.pathname.slice(`/${nonce}/fixture/`.length);
      if (["start.html", "article.html", "wrong.html", "paper.pdf", "article.js"].includes(name)) return new Response(Bun.file(`${bundleDir}/fixture/${name}`));
    }
    if (url.pathname === `/${nonce}/socket` && request.headers.get("origin") === origin && !socket && server.upgrade(request, { data: {} })) return;
    if (url.pathname === `/${nonce}/start` && request.method === "POST" && !request.headers.has("origin")) { startRequested = true; return Response.json({ started: true }); }
    return new Response("Not found", { status: 404 });
  }, websocket: {
    open(ws) { socket = ws; },
    message(_ws, raw) {
      const value = JSON.parse(String(raw));
      if (value.ready === true) { readyResolve(); return; }
      const waiter = pending.get(value.id); if (!waiter) return;
      pending.delete(value.id);
      value.error ? waiter.reject(new Error(value.error)) : waiter.resolve(value.result);
    },
    close() { socket = undefined; for (const waiter of pending.values()) waiter.reject(new Error("Browser disconnected")); pending.clear(); },
  },
});
async function rpc(method: string, input: object = {}): Promise<any> {
  if (!socket) throw new Error("Browser disconnected");
  const id = ++sequence;
  let timer: ReturnType<typeof setTimeout>;
  try { return await new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    timer = setTimeout(() => { pending.delete(id); reject(new Error(`Browser request timed out: ${method}`)); }, 15000);
    socket!.send(JSON.stringify({ id, method, input }));
  }); } finally { clearTimeout(timer!); }
}
const built = await Bun.build({ entrypoints: [new URL("./page-spike-browser.ts", import.meta.url).pathname], outdir: bundleDir, target: "browser", format: "esm", define: { SPIKE_SOCKET: JSON.stringify(`ws://127.0.0.1:${server.port}/${nonce}/socket`), SPIKE_PREFIX: JSON.stringify(`http://127.0.0.1:${server.port}/${nonce}/fixture`) } });
if (!built.success) throw new Error(built.logs.join("\n"));
writeFileSync(`${bundleDir}/run.html`, '<!doctype html><meta charset="utf-8"><title>Papio background spike</title><h1>Papio background spike</h1><p>Local synthetic fixture; one owned background tab. No provider or job changes.</p><pre>Connecting…</pre><script type="module" src="page-spike-browser.js"></script>');
const shell = (title: string, content: string) => `<!doctype html><meta charset="utf-8"><title>${title}</title><style>body{font:20px system-ui;max-width:800px;margin:40px auto}a,button{display:block;margin:24px}</style><main><h1>${title}</h1>${content}</main><aside><h2>References</h2><a href="wrong.html">Related reference PDF</a></aside>`;
writeFileSync(`${bundleDir}/fixture/start.html`, shell("Native acquisition experiment", '<p>Find the main article and download its PDF.</p><a href="article.html">Read the main article</a>'));
writeFileSync(`${bundleDir}/fixture/article.html`, shell("The main article", '<p>Full text available.</p><button id="options">Show download options</button><section id="download" hidden><h2>Article access</h2><a href="paper.pdf" type="application/pdf">Download article PDF</a></section><script src="article.js"></script>'));
writeFileSync(`${bundleDir}/fixture/wrong.html`, shell("Wrong reference", '<p>This is not the requested paper.</p>'));
writeFileSync(`${bundleDir}/fixture/paper.pdf`, sourcePDF);
writeFileSync(`${bundleDir}/fixture/article.js`, 'document.querySelector("#options").addEventListener("click",()=>{document.querySelector("#download").hidden=false;document.querySelector("#options").hidden=true;});');
const receipt = { url: `${origin}/dist/page-spike-${nonce}/run.html`, startURL: `http://127.0.0.1:${server.port}/${nonce}/start`, bundleDir, filename, sha256: digest, bytes: sourcePDF.length, pid: process.pid };
writeFileSync(`${dir}/fixture.json`, JSON.stringify(receipt, null, 2) + "\n", { mode: 0o600 });
console.log(JSON.stringify(receipt));
const abort = new AbortController();
const timeout = setTimeout(() => abort.abort(new Error("Fixture connection/start deadline elapsed")), 300000);
for (const name of ["SIGINT", "SIGTERM"] as const) process.on(name, () => abort.abort(new Error("Fixture cancelled")));
const aborted = new Promise<never>((_resolve, reject) => abort.signal.addEventListener("abort", () => reject(abort.signal.reason), { once: true }));
let created = false;
let monitor: ReturnType<typeof Bun.spawn> | undefined;
let monitorLines: AsyncIterator<string> | undefined;
async function monitorLine() {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const line = await Promise.race([monitorLines!.next(), new Promise<never>((_resolve, reject) => { timer = setTimeout(() => { monitor?.kill(); reject(new Error("Monitor timed out")); }, 5000); })]);
    if (line.done) throw new Error("Monitor exited without a report"); return JSON.parse(line.value);
  } finally { clearTimeout(timer); }
}
try {
  await Promise.race([ready, aborted]);
  const setup = await rpc("setup"); created = true; record({ kind: "setup", ...setup });
  console.log(JSON.stringify({ status: "ready", tabID: setup.tabID }));
  while (!startRequested) { abort.signal.throwIfAborted(); await Bun.sleep(100); }
  clearTimeout(timeout);
  if (values["monitor-helper"]) {
    monitor = Bun.spawn([resolve(values["monitor-helper"]), "--passive-monitor"], { stdin: "pipe", stdout: "pipe", stderr: "ignore" });
    monitorLines = createInterface({ input: Readable.fromWeb(monitor.stdout as ReadableStream<Uint8Array> as never), crlfDelay: Infinity })[Symbol.asyncIterator]();
    record({ kind: "monitor_start", ...await monitorLine() });
  }
  const start = performance.now(), signal = AbortSignal.any([AbortSignal.timeout(90000), abort.signal]);
  let current: NativeObservation | undefined, count = 0;
  const driver: NativeDriver = {
    observe: async () => current = await rpc("observe"),
    act: async (choice, hash) => {
      if (!current || observationHash(current) !== hash) return { status: "stale", focusChanged: false, pointerMoved: false };
      const result = await rpc("act", { choice, revision: current.provenance.revision, filename });
      record({ kind: "browser_effect", ...result });
      // The separate passive native monitor measures OS interference; these
      // fields only state that this driver issues no focus/pointer operations.
      return { status: result.status, focusChanged: false, pointerMoved: false };
    },
    artifact: async () => {
      const item = await rpc("artifact");
      if (!item || item.state !== "complete") return null;
      record({ kind: "download_event", ...item });
      if (basename(item.filename) !== filename || !existsSync(item.filename)) throw new Error("Download destination mismatch");
      const bytes = readFileSync(item.filename), actual = createHash("sha256").update(bytes).digest("hex");
      if (actual !== digest || bytes.length !== sourcePDF.length) throw new Error("Downloaded artifact differs from fixture");
      return { path: item.filename, sha256: actual, bytes: bytes.length };
    },
  };
  const result = await runNativeLoop(driver, { decide: async (observation, signal) => {
    count++;
    if (values.backend === "local-fixture") return { choice: observation.controls.find(c => /Read the main article|Show download options|Download article PDF/.test(c.label))?.id ?? "BLOCKED", observationHash: observationHash(observation) };
    // Model input substitutes a synthetic web URL for the local loopback
    // address. Preserve both hashes so this projection is explicit in receipts.
    const snapshot = { ...observation, page: { ...observation.page, url: `https://papio-fixture.invalid/${new URL(observation.page.url).pathname.split("/").pop()}` } };
    const path = `${dir}/snapshot-${count}.json`; writeFileSync(path, JSON.stringify(snapshot), { mode: 0o600, flag: "wx" });
    const decision = await runTrial({ snapshot: path, runDir: `${dir}/calls`, maxCalls: 10 }, { key: () => {
      const key = Bun.spawnSync(["/usr/bin/security", "find-generic-password", "-s", "typesafe-api-key", "-w"], { stdout: "pipe", stderr: "pipe" });
      if (key.exitCode !== 0) throw new Error("Credential unavailable"); return key.stdout.toString().trim();
    }, fetch: (url, init) => fetch(url, { ...init, signal: AbortSignal.any([signal, init.signal!]) }) });
    record({ kind: "model_projection", originalHash: observationHash(observation), snapshotHash: decision.snapshotHash });
    return { choice: decision.choice, observationHash: observationHash(observation) };
  } }, { signal, maxDecisions: 10, noProgressMs: 15000, wait: () => Bun.sleep(200), record });
  const report = { ...result, backend: values.backend, durationMs: performance.now() - start };
  writeFileSync(`${dir}/result.json`, JSON.stringify(report, null, 2) + "\n", { mode: 0o600 }); console.log(JSON.stringify(report));
} catch (error) {
  writeFileSync(`${dir}/failure.json`, JSON.stringify({ error: error instanceof Error ? error.message : String(error) }) + "\n", { mode: 0o600 }); throw error;
} finally {
  clearTimeout(timeout);
  if (created && socket) { try { record({ kind: "cleanup", ...await rpc("cleanup") }); } catch (error) { record({ kind: "cleanup_failed", error: String(error) }); } }
  if (monitor) {
    try {
      (monitor.stdin as import("bun").FileSink).end();
      const report = await monitorLine();
      writeFileSync(`${dir}/monitor.json`, JSON.stringify(report, null, 2) + "\n", { mode: 0o600, flag: "wx" }); record({ kind: "monitor_stop", ...report });
    } catch (error) { record({ kind: "monitor_failed", error: String(error) }); }
    finally { monitor.kill(); }
  }
  socket?.close(); server.stop(true);
}
