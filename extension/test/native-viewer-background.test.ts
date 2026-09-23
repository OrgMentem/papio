// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { Bridge, type BridgeDeps } from "../src/background";
import { emptyStore, chromeBackend, type ActiveJob, type StoreShape } from "../src/state";
import { nativeViewerAction } from "../src/native-viewer";
import { FakeWebNavigation } from "./fake-tabs";
import { NATIVE_VIEWER_SAVE_FEATURE, parseBrowserMessage } from "../src/protocol";
import type { NativeRequestResult } from "../src/correlation";

const jobID = "job_nativeviewer";
const url = "https://pdf.sciencedirectassets.com/one/main.pdf?token=secret#page=2";
const job = (): ActiveJob => ({ job_id: jobID, tab_id: -1, offered_at: 1, expires_at: 999999,
  status: "awaiting_download", provider_hosts: [], manual_delivery_required: true });
const action = () => ({ kind: "human_action", id: "action:42", rank: 42, title: "Example work",
  links: [], ops: ["open", "dismiss"], sha256: "", size_bytes: 0, job_id: jobID, job_state: "awaiting_human",
  action_kind: "manual_download", action_id: 42, revision: 3,
  facts: [{ label: "Diagnosis", text: "native_viewer_download_required" }] });

/** Run the real delivery/continuation methods and state reducer. Only the browser
 * and daemon boundaries are fake; no adapter, content script or PDF download. */
function harness(seed?: StoreShape) {
  let stored = seed ?? { ...emptyStore(), activeJobs: [job()], connectionStatus: "connected" as const,
    daemonFeatures: [NATIVE_VIEWER_SAVE_FEATURE, "triage_snapshot_v1"] };
  const navigation = new FakeWebNavigation();
  const tabs = [{ id: 7, active: true, url }];
  let items = [action()];
  const requests: Record<string, unknown>[] = [];
  let respond: (request: Record<string, unknown>) => Promise<NativeRequestResult> = async request => ({
    kind: "response", payload: { request_id: "response123", operation_id: "operation123",
      outcome: request.step === "prepare" ? "prepared" : "ready" },
  });
  let beforeSnapshot = () => {};
  let beforeDelay = () => {};
  let clock = 100;
  const timers: (() => void | Promise<void>)[] = [];
  const deps = {
    firefox: true, randomUUID: () => crypto.randomUUID(), now: () => clock,
    setTimeout: (fn: () => void | Promise<void>, ms: number) => {
      if (ms === 250) { clock += ms; beforeDelay(); queueMicrotask(fn); }
      else timers.push(fn);
    },
    backend: { load: async () => stored, save: async (s: StoreShape) => { stored = s; } },
    tabs: { query: async () => tabs.map(t => ({ ...t })), get: async (id: number) => {
      const tab = tabs.find(t => t.id === id); if (!tab) throw Error("removed"); return { ...tab };
    } },
    downloads: { search: async () => [], download: async () => { throw Error("must not download"); } },
    scripting: { executeScript: async () => { throw Error("must not inject"); } },
    webNavigation: navigation, adapterSpecs: [],
  } as unknown as BridgeDeps;
  const bridge = new Bridge(deps);
  Reflect.set(bridge, "store", stored);
  Reflect.set(bridge, "ready", Promise.resolve());
  Reflect.set(bridge, "port", {});
  Reflect.set(bridge, "helloAckGeneration", Reflect.get(bridge, "portGeneration"));
  Reflect.set(bridge, "helloRole", "holder");
  Reflect.set(bridge, "browserEpoch", "browser123");
  Reflect.get(bridge, "bindWebNavigation").call(bridge);
  bridge.requestTriageSnapshot = async () => {
    beforeSnapshot();
    const frame = parseBrowserMessage({ protocol: "papio-browser/1", type: "triage_snapshot_response",
      msg_id: "snapshot123", seq: 1, payload: { request_id: "request123", schema: 1,
        generated_at: "2026-09-23T00:00:00Z", counts: { pending_total: items.length,
          watch_hits: 0, actions: items.length, retractions: 0, jobs_working: 0,
          jobs_needs_review: 0, failure_groups_7d: 0 }, items, has_more: false, unsupported_items_count: 0 } });
    return { ok: true, snapshot: frame.payload };
  };
  bridge.requestCorrelated = async (kind, request, options) => {
    expect(kind).toBe("native_viewer_save_request_v1");
    parseBrowserMessage({ protocol: "papio-browser/1", type: kind, msg_id: "request123", seq: 1,
      job_id: options?.jobID, payload: { ...request, request_id: options?.requestID } });
    requests.push(request);
    return respond(request);
  };
  return { bridge, deps, navigation, tabs, requests, timers,
    state: () => Reflect.get(bridge, "store") as StoreShape,
    automatic: () => Reflect.get(bridge, "startNativeViewerSave").call(bridge, jobID, 7, url, true) as ReturnType<Bridge["startPDFDelivery"]>,
    setItems: (next: typeof items) => { items = next; },
    onSnapshot: (fn: () => void) => { beforeSnapshot = fn; },
    onDelay: (fn: () => void) => { beforeDelay = fn; },
    respond: (fn: typeof respond) => { respond = fn; },
  };
}

async function choose(h: ReturnType<typeof harness>) {
  const offer = await h.bridge.startPDFDelivery({ tab_id: 7, url });
  expect(offer.ok).toBe(true);
  if (!offer.ok || !offer.choice) throw Error("missing one-use picker");
  return h.bridge.startPDFDelivery({ tab_id: 7, url, choice: {
    interaction: offer.choice.interaction, job_id: jobID,
  } });
}

test("parked manual job without an adapter uses fresh action and real document, daemon ready only", async () => {
  const h = harness();
  const reply = await choose(h);
  expect(reply).toMatchObject({ ok: true, state: "adopted", job_id: jobID });
  expect(h.requests.map(r => r.step)).toEqual(["prepare", "advance"]);
  expect(h.requests[0]).toMatchObject({ action_id: 42, action_revision: 3,
    browser_epoch: "browser123", source_url: url, selection: "explicit" });
  expect(h.requests[1]).not.toHaveProperty("selection");
  expect(h.requests[0]?.document_id).toBe(await Reflect.get(h.bridge, "liveDocumentEpoch").call(h.bridge, 7));
  expect(h.state().activeJobs[0]?.native_viewer_save?.state).toBe("settled");
  expect(h.state().activeJobs[0]?.generic_drive_epoch).toBeUndefined();
});

test("automatic path requires exact structured current action diagnosis", async () => {
  const h = harness();
  h.setItems([{ ...action(), facts: [{ label: "Detail", text: "native_viewer_download_required" }] }]);
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "authority_lost" } });
  expect(h.requests).toHaveLength(0);
  expect(nativeViewerAction({ items: [action(), action()] }, jobID, true)).toBeUndefined();
});

test("explicit picker can select another manual diagnosis", async () => {
  const h = harness(); h.setItems([{ ...action(), facts: [] }]);
  expect(await choose(h)).toMatchObject({ ok: true, state: "adopted" });
});

for (const mutation of ["document", "cancel", "revision", "duplicate", "removed", "takeover", "feature", "epoch"] as const) {
  test(`no advance after ${mutation} between native steps`, async () => {
    const h = harness();
    h.onDelay(() => {
      switch (mutation) {
        case "document": void h.navigation.onCommitted.emit({ tabId: 7, frameId: 0, documentId: "different" }); break;
        case "cancel": h.setItems([]); break;
        case "revision": h.setItems([{ ...action(), revision: 4 }]); break;
        case "duplicate": h.tabs.push({ id: 8, active: false, url }); break;
        case "removed": h.tabs.splice(0); break;
        case "takeover": Reflect.set(h.bridge, "helloRole", "pending"); break;
        case "feature": h.state().daemonFeatures = []; break;
        case "epoch": Reflect.set(h.bridge, "browserEpoch", "replacement"); break;
      }
    });
    expect(await h.automatic()).toMatchObject({ ok: false });
    expect(h.requests.filter(r => r.step !== "cancel").map(r => r.step)).toEqual(["prepare"]);
    expect(h.state().activeJobs[0]?.native_viewer_save?.state).toBe("interrupted");
  });
}

test("duplicate invocation cannot issue another prepare while reply is pending", async () => {
  const h = harness(); let release!: (r: NativeRequestResult) => void;
  h.respond(() => new Promise(resolve => { release = resolve; }));
  const first = h.automatic();
  for (let i = 0; i < 50 && !release; i++) await Promise.resolve();
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  release({ kind: "transport", code: "connection_lost", message: "lost" });
  expect(await first).toMatchObject({ ok: false });
  expect(h.requests).toHaveLength(1);
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
});

test("lost reply latches interruption, new worker cannot replay", async () => {
  const h = harness();
  h.respond(async () => ({ kind: "transport", code: "connection_lost", message: "lost" }));
  expect(await h.automatic()).toMatchObject({ ok: false });
  const restarted = harness(h.state());
  expect(await restarted.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  expect(restarted.requests).toHaveLength(0);
});

test("missing browser document ID refuses before prepare", async () => {
  const h = harness(); h.deps.webNavigation!.getFrame = async () => ({});
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "unsupported" } });
  expect(h.requests).toHaveLength(0);
});

test("duplicate exact URL even in inactive other window refuses before prepare", async () => {
  const h = harness(); h.tabs.push({ id: 8, active: false, url });
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "document_changed" } });
  expect(h.requests).toHaveLength(0);
});

test("older Firefox peer keeps original refusal and emits no native request", async () => {
  const h = harness(); h.state().daemonFeatures = ["triage_snapshot_v1"];
  expect(await h.bridge.startPDFDelivery({ tab_id: 7, url })).toMatchObject({ ok: false, error: { code: "not_permitted" } });
  expect(h.requests).toHaveLength(0);
});

for (const outcome of ["review", "rejected", "unavailable"] as const) {
  test(`daemon ${outcome} is terminal without another click or advance`, async () => {
    const h = harness();
    h.respond(async () => ({ kind: "response", payload: { request_id: "response123", outcome } }));
    const reply = await h.automatic();
    expect(reply.ok).toBe(outcome === "review");
    expect(h.requests).toHaveLength(1);
    expect(h.state().pendingDelivery?.status).toBe(outcome === "review" ? "waiting_manual" : "failed");
  });
}

test("local URL-free latch survives session wipe and keeps the same action blocked", async () => {
  const local: Record<string, unknown> = {}; const session: Record<string, unknown> = {};
  const area = (data: Record<string, unknown>) => ({ get: async (key: string) => ({ [key]: data[key] }),
    set: async (values: Record<string, unknown>) => { Object.assign(data, values); } });
  const backend = chromeBackend({ local: area(local), session: area(session) } as never);
  const h = harness(); await h.automatic();
  await backend.save(h.state());
  expect(JSON.stringify(local)).not.toContain("secret");
  expect(JSON.stringify(local)).not.toContain("source_url");
  for (const key of Object.keys(session)) delete session[key];
  const restored = await backend.load();
  expect(restored.activeJobs[0]?.native_viewer_save?.state).toBe("settled");
  const restarted = harness({ ...restored, connectionStatus: "connected", daemonFeatures: [NATIVE_VIEWER_SAVE_FEATURE] });
  expect(await restarted.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  expect(restarted.requests).toHaveLength(0);
});

test("definite side-effect-free prepare refusal allows only a fresh explicit retry", async () => {
  const h = harness();
  h.respond(async () => ({ kind: "response", payload: {
    request_id: "response123", outcome: "unavailable", reason: "unavailable",
  } }));
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(h.state().activeJobs[0]?.native_viewer_save?.state).toBe("retryable");
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  expect(h.requests).toHaveLength(1);
  expect(h.requests[0]?.selection).toBe("automatic");
  h.respond(async () => ({ kind: "response", payload: { request_id: "response123", outcome: "ready" } }));
  expect(await choose(h)).toMatchObject({ ok: true, state: "adopted" });
  expect(h.requests).toHaveLength(2);
  expect(h.requests[1]?.selection).toBe("explicit");
});

test("lost prepare reply times out without a second prepare", async () => {
  const h = harness(); h.respond(() => new Promise(() => {}));
  const result = h.automatic();
  for (let i = 0; i < 60 && !h.requests.length; i++) await Promise.resolve();
  expect(h.requests).toHaveLength(1);
  for (const timer of h.timers) await timer();
  expect(await result).toMatchObject({ ok: false });
  expect(h.state().activeJobs[0]?.native_viewer_save?.state).toBe("interrupted");
  expect(h.requests).toHaveLength(1);
});

test("automatic post-park trigger runs outside inbound chain with save feature replacing old notice feature", async () => {
  const h = harness();
  h.state().activeJobs[0]!.access_mode = "delegated";
  h.state().activeJobs[0]!.tab_id = 7;
  const sent: string[] = [];
  Reflect.set(h.bridge, "send", (type: string) => { sent.push(type); return true; });
  Reflect.set(h.bridge, "retainForManualDownload", async () => {
    h.state().activeJobs = [job()]; // same retained, provider-retired shape
  });
  let release!: () => void;
  const inbound = new Promise<void>(resolve => { release = resolve; });
  Reflect.set(h.bridge, "inboundChain", inbound);
  h.respond(async () => {
    await inbound; // correlated response must be able to follow report's frame
    return { kind: "response", payload: { request_id: "response123", outcome: "ready" } };
  });
  await Reflect.get(h.bridge, "reportNativeViewerDownloadRequired").call(h.bridge, jobID, url, 7);
  release();
  for (let i = 0; i < 120 && h.state().pendingDelivery?.status !== "adopted"; i++) await Promise.resolve();
  expect(sent).toEqual(["provider_outcome"]);
  expect(h.requests).toHaveLength(1);
  expect(h.requests[0]?.selection).toBe("automatic");
  expect(h.state().pendingDelivery?.status).toBe("adopted");
  expect(h.state().activeJobs[0]?.access_mode).toBeUndefined();
});

test("same-URL replacement before first prepare is refused", async () => {
  const h = harness();
  h.onSnapshot(() => { void h.navigation.onCommitted.emit({ tabId: 7, frameId: 0, documentId: "replacement" }); });
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(h.requests).toHaveLength(0);
});

test("exact signed URL changes during prepare cannot advance or report ready", async () => {
  const h = harness();
  h.respond(async () => {
    h.tabs[0]!.url = url.replace("secret", "new-secret");
    return { kind: "response", payload: { request_id: "response123", operation_id: "operation123", outcome: "prepared" } };
  });
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(h.requests.filter(r => r.step !== "cancel")).toHaveLength(1);
});

test("job removal and replacement cannot erase the worker's consumed latch", async () => {
  const h = harness();
  h.onDelay(() => { h.state().activeJobs = [job()]; });
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  expect(h.requests.filter(r => r.step !== "cancel")).toHaveLength(1);
});

test("a refused automatic preparation never escalates itself to explicit selection", async () => {
  const h = harness();
  h.respond(async () => ({ kind: "response", payload: { request_id: "response123",
    outcome: "refused", reason: "source_rejected" } }));
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(h.requests.map(r => r.selection)).toEqual(["automatic"]);
  expect(await h.automatic()).toMatchObject({ ok: false, error: { code: "already_started" } });
  expect(h.requests).toHaveLength(1);
});

test("holder loss during correlation's connect await is rejected at actual wire send", async () => {
  const h = harness(); const posted: unknown[] = [];
  Reflect.set(h.bridge, "port", { postMessage: (frame: unknown) => posted.push(frame) });
  h.bridge.requestCorrelated = async (kind, payload, options) => {
    Reflect.set(h.bridge, "helloRole", "pending");
    const sent = Reflect.get(h.bridge, "send").call(h.bridge, kind,
      { ...payload, request_id: options?.requestID }, options?.jobID);
    expect(sent).toBe(false);
    return { kind: "transport", code: "connection_lost", message: "lost" };
  };
  expect(await h.automatic()).toMatchObject({ ok: false });
  expect(posted).toHaveLength(0);
});

test("current action on a later snapshot page is found before prepare and advance", async () => {
  const h = harness(); const snapshot = h.bridge.requestTriageSnapshot.bind(h.bridge);
  const cursors: (string | undefined)[] = [];
  h.bridge.requestTriageSnapshot = async request => {
    cursors.push(request.cursor);
    const result = await snapshot(request);
    if (!result.ok) return result;
    return request.cursor === undefined
      ? { ok: true, snapshot: { ...result.snapshot, items: [], has_more: true, cursor: "next-page" } }
      : result;
  };
  expect(await h.automatic()).toMatchObject({ ok: true, state: "adopted" });
  expect(cursors).toEqual([undefined, "next-page", undefined, "next-page"]);
});
