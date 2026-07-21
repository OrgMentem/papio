// Copyright 2026 OrgMentem. Licensed under MIT.

import type { TriageCounts, TriageSnapshotItem, TriageSnapshotResponsePayload } from "./protocol";

type Snapshot = Omit<TriageSnapshotResponsePayload, "request_id">;
type TriageOperation = TriageSnapshotItem["ops"][number];
type Verdict = "accept" | "reject";

interface PageElements {
  connection: HTMLElement;
  counts: HTMLElement;
  refresh: HTMLButtonElement;
  reconnect: HTMLButtonElement;
  list: HTMLElement;
  operationStatus: HTMLElement;
  generatedAt: HTMLTimeElement;
  loadMore: HTMLButtonElement;
  dialog: HTMLElement;
  dialogMessage: HTMLElement;
  dialogCancel: HTMLButtonElement;
  dialogConfirm: HTMLButtonElement;
}

interface Confirmation {
  itemID: string;
  verdict: Verdict;
  returnFocus: HTMLElement | null;
}

interface PageState {
  snapshot: Snapshot | null;
  counts: TriageCounts | null;
  generatedAt: string | null;
  connected: boolean;
  connectionMessage: string;
  selectedID: string | null;
  pending: Set<string>;
  previewed: Set<string>;
  itemMessages: Map<string, string>;
  confirmation: Confirmation | null;
  focusSelectionAfterRender: boolean;
  loading: boolean;
}

const state: PageState = {
  snapshot: null,
  counts: null,
  generatedAt: null,
  connected: false,
  connectionMessage: "Connecting to daemon…",
  selectedID: null,
  pending: new Set(),
  previewed: new Set(),
  itemMessages: new Map(),
  confirmation: null,
  focusSelectionAfterRender: false,
  loading: false,
};

let elements: PageElements | null = null;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function errorFromResponse(value: unknown): string {
  if (isRecord(value) && isRecord(value["error"]) && typeof value["error"]["message"] === "string") {
    return value["error"]["message"];
  }
  return "The daemon did not return a usable response.";
}

function responseValue<T>(value: unknown, key: string): { ok: true; value: T } | { ok: false; message: string } {
  if (isRecord(value) && value["ok"] === true && key in value) {
    return { ok: true, value: value[key] as T };
  }
  return { ok: false, message: errorFromResponse(value) };
}

async function runtimeMessage(type: string, request: Record<string, unknown>): Promise<unknown> {
  if (typeof chrome === "undefined" || !chrome.runtime?.sendMessage) {
    throw new Error("The extension runtime is unavailable.");
  }
  return chrome.runtime.sendMessage({ type, request });
}

function setConnection(connected: boolean, message: string): void {
  state.connected = connected;
  state.connectionMessage = message;
}

function itemForID(id: string): TriageSnapshotItem | null {
  return state.snapshot?.items.find((item) => item.id === id) ?? null;
}

function orderedItems(): TriageSnapshotItem[] {
  if (state.snapshot === null) return [];
  const classRank: Record<TriageSnapshotItem["kind"], number> = {
    retraction: 0,
    human_action: 1,
    watch_hit: 2,
  };
  return [...state.snapshot.items].sort((left, right) => classRank[left.kind] - classRank[right.kind] || left.rank - right.rank);
}

function safeExternalURL(value: string): string | null {
  try {
    const url = new URL(value);
    return url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}

function safePreviewURL(value: string): string | null {
  try {
    const url = new URL(value);
    if (
      url.protocol !== "http:" ||
      url.hostname !== "127.0.0.1" ||
      url.port === "" ||
      !url.pathname.startsWith("/p/") ||
      url.search !== "" ||
      url.hash !== "" ||
      url.username !== "" ||
      url.password !== ""
    ) {
      return null;
    }
    return url.href;
  } catch {
    return null;
  }
}

function firstSafeLink(item: TriageSnapshotItem): string | null {
  for (const link of item.links) {
    const url = safeExternalURL(link.url);
    if (url !== null) return url;
  }
  return null;
}

function openNewTab(url: string): void {
  window.open(url, "_blank", "noopener,noreferrer");
}

function previewToken(item: TriageSnapshotItem): string | null {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity") return null;
  if (typeof item.revision !== "number" || typeof item.sha256 !== "string" || item.sha256.length === 0) return null;
  return `${item.id}:${item.revision}:${item.sha256}`;
}

function hasViewedPreview(item: TriageSnapshotItem): boolean {
  const token = previewToken(item);
  return token !== null && state.previewed.has(token);
}

function hasOperation(item: TriageSnapshotItem, operation: TriageOperation): boolean {
  return item.ops.includes(operation);
}

function isMutation(operation: TriageOperation): boolean {
  return operation === "acquire" || operation === "dismiss" || operation === "accept" || operation === "reject";
}

function operationLabel(operation: TriageOperation): string {
  switch (operation) {
    case "acquire":
      return "Acquire";
    case "dismiss":
      return "Dismiss";
    case "accept":
      return "Accept";
    case "reject":
      return "Reject";
    case "open":
      return "Open";
    case "retry":
      return "Retry";
  }
}

function element<K extends keyof HTMLElementTagNameMap>(tag: K, text?: string): HTMLElementTagNameMap[K] {
  const created = document.createElement(tag);
  if (text !== undefined) created.textContent = text;
  return created;
}

function operationMessage(itemID: string, message: string): void {
  state.itemMessages.set(itemID, message);
  if (elements !== null) elements.operationStatus.textContent = message;
}

function announce(message: string): void {
  if (elements !== null) elements.operationStatus.textContent = message;
}

function rowForItem(itemID: string): HTMLElement | null {
  if (elements === null) return null;
  return Array.from(elements.list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))
    .find((row) => row.dataset.triageItemId === itemID) ?? null;
}

function updateRovingTabIndex(): void {
  if (elements === null) return;
  for (const row of Array.from(elements.list.querySelectorAll<HTMLElement>("[data-triage-item-id]"))) {
    row.tabIndex = row.dataset.triageItemId === state.selectedID ? 0 : -1;
  }
}

function selectItem(itemID: string, focus: boolean): void {
  if (itemForID(itemID) === null) return;
  state.selectedID = itemID;
  updateRovingTabIndex();
  if (focus) rowForItem(itemID)?.focus();
}

function renderLinks(item: TriageSnapshotItem): HTMLElement | null {
  const links = element("p");
  links.className = "item-links";
  let count = 0;
  for (const link of item.links) {
    const url = safeExternalURL(link.url);
    if (url === null) continue;
    if (count > 0) links.append(document.createTextNode(" · "));
    const anchor = element("a", `Open ${link.rel}`);
    anchor.href = url;
    anchor.target = "_blank";
    anchor.rel = "noopener noreferrer";
    links.append(anchor);
    count += 1;
  }
  return count > 0 ? links : null;
}

function previewButton(item: TriageSnapshotItem): HTMLButtonElement | null {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity") return null;
  const button = element("button", "View PDF");
  button.type = "button";
  button.dataset.operation = "preview";
  button.disabled = state.pending.has(item.id) || !state.connected;
  button.addEventListener("click", () => {
    void requestPreview(item);
  });
  return button;
}

function operationButton(item: TriageSnapshotItem, operation: TriageOperation): HTMLButtonElement {
  const button = element("button", operationLabel(operation));
  button.type = "button";
  button.dataset.operation = operation;

  const needsPreview = operation === "accept" && item.action_kind === "verify_identity";
  const unavailable = operation === "retry" || (operation === "open" && firstSafeLink(item) === null);
  button.disabled =
    state.pending.has(item.id) ||
    unavailable ||
    (isMutation(operation) && !state.connected) ||
    (needsPreview && !hasViewedPreview(item));
  if (needsPreview && !hasViewedPreview(item)) button.title = "View the PDF before accepting it.";
  if (operation === "retry") button.title = "Retry is not available from this inbox version.";
  button.addEventListener("click", () => {
    activateOperation(item, operation);
  });
  return button;
}

function renderItem(item: TriageSnapshotItem): HTMLElement {
  const card = element("article");
  card.className = "triage-item";
  card.dataset.triageItemId = item.id;
  card.tabIndex = item.id === state.selectedID ? 0 : -1;
  card.setAttribute("aria-label", item.title);
  card.addEventListener("focusin", () => selectItem(item.id, false));
  card.addEventListener("click", () => selectItem(item.id, false));

  card.append(element("h3", item.title));

  if (item.facts.length > 0) {
    const facts = element("dl");
    facts.className = "item-facts";
    for (const fact of item.facts) {
      facts.append(element("dt", fact.label), element("dd", fact.text));
    }
    card.append(facts);
  }

  const links = renderLinks(item);
  if (links !== null) card.append(links);

  const controls = element("div");
  controls.className = "item-controls";
  controls.setAttribute("aria-label", `Actions for ${item.title}`);
  const preview = previewButton(item);
  if (preview !== null) controls.append(preview);
  for (const operation of item.ops) controls.append(operationButton(item, operation));
  if (controls.childElementCount > 0) card.append(controls);

  const message = state.itemMessages.get(item.id);
  if (message !== undefined) {
    const result = element("p", message);
    result.className = "item-result";
    result.setAttribute("role", "status");
    card.append(result);
  }
  return card;
}

function renderGroup(kind: TriageSnapshotItem["kind"], heading: string, items: TriageSnapshotItem[]): HTMLElement | null {
  if (items.length === 0) return null;
  const section = element("section");
  section.className = `triage-group triage-group-${kind}`;
  section.append(element("h2", `${heading} (${items.length})`));
  for (const item of items) section.append(renderItem(item));
  return section;
}

function renderCounts(): void {
  if (elements === null) return;
  const counts = state.counts ?? state.snapshot?.counts;
  if (counts === undefined || counts === null) {
    elements.counts.textContent = "Counts unavailable";
    return;
  }
  elements.counts.textContent = `${counts.pending_total} pending · ${counts.retractions} retractions · ${counts.actions} human actions · ${counts.watch_hits} watch hits`;
}

function renderDialog(): void {
  if (elements === null) return;
  const confirmation = state.confirmation;
  if (confirmation === null) {
    elements.dialog.hidden = true;
    elements.dialogConfirm.disabled = false;
    return;
  }
  const item = itemForID(confirmation.itemID);
  if (item === null) {
    closeDialog(false);
    return;
  }
  if (confirmation.verdict === "accept" && item.action_kind === "verify_identity") {
    elements.dialogMessage.textContent = `Accept this PDF as ${item.title}? It leaves quarantine.`;
  } else if (confirmation.verdict === "reject") {
    elements.dialogMessage.textContent = `Reject ${item.title}? It cancels the job.`;
  } else {
    elements.dialogMessage.textContent = `Accept ${item.title}?`;
  }
  elements.dialogConfirm.textContent = confirmation.verdict === "accept" ? "Accept" : "Reject";
  elements.dialogConfirm.disabled = state.pending.has(item.id);
  elements.dialogCancel.disabled = state.pending.has(item.id);
  elements.dialog.hidden = false;
}

function render(): void {
  if (elements === null) return;
  const isDisconnected = !state.connected;
  elements.connection.textContent = isDisconnected
    ? `Disconnected: ${state.connectionMessage}. Run papio status for details.`
    : state.connectionMessage;
  elements.connection.dataset.state = isDisconnected ? "disconnected" : "connected";
  elements.reconnect.hidden = !isDisconnected;
  elements.refresh.disabled = state.loading;
  elements.reconnect.disabled = state.loading;
  renderCounts();

  elements.list.replaceChildren();
  const items = orderedItems();
  if (state.selectedID !== null && !items.some((item) => item.id === state.selectedID)) state.selectedID = items[0]?.id ?? null;
  if (state.selectedID === null && items.length > 0) state.selectedID = items[0]?.id ?? null;

  const retractions = items.filter((item) => item.kind === "retraction");
  const actions = items.filter((item) => item.kind === "human_action");
  const hits = items.filter((item) => item.kind === "watch_hit");
  const groups = [
    renderGroup("retraction", "Retractions", retractions),
    renderGroup("human_action", "Human actions", actions),
    renderGroup("watch_hit", "Watch hits", hits),
  ];
  for (const group of groups) if (group !== null) elements.list.append(group);

  if (state.snapshot === null) {
    elements.list.append(element("p", "No snapshot is available yet. Reconnect to retrieve the inbox."));
  } else if (items.length === 0) {
    elements.list.append(element("p", "Your inbox is clear."));
  }
  if (state.snapshot?.unsupported_items_count && state.snapshot.unsupported_items_count > 0) {
    elements.list.append(element("p", `${state.snapshot.unsupported_items_count} newer item(s) need a newer extension.`));
  }

  elements.loadMore.hidden = state.snapshot?.has_more !== true;
  elements.loadMore.disabled = state.loading || !state.connected;
  if (state.generatedAt === null) {
    elements.generatedAt.textContent = "generated at —";
    elements.generatedAt.removeAttribute("datetime");
  } else {
    elements.generatedAt.dateTime = state.generatedAt;
    elements.generatedAt.textContent = `generated at ${state.generatedAt}`;
  }

  renderDialog();
  if (state.focusSelectionAfterRender) {
    state.focusSelectionAfterRender = false;
    if (state.selectedID !== null) rowForItem(state.selectedID)?.focus();
  }
}

function decrementCounts(item: TriageSnapshotItem): void {
  const counts = state.counts;
  if (counts === null) return;
  const decrease = (value: number): number => Math.max(0, value - 1);
  state.counts = {
    ...counts,
    pending_total: decrease(counts.pending_total),
    watch_hits: item.kind === "watch_hit" ? decrease(counts.watch_hits) : counts.watch_hits,
    actions: item.kind === "human_action" ? decrease(counts.actions) : counts.actions,
    retractions: item.kind === "retraction" ? decrease(counts.retractions) : counts.retractions,
  };
}

function removeItem(itemID: string): void {
  if (state.snapshot === null) return;
  const items = orderedItems();
  const index = items.findIndex((item) => item.id === itemID);
  const removed = items[index];
  if (removed === undefined) return;
  const remaining = state.snapshot.items.filter((item) => item.id !== itemID);
  state.snapshot = { ...state.snapshot, items: remaining };
  state.itemMessages.delete(itemID);
  decrementCounts(removed);
  if (state.selectedID === itemID) {
    const next = items[index + 1] ?? items[index - 1] ?? null;
    state.selectedID = next?.id ?? null;
    state.focusSelectionAfterRender = true;
  }
}

function resultForMutation(value: unknown): { ok: true; outcome: "applied" | "already_applied" | "conflict" | "error"; detail?: string } | { ok: false; message: string } {
  if (!isRecord(value) || value["ok"] !== true || typeof value["outcome"] !== "string") {
    return { ok: false, message: errorFromResponse(value) };
  }
  const outcome = value["outcome"];
  if (outcome !== "applied" && outcome !== "already_applied" && outcome !== "conflict" && outcome !== "error") {
    return { ok: false, message: "The daemon returned an unknown mutation result." };
  }
  const detail = typeof value["detail"] === "string" ? value["detail"] : undefined;
  return detail === undefined ? { ok: true, outcome } : { ok: true, outcome, detail };
}

async function refreshInbox(append = false): Promise<void> {
  const cursor = append ? state.snapshot?.cursor : undefined;
  if (append && (cursor === undefined || state.snapshot === null)) return;
  state.loading = true;
  render();
  const snapshotRequest: Record<string, unknown> = { schema_versions: [1] };
  if (cursor !== undefined) snapshotRequest["cursor"] = cursor;
  const snapshotPromise = runtimeMessage("papio.triage.snapshot", snapshotRequest)
    .then((response) => responseValue<Snapshot>(response, "snapshot"))
    .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const countsPromise = append
    ? Promise.resolve({ ok: false as const, message: "Counts were not refreshed." })
    : runtimeMessage("papio.triage.counts", {})
      .then((response) => responseValue<TriageCounts>(response, "counts"))
      .catch((error: unknown) => ({ ok: false as const, message: error instanceof Error ? error.message : "The daemon is unavailable." }));
  const [snapshotResult, countsResult] = await Promise.all([snapshotPromise, countsPromise]);
  state.loading = false;

  if (snapshotResult.ok) {
    const snapshot = snapshotResult.value;
    state.snapshot = append && state.snapshot !== null
      ? { ...snapshot, items: [...state.snapshot.items, ...snapshot.items] }
      : snapshot;
    state.counts = snapshot.counts;
    state.generatedAt = snapshot.generated_at;
    setConnection(true, "Connected to daemon.");
  } else {
    setConnection(false, snapshotResult.message);
  }
  if (countsResult.ok) state.counts = countsResult.value;
  render();
}

function requestRefresh(): void {
  void refreshInbox();
}

function beginMutation(item: TriageSnapshotItem): boolean {
  if (!state.connected) {
    operationMessage(item.id, "Daemon unavailable. Reconnect before changing this item.");
    render();
    return false;
  }
  if (state.pending.has(item.id)) return false;
  state.pending.add(item.id);
  render();
  return true;
}

async function finishMutation(item: TriageSnapshotItem, response: unknown): Promise<void> {
  const result = resultForMutation(response);
  state.pending.delete(item.id);
  if (!result.ok) {
    setConnection(false, result.message);
    operationMessage(item.id, result.message);
    render();
    return;
  }
  switch (result.outcome) {
    case "applied":
    case "already_applied":
      announce(result.outcome === "applied" ? "Change applied." : "Change was already applied.");
      removeItem(item.id);
      render();
      return;
    case "conflict":
      operationMessage(item.id, "changed elsewhere — refreshed");
      render();
      await refreshInbox();
      return;
    case "error":
      operationMessage(item.id, result.detail ?? "The daemon could not apply this change.");
      render();
      return;
  }
}

async function decide(item: TriageSnapshotItem, operation: "acquire" | "dismiss"): Promise<void> {
  if (!beginMutation(item)) return;
  try {
    const response = await runtimeMessage("papio.triage.decide", {
      item_id: item.id,
      op: operation,
      ...(operation === "dismiss" ? { watch_scope: "all" } : {}),
    });
    await finishMutation(item, response);
  } catch (error) {
    await finishMutation(item, { ok: false, error: { message: error instanceof Error ? error.message : "The daemon is unavailable." } });
  }
}

function closeDialog(restoreFocus: boolean): void {
  if (elements === null) return;
  const confirmation = state.confirmation;
  state.confirmation = null;
  elements.dialog.hidden = true;
  if (restoreFocus) confirmation?.returnFocus?.focus();
}

function requestConfirmation(item: TriageSnapshotItem, verdict: Verdict): void {
  if (item.kind !== "human_action" || typeof item.action_id !== "number" || typeof item.revision !== "number") {
    operationMessage(item.id, "This action is missing its revision and cannot be changed.");
    render();
    return;
  }
  if (verdict === "accept" && item.action_kind === "verify_identity" && !hasViewedPreview(item)) {
    operationMessage(item.id, "View the PDF before accepting it.");
    render();
    return;
  }
  state.confirmation = {
    itemID: item.id,
    verdict,
    returnFocus: document.activeElement instanceof HTMLElement ? document.activeElement : null,
  };
  renderDialog();
  elements?.dialogCancel.focus();
}

async function resolveConfirmation(): Promise<void> {
  const confirmation = state.confirmation;
  if (confirmation === null) return;
  const item = itemForID(confirmation.itemID);
  if (
    item === null ||
    item.kind !== "human_action" ||
    typeof item.action_id !== "number" ||
    typeof item.revision !== "number"
  ) {
    closeDialog(true);
    return;
  }
  if (confirmation.verdict === "accept" && item.action_kind === "verify_identity" && !hasViewedPreview(item)) {
    operationMessage(item.id, "View the PDF before accepting it.");
    closeDialog(true);
    render();
    return;
  }
  if (confirmation.verdict === "accept" && (typeof item.sha256 !== "string" || item.sha256.length === 0)) {
    operationMessage(item.id, "This PDF is missing its snapshot hash and cannot be accepted.");
    closeDialog(true);
    render();
    return;
  }
  if (!beginMutation(item)) return;
  renderDialog();
  try {
    const response = await runtimeMessage("papio.action.resolve", {
      action_id: item.action_id,
      verdict: confirmation.verdict,
      expected_revision: item.revision,
      ...(confirmation.verdict === "accept" ? { expected_sha256: item.sha256 } : {}),
    });
    closeDialog(false);
    await finishMutation(item, response);
  } catch (error) {
    closeDialog(false);
    await finishMutation(item, { ok: false, error: { message: error instanceof Error ? error.message : "The daemon is unavailable." } });
  }
}

async function requestPreview(item: TriageSnapshotItem): Promise<void> {
  if (item.kind !== "human_action" || item.action_kind !== "verify_identity" || typeof item.action_id !== "number") return;
  if (!beginMutation(item)) return;
  try {
    const response = await runtimeMessage("papio.preview", { action_id: item.action_id });
    const result = responseValue<{ url: string; sha256: string }>(response, "preview");
    state.pending.delete(item.id);
    if (!result.ok) {
      setConnection(false, result.message);
      operationMessage(item.id, result.message);
      render();
      return;
    }
    const url = safePreviewURL(result.value.url);
    if (url === null || result.value.sha256 !== item.sha256) {
      operationMessage(item.id, "Preview did not match this snapshot — refreshed.");
      render();
      await refreshInbox();
      return;
    }
    const token = previewToken(item);
    if (token === null) {
      operationMessage(item.id, "This PDF is missing a verifiable snapshot hash.");
      render();
      return;
    }
    state.previewed.add(token);
    openNewTab(url);
    operationMessage(item.id, "PDF opened. Accept is now available.");
    render();
  } catch (error) {
    state.pending.delete(item.id);
    const message = error instanceof Error ? error.message : "The daemon is unavailable.";
    setConnection(false, message);
    operationMessage(item.id, message);
    render();
  }
}

function activateOperation(item: TriageSnapshotItem, operation: TriageOperation): void {
  switch (operation) {
    case "acquire":
    case "dismiss":
      void decide(item, operation);
      return;
    case "accept":
    case "reject":
      requestConfirmation(item, operation);
      return;
    case "open": {
      const url = firstSafeLink(item);
      if (url === null) {
        operationMessage(item.id, "This item has no safe link to open.");
        render();
      } else {
        openNewTab(url);
        announce("Opened the first link in a new tab.");
      }
      return;
    }
    case "retry":
      operationMessage(item.id, "Retry is not available from this inbox version.");
      render();
      return;
  }
}

function activatePrimary(item: TriageSnapshotItem): void {
  if (item.kind === "watch_hit" && hasOperation(item, "acquire")) {
    void decide(item, "acquire");
    return;
  }
  if (item.kind === "human_action" && hasOperation(item, "accept")) {
    requestConfirmation(item, "accept");
    return;
  }
  if (hasOperation(item, "open")) activateOperation(item, "open");
}

function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.getAttribute("contenteditable") === "true";
}

function handleKeyboard(event: KeyboardEvent): void {
  if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
  if (state.confirmation !== null || isTypingTarget(event.target)) return;
  const items = orderedItems();
  const current = state.selectedID === null ? -1 : items.findIndex((item) => item.id === state.selectedID);
  switch (event.key) {
    case "j":
      if (current >= 0 && current < items.length - 1) {
        event.preventDefault();
        selectItem(items[current + 1]!.id, true);
      }
      return;
    case "k":
      if (current > 0) {
        event.preventDefault();
        selectItem(items[current - 1]!.id, true);
      }
      return;
    case "a":
      if (current >= 0) {
        event.preventDefault();
        activatePrimary(items[current]!);
      }
      return;
    case "d":
      if (current >= 0 && hasOperation(items[current]!, "dismiss")) {
        event.preventDefault();
        void decide(items[current]!, "dismiss");
      }
      return;
    case "o":
      if (current >= 0) {
        event.preventDefault();
        const url = firstSafeLink(items[current]!);
        if (url !== null) {
          openNewTab(url);
          announce("Opened the first link in a new tab.");
        }
      }
      return;
    default:
      return;
  }
}

function trapDialogFocus(event: KeyboardEvent): void {
  if (state.confirmation === null || elements === null) return;
  if (event.key === "Escape") {
    event.preventDefault();
    closeDialog(true);
    return;
  }
  if (event.key !== "Tab") return;
  const focusable = [elements.dialogCancel, elements.dialogConfirm].filter((button) => !button.disabled);
  if (focusable.length === 0) {
    event.preventDefault();
    return;
  }
  const first = focusable[0]!;
  const last = focusable[focusable.length - 1]!;
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

function bootstrap(): void {
  const connection = document.getElementById("connection-status");
  const counts = document.getElementById("inbox-counts");
  const refresh = document.getElementById("refresh-inbox");
  const reconnect = document.getElementById("reconnect-daemon");
  const list = document.getElementById("item-list");
  const operationStatus = document.getElementById("operation-status");
  const generatedAt = document.getElementById("generated-at");
  const loadMore = document.getElementById("load-more");
  const dialog = document.getElementById("confirm-dialog");
  const dialogMessage = document.getElementById("confirm-dialog-message");
  const dialogCancel = document.getElementById("confirm-cancel");
  const dialogConfirm = document.getElementById("confirm-submit");
  if (
    !(connection instanceof HTMLElement) ||
    !(counts instanceof HTMLElement) ||
    !(refresh instanceof HTMLButtonElement) ||
    !(reconnect instanceof HTMLButtonElement) ||
    !(list instanceof HTMLElement) ||
    !(operationStatus instanceof HTMLElement) ||
    !(generatedAt instanceof HTMLTimeElement) ||
    !(loadMore instanceof HTMLButtonElement) ||
    !(dialog instanceof HTMLElement) ||
    !(dialogMessage instanceof HTMLElement) ||
    !(dialogCancel instanceof HTMLButtonElement) ||
    !(dialogConfirm instanceof HTMLButtonElement)
  ) {
    return;
  }
  elements = {
    connection,
    counts,
    refresh,
    reconnect,
    list,
    operationStatus,
    generatedAt,
    loadMore,
    dialog,
    dialogMessage,
    dialogCancel,
    dialogConfirm,
  };
  refresh.addEventListener("click", requestRefresh);
  reconnect.addEventListener("click", requestRefresh);
  loadMore.addEventListener("click", () => {
    void refreshInbox(true);
  });
  dialogCancel.addEventListener("click", () => closeDialog(true));
  dialogConfirm.addEventListener("click", () => {
    void resolveConfirmation();
  });
  document.addEventListener("keydown", handleKeyboard);
  document.addEventListener("keydown", trapDialogFocus);
  render();
  void refreshInbox();
}

if (typeof document !== "undefined") {
  if (document.getElementById("item-list") !== null) bootstrap();
  else document.addEventListener("DOMContentLoaded", bootstrap, { once: true });
}
