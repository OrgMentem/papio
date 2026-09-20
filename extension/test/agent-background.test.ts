// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { Bridge, MIN_DAEMON_VERSION, type BridgeDeps, type DownloadItemLike, type NativePort } from "../src/background";
import { agentDOM, type AgentDOMRequest } from "../src/agent-dom";
import { nativeDownloadDocumentCurrent, NATIVE_CLICK_ADOPTION_FEATURE } from "../src/native-download";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { planGeneric } from "../src/plan";
import { emptyStore, patchJob, migrateManagedState, type ActiveJob, type StoreShape } from "../src/state";
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

async function harness(options: { features?: string[]; firefox?: boolean; ignoredSteeringEvent?: boolean; status?: ActiveJob["status"]; seed?: StoreShape } = {}) {
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
  if (options.firefox && !options.ignoredSteeringEvent) Reflect.deleteProperty(downloads, "onDeterminingFilename");
  const backend = { store: emptyStore(), load: async () => backend.store, save: async (store: StoreShape) => { backend.store = store; } };
  let permitted = true, observations = 0, actions = 0, genericPlans = 0;
  let onAct: (() => Promise<void>) | undefined;
  const deps: BridgeDeps = {
    firefox: options.firefox ?? false,
    connectNative: () => port, manifestVersion: "0.1.0", randomUUID: () => crypto.randomUUID(), now: () => now,
    setTimeout: (fn, ms) => timers.push({ fn, ms }), backend, tabs, downloads, adapterSpecs: [],
    scripting: { executeScript: async injection => {
      if (injection.func === planGeneric) { genericPlans++; return [{ result: { evidence: [], candidates: [] } }]; }
      if (injection.func === nativeDownloadDocumentCurrent) return [{ result: nativeDownloadDocumentCurrent(...injection.args as [string, string]) }];
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
  backend.store = options.seed ?? emptyStore();
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
  return { bridge, deps, backend, frames, win, tabs, downloads, timers, classify, started, decide, request, reply, settle, tick, update, now: () => now,
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
