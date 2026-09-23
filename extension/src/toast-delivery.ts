// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Toast delivery for the papio bridge: ADR-0023's seventh surface. One
// pending toast at a time, raised in the researcher's own page when the
// in-page route is allowed and in a small papio window otherwise, plus the
// filed-paper reopen batch. Deciding WHAT was lost stays with Bridge; this
// module decides how the offer is shown and answers the researcher's reply.
//
// Everything the module needs from Bridge is named in ToastDeliveryContext;
// nothing here imports background.ts.

import type { TabInfo, WindowInfo } from "./browser-types";
import { isPDFPage } from "./deliver";
import { HISTORY_PAGE_PATH, TOAST_PAGE_PATH } from "./page-paths";
import { ALL_SITES_ORIGIN, findByTab, type StoreShape } from "./state";
import type { SurfaceBirthRecord } from "./ledger";
import {
  PAPIO_MARK,
  PAPIO_MARK_SIZE_PX,
  PAPIO_MARK_VIEWBOX,
  TOAST_PAGE_ACTION_MESSAGE,
  TOAST_PAGE_DISMISS_MESSAGE,
  TOAST_WINDOW_MS,
  TOAST_WINDOW_SIZE,
  type ToastInjection,
  type ToastPayload,
  toastCopy,
} from "./toast-view";

/** The reopen toast waits this long after the last close of a batch, so a
 * reconcile pass that closes several filed papers raises one toast. */
const FILED_TOAST_SETTLE_MS = 1_500;
/** How many filed jobs a worker remembers for the reopen toast. */
const FILED_JOB_MEMORY = 64;
/** Prefix of a reopen toast's batch id. It lets Reopen fall back to the
 * history page after the worker slept and forgot the batch itself. */
const FILED_TOAST_PREFIX = "filed:";

/**
 * ADR-0023's seventh surface, delivered into the page the researcher is reading
 * instead of into a small papio window. Runs INSIDE that page via
 * scripting.executeScript, so — like `isBotChallenge` in background.ts — it
 * must stay fully self-contained: no outer-scope reference, module import, or
 * shared constant survives serialization, which is why every value it needs
 * arrives in the one `ToastInjection` argument.
 *
 * Three differences from the sixth surface's chip (`popup.ts`
 * renderInPageAcknowledgement), all of them deliberate:
 *
 * - It is INTERACTIVE. The chip is `pointer-events: none` because it is a
 *   receipt; this one carries the single take-back-control action, so it takes
 *   pointer and keyboard input and is an `alertdialog` like the toast page.
 * - It reads NOTHING from the page. It appends one host element and removes it.
 *   No selector runs, no text is collected, and nothing is returned except
 *   whether the host was appended.
 * - It removes itself from the DOM before it reports, so a capture taken after
 *   an action can never contain it. The host id is also what the capture path
 *   strips, so a toast that is still live is excluded from fixture bytes.
 */
export function renderPageToast(injection: ToastInjection): boolean {
  const HOST_ID = "papio-extension-loss-toast-v1";
  const prior = document.getElementById(HOST_ID);
  if (prior !== null) prior.remove();
  const host = document.createElement("div");
  host.id = HOST_ID;
  host.style.cssText = [
    "position:fixed",
    "right:16px",
    "bottom:16px",
    "z-index:2147483647",
    "margin:0",
    "padding:0",
    "border:0",
  ].join(";");
  // Open, not closed: same reason as the chip — isolation is what the shadow
  // root is for, and an open root stays inspectable without weakening it. It
  // also keeps the copy out of `document.documentElement.outerHTML`, which
  // omits shadow roots, so a capture that races the strip still carries an
  // empty host rather than papio's sentence.
  const root = host.attachShadow({ mode: "open" });
  const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  // The card's own palette, plus papio's brand pair for the mark. The brand
  // colours are the same literals every papio page sets as `--color-brand-*`;
  // they cannot be read as custom properties here, because the document these
  // styles land in is the publisher's, not papio's.
  const [ink, border, surface, accentInk, accent, brandInk, brandAccent] = dark
    ? ["#e9ecf1", "#3a4049", "#1c1f26", "#10131a", "#6f9cf0", "#f0edf3", "#ef6a57"]
    : ["#16181d", "#d3d7de", "#ffffff", "#ffffff", "#1c5fd6", "#2b2d42", "#d94f3d"];
  const card = document.createElement("div");
  card.setAttribute("role", "alertdialog");
  card.setAttribute("aria-describedby", "papio-toast-message");
  card.style.cssText = [
    "align-items:center",
    `background:${surface}`,
    `border:1px solid ${border}`,
    "border-radius:10px",
    // `toast.html`'s body is border-box, so the shared width means the box
    // INCLUDING padding and border there. Without this the same constant sizes
    // the content box here, and the injected card renders 30px wider than the
    // window it is supposed to match — measured at 606 against 576.
    "box-sizing:border-box",
    "box-shadow:0 10px 30px rgb(16 22 33 / 22%)",
    `color:${ink}`,
    "display:flex",
    "font:14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif",
    "gap:12px",
    `max-width:min(${injection.max_width_px}px, calc(100vw - 32px))`,
    "padding:12px 14px",
  ].join(";");
  // The same loop `renderPapioMark` runs, over the same geometry, inline because
  // a function reference cannot cross this serialization boundary. Decorative:
  // `aria-hidden`, no title, no label — the sentence beside it names papio.
  const SVG_NS = "http://www.w3.org/2000/svg";
  const mark = document.createElementNS(SVG_NS, "svg");
  mark.setAttribute("viewBox", injection.mark_viewbox);
  mark.setAttribute("aria-hidden", "true");
  mark.style.cssText = `flex:none;width:${injection.mark_size_px}px;height:${injection.mark_size_px}px`;
  for (const shape of injection.mark) {
    const el = document.createElementNS(SVG_NS, shape.tag);
    for (const [name, value] of Object.entries(shape.attrs)) el.setAttribute(name, value);
    const colour = shape.role === "ink" ? brandInk : brandAccent;
    el.setAttribute("stroke", colour);
    el.setAttribute("fill", shape.filled === true ? colour : "none");
    mark.append(el);
  }
  const message = document.createElement("p");
  message.id = "papio-toast-message";
  message.textContent = injection.message;
  message.style.cssText = "margin:0;flex:1 1 auto";
  const action = document.createElement("button");
  action.type = "button";
  action.textContent = injection.action;
  action.style.cssText = [
    "flex:none",
    `background:${accent}`,
    `color:${accentInk}`,
    "border:1px solid transparent",
    "border-radius:7px",
    "cursor:pointer",
    "font:inherit",
    "padding:6px 12px",
  ].join(";");
  const dismiss = document.createElement("button");
  dismiss.type = "button";
  dismiss.textContent = "Dismiss";
  dismiss.setAttribute("aria-label", "Dismiss this message");
  dismiss.style.cssText = [
    "flex:none",
    "background:transparent",
    `color:${ink}`,
    `border:1px solid ${border}`,
    "border-radius:7px",
    "cursor:pointer",
    "font:inherit",
    "padding:6px 10px",
  ].join(";");
  // One settlement only. Both buttons and the expiry race each other, and a
  // second report would either reopen a paper twice or dismiss an offer the
  // researcher just accepted. `timer` is declared before `settle` reads it:
  // the handlers only fire after the assignment below, but a const declared
  // after its closure is a compile error, not a runtime one.
  let settled = false;
  let timer: number | undefined;
  const settle = (type: string, reason?: string): void => {
    if (settled) return;
    settled = true;
    window.clearTimeout(timer);
    // Remove BEFORE reporting: the action opens a tab, and this page may be
    // captured or classified at any moment after that.
    host.remove();
    const payload: Record<string, unknown> = {
      type,
      job_id: injection.job_id,
      token: injection.token,
    };
    if (reason !== undefined) payload["reason"] = reason;
    void chrome.runtime.sendMessage(payload).catch(() => undefined);
  };
  action.addEventListener("click", () => settle(injection.action_message));
  dismiss.addEventListener("click", () =>
    settle(injection.dismiss_message, "dismissed"),
  );
  // Expiry commits nothing, exactly as the window route's does: the recovery
  // stays in the inbox, so the eight seconds are a shortcut, not a deadline.
  timer = window.setTimeout(
    () => settle(injection.dismiss_message, "expired"),
    injection.window_ms,
  );
  card.append(mark, message, action, dismiss);
  root.append(card);
  document.documentElement.append(host);
  return true;
}

/** The browser seams toast delivery uses: a structural subset of BridgeDeps.
 * Held by reference and read at call time, like every Bridge seam. */
export interface ToastDeliveryDeps {
  randomUUID(): string;
  setTimeout(fn: () => void | Promise<void>, ms: number): void;
  runtimeGetURL?: (path: string) => string;
  tabs: {
    create(props: { url: string; active: boolean }): Promise<TabInfo>;
    query?(query: {
      active?: boolean;
      lastFocusedWindow?: boolean;
    }): Promise<TabInfo[]>;
  };
  windows?: {
    create(props: {
      url: string;
      focused: boolean;
      type?: "popup";
      width?: number;
      height?: number;
    }): Promise<WindowInfo>;
    get(windowID: number): Promise<WindowInfo>;
    update(windowID: number, props: { focused?: boolean }): Promise<unknown>;
    remove(windowID: number): Promise<unknown>;
  };
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      func: (...args: never[]) => unknown;
      args?: unknown[];
    }): Promise<{ result?: unknown }[]>;
  };
  settings: {
    getInPageToast(): Promise<boolean>;
  };
  permissions: {
    contains(perm: { origins: string[] }): Promise<boolean>;
  };
}

/** The Bridge state and behaviour toast delivery depends on. */
export interface ToastDeliveryContext {
  /** The current managed-state snapshot. */
  store(): StoreShape;
  /** The owned-surface ledger cache, when loaded. */
  tabLedgerCache(): Record<string, SurfaceBirthRecord> | undefined;
  /** Decision 9's presence hint: a papio surface is in front right now. */
  papioSurfaceLikelyFocused(): boolean;
  /** Open a tab where papio's own tabs go, per the handoff-surface setting. */
  openBrokerTab(url: string, surfaceFallback: boolean): Promise<number | undefined>;
  /** The fresh route `papio actions open` mints for a job. */
  requestFreshHandoffLink(
    jobID: string,
  ): Promise<{ ok: true; url: string } | { ok: false }>;
}

export class ToastDelivery {
  /** The one pending toast. Bound 1 of the plan lives here rather than in the
   * page: a second loss replaces the first, so the surface can never become a
   * stack of windows. Worker memory is the right home despite MV3 suspension —
   * a toast that did not survive the worker sleeping was an offer nobody was
   * looking at, and the durable record of the loss is the Activity row the
   * daemon already writes. */
  private pendingToast: ToastPayload | undefined;
  private toastWindowID: number | undefined;
  /** Jobs papio filed in this worker lifetime, so a later close of any of
   * their surfaces (a tab deferred while the operator looked at it, the page
   * that led to the viewer) is offered back too. Insertion-ordered and
   * bounded to FILED_JOB_MEMORY; a worker that slept simply forgets. */
  readonly filedJobs = new Set<string>();
  /** Papers whose tabs papio closed after filing them, gathered into one
   * reopen toast. Worker memory only: the URLs are route URLs and are never
   * persisted, so a worker that slept has lost them and Reopen falls back to
   * the history page. `revision` debounces the raise until the batch has
   * been quiet for FILED_TOAST_SETTLE_MS. */
  private filedBatch:
    | { readonly id: string; readonly urls: Map<string, string>; revision: number }
    | undefined;
  /** The in-page route's one-use authorization. Present only while an injected
   * toast is live, and cleared by the first action, dismissal, or replacement.
   *
   * It exists because the injected toast's sender is the researcher's own page,
   * so `sender.url` cannot authorize it the way it authorizes the toast page.
   * The token is what makes the reply papio's own: a page cannot read it (the
   * isolated world is not reachable from page script), and a stale injected
   * toast left on a background tab cannot act after a replacement. */
  private pendingPageToast:
    | { readonly token: string; readonly job_id: string; readonly tab_id: number }
    | undefined;

  constructor(
    private readonly deps: ToastDeliveryDeps,
    private readonly ctx: ToastDeliveryContext,
  ) {}

  /** Try the in-page route. Returns whether an injected toast is now live, so
   * the caller falls back to the window rather than assuming either way.
   *
   * Every gate here is a refusal to interrupt, and they are ordered cheapest
   * first. The preference selects the integrated route; the all-sites grant
   * says papio may reach that page. A provider grant remains insufficient: it
   * was given so papio could finish a download on that host, not so papio could
   * draw there. */
  private async raiseInPageToast(payload: ToastPayload): Promise<boolean> {
    const tabs = this.deps.tabs;
    if (tabs.query === undefined) return false;
    if (!(await this.deps.settings.getInPageToast().catch(() => false)))
      return false;
    const granted = await this.deps.permissions
      .contains({ origins: [ALL_SITES_ORIGIN] })
      .catch(() => false);
    if (granted !== true) return false;
    // Called through its owner, never as an extracted reference: the Chrome
    // adapter is a bound arrow but a class-backed seam is not, and an unbound
    // call throws inside the seam and reads here as "no active tab".
    const [tab] = await tabs
      .query({ active: true, lastFocusedWindow: true })
      .catch(() => []);
    const tabID = tab?.id;
    if (tabID === undefined || typeof tab?.url !== "string") return false;
    // Never into a papio surface. A tab papio tracks or owns is the work
    // surface, not the researcher's reading, and the tab whose loss started
    // this may still be in the ledger cache.
    if (findByTab(this.ctx.store(), tabID) !== undefined) return false;
    if (this.ctx.tabLedgerCache()?.[String(tabID)] !== undefined) return false;
    // Only an ordinary HTTPS page. The grant covers exactly that scheme, and a
    // PDF viewer or privileged page refuses injection anyway — reaching it
    // through the catch below would work, but failing here keeps the window
    // fallback fast instead of paying a rejected round trip.
    let httpsPage = false;
    try {
      httpsPage = new URL(tab.url).protocol === "https:";
    } catch {
      return false;
    }
    if (!httpsPage || isPDFPage(tab.url)) return false;
    const copy = toastCopy(payload);
    const token = this.deps.randomUUID();
    try {
      const [injected] = await this.deps.scripting.executeScript({
        target: { tabId: tabID },
        func: renderPageToast,
        args: [
          {
            kind: payload.kind,
            job_id: payload.job_id,
            token,
            message: copy.message,
            action: copy.action,
            window_ms: TOAST_WINDOW_MS,
            action_message: TOAST_PAGE_ACTION_MESSAGE,
            dismiss_message: TOAST_PAGE_DISMISS_MESSAGE,
            mark: PAPIO_MARK,
            mark_viewbox: PAPIO_MARK_VIEWBOX,
            mark_size_px: PAPIO_MARK_SIZE_PX,
            max_width_px: TOAST_WINDOW_SIZE.width,
          } satisfies ToastInjection,
        ],
      });
      if (injected?.result !== true) return false;
    } catch {
      // Withdrawn grant, a page type that refuses scripting, or a tab that
      // navigated mid-call. The window route still covers this loss.
      return false;
    }
    this.pendingPageToast = { token, job_id: payload.job_id, tab_id: tabID };
    return true;
  }

  /** Raise the seventh surface for a loss papio observed itself, or for
   * papio's own close of a paper it filed. Returns whether a surface was
   * delivered, so the caller can fall back to silence rather than assume. */
  async raiseToast(payload: ToastPayload): Promise<boolean> {
    // A papio surface already in front reports the same event, and Decision 9's
    // presence hint is exactly the signal for that. Interrupting a researcher
    // who is looking at the popup would be a duplicate, not an aid.
    if (this.ctx.papioSurfaceLikelyFocused()) return false;
    this.pendingToast = payload;
    // A loss replaces a pending reopen offer; the closed papers are still in
    // the history page and the browser's own reopen-closed-tab.
    if (payload.kind !== "paper_filed") this.filedBatch = undefined;
    // Replace rather than stack: retire the previous surface on BOTH routes
    // before raising either, so the researcher is never asked about two losses
    // at once and a superseded injected toast can no longer act.
    //
    // An injected toast in a tab the researcher has since left is not removed
    // from that page here — papio would have to inject again to do it. It is
    // bounded instead: it disappears on its own within TOAST_WINDOW_MS, and
    // dropping the token below means the stale offer is refused if clicked.
    await this.closeToastWindow();
    this.pendingPageToast = undefined;
    if (await this.raiseInPageToast(payload)) return true;
    const windows = this.deps.windows;
    const url = this.deps.runtimeGetURL?.(TOAST_PAGE_PATH);
    if (windows === undefined || url === undefined) {
      this.pendingToast = undefined;
      return false;
    }
    try {
      const created = await windows.create({
        url,
        focused: false,
        type: "popup",
        width: TOAST_WINDOW_SIZE.width,
        height: TOAST_WINDOW_SIZE.height,
      });
      this.toastWindowID = created.id;
      // macOS Firefox ignores `focused` at creation (bugzilla 1271047), the
      // same defect the work window already documents. Unfixed here it is
      // worse than there: a minimized work window arriving front is a nuisance,
      // but a toast that steals focus is the opposite of a toast.
      if (created.id !== undefined && created.focused === true) {
        try {
          await windows.update(created.id, { focused: false });
        } catch {
          // Best effort. A toast that took focus is still a delivered toast.
        }
      }
      return true;
    } catch {
      this.pendingToast = undefined;
      this.toastWindowID = undefined;
      return false;
    }
  }

  /** Remember that papio filed this job, so closing its surfaces offers the
   * paper back. */
  noteFiledJob(jobID: string): void {
    this.filedJobs.delete(jobID);
    this.filedJobs.add(jobID);
    for (const oldest of this.filedJobs) {
      if (this.filedJobs.size <= FILED_JOB_MEMORY) break;
      this.filedJobs.delete(oldest);
    }
  }

  /** Add one closed paper to the reopen batch and raise the batch's toast
   * once it has been quiet for FILED_TOAST_SETTLE_MS. A close that lands
   * while the toast is up replaces it with the larger count: one toast at a
   * time, never a stack. */
  noteFiledPaperClosed(jobID: string, url: string): void {
    const batch = this.filedBatch ?? {
      id: `${FILED_TOAST_PREFIX}${this.deps.randomUUID()}`,
      urls: new Map<string, string>(),
      revision: 0,
    };
    this.filedBatch = batch;
    // One entry per paper. The PDF is a better thing to reopen than the page
    // that led to it.
    const known = batch.urls.get(jobID);
    if (known === undefined || (!isPDFPage(known) && isPDFPage(url)))
      batch.urls.set(jobID, url);
    batch.revision += 1;
    const revision = batch.revision;
    this.deps.setTimeout(async () => {
      if (this.filedBatch !== batch || batch.revision !== revision) return;
      const raised = await this.raiseToast({
        kind: "paper_filed",
        job_id: batch.id,
        count: batch.urls.size,
      });
      // An offer nobody saw is dropped rather than carried into a later,
      // larger toast.
      if (!raised && this.filedBatch === batch) this.filedBatch = undefined;
    }, FILED_TOAST_SETTLE_MS);
  }

  /** Reopen what papio closed. One paper opens in front of the operator;
   * several open quietly where papio's own tabs go, per the work-window
   * setting. After the worker slept the URLs are gone, and the history page,
   * which lists every filed paper, is the honest fallback. */
  private async reopenFiledPapers(batchID: string): Promise<boolean> {
    const batch = this.filedBatch;
    this.filedBatch = undefined;
    try {
      if (batch?.id !== batchID) {
        const history = this.deps.runtimeGetURL?.(HISTORY_PAGE_PATH);
        if (history === undefined) return false;
        await this.deps.tabs.create({ url: history, active: true });
        return true;
      }
      const urls = [...batch.urls.values()];
      if (urls.length === 1) {
        await this.deps.tabs.create({ url: urls[0]!, active: true });
        return true;
      }
      for (const url of urls) await this.ctx.openBrokerTab(url, false);
      return true;
    } catch {
      return false;
    }
  }

  /** Close the toast window papio opened, and ONLY that window.
   *
   * The ownership re-check is not defensive padding. A researcher who closes
   * the toast themselves reports nothing — the page's own handlers never run —
   * so this id outlives the window it named. A browser is free to reuse a
   * window id after that, and removing a recycled id would close one of the
   * researcher's own windows. So the id alone never authorizes a removal: the
   * window must still be serving the toast page. */
  private async closeToastWindow(): Promise<void> {
    const windowID = this.toastWindowID;
    this.toastWindowID = undefined;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return;
    const toastURL = this.deps.runtimeGetURL?.(TOAST_PAGE_PATH);
    if (toastURL === undefined) return;
    try {
      const existing = await windows.get(windowID);
      const tabs = existing.tabs ?? [];
      // Exactly one tab, and it must be the toast page. A window the
      // researcher has navigated or added a tab to is theirs, not papio's.
      if (tabs.length !== 1 || tabs[0]?.url !== toastURL) return;
      await windows.remove(windowID);
    } catch {
      // Already gone, or unreadable: either way papio does not remove it.
    }
  }

  /** The page asks for its payload on load. Answering consumes nothing: the
   * page may reload, and a reload that rendered an empty toast would look like
   * a papio failure. The payload is dropped when the page reports an outcome. */
  toastPending(): ToastPayload | undefined {
    return this.pendingToast;
  }

  /** The researcher took the offer. `route_lost` and
   * `institution_claim_lost` both resolve to the same daemon call — the fresh
   * route `papio actions open` mints — because the extension cannot mint one
   * itself: `WithOpenRouteJob` is the daemon's authorization boundary, and an
   * offer that opened a tab by itself is exactly what papio must never do for
   * a paper it asked a human to fetch. `paper_filed` reopens the tabs papio
   * itself closed, from worker memory. */
  async toastAction(jobID: string): Promise<boolean> {
    const payload = this.pendingToast;
    this.pendingToast = undefined;
    this.toastWindowID = undefined;
    // A reopen offer whose worker slept still deserves an answer: the batch is
    // gone with the worker, and reopenFiledPapers falls back to the history.
    if (payload === undefined && jobID.startsWith(FILED_TOAST_PREFIX))
      return this.reopenFiledPapers(jobID);
    // Ignore an id that is not the offer papio made. A stale window reloaded
    // after a replacement would otherwise reopen the wrong paper.
    if (payload === undefined || payload.job_id !== jobID) return false;
    if (payload.kind === "paper_filed") return this.reopenFiledPapers(jobID);
    const minted = await this.ctx.requestFreshHandoffLink(jobID);
    if (minted.ok !== true) return false;
    try {
      await this.deps.tabs.create({ url: minted.url, active: true });
      return true;
    } catch {
      return false;
    }
  }

  /** Dismissed or expired. Both drop the offer and neither performs the
   * action; the recovery stays in the inbox, which is what keeps the eight
   * seconds from being a deadline. */
  toastDismiss(jobID: string): void {
    if (this.pendingToast?.job_id === jobID) this.pendingToast = undefined;
    if (this.filedBatch?.id === jobID) this.filedBatch = undefined;
    this.toastWindowID = undefined;
  }

  /** Consume the injected route's one-use authorization, or refuse.
   *
   * All three facts must agree: the token papio minted, the job it minted it
   * for, and the tab it injected into. The token alone would be enough against
   * a page (it cannot read the isolated world), but the tab check is what makes
   * a replaced offer inert — a superseded toast still sitting on another tab
   * carries a real token for a job that is no longer the pending one.
   *
   * Consuming here rather than in the caller means a refused report cannot
   * silently clear a live offer — pinned by the wrong-token test, which asserts
   * the real offer survives a bad one.
   *
   * The clear on success is defence at this layer only, and deliberately not
   * claimed as more: `toastAction` drops `pendingToast` itself, so a second act
   * is already refused one level down. Keeping it here means this
   * authorization's one-use property does not depend on reading that method.
   */
  private consumePageToast(jobID: string, token: string, tabID: number): boolean {
    const pending = this.pendingPageToast;
    if (
      pending === undefined ||
      pending.token !== token ||
      pending.job_id !== jobID ||
      pending.tab_id !== tabID
    ) {
      return false;
    }
    this.pendingPageToast = undefined;
    return true;
  }

  /** The researcher took the offer in their own page. Same daemon call as the
   * window route, and deliberately the same method afterwards: the recovery is
   * one behaviour with two delivery routes, not two behaviours. */
  async pageToastAction(
    jobID: string,
    token: string,
    tabID: number,
  ): Promise<boolean> {
    if (!this.consumePageToast(jobID, token, tabID)) return false;
    return this.toastAction(jobID);
  }

  /** Dismissed or expired in the page. The injected surface has already removed
   * itself, so this only drops the offer. */
  pageToastDismiss(jobID: string, token: string, tabID: number): void {
    if (!this.consumePageToast(jobID, token, tabID)) return;
    this.toastDismiss(jobID);
  }
}
