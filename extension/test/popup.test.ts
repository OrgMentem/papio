// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import {
  acquireCurrentPage,
  collectPageMetadata,
  OPEN_HANDOFF_MESSAGE,
  OPEN_INBOX_MESSAGE,
  openInbox,
  openInstitutionSignIn,
  deriveSessionCardState,
  deriveSessionRows,
  requestSessionState,
  readCurrentPageMetadata,
  refreshImpactSummary,
  refreshCaptureOptions,
  renderInstitutionSession,
  renderNeedsAttention,
  grantProviderAccess,
  renderDaemonStatus,
  renderPageContext,
  renderPageAcquire,
  renderImpactSummary,
  derivePulseDisplay,
  renderPopupCatchup,
  renderWorkPulse,
  type PopupPulseCache,
  sessionWarmForJob,
  renderResolverGrants,
  renderTermsConsent,
  wireCapture,
  wireDevTools,
  wireHistoryLauncher,
  wireInboxLauncher,
  wirePrimaryShortcut,
  wireSettings,
  renderLeftoverTabs,
  SESSION_PROBE_MESSAGE,
  SESSION_STATE_MESSAGE,
} from "../src/popup";
import type { ActiveJob } from "../src/state";
import type { WorkPulseResponsePayload } from "../src/protocol";
import { SESSION_STALE_MS } from "../src/keepalive";
import { PROVIDERS, SCENARIOS } from "../src/capture";

function popupDocument(): Document {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/popup.html", import.meta.url), "utf8"));
  Object.assign(globalThis, {
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLSelectElement: window.HTMLSelectElement,
  });
  return window.document as unknown as Document;
}

function job(overrides: Partial<ActiveJob> = {}): ActiveJob {
  return {
    job_id: "job-1",
    tab_id: 17,
    offered_at: 1,
    expires_at: 2,
    status: "accepted",
    provider_hosts: ["www.jstor.org"],
    ...overrides,
  };
}

function pulseCache(overrides: Partial<WorkPulseResponsePayload> = {}): PopupPulseCache {
  return {
    pulse: {
      request_id: "pulse-1",
      schema: 1,
      generated_at: new Date().toISOString(),
      projection_complete: true,
      nonterminal_total: 0,
      in_flight: 0,
      continuing: 0,
      scheduled: 0,
      waiting_required: 0,
      stalled: 0,
      ...overrides,
    },
    receivedAt: Date.now(),
    workerEpoch: "worker-1",
  };
}

test("derives every typed pulse state without inventing progress", () => {
  expect(derivePulseDisplay(pulseCache({ in_flight: 2, nonterminal_total: 2 })).primary).toBe("Moving");
  expect(derivePulseDisplay(pulseCache({ waiting_required: 2, nonterminal_total: 2 })).primary).toBe("Waiting on you");
  expect(derivePulseDisplay(pulseCache({
    stalled: 1,
    nonterminal_total: 1,
    stall_episodes: [{
      episode_key: "episode-1",
      cause_kind: "delivery_poll_overdue",
      public_label: "Delivery poll overdue",
      since: new Date(Date.now() - 60_000).toISOString(),
      count: 1,
    }],
  })).primary).toBe("Stalled");
  expect(derivePulseDisplay(pulseCache({ scheduled: 1, nonterminal_total: 1 })).primary).toBe("Scheduled");
  expect(derivePulseDisplay(pulseCache({ last_finished_at: new Date(Date.now() - 7_200_000).toISOString() })).primaryText).toContain("last finished");
  expect(derivePulseDisplay(undefined).primary).toBe("Unknown");
});

test("uses counts-v3 turns for decisions while labelling pulse buckets as nonterminal", () => {
  const cache = pulseCache({ waiting_required: 34, nonterminal_total: 116 });
  const counts = { pending_total: 35, turns_required: 35 };
  const display = derivePulseDisplay(cache, "connected", Date.now(), 15_000, counts);
  expect(display.primaryText).toBe("Waiting on you · 35 decisions");
  expect(display.primaryText).not.toContain("34");
  expect(display.buckets).toContain("Nonterminal breakdown");
  // The bucket must NOT borrow "need you"; that phrase belongs to turns_required,
  // which the inbox renders and which legitimately differs from this bucket.
  expect(display.buckets).toContain("34 awaiting your turn");
  expect(display.buckets).not.toContain("need you");
  expect(display.buckets).toContain("0 in flight");
  expect(display.buckets).toContain("0 continuing");
  expect(display.buckets).toContain("0 scheduled");
  expect(display.buckets).toContain("0 stalled");
  const doc = popupDocument();
  renderWorkPulse(doc, cache, "connected", Date.now(), counts);
  expect(doc.getElementById("popup-pulse-primary")?.textContent).toBe("Waiting on you · 35 decisions");
  expect(doc.getElementById("popup-pulse-buckets")?.textContent).toContain("Nonterminal breakdown");
});

test("uses an honest pending-items label when counts-v3 is unavailable", () => {
  const display = derivePulseDisplay(
    pulseCache({ waiting_required: 2, nonterminal_total: 2 }),
    "connected",
    Date.now(),
    15_000,
    { pending_total: 3 },
  );
  expect(display.primaryText).toBe("Waiting on you · 3 pending items");
  expect(display.primaryText).not.toContain("decisions");
});


test("catch-up says newer Activity without an exact number across a retention gap", async () => {
  const popup = await import(`../src/popup.ts?catchup-gap=${Date.now()}`);
  const doc = popupDocument();
  const storage = {
    get: async () => ({ papio_popup_activity_seen_through_seq_v1: 100 }),
    set: async () => undefined,
  };
  Object.assign(globalThis, { chrome: { storage: { local: storage } } });
  await popup.renderPopupCatchup(doc, {
    entries: [{ seq: 58861, at: new Date().toISOString(), kind: "system", text: "retained" }],
    gap: true,
    paged: true,
  });
  const text = doc.getElementById("popup-catchup-text")?.textContent ?? "";
  expect(text).toContain("newer Activity is available");
  expect(text).not.toMatch(/\d/);
});

test("catch-up uses daemon new_count_since rather than the page size", async () => {
  const popup = await import(`../src/popup.ts?catchup-count=${Date.now()}`);
  const doc = popupDocument();
  const storage = {
    get: async () => ({ papio_popup_activity_seen_through_seq_v1: 100 }),
    set: async () => undefined,
  };
  Object.assign(globalThis, { chrome: { storage: { local: storage } } });
  await popup.renderPopupCatchup(doc, {
    entries: [{ seq: 101, at: new Date().toISOString(), kind: "system", text: "retained" }],
    newCountSince: 12,
    gap: false,
    paged: true,
  });
  expect(doc.getElementById("popup-catchup-text")?.textContent).toBe("While you were away: 12 updates");
});
test("does not classify an incomplete pulse from absent buckets", () => {
  const partial: PopupPulseCache = {
    pulse: {
      request_id: "pulse-partial",
      schema: 1,
      generated_at: new Date().toISOString(),
      projection_complete: false,
      scheduled: 1,
      nonterminal_total: 1,
    },
    receivedAt: Date.now(),
    workerEpoch: "worker-1",
  };
  expect(derivePulseDisplay(partial).primary).toBe("Unknown");
});

test("pulse freshness uses local receipt time and never invents ETA or percentage", () => {
  const cache = pulseCache({ in_flight: 3, nonterminal_total: 3, next_action: {
    at: new Date(Date.now() + 60_000).toISOString(),
    kind: "retry",
    source: "OpenAlex",
    count: 3,
  } });
  const display = derivePulseDisplay({ ...cache, receivedAt: Date.now() - 16_000 }, "connected");
  expect(display.primary).toBe("Unknown");
  expect(display.primaryText).toContain("Status as of");
  expect(display.primaryText).not.toMatch(/%|ETA|queue position/i);
});

test("renders active batch companion facts and disconnected copy", () => {
  const doc = popupDocument();
  renderWorkPulse(doc, pulseCache({
    in_flight: 1,
    nonterminal_total: 2,
    latest_batch: {
      batch_id: "batch-1",
      started_at: new Date().toISOString(),
      membership: "complete",
      total: 2,
      settled: 0,
      nonterminal_total: 2,
    },
  }));
  expect(doc.getElementById("popup-pulse-primary")?.textContent).toContain("Moving");
  expect(doc.getElementById("popup-pulse-batch")?.textContent).toContain("2 papers");
  renderWorkPulse(doc, undefined, "disconnected");
  expect(doc.getElementById("popup-pulse-primary")?.textContent).toBe("Can't tell — daemon disconnected");
});

test("places the acquire icon before inbox and keeps idle feedback hidden", () => {
  const doc = popupDocument();
  const launcher = doc.querySelector(".launcher");
  const headerActions = doc.querySelector(".header-actions");

  expect(doc.querySelector("h1")).toBeNull();
  expect(launcher?.querySelectorAll(".launcher-action")).toHaveLength(0);
  expect(launcher?.querySelector("h2")).toBeNull();
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);
  expect(doc.getElementById("page-acquire-doi")).toBeNull();
  expect(doc.getElementById("page-acquire-context")).toBeNull();
  expect(headerActions?.children[0]?.id).toBe("page-acquire-btn");
  expect(headerActions?.children[1]?.id).toBe("open-inbox-btn");
  expect(doc.getElementById("page-acquire-btn")?.closest("header")).not.toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.querySelector("svg")).toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.textContent).toBe("Acquire");
  expect(doc.getElementById("page-acquire-btn")?.classList.contains("primary")).toBe(true);
  expect(doc.getElementById("page-acquire-btn")?.hidden).toBe(true);
  expect(doc.getElementById("daemon-footer")).toBeNull();
  expect(doc.getElementById("open-inbox-btn")?.getAttribute("aria-label")).toBe("Open inbox");
  expect(doc.getElementById("needs-you-section")).not.toBeNull();
  expect(doc.getElementById("needs-you-section")?.hidden).toBe(true);
  expect(doc.getElementById("terms-consent")).not.toBeNull();
  expect(doc.getElementById("resolver-grant")).not.toBeNull();
});

test("capture selects offer every registered provider and scenario", () => {
  const doc = popupDocument();
  wireCapture(doc, ["page_capture_terms_v1"]);
  const values = (id: string): string[] =>
    Array.from(doc.querySelectorAll<HTMLOptionElement>(`#${id} option`)).map((o) => o.value);
  expect(values("capture-provider")).toEqual([...PROVIDERS]);
  expect(values("capture-scenario")).toEqual([...SCENARIOS]);
});

// The `terms` scenario rides the pre-existing page_capture scenario enum, so
// an old daemon that has never validated it would reject the whole frame —
// and a browser-protocol decode failure tears down the entire native-
// messaging session, not just that capture. The option must therefore stay
// unrendered (not merely disabled) until the daemon proves it can accept it.
test("offers the terms capture scenario once the daemon advertises the gate feature", () => {
  const doc = popupDocument();
  wireCapture(doc, ["page_capture_terms_v1"]);
  const values = Array.from(doc.querySelectorAll<HTMLOptionElement>("#capture-scenario option")).map(
    (o) => o.value,
  );
  expect(values).toEqual([...SCENARIOS]);
  expect(values).toContain("terms");
});

test("withholds the terms capture scenario from a daemon that lacks the gate feature", () => {
  const doc = popupDocument();
  wireCapture(doc, ["page_capture_v1"]);
  const values = Array.from(doc.querySelectorAll<HTMLOptionElement>("#capture-scenario option")).map(
    (o) => o.value,
  );
  expect(values).toEqual(SCENARIOS.filter((s) => s !== "terms"));
  expect(values).not.toContain("terms");
});

test("fails closed and withholds the terms capture scenario before daemon features are known", () => {
  const doc = popupDocument();
  wireCapture(doc);
  const values = Array.from(doc.querySelectorAll<HTMLOptionElement>("#capture-scenario option")).map(
    (o) => o.value,
  );
  expect(values).toEqual(SCENARIOS.filter((s) => s !== "terms"));
  expect(values).not.toContain("terms");
});

test("capture tools require a developer build and an unpacked manifest", () => {
  const flag = "__PAPIO_DEV_CAPTURE__";
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, flag);
  try {
    Object.defineProperty(globalThis, flag, { configurable: true, value: false });
    const releaseBuild = popupDocument();
    wireDevTools(releaseBuild, {});
    expect(releaseBuild.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
    expect(releaseBuild.querySelectorAll("#capture-provider option")).toHaveLength(0);

    Object.defineProperty(globalThis, flag, { configurable: true, value: true });
    const packed = popupDocument();
    wireDevTools(packed, { update_url: "https://clients2.google.com/service/update2/crx" });
    expect(packed.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
    expect(packed.querySelectorAll("#capture-provider option")).toHaveLength(0);

    const unpacked = popupDocument();
    wireDevTools(unpacked, {});
    expect(unpacked.querySelector<HTMLElement>(".capture")?.hidden).toBe(false);
    expect(unpacked.querySelectorAll("#capture-provider option")).toHaveLength(PROVIDERS.length);
    const capture = unpacked.querySelector<HTMLElement>(".capture");
    expect(capture?.tagName).toBe("DETAILS");
    expect(capture?.hasAttribute("open")).toBe(false);
  } finally {
    if (descriptor !== undefined) {
      Object.defineProperty(globalThis, flag, descriptor);
    } else {
      Reflect.deleteProperty(globalThis, flag);
    }
  }
});

test("capture wiring stays one-shot across repeated option refreshes", () => {
  const doc = popupDocument();
  const button = doc.getElementById("capture-btn");
  expect(button).toBeInstanceOf(HTMLButtonElement);
  if (!(button instanceof HTMLButtonElement)) throw new Error("capture button missing");

  const originalAddEventListener = button.addEventListener.bind(button);
  let clickListeners = 0;
  button.addEventListener = ((
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions,
  ) => {
    if (type === "click") clickListeners += 1;
    originalAddEventListener(type, listener, options);
  }) as typeof button.addEventListener;

  wireDevTools(doc, {});
  refreshCaptureOptions(doc, ["page_capture_terms_v1"]);
  refreshCaptureOptions(doc);
  wireCapture(doc, ["page_capture_terms_v1"]);

  expect(button.dataset.wired).toBe("1");
  expect(clickListeners).toBe(1);
});

test("capture option refresh preserves valid selections and safely replaces a gated scenario", () => {
  const doc = popupDocument();
  wireDevTools(doc, {});
  const provider = doc.getElementById("capture-provider");
  const scenario = doc.getElementById("capture-scenario");
  expect(provider).toBeInstanceOf(HTMLSelectElement);
  expect(scenario).toBeInstanceOf(HTMLSelectElement);
  if (!(provider instanceof HTMLSelectElement) || !(scenario instanceof HTMLSelectElement)) {
    throw new Error("capture selects missing");
  }

  provider.value = PROVIDERS[1]!;
  scenario.value = "drift";
  const staleProviderOption = doc.createElement("option");
  staleProviderOption.value = "stale";
  provider.append(staleProviderOption);
  refreshCaptureOptions(doc, ["page_capture_terms_v1"]);
  expect(provider.value).toBe(PROVIDERS[1]!);
  expect(scenario.value).toBe("drift");

  scenario.value = "terms";
  refreshCaptureOptions(doc);
  expect(scenario.value).toBe(SCENARIOS[0]!);
});

test("packed capture panels stay unwired and unpopulated during option refreshes", () => {
  const flag = "__PAPIO_DEV_CAPTURE__";
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, flag);
  try {
    Object.defineProperty(globalThis, flag, { configurable: true, value: true });
    const doc = popupDocument();
    wireDevTools(doc, { update_url: "https://updates.invalid/extension" });
    refreshCaptureOptions(doc, ["page_capture_terms_v1"]);

    const button = doc.getElementById("capture-btn");
    expect(button).toBeInstanceOf(HTMLButtonElement);
    expect(button instanceof HTMLButtonElement ? button.dataset.wired : undefined).toBeUndefined();
    expect(doc.querySelector<HTMLElement>(".capture")?.hidden).toBe(true);
    expect(doc.querySelectorAll("#capture-provider option")).toHaveLength(0);
    expect(doc.querySelectorAll("#capture-scenario option")).toHaveLength(0);
  } finally {
    if (descriptor !== undefined) {
      Object.defineProperty(globalThis, flag, descriptor);
    } else {
      Reflect.deleteProperty(globalThis, flag);
    }
  }
});

test("renders actionable daemon problems without routine version diagnostics", () => {
  const doc = popupDocument();
  renderDaemonStatus(doc, { connectionStatus: "connected", daemonVersion: "0.1.0" });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(true);

  Object.assign(globalThis, { __PAPIO_DAEMON_VERSION__: "0.2.0" });
  renderDaemonStatus(doc, {
    connectionStatus: "connected",
    daemonVersion: "0.1.0",
    daemonUpdateHint: true,
  });
  expect(doc.getElementById("daemon-status")?.hidden).toBe(false);
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe(
    "papio 0.2.0 is available — daemon is v0.1.0",
  );

  renderDaemonStatus(doc, {
    connectionStatus: "connected",
    daemonVersion: "0.1.0-dev.abc123",
    daemonUpdateHint: true,
  });
  expect(doc.getElementById("daemon-status-message")?.textContent).toBe(
    "papio 0.2.0 is available — your daemon is a development build (v0.1.0-dev.abc123)",
  );
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe(
    "Update the source checkout, then run: make dev-deploy",
  );
  delete (globalThis as Record<string, unknown>).__PAPIO_DAEMON_VERSION__;

  renderDaemonStatus(doc, { connectionStatus: "disconnected" });
  expect(doc.getElementById("daemon-status")?.textContent).toContain("papio daemon isn't reachable");
  expect(doc.getElementById("daemon-status-hint")?.textContent).toBe("run: papio daemon status");
  expect(doc.getElementById("daemon-footer")).toBeNull();
});

test("shows the DOI acquire icon with its tooltip even without a negotiated daemon", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    throw new Error("papio daemon isn't reachable");
  });
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  const section = doc.getElementById("page-acquire");
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(section?.hidden).toBe(true);
  expect(button.disabled).toBe(false);
  expect(button.hidden).toBe(false);
  expect(button.title).toBe("Acquire this page · 10.1000/example");
  expect(button.getAttribute("aria-label")).toBe("Acquire this page · 10.1000/example");
  expect(button.getAttribute("aria-disabled")).toBe("false");
  expect(button.textContent).toBe("Acquire");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
  expect(button.disabled).toBe(false);
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("papio daemon isn't reachable");
});

test("keeps a successfully queued acquisition disabled", async () => {
  const doc = popupDocument();
  renderPageAcquire(doc, async () => ({ job_id: "job_page_acquire_001" }));
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();

  expect(button.disabled).toBe(true);
  expect(button.title).toBe("Queued");
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Queued: job_page_acquire_001");
});

test("hides the header acquire action when the current page has no paper", () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  });
  renderPageContext(doc, undefined, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  expect(button.hidden).toBe(true);
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);
  button.click();
  expect(calls).toBe(0);
});

test("shows the PDF acquire icon with the PDF tooltip", () => {
  const doc = popupDocument();
  renderPageAcquire(doc, async () => ({ error: "unused" }), async () => ({ state: "sending", job_id: "job_1234567890abcdef" }));
  renderPageContext(
    doc,
    { url: "https://papers.example.edu/download/paper.pdf?download=1", kind: "pdf", tab_id: 17 },
    [job({ job_id: "job_1234567890abcdef", tab_id: 17 })],
  );
  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.hidden).toBe(false);
  expect(button.title).toBe("Send this PDF to papio");
  expect(button.getAttribute("aria-label")).toBe("Send this PDF to papio");
  expect(button.textContent).toBe("Send PDF");
  expect(button.disabled).toBe(false);
  button.click();
});

test("does not send a DOI-less scraped page to the daemon", async () => {
  popupDocument();
  let messages = 0;
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 1 }] },
      scripting: {
        executeScript: async () => [{
          result: { url: "https://publisher.example.edu/article/42", title: "A DOI-less page" },
        }],
      },
      runtime: {
        sendMessage: async () => {
          messages += 1;
          return { job_id: "job_page_acquire_001" };
        },
      },
    },
  });

  await expect(acquireCurrentPage()).resolves.toEqual({ error: "no DOI found on this page" });
  expect(messages).toBe(0);
});

test("renders a live, honest status card for a local in-flight acquisition", () => {
  const doc = popupDocument();
  const now = Date.now();
  let openedInbox = 0;
  let openedTab = 0;
  renderPageContext(
    doc,
    { url: "https://doi.org/10.1000/example", doi: "10.1000/example" },
    [job({ expected: { title: "A paper in progress", doi: "doi:10.1000/example" }, status: "auth_pending" })],
    undefined,
    [{
      seq: 2,
      at: new Date(now - 11 * 60_000).toISOString(),
      job_id: "job-1",
      kind: "browser.handoff_offered",
      text: "Institution access handoff offered",
      title: "A paper in progress",
    }],
    {
      openInbox: async () => { openedInbox += 1; },
      goToTab: async () => { openedTab += 1; },
    },
  );

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.hidden).toBe(false);
  expect(button.disabled).toBe(true);
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire")?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-live")?.hidden).toBe(false);
  expect(doc.getElementById("page-acquire-live-title")?.textContent).toBe("A paper in progress");
  expect(doc.getElementById("page-acquire-live-status")?.textContent).toContain("No progress for 11m");
  expect(doc.getElementById("page-acquire-live-status")?.textContent).toContain("Institution access handoff offered");
  const inbox = doc.getElementById("page-acquire-open-inbox") as HTMLButtonElement;
  const tab = doc.getElementById("page-acquire-go-tab") as HTMLButtonElement;
  expect(tab.hidden).toBe(false);
  inbox.click();
  tab.click();
  expect(openedInbox).toBe(1);
  expect(openedTab).toBe(1);
});
test("scopes live auth-pending warmth to its demanded resolver origin", () => {
  const now = Date.now();
  const originA = "https://resolver-a.example.edu";
  const originB = "https://resolver-b.example.edu";
  const liveJob = job({
    job_id: "job-b",
    status: "auth_pending",
    expected: { title: "Paper B", doi: "10.1000/paper-b" },
  });
  const originSnapshot = (origin: string, warm: boolean) => ({
    origin,
    authenticated: warm,
    verdict: warm ? ("in" as const) : ("out" as const),
    probeSource: warm ? ("live_tab" as const) : ("none" as const),
    lastProbeOutcome: warm ? ("markers" as const) : ("no_markers" as const),
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: warm,
    pausedForReauth: false,
    lastProbeAt: now,
    dirtySince: null,
  });
  const session = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in" as const,
    probeSource: "live_tab" as const,
    lastProbeOutcome: "markers" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: true,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: originA,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authDemandComplete: true,
    authDemand: [{ job_id: liveJob.job_id, origin: originB }],
    origins: [originSnapshot(originA, true), originSnapshot(originB, false)],
  };
  const activity = [{
    seq: 1,
    at: new Date(now).toISOString(),
    job_id: liveJob.job_id,
    kind: "browser.auth_pending" as const,
    text: "Waiting on you to sign in",
  }];
  const page = { url: "https://doi.org/10.1000/paper-b", doi: "10.1000/paper-b" };

  expect(sessionWarmForJob(session, liveJob.job_id)).toBe(false);
  const waitingDoc = popupDocument();
  renderPageContext(waitingDoc, page, [liveJob], undefined, activity, {}, session);
  expect(waitingDoc.getElementById("page-acquire-live-status")?.textContent).toContain(
    "Waiting on you to sign in",
  );
  expect(waitingDoc.getElementById("page-acquire-live-status")?.textContent).not.toContain("Signed in");

  const warmB = {
    ...session,
    origins: [originSnapshot(originA, false), originSnapshot(originB, true)],
  };
  expect(sessionWarmForJob(warmB, liveJob.job_id)).toBe(true);
  const signedInDoc = popupDocument();
  renderPageContext(signedInDoc, page, [liveJob], undefined, activity, {}, warmB);
  expect(signedInDoc.getElementById("page-acquire-live-status")?.textContent).toContain("Signed in");

  const { authDemand: _dropped, authDemandComplete: _oldWorker, ...legacySession } = session;
  expect(sessionWarmForJob(legacySession, liveJob.job_id, true)).toBe(true);
  expect(sessionWarmForJob({ ...legacySession, authDemandComplete: true }, liveJob.job_id, true)).toBe(false);
});
test("demanded warmth requires one exact fresh authenticated non-checking snapshot", () => {
  const now = Date.now();
  const origin = "https://resolver.example.edu";
  const warmSnapshot = {
    origin,
    authenticated: true,
    verdict: "in" as const,
    probeSource: "live_tab" as const,
    lastProbeOutcome: "markers" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: true,
    pausedForReauth: false,
    lastProbeAt: now,
    dirtySince: null,
  };
  const session = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in" as const,
    probeSource: "live_tab" as const,
    lastProbeOutcome: "markers" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: true,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: origin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authDemandComplete: true,
    authDemand: [{ job_id: "demanded-job", origin }],
    origins: [warmSnapshot],
  };

  expect(sessionWarmForJob(session, "demanded-job")).toBe(true);
  expect(sessionWarmForJob({ ...session, origins: [] }, "demanded-job")).toBe(false);
  expect(
    sessionWarmForJob(
      { ...session, origins: [{ ...warmSnapshot, checking: true }] },
      "demanded-job",
    ),
  ).toBe(false);
  expect(
    sessionWarmForJob(
      { ...session, origins: [{ ...warmSnapshot, authenticated: false }] },
      "demanded-job",
    ),
  ).toBe(false);
  expect(
    sessionWarmForJob(
      {
        ...session,
        authDemand: [
          { job_id: "demanded-job", origin },
          { job_id: "demanded-job", origin },
        ],
      },
      "demanded-job",
    ),
  ).toBe(false);
  expect(
    sessionWarmForJob(
      {
        ...session,
        authDemand: [
          { job_id: "demanded-job", origin },
          { job_id: "demanded-job", origin: "https://other-resolver.example.edu" },
        ],
      },
      "demanded-job",
    ),
  ).toBe(false);
  expect(
    sessionWarmForJob(
      { ...session, origins: [{ ...warmSnapshot, lastVerdictAt: now - 10 * 60 * 1000 - 1 }] },
      "demanded-job",
    ),
  ).toBe(false);
  expect(
    sessionWarmForJob(
      { ...session, origins: [{ ...warmSnapshot, lastVerdictAt: now + 60_000 }] },
      "demanded-job",
    ),
  ).toBe(false);
  const futureSnapshot = { ...warmSnapshot, lastVerdictAt: now + 60_000 };
  expect(
    deriveSessionCardState({
      ...session,
      ...futureSnapshot,
      resolverOrigin: origin,
    }).action,
  ).toBe("signin");
  expect(
    deriveSessionRows({ ...session, origins: [futureSnapshot] })[0]?.action,
  ).toBe("signin");
});



test("merges auth-pending paper rows into the institution session card", async () => {
  const doc = popupDocument();
  const requests: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (message: unknown) => {
          requests.push(message);
          return { ok: true, opened: true };
        },
      },
    },
  });

  renderNeedsAttention(doc, [
    job({
      status: "auth_pending",
      expected: { title: "A paper awaiting institutional access", doi: "10.1000/example" },
    }),
  ]);

  const session = doc.getElementById("institution-session");
  const waiting = doc.getElementById("institution-session-waiting");
  expect(session?.hidden).toBe(false);
  expect(waiting?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-waiting-heading")?.textContent).toBe(
    "Waiting on your sign-in",
  );
  expect(waiting?.querySelector(".institution-session-waiting-title")?.textContent).toBe(
    "A paper awaiting institutional access",
  );
  expect(doc.getElementById("needs-you-section")?.hidden).toBe(true);
  const button = waiting?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Focus");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(requests).toEqual([{ type: OPEN_HANDOFF_MESSAGE, request: { job_id: "job-1" } }]);
  expect(button.textContent).toBe("Focus");
  renderNeedsAttention(doc, []);
  expect(waiting?.hidden).toBe(true);
  expect(session?.hidden).toBe(true);
});

test("a cold institutional handoff renders an explicit Open action", async () => {
  const doc = popupDocument();
  const opened: string[] = [];
  renderNeedsAttention(
    doc,
    [
      job({
        job_id: "job-cold-institution",
        tab_id: -1,
        status: "queued",
        requires_auth: true,
        engagement_required: true,
        expected: { title: "A paper needing institutional access" },
      }),
    ],
    [],
    async (jobID) => {
      opened.push(jobID);
    },
  );

  const waiting = doc.getElementById("institution-session-waiting");
  expect(waiting?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-waiting-heading")?.textContent).toBe(
    "Open institutional access",
  );
  const button = waiting?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Open");
  button.click();
  expect(button.textContent).toBe("Opening…");
  await Promise.resolve();
  await Promise.resolve();
  expect(opened).toEqual(["job-cold-institution"]);
  expect(button.disabled).toBe(false);
  expect(button.textContent).toBe("Open");
});

test("a cold engagement failure displays its structured reason", async () => {
  const doc = popupDocument();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({
          ok: false,
          error: {
            code: "missing_claim",
            message: "The handoff is missing institution identity metadata",
          },
        }),
      },
    },
  });
  renderNeedsAttention(doc, [
    job({
      job_id: "job-cold-invalid",
      tab_id: -1,
      status: "queued",
      requires_auth: true,
      engagement_required: true,
    }),
  ]);

  const waiting = doc.getElementById("institution-session-waiting");
  const button = waiting?.querySelector("button") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(button.textContent).toBe("Try again");
  const failure = waiting?.querySelector(".institution-session-waiting-status") as HTMLElement;
  expect(failure.hidden).toBe(false);
  expect(failure.textContent).toBe("The handoff is missing institution identity metadata");
});

test("a waiting_for_session paper renders no Focus action and distinct copy, unlike a plain auth_pending paper", () => {
  const doc = popupDocument();
  const focused: string[] = [];
  renderNeedsAttention(
    doc,
    [
      job({
        job_id: "job-own-signin",
        status: "auth_pending",
        expected: { title: "Waiting on my own sign-in" },
      }),
      job({
        job_id: "job-waiting-sibling",
        status: "auth_pending",
        waiting_for_session: true,
        expected: { title: "Deferring to a sibling paper" },
      }),
    ],
    [],
    async (jobID) => {
      focused.push(jobID);
    },
  );

  const rows = doc.querySelectorAll(".institution-session-waiting-row");
  expect(rows).toHaveLength(2);

  const ownRow = rows[0] as HTMLElement;
  expect(ownRow.querySelector(".institution-session-waiting-title")?.textContent).toBe(
    "Waiting on my own sign-in",
  );
  const ownButton = ownRow.querySelector("button") as HTMLButtonElement;
  expect(ownButton.textContent).toBe("Focus");
  const ownStatus = ownRow.querySelector(".institution-session-waiting-status") as HTMLElement;
  expect(ownStatus.hidden).toBe(true);
  expect(ownStatus.textContent).toBe("");
  ownButton.click();
  expect(focused).toEqual(["job-own-signin"]);

  const waitingRow = rows[1] as HTMLElement;
  expect(waitingRow.querySelector(".institution-session-waiting-title")?.textContent).toBe(
    "Deferring to a sibling paper",
  );
  expect(waitingRow.querySelector("button")).toBeNull();
  expect(waitingRow.querySelector(".institution-session-waiting-status")?.textContent).toBe(
    "Waiting for the institution sign-in — another paper's tab is at the login page",
  );
});

test("uses a DOI then job id when an awaiting sign-in has no paper title", () => {
  const doc = popupDocument();
  renderNeedsAttention(
    doc,
    [
      job({ job_id: "job-with-doi", status: "auth_pending", expected: { doi: "10.1000/fallback" } }),
      job({ job_id: "job-without-identity", status: "auth_pending" }),
    ],
    [],
    async () => {},
  );

  const labels = Array.from(doc.querySelectorAll(".institution-session-waiting-title")).map(
    (paper) => paper.textContent,
  );
  expect(labels).toEqual(["10.1000/fallback", "job-without-identity"]);
});
test("surfaces a blocked security check with a go-to-tab action", async () => {
  const doc = popupDocument();
  const focused: string[] = [];
  renderNeedsAttention(
    doc,
    [job({ job_id: "job-challenge", status: "auth_pending", challenge_blocked: true, challenge_host: "ScienceDirect.com" })],
    [],
    async (jobID) => {
      focused.push(jobID);
    },
  );

  const section = doc.getElementById("needs-you-section");
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-waiting")?.hidden).toBe(true);
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Security check needs you");
  expect(section?.querySelector(".needs-you-paper")?.textContent).toBe(
    "Security check needs you - sciencedirect.com",
  );
  const button = section?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Go-to-tab");
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(focused).toEqual(["job-challenge"]);
});


test("surfaces each blocked provider host once with a one-click grant", async () => {
  const doc = popupDocument();
  const granted: string[] = [];
  renderNeedsAttention(
    doc,
    [],
    ["journals.sagepub.com", "JOURNALS.SAGEPUB.COM", "www.sciencedirect.com"],
    async () => {},
    async () => {},
    [],
    async () => {},
    async (host) => {
      granted.push(host);
      return true;
    },
  );

  const section = doc.getElementById("needs-you-section");
  expect(section?.hidden).toBe(false);
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Allow provider access");
  expect(doc.getElementById("needs-you-message")?.textContent).toContain("Grant the blocked source here");
  expect(Array.from(section?.querySelectorAll(".needs-you-paper") ?? []).map((item) => item.textContent)).toEqual([
    "journals.sagepub.com",
    "www.sciencedirect.com",
  ]);
  const buttons = Array.from(section?.querySelectorAll("button") ?? []) as HTMLButtonElement[];
  expect(buttons.map((button) => button.textContent)).toEqual(["Allow", "Allow"]);
  buttons[0]?.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(granted).toEqual(["journals.sagepub.com"]);
  expect(buttons[0]?.textContent).toBe("Allowed");
});

test("provider grant requests the exact normalized https origin and rejects paths", async () => {
  const requested: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      permissions: {
        request: async (permission: unknown) => {
          requested.push(permission);
          return true;
        },
      },
    },
  });

  expect(await grantProviderAccess(" Journals.SAGEPUB.COM ")).toBe(true);
  expect(await grantProviderAccess("journals.sagepub.com/redirect")).toBe(false);
  expect(requested).toEqual([{ origins: ["https://journals.sagepub.com/*"] }]);
});

test("opens the singleton inbox through the broker when it acknowledges", async () => {
  const requests: unknown[] = [];
  const created: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: { sendMessage: async (message: unknown) => { requests.push(message); return { opened: true }; } },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
  });

  await openInbox();
  expect(requests).toEqual([{ type: OPEN_INBOX_MESSAGE }]);
  expect(created).toEqual([]);
});

test("falls back to a direct inbox tab when the broker does not answer", async () => {
  const doc = popupDocument();
  const created: unknown[] = [];
  let closed = 0;
  const { promise: dismissed, resolve: onClose } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      runtime: { sendMessage: async () => undefined },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
    window: { close: () => { closed += 1; onClose(); } },
  });
  wireInboxLauncher(doc);

  (doc.getElementById("open-inbox-btn") as HTMLButtonElement).click();
  await dismissed;
  expect(created).toEqual([{ url: "dist/inbox.html" }]);
  // The popup dismisses itself once the inbox is open (Firefox keeps it open otherwise).
  expect(closed).toBe(1);
});

test("renderImpactSummary fills the impact card with real values", () => {
  const doc = popupDocument();
  renderImpactSummary(doc, { acquired_total: 42, failed_total: 14 });

  expect(doc.getElementById("impact-summary")?.hidden).toBe(false);
  // 42 acquired x 5 min ~= 3.5 h; 42 of 56 finished jobs succeeded.
  expect(doc.getElementById("impact-acquired")?.textContent).toBe("42");
  expect(doc.getElementById("impact-time-saved")?.textContent).toBe("3.5 h");
  expect(doc.getElementById("impact-success-rate")?.textContent).toBe("75%");
});

test("keeps the impact title and history link in one header row", () => {
  const doc = popupDocument();
  const header = doc.getElementById("impact-header");
  expect(header?.classList.contains("impact-header")).toBe(true);
  expect(header?.querySelector("h2")?.textContent).toBe("Your papio impact");
  expect(doc.getElementById("view-history-btn")?.parentElement).toBe(header);
  expect(doc.getElementById("impact-summary")?.querySelector(":scope > #view-history-btn")).toBeNull();
});

test("renderImpactSummary hides the impact card when stats are unavailable", () => {
  const doc = popupDocument();
  // Force it visible first so hiding it is a real assertion, not a no-op
  // against popup.html's default hidden state.
  (doc.getElementById("impact-summary") as HTMLElement).hidden = false;

  renderImpactSummary(doc, null);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(true);
});

test("refreshImpactSummary populates the impact card from a daemon stats reply", async () => {
  const doc = popupDocument();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({
          ok: true,
          stats: {
            generated_at: "2026-07-25T08:00:00Z",
            acquired_total: 42,
            failed_total: 14,
            handoffs_required: 9,
            access: { open_access: 18, institutional: 20, licensed_api: 3, other: 1 },
            series: [],
          },
        }),
      },
    },
  });

  await refreshImpactSummary(doc);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(false);
  expect(doc.getElementById("impact-acquired")?.textContent).toBe("42");
  expect(doc.getElementById("impact-success-rate")?.textContent).toBe("75%");
});

test("refreshImpactSummary hides the impact card when the daemon cannot serve stats", async () => {
  const doc = popupDocument();
  (doc.getElementById("impact-summary") as HTMLElement).hidden = false;
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({ ok: false, error: { code: "timeout", message: "no reply" } }),
      },
    },
  });

  await refreshImpactSummary(doc);

  expect(doc.getElementById("impact-summary")?.hidden).toBe(true);
});

test("history launcher opens the manifest-derived history page and closes the popup", async () => {
  const doc = popupDocument();
  const created: unknown[] = [];
  let closed = 0;
  const { promise: dismissed, resolve: onClose } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        // A relocated popup page (not the dist/popup.html default) proves the
        // history URL is derived from the manifest and not a hardcoded
        // sibling literal: pre-fix code always opened "dist/history.html"
        // regardless of where the manifest actually declares the popup.
        getManifest: () => ({ action: { default_popup: "dist/ui/popup.html" } }),
        getURL: (path: string) => path,
      },
      tabs: { create: async (options: unknown) => { created.push(options); } },
    },
    window: { close: () => { closed += 1; onClose(); } },
  });
  wireHistoryLauncher(doc);

  (doc.getElementById("view-history-btn") as HTMLButtonElement).click();
  await dismissed;
  expect(created).toEqual([{ url: "dist/ui/history.html" }]);
  expect(closed).toBe(1);
});

test("Enter invokes the primary acquisition action", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderPageAcquire(doc, async () => {
    calls += 1;
    return { job_id: "job_page_acquire_001" };
  });
  wirePrimaryShortcut(doc);
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example" }, []);

  doc.dispatchEvent(new doc.defaultView!.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(1);
});

test("keeps the informed-consent guidance available", () => {
  const doc = popupDocument();
  const choices: string[] = [];
  renderTermsConsent(doc, [job({ needs_terms_consent: true })], undefined, (choice) => choices.push(choice));

  expect(doc.getElementById("terms-consent")?.hidden).toBe(false);
  (doc.getElementById("terms-consent-enable") as HTMLButtonElement).click();
  expect(choices).toEqual(["accept"]);
});

test("settings cog opens the options page and closes the popup", () => {
  const doc = popupDocument();
  let opened = 0;
  let closed = 0;
  Object.assign(globalThis, {
    chrome: { runtime: { openOptionsPage: () => { opened += 1; return Promise.resolve(); } } },
    window: { close: () => { closed += 1; } },
  });
  wireSettings(doc);
  const button = doc.getElementById("settings-btn") as unknown as HTMLButtonElement;
  button.click();
  expect(opened).toBe(1);
  expect(closed).toBe(1);
});

test("renders a one-click library grant for ungranted resolvers", () => {
  const doc = popupDocument();
  const grants: string[][] = [];
  renderResolverGrants(doc, ["https://onesearch.library.example.edu"], (origins) => grants.push(origins));

  const section = doc.getElementById("resolver-grant");
  expect(section?.hidden).toBe(false);
  expect(section?.textContent).toContain("onesearch.library.example.edu");
  const button = section?.querySelector("button") as HTMLButtonElement | null;
  expect(button?.textContent).toBe("Allow library access");

  button?.click();
  expect(grants).toEqual([["https://onesearch.library.example.edu"]]);
  expect(button?.disabled).toBe(true);
});

test("hides the library grant prompt when every resolver is granted", () => {
  const doc = popupDocument();
  renderResolverGrants(doc, [], () => {});
  const section = doc.getElementById("resolver-grant");
  expect(section?.hidden).toBe(true);
  expect(section?.children.length).toBe(0);
});

// --- collectPageMetadata DOI fallback chain -------------------------------
// SAGE (Atypon) abstract pages carry no citation_doi; the scraper must fall
// back through publication_doi, dc.Identifier[scheme=doi], and the URL path.

function pageDocument(html: string, href: string): void {
  const window = new Window({ url: href });
  window.document.write(html);
  Object.assign(globalThis, { document: window.document, location: new URL(href) });
}

test("collectPageMetadata prefers citation_doi when present", () => {
  pageDocument(
    `<html><head><meta name="citation_doi" content=" 10.1002/prefer "><meta name="publication_doi" content="10.9999/wrong"><meta name="citation_title" content="Preferred"></head></html>`,
    "https://onlinelibrary.wiley.com/doi/10.1002/prefer",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBe("10.1002/prefer");
  expect(page.title).toBe("Preferred");
});

test("collectPageMetadata reads SAGE publication_doi and dc.Identifier", () => {
  pageDocument(
    `<html><head><meta name="dc.Identifier" scheme="publisher-id" content="10.1177_1071181319631264"><meta name="dc.Identifier" scheme="doi" content="10.1177/1071181319631264"><title>Trust Engineering</title></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264",
  );
  expect(collectPageMetadata().doi).toBe("10.1177/1071181319631264");

  pageDocument(
    `<html><head><meta name="publication_doi" content="10.1177/1071181319631264"></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264",
  );
  expect(collectPageMetadata().doi).toBe("10.1177/1071181319631264");
});

test("collectPageMetadata falls back to a DOI-shaped URL path", () => {
  pageDocument(
    `<html><head><title>Bare page</title></head></html>`,
    "https://journals.sagepub.com/doi/abs/10.1177/1071181319631264?journalCode=pro",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBe("10.1177/1071181319631264");
});

test("collectPageMetadata reports no DOI on DOI-less pages", () => {
  pageDocument(
    `<html><head><title>News article</title></head></html>`,
    "https://example.com/news/story-42",
  );
  const page = collectPageMetadata();
  expect(page.doi).toBeUndefined();
  expect(page.title).toBe("News article");
});

test("collectPageMetadata classifies a JSTOR stable landing as its documented DOI", () => {
  const fixture = readFileSync(new URL("../fixtures/jstor/success.html", import.meta.url), "utf8")
    .replaceAll("2095101", "20183234");
  pageDocument(fixture, "https://www.jstor.org/stable/20183234");
  expect(collectPageMetadata().doi).toBe("10.2307/20183234");
});

test("collectPageMetadata finds a DOI in visible body text after metadata and links", () => {
  pageDocument(
    `<html><head><title>Visible paper</title></head><body><p>The DOI is 10.1000/body-layer.</p></body></html>`,
    "https://publisher.example/article",
  );
  expect(collectPageMetadata().doi).toBe("10.1000/body-layer");
});

test("openInstitutionSignIn surfaces the background failure reason", async () => {
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () => ({
          ok: false,
          error: { code: "resolver_unavailable", message: "No resolver configured yet — open a paper first" },
        }),
      },
    },
  });
  await expect(openInstitutionSignIn()).rejects.toThrow("No resolver configured yet — open a paper first");
});

test("readCurrentPageMetadata keeps JSTOR detection when page scripting is unavailable", async () => {
  popupDocument();
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        query: async () => [{ id: 7, url: "https://www.jstor.org/stable/20183234" }],
      },
      scripting: {
        executeScript: async () => {
          throw new Error("script unavailable");
        },
      },
    },
  });
  await expect(readCurrentPageMetadata()).resolves.toMatchObject({
    doi: "10.2307/20183234",
    kind: "doi",
  });
});

test("institution session uses the shared card/button styles and explains missing resolver", () => {
  const doc = popupDocument();
  renderInstitutionSession(doc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    pausedForReauth: false,
    lastProbeAt: null,
    resolverOrigin: null,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  });
  expect(doc.getElementById("institution-session")?.classList.contains("launcher-action")).toBe(true);
  expect(doc.getElementById("institution-session-signin")?.classList.contains("primary")).toBe(true);
  // No resolver: the host slot stays empty and the status line carries the
  // label plus the one actionable hint.
  expect(doc.getElementById("institution-session-origin")?.textContent).toBe("");
  expect(doc.getElementById("institution-session-status")?.textContent).toBe(
    "No resolver configured yet · Open a paper first",
  );
  expect(doc.getElementById("institution-session-dismiss")).toBeNull();
});

test("unblocked notice shows once per release stamp and does not resurrect on polls", async () => {
  const doc = popupDocument();
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    pausedForReauth: false,
    lastProbeAt: Date.now(),
    resolverOrigin: "https://example.primo.exlibrisgroup.com",
    lastAuthReturnedAt: Date.now(),
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 1,
    releasedAuthJobsAt: 1_754_200_000_000,
  };
  renderInstitutionSession(doc, state);
  const notice = doc.getElementById("institution-session-unblocked");
  expect(notice?.hidden).toBe(false);
  expect(notice?.textContent).toBe("Sign-in unblocked 1 item");

  // Simulate the fade timer having hidden the notice, then a 5s session poll
  // re-delivering the same cumulative snapshot: it must stay hidden.
  if (notice instanceof HTMLElement) notice.hidden = true;
  renderInstitutionSession(doc, { ...state });
  expect(notice?.hidden).toBe(true);

  // A NEW release event (fresh stamp) re-announces.
  renderInstitutionSession(doc, { ...state, releasedAuthJobs: 2, releasedAuthJobsAt: 1_754_200_060_000 });
  expect(notice?.hidden).toBe(false);
  expect(notice?.textContent).toBe("Sign-in unblocked 2 items");
});

test("institution sign-in errors return to a working sign-in button with the reason", async () => {
  const doc = popupDocument();
  let attempts = 0;
  renderInstitutionSession(
    doc,
    {
      enabled: true,
      intervalMinutes: 4,
      authenticated: false,
      pausedForReauth: true,
      lastProbeAt: null,
      resolverOrigin: "https://resolver.example.edu",
      lastAuthReturnedAt: null,
      queuedAuthJobs: 0,
      stalledAuthJobs: [],
      releasedAuthJobs: 0,
    },
    async () => {
      attempts += 1;
      throw new Error("Could not open the institution sign-in");
    },
  );
  const button = doc.getElementById("institution-session-signin") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(attempts).toBe(1);
  expect(button.textContent).toBe("Sign in");
  expect(button.disabled).toBe(false);
  expect(doc.getElementById("institution-session-status")?.textContent).toBe(
    "Could not open the institution sign-in",
  );
});
test("session card matrix propagates probe outcomes without hijacking a decided verdict", () => {
  const now = Date.now();
  const base = {
    enabled: true,
    intervalMinutes: 4,
    pausedForReauth: false,
    checking: false,
    likelyAuthenticated: false,
    lastProbeAt: now,
    resolverOrigin: "https://example.primo.exlibrisgroup.com",
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  };

  const noTab = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "none",
    lastProbeOutcome: "no_tab",
    lastVerdictAt: now,
  });
  expect(noTab.label).toBe("No library page open — open your library to verify");

  const noMarkers = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    lastProbeOutcome: "no_markers",
    lastVerdictAt: now,
  });
  expect(noMarkers.label).toBe("Signed-in state unclear on this page");
  expect(noMarkers.detail).toBe("papio inspected your library tab but found no sign-in indicators");
  expect(noMarkers.action).toBe("signin");

  const failed = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    lastProbeOutcome: "scan_failed",
    lastVerdictAt: now,
  });
  expect(failed.label).toBe("papio couldn't read the library page — check site access in Options");
  expect(failed.detail).toContain("via your library tab");

  const partialScan = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    lastProbeOutcome: "partial_scan",
    lastVerdictAt: now,
  });
  expect(partialScan.label).toBe("Too many library tabs to check reliably");

  const conflict = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "live_tab",
    lastProbeOutcome: "conflict",
    lastVerdictAt: now,
  });
  expect(conflict.label).toBe("Your library tabs disagree — open your library page");

  const unknown = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "unknown",
    probeSource: "none",
    lastVerdictAt: now,
  });
  expect(unknown.label).toBe("Session unknown — open your library page to verify");
  expect(unknown.action).toBe("signin");
  expect(unknown.detail).toContain("no probe evidence");

  const signedOut = deriveSessionCardState({
    ...base,
    authenticated: false,
    verdict: "out",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now,
  });
  expect(signedOut.label).toBe("Signed out or expired");
  expect(signedOut.detail).toContain("via your library tab");

  const warm = deriveSessionCardState({
    ...base,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now,
  });
  expect(warm.label).toContain("Session warm");
  expect(warm.detail).toMatch(/via your library tab · (just now|\d+m ago|\d+h ago)$/);
  // A warm session offers no sign-in action — the button is hidden, not dead.
  expect(warm.action).toBe("none");

  // A decided verdict is authoritative: a stale/contradictory outcome left
  // over from an earlier probe attempt must never downgrade a fresh "in"
  // verdict back to outcome-explanation copy. This is the exact regression
  // the ProbeOutcome/verdict split exists to prevent.
  const warmDespiteStaleOutcome = deriveSessionCardState({
    ...base,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "no_tab",
    lastVerdictAt: now,
  });
  expect(warmDespiteStaleOutcome.label).toContain("Session warm");
});

test("an origin that never landed a decisive verdict resolves to its honest probe outcome, not an eternal spinner", () => {
  // Regression for the "Checking session…" that never resolves: commitOriginProbe()
  // (keepalive.ts) only ever sets lastVerdictAt on a DECISIVE commit, so an origin
  // whose every probe has been inconclusive (no_tab, no_markers, …) keeps
  // lastVerdictAt === null forever — this is the everyday steady state whenever no
  // library tab is open, not a rare edge case. The old staleness gate ran before the
  // lastProbeOutcome switch and treated "no fresh verdict" as "still checking",
  // permanently hiding every honest outcome label below behind "Checking session…"
  // once `checking` flipped back to false.
  const now = Date.now();
  const base = {
    enabled: true,
    intervalMinutes: 4,
    pausedForReauth: false,
    checking: false,
    likelyAuthenticated: false,
    lastProbeAt: now,
    resolverOrigin: "https://example.primo.exlibrisgroup.com",
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authenticated: false,
    verdict: "unknown" as const,
    probeSource: "none" as const,
    lastVerdictAt: null,
  };

  const neverProbed = deriveSessionCardState({ ...base });
  expect(neverProbed.label).toBe("Session unknown — open your library page to verify");
  expect(neverProbed.label).not.toBe("Checking session…");

  const noTabForever = deriveSessionCardState({ ...base, lastProbeOutcome: "no_tab" });
  expect(noTabForever.label).toBe("No library page open — open your library to verify");
  expect(noTabForever.label).not.toBe("Checking session…");

  const noMarkersForever = deriveSessionCardState({
    ...base,
    probeSource: "live_tab",
    lastProbeOutcome: "no_markers",
  });
  expect(noMarkersForever.label).toBe("Signed-in state unclear on this page");
  expect(noMarkersForever.label).not.toBe("Checking session…");

  // A decisive verdict that has since aged past SESSION_STALE_MS is a different
  // case — it WAS resolved, it just isn't fresh enough to trust display-wise — and
  // must land on its own honest "needs a recheck" label rather than reusing the
  // in-flight spinner copy, which claims a check is actively running when none is.
  const agedWarm = deriveSessionCardState({
    ...base,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now - (SESSION_STALE_MS + 1),
  });
  expect(agedWarm.label).toBe("Session state unknown — recheck");
  expect(agedWarm.label).not.toBe("Checking session…");
});

test("session status lines omit degenerate probe detail and retain real evidence", () => {
  const now = Date.now();
  const defaultOrigin = "https://example.primo.exlibrisgroup.com";
  const uwaOrigin = "https://onesearch.library.example-college.edu";
  const base = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "unknown" as const,
    probeSource: "none" as const,
    lastVerdictAt: null,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: null,
    resolverOrigin: defaultOrigin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
  };

  const single = popupDocument();
  renderInstitutionSession(single, {
    ...base,
    probeSource: "live_tab",
  });
  // The label doesn't end in an ellipsis (unlike the in-flight "Checking
  // session…" copy), so sessionRowText() does NOT swallow the detail —
  // "via your library tab" is real evidence (a matching tab was actually
  // inspected) and must survive alongside the honest outcome label, which
  // is exactly what "retain real evidence" in this test's name means.
  expect(single.getElementById("institution-session-status")?.textContent).toBe(
    "Session unknown — open your library page to verify · via your library tab",
  );

  const multiple = popupDocument();
  renderInstitutionSession(multiple, {
    ...base,
    origins: [
      {
        origin: defaultOrigin,
        authenticated: false,
        verdict: "unknown",
        probeSource: "none",
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
      {
        origin: uwaOrigin,
        authenticated: false,
        verdict: "out",
        probeSource: "live_tab",
        lastProbeOutcome: "markers",
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
    ],
  });
  const statuses = multiple.querySelectorAll<HTMLElement>(
    "#institution-session-rows .institution-session-status",
  );
  expect(statuses[0]?.textContent).toBe("Session unknown — open your library page to verify");
  expect(statuses[1]?.textContent).toMatch(/^Signed out or expired · via your library tab · /);
});

test("renders independent multi-origin session rows and targets each sign-in origin", async () => {
  const now = Date.now();
  const defaultOrigin = "https://example.primo.exlibrisgroup.com";
  const uwaOrigin = "https://onesearch.library.example-college.edu";
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "unknown" as const,
    probeSource: "none" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: defaultOrigin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    origins: [
      {
        origin: defaultOrigin,
        authenticated: false,
        verdict: "unknown" as const,
        probeSource: "none" as const,
        lastProbeOutcome: "no_markers" as const,
        lastVerdictAt: null,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
      {
        origin: uwaOrigin,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "live_tab" as const,
        lastProbeOutcome: "markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
    ],
  };
  // The warm-and-fresh steady state is filtered; only actionable rows render.
  expect(deriveSessionRows(state)).toEqual([
    expect.objectContaining({ origin: defaultOrigin, action: "signin" }),
    expect.objectContaining({ origin: uwaOrigin, label: "Signed out or expired", action: "signin" }),
  ]);

  const doc = popupDocument();
  const targets: string[] = [];
  renderInstitutionSession(doc, state, async (origin) => {
    if (origin !== undefined) targets.push(origin);
  });
  const rows = doc.querySelectorAll(".institution-session-origin-row");
  expect(rows).toHaveLength(2);
  expect(rows[0]?.textContent).toContain("example.primo.exlibrisgroup.com");
  expect(rows[1]?.textContent).toContain("onesearch.library.example-college.edu");
  expect(doc.getElementById("institution-session-row")).toBeNull();
  const buttons = Array.from(doc.querySelectorAll<HTMLButtonElement>(".institution-session-origin-row button"));
  expect(buttons).toHaveLength(2);
  expect(buttons[0]?.hidden).toBe(false);
  expect(buttons[1]?.hidden).toBe(false);
  expect(buttons[1]?.getAttribute("aria-describedby")).toBe("institution-session-status-1");
  buttons[1]?.click();
  await Promise.resolve();
  expect(targets).toEqual([uwaOrigin]);
});
test("binds waiting demand to its warm origin instead of a stale secondary row", () => {
  const now = Date.now();
  const originA = "https://resolver.example.edu";
  const originB = "https://stale.other.example";
  const waitingJob = job({
    job_id: "waiting-a",
    status: "auth_pending",
    waiting_for_session: true,
    expected: { title: "Paper A" },
  });
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in" as const,
    probeSource: "live_tab" as const,
    lastProbeOutcome: "markers" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: originA,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 1,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authDemand: [{ job_id: waitingJob.job_id, origin: originA }],
    origins: [
      {
        origin: originA,
        authenticated: true,
        verdict: "in" as const,
        probeSource: "live_tab" as const,
        lastProbeOutcome: "markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
      {
        origin: originB,
        authenticated: true,
        verdict: "in" as const,
        probeSource: "keepalive_tab" as const,
        lastProbeOutcome: "markers" as const,
        lastVerdictAt: now - 34 * 60 * 60 * 1000,
        checking: false,
        likelyAuthenticated: true,
        pausedForReauth: false,
        lastProbeAt: now - 34 * 60 * 60 * 1000,
        dirtySince: null,
      },
    ],
  };
  expect(deriveSessionRows(state)).toEqual([
    expect.objectContaining({ origin: originA, label: "Session warm" }),
  ]);
  const doc = popupDocument();
  renderInstitutionSession(doc, state);
  renderNeedsAttention(
    doc,
    [waitingJob],
    [],
    async () => {},
    async () => {},
    [],
    async () => {},
    async () => true,
    state.authDemand,
  );
  expect(doc.getElementById("institution-session-origin")?.textContent).toBe("resolver.example.edu");

  expect(doc.querySelector(".institution-session-origin-row")).toBeNull();
  expect(doc.getElementById("institution-session-waiting")?.textContent).toContain("resolver.example.edu");
});
test("hides unrelated session rows when a waiting job has no safe origin binding", () => {
  const now = Date.now();
  const originA = "https://resolver-a.example.edu";
  const originB = "https://resolver-b.example.edu";
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "out" as const,
    probeSource: "none" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: originA,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authDemand: [],
    origins: [
      {
        origin: originA,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "none" as const,
        lastProbeOutcome: "no_markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
      {
        origin: originB,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "none" as const,
        lastProbeOutcome: "no_markers" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
    ],
  };
  const waitingJob = job({ job_id: "unmapped-waiting", status: "auth_pending" });

  expect(deriveSessionRows(state, [waitingJob])).toEqual([]);
  expect(deriveSessionRows(state)).toHaveLength(2);
});
test("waiting demand for origin B cannot display origin A as its blocker", () => {
  const now = Date.now();
  const originA = "https://resolver.example.edu";
  const originB = "https://other-resolver.example.edu";
  const waitingJob = job({ job_id: "waiting-b", status: "auth_pending", waiting_for_session: true });
  const state = {
    enabled: true,
    intervalMinutes: 4,
    authenticated: false,
    verdict: "out" as const,
    probeSource: "none" as const,
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: originB,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 1,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    authDemand: [{ job_id: waitingJob.job_id, origin: originB }],
    origins: [
      {
        origin: originA,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "none" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
      {
        origin: originB,
        authenticated: false,
        verdict: "out" as const,
        probeSource: "none" as const,
        lastVerdictAt: now,
        checking: false,
        likelyAuthenticated: false,
        pausedForReauth: false,
        lastProbeAt: now,
        dirtySince: null,
      },
    ],
  };
  expect(deriveSessionRows(state).map((row) => row.origin)).toEqual([originB]);
  const doc = popupDocument();
  renderInstitutionSession(doc, state);
  renderNeedsAttention(
    doc,
    [waitingJob],
    [],
    async () => {},
    async () => {},
    [],
    async () => {},
    async () => true,
    state.authDemand,
  );
  expect(doc.getElementById("institution-session-waiting")?.textContent).toContain("other-resolver.example.edu");
  expect(doc.querySelector(".institution-session-waiting-status")?.textContent).toBe(
    "Waiting for other-resolver.example.edu sign-in — another paper's tab is at the login page",
  );
});



test("a calm warm session renders no institution card at all", () => {
  const now = Date.now();
  const origin = "https://example.primo.exlibrisgroup.com";
  const doc = popupDocument();
  renderInstitutionSession(doc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: origin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    origins: [{
      origin,
      authenticated: true,
      verdict: "in",
      probeSource: "live_tab",
      lastProbeOutcome: "markers",
      lastVerdictAt: now,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      lastProbeAt: now,
      dirtySince: null,
    }],
  });
  // Quiet means live: a warm, freshly-verified session with nothing waiting
  // is the assumed steady state and earns zero pixels.
  expect(doc.getElementById("institution-session")?.hidden).toBe(true);

  // The same session gone stale earns the card back.
  const staleDoc = popupDocument();
  const stale = 11 * 60 * 1000;
  renderInstitutionSession(staleDoc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now - stale,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now - stale,
    resolverOrigin: origin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 0,
    origins: [{
      origin,
      authenticated: true,
      verdict: "in",
      probeSource: "live_tab",
      lastProbeOutcome: "markers",
      lastVerdictAt: now - stale,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      lastProbeAt: now - stale,
      dirtySince: null,
    }],
  });
  expect(staleDoc.getElementById("institution-session")?.hidden).toBe(false);
});

test("leftover-tabs card stays hidden at zero and renders a pluralized count", () => {
  const doc = popupDocument();
  renderLeftoverTabs(doc, 0, async () => 0);
  const section = doc.getElementById("leftover-tabs");
  expect(section?.hasAttribute("hidden")).toBe(true);

  renderLeftoverTabs(doc, 3, async () => 3);
  expect(section?.hasAttribute("hidden")).toBe(false);
  expect(doc.getElementById("leftover-tabs-message")?.textContent).toContain("3 papio tabs");

  renderLeftoverTabs(doc, 1, async () => 1);
  expect(doc.getElementById("leftover-tabs-message")?.textContent).toContain("1 papio tab");
});

test("leftover-tabs cleanup uses the latest callback after a rerender", async () => {
  const doc = popupDocument();
  const calls: string[] = [];
  renderLeftoverTabs(doc, 1, async () => {
    calls.push("old");
    return 1;
  });
  renderLeftoverTabs(doc, 1, async () => {
    calls.push("new");
    return 1;
  });
  (doc.getElementById("leftover-tabs-cleanup") as HTMLButtonElement).click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toEqual(["new"]);
});

test("leftover-tabs review keeps the card and a failure re-arms the button", async () => {
  const doc = popupDocument();
  let calls = 0;
  renderLeftoverTabs(doc, 2, async () => {
    calls += 1;
    if (calls === 1) throw new Error("cleanup blocked");
    return 2;
  });
  const section = doc.getElementById("leftover-tabs");
  const button = doc.getElementById("leftover-tabs-cleanup") as HTMLButtonElement;

  button.click();
  expect(button.disabled).toBe(true);
  await Promise.resolve();
  await Promise.resolve();
  // First attempt failed: the card persists and the button re-arms.
  expect(section?.hasAttribute("hidden")).toBe(false);
  expect(button.disabled).toBe(false);
  expect(button.textContent).toBe("Review in browser");

  button.click();
  await Promise.resolve();
  await Promise.resolve();
  expect(calls).toBe(2);
  expect(section?.hasAttribute("hidden")).toBe(false);
  expect(button.disabled).toBe(false);
  expect(button.textContent).toBe("Review in browser");
});

test("a release notice keeps the card with a warm summary, never a bare heading", () => {
  const now = Date.now();
  const origin = "https://example.primo.exlibrisgroup.com";
  const doc = popupDocument();
  renderInstitutionSession(doc, {
    enabled: true,
    intervalMinutes: 4,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now,
    checking: false,
    likelyAuthenticated: false,
    pausedForReauth: false,
    lastProbeAt: now,
    resolverOrigin: origin,
    lastAuthReturnedAt: null,
    queuedAuthJobs: 0,
    stalledAuthJobs: [],
    releasedAuthJobs: 1,
    releasedAuthJobsAt: now,
    origins: [{
      origin,
      authenticated: true,
      verdict: "in",
      probeSource: "live_tab",
      lastProbeOutcome: "markers",
      lastVerdictAt: now,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: false,
      lastProbeAt: now,
      dirtySince: null,
    }],
  });
  expect(doc.getElementById("institution-session")?.hidden).toBe(false);
  expect(doc.getElementById("institution-session-status")?.textContent).toBe("All sessions warm");
  expect(doc.getElementById("institution-session-unblocked")?.textContent).toContain(
    "Sign-in unblocked 1 item",
  );
  expect((doc.getElementById("institution-session-signin") as HTMLButtonElement).hidden).toBe(true);
});

// `captureFixture`'s send step routes through `encodePageCapture`'s real
// `CompressionStream("gzip")` pipeline, which settles on a real event-loop
// turn the click handler's fire-and-forget `.then()` doesn't expose to the
// caller. Rather than guess a wait duration, the fake `sendMessage` below
// signals a `Promise.withResolvers()` gate the instant the pipeline reaches
// it (i.e. once gzip has already finished) — the test awaits that real
// signal, then a few microtask flushes for the remaining `.then()` chain
// back into `statusEl.textContent`.
async function flushMicrotasks(rounds = 10): Promise<void> {
  for (let i = 0; i < rounds; i++) await Promise.resolve();
}

test("capture panel status line surfaces the daemon-upgrade reason from a structured capture_failed refusal", async () => {
  const doc = popupDocument();
  wireCapture(doc, ["page_capture_terms_v1"]);
  const { promise: sent, resolve: onSent } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 7 }] },
      scripting: {
        executeScript: async () => [{
          result: {
            html: "<html><body><h1>Trust</h1></body></html>",
            origin: "https://www.proquest.com",
            path: "/docview/1",
          },
        }],
      },
      runtime: {
        // Mirrors background.ts's runtimeFailure("capture_failed", …) shape
        // for a daemon too old to accept the `terms` scenario.
        sendMessage: async () => {
          onSent();
          return {
            ok: false,
            error: {
              code: "capture_failed",
              message:
                "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
            },
          };
        },
      },
    },
  });

  const provider = doc.getElementById("capture-provider") as HTMLSelectElement;
  const scenario = doc.getElementById("capture-scenario") as HTMLSelectElement;
  provider.value = "proquest";
  scenario.value = "terms";
  const button = doc.getElementById("capture-btn") as HTMLButtonElement;
  button.click();
  await sent;
  await flushMicrotasks();

  expect(doc.getElementById("capture-status")?.textContent).toBe(
    "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
  );
  expect(button.disabled).toBe(false);
});

test("capture panel status line falls back to the generic message when the refusal carries no reason", async () => {
  const doc = popupDocument();
  wireCapture(doc, ["page_capture_terms_v1"]);
  const { promise: sent, resolve: onSent } = Promise.withResolvers<void>();
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 7 }] },
      scripting: {
        executeScript: async () => [{
          result: {
            html: "<html><body><h1>Trust</h1></body></html>",
            origin: "https://www.proquest.com",
            path: "/docview/1",
          },
        }],
      },
      runtime: {
        // A dropped connection or malformed reply carries no `error`; the
        // operator must still see something actionable, not "undefined".
        sendMessage: async () => {
          onSent();
          return undefined;
        },
      },
    },
  });

  const provider = doc.getElementById("capture-provider") as HTMLSelectElement;
  const scenario = doc.getElementById("capture-scenario") as HTMLSelectElement;
  provider.value = "proquest";
  scenario.value = "success";
  const button = doc.getElementById("capture-btn") as HTMLButtonElement;
  button.click();
  await sent;
  await flushMicrotasks();

  expect(doc.getElementById("capture-status")?.textContent).toBe("could not send the capture to papio");
  expect(button.disabled).toBe(false);
});

// refresh()'s probe/snapshot split (`sessionProbedThisPopup` in popup.ts) is
// module-level state, so each fixture needs its own fresh module instance —
// the same versioned-specifier pattern history.test.ts/inbox.test.ts/
// options.test.ts already use for their own module-local UI state. `chrome`
// is withheld from globalThis until AFTER the import completes: popup.ts's
// own import-time bootstrap (guarded on `typeof chrome !== "undefined"`)
// would otherwise fire a real refresh and start a real 5-second
// setInterval, and the whole point here is to drive `refresh()` by hand.
let popupRefreshImportSerial = 0;

function sessionReplyFixture(overrides: Record<string, unknown> = {}): { state: Record<string, unknown> } {
  return {
    state: {
      enabled: true,
      authenticated: true,
      pausedForReauth: false,
      lastProbeAt: null,
      lastAuthReturnedAt: null,
      queuedAuthJobs: 0,
      stalledAuthJobs: [],
      releasedAuthJobs: 0,
      resolverOrigin: "https://resolver.example.edu",
      ...overrides,
    },
  };
}

async function popupRefreshHarness(
  reply: (message: Record<string, unknown>) => unknown = () => sessionReplyFixture(),
): Promise<{ document: Document; refresh: () => Promise<void>; requests: Record<string, unknown>[] }> {
  const window = new Window();
  window.document.write(readFileSync(new URL("../src/popup.html", import.meta.url), "utf8"));
  Object.assign(globalThis, {
    document: window.document,
    Event: window.Event,
    HTMLElement: window.HTMLElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLSelectElement: window.HTMLSelectElement,
    // A prior test may have left a stub `chrome` on globalThis: hold it at
    // `undefined` through the import so popup.ts's own bootstrap (guarded on
    // `typeof chrome !== "undefined"`) stays skipped regardless of test order.
    chrome: undefined,
  });
  popupRefreshImportSerial += 1;
  const popup = (await import(`../src/popup.ts?popup-refresh-test=${popupRefreshImportSerial}`)) as {
    refresh: () => Promise<void>;
  };
  const requests: Record<string, unknown>[] = [];
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (message: Record<string, unknown>) => {
          requests.push(message);
          return reply(message);
        },
      },
      storage: { local: { get: async () => ({}) } },
      tabs: { query: async () => [] },
    },
  });
  return { document: window.document as unknown as Document, refresh: popup.refresh, requests };
}

function sessionMessageTypes(requests: Record<string, unknown>[]): unknown[] {
  return requests
    .filter((r) => r["type"] === SESSION_PROBE_MESSAGE || r["type"] === SESSION_STATE_MESSAGE)
    .map((r) => r["type"]);
}

test("the popup's first refresh sends exactly one session probe, never a snapshot read", async () => {
  const h = await popupRefreshHarness();
  await h.refresh();
  expect(sessionMessageTypes(h.requests)).toEqual([SESSION_PROBE_MESSAGE]);
});

test("every refresh after the first reads a snapshot instead of repeating the probe", async () => {
  const h = await popupRefreshHarness();
  await h.refresh();
  await h.refresh();
  await h.refresh();
  expect(sessionMessageTypes(h.requests)).toEqual([
    SESSION_PROBE_MESSAGE,
    SESSION_STATE_MESSAGE,
    SESSION_STATE_MESSAGE,
  ]);
});

test("the 2-second checking retry reads a snapshot, never re-injecting a probe", async () => {
  const originalSetTimeout = globalThis.setTimeout;
  const retries: Array<() => void> = [];
  globalThis.setTimeout = ((callback: () => void, delay?: number) => {
    if (delay === 2_000) {
      retries.push(callback);
      return 0;
    }
    return originalSetTimeout(callback, delay);
  }) as typeof globalThis.setTimeout;
  try {
    const h = await popupRefreshHarness(() => sessionReplyFixture({ checking: true }));
    await h.refresh();
    expect(sessionMessageTypes(h.requests)).toEqual([SESSION_PROBE_MESSAGE]);
    expect(retries).toHaveLength(1);

    h.requests.length = 0;
    retries[0]?.();
    await flushMicrotasks();
    expect(h.requests).toEqual([{ type: SESSION_STATE_MESSAGE }]);
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});
test("malformed demand metadata falls back to the legacy session state", async () => {
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async () =>
          sessionReplyFixture({
            authDemand: [{ job_id: "waiting-a", origin: "https://resolver.example.edu", extra: true }],
          }),
      },
    },
  });
  const state = await requestSessionState();
  expect(state).toBeDefined();
  expect(state?.authDemand).toBeUndefined();
  expect(state?.resolverOrigin).toBe("https://resolver.example.edu");
});
test("rejects malformed core and per-origin session origins", async () => {
  const malformedOrigins = [
    "https://resolver.example.edu/path",
    "https://resolver.example.edu?query=1",
    "https://resolver.example.edu#fragment",
    "https://user:password@resolver.example.edu",
  ];
  for (const resolverOrigin of malformedOrigins) {
    Object.assign(globalThis, {
      chrome: {
        runtime: {
          sendMessage: async () => sessionReplyFixture({ resolverOrigin }),
        },
      },
    });
    await expect(requestSessionState()).resolves.toBeUndefined();
  }

  const validSnapshot = {
    origin: "https://resolver.example.edu",
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: Date.now(),
    checking: false,
    likelyAuthenticated: true,
    pausedForReauth: false,
    lastProbeAt: Date.now(),
  };
  for (const origin of malformedOrigins) {
    Object.assign(globalThis, {
      chrome: {
        runtime: {
          sendMessage: async () =>
            sessionReplyFixture({
              origins: [{ ...validSnapshot, origin }],
            }),
        },
      },
    });
    await expect(requestSessionState()).resolves.toBeUndefined();
  }
});
