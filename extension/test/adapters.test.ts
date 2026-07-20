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
  type DownloadRule,
  type PageVerdict,
} from "../src/adapters/types";
import { parseBrowserMessage, type BrowserMessage } from "../src/protocol";
import { emptyStore, type StateBackend, type StoreShape, type TermsConsent } from "../src/state";
import {
  Bridge,
  clickTermsAccept,
  resolveDownloadURL,
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

test("registered adapters leave work-window visibility at the default", () => {
  for (const spec of adapters) expect(spec.requiresVisible).toBeUndefined();
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

// Wiley Online Library: captured 2026-07-17 from a Example University-authenticated article
// (fixtures/wiley/success.html). The article page carries the Highwire
// citation_pdf_url meta the adapter downloads through.
const wileyArticle = loadFixture("wiley", "success");
test.skipIf(wileyArticle === null)(
  "captured wiley article fixture classifies as article via the citation metas",
  () => {
    const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
    const verdict = interpret(wileyArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("wiley");
  },
);

test("wiley stays unknown on a page lacking the citation_pdf_url/title metas", () => {
  const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
  const page = parseHTML("<!doctype html><html><head><title>Journal home</title></head><body><h1>Psychology &amp; Marketing</h1></body></html>");
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

test("wiley download builds the /doi/pdfdirect endpoint from the DOI in the page URL", () => {
  const spec = adapters.find((a) => a.id === "wiley") as AdapterSpec;
  const rule = spec.download as DownloadRule;
  expect(rule.method).toBe("url");
  // Mirrors resolveDownloadURL's substitution against location.href.
  const build = (href: string): string | null => {
    const m = href.match(new RegExp(rule.idPattern as string));
    if (!m) return null;
    return (rule.urlTemplate as string).replace(
      /\{(\d+|id)\}/g,
      (_, k: string) => m[k === "id" ? 1 : Number(k)] ?? "",
    );
  };
  const want = "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1002/mar.21498?download=true";
  expect(build("https://onlinelibrary.wiley.com/doi/10.1002/mar.21498")).toBe(want);
  expect(build("https://onlinelibrary.wiley.com/doi/full/10.1002/mar.21498")).toBe(want);
  expect(build("https://onlinelibrary.wiley.com/doi/epdf/10.1002/mar.21498")).toBe(want);
  // A different DOI (slashed suffix) still resolves.
  expect(build("https://onlinelibrary.wiley.com/doi/abs/10.1111/jcpp.13440")).toBe(
    "https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcpp.13440?download=true",
  );
});

// SAGE Journals: captured 2026-07-17 via CDP from a Example University-authenticated article
// (fixtures/sage/success.html). No Highwire metas; classifies on publication_doi
// + the downloadPdfUrl anchor, downloads that anchor's href (method href).
const sageArticle = loadFixture("sage", "success");
test.skipIf(sageArticle === null)(
  "captured sage article fixture classifies as article via publication_doi + pdf anchor",
  () => {
    const spec = adapters.find((a) => a.id === "sage") as AdapterSpec;
    const verdict = interpret(sageArticle as Document, spec, ctx());
    expect(verdict.kind).toBe("article");
    expect(verdict.adapter_id).toBe("sage");
  },
);

test("sage stays unknown without the publication_doi + downloadPdfUrl signals", () => {
  const spec = adapters.find((a) => a.id === "sage") as AdapterSpec;
  const page = parseHTML("<!doctype html><html><head><title>SAGE Journals</title></head><body><h1>Journal home</h1></body></html>");
  expect(interpret(page, spec, ctx()).kind).toBe("unknown");
});

// ProQuest "Find your institution" wall (fixtures/proquest/login-return.html,
// captured live via CDP): Example University routes heavily through ProQuest, and without a
// ProQuest session it blocks the article behind an institution-selection form.
// The login rule (ordered before article) must catch it so papio surfaces a
// sign-in step instead of staying assisted.
const pqLogin = loadFixture("proquest", "login-return");
test.skipIf(pqLogin === null)(
  "proquest institution wall classifies as login, not unknown/article",
  () => {
    const spec = adapters.find((a) => a.id === "proquest") as AdapterSpec;
    expect(interpret(pqLogin as Document, spec, ctx()).kind).toBe("login");
  },
);

const pqSuccess = loadFixture("proquest", "success");
test.skipIf(pqSuccess === null)(
  "proquest entitled docview still classifies as article after the login rule",
  () => {
    const spec = adapters.find((a) => a.id === "proquest") as AdapterSpec;
    expect(interpret(pqSuccess as Document, spec, ctx()).kind).toBe("article");
  },
);

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
  readonly navigations: [number, { active?: boolean; url?: string }][] = [];
  async reload(_tabID: number): Promise<void> {}
  async update(tabID: number, props: { active?: boolean; url?: string }): Promise<TabInfo> {
    this.navigations.push([tabID, props]);
    if (props.url !== undefined) this.live.set(tabID, { id: tabID, url: props.url });
    return this.live.get(tabID) ?? { id: tabID };
  }
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
  emitOnCreated = false;
  crossOriginRedirect: string | undefined = undefined;
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
    if (this.emitOnCreated) {
      // Chrome dispatches onCreated at creation (pre-redirect, so the URL still
      // matches the pending offer) and does NOT wait for the async handler
      // before asking for a filename — so do not await here. This makes the
      // test exercise the synchronous ID binding, not the post-await one.
      void this.onCreated.emit({
        id,
        url: options.url,
        filename: "/Users/test/Downloads/out.pdf",
        fileSize: 12345,
        state: "in_progress",
      });
    }
    this.items.set(id, {
      id,
      url: this.crossOriginRedirect ?? options.url.replace("TOKEN=ephemeral", "TOKEN=normalized"),
      finalUrl: this.crossOriginRedirect ?? "https://media.proquest.com/redirected/out.pdf",
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
  constructedURL: string | null = "https://provider.example.edu/pdf/default.pdf";
  readonly constructedArgs: { tabId: number; selector: string; idPattern: unknown; urlTemplate: unknown; jsonField: unknown }[] = [];
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
    // resolveDownloadURL(selector, idPattern, urlTemplate, jsonField): uniquely
    // 4-arg (interpret is 3, click is 5).
    if (args.length === 4) {
      this.constructedArgs.push({ tabId: inj.target.tabId, selector: String(args[0]), idPattern: args[1], urlTemplate: args[2], jsonField: args[3] });
      return [{ result: this.constructedURL }];
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
  alarms: { created: { name: string }[]; fire(name: string): void };
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
  const alarmListeners: ((a: { name: string }) => void)[] = [];
  const alarms = {
    created: [] as { name: string; info: { periodInMinutes: number } }[],
    create: (name: string, info: { periodInMinutes: number }) => {
      alarms.created.push({ name, info });
    },
    onAlarm: { addListener: (cb: (a: { name: string }) => void) => alarmListeners.push(cb) },
    fire: (name: string) => {
      for (const cb of alarmListeners) cb({ name });
    },
  };
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
    action: {
      setBadgeText: async () => {},
      setBadgeBackgroundColor: async () => {},
    },
    alarms,
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
    alarms,
    frames: () => port.posted.map(parseBrowserMessage),
  };
}

function offer(
  jobID: string,
  expected?: { title?: string },
  providerHosts: string[] = [PROVIDER],
  loginEntityID?: string,
  proquestAccountID?: string,
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
      ...(loginEntityID !== undefined ? { login_entity_id: loginEntityID } : {}),
      ...(proquestAccountID !== undefined ? { proquest_account_id: proquestAccountID } : {}),
    },
  };
}

async function landOnProvider(
  h: MapHarness,
  jobID: string,
  host: string = PROVIDER,
  url: string = `https://${host}/pqdweb`,
): Promise<number> {
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === jobID)?.tab_id ?? -1;
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

test("startup re-drives a pending terms gate when consent was granted while asleep", async () => {
  // The consent grant's one-shot re-drive can miss a job if the worker was
  // asleep when the popup message arrived. On the next connect, startup must
  // re-drive any still-flagged gate now that consent is "accept".
  const h = makeMapHarness([TERMS_SPEC]);
  h.scripting.verdict = termsVerdict; // consent undefined -> flags the gate
  await h.bridge.start();
  await h.port.inbound(offer("job_terms_wake_0001"));
  await landOnProvider(h, "job_terms_wake_0001");
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(h.scripting.termsAccepts.length).toBe(0);

  // Consent recorded directly (popup wrote it) while the one-shot re-drive
  // never ran for this job — it stays flagged with its tab still open.
  h.settings.consent = "accept";
  await h.bridge.start(); // worker wakes

  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(false);
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
});

const TERMS_DL_SPEC: AdapterSpec = {
  id: "termsdl",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [],
  download: { selector: "button.dl", requireKind: "article", method: "click" },
  termsAccept: { modalSelector: "div.terms[open]", textAny: ["accept and download"] },
};

test("a latched download-click keeps re-classifying until a late terms modal is accepted", async () => {
  // Terms-gated providers (JSTOR) upgrade the terms modal AFTER the download
  // click latches download_initiated. The classify retry must keep watching so
  // the late modal is caught and accepted — without ever re-clicking / starting
  // a second download.
  const h = makeMapHarness([TERMS_DL_SPEC]);
  h.settings.consent = "accept";
  const article = { kind: "article" as const, adapter_id: "termsdl", adapter_version: "1.0.0", evidence: [] };
  const terms = { kind: "terms" as const, adapter_id: "termsdl", adapter_version: "1.0.0", evidence: [] };
  // 1st classify: article -> click (latches). 2nd (retry): still article (modal
  // not upgraded). 3rd (retry): terms -> acceptTerms.
  h.scripting.verdictQueue.push(article, article, terms);
  await h.bridge.start();
  await h.port.inbound(offer("job_termsdl_0001"));
  await landOnProvider(h, "job_termsdl_0001");

  expect(h.scripting.clicked.length).toBe(1); // download clicked once (latched)
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
  expect(h.scripting.termsAccepts.length).toBe(0); // modal not upgraded yet
  expect(h.timers.length).toBeGreaterThan(0); // retry scheduled despite the latch

  // Drain retries: the late terms modal is caught and accepted.
  for (let i = 0; i < 8 && h.scripting.termsAccepts.length === 0 && h.timers.length > 0; i++) {
    for (const t of h.timers.splice(0)) await t.fn();
  }
  expect(h.scripting.termsAccepts.length).toBeGreaterThanOrEqual(1);
  expect(h.scripting.clicked.length).toBe(1); // retry never re-clicked the download
});

test("clickTermsAccept clicks the real accept control, not a wrapping container", () => {
  // JSTOR's terms footer is a <div> holding both "Cancel" and "Accept and
  // download"; the accept control is an mfe-*-button with a shadow
  // #button-element. The walk must click the button, never the container div
  // (which is a no-op and left the modal open live).
  const win = new Window();
  const doc = win.document;
  doc.body.innerHTML =
    "<mfe-download-pharos-modal class='terms-and-conditions' open>" +
    "<div class='cta'>" +
    "<mfe-download-pharos-button id='cancel'>Cancel</mfe-download-pharos-button>" +
    "<mfe-download-pharos-button id='accept'>Accept and download</mfe-download-pharos-button>" +
    "</div></mfe-download-pharos-modal>";
  const clicks: string[] = [];
  doc.querySelector(".cta")?.addEventListener("click", () => clicks.push("div"));
  for (const id of ["cancel", "accept"]) {
    const btn = doc.getElementById(id) as unknown as {
      attachShadow: (init: { mode: string }) => ShadowRoot;
    };
    const sr = btn.attachShadow({ mode: "open" });
    sr.innerHTML = "<button id='button-element'></button>";
    sr.querySelector("#button-element")?.addEventListener("click", () => clicks.push(id));
  }
  const prev = globalThis.document;
  Object.assign(globalThis, { document: doc });
  try {
    const ok = clickTermsAccept("mfe-download-pharos-modal.terms-and-conditions[open]", ["accept and download"]);
    expect(ok).toBe(true);
    // The accept button's shadow #button-element is the click TARGET (old bug
    // clicked the container div directly, so target was "div"). A composed
    // click then bubbles to .cta, which is fine — Cancel is never clicked.
    expect(clicks[0]).toBe("accept");
    expect(clicks).not.toContain("cancel");
  } finally {
    Object.assign(globalThis, { document: prev });
  }
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

test("repeated authentication failures cap re-driving and report human_auth_required", async () => {
  // A warm session that cannot clear the IdP (expired SSO) would otherwise be
  // re-offered and re-driven forever, thrashing the provider. After
  // MAX_AUTH_ATTEMPTS drives that reach auth without a download, the extension
  // must stop opening broker tabs and report the human step instead.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";

  // Three drives that each reach authentication but never download.
  for (let i = 0; i < 3; i++) {
    await h.port.inbound(offer("job_authstall_0001"));
    const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authstall_0001")?.tab_id ?? -1;
    expect(tabID).toBeGreaterThanOrEqual(0);
    h.tabs.live.set(tabID, { id: tabID, url: idp });
    await h.tabs.onUpdated.emit(tabID, { url: idp }, { id: tabID, url: idp });
    expect(h.backend.store.activeJobs.find((j) => j.job_id === "job_authstall_0001")?.status).toBe("auth_pending");
    h.tabs.live.delete(tabID); // tab dies before the session ever authenticates
  }
  expect(h.backend.store.authAttempts?.["job_authstall_0001"]).toBe(3);

  const tabsBefore = h.tabs.nextId;
  const outcomesBefore = h.frames().filter((f) => f.type === "provider_outcome").length;

  // Fourth offer is capped: no broker tab opens and one human_auth_required
  // outcome is reported. The job is not re-tracked (no re-drive this session).
  await h.port.inbound(offer("job_authstall_0001"));
  expect(h.tabs.nextId).toBe(tabsBefore); // tabs.create never called
  expect(h.backend.store.activeJobs.some((j) => j.job_id === "job_authstall_0001")).toBe(false);
  const outcomes = h.frames().filter((f) => f.type === "provider_outcome");
  expect(outcomes.length).toBe(outcomesBefore + 1);
  expect(outcomes.at(-1)?.payload["outcome"]).toBe("human_auth_required");

  // A further capped offer this worker lifetime stays quiet (no re-report, no tab).
  await h.port.inbound(offer("job_authstall_0001"));
  expect(h.frames().filter((f) => f.type === "provider_outcome").length).toBe(outcomesBefore + 1);
  expect(h.tabs.nextId).toBe(tabsBefore);
});

test("a completed download clears the auth-failure budget", async () => {
  // An earlier expired-session streak must not cap a job whose session later
  // works: a real download proves auth succeeded and resets the counter.
  const h = makeMapHarness([SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  const idp = "https://idp.example.edu/sso?SAMLRequest=x";

  for (let i = 0; i < 2; i++) {
    await h.port.inbound(offer("job_authreset_0001"));
    const t = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreset_0001")?.tab_id ?? -1;
    h.tabs.live.set(t, { id: t, url: idp });
    await h.tabs.onUpdated.emit(t, { url: idp }, { id: t, url: idp });
    h.tabs.live.delete(t);
  }
  expect(h.backend.store.authAttempts?.["job_authreset_0001"]).toBe(2);

  // Third drive authenticates and downloads the article.
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.port.inbound(offer("job_authreset_0001"));
  const tabID = h.backend.store.activeJobs.find((j) => j.job_id === "job_authreset_0001")?.tab_id ?? -1;
  h.tabs.live.set(tabID, { id: tabID, url: idp });
  await h.tabs.onUpdated.emit(tabID, { url: idp }, { id: tabID, url: idp });
  const prov = `https://${PROVIDER}/doc?x=1`;
  h.tabs.live.set(tabID, { id: tabID, url: prov });
  await h.tabs.onUpdated.emit(tabID, { url: prov }, { id: tabID, url: prov });
  expect(h.downloads.started.length).toBe(1);
  await h.downloads.onChanged.emit({ id: 701, state: { current: "complete" } });

  expect(h.backend.store.authAttempts?.["job_authreset_0001"]).toBeUndefined();
});

test("startup registers the periodic keepalive alarm", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  expect(h.alarms.created.some((a) => a.name === "papio-keepalive")).toBe(true);
});

test("the keepalive alarm reconnects a worker whose native port had dropped", async () => {
  // MV3 dormancy / daemon restart kills the port; the setTimeout backoff dies
  // with a sleeping worker, so the alarm wake must re-establish the connection.
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  const hellosBefore = h.frames().filter((f) => f.type === "hello").length;

  h.port.disconnect(); // port death; onDisconnect nulls the port + queues a backoff timer (unfired)
  await Promise.resolve();
  h.alarms.fire("papio-keepalive");
  await Promise.resolve();
  await Promise.resolve();

  const hellosAfter = h.frames().filter((f) => f.type === "hello").length;
  expect(hellosAfter).toBe(hellosBefore + 1);
});

test("the keepalive alarm is a no-op while the port is healthy", async () => {
  const h = makeMapHarness([SPEC]);
  await h.bridge.start();
  const hellosBefore = h.frames().filter((f) => f.type === "hello").length;
  h.alarms.fire("papio-keepalive");
  await Promise.resolve();
  await Promise.resolve();
  expect(h.frames().filter((f) => f.type === "hello").length).toBe(hellosBefore);
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

// Federated login-routing: on a login wall, when the adapter declares a
// federatedLogin route and the offer carried the institution entityID, papio
// navigates the handoff tab straight to the IdP (auto-selecting the institution)
// — credential entry still stays with the human.
const FED_LOGIN_SPEC: AdapterSpec = {
  id: "proquest",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "login", all: ["#login-form"] }],
  federatedLogin: "https://sp.example/Shibboleth.sso/DS?entityID={entityID}&target=https://sp.example/home",
};

// Account-id unlock (ProQuest): on a login wall, appending ?accountid=<id> to
// the current URL unlocks institutional access with no sign-in. Preferred over
// the federated route when the offer carries an account id.
const ACCT_SPEC: AdapterSpec = {
  id: "proquest",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [{ kind: "login", all: ["#login-form"] }],
  accountIdParam: "accountid",
  federatedLogin: "https://sp.example/Shibboleth.sso/DS?entityID={entityID}&target=https://sp.example/home",
};

test("login verdict appends the account id to the current URL, preferring it over federated login", async () => {
  const h = makeMapHarness([ACCT_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_acct_0001", undefined, [PROVIDER], "https://idp.example.edu/entity", "12345"));
  const tabID = await landOnProvider(h, "job_acct_0001", PROVIDER, `https://${PROVIDER}/openurl/handler/x`);
  const nav = h.tabs.navigations.filter(([, p]) => p.url !== undefined).map(([, p]) => p.url);
  expect(nav).toContain(`https://${PROVIDER}/openurl/handler/x?accountid=12345`);
  // Account id preferred: no federated (DS) navigation.
  expect(nav.some((u) => u?.includes("Shibboleth.sso/DS"))).toBe(false);
});

test("account id unlock does not fire without an offer account id (falls back to federated)", async () => {
  const h = makeMapHarness([ACCT_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_acct_noacct_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_acct_noacct_0001", PROVIDER, `https://${PROVIDER}/openurl/handler/x`);
  const nav = h.tabs.navigations.filter(([, p]) => p.url !== undefined).map(([, p]) => p.url);
  expect(nav.some((u) => u?.includes("accountid="))).toBe(false);
  expect(nav.some((u) => u?.includes("Shibboleth.sso/DS"))).toBe(true);
});

test("login verdict routes the handoff tab to the federated login with the offer entityID", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fedlogin_0001");
  expect(h.tabs.navigations).toContainEqual([
    tabID,
    { url: "https://sp.example/Shibboleth.sso/DS?entityID=https%3A%2F%2Fidp.example.edu%2Fentity&target=https://sp.example/home" },
  ]);
  // Still a human sign-in step: no outcome, no download.
  expect(h.frames().some((f) => f.type === "provider_outcome")).toBe(false);
  expect(h.downloads.started.length).toBe(0);
});

test("login verdict does not route without an offer entityID", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_noent_0001"));
  await landOnProvider(h, "job_fedlogin_noent_0001");
  expect(h.tabs.navigations.some(([, p]) => p.url !== undefined)).toBe(false);
});

test("login verdict does not re-route while the human is signing in (latched)", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedlogin_latch_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  await landOnProvider(h, "job_fedlogin_latch_0001");
  await landOnProvider(h, "job_fedlogin_latch_0001");
  const routes = h.tabs.navigations.filter(([, p]) => p.url !== undefined);
  expect(routes.length).toBe(1);
});

test("federated login return re-drives the openurl once, warm, to reach the article", async () => {
  const h = makeMapHarness([FED_LOGIN_SPEC]);
  h.scripting.verdict = { kind: "login", adapter_id: "proquest", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_fedredrive_0001", undefined, [PROVIDER], "https://idp.example.edu/entity"));
  const tabID = await landOnProvider(h, "job_fedredrive_0001");
  // Simulate the federated round-trip: tab goes to the IdP, then returns.
  const idp = "https://idp.example.edu/idp/profile/SAML2/Redirect/SSO";
  h.tabs.live.set(tabID, { id: tabID, url: idp });
  await h.tabs.onUpdated.emit(tabID, { url: idp }, { id: tabID, url: idp });
  expect(h.backend.store.activeJobs[0]?.status).toBe("auth_pending");
  const prov = `https://${PROVIDER}/pqdweb?doc=1`;
  h.tabs.live.set(tabID, { id: tabID, url: prov });
  await h.tabs.onUpdated.emit(tabID, { url: prov }, { id: tabID, url: prov });
  // On the auth return, papio re-drives the original openurl exactly once.
  const openurlDrives = h.tabs.navigations.filter(([, p]) => p.url === OPENURL);
  expect(openurlDrives.length).toBe(1);
  expect(openurlDrives[0]?.[0]).toBe(tabID);
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

const URL_SPEC: AdapterSpec = {
  id: "urlprov",
  version: "1.0.0",
  hosts: [PROVIDER],
  classify: [],
  download: {
    selector: "button.dl",
    requireKind: "article",
    method: "url",
    idPattern: "/stable/([^?#]+)",
    urlTemplate: "https://provider.example.edu/pdf/{id}.pdf",
    requiresTermsConsent: true,
  },
};

test("url-method adapter fetches the direct endpoint autonomously with terms consent", async () => {
  // JSTOR-class: the entitled PDF is at a constructible URL. With consent,
  // fetch it via the downloads API — no click, no gesture.
  const h = makeMapHarness([URL_SPEC]);
  h.settings.consent = "accept";
  h.scripting.constructedURL = "https://provider.example.edu/pdf/4093878.pdf";
  h.scripting.verdict = { kind: "article", adapter_id: "urlprov", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_url_0001"));
  await landOnProvider(h, "job_url_0001");

  expect(h.scripting.clicked.length).toBe(0); // no gesture click
  expect(h.downloads.started.length).toBe(1);
  expect(h.downloads.started[0]?.url).toBe("https://provider.example.edu/pdf/4093878.pdf");
  expect(h.backend.store.activeJobs[0]?.download_initiated).toBe(true);
});

test("url-method adapter stays assisted (prompts, no fetch) without terms consent", async () => {
  const h = makeMapHarness([URL_SPEC]);
  // consent undefined -> gate stays human
  h.scripting.verdict = { kind: "article", adapter_id: "urlprov", adapter_version: "1.0.0", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_url_0002"));
  await landOnProvider(h, "job_url_0002");

  expect(h.downloads.started.length).toBe(0); // no autonomous fetch
  expect(h.backend.store.activeJobs[0]?.needs_terms_consent).toBe(true);
  expect(h.backend.store.activeJobs[0]?.download_initiated).not.toBe(true);
  const prompts = h.frames().filter((f) => f.type === "provider_outcome" && f.payload["outcome"] === "terms_acceptance_required");
  expect(prompts.length).toBeGreaterThanOrEqual(1);
});

test("resolveDownloadURL (url): builds the endpoint from a multi-group template + gate", async () => {
  const win = new Window({ url: "https://www.jstor.org/stable/4093878?seq=1" });
  win.document.body.innerHTML = "<button class='dl'></button>";
  const prev = { document: globalThis.document, location: globalThis.location };
  Object.assign(globalThis, { document: win.document, location: { href: "https://www.jstor.org/stable/4093878?seq=1" } });
  try {
    expect(await resolveDownloadURL("button.dl", "/stable/([^?#]+)", "https://www.jstor.org/stable/pdf/{id}.pdf", null)).toBe(
      "https://www.jstor.org/stable/pdf/4093878.pdf",
    );
    // entitlement gate: control absent -> null (never fetch a non-downloadable page)
    win.document.body.innerHTML = "";
    expect(await resolveDownloadURL("button.dl", null, "https://x/y.pdf", null)).toBeNull();
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("resolveDownloadURL (api): fetches the aggregator JSON and extracts the PDF URL", async () => {
  // EBSCO's exact two-step: viewer URL -> aggregator API (JSON {url}) -> content URL.
  const win = new Window({ url: "https://research.ebsco.com/c/6to2aa/viewer/pdf/mhqkskujrf?route=details" });
  win.document.body.innerHTML = "<div id='v'></div>";
  let fetched = "";
  const prev = { document: globalThis.document, location: globalThis.location, fetch: globalThis.fetch };
  Object.assign(globalThis, {
    document: win.document,
    location: { href: "https://research.ebsco.com/c/6to2aa/viewer/pdf/mhqkskujrf?route=details" },
    fetch: async (u: string) => {
      fetched = u;
      return { ok: true, json: async () => ({ url: "https://content.ebscohost.com/cds/retrieve?content=TOKEN" }) } as Response;
    },
  });
  try {
    const url = await resolveDownloadURL(
      "#v",
      "/c/([^/]+)/viewer/pdf/([^/?#]+)",
      "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/{2}/fulltext/pdf?sourceRecordId={2}&opid={1}&intent=view",
      "url",
    );
    expect(fetched).toBe(
      "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/mhqkskujrf/fulltext/pdf?sourceRecordId=mhqkskujrf&opid=6to2aa&intent=view",
    );
    expect(url).toBe("https://content.ebscohost.com/cds/retrieve?content=TOKEN");
  } finally {
    Object.assign(globalThis, prev);
  }
});

test("cross-origin api download is relocated into papio/<job>/ via the ID bound at onCreated", async () => {
  const h = makeMapHarness();
  // Chrome fires onCreated (pre-redirect) before asking for the filename.
  h.downloads.emitOnCreated = true;
  // The provider's entitled download redirects to a different origin, so the
  // determine-time URL no longer matches the pending offer (the EBSCO case:
  // research.ebsco.com -> content.ebscohost.com).
  h.downloads.crossOriginRedirect = "https://content.ebscohost.com/cds/retrieve?content=signed";
  // The filename is determined before downloads.download resolves, so the
  // initiation code has not tracked the returned ID yet — only onCreated has.
  h.downloads.determineBeforeReturn = true;
  h.scripting.verdict = { kind: "article", adapter_id: "proquest", adapter_version: "0.3.1", evidence: [] };
  await h.bridge.start();
  await h.port.inbound(offer("job_xorigin_0001", { title: EXPECTED_TITLE }));
  await landOnProvider(h, "job_xorigin_0001");
  // Despite the cross-origin determine URL (no pending-URL match) and the ID
  // not yet tracked by the initiation code, the file lands under papio/<job>/
  // because onCreated bound the download ID to the job synchronously.
  expect(h.downloads.items.get(701)?.filename).toBe(
    "/Users/test/Downloads/papio/job_xorigin_0001/out.pdf",
  );
});
