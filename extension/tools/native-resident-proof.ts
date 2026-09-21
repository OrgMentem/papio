// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only resident-byte oracle. No browser navigation or daemon adoption.
import { createHash } from "node:crypto";
import { lstatSync, readFileSync } from "node:fs";
import type { FixtureRequest, NativeFixtureReceipt } from "./native-spike-fixture";
import { observationHash, type DecisionBackend, type NativeDriver, type NativeObservation, type NativeRunResult } from "./native-spike-loop";

function requireProof(ok: unknown, message: string): asserts ok {
  if (!ok) throw new Error(`Resident PDF: ${message}`);
}
type RecordEvent = (event: Record<string, unknown>) => void;
export type NativeBrowser = "chrome" | "firefox";
export function nativeBrowser(value: string): NativeBrowser {
  requireProof(value === "chrome" || value === "firefox", "--browser must be chrome or firefox");
  return value;
}
// The Firefox 156 AX capture exposes this exact corpus document and page count,
// not Chrome's loading-complete marker. Pin metadata to its known bytes; this
// gate is deliberately not a generic PDF readiness heuristic.
const firefoxFixture = {
  sha256: "66e8c946de161091c09418d1586a8b1cefeeb05f8de011f41d610f65f5676365", bytes: 5661,
  title: "Network Embedding With Adaptive Sampling for Community Detection", doi: "10.5555/sentinel.wrap.003", pages: 3,
};
export function requireResidentOptions(backend: string, attention: string, inspect: boolean) {
  requireProof(backend === "local-fixture", "requires --backend local-fixture");
  requireProof(["background", "owned"].includes(attention), "requires a supported attention mode");
  requireProof(!inspect, "--inspect cannot run the resident proof");
}
export function validateResidentFixture(fixture: NativeFixtureReceipt) {
  const url = new URL(fixture.url);
  requireProof(url.protocol === "http:" && url.hostname === "127.0.0.1" && url.port && !url.username && !url.password, "requires loopback fixture");
  requireProof(/^\/papio-native-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(fixture.prefix), "invalid fixture nonce");
  const base = `${url.origin}${fixture.prefix}`;
  requireProof(fixture.filename === `${fixture.prefix.slice(1)}.pdf` && fixture.url === `${base}/start` &&
    fixture.pdfURL === `${base}/${fixture.filename}` && fixture.revokeURL === `${base}/control/revoke` &&
    fixture.statusURL === `${base}/control/status`, "requires exact PDF and control endpoints from a fresh fixture");
  requireProof(/^[a-f0-9]{64}$/.test(fixture.sha256) && Number.isSafeInteger(fixture.bytes) && fixture.bytes > 0, "invalid fixture byte oracle");
}
export function assertResidentViewer(observation: NativeObservation, fixture: NativeFixtureReceipt, browser: NativeBrowser = "chrome") {
  requireProof(observation.page.url === fixture.pdfURL, "already-open exact fixture PDF viewer required");
  requireProof(observation.provenance.kind === "native-axorcist" && observation.provenance.revision, "native viewer observation required");
  if (browser === "firefox") {
    requireProof(observation.native_surface === "document", "Firefox requires an explicit native document surface");
    requireProof(fixture.sha256 === firefoxFixture.sha256 && fixture.bytes === firefoxFixture.bytes, "Firefox readiness is pinned to the synthetic title_wrap PDF");
    const lines = observation.page.text.split(/\r?\n/).map(line => line.trim());
    requireProof(lines[0] === `of ${firefoxFixture.pages}` && lines.includes(`DOI: ${firefoxFixture.doi}`) &&
      observation.page.text.replace(/\s+/g, " ").includes(firefoxFixture.title), "Firefox fixture title, DOI and three-page marker required");
    const saves = observation.controls.filter(c => c.role === "AXButton" && c.label === "Save" && !c.disabled);
    requireProof(saves.length === 1 && !observation.controls.some(c => c.label === "Download" && !c.disabled), "one enabled Firefox document Save button required");
    return;
  }
  requireProof(/Finished loading PDF/i.test(observation.page.text), "PDF must finish loading before revocation");
  const downloads = observation.controls.filter(c => c.role === "AXButton" && c.label === "Download" && !c.disabled);
  requireProof(downloads.length === 1 && !observation.controls.some(c => c.label === "Save" && !c.disabled), "one enabled native Download button required, without an open Save dialog");
}
export function readFixtureRequests(path: string): FixtureRequest[] {
  const text = readFileSync(path, "utf8");
  requireProof(text === "" || text.endsWith("\n"), "incomplete fixture request journal");
  return text.trim() ? text.trim().split("\n").map(line => JSON.parse(line)) : [];
}
function checkJournal(requests: FixtureRequest[]) {
  for (const [i, request] of requests.entries()) {
    requireProof(request.sequence === i + 1 && Number.isFinite(Date.parse(request.at)) &&
      ["pdf", "html", "json", "text"].includes(request.responseKind) &&
      Number.isSafeInteger(request.bytes) && request.bytes >= 0 && Number.isSafeInteger(request.status) &&
      typeof request.method === "string" && typeof request.path === "string" && typeof request.query === "string" &&
      (request.range === null || typeof request.range === "string") && typeof request.contentType === "string",
    "invalid or incomplete response chronology");
  }
}
function browserLoads(fixture: NativeFixtureReceipt, requests: FixtureRequest[]) {
  return requests.filter(r => r.path === `/${fixture.filename}` && r.query === "" && r.method === "GET" &&
    r.responseKind === "pdf" && r.contentType === "application/pdf" && r.status === 200 && r.bytes === fixture.bytes && !r.revoked);
}

// Bound both headers and body to the existing run deadline and a per-call cap.
// No redirect can turn a loopback negative control into a provider request.
async function requestProof(url: string, method: string, range: string | null, deadline: AbortSignal) {
  const signal = AbortSignal.any([deadline, AbortSignal.timeout(5_000)]);
  signal.throwIfAborted();
  const response = await fetch(url, { method, redirect: "error", cache: "no-store", signal,
    headers: { "User-Agent": "papio-native-resident-proof", ...(range ? { Range: range } : {}) } });
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  const reader = response.body?.getReader();
  try {
    if (reader) while (true) {
      const next = await reader.read();
      signal.throwIfAborted();
      if (next.done) break;
      bytes += next.value.byteLength;
      requireProof(bytes <= 65_536, "control/replay response exceeds limit");
      chunks.push(next.value);
    }
  } finally { await reader?.cancel(); }
  const body = Buffer.concat(chunks);
  const sequence = Number(response.headers.get("x-papio-fixture-sequence"));
  requireProof(Number.isSafeInteger(sequence) && sequence > 0, "response missing fixture chronology");
  return { body, sequence, status: response.status, bytes, contentType: response.headers.get("content-type") ?? "", url, method, range };
}
type Replay = Omit<Awaited<ReturnType<typeof requestProof>>, "body"> & { sha256: string };
const replayRequests = (fixture: NativeFixtureReceipt) => [[fixture.pdfURL, null], [fixture.pdfURL, "bytes=0-127"], [`${fixture.pdfURL}?resident-replay=1`, "bytes=0-"]] as const;
export interface ResidentPreparation { browser: NativeBrowser; observation: NativeObservation; revocationSequence: number; replays: Replay[] }
export async function prepareResidentPDF(fixture: NativeFixtureReceipt, driver: NativeDriver,
  options: { signal: AbortSignal; record: RecordEvent; readRequests: () => FixtureRequest[]; browser?: NativeBrowser }): Promise<ResidentPreparation> {
  validateResidentFixture(fixture);
  const browser = options.browser ?? "chrome";
  options.signal.throwIfAborted();
  // The runner's first observation precedes every network control and native effect.
  const observation = await driver.observe();
  options.record({ phase: "browserload", kind: "resident_viewer_observed", browser, observation });
  assertResidentViewer(observation, fixture, browser);
  const before = options.readRequests();
  checkJournal(before);
  requireProof(before.every(r => r.revoked === false && r.revocationSequence === null), "fixture already revoked; use a fresh fixture");
  requireProof(browserLoads(fixture, before).length > 0, "no full original PDF response in fixture journal");
  options.record({ phase: "browserload", kind: "resident_load_verified", responseSequences: browserLoads(fixture, before).map(r => r.sequence) });
  const revoked = await requestProof(fixture.revokeURL, "POST", null, options.signal);
  const state = JSON.parse(revoked.body.toString());
  requireProof(revoked.status === 200 && state.pdfURL === fixture.pdfURL && state.revoked === true &&
    state.alreadyRevoked === false && state.revocationSequence === revoked.sequence, "revocation not freshly acknowledged");
  const revocationSequence = revoked.sequence;
  options.record({ phase: "revoke", kind: "resident_revoked", revocationSequence, state });
  const replays: Replay[] = [];
  for (const [url, range] of replayRequests(fixture)) {
    const { body, ...response } = await requestProof(url, "GET", range, options.signal);
    const nonPDF = response.status === 410 && /^text\/html\b/i.test(response.contentType) && body.length > 0 && !body.includes(Buffer.from("%PDF-"));
    options.record({ phase: "replaynegatives", kind: "resident_replay", ...response, nonPDF });
    requireProof(nonPDF, "replay was not the revoked non-PDF response");
    replays.push({ ...response, sha256: createHash("sha256").update(body).digest("hex") });
  }
  const prepared = { browser, observation, revocationSequence, replays };
  auditResidentRequests(fixture, prepared, options.readRequests());
  return prepared;
}
export function auditResidentRequests(fixture: NativeFixtureReceipt, prepared: ResidentPreparation, requests: FixtureRequest[]) {
  checkJournal(requests);
  const boundary = requests[prepared.revocationSequence - 1];
  requireProof(boundary?.method === "POST" && boundary.path === "/control/revoke" && boundary.query === "" && boundary.responseKind === "json" && boundary.status === 200, "missing revocation boundary");
  requireProof(browserLoads(fixture, requests.slice(0, prepared.revocationSequence - 1)).length > 0, "missing original PDF response");
  for (const request of requests) {
    const after = request.sequence >= prepared.revocationSequence;
    requireProof(request.revoked === after && request.revocationSequence === (after ? prepared.revocationSequence : null), "revocation latch or chronology changed");
    requireProof(!after || (request.responseKind !== "pdf" && !/application\/pdf/i.test(request.contentType)), "PDF response after revocation");
  }
  requireProof(prepared.replays.length === 3, "missing replay negatives");
  for (const [i, replay] of prepared.replays.entries()) {
    const row = requests[replay.sequence - 1], url = new URL(replay.url);
    const [expectedURL, expectedRange] = replayRequests(fixture)[i]!;
    requireProof(replay.url === expectedURL && replay.range === expectedRange && replay.status === 410 && replay.bytes > 0 &&
      replay.sequence > (prepared.replays[i - 1]?.sequence ?? prepared.revocationSequence) && row?.path === `/${fixture.filename}` && row.query === url.search &&
      row.method === "GET" && row.range === replay.range && row.responseKind === "html" && row.status === 410 &&
      row.bytes === replay.bytes && row.contentType === replay.contentType, "replay negative missing from response journal");
  }
  return { throughSequence: requests.length, revocationSequence: prepared.revocationSequence, pdfResponsesAfterRevocation: 0,
    laterPDFRequests: requests.filter(r => r.sequence > prepared.revocationSequence && r.path === `/${fixture.filename}`).length };
}

// Keep the existing native loop and its freshness/attention checks. Resident mode
// merely restricts deterministic choices to the observed browser's save flow.
function nativeActionsComplete(browser: NativeBrowser, actions: string[]) {
  const sequence = actions.join(",");
  return browser === "firefox" ? ["document:Save", "document:Save,save-dialog:Save", "document:Save,save-dialog:Downloads,save-dialog:Save"].includes(sequence) : sequence === "Download,Save";
}
export function residentNativeSession(fixture: NativeFixtureReceipt, prepared: ResidentPreparation, driver: NativeDriver, record: RecordEvent) {
  const actions: string[] = [];
  let current: NativeObservation | undefined;
  const nextAction = (observation: NativeObservation) => {
    if (prepared.browser === "firefox") {
      if (actions.length === 0 && observation.native_surface === "document") return { label: "Save", receipt: "document:Save" };
      if (observation.native_surface === "save-dialog" && (actions.length === 1 || (actions.length === 2 && actions[1] === "save-dialog:Downloads"))) {
        // Only the helper projects this control after binding the exact fixture
        // dialog. Wait if it persists after dispatch; never retry or bypass it.
        if (observation.controls.some(c => c.role === "AXButton" && c.label === "Choose Downloads")) {
          return actions.length === 1 ? { label: "Choose Downloads", receipt: "save-dialog:Downloads" } : undefined;
        }
        return { label: "Save", receipt: "save-dialog:Save" };
      }
      return undefined; // Never repeat a document Save while waiting for its result.
    }
    const label = ["Download", "Save"][actions.length];
    return label ? { label, receipt: label } : undefined;
  };
  const guarded: NativeDriver = {
    observe: async () => {
      const observation = await driver.observe();
      requireProof(observation.page.url === fixture.pdfURL && observation.provenance.kind === "native-axorcist", "viewer changed during native save");
      if (prepared.browser === "firefox") requireProof(observation.native_surface === "document" || observation.native_surface === "save-dialog", "Firefox native surface missing or invalid");
      if (!actions.length) requireProof(observationHash(observation) === observationHash(prepared.observation), "viewer changed after revocation");
      current = observation;
      return observation;
    },
    act: async (choice, hash) => {
      const target = current?.controls.find(c => c.id === choice && c.role === "AXButton" && !c.disabled);
      const action = current && nextAction(current);
      requireProof(current && observationHash(current) === hash && action && target?.label === action.label, "unexpected native action");
      const result = await driver.act(choice, hash);
      record({ phase: "nativeactions", kind: "resident_native_action", label: action.label, action: action.receipt,
        native_surface: current.native_surface, observationHash: hash, ...result });
      if (result.status === "dispatched") actions.push(action.receipt);
      return result;
    },
    artifact: async () => {
      const artifact = await driver.artifact();
      if (artifact) requireProof(nativeActionsComplete(prepared.browser, actions) && artifact.sha256 === fixture.sha256 && artifact.bytes === fixture.bytes, "file appeared without verified native save actions or has wrong bytes");
      return artifact;
    },
    // A successfully dispatched final dialog Save can close the retained native
    // surface before the file completes. Poll bytes under the outer deadline;
    // observing that vanished dialog must not abort or replay the transfer.
    pendingArtifact: async () => prepared.browser === "firefox" ? actions.at(-1) === "save-dialog:Save" : actions.join(",") === "Download,Save",
  };
  const backend: DecisionBackend = { decide: async (observation, signal) => {
    signal.throwIfAborted();
    const label = nextAction(observation)?.label;
    const targets = observation.controls.filter(c => c.role === "AXButton" && c.label === label && !c.disabled);
    requireProof(targets.length <= 1, "ambiguous native save control");
    return { choice: targets[0]?.id ?? "WAIT", observationHash: observationHash(observation) };
  } };
  return { driver: guarded, backend, actions };
}
export function finishResidentProof(fixture: NativeFixtureReceipt, prepared: ResidentPreparation, actions: string[], result: NativeRunResult, requests: FixtureRequest[], expectedPath: string) {
  const audit = auditResidentRequests(fixture, prepared, requests);
  if (result.status !== "downloaded") return { proven: false, browser: prepared.browser, audit };
  requireProof(nativeActionsComplete(prepared.browser, actions) && result.artifact.path === expectedPath, "missing native actions or wrong destination");
  const stat = lstatSync(expectedPath), bytes = readFileSync(expectedPath);
  const digest = createHash("sha256").update(bytes).digest("hex");
  requireProof(stat.isFile() && stat.size === fixture.bytes && bytes.byteLength === fixture.bytes && digest === fixture.sha256 &&
    result.artifact.bytes === fixture.bytes && result.artifact.sha256 === fixture.sha256, "saved file does not match exact fixture SHA-256 and size");
  return { proven: true, browser: prepared.browser, audit, artifact: { path: expectedPath, sha256: digest, bytes: bytes.byteLength }, daemonAdoption: "not_tested" };
}
