// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Route B of the seventh surface: the delivery that is always available. An
// unfocused extension window needs no host permission and no new manifest
// permission, so it reaches the researcher whatever page they are on — which an
// injected in-page toast cannot promise, because provider hosts are
// `optional_host_permissions` and papio only ever requests the exact resolver
// origin its operator configured.
//
// The window carries no state in its URL. It asks the background for the one
// pending toast on load, which makes the single-instance rule (bound 1) a
// property of the producer rather than a convention this page has to keep.

import {
  TOAST_WINDOW_MS,
  type ToastPayload,
  renderToast,
} from "./toast-view";

export const TOAST_PENDING_MESSAGE = "papio.toast.pending";
export const TOAST_ACTION_MESSAGE = "papio.toast.action";
export const TOAST_DISMISS_MESSAGE = "papio.toast.dismiss";

/** The browser's own timer handle. Named here rather than derived from
 * `setTimeout`, so the contract is this module's and not the platform lib's. */
export type ToastTimer = number;

interface ToastPageDeps {
  readonly doc: Document;
  readonly sendMessage: (message: unknown) => Promise<unknown>;
  readonly closeWindow: () => void;
  readonly setTimer: (run: () => void, ms: number) => ToastTimer;
  readonly clearTimer: (handle: ToastTimer) => void;
  /** Registers interest in this window being brought forward. Injected rather
   * than read off `window`, so the re-arm below is driven by the test. */
  readonly onFocus: (run: () => void) => void;
}
/** Accepts only the closed payload shape, inside the router's own envelope. A
 * malformed, absent, or refused reply closes the window rather than rendering
 * an empty toast: an interruption with nothing in it is worse than none.
 *
 * The envelope is unwrapped here rather than at the call site because the
 * failure disposition is identical for "the producer said no" and "the producer
 * said something I cannot read" — both mean there is nothing to offer. */
export function parseToastPayload(value: unknown): ToastPayload | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  if (!("ok" in value) || value.ok !== true) return undefined;
  const toast = "toast" in value ? value.toast : undefined;
  if (typeof toast !== "object" || toast === null) return undefined;
  const kind = "kind" in toast ? toast.kind : undefined;
  const jobID = "job_id" in toast ? toast.job_id : undefined;
  if (kind !== "route_lost" && kind !== "institution_claim_lost") return undefined;
  if (typeof jobID !== "string" || jobID === "") return undefined;
  return { kind, job_id: jobID };
}

/** Drives one toast window from load to close. Exported with injected deps so
 * the test drives the real timer arithmetic and the real message sequence
 * without a browser. */
export async function runToastPage(deps: ToastPageDeps): Promise<void> {
  // `getElementById` already narrows to HTMLElement or null, so a null check
  // is the whole guard. An `instanceof` against the document's own view would
  // need a non-null assertion and would fail in any harness that supplies a
  // detached document.
  const container = deps.doc.getElementById("toast");
  if (container === null) {
    deps.closeWindow();
    return;
  }
  let reply: unknown;
  try {
    reply = await deps.sendMessage({ type: TOAST_PENDING_MESSAGE });
  } catch {
    deps.closeWindow();
    return;
  }
  const payload = parseToastPayload(reply);
  if (payload === undefined) {
    deps.closeWindow();
    return;
  }
  const elements = renderToast(deps.doc, container, payload);
  // Expiry closes the window and tells the producer the offer lapsed. It
  // commits NOTHING: the recovery is still in the inbox, which is what keeps
  // this window clear of a timed decision.
  const expire = (): void => {
    void deps.sendMessage({ type: TOAST_DISMISS_MESSAGE, job_id: payload.job_id, reason: "expired" });
    deps.closeWindow();
  };
  let timer = deps.setTimer(expire, TOAST_WINDOW_MS);
  // Measured on macOS: the first click on an unfocused window is spent
  // activating it, and does not reach the button under the pointer. A
  // researcher who notices the toast late would then lose the offer to expiry
  // between their two clicks — so being brought forward restarts the clock,
  // and the full window is available for the click that lands.
  //
  // Once only. Re-arming on every focus would let a window cycled in and out
  // of the foreground live indefinitely, and this surface is bounded by
  // design: an offer that never lapses is a decision papio is still holding.
  let rearmed = false;
  deps.onFocus(() => {
    if (rearmed) return;
    rearmed = true;
    deps.clearTimer(timer);
    timer = deps.setTimer(expire, TOAST_WINDOW_MS);
  });
  // Both buttons stop the timer first. Without that, a researcher who clicks
  // the action at 7.9s gets the expiry message racing their own request, and
  // the producer cannot tell which one told the truth.
  elements.action.addEventListener("click", () => {
    deps.clearTimer(timer);
    elements.action.disabled = true;
    elements.dismiss.disabled = true;
    void deps.sendMessage({ type: TOAST_ACTION_MESSAGE, job_id: payload.job_id });
    deps.closeWindow();
  });
  elements.dismiss.addEventListener("click", () => {
    deps.clearTimer(timer);
    void deps.sendMessage({ type: TOAST_DISMISS_MESSAGE, job_id: payload.job_id, reason: "dismissed" });
    deps.closeWindow();
  });
  // The action is the reason this window exists, so it holds focus even though
  // the window itself is deliberately unfocused: the moment the researcher
  // brings the window forward, Enter does the thing they came for.
  elements.action.focus();
}

if (typeof document !== "undefined" && document.getElementById("toast") !== null) {
  void runToastPage({
    doc: document,
    sendMessage: (message) => chrome.runtime.sendMessage(message),
    closeWindow: () => window.close(),
    setTimer: (run, ms) => window.setTimeout(run, ms),
    clearTimer: (handle) => window.clearTimeout(handle),
    onFocus: (run) => window.addEventListener("focus", run),
  });
}
