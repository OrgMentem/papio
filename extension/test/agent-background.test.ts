// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { Bridge, MIN_DAEMON_VERSION, type BridgeDeps, type NativePort } from "../src/background";
import { agentDOM, type AgentDOMRequest } from "../src/agent-dom";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { planGeneric } from "../src/plan";
import { emptyStore, patchJob, type ActiveJob, type StoreShape } from "../src/state";
import { FakeDownloads } from "./fake-downloads";
import { ChromeTabsFake, FakeEmitter } from "./fake-tabs";

const jobID = "job_agent_article";
const tabID = 77;
const url = "https://unregistered.example/article/one";
const doi = "10.1234/article";
const epoch = { drive_attempt_id: "agent-attempt-1", ordinal: 0, strategy: "generic" as const, revision: "1" };
const localEpoch = { ...epoch, attempt_count: 0 };
const features = ["agent_fallback_v1", "provider_drive_epoch_v1", "effect_permit_v1"];
const globals = new Map<string, PropertyDescriptor | undefined>();
afterEach(() => {
  for (const [key, descriptor] of globals) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor); else Reflect.deleteProperty(globalThis, key);
  }
  globals.clear();
});
async function flush() { for (let i = 0; i < 100; i++) await Promise.resolve(); }
async function until(predicate: () => boolean) {
  for (let i = 0; i < 100; i++) {
    if (predicate()) return;
    await new Promise(resolve => setTimeout(resolve, 1));
  }
  expect(predicate()).toBe(true);
}

async function harness(options: { features?: string[]; firefox?: boolean; status?: ActiveJob["status"]; seed?: StoreShape } = {}) {
  const win = new Window({ url, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
  win.document.write(`<meta name="citation_doi" content="${doi}"><meta name="citation_title" content="Example article"><main><h1>Example article</h1><button type="button">Formats</button></main><header><input type="search" value="PRIVATEQUERY"></header>`);
  Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
  for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioArticleAgent: undefined })) {
    if (!globals.has(key)) globals.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
  }
  const timers: { fn: () => void; ms: number }[] = [];
  let now = 1_700_000_000_000, seq = 1;
  const frames: BrowserMessage[] = [];
  const port: NativePort & { onMessage: FakeEmitter<[unknown]> } = {
    onMessage: new FakeEmitter<[unknown]>(), onDisconnect: new FakeEmitter<[]>(),
    disconnect() {}, postMessage(message) { frames.push(parseBrowserMessage(message)); },
  };
  const tabs = new ChromeTabsFake();
  tabs.seed({ id: tabID, url, status: "complete" });
  const downloads = new FakeDownloads();
  if (options.firefox) Reflect.deleteProperty(downloads, "onDeterminingFilename");
  const backend = { store: emptyStore(), load: async () => backend.store, save: async (store: StoreShape) => { backend.store = store; } };
  let permitted = true, observations = 0, actions = 0, genericPlans = 0;
  let onAct: (() => Promise<void>) | undefined;
  const deps: BridgeDeps = {
    connectNative: () => port, manifestVersion: "0.1.0", randomUUID: () => crypto.randomUUID(), now: () => now,
    setTimeout: (fn, ms) => timers.push({ fn, ms }), backend, tabs, downloads, adapterSpecs: [],
    scripting: { executeScript: async injection => {
      if (injection.func === planGeneric) { genericPlans++; return [{ result: { evidence: [], candidates: [] } }]; }
      if (injection.func !== agentDOM) return [];
      const request = injection.args![0] as AgentDOMRequest;
      if (request.method === "observe") observations++;
      else { actions++; await onAct?.(); }
      return [{ result: await agentDOM(request) }];
    } },
    permissions: { contains: async () => permitted },
    settings: { getTermsConsent: async () => undefined, setTermsConsent: async () => {}, getHandoffSurface: async () => "in-window", getInPageToast: async () => false },
    action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {} },
    alarms: { create: () => {}, onAlarm: new FakeEmitter<[{ name: string }]>(), },
  };
  const bridge = new Bridge(deps);
  const inbound = async (type: BrowserMessage["type"], payload: Record<string, unknown>, scoped = true) => port.onMessage.emit({
    protocol: "papio-browser/1", type, msg_id: `agent-test-${seq}`, seq: seq++, ...(scoped ? { job_id: jobID } : {}), payload,
  });
  await bridge.start();
  await inbound("hello_ack", { daemon_version: MIN_DAEMON_VERSION, role: "holder", browser_holder_generation: 1, features: options.features ?? features }, false);
  const update = (reducer: (store: StoreShape) => StoreShape): Promise<void> => Reflect.get(bridge, "update").call(bridge, reducer);
  const job: ActiveJob = { job_id: jobID, tab_id: tabID, offered_at: now - 20_000, expires_at: now + 3600_000,
    status: options.status ?? "accepted", provider_hosts: [], access_mode: "delegated", expected: { doi }, generic_drive_epoch: localEpoch,
    unknown_count: 1, last_unknown_ms: now - 10_000 };
  await update(store => ({ ...store, ...(options.seed ?? {}), activeJobs: options.seed?.activeJobs ?? [job] }));
  Reflect.get(bridge, "handoffDrives").set(jobID, { tabID, token: {} });
  const classify = () => Reflect.get(bridge, "maybeClassify").call(bridge, jobID, "unregistered.example") as Promise<void>;
  const reply = async (request: BrowserMessage, type: BrowserMessage["type"], payload: Record<string, unknown>) => inbound(type, { request_id: request.payload["request_id"], ...payload });
  const request = async (type: BrowserMessage["type"], after = 0) => {
    await until(() => frames.slice(after).some(frame => frame.type === type));
    return frames.slice(after).find(frame => frame.type === type)!;
  };
  const started = async () => {
    const frame = await request("provider_drive_epoch_start_request");
    await reply(frame, "provider_drive_epoch_start_result", { ...epoch, outcome: "started" });
  };
  const decide = async (outcome: string, choice?: string, after = 0) => {
    const frame = await request("agent_decide_request_v1", after);
    const observation = frame.payload["observation"] as { revision: string };
    await reply(frame, "agent_decide_result_v1", { observation_revision: observation.revision, outcome, ...(choice ? { choice } : {}) });
    return frame;
  };
  const settle = async () => {
    const frame = await request("provider_drive_epoch_result_request");
    await reply(frame, "provider_drive_epoch_result", { ...epoch, outcome: "applied" });
    await flush();
  };
  const tick = async () => {
    const index = timers.findIndex(timer => timer.ms === 1000);
    expect(index).toBeGreaterThanOrEqual(0);
    now += 1000; timers.splice(index, 1)[0]!.fn(); await flush();
  };
  return { bridge, deps, backend, frames, win, tabs, downloads, timers, classify, started, decide, request, reply, settle, tick, update,
    counts: () => ({ observations, actions, genericPlans }), setPermission: (value: boolean) => { permitted = value; }, setOnAct: (fn: () => Promise<void>) => { onAct = fn; }, advance: (ms: number) => { now += ms; }, inbound };
}

for (const status of ["accepted", "auth_pending"] as const) test(`no-adapter ${status} article: WAIT, click, exact download correlation and normal adoption`, async () => {
  const h = await harness({ status, features: [...features, "session_evidence_v1"] });
  await h.classify(); // Must return while the epoch-start RPC is pending.
  await h.started();
  const waitRequest = await h.decide("decision", "WAIT");
  const after = h.frames.indexOf(waitRequest) + 1;
  expect(h.counts().actions).toBe(0);
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
  if (status === "auth_pending") {
    expect(h.backend.store.activeJobs[0]?.status).toBe("awaiting_download");
    expect(h.frames.some(frame => frame.type === "auth_returned" || frame.type === "session_evidence" || frame.type === "claim_observation")).toBe(false);
    expect(Reflect.get(h.bridge, "deliverySessionEvidence").has(jobID)).toBe(false);
    expect(Reflect.get(h.bridge, "openAccessLandingObserved")).toBe(false);
    expect(h.backend.store.lastAuthReturnedAt).toBeUndefined();
    expect(h.backend.store.authEvidenceByOrigin ?? {}).toEqual({});
  }
  await h.tick();
  h.setOnAct(async () => {
    // The real producer must already be armed before script dispatch.
    const track = Reflect.get(h.bridge, "downloads").get(jobID);
    expect(track.generic.epoch).toEqual(localEpoch);
    expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
    await h.downloads.onCreated.emit({ id: 901, tabId: tabID, url: "https://unregistered.example/paper", state: "in_progress" });
  });
  await h.decide("decision", "c1", after);
  await until(() => h.counts().actions === 1);
  await flush();
  expect(h.counts().genericPlans).toBe(1);
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBe(901);
  expect(h.backend.store.activeJobs[0]?.adapter_id).toBeUndefined();
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_result_request")).toBe(false);
  let suggested: string | undefined;
  await h.downloads.onDeterminingFilename.emit({ id: 901, tabId: tabID, url: "https://unregistered.example/paper", filename: "paper.pdf" }, suggestion => { suggested = suggestion.filename; });
  expect(suggested).toContain(`papio/${jobID}/`);
  h.downloads.items.set(901, { id: 901, tabId: tabID, filename: `/tmp/papio/${jobID}/paper.pdf`, fileSize: 123, mime: "application/pdf", state: "complete" });
  const completing = h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  await h.settle();
  await completing;
  expect(h.frames.find(frame => frame.type === "download_complete")?.payload["producer"]).toEqual({
    effect_kind: "generic_drive", ...epoch,
  });
  expect(h.frames.find(frame => frame.type === "download_complete")?.job_id).toBe(jobID);
  expect(h.frames.some(frame => frame.type === "auth_returned" || frame.type === "session_evidence" || frame.type === "claim_observation")).toBe(false);
  expect(h.tabs.created).toHaveLength(0);
  expect(JSON.stringify(h.backend.store)).not.toContain(url);
});

for (const mode of ["old", "firefox", "permission"] as const) test(`fallback does not start with ${mode} capability gap`, async () => {
  const h = await harness({ ...(mode === "old" ? { features: ["provider_drive_epoch_v1", "effect_permit_v1"] } : {}), firefox: mode === "firefox" });
  if (mode === "permission") h.setPermission(false);
  await h.classify(); await flush();
  expect(h.frames.some(frame => frame.type === "agent_decide_request_v1" || frame.type === "provider_drive_epoch_start_request")).toBe(false);
  expect(h.counts().actions).toBe(0);
});

for (const outcome of ["unavailable", "exhausted", "stale"] as const) test(`${outcome} returns a specific operator detail and settles the epoch`, async () => {
  const h = await harness(); await h.classify(); await h.started(); await h.decide(outcome); await h.settle();
  const detail = h.frames.find(frame => frame.type === "provider_outcome")?.payload["detail"];
  expect(detail).toContain(outcome === "exhausted" ? "budget" : outcome === "unavailable" ? "backend" : "stale");
  expect(h.counts().actions).toBe(0);
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
});

for (const change of ["downgrade", "epoch", "tab", "cancel", "permission", "human gate"] as const) test(`authority rechecked before click after ${change}`, async () => {
  const h = await harness(); await h.classify(); await h.started();
  await h.request("agent_decide_request_v1");
  if (change === "downgrade") await h.update(s => patchJob(s, jobID, { access_mode: "assisted" }));
  if (change === "epoch") await h.update(s => patchJob(s, jobID, { generic_drive_epoch: { ...localEpoch, ordinal: 1 } }));
  if (change === "tab") await h.update(s => patchJob(s, jobID, { tab_id: 78 }));
  if (change === "cancel") await h.update(s => ({ ...s, activeJobs: [] }));
  if (change === "permission") h.setPermission(false);
  if (change === "human gate") h.win.document.body.insertAdjacentHTML("beforeend", '<input type="password">');
  let clicks = 0; h.win.document.querySelector("button")!.addEventListener("click", () => clicks++);
  await h.decide("decision", "c1"); await h.settle();
  expect(clicks).toBe(0);
  expect(h.downloads.started).toHaveLength(0);
});

test("concurrent classification and replay cannot start a second loop or duplicate an unchanged effect", async () => {
  const h = await harness();
  await Promise.all([h.classify(), h.classify()]); await h.started();
  await h.decide("decision", "c1"); await until(() => h.counts().actions === 1); await flush();
  const after = h.frames.length;
  await h.classify(); await h.tick();
  await h.decide("decision", "c1", after); await h.settle();
  expect(h.counts().actions).toBe(1);
  expect(h.frames.filter(f => f.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.filter(f => f.type === "provider_outcome")).toHaveLength(1);
});

test("an old loop cannot park or rewrite the job after a connection generation changes", async () => {
  const h = await harness(); await h.classify(); await h.started();
  await h.request("agent_decide_request_v1");
  const generation = Reflect.get(h.bridge, "portGeneration") + 1;
  Reflect.set(h.bridge, "portGeneration", generation);
  Reflect.set(h.bridge, "helloAckGeneration", generation);
  await h.decide("decision", "c1");
  await h.settle(); // Historical settlement may release only the old permit.
  expect(h.counts().actions).toBe(0);
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.generic_terminal).not.toBe(true);
  expect(h.backend.store.activeJobs[0]?.status).toBe("accepted");
});

test("persisted attempt survives reload and refuses to re-observe or click", async () => {
  const first = await harness(); await first.classify(); await first.started(); await first.decide("unavailable"); await first.settle();
  const seed = JSON.parse(JSON.stringify(first.backend.store)) as StoreShape;
  // Simulate a daemon reoffer retaining the exact tuple in a new worker.
  seed.activeJobs = [{ ...seed.activeJobs[0]!, tab_id: tabID, status: "accepted" }];
  const second = await harness({ seed }); await second.classify(); await flush();
  expect(second.counts().observations).toBe(0);
  expect(second.counts().actions).toBe(0);
  expect(second.frames.some(f => f.type === "agent_decide_request_v1")).toBe(false);
});

test("a disabled PDF button retains tracking and its tab until delayed onCreated, without a BLOCKED decision", async () => {
  const h = await harness();
  const button = h.win.document.querySelector("button")!;
  button.textContent = "Download PDF";
  button.addEventListener("click", () => { button.disabled = true; });
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  await until(() => h.counts().actions === 1); await flush();
  for (let i = 0; i < 5; i++) await h.tick();
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.some(f => f.type === "provider_outcome" || f.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(h.tabs.snapshot(tabID)).toBeDefined();
  expect(Reflect.get(h.bridge, "downloads").get(jobID).generic.epoch).toEqual(localEpoch);
  await h.downloads.onCreated.emit({ id: 902, tabId: tabID, url: "https://unregistered.example/generated", state: "in_progress" });
  await h.tick();
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBe(902);
  expect(h.frames.some(f => f.type === "provider_outcome" || f.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(Reflect.get(h.bridge, "agentLoops").size).toBe(0);
});

test("download grace has an explicit timeout and no repeat click", async () => {
  const h = await harness(); h.win.document.querySelector("button")!.textContent = "Download PDF";
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  await until(() => h.counts().actions === 1); await flush();
  h.advance(45_000); await h.tick(); await h.settle();
  expect(h.counts().actions).toBe(1);
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("timed out waiting");
});

test("owned provider lease spans model latency while the loop retains the ten-minute resource ceiling", async () => {
  const h = await harness(); await h.classify(); await h.started();
  await h.request("agent_decide_request_v1");
  h.advance(61_000); // Beyond the ordinary provider lease.
  await h.decide("decision", "WAIT");
  await h.tick();
  expect(h.counts().observations).toBe(2);
  const requests = h.frames.filter(f => f.type === "agent_decide_request_v1");
  await until(() => h.frames.filter(f => f.type === "agent_decide_request_v1").length === 2);
  h.advance(10 * 60_000);
  const pending = h.frames.filter(f => f.type === "agent_decide_request_v1")[1]!;
  await h.reply(pending, "agent_decide_result_v1", { observation_revision: (pending.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "c1" });
  await h.settle();
  expect(h.counts().actions).toBe(0);
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("budget");
});

test("worker restart retains an armed pending producer without observing or clicking again", async () => {
  const first = await harness(); first.win.document.querySelector("button")!.textContent = "Download PDF";
  await first.classify(); await first.started(); await first.decide("decision", "c1");
  await until(() => first.counts().actions === 1); await flush();
  const seed = JSON.parse(JSON.stringify(first.backend.store)) as StoreShape;
  const second = await harness({ seed });
  await Reflect.get(second.bridge, "reconcileGenericDownloads").call(second.bridge);
  await second.classify();
  expect(second.counts().observations).toBe(0);
  expect(second.counts().actions).toBe(0);
  await second.downloads.onCreated.emit({ id: 904, tabId: tabID, url: "https://unregistered.example/generated", state: "in_progress" });
  expect(second.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBe(904);
  expect(Reflect.get(second.bridge, "downloads").get(jobID).generic.epoch).toEqual(localEpoch);
  expect(second.frames.some(f => f.type === "provider_outcome")).toBe(false);
});

test("PDF-labelled menu expands immediately and its Download PDF control uses pending grace", async () => {
  const h = await harness();
  const menu = h.win.document.querySelector("button")!;
  menu.textContent = "PDF options";
  menu.setAttribute("aria-haspopup", "menu");
  menu.addEventListener("click", () => {
    h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button" role="menuitem">Download PDF</button>');
  });
  await h.classify(); await h.started();
  const first = await h.decide("decision", "c1");
  await until(() => h.counts().actions === 1); await flush(); await h.tick();
  const second = await h.request("agent_decide_request_v1", h.frames.indexOf(first) + 1);
  const observation = second.payload["observation"] as { revision: string; controls: { id: string; label: string }[] };
  const download = observation.controls.find(c => c.label.startsWith("Download PDF"))!;
  expect(download).toBeDefined();
  await h.reply(second, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: download.id });
  await until(() => h.counts().actions === 2); await flush();
  for (let i = 0; i < 3; i++) await h.tick();
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(2);
  await h.downloads.onCreated.emit({ id: 906, tabId: tabID, url: "https://unregistered.example/generated", state: "in_progress" });
  await h.tick();
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBe(906);
  expect(h.frames.some(f => f.type === "provider_outcome")).toBe(false);
});
