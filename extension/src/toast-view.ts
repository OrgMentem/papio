// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// ADR-0023's seventh surface: a real in-browser toast for a loss *papio*
// observed on a tab it opened itself, carrying exactly one take-back-control
// action. It exists because the third surface cannot serve this case by
// construction — the host-page acknowledgement is defined as a noninteractive
// receipt for an action the researcher just requested in the popup, "never for
// a failure, a later job transition, or an event that arrived on its own"
// (dev/adr/0023-notification-feedback-and-liveness-surfaces.md:43-60). A tab
// closing is exactly an event that arrived on its own.
//
// This module is the shared renderer for both delivery routes: an unfocused
// extension window (always available, no new permission) and — where the
// current tab is already a granted host — an injected in-page toast. The DOM it
// builds is identical, so the two routes can never drift in copy or in what
// they offer.

/** The closed set of losses that may raise a toast. A kind exists only when
 * papio can offer a truthful action for it, which is why an `awaiting_download`
 * park and an in-flight delivery detach are absent: neither lost anything, so
 * neither has a recovery to offer. */
export type ToastKind = "route_lost" | "institution_claim_lost";

export interface ToastPayload {
  readonly kind: ToastKind;
  /** Carried in the extension's own message only. Never rendered, and never
   * placed in a page's DOM or URL: bound 3 of the plan, and the same rule
   * Decision 1 already applies to the host-page acknowledgement. */
  readonly job_id: string;
}

interface ToastCopy {
  /** One sentence naming what papio lost, and what it does next unasked. It
   * names no identifier, title, URL, provider, or job id — the researcher is
   * about to be shown a surface, not a report. */
  readonly message: string;
  /** The single action. `Reopen` is only offered where the route is genuinely
   * resumable; the institutional case says what it really does, because
   * `owner_closed` is terminal for that claim and no undo exists. */
  readonly action: string;
}

export const TOAST_COPY: Readonly<Record<ToastKind, ToastCopy>> = {
  route_lost: {
    message: "papio lost the tab it was driving for a paper. It will try again on its own.",
    action: "Reopen now",
  },
  institution_claim_lost: {
    message: "papio lost the sign-in tab for your library, so a paper is waiting again.",
    action: "Open a new sign-in tab",
  },
};

/** Eight seconds, and expiry commits NOTHING. The recovery stays reachable in
 * the inbox afterwards, which is what keeps this clear of WCAG 2.2.1: the toast
 * is a shortcut to an action that has no deadline, not a timed decision. The
 * existing inbox undo bar runs six (`inbox.ts` UNDO_WINDOW_MS), so this is the
 * longer of papio's two windows, not a new kind of pressure. */
export const TOAST_WINDOW_MS = 8000;

export interface ToastElements {
  readonly root: HTMLElement;
  readonly message: HTMLElement;
  readonly action: HTMLButtonElement;
  readonly dismiss: HTMLButtonElement;
}

/** Build the toast into an existing container. Pure: it touches no chrome API,
 * reads no storage, and starts no timer, so both routes and the tests drive the
 * identical DOM. `textContent` throughout — build.ts rejects a page bundle
 * containing `innerHTML`. */
export function renderToast(doc: Document, container: HTMLElement, payload: ToastPayload): ToastElements {
  const copy = TOAST_COPY[payload.kind];
  container.replaceChildren();
  container.dataset.kind = payload.kind;
  // The live region announces the message only. The buttons are reachable by
  // keyboard in DOM order, so a screen reader is not asked to re-read them.
  const message = doc.createElement("p");
  message.className = "toast-message";
  message.id = "toast-message";
  message.textContent = copy.message;

  const actions = doc.createElement("div");
  actions.className = "toast-actions";

  const action = doc.createElement("button");
  action.type = "button";
  action.id = "toast-action";
  action.className = "toast-action";
  action.textContent = copy.action;

  const dismiss = doc.createElement("button");
  dismiss.type = "button";
  dismiss.id = "toast-dismiss";
  dismiss.className = "toast-dismiss";
  dismiss.textContent = "Dismiss";
  // A glyph-only close would need its own label; a word costs less and reads
  // the same to both.
  dismiss.setAttribute("aria-label", "Dismiss this message");

  actions.append(action, dismiss);
  container.append(message, actions);
  return { root: container, message, action, dismiss };
}

/** The kind this loss deserves, or `undefined` when it deserves none. The
 * producer decides from job state, and this is the one place that mapping is
 * allowed to live, so the two delivery routes cannot disagree about when a
 * toast is honest.
 *
 * `waiting_for_session` is deliberately NOT an input. It re-queues to `queued`
 * and schedules release, and the default branch reports `cancelled` and
 * re-drains — different daemon-side stories, but the same offer to the
 * researcher, because both leave a resumable route. Splitting them here would
 * add a parameter that no caller can act on differently. */
export function toastKindForLoss(loss: {
  institutionalClaimAbandoned: boolean;
  deliveryInFlight: boolean;
  awaitingDownload: boolean;
}): ToastKind | undefined {
  // Checked first: it is the only loss whose recovery is a NEW claim rather
  // than a resumed route, because `owner_closed` abandons the claim, retires
  // the authentication-entry lease, and consumes the one-use close
  // authorization. Offering "Reopen" here would promise a reversal papio
  // cannot perform.
  if (loss.institutionalClaimAbandoned) return "institution_claim_lost";
  // Nothing was lost: the download keeps its correlation, and an
  // awaiting_download park is adopted by the daemon's poll. Interrupting for
  // either would be a false alarm.
  if (loss.deliveryInFlight || loss.awaitingDownload) return undefined;
  return "route_lost";
}
