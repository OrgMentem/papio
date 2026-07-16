// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Declarative adapter tests: the pure `interpret` classifier (rule precedence,
// every PageKind, the ≥60% wrong-work title-token check, static-only evidence),
// the skip-when-missing fixture harness, and the background verdict mapping
// (permission gate, unknown debounce, single-download latch, hello versions).

import { expect, test } from "bun:test";

import {
  adapters,
  interpret,
  type AdapterContext,
  type AdapterSpec,
  type PageVerdict,
} from "../src/adapters/types";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape, type TermsConsent } from "../src/state";
import {
  Bridge,
  type BridgeDeps,
  type DownloadDeltaLike,
  type DownloadItemLike,
  type NativePort,
  type TabChangeInfo,
  type TabInfo,
} from "../src/background";
import { fixtureExists, loadFixture, parseHTML } from "./harness";
import { Window } from "happy-dom";

// A representative ProQuest-shaped spec. Rules are ordered; first match wins.
const SPEC: AdapterSpec = {
  id: "proquest",
  version: "0.3.1",
  hosts: ["www.proquest.com"],
  classify: [
    { kind: "login", any: ["#login-form", 'input[name="password"]'] },
    { kind: "terms", textAny: ["terms of use", "accept the terms"] },
    { kind: "no_entitlement", textAny: ["not available through your", "no full text available"] },
    { kind: "wrong_work_check", all: ["[data-mismatch]"] },
    { kind: "article", all: ["a.download-pdf"] },
  ],
  download: { selector: "a.download-pdf", requireKind: "article", method: "href" },
};

const EXPECTED_TITLE = "Trust in Automation: Designing for Appropriate Reliance";

function ctx(title?: string): AdapterContext {
  return { expected: title === undefined ? {} : { title } };
}

// --- Contract 1/2: interpret --------------------------------------------------

test("every registered adapter is fixture-backed, versioned, and host-scoped", () => {
  for (const spec of adapters) {
    expect(spec.id).toMatch(/^[a-z][a-z0-9_-]*$/);
    expect(spec.version).toMatch(/^\d+\.\d+\.\d+$/);
    expect(spec.hosts.length).toBeGreaterThan(0);
    expect(spec.classify.length).toBeGreaterThan(0);
    // The plan forbids specs without captured evidence: at least the success
    // fixture must be committed for every registered provider.
    expect(fixtureExists(spec.id, "success")).toBe(true);
    if (spec.download) expect(spec.download.requireKind).toBe("article");
  }
  expect(adapters.map((a) => a.id)).toContain("proquest");
});

test("interpret waits for late-upgraded custom elements when settleTimeoutMs is set", async () => {
  // JSTOR's tracked tab fires `complete` post-SSO before its `mfe-*` custom
  // elements upgrade. The live (doc === null) path must observe the DOM until
  // the download button appears, not classify once and give up.
  const jstor = adapters.find((a) => a.id === "jstor");
  expect(jstor?.settleTimeoutMs).toBeGreaterThan(0);

  const win = new Window({ url: "https://www.jstor.org/stable/259290" });
  win.document.write("<html><body><main>Loading full text</main></body></html>");
  const prev = {
    document: globalThis.document,
    MutationObserver: globalThis.MutationObserver,
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
  };
  Object.assign(globalThis, {
    document: win.document,
    MutationObserver: win.MutationObserver,
    setTimeout: win.setTimeout.bind(win),
    clearTimeout: win.clearTimeout.bind(win),
  });
  try {
    const verdict = interpret(null, jstor as AdapterSpec, ctx());
    win.document.body.insertAdjacentHTML(
      "beforeend",
      "<mfe-download-pharos-button data-qa=\"download-pdf\" data-doi=\"10.2307/259290\"></mfe-download-pharos-button>",
    );
    expect((await verdict).kind).toBe("article");
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("a rule with no conditions never matches (no blanket fallback)", () => {
  const spec: AdapterSpec = {
    id: "x",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "article" }], // empty conditions
  };
  const doc = parseHTML("<html><body><a class='download-pdf'>x</a></body></html>");
  expect(interpret(doc, spec, ctx()).kind).toBe("unknown");
});

test("first matching rule wins: login precedes article on an ambiguous page", () => {
  const doc = parseHTML(
    `<html><body><form id="login-form"><input name="password"></form>` +
      `<a class="download-pdf">PDF</a></body></html>`,
  );
  const v = interpret(doc, SPEC, ctx(EXPECTED_TITLE));
  expect(v.kind).toBe("login");
  expect(v.evidence).toEqual(["rule:login matched"]);
});

test("each PageKind is reachable from its own fixture", () => {
  const cases: { html: string; kind: PageVerdict["kind"] }[] = [
    { html: `<form id="login-form"></form>`, kind: "login" },
    { html: `<p>You must accept the terms of use to continue.</p>`, kind: "terms" },
    { html: `<p>No full text available for this item.</p>`, kind: "no_entitlement" },
    { html: `<div data-mismatch="1">different work</div>`, kind: "wrong_work_check" },
    { html: `<h1>${EXPECTED_TITLE}</h1><a class="download-pdf">PDF</a>`, kind: "article" },
    { html: `<p>an unrecognised page state</p>`, kind: "unknown" },
  ];
  for (const c of cases) {
    const doc = parseHTML(`<html><body>${c.html}</body></html>`);
    expect(interpret(doc, SPEC, ctx(EXPECTED_TITLE)).kind).toBe(c.kind);
  }
});

test("any[] matches on at least one selector; all[] needs every selector", () => {
  const anySpec: AdapterSpec = {
    id: "a",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "login", any: ["#a", "#b"] }],
  };
  expect(interpret(parseHTML(`<html><body><i id="b"></i></body></html>`), anySpec, ctx()).kind).toBe("login");
  expect(interpret(parseHTML(`<html><body><i id="c"></i></body></html>`), anySpec, ctx()).kind).toBe("unknown");

  const allSpec: AdapterSpec = {
    id: "a",
    version: "0",
    hosts: ["h"],
    classify: [{ kind: "login", all: ["#a", "#b"] }],
  };
  expect(interpret(parseHTML(`<html><body><i id="a"></i></body></html>`), allSpec, ctx()).kind).toBe("unknown");
  expect(
    interpret(parseHTML(`<html><body><i id="a"></i><i id="b"></i></body></html>`), allSpec, ctx()).kind,
  ).toBe("login");
});

test("wrong-work: matching title tokens keep article; mismatch downgrades to wrong_work", () => {
  // Title signal present in h1 -> ≥60% tokens -> article.
  const good = parseHTML(
    `<html><head><title>Trust in Automation: Designing for Appropriate Reliance</title></head>` +
      `<body><h1>Trust in Automation</h1><a class="download-pdf">PDF</a></body></html>`,
  );
  const gv = interpret(good, SPEC, ctx(EXPECTED_TITLE));
  expect(gv.kind).toBe("article");
  expect(gv.evidence).toContain("title-token-check passed");

  // A completely different work on an otherwise article-shaped page.
  const bad = parseHTML(
    `<html><head><title>Groupthink and the Bay of Pigs</title></head>` +
      `<body><h1>Collective Rationalization in Small Groups</h1>` +
      `<a class="download-pdf">PDF</a></body></html>`,
  );
  const bv = interpret(bad, SPEC, ctx(EXPECTED_TITLE));
  expect(bv.kind).toBe("wrong_work");
  expect(bv.evidence).toContain("title-token-check failed");
});

test("wrong-work check uses citation_title meta as a title source", () => {
  const doc = parseHTML(
    `<html><head><meta name="citation_title" content="Trust in Automation: Designing for Appropriate Reliance"></head>` +
      `<body><h1>Untitled viewer</h1><a class="download-pdf">PDF</a></body></html>`,
  );
  expect(interpret(doc, SPEC, ctx(EXPECTED_TITLE)).kind).toBe("article");
});

test("no expected title present: article is accepted without a token check", () => {
  const doc = parseHTML(`<html><body><h1>Anything</h1><a class="download-pdf">PDF</a></body></html>`);
  const v = interpret(doc, SPEC, ctx());
  expect(v.kind).toBe("article");
  expect(v.evidence).toEqual(["rule:article matched"]);
});

test("evidence carries only static rule labels — never page text", () => {
  const secret = "SECRETXYZ_page_body_marker_do_not_leak";
  const doc = parseHTML(
    `<html><head><title>${secret}</title></head><body><h1>${secret}</h1>` +
      `<p>${secret} more prose ${secret}</p><a class="download-pdf">${secret}</a></body></html>`,
  );
  const v = interpret(doc, SPEC, ctx("something entirely different"));
  const allowed = /^(rule:[a-z_]+ matched|title-token-check (passed|failed)|no rule matched)$/;
  for (const e of v.evidence) expect(e).toMatch(allowed);
  expect(JSON.stringify(v.evidence).includes(secret)).toBe(false);
  expect(JSON.stringify(v.evidence).toLowerCase().includes("secretxyz")).toBe(false);
});

// --- Contract 2: fixture harness skip-when-missing ----------------------------

test("harness reports a missing fixture as absent and loads it as null", () => {
  expect(fixtureExists("proquest", "__does_not_exist__")).toBe(false);
  expect(loadFixture("proquest", "__does_not_exist__")).toBeNull();
});

// Real capture lands later; this must SKIP (not fail) while absent.
const liveArticle = loadFixture("proquest", "article");
test.skipIf(liveArticle === null)("captured proquest article fixture classifies as article", () => {
  expect(interpret(liveArticle as Document, SPEC, ctx(EXPECTED_TITLE)).kind).toBe("article");
});

// --- Contract 4: background verdict mapping -----------------------------------

class FakeEmitter<A extends unknown[]> {
  private readonly cbs: ((...a: A) => unknown)[] = [];
  addListener(cb: (...a: A) => void): void {
    this.cbs.push(cb);
  }
  async emit(...a: A): Promise<void> {
    await Promise.all(this.cbs.map((cb) => cb(...a)));
  }
}

class FakePort implements NativePort {
  readonly posted: object[] = [];
  readonly onMessage = new FakeEmitter<[unknown]>();
  readonly onDisconnect = new FakeEmitter<[]>();
  postMessage(msg: object): void {
    this.posted.push(msg);
  }
  disconnect(): void {
    void this.onDisconnect.emit();
  }
  async inbound(msg: unknown): Promise<void> {
    await this.onMessage.emit(msg);
  }
}

class FakeBackend implements StateBackend {
  store: StoreShape = emptyStore();
  async load(): Promise<StoreShape> {
    return this.store;
  }
  async save(store: StoreShape): Promise<void> {
    this.store = store;
  }
}

class FakeTabs {
  readonly onUpdated = new FakeEmitter<[number, TabChangeInfo, TabInfo]>();
  readonly onRemoved = new FakeEmitter<[number, { isWindowClosing: boolean }]>();
  readonly live = new Map<number, TabInfo>();
  nextId = 200;
  async create(props: { url: string; active: boolean }): Promise<TabInfo> {
    const id = this.nextId++;
    const tab: TabInfo = { id, url: props.url };
    this.live.set(id, tab);
    return tab;
  }
  async get(tabID: number): Promise<TabInfo> {
    const tab = this.live.get(tabID);
    if (!tab) throw new Error("no such tab");
    return tab;
  }
  async reload(_tabID: number): Promise<void> {}
  async remove(tabID: number): Promise<void> {
    this.live.delete(tabID);
  }
}

class FakeDownloads {
  readonly onCreated = new FakeEmitter<[DownloadItemLike]>();
  readonly onChanged = new FakeEmitter<[DownloadDeltaLike]>();
  readonly onDeterminingFilename = new FakeEmitter<
    [DownloadItemLike, (s: { filename: string; conflictAction: "uniquify" }) => void]
  >();
  readonly items = new Map<number, DownloadItemLike>();
  determineBeforeReturn = false;
  readonly started: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }[] = [];
  async download(options: {
    url: string;
    filename: string;
    conflictAction: "uniquify";
    saveAs: false;
  }): Promise<number> {
    this.started.push(options);
    const id = 700 + this.started.length;
    this.items.set(id, {
      id,
      url: options.url.replace("TOKEN=ephemeral", "TOKEN=normalized"),
      finalUrl: "https://media.proquest.com/redirected/out.pdf",
      filename: "/Users/test/Downloads/out.pdf",
      fileSize: 12345,
      state: "in_progress",
    });
    if (this.determineBeforeReturn) await this.determine(id);
    return id;
  }
  async removeFile(_downloadID: number): Promise<void> {}
  async erase(query: { id: number }): Promise<number[]> {
    return [query.id];
  }
  async determine(id: number): Promise<void> {
    const item = this.items.get(id);
    if (!item) throw new Error(`unknown fake download ${id}`);
    let relative = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
    await this.onDeterminingFilename.emit(item, (s) => {
      relative = s.filename;
    });
    this.items.set(id, { ...item, filename: `/Users/test/Downloads/${relative}` });
  }
  async search(query: { id: number }): Promise<DownloadItemLike[]> {
    const item = this.items.get(query.id);
    return item ? [item] : [];
  }
}

// Fake chrome.scripting: interpret injections (3 args) return queued verdicts;
// extractDownloadURL (1 arg) returns a signed URL; declared clicks (5 args)
// record the exact light/shadow/follow-up selectors.
class FakeScripting {
  verdict: PageVerdict | undefined;
  readonly verdictQueue: PageVerdict[] = [];
  href = "https://media.proquest.com/media/signed?TOKEN=ephemeral";
  readonly extracted: { tabId: number; selector: string }[] = [];
  readonly clicked: {
    tabId: number;
    selector: string;
    shadowSelector?: string;
    followupSelector?: string;
  }[] = [];
  readonly rawClickArgs: unknown[][] = [];
  readonly termsAccepts: { tabId: number; modalSelector: string; textAny: unknown }[] = [];
  readonly interpretTabs: number[] = [];
  async executeScript(inj: {
    target: { tabId: number };
    func: (...args: never[]) => unknown;
    args?: unknown[];
  }): Promise<{ result?: unknown }[]> {
    const args = inj.args ?? [];
    if (args.length === 1) {
      this.extracted.push({ tabId: inj.target.tabId, selector: String(args[0]) });
      return [{ result: this.href }];
    }
    if (args.length === 2) {
      this.termsAccepts.push({ tabId: inj.target.tabId, modalSelector: String(args[0]), textAny: args[1] });
      return [{ result: true }];
    }
    if (args.length === 5) {
      this.rawClickArgs.push([...args]);
      this.clicked.push({
        tabId: inj.target.tabId,
        selector: String(args[0]),
        ...(typeof args[1] === "string" ? { shadowSelector: args[1] } : {}),
        ...(typeof args[4] === "string" ? { followupSelector: args[4] } : {}),
      });
      return [{ result: true }];
    }
    this.interpretTabs.push(inj.target.tabId);
    return [{ result: this.verdictQueue.shift() ?? this.verdict }];
  }
}

class FakePermissions {
  granted = true;
  readonly checks: string[][] = [];
  async contains(perm: { origins: string[] }): Promise<boolean> {
    this.checks.push(perm.origins);
    return this.granted;
  }
}

const PROVIDER = "www.proquest.com";
const OPENURL = "https://resolver.example.edu/openurl?ctx=abc";
const EXPIRES = "2027-01-01T00:00:00Z";

interface MapHarness {
  bridge: Bridge;
  port: FakePort;
  backend: FakeBackend;
  tabs: FakeTabs;
  scripting: FakeScripting;
  downloads: FakeDownloads;
  permissions: FakePermissions;
  clock: { now: number };
  timers: { fn: () => void | Promise<void>; ms: number }[];
  settings: { consent: TermsConsent };
  frames(): BrowserMessage[];
}

function makeMapHarness(specs: AdapterSpec[] = [SPEC]): MapHarness {
  const port = new FakePort();
  const backend = new FakeBackend();
  const tabs = new FakeTabs();
  const scripting = new FakeScripting();
  const permissions = new FakePermissions();
  const downloads = new FakeDownloads();
  const clock = { now: 1_700_000_000_000 };
  const timers: { fn: () => void | Promise<void>; ms: number }[] = [];
  const settings = { consent: undefined as TermsConsent };
  const deps: BridgeDeps = {
    connectNative: () => port,
    manifestVersion: "0.1.0",
    randomUUID: () => crypto.randomUUID(),
    now: () => clock.now,
    setTimeout: (fn, ms) => {
      timers.push({ fn, ms });
    },
    backend,
    tabs,
    downloads,
    adapterSpecs: specs,
    scripting,
    permissions,
    settings: {
      getTermsConsent: async () => settings.consent,
      setTermsConsent: async (v) => {
        settings.consent = v;
      },
    },
  };
  return {
    bridge: new Bridge(deps),
    port,
    backend,
    tabs,
    scripting,
    downloads,
    permissions,
    clock,
    timers,
    settings,
    frames: () => port.posted.map(parseBrowserMessage),
  };
}

function offer(
  jobID: string,
  expected?: { title?: string },
  providerHosts: string[] = [PROVIDER],
): unknown {
  return {
    protocol: "papio-browser/1",
    type: "job_offer",
    msg_id: "offer_00000001",
    job_id: jobID,
    seq: 0,
    payload: {
      openurl: OPENURL,
      provider_hosts: providerHosts,
      access_mode: "maximal",
      expires_at: EXPIRES,
      ...(expected !== undefined ? { expected } : {}),
    },
  };
}

async function landOnProvider(
  h: MapHarness,
  jobID: string,
  host: string = PROVIDER,
): Promise<number> {
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === jobID)?.tab_id ?? -1;
  const url = `https://${host}/pqdweb`;
  h.tabs.live.set(tabID, { id: tabID, url });
  await h.tabs.onUpdated.emit(tabID, { url, status: "complete" }, { id: tabID, url });
  return tabID;
}

test("auth return classifies the provider landing even without a complete event", async () => {
  // JSTOR-class providers end SSO with a soft-nav landing that carries no
  // `status: "complete"`, so the complete-gated classify never fires. The
  // auth-return transition must classify the page itself.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_authreturn_0001"));
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreturn_0001")?.tab_id ?? -1;
  expect(tabID).toBeGreaterThanOrEqual(0);

  const idpURL = "https://idp.example.edu/sso?SAMLRequest=x";
  h.tabs.live.set(tabID, { id: tabID, url: idpURL });
  await h.tabs.onUpdated.emit(tabID, { url: idpURL }, { id: tabID, url: idpURL });
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");

  const provURL = `https://${PROVIDER}/pqdweb?doc=1`;
  h.tabs.live.set(tabID, { id: tabID, url: provURL });
  // Note: no `status: "complete"` — this is the post-SSO soft landing.
  await h.tabs.onUpdated.emit(tabID, { url: provURL }, { id: tabID, url: provURL });

  expect(h.frames().some((f) => f.type === "auth_returned")).toBe(true);
  expect(h.scripting.interpretTabs).toContain(tabID);
  expect(h.downloads.started.length).toBe(1);
});

test("a transiently unknown provider page is reclassified until it renders", async () => {
  // The first classify sees an un-upgraded page (unknown); a bounded retry must
  // re-run the classifier so the eventually-rendered article still downloads.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdictQueue.push(
    { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] },
    { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] },
  );
  await h.bridge.start();
  await h.port.inbound(offer("job_retry_0001"));
  const tabID = await landOnProvider(h, "job_retry_0001");

  // Unknown first: no download yet, but a retry is scheduled.
  expect(h.downloads.started.length).toBe(0);
  expect(h.timers.length).toBeGreaterThan(0);

  // Drain the retry; the page now classifies as an article and downloads once.
  for (const t of h.timers.splice(0)) await t.fn();
  expect(h.scripting.interpretTabs.length).toBeGreaterThanOrEqual(2);
  expect(h.downloads.started.length).toBe(1);
  expect(h.tabs.live.get(tabID)?.url).toContain(PROVIDER);
});

test("a provider PDF opened in a new viewer tab is adopted for the opener job", async () => {
  // JSTOR-class providers "download" by opening the PDF in a new tab. That tab
  // is untracked; it must be adopted for the handoff tab that spawned it.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_viewer_0001"));
  const trackedTab = h.backend.store.activeJobs.find(j => j.job_id === 'job_viewer_0001')?.tab_id ?? -1;
  expect(trackedTab).toBeGreaterThanOrEqual(0);

  // A new viewer tab opens on the provider .pdf, spawned by the tracked tab.
  const viewerTab = 999;
  const pdfUrl = `https://${PROVIDER}/doc/259290.pdf?refreqid=x`;
  h.tabs.live.set(viewerTab, { id: viewerTab, url: pdfUrl, openerTabId: trackedTab });
  await h.tabs.onUpdated.emit(viewerTab, { status: 'complete', url: pdfUrl }, { id: viewerTab, url: pdfUrl, openerTabId: trackedTab });

  // The PDF is downloaded for the opener job and the viewer tab is closed.
  expect(h.downloads.started.map(d => d.url)).toContain(pdfUrl);
  expect(h.downloads.started.some(d => d.filename.includes('job_viewer_0001'))).toBe(true);
  expect(h.tabs.live.has(viewerTab)).toBe(false);
});

test("a stray non-opener PDF tab is not adopted", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_viewer_0002"));
  const strayTab = 998;
  const pdfUrl = `https://${PROVIDER}/doc/other.pdf`;
  // openerTabId points at an unrelated tab; no download_initiated job matches.
  h.tabs.live.set(strayTab, { id: strayTab, url: pdfUrl, openerTabId: 12345 });
  await h.tabs.onUpdated.emit(strayTab, { status: 'complete', url: pdfUrl }, { id: strayTab, url: pdfUrl, openerTabId: 12345 });
  expect(h.downloads.started.length).toBe(0);
  expect(h.tabs.live.has(strayTab)).toBe(true);
});

const TERMS_SPEC: AdapterSpec = {
  id: "termsprov",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [],
  termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
};
const termsVerdict = { kind: "terms" as const, adapter_id: "termsprov", adapter_version: "1.0.0", evidence: [] };

test("terms verdict auto-accepts only when the user has consented", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.settings.consent = "accept";
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0001"));
  await landOnProvider(h, "job_terms_0001");

  expect(h.scripting.termsAccepts.length).toBe(1);
  expect(h.scripting.termsAccepts[0]?.modalSelector).toBe("div.terms[open]");
  // Auto-accept emits no provider_outcome; the ensuing download is the record.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("terms verdict stays a human step and flags consent when undecided", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict; // consent stays undefined
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0002"));
  await landOnProvider(h, "job_terms_0002");

  expect(h.scripting.termsAccepts.length).toBe(0);
  expect(h.frames().some((f) => f.type === "provider_outcome" && f.payload.outcome === "terms_acceptance_required")).toBe(true);
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_terms_0002")?.needs_terms_consent).toBe(true);
});

test("granting consent clears the prompt flag and re-attempts the pending terms gate", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0003"));
  await landOnProvider(h, "job_terms_0003");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);

  await h.bridge.requestTermsConsent("accept");
  expect(h.settings.consent).toBe("accept");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(false);
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
});

test("declining consent records manual and never auto-accepts", async () => {
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict;
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_0004"));
  await landOnProvider(h, "job_terms_0004");
  await h.bridge.requestTermsConsent("manual");
  expect(h.settings.consent).toBe("manual");
  expect(h.scripting.termsAccepts.length).toBe(0);
});

test("startup reconciliation re-queues a job whose pre-download tab vanished", async () => {
  // A tab closed while the worker slept never fired onTabRemoved, so the job
  // still points at a dead tab. Reconcile must recover it, not strand it.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0001"));
  const tabID = await landOnProvider(h, "job_recon_0001");
  expect(tabID).toBeGreaterThanOrEqual(0);

  h.tabs.live.delete(tabID); // vanished while worker asleep
  await h.bridge.start(); // worker wakes

  // Recovered: no longer pointed at the dead tab. Auth evidence from the prior
  // landing lets the same start() reopen it immediately; otherwise it is queued
  // and the forced-release timer reopens it. Either way it lands on a live tab.
  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0001");
  expect(job).toBeDefined();
  expect(job?.tab_id).not.toBe(tabID);
  for (const t of h.timers.splice(0)) await t.fn();
  const reopened = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0001");
  expect(reopened?.tab_id).toBeGreaterThanOrEqual(0);
  expect(h.tabs.live.has(reopened?.tab_id ?? -1)).toBe(true);
});

test("startup reconciliation parks a past-auth job whose tab vanished", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0002"));
  const tabID = await landOnProvider(h, "job_recon_0002");
  // Drive through auth to awaiting_download.
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";
  h.tabs.live.set(tabID, { id: tabID, url: idp });
  await h.tabs.onUpdated.emit(tabID, { url: idp }, { id: tabID, url: idp });
  const prov = `https://${PROVIDER}/doc?x=1`;
  h.tabs.live.set(tabID, { id: tabID, url: prov });
  await h.tabs.onUpdated.emit(tabID, { url: prov }, { id: tabID, url: prov });
  expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0002")?.status).toBe("awaiting_download");

  h.tabs.live.delete(tabID);
  await h.bridge.start();
  // Parked: download may have landed in the adoption dir for the daemon to scan.
  expect(h.backend.store.activeJobs.some((j) => j.job_id === "job_recon_0002")).toBe(false);
});

test("startup reconciliation leaves a job with a live tab untouched", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  await h.port.inbound(offer("job_recon_0003"));
  const tabID = await landOnProvider(h, "job_recon_0003");
  await h.bridge.start();
  const job = h.backend.store.activeJobs.find((j) => j.job_id === "job_recon_0003");
  expect(job?.tab_id).toBe(tabID);
  expect(job?.status).toBe("accepted");
});

test("hello reports adapter_versions from the registered specs", async () => {
  const jstor: AdapterSpec = { id: "jstor", version: "1.2.0", hosts: ["www.jstor.org"], classify: [] };
  const h = makeMapHarness([SPEC, jstor]);
  await h.bridge.start();
  const hello = h.frames().find((f) => f.type === "hello");
  expect(hello?.payload["adapter_versions"]).toEqual({ proquest: "0.3.1", jstor: "1.2.0" });
});

test("empty registry reports an empty adapter_versions map", async () => {
  const h = makeMapHarness([]);
  await h.bridge.start();
  const hello = h.frames().find((f) => f.type === "hello");
  expect(hello?.payload["adapter_versions"]).toEqual({});
});

test("article verdict starts one browser-managed job-scoped download, no signed URL frame", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_article_0001", { title: EXPECTED_TITLE }));
  const tabID = await landOnProvider(h, "job_article_0001");

  expect(h.scripting.extracted).toEqual([{ tabId: tabID, selector: "a.download-pdf" }]);
  expect(h.downloads.started).toEqual([
    {
      url: h.scripting.href,
      filename: "papio/job_article_0001/paper.pdf",
      conflictAction: "uniquify",
      saveAs: false,
    },
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  // Signed URL remains extension-memory-only: no frame contains it.
  expect(h.frames().some((f) => JSON.stringify(f).includes("TOKEN=ephemeral"))).toBe(false);
  expect(h.frames().some((f) => f.type === "download_started")).toBe(false);
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);

  // A re-classification (another page load) must NOT initiate a second download.
  await landOnProvider(h, "job_article_0001");
  expect(h.downloads.started.length).toBe(1);
  // Live Chrome returned the download ID before asking for a filename.
  await h.downloads.determine(701);
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_article_0001/out.pdf",
  );
  // Completion is correlated by chrome.downloads.download's returned ID even
  // if onCreated raced before the Promise resolved.
  await h.downloads.onChanged.emit({ id: 701, state: { current: "complete" } });
  const complete = h.frames().find((f) => f.type === "download_complete");
  expect(complete?.job_id).toBe("job_article_0001");
  expect(complete?.payload["filename"]).toBe("out.pdf");
  expect(complete?.payload["size_bytes"]).toBe(12345);
});

test("declared shadow click reclassifies an in-page terms gate", async () => {
  const clickSpec: AdapterSpec = {
    id: "jstor",
    version: "0.1.0",
    hosts: [PROVIDER],
    classify: [{ kind: "article", all: ["mfe-download"] }],
    download: {
      selector: "mfe-download",
      requireKind: "article",
      method: "click",
      shadowSelector: "#button-element",
      postClickWaitFor: ".terms[open]",
      postClickTimeoutMs: 3000,
    },
  };
  const h = makeMapHarness([clickSpec]);
  h.scripting.verdictQueue.push(
    { kind: "article", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
    { kind: "terms", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
  );
  await h.bridge.start();
  await h.port.inbound(offer("job_jstor_terms_0001"));
  const tabID = await landOnProvider(h, "job_jstor_terms_0001");

  expect(h.scripting.clicked).toEqual([
    { tabId: tabID, selector: "mfe-download", shadowSelector: "#button-element" },
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);

  const outcome = h.frames().find((f) => f.type === "provider_outcome");
  expect(outcome?.payload["outcome"]).toBe("terms_acceptance_required");
  expect(outcome?.payload["adapter_version"]).toBe("0.1.0");
  expect(h.scripting.interpretTabs.length).toBe(2);
  expect(h.downloads.started).toHaveLength(0);
});
test("declared provider modal follow-up stays inside the one click helper", async () => {
  const clickSpec: AdapterSpec = {
    id: "ebsco",
    version: "0.1.0",
    hosts: [PROVIDER],
    classify: [{ kind: "article", all: ["meta[name='citation_title']"] }],
    download: {
      selector: "[data-auto='download']",
      requireKind: "article",
      method: "click",
      followupSelector: "[data-auto='confirm-download']",
      postClickTimeoutMs: 3000,
    },
  };
  const h = makeMapHarness([clickSpec]);
  h.scripting.verdict = {
    kind: "article",
    adapter_id: "ebsco",
    adapter_version: "0.1.0",
    evidence: [],
  };
  await h.bridge.start();
  await h.port.inbound(offer("job_ebsco_click_0001"));
  const tabID = await landOnProvider(h, "job_ebsco_click_0001");

  expect(h.scripting.clicked).toEqual([
    {
      tabId: tabID,
      selector: "[data-auto='download']",
      followupSelector: "[data-auto='confirm-download']",
    },
  ]);
  expect(h.scripting.rawClickArgs).toEqual([
    [
      "[data-auto='download']",
      null,
      null,
      3000,
      "[data-auto='confirm-download']",
    ],
  ]);
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(h.downloads.started).toHaveLength(0);
});
test("click downloads correlate by adapter when concurrent handoffs share provider hosts", async () => {
  const jstor: AdapterSpec = {
    id: "jstor",
    version: "0.1.0",
    hosts: ["www.jstor.org"],
    classify: [{ kind: "article", all: [".download"] }],
    download: { selector: ".download", requireKind: "article", method: "click" },
  };
  const ebsco: AdapterSpec = {
    id: "ebsco",
    version: "0.1.0",
    hosts: ["research.ebsco.com"],
    classify: [{ kind: "article", all: [".download"] }],
    download: { selector: ".download", requireKind: "article", method: "click" },
  };
  const providerHosts = ["www.jstor.org", "research.ebsco.com"];
  const h = makeMapHarness([jstor, ebsco]);
  h.scripting.verdictQueue.push(
    { kind: "article", adapter_id: "jstor", adapter_version: "0.1.0", evidence: [] },
    { kind: "article", adapter_id: "ebsco", adapter_version: "0.1.0", evidence: [] },
  );
  await h.bridge.start();
  await h.bridge.setKeepaliveAuthenticated(true);
  await h.port.inbound(offer("job_jstor_concurrent_0001", undefined, providerHosts));
  await landOnProvider(h, "job_jstor_concurrent_0001", "www.jstor.org");
  await h.port.inbound(offer("job_ebsco_concurrent_0001", undefined, providerHosts));
  await landOnProvider(h, "job_ebsco_concurrent_0001", "research.ebsco.com");

  const item: DownloadItemLike = {
    id: 901,
    url: "blob:https://research.ebsco.com/download",
    referrer: "https://research.ebsco.com/c/record",
    filename: "/Users/test/Downloads/EBSCO-FullText.pdf",
    state: "in_progress",
  };
  let suggested: { filename: string; conflictAction: "uniquify" } | undefined;
  await h.downloads.onDeterminingFilename.emit(item, (value) => {
    suggested = value;
  });

  expect(suggested).toEqual({
    filename: "papio/job_ebsco_concurrent_0001/EBSCO-FullText.pdf",
    conflictAction: "uniquify",
  });
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_jstor_concurrent_0001")?.adapter_id).toBe(
    "jstor",
  );
  expect(h.backend.store.activeJobs.find((job) => job.job_id === "job_ebsco_concurrent_0001")?.adapter_id).toBe(
    "ebsco",
  );
});



test("filename steering also handles determination before the download ID returns", async () => {
  const h = makeMapHarness();
  h.downloads.determineBeforeReturn = true;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_early_name_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_early_name_0001");
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_early_name_0001/out.pdf",
  );
});

test("classification is gated on an optional-host-permission grant", async () => {
  const h = makeMapHarness();
  h.permissions.granted = false;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_nogrant_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_nogrant_0001");
  expect(h.scripting.interpretTabs.length).toBe(0);
  expect(h.downloads.started.length).toBe(0);
  expect(h.permissions.checks).toContainEqual([`https://${PROVIDER}/*`]);
});

test("no registered adapter for the host stays assisted (no injection)", async () => {
  const h = makeMapHarness([]);
  h.scripting.verdict = { kind: "article", adapter_id: "x", adapter_version: "0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_noadapter_0001"));
  await landOnProvider(h, "job_noadapter_0001");
  expect(h.scripting.interpretTabs.length).toBe(0);
});

test("terms/no_entitlement/wrong_work map to their provider outcomes", async () => {
  const cases: { kind: PageVerdict["kind"]; outcome: string }[] = [
    { kind: "terms", outcome: "terms_acceptance_required" },
    { kind: "no_entitlement", outcome: "no_entitlement" },
    { kind: "wrong_work", outcome: "wrong_work" },
    { kind: "wrong_work_check", outcome: "wrong_work" },
  ];
  for (const c of cases) {
    const h = makeMapHarness();
    h.scripting.verdict = { kind: c.kind, adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
    await h.bridge.start();
    const jobID = `job_${c.kind}_0001`;
    await h.port.inbound(offer(jobID));
    await landOnProvider(h, jobID);
    const outcome = h.frames().find((f) => f.type === "provider_outcome");
    expect(outcome?.payload["outcome"]).toBe(c.outcome);
    expect(outcome?.payload["adapter_version"]).toBe("0.3.1");
    expect(h.downloads.started.length).toBe(0);
  }
});

test("login verdict stays auth_pending — no outcome frame, no click", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_login_0001"));
  await landOnProvider(h, "job_login_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.downloads.started.length).toBe(0);
});

test("unknown escalates to ui_changed only on the second observation ≥5s later", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_unknown_0001"));

  // First unknown: no outcome, streak recorded.
  await landOnProvider(h, "job_unknown_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);

  // Second unknown, ≥5s later: ui_changed emitted once.
  h.clock.now += 5000;
  await landOnProvider(h, "job_unknown_0001");
  const outcomes = h.frames().filter((f) => f.type === "provider_outcome");
  expect(outcomes.length).toBe(1);
  expect(outcomes[0]?.payload["outcome"]).toBe("ui_changed");
});

test("two unknowns <5s apart do not escalate", async () => {
  const h = makeMapHarness();
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_unknown_0002"));
  await landOnProvider(h, "job_unknown_0002");
  h.clock.now += 4000;
  await landOnProvider(h, "job_unknown_0002");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
});

test("a decisive verdict between two unknowns resets the streak", async () => {
  const h = makeMapHarness();
  await h.bridge.start();
  await h.port.inbound(offer("job_reset_0001"));

  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await landOnProvider(h, "job_reset_0001");
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);

  // A login page (decisive) breaks the streak.
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  h.clock.now += 6000;
  await landOnProvider(h, "job_reset_0001");
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(0);

  // Next unknown starts a fresh streak (count 1), so no ui_changed yet.
  h.scripting.verdict = { kind: "unknown", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  h.clock.now += 6000;
  await landOnProvider(h, "job_reset_0001");
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.backend.store.activeJobs[0]?.unknown_count).toBe(1);
});
