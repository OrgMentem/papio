// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { test, expect } from "bun:test";
import { Window } from "happy-dom";

import {
  PAPIO_MARK,
  PAPIO_MARK_SIZE_PX,
  PAPIO_MARK_VIEWBOX,
  TOAST_COPY,
  TOAST_WINDOW_MS,
  TOAST_WINDOW_SIZE,
  renderPapioMark,
  renderToast,
  toastKindForLoss,
} from "../src/toast-view";

function toastDocument(): { doc: Document; container: HTMLElement } {
  const window = new Window();
  const doc = window.document as unknown as Document;
  const container = doc.createElement("div");
  doc.body.append(container);
  return { doc, container };
}

test("the toast offers exactly one action and one dismissal", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });

  // Decision 1's density rule applied to the seventh surface: a toast that
  // grows a second action becomes a dialog, and a dialog belongs in the inbox.
  const buttons = container.querySelectorAll("button");
  expect(buttons).toHaveLength(2);
  expect(doc.getElementById("toast-action")?.textContent).toBe("Reopen now");
  expect(doc.getElementById("toast-dismiss")?.textContent).toBe("Dismiss");
});

test("the institutional loss never offers to reopen", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "institution_claim_lost", job_id: "job-2" });

  // `owner_closed` abandons the claim, retires the lease, and consumes the
  // one-use close authorization, so no reversal exists. A button saying
  // "Reopen" or "Undo" here would promise one. This is the assertion that
  // fails if someone unifies the two copy entries to save a line.
  const label = doc.getElementById("toast-action")?.textContent ?? "";
  expect(label).toBe("Open a new sign-in tab");
  expect(label.toLowerCase()).not.toContain("reopen");
  expect(label.toLowerCase()).not.toContain("undo");
});

test("no rendered text carries the job id", () => {
  const { doc, container } = toastDocument();
  const jobID = "job_b08c0e5e6e632d13ab2b607ccd";
  renderToast(doc, container, { kind: "route_lost", job_id: jobID });

  // Bound 3, and the same rule Decision 1 already holds the host-page
  // acknowledgement to. The id travels in the extension's own message; a page
  // never sees it. Checked on the whole subtree, not just the message node,
  // because a helpful `data-job-id` on the button would leak it just as far.
  expect(container.textContent ?? "").not.toContain(jobID);
  expect(container.outerHTML).not.toContain(jobID);
});

test("papio's mark leads the toast, and says nothing to a screen reader", () => {
  const { doc, container } = toastDocument();
  const { mark } = renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });

  // First child, not last: this is the one papio surface that can appear inside
  // a publisher's page, so the sender must read before the claim.
  expect(container.firstElementChild).toBe(mark as unknown as Element);
  expect(mark.getAttribute("viewBox")).toBe(PAPIO_MARK_VIEWBOX);
  // Decorative. The sentence beside it already names papio, so a title or label
  // here would announce the sender twice.
  expect(mark.getAttribute("aria-hidden")).toBe("true");
  expect(mark.querySelector("title")).toBeNull();
  expect(mark.getAttribute("aria-label")).toBeNull();
  // The window route is a papio document, so it names its colours rather than
  // baking literals a theme change would strand. Asserted in order, not as a
  // set: the ring is the ink shape and the other three are accent, so an
  // ink/accent swap must fail here rather than pass on "both colours appear".
  const strokes = [...mark.children].map((el) => el.getAttribute("stroke"));
  expect(strokes).toEqual(
    PAPIO_MARK.map((shape) =>
      shape.role === "ink" ? "var(--color-brand-ink)" : "var(--color-brand-accent)",
    ),
  );
  expect(strokes[0]).toBe("var(--color-brand-ink)");
});

test("the mark's outlines are not filled", () => {
  // The ring and the stem are stroked paths; an inherited `fill` turns each into
  // a black blob, which is what happens when the fill attribute is simply
  // omitted. Only the descender's arrowhead is a filled shape.
  const { doc } = toastDocument();
  const mark = renderPapioMark(doc, "#111111", "#222222");
  const fills = [...mark.children].map((el) => el.getAttribute("fill"));
  expect(fills).toEqual(["none", "none", "none", "#222222"]);
  expect(PAPIO_MARK.filter((shape) => shape.filled === true)).toHaveLength(1);
});

test("the mark carries no colour of its own", () => {
  // The geometry is shared with the injected route, which cannot resolve custom
  // properties. A colour attribute baked into `PAPIO_MARK` would therefore paint
  // one route correctly and the other wrongly, with nothing to catch it.
  for (const shape of PAPIO_MARK) {
    for (const name of Object.keys(shape.attrs)) {
      expect(name, `${shape.role} shape carries ${name}`).not.toBe("fill");
      expect(name, `${shape.role} shape carries ${name}`).not.toBe("stroke");
    }
  }
});

test("no copy entry names an identifier, provider, or URL", () => {
  // The closed copy table is the whole vocabulary of this surface. A future
  // entry that interpolates a title or a provider name fails here rather than
  // in review.
  for (const [kind, copy] of Object.entries(TOAST_COPY)) {
    expect(copy.message, kind).not.toMatch(/https?:|\bdoi\b|10\.\d{4}|job[_-]/i);
    expect(copy.action, kind).not.toMatch(/https?:|\bdoi\b|10\.\d{4}|job[_-]/i);
    // Brevity, measured directly. A sentence count was the wrong proxy:
    // `route_lost` needs its second sentence, because ADR-0023 Decision 3
    // requires copy that does not overpromise, and "It will try again on its
    // own" is what makes the offer a shortcut rather than the only recovery.
    // A body past this bound is a report, and reports belong in Activity.
    expect(copy.message.length, kind).toBeLessThanOrEqual(110);
    expect(copy.action.length, kind).toBeLessThanOrEqual(28);
  }
});

test("re-rendering replaces the toast instead of stacking it", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });
  renderToast(doc, container, { kind: "institution_claim_lost", job_id: "job-2" });

  // Bound 1. Two losses in quick succession is the normal case when a window
  // closes with several papio tabs in it, and an appending renderer would
  // build a list nobody asked for.
  expect(container.querySelectorAll("button")).toHaveLength(2);
  expect(container.querySelectorAll(".toast-message")).toHaveLength(1);
  expect(doc.getElementById("toast-action")?.textContent).toBe("Open a new sign-in tab");
  expect(container.dataset.kind).toBe("institution_claim_lost");
});

test("a loss that lost nothing raises no toast", () => {
  // The two branches where the tab closing cost the researcher nothing: the
  // download keeps its own correlation, and an awaiting_download park is
  // adopted by the daemon's poll scan. A toast for either is a false alarm,
  // and false alarms are how a proactive surface gets turned off.
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: true, awaitingDownload: false }),
  ).toBeUndefined();
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: false, awaitingDownload: true }),
  ).toBeUndefined();
});

test("an institutional claim loss outranks a delivery still in flight", () => {
  // Both true is reachable: a paper can hold an institutional claim and have a
  // download correlated when its window closes. The claim is the loss with no
  // recovery, so it must win — the opposite order would report nothing at all
  // and strand the library's sign-in slot silently, which is the exact failure
  // the surface-lifecycle work exists to prevent.
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: true, deliveryInFlight: true, awaitingDownload: true }),
  ).toBe("institution_claim_lost");
});

test("an ordinary route loss offers a reopen", () => {
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: false, awaitingDownload: false }),
  ).toBe("route_lost");
});

test("the toast window outlasts the inbox undo window", () => {
  // Not a magic number check: the inbox already commits a dismissal after six
  // seconds, and this surface asks for an action rather than deferring one, so
  // it must not be the tighter of the two.
  expect(TOAST_WINDOW_MS).toBeGreaterThan(6000);
});

test("the window is wide enough for the longest copy to wrap to two lines", () => {
  // A measurement, kept as an assertion because the first size was a guess and
  // clipped. At 420px — the popup's width — the institutional message wraps to
  // four lines and needs 106px of inner height, while windows.create's height
  // includes the platform frame, so 108 outer left ~80 inner and hid the
  // button. At 520 both messages wrapped to two lines and needed 65px inner.
  //
  // Adding the mark took 40px off that row (28px plus the 12px gap) and pushed
  // the institutional message back to three lines and 85px — one past the 84
  // that a 116 outer leaves on macOS. Re-measured in a real browser at
  // PAPIO_MARK_SIZE_PX, the two-line boundary is exactly 552px.
  //
  // Two assertions, because the surface has two independent inputs. This one
  // pins the WIDTH against that boundary, with slack for platforms whose
  // system-ui metrics run wider than macOS's.
  const measuredBoundaryAt28 = 552;
  expect(TOAST_WINDOW_SIZE.width).toBeGreaterThanOrEqual(measuredBoundaryAt28 + 20);
  // And this one pins the COPY, which is what a future wording change would
  // break: a longer sentence re-wraps and the window no longer fits it.
  for (const [kind, copy] of Object.entries(TOAST_COPY)) {
    const longestWord = Math.max(...copy.message.split(" ").map((word) => word.length));
    expect(longestWord, `${kind} has an unwrappable word`).toBeLessThanOrEqual(14);
    // Mark, message, and action label share one row. Both together past this and
    // the two-line measurement no longer holds.
    expect(copy.message.length + copy.action.length, kind).toBeLessThanOrEqual(100);
  }
});

test("the mark fits the two-line text block without setting the card's height", () => {
  // Both bounds are measured, and the width boundary above was measured at this
  // size — so this is the assertion that catches a size change made for looks
  // and invalidating that measurement silently.
  //
  // Lower bound: the 20px line box, which was the first rule tried and rendered
  // visibly undersized against two lines of copy. Upper bound: the 41px two-line
  // block, past which the mark, not the sentence, decides how tall the card is.
  expect(PAPIO_MARK_SIZE_PX).toBeGreaterThan(Math.round(14 * 1.45));
  expect(PAPIO_MARK_SIZE_PX).toBeLessThan(41);
});

test("one constant sizes the mark on both routes", () => {
  // `toast.html` used to carry a `.toast-mark` rule with its own pixel value,
  // which the injected route could not read — two numbers that had to agree with
  // nothing making them. The shared builder now sizes the element itself.
  const { doc, container } = toastDocument();
  const { mark } = renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });
  expect(mark.style.width).toBe(`${PAPIO_MARK_SIZE_PX}px`);
  expect(mark.style.height).toBe(`${PAPIO_MARK_SIZE_PX}px`);
  // A mark that can shrink squashes exactly when the copy is longest, which is
  // the case the width measurement is about.
  expect(mark.style.flexShrink).toBe("0");
});
