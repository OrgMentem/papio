// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { startNativeFixture, type NativeFixtureReceipt } from "../tools/native-spike-fixture";
import { assertResidentViewer, auditResidentRequests, finishResidentProof, nativeBrowser, prepareResidentPDF, readFixtureRequests, requireResidentOptions, residentNativeSession, validateResidentFixture, type NativeBrowser } from "../tools/native-resident-proof";
import { observationHash, runNativeLoop, type NativeDriver, type NativeObservation } from "../tools/native-spike-loop";

const cleanups: (() => void)[] = [];
afterEach(() => { for (const cleanup of cleanups.splice(0).reverse()) cleanup(); });
function fixture() {
  const root = mkdtempSync(join(tmpdir(), "papio-resident-test-"));
  cleanups.push(() => rmSync(root, { recursive: true, force: true }));
  const server = startNativeFixture(join(root, "fixture"));
  cleanups.push(() => { server.stop(); });
  return { ...server, root, readRequests: () => readFixtureRequests(server.requestsPath) };
}
const viewer = (f: NativeFixtureReceipt): NativeObservation => ({ goal: "Save resident PDF", page: { url: f.pdfURL, title: f.filename, text: "Finished loading PDF" },
  controls: [{ id: "c9", role: "AXButton", label: "Download" }], provenance: { kind: "native-axorcist", revision: "loaded" } });
// Text and control labels observed through Firefox 156 native AX for the real
// synthetic corpus document; no generic loading-completion marker was exposed.
const firefoxViewer = (f: NativeFixtureReceipt): NativeObservation => ({ goal: "Save resident PDF", native_surface: "document",
  page: { url: f.pdfURL, title: "", text: "of 3\n1\nManage pages\nPrevious\nNext\nZoom Out\nZoom In\nPrint\nSave\nNetwork Embedding With Adaptive Sampling\nfor Community Detection\nElena Vargas, Kenji Tanaka\nPublished 2024\nDOI: 10.5555/sentinel.wrap.003\nAbstract" },
  controls: [{ id: "c7", role: "AXButton", label: "Save" }], provenance: { kind: "native-axorcist", revision: "loaded-firefox" } });
const observer = (observation: NativeObservation): NativeDriver => ({ observe: async () => observation,
  act: async () => { throw new Error("unexpected native action"); }, artifact: async () => null });
const options = (readRequests: ReturnType<typeof fixture>["readRequests"]) => ({ signal: AbortSignal.timeout(5_000), readRequests, record: (_event: Record<string, unknown>) => {} });
async function load(f: ReturnType<typeof fixture>) {
  const response = await fetch(f.receipt.pdfURL);
  return Buffer.from(await response.arrayBuffer());
}
async function preparedFixture(browser: NativeBrowser = "chrome") {
  const f = fixture(), bytes = await load(f);
  const prepared = await prepareResidentPDF(f.receipt, observer(browser === "firefox" ? firefoxViewer(f.receipt) : viewer(f.receipt)), { ...options(f.readRequests), browser });
  return { ...f, bytes, prepared };
}

test("baseline pages still serve the real synthetic PDF and private chronological response receipts", async () => {
  const f = fixture();
  const start = await fetch(f.receipt.url), startBody = await start.text();
  expect(startBody).toContain("Read the main article");
  expect(await (await fetch(f.receipt.url.replace(/start$/, "article"))).text()).toContain(f.receipt.filename);
  const pdf = await load(f);
  expect(pdf.equals(readFileSync(new URL("../../internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf", import.meta.url)))).toBe(true);
  expect(createHash("sha256").update(pdf).digest("hex")).toBe(f.receipt.sha256);
  expect(pdf.length).toBe(f.receipt.bytes);
  expect(f.readRequests()).toMatchObject([
    { sequence: 1, path: "/start", responseKind: "html", status: 200, bytes: Buffer.byteLength(startBody), revoked: false },
    { sequence: 2, path: "/article", responseKind: "html" },
    { sequence: 3, responseKind: "pdf", status: 200, bytes: pdf.length },
  ]);
  expect(JSON.parse(readFileSync(join(f.root, "fixture/fixture.json"), "utf8"))).toEqual(f.receipt);
  if (process.platform !== "win32") {
    expect(statSync(join(f.root, "fixture")).mode & 0o777).toBe(0o700);
    expect(statSync(f.requestsPath).mode & 0o777).toBe(0o600);
  }
  expect(() => startNativeFixture(join(f.root, "fixture"))).toThrow();
});

test("revocation is irreversible for full, Range, query, repeated and concurrent PDF requests", async () => {
  const f = fixture();
  await load(f);
  const first = await (await fetch(f.receipt.revokeURL, { method: "POST" })).json();
  expect(first).toMatchObject({ revoked: true, alreadyRevoked: false, revocationSequence: 2 });
  const responses = await Promise.all([null, "bytes=0-127", "bytes=128-", "bytes=-32", "bytes=0-1,4-5", "garbage"].flatMap(range =>
    ["", "?download=1", "?reset=true"].map(async query => {
      const response = await fetch(f.receipt.pdfURL + query, { headers: range ? { Range: range } : {} });
      const body = await response.text();
      expect(response.status).toBe(410);
      expect(response.headers.get("content-type")).toContain("text/html");
      expect(body).not.toContain("%PDF-");
      return response;
    })));
  expect(responses).toHaveLength(18);
  const repeat = await (await fetch(f.receipt.revokeURL, { method: "POST" })).json();
  expect(repeat).toMatchObject({ revoked: true, alreadyRevoked: true, revocationSequence: first.revocationSequence });
  for (const method of ["HEAD", "POST"]) expect((await fetch(f.receipt.pdfURL + "?other=1", { method })).status).toBe(410);
  expect((await fetch(f.receipt.revokeURL.replace(/revoke$/, "reset"), { method: "POST" })).status).toBe(404);
  expect(await (await fetch(f.receipt.statusURL)).json()).toMatchObject({ revoked: true, revocationSequence: first.revocationSequence });
  const rows = f.readRequests();
  expect(rows.map(r => r.sequence)).toEqual(rows.map((_, i) => i + 1));
  expect(rows.slice(1).every(r => r.revoked && r.responseKind !== "pdf")).toBe(true);
  expect(rows.find(r => r.method === "HEAD")?.bytes).toBe(0);
});

test("invalid control route, nonce, query and method cannot revoke or restore the fixture", async () => {
  const f = fixture();
  for (const [url, method, status] of [
    [f.receipt.revokeURL, "GET", 405], [f.receipt.revokeURL, "HEAD", 405], [f.receipt.revokeURL, "PUT", 405],
    [f.receipt.statusURL, "POST", 405], [f.receipt.revokeURL + "?revoke=true", "POST", 404],
    [f.receipt.revokeURL + "/", "POST", 404], [f.receipt.revokeURL.replace(f.receipt.prefix, "/wrong-nonce"), "POST", 404],
  ] as const) expect((await fetch(url, { method })).status).toBe(status);
  expect(await (await fetch(f.receipt.statusURL)).json()).toMatchObject({ revoked: false, revocationSequence: null });
  expect((await load(f)).length).toBe(f.receipt.bytes);
  expect(f.readRequests().every(r => !r.revoked)).toBe(true);
});

test("resident options allow background and owned, but never inference or inspect bypass", () => {
  for (const attention of ["background", "owned"]) expect(() => requireResidentOptions("local-fixture", attention, false)).not.toThrow();
  expect(() => requireResidentOptions("jev", "background", false)).toThrow("local-fixture");
  expect(() => requireResidentOptions("local-fixture", "other", false)).toThrow("attention");
  expect(() => requireResidentOptions("local-fixture", "owned", true)).toThrow("inspect");
});

test("resident receipts bind exact nonce, PDF, endpoints and byte oracle", () => {
  const f = fixture().receipt;
  expect(() => validateResidentFixture(f)).not.toThrow();
  for (const changed of [
    { pdfURL: f.pdfURL + "?same-path" }, { revokeURL: f.revokeURL + "/" }, { statusURL: f.statusURL.replace("127.0.0.1", "example.org") },
    { filename: "other.pdf" }, { sha256: "wrong" }, { bytes: 0 }, { url: f.url.replace("http:", "https:") },
  ]) expect(() => validateResidentFixture({ ...f, ...changed })).toThrow();
});

test("viewer preconditions reject other documents, loading, disabled, ambiguous or nonnative controls", async () => {
  const f = fixture(), valid = viewer(f.receipt);
  const invalid = [
    { ...valid, page: { ...valid.page, url: f.receipt.url } },
    { ...valid, page: { ...valid.page, url: f.receipt.pdfURL + "?other" } },
    { ...valid, page: { ...valid.page, text: "Loading PDF" } },
    { ...valid, provenance: { kind: "dom", revision: "1" } },
    { ...valid, controls: [{ ...valid.controls[0]!, disabled: true }] },
    { ...valid, controls: [{ ...valid.controls[0]!, role: "AXLink" }] },
    { ...valid, controls: [...valid.controls, { ...valid.controls[0]!, id: "duplicate" }] },
    { ...valid, controls: [...valid.controls, { id: "save", role: "AXButton", label: "Save" }] },
  ];
  expect(() => assertResidentViewer(valid, f.receipt)).not.toThrow();
  for (const observation of invalid) await expect(prepareResidentPDF(f.receipt, observer(observation), options(f.readRequests))).rejects.toThrow();
  expect(f.readRequests()).toEqual([]); // No revoke, replay or PDF prefetch on a failed gate.
});

test("native observation precedes revocation, full/Range/query negatives and every native action", async () => {
  const f = fixture();
  await load(f);
  const events: Record<string, unknown>[] = [];
  const driver = observer(viewer(f.receipt));
  driver.observe = async () => { expect(f.readRequests()).toHaveLength(1); return viewer(f.receipt); };
  const prepared = await prepareResidentPDF(f.receipt, driver, { ...options(f.readRequests), record: event => { events.push(event); } });
  expect(events.map(e => e.phase)).toEqual(["browserload", "browserload", "revoke", "replaynegatives", "replaynegatives", "replaynegatives"]);
  expect(f.readRequests().map(r => [r.method, r.responseKind, r.range])).toEqual([
    ["GET", "pdf", null], ["POST", "json", null], ["GET", "html", null], ["GET", "html", "bytes=0-127"], ["GET", "html", "bytes=0-"],
  ]);
  expect(auditResidentRequests(f.receipt, prepared, f.readRequests())).toMatchObject({ pdfResponsesAfterRevocation: 0, laterPDFRequests: 3 });
});

test("missing load, reused revoked fixture and aborted deadline stop before further effects", async () => {
  const f = fixture();
  await expect(prepareResidentPDF(f.receipt, observer(viewer(f.receipt)), options(f.readRequests))).rejects.toThrow("no full original PDF");
  expect(f.readRequests()).toEqual([]);
  await load(f);
  await fetch(f.receipt.revokeURL, { method: "POST" });
  await expect(prepareResidentPDF(f.receipt, observer(viewer(f.receipt)), options(f.readRequests))).rejects.toThrow("already revoked");
  const controller = new AbortController(); controller.abort();
  await expect(prepareResidentPDF(f.receipt, observer(viewer(f.receipt)), { ...options(f.readRequests), signal: controller.signal })).rejects.toThrow();
  expect(f.readRequests()).toHaveLength(2);
});

test("final audit rejects later PDF, missing or altered chronology and duplicated replay negatives", async () => {
  const f = await preparedFixture(), original = f.readRequests();
  for (const mutate of [
    (rows: typeof original) => { rows[2]!.responseKind = "pdf"; },
    (rows: typeof original) => { rows[2]!.contentType = "application/pdf"; },
    (rows: typeof original) => { rows[2]!.revoked = false; },
    (rows: typeof original) => { rows[2]!.bytes++; },
    (rows: typeof original) => { rows[2]!.sequence++; },
    (rows: typeof original) => { rows.pop(); },
  ]) {
    const rows = structuredClone(original); mutate(rows);
    expect(() => auditResidentRequests(f.receipt, f.prepared, rows)).toThrow();
  }
  const duplicated = { ...f.prepared, replays: Array(3).fill(f.prepared.replays[0]) };
  expect(() => auditResidentRequests(f.receipt, duplicated, original)).toThrow();
  const rows = structuredClone(original);
  rows.push({ ...rows[2]!, sequence: rows.length + 1, responseKind: "pdf" });
  expect(() => auditResidentRequests(f.receipt, f.prepared, rows)).toThrow("PDF response after revocation");
});

async function nativeHarness(mode: "pdf" | "html" = "pdf") {
  const f = await preparedFixture(), path = join(f.root, f.receipt.filename);
  let stage = 0, time = 0;
  const events: Record<string, unknown>[] = [];
  const driver: NativeDriver = {
    observe: async () => stage === 0 ? viewer(f.receipt) : { ...viewer(f.receipt), controls: stage === 1 ? [{ id: "save", label: "Save", role: "AXButton" }] : [], provenance: { kind: "native-axorcist", revision: String(stage) } },
    act: async () => {
      expect(f.readRequests().slice(1).every(r => r.revoked)).toBe(true);
      if (++stage === 2) writeFileSync(path, mode === "pdf" ? f.bytes : "<html>revoked</html>");
      return { status: "dispatched", focusChanged: false, pointerMoved: false };
    },
    artifact: async () => stage === 2 && mode === "pdf" ? { path, bytes: f.bytes.length, sha256: f.receipt.sha256 } : null,
  };
  const session = residentNativeSession(f.receipt, f.prepared, driver, event => { events.push(event); });
  const loopOptions = { signal: AbortSignal.timeout(5_000), maxDecisions: 10, noProgressMs: 100, wait: async () => { time += 25; }, now: () => time, record: (event: Record<string, unknown>) => { events.push(event); } };
  return { ...f, path, driver, session, events, loopOptions };
}

test("unchanged native loop independently executes Download/Save and exact saved bytes prove only the fixture", async () => {
  const h = await nativeHarness();
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result).toMatchObject({ status: "downloaded", decisions: 2 });
  expect(h.session.actions).toEqual(["Download", "Save"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path)).toMatchObject({ proven: true, daemonAdoption: "not_tested", audit: { pdfResponsesAfterRevocation: 0 } });
});

test("HTML saved by native controls is not resident-byte success", async () => {
  const h = await nativeHarness("html");
  const deadline = new AbortController();
  await expect(runNativeLoop(h.session.driver, h.session.backend, { ...h.loopOptions, signal: deadline.signal,
    wait: async () => { await h.loopOptions.wait(); if (h.session.actions.length === 2) deadline.abort(new Error("fixture deadline")); },
  })).rejects.toThrow("fixture deadline");
  expect(h.session.actions).toEqual(["Download", "Save"]);
  expect(readFileSync(h.path, "utf8")).toContain("<html>");
  expect(() => finishResidentProof(h.receipt, h.prepared, h.session.actions,
    { status: "downloaded", decisions: 2, artifact: { path: h.path, sha256: h.receipt.sha256, bytes: h.receipt.bytes } },
    h.readRequests(), h.path)).toThrow("SHA-256 and size");
});

test("premature artifacts and changed viewer state cannot pass the native loop", async () => {
  const h = await nativeHarness();
  h.driver.artifact = async () => ({ path: h.path, bytes: h.receipt.bytes, sha256: h.receipt.sha256 });
  await expect(runNativeLoop(h.session.driver, h.session.backend, h.loopOptions)).rejects.toThrow("without verified native");
  h.driver.artifact = async () => null;
  h.driver.observe = async () => ({ ...viewer(h.receipt), page: { ...viewer(h.receipt).page, url: h.receipt.url } });
  await expect(runNativeLoop(h.session.driver, h.session.backend, h.loopOptions)).rejects.toThrow("viewer changed");
  h.driver.observe = async () => ({ ...viewer(h.receipt), provenance: { kind: "native-axorcist", revision: "replaced" } });
  await expect(runNativeLoop(h.session.driver, h.session.backend, h.loopOptions)).rejects.toThrow("viewer changed after revocation");
});

test("file proof rejects wrong content of the same size, wrong size, symlink and altered receipt", async () => {
  const h = await nativeHarness();
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  if (result.status !== "downloaded") throw new Error("expected fake native download");
  const finish = () => finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path);
  writeFileSync(h.path, Buffer.alloc(h.receipt.bytes)); expect(finish).toThrow("SHA-256 and size");
  writeFileSync(h.path, "<html>revoked</html>"); expect(finish).toThrow("SHA-256 and size");
  rmSync(h.path); const target = join(h.root, "target.pdf"); writeFileSync(target, h.bytes); symlinkSync(target, h.path);
  expect(finish).toThrow("SHA-256 and size");
  rmSync(h.path); writeFileSync(h.path, h.bytes);
  result.artifact.sha256 = "0".repeat(64); expect(finish).toThrow("SHA-256 and size");
});

async function fakeHelperRun(f: ReturnType<typeof fixture>, exactViewer: boolean | NativeObservation, extra: string[] = []) {
  const helperPath = join(f.root, "fake-helper.ts"), callPath = join(f.root, "helper-calls.jsonl"), runDir = join(f.root, "run");
  const observation = typeof exactViewer !== "boolean" ? exactViewer : exactViewer ? viewer(f.receipt) : { ...viewer(f.receipt), page: { ...viewer(f.receipt).page, url: f.receipt.url } };
  writeFileSync(helperPath, `#!/usr/bin/env bun\nimport { appendFileSync, readFileSync } from "node:fs";\nimport { createInterface } from "node:readline";
const observation = ${JSON.stringify(observation)};
const status = { focusObserved:true,pointerObserved:true,frontPID:715,frontWindow:61,pointerX:5,pointerY:5 };
for await (const line of createInterface({ input:process.stdin })) {
 const { method, browser } = JSON.parse(line);
 appendFileSync(${JSON.stringify(callPath)}, JSON.stringify({method, ...(method === "configure" ? {browser} : {}), requests:readFileSync(${JSON.stringify(f.requestsPath)},"utf8").trim().split("\\n").length})+"\\n");
 const result = method === "observe" ? observation : method === "act" ? {status:"dispatched",focusChanged:false,pointerMoved:false} : method === "stop_monitor" ? {changeCounts:{app:0,window:0,pointer:0}} : status;
 console.log(JSON.stringify({ok:true,result}));
}
`);
  chmodSync(helperPath, 0o700);
  const process = Bun.spawn([Bun.which("bun")!, new URL("../tools/native-spike-run.ts", import.meta.url).pathname,
    "--helper", helperPath, "--fixture", join(f.root, "fixture/fixture.json"), "--run-dir", runDir, "--resident-pdf", "--no-progress-seconds", "0.01", "--deadline-seconds", "3", ...extra], { stdout: "pipe", stderr: "pipe" });
  const [code, stdout, stderr] = await Promise.all([process.exited, new Response(process.stdout).text(), new Response(process.stderr).text()]);
  return { code, stdout, stderr, runDir, calls: existsSync(callPath) ? readFileSync(callPath, "utf8").trim().split("\n").map(line => JSON.parse(line)) : [] };
}

test("runner rejects wrong viewer before revoking, and still stops its fake monitor", async () => {
  const f = fixture(); await load(f);
  const run = await fakeHelperRun(f, false);
  expect(run.code).not.toBe(0);
  expect(run.stderr).toContain("exact fixture PDF viewer required");
  expect(run.calls.map(c => c.method)).toEqual(["configure", "start_monitor", "observe", "stop_monitor"]);
  expect(f.readRequests()).toHaveLength(1);
  expect(existsSync(join(run.runDir, "failure.json"))).toBe(true);
});

test("runner default background/AX reaches fake native actions only after negatives; no progress stays unproven", async () => {
  const f = fixture(); await load(f);
  const run = await fakeHelperRun(f, true);
  expect(run.code).toBe(0);
  const result = JSON.parse(readFileSync(join(run.runDir, "result.json"), "utf8"));
  expect(result).toMatchObject({ status: "no_progress", residentProof: { proven: false, audit: { pdfResponsesAfterRevocation: 0 } } });
  expect(run.calls.filter(c => c.method === "act")).toEqual([{ method: "act", requests: 5 }]);
  expect(run.calls[0].browser).toBe("chrome");
  const events = readFileSync(join(run.runDir, "events.jsonl"), "utf8").trim().split("\n").map(line => JSON.parse(line));
  expect(events[0]).toMatchObject({ attention: "background", delivery: "ax" });
  expect(new Set(events.map(e => e.phase))).toEqual(new Set(["setup", "browserload", "revoke", "replaynegatives", "nativeactions", "fileproof"]));
  expect(existsSync(join(run.runDir, "monitor.json"))).toBe(true);
});

test("runner rejects Jev before helper launch, credential use or revocation", async () => {
  const f = fixture();
  const run = await fakeHelperRun(f, true, ["--backend", "jev"]);
  expect(run.code).not.toBe(0);
  expect(run.stderr).toContain("requires --backend local-fixture");
  expect(run.calls).toEqual([]);
  expect(f.readRequests()).toEqual([]);
  expect(existsSync(run.runDir)).toBe(false);
});

test("Firefox admission requires the observed document surface, fixture text and enabled Save", async () => {
  const f = fixture(), valid = firefoxViewer(f.receipt);
  expect(nativeBrowser("firefox")).toBe("firefox");
  expect(nativeBrowser("chrome")).toBe("chrome");
  expect(() => nativeBrowser("safari")).toThrow("--browser");
  expect(() => assertResidentViewer(valid, f.receipt, "firefox")).not.toThrow();
  expect(() => assertResidentViewer(valid, f.receipt, "chrome")).toThrow("finish loading");
  expect(() => assertResidentViewer(valid, { ...f.receipt, sha256: "0".repeat(64) }, "firefox")).toThrow("pinned");
  const missing = { ...valid }; delete missing.native_surface;
  const invalid: NativeObservation[] = [
    missing, { ...valid, native_surface: "save-dialog" },
    { ...valid, page: { ...valid.page, url: f.receipt.pdfURL + "?other" } },
    { ...valid, page: { ...valid.page, text: valid.page.text.replace("of 3", "of 30") } },
    { ...valid, page: { ...valid.page, text: valid.page.text.replace("Community Detection", "Link Prediction") } },
    { ...valid, page: { ...valid.page, text: valid.page.text.replace("sentinel.wrap.003", "sentinel.wrap.0034") } },
    { ...valid, page: { ...valid.page, text: "Finished loading PDF" } },
    { ...valid, controls: [{ ...valid.controls[0]!, disabled: true }] },
    { ...valid, controls: [{ ...valid.controls[0]!, role: "AXLink" }] },
    { ...valid, controls: [...valid.controls, { ...valid.controls[0]!, id: "ambiguous" }] },
  ];
  for (const observation of invalid) await expect(prepareResidentPDF(f.receipt, observer(observation), { ...options(f.readRequests), browser: "firefox" })).rejects.toThrow();
  expect(f.readRequests()).toEqual([]);
  // Even the correct Firefox readiness markers cannot replace a full PDF load.
  await expect(prepareResidentPDF(f.receipt, observer(valid), { ...options(f.readRequests), browser: "firefox" })).rejects.toThrow("no full original PDF");
});

async function firefoxHarness(dialog: boolean) {
  const f = await preparedFixture("firefox"), path = join(f.root, f.receipt.filename);
  let stage = 0, time = 0;
  const events: Record<string, unknown>[] = [];
  const document = firefoxViewer(f.receipt);
  // Deliberately identical page, controls and revision: only native_surface
  // changes. Omitting it from semantic progress/effect state strands this Save.
  const sheet: NativeObservation = { ...document, native_surface: "save-dialog" };
  const driver: NativeDriver = {
    observe: async () => stage === 0 ? document : sheet,
    act: async () => {
      expect(f.readRequests().slice(1).every(r => r.revoked)).toBe(true);
      if (++stage === (dialog ? 2 : 1)) writeFileSync(path, f.bytes);
      return { status: "dispatched", focusChanged: false, pointerMoved: false };
    },
    artifact: async () => existsSync(path) ? { path, sha256: f.receipt.sha256, bytes: f.receipt.bytes } : null,
  };
  const session = residentNativeSession(f.receipt, f.prepared, driver, event => { events.push(event); });
  const loopOptions = { signal: AbortSignal.timeout(5_000), maxDecisions: 6, noProgressMs: 100, wait: async () => { time += 25; }, now: () => time, record: (event: Record<string, unknown>) => { events.push(event); } };
  return { ...f, path, driver, session, events, loopOptions, document, sheet };
}

test("Firefox document Save alone can prove exact bytes without inventing a dialog", async () => {
  const h = await firefoxHarness(false);
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result).toMatchObject({ status: "downloaded", decisions: 1 });
  expect(h.session.actions).toEqual(["document:Save"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path)).toMatchObject({ proven: true, browser: "firefox", daemonAdoption: "not_tested" });
  expect(h.events.filter(e => e.kind === "resident_native_action")).toMatchObject([{ label: "Save", action: "document:Save", native_surface: "document" }]);
  for (const actions of [[], ["Save"], ["save-dialog:Save"], ["document:Save", "document:Save"]]) {
    expect(() => finishResidentProof(h.receipt, h.prepared, actions, result, h.readRequests(), h.path)).toThrow("missing native actions");
  }
});

test("native surface alone distinguishes Firefox document/dialog Save in observation, progress and effect hashes", async () => {
  const h = await firefoxHarness(true);
  expect(observationHash(h.document)).not.toBe(observationHash(h.sheet));
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result).toMatchObject({ status: "downloaded", decisions: 2 });
  expect(h.session.actions).toEqual(["document:Save", "save-dialog:Save"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path).proven).toBe(true);
});

test("Firefox never repeats document Save, even while document state keeps changing", async () => {
  const h = await firefoxHarness(true);
  let dispatched = 0, observations = 0;
  h.driver.act = async () => { dispatched++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; };
  h.driver.observe = async () => dispatched ? { ...h.document, page: { ...h.document.page, text: h.document.page.text + `\nChanging document state ${++observations}` } } : h.document;
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result.status).toBe("budget_exhausted");
  expect(dispatched).toBe(1);
  expect(h.session.actions).toEqual(["document:Save"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path).proven).toBe(false);
});

test("Firefox missing surface after document Save cannot execute a second identically labelled control", async () => {
  const h = await firefoxHarness(true), missing = { ...h.sheet }; delete missing.native_surface;
  const observe = h.driver.observe;
  h.driver.observe = async () => {
    const observation = await observe();
    return observation.native_surface === "save-dialog" ? missing : observation;
  };
  await expect(runNativeLoop(h.session.driver, h.session.backend, h.loopOptions)).rejects.toThrow("native surface missing");
  expect(h.session.actions).toEqual(["document:Save"]);
});

test("Firefox fake premature completion and a same-size wrong-byte file cannot prove resident export", async () => {
  const h = await firefoxHarness(false);
  const artifact = h.driver.artifact;
  h.driver.artifact = async () => ({ path: h.path, sha256: h.receipt.sha256, bytes: h.receipt.bytes });
  await expect(runNativeLoop(h.session.driver, h.session.backend, h.loopOptions)).rejects.toThrow("without verified native");
  h.driver.artifact = artifact;
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  writeFileSync(h.path, Buffer.alloc(h.receipt.bytes));
  expect(() => finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path)).toThrow("SHA-256 and size");
});

test("runner sends Firefox selection to configure and waits after its single document Save", async () => {
  const f = fixture(); await load(f);
  const run = await fakeHelperRun(f, firefoxViewer(f.receipt), ["--browser", "firefox"]);
  expect(run.code).toBe(0);
  expect(run.calls[0]).toMatchObject({ method: "configure", browser: "firefox" });
  expect(run.calls.filter(c => c.method === "act")).toEqual([{ method: "act", requests: 5 }]);
  const result = JSON.parse(readFileSync(join(run.runDir, "result.json"), "utf8"));
  expect(result).toMatchObject({ browser: "firefox", status: "no_progress", residentProof: { proven: false, browser: "firefox" } });
  expect(JSON.parse(readFileSync(join(run.runDir, "resident-proof.json"), "utf8")).actions).toEqual(["document:Save"]);
});

test("runner rejects an unknown browser before helper launch or revocation", async () => {
  const f = fixture();
  const run = await fakeHelperRun(f, true, ["--browser", "safari"]);
  expect(run.code).not.toBe(0);
  expect(run.stderr).toContain("--browser must be chrome or firefox");
  expect(run.calls).toEqual([]);
  expect(f.readRequests()).toEqual([]);
});

const downloadsSheet = (document: NativeObservation, ready = false): NativeObservation => ({ ...document, native_surface: "save-dialog",
  page: { ...document.page, text: ready ? "Save As — Downloads" : "Save As — previous destination" },
  controls: ready ? [{ id: "save", role: "AXButton", label: "Save" }] : [
    { id: "save", role: "AXButton", label: "Save", disabled: true },
    { id: "downloads", role: "AXButton", label: "Choose Downloads" },
  ], provenance: { kind: "native-axorcist", revision: ready ? "downloads-destination" : "previous-destination" },
});

test("Firefox optionally chooses Downloads in the bound dialog before Save and still verifies exact bytes", async () => {
  const h = await firefoxHarness(true);
  let stage = 0;
  h.driver.observe = async () => stage === 0 ? h.document : downloadsSheet(h.document, stage >= 2);
  h.driver.act = async choice => {
    expect(choice).toBe(["c7", "downloads", "save"][stage]!);
    if (++stage === 3) writeFileSync(h.path, h.bytes);
    return { status: "dispatched", focusChanged: false, pointerMoved: false };
  };
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result).toMatchObject({ status: "downloaded", decisions: 3 });
  expect(h.session.actions).toEqual(["document:Save", "save-dialog:Downloads", "save-dialog:Save"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path)).toMatchObject({ proven: true, daemonAdoption: "not_tested" });
  expect(h.events.filter(e => e.kind === "resident_native_action")).toMatchObject([
    { action: "document:Save", label: "Save", native_surface: "document" },
    { action: "save-dialog:Downloads", label: "Choose Downloads", native_surface: "save-dialog" },
    { action: "save-dialog:Save", label: "Save", native_surface: "save-dialog" },
  ]);
  for (const actions of [
    ["document:Save", "save-dialog:Downloads"],
    ["save-dialog:Downloads", "document:Save", "save-dialog:Save"],
    ["document:Save", "save-dialog:Downloads", "save-dialog:Downloads", "save-dialog:Save"],
    ["document:Save", "save-dialog:Downloads", "save-dialog:Save", "save-dialog:Save"],
  ]) expect(() => finishResidentProof(h.receipt, h.prepared, actions, result, h.readRequests(), h.path)).toThrow("missing native actions");
  writeFileSync(h.path, Buffer.alloc(h.receipt.bytes));
  expect(() => finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path)).toThrow("SHA-256 and size");
});

test("Firefox never repeats Choose Downloads or bypasses it if the dialog keeps changing", async () => {
  const h = await firefoxHarness(true);
  let effects = 0, observations = 0;
  h.driver.observe = async () => {
    if (effects === 0) return h.document;
    const sheet = downloadsSheet(h.document);
    return effects >= 2 ? { ...sheet, page: { ...sheet.page, text: sheet.page.text + ` ${++observations}` } } : sheet;
  };
  h.driver.act = async () => { effects++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; };
  const result = await runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
  expect(result.status).toBe("budget_exhausted");
  expect(effects).toBe(2);
  expect(h.session.actions).toEqual(["document:Save", "save-dialog:Downloads"]);
  expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path).proven).toBe(false);
});

test("Firefox cannot choose Downloads on a document or repeat dialog Save after destination selection", async () => {
  for (const documentSurface of [true, false]) {
    const h = await firefoxHarness(true);
    let effects = 0, observations = 0;
    h.driver.observe = async () => {
      if (effects === 0) return h.document;
      const sheet = downloadsSheet(h.document, effects >= 2);
      return { ...sheet, native_surface: documentSurface ? "document" : "save-dialog",
        page: { ...sheet.page, text: sheet.page.text + (documentSurface || effects >= 3 ? ` ${++observations}` : "") } };
    };
    h.driver.act = async () => { effects++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; };
    const deadline = new AbortController();
    const run = runNativeLoop(h.session.driver, h.session.backend, { ...h.loopOptions, signal: deadline.signal,
      wait: async () => { await h.loopOptions.wait(); if (effects === 3) deadline.abort(new Error("fixture deadline")); },
    });
    if (documentSurface) expect((await run).status).toBe("budget_exhausted");
    else await expect(run).rejects.toThrow("fixture deadline");
    expect(effects).toBe(documentSurface ? 1 : 3);
    expect(h.session.actions).toEqual(documentSurface ? ["document:Save"] : ["document:Save", "save-dialog:Downloads", "save-dialog:Save"]);
  }
});

test("Firefox waits for a disabled Choose Downloads control and rejects ambiguous projected controls", async () => {
  for (const ambiguous of [false, true]) {
    const h = await firefoxHarness(true);
    let effects = 0;
    h.driver.observe = async () => {
      if (!effects) return h.document;
      const sheet = downloadsSheet(h.document);
      if (ambiguous) sheet.controls.push({ id: "duplicate", role: "AXButton", label: "Choose Downloads" });
      else sheet.controls[1]!.disabled = true;
      return sheet;
    };
    h.driver.act = async () => { effects++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; };
    const run = runNativeLoop(h.session.driver, h.session.backend, h.loopOptions);
    if (ambiguous) await expect(run).rejects.toThrow("ambiguous native save control");
    else expect((await run).status).toBe("no_progress");
    expect(h.session.actions).toEqual(["document:Save"]);
  }
});

async function pendingSaveHarness(browser: NativeBrowser) {
  const f = await preparedFixture(browser), path = join(f.root, f.receipt.filename);
  const document = browser === "firefox" ? firefoxViewer(f.receipt) : viewer(f.receipt);
  const dialog: NativeObservation = { ...document, native_surface: "save-dialog", page: { ...document.page, text: "Save As — Downloads" },
    controls: [{ id: "save", role: "AXButton", label: "Save" }] };
  const stages = browser === "firefox" ? [document, downloadsSheet(document), dialog] : [document, dialog];
  let effects = 0, reads = 0, polls = 0, waits = 0;
  const events: Record<string, unknown>[] = [];
  const driver: NativeDriver = {
    observe: async () => {
      if (effects === stages.length) throw new Error("retained save dialog disappeared");
      reads++;
      return stages[effects]!;
    },
    act: async () => { effects++; return { status: "dispatched", focusChanged: false, pointerMoved: false }; },
    artifact: async () => {
      polls++;
      return existsSync(path) ? { path, sha256: f.receipt.sha256, bytes: f.receipt.bytes } : null;
    },
  };
  const session = residentNativeSession(f.receipt, f.prepared, driver, event => { events.push(event); });
  const deadline = new AbortController();
  const options = { signal: deadline.signal, maxDecisions: stages.length, noProgressMs: 1, now: () => waits * 10,
    wait: async () => { waits++; }, record: (event: Record<string, unknown>) => { events.push(event); } };
  return { ...f, path, driver, session, deadline, options, events, stages, effects: () => effects, reads: () => reads, polls: () => polls };
}

test("final dialog Save waits for delayed file bytes without observing the vanished surface or spending decisions", async () => {
  for (const browser of ["chrome", "firefox"] as const) {
    const h = await pendingSaveHarness(browser);
    let pendingWaits = 0;
    const result = await runNativeLoop(h.session.driver, h.session.backend, { ...h.options, wait: async () => {
      await h.options.wait();
      const finalSave = h.effects() === h.stages.length;
      expect(await h.session.driver.pendingArtifact!()).toBe(finalSave);
      if (finalSave && ++pendingWaits === 3) writeFileSync(h.path, h.bytes);
    } });
    expect(result).toMatchObject({ status: "downloaded", decisions: h.stages.length });
    expect(pendingWaits).toBe(3);
    expect(h.effects()).toBe(h.stages.length);
    expect(h.reads()).toBe(h.stages.length * 2); // Only pre-dispatch observations.
    expect(h.polls()).toBe(h.stages.length + 3);
    expect(finishResidentProof(h.receipt, h.prepared, h.session.actions, result, h.readRequests(), h.path).proven).toBe(true);
  }
});

test("stalled post-dialog file completion aborts at the outer deadline without observing or replaying native effects", async () => {
  for (const browser of ["chrome", "firefox"] as const) {
    const h = await pendingSaveHarness(browser);
    let pendingWaits = 0;
    await expect(runNativeLoop(h.session.driver, h.session.backend, { ...h.options, wait: async () => {
      await h.options.wait();
      if (h.effects() === h.stages.length && ++pendingWaits === 3) h.deadline.abort(new Error("fixture deadline"));
    } })).rejects.toThrow("fixture deadline");
    expect(pendingWaits).toBe(3);
    expect(h.effects()).toBe(h.stages.length);
    expect(h.reads()).toBe(h.stages.length * 2);
    expect(h.events.filter(event => event.kind === "decision")).toHaveLength(h.stages.length);
    expect(existsSync(h.path)).toBe(false);
  }
});

test("pending artifact latch excludes Firefox document Save and any stale final dialog dispatch", async () => {
  const h = await firefoxHarness(true);
  expect(await h.session.driver.pendingArtifact!()).toBe(false);
  let observation = await h.session.driver.observe();
  await h.session.driver.act("c7", observationHash(observation));
  expect(h.session.actions).toEqual(["document:Save"]);
  expect(await h.session.driver.pendingArtifact!()).toBe(false);
  const act = h.driver.act;
  h.driver.act = async () => ({ status: "stale", focusChanged: false, pointerMoved: false });
  observation = await h.session.driver.observe();
  await h.session.driver.act("c7", observationHash(observation));
  expect(h.session.actions).toEqual(["document:Save"]);
  expect(await h.session.driver.pendingArtifact!()).toBe(false);
  h.driver.act = act;
  observation = await h.session.driver.observe();
  await h.session.driver.act("c7", observationHash(observation));
  expect(h.session.actions).toEqual(["document:Save", "save-dialog:Save"]);
  expect(await h.session.driver.pendingArtifact!()).toBe(true);
});
