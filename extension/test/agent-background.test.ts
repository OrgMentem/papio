// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";
import { Bridge, MIN_DAEMON_VERSION, assessDrivenPage, isBotChallenge, type BridgeDeps, type DownloadItemLike, type NativePort } from "../src/background";
import { agentDOM, agentPageReadiness, type AgentDOMRequest } from "../src/agent-dom";
import { nativeDownloadDocumentCurrent, NATIVE_CLICK_ADOPTION_FEATURE } from "../src/native-download";
import { AGENT_NAVIGATION_FEATURE, parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { planExecution, planGeneric } from "../src/plan";
import { emptyStore, patchJob, migrateManagedState, type ActiveJob, type StoreShape } from "../src/state";
import { FakeDownloads } from "./fake-downloads";
import { ChromeTabsFake, FakeEmitter, FakeWebNavigation } from "./fake-tabs";

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

async function harness(options: { features?: string[]; knownAdapter?: boolean; firefox?: boolean; ignoredSteeringEvent?: boolean; status?: ActiveJob["status"]; seed?: StoreShape; helloPending?: boolean; readiness?: boolean; page?: { url: string; html: string; doi: string }; genericCandidates?: { strategy_id: string; strategy_version: string; url: string }[] } = {}) {
  const pageURL = options.page?.url ?? url, pageDOI = options.page?.doi ?? doi;
  const win = new Window({ url: pageURL, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
  win.document.write(options.page?.html ?? `<meta name="citation_doi" content="${doi}"><meta name="citation_title" content="Example article"><main><h1>Example article</h1><button type="button">Formats</button></main><header><input type="search" value="PRIVATEQUERY"></header>`);
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
  tabs.seed({ id: tabID, url: pageURL, status: "complete" });
  const downloads = new FakeDownloads();
  if (options.firefox && !options.ignoredSteeringEvent) Reflect.deleteProperty(downloads, "onDeterminingFilename");
  const backend = { store: emptyStore(), load: async () => backend.store, save: async (store: StoreShape) => { backend.store = store; } };
  let permitted = true, observations = 0, actions = 0, genericPlans = 0, menuChecks = 0;
  let onAct: (() => Promise<void>) | undefined;
  let afterAct: (() => Promise<void>) | undefined;
  let onMenuCheck: (() => Promise<void>) | undefined;
  const deps: BridgeDeps = {
    webNavigation: new FakeWebNavigation(),
    firefox: options.firefox ?? false,
    connectNative: () => port, manifestVersion: "0.1.0", randomUUID: () => crypto.randomUUID(), now: () => now,
    setTimeout: (fn, ms) => timers.push({ fn, ms }), backend, tabs, downloads,
    adapterSpecs: options.knownAdapter ? [{ id: "test-unknown", version: "1", hosts: ["unregistered.example"], classify: [] }] : [],
    scripting: { executeScript: async injection => {
      if (injection.func === planGeneric) { genericPlans++; return [{ result: { evidence: [], candidates: options.genericCandidates ?? [] } }]; }
      if (injection.func === planExecution) {
        const [, spec, expected, policy] = injection.args as Parameters<typeof planExecution>;
        return [{ result: planExecution(win.document as unknown as Document, spec, expected, policy) }];
      }
      if (injection.func === nativeDownloadDocumentCurrent) return [{ result: nativeDownloadDocumentCurrent(...injection.args as [string, string]) }];
      if (injection.func === agentPageReadiness) return options.readiness ? [{ result: agentPageReadiness() }] : [];
      if (injection.func !== agentDOM) return [];
      const request = injection.args![0] as AgentDOMRequest;
      if (request.method === "observe") observations++;
      else if (request.method === "check_menu") { menuChecks++; await onMenuCheck?.(); }
      else if (request.method === "act") { actions++; await onAct?.(); }
      const result = await agentDOM(request);
      if (request.method === "act") await afterAct?.();
      return [{ result }];
    } },
    permissions: { contains: async () => permitted },
    settings: { getTermsConsent: async () => undefined, setTermsConsent: async () => {}, getHandoffSurface: async () => "in-window", getInPageToast: async () => false },
    action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {} },
    alarms: { create: () => {}, onAlarm: new FakeEmitter<[{ name: string }]>(), },
  };
  const bridge = new Bridge(deps);
  const inbound = async (type: BrowserMessage["type"], payload: Record<string, unknown>, scoped: boolean | string = true) => port.onMessage.emit({
    protocol: "papio-browser/1", type, msg_id: `agent-test-${seq}`, seq: seq++, ...(scoped ? { job_id: typeof scoped === "string" ? scoped : jobID } : {}), payload,
  });
  backend.store = options.seed ?? emptyStore();
  await bridge.start();
  if (!options.helloPending) await inbound("hello_ack", { daemon_version: MIN_DAEMON_VERSION, role: "holder", browser_holder_generation: 1, features: options.features ?? features }, false);
  const update = (reducer: (store: StoreShape) => StoreShape): Promise<void> => Reflect.get(bridge, "update").call(bridge, reducer);
  const job: ActiveJob = { job_id: jobID, tab_id: tabID, offered_at: now - 20_000, expires_at: now + 3600_000,
    status: options.status ?? "accepted", provider_hosts: [], access_mode: "delegated", expected: { doi: pageDOI }, generic_drive_epoch: localEpoch,
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
  return { bridge, deps, backend, frames, win, tabs, downloads, timers, classify, started, decide, request, reply, settle, tick, update, now: () => now,
    counts: () => ({ observations, actions, genericPlans, menuChecks }), setPermission: (value: boolean) => { permitted = value; }, setOnAct: (fn: () => Promise<void>) => { onAct = fn; },
    setAfterAct: (fn: () => Promise<void>) => { afterAct = fn; }, setOnMenuCheck: (fn: () => Promise<void>) => { onMenuCheck = fn; }, advance: (ms: number) => { now += ms; }, inbound };
}

test("a Duo prompt preserves the sign-in wait without consuming an article-agent attempt", async () => {
  const h = await harness({ status: "auth_pending" });
  const promptURL = "https://api-a1b2c3d4.duosecurity.com/prompt/EXAMPLE";
  h.win.location.href = promptURL;
  h.win.document.head.innerHTML = "<title>Login</title>";
  h.win.document.body.innerHTML = "<main><h1>Check for a Duo Push</h1><button>Other options</button></main>";
  h.tabs.patch(tabID, { url: promptURL, status: "complete" });
  await h.tabs.onUpdated.emit(tabID, { url: promptURL, status: "complete" }, h.tabs.snapshot(tabID)!);
  await flush();
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  expect(h.counts()).toEqual({ observations: 0, actions: 0, genericPlans: 0, menuChecks: 0 });
  expect(h.frames.filter(frame => ["provider_drive_epoch_start_request", "agent_decide_request_v1", "provider_outcome", "page_capture", "auth_returned"].includes(frame.type))).toEqual([]);
});

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

test("a delayed candidate refresh cannot re-park an authorized no-adapter materialization", async () => {
  const h = await harness({ features: [...features, "institutional_materialization_v1", "authentication_claim_v1", "handoff_link_v1"] });
  const candidateID = "candidate_agent_refresh";
  const claimID = "claim_agent_refresh";
  const bindingID = "binding_agent_refresh";
  const expiresAt = "2030-01-01T00:00:00Z";
  // Start at a bound scaffold, still waiting for a daemon-issued route. No
  // injected handoff drive: the real materialization path must establish it.
  Reflect.get(h.bridge, "handoffDrives").clear();
  await h.update(store => ({ ...store,
    activeJobs: store.activeJobs.map(job => ({ ...job, status: "queued", requires_auth: true,
      engagement_required: true, fresh_handoff: true })),
    materializations: { [jobID]: { job_id: jobID, candidate_id: candidateID, claim_id: claimID,
      binding_id: bindingID, materialization_kind: "browser_tab", candidate_expires_at: expiresAt,
      lease_until: expiresAt, browser_holder_generation: 1, phase: "bound", tab_id: tabID } },
  }));
  await h.classify();
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request")).toBe(false);

  Reflect.get(h.bridge, "scheduleMaterialization").call(h.bridge, jobID);
  const route = await h.request("institutional_route_request");
  let releaseAlarm!: () => void;
  const alarmRead = new Promise<void>(resolve => { releaseAlarm = resolve; });
  let alarmStarted = false;
  h.deps.alarms.get = async () => { alarmStarted = true; await alarmRead; return undefined; };
  // Candidate notifications run off the inbound FIFO. Hold this one across
  // navigation and its acknowledgement, just as a slow chrome.alarms read can.
  await h.inbound("institutional_candidate_offer", {
    candidate_id: candidateID, materialization_kind: "browser_tab", expires_at: expiresAt,
    provider_hosts: ["unregistered.example"], expected: { doi }, access_mode: "delegated",
    requires_auth: true, drive_attempt_id: epoch.drive_attempt_id, drive_ordinal: epoch.ordinal,
    drive_strategy: "generic", drive_revision: epoch.revision,
  });
  await until(() => alarmStarted);
  await h.reply(route, "institutional_route_response", {
    outcome: "issued", claim_id: claimID, binding_id: bindingID, route_issuance_ordinal: 1,
    effect_ordinal: 1, institutional_request_id: route.payload["institutional_request_id"], url,
  });
  const navigated = await h.request("institutional_navigated_request");
  expect(h.backend.store.activeJobs[0]).toMatchObject({ status: "accepted", engagement_required: false });
  await h.reply(navigated, "institutional_navigated_response", {
    outcome: "acknowledged", claim_id: claimID, binding_id: bindingID,
  });
  await until(() => h.backend.store.materializations?.[jobID]?.phase === "navigated" &&
    Reflect.get(h.bridge, "materializationRuns").size === 0);
  releaseAlarm();
  await flush();
  expect(h.backend.store.activeJobs[0]).toMatchObject({ status: "accepted", engagement_required: false,
    requires_auth: true, fresh_handoff: true, tab_id: tabID });
  expect(Reflect.get(h.bridge, "handoffDrives").has(jobID)).toBe(true);

  await h.tabs.completeNavigation(tabID);
  await h.started();
  await h.decide("decision", "BLOCKED");
  await h.settle();
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.some(frame => frame.type === "auth_returned" || frame.type === "session_evidence" || frame.type === "claim_observation")).toBe(false);
  expect(h.counts().actions).toBe(0);
  expect(h.tabs.created).toHaveLength(0);
});

async function queuedMaterialization(knownAdapter = false) {
  const h = await harness({ knownAdapter, features: [...features, "institutional_materialization_v1", "authentication_claim_v1", "handoff_link_v1"] });
  const occupierID = "job_occupying_drive";
  const claimID = "claim_queued_article", bindingID = "binding_queued_article";
  const expiresAt = "2030-01-01T00:00:00Z";
  Reflect.get(h.bridge, "handoffDrives").clear();
  h.tabs.seed({ id: 78, url: "https://other.example/article", status: "complete" });
  await h.update(store => ({ ...store,
    activeJobs: [
      { ...store.activeJobs[0]!, status: "queued", requires_auth: true, engagement_required: true, fresh_handoff: true },
      { job_id: occupierID, tab_id: 78, status: "accepted", offered_at: h.now(), expires_at: h.now() + 3600_000, provider_hosts: ["other.example"], access_mode: "delegated" },
    ],
    materializations: { [jobID]: { job_id: jobID, candidate_id: "candidate_queued_article", claim_id: claimID,
      binding_id: bindingID, materialization_kind: "browser_tab", candidate_expires_at: expiresAt,
      lease_until: expiresAt, browser_holder_generation: 1, phase: "bound", tab_id: tabID } },
  }));
  if (!knownAdapter) await h.update(store => ({ ...store, activeJobs: store.activeJobs.map(job => {
    if (job.job_id !== jobID) return job;
    const fresh = { ...job };
    delete fresh.unknown_count; delete fresh.last_unknown_ms;
    return fresh;
  }) }));
  Reflect.get(h.bridge, "registerHandoffDrive").call(h.bridge, occupierID, 78);
  Reflect.get(h.bridge, "scheduleMaterialization").call(h.bridge, jobID);
  const route = await h.request("institutional_route_request");
  await h.reply(route, "institutional_route_response", { outcome: "issued", claim_id: claimID, binding_id: bindingID,
    route_issuance_ordinal: 1, effect_ordinal: 1, institutional_request_id: route.payload["institutional_request_id"], url });
  const navigated = await h.request("institutional_navigated_request");
  await h.reply(navigated, "institutional_navigated_response", { outcome: "acknowledged", claim_id: claimID, binding_id: bindingID });
  await until(() => h.backend.store.materializations?.[jobID]?.phase === "navigated" && Reflect.get(h.bridge, "materializationRuns").size === 0);
  expect(Reflect.get(h.bridge, "handoffDrives").has(occupierID)).toBe(true);
  expect(Reflect.get(h.bridge, "handoffDrives").has(jobID)).toBe(false);
  expect(Reflect.get(h.bridge, "queuedDriveJobIDs").has(jobID)).toBe(true);
  const classifyTick = async () => {
    const pending = h.timers.filter(timer => timer.ms === 2500);
    for (const timer of pending) h.timers.splice(h.timers.indexOf(timer), 1);
    h.advance(5000);
    for (const timer of pending) await timer.fn();
    await flush();
  };
  return { ...h, classifyTick, releaseOccupier: () => h.inbound("cancel", {}, occupierID) };
}

for (const knownAdapter of [false, true]) test(`occupied-slot materialization ${knownAdapter ? "known unknown" : "no-adapter"} waits, then observes and downloads without renavigation`, async () => {
  const h = await queuedMaterialization(knownAdapter);
  await h.tabs.completeNavigation(tabID);
  h.advance(5000);
  await h.classify();
  expect(h.frames.filter(frame => frame.type === "provider_outcome")).toHaveLength(0);
  expect(h.backend.store.activeJobs.find(job => job.job_id === jobID)?.parked_with_tab).not.toBe(true);
  expect(h.counts()).toMatchObject({ genericPlans: 0, observations: 0, actions: 0 });
  if (!knownAdapter) expect(h.backend.store.activeJobs.find(job => job.job_id === jobID)?.last_unknown_ms).toBeUndefined();
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request" || frame.type === "agent_decide_request_v1")).toBe(false);
  h.win.document.querySelector("button")!.textContent = "Download PDF after queue";
  // A browser lookup can yield after the occupier releases its slot. The
  // landing must still wait until the queued drive is actually registered.
  const getTab = h.tabs.get.bind(h.tabs);
  let resumeLookup!: () => void, lookupStarted = false;
  const lookup = new Promise<void>(resolve => { resumeLookup = resolve; });
  h.tabs.get = async id => {
    if (id === tabID && !lookupStarted) { lookupStarted = true; await lookup; }
    return getTab(id);
  };
  const released = h.releaseOccupier();
  await until(() => lookupStarted);
  await h.classify();
  expect(h.frames.filter(frame => frame.type === "provider_outcome")).toHaveLength(0);
  expect(h.counts()).toMatchObject({ genericPlans: 0, observations: 0, actions: 0 });
  resumeLookup(); await released;
  await h.classifyTick(); await h.classifyTick();
  await h.started();
  const decision = await h.request("agent_decide_request_v1");
  expect(decision.payload["observation"]).toMatchObject({ controls: [{ label: "Download PDF after queue [Example article]" }] });
  h.setOnAct(async () => {
    expect(Reflect.get(h.bridge, "handoffDrives").size).toBe(1);
    expect(Reflect.get(h.bridge, "handoffDrives").has(jobID)).toBe(true);
    expect(Reflect.get(h.bridge, "downloads").get(jobID).generic.epoch).toEqual(localEpoch);
    await h.downloads.onCreated.emit({ id: 901, tabId: tabID, url: "https://unregistered.example/paper", state: "in_progress" });
  });
  await h.decide("decision", "c1");
  await until(() => h.backend.store.activeJobs.find(job => job.job_id === jobID)?.generic_drive_epoch?.in_flight_download_id === 901);
  let suggested: string | undefined;
  await h.downloads.onDeterminingFilename.emit({ id: 901, tabId: tabID, url: "https://unregistered.example/paper", filename: "paper.pdf" }, value => { suggested = value.filename; });
  expect(suggested).toContain(`papio/${jobID}/`);
  h.downloads.items.set(901, { id: 901, tabId: tabID, filename: `/tmp/papio/${jobID}/paper.pdf`, fileSize: 123, mime: "application/pdf", state: "complete" });
  const completing = h.downloads.onChanged.emit({ id: 901, state: { current: "complete" } });
  await h.settle(); await completing;
  expect(h.frames.find(frame => frame.type === "download_complete")?.payload["producer"]).toEqual({ effect_kind: "generic_drive", ...epoch });
  expect(h.counts().actions).toBe(1);
  expect(h.tabs.navigations).toEqual([{ tabID, url }]);
  expect(h.tabs.created).toHaveLength(0);
});

for (const stop of ["cancel", "disconnect"] as const) test(`occupied-slot materialization has no late agent work after ${stop}`, async () => {
  const h = await queuedMaterialization();
  await h.tabs.completeNavigation(tabID);
  if (stop === "cancel") await h.inbound("cancel", {});
  else await Reflect.get(h.bridge, "port").onDisconnect.emit();
  await h.releaseOccupier();
  await h.classifyTick(); await h.classifyTick();
  expect(h.counts()).toMatchObject({ genericPlans: 0, observations: 0, actions: 0 });
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request" || frame.type === "agent_decide_request_v1" || frame.type === "download_complete")).toBe(false);
  expect(h.tabs.navigations).toEqual([{ tabID, url }]);
  expect(h.downloads.started).toHaveLength(0);
});

test("occupied-slot materialization still reports a persistent provider challenge", async () => {
  const h = await queuedMaterialization(true);
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => {
    if (injection.func === assessDrivenPage) return [{ result: { kind: "challenge" } }];
    if (injection.func === isBotChallenge) return [{ result: true }];
    return execute(injection);
  };
  await h.tabs.completeNavigation(tabID);
  const confirmations = h.timers.filter(timer => timer.ms === 8000);
  expect(confirmations.length).toBeGreaterThan(0);
  h.advance(8000);
  for (const timer of confirmations) await timer.fn();
  await flush();
  expect(h.backend.store.activeJobs.find(job => job.job_id === jobID)?.challenge_blocked).toBe(true);
  expect(Reflect.get(h.bridge, "queuedDriveJobIDs").has(jobID)).toBe(false);
  await h.releaseOccupier(); await h.classifyTick();
  expect(h.frames.some(frame => frame.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().actions).toBe(0);
});

test("a candidate refresh preserves an authentication gate observed during its alarm lookup", async () => {
  const h = await harness({ features: [...features, "institutional_materialization_v1"] });
  const candidateID = "candidate_agent_auth_refresh";
  const expiresAt = "2030-01-01T00:00:00Z";
  await h.update(store => ({ ...store,
    materializations: { [jobID]: { job_id: jobID, candidate_id: candidateID,
      binding_id: "binding_agent_auth_refresh", materialization_kind: "browser_tab",
      candidate_expires_at: expiresAt, phase: "navigated", tab_id: tabID } },
  }));
  let releaseAlarm!: () => void;
  const alarmRead = new Promise<void>(resolve => { releaseAlarm = resolve; });
  let alarmStarted = false;
  h.deps.alarms.get = async () => { alarmStarted = true; await alarmRead; return undefined; };
  await h.inbound("institutional_candidate_offer", {
    candidate_id: candidateID, materialization_kind: "browser_tab", expires_at: expiresAt,
    provider_hosts: ["unregistered.example"], expected: { doi }, access_mode: "delegated",
    requires_auth: true, drive_attempt_id: epoch.drive_attempt_id, drive_ordinal: epoch.ordinal,
    drive_strategy: "generic", drive_revision: epoch.revision,
  });
  await until(() => alarmStarted);
  // A real gate wins over the earlier accepted snapshot just as an issued
  // route wins over an earlier queued snapshot. A refresh grants no authority.
  await h.update(store => patchJob(store, jobID, { status: "auth_pending", auth_started_ms: 1_700_000_000_000,
    engagement_required: true, needs_terms_consent: true, challenge_blocked: true }));
  releaseAlarm();
  await flush();
  expect(h.backend.store.activeJobs[0]).toMatchObject({ status: "auth_pending", auth_started_ms: 1_700_000_000_000,
    engagement_required: true, needs_terms_consent: true, challenge_blocked: true, requires_auth: true });
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request" || frame.type === "agent_decide_request_v1")).toBe(false);
});

for (const mode of ["old", "firefox", "permission"] as const) test(`fallback does not start with ${mode} capability gap`, async () => {
  const h = await harness({ ...(mode === "old" ? { features: ["provider_drive_epoch_v1", "effect_permit_v1"] } : {}), firefox: mode === "firefox" });
  if (mode === "permission") h.setPermission(false);
  await h.classify(); await flush();
  expect(h.frames.some(frame => frame.type === "agent_decide_request_v1" || frame.type === "provider_drive_epoch_start_request")).toBe(false);
  expect(h.counts().actions).toBe(0);
});

test("a delegated fallback waits for the current port's hello before reporting drift", async () => {
  const h = await harness({ helloPending: true });
  const classify = h.classify();
  h.advance(200);
  await flush();
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
  await h.inbound("hello_ack", { daemon_version: MIN_DAEMON_VERSION, role: "holder", browser_holder_generation: 1, features }, false);
  await classify;
  await h.started();
  await h.request("agent_decide_request_v1");
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
});

test("an unacknowledged port reports hello_pending only after its bounded hello wait", async () => {
  const h = await harness({ helloPending: true });
  await h.classify();
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
  const timeout = h.timers.find(timer => timer.ms === 5000);
  expect(timeout).toBeDefined();
  h.advance(5000);
  timeout!.fn();
  await flush();
  const outcomes = h.frames.filter(frame => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]!.payload["outcome"]).toBe("ui_changed");
  expect(outcomes[0]!.payload["detail"]).toContain("Article agent skipped: this browser has not received a hello acknowledgement");
  expect(h.frames.some(frame => frame.type === "agent_decide_request_v1")).toBe(false);
});

test("a delegated fallback refuses an acknowledged pending browser", async () => {
  const h = await harness();
  await h.inbound("hello_ack", { daemon_version: MIN_DAEMON_VERSION, role: "pending", features }, false);
  await h.classify();
  const outcomes = h.frames.filter(frame => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]!.payload["outcome"]).toBe("ui_changed");
  expect(outcomes[0]!.payload["detail"]).toContain("Article agent skipped: this browser lacks authority for the attempt.");
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request" || frame.type === "agent_decide_request_v1")).toBe(false);
});

for (const knownAdapter of [false, true]) for (const [reason, message] of [
  ["backend_feature_missing", "Article agent unavailable: reconnect to a daemon with Jev enabled."],
  ["generic_epoch_missing", "Article agent skipped: the daemon has not authorized a fresh browser attempt."],
  ["expected_doi_missing", "Article agent skipped: this attempt has no DOI."],
  ["access_mode_not_delegated", "Article agent skipped: this paper needs your own sign-in or download, so the autonomous agent stays off."],
] as const) test(`${knownAdapter ? "known unknown" : "no-adapter"} reports safe fallback skip: ${reason}`, async () => {
  const h = await harness({ knownAdapter,
    ...(reason === "backend_feature_missing" ? { features: features.filter(feature => feature !== "agent_fallback_v1") } : {}),
  });
  if (reason === "generic_epoch_missing") await h.update(s => ({ ...s, activeJobs: s.activeJobs.map(job => {
    const current = { ...job }; delete current.generic_drive_epoch; return current;
  }) }));
  if (reason === "expected_doi_missing") await h.update(s => patchJob(s, jobID, { expected: { title: "PRIVATEEXPECTED" } }));
  if (reason === "access_mode_not_delegated") await h.update(s => patchJob(s, jobID, { access_mode: "assisted" }));
  await h.classify(); await flush();
  const outcomes = h.frames.filter(frame => frame.type === "provider_outcome");
  expect(outcomes).toHaveLength(1);
  expect(outcomes[0]!.payload["outcome"]).toBe("ui_changed");
  const detail = String(outcomes[0]!.payload["detail"]);
  expect(detail).toContain(message);
  // The daemon retains only 200 bytes of provider detail.
  expect(new TextDecoder().decode(new TextEncoder().encode(detail).slice(0, 200))).toContain(message);
  expect(detail).not.toContain("PRIVATE"); expect(detail).not.toContain(url); expect(detail).not.toContain(doi);
  expect(h.counts().genericPlans).toBe(1);
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request" || frame.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().observations).toBe(0); expect(h.counts().actions).toBe(0);
  expect(h.downloads.started).toHaveLength(0);
});

for (const knownAdapter of [false, true]) test(`${knownAdapter ? "known unknown" : "no-adapter"} keeps an already-running agent instead of parking`, async () => {
  const h = await harness({ knownAdapter });
  await h.classify(); await h.started();
  await h.request("agent_decide_request_v1");
  // A repeated classification while the daemon decision is pending must not
  // turn the active loop into a refused start or emit an unknown outcome.
  await h.classify(); await flush();
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
  await h.decide("decision", "BLOCKED"); await h.settle();
});

for (const outcome of ["unavailable", "exhausted", "stale"] as const) test(`${outcome} returns a specific operator detail and settles the epoch`, async () => {
  const h = await harness(); await h.classify(); await h.started(); await h.decide(outcome); await h.settle();
  const detail = h.frames.find(frame => frame.type === "provider_outcome")?.payload["detail"];
  expect(detail).toContain(outcome === "exhausted" ? "budget" : outcome === "unavailable" ? "backend" : "stale");
  expect(h.counts().actions).toBe(0);
  expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
});

for (const firefox of [false, true]) for (const reason of ["identity_missing", "identity_conflicting", "identity_invalid"] as const)
  test(`${firefox ? "Firefox" : "Chrome"} ${reason} stops before any decision or action without asserting a human gate`, async () => {
    const h = await harness({ firefox, features: [...features, NATIVE_CLICK_ADOPTION_FEATURE], status: "auth_pending" });
    if (reason === "identity_missing") h.win.document.querySelector('meta[name="citation_doi"]')!.remove();
    if (reason === "identity_conflicting") h.win.document.head.insertAdjacentHTML("beforeend", '<meta name="dc.identifier" content="doi:10.9999/PRIVATEOTHER">');
    if (reason === "identity_invalid") await h.update(s => patchJob(s, jobID, { expected: { doi: "PRIVATEINVALID" } }));
    await h.classify(); await h.started(); await h.settle();
    const settlement = h.frames.find(f => f.type === "provider_drive_epoch_result_request")!;
    const outcome = h.frames.find(f => f.type === "provider_outcome")!;
    expect(settlement.payload["outcome"]).toBe("unknown");
    expect(outcome.payload["outcome"]).toBe("ui_changed");
    expect(outcome.payload["detail"]).toBe(settlement.payload["detail"]);
    expect(outcome.payload["detail"]).toContain(`[${reason}]`);
    expect(outcome.payload["detail"]).toContain("identity");
    expect(outcome.payload["detail"]).not.toContain("human gate");
    expect(JSON.stringify(h.frames)).not.toContain("PRIVATE");
    expect(h.frames.some(f => ["agent_decide_request_v1", "native_download_arm_request_v1", "auth_returned", "session_evidence", "claim_observation"].includes(f.type))).toBe(false);
    expect(h.counts().observations).toBe(1); expect(h.counts().actions).toBe(0);
    expect(h.downloads.started).toHaveLength(0);
    const retained = h.backend.store.activeJobs[0]!;
    expect(retained.challenge_blocked).not.toBe(true); expect(retained.needs_terms_consent).not.toBe(true);
    expect(retained.requires_auth).not.toBe(true); expect(retained.engagement_required).not.toBe(true);
    expect(Reflect.get(h.bridge, "effectGovernorOwner")).toBeUndefined();
  });
// An aggregator page that states no DOI (measured on ProQuest, EBSCO and
// Informit record pages) still cannot identify the work itself. When its one
// explicit PDF link is the only eligible control, the worker lets the model
// take only that download; the daemon's byte validation decides identity.
test("an identity-missing page with one explicit PDF link proceeds to exactly that download", async () => {
  const pageURL = "https://unregistered.example/record/abc";
  const h = await harness({ page: { url: pageURL,
    html: `<main><h1>Example article</h1><a href="https://unregistered.example/record/abc.pdf">Download PDF</a><button type="button">Export citation</button></main>`,
    doi } });
  await h.classify(); await h.started();
  const frame = await h.request("agent_decide_request_v1");
  const observation = frame.payload["observation"] as { revision: string; controls: { id: string; label: string; disabled: boolean }[] };
  expect(observation.controls.filter(c => !c.disabled).map(c => c.label)).toEqual(["Download PDF [Example article]"]);
  h.setOnAct(async () => {
    await h.downloads.onCreated.emit({ id: 917, tabId: tabID, url: "https://unregistered.example/record/abc.pdf", state: "in_progress" });
  });
  await h.reply(frame, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: observation.controls[0]!.id });
  await until(() => h.counts().actions === 1);
  await flush();
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBe(917);
  expect(h.counts().observations).toBe(1);
  expect(h.frames.some(frame => frame.type === "provider_outcome" || frame.type === "provider_drive_epoch_result_request")).toBe(false);
});

test("an identity-missing page with a foreign DOI still refuses its PDF link", async () => {
  const pageURL = "https://unregistered.example/record/abc";
  const h = await harness({ page: { url: pageURL,
    html: `<meta name="citation_doi" content="10.9999/other"><main><h1>Example article</h1><a href="https://unregistered.example/record/abc.pdf">Download PDF</a></main>`,
    doi } });
  await h.classify(); await h.started(); await h.settle();
  const detail = h.frames.find(frame => frame.type === "provider_outcome")?.payload["detail"];
  expect(detail).toContain("[identity_conflicting]");
  expect(h.frames.some(frame => frame.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().actions).toBe(0);
  expect(h.downloads.started).toHaveLength(0);
});

// The cookie-check shell pmc.ncbi.nlm.nih.gov served ~1 s after navigation on
// 2026-09-23 (607 bytes, no DOI), reduced to its public markup.
const cookieShell = '<div id="cookie-required" class="cookie-required-message" hidden><h1>Cookies must be enabled</h1><p>Enable cookies for <span id="cookie-domain">unregistered.example</span> and reload this page to continue.</p></div>';
/** Serve the shell until the harness clock passes `readyAfterMs`, then the article. */
function servesShell(h: AgentHarness, readyAfterMs?: number) {
  const article = { head: h.win.document.head.innerHTML, body: h.win.document.body.innerHTML };
  h.win.document.head.innerHTML = "<title>unregistered.example</title>";
  h.win.document.body.innerHTML = cookieShell;
  let readyAt = readyAfterMs === undefined ? Infinity : h.now() + readyAfterMs;
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => {
    if (h.now() >= readyAt) {
      readyAt = Infinity;
      h.win.document.head.innerHTML = article.head; h.win.document.body.innerHTML = article.body;
    }
    return execute(injection);
  };
}
/** Advance the harness clock through the loop's one-second waits, however
 * many it takes, until it asks for a decision or settles its epoch. */
async function runToFirstOutcome(h: AgentHarness) {
  const done = () => h.frames.some(f => f.type === "agent_decide_request_v1" || f.type === "provider_drive_epoch_result_request");
  for (let i = 0; i < 20 && !done(); i++) {
    await until(() => done() || h.timers.some(timer => timer.ms === 1000));
    if (!done()) await h.tick();
  }
}

test("a cookie-check shell that becomes the article 500 ms after navigation proceeds to a decision", async () => {
  const h = await harness({ readiness: true }); servesShell(h, 500);
  await h.classify(); await h.started(); await runToFirstOutcome(h);
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(true);
  expect(h.frames.some(f => f.type === "provider_outcome" || f.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(h.counts().observations).toBe(1);
});

test("a shell still present at the first observation is re-observed once after the settle", async () => {
  const h = await harness({ readiness: true }); servesShell(h, 1500);
  await h.classify(); await h.started(); await runToFirstOutcome(h);
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(true);
  expect(h.counts().observations).toBe(2);
});

test("a page that stays a tiny shell still records identity_missing after one re-observation", async () => {
  const h = await harness({ readiness: true }); servesShell(h);
  await h.classify(); await h.started(); await runToFirstOutcome(h); await h.settle();
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("[identity_missing]");
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().observations).toBe(2); expect(h.counts().actions).toBe(0);
});

for (const urlChanged of [false, true]) test(`a full page without a DOI is re-observed ${urlChanged ? "only because its URL changed" : "never"}`, async () => {
  const h = await harness({ readiness: true });
  h.win.document.querySelector('meta[name="citation_doi"]')!.remove();
  h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<p>${"Article text. ".repeat(400)}</p>`);
  await h.classify(); await h.started();
  if (urlChanged) h.tabs.seed({ id: tabID, url: `${url}?cookie=1`, status: "complete" });
  await runToFirstOutcome(h); await h.settle();
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("[identity_missing]");
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().observations).toBe(urlChanged ? 2 : 1);
});

// Measured 2026-09-23: Elsevier served its refusal page in place of four
// articles, the agent read it as identity_missing, and the daemon latched
// sciencedirect drift. The provider refused the browser; nothing drifted.
test("a provider refusal page that persists ends the agent as rate_limited, not identity_missing drift", async () => {
  const refusal = new Window({ url });
  refusal.document.write(readFileSync(new URL("../fixtures/sciencedirect/blocked.html", import.meta.url), "utf8"));
  const h = await harness();
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => injection.func === assessDrivenPage
    ? [{ result: assessDrivenPage(h.win.document as unknown as Document) }] : execute(injection);
  await h.classify();
  // The provider swaps the article for its refusal page once the drive starts.
  h.win.document.head.innerHTML = refusal.document.head.innerHTML;
  h.win.document.body.innerHTML = refusal.document.body.innerHTML;
  await h.started();
  await until(() => h.timers.some(timer => timer.ms === 8000) || h.frames.some(f => f.type === "provider_drive_epoch_result_request"));
  const confirmation = h.timers.findIndex(timer => timer.ms === 8000);
  if (confirmation >= 0) { h.advance(8000); h.timers.splice(confirmation, 1)[0]!.fn(); }
  await h.settle();
  await until(() => h.frames.some(f => f.type === "provider_outcome"));
  const outcomes = h.frames.filter(f => f.type === "provider_outcome");
  expect(outcomes.map(f => f.payload["outcome"])).toEqual(["rate_limited"]);
  expect(outcomes[0]!.payload["host"]).toBe("unregistered.example");
  expect(String(outcomes[0]!.payload["detail"])).not.toContain("identity_missing");
  expect(h.backend.store.challengeCooldowns).toEqual({ "unregistered.example": h.now() + 600_000 });
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().actions).toBe(0);
});

for (const [html, reason, message] of [
  ['<input type="password" value="PRIVATESECRET">', "credentials_required", "credential gate"],
  ['<iframe title="CAPTCHA PRIVATESECRET"></iframe>', "challenge_required", "CAPTCHA challenge"],
  ['<dialog open>Accept PRIVATESECRET terms</dialog>', "consent_required", "consent or permission dialog"],
] as const) test(`standard DC identity exposes the actual ${reason} gate, without page content`, async () => {
  const h = await harness();
  h.win.document.querySelector('meta[name="citation_doi"]')!.setAttribute("name", "dc.identifier");
  h.win.document.querySelector('meta[name="dc.identifier"]')!.setAttribute("content", `doi:${doi}`);
  h.win.document.body.insertAdjacentHTML("beforeend", html);
  await h.classify(); await h.started(); await h.settle();
  const detail = h.frames.find(f => f.type === "provider_outcome")!.payload["detail"];
  expect(detail).toContain(`[${reason}]`); expect(detail).toContain(message);
  expect(JSON.stringify(h.frames)).not.toContain("PRIVATESECRET");
  expect(h.frames.some(f => f.type === "agent_decide_request_v1")).toBe(false);
  expect(h.counts().actions).toBe(0);
});

for (const reason of ["identity_missing", "identity_conflicting", "consent_required", "page_binding_failed"] as const)
  test(`pre-click refusal reports ${reason} after a model decision`, async () => {
    const h = await harness(); await h.classify(); await h.started(); await h.request("agent_decide_request_v1");
    let clicks = 0; h.win.document.querySelector("button")!.addEventListener("click", () => clicks++);
    if (reason === "identity_missing") h.win.document.querySelector('meta[name="citation_doi"]')!.remove();
    if (reason === "identity_conflicting") h.win.document.head.insertAdjacentHTML("beforeend", '<meta name="prism.doi" content="10.9999/PRIVATEOTHER">');
    if (reason === "consent_required") h.win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept PRIVATECONSENT</dialog>');
    if (reason === "page_binding_failed") h.tabs.seed({ id: tabID, url: "https://unregistered.example/PRIVATEOTHER", status: "complete" });
    await h.decide("decision", "c1"); await h.settle();
    expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain(`[${reason}]`);
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(clicks).toBe(0); expect(h.downloads.started).toHaveLength(0);
    expect(JSON.stringify(h.frames)).not.toContain("PRIVATE");
  });

test("backend BLOCKED is reported as a decision, not proof of a DOM human gate", async () => {
  const h = await harness(); await h.classify(); await h.started(); await h.decide("decision", "BLOCKED"); await h.settle();
  const detail = h.frames.find(f => f.type === "provider_outcome")?.payload["detail"];
  expect(detail).toContain("decision backend reported BLOCKED");
  expect(detail).not.toContain("human gate"); expect(h.counts().actions).toBe(0);
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

test("concurrent classification and an unchanged effect get bounded rechecks without a second paid decision", async () => {
  const h = await harness();
  await Promise.all([h.classify(), h.classify()]); await h.started();
  await h.decide("decision", "c1"); await until(() => h.counts().actions === 1); await flush();
  await h.classify();
  for (let i = 0; i < 2; i++) {
    await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  }
  await h.tick(); await h.settle();
  expect(h.counts().actions).toBe(1);
  expect(h.counts().observations).toBe(4);
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.filter(f => f.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.filter(f => f.type === "provider_outcome")).toHaveLength(1);
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("no observable article change");
  expect(h.frames.find(f => f.type === "provider_drive_epoch_result_request")?.payload["detail"]).toContain("no observable article change");
  expect(Reflect.get(h.bridge, "downloads").has(jobID)).toBe(false);
});

test("a menu that renders during the bounded rechecks gets a fresh decision and exact download tracking", async () => {
  const h = await harness(); await h.classify(); await h.started();
  const first = await h.decide("decision", "c1");
  await until(() => h.counts().actions === 1); await flush();
  for (let i = 0; i < 2; i++) {
    await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  }
  h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button">Download PDF</button>');
  await h.tick();
  const second = await h.request("agent_decide_request_v1", h.frames.indexOf(first) + 1);
  const observation = second.payload["observation"] as { revision: string; controls: { id: string; label: string }[] };
  expect(observation.revision).not.toBe((first.payload["observation"] as { revision: string }).revision);
  const download = observation.controls.find(c => c.label.startsWith("Download PDF"))!;
  h.setOnAct(async () => { await h.downloads.onCreated.emit({ id: 908, tabId: tabID, url: "https://unregistered.example/generated", state: "in_progress" }); });
  await h.reply(second, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: download.id });
  await until(() => h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id === 908);
  expect(h.counts().actions).toBe(2);
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(2);
  expect(h.frames.some(f => f.type === "provider_outcome" || f.type === "provider_drive_epoch_result_request")).toBe(false);
});

for (const change of ["permission", "navigation", "document", "gate", "generation", "cancel"] as const)
  test(`bounded no-progress rechecks retain the ${change} guard`, async () => {
    const h = await harness(); await h.classify(); await h.started(); await h.decide("decision", "c1");
    await until(() => h.counts().actions === 1); await flush();
    await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    if (change === "permission") h.setPermission(false);
    if (change === "navigation") h.tabs.seed({ id: tabID, url: url + "/other", status: "complete" });
    if (change === "document") Reflect.deleteProperty(globalThis, "papioArticleAgent");
    if (change === "gate") h.win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept PRIVATECONSENT</dialog>');
    if (change === "generation") {
      const generation = Reflect.get(h.bridge, "portGeneration") + 1;
      Reflect.set(h.bridge, "portGeneration", generation); Reflect.set(h.bridge, "helloAckGeneration", generation);
    }
    if (change === "cancel") await h.update(store => ({ ...store, activeJobs: [] }));
    await h.tick(); await h.settle();
    expect(h.counts().actions).toBe(1);
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(f => f.type === "download_complete")).toBe(false);
    if (change === "generation" || change === "cancel") expect(h.frames.some(f => f.type === "provider_outcome")).toBe(false);
    const detail = h.frames.find(f => f.type === "provider_drive_epoch_result_request")?.payload["detail"];
    expect(detail).not.toContain("PRIVATECONSENT");
    if (change === "gate") expect(detail).toContain("consent_required");
    if (change === "document") expect(detail).toContain("document_changed");
    if (change === "navigation") expect(detail).toContain("page_binding_failed");
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

const nativeFeatures = [...features, NATIVE_CLICK_ADOPTION_FEATURE];
const reservationID = "native-reservation-1";
const sourcePath = "C:\\Users\\Researcher\\Downloads\\Article paper (2).pdf";
type AgentHarness = Awaited<ReturnType<typeof harness>>;
function firefoxItem(h: AgentHarness, patch: Partial<DownloadItemLike> = {}): DownloadItemLike {
  // Firefox DownloadItem has no tabId, finalUrl or DOM documentId.
  return { id: 1401, url: "https://unregistered.example/one-use?token=PRIVATE", referrer: url,
    startTime: new Date(h.now()).toISOString(), incognito: false, cookieStoreId: "firefox-default",
    state: "in_progress", exists: true, filename: sourcePath, fileSize: -1, ...patch };
}
async function nativeHarness(options: { ignoredSteeringEvent?: boolean } = {}) {
  const h = await harness({ firefox: true, features: nativeFeatures, ...options });
  h.win.document.querySelector("button")!.textContent = "Download PDF";
  return h;
}
async function nativeArm(h: AgentHarness) {
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  const arm = await h.request("native_download_arm_request_v1");
  expect(h.counts().observations).toBe(1); expect(h.counts().actions).toBe(0);
  expect(arm.payload["producer"]).toEqual({ effect_kind: "generic_drive", ...epoch });
  await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
  await until(() => h.counts().actions === 1); await flush();
  return arm;
}
function completeNative(h: AgentHarness, item: DownloadItemLike, patch: Partial<DownloadItemLike> = {}) {
  h.downloads.items.set(item.id, { ...item, state: "complete", fileSize: 12345, mime: "application/pdf", ...patch });
  return h.downloads.onChanged.emit({ id: item.id, state: { current: "complete" } });
}
function expectNativeUntouched(h: AgentHarness) {
  expect(h.downloads.started).toHaveLength(0); expect(h.downloads.removedFiles).toHaveLength(0); expect(h.downloads.erased).toHaveLength(0);
  expect(h.frames.some(f => f.type === "download_complete" || f.type === "download_started" || f.type === "auth_returned" || f.type === "session_evidence")).toBe(false);
}

for (const outcome of ["ready", "review", "rejected"] as const) test(`Firefox synchronous native click imports exact actual file before ${outcome} settlement`, async () => {
  const h = await nativeHarness();
  let item: DownloadItemLike | undefined, receipt: Promise<void> | undefined, clicks = 0;
  h.win.document.querySelector("button")!.addEventListener("click", () => {
    clicks++; item = firefoxItem(h); receipt = h.downloads.onCreated.emit(item);
    expect(Reflect.get(h.bridge, "downloads").get(jobID).ids.has(item.id)).toBe(true);
  });
  const arm = await nativeArm(h); await receipt;
  const completing = completeNative(h, item!, outcome === "rejected" ? { mime: "text/html" } : {});
  const request = await h.request("native_download_import_request_v1");
  expect(request.payload).toMatchObject({ reservation_id: reservationID,
    browser_epoch: arm.payload["browser_epoch"], document_id: arm.payload["document_id"],
    download_id: item!.id, source_path: sourcePath, size_bytes: 12345,
    started_at_ms: Date.parse(item!.startTime!), producer: { effect_kind: "generic_drive", ...epoch } });
  expect(h.downloads.searches.filter(query => query.id === item!.id)).toEqual([{ id: item!.id }]);
  expect(h.downloads.searches.some(query => query.filename !== undefined || query.filenameRegex !== undefined)).toBe(false);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(Reflect.get(h.bridge, "downloads").has(jobID)).toBe(true);
  await h.reply(request, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item!.id, outcome }); await completing;
  expect(Reflect.get(h.bridge, "downloads").has(jobID)).toBe(false);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(clicks).toBe(1); expect(h.counts().actions).toBe(1); expectNativeUntouched(h);
  const saved = JSON.stringify(h.backend.store);
  expect(saved).not.toContain("PRIVATE"); expect(saved).not.toContain(sourcePath); expect(saved).not.toContain(url);
});

test("Firefox identity overrides an exposed ignored filename event", async () => {
  const h = await nativeHarness({ ignoredSteeringEvent: true }); await nativeArm(h);
  const item = firefoxItem(h); await h.downloads.onCreated.emit(item);
  expect(h.backend.store.activeJobs[0]?.native_download?.download_id).toBe(item.id);
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.in_flight_download_id).toBeUndefined(); expectNativeUntouched(h);
});

test("Firefox delayed creation holds grace and never asks for another decision", async () => {
  const h = await nativeHarness();
  h.win.document.querySelector("button")!.addEventListener("click", () => { h.win.document.querySelector("button")!.disabled = true; });
  await nativeArm(h); for (let i = 0; i < 5; i++) await h.tick();
  const item = firefoxItem(h); await h.downloads.onCreated.emit(item); await h.tick();
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.native_download).toMatchObject({ download_id: item.id, phase: "observed" });
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false); expectNativeUntouched(h);
});

test("Firefox menu then download uses two fresh DOM choices and a single reservation", async () => {
  const h = await nativeHarness(); const button = h.win.document.querySelector("button")!;
  button.textContent = "Formats"; let clicks = 0;
  button.addEventListener("click", () => {
    clicks++; button.disabled = true;
    const download = h.win.document.createElement("button"); download.type = "button"; download.textContent = "Download PDF";
    download.addEventListener("click", () => { clicks++; void h.downloads.onCreated.emit(firefoxItem(h)); }); button.after(download);
  });
  await nativeArm(h); const after = h.frames.length; await h.tick(); await h.decide("decision", "c2", after);
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
  expect(clicks).toBe(2); expect(h.counts().actions).toBe(2);
  expect(h.frames.filter(f => f.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false); expectNativeUntouched(h);
});

for (const variant of ["absent", "origin", "wrong", "query", "old-start", "future-start", "other-extension", "private", "container", "blob"] as const)
  test(`Firefox refuses ${variant} provenance without guessing or replay`, async () => {
    const h = await nativeHarness(); await nativeArm(h);
    const patch: Partial<DownloadItemLike> = variant === "absent" ? { referrer: undefined } :
      variant === "origin" ? { referrer: "https://unregistered.example/" } : variant === "wrong" ? { referrer: url + "/different" } :
      variant === "query" ? { referrer: url + "?other=1" } : variant === "old-start" ? { startTime: new Date(h.now() - 1).toISOString() } :
      variant === "future-start" ? { startTime: new Date(h.now() + 1000).toISOString() } : variant === "other-extension" ? { byExtensionId: "someone@example.org" } :
      variant === "private" ? { incognito: true } : variant === "container" ? { cookieStoreId: "firefox-container-2" } : { url: "blob:https://unregistered.example/opaque" };
    const item = firefoxItem(h, patch); await h.downloads.onCreated.emit(item);
    expect(h.backend.store.activeJobs[0]?.native_download?.download_id).toBeUndefined();
    await completeNative(h, item);
    expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false);
    h.advance(45_000); await h.tick(); await h.settle(); expect(h.counts().actions).toBe(1); expectNativeUntouched(h);
  });

for (const stage of ["before-arm", "after-dispatch"] as const) test(`Firefox refuses same-article second tab ${stage}`, async () => {
  const h = await nativeHarness();
  if (stage === "before-arm") {
    h.tabs.seed({ id: 78, url, status: "complete" }); await h.classify(); await h.started(); await h.decide("decision", "c1"); await h.settle();
    expect(h.frames.some(f => f.type === "native_download_arm_request_v1")).toBe(false); expect(h.counts().actions).toBe(0);
  } else {
    await nativeArm(h); h.tabs.seed({ id: 78, url, status: "complete" });
    const item = firefoxItem(h); await h.downloads.onCreated.emit(item); await completeNative(h, item);
    expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false);
  }
  expectNativeUntouched(h);
});

test("Firefox refuses two jobs bound to the same article tab", async () => {
  const h = await nativeHarness(); await h.update(s => ({ ...s, activeJobs: [...s.activeJobs, { ...s.activeJobs[0]!, job_id: "job_other_article" }] }));
  await h.classify(); await h.started(); await h.decide("decision", "c1"); await h.settle();
  expect(h.counts().actions).toBe(0); expect(h.frames.some(f => f.type === "native_download_arm_request_v1")).toBe(false);
});

for (const change of ["document", "epoch", "generation", "holder", "cancel", "permission", "expired"] as const)
  test(`Firefox refuses completion after ${change} authority changes`, async () => {
    const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h); await h.downloads.onCreated.emit(item);
    if (change === "document") Reflect.set(globalThis, "papioArticleAgent", undefined);
    if (change === "epoch") await h.update(s => patchJob(s, jobID, { generic_drive_epoch: { ...localEpoch, ordinal: 1 } }));
    if (change === "generation") { const next = Reflect.get(h.bridge, "portGeneration") + 1; Reflect.set(h.bridge, "portGeneration", next); Reflect.set(h.bridge, "helloAckGeneration", next); }
    if (change === "holder") Reflect.set(h.bridge, "lastKnownBrowserHolderGeneration", 2);
    if (change === "cancel") await h.inbound("cancel", {});
    if (change === "permission") h.setPermission(false); if (change === "expired") h.advance(120_001);
    await completeNative(h, item); expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false);
    expect(h.counts().actions).toBe(1); expectNativeUntouched(h);
  });

for (const change of ["document", "permission", "generation", "holder", "epoch", "cancel"] as const)
  test(`Firefox does not dispatch after ${change} during arm`, async () => {
    const h = await nativeHarness(); await h.classify(); await h.started(); await h.decide("decision", "c1"); const arm = await h.request("native_download_arm_request_v1");
    if (change === "document") Reflect.set(globalThis, "papioArticleAgent", undefined); if (change === "permission") h.setPermission(false);
    if (change === "generation") { const next = Reflect.get(h.bridge, "portGeneration") + 1; Reflect.set(h.bridge, "portGeneration", next); Reflect.set(h.bridge, "helloAckGeneration", next); }
    if (change === "epoch") await h.update(s => patchJob(s, jobID, { generic_drive_epoch: { ...localEpoch, ordinal: 1 } }));
    if (change === "cancel") await h.update(s => ({ ...s, activeJobs: [] }));
    if (change === "holder") Reflect.set(h.bridge, "lastKnownBrowserHolderGeneration", 2);
    await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 }); await h.settle();
    expect(h.counts().actions).toBe(0); expectNativeUntouched(h);
  });

for (const duplicate of ["different-id", "same-id-changed-start", "same-id-changed-url"] as const)
  test(`Firefox refuses ambiguous ${duplicate} creation`, async () => {
    const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h); await h.downloads.onCreated.emit(item); h.advance(1);
    const patch = duplicate === "different-id" ? { id: 1402 } : duplicate === "same-id-changed-start"
      ? { startTime: new Date(h.now()).toISOString() } : { url: "https://unregistered.example/other" };
    await h.downloads.onCreated.emit({ ...item, ...patch }); await completeNative(h, item);
    expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false); expectNativeUntouched(h);
  });

for (const invalid of ["missing", "size", "relative", "interrupted", "search-id", "changed-start", "changed-referrer"] as const)
  test(`Firefox refuses ${invalid} exact completion record without deleting the source`, async () => {
    const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h); await h.downloads.onCreated.emit(item);
    if (invalid === "interrupted") await h.downloads.onChanged.emit({ id: item.id, state: { current: "interrupted" } });
    else {
      if (invalid === "search-id") h.downloads.search = async () => [{ ...item, id: 44, state: "complete", fileSize: 12345 }];
      const patch: Partial<DownloadItemLike> = invalid === "missing" ? { exists: false } : invalid === "size" ? { fileSize: -1, totalBytes: 12345 } :
        invalid === "relative" ? { filename: "Article paper (2).pdf" } : invalid === "changed-start" ? { startTime: new Date(h.now() - 1000).toISOString() } :
        invalid === "changed-referrer" ? { referrer: url + "?changed=1" } : {};
      await completeNative(h, item, patch);
    }
    expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false); expectNativeUntouched(h);
  });

test("Firefox duplicate complete/deferred retains exactly one import and never replays", async () => {
  const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h);
  await h.downloads.onCreated.emit(item); await h.downloads.onCreated.emit({ ...item });
  const first = completeNative(h, item); const request = await h.request("native_download_import_request_v1"); await completeNative(h, item);
  await h.reply(request, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item.id, outcome: "deferred", reason: "validation_pending" });
  await first; await completeNative(h, item); await h.classify();
  expect(h.frames.filter(f => f.type === "native_download_import_request_v1")).toHaveLength(1);
  expect(h.backend.store.activeJobs[0]?.native_download).toMatchObject({ phase: "deferred", download_id: item.id });
  expect(h.counts().actions).toBe(1); expectNativeUntouched(h);
});

test("Firefox mismatched import reply cannot retire the exact track", async () => {
  const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h); await h.downloads.onCreated.emit(item);
  const completing = completeNative(h, item); const request = await h.request("native_download_import_request_v1");
  await h.reply(request, "native_download_import_result_v1", { reservation_id: "different-reservation", download_id: item.id, outcome: "ready" }); await completing;
  expect(Reflect.get(h.bridge, "downloads").has(jobID)).toBe(true); expect(h.backend.store.activeJobs[0]?.generic_terminal).not.toBe(true);
  expect(h.backend.store.activeJobs[0]?.native_download?.phase).toBe("deferred"); expectNativeUntouched(h);
});

test("Firefox worker restart preserves a URL-free receipt but never searches or binds its reused ID", async () => {
  const first = await nativeHarness(); await nativeArm(first); const item = firefoxItem(first); await first.downloads.onCreated.emit(item);
  const seed = migrateManagedState({ version: 8, ...JSON.parse(JSON.stringify(first.backend.store)) });
  expect(seed.activeJobs[0]?.native_download?.download_id).toBe(item.id);
  const second = await harness({ firefox: true, features: nativeFeatures, seed }); await second.classify();
  await second.downloads.onCreated.emit(firefoxItem(second)); await completeNative(second, firefoxItem(second));
  expect(second.downloads.searches.some(query => query.id === item.id)).toBe(false); expect(second.counts().actions).toBe(0);
  expect(second.frames.some(f => f.type === "native_download_arm_request_v1" || f.type === "native_download_import_request_v1")).toBe(false); expectNativeUntouched(second);
});

for (const outcome of ["refused", "unavailable", "stale"] as const) test(`Firefox ${outcome} arm never dispatches`, async () => {
  const h = await nativeHarness(); await h.classify(); await h.started(); await h.decide("decision", "c1");
  const arm = await h.request("native_download_arm_request_v1");
  await h.reply(arm, "native_download_arm_result_v1", { outcome, reason: "unavailable" }); await h.settle();
  expect(h.counts().actions).toBe(0); expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false); expectNativeUntouched(h);
});

test("Firefox completion waits for a pending creation provenance check", async () => {
  const h = await nativeHarness(); await nativeArm(h);
  const original = h.deps.scripting.executeScript;
  let release!: () => void, blocked = false;
  const gate = new Promise<void>(resolve => { release = resolve; });
  h.deps.scripting.executeScript = async injection => {
    if (injection.func === nativeDownloadDocumentCurrent && !blocked) { blocked = true; await gate; }
    return original(injection);
  };
  const item = firefoxItem(h); const creating = h.downloads.onCreated.emit(item);
  await until(() => blocked);
  const completing = completeNative(h, item); await flush();
  expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false);
  release(); await creating;
  const request = await h.request("native_download_import_request_v1");
  await h.reply(request, "native_download_import_result_v1", { outcome: "review", reservation_id: reservationID, download_id: item.id });
  await completing; expectNativeUntouched(h);
});

test("Firefox late import result cannot mark a newer generic tuple terminal", async () => {
  const h = await nativeHarness(); await nativeArm(h); const item = firefoxItem(h); await h.downloads.onCreated.emit(item);
  const completing = completeNative(h, item); const request = await h.request("native_download_import_request_v1");
  await h.update(s => patchJob(s, jobID, { generic_drive_epoch: { ...localEpoch, ordinal: 1 } }));
  await h.reply(request, "native_download_import_result_v1", { outcome: "ready", reservation_id: reservationID, download_id: item.id }); await completing;
  expect(h.backend.store.activeJobs[0]?.generic_terminal).not.toBe(true);
  expect(h.backend.store.activeJobs[0]?.generic_drive_epoch?.ordinal).toBe(1); expectNativeUntouched(h);
});

test("Firefox explicit PDF link retains native adoption through delayed browser creation", async () => {
  const h = await nativeHarness();
  h.win.document.querySelector("main")!.innerHTML = '<a href="/download/paper.pdf?ticket=PRIVATE">Article PDF</a>';
  const anchor = h.win.document.querySelector("a")!;
  let clicks = 0;
  anchor.addEventListener("click", event => {
    clicks++;
    expect(anchor.getAttribute("download")).toBe("");
    expect(h.backend.store.activeJobs[0]?.native_download?.phase).toBe("armed");
    event.preventDefault();
  });
  await nativeArm(h);
  expect(anchor.hasAttribute("download")).toBe(false);
  for (let i = 0; i < 5; i++) await h.tick();
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  const item = firefoxItem(h, { url: "https://unregistered.example/download/paper.pdf?ticket=PRIVATE" });
  await h.downloads.onCreated.emit(item);
  await h.tick();
  const completing = completeNative(h, item);
  const request = await h.request("native_download_import_request_v1");
  expect(request.payload["download_id"]).toBe(item.id);
  expect(request.payload["source_path"]).toBe(sourcePath);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  await h.reply(request, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item.id, outcome: "ready" });
  await completing;
  expect(clicks).toBe(1);
  expectNativeUntouched(h);
});

for (const render of ["inserted", "clipped"] as const) test(`Firefox bare Download progresses to a ${render} PDF choice under one native reservation`, async () => {
  const h = await nativeHarness();
  const main = h.win.document.querySelector("main")!;
  main.innerHTML = `<button>Download</button><section ${render === "clipped" ? 'style="clip-path:inset(50%)"' : ''}>${render === "clipped" ? '<a href="/paper.pdf">Article PDF</a>' : ''}</section>`;
  const trigger = main.querySelector("button")!;
  let clicks = 0;
  trigger.addEventListener("click", () => {
    clicks++;
    const panel = main.querySelector("section")!;
    panel.removeAttribute("style");
    if (render === "inserted") panel.insertAdjacentHTML("beforeend", '<a href="/paper.pdf">Article PDF</a>');
    panel.querySelector("a")!.addEventListener("click", event => {
      clicks++; event.preventDefault(); void h.downloads.onCreated.emit(firefoxItem(h));
    });
  });
  await nativeArm(h);
  const after = h.frames.length;
  await h.tick();
  const request = await h.request("agent_decide_request_v1", after);
  const observation = request.payload["observation"] as { revision: string; controls: { id: string; label: string; disabled: boolean }[] };
  const pdf = observation.controls.find(c => c.label === "Article PDF" && !c.disabled)!;
  expect(pdf).toBeDefined();
  await h.reply(request, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: pdf.id });
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
  expect(clicks).toBe(2);
  expect(h.frames.filter(f => f.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  expectNativeUntouched(h);
});

for (const timing of ["synchronous-with-menu", "delayed-without-menu"] as const)
  test(`Firefox bare Download retains a real ${timing} native download without another decision`, async () => {
    const h = await nativeHarness();
    const trigger = h.win.document.querySelector("button")!;
    trigger.textContent = "Download";
    trigger.addEventListener("click", () => {
      if (timing === "synchronous-with-menu") {
        h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
        void h.downloads.onCreated.emit(firefoxItem(h));
      }
    });
    await nativeArm(h);
    if (timing === "delayed-without-menu") {
      for (let i = 0; i < 5; i++) await h.tick();
      await h.downloads.onCreated.emit(firefoxItem(h));
      await h.tick();
    }
    await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.counts().actions).toBe(1);
    expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
    expectNativeUntouched(h);
  });

test("Firefox asynchronous Download menu resumes after a local grace check, without paid polling", async () => {
  const h = await nativeHarness();
  const trigger = h.win.document.querySelector("button")!;
  trigger.textContent = "Download";
  trigger.addEventListener("click", () => {
    h.deps.setTimeout(() => {
      h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<a href="/paper.pdf">Article PDF</a>');
      h.win.document.querySelector('a[href="/paper.pdf"]')!.addEventListener("click", event => {
        event.preventDefault(); void h.downloads.onCreated.emit(firefoxItem(h));
      });
    }, 2500);
  });
  await nativeArm(h);
  const after = h.frames.length;
  for (let i = 0; i < 2; i++) {
    await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  }
  const render = h.timers.findIndex(timer => timer.ms === 2500);
  expect(render).toBeGreaterThanOrEqual(0);
  h.advance(500); h.timers.splice(render, 1)[0]!.fn();
  await h.tick();
  const request = await h.request("agent_decide_request_v1", after);
  const observation = request.payload["observation"] as { revision: string; controls: { id: string; label: string }[] };
  const pdf = observation.controls.find(c => c.label.startsWith("Article PDF"))!;
  await h.reply(request, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: pdf.id });
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
  expect(h.counts().actions).toBe(2);
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(2);
  expect(h.frames.filter(f => f.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  expectNativeUntouched(h);
});

for (const change of ["navigation", "identity", "permission", "cancel"] as const)
  test(`Firefox async menu settling stops after ${change} without a second decision or click`, async () => {
    const h = await nativeHarness(); h.win.document.querySelector("button")!.textContent = "Download";
    await nativeArm(h); await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
    if (change === "navigation") h.tabs.seed({ id: tabID, url: url + "/different", status: "complete" });
    if (change === "identity") h.win.document.querySelector('meta[name="citation_doi"]')!.setAttribute("content", "10.9999/other");
    if (change === "permission") h.setPermission(false);
    if (change === "cancel") await h.update(store => ({ ...store, activeJobs: [] }));
    await h.tick(); await h.settle();
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.counts().actions).toBe(1);
    expect(h.frames.some(f => f.type === "native_download_import_request_v1")).toBe(false);
    expectNativeUntouched(h);
  });

test("Firefox bare Download with no menu progress keeps its original grace deadline", async () => {
  const h = await nativeHarness(); h.win.document.querySelector("button")!.textContent = "Download";
  await nativeArm(h);
  for (let i = 0; i < 3; i++) {
    await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
    expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  }
  h.advance(41_000); await h.tick(); await h.settle();
  expect(h.frames.find(f => f.type === "provider_outcome")?.payload["detail"]).toContain("timed out waiting");
  expect(h.counts().actions).toBe(1);
});

test("Firefox exact native receipt arriving during an async menu check wins without another decision", async () => {
  const h = await nativeHarness(); h.win.document.querySelector("button")!.textContent = "Download";
  await nativeArm(h);
  const dispatched = h.backend.store.activeJobs[0]?.native_download?.dispatched_at_ms;
  await h.tick(); await until(() => h.timers.some(timer => timer.ms === 1000));
  expect(h.backend.store.activeJobs[0]?.native_download?.dispatched_at_ms).toBe(dispatched);
  const item = firefoxItem(h);
  h.setOnMenuCheck(async () => {
    h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>Download PDF</button>');
    await h.downloads.onCreated.emit(item);
  });
  await h.tick();
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === item.id);
  const completing = completeNative(h, item);
  const request = await h.request("native_download_import_request_v1");
  expect(request.payload["download_id"]).toBe(item.id);
  expect(request.payload["source_path"]).toBe(sourcePath);
  await h.reply(request, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item.id, outcome: "ready" });
  await completing;
  expect(h.counts().actions).toBe(1); expect(h.counts().menuChecks).toBe(2);
  expect(h.frames.filter(f => f.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.some(f => f.type === "provider_drive_epoch_result_request")).toBe(false);
  expectNativeUntouched(h);
});


const navigationURL = "https://unregistered.example/article/full?view=print";
async function navigationHarness(firefox = true) {
  const h = await harness({ firefox, features: [...nativeFeatures, AGENT_NAVIGATION_FEATURE] });
  h.win.document.querySelector("main")!.innerHTML = `<a href="${navigationURL}">Read this work</a>`;
  // Simulate document commits, not an HTML mutation masquerading as a new page.
  const land = async (destination = navigationURL, metadata = `<meta name="citation_doi" content="${doi}">`) => {
    const win = new Window({ url: destination, settings: { enableJavaScriptEvaluation: false, disableCSSFileLoading: true, disableJavaScriptFileLoading: true, disableIframePageLoading: true } });
    win.document.write(`${metadata}<main><button type="button">Download PDF</button></main>`);
    Object.assign(win.HTMLElement.prototype, { getClientRects: () => [{ width: 10, height: 10 }] });
    for (const [key, value] of Object.entries({ document: win.document, location: win.location, getComputedStyle: win.getComputedStyle.bind(win), HTMLElement: win.HTMLElement, papioArticleAgent: undefined })) Object.defineProperty(globalThis, key, { value, writable: true, configurable: true });
    h.tabs.seed({ id: tabID, url: destination, status: "complete" });
    await h.tabs.onUpdated.emit(tabID, { url: destination, status: "complete" }, h.tabs.snapshot(tabID)!);
    return win;
  };
  h.win.document.querySelector("a")!.addEventListener("click", event => event.preventDefault());
  return { ...h, land };
}

test("Firefox observed navigation rebinds the same reservation before a destination decision and native import", async () => {
  const h = await navigationHarness();
  h.setAfterAct(async () => { if (h.counts().actions === 1) await h.land(); });
  await h.classify(); await h.started();
  const first = await h.request("agent_decide_request_v1");
  expect((first.payload["observation"] as { controls: { disabled: boolean }[] }).controls[0]?.disabled).toBe(false);
  await h.decide("decision", "c1");
  const arm = await h.request("native_download_arm_request_v1");
  await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
  const rebound = await h.request("native_download_rebind_request_v1");
  expect(rebound.payload).toMatchObject({ reservation_id: reservationID, producer: arm.payload["producer"], browser_epoch: arm.payload["browser_epoch"], document_id: arm.payload["document_id"] });
  expect(rebound.payload["next_document_id"]).not.toBe(arm.payload["document_id"]);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  await h.reply(rebound, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  const second = await h.request("agent_decide_request_v1", h.frames.indexOf(first) + 1);
  expect(JSON.stringify(h.frames)).not.toContain(navigationURL);
  expect(JSON.stringify(h.backend.store)).not.toContain(navigationURL);
  h.setOnAct(async () => { await h.downloads.onCreated.emit(firefoxItem(h, { referrer: navigationURL })); });
  await h.reply(second, "agent_decide_result_v1", { observation_revision: (second.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "c1" });
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
  const completing = completeNative(h, firefoxItem(h, { referrer: navigationURL }));
  const imported = await h.request("native_download_import_request_v1");
  expect(imported.payload["document_id"]).toBe(rebound.payload["next_document_id"]);
  await h.reply(imported, "native_download_import_result_v1", { reservation_id: reservationID, download_id: 1401, outcome: "ready" });
  await completing;
  expect(h.frames.filter(frame => frame.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_result_request")).toBe(false);
  expect(h.counts().actions).toBe(2);
  expectNativeUntouched(h);
});

for (const timing of ["synchronous", "delayed-loading"] as const) test(`Firefox navigation Content-Disposition receipt ${timing} keeps the source reservation`, async () => {
  const h = await navigationHarness();
  let created: Promise<void> | undefined;
  const item = firefoxItem(h);
  h.win.document.querySelector("a")!.addEventListener("click", () => {
    if (timing === "synchronous") created = h.downloads.onCreated.emit(item);
    else h.tabs.seed({ id: tabID, url, status: "loading" });
  });
  const arm = await nativeArm(h);
  if (timing === "delayed-loading") {
    for (let i = 0; i < 3; i++) await h.tick();
    created = h.downloads.onCreated.emit(item);
    await flush();
    h.tabs.seed({ id: tabID, url, status: "complete" });
    await h.tabs.onUpdated.emit(tabID, { status: "complete" }, h.tabs.snapshot(tabID)!);
    await h.tick();
  }
  await created;
  const completing = completeNative(h, item);
  const imported = await h.request("native_download_import_request_v1");
  expect(imported.payload["document_id"]).toBe(arm.payload["document_id"]);
  expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  await h.reply(imported, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item.id, outcome: "ready" });
  await completing;
  expectNativeUntouched(h);
});

test("synchronous navigation with an unloaded injection reply continues once without replay", async () => {
  const h = await navigationHarness();
  let clicks = 0;
  h.win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  h.setAfterAct(async () => { await h.land(); throw new Error("document unloaded"); });
  await nativeArm(h);
  const rebind = await h.request("native_download_rebind_request_v1");
  expect(clicks).toBe(1);
  await h.reply(rebind, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  const next = await h.request("agent_decide_request_v1", h.frames.indexOf(rebind));
  await h.reply(next, "agent_decide_result_v1", { observation_revision: (next.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "BLOCKED" });
  await h.settle();
  expect(clicks).toBe(1);
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(Reflect.get(h.bridge, "agentNavigations").size).toBe(0);
});

for (const change of ["manual-other", "cross-origin", "wrong-doi", "missing-doi", "consent", "reload", "same-document", "permission", "duplicate-tab"] as const)
  test(`prepared navigation refuses ${change} before rebind or another decision`, async () => {
    const h = await navigationHarness();
    h.setAfterAct(async () => {
      if (change === "same-document") { h.win.location.href = navigationURL; h.tabs.seed({ id: tabID, url: navigationURL, status: "complete" }); return; }
      const destination = change === "manual-other" ? navigationURL + "&manual=1" : change === "cross-origin" ? "https://foreign.example/article" : change === "reload" ? url : navigationURL;
      const metadata = change === "wrong-doi" ? '<meta name="citation_doi" content="10.9999/wrong">' : change === "missing-doi" ? "" : `<meta name="citation_doi" content="${doi}">${change === "consent" ? '<dialog open>Accept the licence terms</dialog>' : ''}`;
      await h.land(destination, metadata);
      if (change === "permission") h.setPermission(false);
      if (change === "duplicate-tab") h.tabs.seed({ id: 78, url: navigationURL, status: "complete" });
    });
    await nativeArm(h); await h.settle();
    expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
    expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.counts().actions).toBe(1);
    expect(Reflect.get(h.bridge, "agentNavigations").size).toBe(0);
    expectNativeUntouched(h);
  });

for (const change of ["source-receipt", "reload", "permission", "refused", "wrong-reservation"] as const)
  test(`navigation rebind ${change} cannot authorize a destination action`, async () => {
    const h = await navigationHarness(); h.setAfterAct(async () => { await h.land(); });
    await nativeArm(h);
    const request = await h.request("native_download_rebind_request_v1");
    let created: Promise<void> | undefined;
    if (change === "source-receipt") created = h.downloads.onCreated.emit(firefoxItem(h));
    if (change === "reload") await h.land();
    if (change === "permission") h.setPermission(false);
    await h.reply(request, "native_download_rebind_result_v1", { reservation_id: change === "wrong-reservation" ? "wrong-reservation" : reservationID, outcome: change === "refused" ? "refused" : "rebound" });
    if (created) { await created; await completeNative(h, firefoxItem(h)); }
    else await h.settle();
    expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(frame => frame.type === "native_download_import_request_v1")).toBe(false);
    expect(h.counts().actions).toBe(1);
    expectNativeUntouched(h);
  });

for (const stop of ["cancel", "disconnect", "replacement", "error", "timeout"] as const)
  test(`navigation settled promise releases source receipt checking on ${stop} during a hanging injection`, async () => {
    const h = await navigationHarness();
    let created: Promise<void> | undefined;
    h.win.document.querySelector("a")!.addEventListener("click", () => { created = h.downloads.onCreated.emit(firefoxItem(h)); });
    let release!: () => void;
    h.setAfterAct(() => new Promise<void>(resolve => { release = resolve; }));
    await nativeArm(h);
    const pending = Reflect.get(h.bridge, "agentNavigations").get(jobID);
    let settled = false; void pending.settled.then(() => { settled = true; });
    if (stop === "cancel") await h.inbound("cancel", {});
    if (stop === "disconnect") await Reflect.get(h.bridge, "port").onDisconnect.emit();
    if (stop === "replacement") await (h.deps.webNavigation as FakeWebNavigation).onTabReplaced.emit({ tabId: 88, replacedTabId: tabID });
    if (stop === "error") await (h.deps.webNavigation as FakeWebNavigation).emitError(tabID, "net::ERR_FAILED");
    if (stop === "timeout") {
      h.advance(45_000);
      const timers = h.timers.filter(timer => timer.ms === 45_000); expect(timers.length).toBeGreaterThan(0); for (const timer of timers) timer.fn();
    }
    await until(() => settled);
    await created;
    await completeNative(h, firefoxItem(h));
    release(); await flush();
    expect(h.frames.some(frame => frame.type === "native_download_import_request_v1" || frame.type === "native_download_rebind_request_v1")).toBe(false);
    expect(Reflect.get(h.bridge, "agentNavigations").size).toBe(0);
    expect(h.counts().actions).toBe(1);
    expectNativeUntouched(h);
  });

test("queued navigation injection cannot click after its original absolute deadline", async () => {
  const h = await navigationHarness();
  let clicks = 0; h.win.document.querySelector("a")!.addEventListener("click", () => clicks++);
  let release!: () => void, actionDeadline = 0;
  const queued = new Promise<void>(resolve => { release = resolve; });
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => {
    const request = injection.args?.[0] as AgentDOMRequest | undefined;
    if (injection.func === agentDOM && request?.method === "act") { actionDeadline = request.actionDeadline!; await queued; }
    return execute(injection);
  };
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  const arm = await h.request("native_download_arm_request_v1");
  await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
  await until(() => actionDeadline > 0);
  h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn();
  await h.settle();
  const originalNow = Date.now;
  try { Date.now = () => actionDeadline + 1; release(); await flush(); }
  finally { Date.now = originalNow; }
  expect(clicks).toBe(0);
  expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
});

for (const receipt of ["destination", "late-source"] as const) test(`Chrome navigation ${receipt === "destination" ? "retains destination filename steering" : "refuses late source filename steering"}`, async () => {
  const h = await navigationHarness(false);
  h.setAfterAct(async () => { if (h.counts().actions === 1) await h.land(); });
  await h.classify(); await h.started();
  const first = await h.decide("decision", "c1");
  const next = await h.request("agent_decide_request_v1", h.frames.indexOf(first) + 1);
  const item = firefoxItem(h, { tabId: tabID, referrer: receipt === "destination" ? navigationURL : url, filename: "paper.pdf" });
  h.setOnAct(async () => { await h.downloads.onCreated.emit(item); });
  await h.reply(next, "agent_decide_result_v1", { observation_revision: (next.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "c1" });
  await until(() => h.counts().actions === 2); await flush();
  let suggested: string | undefined;
  await h.downloads.onDeterminingFilename.emit(item, value => { suggested = value.filename; });
  h.downloads.items.set(item.id, { ...item, filename: `/tmp/papio/${jobID}/paper.pdf`, state: "complete", fileSize: 12345, mime: "application/pdf" });
  const completing = h.downloads.onChanged.emit({ id: item.id, state: { current: "complete" } });
  if (receipt === "destination") {
    expect(suggested).toContain(`papio/${jobID}/`);
    await h.settle(); await completing;
    expect(h.frames.find(frame => frame.type === "download_complete")?.payload["producer"]).toEqual({ effect_kind: "generic_drive", ...epoch });
    expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_result_request")).toHaveLength(1);
  } else { await completing; expect(suggested).toBeUndefined(); expect(h.frames.some(frame => frame.type === "download_complete")).toBe(false); }
  expect(h.frames.some(frame => frame.type.startsWith("native_download_"))).toBe(false);
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.counts().actions).toBe(2);
});

test("source download and a replacement document refuse import and destination progression", async () => {
  const h = await navigationHarness();
  let created: Promise<void> | undefined;
  h.win.document.querySelector("a")!.addEventListener("click", () => { created = h.downloads.onCreated.emit(firefoxItem(h)); });
  h.setAfterAct(async () => { await h.land(); });
  await nativeArm(h); await created;
  await completeNative(h, firefoxItem(h));
  expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1" || frame.type === "native_download_import_request_v1")).toBe(false);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expectNativeUntouched(h);
});

test("timed out rebind ignores a late rebound without a destination decision", async () => {
  const h = await navigationHarness(); h.setAfterAct(async () => { await h.land(); });
  await nativeArm(h);
  const request = await h.request("native_download_rebind_request_v1");
  h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn();
  await h.settle();
  await h.reply(request, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.counts().actions).toBe(1);
  expect(Reflect.get(h.bridge, "agentNavigations").size).toBe(0);
});

test("handler-opened viewer tab cannot run a competing adoption path during navigation", async () => {
  const h = await navigationHarness();
  h.setAfterAct(async () => {
    h.tabs.seed({ id: 88, openerTabId: tabID, url: "https://unregistered.example/hidden.pdf", status: "complete" });
    await h.tabs.onUpdated.emit(88, { status: "complete" }, h.tabs.snapshot(88)!);
  });
  await nativeArm(h);
  h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn();
  await h.settle();
  expect(h.downloads.started).toHaveLength(0);
  expect(h.backend.store.activeJobs[0]?.tab_id).not.toBe(88);
  expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
});

test("menu then navigation retains the armed reservation and original decision budget", async () => {
  const h = await navigationHarness();
  h.win.document.querySelector("main")!.innerHTML = '<button type="button">Share</button>';
  h.win.document.querySelector("button")!.addEventListener("click", () => {
    h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", `<a href="${navigationURL}">Read this work</a>`);
    h.win.document.querySelector("a")!.addEventListener("click", event => { event.preventDefault(); });
  });
  const arm = await nativeArm(h); await h.tick();
  const second = await h.request("agent_decide_request_v1", h.frames.indexOf(arm));
  const observation = second.payload["observation"] as { revision: string; controls: { id: string; label: string }[] };
  h.setAfterAct(async () => { if (h.counts().actions === 2) await h.land(); });
  await h.reply(second, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: observation.controls.find(control => control.label === "Read this work")!.id });
  const request = await h.request("native_download_rebind_request_v1");
  expect(request.payload["reservation_id"]).toBe(reservationID);
  expect(request.payload["document_id"]).toBe(arm.payload["document_id"]);
  await h.reply(request, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  const third = await h.request("agent_decide_request_v1", h.frames.indexOf(second) + 1);
  await h.reply(third, "agent_decide_result_v1", { observation_revision: (third.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "BLOCKED" });
  await h.settle();
  expect(h.frames.filter(frame => frame.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(3);
  expect(h.counts().actions).toBe(2);
});

test("Chrome navigation response can remain a source download without native arm or rebind", async () => {
  const h = await navigationHarness(false);
  const item = firefoxItem(h, { tabId: tabID, filename: "paper.pdf" });
  let created: Promise<void> | undefined;
  h.win.document.querySelector("a")!.addEventListener("click", () => { created = h.downloads.onCreated.emit(item); });
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  await until(() => h.counts().actions === 1); await created;
  let suggestion: string | undefined;
  await h.downloads.onDeterminingFilename.emit(item, value => { suggestion = value.filename; });
  expect(suggestion).toContain(`papio/${jobID}/`);
  h.downloads.items.set(item.id, { ...item, filename: `/tmp/papio/${jobID}/paper.pdf`, state: "complete", fileSize: 123, mime: "application/pdf" });
  const completing = h.downloads.onChanged.emit({ id: item.id, state: { current: "complete" } });
  await h.settle(); await completing;
  expect(h.frames.some(frame => frame.type.startsWith("native_download_"))).toBe(false);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.frames.find(frame => frame.type === "download_complete")?.payload["producer"]).toEqual({ effect_kind: "generic_drive", ...epoch });
});

for (const timing of ["synchronous", "delayed"] as const) test(`navigation anchor with ${timing} JS menu resumes source decision without rebind or repeated click`, async () => {
  const h = await navigationHarness();
  h.win.document.querySelector("a")!.textContent = "Download";
  const insert = () => h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button">Download PDF</button>');
  let clicks = 0;
  h.win.document.querySelector("a")!.addEventListener("click", event => { event.preventDefault(); clicks++; if (timing === "synchronous") insert(); });
  const arm = await nativeArm(h);
  if (timing === "delayed") { await h.tick(); insert(); await h.tick(); }
  const next = await h.request("agent_decide_request_v1", h.frames.indexOf(arm));
  const observation = next.payload["observation"] as { revision: string; controls: { id: string; label: string }[] };
  h.setOnAct(async () => { await h.downloads.onCreated.emit(firefoxItem(h)); });
  await h.reply(next, "agent_decide_result_v1", { observation_revision: observation.revision, outcome: "decision", choice: observation.controls.find(control => control.label === "Download PDF")!.id });
  await until(() => h.backend.store.activeJobs[0]?.native_download?.download_id === 1401);
  const completing = completeNative(h, firefoxItem(h));
  const imported = await h.request("native_download_import_request_v1");
  expect(imported.payload["document_id"]).toBe(arm.payload["document_id"]);
  await h.reply(imported, "native_download_import_result_v1", { reservation_id: reservationID, download_id: 1401, outcome: "ready" });
  await completing;
  expect(clicks).toBe(1);
  expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
  expect(h.frames.filter(frame => frame.type === "native_download_arm_request_v1")).toHaveLength(1);
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(2);
});

for (const change of ["committed", "other-url-then-back", "identity", "consent", "existing-pdf"] as const)
  test(`navigation menu continuation refuses ${change} evidence`, async () => {
    const h = await navigationHarness();
    if (change === "existing-pdf") h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button">Download PDF</button>');
    h.setAfterAct(async () => {
      if (change !== "existing-pdf") h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button type="button">Download PDF</button>');
      if (change === "committed") await (h.deps.webNavigation as FakeWebNavigation).onCommitted.emit({ tabId: tabID, frameId: 0, url: navigationURL });
      if (change === "other-url-then-back") {
        await h.tabs.onUpdated.emit(tabID, { url: navigationURL }, { id: tabID, url: navigationURL, status: "loading" });
        await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, h.tabs.snapshot(tabID)!);
      }
      if (change === "identity") h.win.document.querySelector("meta[name='citation_doi']")!.setAttribute("content", "10.9999/other");
      if (change === "consent") h.win.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept the licence terms</dialog>');
    });
    await h.classify(); await h.started(); await h.decide("decision", "c1");
    const arm = await h.request("native_download_arm_request_v1");
    await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
    await until(() => h.counts().actions === 1); await flush();
    h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn();
    await h.settle();
    expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(frame => frame.type === "native_download_rebind_request_v1")).toBe(false);
    expect(h.counts().actions).toBe(1);
  });

for (const departed of [true, false]) test(`Chrome filename steering ${departed ? "refuses known departure before verification" : "preserves an unchanged source during navigation wait"}`, async () => {
  const h = await navigationHarness(false);
  let dispatchReturned = false;
  h.setAfterAct(async () => {
    if (departed) {
      h.tabs.seed({ id: tabID, url: navigationURL, status: "loading" });
      await h.tabs.onUpdated.emit(tabID, { url: navigationURL, status: "loading" }, h.tabs.snapshot(tabID)!);
    }
    dispatchReturned = true;
  });
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  await until(() => dispatchReturned); await flush();
  const pending = Reflect.get(h.bridge, "agentNavigations").get(jobID);
  expect(pending).toMatchObject({ departed, verifying: false, invalid: false });
  const item = firefoxItem(h, { tabId: tabID, filename: "paper.pdf" });
  let suggestion: string | undefined;
  // Chrome may determine the filename before onCreated. Do not first mark the
  // track ambiguous: steering alone makes this path visible to the sweeper.
  await h.downloads.onDeterminingFilename.emit(item, value => { suggestion = value.filename; });
  h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn();
  await h.settle();
  if (departed) expect(suggestion).toBeUndefined();
  else expect(suggestion).toContain(`papio/${jobID}/`);
  expect(h.frames.some(frame => frame.type === "download_complete")).toBe(false);
});

for (const firefox of [false, true]) for (const timing of ["synchronous", "delayed"] as const) test(`${firefox ? "Firefox" : "Chrome"} agent PDF wrapper saves the exposed file with the original producer (${timing})`, async () => {
  const h = await navigationHarness(firefox);
  const fileURL = "https://unregistered.example/files/article.pdf?token=private";
  const item = firefoxItem(h, { referrer: navigationURL, url: fileURL, ...(firefox ? {} : { filename: "article.pdf" }) });
  let created: Promise<void> | undefined, saves = 0;
  h.setAfterAct(async () => {
    const win = await h.land(navigationURL, "");
    win.document.body.innerHTML = `<iframe src="${fileURL}"></iframe>`;
    win.document.body.addEventListener("click", event => {
      const anchor = event.target as unknown as HTMLAnchorElement;
      if (anchor.tagName !== "A") return;
      event.preventDefault();
      expect(anchor.href).toBe(fileURL); expect(anchor.hasAttribute("download")).toBe(true);
      saves++; if (timing === "synchronous") created = h.downloads.onCreated.emit(item);
    });
  });
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  if (firefox) {
    const arm = await h.request("native_download_arm_request_v1");
    await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
    const rebound = await h.request("native_download_rebind_request_v1");
    expect(saves).toBe(0);
    await h.reply(rebound, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  }
  await until(() => saves === 1);
  if (timing === "delayed") {
    await until(() => h.timers.some(timer => timer.ms === 1000));
    await h.tick();
    expect(h.frames.some(frame => frame.type === "provider_drive_epoch_result_request")).toBe(false);
    created = h.downloads.onCreated.emit(item);
  }
  await created;
  expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
  expect(h.counts().actions).toBe(1);
  if (firefox) {
    const completing = completeNative(h, item);
    const imported = await h.request("native_download_import_request_v1");
    const rebound = h.frames.find(frame => frame.type === "native_download_rebind_request_v1")!;
    expect(imported.payload["document_id"]).toBe(rebound.payload["next_document_id"]);
    await h.reply(imported, "native_download_import_result_v1", { reservation_id: reservationID, download_id: item.id, outcome: "ready" });
    await completing;
    expect(h.frames.some(frame => frame.type === "provider_drive_epoch_result_request")).toBe(false);
  } else {
    let suggestion: string | undefined;
    await h.downloads.onDeterminingFilename.emit(item, value => { suggestion = value.filename; });
    expect(suggestion).toContain(`papio/${jobID}/`);
    h.downloads.items.set(item.id, { ...item, filename: `/tmp/papio/${jobID}/article.pdf`, state: "complete", fileSize: 123, mime: "application/pdf" });
    const completing = h.downloads.onChanged.emit({ id: item.id, state: { current: "complete" } });
    await h.settle(); await completing;
    expect(h.frames.find(frame => frame.type === "download_complete")?.payload["producer"]).toEqual({ effect_kind: "generic_drive", ...epoch });
  }
  expect(JSON.stringify(h.frames)).not.toContain(fileURL);
  expect(JSON.stringify(h.backend.store)).not.toContain(fileURL);
  expect(h.tabs.created).toHaveLength(0);
});

for (const change of ["file", "consent", "permission", "reload", "source-receipt"] as const)
  test(`agent PDF wrapper ${change} during native rebind refuses the save`, async () => {
    const h = await navigationHarness();
    let wrapper: Window | undefined, saves = 0;
    h.setAfterAct(async () => {
      wrapper = await h.land(navigationURL, "");
      wrapper.document.body.innerHTML = '<iframe src="/article.pdf"></iframe>';
      wrapper.document.body.addEventListener("click", event => { if ((event.target as unknown as Element).tagName === "A") { saves++; event.preventDefault(); } });
    });
    await nativeArm(h);
    const request = await h.request("native_download_rebind_request_v1");
    if (change === "file") wrapper!.document.querySelector("iframe")!.src = "https://unregistered.example/other.pdf";
    if (change === "consent") wrapper!.document.body.insertAdjacentHTML("beforeend", '<dialog open>Accept terms</dialog>');
    if (change === "permission") h.setPermission(false);
    if (change === "reload") await h.land(navigationURL, "");
    const created = change === "source-receipt" ? h.downloads.onCreated.emit(firefoxItem(h)) : undefined;
    await h.reply(request, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
    if (created) { await created; await completeNative(h, firefoxItem(h)); }
    else await h.settle();
    expect(saves).toBe(0);
    expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.frames.some(frame => frame.type === "native_download_import_request_v1")).toBe(false);
    expect(Reflect.get(h.bridge, "agentNavigations").size).toBe(0);
  });

for (const stop of ["cancel", "disconnect", "replacement", "commit", "timeout"] as const)
  test(`agent PDF wrapper hanging save releases on ${stop} without replay`, async () => {
    const h = await navigationHarness(false);
    h.setAfterAct(async () => {
      const win = await h.land(navigationURL, "");
      win.document.body.innerHTML = '<iframe src="/article.pdf"></iframe>';
    });
    let started = false;
    const execute = h.deps.scripting.executeScript;
    h.deps.scripting.executeScript = async injection => {
      if (injection.func === agentDOM && (injection.args?.[0] as AgentDOMRequest)?.method === "act_pdf") {
        started = true; return new Promise(() => {});
      }
      return execute(injection);
    };
    await h.classify(); await h.started(); await h.decide("decision", "c1");
    await until(() => started);
    if (stop === "cancel") await h.inbound("cancel", {});
    if (stop === "disconnect") await Reflect.get(h.bridge, "port").onDisconnect.emit();
    if (stop === "replacement") await (h.deps.webNavigation as FakeWebNavigation).onTabReplaced.emit({ tabId: 88, replacedTabId: tabID });
    if (stop === "commit") await (h.deps.webNavigation as FakeWebNavigation).onCommitted.emit({ tabId: tabID, frameId: 0, url: navigationURL });
    if (stop === "timeout") { h.advance(45_000); for (const timer of h.timers.filter(timer => timer.ms === 45_000)) timer.fn(); }
    await until(() => Reflect.get(h.bridge, "agentNavigations").size === 0);
    expect(h.frames.filter(frame => frame.type === "agent_decide_request_v1")).toHaveLength(1);
    expect(h.downloads.started).toHaveLength(0);
  });

for (const firefox of [false, true]) test(`${firefox ? "Firefox" : "Chrome"} agent PDF wrapper invalidates a receipt when commit overtakes its document check`, async () => {
  const h = await navigationHarness(firefox);
  const item = firefoxItem(h, { referrer: navigationURL, url: "https://unregistered.example/article.pdf", ...(firefox ? {} : { filename: "article.pdf" }) });
  let created: Promise<void> | undefined, checkingReceipt = false, checkingDocument = false;
  let releaseDocument!: () => void, releaseAction!: () => void;
  const documentGate = new Promise<void>(resolve => { releaseDocument = resolve; });
  const actionGate = new Promise<void>(resolve => { releaseAction = resolve; });
  h.setAfterAct(async () => {
    const win = await h.land(navigationURL, "");
    win.document.body.innerHTML = '<iframe src="/article.pdf"></iframe>';
    win.document.body.addEventListener("click", event => {
      if ((event.target as unknown as Element).tagName !== "A") return;
      event.preventDefault(); checkingReceipt = true; created = h.downloads.onCreated.emit(item);
    });
  });
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => {
    const result = await execute(injection);
    if (injection.func === nativeDownloadDocumentCurrent && checkingReceipt) { checkingDocument = true; await documentGate; }
    if (injection.func === agentDOM && (injection.args?.[0] as AgentDOMRequest)?.method === "act_pdf") await actionGate;
    return result;
  };
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  if (firefox) {
    const arm = await h.request("native_download_arm_request_v1");
    await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
    const rebind = await h.request("native_download_rebind_request_v1");
    await h.reply(rebind, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  }
  await until(() => checkingDocument);
  await (h.deps.webNavigation as FakeWebNavigation).onCommitted.emit({ tabId: tabID, frameId: 0, url: navigationURL });
  await until(() => Reflect.get(h.bridge, "agentNavigations").size === 0);
  releaseDocument(); await created;
  expect(Reflect.get(h.bridge, "downloads").get(jobID)?.ambiguous).toBe(true);
  releaseAction(); await flush();
  expect(h.frames.some(frame => frame.type === "native_download_import_request_v1" || frame.type === "download_complete")).toBe(false);
});

for (const firefox of [false, true]) test(`${firefox ? "Firefox" : "Chrome"} wrapper freshness retains an invalidated transition after map removal`, async () => {
  const h = await navigationHarness(firefox);
  h.setAfterAct(async () => { const win = await h.land(navigationURL, ""); win.document.body.innerHTML = '<iframe src="/article.pdf"></iframe>'; });
  let reachedSave = false;
  const execute = h.deps.scripting.executeScript;
  h.deps.scripting.executeScript = async injection => {
    if (injection.func === agentDOM && (injection.args?.[0] as AgentDOMRequest)?.method === "act_pdf") { reachedSave = true; return new Promise(() => {}); }
    return execute(injection);
  };
  await h.classify(); await h.started(); await h.decide("decision", "c1");
  if (firefox) {
    const arm = await h.request("native_download_arm_request_v1");
    await h.reply(arm, "native_download_arm_result_v1", { outcome: "armed", reservation_id: reservationID, expires_at_ms: h.now() + 120_000 });
    const rebind = await h.request("native_download_rebind_request_v1");
    await h.reply(rebind, "native_download_rebind_result_v1", { reservation_id: reservationID, outcome: "rebound" });
  }
  await until(() => reachedSave);
  let captured = false, release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  h.deps.scripting.executeScript = async injection => {
    const result = await execute(injection);
    if (injection.func === nativeDownloadDocumentCurrent) { captured = true; await gate; }
    return result;
  };
  const track = Reflect.get(h.bridge, "downloads").get(jobID);
  const pending = Reflect.get(h.bridge, "agentNavigations").get(jobID);
  const checking = Reflect.get(h.bridge, firefox ? "nativeDownloadFresh" : "agentNavigationDownloadFresh").call(h.bridge, jobID, track) as Promise<boolean>;
  await until(() => captured);
  pending.invalid = true;
  Reflect.get(h.bridge, "agentNavigations").delete(jobID);
  release();
  expect(await checking).toBe(false);
  pending.stop();
});

test("agent refreshes a changed pre-click observation without spending or replaying a click", async () => {
  const h = await navigationHarness(false);
  await h.classify(); await h.started();
  const first = await h.request("agent_decide_request_v1");
  h.win.document.querySelector("main")!.insertAdjacentHTML("beforeend", '<button>More formats</button>');
  await h.decide("decision", "c1");
  await until(() => h.timers.some(timer => timer.ms === 1000));
  await h.tick();
  const next = await h.request("agent_decide_request_v1", h.frames.indexOf(first) + 1);
  expect(next.payload["observation"]).not.toEqual(first.payload["observation"]);
  expect(h.counts().actions).toBe(0);
  expect(h.frames.filter(frame => frame.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  expect(h.frames.some(frame => frame.type === "provider_drive_epoch_result_request")).toBe(false);
  await h.reply(next, "agent_decide_result_v1", { observation_revision: (next.payload["observation"] as { revision: string }).revision, outcome: "decision", choice: "BLOCKED" });
  await h.settle();
});

for (const change of ["referrer", "old-time", "future-time", "extension", "holder", "generation", "epoch", "expired", "not-initiated", "ambiguous", "duplicate"] as const)
  test(`Chrome agent receipt without tabId refuses ${change}`, async () => {
    const h = await navigationHarness(false);
    h.setAfterAct(async () => { const win = await h.land(navigationURL, ""); win.document.body.innerHTML = '<iframe src="/article.pdf"></iframe>'; });
    let reached = false;
    const execute = h.deps.scripting.executeScript;
    h.deps.scripting.executeScript = async injection => {
      if (injection.func === agentDOM && (injection.args?.[0] as AgentDOMRequest)?.method === "act_pdf") { reached = true; return [{ result: { status: "dispatched", downloadExpected: true } }]; }
      return execute(injection);
    };
    await h.classify(); await h.started(); await h.decide("decision", "c1");
    await until(() => reached);
    const track = Reflect.get(h.bridge, "downloads").get(jobID);
    const pending = Reflect.get(h.bridge, "agentNavigations").get(jobID);
    const item = firefoxItem(h, { referrer: navigationURL, filename: "article.pdf" });
    if (change === "referrer") item.referrer = "https://unregistered.example/other-article";
    if (change === "old-time") item.startTime = new Date(h.now() - 1).toISOString();
    if (change === "future-time") item.startTime = new Date(h.now() + 1).toISOString();
    if (change === "extension") item.byExtensionId = "other-extension";
    if (change === "holder") Reflect.set(h.bridge, "lastKnownBrowserHolderGeneration", 2);
    if (change === "generation") Reflect.set(h.bridge, "portGeneration", Reflect.get(h.bridge, "portGeneration") + 1);
    if (change === "epoch") await h.update(s => patchJob(s, jobID, { generic_drive_epoch: { ...localEpoch, drive_attempt_id: "other-attempt" } }));
    if (change === "expired") await h.update(s => patchJob(s, jobID, { expires_at: h.now() }));
    if (change === "not-initiated") await h.update(s => patchJob(s, jobID, { download_initiated: false }));
    if (change === "ambiguous") track.ambiguous = true;
    if (change === "duplicate") Reflect.get(h.bridge, "downloads").set("job_other", { ...track, ids: new Set(), agentDocument: { ...track.agentDocument } });
    let suggestion: string | undefined;
    await h.downloads.onDeterminingFilename.emit(item, value => { suggestion = value.filename; });
    expect(suggestion).toBeUndefined();
    expect(item.tabId).toBeUndefined();
    pending.stop(); await flush();
  });

// Measured 2026-09-23 on job_6137e775227b19bb52eb79bf5d: the drive went through
// the OpenAthens redirector (a genuine sign-in hop, so auth_pending), landed on
// this SAGE Research Methods chapter, and stopped with "page or authority became
// stale" 66 ms after its capture. No start reached the daemon. Two sibling UNE
// jobs with the same resolver-only provider key were active, and all drives
// share one effect slot. A busy sibling is contention, not a stale attempt.
const sageURL = "https://methods.sagepub.com/book/edvol/researching-childrens-experience/chpt/researching-child-developmental-psychology";
const sageHTML = readFileSync(new URL("../fixtures/sage-research-methods/success.html", import.meta.url), "utf8");
for (const holder of ["effect slot", "provider lease"] as const)
  test(`an agent drive back from a sign-in hop waits out a sibling's ${holder} and decides on the article`, async () => {
    const h = await harness({ page: { url: sageURL, html: sageHTML, doi: "10.4135/9781849209823.n2" } });
    // Captures blank inline style, so the page's closed Kendo sign-in windows
    // parse as open. Restore the closed state they carry live.
    for (const dialog of h.win.document.querySelectorAll(".k-window")) dialog.setAttribute("style", "display: none");
    const navigate = async (to: string) => {
      h.tabs.patch(tabID, { url: to, status: "loading" });
      await h.tabs.onUpdated.emit(tabID, { url: to, status: "loading" }, h.tabs.snapshot(tabID)!);
      h.tabs.patch(tabID, { url: to, status: "complete" });
      await h.tabs.onUpdated.emit(tabID, { status: "complete" }, h.tabs.snapshot(tabID)!);
      await flush();
    };
    await navigate("https://go.openathens.net/redirector/une.edu.au?url=https%3A%2F%2Fdoi.org%2F10.4135%2F9781849209823.n2");
    expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
    const providerKey = "unknown-provider";
    if (holder === "effect slot") Reflect.set(h.bridge, "effectGovernorOwner", { jobID: "job_sibling", token: "sibling" });
    else {
      Reflect.get(h.bridge, "providerDrainLeaseOwners").set(providerKey, "sibling");
      Reflect.get(h.bridge, "providerDrainLeaseJobs").set(providerKey, "job_sibling");
      await h.update(store => ({ ...store, providerDrainLeases: { [providerKey]: { providerKey, expiresAt: h.now() + 60_000 } } }));
    }
    await navigate(sageURL);
    await until(() => h.timers.some(timer => timer.ms === 1000) || h.frames.some(frame => frame.type === "provider_outcome"));
    expect(h.frames.find(frame => frame.type === "provider_outcome")?.payload["detail"]).toBeUndefined();
    expect(h.frames.some(frame => frame.type === "provider_drive_epoch_start_request")).toBe(false);
    if (holder === "effect slot") Reflect.get(h.bridge, "releaseEffectGovernor").call(h.bridge, "job_sibling", "sibling", false);
    else await Reflect.get(h.bridge, "releaseProviderDrainLease").call(h.bridge, providerKey, "sibling");
    await h.tick();
    await h.started();
    const observation = (await h.request("agent_decide_request_v1")).payload["observation"];
    expect(observation !== null && typeof observation === "object" && "controls" in observation &&
      Array.isArray(observation.controls) && observation.controls.length > 0).toBe(true);
    expect(h.backend.store.activeJobs[0]?.status).toBe("awaiting_download");
    expect(h.frames.some(frame => frame.type === "provider_outcome")).toBe(false);
    await h.decide("decision", "BLOCKED"); await h.settle();
  });

// Measured live 2026-09-23 (job_2c3c6f40ad69e1e4282a272d25, Nature Medicine):
// the nature adapter settled unknown, the generic citation_pdf_url candidate
// downloaded HTML, and the extension settled `html` and then said nothing
// else. The navigated claim held publisher:doi.org for its whole 30-minute
// lease while two sibling papers queued behind it. An HTML candidate leaves
// the article page as the only lead: the agent gets the still-held epoch, or,
// without an agent, the drive ends with an attributed provider outcome.
const htmlCandidate = { strategy_id: "generic-citation-pdf/1", strategy_version: "1", url: "https://unregistered.example/article/one.pdf" };
async function genericCandidateReturnsHTML(h: AgentHarness) {
  // The generic drive awaits its start reply inside classification.
  const classifying = h.classify();
  await h.started();
  await classifying;
  await until(() => h.downloads.started.length === 1);
  const id = 901;
  h.downloads.items.set(id, { id, tabId: tabID, url: htmlCandidate.url, finalUrl: url, filename: `/tmp/papio/${jobID}/one.pdf`, mime: "text/html", state: "complete" });
  const before = h.frames.length;
  // Settling awaits its daemon reply inside the download handler.
  void h.downloads.onChanged.emit({ id, state: { current: "complete" } });
  await flush();
  return before;
}
for (const knownAdapter of [false, true]) {
  test(`${knownAdapter ? "known unknown" : "no-adapter"} generic HTML candidate hands its held epoch to the article agent`, async () => {
    const h = await harness({ knownAdapter, genericCandidates: [htmlCandidate] });
    const before = await genericCandidateReturnsHTML(h);
    // The agent replays the exact held tuple; HTML is not settled first,
    // because a settled tuple can never be started again.
    const replay = await h.request("provider_drive_epoch_start_request", before);
    expect(replay.payload).toMatchObject(epoch);
    await h.reply(replay, "provider_drive_epoch_start_result", { ...epoch, outcome: "started" });
    await h.decide("decision", "BLOCKED", before);
    await h.settle();
    const results = h.frames.filter(f => f.type === "provider_drive_epoch_result_request");
    const outcomes = h.frames.filter(f => f.type === "provider_outcome");
    expect(results).toHaveLength(1);
    expect(outcomes).toHaveLength(1);
    expect(outcomes[0]!.payload["outcome"]).toBe("ui_changed");
    expect(outcomes[0]!.payload["detail"]).toContain("generic candidate returned HTML");
    expect(outcomes[0]!.payload["adapter_id"]).toBe(knownAdapter ? "test-unknown" : undefined);
    expect(h.counts().actions).toBe(0);
  });

  test(`${knownAdapter ? "known unknown" : "no-adapter"} generic HTML candidate without an agent settles HTML and ends the drive`, async () => {
    const h = await harness({ knownAdapter, genericCandidates: [htmlCandidate], features: ["provider_drive_epoch_v1", "effect_permit_v1"] });
    const before = await genericCandidateReturnsHTML(h);
    const result = await h.request("provider_drive_epoch_result_request", before);
    expect(result.payload).toMatchObject({ ...epoch, outcome: "html" });
    await h.reply(result, "provider_drive_epoch_result", { ...epoch, outcome: "applied" });
    await until(() => h.frames.some(f => f.type === "provider_outcome"));
    const outcomes = h.frames.filter(f => f.type === "provider_outcome");
    expect(outcomes).toHaveLength(1);
    expect(outcomes[0]!.payload["outcome"]).toBe("ui_changed");
    expect(outcomes[0]!.payload["detail"]).toContain("generic candidate returned HTML");
    expect(outcomes[0]!.payload["adapter_id"]).toBe(knownAdapter ? "test-unknown" : undefined);
    expect(h.frames.filter(f => f.type === "provider_drive_epoch_start_request")).toHaveLength(1);
  });
}
