// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { describe, expect, test } from "bun:test";
import { Bridge, type BridgeDeps } from "../src/background";
import { emptyStore, type ActiveJob } from "../src/state";
import { ChromeTabsFake, FakeEmitter, FakeWebNavigation } from "./fake-tabs";
import { FakeDownloads } from "./fake-downloads";
import {
  createPageSpikeDeliveryHandler,
  PAGE_SPIKE_DELIVERY,
  pageSpikeBuildConfig,
  parsePageSpikeConfig,
  type PageSpikeDeliveryFence,
} from "../tools/page-spike-delivery";

const nonce = "a13e4262-d662-4ba2-97d9-2b7e01850bbb";
const config = {
  controllerPath: `dist/page-spike-${nonce}/run.html`,
  pdfURL: `http://127.0.0.1:54321/${nonce}/fixture/papio-page-spike-${nonce}.pdf`,
  jobID: "job_0123456789abcdef01234567",
};
const runtimeID = "ehhfplhmddankkocjpldplaokajlbmah";
const sender = { id: runtimeID, url: `chrome-extension://${runtimeID}/${config.controllerPath}` };
const message = { type: PAGE_SPIKE_DELIVERY, tab_id: 7 };
type Start = Bridge["startPDFDelivery"];

function fakeDeps(start?: Start) {
  const requests: Parameters<Start>[0][] = [];
  const deps = {
    runtimeID,
    getSelf: async () => ({ id: runtimeID, installType: "development" }),
    getTab: async (_id: number): Promise<{ url?: string }> => ({ url: config.pdfURL }),
    startPDFDelivery: async (request: Parameters<Start>[0], fence: PageSpikeDeliveryFence) => {
      requests.push(request);
      if (start) return start(request, fence);
      if (!request.choice) return { ok: true as const, state: "needs_choice" as const, choice: {
        interaction: "one-use-token", candidates: [{ job_id: config.jobID, title: "Fixture" }],
      } };
      return { ok: true as const, state: "sending" as const, job_id: config.jobID };
    },
  };
  return { deps, requests };
}

function expectFailure(reply: unknown, code: string) {
  expect(reply).toMatchObject({ ok: false, error: { code } });
}

describe("page fixture build boundary", () => {
  test("default and release builds are disabled, only explicit development config enables it", () => {
    expect(pageSpikeBuildConfig(null, "0.0.0-dev")).toBeNull();
    expect(pageSpikeBuildConfig(null, "0.21.1")).toBeNull();
    expect(pageSpikeBuildConfig(config, "0.21.1-dev.abc123")).toEqual(config);
    for (const version of ["0.21.1", "0.21.1-rc.1", "dev", "0.21.1-devish"]) {
      expect(() => pageSpikeBuildConfig(config, version)).toThrow("development build");
    }
    const parsed = parsePageSpikeConfig(config);
    expect(parsed).not.toBe(config);
    expect(Object.isFrozen(parsed)).toBe(true);
    expect(createPageSpikeDeliveryHandler(null, fakeDeps().deps)).toBeUndefined();
  });

  test("strict canonical config rejects extra authority and alternate URLs", () => {
    const invalid = [null, [], {}, { ...config, jobID: "job_x" }, { ...config, extra: true },
      { ...config, controllerPath: config.controllerPath + "?other" },
      { ...config, controllerPath: config.controllerPath.replace(nonce, crypto.randomUUID()) },
      ...[
        config.pdfURL + "?token=secret", config.pdfURL + "#page=1",
        config.pdfURL.replace("http:", "https:"),
        config.pdfURL.replace("127.0.0.1", "localhost"),
        config.pdfURL.replace("127.0.0.1", "127.1"),
        config.pdfURL.replace("127.0.0.1", "provider.example"),
        config.pdfURL.replace("127.0.0.1", "user@127.0.0.1"),
        config.pdfURL.replace(":54321", ":0"),
        config.pdfURL.replace(":54321", ":65536"),
        config.pdfURL.replace(":54321", ":054321"),
        config.pdfURL.replace("/fixture/", "/fixture/../fixture/"),
        config.pdfURL.replace("/fixture/", "/fixture/%2e%2e/"),
        config.pdfURL.replace(".pdf", ".html"),
      ].map((pdfURL) => ({ ...config, pdfURL })),
    ];
    for (const value of invalid) expect(() => parsePageSpikeConfig(value)).toThrow();
  });
});

describe("page fixture runtime boundary", () => {
  test("exact sender and message shape are required before any browser reads or delivery", async () => {
    const { deps, requests } = fakeDeps();
    let reads = 0;
    deps.getSelf = async () => { reads++; return { id: runtimeID, installType: "development" }; };
    const handle = createPageSpikeDeliveryHandler(config, deps)!;
    for (const invalidSender of [{}, { ...sender, id: "another-extension" },
      { ...sender, url: sender.url + "?extra" }, { ...sender, url: sender.url + "/child" },
      { ...sender, url: `chrome-extension://${runtimeID}/dist/popup.html` }]) {
      expectFailure(await handle(message, invalidSender), "unauthorized");
    }
    for (const invalidMessage of [null, [], { ...message, tab_id: -1 }, { ...message, tab_id: 1.5 },
      { ...message, tab_id: Number.MAX_SAFE_INTEGER + 1 }, { ...message, tab_id: "7" },
      { ...message, url: config.pdfURL }, { ...message, job_id: config.jobID },
      { ...message, type: "papio.delivery.start" }]) {
      expectFailure(await handle(invalidMessage, sender), "invalid_request");
    }
    expect(reads).toBe(0);
    expect(requests).toHaveLength(0);
    expect(await handle(message, sender)).toMatchObject({ ok: true, job_id: config.jobID });
  });

  test("runtime revalidates config, development install, and exact live PDF URL", async () => {
    const invalid = createPageSpikeDeliveryHandler({ ...config, jobID: "wrong" }, fakeDeps().deps)!;
    expectFailure(await invalid(message, sender), "invalid_config");
    for (const installType of ["normal", "admin", "sideload", "other"]) {
      const { deps, requests } = fakeDeps();
      deps.getSelf = async () => ({ id: runtimeID, installType });
      expectFailure(await createPageSpikeDeliveryHandler(config, deps)!(message, sender), "not_development");
      expect(requests).toHaveLength(0);
    }
    for (const url of [undefined, config.pdfURL + "#page=1", config.pdfURL.replace(".pdf", ".html"), "https://provider.example/paper.pdf"]) {
      const { deps, requests } = fakeDeps();
      deps.getTab = async () => url === undefined ? {} : { url };
      expectFailure(await createPageSpikeDeliveryHandler(config, deps)!(message, sender), "fixture_changed");
      expect(requests).toHaveLength(0);
    }
  });

  test("consumes only the allowlisted one-use choice and preserves its reply", async () => {
    const { deps, requests } = fakeDeps();
    const handle = createPageSpikeDeliveryHandler(config, deps)!;
    expect(await handle(message, sender)).toEqual({ ok: true, state: "sending", job_id: config.jobID });
    expect(requests).toEqual([
      { tab_id: 7, url: config.pdfURL },
      { tab_id: 7, url: config.pdfURL, choice: { interaction: "one-use-token", job_id: config.jobID } },
    ]);
    expectFailure(await handle(message, sender), "duplicate_dispatch");
    expect(requests).toHaveLength(2);
  });

  test("missing allowlisted candidate cannot cause a second dispatch", async () => {
    const { deps, requests } = fakeDeps(async () => ({ ok: true, state: "needs_choice", choice: {
      interaction: "other-choice", candidates: [{ job_id: "job_other", title: "Other" }],
    } }));
    expectFailure(await createPageSpikeDeliveryHandler(config, deps)!(message, sender), "job_mismatch");
    expect(requests).toHaveLength(1);
  });

  test("concurrent duplicates and uncertain failures never retry", async () => {
    const { deps, requests } = fakeDeps(async () => { throw new Error("private data"); });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    deps.getSelf = async () => { await gate; return { id: runtimeID, installType: "development" }; };
    const handle = createPageSpikeDeliveryHandler(config, deps)!;
    const first = handle(message, sender);
    expectFailure(await handle(message, sender), "duplicate_dispatch");
    release();
    const reply = await first;
    expectFailure(reply, "fixture_delivery_failed");
    expect(JSON.stringify(reply)).not.toContain("private data");
    expectFailure(await handle(message, sender), "duplicate_dispatch");
    expect(requests).toHaveLength(1);
  });
});

const now = 1_700_000_000_000;
const fixtureJob = (patch: Partial<ActiveJob> = {}): ActiveJob => ({
  job_id: config.jobID, tab_id: 7, offered_at: now, expires_at: now + 600_000,
  status: "awaiting_download", provider_hosts: ["127.0.0.1"], ...patch,
});

/** Ordinary persisted-job hydration through the real Bridge, with no network,
 * timers or private-field access. Job creation/offer remains the live parent's job. */
async function bridgeHarness(jobs: ActiveJob[] = [fixtureJob()]) {
  let store = { ...emptyStore(), activeJobs: jobs };
  const tabs = new ChromeTabsFake(), downloads = new FakeDownloads(), nav = new FakeWebNavigation();
  tabs.seed({ id: 7, url: config.pdfURL, status: "complete" });
  for (const job of jobs) if (job.tab_id >= 0 && job.tab_id !== 7) tabs.seed({ id: job.tab_id, url: "https://example.org/fixture" });
  const deps: BridgeDeps = {
    connectNative: () => ({ postMessage: () => {}, disconnect: () => {}, onMessage: new FakeEmitter<[unknown]>(), onDisconnect: new FakeEmitter<[]>() }),
    manifestVersion: "0.15.0", randomUUID: () => crypto.randomUUID(), now: () => now,
    setTimeout: () => {}, backend: { load: async () => store, save: async (value) => { store = value; } },
    tabs, downloads, webNavigation: nav, adapterSpecs: [], scripting: { executeScript: async () => [] },
    permissions: { contains: async () => false },
    settings: { getTermsConsent: async () => undefined, setTermsConsent: async () => {}, getHandoffSurface: async () => "in-window", getInPageToast: async () => false },
    action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {} },
    alarms: { create: () => {}, onAlarm: new FakeEmitter<[{ name: string }]>() },
  };
  const bridge = new Bridge(deps);
  await bridge.start();
  const handlerDeps = fakeDeps(bridge.startPDFDelivery.bind(bridge)).deps;
  return { bridge, tabs, downloads, nav, handlerDeps, store: () => store,
    handle: createPageSpikeDeliveryHandler(config, handlerDeps)! };
}

describe("fixture delivery uses the real Bridge effect boundary", () => {
  test("bound and unbound PDF tabs both consume a live choice before the existing download path", async () => {
    for (const tab_id of [7, -1]) {
      const h = await bridgeHarness([fixtureJob({ tab_id })]);
      expect(await h.handle(message, sender)).toEqual({ ok: true, state: "sending", job_id: config.jobID });
      expect(h.downloads.started).toEqual([{ url: config.pdfURL, filename: `papio/${config.jobID}/paper.pdf`, conflictAction: "uniquify", saveAs: false }]);
      expect(h.store().activeJobs.map((job) => job.job_id)).toEqual([config.jobID]);
      // A fresh handler cannot repeat a delivery already persisted by Bridge.
      expectFailure(await createPageSpikeDeliveryHandler(config, h.handlerDeps)!(message, sender), "duplicate_dispatch");
      expect(h.downloads.started).toHaveLength(1);
    }
  });

  test("wrong automatic tab or opener binding is rejected before download", async () => {
    for (const wrongTab of [7, 8]) {
      const h = await bridgeHarness([fixtureJob({ tab_id: 9 }), fixtureJob({ job_id: "job_fedcba9876543210fedcba98", tab_id: wrongTab })]);
      if (wrongTab === 8) h.tabs.patch(7, { openerTabId: 8 });
      expectFailure(await h.handle(message, sender), "job_mismatch");
      expect(h.downloads.started).toHaveLength(0);
      expect(h.store().pendingDelivery).toBeUndefined();
    }
  });

  test("absent or ineligible fixture jobs never fall through to PDF grab", async () => {
    for (const jobs of [[], [fixtureJob({ status: "accepted" })]]) {
      const h = await bridgeHarness(jobs);
      expectFailure(await h.handle(message, sender), "job_mismatch");
      expect(h.downloads.started).toHaveLength(0);
      expect(h.store().pendingDelivery).toBeUndefined();
    }
  });

  test("a live URL change after outer preflight is refused inside Bridge", async () => {
    const h = await bridgeHarness();
    h.handlerDeps.getTab = async () => {
      h.tabs.patch(7, { url: "https://provider.example/other.pdf" });
      return { url: config.pdfURL };
    };
    expectFailure(await h.handle(message, sender), "fixture_changed");
    expect(h.downloads.started).toHaveLength(0);
  });

  test("changed document epochs and unavailable identity refuse the existing choice", async () => {
    for (const change of ["missing", "replace", "url"]) {
      const h = await bridgeHarness();
      if (change === "missing") h.nav.clearFrame(7);
      const start = h.handlerDeps.startPDFDelivery;
      h.handlerDeps.startPDFDelivery = async (request, fence) => {
        const reply = await start(request, fence);
        if (!request.choice) {
          if (change === "replace") h.nav.setFrame(7, "new-document");
          if (change === "url") h.tabs.patch(7, { url: config.pdfURL + "#page=1" });
        }
        return reply;
      };
      expectFailure(await h.handle(message, sender), change === "missing" ? "page_unverified" : change === "url" ? "fixture_changed" : "choice_expired");
      expect(h.downloads.started).toHaveLength(0);
    }
  });
});
