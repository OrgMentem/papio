// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { Window } from "happy-dom";

import {
  acquireCurrentPage,
  sendCurrentPDF,
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
  selectedManualDeliveryTarget,
  pageDeliveryJob,
  deliveryStatusText,
  sessionWarmForJob,
  renderResolverGrants,
  renderTermsConsent,
  wireCapture,
  wireDevTools,
  wireHistoryLauncher,
  wireInboxLauncher,
  wirePrimaryShortcut,
  wirePageBulkScanLauncher,
  wireSettings,
  startPageBulkScan,
  acknowledgeInPage,
  renderInPageAcknowledgement,
  announcePopupOperation,
  beginPopupOperation,
  finishPopupOperation,
  popupOperation,
  prunePopupOperations,
  PAGE_BULK_SCAN_MESSAGE,
  PAGE_CHANGED_MESSAGE,
  type InPageAcknowledgementKind,
  type PageActionBinding,
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

test("delivery feedback distinguishes viewer action from adopted success", () => {
  expect(deliveryStatusText({
    job_id: "job-viewer",
    initiated_at: 1,
    status: "waiting_manual",
    error: "Use the PDF viewer Download button — papio will adopt that authorized file",
  })).toMatch(/viewer Download button/i);
  expect(deliveryStatusText({
    job_id: "job-adopted",
    initiated_at: 2,
    status: "adopted",
  })).toBe("papio adopted PDF (validating)");
});

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
  // The popup prints the primary line, not the five-bucket inventory. Waiting on
  // you already owns the turn, so the companion adds nothing here.
  expect(doc.getElementById("popup-pulse-primary")?.textContent).toBe("Waiting on you · 35 decisions");
  expect(doc.getElementById("popup-pulse-buckets")).toBeNull();
  // Full validated measurements stay reachable without occupying a line.
  expect(doc.getElementById("popup-pulse")?.getAttribute("title")).toContain("Nonterminal breakdown");
  expect(doc.getElementById("popup-pulse")?.dataset.state).toBe("Waiting on you");
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

test("puts every current-page action in one rail, in the accepted hierarchy", () => {
  const doc = popupDocument();

  expect(doc.querySelector("h1")).toBeNull();
  // Acquire is no longer a header utility: the header owns only inbox and
  // settings, and both page actions are siblings in the rail.
  const headerActions = doc.querySelector(".header-actions");
  expect(Array.from(headerActions?.children ?? []).map((child) => child.id)).toEqual([
    "open-inbox-btn",
    "settings-btn",
  ]);
  expect(doc.getElementById("page-acquire-btn")?.closest("header")).toBeNull();
  const rail = doc.getElementById("current-page-actions");
  expect(doc.getElementById("page-acquire-btn")?.closest("#current-page-actions")).toBe(rail);
  expect(doc.getElementById("page-bulk-scan-btn")?.closest("#current-page-actions")).toBe(rail);
  expect(doc.getElementById("page-acquire-status")?.closest("#current-page-actions")).toBe(rail);
  expect(doc.getElementById("page-bulk-scan-status")?.closest("#current-page-actions")).toBe(rail);
  expect(doc.getElementById("page-acquire-live")?.closest("#current-page-actions")).toBe(rail);
  // The standalone page-bulk card is gone.
  expect(doc.querySelector(".page-bulk-scan")).toBeNull();

  // Exact visible/DOM order of the popup's own sections.
  const order = Array.from(doc.querySelectorAll("main > section, main > details")).map(
    (node) => node.id || node.className,
  );
  expect(order).toEqual([
    "current-page-actions",
    "needs-you-section",
    "institution-session",
    "leftover-tabs",
    "popup-catchup",
    "resolver-grant",
    "terms-consent",
    "impact-summary",
    "capture",
  ]);
  // The global pulse still precedes the rail, and the abnormal daemon band
  // precedes both.
  const beforeMain = Array.from(doc.body.children).map((node) => node.id || node.tagName.toLowerCase());
  expect(beforeMain.indexOf("daemon-status")).toBeLessThan(beforeMain.indexOf("popup-pulse"));
  expect(beforeMain.indexOf("popup-pulse")).toBeLessThan(beforeMain.indexOf("main"));

  // Nothing in the rail reserves pixels while empty.
  expect(rail?.hidden).toBe(true);
  expect(doc.getElementById("page-acquire")?.hidden).toBe(true);
  expect(doc.getElementById("page-acquire-btn")?.hidden).toBe(true);
  expect(doc.getElementById("page-bulk-scan-btn")?.hidden).toBe(true);
  expect(doc.getElementById("page-bulk-consent")?.hidden).toBe(true);
  expect(doc.getElementById("page-acquire-doi")).toBeNull();
  expect(doc.getElementById("page-acquire-context")).toBeNull();
  expect(doc.getElementById("daemon-footer")).toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.querySelector("svg")).toBeNull();
  expect(doc.getElementById("page-acquire-btn")?.textContent).toBe("Acquire");
  expect(doc.getElementById("page-acquire-btn")?.classList.contains("primary")).toBe(true);
  expect(doc.getElementById("open-inbox-btn")?.getAttribute("aria-label")).toBe("Open inbox");
  expect(doc.getElementById("needs-you-section")?.hidden).toBe(true);

  // One stable announcer; local results carry no live role of their own.
  const announcer = doc.getElementById("popup-operation-status");
  expect(announcer?.getAttribute("role")).toBe("status");
  expect(announcer?.getAttribute("aria-live")).toBe("polite");
  for (const id of ["page-acquire-status", "page-bulk-scan-status", "page-acquire-live", "open-inbox-status"]) {
    expect(doc.getElementById(id)?.getAttribute("aria-live")).toBeNull();
  }
  // The daemon band and pulse keep their separate liveness responsibility.
  expect(doc.getElementById("daemon-status")?.getAttribute("aria-live")).toBe("polite");
  expect(doc.getElementById("popup-pulse-primary")?.getAttribute("aria-live")).toBe("polite");
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
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example", tab_id: 1, tab_url: "https://doi.org/10.1000/example" }, []);

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
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example", tab_id: 1, tab_url: "https://doi.org/10.1000/example" }, []);

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  button.click();
  await Promise.resolve();
  await Promise.resolve();

  expect(button.disabled).toBe(true);
  // No raw job id: the popup says whether the click landed, and the inbox owns
  // the durable identifier.
  expect(button.title).toBe("Added to papio");
  expect(button.getAttribute("aria-disabled")).toBe("true");
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Added to papio");
  expect(doc.getElementById("page-acquire-status")?.dataset.tone).toBe("success");
  expect(doc.getElementById("page-acquire-status")?.textContent).not.toContain("job_");
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
    { url: "https://papers.example.edu/download/paper.pdf?download=1", kind: "pdf", tab_id: 17, tab_url: "https://papers.example.edu/download/paper.pdf?download=1" },
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

test("Send PDF does not attach an Open pin job_id for a DOI-less unmatched tab", async () => {
  popupDocument();
  const sent: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        query: async () => [{
          id: 77,
          url: "https://provider.example.edu/download/article.pdf",
          contentType: "application/pdf",
        }],
      },
      scripting: { executeScript: async () => { throw new Error("PDF viewer blocks scripting"); } },
      runtime: {
        sendMessage: async (message: unknown) => {
          sent.push(message);
          return { ok: true, state: "sending", message: "papio will identify this PDF from the file" };
        },
      },
    },
  });

  await expect(
    sendCurrentPDF({
      url: "https://provider.example.edu/download/article.pdf",
      kind: "pdf",
      tab_id: 77,
      tab_url: "https://provider.example.edu/download/article.pdf",
    }),
  ).resolves.toMatchObject({
    ok: true,
    state: "sending",
  });
  expect(sent).toEqual([{
    type: "papio.delivery.start",
    request: {
      tab_id: 77,
      url: "https://provider.example.edu/download/article.pdf",
    },
  }]);
});

test("pageDeliveryJob prefers tab then DOI then pin-on-this-tab", () => {
  const pin = job({
    job_id: "job_pin",
    tab_id: 11,
    status: "awaiting_download",
    manual_delivery_target: true,
    expected: { doi: "10.1/pin" },
  });
  const other = job({
    job_id: "job_other",
    tab_id: 22,
    status: "awaiting_download",
    expected: { doi: "10.1/other" },
  });
  expect(pageDeliveryJob([pin, other], { tab_id: 22 })).toBe(other);
  expect(pageDeliveryJob([pin, other], { doi: "10.1/other" })).toBe(other);
  expect(pageDeliveryJob([pin], { tab_id: 11 })).toBe(pin);
  expect(pageDeliveryJob([pin], { tab_id: 99 })).toBeUndefined();
});

test("manual delivery target selection is unique and fail-closed", () => {
  const first = job({
    job_id: "job_manual_target_one",
    tab_id: -1,
    status: "awaiting_download",
    manual_delivery_target: true,
  });
  const second = job({
    job_id: "job_manual_target_two",
    tab_id: -1,
    status: "awaiting_download",
    manual_delivery_target: true,
  });
  expect(selectedManualDeliveryTarget([first])).toBe(first);
  expect(selectedManualDeliveryTarget([first, second])).toBeUndefined();
  expect(selectedManualDeliveryTarget([{ ...first, access_mode: "delegated" } as unknown as ActiveJob])).toBeDefined();
  expect(selectedManualDeliveryTarget([{ ...first, tab_id: 42 } as unknown as ActiveJob])).toBeDefined();
  expect(selectedManualDeliveryTarget([{ ...first, status: "accepted" as const } as unknown as ActiveJob])).toBeUndefined();
});
test("does not send a DOI-less scraped page to the daemon", async () => {
  popupDocument();
  let messages = 0;
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        // Binding validation re-reads the active tab, so the fake must answer
        // with the same tab id and byte-identical URL the binding carries.
        query: async () => [{ id: 1, url: "https://publisher.example.edu/article/42" }],
      },
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

  await expect(
    acquireCurrentPage({
      url: "https://publisher.example.edu/article/42",
      title: "A DOI-less page",
      tab_id: 1,
      tab_url: "https://publisher.example.edu/article/42",
    }),
  ).resolves.toEqual({ error: "no DOI found on this page" });
  expect(messages).toBe(0);
});

test("renders a live, honest status card for a local in-flight acquisition", () => {
  const doc = popupDocument();
  const now = Date.now();
  let openedInbox = 0;
  let openedTab = 0;
  renderPageContext(
    doc,
    { url: "https://doi.org/10.1000/example", doi: "10.1000/example", tab_id: 1, tab_url: "https://doi.org/10.1000/example" },
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
  const page = { url: "https://doi.org/10.1000/paper-b", doi: "10.1000/paper-b", tab_id: 1, tab_url: "https://doi.org/10.1000/paper-b" };

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
  // One heading, always: a per-kind heading read as a different section.
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Needs you");
  expect(section?.querySelector(".needs-you-paper")?.textContent).toBe(
    "Security check — sciencedirect.com",
  );
  const button = section?.querySelector("button") as HTMLButtonElement;
  expect(button.textContent).toBe("Open tab");
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
  expect(doc.getElementById("needs-you-heading")?.textContent).toBe("Needs you");
  expect(doc.getElementById("needs-you-message")?.textContent).toContain("Allow the blocked source here");
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
  // The button keeps naming its action; the outcome lives in the row's own
  // persistent, non-live result so a rerender cannot erase it.
  expect(buttons[0]?.textContent).toBe("Allow");
  const result = section?.querySelectorAll(".popup-result")[0] as HTMLElement | undefined;
  expect(result?.textContent).toBe("Allowed");
  expect(result?.dataset.tone).toBe("success");
  expect(result?.getAttribute("aria-live")).toBeNull();
  expect(doc.getElementById("popup-operation-status")?.textContent).toBe("Allowed");
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
  // Both figures are measured: 42 acquired jobs, 42 of 56 finished jobs succeeded.
  expect(doc.getElementById("impact-acquired")?.textContent).toBe("42");
  expect(doc.getElementById("impact-success-rate")?.textContent).toBe("75%");
});

test("the impact footer is one measured line — never an invented time saved", () => {
  const doc = popupDocument();
  renderImpactSummary(doc, { acquired_total: 42, failed_total: 14 });

  // papio measures counts, not clocks: no estimated-time-saved figure, ETA, or
  // acquisition-progress percentage may return to this footer under any wording.
  expect(doc.getElementById("impact-time-saved")).toBeNull();
  const section = doc.getElementById("impact-summary") as HTMLElement;
  expect(section.textContent).not.toMatch(/saved|hours?\b|\bh\b|minutes|remaining|eta/i);
  // One line, one link. The heading and the two-cell metric grid are gone.
  expect(section.querySelector("h2")).toBeNull();
  expect(section.querySelectorAll(".impact-metric")).toHaveLength(0);
  expect(doc.getElementById("impact-header")).toBeNull();
  expect(section.querySelector(".impact-line")?.textContent?.replace(/\s+/g, " ").trim()).toBe(
    "42 acquired · 75% success",
  );
  expect(doc.getElementById("view-history-btn")?.textContent).toBe("View history");
  expect(doc.getElementById("view-history-btn")?.parentElement).toBe(section);
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
  renderPageContext(doc, { url: "https://doi.org/10.1000/example", doi: "10.1000/example", tab_id: 1, tab_url: "https://doi.org/10.1000/example" }, []);

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

test("SAGE's epub journal viewer becomes a direct Send PDF surface", async () => {
  const viewerURL = "https://journals.sagepub.com/doi/epub/10.1177/14757257231222647";
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        query: async () => [{ id: 17, url: viewerURL, title: "Against All Odds" }],
      },
      scripting: {
        executeScript: async () => [{
          result: {
            url: viewerURL,
            doi: "10.1177/14757257231222647",
            title: "Against All Odds",
          },
        }],
      },
    },
  });
  await expect(readCurrentPageMetadata()).resolves.toEqual({
    url: "https://journals.sagepub.com/doi/pdf/10.1177/14757257231222647?download=true",
    doi: "10.1177/14757257231222647",
    title: "Against All Odds",
    kind: "pdf",
    tab_id: 17,
    tab_url: viewerURL,
  });
});

test("Taylor and Francis's epdf viewer becomes a direct Send PDF surface", async () => {
  const viewerURL = "https://www.tandfonline.com/doi/epdf/10.1080/10705511.2018.1431046?needAccess=true";
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 18, url: viewerURL, title: "Drawing Conclusions" }] },
      scripting: {
        executeScript: async () => [{
          result: {
            url: viewerURL,
            doi: "10.1080/10705511.2018.1431046",
            title: "Drawing Conclusions",
          },
        }],
      },
    },
  });
  await expect(readCurrentPageMetadata()).resolves.toEqual({
    url: "https://www.tandfonline.com/doi/pdf/10.1080/10705511.2018.1431046?download=true",
    doi: "10.1080/10705511.2018.1431046",
    title: "Drawing Conclusions",
    kind: "pdf",
    tab_id: 18,
    tab_url: viewerURL,
  });
});

test("Cell's PII PDF response becomes Send PDF even when scripting is unavailable", async () => {
  const viewerURL = "https://www.cell.com/action/showPdf?pii=S2405-8440%2817%2930127-5";
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 19, url: viewerURL, title: "Latent profile analysis" }] },
      scripting: { executeScript: async () => { throw new Error("PDF viewer blocks scripting"); } },
    },
  });
  await expect(readCurrentPageMetadata()).resolves.toEqual({
    url: viewerURL,
    title: "Latent profile analysis",
    kind: "pdf",
    tab_id: 19,
    tab_url: viewerURL,
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

  // A decisive verdict that has aged past SESSION_STALE_MS remains evidence:
  // the popup says what it last verified while scheduling one bounded recheck,
  // rather than falsely presenting the durable `in` verdict as unknown.
  const agedWarm = deriveSessionCardState({
    ...base,
    authenticated: true,
    verdict: "in",
    probeSource: "live_tab",
    lastProbeOutcome: "markers",
    lastVerdictAt: now - (SESSION_STALE_MS + 1),
  });
  expect(agedWarm.label).toBe("Last verified signed in — rechecking");
  expect(agedWarm.action).toBe("none");
  expect(agedWarm.label).not.toContain("unknown");
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
    // The retry reads a snapshot; it must never re-inject the probe. Its repaint
    // now goes through refresh() so it sits inside the generation fence, which is
    // why other refresh reads appear here too.
    expect(sessionMessageTypes(h.requests)).toContain(SESSION_STATE_MESSAGE);
    expect(sessionMessageTypes(h.requests)).not.toContain(SESSION_PROBE_MESSAGE);
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});

test("a stale decisive session schedules one re-probe without discarding its verdict", async () => {
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
    const lastVerdictAt = Date.now() - (SESSION_STALE_MS + 1);
    const h = await popupRefreshHarness(() =>
      sessionReplyFixture({
        authenticated: true,
        verdict: "in",
        probeSource: "live_tab",
        lastProbeOutcome: "markers",
        lastProbeAt: lastVerdictAt,
        lastVerdictAt,
      }),
    );
    await h.refresh();
    expect(sessionMessageTypes(h.requests)).toEqual([SESSION_PROBE_MESSAGE]);
    expect(retries).toHaveLength(1);

    h.requests.length = 0;
    retries[0]?.();
    await flushMicrotasks();
    // A stale decisive verdict earns exactly one re-probe.
    expect(sessionMessageTypes(h.requests)[0]).toBe(SESSION_PROBE_MESSAGE);
    expect(
      sessionMessageTypes(h.requests).filter((type) => type === SESSION_PROBE_MESSAGE),
    ).toHaveLength(1);

    h.requests.length = 0;
    await h.refresh();
    // Later refreshes are snapshot-only, and the stale verdict does not schedule
    // a second retry.
    expect(sessionMessageTypes(h.requests)).toEqual([SESSION_STATE_MESSAGE]);
    expect(retries).toHaveLength(1);
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

test("popup with a known job or manual target in PDF context does not show no-DOI copy", async () => {
  const doc = popupDocument();
  const fakeSend = async () => ({
    ok: true as const,
    state: "sending" as const,
    message: "papio will identify this PDF from the file",
  });
  renderPageAcquire(doc, async () => ({ ok: true, state: "sending", job_id: "unused" }), fakeSend);
  const manual = job({
    job_id: "job_manual_1",
    tab_id: -1,
    status: "awaiting_download",
    manual_delivery_target: true,
  });
  expect(selectedManualDeliveryTarget([manual])).toBeDefined();
  renderPageContext(
    doc,
    { url: "https://provider.example.edu/download/paper.pdf", kind: "pdf", tab_id: 42, tab_url: "https://provider.example.edu/download/paper.pdf" },
    [manual],
  );
  expect(doc.documentElement.innerHTML).not.toContain("This PDF has no DOI to queue");

  const button = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  expect(button.disabled).toBe(false);
  button.click();
  await Promise.resolve();
  await Promise.resolve();

  const txt = doc.getElementById("page-acquire-status")?.textContent ?? "";
  expect(txt).toMatch(/identify/i);
  expect(txt).not.toMatch(/This PDF has no DOI to queue/);
});

test("knownJob PDF context never contains This PDF has no DOI to queue", () => {
  const mod = require("../src/popup") as typeof import("../src/popup");
  // Direct string search in built popup module text
  const fs = require("node:fs") as typeof import("node:fs");
  const popupSrc = fs.readFileSync(new URL("../src/popup.ts", import.meta.url), "utf8");
  expect(popupSrc).not.toContain("This PDF has no DOI to queue");
});

// ---------------------------------------------------------------------------
// The current-page rail: bound, refresh-safe actions (ADR-0023 Decision 1's
// popup responsibility) and ADR-0019 Decision 2's scanner consent.
// ---------------------------------------------------------------------------

/** A binding for a page the rail can act on. `tab_url` is the unrewritten tab
 * address, which is what validation compares. */
function binding(overrides: Partial<PageActionBinding> = {}): PageActionBinding {
  const url = overrides.url ?? "https://scholar.example.edu/refs";
  return {
    url,
    kind: "landing",
    tab_id: 42,
    tab_url: overrides.tab_url ?? url,
    ...overrides,
  } as PageActionBinding;
}

/** A chrome stub whose active tab matches `tab`, so binding validation passes
 * unless a test deliberately moves it. */
function tabChrome(
  tab: { id?: number; url?: string; contentType?: string },
  onMessage: (message: Record<string, unknown>) => unknown = () => ({ ok: true }),
): { sent: Record<string, unknown>[]; injections: unknown[] } {
  const sent: Record<string, unknown>[] = [];
  const injections: unknown[] = [];
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => (tab.id === undefined ? [] : [tab]) },
      scripting: {
        executeScript: async (injection: unknown) => {
          injections.push(injection);
          return [{ result: undefined }];
        },
      },
      storage: { local: { get: async () => ({}) } },
      runtime: {
        sendMessage: async (message: Record<string, unknown>) => {
          sent.push(message);
          return onMessage(message);
        },
      },
    },
  });
  return { sent, injections };
}

test("a DOI landing page offers solid Acquire beside outlined bulk selection", () => {
  const doc = popupDocument();
  renderPageContext(doc, binding({ url: "https://doi.org/10.1000/x", doi: "10.1000/x" }), []);

  const acquire = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  const scan = doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement;
  expect(acquire.hidden).toBe(false);
  expect(acquire.classList.contains("primary")).toBe(true);
  expect(scan.hidden).toBe(false);
  expect(scan.classList.contains("ghost")).toBe(true);
  // ADR-0019's exact visible label.
  expect(scan.textContent).toBe("Select papers on this page");
  expect(doc.getElementById("current-page-actions")?.hidden).toBe(false);
  // With two actions, Enter means the specific one.
  expect(acquire.dataset.primaryAction).toBe("true");
  expect(scan.dataset.primaryAction).toBeUndefined();
});

test("a PDF page offers Send PDF beside bulk selection, preserving the PDF-grab entry", () => {
  const doc = popupDocument();
  renderPageContext(
    doc,
    binding({ url: "https://papers.example.edu/a.pdf", kind: "pdf", tab_url: "https://papers.example.edu/a.pdf" }),
    [],
  );
  const acquire = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  const scan = doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement;
  expect(acquire.hidden).toBe(false);
  expect(acquire.title).toBe("Send this PDF to papio");
  expect(scan.hidden).toBe(false);
  expect(acquire.dataset.primaryAction).toBe("true");
});

test("an ordinary HTTPS page without a DOI shows bulk selection alone — no disabled Acquire placeholder", () => {
  const doc = popupDocument();
  renderPageContext(doc, binding({ url: "https://library.example.edu/list" }), []);

  const acquire = doc.getElementById("page-acquire-btn") as HTMLButtonElement;
  const scan = doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement;
  expect(acquire.hidden).toBe(true);
  expect(scan.hidden).toBe(false);
  expect(doc.getElementById("current-page-actions")?.hidden).toBe(false);
  // Enter falls to the only visible action.
  expect(scan.dataset.primaryAction).toBe("true");
  expect(acquire.dataset.primaryAction).toBeUndefined();
});

test("a restricted or absent page hides both actions and collapses the rail", () => {
  const doc = popupDocument();
  for (const page of [
    undefined,
    binding({ url: "chrome://settings", tab_url: "chrome://settings" }),
    binding({ url: "http://insecure.example.edu/x", tab_url: "http://insecure.example.edu/x" }),
    binding({ url: "not a url", tab_url: "not a url" }),
  ]) {
    renderPageContext(doc, page, []);
    expect((doc.getElementById("page-acquire-btn") as HTMLButtonElement).hidden).toBe(true);
    expect((doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).hidden).toBe(true);
    expect(doc.getElementById("current-page-actions")?.hidden).toBe(true);
  }
});

test("Enter activates whichever rail action is marked primary", async () => {
  const doc = popupDocument();
  let acquired = 0;
  let scanned = 0;
  renderPageAcquire(doc, async () => {
    acquired += 1;
    return { job_id: "job_1" };
  });
  wirePageBulkScanLauncher(doc, async () => {
    scanned += 1;
    return { ok: true };
  });
  wirePrimaryShortcut(doc);
  const press = (): void => {
    doc.dispatchEvent(new doc.defaultView!.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  };

  // Bulk-only page: Enter scans.
  renderPageContext(doc, binding({ url: "https://library.example.edu/list" }), []);
  press();
  await flushMicrotasks();
  expect(scanned).toBe(1);
  expect(acquired).toBe(0);

  // DOI page: Enter acquires, never the bulk action.
  renderPageContext(doc, binding({ url: "https://doi.org/10.1000/x", doi: "10.1000/x" }), []);
  press();
  await flushMicrotasks();
  expect(acquired).toBe(1);
  expect(scanned).toBe(1);

  // Restricted page: Enter does nothing at all.
  renderPageContext(doc, undefined, []);
  press();
  await flushMicrotasks();
  expect(acquired).toBe(1);
  expect(scanned).toBe(1);
});

test("readCurrentPageMetadata refuses to fuse metadata from a page that changed under the probe", async () => {
  let queries = 0;
  Object.assign(globalThis, {
    chrome: {
      tabs: {
        query: async () => {
          queries += 1;
          // The probe round trip is where a navigation can land, so the second
          // read intentionally reports a different address.
          return queries === 1
            ? [{ id: 5, url: "https://a.example.edu/one" }]
            : [{ id: 5, url: "https://a.example.edu/two" }];
        },
      },
      scripting: {
        executeScript: async () => [{ result: { url: "https://a.example.edu/one", doi: "10.1000/one" } }],
      },
    },
  });
  await expect(readCurrentPageMetadata()).rejects.toThrow(PAGE_CHANGED_MESSAGE);
  expect(queries).toBe(2);
});

test("a bound action makes no request once the tab or its URL has changed", async () => {
  const moved = tabChrome({ id: 42, url: "https://elsewhere.example.edu/other" });
  await expect(
    acquireCurrentPage(binding({ url: "https://doi.org/10.1000/x", doi: "10.1000/x", tab_url: "https://doi.org/10.1000/x" })),
  ).rejects.toThrow(PAGE_CHANGED_MESSAGE);
  expect(moved.sent).toEqual([]);

  const otherTab = tabChrome({ id: 99, url: "https://doi.org/10.1000/x" });
  await expect(
    sendCurrentPDF(binding({ url: "https://doi.org/10.1000/x.pdf", kind: "pdf", tab_url: "https://doi.org/10.1000/x.pdf" })),
  ).rejects.toThrow(PAGE_CHANGED_MESSAGE);
  expect(otherTab.sent).toEqual([]);

  const navigated = tabChrome({ id: 42, url: "https://elsewhere.example.edu/other" });
  await expect(
    startPageBulkScan(binding({ url: "https://scholar.example.edu/refs" })),
  ).resolves.toEqual({ ok: false, code: "page_changed", error: PAGE_CHANGED_MESSAGE });
  expect(navigated.sent).toEqual([]);
});

test("a bound scan sends the bound tab and its bare origin as expected_origin", async () => {
  const stub = tabChrome({ id: 42, url: "https://scholar.example.edu/refs" }, () => ({ ok: true, scan_id: "scan-1" }));
  await expect(startPageBulkScan(binding())).resolves.toEqual({ ok: true });
  expect(stub.sent).toEqual([{
    type: PAGE_BULK_SCAN_MESSAGE,
    request: { tab_id: 42, expected_origin: "https://scholar.example.edu" },
  }]);
});

test("a non-HTTPS bound page is refused before any scan request leaves the popup", async () => {
  const stub = tabChrome({ id: 42, url: "chrome://settings" });
  const result = await startPageBulkScan(binding({ url: "chrome://settings", tab_url: "chrome://settings" }));
  expect(result.ok).toBe(false);
  expect(result.code).toBe("invalid_page");
  expect(stub.sent).toEqual([]);
});

test("the first scan of an unapproved site asks once, focuses Allow, and performs no scan", async () => {
  const doc = popupDocument();
  const scans: PageActionBinding[] = [];
  const allowed: string[] = [];
  wirePageBulkScanLauncher(
    doc,
    async (bound) => {
      scans.push(bound);
      return {
        ok: false,
        code: "scanner_consent_required",
        error: "Allow scanning on this site before papio reads the page",
      };
    },
    async (origin) => {
      allowed.push(origin);
      return true;
    },
  );
  renderPageContext(doc, binding(), []);
  (doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).click();
  await flushMicrotasks();

  const prompt = doc.getElementById("page-bulk-consent") as HTMLElement;
  expect(prompt.hidden).toBe(false);
  expect(doc.getElementById("page-bulk-consent-message")?.textContent).toBe(
    "Allow papio to scan pages on scholar.example.edu for paper identifiers? Detection stays in this tab; only papers you select are sent to the papio app.",
  );
  // Focus lands on the affirmative choice, and nothing was stored yet.
  expect(doc.activeElement?.id).toBe("page-bulk-consent-allow");
  expect(allowed).toEqual([]);
  // The refusal is a question, not an error: no error result is left behind.
  expect((doc.getElementById("page-bulk-scan-status") as HTMLElement).hidden).toBe(true);
  expect(scans).toHaveLength(1);
});

test("Allow and scan stores the exact origin and retries only after the write is acknowledged", async () => {
  const doc = popupDocument();
  const order: string[] = [];
  let refuse = true;
  wirePageBulkScanLauncher(
    doc,
    async () => {
      order.push("scan");
      if (refuse) {
        return { ok: false, code: "scanner_consent_required", error: "nope" };
      }
      return { ok: true };
    },
    async (origin) => {
      order.push(`allow:${origin}`);
      refuse = false;
      return true;
    },
  );
  renderPageContext(doc, binding(), []);
  (doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).click();
  await flushMicrotasks();
  (doc.getElementById("page-bulk-consent-allow") as HTMLButtonElement).click();
  await flushMicrotasks();

  expect(order).toEqual(["scan", "allow:https://scholar.example.edu", "scan"]);
  expect((doc.getElementById("page-bulk-consent") as HTMLElement).hidden).toBe(true);
});

test("a failed consent write leaves a persistent error and does not scan", async () => {
  const doc = popupDocument();
  let scans = 0;
  wirePageBulkScanLauncher(
    doc,
    async () => {
      scans += 1;
      return { ok: false, code: "scanner_consent_required", error: "nope" };
    },
    async () => false,
  );
  renderPageContext(doc, binding(), []);
  (doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).click();
  await flushMicrotasks();
  (doc.getElementById("page-bulk-consent-allow") as HTMLButtonElement).click();
  await flushMicrotasks();

  expect(scans).toBe(1);
  const status = doc.getElementById("page-bulk-scan-status") as HTMLElement;
  expect(status.hidden).toBe(false);
  expect(status.textContent).toBe("Could not save scanning permission for this site");
  expect(status.dataset.tone).toBe("error");
  // The prompt stays available so the researcher can retry the write.
  expect((doc.getElementById("page-bulk-consent") as HTMLElement).hidden).toBe(false);
  expect((doc.getElementById("page-bulk-consent-allow") as HTMLButtonElement).disabled).toBe(false);
});

test("Cancel clears the consent prompt, writes nothing, and returns focus to the action", async () => {
  const doc = popupDocument();
  let allows = 0;
  wirePageBulkScanLauncher(
    doc,
    async () => ({ ok: false, code: "scanner_consent_required", error: "nope" }),
    async () => {
      allows += 1;
      return true;
    },
  );
  renderPageContext(doc, binding(), []);
  (doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).click();
  await flushMicrotasks();
  (doc.getElementById("page-bulk-consent-cancel") as HTMLButtonElement).click();
  await flushMicrotasks();

  expect(allows).toBe(0);
  expect((doc.getElementById("page-bulk-consent") as HTMLElement).hidden).toBe(true);
  expect(doc.activeElement?.id).toBe("page-bulk-scan-btn");
});

test("a page change clears a pending consent prompt without writing anything", async () => {
  const doc = popupDocument();
  let allows = 0;
  wirePageBulkScanLauncher(
    doc,
    async () => ({ ok: false, code: "scanner_consent_required", error: "nope" }),
    async () => {
      allows += 1;
      return true;
    },
  );
  renderPageContext(doc, binding(), []);
  (doc.getElementById("page-bulk-scan-btn") as HTMLButtonElement).click();
  await flushMicrotasks();
  expect((doc.getElementById("page-bulk-consent") as HTMLElement).hidden).toBe(false);

  // The researcher navigated; the prompt belonged to the previous origin.
  renderPageContext(doc, binding({ url: "https://other.example.edu/refs" }), []);
  expect((doc.getElementById("page-bulk-consent") as HTMLElement).hidden).toBe(true);
  expect(allows).toBe(0);
});

test("a rail result survives a rerender and is keyed to its own page", async () => {
  const doc = popupDocument();
  renderPageAcquire(doc, async () => ({ job_id: "job_1" }));
  const first = binding({ url: "https://doi.org/10.1000/x", doi: "10.1000/x" });
  renderPageContext(doc, first, []);
  (doc.getElementById("page-acquire-btn") as HTMLButtonElement).click();
  await flushMicrotasks();
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Added to papio");

  // A five-second refresh of the SAME page keeps the result.
  renderPageContext(doc, first, []);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Added to papio");

  // Rendering a DIFFERENT page must not show the first page's outcome.
  renderPageContext(doc, binding({ url: "https://doi.org/10.1000/y", doi: "10.1000/y" }), []);
  expect((doc.getElementById("page-acquire-status") as HTMLElement).hidden).toBe(true);

  // Returning to the first page recovers it: the result is owned by the page.
  renderPageContext(doc, first, []);
  expect(doc.getElementById("page-acquire-status")?.textContent).toBe("Added to papio");
});

test("a stale generation cannot overwrite a newer operation's result", () => {
  const doc = popupDocument();
  const stale = beginPopupOperation(doc, "page:k:doi", "k", "Acquiring…");
  const fresh = beginPopupOperation(doc, "page:k:doi", "k", "Acquiring…");
  expect(fresh).toBeGreaterThan(stale);

  expect(finishPopupOperation(doc, "page:k:doi", stale, { ownerKey: "k", text: "old", tone: "error" })).toBe(false);
  expect(popupOperation(doc, "page:k:doi")?.phase).toBe("pending");
  expect(finishPopupOperation(doc, "page:k:doi", fresh, { ownerKey: "k", text: "new", tone: "success" })).toBe(true);
  expect(popupOperation(doc, "page:k:doi")?.text).toBe("new");

  // A reply whose owner has been replaced underneath it is also refused.
  const next = beginPopupOperation(doc, "page:k:doi", "k2", "Acquiring…");
  expect(finishPopupOperation(doc, "page:k:doi", next, { ownerKey: "k", text: "wrong owner", tone: "success" })).toBe(false);
});

test("operation state is dropped only when its owner disappears", () => {
  const doc = popupDocument();
  beginPopupOperation(doc, "provider:a.example", "a.example", "Allowing…");
  beginPopupOperation(doc, "provider:b.example", "b.example", "Allowing…");
  prunePopupOperations(doc, (owner) => owner === "a.example");
  expect(popupOperation(doc, "provider:a.example")).toBeDefined();
  expect(popupOperation(doc, "provider:b.example")).toBeUndefined();
});

test("the stable announcer speaks a transition once, not on every rerender", () => {
  const doc = popupDocument();
  const announcer = doc.getElementById("popup-operation-status") as HTMLElement;
  announcePopupOperation(doc, "Acquiring…");
  expect(announcer.textContent).toBe("Acquiring…");
  announcer.textContent = "";
  // Identical text is not re-spoken: a rerender is not a transition.
  announcePopupOperation(doc, "Acquiring…");
  expect(announcer.textContent).toBe("");
  announcePopupOperation(doc, "Added to papio");
  expect(announcer.textContent).toBe("Added to papio");
});

test("the pulse companion names simultaneous work the primary line does not", () => {
  const moving = derivePulseDisplay(
    pulseCache({ in_flight: 2, scheduled: 3, stalled: 1, nonterminal_total: 6 }),
    "connected",
    Date.now(),
    15_000,
    { pending_total: 4, turns_required: 4 },
  );
  expect(moving.primary).toBe("Moving");
  expect(moving.companion).toBe("4 decisions waiting · 3 scheduled · 1 stalled");

  // Zero is omitted, and the primary's own class is never repeated.
  const scheduled = derivePulseDisplay(
    pulseCache({ scheduled: 2, nonterminal_total: 2 }),
    "connected",
    Date.now(),
    15_000,
    { pending_total: 0, turns_required: 0 },
  );
  expect(scheduled.primary).toBe("Scheduled");
  expect(scheduled.companion).toBe("");

  // An inexact measurement contributes nothing rather than a guess.
  const noCounts = derivePulseDisplay(
    pulseCache({ in_flight: 1, nonterminal_total: 1 }),
    "connected",
  );
  expect(noCounts.companion).toBe("");
});

test("the popup pulse renders at most three lines and hides while disconnected", () => {
  const doc = popupDocument();
  renderWorkPulse(
    doc,
    pulseCache({
      in_flight: 1,
      nonterminal_total: 1,
      next_action: { kind: "retry", at: new Date().toISOString() },
      effect_capacity: { busy: 1, limit: 2 },
      latest_batch: { membership: "complete", total: 2, settled: 2 },
    } as Partial<WorkPulseResponsePayload>),
    "connected",
    Date.now(),
    { pending_total: 2, turns_required: 2 },
  );
  const visible = [
    doc.getElementById("popup-pulse-primary"),
    doc.getElementById("popup-pulse-next"),
    doc.getElementById("popup-pulse-capacity"),
    doc.getElementById("popup-pulse-batch"),
  ].filter((node): node is HTMLElement => node instanceof HTMLElement && !node.hidden);
  expect(visible.length).toBeLessThanOrEqual(3);
  expect(visible[0]?.textContent).toBe("Moving · 1 paper · 2 decisions waiting");
  // Capacity is constraining, so the third line is capacity, not the batch.
  expect(doc.getElementById("popup-pulse-capacity")?.hidden).toBe(false);
  expect(doc.getElementById("popup-pulse-batch")?.hidden).toBe(true);

  // Disconnected belongs to the daemon band alone.
  renderWorkPulse(doc, undefined, "disconnected");
  expect(doc.getElementById("popup-pulse")?.hidden).toBe(true);
});

test("Needs you keeps its three row classes in order and invents no Downloads row", () => {
  const doc = popupDocument();
  renderNeedsAttention(
    doc,
    [
      job({ job_id: "job-challenge", challenge_blocked: true, challenge_host: "sciencedirect.com", tab_id: 3 }),
      job({ job_id: "job-stalled", tab_id: 4 }),
    ],
    ["a.example.edu", "b.example.edu", "c.example.edu"],
    async () => {},
    async () => {},
    ["job-stalled"],
    async () => {},
    async () => true,
  );
  const rows = Array.from(doc.querySelectorAll("#needs-you-list .needs-you-item"));
  // One challenge, then one auth retry, then provider permissions, capped at 3.
  expect(rows).toHaveLength(3);
  expect(rows[0]?.querySelector(".needs-you-paper")?.textContent).toBe("Security check — sciencedirect.com");
  expect(rows[1]?.querySelector("button")?.textContent).toBe("Retry now");
  expect(rows[2]?.querySelector(".needs-you-paper")?.textContent).toBe("a.example.edu");
  expect(doc.getElementById("needs-you-message")?.textContent).toContain("2 more in inbox");
  // No Downloads projection exists in the store, so no Downloads row may appear.
  expect(doc.getElementById("needs-you-section")?.textContent).not.toMatch(/download/i);
});

test("a blocker's pending state survives a rerender instead of reverting", async () => {
  const doc = popupDocument();
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const render = (): void => {
    renderNeedsAttention(
      doc,
      [],
      ["a.example.edu"],
      async () => {},
      async () => {},
      [],
      async () => {},
      async () => promise,
    );
  };
  render();
  const button = doc.querySelector("#needs-you-list button") as HTMLButtonElement;
  button.click();
  await flushMicrotasks();
  expect(button.textContent).toBe("Allowing…");

  // The five-second repaint rebuilds the row; the in-flight grant must not look
  // idle and clickable again while it is still running.
  render();
  const rebuilt = doc.querySelector("#needs-you-list button") as HTMLButtonElement;
  expect(rebuilt.textContent).toBe("Allowing…");
  expect(rebuilt.disabled).toBe(true);

  resolve(true);
  await flushMicrotasks();
  render();
  const settled = doc.querySelector("#needs-you-list button") as HTMLButtonElement;
  expect(settled.disabled).toBe(false);
  expect(doc.querySelector("#needs-you-list .popup-result")?.textContent).toBe("Allowed");
});

// ---------------------------------------------------------------------------
// ADR-0023's sixth surface: the transient host-page acknowledgement.
// ---------------------------------------------------------------------------

function ackPage(): { window: Window; document: Document } {
  const window = new Window();
  window.document.write("<!doctype html><html><body><p>An article</p></body></html>");
  Object.assign(globalThis, {
    document: window.document,
    window,
    requestAnimationFrame: (cb: () => void) => {
      cb();
      return 0;
    },
    HTMLElement: window.HTMLElement,
  });
  return { window, document: window.document as unknown as Document };
}

test("the host-page acknowledgement mounts one open shadow host with closed copy only", () => {
  const page = ackPage();
  renderInPageAcknowledgement("queued");
  const host = page.document.getElementById("papio-extension-action-ack-v1");
  expect(host).not.toBeNull();
  expect(host?.shadowRoot).not.toBeNull();
  // Announced already by the popup; a second announcement would be a duplicate.
  expect(host?.getAttribute("aria-hidden")).toBe("true");
  expect(host?.shadowRoot?.textContent).toBe("✓Added to papio");
  // Nothing identifying, and nothing interactive.
  expect(host?.shadowRoot?.querySelectorAll("button, a, input")).toHaveLength(0);
  expect((host as HTMLElement).style.pointerEvents).toBe("none");
});

test("each acknowledgement kind maps to exactly its own label and tone", () => {
  const expected: Record<string, { text: string; ink: string }> = {
    queued: { text: "✓Added to papio", ink: "#245e45" },
    already_queued: { text: "•Already in papio", ink: "#12549b" },
    pdf_started: { text: "→papio is handling this PDF", ink: "#426789" },
    pdf_received: { text: "✓PDF received by papio", ink: "#245e45" },
  };
  for (const [kind, want] of Object.entries(expected)) {
    const page = ackPage();
    renderInPageAcknowledgement(kind as InPageAcknowledgementKind);
    const host = page.document.getElementById("papio-extension-action-ack-v1");
    expect(host?.shadowRoot?.textContent).toBe(want.text);
    const chip = host?.shadowRoot?.firstElementChild as HTMLElement;
    expect(chip.style.color).toBe(want.ink);
    // PDF-started must never claim adoption.
    if (kind === "pdf_started") expect(want.text).not.toMatch(/adopt|received|added/i);
  }
});

test("a second acknowledgement replaces the first host rather than stacking", () => {
  const page = ackPage();
  renderInPageAcknowledgement("queued");
  renderInPageAcknowledgement("already_queued");
  expect(page.document.querySelectorAll("#papio-extension-action-ack-v1")).toHaveLength(1);
  expect(
    page.document.getElementById("papio-extension-action-ack-v1")?.shadowRoot?.textContent,
  ).toBe("•Already in papio");
});

test("the acknowledgement installs no observer, listener, or persistence", () => {
  const page = ackPage();
  renderInPageAcknowledgement("queued");
  // Nothing was written to the page beyond the one host element.
  expect(page.document.body.querySelectorAll("*")).toHaveLength(1);
  expect(page.document.documentElement.querySelectorAll("script, link")).toHaveLength(0);
});

test("acknowledgeInPage injects only under the all-requests preference", async () => {
  for (const [mode, expectedInjections] of [["all", 1], ["errors", 0], ["off", 0]] as const) {
    const stub = tabChrome({ id: 42, url: "https://scholar.example.edu/refs" });
    Object.assign(globalThis, {
      chrome: {
        ...(globalThis as unknown as { chrome: Record<string, unknown> }).chrome,
        storage: { local: { get: async () => ({ papio_success_ack_mode_v1: mode }) } },
      },
    });
    await acknowledgeInPage(binding(), "queued");
    expect(stub.injections).toHaveLength(expectedInjections);
  }
});

test("acknowledgeInPage skips a page that changed after the click", async () => {
  const stub = tabChrome({ id: 42, url: "https://elsewhere.example.edu/other" });
  Object.assign(globalThis, {
    chrome: {
      ...(globalThis as unknown as { chrome: Record<string, unknown> }).chrome,
      storage: { local: { get: async () => ({ papio_success_ack_mode_v1: "all" }) } },
    },
  });
  await acknowledgeInPage(binding(), "queued");
  expect(stub.injections).toHaveLength(0);
});

test("acknowledgeInPage swallows a refused injection and asks for nothing more", async () => {
  Object.assign(globalThis, {
    chrome: {
      tabs: { query: async () => [{ id: 42, url: "https://scholar.example.edu/refs" }] },
      storage: { local: { get: async () => ({ papio_success_ack_mode_v1: "all" }) } },
      scripting: {
        executeScript: async () => {
          throw new Error("Cannot access a chrome:// URL");
        },
      },
    },
  });
  await expect(acknowledgeInPage(binding(), "queued")).resolves.toBeUndefined();
});

test("acknowledgeInPage fails closed when the preference cannot be read at all", async () => {
  const stub = tabChrome({ id: 42, url: "https://scholar.example.edu/refs" });
  Object.assign(globalThis, {
    chrome: {
      ...(globalThis as unknown as { chrome: Record<string, unknown> }).chrome,
      storage: undefined,
    },
  });
  await acknowledgeInPage(binding(), "queued");
  expect(stub.injections).toHaveLength(0);
});

test("only a validated success earns an acknowledgement, and never an error or a bulk scan", async () => {
  const kinds: string[] = [];
  const run = async (
    mode: "doi" | "pdf",
    response: Record<string, unknown>,
  ): Promise<void> => {
    const doc = popupDocument();
    renderPageAcquire(
      doc,
      async () => response,
      async () => response,
      async (_bound, kind) => {
        kinds.push(kind);
      },
    );
    renderPageContext(
      doc,
      mode === "pdf"
        ? binding({ url: "https://papers.example.edu/a.pdf", kind: "pdf", tab_url: "https://papers.example.edu/a.pdf" })
        : binding({ url: "https://doi.org/10.1000/x", doi: "10.1000/x" }),
      [],
    );
    (doc.getElementById("page-acquire-btn") as HTMLButtonElement).click();
    await flushMicrotasks();
  };

  await run("doi", { job_id: "job_1" });
  await run("doi", { job_id: "job_1", duplicate: true });
  await run("pdf", { state: "sending" });
  await run("pdf", { state: "downloaded" });
  await run("pdf", { state: "adopted" });
  expect(kinds).toEqual(["queued", "already_queued", "pdf_started", "pdf_started", "pdf_received"]);

  // Errors, empty acknowledgements, and unrelated states earn nothing.
  kinds.length = 0;
  await run("doi", { error: "no DOI found on this page" });
  await run("doi", {});
  await run("pdf", { state: "failed" });
  await run("pdf", { error: { code: "nope", message: "refused" } });
  expect(kinds).toEqual([]);
});

test("a slower earlier refresh cannot paint over a newer one", async () => {
  const slow = Promise.withResolvers<unknown>();
  let countsCalls = 0;
  const countsReply = (turns: number): unknown => ({
    ok: true,
    counts: {
      pending_total: turns,
      turns_required: turns,
      watch_hits: 0,
      actions: turns,
      retractions: 0,
    },
  });
  const h = await popupRefreshHarness((message) => {
    if (message["type"] === "papio.triage.counts") {
      countsCalls += 1;
      // The FIRST refresh's slow input resolves last, which is exactly the
      // reverse-order case the generation fence exists for.
      return countsCalls === 1 ? slow.promise : countsReply(2);
    }
    if (message["type"] === "papio.work.pulse") {
      return {
        ok: true,
        available: true,
        received_at: Date.now(),
        worker_epoch: "worker-1",
        pulse: {
          request_id: "pulse-1",
          schema: 1,
          generated_at: new Date().toISOString(),
          projection_complete: true,
          nonterminal_total: 1,
          in_flight: 0,
          continuing: 0,
          scheduled: 0,
          waiting_required: 1,
          stalled: 0,
        },
      };
    }
    return sessionReplyFixture();
  });
  // A connected daemon: pulse is hidden entirely while disconnected, so the
  // painted line would otherwise be the daemon band's story instead.
  Object.assign(globalThis, {
    chrome: {
      ...(globalThis as unknown as { chrome: Record<string, unknown> }).chrome,
      storage: {
        local: {
          get: async () => ({ papio_state_v1: { activeJobs: [], connectionStatus: "connected" } }),
          set: async () => {},
        },
      },
    },
  });

  const first = h.refresh();
  // Let the older refresh get past its store read and actually issue its slow
  // wave-2 requests, so this exercises reverse-order resolution rather than the
  // trivial case where the older wave is fenced before it starts.
  await flushMicrotasks();
  expect(countsCalls).toBe(1);
  const second = h.refresh();
  await second;
  expect(h.document.getElementById("popup-pulse-primary")?.textContent).toBe(
    "Waiting on you · 2 decisions",
  );

  slow.resolve(countsReply(99));
  await first;
  // The older wave finished last and must have abandoned its writes.
  expect(h.document.getElementById("popup-pulse-primary")?.textContent).toBe(
    "Waiting on you · 2 decisions",
  );
  expect(h.document.getElementById("popup-pulse-primary")?.textContent).not.toContain("99");
});
