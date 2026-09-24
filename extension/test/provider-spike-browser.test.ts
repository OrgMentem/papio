// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, beforeAll, expect, mock, test } from "bun:test";
import { join } from "node:path";
import type { ActiveJob } from "../src/state";
import { FakeEmitter } from "./fake-tabs";

const config = {
  socket: "ws://127.0.0.1:54321/test/socket",
  jobID: "job_0123456789abcdef01234567",
  entryURL: "https://provider.example/article/10.1234/Test",
  doi: "10.1234/Test",
};
const managedID = 7, recoveryID = 42;
const bundles = new Map<boolean, string>();
const savedGlobals = new Map<string, PropertyDescriptor | undefined>();
const storageWrites: unknown[] = [];

beforeAll(async () => {
  // Exercise the same explicit define and bundled entry point as the runner,
  // without importing that runner, opening a socket, or writing a dist bundle.
  for (const afterDrift of [false, true]) {
    const built = await Bun.build({
      // A file URL's pathname is "/C:/..." on Windows, which Bun.build rejects.
      entrypoints: [join(import.meta.dir, "..", "tools", "provider-spike-browser.ts")],
      target: "browser", format: "esm",
      define: { PROVIDER_SPIKE: JSON.stringify({ ...config, afterDrift }) },
    });
    if (!built.success || built.outputs.length !== 1) throw new Error(built.logs.join("\n"));
    bundles.set(afterDrift, await built.outputs[0]!.text());
  }
});

afterEach(() => {
  for (const [name, descriptor] of savedGlobals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
  savedGlobals.clear();
  const writes = storageWrites.splice(0);
  expect(writes).toEqual([]);
});

const activeJob = (patch: Partial<ActiveJob> = {}): ActiveJob => ({
  job_id: config.jobID, tab_id: managedID, status: "awaiting_download",
  offered_at: Date.now(), expires_at: Date.now() + 600_000,
  access_mode: "assisted", expected: { doi: config.doi },
  provider_hosts: ["provider.example"], ...patch,
});
const manualJob = (patch: Partial<ActiveJob> = {}): ActiveJob => {
  const job = activeJob({ tab_id: -1 });
  delete job.access_mode;
  return { ...job, ...patch };
};

interface Download {
  id: number;
  url: string;
  filename: string;
  startTime: string;
  state: "in_progress" | "complete" | "interrupted";
}
const download = (patch: Partial<Download> = {}): Download => ({
  id: 409, url: "https://provider.example/main.pdf",
  filename: `/Downloads/papio/${config.jobID}/paper.pdf`,
  startTime: new Date(Date.now() + 1000).toISOString(), state: "complete", ...patch,
});
interface Reply { id?: number; ready?: boolean; result?: unknown; error?: string }
type Tab = { id: number; url: string; status: "complete" | "loading" };

async function harness(options: { afterDrift?: boolean; jobs?: ActiveJob[]; installType?: string; localOnly?: boolean } = {}) {
  const jobs = structuredClone(options.jobs ?? [activeJob()]);
  const tabs = new Map<number, Tab>([[managedID, { id: managedID, url: config.entryURL, status: "complete" }]]);
  const downloads: Download[] = [];
  const onCreated = new FakeEmitter<[Download]>();
  const onChanged = new FakeEmitter<[{ id: number; state: { current: string } }]>();
  const storage = () => ({
    get: mock(async (key: string) => {
      expect(key).toBe("papio_state_v1");
      return { papio_state_v1: structuredClone({ version: 1, activeJobs: jobs }) };
    }),
    set: mock(async (value: unknown) => { storageWrites.push({ method: "set", value }); }),
    remove: mock(async (value: unknown) => { storageWrites.push({ method: "remove", value }); }),
    clear: mock(async () => { storageWrites.push({ method: "clear" }); }),
  });
  const local = storage(), session = storage();
  const create = mock(async (properties: { url: string; active: boolean }) => {
    const tab: Tab = { id: recoveryID, url: properties.url, status: "complete" };
    tabs.set(tab.id, tab);
    return structuredClone(tab);
  });
  const remove = mock(async (id: number) => { tabs.delete(id); });
  let scriptResult: unknown = { status: "blocked", reason: "No permitted control" };
  const executeScript = mock(async (_injection: unknown) => [{ documentId: "provider-document-1", result: scriptResult }]);
  const search = mock(async ({ startedAfter }: { startedAfter: string }) =>
    structuredClone(downloads.filter(item => Date.parse(item.startTime) >= Date.parse(startedAfter))));
  const chromeFake = {
    storage: { local, ...(options.localOnly ? {} : { session }) },
    management: { getSelf: mock(async () => ({ installType: options.installType ?? "development" })) },
    tabs: {
      create, remove,
      get: mock(async (id: number) => {
        const tab = tabs.get(id);
        if (!tab) throw new Error("No such tab");
        return structuredClone(tab);
      }),
    },
    downloads: { onCreated, onChanged, search },
    scripting: { executeScript },
  };
  const open = new FakeEmitter<[]>(), message = new FakeEmitter<[{ data: string }]>();
  const sent: Reply[] = [];
  class Socket {
    constructor(url: string) { expect(url).toBe(config.socket); }
    addEventListener(name: string, listener: (...args: any[]) => unknown) {
      if (name === "open") open.addListener(listener);
      else if (name === "message") message.addListener(listener);
      else throw new Error(`Unexpected socket listener: ${name}`);
    }
    send(data: string) { sent.push(JSON.parse(data) as Reply); }
  }
  const pre = { textContent: "" };
  for (const [name, value] of Object.entries({
    chrome: chromeFake, WebSocket: Socket,
    document: { querySelector: (selector: string) => { expect(selector).toBe("pre"); return pre; } },
  })) {
    if (!savedGlobals.has(name)) savedGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  // Each import owns fresh module state/listeners, even for the same config.
  const source = bundles.get(options.afterDrift ?? false)! + `\n// test instance ${crypto.randomUUID()}\n`;
  await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
  await open.emit();
  expect(sent).toEqual([{ ready: true }]);
  let sequence = 0;
  return {
    jobs, tabs, downloads, onCreated, onChanged, create, remove, executeScript, search, local, session,
    setScriptResult(value: unknown) { scriptResult = value; },
    async rpc(method: string, input: object = {}): Promise<Reply> {
      const id = ++sequence, before = sent.length;
      await message.emit({ data: JSON.stringify({ id, method, input }) });
      expect(sent).toHaveLength(before + 1);
      expect(sent.at(-1)?.id).toBe(id);
      return sent.at(-1)!;
    },
  };
}

test("managed tab survives a blocked trial and repeated cleanup until its binding retires", async () => {
  const h = await harness();
  expect(await h.rpc("setup")).toMatchObject({ result: { tabID: managedID, recoveryTab: false } });
  expect(await h.rpc("observe")).toMatchObject({ result: { status: "blocked" } });
  for (let i = 0; i < 2; i++) {
    expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false, reason: expect.stringContaining("bound") } });
  }
  expect(h.create).not.toHaveBeenCalled();
  expect(h.remove).not.toHaveBeenCalled();
  h.jobs.splice(0); // the real bridge retired the binding
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: true } });
  expect(h.remove.mock.calls).toEqual([[managedID]]);
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: true, alreadyGone: true } });
  expect(h.remove).toHaveBeenCalledTimes(1);
});

test("download completion alone never retires a managed tab or its active job", async () => {
  const h = await harness(), before = structuredClone(h.jobs);
  expect(await h.rpc("setup")).toHaveProperty("result");
  const item = download({ state: "in_progress" });
  h.downloads.push(item);
  await h.onCreated.emit(item);
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false } });
  item.state = "complete";
  await h.onChanged.emit({ id: item.id, state: { current: "complete" } });
  expect(await h.rpc("artifact")).toMatchObject({ result: {
    item: { id: item.id, state: "complete" }, events: [{ kind: "created" }, { kind: "changed" }],
  } });
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false } });
  expect(h.remove).not.toHaveBeenCalled();
  expect(h.jobs).toEqual(before);
  h.jobs.splice(0);
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: true } });
  expect(h.remove.mock.calls).toEqual([[managedID]]);
});

test.each([["session", false], ["local", true]] as const)("drift recovery uses the retained manual window via %s storage without creating job state", async (_area, localOnly) => {
  const job = manualJob({ expected: { doi: config.doi.toUpperCase() } });
  const otherJob = activeJob({ job_id: "job_other", tab_id: 9 });
  const h = await harness({ afterDrift: true, jobs: [otherJob, job], localOnly });
  const before = structuredClone(h.jobs);
  expect(await h.rpc("setup")).toMatchObject({ result: { tabID: recoveryID, recoveryTab: true, job } });
  expect(h.create.mock.calls).toEqual([[{ url: config.entryURL, active: false }]]);
  expect(await h.rpc("setup")).toMatchObject({ error: "Already set up" });
  expect(h.create).toHaveBeenCalledTimes(1);
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: true } });
  expect(h.remove.mock.calls).toEqual([[recoveryID]]);
  expect(h.tabs.has(managedID)).toBe(true);
  expect(h.jobs).toEqual(before);
  expect(localOnly ? h.session.get : h.local.get).not.toHaveBeenCalled();
});

test("recovery tab stays open during a job download and can close after completion with the manual window retained", async () => {
  const h = await harness({ afterDrift: true, jobs: [manualJob()] }), before = structuredClone(h.jobs);
  expect(await h.rpc("setup")).toHaveProperty("result");
  const item = download({ state: "in_progress" });
  h.downloads.push(item);
  await h.onCreated.emit(item);
  for (let i = 0; i < 2; i++) {
    expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false, reason: "Download in progress; retained" } });
  }
  expect(h.remove).not.toHaveBeenCalled();
  item.state = "complete";
  await h.onChanged.emit({ id: item.id, state: { current: "complete" } });
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: true } });
  expect(h.remove.mock.calls).toEqual([[recoveryID]]);
  expect(h.jobs).toEqual(before);
});

test("ambiguous job downloads and a tab that left the article are retained for inspection", async () => {
  const h = await harness({ afterDrift: true, jobs: [manualJob()] });
  expect(await h.rpc("setup")).toHaveProperty("result");
  h.downloads.push(download(), download({ id: 410, state: "in_progress" }));
  expect(await h.rpc("cleanup")).toMatchObject({ error: expect.stringContaining("Multiple job downloads") });
  expect(h.remove).not.toHaveBeenCalled();
  h.downloads.splice(0);
  h.tabs.get(recoveryID)!.url = "https://provider.example/another-article";
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false, reason: expect.stringContaining("changed") } });
  expect(h.remove).not.toHaveBeenCalled();
});

test.each([
  ["wrong DOI", { expected: { doi: "10.1234/Other" } }],
  ["missing DOI", { expected: {} }],
  ["wrong host", { provider_hosts: ["other.example"] }],
  ["lookalike host", { provider_hosts: ["provider.example.attacker.test"] }],
  ["wrong status", { status: "auth_pending" }],
  ["assisted mode still set", { access_mode: "assisted" }],
  ["delegated mode still set", { access_mode: "delegated" }],
  ["manual delivery required", { manual_delivery_required: true }],
  ["download already initiated", { download_initiated: true }],
  ["different job", { job_id: "job_other" }],
] satisfies [string, Partial<ActiveJob>][])("recovery refuses an invalid retained window: %s", async (_name, patch) => {
  const h = await harness({ afterDrift: true, jobs: [manualJob(patch)] });
  expect(await h.rpc("setup")).toHaveProperty("error");
  expect(h.create).not.toHaveBeenCalled();
  expect(h.executeScript).not.toHaveBeenCalled();
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false } });
  expect(h.remove).not.toHaveBeenCalled();
});

test("missing job or missing drift attribution cannot create a recovery surface", async () => {
  for (const options of [{ afterDrift: true, jobs: [] }, { afterDrift: false, jobs: [manualJob()] }]) {
    const h = await harness(options);
    expect(await h.rpc("setup")).toHaveProperty("error");
    expect(h.create).not.toHaveBeenCalled();
    expect(h.remove).not.toHaveBeenCalled();
  }
});

test.each(["normal", "admin", "sideload", "other"])("installation %s cannot select or create provider tabs", async installType => {
  const h = await harness({ afterDrift: true, jobs: [manualJob()], installType });
  expect(await h.rpc("setup")).toMatchObject({ error: "Development installation required" });
  expect(h.session.get).not.toHaveBeenCalled();
  expect(h.local.get).not.toHaveBeenCalled();
  expect(h.create).not.toHaveBeenCalled();
  expect(h.executeScript).not.toHaveBeenCalled();
  expect(h.remove).not.toHaveBeenCalled();
});

test("recovery rechecks the retained window before injecting another action", async () => {
  const h = await harness({ afterDrift: true, jobs: [manualJob()] });
  expect(await h.rpc("setup")).toHaveProperty("result");
  h.setScriptResult({ status: "observed" });
  expect(await h.rpc("observe")).toMatchObject({ result: { status: "observed" } });
  h.jobs[0]!.expected = { doi: "10.1234/Other" };
  expect(await h.rpc("act", { choice: "c1", revision: "revision-1" })).toMatchObject({ error: expect.stringContaining("binding changed") });
  expect(h.executeScript).toHaveBeenCalledTimes(1);
});

test("recovery retains a provider download before Chrome assigns its job filename", async () => {
  const h = await harness({ afterDrift: true, jobs: [manualJob()] });
  expect(await h.rpc("setup")).toHaveProperty("result");
  const item = download({ filename: "", state: "in_progress" });
  h.downloads.push(item);
  await h.onCreated.emit(item);
  expect(await h.rpc("artifact")).toMatchObject({ result: { events: [{ kind: "created" }] } });
  expect(await h.rpc("cleanup")).toMatchObject({ result: { closed: false } });
  expect(h.remove).not.toHaveBeenCalled();
});
