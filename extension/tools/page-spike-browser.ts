// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Packaged only by the explicit development runner, never by build.ts.
import { pageSpikeDOM } from "./page-spike-dom";
declare const SPIKE_SOCKET: string;
declare const SPIKE_PREFIX: string;
let tabID: number | undefined, documentID: string | undefined, downloadID: number | undefined;
const prefix = SPIKE_PREFIX;
const goal = "Acquire the main article PDF. Ignore reference PDFs. Follow article access controls, then download its PDF.";
const socket = new WebSocket(SPIKE_SOCKET);
const log = (value: unknown) => { document.querySelector("pre")!.textContent = JSON.stringify(value, null, 2); };
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
    return { tabID, prefix, permissions: await chrome.permissions.getAll() };
  }
  if (method === "cleanup") {
    if (tabID !== undefined) { await boundTab(); await chrome.tabs.remove(tabID); tabID = undefined; }
    return { cleaned: true };
  }
  if (method === "artifact") {
    if (downloadID === undefined) return null;
    const [item] = await chrome.downloads.search({ id: downloadID });
    return item ? { id: item.id, state: item.state, filename: item.filename, bytes: item.fileSize, error: item.error ?? null } : null;
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
      if (downloadID !== undefined) throw new Error("Download already dispatched");
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
