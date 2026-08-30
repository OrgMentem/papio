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

/** papio's mark: the same four shapes every other papio surface inlines
 * (`popup.html`, `inbox.html`, `options.html`, `history.html`,
 * `page-bulk.html`, and `docs/assets/logo.svg`). The path data is copied from
 * them rather than redrawn, because a second drawing of the mark is a second
 * brand.
 *
 * Geometry and a role per shape, deliberately WITHOUT colours. The two routes
 * must resolve those differently: the window route is a papio page and reads its
 * own `--color-brand-*` custom properties, while the injected route runs inside
 * a page whose custom properties are not papio's and so must carry literals. A
 * role per shape is what lets one definition serve both.
 *
 * It reaches the injected route as data on `ToastInjection`, for the same reason
 * `TOAST_COPY` does: `chrome.scripting` drops every outer-scope reference, and a
 * second copy of the geometry is a thing that can drift.
 *
 * Why the mark is here at all: this is the one papio surface that can appear
 * inside a publisher's page, where a bare sentence has no sender. The mark
 * answers "who is speaking" without adding an identifier, a title, a URL, or a
 * job reference — none of which this surface is allowed to carry. */
export const PAPIO_MARK_VIEWBOX = "0 0 512 512";

export interface MarkShape {
  readonly tag: "path" | "circle";
  /** Which brand colour strokes this shape. */
  readonly role: "ink" | "accent";
  /** Applied verbatim. Colour attributes are absent by construction. */
  readonly attrs: Readonly<Record<string, string>>;
  /** The arrowhead is a filled triangle; the ring and the stem are stroked
   * outlines, so their fill must stay `none` rather than inherit black. */
  readonly filled?: boolean;
}

export const PAPIO_MARK: readonly MarkShape[] = [
  // The broken ring: the library the fetcher reaches out of.
  {
    tag: "path",
    role: "ink",
    attrs: { d: "M 95.0 374.6 A 200 200 0 1 1 243.7 455.6", "stroke-width": "46", "stroke-linecap": "round" },
  },
  // The oblique p's bowl. A transform on the circle rather than a wrapping `g`:
  // identical rendering for a single child, one fewer node to serialize.
  {
    tag: "circle",
    role: "accent",
    attrs: { cx: "0", cy: "0", r: "68", "stroke-width": "42", transform: "translate(264 244) skewX(-10)" },
  },
  // The stem, which becomes the descender.
  {
    tag: "path",
    role: "accent",
    attrs: { d: "M 208.0 176.0 L 163.2 430.0", "stroke-width": "42", "stroke-linecap": "round" },
  },
  // The descender's arrowhead, exiting through the ring's gap.
  {
    tag: "path",
    role: "accent",
    filled: true,
    attrs: { d: "M 115.7 416.0 L 215.7 416.0 L 152.6 490.0 Z", "stroke-width": "10", "stroke-linejoin": "round" },
  },
];

/** Sized to the two-line text block, not to one line. Both messages wrap to two
 * lines at this width, so the copy beside the mark is a 41px block; 28px sits
 * inside that (68%) and reads clearly, while never being the tallest thing in the
 * row and so never setting the card's height. Chosen by looking at 20, 24, 28,
 * and 32 rendered at the real width: 20 — the line height, which was the first
 * rule tried — is legible but visibly undersized against two lines, and 32, the
 * `--brand-mark-size` the other papio surfaces use, is sized for a page header
 * beside a wordmark at title size rather than a 14px card.
 *
 * Load-bearing for the width below: the two-line boundary was measured at THIS
 * size. */
export const PAPIO_MARK_SIZE_PX = 28;

/** Build the mark. Used by the window route; the injected route runs the same
 * loop inline over the same data, because a function reference cannot cross
 * `chrome.scripting`'s serialization boundary.
 *
 * Decorative on purpose: `aria-hidden`, no title, no label. The sentence beside
 * it already names papio, so an accessible name here would announce the sender
 * twice. */
export function renderPapioMark(doc: Document, ink: string, accent: string): SVGSVGElement {
  const NS = "http://www.w3.org/2000/svg";
  const svg = doc.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", PAPIO_MARK_VIEWBOX);
  svg.setAttribute("aria-hidden", "true");
  // Sized here rather than in `toast.html`, so one constant sizes both routes:
  // a CSS rule for the window and a constant for the injected body is two
  // numbers that must agree, with nothing making them.
  svg.style.cssText = `flex:none;width:${PAPIO_MARK_SIZE_PX}px;height:${PAPIO_MARK_SIZE_PX}px`;
  for (const shape of PAPIO_MARK) {
    const el = doc.createElementNS(NS, shape.tag);
    for (const [name, value] of Object.entries(shape.attrs)) el.setAttribute(name, value);
    const colour = shape.role === "ink" ? ink : accent;
    el.setAttribute("stroke", colour);
    el.setAttribute("fill", shape.filled === true ? colour : "none");
    svg.append(el);
  }
  return svg;
}

/** A toast, not a browser window: small, chrome-less, and deliberately
 * unfocused. Lives here rather than beside `windows.create` because both routes
 * now need the width — the injected card is sized to the same measurement.
 *
 * Measured, not guessed, three times. At 420px — the popup's width, which this
 * first used — the institutional message wraps to FOUR lines and needs 106px of
 * inner height, while `windows.create` height includes the window frame, so a
 * 108 outer left roughly 80 inner and clipped the copy. At 520 both messages
 * wrapped to two lines and needed 65 inner. Adding the mark costs the message
 * 40px of its row (28px plus the 12px gap), which re-wrapped the institutional
 * message to three lines and 85 inner — one pixel past the 84 that a 116 outer
 * leaves on macOS.
 *
 * Re-measured in a real browser at `PAPIO_MARK_SIZE_PX`: the two-line boundary
 * is 552, so 576 carries 24px of slack (~4%) for platforms whose system-ui
 * metrics run wider than macOS's. Both numbers move together — a larger mark
 * raises the boundary, which is why the size constant says so. `toast.html`
 * also scrolls rather than clips, so copy or metrics that outgrow this degrade
 * visibly instead of silently. */
export const TOAST_WINDOW_SIZE = { width: 576, height: 116 } as const;

/** The injected route's own message types, deliberately NOT the extension
 * page's `TOAST_ACTION_MESSAGE`.
 *
 * That type is authorized by `isToastSender`, which requires the sender to BE
 * the toast page (`sender.url === urls.toastURL`). An injected toast's sender
 * is the researcher's own web page, so serving it through the same type would
 * mean relaxing that check to admit page senders — and every page papio can
 * inject into could then speak as the toast surface. Separate types keep the
 * two gates separate, and this one additionally requires the one-use token
 * papio minted when it injected. */
export const TOAST_PAGE_ACTION_MESSAGE = "papio.toast.pageAction";
export const TOAST_PAGE_DISMISS_MESSAGE = "papio.toast.pageDismiss";

/** Everything the injected function needs, as one serializable argument.
 *
 * `chrome.scripting` drops every outer-scope reference, so the injected body
 * cannot import `TOAST_COPY` or `PAPIO_MARK`, and cannot call `renderToast` or
 * `renderPapioMark`. Passing them as data keeps the single source: both routes
 * still read the same copy table and the same mark geometry, so neither the
 * sentence, the offer, nor the drawing can diverge even though the two routes
 * build their DOM separately. */
export interface ToastInjection {
  readonly kind: ToastKind;
  readonly job_id: string;
  /** One-use, minted per injection. The action message is refused without it. */
  readonly token: string;
  readonly message: string;
  readonly action: string;
  readonly window_ms: number;
  readonly action_message: string;
  readonly dismiss_message: string;
  /** See `PAPIO_MARK`. Geometry only; the injected body resolves the roles to
   * literals because the page's custom properties are not papio's. */
  readonly mark: readonly MarkShape[];
  readonly mark_viewbox: string;
  readonly mark_size_px: number;
  /** `TOAST_WINDOW_SIZE.width`, so the injected card is sized to the same
   * measurement as the window rather than to a second literal. */
  readonly max_width_px: number;
}

export interface ToastElements {
  readonly root: HTMLElement;
  /** Decorative, and the first child: the sender reads before the sentence. */
  readonly mark: SVGSVGElement;
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
  // papio's mark leads, so the researcher reads the sender before the claim.
  // The window route can name its colours, because this document is papio's.
  const mark = renderPapioMark(doc, "var(--color-brand-ink)", "var(--color-brand-accent)");
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
  container.append(mark, message, actions);
  return { root: container, mark, message, action, dismiss };
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
