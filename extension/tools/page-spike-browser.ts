// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Packaged only by the explicit development runner, never by build.ts.
import { pageSpikeDOM } from "./page-spike-dom";
import type { ActiveJob } from "../src/state";
declare const SPIKE_SOCKET: string;
declare const SPIKE_PREFIX: string;
declare const SPIKE_JOB_ID: string | null;
declare const SPIKE_PDF_URL: string;
let tabID: number | undefined, documentID: string | undefined, downloadID: number | undefined;
let deliveryStarted = false;
const downloadEvents: unknown[] = [];
const fixtureDownloads = new Set<number>();
chrome.downloads.onCreated.addListener(item => {
  if (item.url !== SPIKE_PDF_URL && item.finalUrl !== SPIKE_PDF_URL) return;
  fixtureDownloads.add(item.id);
  downloadEvents.push({ at: new Date().toISOString(), kind: "created", item });
});
chrome.downloads.onChanged.addListener(delta => {
  if (fixtureDownloads.has(delta.id)) downloadEvents.push({ at: new Date().toISOString(), kind: "changed", delta });
});
const prefix = SPIKE_PREFIX;
const goal = "Acquire the main article PDF. Ignore reference PDFs. Follow article access controls, then download its PDF.";
const socket = new WebSocket(SPIKE_SOCKET);
const log = (value: unknown) => { document.querySelector("pre")!.textContent = JSON.stringify(value, null, 2); };
async function bindingSnapshot() {
  if (!SPIKE_JOB_ID) return null;
  const stored = await (chrome.storage.session ?? chrome.storage.local).get("papio_state_v1");
  const state = stored.papio_state_v1 as { version?: number; activeJobs?: ActiveJob[] } | undefined;
  const job = state?.activeJobs?.find((entry: { job_id?: string }) => entry.job_id === SPIKE_JOB_ID);
  // Inspect only this experiment's record, never serialize other jobs.
  return { version: state?.version ?? null, job: job ?? null };
}
async function boundTab() {
  if (tabID === undefined) throw new Error("Fixture tab not created");
  const tab = await chrome.tabs.get(tabID);
  if (!(tab.url ?? "").startsWith(prefix + "/")) throw new Error("Owned tab left fixture");
  return tab;
}
async function request(method: string, input: any) {
  if (method === "setup") {
    if ((await chrome.management.getSelf()).installType !== "development") throw new Error("Development installation required");
    if (tabID !== undefined) throw new Error("Already created");
    const tab = await chrome.tabs.create({ url: prefix + "/start.html", active: false }); tabID = tab.id!;
    return { tabID, prefix, permissions: await chrome.permissions.getAll(), binding: await bindingSnapshot() };
  }
  if (method === "binding") return bindingSnapshot();
  if (method === "cleanup") {
    if (tabID !== undefined) { await boundTab(); await chrome.tabs.remove(tabID); tabID = undefined; }
    return { cleaned: true };
  }
  if (method === "artifact") {
    if (deliveryStarted && downloadID === undefined) {
      const matches = (await chrome.downloads.search({ url: SPIKE_PDF_URL })).filter(item => item.url === SPIKE_PDF_URL);
      if (matches.length > 1) throw new Error("Ambiguous fixture download");
      downloadID = matches[0]?.id;
    }
    if (downloadID === undefined) return null;
    const [item] = await chrome.downloads.search({ id: downloadID });
    return item ? { id: item.id, state: item.state, filename: item.filename, bytes: item.fileSize, error: item.error ?? null, item, events: downloadEvents } : null;
  }
  await boundTab();
  const target: chrome.scripting.InjectionTarget = method === "act" && documentID ? { tabId: tabID!, documentIds: [documentID] } : { tabId: tabID!, frameIds: [0] };
  const [response] = await chrome.scripting.executeScript({ target, func: pageSpikeDOM, args: [{ method, prefix, goal, ...input }] });
  if (!response?.result) throw new Error("No page result");
  if (method === "observe") documentID = response.documentId;
  const result = response.result;
  if (method === "act" && result.status === "dispatched") {
    if (result.effect === "navigate") await chrome.tabs.update(tabID!, { url: result.url });
    if (result.effect === "download") {
      if (downloadID !== undefined || deliveryStarted) throw new Error("Download already dispatched");
      if (SPIKE_JOB_ID) {
        if (result.url !== SPIKE_PDF_URL) throw new Error("Wrong fixture PDF selected");
        // Delivery validates a live PDF page, its document epoch, and an
        // existing daemon job. Do not fabricate extension job/binding state.
        await chrome.tabs.update(tabID!, { url: result.url });
        const deadline = Date.now() + 8000;
        while ((await boundTab()).status !== "complete") {
          if (Date.now() >= deadline) throw new Error("Fixture PDF navigation timed out");
          await new Promise(resolve => setTimeout(resolve, 100));
        }
        deliveryStarted = true;
        const delivery = await chrome.runtime.sendMessage({ type: "papio.page_spike.delivery", tab_id: tabID });
        if (!delivery?.ok || delivery.job_id !== SPIKE_JOB_ID || !["sending", "downloaded"].includes(delivery.state)) throw new Error(`Fixture delivery refused: ${JSON.stringify(delivery)}`);
        return { ...result, delivery };
      }
      downloadID = await chrome.downloads.download({ url: result.url, filename: input.filename, saveAs: false, conflictAction: "uniquify" });
    }
  }
  return result;
}
socket.addEventListener("open", () => { log({ status: "ready", prefix }); socket.send(JSON.stringify({ ready: true })); });
socket.addEventListener("message", async event => {
  const { id, method, input } = JSON.parse(event.data);
  try { const result = await request(method, input); socket.send(JSON.stringify({ id, result })); log({ method, result }); }
  catch (error) { const message = error instanceof Error ? error.message : String(error); socket.send(JSON.stringify({ id, error: message })); log({ method, error: message }); }
});
