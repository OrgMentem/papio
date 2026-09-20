// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Built only by the explicit development runner, never by build.ts.
import { providerSpikeDOM } from "./provider-spike-dom";
import type { ActiveJob } from "../src/state";
declare const PROVIDER_SPIKE: { socket: string; jobID: string; entryURL: string; doi: string; afterDrift: boolean };
const config = PROVIDER_SPIKE, entry = new URL(config.entryURL);
const socket = new WebSocket(config.socket);
const goal = `Acquire the main article PDF for DOI ${config.doi}. Ignore references, supplements and citation exports.`;
let tabID: number | undefined, documentID: string | undefined, startedAfter = new Date().toISOString();
let recoveryTab = false;
const events: unknown[] = [], observedIDs = new Set<number>();
const log = (value: unknown) => { document.querySelector("pre")!.textContent = JSON.stringify(value, null, 2); };
const scoped = (url: string | undefined) => {
  if (!url) return false;
  const value = new URL(url);
  return value.origin === entry.origin && value.pathname === entry.pathname;
};
async function binding() {
  const stored = await (chrome.storage.session ?? chrome.storage.local).get("papio_state_v1");
  const state = stored.papio_state_v1 as { activeJobs?: ActiveJob[] } | undefined;
  return state?.activeJobs?.find(job => job.job_id === config.jobID);
}
async function boundTab() {
  if (tabID === undefined) throw new Error("Provider tab not selected");
  const job = await binding(), tab = await chrome.tabs.get(tabID);
  if (!job || !scoped(tab.url) || (recoveryTab ? !manualWindow(job) : job.tab_id !== tabID)) throw new Error("Provider job/tab binding changed");
  return tab;
}
const manualWindow = (job: ActiveJob) => job.tab_id < 0 && job.status === "awaiting_download" && job.access_mode === undefined &&
  !job.manual_delivery_required && job.expected?.doi?.toLowerCase() === config.doi.toLowerCase() && job.provider_hosts.includes(entry.hostname);
chrome.downloads.onCreated.addListener(item => {
  if (tabID === undefined || Date.parse(item.startTime) < Date.parse(startedAfter)) return;
  const fromProvider = [item.url, item.finalUrl, item.referrer].some(value => {
    try { return new URL(value).origin === entry.origin; } catch { return false; }
  });
  if (!fromProvider) return;
  observedIDs.add(item.id); events.push({ kind: "created", at: new Date().toISOString(), item });
});
chrome.downloads.onChanged.addListener(delta => {
  if (observedIDs.has(delta.id)) events.push({ kind: "changed", at: new Date().toISOString(), delta });
});
async function request(method: string, input: { choice?: string; revision?: string }) {
  if (method === "setup") {
    if ((await chrome.management.getSelf()).installType !== "development") throw new Error("Development installation required");
    if (tabID !== undefined) throw new Error("Already set up");
    const job = await binding();
    if (!job || job.download_initiated || job.expected?.doi?.toLowerCase() !== config.doi.toLowerCase()) throw new Error("No fresh provider binding for the configured job and DOI");
    // Attribute this explicitly authorized experiment either to an assisted job
    // or a recorded prior adapter failure. Neither changes production policy.
    if (job.access_mode !== "assisted" && !config.afterDrift) throw new Error("Assisted job or recorded drift required for attribution");
    if (config.afterDrift && manualWindow(job)) {
      // The existing bridge deliberately closes failed provider tabs and retains
      // a manual-download correlation window. Own a new experimental surface;
      // never fabricate or edit an ActiveJob, offer, permit or daemon binding.
      recoveryTab = true;
      tabID = (await chrome.tabs.create({ url: entry.href, active: false })).id;
      const deadline = Date.now() + 15000;
      while ((await chrome.tabs.get(tabID!)).status !== "complete") {
        if (Date.now() > deadline) throw new Error("Recovery article did not finish loading");
        await new Promise(resolve => setTimeout(resolve, 100));
      }
    } else {
      if (job.tab_id < 0) throw new Error("No bound provider tab");
      tabID = job.tab_id;
    }
    await boundTab(); startedAfter = new Date().toISOString();
    return { tabID, job, startedAfter, recoveryTab };
  }
  if (method === "artifact") {
    const downloads = await chrome.downloads.search({ startedAfter });
    const matches = downloads.filter(item => item.filename.replaceAll("\\", "/").includes(`/papio/${config.jobID}/`));
    if (matches.length > 1) throw new Error("Multiple job downloads; inspect before continuing");
    // Chrome's onCreated event precedes filename assignment. Such a transfer
    // proves only that cleanup should wait, never that this job succeeded.
    const pending = downloads.some(item => observedIDs.has(item.id) && item.state === "in_progress");
    return { item: matches[0] ?? null, pending, events };
  }
  if (method === "cleanup") {
    if (tabID === undefined) return { closed: false };
    // Closing a managed tab cancels its job. Let the real bridge finish adoption
    // and retire the binding before removing our tab, including failed trials.
    if (!recoveryTab && await binding()) return { closed: false, reason: "Job remains bound; retained until daemon adoption settles" };
    if (recoveryTab) {
      const transfer = await request("artifact", {});
      if (transfer.pending || transfer.item?.state === "in_progress") return { closed: false, reason: "Download in progress; retained" };
    }
    let tab: chrome.tabs.Tab;
    try { tab = await chrome.tabs.get(tabID); } catch { return { closed: true, alreadyGone: true }; }
    if (!scoped(tab.url)) return { closed: false, reason: "Owned tab changed; retained for operator" };
    await chrome.tabs.remove(tabID); return { closed: true };
  }
  await boundTab();
  const target: chrome.scripting.InjectionTarget = method === "act" && documentID ? { tabId: tabID!, documentIds: [documentID] } : { tabId: tabID!, frameIds: [0] };
  const [response] = await chrome.scripting.executeScript({ target, func: providerSpikeDOM, args: [{ method: method as "observe" | "act", entryURL: config.entryURL, doi: config.doi, goal, ...input }] });
  if (!response?.result) throw new Error("Provider returned no observation");
  if (method === "observe") documentID = response.documentId;
  return response.result;
}
socket.addEventListener("open", () => { log({ ready: true, jobID: config.jobID }); socket.send(JSON.stringify({ ready: true })); });
socket.addEventListener("message", async event => {
  const { id, method, input } = JSON.parse(event.data);
  try { const result = await request(method, input); socket.send(JSON.stringify({ id, result })); log({ method, result }); }
  catch (error) { const message = error instanceof Error ? error.message : String(error); socket.send(JSON.stringify({ id, error: message })); log({ method, error: message }); }
});
